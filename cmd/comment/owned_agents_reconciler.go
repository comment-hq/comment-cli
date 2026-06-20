package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// Owned-agents reconciler (Phase 8b).
//
// The account-wide auto-install loop: every pass fetches the server-side
// manifest of agents this PAIRED DAEMON should have installed
// (GET /daemon/owned-agents — the server applies the eligibility rules:
// generic agents on every auto-install daemon, individual Botlets bots on
// exactly one, team-workspace bots excluded), diffs it against the locally
// installed agent profiles, and SELF-CREATES an enrollment
// (POST /daemon/agent-enrollments) for each missing agent. The reconciler
// never redeems or installs anything itself: the existing 4s enrollment
// worker picks the self-created enrollments up and runs the whole proven
// redeem -> install -> verify -> ack lifecycle, so status records, browser
// visibility, sweeps, and revocation are reused unchanged.
//
// The server's fingerprint is the fast path: an unchanged manifest answers
// `unchanged` and the pass ends without the local diff. The in-memory
// fingerprint is updated only after a FULLY successful pass — any failure
// clears it so the next pass re-fetches and retries (the same lesson as the
// enrollment worker's ETag-clear: a kept fingerprint after a partial failure
// would skip the retry forever).
//
// Like the enrollment worker, the reconciler no-ops cleanly while unpaired
// and parks a token the server reported revoked until the pairing file
// changes.
const (
	// ownedAgentsReconcileInterval is deliberately slow: this loop is a
	// convergence sweep, not a human-facing spinner — the human-visible install
	// latency lives in the 4s enrollment worker that redeems what this
	// reconciler enrolls.
	ownedAgentsReconcileInterval = 60 * time.Second
	// ownedAgentsReconcileInitialDelay gives the daemon's startup (socket,
	// profile load, pairing) a moment to settle before the first pass.
	ownedAgentsReconcileInitialDelay = 10 * time.Second
	ownedAgentsRequestTimeout        = 30 * time.Second
)

var ownedAgentsHTTPClient = &http.Client{Timeout: ownedAgentsRequestTimeout}

func startOwnedAgentsReconciler(ctx context.Context, paths commentbus.Paths, botletsHomeHint string) {
	go runOwnedAgentsReconciler(ctx, paths, botletsHomeHint)
}

func runOwnedAgentsReconciler(ctx context.Context, paths commentbus.Paths, botletsHomeHint string) {
	worker := newOwnedAgentsReconciler(paths)
	worker.botletsHomeHint = botletsHomeHint
	timer := time.NewTimer(ownedAgentsReconcileInitialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			worker.runOnce(ctx)
			timer.Reset(ownedAgentsReconcileInterval)
		}
	}
}

// ownedAgentsReconciler carries the mutable state between passes: the last
// fully-successful manifest fingerprint, the exact token observed revoked (so
// a re-pair with a fresh token resumes immediately while the dead token stays
// parked), and the log-once latches.
type ownedAgentsReconciler struct {
	paths commentbus.Paths
	// botletsHomeHint is the daemon's --botlets-home value, plumbed through so
	// the Botlets registry check resolves the SAME home the running daemon and
	// the enrollment worker use (resolveDaemonBotletsHome).
	botletsHomeHint string
	lastFingerprint string
	// lastAgents are the manifest agents (handle + kind) cached alongside
	// lastFingerprint. The server's `unchanged` answer only proves the SERVER
	// side did not move; these let the fast path also verify the local installs
	// still exist (a profile deleted or restored from an older backup must
	// converge, not stay invisible until an unrelated manifest change). The
	// kind rides along because a Botlets bot's install is profile + registry
	// entry, not the profile alone.
	lastAgents        []ownedAgentsManifestAgent
	revokedToken      string
	loggedAuthBroken  bool
	loggedAuthRevoked bool
}

func newOwnedAgentsReconciler(paths commentbus.Paths) *ownedAgentsReconciler {
	return &ownedAgentsReconciler{paths: paths}
}

