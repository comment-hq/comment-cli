package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withRuntimeLookPath(t *testing.T, fn func(name string) (string, error)) {
	t.Helper()
	prev := runtimeLookPath
	runtimeLookPath = fn
	t.Cleanup(func() { runtimeLookPath = prev })
}

// lookPathSet returns a runtimeLookPath stub where only the named binaries resolve.
func lookPathSet(present ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/local/bin/" + name, nil
		}
		return "", errors.New("not found")
	}
}

func TestDetectRunRuntimePrefersClaudeFallsBackToCodex(t *testing.T) {
	withRuntimeLookPath(t, lookPathSet("claude", "codex"))
	if got := detectRunRuntime(); got != "claude" {
		t.Fatalf("both installed: got %q, want claude", got)
	}
	withRuntimeLookPath(t, lookPathSet("codex"))
	if got := detectRunRuntime(); got != "codex" {
		t.Fatalf("codex only: got %q, want codex", got)
	}
	withRuntimeLookPath(t, lookPathSet("claude"))
	if got := detectRunRuntime(); got != "claude" {
		t.Fatalf("claude only: got %q, want claude", got)
	}
	withRuntimeLookPath(t, lookPathSet())
	if got := detectRunRuntime(); got != "claude" {
		t.Fatalf("neither installed: got %q, want claude (fallback to the standard not-installed error path)", got)
	}
}

