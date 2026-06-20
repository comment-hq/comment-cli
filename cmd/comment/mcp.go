package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

type mcpRunOptions struct {
	Home          string
	Profile       string
	BaseURL       string
	Node          string
	Entrypoint    string
	UnsafeDevHost bool
}

type mcpProgram struct {
	Command string
	Args    []string
	Dir     string
}

var runMCPChild = defaultRunMCPChild

// mcpDefaultBaseURL is intentionally the fixed production host, not the
// environment-aware default: `comment mcp run` must resolve a legacy profile
// (one with no base_url) deterministically and must not inherit an ambient
// COMMENT_IO_BASE_URL. Staging selection flows through the profile's own
// base_url, which still takes precedence below.
const mcpDefaultBaseURL = "https://comment.io"

func runMCP(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: comment mcp run --profile <handle> [--home ~/.comment-io]")
	}
	switch args[0] {
	case "run":
		options, err := parseMCPRunArgs(args[1:])
		if err != nil {
			return err
		}
		return runMCPCommand(options)
	default:
		return fmt.Errorf("unknown mcp command %q", args[0])
	}
}

func parseMCPRunArgs(args []string) (mcpRunOptions, error) {
	fs := flag.NewFlagSet("comment mcp run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	options := mcpRunOptions{}
	fs.StringVar(&options.Home, "home", "", "Comment.io home directory")
	fs.StringVar(&options.Profile, "profile", "", "agent profile handle")
	fs.StringVar(&options.BaseURL, "base-url", "", "override Comment.io API base URL")
	fs.StringVar(&options.Node, "node", "", "node executable")
	fs.StringVar(&options.Entrypoint, "entrypoint", "", "MCP entrypoint override")
	fs.BoolVar(&options.UnsafeDevHost, "unsafe-dev-host", false, "allow localhost MCP API base URL for development")
	if err := fs.Parse(args); err != nil {
		return mcpRunOptions{}, err
	}
	if len(fs.Args()) > 0 {
		return mcpRunOptions{}, errors.New("comment mcp run does not accept positional arguments")
	}
	options.Profile = strings.TrimPrefix(options.Profile, "@")
	if options.Profile == "" {
		return mcpRunOptions{}, errors.New("comment mcp run requires --profile")
	}
	if !commentbus.ProfileRE.MatchString(options.Profile) {
		return mcpRunOptions{}, errors.New("invalid profile")
	}
	if strings.ContainsAny(options.Node, "\r\n\x00") || strings.ContainsAny(options.Entrypoint, "\r\n\x00") {
		return mcpRunOptions{}, errors.New("invalid mcp launcher path")
	}
	return options, nil
}

func runMCPCommand(options mcpRunOptions) error {
	paths, err := resolveCLIPaths(options.Home)
	if err != nil {
		return err
	}
	profiles, profileErrors := commentbus.LoadAgentProfiles(context.Background(), paths, mcpDefaultBaseURL)
	profile, ok := profiles[options.Profile]
	if !ok {
		return fmt.Errorf("agent profile %q was not found under %s%s", options.Profile, filepath.Join(paths.Home, "agents"), profileErrorSuffix(profileErrors, options.Profile))
	}
	baseURL, err := normalizeMCPBaseURL(profile.BaseURL)
	if err != nil {
		return fmt.Errorf("invalid base_url in profile %q: %w", options.Profile, err)
	}
	if options.BaseURL != "" {
		overrideBaseURL, err := normalizeMCPBaseURL(options.BaseURL)
		if err != nil {
			return err
		}
		if overrideBaseURL != baseURL {
			return fmt.Errorf("--base-url must match selected profile base_url %s; create or use a profile for %s instead", baseURL, overrideBaseURL)
		}
		baseURL = overrideBaseURL
	}
	program, err := resolveMCPProgram(options)
	if err != nil {
		return err
	}
	daemonAvailable := mcpDaemonAvailable(paths)
	env := mcpChildEnv(mcpBaseEnv(os.Environ()), map[string]string{
		"COMMENT_IO_AGENT_PROFILE":        profile.Handle,
		"COMMENT_IO_AGENT_SECRET":         profile.AgentSecret,
		"COMMENT_IO_BASE_URL":             baseURL,
		"COMMENT_IO_HOME":                 paths.Home,
		"COMMENT_IO_MCP_LAUNCHED_BY":      "comment-cli",
		"COMMENT_IO_MCP_DAEMON_AVAILABLE": boolEnv(daemonAvailable),
	}, nil)
	if options.UnsafeDevHost {
		env = mcpChildEnv(env, map[string]string{"COMMENT_IO_UNSAFE_DEV_HOST": "1"}, nil)
	}
	return runMCPChild(program, env)
}

func resolveMCPProgram(options mcpRunOptions) (mcpProgram, error) {
	if options.Entrypoint != "" {
		return mcpProgramForEntrypoint(options.Entrypoint, options.Node, "")
	}
	if packageRoot, ok := inferMCPPackageRootFromExecutable(); ok {
		return mcpProgramForEntrypoint(filepath.Join(packageRoot, "mcp", "comment-mcp.mjs"), options.Node, "")
	}
	repoRoot, ok := findMCPRepoRoot()
	if ok {
		entrypoint := filepath.Join(repoRoot, "packages", "comment-mcp", "src", "index.ts")
		return mcpProgramForEntrypoint(entrypoint, options.Node, repoRoot)
	}
	return mcpProgram{}, errors.New("could not locate Comment.io MCP entrypoint; reinstall @comment-io/cli or run from the monorepo")
}

func inferMCPPackageRootFromExecutable() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		exe = filepath.Clean(exe)
	}
	distDir := filepath.Dir(exe)
	if filepath.Base(distDir) != "dist" {
		return "", false
	}
	packageRoot := filepath.Dir(distDir)
	if !regularFileExists(filepath.Join(packageRoot, "mcp", "comment-mcp.mjs")) {
		return "", false
	}
	return packageRoot, true
}

