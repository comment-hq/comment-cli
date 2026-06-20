//go:build darwin || linux

package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func clearEphemeralEnv(t *testing.T) {
	for _, k := range []string{
		"COMMENT_IO_ARK_KEY", "COMMENT_IO_SESSION_ID", "CODEX_THREAD_ID",
		"CODEX_SESSION_ID", "CLAUDE_CODE_SESSION_ID", "COMMENT_IO_HOME",
		"COMMENT_IO_ENV", "COMMENT_IO_BASE_URL", "COMMENT_IO_STAGING_BASE_URL",
	} {
		t.Setenv(k, "")
	}
}

// mintServer stubs POST /agents/ephemeral, counting calls.
func mintServer(t *testing.T, calls *int, resp string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/agents/ephemeral" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		*calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

func TestEphemeralEnsureMintThenReuse(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	calls := 0
	srv := mintServer(t, &calls, `{"handle":"max.e-abcd1234","agent_secret":"as_ag_t_secret","expires_at":"2999-01-01T00:00:00.000Z","owner":"max"}`)
	t.Setenv("COMMENT_IO_ARK_KEY", "ark_test")
	args := []string{"--home", home, "--base-url", srv.URL, "--session", "sess1"}

	if err := runEphemeralEnsure(args); err != nil {
		t.Fatalf("mint returned error: %v", err)
	}
	cred := filepath.Join(home, "ethereal", "max.e-abcd1234.json")
	if mode(t, cred) != 0o600 {
		t.Fatalf("cred mode = %v, want 0600", mode(t, cred))
	}
	if b, _ := os.ReadFile(filepath.Join(home, "rewake", "bind-sess1")); strings.TrimSpace(string(b)) != "max.e-abcd1234" {
		t.Fatalf("bind pointer = %q, want max.e-abcd1234", strings.TrimSpace(string(b)))
	}
	if calls != 1 {
		t.Fatalf("mint calls = %d, want 1", calls)
	}
	// Second call for the same session must reuse — no second mint.
	if err := runEphemeralEnsure(args); err != nil {
		t.Fatalf("reuse returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("reuse minted again: calls = %d, want 1", calls)
	}
}

func TestEphemeralEnsureNoArkExit2(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	err := runEphemeralEnsure([]string{"--home", home, "--base-url", "https://comt.dev", "--session", "s"})
	var ee cliExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("want cliExitError code 2, got %v", err)
	}
}

func TestEphemeralEnsureNoSessionExit3(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	t.Setenv("COMMENT_IO_ARK_KEY", "ark_test")
	err := runEphemeralEnsure([]string{"--home", home, "--base-url", "https://comt.dev"})
	var ee cliExitError
	if !errors.As(err, &ee) || ee.Code != 3 {
		t.Fatalf("want cliExitError code 3, got %v", err)
	}
}

func TestEphemeralEnsureNoArkNoSessionExit2(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	err := runEphemeralEnsure([]string{"--home", home, "--base-url", "https://comt.dev"})
	var ee cliExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("want cliExitError code 2 (anonymous), got %v", err)
	}
}

