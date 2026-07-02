package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

type runtimeRunOptions struct {
	Home           string
	Runtime        string // user-typed identifier (e.g. "claude"); kept stable for telemetry
	RuntimePath    string // client-resolved absolute path; sent to daemon for execution
	Profile        string
	BotShortcut    string
	Role           string
	CWD            string
	SetupAttemptID string
	RuntimeArgs    []string
	// Detach launches the runtime via the daemon but skips the interactive
	// tmux attach. The daemon keeps servicing the detached session (poller +
	// asyncRewake Stop hook), so the agent still answers @mentions; the caller
	// just doesn't need a TTY. Set by `--detach` or COMMENT_IO_SKIP_ATTACH=1
	// (the latter for headless contexts like the agent-sandbox container, where
	// the launch command is fixed but the environment is controllable).
	Detach bool
}

type runtimeStartResult struct {
	Record   commentbus.TransientRuntimeRecord
	Existing bool
}

type managedSessionStartResult struct {
	Record commentbus.SessionRecord
	Found  bool
}

type configuredManagedRuntime struct {
	Runtime          string
	AllowLegacyRetry bool
}

type cliExitError struct {
	Code int
	// Message, when set, is printed to stderr before exiting with Code. Leave it
	// empty to preserve the historical "exit with this status, print nothing"
	// behavior (e.g. forwarding a child process's own exit code).
	Message string
}

func (err cliExitError) Error() string {
	if err.Message != "" {
		return err.Message
	}
	return fmt.Sprintf("exit status %d", err.Code)
}

// exitTmuxMissing is the dedicated exit status the CLI returns when tmux — which
// is required to host agent runtimes — is not installed. It is intentionally
// distinct from the generic failure code (1) and the doctor check-failure code
// (2) so users and scripts can recognize the missing-tmux condition. The value
// follows sysexits.h EX_UNAVAILABLE ("a required service is unavailable").
const exitTmuxMissing = 69

// tmuxMissingExitError builds the dedicated missing-tmux failure: a clear,
// OS-aware install message paired with the exitTmuxMissing status.
func tmuxMissingExitError() cliExitError {
	return cliExitError{Code: exitTmuxMissing, Message: commentbus.TmuxNotInstalledMessage()}
}

// exitBmuxMissing is the dedicated exit status the CLI returns when bmux — the
// terminal multiplexer that hosts agent runtimes — is not installed. It shares
// the missing-multiplexer code with exitTmuxMissing (sysexits.h EX_UNAVAILABLE)
// so scripts can recognize "a required host is unavailable" regardless of which
// multiplexer the session uses.
const exitBmuxMissing = exitTmuxMissing

// bmuxMissingExitError builds the dedicated missing-bmux failure: a clear install
// message paired with the exitBmuxMissing status.
func bmuxMissingExitError() cliExitError {
	return cliExitError{Code: exitBmuxMissing, Message: commentbus.BmuxNotInstalledMessage()}
}

var (
	runRuntimeCommand         = defaultRunRuntimeCommand
	runRuntimeAttach          = attachRuntimeTmuxSession
	runTransientRuntimeAttach = attachTransientRuntimeSession
	runTransientRuntimeExited = transientRuntimeSessionExited
	runManagedSessionAttach   = attachManagedRuntimeSession
	runRuntimeHas             = runtimeTmuxSessionExists
)

const (
	runtimeExitOutputMaxLines = 80
	runtimeExitOutputMaxBytes = runtimeTailDefaultBytes
	runtimeExitOutputWait     = 500 * time.Millisecond
	managedSessionStartWait   = 2*time.Minute + 30*time.Second
	managedSessionSendWait    = managedSessionStartWait + 30*time.Second
)

var errBotletsRunShortcutNotFound = errors.New("botlets run shortcut not found")
var errBotletsRunStateUnavailable = errors.New("botlets run state unavailable")

type botletsRunShortcutNotFoundError struct {
	selector string
}

func (err botletsRunShortcutNotFoundError) Error() string {
	return fmt.Sprintf("Botlets bot %q is not installed on this computer; run `comment botlets status` to see local bots", err.selector)
}

func (err botletsRunShortcutNotFoundError) Is(target error) bool {
	return target == errBotletsRunShortcutNotFound
}

type botletsRunStateUnavailableError struct {
	message string
}

func (err botletsRunStateUnavailableError) Error() string {
	return err.message
}

func (err botletsRunStateUnavailableError) Is(target error) bool {
	return target == errBotletsRunStateUnavailable
}

func hasRootRuntimeFlag(args []string) bool {
	if len(args) == 0 || !strings.HasPrefix(args[0], "-") {
		return false
	}
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		name, _, ok := flagArgName(arg)
		if !ok {
			continue
		}
		if name == "runtime" {
			return true
		}
	}
	return false
}

func runRuntime(args []string) error {
	options, err := parseRuntimeRunArgs(args)
	if err != nil {
		return err
	}
	return runRuntimeCommand(options)
}

func runRuntimeControl(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: comment runtime status|list|stop")
	}
	switch args[0] {
	case "status":
		return runRuntimeStatus(args[1:])
	case "list":
		return runRuntimeList(args[1:])
	case "stop":
		return runRuntimeStop(args[1:])
	default:
		return fmt.Errorf("unknown runtime command %q", args[0])
	}
}

func parseRuntimeRunArgs(args []string) (runtimeRunOptions, error) {
	options := runtimeRunOptions{}
	passThrough := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			passThrough = append(passThrough, args[i+1:]...)
			break
		}
		name, inlineValue, ok := flagArgName(arg)
		if !ok {
			passThrough = append(passThrough, arg)
			continue
		}
		if name == "detach" {
			if inlineValue {
				return runtimeRunOptions{}, errors.New("--detach does not accept a value; use bare --detach")
			}
			options.Detach = true
			continue
		}
		if !isRuntimeRunValueFlag(name) {
			passThrough = append(passThrough, arg)
			continue
		}
		value, err := consumeRuntimeRunFlagValue(name, arg, inlineValue, args, &i)
		if err != nil {
			return runtimeRunOptions{}, err
		}
		switch name {
		case "home":
			options.Home = value
		case "runtime":
			options.Runtime = value
		case "profile":
			options.Profile = strings.TrimPrefix(value, "@")
		case "cwd":
			options.CWD = value
		case "role":
			options.Role = value
		case "setup-attempt-id":
			options.SetupAttemptID = strings.TrimSpace(value)
		}
	}
	if options.Runtime == "" && options.Profile == "" && len(passThrough) == 1 && !strings.HasPrefix(passThrough[0], "-") {
		options.BotShortcut = strings.TrimPrefix(passThrough[0], "@")
		passThrough = nil
	}
	if options.Runtime == "" && options.Profile == "" && options.BotShortcut == "" && len(passThrough) == 0 {
		options.BotShortcut = "default"
	}
	options.RuntimeArgs = passThrough
	if options.Runtime == "" && options.BotShortcut == "" {
		return runtimeRunOptions{}, errors.New("comment run requires --runtime, an agent profile like `comment run max.reviewer`, or a Botlets bot name like `comment run reviewer`")
	}
	if options.Profile != "" && !commentbus.ProfileRE.MatchString(options.Profile) {
		return runtimeRunOptions{}, errors.New("invalid profile")
	}
	if options.BotShortcut != "" && !commentbus.BotNameRE.MatchString(options.BotShortcut) && !commentbus.ProfileRE.MatchString(options.BotShortcut) {
		return runtimeRunOptions{}, errors.New("invalid Botlets bot name")
	}
	if options.Role == "" {
		options.Role = commentbus.RuntimeRoleMain
	}
	if options.Role != commentbus.RuntimeRoleMain && options.Role != commentbus.RuntimeRoleTask {
		return runtimeRunOptions{}, errors.New("invalid runtime role")
	}
	if options.SetupAttemptID != "" && !botletsSetupAttemptIDRE.MatchString(options.SetupAttemptID) {
		return runtimeRunOptions{}, errors.New("invalid setup attempt id")
	}
	if options.BotShortcut != "" && options.Role != commentbus.RuntimeRoleMain && !commentbus.ProfileRE.MatchString(options.BotShortcut) {
		return runtimeRunOptions{}, errors.New("comment run <bot> only starts the main Botlets session")
	}
	return options, nil
}

