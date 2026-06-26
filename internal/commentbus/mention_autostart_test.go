//go:build darwin || linux

package commentbus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// mentionAutoStartRecorder is a stubbed launch helper that records each handle
// it was asked to launch and lets a test block until the async launch goroutine
// has called it (and finishMentionAutoStart has run). It stands in for
// launchAgentRuntimeDetached, the same detached `comment run <handle>` the web
// "Start your agent" button uses.
type mentionAutoStartRecorder struct {
	mu         sync.Mutex
	handles    []string
	launchCtxs []context.Context
	launchErrs []error
	launched   chan string
	err        error
}

func newMentionAutoStartRecorder() *mentionAutoStartRecorder {
	return &mentionAutoStartRecorder{launched: make(chan string, 16)}
}

func (r *mentionAutoStartRecorder) launch(ctx context.Context, _ Paths, handle string) error {
	r.mu.Lock()
	r.handles = append(r.handles, handle)
	// Record the launch context and its cancellation state AT LAUNCH TIME so a test
	// can assert the detached launch did NOT observe an already-cancelled parent
	// (the per-ingest WAIT/REWAKE context bug).
	r.launchCtxs = append(r.launchCtxs, ctx)
	var ctxErr error
	if ctx != nil {
		ctxErr = ctx.Err()
	}
	r.launchErrs = append(r.launchErrs, ctxErr)
	err := r.err
	r.mu.Unlock()
	r.launched <- handle
	return err
}

// lastLaunchCtxErr returns ctx.Err() captured at the most recent launch.
func (r *mentionAutoStartRecorder) lastLaunchCtxErr() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.launchErrs) == 0 {
		return nil
	}
	return r.launchErrs[len(r.launchErrs)-1]
}

// lastLaunchCtx returns the context passed to the most recent launch.
func (r *mentionAutoStartRecorder) lastLaunchCtx() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.launchCtxs) == 0 {
		return nil
	}
	return r.launchCtxs[len(r.launchCtxs)-1]
}

func (r *mentionAutoStartRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.handles)
}

// waitForLaunch blocks until one launch is observed or the timeout elapses.
func (r *mentionAutoStartRecorder) waitForLaunch(t *testing.T) string {
	t.Helper()
	select {
	case handle := <-r.launched:
		return handle
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an auto-launch")
		return ""
	}
}

// expectNoLaunch asserts no launch happens within a short window.
func (r *mentionAutoStartRecorder) expectNoLaunch(t *testing.T) {
	t.Helper()
	select {
	case handle := <-r.launched:
		t.Fatalf("unexpected auto-launch for %q", handle)
	case <-time.After(150 * time.Millisecond):
	}
}

// newMentionAutoStartDaemon builds a minimal Daemon wired for the mention
// auto-launch unit tests: a fixed clock, the recorder's launch hook, and empty
// transient-runtime maps (nothing running).
func newMentionAutoStartDaemon(rec *mentionAutoStartRecorder, clock func() time.Time) *Daemon {
	return &Daemon{
		now:                          clock,
		mentionAutoStart:             rec.launch,
		mentionAutoStartState:        map[string]*mentionAutoStartRecord{},
		transientRuntimes:            map[string]*transientRuntime{},
		transientRuntimeMainProfiles: map[string]string{},
		transientRuntimeMainIDs:      map[string]string{},
	}
}

// mentionAutoStartRuntimeRecord builds a main transient-runtime record for the
// recipient with a tmux session/pane target that verifyTransientRuntime can
// inspect. Shell launch mode skips the binary trust-walk (there is no real
// binary in a unit test); RuntimeCommand="claude" makes "claude" the expected
// pane command, which the testTmuxController returns by default for a live pane.
func mentionAutoStartRuntimeRecord(profile, botID string) TransientRuntimeRecord {
	return TransientRuntimeRecord{
		RunID:             "sess_mentionautostartrec01",
		Profile:           profile,
		Role:              RuntimeRoleMain,
		BotID:             botID,
		SessionName:       "comment-guy",
		PaneTarget:        "comment-guy:0.0",
		Runtime:           "claude",
		RuntimeCommand:    []string{"claude"},
		RuntimeLaunchMode: RuntimeLaunchModeShell,
		State:             "alive",
	}
}

