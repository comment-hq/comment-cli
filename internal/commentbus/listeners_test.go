//go:build darwin || linux

// Same platform tag as daemon_test.go: these tests use its daemon harness
// (startTestDaemon / requestDaemon), which is darwin/linux-only. Without the tag
// the Windows `go test -c` compile job pulls this file in while the harness is
// excluded, breaking the build.

package commentbus

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListenerRegistryPullWaiterRefcount(t *testing.T) {
	r := newListenerRegistry()
	if r.hasPullWaiter("max.bot", "sess_aaaaaaaaaaaaaaaaaaaa", "gen_bbbbbbbbbbbbbbbb") {
		t.Fatal("unexpected waiter before registration")
	}
	release1 := r.registerPullWaiter("max.bot", "sess_aaaaaaaaaaaaaaaaaaaa", "gen_bbbbbbbbbbbbbbbb")
	release2 := r.registerPullWaiter("max.bot", "sess_aaaaaaaaaaaaaaaaaaaa", "gen_bbbbbbbbbbbbbbbb")
	if !r.hasPullWaiter("max.bot", "sess_aaaaaaaaaaaaaaaaaaaa", "gen_bbbbbbbbbbbbbbbb") {
		t.Fatal("waiter not registered")
	}
	// A session-scoped waiter must not be matched by a profile-only lookup.
	if r.hasPullWaiter("max.bot", "", "") {
		t.Fatal("profile-only lookup matched a session-scoped waiter")
	}
	release1()
	if !r.hasPullWaiter("max.bot", "sess_aaaaaaaaaaaaaaaaaaaa", "gen_bbbbbbbbbbbbbbbb") {
		t.Fatal("waiter cleared while a second registration was still held")
	}
	release2()
	if r.hasPullWaiter("max.bot", "sess_aaaaaaaaaaaaaaaaaaaa", "gen_bbbbbbbbbbbbbbbb") {
		t.Fatal("waiter not cleared after both registrations released")
	}
	// Idempotent: calling a release twice does not underflow the count.
	release2()
	if r.hasPullWaiter("max.bot", "sess_aaaaaaaaaaaaaaaaaaaa", "gen_bbbbbbbbbbbbbbbb") {
		t.Fatal("idempotent release changed waiter state")
	}
}

func TestListenerRegistryClaimReleaseBusy(t *testing.T) {
	r := newListenerRegistry()
	now := time.Now().UTC()
	if _, ok := r.claimFor("max.free"); ok {
		t.Fatal("unexpected claim before any claim")
	}
	claim, ok := r.claimListen("max.free", "sess-one", now)
	if !ok || claim.ClaimedBy != "sess-one" {
		t.Fatalf("first claim failed: %+v ok=%v", claim, ok)
	}
	existing, ok := r.claimListen("max.free", "sess-two", now)
	if ok {
		t.Fatal("second claim should be refused (no takeover)")
	}
	if existing.ClaimedBy != "sess-one" {
		t.Fatalf("busy claim should report the original claimant: %+v", existing)
	}
	released, ok := r.releaseListen("max.free")
	if !ok || released.ClaimedBy != "sess-one" {
		t.Fatalf("release failed: %+v ok=%v", released, ok)
	}
	if _, ok := r.releaseListen("max.free"); ok {
		t.Fatal("release of an unclaimed handle should report not-released")
	}
	if _, ok := r.claimListen("max.free", "sess-three", now); !ok {
		t.Fatal("claim after release should succeed")
	}
}

func TestListenerRegistryReleaseScoped(t *testing.T) {
	r := newListenerRegistry()
	now := time.Now().UTC()

	// An anonymous claim (no owning session) is releasable without force/session.
	if _, ok := r.claimListen("max.free", "", now); !ok {
		t.Fatal("anonymous claim should succeed")
	}
	if _, released, mismatch := r.releaseListenScoped("max.free", "", false); !released || mismatch {
		t.Fatalf("anonymous release = released:%v mismatch:%v, want released", released, mismatch)
	}

	// A session-owned claim is NOT releasable by an empty or mismatched session.
	if _, ok := r.claimListen("max.free", "sess-a", now); !ok {
		t.Fatal("owned claim should succeed")
	}
	if _, released, mismatch := r.releaseListenScoped("max.free", "", false); released || !mismatch {
		t.Fatalf("unscoped release of owned claim = released:%v mismatch:%v, want refused", released, mismatch)
	}
	if _, released, mismatch := r.releaseListenScoped("max.free", "sess-b", false); released || !mismatch {
		t.Fatalf("wrong-session release = released:%v mismatch:%v, want refused", released, mismatch)
	}
	// --force releases regardless of owner; matching session also releases.
	if _, released, _ := r.releaseListenScoped("max.free", "", true); !released {
		t.Fatal("force release should succeed")
	}
	if _, ok := r.claimListen("max.free", "sess-c", now); !ok {
		t.Fatal("re-claim should succeed")
	}
	if _, released, mismatch := r.releaseListenScoped("max.free", "sess-c", false); !released || mismatch {
		t.Fatalf("matching-session release = released:%v mismatch:%v, want released", released, mismatch)
	}
}

