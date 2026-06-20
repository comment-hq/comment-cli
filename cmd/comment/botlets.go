package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
	"github.com/comment-hq/comment-cli/internal/commentsync"
)

func runBotlets(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(botletsUsage())
		return nil
	}
	switch args[0] {
	case "setup":
		return runBotletsSetup(args[1:])
	case "register":
		return runBotletsRegister(args[1:])
	case "team-setup":
		return runBotletsTeamSetup(args[1:])
	case "team-resync":
		return runBotletsTeamResync(args[1:])
	case "session":
		return runBotletsSession(args[1:])
	case "log", "logs":
		return runBotletsLogs(args[1:])
	case "status":
		return runBotletsStatus(args[1:])
	default:
		return fmt.Errorf("unknown botlets command %q\n\n%s", args[0], botletsUsage())
	}
}

func botletsUsage() string {
	return strings.Join([]string{
		"Usage:",
		"  comment botlets setup --bot <owner.slug|slug> [--owner <handle>] [--runtime claude|codex] [--home ~/.comment-io] [--botlets-home ~/botlets] [--base-url URL] [--timeout 10m] [--setup-attempt-id ID] [--json]",
		"  comment botlets register --handle <owner.slug> --bot-slug <slug> --bot-agent-id <id> --owner-agent-id <id> --workspace-id <id> --container-id <id> --root-folder-id <id> --setup-generation <n> [--bot-id <id>] [--slug-alias <slug[,slug]>] [--handle-alias <handle[,handle]>] [--display-name <name>] [--timezone <tz>] [--runtime claude|codex] [--secret-stdin|--secret <secret>] [--base-url URL] [--home ~/.comment-io] [--botlets-home ~/botlets] [--json]",
		"  comment botlets team-setup --workspace-id <bw_id> --code <setupCodeId> [--token-stdin|--token <token>] [--runtime claude|codex] [--base-url URL] [--home ~/.comment-io] [--botlets-home ~/botlets] [--json]",
		"  comment botlets team-resync [--home ~/.comment-io] [--botlets-home ~/botlets] [--json]",
		"  comment botlets session reset --log-path <path> [--home ~/.comment-io]",
		"  comment botlets logs --bot <name-or-handle> [--run <run-id>|--message <message-id>] [--home ~/.comment-io] [--botlets-home ~/botlets] [--claude-home ~/.claude] [--codex-home ~/.codex] [--json] [--no-open]",
		"  comment botlets status [--bot <name-or-handle>] [--home ~/.comment-io] [--botlets-home ~/botlets] [--json]",
	}, "\n")
}

