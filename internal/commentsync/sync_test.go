package commentsync

import (
	"bufio"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoginStatusLogout(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	var revoked bool
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/library-sync/current-device" || r.Method != http.MethodDelete {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		revoked = true
		writeJSON(t, w, map[string]any{"ok": true})
	}))
	defer server.Close()
	cfg, err := Login(context.Background(), Options{
		Home:    home,
		Root:    root,
		BaseURL: server.URL,
		APIKey:  key,
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if cfg.Root != root || cfg.BaseURL != server.URL || !cfg.ManualOnly || cfg.BackgroundSync {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	credInfo, err := os.Stat(filepath.Join(home, "sync", "credentials.json"))
	if err != nil {
		t.Fatalf("credentials stat: %v", err)
	}
	if credInfo.Mode().Perm() != 0o600 {
		t.Fatalf("credentials mode = %o", credInfo.Mode().Perm())
	}
	status, err := ReadStatus(Options{Home: home})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.Configured || !status.ManualOnly || status.Root != root {
		t.Fatalf("unexpected status: %+v", status)
	}
	if status.LiveSyncEnabled {
		t.Fatalf("live sync should default disabled: %+v", status)
	}
	if err := SetLiveSync(context.Background(), Options{Home: home}, true); err != nil {
		t.Fatalf("enable live sync: %v", err)
	}
	status, err = ReadStatus(Options{Home: home})
	if err != nil {
		t.Fatalf("status after live enable: %v", err)
	}
	if !status.LiveSyncEnabled {
		t.Fatalf("live sync was not enabled: %+v", status)
	}
	logout, err := Logout(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	if !logout.Removed || !logout.ServerRevoked || !revoked {
		t.Fatalf("logout did not remove config and revoke server key: %+v revoked=%v", logout, revoked)
	}
	status, err = ReadStatus(Options{Home: home})
	if err != nil {
		t.Fatalf("status after logout: %v", err)
	}
	if status.Configured {
		t.Fatalf("configured after logout: %+v", status)
	}
}

func TestStartDeviceLoginSendsStableDeviceID(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Comment Docs")
	var payload map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/library-sync/device-codes" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, map[string]any{
			"userCode":        "ABCDEFGH",
			"deviceCode":      "ldc_test",
			"verificationUri": "/settings#local-sync-devices?code=ABCDEFGH",
			"expiresAt":       time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
			"interval":        3,
			"scopeLabel":      "My Files, Shared With Me, Team Wiki, and Botlets brains",
		})
	}))
	defer server.Close()

	session, err := StartDeviceLogin(context.Background(), Options{Root: root, BaseURL: server.URL})
	if err != nil {
		t.Fatalf("start device login: %v", err)
	}
	if payload["device_id"] != librarySyncDeviceID(root) {
		t.Fatalf("device_id = %v, want %s", payload["device_id"], librarySyncDeviceID(root))
	}
	if payload["root_label"] != root {
		t.Fatalf("root_label = %v, want %s", payload["root_label"], root)
	}
	if session.VerificationURI != server.URL+"/settings#local-sync-devices?code=ABCDEFGH" {
		t.Fatalf("verification uri = %q", session.VerificationURI)
	}
}

func TestLoginValidatesKeyBeforeWritingConfigAndActivatesAfterWriting(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	var validated bool
	var activated bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			validated = true
			if r.Header.Get("Authorization") != "Bearer "+key {
				http.Error(w, "bad auth", http.StatusUnauthorized)
				return
			}
			writeJSON(t, w, map[string]any{
				"snapshotId":          "lse_login_validate",
				"scopeLabel":          "My Files, Shared With Me, Team Wiki, and Botlets brains",
				"coveredSections":     []map[string]any{{"id": "botlets-brains", "label": "Botlets brains", "covered": true, "authoritative": true, "count": 0}},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows":                []map[string]any{},
				"pageInfo":            map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/auth/library-sync/current-device/activate":
			activated = true
			if r.Method != http.MethodPost {
				http.Error(w, "bad method", http.StatusMethodNotAllowed)
				return
			}
			if r.Header.Get("Authorization") != "Bearer "+key {
				http.Error(w, "bad auth", http.StatusUnauthorized)
				return
			}
			if _, err := os.Stat(filepath.Join(home, "sync", "credentials.json")); err != nil {
				t.Fatalf("activation happened before credentials were written: %v", err)
			}
			writeJSON(t, w, map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key, ValidateKey: true}); err != nil {
		t.Fatalf("login validate: %v", err)
	}
	if !validated {
		t.Fatalf("expected login to validate key before writing config")
	}
	if !activated {
		t.Fatalf("expected login to activate key after writing config")
	}
	if _, err := os.Stat(filepath.Join(home, "sync", "credentials.json")); err != nil {
		t.Fatalf("credentials stat: %v", err)
	}
}

func TestLoginRejectsInvalidKeyBeforeWritingConfig(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/library-sync/snapshot" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(t, w, map[string]any{
			"error":          "Invalid library sync user API key",
			"code":           "INVALID_LIBRARY_SYNC_KEY",
			"required_scope": "library-sync:read:botlets-brains",
		})
	}))
	defer server.Close()

	_, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key, ValidateKey: true})
	if err == nil {
		t.Fatalf("expected invalid key login to fail")
	}
	if !strings.Contains(err.Error(), "library sync login rejected the API key") || !strings.Contains(err.Error(), "expired, revoked, or copied from an old setup command") {
		t.Fatalf("login error was not clear: %v", err)
	}
	for _, path := range []string{
		filepath.Join(home, "sync", "config.json"),
		filepath.Join(home, "sync", "credentials.json"),
	} {
		if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
			t.Fatalf("login wrote %s despite invalid key; statErr=%v", path, statErr)
		}
	}
}

func TestLoginValidationServerFailureIsNotReportedAsRejectedKey(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/library-sync/snapshot" {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	_, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key, ValidateKey: true})
	if err == nil {
		t.Fatalf("expected validation server failure to fail login")
	}
	if !strings.Contains(err.Error(), "could not validate the API key") || strings.Contains(err.Error(), "rejected the API key") {
		t.Fatalf("login error misclassified server failure: %v", err)
	}
}

func TestLoginMalformedKeyHasActionableError(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	_, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: "https://example.test", APIKey: "fresh key copied by button", ValidateKey: true})
	if err == nil {
		t.Fatalf("expected malformed key login to fail")
	}
	if !strings.Contains(err.Error(), "scoped usk_v2 key") || !strings.Contains(err.Error(), "copy a fresh setup command") {
		t.Fatalf("malformed key error was not actionable: %v", err)
	}
}

func TestOnceInvalidKeyHasActionableError(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/library-sync/snapshot" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(t, w, map[string]any{
			"error":          "Invalid library sync user API key",
			"code":           "INVALID_LIBRARY_SYNC_KEY",
			"required_scope": "library-sync:read:botlets-brains",
		})
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login seed: %v", err)
	}
	_, err := Once(context.Background(), Options{Home: home})
	if err == nil {
		t.Fatalf("expected once with invalid key to fail")
	}
	if !strings.Contains(err.Error(), "library sync key was rejected") || !strings.Contains(err.Error(), "copy a fresh setup command") {
		t.Fatalf("once error was not actionable: %v", err)
	}
}

