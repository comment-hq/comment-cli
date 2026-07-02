package commentbus

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf16"
)

// Sentinel errors for PrepareAgentProfileWrite validation failures. Callers
// that need product-specific phrasing (e.g. Botlets) can map these with
// errors.Is.
var (
	ErrMissingAgentCredential = errors.New("missing agent credential")
	ErrInvalidAgentHandle     = errors.New("invalid agent handle")
	ErrInvalidAgentRuntime    = errors.New("invalid runtime")
	ErrInvalidAgentModel      = errors.New("invalid model")
)

// AgentProfileWrite is a prepared, validated agent profile write: the target
// path under the agents directory, the serialized JSON payload, and the
// in-memory AgentProfile the payload represents.
type AgentProfileWrite struct {
	Path    string
	Data    []byte
	Profile AgentProfile
}

// PrepareAgentProfileWrite validates the agent credential and target path and
// builds the profile JSON without touching the filesystem beyond trust checks
// on the agents directory. Call Write on the result to persist it.
func PrepareAgentProfileWrite(paths Paths, handle, agentSecret, baseURL, runtime string) (AgentProfileWrite, error) {
	return prepareAgentProfileWrite(paths, handle, agentSecret, baseURL, runtime, "")
}

func PrepareAgentProfileWriteWithModel(paths Paths, handle, agentSecret, baseURL, runtime, model string) (AgentProfileWrite, error) {
	return prepareAgentProfileWrite(paths, handle, agentSecret, baseURL, runtime, model)
}

func NormalizeAgentModel(value string) (string, bool) {
	return normalizeAgentModel(value)
}

func normalizeAgentModel(value string) (string, bool) {
	model := strings.TrimSpace(value)
	if model == "" {
		return "", true
	}
	if agentModelLength(model) > 120 || strings.ContainsAny(model, "\x00\r\n") || containsSecretValue(model) {
		return "", false
	}
	for _, r := range model {
		if r >= 0 && r < 0x20 {
			return "", false
		}
	}
	return model, true
}

func agentModelLength(value string) int {
	length := 0
	for _, r := range value {
		length += utf16.RuneLen(r)
	}
	return length
}

func prepareAgentProfileWrite(paths Paths, handle, agentSecret, baseURL, runtime, model string) (AgentProfileWrite, error) {
	if handle == "" || agentSecret == "" {
		return AgentProfileWrite{}, ErrMissingAgentCredential
	}
	if !ProfileRE.MatchString(handle) {
		return AgentProfileWrite{}, ErrInvalidAgentHandle
	}
	runtime = strings.TrimSpace(runtime)
	if runtime != "" && runtime != "claude" && runtime != "codex" {
		return AgentProfileWrite{}, ErrInvalidAgentRuntime
	}
	model, ok := normalizeAgentModel(model)
	if !ok {
		return AgentProfileWrite{}, ErrInvalidAgentModel
	}
	profilePath, err := ValidateAgentProfileWriteTarget(paths, handle)
	if err != nil {
		return AgentProfileWrite{}, err
	}
	profileData := map[string]string{
		"handle":       handle,
		"agent_secret": agentSecret,
		"base_url":     baseURL,
	}
	if runtime != "" {
		profileData["runtime"] = runtime
	}
	if model != "" {
		profileData["model"] = model
	}
	data, err := json.MarshalIndent(profileData, "", "  ")
	if err != nil {
		return AgentProfileWrite{}, err
	}
	return AgentProfileWrite{
		Path: profilePath,
		Data: append(data, '\n'),
		Profile: AgentProfile{
			Handle:      handle,
			AgentSecret: agentSecret,
			BaseURL:     strings.TrimSuffix(baseURL, "/"),
			Runtime:     runtime,
			Model:       model,
			Path:        profilePath,
		},
	}, nil
}

// Write atomically persists the prepared profile with owner-only permissions.
func (w AgentProfileWrite) Write() error {
	return WritePrivateFileAtomicExistingDir(w.Path, w.Data, 0o600)
}