type ownedAgentsManifestAgent struct {
	AgentID     string `json:"agent_id"`
	Handle      string `json:"handle"`
	DisplayName string `json:"display_name"`
	Runtime     string `json:"runtime"`
	// ScheduleTimezone is the managed-session reset timezone the server wants
	// this (Botlets) bot installed with. A change must re-enroll so the local
	// registry's timezone converges (cf carries it as schedule_timezone; null
	// decodes to ""). Empty for generic agents.
	ScheduleTimezone string `json:"schedule_timezone"`
	// Brain is the desired brain reference for a Botlets bot (null/absent for a
	// generic agent, and for a server too old to carry it). cf nests
	// setup_generation under this block (mirroring the redeem hint), and hashes
	// the whole block into the fingerprint, so a brain re-provision must
	// re-install even when handle/runtime/timezone are unchanged.
	Brain *ownedAgentsManifestBrain `json:"brain"`
	Kind  string                    `json:"kind"`
}

type ownedAgentsManifestBrain struct {
	WorkspaceID  string `json:"workspace_id"`
	ContainerID  string `json:"container_id"`
	RootFolderID string `json:"root_folder_id"`
	// BotID, BotAgentID, and OwnerAgentID are the brain identity fields cf now
	// nests here and hashes into the fingerprint. BotAgentID and OwnerAgentID are
	// recorded into the registry's brain_ref at install time, so a change must
	// re-enroll (compared in agentLocallyInstalled). BotID is the bot's durable,
	// immutable identity: the registry does NOT store it in brain_ref, and a
	// different bot_id means a different agent_id/handle — i.e. a different
	// manifest entry that surfaces as a MISSING handle (not a same-handle field
	// drift), so it cannot diverge for a stable handle and is decoded but not
	// compared.
	BotID        string `json:"bot_id"`
	BotAgentID   string `json:"bot_agent_id"`
	OwnerAgentID string `json:"owner_agent_id"`
	// SetupGeneration is a pointer so we only compare when the manifest actually
	// declares it (null/absent -> skip rather than churn against a locally
	// recorded 0 the server never asserted).
	SetupGeneration *int `json:"setup_generation"`
}

type ownedAgentsManifest struct {
	OK          bool                       `json:"ok"`
	AutoInstall bool                       `json:"auto_install"`
	Fingerprint string                     `json:"fingerprint"`
	Unchanged   bool                       `json:"unchanged"`
	Agents      []ownedAgentsManifestAgent `json:"agents"`
}