// markRuntimeRunningForTest registers a GENUINELY-LIVE main transient runtime for
// the profile: it wires a tmux controller whose session/pane exists so
// verifyTransientRuntime returns healthy, and activeMainTransientRuntimeForTarget
// reports it as already running.
func (d *Daemon) markRuntimeRunningForTest(profile, botID string) {
	record := mentionAutoStartRuntimeRecord(profile, botID)
	tmux := newTestTmuxController()
	tmux.setActivePaneLocked(record.SessionName, record.PaneTarget)
	d.tmux = tmux
	runtime := &transientRuntime{record: record, expectedNames: expectedTransientRuntimeCommandNames(record)}
	d.transientRuntimeMu.Lock()
	d.addTransientRuntimeMainReservationLocked(record)
	d.transientRuntimes[record.RunID] = runtime
	d.transientRuntimeMu.Unlock()
}

// markRuntimeStaleForTest registers a main transient-runtime record whose backing
// tmux session/pane is GONE (the prior session exited). verifyTransientRuntime
// reports CONFLICT for it, so it is a stale record that must NOT permanently
// block auto-start. A real store is wired so the verify-and-forget path can
// delete the stale row exactly as production does.
func (d *Daemon) markRuntimeStaleForTest(t *testing.T, profile, botID string) {
	t.Helper()
	store, err := OpenStore(context.Background(), testDaemonPaths(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	d.store = store
	record := mentionAutoStartRuntimeRecord(profile, botID)
	// Empty tmux controller: the session/pane does not exist, so PaneTarget
	// resolves to a session that fails the liveness check -> CONFLICT (stale).
	d.tmux = newTestTmuxController()
	runtime := &transientRuntime{record: record, expectedNames: expectedTransientRuntimeCommandNames(record)}
	d.transientRuntimeMu.Lock()
	d.addTransientRuntimeMainReservationLocked(record)
	d.transientRuntimes[record.RunID] = runtime
	d.transientRuntimeMu.Unlock()
}

func respondsBot(handle string) BotRegistryEntry {
	return BotRegistryEntry{Name: "guy", Handle: handle, BotID: "bot_guy", RespondsToMentions: true}
}

func TestMentionAutoStartLaunchesOnceForRespondingBot(t *testing.T) {
	rec := newMentionAutoStartRecorder()
	clock := func() time.Time { return time.Unix(1000, 0).UTC() }
	d := newMentionAutoStartDaemon(rec, clock)

	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), "mention")

	if got := rec.waitForLaunch(t); got != "max.guy" {
		t.Fatalf("launched handle = %q, want max.guy", got)
	}
	if rec.count() != 1 {
		t.Fatalf("launch count = %d, want exactly 1", rec.count())
	}
}

