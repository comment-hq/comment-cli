//go:build darwin || linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

func stubBusPairSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	restore := busPairSleep
	slept := &[]time.Duration{}
	busPairSleep = func(d time.Duration) {
		*slept = append(*slept, d)
	}
	t.Cleanup(func() { busPairSleep = restore })
	return slept
}

func testBusPairHome(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), ".comment-io")
}

const testBusPairDeviceCode = "dvc_11111111-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222"

func busPairStartHandler(t *testing.T, serverURL func() string, interval int, expiresIn int) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("pair start method = %s", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("pair start body unreadable: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":               testBusPairDeviceCode,
			"user_code":                 "ABCDEFGH",
			"verification_uri":          serverURL() + "/setup/daemon",
			"verification_uri_complete": serverURL() + "/setup/daemon?code=ABCDEFGH",
			"interval":                  interval,
			"expires_in":                expiresIn,
		})
	}
}

func TestBusPairHappyPathWritesDaemonAuth(t *testing.T) {
	home := testBusPairHome(t)
	slept := stubBusPairSleep(t)
	var redeems int32
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/pair/start", busPairStartHandler(t, func() string { return server.URL }, 1, 600))
	mux.HandleFunc("/daemon/pair/redeem", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("redeem body unreadable: %v", err)
		}
		if body["device_code"] != testBusPairDeviceCode {
			t.Errorf("redeem device_code = %#v", body["device_code"])
		}
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&redeems, 1) == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":    "authorization_pending",
				"code":     "AUTHORIZATION_PENDING",
				"interval": 1,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"daemon_token": "ldt_ag_owner_ld_x_test-secret-token",
			"daemon_id":    "ld_aaaa",
			"owner_handle": "max",
			"label":        "Test Mac",
			"capabilities": []string{"agent_enrollment:v1"},
		})
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	var out bytes.Buffer
	if err := busPair(&out, home, "Test Mac", server.URL, false); err != nil {
		t.Fatalf("busPair err = %v\noutput:\n%s", err, out.String())
	}
	if got := atomic.LoadInt32(&redeems); got != 2 {
		t.Fatalf("redeem calls = %d, want 2", got)
	}
	output := out.String()
	if !strings.Contains(output, server.URL+"/setup/daemon?code=ABCDEFGH") {
		t.Fatalf("output missing verification_uri_complete:\n%s", output)
	}
	if !strings.Contains(output, "ABCDEFGH") {
		t.Fatalf("output missing user code:\n%s", output)
	}
	if strings.Contains(output, "ldt_ag_owner_ld_x_test-secret-token") {
		t.Fatalf("output leaked the daemon token:\n%s", output)
	}
	if !strings.Contains(output, server.URL+"/s/welcome") {
		t.Fatalf("output missing Guy welcome guide link:\n%s", output)
	}
	if len(*slept) == 0 {
		t.Fatal("pair never waited between polls")
	}

	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(commentbus.DaemonAuthPath(paths))
	if err != nil {
		t.Fatalf("daemon-auth.json missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("daemon-auth.json mode = %v, want 0600", info.Mode().Perm())
	}
	auth, ok, err := commentbus.LoadDaemonAuth(paths)
	if err != nil || !ok {
		t.Fatalf("LoadDaemonAuth = ok %v err %v", ok, err)
	}
	if auth.Token != "ldt_ag_owner_ld_x_test-secret-token" || auth.DaemonID != "ld_aaaa" {
		t.Fatalf("saved auth = %+v", auth)
	}
	if auth.Label != "Test Mac" || auth.BaseURL != server.URL {
		t.Fatalf("saved auth label/base = %+v", auth)
	}
	if len(auth.Capabilities) != 1 || auth.Capabilities[0] != "agent_enrollment:v1" {
		t.Fatalf("saved capabilities = %#v", auth.Capabilities)
	}
}

func TestBusPairAlreadyPairedIsANoop(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_existing",
		Token:    "ldt_existing_secret",
		Label:    "Existing Mac",
	}); err != nil {
		t.Fatal(err)
	}
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	var out bytes.Buffer
	if err := busPair(&out, home, "", server.URL, false); err != nil {
		t.Fatalf("busPair err = %v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 0 {
		t.Fatalf("already-paired pair made %d server requests, want 0", got)
	}
	if !strings.Contains(out.String(), "already paired") || !strings.Contains(out.String(), "Existing Mac") {
		t.Fatalf("output = %q, want already-paired message with label", out.String())
	}
}

