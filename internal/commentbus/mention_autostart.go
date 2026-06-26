package commentbus

import (
	"context"
	"time"
)

// Mention-driven runtime auto-launch.
//
// When a doc @mention arrives for a bot whose "Responds to @mentions" opt-in is
// on and no runtime is already running, the daemon launches that bot's runtime
// detached — the SAME `comment run <handle>` (COMMENT_IO_SKIP_ATTACH=1) path the
// web "Start your agent" button uses (launchAgentRuntimeDetached). So a user who
// @mentions their guide in a doc gets a reply with no manual `comment run`.
//
// The launch is necessarily best-effort and racy at the edges (the CLI may be
// unauthed, the user may mention three times in a second, two notifications may
// arrive while one launch is mid-flight). The guards below make that safe:
//
//   - A per-recipient in-flight flag prevents a second launch while one is
//     running for the same handle.
//   - A per-recipient cooldown + exponential failure backoff prevents a flurry
//     of mentions, or a bot that fails to launch (broken/unauthed CLI), from
//     relaunch-looping. The cooldown grows on repeated failures up to a cap, and
//     after a small number of consecutive failures the recipient is parked until
//     the cap window elapses.
//   - The running-runtime check and the in-flight reservation happen together
//     under the same lock, so a mention can't slip a second launch past a check
//     that has already decided to launch.
//
// A successful launch resets the failure count (the next mention after the
// launch settles is free to re-launch if the session has since exited).

const (
	// mentionAutoStartBaseCooldown is the minimum gap between auto-launch
	// attempts for the same recipient. A mention inside this window is ignored
	// (the just-launched session is still coming up, or a prior attempt is too
	// recent to retry).
	mentionAutoStartBaseCooldown = 30 * time.Second
	// mentionAutoStartMaxCooldown caps the backoff so a permanently-broken bot is
	// retried at most once per this window rather than never (a transient outage
	// must eventually heal) and never faster. With the base cooldown doubling per
	// consecutive failure, the cap is reached after ~4 failures, so a bot that
	// keeps failing to launch is effectively parked at one attempt per this
	// window — it can never relaunch-loop.
	mentionAutoStartMaxCooldown = 5 * time.Minute
	// mentionAutoStartLaunchTimeout bounds the detached launch goroutine so a wedged
	// `comment run` can't hold the in-flight reservation forever. The goroutine runs
	// on the daemon's long-lived base context (NOT the per-ingest context, which the
	// owner WAIT/REWAKE path cancels the instant ingest returns), so this is the only
	// thing that bounds it short of daemon shutdown. It is generous enough to cover a
	// slow skip-attach start — the launch helper applies its own tighter exec timeout
	// (runtimeRequestLaunchTimeout) — while still preventing an indefinite hang.
	mentionAutoStartLaunchTimeout = 2 * time.Minute
)

// mentionAutoStartRecord is the per-recipient backoff + in-flight state. Guarded
// by Daemon.mentionAutoStartMu.
type mentionAutoStartRecord struct {
	// inFlight is true while a launch goroutine for this recipient is running.
	inFlight bool
	// lastAttempt is when the most recent launch was started (set when a launch
	// is reserved, used for the cooldown gate).
	lastAttempt time.Time
	// failures counts consecutive launch failures; reset to 0 on success. Drives
	// the cooldown growth.
	failures int
}

// cooldownFor returns the cooldown window this record's failure count earns:
// the base cooldown doubled per consecutive failure, capped at the max.
func (r *mentionAutoStartRecord) cooldownFor() time.Duration {
	if r == nil || r.failures <= 0 {
		return mentionAutoStartBaseCooldown
	}
	cooldown := mentionAutoStartBaseCooldown
	for i := 0; i < r.failures && cooldown < mentionAutoStartMaxCooldown; i++ {
		cooldown *= 2
	}
	if cooldown > mentionAutoStartMaxCooldown {
		cooldown = mentionAutoStartMaxCooldown
	}
	return cooldown
}