// sendManagedTestMessage sends a local message to the given recipient bot and
// returns its id.
func sendManagedTestMessage(t *testing.T, paths Paths, capability, fromBot, toBot, reqID, body string) string {
	t.Helper()
	send := requestDaemon(t, paths, map[string]any{
		"id": reqID,
		"op": "messages.send",
		"auth": map[string]any{
			"mode":       "owner",
			"capability": capability,
		},
		"params": map[string]any{
			"from_bot": fromBot,
			"to":       []any{toBot},
			"body":     map[string]any{"format": "markdown", "content": body},
		},
	})
	if !send.OK {
		t.Fatalf("send failed: %+v", send.Error)
	}
	return send.Result.(map[string]any)["messages"].([]any)[0].(map[string]any)["id"].(string)
}

func sessionRecordForBot(t *testing.T, paths Paths, botName string) SessionRecord {
	t.Helper()
	records, err := ListSessionRecords(paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.BotName == botName {
			return record
		}
	}
	t.Fatalf("no session record for bot %q", botName)
	return SessionRecord{}
}

func TestDaemonSkipsBmuxNudgeForClaudeSessionWithRewakeWaiter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	capability := loadMessageTestBots(t, paths)
	tmux := daemon.tmux.(*testTmuxController)

	// Lazily starts the claude reviewer session and performs the first nudge via
	// bmux (no rewake waiter yet).
	messageID := sendManagedTestMessage(t, paths, capability, "sender", "reviewer", "req_rewakeskipsend", "rewake skip body")
	record := sessionRecordForBot(t, paths, "reviewer")
	if record.Runtime != "claude" {
		t.Fatalf("reviewer session runtime = %q, want claude", record.Runtime)
	}
	message, err := daemon.store.GetInboxMessage(ctx, "max.reviewer", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteMessageSpool(paths, message); err != nil {
		t.Fatal(err)
	}

	// A live rewake pull-waiter for this exact session means the session pulls its
	// own messages; the daemon must skip the bmux keystroke.
	deregister := daemon.listeners.registerPullWaiter(record.Profile, record.SessionID, record.Generation)
	defer deregister()

	tmux.mu.Lock()
	sendsBefore := len(tmux.sends)
	tmux.mu.Unlock()
	daemon.sessionMu.Lock()
	updated, nudgeErr := daemon.sendSessionNudgeLocked(record, message)
	daemon.sessionMu.Unlock()
	if nudgeErr != nil {
		t.Fatalf("async-rewake nudge error = %+v", nudgeErr)
	}
	tmux.mu.Lock()
	newSends := append([]string{}, tmux.sends[sendsBefore:]...)
	tmux.mu.Unlock()
	if len(newSends) != 0 {
		t.Fatalf("async-rewake nudge sent runtime input: %#v", newSends)
	}
	if updated.LastNudge.SucceededAt == nil {
		t.Fatalf("async-rewake nudge did not record success: %+v", updated.LastNudge)
	}
	if updated.LastNudge.Stuck {
		t.Fatalf("async-rewake nudge entered the stuck branch: %+v", updated.LastNudge)
	}
	if updated.LastNudge.FailureReason != "" {
		t.Fatalf("async-rewake nudge recorded a failure reason: %q", updated.LastNudge.FailureReason)
	}
	// The ready message must remain unclaimed for the waiter to pull.
	stored, err := daemon.store.GetInboxMessage(ctx, "max.reviewer", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Delivery.State == "claimed" {
		t.Fatalf("async-rewake nudge claimed the message: %+v", stored.Delivery)
	}
	// Persisted record reflects the skip too.
	persisted, err := ReadSessionRecord(paths, record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.LastNudge.SucceededAt == nil || persisted.LastNudge.Stuck {
		t.Fatalf("persisted last nudge after async-rewake skip = %+v", persisted.LastNudge)
	}
}

func TestDaemonBmuxNudgesClaudeSessionWithoutRewakeWaiter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	capability := loadMessageTestBots(t, paths)
	tmux := daemon.tmux.(*testTmuxController)

	messageID := sendManagedTestMessage(t, paths, capability, "sender", "reviewer", "req_nowaiterclaude", "no waiter body")
	record := sessionRecordForBot(t, paths, "reviewer")
	message, err := daemon.store.GetInboxMessage(ctx, "max.reviewer", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteMessageSpool(paths, message); err != nil {
		t.Fatal(err)
	}

	// No rewake waiter registered: a claude session is nudged via bmux as usual.
	tmux.mu.Lock()
	sendsBefore := len(tmux.sends)
	tmux.mu.Unlock()
	daemon.sessionMu.Lock()
	updated, nudgeErr := daemon.sendSessionNudgeLocked(record, message)
	daemon.sessionMu.Unlock()
	if nudgeErr != nil {
		t.Fatalf("nudge error = %+v", nudgeErr)
	}
	tmux.mu.Lock()
	newSends := append([]string{}, tmux.sends[sendsBefore:]...)
	tmux.mu.Unlock()
	if len(newSends) == 0 {
		t.Fatal("claude session with no rewake waiter must still be nudged via bmux")
	}
	if updated.LastNudge.SucceededAt == nil {
		t.Fatalf("nudge did not record success: %+v", updated.LastNudge)
	}
}

func TestDaemonColdStartsManagedBotAndNudgesWithoutWaiter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	capability := loadMessageTestBots(t, paths)
	tmux := daemon.tmux.(*testTmuxController)

	// No session running and no rewake waiter exists at cold-start time; the
	// managed bot must still cold-start and nudge via bmux. (Regression for the
	// whole point of asyncRewake: the skip must never short-circuit cold-start.)
	if records, err := ListSessionRecords(paths); err != nil {
		t.Fatal(err)
	} else if len(records) != 0 {
		t.Fatalf("expected no sessions before send, got %#v", records)
	}

	messageID := sendManagedTestMessage(t, paths, capability, "sender", "reviewer", "req_coldstart", "cold start body")

	record := sessionRecordForBot(t, paths, "reviewer")
	if record.State != "alive" {
		t.Fatalf("cold-started reviewer session state = %q, want alive", record.State)
	}
	tmux.mu.Lock()
	sends := append([]string{}, tmux.sends...)
	tmux.mu.Unlock()
	foundReceiveNudge := false
	for _, send := range sends {
		if strings.Contains(send, "comment messages receive "+messageID) {
			foundReceiveNudge = true
			break
		}
	}
	if !foundReceiveNudge {
		t.Fatalf("cold-start did not bmux-nudge the receive command: %#v", sends)
	}
}

func TestDaemonBmuxNudgesCodexSessionEvenWithRewakeWaiter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	tmux := daemon.tmux.(*testTmuxController)
	// Every launched session's pane reports the codex runtime.
	tmux.defaultPaneCommandSequence = []string{"codex"}

	prependFakeRuntime(t, "codex")
	senderPath := writeAgentProfile(t, paths, "max.sender", map[string]any{
		"agent_secret": "as_sender_profile_secret",
	})
	codexPath := writeAgentProfile(t, paths, "max.codexbot", map[string]any{
		"agent_secret": "as_codex_profile_secret",
	})
	botletsHome := filepath.Join(paths.Home, "botlets")
	writeDaemonBotletsRegistry(t, botletsHome, []map[string]any{
		{
			"name":               "sender",
			"handle":             "max.sender",
			"credential_profile": senderPath,
			"managed_session":    map[string]any{"enabled": true, "runtime": "claude"},
		},
		{
			"name":               "codexbot",
			"handle":             "max.codexbot",
			"credential_profile": codexPath,
			"managed_session":    map[string]any{"enabled": true, "runtime": "codex", "host": "tmux"},
		},
	})
	capability, err := ReadCapability(paths.OwnerCapability)
	if err != nil {
		t.Fatal(err)
	}
	reload := requestDaemon(t, paths, map[string]any{
		"id": "req_codexrewakereload",
		"op": "reload-profiles",
		"auth": map[string]any{
			"mode":       "owner",
			"capability": capability,
		},
		"params": map[string]any{"botlets_home": botletsHome},
	})
	if !reload.OK {
		t.Fatalf("reload failed: %+v", reload.Error)
	}

	messageID := sendManagedTestMessage(t, paths, capability, "sender", "codexbot", "req_codexrewakesend", "codex nudge body")
	record := sessionRecordForBot(t, paths, "codexbot")
	if record.Runtime != "codex" {
		t.Fatalf("codexbot session runtime = %q, want codex", record.Runtime)
	}
	message, err := daemon.store.GetInboxMessage(ctx, "max.codexbot", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteMessageSpool(paths, message); err != nil {
		t.Fatal(err)
	}

	// A codex session must NEVER consult the rewake registry: even with a waiter
	// registered against its identity, it is nudged via bmux.
	deregister := daemon.listeners.registerPullWaiter(record.Profile, record.SessionID, record.Generation)
	defer deregister()

	tmux.mu.Lock()
	sendsBefore := len(tmux.sends)
	tmux.mu.Unlock()
	daemon.sessionMu.Lock()
	updated, nudgeErr := daemon.sendSessionNudgeLocked(record, message)
	daemon.sessionMu.Unlock()
	if nudgeErr != nil {
		t.Fatalf("codex nudge error = %+v", nudgeErr)
	}
	tmux.mu.Lock()
	newSends := append([]string{}, tmux.sends[sendsBefore:]...)
	tmux.mu.Unlock()
	if len(newSends) == 0 {
		t.Fatal("codex session must be nudged via bmux even with a rewake waiter registered")
	}
	if updated.LastNudge.SucceededAt == nil {
		t.Fatalf("codex nudge did not record success: %+v", updated.LastNudge)
	}
}

func TestDaemonRewakeWaitRegistersAndDeregistersPullWaiter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	capability := loadMessageTestBots(t, paths)

	sessionID, err := GenerateLocalID("sess", 0)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := GenerateLocalID("gen", 0)
	if err != nil {
		t.Fatal(err)
	}

	if daemon.listeners.hasPullWaiter("max.reviewer", sessionID, generation) {
		t.Fatal("pull-waiter registered before the rewake wait started")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		requestDaemon(t, paths, map[string]any{
			"id": "req_rewakeregister",
			"op": "messages.wait",
			"auth": map[string]any{
				"mode":       "owner",
				"capability": capability,
			},
			"params": map[string]any{
				"bot":                "reviewer",
				"rewake":             true,
				"session_id":         sessionID,
				"session_generation": generation,
				// Unfiltered wait: a kind-filtered rewake wait deliberately does NOT
				// register a nudge-suppressing waiter (it can't receive other kinds).
				"timeout_ms": 1000,
			},
		})
	}()
	waitForCondition(t, "rewake wait registers a pull-waiter", func() bool {
		return daemon.listeners.hasPullWaiter("max.reviewer", sessionID, generation)
	})
	<-done
	waitForCondition(t, "rewake wait deregisters its pull-waiter", func() bool {
		return !daemon.listeners.hasPullWaiter("max.reviewer", sessionID, generation)
	})
}

