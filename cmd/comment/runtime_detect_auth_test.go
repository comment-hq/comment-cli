package main

import (
	"errors"
	"os"
	"path/filepath"
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
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"oauthAccount":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, _ := runtimeAuthState("claude"); ok {
		t.Fatal("oauthAccount null: want not-authed")
	}
	if err := os.Remove(filepath.Join(home, ".claude.json")); err != nil {
		t.Fatal(err)
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
