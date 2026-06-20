package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// runtimeAuthState reports whether the given coding CLI ("claude" or "codex")
// appears to have valid local auth, plus a one-line "how to log in" hint when it
// does not. It is best-effort and non-interactive: it reads the CLI's own config
// file rather than invoking the CLI, and treats an unknown runtime or unreadable/
// malformed config as authed=true so we never block or nag on something we
// cannot reliably assess. Callers should only consult it for a runtime they have
// already confirmed is installed.
func runtimeAuthState(runtime string) (authed bool, hint string) {
	switch runtime {
	case "claude":
		return claudeAuthState()
	case "codex":
		return codexAuthState()
	default:
		return true, ""
	}
}

// claudeAuthState: Claude Code records OAuth login as a non-null `oauthAccount`
// object in ~/.claude.json; an ANTHROPIC_API_KEY also counts as authenticated.
func claudeAuthState() (bool, string) {
	const hint = "Claude Code isn't logged in yet — run `claude` and sign in (or `claude setup-token`)."
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return true, ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return true, "" // can't assess — don't nag
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return false, hint
	}
	var cfg struct {
		OAuthAccount json.RawMessage `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return true, "" // malformed — don't nag
	}
	if len(cfg.OAuthAccount) > 0 && string(cfg.OAuthAccount) != "null" {
		return true, ""
	}
	return false, hint
}

// codexAuthState: Codex stores tokens in $CODEX_HOME/auth.json (default
// ~/.codex/auth.json) as tokens.access_token; an OPENAI_API_KEY (in the file or
// the environment) also counts as authenticated.
func codexAuthState() (bool, string) {
	const hint = "Codex isn't logged in yet — run `codex login`."
	if os.Getenv("OPENAI_API_KEY") != "" {
		return true, ""
	}
	codexHome := os.Getenv("CODEX_HOME")
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return true, "" // can't assess — don't nag
		}
		codexHome = filepath.Join(home, ".codex")
	}
	data, err := os.ReadFile(filepath.Join(codexHome, "auth.json"))
	if err != nil {
		return false, hint
	}
	var cfg struct {
		OpenAIAPIKey string `json:"OPENAI_API_KEY"`
		Tokens       struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return true, "" // malformed — don't nag
	}
	if cfg.Tokens.AccessToken != "" || cfg.OpenAIAPIKey != "" {
		return true, ""
	}
	return false, hint
}
