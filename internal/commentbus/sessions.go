package commentbus

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	ErrInvalidSession       = errors.New("invalid session")
	ErrSessionAlreadyExists = errors.New("session already exists")
)

type SessionRecord struct {
	SessionID          string   `json:"session_id"`
	Host               string   `json:"host,omitempty"`
	Profile            string   `json:"profile"`
	BotName            string   `json:"bot_name"`
	BotID              string   `json:"bot_id,omitempty"`
	BotAgentID         string   `json:"bot_agent_id,omitempty"`
	ScopeType          string   `json:"scope_type"`
	ScopeID            string   `json:"scope_id"`
	BotletsHome        string   `json:"botlets_home"`
	SessionName        string   `json:"session_name"`
	PaneTarget         string   `json:"pane_target"`
	Generation         string   `json:"generation"`
	CapabilityFile     string   `json:"capability_file"`
	Runtime            string   `json:"runtime"`
	RuntimePath        string   `json:"runtime_path,omitempty"`
	RuntimeSessionRef  string   `json:"runtime_session_ref,omitempty"`
	RuntimeCommandPath string   `json:"runtime_command_path,omitempty"`
	RuntimeCommand     []string `json:"runtime_command"`
	// RuntimeLaunchMode: "path" (legacy trusted-binary exec) or "shell"
	// (resolve the runtime name through the user's login shell). Empty
	// normalizes to "path". See normalizeRuntimeLaunchMode.
	RuntimeLaunchMode string                     `json:"runtime_launch_mode,omitempty"`
	WorkingDir        string                     `json:"working_dir,omitempty"`
	OutputLogPath     string                     `json:"output_log_path,omitempty"`
	CreatedAt         string                     `json:"created_at"`
	LastNudge         LastNudgeRecord            `json:"last_nudge"`
	AutomaticNudges   map[string]LastNudgeRecord `json:"automatic_nudges,omitempty"`
	DailyReset        *DailyResetRecord          `json:"daily_reset,omitempty"`
	State             string                     `json:"state"`
}

type DailyResetRecord struct {
	Date                 string `json:"date"`
	State                string `json:"state"`
	Reason               string `json:"reason"`
	RequestedAt          string `json:"requested_at"`
	PromptedAt           string `json:"prompted_at,omitempty"`
	DeadlineAt           string `json:"deadline_at"`
	LogPath              string `json:"log_path"`
	CompletedAt          string `json:"completed_at,omitempty"`
	ReplacementSessionID string `json:"replacement_session_id,omitempty"`
}

type LastNudgeRecord struct {
	MessageID       *string `json:"message_id"`
	PaneTarget      *string `json:"pane_target"`
	AttemptedAt     *string `json:"attempted_at"`
	SucceededAt     *string `json:"succeeded_at"`
	ClaimGeneration *string `json:"claim_generation,omitempty"`
	AttemptCount    int     `json:"attempt_count,omitempty"`
	NextEligibleAt  *string `json:"next_eligible_at,omitempty"`
	FailureReason   string  `json:"failure_reason,omitempty"`
	Stuck           bool    `json:"stuck,omitempty"`
}

type RegisterSessionOptions struct {
	Paths             Paths
	Host              string
	Profile           string
	BotName           string
	BotID             string
	BotAgentID        string
	ScopeType         string
	ScopeID           string
	SessionID         string
	Generation        string
	BotletsHome       string
	SessionName       string
	PaneTarget        string
	Runtime           string
	RuntimeSessionRef string
	LaunchMode        string
	State             string
	Now               time.Time
}