func isRuntimeRunValueFlag(name string) bool {
	switch name {
	case "home", "runtime", "profile", "cwd", "role", "setup-attempt-id":
		return true
	default:
		return false
	}
}

func consumeRuntimeRunFlagValue(name string, arg string, inlineValue bool, args []string, index *int) (string, error) {
	var value string
	if inlineValue {
		_, value, _ = strings.Cut(arg, "=")
	} else {
		if *index+1 >= len(args) {
			return "", fmt.Errorf("flag needs an argument: --%s", name)
		}
		*index = *index + 1
		value = args[*index]
	}
	if value == "" {
		return "", fmt.Errorf("flag needs an argument: --%s", name)
	}
	if strings.ContainsAny(value, "\r\n\x00") {
		return "", fmt.Errorf("invalid --%s value", name)
	}
	return value, nil
}

// resolveRuntimeCommandPath turns a `--runtime <value>` arg into an
// absolute filesystem path on the client side, using the caller's PATH.
// The daemon side then receives an absolute path and only has to validate
// ownership and permissions — it doesn't need its own hardcoded list of
// search dirs.
//
//   - `claude`                           → exec.LookPath against the caller's PATH
//   - `/home/user/.local/bin/claude`     → returned as-is (cleaned)
//   - `~/.local/bin/claude`              → expanded to the user's home then cleaned
//   - `./bin/wrapper`                    → resolved against the caller's cwd
//
// The daemon's PATH (under launchd/systemd) typically does not include
// user-local install locations like ~/.local/bin, which is why we resolve
// here instead of letting the daemon do it.
func resolveRuntimeCommandPath(runtime string) (string, error) {
	if runtime == "" {
		return "", errors.New("comment run requires --runtime")
	}
	// Expand `~` first so `~/.local/bin/claude` becomes an absolute path
	// before we decide whether to treat it as a bare name or a filesystem path.
	if strings.HasPrefix(runtime, "~") {
		expanded, err := commentbus.ExpandHome(runtime)
		if err != nil {
			return "", fmt.Errorf("comment run: cannot resolve %q: %w", runtime, err)
		}
		runtime = expanded
	}
	if filepath.IsAbs(runtime) {
		return filepath.Clean(runtime), nil
	}
	if strings.ContainsRune(runtime, filepath.Separator) {
		abs, err := filepath.Abs(runtime)
		if err != nil {
			return "", fmt.Errorf("comment run: cannot resolve %q: %w", runtime, err)
		}
		return abs, nil
	}
	resolved, err := exec.LookPath(runtime)
	if err != nil {
		return "", fmt.Errorf("comment run: cannot find %q on PATH; install it or pass an absolute --runtime path", runtime)
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// shouldSkipRuntimeAttach reports whether `comment run` should launch the
// runtime via the daemon but skip the interactive tmux attach. The daemon keeps
// servicing the detached session (transient runtime poller + asyncRewake Stop
// hook, or the managed-session launcher), so the agent still answers @mentions
// headlessly — the caller just never needs a TTY. Honors the explicit `--detach`
// flag or COMMENT_IO_SKIP_ATTACH=1 (the env form lets headless contexts like the
// agent-sandbox container opt in without changing a fixed launch command).
func shouldSkipRuntimeAttach(options runtimeRunOptions) bool {
	return options.Detach || os.Getenv("COMMENT_IO_SKIP_ATTACH") == "1"
}

// printDetachedRuntimeNotice tells the operator the runtime is up and being
// serviced even though we didn't attach a TTY, and how to attach/inspect it.
// runID is the transient runtime's run id (empty for a managed session, which is
// not stopped via `runtime stop <run-id>`) — `comment runtime stop` requires a
// positional run id, so we only print that hint when we have one.
func printDetachedRuntimeNotice(options runtimeRunOptions, sessionName string, runID string) {
	target := options.Profile
	if target == "" {
		target = options.BotShortcut
	}
	runtime := options.Runtime
	if runtime == "" {
		runtime = "runtime"
	}
	fmt.Printf("Started %s runtime for @%s detached (tmux session %q).\n", runtime, target, sessionName)
	fmt.Println("The daemon keeps servicing it, so it answers @mentions with no TTY attached.")
	fmt.Printf("Attach from a terminal:  comment run --runtime %s --profile %s\n", runtime, target)
	fmt.Printf("Status:                  comment runtime status --profile %s\n", target)
	if runID != "" {
		fmt.Printf("Stop:                    comment runtime stop --profile %s %s\n", target, runID)
	}
}

// runtimeLookPath is exec.LookPath, indirected so tests can stub which coding
// CLIs appear installed.
var runtimeLookPath = exec.LookPath

// detectRunRuntime picks the coding CLI to host the agent when the caller didn't
// pass --runtime and no runtime is saved on the profile/botlet: prefer Claude
// Code, fall back to Codex if only Codex is installed. If neither is found it
// returns "claude" so the existing resolveRuntimeCommandPath "claude not
// installed" error path is unchanged (and `comment doctor` reports the accurate
// "neither claude nor codex" guidance).
func detectRunRuntime() string {
	if _, err := runtimeLookPath("claude"); err == nil {
		return "claude"
	}
	if _, err := runtimeLookPath("codex"); err == nil {
		return "codex"
	}
	return "claude"
}

func defaultRunRuntimeCommand(options runtimeRunOptions) (err error) {
	if options.Profile == "" && options.BotShortcut == "" {
		return errors.New("comment run requires --profile")
	}
	paths, err := resolveCLIPaths(options.Home)
	if err != nil {
		return err
	}
	// Emit failure telemetry on any error return once a profile/paths exist, so a
	// failed `comment run` (e.g. a runtime that can't be resolved/launched) is
	// observable server-side. The success path emits separately. The closure
	// reads the final `options` (BotShortcut resolution below sets Profile/Runtime).
	defer func() {
		if err != nil {
			emitBotletsLocalRuntimeFailed(paths, options, err)
		}
	}()
	if handled, dockerErr := maybeDelegateRuntimeToDocker(context.Background(), paths, options); handled {
		return dockerErr
	}
	if options.BotShortcut != "" {
		if options.Role != commentbus.RuntimeRoleMain && commentbus.ProfileRE.MatchString(options.BotShortcut) {
			profile, profileErr := resolveAgentProfileRunShortcut(paths, options.BotShortcut)
			if profileErr != nil {
				return profileErr
			}
			options.Profile = profile.Handle
			options.Runtime = profile.Runtime
			if options.Runtime == "" {
				options.Runtime = detectRunRuntime()
			}
			options.RuntimeArgs = profileShortcutRuntimeArgs(options.Runtime)
		} else {
			entry, err := resolveBotletsRunShortcut(paths, options.BotShortcut)
			if err == nil {
				options.Profile = entry.Handle
				options.Runtime = entry.ManagedSession.Runtime
				if options.Runtime == "" {
					options.Runtime = detectRunRuntime()
				}
				options.RuntimeArgs = botletsShortcutRuntimeArgs(options.Runtime, entry.Name)
			} else {
				profile, profileErr := resolveAgentProfileRunShortcut(paths, options.BotShortcut)
				if profileErr == nil && (errors.Is(err, errBotletsRunShortcutNotFound) || errors.Is(err, errBotletsRunStateUnavailable)) {
					options.Profile = profile.Handle
					options.Runtime = profile.Runtime
					if options.Runtime == "" {
						options.Runtime = detectRunRuntime()
					}
					options.RuntimeArgs = profileShortcutRuntimeArgs(options.Runtime)
				} else {
					if errors.Is(err, errBotletsRunStateUnavailable) && profileErr != nil && commentbus.ProfileRE.MatchString(strings.TrimPrefix(options.BotShortcut, "@")) {
						return profileErr
					}
					if !errors.Is(err, errBotletsRunShortcutNotFound) {
						return err
					}
					if commentbus.ProfileRE.MatchString(strings.TrimPrefix(options.BotShortcut, "@")) {
						return profileErr
					}
					return err
				}
			}
		}
	}
	// Heads-up once if the chosen CLI isn't logged in — this covers BOTH the
	// managed-session path (which returns before the transient resolution below)
	// and the transient path. Non-fatal: the launch proceeds and the runtime
	// prompts as needed. runtimeAuthState is a no-op for an empty/unknown runtime.
	if authed, hint := runtimeAuthState(options.Runtime); !authed {
		// For an interactive, attached launch render the full readiness panel so
		// the "log in to a coding agent" step is impossible to miss. Headless /
		// detached launches (COMMENT_IO_SKIP_ATTACH, --detach) keep the single
		// stderr line so service logs aren't flooded with an ANSI box redraw on
		// every cold start.
		if !shouldSkipRuntimeAttach(options) && isTerminalFile(os.Stderr) {
			// Scope the panel to the runtime about to launch so it never reports
			// "ready" about a different runtime than the one that's logged out.
			rd := gatherReadiness(paths)
			rd.focusRuntime = options.Runtime
			renderReadinessBox(os.Stderr, rd, colorEnabled(os.Stderr))
		} else {
			fmt.Fprintf(os.Stderr, "Heads-up: %s\n", hint)
		}
	}
	var startResult runtimeStartResult
	if options.Role == "" || options.Role == commentbus.RuntimeRoleMain {
		expectedManagedRuntime := requestedManagedRuntimeName(options.Runtime)
		allowManagedStartLegacyRetry := false
		if configured, ok := configuredManagedSessionRuntime(paths, options.Profile); ok {
			selectedRuntime, err := validateRequestedManagedRuntime(options.Profile, configured.Runtime, options.Runtime)
			if err != nil {
				return err
			}
			expectedManagedRuntime = selectedRuntime
			allowManagedStartLegacyRetry = configured.AllowLegacyRetry
		}
		managedResult, err := startManagedSessionViaDaemon(context.Background(), paths, options.Profile, expectedManagedRuntime, allowManagedStartLegacyRetry)
		if err != nil {
			if isDaemonUnavailableError(err) {
				return errors.New("comment bus daemon is not running; run `comment bus install` or `comment bus start` first")
			}
			return err
		}
		if managedResult.Found {
			if err := validateManagedSessionRuntime(managedResult.Record, options.Runtime); err != nil {
				return err
			}
			emitBotletsLocalRuntimeStarted(paths, options)
			if shouldSkipRuntimeAttach(options) {
				printDetachedRuntimeNotice(options, managedResult.Record.SessionName, "")
				return nil
			}
			attachErr := runManagedSessionAttach(paths, managedResult.Record)
			if attachErr != nil {
				if managedSessionExited(paths, managedResult.Record) {
					return nil
				}
				return attachErr
			}
			return nil
		}
		startResult, err = existingMainRuntimeViaDaemon(context.Background(), paths, options.Profile)
		if err != nil && !isRuntimeStatusUnsupportedError(err) {
			if isDaemonUnavailableError(err) {
				return errors.New("comment bus daemon is not running; run `comment bus install` or `comment bus start` first")
			}
			return err
		}
	}
	// Resolve `--runtime <name>` to an absolute path on the client side.
	// The bus daemon runs under launchd / systemd with a restricted PATH
	// that does not see user-local installs (~/.local/bin/claude, etc.),
	// so resolving here using the caller's interactive PATH avoids the
	// daemon having to maintain a hardcoded allowlist of search dirs.
	// Important: we keep `options.Runtime` (the user-typed identifier
	// like "claude") unchanged for telemetry / pane-current-command
	// matching and only stash the resolved absolute path on the side.
	if startResult.Record.RunID == "" {
		resolvedRuntime, err := resolveRuntimeCommandPath(options.Runtime)
		if err != nil {
			return err
		}
		options.RuntimePath = resolvedRuntime
		cwd, err := resolveRuntimeRunCWDForOptions(paths, options)
		if err != nil {
			return err
		}
		if options.CWD == "" {
			if brainRoot, ok := defaultRuntimeRunBotletsBrainRoot(paths, options.Profile); ok && brainRoot == cwd {
				warnIfBotletsBrainEmpty(options, cwd, true)
			}
		}
		startResult, err = startRuntimeViaDaemon(context.Background(), paths, options, cwd)
		if err != nil {
			if isDaemonUnavailableError(err) {
				return errors.New("comment bus daemon is not running; run `comment bus install` or `comment bus start` first")
			}
			return err
		}
	}
	record := startResult.Record
	if record.State == "exited" {
		printRuntimeExitOutput(record.OutputLogPath)
		return nil
	}
	if !startResult.Existing {
		emitBotletsLocalRuntimeStarted(paths, options)
	}
	if shouldSkipRuntimeAttach(options) {
		printDetachedRuntimeNotice(options, record.SessionName, record.RunID)
		return nil
	}
	attachErr := runTransientRuntimeAttach(paths, record)
	exited := runTransientRuntimeExited(paths, record)
	if exited {
		printRuntimeExitOutput(record.OutputLogPath)
	}
	if attachErr != nil {
		if exited {
			return nil
		}
		if !startResult.Existing {
			_ = stopRuntimeViaDaemon(context.Background(), paths, record.Profile, record.RunID)
		}
		return attachErr
	}
	return nil
}

func resolveBotletsRunShortcut(paths commentbus.Paths, selector string) (commentbus.BotRegistryEntry, error) {
	state, errorsOut := commentbus.LoadProfileState(context.Background(), commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: persistedCLIBotletsHome(paths, ""),
	})
	if commentbus.HasFatalProfileReloadError(errorsOut) {
		return commentbus.BotRegistryEntry{}, botletsRunStateUnavailableError{message: fmt.Sprintf("could not load Botlets bot registry: %+v", errorsOut)}
	}
	if strings.EqualFold(strings.TrimSpace(selector), "default") {
		entry, ok, err := selectManagedDefaultBotletsRunShortcut(state.BotRegistry)
		if err != nil {
			return commentbus.BotRegistryEntry{}, err
		}
		if ok {
			return entry, nil
		}
	}
	entry, ok, err := selectBotletsEntryBySelector(state.BotRegistry, selector)
	if err != nil {
		return commentbus.BotRegistryEntry{}, err
	}
	if !ok {
		return commentbus.BotRegistryEntry{}, botletsRunShortcutNotFoundError{selector: selector}
	}
	if !entry.ManagedSession.Enabled {
		return commentbus.BotRegistryEntry{}, fmt.Errorf("Botlets bot %q is not configured for `comment run <bot>`; run `comment botlets setup --bot %s` again", selector, entry.Handle)
	}
	if entry.ManagedSession.Runtime != "" && entry.ManagedSession.Runtime != "claude" && entry.ManagedSession.Runtime != "codex" {
		return commentbus.BotRegistryEntry{}, fmt.Errorf("Botlets bot %q has invalid runtime %q", selector, entry.ManagedSession.Runtime)
	}
	return entry, nil
}

func selectManagedDefaultBotletsRunShortcut(registry map[string]commentbus.BotRegistryEntry) (commentbus.BotRegistryEntry, bool, error) {
	var matches []commentbus.BotRegistryEntry
	for _, entry := range registry {
		if entry.ManagedSession.Enabled && botletsCurrentDefaultIdentity(entry) {
			matches = append(matches, entry)
		}
	}
	if len(matches) > 1 {
		return commentbus.BotRegistryEntry{}, false, errors.New("Botlets default bot is ambiguous; use the full handle")
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	return commentbus.BotRegistryEntry{}, false, nil
}

// defaultGuideSlugCanonical is the owner-scoped slug of the protected account
// guide Botlet ("Guy the Guide"). "default" is the pre-rebrand legacy slug,
// still recognized so guides provisioned before the rebrand keep resolving.
// Mirrors BOTLETS_DEFAULT_BOT_SLUG / LEGACY_DEFAULT_BOT_SLUGS in
// cf/src/botlets-default-botlet.ts.
const defaultGuideSlugCanonical = "guy"

// isDefaultGuideSlug reports whether a slug/handle-suffix/name names the
// protected account guide Botlet (canonical "guy" or the legacy "default").
func isDefaultGuideSlug(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case defaultGuideSlugCanonical, "default":
		return true
	default:
		return false
	}
}

func botletsCurrentDefaultIdentity(entry commentbus.BotRegistryEntry) bool {
	if isDefaultGuideSlug(entry.Name) {
		return true
	}
	handle := strings.TrimSpace(entry.Handle)
	if isDefaultGuideSlug(handle) {
		return true
	}
	_, suffix, ok := strings.Cut(handle, ".")
	return ok && isDefaultGuideSlug(suffix)
}

func botletsRunShortcutMatches(entry commentbus.BotRegistryEntry, selector string) bool {
	return entry.MatchesSelector(selector)
}

func botletsShortcutRuntimeArgs(runtime string, botName string) []string {
	// Botlets are not Claude subagents — passing the unknown `--agent <bot>` flag
	// makes Claude Code >= 2.1.x hard-error (`--agent '<bot>' not found`). Identity
	// comes from the brain working dir + env injection, so we never pass it (issue
	// #1420). botName is retained for signature symmetry with the codex path; it
	// is no longer part of any Claude command.
	if runtime == "codex" {
		return []string{"--yolo"}
	}
	return []string{"--dangerously-skip-permissions"}
}

func resolveAgentProfileRunShortcut(paths commentbus.Paths, selector string) (commentbus.AgentProfile, error) {
	profileName := strings.TrimPrefix(strings.TrimSpace(selector), "@")
	if !commentbus.ProfileRE.MatchString(profileName) {
		return commentbus.AgentProfile{}, errors.New("not an agent profile selector")
	}
	profiles, errorsOut := commentbus.LoadAgentProfiles(context.Background(), paths, "")
	if commentbus.HasFatalProfileReloadError(errorsOut) {
		return commentbus.AgentProfile{}, fmt.Errorf("could not load agent profiles: %+v", errorsOut)
	}
	profile, ok := profiles[profileName]
	if !ok {
		for _, loadErr := range errorsOut {
			if strings.EqualFold(loadErr.Profile, profileName) {
				return commentbus.AgentProfile{}, fmt.Errorf("agent profile %q could not be loaded: %s", profileName, loadErr.Message)
			}
		}
		return commentbus.AgentProfile{}, fmt.Errorf("agent profile %q was not found under %s", profileName, filepath.Join(paths.Home, "agents"))
	}
	return profile, nil
}

func profileShortcutRuntimeArgs(runtime string) []string {
	if runtime == "codex" {
		return []string{"--yolo"}
	}
	return []string{"--dangerously-skip-permissions"}
}

func emitBotletsLocalRuntimeStarted(paths commentbus.Paths, options runtimeRunOptions) {
	setupAttemptID := strings.TrimSpace(options.SetupAttemptID)
	if setupAttemptID == "" {
		setupAttemptID = strings.TrimSpace(os.Getenv("COMMENT_BOTLETS_SETUP_ATTEMPT_ID"))
	}
	if setupAttemptID == "" || !botletsSetupAttemptIDRE.MatchString(setupAttemptID) || options.Profile == "" {
		return
	}
	state, _ := commentbus.LoadProfileState(context.Background(), commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: persistedCLIBotletsHome(paths, ""),
	})
	profile, ok := state.AgentProfiles[options.Profile]
	if !ok || strings.TrimSpace(profile.BaseURL) == "" {
		return
	}
	botSlug := ""
	for _, entry := range state.BotRegistry {
		if entry.MatchesProfile(options.Profile) {
			botSlug = entry.Name
			break
		}
	}
	emitBotletsSetupTelemetryForRuntime(context.Background(), &http.Client{Timeout: 5 * time.Second}, profile.BaseURL, "botlets_local_runtime_started", map[string]any{
		"setup_attempt_id": setupAttemptID,
		"bot_slug":         botSlug,
		"runtime":          options.Runtime,
		"cli_version":      version,
	})
}