func TestDaemonListenClaimHandlesOps(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	capability := loadMessageTestBots(t, paths)
	// max.reviewer is a managed bot; add a free (non-managed) configured profile.
	writeAgentProfile(t, paths, "max.free", map[string]any{"agent_secret": "as_free_profile_secret"})
	reload := requestDaemon(t, paths, map[string]any{
		"id":     "req_listenreload",
		"op":     "reload-profiles",
		"auth":   map[string]any{"mode": "owner", "capability": capability},
		"params": map[string]any{},
	})
	if !reload.OK {
		t.Fatalf("reload failed: %+v", reload.Error)
	}

	// Claiming a daemon-managed handle is refused.
	managedClaim := requestDaemon(t, paths, map[string]any{
		"id":     "req_listenclaimmanaged",
		"op":     "listen.claim",
		"auth":   map[string]any{"mode": "owner", "capability": capability, "profile": "max.reviewer"},
		"params": map[string]any{"profile": "max.reviewer"},
	})
	if managedClaim.OK || managedClaim.Error == nil || managedClaim.Error.Code != "MANAGED_HANDLE" {
		t.Fatalf("managed-handle claim = %+v, want MANAGED_HANDLE", managedClaim)
	}

	// Claiming a free handle succeeds.
	claim := requestDaemon(t, paths, map[string]any{
		"id":     "req_listenclaimfree",
		"op":     "listen.claim",
		"auth":   map[string]any{"mode": "owner", "capability": capability, "profile": "max.free"},
		"params": map[string]any{"profile": "max.free", "session": "sess-listen-one"},
	})
	if !claim.OK {
		t.Fatalf("free-handle claim failed: %+v", claim.Error)
	}
	if result := claim.Result.(map[string]any); result["claimed"] != true || result["claimed_by"] != "sess-listen-one" {
		t.Fatalf("free-handle claim result = %#v", result)
	}

	// A second claim on the held handle is refused.
	busy := requestDaemon(t, paths, map[string]any{
		"id":     "req_listenclaimbusy",
		"op":     "listen.claim",
		"auth":   map[string]any{"mode": "owner", "capability": capability, "profile": "max.free"},
		"params": map[string]any{"profile": "max.free", "session": "sess-listen-two"},
	})
	if busy.OK || busy.Error == nil || busy.Error.Code != "HANDLE_BUSY" {
		t.Fatalf("second claim = %+v, want HANDLE_BUSY", busy)
	}

	// handles reflects managed + claimed state.
	handles := requestDaemon(t, paths, map[string]any{
		"id":     "req_listenhandles",
		"op":     "listen.handles",
		"auth":   map[string]any{"mode": "owner", "capability": capability},
		"params": map[string]any{},
	})
	if !handles.OK {
		t.Fatalf("listen.handles failed: %+v", handles.Error)
	}
	rows := handles.Result.(map[string]any)["handles"].([]any)
	sawManaged := false
	sawClaimedFree := false
	for _, raw := range rows {
		entry := raw.(map[string]any)
		switch entry["handle"] {
		case "max.reviewer":
			if entry["managed"] != true {
				t.Fatalf("max.reviewer should be managed: %#v", entry)
			}
			sawManaged = true
		case "max.free":
			if entry["managed"] != false || entry["claimed"] != true || entry["claimed_by"] != "sess-listen-one" {
				t.Fatalf("max.free listen state = %#v", entry)
			}
			sawClaimedFree = true
		}
	}
	if !sawManaged || !sawClaimedFree {
		t.Fatalf("handles missing expected entries: %#v", rows)
	}

	// Releasing with a non-matching session is refused (no takeover).
	wrongRelease := requestDaemon(t, paths, map[string]any{
		"id":     "req_listenreleasewrong",
		"op":     "listen.release",
		"auth":   map[string]any{"mode": "owner", "capability": capability, "profile": "max.free"},
		"params": map[string]any{"profile": "max.free", "session": "sess-other"},
	})
	if wrongRelease.OK || wrongRelease.Error == nil || wrongRelease.Error.Code != "HANDLE_BUSY" {
		t.Fatalf("wrong-session release = %+v, want HANDLE_BUSY", wrongRelease)
	}

	// Releasing with the matching session frees the handle for a new claim.
	release := requestDaemon(t, paths, map[string]any{
		"id":     "req_listenrelease",
		"op":     "listen.release",
		"auth":   map[string]any{"mode": "owner", "capability": capability, "profile": "max.free"},
		"params": map[string]any{"profile": "max.free", "session": "sess-listen-one"},
	})
	if !release.OK || release.Result.(map[string]any)["released"] != true {
		t.Fatalf("release failed: %+v", release)
	}
	reclaim := requestDaemon(t, paths, map[string]any{
		"id":     "req_listenreclaim",
		"op":     "listen.claim",
		"auth":   map[string]any{"mode": "owner", "capability": capability, "profile": "max.free"},
		"params": map[string]any{"profile": "max.free"},
	})
	if !reclaim.OK {
		t.Fatalf("reclaim after release failed: %+v", reclaim.Error)
	}
}

