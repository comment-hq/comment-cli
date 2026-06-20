package commentbus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	RuntimeRoleMain = "main"
	RuntimeRoleTask = "task"

	defaultTmuxPollInterval = time.Second
	minTmuxPollInterval     = 250 * time.Millisecond
	maxTmuxPollInterval     = time.Minute
)

var (
	transientRuntimeStartupInputReadyWait  = 5 * time.Second
	transientRuntimeStartupInputReadyPoll  = 100 * time.Millisecond
	transientRuntimeStartupInputQuietDelay = 1250 * time.Millisecond
)

func DefaultTmuxPollInterval() time.Duration {
	value := strings.TrimSpace(os.Getenv("COMMENT_IO_TMUX_POLL_INTERVAL"))
	if value == "" {
		return defaultTmuxPollInterval
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return defaultTmuxPollInterval
	}
	return clampTmuxPollInterval(parsed)
}

func normalizeTmuxPollInterval(value time.Duration) time.Duration {
	if value <= 0 {
		value = DefaultTmuxPollInterval()
	}
	return clampTmuxPollInterval(value)
}

func clampTmuxPollInterval(value time.Duration) time.Duration {
	if value < minTmuxPollInterval {
		return minTmuxPollInterval
	}
	if value > maxTmuxPollInterval {
		return maxTmuxPollInterval
	}
	return value
}

func normalizeTmuxPollSlowInterval(value time.Duration) time.Duration {
	value = normalizeTmuxPollInterval(value)
	slow := 5 * value
	if slow < 5*time.Second {
		return 5 * time.Second
	}
	if slow > maxTmuxPollInterval {
		return maxTmuxPollInterval
	}
	return slow
}

type TransientRuntimeRecord struct {
	RunID              string   `json:"run_id"`
	Host               string   `json:"host,omitempty"`
	BmuxBinary         string   `json:"bmux_binary,omitempty"`
	Profile            string   `json:"profile"`
	Role               string   `json:"role"`
	BotName            string   `json:"bot_name"`
	BotID              string   `json:"bot_id,omitempty"`
	BotAgentID         string   `json:"bot_agent_id,omitempty"`
	SessionName        string   `json:"session_name"`
	PaneTarget         string   `json:"pane_target"`
	Runtime            string   `json:"runtime"`
	RuntimeCommand     []string `json:"runtime_command"`
	RuntimeCommandPath string   `json:"runtime_command_path"`
	CommentCommandPath string   `json:"comment_command_path,omitempty"`
	OutputLogPath      string   `json:"output_log_path,omitempty"`
	RuntimePath        string   `json:"runtime_path"`
	CWD                string   `json:"cwd"`
	Env                []string `json:"env,omitempty"`
	State              string   `json:"state"`
	StartedAt          string   `json:"started_at"`
	// RuntimeLaunchMode selects how the runtime is launched: "path" (legacy:
	// resolve+exec a trusted absolute binary path) or "shell" (resolve the
	// runtime name through the user's interactive login shell, supporting PATH
	// binaries, aliases, and functions). In "shell" mode RuntimePath /
	// RuntimeCommandPath carry no meaning and are empty. Empty normalizes to
	// "path" for legacy records.
	RuntimeLaunchMode string `json:"runtime_launch_mode,omitempty"`
}

const (
	RuntimeLaunchModePath  = "path"
	RuntimeLaunchModeShell = "shell"
)

// normalizeRuntimeLaunchMode coerces a stored/legacy launch mode to a known
// value. Anything other than the explicit "shell" marker (including "" on
// legacy records that predate the field) is treated as "path", preserving the
// pre-shell-native behavior.
func normalizeRuntimeLaunchMode(mode string) string {
	if mode == RuntimeLaunchModeShell {
		return RuntimeLaunchModeShell
	}
	return RuntimeLaunchModePath
}

type transientRuntime struct {
	record        TransientRuntimeRecord
	cancel        context.CancelFunc
	done          chan struct{}
	expectedNames map[string]struct{}

	mu                  sync.Mutex
	lastNudgedMessageID string
	// lastRewakeSkipMessageID dedupes the async-rewake skip log only; it never
	// short-circuits the skip check (which must re-evaluate the live waiter each
	// poll so a waiter that dies promptly falls back to the keystroke nudge).
	lastRewakeSkipMessageID string
}

type transientRuntimeStartResult struct {
	Record   TransientRuntimeRecord
	Existing bool
}

type TransientRuntimeStatus struct {
	Runtime TransientRuntimeRecord `json:"runtime"`
	Health  string                 `json:"health"`
}