var emitBotletsSetupTelemetryForRuntime = emitBotletsSetupTelemetry

// emitBotletsLocalRuntimeFailed reports a failed `comment run` to the telemetry
// sink. Unlike the success emit it is NOT gated on a setup-attempt id — it fires
// for any invocation whose profile resolves to a base URL — so ordinary
// `comment run` failures (the class that previously left no server-side trace)
// are observable. setup_attempt_id is attached when present. It reuses
// emitBotletsSetupTelemetry, which sends the allowlisted component the sink
// accepts. Best-effort: never affects the returned error.
func emitBotletsLocalRuntimeFailed(paths commentbus.Paths, options runtimeRunOptions, reason error) {
	if reason == nil || options.Profile == "" {
		return
	}
	state, _ := commentbus.LoadProfileState(context.Background(), commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: persistedCLIBotletsHome(paths, ""),
	})
	profile, ok := state.AgentProfiles[options.Profile]
	if !ok || strings.TrimSpace(profile.BaseURL) == "" {
		return
	}
	botSlug := ""
	for _, entry := range state.BotRegistry {
		if entry.Handle == options.Profile {
			botSlug = entry.Name
			break
		}
	}
	data := map[string]any{
		"bot_slug":    botSlug,
		"runtime":     options.Runtime,
		"cli_version": version,
		"reason":      reason.Error(),
	}
	setupAttemptID := strings.TrimSpace(options.SetupAttemptID)
	if setupAttemptID == "" {
		setupAttemptID = strings.TrimSpace(os.Getenv("COMMENT_BOTLETS_SETUP_ATTEMPT_ID"))
	}
	if setupAttemptID != "" && botletsSetupAttemptIDRE.MatchString(setupAttemptID) {
		data["setup_attempt_id"] = setupAttemptID
	}
	emitBotletsSetupTelemetryForRuntime(context.Background(), &http.Client{Timeout: 5 * time.Second}, profile.BaseURL, "botlets_local_runtime_failed", data)
}

