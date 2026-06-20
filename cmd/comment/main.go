package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		code, stderr := exitForError(err)
		if stderr != "" {
			fmt.Fprintln(os.Stderr, stderr)
		}
		os.Exit(code)
	}
}

// exitForError maps a CLI error to its process exit code and the line (if any)
// to print to stderr. Extracted from main so the missing-multiplexer remapping
// is unit-testable without spawning a process.
func exitForError(err error) (code int, stderr string) {
	if err == nil {
		return 0, ""
	}
	// A missing legacy tmux failure can reach us as a daemon socket error; remap
	// it to the dedicated exit code and clear message the local paths use, so the
	// exit status is identical no matter where tmux turned up missing.
	var sockErr cliSocketError
	if errors.As(err, &sockErr) && sockErr.Code == commentbus.SocketErrorCodeTmuxNotInstalled {
		err = tmuxMissingExitError()
	}
	// Likewise remap a missing-bmux daemon socket error to the dedicated exit code
	// and install guidance, so the exit status is identical no matter where bmux
	// turned up missing.
	if errors.As(err, &sockErr) && sockErr.Code == commentbus.SocketErrorCodeBmuxNotInstalled {
		err = bmuxMissingExitError()
	}
	var exitErr cliExitError
	if errors.As(err, &exitErr) {
		// An empty Message preserves the historical "exit with this status, print
		// nothing" behavior (e.g. forwarding a child's exit code).
		return exitErr.Code, exitErr.Message
	}
	return 1, err.Error()
}

func run(args []string) error {
	// Publish the build version so bmux channel selection can follow this
	// binary's npm release channel (a prerelease build => the staging bmux
	// channel), not just COMMENT_IO_ENV. Set before any path that resolves the
	// bmux install channel (the manual install hint, or an explicit bmux opt-in);
	// bmux is no longer auto-installed by bus install / doctor.
	commentbus.SetCLIReleaseVersion(version)
	args, err := applyEnvironment(os.Args[0], args)
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(usage())
		return nil
	}
	if args[0] == "help" {
		return runHelp(args[1:])
	}
	if args[0] == "version" || args[0] == "--version" || args[0] == "-v" {
		fmt.Println(version)
		return nil
	}
	if err := enforceCLIVersion(args); err != nil {
		return err
	}
	if hasRootRuntimeFlag(args) {
		return runRuntime(args)
	}
	switch args[0] {
	case "docs":
		return runDocs(args[1:])
	case "bus":
		return runBus(args[1:])
	case "daemon":
		return runDaemonCompat(args[1:])
	case "run":
		return runRuntime(args[1:])
	case "runtime":
		return runRuntimeControl(args[1:])
	case "messages":
		return runMessages(args[1:])
	case "listen":
		return runListen(args[1:])
	case "ephemeral":
		return runEphemeral(args[1:])
	case "secrets":
		return runSecrets(args[1:])
	case "skill":
		return runSkill(args[1:])
	case "mcp":
		return runMCP(args[1:])
	case "activity":
		return runActivity(args[1:])
	case "notifications":
		return errors.New("comment notifications has been removed; use comment run for live runtime delivery, or comment messages wait/receive/renew/ack/release for local daemon messages")
	case "plugin":
		return runPlugin(args[1:])
	case "doctor":
		return runDoctor(args[1:])
	case "sync":
		return runSync(args[1:])
	case "botlets":
		return runBotlets(args[1:])
	case "sessions":
		return runSessions(args[1:])
	case "session-exec":
		return runSessionExec(args[1:])
	case "__runtime-tail":
		return runRuntimeTail(args[1:])
	case "diagnose":
		return runDiagnose(args[1:])
	case "upgrade":
		return runUpgrade(args[1:])
	case "uninstall":
		return runUninstall(args[1:])
	default:
		return fmt.Errorf("unknown command %q\n\n%s", args[0], usage())
	}
}