func RegisterSession(options RegisterSessionOptions) (SessionRecord, error) {
	sessionID := options.SessionID
	if sessionID == "" {
		var err error
		sessionID, err = GenerateLocalID("sess", 0)
		if err != nil {
			return SessionRecord{}, fmt.Errorf("generate session id: %w", err)
		}
	}
	generation := options.Generation
	if generation == "" {
		var err error
		generation, err = GenerateLocalID("gen", 0)
		if err != nil {
			return SessionRecord{}, fmt.Errorf("generate session generation: %w", err)
		}
	}
	if err := ValidateLocalID("sess", sessionID); err != nil {
		return SessionRecord{}, fmt.Errorf("%w: %v", ErrInvalidSession, err)
	}
	if err := ValidateLocalID("gen", generation); err != nil {
		return SessionRecord{}, fmt.Errorf("%w: %v", ErrInvalidSession, err)
	}
	if !ProfileRE.MatchString(options.Profile) || !isBotName(options.BotName) || !isSessionScopeType(options.ScopeType) || !isSafeScopeID(options.ScopeType, options.ScopeID) {
		return SessionRecord{}, ErrInvalidSession
	}
	if !safeOptionalRegistryIdentity(options.BotID) || !safeOptionalRegistryIdentity(options.BotAgentID) {
		return SessionRecord{}, ErrInvalidSession
	}
	if !isV1RegisterSessionScope(options.Profile, options.ScopeType, options.ScopeID) {
		return SessionRecord{}, ErrInvalidSession
	}
	if options.Runtime == "" {
		options.Runtime = "claude"
	}
	if !isManagedSessionRuntime(options.Runtime) {
		return SessionRecord{}, ErrInvalidSession
	}
	host := normalizeSessionHost(options.Host)
	if !isSessionHost(host) {
		return SessionRecord{}, ErrInvalidSession
	}
	if options.SessionName != "" {
		if err := validateSessionNameForHost(host, options.SessionName); err != nil {
			return SessionRecord{}, ErrInvalidSession
		}
	}
	if options.PaneTarget != "" {
		if err := validatePaneTargetForHost(host, options.PaneTarget); err != nil {
			return SessionRecord{}, ErrInvalidSession
		}
		if host == SessionHostBmux {
			if err := validateBmuxSessionPaneTarget(options.Paths, options.SessionName, options.PaneTarget); err != nil {
				return SessionRecord{}, ErrInvalidSession
			}
		}
	}
	if options.State == "" {
		options.State = "alive"
	}
	if !isSessionState(options.State) {
		return SessionRecord{}, ErrInvalidSession
	}
	if options.Now.IsZero() {
		options.Now = time.Now().UTC()
	}
	launchMode := normalizeManagedSessionLaunchMode(options.LaunchMode)
	runtimeSessionRef := options.RuntimeSessionRef
	if launchMode == managedSessionLaunchResume && runtimeSessionRef == "" {
		launchMode = managedSessionLaunchFresh
	}
	if host == SessionHostBmux && options.Runtime == "claude" && runtimeSessionRef == "" {
		var err error
		runtimeSessionRef, err = GenerateUUIDv4()
		if err != nil {
			return SessionRecord{}, fmt.Errorf("generate runtime session ref: %w", err)
		}
	}
	if runtimeSessionRef != "" && !isRuntimeSessionRef(options.Runtime, runtimeSessionRef) {
		return SessionRecord{}, ErrInvalidSession
	}
	outputLogPath := ""
	if host == SessionHostBmux {
		var err error
		outputLogPath, err = managedSessionOutputLogPath(options.Paths, sessionID)
		if err != nil {
			return SessionRecord{}, err
		}
	}

	capabilityFile := sessionCapabilityPath(options.Paths, options.Profile, sessionID, generation)
	capability, err := generateCapabilityToken()
	if err != nil {
		return SessionRecord{}, err
	}
	if err := WritePrivateFileAtomicNoReplace(capabilityFile, []byte(capability+"\n"), 0o600); err != nil {
		if errors.Is(err, os.ErrExist) {
			return SessionRecord{}, ErrSessionAlreadyExists
		}
		return SessionRecord{}, err
	}

	record := SessionRecord{
		SessionID:         sessionID,
		Host:              host,
		Profile:           options.Profile,
		BotName:           options.BotName,
		BotID:             options.BotID,
		BotAgentID:        options.BotAgentID,
		ScopeType:         options.ScopeType,
		ScopeID:           options.ScopeID,
		BotletsHome:       options.BotletsHome,
		SessionName:       options.SessionName,
		PaneTarget:        options.PaneTarget,
		Generation:        generation,
		CapabilityFile:    capabilityFile,
		Runtime:           options.Runtime,
		RuntimeSessionRef: runtimeSessionRef,
		RuntimeCommand:    managedSessionRuntimeCommandForLaunch(options.Runtime, options.BotName, runtimeSessionRef, launchMode),
		// New managed sessions resolve the runtime through the user's login
		// shell (supports PATH binaries, aliases, functions); no trusted-binary
		// path is pinned. Legacy records without this field coerce to "path".
		RuntimeLaunchMode: RuntimeLaunchModeShell,
		OutputLogPath:     outputLogPath,
		CreatedAt:         options.Now.UTC().Format(time.RFC3339),
		LastNudge:         LastNudgeRecord{},
		State:             options.State,
	}
	if err := WriteNewSessionRecord(options.Paths, record); err != nil {
		_ = os.Remove(capabilityFile)
		if errors.Is(err, os.ErrExist) {
			return SessionRecord{}, ErrSessionAlreadyExists
		}
		return SessionRecord{}, err
	}
	return record, nil
}