func TestBusPairRespectsSlowDownBackoff(t *testing.T) {
	home := testBusPairHome(t)
	slept := stubBusPairSleep(t)
	var redeems int32
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/pair/start", busPairStartHandler(t, func() string { return server.URL }, 1, 600))
	mux.HandleFunc("/daemon/pair/redeem", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&redeems, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":    "slow_down",
				"code":     "SLOW_DOWN",
				"interval": 5,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"daemon_token": "ldt_slow_secret",
			"daemon_id":    "ld_slow",
			"label":        "Slow Mac",
			"capabilities": []string{"agent_enrollment:v1"},
		})
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	var out bytes.Buffer
	if err := busPair(&out, home, "Slow Mac", server.URL, false); err != nil {
		t.Fatalf("busPair err = %v", err)
	}
	if len(*slept) < 2 {
		t.Fatalf("sleeps = %v, want at least 2", *slept)
	}
	first, second := (*slept)[0], (*slept)[1]
	if second <= first {
		t.Fatalf("slow_down did not back off: first wait %v, next wait %v", first, second)
	}
	if second < 5*time.Second {
		t.Fatalf("slow_down wait = %v, want at least the server-requested 5s", second)
	}
}

func TestBusPairExpiredCodeFailsWithoutWritingAuth(t *testing.T) {
	home := testBusPairHome(t)
	stubBusPairSleep(t)
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/pair/start", busPairStartHandler(t, func() string { return server.URL }, 1, 600))
	mux.HandleFunc("/daemon/pair/redeem", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusGone)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "pair_code_expired",
			"code":  "PAIR_CODE_EXPIRED",
		})
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	var out bytes.Buffer
	err := busPair(&out, home, "Test Mac", server.URL, false)
	if err == nil || !strings.Contains(err.Error(), "expired") || !strings.Contains(err.Error(), "comment bus pair") {
		t.Fatalf("expired pair err = %v, want expired + re-run guidance", err)
	}
	paths, pathsErr := commentbus.ResolvePaths(home)
	if pathsErr != nil {
		t.Fatal(pathsErr)
	}
	if _, statErr := os.Stat(commentbus.DaemonAuthPath(paths)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-auth.json should not exist after expiry, stat err = %v", statErr)
	}
}

func TestBusPairAlreadyRedeemedCodeFails(t *testing.T) {
	home := testBusPairHome(t)
	stubBusPairSleep(t)
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/pair/start", busPairStartHandler(t, func() string { return server.URL }, 1, 600))
	mux.HandleFunc("/daemon/pair/redeem", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": "pair_code_already_redeemed",
			"code":  "PAIR_CODE_ALREADY_REDEEMED",
		})
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	var out bytes.Buffer
	err := busPair(&out, home, "Test Mac", server.URL, false)
	if err == nil || !strings.Contains(err.Error(), "already redeemed") {
		t.Fatalf("already-redeemed pair err = %v", err)
	}
	// 409 can also mean THIS terminal's redeem committed but the response was
	// lost; the error must name the orphaned-pairing recovery path.
	if !strings.Contains(err.Error(), "Paired computers") {
		t.Fatalf("already-redeemed pair err = %v, want Settings -> Paired computers recovery guidance", err)
	}
}

func TestBusPairPollRedeemRetriesTransportFailure(t *testing.T) {
	// A network-level redeem failure is AMBIGUOUS: the request may have reached
	// the Worker and committed the pairing even though the response was lost.
	// The poll loop must keep polling (an uncommitted attempt succeeds later;
	// a committed one answers 409) instead of aborting and dropping the only
	// delivery of the daemon token.
	home := testBusPairHome(t)
	stubBusPairSleep(t)
	var redeems int32
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/pair/start", busPairStartHandler(t, func() string { return server.URL }, 1, 600))
	mux.HandleFunc("/daemon/pair/redeem", func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&redeems, 1) == 1 {
			// Kill the connection before writing a response: the client sees a
			// transport error (EOF), not an HTTP status.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support hijacking")
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Fatalf("hijack failed: %v", err)
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"daemon_token": "ldt_ag_owner_ld_x_retry-secret-token",
			"daemon_id":    "ld_retry",
			"owner_handle": "max",
			"label":        "Retry Mac",
		})
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	var out bytes.Buffer
	if err := busPair(&out, home, "Retry Mac", server.URL, false); err != nil {
		t.Fatalf("busPair must survive a transient redeem transport failure: %v", err)
	}
	if got := atomic.LoadInt32(&redeems); got != 2 {
		t.Fatalf("redeem calls = %d, want 2 (one dropped, one retried)", got)
	}
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	auth, ok, err := commentbus.LoadDaemonAuth(paths)
	if err != nil || !ok || auth.DaemonID != "ld_retry" {
		t.Fatalf("LoadDaemonAuth after retry = ok %v err %v auth %+v", ok, err, auth)
	}
}

