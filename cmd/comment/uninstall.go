package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
	"github.com/comment-hq/comment-cli/internal/commentsync"
)

const uninstallCLIPackageName = "@comment-io/cli"

var uninstallCombinedOutput = func(ctx context.Context, command string, args ...string) ([]byte, error) {
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

var uninstallLookPath = exec.LookPath

type uninstallTmuxController interface {
	KillSession(context.Context, string) error
}

var newUninstallTmuxController = func(tmuxBinary string) uninstallTmuxController {
	return commentbus.ExecTmuxController{Binary: tmuxBinary}
}

type uninstallOptions struct {
	Home         string
	BotletsHome  string
	NPM          string
	Yes          bool
	DryRun       bool
	SkipCLI      bool
	SkipPlugins  bool
	KeepSyncRoot bool
}

type uninstallAction struct {
	Name    string   `json:"name"`
	Status  string   `json:"status"`
	Path    string   `json:"path,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	Args    []string `json:"args,omitempty"`
	Message string   `json:"message,omitempty"`
	Error   string   `json:"error,omitempty"`
	Detail  any      `json:"detail,omitempty"`
}

type uninstallResult struct {
	OK                   bool              `json:"ok"`
	DryRun               bool              `json:"dry_run"`
	ConfirmationRequired bool              `json:"confirmation_required,omitempty"`
	Confirmed            bool              `json:"confirmed,omitempty"`
	Home                 string            `json:"home"`
	BotletsHome          string            `json:"botlets_home"`
	SyncRoot             string            `json:"sync_root,omitempty"`
	Actions              []uninstallAction `json:"actions"`
	Message              string            `json:"message,omitempty"`
}

func runUninstall(args []string) error {
	options := uninstallOptions{NPM: "npm"}
	fs := flag.NewFlagSet("comment uninstall", flag.ContinueOnError)
	fs.StringVar(&options.Home, "home", "", "Comment.io home directory")
	fs.StringVar(&options.BotletsHome, "botlets-home", "", "Botlets home directory")
	fs.StringVar(&options.NPM, "npm", "npm", "npm binary path")
	fs.BoolVar(&options.Yes, "yes", false, "confirm destructive uninstall without prompting")
	fs.BoolVar(&options.DryRun, "dry-run", false, "print the uninstall plan without removing anything")
	fs.BoolVar(&options.SkipCLI, "skip-cli", false, "leave the global @comment-io/cli npm package installed")
	fs.BoolVar(&options.SkipPlugins, "skip-plugins", false, "leave Claude/OpenClaw plugin caches and skills alone")
	fs.BoolVar(&options.KeepSyncRoot, "keep-sync-root", false, "do not purge generated files from the local Comment Docs sync root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("uninstall does not accept positional arguments")
	}
	result, err := newUninstallResult(options)
	if err != nil {
		return err
	}
	if options.DryRun {
		result.DryRun = true
		result.Message = "Dry run only. Re-run with --yes to remove Comment.io and Botlets local state."
		addUninstallPlanActions(result, options)
		return printJSON(result)
	}
	if !options.Yes {
		if !stdinIsInteractive() {
			result.OK = false
			result.DryRun = true
			result.ConfirmationRequired = true
			result.Message = "Refusing to uninstall without confirmation. Re-run with --yes, or use --dry-run to inspect the plan."
			addUninstallPlanActions(result, options)
			if err := printJSON(result); err != nil {
				return err
			}
			return cliExitError{Code: 2}
		}
		if !confirmUninstall(result.Home, result.BotletsHome) {
			result.OK = false
			result.Message = "Uninstall canceled."
			if err := printJSON(result); err != nil {
				return err
			}
			return cliExitError{Code: 2}
		}
	}
	result.Confirmed = true
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	performUninstall(ctx, result, options)
	if err := printJSON(result); err != nil {
		return err
	}
	if !result.OK {
		return cliExitError{Code: 1}
	}
	return nil
}

func newUninstallResult(options uninstallOptions) (*uninstallResult, error) {
	paths, err := resolveCLIPaths(options.Home)
	if err != nil {
		return nil, err
	}
	botletsHome, err := resolveUninstallBotletsHome(paths, options.BotletsHome)
	if err != nil {
		return nil, err
	}
	result := &uninstallResult{
		OK:          true,
		Home:        paths.Home,
		BotletsHome: botletsHome,
	}
	status, err := commentsync.ReadStatus(commentsync.Options{Home: paths.Home})
	if err != nil {
		return nil, fmt.Errorf("read library sync status: %w", err)
	}
	if status.Configured {
		result.SyncRoot = status.Root
	}
	if options.KeepSyncRoot && result.SyncRoot != "" {
		if uninstallPathContains(result.Home, result.SyncRoot) {
			return nil, fmt.Errorf("--keep-sync-root cannot preserve %s because it is inside Comment.io home %s", result.SyncRoot, result.Home)
		}
		if uninstallPathContains(result.BotletsHome, result.SyncRoot) {
			return nil, fmt.Errorf("--keep-sync-root cannot preserve %s because it is inside Botlets home %s", result.SyncRoot, result.BotletsHome)
		}
	}
	return result, nil
}

func resolveUninstallBotletsHome(paths commentbus.Paths, explicit string) (string, error) {
	if explicit != "" {
		return commentbus.ResolveBotletsHome(explicit)
	}
	if config, ok, err := commentbus.ReadBusConfig(paths); err != nil {
		return "", fmt.Errorf("read botlets home from bus config: %w", err)
	} else if ok && config.BotletsHome != "" {
		return config.BotletsHome, nil
	}
	return commentbus.ResolveBotletsHome("")
}

func addUninstallPlanActions(result *uninstallResult, options uninstallOptions) {
	paths, err := commentbus.ResolvePaths(result.Home)
	if err != nil {
		appendUninstallAction(result, uninstallAction{Name: "resolve_paths", Status: "error", Error: err.Error()})
		return
	}
	sessionCount := 0
	if records, err := commentbus.ListSessionRecords(paths); err == nil {
		sessionCount = len(records)
	}
	appendUninstallAction(result, uninstallAction{
		Name:    "stop_managed_sessions",
		Status:  "planned",
		Message: fmt.Sprintf("would stop %d managed session(s)", sessionCount),
	})
	if options.KeepSyncRoot {
		appendUninstallAction(result, uninstallAction{Name: "purge_sync_root", Status: "skipped", Path: result.SyncRoot, Message: "--keep-sync-root set"})
	} else if result.SyncRoot != "" {
		appendUninstallAction(result, uninstallAction{Name: "purge_sync_root", Status: "planned", Path: result.SyncRoot})
	}
	appendUninstallAction(result, uninstallAction{Name: "uninstall_daemon", Status: "planned", Path: result.Home})
	appendUninstallAction(result, uninstallAction{Name: "reclaim_daemon_socket", Status: "planned", Path: paths.Socket})
	appendUninstallAction(result, uninstallAction{Name: "remove_comment_home", Status: "planned", Path: result.Home})
	appendUninstallAction(result, uninstallAction{Name: "remove_botlets_home", Status: "planned", Path: result.BotletsHome})
	addPluginPlanActions(result, options)
	addCLIPlanAction(result, options)
}

func performUninstall(ctx context.Context, result *uninstallResult, options uninstallOptions) {
	paths, err := commentbus.ResolvePaths(result.Home)
	if err != nil {
		appendUninstallAction(result, uninstallAction{Name: "resolve_paths", Status: "error", Error: err.Error()})
		return
	}
	action := stopManagedSessionsForUninstall(ctx, paths)
	appendUninstallAction(result, action)
	if action.Status == "error" {
		return
	}
	if options.KeepSyncRoot {
		appendUninstallAction(result, uninstallAction{Name: "purge_sync_root", Status: "skipped", Path: result.SyncRoot, Message: "--keep-sync-root set"})
	} else {
		action = purgeSyncRootForUninstall(ctx, paths)
		appendUninstallAction(result, action)
		if action.Status == "error" {
			return
		}
	}
	action = uninstallDaemonForUninstall(paths)
	appendUninstallAction(result, action)
	if action.Status == "error" {
		return
	}
	appendUninstallAction(result, reclaimSocketForUninstall(paths))
	removed := map[string]bool{}
	appendUninstallAction(result, removeTreeForUninstall("remove_comment_home", result.Home, removed))
	appendUninstallAction(result, removeTreeForUninstall("remove_botlets_home", result.BotletsHome, removed))
	if options.SkipPlugins {
		appendUninstallAction(result, uninstallAction{Name: "remove_plugins", Status: "skipped", Message: "--skip-plugins set"})
	} else {
		removePluginFilesForUninstall(result, removed)
		runPluginCommandsForUninstall(ctx, result)
	}
	if options.SkipCLI {
		appendUninstallAction(result, uninstallAction{Name: "uninstall_cli", Status: "skipped", Message: "--skip-cli set"})
	} else {
		appendUninstallAction(result, uninstallCLIForUninstall(ctx, options.NPM))
	}
}

func appendUninstallAction(result *uninstallResult, action uninstallAction) {
	if action.Status == "" {
		action.Status = "ok"
	}
	if action.Status == "error" {
		result.OK = false
	}
	result.Actions = append(result.Actions, action)
}

func stopManagedSessionsForUninstall(ctx context.Context, paths commentbus.Paths) uninstallAction {
	records, err := commentbus.ListSessionRecords(paths)
	if err != nil {
		return uninstallAction{Name: "stop_managed_sessions", Status: "error", Error: err.Error()}
	}
	if len(records) == 0 {
		return uninstallAction{Name: "stop_managed_sessions", Status: "skipped", Message: "no managed sessions recorded"}
	}
	auth, authErr := ownerOnlyAuth(paths, "")
	// Fallback session cleanup may run after the daemon socket is gone, so it
	// needs the host binaries the daemon actually used. Mirror doctor: a service
	// pin wins; an installed-but-unpinned service forces trusted-dir
	// auto-discovery, bypassing shell env pins the service did not inherit.
	serviceTmuxPin, serviceExists := installedServiceTmuxConfig(paths)
	tmux := newUninstallTmuxController(effectiveTmuxResolveInput(serviceTmuxPin, serviceExists))
	serviceBmuxPin, bmuxServiceExists := installedServiceBmuxConfig(paths)
	bmux := commentbus.NewExecBmuxController(paths, effectiveBmuxResolveInput(serviceBmuxPin, bmuxServiceExists))
	stopped := 0
	killed := 0
	missing := 0
	var failures []string
	for _, record := range records {
		if authErr == nil {
			response, err := callSocket(ctx, paths, "sessions.stop", auth, map[string]any{
				"profile":    record.Profile,
				"session_id": record.SessionID,
				"reason":     "uninstall",
			}, 5*time.Second)
			if err == nil && response.OK {
				stopped++
				continue
			}
		}
		if record.SessionName == "" {
			failures = append(failures, record.SessionID+": missing session name")
			continue
		}
		controller := uninstallTmuxController(tmux)
		if record.Host == commentbus.SessionHostBmux {
			controller = bmux
		}
		if err := controller.KillSession(ctx, record.SessionName); err == nil {
			killed++
		} else if errors.Is(err, commentbus.ErrTmuxSessionMissing) {
			missing++
		} else {
			failures = append(failures, record.SessionName+": "+err.Error())
		}
	}
	action := uninstallAction{
		Name:   "stop_managed_sessions",
		Status: "removed",
		Detail: map[string]any{
			"records":            len(records),
			"stopped_via_daemon": stopped,
			"killed_tmux":        killed,
			"already_missing":    missing,
		},
	}
	if len(failures) > 0 {
		action.Status = "error"
		action.Error = strings.Join(failures, "; ")
	}
	return action
}

func purgeSyncRootForUninstall(ctx context.Context, paths commentbus.Paths) uninstallAction {
	status, err := commentsync.ReadStatus(commentsync.Options{Home: paths.Home})
	if err != nil {
		return uninstallAction{Name: "purge_sync_root", Status: "error", Error: err.Error()}
	}
	if !status.Configured {
		return uninstallAction{Name: "purge_sync_root", Status: "skipped", Message: "library sync is not configured"}
	}
	logout, err := commentsync.Logout(ctx, commentsync.Options{Home: paths.Home, PurgeLocal: true})
	if err != nil {
		return uninstallAction{Name: "purge_sync_root", Status: "error", Path: status.Root, Error: err.Error()}
	}
	return uninstallAction{Name: "purge_sync_root", Status: "removed", Path: status.Root, Detail: logout}
}

func uninstallDaemonForUninstall(paths commentbus.Paths) uninstallAction {
	result, err := busUninstall(paths.Home)
	if err != nil {
		return uninstallAction{Name: "uninstall_daemon", Status: "error", Path: paths.Home, Error: err.Error()}
	}
	return uninstallAction{Name: "uninstall_daemon", Status: "removed", Path: paths.Home, Detail: result}
}

func reclaimSocketForUninstall(paths commentbus.Paths) uninstallAction {
	if err := reclaimDaemonSocket(paths.Socket); err != nil {
		return uninstallAction{Name: "reclaim_daemon_socket", Status: "warning", Path: paths.Socket, Error: err.Error()}
	}
	return uninstallAction{Name: "reclaim_daemon_socket", Status: "removed", Path: paths.Socket}
}

func removeTreeForUninstall(name string, path string, removed map[string]bool) uninstallAction {
	clean, err := cleanUninstallRemovalPath(path)
	if err != nil {
		return uninstallAction{Name: name, Status: "error", Path: path, Error: err.Error()}
	}
	if removed[clean] {
		return uninstallAction{Name: name, Status: "skipped", Path: clean, Message: "already removed"}
	}
	info, err := os.Lstat(clean)
	if errors.Is(err, os.ErrNotExist) {
		return uninstallAction{Name: name, Status: "skipped", Path: clean, Message: "path does not exist"}
	}
	if err != nil {
		return uninstallAction{Name: name, Status: "error", Path: clean, Error: err.Error()}
	}
	if err := validateUninstallRemovalOwner(info); err != nil {
		return uninstallAction{Name: name, Status: "error", Path: clean, Error: fmt.Sprintf("refusing to remove path: %v", err)}
	}
	if err := os.RemoveAll(clean); err != nil {
		return uninstallAction{Name: name, Status: "error", Path: clean, Error: err.Error()}
	}
	removed[clean] = true
	return uninstallAction{Name: name, Status: "removed", Path: clean}
}

func cleanUninstallRemovalPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("empty path")
	}
	clean, err := commentbus.ExpandHome(path)
	if err != nil {
		return "", err
	}
	if clean == filepath.Clean(string(os.PathSeparator)) {
		return "", errors.New("refusing to remove filesystem root")
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(clean) == filepath.Clean(home) {
		return "", errors.New("refusing to remove user home directory")
	}
	return filepath.Clean(clean), nil
}

func uninstallPathContains(parent string, child string) bool {
	parent = filepath.Clean(parent)
	child = filepath.Clean(child)
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func addPluginPlanActions(result *uninstallResult, options uninstallOptions) {
	if options.SkipPlugins {
		appendUninstallAction(result, uninstallAction{Name: "remove_plugins", Status: "skipped", Message: "--skip-plugins set"})
		return
	}
	paths := pluginRemovalPathsForUninstall()
	appendUninstallAction(result, uninstallAction{Name: "remove_plugin_files", Status: "planned", Paths: paths})
	appendUninstallAction(result, uninstallAction{Name: "uninstall_claude_plugins", Status: "planned", Args: claudePluginUninstallCommandsForPlan()})
}

func removePluginFilesForUninstall(result *uninstallResult, removed map[string]bool) {
	for _, path := range pluginRemovalPathsForUninstall() {
		appendUninstallAction(result, removeTreeForUninstall("remove_plugin_files", path, removed))
	}
}

func pluginRemovalPathsForUninstall() []string {
	var paths []string
	if claudeHome, err := resolveClaudeHome(""); err == nil {
		paths = append(paths,
			filepath.Join(claudeHome, "skills", "comment"),
			filepath.Join(claudeHome, "plugins", "cache", "comment-io-plugins", "comment-io"),
			// Successive generations of GitHub org names own these cache paths:
			// botspring-ai (oldest), botlets-ai, comment-io (interim), and
			// comment-hq (current). Remove all of them so uninstall is clean
			// regardless of which org the plugin was installed under.
			filepath.Join(claudeHome, "plugins", "cache", "botspring-ai", "comment-io-claude-code-plugin"),
			filepath.Join(claudeHome, "plugins", "cache", "botspring-ai", "botspring-claude-code-plugin"),
			filepath.Join(claudeHome, "plugins", "cache", "botlets-ai", "comment-io-claude-code-plugin"),
			filepath.Join(claudeHome, "plugins", "cache", "botlets-ai", "botspring-claude-code-plugin"),
			filepath.Join(claudeHome, "plugins", "cache", "comment-io", "comment-io-claude-code-plugin"),
			filepath.Join(claudeHome, "plugins", "cache", "comment-io", "botspring-claude-code-plugin"),
			filepath.Join(claudeHome, "plugins", "cache", "comment-hq", "comment-io-claude-code-plugin"),
			filepath.Join(claudeHome, "plugins", "cache", "comment-hq", "botspring-claude-code-plugin"),
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".openclaw", "skills", "comment"))
	}
	return paths
}

func claudePluginUninstallCommandsForPlan() []string {
	var commands []string
	for _, args := range claudePluginUninstallArgs() {
		commands = append(commands, "claude "+strings.Join(args, " "))
	}
	return commands
}

func claudePluginUninstallArgs() [][]string {
	return [][]string{
		{"plugin", "uninstall", "comment-io@comment-io-plugins"},
		// Plugin marketplace identity flips at §4 (repo rename + republish),
		// not here: keep botspring@botspring-plugins until the published
		// marketplace is renamed so `comment uninstall` targets the currently
		// installed plugin.
		{"plugin", "uninstall", "botspring@botspring-plugins"},
		{"plugin", "marketplace", "remove", "comment-io-plugins"},
		{"plugin", "marketplace", "remove", "botspring-plugins"},
	}
}

func runPluginCommandsForUninstall(ctx context.Context, result *uninstallResult) {
	claudeBin, err := uninstallLookPath("claude")
	if err != nil {
		appendUninstallAction(result, uninstallAction{Name: "uninstall_claude_plugins", Status: "skipped", Message: "claude not found on PATH"})
		return
	}
	for _, args := range claudePluginUninstallArgs() {
		action := runUninstallExternalCommand(ctx, "uninstall_claude_plugins", claudeBin, args, true)
		appendUninstallAction(result, action)
	}
}

func addCLIPlanAction(result *uninstallResult, options uninstallOptions) {
	if options.SkipCLI {
		appendUninstallAction(result, uninstallAction{Name: "uninstall_cli", Status: "skipped", Message: "--skip-cli set"})
		return
	}
	appendUninstallAction(result, uninstallAction{
		Name:   "uninstall_cli",
		Status: "planned",
		Args:   []string{options.NPM, "uninstall", "-g", uninstallCLIPackageName},
	})
}

func uninstallCLIForUninstall(ctx context.Context, npm string) uninstallAction {
	npmBin, err := resolveUninstallExecutable(npm)
	if err != nil {
		return uninstallAction{Name: "uninstall_cli", Status: "error", Args: []string{npm, "uninstall", "-g", uninstallCLIPackageName}, Error: err.Error()}
	}
	return runUninstallExternalCommand(ctx, "uninstall_cli", npmBin, []string{"uninstall", "-g", uninstallCLIPackageName}, true)
}

func runUninstallExternalCommand(ctx context.Context, name string, command string, args []string, hardFailure bool) uninstallAction {
	output, err := runUninstallCommand(ctx, 2*time.Minute, command, args...)
	action := uninstallAction{Name: name, Status: "removed", Args: append([]string{command}, args...)}
	if len(output) > 0 {
		action.Detail = map[string]string{"output": strings.TrimSpace(string(output))}
	}
	if err != nil {
		action.Error = err.Error()
		if hardFailure {
			action.Status = "error"
		} else {
			action.Status = "warning"
		}
	}
	return action
}

func runUninstallCommand(parent context.Context, timeout time.Duration, command string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	output, err := uninstallCombinedOutput(ctx, command, args...)
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s timed out after %s", strings.Join(append([]string{command}, args...), " "), timeout)
		}
		return nil, fmt.Errorf("%s canceled: %w", strings.Join(append([]string{command}, args...), " "), ctx.Err())
	}
	if err != nil {
		trimmed := strings.TrimSpace(string(output))
		if trimmed == "" {
			return output, fmt.Errorf("%s: %w", strings.Join(append([]string{command}, args...), " "), err)
		}
		return output, fmt.Errorf("%s: %w\n%s", strings.Join(append([]string{command}, args...), " "), err, trimmed)
	}
	return output, nil
}

func resolveUninstallExecutable(command string) (string, error) {
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
	resolved, err := uninstallLookPath(clean)
	if err != nil {
		return "", fmt.Errorf("%s not found on PATH: %w", clean, err)
	}
	return resolved, nil
}

func stdinIsInteractive() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func confirmUninstall(home string, botletsHome string) bool {
	fmt.Fprintf(os.Stderr, "This will remove Comment.io and Botlets local state:\n  %s\n  %s\nType 'uninstall' to continue: ", home, botletsHome)
	scanner := bufio.NewScanner(os.Stdin)
	if !scanner.Scan() {
		return false
	}
	return strings.TrimSpace(scanner.Text()) == "uninstall"
}