func (d *Daemon) startTransientRuntime(req SocketRequest) (transientRuntimeStartResult, *SocketError) {
	profile := req.Params["profile"].(string)
	role := transientRuntimeRoleFromParams(req.Params)
	if scopeErr := requireProfileScopedOwnerAuth(req, profile); scopeErr != nil {
		return transientRuntimeStartResult{}, scopeErr
	}
	command, ok := runtimeCommandParam(req.Params["runtime_command"])
	if !ok {
		return transientRuntimeStartResult{}, socketError("VALIDATION_ERROR", "invalid runtime command", false)
	}
	clientCommandPath, _ := req.Params["runtime_command_path"].(string)
	cwd, cwdErr := resolveTrustedTmuxLaunchDir(req.Params["cwd"].(string), "runtime working directory")
	if cwdErr != nil {
		return transientRuntimeStartResult{}, socketError("VALIDATION_ERROR", "invalid runtime working directory", false)
	}
	profileConfig, bot, targetOK := d.cloudNotificationTarget(profile, "", true)
	if !targetOK || profileConfig.Handle != profile || bot.Handle != profile {
		return transientRuntimeStartResult{}, socketError("NOT_FOUND", "profile is not loaded", false)
	}
	if role == RuntimeRoleMain {
		// Single-listener: if an impromptu `/comment listen` session already holds a
		// claim on this handle, a main transient runtime would double-deliver (it gets
		// nudged while the impromptu waiter also pulls). Refuse, symmetric with
		// listen.claim refusing a handle that has an active transient runtime.
		// reserveMainEstablish does this check AND marks the handle as starting under
		// listenEstablishMu, so a listen.claim racing in cannot also win the handle in
		// the window before our reservation lands. Held (via defer) for the whole
		// launch; a failed launch releases it and the runtime reservation rolls back.
		releaseEstablish, claim, blocked := d.reserveMainEstablish(profile)
		if blocked {
			message := "handle " + profile + " is being listened to by an impromptu session"
			if claim.ClaimedBy != "" {
				message += " (" + claim.ClaimedBy + ")"
			}
			message += "; detach it with `comment listen release` before running it here"
			return transientRuntimeStartResult{}, socketError("HANDLE_BUSY", message, false)
		}
		defer releaseEstablish()
		if runtime := d.activeMainTransientRuntimeForTarget(profile, bot); runtime != nil {
			runtimeErr := d.verifyTransientRuntime(context.Background(), runtime)
			if runtimeErr == nil || runtimeErr.Code == "PANE_BUSY" {
				return transientRuntimeStartResult{Record: runtime.record, Existing: true}, nil
			}
			if runtimeErr.Code != "CONFLICT" {
				return transientRuntimeStartResult{Record: runtime.record, Existing: true}, nil
			}
			if !d.forgetInactiveTransientRuntime(runtime, runtimeErr) && d.activeMainTransientRuntimeForTarget(profile, bot) != nil {
				return transientRuntimeStartResult{}, socketError("CONFLICT", "runtime is already active for profile", false)
			}
		}
	}
	resolution, resolveErr := resolveTransientRuntimeCommandWithPath(command, clientCommandPath)
	if resolveErr != nil {
		return transientRuntimeStartResult{}, socketError("UPSTREAM_ERROR", "could not trust runtime binary: "+resolveErr.Error(), true)
	}
	commentCommandPath, commentErr := d.prepareTransientRuntimeCommentCommand()
	if commentErr != nil {
		return transientRuntimeStartResult{}, socketError("UPSTREAM_ERROR", "could not prepare comment command: "+commentErr.Error(), true)
	}
	runID, idErr := GenerateLocalID("sess", 0)
	if idErr != nil {
		return transientRuntimeStartResult{}, socketError("UPSTREAM_ERROR", "could not allocate runtime id", true)
	}
	outputLogPath, outputLogErr := transientRuntimeOutputLogPath(d.paths, runID)
	if outputLogErr != nil {
		return transientRuntimeStartResult{}, socketError("UPSTREAM_ERROR", "could not prepare runtime output log", true)
	}
	sessionName, nameErr := transientRuntimeSessionName(profile, runID)
	if nameErr != nil {
		return transientRuntimeStartResult{}, socketError("VALIDATION_ERROR", "could not build runtime session name", false)
	}
	startedAt := busTime(time.Now().UTC())
	// Transient `comment run` runtimes default to tmux. bmux remains an explicit
	// opt-in: a pinned absolute bmux binary (COMMENT_IO_BMUX_BIN / --bmux-bin,
	// surfaced here as a non-empty record value) selects the bmux host. Without a
	// pin the bare "bmux" default resolves to an empty record value, so we stay on
	// tmux. Managed/botlet sessions choose their host via the profile instead.
	bmuxBinaryValue := transientRuntimeBmuxBinaryRecordValue(d.bmuxBinary)
	transientHost := defaultNewSessionHost()
	if bmuxBinaryValue != "" {
		transientHost = SessionHostBmux
	}
	record := TransientRuntimeRecord{
		RunID:              runID,
		Host:               transientHost,
		BmuxBinary:         bmuxBinaryValue,
		Profile:            profile,
		Role:               role,
		BotName:            bot.Name,
		BotID:              bot.BotID,
		BotAgentID:         botAgentID(bot),
		SessionName:        sessionName,
		Runtime:            command[0],
		RuntimeCommand:     append([]string{}, command...),
		RuntimeCommandPath: resolution.CommandPath,
		CommentCommandPath: commentCommandPath,
		OutputLogPath:      outputLogPath,
		RuntimePath:        resolution.RuntimePath,
		CWD:                cwd,
		Env:                localSyncRuntimeEnv(d.paths),
		State:              "starting",
		StartedAt:          startedAt,
	}

	if reserved := d.reserveTransientRuntimeRecord(record); !reserved {
		if !d.clearInactiveTransientRuntimeForTarget(profile, bot) && d.activeMainTransientRuntimeForTarget(profile, bot) != nil {
			return transientRuntimeStartResult{}, socketError("CONFLICT", "runtime is already active for profile", false)
		}
		if !d.reserveTransientRuntimeRecord(record) {
			return transientRuntimeStartResult{}, socketError("CONFLICT", "runtime is already active for profile", false)
		}
	}
	releaseReservation := true
	defer func() {
		if releaseReservation {
			d.removeTransientRuntimeReservation(profile, role, runID)
		}
	}()

	commandLine := transientRuntimeLaunchCommand(record)
	controller := d.controllerForHost(record.Host)
	if err := controller.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName:       record.SessionName,
		WorkingDir:        record.CWD,
		CommentHome:       d.paths.Home,
		BotletsHome:       d.paths.Home,
		Command:           commandLine,
		OutputPipeCommand: transientRuntimeOutputPipeCommand(record.OutputLogPath, record.CommentCommandPath),
	}); err != nil {
		return transientRuntimeStartResult{}, launchTransientRuntimeSocketError(record, err)
	}
	paneTarget, paneErr := controller.PaneTarget(context.Background(), record.SessionName)
	if paneErr != nil {
		if errors.Is(paneErr, ErrTmuxSessionMissing) {
			record.State = "exited"
			d.logger.warn("runtime.exited_before_attach", transientRuntimeLogData(record))
			return transientRuntimeStartResult{Record: record}, nil
		}
		_ = controller.KillSession(context.Background(), record.SessionName)
		return transientRuntimeStartResult{}, socketError("UPSTREAM_ERROR", "could not resolve runtime pane target", true)
	}
	record.PaneTarget = paneTarget
	expected := expectedTransientRuntimeCommandNames(record)
	if runtimeErr := d.waitForTransientRuntime(record, expected, sessionRuntimeStartupTimeout); runtimeErr != nil {
		if runtimeErr.Code == "CONFLICT" {
			record.State = "exited"
			d.logger.warn("runtime.exited_before_attach", transientRuntimeLogData(record))
			return transientRuntimeStartResult{Record: record}, nil
		}
		_ = controller.KillSession(context.Background(), record.SessionName)
		return transientRuntimeStartResult{}, runtimeErr
	}
	if startupErr := d.sendTransientRuntimeStartupInstruction(context.Background(), record, profileConfig.BaseURL, profileConfig.Handle, bot); startupErr != nil {
		logData := transientRuntimeLogData(record)
		logData["error_code"] = startupErr.Code
		d.logger.warn("runtime.startup_instruction_failed", logData)
	}
	record.State = "alive"
	if err := d.store.PutTransientRuntime(context.Background(), record); err != nil {
		_ = controller.KillSession(context.Background(), record.SessionName)
		return transientRuntimeStartResult{}, socketError("UPSTREAM_ERROR", "could not persist runtime", true)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runtime := &transientRuntime{
		record:        record,
		cancel:        cancel,
		done:          make(chan struct{}),
		expectedNames: expected,
	}
	d.activateTransientRuntime(profile, role, runID, runtime)
	releaseReservation = false
	d.launchTransientRuntime(ctx, runtime)
	d.logger.info("runtime.started", transientRuntimeLogData(record))
	return transientRuntimeStartResult{Record: record}, nil
}

func launchTransientRuntimeSocketError(record TransientRuntimeRecord, err error) *SocketError {
	if errors.Is(err, ErrTmuxNotInstalled) {
		return socketError(SocketErrorCodeTmuxNotInstalled, TmuxNotInstalledMessage(), true)
	}
	if errors.Is(err, ErrBmuxNotInstalled) {
		return socketError(SocketErrorCodeBmuxNotInstalled, BmuxNotInstalledMessage(), true)
	}
	message := "could not launch runtime"
	if normalizeSessionHost(record.Host) == SessionHostBmux {
		detail := strings.TrimSpace(err.Error())
		if strings.Contains(detail, BmuxBinaryEnv) && !containsSecretValue(detail) && !strings.ContainsAny(detail, "\r\n\x00") {
			message += ": " + detail
		}
	}
	return socketError("UPSTREAM_ERROR", message, true)
}

func (d *Daemon) sendTransientRuntimeStartupInstruction(ctx context.Context, record TransientRuntimeRecord, baseURL string, handle string, bot BotRegistryEntry) *SocketError {
	if normalizeSessionHost(record.Host) == SessionHostTmux {
		d.waitForTransientRuntimeStartupInput(ctx, d.tmux, record)
		return d.sendTmuxStartupInstruction(ctx, record.SessionName, record.PaneTarget, baseURL, handle, bot)
	}
	controller := d.controllerForHost(record.Host)
	if _, ok := controller.(bmuxOutputWaiter); !ok {
		d.waitForTransientRuntimeStartupInput(ctx, controller, record)
		return d.sendTransientRuntimeStartupInstructionWithController(ctx, controller, record, baseURL, handle, bot)
	}
	sessionRecord := transientRuntimeSessionRecord(record)
	if readyErr := d.waitForBmuxStartupInputReady(ctx, sessionRecord); readyErr != nil {
		if readyErr.Code == "CONFLICT" {
			return readyErr
		}
		data := transientRuntimeLogData(record)
		data["error_code"] = readyErr.Code
		data["error_message"] = readyErr.Message
		d.logger.warn("runtime.startup_ready_marker_fallback", data)
	}
	d.clearClaudeStartupGateBlind(ctx, controller, record.PaneTarget)
	return d.sendTransientRuntimeStartupInstructionWithController(ctx, controller, record, baseURL, handle, bot)
}