func runtimeSessionExited(paths commentbus.Paths, sessionName string) bool {
	live, err := runRuntimeHas(paths, sessionName)
	return err == nil && !live
}

func managedSessionExited(paths commentbus.Paths, record commentbus.SessionRecord) bool {
	if record.Host == commentbus.SessionHostBmux {
		_, err := managedRuntimeBmuxController(paths).PaneCurrentCommand(context.Background(), record.PaneTarget)
		return errors.Is(err, commentbus.ErrTmuxSessionMissing)
	}
	return runtimeSessionExited(paths, record.SessionName)
}

func transientRuntimeSessionExited(paths commentbus.Paths, record commentbus.TransientRuntimeRecord) bool {
	if record.Host == commentbus.SessionHostBmux {
		_, err := runtimeBmuxController(paths, record.BmuxBinary).PaneCurrentCommand(context.Background(), record.PaneTarget)
		return errors.Is(err, commentbus.ErrTmuxSessionMissing)
	}
	return runtimeSessionExited(paths, record.SessionName)
}

func managedRuntimeBmuxController(paths commentbus.Paths) commentbus.ExecBmuxController {
	return runtimeBmuxController(paths, "")
}

func managedRuntimeBmuxResolveInput(paths commentbus.Paths) string {
	pin, serviceExists := installedServiceBmuxConfig(paths)
	return effectiveBmuxResolveInput(pin, serviceExists)
}

