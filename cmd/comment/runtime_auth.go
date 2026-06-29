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
// ~/.codex/auth.json) + env keys, which are reliable in any PATH context. The
// Docker agent sandbox deliberately ignores env keys here because managed
// runtime sessions run with a scrubbed env; only file-backed auth survives.
func runtimeAuthHeaderValue() string {
	authed := make([]string, 0, len(reportableRuntimes))
	sandbox := runningInsideDockerAgentSandbox()
	for _, runtime := range reportableRuntimes {
		if ok, _ := runtimeAuthStateWithOptions(runtime, runtimeAuthOptions{
			allowEnvKeys:        !sandbox,
			allowRuntimeHomeEnv: !sandbox,
			strictFileAuth:      sandbox,
		}); ok {
			authed = append(authed, runtime)
		}
	}
	return strings.Join(authed, ",")
}

type runtimeAuthOptions struct {
	allowEnvKeys        bool
	allowRuntimeHomeEnv bool
	strictFileAuth      bool
}

// runtimeAuthState reports whether the given coding CLI ("claude" or "codex")
// appears to have valid local auth, plus a one-line "how to log in" hint when it
// does not. It is best-effort and non-interactive: it reads the CLI's own config
// file rather than invoking the CLI, and treats an unknown runtime or unreadable/
// malformed config as authed=true so we never block or nag on something we
// cannot reliably assess. Callers should only consult it for a runtime they have
// already confirmed is installed.
func runtimeAuthState(runtime string) (authed bool, hint string) {
	return runtimeAuthStateWithOptions(runtime, runtimeAuthOptions{allowEnvKeys: true, allowRuntimeHomeEnv: true})
}

func runtimeFileAuthState(runtime string) (authed bool, hint string) {
	return runtimeAuthStateWithOptions(runtime, runtimeAuthOptions{allowEnvKeys: false, allowRuntimeHomeEnv: false, strictFileAuth: true})
}

func runtimeAuthStateWithOptions(runtime string, options runtimeAuthOptions) (authed bool, hint string) {
	switch runtime {
	case "claude":
		return claudeAuthState(options)
	case "codex":
		return codexAuthState(options)
	default:
		return true, ""
	}
}

// claudeAuthState: Claude Code records OAuth login as a non-null `oauthAccount`
// object in ~/.claude.json on some versions, and subscription credentials in
// ~/.claude/.credentials.json on others; an ANTHROPIC_API_KEY can also count as
// authenticated when the caller allows env-key auth.
func claudeAuthState(options runtimeAuthOptions) (bool, string) {
	hint := "Claude Code isn't logged in yet — run `claude` and sign in (or `claude setup-token`)."
	if options.strictFileAuth {
		hint = "Claude Code isn't logged in yet — run `claude` inside the Docker sandbox, then `/login`."
	}
	if options.allowEnvKeys && os.Getenv("ANTHROPIC_API_KEY") != "" {
		return true, ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return true, "" // can't assess — don't nag
	}
	if ok, known := claudeJSONAuthState(filepath.Join(home, ".claude.json"), options.strictFileAuth); ok || !known {
		if !known && options.strictFileAuth {
			// Continue to the alternate credentials file before declaring strict
			// Docker readiness missing; a valid subscription file is enough.
		} else {
			return true, ""
		}
	}
	if ok, known := claudeCredentialsAuthState(filepath.Join(home, ".claude", ".credentials.json")); ok || !known {
		if !known && options.strictFileAuth {
			return false, hint
		}
		return true, ""
	}
	return false, hint
}

func claudeJSONAuthState(path string, strict bool) (authed bool, known bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, true
		}
		return true, false // can't assess — don't nag
	}
	var cfg struct {
		OAuthAccount json.RawMessage `json:"oauthAccount"`
	}
	if json.Unmarshal(data, &cfg) != nil {
		return true, false // malformed — don't nag
	}
	if len(cfg.OAuthAccount) > 0 && string(cfg.OAuthAccount) != "null" {
		if strict && !hasTokenBearingCredentialJSON(cfg.OAuthAccount) {
			return false, true
		}
		return true, true
	}
	return false, true
}

func hasTokenBearingCredentialJSON(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	return hasTokenBearingCredential(value)
}

func claudeCredentialsAuthState(path string) (authed bool, known bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, true
		}
		return true, false // can't assess — don't nag
	}
	if strings.TrimSpace(string(data)) == "" {
		return false, true
	}
	var cfg map[string]any
	if json.Unmarshal(data, &cfg) != nil {
		return true, false // malformed — don't nag
	}
	return hasTokenBearingCredential(cfg), true
}

func hasTokenBearingCredential(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if credentialTokenKey(key) {
				if s, ok := child.(string); ok && strings.TrimSpace(s) != "" {
					return true
				}
			}
			if hasTokenBearingCredential(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if hasTokenBearingCredential(child) {
				return true
			}
		}
	}
	return false
}

func credentialTokenKey(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	switch normalized {
	case "token", "accesstoken", "refreshtoken", "idtoken", "sessiontoken", "apikey", "anthropicapikey":
		return true
	default:
		return false
	}
}

// codexAuthState: Codex stores tokens in $CODEX_HOME/auth.json (default
// ~/.codex/auth.json) as tokens.access_token; an OPENAI_API_KEY in the file (or
// the environment, when allowed) also counts as authenticated. Docker sandbox
// readiness deliberately ignores CODEX_HOME because managed sessions scrub the
// env and Codex will use the default /home/agent/.codex path after reboot.
func codexAuthState(options runtimeAuthOptions) (bool, string) {
	const hint = "Codex isn't logged in yet — run `codex login`."
	if options.allowEnvKeys && os.Getenv("OPENAI_API_KEY") != "" {
		return true, ""
	}
	codexHome := ""
	if options.allowRuntimeHomeEnv {
		codexHome = os.Getenv("CODEX_HOME")
	}
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
		if options.strictFileAuth {
			return false, hint
		}
		return true, "" // malformed — don't nag
	}
	if strings.TrimSpace(cfg.Tokens.AccessToken) != "" || strings.TrimSpace(cfg.OpenAIAPIKey) != "" {
		return true, ""
	}
	return false, hint
}
