package commentbus

import (
	"encoding/json"
	"errors"
	"strings"
)

// Sentinel errors for PrepareAgentProfileWrite validation failures. Callers
// that need product-specific phrasing (e.g. Botlets) can map these with
// errors.Is.
var (
	ErrMissingAgentCredential = errors.New("missing agent credential")
	ErrInvalidAgentHandle     = errors.New("invalid agent handle")
	ErrInvalidAgentRuntime    = errors.New("invalid runtime")
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
			Path:        profilePath,
		},
	}, nil
}

// Write atomically persists the prepared profile with owner-only permissions.
func (w AgentProfileWrite) Write() error {
	return WritePrivateFileAtomicExistingDir(w.Path, w.Data, 0o600)
}
