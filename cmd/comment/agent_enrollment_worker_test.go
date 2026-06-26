//go:build darwin || linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// testEnrollmentBotletsHint is the `botlets` hint block a Botlets-bot redeem
// response carries. Brain ids are distinct sentinels so the mapping
// hint -> botletsRegisterInput can be asserted field by field.
func testEnrollmentBotletsHint() map[string]any {
	return map[string]any{
		"runtime":              "claude",
		"schedule_timezone":    "America/New_York",
		"responds_to_mentions": true,
		"brain": map[string]any{
			"workspace_id":     "bw_team",
			"bot_id":           "bot_xyz",
			"bot_agent_id":     "ag_botxyz",
			"container_id":     "cont_1",
			"root_folder_id":   "fold_1",
			"owner_agent_id":   "ag_owner",
			"setup_generation": 7,
		},
	}
}

// stubBotletsTeamRegister replaces the registerBotletsBotLocally seam shared by
// the team-resync worker (botletsTeamRegisterLocally) with a recorder so the
// enrollment worker's Botlets path can be exercised hermetically. It captures
// every botletsRegisterInput and returns whatever fn supplies.
func stubBotletsTeamRegister(t *testing.T, fn func(in botletsRegisterInput) (botletsRegisterResult, error)) *[]botletsRegisterInput {
	t.Helper()
	old := botletsTeamRegisterLocally
	captured := &[]botletsRegisterInput{}
	botletsTeamRegisterLocally = func(_ context.Context, in botletsRegisterInput) (botletsRegisterResult, error) {
		*captured = append(*captured, in)
		return fn(in)
	}
	t.Cleanup(func() { botletsTeamRegisterLocally = old })
	return captured
}

const (
	testEnrollmentDaemonToken = "ldt_ag_owner_ld_test_worker-secret-token"
	testEnrollmentID          = "enr_11111111-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222"
	testEnrollmentCredID      = "alc_33333333-3333-4333-8333-333333333333"
	testEnrollmentSecret      = "as_ag_reviewer_44444444-4444-4444-8444-444444444444"
)

func testAgentEnrollmentPaths(t *testing.T) commentbus.Paths {
	t.Helper()
	paths, err := commentbus.ResolvePaths(filepath.Join(t.TempDir(), ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func writeEnrollmentDaemonAuth(t *testing.T, paths commentbus.Paths, baseURL string, token string) {
	t.Helper()
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_worker-test",
		Token:    token,
		BaseURL:  baseURL,
		Label:    "Worker Test Mac",
	}); err != nil {
		t.Fatal(err)
	}
}

// stubAgentEnrollmentReload replaces the daemon reload with a recorder. Each
// call appends the handle it was asked about; fn (optional) supplies the
// returned error text.
func stubAgentEnrollmentReload(t *testing.T, record *callRecorder, fn func(handle string) string) {
	t.Helper()
	old := agentEnrollmentReloadProfiles
	agentEnrollmentReloadProfiles = func(_ context.Context, _ commentbus.Paths, handle string) string {
		record.add("reload:" + handle)
		if fn != nil {
			return fn(handle)
		}
		return ""
	}
	t.Cleanup(func() { agentEnrollmentReloadProfiles = old })
}

type callRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *callRecorder) add(call string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *callRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.calls...)
}

func (r *callRecorder) joined() string {
	return strings.Join(r.snapshot(), " ")
}

func enrollmentListPayload(state string, credentialID string) map[string]any {
	item := map[string]any{
		"enrollment_id": testEnrollmentID,
		"state":         state,
		"agent_id":      "ag_reviewer",
		"handle":        "max.reviewer",
		"display_name":  "Reviewer",
		"runtime":       "codex",
		"expires_at":    time.Now().Add(10 * time.Minute).UTC().Format(time.RFC3339),
	}
	if credentialID != "" {
		item["credential_id"] = credentialID
	}
	return map[string]any{"enrollments": []any{item}}
}

// enrollmentCleanupListPayload is a poll body whose only work is one
// redeemed-then-terminal `cleanups` item (state cancelled/failed/expired) —
// the shape GET /daemon/agent-enrollments serves a daemon that was stopped
// when the terminal transition landed.
func enrollmentCleanupListPayload(state string) map[string]any {
	return map[string]any{
		"enrollments": []any{},
		"cleanups": []any{map[string]any{
			"enrollment_id": testEnrollmentID,
			"state":         state,
			"agent_id":      "ag_reviewer",
			"handle":        "max.reviewer",
			"display_name":  "Reviewer",
			"runtime":       "codex",
			"expires_at":    time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
			"credential_id": testEnrollmentCredID,
		}},
	}
}

// newEnrollmentTestServer builds the fake Comment.io API for one enrollment.
// Handlers may be nil to use the happy-path default; every request asserts
// its bearer token and records itself in order.
type enrollmentServerConfig struct {
	listState string // initial item state ("pending" / "redeemed")
	// list, when non-nil, replaces the default single-`enrollments`-item list
	// body (the ETag header and auth assertion still run first) —
	// cleanup-reconciliation tests serve a `cleanups` item, then the drained
	// empty list.
	list      http.HandlerFunc
	agentsMe  http.HandlerFunc
	ack       http.HandlerFunc
	ackBodies *[]map[string]any
	// redeemBotlets, when non-nil, is added as the `botlets` hint block on the
	// redeem response (the server emits it only for Botlets bots).
	redeemBotlets map[string]any
	serverURLRef  *string
}