// runOnce performs one reconcile pass.
func (w *ownedAgentsReconciler) runOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	auth, paired, err := commentbus.LoadDaemonAuth(w.paths)
	if err != nil {
		if !w.loggedAuthBroken {
			w.loggedAuthBroken = true
			w.logWarn("owned_agents.daemon_auth_unreadable", map[string]any{"error": err.Error()})
		}
		return
	}
	if !paired {
		w.loggedAuthBroken = false
		// Unpaired: no-op cleanly, and forget any prior revoked-token latch and
		// fingerprint so a future pairing starts fresh.
		w.revokedToken = ""
		w.loggedAuthRevoked = false
		w.lastFingerprint = ""
		w.lastAgents = nil
		return
	}
	if strings.TrimSpace(auth.BaseURL) == "" {
		if !w.loggedAuthBroken {
			w.loggedAuthBroken = true
			w.logWarn("owned_agents.daemon_auth_missing_base_url", map[string]any{"daemon_id": auth.DaemonID})
		}
		return
	}
	w.loggedAuthBroken = false
	if w.revokedToken != "" && auth.Token == w.revokedToken {
		// The server told us this exact token is dead. Stop calling with it;
		// the pairing-file re-read above resumes work after a re-pair.
		return
	}
	w.revokedToken = ""
	w.loggedAuthRevoked = false

	manifest, status, fetchErr := w.fetchOwnedAgents(ctx, auth)
	switch {
	case fetchErr != nil:
		w.lastFingerprint = ""
		w.logWarn("owned_agents.reconcile_failed", map[string]any{
			"daemon_id": auth.DaemonID,
			"error":     fetchErr.Error(),
		})
		return
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// Daemon token revoked server-side: log once and park this token.
		w.revokedToken = auth.Token
		w.lastFingerprint = ""
		if !w.loggedAuthRevoked {
			w.loggedAuthRevoked = true
			w.logWarn("owned_agents.daemon_token_revoked", map[string]any{
				"daemon_id": auth.DaemonID,
				"status":    status,
			})
		}
		return
	case status != http.StatusOK:
		w.lastFingerprint = ""
		w.logWarn("owned_agents.reconcile_failed", map[string]any{
			"daemon_id": auth.DaemonID,
			"error":     fmt.Sprintf("owned-agents manifest returned status %d", status),
		})
		return
	}
	if !manifest.AutoInstall {
		// Auto-install is switched off for this daemon: store nothing, do
		// nothing. The stored fingerprint is left as-is — the server
		// short-circuits before comparing it anyway.
		return
	}
	if manifest.Unchanged {
		// Fingerprint fast path: nothing changed SERVER-side since the last
		// fully successful pass. Still verify the local installs the cached
		// fingerprint vouched for: if one went missing (deleted profile,
		// restore from an older backup, a Botlets registry that lost its
		// entry), drop the fingerprint so the next pass re-fetches the full
		// manifest and re-enrolls — otherwise the server keeps answering
		// `unchanged` and the gap never heals.
		for _, agent := range w.lastAgents {
			if w.agentLocallyInstalled(agent, auth.DaemonID) {
				continue
			}
			w.lastFingerprint = ""
			w.lastAgents = nil
			w.logWarn("owned_agents.local_profile_missing", map[string]any{
				"daemon_id": auth.DaemonID,
				"handle":    strings.TrimSpace(agent.Handle),
			})
			return
		}
		return
	}
	enrolled := 0
	failed := 0
	for _, agent := range manifest.Agents {
		if ctx.Err() != nil {
			// Shutdown mid-pass: the pass did not complete, so do not keep a
			// fingerprint that would skip the unfinished work after a restart.
			w.lastFingerprint = ""
			return
		}
		handle := strings.TrimSpace(agent.Handle)
		if !commentbus.ProfileRE.MatchString(handle) {
			// A handle that cannot map to a safe profile path can never install;
			// enrolling it would just wedge the enrollment worker. Skip without
			// failing the pass — only a changed manifest can change the outcome.
			w.logWarn("owned_agents.invalid_handle", map[string]any{
				"agent_id": agent.AgentID,
				"handle":   agent.Handle,
			})
			continue
		}
		if w.agentLocallyInstalled(agent, auth.DaemonID) {
			continue
		}
		if w.selfEnroll(ctx, auth, agent) {
			enrolled++
		} else {
			failed++
		}
	}
	if failed > 0 {
		// Partial failure: clear the fingerprint so the next pass re-fetches
		// and retries the missed enrollments (the server-side create is
		// idempotent, so re-enrolling the ones that succeeded is harmless).
		w.lastFingerprint = ""
		w.logWarn("owned_agents.reconcile_failed", map[string]any{
			"daemon_id": auth.DaemonID,
			"error":     fmt.Sprintf("%d of %d agent enrollment(s) failed", failed, len(manifest.Agents)),
		})
		return
	}
	if enrolled == 0 {
		// Every manifest agent already has a local profile — the manifest is
		// genuinely in sync with local state, so cache the fingerprint for the
		// fast path. When anything was enrolled this pass, the install has NOT
		// happened yet (the enrollment worker redeems asynchronously), and the
		// enrollment could still expire or be cancelled without a profile ever
		// landing. Keeping the fingerprint then would let the server's
		// `unchanged` fast path skip re-enrollment forever; leaving it empty
		// makes the next pass re-diff (the server create is idempotent while
		// the enrollment is active, and mints a fresh one after expiry).
		w.lastFingerprint = manifest.Fingerprint
		agents := make([]ownedAgentsManifestAgent, 0, len(manifest.Agents))
		for _, agent := range manifest.Agents {
			if handle := strings.TrimSpace(agent.Handle); commentbus.ProfileRE.MatchString(handle) {
				agents = append(agents, agent)
			}
		}
		w.lastAgents = agents
	}
	w.logInfo("owned_agents.reconcile_complete", map[string]any{
		"daemon_id": auth.DaemonID,
		"enrolled":  enrolled,
		"agents":    len(manifest.Agents),
	})
}

// agentProfileInstalled reports whether a local agent profile already exists
// for handle — the exact `<home>/agents/<handle>.json` file the enrollment
// worker's install step writes (commentbus.PrepareAgentProfileWrite target).
// The handle is ProfileRE-validated by the caller before building the path.
func (w *ownedAgentsReconciler) agentProfileInstalled(handle string, currentDaemonID string) bool {
	path := agentProfileFilePath(w.paths, handle)
	if !profileFileExists(path) {
		return false
	}
	if enrollJournalProfileNeedsDaemonRefresh(w.paths, handle, path, currentDaemonID) {
		return false
	}
	// A profile file that exists but lacks the credentials `comment run` needs
	// (handle/agent_secret/base_url) is NOT a complete install — caching the
	// fingerprint as done over it would strand a bot that can never launch.
	// Indeterminate (unreadable/partial write) stays "installed" to avoid
	// churning a re-enroll over a transient read.
	credentialed, indeterminate := agentProfileCredentialed(path)
	return credentialed || indeterminate
}