// maybeAutoStartRuntimeForMention is called from ingestCloudNotification AFTER a
// genuinely new incoming doc-notification message is stored. It launches the
// recipient bot's runtime asynchronously when every gate passes; it never blocks
// the caller (the poller) and holds the daemon lock only for the small backoff
// check/reservation, never across the launch.
//
// The gate keys on the RAW notification kind (the upstream notification.Type),
// NOT the stored/normalized message kind. NormalizeNotificationKind collapses
// mention / reply / comment / suggestion all into "doc.mention", so the
// normalized kind cannot tell a real @mention apart from an ordinary
// comment/reply/suggestion on a followed doc. Auto-start is the "Responds to
// @mentions" opt-in — it must fire ONLY when the bot was actually @mentioned (or
// had a review requested), never on a passive comment/reply/suggestion. The raw
// kind is threaded from the lease so the gate sees the un-normalized signal.
//
// Gates, in order:
//  1. auto-launch wired (mentionAutoStart != nil)
//  2. rawKind is an explicit @mention or review request (not a comment / reply /
//     suggestion, a botlets.task, or any other notification)
//  3. the recipient bot opted in (RespondsToMentions)
//  4. no GENUINELY-LIVE runtime is already running for the recipient — a stale
//     main-runtime record (the prior transient/tmux session has exited but the
//     daemon still holds its record) is verified-and-forgotten so it does NOT
//     permanently block auto-start (mentionAutoStartTargetIsLive)
//  5. no launch is in-flight for the recipient, and the cooldown/backoff window
//     has elapsed
func (d *Daemon) maybeAutoStartRuntimeForMention(ctx context.Context, profile string, bot BotRegistryEntry, rawKind string) {
	if d.mentionAutoStart == nil {
		return
	}
	if !mentionAutoStartRawKindEligible(rawKind) {
		return
	}
	if !bot.RespondsToMentions {
		return
	}
	if profile == "" || bot.Handle != profile {
		// Defensive: the recipient handle is what `comment run <handle>` launches.
		// A blank or mismatched target can never resolve a profile, so skip rather
		// than exec a meaningless launch.
		return
	}
	// Suppress only when a GENUINELY-LIVE runtime is already serving the recipient.
	// A bare "a record exists" check would let a STALE record (the prior session
	// exited but the daemon still holds its main-runtime record) permanently block
	// auto-start until the daemon restarts. mentionAutoStartTargetIsLive verifies
	// the record and forgets it when it's dead — the same verify-and-forget the
	// `runtime.start` path uses — so a stale record falls through to a fresh launch
	// while a live one still suppresses. The reservation below (held under
	// mentionAutoStartMu) still serializes concurrent mentions into one launch.
	if d.mentionAutoStartTargetIsLive(ctx, profile, bot) {
		return
	}
	if !d.reserveMentionAutoStart(profile) {
		return
	}
	d.logger.info("mention.auto_start.launching", map[string]any{
		"profile":  profile,
		"bot":      bot.Name,
		"raw_kind": rawKind,
	})
	// The detached launch must NOT inherit the ingest ctx: when this mention was
	// ingested by an owner WAIT/REWAKE request (not the long-lived background
	// poller), that ctx is a short-lived per-ingest context the caller CANCELS the
	// instant ingest returns. A launch goroutine carrying it would see an
	// already-cancelled parent and fail before `comment run` even starts — the
	// opted-in bot stays silent despite an accepted notification. runMentionAutoStart
	// derives its own context from the daemon's long-lived base context instead, so
	// the launch outlives the per-ingest request (bounded by its own internal
	// timeout, and torn down only on daemon shutdown). The synchronous liveness check
	// above intentionally KEEPS the ingest ctx — it runs while the caller's request
	// is still alive and should honor its deadline.
	go d.runMentionAutoStart(profile, bot)
}

// mentionAutoStartTargetIsLive reports whether a GENUINELY-LIVE main runtime is
// already serving the recipient, so a fresh auto-launch should be suppressed. It
// reuses the daemon's verify-and-forget mechanism (verifyTransientRuntime +
// forgetInactiveTransientRuntime), the same liveness check the `runtime.start`
// path applies before reusing/replacing an existing record:
//
//   - no record at all -> not live (launch).
//   - verify nil (healthy) or PANE_BUSY (busy but alive) -> live (suppress).
//   - verify error other than CONFLICT (e.g. a transient inspect failure) ->
//     treated as live (suppress) — conservatively avoid double-launching while
//     the runtime's liveness is merely unknown; the next mention re-checks.
//   - verify CONFLICT (the session/pane is gone) -> the record is STALE: forget
//     it and report not-live so the opted-in bot relaunches. Only when forget
//     can't drop it AND a record still resolves do we suppress, mirroring
//     startTransientRuntime's race-safe fallback.
//
// The caller holds no daemon lock here; the in-flight reservation taken next (and
// re-checked under mentionAutoStartMu) is what serializes concurrent mentions, so
// reading liveness outside that lock cannot double-launch.
func (d *Daemon) mentionAutoStartTargetIsLive(ctx context.Context, profile string, bot BotRegistryEntry) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	runtime := d.activeMainTransientRuntimeForTarget(profile, bot)
	if runtime == nil {
		return false
	}
	runtimeErr := d.verifyTransientRuntime(ctx, runtime)
	if runtimeErr == nil || runtimeErr.Code == "PANE_BUSY" {
		return true
	}
	if runtimeErr.Code != "CONFLICT" {
		return true
	}
	// CONFLICT == the underlying session/pane is gone. Forget the stale record so
	// it stops blocking auto-start; only keep suppressing if the forget lost a race
	// and a record still resolves for the target.
	if !d.forgetInactiveTransientRuntime(runtime, runtimeErr) && d.activeMainTransientRuntimeForTarget(profile, bot) != nil {
		return true
	}
	return false
}