func runtimeBmuxController(paths commentbus.Paths, bmuxBinary string) commentbus.ExecBmuxController {
	return commentbus.NewExecBmuxController(paths, runtimeBmuxResolveInput(paths, bmuxBinary))
}

func runtimeBmuxResolveInput(paths commentbus.Paths, bmuxBinary string) string {
	if pin := strings.TrimSpace(bmuxBinary); pin != "" {
		return pin
	}
	return managedRuntimeBmuxResolveInput(paths)
}

func printRuntimeExitOutput(path string) {
	if path == "" {
		return
	}
	data := waitRuntimeExitOutputTail(path)
	_ = os.Remove(path)
	if len(data) == 0 {
		return
	}
	text := string(data)
	lines := strings.SplitAfter(text, "\n")
	if len(lines) > runtimeExitOutputMaxLines {
		lines = lines[len(lines)-runtimeExitOutputMaxLines:]
	}
	output := strings.Join(lines, "")
	if output == "" {
		return
	}
	if !strings.HasPrefix(output, "\n") {
		fmt.Fprintln(os.Stdout)
	}
	fmt.Fprint(os.Stdout, output)
	if !strings.HasSuffix(output, "\n") {
		fmt.Fprintln(os.Stdout)
	}
}

func waitRuntimeExitOutputTail(path string) []byte {
	deadline := time.Now().Add(runtimeExitOutputWait)
	var latest []byte
	for {
		data, exists := readRuntimeExitOutputTail(path, runtimeExitOutputMaxBytes)
		if exists {
			latest = data
		}
		if time.Now().After(deadline) {
			return latest
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func readRuntimeExitOutputTail(path string, maxBytes int64) ([]byte, bool) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false
	}
	if err != nil {
		return nil, true
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, true
	}
	size := info.Size()
	start := int64(0)
	if size > maxBytes {
		start = size - maxBytes
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, true
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes))
	if err != nil {
		return nil, true
	}
	if start > 0 {
		if index := bytes.IndexByte(data, '\n'); index >= 0 && index+1 < len(data) {
			data = data[index+1:]
		}
	}
	return data, true
}

func startRuntimeViaDaemon(ctx context.Context, paths commentbus.Paths, options runtimeRunOptions, cwd string) (runtimeStartResult, error) {
	auth, err := ownerOnlyAuth(paths, options.Profile)
	if err != nil {
		return runtimeStartResult{}, err
	}
	command := append([]string{options.Runtime}, options.RuntimeArgs...)

	// Preferred path: send the original user-typed identifier in
	// `runtime_command[0]` and the client-resolved absolute path in the
	// new `runtime_command_path` field. New daemons use the abs path for
	// trust validation and keep the identifier for telemetry.
	params := map[string]any{
		"profile":         options.Profile,
		"cwd":             cwd,
		"runtime_command": command,
	}
	if options.RuntimePath != "" {
		params["runtime_command_path"] = options.RuntimePath
	}
	if options.Role != "" && options.Role != commentbus.RuntimeRoleMain {
		params["role"] = options.Role
	}
	response, err := callSocket(ctx, paths, "runtime.start", auth, params, 15*time.Second)
	if err != nil {
		return runtimeStartResult{}, err
	}
	// Compatibility fallback: an older daemon validates `runtime.start`
	// with a strict `exactParams` allowlist that does not know about
	// `runtime_command_path`. During CLI/daemon upgrades the launchd
	// process can still be the old binary until `comment bus install`
	// reloads it. If we see the schema rejection, retry with the
	// absolute path baked into `runtime_command[0]` so the old daemon
	// can resolve and validate it directly.
	if options.RuntimePath != "" && shouldRetryWithoutRuntimePath(response) {
		legacyCommand := append([]string{options.RuntimePath}, options.RuntimeArgs...)
		legacyParams := map[string]any{
			"profile":         options.Profile,
			"cwd":             cwd,
			"runtime_command": legacyCommand,
		}
		if options.Role != "" && options.Role != commentbus.RuntimeRoleMain {
			legacyParams["role"] = options.Role
		}
		response, err = callSocket(ctx, paths, "runtime.start", auth, legacyParams, 15*time.Second)
		if err != nil {
			return runtimeStartResult{}, err
		}
	}
	if err := socketResponseError(response); err != nil {
		return runtimeStartResult{}, err
	}
	var decoded struct {
		Runtime  commentbus.TransientRuntimeRecord `json:"runtime"`
		Existing bool                              `json:"existing"`
	}
	data, err := json.Marshal(response.Result)
	if err != nil {
		return runtimeStartResult{}, err
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return runtimeStartResult{}, err
	}
	if decoded.Runtime.RunID == "" || decoded.Runtime.SessionName == "" {
		return runtimeStartResult{}, errors.New("runtime start response was incomplete")
	}
	return runtimeStartResult{Record: decoded.Runtime, Existing: decoded.Existing}, nil
}