func TestBusPairEmitsHeartbeatWhileWaiting(t *testing.T) {
	// While waiting for the user to approve the pairing in their browser, the
	// poll loop must periodically reprint a progress line (with the URL + code)
	// so a long wait reads as "waiting for you" rather than a hung install.
	home := testBusPairHome(t)
	stubBusPairSleep(t)
	var redeems int32
	var server *httptest.Server
	mux := http.NewServeMux()
	// interval 30s so each stubbed poll accumulates a full heartbeat window.
	mux.HandleFunc("/daemon/pair/start", busPairStartHandler(t, func() string { return server.URL }, 30, 600))
	mux.HandleFunc("/daemon/pair/redeem", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if atomic.AddInt32(&redeems, 1) < 3 {
			// Not approved yet: keep the user waiting for a couple of polls.
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"daemon_token": "ldt_ag_owner_ld_x_hb-secret-token",
			"daemon_id":    "ld_hb",
			"owner_handle": "max",
			"label":        "HB Mac",
		})
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	var out bytes.Buffer
	if err := busPair(&out, home, "HB Mac", server.URL, false); err != nil {
		t.Fatalf("busPair: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "still waiting for you to approve") {
		t.Fatalf("expected a heartbeat progress line during the wait; got:\n%s", got)
	}
	if !strings.Contains(got, "ABCDEFGH") {
		t.Fatalf("heartbeat should re-show the pairing code; got:\n%s", got)
	}
}

func TestBusPairSelfRevokesWhenAuthSaveFails(t *testing.T) {
	// If the server pairing commits but the daemon credentials cannot be
	// persisted locally, the just-received token is the ONLY thing that can
	// still revoke the server-side daemon — so the CLI must self-revoke before
	// surfacing the error, or the computer lingers paired-but-unusable.
	home := testBusPairHome(t)
	stubBusPairSleep(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	// Make SaveDaemonAuth fail: a non-empty DIRECTORY at the daemon-auth.json
	// path defeats the atomic rename.
	if err := os.MkdirAll(filepath.Join(commentbus.DaemonAuthPath(paths), "block"), 0o700); err != nil {
		t.Fatal(err)
	}
	var revokes int32
	var sawRevokeAuth string
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/pair/start", busPairStartHandler(t, func() string { return server.URL }, 1, 600))
	mux.HandleFunc("/daemon/pair/redeem", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"daemon_token": "ldt_ag_owner_ld_x_unsavable-token",
			"daemon_id":    "ld_unsavable",
			"owner_handle": "max",
			"label":        "Unsavable Mac",
		})
	})
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&revokes, 1)
		sawRevokeAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	var out bytes.Buffer
	err = busPair(&out, home, "Unsavable Mac", server.URL, true)
	if err == nil || !strings.Contains(err.Error(), "could not be saved") {
		t.Fatalf("busPair err = %v, want save failure", err)
	}
	if got := atomic.LoadInt32(&revokes); got != 1 {
		t.Fatalf("self-revoke calls = %d, want 1", got)
	}
	if sawRevokeAuth != "Bearer ldt_ag_owner_ld_x_unsavable-token" {
		t.Fatalf("self-revoke Authorization = %q, want the just-redeemed token", sawRevokeAuth)
	}
	if !strings.Contains(err.Error(), "revoked on the server") {
		t.Fatalf("busPair err = %v, want confirmation the orphaned pairing was revoked", err)
	}
	if strings.Contains(err.Error(), "ldt_ag_owner_ld_x_unsavable-token") {
		t.Fatalf("error leaked the daemon token: %v", err)
	}
}

func TestBusUnpairDeletesDaemonAuthWithYes(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_gone",
		Token:    "ldt_gone_secret",
		Label:    "Old Mac",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := busUnpair(&out, home, true); err != nil {
		t.Fatalf("busUnpair err = %v", err)
	}
	if _, statErr := os.Stat(commentbus.DaemonAuthPath(paths)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-auth.json still exists after unpair, stat err = %v", statErr)
	}
	output := out.String()
	if !strings.Contains(output, "Paired computers") {
		t.Fatalf("unpair output does not point at web-app revocation:\n%s", output)
	}
	if strings.Contains(output, "ldt_gone_secret") {
		t.Fatalf("unpair output leaked the daemon token:\n%s", output)
	}
}