func (d *Daemon) sendTransientRuntimeStartupInstructionWithController(ctx context.Context, controller TmuxController, record TransientRuntimeRecord, baseURL string, handle string, bot BotRegistryEntry) *SocketError {
	if bot.BrainRef != nil {
		if sent, sendErr := d.sendBotletsMultilineOrientationWithController(ctx, controller, record.PaneTarget, handle, bot, ""); sent {
			return sendErr
		}
	}
	instruction, err := d.buildStartupInstructionForBot(baseURL, handle, bot)
	if err != nil {
		d.logger.warn("runtime.startup_instruction.skipped", map[string]any{
			"session_name": record.SessionName,
			"handle":       handle,
			"bot_name":     bot.Name,
			"error":        err.Error(),
		})
		return socketError("VALIDATION_ERROR", "invalid startup instruction", false)
	}
	if _, err := d.sendPrompt(ctx, controller, record.PaneTarget, instruction); err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			return socketError("CONFLICT", "session is not running", false)
		}
		return socketError("UPSTREAM_ERROR", "could not send startup instruction", true)
	}
	if err := d.waitForTmuxSubmitSettle(ctx); err != nil {
		return socketError("CANCELED", "startup instruction canceled before submit", false)
	}
	if err := controller.SendEnter(ctx, record.PaneTarget); err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			return socketError("CONFLICT", "session is not running", false)
		}
		return socketError("UPSTREAM_ERROR", "could not submit startup instruction", true)
	}
	return nil
}

func transientRuntimeSessionRecord(record TransientRuntimeRecord) SessionRecord {
	return SessionRecord{
		Host:               record.Host,
		Profile:            record.Profile,
		BotName:            record.BotName,
		BotID:              record.BotID,
		BotAgentID:         record.BotAgentID,
		SessionName:        record.SessionName,
		PaneTarget:         record.PaneTarget,
		Runtime:            record.Runtime,
		RuntimePath:        record.RuntimePath,
		RuntimeCommandPath: record.RuntimeCommandPath,
		RuntimeCommand:     append([]string{}, record.RuntimeCommand...),
		RuntimeLaunchMode:  record.RuntimeLaunchMode,
		WorkingDir:         record.CWD,
		OutputLogPath:      record.OutputLogPath,
		State:              record.State,
	}
}

func transientRuntimeBmuxBinaryRecordValue(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || !isSafeAbsoluteLocalPath(value) {
		return ""
	}
	return filepath.Clean(value)
}

func runtimeCommandParam(value any) ([]string, bool) {
	switch v := value.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			text, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, text)
		}
		return out, len(out) > 0
	case []string:
		return append([]string{}, v...), len(v) > 0
	default:
		return nil, false
	}
}

func transientRuntimeRoleFromParams(params map[string]any) string {
	role, _ := params["role"].(string)
	if role == RuntimeRoleTask {
		return RuntimeRoleTask
	}
	return RuntimeRoleMain
}

func isTransientRuntimeRole(role string) bool {
	return role == RuntimeRoleMain || role == RuntimeRoleTask
}

func (d *Daemon) activeMainTransientRuntimeForTarget(profile string, bot BotRegistryEntry) *transientRuntime {
	d.transientRuntimeMu.Lock()
	defer d.transientRuntimeMu.Unlock()
	if runtime := d.activeMainTransientRuntimeLocked(profile); runtime != nil {
		return runtime
	}
	return d.findMainTransientRuntimeByStableIdentityLocked(profile, bot.BotID, botAgentID(bot))
}

func (d *Daemon) activeMainTransientRuntimeLocked(profile string) *transientRuntime {
	runID := d.transientRuntimeMainProfiles[profile]
	if runID == "" {
		return nil
	}
	return d.transientRuntimes[runID]
}

func (d *Daemon) findMainTransientRuntimeByStableIdentityLocked(profile string, botID string, botAgentID string) *transientRuntime {
	if botID == "" && botAgentID == "" {
		return nil
	}
	var match *transientRuntime
	for _, runtime := range d.transientRuntimes {
		if runtime == nil || runtime.record.Role != RuntimeRoleMain || runtime.record.Profile == profile {
			continue
		}
		if !sameStableBotIdentity(runtime.record.BotID, runtime.record.BotAgentID, botID, botAgentID) {
			continue
		}
		if match == nil || transientRuntimeRecordLess(runtime.record, match.record) {
			match = runtime
		}
	}
	return match
}

func transientRuntimeRecordLess(left TransientRuntimeRecord, right TransientRuntimeRecord) bool {
	if left.StartedAt != right.StartedAt {
		return left.StartedAt < right.StartedAt
	}
	return left.RunID < right.RunID
}

func (d *Daemon) reserveTransientRuntimeRecord(record TransientRuntimeRecord) bool {
	if record.Role != RuntimeRoleMain {
		return true
	}
	d.transientRuntimeMu.Lock()
	defer d.transientRuntimeMu.Unlock()
	if runID, ok := d.transientRuntimeMainProfiles[record.Profile]; ok && runID != record.RunID {
		return false
	}
	for _, key := range transientRuntimeStableReservationKeys(record.BotID, record.BotAgentID) {
		if runID := d.transientRuntimeMainIDs[key]; runID != "" && runID != record.RunID {
			return false
		}
	}
	for _, runtime := range d.transientRuntimes {
		if runtime == nil || runtime.record.RunID == record.RunID || runtime.record.Role != RuntimeRoleMain {
			continue
		}
		if sameStableBotIdentity(runtime.record.BotID, runtime.record.BotAgentID, record.BotID, record.BotAgentID) {
			return false
		}
	}
	d.addTransientRuntimeMainReservationLocked(record)
	return true
}

func (d *Daemon) clearInactiveTransientRuntimeForTarget(profile string, bot BotRegistryEntry) bool {
	runtime := d.activeMainTransientRuntimeForTarget(profile, bot)
	if runtime == nil {
		return false
	}
	runtimeErr := d.verifyTransientRuntime(context.Background(), runtime)
	if runtimeErr == nil || runtimeErr.Code != "CONFLICT" {
		return false
	}
	return d.forgetInactiveTransientRuntime(runtime, runtimeErr)
}

func (d *Daemon) activateTransientRuntime(profile string, role string, runID string, runtime *transientRuntime) {
	d.transientRuntimeMu.Lock()
	defer d.transientRuntimeMu.Unlock()
	if role == RuntimeRoleMain {
		d.addTransientRuntimeMainReservationLocked(runtime.record)
	}
	d.transientRuntimes[runID] = runtime
}

func (d *Daemon) removeTransientRuntimeReservation(profile string, role string, runID string) {
	if role != RuntimeRoleMain {
		return
	}
	d.transientRuntimeMu.Lock()
	defer d.transientRuntimeMu.Unlock()
	d.removeTransientRuntimeMainReservationsLocked(runID)
}

func (d *Daemon) stopTransientRuntimeForRequest(req SocketRequest) (map[string]any, *SocketError) {
	runID := req.Params["run_id"].(string)
	authProfile := ""
	if req.Auth != nil && req.Auth.Profile != nil {
		authProfile = *req.Auth.Profile
	}
	runtime, ok, err := d.removeTransientRuntimeForStop(runID, authProfile)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, socketError("NOT_FOUND", "runtime is not active", false)
	}
	runtime.cancel()
	<-runtime.done
	if err := d.store.DeleteTransientRuntime(context.Background(), runID); err != nil {
		return nil, socketError("UPSTREAM_ERROR", "could not forget runtime", true)
	}
	d.logger.info("runtime.stopped", transientRuntimeLogData(runtime.record))
	return map[string]any{"ok": true, "run_id": runID, "state": "stopped"}, nil
}

