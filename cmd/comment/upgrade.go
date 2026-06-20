package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const defaultUpgradePackageSpec = "@comment-io/cli@latest"

var upgradeCombinedOutput = func(ctx context.Context, env []string, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	if env != nil {
		cmd.Env = env
	}
	configureUpgradeCommandCancel(cmd)
	cmd.WaitDelay = 5 * time.Second
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return append(stdout.Bytes(), stderr.Bytes()...), err
	}
	return stdout.Bytes(), nil
}

var upgradeLookPath = exec.LookPath

var upgradeDaemonHealthRetryDelay = 500 * time.Millisecond

type upgradeOptions struct {
	Home          string
	BotletsHome string
	PackageSpec   string
	NPM           string
	SkipDaemon    bool
	DryRun        bool
}

type upgradeInstalledPaths struct {
	NpmPrefix      string `json:"npm_prefix"`
	NpmRoot        string `json:"npm_root"`
	PackageRoot    string `json:"package_root"`
	PackageVersion string `json:"package_version"`
	CommentBin     string `json:"comment_bin"`
	PathBin        string `json:"path_bin,omitempty"`
	PathWarning    string `json:"path_warning,omitempty"`
	NativeBin      string `json:"native_bin,omitempty"`
	ServiceBin     string `json:"service_bin"`
}

func runUpgrade(args []string) error {
	options := upgradeOptions{}
	fs := flag.NewFlagSet("comment upgrade", flag.ContinueOnError)
	fs.StringVar(&options.Home, "home", "", "Comment.io home directory")
	fs.StringVar(&options.BotletsHome, "botlets-home", "", "Botlets home directory")
	fs.StringVar(&options.PackageSpec, "package", defaultUpgradePackage(), "npm package spec to install")
	fs.StringVar(&options.NPM, "npm", "npm", "npm binary path")
	fs.BoolVar(&options.SkipDaemon, "skip-daemon", false, "upgrade the CLI without reinstalling the persistent daemon")
	fs.BoolVar(&options.DryRun, "dry-run", false, "print the upgrade plan without making changes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("upgrade does not accept positional arguments")
	}
	if strings.TrimSpace(options.PackageSpec) == "" {
		return errors.New("upgrade package cannot be empty")
	}
	if strings.TrimSpace(options.NPM) == "" {
		return errors.New("npm binary cannot be empty")
	}
	if err := validateUpgradeSupportedPlatform(); err != nil {
		return err
	}
	if options.DryRun {
		return printJSON(upgradeDryRunResult(options))
	}
	ctx, stop := signal.NotifyContext(context.Background(), upgradeShutdownSignals()...)
	defer stop()
	result, err := performUpgrade(ctx, options)
	if err != nil {
		return err
	}
	return printJSON(result)
}

func defaultUpgradePackage() string {
	if value := strings.TrimSpace(os.Getenv("COMMENT_IO_CLI_PACKAGE")); value != "" {
		return value
	}
	return defaultUpgradePackageSpec
}

func validateUpgradeSupportedPlatform() error {
	return validateUpgradeSupportedPlatformForRuntime(runtime.GOOS, runtime.GOARCH)
}

func validateUpgradeSupportedPlatformForRuntime(goos string, goarch string) error {
	switch goos {
	case "darwin", "linux":
	default:
		return fmt.Errorf("comment upgrade is supported on macOS and Linux on x64 or arm64 only; @comment-io/cli currently ships npm binaries only for macOS and Linux on x64 or arm64")
	}
	switch goarch {
	case "amd64", "arm64":
		return nil
	default:
		return fmt.Errorf("comment upgrade is supported on macOS and Linux on x64 or arm64 only; @comment-io/cli currently ships npm binaries only for macOS and Linux on x64 or arm64")
	}
}