func isManagedSessionRuntime(runtime string) bool {
	return runtime == "claude" || runtime == "codex"
}

// sessionRuntimeResolvable is the health-check trust gate. In "path" mode it
// re-validates the pinned trusted binary (the legacy contract). In "shell" mode
// there is no path to re-validate — the runtime name is resolved through the
// user's login shell at exec time — so it only sanity-checks the command name;
// liveness comes from the pane checks.
func sessionRuntimeResolvable(record SessionRecord) error {
	if normalizeRuntimeLaunchMode(record.RuntimeLaunchMode) == RuntimeLaunchModeShell {
		if len(record.RuntimeCommand) == 0 || !isManagedSessionRuntime(record.RuntimeCommand[0]) {
			return errors.New("invalid runtime command")
		}
		return nil
	}
	_, err := resolveRuntimeCommandExecutable(record, nil)
	return err
}

// isLoginShellCommandName reports whether a tmux pane_current_command is a bare
// login shell. In shell mode the launch shell is the pane's foreground process
// only briefly while rc loads (the `-ilc` shell exits when the runtime exits),
// so a shell foreground means "still starting / not running the runtime yet".
func isLoginShellCommandName(name string) bool {
	switch strings.TrimPrefix(strings.ToLower(name), "-") {
	case "zsh", "bash", "sh", "dash", "fish", "ksh", "csh", "tcsh":
		return true
	}
	return false
}

func managedSessionRuntimeCommand(runtime string, botName string) []string {
	return managedSessionRuntimeCommandForLaunch(runtime, botName, "", managedSessionLaunchFresh)
}

func managedSessionRuntimeCommandForRef(runtime string, botName string, runtimeSessionRef string) []string {
	return managedSessionRuntimeCommandForLaunch(runtime, botName, runtimeSessionRef, managedSessionLaunchFresh)
}

const (
	managedSessionLaunchFresh  = "fresh"
	managedSessionLaunchResume = "resume"
)

func normalizeManagedSessionLaunchMode(mode string) string {
	if mode == managedSessionLaunchResume {
		return managedSessionLaunchResume
	}
	return managedSessionLaunchFresh
}