func (d *Daemon) transientRuntimeStatusForRequest(req SocketRequest) (map[string]any, *SocketError) {
	profile := req.Params["profile"].(string)
	if scopeErr := requireProfileScopedOwnerAuth(req, profile); scopeErr != nil {
		return nil, scopeErr
	}
	runtime := d.activeMainTransientRuntimeForTarget(profile, d.transientRuntimeBotForProfile(profile))
	if runtime == nil {
		return map[string]any{"runtime": nil}, nil
	}
	status, ok, err := d.inspectTrackedTransientRuntime(runtime)
	if err != nil {
		return nil, err
	}
	if !ok {
		return map[string]any{"runtime": nil}, nil
	}
	return map[string]any{"runtime": status.Runtime, "health": status.Health}, nil
}

func (d *Daemon) listTransientRuntimesForRequest(req SocketRequest) (map[string]any, *SocketError) {
	profile := req.Params["profile"].(string)
	if scopeErr := requireProfileScopedOwnerAuth(req, profile); scopeErr != nil {
		return nil, scopeErr
	}
	bot := d.transientRuntimeBotForProfile(profile)
	d.transientRuntimeMu.Lock()
	runtimes := make([]*transientRuntime, 0, len(d.transientRuntimes))
	for _, runtime := range d.transientRuntimes {
		if runtime != nil && transientRuntimeMatchesProfileOrStableIdentity(runtime.record, profile, bot) {
			runtimes = append(runtimes, runtime)
		}
	}
	d.transientRuntimeMu.Unlock()
	sort.Slice(runtimes, func(i, j int) bool {
		left := runtimes[i].record
		right := runtimes[j].record
		leftRank := transientRuntimeRoleRank(left.Role)
		rightRank := transientRuntimeRoleRank(right.Role)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if left.StartedAt != right.StartedAt {
			return left.StartedAt < right.StartedAt
		}
		return left.RunID < right.RunID
	})

	statuses := make([]TransientRuntimeStatus, 0, len(runtimes))
	for _, runtime := range runtimes {
		status, ok, err := d.inspectTrackedTransientRuntime(runtime)
		if err != nil {
			return nil, err
		}
		if ok {
			statuses = append(statuses, status)
		}
	}
	return map[string]any{"runtimes": statuses}, nil
}

func transientRuntimeRoleRank(role string) int {
	if role == RuntimeRoleMain {
		return 0
	}
	return 1
}

func (d *Daemon) inspectTrackedTransientRuntime(runtime *transientRuntime) (TransientRuntimeStatus, bool, *SocketError) {
	runtimeErr := d.verifyTransientRuntime(context.Background(), runtime)
	if runtimeErr == nil {
		return TransientRuntimeStatus{Runtime: runtime.record, Health: "healthy"}, true, nil
	}
	if runtimeErr.Code == "PANE_BUSY" {
		return TransientRuntimeStatus{Runtime: runtime.record, Health: "busy"}, true, nil
	}
	if runtimeErr.Code == "CONFLICT" || !runtimeErr.Retryable {
		d.forgetInactiveTransientRuntime(runtime, runtimeErr)
		return TransientRuntimeStatus{}, false, nil
	}
	return TransientRuntimeStatus{Runtime: runtime.record, Health: "unknown"}, true, nil
}

func (d *Daemon) forgetInactiveTransientRuntime(runtime *transientRuntime, runtimeErr *SocketError) bool {
	d.transientRuntimeMu.Lock()
	if d.transientRuntimes[runtime.record.RunID] != runtime {
		d.transientRuntimeMu.Unlock()
		return false
	}
	delete(d.transientRuntimes, runtime.record.RunID)
	d.removeTransientRuntimeMainReservationsLocked(runtime.record.RunID)
	d.transientRuntimeMu.Unlock()

	if runtime.cancel != nil {
		runtime.cancel()
	}
	if runtime.done != nil {
		<-runtime.done
	}
	if err := d.store.DeleteTransientRuntime(context.Background(), runtime.record.RunID); err != nil {
		d.logger.warn("runtime.persist_delete_failed", transientRuntimeLogData(runtime.record))
	}
	logData := transientRuntimeLogData(runtime.record)
	if runtimeErr != nil {
		logData["error_code"] = runtimeErr.Code
	}
	d.logger.warn("runtime.cleared_inactive", logData)
	return true
}

func (d *Daemon) removeTransientRuntimeForStop(runID string, authProfile string) (*transientRuntime, bool, *SocketError) {
	bot := d.transientRuntimeBotForProfile(authProfile)
	d.transientRuntimeMu.Lock()
	defer d.transientRuntimeMu.Unlock()
	runtime, ok := d.transientRuntimes[runID]
	if !ok || runtime == nil {
		return nil, false, nil
	}
	if authProfile == "" || !transientRuntimeMatchesProfileOrStableIdentity(runtime.record, authProfile, bot) {
		return nil, false, socketError("FORBIDDEN", "owner profile does not match runtime profile", false)
	}
	delete(d.transientRuntimes, runID)
	d.removeTransientRuntimeMainReservationsLocked(runID)
	return runtime, true, nil
}

func (d *Daemon) removeTransientRuntime(runtime *transientRuntime) {
	d.transientRuntimeMu.Lock()
	current := d.transientRuntimes[runtime.record.RunID]
	if current != runtime {
		d.transientRuntimeMu.Unlock()
		return
	}
	delete(d.transientRuntimes, runtime.record.RunID)
	d.removeTransientRuntimeMainReservationsLocked(runtime.record.RunID)
	d.transientRuntimeMu.Unlock()
	if err := d.store.DeleteTransientRuntime(context.Background(), runtime.record.RunID); err != nil {
		d.logger.warn("runtime.persist_delete_failed", transientRuntimeLogData(runtime.record))
	}
}

func (d *Daemon) stopAllTransientRuntimes() {
	d.transientRuntimeMu.Lock()
	runtimes := make([]*transientRuntime, 0, len(d.transientRuntimes))
	for runID, runtime := range d.transientRuntimes {
		if runtime != nil {
			runtimes = append(runtimes, runtime)
		}
		delete(d.transientRuntimes, runID)
	}
	for profile := range d.transientRuntimeMainProfiles {
		delete(d.transientRuntimeMainProfiles, profile)
	}
	for stableKey := range d.transientRuntimeMainIDs {
		delete(d.transientRuntimeMainIDs, stableKey)
	}
	d.transientRuntimeMu.Unlock()

	for _, runtime := range runtimes {
		runtime.cancel()
		<-runtime.done
	}
}

func (d *Daemon) reconcileTransientRuntimes(ctx context.Context) error {
	rows, err := d.store.listTransientRuntimeRows(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		record := row.Record
		if !row.Valid {
			if err := d.store.deleteTransientRuntimeRow(ctx, row.RunID); err != nil {
				return err
			}
			d.logger.warn("runtime.reconcile_dropped_invalid", transientRuntimeLogData(record))
			continue
		}
		record.Role = normalizeTransientRuntimeRole(record.Role)
		if !isTransientRuntimeRole(record.Role) || record.State != "alive" {
			if err := d.store.DeleteTransientRuntime(ctx, record.RunID); err != nil {
				return err
			}
			d.logger.warn("runtime.reconcile_dropped", transientRuntimeLogData(record))
			continue
		}
		expected := expectedTransientRuntimeCommandNames(record)
		runtime := &transientRuntime{
			record:        record,
			expectedNames: expected,
		}
		if runtimeErr := d.verifyTransientRuntime(ctx, runtime); runtimeErr != nil {
			if runtimeErr.Code == "CONFLICT" || !runtimeErr.Retryable {
				if err := d.store.DeleteTransientRuntime(ctx, record.RunID); err != nil {
					return err
				}
				logData := transientRuntimeLogData(record)
				logData["error_code"] = runtimeErr.Code
				d.logger.warn("runtime.reconcile_dropped", logData)
				continue
			}
			logData := transientRuntimeLogData(record)
			logData["error_code"] = runtimeErr.Code
			d.logger.warn("runtime.reconcile_degraded", logData)
		}
		if !d.reserveTransientRuntimeRecord(record) {
			if err := d.store.DeleteTransientRuntime(ctx, record.RunID); err != nil {
				return err
			}
			d.logger.warn("runtime.reconcile_duplicate_main", transientRuntimeLogData(record))
			continue
		}
		runtimeCtx, cancel := context.WithCancel(context.Background())
		runtime.cancel = cancel
		runtime.done = make(chan struct{})
		d.activateTransientRuntime(record.Profile, record.Role, record.RunID, runtime)
		d.launchTransientRuntime(runtimeCtx, runtime)
		d.logger.info("runtime.reconciled", transientRuntimeLogData(record))
	}
	return nil
}