// The regression for the P2 finding: when a mention is ingested by an owner
// WAIT/REWAKE request (not the long-lived poller), the ingest ctx is a short-lived
// per-ingest context the caller CANCELS the instant ingest returns. The detached
// launch goroutine used to inherit that ctx, so it observed an already-cancelled
// parent and `launchAgentRuntimeDetached` failed before `comment run` started —
// the opted-in bot stayed silent despite an accepted notification. The launch must
// run on the daemon's long-lived base context instead, so a CANCELLED ingest ctx
// still produces a launch on a LIVE (non-cancelled) context.
func TestMentionAutoStartLaunchesOnCancelledIngestCtx(t *testing.T) {
	rec := newMentionAutoStartRecorder()
	clock := func() time.Time { return time.Unix(1000, 0).UTC() }
	d := newMentionAutoStartDaemon(rec, clock)
	// Wire a live daemon base context (StartDaemon sets this to its daemonCtx); the
	// launch goroutine must derive from it, not from the per-ingest ctx.
	d.baseCtx = context.Background()

	// Simulate the owner WAIT/REWAKE caller: hand ingest a per-ingest context that
	// is ALREADY cancelled by the time the detached launch goroutine runs.
	ingestCtx, cancel := context.WithCancel(context.Background())
	cancel()

	d.maybeAutoStartRuntimeForMention(ingestCtx, "max.guy", respondsBot("max.guy"), "mention")

	if got := rec.waitForLaunch(t); got != "max.guy" {
		t.Fatalf("launched handle = %q, want max.guy (cancelled ingest ctx must not block the launch)", got)
	}
	waitMentionAutoStartIdle(t, d, "max.guy")
	if rec.count() != 1 {
		t.Fatalf("launch count = %d, want exactly 1", rec.count())
	}
	// The launch must NOT have observed the cancelled per-ingest parent.
	if err := rec.lastLaunchCtxErr(); err != nil {
		t.Fatalf("launch ran on a cancelled context (err=%v); it must derive from the daemon base context, not the per-ingest ctx", err)
	}
	if launchCtx := rec.lastLaunchCtx(); launchCtx == ingestCtx {
		t.Fatal("launch reused the per-ingest ctx; it must derive its own context from the daemon base context")
	}
}

// When no daemon base context is wired (a bare &Daemon{} fixture), the launch must
// fall back to context.Background() and still run uncancelled even if the ingest
// ctx was cancelled — never panic or skip the launch.
func TestMentionAutoStartLaunchesWithoutBaseCtxOnCancelledIngest(t *testing.T) {
	rec := newMentionAutoStartRecorder()
	clock := func() time.Time { return time.Unix(1000, 0).UTC() }
	d := newMentionAutoStartDaemon(rec, clock)
	// Intentionally leave d.baseCtx nil (the bare-fixture case).

	ingestCtx, cancel := context.WithCancel(context.Background())
	cancel()

	d.maybeAutoStartRuntimeForMention(ingestCtx, "max.guy", respondsBot("max.guy"), "mention")

	if got := rec.waitForLaunch(t); got != "max.guy" {
		t.Fatalf("launched handle = %q, want max.guy", got)
	}
	waitMentionAutoStartIdle(t, d, "max.guy")
	if err := rec.lastLaunchCtxErr(); err != nil {
		t.Fatalf("launch ran on a cancelled context (err=%v) with nil baseCtx; it must fall back to context.Background()", err)
	}
}

func TestMentionAutoStartCooldownSuppressesSecondImmediateMention(t *testing.T) {
	rec := newMentionAutoStartRecorder()
	clock := func() time.Time { return time.Unix(1000, 0).UTC() }
	d := newMentionAutoStartDaemon(rec, clock)

	// First mention launches; wait for the goroutine to finish so the in-flight
	// flag is cleared and only the cooldown gate remains for the second mention.
	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), "mention")
	rec.waitForLaunch(t)
	waitMentionAutoStartIdle(t, d, "max.guy")

	// Second mention at the SAME clock time is inside the cooldown window — no
	// second launch.
	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), "mention")
	rec.expectNoLaunch(t)
	if rec.count() != 1 {
		t.Fatalf("launch count = %d, want 1 (cooldown suppressed the second)", rec.count())
	}
}

func TestMentionAutoStartLaunchesAgainAfterCooldownElapses(t *testing.T) {
	rec := newMentionAutoStartRecorder()
	now := time.Unix(1000, 0).UTC()
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	d := newMentionAutoStartDaemon(rec, clock)

	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), "mention")
	rec.waitForLaunch(t)
	waitMentionAutoStartIdle(t, d, "max.guy")

	// Advance past the base cooldown; the next mention launches again.
	mu.Lock()
	now = now.Add(mentionAutoStartBaseCooldown + time.Second)
	mu.Unlock()
	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), "mention")
	rec.waitForLaunch(t)
	if rec.count() != 2 {
		t.Fatalf("launch count = %d, want 2 after the cooldown elapsed", rec.count())
	}
}