func newEnrollmentTestServer(t *testing.T, record *callRecorder, cfg enrollmentServerConfig) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/daemon/agent-enrollments", func(w http.ResponseWriter, r *http.Request) {
		record.add("list")
		if got := r.Header.Get("Authorization"); got != "Bearer "+testEnrollmentDaemonToken {
			t.Errorf("list Authorization = %q", got)
		}
		w.Header().Set("ETag", `"etag-1"`)
		w.Header().Set("Content-Type", "application/json")
		if cfg.list != nil {
			cfg.list(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(enrollmentListPayload(cfg.listState, ""))
	})
	mux.HandleFunc("/daemon/agent-enrollments/"+testEnrollmentID+"/redeem", func(w http.ResponseWriter, r *http.Request) {
		record.add("redeem")
		if got := r.Header.Get("Authorization"); got != "Bearer "+testEnrollmentDaemonToken {
			t.Errorf("redeem Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		redeemBody := map[string]any{
			"enrollment_id": testEnrollmentID,
			"agent": map[string]any{
				"agent_id":     "ag_reviewer",
				"handle":       "max.reviewer",
				"display_name": "Reviewer",
			},
			"local_credential": map[string]any{
				"credential_id": testEnrollmentCredID,
				"agent_secret":  testEnrollmentSecret,
				"base_url":      server.URL,
				"runtime":       "codex",
			},
		}
		if cfg.redeemBotlets != nil {
			redeemBody["botlets"] = cfg.redeemBotlets
		}
		_ = json.NewEncoder(w).Encode(redeemBody)
	})
	mux.HandleFunc("/agents/me", func(w http.ResponseWriter, r *http.Request) {
		record.add("verify")
		if got := r.Header.Get("Authorization"); got != "Bearer "+testEnrollmentSecret {
			t.Errorf("verify Authorization = %q", got)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("verify must be a PLAIN /agents/me, got query %q", r.URL.RawQuery)
		}
		if cfg.agentsMe != nil {
			cfg.agentsMe(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"agent_id": "ag_reviewer", "handle": "max.reviewer"})
	})
	mux.HandleFunc("/daemon/agent-enrollments/"+testEnrollmentID+"/ack", func(w http.ResponseWriter, r *http.Request) {
		record.add("ack")
		if got := r.Header.Get("Authorization"); got != "Bearer "+testEnrollmentDaemonToken {
			t.Errorf("ack Authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("ack body unreadable: %v", err)
		}
		if cfg.ackBodies != nil {
			*cfg.ackBodies = append(*cfg.ackBodies, body)
		}
		if cfg.ack != nil {
			cfg.ack(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	if cfg.serverURLRef != nil {
		*cfg.serverURLRef = server.URL
	}
	return server
}

func enrollmentProfilePath(paths commentbus.Paths) string {
	return filepath.Join(paths.Home, "agents", "max.reviewer.json")
}

// seedEnrollJournalForTest records the attribution evidence processCleanup
// requires before it may touch a profile: THIS enrollment wrote this file
// with this secret (the entry the install paths journal just before writing).
func seedEnrollJournalForTest(t *testing.T, paths commentbus.Paths, secret string, botletsHome string) {
	t.Helper()
	entry := enrollJournalEntry{
		EnrollmentID: testEnrollmentID,
		Handle:       "max.reviewer",
		ProfilePath:  enrollmentProfilePath(paths),
		SecretSHA256: enrollSecretSHA256(secret),
	}
	if botletsHome != "" {
		entry.BotletsHandle = "max.reviewer"
		entry.BotletsHome = botletsHome
	}
	if err := enrollJournalRecord(paths, entry); err != nil {
		t.Fatal(err)
	}
}

// TestEnrollJournalPreservesPriorSecretOnRetry covers the Codex round-9 finding:
// on a retry a re-redeemed enrollment journals a NEW secret hash before the
// replacement profile write lands. If that write never lands, disk still holds
// the PRIOR credential — cleanup must still attribute it (preserve-old-until-
// replacement-lands) rather than abandon the revoked profile as unowned.
func TestEnrollJournalPreservesPriorSecretOnRetry(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	profilePath := filepath.Join(paths.Home, "agents", "max.reviewer.json")
	if err := os.MkdirAll(filepath.Dir(profilePath), 0o700); err != nil {
		t.Fatal(err)
	}
	// First pass: wrote credential A and journaled hash A.
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: "enr_1", Handle: "max.reviewer", ProfilePath: profilePath,
		SecretSHA256: enrollSecretSHA256("secret_A"),
	}); err != nil {
		t.Fatal(err)
	}
	// Disk still holds credential A (the replacement write for B has not landed).
	if err := os.WriteFile(profilePath, []byte(`{"agent_secret":"secret_A"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Retry: re-redeem swapped to credential B; the pre-write upsert records B.
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: "enr_1", Handle: "max.reviewer", ProfilePath: profilePath,
		SecretSHA256: enrollSecretSHA256("secret_B"),
	}); err != nil {
		t.Fatal(err)
	}
	entry, ok, _ := enrollJournalLookup(paths, "enr_1")
	if !ok {
		t.Fatal("journal entry missing after retry upsert")
	}
	if entry.SecretSHA256 != enrollSecretSHA256("secret_B") {
		t.Fatalf("current hash = %q, want B", entry.SecretSHA256)
	}
	if len(entry.PrevSecretSHA256) != 1 || entry.PrevSecretSHA256[0] != enrollSecretSHA256("secret_A") {
		t.Fatalf("prev hashes = %v, want [A]", entry.PrevSecretSHA256)
	}
	// The still-on-disk prior credential A must be attributed to this enrollment.
	owns, indeterminate := enrollJournalEntryOwnsProfile(entry)
	if indeterminate || !owns {
		t.Fatalf("owns=%v indeterminate=%v, want owns of the on-disk prior credential", owns, indeterminate)
	}
	// A DIFFERENT install's credential must still be left alone.
	if err := os.WriteFile(profilePath, []byte(`{"agent_secret":"secret_other"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	owns, _ = enrollJournalEntryOwnsProfile(entry)
	if owns {
		t.Fatal("must not own a profile holding a foreign credential")
	}
}

// TestExistingProfileIsEnrollmentOwned covers the Codex round-15 finding: a
// retried install must not snapshot its OWN earlier (about-to-be-revoked)
// credential as a .enroll-backup. The backup-skip gate treats a profile on disk
// as enrollment-owned only with positive journal attribution; an unjournaled or
// foreign-credential file is left to be backed up (the safe default).
func TestExistingProfileIsEnrollmentOwned(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	profilePath := enrollmentProfilePath(paths)
	if err := os.MkdirAll(filepath.Dir(profilePath), 0o700); err != nil {
		t.Fatal(err)
	}
	worker := newAgentEnrollmentWorker(paths)
	writeProfile := func(secret string) {
		if err := os.WriteFile(profilePath, []byte(`{"handle":"max.reviewer","agent_secret":"`+secret+`","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// No journal entry: ownership cannot be proven -> not owned (back it up).
	writeProfile("as_enrolled")
	if worker.existingProfileIsEnrollmentOwned(testEnrollmentID, profilePath) {
		t.Fatal("an unjournaled profile must not be treated as enrollment-owned")
	}

	// Journaled with the matching secret: this enrollment's own prior write.
	seedEnrollJournalForTest(t, paths, "as_enrolled", "")
	if !worker.existingProfileIsEnrollmentOwned(testEnrollmentID, profilePath) {
		t.Fatal("a journaled profile holding this enrollment's secret must be owned")
	}

	// A foreign (pre-existing user install) credential now sits on disk: the
	// journal hash no longer matches -> not owned, so the real install is
	// preserved by a backup rather than removed on cleanup.
	writeProfile("as_preexisting_user_install")
	if worker.existingProfileIsEnrollmentOwned(testEnrollmentID, profilePath) {
		t.Fatal("a foreign credential on disk must not be attributed to this enrollment")
	}

	// A different enrollment id never matches this entry.
	writeProfile("as_enrolled")
	if worker.existingProfileIsEnrollmentOwned("enr_someone_else", profilePath) {
		t.Fatal("a different enrollment id must not claim this profile")
	}
}

// TestEnrollJournalIndeterminateOnReadError covers the Codex round-9 finding: a
// journaled profile that exists but cannot be read (here a path that is a
// directory, an EISDIR that is not ErrNotExist) must surface indeterminate, not
// a definitive non-match that would prune/confirm-drain a possibly-ours profile.
func TestEnrollJournalIndeterminateOnReadError(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	dirAsProfile := filepath.Join(paths.Home, "agents", "max.reviewer.json")
	if err := os.MkdirAll(dirAsProfile, 0o700); err != nil {
		t.Fatal(err)
	}
	entry := enrollJournalEntry{
		EnrollmentID: "enr_2", Handle: "max.reviewer", ProfilePath: dirAsProfile,
		SecretSHA256: enrollSecretSHA256("secret_A"),
	}
	matches, fileExists, indeterminate := enrollJournalProfileSecretMatches(entry)
	if matches || !fileExists || !indeterminate {
		t.Fatalf("matches=%v fileExists=%v indeterminate=%v, want false/true/true", matches, fileExists, indeterminate)
	}
	owns, ind := enrollJournalEntryOwnsProfile(entry)
	if owns || !ind {
		t.Fatalf("owns=%v indeterminate=%v, want not-owns + indeterminate", owns, ind)
	}
}

func TestAgentEnrollmentWorkerNoopsWhenUnpaired(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	worker := newAgentEnrollmentWorker(paths)

	wait := worker.runOnce(context.Background())
	if wait != agentEnrollmentPairingRecheckInterval {
		t.Fatalf("unpaired wait = %v, want pairing recheck %v", wait, agentEnrollmentPairingRecheckInterval)
	}
	if len(record.snapshot()) != 0 {
		t.Fatalf("unpaired pass did work: %v", record.snapshot())
	}

	// Pairing mid-run activates the worker without a restart: the same worker
	// picks up the new daemon-auth.json on its next pass.
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{listState: "pending", ackBodies: &ackBodies})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	wait = worker.runOnce(context.Background())
	if wait != agentEnrollmentPollInterval {
		t.Fatalf("paired wait = %v, want fast poll %v", wait, agentEnrollmentPollInterval)
	}
	if got := record.joined(); got != "list redeem reload:max.reviewer verify ack" {
		t.Fatalf("call sequence after pairing = %q", got)
	}
}

func TestAgentEnrollmentWorkerInstallsPendingEnrollment(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{listState: "pending", ackBodies: &ackBodies})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	wait := worker.runOnce(context.Background())
	if wait != agentEnrollmentPollInterval {
		t.Fatalf("wait = %v, want %v", wait, agentEnrollmentPollInterval)
	}
	if got := record.joined(); got != "list redeem reload:max.reviewer verify ack" {
		t.Fatalf("call sequence = %q", got)
	}
	if len(ackBodies) != 1 {
		t.Fatalf("ack bodies = %v", ackBodies)
	}
	if ackBodies[0]["state"] != "installed" || ackBodies[0]["credential_id"] != testEnrollmentCredID {
		t.Fatalf("installed ack body = %#v", ackBodies[0])
	}

	info, err := os.Stat(enrollmentProfilePath(paths))
	if err != nil {
		t.Fatalf("profile file missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("profile mode = %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(enrollmentProfilePath(paths))
	if err != nil {
		t.Fatal(err)
	}
	var profile map[string]string
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatalf("profile not JSON: %v\n%s", err, data)
	}
	if profile["handle"] != "max.reviewer" || profile["agent_secret"] != testEnrollmentSecret {
		t.Fatalf("profile contents = %#v", profile)
	}
	if profile["base_url"] != server.URL || profile["runtime"] != "codex" {
		t.Fatalf("profile base/runtime = %#v", profile)
	}

	logText := readBotletsTeamResyncLog(t, paths)
	if !strings.Contains(logText, "agent_enrollment.installed") {
		t.Fatalf("log missing install record: %s", logText)
	}
	if strings.Contains(logText, testEnrollmentSecret) || strings.Contains(logText, testEnrollmentDaemonToken) {
		t.Fatalf("log leaked a secret: %s", logText)
	}
}

func TestAgentEnrollmentWorkerRunsBotletsWiringWhenHintPresent(t *testing.T) {
	stubEnsureSyncConfiguredViaDaemon(t)
	// A redeem response carrying a `botlets` hint routes the install through the
	// SAME local brain/registry wiring the team-resync worker uses
	// (registerBotletsBotLocally via the botletsTeamRegisterLocally seam) instead
	// of the generic profile write. The worker maps the hint into a
	// botletsRegisterInput, then runs the shared verify + installed ack.
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	// Stub the generic reload so a stray call would surface as "reload:" in the
	// sequence — the Botlets path must NOT touch it (registerBotletsBotLocally
	// owns its own reload, here stubbed away).
	stubAgentEnrollmentReload(t, record, nil)
	captured := stubBotletsTeamRegister(t, func(in botletsRegisterInput) (botletsRegisterResult, error) {
		return botletsRegisterResult{ProfilePath: filepath.Join(in.Paths.Home, "agents", in.BotHandle+".json")}, nil
	})
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState:     "pending",
		ackBodies:     &ackBodies,
		redeemBotlets: testEnrollmentBotletsHint(),
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)
	// The daemon's --botlets-home selection must flow into the install (same
	// resolution as StartDaemon), not the persisted/default home.
	hintedBotletsHome := filepath.Join(t.TempDir(), "hinted-botlets")
	worker.botletsHomeHint = hintedBotletsHome

	worker.runOnce(context.Background())

	if got := record.joined(); got != "list redeem verify ack" {
		t.Fatalf("call sequence = %q, want botlets wiring (no generic reload)", got)
	}
	if len(*captured) != 1 {
		t.Fatalf("botletsTeamRegisterLocally called %d times, want 1", len(*captured))
	}
	in := (*captured)[0]
	if in.BotletsHome != hintedBotletsHome {
		t.Fatalf("mapped botlets home = %q, want the daemon hint %q", in.BotletsHome, hintedBotletsHome)
	}
	if in.BotHandle != "max.reviewer" || in.BotSlug != "reviewer" {
		t.Fatalf("mapped handle/slug = %q / %q, want max.reviewer / reviewer", in.BotHandle, in.BotSlug)
	}
	if in.AgentSecret != testEnrollmentSecret {
		t.Fatalf("mapped agent secret = %q", in.AgentSecret)
	}
	if in.BaseURL != server.URL {
		t.Fatalf("mapped base url = %q, want %q", in.BaseURL, server.URL)
	}
	// Runtime comes from the HINT (claude), not the local credential (codex).
	if in.Runtime != "claude" {
		t.Fatalf("mapped runtime = %q, want claude (from hint, not credential)", in.Runtime)
	}
	if in.WorkspaceID != "bw_team" || in.BotID != "bot_xyz" || in.BotAgentID != "ag_botxyz" {
		t.Fatalf("mapped brain ids = %#v", in)
	}
	if in.ContainerID != "cont_1" || in.RootFolderID != "fold_1" || in.OwnerAgentID != "ag_owner" {
		t.Fatalf("mapped brain refs = %#v", in)
	}
	if in.SetupGeneration != 7 {
		t.Fatalf("mapped setup generation = %d, want 7", in.SetupGeneration)
	}
	if in.ScheduleTimezone != "America/New_York" {
		t.Fatalf("mapped schedule timezone = %q, want America/New_York (from the hint)", in.ScheduleTimezone)
	}
	if !in.RespondsToMentions {
		t.Fatalf("mapped responds_to_mentions = %v, want true (from the hint)", in.RespondsToMentions)
	}
	if in.BotDisplayName != "Reviewer" {
		t.Fatalf("mapped display name = %q, want Reviewer", in.BotDisplayName)
	}
	if len(ackBodies) != 1 || ackBodies[0]["state"] != "installed" || ackBodies[0]["credential_id"] != testEnrollmentCredID {
		t.Fatalf("installed ack body = %#v", ackBodies)
	}
}

func TestAgentEnrollmentWorkerNoHintUsesGenericPath(t *testing.T) {
	// Without a `botlets` hint the worker keeps the generic profile-write path
	// and never invokes the Botlets wiring seam.
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	botletsCalls := 0
	stubBotletsTeamRegister(t, func(botletsRegisterInput) (botletsRegisterResult, error) {
		botletsCalls++
		return botletsRegisterResult{}, nil
	})
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{listState: "pending", ackBodies: &ackBodies})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	worker.runOnce(context.Background())

	if botletsCalls != 0 {
		t.Fatalf("generic enrollment invoked Botlets wiring %d times, want 0", botletsCalls)
	}
	if got := record.joined(); got != "list redeem reload:max.reviewer verify ack" {
		t.Fatalf("call sequence = %q, want generic profile-write path", got)
	}
	if _, err := os.Stat(enrollmentProfilePath(paths)); err != nil {
		t.Fatalf("generic path did not write the profile: %v", err)
	}
	if len(ackBodies) != 1 || ackBodies[0]["state"] != "installed" {
		t.Fatalf("installed ack body = %#v", ackBodies)
	}
}

func TestAgentEnrollmentWorkerBotletsWiringFailureAcksRetryable(t *testing.T) {
	stubEnsureSyncConfiguredViaDaemon(t)
	// A Botlets local-wiring failure after the credential was minted is a
	// retryable ack (credential kept, enrollment stays redeemed). Verify is NOT
	// reached because the install step failed.
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	stubBotletsTeamRegister(t, func(botletsRegisterInput) (botletsRegisterResult, error) {
		return botletsRegisterResult{}, errors.New("brain projection unavailable")
	})
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState:     "pending",
		ackBodies:     &ackBodies,
		redeemBotlets: testEnrollmentBotletsHint(),
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	if wait := worker.runOnce(context.Background()); wait != agentEnrollmentPollInterval {
		t.Fatalf("wait = %v, want fast poll (retry on next pass)", wait)
	}
	if got := record.joined(); got != "list redeem ack" {
		t.Fatalf("call sequence = %q, want ack without verify after wiring failure", got)
	}
	if worker.etag != "" {
		t.Fatalf("ETag not cleared after retryable Botlets wiring failure: %q", worker.etag)
	}
	if len(ackBodies) != 1 {
		t.Fatalf("ack bodies = %#v", ackBodies)
	}
	body := ackBodies[0]
	if body["state"] != "failed" || body["retryable"] != true || body["failure_code"] != "BOTLETS_WIRING_FAILED" {
		t.Fatalf("botlets wiring ack = %#v, want failed/retryable/BOTLETS_WIRING_FAILED", body)
	}
	if body["credential_id"] != testEnrollmentCredID {
		t.Fatalf("ack credential_id = %#v, want %q", body["credential_id"], testEnrollmentCredID)
	}
}

func TestAgentEnrollmentWorkerSkipsWorkOn304(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	listCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/agent-enrollments", func(w http.ResponseWriter, r *http.Request) {
		listCalls++
		record.add("list")
		switch listCalls {
		case 1:
			if r.Header.Get("If-None-Match") != "" {
				t.Errorf("first poll sent If-None-Match %q", r.Header.Get("If-None-Match"))
			}
			w.Header().Set("ETag", `"etag-empty"`)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"enrollments": []any{}})
		default:
			if got := r.Header.Get("If-None-Match"); got != `"etag-empty"` {
				t.Errorf("second poll If-None-Match = %q, want the first ETag", got)
			}
			w.Header().Set("ETag", `"etag-empty"`)
			w.WriteHeader(http.StatusNotModified)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	if wait := worker.runOnce(context.Background()); wait != agentEnrollmentPollInterval {
		t.Fatalf("first wait = %v", wait)
	}
	if wait := worker.runOnce(context.Background()); wait != agentEnrollmentPollInterval {
		t.Fatalf("304 wait = %v, want fast poll", wait)
	}
	if got := record.joined(); got != "list list" {
		t.Fatalf("304 pass did extra work: %q", got)
	}
}

func TestAgentEnrollmentWorkerRecoversRedeemedUnacked(t *testing.T) {
	// Crash recovery: the daemon redeemed and died before acking. The list
	// returns the redeemed-unacked enrollment; the worker re-redeems (the
	// server revokes the old credential and mints a fresh one) and installs.
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{listState: "redeemed", ackBodies: &ackBodies})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	worker.runOnce(context.Background())
	if got := record.joined(); got != "list redeem reload:max.reviewer verify ack" {
		t.Fatalf("call sequence = %q", got)
	}
	if len(ackBodies) != 1 || ackBodies[0]["state"] != "installed" || ackBodies[0]["credential_id"] != testEnrollmentCredID {
		t.Fatalf("ack bodies = %#v", ackBodies)
	}
	if _, err := os.Stat(enrollmentProfilePath(paths)); err != nil {
		t.Fatalf("profile file missing after recovery: %v", err)
	}
}

func TestAgentEnrollmentWorkerVerify401DoesNotAck(t *testing.T) {
	// 401/403 on /agents/me means the credential is bad: recover by re-redeem
	// on the next poll, never by retrying the same token, and never by acking
	// failed (a non-retryable ack would revoke and terminalize a recoverable
	// enrollment).
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState: "pending",
		agentsMe: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Invalid agent secret"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	if wait := worker.runOnce(context.Background()); wait != agentEnrollmentPollInterval {
		t.Fatalf("wait = %v, want fast poll (next pass re-redeems)", wait)
	}
	if got := record.joined(); got != "list redeem reload:max.reviewer verify" {
		t.Fatalf("call sequence = %q, want NO ack after verify 401", got)
	}
}

func TestAgentEnrollmentWorkerClearsETagAndRetriesAfterTransientRedeemFailure(t *testing.T) {
	// A transient redeem failure (503 ENROLLMENT_REVOKE_FAILED) leaves the
	// enrollment unchanged server-side: same enrollment_id, state ('redeemed'),
	// and credential_id, so the list fingerprint — and thus the ETag — does NOT
	// move. The fake server below returns 304 to ANY conditional request, which
	// is exactly what production would do for an unchanged list. If the worker
	// kept its ETag, the next poll would 304 and the redeem would never be
	// retried — the enrollment is wedged forever. The fix: clear the ETag after a
	// retryable per-item failure so the next poll re-fetches UNCONDITIONALLY and
	// re-redeems.
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	ackBodies := []map[string]any{}

	var serverURL string
	redeemCalls := 0
	var listConditional []string // If-None-Match observed on each list call
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/agent-enrollments", func(w http.ResponseWriter, r *http.Request) {
		record.add("list")
		inm := r.Header.Get("If-None-Match")
		listConditional = append(listConditional, inm)
		// Prove the wedge would happen without the fix: any conditional poll gets
		// a 304. Only an unconditional poll (cleared ETag) sees the live item.
		w.Header().Set("ETag", `"etag-1"`)
		if inm != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(enrollmentListPayload("redeemed", testEnrollmentCredID))
	})
	mux.HandleFunc("/daemon/agent-enrollments/"+testEnrollmentID+"/redeem", func(w http.ResponseWriter, _ *http.Request) {
		record.add("redeem")
		redeemCalls++
		if redeemCalls == 1 {
			// Transient: the revoke/mint half failed; the enrollment stays
			// 'redeemed' with its existing credential_id, so the list fingerprint
			// is unchanged.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "revoke failed", "code": "ENROLLMENT_REVOKE_FAILED"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"enrollment_id": testEnrollmentID,
			"agent":         map[string]any{"agent_id": "ag_reviewer", "handle": "max.reviewer", "display_name": "Reviewer"},
			"local_credential": map[string]any{
				"credential_id": testEnrollmentCredID,
				"agent_secret":  testEnrollmentSecret,
				"base_url":      serverURL,
				"runtime":       "codex",
			},
		})
	})
	mux.HandleFunc("/agents/me", func(w http.ResponseWriter, _ *http.Request) {
		record.add("verify")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"agent_id": "ag_reviewer", "handle": "max.reviewer"})
	})
	mux.HandleFunc("/daemon/agent-enrollments/"+testEnrollmentID+"/ack", func(w http.ResponseWriter, r *http.Request) {
		record.add("ack")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		ackBodies = append(ackBodies, body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	serverURL = server.URL
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	// Pass 1: unconditional fetch (no ETag yet), redeem fails 503. The worker
	// must drop the ETag it just stored so the next poll is unconditional.
	worker.runOnce(context.Background())
	if got := record.joined(); got != "list redeem" {
		t.Fatalf("first pass = %q, want \"list redeem\" (no install after 503)", got)
	}
	if worker.etag != "" {
		t.Fatalf("ETag not cleared after transient redeem failure: %q (would 304-wedge the retry)", worker.etag)
	}

	// Pass 2: because the ETag was cleared, the list call carries no
	// If-None-Match, the server returns the live item (not 304), and the redeem
	// is retried and installs. Without the fix this poll sends
	// If-None-Match:"etag-1", gets a 304, and the enrollment is wedged.
	worker.runOnce(context.Background())
	if got := record.joined(); got != "list redeem list redeem reload:max.reviewer verify ack" {
		t.Fatalf("second pass = %q, want re-fetch + re-redeem + install", got)
	}
	if len(listConditional) != 2 {
		t.Fatalf("list call count = %d (%v), want 2", len(listConditional), listConditional)
	}
	if listConditional[1] != "" {
		t.Fatalf("second poll sent If-None-Match %q, want empty (ETag must be cleared)", listConditional[1])
	}
	if len(ackBodies) != 1 || ackBodies[0]["state"] != "installed" {
		t.Fatalf("ack bodies = %#v, want exactly one installed ack", ackBodies)
	}
}

func TestAgentEnrollmentWorkerVerify401ClearsETagToReRedeem(t *testing.T) {
	// The verify 401 path intentionally does NOT ack: the enrollment stays
	// 'redeemed' with the same credential_id, so the list fingerprint is
	// unchanged. The worker must clear its ETag, otherwise the re-redeem the
	// lifecycle promises never happens (the conditional poll 304s forever).
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState: "pending",
		agentsMe: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Invalid agent secret"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	worker.runOnce(context.Background())
	if got := record.joined(); got != "list redeem reload:max.reviewer verify" {
		t.Fatalf("call sequence = %q, want NO ack after verify 401", got)
	}
	if worker.etag != "" {
		t.Fatalf("ETag not cleared after verify 401: %q (would wedge the re-redeem)", worker.etag)
	}
}

func TestAgentEnrollmentWorkerVerify5xxAcksRetryableFailure(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState: "pending",
		ackBodies: &ackBodies,
		agentsMe: func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	worker.runOnce(context.Background())
	if got := record.joined(); got != "list redeem reload:max.reviewer verify ack" {
		t.Fatalf("call sequence = %q", got)
	}
	if len(ackBodies) != 1 {
		t.Fatalf("ack bodies = %#v", ackBodies)
	}
	body := ackBodies[0]
	if body["state"] != "failed" || body["retryable"] != true {
		t.Fatalf("verify-5xx ack = %#v, want failed retryable", body)
	}
	if body["failure_code"] != "VERIFY_UNAVAILABLE" || body["credential_id"] != testEnrollmentCredID {
		t.Fatalf("verify-5xx ack = %#v", body)
	}
}

// A persistently-failing per-enrollment step (here a 5xx verify) must NOT make
// the worker re-redeem on every poll — that is the #1321 churn, where each
// re-redeem makes the server revoke+mint a credential. The stuck enrollment is
// gated by a per-enrollment exponential backoff, while the LIST poll keeps its
// base cadence (so a concurrent new/healthy enrollment is never delayed). A
// success clears the backoff.
func TestAgentEnrollmentWorkerThrottlesPersistentReRedeem(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	verifyFailing := true
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState: "pending",
		agentsMe: func(w http.ResponseWriter, _ *http.Request) {
			if verifyFailing {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"agent_id": "ag_reviewer", "handle": "max.reviewer"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)
	clock := time.Unix(1_700_000_000, 0)
	worker.nowFn = func() time.Time { return clock }

	// Pass 1: due, redeems + verify(503). The first failure retries at the normal
	// poll cadence (transient tolerance), so no backoff is armed yet, and the loop
	// wait stays at the base interval — backing off the whole loop would delay
	// unrelated new enrollments, so only the stuck item's re-redeem is throttled.
	if wait := worker.runOnce(context.Background()); wait != agentEnrollmentPollInterval {
		t.Fatalf("pass 1 wait = %v, want base poll interval", wait)
	}
	if n := strings.Count(record.joined(), "redeem"); n != 1 {
		t.Fatalf("after pass 1, redeem count = %d, want 1", n)
	}

	// Many more passes WITHOUT advancing the clock: the second failure arms the
	// per-enrollment backoff, and every subsequent pass then SKIPS the re-redeem.
	// The loop wait stays fast throughout (concurrent enrollments aren't delayed).
	// Pre-fix this churned one redeem per poll.
	for i := 0; i < 7; i++ {
		if wait := worker.runOnce(context.Background()); wait != agentEnrollmentPollInterval {
			t.Fatalf("throttled pass wait = %v, want base poll interval", wait)
		}
	}
	if n := strings.Count(record.joined(), "redeem"); n != 2 {
		t.Fatalf("re-redeem not throttled: redeem count = %d after 8 passes, want 2", n)
	}

	// Advancing past the backoff window allows exactly one more re-redeem.
	clock = clock.Add(agentEnrollmentBackoffCap + time.Second)
	worker.runOnce(context.Background())
	if n := strings.Count(record.joined(), "redeem"); n != 3 {
		t.Fatalf("post-backoff redeem count = %d, want 3", n)
	}

	// Recovery: a successful verify acks installed and clears the backoff state.
	verifyFailing = false
	clock = clock.Add(agentEnrollmentBackoffCap + time.Second)
	if wait := worker.runOnce(context.Background()); wait != agentEnrollmentPollInterval {
		t.Fatalf("post-recovery wait = %v, want fast poll", wait)
	}
	if _, stuck := worker.enrollNextAttempt[testEnrollmentID]; stuck {
		t.Fatalf("backoff state not cleared after a successful install")
	}
}

func TestAgentEnrollmentWorkerAckCancelledDeletesProfile(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState: "pending",
		ack: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Enrollment was cancelled", "code": "ENROLLMENT_CANCELLED"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	worker.runOnce(context.Background())
	// The install reload ran, then the cancelled ack deleted the profile,
	// reloaded again so the dead profile never lingers, and CONFIRMED the
	// finished cleanup with a cleanup_done re-ack so the server drains the
	// enrollment from the cleanups list.
	if got := record.joined(); got != "list redeem reload:max.reviewer verify ack reload: ack" {
		t.Fatalf("call sequence = %q", got)
	}
	if _, err := os.Stat(enrollmentProfilePath(paths)); !os.IsNotExist(err) {
		t.Fatalf("profile still on disk after ENROLLMENT_CANCELLED, stat err = %v", err)
	}
}

func TestAgentEnrollmentWorkerAckExpiredDeletesProfile(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState: "pending",
		ack: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Enrollment expired", "code": "ENROLLMENT_EXPIRED"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	worker.runOnce(context.Background())
	if _, err := os.Stat(enrollmentProfilePath(paths)); !os.IsNotExist(err) {
		t.Fatalf("profile still on disk after ENROLLMENT_EXPIRED, stat err = %v", err)
	}
}

func TestAgentEnrollmentWorkerAckFailedDeletesProfile(t *testing.T) {
	// ENROLLMENT_FAILED on the ack means the enrollment is already terminal
	// failed server-side — e.g. the owner DO's sweep timed out a
	// redeemed-unacked enrollment after ~15 minutes and REVOKED its credential
	// (a daemon resuming after a pause sees exactly this). The revoked profile
	// must be cleaned up like cancelled/expired, not reloaded as a broken agent.
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState: "redeemed",
		ack: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Enrollment already failed", "code": "ENROLLMENT_FAILED"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	worker.runOnce(context.Background())
	if got := record.joined(); got != "list redeem reload:max.reviewer verify ack reload: ack" {
		t.Fatalf("call sequence = %q, want cleanup reload + confirm re-ack after ENROLLMENT_FAILED", got)
	}
	if _, err := os.Stat(enrollmentProfilePath(paths)); !os.IsNotExist(err) {
		t.Fatalf("profile still on disk after ENROLLMENT_FAILED, stat err = %v", err)
	}
}

func TestAgentEnrollmentWorkerRetryDoesNotClobberOriginalBackup(t *testing.T) {
	// Pass 1 overwrites a pre-existing profile and hits a retryable reload
	// failure; pass 2 retries the SAME redeemed enrollment and must NOT
	// re-snapshot the enrollment-written profile over the original backup —
	// the eventual cancelled cleanup restores the USER's profile, not the
	// enrollment's revoked credential (Codex round-3).
	paths := testAgentEnrollmentPaths(t)
	if err := os.MkdirAll(filepath.Join(paths.Home, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enrollmentProfilePath(paths), []byte(`{"handle":"max.reviewer","agent_secret":"as_preexisting","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := &callRecorder{}
	reloadFailures := 1
	stubAgentEnrollmentReload(t, record, func(handle string) string {
		if handle != "" && reloadFailures > 0 {
			reloadFailures--
			return "simulated reload failure"
		}
		return ""
	})
	ackCount := 0
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState: "redeemed",
		ack: func(w http.ResponseWriter, _ *http.Request) {
			ackCount++
			w.Header().Set("Content-Type", "application/json")
			if ackCount == 1 {
				// Pass 1's retryable reload-failed ack is accepted.
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
				return
			}
			// Pass 2's ack answers cancelled -> cleanup restores the backup.
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Enrollment was cancelled", "code": "ENROLLMENT_CANCELLED"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	worker.runOnce(context.Background()) // pass 1: overwrite + retryable reload failure
	worker.runOnce(context.Background()) // pass 2: re-redeem, cancelled -> restore

	data, err := os.ReadFile(enrollmentProfilePath(paths))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "as_preexisting") {
		t.Fatalf("profile = %q, want the ORIGINAL pre-existing credential restored after a retried enrollment was cancelled", string(data))
	}
}

func TestAgentEnrollmentWorkerCleanupPreservesPreexistingProfile(t *testing.T) {
	// A profile that already existed for the handle BEFORE this enrollment
	// (manual install, older daemon) must never be deleted by this
	// enrollment's terminal cleanup — only files this enrollment created may
	// be removed.
	paths := testAgentEnrollmentPaths(t)
	if err := os.MkdirAll(filepath.Join(paths.Home, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enrollmentProfilePath(paths), []byte(`{"handle":"max.reviewer","agent_secret":"as_preexisting","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState: "pending",
		ack: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Enrollment was cancelled", "code": "ENROLLMENT_CANCELLED"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	worker.runOnce(context.Background())
	// The cancelled enrollment's cleanup RESTORES the pre-existing install
	// from its backup (the overwrite carried a now-revoked credential),
	// reloads the daemon so the restored profile is live again, and confirms
	// the finished cleanup with a cleanup_done re-ack.
	if got := record.joined(); got != "list redeem reload:max.reviewer verify ack reload: ack" {
		t.Fatalf("call sequence = %q, want a cleanup reload after restoring the pre-existing profile", got)
	}
	data, err := os.ReadFile(enrollmentProfilePath(paths))
	if err != nil {
		t.Fatalf("pre-existing profile was deleted by another enrollment's cancellation: %v", err)
	}
	if !strings.Contains(string(data), "as_preexisting") {
		t.Fatalf("profile = %q, want the ORIGINAL pre-existing credential restored, not the enrollment's revoked one", string(data))
	}
	if _, err := os.Lstat(enrollmentProfilePath(paths) + ".enroll-backup"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup sidecar should be consumed by the restore, stat err = %v", err)
	}
}

func TestAgentEnrollmentWorkerBotletsPartialWriteCleanedUpOnCancelled(t *testing.T) {
	stubEnsureSyncConfiguredViaDaemon(t)
	// registerBotletsBotLocally can fail AFTER its registry/profile write (e.g.
	// the later daemon-orientation step). The retryable ack must still carry
	// the file this enrollment created so a cancelled/expired/failed answer
	// removes the revoked-credential profile instead of leaving it on disk.
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	stubBotletsTeamRegister(t, func(in botletsRegisterInput) (botletsRegisterResult, error) {
		// Simulate the partial write: profile lands, then a later step fails.
		if err := os.MkdirAll(filepath.Join(in.Paths.Home, "agents"), 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(in.Paths.Home, "agents", in.BotHandle+".json")
		if err := os.WriteFile(path, []byte(`{"handle":"`+in.BotHandle+`","agent_secret":"as_partial","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return botletsRegisterResult{}, errors.New("daemon orientation failed")
	})
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState:     "pending",
		ackBodies:     &ackBodies,
		redeemBotlets: testEnrollmentBotletsHint(),
		ack: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Enrollment was cancelled", "code": "ENROLLMENT_CANCELLED"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	worker.runOnce(context.Background())
	// First the BOTLETS_WIRING_FAILED report (answered cancelled -> cleanup),
	// then the cleanup_done confirm re-ack for the finished cleanup.
	if len(ackBodies) != 2 || ackBodies[0]["failure_code"] != "BOTLETS_WIRING_FAILED" {
		t.Fatalf("ack bodies = %#v, want a BOTLETS_WIRING_FAILED ack then a confirm", ackBodies)
	}
	if ackBodies[1]["failure_code"] != "CLEANUP" || ackBodies[1]["cleanup_done"] != true {
		t.Fatalf("ack bodies = %#v, want a cleanup_done confirm re-ack", ackBodies)
	}
	if _, err := os.Stat(enrollmentProfilePath(paths)); !os.IsNotExist(err) {
		t.Fatalf("partially written Botlets profile still on disk after cancellation, stat err = %v", err)
	}
}

func TestResolveDaemonBotletsHomeMatchesStartDaemonOrder(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	envHome := filepath.Join(t.TempDir(), "env-botlets")
	t.Setenv("BOTLETS_HOME", envHome)

	// No hint, no persisted config: the env var wins (StartDaemon parity).
	resolved, err := resolveDaemonBotletsHome(paths, "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != envHome {
		t.Fatalf("resolved = %q, want BOTLETS_HOME %q", resolved, envHome)
	}

	// An explicit hint (the daemon's --botlets-home) outranks the env var.
	hint := filepath.Join(t.TempDir(), "hint-botlets")
	resolved, err = resolveDaemonBotletsHome(paths, hint)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != hint {
		t.Fatalf("resolved = %q, want hint %q", resolved, hint)
	}
}

func TestAgentEnrollmentWorkerReloadFailureAcksRetryable(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, func(string) string { return "daemon reload failed" })
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{listState: "pending", ackBodies: &ackBodies})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	worker.runOnce(context.Background())
	if got := record.joined(); got != "list redeem reload:max.reviewer ack" {
		t.Fatalf("call sequence = %q, want ack without verify after reload failure", got)
	}
	if len(ackBodies) != 1 || ackBodies[0]["state"] != "failed" || ackBodies[0]["retryable"] != true || ackBodies[0]["failure_code"] != "RELOAD_FAILED" {
		t.Fatalf("reload-failure ack = %#v", ackBodies)
	}
}

func TestAgentEnrollmentWorkerStopsPollingOnRevokedToken(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/agent-enrollments", func(w http.ResponseWriter, _ *http.Request) {
		record.add("list")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "Invalid or revoked daemon token", "code": "INVALID_DAEMON_TOKEN"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	if wait := worker.runOnce(context.Background()); wait != agentEnrollmentPairingRecheckInterval {
		t.Fatalf("revoked-token wait = %v, want pairing recheck", wait)
	}
	// Parked: subsequent passes with the same dead token make NO requests.
	if wait := worker.runOnce(context.Background()); wait != agentEnrollmentPairingRecheckInterval {
		t.Fatalf("parked wait = %v", wait)
	}
	if wait := worker.runOnce(context.Background()); wait != agentEnrollmentPairingRecheckInterval {
		t.Fatalf("parked wait = %v", wait)
	}
	if got := record.joined(); got != "list" {
		t.Fatalf("revoked token kept polling: %q", got)
	}
	logText := readBotletsTeamResyncLog(t, paths)
	if got := strings.Count(logText, "agent_enrollment.daemon_token_revoked"); got != 1 {
		t.Fatalf("revoked-token log count = %d, want exactly 1:\n%s", got, logText)
	}

	// Re-pairing with a FRESH token resumes polling.
	writeEnrollmentDaemonAuth(t, paths, server.URL, "ldt_ag_owner_ld_test_fresh-token")
	worker.runOnce(context.Background())
	if got := record.joined(); got != "list list" {
		t.Fatalf("fresh token did not resume polling: %q", got)
	}
}

func TestAgentEnrollmentWorkerBacksOffOnPollErrors(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	failing := true
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/agent-enrollments", func(w http.ResponseWriter, _ *http.Request) {
		record.add("list")
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("ETag", `"ok"`)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"enrollments": []any{}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)

	var waits []time.Duration
	for i := 0; i < 6; i++ {
		waits = append(waits, worker.runOnce(context.Background()))
	}
	for i := 1; i < len(waits); i++ {
		if waits[i] < waits[i-1] {
			t.Fatalf("backoff shrank: %v", waits)
		}
	}
	if waits[0] != agentEnrollmentPollInterval {
		t.Fatalf("first failure wait = %v, want base poll interval", waits[0])
	}
	if last := waits[len(waits)-1]; last != agentEnrollmentBackoffCap {
		t.Fatalf("backoff did not reach cap: %v", waits)
	}

	// One success resets the cadence.
	failing = false
	if wait := worker.runOnce(context.Background()); wait != agentEnrollmentPollInterval {
		t.Fatalf("post-recovery wait = %v, want fast poll", wait)
	}
}

func TestBusUnpairCallsSelfRevokeBeforeDeletingAuth(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	revokes := 0
	var sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/daemon/self-revoke" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		revokes++
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer server.Close()
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_revoke-me",
		Token:    "ldt_revoke_secret",
		BaseURL:  server.URL,
		Label:    "Revoke Mac",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := busUnpair(&out, home, true); err != nil {
		t.Fatalf("busUnpair err = %v\n%s", err, out.String())
	}
	if revokes != 1 {
		t.Fatalf("self-revoke calls = %d, want 1", revokes)
	}
	if sawAuth != "Bearer ldt_revoke_secret" {
		t.Fatalf("self-revoke Authorization = %q", sawAuth)
	}
	if _, statErr := os.Stat(commentbus.DaemonAuthPath(paths)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-auth.json still exists after unpair, stat err = %v", statErr)
	}
	if !strings.Contains(out.String(), "Revoked this daemon on the server") {
		t.Fatalf("output missing server revoke confirmation:\n%s", out.String())
	}
}

func TestBusUnpairProceedsWhenSelfRevokeAnswers401(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "Invalid or revoked daemon token", "code": "INVALID_DAEMON_TOKEN"})
	}))
	defer server.Close()
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_already-gone",
		Token:    "ldt_already_revoked",
		BaseURL:  server.URL,
		Label:    "Gone Mac",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := busUnpair(&out, home, true); err != nil {
		t.Fatalf("busUnpair err = %v", err)
	}
	// daemon-auth.json is still removed (a rejected local token has no value),
	// but a 401 is NOT a confirmed revoke — cf returns the same
	// INVALID_DAEMON_TOKEN for a corrupted/stale token AND an already-revoked
	// daemon, so unpair must NOT claim it was already revoked and must point the
	// user at the web app instead (Codex round-13).
	if _, statErr := os.Stat(commentbus.DaemonAuthPath(paths)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-auth.json still exists, stat err = %v", statErr)
	}
	if strings.Contains(out.String(), "already revoked") {
		t.Fatalf("a rejected token must not be reported as already revoked:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "could NOT be confirmed") || !strings.Contains(out.String(), "Paired computers") {
		t.Fatalf("output missing the unconfirmed-revoke web-app guidance:\n%s", out.String())
	}
}

func TestBusUnpairWarnsWhenSelfRevokeUnreachable(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	// A server that is already closed: the revoke call fails at the network
	// layer, unpair still deletes the local credentials and points the user at
	// the web app for server-side revocation.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := server.URL
	server.Close()
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_unreachable",
		Token:    "ldt_unreachable_secret",
		BaseURL:  deadURL,
		Label:    "Offline Mac",
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := busUnpair(&out, home, true); err != nil {
		t.Fatalf("busUnpair err = %v", err)
	}
	if _, statErr := os.Stat(commentbus.DaemonAuthPath(paths)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-auth.json still exists, stat err = %v", statErr)
	}
	output := out.String()
	if !strings.Contains(output, "could not reach the server") || !strings.Contains(output, "Paired computers") {
		t.Fatalf("output missing unreachable warning with web-app pointer:\n%s", output)
	}
	if strings.Contains(output, "ldt_unreachable_secret") {
		t.Fatalf("output leaked the daemon token:\n%s", output)
	}
}

// readBotletsRegistryHandlesForTest reads the raw registry.json handle list.
func readBotletsRegistryHandlesForTest(t *testing.T, botletsHome string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(botletsHome, "registry.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []struct {
			Handle string `json:"handle"`
		} `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	handles := make([]string, 0, len(registry.Bots))
	for _, bot := range registry.Bots {
		handles = append(handles, bot.Handle)
	}
	return handles
}

func writeBotletsRegistryForEnrollmentTest(t *testing.T, botletsHome string, handles ...string) {
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

func TestAgentEnrollmentWorkerAckCancelledRemovesBotletsRegistryEntry(t *testing.T) {
	// A Botlets enrollment writes the profile AND the registry entry before the
	// ack. When the enrollment turns out cancelled (credential already revoked
	// server-side), the terminal cleanup must roll the registry entry back
	// along with the profile — a surviving entry would point at the removed
	// credential profile and load as MISSING_CREDENTIAL_PROFILE on every later
	// daemon start (Codex round-5).
	stubEnsureSyncConfiguredViaDaemon(t)
	paths := testAgentEnrollmentPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	stubBotletsTeamRegister(t, func(in botletsRegisterInput) (botletsRegisterResult, error) {
		// Simulate the full wiring: profile + registry entry land.
		if err := os.MkdirAll(filepath.Join(in.Paths.Home, "agents"), 0o700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(in.Paths.Home, "agents", in.BotHandle+".json")
		if err := os.WriteFile(path, []byte(`{"handle":"`+in.BotHandle+`","agent_secret":"as_enrolled","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeBotletsRegistryForEnrollmentTest(t, in.BotletsHome, in.BotHandle)
		return botletsRegisterResult{ProfilePath: path}, nil
	})
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState:     "pending",
		redeemBotlets: testEnrollmentBotletsHint(),
		ack: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Enrollment was cancelled", "code": "ENROLLMENT_CANCELLED"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)
	worker.botletsHomeHint = botletsHome

	worker.runOnce(context.Background())
	if _, err := os.Stat(enrollmentProfilePath(paths)); !os.IsNotExist(err) {
		t.Fatalf("profile still on disk after ENROLLMENT_CANCELLED, stat err = %v", err)
	}
	if handles := readBotletsRegistryHandlesForTest(t, botletsHome); len(handles) != 0 {
		t.Fatalf("registry handles = %v, want the cancelled enrollment's entry removed", handles)
	}
	// The cleanup reload runs so the daemon drops the removed bot, then the
	// finished cleanup is confirmed with a cleanup_done re-ack.
	if got := record.joined(); !strings.HasSuffix(got, "ack reload: ack") {
		t.Fatalf("call sequence = %q, want a cleanup reload + confirm after the registry rollback", got)
	}
}

func TestAgentEnrollmentWorkerAckCancelledKeepsRegistryEntryWhenProfileRestored(t *testing.T) {
	// A pre-existing Botlets install (profile + registry entry) that an
	// enrollment overwrote must come back WHOLE when the enrollment is
	// cancelled: the profile is restored from its backup and the still-valid
	// registry entry is kept — removing it would unwire a working bot.
	stubEnsureSyncConfiguredViaDaemon(t)
	paths := testAgentEnrollmentPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	if err := os.MkdirAll(filepath.Join(paths.Home, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enrollmentProfilePath(paths), []byte(`{"handle":"max.reviewer","agent_secret":"as_preexisting","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeBotletsRegistryForEnrollmentTest(t, botletsHome, "max.reviewer")
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	stubBotletsTeamRegister(t, func(in botletsRegisterInput) (botletsRegisterResult, error) {
		path := filepath.Join(in.Paths.Home, "agents", in.BotHandle+".json")
		if err := os.WriteFile(path, []byte(`{"handle":"`+in.BotHandle+`","agent_secret":"as_enrolled","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeBotletsRegistryForEnrollmentTest(t, in.BotletsHome, in.BotHandle)
		return botletsRegisterResult{ProfilePath: path}, nil
	})
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		listState:     "pending",
		redeemBotlets: testEnrollmentBotletsHint(),
		ack: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Enrollment was cancelled", "code": "ENROLLMENT_CANCELLED"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)
	worker.botletsHomeHint = botletsHome

	worker.runOnce(context.Background())
	data, err := os.ReadFile(enrollmentProfilePath(paths))
	if err != nil {
		t.Fatalf("pre-existing profile missing after restore: %v", err)
	}
	if !strings.Contains(string(data), "as_preexisting") {
		t.Fatalf("profile = %q, want the ORIGINAL pre-existing credential restored", string(data))
	}
	if handles := readBotletsRegistryHandlesForTest(t, botletsHome); strings.Join(handles, ",") != "max.reviewer" {
		t.Fatalf("registry handles = %v, want the restored install's entry kept", handles)
	}
}

func TestCleanupClosedEnrollmentReportsIncompleteWhenRegistryRemovalFails(t *testing.T) {
	// Terminal cleanup removed the profile but could not rewrite the Botlets
	// registry. The cleanup must report INCOMPLETE (false) so the caller does
	// NOT send cleanup_done / prune the journal / let the server drain — the
	// cleanups-list retry then re-attempts the registry removal. A true here
	// would strand the entry pointing at the now-missing profile forever
	// (MISSING_CREDENTIAL_PROFILE) (Codex round-8).
	paths := testAgentEnrollmentPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	if err := os.MkdirAll(filepath.Join(paths.Home, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(paths.Home, "agents", "max.reviewer.json")
	if err := os.WriteFile(profilePath, []byte(`{"handle":"max.reviewer","agent_secret":"as_enrolled","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeBotletsRegistryForEnrollmentTest(t, botletsHome, "max.reviewer")
	// Force a deterministic, uid-independent registry-write failure: the
	// registry lock path is a DIRECTORY, so the lock open (O_CREAT|O_RDWR) fails
	// with EISDIR for any user while registry.json stays trusted and readable.
	if err := os.Mkdir(filepath.Join(botletsHome, ".registry.lock"), 0o700); err != nil {
		t.Fatal(err)
	}

	worker := newAgentEnrollmentWorker(paths)
	complete := worker.cleanupClosedEnrollment(context.Background(), "enr_round8", enrollCleanup{
		profilePath:   profilePath,
		botletsHandle: "max.reviewer",
		botletsHome:   botletsHome,
	})
	if complete {
		t.Fatal("cleanupClosedEnrollment returned true; want false when the registry rollback failed")
	}
	// The profile was still removed (state stays consistent; the retry's
	// restore-or-remove is then a no-op) ...
	if _, err := os.Stat(profilePath); !os.IsNotExist(err) {
		t.Fatalf("profile should be removed even when the registry rollback fails, stat err = %v", err)
	}
	// ... and the stale registry entry survives so the retry can remove it.
	if handles := readBotletsRegistryHandlesForTest(t, botletsHome); strings.Join(handles, ",") != "max.reviewer" {
		t.Fatalf("registry handles = %v, want the entry kept for the cleanups-list retry", handles)
	}
}

func TestAgentEnrollmentWorkerCleanupItemRemovesProfileAndRegistryThenDrains(t *testing.T) {
	// A cancel that landed while the daemon was stopped: the poll's `cleanups`
	// list surfaces the redeemed-then-terminal enrollment, and the daemon runs
	// the SAME terminal cleanup the ack-answer path runs — remove the profile
	// this enrollment wrote (revoked credential) plus the Botlets registry
	// entry pointing at it — then re-acks so the server stamps the
	// confirmation that drains the item.
	paths := testAgentEnrollmentPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	if err := os.MkdirAll(filepath.Join(paths.Home, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enrollmentProfilePath(paths), []byte(`{"handle":"max.reviewer","agent_secret":"as_enrolled","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The journal entry the crashed pass recorded before writing: the
	// attribution evidence without which the reconciliation refuses to delete.
	seedEnrollJournalForTest(t, paths, "as_enrolled", botletsHome)
	writeBotletsRegistryForEnrollmentTest(t, botletsHome, "max.reviewer")
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	polls := 0
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		ackBodies: &ackBodies,
		list: func(w http.ResponseWriter, _ *http.Request) {
			polls++
			if polls == 1 {
				_ = json.NewEncoder(w).Encode(enrollmentCleanupListPayload("cancelled"))
				return
			}
			// The confirmed item is drained from the second poll.
			_ = json.NewEncoder(w).Encode(map[string]any{"enrollments": []any{}, "cleanups": []any{}})
		},
		ack: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Enrollment was cancelled", "code": "ENROLLMENT_CANCELLED"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)
	worker.botletsHomeHint = botletsHome

	wait := worker.runOnce(context.Background())
	if wait != agentEnrollmentPollInterval {
		t.Fatalf("wait = %v, want %v", wait, agentEnrollmentPollInterval)
	}
	// Cleanup FIRST (restore-or-remove + reload), then the reconciling re-ack.
	if got := record.joined(); got != "list reload: ack" {
		t.Fatalf("call sequence = %q", got)
	}
	if _, err := os.Stat(enrollmentProfilePath(paths)); !os.IsNotExist(err) {
		t.Fatalf("revoked-credential profile still on disk, stat err = %v", err)
	}
	if handles := readBotletsRegistryHandlesForTest(t, botletsHome); len(handles) != 0 {
		t.Fatalf("registry handles = %v, want the dead enrollment's entry removed", handles)
	}
	if len(ackBodies) != 1 {
		t.Fatalf("ack bodies = %#v, want one reconciling re-ack", ackBodies)
	}
	body := ackBodies[0]
	if body["state"] != "failed" || body["retryable"] != false {
		t.Fatalf("re-ack body = %#v, want failed non-retryable", body)
	}
	if body["credential_id"] != testEnrollmentCredID || body["failure_code"] != "CLEANUP" {
		t.Fatalf("re-ack body = %#v, want the item's credential id echoed", body)
	}
	if body["cleanup_done"] != true {
		t.Fatalf("re-ack body = %#v, want cleanup_done so the server stamps the confirmation", body)
	}
	if _, journaled, _ := enrollJournalLookup(paths, testEnrollmentID); journaled {
		t.Fatal("journal entry survived a confirmed cleanup, want it pruned")
	}
	// The terminal answer confirmed the cleanup, so the fingerprint moves on
	// its own — the ETag is kept, not force-cleared.
	if worker.etag == "" {
		t.Fatal("ETag cleared after a confirmed cleanup, want it kept")
	}

	// A second poll with the item gone does nothing: no cleanup, no re-ack.
	worker.runOnce(context.Background())
	if got := record.joined(); got != "list reload: ack list" {
		t.Fatalf("call sequence after drained poll = %q", got)
	}
	if len(ackBodies) != 1 {
		t.Fatalf("ack bodies after drained poll = %#v, want still one", ackBodies)
	}
}

func TestAgentEnrollmentWorkerCleanupItemRestoresPreexistingBackup(t *testing.T) {
	// The stopped daemon had overwritten a pre-existing install (backup
	// sidecar on disk) for the enrollment that then went terminal. The
	// cleanups reconciliation must RESTORE the original profile from its
	// backup and keep the still-valid registry entry — identical semantics to
	// the terminal-ack answer path.
	paths := testAgentEnrollmentPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	if err := os.MkdirAll(filepath.Join(paths.Home, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enrollmentProfilePath(paths), []byte(`{"handle":"max.reviewer","agent_secret":"as_enrolled","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enrollmentProfilePath(paths)+".enroll-backup", []byte(`{"handle":"max.reviewer","agent_secret":"as_preexisting","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedEnrollJournalForTest(t, paths, "as_enrolled", botletsHome)
	writeBotletsRegistryForEnrollmentTest(t, botletsHome, "max.reviewer")
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		list: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(enrollmentCleanupListPayload("failed"))
		},
		ack: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Enrollment failed", "code": "ENROLLMENT_FAILED"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)
	worker.botletsHomeHint = botletsHome

	worker.runOnce(context.Background())
	data, err := os.ReadFile(enrollmentProfilePath(paths))
	if err != nil {
		t.Fatalf("pre-existing profile missing after restore: %v", err)
	}
	if !strings.Contains(string(data), "as_preexisting") {
		t.Fatalf("profile = %q, want the ORIGINAL pre-existing credential restored", string(data))
	}
	if _, err := os.Lstat(enrollmentProfilePath(paths) + ".enroll-backup"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup sidecar should be consumed by the restore, stat err = %v", err)
	}
	if handles := readBotletsRegistryHandlesForTest(t, botletsHome); strings.Join(handles, ",") != "max.reviewer" {
		t.Fatalf("registry handles = %v, want the restored install's entry kept", handles)
	}
	if got := record.joined(); got != "list reload: ack" {
		t.Fatalf("call sequence = %q", got)
	}
}

func TestCleanupRestoresPreexistingBotletsRegistryEntry(t *testing.T) {
	// Codex round-15: a Botlets enrollment OVERWROTE a working install (profile
	// backed up) and upserted this handle's registry entry with its OWN brain.
	// When the enrollment goes terminal, cleanup restores the profile from
	// backup — and must ALSO roll the registry entry back to the pre-existing
	// brain, or the restored credential runs against the dead enrollment's brain.
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")

	// The pre-existing user install's credential profile, snapshotted to the
	// .enroll-backup the (now-terminal) enrollment took before overwriting it.
	credPath, err := writeBotletsAgentProfile(paths, "max.reviewer", "as_ag_preexisting", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	preexistingBytes, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath+".enroll-backup", preexistingBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// The enrollment overwrote the credential profile with its own secret.
	if _, err := writeBotletsAgentProfile(paths, "max.reviewer", "as_ag_enrolled", "https://comment.io"); err != nil {
		t.Fatal(err)
	}

	mkEntry := func(ws string) commentbus.BotRegistryEntry {
		return commentbus.BotRegistryEntry{
			Name: "reviewer", BotID: "ag_bot", Handle: "max.reviewer", CredentialProfile: credPath,
			BrainRef:       &commentbus.BotBrainRef{WorkspaceID: ws, OwnerAgentID: "ag_owner", BotAgentID: "ag_bot", ContainerID: "lc_brain", RootFolderID: "lf_brain", RelativePath: "Botlets/max/reviewer/brain", SetupGeneration: 1},
			ManagedSession: commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
		}
	}
	// CURRENT registry carries the enrollment's brain (ws_enrollment).
	if _, err := upsertBotletsRegistryReturningEntry(paths, botletsHome, mkEntry("ws_enrollment")); err != nil {
		t.Fatal(err)
	}
	// Journal the pre-existing entry (ws_original) as the rollback snapshot.
	prevRaw, err := json.Marshal(mkEntry("ws_original"))
	if err != nil {
		t.Fatal(err)
	}
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: testEnrollmentID, Handle: "max.reviewer", ProfilePath: credPath,
		SecretSHA256: enrollSecretSHA256("as_ag_enrolled"), BotletsHandle: "max.reviewer",
		BotletsHome: botletsHome, PrevBotletsRegistry: prevRaw,
	}); err != nil {
		t.Fatal(err)
	}
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	worker := newAgentEnrollmentWorker(paths)
	worker.botletsHomeHint = botletsHome

	ok := worker.cleanupClosedEnrollment(context.Background(), testEnrollmentID, enrollCleanup{
		profilePath: credPath, botletsHandle: "max.reviewer", botletsHome: botletsHome,
	})
	if !ok {
		t.Fatal("cleanup reported incomplete, want success")
	}
	// Profile restored to the pre-existing credential.
	data, err := os.ReadFile(credPath)
	if err != nil || !strings.Contains(string(data), "as_ag_preexisting") {
		t.Fatalf("profile = %q err=%v, want the pre-existing credential restored", string(data), err)
	}
	// Registry entry rolled back to the ORIGINAL brain, not the enrollment's.
	got, err := readBotletsRegistryEntryForHandle(botletsHome, "max.reviewer")
	if err != nil || got == nil || got.BrainRef == nil {
		t.Fatalf("registry entry = %+v err=%v, want it present with a brain ref", got, err)
	}
	if got.BrainRef.WorkspaceID != "ws_original" {
		t.Fatalf("restored brain workspace = %q, want ws_original (the pre-existing entry)", got.BrainRef.WorkspaceID)
	}
}

func TestEnrollJournalPreservesPrevBotletsRegistryAcrossPasses(t *testing.T) {
	// First-writer-wins: the FIRST pass (which actually overwrote a pre-existing
	// install) captured the registry snapshot. A retry pass sees the now-
	// enrollment-owned profile, takes no fresh backup, and journals no snapshot —
	// the original must survive so a restore-time rollback can still find it.
	paths := testAgentEnrollmentPaths(t)
	snapshot := json.RawMessage(`{"name":"reviewer","handle":"max.reviewer","brain_ref":{"workspace_id":"ws_original"}}`)
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: testEnrollmentID, Handle: "max.reviewer", ProfilePath: enrollmentProfilePath(paths),
		SecretSHA256: enrollSecretSHA256("as_first"), BotletsHandle: "max.reviewer", BotletsHome: "/x",
		PrevBotletsRegistry: snapshot,
	}); err != nil {
		t.Fatal(err)
	}
	// Retry pass, no snapshot supplied.
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: testEnrollmentID, Handle: "max.reviewer", ProfilePath: enrollmentProfilePath(paths),
		SecretSHA256: enrollSecretSHA256("as_second"), BotletsHandle: "max.reviewer", BotletsHome: "/x",
	}); err != nil {
		t.Fatal(err)
	}
	entry, ok, _ := enrollJournalLookup(paths, testEnrollmentID)
	if !ok {
		t.Fatal("journal entry missing after retry")
	}
	// The journal re-serializes (indents) on save, so compare semantically: the
	// snapshot survived if it still round-trips to the original entry.
	if len(entry.PrevBotletsRegistry) == 0 {
		t.Fatal("snapshot dropped on the retry pass, want it preserved (first-writer-wins)")
	}
	var got commentbus.BotRegistryEntry
	if err := json.Unmarshal(entry.PrevBotletsRegistry, &got); err != nil {
		t.Fatalf("preserved snapshot does not parse: %v", err)
	}
	if got.BrainRef == nil || got.BrainRef.WorkspaceID != "ws_original" {
		t.Fatalf("preserved snapshot brain = %+v, want ws_original from the first pass", got.BrainRef)
	}
}

func TestAgentEnrollmentWorkerCleanupItemRetriesWhenJournalUnreadable(t *testing.T) {
	// The enroll journal exists but is malformed/unreadable, so attribution is
	// indeterminate. The cleanup must NOT confirm-drain (that would abandon a
	// revoked profile the journal might attribute) — it leaves the item listed
	// and clears the ETag so the next poll retries (Codex round-11).
	paths := testAgentEnrollmentPaths(t)
	if err := os.MkdirAll(paths.Bus, 0o700); err != nil {
		t.Fatal(err)
	}
	// Corrupt journal bytes — readable file, unparseable JSON.
	if err := os.WriteFile(enrollJournalPath(paths), []byte("{ this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		ackBodies: &ackBodies,
		list: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(enrollmentCleanupListPayload("cancelled"))
		},
		ack: func(w http.ResponseWriter, _ *http.Request) {
			t.Fatalf("ack must not be sent while journal attribution is indeterminate")
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)
	worker.botletsHomeHint = filepath.Join(t.TempDir(), "botlets")

	worker.runOnce(context.Background())
	if len(ackBodies) != 0 {
		t.Fatalf("ack bodies = %#v, want NO confirm while indeterminate", ackBodies)
	}
	if worker.etag != "" {
		t.Fatal("ETag must be cleared so the next poll retries the indeterminate cleanup")
	}
}

func TestAgentEnrollmentWorkerCleanupItemWithoutLocalProfileStillDrains(t *testing.T) {
	// The daemon died between redeem and the profile write (no journal entry,
	// nothing on disk for the handle). With no attribution evidence the
	// reconciliation touches no local state, but must still confirm so the
	// server stamps and the item drains — otherwise it would be re-listed
	// forever.
	paths := testAgentEnrollmentPaths(t)
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		ackBodies: &ackBodies,
		list: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(enrollmentCleanupListPayload("expired"))
		},
		ack: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Enrollment expired", "code": "ENROLLMENT_EXPIRED"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)
	worker.botletsHomeHint = filepath.Join(t.TempDir(), "botlets")

	worker.runOnce(context.Background())
	if got := record.joined(); got != "list ack" {
		t.Fatalf("call sequence = %q, want the unattributed cleanup to skip local work but still confirm", got)
	}
	if len(ackBodies) != 1 || ackBodies[0]["credential_id"] != testEnrollmentCredID || ackBodies[0]["cleanup_done"] != true {
		t.Fatalf("ack bodies = %#v, want one cleanup_done confirm echoing the credential id", ackBodies)
	}
	if _, err := os.Stat(enrollmentProfilePath(paths)); !os.IsNotExist(err) {
		t.Fatalf("cleanup conjured a profile from nothing, stat err = %v", err)
	}
	if worker.etag == "" {
		t.Fatal("ETag cleared after a confirmed cleanup, want it kept")
	}
}

func TestAgentEnrollmentWorkerCleanupItemPreservesUnattributedProfile(t *testing.T) {
	// The daemon redeemed, crashed BEFORE writing (journal entry recorded with
	// the enrollment's secret), and the handle was later installed by someone
	// else (manual install / another enrollment) — the file on disk holds a
	// DIFFERENT credential than the journal attributes to this enrollment.
	// The reconciliation must leave that working profile (and the registry)
	// alone and only confirm so the item drains (Codex round-6).
	paths := testAgentEnrollmentPaths(t)
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	if err := os.MkdirAll(filepath.Join(paths.Home, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(enrollmentProfilePath(paths), []byte(`{"handle":"max.reviewer","agent_secret":"as_someone_elses","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	seedEnrollJournalForTest(t, paths, "as_enrolled_never_written", botletsHome)
	writeBotletsRegistryForEnrollmentTest(t, botletsHome, "max.reviewer")
	record := &callRecorder{}
	stubAgentEnrollmentReload(t, record, nil)
	ackBodies := []map[string]any{}
	server := newEnrollmentTestServer(t, record, enrollmentServerConfig{
		ackBodies: &ackBodies,
		list: func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(enrollmentCleanupListPayload("cancelled"))
		},
		ack: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "Enrollment was cancelled", "code": "ENROLLMENT_CANCELLED"})
		},
	})
	writeEnrollmentDaemonAuth(t, paths, server.URL, testEnrollmentDaemonToken)
	worker := newAgentEnrollmentWorker(paths)
	worker.botletsHomeHint = botletsHome

	worker.runOnce(context.Background())
	if got := record.joined(); got != "list ack" {
		t.Fatalf("call sequence = %q, want no local cleanup for a profile this enrollment cannot claim", got)
	}
	data, err := os.ReadFile(enrollmentProfilePath(paths))
	if err != nil {
		t.Fatalf("the later install's profile was deleted: %v", err)
	}
	if !strings.Contains(string(data), "as_someone_elses") {
		t.Fatalf("profile = %q, want the later install left untouched", string(data))
	}
	if handles := readBotletsRegistryHandlesForTest(t, botletsHome); strings.Join(handles, ",") != "max.reviewer" {
		t.Fatalf("registry handles = %v, want the later install's entry kept", handles)
	}
	if len(ackBodies) != 1 || ackBodies[0]["cleanup_done"] != true {
		t.Fatalf("ack bodies = %#v, want one cleanup_done confirm", ackBodies)
	}
	if _, journaled, _ := enrollJournalLookup(paths, testEnrollmentID); journaled {
		t.Fatal("stale journal entry survived the confirmed drain, want it pruned")
	}
}