func (d *Daemon) removeTransientRuntimeMainReservationsLocked(runID string) {
	for profile, current := range d.transientRuntimeMainProfiles {
		if current == runID {
			delete(d.transientRuntimeMainProfiles, profile)
		}
	}
	for stableKey, current := range d.transientRuntimeMainIDs {
		if current == runID {
			delete(d.transientRuntimeMainIDs, stableKey)
		}
	}
}

func (d *Daemon) addTransientRuntimeMainReservationLocked(record TransientRuntimeRecord) {
	if d.transientRuntimeMainProfiles == nil {
		d.transientRuntimeMainProfiles = map[string]string{}
	}
	if d.transientRuntimeMainIDs == nil {
		d.transientRuntimeMainIDs = map[string]string{}
	}
	d.transientRuntimeMainProfiles[record.Profile] = record.RunID
	for _, key := range transientRuntimeStableReservationKeys(record.BotID, record.BotAgentID) {
		d.transientRuntimeMainIDs[key] = record.RunID
	}
}

func transientRuntimeStableReservationKeys(botID string, botAgentID string) []string {
	keys := make([]string, 0, 2)
	if botID != "" {
		keys = append(keys, "bot_id:"+botID)
	}
	if botAgentID != "" {
		keys = append(keys, "bot_agent_id:"+botAgentID)
	}
	return keys
}

func (d *Daemon) transientRuntimeBotForProfile(profile string) BotRegistryEntry {
	profileConfig, bot, ok := d.cloudNotificationTarget(profile, "", true)
	if !ok || profileConfig.Handle != profile || bot.Handle != profile {
		return BotRegistryEntry{}
	}
	return bot
}

func transientRuntimeMatchesProfileOrStableIdentity(record TransientRuntimeRecord, profile string, bot BotRegistryEntry) bool {
	if record.Profile == profile {
		return true
	}
	return bot.MatchesStableIdentity(record.BotID, record.BotAgentID)
}

func normalizeTransientRuntimeRole(role string) string {
	if role == "" {
		return RuntimeRoleMain
	}
	return role
}

func (d *Daemon) launchTransientRuntime(ctx context.Context, runtime *transientRuntime) {
	if runtime.record.Role == RuntimeRoleTask {
		go d.runTransientRuntimeWatcher(ctx, runtime)
		return
	}
	go d.runTransientRuntimePoller(ctx, runtime)
}

func (d *Daemon) runTransientRuntimeWatcher(ctx context.Context, runtime *transientRuntime) {
	ctx, cancel := context.WithCancel(ctx)
	defer close(runtime.done)
	defer d.removeTransientRuntime(runtime)
	defer cancel()
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		d.watchTransientRuntime(ctx, runtime, cancel)
	}()
	<-ctx.Done()
	<-watcherDone
}

func (d *Daemon) runTransientRuntimePoller(ctx context.Context, runtime *transientRuntime) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		d.watchTransientRuntime(ctx, runtime, cancel)
	}()
	defer close(runtime.done)
	defer d.removeTransientRuntime(runtime)
	defer func() {
		cancel()
		<-watcherDone
	}()
	lastLoggedNudgeErrCode := ""
	for {
		if ctx.Err() != nil {
			return
		}
		ready, nudgeErr := d.nudgeTransientReadyQueueHead(ctx, runtime)
		if nudgeErr != nil {
			if nudgeErr.Code == "CONFLICT" || !nudgeErr.Retryable {
				d.logger.warn("runtime.poller_stopped", map[string]any{
					"profile":    runtime.record.Profile,
					"bot_name":   runtime.record.BotName,
					"run_id":     runtime.record.RunID,
					"error_code": nudgeErr.Code,
				})
				return
			}
			if nudgeErr.Code != lastLoggedNudgeErrCode {
				event := "runtime.poller_error"
				if nudgeErr.Code == "PANE_BUSY" {
					event = "runtime.poller_paused"
				}
				d.logger.warn(event, map[string]any{
					"profile":    runtime.record.Profile,
					"bot_name":   runtime.record.BotName,
					"run_id":     runtime.record.RunID,
					"error_code": nudgeErr.Code,
				})
				lastLoggedNudgeErrCode = nudgeErr.Code
			}
			if !sleepWithContext(ctx, cloudNotificationPollErrorDelay) {
				return
			}
			continue
		}
		lastLoggedNudgeErrCode = ""
		if ready {
			if !sleepWithContext(ctx, d.tmuxPollInterval) {
				return
			}
			continue
		}
		acquired, ingestErr := d.waitForCloudNotificationWake(ctx, runtime.record.Profile, runtime.record.BotName, true, 0, nil)
		if ingestErr != nil {
			if ctx.Err() != nil {
				return
			}
			d.logger.warn("runtime.notification_poll_failed", map[string]any{
				"profile":    runtime.record.Profile,
				"bot_name":   runtime.record.BotName,
				"run_id":     runtime.record.RunID,
				"error_code": ingestErr.Code,
			})
			if !sleepWithContext(ctx, cloudNotificationPollErrorDelay) {
				return
			}
			continue
		}
		if acquired {
			continue
		}
		if !sleepWithContext(ctx, cloudNotificationPollIdleDelay) {
			return
		}
	}
}

func (d *Daemon) watchTransientRuntime(ctx context.Context, runtime *transientRuntime, cancel context.CancelFunc) {
	const (
		stateHealthy = "healthy"
		statePaused  = "paused"
		stateErrored = "errored"
	)
	state := stateHealthy
	for {
		if ctx.Err() != nil {
			return
		}
		verifyErr := d.verifyTransientRuntime(ctx, runtime)
		switch {
		case verifyErr == nil:
			if state != stateHealthy {
				d.logger.info("runtime.poller_resumed", map[string]any{
					"profile":  runtime.record.Profile,
					"bot_name": runtime.record.BotName,
					"run_id":   runtime.record.RunID,
				})
				state = stateHealthy
			}
		case verifyErr.Code == "CONFLICT" || !verifyErr.Retryable:
			d.logger.warn("runtime.poller_stopped", map[string]any{
				"profile":    runtime.record.Profile,
				"bot_name":   runtime.record.BotName,
				"run_id":     runtime.record.RunID,
				"error_code": verifyErr.Code,
			})
			cancel()
			return
		default:
			next := stateErrored
			event := "runtime.poller_error"
			if verifyErr.Code == "PANE_BUSY" {
				next = statePaused
				event = "runtime.poller_paused"
			}
			if state != next {
				d.logger.warn(event, map[string]any{
					"profile":    runtime.record.Profile,
					"bot_name":   runtime.record.BotName,
					"run_id":     runtime.record.RunID,
					"error_code": verifyErr.Code,
				})
				state = next
			}
		}
		delay := d.tmuxPollInterval
		if state != stateHealthy {
			delay = d.tmuxPollSlowInterval
		}
		if !sleepWithContext(ctx, delay) {
			return
		}
	}
}

