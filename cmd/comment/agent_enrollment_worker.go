package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// Daemon-mediated agent enrollment worker (Phase 3).
//
// The browser creates pending enrollments on the owner's account; this worker
// is the daemon half: it fast-polls GET /daemon/agent-enrollments with the
// paired daemon token from `<home>/bus/daemon-auth.json`, redeems each
// pending (or crash-recovered redeemed-unacked) enrollment for a daemon-bound
// generic local credential, writes `~/.comment-io/agents/<handle>.json`,
// reloads the daemon's profiles, verifies the credential with a plain
// GET /agents/me (NOT ?setup_connect=1 — that is a browser setup-page side
// channel that would falsely advance the setup checklist), and acks the
// outcome.
//
// Failure classification (per the enrollment lifecycle):
//   - verify 401/403  -> the credential is bad; recover by RE-REDEEM, never by
//     retrying the same token. No ack: the enrollment stays redeemed and the
//     next poll re-redeems (the server revokes the old credential and mints a
//     replacement).
//   - verify 5xx / network -> ack {state:"failed", retryable:true}; the
//     enrollment stays redeemed with the failure visible in the browser.
//   - ack answered ENROLLMENT_CANCELLED / ENROLLMENT_EXPIRED -> the credential
//     is already revoked server-side; delete the profile file this worker
//     wrote and reload so a dead profile never lingers on disk.
//
// The worker no-ops cleanly while unpaired and re-checks daemon-auth.json
// periodically so pairing mid-run activates it without a daemon restart. A
// revoked daemon token (401/403 on the list call) is logged once and polling
// stops until the pairing file changes.
const (
	// agentEnrollmentPollInterval is the fast poll cadence — a human is
	// watching a browser spinner, so this is deliberately NOT the 30s Botlets
	// team-resync cadence. The ETag/If-None-Match pair on
	// GET /daemon/agent-enrollments makes an unchanged poll a near-free 304.
	agentEnrollmentPollInterval = 4 * time.Second
	// agentEnrollmentPairingRecheckInterval is how often the worker re-reads
	// daemon-auth.json while unpaired or after its token was revoked.
	agentEnrollmentPairingRecheckInterval = 30 * time.Second
	// agentEnrollmentBackoffCap bounds the exponential backoff applied both to
	// repeated list-poll errors (network / 5xx) and to per-enrollment re-redeem
	// retries. Each counter resets independently on its first success.
	agentEnrollmentBackoffCap     = 60 * time.Second
	agentEnrollmentRequestTimeout = 30 * time.Second
)

var (
	agentEnrollmentHTTPClient = &http.Client{Timeout: agentEnrollmentRequestTimeout}
	// agentEnrollmentReloadProfiles asks the daemon to re-read the generic
	// agent profiles (the same socket reload `comment bus reload-profiles`
	// uses). Returns "" on success or an error description. Stubbed in tests.
	agentEnrollmentReloadProfiles = reloadAgentProfilesViaSocket
)

func startAgentEnrollmentWorker(ctx context.Context, paths commentbus.Paths, botletsHomeHint string) {
	go runAgentEnrollmentWorker(ctx, paths, botletsHomeHint)
}

func runAgentEnrollmentWorker(ctx context.Context, paths commentbus.Paths, botletsHomeHint string) {
	worker := newAgentEnrollmentWorker(paths)
	worker.botletsHomeHint = botletsHomeHint
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			wait := worker.runOnce(ctx)
			timer.Reset(wait)
		}
	}
}

// agentEnrollmentWorker carries the mutable poll state between passes: the
// last list ETag, the consecutive poll-error count driving backoff, the
// consecutive per-enrollment retry count driving re-redeem backoff, and the
// exact token observed revoked (so a re-pair with a fresh token resumes
// polling immediately while the dead token stays parked).
type agentEnrollmentWorker struct {
	paths commentbus.Paths
	// botletsHomeHint is the daemon's --botlets-home value, plumbed through so
	// Botlets enrollment installs resolve the SAME home the running daemon
	// loaded profiles from (resolveDaemonBotletsHome).
	botletsHomeHint string
	etag            string
	pollFailures    int
	// enrollFailures / enrollNextAttempt throttle the per-enrollment RE-REDEEM
	// cadence, keyed by enrollment_id. Each re-redeem makes the server revoke the
	// prior credential and mint a fresh one, so a persistently-stuck enrollment
	// (verify 5xx, install failure, …) re-redeeming every 4s poll is the churn
	// behind #1321. On a retryable failure we record a per-enrollment exponential
	// backoff (base poll interval, doubling, capped) and SKIP re-redeeming that
	// enrollment until it elapses. The list poll itself stays at the base cadence
	// so a brand-new or healthy enrollment is never delayed by an unrelated stuck
	// one. Entries are cleared when the enrollment succeeds, drops off the list,
	// or the daemon token is revoked / unpaired. Distinct from pollFailures (the
	// LIST-call backoff), which list-success resets every poll.
	enrollFailures    map[string]int
	enrollNextAttempt map[string]time.Time
	// nowFn is the clock used for backoff gating; overridden in tests.
	nowFn             func() time.Time
	revokedToken      string
	loggedAuthBroken  bool
	loggedAuthRevoked bool
}

func newAgentEnrollmentWorker(paths commentbus.Paths) *agentEnrollmentWorker {
	return &agentEnrollmentWorker{
		paths:             paths,
		enrollFailures:    map[string]int{},
		enrollNextAttempt: map[string]time.Time{},
		nowFn:             time.Now,
	}
}

// resetEnrollRetry forgets all per-enrollment re-redeem backoff state (used when
// the daemon unpairs or its token is revoked — a fresh pairing starts clean).
func (w *agentEnrollmentWorker) resetEnrollRetry() {
	w.enrollFailures = map[string]int{}
	w.enrollNextAttempt = map[string]time.Time{}
}

// pruneEnrollRetry drops backoff state for enrollments no longer on the list
// (terminal / cancelled / drained), bounding the maps to live enrollments.
func (w *agentEnrollmentWorker) pruneEnrollRetry(seen map[string]bool) {
	for id := range w.enrollFailures {
		if !seen[id] {
			delete(w.enrollFailures, id)
			delete(w.enrollNextAttempt, id)
		}
	}
}

type agentEnrollmentListItem struct {
	EnrollmentID string `json:"enrollment_id"`
	State        string `json:"state"`
	AgentID      string `json:"agent_id"`
	Handle       string `json:"handle"`
	DisplayName  string `json:"display_name"`
	Runtime      string `json:"runtime"`
	ExpiresAt    string `json:"expires_at"`
	CredentialID string `json:"credential_id"`
}

type agentEnrollmentRedeemResponse struct {
	EnrollmentID string `json:"enrollment_id"`
	Agent        struct {
		AgentID     string `json:"agent_id"`
		Handle      string `json:"handle"`
		DisplayName string `json:"display_name"`
	} `json:"agent"`
	LocalCredential struct {
		CredentialID string `json:"credential_id"`
		AgentSecret  string `json:"agent_secret"`
		BaseURL      string `json:"base_url"`
		Runtime      string `json:"runtime"`
	} `json:"local_credential"`
	// Botlets is the optional `botlets` hint block: present (non-nil) only when
	// the enrolled agent is a Botlets bot. nil for a plain registered agent.
	Botlets *agentEnrollmentBotletsHint `json:"botlets,omitempty"`
}