func TestBusUnpairWithoutAuthIsANoop(t *testing.T) {
	home := testBusPairHome(t)
	var out bytes.Buffer
	if err := busUnpair(&out, home, true); err != nil {
		t.Fatalf("busUnpair err = %v", err)
	}
	if !strings.Contains(out.String(), "not paired") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestBusUnpairCleansEnrollmentInstalledProfiles(t *testing.T) {
	// A CONFIRMED self-revoke kills every credential this daemon's enrollments
	// installed, so unpair must also clean the journaled profiles holding
	// those now-dead as_ tokens — left on disk they wedge a later re-pair
	// (the owned-agents reconciler treats an existing profile as installed
	// and never re-enrolls for fresh credentials). Manual installs (no journal
	// entry) are kept, and an enrollment that overwrote a manual install
	// restores its pre-enrollment backup (Codex round-6).
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profileJSON := func(handle, secret string) []byte {
		return []byte(`{"handle":"` + handle + `","agent_secret":"` + secret + `","base_url":"https://comment.io"}` + "\n")
	}
	// Enrollment-created install: journaled, hash matches -> removed.
	enrolledPath := filepath.Join(agentsDir, "max.enrolled.json")
	if err := os.WriteFile(enrolledPath, profileJSON("max.enrolled", "as_enrolled_1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: "enr_aaaaaaaa-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222",
		Handle:       "max.enrolled",
		ProfilePath:  enrolledPath,
		SecretSHA256: enrollSecretSHA256("as_enrolled_1"),
	}); err != nil {
		t.Fatal(err)
	}
	// Enrollment that overwrote a manual install (backup sidecar) -> restored.
	overwrittenPath := filepath.Join(agentsDir, "max.overwritten.json")
	if err := os.WriteFile(overwrittenPath, profileJSON("max.overwritten", "as_enrolled_2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overwrittenPath+".enroll-backup", profileJSON("max.overwritten", "as_manual_original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: "enr_bbbbbbbb-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222",
		Handle:       "max.overwritten",
		ProfilePath:  overwrittenPath,
		SecretSHA256: enrollSecretSHA256("as_enrolled_2"),
	}); err != nil {
		t.Fatal(err)
	}
	// Purely manual install: no journal entry -> never touched.
	manualPath := filepath.Join(agentsDir, "max.manual.json")
	if err := os.WriteFile(manualPath, profileJSON("max.manual", "as_manual"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_cleanup",
		Token:    "ldt_cleanup_secret",
		BaseURL:  server.URL,
		Label:    "Cleanup Mac",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := busUnpair(&out, home, true); err != nil {
		t.Fatalf("busUnpair err = %v", err)
	}
	if _, statErr := os.Stat(enrolledPath); !os.IsNotExist(statErr) {
		t.Fatalf("enrollment-installed profile still on disk after unpair, stat err = %v", statErr)
	}
	restored, err := os.ReadFile(overwrittenPath)
	if err != nil {
		t.Fatalf("overwritten manual install missing after unpair: %v", err)
	}
	if !strings.Contains(string(restored), "as_manual_original") {
		t.Fatalf("profile = %q, want the pre-enrollment manual install restored", string(restored))
	}
	if _, statErr := os.Stat(overwrittenPath + ".enroll-backup"); !os.IsNotExist(statErr) {
		t.Fatalf("backup sidecar should be consumed by the restore, stat err = %v", statErr)
	}
	manual, err := os.ReadFile(manualPath)
	if err != nil {
		t.Fatalf("manual install was deleted by unpair: %v", err)
	}
	if !strings.Contains(string(manual), "as_manual") {
		t.Fatalf("manual profile = %q, want it untouched", string(manual))
	}
	if _, statErr := os.Stat(enrollJournalPath(paths)); !os.IsNotExist(statErr) {
		t.Fatalf("enroll journal still on disk after unpair, stat err = %v", statErr)
	}
	if !strings.Contains(record.joined(), "reload:") {
		t.Fatalf("call sequence = %q, want a daemon reload after the profile cleanup", record.joined())
	}
	if !strings.Contains(out.String(), "Cleaned up 2 enrollment-installed agent profile(s)") {
		t.Fatalf("unpair output missing the cleanup summary:\n%s", out.String())
	}
}

func TestBusUnpairCleansOnlyRevokedDaemonProfiles(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profileJSON := func(handle, secret string) []byte {
		return []byte(`{"handle":"` + handle + `","agent_secret":"` + secret + `","base_url":"https://comment.io"}` + "\n")
	}
	oldPath := filepath.Join(agentsDir, "max.old.json")
	if err := os.WriteFile(oldPath, profileJSON("max.old", "as_old_daemon"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldID := "enr_11111111-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222"
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: oldID,
		DaemonID:     "ld_old",
		Handle:       "max.old",
		ProfilePath:  oldPath,
		SecretSHA256: enrollSecretSHA256("as_old_daemon"),
	}); err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(agentsDir, "max.new.json")
	if err := os.WriteFile(newPath, profileJSON("max.new", "as_new_daemon"), 0o600); err != nil {
		t.Fatal(err)
	}
	newID := "enr_33333333-3333-4333-8333-333333333333_44444444-4444-4444-8444-444444444444"
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: newID,
		DaemonID:     "ld_new",
		Handle:       "max.new",
		ProfilePath:  newPath,
		SecretSHA256: enrollSecretSHA256("as_new_daemon"),
	}); err != nil {
		t.Fatal(err)
	}
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_new",
		Token:    "ldt_new_secret",
		BaseURL:  server.URL,
		Label:    "New Mac",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := busUnpair(&out, home, true); err != nil {
		t.Fatalf("busUnpair err = %v", err)
	}

	oldProfile, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatalf("old-daemon profile should survive new-daemon unpair: %v", err)
	}
	if !strings.Contains(string(oldProfile), "as_old_daemon") {
		t.Fatalf("old profile = %q, want preserved old-daemon credential", string(oldProfile))
	}
	if _, statErr := os.Stat(newPath); !os.IsNotExist(statErr) {
		t.Fatalf("new-daemon profile should be removed after unpair, stat err = %v", statErr)
	}
	remaining, err := enrollJournalLoad(paths)
	if err != nil {
		t.Fatalf("journal load after daemon-scoped cleanup: %v", err)
	}
	if _, ok := remaining[oldID]; !ok {
		t.Fatalf("old-daemon entry should be retained in the journal: %#v", remaining)
	}
	if _, ok := remaining[newID]; ok {
		t.Fatalf("new-daemon entry should be pruned after cleanup: %#v", remaining)
	}
	if len(remaining) != 1 {
		t.Fatalf("journal should keep exactly the old-daemon entry, got %d: %#v", len(remaining), remaining)
	}
	if !strings.Contains(record.joined(), "reload:") {
		t.Fatalf("call sequence = %q, want a daemon reload after the profile cleanup", record.joined())
	}
	if !strings.Contains(out.String(), "Cleaned up 1 enrollment-installed agent profile(s)") {
		t.Fatalf("unpair output missing the cleanup summary:\n%s", out.String())
	}
}