// applyEnvironment resolves the target deployment (production or staging) for
// this invocation and publishes it via COMMENT_IO_ENV so the daemon, managed
// runtimes, and every default resolver agree on the same on-disk root, API
// endpoint, and synced-docs folder. Precedence: an explicit --staging or
// --production root flag, then the invoked binary name (comment-staging), then
// the COMMENT_IO_ENV/COMMENT_ENV variables, defaulting to production. The root
// flags are stripped from the returned args so per-command flag parsers never
// see them; arguments after a "--" terminator are passed through untouched.
func applyEnvironment(argv0 string, args []string) ([]string, error) {
	staging := false
	production := false
	// Only the leading run of arguments (before the subcommand) is treated as the
	// global environment selector. A later "--staging"/"--production" token — for
	// example a `--body --staging` flag value — is left untouched for the
	// subcommand's own parser. Staging is also selectable via the comment-staging
	// binary name and COMMENT_IO_ENV, so requiring the flag to lead is not limiting.
	consumed := 0
	for _, arg := range args {
		if arg == "--staging" {
			staging = true
			consumed++
			continue
		}
		if arg == "--production" {
			production = true
			consumed++
			continue
		}
		break
	}
	filtered := append([]string(nil), args[consumed:]...)
	if staging && production {
		return nil, errors.New("cannot use --staging and --production together")
	}

	var env string
	switch {
	case staging:
		env = commentbus.EnvStaging
	case production:
		env = commentbus.EnvProduction
	case filepath.Base(argv0) == "comment-staging":
		env = commentbus.EnvStaging
	default:
		selector := strings.TrimSpace(os.Getenv(commentbus.EnvVar))
		if selector == "" {
			selector = strings.TrimSpace(os.Getenv("COMMENT_ENV"))
		}
		if strings.EqualFold(selector, commentbus.EnvStaging) {
			env = commentbus.EnvStaging
		} else {
			env = commentbus.EnvProduction
		}
	}
	if err := os.Setenv(commentbus.EnvVar, env); err != nil {
		return nil, err
	}
	return filtered, nil
}

func runDaemonCompat(args []string) error {
	if len(args) == 0 {
		return errors.New(usage())
	}
	switch args[0] {
	case "health":
		fs := flag.NewFlagSet("comment daemon health", flag.ContinueOnError)
		home := fs.String("home", "", "Comment.io home directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if len(fs.Args()) > 0 {
			return errors.New("daemon health does not accept positional arguments")
		}
		return daemonSocketHealth(*home)
	case "run":
		return runBus(append([]string{"run"}, args[1:]...))
	case "install":
		return runBusInstall(args[1:])
	case "start":
		return runBusStart(args[1:])
	case "stop":
		return runBusStop(args[1:])
	case "status":
		return runBusStatus(args[1:])
	case "uninstall":
		return runBusUninstall(args[1:])
	default:
		return fmt.Errorf("unknown daemon command %q", args[0])
	}
}

func runBus(args []string) error {
	if len(args) == 0 {
		return errors.New(usage())
	}
	switch args[0] {
	case "init":
		fs := flag.NewFlagSet("comment bus init", flag.ContinueOnError)
		home := fs.String("home", "", "Comment.io home directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return initBus(*home)
	case "health":
		fs := flag.NewFlagSet("comment bus health", flag.ContinueOnError)
		home := fs.String("home", "", "Comment.io home directory")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return health(*home)
	case "run":
		fs := flag.NewFlagSet("comment bus run", flag.ContinueOnError)
		home := fs.String("home", "", "Comment.io home directory")
		botletsHome := fs.String("botlets-home", "", "Botlets home directory")
		tmuxPollInterval := fs.Duration("tmux-poll-interval", commentbus.DefaultTmuxPollInterval(), "runtime liveness poll interval (legacy env COMMENT_IO_TMUX_POLL_INTERVAL)")
		tmuxSubmitDelay := fs.Duration("tmux-submit-delay", commentbus.DefaultTmuxSubmitDelay(), "delay between runtime text injection and Enter submission (legacy env COMMENT_IO_TMUX_SUBMIT_DELAY)")
		tmuxBin := fs.String("tmux-bin", "", "pin the tmux binary to an explicit path (or COMMENT_IO_TMUX_BIN); default scans trusted dirs, never $PATH")
		bmuxBin := fs.String("bmux-bin", "", "opt into the bmux host and pin its binary to an explicit path (or COMMENT_IO_BMUX_BIN); default host is tmux")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return runDaemon(*home, *botletsHome, *tmuxPollInterval, *tmuxSubmitDelay, *tmuxBin, *bmuxBin)
	case "install":
		return runBusInstall(args[1:])
	case "start":
		return runBusStart(args[1:])
	case "stop":
		return runBusStop(args[1:])
	case "status":
		return runBusStatus(args[1:])
	case "uninstall":
		return runBusUninstall(args[1:])
	case "pair":
		return runBusPair(args[1:])
	case "unpair":
		return runBusUnpair(args[1:])
	case "repair":
		return repair(args[1:])
	case "reload-profiles":
		return reloadProfiles(args[1:])
	default:
		return fmt.Errorf("unknown bus command %q", args[0])
	}
}

func initBus(home string) error {
	ctx := context.Background()
	paths, err := resolveCLIPaths(home)
	if err != nil {
		return err
	}
	store, err := commentbus.OpenStore(ctx, paths)
	if err != nil {
		return err
	}
	defer store.Close()
	capability, err := commentbus.EnsureOwnerCapability(paths)
	if err != nil {
		return err
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"ok":                    true,
		"schema_version":        version,
		"home":                  paths.Home,
		"socket_path":           paths.Socket,
		"history_path":          store.Path(),
		"owner_capability_path": capability.Path,
		"owner_capability_new":  capability.Created,
	})
}