// agentLocallyInstalled reports whether a manifest agent's LOCAL install is
// complete AND matches the manifest's DESIRED state. Existence alone is not
// enough: when the manifest changes only a desired field (Claude->Codex
// runtime, a new schedule timezone, a re-provisioned brain's setup generation)
// the handle/profile still exist, so a presence-only check would cache the new
// fingerprint with the stale install on disk and `comment run <handle>` would
// keep launching the old runtime / managed-session timezone / brain projection.
// We therefore read the locally-recorded desired fields back and treat a
// mismatch as NOT installed so the normal enroll -> install path rewrites them.
//
// For a generic agent the install is the profile file alone (its runtime is
// recorded in the profile). A Botlets bot is only installed when the Botlets
// registry ALSO carries its entry — the enrollment worker writes both, and a
// registry that lost the bot (deleted/corrupted registry.json, restore from an
// older backup) leaves the bot unwired even though the profile survives. The
// registry entry records runtime, timezone, and brain setup generation.
// The caller validated the handle against commentbus.ProfileRE.
func (w *ownedAgentsReconciler) agentLocallyInstalled(agent ownedAgentsManifestAgent, currentDaemonID string) bool {
	handle := strings.TrimSpace(agent.Handle)
	if !w.agentProfileInstalled(handle, currentDaemonID) {
		return false
	}
	if agent.Kind != "botlets" {
		// Generic: compare the profile's recorded runtime against the desired
		// runtime. An unreadable profile is indeterminate, not a mismatch —
		// treat it as installed so a transient read error does not churn a
		// re-enroll over a working install.
		installedRuntime, ok := agentProfileRuntime(agentProfileFilePath(w.paths, handle))
		if !ok {
			return true
		}
		// Compare runtime ASYMMETRICALLY, like the brain fields below: only a
		// manifest that DECLARES a concrete runtime drives a runtime reinstall.
		// A null/empty manifest runtime means the server asserts no runtime — a
		// generic agent with no prior enrollment runtime makes cf fall back to a
		// null latestEnrollmentRuntime — so leave the locally-installed runtime
		// alone. Comparing here would route empty through normalizeAgentRuntime's
		// empty->claude coercion, flag an already-installed codex profile as
		// missing, and rewrite it to the Claude fallback (PrepareAgentProfileWrite
		// omits the runtime field on a runtime-less self-enroll).
		if desired := strings.TrimSpace(agent.Runtime); desired != "" {
			return normalizeAgentRuntime(installedRuntime) == normalizeAgentRuntime(desired)
		}
		return true
	}
	botletsHome, err := resolveDaemonBotletsHome(w.paths, w.botletsHomeHint)
	if err != nil {
		// Environmental (untrusted/invalid Botlets home): enrolling cannot fix
		// it — the install step would only churn BOTLETS_WIRING_FAILED retries
		// — so fall back to the profile-only signal until the home is usable.
		return true
	}
	entries, readable := botletsRegistryEntries(botletsHome)
	if !readable {
		return false
	}
	entry, ok := entries[handle]
	if !ok {
		return false
	}
	if normalizeAgentRuntime(entry.Runtime) != normalizeAgentRuntime(agent.Runtime) {
		return false
	}
	if strings.TrimSpace(entry.Timezone) != strings.TrimSpace(agent.ScheduleTimezone) {
		return false
	}
	// Brain reference: re-install when the server's desired brain projection
	// differs from the locally recorded one. cf nests these under `brain` and
	// hashes them into the fingerprint, so a re-provision (setup_generation bump
	// or a workspace/container/root-folder change) reaches here as a full
	// manifest. Compare a declared field asymmetrically: a non-empty manifest
	// value (or a non-nil setup_generation) must match; a null/absent value is
	// "not asserted" and is skipped so a server too old to carry the brain — or
	// a deliberately-null field — does not churn re-enrollments.
	if brain := agent.Brain; brain != nil {
		if v := strings.TrimSpace(brain.WorkspaceID); v != "" && v != strings.TrimSpace(entry.WorkspaceID) {
			return false
		}
		if v := strings.TrimSpace(brain.ContainerID); v != "" && v != strings.TrimSpace(entry.ContainerID) {
			return false
		}
		if v := strings.TrimSpace(brain.RootFolderID); v != "" && v != strings.TrimSpace(entry.RootFolderID) {
			return false
		}
		// Brain identity ids the install records into brain_ref. A change (e.g. a
		// re-provision that re-mints the bot or owner agent) must re-enroll so the
		// registry converges. bot_id is intentionally NOT compared: it is the
		// immutable durable bot identity and is not stored in brain_ref (see
		// ownedAgentsManifestBrain).
		if v := strings.TrimSpace(brain.OwnerAgentID); v != "" && v != strings.TrimSpace(entry.OwnerAgentID) {
			return false
		}
		if v := strings.TrimSpace(brain.BotAgentID); v != "" && v != strings.TrimSpace(entry.BotAgentID) {
			return false
		}
		if brain.SetupGeneration != nil && entry.SetupGeneration != *brain.SetupGeneration {
			return false
		}
	}
	return true
}