func TestDaemonListenWaitIsClaimScoped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	capability := loadMessageTestBots(t, paths)
	writeAgentProfile(t, paths, "max.free", map[string]any{"agent_secret": "as_free_profile_secret"})
	if reload := requestDaemon(t, paths, map[string]any{
		"id": "req_claimscopereload", "op": "reload-profiles",
		"auth": map[string]any{"mode": "owner", "capability": capability}, "params": map[string]any{},
	}); !reload.OK {
		t.Fatalf("reload failed: %+v", reload.Error)
	}
	if claim := requestDaemon(t, paths, map[string]any{
		"id": "req_claimscopeclaim", "op": "listen.claim",
		"auth":   map[string]any{"mode": "owner", "capability": capability, "profile": "max.free"},
		"params": map[string]any{"profile": "max.free", "session": "sess-claim"},
	}); !claim.OK {
		t.Fatalf("claim failed: %+v", claim.Error)
	}

	waitReq := func(id string) map[string]any {
		return map[string]any{
			"id": id, "op": "messages.wait",
			"auth":   map[string]any{"mode": "owner", "capability": capability, "profile": "max.free"},
			"params": map[string]any{"profile": "max.free", "rewake": true, "listen_session": "sess-claim", "timeout_ms": 200},
		}
	}

	// Claim still held by this session => a no-message rewake wait times out normally.
	held := requestDaemon(t, paths, waitReq("req_claimscopeheld"))
	if !held.OK {
		t.Fatalf("held wait failed: %+v", held.Error)
	}
	if hr := held.Result.(map[string]any); hr["timeout"] != true || hr["claim_lost"] == true {
		t.Fatalf("held wait = %#v, want timeout (claim still held)", hr)
	}

	// After release, a wait scoped to the same session reports claim_lost so the
	// now-stale listener exits and frees the handle.
	if release := requestDaemon(t, paths, map[string]any{
		"id": "req_claimscoperelease", "op": "listen.release",
		"auth":   map[string]any{"mode": "owner", "capability": capability, "profile": "max.free"},
		"params": map[string]any{"profile": "max.free", "session": "sess-claim"},
	}); !release.OK {
		t.Fatalf("release failed: %+v", release.Error)
	}
	lost := requestDaemon(t, paths, waitReq("req_claimscopelost"))
	if !lost.OK {
		t.Fatalf("lost wait failed: %+v", lost.Error)
	}
	if lost.Result.(map[string]any)["claim_lost"] != true {
		t.Fatalf("post-release wait = %#v, want claim_lost", lost.Result)
	}
}