func managedSessionRuntimeCommandForLaunch(runtime string, botName string, runtimeSessionRef string, launchMode string) []string {
	launchMode = normalizeManagedSessionLaunchMode(launchMode)
	// Botlets are NOT Claude subagents — their identity comes from the brain
	// working dir + the session-exec env injection, not a `--agent` flag. Older
	// `claude` silently ignored an unknown `--agent <bot>`; Claude Code >= 2.1.x
	// hard-errors (`--agent '<bot>' not found`), killing the runtime ~1s after
	// launch and driving the daemon into an infinite relaunch loop (issue #1420).
	// We therefore never pass `--agent` to `claude`. botName is retained as a
	// parameter for the codex/shape symmetry and existing call sites (it is no
	// longer part of any Claude command).
	if launchMode == managedSessionLaunchResume && runtimeSessionRef != "" && isRuntimeSessionRef(runtime, runtimeSessionRef) {
		if runtime == "codex" {
			return []string{"codex", "resume", runtimeSessionRef, "--yolo"}
		}
		return []string{"claude", "--resume", runtimeSessionRef, "--dangerously-skip-permissions"}
	}
	if runtime == "codex" {
		return []string{"codex", "--yolo"}
	}
	if runtimeSessionRef != "" {
		return []string{"claude", "--session-id", runtimeSessionRef, "--dangerously-skip-permissions"}
	}
	return []string{"claude", "--dangerously-skip-permissions"}
}

func normalizeManagedSessionRuntimeCommand(record SessionRecord) []string {
	if record.Runtime != "claude" || !managedSessionRuntimeCommandMatches(record) {
		return append([]string(nil), record.RuntimeCommand...)
	}
	hasRef := record.RuntimeSessionRef != "" && isRuntimeSessionRef(record.Runtime, record.RuntimeSessionRef)
	if hasRef && len(record.RuntimeCommand) == 6 &&
		record.RuntimeCommand[0] == "claude" &&
		record.RuntimeCommand[1] == "--session-id" &&
		record.RuntimeCommand[2] == record.RuntimeSessionRef &&
		record.RuntimeCommand[3] == "--agent" &&
		record.RuntimeCommand[4] == record.BotName &&
		record.RuntimeCommand[5] == "--dangerously-skip-permissions" {
		return []string{"claude", "--session-id", record.RuntimeSessionRef, "--dangerously-skip-permissions"}
	}
	if hasRef && len(record.RuntimeCommand) == 6 &&
		record.RuntimeCommand[0] == "claude" &&
		record.RuntimeCommand[1] == "--resume" &&
		record.RuntimeCommand[2] == record.RuntimeSessionRef &&
		record.RuntimeCommand[3] == "--agent" &&
		record.RuntimeCommand[4] == record.BotName &&
		record.RuntimeCommand[5] == "--dangerously-skip-permissions" {
		return []string{"claude", "--resume", record.RuntimeSessionRef, "--dangerously-skip-permissions"}
	}
	if len(record.RuntimeCommand) == 4 &&
		record.RuntimeCommand[0] == "claude" &&
		record.RuntimeCommand[1] == "--agent" &&
		record.RuntimeCommand[2] == record.BotName &&
		record.RuntimeCommand[3] == "--dangerously-skip-permissions" {
		return []string{"claude", "--dangerously-skip-permissions"}
	}
	if len(record.RuntimeCommand) == 3 &&
		record.RuntimeCommand[0] == "claude" &&
		record.RuntimeCommand[1] == "--agent" &&
		record.RuntimeCommand[2] == record.BotName {
		return []string{"claude"}
	}
	return append([]string(nil), record.RuntimeCommand...)
}

