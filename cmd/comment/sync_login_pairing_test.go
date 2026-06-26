//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

func TestSyncStatusExplainsPairingProvisioning(t *testing.T) {
	output, err := captureRun(t, []string{"sync", "status", "--home", t.TempDir()})
	if err != nil {
		t.Fatalf("sync status failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "Library sync is not configured.") ||
		!strings.Contains(output, "no second browser approval") ||
		!strings.Contains(output, "already paired to the same Comment.io origin") {
		t.Fatalf("sync status output did not explain pairing-based provisioning:\n%s", output)
	}
}

func TestSyncLoginUsesPairingCredentialWithoutBrowserApproval(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_synctest.key1.secret1"
	var syncCredentialCalls int
	var deviceCodeCalls int
	var snapshotCalls int
	var activateCalls int

	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/sync-credential", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("sync-credential method = %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testEnrollmentDaemonToken {
			t.Errorf("sync-credential Authorization = %q", got)
		}
		syncCredentialCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"api_key": key,
			"scope":   "library-sync:read:botlets-brains",
		})
	})
	mux.HandleFunc("/auth/library-sync/device-codes", func(w http.ResponseWriter, r *http.Request) {
		deviceCodeCalls++
		http.Error(w, "device login should not be used for same-origin pairing", http.StatusInternalServerError)
	})
	mux.HandleFunc("/auth/library-sync/snapshot", func(w http.ResponseWriter, r *http.Request) {
		snapshotCalls++
		if got := r.Header.Get("Authorization"); got != "Bearer "+key {
			t.Errorf("snapshot Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"snapshotId":          "lse_login_validate",
			"scopeLabel":          "My Files, Shared With Me, Team Wiki, and Botlets brains",
			"coveredSections":     []map[string]any{{"id": "botlets-brains", "label": "Botlets brains", "covered": true, "authoritative": true, "count": 0}},
			"unsupportedSections": []map[string]any{},
			"snapshotComplete":    true,
			"rows":                []map[string]any{},
			"pageInfo":            map[string]any{"nextCursor": nil, "partial": false},
		})
	})
	mux.HandleFunc("/auth/library-sync/current-device/activate", func(w http.ResponseWriter, r *http.Request) {
		activateCalls++
		if r.Method != http.MethodPost {
			t.Errorf("activate method = %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+key {
			t.Errorf("activate Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	if err := commentbus.SaveDaemonAuth(paths, testDaemonAuthFor(server.URL)); err != nil {
		t.Fatalf("SaveDaemonAuth = %v", err)
	}

	output, stderr, err := captureRunWithStderr(t, []string{"sync", "login", "--home", paths.Home, "--root", root, "--base-url", server.URL})
	if err != nil {
		t.Fatalf("sync login failed: %v\n%s", err, output)
	}
	if syncCredentialCalls != 1 {
		t.Fatalf("sync-credential calls = %d, want 1", syncCredentialCalls)
	}
	if deviceCodeCalls != 0 {
		t.Fatalf("device-code calls = %d, want 0 for same-origin paired sync login", deviceCodeCalls)
	}
	if snapshotCalls != 1 || activateCalls != 1 {
		t.Fatalf("snapshot/activate calls = %d/%d, want 1/1", snapshotCalls, activateCalls)
	}
	if !strings.Contains(output, `"configured": true`) || !strings.Contains(output, `"base_url": "`+server.URL+`"`) {
		t.Fatalf("sync login output did not report configured sync for this origin:\n%s", output)
	}
	if strings.Contains(stderr, "Open this URL to approve local sync") {
		t.Fatalf("sync login should not print browser approval guidance when paired to the same origin:\n%s", stderr)
	}
	if !strings.Contains(stderr, "no browser approval needed") {
		t.Fatalf("sync login stderr did not explain pairing provisioning:\n%s", stderr)
	}
}

func captureRunWithStderr(t *testing.T, args []string) (string, string, error) {
	t.Helper()
	oldStdout := os.Stdout
	oldStderr := os.Stderr
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = stdoutWriter
	os.Stderr = stderrWriter
	runErr := run(args)
	if err := stdoutWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stderrWriter.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = oldStdout
	os.Stderr = oldStderr
	stdout, err := io.ReadAll(stdoutReader)
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := io.ReadAll(stderrReader)
	if err != nil {
		t.Fatal(err)
	}
	return string(stdout), string(stderr), runErr
}

// `comment sync login` rides on an existing pairing: when the computer is paired
// to the same origin, the key is minted over the daemon token (no browser).
func TestSyncKeyViaPairingMintsWhenPairedToSameOrigin(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	calls := 0
	server := newSyncProvisionTestServer(t, &calls)
	if err := commentbus.SaveDaemonAuth(paths, testDaemonAuthFor(server.URL)); err != nil {
		t.Fatalf("SaveDaemonAuth = %v", err)
	}

	key, err := syncKeyViaPairing(context.Background(), paths.Home, server.URL)
	if err != nil {
		t.Fatalf("syncKeyViaPairing err = %v, want nil (paired to same origin)", err)
	}
	if key == "" {
		t.Fatalf("syncKeyViaPairing returned empty key, want a minted key")
	}
	if calls != 1 {
		t.Fatalf("sync-credential mint calls = %d, want 1", calls)
	}
}

// Unpaired computer: no daemon mint, no error — the caller falls back to the
// browser device flow.
func TestSyncKeyViaPairingSkipsWhenUnpaired(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	calls := 0
	server := newSyncProvisionTestServer(t, &calls)

	key, err := syncKeyViaPairing(context.Background(), paths.Home, server.URL)
	if err != nil || key != "" {
		t.Fatalf("syncKeyViaPairing = (%q, %v), want (\"\", nil) when unpaired", key, err)
	}
	if calls != 0 {
		t.Fatalf("sync-credential mint calls = %d, want 0 (must not mint when unpaired)", calls)
	}
}

// Paired to a DIFFERENT origin: the pairing token can't provision this origin, so
// the daemon path is skipped (one home syncs one origin) with no error and the
// caller falls back to the browser device flow.
func TestSyncKeyViaPairingSkipsOnOriginMismatch(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	calls := 0
	server := newSyncProvisionTestServer(t, &calls)
	if err := commentbus.SaveDaemonAuth(paths, testDaemonAuthFor(server.URL)); err != nil {
		t.Fatalf("SaveDaemonAuth = %v", err)
	}

	key, err := syncKeyViaPairing(context.Background(), paths.Home, "https://other.example.test")
	if err != nil || key != "" {
		t.Fatalf("syncKeyViaPairing = (%q, %v), want (\"\", nil) on origin mismatch", key, err)
	}
	if calls != 0 {
		t.Fatalf("sync-credential mint calls = %d, want 0 (must not mint for a different origin)", calls)
	}
}

// Without --home, the pairing lookup must honor COMMENT_IO_HOME (the same home
// commentsync.Login writes to) — not the default home. Otherwise a multi-home
// setup could mint from the wrong account's pairing.
func TestSyncKeyViaPairingHonorsCommentIOHome(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	calls := 0
	server := newSyncProvisionTestServer(t, &calls)
	if err := commentbus.SaveDaemonAuth(paths, testDaemonAuthFor(server.URL)); err != nil {
		t.Fatalf("SaveDaemonAuth = %v", err)
	}
	t.Setenv("COMMENT_IO_HOME", paths.Home)

	// home="" must resolve to COMMENT_IO_HOME and find that home's pairing.
	key, err := syncKeyViaPairing(context.Background(), "", server.URL)
	if err != nil {
		t.Fatalf("syncKeyViaPairing err = %v, want nil (COMMENT_IO_HOME pairing)", err)
	}
	if key == "" || calls != 1 {
		t.Fatalf("expected a mint via the COMMENT_IO_HOME pairing (key=%q calls=%d)", key, calls)
	}
}

// Sync disabled on the pairing (409 CAPABILITY_DISABLED): a definitive,
// actionable state — return an error so the caller surfaces "re-enable sync"
// rather than silently minting an unrelated standalone key via the browser flow.
func TestSyncKeyViaPairingErrorsWhenSyncDisabled(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/sync-credential", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"Sync is disabled on this computer","code":"CAPABILITY_DISABLED","capability":"sync"}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	if err := commentbus.SaveDaemonAuth(paths, testDaemonAuthFor(server.URL)); err != nil {
		t.Fatalf("SaveDaemonAuth = %v", err)
	}

	key, err := syncKeyViaPairing(context.Background(), paths.Home, server.URL)
	if err == nil {
		t.Fatalf("syncKeyViaPairing err = nil, want a clear 'sync disabled' error")
	}
	if key != "" {
		t.Fatalf("syncKeyViaPairing key = %q, want empty on sync-disabled", key)
	}
}

// A 409 from a REPLACED pairing (DAEMON_REPLACED) is NOT "sync disabled" — it's
// an inapplicable/stale pairing, so the caller must fall back to the browser
// device flow (no error), not report sync as turned off.
func TestSyncKeyViaPairingFallsBackOnReplacedPairing(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/sync-credential", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"Daemon was replaced by a newer pairing","code":"DAEMON_REPLACED"}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	if err := commentbus.SaveDaemonAuth(paths, testDaemonAuthFor(server.URL)); err != nil {
		t.Fatalf("SaveDaemonAuth = %v", err)
	}

	key, err := syncKeyViaPairing(context.Background(), paths.Home, server.URL)
	if err != nil || key != "" {
		t.Fatalf("syncKeyViaPairing = (%q, %v), want (\"\", nil) so the caller falls back on a replaced pairing", key, err)
	}
}