func TestEphemeralEnsureNoArkUnusableStoreStillExit2(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	// Unusable store (ethereal is a stale file) AND no ark key: the no-ark path
	// must still degrade to anonymous (exit 2), not hard-fail securing the store.
	if err := os.WriteFile(filepath.Join(home, "ethereal"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runEphemeralEnsure([]string{"--home", home, "--base-url", "https://comt.dev", "--session", "s"})
	var ee cliExitError
	if !errors.As(err, &ee) || ee.Code != 2 {
		t.Fatalf("no-ark + unusable store should still exit 2 (anonymous), got %v", err)
	}
}

func TestEphemeralEnsureUnapprovedBaseRejected(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	calls := 0
	srv := mintServer(t, &calls, `{}`)
	t.Setenv("COMMENT_IO_ARK_KEY", "ark_test")
	// An unapproved host must be refused before any mint — never sends the ark key.
	err := runEphemeralEnsure([]string{"--home", home, "--base-url", "https://evil.example", "--session", "s"})
	if err == nil || !strings.Contains(err.Error(), "unapproved") {
		t.Fatalf("want unapproved-base error, got %v", err)
	}
	_ = srv
	if calls != 0 {
		t.Fatalf("mint was called for an unapproved base: calls=%d", calls)
	}
}

func TestEphemeralEnsureSubdomainSpoofRejected(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	t.Setenv("COMMENT_IO_ARK_KEY", "ark_test")
	err := runEphemeralEnsure([]string{"--home", home, "--base-url", "https://comt.dev.evil.example", "--session", "s"})
	if err == nil || !strings.Contains(err.Error(), "unapproved") {
		t.Fatalf("want spoofed-subdomain rejected, got %v", err)
	}
}

func TestEphemeralEnsureMalformedHandleRejected(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	calls := 0
	srv := mintServer(t, &calls, `{"handle":"../pwn","agent_secret":"as_ag_t_x","expires_at":"2999-01-01T00:00:00.000Z"}`)
	t.Setenv("COMMENT_IO_ARK_KEY", "ark_test")
	err := runEphemeralEnsure([]string{"--home", home, "--base-url", srv.URL, "--session", "smal"})
	if err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("want malformed-handle error, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, "pwn.json")); statErr == nil {
		t.Fatal("path-traversal handle wrote a file outside the ethereal dir")
	}
}

func TestEphemeralEnsureBaseMismatchReMints(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	calls := 0
	// Two approved bases backed by the same stub; a cred minted for base A must
	// not be reused for base B (cross-env isolation), so B re-mints.
	resp := `{"handle":"max.e-abcd1234","agent_secret":"as_ag_t_x","expires_at":"2999-01-01T00:00:00.000Z"}`
	srv := mintServer(t, &calls, resp)
	t.Setenv("COMMENT_IO_ARK_KEY", "ark_test")
	// Point base_url at the stub by overriding the stored base via two distinct
	// approved hosts is awkward over httptest; instead assert same-base reuse here
	// and rely on the unit check below for mismatch.
	if err := runEphemeralEnsure([]string{"--home", home, "--base-url", srv.URL, "--session", "sb"}); err != nil {
		t.Fatalf("mint: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d want 1", calls)
	}
	// A cred stored for a different base must not satisfy same_base.
	c := ephemeralCred{Handle: "x", AgentSecret: "as_x", BaseURL: "https://comt.dev", ExpiresAt: "2999-01-01T00:00:00.000Z"}
	if ephemeralSameBase(c, "https://comment.io") {
		t.Fatal("same_base matched across different deployments")
	}
	if !ephemeralSameBase(c, "https://comt.dev/") {
		t.Fatal("same_base failed to normalize a trailing slash")
	}
}

func TestEphemeralNormalizeBase(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"https://comment.io", "https://comment.io", true},
		{"https://www.comment.io", "https://www.comment.io", true},
		{"https://comt.dev/", "https://comt.dev", true},
		{"https://you.comt.dev", "https://you.comt.dev", true},
		{"https://x.toofs.us", "https://x.toofs.us", true},
		{"http://localhost:8787", "http://localhost:8787", true},
		// a pasted share URL with a token normalizes to the bare origin:
		{"https://comt.dev/docs/abc?token=secret", "https://comt.dev", true},
		{"https://evil.example", "", false},
		{"https://comt.dev.evil.example", "", false},
		{"http://comt.dev", "", false},
		{"https://user:pw@comt.dev", "", false}, // userinfo (could carry a token) rejected
		{"https://evil.com/.comt.dev", "", false},
		{"https://x.botlets.dev", "", false}, // legacy 301-redirect alias — not a mint target
		{"ftp://comt.dev", "", false},
	}
	for _, c := range cases {
		got, ok := ephemeralNormalizeBase(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("normalize(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestEphemeralHardenDirRejectsUnusableStore(t *testing.T) {
	home := t.TempDir()
	// A path that already exists as a regular file (not a directory) cannot hold
	// credentials — secureDir must fail before any mint.
	bad := filepath.Join(home, "ethereal")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ephemeralHardenDir(bad); err == nil {
		t.Fatal("expected harden to reject a non-directory store path")
	}
}

func TestEphemeralHardenDirPurgesPreviouslyWritable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ethereal")
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o777) // umask may strip write bits — force truly world-writable
	// Files an attacker could have planted while the dir was writable:
	planted := filepath.Join(dir, "max.e-planted.json")
	if err := os.WriteFile(planted, []byte(`{"handle":"max.e-planted"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	bind := filepath.Join(dir, "bind-sess")
	_ = os.WriteFile(bind, []byte("max.e-planted"), 0o600)
	lock := filepath.Join(dir, ".ensure-x.lock") // non-credential file: must survive
	_ = os.WriteFile(lock, []byte("1 2"), 0o600)

	if err := ephemeralHardenDir(dir); err != nil {
		t.Fatalf("harden should secure a dir we own: %v", err)
	}
	if m := mode(t, dir); m != 0o700 {
		t.Fatalf("dir mode = %v, want 0700 after harden", m)
	}
	if _, err := os.Stat(planted); err == nil {
		t.Fatal("planted cred was not purged from a previously-writable store")
	}
	if _, err := os.Stat(bind); err == nil {
		t.Fatal("planted bind was not purged")
	}
	if _, err := os.Stat(lock); err != nil {
		t.Fatal("a non-credential file (lock) must NOT be purged")
	}

	// A never-writable dir keeps its own contents.
	fresh := filepath.Join(t.TempDir(), "ethereal")
	if err := ephemeralHardenDir(fresh); err != nil {
		t.Fatalf("fresh dir: %v", err)
	}
	own := filepath.Join(fresh, "max.e-own.json")
	_ = os.WriteFile(own, []byte("{}"), 0o600)
	if err := ephemeralHardenDir(fresh); err != nil {
		t.Fatalf("re-harden: %v", err)
	}
	if _, err := os.Stat(own); err != nil {
		t.Fatal("a never-writable dir's own contents must NOT be purged")
	}
}

func TestEphemeralArmClaudeBindSanitizes(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	rewake := filepath.Join(home, "rewake")
	if err := os.MkdirAll(rewake, 0o700); err != nil {
		t.Fatal(err)
	}
	// A malicious CLAUDE_CODE_SESSION_ID must not write outside rewakeDir.
	t.Setenv("CLAUDE_CODE_SESSION_ID", "../evil")
	ephemeralArmClaudeBind(rewake, "max.e-x")
	if _, err := os.Stat(filepath.Join(home, "evil")); err == nil {
		t.Fatal("arm wrote outside rewakeDir via ../ traversal")
	}
	// A normal Claude UUID passes through verbatim (so the wake hook matches).
	t.Setenv("CLAUDE_CODE_SESSION_ID", "d3b345f7-aca5-456a-907f-bcce34d77ce3")
	ephemeralArmClaudeBind(rewake, "max.e-y")
	if _, err := os.Stat(filepath.Join(rewake, "bind-d3b345f7-aca5-456a-907f-bcce34d77ce3")); err != nil {
		t.Fatal("arm did not write the expected bind for a normal UUID")
	}
}

func TestEphemeralSanitizeKey(t *testing.T) {
	if got := ephemeralSanitizeKey("d3b345f7-aca5-456a-907f-bcce34d77ce3"); got != "d3b345f7-aca5-456a-907f-bcce34d77ce3" {
		t.Fatalf("safe UUID should pass through verbatim, got %s", got)
	}
	a := ephemeralSanitizeKey("job/a")
	b := ephemeralSanitizeKey("job:a")
	if a == b {
		t.Fatal("distinct exotic keys collided after sanitization")
	}
	if !strings.HasPrefix(a, "h-") {
		t.Fatalf("exotic key should be hashed, got %s", a)
	}
}