func upgradeDryRunResult(options upgradeOptions) map[string]any {
	npmBin, err := resolveUpgradeExecutable(options.NPM)
	npmCommand := options.NPM
	result := map[string]any{
		"ok":           true,
		"dry_run":      true,
		"from_version": version,
		"package":      options.PackageSpec,
		"npm": map[string]any{
			"bin":  npmCommand,
			"args": []string{"install", "-g", "--prefer-online", options.PackageSpec},
		},
		"daemon": map[string]any{
			"skipped": options.SkipDaemon,
		},
	}
	if err == nil {
		npmCommand = npmBin
		result["npm"].(map[string]any)["bin"] = npmCommand
	} else {
		result["npm"].(map[string]any)["resolution_error"] = err.Error()
	}
	if !options.SkipDaemon {
		args := []string{"bus", "install"}
		args = appendOptionalFlag(args, "--home", options.Home)
		args = appendOptionalFlag(args, "--botlets-home", options.BotletsHome)
		args = append(args, "--bin", "<resolved npm native binary>")
		result["daemon"].(map[string]any)["args"] = args
	}
	return result
}

func performUpgrade(ctx context.Context, options upgradeOptions) (map[string]any, error) {
	if err := validateUpgradeSupportedPlatform(); err != nil {
		return nil, err
	}
	npmBin, err := resolveUpgradeExecutable(options.NPM)
	if err != nil {
		return nil, err
	}
	installArgs := []string{"install", "-g", "--prefer-online", options.PackageSpec}
	if _, err := runUpgradeCommand(ctx, 10*time.Minute, npmBin, installArgs...); err != nil {
		return nil, fmt.Errorf("npm install failed: %w", err)
	}
	paths, err := resolveNpmInstalledCommentPaths(ctx, npmBin)
	if err != nil {
		return nil, err
	}
	health, err := runUpgradeFreshCLIHealth(ctx, paths.CommentBin)
	if err != nil {
		return nil, fmt.Errorf("fresh CLI health check failed: %w", err)
	}
	if err := validateUpgradeCLIHealth(health, paths.PackageVersion); err != nil {
		return nil, err
	}
	serviceHealth := health
	if paths.ServiceBin != paths.CommentBin {
		serviceHealth, err = runUpgradeFreshCLIHealth(ctx, paths.ServiceBin)
		if err != nil {
			return nil, fmt.Errorf("fresh service binary health check failed: %w", err)
		}
		if err := validateUpgradeCLIHealth(serviceHealth, paths.PackageVersion); err != nil {
			return nil, err
		}
	}
	warnings := upgradeWarnings(paths)
	result := map[string]any{
		"ok":           true,
		"from_version": version,
		"package":      options.PackageSpec,
		"npm": map[string]any{
			"bin":  npmBin,
			"args": installArgs,
		},
		"cli": map[string]any{
			"paths":          paths,
			"health":         health,
			"service_health": serviceHealth,
		},
		"daemon": map[string]any{
			"skipped": options.SkipDaemon,
		},
	}
	if len(warnings) > 0 {
		result["warnings"] = warnings
	}
	if options.SkipDaemon {
		return result, nil
	}
	daemonArgs := []string{"bus", "install"}
	daemonArgs = appendOptionalFlag(daemonArgs, "--home", options.Home)
	daemonArgs = appendOptionalFlag(daemonArgs, "--botlets-home", options.BotletsHome)
	daemonArgs = append(daemonArgs, "--bin", paths.ServiceBin)
	daemonResult, err := runUpgradeJSONCommandWithEnv(ctx, 2*time.Minute, upgradeDaemonInstallEnv(options), paths.ServiceBin, daemonArgs...)
	if err != nil {
		return nil, fmt.Errorf("daemon reinstall failed: %w", err)
	}
	expectedDaemonVersion, _ := serviceHealth["version"].(string)
	daemonHealth, err := waitForUpgradeDaemonHealth(ctx, paths.ServiceBin, options.Home, expectedDaemonVersion)
	if err != nil {
		return nil, err
	}
	result["daemon"] = map[string]any{
		"skipped": false,
		"args":    daemonArgs,
		"health":  daemonHealth,
		"result":  daemonResult,
	}
	return result, nil
}