func startManagedSessionViaDaemon(ctx context.Context, paths commentbus.Paths, profile string, expectedRuntime string, allowLegacyRetry bool) (managedSessionStartResult, error) {
	auth, err := ownerOnlyAuth(paths, profile)
	if err != nil {
		return managedSessionStartResult{}, err
	}
	params := map[string]any{"profile": profile}
	if expectedRuntime != "" {
		params["expected_runtime"] = expectedRuntime
	}
	response, err := callSocket(ctx, paths, "sessions.start", auth, params, managedSessionStartWait)
	if err != nil {
		return managedSessionStartResult{}, err
	}
	if expectedRuntime != "" && shouldRetryManagedStartWithoutExpectedRuntime(response) {
		if !allowLegacyRetry {
			return managedSessionStartResult{}, nil
		}
		response, err = callSocket(ctx, paths, "sessions.start", auth, map[string]any{"profile": profile}, managedSessionStartWait)
		if err != nil {
			return managedSessionStartResult{}, err
		}
	}
	if !response.OK {
		if shouldFallbackToTransientRuntime(response) {
			return managedSessionStartResult{}, nil
		}
		return managedSessionStartResult{}, socketResponseError(response)
	}
	var decoded struct {
		Session commentbus.SessionRecord `json:"session"`
	}
	data, err := json.Marshal(response.Result)
	if err != nil {
		return managedSessionStartResult{}, err
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return managedSessionStartResult{}, err
	}
	if decoded.Session.SessionID == "" || decoded.Session.SessionName == "" {
		return managedSessionStartResult{}, errors.New("managed session start response was incomplete")
	}
	return managedSessionStartResult{Record: decoded.Session, Found: true}, nil
}

func validateManagedSessionRuntime(record commentbus.SessionRecord, requested string) error {
	_, err := validateRequestedManagedRuntime(record.Profile, record.Runtime, requested)
	return err
}

func validateRequestedManagedRuntime(profile string, configuredRuntime string, requested string) (string, error) {
	requestedRuntime := requestedManagedRuntimeName(requested)
	if requestedRuntime == "" {
		return "", fmt.Errorf("managed session for %s uses runtime %q; cannot attach with --runtime %s", profile, configuredRuntime, requested)
	}
	if configuredRuntime == "" || configuredRuntime == requestedRuntime {
		return requestedRuntime, nil
	}
	return "", fmt.Errorf("managed session for %s uses runtime %q; cannot attach with --runtime %s", profile, configuredRuntime, requested)
}

func requestedManagedRuntimeName(runtime string) string {
	name := filepath.Base(strings.TrimSpace(runtime))
	switch name {
	case "claude", "codex":
		return name
	default:
		return ""
	}
}

func configuredManagedSessionRuntime(paths commentbus.Paths, profile string) (configuredManagedRuntime, bool) {
	state, _ := commentbus.LoadProfileState(context.Background(), commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: persistedCLIBotletsHome(paths, ""),
	})
	for _, entry := range state.BotRegistry {
		if entry.MatchesProfile(profile) && entry.ManagedSession.Enabled {
			profileRuntime := ""
			if profile, ok := state.AgentProfiles[entry.Handle]; ok {
				profileRuntime = profile.Runtime
			}
			return configuredManagedRuntime{
				Runtime:          entry.ManagedSession.Runtime,
				AllowLegacyRetry: shouldAllowManagedStartLegacyRetry(profileRuntime, entry.RegistryRuntime),
			}, true
		}
	}
	return configuredManagedRuntime{}, false
}

func shouldAllowManagedStartLegacyRetry(profileRuntime string, registryRuntime string) bool {
	if profileRuntime == "" {
		return true
	}
	return managedSessionRuntimeForLegacyDaemon(registryRuntime) == profileRuntime
}

func managedSessionRuntimeForLegacyDaemon(registryRuntime string) string {
	switch registryRuntime {
	case "claude", "codex":
		return registryRuntime
	default:
		return "claude"
	}
}

func shouldRetryManagedStartWithoutExpectedRuntime(response commentbus.SocketResponse) bool {
	if response.OK || response.Error == nil || response.Error.Code != "VALIDATION_ERROR" {
		return false
	}
	return strings.Contains(strings.ToLower(response.Error.Message), "unexpected param")
}

func shouldFallbackToTransientRuntime(response commentbus.SocketResponse) bool {
	if response.OK || response.Error == nil {
		return false
	}
	message := strings.ToLower(response.Error.Message)
	switch response.Error.Code {
	case "NOT_FOUND":
		return strings.Contains(message, "bot profile is not loaded")
	case "CONFLICT":
		return strings.Contains(message, "not configured for managed sessions")
	case "VALIDATION_ERROR":
		return strings.Contains(message, "operation")
	default:
		return false
	}
}

func existingMainRuntimeViaDaemon(ctx context.Context, paths commentbus.Paths, profile string) (runtimeStartResult, error) {
	auth, err := ownerOnlyAuth(paths, profile)
	if err != nil {
		return runtimeStartResult{}, err
	}
	response, err := callSocket(ctx, paths, "runtime.status", auth, map[string]any{"profile": profile}, 10*time.Second)
	if err != nil {
		return runtimeStartResult{}, err
	}
	if err := socketResponseError(response); err != nil {
		return runtimeStartResult{}, err
	}
	var decoded struct {
		Runtime *commentbus.TransientRuntimeRecord `json:"runtime"`
	}
	data, err := json.Marshal(response.Result)
	if err != nil {
		return runtimeStartResult{}, err
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return runtimeStartResult{}, err
	}
	if decoded.Runtime == nil || decoded.Runtime.RunID == "" {
		return runtimeStartResult{}, nil
	}
	return runtimeStartResult{Record: *decoded.Runtime, Existing: true}, nil
}

func isRuntimeStatusUnsupportedError(err error) bool {
	var socketErr cliSocketError
	if !errors.As(err, &socketErr) {
		return false
	}
	return socketErr.Code == "VALIDATION_ERROR" && strings.Contains(strings.ToLower(socketErr.Message), "operation")
}

func parseRuntimeControlProfileArgs(command string, args []string) (string, string, []string, error) {
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	profile := fs.String("profile", "", "profile handle")
	if err := fs.Parse(args); err != nil {
		return "", "", nil, err
	}
	if *profile == "" {
		return "", "", nil, fmt.Errorf("%s requires --profile", command)
	}
	selectedProfile := strings.TrimPrefix(*profile, "@")
	if !commentbus.ProfileRE.MatchString(selectedProfile) {
		return "", "", nil, errors.New("invalid profile")
	}
	return *home, selectedProfile, fs.Args(), nil
}

