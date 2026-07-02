package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

const (
	dockerRuntimeSandboxEnv     = "COMMENT_IO_AGENT_SANDBOX"
	dockerRuntimeInstallMarker  = "docker-agent.json"
	dockerRuntimeProbeTimeout   = 5 * time.Second
	dockerRuntimeCommandTimeout = 10 * time.Second
)

type dockerRuntimeTarget struct {
	BaseURL            string `json:"base_url"`
	Container          string `json:"container"`
	ProjectedAgentsDir string `json:"projected_agents_dir,omitempty"`
	fromMarker         bool
	fromProjection     bool
	profile            string
}

type dockerRuntimeContainerState struct {
	Exists  bool
	Running bool
}

type dockerRuntimeLocalProfile struct {
	Handle    string
	BaseURL   string
	Found     bool
	Projected bool
}

var (
	dockerRuntimeLookPath       = exec.LookPath
	dockerRuntimeCombinedOutput = func(ctx context.Context, command string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, command, args...)
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
	dockerRuntimeRunCommand = func(ctx context.Context, command string, args ...string) error {
		cmd := exec.CommandContext(ctx, command, args...)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		if err == nil {
			return nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return cliExitError{Code: exitErr.ExitCode()}
		}
		return err
	}
	dockerRuntimeHostDaemonUnavailable = hostDaemonUnavailableForDockerRuntime
	dockerRuntimeStdinIsTerminal       = func() bool { return isTerminalFile(os.Stdin) }
	dockerRuntimeStdoutIsTerminal      = func() bool { return isTerminalFile(os.Stdout) }
	dockerRuntimePrivateFileChecksOK   = func() bool { return runtime.GOOS == "darwin" || runtime.GOOS == "linux" }
)

func maybeDelegateRuntimeToDocker(ctx context.Context, paths commentbus.Paths, options runtimeRunOptions) (bool, error) {
	if runningInsideDockerAgentSandbox() || !dockerRuntimeInvocationSafe(options) {
		return false, nil
	}
	if !dockerRuntimeHostDaemonUnavailable(ctx, paths) {
		return false, nil
	}
	target, ok, err := dockerRuntimeTargetForPaths(paths, options)
	if err != nil {
		return true, err
	}
	if !ok {
		return false, nil
	}
	dockerBin, err := dockerRuntimeLookPath("docker")
	if err != nil {
		if target.configured() {
			return true, fmt.Errorf("Docker agent container %q is configured for this Comment.io install, but the docker command is not on your PATH; install Docker or rerun the Docker installer after Docker is available", target.Container)
		}
		return false, nil
	}
	state, err := inspectDockerRuntimeContainer(ctx, dockerBin, target.Container)
	if err != nil {
		if target.configured() {
			return true, fmt.Errorf("Docker agent container %q could not be inspected; start Docker and try again: %w", target.Container, err)
		}
		return false, nil
	}
	if !state.Exists {
		if target.fromMarker {
			return true, fmt.Errorf("Docker agent container %q is configured for this Comment.io install but was not found; rerun the Docker installer or remove %s", target.Container, dockerRuntimeInstallMarkerPath(paths))
		}
		if target.fromProjection {
			return true, fmt.Errorf("Docker agent container %q is expected for projected profile %q but was not found; rerun the Docker installer or remove the projected profile manifest", target.Container, target.profile)
		}
		return false, nil
	}
	if !state.Running {
		return true, fmt.Errorf("Docker agent container %q is stopped; run `docker start %s`, then retry `comment run`", target.Container, target.Container)
	}
	containerBase, err := dockerRuntimeContainerBaseURL(ctx, dockerBin, target.Container)
	if err != nil {
		return true, fmt.Errorf("Docker agent container %q is running but not paired yet; pair it with `docker exec -it %s comment bus pair --base-url %s`", target.Container, target.Container, target.BaseURL)
	}
	if !sameBusPairBaseURL(containerBase, target.BaseURL) {
		return true, fmt.Errorf("Docker agent container %q is paired to %s, but this host command targets %s; set the matching Comment.io environment or reinstall the Docker agent for this origin", target.Container, containerBase, target.BaseURL)
	}
	attach := !shouldSkipRuntimeAttach(options)
	if attach && (!dockerRuntimeStdinIsTerminal() || !dockerRuntimeStdoutIsTerminal()) {
		return true, errors.New("Docker agent runtime attach needs an interactive terminal; rerun in a terminal or pass --detach")
	}
	return true, execDockerRuntimeCommand(ctx, dockerBin, target.Container, attach, options, dockerRuntimeDelegatedArgv(options))
}