func TestBusUnpairKeepsJournalEntryWhenProfileCleanupFails(t *testing.T) {
	// When a confirmed self-revoke kills the credentials but a per-profile
	// cleanup FAILS (here: an unwritable/blocked profile restore), unpair must
	// NOT wipe the whole journal — it must keep the failed entry (and the
	// journal file) so a later retry can still tie the stale profile to the
	// revoked credential, prune only the entries it actually cleaned, and tell
	// the user which profiles still need cleanup (Codex round-7).
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profileJSON := func(handle, secret string) []byte {
		return []byte(`{"handle":"` + handle + `","agent_secret":"` + secret + `","base_url":"https://comment.io"}` + "\n")
	}
	// Entry that cleans successfully -> pruned from the journal.
	cleanPath := filepath.Join(agentsDir, "max.clean.json")
	if err := os.WriteFile(cleanPath, profileJSON("max.clean", "as_clean_1"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanID := "enr_cccccccc-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222"
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: cleanID,
		Handle:       "max.clean",
		ProfilePath:  cleanPath,
		SecretSHA256: enrollSecretSHA256("as_clean_1"),
	}); err != nil {
		t.Fatal(err)
	}
	// Entry whose restore FAILS: its backup sidecar is a directory, so the
	// restore's rename(backup -> profile) errors deterministically (rename of a
	// directory onto an existing regular file fails for any uid). The profile
	// stays on disk and the entry must be KEPT.
	stuckPath := filepath.Join(agentsDir, "max.stuck.json")
	if err := os.WriteFile(stuckPath, profileJSON("max.stuck", "as_stuck_1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stuckPath+".enroll-backup", 0o700); err != nil {
		t.Fatal(err)
	}
	stuckID := "enr_dddddddd-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222"
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: stuckID,
		Handle:       "max.stuck",
		ProfilePath:  stuckPath,
		SecretSHA256: enrollSecretSHA256("as_stuck_1"),
	}); err != nil {
		t.Fatal(err)
	}
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_partial",
		Token:    "ldt_partial_secret",
		BaseURL:  server.URL,
		Label:    "Partial Mac",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := busUnpair(&out, home, true); err != nil {
		t.Fatalf("busUnpair err = %v", err)
	}

	// The clean profile is gone; the stuck profile (failed cleanup) survives.
	if _, statErr := os.Stat(cleanPath); !os.IsNotExist(statErr) {
		t.Fatalf("clean profile still on disk after unpair, stat err = %v", statErr)
	}
	stuck, err := os.ReadFile(stuckPath)
	if err != nil {
		t.Fatalf("stuck profile should survive a failed cleanup: %v", err)
	}
	if !strings.Contains(string(stuck), "as_stuck_1") {
		t.Fatalf("stuck profile = %q, want it left intact for retry", string(stuck))
	}

	// The journal is pruned to only the failed entry, not wiped.
	remaining, err := enrollJournalLoad(paths)
	if err != nil {
		t.Fatalf("journal load after partial cleanup: %v", err)
	}
	if _, ok := remaining[cleanID]; ok {
		t.Fatalf("cleaned entry should be pruned from the journal: %#v", remaining)
	}
	if _, ok := remaining[stuckID]; !ok {
		t.Fatalf("failed-cleanup entry must be kept in the journal: %#v", remaining)
	}
	if len(remaining) != 1 {
		t.Fatalf("journal should keep exactly the 1 failed entry, got %d: %#v", len(remaining), remaining)
	}
	if _, statErr := os.Stat(enrollJournalPath(paths)); statErr != nil {
		t.Fatalf("enroll journal file should remain while entries are kept, stat err = %v", statErr)
	}

	// The user is told which profile still needs cleanup, and daemon-auth.json
	// is still removed (the credentials are revoked server-side regardless).
	if !strings.Contains(out.String(), "still need cleanup") || !strings.Contains(out.String(), stuckPath) {
		t.Fatalf("unpair output missing the still-needs-cleanup note for %s:\n%s", stuckPath, out.String())
	}
	if _, statErr := os.Stat(commentbus.DaemonAuthPath(paths)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-auth.json should be removed after unpair, stat err = %v", statErr)
	}
}

