//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
	"github.com/comment-hq/comment-cli/internal/commentsync"
)

// newSyncProvisionTestServer serves POST /daemon/sync-credential, recording
// each call. The key parses as usk_v2.<agentId>.<keyId>.<secret>.
func newSyncProvisionTestServer(t *testing.T, calls *int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/sync-credential", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("sync-credential method = %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testEnrollmentDaemonToken {
			t.Errorf("sync-credential Authorization = %q", got)
		}
		*calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"api_key": "usk_v2.ag_synctest.key1.secret1",
			"scope":   "library-sync:read:botlets-brains",
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// stubEnsureSyncConfiguredViaDaemon makes the sync-provision step a no-op for
// tests that exercise other seams; provisioning has its own dedicated tests.
func stubEnsureSyncConfiguredViaDaemon(t *testing.T) {
	t.Helper()
	prev := ensureSyncConfiguredViaDaemonSeam
	ensureSyncConfiguredViaDaemonSeam = func(context.Context, commentbus.Paths, commentbus.DaemonAuth) error { return nil }
	t.Cleanup(func() { ensureSyncConfiguredViaDaemonSeam = prev })
}

func testDaemonAuthFor(base string) commentbus.DaemonAuth {
	return commentbus.DaemonAuth{DaemonID: "ld_worker-test", Token: testEnrollmentDaemonToken, BaseURL: base}
}

func TestEnsureSyncConfiguredViaDaemonProvisionsAndIsIdempotent(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	t.Setenv("HOME", t.TempDir()) // default sync root resolves under a scratch home
	calls := 0
	server := newSyncProvisionTestServer(t, &calls)

	if err := ensureSyncConfiguredViaDaemon(context.Background(), paths, testDaemonAuthFor(server.URL)); err != nil {
		t.Fatalf("ensure (first) = %v", err)
	}
	if calls != 1 {
		t.Fatalf("provision calls = %d, want 1", calls)
	}
	status, err := commentsync.ReadStatus(commentsync.Options{Home: paths.Home})
	if err != nil || !status.Configured {
		t.Fatalf("sync status after provision = %+v err=%v, want configured", status, err)
	}
	if status.BaseURL != server.URL {
		t.Fatalf("sync base url = %q, want %q", status.BaseURL, server.URL)
	}

	// Already configured: no second mint.
	if err := ensureSyncConfiguredViaDaemon(context.Background(), paths, testDaemonAuthFor(server.URL)); err != nil {
		t.Fatalf("ensure (second) = %v", err)
	}
	if calls != 1 {
		t.Fatalf("provision calls after idempotent ensure = %d, want 1", calls)
	}
}

func TestEnsureSyncConfiguredViaDaemonFallsBackWhenDefaultRootOwnedElsewhere(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	calls := 0
	server := newSyncProvisionTestServer(t, &calls)

	// The default root is already owned by a DIFFERENT server/account/home —
	// e.g. this machine also syncs production. Marker hashes won't match.
	defaultRoot := filepath.Join(home, "Comment Docs")
	if err := os.MkdirAll(defaultRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := []byte(`{"managed_by":"comment sync","base_url_hash":"deadbeef","human_id_hash":"deadbeef","state_home_id":"deadbeef"}`)
	if err := os.WriteFile(filepath.Join(defaultRoot, ".comment-sync-root.json"), marker, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := ensureSyncConfiguredViaDaemon(context.Background(), paths, testDaemonAuthFor(server.URL)); err != nil {
		t.Fatalf("ensure = %v", err)
	}
	status, err := commentsync.ReadStatus(commentsync.Options{Home: paths.Home})
	if err != nil || !status.Configured {
		t.Fatalf("sync status = %+v err=%v, want configured", status, err)
	}
	if status.Root == defaultRoot {
		t.Fatalf("root = %q, want the host-suffixed fallback, not the foreign-owned default", status.Root)
	}
	if filepath.Dir(status.Root) != home {
		t.Fatalf("fallback root %q not under the test home %q", status.Root, home)
	}
}

// resetDaemonSyncMintCache clears the package-level minted-key cache so tests
// neither leak a cached key into each other nor observe one from earlier tests.
func resetDaemonSyncMintCache(t *testing.T) {
	t.Helper()
	daemonSyncProvisionMu.Lock()
	daemonSyncMintedKey = ""
	daemonSyncMintedKeyBase = ""
	daemonSyncMintedKeyToken = ""
	daemonSyncProvisionMu.Unlock()
	t.Cleanup(func() {
		daemonSyncProvisionMu.Lock()
		daemonSyncMintedKey = ""
		daemonSyncMintedKeyBase = ""
		daemonSyncMintedKeyToken = ""
		daemonSyncProvisionMu.Unlock()
	})
}

func TestEnsureSyncConfiguredViaDaemonFailsOnOriginMismatch(t *testing.T) {
	// Sync already configured for a DIFFERENT origin than the daemon's pairing:
	// one home supports one sync origin, and silently treating the foreign
	// mirror as usable sends every Botlets install searching the wrong
	// projection forever. The helper must fail with an actionable error and
	// must NOT clobber the existing config or mint a key.
	resetDaemonSyncMintCache(t)
	paths := testAgentEnrollmentPaths(t)
	t.Setenv("HOME", t.TempDir())
	calls := 0
	configured := newSyncProvisionTestServer(t, &calls)
	if err := ensureSyncConfiguredViaDaemon(context.Background(), paths, testDaemonAuthFor(configured.URL)); err != nil {
		t.Fatalf("initial provision = %v", err)
	}
	if calls != 1 {
		t.Fatalf("provision calls = %d, want 1", calls)
	}

	otherCalls := 0
	other := newSyncProvisionTestServer(t, &otherCalls)
	err := ensureSyncConfiguredViaDaemon(context.Background(), paths, testDaemonAuthFor(other.URL))
	if err == nil {
		t.Fatal("ensure must fail when sync is configured for a different origin")
	}
	if !strings.Contains(err.Error(), configured.URL) || !strings.Contains(err.Error(), other.URL) {
		t.Fatalf("origin-mismatch err = %v, want both origins named", err)
	}
	if otherCalls != 0 {
		t.Fatalf("mint calls against the mismatched origin = %d, want 0", otherCalls)
	}
	status, statusErr := commentsync.ReadStatus(commentsync.Options{Home: paths.Home})
	if statusErr != nil || !status.Configured || status.BaseURL != configured.URL {
		t.Fatalf("existing sync config must be untouched, status = %+v err=%v", status, statusErr)
	}
}

func TestEnsureSyncConfiguredViaDaemonReusesMintedKeyWhileLoginFails(t *testing.T) {
	// Every POST /daemon/sync-credential mints a NEW live usk_ key and no
	// daemon-token revoke endpoint exists, so a persistent LOCAL login failure
	// must not mint a fresh key on every retry — the first minted key is
	// cached in memory and reused until it persists.
	resetDaemonSyncMintCache(t)
	paths := testAgentEnrollmentPaths(t)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	calls := 0
	server := newSyncProvisionTestServer(t, &calls)

	// Make Login fail locally: the sync root cannot be created under a
	// read-only $HOME.
	if err := os.Chmod(userHome, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(userHome, 0o700) })

	for i := 0; i < 3; i++ {
		if err := ensureSyncConfiguredViaDaemon(context.Background(), paths, testDaemonAuthFor(server.URL)); err == nil {
			t.Fatalf("ensure attempt %d should fail while HOME is read-only", i+1)
		}
	}
	if calls != 1 {
		t.Fatalf("provision calls = %d, want 1 (retries must reuse the cached key)", calls)
	}

	// Once the local problem is fixed, the SAME key completes the login.
	if err := os.Chmod(userHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ensureSyncConfiguredViaDaemon(context.Background(), paths, testDaemonAuthFor(server.URL)); err != nil {
		t.Fatalf("ensure after fixing HOME = %v", err)
	}
	if calls != 1 {
		t.Fatalf("provision calls after recovery = %d, want still 1", calls)
	}
	status, err := commentsync.ReadStatus(commentsync.Options{Home: paths.Home})
	if err != nil || !status.Configured {
		t.Fatalf("sync status after recovery = %+v err=%v, want configured", status, err)
	}
}

func TestEnsureSyncConfiguredViaDaemonRemintsWhenPairingTokenChanges(t *testing.T) {
	// A re-pair / `comment bus pair --force` without restarting the daemon
	// rotates the pairing token. The previous token's cached usk_ may be dead
	// (its daemon was revoked) or belong to another account, so a same-origin
	// ensure under the NEW token must mint a fresh key instead of reusing the
	// stale one cached only by origin.
	resetDaemonSyncMintCache(t)
	paths := testAgentEnrollmentPaths(t)
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)

	var mintedTokens []string
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/sync-credential", func(w http.ResponseWriter, r *http.Request) {
		mintedTokens = append(mintedTokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":      true,
			"api_key": "usk_v2.ag_synctest.key1.secret1",
			"scope":   "library-sync:read:botlets-brains",
		})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	// Keep Login failing so the key stays cached (never persisted) between the
	// two pairings — isolating the token-identity cache decision from the
	// already-configured fast path.
	if err := os.Chmod(userHome, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(userHome, 0o700) })

	authA := commentbus.DaemonAuth{DaemonID: "ld_old", Token: "ldt_tokenA", BaseURL: server.URL}
	authB := commentbus.DaemonAuth{DaemonID: "ld_new", Token: "ldt_tokenB", BaseURL: server.URL}

	if err := ensureSyncConfiguredViaDaemon(context.Background(), paths, authA); err == nil {
		t.Fatal("ensure under token A should fail while HOME is read-only")
	}
	// Same origin AND same token: retry reuses the cached key (no new mint).
	if err := ensureSyncConfiguredViaDaemon(context.Background(), paths, authA); err == nil {
		t.Fatal("ensure retry under token A should fail while HOME is read-only")
	}
	if len(mintedTokens) != 1 {
		t.Fatalf("mints after two token-A attempts = %d, want 1 (cache reuse)", len(mintedTokens))
	}
	// Re-pair: the pairing token changed. Must mint fresh, not reuse token A's
	// (possibly revoked) key.
	if err := ensureSyncConfiguredViaDaemon(context.Background(), paths, authB); err == nil {
		t.Fatal("ensure under token B should fail while HOME is read-only")
	}
	if len(mintedTokens) != 2 {
		t.Fatalf("mints after re-pair = %d, want 2 (fresh mint for the new token)", len(mintedTokens))
	}
	if mintedTokens[1] != "ldt_tokenB" {
		t.Fatalf("second mint used token %q, want ldt_tokenB", mintedTokens[1])
	}
}