func runUpgradeFreshCLIHealth(ctx context.Context, commentBin string) (map[string]any, error) {
	home, err := os.MkdirTemp("", "comment-upgrade-health-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	return runUpgradeJSONCommand(ctx, 30*time.Second, commentBin, "bus", "health", "--home", home)
}

func waitForUpgradeDaemonHealth(ctx context.Context, commentBin string, home string, expectedVersion string) (map[string]any, error) {
	args := []string{"daemon", "health"}
	args = appendOptionalFlag(args, "--home", home)
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for {
		health, err := runUpgradeJSONCommand(ctx, 5*time.Second, commentBin, args...)
		if err == nil {
			if err := validateUpgradeDaemonVersion(health, expectedVersion); err == nil {
				return health, nil
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("daemon did not become healthy after upgrade: %w", lastErr)
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("daemon health check canceled after upgrade: %w", ctx.Err())
		case <-time.After(upgradeDaemonHealthRetryDelay):
		}
	}
}

func validateUpgradeDaemonVersion(health map[string]any, expectedVersion string) error {
	if expectedVersion == "" {
		return nil
	}
	actualVersion, ok := health["version"].(string)
	if !ok || actualVersion == "" {
		return fmt.Errorf("daemon health did not report a version")
	}
	if actualVersion != expectedVersion {
		return fmt.Errorf("daemon version %q does not match fresh CLI version %q", actualVersion, expectedVersion)
	}
	return nil
}

func validateUpgradeCLIHealth(health map[string]any, expectedVersion string) error {
	actualVersion, ok := health["version"].(string)
	if !ok || actualVersion == "" {
		return fmt.Errorf("fresh CLI health did not report a version")
	}
	if expectedVersion != "" && actualVersion != expectedVersion {
		return fmt.Errorf("fresh CLI version %q does not match installed package version %q", actualVersion, expectedVersion)
	}
	return nil
}

func upgradeDaemonInstallEnv(options upgradeOptions) []string {
	if options.BotletsHome != "" {
		return nil
	}
	return envWithValue(os.Environ(), "BOTLETS_HOME", "")
}

func envWithValue(env []string, key string, value string) []string {
	prefix := key + "="
	next := make([]string, 0, len(env)+1)
	replaced := false
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if !replaced {
				next = append(next, prefix+value)
				replaced = true
			}
			continue
		}
		next = append(next, entry)
	}
	if !replaced {
		next = append(next, prefix+value)
	}
	return next
}

func resolveNpmInstalledCommentPaths(ctx context.Context, npmBin string) (upgradeInstalledPaths, error) {
	prefix, err := runUpgradeOutputString(ctx, 30*time.Second, npmBin, "prefix", "-g")
	if err != nil {
		return upgradeInstalledPaths{}, fmt.Errorf("npm prefix failed: %w", err)
	}
	root, err := runUpgradeOutputString(ctx, 30*time.Second, npmBin, "root", "-g")
	if err != nil {
		return upgradeInstalledPaths{}, fmt.Errorf("npm root failed: %w", err)
	}
	packageRoot := filepath.Join(root, "@comment-io", "cli")
	packageVersion, err := readUpgradePackageVersion(packageRoot)
	if err != nil {
		return upgradeInstalledPaths{}, err
	}
	commentBin, err := resolveUpgradeCommentBin(prefix, packageRoot)
	if err != nil {
		return upgradeInstalledPaths{}, err
	}
	if err := validateUpgradeBinBelongsToPackage(commentBin, packageRoot); err != nil {
		return upgradeInstalledPaths{}, err
	}
	if err := validateUpgradeExecutable(commentBin, "comment binary"); err != nil {
		return upgradeInstalledPaths{}, err
	}
	nativeBin := ""
	serviceBin := commentBin
	if target := cliNativeTargetName(); target != "" {
		candidate := filepath.Join(packageRoot, "dist", target)
		if isExecutableFile(candidate) {
			nativeBin = candidate
			serviceBin = candidate
		}
	}
	pathBin, pathWarning := inspectUpgradePathCommand(commentBin, serviceBin)
	cleanPathBin := ""
	if pathBin != "" {
		cleanPathBin = filepath.Clean(pathBin)
	}
	return upgradeInstalledPaths{
		NpmPrefix:      prefix,
		NpmRoot:        root,
		PackageRoot:    filepath.Clean(packageRoot),
		PackageVersion: packageVersion,
		CommentBin:     filepath.Clean(commentBin),
		PathBin:        cleanPathBin,
		PathWarning:    pathWarning,
		NativeBin:      nativeBin,
		ServiceBin:     filepath.Clean(serviceBin),
	}, nil
}