func health(home string) error {
	ctx := context.Background()
	paths, err := resolveCLIPaths(home)
	if err != nil {
		return err
	}
	// Pairing status re-reads daemon-auth.json on every call so pair/unpair
	// show up immediately. Only the public daemon id is reported, never the
	// token; an unreadable file reports as unpaired rather than failing health.
	daemonPaired := false
	var daemonID any
	if auth, ok, authErr := commentbus.LoadDaemonAuth(paths); authErr == nil && ok {
		daemonPaired = true
		daemonID = auth.DaemonID
	}
	store, err := commentbus.OpenExistingStore(ctx, paths)
	if errors.Is(err, commentbus.ErrStoreNotInitialized) {
		return printJSON(map[string]any{
			"ok":               true,
			"version":          version,
			"protocol_version": commentbus.BusProtocolVersion,
			"initialized":      false,
			"schema_version":   nil,
			"home":             paths.Home,
			"socket_path":      paths.Socket,
			"history_path":     paths.History,
			"daemon_paired":    daemonPaired,
			"daemon_id":        daemonID,
			"auto_update":      commentbus.AutoUpdateHealth(paths, version),
			"features": map[string]any{
				commentbus.FeatureDaemonPairing:   commentbus.FeatureDaemonPairingVersion,
				commentbus.FeatureAgentEnrollment: commentbus.FeatureAgentEnrollmentVersion,
			},
		})
	}
	if err != nil {
		return err
	}
	defer store.Close()
	schemaVersion, err := store.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{
		"ok":               true,
		"version":          version,
		"protocol_version": commentbus.BusProtocolVersion,
		"initialized":      true,
		"schema_version":   schemaVersion,
		"home":             paths.Home,
		"socket_path":      paths.Socket,
		"history_path":     store.Path(),
		"daemon_paired":    daemonPaired,
		"daemon_id":        daemonID,
		"auto_update":      commentbus.AutoUpdateHealth(paths, version),
		"features": map[string]any{
			commentbus.FeatureDaemonPairing:   commentbus.FeatureDaemonPairingVersion,
			commentbus.FeatureAgentEnrollment: commentbus.FeatureAgentEnrollmentVersion,
		},
	})
}

func daemonSocketHealth(home string) error {
	paths, err := resolveCLIPaths(home)
	if err != nil {
		return err
	}
	return callSocketResultAndPrint(context.Background(), paths, "health", nil, nil, 5*time.Second)
}

