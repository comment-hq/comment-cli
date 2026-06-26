package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// runtimeAuthHeaderName is the daemon poll header that carries which coding
// runtimes are locally authenticated. The server reads it in requireDaemonAuth
// and persists it on the paired-daemon record so /setup and `comment status`
// can show the "log in to a coding agent" readiness step.
const runtimeAuthHeaderName = "X-Comment-Runtime-Auth"

// reportableRuntimes is the canonical (already-sorted) set of runtimes whose
// local auth state the daemon reports. Order here defines the header order, so
// the server's change-detection never churns a write on reordering.
var reportableRuntimes = []string{"claude", "codex"}

// runtimeAuthHeaderValue returns the comma-separated list of coding runtimes
// that appear logged in here (e.g. "claude,codex"), or "" when none are. The
// daemon always sends this header on token-authenticated polls, so the server
// can tell "nothing logged in yet" (empty value) apart from "daemon too old to
// report" (header absent).
//
// Login state is the ONLY thing checked — deliberately NOT install/PATH. This
// runs inside the background daemon, whose launchd/systemd PATH does not include
// user-local install locations (~/.local/bin, Homebrew, nvm — where claude/codex
// normally live); `comment run` resolves the runtime against the user's
// interactive PATH on the client instead (see resolveRuntimeCommandPath). So an
// install/PATH gate here would report "none" for the COMMON case (installed +
// logged in, just not on the daemon's PATH) and leave /setup stuck after login —
// a far worse and more common failure than the rare logged-in-but-uninstalled
// edge. runtimeAuthState reads fixed config paths (~/.claude.json,
// ~/.codex/auth.json) + env keys, which are reliable in any PATH context.
func runtimeAuthHeaderValue() string {
	authed := make([]string, 0, len(reportableRuntimes))
	for _, runtime := range reportableRuntimes {
		if ok, _ := runtimeAuthState(runtime); ok {
			authed = append(authed, runtime)
		}
	}
	return strings.Join(authed, ",")
}

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