// fetchOwnedAgents performs the manifest poll. The last fully-successful
// fingerprint rides along as a query parameter so an unchanged manifest is a
// near-free `unchanged` answer.
func (w *ownedAgentsReconciler) fetchOwnedAgents(ctx context.Context, auth commentbus.DaemonAuth) (ownedAgentsManifest, int, error) {
	endpoint := strings.TrimRight(auth.BaseURL, "/") + "/daemon/owned-agents?fingerprint=" + url.QueryEscape(w.lastFingerprint)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ownedAgentsManifest{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("X-Comment-CLI-Version", version)
	resp, err := ownedAgentsHTTPClient.Do(req)
	if err != nil {
		return ownedAgentsManifest{}, 0, err
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return ownedAgentsManifest{}, resp.StatusCode, nil
	}
	var manifest ownedAgentsManifest
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return ownedAgentsManifest{}, resp.StatusCode, errors.New("owned-agents manifest returned an unreadable response")
	}
	return manifest, resp.StatusCode, nil
}

// selfEnroll creates (or re-finds — the server is idempotent per
// (agent, daemon) and answers 200 for an existing pending/redeemed enrollment,
// 201 for a fresh one) the enrollment the 4s enrollment worker will redeem
// and install. Returns true when the enrollment exists server-side.
func (w *ownedAgentsReconciler) selfEnroll(ctx context.Context, auth commentbus.DaemonAuth, agent ownedAgentsManifestAgent) bool {
	payload := map[string]string{"agent_id": agent.AgentID}
	if runtime := strings.TrimSpace(agent.Runtime); runtime != "" {
		// Preserve the manifest's runtime selection: without it the server
		// records runtime:null on the self-created enrollment for a generic
		// agent and the installed profile silently falls back to Claude.
		// Servers that do not yet accept the field ignore unknown JSON keys.
		payload["runtime"] = runtime
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	endpoint := strings.TrimRight(auth.BaseURL, "/") + "/daemon/agent-enrollments"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("X-Comment-CLI-Version", version)
	resp, err := ownedAgentsHTTPClient.Do(req)
	if err != nil {
		w.logWarn("owned_agents.enroll_failed", map[string]any{
			"agent_id": agent.AgentID,
			"handle":   agent.Handle,
			"error":    err.Error(),
		})
		return false
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		w.logInfo("owned_agents.enrolled", map[string]any{
			"agent_id": agent.AgentID,
			"handle":   agent.Handle,
		})
		return true
	}
	w.logWarn("owned_agents.enroll_failed", map[string]any{
		"agent_id": agent.AgentID,
		"handle":   agent.Handle,
		"status":   resp.StatusCode,
		"code":     decodeErrorCode(resp.Body),
	})
	return false
}

func (w *ownedAgentsReconciler) logInfo(msg string, data map[string]any) {
	writeDaemonWorkerLog(w.paths, "agent.owned_agents", "info", msg, data)
}

func (w *ownedAgentsReconciler) logWarn(msg string, data map[string]any) {
	writeDaemonWorkerLog(w.paths, "agent.owned_agents", "warn", msg, data)
}
