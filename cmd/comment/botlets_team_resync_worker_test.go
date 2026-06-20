package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
	"github.com/comment-hq/comment-cli/internal/commentsync"
)

func TestBotletsTeamResyncWorkerSkipsWhenTeamRuntimeMissing(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	restore := stubBotletsTeamResyncManifest(t, func(context.Context, *http.Client, commentbus.Paths, string, botletsTeamRuntimeConfig, string, string) ([]string, error) {
		t.Fatal("team resync should not run without team-runtime.json")
		return nil, nil
	})
	defer restore()

	summary := runBotletsTeamResyncWorkerOnce(context.Background(), paths, botletsHome, "")
	if summary.Configured {
		t.Fatalf("summary.Configured = true, want false")
	}
	if _, err := os.Stat(filepath.Join(paths.Logs, "commentd.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("log file err = %v, want not exist", err)
	}
}

func TestWriteBotletsTeamRuntimeConfigTightensExistingFileMode(t *testing.T) {
	botletsHome := t.TempDir()
	path := botletsTeamRuntimeConfigPath(botletsHome)
	if err := os.WriteFile(path, []byte(`{"runner_secret":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:        "bw_mode",
		RunnerID:           "br_mode",
		RunnerSecret:       "brs_secret_must_not_be_logged",
		BaseURL:            "https://comment.io",
		Runtime:            "claude",
		LastManifestAgents: []string{},
		InstallationID:     "install_mode",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("team-runtime.json mode = %o, want 0600", got)
	}
}

func TestBotletsTeamResyncWorkerRunsConfiguredTeamRuntime(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      "https://comment.io",
		Runtime:      "codex",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	configureSyncForTest(t, paths, cfg.BaseURL)
	calls := 0
	restore := stubBotletsTeamResyncManifest(t, func(_ context.Context, client *http.Client, gotPaths commentbus.Paths, gotHome string, gotCfg botletsTeamRuntimeConfig, runtime string, priorResyncError string) ([]string, error) {
		calls++
		if client != botletsTeamResyncHTTPClient {
			t.Fatal("worker did not pass the configured HTTP client")
		}
		if gotPaths.Home != paths.Home || gotHome != botletsHome {
			t.Fatalf("paths/home = %q %q, want %q %q", gotPaths.Home, gotHome, paths.Home, botletsHome)
		}
		if gotCfg.WorkspaceID != cfg.WorkspaceID || gotCfg.RunnerID != cfg.RunnerID || gotCfg.RunnerSecret != cfg.RunnerSecret {
			t.Fatalf("config = %+v, want %+v", gotCfg, cfg)
		}
		if runtime != "codex" {
			t.Fatalf("runtime = %q, want codex", runtime)
		}
		if priorResyncError != "" {
			t.Fatalf("priorResyncError = %q, want empty", priorResyncError)
		}
		return []string{"max.alpha", "max.beta"}, nil
	})
	defer restore()

	summary := runBotletsTeamResyncWorkerOnce(context.Background(), paths, botletsHome, "")
	if calls != 1 {
		t.Fatalf("resync calls = %d, want 1", calls)
	}
	if !summary.Configured || summary.WorkspaceID != cfg.WorkspaceID || summary.RunnerID != cfg.RunnerID {
		t.Fatalf("summary = %+v", summary)
	}
	if got := strings.Join(summary.Agents, ","); got != "max.alpha,max.beta" {
		t.Fatalf("summary agents = %q", got)
	}
	logText := readBotletsTeamResyncLog(t, paths)
	if !strings.Contains(logText, "botlets.team_resync_complete") {
		t.Fatalf("log did not contain completion: %s", logText)
	}
	if strings.Contains(logText, cfg.RunnerSecret) {
		t.Fatalf("log leaked runner secret: %s", logText)
	}
}

func TestBotletsTeamResyncWorkerFailsClosedWhenSyncUnusableWithoutProvisionError(t *testing.T) {
	// Codex round-16: a team-runtime machine that is NOT daemon-paired performs
	// no sync provisioning, so syncProvisionErr stays nil. The worker must STILL
	// fail closed when local sync is unusable, rather than minting runner-bound
	// credentials every pass while local registration fails for missing sync.
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      "https://comment.io",
		Runtime:      "claude",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	// Deliberately NO configureSyncForTest and NO daemon pairing.
	restore := stubBotletsTeamResyncManifest(t, func(context.Context, *http.Client, commentbus.Paths, string, botletsTeamRuntimeConfig, string, string) ([]string, error) {
		t.Fatal("manifest fetch must be skipped when sync is unusable")
		return nil, nil
	})
	defer restore()

	summary := runBotletsTeamResyncWorkerOnce(context.Background(), paths, botletsHome, "")
	if !summary.Configured {
		t.Fatal("summary.Configured = false, want true")
	}
	if summary.ResyncError == "" {
		t.Fatal("ResyncError empty, want the synthesized sync-not-configured error")
	}
	if !strings.Contains(summary.ResyncError, "comment sync login") {
		t.Fatalf("ResyncError = %q, want it to name the `comment sync login` fix", summary.ResyncError)
	}
}

func TestResyncBotletsTeamManifestPersistsManifestFingerprint(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	var sawManifest bool
	var sawAck bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":3}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest"):
			sawManifest = true
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_9","agents":[]}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest/ack"):
			sawAck = true
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      server.URL,
		Runtime:      "claude",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}

	registered, err := resyncBotletsTeamManifest(context.Background(), http.DefaultClient, paths, botletsHome, cfg, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(registered) != 0 || !sawManifest || !sawAck {
		t.Fatalf("registered=%v sawManifest=%v sawAck=%v", registered, sawManifest, sawAck)
	}
	updated, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastManifestFingerprint != "btmf_9" {
		t.Fatalf("last manifest fingerprint = %q, want btmf_9", updated.LastManifestFingerprint)
	}
	if updated.RunnerSecret != cfg.RunnerSecret {
		t.Fatal("team runtime config lost runner secret")
	}
}

func TestResyncBotletsTeamManifestDoesNotPersistGatedFingerprint(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":3}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_gated","manifest_gated":true,"agents":[]}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest/ack"):
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      server.URL,
		Runtime:      "claude",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}

	if _, err := resyncBotletsTeamManifest(context.Background(), http.DefaultClient, paths, botletsHome, cfg, "claude", ""); err != nil {
		t.Fatal(err)
	}
	updated, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastManifestFingerprint != "" {
		t.Fatalf("last manifest fingerprint = %q, want empty for gated manifest", updated.LastManifestFingerprint)
	}
}

func TestResyncBotletsTeamManifestDoesNotPersistFingerprintWhenAckFails(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":3}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_9","agents":[]}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest/ack"):
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"ack failed"}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      server.URL,
		Runtime:      "claude",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}

	shrinkBotletsTeamAckRetryDelay(t)
	_, err := resyncBotletsTeamManifest(context.Background(), http.DefaultClient, paths, botletsHome, cfg, "claude", "")
	if err == nil || !strings.Contains(err.Error(), "acknowledge team manifest") {
		t.Fatalf("error = %v, want ack failure", err)
	}
	updated, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastManifestFingerprint != "" {
		t.Fatalf("last manifest fingerprint = %q, want empty after ack failure", updated.LastManifestFingerprint)
	}
}

// shrinkBotletsTeamAckRetryDelay makes the in-pass team-manifest ack retry
// near-instant for tests that exercise ack failures.
func shrinkBotletsTeamAckRetryDelay(t *testing.T) {
	t.Helper()
	old := botletsTeamManifestAckRetryDelay
	botletsTeamManifestAckRetryDelay = time.Millisecond
	t.Cleanup(func() { botletsTeamManifestAckRetryDelay = old })
}

func TestResyncBotletsTeamManifestRetriesAckTransientFailure(t *testing.T) {
	// The manifest fetch mints fresh runner-bound credentials for every bot,
	// and a failed pass triggers a FULL re-fetch (another mint per bot) on the
	// next 30s tick. A transient ack hiccup must therefore be retried within
	// the pass instead of failing it.
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	shrinkBotletsTeamAckRetryDelay(t)
	manifestCalls := 0
	ackCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":3}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest"):
			manifestCalls++
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_retry","agents":[{"handle":"max.alpha","agent_secret":"as_secret","bot_slug":"alpha","bot_agent_id":"ag_alpha","owner_agent_id":"ag_max","bot_id":"bb_alpha","setup_generation":2,"brain":{"workspaceId":"bw_brain","containerId":"lib_brain","rootFolderId":"fld_brain"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest/ack"):
			ackCalls++
			if ackCalls == 1 {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = w.Write([]byte(`{"error":"ack failed"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.Contains(r.URL.Path, "/bindings/"):
			_, _ = w.Write([]byte(`{"ok":true,"binding":{}}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      server.URL,
		Runtime:      "claude",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	restoreRegister := stubBotletsTeamRegisterLocally(t, func(_ context.Context, in botletsRegisterInput) (botletsRegisterResult, error) {
		return botletsRegisterResult{ProfilePath: filepath.Join(in.Paths.Home, "agents", in.BotHandle+".json")}, nil
	})
	defer restoreRegister()

	registered, err := resyncBotletsTeamManifest(context.Background(), http.DefaultClient, paths, botletsHome, cfg, "claude", "")
	if err != nil {
		t.Fatalf("resync should survive one transient ack failure: %v", err)
	}
	if strings.Join(registered, ",") != "max.alpha" {
		t.Fatalf("registered = %v", registered)
	}
	if manifestCalls != 1 || ackCalls != 2 {
		t.Fatalf("manifestCalls=%d ackCalls=%d, want 1 manifest fetch and an ack retry (no credential re-mint)", manifestCalls, ackCalls)
	}
	updated, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastManifestFingerprint != "btmf_retry" {
		t.Fatalf("last manifest fingerprint = %q, want btmf_retry", updated.LastManifestFingerprint)
	}
	if strings.Join(updated.LastManifestAgents, ",") != "max.alpha" {
		t.Fatalf("last manifest agents = %v, want the registered handles persisted", updated.LastManifestAgents)
	}
}

func TestResyncBotletsTeamManifestFailsThePassWhenReloadWarns(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":3}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_reload","agents":[{"handle":"max.alpha","agent_secret":"as_secret","bot_slug":"alpha","bot_agent_id":"ag_alpha","owner_agent_id":"ag_max","bot_id":"bb_alpha","setup_generation":2,"brain":{"workspaceId":"bw_brain","containerId":"lib_brain","rootFolderId":"fld_brain"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest/ack"):
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.Contains(r.URL.Path, "/bindings/"):
			_, _ = w.Write([]byte(`{"ok":true,"binding":{}}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      server.URL,
		Runtime:      "claude",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	restoreRegister := stubBotletsTeamRegisterLocally(t, func(_ context.Context, in botletsRegisterInput) (botletsRegisterResult, error) {
		if in.BotHandle != "max.alpha" || in.SetupGeneration != 2 {
			t.Fatalf("register input = %+v", in)
		}
		return botletsRegisterResult{ReloadError: "daemon reload failed"}, nil
	})
	defer restoreRegister()

	registered, err := resyncBotletsTeamManifest(context.Background(), http.DefaultClient, paths, botletsHome, cfg, "claude", "")
	// A reload warning now FAILS the pass: the files landed but the daemon is
	// not running the bot, so success here (ready + ack + cleared error) would
	// hide a broken runner while every 30s retry re-mints credentials behind
	// a healthy-looking UI. The error is what surfaces resyncError in the UI.
	if err == nil || !strings.Contains(err.Error(), "did not reload") {
		t.Fatalf("err = %v, want a did-not-reload failure", err)
	}
	if len(registered) != 0 {
		t.Fatalf("registered = %v, want none reported for a failed pass", registered)
	}
	updated, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastManifestFingerprint != "" {
		t.Fatalf("last manifest fingerprint = %q, want empty after reload failure", updated.LastManifestFingerprint)
	}
}

func TestResyncBotletsTeamManifestDoesNotOverwriteChangedRuntimeConfig(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":3}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_old","agents":[]}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest/ack"):
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	oldCfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_old",
		RunnerSecret: "brs_old",
		BaseURL:      server.URL,
		Runtime:      "claude",
	}
	newCfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_new",
		RunnerSecret: "brs_new",
		BaseURL:      server.URL,
		Runtime:      "codex",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, oldCfg); err != nil {
		t.Fatal(err)
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, newCfg); err != nil {
		t.Fatal(err)
	}

	if _, err := resyncBotletsTeamManifest(context.Background(), http.DefaultClient, paths, botletsHome, oldCfg, "claude", ""); err != nil {
		t.Fatal(err)
	}
	updated, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RunnerID != newCfg.RunnerID || updated.RunnerSecret != newCfg.RunnerSecret || updated.Runtime != newCfg.Runtime {
		t.Fatalf("runtime config was overwritten: %+v", updated)
	}
	if updated.LastManifestFingerprint != "" {
		t.Fatalf("last manifest fingerprint = %q, want empty for replaced runner", updated.LastManifestFingerprint)
	}
}

func TestBotletsTeamResyncWorkerSkipsUpToDateManifestFingerprint(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":7}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/team-manifest-status") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_7"}`))
			return
		}
		t.Fatalf("unexpected request: %s", r.URL.Path)
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:             "bw_workspace",
		RunnerID:                "br_runner",
		RunnerSecret:            "brs_secret_must_not_be_logged",
		BaseURL:                 server.URL,
		Runtime:                 "claude",
		LastManifestFingerprint: "btmf_7",
		// A genuinely zero-bot team: a NON-nil empty list is "fully synced, no
		// bots", which stays on the fast path (a nil list would instead force a
		// backfill resync).
		LastManifestAgents: []string{},
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	restore := stubBotletsTeamResyncManifest(t, func(context.Context, *http.Client, commentbus.Paths, string, botletsTeamRuntimeConfig, string, string) ([]string, error) {
		t.Fatal("team manifest should not be fetched when generation is unchanged")
		return nil, nil
	})
	defer restore()

	summary := runBotletsTeamResyncWorkerOnce(context.Background(), paths, botletsHome, "")
	if !summary.Configured || !summary.UpToDate {
		t.Fatalf("summary = %+v, want configured/up-to-date", summary)
	}
	if _, err := os.Stat(filepath.Join(paths.Logs, "commentd.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("log file err = %v, want not exist for quiet up-to-date skip", err)
	}
}

// writeBotletsTeamLocalInstall writes the raw registry entry + profile file
// the fingerprint fast path verifies (missingBotletsTeamLocalInstall).
func writeBotletsTeamLocalInstall(t *testing.T, paths commentbus.Paths, botletsHome string, handle string) {
	t.Helper()
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	registry := []byte(`{"bots":[{"name":"alpha","handle":"` + handle + `","credential_profile":"` + filepath.Join(paths.Home, "agents", handle+".json") + `","managed_session":{"enabled":true,"runtime":"claude"}}]}`)
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), registry, 0o600); err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := []byte(`{"handle":"` + handle + `","agent_secret":"as_installed","base_url":"https://comment.io"}` + "\n")
	if err := os.WriteFile(filepath.Join(agentsDir, handle+".json"), profile, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBotletsTeamResyncWorkerUpToDateVerifiesLocalInstalls(t *testing.T) {
	// The persisted fingerprint only proves the SERVER manifest is unchanged.
	// When the local install for a persisted manifest handle is gone (profile
	// deleted, registry lost), the fast path must fall through to a full
	// resync so the bot is repaired — and when everything is present the fast
	// path must keep skipping the (credential-minting) manifest fetch.
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":7}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest-status"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_7"}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:             "bw_workspace",
		RunnerID:                "br_runner",
		RunnerSecret:            "brs_secret_must_not_be_logged",
		BaseURL:                 server.URL,
		Runtime:                 "claude",
		LastManifestFingerprint: "btmf_7",
		LastManifestAgents:      []string{"max.alpha"},
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	configureSyncForTest(t, paths, cfg.BaseURL)

	// Install missing locally: the unchanged fingerprint must NOT be trusted.
	resyncs := 0
	restore := stubBotletsTeamResyncManifest(t, func(context.Context, *http.Client, commentbus.Paths, string, botletsTeamRuntimeConfig, string, string) ([]string, error) {
		resyncs++
		return []string{"max.alpha"}, nil
	})
	summary := runBotletsTeamResyncWorkerOnce(context.Background(), paths, botletsHome, "")
	restore()
	if resyncs != 1 {
		t.Fatalf("resyncs = %d, want a full repair resync while the local install is missing", resyncs)
	}
	if summary.UpToDate {
		t.Fatalf("summary = %+v, want NOT up-to-date while the local install is missing", summary)
	}
	logText := readBotletsTeamResyncLog(t, paths)
	if !strings.Contains(logText, "botlets.team_resync_local_install_missing") {
		t.Fatalf("log missing local-install-missing warning: %s", logText)
	}

	// With the registry entry + profile present, the fast path skips the fetch.
	writeBotletsTeamLocalInstall(t, paths, botletsHome, "max.alpha")
	restore = stubBotletsTeamResyncManifest(t, func(context.Context, *http.Client, commentbus.Paths, string, botletsTeamRuntimeConfig, string, string) ([]string, error) {
		t.Fatal("team manifest should not be fetched when fingerprint and local installs are intact")
		return nil, nil
	})
	defer restore()
	summary = runBotletsTeamResyncWorkerOnce(context.Background(), paths, botletsHome, "")
	if !summary.Configured || !summary.UpToDate {
		t.Fatalf("summary = %+v, want configured/up-to-date once installs are present", summary)
	}
}

func TestBotletsTeamResyncWorkerForcesResyncWhenAgentListNeverPersisted(t *testing.T) {
	// A config upgraded from before LastManifestAgents existed carries the
	// fingerprint with a nil handle list. Even when the SERVER fingerprint is
	// unchanged, the worker must force ONE full resync (which backfills the
	// list) rather than honoring the fast path with no handles to verify —
	// otherwise the local-install verification stays disabled indefinitely
	// (Codex round-7).
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":7}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest-status"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_7"}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:             "bw_workspace",
		RunnerID:                "br_runner",
		RunnerSecret:            "brs_secret_must_not_be_logged",
		BaseURL:                 server.URL,
		Runtime:                 "claude",
		LastManifestFingerprint: "btmf_7",
		// LastManifestAgents intentionally left nil (never persisted).
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	configureSyncForTest(t, paths, cfg.BaseURL)
	// Sanity: the round-tripped config keeps the handle list nil (an upgraded
	// config has no last_manifest_agents key) so the worker takes the forced
	// path rather than the fast path.
	if reread, err := readBotletsTeamRuntimeConfig(botletsHome); err != nil {
		t.Fatal(err)
	} else if reread.LastManifestAgents != nil {
		t.Fatalf("persisted agents = %#v, want nil for an upgraded config", reread.LastManifestAgents)
	}
	resyncs := 0
	restore := stubBotletsTeamResyncManifest(t, func(context.Context, *http.Client, commentbus.Paths, string, botletsTeamRuntimeConfig, string, string) ([]string, error) {
		resyncs++
		return []string{"max.alpha"}, nil
	})
	defer restore()

	summary := runBotletsTeamResyncWorkerOnce(context.Background(), paths, botletsHome, "")
	if resyncs != 1 {
		t.Fatalf("resyncs = %d, want a forced backfill resync when the handle list was never persisted", resyncs)
	}
	if summary.UpToDate {
		t.Fatalf("summary = %+v, want NOT up-to-date while the handle list is unpersisted", summary)
	}
	if strings.Join(summary.Agents, ",") != "max.alpha" {
		t.Fatalf("summary agents = %v, want the resync's registered handles", summary.Agents)
	}
}

func TestBotletsTeamResyncWorkerWarnsOnIncompleteConfig(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID: "bw_workspace",
		RunnerID:    "br_runner",
		BaseURL:     "https://comment.io",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	restore := stubBotletsTeamResyncManifest(t, func(context.Context, *http.Client, commentbus.Paths, string, botletsTeamRuntimeConfig, string, string) ([]string, error) {
		t.Fatal("team resync should not run with incomplete config")
		return nil, nil
	})
	defer restore()

	summary := runBotletsTeamResyncWorkerOnce(context.Background(), paths, botletsHome, "")
	if !summary.Configured {
		t.Fatalf("summary.Configured = false, want true for readable but incomplete config")
	}
	logText := readBotletsTeamResyncLog(t, paths)
	if !strings.Contains(logText, "botlets.team_resync_config_incomplete") {
		t.Fatalf("log did not contain incomplete-config warning: %s", logText)
	}
	if !strings.Contains(logText, "runner_secret") {
		t.Fatalf("log did not name missing runner_secret: %s", logText)
	}
}

func testBotletsTeamResyncPaths(t *testing.T) commentbus.Paths {
	t.Helper()
	paths, err := commentbus.ResolvePaths(filepath.Join(t.TempDir(), "comment"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// configureSyncForTest marks local library sync usable for baseURL so a worker
// test reaches the full team-manifest path. The worker now fails closed when
// `daemonSyncUsableForOrigin` is false (Codex round-16), so any test that
// exercises the manifest fetch must first establish a usable sync config. Login
// writes the config locally (no network — ValidateKey defaults false) under a
// scratch root.
func configureSyncForTest(t *testing.T, paths commentbus.Paths, baseURL string) {
	t.Helper()
	if _, err := commentsync.Login(context.Background(), commentsync.Options{
		Home:    paths.Home,
		BaseURL: baseURL,
		Root:    filepath.Join(t.TempDir(), "Comment Docs"),
		APIKey:  "usk_v2.ag_synctest.key1.secret1",
	}); err != nil {
		t.Fatalf("configure sync for test: %v", err)
	}
}

func stubBotletsTeamResyncManifest(t *testing.T, fn func(context.Context, *http.Client, commentbus.Paths, string, botletsTeamRuntimeConfig, string, string) ([]string, error)) func() {
	t.Helper()
	old := botletsTeamResyncManifest
	botletsTeamResyncManifest = fn
	return func() {
		botletsTeamResyncManifest = old
	}
}

func stubBotletsTeamRegisterLocally(t *testing.T, fn func(context.Context, botletsRegisterInput) (botletsRegisterResult, error)) func() {
	t.Helper()
	old := botletsTeamRegisterLocally
	botletsTeamRegisterLocally = fn
	return func() {
		botletsTeamRegisterLocally = old
	}
}

func readBotletsTeamResyncLog(t *testing.T, paths commentbus.Paths) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(paths.Logs, "commentd.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// bindingReport is one captured POST to the runner binding endpoint.
type bindingReport struct {
	path string
	body map[string]any
}

func TestResyncBotletsTeamManifestReportsBindingReadyPerAgent(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	var bindings []bindingReport
	var sawAck bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":3}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_bind","agents":[` +
				`{"handle":"max.alpha","agent_secret":"as_secret_a","bot_slug":"alpha","bot_agent_id":"ag_alpha","owner_agent_id":"ag_max","bot_id":"bb_alpha","setup_generation":1,"schedule_timezone":"America/Chicago","brain":{"workspaceId":"bw_brain","containerId":"lib_brain","rootFolderId":"fld_brain"}},` +
				`{"handle":"max.beta","agent_secret":"as_secret_b","bot_slug":"beta","bot_agent_id":"ag_beta","owner_agent_id":"ag_max","bot_id":"bb_beta","setup_generation":1,"brain":{"workspaceId":"bw_brain","containerId":"lib_brain","rootFolderId":"fld_brain"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest/ack"):
			sawAck = true
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.Contains(r.URL.Path, "/bindings/"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("binding body unreadable: %v", err)
			}
			bindings = append(bindings, bindingReport{path: r.URL.Path, body: body})
			_, _ = w.Write([]byte(`{"ok":true,"binding":{}}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      server.URL,
		Runtime:      "claude",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	var captured []botletsRegisterInput
	restoreRegister := stubBotletsTeamRegisterLocally(t, func(_ context.Context, in botletsRegisterInput) (botletsRegisterResult, error) {
		captured = append(captured, in)
		return botletsRegisterResult{}, nil
	})
	defer restoreRegister()

	registered, err := resyncBotletsTeamManifest(context.Background(), http.DefaultClient, paths, botletsHome, cfg, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(registered, ",") != "max.alpha,max.beta" || !sawAck {
		t.Fatalf("registered=%v sawAck=%v", registered, sawAck)
	}
	// The manifest's schedule_timezone flows into the local registration's
	// managed-session timezone (same as the enrollment hint); a manifest agent
	// without one falls back to the registry default (empty).
	if len(captured) != 2 {
		t.Fatalf("register inputs = %d, want 2", len(captured))
	}
	if captured[0].ScheduleTimezone != "America/Chicago" {
		t.Fatalf("alpha schedule timezone = %q, want America/Chicago (from the manifest)", captured[0].ScheduleTimezone)
	}
	if captured[1].ScheduleTimezone != "" {
		t.Fatalf("beta schedule timezone = %q, want empty (manifest omitted it)", captured[1].ScheduleTimezone)
	}
	if len(bindings) != 2 {
		t.Fatalf("binding reports = %d, want 2: %+v", len(bindings), bindings)
	}
	wantPaths := []string{
		"/botlets/workspaces/bw_workspace/runners/br_runner/bindings/ag_alpha/ready",
		"/botlets/workspaces/bw_workspace/runners/br_runner/bindings/ag_beta/ready",
	}
	for i, want := range wantPaths {
		if bindings[i].path != want {
			t.Fatalf("binding[%d] path = %q, want %q", i, bindings[i].path, want)
		}
		if got := bindings[i].body["runnerSecret"]; got != cfg.RunnerSecret {
			t.Fatalf("binding[%d] runnerSecret = %v", i, got)
		}
	}
	updated, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastManifestFingerprint != "btmf_bind" {
		t.Fatalf("last manifest fingerprint = %q, want btmf_bind", updated.LastManifestFingerprint)
	}
}

func TestResyncBotletsTeamManifestReportsBindingProblemOnRegisterFailure(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	var bindings []bindingReport
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":3}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_fail","agents":[{"handle":"max.alpha","agent_secret":"as_secret","bot_slug":"alpha","bot_agent_id":"ag_alpha","owner_agent_id":"ag_max","bot_id":"bb_alpha","setup_generation":1,"brain":{"workspaceId":"bw_brain","containerId":"lib_brain","rootFolderId":"fld_brain"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest/ack"):
			t.Error("manifest must not be acked when a registration failed")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.Contains(r.URL.Path, "/bindings/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			bindings = append(bindings, bindingReport{path: r.URL.Path, body: body})
			_, _ = w.Write([]byte(`{"ok":true,"binding":{}}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      server.URL,
		Runtime:      "claude",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	restoreRegister := stubBotletsTeamRegisterLocally(t, func(_ context.Context, in botletsRegisterInput) (botletsRegisterResult, error) {
		return botletsRegisterResult{}, errors.New("comment sync is not configured")
	})
	defer restoreRegister()

	_, err := resyncBotletsTeamManifest(context.Background(), http.DefaultClient, paths, botletsHome, cfg, "claude", "")
	if err == nil || !strings.Contains(err.Error(), "could not register team agent max.alpha") {
		t.Fatalf("error = %v, want registration failure", err)
	}
	if len(bindings) != 1 {
		t.Fatalf("binding reports = %d, want 1: %+v", len(bindings), bindings)
	}
	if want := "/botlets/workspaces/bw_workspace/runners/br_runner/bindings/ag_alpha/problem"; bindings[0].path != want {
		t.Fatalf("binding path = %q, want %q", bindings[0].path, want)
	}
	if got := bindings[0].body["runnerSecret"]; got != cfg.RunnerSecret {
		t.Fatalf("binding runnerSecret = %v", got)
	}
}

func TestResyncBotletsTeamManifestBindingFailureDoesNotBlockResync(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	var sawAck bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":3}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_bindfail","agents":[{"handle":"max.alpha","agent_secret":"as_secret","bot_slug":"alpha","bot_agent_id":"ag_alpha","owner_agent_id":"ag_max","bot_id":"bb_alpha","setup_generation":1,"brain":{"workspaceId":"bw_brain","containerId":"lib_brain","rootFolderId":"fld_brain"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest/ack"):
			sawAck = true
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.Contains(r.URL.Path, "/bindings/"):
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"binding store unavailable"}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      server.URL,
		Runtime:      "claude",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	restoreRegister := stubBotletsTeamRegisterLocally(t, func(_ context.Context, in botletsRegisterInput) (botletsRegisterResult, error) {
		return botletsRegisterResult{}, nil
	})
	defer restoreRegister()

	registered, err := resyncBotletsTeamManifest(context.Background(), http.DefaultClient, paths, botletsHome, cfg, "claude", "")
	if err != nil {
		t.Fatalf("binding failure must not fail the resync: %v", err)
	}
	if strings.Join(registered, ",") != "max.alpha" || !sawAck {
		t.Fatalf("registered=%v sawAck=%v", registered, sawAck)
	}
	updated, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastManifestFingerprint != "btmf_bindfail" {
		t.Fatalf("last manifest fingerprint = %q, want btmf_bindfail", updated.LastManifestFingerprint)
	}
	logText := readBotletsTeamResyncLog(t, paths)
	if !strings.Contains(logText, "botlets.team_resync_binding_report_failed") {
		t.Fatalf("log did not contain binding warning: %s", logText)
	}
	if strings.Contains(logText, cfg.RunnerSecret) {
		t.Fatalf("log leaked runner secret: %s", logText)
	}
}

// captureResyncHeartbeats returns the resyncError values (string, or nil for
// an explicit JSON null) seen on each /heartbeat POST, asserting the field is
// always present.
func captureResyncHeartbeats(t *testing.T, heartbeats *[]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("heartbeat body unreadable: %v", err)
		}
		value, present := body["resyncError"]
		if !present {
			t.Error("heartbeat body missing resyncError field")
		}
		*heartbeats = append(*heartbeats, value)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":3}`))
	}
}

func TestBotletsTeamResyncWorkerHeartbeatCarriesPriorResyncErrorAndClearsWhenUpToDate(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	var heartbeats []any
	heartbeatHandler := captureResyncHeartbeats(t, &heartbeats)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			heartbeatHandler(w, r)
		case strings.HasSuffix(r.URL.Path, "/team-manifest-status"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_7"}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:             "bw_workspace",
		RunnerID:                "br_runner",
		RunnerSecret:            "brs_secret_must_not_be_logged",
		BaseURL:                 server.URL,
		Runtime:                 "claude",
		LastManifestFingerprint: "btmf_7",
		// Non-nil empty list: a fully-synced zero-bot team stays on the fast
		// path (a nil list would force a backfill resync instead).
		LastManifestAgents: []string{},
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	restore := stubBotletsTeamResyncManifest(t, func(context.Context, *http.Client, commentbus.Paths, string, botletsTeamRuntimeConfig, string, string) ([]string, error) {
		t.Fatal("team manifest should not be fetched when fingerprint is unchanged")
		return nil, nil
	})
	defer restore()

	summary := runBotletsTeamResyncWorkerOnce(context.Background(), paths, botletsHome, "previous resync failure")
	if !summary.UpToDate || summary.ResyncError != "" {
		t.Fatalf("summary = %+v, want up-to-date with cleared resync error", summary)
	}
	if len(heartbeats) != 2 {
		t.Fatalf("heartbeats = %d, want 2 (carry prior + explicit clear): %v", len(heartbeats), heartbeats)
	}
	if heartbeats[0] != "previous resync failure" {
		t.Fatalf("heartbeat[0] resyncError = %v, want prior failure", heartbeats[0])
	}
	if heartbeats[1] != nil {
		t.Fatalf("heartbeat[1] resyncError = %v, want explicit null clear", heartbeats[1])
	}
}

func TestBotletsTeamResyncWorkerReportsFreshResyncErrorOnFailure(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	var heartbeats []any
	heartbeatHandler := captureResyncHeartbeats(t, &heartbeats)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			heartbeatHandler(w, r)
			return
		}
		t.Fatalf("unexpected request: %s", r.URL.Path)
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      server.URL,
		Runtime:      "claude",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	configureSyncForTest(t, paths, cfg.BaseURL)
	longError := strings.Repeat("x", 400)
	restore := stubBotletsTeamResyncManifest(t, func(context.Context, *http.Client, commentbus.Paths, string, botletsTeamRuntimeConfig, string, string) ([]string, error) {
		return nil, errors.New(longError)
	})
	defer restore()

	summary := runBotletsTeamResyncWorkerOnce(context.Background(), paths, botletsHome, "")
	want := strings.Repeat("x", 300)
	if summary.ResyncError != want {
		t.Fatalf("summary.ResyncError length = %d, want 300-char truncation", len(summary.ResyncError))
	}
	if len(heartbeats) != 1 {
		t.Fatalf("heartbeats = %d, want 1 fresh-failure report: %v", len(heartbeats), heartbeats)
	}
	if heartbeats[0] != want {
		t.Fatalf("heartbeat resyncError = %v, want truncated failure", heartbeats[0])
	}
	logText := readBotletsTeamResyncLog(t, paths)
	if !strings.Contains(logText, "botlets.team_resync_failed") {
		t.Fatalf("log did not contain resync failure: %s", logText)
	}
}

func TestBotletsTeamResyncWorkerClearsResyncErrorAfterSuccessfulResync(t *testing.T) {
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	var heartbeats []any
	heartbeatHandler := captureResyncHeartbeats(t, &heartbeats)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			heartbeatHandler(w, r)
			return
		}
		t.Fatalf("unexpected request: %s", r.URL.Path)
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      server.URL,
		Runtime:      "claude",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	configureSyncForTest(t, paths, cfg.BaseURL)
	restore := stubBotletsTeamResyncManifest(t, func(_ context.Context, _ *http.Client, _ commentbus.Paths, _ string, _ botletsTeamRuntimeConfig, _ string, priorResyncError string) ([]string, error) {
		if priorResyncError != "old resync failure" {
			t.Fatalf("priorResyncError = %q, want carried-over failure", priorResyncError)
		}
		return []string{"max.alpha"}, nil
	})
	defer restore()

	summary := runBotletsTeamResyncWorkerOnce(context.Background(), paths, botletsHome, "old resync failure")
	if summary.ResyncError != "" {
		t.Fatalf("summary.ResyncError = %q, want cleared", summary.ResyncError)
	}
	if len(heartbeats) != 1 {
		t.Fatalf("heartbeats = %d, want 1 explicit clear: %v", len(heartbeats), heartbeats)
	}
	if heartbeats[0] != nil {
		t.Fatalf("heartbeat resyncError = %v, want explicit null clear", heartbeats[0])
	}
}

func TestResyncBotletsTeamManifestBackfillsAgentsOnUnchangedFingerprint(t *testing.T) {
	// A team-runtime config persisted before LastManifestAgents existed carries
	// the fingerprint with no handle list. A successful full resync whose
	// server fingerprint is UNCHANGED must still persist the registered
	// handles — otherwise the fast-path local-install verification
	// (missingBotletsTeamLocalInstall) never becomes active for that install
	// until the server manifest happens to change (Codex round-5).
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":3}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest"):
			_, _ = w.Write([]byte(`{"ok":true,"manifest_fingerprint":"btmf_9","agents":[{"handle":"max.alpha","agent_secret":"as_alpha","bot_slug":"alpha","bot_agent_id":"ag_alpha","owner_agent_id":"ag_owner","bot_id":"bot_alpha","setup_generation":1,"brain":{"workspaceId":"bw_1","containerId":"cont_1","rootFolderId":"fold_1"}}]}`))
		case strings.HasSuffix(r.URL.Path, "/team-manifest/ack"):
			_, _ = w.Write([]byte(`{"ok":true}`))
		case strings.Contains(r.URL.Path, "/bindings/"):
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      server.URL,
		Runtime:      "claude",
		// Upgraded install: fingerprint persisted, handle list never was.
		LastManifestFingerprint: "btmf_9",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	restore := stubBotletsTeamRegisterLocally(t, func(_ context.Context, in botletsRegisterInput) (botletsRegisterResult, error) {
		return botletsRegisterResult{ProfilePath: filepath.Join(in.Paths.Home, "agents", in.BotHandle+".json")}, nil
	})
	defer restore()

	registered, err := resyncBotletsTeamManifest(context.Background(), http.DefaultClient, paths, botletsHome, cfg, "claude", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(registered, ",") != "max.alpha" {
		t.Fatalf("registered = %v, want max.alpha", registered)
	}
	updated, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		t.Fatal(err)
	}
	if updated.LastManifestFingerprint != "btmf_9" {
		t.Fatalf("last manifest fingerprint = %q, want btmf_9 kept", updated.LastManifestFingerprint)
	}
	if strings.Join(updated.LastManifestAgents, ",") != "max.alpha" {
		t.Fatalf("last manifest agents = %v, want backfilled [max.alpha] despite the unchanged fingerprint", updated.LastManifestAgents)
	}
}

func TestBotletsTeamResyncWorkerFailsPassWhenPairedSyncProvisionFails(t *testing.T) {
	// A paired team-runtime daemon whose sync self-provisioning fails (and
	// whose local sync is not otherwise configured) must FAIL the pass before
	// the manifest fetch: every fetch mints fresh runner-bound credentials,
	// and the registration that needs sync would then fail locally — minting
	// another credential set every 30 seconds forever (Codex round-5).
	paths := testBotletsTeamResyncPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	heartbeatErrors := []any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if value, ok := body["resyncError"]; ok {
				heartbeatErrors = append(heartbeatErrors, value)
			}
			_, _ = w.Write([]byte(`{"ok":true,"manifest_generation":3}`))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:  "bw_workspace",
		RunnerID:     "br_runner",
		RunnerSecret: "brs_secret_must_not_be_logged",
		BaseURL:      server.URL,
		Runtime:      "claude",
	}
	if err := writeBotletsTeamRuntimeConfig(botletsHome, cfg); err != nil {
		t.Fatal(err)
	}
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_worker-test",
		Token:    "ldt_ag_owner_ld_test_worker-secret-token",
		BaseURL:  server.URL,
		Label:    "Worker Test Mac",
	}); err != nil {
		t.Fatal(err)
	}
	prevSeam := ensureSyncConfiguredViaDaemonSeam
	ensureSyncConfiguredViaDaemonSeam = func(context.Context, commentbus.Paths, commentbus.DaemonAuth) error {
		return errors.New("sync root belongs to a different account")
	}
	t.Cleanup(func() { ensureSyncConfiguredViaDaemonSeam = prevSeam })
	restore := stubBotletsTeamResyncManifest(t, func(context.Context, *http.Client, commentbus.Paths, string, botletsTeamRuntimeConfig, string, string) ([]string, error) {
		t.Fatal("manifest must not be fetched (minting credentials) while sync is unusable")
		return nil, nil
	})
	defer restore()

	summary := runBotletsTeamResyncWorkerOnce(context.Background(), paths, botletsHome, "")
	if !strings.Contains(summary.ResyncError, "sync root belongs to a different account") {
		t.Fatalf("summary.ResyncError = %q, want the provisioning failure surfaced as the pass error", summary.ResyncError)
	}
	if len(heartbeatErrors) != 1 || heartbeatErrors[0] != "sync root belongs to a different account" {
		t.Fatalf("heartbeat resyncError reports = %v, want the fresh provisioning failure", heartbeatErrors)
	}
	logText := readBotletsTeamResyncLog(t, paths)
	if !strings.Contains(logText, "botlets.team_resync_sync_provision_failed") {
		t.Fatalf("log missing sync-provision failure: %s", logText)
	}
}