func TestMentionAutoStartFailureGrowsBackoff(t *testing.T) {
	rec := newMentionAutoStartRecorder()
	rec.err = errors.New("comment CLI not logged in")
	now := time.Unix(1000, 0).UTC()
	var mu sync.Mutex
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	d := newMentionAutoStartDaemon(rec, clock)

	// First (failing) launch.
	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), "mention")
	rec.waitForLaunch(t)
	waitMentionAutoStartIdle(t, d, "max.guy")

	// After one failure the cooldown doubled (base*2). A mention just past the
	// BASE cooldown (but under the doubled window) must still be suppressed.
	mu.Lock()
	now = now.Add(mentionAutoStartBaseCooldown + time.Second)
	mu.Unlock()
	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), "mention")
	rec.expectNoLaunch(t)
	if rec.count() != 1 {
		t.Fatalf("launch count = %d, want 1 (backoff suppressed the early retry)", rec.count())
	}

	// Past the doubled window, the failing bot is retried (still throttled, never
	// looping).
	mu.Lock()
	now = now.Add(mentionAutoStartBaseCooldown * 2)
	mu.Unlock()
	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), "mention")
	rec.waitForLaunch(t)
	if rec.count() != 2 {
		t.Fatalf("launch count = %d, want 2 after the backoff window elapsed", rec.count())
	}
}

func TestMentionAutoStartSkipsRespondsDisabledBot(t *testing.T) {
	rec := newMentionAutoStartRecorder()
	clock := func() time.Time { return time.Unix(1000, 0).UTC() }
	d := newMentionAutoStartDaemon(rec, clock)

	bot := respondsBot("max.guy")
	bot.RespondsToMentions = false
	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", bot, "mention")

	rec.expectNoLaunch(t)
	if rec.count() != 0 {
		t.Fatalf("launch count = %d, want 0 for a responds-disabled bot", rec.count())
	}
}

func TestMentionAutoStartSkipsWhenAlreadyRunning(t *testing.T) {
	rec := newMentionAutoStartRecorder()
	clock := func() time.Time { return time.Unix(1000, 0).UTC() }
	d := newMentionAutoStartDaemon(rec, clock)
	d.markRuntimeRunningForTest("max.guy", "bot_guy")

	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), "mention")

	rec.expectNoLaunch(t)
	if rec.count() != 0 {
		t.Fatalf("launch count = %d, want 0 when a runtime is already running", rec.count())
	}
}

// A STALE main-runtime record (the prior transient session has exited but the
// daemon still holds its record) must NOT suppress a fresh auto-launch — the
// regression for finding 1. verifyTransientRuntime reports CONFLICT for the dead
// session, so the record is forgotten and the opted-in bot relaunches.
func TestMentionAutoStartLaunchesPastStaleRuntimeRecord(t *testing.T) {
	rec := newMentionAutoStartRecorder()
	clock := func() time.Time { return time.Unix(1000, 0).UTC() }
	d := newMentionAutoStartDaemon(rec, clock)
	d.markRuntimeStaleForTest(t, "max.guy", "bot_guy")

	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), "mention")

	if got := rec.waitForLaunch(t); got != "max.guy" {
		t.Fatalf("launched handle = %q, want max.guy (stale record must not block)", got)
	}
	if rec.count() != 1 {
		t.Fatalf("launch count = %d, want 1 (a stale runtime record must not suppress)", rec.count())
	}
	// The stale record was forgotten, not left dangling.
	d.transientRuntimeMu.Lock()
	_, stillTracked := d.transientRuntimes[mentionAutoStartRuntimeRecord("max.guy", "bot_guy").RunID]
	d.transientRuntimeMu.Unlock()
	if stillTracked {
		t.Fatal("stale runtime record was not forgotten after auto-start")
	}
}