// agentEnrollmentBotletsHint is the `botlets` block the redeem response carries
// when the enrolled agent is a Botlets bot. The server assembles it ENTIRELY
// from the agent's OWN botlets_bot metadata on its AgentDO (never from
// daemon-supplied input), and the fields mirror exactly what a team-manifest
// member carries, so the worker can run the SAME local brain/registry wiring
// registerBotletsBotLocally already performs for team bots. `relative_path` is
// computed locally by registerBotletsBotLocally from the synced brain
// projection, so the server omits it.
type agentEnrollmentBotletsHint struct {
	Runtime string `json:"runtime"`
	// ScheduleTimezone is the bot's configured schedule timezone, mirrored into
	// the local registry entry's managed-session setting so daily session
	// resets follow the bot's timezone (same as `botlets register --timezone`).
	// Optional: older servers omit it and the registry falls back to the
	// default timezone.
	ScheduleTimezone string `json:"schedule_timezone"`
	// RespondsToMentions is the bot's "Responds to @mentions" opt-in, mirrored
	// into the local registry entry so the daemon can auto-launch the runtime on
	// a doc @mention. Optional: a server that omits it (false) leaves the bot in
	// the default no-auto-launch state.
	RespondsToMentions bool `json:"responds_to_mentions"`
	Brain              struct {
		WorkspaceID     string `json:"workspace_id"`
		BotID           string `json:"bot_id"`
		BotAgentID      string `json:"bot_agent_id"`
		ContainerID     string `json:"container_id"`
		RootFolderID    string `json:"root_folder_id"`
		OwnerAgentID    string `json:"owner_agent_id"`
		SetupGeneration int    `json:"setup_generation"`
		RelativePath    string `json:"relative_path,omitempty"`
	} `json:"brain"`
}

// runOnce performs one worker pass and returns how long to wait before the
// next one.
func (w *agentEnrollmentWorker) runOnce(ctx context.Context) time.Duration {
	if ctx.Err() != nil {
		return agentEnrollmentPairingRecheckInterval
	}
	auth, paired, err := commentbus.LoadDaemonAuth(w.paths)
	if err != nil {
		if !w.loggedAuthBroken {
			w.loggedAuthBroken = true
			w.logWarn("agent_enrollment.daemon_auth_unreadable", map[string]any{"error": err.Error()})
		}
		return agentEnrollmentPairingRecheckInterval
	}
	if !paired {
		w.loggedAuthBroken = false
		// Unpaired: no-op cleanly, and forget any prior revoked-token latch and
		// per-enrollment backoff state so a future pairing starts fresh.
		w.revokedToken = ""
		w.loggedAuthRevoked = false
		w.etag = ""
		w.resetEnrollRetry()
		return agentEnrollmentPairingRecheckInterval
	}
	if strings.TrimSpace(auth.BaseURL) == "" {
		if !w.loggedAuthBroken {
			w.loggedAuthBroken = true
			w.logWarn("agent_enrollment.daemon_auth_missing_base_url", map[string]any{"daemon_id": auth.DaemonID})
		}
		return agentEnrollmentPairingRecheckInterval
	}
	w.loggedAuthBroken = false
	if w.revokedToken != "" && auth.Token == w.revokedToken {
		// The server told us this exact token is dead. Stop polling with it;
		// keep re-checking the pairing file so a re-pair resumes work.
		return agentEnrollmentPairingRecheckInterval
	}
	w.revokedToken = ""
	w.loggedAuthRevoked = false

	items, cleanups, status, listErr := w.listEnrollments(ctx, auth)
	switch {
	case listErr != nil:
		w.pollFailures++
		w.logWarn("agent_enrollment.poll_failed", map[string]any{
			"daemon_id": auth.DaemonID,
			"error":     listErr.Error(),
		})
		return w.backoffWait()
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// Daemon token revoked server-side: log once, park this token, and
		// fall back to the pairing-file recheck cadence.
		w.revokedToken = auth.Token
		w.etag = ""
		w.pollFailures = 0
		w.resetEnrollRetry()
		if !w.loggedAuthRevoked {
			w.loggedAuthRevoked = true
			w.logWarn("agent_enrollment.daemon_token_revoked", map[string]any{
				"daemon_id": auth.DaemonID,
				"status":    status,
			})
		}
		return agentEnrollmentPairingRecheckInterval
	case status == http.StatusNotModified:
		w.pollFailures = 0
		return agentEnrollmentPollInterval
	case status != http.StatusOK:
		w.pollFailures++
		w.logWarn("agent_enrollment.poll_failed", map[string]any{
			"daemon_id": auth.DaemonID,
			"status":    status,
		})
		return w.backoffWait()
	}
	w.pollFailures = 0
	// retryWanted latches when any item failed in a way that left the server
	// state (and therefore the list fingerprint enrollment_id:state:credential_id)
	// unchanged. The ETag would otherwise 304 the next poll forever and wedge the
	// retry the lifecycle promises, so we force a full unconditional re-fetch by
	// clearing the stored ETag at the end of the pass. Clearing is always safe:
	// the worst case is one extra full fetch that self-heals once the item
	// reaches a terminal state.
	retryWanted := false
	deferred := false
	now := w.nowFn()
	seen := make(map[string]bool, len(items)+len(cleanups))
	for _, item := range items {
		if ctx.Err() != nil {
			return agentEnrollmentPollInterval
		}
		// pending: normal path. redeemed: crash recovery — this daemon (or a
		// previous run of it) redeemed but never acked; re-redeem (the server
		// revokes the prior credential and mints a replacement) and continue
		// down the same install path.
		if item.State != "pending" && item.State != "redeemed" {
			continue
		}
		seen[item.EnrollmentID] = true
		// Skip re-redeeming an enrollment still inside its per-enrollment backoff
		// window: each re-redeem churns a server credential (#1321). The list poll
		// keeps its base cadence below, so this throttles ONLY the stuck item —
		// new/healthy enrollments are processed immediately.
		if next, backedOff := w.enrollNextAttempt[item.EnrollmentID]; backedOff && now.Before(next) {
			deferred = true
			continue
		}
		if w.processEnrollment(ctx, auth, item) {
			retryWanted = true
			w.enrollFailures[item.EnrollmentID]++
			// The first failure retries at the normal poll cadence — a one-off
			// transient blip shouldn't be slowed. Only a REPEATED failure arms the
			// per-enrollment backoff that throttles the re-redeem churn of #1321.
			if w.enrollFailures[item.EnrollmentID] > 1 {
				w.enrollNextAttempt[item.EnrollmentID] = now.Add(agentEnrollmentExpBackoff(w.enrollFailures[item.EnrollmentID]))
			}
		} else {
			// Settled (installed or terminal): clear its backoff so a future
			// re-enrollment under the same id starts at the fast cadence.
			delete(w.enrollFailures, item.EnrollmentID)
			delete(w.enrollNextAttempt, item.EnrollmentID)
		}
	}
	// `cleanups`: redeemed-then-terminal enrollments whose terminal answer this
	// daemon never heard (the cancel/sweep landed while it was stopped). Each
	// is reconciled with the same restore-or-remove cleanup the terminal-ack
	// path runs, then re-acked so the server stamps the confirmation that
	// drains the item. Cleanups don't re-redeem, so they are not backoff-gated.
	for _, item := range cleanups {
		if ctx.Err() != nil {
			return agentEnrollmentPollInterval
		}
		seen[item.EnrollmentID] = true
		if w.processCleanup(ctx, auth, item) {
			retryWanted = true
		}
	}
	w.pruneEnrollRetry(seen)
	if retryWanted || deferred {
		// Force a full re-fetch next poll: a retried item left the list
		// fingerprint (enrollment_id:state:credential_id) unchanged so it would
		// 304 forever, and a deferred item must reappear so we can re-redeem it
		// once its backoff elapses. Clearing is always safe — the worst case is
		// one extra full fetch that self-heals once the item reaches a terminal
		// state. The poll cadence itself stays fast so concurrent new/healthy
		// enrollments are never delayed by a stuck one.
		w.etag = ""
	}
	return agentEnrollmentPollInterval
}

