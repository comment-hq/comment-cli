package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

const (
	botletsTeamResyncWorkerInterval = 30 * time.Second
	botletsTeamResyncWorkerTimeout  = 5 * time.Minute
)

var (
	botletsTeamResyncHTTPClient = &http.Client{Timeout: 60 * time.Second}
	botletsTeamResyncManifest   = resyncBotletsTeamManifest
)

type botletsTeamResyncSummary struct {
	Configured  bool
	BotletsHome string
	WorkspaceID string
	RunnerID    string
	Agents      []string
	UpToDate    bool
	// ResyncError is the truncated failure detail from this pass (or the prior
	// error carried forward when the pass could not run). The worker feeds it
	// into the next pass's runner heartbeat (Phase 9b) so a persistent failure
	// — e.g. "sync not configured" — stays visible on the runner record, and a
	// success explicitly clears it.
	ResyncError string
}

func startBotletsTeamResyncWorker(ctx context.Context, paths commentbus.Paths, botletsHomeHint string) {
	go runBotletsTeamResyncWorker(ctx, paths, botletsHomeHint, botletsTeamResyncWorkerInterval)
}

func runBotletsTeamResyncWorker(ctx context.Context, paths commentbus.Paths, botletsHomeHint string, interval time.Duration) {
	if interval <= 0 {
		interval = botletsTeamResyncWorkerInterval
	}
	lastResyncError := ""
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			summary := runBotletsTeamResyncWorkerOnce(ctx, paths, botletsHomeHint, lastResyncError)
			lastResyncError = summary.ResyncError
			timer.Reset(interval)
		}
	}
}