func hostDaemonUnavailableForDockerRuntime(ctx context.Context, paths commentbus.Paths) bool {
	probeCtx, cancel := context.WithTimeout(ctx, dockerRuntimeProbeTimeout)
	defer cancel()
	_, err := callSocket(probeCtx, paths, "health", nil, nil, time.Second)
	if err != nil {
		return isDaemonUnavailableError(err)
	}
	return false
}

func dockerRuntimeInvocationSafe(options runtimeRunOptions) bool {
	if strings.TrimSpace(options.Home) != "" || strings.TrimSpace(options.CWD) != "" {
		return false
	}
	if strings.TrimSpace(options.Runtime) == "" {
		return true
	}
	runtime := strings.TrimSpace(options.Runtime)
	if dockerRuntimeHostPathLike(runtime) {
		return false
	}
	return requestedManagedRuntimeName(runtime) != ""
}

func dockerRuntimeHostPathLike(runtime string) bool {
	return filepath.IsAbs(runtime) || strings.ContainsAny(runtime, `/\`) || strings.HasPrefix(runtime, "~")
}

func runningInsideDockerAgentSandbox() bool {
	if os.Getenv(dockerRuntimeSandboxEnv) == "1" {
		return true
	}
	if os.Getenv("COMMENT_IO_HOME") == "/state" {
		if _, err := os.Stat("/usr/local/bin/agent-entrypoint.sh"); err == nil {
			return true
		}
	}
	return false
}

func dockerRuntimeInstallMarkerPath(paths commentbus.Paths) string {
	return filepath.Join(paths.Bus, dockerRuntimeInstallMarker)
}

func dockerRuntimeTargetForPaths(paths commentbus.Paths, options runtimeRunOptions) (dockerRuntimeTarget, bool, error) {
	target, markerOK, markerErr := readDockerRuntimeInstallMarker(paths)
	if markerErr != nil {
		return target, true, markerErr
	}
	local, err := dockerRuntimeLocalProfileForOptions(paths, options, markerOK)
	if err != nil {
		return dockerRuntimeTarget{}, true, err
	}
	if markerOK {
		if local.Found {
			if !local.Projected {
				return dockerRuntimeTarget{}, false, nil
			}
			if !sameBusPairBaseURL(local.BaseURL, target.BaseURL) {
				projected := dockerRuntimeTargetForBaseURL(local.BaseURL)
				projected.fromProjection = true
				projected.profile = local.Handle
				return projected, true, nil
			}
		}
		return target, true, nil
	}
	if local.Found && local.Projected && local.BaseURL != "" {
		target := dockerRuntimeTargetForBaseURL(local.BaseURL)
		target.fromProjection = true
		target.profile = local.Handle
		return target, true, nil
	}
	return dockerRuntimeTarget{}, false, nil
}

func dockerRuntimeTargetForBaseURL(base string) dockerRuntimeTarget {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	return dockerRuntimeTarget{
		BaseURL:   base,
		Container: dockerAgentContainerName(dockerAgentSlug(base)),
	}
}

func (target dockerRuntimeTarget) configured() bool {
	return target.fromMarker || target.fromProjection
}

func readDockerRuntimeInstallMarker(paths commentbus.Paths) (dockerRuntimeTarget, bool, error) {
	path := dockerRuntimeInstallMarkerPath(paths)
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dockerRuntimeTarget{}, false, nil
		}
		return dockerRuntimeTarget{}, true, fmt.Errorf("could not inspect Docker agent marker %s: %w", path, err)
	}
	file, err := openDockerRuntimeHostFile(paths, path, "Docker agent marker")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return dockerRuntimeTarget{}, false, nil
		}
		return dockerRuntimeTarget{}, true, fmt.Errorf("could not open Docker agent marker %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		return dockerRuntimeTarget{}, true, fmt.Errorf("could not read Docker agent marker %s: %w", path, err)
	}
	var marker dockerRuntimeTarget
	if err := json.Unmarshal(data, &marker); err != nil {
		return dockerRuntimeTarget{}, true, fmt.Errorf("Docker agent marker %s is invalid JSON: %w", path, err)
	}
	marker.BaseURL = strings.TrimRight(strings.TrimSpace(marker.BaseURL), "/")
	marker.Container = strings.TrimSpace(marker.Container)
	marker.ProjectedAgentsDir = strings.TrimSpace(marker.ProjectedAgentsDir)
	if marker.BaseURL == "" {
		return dockerRuntimeTarget{}, true, fmt.Errorf("Docker agent marker %s is missing base_url; rerun the Docker installer or remove this marker", path)
	}
	if marker.Container == "" {
		marker.Container = dockerAgentContainerName(dockerAgentSlug(marker.BaseURL))
	}
	if !strings.HasPrefix(marker.Container, dockerAgentResourcePrefix) || strings.ContainsAny(marker.Container, "\r\n\x00/") {
		return dockerRuntimeTarget{}, true, fmt.Errorf("Docker agent marker %s has invalid container %q; rerun the Docker installer or remove this marker", path, marker.Container)
	}
	if marker.ProjectedAgentsDir != "" {
		projectedAgentsDir, err := normalizeDockerProjectedAgentsDir(marker.ProjectedAgentsDir)
		if err != nil {
			return dockerRuntimeTarget{}, true, fmt.Errorf("Docker agent marker %s has invalid projected_agents_dir: %w", path, err)
		}
		marker.ProjectedAgentsDir = projectedAgentsDir
	}
	marker.fromMarker = true
	return marker, true, nil
}

func normalizeDockerProjectedAgentsDir(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil
	}
	if strings.ContainsAny(path, "\r\n\x00") {
		return "", errors.New("must not contain control characters")
	}
	return commentbus.ExpandHome(path)
}

func dockerRuntimeLocalProfileForOptions(paths commentbus.Paths, options runtimeRunOptions, tolerateRegistryErrors bool) (dockerRuntimeLocalProfile, error) {
	var handles []string
	if profile := strings.TrimPrefix(strings.TrimSpace(options.Profile), "@"); profile != "" {
		handles = append(handles, profile)
	}
	if selector := strings.TrimPrefix(strings.TrimSpace(options.BotShortcut), "@"); selector != "" {
		profileSelector := commentbus.ProfileRE.MatchString(selector)
		if options.Role == "" || options.Role == commentbus.RuntimeRoleMain {
			registryHandles, err := dockerRuntimeRegistryProfileHandles(paths, selector, tolerateRegistryErrors || profileSelector)
			if err != nil {
				return dockerRuntimeLocalProfile{}, err
			}
			handles = append(handles, registryHandles...)
		}
		if profileSelector {
			handles = append(handles, selector)
		}
	}
	profiles, errorsOut := commentbus.LoadAgentProfiles(context.Background(), paths, "")
	if len(handles) == 0 && commentbus.HasFatalProfileReloadError(errorsOut) && tolerateRegistryErrors {
		return dockerRuntimeLocalProfile{}, nil
	}
	if len(handles) > 0 && commentbus.HasFatalProfileReloadError(errorsOut) {
		return dockerRuntimeLocalProfile{}, fmt.Errorf("could not load local profiles before Docker delegation: %+v", errorsOut)
	}
	seen := map[string]bool{}
	for _, handle := range handles {
		key := strings.ToLower(handle)
		if seen[key] {
			continue
		}
		seen[key] = true
		profile, ok := profiles[handle]
		if !ok {
			if loadErr, ok := profileLoadErrorForHandle(errorsOut, handle); ok {
				return dockerRuntimeLocalProfile{}, fmt.Errorf("agent profile %q could not be loaded: %s", handle, loadErr.Message)
			}
			continue
		}
		if base := strings.TrimRight(strings.TrimSpace(profile.BaseURL), "/"); base != "" {
			return dockerRuntimeLocalProfile{
				Handle:    handle,
				BaseURL:   base,
				Found:     true,
				Projected: dockerRuntimeProjectionOwnsProfile(paths, handle),
			}, nil
		}
	}
	return dockerRuntimeLocalProfile{}, nil
}

func dockerRuntimeRegistryProfileHandles(paths commentbus.Paths, selector string, tolerateRegistryErrors bool) ([]string, error) {
	if selector == "" {
		return nil, nil
	}
	state, errorsOut := commentbus.LoadProfileState(context.Background(), commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: persistedCLIBotletsHome(paths, ""),
	})
	if commentbus.HasFatalProfileReloadError(errorsOut) {
		if tolerateRegistryErrors {
			return nil, nil
		}
		return nil, fmt.Errorf("could not load local profiles before Docker delegation: %+v", errorsOut)
	}
	var handles []string
	if strings.EqualFold(selector, "default") {
		if entry, ok, err := selectManagedDefaultBotletsRunShortcut(state.BotRegistry); err == nil && ok {
			handles = append(handles, entry.Handle)
		}
	}
	if entry, ok, err := selectBotletsEntryBySelector(state.BotRegistry, selector); err == nil && ok {
		handles = append(handles, entry.Handle)
	}
	return handles, nil
}

func profileLoadErrorForHandle(errorsOut []commentbus.ProfileReloadError, handle string) (commentbus.ProfileReloadError, bool) {
	for _, loadErr := range errorsOut {
		if strings.EqualFold(loadErr.Profile, handle) {
			return loadErr, true
		}
	}
	return commentbus.ProfileReloadError{}, false
}

func dockerRuntimeProjectionOwnsProfile(paths commentbus.Paths, handle string) bool {
	if !commentbus.ProfileRE.MatchString(handle) {
		return false
	}
	file, err := openDockerRuntimeHostFile(paths, filepath.Join(paths.Home, "agents", ".comment-agent-projected.manifest"), "projected agent manifest")
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(file)
	if err != nil {
		return false
	}
	var manifest struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false
	}
	want := handle + ".json"
	for _, file := range manifest.Files {
		if file == want {
			return true
		}
	}
	return false
}

func openDockerRuntimeHostFile(paths commentbus.Paths, path string, label string) (*os.File, error) {
	return openDockerRuntimeHostFileUnderRoot(paths.Home, path, label)
}

func openDockerRuntimeHostFileUnderRoot(root string, path string, label string) (*os.File, error) {
	if dockerRuntimePrivateFileChecksOK() {
		return commentbus.OpenPrivateFile(root, path, label)
	}
	root = filepath.Clean(root)
	cleanPath := filepath.Clean(path)
	rel, err := filepath.Rel(root, cleanPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return nil, fmt.Errorf("%s must live under selected root", label)
	}
	info, err := os.Lstat(cleanPath)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s must not be a symlink", label)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", label)
	}
	return os.Open(cleanPath)
}

func inspectDockerRuntimeContainer(ctx context.Context, dockerBin string, container string) (dockerRuntimeContainerState, error) {
	inspectCtx, cancel := context.WithTimeout(ctx, dockerRuntimeCommandTimeout)
	defer cancel()
	out, err := dockerRuntimeCombinedOutput(inspectCtx, dockerBin, "inspect", "-f", "{{.State.Running}}", container)
	if err != nil {
		text := strings.ToLower(string(out))
		if strings.Contains(text, "no such object") || strings.Contains(text, "no such container") {
			return dockerRuntimeContainerState{}, nil
		}
		return dockerRuntimeContainerState{}, err
	}
	return dockerRuntimeContainerState{Exists: true, Running: strings.TrimSpace(string(out)) == "true"}, nil
}

func dockerRuntimeContainerBaseURL(ctx context.Context, dockerBin string, container string) (string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, dockerRuntimeCommandTimeout)
	defer cancel()
	out, err := dockerRuntimeCombinedOutput(probeCtx, dockerBin, "exec", container, "cat", "/state/bus/daemon-auth.json")
	if err != nil {
		return "", err
	}
	var auth commentbus.DaemonAuth
	if err := json.Unmarshal(out, &auth); err != nil {
		return "", err
	}
	base := strings.TrimRight(strings.TrimSpace(auth.BaseURL), "/")
	if base == "" {
		return "", errors.New("daemon auth is missing base_url")
	}
	return base, nil
}

func execDockerRuntimeCommand(ctx context.Context, dockerBin string, container string, attach bool, options runtimeRunOptions, delegated []string) error {
	args := []string{"exec"}
	if attach {
		args = append(args, "-it")
	}
	args = append(args, "-e", dockerRuntimeSandboxEnv+"=1")
	if options.ModelSet {
		for _, entry := range withRuntimeRequestModelEnv(nil, options.Model) {
			args = append(args, "-e", entry)
		}
	}
	if os.Getenv("COMMENT_IO_SKIP_ATTACH") == "1" {
		args = append(args, "-e", "COMMENT_IO_SKIP_ATTACH=1")
	}
	if term := sanitizedDockerRuntimeEnv("TERM"); term != "" {
		args = append(args, "-e", "TERM="+term)
	}
	args = append(args, container, "comment")
	args = append(args, delegated...)
	return dockerRuntimeRunCommand(ctx, dockerBin, args...)
}

func sanitizedDockerRuntimeEnv(key string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return ""
	}
	return value
}

func dockerRuntimeDelegatedArgv(options runtimeRunOptions) []string {
	args := []string{"run"}
	if options.Runtime != "" {
		args = append(args, "--runtime", options.Runtime)
	}
	if options.Profile != "" {
		args = append(args, "--profile", options.Profile)
	}
	if options.Model != "" {
		args = append(args, "--model", options.Model)
	}
	if options.Role != "" && options.Role != commentbus.RuntimeRoleMain {
		args = append(args, "--role", options.Role)
	}
	if options.SetupAttemptID != "" {
		args = append(args, "--setup-attempt-id", options.SetupAttemptID)
	}
	if options.Detach {
		args = append(args, "--detach")
	}
	if options.BotShortcut != "" && !(options.BotShortcut == "default" && options.Runtime == "" && options.Profile == "") {
		args = append(args, options.BotShortcut)
	}
	if len(options.RuntimeArgs) > 0 {
		args = append(args, "--")
		args = append(args, options.RuntimeArgs...)
	}
	return args
}

func writeDockerRuntimeInstallMarker(paths commentbus.Paths, target dockerRuntimeTarget) error {
	target.BaseURL = strings.TrimRight(strings.TrimSpace(target.BaseURL), "/")
	target.Container = strings.TrimSpace(target.Container)
	if target.BaseURL == "" || target.Container == "" {
		return errors.New("docker runtime marker requires base_url and container")
	}
	if err := os.MkdirAll(paths.Bus, 0o700); err != nil {
		return err
	}
	payload := map[string]string{
		"base_url":  target.BaseURL,
		"container": target.Container,
	}
	if projectedAgentsDir, err := normalizeDockerProjectedAgentsDir(target.ProjectedAgentsDir); err != nil {
		return fmt.Errorf("docker runtime marker projected_agents_dir: %w", err)
	} else if projectedAgentsDir != "" {
		payload["projected_agents_dir"] = projectedAgentsDir
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dockerRuntimeInstallMarkerPath(paths), append(data, '\n'), 0o600)
}