func callRuntimeControlOperation(ctx context.Context, home string, profile string, op string, params map[string]any) (commentbus.SocketResponse, error) {
	paths, err := resolveCLIPaths(home)
	if err != nil {
		return commentbus.SocketResponse{}, err
	}
	auth, err := ownerOnlyAuth(paths, profile)
	if err != nil {
		return commentbus.SocketResponse{}, err
	}
	response, err := callSocket(ctx, paths, op, auth, params, 10*time.Second)
	if err != nil {
		if isDaemonUnavailableError(err) {
			return commentbus.SocketResponse{}, errors.New("comment bus daemon is not running; run `comment bus install` or `comment bus start` first")
		}
		return commentbus.SocketResponse{}, err
	}
	if err := socketResponseError(response); err != nil {
		return commentbus.SocketResponse{}, err
	}
	return response, nil
}

func runRuntimeStatus(args []string) error {
	home, profile, rest, err := parseRuntimeControlProfileArgs("comment runtime status", args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errors.New("comment runtime status does not accept positional arguments")
	}
	response, err := callRuntimeControlOperation(context.Background(), home, profile, "runtime.status", map[string]any{"profile": profile})
	if err != nil {
		return err
	}
	return printJSON(response.Result)
}

func runRuntimeList(args []string) error {
	home, profile, rest, err := parseRuntimeControlProfileArgs("comment runtime list", args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errors.New("comment runtime list does not accept positional arguments")
	}
	response, err := callRuntimeControlOperation(context.Background(), home, profile, "runtime.list", map[string]any{"profile": profile})
	if err != nil {
		return err
	}
	return printJSON(response.Result)
}

func runRuntimeStop(args []string) error {
	home, profile, rest, err := parseRuntimeControlProfileArgs("comment runtime stop", args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("comment runtime stop requires exactly one run-id")
	}
	runID := rest[0]
	if !commentbus.LocalSessionIDRE.MatchString(runID) {
		return errors.New("invalid run-id")
	}
	response, err := callRuntimeControlOperation(context.Background(), home, profile, "runtime.stop", map[string]any{"run_id": runID})
	if err != nil {
		return err
	}
	return printJSON(response.Result)
}

// shouldRetryWithoutRuntimePath reports whether a runtime.start response
// looks like an older daemon rejecting our new `runtime_command_path`
// field. The daemon validates params via `exactParams`, which returns
// VALIDATION_ERROR with "unexpected param" when it sees an unknown key.
func shouldRetryWithoutRuntimePath(response commentbus.SocketResponse) bool {
	if response.OK || response.Error == nil {
		return false
	}
	if response.Error.Code != "VALIDATION_ERROR" {
		return false
	}
	return strings.Contains(response.Error.Message, "unexpected param")
}

func stopRuntimeViaDaemon(ctx context.Context, paths commentbus.Paths, profile string, runID string) error {
	auth, err := ownerOnlyAuth(paths, profile)
	if err != nil {
		return err
	}
	response, err := callSocket(ctx, paths, "runtime.stop", auth, map[string]any{"run_id": runID}, 10*time.Second)
	if err != nil {
		return err
	}
	return socketResponseError(response)
}

func attachManagedRuntimeSession(paths commentbus.Paths, record commentbus.SessionRecord) error {
	if record.Host == commentbus.SessionHostBmux {
		return attachRuntimeBmuxSession(paths, record)
	}
	return runRuntimeAttach(paths, record.SessionName)
}

func attachTransientRuntimeSession(paths commentbus.Paths, record commentbus.TransientRuntimeRecord) error {
	if record.Host == commentbus.SessionHostBmux {
		return attachRuntimeBmuxSessionWithBinary(paths, sessionRecordForTransientRuntime(record), record.BmuxBinary)
	}
	return runRuntimeAttach(paths, record.SessionName)
}

func sessionRecordForTransientRuntime(record commentbus.TransientRuntimeRecord) commentbus.SessionRecord {
	return commentbus.SessionRecord{
		Host:        record.Host,
		SessionName: record.SessionName,
		PaneTarget:  record.PaneTarget,
		Runtime:     record.Runtime,
	}
}

// clientTmuxBinary is the tmux binary the interactive attach / has-session
// commands should exec. It honors an explicit COMMENT_IO_TMUX_BIN pin -- the
// same override the missing-tmux guidance tells users to set, and the same one
// the daemon may have used to start the session -- so that guidance actually
// works for attaching. With no pin it falls back to the literal "tmux" on PATH,
// preserving prior behavior; genuine absence is still detected via
// exec.ErrNotFound at run time and mapped to the dedicated missing-tmux exit.
func clientTmuxBinary(paths commentbus.Paths) string {
	// An explicit COMMENT_IO_TMUX_BIN from the caller's shell wins first — it is
	// the same override the missing-tmux guidance tells users to set, and an
	// intentional choice for this attach.
	if pin := strings.TrimSpace(os.Getenv(commentbus.TmuxBinaryEnv)); pin != "" {
		return pin
	}
	// Otherwise, when an installed service owns the *selected* home, resolve tmux
	// exactly the way that daemon does — a baked COMMENT_IO_TMUX_BIN pin, else bare
	// "tmux" from the trusted directories — so the client attaches with the same
	// binary the daemon used to create the session, even when it is not on the
	// caller's PATH (or PATH has a tmux shim). This is the tmux equivalent of
	// managedRuntimeBmuxResolveInput on the bmux path, and matters now that tmux is
	// the default host.
	if pin, serviceExists := installedServiceTmuxConfig(paths); serviceExists {
		if resolved, err := commentbus.ResolveDaemonTmuxBinary(effectiveTmuxResolveInput(pin, serviceExists)); err == nil {
			return resolved
		}
	}
	// No installed service (foreground/shell context), or daemon-trusted resolution
	// failed: fall back to bare "tmux" on PATH, preserving prior behavior; genuine
	// absence is still detected via exec.ErrNotFound and mapped to the dedicated
	// missing-tmux exit.
	return "tmux"
}

func attachRuntimeBmuxSession(paths commentbus.Paths, record commentbus.SessionRecord) error {
	return attachRuntimeBmuxSessionWithBinary(paths, record, "")
}

func attachRuntimeBmuxSessionWithBinary(paths commentbus.Paths, record commentbus.SessionRecord, bmuxBinary string) error {
	if record.Host != commentbus.SessionHostBmux {
		return errors.New("runtime session is not a bmux session")
	}
	if !commentbus.TmuxSessionNameRE.MatchString(record.SessionName) {
		return errors.New("invalid runtime bmux session")
	}
	if record.PaneTarget == "" {
		return errors.New("runtime bmux socket is not available")
	}
	socketPath, err := commentbus.BmuxSocketPathForSession(paths, record.SessionName)
	if err != nil {
		return err
	}
	if record.PaneTarget != socketPath {
		return errors.New("runtime bmux socket does not match session")
	}
	tokenFile, err := commentbus.BmuxTokenFileForSession(paths, record.SessionName)
	if err != nil {
		return err
	}
	bmuxBinaryPath, err := commentbus.TrustedBmuxBinaryPath(runtimeBmuxResolveInput(paths, bmuxBinary))
	if err != nil {
		return err
	}
	command := exec.Command(bmuxBinaryPath, "attach", "-S", socketPath)
	command.Env = commentbus.BmuxClientEnv(os.Environ(), tokenFile)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return cliExitError{Code: exitErr.ExitCode()}
		}
		return err
	}
	return nil
}