func TestBusUnpairRetriesRegistryOnlyPendingCleanup(t *testing.T) {
	// A prior unpair pass removed THIS Botlets install's profile but failed to
	// rewrite the registry, so the entry is registry-only incomplete: the
	// profile file is gone (its secret hash no longer matches) yet the Botlets
	// registry still lists the handle. The plain `!fileExists` gate would call
	// it "not ours" and prune the journal record WITHOUT removing the stale
	// registry row, leaving a permanent MISSING_CREDENTIAL_PROFILE. The retry
	// must instead recognize file-gone + registry-present as ours-and-incomplete
	// and finish the registry removal (Codex round-8).
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	writeBotletsRegistryForEnrollmentTest(t, botletsHome, "max.bot")
	// The profile this install wrote is already gone (removed by the earlier
	// pass); only its journal entry and the un-rolled-back registry row remain.
	profilePath := filepath.Join(agentsDir, "max.bot.json")
	entryID := "enr_eeeeeeee-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222"
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID:  entryID,
		Handle:        "max.bot",
		ProfilePath:   profilePath,
		SecretSHA256:  enrollSecretSHA256("as_gone"),
		BotletsHandle: "max.bot",
		BotletsHome:   botletsHome,
	}); err != nil {
		t.Fatal(err)
	}
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)

	var out bytes.Buffer
	busUnpairCleanupEnrolledProfiles(&out, paths, "ld_retry")

	// The stale registry row is removed ...
	if handles := readBotletsRegistryHandlesForTest(t, botletsHome); len(handles) != 0 {
		t.Fatalf("registry handles = %v, want the registry-only pending row removed on retry", handles)
	}
	// ... and only now is the journal record pruned (file removed, nothing left).
	if _, statErr := os.Stat(enrollJournalPath(paths)); !os.IsNotExist(statErr) {
		remaining, _ := enrollJournalLoad(paths)
		t.Fatalf("journal should be pruned once the registry row is gone, remaining=%#v stat err=%v", remaining, statErr)
	}
	if !strings.Contains(out.String(), "Cleaned up 1 enrollment-installed agent profile(s)") {
		t.Fatalf("unpair output missing the cleanup summary:\n%s", out.String())
	}
}

