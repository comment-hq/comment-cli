//go:build darwin || linux

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
)

// ownedAgentsServerState drives the fake /daemon/owned-agents +
// /daemon/agent-enrollments API for one reconciler test: each list call
// records the fingerprint query it saw and answers from `responses` in order
// (the last response repeats); each enroll call records its agent_id and
// answers from `enrollStatus` (default 201).
type ownedAgentsServerState struct {
	responses      []map[string]any
	enrollStatus   []int
	fingerprints   []string
	enrolls        []string
	enrollRuntimes []string
	enrollCh       chan string
}

func newOwnedAgentsTestServer(t *testing.T, state *ownedAgentsServerState) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/owned-agents", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("owned-agents method = %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testEnrollmentDaemonToken {
			t.Errorf("owned-agents Authorization = %q", got)
		}
		state.fingerprints = append(state.fingerprints, r.URL.Query().Get("fingerprint"))
		idx := len(state.fingerprints) - 1
		if idx >= len(state.responses) {
			idx = len(state.responses) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state.responses[idx])
	})
	mux.HandleFunc("/daemon/agent-enrollments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("agent-enrollments method = %s", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+testEnrollmentDaemonToken {
			t.Errorf("enroll Authorization = %q", got)
		}
		var body struct {
			AgentID string `json:"agent_id"`
			Runtime string `json:"runtime"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("enroll body unreadable: %v", err)
		}
		state.enrolls = append(state.enrolls, body.AgentID)
		state.enrollRuntimes = append(state.enrollRuntimes, body.Runtime)
		if state.enrollCh != nil {
			select {
			case state.enrollCh <- body.AgentID:
			default:
			}
		}
		status := http.StatusCreated
		if idx := len(state.enrolls) - 1; idx < len(state.enrollStatus) {
			status = state.enrollStatus[idx]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": status < 400})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func TestOwnedAgentsReconcilerRunsFirstPassImmediately(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	state := &ownedAgentsServerState{
		responses: []map[string]any{ownedAgentsManifestResponse(
			"fp_1",
			ownedAgentsManifestAgentPayload("ag_guy", "max.guy"),
		)},
		enrollCh: make(chan string, 1),
	}
	server := newOwnedAgentsTestServer(t, state)
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer stopOwnedAgentsReconcilerForTest(t, cancel, done)

	go func() {
		defer close(done)
		runOwnedAgentsReconciler(ctx, paths, "")
	}()

	select {
	case got := <-state.enrollCh:
		if got != "ag_guy" {
			t.Fatalf("first enrollment = %q, want ag_guy", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owned-agents reconciler did not run an immediate first pass")
	}
}

func TestOwnedAgentsReconcilerRetriesPromptlyAfterPairingLands(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	state := &ownedAgentsServerState{
		responses: []map[string]any{ownedAgentsManifestResponse(
			"fp_1",
			ownedAgentsManifestAgentPayload("ag_guy", "max.guy"),
		)},
		enrollCh: make(chan string, 1),
	}
	server := newOwnedAgentsTestServer(t, state)
	worker := newOwnedAgentsReconciler(paths)

	// The immediate startup pass sees the normal installer state: daemon running
	// but not paired yet. It must ask the scheduler for the short auth retry
	// cadence from that same observation.
	if pairedAuth := worker.runOnce(context.Background()); pairedAuth {
		t.Fatal("unpaired runOnce returned paired auth")
	}
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)

	if pairedAuth := worker.runOnce(context.Background()); !pairedAuth {
		t.Fatal("paired runOnce did not report paired auth")
	}
	if got := strings.Join(state.enrolls, ","); got != "ag_guy" {
		t.Fatalf("post-pairing enrollment = %q, want ag_guy", got)
	}
}

func TestOwnedAgentsReconcilerLoopUsesAuthRetryUntilUsableAuth(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	state := &ownedAgentsServerState{
		responses: []map[string]any{ownedAgentsManifestResponse(
			"fp_1",
			ownedAgentsManifestAgentPayload("ag_guy", "max.guy"),
		)},
		enrollCh: make(chan string, 1),
	}
	server := newOwnedAgentsTestServer(t, state)
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_worker-test",
		Token:    testEnrollmentDaemonToken,
		Label:    "Worker Test Mac",
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer stopOwnedAgentsReconcilerForTest(t, cancel, done)

	go func() {
		defer close(done)
		runOwnedAgentsReconcilerWithDelays(ctx, paths, "", time.Hour, 20*time.Millisecond)
	}()

	waitForOwnedAgentsLogContains(t, paths, "owned_agents.daemon_auth_missing_base_url")
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)

	select {
	case got := <-state.enrollCh:
		if got != "ag_guy" {
			t.Fatalf("post-auth enrollment = %q, want ag_guy", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owned-agents reconciler did not use auth retry delay after unusable auth")
	}
}

func ownedAgentsManifestResponse(fingerprint string, agents ...map[string]any) map[string]any {
	list := make([]any, 0, len(agents))
	for _, agent := range agents {
		list = append(list, agent)
	}
	return map[string]any{
		"ok":           true,
		"auto_install": true,
		"fingerprint":  fingerprint,
		"agents":       list,
	}
}

func ownedAgentsManifestAgentPayload(agentID string, handle string) map[string]any {
	return map[string]any{
		"agent_id":     agentID,
		"handle":       handle,
		"display_name": "Agent " + handle,
		"runtime":      "claude",
		"kind":         "generic",
	}
}

// writeInstalledAgentProfile writes the `<home>/agents/<handle>.json` file the
// enrollment worker's install step produces, marking the agent installed.
func writeInstalledAgentProfile(t *testing.T, paths commentbus.Paths, handle string) {
	t.Helper()
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := []byte(`{"handle":"` + handle + `","agent_secret":"as_installed","base_url":"https://comment.io"}` + "\n")
	if err := os.WriteFile(filepath.Join(agentsDir, handle+".json"), profile, 0o600); err != nil {
		t.Fatal(err)
	}
}

func stopOwnedAgentsReconcilerForTest(t *testing.T, cancel context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("owned-agents reconciler did not stop")
	}
}

func waitForOwnedAgentsLogContains(t *testing.T, paths commentbus.Paths, needle string) {
	t.Helper()
	logPath := filepath.Join(paths.Logs, "commentd.jsonl")
	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(logPath)
		if err == nil && strings.Contains(string(data), needle) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for log %q in %s; last err=%v body=%q", needle, logPath, err, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// writeInstalledAgentProfileWithRuntime writes an installed generic profile that
// records a concrete runtime (the field the launcher reads back).
func writeInstalledAgentProfileWithRuntime(t *testing.T, paths commentbus.Paths, handle string, runtime string) {
	t.Helper()
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := []byte(`{"handle":"` + handle + `","agent_secret":"as_installed","runtime":"` + runtime + `","base_url":"https://comment.io"}` + "\n")
	if err := os.WriteFile(filepath.Join(agentsDir, handle+".json"), profile, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedAgentsReconcilerSkipsWhenUnpaired(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	worker := newOwnedAgentsReconciler(paths)

	worker.runOnce(context.Background())

	if _, err := os.Stat(filepath.Join(paths.Logs, "commentd.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("log file err = %v, want not exist for quiet unpaired skip", err)
	}
}

func TestOwnedAgentsReconcilerEnrollsOnlyMissingAgents(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	state := &ownedAgentsServerState{
		responses: []map[string]any{ownedAgentsManifestResponse(
			"fp_1",
			ownedAgentsManifestAgentPayload("ag_installed", "max.installed"),
			ownedAgentsManifestAgentPayload("ag_missing", "max.missing"),
		)},
	}
	server := newOwnedAgentsTestServer(t, state)
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	writeInstalledAgentProfile(t, paths, "max.installed")
	worker := newOwnedAgentsReconciler(paths)

	worker.runOnce(context.Background())

	if got := strings.Join(state.enrolls, ","); got != "ag_missing" {
		t.Fatalf("enrolled agent ids = %q, want only ag_missing", got)
	}
	// The self-enroll must carry the manifest's runtime so a generic Codex
	// agent does not get installed with a Claude fallback profile.
	if got := strings.Join(state.enrollRuntimes, ","); got != "claude" {
		t.Fatalf("enrolled runtimes = %q, want the manifest runtime", got)
	}
	if got := strings.Join(state.fingerprints, ","); got != "" {
		t.Fatalf("first pass fingerprint = %q, want empty", got)
	}
	// An enrollment was created this pass but its install has not happened
	// yet (and might never — expiry/cancel), so the fingerprint must NOT be
	// cached: the next pass has to re-diff (3wise round-1 fix).
	if worker.lastFingerprint != "" {
		t.Fatalf("lastFingerprint = %q, want empty while an enrollment is in flight", worker.lastFingerprint)
	}
	logText := readBotletsTeamResyncLog(t, paths)
	if !strings.Contains(logText, "owned_agents.reconcile_complete") {
		t.Fatalf("log did not contain reconcile completion: %s", logText)
	}

	// Once the enrollment worker lands the profile, the follow-up pass is a
	// no-op (enrolled == 0) and the fingerprint is finally cached.
	writeInstalledAgentProfile(t, paths, "max.missing")
	worker.runOnce(context.Background())
	if got := strings.Join(state.enrolls, ","); got != "ag_missing" {
		t.Fatalf("enrolls after install = %q, want no re-enroll once the profile exists", got)
	}
	if worker.lastFingerprint != "fp_1" {
		t.Fatalf("lastFingerprint = %q, want fp_1 cached after the in-sync pass", worker.lastFingerprint)
	}
}

func TestOwnedAgentsReconcilerRefreshesPreviousDaemonProfile(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	state := &ownedAgentsServerState{
		responses: []map[string]any{ownedAgentsManifestResponse(
			"fp_1",
			ownedAgentsManifestAgentPayload("ag_installed", "max.installed"),
		)},
	}
	server := newOwnedAgentsTestServer(t, state)
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(agentsDir, "max.installed.json")
	if err := os.WriteFile(profilePath, []byte(`{"handle":"max.installed","agent_secret":"as_old_daemon","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: "enr_oldoldold-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222",
		DaemonID:     "ld_previous",
		Handle:       "max.installed",
		ProfilePath:  profilePath,
		SecretSHA256: enrollSecretSHA256("as_old_daemon"),
	}); err != nil {
		t.Fatal(err)
	}
	worker := newOwnedAgentsReconciler(paths)

	worker.runOnce(context.Background())

	if got := strings.Join(state.enrolls, ","); got != "ag_installed" {
		t.Fatalf("enrolled agent ids = %q, want stale previous-daemon profile refreshed", got)
	}
	if worker.lastFingerprint != "" {
		t.Fatalf("lastFingerprint = %q, want empty while replacement enrollment is in flight", worker.lastFingerprint)
	}

	if err := os.WriteFile(profilePath, []byte(`{"handle":"max.installed","agent_secret":"as_new_daemon","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: "enr_newnewnew-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222",
		DaemonID:     "ld_worker-test",
		Handle:       "max.installed",
		ProfilePath:  profilePath,
		SecretSHA256: enrollSecretSHA256("as_new_daemon"),
	}); err != nil {
		t.Fatal(err)
	}
	worker.runOnce(context.Background())
	if got := strings.Join(state.enrolls, ","); got != "ag_installed" {
		t.Fatalf("enrolls after replacement = %q, want no re-enroll once the current daemon profile lands", got)
	}
	if worker.lastFingerprint != "fp_1" {
		t.Fatalf("lastFingerprint = %q, want fp_1 cached after current daemon install", worker.lastFingerprint)
	}
}

func TestOwnedAgentsReconcilerSkipsUnchangedFingerprint(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	state := &ownedAgentsServerState{
		responses: []map[string]any{
			ownedAgentsManifestResponse("fp_1"),
			{"ok": true, "auto_install": true, "fingerprint": "fp_1", "unchanged": true},
		},
	}
	server := newOwnedAgentsTestServer(t, state)
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newOwnedAgentsReconciler(paths)

	worker.runOnce(context.Background())
	worker.runOnce(context.Background())

	if got := strings.Join(state.fingerprints, ","); got != ",fp_1" {
		t.Fatalf("fingerprint queries = %q, want \",fp_1\"", got)
	}
	if len(state.enrolls) != 0 {
		t.Fatalf("enrolls = %v, want none for an unchanged manifest", state.enrolls)
	}
	if worker.lastFingerprint != "fp_1" {
		t.Fatalf("lastFingerprint = %q, want kept fp_1", worker.lastFingerprint)
	}
}

func TestOwnedAgentsReconcilerClearsFingerprintOnEnrollFailure(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	state := &ownedAgentsServerState{
		responses: []map[string]any{ownedAgentsManifestResponse(
			"fp_1",
			ownedAgentsManifestAgentPayload("ag_missing", "max.missing"),
		)},
		enrollStatus: []int{http.StatusInternalServerError, http.StatusOK},
	}
	server := newOwnedAgentsTestServer(t, state)
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newOwnedAgentsReconciler(paths)

	worker.runOnce(context.Background())
	if worker.lastFingerprint != "" {
		t.Fatalf("lastFingerprint = %q, want cleared after enroll failure", worker.lastFingerprint)
	}
	logText := readBotletsTeamResyncLog(t, paths)
	if !strings.Contains(logText, "owned_agents.enroll_failed") || !strings.Contains(logText, "owned_agents.reconcile_failed") {
		t.Fatalf("log missing enroll/reconcile failure: %s", logText)
	}

	// The cleared fingerprint forces a full re-fetch; the retry (idempotent
	// 200 answer) completes the pass and stores the fingerprint.
	worker.runOnce(context.Background())
	if got := strings.Join(state.fingerprints, ","); got != "," {
		t.Fatalf("fingerprint queries = %q, want two empty queries", got)
	}
	if got := strings.Join(state.enrolls, ","); got != "ag_missing,ag_missing" {
		t.Fatalf("enrolls = %q, want a retry for the failed agent", got)
	}
	// The retry created an enrollment, so the fingerprint is still withheld
	// until a pass where everything already has a local profile.
	if worker.lastFingerprint != "" {
		t.Fatalf("lastFingerprint = %q, want empty while the retried enrollment is in flight", worker.lastFingerprint)
	}
	writeInstalledAgentProfile(t, paths, "max.missing")
	worker.runOnce(context.Background())
	if worker.lastFingerprint != "fp_1" {
		t.Fatalf("lastFingerprint = %q, want fp_1 once the install landed", worker.lastFingerprint)
	}
}

func TestOwnedAgentsReconcilerUnchangedRepairsMissingProfile(t *testing.T) {
	// The server's `unchanged` answer only proves the SERVER manifest did not
	// move. If a local profile is deleted (or restored from an older backup)
	// after the fingerprint was cached, the reconciler must drop the
	// fingerprint so the next pass re-fetches the full manifest and re-enrolls
	// — otherwise the gap stays invisible until an unrelated manifest change.
	paths := testAgentEnrollmentPaths(t)
	state := &ownedAgentsServerState{
		responses: []map[string]any{
			ownedAgentsManifestResponse("fp_1", ownedAgentsManifestAgentPayload("ag_a", "max.agentaa")),
			{"ok": true, "auto_install": true, "fingerprint": "fp_1", "unchanged": true},
			ownedAgentsManifestResponse("fp_1", ownedAgentsManifestAgentPayload("ag_a", "max.agentaa")),
		},
	}
	server := newOwnedAgentsTestServer(t, state)
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	writeInstalledAgentProfile(t, paths, "max.agentaa")
	worker := newOwnedAgentsReconciler(paths)

	// Pass 1: everything installed; fingerprint + handles cached.
	worker.runOnce(context.Background())
	if worker.lastFingerprint != "fp_1" {
		t.Fatalf("lastFingerprint = %q, want fp_1 cached", worker.lastFingerprint)
	}

	// The local install disappears; the server keeps answering `unchanged`.
	if err := os.Remove(filepath.Join(paths.Home, "agents", "max.agentaa.json")); err != nil {
		t.Fatal(err)
	}

	// Pass 2: unchanged answer, but the local verify finds the gap and drops
	// the fingerprint (no enroll yet — the full manifest is gone from this
	// response).
	worker.runOnce(context.Background())
	if worker.lastFingerprint != "" {
		t.Fatalf("lastFingerprint = %q, want cleared after local profile went missing", worker.lastFingerprint)
	}
	if len(state.enrolls) != 0 {
		t.Fatalf("enrolls = %v, want none on the unchanged pass", state.enrolls)
	}
	logText := readBotletsTeamResyncLog(t, paths)
	if !strings.Contains(logText, "owned_agents.local_profile_missing") {
		t.Fatalf("log missing local_profile_missing record: %s", logText)
	}

	// Pass 3: the cleared fingerprint forces a full re-fetch and the missing
	// agent is re-enrolled.
	worker.runOnce(context.Background())
	if got := strings.Join(state.enrolls, ","); got != "ag_a" {
		t.Fatalf("enrolls = %q, want re-enroll of the missing agent", got)
	}
	if got := strings.Join(state.fingerprints, ","); got != ",fp_1," {
		t.Fatalf("fingerprint queries = %q, want \",fp_1,\" (cleared on pass 3)", got)
	}
}

func TestOwnedAgentsReconcilerAutoInstallOffDoesNothing(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	state := &ownedAgentsServerState{
		responses: []map[string]any{{
			"ok":           true,
			"auto_install": false,
			"fingerprint":  "fp_off",
			"agents":       []any{ownedAgentsManifestAgentPayload("ag_missing", "max.missing")},
		}},
	}
	server := newOwnedAgentsTestServer(t, state)
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newOwnedAgentsReconciler(paths)

	worker.runOnce(context.Background())
	worker.runOnce(context.Background())

	if len(state.enrolls) != 0 {
		t.Fatalf("enrolls = %v, want none while auto-install is off", state.enrolls)
	}
	if got := strings.Join(state.fingerprints, ","); got != "," {
		t.Fatalf("fingerprint queries = %q, want nothing stored across passes", got)
	}
	if worker.lastFingerprint != "" {
		t.Fatalf("lastFingerprint = %q, want nothing stored while auto-install is off", worker.lastFingerprint)
	}
}

func ownedAgentsManifestBotletsPayload(agentID string, handle string) map[string]any {
	return map[string]any{
		"agent_id":     agentID,
		"handle":       handle,
		"display_name": "Bot " + handle,
		"runtime":      "claude",
		"kind":         "botlets",
	}
}

// writeOwnedAgentsBotletsRegistry writes a raw registry.json carrying exactly
// the given handles, matching what botletsRegistryHandles reads.
func writeOwnedAgentsBotletsRegistry(t *testing.T, botletsHome string, handles ...string) {
	t.Helper()
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	bots := make([]map[string]any, 0, len(handles))
	for _, handle := range handles {
		bots = append(bots, map[string]any{"name": handle, "handle": handle})
	}
	data, err := json.Marshal(map[string]any{"bots": bots})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedAgentsReconcilerBotletsKindRequiresRegistryEntry(t *testing.T) {
	// A Botlets bot's local install is profile + registry entry. A registry
	// that lost the bot (deleted/corrupted registry.json, restore from an
	// older backup) leaves the bot unwired even though the profile file
	// survives — the reconciler must treat that as NOT installed and
	// self-enroll so the enrollment worker rewrites both (Codex round-5).
	paths := testAgentEnrollmentPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	state := &ownedAgentsServerState{
		responses: []map[string]any{
			ownedAgentsManifestResponse("fp_1", ownedAgentsManifestBotletsPayload("ag_bot", "max.bot")),
			ownedAgentsManifestResponse("fp_1", ownedAgentsManifestBotletsPayload("ag_bot", "max.bot")),
			{"ok": true, "auto_install": true, "fingerprint": "fp_1", "unchanged": true},
		},
	}
	server := newOwnedAgentsTestServer(t, state)
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	writeInstalledAgentProfile(t, paths, "max.bot")
	worker := newOwnedAgentsReconciler(paths)
	worker.botletsHomeHint = botletsHome

	// Pass 1: profile present but no registry entry -> the bot is unwired and
	// must be re-enrolled; the fingerprint stays withheld.
	worker.runOnce(context.Background())
	if got := strings.Join(state.enrolls, ","); got != "ag_bot" {
		t.Fatalf("enrolls = %q, want the registry-less Botlets bot re-enrolled", got)
	}
	if worker.lastFingerprint != "" {
		t.Fatalf("lastFingerprint = %q, want withheld while the enrollment is in flight", worker.lastFingerprint)
	}

	// The enrollment worker wires the registry; pass 2 is in sync and caches.
	writeOwnedAgentsBotletsRegistry(t, botletsHome, "max.bot")
	worker.runOnce(context.Background())
	if got := strings.Join(state.enrolls, ","); got != "ag_bot" {
		t.Fatalf("enrolls = %q, want no re-enroll once profile AND registry entry exist", got)
	}
	if worker.lastFingerprint != "fp_1" {
		t.Fatalf("lastFingerprint = %q, want fp_1 cached once the install is whole", worker.lastFingerprint)
	}

	// The registry loses the bot while the server keeps answering `unchanged`:
	// the fast path must drop the fingerprint so the next pass repairs.
	writeOwnedAgentsBotletsRegistry(t, botletsHome)
	worker.runOnce(context.Background())
	if worker.lastFingerprint != "" {
		t.Fatalf("lastFingerprint = %q, want cleared after the registry entry went missing", worker.lastFingerprint)
	}
	logText := readBotletsTeamResyncLog(t, paths)
	if !strings.Contains(logText, "owned_agents.local_profile_missing") {
		t.Fatalf("log missing local install gap record: %s", logText)
	}
}

// writeOwnedAgentsBotletsRegistryDesired writes a registry.json whose single bot
// carries the managed-session runtime/timezone, the "Responds to @mentions"
// opt-in, and brain setup generation, so a test can assert the reconciler
// re-enrolls when the manifest's DESIRED state diverges from what is installed
// (Codex round-9).
func writeOwnedAgentsBotletsRegistryDesired(t *testing.T, botletsHome, handle, runtime, model, timezone string, respondsToMentions bool, setupGeneration int) {
	t.Helper()
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	bot := map[string]any{
		"name":   handle,
		"handle": handle,
		"managed_session": map[string]any{
			"enabled":  true,
			"runtime":  runtime,
			"model":    model,
			"timezone": timezone,
		},
		"responds_to_mentions": respondsToMentions,
		"brain_ref": map[string]any{
			"workspace_id":     "ws_1",
			"owner_agent_id":   "ag_owner",
			"bot_agent_id":     "ag_bot_agent",
			"container_id":     "c_1",
			"root_folder_id":   "rf_1",
			"setup_generation": setupGeneration,
		},
	}
	data, err := json.Marshal(map[string]any{"bots": []any{bot}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func ownedAgentsManifestBotletsDesiredPayload(agentID, handle, runtime, model, timezone string, respondsToMentions bool, setupGeneration int) map[string]any {
	return map[string]any{
		"agent_id":             agentID,
		"handle":               handle,
		"display_name":         "Bot " + handle,
		"runtime":              runtime,
		"model":                model,
		"schedule_timezone":    timezone,
		"responds_to_mentions": respondsToMentions,
		// Brain block mirrors the cf manifest: setup_generation is NESTED here,
		// alongside the brain ref ids the install records into the registry.
		"brain": map[string]any{
			"workspace_id":     "ws_1",
			"bot_id":           "bot_1",
			"owner_agent_id":   "ag_owner",
			"bot_agent_id":     "ag_bot_agent",
			"container_id":     "c_1",
			"root_folder_id":   "rf_1",
			"setup_generation": setupGeneration,
		},
		"kind": "botlets",
	}
}

// TestOwnedAgentsReconcilerReinstallsOnDesiredStateChange covers the Codex
// round-9 finding: when the manifest changes only a DESIRED field (runtime,
// schedule timezone, or brain setup generation) for an already-installed Botlets
// bot, the profile + registry handle still exist, so a presence-only check would
// cache the new fingerprint with a stale install. The reconciler must compare
// the manifest's desired fields against the locally-recorded ones and re-enroll
// on any mismatch.
func TestOwnedAgentsReconcilerKeepsCodexProfileWhenManifestRuntimeNull(t *testing.T) {
	// A generic agent with no prior enrollment runtime makes cf fall back to a
	// null latestEnrollmentRuntime, so the manifest carries runtime: null. The
	// reconciler must NOT coerce that empty runtime to claude and re-enroll a
	// profile that is already installed/running as codex — a null manifest
	// runtime asserts no runtime, so the locally-installed runtime is left alone
	// (otherwise the self-enroll rewrites the profile to the Claude fallback).
	paths := testAgentEnrollmentPaths(t)
	payload := map[string]any{
		"agent_id":     "ag_generic",
		"handle":       "max.codexbot",
		"display_name": "Codex Bot",
		"runtime":      nil,
		"kind":         "generic",
	}
	state := &ownedAgentsServerState{
		responses: []map[string]any{ownedAgentsManifestResponse("fp_generic", payload)},
	}
	server := newOwnedAgentsTestServer(t, state)
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	writeInstalledAgentProfileWithRuntime(t, paths, "max.codexbot", "codex")

	worker := newOwnedAgentsReconciler(paths)
	worker.runOnce(context.Background())

	if len(state.enrolls) != 0 {
		t.Fatalf("re-enrolled %q, want no re-enroll for a null manifest runtime", strings.Join(state.enrolls, ","))
	}
	if worker.lastFingerprint != "fp_generic" {
		t.Fatalf("lastFingerprint = %q, want fp_generic cached when the codex install is left alone", worker.lastFingerprint)
	}
	rt, _, ok := agentProfileRuntimeAndModel(agentProfileFilePath(paths, "max.codexbot"))
	if !ok || rt != "codex" {
		t.Fatalf("installed runtime = %q ok=%v, want codex left in place", rt, ok)
	}
}

func TestOwnedAgentsReconcilerReinstallsOnDesiredStateChange(t *testing.T) {
	cases := []struct {
		name               string
		runtime            string
		model              string
		timezone           string
		respondsToMentions bool
		setupGen           int
		brainContainer     string
		ownerAgentID       string
		botAgentID         string
		botID              string
		wantReinstall      bool
	}{
		{name: "in-sync", runtime: "claude", model: "sonnet-local", timezone: "America/New_York", respondsToMentions: true, setupGen: 3, wantReinstall: false},
		{name: "runtime-change", runtime: "codex", model: "sonnet-local", timezone: "America/New_York", respondsToMentions: true, setupGen: 3, wantReinstall: true},
		{name: "model-change", runtime: "claude", model: "opus-remote", timezone: "America/New_York", respondsToMentions: true, setupGen: 3, wantReinstall: true},
		{name: "timezone-change", runtime: "claude", model: "sonnet-local", timezone: "Europe/Berlin", respondsToMentions: true, setupGen: 3, wantReinstall: true},
		// Toggling "Responds to @mentions" must re-enroll so the local registry's
		// flag converges and the daemon's mention auto-launch gate reads it.
		{name: "responds-to-mentions-on", runtime: "claude", model: "sonnet-local", timezone: "America/New_York", respondsToMentions: false, setupGen: 3, wantReinstall: true},
		{name: "setup-generation-change", runtime: "claude", model: "sonnet-local", timezone: "America/New_York", respondsToMentions: true, setupGen: 4, wantReinstall: true},
		{name: "brain-ref-change", runtime: "claude", model: "sonnet-local", timezone: "America/New_York", respondsToMentions: true, setupGen: 3, brainContainer: "c_2", wantReinstall: true},
		{name: "owner-agent-id-change", runtime: "claude", model: "sonnet-local", timezone: "America/New_York", respondsToMentions: true, setupGen: 3, ownerAgentID: "ag_owner_2", wantReinstall: true},
		{name: "bot-agent-id-change", runtime: "claude", model: "sonnet-local", timezone: "America/New_York", respondsToMentions: true, setupGen: 3, botAgentID: "ag_bot_agent_2", wantReinstall: true},
		// bot_id is the immutable durable bot identity, not stored in brain_ref:
		// a changed manifest bot_id alone must NOT churn a re-enroll for a stable
		// handle (a different bot is a different handle entirely).
		{name: "bot-id-change-only", runtime: "claude", model: "sonnet-local", timezone: "America/New_York", respondsToMentions: true, setupGen: 3, botID: "bot_2", wantReinstall: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := testAgentEnrollmentPaths(t)
			botletsHome := filepath.Join(t.TempDir(), "botlets")
			payload := ownedAgentsManifestBotletsDesiredPayload(
				"ag_bot", "max.bot", tc.runtime, tc.model, tc.timezone, tc.respondsToMentions, tc.setupGen)
			if tc.brainContainer != "" {
				payload["brain"].(map[string]any)["container_id"] = tc.brainContainer
			}
			if tc.ownerAgentID != "" {
				payload["brain"].(map[string]any)["owner_agent_id"] = tc.ownerAgentID
			}
			if tc.botAgentID != "" {
				payload["brain"].(map[string]any)["bot_agent_id"] = tc.botAgentID
			}
			if tc.botID != "" {
				payload["brain"].(map[string]any)["bot_id"] = tc.botID
			}
			state := &ownedAgentsServerState{
				responses: []map[string]any{ownedAgentsManifestResponse("fp_new", payload)},
			}
			server := newOwnedAgentsTestServer(t, state)
			writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
			writeInstalledAgentProfile(t, paths, "max.bot")
			// Installed state: runtime claude, NY timezone, responds-to-mentions on,
			// model sonnet-local, and setup generation 3.
			writeOwnedAgentsBotletsRegistryDesired(t, botletsHome, "max.bot", "claude", "sonnet-local", "America/New_York", true, 3)
			worker := newOwnedAgentsReconciler(paths)
			worker.botletsHomeHint = botletsHome

			worker.runOnce(context.Background())

			reinstalled := strings.Join(state.enrolls, ",") == "ag_bot"
			if reinstalled != tc.wantReinstall {
				t.Fatalf("re-enrolled = %v, want %v (enrolls = %q)", reinstalled, tc.wantReinstall, strings.Join(state.enrolls, ","))
			}
			if tc.wantReinstall && worker.lastFingerprint != "" {
				t.Fatalf("lastFingerprint = %q, want withheld while the desired-state re-enroll is in flight", worker.lastFingerprint)
			}
			if !tc.wantReinstall && worker.lastFingerprint != "fp_new" {
				t.Fatalf("lastFingerprint = %q, want fp_new cached when desired state already matches", worker.lastFingerprint)
			}
		})
	}
}
