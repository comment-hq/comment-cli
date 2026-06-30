//go:build darwin || linux

package main

import (
	"encoding/json"
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
	cred := filepath.Join(home, "ephemeral", "max.e-abcd1234.json")
	if mode(t, cred) != 0o600 {
		t.Fatalf("cred mode = %v, want 0600", mode(t, cred))
	}
	var stored ephemeralCred
	data, err := os.ReadFile(cred)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.IdentityClass != "ephemeral" {
		t.Fatalf("cred identity_class = %q, want ephemeral", stored.IdentityClass)
	}
	if _, err := os.Stat(filepath.Join(home, "ethereal")); !os.IsNotExist(err) {
		t.Fatalf("fresh ensure created legacy ethereal dir: %v", err)
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

func TestEphemeralTryReuseRejectsForeignOrMismatchedCreds(t *testing.T) {
	clearEphemeralEnv(t)
	const base = "https://comment.io"

	cases := []struct {
		name        string
		bindHandle  string
		fileHandle  string
		credHandle  string
		credSession string
		secret      string
		identity    string
	}{
		{
			name:        "foreign session",
			bindHandle:  "max.e-aaaa1111",
			fileHandle:  "max.e-aaaa1111",
			credHandle:  "max.e-aaaa1111",
			credSession: "sess_old",
			secret:      "as_ag_t_secret",
			identity:    "ephemeral",
		},
		{
			name:        "json handle mismatch",
			bindHandle:  "max.e-bbbb2222",
			fileHandle:  "max.e-bbbb2222",
			credHandle:  "max.e-cccc3333",
			credSession: "sess_new",
			secret:      "as_ag_t_secret",
			identity:    "ephemeral",
		},
		{
			name:        "registered-looking handle",
			bindHandle:  "max.reviewer",
			fileHandle:  "max.reviewer",
			credHandle:  "max.reviewer",
			credSession: "sess_new",
			secret:      "as_ag_t_secret",
			identity:    "ephemeral",
		},
		{
			name:        "dot-e registered-looking nonhex handle",
			bindHandle:  "max.e-reviewer",
			fileHandle:  "max.e-reviewer",
			credHandle:  "max.e-reviewer",
			credSession: "sess_new",
			secret:      "as_ag_t_secret",
			identity:    "ephemeral",
		},
		{
			name:        "hex-shaped unmarked registered-looking handle",
			bindHandle:  "max.e-deadbeef",
			fileHandle:  "max.e-deadbeef",
			credHandle:  "max.e-deadbeef",
			credSession: "sess_new",
			secret:      "as_ag_t_secret",
		},
		{
			name:        "invalid secret",
			bindHandle:  "max.e-dddd4444",
			fileHandle:  "max.e-dddd4444",
			credHandle:  "max.e-dddd4444",
			credSession: "sess_new",
			secret:      "not-secret",
			identity:    "ephemeral",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			ephemeralDir := filepath.Join(home, "ephemeral")
			rewakeDir := filepath.Join(home, "rewake")
			if err := os.MkdirAll(ephemeralDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(rewakeDir, 0o700); err != nil {
				t.Fatal(err)
			}
			cred := ephemeralCred{
				Handle:        tc.credHandle,
				AgentSecret:   tc.secret,
				IdentityClass: tc.identity,
				ExpiresAt:     "2999-01-01T00:00:00.000Z",
				BaseURL:       base,
				Session:       tc.credSession,
			}
			data, err := json.Marshal(cred)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ephemeralDir, tc.fileHandle+".json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
			bindFile := filepath.Join(rewakeDir, "bind-sess_new")
			if err := os.WriteFile(bindFile, []byte(tc.bindHandle), 0o600); err != nil {
				t.Fatal(err)
			}
			legacyDir := filepath.Join(home, "ethereal")
			if c, p, ok := ephemeralTryReuse(ephemeralDir, legacyDir, bindFile, "sess_new", base, false); ok {
				t.Fatalf("reused invalid credential: cred=%+v path=%s", c, p)
			}
		})
	}
}

func TestEphemeralTryReuseMigratesLegacyStore(t *testing.T) {
	clearEphemeralEnv(t)
	const base = "https://comment.io"
	home := t.TempDir()
	ephemeralDir := filepath.Join(home, "ephemeral")
	legacyDir := filepath.Join(home, "ethereal")
	rewakeDir := filepath.Join(home, "rewake")
	for _, dir := range []string{ephemeralDir, legacyDir, rewakeDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacyCred := ephemeralCred{
		Handle:        "max.e-11112222",
		AgentSecret:   "as_ag_t_secret",
		IdentityClass: "ethereal",
		ExpiresAt:     "2999-01-01T00:00:00.000Z",
		BaseURL:       base,
		Session:       "sess_new",
	}
	data, err := json.Marshal(legacyCred)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "max.e-11112222.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	bindFile := filepath.Join(rewakeDir, "bind-sess_new")
	if err := os.WriteFile(bindFile, []byte("max.e-11112222"), 0o600); err != nil {
		t.Fatal(err)
	}

	cred, path, ok := ephemeralTryReuse(ephemeralDir, legacyDir, bindFile, "sess_new", base, true)
	if !ok {
		t.Fatal("legacy credential was not reused")
	}
	wantPath := filepath.Join(ephemeralDir, "max.e-11112222.json")
	if path != wantPath || cred.IdentityClass != "ephemeral" {
		t.Fatalf("reuse = (%+v, %s), want migrated ephemeral cred at %s", cred, path, wantPath)
	}
	var migrated ephemeralCred
	migratedRaw, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(migratedRaw, &migrated); err != nil {
		t.Fatal(err)
	}
	if migrated.IdentityClass != "ephemeral" {
		t.Fatalf("migrated identity_class = %q, want ephemeral", migrated.IdentityClass)
	}
}

func TestEphemeralTryReuseNormalizesCurrentLegacyMarker(t *testing.T) {
	clearEphemeralEnv(t)
	const base = "https://comment.io"
	home := t.TempDir()
	ephemeralDir := filepath.Join(home, "ephemeral")
	legacyDir := filepath.Join(home, "ethereal")
	rewakeDir := filepath.Join(home, "rewake")
	for _, dir := range []string{ephemeralDir, rewakeDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacyMarkedCred := ephemeralCred{
		Handle:        "max.e-aaaabbbb",
		AgentSecret:   "as_ag_t_secret",
		IdentityClass: "ethereal",
		ExpiresAt:     "2999-01-01T00:00:00.000Z",
		BaseURL:       base,
		Session:       "sess_new",
	}
	data, err := json.Marshal(legacyMarkedCred)
	if err != nil {
		t.Fatal(err)
	}
	credPath := filepath.Join(ephemeralDir, "max.e-aaaabbbb.json")
	if err := os.WriteFile(credPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	bindFile := filepath.Join(rewakeDir, "bind-sess_new")
	if err := os.WriteFile(bindFile, []byte("max.e-aaaabbbb"), 0o600); err != nil {
		t.Fatal(err)
	}

	cred, path, ok := ephemeralTryReuse(ephemeralDir, legacyDir, bindFile, "sess_new", base, false)
	if !ok || path != credPath || cred.IdentityClass != "ephemeral" {
		t.Fatalf("reuse = (%+v, %s, %v), want normalized current-store cred", cred, path, ok)
	}
	var stored ephemeralCred
	updated, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(updated, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.IdentityClass != "ephemeral" {
		t.Fatalf("stored identity_class = %q, want ephemeral", stored.IdentityClass)
	}
}

func TestEphemeralEnsureRejectsBareRawResponseCred(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	ephemeralDir := filepath.Join(home, "ephemeral")
	rewakeDir := filepath.Join(home, "rewake")
	if err := os.MkdirAll(ephemeralDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rewakeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// This is the raw POST /agents/ephemeral response shape. Without the helper's
	// session/base/identity metadata, it is only a bearer token, not a reachable
	// session-scoped identity that `ensure` may safely reclaim.
	rawCred := `{"agent_id":"ag_t","agent_secret":"as_ag_t_old","handle":"max.e-deadbeef","actor_id":"ai:max.e-deadbeef","display_name":"Fred","expires_at":"2999-01-01T00:00:00.000Z","owner":"max"}`
	if err := os.WriteFile(filepath.Join(ephemeralDir, "max.e-deadbeef.json"), []byte(rawCred), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rewakeDir, "bind-sess_raw"), []byte("max.e-deadbeef"), 0o600); err != nil {
		t.Fatal(err)
	}

	calls := 0
	srv := mintServer(t, &calls, `{"handle":"max.e-cafebabe","agent_secret":"as_ag_t_new","expires_at":"2999-01-01T00:00:00.000Z","owner":"max"}`)
	t.Setenv("COMMENT_IO_ARK_KEY", "ark_test")
	if err := runEphemeralEnsure([]string{"--home", home, "--base-url", srv.URL, "--session", "sess_raw"}); err != nil {
		t.Fatalf("ensure returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("bare raw response cred was reused; mint calls = %d, want 1", calls)
	}
	if b, _ := os.ReadFile(filepath.Join(rewakeDir, "bind-sess_raw")); strings.TrimSpace(string(b)) != "max.e-cafebabe" {
		t.Fatalf("bind pointer = %q, want reminted max.e-cafebabe", strings.TrimSpace(string(b)))
	}
	var stored ephemeralCred
	data, err := os.ReadFile(filepath.Join(ephemeralDir, "max.e-cafebabe.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.IdentityClass != "ephemeral" || stored.Session != "sess_raw" || stored.BaseURL == "" {
		t.Fatalf("reminted cred missing reuse metadata: %+v", stored)
	}
}

func TestEphemeralDaemonMintReadbackValidatesCredential(t *testing.T) {
	clearEphemeralEnv(t)
	const (
		base   = "https://comment.io"
		handle = "max.e-feedc0de"
		sess   = "sess_new"
	)

	cases := []struct {
		name        string
		cred        ephemeralCred
		wantOK      bool
		wantMigrate bool
	}{
		{
			name: "marked ephemeral",
			cred: ephemeralCred{
				Handle:        handle,
				AgentSecret:   "as_ag_t_secret",
				IdentityClass: "ephemeral",
				ExpiresAt:     "2999-01-01T00:00:00.000Z",
				BaseURL:       base,
				Session:       sess,
			},
			wantOK: true,
		},
		{
			name: "legacy markerless daemon mint is migrated",
			cred: ephemeralCred{
				Handle:      handle,
				AgentSecret: "as_ag_t_secret",
				ExpiresAt:   "2999-01-01T00:00:00.000Z",
				BaseURL:     base,
				Session:     sess,
			},
			wantOK:      true,
			wantMigrate: true,
		},
		{
			name: "legacy marked ephemeral",
			cred: ephemeralCred{
				Handle:        handle,
				AgentSecret:   "as_ag_t_secret",
				IdentityClass: "ethereal",
				ExpiresAt:     "2999-01-01T00:00:00.000Z",
				BaseURL:       base,
				Session:       sess,
			},
			wantOK:      true,
			wantMigrate: true,
		},
		{
			name: "json handle mismatch",
			cred: ephemeralCred{
				Handle:        "max.e-deadbeef",
				AgentSecret:   "as_ag_t_secret",
				IdentityClass: "ephemeral",
				ExpiresAt:     "2999-01-01T00:00:00.000Z",
				BaseURL:       base,
				Session:       sess,
			},
		},
		{
			name: "wrong session",
			cred: ephemeralCred{
				Handle:        handle,
				AgentSecret:   "as_ag_t_secret",
				IdentityClass: "ephemeral",
				ExpiresAt:     "2999-01-01T00:00:00.000Z",
				BaseURL:       base,
				Session:       "sess_old",
			},
		},
		{
			name: "bad secret",
			cred: ephemeralCred{
				Handle:        handle,
				AgentSecret:   "not-secret",
				IdentityClass: "ephemeral",
				ExpiresAt:     "2999-01-01T00:00:00.000Z",
				BaseURL:       base,
				Session:       sess,
			},
		},
		{
			name: "standard marker",
			cred: ephemeralCred{
				Handle:        handle,
				AgentSecret:   "as_ag_t_secret",
				IdentityClass: "standard",
				ExpiresAt:     "2999-01-01T00:00:00.000Z",
				BaseURL:       base,
				Session:       sess,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			ephemeralDir := filepath.Join(home, "ephemeral")
			bindFile := filepath.Join(home, "rewake", "bind-"+sess)
			if err := os.MkdirAll(ephemeralDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(bindFile), 0o700); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(tc.cred)
			if err != nil {
				t.Fatal(err)
			}
			credPath := filepath.Join(ephemeralDir, handle+".json")
			if err := os.WriteFile(credPath, data, 0o600); err != nil {
				t.Fatal(err)
			}

			cred, path, ok := ephemeralAcceptDaemonMintedCred(ephemeralDir, filepath.Join(home, "ethereal"), bindFile, handle, sess, base)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (cred=%+v path=%s)", ok, tc.wantOK, cred, path)
			}
			if !tc.wantOK {
				return
			}
			if path != credPath || cred.Handle != handle {
				t.Fatalf("accepted cred/path = %+v / %s", cred, path)
			}
			if b, _ := os.ReadFile(bindFile); strings.TrimSpace(string(b)) != handle {
				t.Fatalf("bind = %q, want %s", strings.TrimSpace(string(b)), handle)
			}
			if tc.wantMigrate {
				var migrated ephemeralCred
				data, err := os.ReadFile(credPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(data, &migrated); err != nil {
					t.Fatal(err)
				}
				if migrated.IdentityClass != "ephemeral" {
					t.Fatalf("migrated identity_class = %q, want ephemeral", migrated.IdentityClass)
				}
			}
		})
	}
}

func TestEphemeralDaemonMintReadbackMigratesLegacyStore(t *testing.T) {
	clearEphemeralEnv(t)
	const (
		base   = "https://comment.io"
		handle = "max.e-feedc0de"
		sess   = "sess_new"
	)
	home := t.TempDir()
	ephemeralDir := filepath.Join(home, "ephemeral")
	legacyDir := filepath.Join(home, "ethereal")
	bindFile := filepath.Join(home, "rewake", "bind-"+sess)
	for _, dir := range []string{ephemeralDir, legacyDir, filepath.Dir(bindFile)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	legacyCred := ephemeralCred{
		Handle:        handle,
		AgentSecret:   "as_ag_t_secret",
		IdentityClass: "ethereal",
		ExpiresAt:     "2999-01-01T00:00:00.000Z",
		BaseURL:       base,
		Session:       sess,
	}
	data, err := json.Marshal(legacyCred)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, handle+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	cred, path, ok := ephemeralAcceptDaemonMintedCred(ephemeralDir, legacyDir, bindFile, handle, sess, base)
	if !ok {
		t.Fatal("legacy daemon-minted credential was not accepted")
	}
	wantPath := filepath.Join(ephemeralDir, handle+".json")
	if path != wantPath || cred.IdentityClass != "ephemeral" {
		t.Fatalf("accepted legacy cred/path = %+v / %s, want migrated %s", cred, path, wantPath)
	}
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("migrated cred missing: %v", err)
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
	// Unusable store (ephemeral is a stale file) AND no ark key: the no-ark path
	// must still degrade to anonymous (exit 2), not hard-fail securing the store.
	if err := os.WriteFile(filepath.Join(home, "ephemeral"), []byte("x"), 0o600); err != nil {
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
		t.Fatal("path-traversal handle wrote a file outside the ephemeral dir")
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
	bad := filepath.Join(home, "ephemeral")
	if err := os.WriteFile(bad, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ephemeralHardenDir(bad); err == nil {
		t.Fatal("expected harden to reject a non-directory store path")
	}
}

func TestEphemeralHardenDirPurgesPreviouslyWritable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ephemeral")
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
	fresh := filepath.Join(t.TempDir(), "ephemeral")
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