func (d *Daemon) nudgeTransientReadyQueueHead(ctx context.Context, runtime *transientRuntime) (bool, *SocketError) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return false, nil
	}
	ctx = contextWithDiagnosticSocketRequest(ctx, internalCloudReleaseSocketRequest("runtime.transient_nudge", runtime.record.Profile, ""))
	d.lockBusForContext(ctx)
	var summary *MessageWaitSummary
	storeErr := d.runSocketStageForContext(ctx, "runtime.transient_ready_summary", func() error {
		var err error
		summary, err = d.store.WaitMessageSummary(context.Background(), MessageListFilter{
			Profile: runtime.record.Profile,
			BotName: runtime.record.BotName,
		})
		return err
	})
	if storeErr != nil {
		d.busMu.Unlock()
		return false, classifyMessageStoreError(storeErr)
	}
	if summary == nil {
		d.busMu.Unlock()
		return false, nil
	}
	if req, ok := socketRequestFromContext(ctx); ok {
		ctx = contextWithSocketRequest(ctx, socketRequestWithMessageID(req, summary.MessageID))
	}
	message, storeErr := d.getInboxMessageForContext(ctx, runtime.record.Profile, summary.MessageID, "runtime.transient_get_message")
	if storeErr != nil {
		d.busMu.Unlock()
		return false, classifyMessageStoreError(storeErr)
	}
	if message.Source == "comment.io" {
		if metadataErr := d.runSocketStageForContext(ctx, "runtime.transient_metadata_read", func() error {
			_, err := ReadPrivateCloudMessageMetadata(d.paths, runtime.record.Profile, message.ID)
			return err
		}); metadataErr != nil {
			if _, err := d.quarantineCloudMessageForMissingMetadata(ctx, runtime.record.Profile, message.ID, time.Now().UTC()); err != nil {
				d.busMu.Unlock()
				return false, classifyMessageStoreError(err)
			}
			d.busMu.Unlock()
			return false, socketError("UPSTREAM_ERROR", "could not read notification metadata", true)
		}
	}
	d.busMu.Unlock()
	if ctx.Err() != nil {
		return false, nil
	}
	runtime.mu.Lock()
	alreadyNudged := runtime.lastNudgedMessageID == summary.MessageID
	runtime.mu.Unlock()
	if alreadyNudged {
		return true, nil
	}
	// asyncRewake: a `comment run --runtime claude` session arms the rewake Stop
	// hook (the launch injects COMMENT_IO_LISTEN), which holds a profile-scoped
	// pull-waiter while idle and pulls its own messages. When that waiter is live,
	// skip the tmux keystroke and leave the ready message unclaimed for the waiter
	// to claim — the transient mirror of the managed-session skip. We do NOT record
	// lastNudgedMessageID (so a waiter that dies before claiming falls back to the
	// keystroke on the next poll); the skip is re-evaluated every poll and only the
	// log is deduped. Non-claude runtimes and claude with no live waiter fall
	// through to the keystroke below.
	if transientRuntimeArmsRewake(runtime.record) && d.listeners.hasPullWaiter(runtime.record.Profile, "", "") {
		runtime.mu.Lock()
		firstSkip := runtime.lastRewakeSkipMessageID != summary.MessageID
		runtime.lastRewakeSkipMessageID = summary.MessageID
		runtime.mu.Unlock()
		if firstSkip {
			d.logger.info("runtime.nudge_skipped_async_rewake", map[string]any{
				"profile":    runtime.record.Profile,
				"bot_name":   runtime.record.BotName,
				"run_id":     runtime.record.RunID,
				"message_id": summary.MessageID,
			})
		}
		return true, nil
	}
	if runtimeErr := d.verifyTransientRuntime(ctx, runtime); runtimeErr != nil {
		return false, runtimeErr
	}
	nudgeText, err := formatProfileTmuxNudge(runtime.record.Profile, summary.MessageID, runtime.record.CommentCommandPath)
	if err != nil {
		return false, socketError("VALIDATION_ERROR", "invalid nudge text", false)
	}
	unlock := d.tmuxNudgeLocks.lock(runtime.record.SessionName)
	defer unlock()
	controller := d.controllerForHost(runtime.record.Host)
	_, sendErr := d.sendPrompt(ctx, controller, runtime.record.PaneTarget, nudgeText)
	if sendErr != nil {
		if errors.Is(sendErr, ErrTmuxSessionMissing) {
			return false, socketError("CONFLICT", "runtime session is not running", false)
		}
		return false, socketError("UPSTREAM_ERROR", "could not nudge runtime", true)
	}
	if ctx.Err() != nil {
		return false, nil
	}
	if err := d.waitForTmuxSubmitSettle(ctx); err != nil {
		return false, nil
	}
	if err := controller.SendEnter(ctx, runtime.record.PaneTarget); err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			return false, socketError("CONFLICT", "runtime session is not running", false)
		}
		return false, socketError("UPSTREAM_ERROR", "could not submit runtime nudge", true)
	}
	runtime.mu.Lock()
	runtime.lastNudgedMessageID = summary.MessageID
	runtime.mu.Unlock()
	d.logger.info("runtime.nudge_succeeded", map[string]any{
		"profile":    runtime.record.Profile,
		"bot_name":   runtime.record.BotName,
		"run_id":     runtime.record.RunID,
		"message_id": summary.MessageID,
	})
	if message.Source == "comment.io" {
		d.publishCloudHandlingStartBestEffortAsync(runtime.record.Profile, summary.MessageID)
	}
	return true, nil
}

func (d *Daemon) verifyTransientRuntime(ctx context.Context, runtime *transientRuntime) *SocketError {
	if ctx == nil {
		ctx = context.Background()
	}
	if runtime.record.PaneTarget == "" {
		return socketError("CONFLICT", "runtime pane is not available", false)
	}
	controller := d.controllerForHost(runtime.record.Host)
	switch normalizeSessionHost(runtime.record.Host) {
	case SessionHostBmux:
		if err := validateBmuxSessionPaneTarget(d.paths, runtime.record.SessionName, runtime.record.PaneTarget); err != nil {
			return socketError("CONFLICT", "runtime pane is not in runtime session", false)
		}
	default:
		if targetSession, ok := TmuxPaneTargetSession(runtime.record.PaneTarget); ok {
			if targetSession != runtime.record.SessionName {
				return socketError("CONFLICT", "runtime pane is not in runtime session", false)
			}
		} else {
			live, err := controller.HasSession(ctx, runtime.record.SessionName)
			if err != nil {
				return socketError("UPSTREAM_ERROR", "could not inspect runtime", true)
			}
			if !live {
				return socketError("CONFLICT", "runtime session is not running", false)
			}
			belongs, err := controller.PaneBelongsToSession(ctx, runtime.record.SessionName, runtime.record.PaneTarget)
			if err != nil {
				if errors.Is(err, ErrTmuxSessionMissing) {
					return socketError("CONFLICT", "runtime pane is not running", false)
				}
				return socketError("UPSTREAM_ERROR", "could not inspect runtime pane session", true)
			}
			if !belongs {
				return socketError("CONFLICT", "runtime pane is not in runtime session", false)
			}
		}
	}
	// Path mode re-validates the pinned trusted binary; shell mode has no path to
	// re-check (the runtime is resolved through the login shell at exec time), so
	// skip the trust walk to avoid loop-killing a shell-mode record. Transient
	// runtimes are path mode today, but the guard keeps this consistent with the
	// other consumers should that change.
	if normalizeRuntimeLaunchMode(runtime.record.RuntimeLaunchMode) == RuntimeLaunchModePath {
		runtimePath, err := resolveTrustedExecutable(runtime.record.RuntimePath, "runtime binary")
		if err != nil || runtimePath != runtime.record.RuntimePath {
			return socketError("UPSTREAM_ERROR", "could not verify runtime binary", true)
		}
		commandRuntimePath, err := resolveTrustedExecutable(runtime.record.RuntimeCommandPath, "runtime command")
		if err != nil || commandRuntimePath != runtime.record.RuntimePath {
			return socketError("UPSTREAM_ERROR", "could not verify runtime command", true)
		}
	}
	currentCommand, err := controller.PaneCurrentCommand(ctx, runtime.record.PaneTarget)
	if err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			return socketError("CONFLICT", "runtime session is not running", false)
		}
		return socketError("UPSTREAM_ERROR", "could not inspect runtime command", true)
	}
	if _, ok := runtime.expectedNames[currentCommand]; ok {
		return nil
	}
	return socketError("PANE_BUSY", "runtime pane is busy", true)
}