var botletsSetupAttemptIDRE = regexp.MustCompile(`^bla_[A-Za-z0-9_-]{12,80}$`)
var botletsTelemetryEmailRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
var botletsTelemetryURLRE = regexp.MustCompile(`https?://[^\s)]+`)
var botletsTelemetryLongTokenRE = regexp.MustCompile(`[A-Za-z0-9_-]{32,}`)
var botletsTelemetryLibrarySyncKeyRE = regexp.MustCompile(`usk_v2\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
var botletsTelemetryAgentSecretRE = regexp.MustCompile(`as_[A-Za-z0-9_-]+`)
var botletsTelemetrySetupAttemptRE = regexp.MustCompile(`bla_[A-Za-z0-9_-]+`)

type botletsSetupTelemetryState struct {
	BaseURL        string
	OwnerHandle    string
	BotSlug        string
	BotHandle      string
	Runtime        string
	SetupAttemptID string
	Paths          commentbus.Paths
	HasPaths       bool
	JSONOut        bool
	ReportFailure  bool
}

type botletsSetupStartResponse struct {
	UserCode        string `json:"userCode"`
	DeviceCode      string `json:"deviceCode"`
	VerificationURI string `json:"verificationUri"`
	ExpiresAt       string `json:"expiresAt"`
	Interval        int    `json:"interval"`
	BotSlug         string `json:"botSlug"`
	OwnerHandle     string `json:"ownerHandle"`
}

type botletsSetupPollResponse struct {
	Status                   string   `json:"status"`
	Interval                 int      `json:"interval,omitempty"`
	ExpiresAt                string   `json:"expiresAt,omitempty"`
	UserCode                 string   `json:"user_code,omitempty"`
	AgentSecret              string   `json:"agent_secret,omitempty"`
	CompletionToken          string   `json:"completion_token,omitempty"`
	CompletionTokenExpiresAt string   `json:"completion_token_expires_at,omitempty"`
	OwnerAgentID             string   `json:"owner_agent_id,omitempty"`
	BotID                    string   `json:"bot_id,omitempty"`
	BotAgentID               string   `json:"bot_agent_id,omitempty"`
	BotSlug                  string   `json:"bot_slug,omitempty"`
	BotHandle                string   `json:"bot_handle,omitempty"`
	BotName                  string   `json:"bot_name,omitempty"`
	SlugAliases              []string `json:"slug_aliases,omitempty"`
	HandleAliases            []string `json:"handle_aliases,omitempty"`
	SetupGeneration          int      `json:"setup_generation,omitempty"`
	SetupAttemptID           string   `json:"setup_attempt_id,omitempty"`
	ScheduleTimezone         string   `json:"schedule_timezone,omitempty"`
	Brain                    struct {
		WorkspaceID  string `json:"workspaceId"`
		ContainerID  string `json:"containerId"`
		RootFolderID string `json:"rootFolderId"`
	} `json:"brain,omitempty"`
}

func runBotletsSession(args []string) error {
	if len(args) == 0 {
		return errors.New("botlets session requires a subcommand")
	}
	switch args[0] {
	case "reset":
		return runBotletsSessionReset(args[1:])
	default:
		return fmt.Errorf("unknown botlets session command %q", args[0])
	}
}

func runBotletsSessionReset(args []string) error {
	fs := flag.NewFlagSet("comment botlets session reset", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	logPath := fs.String("log-path", "", "daily reset log path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("botlets session reset does not accept positional arguments")
	}
	if strings.TrimSpace(*logPath) == "" {
		return errors.New("botlets session reset requires --log-path")
	}
	cleanLogPath := filepath.Clean(*logPath)
	if !filepath.IsAbs(cleanLogPath) {
		abs, err := filepath.Abs(cleanLogPath)
		if err != nil {
			return err
		}
		cleanLogPath = abs
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	auth, _, present, err := sessionAuthFromEnv(paths)
	if err != nil {
		return err
	}
	if !present {
		return errors.New("botlets session reset must be run from a managed Botlets session")
	}
	return callSocketAndPrint(context.Background(), paths, "sessions.reset-complete", auth, map[string]any{
		"log_path": cleanLogPath,
	}, 30*time.Second)
}

func runBotletsSetup(args []string) (err error) {
	telemetry := botletsSetupTelemetryState{
		BaseURL: commentsync.DefaultBaseURL(),
		Runtime: "claude",
	}
	defer func() {
		if err == nil || !telemetry.ReportFailure {
			return
		}
		emitBotletsSetupFailureTelemetry(context.Background(), telemetry, err)
		maybeRunBotletsSetupDiagnose(telemetry)
	}()

	fs := flag.NewFlagSet("comment botlets setup", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	botletsHomeFlag := fs.String("botlets-home", "", "Botlets home directory")
	botFlag := fs.String("bot", "", "Botlets bot as owner.slug or slug")
	ownerFlag := fs.String("owner", "", "owner handle when --bot is just a slug")
	runtimeFlag := fs.String("runtime", "claude", "managed runtime to start for this bot (claude or codex)")
	baseURLFlag := fs.String("base-url", "", "Comment.io base URL")
	timeoutFlag := fs.Duration("timeout", 10*time.Minute, "setup approval timeout")
	setupAttemptIDFlag := fs.String("setup-attempt-id", "", "privacy-safe setup attempt correlation id")
	jsonOut := fs.Bool("json", false, "print JSON")
	yes := fs.Bool("yes", false, "run without interactive prompts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = yes
	telemetry.JSONOut = *jsonOut
	telemetry.BaseURL = strings.TrimRight(firstNonEmpty(*baseURLFlag, commentsync.DefaultBaseURL()), "/")
	telemetry.Runtime = strings.TrimSpace(*runtimeFlag)
	telemetry.SetupAttemptID = strings.TrimSpace(*setupAttemptIDFlag)
	if len(fs.Args()) > 0 {
		return errors.New("botlets setup does not accept positional arguments")
	}
	ownerHandle, botSlug, err := parseBotletsBotSelector(*ownerFlag, *botFlag)
	if err != nil {
		return err
	}
	telemetry.OwnerHandle = ownerHandle
	telemetry.BotSlug = botSlug
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	telemetry.Paths = paths
	telemetry.HasPaths = true
	botletsHome, err := commentbus.ResolveBotletsHome(firstNonEmpty(*botletsHomeFlag, persistedCLIBotletsHome(paths, "")))
	if err != nil {
		return err
	}
	setupAttemptID := strings.TrimSpace(*setupAttemptIDFlag)
	if setupAttemptID != "" && !botletsSetupAttemptIDRE.MatchString(setupAttemptID) {
		return errors.New("invalid setup attempt id")
	}
	runtime := strings.TrimSpace(*runtimeFlag)
	if runtime != "claude" && runtime != "codex" {
		return errors.New("invalid Botlets runtime")
	}
	telemetry.Runtime = runtime
	telemetry.ReportFailure = true
	syncStatus, err := commentsync.ReadStatus(commentsync.Options{Home: paths.Home})
	if err != nil {
		return err
	}
	baseURL := strings.TrimRight(firstNonEmpty(*baseURLFlag, syncStatus.BaseURL, commentsync.DefaultBaseURL()), "/")
	if baseURL == "" {
		baseURL = commentsync.DefaultBaseURL()
	}
	telemetry.BaseURL = baseURL
	telemetryClient := &http.Client{Timeout: 5 * time.Second}
	emitBotletsSetupTelemetryForSetup(context.Background(), telemetryClient, baseURL, "info", "botlets_cli_prereq_checked", map[string]any{
		"setup_attempt_id": setupAttemptID,
		"owner_handle":     ownerHandle,
		"bot_slug":         botSlug,
		"runtime":          runtime,
		"prereq":           "library_sync",
		"outcome":          prereqOutcome(syncStatus.Configured),
		"failure_code":     prereqFailureCode(syncStatus.Configured),
		"cli_version":      version,
	})
	if !syncStatus.Configured {
		return errors.New("sign in so this computer can read the bot's instructions, then rerun this command. Advanced: run `comment sync login` before `comment botlets setup`")
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeoutFlag)
	defer cancel()
	client := &http.Client{Timeout: 30 * time.Second}
	emitBotletsSetupTelemetryForSetup(ctx, client, baseURL, "info", "botlets_cli_setup_started", map[string]any{
		"setup_attempt_id": setupAttemptID,
		"owner_handle":     ownerHandle,
		"bot_slug":         botSlug,
		"runtime":          runtime,
		"cli_version":      version,
	})
	session, err := startBotletsSetup(ctx, client, baseURL, ownerHandle, botSlug, syncStatus.Root, setupAttemptID)
	if err != nil {
		return err
	}
	verificationURI := session.VerificationURI
	if strings.HasPrefix(verificationURI, "/") {
		verificationURI = baseURL + verificationURI
	}
	if !*jsonOut {
		setupLabel := botSlug
		if ownerHandle != "" {
			setupLabel = ownerHandle + "." + botSlug
		}
		fmt.Fprintf(os.Stderr, "Open this URL to approve connecting this computer to %s:\n%s\n\nCode: %s\nOnly approve if the browser code matches this terminal code.\n", setupLabel, verificationURI, session.UserCode)
	}
	poll, err := pollBotletsSetup(ctx, client, baseURL, session)
	if err != nil {
		return err
	}
	if setupAttemptID == "" {
		setupAttemptID = poll.SetupAttemptID
	}
	telemetry.SetupAttemptID = setupAttemptID
	telemetry.BotSlug = firstNonEmpty(poll.BotSlug, botSlug)
	telemetry.BotHandle = poll.BotHandle
	reg, err := registerBotletsBotLocally(ctx, botletsRegisterInput{
		Paths:            paths,
		BotletsHome:      botletsHome,
		BaseURL:          baseURL,
		BotHandle:        poll.BotHandle,
		AgentSecret:      poll.AgentSecret,
		BotSlug:          poll.BotSlug,
		BotDisplayName:   poll.BotName,
		BotID:            poll.BotID,
		SlugAliases:      poll.SlugAliases,
		HandleAliases:    poll.HandleAliases,
		OwnerAgentID:     poll.OwnerAgentID,
		BotAgentID:       poll.BotAgentID,
		WorkspaceID:      poll.Brain.WorkspaceID,
		ContainerID:      poll.Brain.ContainerID,
		RootFolderID:     poll.Brain.RootFolderID,
		SetupGeneration:  poll.SetupGeneration,
		ScheduleTimezone: poll.ScheduleTimezone,
		Runtime:          runtime,
	})
	if err != nil {
		return err
	}
	if err := handleBotletsSetupReloadFailure(ctx, client, telemetry, "after writing local files", reg.ReloadError); err != nil {
		return err
	}
	profilePath := reg.ProfilePath
	projection := reg.Projection
	entry := reg.Entry
	daemonOrientation := reg.DaemonOrientation
	reloadResult := reg.ReloadResult
	reloadErr := reg.ReloadError
	now := time.Now().UTC().Format(time.RFC3339)
	complete, err := completeBotletsSetup(ctx, client, baseURL, poll, projection, now, setupAttemptID, runtime)
	if err != nil {
		emitBotletsSetupTelemetryForSetup(ctx, client, baseURL, "error", "botlets_local_setup_finished", map[string]any{
			"setup_attempt_id": setupAttemptID,
			"owner_handle":     ownerHandle,
			"bot_slug":         poll.BotSlug,
			"bot_handle":       poll.BotHandle,
			"runtime":          runtime,
			"outcome":          "failed",
			"failure_code":     "complete_failed",
			"cli_version":      version,
		})
		return err
	}
	entry, err = reconcileBotletsRegistryAfterCompletion(paths, botletsHome, baseURL, poll.AgentSecret, entry, complete)
	if err != nil {
		return err
	}
	profilePath = entry.CredentialProfile
	telemetry.BotSlug = entry.Name
	telemetry.BotHandle = entry.Handle
	reloadResult, reloadErr = reloadBotletsProfiles(ctx, paths, botletsHome)
	if err := handleBotletsSetupReloadFailure(ctx, client, telemetry, "after reconciling local bot identity", reloadErr); err != nil {
		return err
	}
	brainRoot, brainErr := commentbus.ValidateBotletsBrainProjection(paths, entry)
	if brainErr != nil {
		brainRoot = projection.Root
	}
	hasBootstrap := false
	var bootstrapErr error
	bootstrapProbeError := errorString(brainErr)
	if bootstrapProbeError == "" {
		hasBootstrap, bootstrapErr = commentbus.BotletsBootstrapPresent(brainRoot)
		bootstrapProbeError = errorString(bootstrapErr)
	}
	var (
		orientation    string
		orientationErr error
	)
	docsRoot, _ := commentbus.LocalSyncOrientationPaths(paths)
	orientation, orientationErr = commentbus.BuildBotletsSetupOrientation(commentbus.BotletsSetupOrientationInput{
		BotName:             entry.Name,
		BotDisplayName:      entry.DisplayName,
		BotHandle:           entry.Handle,
		BrainRoot:           brainRoot,
		BaseURL:             baseURL,
		DocsRoot:            docsRoot,
		HasBootstrap:        hasBootstrap,
		BootstrapProbeError: bootstrapProbeError,
	})
	resultSetupGeneration := poll.SetupGeneration
	if entry.BrainRef != nil && entry.BrainRef.SetupGeneration > 0 {
		resultSetupGeneration = entry.BrainRef.SetupGeneration
	}
	result := map[string]any{
		"ok":                  true,
		"bot":                 entry.Name,
		"handle":              entry.Handle,
		"profile_path":        profilePath,
		"registry_path":       filepath.Join(botletsHome, "registry.json"),
		"brain_path":          projection.Root,
		"brain_relative_path": projection.RelativePath,
		"sync_root":           projection.SyncRoot,
		"setup_generation":    resultSetupGeneration,
		"cloud_setup":         complete,
		"daemon_reload":       reloadResult,
		"daemon_reload_error": reloadErr,
		"daemon_orientation":  daemonOrientation,
		"setup_orientation":   orientation,
		"setup_run_command":   botletsSetupRunCommand(entry.Name),
	}
	if daemonOrientation.Warning != "" {
		result["setup_orientation_warning"] = daemonOrientation.Warning
		emitBotletsSetupWarningTelemetry(ctx, client, telemetry, "daemon_orientation_warning", daemonOrientation.Warning)
	}
	if orientation != "" && daemonOrientation.Supported {
		result["setup_orientation_delivery"] = "automatic_tmux_startup"
	}
	// `has_bootstrap` is only included when the probe completed; on probe
	// failure we surface `bootstrap_probe_error` instead so automation cannot
	// mistake an unknown state for a definitive "absent".
	if bootstrapProbeError == "" {
		result["has_bootstrap"] = hasBootstrap
	} else {
		result["bootstrap_probe_error"] = bootstrapProbeError
		emitBotletsSetupWarningTelemetry(ctx, client, telemetry, "bootstrap_probe_failed", bootstrapProbeError)
	}
	if orientationErr != nil {
		result["setup_orientation_error"] = orientationErr.Error()
		emitBotletsSetupWarningTelemetry(ctx, client, telemetry, "setup_orientation_build_failed", orientationErr.Error())
	}
	finishedData := map[string]any{
		"setup_attempt_id": setupAttemptID,
		"owner_handle":     ownerHandle,
		"bot_slug":         entry.Name,
		"bot_handle":       entry.Handle,
		"runtime":          runtime,
		"outcome":          "ok",
		"cli_version":      version,
	}
	emitBotletsSetupTelemetryForSetup(ctx, client, baseURL, "info", "botlets_local_setup_finished", finishedData)
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Print(formatBotletsSetupHumanOutput(botletsSetupHumanOutput{
		BotName:                 entry.Name,
		BotHandle:               entry.Handle,
		Runtime:                 entry.ManagedSession.Runtime,
		ProfilePath:             profilePath,
		RegistryPath:            filepath.Join(botletsHome, "registry.json"),
		BrainPath:               projection.Root,
		DaemonReload:            reloadErr,
		DaemonRefreshed:         daemonOrientation.Refreshed,
		BootstrapProbeError:     bootstrapProbeError,
		SetupOrientationWarning: daemonOrientation.Warning,
		SetupOrientationError:   errorString(orientationErr),
		SetupOrientation:        orientation,
		SetupAttemptID:          setupAttemptID,
	}))
	return nil
}

func botletsSetupRunCommand(botName string) string {
	if isDefaultGuideSlug(botName) {
		return "comment run"
	}
	return fmt.Sprintf("comment run %s", botName)
}

// botletsRegisterInput carries everything needed to wire a Botlets bot into the
// local registry once its credentials and brain reference are already known.
// The interactive setup path fills it from the device-code poll response; the
// non-interactive `comment botlets register` path (team install) fills it from
// the install manifest. No device-code completion token is involved.
type botletsRegisterInput struct {
	Paths            commentbus.Paths
	BotletsHome      string
	BaseURL          string
	BotHandle        string
	AgentSecret      string
	BotSlug          string
	BotDisplayName   string
	BotID            string
	SlugAliases      []string
	HandleAliases    []string
	OwnerAgentID     string
	BotAgentID       string
	WorkspaceID      string
	ContainerID      string
	RootFolderID     string
	SetupGeneration  int
	ScheduleTimezone string
	Runtime          string
}

type botletsRegisterResult struct {
	Entry             commentbus.BotRegistryEntry
	Projection        commentsync.BotletsBrainProjection
	ProfilePath       string
	DaemonOrientation botletsSetupDaemonOrientation
	ReloadResult      any
	ReloadError       string
}

// registerBotletsBotLocally performs the local wiring half of Botlets setup
// (everything after the credentials + brain reference are known): write the
// agent profile, locate and validate the brain projection (refreshing local
// sync once if it is not yet present), upsert the bot registry entry with its
// brain ref, persist the bus config, and refresh + reload the daemon. It is
// shared by the interactive `comment botlets setup` (after device-code approval)
// and the non-interactive `comment botlets register` (team install).
//
// It deliberately does NOT call the server-side completion endpoint: the
// interactive path owns that with its device-code completion token, and
// team-install members are marked ready server-side at token-mint time. The
// daemon reload is reported via ReloadError rather than returned as an error so
// each caller decides whether a stale daemon is fatal (interactive setup) or a
// non-fatal warning (team install, where the registry entry is already durable
// and the daemon picks it up on its next reload).
func registerBotletsBotLocally(ctx context.Context, in botletsRegisterInput) (botletsRegisterResult, error) {
	if in.Runtime != "claude" && in.Runtime != "codex" {
		return botletsRegisterResult{}, errors.New("invalid Botlets runtime")
	}
	profileWrite, err := prepareBotletsAgentProfileWithRuntime(in.Paths, in.BotHandle, in.AgentSecret, in.BaseURL, in.Runtime)
	if err != nil {
		return botletsRegisterResult{}, err
	}
	profilePath := profileWrite.path
	query := commentsync.BotletsBrainProjectionQuery{
		WorkspaceID:  in.WorkspaceID,
		BotID:        in.BotID,
		BotAgentID:   in.BotAgentID,
		ContainerID:  in.ContainerID,
		RootFolderID: in.RootFolderID,
	}
	projection, err := commentsync.FindBotletsBrainProjection(ctx, commentsync.Options{Home: in.Paths.Home}, query)
	if errors.Is(err, commentsync.ErrBotletsBrainProjectionNotFound) {
		if _, syncErr := commentsync.Once(ctx, commentsync.Options{Home: in.Paths.Home}); syncErr != nil {
			return botletsRegisterResult{}, fmt.Errorf("could not refresh local sync before locating Botlets brain projection: %w", syncErr)
		}
		projection, err = commentsync.FindBotletsBrainProjection(ctx, commentsync.Options{Home: in.Paths.Home}, query)
	}
	if err != nil {
		return botletsRegisterResult{}, err
	}
	entry := commentbus.BotRegistryEntry{
		Name:              in.BotSlug,
		DisplayName:       commentbus.NormalizeBotDisplayNameForRegistry(in.BotDisplayName),
		BotID:             firstNonEmpty(in.BotID, in.BotAgentID),
		Handle:            in.BotHandle,
		SlugAliases:       in.SlugAliases,
		HandleAliases:     in.HandleAliases,
		CredentialProfile: profilePath,
		BrainRef: &commentbus.BotBrainRef{
			WorkspaceID:     in.WorkspaceID,
			OwnerAgentID:    in.OwnerAgentID,
			BotAgentID:      in.BotAgentID,
			ContainerID:     in.ContainerID,
			RootFolderID:    in.RootFolderID,
			RelativePath:    projection.RelativePath,
			SetupGeneration: in.SetupGeneration,
		},
		ManagedSession: commentbus.ManagedSessionSetting{Enabled: true, Runtime: in.Runtime, Timezone: in.ScheduleTimezone},
	}
	var aliasWrites []botletsAgentProfileAliasWrite
	extraProfiles := map[string]commentbus.AgentProfile{profileWrite.profile.Handle: profileWrite.profile}
	rollbackBusConfig, err := writeBotletsBusConfigWithRollback(in.Paths, commentbus.BusConfig{BotletsHome: in.BotletsHome})
	if err != nil {
		return botletsRegisterResult{}, err
	}
	entry, err = upsertBotletsRegistryReturningEntryWithPreflight(in.Paths, in.BotletsHome, entry, extraProfiles, func(candidate commentbus.BotRegistryEntry) error {
		var prepareErr error
		aliasWrites, prepareErr = prepareBotletsAgentProfileAliases(in.Paths, in.BotletsHome, candidate.Handle, candidate.HandleAliases, firstNonEmpty(candidate.BotID, in.BotAgentID), in.BotAgentID)
		return prepareErr
	}, func(commentbus.BotRegistryEntry) error {
		return writePreparedBotletsAgentProfileSetRollbackable(profileWrite, aliasWrites)
	})
	if err != nil {
		if rollbackErr := rollbackBusConfig(); rollbackErr != nil {
			return botletsRegisterResult{}, fmt.Errorf("%w (bus config rollback failed: %v)", err, rollbackErr)
		}
		return botletsRegisterResult{}, err
	}
	daemonOrientation, daemonOrientationErr := ensureBotletsSetupOrientationDaemon(ctx, in.Paths, in.BotletsHome)
	if daemonOrientationErr != nil {
		return botletsRegisterResult{}, daemonOrientationErr
	}
	reloadResult, reloadErr := reloadBotletsProfiles(ctx, in.Paths, in.BotletsHome)
	return botletsRegisterResult{
		Entry:             entry,
		Projection:        projection,
		ProfilePath:       profilePath,
		DaemonOrientation: daemonOrientation,
		ReloadResult:      reloadResult,
		ReloadError:       reloadErr,
	}, nil
}

var botletsTeamRegisterLocally = registerBotletsBotLocally

// runBotletsRegister wires a Botlets bot into the local registry from
// already-issued credentials + brain reference, with no browser device-code
// round-trip. It is what the team-install script calls per member after running
// `comment sync once`, so team agents get the full Botlets orientation. The
// agent secret is read from stdin (--secret-stdin) by default so it never lands
// in argv or shell history.
func runBotletsRegister(args []string) error {
	fs := flag.NewFlagSet("comment botlets register", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	botletsHomeFlag := fs.String("botlets-home", "", "Botlets home directory")
	baseURLFlag := fs.String("base-url", "", "Comment.io base URL")
	handleFlag := fs.String("handle", "", "bot handle as owner.slug")
	botSlugFlag := fs.String("bot-slug", "", "bot slug (used as the local bot name)")
	botIDFlag := fs.String("bot-id", "", "stable bot id")
	slugAliasFlag := fs.String("slug-alias", "", "comma-separated old bot slug aliases")
	handleAliasFlag := fs.String("handle-alias", "", "comma-separated old bot handle aliases")
	botAgentIDFlag := fs.String("bot-agent-id", "", "bot agent id")
	ownerAgentIDFlag := fs.String("owner-agent-id", "", "owner agent id")
	workspaceIDFlag := fs.String("workspace-id", "", "brain workspace id")
	containerIDFlag := fs.String("container-id", "", "brain container id")
	rootFolderIDFlag := fs.String("root-folder-id", "", "brain root folder id")
	setupGenerationFlag := fs.Int("setup-generation", 0, "brain setup generation (>= 1)")
	displayNameFlag := fs.String("display-name", "", "bot display name")
	timezoneFlag := fs.String("timezone", "", "schedule timezone")
	runtimeFlag := fs.String("runtime", "claude", "managed runtime (claude or codex)")
	secretFlag := fs.String("secret", "", "agent secret (prefer --secret-stdin)")
	secretStdin := fs.Bool("secret-stdin", false, "read the agent secret from stdin")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("botlets register does not accept positional arguments")
	}
	handle := strings.TrimSpace(*handleFlag)
	if handle == "" {
		return errors.New("botlets register requires --handle")
	}
	botSlug := strings.TrimSpace(*botSlugFlag)
	if botSlug == "" {
		return errors.New("botlets register requires --bot-slug")
	}
	runtime := strings.TrimSpace(*runtimeFlag)
	if runtime != "claude" && runtime != "codex" {
		return errors.New("invalid Botlets runtime")
	}
	if *setupGenerationFlag < 1 {
		return errors.New("botlets register requires --setup-generation >= 1")
	}
	required := []struct {
		flag  string
		value string
	}{
		{"--bot-agent-id", strings.TrimSpace(*botAgentIDFlag)},
		{"--owner-agent-id", strings.TrimSpace(*ownerAgentIDFlag)},
		{"--workspace-id", strings.TrimSpace(*workspaceIDFlag)},
		{"--container-id", strings.TrimSpace(*containerIDFlag)},
		{"--root-folder-id", strings.TrimSpace(*rootFolderIDFlag)},
	}
	for _, req := range required {
		if req.value == "" {
			return fmt.Errorf("botlets register requires %s", req.flag)
		}
	}
	var secret string
	if *secretStdin {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 16*1024))
		if err != nil {
			return err
		}
		secret = strings.TrimSpace(string(data))
	} else {
		secret = strings.TrimSpace(*secretFlag)
	}
	if secret == "" {
		return errors.New("botlets register requires an agent secret (--secret-stdin or --secret)")
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	botletsHome, err := commentbus.ResolveBotletsHome(firstNonEmpty(*botletsHomeFlag, persistedCLIBotletsHome(paths, "")))
	if err != nil {
		return err
	}
	baseURL := strings.TrimRight(strings.TrimSpace(*baseURLFlag), "/")
	if baseURL == "" {
		if status, statusErr := commentsync.ReadStatus(commentsync.Options{Home: paths.Home}); statusErr == nil && status.BaseURL != "" {
			baseURL = strings.TrimRight(status.BaseURL, "/")
		}
	}
	if baseURL == "" {
		baseURL = commentsync.DefaultBaseURL()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	reg, err := registerBotletsBotLocally(ctx, botletsRegisterInput{
		Paths:            paths,
		BotletsHome:      botletsHome,
		BaseURL:          baseURL,
		BotHandle:        handle,
		AgentSecret:      secret,
		BotSlug:          botSlug,
		BotDisplayName:   strings.TrimSpace(*displayNameFlag),
		BotID:            strings.TrimSpace(*botIDFlag),
		SlugAliases:      splitBotletsAliasList(*slugAliasFlag),
		HandleAliases:    splitBotletsAliasList(*handleAliasFlag),
		OwnerAgentID:     strings.TrimSpace(*ownerAgentIDFlag),
		BotAgentID:       strings.TrimSpace(*botAgentIDFlag),
		WorkspaceID:      strings.TrimSpace(*workspaceIDFlag),
		ContainerID:      strings.TrimSpace(*containerIDFlag),
		RootFolderID:     strings.TrimSpace(*rootFolderIDFlag),
		SetupGeneration:  *setupGenerationFlag,
		ScheduleTimezone: strings.TrimSpace(*timezoneFlag),
		Runtime:          runtime,
	})
	if err != nil {
		return err
	}
	result := map[string]any{
		"ok":             true,
		"bot":            reg.Entry.Name,
		"bot_id":         reg.Entry.BotID,
		"handle":         reg.Entry.Handle,
		"slug_aliases":   reg.Entry.SlugAliases,
		"handle_aliases": reg.Entry.HandleAliases,
		"profile_path":   reg.ProfilePath,
		"registry_path":  filepath.Join(botletsHome, "registry.json"),
		"brain_path":     reg.Projection.Root,
		"daemon_reload":  reg.ReloadResult,
	}
	if reg.ReloadError != "" {
		result["daemon_reload_error"] = reg.ReloadError
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("Registered %s (%s) with its Botlets brain at %s.\n", reg.Entry.Name, reg.Entry.Handle, reg.Projection.Root)
	if reg.ReloadError != "" {
		fmt.Fprintf(os.Stderr, "comment botlets register: daemon reload warning: %s\n", reg.ReloadError)
	}
	return nil
}

// botletsTeamRuntimeConfig persists the team runtime's runner identity + secret
// so the daemon's heartbeat/manifest-resync loop can authenticate as the team
// runtime. Stored 0o600 under the Botlets home.
type botletsTeamRuntimeConfig struct {
	WorkspaceID  string `json:"workspace_id"`
	RunnerID     string `json:"runner_id"`
	RunnerSecret string `json:"runner_secret"`
	BaseURL      string `json:"base_url"`
	Runtime      string `json:"runtime,omitempty"`
	// LastManifestFingerprint is the server-computed fingerprint of the full
	// ungated team manifest last fetched, registered locally, and acked. The
	// daemon resync worker compares this with the lightweight manifest status so
	// it does not mint fresh runner-bound bot credentials on every poll when the
	// team manifest has not changed.
	LastManifestFingerprint string `json:"last_manifest_fingerprint,omitempty"`
	// LastManifestAgents are the bot handles registered by that same acked
	// manifest, persisted next to the fingerprint so the resync worker's fast
	// path can verify the local installs still exist before trusting an
	// unchanged server-side fingerprint (missingBotletsTeamLocalInstall).
	//
	// NOT omitempty on purpose: the field distinguishes nil ("never persisted"
	// — a config upgraded from before this field existed has the key absent and
	// unmarshals to nil) from a non-nil empty list ("a full resync acked a
	// genuinely empty team"). The fast path forces ONE full resync on nil to
	// backfill the list, but honors a non-nil empty list as up to date — so a
	// zero-bot team is not re-fetched every 30s. Persisting [] (not dropping it)
	// is what flips nil -> non-nil and ends that one-time forced resync.
	LastManifestAgents []string `json:"last_manifest_agents"`
	// A RANDOM per-install id (NOT derived from hostname) used as the redeem
	// idempotency key + installation hash, persisted so retries reuse it. Deriving
	// it from the hostname would make two machines with the same hostname collide:
	// the second redeem would rotate/return the first machine's runner secret and
	// the duplicate-install guard would treat them as the same install.
	InstallationID string `json:"installation_id,omitempty"`
}

func botletsTeamRuntimeConfigPath(botletsHome string) string {
	return filepath.Join(botletsHome, "team-runtime.json")
}

func readBotletsTeamRuntimeConfig(botletsHome string) (botletsTeamRuntimeConfig, error) {
	var cfg botletsTeamRuntimeConfig
	data, err := os.ReadFile(botletsTeamRuntimeConfigPath(botletsHome))
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func writeBotletsTeamRuntimeConfig(botletsHome string, cfg botletsTeamRuntimeConfig) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		return err
	}
	path := botletsTeamRuntimeConfigPath(botletsHome)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// readMachineID returns a stable per-MACHINE identifier that does NOT travel with
// a copied team-runtime.json (unlike a value persisted in that file, and unlike
// the hostname which can collide). Empty if none can be read.
func readMachineID() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				return s
			}
		}
	}
	// macOS: the hardware IOPlatformUUID.
	if out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output(); err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.Contains(line, "IOPlatformUUID") {
				parts := strings.Split(line, "\"")
				if len(parts) >= 4 && strings.TrimSpace(parts[3]) != "" {
					return strings.TrimSpace(parts[3])
				}
			}
		}
	}
	return ""
}

// machineInstallVerifier derives the install id from the per-machine machine-id
// (bound to the workspace), so a copied/restored team-runtime.json re-derives a
// DIFFERENT id on another machine — its heartbeat then mismatches the pinned id
// and the runner is flagged needs_attention (the duplicate-install guard). Falls
// back to a random persisted id only when no machine-id is available (the value
// is then copyable, but still unique per first install). Deterministic per
// machine, so retries on the same machine stay idempotent.
func machineInstallVerifier(workspaceID, botletsHome string) (string, error) {
	if raw := readMachineID(); raw != "" {
		sum := sha256.Sum256([]byte("botlets-team-install:" + workspaceID + ":" + raw))
		return hex.EncodeToString(sum[:])[:32], nil
	}
	return ensureBotletsInstallationID(botletsHome)
}

// ensureBotletsInstallationID returns a stable RANDOM per-install id, reusing the
// one persisted in team-runtime.json (so a retried redeem is recognized as the
// same install) or minting + persisting a fresh one on first run.
func ensureBotletsInstallationID(botletsHome string) (string, error) {
	if cfg, err := readBotletsTeamRuntimeConfig(botletsHome); err == nil && cfg.InstallationID != "" {
		return cfg.InstallationID, nil
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	// Persist immediately (preserving any existing fields) so a retry reuses it.
	cfg, _ := readBotletsTeamRuntimeConfig(botletsHome)
	cfg.InstallationID = id
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		return "", err
	}
	return id, nil
}

type botletsTeamRuntimeHeartbeat struct {
	ManifestGeneration int `json:"manifest_generation,omitempty"`
}

type botletsTeamRuntimeManifestStatus struct {
	ManifestFingerprint string `json:"manifest_fingerprint,omitempty"`
}

// botletsResyncErrorMaxLen bounds the resync failure detail carried on the
// runner heartbeat so a pathological error chain cannot bloat the stored
// runner record server-side.
const botletsResyncErrorMaxLen = 300

// truncateBotletsResyncError trims and caps the resync failure detail reported
// on the runner heartbeat (Phase 9b). Empty means "no failure".
func truncateBotletsResyncError(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		return ""
	}
	runes := []rune(message)
	if len(runes) <= botletsResyncErrorMaxLen {
		return message
	}
	return string(runes[:botletsResyncErrorMaxLen])
}

func heartbeatBotletsTeamRuntime(ctx context.Context, client *http.Client, botletsHome string, cfg botletsTeamRuntimeConfig, resyncError string) (botletsTeamRuntimeHeartbeat, error) {
	verifier, _ := machineInstallVerifier(cfg.WorkspaceID, botletsHome)
	if verifier == "" {
		return botletsTeamRuntimeHeartbeat{}, nil
	}
	payload := map[string]any{
		"runnerSecret":       cfg.RunnerSecret,
		"installationIdHash": verifier,
		"cliVersion":         version,
		"daemonVersion":      version,
	}
	// resyncError is ALWAYS present (Phase 9b): the truncated failure detail
	// from the latest failed resync pass, or an explicit null on success so the
	// server clears a previously stored error instead of latching it forever.
	if trimmed := truncateBotletsResyncError(resyncError); trimmed != "" {
		payload["resyncError"] = trimmed
	} else {
		payload["resyncError"] = nil
	}
	var heartbeat botletsTeamRuntimeHeartbeat
	err := postBotletsJSON(ctx, client, cfg.BaseURL+"/botlets/workspaces/"+cfg.WorkspaceID+"/runners/"+cfg.RunnerID+"/heartbeat", payload, 200, &heartbeat)
	return heartbeat, err
}

// reportBotletsRunnerBinding best-effort reports one runner<->bot binding state
// to the workspace DO (Phase 9b): "ready" after a successful local
// registration, "problem" (which the DO stores as binding status `needs_sync`)
// after a failed one, so the workspace UI can show per-bot install state on the
// team computer. The DO's binding route accepts ONLY ready|problem and
// authenticates with the runner secret in the body. Failures are logged as
// warnings and swallowed: binding visibility must never block the manifest
// ack or the fingerprint persistence that drive the actual resync lifecycle.
func reportBotletsRunnerBinding(ctx context.Context, client *http.Client, paths commentbus.Paths, cfg botletsTeamRuntimeConfig, botAgentID string, state string) {
	botAgentID = strings.TrimSpace(botAgentID)
	if botAgentID == "" {
		return
	}
	endpoint := cfg.BaseURL + "/botlets/workspaces/" + cfg.WorkspaceID + "/runners/" + cfg.RunnerID + "/bindings/" + botAgentID + "/" + state
	if err := postBotletsJSON(ctx, client, endpoint, map[string]any{
		"runnerSecret": cfg.RunnerSecret,
	}, 200, nil); err != nil {
		writeDaemonWorkerLog(paths, "botlets.team_resync", "warn", "botlets.team_resync_binding_report_failed", map[string]any{
			"workspace_id": cfg.WorkspaceID,
			"runner_id":    cfg.RunnerID,
			"bot_agent_id": botAgentID,
			"state":        state,
			"error":        err.Error(),
		})
	}
}

func fetchBotletsTeamRuntimeManifestStatus(ctx context.Context, client *http.Client, botletsHome string, cfg botletsTeamRuntimeConfig) (botletsTeamRuntimeManifestStatus, error) {
	verifier, _ := machineInstallVerifier(cfg.WorkspaceID, botletsHome)
	if verifier == "" {
		return botletsTeamRuntimeManifestStatus{}, nil
	}
	var status botletsTeamRuntimeManifestStatus
	err := postBotletsJSON(ctx, client, cfg.BaseURL+"/botlets/workspaces/"+cfg.WorkspaceID+"/runners/"+cfg.RunnerID+"/team-manifest-status", map[string]any{
		"runnerSecret":       cfg.RunnerSecret,
		"installationIdHash": verifier,
	}, 200, &status)
	return status, err
}

func persistBotletsTeamManifestFingerprint(botletsHome string, cfg botletsTeamRuntimeConfig, fingerprint string, agents []string) error {
	if fingerprint == "" {
		return nil
	}
	current, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		return err
	}
	if current.WorkspaceID != cfg.WorkspaceID ||
		current.RunnerID != cfg.RunnerID ||
		current.RunnerSecret != cfg.RunnerSecret ||
		current.BaseURL != cfg.BaseURL {
		return nil
	}
	// An unchanged fingerprint alone must NOT skip the write: a config
	// persisted before LastManifestAgents existed carries the fingerprint with
	// a nil handle list, and skipping here would keep the fast-path local
	// install verification (missingBotletsTeamLocalInstall) disabled for that
	// install until the server manifest happens to change. Persist whenever
	// either field is out of date — and treat a nil on-disk list as out of date
	// even when the registered set is empty, so the backfill write flips nil ->
	// non-nil (slices.Equal(nil, []) is true, which would otherwise skip it).
	if current.LastManifestFingerprint == fingerprint &&
		current.LastManifestAgents != nil &&
		slices.Equal(current.LastManifestAgents, agents) {
		return nil
	}
	current.LastManifestFingerprint = fingerprint
	current.LastManifestAgents = append([]string{}, agents...)
	return writeBotletsTeamRuntimeConfig(botletsHome, current)
}

// botletsTeamManifestAckAttempts / botletsTeamManifestAckRetryDelay bound the
// in-pass retry on the team-manifest ack (see resyncBotletsTeamManifest). The
// delay is a package var so tests can shrink it.
const botletsTeamManifestAckAttempts = 3

var botletsTeamManifestAckRetryDelay = 2 * time.Second

// preflightBotletsTeamLocalInstall verifies the deterministic, persistent local
// prerequisites the per-bot team registration needs (a writable Botlets home and
// a writable agent-profiles directory) WITHOUT contacting the server. It runs
// before the manifest fetch so a stable local fault fails the pass before the
// server mints runner-bound credentials this daemon could neither install nor
// revoke. It deliberately covers only the prerequisites that recur every resync
// tick; transient post-fetch faults are handled by the registration path itself.
func preflightBotletsTeamLocalInstall(paths commentbus.Paths, botletsHome string) error {
	if err := botletsTeamInstallDirWritable(botletsHome); err != nil {
		return fmt.Errorf("botlets home is not writable: %w", err)
	}
	if err := botletsTeamInstallDirWritable(filepath.Join(paths.Home, "agents")); err != nil {
		return fmt.Errorf("agent profiles directory is not writable: %w", err)
	}
	return nil
}

// botletsTeamInstallDirWritable confirms dir can be created and written to by
// creating and removing a probe file. A failure (missing/unwritable parent,
// read-only filesystem, permission loss) is the persistent local-install fault
// the team-resync preflight refuses to mint credentials against.
func botletsTeamInstallDirWritable(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("empty directory path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".botlets-preflight-*")
	if err != nil {
		return err
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

// resyncBotletsTeamManifest fetches the team manifest for the configured runner
// and registers every active team agent locally (minting/refreshing their
// runner-bound credentials). Shared by `team-setup` and `team-resync` so a
// team computer can pick up bots shared AFTER the initial install.
// preflightBotletsTeamManifestSync fails closed when local library sync is not
// usable for the team's own origin, BEFORE a manually-run `team-setup` /
// `team-resync` fetches the manifest. The daemon resync worker self-provisions
// sync over its pairing token and has its own usable-or-skip gate; the
// human-run CLI paths have no daemon token to provision with, so without this
// guard they would POST /team-manifest — minting a fresh runner-bound
// credential per bot (issuance is NOT idempotent and there is no client-facing
// revoke path) — and only then fail inside registerBotletsBotLocally's brain
// projection sync, orphaning every just-minted credential and re-minting on
// each retry. Tell the operator to configure sync first instead.
func preflightBotletsTeamManifestSync(paths commentbus.Paths, cfg botletsTeamRuntimeConfig) error {
	if daemonSyncUsableForOrigin(paths.Home, cfg.BaseURL) {
		return nil
	}
	return fmt.Errorf("local library sync is not configured for %s; run `comment sync login` against %s before installing team bots (skipped the team manifest fetch to avoid minting unrevocable runner-bound credentials)", cfg.BaseURL, cfg.BaseURL)
}

func resyncBotletsTeamManifest(ctx context.Context, client *http.Client, paths commentbus.Paths, botletsHome string, cfg botletsTeamRuntimeConfig, runtime string, priorResyncError string) ([]string, error) {
	// Heartbeat FIRST, carrying the CURRENT machine's freshly-derived install
	// verifier (NOT the value persisted in the copyable team-runtime.json), so the
	// server's duplicate-installation guard runs every resync: a copied/restored
	// config run on a different machine re-derives a different verifier, mismatches
	// the pinned install id, and is marked needs_attention. Best-effort (a heartbeat
	// hiccup shouldn't block a legit resync) — the manifest fetch below ALSO carries
	// and the server VERIFIES the same verifier, so a copy fails closed there even
	// if this heartbeat was skipped/failed. priorResyncError (Phase 9b) is the
	// previous pass's failure detail, carried so a persistent failure stays
	// visible on the runner record; "" explicitly clears it.
	_, _ = heartbeatBotletsTeamRuntime(ctx, client, botletsHome, cfg, priorResyncError)
	verifier, _ := machineInstallVerifier(cfg.WorkspaceID, botletsHome)
	// Preflight the PERSISTENT local-install prerequisites BEFORE fetching the
	// manifest. The /team-manifest fetch mints a fresh runner-bound credential
	// per bot (issuance is NOT idempotent — old credentials stay live until
	// revoked), but this daemon has no client-facing revoke path and the manifest
	// response carries each bot's agent_secret, not its credential id, so a local
	// registration failure here cannot revoke the just-minted batch. Unguarded, a
	// stable local fault (an unwritable Botlets home/registry or agents directory)
	// makes every 30s resync re-fetch and re-mint, accumulating live credentials
	// indefinitely behind a never-succeeding install. Fail closed before the mint
	// when those deterministic prerequisites are not met. (Transient post-fetch
	// faults — a daemon that did not reload, a brain projection not yet synced, an
	// ack hiccup — are not preventable here; the ack has its own in-pass retry and
	// a reload/registration miss surfaces as a problem binding.)
	if err := preflightBotletsTeamLocalInstall(paths, botletsHome); err != nil {
		return nil, fmt.Errorf("skipping team manifest fetch; local install prerequisites are not met (avoids minting unrevocable runner-bound credentials): %w", err)
	}
	var manifest struct {
		Agents []struct {
			Handle          string `json:"handle"`
			AgentSecret     string `json:"agent_secret"`
			BotSlug         string `json:"bot_slug"`
			BotAgentID      string `json:"bot_agent_id"`
			OwnerAgentID    string `json:"owner_agent_id"`
			BotID           string `json:"bot_id"`
			SetupGeneration int    `json:"setup_generation"`
			// ScheduleTimezone is the bot's configured schedule timezone,
			// mirrored into the local registry entry's managed-session setting
			// so daily session resets follow the bot's timezone — the same
			// field the individual-bot enrollment hint carries. Optional:
			// older servers omit it and the registry falls back to the
			// default timezone.
			ScheduleTimezone string `json:"schedule_timezone"`
			Brain            struct {
				WorkspaceID  string `json:"workspaceId"`
				ContainerID  string `json:"containerId"`
				RootFolderID string `json:"rootFolderId"`
			} `json:"brain"`
		} `json:"agents"`
		ManifestFingerprint string `json:"manifest_fingerprint,omitempty"`
		ManifestGated       bool   `json:"manifest_gated,omitempty"`
	}
	if err := postBotletsJSON(ctx, client, cfg.BaseURL+"/botlets/workspaces/"+cfg.WorkspaceID+"/runners/"+cfg.RunnerID+"/team-manifest", map[string]any{
		"runnerSecret": cfg.RunnerSecret, "installationIdHash": verifier,
	}, 200, &manifest); err != nil {
		return nil, fmt.Errorf("team manifest fetch failed: %w", err)
	}
	registered := make([]string, 0, len(manifest.Agents))
	for _, a := range manifest.Agents {
		gen := a.SetupGeneration
		if gen < 1 {
			gen = 1
		}
		reg, err := botletsTeamRegisterLocally(ctx, botletsRegisterInput{
			Paths: paths, BotletsHome: botletsHome, BaseURL: cfg.BaseURL,
			BotHandle: a.Handle, AgentSecret: a.AgentSecret, BotSlug: a.BotSlug,
			BotID: a.BotID, OwnerAgentID: a.OwnerAgentID, BotAgentID: a.BotAgentID,
			WorkspaceID: a.Brain.WorkspaceID, ContainerID: a.Brain.ContainerID, RootFolderID: a.Brain.RootFolderID,
			SetupGeneration: gen, ScheduleTimezone: strings.TrimSpace(a.ScheduleTimezone), Runtime: runtime,
		})
		if err != nil {
			// Best-effort per-bot visibility (Phase 9b): mark the binding
			// needs_sync (the DO's "problem" state) so the workspace UI shows
			// which bot the team computer could not register. Never fatal.
			reportBotletsRunnerBinding(ctx, client, paths, cfg, a.BotAgentID, "problem")
			return registered, fmt.Errorf("could not register team agent %s: %w", a.Handle, err)
		}
		if errText := strings.TrimSpace(reg.ReloadError); errText != "" {
			// Registration wrote the files but the daemon did NOT reload: the
			// bot is not actually running here. Treating this as success (ready
			// binding + ack + cleared resync error) while skipping the
			// fingerprint would make every 30s pass silently re-fetch and
			// re-mint credentials behind a healthy-looking UI. Fail the pass
			// instead: problem binding, resync error surfaced, no ack — the
			// retry loop owns recovery once the daemon reloads.
			reportBotletsRunnerBinding(ctx, client, paths, cfg, a.BotAgentID, "problem")
			return registered, fmt.Errorf("registered %s but the daemon did not reload: %s", a.Handle, errText)
		}
		// Best-effort per-bot visibility (Phase 9b): the bot's local
		// registration landed on this runner, so report its binding ready.
		reportBotletsRunnerBinding(ctx, client, paths, cfg, a.BotAgentID, "ready")
		registered = append(registered, a.Handle)
	}
	// Acknowledge full registration so the server consumes the install-time review
	// gate ONLY after every agent was fetched + registered end-to-end. A lost
	// manifest response (before this ack) leaves the gate applied, so a retry won't
	// leak unreviewed pre-redemption bots. Do not cache the generation unless the
	// ack succeeds; otherwise the daemon could skip the retry that consumes the gate.
	//
	// Retry the ack a few times before failing the pass: the manifest fetch
	// above minted a fresh runner-bound credential for every bot (the server
	// issue is NOT idempotent — old credentials stay live until revoked), and a
	// failed pass sends the resync worker back through a FULL re-fetch — another
	// mint per bot — on its next 30s tick. A short in-pass retry keeps a
	// transient ack hiccup from multiplying live credentials.
	var ackErr error
	for attempt := 0; attempt < botletsTeamManifestAckAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return registered, fmt.Errorf("could not acknowledge team manifest: %w", ackErr)
			case <-time.After(botletsTeamManifestAckRetryDelay):
			}
		}
		ackErr = postBotletsJSON(ctx, client, cfg.BaseURL+"/botlets/workspaces/"+cfg.WorkspaceID+"/runners/"+cfg.RunnerID+"/team-manifest/ack", map[string]any{
			"runnerSecret": cfg.RunnerSecret, "installationIdHash": verifier,
		}, 200, nil)
		if ackErr == nil {
			break
		}
	}
	if ackErr != nil {
		return registered, fmt.Errorf("could not acknowledge team manifest: %w", ackErr)
	}
	// Persist when the fingerprint moved OR the registered handle list differs
	// from the persisted one OR the list was never persisted (nil): a full
	// resync against an unchanged server manifest (manual team-resync,
	// manifest-status fetch hiccup, or the forced resync the worker runs for an
	// upgraded config) is the only chance an upgraded pre-LastManifestAgents
	// config gets to backfill the handle list that activates the fast-path
	// local install verification. The explicit nil check makes an upgraded,
	// genuinely-empty team persist [] once (slices.Equal(nil, []) is true), so
	// the worker's forced-resync-on-nil does not loop every 30s.
	if !manifest.ManifestGated && manifest.ManifestFingerprint != "" &&
		(cfg.LastManifestFingerprint != manifest.ManifestFingerprint ||
			cfg.LastManifestAgents == nil ||
			!slices.Equal(cfg.LastManifestAgents, registered)) {
		if err := persistBotletsTeamManifestFingerprint(botletsHome, cfg, manifest.ManifestFingerprint, registered); err != nil {
			return registered, fmt.Errorf("could not persist team manifest fingerprint: %w", err)
		}
	}
	return registered, nil
}

// runBotletsTeamResync re-fetches the team manifest using the persisted team
// runtime config and registers any team agents shared since the last sync. This
// is the consumer of team-runtime.json that lets a team computer pick up bots
// shared after the initial `team-setup` (a daemon/cron can invoke it on a loop).
func runBotletsTeamResync(args []string) error {
	fs := flag.NewFlagSet("comment botlets team-resync", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	botletsHomeFlag := fs.String("botlets-home", "", "Botlets home directory")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	botletsHome, err := commentbus.ResolveBotletsHome(firstNonEmpty(*botletsHomeFlag, persistedCLIBotletsHome(paths, "")))
	if err != nil {
		return err
	}
	cfg, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		return fmt.Errorf("no team runtime configured (run `comment botlets team-setup` first): %w", err)
	}
	if cfg.WorkspaceID == "" || cfg.RunnerID == "" || cfg.RunnerSecret == "" || cfg.BaseURL == "" {
		return errors.New("team runtime config is incomplete; re-run `comment botlets team-setup`")
	}
	runtime := cfg.Runtime
	if runtime != "claude" && runtime != "codex" {
		runtime = "claude"
	}
	if err := preflightBotletsTeamManifestSync(paths, cfg); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 60 * time.Second}
	registered, err := resyncBotletsTeamManifest(ctx, client, paths, botletsHome, cfg, runtime, "")
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(map[string]any{"ok": true, "runner_id": cfg.RunnerID, "agents": registered})
	}
	fmt.Printf("Team runtime resynced (runner %s). Registered %d team agent(s): %s\n", cfg.RunnerID, len(registered), strings.Join(registered, ", "))
	return nil
}

// runBotletsTeamSetup installs THIS machine as a workspace's single team runtime:
// it redeems a primary-owner-minted team-runtime setup code, fetches the manifest
// of active team agents (each with a freshly-minted, runner-bound credential),
// persists the runner secret, and registers every team agent locally so the
// daemon runs them all. This is the Botlets team-collaboration counterpart to the
// per-bot `comment botlets setup`.
func runBotletsTeamSetup(args []string) error {
	fs := flag.NewFlagSet("comment botlets team-setup", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	botletsHomeFlag := fs.String("botlets-home", "", "Botlets home directory")
	baseURLFlag := fs.String("base-url", "", "Comment.io base URL")
	workspaceIDFlag := fs.String("workspace-id", "", "workspace id (bw_...)")
	codeFlag := fs.String("code", "", "team-runtime setup code id")
	tokenFlag := fs.String("token", "", "team-runtime setup token (prefer --token-stdin)")
	tokenStdin := fs.Bool("token-stdin", false, "read the setup token from stdin")
	runtimeFlag := fs.String("runtime", "claude", "managed runtime (claude or codex)")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("botlets team-setup does not accept positional arguments")
	}
	workspaceID := strings.TrimSpace(*workspaceIDFlag)
	code := strings.TrimSpace(*codeFlag)
	if workspaceID == "" || code == "" {
		return errors.New("botlets team-setup requires --workspace-id and --code")
	}
	runtime := strings.TrimSpace(*runtimeFlag)
	if runtime != "claude" && runtime != "codex" {
		return errors.New("invalid Botlets runtime")
	}
	var token string
	if *tokenStdin {
		data, err := io.ReadAll(io.LimitReader(os.Stdin, 16*1024))
		if err != nil {
			return err
		}
		token = strings.TrimSpace(string(data))
	} else {
		token = strings.TrimSpace(*tokenFlag)
	}
	if token == "" {
		return errors.New("botlets team-setup requires a setup token (--token-stdin or --token)")
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	botletsHome, err := commentbus.ResolveBotletsHome(firstNonEmpty(*botletsHomeFlag, persistedCLIBotletsHome(paths, "")))
	if err != nil {
		return err
	}
	baseURL := strings.TrimRight(strings.TrimSpace(*baseURLFlag), "/")
	if baseURL == "" {
		if status, statusErr := commentsync.ReadStatus(commentsync.Options{Home: paths.Home}); statusErr == nil && status.BaseURL != "" {
			baseURL = strings.TrimRight(status.BaseURL, "/")
		}
	}
	if baseURL == "" {
		baseURL = commentsync.DefaultBaseURL()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 60 * time.Second}

	// 1. Redeem the team-runtime setup code → install this machine as the team runtime.
	//    The idempotency key + installation hash come from a RANDOM persisted
	//    per-install id (not the hostname), so two machines with the same hostname
	//    are never conflated into one install/retry.
	installationID, err := machineInstallVerifier(workspaceID, botletsHome)
	if err != nil {
		return fmt.Errorf("could not allocate an installation id: %w", err)
	}
	var redeemed struct {
		RunnerID     string `json:"runnerId"`
		RunnerSecret string `json:"runnerSecret"`
	}
	// Accept 201 (first redeem) AND 200 (idempotent retry after a lost response),
	// so a retried `team-setup` still proceeds to the manifest fetch + config write.
	if err := postBotletsJSONAccept(ctx, client, baseURL+"/botlets/workspaces/"+workspaceID+"/runners/redeem", map[string]any{
		"setupCodeId": code, "token": token, "idempotencyKey": "team-" + installationID, "installationIdHash": installationID,
	}, &redeemed, 201, 200); err != nil {
		return fmt.Errorf("team runtime redeem failed: %w", err)
	}

	// 2. Persist the runner secret + identity (keeping the installation id) so the
	//    resync consumer can authenticate as the team runtime.
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID: workspaceID, RunnerID: redeemed.RunnerID, RunnerSecret: redeemed.RunnerSecret,
		BaseURL: baseURL, Runtime: runtime, InstallationID: installationID,
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		return fmt.Errorf("could not persist team runtime config: %w", err)
	}

	// 3. Fetch the team manifest + register every active team agent locally so the
	//    daemon's per-bot pollers run them. (Shared with `team-resync`.)
	if err := preflightBotletsTeamManifestSync(paths, cfg); err != nil {
		return err
	}
	registered, err := resyncBotletsTeamManifest(ctx, client, paths, botletsHome, cfg, runtime, "")
	if err != nil {
		return err
	}

	result := map[string]any{"ok": true, "runner_id": redeemed.RunnerID, "agents": registered}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("Team runtime installed (runner %s). Registered %d team agent(s): %s\n", redeemed.RunnerID, len(registered), strings.Join(registered, ", "))
	return nil
}

func splitBotletsAliasList(value string) []string {
	var out []string
	seen := map[string]struct{}{}
	for _, part := range strings.Split(value, ",") {
		item := strings.TrimSpace(part)
		if item == "" {
			continue
		}
		key := strings.ToLower(item)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

type botletsSetupHumanOutput struct {
	BotName                 string
	BotHandle               string
	Runtime                 string
	ProfilePath             string
	RegistryPath            string
	BrainPath               string
	DaemonReload            string
	DaemonRefreshed         bool
	BootstrapProbeError     string
	SetupOrientationWarning string
	SetupOrientationError   string
	SetupOrientation        string
	SetupAttemptID          string
}

func formatBotletsSetupHumanOutput(output botletsSetupHumanOutput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Botlets setup finished.\n")
	fmt.Fprintf(&b, "Connected bot: %s (%s)\n", output.BotName, output.BotHandle)
	fmt.Fprintf(&b, "\nLocal paperwork, now pleasantly filed:\n")
	fmt.Fprintf(&b, "  credentials: %s\n", output.ProfilePath)
	fmt.Fprintf(&b, "  bot registry: %s\n", output.RegistryPath)
	fmt.Fprintf(&b, "  brain folder: %s\n", output.BrainPath)
	if output.DaemonReload != "" {
		fmt.Fprintf(&b, "  daemon reload: %s\n", output.DaemonReload)
	}
	fmt.Fprintf(&b, "\nRuntime prep:\n")
	if output.DaemonRefreshed {
		fmt.Fprintf(&b, "  background helper: refreshed so future nudges know where to go\n")
	}
	if output.BootstrapProbeError != "" {
		fmt.Fprintf(&b, "  bootstrap check: %s\n", output.BootstrapProbeError)
	}
	if output.SetupOrientationError != "" {
		fmt.Fprintf(&b, "  startup note: could not prepare automatic delivery (%s)\n", output.SetupOrientationError)
	} else if output.SetupOrientationWarning != "" {
		fmt.Fprintf(&b, "  startup note: %s\n", output.SetupOrientationWarning)
	} else if output.SetupOrientation != "" {
		fmt.Fprintf(&b, "  startup note: queued for automatic delivery when the bot starts\n")
	} else {
		fmt.Fprintf(&b, "  startup note: no extra orientation needed\n")
	}
	fmt.Fprintf(&b, "\nNext:\n")
	fmt.Fprintf(&b, "  start: %s\n", botletsSetupRunCommand(output.BotName))
	return b.String()
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var (
	botletsSetupBusInstall             = busInstall
	botletsSetupDaemonHealth           = botletsDaemonHealth
	botletsSetupDaemonHealthWait       = 10 * time.Second
	botletsSetupDaemonHealthRetryDelay = 250 * time.Millisecond
)

type botletsSetupDaemonOrientation struct {
	Supported     bool           `json:"supported"`
	Refreshed     bool           `json:"refreshed"`
	Warning       string         `json:"warning,omitempty"`
	InitialHealth any            `json:"initial_health,omitempty"`
	InitialError  string         `json:"initial_error,omitempty"`
	Health        any            `json:"health,omitempty"`
	InstallResult map[string]any `json:"install_result,omitempty"`
}

func ensureBotletsSetupOrientationDaemon(ctx context.Context, paths commentbus.Paths, botletsHome string) (botletsSetupDaemonOrientation, error) {
	status := botletsSetupDaemonOrientation{}
	connected, health, healthErr := botletsSetupDaemonHealth(ctx, paths)
	status.InitialHealth = health
	status.InitialError = healthErr
	if connected && healthErr == "" && botletsDaemonSupportsSetupOrientation(health) {
		status.Supported = true
		status.Health = health
		return status, nil
	}
	// pair=false: this daemon refresh runs as a SIDE step of bot registration —
	// including from the background enrollment/team-resync workers, where an
	// unpaired home plus a /dev/null stdin (a character device the
	// interactivity heuristic can misread) would block the worker in the
	// device-pair flow. Pairing is chained only on the human-driven install
	// commands (`comment bus install`, `comment doctor --fix`).
	installResult, err := botletsSetupBusInstall(paths.Home, botletsHome, "", false, false)
	status.InstallResult = installResult
	if err != nil {
		status.Warning = botletsSetupOrientationDaemonWarning(fmt.Sprintf("automatic daemon refresh failed: %v", err))
		return status, nil
	}
	status.Refreshed = true
	deadline := time.Now().Add(botletsSetupDaemonHealthWait)
	lastErr := healthErr
	for {
		connected, health, healthErr = botletsSetupDaemonHealth(ctx, paths)
		status.Health = health
		if connected && healthErr == "" && botletsDaemonSupportsSetupOrientation(health) {
			status.Supported = true
			return status, nil
		}
		if healthErr != "" {
			lastErr = healthErr
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			status.Warning = botletsSetupOrientationDaemonWarning(fmt.Sprintf("daemon health wait was canceled before it advertised %q: %v", commentbus.FeatureBotletsSetupOrientation, ctx.Err()))
			return status, nil
		case <-time.After(botletsSetupDaemonHealthRetryDelay):
		}
	}
	message := fmt.Sprintf("Botlets setup refreshed the Comment.io bus daemon, but daemon health still did not advertise %q", commentbus.FeatureBotletsSetupOrientation)
	if lastErr != "" {
		message += ": " + lastErr
	}
	status.Warning = botletsSetupOrientationDaemonWarning(message)
	return status, nil
}

func botletsSetupOrientationDaemonWarning(reason string) string {
	return fmt.Sprintf("automatic tmux setup orientation is not confirmed because the persistent Comment.io bus is not current (%s). Setup files were written; run `comment bus install` with the freshly installed CLI, then start the runtime again if the first launch does not show the Botlets orientation.", reason)
}

func botletsDaemonSupportsSetupOrientation(health any) bool {
	result, ok := health.(map[string]any)
	if !ok {
		return false
	}
	features, ok := result["features"].(map[string]any)
	if !ok {
		return false
	}
	value, ok := features[commentbus.FeatureBotletsSetupOrientation]
	if !ok {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		normalized := strings.TrimSpace(strings.ToLower(typed))
		return normalized != "" && normalized != "0" && normalized != "false"
	default:
		return false
	}
}

func prereqOutcome(ok bool) string {
	if ok {
		return "ok"
	}
	return "failed"
}

func prereqFailureCode(ok bool) string {
	if ok {
		return ""
	}
	return "not_configured"
}

func botletsSetupBaseTelemetryData(state botletsSetupTelemetryState) map[string]any {
	return map[string]any{
		"setup_attempt_id": state.SetupAttemptID,
		"owner_handle":     state.OwnerHandle,
		"bot_slug":         state.BotSlug,
		"bot_handle":       state.BotHandle,
		"runtime":          state.Runtime,
		"cli_version":      version,
	}
}

func emitBotletsSetupFailureTelemetry(ctx context.Context, state botletsSetupTelemetryState, reason error) {
	if reason == nil || strings.TrimSpace(state.BaseURL) == "" {
		return
	}
	data := botletsSetupBaseTelemetryData(state)
	data["outcome"] = "failed"
	data["failure_code"] = botletsSetupFailureCode(reason)
	data["reason_summary"] = botletsTelemetryReasonSummary(reason)
	data["reason_hash"] = botletsTelemetryReasonHash(reason)
	data["reason_length"] = len(reason.Error())
	if status := botletsTelemetryHTTPStatus(reason); status > 0 {
		data["http_status"] = status
	}
	emitBotletsSetupTelemetryForSetup(ctx, &http.Client{Timeout: 5 * time.Second}, state.BaseURL, "error", "botlets_cli_setup_failed", data)
}

func emitBotletsSetupWarningTelemetry(ctx context.Context, client *http.Client, state botletsSetupTelemetryState, warningCode string, reason string) {
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(state.BaseURL) == "" {
		return
	}
	data := botletsSetupBaseTelemetryData(state)
	data["outcome"] = "warning"
	data["warning_code"] = warningCode
	data["reason_summary"] = botletsTelemetryReasonSummary(errors.New(reason))
	data["reason_hash"] = botletsTelemetryReasonHash(errors.New(reason))
	data["reason_length"] = len(reason)
	emitBotletsSetupTelemetryForSetup(ctx, client, state.BaseURL, "warn", "botlets_cli_setup_warning", data)
}

func botletsSetupFailureCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "approval_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "sign in so this computer can read"):
		return "library_sync_not_configured"
	case strings.Contains(text, "invalid setup attempt id"):
		return "invalid_setup_attempt_id"
	case strings.Contains(text, "invalid botlets runtime"):
		return "invalid_runtime"
	case strings.Contains(text, "invalid botlets bot selector") || strings.Contains(text, "botlets setup requires --bot") || strings.Contains(text, "owner handle"):
		return "invalid_bot_selector"
	case strings.Contains(text, "botlets setup code expired"):
		return "device_code_expired"
	case strings.Contains(text, "botlets setup poll failed"):
		return "poll_http_error"
	case strings.Contains(text, "botlets setup request failed"):
		return "request_http_error"
	case strings.Contains(text, "daemon profile reload failed"):
		return "daemon_reload_failed"
	case strings.Contains(text, "could not refresh local sync"):
		return "library_sync_refresh_failed"
	case strings.Contains(text, "brain projection"):
		return "brain_projection_failed"
	default:
		return "setup_failed"
	}
}

func botletsTelemetryReasonSummary(err error) string {
	if err == nil {
		return ""
	}
	summary := err.Error()
	summary = botletsTelemetryEmailRE.ReplaceAllString(summary, "[email]")
	summary = botletsTelemetryURLRE.ReplaceAllString(summary, "[url]")
	summary = strings.ReplaceAll(summary, "\\", "/")
	summary = botletsTelemetryLibrarySyncKeyRE.ReplaceAllString(summary, "usk_v2.[redacted]")
	summary = botletsTelemetryAgentSecretRE.ReplaceAllString(summary, "as_[redacted]")
	summary = botletsTelemetrySetupAttemptRE.ReplaceAllString(summary, "bla_[redacted]")
	summary = botletsTelemetryLongTokenRE.ReplaceAllString(summary, "[token]")
	summary = redactBotletsTelemetryPaths(summary)
	summary = strings.Join(strings.Fields(summary), " ")
	if len(summary) > 180 {
		return summary[:180] + "..."
	}
	return summary
}

func redactBotletsTelemetryPaths(value string) string {
	var out strings.Builder
	for i := 0; i < len(value); {
		if isBotletsTelemetryWindowsPathStart(value, i) {
			i = consumeBotletsTelemetryPath(value, i+2)
			out.WriteString("[path]")
			continue
		}
		if value[i] == '/' && i+1 < len(value) && !isBotletsTelemetryPathDelimiter(value[i+1]) {
			i = consumeBotletsTelemetryPath(value, i)
			out.WriteString("[path]")
			continue
		}
		out.WriteByte(value[i])
		i++
	}
	return out.String()
}

func isBotletsTelemetryWindowsPathStart(value string, i int) bool {
	if i+2 >= len(value) || value[i+1] != ':' || value[i+2] != '/' {
		return false
	}
	ch := value[i]
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
}

func consumeBotletsTelemetryPath(value string, start int) int {
	i := start
	for i < len(value) {
		for i < len(value) && !isBotletsTelemetryPathDelimiter(value[i]) && !isBotletsTelemetryWhitespace(value[i]) {
			i++
		}
		if i >= len(value) || !isBotletsTelemetryWhitespace(value[i]) {
			return i
		}
		spaceStart := i
		for i < len(value) && isBotletsTelemetryWhitespace(value[i]) {
			i++
		}
		tokenStart := i
		for i < len(value) && !isBotletsTelemetryPathDelimiter(value[i]) && !isBotletsTelemetryWhitespace(value[i]) {
			i++
		}
		token := value[tokenStart:i]
		if token == "" || isBotletsTelemetryPathStopWord(token) {
			return spaceStart
		}
	}
	return i
}

func isBotletsTelemetryWhitespace(ch byte) bool {
	return ch == ' ' || ch == '\n' || ch == '\r' || ch == '\t'
}

func isBotletsTelemetryPathDelimiter(ch byte) bool {
	switch ch {
	case ':', ';', ',', '"', '\'', ')', ']', '}':
		return true
	default:
		return false
	}
}

func isBotletsTelemetryPathStopWord(token string) bool {
	switch strings.ToLower(strings.Trim(token, ".?!")) {
	case "and", "or", "but", "with", "from", "for", "to", "in", "because", "before", "after", "then", "http", "https", "failed", "error":
		return true
	default:
		return false
	}
}

func botletsTelemetryReasonHash(err error) string {
	summary := botletsTelemetryReasonSummary(err)
	if summary == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(summary))
	return hex.EncodeToString(digest[:12])
}

func botletsTelemetryHTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	match := regexp.MustCompile(`HTTP ([1-5][0-9]{2})`).FindStringSubmatch(err.Error())
	if len(match) != 2 {
		return 0
	}
	status := 0
	for _, ch := range match[1] {
		status = status*10 + int(ch-'0')
	}
	return status
}

func maybeRunBotletsSetupDiagnose(state botletsSetupTelemetryState) {
	if state.JSONOut || !state.HasPaths || strings.TrimSpace(state.BaseURL) == "" || !botletsSetupCanPrompt() {
		return
	}
	fmt.Fprintln(os.Stdout, "\nCreating a Comment.io diagnostic bundle for this setup failure.")
	if err := runDiagnose([]string{"--home", state.Paths.Home, "--base-url", state.BaseURL}); err != nil {
		fmt.Fprintf(os.Stdout, "Could not create diagnostics automatically: %v\n", err)
	}
}

func botletsSetupCanPrompt() bool {
	return fileIsCharDevice(os.Stdin) && fileIsCharDevice(os.Stdout)
}

func fileIsCharDevice(file *os.File) bool {
	if file == nil {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func emitBotletsSetupTelemetry(ctx context.Context, client *http.Client, baseURL string, msg string, data map[string]any) {
	emitBotletsSetupTelemetryWithLevel(ctx, client, baseURL, "info", msg, data)
}

var emitBotletsSetupTelemetryForSetup = emitBotletsSetupTelemetryWithLevel

func emitBotletsSetupTelemetryWithLevel(ctx context.Context, client *http.Client, baseURL string, level string, msg string, data map[string]any) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" || client == nil {
		return
	}
	if level != "info" && level != "warn" && level != "error" {
		level = "info"
	}
	eventData := map[string]any{}
	for key, value := range data {
		if value == "" || value == nil {
			continue
		}
		eventData[key] = value
	}
	entry := []map[string]any{{
		"v":         1,
		"ts":        time.Now().UTC().Format(time.RFC3339),
		"side":      "client",
		"level":     level,
		"component": "page",
		"msg":       msg,
		"data":      eventData,
	}}
	body, err := json.Marshal(entry)
	if err != nil {
		return
	}
	telemetryCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(telemetryCtx, http.MethodPost, baseURL+"/api/logs", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Comment-CLI-Version", version)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func expectedBotletsBrainRelativePath(botHandle string, botSlug string) (string, error) {
	parts := strings.SplitN(botHandle, ".", 2)
	if len(parts) < 2 || parts[0] == "" || botSlug == "" {
		return "", errors.New("invalid Botlets bot identity")
	}
	return filepath.ToSlash(filepath.Join("Botlets", parts[0], botSlug, "brain")), nil
}

func startBotletsSetup(ctx context.Context, client *http.Client, baseURL string, ownerHandle string, botSlug string, syncRoot string, setupAttemptID string) (botletsSetupStartResponse, error) {
	payload := map[string]any{
		"bot_slug":     botSlug,
		"device_label": defaultBotletsDeviceLabel(),
		"sync_root":    syncRoot,
	}
	if ownerHandle != "" {
		payload["owner_handle"] = ownerHandle
	}
	if setupAttemptID != "" {
		payload["setup_attempt_id"] = setupAttemptID
	}
	var out botletsSetupStartResponse
	if err := postBotletsJSON(ctx, client, baseURL+"/auth/botlets/local-setup/device-codes", payload, http.StatusCreated, &out); err != nil {
		return out, err
	}
	if out.Interval <= 0 {
		out.Interval = 3
	}
	return out, nil
}

func pollBotletsSetup(ctx context.Context, client *http.Client, baseURL string, session botletsSetupStartResponse) (botletsSetupPollResponse, error) {
	interval := time.Duration(session.Interval) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return botletsSetupPollResponse{}, ctx.Err()
		default:
		}
		payload := map[string]any{"device_code": session.DeviceCode}
		reqBody, _ := json.Marshal(payload)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/auth/botlets/local-setup/device-codes/"+session.UserCode+"/poll", bytes.NewReader(reqBody))
		if err != nil {
			return botletsSetupPollResponse{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Comment-CLI-Version", version)
		resp, err := client.Do(req)
		if err != nil {
			return botletsSetupPollResponse{}, err
		}
		var poll botletsSetupPollResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&poll)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && decodeErr == nil && poll.Status == "approved" && poll.AgentSecret != "" && poll.CompletionToken != "" && poll.UserCode != "" {
			return poll, nil
		}
		if resp.StatusCode == http.StatusGone {
			return botletsSetupPollResponse{}, errors.New("Botlets setup code expired")
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTooManyRequests {
			return botletsSetupPollResponse{}, fmt.Errorf("Botlets setup poll failed: HTTP %d", resp.StatusCode)
		}
		if poll.Interval > 0 {
			interval = time.Duration(poll.Interval) * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return botletsSetupPollResponse{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func completeBotletsSetup(ctx context.Context, client *http.Client, baseURL string, poll botletsSetupPollResponse, projection commentsync.BotletsBrainProjection, verifiedAt string, setupAttemptID string, runtime string) (map[string]any, error) {
	payload := map[string]any{
		"completion_token":             poll.CompletionToken,
		"user_code":                    poll.UserCode,
		"owner_agent_id":               poll.OwnerAgentID,
		"bot_agent_id":                 poll.BotAgentID,
		"bot_slug":                     poll.BotSlug,
		"setup_generation":             poll.SetupGeneration,
		"registry_synced_at":           verifiedAt,
		"brain_projection_verified_at": projection.UpdatedAt.UTC().Format(time.RFC3339),
		"sync_root_fingerprint":        projection.SyncRootFingerprint,
		"ready_for_runs_at":            verifiedAt,
	}
	if setupAttemptID != "" {
		payload["setup_attempt_id"] = setupAttemptID
	}
	if poll.BotID != "" {
		payload["bot_id"] = poll.BotID
	}
	if runtime == "claude" || runtime == "codex" {
		payload["local_setup_runtime"] = runtime
	}
	if projection.UpdatedAt.IsZero() {
		payload["brain_projection_verified_at"] = verifiedAt
	}
	out := map[string]any{}
	return out, postBotletsJSON(ctx, client, strings.TrimRight(baseURL, "/")+"/auth/botlets/local-setup/complete", payload, http.StatusOK, &out)
}

func botletsSetupReloadFailure(stage string, reloadErr string) error {
	if strings.TrimSpace(reloadErr) == "" {
		return nil
	}
	return fmt.Errorf("Botlets daemon profile reload failed %s: %s", stage, reloadErr)
}

func handleBotletsSetupReloadFailure(ctx context.Context, client *http.Client, telemetry botletsSetupTelemetryState, stage string, reloadErr string) error {
	err := botletsSetupReloadFailure(stage, reloadErr)
	if err == nil {
		return nil
	}
	if isManagedSessionOwnerOnlyReloadError(reloadErr) {
		emitBotletsSetupWarningTelemetry(ctx, client, telemetry, "daemon_reload_managed_session", err.Error())
		return nil
	}
	return err
}

func isManagedSessionOwnerOnlyReloadError(reloadErr string) bool {
	return strings.Contains(strings.ToLower(reloadErr), "owner-only command is not allowed from a managed session")
}

func reconcileBotletsRegistryAfterCompletion(paths commentbus.Paths, botletsHome string, baseURL string, agentSecret string, entry commentbus.BotRegistryEntry, complete map[string]any) (commentbus.BotRegistryEntry, error) {
	metadata, ok := complete["metadata"].(map[string]any)
	if !ok {
		return entry, nil
	}
	changed := false
	profileWrite := botletsAgentProfileWrite{}
	if botHandle, ok := complete["bot_handle"].(string); ok && botHandle != "" && !strings.EqualFold(botHandle, entry.Handle) {
		prepared, err := prepareBotletsAgentProfileWithRuntime(paths, botHandle, agentSecret, baseURL, entry.ManagedSession.Runtime)
		if err != nil {
			return commentbus.BotRegistryEntry{}, err
		}
		profileWrite = prepared
		entry.HandleAliases = mergeBotletsRegistryAliases(botHandle, []string{entry.Handle}, entry.HandleAliases)
		entry.Handle = botHandle
		entry.CredentialProfile = prepared.path
		changed = true
	}
	if botID, ok := metadata["botId"].(string); ok && botID != "" && entry.BotID != botID {
		entry.BotID = botID
		changed = true
	}
	if botSlug, ok := metadata["botSlug"].(string); ok && botSlug != "" && entry.Name != botSlug {
		entry.SlugAliases = mergeBotletsRegistryAliases(botSlug, append(botletsStringSliceFromAny(metadata["slugAliases"]), entry.Name), entry.SlugAliases)
		entry.Name = botSlug
		changed = true
	} else if aliases := botletsStringSliceFromAny(metadata["slugAliases"]); len(aliases) > 0 {
		merged := mergeBotletsRegistryAliases(entry.Name, aliases, entry.SlugAliases)
		if strings.Join(merged, "\n") != strings.Join(entry.SlugAliases, "\n") {
			entry.SlugAliases = merged
			changed = true
		}
	}
	if aliases := botletsStringSliceFromAny(metadata["handleAliases"]); len(aliases) > 0 {
		merged := mergeBotletsRegistryAliases(entry.Handle, aliases, entry.HandleAliases)
		if strings.Join(merged, "\n") != strings.Join(entry.HandleAliases, "\n") {
			entry.HandleAliases = merged
			changed = true
		}
	}
	if localSetup, ok := metadata["localSetup"].(map[string]any); ok && entry.BrainRef != nil {
		if setupGeneration := intFromJSONNumber(localSetup["setupGeneration"]); setupGeneration > 0 && entry.BrainRef.SetupGeneration != setupGeneration {
			brainRef := *entry.BrainRef
			brainRef.SetupGeneration = setupGeneration
			entry.BrainRef = &brainRef
			changed = true
		}
	}
	if !changed {
		return entry, nil
	}
	if profileWrite.path == "" {
		return upsertBotletsRegistryReturningEntry(paths, botletsHome, entry)
	}
	var aliasWrites []botletsAgentProfileAliasWrite
	extraProfiles := map[string]commentbus.AgentProfile{profileWrite.profile.Handle: profileWrite.profile}
	return upsertBotletsRegistryReturningEntryWithPreflight(paths, botletsHome, entry, extraProfiles, func(candidate commentbus.BotRegistryEntry) error {
		var prepareErr error
		aliasWrites, prepareErr = prepareBotletsAgentProfileAliases(paths, botletsHome, candidate.Handle, candidate.HandleAliases, firstNonEmpty(candidate.BotID, botletsRegistryEntryBotAgentID(candidate)), botletsRegistryEntryBotAgentID(candidate))
		return prepareErr
	}, func(commentbus.BotRegistryEntry) error {
		return writePreparedBotletsAgentProfileSetRollbackable(profileWrite, aliasWrites)
	})
}

func intFromJSONNumber(value any) int {
	switch n := value.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		if n == float64(int(n)) {
			return int(n)
		}
	}
	return 0
}

func botletsStringSliceFromAny(value any) []string {
	switch items := value.(type) {
	case []string:
		out := make([]string, 0, len(items))
		for _, item := range items {
			if strings.TrimSpace(item) != "" {
				out = append(out, item)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(items))
		for _, item := range items {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func postBotletsJSON(ctx context.Context, client *http.Client, url string, payload any, wantStatus int, out any) error {
	return postBotletsJSONAccept(ctx, client, url, payload, out, wantStatus)
}

// postBotletsJSONAccept is like postBotletsJSON but accepts any of several
// success statuses. Needed for idempotent endpoints (e.g. the team-runtime
// redeem) that return 201 on the first call but 200 (rotated secret) on an
// idempotent retry after a lost response — both must succeed here, or the
// retry fails before the manifest fetch / team-runtime.json write.
func postBotletsJSONAccept(ctx context.Context, client *http.Client, url string, payload any, out any, wantStatuses ...int) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Comment-CLI-Version", version)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	accepted := false
	for _, want := range wantStatuses {
		if resp.StatusCode == want {
			accepted = true
			break
		}
	}
	if !accepted {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("Botlets setup request failed: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type botletsAgentProfileWrite struct {
	path    string
	data    []byte
	profile commentbus.AgentProfile
}

type botletsCredentialProfileRenameRepair struct {
	entry          commentbus.BotRegistryEntry
	profileWrite   botletsAgentProfileWrite
	writeCanonical bool
	aliasWrites    []botletsAgentProfileAliasWrite
}

func prepareBotletsAgentProfile(paths commentbus.Paths, handle string, agentSecret string, baseURL string) (botletsAgentProfileWrite, error) {
	return prepareBotletsAgentProfileWithRuntime(paths, handle, agentSecret, baseURL, "")
}

func prepareBotletsAgentProfileWithRuntime(paths commentbus.Paths, handle string, agentSecret string, baseURL string, runtime string) (botletsAgentProfileWrite, error) {
	write, err := commentbus.PrepareAgentProfileWrite(paths, handle, agentSecret, baseURL, runtime)
	if err != nil {
		// Preserve the Botlets-specific phrasing that call sites and tests
		// assert on; the shared helper reports product-neutral messages.
		switch {
		case errors.Is(err, commentbus.ErrMissingAgentCredential):
			return botletsAgentProfileWrite{}, errors.New("missing Botlets agent credential")
		case errors.Is(err, commentbus.ErrInvalidAgentHandle):
			return botletsAgentProfileWrite{}, errors.New("invalid Botlets agent handle")
		case errors.Is(err, commentbus.ErrInvalidAgentRuntime):
			return botletsAgentProfileWrite{}, errors.New("invalid Botlets runtime")
		}
		return botletsAgentProfileWrite{}, err
	}
	return botletsAgentProfileWrite{
		path:    write.Path,
		data:    write.Data,
		profile: write.Profile,
	}, nil
}

func writePreparedBotletsAgentProfile(write botletsAgentProfileWrite) error {
	return commentbus.WritePrivateFileAtomicExistingDir(write.path, write.data, 0o600)
}

func writeBotletsAgentProfile(paths commentbus.Paths, handle string, agentSecret string, baseURL string) (string, error) {
	write, err := prepareBotletsAgentProfile(paths, handle, agentSecret, baseURL)
	if err != nil {
		return "", err
	}
	return write.path, writePreparedBotletsAgentProfile(write)
}

func prepareBotletsCredentialProfileRenameRepair(paths commentbus.Paths, botletsHome string, previous commentbus.BotRegistryEntry, candidate commentbus.BotRegistryEntry) (botletsCredentialProfileRenameRepair, bool, error) {
	if strings.EqualFold(previous.Handle, candidate.Handle) {
		return botletsCredentialProfileRenameRepair{}, false, nil
	}
	botID := firstNonEmpty(candidate.BotID, previous.BotID)
	botAgentID := firstNonEmpty(botletsRegistryEntryBotAgentID(candidate), botletsRegistryEntryBotAgentID(previous))
	if !botletsRegistryEntryHasIdentity(previous, botID, botAgentID) || !botletsRegistryEntryHasIdentity(candidate, botID, botAgentID) {
		return botletsCredentialProfileRenameRepair{}, false, nil
	}
	oldCredentialPath, ok, err := resolveBotletsRegistryCredentialPathForRepair(botletsHome, previous.CredentialProfile)
	if err != nil || !ok {
		return botletsCredentialProfileRenameRepair{}, false, err
	}
	oldProfile, ok, err := loadBotletsAgentProfileByPathForRepair(paths, oldCredentialPath)
	if err != nil || !ok {
		return botletsCredentialProfileRenameRepair{}, false, err
	}
	if !strings.EqualFold(oldProfile.Handle, previous.Handle) {
		return botletsCredentialProfileRenameRepair{}, false, nil
	}
	profileWrite, err := prepareBotletsAgentProfileWithRuntime(paths, candidate.Handle, oldProfile.AgentSecret, oldProfile.BaseURL, firstNonEmpty(oldProfile.Runtime, previous.ManagedSession.Runtime))
	if err != nil {
		return botletsCredentialProfileRenameRepair{}, false, err
	}
	targetCredentialPath, targetOK, err := resolveBotletsRegistryCredentialPathForRepair(botletsHome, candidate.CredentialProfile)
	if err != nil {
		return botletsCredentialProfileRenameRepair{}, false, err
	}
	if targetOK && targetCredentialPath != oldCredentialPath && targetCredentialPath != profileWrite.path {
		return botletsCredentialProfileRenameRepair{}, false, nil
	}
	writeCanonical, err := shouldWriteBotletsCanonicalProfileRepairTarget(profileWrite.path, candidate.Handle)
	if err != nil {
		return botletsCredentialProfileRenameRepair{}, false, err
	}
	candidate.CredentialProfile = profileWrite.path
	aliasWrites, err := prepareBotletsAgentProfileAliases(paths, botletsHome, candidate.Handle, candidate.HandleAliases, botID, botAgentID)
	if err != nil {
		return botletsCredentialProfileRenameRepair{}, false, err
	}
	return botletsCredentialProfileRenameRepair{entry: candidate, profileWrite: profileWrite, writeCanonical: writeCanonical, aliasWrites: aliasWrites}, true, nil
}

func resolveBotletsRegistryCredentialPathForRepair(botletsHome string, value string) (string, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false, nil
	}
	if strings.ContainsAny(value, "\r\n\x00") || strings.Contains(value, "://") {
		return "", false, nil
	}
	resolved := value
	if strings.HasPrefix(value, "~/") || value == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false, err
		}
		if value == "~" {
			resolved = home
		} else {
			resolved = filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
	} else if !filepath.IsAbs(value) {
		cleaned := filepath.Clean(value)
		if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			return "", false, nil
		}
		resolved = filepath.Join(botletsHome, cleaned)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", false, err
	}
	return filepath.Clean(absolute), true, nil
}

func loadBotletsAgentProfileByPathForRepair(paths commentbus.Paths, credentialPath string) (commentbus.AgentProfile, bool, error) {
	profiles, _, errorsOut := commentbus.LoadAgentProfilesWithAliases(context.Background(), paths, "")
	if commentbus.HasFatalProfileReloadError(errorsOut) {
		return commentbus.AgentProfile{}, false, fmt.Errorf("Botlets credential profile repair failed: %+v", errorsOut)
	}
	for _, profile := range profiles {
		if profile.Path == credentialPath {
			return profile, true, nil
		}
	}
	return commentbus.AgentProfile{}, false, nil
}

func shouldWriteBotletsCanonicalProfileRepairTarget(path string, targetHandle string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("Botlets canonical profile %q cannot overwrite an unsafe profile path", targetHandle)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var raw struct {
		ProfileKind        string `json:"profile_kind"`
		Handle             string `json:"handle"`
		AgentSecret        string `json:"agent_secret"`
		AliasOf            string `json:"alias_of"`
		BotID              string `json:"bot_id"`
		BotAgentID         string `json:"bot_agent_id"`
		DisabledForPolling bool   `json:"disabled_for_polling"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, fmt.Errorf("Botlets canonical profile %q cannot overwrite an invalid profile file", targetHandle)
	}
	if raw.ProfileKind == "alias" {
		return false, fmt.Errorf("Botlets canonical profile %q cannot overwrite an alias profile", targetHandle)
	}
	if raw.ProfileKind != "" || (raw.Handle != "" && !strings.EqualFold(raw.Handle, targetHandle)) || raw.AgentSecret == "" {
		return false, fmt.Errorf("Botlets canonical profile %q already belongs to another credential", targetHandle)
	}
	return false, nil
}