func managedSessionRuntimeCommandMatches(record SessionRecord) bool {
	switch record.Runtime {
	case "claude":
		hasRef := record.RuntimeSessionRef != "" && isRuntimeSessionRef(record.Runtime, record.RuntimeSessionRef)
		// Current agentless shapes (issue #1420): `claude` no longer takes the
		// unknown `--agent <bot>` flag, so the launch command is one pair shorter.
		pinned := hasRef &&
			len(record.RuntimeCommand) == 4 &&
			record.RuntimeCommand[0] == "claude" &&
			record.RuntimeCommand[1] == "--session-id" &&
			record.RuntimeCommand[2] == record.RuntimeSessionRef &&
			record.RuntimeCommand[3] == "--dangerously-skip-permissions"
		resume := hasRef &&
			len(record.RuntimeCommand) == 4 &&
			record.RuntimeCommand[0] == "claude" &&
			record.RuntimeCommand[1] == "--resume" &&
			record.RuntimeCommand[2] == record.RuntimeSessionRef &&
			record.RuntimeCommand[3] == "--dangerously-skip-permissions"
		fresh := len(record.RuntimeCommand) == 2 &&
			record.RuntimeCommand[0] == "claude" &&
			record.RuntimeCommand[1] == "--dangerously-skip-permissions"
		legacyAgentless := len(record.RuntimeCommand) == 1 &&
			record.RuntimeCommand[0] == "claude"
		// Legacy `--agent <bot>` shapes from records written before the fix. They
		// stay accepted so existing records read cleanly (and don't poison the
		// session read); the daemon regenerates them agentless on relaunch.
		legacyPinned := hasRef &&
			len(record.RuntimeCommand) == 6 &&
			record.RuntimeCommand[0] == "claude" &&
			record.RuntimeCommand[1] == "--session-id" &&
			record.RuntimeCommand[2] == record.RuntimeSessionRef &&
			record.RuntimeCommand[3] == "--agent" &&
			record.RuntimeCommand[4] == record.BotName &&
			record.RuntimeCommand[5] == "--dangerously-skip-permissions"
		legacyResume := hasRef &&
			len(record.RuntimeCommand) == 6 &&
			record.RuntimeCommand[0] == "claude" &&
			record.RuntimeCommand[1] == "--resume" &&
			record.RuntimeCommand[2] == record.RuntimeSessionRef &&
			record.RuntimeCommand[3] == "--agent" &&
			record.RuntimeCommand[4] == record.BotName &&
			record.RuntimeCommand[5] == "--dangerously-skip-permissions"
		legacyModern := len(record.RuntimeCommand) == 4 &&
			record.RuntimeCommand[0] == "claude" &&
			record.RuntimeCommand[1] == "--agent" &&
			record.RuntimeCommand[2] == record.BotName &&
			record.RuntimeCommand[3] == "--dangerously-skip-permissions"
		legacy := len(record.RuntimeCommand) == 3 &&
			record.RuntimeCommand[0] == "claude" &&
			record.RuntimeCommand[1] == "--agent" &&
			record.RuntimeCommand[2] == record.BotName
		if normalizeSessionHost(record.Host) == SessionHostBmux {
			return pinned || resume || legacyPinned || legacyResume
		}
		return pinned || resume || fresh || legacyAgentless || legacyPinned || legacyResume || legacyModern || legacy
	case "codex":
		modern := len(record.RuntimeCommand) == 2 &&
			record.RuntimeCommand[0] == "codex" &&
			record.RuntimeCommand[1] == "--yolo"
		resume := record.RuntimeSessionRef != "" &&
			isRuntimeSessionRef(record.Runtime, record.RuntimeSessionRef) &&
			len(record.RuntimeCommand) == 4 &&
			record.RuntimeCommand[0] == "codex" &&
			record.RuntimeCommand[1] == "resume" &&
			record.RuntimeCommand[2] == record.RuntimeSessionRef &&
			record.RuntimeCommand[3] == "--yolo"
		legacy := len(record.RuntimeCommand) == 1 && record.RuntimeCommand[0] == "codex"
		if normalizeSessionHost(record.Host) == SessionHostBmux {
			return modern || resume
		}
		return modern || resume || legacy
	default:
		return false
	}
}

func isRuntimeSessionRef(runtime string, value string) bool {
	switch runtime {
	case "claude":
		return UUIDRE.MatchString(value)
	case "codex":
		return UUIDLikeRE.MatchString(value)
	default:
		return false
	}
}

