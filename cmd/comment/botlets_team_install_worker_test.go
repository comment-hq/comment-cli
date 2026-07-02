package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

func teamInstallTestPaths(t *testing.T) commentbus.Paths {
	t.Helper()
	paths, err := commentbus.ResolvePaths(filepath.Join(t.TempDir(), ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// A successful poll redeems the runner over the pairing token, writes a complete
// team-runtime.json (the four-field resync gate + install id), and acks installed.
func TestBotletsTeamInstallWorkerRedeemsAndWritesConfig(t *testing.T) {
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	paths := teamInstallTestPaths(t)

	var ackedState string
	var sawCapabilityHeader bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/daemon/team-installs":
			if r.Header.Get("X-Comment-Daemon-Capabilities") == teamInstallCapability {
				sawCapabilityHeader = true
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"team_installs": []map[string]any{
					{"request_id": "tir_abc", "workspace_id": "bw_team1", "runtime": "claude", "state": "pending"},
				},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/redeem"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true, "request_id": "tir_abc",
				"runner_id": "br_runner1", "runner_secret": "brs_secret1",
				"workspace_id": "bw_team1", "runtime": "claude", "base_url": server2BaseURL(r),
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ack"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if s, ok := body["state"].(string); ok {
				ackedState = s
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_test", Token: "ldt_ag_owner_ld_test_tok", BaseURL: server.URL, Label: "Test Mac",
	}); err != nil {
		t.Fatal(err)
	}

	w := &botletsTeamInstallWorker{paths: paths, botletsHomeHint: botletsHome}
	if !w.runOnce(context.Background()) {
		t.Fatal("runOnce should report a steady (paired, polled) pass")
	}
	if !sawCapabilityHeader {
		t.Fatal("worker must advertise team_install:v1 in its capability header")
	}
	cfg, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		t.Fatalf("team-runtime.json not written: %v", err)
	}
	if cfg.RunnerID != "br_runner1" || cfg.RunnerSecret != "brs_secret1" || cfg.WorkspaceID != "bw_team1" {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.BaseURL == "" || cfg.InstallationID == "" {
		t.Fatalf("config missing base_url / installation_id: %+v", cfg)
	}
	if ackedState != "installed" {
		t.Fatalf("ack state = %q, want installed", ackedState)
	}
}

// The worker must NOT clobber a team-runtime.json already configured for a
// DIFFERENT workspace — a second pending install for another workspace is skipped.
func TestBotletsTeamInstallWorkerSkipsOtherWorkspace(t *testing.T) {
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	paths := teamInstallTestPaths(t)

	if err := writeBotletsTeamRuntimeConfig(botletsHome, botletsTeamRuntimeConfig{
		WorkspaceID: "bw_existing", RunnerID: "br_existing", RunnerSecret: "brs_existing",
		BaseURL: "http://existing", Runtime: "claude", InstallationID: "inst-existing",
	}); err != nil {
		t.Fatal(err)
	}

	redeemCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/daemon/team-installs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"team_installs": []map[string]any{
					{"request_id": "tir_new", "workspace_id": "bw_new", "runtime": "claude", "state": "pending"},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/redeem"):
			redeemCalled = true
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer server.Close()

	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_test", Token: "ldt_ag_owner_ld_test_tok", BaseURL: server.URL, Label: "Test Mac",
	}); err != nil {
		t.Fatal(err)
	}

	w := &botletsTeamInstallWorker{paths: paths, botletsHomeHint: botletsHome}
	w.runOnce(context.Background())

	if redeemCalled {
		t.Fatal("worker must not redeem a different workspace while one is already configured")
	}
	cfg, err := readBotletsTeamRuntimeConfig(botletsHome)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkspaceID != "bw_existing" || cfg.RunnerID != "br_existing" {
		t.Fatalf("existing team runtime was clobbered: %+v", cfg)
	}
}

// A transient redeem failure (e.g. a 5xx while the workspace runner is being
// provisioned) must NOT be acked 'failed'. The redeem route consumes the request
// before it provisions, and consume is idempotent on the same key, so the server
// leaves the request 'redeemed' and re-returns it on the next poll for an
// idempotent retry. Acking 'failed' here would flip it terminal and permanently
// break that recovery — failing the browser install on a momentary blip.
func TestBotletsTeamInstallWorkerLeavesTransientRedeemFailureUnacked(t *testing.T) {
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	paths := teamInstallTestPaths(t)

	ackCalled := false
	var ackedState string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/daemon/team-installs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"team_installs": []map[string]any{
					{"request_id": "tir_abc", "workspace_id": "bw_team1", "runtime": "claude", "state": "pending"},
				},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/redeem"):
			// Runner provisioning is transiently unavailable.
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ack"):
			ackCalled = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if s, ok := body["state"].(string); ok {
				ackedState = s
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_test", Token: "ldt_ag_owner_ld_test_tok", BaseURL: server.URL, Label: "Test Mac",
	}); err != nil {
		t.Fatal(err)
	}

	w := &botletsTeamInstallWorker{paths: paths, botletsHomeHint: botletsHome}
	w.runOnce(context.Background())

	if ackCalled {
		t.Fatalf("worker acked %q on a transient redeem failure; must leave the request unacked for the next poll to retry", ackedState)
	}
	if _, err := readBotletsTeamRuntimeConfig(botletsHome); err == nil {
		t.Fatal("team-runtime.json must not be written when redeem fails")
	}
}

// A TERMINAL redeem failure (a 4xx — e.g. the workspace authz epoch went stale,
// or the request was cancelled) MUST be acked 'failed'. The request was already
// consumed (pending -> redeemed) and the owner DO's sweep only expires 'pending'
// requests, so an unacked terminal failure would be re-redeemed every poll
// forever and the browser would hang on "Installing…". Acking 'failed' moves it
// terminal so the browser can fall back to the setup command.
func TestBotletsTeamInstallWorkerAcksTerminalRedeemFailure(t *testing.T) {
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	paths := teamInstallTestPaths(t)

	var ackedState, ackedCode string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/daemon/team-installs":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"team_installs": []map[string]any{
					{"request_id": "tir_abc", "workspace_id": "bw_team1", "runtime": "claude", "state": "pending"},
				},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/redeem"):
			// Non-retryable: the workspace authz epoch is stale.
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "stale", "code": "TEAM_INSTALL_STALE"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ack"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if s, ok := body["state"].(string); ok {
				ackedState = s
			}
			if code, ok := body["failure_code"].(string); ok {
				ackedCode = code
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_test", Token: "ldt_ag_owner_ld_test_tok", BaseURL: server.URL, Label: "Test Mac",
	}); err != nil {
		t.Fatal(err)
	}

	w := &botletsTeamInstallWorker{paths: paths, botletsHomeHint: botletsHome}
	w.runOnce(context.Background())

	if ackedState != "failed" {
		t.Fatalf("ack state = %q, want failed on a terminal redeem failure", ackedState)
	}
	if ackedCode != "REDEEM_FAILED" {
		t.Fatalf("ack failure_code = %q, want REDEEM_FAILED", ackedCode)
	}
}

// server2BaseURL returns a base url for the redeem response body. The handler
// can't see its own server.URL, so derive it from the request host.
func server2BaseURL(r *http.Request) string {
	return "http://" + r.Host
}