func resolveUpgradeCommentBin(prefix string, packageRoot string) (string, error) {
	var candidates []string
	for _, candidate := range npmCommentBinCandidates(prefix) {
		if !isExecutableFile(candidate) {
			candidates = append(candidates, candidate)
			continue
		}
		if err := validateUpgradeBinBelongsToPackage(candidate, packageRoot); err != nil {
			return "", err
		}
		return candidate, nil
	}
	return "", fmt.Errorf("fresh global comment binary not found; checked %s; npm may have bin-links disabled or a nonstandard global bin layout", strings.Join(candidates, ", "))
}

func npmCommentBinCandidates(prefix string) []string {
	return []string{filepath.Join(prefix, "bin", "comment")}
}

func upgradeWarnings(paths upgradeInstalledPaths) []string {
	if paths.PathWarning == "" {
		return nil
	}
	return []string{paths.PathWarning}
}

func readUpgradePackageVersion(packageRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(packageRoot, "package.json"))
	if err != nil {
		return "", fmt.Errorf("fresh @comment-io/cli package.json not found under npm root: %w", err)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return "", fmt.Errorf("fresh @comment-io/cli package.json is not valid JSON: %w", err)
	}
	if strings.TrimSpace(pkg.Version) == "" {
		return "", errors.New("fresh @comment-io/cli package.json is missing version")
	}
	return pkg.Version, nil
}

func validateUpgradeBinBelongsToPackage(commentBin string, packageRoot string) error {
	resolvedBin, err := filepath.EvalSymlinks(commentBin)
	if err != nil {
		return fmt.Errorf("could not resolve fresh global comment binary: %w", err)
	}
	resolvedPackageRoot, err := filepath.EvalSymlinks(packageRoot)
	if err != nil {
		return fmt.Errorf("could not resolve fresh @comment-io/cli package root: %w", err)
	}
	rel, err := filepath.Rel(resolvedPackageRoot, resolvedBin)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || rel == ".." || filepath.IsAbs(rel) {
		return fmt.Errorf("fresh global comment binary at %s does not resolve into installed package root %s", commentBin, packageRoot)
	}
	return nil
}

func inspectUpgradePathCommand(commentBin string, acceptedBins ...string) (string, string) {
	pathBin, err := upgradeLookPath("comment")
	if err != nil {
		return "", fmt.Sprintf("fresh global comment binary is installed at %s, but comment was not found on PATH: %v", commentBin, err)
	}
	for _, accepted := range uniqueUpgradeAcceptedBins(append([]string{commentBin}, acceptedBins...)) {
		same, err := sameResolvedExecutable(pathBin, accepted)
		if err != nil {
			return pathBin, err.Error()
		}
		if same {
			return pathBin, ""
		}
	}
	return pathBin, fmt.Sprintf("comment on PATH resolves to %s, not fresh global binary %s", pathBin, commentBin)
}

func uniqueUpgradeAcceptedBins(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		clean := filepath.Clean(strings.TrimSpace(value))
		if clean == "." || seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	return out
}