func isSafeRuntimeSessionRef(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsAny(value, "/\\\r\n\x00") && !containsSecretValue(value)
}

func WriteSessionRecord(paths Paths, record SessionRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivateFileAtomic(sessionRecordPath(paths, record.SessionID), data, 0o600)
}

func WriteNewSessionRecord(paths Paths, record SessionRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivateFileAtomicNoReplace(sessionRecordPath(paths, record.SessionID), data, 0o600)
}

func ReadSessionRecord(paths Paths, sessionID string) (SessionRecord, error) {
	if !LocalSessionIDRE.MatchString(sessionID) {
		return SessionRecord{}, errors.New("invalid session id")
	}
	path := sessionRecordPath(paths, sessionID)
	if err := validateSessionRecordFileTrust(path); err != nil {
		return SessionRecord{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return SessionRecord{}, errors.New("could not read session")
	}
	var record SessionRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return SessionRecord{}, errors.New("invalid session json")
	}
	record.Host = normalizeSessionHost(record.Host)
	// Legacy session record files predate RuntimeLaunchMode; json.Unmarshal
	// leaves it "" which must coerce to "path" so existing path-mode sessions
	// validate and continue under the old trusted-binary model (mirrors the
	// normalizeSessionHost coercion above).
	record.RuntimeLaunchMode = normalizeRuntimeLaunchMode(record.RuntimeLaunchMode)
	if record.SessionID != sessionID {
		return SessionRecord{}, errors.New("invalid session record")
	}
	if err := validateSessionRecord(paths, record); err != nil {
		return SessionRecord{}, err
	}
	record.RuntimeCommand = normalizeManagedSessionRuntimeCommand(record)
	return record, nil
}

// ListSessionRecords reads all session records strictly: a single malformed or
// invalid record fails the whole read. This fail-closed behavior is relied on by
// safety-sensitive callers like `comment uninstall`, which must NOT proceed to
// delete state when it cannot positively account for every managed session.
// Liveness/status read paths that must tolerate a poisoned record instead use
// ListSessionRecordsLenient (see issue #1420 Bug 2).
func ListSessionRecords(paths Paths) ([]SessionRecord, error) {
	entries, err := os.ReadDir(paths.Sessions)
	if errors.Is(err, os.ErrNotExist) {
		return []SessionRecord{}, nil
	}
	if err != nil {
		return nil, errors.New("could not read sessions")
	}
	var records []SessionRecord
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		sessionID := name[:len(name)-len(".json")]
		record, err := ReadSessionRecord(paths, sessionID)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].SessionID < records[j].SessionID })
	return records, nil
}

// ListSessionRecordsLenient reads every valid session record, skipping (rather
// than failing the whole batch on) any individual record that is malformed or
// fails validation. A single poisoned record must not abort the read: that
// previously surfaced to clients as `UPSTREAM_ERROR: could not read sessions`
// and took down `comment run` / `comment sessions status` whenever the relaunch
// loop leaked a dangling record (issue #1420). The skipped session IDs are
// returned so callers can log/quarantine them. Only a directory-level read
// failure is fatal.
func ListSessionRecordsLenient(paths Paths) ([]SessionRecord, []string, error) {
	entries, err := os.ReadDir(paths.Sessions)
	if errors.Is(err, os.ErrNotExist) {
		return []SessionRecord{}, nil, nil
	}
	if err != nil {
		return nil, nil, errors.New("could not read sessions")
	}
	var records []SessionRecord
	var skipped []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".json" {
			continue
		}
		sessionID := name[:len(name)-len(".json")]
		record, err := ReadSessionRecord(paths, sessionID)
		if err != nil {
			skipped = append(skipped, sessionID)
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].SessionID < records[j].SessionID })
	return records, skipped, nil
}