// agentEnrollmentExpBackoff maps a consecutive-failure count to an exponential
// wait (base poll interval, doubling) capped at agentEnrollmentBackoffCap. A
// count of 0 or 1 returns the base interval, so the first failure is not slowed.
func agentEnrollmentExpBackoff(failures int) time.Duration {
	wait := agentEnrollmentPollInterval
	for i := 1; i < failures && wait < agentEnrollmentBackoffCap; i++ {
		wait *= 2
	}
	if wait > agentEnrollmentBackoffCap {
		return agentEnrollmentBackoffCap
	}
	return wait
}

func (w *agentEnrollmentWorker) backoffWait() time.Duration {
	return agentEnrollmentExpBackoff(w.pollFailures)
}

// listEnrollments performs the fast poll. A 304 (ETag unchanged) returns no
// items; a 200 refreshes the stored ETag. The second list is `cleanups`:
// redeemed-then-terminal enrollments the daemon must reconcile locally (the
// server covers them with the same ETag, so a new cleanup item breaks the 304
// fast path). Older servers omit the field; it decodes as empty.
func (w *agentEnrollmentWorker) listEnrollments(ctx context.Context, auth commentbus.DaemonAuth) ([]agentEnrollmentListItem, []agentEnrollmentListItem, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(auth.BaseURL, "/")+"/daemon/agent-enrollments", nil)
	if err != nil {
		return nil, nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("X-Comment-CLI-Version", version)
	req.Header.Set(runtimeAuthHeaderName, runtimeAuthHeaderValue())
	if w.etag != "" {
		req.Header.Set("If-None-Match", w.etag)
	}
	resp, err := agentEnrollmentHTTPClient.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusNotModified {
		return nil, nil, resp.StatusCode, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, nil, resp.StatusCode, nil
	}
	var body struct {
		Enrollments []agentEnrollmentListItem `json:"enrollments"`
		Cleanups    []agentEnrollmentListItem `json:"cleanups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, nil, resp.StatusCode, errors.New("enrollment list returned an unreadable response")
	}
	if etag := resp.Header.Get("ETag"); etag != "" {
		w.etag = etag
	}
	return body.Enrollments, body.Cleanups, resp.StatusCode, nil
}

// processEnrollment drives one enrollment through
// redeem -> profile write -> reload -> verify -> ack.
//
// It returns true when this pass failed WITHOUT producing a durable server-side
// state change — i.e. it neither reached a successful "installed" ack nor a
// successful non-retryable "failed" ack. In those cases the enrollment is still
// 'redeemed' with the same credential_id, so the list fingerprint is unchanged
// and the caller must clear the ETag to force a re-fetch (otherwise the next
// conditional poll 304s and the retry the lifecycle promises never happens).
// Returning false means a terminal/durable transition landed and the fingerprint
// will move on its own, so the ETag can be kept.
func (w *agentEnrollmentWorker) processEnrollment(ctx context.Context, auth commentbus.DaemonAuth, item agentEnrollmentListItem) bool {
	redeemed, ok := w.redeemEnrollment(ctx, auth, item)
	if !ok {
		// Redeem failed (transient error, unreadable/incomplete body, or a
		// rejection). Nothing was installed; retry on the next poll.
		return true
	}
	cred := redeemed.LocalCredential
	handle := redeemed.Agent.Handle
	baseURL := strings.TrimRight(cred.BaseURL, "/")
	if baseURL == "" {
		baseURL = strings.TrimRight(auth.BaseURL, "/")
	}

	// Install step. A Botlets-bot enrollment (the redeem response carried a
	// `botlets` hint) routes through the SAME local brain/registry wiring the
	// team-resync worker uses (registerBotletsBotLocally via the
	// botletsTeamRegisterLocally seam): that helper writes the profile, locates +
	// validates the brain projection (refreshing local sync once if missing),
	// upserts the registry entry with its brain ref, and reloads the daemon — so
	// for a Botlets bot we deliberately do NOT also run the generic profile write
	// (it would write the same profile a second time and skip the brain/registry
	// wiring). A plain registered agent takes the generic profile-write path.
	// Either branch, on a non-terminal install problem, acks the failure and
	// returns retryWanted via handled=true; on success it returns the cleanup
	// descriptor and we fall through to the shared verify + install ack.
	var cleanup enrollCleanup
	var handled, retryWanted bool
	if redeemed.Botlets != nil {
		cleanup, handled, retryWanted = w.installBotletsEnrollment(ctx, auth, item, redeemed, baseURL)
	} else {
		cleanup, handled, retryWanted = w.installGenericEnrollment(ctx, auth, item, redeemed, baseURL)
	}
	if handled {
		return retryWanted
	}

	verifyStatus, verifyErr := w.verifyAgentCredential(ctx, baseURL, cred.AgentSecret)
	switch {
	case verifyErr != nil || verifyStatus >= 500:
		// Server hiccup or network: retryable ack failure.
		data := map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"status":        verifyStatus,
		}
		if verifyErr != nil {
			data["error"] = verifyErr.Error()
		}
		w.logWarn("agent_enrollment.verify_unavailable", data)
		w.ackEnrollment(ctx, auth, item.EnrollmentID, cleanup, ackOutcome{
			State:          "failed",
			CredentialID:   cred.CredentialID,
			FailureCode:    "VERIFY_UNAVAILABLE",
			FailureMessage: fmt.Sprintf("GET /agents/me failed (status %d)", verifyStatus),
			Retryable:      true,
		})
		// Retryable: enrollment stays 'redeemed', fingerprint unchanged.
		return true
	case verifyStatus == http.StatusUnauthorized || verifyStatus == http.StatusForbidden:
		// The freshly minted credential is bad. Recover by re-redeem — never
		// retry the same token. No ack: the enrollment stays redeemed and the
		// next poll re-redeems (revoke-and-remint on the server).
		w.logWarn("agent_enrollment.verify_unauthorized", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"status":        verifyStatus,
		})
		// Intentionally NO ack: the enrollment stays 'redeemed' with the same
		// credential_id so the next poll re-redeems. That is exactly the case the
		// ETag would wedge — the fingerprint never moves — so force a re-fetch.
		return true
	case verifyStatus != http.StatusOK:
		w.logWarn("agent_enrollment.verify_unexpected_status", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"status":        verifyStatus,
		})
		w.ackEnrollment(ctx, auth, item.EnrollmentID, cleanup, ackOutcome{
			State:          "failed",
			CredentialID:   cred.CredentialID,
			FailureCode:    "VERIFY_FAILED",
			FailureMessage: fmt.Sprintf("GET /agents/me returned status %d", verifyStatus),
			Retryable:      true,
		})
		// Retryable: enrollment stays 'redeemed', fingerprint unchanged.
		return true
	}
	if w.ackEnrollment(ctx, auth, item.EnrollmentID, cleanup, ackOutcome{
		State:        "installed",
		CredentialID: cred.CredentialID,
	}) {
		w.logInfo("agent_enrollment.installed", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"agent_id":      redeemed.Agent.AgentID,
		})
		// Installed: the server flips to 'installed' and the fingerprint moves —
		// keep the ETag so the next poll can 304.
		return false
	}
	// The install ack did not land (non-200, or a cancelled/expired cleanup
	// path). If it was cancelled/expired the state already moved terminal and a
	// re-fetch is a harmless no-op; if it was a transient non-200 the enrollment
	// is still 'redeemed' and must be retried. Either way, force a re-fetch.
	return true
}

// installGenericEnrollment is the plain registered-agent install step: prepare,
// write, and reload the `<handle>.json` profile. It returns
// (cleanup, handled, retryWanted): handled=true means it already acked the
// outcome (failure) and the caller should return retryWanted without verifying;
// handled=false means the install succeeded and cleanup names the file the
// shared verify + install ack may DELETE if the enrollment turns out
// cancelled/expired/failed. The cleanup path is empty when a profile already
// existed for this handle before this pass: cleanup must only ever remove a
// file this enrollment created, never a pre-existing install that merely got
// overwritten (deleting it would take out a previously working agent because
// an unrelated enrollment died).
func (w *agentEnrollmentWorker) installGenericEnrollment(ctx context.Context, auth commentbus.DaemonAuth, item agentEnrollmentListItem, redeemed agentEnrollmentRedeemResponse, baseURL string) (enrollCleanup, bool, bool) {
	cred := redeemed.LocalCredential
	handle := redeemed.Agent.Handle
	write, err := commentbus.PrepareAgentProfileWrite(w.paths, handle, cred.AgentSecret, baseURL, cred.Runtime)
	if err != nil {
		// Validation failures (bad handle/runtime, untrusted agents dir) will
		// not heal by re-minting a credential: terminal, non-retryable.
		w.logWarn("agent_enrollment.profile_prepare_failed", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"error":         err.Error(),
		})
		// Non-retryable: a successful ack flips the enrollment to 'failed' and
		// the fingerprint moves on its own (keep the ETag). If the ack itself did
		// not land, the state is still 'redeemed' — force a re-fetch.
		acked := w.ackEnrollment(ctx, auth, item.EnrollmentID, enrollCleanup{}, ackOutcome{
			State:          "failed",
			CredentialID:   cred.CredentialID,
			FailureCode:    "PROFILE_INVALID",
			FailureMessage: err.Error(),
			Retryable:      false,
		})
		return enrollCleanup{}, true, !acked
	}
	if profileFileExists(write.Path) && !w.existingProfileIsEnrollmentOwned(item.EnrollmentID, write.Path) {
		if err := backupProfileForEnrollment(write.Path); err != nil {
			// Without a snapshot, a later closed-enrollment cleanup would have
			// to choose between deleting a working install and leaving a
			// revoked credential in place. Retryable: nothing written yet.
			w.logWarn("agent_enrollment.profile_backup_failed", map[string]any{
				"enrollment_id": item.EnrollmentID,
				"handle":        handle,
				"error":         err.Error(),
			})
			w.ackEnrollment(ctx, auth, item.EnrollmentID, enrollCleanup{}, ackOutcome{
				State:          "failed",
				CredentialID:   cred.CredentialID,
				FailureCode:    "PROFILE_WRITE_FAILED",
				FailureMessage: err.Error(),
				Retryable:      true,
			})
			return enrollCleanup{}, true, true
		}
	}
	// Journal the write BEFORE it happens: the entry is the attribution
	// evidence processCleanup and `comment bus unpair` need to know this
	// daemon's enrollment owns the file. Best-effort — a failed journal write
	// only degrades those paths to leave-the-file-alone, never breaks the
	// install.
	if err := enrollJournalRecord(w.paths, enrollJournalEntry{
		EnrollmentID: item.EnrollmentID,
		DaemonID:     auth.DaemonID,
		Handle:       handle,
		ProfilePath:  write.Path,
		SecretSHA256: enrollSecretSHA256(cred.AgentSecret),
	}); err != nil {
		w.logWarn("agent_enrollment.journal_write_failed", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"error":         err.Error(),
		})
	}
	if err := write.Write(); err != nil {
		// Filesystem trouble can be transient (disk full, races): retryable —
		// the enrollment stays redeemed and the next poll re-redeems.
		w.logWarn("agent_enrollment.profile_write_failed", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"error":         err.Error(),
		})
		// Pass the cleanup descriptor even though this is a RETRYABLE failure:
		// a concurrent cancel/expiry can make this ack receive a terminal
		// ENROLLMENT_* answer, and the inline terminal-cleanup must then
		// restore-or-remove the handle so the .enroll-backup sidecar this pass
		// created is consumed — otherwise a later enrollment for the same
		// handle inherits a stale backup and could restore the old profile.
		w.ackEnrollment(ctx, auth, item.EnrollmentID, enrollCleanup{profilePath: write.Path}, ackOutcome{
			State:          "failed",
			CredentialID:   cred.CredentialID,
			FailureCode:    "PROFILE_WRITE_FAILED",
			FailureMessage: err.Error(),
			Retryable:      true,
		})
		// Retryable failure: the enrollment stays 'redeemed' with the same
		// credential_id, so clear the ETag to keep retrying.
		return enrollCleanup{}, true, true
	}
	// Cleanup is restore-or-remove: a pre-existing install restores from its
	// backup; a file this enrollment created is removed.
	cleanup := enrollCleanup{profilePath: write.Path}
	if errText := agentEnrollmentReloadProfiles(ctx, w.paths, handle); errText != "" {
		// Profile written but the daemon has not picked it up: retryable
		// ("reload only" per the lifecycle) — credential kept, daemon retries.
		w.logWarn("agent_enrollment.reload_failed", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"error":         errText,
		})
		w.ackEnrollment(ctx, auth, item.EnrollmentID, cleanup, ackOutcome{
			State:          "failed",
			CredentialID:   cred.CredentialID,
			FailureCode:    "RELOAD_FAILED",
			FailureMessage: errText,
			Retryable:      true,
		})
		// Retryable: enrollment stays 'redeemed', fingerprint unchanged.
		return enrollCleanup{}, true, true
	}
	return cleanup, false, false
}

// profileFileExists reports whether an agent profile file is already on disk.
// Used to decide whether an enrollment's terminal cleanup may delete the file:
// only when THIS enrollment created it.
// enrollProfileBackupPath is the sidecar holding a PRE-EXISTING profile's
// bytes while an enrollment overwrites the file. The suffix deliberately
// does not match the daemon's agents/*.json glob. On a closed enrollment the
// backup is restored; on a successful install it is discarded.
func enrollProfileBackupPath(path string) string { return path + ".enroll-backup" }

// backupProfileForEnrollment snapshots a pre-existing profile so a
// cancelled/expired/failed enrollment can restore the working install it
// overwrote (the new credential gets revoked server-side on those outcomes).
//
// FIRST WRITER WINS: a retried pass for the same redeemed enrollment sees the
// ENROLLMENT-written profile on disk and would otherwise overwrite the backup
// with the enrollment's own (about-to-be-revoked) credential — cleanup would
// then "restore" a broken profile. An existing backup is therefore preserved;
// it always holds the oldest known-good install for the handle. Backups are
// consumed on restore and discarded on a successful installed-ack, so a
// surviving one is exactly the unresolved overwrite we must not lose.
func backupProfileForEnrollment(path string) error {
	backup := enrollProfileBackupPath(path)
	if _, err := os.Lstat(backup); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(backup, data, 0o600)
}

// restoreOrRemoveEnrollProfile undoes this enrollment's profile write: when a
// pre-existence backup exists the original install is restored, otherwise the
// file this enrollment created is removed. The first return reports which
// branch ran (restored=true when a backup was put back) so a Botlets cleanup
// can decide whether the registry entry must be rolled back as well.
func restoreOrRemoveEnrollProfile(path string) (bool, error) {
	backup := enrollProfileBackupPath(path)
	if _, err := os.Lstat(backup); err == nil {
		return true, os.Rename(backup, path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return false, nil
}

func profileFileExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// existingProfileIsEnrollmentOwned reports whether the profile already on disk
// at path was written by THIS enrollment on an earlier (un-acked) pass, per the
// enroll journal's secret attribution. It exists to keep a retried install from
// snapshotting its own about-to-be-revoked credential as a ".enroll-backup":
// the first pass writes the profile, hits the 401/403 verify path and does not
// ack, and the re-redeem then sees that enrollment-owned file on disk. Without
// this check it would be backed up and a later cancelled/expired/failed cleanup
// would "restore" the dead credential, leaving a revoked profile that blocks
// future auto-installs.
//
// Only a DEFINITIVE match counts. When the journal is missing/unreadable, has
// no entry for a different path, or attribution is indeterminate (the file
// could not be read), it returns false so the caller falls back to the SAFE
// default of backing the file up. That asymmetry is deliberate: wrongly backing
// up a real user install only strands a recoverable revoked profile on restore,
// whereas wrongly skipping the backup would let cleanup DELETE a working
// pre-existing install — so we skip the backup only with positive evidence the
// file is our own write.
func (w *agentEnrollmentWorker) existingProfileIsEnrollmentOwned(enrollmentID string, path string) bool {
	entry, journaled, indeterminate := enrollJournalLookup(w.paths, enrollmentID)
	if !journaled || indeterminate || entry.ProfilePath != path {
		return false
	}
	matches, _, matchIndeterminate := enrollJournalProfileSecretMatches(entry)
	return matches && !matchIndeterminate
}

// installBotletsEnrollment is the Botlets-bot install step. It maps the redeem
// response's `botlets` hint into a botletsRegisterInput and runs the SAME local
// wiring the team-resync worker performs (registerBotletsBotLocally via the
// botletsTeamRegisterLocally seam): profile write, brain projection lookup
// (registerBotletsBotLocally itself runs `comment sync once` and retries when
// the projection is not yet present — ErrBotletsBrainProjectionNotFound), bot
// registry upsert with the brain ref, and daemon reload. It returns the same
// (profilePath, handled, retryWanted) contract as installGenericEnrollment.
//
// Any wiring failure AFTER the credential was minted is acked as a
// Botlets-specific RETRYABLE failure (BOTLETS_WIRING_FAILED): the enrollment
// stays 'redeemed', the credential is kept, and the worker retries on the next
// poll — consistent with the retryable-ack lifecycle and the plan's Botlets
// rule.
//
// Like installGenericEnrollment, the returned cleanup names what this
// enrollment created: the profile file (empty when a profile already existed
// for the handle, which terminal cleanup must never delete) plus the Botlets
// handle/home so a closed enrollment's cleanup can also roll back the registry
// entry the wiring upserted.
func (w *agentEnrollmentWorker) installBotletsEnrollment(ctx context.Context, auth commentbus.DaemonAuth, item agentEnrollmentListItem, redeemed agentEnrollmentRedeemResponse, baseURL string) (enrollCleanup, bool, bool) {
	cred := redeemed.LocalCredential
	hint := redeemed.Botlets
	handle := redeemed.Agent.Handle
	botletsHome, err := resolveDaemonBotletsHome(w.paths, w.botletsHomeHint)
	if err != nil {
		// A bad/untrusted Botlets home is environmental, not credential-fatal:
		// retryable so a corrected config heals without losing the credential.
		w.logWarn("agent_enrollment.botlets_home_invalid", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"error":         err.Error(),
		})
		w.ackEnrollment(ctx, auth, item.EnrollmentID, enrollCleanup{}, ackOutcome{
			State:          "failed",
			CredentialID:   cred.CredentialID,
			FailureCode:    "BOTLETS_WIRING_FAILED",
			FailureMessage: err.Error(),
			Retryable:      true,
		})
		return enrollCleanup{}, true, true
	}
	// One-approval trust: a Botlets install needs local library sync (the
	// brain projection). Self-provision the sync credential over the pairing
	// token instead of failing until a human runs `comment sync login` —
	// the pairing approval already covered this machine.
	if err := ensureSyncConfiguredViaDaemonSeam(ctx, w.paths, auth); err != nil {
		w.logWarn("agent_enrollment.sync_provision_failed", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"error":         err.Error(),
		})
		w.ackEnrollment(ctx, auth, item.EnrollmentID, enrollCleanup{}, ackOutcome{
			State:          "failed",
			CredentialID:   cred.CredentialID,
			FailureCode:    "SYNC_PROVISION_FAILED",
			FailureMessage: err.Error(),
			Retryable:      true,
		})
		return enrollCleanup{}, true, true
	}
	runtime := strings.TrimSpace(hint.Runtime)
	if runtime == "" {
		// The hint always carries the runtime, but the local credential mirrors
		// it; fall back rather than reject a usable install.
		runtime = strings.TrimSpace(cred.Runtime)
	}
	setupGeneration := hint.Brain.SetupGeneration
	if setupGeneration < 1 {
		setupGeneration = 1
	}
	// Track whether a profile for this handle already existed BEFORE the
	// register call: terminal cleanup may only delete a file this enrollment
	// created. The path mirrors the PrepareAgentProfileWrite target
	// registerBotletsBotLocally writes through.
	expectedProfilePath := agentProfileFilePath(w.paths, handle)
	// A file this enrollment wrote on an earlier un-acked pass is NOT a
	// pre-existing user install: treat it as enrollment-created so cleanup
	// REMOVES it rather than backing it up and "restoring" a dead credential.
	profilePreexisted := profileFileExists(expectedProfilePath) &&
		!w.existingProfileIsEnrollmentOwned(item.EnrollmentID, expectedProfilePath)
	// Snapshot the pre-existing registry entry alongside the profile backup so a
	// terminal-cleanup RESTORE rolls the entry back too (not just the profile).
	// Captured BEFORE registerBotletsBotLocally upserts the enrollment's brain/
	// runtime/timezone. Best-effort: a read failure only degrades the restore to
	// "profile restored, entry left as the enrollment wrote it" (the prior
	// behavior), never blocks the install.
	var prevRegistrySnapshot json.RawMessage
	if profilePreexisted {
		if prev, err := readBotletsRegistryEntryForHandle(botletsHome, handle); err != nil {
			w.logWarn("agent_enrollment.registry_snapshot_failed", map[string]any{
				"enrollment_id": item.EnrollmentID,
				"handle":        handle,
				"error":         err.Error(),
			})
		} else if prev != nil {
			if raw, err := json.Marshal(prev); err == nil {
				prevRegistrySnapshot = raw
			}
		}
		if err := backupProfileForEnrollment(expectedProfilePath); err != nil {
			// Same rationale as the generic path: without a snapshot, terminal
			// cleanup could only delete a working install or leave a revoked
			// credential behind. Retryable: nothing rewritten yet.
			w.logWarn("agent_enrollment.profile_backup_failed", map[string]any{
				"enrollment_id": item.EnrollmentID,
				"handle":        handle,
				"error":         err.Error(),
			})
			w.ackEnrollment(ctx, auth, item.EnrollmentID, enrollCleanup{}, ackOutcome{
				State:          "failed",
				CredentialID:   cred.CredentialID,
				FailureCode:    "BOTLETS_WIRING_FAILED",
				FailureMessage: err.Error(),
				Retryable:      true,
			})
			return enrollCleanup{}, true, true
		}
	}
	// Journal the write BEFORE registerBotletsBotLocally rewrites the profile:
	// the attribution evidence processCleanup and `comment bus unpair` need.
	// Best-effort, same rationale as the generic path.
	if err := enrollJournalRecord(w.paths, enrollJournalEntry{
		EnrollmentID:        item.EnrollmentID,
		DaemonID:            auth.DaemonID,
		Handle:              handle,
		ProfilePath:         expectedProfilePath,
		SecretSHA256:        enrollSecretSHA256(cred.AgentSecret),
		BotletsHandle:       handle,
		BotletsHome:         botletsHome,
		PrevBotletsRegistry: prevRegistrySnapshot,
	}); err != nil {
		w.logWarn("agent_enrollment.journal_write_failed", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"error":         err.Error(),
		})
	}
	reg, err := botletsTeamRegisterLocally(ctx, botletsRegisterInput{
		Paths:              w.paths,
		BotletsHome:        botletsHome,
		BaseURL:            baseURL,
		BotHandle:          handle,
		AgentSecret:        cred.AgentSecret,
		BotSlug:            botletsBotSlugFromHandle(handle),
		BotDisplayName:     redeemed.Agent.DisplayName,
		BotID:              hint.Brain.BotID,
		OwnerAgentID:       hint.Brain.OwnerAgentID,
		BotAgentID:         hint.Brain.BotAgentID,
		WorkspaceID:        hint.Brain.WorkspaceID,
		ContainerID:        hint.Brain.ContainerID,
		RootFolderID:       hint.Brain.RootFolderID,
		SetupGeneration:    setupGeneration,
		ScheduleTimezone:   strings.TrimSpace(hint.ScheduleTimezone),
		RespondsToMentions: hint.RespondsToMentions,
		Runtime:            runtime,
	})
	if err != nil {
		// Local Botlets wiring failed after the credential was minted (brain
		// projection still unavailable even after registerBotletsBotLocally's own
		// sync-once retry, registry write trouble, etc.). RETRYABLE: keep the
		// credential, stay redeemed, retry on the next poll.
		//
		// The failure may have happened AFTER the registry/profile write (e.g.
		// the later daemon-orientation step), so report the install this
		// enrollment created: if the enrollment is then cancelled/expired/failed
		// before a retry succeeds, the ack's terminal cleanup can still remove
		// the revoked-credential profile AND the registry entry that points at
		// it instead of leaving them for later daemon starts to load.
		// restore-or-remove: a pre-existing install restores from its backup.
		cleanup := enrollCleanup{}
		if profileFileExists(expectedProfilePath) || profilePreexisted {
			cleanup = enrollCleanup{profilePath: expectedProfilePath, botletsHandle: handle, botletsHome: botletsHome}
		}
		w.logWarn("agent_enrollment.botlets_wiring_failed", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"error":         err.Error(),
		})
		w.ackEnrollment(ctx, auth, item.EnrollmentID, cleanup, ackOutcome{
			State:          "failed",
			CredentialID:   cred.CredentialID,
			FailureCode:    "BOTLETS_WIRING_FAILED",
			FailureMessage: err.Error(),
			Retryable:      true,
		})
		return enrollCleanup{}, true, true
	}
	// Cleanup is restore-or-remove: a pre-existing install restores from its
	// backup (keeping its still-valid registry entry); an install this
	// enrollment created is removed, registry entry included.
	cleanup := enrollCleanup{profilePath: reg.ProfilePath, botletsHandle: handle, botletsHome: botletsHome}
	if errText := strings.TrimSpace(reg.ReloadError); errText != "" {
		// Profile + registry written but the daemon has not reloaded: retryable
		// ("reload only" per the lifecycle), same as the generic path. Credential
		// kept; cleanup names what was written for any cancelled/expired/failed
		// cleanup on the ack.
		w.logWarn("agent_enrollment.botlets_reload_failed", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        handle,
			"error":         errText,
		})
		w.ackEnrollment(ctx, auth, item.EnrollmentID, cleanup, ackOutcome{
			State:          "failed",
			CredentialID:   cred.CredentialID,
			FailureCode:    "RELOAD_FAILED",
			FailureMessage: errText,
			Retryable:      true,
		})
		return enrollCleanup{}, true, true
	}
	return cleanup, false, false
}

// agentProfileFilePath is the canonical on-disk location of a generic agent
// profile — the exact file commentbus.PrepareAgentProfileWrite targets.
func agentProfileFilePath(paths commentbus.Paths, handle string) string {
	return filepath.Join(paths.Home, "agents", handle+".json")
}

// botletsBotSlugFromHandle derives the local bot slug (the registry entry Name,
// e.g. what `comment run <slug>` uses) from a Botlets bot handle of the form
// owner.slug. The enrollment hint carries the brain reference but not the slug,
// which is always the handle's trailing segment.
func botletsBotSlugFromHandle(handle string) string {
	handle = strings.TrimSpace(handle)
	if idx := strings.LastIndex(handle, "."); idx >= 0 && idx+1 < len(handle) {
		return handle[idx+1:]
	}
	return handle
}

// redeemEnrollment redeems (or re-redeems, for crash recovery) one enrollment.
// Terminal answers (cancelled/expired/installed/failed) and transient errors
// both return ok=false; transient errors recover on the next poll.
func (w *agentEnrollmentWorker) redeemEnrollment(ctx context.Context, auth commentbus.DaemonAuth, item agentEnrollmentListItem) (agentEnrollmentRedeemResponse, bool) {
	url := strings.TrimRight(auth.BaseURL, "/") + "/daemon/agent-enrollments/" + item.EnrollmentID + "/redeem"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return agentEnrollmentRedeemResponse{}, false
	}
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("X-Comment-CLI-Version", version)
	resp, err := agentEnrollmentHTTPClient.Do(req)
	if err != nil {
		w.logWarn("agent_enrollment.redeem_failed", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"error":         err.Error(),
		})
		return agentEnrollmentRedeemResponse{}, false
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		code := decodeErrorCode(resp.Body)
		w.logWarn("agent_enrollment.redeem_rejected", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"status":        resp.StatusCode,
			"code":          code,
		})
		return agentEnrollmentRedeemResponse{}, false
	}
	var redeemed agentEnrollmentRedeemResponse
	if err := json.NewDecoder(resp.Body).Decode(&redeemed); err != nil {
		w.logWarn("agent_enrollment.redeem_unreadable", map[string]any{"enrollment_id": item.EnrollmentID})
		return agentEnrollmentRedeemResponse{}, false
	}
	if redeemed.Agent.Handle == "" || redeemed.LocalCredential.AgentSecret == "" || redeemed.LocalCredential.CredentialID == "" {
		w.logWarn("agent_enrollment.redeem_incomplete", map[string]any{"enrollment_id": item.EnrollmentID})
		return agentEnrollmentRedeemResponse{}, false
	}
	return redeemed, true
}

type ackOutcome struct {
	State          string
	CredentialID   string
	FailureCode    string
	FailureMessage string
	Retryable      bool
	// CleanupDone marks a confirm re-ack: the local restore-or-remove cleanup
	// for a terminal enrollment ALREADY ran. The server stamps the cleanup
	// confirmation (draining the item from the poll's `cleanups` list) only
	// on this explicit signal — a plain terminal answer keeps the item listed
	// so a failed local cleanup retries.
	CleanupDone bool
}

// enrollCleanup describes what a closed (cancelled/expired/failed) enrollment's
// terminal cleanup must undo. profilePath is the profile file this enrollment
// wrote ("" when nothing was written, or when a pre-existing file must be
// preserved and no backup exists). botletsHandle/botletsHome are set for a
// Botlets enrollment whose install also upserted a registry entry: when the
// cleanup REMOVES the profile (no pre-existence backup — this enrollment
// created the install), the registry entry it wrote must go too, or
// LoadBotletsRegistry reports MISSING_CREDENTIAL_PROFILE forever. When the
// cleanup RESTORES a backup instead, the pre-existing install's registry entry
// is still valid and is kept.
type enrollCleanup struct {
	profilePath   string
	botletsHandle string
	botletsHome   string
}

// ackEnrollment reports the install outcome. If the server answers
// ENROLLMENT_CANCELLED, ENROLLMENT_EXPIRED, or ENROLLMENT_FAILED the
// credential is already revoked server-side, so the install this enrollment
// CREATED (cleanup.profilePath, plus the Botlets registry entry when
// cleanup.botletsHandle is set; profilePath is "" when nothing was written or
// when the file pre-existed this enrollment and must be preserved) is undone
// and the daemon reloaded. Returns true when the ack was accepted.
func (w *agentEnrollmentWorker) ackEnrollment(ctx context.Context, auth commentbus.DaemonAuth, enrollmentID string, cleanup enrollCleanup, outcome ackOutcome) bool {
	status, code, err := w.postEnrollmentAck(ctx, auth, enrollmentID, outcome)
	if err != nil {
		w.logWarn("agent_enrollment.ack_failed", map[string]any{
			"enrollment_id": enrollmentID,
			"error":         err.Error(),
		})
		return false
	}
	if status == http.StatusOK {
		if outcome.State == "installed" && cleanup.profilePath != "" {
			// The install stuck: the pre-existence backup (if any) is obsolete.
			_ = os.Remove(enrollProfileBackupPath(cleanup.profilePath))
		}
		return true
	}
	if isEnrollmentClosedCode(code) {
		// The enrollment died while we worked: cancelled in the browser, expired,
		// or already marked failed by the owner DO's sweep (which times out a
		// redeemed-unacked enrollment after ~15 minutes AND revokes its
		// credential — exactly what a daemon resuming after a long pause sees).
		// The credential is already revoked server-side; a dead profile left on
		// disk would confuse every later daemon start. Delete exactly what this
		// pass wrote and reload, then CONFIRM the cleanup with a cleanup_done
		// re-ack — the terminal answer alone no longer drains the enrollment
		// from the poll's `cleanups` list, so a local cleanup failure (or a
		// lost confirm) leaves the item listed and processCleanup retries it.
		w.logWarn("agent_enrollment.enrollment_closed", map[string]any{
			"enrollment_id": enrollmentID,
			"code":          code,
		})
		if w.cleanupClosedEnrollment(ctx, enrollmentID, cleanup) {
			if w.confirmEnrollmentCleanup(ctx, auth, enrollmentID, outcome.CredentialID) {
				enrollJournalRemove(w.paths, enrollmentID)
			}
		}
		return false
	}
	w.logWarn("agent_enrollment.ack_rejected", map[string]any{
		"enrollment_id": enrollmentID,
		"status":        status,
		"code":          code,
	})
	return false
}

// postEnrollmentAck POSTs one enrollment ack and returns the HTTP status plus
// the error `code` of a non-200 answer ("" on 200). A transport-level failure
// returns err. Shared by ackEnrollment (the install/failure outcome report)
// and processCleanup (the reconciling re-ack for a terminal enrollment).
func (w *agentEnrollmentWorker) postEnrollmentAck(ctx context.Context, auth commentbus.DaemonAuth, enrollmentID string, outcome ackOutcome) (int, string, error) {
	payload := map[string]any{
		"state":         outcome.State,
		"credential_id": outcome.CredentialID,
	}
	if outcome.State == "failed" {
		payload["failure_code"] = outcome.FailureCode
		payload["failure_message"] = outcome.FailureMessage
		payload["retryable"] = outcome.Retryable
	}
	if outcome.CleanupDone {
		payload["cleanup_done"] = true
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", err
	}
	url := strings.TrimRight(auth.BaseURL, "/") + "/daemon/agent-enrollments/" + enrollmentID + "/ack"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("X-Comment-CLI-Version", version)
	resp, err := agentEnrollmentHTTPClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, "", nil
	}
	return resp.StatusCode, decodeErrorCode(resp.Body), nil
}

// isEnrollmentClosedCode reports whether an ack answer says the enrollment is
// terminally closed (credential already revoked server-side) — the answers the
// local restore-or-remove cleanup contract runs on.
func isEnrollmentClosedCode(code string) bool {
	return code == "ENROLLMENT_CANCELLED" || code == "ENROLLMENT_EXPIRED" || code == "ENROLLMENT_FAILED"
}

// confirmEnrollmentCleanup tells the server the local cleanup for a terminal
// enrollment actually ran: a cleanup_done re-ack. The enrollment is terminal,
// so the answer is its ENROLLMENT_* code (never a 200) — hearing it back means
// the server stamped the confirmation that drains the item from the poll's
// `cleanups` list. Shared by ackEnrollment's terminal-answer branch and
// processCleanup. Returns true when the confirmation landed.
func (w *agentEnrollmentWorker) confirmEnrollmentCleanup(ctx context.Context, auth commentbus.DaemonAuth, enrollmentID string, credentialID string) bool {
	status, code, err := w.postEnrollmentAck(ctx, auth, enrollmentID, ackOutcome{
		State:          "failed",
		CredentialID:   credentialID,
		FailureCode:    "CLEANUP",
		FailureMessage: "daemon reconciled a terminal enrollment's local install",
		Retryable:      false,
		CleanupDone:    true,
	})
	if err != nil {
		w.logWarn("agent_enrollment.cleanup_ack_failed", map[string]any{
			"enrollment_id": enrollmentID,
			"error":         err.Error(),
		})
		return false
	}
	if isEnrollmentClosedCode(code) {
		return true
	}
	w.logWarn("agent_enrollment.cleanup_ack_rejected", map[string]any{
		"enrollment_id": enrollmentID,
		"status":        status,
		"code":          code,
	})
	return false
}

// cleanupClosedEnrollment undoes the install a closed (cancelled/expired/
// failed) enrollment left behind: restore-or-remove the profile, roll back the
// Botlets registry entry when the profile was REMOVED (not restored), and
// reload the daemon. Shared by ackEnrollment's terminal-answer branch and the
// poll's `cleanups` reconciliation (processCleanup). A profilePath with
// nothing on disk and no backup is a clean no-op. Returns false when the
// profile restore-or-remove itself failed; the registry and reload steps stay
// best-effort (logged, never fatal), exactly as before the extraction.
func (w *agentEnrollmentWorker) cleanupClosedEnrollment(ctx context.Context, enrollmentID string, cleanup enrollCleanup) bool {
	if cleanup.profilePath == "" {
		return true
	}
	restored, err := restoreOrRemoveEnrollProfile(cleanup.profilePath)
	if err != nil {
		w.logWarn("agent_enrollment.profile_cleanup_failed", map[string]any{
			"enrollment_id": enrollmentID,
			"error":         err.Error(),
		})
		return false
	}
	if !restored && cleanup.botletsHandle != "" {
		// This enrollment created the install (no pre-existence backup),
		// so the Botlets registry entry it upserted alongside the profile
		// must go too — a surviving entry would point at the credential
		// profile just removed and load as MISSING_CREDENTIAL_PROFILE on
		// every later daemon start.
		if err := removeBotletsRegistryEntryForHandle(cleanup.botletsHome, cleanup.botletsHandle); err != nil {
			w.logWarn("agent_enrollment.registry_cleanup_failed", map[string]any{
				"enrollment_id": enrollmentID,
				"handle":        cleanup.botletsHandle,
				"error":         err.Error(),
			})
			// The profile is gone but its Botlets registry entry survives,
			// still pointing at the now-missing credential profile (it would
			// load as MISSING_CREDENTIAL_PROFILE forever). Report the cleanup
			// INCOMPLETE so the caller does NOT send cleanup_done / prune the
			// journal / let the server drain the item — the cleanups-list retry
			// re-enters with the profile already removed (restore-or-remove is
			// then a no-op) and re-attempts the registry removal until it lands.
			return false
		}
	} else if restored && cleanup.botletsHandle != "" {
		// The pre-existing install's PROFILE is whole again, but
		// registerBotletsBotLocally already upserted this handle's registry
		// entry with the (now-revoked) enrollment's brain/runtime/timezone.
		// Roll the entry back to the snapshot captured at backup time so the
		// restored credential runs with its ORIGINAL registry state. The
		// snapshot lives in the enroll journal (cross-process safe: the
		// `cleanups` reconciliation in another daemon process reads the same
		// entry). A missing snapshot (older journal, or a handle the enrollment
		// created fresh — those go through the !restored branch) leaves the
		// current entry untouched, the prior behavior.
		//
		// Best-effort, NOT a fail-and-retry like the removal branch: the profile
		// restore already CONSUMED the .enroll-backup (renamed it into place), so
		// returning false here would re-enter cleanup with no backup left —
		// restore-or-remove would then DELETE the just-restored install. A rare
		// registry-write failure therefore degrades to "profile restored, entry
		// left as the enrollment wrote it" (exactly the pre-fix behavior) rather
		// than risk removing a working install.
		if entry, journaled, indeterminate := enrollJournalLookup(w.paths, enrollmentID); journaled && !indeterminate && len(entry.PrevBotletsRegistry) > 0 {
			if err := restorePreexistingBotletsRegistryEntry(w.paths, cleanup.botletsHome, cleanup.botletsHandle, entry.PrevBotletsRegistry); err != nil {
				w.logWarn("agent_enrollment.registry_restore_failed", map[string]any{
					"enrollment_id": enrollmentID,
					"handle":        cleanup.botletsHandle,
					"error":         err.Error(),
				})
			}
		}
	}
	// The daemon's reload-profiles op re-reads the agent profiles AND the
	// Botlets registry (it falls back to the daemon's own botlets home),
	// so one reload covers both cleanups.
	if errText := agentEnrollmentReloadProfiles(ctx, w.paths, ""); errText != "" {
		w.logWarn("agent_enrollment.cleanup_reload_failed", map[string]any{
			"enrollment_id": enrollmentID,
			"error":         errText,
		})
	}
	return true
}

// processCleanup reconciles one redeemed-then-terminal enrollment surfaced in
// the poll's `cleanups` list: the cancel or ack-timeout sweep landed while
// this daemon was stopped, so the terminal ack answer that normally triggers
// the local cleanup was never heard, and the profile written after redeem
// still carries the revoked credential (or shadows a pre-existing install's
// unrestored backup). Run the SAME local cleanup the terminal-answer path
// runs, then confirm with a cleanup_done re-ack: the server answers the
// terminal code and stamps the confirmation, draining the item from the list.
//
// ATTRIBUTION: the server only proves the enrollment was redeemed and went
// terminal — not that this daemon ever wrote the profile. The local enroll
// journal (recorded just before each install's profile write) is the evidence:
// without an entry, or when the file on disk holds a credential this
// enrollment did not write (the daemon crashed before writing and the handle
// was since installed manually or by another enrollment), the local profile is
// left alone and the item is merely confirmed so it drains.
//
// Returns true when reconciliation did NOT land (local cleanup failed, ack
// unreachable, or an unexpected answer): the item then stays in the list with
// an unchanged fingerprint, so the caller must clear the ETag or the next
// conditional poll would 304 and the retry would never run.
func (w *agentEnrollmentWorker) processCleanup(ctx context.Context, auth commentbus.DaemonAuth, item agentEnrollmentListItem) bool {
	entry, journaled, journalIndeterminate := enrollJournalLookup(w.paths, item.EnrollmentID)
	if journalIndeterminate {
		// The journal file exists but is unreadable/malformed, so we cannot tell
		// whether it attributes this terminal enrollment's profile. Confirming
		// now would drain the item and potentially abandon a revoked profile the
		// journal would have identified. Retry later (keep the item; clear ETag).
		w.logWarn("agent_enrollment.cleanup_journal_indeterminate", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        item.Handle,
		})
		return true
	}
	attributed := false
	if journaled {
		owns, indeterminate := enrollJournalEntryOwnsProfile(entry)
		if indeterminate {
			// The journaled profile exists but could not be read to attribute it
			// (permissions/transient). Confirming the cleanup now would drain the
			// item and abandon what may be THIS daemon's revoked profile. Treat it
			// as "cannot determine — retry later": leave the item in the list with
			// an unchanged fingerprint (return true so the caller clears the ETag).
			w.logWarn("agent_enrollment.cleanup_attribution_indeterminate", map[string]any{
				"enrollment_id": item.EnrollmentID,
				"handle":        item.Handle,
				"profile_path":  entry.ProfilePath,
			})
			return true
		}
		attributed = owns
	}
	if attributed {
		cleanup := enrollCleanup{
			profilePath:   entry.ProfilePath,
			botletsHandle: entry.BotletsHandle,
			botletsHome:   entry.BotletsHome,
		}
		if !w.cleanupClosedEnrollment(ctx, item.EnrollmentID, cleanup) {
			return true
		}
	} else {
		// No evidence this daemon's enrollment owns the file (no journal entry,
		// or the file now holds someone else's credential). Touch nothing —
		// deleting it could take out a working manual or later-enrollment
		// install — and just confirm so the item drains. An orphaned
		// .enroll-backup sidecar (if any) is deliberately left for the install
		// that now owns the handle to resolve.
		w.logInfo("agent_enrollment.cleanup_unattributed", map[string]any{
			"enrollment_id": item.EnrollmentID,
			"handle":        item.Handle,
			"journaled":     journaled,
		})
	}
	if !w.confirmEnrollmentCleanup(ctx, auth, item.EnrollmentID, item.CredentialID) {
		return true
	}
	// Confirmed server-side: the journal entry has served its purpose (and a
	// stale unattributed entry would never match anything again).
	if journaled {
		enrollJournalRemove(w.paths, item.EnrollmentID)
	}
	w.logInfo("agent_enrollment.cleanup_reconciled", map[string]any{
		"enrollment_id": item.EnrollmentID,
		"handle":        item.Handle,
		"state":         item.State,
		"attributed":    attributed,
	})
	return false
}

// verifyAgentCredential checks the freshly installed credential end to end
// with a plain GET /agents/me. Deliberately NOT ?setup_connect=1: that is a
// browser setup-page side channel that force-touches last_seen_at and would
// falsely advance the setup checklist's "Listening" state.
func (w *agentEnrollmentWorker) verifyAgentCredential(ctx context.Context, baseURL string, agentSecret string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/agents/me", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+agentSecret)
	req.Header.Set("X-Comment-CLI-Version", version)
	resp, err := agentEnrollmentHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer drainAndClose(resp)
	return resp.StatusCode, nil
}

// reloadAgentProfilesViaSocket is the production reload: the same daemon
// socket op `comment bus reload-profiles` uses for generic agent profiles
// (no botlets_home — this is not the Botlets path). When handle is non-empty,
// per-profile reload errors for that handle fail the reload; unrelated broken
// profiles must not block an otherwise good install.
func reloadAgentProfilesViaSocket(ctx context.Context, paths commentbus.Paths, handle string) string {
	auth, err := ownerOnlyAuth(paths, "")
	if err != nil {
		return err.Error()
	}
	response, err := callSocket(ctx, paths, "reload-profiles", auth, map[string]any{}, 10*time.Second)
	if err != nil {
		return err.Error()
	}
	if !response.OK {
		if err := socketResponseError(response); err != nil {
			return err.Error()
		}
	}
	if handle == "" {
		return ""
	}
	var reload commentbus.ProfileReloadResult
	data, err := json.Marshal(response.Result)
	if err != nil {
		return "daemon profile reload returned an unreadable result"
	}
	if err := json.Unmarshal(data, &reload); err != nil {
		return "daemon profile reload returned an unreadable result"
	}
	for _, loadErr := range reload.Errors {
		if loadErr.Profile == handle {
			return fmt.Sprintf("profile %s failed to load: %s", handle, loadErr.Message)
		}
	}
	return ""
}

func decodeErrorCode(body io.Reader) string {
	var payload struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(body, 1<<16)).Decode(&payload)
	return payload.Code
}

func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

func (w *agentEnrollmentWorker) logInfo(msg string, data map[string]any) {
	writeDaemonWorkerLog(w.paths, "agent.enrollment", "info", msg, data)
}

func (w *agentEnrollmentWorker) logWarn(msg string, data map[string]any) {
	writeDaemonWorkerLog(w.paths, "agent.enrollment", "warn", msg, data)
}
