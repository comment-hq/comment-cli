//go:build darwin || linux

package commentbus

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

type SessionExecOptions struct {
	Paths      Paths
	SessionID  string
	Generation string
	Environ    []string
	LookPath   func(string) (string, error)
	Exec       func(string, []string, []string) error
}

type runtimeCommandResolution struct {
	CommandPath string
	RuntimePath string
}

func ExecManagedSession(paths Paths, sessionID string, generation string) error {
	return RunSessionExec(SessionExecOptions{
		Paths:      paths,
		SessionID:  sessionID,
		Generation: generation,
		Environ:    os.Environ(),
		Exec:       syscall.Exec,
	})
}

func RunSessionExec(options SessionExecOptions) error {
	if !LocalSessionIDRE.MatchString(options.SessionID) || !LocalSessionGenerationIDRE.MatchString(options.Generation) {
		return errors.New("invalid managed session")
	}
	record, err := ReadSessionRecord(options.Paths, options.SessionID)
	if err != nil {
		return err
	}
	if record.Generation != options.Generation {
		return errors.New("invalid managed session generation")
	}
	if record.State != "starting" && record.State != "alive" {
		return errors.New("managed session is not runnable")
	}
	if err := validateRuntimeCommand(record); err != nil {
		return err
	}
	if options.Exec == nil {
		options.Exec = syscall.Exec
	}
	if normalizeRuntimeLaunchMode(record.RuntimeLaunchMode) == RuntimeLaunchModeShell {
		return runShellSessionExec(record, options)
	}
	runtimePath, err := resolveRuntimeCommandExecutable(record, options.LookPath)
	if err != nil {
		return err
	}
	argv := append([]string{runtimePath}, record.RuntimeCommand[1:]...)
	execRecord := record
	execRecord.RuntimePath = runtimePath
	env := managedSessionEnv(options.Environ, options.Paths, execRecord)
	return options.Exec(runtimePath, argv, env)
}

// runShellSessionExec launches a managed runtime in "shell" mode: it resolves
// the runtime *name* through the user's interactive login shell, supporting
// PATH binaries, aliases, and functions. No trusted-binary path resolution is
// performed (that is the deliberate design — see the plan §2). Injected
// COMMENT_IO_* / deployment vars are both placed in the process env and
// re-asserted via an in-script `export` prefix so an env-manager rc cannot drop
// them (§4.7).
func runShellSessionExec(record SessionRecord, options SessionExecOptions) error {
	runtimeName := record.RuntimeCommand[0]
	if !isManagedSessionRuntime(runtimeName) || !isShellSafeRuntimeName(runtimeName) {
		return errors.New("invalid runtime command")
	}
	shellPath, family, err := resolveLoginShell()
	if err != nil {
		return err
	}
	prefixEnv := append(managedSessionInjectedEnv(options.Paths, record), runtimeTerminalColorEnv(safeProcessEnv(options.Environ))...)
	prefix, err := shellExportPrefix(prefixEnv)
	if err != nil {
		return err
	}
	prefix = "unset NO_COLOR; " + prefix
	argv := buildShellLaunchArgv(shellPath, family, runtimeName, prefix, record.RuntimeCommand[1:])
	env := appendShellLaunchEnv(managedSessionEnv(options.Environ, options.Paths, record), family)
	return options.Exec(shellPath, argv, env)
}

func validateRuntimeCommand(record SessionRecord) error {
	if !isManagedSessionRuntime(record.Runtime) {
		return errors.New("unsupported runtime")
	}
	if !managedSessionRuntimeCommandMatches(record) {
		return errors.New("invalid runtime command")
	}
	if !isBotName(record.BotName) {
		return errors.New("invalid runtime bot")
	}
	if normalizeRuntimeLaunchMode(record.RuntimeLaunchMode) == RuntimeLaunchModeShell {
		// Shell mode resolves the runtime name through the login shell at
		// launch; the path fields carry no meaning and must be empty.
		if record.RuntimePath != "" || record.RuntimeCommandPath != "" {
			return errors.New("invalid runtime command")
		}
		return nil
	}
	if record.RuntimePath != "" {
		if !isSafeAbsoluteLocalPath(record.RuntimePath) {
			return errors.New("invalid runtime command")
		}
		if !isSafeAbsoluteLocalPath(record.RuntimeCommandPath) || filepath.Base(record.RuntimeCommandPath) != record.Runtime {
			return errors.New("invalid runtime command")
		}
	} else if record.RuntimeCommandPath != "" {
		return errors.New("invalid runtime command")
	}
	return nil
}

func resolveRuntimeCommandExecutable(record SessionRecord, lookPath func(string) (string, error)) (string, error) {
	if record.RuntimePath == "" || record.RuntimeCommandPath == "" {
		return "", errors.New("runtime command is not pinned")
	}
	runtimePath, err := resolveTrustedExecutable(record.RuntimePath, "runtime binary")
	if err != nil {
		return "", err
	}
	commandResolution, err := resolveRuntimeCommandPath(record.RuntimeCommandPath)
	if err != nil {
		return "", err
	}
	if filepath.Base(commandResolution.CommandPath) != record.Runtime {
		return "", errors.New("runtime command does not match configured runtime")
	}
	if commandResolution.RuntimePath != runtimePath {
		return "", errors.New("runtime command does not match pinned runtime binary")
	}
	return runtimePath, nil
}