func TestPreflightBotletsTeamManifestSync(t *testing.T) {
	// The manual team-setup/team-resync paths must fail closed before the
	// manifest mint when local sync is not usable for the team's origin.
	paths := testAgentEnrollmentPaths(t)
	t.Setenv("HOME", t.TempDir())
	cfg := botletsTeamRuntimeConfig{BaseURL: "https://botlets.example"}

	err := preflightBotletsTeamManifestSync(paths, cfg)
	if err == nil {
		t.Fatal("preflight should fail when sync is unconfigured")
	}
	if !strings.Contains(err.Error(), cfg.BaseURL) || !strings.Contains(err.Error(), "comment sync login") {
		t.Fatalf("preflight err = %v, want it to name the origin and the `comment sync login` fix", err)
	}

	// Configure sync for the team's own origin, then the preflight passes.
	resetDaemonSyncMintCache(t)
	calls := 0
	server := newSyncProvisionTestServer(t, &calls)
	if err := ensureSyncConfiguredViaDaemon(context.Background(), paths, testDaemonAuthFor(server.URL)); err != nil {
		t.Fatalf("configure sync = %v", err)
	}
	cfg.BaseURL = server.URL
	if err := preflightBotletsTeamManifestSync(paths, cfg); err != nil {
		t.Fatalf("preflight after configuring sync for the origin = %v", err)
	}

	// Configured, but for a DIFFERENT origin than the team's: still fails closed.
	cfg.BaseURL = "https://other.example"
	if err := preflightBotletsTeamManifestSync(paths, cfg); err == nil {
		t.Fatal("preflight should fail when sync is configured for a different origin")
	}
}

func TestEnsureSyncConfiguredViaDaemonPropagatesMintFailure(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	t.Setenv("HOME", t.TempDir())
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/sync-credential", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	err := ensureSyncConfiguredViaDaemon(context.Background(), paths, testDaemonAuthFor(server.URL))
	if err == nil {
		t.Fatal("ensure should fail when the mint endpoint fails")
	}
	if status, statusErr := commentsync.ReadStatus(commentsync.Options{Home: paths.Home}); statusErr == nil && status.Configured {
		t.Fatal("sync must not be configured after a failed mint")
	}
}