func (d *Daemon) waitForTransientRuntime(record TransientRuntimeRecord, expected map[string]struct{}, timeout time.Duration) *SocketError {
	deadline := time.Now().Add(timeout)
	controller := d.controllerForHost(record.Host)
	for {
		currentCommand, err := controller.PaneCurrentCommand(context.Background(), record.PaneTarget)
		if err != nil {
			if errors.Is(err, ErrTmuxSessionMissing) {
				return socketError("CONFLICT", "runtime session is not running", false)
			}
			return socketError("UPSTREAM_ERROR", "could not inspect runtime command", true)
		}
		if _, ok := expected[currentCommand]; ok {
			return nil
		}
		if !time.Now().Before(deadline) {
			return socketError("UPSTREAM_ERROR", "runtime did not start", true)
		}
		time.Sleep(sessionRuntimeStartupPoll)
	}
}

func (d *Daemon) waitForTransientRuntimeStartupInput(ctx context.Context, controller TmuxController, record TransientRuntimeRecord) {
	if ctx == nil {
		ctx = context.Background()
	}
	if record.PaneTarget == "" {
		return
	}
	wait := transientRuntimeStartupInputReadyWait
	if wait <= 0 {
		return
	}
	poll := transientRuntimeStartupInputReadyPoll
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	quietDelay := transientRuntimeStartupInputQuietDelay
	if quietDelay <= 0 {
		quietDelay = wait
	}
	deadline := time.Now().Add(wait)
	firstSnapshot := ""
	lastSnapshot := ""
	stableSince := time.Time{}
	sawInitialSnapshot := false
	sawChangeAfterInitial := false
	for {
		if ctx.Err() != nil {
			return
		}
		text, err := controller.CapturePane(ctx, record.PaneTarget, 80)
		if errors.Is(err, ErrTmuxSessionMissing) {
			return
		}
		if err == nil {
			now := time.Now()
			snapshot := normalizeStartupPaneSnapshot(text)
			if snapshot != "" {
				if !sawInitialSnapshot {
					firstSnapshot = snapshot
					lastSnapshot = snapshot
					stableSince = now
					sawInitialSnapshot = true
				} else {
					if snapshot != lastSnapshot {
						if snapshot != firstSnapshot {
							sawChangeAfterInitial = true
						}
						lastSnapshot = snapshot
						stableSince = now
					} else if sawChangeAfterInitial && now.Sub(stableSince) >= quietDelay {
						return
					}
				}
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		delay := poll
		if remaining < delay {
			delay = remaining
		}
		if !sleepWithContext(ctx, delay) {
			return
		}
	}
}

func normalizeStartupPaneSnapshot(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return strings.TrimSpace(text)
}

// resolveTransientRuntimeCommandWithPath validates and resolves a runtime
// command. When clientCommandPath is non-empty, the client (CLI) has
// already located the binary using its real PATH (which includes user-local
// install dirs the daemon cannot see), and we just validate that absolute
// path. When empty, we fall back to the legacy daemon-side search.
func resolveTransientRuntimeCommandWithPath(command []string, clientCommandPath string) (runtimeCommandResolution, error) {
	if clientCommandPath == "" {
		return resolveTransientRuntimeCommand(command)
	}
	if !filepath.IsAbs(clientCommandPath) {
		return runtimeCommandResolution{}, errors.New("runtime command path must be absolute")
	}
	commandPath := filepath.Clean(clientCommandPath)
	runtimePath, err := resolveTrustedExecutable(commandPath, "runtime binary")
	if err != nil {
		return runtimeCommandResolution{}, err
	}
	return runtimeCommandResolution{CommandPath: commandPath, RuntimePath: runtimePath}, nil
}

func resolveTransientRuntimeCommand(command []string) (runtimeCommandResolution, error) {
	if len(command) == 0 || command[0] == "" {
		return runtimeCommandResolution{}, errors.New("invalid runtime command")
	}
	commandName, err := expandHome(command[0])
	if err != nil {
		return runtimeCommandResolution{}, err
	}
	if strings.ContainsRune(commandName, filepath.Separator) {
		commandPath := filepath.Clean(commandName)
		runtimePath, err := resolveTrustedExecutable(commandPath, "runtime binary")
		if err != nil {
			return runtimeCommandResolution{}, err
		}
		return runtimeCommandResolution{CommandPath: commandPath, RuntimePath: runtimePath}, nil
	}
	for _, dir := range trustedExecutableSearchDirs("") {
		candidate := filepath.Join(dir, commandName)
		if _, err := os.Lstat(candidate); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return runtimeCommandResolution{}, err
		}
		runtimePath, err := resolveTrustedExecutable(candidate, "runtime binary")
		if err != nil {
			return runtimeCommandResolution{}, err
		}
		return runtimeCommandResolution{CommandPath: filepath.Clean(candidate), RuntimePath: runtimePath}, nil
	}
	runtimePath, err := resolveTrustedExecutable(commandName, "runtime binary")
	if err != nil {
		return runtimeCommandResolution{}, err
	}
	return runtimeCommandResolution{CommandPath: runtimePath, RuntimePath: runtimePath}, nil
}

func (d *Daemon) prepareTransientRuntimeCommentCommand() (string, error) {
	executablePath := d.commentExecutablePath
	if executablePath == nil {
		executablePath = commentExecutablePath
	}
	commentBinary, err := executablePath()
	if err != nil {
		return "", err
	}
	return ensureCommentCommandShim(d.paths, commentBinary)
}

func ensureCommentCommandShim(paths Paths, commentBinary string) (string, error) {
	commentBinary, err := resolveTrustedExecutable(commentBinary, "comment binary")
	if err != nil {
		return "", err
	}
	if paths.Bus == "" {
		return "", errors.New("comment bus path is not configured")
	}
	binDir := filepath.Join(paths.Bus, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(binDir, 0o700); err != nil {
		return "", err
	}
	if err := validateTrustedSearchPathDir(binDir, "comment command directory"); err != nil {
		return "", err
	}
	shimPath := filepath.Join(binDir, "comment")
	tmpPath := filepath.Join(binDir, ".comment-"+strconv.Itoa(os.Getpid())+"-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".tmp")
	if err := os.Symlink(commentBinary, tmpPath); err != nil {
		return "", err
	}
	if err := os.Rename(tmpPath, shimPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if _, err := resolveTrustedExecutable(shimPath, "comment command shim"); err != nil {
		return "", err
	}
	return shimPath, nil
}

// transientRuntimeIsClaude reports whether a transient runtime is the Claude CLI,
// matched by command basename so an absolute path (`comment run --runtime
// /opt/homebrew/bin/claude`, or a legacy absolute runtime_command[0]) is still
// recognized for asyncRewake arming and the nudge-skip — not just the bare
// "claude" string. Mirrors expectedTransientRuntimeCommandNames' basename logic.
func transientRuntimeIsClaude(record TransientRuntimeRecord) bool {
	candidates := []string{record.Runtime}
	if len(record.RuntimeCommand) > 0 {
		candidates = append(candidates, record.RuntimeCommand[0])
	}
	candidates = append(candidates, record.RuntimeCommandPath, record.RuntimePath)
	for _, c := range candidates {
		if c != "" && filepath.Base(c) == "claude" {
			return true
		}
	}
	return false
}

// transientRuntimeArmsRewake reports whether a transient runtime should arm the
// asyncRewake listener (and be eligible for the keystroke-skip). Only a MAIN-role
// Claude runtime qualifies: a task-role runtime does not reserve the profile via
// reserveMainEstablish / transientRuntimeMainProfiles, so arming it as a
// profile-scoped listener would let an idle task helper pull/wake for the handle
// and race the main or impromptu listener (no single-listener coverage).
func transientRuntimeArmsRewake(record TransientRuntimeRecord) bool {
	return record.Role == RuntimeRoleMain && transientRuntimeIsClaude(record)
}

func transientRuntimeLaunchCommand(record TransientRuntimeRecord) string {
	commands := []string{}
	if record.OutputLogPath != "" {
		commands = append(commands, "sleep 0.1")
	}
	commands = append(commands, "export COMMENT_IO_PROFILE="+shellQuote(record.Profile), "COMMENT_IO_RUNTIME_RUN=1")
	// A claude transient runtime arms the rewake Stop hook so an idle session is
	// woken natively (asyncRewake) instead of only via a tmux keystroke. The hook
	// keys off COMMENT_IO_LISTEN; no session triple/listen_session is injected, so
	// the waiter is profile-scoped (single-listener per handle is enforced by the
	// establish lock) and not claim-scoped. nudgeTransientReadyQueueHead skips the
	// keystroke while that waiter is live, so this does not double-deliver.
	if transientRuntimeArmsRewake(record) {
		commands = append(commands, "COMMENT_IO_LISTEN=1", "export COMMENT_IO_LISTEN")
	}
	for _, entry := range record.Env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !isRuntimeEnvKey(key) || strings.ContainsAny(value, "\r\n\x00") || containsSecretValue(value) {
			continue
		}
		commands = append(commands, "export "+key+"="+shellQuote(value))
	}
	commands = append(commands, "unset NO_COLOR")
	commands = append(commands, transientRuntimeTerminalColorCommands(record.Env)...)
	extraSearchDirs := []string{}
	if record.CommentCommandPath != "" {
		extraSearchDirs = append(extraSearchDirs, filepath.Dir(record.CommentCommandPath))
	}
	if record.RuntimeCommandPath != "" {
		extraSearchDirs = append(extraSearchDirs, filepath.Dir(record.RuntimeCommandPath))
	}
	if path := trustedRuntimeSearchPath(record.RuntimePath, extraSearchDirs...); path != "" {
		commands = append(commands, "PATH="+shellQuote(path))
	}
	commands = append(commands, "export COMMENT_IO_RUNTIME_RUN PATH")
	// Transient runtimes are always "path" mode: created via
	// resolveTransientRuntimeCommandWithPath with a resolved RuntimePath, and
	// validateTransientRuntimeRecord forbids empty paths outside shell mode. A
	// shell-mode transient record therefore cannot be stored and cannot reach
	// here; if transient launches ever adopt shell mode this must build the
	// shell-native form (see runShellSessionExec) instead of exec'ing a path.
	execParts := []string{"exec", shellQuote(record.RuntimePath)}
	for _, arg := range record.RuntimeCommand[1:] {
		execParts = append(execParts, shellQuote(arg))
	}
	commands = append(commands, strings.Join(execParts, " "))
	return strings.Join(commands, "; ")
}

func transientRuntimeTerminalColorCommands(env []string) []string {
	commands := []string{}
	if term, ok := envEntryValue(env, "TERM"); ok {
		if !isUsableRuntimeTerm(term) {
			term = runtimeTermDefault
		}
		commands = append(commands, "export TERM="+shellQuote(term))
	} else {
		commands = append(commands, "case \"${TERM:-}\" in ''|dumb) export TERM="+shellQuote(runtimeTermDefault)+";; esac")
	}
	for _, entry := range runtimeTerminalColorEnv(env) {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "TERM" || !isRuntimeTerminalColorKey(key) || !isSafeRuntimeColorValue(value) {
			continue
		}
		commands = append(commands, "export "+key+"="+shellQuote(value))
	}
	return commands
}

func isRuntimeEnvKey(key string) bool {
	switch key {
	case "COMMENT_IO_LOCAL_SYNC",
		"COMMENT_IO_LOCAL_SYNC_ROOT",
		"COMMENT_IO_LOCAL_DOCS_ROOT",
		"COMMENT_IO_LOCAL_SYNC_MODE",
		"COMMENT_IO_LOCAL_SYNC_FRESHNESS":
		return true
	default:
		return false
	}
}

func isRuntimeTerminalColorKey(key string) bool {
	switch key {
	case "TERM", "COLORTERM", "FORCE_COLOR", "CLICOLOR_FORCE":
		return true
	default:
		return false
	}
}

func transientRuntimeOutputPipeCommand(outputLogPath string, tailerPath string) string {
	if outputLogPath == "" {
		return ""
	}
	quoted := shellQuote(outputLogPath)
	if tailerPath == "" {
		tailerPath = "comment"
	}
	tailer := shellQuote(tailerPath)
	return tailer + " __runtime-tail --log " + quoted + " --bytes 65536"
}

func transientRuntimeOutputLogPath(paths Paths, runID string) (string, error) {
	if paths.Bus == "" || !LocalSessionIDRE.MatchString(runID) {
		return "", errors.New("invalid runtime output log path")
	}
	dir := filepath.Join(paths.Bus, "runtime-logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, runID+".log"), nil
}

func managedSessionOutputLogPath(paths Paths, sessionID string) (string, error) {
	return transientRuntimeOutputLogPath(paths, sessionID)
}

func expectedTransientRuntimeCommandNames(record TransientRuntimeRecord) map[string]struct{} {
	expected := map[string]struct{}{}
	add := func(name string) {
		if isSafePaneCommandName(name) {
			expected[name] = struct{}{}
		}
	}
	if len(record.RuntimeCommand) > 0 && record.RuntimeCommand[0] != "" {
		add(filepath.Base(record.RuntimeCommand[0]))
	}
	if record.RuntimeCommandPath != "" {
		add(filepath.Base(record.RuntimeCommandPath))
	}
	if record.RuntimePath != "" {
		add(filepath.Base(record.RuntimePath))
		for _, name := range runtimeScriptCommandNames(record.RuntimePath) {
			add(name)
		}
	}
	return expected
}

func transientRuntimeSessionName(profile string, runID string) (string, error) {
	if !ProfileRE.MatchString(profile) || !LocalSessionIDRE.MatchString(runID) {
		return "", errors.New("invalid runtime session selector")
	}
	botName := profileOnlyNotificationBot(profile).Name
	sum := sha256.Sum256([]byte(profile + ":" + runID))
	suffix := hex.EncodeToString(sum[:])[:12]
	name := "comment-run-" + botName + "-" + suffix
	if err := validateTmuxSessionName(name); err != nil {
		return "", err
	}
	return name, nil
}

func formatProfileTmuxNudge(profile string, messageID string, commentCommandPath string) (string, error) {
	if !ProfileRE.MatchString(profile) || !LocalMessageIDRE.MatchString(messageID) {
		return "", errors.New("invalid nudge input")
	}
	command := "comment"
	if commentCommandPath != "" {
		commandPath := filepath.Clean(commentCommandPath)
		if !isSafeAbsoluteLocalPath(commandPath) {
			return "", errors.New("invalid comment command path")
		}
		command = shellQuote(commandPath)
	}
	text := "# comment.io message for " + profile + ": run " + command + " messages receive --profile " + profile + " " + messageID + " then ack or release. If no visible reply is needed run " + command + " activity complete " + messageID
	if strings.ContainsAny(text, "`\"$;&|<>\r\n\x00") {
		return "", errors.New("unsafe nudge text")
	}
	return text, nil
}

func transientRuntimeLogData(record TransientRuntimeRecord) map[string]any {
	data := map[string]any{
		"profile":      record.Profile,
		"bot_name":     record.BotName,
		"run_id":       record.RunID,
		"host":         normalizeSessionHost(record.Host),
		"session_name": record.SessionName,
		"pane_target":  record.PaneTarget,
		"runtime":      record.Runtime,
		"state":        record.State,
	}
	if record.BotID != "" {
		data["bot_id"] = record.BotID
	}
	if record.BotAgentID != "" {
		data["bot_agent_id"] = record.BotAgentID
	}
	return data
}