func sameResolvedExecutable(a string, b string) (bool, error) {
	resolvedA, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false, fmt.Errorf("could not resolve PATH comment binary %s: %w", a, err)
	}
	resolvedB, err := filepath.EvalSymlinks(b)
	if err != nil {
		return false, fmt.Errorf("could not resolve fresh global comment binary %s: %w", b, err)
	}
	return filepath.Clean(resolvedA) == filepath.Clean(resolvedB), nil
}

func cliNativeTargetName() string {
	goos := runtime.GOOS
	if goos != "darwin" && goos != "linux" {
		return ""
	}
	goarch := runtime.GOARCH
	switch goarch {
	case "amd64", "arm64":
	default:
		return ""
	}
	return "comment-" + goos + "-" + goarch
}

func appendOptionalFlag(args []string, flagName string, value string) []string {
	if value == "" {
		return args
	}
	return append(args, flagName, value)
}

func runUpgradeOutputString(ctx context.Context, timeout time.Duration, command string, args ...string) (string, error) {
	output, err := runUpgradeCommand(ctx, timeout, command, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

func runUpgradeJSONCommand(ctx context.Context, timeout time.Duration, command string, args ...string) (map[string]any, error) {
	return runUpgradeJSONCommandWithEnv(ctx, timeout, nil, command, args...)
}

func runUpgradeJSONCommandWithEnv(ctx context.Context, timeout time.Duration, env []string, command string, args ...string) (map[string]any, error) {
	output, err := runUpgradeCommandWithEnv(ctx, timeout, env, command, args...)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if err := json.Unmarshal(output, &decoded); err != nil {
		return nil, fmt.Errorf("%s returned non-JSON output: %q", filepath.Base(command), strings.TrimSpace(string(output)))
	}
	return decoded, nil
}

func runUpgradeCommand(parent context.Context, timeout time.Duration, command string, args ...string) ([]byte, error) {
	return runUpgradeCommandWithEnv(parent, timeout, nil, command, args...)
}

func runUpgradeCommandWithEnv(parent context.Context, timeout time.Duration, env []string, command string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	output, err := upgradeCombinedOutput(ctx, env, command, args...)
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s timed out after %s", upgradeCommandString(command, args...), timeout)
		}
		return nil, fmt.Errorf("%s canceled: %w", upgradeCommandString(command, args...), ctx.Err())
	}
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return nil, fmt.Errorf("%s: %w", upgradeCommandString(command, args...), err)
		}
		return nil, fmt.Errorf("%s: %w\n%s", upgradeCommandString(command, args...), err, trimmed)
	}
	return output, nil
}

func upgradeCommandString(command string, args ...string) string {
	parts := append([]string{command}, args...)
	return strings.Join(parts, " ")
}

func resolveUpgradeExecutable(command string) (string, error) {
	clean := strings.TrimSpace(command)
	if clean == "" {
		return "", errors.New("executable path cannot be empty")
	}
	if filepath.IsAbs(clean) || strings.Contains(clean, string(os.PathSeparator)) {
		if !filepath.IsAbs(clean) {
			abs, err := filepath.Abs(clean)
			if err != nil {
				return "", err
			}
			clean = abs
		}
		clean = filepath.Clean(clean)
		if err := validateUpgradeExecutable(clean, "executable"); err != nil {
			return "", err
		}
		return clean, nil
	}
	resolved, err := upgradeLookPath(clean)
	if err != nil {
		return "", fmt.Errorf("%s not found on PATH: %w", clean, err)
	}
	return resolved, nil
}

func validateUpgradeExecutable(path string, label string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s must exist: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", label)
	}
	if runtime.GOOS == "windows" {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".exe", ".cmd", ".bat", ".com":
			return nil
		}
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s must be executable", label)
	}
	return nil
}

func isExecutableFile(path string) bool {
	return validateUpgradeExecutable(path, "executable") == nil
}