func TestClaudeAuthState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ANTHROPIC_API_KEY", "")

	if ok, hint := runtimeAuthState("claude"); ok || hint == "" {
		t.Fatalf("no config: want not-authed with hint, got ok=%v hint=%q", ok, hint)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{"email":"x@y.z"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := runtimeAuthState("claude"); !ok {
		t.Fatal("oauthAccount present: want authed")
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, hint := runtimeAuthState("claude"); !ok || hint != "" {
		t.Fatalf("malformed .claude.json: want authed/no hint, got ok=%v hint=%q", ok, hint)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := runtimeAuthState("claude"); ok {
		t.Fatal("oauthAccount null: want not-authed")
	}
	if err := os.Remove(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal(err)
	}
	credentialsDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(credentialsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialsDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := runtimeAuthState("claude"); !ok {
		t.Fatal(".claude/.credentials.json present: want authed")
	}
	if err := os.WriteFile(filepath.Join(credentialsDir, ".credentials.json"), []byte(`{"subscriptionType":"pro"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := runtimeAuthState("claude"); ok {
		t.Fatal("metadata-only .claude/.credentials.json: want not-authed")
	}
	if err := os.WriteFile(filepath.Join(credentialsDir, ".credentials.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := runtimeAuthState("claude"); ok {
		t.Fatal("empty .claude/.credentials.json: want not-authed")
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	if ok, _ := runtimeAuthState("claude"); !ok {
		t.Fatal("ANTHROPIC_API_KEY set: want authed")
	}
}

func TestCodexAuthState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("OPENAI_API_KEY", "")
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if ok, hint := runtimeAuthState("codex"); ok || hint == "" {
		t.Fatalf("no auth.json: want not-authed with hint, got ok=%v hint=%q", ok, hint)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(`{"tokens":{"access_token":"abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := runtimeAuthState("codex"); !ok {
		t.Fatal("access_token present: want authed")
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(`{"tokens":{"access_token":""}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := runtimeAuthState("codex"); ok {
		t.Fatal("empty access_token: want not-authed")
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(`{"OPENAI_API_KEY":"sk-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := runtimeAuthState("codex"); !ok {
		t.Fatal("OPENAI_API_KEY in auth.json: want authed")
	}
}

func TestCodexAuthStateRespectsCODEXHomeOutsideSandbox(t *testing.T) {
	home := t.TempDir()
	customCodexHome := filepath.Join(t.TempDir(), "custom-codex")
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", customCodexHome)
	t.Setenv("OPENAI_API_KEY", "")
	if err := os.MkdirAll(customCodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customCodexHome, "auth.json"), []byte(`{"tokens":{"access_token":"abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if ok, _ := runtimeAuthState("codex"); !ok {
		t.Fatal("normal codex auth should respect CODEX_HOME")
	}
	if ok, _ := runtimeFileAuthState("codex"); ok {
		t.Fatal("file-backed sandbox codex auth must ignore CODEX_HOME")
	}
}

func TestRuntimeFileAuthStateIsStrictForDockerReadiness(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, hint := runtimeAuthState("claude"); !ok || hint != "" {
		t.Fatalf("normal malformed Claude auth should not nag, got ok=%v hint=%q", ok, hint)
	}
	if ok, hint := runtimeFileAuthState("claude"); ok || hint == "" {
		t.Fatalf("strict malformed Claude auth should not count, got ok=%v hint=%q", ok, hint)
	} else if !strings.Contains(hint, "claude") || !strings.Contains(hint, "/login") || strings.Contains(hint, "setup-token") {
		t.Fatalf("strict Docker Claude hint should point at persistent login only, got %q", hint)
	}
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, hint := runtimeAuthState("claude"); !ok || hint != "" {
		t.Fatalf("normal empty Claude oauthAccount should remain lenient, got ok=%v hint=%q", ok, hint)
	}
	if ok, hint := runtimeFileAuthState("claude"); ok || hint == "" {
		t.Fatalf("strict empty Claude oauthAccount should not count, got ok=%v hint=%q", ok, hint)
	}
	credentialsDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(credentialsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialsDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := runtimeFileAuthState("claude"); !ok {
		t.Fatal("strict Claude auth should accept a valid alternate credentials file")
	}

	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, hint := runtimeAuthState("codex"); !ok || hint != "" {
		t.Fatalf("normal malformed Codex auth should not nag, got ok=%v hint=%q", ok, hint)
	}
	if ok, hint := runtimeFileAuthState("codex"); ok || hint == "" {
		t.Fatalf("strict malformed Codex auth should not count, got ok=%v hint=%q", ok, hint)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "auth.json"), []byte(`{"tokens":{"access_token":"   "},"OPENAI_API_KEY":"\t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, hint := runtimeFileAuthState("codex"); ok || hint == "" {
		t.Fatalf("strict whitespace-only Codex auth should not count, got ok=%v hint=%q", ok, hint)
	}
}

func TestRuntimeAuthHeaderValueIgnoresEnvKeysInsideDockerSandbox(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	customCodexHome := filepath.Join(t.TempDir(), "custom-codex")
	t.Setenv("CODEX_HOME", customCodexHome)
	t.Setenv(dockerRuntimeSandboxEnv, "1")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	if err := os.MkdirAll(customCodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customCodexHome, "auth.json"), []byte(`{"tokens":{"access_token":"abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := runtimeAuthHeaderValue(); got != "" {
		t.Fatalf("runtimeAuthHeaderValue() in sandbox = %q, want env keys and CODEX_HOME ignored", got)
	}
}

func TestCheckRuntimeOnPATHAcceptsEitherRuntime(t *testing.T) {
	withRuntimeLookPath(t, lookPathSet("codex"))
	if c := checkRuntimeOnPATH(); c.Status != "ok" {
		t.Fatalf("codex only: want ok, got %q (%s)", c.Status, c.Message)
	}
	withRuntimeLookPath(t, lookPathSet("claude"))
	if c := checkRuntimeOnPATH(); c.Status != "ok" {
		t.Fatalf("claude only: want ok, got %q (%s)", c.Status, c.Message)
	}
	withRuntimeLookPath(t, lookPathSet())
	if c := checkRuntimeOnPATH(); c.Status != "error" {
		t.Fatalf("neither: want error, got %q", c.Status)
	}
}

func TestCheckRuntimeAuthWarnsForInstalledButLoggedOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	// Claude installed but no config → warn.
	withRuntimeLookPath(t, lookPathSet("claude"))
	if c := checkRuntimeAuth(); c.Status != "warn" {
		t.Fatalf("installed + logged out: want warn, got %q (%s)", c.Status, c.Message)
	}
	// Neither installed → nothing to nag about → ok.
	withRuntimeLookPath(t, lookPathSet())
	if c := checkRuntimeAuth(); c.Status != "ok" {
		t.Fatalf("nothing installed: want ok, got %q", c.Status)
	}
}

func TestCheckRuntimeAuthUsesFileBackedAuthInsideDockerSandbox(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("COMMENT_IO_AGENT_SANDBOX", "1")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	withRuntimeLookPath(t, lookPathSet("claude"))

	if c := checkRuntimeAuth(); c.Status != "warn" {
		t.Fatalf("env-only sandbox Claude auth: want warn, got %q (%s)", c.Status, c.Message)
	}

	credentialsDir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(credentialsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credentialsDir, ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := checkRuntimeAuth(); c.Status != "ok" {
		t.Fatalf("file-backed sandbox Claude auth: want ok, got %q (%s)", c.Status, c.Message)
	}
}

func TestCheckRuntimeAuthIgnoresCodexHomeInsideDockerSandbox(t *testing.T) {
	home := t.TempDir()
	codexHome := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("COMMENT_IO_AGENT_SANDBOX", "1")
	t.Setenv("OPENAI_API_KEY", "")
	withRuntimeLookPath(t, lookPathSet("codex"))

	if err := os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(`{"tokens":{"access_token":"abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := checkRuntimeAuth(); c.Status != "warn" {
		t.Fatalf("sandbox Codex auth in CODEX_HOME: want warn, got %q (%s)", c.Status, c.Message)
	}

	defaultCodexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(defaultCodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultCodexHome, "auth.json"), []byte(`{"tokens":{"access_token":"abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := checkRuntimeAuth(); c.Status != "ok" {
		t.Fatalf("sandbox Codex auth in default home: want ok, got %q (%s)", c.Status, c.Message)
	}
}