func TestBusUnpairInvalidTokenDoesNotCleanProfiles(t *testing.T) {
	// daemon-auth.json is present and parses, but the token is stale/corrupted,
	// so /daemon/self-revoke answers 401 INVALID_DAEMON_TOKEN — the SAME status
	// cf returns for an already-revoked daemon (requireDaemonAuth has no distinct
	// "already revoked" signal). The CLI cannot prove the server revoked
	// anything, so it must NOT run the credential-revoking profile cleanup and
	// must point the user at the web app instead (Codex round-13).
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	enrolledPath := filepath.Join(agentsDir, "max.enrolled.json")
	if err := os.WriteFile(enrolledPath, []byte(`{"handle":"max.enrolled","agent_secret":"as_enrolled_1","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: "enr_eeeeeeee-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222",
		Handle:       "max.enrolled",
		ProfilePath:  enrolledPath,
		SecretSHA256: enrollSecretSHA256("as_enrolled_1"),
	}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "Invalid or revoked daemon token", "code": "INVALID_DAEMON_TOKEN"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_stale",
		Token:    "ldt_stale_secret",
		BaseURL:  server.URL,
		Label:    "Stale Mac",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := busUnpair(&out, home, true); err != nil {
		t.Fatalf("busUnpair err = %v", err)
	}

	// The enrollment profile and journal are LEFT IN PLACE (revoke not confirmed).
	if _, statErr := os.Stat(enrolledPath); statErr != nil {
		t.Fatalf("enrollment profile must survive an unconfirmed revoke: %v", statErr)
	}
	if _, statErr := os.Stat(enrollJournalPath(paths)); statErr != nil {
		t.Fatalf("enroll journal must survive an unconfirmed revoke: %v", statErr)
	}
	output := out.String()
	if strings.Contains(output, "Cleaned up") {
		t.Fatalf("unpair must not report cleanup on an unconfirmed revoke:\n%s", output)
	}
	if strings.Contains(output, "already revoked") {
		t.Fatalf("a rejected token must not be reported as already revoked:\n%s", output)
	}
	if !strings.Contains(output, "could NOT be confirmed") || !strings.Contains(output, "Paired computers") {
		t.Fatalf("unpair output missing the actionable web-app revoke guidance:\n%s", output)
	}
	// daemon-auth.json is still removed (a stale local token has no value).
	if _, statErr := os.Stat(commentbus.DaemonAuthPath(paths)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-auth.json should still be removed, stat err = %v", statErr)
	}
}

func TestBusPairForcePreservesExistingAgentProfiles(t *testing.T) {
	// `comment bus pair --force` is a local daemon re-pair. It must not
	// implicitly revoke the OLD daemon or clean its enrollment-installed
	// profiles, because that deletes/revokes the as_ credentials that
	// long-running agent sessions may still need. Explicit `comment bus unpair`
	// remains the credential-revoking cleanup command.
	home := testBusPairHome(t)
	_ = stubBusPairSleep(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	// An enrollment-installed profile attributed to the old daemon.
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldProfile := filepath.Join(agentsDir, "max.oldbot.json")
	if err := os.WriteFile(oldProfile, []byte(`{"handle":"max.oldbot","agent_secret":"as_old_1","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: "enr_ffffffff-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222",
		Handle:       "max.oldbot",
		ProfilePath:  oldProfile,
		SecretSHA256: enrollSecretSHA256("as_old_1"),
	}); err != nil {
		t.Fatal(err)
	}
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)

	var server *httptest.Server
	var revokedCount int32
	var replacedCount int32
	var sawReplaceAuth string
	var sawReplacePrevious string
	var sawReplacePreviousToken string
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/pair/start", busPairStartHandler(t, func() string { return server.URL }, 1, 600))
	mux.HandleFunc("/daemon/pair/redeem", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"daemon_token": "ldt_new_secret",
			"daemon_id":    "ld_new",
			"owner_handle": "max",
			"label":        "New Mac",
			"capabilities": []string{"agent_enrollment:v1"},
		})
	})
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&revokedCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/daemon/replace-previous", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&replacedCount, 1)
		sawReplaceAuth = r.Header.Get("Authorization")
		var body struct {
			PreviousDaemonID    string `json:"previous_daemon_id"`
			PreviousDaemonToken string `json:"previous_daemon_token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("replace-previous body decode: %v", err)
		}
		sawReplacePrevious = body.PreviousDaemonID
		sawReplacePreviousToken = body.PreviousDaemonToken
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server = httptest.NewServer(mux)
	defer server.Close()
	// Existing pairing for the OLD daemon, pointed at this server.
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_old",
		Token:    "ldt_old_secret",
		BaseURL:  server.URL,
		Label:    "Old Mac",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := busPair(&out, home, "New Mac", server.URL, true); err != nil {
		t.Fatalf("busPair --force err = %v\noutput:\n%s", err, out.String())
	}

	if atomic.LoadInt32(&revokedCount) != 0 {
		t.Fatalf("old daemon self-revoke calls = %d, want 0", revokedCount)
	}
	if atomic.LoadInt32(&replacedCount) != 1 {
		t.Fatalf("replace-previous calls = %d, want 1", replacedCount)
	}
	if sawReplaceAuth != "Bearer ldt_new_secret" {
		t.Fatalf("replace-previous Authorization = %q, want the new daemon token", sawReplaceAuth)
	}
	if sawReplacePrevious != "ld_old" {
		t.Fatalf("replace-previous body previous_daemon_id = %q, want ld_old", sawReplacePrevious)
	}
	if sawReplacePreviousToken != "ldt_old_secret" {
		t.Fatalf("replace-previous body previous_daemon_token = %q, want the old daemon token", sawReplacePreviousToken)
	}
	if _, statErr := os.Stat(oldProfile); statErr != nil {
		t.Fatalf("old daemon's enrolled profile should survive re-pair: %v", statErr)
	}
	entries, err := enrollJournalLoad(paths)
	if err != nil {
		t.Fatalf("enroll journal load after re-pair: %v", err)
	}
	if entries["enr_ffffffff-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222"].DaemonID != "ld_old" {
		t.Fatalf("journal entries after re-pair = %#v, want old daemon attribution", entries)
	}
	if _, statErr := os.Stat(enrollJournalPath(paths)); statErr != nil {
		t.Fatalf("enroll journal should survive re-pair: %v", statErr)
	}
	if record.joined() != "" {
		t.Fatalf("call sequence = %q, want no profile cleanup reload during re-pair", record.joined())
	}
	output := out.String()
	if !strings.Contains(output, "Marked previous daemon ld_old") || !strings.Contains(output, "without revoking it") || !strings.Contains(output, "journaled profiles will refresh through the new daemon") {
		t.Fatalf("force-pair output missing preservation warning:\n%s", output)
	}
	// The NEW pairing replaced the old auth.
	auth, ok, err := commentbus.LoadDaemonAuth(paths)
	if err != nil || !ok {
		t.Fatalf("LoadDaemonAuth = ok %v err %v", ok, err)
	}
	if auth.DaemonID != "ld_new" || auth.Token != "ldt_new_secret" {
		t.Fatalf("saved auth = %+v, want the new daemon", auth)
	}
}

func TestBusPairForceDoesNotSendPreviousTokenAcrossBaseURLs(t *testing.T) {
	home := testBusPairHome(t)
	_ = stubBusPairSleep(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldProfile := filepath.Join(agentsDir, "max.oldbot.json")
	if err := os.WriteFile(oldProfile, []byte(`{"handle":"max.oldbot","agent_secret":"as_old_1","base_url":"https://old.example.com"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	entryID := "enr_crossbase-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222"
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: entryID,
		Handle:       "max.oldbot",
		ProfilePath:  oldProfile,
		SecretSHA256: enrollSecretSHA256("as_old_1"),
	}); err != nil {
		t.Fatal(err)
	}

	var server *httptest.Server
	var replacedCount int32
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/pair/start", busPairStartHandler(t, func() string { return server.URL }, 1, 600))
	mux.HandleFunc("/daemon/pair/redeem", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"daemon_token": "ldt_new_secret",
			"daemon_id":    "ld_new",
			"owner_handle": "max",
			"label":        "New Mac",
			"capabilities": []string{"agent_enrollment:v1"},
		})
	})
	mux.HandleFunc("/daemon/replace-previous", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&replacedCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server = httptest.NewServer(mux)
	defer server.Close()
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_old",
		Token:    "ldt_old_secret",
		BaseURL:  "https://old.example.com",
		Label:    "Old Mac",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := busPair(&out, home, "New Mac", server.URL, true); err != nil {
		t.Fatalf("busPair --force err = %v\noutput:\n%s", err, out.String())
	}

	if atomic.LoadInt32(&replacedCount) != 0 {
		t.Fatalf("replace-previous calls = %d, want 0 across base URLs", replacedCount)
	}
	entries, err := enrollJournalLoad(paths)
	if err != nil {
		t.Fatalf("enroll journal load after cross-base re-pair: %v", err)
	}
	if entries[entryID].DaemonID != "ld_old" {
		t.Fatalf("journal entry after cross-base re-pair = %#v, want old daemon attribution", entries[entryID])
	}
	output := out.String()
	if !strings.Contains(output, "Skipped marking previous daemon ld_old") || !strings.Contains(output, "previous daemon token was not sent") {
		t.Fatalf("force-pair output missing cross-base token warning:\n%s", output)
	}

	out.Reset()
	if err := busUnpair(&out, home, true); err != nil {
		t.Fatalf("busUnpair after cross-base re-pair err = %v", err)
	}
	if _, statErr := os.Stat(oldProfile); statErr != nil {
		t.Fatalf("old-base profile should survive unpairing the new-base daemon: %v", statErr)
	}
	remaining, err := enrollJournalLoad(paths)
	if err != nil {
		t.Fatalf("enroll journal load after new-base unpair: %v", err)
	}
	if remaining[entryID].DaemonID != "ld_old" {
		t.Fatalf("old-base journal entry after new-base unpair = %#v, want retained old daemon attribution", remaining)
	}
}