func VerifySessionCapability(paths Paths, auth SocketAuth) (SessionRecord, error) {
	return verifySessionCapabilityForStates(paths, auth, map[string]struct{}{
		"alive":    {},
		"starting": {},
	})
}

func VerifySessionCapabilityForResetComplete(paths Paths, auth SocketAuth) (SessionRecord, error) {
	return verifySessionCapabilityForStates(paths, auth, map[string]struct{}{
		"alive":    {},
		"starting": {},
		"dead":     {},
	})
}

func verifySessionCapabilityForStates(paths Paths, auth SocketAuth, allowedStates map[string]struct{}) (SessionRecord, error) {
	if auth.Profile == nil || auth.SessionID == nil || auth.SessionGeneration == nil {
		return SessionRecord{}, errors.New("invalid session auth")
	}
	record, err := ReadSessionRecord(paths, *auth.SessionID)
	if err != nil {
		return SessionRecord{}, errors.New("invalid session capability")
	}
	if record.SessionID != *auth.SessionID || record.Profile != *auth.Profile || record.Generation != *auth.SessionGeneration {
		return SessionRecord{}, errors.New("invalid session capability")
	}
	if _, ok := allowedStates[record.State]; !ok {
		return SessionRecord{}, errors.New("invalid session capability")
	}
	expectedPath, err := sessionCapabilityPathForRecord(paths, record)
	if err != nil {
		return SessionRecord{}, errors.New("invalid session capability")
	}
	expected, err := ReadPrivateCapability(paths.Home, expectedPath, "capability file")
	if err != nil {
		return SessionRecord{}, errors.New("invalid session capability")
	}
	if !CapabilityTokenRE.MatchString(expected) {
		return SessionRecord{}, errors.New("invalid session capability")
	}
	if subtle.ConstantTimeCompare([]byte(auth.Capability), []byte(expected)) != 1 {
		return SessionRecord{}, errors.New("invalid session capability")
	}
	return record, nil
}

func validateSessionRecord(paths Paths, record SessionRecord) error {
	if !LocalSessionIDRE.MatchString(record.SessionID) || !ProfileRE.MatchString(record.Profile) || !isBotName(record.BotName) {
		return errors.New("invalid session record")
	}
	if !safeOptionalRegistryIdentity(record.BotID) || !safeOptionalRegistryIdentity(record.BotAgentID) {
		return errors.New("invalid session record")
	}
	if !isSessionScopeType(record.ScopeType) || !isSafeScopeID(record.ScopeType, record.ScopeID) {
		return errors.New("invalid session record")
	}
	if !LocalSessionGenerationIDRE.MatchString(record.Generation) {
		return errors.New("invalid session record")
	}
	if _, err := sessionCapabilityPathForRecord(paths, record); err != nil {
		return errors.New("invalid session record")
	}
	record.Host = normalizeSessionHost(record.Host)
	if !isSessionHost(record.Host) || !isManagedSessionRuntime(record.Runtime) || !isSessionState(record.State) {
		return errors.New("invalid session record")
	}
	if record.RuntimeSessionRef != "" && !isRuntimeSessionRef(record.Runtime, record.RuntimeSessionRef) {
		return errors.New("invalid session record")
	}
	if record.Host == SessionHostBmux && record.Runtime == "claude" && record.RuntimeSessionRef == "" {
		return errors.New("invalid session record")
	}
	if record.OutputLogPath != "" && !isSafeAbsoluteLocalPath(record.OutputLogPath) {
		return errors.New("invalid session record")
	}
	if record.Host == SessionHostBmux && record.OutputLogPath == "" {
		return errors.New("invalid session record")
	}
	if record.WorkingDir != "" && !isSafeAbsoluteLocalPath(record.WorkingDir) {
		return errors.New("invalid session record")
	}
	if record.SessionName != "" {
		if err := validateSessionNameForHost(record.Host, record.SessionName); err != nil {
			return errors.New("invalid session record")
		}
	}
	if record.PaneTarget != "" {
		if err := validatePaneTargetForHost(record.Host, record.PaneTarget); err != nil {
			return errors.New("invalid session record")
		}
		if record.Host == SessionHostBmux {
			if err := validateBmuxSessionPaneTarget(paths, record.SessionName, record.PaneTarget); err != nil {
				return errors.New("invalid session record")
			}
		}
	}
	if err := validateRuntimeCommand(record); err != nil {
		return errors.New("invalid session record")
	}
	if record.DailyReset != nil {
		if err := validateDailyResetRecord(*record.DailyReset); err != nil {
			return errors.New("invalid session record")
		}
	}
	return nil
}