// A real "review_requested" raw kind is a deliberate, agent-directed signal —
// like an explicit @mention — so it MUST auto-start the opted-in bot.
func TestMentionAutoStartLaunchesOnReviewRequested(t *testing.T) {
	rec := newMentionAutoStartRecorder()
	clock := func() time.Time { return time.Unix(1000, 0).UTC() }
	d := newMentionAutoStartDaemon(rec, clock)

	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), "review_requested")

	if got := rec.waitForLaunch(t); got != "max.guy" {
		t.Fatalf("launched handle = %q, want max.guy on a review request", got)
	}
	if rec.count() != 1 {
		t.Fatalf("launch count = %d, want exactly 1 for review_requested", rec.count())
	}
}

// The regression for finding 1: NormalizeNotificationKind collapses comment /
// reply / suggestion into the SAME stored "doc.mention" kind as a real mention,
// so a passive comment/reply/suggestion on a followed doc would auto-start the
// bot even though nobody @mentioned it — broader than the opt-in advertises.
// Gating on the RAW kind keeps these from auto-starting; only a real "mention"
// (and "review_requested") does. A scheduled "botlets_task" is the managed
// session's own work, also excluded.
func TestMentionAutoStartSkipsNonMentionRawKinds(t *testing.T) {
	for _, rawKind := range []string{"comment", "reply", "suggestion", "botlets_task", "", "doc.mention"} {
		t.Run(rawKind, func(t *testing.T) {
			rec := newMentionAutoStartRecorder()
			clock := func() time.Time { return time.Unix(1000, 0).UTC() }
			d := newMentionAutoStartDaemon(rec, clock)

			d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), rawKind)

			rec.expectNoLaunch(t)
			if rec.count() != 0 {
				t.Fatalf("launch count = %d, want 0 for raw kind %q", rec.count(), rawKind)
			}
		})
	}
}

func TestMentionAutoStartNoHookIsInert(t *testing.T) {
	// A daemon without the launch hook wired (mentionAutoStart == nil) must be a
	// clean no-op — this is the unit-test / hook-not-injected default.
	d := &Daemon{
		now:                   func() time.Time { return time.Unix(1000, 0).UTC() },
		mentionAutoStartState: map[string]*mentionAutoStartRecord{},
	}
	// Should not panic or block.
	d.maybeAutoStartRuntimeForMention(context.Background(), "max.guy", respondsBot("max.guy"), "mention")
}

