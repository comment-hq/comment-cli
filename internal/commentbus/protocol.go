package commentbus

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	socketRequestIDRE    = regexp.MustCompile(`^req_[A-Za-z0-9-]{1,116}$`)
	socketCursorRE       = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,256}$`)
	ListenSessionTokenRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	safeParamKeyRE       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	secretValueREs       = []*regexp.Regexp{
		regexp.MustCompile(`(^|[^A-Za-z0-9])as_[A-Za-z0-9_-]+($|[^A-Za-z0-9])`),
		regexp.MustCompile(`(^|[^A-Za-z0-9])ark_[A-Za-z0-9_.-]+_[A-Za-z0-9_-]+($|[^A-Za-z0-9])`),
		regexp.MustCompile(`(^|[^A-Za-z0-9])(cap|clm|ntf|mbx)_[A-Za-z0-9_-]+($|[^A-Za-z0-9])`),
	}
	sensitiveFieldRE = regexp.MustCompile(`(?i)(^|_)(access_token|agent_secret|ark_key|capability|claim_id|mailbox_message_id|notification_id|secret|token)(_|$)`)
)

var socketOperations = map[string]struct{}{
	"health":                  {},
	"reload-profiles":         {},
	"messages.send":           {},
	"messages.wait":           {},
	"messages.receive":        {},
	"messages.renew":          {},
	"messages.ack":            {},
	"messages.release":        {},
	"messages.list":           {},
	"messages.sent":           {},
	"messages.repair":         {},
	"activity.complete":       {},
	"sessions.register":       {},
	"sessions.start":          {},
	"sessions.stop":           {},
	"sessions.reset-complete": {},
	"sessions.status":         {},
	"sessions.nudge":          {},
	"runtime.start":           {},
	"runtime.stop":            {},
	"runtime.status":          {},
	"runtime.list":            {},
	"listen.handles":          {},
	"listen.claim":            {},
	"listen.release":          {},
	"agents.mint-ephemeral":   {},
}

func sessionAuthAllowedForOperation(op string) bool {
	switch op {
	case "reload-profiles", "messages.send", "messages.wait", "messages.receive", "messages.renew", "messages.ack", "messages.release", "messages.list", "activity.complete", "sessions.status", "sessions.stop", "sessions.reset-complete", "sessions.nudge":
		return true
	default:
		return false
	}
}

type SocketRequest struct {
	ID          string         `json:"id"`
	Op          string         `json:"op"`
	Auth        *SocketAuth    `json:"auth,omitempty"`
	Params      map[string]any `json:"params"`
	authPresent bool
	rawParams   json.RawMessage
}

type SocketAuth struct {
	Mode                     string  `json:"mode"`
	Capability               string  `json:"capability"`
	Profile                  *string `json:"profile,omitempty"`
	SessionID                *string `json:"session_id,omitempty"`
	SessionGeneration        *string `json:"session_generation,omitempty"`
	profilePresent           bool
	sessionIDPresent         bool
	sessionGenerationPresent bool
}

func (req *SocketRequest) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var decoded SocketRequest
	if value, ok := raw["id"]; ok {
		_ = json.Unmarshal(value, &decoded.ID)
	}
	if value, ok := raw["op"]; ok {
		_ = json.Unmarshal(value, &decoded.Op)
	}
	_, authPresent := raw["auth"]
	if value, ok := raw["auth"]; ok && string(value) != "null" && isJSONRawObject(value) {
		var auth SocketAuth
		if err := json.Unmarshal(value, &auth); err == nil {
			decoded.Auth = &auth
		} else {
			decoded.Auth = &SocketAuth{}
		}
	}
	decoded.rawParams = raw["params"]
	decoded.authPresent = authPresent
	*req = decoded
	return nil
}

func (auth *SocketAuth) UnmarshalJSON(data []byte) error {
	type authAlias SocketAuth
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	var decoded authAlias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*auth = SocketAuth(decoded)
	_, auth.profilePresent = raw["profile"]
	_, auth.sessionIDPresent = raw["session_id"]
	_, auth.sessionGenerationPresent = raw["session_generation"]
	return nil
}

func isJSONRawObject(raw json.RawMessage) bool {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return false
	}
	_, ok := value.(map[string]any)
	return ok
}

func ValidateSocketRequest(req SocketRequest) error {
	_, err := validateAndDecodeSocketRequest(req)
	return err
}

func validateAndDecodeSocketRequest(req SocketRequest) (SocketRequest, error) {
	if err := validateSocketEnvelopeForAuth(req); err != nil {
		return req, err
	}
	var err error
	req, err = decodeSocketParams(req)
	if err != nil {
		return req, err
	}
	if err := validateSocketParams(req.Op, req.Params); err != nil {
		return req, err
	}
	if err := validateAuthScopedSocketParams(req); err != nil {
		return req, err
	}
	return req, nil
}

func validateAuthScopedSocketParams(req SocketRequest) error {
	if req.Auth != nil && req.Auth.Mode == "session" && !sessionAuthAllowedForOperation(req.Op) {
		return errors.New("session auth is not allowed for this operation")
	}
	if req.Auth != nil && req.Auth.Mode == "session" && req.Op == "reload-profiles" {
		if _, ok := req.Params["botlets_home"].(string); !ok {
			return errors.New("session reload requires botlets_home")
		}
	}
	if req.Auth != nil && req.Auth.Mode == "owner" && req.Op == "activity.complete" {
		profile, _ := req.Params["profile"].(string)
		if req.Auth.Profile == nil || profile == "" || *req.Auth.Profile != profile {
			return errors.New("owner profile does not match activity profile")
		}
	}
	if req.Auth != nil && req.Auth.Mode == "owner" && (req.Op == "sessions.stop" || req.Op == "sessions.nudge") {
		if _, hasBot := req.Params["bot"]; !hasBot {
			if _, hasProfile := req.Params["profile"]; !hasProfile {
				if _, hasSessionID := req.Params["session_id"]; !hasSessionID {
					return errors.New("missing session selector")
				}
			}
		}
	}
	if req.Auth != nil && req.Auth.Mode == "owner" && (req.Op == "runtime.start" || req.Op == "runtime.status" || req.Op == "runtime.list") {
		profile, _ := req.Params["profile"].(string)
		if req.Auth.Profile == nil || *req.Auth.Profile != profile {
			return errors.New("owner profile does not match runtime profile")
		}
	}
	if req.Auth != nil && req.Auth.Mode == "owner" && req.Op == "runtime.stop" {
		if req.Auth.Profile == nil {
			return errors.New("runtime stop requires owner profile")
		}
	}
	return nil
}

func validateSocketEnvelopeForAuth(req SocketRequest) error {
	if !isSafeSocketRequestID(req.ID) {
		return errors.New("invalid request id")
	}
	if _, ok := socketOperations[req.Op]; !ok {
		return errors.New("invalid operation")
	}
	if req.Op == "health" {
		if req.authPresent || req.Auth != nil {
			return errors.New("health must not include auth")
		}
		return nil
	}
	// agents.mint-ephemeral runs during a no-credential bootstrap (a fresh
	// session with no ark_ key and no owner capability), so auth is OPTIONAL:
	// the socket is already UID-gated to the owner and the daemon mints with its
	// OWN pairing token. If auth IS supplied it must still be well-formed.
	if req.Op == "agents.mint-ephemeral" {
		if req.Auth == nil {
			return nil
		}
		return validateSocketAuth(*req.Auth)
	}
	if req.Auth == nil {
		return errors.New("missing auth")
	}
	return validateSocketAuth(*req.Auth)
}

func decodeSocketParams(req SocketRequest) (SocketRequest, error) {
	if req.rawParams != nil {
		var params map[string]any
		if err := json.Unmarshal(req.rawParams, &params); err != nil || params == nil {
			return req, errors.New("invalid params")
		}
		req.Params = params
	}
	if req.Params == nil {
		return req, errors.New("invalid params")
	}
	return req, nil
}

func isSafeSocketRequestID(id string) bool {
	return socketRequestIDRE.MatchString(id) && !containsSecretValue(id)
}

func validateSocketAuth(auth SocketAuth) error {
	switch auth.Mode {
	case "owner":
		if len(auth.Capability) < 20 {
			return errors.New("invalid owner capability")
		}
		if auth.sessionIDPresent || auth.sessionGenerationPresent || auth.SessionID != nil || auth.SessionGeneration != nil {
			return errors.New("owner auth must not include session fields")
		}
		if auth.profilePresent && auth.Profile == nil {
			return errors.New("invalid owner profile")
		}
		if auth.Profile != nil && !ProfileRE.MatchString(*auth.Profile) {
			return errors.New("invalid owner profile")
		}
		return nil
	case "session":
		if len(auth.Capability) < 20 {
			return errors.New("invalid session capability")
		}
		if auth.Profile == nil || !ProfileRE.MatchString(*auth.Profile) {
			return errors.New("invalid session profile")
		}
		if auth.SessionID == nil || !LocalSessionIDRE.MatchString(*auth.SessionID) {
			return errors.New("invalid session id")
		}
		if auth.SessionGeneration == nil || !LocalSessionGenerationIDRE.MatchString(*auth.SessionGeneration) {
			return errors.New("invalid session generation")
		}
		return nil
	default:
		return errors.New("invalid auth mode")
	}
}

func validateSocketParams(op string, params map[string]any) error {
	switch op {
	case "health":
		return exactParams(params)
	case "reload-profiles":
		return validateReloadProfilesParams(params)
	case "messages.send":
		return validateSendParams(params)
	case "messages.wait":
		return validateWaitParams(params)
	case "messages.receive":
		return validateMessageMutationParams(params)
	case "messages.renew":
		return validateMessageMutationParams(params, "lease_ttl_ms", "op_id")
	case "messages.ack":
		return validateMessageMutationParams(params, "op_id")
	case "messages.release":
		return validateMessageMutationParams(params, "op_id", "reason")
	case "messages.list":
		return validateMessageListParams(params, true)
	case "messages.sent":
		return validateMessageListParams(params, false)
	case "messages.repair":
		return validateRepairParams(params)
	case "activity.complete":
		return validateMessageMutationParams(params, "op_id")
	case "sessions.register":
		return validateSessionRegisterParams(params)
	case "sessions.start":
		return validateSessionStartParams(params)
	case "sessions.stop":
		return validateSessionStopParams(params)
	case "sessions.reset-complete":
		return validateSessionResetCompleteParams(params)
	case "sessions.status":
		return validateSessionLookupParams(params)
	case "sessions.nudge":
		return validateSessionNudgeParams(params)
	case "runtime.start":
		return validateRuntimeStartParams(params)
	case "runtime.stop":
		return validateRuntimeStopParams(params)
	case "runtime.status":
		return validateRuntimeLookupParams(params)
	case "runtime.list":
		return validateRuntimeLookupParams(params)
	case "listen.handles":
		return exactParams(params)
	case "listen.claim":
		return validateListenClaimParams(params)
	case "listen.release":
		return validateListenClaimParams(params)
	case "agents.mint-ephemeral":
		return validateMintEphemeralParams(params)
	default:
		return errors.New("invalid operation")
	}
}

// validateMintEphemeralParams accepts a required, safe `session` token (the
// stable per-session id the cred is keyed to) and an optional `display_name`.
func validateMintEphemeralParams(params map[string]any) error {
	if err := exactParams(params, "session", "display_name"); err != nil {
		return err
	}
	if !isListenSessionToken(params["session"]) {
		return errors.New("invalid session")
	}
	if value, ok := params["display_name"]; ok {
		name, isStr := value.(string)
		if !isStr || len(name) > 200 || strings.ContainsRune(name, '\x00') || containsSecretValue(name) {
			return errors.New("invalid display_name")
		}
	}
	return nil
}

func validateListenClaimParams(params map[string]any) error {
	if err := exactParams(params, "profile", "session", "force"); err != nil {
		return err
	}
	if !isStringMatch(params["profile"], ProfileRE) {
		return errors.New("invalid profile")
	}
	if value, ok := params["session"]; ok && !isListenSessionToken(value) {
		return errors.New("invalid session")
	}
	if value, ok := params["force"]; ok {
		if _, isBool := value.(bool); !isBool {
			return errors.New("invalid force")
		}
	}
	return nil
}

// isListenSessionToken accepts a short, safe identifier for the impromptu
// listening session (for example a Claude Code session uuid). It is opaque to
// the daemon — used only as the claimed_by label — so it is validated as a safe
// token rather than a specific local-id shape.
func isListenSessionToken(value any) bool {
	token, ok := value.(string)
	if !ok {
		return false
	}
	if len(token) == 0 || len(token) > 128 {
		return false
	}
	if containsSecretValue(token) {
		return false
	}
	return ListenSessionTokenRE.MatchString(token)
}

func validateReloadProfilesParams(params map[string]any) error {
	if err := exactParams(params, "botlets_home"); err != nil {
		return err
	}
	if value, ok := params["botlets_home"]; ok && !isSafeLocalPath(value) {
		return errors.New("invalid botlets_home")
	}
	return nil
}

func validateSendParams(params map[string]any) error {
	if err := exactParams(params, "from_bot", "to", "body", "refs", "idempotency_key", "thread_id"); err != nil {
		return err
	}
	if value, ok := params["from_bot"]; ok && !isBotName(value) {
		return errors.New("invalid from_bot")
	}
	to, ok := params["to"].([]any)
	if !ok || len(to) == 0 || len(to) > 50 {
		return errors.New("invalid to")
	}
	for _, target := range to {
		if !isSafeRecipientTarget(target) {
			return errors.New("invalid to")
		}
	}
	body, ok := params["body"].(map[string]any)
	if !ok || body["format"] != "markdown" {
		return errors.New("invalid body")
	}
	content, ok := body["content"].(string)
	if !ok || len(content) > 1_000_000 || containsSecretValue(content) {
		return errors.New("invalid body")
	}
	if refs, ok := params["refs"]; ok && !isSafeRefs(refs) {
		return errors.New("invalid refs")
	}
	if value, ok := params["idempotency_key"]; ok && !isStringMatch(value, LocalOperationIDRE) {
		return errors.New("invalid idempotency_key")
	}
	if value, ok := params["thread_id"]; ok && value != nil {
		threadID, ok := value.(string)
		if !ok || len(threadID) > 256 || strings.ContainsAny(threadID, "\r\n\x00") || containsSecretValue(threadID) {
			return errors.New("invalid thread_id")
		}
	}
	return nil
}

func validateWaitParams(params map[string]any) error {
	if err := exactParams(params, "bot", "profile", "timeout_ms", "kinds", "rewake", "session_id", "session_generation", "listen_session"); err != nil {
		return err
	}
	if err := validateOptionalBotProfile(params); err != nil {
		return err
	}
	if value, ok := params["rewake"]; ok {
		if _, isBool := value.(bool); !isBool {
			return errors.New("invalid rewake")
		}
	}
	if value, ok := params["listen_session"]; ok && !isListenSessionToken(value) {
		return errors.New("invalid listen_session")
	}
	if value, ok := params["session_id"]; ok && !isStringMatch(value, LocalSessionIDRE) {
		return errors.New("invalid session_id")
	}
	if value, ok := params["session_generation"]; ok && !isStringMatch(value, LocalSessionGenerationIDRE) {
		return errors.New("invalid session_generation")
	}
	if value, ok := params["timeout_ms"]; ok && !isNumberInRange(value, 0, 24*60*60_000) {
		return errors.New("invalid timeout_ms")
	}
	if raw, ok := params["kinds"]; ok {
		kinds, ok := raw.([]any)
		if !ok || len(kinds) == 0 {
			return errors.New("invalid kinds")
		}
		for _, kind := range kinds {
			s, ok := kind.(string)
			if !ok || !isMessageKind(s) {
				return errors.New("invalid kinds")
			}
		}
	}
	return nil
}

func validateMessageMutationParams(params map[string]any, optional ...string) error {
	allowed := append([]string{"message_id", "bot", "profile"}, optional...)
	if err := exactParams(params, allowed...); err != nil {
		return err
	}
	if err := validateOptionalBotProfile(params); err != nil {
		return err
	}
	if !isStringMatch(params["message_id"], LocalMessageIDRE) {
		return errors.New("invalid message_id")
	}
	if value, ok := params["lease_ttl_ms"]; ok && !isNumberInRange(value, 1_000, 60*60_000) {
		return errors.New("invalid lease_ttl_ms")
	}
	if value, ok := params["op_id"]; ok && !isStringMatch(value, LocalOperationIDRE) {
		return errors.New("invalid op_id")
	}
	if value, ok := params["reason"]; ok {
		reason, ok := value.(string)
		if !ok || len(reason) > 512 || strings.ContainsAny(reason, "\r\n\x00") || containsSecretValue(reason) {
			return errors.New("invalid reason")
		}
	}
	return nil
}

func validateMessageListParams(params map[string]any, includeState bool) error {
	allowed := []string{"bot", "profile", "limit", "cursor"}
	if includeState {
		allowed = append(allowed, "state")
	}
	if err := exactParams(params, allowed...); err != nil {
		return err
	}
	if err := validateOptionalBotProfile(params); err != nil {
		return err
	}
	if value, ok := params["state"]; ok && !isDeliveryState(value) {
		return errors.New("invalid state")
	}
	if value, ok := params["limit"]; ok && !isNumberInRange(value, 1, 200) {
		return errors.New("invalid limit")
	}
	if value, ok := params["cursor"]; ok && !isStringMatch(value, socketCursorRE) {
		return errors.New("invalid cursor")
	}
	return nil
}

func validateRepairParams(params map[string]any) error {
	if err := exactParams(params, "dry_run", "message_id", "op_id"); err != nil {
		return err
	}
	if _, ok := params["dry_run"].(bool); !ok {
		return errors.New("repair requires dry_run")
	}
	if value, ok := params["message_id"]; ok && !isStringMatch(value, LocalMessageIDRE) {
		return errors.New("invalid message_id")
	}
	if value, ok := params["op_id"]; ok && !isStringMatch(value, LocalOperationIDRE) {
		return errors.New("invalid op_id")
	}
	return nil
}

func validateSessionRegisterParams(params map[string]any) error {
	if err := exactParams(params, "profile", "bot_name", "scope_type", "scope_id", "session_id", "generation"); err != nil {
		return err
	}
	if !isStringMatch(params["profile"], ProfileRE) {
		return errors.New("invalid profile")
	}
	if !isBotName(params["bot_name"]) {
		return errors.New("invalid bot_name")
	}
	scopeType, ok := params["scope_type"].(string)
	if !ok || !isSessionScopeType(scopeType) || scopeType != "profile" {
		return errors.New("invalid scope_type")
	}
	scopeID, ok := params["scope_id"].(string)
	if !ok || !isSafeScopeID(scopeType, scopeID) {
		return errors.New("invalid scope_id")
	}
	if scopeID != params["profile"] {
		return errors.New("invalid scope_id")
	}
	if value, ok := params["session_id"]; ok && !isStringMatch(value, LocalSessionIDRE) {
		return errors.New("invalid session_id")
	}
	if value, ok := params["generation"]; ok && !isStringMatch(value, LocalSessionGenerationIDRE) {
		return errors.New("invalid generation")
	}
	return nil
}

func validateSessionStartParams(params map[string]any) error {
	if err := exactParams(params, "bot", "profile", "scope_type", "scope_id", "expected_runtime"); err != nil {
		return err
	}
	if err := validateOptionalBotProfile(params); err != nil {
		return err
	}
	if expectedRuntime, ok := params["expected_runtime"]; ok {
		value, ok := expectedRuntime.(string)
		if !ok || !isManagedSessionRuntime(value) {
			return errors.New("invalid expected_runtime")
		}
	}
	_, hasType := params["scope_type"]
	_, hasID := params["scope_id"]
	if hasType || hasID {
		scopeType, ok := params["scope_type"].(string)
		if !ok || !isSessionScopeType(scopeType) || scopeType != "profile" {
			return errors.New("invalid scope_type")
		}
		scopeID, ok := params["scope_id"].(string)
		if !ok || !isSafeScopeID(scopeType, scopeID) {
			return errors.New("invalid scope_id")
		}
		profile, ok := params["profile"].(string)
		if !ok || scopeID != profile {
			return errors.New("invalid scope_id")
		}
	}
	return nil
}

func validateSessionStopParams(params map[string]any) error {
	if err := exactParams(params, "bot", "profile", "session_id", "reason"); err != nil {
		return err
	}
	if err := validateOptionalBotProfile(params); err != nil {
		return err
	}
	if value, ok := params["session_id"]; ok && !isStringMatch(value, LocalSessionIDRE) {
		return errors.New("invalid session_id")
	}
	if value, ok := params["reason"]; ok {
		reason, ok := value.(string)
		if !ok || len(reason) > 512 || strings.ContainsAny(reason, "\r\n\x00") || containsSecretValue(reason) {
			return errors.New("invalid reason")
		}
	}
	return nil
}

func validateSessionResetCompleteParams(params map[string]any) error {
	if err := exactParams(params, "bot", "profile", "session_id", "log_path"); err != nil {
		return err
	}
	if err := validateOptionalBotProfile(params); err != nil {
		return err
	}
	if value, ok := params["session_id"]; ok && !isStringMatch(value, LocalSessionIDRE) {
		return errors.New("invalid session_id")
	}
	if value, ok := params["log_path"]; ok {
		path, ok := value.(string)
		if !ok || !isSafeAbsoluteLocalPath(path) {
			return errors.New("invalid log_path")
		}
	}
	return nil
}

func validateSessionLookupParams(params map[string]any) error {
	if err := exactParams(params, "bot", "profile", "session_id"); err != nil {
		return err
	}
	if err := validateOptionalBotProfile(params); err != nil {
		return err
	}
	if value, ok := params["session_id"]; ok && !isStringMatch(value, LocalSessionIDRE) {
		return errors.New("invalid session_id")
	}
	return nil
}

func validateSessionNudgeParams(params map[string]any) error {
	if err := exactParams(params, "bot", "profile", "session_id", "message_id"); err != nil {
		return err
	}
	if err := validateOptionalBotProfile(params); err != nil {
		return err
	}
	if value, ok := params["session_id"]; ok && !isStringMatch(value, LocalSessionIDRE) {
		return errors.New("invalid session_id")
	}
	if !isStringMatch(params["message_id"], LocalMessageIDRE) {
		return errors.New("invalid message_id")
	}
	return nil
}

func validateRuntimeStartParams(params map[string]any) error {
	if err := exactParams(params, "profile", "cwd", "runtime_command", "runtime_command_path", "role"); err != nil {
		return err
	}
	if !isStringMatch(params["profile"], ProfileRE) {
		return errors.New("invalid profile")
	}
	if !isSafeLocalPath(params["cwd"]) {
		return errors.New("invalid cwd")
	}
	command, ok := params["runtime_command"].([]any)
	if !ok || len(command) == 0 || len(command) > 128 {
		return errors.New("invalid runtime_command")
	}
	for _, arg := range command {
		text, ok := arg.(string)
		if !ok || text == "" || len(text) > 4096 || strings.ContainsAny(text, "\r\n\x00") || containsSecretValue(text) {
			return errors.New("invalid runtime_command")
		}
	}
	if raw, present := params["runtime_command_path"]; present {
		path, ok := raw.(string)
		if !ok || path == "" || len(path) > 4096 || !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") || containsSecretValue(path) {
			return errors.New("invalid runtime_command_path")
		}
	}
	if raw, present := params["role"]; present {
		role, ok := raw.(string)
		if !ok || !isTransientRuntimeRole(role) {
			return errors.New("invalid role")
		}
	}
	return nil
}

func validateRuntimeStopParams(params map[string]any) error {
	if err := exactParams(params, "run_id"); err != nil {
		return err
	}
	if !isStringMatch(params["run_id"], LocalSessionIDRE) {
		return errors.New("invalid run_id")
	}
	return nil
}

func validateRuntimeLookupParams(params map[string]any) error {
	if err := exactParams(params, "profile"); err != nil {
		return err
	}
	if !isStringMatch(params["profile"], ProfileRE) {
		return errors.New("invalid profile")
	}
	return nil
}

func validateOptionalBotProfile(params map[string]any) error {
	if value, ok := params["bot"]; ok && !isBotName(value) {
		return errors.New("invalid bot")
	}
	if value, ok := params["profile"]; ok && !isStringMatch(value, ProfileRE) {
		return errors.New("invalid profile")
	}
	return nil
}

func exactParams(params map[string]any, allowed ...string) error {
	allowedSet := map[string]struct{}{}
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	for key := range params {
		if _, ok := allowedSet[key]; !ok {
			return errors.New("unexpected param")
		}
	}
	return nil
}

func isStringMatch(value any, re *regexp.Regexp) bool {
	s, ok := value.(string)
	return ok && re.MatchString(s)
}

func isBotName(value any) bool {
	name, ok := value.(string)
	return ok && len(name) >= 3 && len(name) <= 63 && BotNameRE.MatchString(name)
}

func isNumberInRange(value any, minValue, maxValue int) bool {
	var n float64
	switch v := value.(type) {
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case float64:
		n = v
	default:
		return false
	}
	return n == float64(int(n)) && n >= float64(minValue) && n <= float64(maxValue)
}

func isSafeRecipientTarget(value any) bool {
	target, ok := value.(string)
	if !ok {
		return false
	}
	target = strings.TrimPrefix(target, "@")
	return isBotName(target) || ProfileRE.MatchString(target)
}

func isSafeLocalPath(value any) bool {
	path, ok := value.(string)
	return ok && isSafeHomeOrAbsolutePath(path)
}

func isSafeRefs(value any) bool {
	refs, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for key, entry := range refs {
		if !safeParamKeyRE.MatchString(key) || sensitiveFieldRE.MatchString(key) {
			return false
		}
		switch v := entry.(type) {
		case nil, bool, int, int64, float64:
		case string:
			if len(v) > 1024 || strings.ContainsRune(v, '\x00') || containsSecretValue(v) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func isSafeScopeID(scopeType, value string) bool {
	switch scopeType {
	case "profile":
		return ProfileRE.MatchString(value)
	case "doc":
		return DocSlugRE.MatchString(value)
	case "message":
		return LocalMessageIDRE.MatchString(value)
	default:
		return false
	}
}

func isMessageKind(value string) bool {
	switch value {
	case "doc.mention", "doc.review_requested", "botlets.task", "message", "system":
		return true
	default:
		return false
	}
}

func isDeliveryState(value any) bool {
	state, ok := value.(string)
	if !ok {
		return false
	}
	switch state {
	case "unclaimed", "claimed", "acked", "released":
		return true
	default:
		return false
	}
}

func isSessionScopeType(value string) bool {
	switch value {
	case "profile", "doc", "message":
		return true
	default:
		return false
	}
}

func containsSecretValue(value string) bool {
	for _, re := range secretValueREs {
		if re.MatchString(value) {
			return true
		}
	}
	return false
}