func resolveRuntimeCommandReference(record SessionRecord, lookPath func(string) (string, error)) (runtimeCommandResolution, error) {
	if lookPath != nil {
		runtimePath, err := lookPath(record.RuntimeCommand[0])
		if err != nil {
			return runtimeCommandResolution{}, err
		}
		return resolveRuntimeCommandPath(runtimePath)
	}
	if resolution, ok, err := resolveRuntimeCommandFromTrustedSearch(record.RuntimeCommand[0]); ok || err != nil {
		return resolution, err
	}
	runtimePath, err := exec.LookPath(record.RuntimeCommand[0])
	if err != nil {
		return runtimeCommandResolution{}, err
	}
	return resolveRuntimeCommandPath(runtimePath)
}

func resolveRuntimeCommandFromTrustedSearch(command string) (runtimeCommandResolution, bool, error) {
	for _, dir := range trustedExecutableSearchDirs("") {
		candidate := filepath.Join(dir, command)
		if _, err := os.Lstat(candidate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return runtimeCommandResolution{}, true, err
		}
		resolution, err := resolveRuntimeCommandPath(candidate)
		if err != nil {
			return runtimeCommandResolution{}, true, err
		}
		return resolution, true, nil
	}
	return runtimeCommandResolution{}, false, nil
}

func resolveRuntimeCommandPath(runtimeCommandPath string) (runtimeCommandResolution, error) {
	if !isSafeAbsoluteLocalPath(runtimeCommandPath) || !isManagedSessionRuntime(filepath.Base(runtimeCommandPath)) {
		return runtimeCommandResolution{}, errors.New("invalid runtime binary")
	}
	commandPath := filepath.Clean(runtimeCommandPath)
	runtimePath, err := resolveTrustedExecutable(commandPath, "runtime binary")
	if err != nil {
		return runtimeCommandResolution{}, err
	}
	return runtimeCommandResolution{CommandPath: commandPath, RuntimePath: runtimePath}, nil
}

func managedSessionEnv(base []string, paths Paths, record SessionRecord) []string {
	out := ensureManagedRuntimeTerminalEnv(safeProcessEnv(base))
	managedKeys := map[string]struct{}{
		"COMMENT_IO_HOME":               {},
		"COMMENT_IO_PROFILE":            {},
		"COMMENT_IO_BOT_NAME":           {},
		"COMMENT_IO_SESSION_ID":         {},
		"COMMENT_IO_SESSION_GENERATION": {},
		"COMMENT_IO_SESSION_CAP_FILE":   {},
		"BOTLETS_HOME":                  {},
	}
	filtered := out[:0]
	for _, entry := range out {
		key := envKey(entry)
		if _, ok := managedKeys[key]; ok {
			continue
		}
		filtered = append(filtered, entry)
	}
	filtered = append(filtered, managedSessionInjectedEnv(paths, record)...)
	// PATH for the launched process. In path mode this includes the resolved
	// runtime binary's directory. In shell mode RuntimePath is empty, so this is
	// the fixed trusted-dir set (which includes ~/.local/bin and ~/bin) — a sane
	// *bootstrap* PATH for the `-ilc` login shell, which then sources rc and
	// extends PATH with the user's own dirs before resolving the runtime name.
	if runtimePath := trustedRuntimeSearchPath(record.RuntimePath); runtimePath != "" {
		filtered = append(filtered, "PATH="+runtimePath)
	}
	return filtered
}

// managedSessionInjectedEnv returns the daemon-injected env entries that a
// managed runtime depends on (session identity, capability file, deployment
// selector). In shell mode these are also re-asserted via an in-script `export`
// prefix (§4.7) so an env-manager rc (`eval "$(mise env)"`) cannot drop them.
func managedSessionInjectedEnv(paths Paths, record SessionRecord) []string {
	injected := []string{
		"COMMENT_IO_HOME=" + paths.Home,
		"COMMENT_IO_PROFILE=" + record.Profile,
		"COMMENT_IO_BOT_NAME=" + record.BotName,
		"COMMENT_IO_SESSION_ID=" + record.SessionID,
		"COMMENT_IO_SESSION_GENERATION=" + record.Generation,
		"COMMENT_IO_SESSION_CAP_FILE=" + sessionCapabilityPath(paths, record.Profile, record.SessionID, record.Generation),
		"BOTLETS_HOME=" + record.BotletsHome,
	}
	// Forward the daemon-resolved deployment env (COMMENT_IO_ENV + any base-URL
	// override). Nil for production, so production runtimes are unchanged.
	injected = append(injected, RuntimeEnvironmentVars()...)
	return injected
}

func ensureManagedRuntimeTerminalEnv(env []string) []string {
	return normalizeRuntimeTerminalColorEnv(env)
}