func runBotletsTeamResyncWorkerOnce(ctx context.Context, paths commentbus.Paths, botletsHomeHint string, priorResyncError string) botletsTeamResyncSummary {
	summary := botletsTeamResyncSummary{ResyncError: priorResyncError}
	if ctx.Err() != nil {
		return summary
	}
	botletsHome, err := resolveDaemonBotletsHome(paths, botletsHomeHint)
	if err != nil {
		writeDaemonWorkerLog(paths, "botlets.team_resync", "warn", "botlets.team_resync_home_invalid", map[string]any{
			"error": err.Error(),
		})
		return summary
	}
	summary.BotletsHome = botletsHome
	cfg, err := readBotletsTeamRuntimeConfig(botletsHome)
	if errors.Is(err, os.ErrNotExist) {
		return summary
	}
	if err != nil {
		writeDaemonWorkerLog(paths, "botlets.team_resync", "warn", "botlets.team_resync_config_read_failed", map[string]any{
			"botlets_home": botletsHome,
			"error":        err.Error(),
		})
		return summary
	}
	summary.Configured = true
	summary.WorkspaceID = cfg.WorkspaceID
	summary.RunnerID = cfg.RunnerID
	// One-approval trust: registering team bots needs local library sync. When
	// this machine is ALSO daemon-paired, self-provision the sync credential
	// over the pairing token instead of failing until a human runs
	// `comment sync login`. Best-effort: an unpaired team-runtime machine
	// keeps today's behavior (the resync error is browser-visible).
	var syncProvisionErr error
	if auth, paired, authErr := commentbus.LoadDaemonAuth(paths); authErr == nil && paired && strings.TrimSpace(auth.BaseURL) != "" {
		if provErr := ensureSyncConfiguredViaDaemonSeam(ctx, paths, auth); provErr != nil {
			syncProvisionErr = provErr
			writeDaemonWorkerLog(paths, "botlets.team_resync", "warn", "botlets.team_resync_sync_provision_failed", map[string]any{
				"botlets_home": botletsHome,
				"error":        provErr.Error(),
			})
		}
	}
	if cfg.WorkspaceID == "" || cfg.RunnerID == "" || cfg.RunnerSecret == "" || cfg.BaseURL == "" {
		writeDaemonWorkerLog(paths, "botlets.team_resync", "warn", "botlets.team_resync_config_incomplete", map[string]any{
			"botlets_home": botletsHome,
			"workspace_id": cfg.WorkspaceID,
			"runner_id":    cfg.RunnerID,
			"missing":      botletsTeamRuntimeConfigMissingFields(cfg),
		})
		return summary
	}
	runtime := cfg.Runtime
	if runtime != "claude" && runtime != "codex" {
		runtime = "claude"
	}
	runCtx, cancel := context.WithTimeout(ctx, botletsTeamResyncWorkerTimeout)
	defer cancel()
	if cfg.LastManifestFingerprint != "" {
		_, _ = heartbeatBotletsTeamRuntime(runCtx, botletsTeamResyncHTTPClient, botletsHome, cfg, priorResyncError)
		status, err := fetchBotletsTeamRuntimeManifestStatus(runCtx, botletsTeamResyncHTTPClient, botletsHome, cfg)
		if err != nil {
			// A FAILED lightweight status poll is NOT evidence the manifest
			// changed. Falling through to the full /team-manifest fetch below
			// would mint fresh runner-bound credentials for every bot, so a
			// status-route outage or persistent 502 would orphan an identical
			// credential set every 30s. Treat it as a retryable skip: keep the
			// persisted fingerprint untouched and report the error.
			if runCtx.Err() != nil && ctx.Err() != nil {
				return summary
			}
			summary.ResyncError = truncateBotletsResyncError(err.Error())
			writeDaemonWorkerLog(paths, "botlets.team_resync", "warn", "botlets.team_resync_status_failed", map[string]any{
				"botlets_home": botletsHome,
				"workspace_id": cfg.WorkspaceID,
				"runner_id":    cfg.RunnerID,
				"error":        err.Error(),
			})
			// The leading heartbeat only carried the prior error; report the
			// fresh failure now (best-effort, own deadline) rather than one
			// interval later.
			if summary.ResyncError != priorResyncError {
				hbCtx, hbCancel := context.WithTimeout(ctx, 30*time.Second)
				_, _ = heartbeatBotletsTeamRuntime(hbCtx, botletsTeamResyncHTTPClient, botletsHome, cfg, summary.ResyncError)
				hbCancel()
			}
			return summary
		}
		// A nil persisted handle list means the fingerprint was persisted before
		// LastManifestAgents existed (config upgraded in place): the fast-path
		// local-install verification has no handles to check, so honoring the
		// unchanged fingerprint would leave that verification disabled
		// indefinitely. Force ONE full resync in that case — it backfills the
		// list (persist fires on a nil list, even for a genuinely empty team),
		// after which this fast path verifies installs as usual. A non-nil empty
		// list is a real zero-bot team and stays on the fast path.
		if status.ManifestFingerprint != "" && status.ManifestFingerprint == cfg.LastManifestFingerprint && cfg.LastManifestAgents != nil {
			// The SERVER manifest is unchanged — but that alone does not prove
			// the local installs survived (a deleted/corrupted registry or
			// profile, or a restore from an older backup, would otherwise stay
			// broken until some unrelated server-side manifest change). Verify
			// the handles persisted with the fingerprint before honoring the
			// fast path; on a gap, fall through to the full resync to repair.
			if missing, ok := missingBotletsTeamLocalInstall(paths, botletsHome, cfg.LastManifestAgents); ok {
				writeDaemonWorkerLog(paths, "botlets.team_resync", "warn", "botlets.team_resync_local_install_missing", map[string]any{
					"botlets_home": botletsHome,
					"workspace_id": cfg.WorkspaceID,
					"handle":       missing,
				})
			} else {
				summary.UpToDate = true
				// Up to date means the last full resync landed; clear a stale
				// failure both locally and (when one was reported) server-side.
				summary.ResyncError = ""
				if priorResyncError != "" {
					_, _ = heartbeatBotletsTeamRuntime(runCtx, botletsTeamResyncHTTPClient, botletsHome, cfg, "")
				}
				return summary
			}
		}
	}
	// Sync gate: every full resync mints fresh runner-bound credentials
	// server-side (the team-manifest issue is NOT idempotent), and a machine
	// whose local sync is unusable will then fail every local registration —
	// continuing here would orphan one credential set per bot every 30 seconds
	// without ever installing anything. When this pass's self-provisioning
	// failed AND sync is not already configured for the team's own origin,
	// surface the provisioning failure as the pass error and skip the manifest
	// fetch until sync is usable. (Sync configured for cfg.BaseURL stays
	// usable even when the pairing-token provisioning, which targets the
	// daemon's possibly different origin, failed.)
	if !daemonSyncUsableForOrigin(paths.Home, cfg.BaseURL) {
		// Fail closed whenever local sync is unusable for the team's origin —
		// not only when THIS pass attempted and failed to self-provision.
		// syncProvisionErr stays nil when no provisioning was attempted: an
		// unpaired team-runtime machine, or one where `team-setup` redeemed and
		// persisted the config before its own sync preflight failed. Those would
		// otherwise slip past and mint a fresh runner-bound credential per bot
		// every 30s while local registration keeps failing for missing sync.
		// Surface the provisioning error when there is one; otherwise synthesize
		// the actionable "configure sync" message.
		syncErr := syncProvisionErr
		if syncErr == nil {
			syncErr = fmt.Errorf("local library sync is not configured for %s; run `comment sync login` against %s (skipped the team manifest fetch to avoid minting unrevocable runner-bound credentials)", cfg.BaseURL, cfg.BaseURL)
		}
		summary.ResyncError = truncateBotletsResyncError(syncErr.Error())
		if summary.ResyncError != priorResyncError {
			hbCtx, hbCancel := context.WithTimeout(ctx, 30*time.Second)
			_, _ = heartbeatBotletsTeamRuntime(hbCtx, botletsTeamResyncHTTPClient, botletsHome, cfg, summary.ResyncError)
			hbCancel()
		}
		return summary
	}
	registered, err := botletsTeamResyncManifest(runCtx, botletsTeamResyncHTTPClient, paths, botletsHome, cfg, runtime, priorResyncError)
	if err != nil {
		if runCtx.Err() != nil && ctx.Err() != nil {
			return summary
		}
		summary.ResyncError = truncateBotletsResyncError(err.Error())
		writeDaemonWorkerLog(paths, "botlets.team_resync", "warn", "botlets.team_resync_failed", map[string]any{
			"botlets_home": botletsHome,
			"workspace_id": cfg.WorkspaceID,
			"runner_id":    cfg.RunnerID,
			"error":        err.Error(),
		})
		// The leading heartbeat inside the resync only carried the PRIOR error.
		// Report the fresh failure now (best-effort, own deadline: runCtx may be
		// the very thing that expired) instead of one interval later.
		if summary.ResyncError != priorResyncError {
			hbCtx, hbCancel := context.WithTimeout(ctx, 30*time.Second)
			_, _ = heartbeatBotletsTeamRuntime(hbCtx, botletsTeamResyncHTTPClient, botletsHome, cfg, summary.ResyncError)
			hbCancel()
		}
		return summary
	}
	summary.ResyncError = ""
	if priorResyncError != "" {
		// The pass recovered but its leading heartbeat still carried the prior
		// failure; send the explicit clear now rather than one interval later.
		hbCtx, hbCancel := context.WithTimeout(ctx, 30*time.Second)
		_, _ = heartbeatBotletsTeamRuntime(hbCtx, botletsTeamResyncHTTPClient, botletsHome, cfg, "")
		hbCancel()
	}
	summary.Agents = append([]string{}, registered...)
	writeDaemonWorkerLog(paths, "botlets.team_resync", "info", "botlets.team_resync_complete", map[string]any{
		"botlets_home": botletsHome,
		"workspace_id": cfg.WorkspaceID,
		"runner_id":    cfg.RunnerID,
		"agents":       len(registered),
	})
	return summary
}