// mentionAutoStartRawKindEligible reports whether a RAW upstream notification
// kind represents a deliberate, agent-directed signal that should drive an
// auto-launch under the "Responds to @mentions" opt-in.
//
// Only an explicit "mention" (the bot was actually @mentioned) and a
// "review_requested" (the bot was asked to review) qualify. "comment", "reply",
// and "suggestion" are ordinary activity on a doc the bot merely follows — they
// must NOT auto-start, even though NormalizeNotificationKind collapses all of
// mention / reply / comment / suggestion into the single "doc.mention" stored
// kind. Gating on the raw kind keeps auto-start as narrow as the opt-in
// advertises. A scheduled "botlets_task" is the managed session's own work, not
// an interactive mention, so it is excluded too.
func mentionAutoStartRawKindEligible(rawKind string) bool {
	switch rawKind {
	case "mention", "review_requested":
		return true
	default:
		return false
	}
}

// reserveMentionAutoStart records a launch attempt for the recipient and returns
// true when the caller may proceed with the launch. It returns false when a
// launch is already in-flight or the cooldown/backoff window has not elapsed.
// The reservation (inFlight=true, lastAttempt=now) is committed under the lock
// so it is visible to a concurrent mention before the launch goroutine runs.
func (d *Daemon) reserveMentionAutoStart(profile string) bool {
	d.mentionAutoStartMu.Lock()
	defer d.mentionAutoStartMu.Unlock()
	if d.mentionAutoStartState == nil {
		d.mentionAutoStartState = map[string]*mentionAutoStartRecord{}
	}
	record := d.mentionAutoStartState[profile]
	if record == nil {
		record = &mentionAutoStartRecord{}
		d.mentionAutoStartState[profile] = record
	}
	if record.inFlight {
		return false
	}
	now := d.nowUTC()
	if !record.lastAttempt.IsZero() && now.Sub(record.lastAttempt) < record.cooldownFor() {
		return false
	}
	record.inFlight = true
	record.lastAttempt = now
	return true
}

// finishMentionAutoStart clears the in-flight flag and updates the failure
// count: a success resets it to 0, a failure increments it (growing the next
// cooldown). The lastAttempt timestamp set at reservation time is left as-is so
// the cooldown is measured from when the launch STARTED.
func (d *Daemon) finishMentionAutoStart(profile string, launchErr error) {
	d.mentionAutoStartMu.Lock()
	defer d.mentionAutoStartMu.Unlock()
	record := d.mentionAutoStartState[profile]
	if record == nil {
		return
	}
	record.inFlight = false
	if launchErr == nil {
		record.failures = 0
		return
	}
	record.failures++
}

// runMentionAutoStart performs the detached launch outside all daemon locks and
// records the outcome for backoff. It is the goroutine body spawned by
// maybeAutoStartRuntimeForMention.
//
// It deliberately does NOT take the ingest ctx: the owner WAIT/REWAKE ingest path
// cancels its per-ingest context the instant ingest returns, which would race this
// detached goroutine and cancel the launch before it starts. Instead it derives a
// fresh, bounded context from the daemon's long-lived base context (mentionAutoStartLaunchCtx),
// so the launch survives the per-ingest request and is torn down only by its own
// timeout or daemon shutdown.
func (d *Daemon) runMentionAutoStart(profile string, bot BotRegistryEntry) {
	ctx, cancel := d.mentionAutoStartLaunchCtx()
	defer cancel()
	launchErr := d.mentionAutoStart(ctx, d.paths, bot.Handle)
	d.finishMentionAutoStart(profile, launchErr)
	if launchErr != nil {
		// The launch helper already logs + (on the runtime-request path) acks its
		// own failure detail; this is the auto-launch-specific record for the
		// backoff that just incremented. RETRYABLE by design: the backoff above
		// throttles the next attempt rather than looping.
		d.logger.warn("mention.auto_start.failed", map[string]any{
			"profile": profile,
			"bot":     bot.Name,
			"error":   launchErr.Error(),
		})
		return
	}
	d.logger.info("mention.auto_start.started", map[string]any{
		"profile": profile,
		"bot":     bot.Name,
	})
}

// mentionAutoStartLaunchCtx builds the context for the detached launch goroutine.
// It is rooted at the daemon's long-lived base context (d.baseCtx) — NOT any
// per-ingest context — so the launch outlives the owner WAIT/REWAKE request that
// triggered it (that request cancels its per-ingest context the moment ingest
// returns). It is bounded by mentionAutoStartLaunchTimeout so a wedged launch can't
// hold the in-flight reservation indefinitely, and it is cancelled when the daemon
// shuts down (d.baseCtx is the daemonCtx StartDaemon cancels on Close). Falls back
// to context.Background() for bare &Daemon{} test fixtures that never set baseCtx.
// The returned cancel MUST be called (the caller defers it) to release the timer.
func (d *Daemon) mentionAutoStartLaunchCtx() (context.Context, context.CancelFunc) {
	base := d.baseCtx
	if base == nil {
		base = context.Background()
	}
	return context.WithTimeout(base, mentionAutoStartLaunchTimeout)
}

// nowUTC returns the daemon's clock (test-overridable via Daemon.now) in UTC,
// falling back to the wall clock when unset (tests that build a bare &Daemon{}).
func (d *Daemon) nowUTC() time.Time {
	if d.now != nil {
		return d.now().UTC()
	}
	return time.Now().UTC()
}