func TestDoctorCheckDaemonPairedFindings(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}

	// Daemon installed but unpaired: a distinct, fixable warn naming the
	// exact command.
	got := checkDaemonPaired(paths, false, true)
	if got.Status != "warn" || !strings.Contains(got.Message, "comment bus pair") || !strings.Contains(got.Message, "not paired") {
		t.Fatalf("unpaired check = %#v", got)
	}

	// Daemon not installed: pairing is reported as skipped, not as the
	// installed-but-unpaired finding.
	got = checkDaemonPaired(paths, false, false)
	if got.Status != "warn" || !strings.Contains(got.Message, "skipped") {
		t.Fatalf("not-installed check = %#v", got)
	}

	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_doc",
		Token:    "ldt_doc_secret",
		Label:    "Doctor Mac",
	}); err != nil {
		t.Fatal(err)
	}
	got = checkDaemonPaired(paths, false, true)
	if got.Status != "ok" || !strings.Contains(got.Message, "Doctor Mac") {
		t.Fatalf("paired check = %#v", got)
	}
	detail, _ := got.Detail.(map[string]any)
	if detail["daemon_id"] != "ld_doc" {
		t.Fatalf("paired check detail = %#v", got.Detail)
	}
}

// Offline `comment bus health` must advertise the pairing capability even
// when the daemon is not running — installers and the browser fallback gate
// read this output to decide whether the new flow exists on this CLI.
func TestOfflineBusHealthAdvertisesPairingFeature(t *testing.T) {
	home := t.TempDir()
	output, err := captureRun(t, []string{"bus", "health", "--home", home})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if jsonErr := json.Unmarshal([]byte(output), &payload); jsonErr != nil {
		t.Fatalf("bus health output not JSON: %v\n%s", jsonErr, output)
	}
	features, ok := payload["features"].(map[string]any)
	if !ok {
		t.Fatalf("bus health missing features map: %s", output)
	}
	if features[commentbus.FeatureDaemonPairing] != float64(commentbus.FeatureDaemonPairingVersion) {
		t.Fatalf("daemon_pairing feature = %#v", features[commentbus.FeatureDaemonPairing])
	}
	if features[commentbus.FeatureAgentEnrollment] != float64(commentbus.FeatureAgentEnrollmentVersion) {
		t.Fatalf("agent_enrollment feature = %#v", features[commentbus.FeatureAgentEnrollment])
	}
	if paired, _ := payload["daemon_paired"].(bool); paired {
		t.Fatalf("fresh home should be unpaired: %s", output)
	}
}