// missingBotletsTeamLocalInstall reports the first handle from the last acked
// manifest whose local install is gone: its agent profile file was deleted, or
// the Botlets registry no longer carries an entry for it (registry deleted or
// unreadable). It deliberately checks raw file/JSON presence rather than the
// full profile-validation loader, so only a genuinely missing install — one
// the full resync's registerBotletsBotLocally rewrite actually repairs —
// disables the fingerprint fast path (a validation-only failure repeating
// every pass would otherwise mint fresh credentials in a 30s loop). handles is
// non-nil here: the caller forces a full resync (not this verification) for a
// nil list (a config upgraded from before LastManifestAgents existed), and a
// non-nil empty list is a genuinely zero-bot team with nothing to verify.
func missingBotletsTeamLocalInstall(paths commentbus.Paths, botletsHome string, handles []string) (string, bool) {
	if len(handles) == 0 {
		return "", false
	}
	registryHandles, registryReadable := botletsRegistryHandles(botletsHome)
	for _, handle := range handles {
		if !registryReadable || !registryHandles[handle] {
			return handle, true
		}
		if !profileFileExists(agentProfileFilePath(paths, handle)) {
			return handle, true
		}
	}
	return "", false
}

// botletsRegistryHandles reads <botletsHome>/registry.json raw (no profile
// validation — see missingBotletsTeamLocalInstall for why) and returns the set
// of canonical bot handles it carries, plus whether the file was readable
// (false for a missing or unparseable registry). Shared by the team resync
// fast-path verification and the owned-agents reconciler's Botlets install
// check.
func botletsRegistryHandles(botletsHome string) (map[string]bool, bool) {
	entries, readable := botletsRegistryEntries(botletsHome)
	if !readable {
		return nil, false
	}
	handles := make(map[string]bool, len(entries))
	for handle := range entries {
		handles[handle] = true
	}
	return handles, true
}

// botletsRegistryInstalledState is the locally-recorded desired state for an
// installed Botlets bot, read back from registry.json so the owned-agents
// reconciler can compare it against the server manifest's DESIRED fields and
// re-enroll when a bot's runtime, schedule timezone, or brain setup generation
// changes server-side while its handle stays the same.
type botletsRegistryInstalledState struct {
	Runtime         string
	Timezone        string
	WorkspaceID     string
	OwnerAgentID    string
	BotAgentID      string
	ContainerID     string
	RootFolderID    string
	SetupGeneration int
}