// TestIngestCloudNotificationRunsAutoStartOutsideIngestLocks is the regression
// guard for the P1 ingest deadlock. ingestCloudNotification used to invoke the
// mention auto-start INLINE on the new-message success path, while the ingest
// locks (sessionMu / busMu / profileMu) were still held — the deferred
// unlockIngestLocks had not run yet (deferred calls run after the function
// returns). The synchronous part of maybeAutoStartRuntimeForMention does a
// liveness verify-and-forget: when a STALE main-runtime record exists for the
// recipient, mentionAutoStartTargetIsLive -> forgetInactiveTransientRuntime
// WAITS on `<-runtime.done`. The runtime goroutine that closes `done` can be
// parked acquiring busMu inside nudgeTransientReadyQueueHead. Holding busMu in
// the ingest path while waiting on that goroutine = deadlock: a fresh @mention
// hangs the whole daemon.
//
// The fix records the trigger on the success path and fires the auto-start from
// a defer that runs AFTER unlockIngestLocks releases the ingest locks.
//
// This test drives a genuinely-new "mention" through the real ingest path with
// exactly that hazard wired up: a STALE main runtime for the recipient (empty
// PaneTarget -> verifyTransientRuntime returns CONFLICT, so the forget path is
// taken) whose `done` channel is closed by a goroutine that FIRST acquires
// busMu — standing in for the real poller parked on busMu. If the auto-start ran
// inside the ingest locks (the bug), forgetInactiveTransientRuntime would block
// on `<-runtime.done` forever because the closer can't get busMu, and the
// watchdog below fires. With the fix, the locks are released before the verify,
// the closer acquires busMu, closes `done`, and ingest completes.
func TestIngestCloudNotificationRunsAutoStartOutsideIngestLocks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	launched := make(chan string, 1)
	mentionAutoStart := func(_ context.Context, _ Paths, handle string) error {
		launched <- handle
		return nil
	}

	claimedAt, leaseExpiresAt := testCloudLeaseTimes()
	commentID := "cmt_autostartdeadlock"
	lease := &CloudNotificationLease{
		ClaimID:        "clm_autostartdeadlock123456",
		NotificationID: "ntf_autostartdeadlock123456",
		ClaimedAt:      claimedAt,
		LeaseExpiresAt: leaseExpiresAt,
		Notification: CloudNotification{
			ID:         "ntf_autostartdeadlock123456",
			Type:       "mention",
			DocSlug:    "doc-autostart",
			DocTitle:   "Auto Start",
			CommentID:  &commentID,
			FromHandle: "max.sender",
			FromName:   "Max",
			Context:    "Please respond.",
			CreatedAt:  claimedAt,
		},
	}
	client := &fakeNotificationClient{leases: []*CloudNotificationLease{lease}}
	paths := testDaemonPaths(t)
	tmux := newTestTmuxController()
	daemon, err := startDaemonForTest(t, ctx, DaemonOptions{
		Paths:              paths,
		Version:            "test",
		Tmux:               tmux,
		BotletsHome:        filepath.Join(paths.Home, "botlets"),
		NotificationClient: client,
		MentionAutoStart:   mentionAutoStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	loadMessageTestBots(t, paths)

	// Opt the reviewer bot into "Responds to @mentions" so the auto-start gates
	// pass for the incoming mention.
	daemon.profileMu.Lock()
	bot, ok := daemon.profileState.BotRegistry["reviewer"]
	if !ok {
		daemon.profileMu.Unlock()
		t.Fatal("reviewer bot is not loaded")
	}
	bot.RespondsToMentions = true
	daemon.profileState.BotRegistry["reviewer"] = bot
	daemon.profileMu.Unlock()

	// Register a STALE main runtime for max.reviewer. PaneTarget="" makes
	// verifyTransientRuntime short-circuit to CONFLICT, so mentionAutoStartTargetIsLive
	// takes the forgetInactiveTransientRuntime path, which waits on `<-runtime.done`.
	// The closer goroutine reproduces the real hazard: it closes `done` only AFTER
	// acquiring busMu (mirroring the poller parked on busMu inside
	// nudgeTransientReadyQueueHead). forgetInactiveTransientRuntime calls
	// runtime.cancel() before waiting, which is what unblocks the closer here.
	staleRecord := TransientRuntimeRecord{
		RunID:             "sess_autostartdeadlock01",
		Profile:           "max.reviewer",
		Role:              RuntimeRoleMain,
		BotID:             "bot_reviewer_stale",
		SessionName:       "comment-reviewer",
		PaneTarget:        "", // -> verifyTransientRuntime returns CONFLICT (stale)
		Runtime:           "claude",
		RuntimeCommand:    []string{"claude"},
		RuntimeLaunchMode: RuntimeLaunchModeShell,
		State:             "alive",
	}
	done := make(chan struct{})
	cancelCalled := make(chan struct{})
	staleRuntime := &transientRuntime{
		record:        staleRecord,
		expectedNames: expectedTransientRuntimeCommandNames(staleRecord),
		done:          done,
		cancel:        func() { close(cancelCalled) },
	}
	go func() {
		// Wait for forgetInactiveTransientRuntime to signal cancel, then take busMu
		// (the lock the ingest path must NOT be holding) and only then close done.
		<-cancelCalled
		daemon.busMu.Lock()
		daemon.busMu.Unlock()
		close(done)
	}()
	daemon.transientRuntimeMu.Lock()
	daemon.addTransientRuntimeMainReservationLocked(staleRecord)
	daemon.transientRuntimes[staleRecord.RunID] = staleRuntime
	daemon.transientRuntimeMu.Unlock()

	// Drive the ingest under a watchdog: a regression that holds busMu across the
	// synchronous verify-and-forget would deadlock here instead of returning.
	ingestDone := make(chan struct{})
	go func() {
		defer close(ingestDone)
		acquired, ingestErr := daemon.ingestCloudNotification(ctx, "max.reviewer", "reviewer", true, 0, nil)
		if ingestErr != nil || !acquired {
			t.Errorf("ingest acquired=%v err=%+v", acquired, ingestErr)
		}
	}()
	select {
	case <-ingestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ingestCloudNotification hung: auto-start verify-and-forget ran while the ingest locks (busMu) were held")
	}

	// The stale record was forgotten, so the auto-start falls through to a launch.
	select {
	case got := <-launched:
		if got != "max.reviewer" {
			t.Fatalf("launched handle = %q, want max.reviewer", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auto-start launch never fired for the new mention")
	}
	waitMentionAutoStartIdle(t, daemon, "max.reviewer")
}

// completionFailingNotificationClient leases a notification like
// fakeNotificationClient but, the first time a lease is handed out, plants a
// regular file at blockOnLeasePath so a later CompleteCloudNotificationWaitOperation
// write into that directory fails (os.MkdirAll -> ENOTDIR). The lease itself
// still succeeds, so ingest proceeds to the durable insert before the completion
// fails — exactly the durably-stored-but-completion-failed window the auto-start
// fix targets.
type completionFailingNotificationClient struct {
	fakeNotificationClient
	t                *testing.T
	blockOnLeasePath string
	blockedOnce      sync.Once
}

func (c *completionFailingNotificationClient) LeaseNotification(ctx context.Context, profile AgentProfile, leaseTTL time.Duration, leaseHolder string, idempotencyKey string, kinds ...string) (*CloudNotificationLease, error) {
	lease, err := c.fakeNotificationClient.LeaseNotification(ctx, profile, leaseTTL, leaseHolder, idempotencyKey, kinds...)
	if lease != nil && err == nil {
		c.blockedOnce.Do(func() {
			if _, statErr := os.Stat(c.blockOnLeasePath); statErr == nil {
				c.t.Fatalf("%q already exists; cannot plant a blocking file there", c.blockOnLeasePath)
			} else if !os.IsNotExist(statErr) {
				c.t.Fatalf("stat %q: %v", c.blockOnLeasePath, statErr)
			}
			if writeErr := os.WriteFile(c.blockOnLeasePath, []byte("block-mkdir"), 0o600); writeErr != nil {
				c.t.Fatalf("plant blocking file at %q: %v", c.blockOnLeasePath, writeErr)
			}
		})
	}
	return lease, err
}

// TestIngestCloudNotificationArmsAutoStartDespiteCompletionFailure is the
// regression guard for the P2 "opted-in bot stays idle on an accepted @mention"
// bug. The new @mention is DURABLY stored by InsertCloudNotificationMessage
// before the lease-bookkeeping CompleteCloudNotificationWaitOperation write runs.
// The auto-start trigger used to be armed only AFTER that completion write
// succeeded, so a completion failure dropped the launch — and because the
// message is already stored, subsequent poller passes see an existing message
// (a refresh, not a new insert) and never auto-start either. The recipient bot
// then stays idle on an accepted @mention until someone launches it manually.
//
// The fix arms the trigger immediately after the durable insert, BEFORE the
// completion write, so the auto-start depends only on the mention being durably
// stored. This test drives a genuinely-new "mention" through the real ingest
// path with the completion write forced to FAIL (a regular file is planted at
// the OpsDone/notification-wait directory path so os.MkdirAll inside
// WritePrivateFileAtomic returns ENOTDIR). It asserts ingest reports the
// completion error AND the auto-start launch still fires.
func TestIngestCloudNotificationArmsAutoStartDespiteCompletionFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	launched := make(chan string, 1)
	mentionAutoStart := func(_ context.Context, _ Paths, handle string) error {
		launched <- handle
		return nil
	}

	claimedAt, leaseExpiresAt := testCloudLeaseTimes()
	commentID := "cmt_autostartcomplete"
	lease := &CloudNotificationLease{
		ClaimID:        "clm_autostartcompletefail12",
		NotificationID: "ntf_autostartcompletefail12",
		ClaimedAt:      claimedAt,
		LeaseExpiresAt: leaseExpiresAt,
		Notification: CloudNotification{
			ID:         "ntf_autostartcompletefail12",
			Type:       "mention",
			DocSlug:    "doc-autostart-complete",
			DocTitle:   "Auto Start Completion",
			CommentID:  &commentID,
			FromHandle: "max.sender",
			FromName:   "Max",
			Context:    "Please respond.",
			CreatedAt:  claimedAt,
		},
	}
	paths := testDaemonPaths(t)
	// Force the FINAL CompleteCloudNotificationWaitOperation write (after the
	// durable insert) to FAIL — without breaking the earlier begin/record reads of
	// the same OpsDone/notification-wait directory. The lease hook plants a regular
	// FILE at the OpsDone/notification-wait directory path right AFTER the lease is
	// acquired: by then begin + RecordCloudNotificationWaitOperationAttempt have
	// already done their done-op reads (which expect that subdir to be absent), and
	// the only remaining access to it on the new-insert path is the completion
	// WRITE. With a file planted there, WritePrivateFileAtomic's os.MkdirAll
	// returns ENOTDIR (a path component is a file), so the completion fails while
	// the SQLite store insert (paths.History) still succeeds.
	doneWaitDir := filepath.Join(paths.OpsDone, "notification-wait")
	client := &completionFailingNotificationClient{
		fakeNotificationClient: fakeNotificationClient{leases: []*CloudNotificationLease{lease}},
		t:                      t,
		blockOnLeasePath:       doneWaitDir,
	}
	tmux := newTestTmuxController()
	daemon, err := startDaemonForTest(t, ctx, DaemonOptions{
		Paths:              paths,
		Version:            "test",
		Tmux:               tmux,
		BotletsHome:        filepath.Join(paths.Home, "botlets"),
		NotificationClient: client,
		MentionAutoStart:   mentionAutoStart,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	loadMessageTestBots(t, paths)

	// Opt the reviewer bot into "Responds to @mentions" so the auto-start gates
	// pass for the incoming mention.
	daemon.profileMu.Lock()
	bot, ok := daemon.profileState.BotRegistry["reviewer"]
	if !ok {
		daemon.profileMu.Unlock()
		t.Fatal("reviewer bot is not loaded")
	}
	bot.RespondsToMentions = true
	daemon.profileState.BotRegistry["reviewer"] = bot
	daemon.profileMu.Unlock()

	// Drive ingest. The completion write fails, so ingest reports an error — but
	// the mention is durably stored and the trigger is armed before the
	// completion, so the deferred auto-start still launches the opted-in bot.
	acquired, ingestErr := daemon.ingestCloudNotification(ctx, "max.reviewer", "reviewer", true, 0, nil)
	if ingestErr == nil {
		t.Fatal("expected CompleteCloudNotificationWaitOperation to fail, got nil error")
	}
	if acquired {
		t.Fatalf("ingest acquired=%v, want false on completion failure", acquired)
	}

	// The durable insert + pre-completion arming means the auto-start fires
	// despite the completion failure. This is the behavior the fix restores.
	select {
	case got := <-launched:
		if got != "max.reviewer" {
			t.Fatalf("launched handle = %q, want max.reviewer", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auto-start launch never fired after a completion-write failure on a durably-stored new mention")
	}
	waitMentionAutoStartIdle(t, daemon, "max.reviewer")
}

// waitMentionAutoStartIdle blocks until the recipient's in-flight flag clears,
// so a test can deterministically separate the in-flight guard from the cooldown
// guard.
func waitMentionAutoStartIdle(t *testing.T, d *Daemon, profile string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.mentionAutoStartMu.Lock()
		record := d.mentionAutoStartState[profile]
		inFlight := record != nil && record.inFlight
		d.mentionAutoStartMu.Unlock()
		if !inFlight {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("auto-start in-flight flag never cleared")
}