func mcpProgramForEntrypoint(entrypoint string, nodeOverride string, dir string) (mcpProgram, error) {
	path, err := filepath.Abs(entrypoint)
	if err != nil {
		return mcpProgram{}, err
	}
	path = filepath.Clean(path)
	if !regularFileExists(path) {
		return mcpProgram{}, fmt.Errorf("MCP entrypoint does not exist: %s", path)
	}
	if strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx") {
		command, err := resolveMCPDevTSX(path, nodeOverride, dir)
		if err != nil {
			return mcpProgram{}, err
		}
		args := []string{path}
		if dir != "" {
			if rel, err := filepath.Rel(dir, path); err == nil && !strings.HasPrefix(rel, "..") {
				args = []string{rel}
			}
		}
		return mcpProgram{Command: command, Args: args, Dir: dir}, nil
	}
	command := nodeOverride
	if command == "" {
		var err error
		command, err = exec.LookPath("node")
		if err != nil {
			return mcpProgram{}, errors.New("comment mcp run requires node on PATH")
		}
	}
	return mcpProgram{Command: command, Args: []string{path}, Dir: dir}, nil
}

func resolveMCPDevTSX(entrypoint string, nodeOverride string, repoRoot string) (string, error) {
	if nodeOverride != "" {
		return nodeOverride, nil
	}
	if repoRoot == "" {
		return "", errors.New("development MCP entrypoint requires a trusted repo root")
	}
	repoRoot = filepath.Clean(repoRoot)
	dir := filepath.Dir(entrypoint)
	for {
		rel, err := filepath.Rel(repoRoot, dir)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			break
		}
		candidate := filepath.Join(dir, "node_modules", ".bin", "tsx")
		if runtime.GOOS == "windows" {
			candidate += ".cmd"
		}
		if regularFileExists(candidate) {
			return candidate, nil
		}
		if dir == repoRoot {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("development MCP entrypoint requires tsx; run npm install in the monorepo")
}

func findMCPRepoRoot() (string, bool) {
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Dir(exe))
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Dir(file))
	}
	for _, start := range candidates {
		dir := filepath.Clean(start)
		for {
			if regularFileExists(filepath.Join(dir, "packages", "comment-mcp", "src", "index.ts")) &&
				regularFileExists(filepath.Join(dir, "package.json")) {
				return dir, true
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", false
}

func defaultRunMCPChild(program mcpProgram, env []string) error {
	cmd := exec.Command(program.Command, program.Args...)
	if program.Dir != "" {
		cmd.Dir = program.Dir
	}
	cmd.Env = env
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return cliExitError{Code: exitErr.ExitCode()}
		}
		return err
	}
	return nil
}

func mcpDaemonAvailable(paths commentbus.Paths) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	response, err := callSocket(ctx, paths, "health", nil, nil, time.Second)
	return err == nil && response.OK
}

func mcpChildEnv(base []string, set map[string]string, unset []string) []string {
	values := map[string]string{}
	order := []string{}
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	for _, key := range unset {
		delete(values, key)
	}
	for key, value := range set {
		if _, exists := values[key]; !exists {
			order = append(order, key)
		}
		values[key] = value
	}
	env := make([]string, 0, len(values))
	for _, key := range order {
		value, ok := values[key]
		if ok {
			env = append(env, key+"="+value)
		}
	}
	return env
}

func normalizeMCPBaseURL(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid --base-url %q", value)
	}
	if parsed.User != nil && parsed.User.String() != "" {
		return "", fmt.Errorf("base URL must not include username or password")
	}
	parsed.Path = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func mcpBaseEnv(base []string) []string {
	allowed := map[string]struct{}{
		"HOME":    {},
		"LANG":    {},
		"LC_ALL":  {},
		"LOGNAME": {},
		"PATH":    {},
		"SHELL":   {},
		"TEMP":    {},
		"TMP":     {},
		"TMPDIR":  {},
		"USER":    {},
	}
	out := []string{}
	seen := map[string]struct{}{}
	for _, entry := range base {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			continue
		}
		if _, include := allowed[key]; !include {
			continue
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key+"="+value)
	}
	return out
}

func profileErrorSuffix(errorsOut []commentbus.ProfileReloadError, profile string) string {
	for _, loadErr := range errorsOut {
		if loadErr.Profile == profile {
			return ": " + loadErr.Message
		}
	}
	if len(errorsOut) > 0 {
		return fmt.Sprintf(" (%d profile load error(s); run `comment doctor` for details)", len(errorsOut))
	}
	return ""
}

func boolEnv(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func regularFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
