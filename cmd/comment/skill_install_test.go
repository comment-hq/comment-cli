//go:build darwin || linux

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clearSkillDetectEnv neutralizes the agent-detection env vars so the tests see
// only the directories under the temp home (not the CI runner's real agents).
func clearSkillDetectEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv("CODEX_HOME", "")
}

func TestSkillInstallWritesSkillToDetectedAgentDir(t *testing.T) {
	clearSkillDetectEnv(t)
	home := t.TempDir()
	// Make this look like a Claude Code machine.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/comment.SKILL.md" {
			_, _ = w.Write([]byte("---\nname: comment\n---\nUNIVERSAL SKILL BODY"))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	out, err := captureRun(t, []string{"skill", "install", "--home", home, "--base-url", server.URL})
	if err != nil {
		t.Fatalf("skill install failed: %v", err)
	}
	dst := filepath.Join(home, ".claude", "skills", "comment", "SKILL.md")
	body, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("expected SKILL.md at %s: %v", dst, readErr)
	}
	if !strings.Contains(string(body), "UNIVERSAL SKILL BODY") {
		t.Fatalf("SKILL.md missing universal body: %q", string(body))
	}
	if !strings.Contains(out, "\"ok\": true") {
		t.Fatalf("expected ok:true in JSON output: %q", out)
	}
	if !strings.Contains(out, dst) {
		t.Fatalf("expected installed path %s in output: %q", dst, out)
	}
}

func TestSkillInstallFallsBackToAgentsMD(t *testing.T) {
	clearSkillDetectEnv(t)
	home := t.TempDir() // no agent directories
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		switch r.URL.Path {
		case "/comment.SKILL.md":
			_, _ = w.Write([]byte("skill"))
		case "/agents.md":
			_, _ = w.Write([]byte("# Comment.io\nAGENTS FALLBACK BODY"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := captureRun(t, []string{"skill", "install", "--home", home, "--base-url", server.URL}); err != nil {
		t.Fatalf("skill install failed: %v", err)
	}
	dst := filepath.Join(home, ".agents", "AGENTS.md")
	body, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("expected AGENTS.md fallback at %s: %v", dst, readErr)
	}
	if !strings.Contains(string(body), "AGENTS FALLBACK BODY") {
		t.Fatalf("AGENTS.md missing fallback body: %q", string(body))
	}
	// The no-agent path must NOT fetch the SKILL.md (only /agents.md).
	for _, p := range requested {
		if p == "/comment.SKILL.md" {
			t.Fatalf("SKILL.md should not be fetched when no agent dir exists; requested=%v", requested)
		}
	}
}

func TestSkillInstallAppendsToExistingAgentsMD(t *testing.T) {
	clearSkillDetectEnv(t)
	home := t.TempDir() // no agent directories
	// The user already keeps global AGENTS instructions here.
	agentsPath := filepath.Join(home, ".agents", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(agentsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const userContent = "# My own agent instructions\nDo not delete me.\n"
	if err := os.WriteFile(agentsPath, []byte(userContent), 0o644); err != nil {
		t.Fatal(err)
	}
	fallbackBody := agentsFallbackMarker + "\n# Comment.io\nAGENTS FALLBACK BODY"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agents.md" {
			_, _ = w.Write([]byte(fallbackBody))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	if _, err := captureRun(t, []string{"skill", "install", "--home", home, "--base-url", server.URL}); err != nil {
		t.Fatalf("skill install failed: %v", err)
	}
	got, readErr := os.ReadFile(agentsPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(got), "Do not delete me.") {
		t.Fatalf("user's existing AGENTS.md content was clobbered: %q", string(got))
	}
	if !strings.Contains(string(got), "AGENTS FALLBACK BODY") {
		t.Fatalf("Comment.io fallback was not appended: %q", string(got))
	}
}

func TestSkillInstallSkipsAlreadyMarkedAgentsMD(t *testing.T) {
	clearSkillDetectEnv(t)
	home := t.TempDir() // no agent directories
	agentsPath := filepath.Join(home, ".agents", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(agentsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Already contains our marker — a re-run must not duplicate or refetch.
	existing := agentsFallbackMarker + "\n# Comment.io\nfirst install\n"
	if err := os.WriteFile(agentsPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	var requested []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = append(requested, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()

	if _, err := captureRun(t, []string{"skill", "install", "--home", home, "--base-url", server.URL}); err != nil {
		t.Fatalf("skill install failed: %v", err)
	}
	got, readErr := os.ReadFile(agentsPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != existing {
		t.Fatalf("already-marked AGENTS.md should be left untouched; got: %q", string(got))
	}
	for _, p := range requested {
		if p == "/agents.md" {
			t.Fatalf("should not refetch /agents.md when the marker is already present; requested=%v", requested)
		}
	}
}

func TestSkillInstallUsesOpenClawSkillForOpenClaw(t *testing.T) {
	clearSkillDetectEnv(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".openclaw"), 0o755); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openclaw.SKILL.md":
			_, _ = w.Write([]byte("OPENCLAW CHANNEL SKILL"))
		case "/comment.SKILL.md":
			_, _ = w.Write([]byte("GENERIC SKILL"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := captureRun(t, []string{"skill", "install", "--home", home, "--base-url", server.URL}); err != nil {
		t.Fatalf("skill install failed: %v", err)
	}
	dst := filepath.Join(home, ".openclaw", "skills", "comment", "SKILL.md")
	body, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("expected OpenClaw SKILL.md at %s: %v", dst, readErr)
	}
	if !strings.Contains(string(body), "OPENCLAW CHANNEL SKILL") {
		t.Fatalf("OpenClaw should receive its channel-specific skill, got: %q", string(body))
	}
}

func TestSkillUsageOnUnknownSubcommand(t *testing.T) {
	if err := runSkill([]string{"bogus"}); err == nil {
		t.Fatal("expected an error for an unknown skill subcommand")
	}
}