// botletsRegistryEntries reads <botletsHome>/registry.json raw (no profile
// validation — see missingBotletsTeamLocalInstall for why) and returns the
// per-handle installed state (managed-session runtime + timezone and the brain
// setup generation), plus whether the file was readable (false for a missing or
// unparseable registry).
func botletsRegistryEntries(botletsHome string) (map[string]botletsRegistryInstalledState, bool) {
	data, err := os.ReadFile(filepath.Join(botletsHome, "registry.json"))
	if err != nil {
		return nil, false
	}
	var registry struct {
		Bots []struct {
			Handle         string `json:"handle"`
			ManagedSession struct {
				Runtime  string `json:"runtime"`
				Timezone string `json:"timezone"`
			} `json:"managed_session"`
			BrainRef *struct {
				WorkspaceID     string `json:"workspace_id"`
				OwnerAgentID    string `json:"owner_agent_id"`
				BotAgentID      string `json:"bot_agent_id"`
				ContainerID     string `json:"container_id"`
				RootFolderID    string `json:"root_folder_id"`
				SetupGeneration int    `json:"setup_generation"`
			} `json:"brain_ref"`
		} `json:"bots"`
	}
	if json.Unmarshal(data, &registry) != nil {
		return nil, false
	}
	entries := make(map[string]botletsRegistryInstalledState, len(registry.Bots))
	for _, bot := range registry.Bots {
		state := botletsRegistryInstalledState{
			Runtime:  bot.ManagedSession.Runtime,
			Timezone: bot.ManagedSession.Timezone,
		}
		if bot.BrainRef != nil {
			state.WorkspaceID = bot.BrainRef.WorkspaceID
			state.OwnerAgentID = bot.BrainRef.OwnerAgentID
			state.BotAgentID = bot.BrainRef.BotAgentID
			state.ContainerID = bot.BrainRef.ContainerID
			state.RootFolderID = bot.BrainRef.RootFolderID
			state.SetupGeneration = bot.BrainRef.SetupGeneration
		}
		entries[bot.Handle] = state
	}
	return entries, true
}

// agentProfileRuntime reads back the persisted runtime from a generic agent
// profile file (<home>/agents/<handle>.json). The second return is false when
// the file cannot be read or parsed (the caller treats an unreadable profile as
// indeterminate rather than a runtime mismatch). An empty runtime is normal: the
// profile writer omits the field when no runtime was selected and the launcher
// falls back to claude.
func agentProfileRuntime(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var profile struct {
		Runtime string `json:"runtime"`
	}
	if json.Unmarshal(data, &profile) != nil {
		return "", false
	}
	return profile.Runtime, true
}

// agentProfileCredentialed reports whether a generic agent profile file carries
// the fields `comment run <handle>` actually needs to authenticate — handle,
// agent_secret, and base_url. A presence-only install check treats a
// syntactically-valid but credential-less file (`{}`, or one written before a
// field was populated) as installed, so the owned-agents reconciler caches the
// manifest fingerprint as done and never re-enrolls to repair it, leaving a bot
// that can never launch. The second return is indeterminate=true when the file
// cannot be read or parsed (a transient/partial write): the caller treats that
// as installed to avoid churning a re-enroll over a momentarily-unreadable but
// otherwise working profile. A readable, parseable file missing a required field
// is a DEFINITIVE not-credentialed (false, false).
func agentProfileCredentialed(path string) (credentialed bool, indeterminate bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, true
	}
	var profile struct {
		Handle      string `json:"handle"`
		AgentSecret string `json:"agent_secret"`
		BaseURL     string `json:"base_url"`
	}
	if json.Unmarshal(data, &profile) != nil {
		return false, true
	}
	if strings.TrimSpace(profile.Handle) == "" ||
		strings.TrimSpace(profile.AgentSecret) == "" ||
		strings.TrimSpace(profile.BaseURL) == "" {
		return false, false
	}
	return true, false
}

// normalizeAgentRuntime collapses the runtime to its effective launch value so a
// desired-vs-installed comparison does not churn on the documented claude
// fallback: codex stays codex; an empty/unknown/claude value all resolve to
// claude (matching the launcher fallback and the team resync's own coercion).
func normalizeAgentRuntime(runtime string) string {
	if strings.TrimSpace(runtime) == "codex" {
		return "codex"
	}
	return "claude"
}

func botletsTeamRuntimeConfigMissingFields(cfg botletsTeamRuntimeConfig) []string {
	var missing []string
	if strings.TrimSpace(cfg.WorkspaceID) == "" {
		missing = append(missing, "workspace_id")
	}
	if strings.TrimSpace(cfg.RunnerID) == "" {
		missing = append(missing, "runner_id")
	}
	if strings.TrimSpace(cfg.RunnerSecret) == "" {
		missing = append(missing, "runner_secret")
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		missing = append(missing, "base_url")
	}
	return missing
}