func attachRuntimeTmuxSession(paths commentbus.Paths, sessionName string) error {
	if !commentbus.TmuxSessionNameRE.MatchString(sessionName) {
		return errors.New("invalid runtime tmux session")
	}
	socketName, err := commentbus.TmuxSocketNameForSession(sessionName)
	if err != nil {
		return err
	}
	args := []string{"-L", socketName, "attach-session", "-t", sessionName}
	env := os.Environ()
	if os.Getenv("TMUX") != "" {
		if currentTmuxSocketName() == socketName {
			args = []string{"-L", socketName, "switch-client", "-t", sessionName}
		} else {
			env = withoutEnvKey(env, "TMUX")
		}
	}
	command := exec.Command(clientTmuxBinary(paths), args...)
	command.Env = env
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		// tmux not on PATH for the interactive attach (the daemon resolves tmux
		// from trusted dirs, but attach runs tmux directly). Surface the same
		// clear install guidance and dedicated exit code.
		if errors.Is(err, exec.ErrNotFound) {
			return tmuxMissingExitError()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return cliExitError{Code: exitErr.ExitCode()}
		}
		return err
	}
	return nil
}

func runtimeTmuxSessionExists(paths commentbus.Paths, sessionName string) (bool, error) {
	if !commentbus.TmuxSessionNameRE.MatchString(sessionName) {
		return false, errors.New("invalid runtime tmux session")
	}
	socketName, err := commentbus.TmuxSocketNameForSession(sessionName)
	if err != nil {
		return false, err
	}
	command := exec.Command(clientTmuxBinary(paths), "-L", socketName, "has-session", "-t", sessionName)
	out, err := command.CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return false, tmuxMissingExitError()
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && runtimeTmuxHasSessionMissingOutput(string(out)) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func runtimeTmuxHasSessionMissingOutput(output string) bool {
	lower := strings.ToLower(output)
	return strings.Contains(lower, "can't find session") ||
		strings.Contains(lower, "can't find pane") ||
		strings.Contains(lower, "can't find window") ||
		strings.Contains(lower, "no server running") ||
		strings.Contains(lower, "no such file or directory")
}

func currentTmuxSocketName() string {
	value := os.Getenv("TMUX")
	if value == "" {
		return ""
	}
	socketPath := strings.SplitN(value, ",", 2)[0]
	if socketPath == "" {
		return ""
	}
	return filepath.Base(socketPath)
}

func withoutEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := env[:0]
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func resolveRuntimeRunCWD(value string) (string, error) {
	if value == "" {
		return os.Getwd()
	}
	resolved, err := expandRuntimeRunPath(value)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("invalid cwd: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("invalid cwd: not a directory")
	}
	return abs, nil
}

func resolveRuntimeRunCWDForOptions(paths commentbus.Paths, options runtimeRunOptions) (string, error) {
	if options.CWD != "" {
		return resolveRuntimeRunCWD(options.CWD)
	}
	if brainRoot, ok := defaultRuntimeRunBotletsBrainRoot(paths, options.Profile); ok {
		return brainRoot, nil
	}
	return resolveRuntimeRunCWD("")
}

func defaultRuntimeRunBotletsBrainRoot(paths commentbus.Paths, profile string) (string, bool) {
	if profile == "" {
		return "", false
	}
	state, _ := commentbus.LoadProfileState(context.Background(), commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: persistedCLIBotletsHome(paths, ""),
	})
	return commentbus.BotletsBrainRootForProfile(paths, state, profile)
}

// warnIfBotletsBrainEmpty surfaces a loud warning to stderr when a Botlets bot
// is about to launch from a brain projection directory that contains no persona
// documents (bug #531). An empty brain means the bot would silently fall back
// to inheriting the human operator's ~/.claude/CLAUDE.md instead of its own
// persona — the exact symptom of the brain-sync collision. The warning is only
// emitted when the launch is implicitly anchored to a Botlets brain root (i.e.
// the user did not pass an explicit --cwd) so manual runs are never nagged.
func warnIfBotletsBrainEmpty(options runtimeRunOptions, cwd string, isBrainRoot bool) {
	if options.CWD != "" || !isBrainRoot || cwd == "" {
		return
	}
	if botletsBrainHasPersonaDocs(cwd) {
		return
	}
	fmt.Fprintf(os.Stderr, "comment run: WARNING the Botlets brain at %q has no persona documents — the bot will launch WITHOUT its synced brain and may inherit your personal ~/.claude/CLAUDE.md. Run `comment sync` to repair the brain projection, then relaunch (see bug #531).\n", cwd)
}

// botletsBrainHasPersonaDocs reports whether the brain directory contains at
// least one persona markdown document. Hidden/dot files and non-markdown
// scaffolding (e.g. README, the local agent docs folder) are ignored.
func botletsBrainHasPersonaDocs(brainRoot string) bool {
	entries, err := os.ReadDir(brainRoot)
	if err != nil {
		// On any read error, do not raise a false alarm; the launch path has
		// other validation and we only want to flag the unambiguous empty case.
		return true
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if entry.IsDir() {
			continue
		}
		// Generated scaffolding (e.g. README.md) is not a persona document; a
		// brain emptied down to only infra files is still effectively empty and
		// must still raise the warning.
		if strings.EqualFold(name, "README.md") {
			continue
		}
		if strings.EqualFold(filepath.Ext(name), ".md") {
			return true
		}
	}
	return false
}

func expandRuntimeRunPath(value string) (string, error) {
	if value == "~" || strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if value == "~" {
			return home, nil
		}
		return filepath.Join(home, value[2:]), nil
	}
	return value, nil
}

func runtimeRunEnv(base []string, home string, profile string) []string {
	out := make([]string, 0, len(base)+2)
	skip := map[string]struct{}{
		"COMMENT_IO_HOME":               {},
		"COMMENT_IO_PROFILE":            {},
		"COMMENT_IO_BOT_NAME":           {},
		"COMMENT_IO_SESSION_ID":         {},
		"COMMENT_IO_SESSION_GENERATION": {},
		"COMMENT_IO_SESSION_CAP_FILE":   {},
		"COMMENT_IO_RUNTIME_RUN":        {},
	}
	for _, entry := range base {
		key := runtimeRunEnvKey(entry)
		if _, ok := skip[key]; ok {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, "COMMENT_IO_HOME="+home)
	if profile != "" {
		out = append(out, "COMMENT_IO_PROFILE="+profile)
	}
	return out
}

func runtimeRunEnvKey(entry string) string {
	key, _, ok := strings.Cut(entry, "=")
	if !ok {
		return entry
	}
	return key
}