func TestSyncStateMigratesV1PlacementHashes(t *testing.T) {
	home := t.TempDir()
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := ensurePrivateDirs(paths); err != nil {
		t.Fatalf("ensure private dirs: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "sync", "library.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	bodyHash := sha256Hex("# Legacy\n")
	if _, err := db.Exec(`
		CREATE TABLE placements (
			visible_id TEXT PRIMARY KEY,
			slug TEXT NOT NULL,
			section TEXT NOT NULL,
			path TEXT NOT NULL,
			canonical_path TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			revision INTEGER NOT NULL,
			last_seen_snapshot TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);
		INSERT INTO placements (visible_id, slug, section, path, canonical_path, content_hash, revision, last_seen_snapshot, updated_at)
		VALUES ('doc-1', 'abc123', 'my-files', '/tmp/Legacy.md', '/tmp/Legacy.md', ?, 1, 'lse_old', '2026-05-20T00:00:00Z');
		PRAGMA user_version = 1;
	`, bodyHash); err != nil {
		_ = db.Close()
		t.Fatalf("seed v1 db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
	state, err := openSyncState(context.Background(), paths)
	if err != nil {
		t.Fatalf("open migrated state: %v", err)
	}
	defer state.Close()
	meta, ok, err := state.getPlacement(context.Background(), "doc-1")
	if err != nil {
		t.Fatalf("get placement: %v", err)
	}
	if !ok || meta.BodyContentHash != bodyHash || meta.ContentHash != bodyHash || meta.ProjectionFormatVersion != 0 {
		t.Fatalf("migrated placement = %+v, want body/content hash %s and format 0", meta, bodyHash)
	}
	var version int
	if err := state.db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != stateSchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, stateSchemaVersion)
	}
}

func TestSyncStateRecreatesV3BotspringSchema(t *testing.T) {
	// A pre-rebrand sync DB (created by the Botspring CLI in the shared
	// ~/.comment-io home) is at user_version 3 with botspring_* placement
	// columns. Opening it under the Botlets binary must recreate local sync
	// state (plan §1: recreate, no RENAME COLUMN) and reach the current schema,
	// NOT fail with "sync database migration did not reach current schema".
	home := t.TempDir()
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := ensurePrivateDirs(paths); err != nil {
		t.Fatalf("ensure private dirs: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "sync", "library.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE placements (
			visible_id TEXT PRIMARY KEY,
			slug TEXT NOT NULL,
			section TEXT NOT NULL,
			path TEXT NOT NULL,
			canonical_path TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			body_content_hash TEXT NOT NULL DEFAULT '',
			rendered_projection_hash TEXT NOT NULL DEFAULT '',
			projection_format_version INTEGER NOT NULL DEFAULT 0,
			etag TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL,
			last_seen_snapshot TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			botspring_owner_handle TEXT NOT NULL DEFAULT '',
			botspring_bot_slug TEXT NOT NULL DEFAULT '',
			botspring_bot_local_name TEXT NOT NULL DEFAULT '',
			botspring_bot_handle TEXT NOT NULL DEFAULT '',
			botspring_bot_agent_id TEXT NOT NULL DEFAULT '',
			botspring_brain_container_id TEXT NOT NULL DEFAULT '',
			botspring_brain_root_folder_id TEXT NOT NULL DEFAULT '',
			botspring_brain_node_id TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO placements (visible_id, slug, section, path, canonical_path, content_hash, revision, last_seen_snapshot, updated_at)
		VALUES ('doc-old', 'old123', 'botspring-brains', '/tmp/Old.md', '/tmp/Old.md', 'h', 1, 'lse_old', '2026-05-20T00:00:00Z');
		PRAGMA user_version = 3;
	`); err != nil {
		_ = db.Close()
		t.Fatalf("seed v3 db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
	state, err := openSyncState(context.Background(), paths)
	if err != nil {
		t.Fatalf("open migrated state: %v (v3 recreate must not error)", err)
	}
	defer state.Close()
	var version int
	if err := state.db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != stateSchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, stateSchemaVersion)
	}
	// The placements table was recreated at the botlets_* schema; querying the
	// renamed column must succeed and the stale botspring-era row must be gone.
	if _, err := state.db.ExecContext(context.Background(), `SELECT botlets_bot_slug FROM placements LIMIT 0`); err != nil {
		t.Fatalf("botlets_bot_slug column missing after recreate: %v", err)
	}
	var count int
	if err := state.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM placements`).Scan(&count); err != nil {
		t.Fatalf("count placements: %v", err)
	}
	if count != 0 {
		t.Fatalf("placements after recreate = %d, want 0 (recreated; re-syncs on next run)", count)
	}
}

func TestSyncStateMigratesV4BotletsBotID(t *testing.T) {
	home := t.TempDir()
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := ensurePrivateDirs(paths); err != nil {
		t.Fatalf("ensure private dirs: %v", err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, "sync", "library.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE placements (
			visible_id TEXT PRIMARY KEY,
			slug TEXT NOT NULL,
			section TEXT NOT NULL,
			path TEXT NOT NULL,
			canonical_path TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			body_content_hash TEXT NOT NULL DEFAULT '',
			rendered_projection_hash TEXT NOT NULL DEFAULT '',
			projection_format_version INTEGER NOT NULL DEFAULT 0,
			etag TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL,
			last_seen_snapshot TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			botlets_owner_handle TEXT NOT NULL DEFAULT '',
			botlets_bot_slug TEXT NOT NULL DEFAULT '',
			botlets_bot_local_name TEXT NOT NULL DEFAULT '',
			botlets_bot_handle TEXT NOT NULL DEFAULT '',
			botlets_bot_agent_id TEXT NOT NULL DEFAULT '',
			botlets_brain_container_id TEXT NOT NULL DEFAULT '',
			botlets_brain_root_folder_id TEXT NOT NULL DEFAULT '',
			botlets_brain_node_id TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO placements (visible_id, slug, section, path, canonical_path, content_hash, revision, last_seen_snapshot, updated_at, botlets_bot_slug, botlets_bot_agent_id)
		VALUES ('brain-doc', 'identity', 'botlets-brains', '/tmp/Identity.md', '/tmp/Identity.md', 'hash', 1, 'lse_old', '2026-05-20T00:00:00Z', 'reviewer', 'ag_bot');
		PRAGMA user_version = 4;
	`); err != nil {
		_ = db.Close()
		t.Fatalf("seed v4 db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}
	state, err := openSyncState(context.Background(), paths)
	if err != nil {
		t.Fatalf("open migrated state: %v", err)
	}
	defer state.Close()
	meta, ok, err := state.getPlacement(context.Background(), "brain-doc")
	if err != nil {
		t.Fatalf("get placement: %v", err)
	}
	if !ok || meta.BotletsBotSlug != "reviewer" || meta.BotletsBotAgentID != "ag_bot" || meta.BotletsBotID != "" {
		t.Fatalf("migrated placement = %+v", meta)
	}
	var version int
	if err := state.db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != stateSchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, stateSchemaVersion)
	}
}

func TestLogoutRemovesLocalConfigWhenServerRevokeFails(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "already revoked", http.StatusForbidden)
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	logout, err := Logout(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	if !logout.Removed || logout.ServerRevoked {
		t.Fatalf("unexpected logout result: %+v", logout)
	}
	for _, path := range []string{
		filepath.Join(home, "sync", "config.json"),
		filepath.Join(home, "sync", "credentials.json"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("local sync file remains after logout: %s err=%v", path, err)
		}
	}
}

func TestLogoutPurgeLocalRemovesOnlyCleanProjections(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-clean", "section": "my-files", "kind": "document", "name": "Clean", "parentVisibleInstanceId": nil, "docSlug": "clean"},
		{"visibleInstanceId": "doc-dirty", "section": "my-files", "kind": "document", "name": "Dirty", "parentVisibleInstanceId": nil, "docSlug": "dirty"},
	}, map[string]string{
		"clean": "# Clean\n",
		"dirty": "# Dirty\n",
	})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	cleanPath := filepath.Join(root, "My Files", "Clean.md")
	dirtyPath := filepath.Join(root, "My Files", "Dirty.md")
	if err := os.Chmod(dirtyPath, 0o644); err != nil {
		t.Fatalf("chmod dirty projection: %v", err)
	}
	if err := os.WriteFile(dirtyPath, []byte("# Local dirty edit\n"), 0o644); err != nil {
		t.Fatalf("write dirty projection: %v", err)
	}

	logout, err := Logout(context.Background(), Options{Home: home, PurgeLocal: true})
	if err != nil {
		t.Fatalf("logout purge local: %v", err)
	}
	if !logout.Removed || !logout.PurgedLocal || logout.ProjectionsRemoved != 1 || logout.RecoveriesPreserved != 1 {
		t.Fatalf("unexpected logout result: %+v", logout)
	}
	if _, err := os.Stat(cleanPath); !os.IsNotExist(err) {
		t.Fatalf("clean projection remains after purge: err=%v", err)
	}
	if got := mustRead(t, dirtyPath); got != "# Local dirty edit\n" {
		t.Fatalf("dirty projection was not preserved: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".comment-sync-root.json")); !os.IsNotExist(err) {
		t.Fatalf("generated root marker remains after purge: err=%v", err)
	}
	recoveries, err := ListRecoveries(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("list recoveries: %v", err)
	}
	if len(recoveries) != 1 || recoveries[0].Reason != "local_dirty_before_purge" {
		t.Fatalf("recoveries = %+v", recoveries)
	}
}

func TestLogoutThenLoginDifferentRootClearsProjectionState(t *testing.T) {
	home := t.TempDir()
	firstRoot := filepath.Join(t.TempDir(), "Comment Docs")
	secondRoot := filepath.Join(t.TempDir(), "Other Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: firstRoot, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("first once: %v", err)
	}
	if got := mustRead(t, filepath.Join(firstRoot, "My Files", "Launch.md")); got != "# Launch\n" {
		t.Fatalf("first projection = %q", got)
	}
	if _, err := Logout(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := Login(context.Background(), Options{Home: home, Root: secondRoot, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("second login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("second once after root change: %v", err)
	}
	if got := mustRead(t, filepath.Join(secondRoot, "My Files", "Launch.md")); got != "# Launch\n" {
		t.Fatalf("second projection = %q", got)
	}
}

func TestLoginDifferentInvalidRootPreservesExistingProjectionState(t *testing.T) {
	home := t.TempDir()
	firstRoot := filepath.Join(t.TempDir(), "Comment Docs")
	otherHome := t.TempDir()
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: firstRoot, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("first once: %v", err)
	}
	foreignRoot := filepath.Join(t.TempDir(), "Foreign Root")
	if _, err := Login(context.Background(), Options{Home: otherHome, Root: foreignRoot, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("foreign login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: otherHome}); err != nil {
		t.Fatalf("foreign once: %v", err)
	}
	if _, err := Login(context.Background(), Options{Home: home, Root: foreignRoot, BaseURL: server.URL, APIKey: key}); err == nil || !strings.Contains(err.Error(), "state_home_id") {
		t.Fatalf("expected root ownership error, got %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once after failed relogin: %v", err)
	}
	if got := mustRead(t, filepath.Join(firstRoot, "My Files", "Launch.md")); got != "# Launch\n" {
		t.Fatalf("projection after failed relogin = %q", got)
	}
}

func TestLogoutThenLoginDifferentRootClearsPendingOps(t *testing.T) {
	home := t.TempDir()
	firstRoot := filepath.Join(t.TempDir(), "Comment Docs")
	secondRoot := filepath.Join(t.TempDir(), "Other Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: firstRoot, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("first login: %v", err)
	}
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	state, err := openSyncState(context.Background(), paths)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	oldTarget := filepath.Join(firstRoot, "My Files", "Launch.md")
	if _, err := state.beginOp(context.Background(), "write_projection", "doc-1", "abc123", oldTarget, "lse_crash"); err != nil {
		t.Fatalf("begin old-root op: %v", err)
	}
	if err := state.Close(); err != nil {
		t.Fatalf("close state: %v", err)
	}
	if _, err := Logout(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := Login(context.Background(), Options{Home: home, Root: secondRoot, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("second login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once after pending-op reset: %v", err)
	}
	if got := mustRead(t, filepath.Join(secondRoot, "My Files", "Launch.md")); got != "# Launch\n" {
		t.Fatalf("projection after pending-op reset = %q", got)
	}
}

func TestOnceMirrorsLibrarySnapshotAndPreservesLocalEdits(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	var projectionFetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_test",
				"scopeLabel": "My Files and Shared With Me",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 1},
					{"id": "shared-with-me", "label": "Shared With Me", "covered": true, "authoritative": true, "count": 1},
				},
				"unsupportedSections": []map[string]any{
					{"id": "team-workspaces", "label": "Team workspaces", "covered": false, "authoritative": false, "count": 0, "reason": "not in v1"},
				},
				"snapshotComplete": true,
				"rows": []map[string]any{
					{"visibleInstanceId": "folder-1", "section": "my-files", "kind": "folder", "name": "Plans", "parentVisibleInstanceId": nil},
					{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": "folder-1", "docSlug": "abc123"},
					{"visibleInstanceId": "shared-1", "section": "shared-with-me", "kind": "document", "name": "Shared Spec", "parentVisibleInstanceId": nil, "docSlug": "def456", "sourceLabel": "Max"},
				},
				"pageInfo": map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/docs/abc123":
			projectionFetches++
			writeJSON(t, w, projection("abc123", "# Launch\n\nServer copy.\n", 3))
		case "/docs/def456":
			projectionFetches++
			body := projection("def456", "# Shared\n", 4)
			body["title"] = nil
			writeJSON(t, w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	first, err := Once(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("once: %v", err)
	}
	if first.DocumentsWritten != 2 || projectionFetches != 2 {
		t.Fatalf("unexpected first result: %+v projectionFetches=%d", first, projectionFetches)
	}
	launchPath := filepath.Join(root, "My Files", "Plans", "Launch.md")
	if got := mustRead(t, launchPath); got != "# Launch\n\nServer copy.\n" {
		t.Fatalf("launch markdown = %q", got)
	}
	if mode := mustMode(t, launchPath); mode&0o222 != 0 {
		t.Fatalf("projection should be read-only, mode=%o", mode)
	}
	sharedPath := filepath.Join(root, "Shared With Me", "Max", "Shared Spec.md")
	if got := mustRead(t, sharedPath); got != "# Shared\n" {
		t.Fatalf("shared markdown = %q", got)
	}
	if err := os.Chmod(launchPath, 0o644); err != nil {
		t.Fatalf("chmod dirty: %v", err)
	}
	if err := os.WriteFile(launchPath, []byte("# Local edit\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}
	second, err := Once(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("second once: %v", err)
	}
	if second.RecoveriesPreserved != 1 {
		t.Fatalf("expected one recovery, got %+v", second)
	}
	recoveries, err := os.ReadDir(filepath.Join(home, "sync", "recovery"))
	if err != nil {
		t.Fatalf("read recoveries: %v", err)
	}
	if countRecoveryMarkdown(recoveries) != 1 {
		t.Fatalf("recoveries = %+v", recoveries)
	}
	if got := mustRead(t, launchPath); got != "# Launch\n\nServer copy.\n" {
		t.Fatalf("launch markdown after recovery = %q", got)
	}
}

func TestOnceWritesHeaderedProjectionAndLocalAgentDocs(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/llms.txt":
			if r.Header.Get("Authorization") != "" {
				t.Fatalf("local docs fetch should not send authorization")
			}
			_, _ = w.Write([]byte("# Local docs\n\nRead local, write API.\n"))
			return
		case strings.HasPrefix(r.URL.Path, "/llms/"):
			if r.Header.Get("Authorization") != "" {
				t.Fatalf("split docs fetch should not send authorization")
			}
			_, _ = w.Write([]byte("# Split docs\n"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_test",
				"scopeLabel": "My Files and Shared With Me",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 1},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows": []map[string]any{
					{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
				},
				"pageInfo": map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/docs/abc123":
			body := projection("abc123", "# Launch\n", 7)
			body["canonical_url"] = server.URL + "/docs/abc123?token=should-strip"
			writeJSON(t, w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	path := filepath.Join(root, "My Files", "Launch.md")
	raw := mustReadRaw(t, path)
	if !strings.HasPrefix(raw, "<!-- comment.io:projection\n") || !strings.Contains(raw, "comment.io:projection:end -->\n\n# Launch\n") {
		t.Fatalf("projection header not rendered correctly:\n%s", raw)
	}
	for _, want := range []string{
		"read-only: true",
		"canonical-url: " + server.URL + "/docs/abc123",
		"slug: abc123",
		"revision: 7",
		"content-sha256: " + sha256Hex("# Launch\n"),
		"local-docs-root: _Comment.io Docs",
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("projection header missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, key) || strings.Contains(raw, "should-strip") {
		t.Fatalf("projection header leaked secret-like value:\n%s", raw)
	}
	if got := mustRead(t, path); got != "# Launch\n" {
		t.Fatalf("projection body = %q", got)
	}
	if mode := mustMode(t, path); mode&0o222 != 0 {
		t.Fatalf("projection should be read-only, mode=%o", mode)
	}
	docsRoot := filepath.Join(root, localAgentDocsDirName)
	if got := mustRead(t, filepath.Join(docsRoot, "llms.txt")); !strings.Contains(got, "Read local, write API") || strings.Contains(got, key) {
		t.Fatalf("local llms docs = %q", got)
	}
	if marker := mustReadRaw(t, filepath.Join(docsRoot, localAgentDocsMarkerName)); !strings.Contains(marker, `"kind": "local-agent-docs"`) {
		t.Fatalf("local agent docs marker = %q", marker)
	}
	if mode := mustMode(t, filepath.Join(docsRoot, "llms.txt")); mode&0o222 != 0 {
		t.Fatalf("local docs should be read-only, mode=%o", mode)
	}
	if got := mustRead(t, filepath.Join(root, "README.md")); !strings.Contains(got, "comment.io:projection header") || !strings.Contains(got, "_Comment.io Docs/") {
		t.Fatalf("root README missing header/local docs guidance:\n%s", got)
	}
	extraPath := filepath.Join(docsRoot, "user-notes.txt")
	if err := os.WriteFile(extraPath, []byte("do not delete\n"), 0o644); err != nil {
		t.Fatalf("write extra docs file: %v", err)
	}
	if _, err := Logout(context.Background(), Options{Home: home, PurgeLocal: true}); err != nil {
		t.Fatalf("logout purge: %v", err)
	}
	if got := mustReadRaw(t, extraPath); got != "do not delete\n" {
		t.Fatalf("unexpected docs file was removed during purge: %q", got)
	}
	if _, err := os.Stat(filepath.Join(docsRoot, "llms.txt")); !os.IsNotExist(err) {
		t.Fatalf("generated docs file remains after purge: %v", err)
	}
}

func TestOnceDoesNotOverwriteUnknownLocalAgentDocsFolder(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/llms"):
			_, _ = w.Write([]byte("# Generated docs\n"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/auth/library-sync/snapshot" {
			writeJSON(t, w, map[string]any{
				"snapshotId":          "lse_test",
				"scopeLabel":          "My Files and Shared With Me",
				"coveredSections":     []map[string]any{},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows":                []map[string]any{},
				"pageInfo":            map[string]any{"nextCursor": nil, "partial": false},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	docsRoot := filepath.Join(root, localAgentDocsDirName)
	if err := os.MkdirAll(docsRoot, 0o755); err != nil {
		t.Fatalf("mkdir unknown docs root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsRoot, "README.md"), []byte("user readme\n"), 0o644); err != nil {
		t.Fatalf("write unknown docs readme: %v", err)
	}
	if err := os.WriteFile(filepath.Join(docsRoot, "llms.txt"), []byte("user llms\n"), 0o644); err != nil {
		t.Fatalf("write unknown docs llms: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	if got := mustReadRaw(t, filepath.Join(docsRoot, "README.md")); got != "user readme\n" {
		t.Fatalf("unknown docs README overwritten: %q", got)
	}
	if got := mustReadRaw(t, filepath.Join(docsRoot, "llms.txt")); got != "user llms\n" {
		t.Fatalf("unknown docs llms overwritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(docsRoot, localAgentDocsMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("marker written into unknown docs folder: %v", err)
	}
	if _, err := Logout(context.Background(), Options{Home: home, PurgeLocal: true}); err != nil {
		t.Fatalf("logout purge: %v", err)
	}
	if got := mustReadRaw(t, filepath.Join(docsRoot, "README.md")); got != "user readme\n" {
		t.Fatalf("unknown docs README removed during purge: %q", got)
	}
}

func TestOnceMigratesLegacyRawProjectionWithoutRecovery(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("first once: %v", err)
	}
	path := filepath.Join(root, "My Files", "Launch.md")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod legacy: %v", err)
	}
	if err := os.WriteFile(path, []byte("# Launch\n"), 0o644); err != nil {
		t.Fatalf("write legacy raw projection: %v", err)
	}
	result, err := Once(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("second once: %v", err)
	}
	if result.RecoveriesPreserved != 0 {
		t.Fatalf("legacy clean projection should not recover: %+v", result)
	}
	if raw := mustReadRaw(t, path); !strings.HasPrefix(raw, "<!-- comment.io:projection\n") {
		t.Fatalf("legacy projection was not migrated to header format:\n%s", raw)
	}
	recoveries, err := ListRecoveries(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("list recoveries: %v", err)
	}
	if len(recoveries) != 0 {
		t.Fatalf("unexpected recoveries after legacy migration: %+v", recoveries)
	}
}

func TestProjectionBodyForDirtyCheckRequiresValidHeaderMetadata(t *testing.T) {
	raw := []byte("<!-- comment.io:projection\nnot really ours\ncomment.io:projection:end -->\n# Body\n")
	if got := string(projectionBodyForDirtyCheck(raw)); got != string(raw) {
		t.Fatalf("invalid projection-like header should not be stripped: %q", got)
	}
	if got := string(projectionBodyForDirtyCheckWithExpectedHash(raw, sha256Hex("# Body\n"))); got != "# Body\n" {
		t.Fatalf("expected-hash dirty check should strip structurally matching generated header: %q", got)
	}
}

func TestBotletsBrainProjectionHeaderAndRecovery(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	rows := []map[string]any{
		{
			"visibleInstanceId":        "brain-doc-1",
			"section":                  "botlets-brains",
			"kind":                     "document",
			"name":                     "Identity",
			"parentVisibleInstanceId":  nil,
			"docSlug":                  "identity",
			"botletsOwnerHandle":       "max",
			"botletsBotSlug":           "pmf-tracker",
			"botletsBotLocalName":      "pmf-tracker",
			"botletsBotId":             "bot_pmf",
			"botletsBotHandle":         "max.pmf-tracker",
			"botletsBotAgentId":        "ag_bot",
			"botletsBrainContainerId":  "lc_brain",
			"botletsBrainRootFolderId": "lf_brain_root",
			"botletsBrainNodeId":       "ln_identity",
		},
	}
	server := syncTestServer(t, key, rows, map[string]string{"identity": "# Identity\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	target := filepath.Join(root, "Botlets", "max", "pmf-tracker", "brain", "Identity.md")
	projection, err := FindBotletsBrainProjection(context.Background(), Options{Home: home}, BotletsBrainProjectionQuery{
		WorkspaceID:  "ws_brain",
		BotID:        "bot_pmf",
		BotAgentID:   "ag_bot",
		ContainerID:  "lc_brain",
		RootFolderID: "lf_brain_root",
	})
	if err != nil {
		t.Fatalf("find Botlets brain projection: %v", err)
	}
	if projection.Root != filepath.Dir(target) || projection.RelativePath != "Botlets/max/pmf-tracker/brain" {
		t.Fatalf("projection = %+v, want root %s", projection, filepath.Dir(target))
	}
	if projection.WorkspaceID != "ws_brain" || projection.BotID != "bot_pmf" || projection.SyncRootFingerprint != SyncRootFingerprint(root) {
		t.Fatalf("projection identifiers = %+v", projection)
	}
	if _, err := FindBotletsBrainProjection(context.Background(), Options{Home: home}, BotletsBrainProjectionQuery{
		BotID:        "bot_missing",
		BotAgentID:   "ag_bot",
		ContainerID:  "lc_brain",
		RootFolderID: "lf_brain_root",
	}); !errors.Is(err, ErrBotletsBrainProjectionNotFound) {
		t.Fatalf("missing bot id projection err = %v", err)
	}
	if _, err := FindBotletsBrainProjection(context.Background(), Options{Home: home}, BotletsBrainProjectionQuery{
		BotAgentID:   "ag_missing",
		ContainerID:  "lc_brain",
		RootFolderID: "lf_brain_root",
	}); !errors.Is(err, ErrBotletsBrainProjectionNotFound) {
		t.Fatalf("missing projection err = %v", err)
	}
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	state, err := openSyncState(context.Background(), paths)
	if err != nil {
		t.Fatalf("open sync state: %v", err)
	}
	if _, err := state.db.ExecContext(context.Background(), `UPDATE placements SET botlets_bot_id = '' WHERE visible_id = ?`, "brain-doc-1"); err != nil {
		state.Close()
		t.Fatalf("clear legacy bot id: %v", err)
	}
	state.Close()
	legacyProjection, err := FindBotletsBrainProjection(context.Background(), Options{Home: home}, BotletsBrainProjectionQuery{
		WorkspaceID:  "ws_brain",
		BotID:        "bot_pmf",
		BotAgentID:   "ag_bot",
		ContainerID:  "lc_brain",
		RootFolderID: "lf_brain_root",
	})
	if err != nil {
		t.Fatalf("find legacy Botlets brain projection: %v", err)
	}
	if legacyProjection.BotID != "" || legacyProjection.BotAgentID != "ag_bot" || legacyProjection.Root != filepath.Dir(target) {
		t.Fatalf("legacy projection = %+v", legacyProjection)
	}
	raw := mustReadRaw(t, target)
	for _, want := range []string{
		"section: botlets-brains",
		"botlets-owner-handle: max",
		"botlets-bot-slug: pmf-tracker",
		"botlets-bot-local-name: pmf-tracker",
		"botlets-bot-id: bot_pmf",
		"botlets-bot-handle: max.pmf-tracker",
		"botlets-bot-agent-id: ag_bot",
		"botlets-brain-container-id: lc_brain",
		"botlets-brain-root-folder-id: lf_brain_root",
		"botlets-brain-node-id: ln_identity",
	} {
		if !strings.Contains(raw, want) {
			t.Fatalf("Botlets projection header missing %q:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, key) {
		t.Fatalf("Botlets projection header leaked API key:\n%s", raw)
	}

	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod brain projection: %v", err)
	}
	if err := os.WriteFile(target, []byte("# Locally edited identity\n"), 0o644); err != nil {
		t.Fatalf("write dirty brain projection: %v", err)
	}
	result, err := RecoverDirtyProjections(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("recover dirty brain projection: %v", err)
	}
	if result.Checked != 1 || result.ProjectionRefreshes != 1 || result.RecoveriesPreserved != 1 {
		t.Fatalf("unexpected recovery result: %+v", result)
	}
	if got := mustRead(t, target); got != "# Identity\n" {
		t.Fatalf("brain projection body after recovery = %q", got)
	}
	if raw := mustReadRaw(t, target); !strings.Contains(raw, "botlets-brain-node-id: ln_identity") {
		t.Fatalf("Botlets metadata was not preserved after recovery:\n%s", raw)
	}
	if err := os.RemoveAll(filepath.Dir(target)); err != nil {
		t.Fatalf("remove brain projection root: %v", err)
	}
	if _, err := FindBotletsBrainProjection(context.Background(), Options{Home: home}, BotletsBrainProjectionQuery{
		WorkspaceID:  "ws_brain",
		BotAgentID:   "ag_bot",
		ContainerID:  "lc_brain",
		RootFolderID: "lf_brain_root",
	}); !errors.Is(err, ErrBotletsBrainProjectionNotFound) {
		t.Fatalf("stale projection root err = %v", err)
	}
	external := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(filepath.Join(external, "pmf-tracker", "brain"), 0o700); err != nil {
		t.Fatalf("create external brain root: %v", err)
	}
	symlinkAncestor := filepath.Join(root, "Botlets", "max")
	if err := os.RemoveAll(symlinkAncestor); err != nil {
		t.Fatalf("remove projection ancestor: %v", err)
	}
	if err := os.Symlink(external, symlinkAncestor); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := FindBotletsBrainProjection(context.Background(), Options{Home: home}, BotletsBrainProjectionQuery{
		WorkspaceID:  "ws_brain",
		BotAgentID:   "ag_bot",
		ContainerID:  "lc_brain",
		RootFolderID: "lf_brain_root",
	}); !errors.Is(err, ErrBotletsBrainProjectionNotFound) {
		t.Fatalf("symlinked projection ancestor err = %v", err)
	}
}

func TestOnceWritesBotletsBrainProjectionWithLongVisibleID(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_32d995fa5016995a9f9c8401.key.secret-secret-secret"
	longVisibleID := "botlets:" + strings.Repeat("ag_32d995fa5016995a9f9c8401:", 6) + "ln_bs_bot_perm_32d995fa5016995a9f9c8401_agents"
	rows := []map[string]any{
		{
			"visibleInstanceId":        longVisibleID,
			"section":                  "botlets-brains",
			"kind":                     "document",
			"name":                     "AGENTS",
			"parentVisibleInstanceId":  nil,
			"docSlug":                  "agents",
			"botletsOwnerHandle":       "max",
			"botletsBotSlug":           "transcript-cleanup",
			"botletsBotLocalName":      "transcript-cleanup",
			"botletsBotId":             "bot_perm_32d995fa5016995a9f9c8401",
			"botletsBotHandle":         "max.transcript-cleanup",
			"botletsBotAgentId":        "ag_32d995fa5016995a9f9c8401",
			"botletsBrainContainerId":  "lc_bs_bot_perm_32d995fa5016995a9f9c8401_ag_32d995fa5016995a9f9c8401",
			"botletsBrainRootFolderId": "lf_bs_bot_perm_32d995fa5016995a9f9c8401_ag_32d995fa5016995a9f9c8401",
			"botletsBrainNodeId":       "ln_bs_bot_perm_32d995fa5016995a9f9c8401_ag_32d995fa5016995a9f9c8401_agents",
		},
	}
	server := syncTestServer(t, key, rows, map[string]string{"agents": "# Transcript Cleanup\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	legacyBase := filepath.Base(legacyPlacementMetadataPath(paths, longVisibleID))
	if len(legacyBase) <= 255 {
		t.Fatalf("test visible id no longer reproduces the old overlong metadata filename: %d", len(legacyBase))
	}
	result, err := Once(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("once with long Botlets visible id: %v", err)
	}
	if result.DocumentsWritten != 1 {
		t.Fatalf("documents written = %d, want 1", result.DocumentsWritten)
	}
	target := filepath.Join(root, "Botlets", "max", "transcript-cleanup", "brain", "AGENTS.md")
	if got := mustRead(t, target); got != "# Transcript Cleanup\n" {
		t.Fatalf("projection body = %q", got)
	}
	metaPath := placementMetadataPath(paths, longVisibleID)
	if base := filepath.Base(metaPath); len(base) > 90 {
		t.Fatalf("hashed metadata filename is unexpectedly long: %q (%d)", base, len(base))
	}
	var meta placementMeta
	if err := readJSON(metaPath, &meta); err != nil {
		t.Fatalf("read hashed placement metadata: %v", err)
	}
	if meta.VisibleInstanceID != longVisibleID || meta.BotletsBotSlug != "transcript-cleanup" {
		t.Fatalf("metadata did not preserve Botlets identity: %+v", meta)
	}
	entries, err := os.ReadDir(filepath.Dir(metaPath))
	if err != nil {
		t.Fatalf("read metadata dir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() == legacyBase {
			t.Fatalf("legacy overlong metadata filename was written: %s", entry.Name())
		}
	}

	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod long Botlets projection: %v", err)
	}
	if err := os.WriteFile(target, []byte("# Locally edited transcript cleanup\n"), 0o644); err != nil {
		t.Fatalf("dirty long Botlets projection: %v", err)
	}
	result, err = Once(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("once recovering long Botlets visible id: %v", err)
	}
	if result.RecoveriesPreserved != 1 {
		t.Fatalf("recoveries preserved = %d, want 1", result.RecoveriesPreserved)
	}
	if got := mustRead(t, target); got != "# Transcript Cleanup\n" {
		t.Fatalf("projection body after dirty recovery = %q", got)
	}
	recoveries, err := ListRecoveries(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("list long Botlets recoveries: %v", err)
	}
	if len(recoveries) != 1 {
		t.Fatalf("recoveries = %d, want 1", len(recoveries))
	}
	recoveryBase := filepath.Base(recoveries[0].ArtifactPath)
	if len(recoveryBase) > 120 {
		t.Fatalf("hashed recovery filename is unexpectedly long: %q (%d)", recoveryBase, len(recoveryBase))
	}
	if !strings.HasPrefix(recoveries[0].ID, hashedPathID(longVisibleID)+"-") {
		t.Fatalf("recovery id did not use hashed visible id prefix: %q", recoveries[0].ID)
	}
	if strings.HasPrefix(recoveries[0].ID, encodePathID(longVisibleID)+"-") {
		t.Fatalf("recovery id used legacy overlong visible id prefix: %q", recoveries[0].ID)
	}
	if recoveries[0].VisibleID != longVisibleID || recoveries[0].Reason != "local_dirty_before_overwrite" {
		t.Fatalf("recovery metadata did not preserve Botlets identity: %+v", recoveries[0])
	}
}

func TestOnceRepairsHeaderOnlyEditWithoutRecovery(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("first once: %v", err)
	}
	path := filepath.Join(root, "My Files", "Launch.md")
	raw := mustReadRaw(t, path)
	editedHeader := strings.Replace(raw, "read-only: true", "read-only: false", 1)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod header edit: %v", err)
	}
	if err := os.WriteFile(path, []byte(editedHeader), 0o644); err != nil {
		t.Fatalf("write header edit: %v", err)
	}
	result, err := Once(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("second once: %v", err)
	}
	if result.RecoveriesPreserved != 0 {
		t.Fatalf("header-only edit should not recover: %+v", result)
	}
	if raw := mustReadRaw(t, path); !strings.Contains(raw, "read-only: true") || strings.Contains(raw, "read-only: false") {
		t.Fatalf("header-only edit was not repaired:\n%s", raw)
	}
}

func TestOnceRepairsMalformedHeaderOnlyEditWithoutRecovery(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("first once: %v", err)
	}
	path := filepath.Join(root, "My Files", "Launch.md")
	raw := mustReadRaw(t, path)
	editedHeader := strings.Replace(raw, "revision: 1", "revision: typo", 1)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod malformed header edit: %v", err)
	}
	if err := os.WriteFile(path, []byte(editedHeader), 0o644); err != nil {
		t.Fatalf("write malformed header edit: %v", err)
	}
	result, err := Once(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("second once: %v", err)
	}
	if result.RecoveriesPreserved != 0 {
		t.Fatalf("malformed header-only edit should not recover: %+v", result)
	}
	if raw := mustReadRaw(t, path); !strings.Contains(raw, "revision: 1") || strings.Contains(raw, "revision: typo") {
		t.Fatalf("malformed header-only edit was not repaired:\n%s", raw)
	}
}

func TestAllocatePathsUsesStableUniqueCollisionSuffixes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Comment Docs")
	rows := []snapshotRow{
		{VisibleInstanceID: "doc-a", Section: "my-files", Kind: "document", Name: "Launch", DocSlug: "aaa"},
		{VisibleInstanceID: "doc-b", Section: "my-files", Kind: "document", Name: "Launch", DocSlug: "bbb"},
		{VisibleInstanceID: "doc-c", Section: "my-files", Kind: "document", Name: "Launch-" + shortStableSuffix("doc-b"), DocSlug: "ccc"},
	}
	first := allocatePaths(root, rows)
	second := allocatePaths(root, rows)
	reversedRows := append([]snapshotRow(nil), rows...)
	for left, right := 0, len(reversedRows)-1; left < right; left, right = left+1, right-1 {
		reversedRows[left], reversedRows[right] = reversedRows[right], reversedRows[left]
	}
	reversed := allocatePaths(root, reversedRows)
	seen := map[string]bool{}
	for _, row := range rows {
		if first[row.VisibleInstanceID] == "" {
			t.Fatalf("missing path for %s", row.VisibleInstanceID)
		}
		if first[row.VisibleInstanceID] != second[row.VisibleInstanceID] {
			t.Fatalf("path for %s was not stable: %s vs %s", row.VisibleInstanceID, first[row.VisibleInstanceID], second[row.VisibleInstanceID])
		}
		if first[row.VisibleInstanceID] != reversed[row.VisibleInstanceID] {
			t.Fatalf("path for %s depended on row order: %s vs %s", row.VisibleInstanceID, first[row.VisibleInstanceID], reversed[row.VisibleInstanceID])
		}
		key := strings.ToLower(first[row.VisibleInstanceID])
		if seen[key] {
			t.Fatalf("duplicate allocated path: %s", first[row.VisibleInstanceID])
		}
		seen[key] = true
	}
}

func TestAllocatePathsPlacesBotletsBrainsUnderBotletsRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Comment Docs")
	parent := "brain-folder"
	rows := []snapshotRow{
		{
			VisibleInstanceID:        "brain-doc-1",
			Section:                  "botlets-brains",
			Kind:                     "document",
			Name:                     "Identity",
			DocSlug:                  "identity",
			BotletsOwnerHandle:       "max",
			BotletsBotSlug:           "pmf-tracker",
			BotletsBotLocalName:      "pmf-tracker",
			BotletsBotHandle:         "max.pmf-tracker",
			BotletsBotAgentID:        "ag_bot",
			BotletsBrainContainerID:  "lc_brain",
			BotletsBrainRootFolderID: "lf_brain_root",
			BotletsBrainNodeID:       "ln_identity",
		},
		{
			VisibleInstanceID:        parent,
			Section:                  "botlets-brains",
			Kind:                     "folder",
			Name:                     "Research",
			BotletsOwnerHandle:       "max",
			BotletsBotSlug:           "pmf-tracker",
			BotletsBotLocalName:      "pmf-tracker",
			BotletsBotHandle:         "max.pmf-tracker",
			BotletsBotAgentID:        "ag_bot",
			BotletsBrainContainerID:  "lc_brain",
			BotletsBrainRootFolderID: "lf_brain_root",
			BotletsBrainNodeID:       "ln_research",
		},
		{
			VisibleInstanceID:        "brain-doc-2",
			Section:                  "botlets-brains",
			Kind:                     "document",
			Name:                     "Signals",
			ParentVisibleInstance:    &parent,
			DocSlug:                  "signals",
			BotletsOwnerHandle:       "max",
			BotletsBotSlug:           "pmf-tracker",
			BotletsBotLocalName:      "pmf-tracker",
			BotletsBotHandle:         "max.pmf-tracker",
			BotletsBotAgentID:        "ag_bot",
			BotletsBrainContainerID:  "lc_brain",
			BotletsBrainRootFolderID: "lf_brain_root",
			BotletsBrainNodeID:       "ln_signals",
		},
		{
			VisibleInstanceID: "personal-doc",
			Section:           "my-files",
			Kind:              "document",
			Name:              "Identity",
			DocSlug:           "personal",
		},
		{
			VisibleInstanceID: "team-doc",
			Section:           "team-wiki",
			Kind:              "document",
			Name:              "Playbook",
			DocSlug:           "team",
		},
	}

	paths := allocatePaths(root, rows)
	if got, want := paths["brain-doc-1"], filepath.Join(root, "Botlets", "max", "pmf-tracker", "brain", "Identity.md"); got != want {
		t.Fatalf("brain root document path = %q, want %q", got, want)
	}
	if got, want := paths[parent], filepath.Join(root, "Botlets", "max", "pmf-tracker", "brain", "Research"); got != want {
		t.Fatalf("brain folder path = %q, want %q", got, want)
	}
	if got, want := paths["brain-doc-2"], filepath.Join(root, "Botlets", "max", "pmf-tracker", "brain", "Research", "Signals.md"); got != want {
		t.Fatalf("nested brain document path = %q, want %q", got, want)
	}
	if got := paths["personal-doc"]; filepath.Dir(got) != filepath.Join(root, "My Files") {
		t.Fatalf("personal row was not kept under My Files: %q", got)
	}
	if got, want := paths["team-doc"], filepath.Join(root, "Team Wiki", "Playbook.md"); got != want {
		t.Fatalf("team wiki document path = %q, want %q", got, want)
	}
}

func TestAllocatePathsGroupsBotletsBrainsByBotIDBeforeMutableLabels(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Comment Docs")
	rows := []snapshotRow{
		{
			VisibleInstanceID:   "old-label-doc",
			Section:             "botlets-brains",
			Kind:                "document",
			Name:                "Old Label",
			DocSlug:             "old-label",
			BotletsOwnerHandle:  "max",
			BotletsBotSlug:      "old-reviewer",
			BotletsBotLocalName: "old-reviewer",
			BotletsBotID:        "bot_reviewer",
		},
		{
			VisibleInstanceID:   "new-label-doc",
			Section:             "botlets-brains",
			Kind:                "document",
			Name:                "New Label",
			DocSlug:             "new-label",
			BotletsOwnerHandle:  "max",
			BotletsBotSlug:      "reviewer",
			BotletsBotLocalName: "reviewer",
			BotletsBotID:        "bot_reviewer",
			BotletsBotAgentID:   "ag_reviewer",
		},
	}
	paths := allocatePaths(root, rows)
	oldRoot := filepath.Dir(paths["old-label-doc"])
	newRoot := filepath.Dir(paths["new-label-doc"])
	if oldRoot != newRoot {
		t.Fatalf("same bot_id rows allocated different brain roots: old=%s new=%s", oldRoot, newRoot)
	}
	if want := filepath.Join(root, "Botlets", "max", "old-reviewer", "brain"); oldRoot != want {
		t.Fatalf("brain root = %q, want deterministic first bot_id group label %q", oldRoot, want)
	}
}

func TestOnceAvoidsUnknownExistingFiles(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	unknownPath := filepath.Join(root, "My Files", "Launch.md")
	if err := os.MkdirAll(filepath.Dir(unknownPath), 0o755); err != nil {
		t.Fatalf("mkdir unknown parent: %v", err)
	}
	if err := os.WriteFile(unknownPath, []byte("user file\n"), 0o644); err != nil {
		t.Fatalf("write unknown: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	if got := mustRead(t, unknownPath); got != "user file\n" {
		t.Fatalf("unknown file was overwritten: %q", got)
	}
	suffixed := filepath.Join(root, "My Files", "Launch-"+shortStableSuffix("doc-1")+".md")
	if got := mustRead(t, suffixed); got != "# Launch\n" {
		t.Fatalf("suffixed projection = %q", got)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("second once: %v", err)
	}
	if got := mustRead(t, suffixed); got != "# Launch\n" {
		t.Fatalf("suffixed projection after second sync = %q", got)
	}
	churned := filepath.Join(root, "My Files", "Launch-"+shortStableSuffix("doc-1")+"-2.md")
	if _, err := os.Stat(churned); !os.IsNotExist(err) {
		t.Fatalf("projection path churned to %s, err=%v", churned, err)
	}
}

func TestOnceDoesNotOverwriteUnknownRootFiles(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{}, map[string]string{})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("user readme\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	if got := mustRead(t, filepath.Join(root, "README.md")); got != "user readme\n" {
		t.Fatalf("unknown README was overwritten: %q", got)
	}
	if got := mustRead(t, filepath.Join(root, ".comment-sync-root.json")); !strings.Contains(got, `"managed_by": "comment sync"`) {
		t.Fatalf("marker was not written: %q", got)
	}

	otherRoot := filepath.Join(t.TempDir(), "Other Root")
	if _, err := Login(context.Background(), Options{Home: home, Root: otherRoot, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("second login: %v", err)
	}
	if err := os.MkdirAll(otherRoot, 0o755); err != nil {
		t.Fatalf("mkdir other root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(otherRoot, ".comment-sync-root.json"), []byte(`{"managed_by":"someone else"}`), 0o644); err != nil {
		t.Fatalf("write foreign marker: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err == nil || !strings.Contains(err.Error(), "not managed") {
		t.Fatalf("expected foreign marker error, got %v", err)
	}
}

func TestManagedWritesDoNotOverwriteUnknownTempFiles(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	tempSibling := filepath.Join(root, "My Files", "Launch.md.tmp")
	if err := os.MkdirAll(filepath.Dir(tempSibling), 0o755); err != nil {
		t.Fatalf("mkdir temp sibling parent: %v", err)
	}
	if err := os.WriteFile(tempSibling, []byte("user temp file\n"), 0o644); err != nil {
		t.Fatalf("write temp sibling: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	if got := mustRead(t, tempSibling); got != "user temp file\n" {
		t.Fatalf("unknown temp sibling was overwritten: %q", got)
	}
}

func TestOnceMovesCleanProjectionAndRecoversDirtyOldPath(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	rows := []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}
	markdown := map[string]string{"abc123": "# Launch\n"}
	server := syncTestServer(t, key, rows, markdown)
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("first once: %v", err)
	}
	oldPath := filepath.Join(root, "My Files", "Launch.md")
	rows[0]["name"] = "Renamed"
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("rename once: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("clean old path was not removed, err=%v", err)
	}
	newPath := filepath.Join(root, "My Files", "Renamed.md")
	if got := mustRead(t, newPath); got != "# Launch\n" {
		t.Fatalf("renamed projection = %q", got)
	}
	if err := os.Chmod(newPath, 0o644); err != nil {
		t.Fatalf("chmod dirty: %v", err)
	}
	if err := os.WriteFile(newPath, []byte("# Dirty rename\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}
	rows[0]["name"] = "Final"
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("dirty move once: %v", err)
	}
	recoveries, err := os.ReadDir(filepath.Join(home, "sync", "recovery"))
	if err != nil {
		t.Fatalf("read recoveries: %v", err)
	}
	if countRecoveryMarkdown(recoveries) != 1 {
		t.Fatalf("expected one dirty move recovery, got %d", countRecoveryMarkdown(recoveries))
	}
	if got := mustRead(t, filepath.Join(root, "My Files", "Final.md")); got != "# Launch\n" {
		t.Fatalf("final projection = %q", got)
	}
}

func TestOncePlacesGenericSharedWithMeSourceAtSectionRoot(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	rows := []map[string]any{
		{
			"visibleInstanceId":       "shared-1",
			"section":                 "shared-with-me",
			"kind":                    "document",
			"name":                    "GTM Demo Milestone",
			"parentVisibleInstanceId": nil,
			"docSlug":                 "def456",
		},
	}
	server := syncTestServer(t, key, rows, map[string]string{"def456": "# Shared\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	directPath := filepath.Join(root, "Shared With Me", "GTM Demo Milestone.md")
	if got := mustRead(t, directPath); got != "# Shared\n" {
		t.Fatalf("shared markdown = %q", got)
	}
	nestedGenericPath := filepath.Join(root, "Shared With Me", "Shared with me", "GTM Demo Milestone.md")
	if _, err := os.Stat(nestedGenericPath); !os.IsNotExist(err) {
		t.Fatalf("generic source label created nested folder, err=%v", err)
	}
}

func TestOncePlacesPresentSharedWithMeSourceInSourceFolder(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	rows := []map[string]any{
		{
			"visibleInstanceId":       "shared-1",
			"section":                 "shared-with-me",
			"kind":                    "document",
			"name":                    "Shared Spec",
			"parentVisibleInstanceId": nil,
			"docSlug":                 "def456",
			"sourceLabel":             "Shared with me",
		},
	}
	server := syncTestServer(t, key, rows, map[string]string{"def456": "# Shared\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	sourcePath := filepath.Join(root, "Shared With Me", "Shared with me", "Shared Spec.md")
	if got := mustRead(t, sourcePath); got != "# Shared\n" {
		t.Fatalf("shared markdown = %q", got)
	}
}

func TestOncePlacesBlankSharedSourceInStableFallbackFolder(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	rows := []map[string]any{
		{
			"visibleInstanceId":       "shared-1",
			"section":                 "shared-with-me",
			"kind":                    "document",
			"name":                    "Shared Spec",
			"parentVisibleInstanceId": nil,
			"docSlug":                 "def456",
			"sourceLabel":             " ",
		},
	}
	server := syncTestServer(t, key, rows, map[string]string{"def456": "# Shared\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	fallbackPath := filepath.Join(root, "Shared With Me", "Shared", "Shared Spec.md")
	if got := mustRead(t, fallbackPath); got != "# Shared\n" {
		t.Fatalf("shared markdown = %q", got)
	}
}

func TestOnceMovesCleanSharedProjectionWhenSourceBecomesGeneric(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	rows := []map[string]any{
		{
			"visibleInstanceId":       "shared-1",
			"section":                 "shared-with-me",
			"kind":                    "document",
			"name":                    "Shared Spec",
			"parentVisibleInstanceId": nil,
			"docSlug":                 "def456",
			"sourceLabel":             "Max",
		},
	}
	server := syncTestServer(t, key, rows, map[string]string{"def456": "# Shared\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("first once: %v", err)
	}
	oldPath := filepath.Join(root, "Shared With Me", "Max", "Shared Spec.md")
	if got := mustRead(t, oldPath); got != "# Shared\n" {
		t.Fatalf("old shared markdown = %q", got)
	}
	delete(rows[0], "sourceLabel")
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("move once: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("clean old shared path was not removed, err=%v", err)
	}
	newPath := filepath.Join(root, "Shared With Me", "Shared Spec.md")
	if got := mustRead(t, newPath); got != "# Shared\n" {
		t.Fatalf("new shared markdown = %q", got)
	}
}

func TestPreserveRecoveryUsesUniqueNames(t *testing.T) {
	home := t.TempDir()
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := ensurePrivateDirs(paths); err != nil {
		t.Fatalf("ensure private dirs: %v", err)
	}
	state, err := openSyncState(context.Background(), paths)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()
	if err := preserveRecovery(context.Background(), paths, state, "doc-1", "abc123", "/tmp/original.md", "test", []byte("first")); err != nil {
		t.Fatalf("first recovery: %v", err)
	}
	if err := preserveRecovery(context.Background(), paths, state, "doc-1", "abc123", "/tmp/original.md", "test", []byte("second")); err != nil {
		t.Fatalf("second recovery: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(home, "sync", "recovery"))
	if err != nil {
		t.Fatalf("read recovery dir: %v", err)
	}
	if countRecoveryMarkdown(entries) != 2 {
		t.Fatalf("expected two recovery files, got %d", countRecoveryMarkdown(entries))
	}
}

func countRecoveryMarkdown(entries []os.DirEntry) int {
	count := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".local.md") {
			count++
		}
	}
	return count
}

func TestOnceRejectsNonAuthoritativeSnapshot(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/auth/library-sync/snapshot" {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, map[string]any{
			"snapshotId": "lse_partial",
			"scopeLabel": "My Files and Shared With Me",
			"coveredSections": []map[string]any{
				{"id": "my-files", "label": "My Files", "covered": true, "authoritative": false, "count": 0, "reason": "partial"},
			},
			"unsupportedSections": []map[string]any{},
			"snapshotComplete":    false,
			"rows":                []map[string]any{},
			"pageInfo":            map[string]any{"nextCursor": nil, "partial": false},
		})
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err == nil || !strings.Contains(err.Error(), "not complete") {
		t.Fatalf("expected partial snapshot error, got %v", err)
	}
}

func TestOnceUsesFinalIncompleteMetadataBeforeAbsentRemoval(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	mode := "seed"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			if mode == "seed" {
				writeJSON(t, w, map[string]any{
					"snapshotId": "lse_seed",
					"scopeLabel": "My Files and Shared With Me",
					"coveredSections": []map[string]any{
						{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 1},
					},
					"unsupportedSections": []map[string]any{},
					"snapshotComplete":    true,
					"rows": []map[string]any{
						{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
					},
					"pageInfo": map[string]any{"nextCursor": nil, "partial": false},
				})
				return
			}
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_next",
				"scopeLabel": "dangerous first metadata",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 0},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows":                []map[string]any{},
				"pageInfo":            map[string]any{"nextCursor": "final", "partial": true},
			})
		case "/auth/library-sync/snapshot/lse_next/pages/final":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_next",
				"scopeLabel": "final incomplete metadata",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": false, "count": 0, "reason": "still building"},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    false,
				"rows":                []map[string]any{},
				"pageInfo":            map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/docs/abc123":
			writeJSON(t, w, projection("abc123", "# Launch\n", 1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("seed once: %v", err)
	}
	target := filepath.Join(root, "My Files", "Launch.md")
	if got := mustRead(t, target); got != "# Launch\n" {
		t.Fatalf("seed projection = %q", got)
	}

	mode = "next"
	if _, err := Once(context.Background(), Options{Home: home}); err == nil || !strings.Contains(err.Error(), "not complete") {
		t.Fatalf("expected incomplete final metadata error, got %v", err)
	}
	if got := mustRead(t, target); got != "# Launch\n" {
		t.Fatalf("projection should survive incomplete final metadata, got %q", got)
	}
}

func TestOnceAllowsAbsentRemovalAfterFinalCompleteMetadata(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	mode := "seed"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			if mode == "seed" {
				writeJSON(t, w, map[string]any{
					"snapshotId": "lse_seed",
					"scopeLabel": "My Files and Shared With Me",
					"coveredSections": []map[string]any{
						{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 1},
					},
					"unsupportedSections": []map[string]any{},
					"snapshotComplete":    true,
					"rows": []map[string]any{
						{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
					},
					"pageInfo": map[string]any{"nextCursor": nil, "partial": false},
				})
				return
			}
			writeJSON(t, w, map[string]any{
				"snapshotId":          "lse_final_delete",
				"scopeLabel":          "first incomplete metadata",
				"coveredSections":     []map[string]any{},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    false,
				"rows":                []map[string]any{},
				"pageInfo":            map[string]any{"nextCursor": "final", "partial": true},
			})
		case "/auth/library-sync/snapshot/lse_final_delete/pages/final":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_final_delete",
				"scopeLabel": "final complete metadata",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 0},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows":                []map[string]any{},
				"pageInfo":            map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/docs/abc123":
			writeJSON(t, w, projection("abc123", "# Launch\n", 1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("seed once: %v", err)
	}
	target := filepath.Join(root, "My Files", "Launch.md")
	if got := mustRead(t, target); got != "# Launch\n" {
		t.Fatalf("seed projection = %q", got)
	}

	mode = "next"
	result, err := Once(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("delete once: %v", err)
	}
	if result.DocumentsRemoved != 1 || result.ScopeLabel != "final complete metadata" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("expected projection to be removed after final authoritative metadata, err=%v", err)
	}
}

func TestOnceRejectsStaleSnapshotContentHash(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{
			"visibleInstanceId":       "doc-1",
			"section":                 "my-files",
			"kind":                    "document",
			"name":                    "Launch",
			"parentVisibleInstanceId": nil,
			"docSlug":                 "abc123",
			"revision":                1,
			"contentHash":             strings.Repeat("0", 64),
		},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err == nil || !strings.Contains(err.Error(), "content hash") {
		t.Fatalf("expected content hash mismatch, got %v", err)
	}
}

func TestOnceRemovesAbsentCleanProjectionAndRecoversDirtyAbsentProjection(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	rows := []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_test",
				"scopeLabel": "My Files and Shared With Me",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": len(rows)},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows":                rows,
				"pageInfo":            map[string]any{"nextCursor": nil, "partial": false},
			})
		case r.URL.Path == "/docs/abc123":
			writeJSON(t, w, projection("abc123", "# Launch\n", 1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("first once: %v", err)
	}
	cleanPath := filepath.Join(root, "My Files", "Launch.md")
	rows = []map[string]any{}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("remove clean once: %v", err)
	}
	if _, err := os.Stat(cleanPath); !os.IsNotExist(err) {
		t.Fatalf("clean absent projection was not removed: %v", err)
	}
	status, err := ReadStatus(Options{Home: home})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Documents != 0 {
		t.Fatalf("documents after removal = %d", status.Documents)
	}

	rows = []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("restore once: %v", err)
	}
	if err := os.Chmod(cleanPath, 0o644); err != nil {
		t.Fatalf("chmod dirty: %v", err)
	}
	if err := os.WriteFile(cleanPath, []byte("# Dirty absent\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}
	rows = []map[string]any{}
	result, err := Once(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("remove dirty once: %v", err)
	}
	if result.RecoveriesPreserved != 1 || result.DocumentsRemoved != 1 {
		t.Fatalf("unexpected remove dirty result: %+v", result)
	}
	if _, err := os.Stat(cleanPath); !os.IsNotExist(err) {
		t.Fatalf("dirty absent projection was not removed after recovery: %v", err)
	}
	recoveries, err := ListRecoveries(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("list recoveries: %v", err)
	}
	if len(recoveries) != 1 || recoveries[0].Reason != "local_dirty_before_absent_removal" {
		t.Fatalf("recoveries = %+v", recoveries)
	}
}

func TestOnceOnlyRemovesAbsentPlacementsFromCoveredSections(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	rows := []map[string]any{
		{
			"visibleInstanceId":        "brain-doc-1",
			"section":                  "botlets-brains",
			"kind":                     "document",
			"name":                     "Memory",
			"parentVisibleInstanceId":  nil,
			"docSlug":                  "brain123",
			"botletsOwnerHandle":       "max",
			"botletsBotSlug":           "research",
			"botletsBotLocalName":      "research",
			"botletsBotAgentId":        "ag_bot_research",
			"botletsBrainContainerId":  "lc_brain",
			"botletsBrainRootFolderId": "lf_brain_root",
			"botletsBrainNodeId":       "ln_brain_memory",
		},
	}
	coveredSections := []map[string]any{
		{"id": "botlets-brains", "label": "Botlets brains", "covered": true, "authoritative": true, "count": 1},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId":          "lse_test",
				"scopeLabel":          "My Files, Shared With Me, Team Wiki, and Botlets brains",
				"coveredSections":     coveredSections,
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows":                rows,
				"pageInfo":            map[string]any{"nextCursor": nil, "partial": false},
			})
		case r.URL.Path == "/docs/brain123":
			writeJSON(t, w, projection("brain123", "# Memory\n", 1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("first once: %v", err)
	}
	brainPath := filepath.Join(root, "Botlets", "max", "research", "brain", "Memory.md")
	if got := mustRead(t, brainPath); got != "# Memory\n" {
		t.Fatalf("brain projection = %q", got)
	}

	rows = []map[string]any{}
	coveredSections = []map[string]any{
		{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 0},
		{"id": "shared-with-me", "label": "Shared With Me", "covered": true, "authoritative": true, "count": 0},
	}
	legacyResult, err := Once(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("legacy once: %v", err)
	}
	if legacyResult.DocumentsRemoved != 0 {
		t.Fatalf("legacy uncovered snapshot removed documents: %+v", legacyResult)
	}
	if got := mustRead(t, brainPath); got != "# Memory\n" {
		t.Fatalf("brain projection was removed by uncovered snapshot: %q", got)
	}

	coveredSections = []map[string]any{
		{"id": "botlets-brains", "label": "Botlets brains", "covered": true, "authoritative": true, "count": 0},
	}
	removeResult, err := Once(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("authoritative brain removal once: %v", err)
	}
	if removeResult.DocumentsRemoved != 1 {
		t.Fatalf("authoritative brain snapshot did not remove absent projection: %+v", removeResult)
	}
	if _, err := os.Stat(brainPath); !os.IsNotExist(err) {
		t.Fatalf("brain projection still exists after authoritative empty brain section: %v", err)
	}
}

func TestRecoverCopyDiffAndDiscard(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("first once: %v", err)
	}
	path := filepath.Join(root, "My Files", "Launch.md")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	if err := os.WriteFile(path, []byte("# Local edit\n"), 0o644); err != nil {
		t.Fatalf("dirty write: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("second once: %v", err)
	}
	recoveries, err := ListRecoveries(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(recoveries) != 1 {
		t.Fatalf("recoveries = %+v", recoveries)
	}
	diff, err := Recover(context.Background(), Options{Home: home}, recoveries[0].ID, RecoverActionDiff)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !strings.Contains(diff.Diff, "# Local edit") {
		t.Fatalf("diff did not include local edit: %q", diff.Diff)
	}
	if strings.Contains(diff.Diff, "comment.io:projection") {
		t.Fatalf("diff should not include generated projection header: %q", diff.Diff)
	}
	copied, err := Recover(context.Background(), Options{Home: home}, recoveries[0].ID, RecoverActionCopyNextToOriginal)
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if got := mustRead(t, copied.OutputPath); got != "# Local edit\n" {
		t.Fatalf("copied recovery = %q", got)
	}
	discarded, err := Recover(context.Background(), Options{Home: home}, recoveries[0].ID, RecoverActionDiscard)
	if err != nil {
		t.Fatalf("discard: %v", err)
	}
	if !discarded.Discarded {
		t.Fatalf("discard result = %+v", discarded)
	}
	recoveries, err = ListRecoveries(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("list after discard: %v", err)
	}
	if len(recoveries) != 0 {
		t.Fatalf("recoveries after discard = %+v", recoveries)
	}
}

func TestSyncRejectsSymlinkProjectionAndRootReuseAcrossHomes(t *testing.T) {
	home := t.TempDir()
	otherHome := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("first once: %v", err)
	}
	if _, err := Login(context.Background(), Options{Home: otherHome, Root: root, BaseURL: server.URL, APIKey: key}); err == nil || !strings.Contains(err.Error(), "state_home_id") {
		t.Fatalf("expected root ownership error, got %v", err)
	}
	path := filepath.Join(root, "My Files", "Launch.md")
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove projection: %v", err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside.md"), path); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink refusal, got %v", err)
	}
}

func TestOnceRejectsSymlinkRootBeforeWritingRootFiles(t *testing.T) {
	home := t.TempDir()
	outside := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{}, map[string]string{})
	defer server.Close()

	if err := os.Symlink(outside, root); err != nil {
		t.Fatalf("symlink root: %v", err)
	}
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := ensurePrivateDirs(paths); err != nil {
		t.Fatalf("ensure private dirs: %v", err)
	}
	cfg := Config{
		Version:          configVersion,
		BaseURL:          server.URL,
		Root:             root,
		Scope:            "library-sync:read",
		ScopeLabel:       "My Files and Shared With Me",
		HumanID:          "ag_test",
		KeyID:            "key",
		ConfigGeneration: 1,
	}
	creds := Credentials{Version: configVersion, APIKey: key}
	if err := writeJSON0600(filepath.Join(home, "sync", "config.json"), cfg); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := writeJSON0600(filepath.Join(home, "sync", "credentials.json"), creds); err != nil {
		t.Fatalf("write creds: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err == nil || !strings.Contains(err.Error(), "sync root refuses symlink") {
		t.Fatalf("expected symlink root refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "README.md")); !os.IsNotExist(err) {
		t.Fatalf("root README was written through symlink, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, ".comment-sync-root.json")); !os.IsNotExist(err) {
		t.Fatalf("root marker was written through symlink, err=%v", err)
	}
}

func TestOnceRejectsSymlinkOldPathBeforeMoveRecovery(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	rows := []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}
	server := syncTestServer(t, key, rows, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("first once: %v", err)
	}
	oldPath := filepath.Join(root, "My Files", "Launch.md")
	if err := os.Remove(oldPath); err != nil {
		t.Fatalf("remove old path: %v", err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "outside.md"), oldPath); err != nil {
		t.Fatalf("symlink old path: %v", err)
	}
	rows[0]["name"] = "Renamed"
	if _, err := Once(context.Background(), Options{Home: home}); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("expected symlink old path refusal, got %v", err)
	}
}

func TestReplayIncompleteProjectionWriteAvoidsDuplicateProjection(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	state, err := openSyncState(context.Background(), paths)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	target := filepath.Join(root, "My Files", "Launch.md")
	op, err := state.beginOp(context.Background(), "write_projection", "doc-1", "abc123", target, "lse_crash")
	if err != nil {
		t.Fatalf("begin op: %v", err)
	}
	if op.State != "started" {
		t.Fatalf("unexpected op: %+v", op)
	}
	if err := atomicWriteFile(target, []byte("# Partial crash write\n"), 0o444); err != nil {
		t.Fatalf("partial write: %v", err)
	}
	if err := state.Close(); err != nil {
		t.Fatalf("close state: %v", err)
	}

	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once after replay: %v", err)
	}
	if got := mustRead(t, target); got != "# Launch\n" {
		t.Fatalf("canonical path was not rewritten after replay: %q", got)
	}
	suffixed := filepath.Join(root, "My Files", "Launch-"+shortStableSuffix("doc-1")+".md")
	if _, err := os.Stat(suffixed); !os.IsNotExist(err) {
		t.Fatalf("replay created duplicate suffixed projection, err=%v", err)
	}
	recoveries, err := ListRecoveries(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("list recoveries: %v", err)
	}
	if len(recoveries) != 1 || recoveries[0].Reason != "incomplete_write_replayed" {
		t.Fatalf("recoveries = %+v", recoveries)
	}
}

func TestRefreshProjectionUpdatesExistingPlacementWithETag(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	markdown := "# Launch\n"
	revision := 1
	var ifNoneMatch string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_test",
				"scopeLabel": "My Files and Shared With Me",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 1},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows": []map[string]any{
					{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
				},
				"pageInfo": map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/docs/abc123":
			ifNoneMatch = r.Header.Get("If-None-Match")
			body := projection("abc123", markdown, revision)
			body["etag"] = "etag-live"
			w.Header().Set("ETag", "etag-live")
			writeJSON(t, w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	markdown = "# Launch\n\nLive update.\n"
	revision = 2
	result, err := RefreshProjection(context.Background(), Options{Home: home}, LiveEvent{
		Type:        "document_projection_changed",
		Cursor:      "cur_0000000000000001",
		Slug:        "abc123",
		Revision:    2,
		ContentHash: sha256Hex(markdown),
	})
	if err != nil {
		t.Fatalf("refresh projection: %v", err)
	}
	if result.Refreshed != 1 || result.NotModified != 0 || result.NeedsSnapshot {
		t.Fatalf("unexpected refresh result: %+v", result)
	}
	if ifNoneMatch != "etag-live" {
		t.Fatalf("expected If-None-Match from placement etag, got %q", ifNoneMatch)
	}
	if got := mustRead(t, filepath.Join(root, "My Files", "Launch.md")); got != markdown {
		t.Fatalf("live projection body = %q", got)
	}
}

func TestRefreshProjectionHandlesNotModified(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	var notModifiedRequest bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_test",
				"scopeLabel": "My Files and Shared With Me",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 1},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows": []map[string]any{
					{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
				},
				"pageInfo": map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/docs/abc123":
			if r.Header.Get("If-None-Match") == "test" {
				notModifiedRequest = true
				w.WriteHeader(http.StatusNotModified)
				return
			}
			writeJSON(t, w, projection("abc123", "# Launch\n", 1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	result, err := RefreshProjection(context.Background(), Options{Home: home}, LiveEvent{
		Type:     "document_projection_changed",
		Cursor:   "cur_0000000000000001",
		Slug:     "abc123",
		Revision: 1,
	})
	if err != nil {
		t.Fatalf("refresh projection: %v", err)
	}
	if !notModifiedRequest || result.NotModified != 1 || result.Refreshed != 0 || result.NeedsSnapshot {
		t.Fatalf("unexpected not-modified result: %+v notModifiedRequest=%v", result, notModifiedRequest)
	}
}

func TestRefreshProjectionAcceptsAlreadyAdvancedProjection(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	markdown := "# Launch\n"
	revision := 1
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_test",
				"scopeLabel": "My Files and Shared With Me",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 1},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows": []map[string]any{
					{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
				},
				"pageInfo": map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/docs/abc123":
			writeJSON(t, w, projection("abc123", markdown, revision))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	oldHash := sha256Hex(markdown)
	markdown = "# Launch\n\nAlready advanced.\n"
	revision = 2
	result, err := RefreshProjection(context.Background(), Options{Home: home}, LiveEvent{
		Type:        "document_projection_changed",
		Cursor:      "cur_0000000000000001",
		Slug:        "abc123",
		Revision:    1,
		ContentHash: oldHash,
	})
	if err != nil {
		t.Fatalf("refresh advanced projection: %v", err)
	}
	if result.Refreshed != 1 || result.NeedsSnapshot {
		t.Fatalf("unexpected advanced projection result: %+v", result)
	}
	if got := mustRead(t, filepath.Join(root, "My Files", "Launch.md")); got != markdown {
		t.Fatalf("advanced projection body = %q", got)
	}
}

func TestRefreshProjectionRestoresDirtyLocalFileOnNotModified(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	var conditionalRequests int
	var unconditionalRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_test",
				"scopeLabel": "My Files and Shared With Me",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 1},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows": []map[string]any{
					{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
				},
				"pageInfo": map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/docs/abc123":
			if r.Header.Get("If-None-Match") == "test" {
				conditionalRequests++
				w.WriteHeader(http.StatusNotModified)
				return
			}
			unconditionalRequests++
			writeJSON(t, w, projection("abc123", "# Launch\n", 1))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	target := filepath.Join(root, "My Files", "Launch.md")
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod projection: %v", err)
	}
	if err := os.WriteFile(target, []byte("# Local edit\n"), 0o644); err != nil {
		t.Fatalf("dirty projection: %v", err)
	}
	result, err := RefreshProjection(context.Background(), Options{Home: home}, LiveEvent{
		Type:     "document_projection_changed",
		Cursor:   "cur_0000000000000001",
		Slug:     "abc123",
		Revision: 1,
	})
	if err != nil {
		t.Fatalf("refresh projection: %v", err)
	}
	if conditionalRequests != 1 || unconditionalRequests != 2 || result.Refreshed != 1 || result.NotModified != 0 || result.NeedsSnapshot {
		t.Fatalf("unexpected dirty not-modified result: %+v conditional=%d unconditional=%d", result, conditionalRequests, unconditionalRequests)
	}
	if got := mustRead(t, target); got != "# Launch\n" {
		t.Fatalf("projection body after dirty 304 restore = %q", got)
	}
	recoveries, err := ListRecoveries(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("list recoveries: %v", err)
	}
	if len(recoveries) != 1 || recoveries[0].Reason != "local_dirty_before_overwrite" {
		t.Fatalf("recoveries = %+v", recoveries)
	}
}

func TestRecoverDirtyProjectionsRestoresDirtyProjection(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	target := filepath.Join(root, "My Files", "Launch.md")
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod projection: %v", err)
	}
	if err := os.WriteFile(target, []byte("# Local edit\n"), 0o644); err != nil {
		t.Fatalf("dirty projection: %v", err)
	}

	result, err := RecoverDirtyProjections(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("recover dirty projections: %v", err)
	}
	if result.Checked != 1 || result.ProjectionRefreshes != 1 || result.RecoveriesPreserved != 1 {
		t.Fatalf("unexpected recovery scan result: %+v", result)
	}
	if got := mustRead(t, target); got != "# Launch\n" {
		t.Fatalf("projection body after recovery scan = %q", got)
	}
	recoveries, err := ListRecoveries(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("list recoveries: %v", err)
	}
	if len(recoveries) != 1 || recoveries[0].Reason != "local_dirty_before_overwrite" {
		t.Fatalf("recoveries = %+v", recoveries)
	}
}

func TestRecoverDirtyProjectionsRepairsCleanHeaderAndModeWithoutRecovery(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	target := filepath.Join(root, "My Files", "Launch.md")
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod projection: %v", err)
	}
	raw := mustReadRaw(t, target)
	headerOnly := strings.Replace(raw, "revision: 1", "revision: 999", 1)
	if headerOnly == raw {
		t.Fatal("test projection did not contain revision header")
	}
	if err := os.WriteFile(target, []byte(headerOnly), 0o644); err != nil {
		t.Fatalf("header-only projection edit: %v", err)
	}

	result, err := RecoverDirtyProjections(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("recover dirty projections: %v", err)
	}
	if result.Checked != 1 || result.ProjectionRefreshes != 1 || result.RecoveriesPreserved != 0 {
		t.Fatalf("unexpected recovery scan result: %+v", result)
	}
	if got := mustRead(t, target); got != "# Launch\n" {
		t.Fatalf("projection body after header repair = %q", got)
	}
	if info, err := os.Stat(target); err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("projection mode after header repair = %v err=%v", info.Mode().Perm(), err)
	}
	recoveries, err := ListRecoveries(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("list recoveries: %v", err)
	}
	if len(recoveries) != 0 {
		t.Fatalf("unexpected recoveries after clean header repair: %+v", recoveries)
	}
}

func TestRecoverDirtyProjectionsRestoresDeletedProjection(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := syncTestServer(t, key, []map[string]any{
		{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
	}, map[string]string{"abc123": "# Launch\n"})
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	target := filepath.Join(root, "My Files", "Launch.md")
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove projection: %v", err)
	}

	result, err := RecoverDirtyProjections(context.Background(), Options{Home: home})
	if err != nil {
		t.Fatalf("recover dirty projections: %v", err)
	}
	if result.Checked != 1 || result.ProjectionRefreshes != 1 || result.RecoveriesPreserved != 0 {
		t.Fatalf("unexpected recovery scan result: %+v", result)
	}
	if got := mustRead(t, target); got != "# Launch\n" {
		t.Fatalf("projection body after delete repair = %q", got)
	}
}

func TestLiveCursorPersistsPerKey(t *testing.T) {
	home := t.TempDir()
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := ensurePrivateDirs(paths); err != nil {
		t.Fatalf("ensure private dirs: %v", err)
	}
	first := Config{KeyID: "key-one"}
	second := Config{KeyID: "key-two"}
	if err := writeLiveCursor(paths, first, "cur_0000000000000007"); err != nil {
		t.Fatalf("write first cursor: %v", err)
	}
	if err := writeLiveCursor(paths, second, "cur_0000000000000003"); err != nil {
		t.Fatalf("write second cursor: %v", err)
	}
	firstCursor, err := readLiveCursor(paths, first)
	if err != nil || firstCursor != "cur_0000000000000007" {
		t.Fatalf("first cursor = %q err=%v", firstCursor, err)
	}
	secondCursor, err := readLiveCursor(paths, second)
	if err != nil || secondCursor != "cur_0000000000000003" {
		t.Fatalf("second cursor = %q err=%v", secondCursor, err)
	}
}

func TestLivePingPayloadIncludesDurableCursor(t *testing.T) {
	var payload map[string]string
	if err := json.Unmarshal(livePingPayload("cur_0000000000000007"), &payload); err != nil {
		t.Fatalf("unmarshal ping payload: %v", err)
	}
	if payload["type"] != "ping" || payload["cursor"] != "cur_0000000000000007" {
		t.Fatalf("ping payload = %#v", payload)
	}
	var empty map[string]string
	if err := json.Unmarshal(livePingPayload(""), &empty); err != nil {
		t.Fatalf("unmarshal empty ping payload: %v", err)
	}
	if empty["type"] != "ping" {
		t.Fatalf("empty ping payload = %#v", empty)
	}
	if _, ok := empty["cursor"]; ok {
		t.Fatalf("empty ping payload should omit cursor: %#v", empty)
	}
}

func TestDialLiveWebSocketHonorsClientTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer listener.Close()

	releaseServer := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-releaseServer
	}()

	start := time.Now()
	conn, reader, err := dialLiveWebSocket(
		context.Background(),
		&http.Client{Timeout: 100 * time.Millisecond},
		"ws://"+listener.Addr().String()+"/auth/library-sync/events?v=1",
		"usk_v2.ag_test.key.secret-secret-secret",
	)
	close(releaseServer)
	_ = listener.Close()
	<-serverDone
	if err == nil {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("expected timeout error, got conn=%v reader=%v", conn, reader)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("websocket handshake ignored client timeout: elapsed=%s err=%v", elapsed, err)
	}
}

func TestRunLiveSyncCoalescesDuplicateProjectionEvents(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	markdown := "# One\n"
	revision := 1
	var projectionFetches int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_test",
				"scopeLabel": "My Files and Shared With Me",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 1},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows": []map[string]any{
					{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
				},
				"pageInfo": map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/docs/abc123":
			projectionFetches++
			writeJSON(t, w, projection("abc123", markdown, revision))
		case "/auth/library-sync/events":
			serveLiveEventSocket(t, w, r, []LiveEvent{
				{
					Type:        "document_projection_changed",
					EventID:     "lse_one",
					Cursor:      "cur_0000000000000001",
					Slug:        "abc123",
					Revision:    1,
					ContentHash: sha256Hex("# stale\n"),
					UpdatedAt:   "2026-05-20T12:00:00Z",
				},
				{
					Type:        "document_projection_changed",
					EventID:     "lse_two",
					Cursor:      "cur_0000000000000002",
					Slug:        "abc123",
					Revision:    2,
					ContentHash: sha256Hex("# Two\n"),
					UpdatedAt:   "2026-05-20T12:00:01Z",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := SetBackgroundSync(context.Background(), Options{Home: home}, true); err != nil {
		t.Fatalf("enable background sync: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	markdown = "# Two\n"
	revision = 2
	result, err := RunLiveSync(context.Background(), Options{Home: home})
	if err != nil && err != io.EOF {
		t.Fatalf("run live sync: result=%+v err=%v", result, err)
	}
	if result.EventsProcessed != 2 || result.ProjectionRefreshes != 1 {
		t.Fatalf("expected two events coalesced to one refresh, got %+v", result)
	}
	if projectionFetches != 2 {
		t.Fatalf("expected one initial and one live projection fetch, got %d", projectionFetches)
	}
	if got := mustRead(t, filepath.Join(root, "My Files", "Launch.md")); got != markdown {
		t.Fatalf("live projection body = %q", got)
	}
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	cursor, err := readLiveCursor(paths, Config{KeyID: "key"})
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursor != "cur_0000000000000002" {
		t.Fatalf("cursor = %q", cursor)
	}
}

func TestRunLiveSyncAcknowledgesProjectionCursorsCoveredBySnapshot(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_test",
				"scopeLabel": "My Files and Shared With Me",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 1},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows": []map[string]any{
					{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
				},
				"pageInfo": map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/docs/abc123":
			writeJSON(t, w, projection("abc123", "# Launch\n", 1))
		case "/auth/library-sync/events":
			serveLiveEventSocket(t, w, r, []LiveEvent{
				{
					Type:      "snapshot_invalidated",
					EventID:   "lse_snapshot",
					Cursor:    "cur_0000000000000001",
					UpdatedAt: "2026-05-20T12:00:00Z",
				},
				{
					Type:        "document_projection_changed",
					EventID:     "lse_projection",
					Cursor:      "cur_0000000000000002",
					Slug:        "abc123",
					Revision:    2,
					ContentHash: sha256Hex("# Launch\n"),
					UpdatedAt:   "2026-05-20T12:00:01Z",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := SetBackgroundSync(context.Background(), Options{Home: home}, true); err != nil {
		t.Fatalf("enable background sync: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	result, err := RunLiveSync(context.Background(), Options{Home: home})
	if err != nil && err != io.EOF {
		t.Fatalf("run live sync: result=%+v err=%v", result, err)
	}
	if result.EventsProcessed != 2 || result.SnapshotRefreshes != 1 {
		t.Fatalf("expected snapshot to cover both events, got %+v", result)
	}
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	cursor, err := readLiveCursor(paths, Config{KeyID: "key"})
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursor != "cur_0000000000000002" {
		t.Fatalf("cursor = %q", cursor)
	}
}

func TestRunLiveSyncDoesNotAdvanceHeartbeatCursorBeforePendingWorkFlushes(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	projectionFetches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_test",
				"scopeLabel": "My Files and Shared With Me",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 1},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows": []map[string]any{
					{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
				},
				"pageInfo": map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/docs/abc123":
			projectionFetches++
			if projectionFetches > 1 {
				http.Error(w, "temporary projection failure", http.StatusInternalServerError)
				return
			}
			writeJSON(t, w, projection("abc123", "# Launch\n", 1))
		case "/auth/library-sync/events":
			serveLiveEventSocket(t, w, r, []LiveEvent{
				{
					Type:        "document_projection_changed",
					EventID:     "lse_one",
					Cursor:      "cur_0000000000000001",
					Slug:        "abc123",
					Revision:    2,
					ContentHash: sha256Hex("# Two\n"),
					UpdatedAt:   "2026-05-20T12:00:00Z",
				},
				{
					Type:      "heartbeat",
					EventID:   "lse_heartbeat",
					Cursor:    "cur_0000000000000002",
					UpdatedAt: "2026-05-20T12:00:01Z",
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := Login(context.Background(), Options{Home: home, Root: root, BaseURL: server.URL, APIKey: key}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := SetBackgroundSync(context.Background(), Options{Home: home}, true); err != nil {
		t.Fatalf("enable background sync: %v", err)
	}
	if _, err := Once(context.Background(), Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	result, err := RunLiveSync(context.Background(), Options{Home: home})
	if err == nil {
		t.Fatalf("expected projection refresh error, got result=%+v", result)
	}
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	cursor, err := readLiveCursor(paths, Config{KeyID: "key"})
	if err != nil {
		t.Fatalf("read cursor: %v", err)
	}
	if cursor != "" {
		t.Fatalf("cursor advanced before pending work flushed: %q", cursor)
	}
}

func serveLiveEventSocket(t *testing.T, w http.ResponseWriter, r *http.Request, events []LiveEvent) {
	t.Helper()
	if r.Header.Get("Upgrade") != "websocket" {
		http.Error(w, "missing upgrade", http.StatusBadRequest)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("response writer does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		t.Fatalf("hijack: %v", err)
	}
	go func(conn net.Conn, rw *bufio.ReadWriter) {
		defer conn.Close()
		key := r.Header.Get("Sec-WebSocket-Key")
		acceptHash := sha1.Sum([]byte(key + websocketAcceptGUID))
		accept := base64.StdEncoding.EncodeToString(acceptHash[:])
		_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
		if err := rw.Flush(); err != nil {
			return
		}
		if _, _, err := readWebSocketServerFrame(rw.Reader); err != nil {
			return
		}
		for _, event := range events {
			payload, err := json.Marshal(event)
			if err != nil {
				return
			}
			if err := writeWebSocketServerText(conn, payload); err != nil {
				return
			}
		}
	}(conn, rw)
}

func writeWebSocketServerText(w io.Writer, payload []byte) error {
	if len(payload) > liveMaxFrameBytes {
		return fmt.Errorf("payload too large")
	}
	header := []byte{0x81}
	switch {
	case len(payload) < 126:
		header = append(header, byte(len(payload)))
	case len(payload) <= 0xFFFF:
		header = append(header, 126, byte(len(payload)>>8), byte(len(payload)))
	default:
		return fmt.Errorf("payload too large")
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("write json: %v", err)
	}
}

func syncTestServer(t *testing.T, key string, rows []map[string]any, markdownBySlug map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId": "lse_test",
				"scopeLabel": "My Files and Shared With Me",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": len(rows)},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows":                rows,
				"pageInfo":            map[string]any{"nextCursor": nil, "partial": false},
			})
		case strings.HasPrefix(r.URL.Path, "/docs/"):
			slug := strings.TrimPrefix(r.URL.Path, "/docs/")
			markdown, ok := markdownBySlug[slug]
			if !ok {
				http.NotFound(w, r)
				return
			}
			writeJSON(t, w, projection(slug, markdown, 1))
		default:
			http.NotFound(w, r)
		}
	}))
}

func projection(slug, markdown string, revision int) map[string]any {
	return map[string]any{
		"slug":         slug,
		"title":        slug,
		"markdown":     markdown,
		"revision":     revision,
		"content_hash": sha256Hex(markdown),
		"etag":         "test",
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(projectionBodyForDirtyCheck(data))
}

func mustReadRaw(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func mustMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// TestValidateLibrarySyncAPIKeyTransportTimeoutIsNotKeyValidation is the
// regression test for bug #523: when the snapshot login transport times out
// (context deadline exceeded / Client.Timeout exceeded) the user must see a
// timeout-flavored message that suggests retrying, NOT a message claiming the
// key could not be validated or was rejected. The key was never evaluated by
// the server when the transport never delivered a response.
func TestValidateLibrarySyncAPIKeyTransportTimeoutIsNotKeyValidation(t *testing.T) {
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Block past the client timeout so client.Do returns a transport
		// timeout error rather than any HTTP status.
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(release)

	client := &http.Client{Timeout: 50 * time.Millisecond}
	err := validateLibrarySyncAPIKey(context.Background(), client, server.URL, key)
	if err == nil {
		t.Fatalf("expected a timeout error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, "could not validate the API key") {
		t.Fatalf("transport timeout misreported as key-validation failure: %q", msg)
	}
	if strings.Contains(msg, "rejected the API key") {
		t.Fatalf("transport timeout misreported as key rejection: %q", msg)
	}
	lower := strings.ToLower(msg)
	if !strings.Contains(lower, "time") && !strings.Contains(lower, "retry") {
		t.Fatalf("expected a timeout-flavored message suggesting retry, got %q", msg)
	}
	if !errors.Is(err, errLibrarySyncTimeout) {
		t.Fatalf("expected error to wrap errLibrarySyncTimeout, got %q", msg)
	}
}

// TestValidateLibrarySyncAPIKeyContextDeadlineIsNotKeyValidation covers the
// caller-supplied context deadline path for bug #523: a context that expires
// mid-request must also be reported as a timeout, not a key validation failure.
func TestValidateLibrarySyncAPIKeyContextDeadlineIsNotKeyValidation(t *testing.T) {
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	client := &http.Client{}
	err := validateLibrarySyncAPIKey(ctx, client, server.URL, key)
	if err == nil {
		t.Fatalf("expected a timeout error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, "could not validate the API key") || strings.Contains(msg, "rejected the API key") {
		t.Fatalf("context deadline misreported as key validation/rejection: %q", msg)
	}
	if !errors.Is(err, errLibrarySyncTimeout) {
		t.Fatalf("expected error to wrap errLibrarySyncTimeout, got %q", msg)
	}
}

// snapshotHandler serves the library-sync snapshot endpoint, reporting
// snapshotComplete=false for the first incompleteResponses calls and true
// afterwards. It records how many times it was hit.
func snapshotHandler(t *testing.T, incompleteResponses int) (http.Handler, *int) {
	t.Helper()
	var calls int
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/library-sync/snapshot" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		calls++
		complete := calls > incompleteResponses
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"snapshotId":       "snap_test",
			"scopeLabel":       "test",
			"coveredSections":  []map[string]any{{"id": "botlets-brains", "label": "Botlets brains", "covered": true, "authoritative": true, "count": 0}},
			"snapshotComplete": complete,
		})
	})
	return h, &calls
}

func withFastSnapshotRetry(t *testing.T, interval, maxWait time.Duration) {
	t.Helper()
	origInterval, origMax := librarySyncSnapshotRetryInterval, librarySyncSnapshotRetryMaxWait
	librarySyncSnapshotRetryInterval = interval
	librarySyncSnapshotRetryMaxWait = maxWait
	t.Cleanup(func() {
		librarySyncSnapshotRetryInterval = origInterval
		librarySyncSnapshotRetryMaxWait = origMax
	})
}

// TestValidateLibrarySyncAPIKeyRetriesIncompleteSnapshot is the regression test
// for bug #564: right after a brand-new bot brain is created, its export
// snapshot is still generating, so the snapshot reports snapshotComplete=false
// for a few seconds. The login path must poll through that transient state
// instead of hard-failing on the first incomplete read.
func TestValidateLibrarySyncAPIKeyRetriesIncompleteSnapshot(t *testing.T) {
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	withFastSnapshotRetry(t, time.Millisecond, 2*time.Second)
	handler, calls := snapshotHandler(t, 2) // incomplete twice, then complete
	server := httptest.NewServer(handler)
	defer server.Close()

	client := &http.Client{}
	if err := validateLibrarySyncAPIKey(context.Background(), client, server.URL, key); err != nil {
		t.Fatalf("expected success after the snapshot completes, got: %v", err)
	}
	if *calls < 3 {
		t.Fatalf("expected the validator to poll past the incomplete snapshots (>=3 calls), got %d", *calls)
	}
}

func TestValidateLibrarySyncAPIKeyUsesFinalPageMetadata(t *testing.T) {
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	var sawFinalPage bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			writeJSON(t, w, map[string]any{
				"snapshotId":          "snap_paged_login",
				"scopeLabel":          "first page metadata",
				"coveredSections":     []map[string]any{},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    false,
				"rows":                []map[string]any{},
				"pageInfo":            map[string]any{"nextCursor": "final", "partial": true},
			})
		case "/auth/library-sync/snapshot/snap_paged_login/pages/final":
			sawFinalPage = true
			writeJSON(t, w, map[string]any{
				"snapshotId": "snap_paged_login",
				"scopeLabel": "final page metadata",
				"coveredSections": []map[string]any{
					{"id": "botlets-brains", "label": "Botlets brains", "covered": true, "authoritative": true, "count": 0},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows":                []map[string]any{},
				"pageInfo":            map[string]any{"nextCursor": nil, "partial": false},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &http.Client{}
	if err := validateLibrarySyncAPIKey(context.Background(), client, server.URL, key); err != nil {
		t.Fatalf("expected validation to use final page metadata, got: %v", err)
	}
	if !sawFinalPage {
		t.Fatalf("validator did not fetch the final snapshot page")
	}
}

func TestValidateLibrarySyncAPIKeyGivesUpOnBuildingFirstPageWithRetryMessage(t *testing.T) {
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	withFastSnapshotRetry(t, time.Millisecond, 30*time.Millisecond)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot", "/auth/library-sync/snapshot/snap_building/pages/first":
			writeJSON(t, w, map[string]any{
				"snapshotId":          "snap_building",
				"scopeLabel":          "building metadata",
				"coveredSections":     []map[string]any{},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    false,
				"rows":                []map[string]any{},
				"pageInfo":            map[string]any{"nextCursor": nil, "partial": true},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &http.Client{}
	err := validateLibrarySyncAPIKey(context.Background(), client, server.URL, key)
	if err == nil {
		t.Fatalf("expected an error when the incremental snapshot never completes")
	}
	msg := err.Error()
	if strings.Contains(msg, "could not validate the API key") {
		t.Fatalf("building snapshot timeout misreported as generic key validation: %q", msg)
	}
	if !strings.Contains(msg, "export snapshot is not complete") {
		t.Fatalf("expected the incomplete-snapshot retry message, got: %v", err)
	}
}

func TestValidateLibrarySyncAPIKeyContextDeadlineDuringMaterializationIsTimeout(t *testing.T) {
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	withFastSnapshotRetry(t, 100*time.Millisecond, time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		writeJSON(t, w, map[string]any{
			"snapshotId":          "snap_waiting",
			"scopeLabel":          "waiting metadata",
			"coveredSections":     []map[string]any{},
			"unsupportedSections": []map[string]any{},
			"snapshotComplete":    false,
			"rows":                []map[string]any{},
			"pageInfo":            map[string]any{"nextCursor": nil, "partial": true},
		})
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	client := &http.Client{}
	err := validateLibrarySyncAPIKey(ctx, client, server.URL, key)
	if err == nil {
		t.Fatalf("expected a timeout error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, "could not validate the API key") || strings.Contains(msg, "rejected the API key") {
		t.Fatalf("context deadline during materialization misreported as key validation/rejection: %q", msg)
	}
	if !errors.Is(err, errLibrarySyncTimeout) {
		t.Fatalf("expected error to wrap errLibrarySyncTimeout, got %q", msg)
	}
}

// TestValidateLibrarySyncAPIKeyGivesUpAfterMaxWait confirms the poll still
// terminates: a snapshot that never completes fails with the incomplete-snapshot
// message after the max wait elapses (preserving the pre-#564 final behavior).
func TestValidateLibrarySyncAPIKeyGivesUpAfterMaxWait(t *testing.T) {
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	withFastSnapshotRetry(t, time.Millisecond, 30*time.Millisecond)
	handler, _ := snapshotHandler(t, 1_000_000) // never completes
	server := httptest.NewServer(handler)
	defer server.Close()

	client := &http.Client{}
	err := validateLibrarySyncAPIKey(context.Background(), client, server.URL, key)
	if err == nil {
		t.Fatalf("expected an error when the snapshot never completes")
	}
	if !strings.Contains(err.Error(), "export snapshot is not complete") {
		t.Fatalf("expected the incomplete-snapshot message, got: %v", err)
	}
}

// TestRemoveAbsentPlacementsSkipsLivePathCollision is the regression test for
// bug #531: when two agents/documents collide on the SAME local brain path
// (duplicate agents minted on a team retry sharing owner-handle + bot-slug),
// the absent-mapping placement must NOT be swept, because that would silently
// delete the live document the surviving entry just wrote — leaving the bot's
// brain dir empty.
func TestRemoveAbsentPlacementsSkipsLivePathCollision(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := ensurePrivateDirs(paths); err != nil {
		t.Fatalf("ensure private dirs: %v", err)
	}
	brainDir := filepath.Join(root, "Botlets", "max", "pmf-tracker", "brain")
	if err := os.MkdirAll(brainDir, 0o755); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	livePath := filepath.Join(brainDir, "Identity.md")
	liveBody := "# Identity\n\nThe persona for this bot.\n"
	if err := os.WriteFile(livePath, []byte(liveBody), 0o444); err != nil {
		t.Fatalf("write live brain doc: %v", err)
	}

	state, err := openSyncState(context.Background(), paths)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	bodyHash := sha256Hex(liveBody)
	// The surviving/live document (still present in the snapshot) owns livePath.
	live := placementMeta{
		VisibleInstanceID: "brain-doc-live",
		Slug:              "identity",
		Section:           "botlets-brains",
		Path:              livePath,
		CanonicalPath:     livePath,
		ContentHash:       bodyHash,
		BodyContentHash:   bodyHash,
		Revision:          1,
		LastSeenSnapshot:  "lse_now",
	}
	if err := state.upsertPlacement(context.Background(), live); err != nil {
		t.Fatalf("upsert live placement: %v", err)
	}
	// The duplicate/old mapping is now absent from the snapshot but its stale
	// placement still points at the SAME path.
	absent := placementMeta{
		VisibleInstanceID: "brain-doc-dup",
		Slug:              "identity",
		Section:           "botlets-brains",
		Path:              livePath,
		CanonicalPath:     livePath,
		ContentHash:       bodyHash,
		BodyContentHash:   bodyHash,
		Revision:          1,
		LastSeenSnapshot:  "lse_old",
	}
	if err := state.upsertPlacement(context.Background(), absent); err != nil {
		t.Fatalf("upsert absent placement: %v", err)
	}

	seen := map[string]snapshotRow{
		"brain-doc-live": {VisibleInstanceID: "brain-doc-live", Kind: "document", DocSlug: "identity", Section: "botlets-brains"},
	}
	coveredSections := map[string]bool{"botlets-brains": true}
	allocated := map[string]string{"brain-doc-live": livePath}

	removed, recovered, err := removeAbsentPlacements(context.Background(), paths, state, root, "lse_now", seen, allocated, coveredSections)
	if err != nil {
		t.Fatalf("removeAbsentPlacements: %v", err)
	}

	// The live brain doc must still be on disk and unchanged.
	got, readErr := os.ReadFile(livePath)
	if readErr != nil {
		t.Fatalf("live brain doc was deleted by absent-removal sweep (bug #531): %v", readErr)
	}
	if string(got) != liveBody {
		t.Fatalf("live brain doc was clobbered: %q", string(got))
	}
	if removed != 0 {
		t.Fatalf("expected 0 destructive removals on a path collision, got %d", removed)
	}
	if recovered != 0 {
		t.Fatalf("expected 0 recoveries on a path collision, got %d", recovered)
	}

	// The stale colliding placement record should be dropped...
	if _, ok, err := state.getPlacement(context.Background(), "brain-doc-dup"); err != nil {
		t.Fatalf("get dup placement: %v", err)
	} else if ok {
		t.Fatalf("stale colliding placement should have been dropped")
	}
	// ...while the live placement remains intact.
	if _, ok, err := state.getPlacement(context.Background(), "brain-doc-live"); err != nil {
		t.Fatalf("get live placement: %v", err)
	} else if !ok {
		t.Fatalf("live placement must remain")
	}

	// A loud structured error should have been logged.
	logBytes, _ := os.ReadFile(filepath.Join(paths.Logs, "commentd.jsonl"))
	if !strings.Contains(string(logBytes), "sync.reconcile.path_collision_skip_removal") {
		t.Fatalf("expected loud collision log, got: %s", string(logBytes))
	}
}

// TestRemoveAbsentPlacementsGuardsViaAllocatedPath covers bug #531 defense (C)
// when the live document's path is known only from the freshly allocated path
// map (pathsByVisibleID) and not yet from a placement DB record. The absent
// duplicate must still be refused so the live brain doc is not swept.
func TestRemoveAbsentPlacementsGuardsViaAllocatedPath(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := ensurePrivateDirs(paths); err != nil {
		t.Fatalf("ensure private dirs: %v", err)
	}
	brainDir := filepath.Join(root, "Botlets", "max", "pmf-tracker", "brain")
	if err := os.MkdirAll(brainDir, 0o755); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	livePath := filepath.Join(brainDir, "Identity.md")
	liveBody := "# Identity\n\nThe persona for this bot.\n"
	if err := os.WriteFile(livePath, []byte(liveBody), 0o444); err != nil {
		t.Fatalf("write live brain doc: %v", err)
	}

	state, err := openSyncState(context.Background(), paths)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	bodyHash := sha256Hex(liveBody)
	// Only the absent duplicate has a placement DB record; the live document's
	// ownership of livePath is known solely via the allocation map below.
	absent := placementMeta{
		VisibleInstanceID: "brain-doc-dup",
		Slug:              "identity",
		Section:           "botlets-brains",
		Path:              livePath,
		CanonicalPath:     livePath,
		ContentHash:       bodyHash,
		BodyContentHash:   bodyHash,
		Revision:          1,
		LastSeenSnapshot:  "lse_old",
	}
	if err := state.upsertPlacement(context.Background(), absent); err != nil {
		t.Fatalf("upsert absent placement: %v", err)
	}

	seen := map[string]snapshotRow{
		"brain-doc-live": {VisibleInstanceID: "brain-doc-live", Kind: "document", DocSlug: "identity", Section: "botlets-brains"},
	}
	allocated := map[string]string{"brain-doc-live": livePath}
	coveredSections := map[string]bool{"botlets-brains": true}

	removed, _, err := removeAbsentPlacements(context.Background(), paths, state, root, "lse_now", seen, allocated, coveredSections)
	if err != nil {
		t.Fatalf("removeAbsentPlacements: %v", err)
	}
	if removed != 0 {
		t.Fatalf("expected 0 destructive removals when allocated path guards the doc, got %d", removed)
	}
	if _, readErr := os.ReadFile(livePath); readErr != nil {
		t.Fatalf("live brain doc was deleted despite allocated-path guard (bug #531): %v", readErr)
	}
	if _, ok, err := state.getPlacement(context.Background(), "brain-doc-dup"); err != nil {
		t.Fatalf("get dup placement: %v", err)
	} else if ok {
		t.Fatalf("stale colliding placement should have been dropped")
	}
}

// TestRemoveAbsentPlacementsGuardsPartialPathCollision covers the case where an
// absent placement collides with a live doc only on its CanonicalPath (its Path
// points elsewhere). absentPlacementCollidesWithSeen checks both fields, so the
// guard must still fire and refuse the destructive sweep.
func TestRemoveAbsentPlacementsGuardsPartialPathCollision(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := ensurePrivateDirs(paths); err != nil {
		t.Fatalf("ensure private dirs: %v", err)
	}
	brainDir := filepath.Join(root, "Botlets", "max", "pmf-tracker", "brain")
	if err := os.MkdirAll(brainDir, 0o755); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	livePath := filepath.Join(brainDir, "Identity.md")
	liveBody := "# Identity\n\nThe persona for this bot.\n"
	if err := os.WriteFile(livePath, []byte(liveBody), 0o444); err != nil {
		t.Fatalf("write live brain doc: %v", err)
	}

	state, err := openSyncState(context.Background(), paths)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	bodyHash := sha256Hex(liveBody)
	// The absent duplicate's actual Path is some other (non-existent) location,
	// but its CanonicalPath collides with the live doc's path.
	absent := placementMeta{
		VisibleInstanceID: "brain-doc-dup",
		Slug:              "identity",
		Section:           "botlets-brains",
		Path:              filepath.Join(brainDir, "Identity-stale.md"),
		CanonicalPath:     livePath,
		ContentHash:       bodyHash,
		BodyContentHash:   bodyHash,
		Revision:          1,
		LastSeenSnapshot:  "lse_old",
	}
	if err := state.upsertPlacement(context.Background(), absent); err != nil {
		t.Fatalf("upsert absent placement: %v", err)
	}

	seen := map[string]snapshotRow{
		"brain-doc-live": {VisibleInstanceID: "brain-doc-live", Kind: "document", DocSlug: "identity", Section: "botlets-brains"},
	}
	allocated := map[string]string{"brain-doc-live": livePath}
	coveredSections := map[string]bool{"botlets-brains": true}

	removed, _, err := removeAbsentPlacements(context.Background(), paths, state, root, "lse_now", seen, allocated, coveredSections)
	if err != nil {
		t.Fatalf("removeAbsentPlacements: %v", err)
	}
	if removed != 0 {
		t.Fatalf("expected 0 destructive removals on a canonical-path collision, got %d", removed)
	}
	if _, readErr := os.ReadFile(livePath); readErr != nil {
		t.Fatalf("live brain doc was deleted via partial-collision gap (bug #531): %v", readErr)
	}
	if _, ok, err := state.getPlacement(context.Background(), "brain-doc-dup"); err != nil {
		t.Fatalf("get dup placement: %v", err)
	} else if ok {
		t.Fatalf("stale colliding placement should have been dropped")
	}
}

// TestRemoveAbsentPlacementsAbsentAbsentSamePath covers the absent-vs-absent
// collision: two stale placements point at the same orphaned path with no live
// owner. The first sweep removes the file; the second finds it already gone and
// drops its record without erroring. Both records are cleaned up.
func TestRemoveAbsentPlacementsAbsentAbsentSamePath(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(t.TempDir(), "Comment Docs")
	paths, err := resolvePaths(home)
	if err != nil {
		t.Fatalf("resolve paths: %v", err)
	}
	if err := ensurePrivateDirs(paths); err != nil {
		t.Fatalf("ensure private dirs: %v", err)
	}
	brainDir := filepath.Join(root, "Botlets", "max", "pmf-tracker", "brain")
	if err := os.MkdirAll(brainDir, 0o755); err != nil {
		t.Fatalf("mkdir brain: %v", err)
	}
	orphanPath := filepath.Join(brainDir, "Identity.md")
	orphanBody := "# Identity\n\nOrphaned content.\n"
	if err := os.WriteFile(orphanPath, []byte(orphanBody), 0o444); err != nil {
		t.Fatalf("write orphan doc: %v", err)
	}

	state, err := openSyncState(context.Background(), paths)
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer state.Close()

	bodyHash := sha256Hex(orphanBody)
	for _, id := range []string{"brain-doc-a", "brain-doc-b"} {
		if err := state.upsertPlacement(context.Background(), placementMeta{
			VisibleInstanceID: id,
			Slug:              "identity",
			Section:           "botlets-brains",
			Path:              orphanPath,
			CanonicalPath:     orphanPath,
			ContentHash:       bodyHash,
			BodyContentHash:   bodyHash,
			Revision:          1,
			LastSeenSnapshot:  "lse_old",
		}); err != nil {
			t.Fatalf("upsert %s: %v", id, err)
		}
	}

	// Neither placement is seen this pass — both are genuinely orphaned.
	seen := map[string]snapshotRow{}
	allocated := map[string]string{}
	coveredSections := map[string]bool{"botlets-brains": true}

	removed, _, err := removeAbsentPlacements(context.Background(), paths, state, root, "lse_now", seen, allocated, coveredSections)
	if err != nil {
		t.Fatalf("removeAbsentPlacements on absent-absent collision returned error: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected exactly 1 destructive removal for an orphaned file, got %d", removed)
	}
	if _, statErr := os.Stat(orphanPath); !os.IsNotExist(statErr) {
		t.Fatalf("orphaned file should have been removed, stat err = %v", statErr)
	}
	for _, id := range []string{"brain-doc-a", "brain-doc-b"} {
		if _, ok, err := state.getPlacement(context.Background(), id); err != nil {
			t.Fatalf("get %s: %v", id, err)
		} else if ok {
			t.Fatalf("orphaned placement %s should have been dropped", id)
		}
	}
}

// TestDetectAllocationCollisions covers bug #531 defense (A): two document rows
// mapped to the same local path must be detected so the sync can surface a loud
// warning instead of silently clobbering one with the other.
func TestDetectAllocationCollisions(t *testing.T) {
	rows := []snapshotRow{
		{VisibleInstanceID: "a", Kind: "document"},
		{VisibleInstanceID: "b", Kind: "document"},
		{VisibleInstanceID: "c", Kind: "document"},
		{VisibleInstanceID: "f", Kind: "folder"},
	}
	pathsByVisibleID := map[string]string{
		"a": "/root/Botlets/max/pmf-tracker/brain/Identity.md",
		"b": "/root/Botlets/max/pmf-tracker/brain/Identity.md", // collides with a
		"c": "/root/My Files/Notes.md",
		"f": "/root/Botlets/max/pmf-tracker/brain", // folder, ignored
	}
	collisions := detectAllocationCollisions(rows, pathsByVisibleID)
	if len(collisions) != 1 {
		t.Fatalf("expected exactly 1 collision, got %d: %+v", len(collisions), collisions)
	}
	if collisions[0].Path != "/root/Botlets/max/pmf-tracker/brain/Identity.md" {
		t.Fatalf("unexpected collision path: %q", collisions[0].Path)
	}
	if len(collisions[0].VisibleInstanceIDs) != 2 ||
		collisions[0].VisibleInstanceIDs[0] != "a" ||
		collisions[0].VisibleInstanceIDs[1] != "b" {
		t.Fatalf("unexpected collision members: %+v", collisions[0].VisibleInstanceIDs)
	}
}