func validateDailyResetRecord(record DailyResetRecord) error {
	if _, err := time.Parse("2006-01-02", record.Date); err != nil {
		return err
	}
	switch record.State {
	case "requested", "replacing", "completed":
	default:
		return errors.New("invalid daily reset state")
	}
	if record.Reason == "" || len(record.Reason) > 128 || strings.ContainsAny(record.Reason, "\r\n\x00") || containsSecretValue(record.Reason) {
		return errors.New("invalid daily reset reason")
	}
	if _, err := time.Parse(time.RFC3339Nano, record.RequestedAt); err != nil {
		return err
	}
	if record.PromptedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, record.PromptedAt); err != nil {
			return err
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, record.DeadlineAt); err != nil {
		return err
	}
	if !isSafeAbsoluteLocalPath(record.LogPath) {
		return errors.New("invalid daily reset log path")
	}
	if record.CompletedAt != "" {
		if _, err := time.Parse(time.RFC3339Nano, record.CompletedAt); err != nil {
			return err
		}
	}
	if record.ReplacementSessionID != "" && !LocalSessionIDRE.MatchString(record.ReplacementSessionID) {
		return errors.New("invalid replacement session id")
	}
	return nil
}

func isSessionState(state string) bool {
	switch state {
	case "starting", "alive", "stale", "dead":
		return true
	default:
		return false
	}
}

func isV1RegisterSessionScope(profile string, scopeType string, scopeID string) bool {
	return scopeType == "profile" && scopeID == profile
}

func validateSessionRecordFileTrust(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("could not inspect session")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("session file must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return errors.New("session path is not a regular file")
	}
	if err := validateCurrentUserOwner(info, "session file"); err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("session file must be private")
	}
	return nil
}

func sessionRecordPath(paths Paths, sessionID string) string {
	return filepath.Join(paths.Sessions, sessionID+".json")
}

func sessionCapabilityPath(paths Paths, profile string, sessionID string, generation string) string {
	return filepath.Clean(filepath.Join(paths.Capabilities, profile, sessionID, generation+".cap"))
}

func sessionCapabilityPathForRecord(paths Paths, record SessionRecord) (string, error) {
	expectedPath := sessionCapabilityPath(paths, record.Profile, record.SessionID, record.Generation)
	if sessionCapabilityPathMatches(record.CapabilityFile, expectedPath) {
		return expectedPath, nil
	}
	return "", errors.New("invalid session capability path")
}

func sessionCapabilityPathMatches(recordPath string, expectedPath string) bool {
	recordPath = filepath.Clean(recordPath)
	expectedPath = filepath.Clean(expectedPath)
	if recordPath == expectedPath {
		return true
	}
	if !filepath.IsAbs(recordPath) || !filepath.IsAbs(expectedPath) {
		return false
	}
	recordInfo, err := os.Lstat(recordPath)
	if err != nil || recordInfo.Mode()&os.ModeSymlink != 0 {
		return false
	}
	expectedInfo, err := os.Lstat(expectedPath)
	if err != nil || expectedInfo.Mode()&os.ModeSymlink != 0 {
		return false
	}
	return os.SameFile(recordInfo, expectedInfo)
}