func repair(args []string) error {
	fs := flag.NewFlagSet("comment messages repair", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	dryRun := fs.Bool("dry-run", false, "report repair actions without mutating")
	messageID := fs.String("message-id", "", "target a single local message id")
	opID := fs.String("op-id", "", "target a single pending operation id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("messages repair does not accept positional arguments")
	}
	if *messageID != "" && !commentbus.LocalMessageIDRE.MatchString(*messageID) {
		return errors.New("invalid message_id")
	}
	if *opID != "" && !commentbus.LocalOperationIDRE.MatchString(*opID) {
		return errors.New("invalid op_id")
	}
	ctx := context.Background()
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	store, err := commentbus.OpenExistingStore(ctx, paths)
	if errors.Is(err, commentbus.ErrStoreNotInitialized) {
		if !*dryRun {
			return errors.New("bus is not initialized")
		}
		return printJSON(map[string]any{
			"ok":          true,
			"dry_run":     true,
			"initialized": false,
			"actions":     []commentbus.RepairAction{},
		})
	}
	if err != nil {
		return err
	}
	filter := commentbus.RepairFilter{
		MessageID: *messageID,
		OpID:      *opID,
	}
	if *dryRun {
		defer store.Close()
		if _, err := ownerOnlyAuth(paths, ""); err != nil {
			return err
		}
		actions, err := commentbus.BusRepairDryRunWithFilter(ctx, paths, store, filter)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{
			"ok":          true,
			"dry_run":     true,
			"initialized": true,
			"actions":     actions,
		})
	}
	if closeErr := store.Close(); closeErr != nil {
		return closeErr
	}
	auth, err := ownerOnlyAuth(paths, "")
	if err != nil {
		return err
	}
	params := map[string]any{"dry_run": *dryRun}
	if *messageID != "" {
		params["message_id"] = *messageID
	}
	if *opID != "" {
		params["op_id"] = *opID
	}
	return callSocketResultAndPrint(ctx, paths, "messages.repair", auth, params, 30*time.Second)
}

func runDaemon(home string, botletsHome string, tmuxPollInterval time.Duration, tmuxSubmitDelay time.Duration, tmuxBinary string, bmuxBinary string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runDaemonWithContext(ctx, home, botletsHome, tmuxPollInterval, tmuxSubmitDelay, tmuxBinary, bmuxBinary)
}

func runDaemonWithContext(ctx context.Context, home string, botletsHome string, tmuxPollInterval time.Duration, tmuxSubmitDelay time.Duration, tmuxBinary string, bmuxBinary string) error {
	paths, err := resolveCLIPaths(home)
	if err != nil {
		return err
	}
	// Reconcile any pending auto-update BEFORE StartDaemon: this counts a boot
	// attempt (and rolls back at the cap) so a binary that crashes DURING
	// StartDaemon still self-heals — it would otherwise never reach the
	// post-start reconcile below (Phase 7).
	reconcileAutoUpdatePreStart(ctx, paths, botletsHome)
	daemon, err := commentbus.StartDaemon(ctx, commentbus.DaemonOptions{
		Paths:                     paths,
		Version:                   version,
		BotletsHome:               botletsHome,
		NotificationClient:        commentbus.NewVersionedHTTPNotificationClient(version, nil),
		EnableNotificationPollers: true,
		TmuxBinary:                tmuxBinary,
		BmuxBinary:                bmuxBinary,
		TmuxPollInterval:          tmuxPollInterval,
		TmuxSubmitDelay:           tmuxSubmitDelay,
		TCPListenAddr:             strings.TrimSpace(os.Getenv("COMMENT_IO_BUS_TCP_LISTEN")),
		AllowNonLoopbackTCP:       os.Getenv("COMMENT_IO_BUS_TCP_ALLOW_NONLOOPBACK") == "1",
		DisableUnixListener:       os.Getenv("COMMENT_IO_BUS_DISABLE_UNIX") == "1",
	})
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return nil
		}
		return err
	}
	// StartDaemon succeeded, so the new binary can at least boot. Run the real
	// post-update health check: commit a healthy upgrade or roll back an
	// unhealthy one immediately (Phase 7). The bus base dirs and socket are up
	// at this point, so the self-check and a possible rollback restart run cleanly.
	reconcileAutoUpdatePostStart(ctx, paths, botletsHome)
	startSyncWorker(ctx, paths)
	startBotletsTeamResyncWorker(ctx, paths, botletsHome)
	startAgentEnrollmentWorker(ctx, paths, botletsHome)
	startAgentRuntimeRequestWorker(ctx, paths)
	startOwnedAgentsReconciler(ctx, paths, botletsHome)
	startAutoUpdateWorker(ctx, paths, botletsHome)
	<-ctx.Done()
	if err := daemon.Close(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func runSessionExec(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: comment session-exec <session-id> <generation>")
	}
	paths, err := resolveCLIPaths("")
	if err != nil {
		return err
	}
	return commentbus.ExecManagedSession(paths, args[0], args[1])
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func usage() string {
	return cliUsage()
}