// TestDaemonRewakeWaitAbortsOnConnectionDrop covers the dropped-waiter case: a
// rewake wait whose connection has gone away (the Claude Code session/hook was
// killed mid-wait) must NOT claim a ready message — it reports claim_lost and
// leaves the message unclaimed for re-dispatch instead of stranding it on a dead
// lease until expiry.
func TestDaemonRewakeWaitAbortsOnConnectionDrop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	capability := loadMessageTestBots(t, paths)
	messageID := sendManagedTestMessage(t, paths, capability, "sender", "reviewer", "req_rewakedropsend", "drop body")

	before, err := daemon.store.GetInboxMessage(ctx, "max.reviewer", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Delivery.State == "claimed" {
		t.Fatalf("message claimed before wait: %+v", before.Delivery)
	}

	profile := "max.reviewer"
	req := SocketRequest{
		ID:     "req_rewakedropwait",
		Op:     "messages.wait",
		Auth:   &SocketAuth{Mode: "owner", Capability: capability, Profile: &profile},
		Params: map[string]any{"profile": "max.reviewer", "rewake": true, "timeout_ms": 5000},
	}
	// A pre-cancelled context stands in for the peer having already disconnected.
	dropped, cancelDrop := context.WithCancel(context.Background())
	cancelDrop()
	resp := daemon.dispatch(dropped, req)
	if !resp.OK {
		t.Fatalf("dropped wait failed: %+v", resp.Error)
	}
	if resp.Result.(map[string]any)["claim_lost"] != true {
		t.Fatalf("dropped-connection wait = %#v, want claim_lost", resp.Result)
	}
	after, err := daemon.store.GetInboxMessage(ctx, "max.reviewer", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Delivery.State == "claimed" {
		t.Fatalf("dropped-connection wait claimed the message: %+v", after.Delivery)
	}
}

// TestRewakeOwnershipLost pins the predicate that decides whether a rewake waiter
// is still entitled to a message it just claimed.
func TestRewakeOwnershipLost(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	req := SocketRequest{Params: map[string]any{"profile": "max.free"}}

	cancelled, cancelIt := context.WithCancel(context.Background())
	cancelIt()
	if !daemon.rewakeOwnershipLost(cancelled, req, "") {
		t.Fatal("cancelled connection should be ownership-lost")
	}
	live := context.Background()
	if !daemon.rewakeOwnershipLost(live, req, "sess-x") {
		t.Fatal("a listen-scoped wait with no claim should be ownership-lost")
	}
	daemon.listeners.claimListen("max.free", "sess-x", daemon.currentTime())
	if daemon.rewakeOwnershipLost(live, req, "sess-x") {
		t.Fatal("a matching held claim should NOT be ownership-lost")
	}
	if !daemon.rewakeOwnershipLost(live, req, "sess-other") {
		t.Fatal("a mismatched listen session should be ownership-lost")
	}
	if daemon.rewakeOwnershipLost(live, req, "") {
		t.Fatal("a non-listen-scoped wait on a live connection should NOT be ownership-lost")
	}
}

// TestDaemonReleaseRewakeMessageReturnsItToReady covers the recovery step for the
// claim-lost-during-receive window: a message claimed by the atomic rewake wait
// is released back to the queue so it re-dispatches instead of dead-leasing.
func TestDaemonReleaseRewakeMessageReturnsItToReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	capability := loadMessageTestBots(t, paths)
	messageID := sendManagedTestMessage(t, paths, capability, "sender", "reviewer", "req_rwrelsend", "release-back body")

	profile := "max.reviewer"
	recvReq := SocketRequest{
		ID:     "req_rwrelclaim",
		Op:     "messages.receive",
		Auth:   &SocketAuth{Mode: "owner", Capability: capability, Profile: &profile},
		Params: map[string]any{"profile": "max.reviewer", "message_id": messageID},
	}
	message, recvErr := daemon.receiveMessage(recvReq)
	if recvErr != nil {
		t.Fatalf("receive failed: %+v", recvErr)
	}
	claimed, err := daemon.store.GetInboxMessage(ctx, "max.reviewer", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Delivery.State != "claimed" {
		t.Fatalf("message not claimed after receive: %+v", claimed.Delivery)
	}

	daemon.releaseRewakeMessage(recvReq, message)
	released, err := daemon.store.GetInboxMessage(ctx, "max.reviewer", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if released.Delivery.State == "claimed" {
		t.Fatalf("releaseRewakeMessage left the message claimed: %+v", released.Delivery)
	}
}

// TestDaemonTransientRuntimeSkipsNudgeForClaudeWithRewakeWaiter covers Feature B:
// a `comment run --runtime claude` runtime arms the rewake hook (COMMENT_IO_LISTEN
// → profile-scoped pull-waiter); while that waiter is live the transient nudge
// path must NOT type the keystroke and must leave the message receivable for the
// waiter to claim.
func TestDaemonTransientRuntimeSkipsNudgeForClaudeWithRewakeWaiter(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	claimedAt, leaseExpiresAt := testCloudLeaseTimes()
	lease := &CloudNotificationLease{
		ClaimID:        "clm_rewakeskipclaim123456789",
		NotificationID: "ntf_rewakeskipcloud12345678",
		ClaimedAt:      claimedAt,
		LeaseExpiresAt: leaseExpiresAt,
		Notification: CloudNotification{
			ID:         "ntf_rewakeskipcloud12345678",
			Type:       "mention",
			DocSlug:    "abc123",
			DocTitle:   "Rewake Skip",
			FromHandle: "max.sender",
			Context:    "A claude comment-run with a live waiter pulls this itself.",
			CreatedAt:  claimedAt,
		},
	}
	paths := testDaemonPaths(t)
	writeAgentProfile(t, paths, "max.codex", map[string]any{
		"agent_secret": "as_codex_profile_secret",
		"base_url":     "https://comment.example",
	})
	runtimePath := prependFakeRuntime(t, "claude")
	tmux := newTestTmuxController()
	tmux.defaultPaneCommandSequence = []string{"claude"}
	client := &fakeNotificationClient{leasesByProfile: map[string][]*CloudNotificationLease{
		"max.codex": {lease},
	}}
	daemon, err := startDaemonForTest(t, ctx, DaemonOptions{
		Paths:              paths,
		Version:            "test",
		Tmux:               tmux,
		BotletsHome:        filepath.Join(paths.Home, "botlets"),
		NotificationClient: client,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	capability, err := ReadCapability(paths.OwnerCapability)
	if err != nil {
		t.Fatal(err)
	}

	// Arm the rewake hook's profile-scoped waiter BEFORE the runtime polls, so the
	// nudge path sees a live waiter and skips the keystroke.
	deregister := daemon.listeners.registerPullWaiter("max.codex", "", "")
	defer deregister()

	start := requestDaemon(t, paths, map[string]any{
		"id": "req_rewakeskipstart",
		"op": "runtime.start",
		"auth": map[string]any{
			"mode":       "owner",
			"profile":    "max.codex",
			"capability": capability,
		},
		"params": map[string]any{
			"profile":         "max.codex",
			"cwd":             paths.Home,
			"runtime_command": []any{runtimePath, "--model", "test"},
		},
	})
	if !start.OK {
		t.Fatalf("runtime.start failed: %+v", start.Error)
	}
	runID := start.Result.(map[string]any)["runtime"].(map[string]any)["run_id"].(string)

	// The cloud message becomes ready (the poller ingests it), but with a live
	// waiter the keystroke is skipped, so it stays unclaimed/receivable.
	var messageID string
	waitForCondition(t, "claude comment-run message ready without keystroke", func() bool {
		daemon.busMu.Lock()
		summary, storeErr := daemon.store.WaitMessageSummary(context.Background(), MessageListFilter{Profile: "max.codex", BotName: "codex"})
		daemon.busMu.Unlock()
		if storeErr != nil || summary == nil {
			return false
		}
		messageID = summary.MessageID
		return true
	})

	// No `messages receive` keystroke should have been typed for this message.
	tmux.mu.Lock()
	for _, send := range tmux.sends {
		if strings.Contains(send, "messages receive") {
			tmux.mu.Unlock()
			t.Fatalf("transient claude nudge typed a keystroke despite a live rewake waiter: %q", send)
		}
	}
	tmux.mu.Unlock()

	// The message is still receivable — the waiter (had it been a real pull) claims it.
	receive := requestDaemon(t, paths, map[string]any{
		"id": "req_rewakeskipreceive",
		"op": "messages.receive",
		"auth": map[string]any{
			"mode":       "owner",
			"profile":    "max.codex",
			"capability": capability,
		},
		"params": map[string]any{"profile": "max.codex", "message_id": messageID},
	})
	if !receive.OK {
		t.Fatalf("message should still be receivable after skip: %+v", receive.Error)
	}
	_ = requestDaemon(t, paths, map[string]any{
		"id":     "req_rewakeskipstop",
		"op":     "runtime.stop",
		"auth":   map[string]any{"mode": "owner", "profile": "max.codex", "capability": capability},
		"params": map[string]any{"run_id": runID},
	})
}

func TestListenerRegistryDropsClaimsForManaged(t *testing.T) {
	r := newListenerRegistry()
	now := time.Now()
	if _, ok := r.claimListen("max.free", "sess-a", now); !ok {
		t.Fatal("claim max.free failed")
	}
	if _, ok := r.claimListen("max.other", "sess-b", now); !ok {
		t.Fatal("claim max.other failed")
	}
	dropped := r.dropClaimsForManaged(map[string]struct{}{"max.free": {}})
	if len(dropped) != 1 || dropped[0] != "max.free" {
		t.Fatalf("dropped = %v, want [max.free]", dropped)
	}
	if _, ok := r.claimFor("max.free"); ok {
		t.Fatal("max.free claim should be dropped after it became managed")
	}
	if _, ok := r.claimFor("max.other"); !ok {
		t.Fatal("max.other claim (still free) should remain")
	}
	// Idempotent / empty-set safe.
	if got := r.dropClaimsForManaged(nil); got != nil {
		t.Fatalf("dropClaimsForManaged(nil) = %v, want nil", got)
	}
}

// TestDaemonListenClaimIdempotentForSameSession covers the daemon-restart-recovery
// re-claim: re-claiming a handle for the SAME session succeeds (so a listener
// re-establishes its claim after a restart wiped the in-memory registry), while a
// different session is still refused HANDLE_BUSY (single-listener, no takeover).
func TestDaemonListenClaimIdempotentForSameSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	capability := loadMessageTestBots(t, paths)
	writeAgentProfile(t, paths, "max.free", map[string]any{"agent_secret": "as_free_profile_secret"})
	if reload := requestDaemon(t, paths, map[string]any{
		"id": "req_idemreload", "op": "reload-profiles",
		"auth": map[string]any{"mode": "owner", "capability": capability}, "params": map[string]any{},
	}); !reload.OK {
		t.Fatalf("reload failed: %+v", reload.Error)
	}
	claim := func(id, session string) SocketResponse {
		return requestDaemon(t, paths, map[string]any{
			"id": id, "op": "listen.claim",
			"auth":   map[string]any{"mode": "owner", "capability": capability, "profile": "max.free"},
			"params": map[string]any{"profile": "max.free", "session": session},
		})
	}
	if first := claim("req_idem1", "sess-a"); !first.OK {
		t.Fatalf("first claim failed: %+v", first.Error)
	}
	// Same session re-claims successfully (idempotent — the restart-recovery path).
	if again := claim("req_idem2", "sess-a"); !again.OK || again.Result.(map[string]any)["claimed"] != true {
		t.Fatalf("same-session re-claim = %#v, want claimed", again)
	}
	// A different session is still refused.
	if other := claim("req_idem3", "sess-b"); other.OK || other.Error == nil || other.Error.Code != "HANDLE_BUSY" {
		t.Fatalf("foreign-session claim = %#v, want HANDLE_BUSY", other)
	}
}

// TestDaemonListenClaimAndRuntimeEstablishMutuallyExclude covers the init race
// between `comment run` (a main runtime) and an impromptu `comment listen claim`
// for the same handle: serialized through listenEstablishMu, exactly one can hold
// a handle, in either order.
func TestDaemonListenClaimAndRuntimeEstablishMutuallyExclude(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	capability := loadMessageTestBots(t, paths)
	writeAgentProfile(t, paths, "max.free", map[string]any{"agent_secret": "as_free_profile_secret"})
	if reload := requestDaemon(t, paths, map[string]any{
		"id": "req_estreload", "op": "reload-profiles",
		"auth": map[string]any{"mode": "owner", "capability": capability}, "params": map[string]any{},
	}); !reload.OK {
		t.Fatalf("reload failed: %+v", reload.Error)
	}

	// A `comment run` mid-launch blocks an impromptu listen.claim for the handle.
	release, _, blocked := daemon.reserveMainEstablish("max.free")
	if blocked {
		t.Fatal("first establish should not be blocked")
	}
	busy := requestDaemon(t, paths, map[string]any{
		"id": "req_estbusy", "op": "listen.claim",
		"auth":   map[string]any{"mode": "owner", "capability": capability, "profile": "max.free"},
		"params": map[string]any{"profile": "max.free", "session": "sess-est"},
	})
	if busy.OK || busy.Error == nil || busy.Error.Code != "HANDLE_BUSY" {
		t.Fatalf("listen.claim during establish = %#v, want HANDLE_BUSY", busy)
	}

	// Once the establish releases (a failed/finished launch), the claim succeeds.
	release()
	ok := requestDaemon(t, paths, map[string]any{
		"id": "req_estok", "op": "listen.claim",
		"auth":   map[string]any{"mode": "owner", "capability": capability, "profile": "max.free"},
		"params": map[string]any{"profile": "max.free", "session": "sess-est"},
	})
	if !ok.OK {
		t.Fatalf("claim after establish release failed: %+v", ok.Error)
	}

	// The reverse direction: a held listen claim blocks a `comment run` establish.
	_, conflict, blocked2 := daemon.reserveMainEstablish("max.free")
	if !blocked2 {
		t.Fatal("establish should be blocked by the held listen claim")
	}
	if conflict.ClaimedBy != "sess-est" {
		t.Fatalf("establish conflict claim = %#v, want ClaimedBy sess-est", conflict)
	}
}