type botletsAgentProfileAliasWrite struct {
	path string
	data []byte
}

func prepareBotletsAgentProfileAliases(paths commentbus.Paths, botletsHome string, canonicalHandle string, aliases []string, botID string, botAgentID string) ([]botletsAgentProfileAliasWrite, error) {
	writes := make([]botletsAgentProfileAliasWrite, 0, len(aliases))
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" || strings.EqualFold(alias, canonicalHandle) {
			continue
		}
		if !commentbus.ProfileRE.MatchString(alias) {
			return nil, errors.New("invalid Botlets alias handle")
		}
		profilePath, err := commentbus.ValidateAgentProfileWriteTarget(paths, alias)
		if err != nil {
			return nil, err
		}
		if err := validateBotletsAgentProfileAliasTarget(botletsHome, canonicalHandle, alias, profilePath, botID, botAgentID); err != nil {
			return nil, err
		}
		data, err := json.MarshalIndent(map[string]any{
			"profile_kind":         "alias",
			"alias_of":             canonicalHandle,
			"bot_id":               botID,
			"bot_agent_id":         botAgentID,
			"disabled_for_polling": true,
		}, "", "  ")
		if err != nil {
			return nil, err
		}
		writes = append(writes, botletsAgentProfileAliasWrite{path: profilePath, data: append(data, '\n')})
	}
	return writes, nil
}

func validateBotletsAgentProfileAliasTarget(botletsHome string, canonicalHandle string, alias string, profilePath string, botID string, botAgentID string) error {
	info, err := os.Lstat(profilePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("Botlets alias profile %q cannot overwrite an unsafe profile path", alias)
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return err
	}
	var raw struct {
		ProfileKind        string `json:"profile_kind"`
		Handle             string `json:"handle"`
		AgentSecret        string `json:"agent_secret"`
		AliasOf            string `json:"alias_of"`
		BotID              string `json:"bot_id"`
		BotAgentID         string `json:"bot_agent_id"`
		DisabledForPolling bool   `json:"disabled_for_polling"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("Botlets alias profile %q cannot overwrite an invalid profile file", alias)
	}
	if raw.ProfileKind == "alias" {
		if !raw.DisabledForPolling {
			return fmt.Errorf("Botlets alias profile %q is not safe to replace", alias)
		}
		if strings.EqualFold(raw.AliasOf, canonicalHandle) || botletsProfileAliasIdentityMatches(raw.BotID, raw.BotAgentID, botID, botAgentID) {
			return nil
		}
		return fmt.Errorf("Botlets alias profile %q already belongs to another bot", alias)
	}
	if raw.ProfileKind != "" || (raw.Handle != "" && !strings.EqualFold(raw.Handle, alias)) || raw.AgentSecret == "" {
		return fmt.Errorf("Botlets alias profile %q cannot overwrite an unrecognized profile file", alias)
	}
	ok, err := botletsRegistryHasProfileAliasForIdentity(botletsHome, alias, profilePath, botID, botAgentID)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return fmt.Errorf("Botlets alias profile %q already belongs to another bot", alias)
}

func botletsProfileAliasIdentityMatches(existingBotID string, existingBotAgentID string, botID string, botAgentID string) bool {
	if botID != "" && existingBotID != "" && existingBotID == botID {
		return true
	}
	return botAgentID != "" && existingBotAgentID != "" && existingBotAgentID == botAgentID
}

func botletsRegistryHasProfileAliasForIdentity(botletsHome string, alias string, profilePath string, botID string, botAgentID string) (bool, error) {
	if strings.TrimSpace(botletsHome) == "" {
		return false, nil
	}
	botletsHome, err := commentbus.ValidateBotletsRegistryWriteTarget(botletsHome)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(botletsHome, "registry.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		return false, err
	}
	cleanProfilePath, err := filepath.Abs(profilePath)
	if err != nil {
		return false, err
	}
	cleanProfilePath = filepath.Clean(cleanProfilePath)
	for _, entry := range registry.Bots {
		if !botletsRegistryEntryHasIdentity(entry, botID, botAgentID) || !entry.MatchesProfile(alias) {
			continue
		}
		credentialPath := entry.CredentialProfile
		if credentialPath != "" && !filepath.IsAbs(credentialPath) {
			credentialPath = filepath.Join(botletsHome, credentialPath)
		}
		if credentialPath == "" {
			return true, nil
		}
		credentialPath, err = filepath.Abs(credentialPath)
		if err != nil {
			return false, err
		}
		if filepath.Clean(credentialPath) == cleanProfilePath || strings.EqualFold(entry.Handle, alias) {
			return true, nil
		}
	}
	return false, nil
}

func writePreparedBotletsAgentProfileAliases(writes []botletsAgentProfileAliasWrite) error {
	for _, write := range writes {
		if err := commentbus.WritePrivateFileAtomicExistingDir(write.path, write.data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

type botletsAgentProfileFileWrite struct {
	path string
	data []byte
}

type botletsAgentProfileFileBackup struct {
	exists bool
	data   []byte
	mode   os.FileMode
}

func writePreparedBotletsAgentProfileSetRollbackable(primary botletsAgentProfileWrite, aliases []botletsAgentProfileAliasWrite) error {
	writes := []botletsAgentProfileFileWrite{{path: primary.path, data: primary.data}}
	for _, alias := range aliases {
		writes = append(writes, botletsAgentProfileFileWrite{path: alias.path, data: alias.data})
	}
	return writeBotletsAgentProfileFilesRollbackable(writes)
}

func writeBotletsAgentProfileFilesRollbackable(writes []botletsAgentProfileFileWrite) error {
	backups := make(map[string]botletsAgentProfileFileBackup, len(writes))
	written := make([]string, 0, len(writes))
	for _, write := range writes {
		if _, ok := backups[write.path]; !ok {
			info, statErr := os.Stat(write.path)
			if statErr == nil {
				data, readErr := os.ReadFile(write.path)
				if readErr != nil {
					return readErr
				}
				backups[write.path] = botletsAgentProfileFileBackup{exists: true, data: data, mode: info.Mode().Perm()}
			} else if errors.Is(statErr, os.ErrNotExist) {
				backups[write.path] = botletsAgentProfileFileBackup{}
			} else {
				return statErr
			}
		}
		if err := commentbus.WritePrivateFileAtomicExistingDir(write.path, write.data, 0o600); err != nil {
			if rollbackErr := restoreBotletsAgentProfileFiles(backups, written); rollbackErr != nil {
				return fmt.Errorf("%w (profile rollback failed: %v)", err, rollbackErr)
			}
			return err
		}
		written = append(written, write.path)
	}
	return nil
}

func restoreBotletsAgentProfileFiles(backups map[string]botletsAgentProfileFileBackup, paths []string) error {
	var rollbackErr error
	for i := len(paths) - 1; i >= 0; i-- {
		path := paths[i]
		backup := backups[path]
		if backup.exists {
			mode := backup.mode
			if mode == 0 {
				mode = 0o600
			}
			if err := commentbus.WritePrivateFileAtomicExistingDir(path, backup.data, mode); err != nil && rollbackErr == nil {
				rollbackErr = err
			}
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && rollbackErr == nil {
			rollbackErr = err
		}
	}
	return rollbackErr
}

func writeBotletsAgentProfileAliases(paths commentbus.Paths, botletsHome string, canonicalHandle string, aliases []string, botID string, botAgentID string) error {
	writes, err := prepareBotletsAgentProfileAliases(paths, botletsHome, canonicalHandle, aliases, botID, botAgentID)
	if err != nil {
		return err
	}
	return writePreparedBotletsAgentProfileAliases(writes)
}

func writeBotletsBusConfigWithRollback(paths commentbus.Paths, config commentbus.BusConfig) (func() error, error) {
	existing, exists, readErr := commentbus.ReadBusConfig(paths)
	if readErr != nil {
		return nil, readErr
	}
	configPath := filepath.Join(paths.Bus, "config.json")
	if err := commentbus.WriteBusConfig(paths, config); err != nil {
		return nil, err
	}
	return func() error {
		if exists {
			return commentbus.WriteBusConfig(paths, existing)
		}
		if err := os.Remove(configPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}, nil
}

func upsertBotletsRegistry(paths commentbus.Paths, botletsHome string, entry commentbus.BotRegistryEntry) error {
	_, err := upsertBotletsRegistryReturningEntry(paths, botletsHome, entry)
	return err
}

func upsertBotletsRegistryReturningEntry(paths commentbus.Paths, botletsHome string, entry commentbus.BotRegistryEntry) (commentbus.BotRegistryEntry, error) {
	return upsertBotletsRegistryReturningEntryWithPreflight(paths, botletsHome, entry, nil, nil, nil)
}

func upsertBotletsRegistryReturningEntryWithPreflight(paths commentbus.Paths, botletsHome string, entry commentbus.BotRegistryEntry, extraProfiles map[string]commentbus.AgentProfile, preflight func(commentbus.BotRegistryEntry) error, afterCommit func(commentbus.BotRegistryEntry) error) (commentbus.BotRegistryEntry, error) {
	var err error
	botletsHome, err = commentbus.ValidateBotletsRegistryWriteTarget(botletsHome)
	if err != nil {
		return commentbus.BotRegistryEntry{}, err
	}
	if err := os.Mkdir(botletsHome, 0o700); err != nil && !os.IsExist(err) {
		return commentbus.BotRegistryEntry{}, err
	}
	if botletsHome, err = commentbus.ValidateBotletsRegistryWriteTarget(botletsHome); err != nil {
		return commentbus.BotRegistryEntry{}, err
	}
	if err := os.Chmod(botletsHome, 0o700); err != nil {
		return commentbus.BotRegistryEntry{}, err
	}
	if botletsHome, err = commentbus.ValidateBotletsRegistryWriteTarget(botletsHome); err != nil {
		return commentbus.BotRegistryEntry{}, err
	}
	lockFile, err := openBotletsRegistryLock(filepath.Join(botletsHome, ".registry.lock"))
	if err != nil {
		return commentbus.BotRegistryEntry{}, err
	}
	defer lockFile.Close()
	if err := lockFile.Chmod(0o600); err != nil {
		return commentbus.BotRegistryEntry{}, err
	}
	if err := lockBotletsRegistryFile(lockFile); err != nil {
		return commentbus.BotRegistryEntry{}, err
	}
	defer unlockBotletsRegistryFile(lockFile)
	if _, err := commentbus.ValidateBotletsRegistryWriteTarget(botletsHome); err != nil {
		return commentbus.BotRegistryEntry{}, err
	}
	registryPath := filepath.Join(botletsHome, "registry.json")
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	var previousRegistryData []byte
	previousRegistryExists := false
	if data, err := os.ReadFile(registryPath); err == nil {
		previousRegistryData = append([]byte(nil), data...)
		previousRegistryExists = true
		if err := json.Unmarshal(data, &registry); err != nil {
			return commentbus.BotRegistryEntry{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return commentbus.BotRegistryEntry{}, err
	}
	nextBots := make([]commentbus.BotRegistryEntry, 0, len(registry.Bots)+1)
	var previousEntry *commentbus.BotRegistryEntry
	for i := range registry.Bots {
		overlap, err := botletsRegistryEntriesOverlap(registry.Bots[i], entry)
		if err != nil {
			return commentbus.BotRegistryEntry{}, err
		}
		if overlap {
			existing := registry.Bots[i]
			previousEntry = &existing
			entry = mergeBotletsRegistryEntryForUpsert(existing, entry)
			continue
		}
		nextBots = append(nextBots, registry.Bots[i])
	}
	if previousEntry != nil && extraProfiles == nil && preflight == nil && afterCommit == nil {
		repair, ok, err := prepareBotletsCredentialProfileRenameRepair(paths, botletsHome, *previousEntry, entry)
		if err != nil {
			return commentbus.BotRegistryEntry{}, err
		}
		if ok {
			entry = repair.entry
			if repair.writeCanonical {
				extraProfiles = map[string]commentbus.AgentProfile{repair.profileWrite.profile.Handle: repair.profileWrite.profile}
			}
			afterCommit = func(commentbus.BotRegistryEntry) error {
				if repair.writeCanonical {
					return writePreparedBotletsAgentProfileSetRollbackable(repair.profileWrite, repair.aliasWrites)
				}
				return writePreparedBotletsAgentProfileAliases(repair.aliasWrites)
			}
		}
	}
	registry.Bots = append(nextBots, entry)
	sort.Slice(registry.Bots, func(i, j int) bool {
		return registry.Bots[i].Name < registry.Bots[j].Name
	})
	profiles, _, profileErrors := commentbus.LoadAgentProfilesWithAliases(context.Background(), paths, "")
	for handle, profile := range extraProfiles {
		profiles[handle] = profile
	}
	if commentbus.HasFatalProfileReloadError(profileErrors) {
		return commentbus.BotRegistryEntry{}, fmt.Errorf("Botlets registry validation failed: %+v", profileErrors)
	}
	stateBots, registryErrors := commentbus.ValidateBotletsRegistryEntries(botletsHome, profiles, registry.Bots)
	errorsOut := append(profileErrors, registryErrors...)
	if commentbus.HasFatalProfileReloadError(errorsOut) {
		return commentbus.BotRegistryEntry{}, fmt.Errorf("Botlets registry validation failed: %+v", errorsOut)
	}
	if _, ok := stateBots[entry.Name]; !ok {
		return commentbus.BotRegistryEntry{}, errors.New("Botlets registry validation failed: bot was not loaded")
	}
	if preflight != nil {
		if err := preflight(entry); err != nil {
			return commentbus.BotRegistryEntry{}, err
		}
	}
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return commentbus.BotRegistryEntry{}, err
	}
	if err := commentbus.WritePrivateFileAtomic(registryPath, append(data, '\n'), 0o600); err != nil {
		return commentbus.BotRegistryEntry{}, err
	}
	if afterCommit != nil {
		if err := afterCommit(entry); err != nil {
			if rollbackErr := restoreBotletsRegistryFile(registryPath, previousRegistryData, previousRegistryExists); rollbackErr != nil {
				return commentbus.BotRegistryEntry{}, fmt.Errorf("%w (registry rollback failed: %v)", err, rollbackErr)
			}
			return commentbus.BotRegistryEntry{}, err
		}
	}
	return entry, nil
}

// removeBotletsRegistryEntryForHandle removes the registry entry whose
// canonical handle matches, under the same registry lock the upsert takes. It
// exists for the enrollment worker's terminal cleanup: a cancelled/expired/
// failed Botlets enrollment that already wired the registry must not leave an
// entry pointing at the credential profile the cleanup just removed
// (LoadBotletsRegistry would report it as MISSING_CREDENTIAL_PROFILE forever).
// A missing registry file or an absent entry is a clean no-op.
func removeBotletsRegistryEntryForHandle(botletsHome string, handle string) error {
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return nil
	}
	var err error
	botletsHome, err = commentbus.ValidateBotletsRegistryWriteTarget(botletsHome)
	if err != nil {
		return err
	}
	lockFile, err := openBotletsRegistryLock(filepath.Join(botletsHome, ".registry.lock"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // no registry dir — nothing to remove
		}
		return err
	}
	defer lockFile.Close()
	if err := lockBotletsRegistryFile(lockFile); err != nil {
		return err
	}
	defer unlockBotletsRegistryFile(lockFile)
	registryPath := filepath.Join(botletsHome, "registry.json")
	data, err := os.ReadFile(registryPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		return err
	}
	nextBots := make([]commentbus.BotRegistryEntry, 0, len(registry.Bots))
	removed := false
	for _, bot := range registry.Bots {
		if strings.EqualFold(strings.TrimSpace(bot.Handle), handle) {
			removed = true
			continue
		}
		nextBots = append(nextBots, bot)
	}
	if !removed {
		return nil
	}
	registry.Bots = nextBots
	out, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	return commentbus.WritePrivateFileAtomic(registryPath, append(out, '\n'), 0o600)
}

// botletsRegistryEntryExistsForHandle reports whether the Botlets registry has
// an entry whose canonical handle matches. It is the completeness signal the
// cleanup paths use to tell "registry rollback still pending" (entry present,
// profile already gone -> ours, retry the removal) apart from "already clean /
// not ours" (no entry -> safe to prune the journal record). A missing registry
// dir or file is reported as absent (false) with no error.
func botletsRegistryEntryExistsForHandle(botletsHome string, handle string) (bool, error) {
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return false, nil
	}
	botletsHome, err := commentbus.ValidateBotletsRegistryWriteTarget(botletsHome)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(botletsHome, "registry.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		return false, err
	}
	for _, bot := range registry.Bots {
		if strings.EqualFold(strings.TrimSpace(bot.Handle), handle) {
			return true, nil
		}
	}
	return false, nil
}

// readBotletsRegistryEntryForHandle returns the registry entry whose canonical
// handle matches, or (nil, nil) when there is none (missing dir/file/entry). It
// is the snapshot source for rolling a handle's entry back to its pre-existing
// state when a terminal Botlets enrollment's profile is restored from backup.
func readBotletsRegistryEntryForHandle(botletsHome string, handle string) (*commentbus.BotRegistryEntry, error) {
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return nil, nil
	}
	botletsHome, err := commentbus.ValidateBotletsRegistryWriteTarget(botletsHome)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(botletsHome, "registry.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		return nil, err
	}
	for _, bot := range registry.Bots {
		if strings.EqualFold(strings.TrimSpace(bot.Handle), handle) {
			entry := bot
			return &entry, nil
		}
	}
	return nil, nil
}

// restorePreexistingBotletsRegistryEntry rolls the handle's registry entry back
// to the snapshot captured before a terminal enrollment overwrote it. A
// zero-length snapshot is a no-op (the install created the handle fresh — the
// caller's other branch removes the entry instead).
//
// Implemented as a single upsert of the snapshot, deliberately WITHOUT a
// pre-remove. upsert's overlap-merge replaces the same-identity slot and
// preserves the snapshot's BrainRef, ManagedSession (runtime), and
// ScheduleTimezone wholesale — those are exactly the fields a dead enrollment
// could have changed — while only filling an empty BotID/DisplayName/alias from
// the current entry (identical for the same bot). Avoiding the pre-remove keeps
// the operation crash- and validation-safe: upsert validates the whole registry
// BEFORE writing, so a snapshot that fails to validate leaves the existing entry
// intact rather than deleting it and then failing to put the replacement back.
func restorePreexistingBotletsRegistryEntry(paths commentbus.Paths, botletsHome string, handle string, snapshot json.RawMessage) error {
	if len(snapshot) == 0 {
		return nil
	}
	var prev commentbus.BotRegistryEntry
	if err := json.Unmarshal(snapshot, &prev); err != nil {
		return err
	}
	return upsertBotletsRegistry(paths, botletsHome, prev)
}

func restoreBotletsRegistryFile(path string, previous []byte, exists bool) error {
	if exists {
		return commentbus.WritePrivateFileAtomic(path, previous, 0o600)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func mergeBotletsRegistryEntryForUpsert(existing commentbus.BotRegistryEntry, next commentbus.BotRegistryEntry) commentbus.BotRegistryEntry {
	if next.BotID == "" {
		next.BotID = existing.BotID
	}
	if next.DisplayName == "" {
		next.DisplayName = existing.DisplayName
	}
	preferredSlugAliases := append([]string{}, next.SlugAliases...)
	preferredSlugAliases = append(preferredSlugAliases, existing.Name)
	preferredHandleAliases := append([]string{}, next.HandleAliases...)
	preferredHandleAliases = append(preferredHandleAliases, existing.Handle)
	next.SlugAliases = mergeBotletsRegistryAliases(next.Name, preferredSlugAliases, existing.SlugAliases)
	next.HandleAliases = mergeBotletsRegistryAliases(next.Handle, preferredHandleAliases, existing.HandleAliases)
	return next
}

func mergeBotletsRegistryAliases(current string, preferred []string, fallback []string) []string {
	out := make([]string, 0, len(preferred)+len(fallback))
	seen := map[string]struct{}{}
	if current = strings.TrimSpace(current); current != "" {
		seen[strings.ToLower(current)] = struct{}{}
	}
	for _, labels := range [][]string{preferred, fallback} {
		for _, label := range labels {
			label = strings.TrimSpace(label)
			if label == "" {
				continue
			}
			key := strings.ToLower(label)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, label)
		}
	}
	return out
}

func botletsRegistryEntriesOverlap(existing commentbus.BotRegistryEntry, next commentbus.BotRegistryEntry) (bool, error) {
	if next.BotID != "" && existing.BotID == next.BotID {
		existingBotAgentID := botletsRegistryEntryBotAgentID(existing)
		nextBotAgentID := botletsRegistryEntryBotAgentID(next)
		if existingBotAgentID != "" && nextBotAgentID != "" && existingBotAgentID != nextBotAgentID {
			return false, fmt.Errorf("Botlets registry bot id %q already belongs to another bot agent", next.BotID)
		}
		return true, nil
	}
	if existing.BrainRef != nil && next.BrainRef != nil && existing.BrainRef.BotAgentID != "" && existing.BrainRef.BotAgentID == next.BrainRef.BotAgentID {
		if existing.BotID != "" && next.BotID != "" && existing.BotID != next.BotID {
			return false, fmt.Errorf("Botlets registry bot agent %q already belongs to another bot", existing.BrainRef.BotAgentID)
		}
		return true, nil
	}
	existingLabels := map[string]bool{}
	for _, label := range []string{existing.Name, existing.Handle} {
		if strings.TrimSpace(label) == "" {
			continue
		}
		existingLabels[strings.ToLower(label)] = true
	}
	for _, label := range append(existing.SlugAliases, existing.HandleAliases...) {
		if strings.TrimSpace(label) == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := existingLabels[key]; !ok {
			existingLabels[key] = false
		}
	}
	nextLabels := map[string]bool{}
	for _, label := range []string{next.Name, next.Handle} {
		if strings.TrimSpace(label) == "" {
			continue
		}
		nextLabels[strings.ToLower(label)] = true
	}
	for _, label := range append(next.SlugAliases, next.HandleAliases...) {
		if strings.TrimSpace(label) == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := nextLabels[key]; !ok {
			nextLabels[key] = false
		}
	}
	for key, nextCanonical := range nextLabels {
		if existingCanonical, ok := existingLabels[key]; ok {
			existingIdentity := firstNonEmpty(existing.BotID, botletsRegistryEntryBotAgentID(existing))
			nextIdentity := firstNonEmpty(next.BotID, botletsRegistryEntryBotAgentID(next))
			if existingIdentity != "" && nextIdentity != "" && existingIdentity != nextIdentity {
				return false, fmt.Errorf("Botlets registry label %q already belongs to another bot", key)
			}
			if existingIdentity != "" && nextIdentity != "" {
				return true, nil
			}
			if !existingCanonical || !nextCanonical {
				return false, fmt.Errorf("Botlets registry label %q already belongs to another bot", key)
			}
			return true, nil
		}
	}
	return false, nil
}

func botletsRegistryEntryBotAgentID(entry commentbus.BotRegistryEntry) string {
	if entry.BrainRef == nil {
		return ""
	}
	return entry.BrainRef.BotAgentID
}

func botletsRegistryEntryHasIdentity(entry commentbus.BotRegistryEntry, botID string, botAgentID string) bool {
	if botID != "" && entry.BotID != "" && entry.BotID == botID {
		return true
	}
	entryBotAgentID := botletsRegistryEntryBotAgentID(entry)
	return botAgentID != "" && entryBotAgentID != "" && entryBotAgentID == botAgentID
}

func reloadBotletsProfiles(ctx context.Context, paths commentbus.Paths, botletsHome string) (any, string) {
	auth, err := reloadBotletsProfilesAuth(paths)
	if err != nil {
		return nil, err.Error()
	}
	response, err := callSocket(ctx, paths, "reload-profiles", auth, map[string]any{"botlets_home": botletsHome}, 10*time.Second)
	if err != nil {
		return nil, err.Error()
	}
	if !response.OK {
		if err := socketResponseError(response); err != nil {
			return response, err.Error()
		}
	}
	if errText := botletsProfileReloadError(response.Result); errText != "" {
		return response.Result, errText
	}
	return response.Result, ""
}

func reloadBotletsProfilesAuth(paths commentbus.Paths) (*commentbus.SocketAuth, error) {
	auth, err := ownerOnlyAuth(paths, "")
	if err == nil {
		return auth, nil
	}
	if !isManagedSessionOwnerOnlyReloadError(err.Error()) {
		return nil, err
	}
	auth, _, present, sessionErr := sessionAuthFromEnv(paths)
	if sessionErr != nil {
		return nil, sessionErr
	}
	if !present {
		return nil, err
	}
	return auth, nil
}

func botletsProfileReloadError(result any) string {
	var reload commentbus.ProfileReloadResult
	switch value := result.(type) {
	case commentbus.ProfileReloadResult:
		reload = value
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return "daemon profile reload returned an unreadable result"
		}
		if err := json.Unmarshal(data, &reload); err != nil {
			return "daemon profile reload returned an unreadable result"
		}
	}
	if len(reload.Errors) == 0 {
		return ""
	}
	parts := make([]string, 0, len(reload.Errors))
	for _, item := range reload.Errors {
		label := strings.TrimSpace(item.Code)
		message := strings.TrimSpace(item.Message)
		if label != "" && message != "" {
			parts = append(parts, label+": "+message)
		} else if label != "" {
			parts = append(parts, label)
		} else if message != "" {
			parts = append(parts, message)
		}
	}
	if len(parts) == 0 {
		return fmt.Sprintf("daemon profile reload reported %d error(s)", len(reload.Errors))
	}
	return fmt.Sprintf("daemon profile reload reported %d error(s): %s", len(reload.Errors), strings.Join(parts, "; "))
}

func runBotletsStatus(args []string) error {
	fs := flag.NewFlagSet("comment botlets status", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	botletsHomeFlag := fs.String("botlets-home", "", "Botlets home directory")
	bot := fs.String("bot", "", "bot name or handle")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("botlets status does not accept positional arguments")
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	botletsHome, err := commentbus.ResolveBotletsHome(firstNonEmpty(*botletsHomeFlag, persistedCLIBotletsHome(paths, "")))
	if err != nil {
		return err
	}
	syncStatus, syncErr := commentsync.ReadStatus(commentsync.Options{Home: paths.Home})
	state, profileErrors := commentbus.LoadProfileState(context.Background(), commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	daemonConnected, daemonHealth, daemonErr := botletsDaemonHealth(context.Background(), paths)
	result := map[string]any{
		"ok":               len(profileErrors) == 0 && syncErr == nil,
		"home":             paths.Home,
		"botlets_home":     botletsHome,
		"sync":             syncStatus,
		"sync_error":       errorString(syncErr),
		"daemon_connected": daemonConnected,
		"daemon_health":    daemonHealth,
		"daemon_error":     daemonErr,
		"profile_errors":   profileErrors,
		"bots_loaded":      len(state.BotRegistry),
		"profiles_loaded":  len(state.AgentProfiles),
	}
	if *bot != "" {
		entry, profile, ok, selectErr := selectBotletsStatusBot(state, *bot)
		if selectErr != nil {
			return selectErr
		}
		result["bot_found"] = ok
		if ok {
			brainPath := ""
			brainExists := false
			brainError := ""
			if entry.BrainRef != nil && syncStatus.Root != "" {
				if path, err := commentbus.ValidateBotletsBrainProjection(paths, entry); err == nil {
					brainPath = path
					brainExists = true
				} else {
					brainPath = filepath.Join(syncStatus.Root, filepath.FromSlash(entry.BrainRef.RelativePath))
					brainError = err.Error()
				}
			}
			result["bot"] = map[string]any{
				"name":               entry.Name,
				"handle":             entry.Handle,
				"profile_loaded":     profile.Handle != "",
				"credential_profile": entry.CredentialProfile,
				"brain_ref":          entry.BrainRef,
				"brain_path":         brainPath,
				"brain_exists":       brainExists,
				"brain_error":        brainError,
			}
		}
	}
	if *jsonOut {
		return printJSON(result)
	}
	fmt.Printf("home: %s\n", paths.Home)
	fmt.Printf("botlets_home: %s\n", botletsHome)
	fmt.Printf("sync_configured: %v\n", syncStatus.Configured)
	fmt.Printf("daemon_connected: %v\n", daemonConnected)
	fmt.Printf("bots_loaded: %d\n", len(state.BotRegistry))
	if *bot != "" {
		fmt.Printf("bot_found: %v\n", result["bot_found"])
	}
	if syncErr != nil {
		fmt.Printf("sync_error: %s\n", syncErr)
	}
	if daemonErr != "" {
		fmt.Printf("daemon_error: %s\n", daemonErr)
	}
	for _, err := range profileErrors {
		fmt.Printf("profile_error: %s %s\n", err.Code, err.Message)
	}
	return nil
}

func botletsDaemonHealth(ctx context.Context, paths commentbus.Paths) (bool, any, string) {
	response, err := callSocket(ctx, paths, "health", nil, nil, 3*time.Second)
	if err != nil {
		return false, nil, err.Error()
	}
	if !response.OK {
		if err := socketResponseError(response); err != nil {
			return false, response, err.Error()
		}
	}
	return true, response.Result, ""
}

func selectBotletsStatusBot(state commentbus.ProfileState, selector string) (commentbus.BotRegistryEntry, commentbus.AgentProfile, bool, error) {
	entry, ok, err := selectBotletsEntryBySelector(state.BotRegistry, selector)
	if !ok || err != nil {
		return commentbus.BotRegistryEntry{}, commentbus.AgentProfile{}, ok, err
	}
	return entry, state.AgentProfiles[entry.Handle], true, nil
}

func selectBotletsEntryBySelector(registry map[string]commentbus.BotRegistryEntry, selector string) (commentbus.BotRegistryEntry, bool, error) {
	selector = strings.TrimSpace(selector)
	var exactMatches []commentbus.BotRegistryEntry
	for _, entry := range registry {
		if entry.MatchesSlug(selector) || entry.MatchesProfile(selector) {
			exactMatches = append(exactMatches, entry)
		}
	}
	if len(exactMatches) > 1 {
		return commentbus.BotRegistryEntry{}, false, fmt.Errorf("Botlets bot name %q is ambiguous; use the full handle", selector)
	}
	if len(exactMatches) == 1 {
		return exactMatches[0], true, nil
	}
	var suffixMatches []commentbus.BotRegistryEntry
	for _, entry := range registry {
		if botletsStatusMatchesHandleSuffix(entry, selector) {
			suffixMatches = append(suffixMatches, entry)
		}
	}
	if len(suffixMatches) > 1 {
		return commentbus.BotRegistryEntry{}, false, fmt.Errorf("Botlets bot name %q is ambiguous; use the full handle", selector)
	}
	if len(suffixMatches) == 1 {
		return suffixMatches[0], true, nil
	}
	return commentbus.BotRegistryEntry{}, false, nil
}

func botletsStatusMatchesHandleSuffix(entry commentbus.BotRegistryEntry, selector string) bool {
	key := strings.ToLower(strings.TrimSpace(selector))
	if key == "" || strings.Contains(key, ".") {
		return false
	}
	for _, handle := range append([]string{entry.Handle}, entry.HandleAliases...) {
		_, suffix, ok := strings.Cut(handle, ".")
		if ok && strings.ToLower(suffix) == key {
			return true
		}
	}
	return false
}

func parseBotletsBotSelector(owner string, bot string) (string, string, error) {
	bot = strings.TrimSpace(bot)
	owner = strings.TrimPrefix(strings.TrimSpace(owner), "@")
	if bot == "" {
		return "", "", errors.New("--bot is required")
	}
	if strings.Contains(bot, ".") && owner == "" {
		parts := strings.SplitN(bot, ".", 2)
		owner = strings.TrimPrefix(parts[0], "@")
		bot = parts[1]
	}
	bot = strings.TrimSpace(bot)
	if owner != "" && !commentbus.ProfileRE.MatchString(owner+".placeholder") {
		return "", "", errors.New("invalid owner handle")
	}
	if owner == "" {
		return "", "", errors.New("owner handle is required; pass --bot <owner.slug> or --owner <handle>")
	}
	if bot == "" || strings.ContainsAny(bot, "/\\ \t\r\n") {
		return "", "", errors.New("invalid bot slug")
	}
	return owner, bot, nil
}

func defaultBotletsDeviceLabel() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "Botlets local runtime"
	}
	return "Botlets local runtime on " + strings.TrimSpace(host)
}
