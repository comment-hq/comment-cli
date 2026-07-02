package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// Command-free team install worker.
//
// Polls GET /daemon/team-installs for "Install this team on this computer"
// requests the owner created in the browser, and for each one redeems a runner
// over the daemon's OWN pairing token (no `comment botlets team-setup` command,
// no second human approval — the pairing approval is the proof). It then writes
// team-runtime.json; the existing team-resync worker picks that up on its next
// tick and installs every team bot. This worker therefore contains ZERO install
// code — it only obtains the runner secret command-free.
//
// It advertises `team_install:v1` on every call so the server lights up this
// daemon's capability (the browser filters install targets by it); a daemon too
// old to run this worker never advertises it and the browser keeps the
// copy-paste team install command as the fallback.
const (
	teamInstallPollInterval      = 30 * time.Second
	teamInstallAuthRetryInterval = 2 * time.Second
	teamInstallCapability        = "team_install:v1"
)

var teamInstallHTTPClient = &http.Client{Timeout: 60 * time.Second}

func startBotletsTeamInstallWorker(ctx context.Context, paths commentbus.Paths, botletsHomeHint string) {
	go runBotletsTeamInstallWorkerWithDelays(ctx, paths, botletsHomeHint, teamInstallPollInterval, teamInstallAuthRetryInterval)
}

func runBotletsTeamInstallWorkerWithDelays(ctx context.Context, paths commentbus.Paths, botletsHomeHint string, steadyDelay, authRetryDelay time.Duration) {
	w := &botletsTeamInstallWorker{paths: paths, botletsHomeHint: botletsHomeHint}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if w.runOnce(ctx) {
				timer.Reset(steadyDelay)
			} else {
				timer.Reset(authRetryDelay)
			}
		}
	}
}

type botletsTeamInstallWorker struct {
	paths             commentbus.Paths
	botletsHomeHint   string
	revokedToken      string
	loggedAuthBroken  bool
	loggedAuthRevoked bool
}

func (w *botletsTeamInstallWorker) logWarn(msg string, data map[string]any) {
	writeDaemonWorkerLog(w.paths, "agent.team_install", "warn", msg, data)
}

func (w *botletsTeamInstallWorker) logInfo(msg string, data map[string]any) {
	writeDaemonWorkerLog(w.paths, "agent.team_install", "info", msg, data)
}

type teamInstallItem struct {
	RequestID   string `json:"request_id"`
	WorkspaceID string `json:"workspace_id"`
	Runtime     string `json:"runtime"`
	State       string `json:"state"`
}

type teamInstallListResponse struct {
	TeamInstalls []teamInstallItem `json:"team_installs"`
}

type teamInstallRedeemResponse struct {
	RunnerID     string `json:"runner_id"`
	RunnerSecret string `json:"runner_secret"`
	WorkspaceID  string `json:"workspace_id"`
	Runtime      string `json:"runtime"`
	BaseURL      string `json:"base_url"`
}

// runOnce returns true when it polled successfully (run on the steady cadence),
// false when it should retry on the faster auth cadence (unpaired / unreadable
// auth) — mirroring the owned-agents reconciler's contract.
func (w *botletsTeamInstallWorker) runOnce(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	auth, paired, err := commentbus.LoadDaemonAuth(w.paths)
	if err != nil {
		if !w.loggedAuthBroken {
			w.loggedAuthBroken = true
			w.logWarn("team_install.daemon_auth_unreadable", map[string]any{"error": err.Error()})
		}
		return false
	}
	if !paired {
		w.loggedAuthBroken = false
		w.revokedToken = ""
		w.loggedAuthRevoked = false
		return false
	}
	if strings.TrimSpace(auth.BaseURL) == "" {
		if !w.loggedAuthBroken {
			w.loggedAuthBroken = true
			w.logWarn("team_install.daemon_auth_missing_base_url", map[string]any{"daemon_id": auth.DaemonID})
		}
		return false
	}
	w.loggedAuthBroken = false
	if w.revokedToken != "" && auth.Token == w.revokedToken {
		return true
	}
	w.revokedToken = ""
	w.loggedAuthRevoked = false

	var list teamInstallListResponse
	status, ferr := w.doDaemonJSON(ctx, auth, http.MethodGet, strings.TrimRight(auth.BaseURL, "/")+"/daemon/team-installs", nil, &list)
	switch {
	case ferr != nil:
		w.logWarn("team_install.poll_failed", map[string]any{"daemon_id": auth.DaemonID, "error": ferr.Error()})
		return true
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		w.revokedToken = auth.Token
		if !w.loggedAuthRevoked {
			w.loggedAuthRevoked = true
			w.logWarn("team_install.daemon_token_revoked", map[string]any{"daemon_id": auth.DaemonID, "status": status})
		}
		return true
	case status != http.StatusOK:
		w.logWarn("team_install.poll_failed", map[string]any{"daemon_id": auth.DaemonID, "status": status})
		return true
	}
	if len(list.TeamInstalls) == 0 {
		return true
	}
	home, herr := resolveDaemonBotletsHome(w.paths, w.botletsHomeHint)
	if herr != nil {
		w.logWarn("team_install.home_unresolved", map[string]any{"error": herr.Error()})
		return true
	}
	for _, item := range list.TeamInstalls {
		w.installOne(ctx, auth, home, item)
	}
	return true
}

func (w *botletsTeamInstallWorker) installOne(ctx context.Context, auth commentbus.DaemonAuth, home string, item teamInstallItem) {
	// Guard: never clobber a team-runtime.json already configured for a DIFFERENT
	// workspace. A second pending install for another workspace must not hijack
	// this computer's existing team runtime; the user moves runtimes deliberately.
	if cfg, err := readBotletsTeamRuntimeConfig(home); err == nil &&
		cfg.WorkspaceID != "" && cfg.RunnerID != "" && cfg.WorkspaceID != item.WorkspaceID {
		w.logWarn("team_install.skip_other_workspace", map[string]any{
			"request_id":     item.RequestID,
			"want_workspace": item.WorkspaceID,
			"have_workspace": cfg.WorkspaceID,
		})
		return
	}
	// Deterministic per-machine install id: the redeem idempotency key + the
	// duplicate-install guard hash. A copied team-runtime.json re-derives a
	// different id on another machine, which the server flags on heartbeat.
	installationID, err := machineInstallVerifier(item.WorkspaceID, home)
	if err != nil {
		w.logWarn("team_install.installation_id_failed", map[string]any{"request_id": item.RequestID, "error": err.Error()})
		return
	}
	var redeemed teamInstallRedeemResponse
	redeemURL := strings.TrimRight(auth.BaseURL, "/") + "/daemon/team-installs/" + url.PathEscape(item.RequestID) + "/redeem"
	status, rerr := w.doDaemonJSON(ctx, auth, http.MethodPost, redeemURL, map[string]any{
		"idempotency_key":      "team-" + installationID,
		"installation_id_hash": installationID,
		"cli_version":          version,
	}, &redeemed)
	if rerr != nil || status != http.StatusOK || redeemed.RunnerID == "" || redeemed.RunnerSecret == "" {
		errMsg := ""
		if rerr != nil {
			errMsg = rerr.Error()
		}
		w.logWarn("team_install.redeem_failed", map[string]any{"request_id": item.RequestID, "status": status, "error": errMsg})
		// The redeem route consumes the request (pending -> redeemed) BEFORE it
		// provisions the workspace runner, and consume is idempotent on the same
		// idempotency key. On a RETRYABLE failure (transport error, a 5xx while
		// runner provisioning is flaky, or a rate-limit) leave the request unacked:
		// it stays 'redeemed', the daemon poll re-returns it (the list returns
		// pending AND redeemed), and the next tick re-redeems with the same key and
		// recovers. Acking 'failed' on a momentary blip would flip it terminal and
		// permanently break that recovery, failing the browser install.
		if teamInstallRedeemRetryable(status, rerr) {
			return
		}
		// TERMINAL failure (a 4xx such as stale / cancelled / expired /
		// daemon-mismatch, or a 200 carrying no runner): retrying can never
		// succeed. The owner DO's sweep expires only 'pending' requests, so a
		// consumed-then-terminally-failed request left unacked would be re-redeemed
		// every poll forever and the browser would hang on "Installing…". Ack
		// 'failed' so the request reaches a terminal state and the browser falls
		// back to the setup command.
		w.ack(ctx, auth, item.RequestID, "failed", "REDEEM_FAILED", errMsg)
		return
	}
	cfg := botletsTeamRuntimeConfig{
		WorkspaceID:    redeemed.WorkspaceID,
		RunnerID:       redeemed.RunnerID,
		RunnerSecret:   redeemed.RunnerSecret,
		BaseURL:        redeemed.BaseURL,
		Runtime:        redeemed.Runtime,
		InstallationID: installationID,
	}
	// The four-field resync gate requires all of these; backfill from what we know.
	if cfg.BaseURL == "" {
		cfg.BaseURL = auth.BaseURL
	}
	if cfg.Runtime == "" {
		cfg.Runtime = item.Runtime
	}
	if cfg.WorkspaceID == "" {
		cfg.WorkspaceID = item.WorkspaceID
	}
	if err := writeBotletsTeamRuntimeConfig(home, cfg); err != nil {
		w.logWarn("team_install.config_write_failed", map[string]any{"request_id": item.RequestID, "error": err.Error()})
		w.ack(ctx, auth, item.RequestID, "failed", "CONFIG_WRITE_FAILED", err.Error())
		return
	}
	// team-runtime.json now exists: the existing team-resync worker installs every
	// team bot on its next tick. Nothing to install here.
	w.ack(ctx, auth, item.RequestID, "installed", "", "")
	w.logInfo("team_install.installed", map[string]any{
		"request_id":   item.RequestID,
		"workspace_id": cfg.WorkspaceID,
		"runner_id":    cfg.RunnerID,
	})
}

// teamInstallRedeemRetryable reports whether a failed redeem should be left
// unacked for the daemon poll to retry, rather than acked terminally. A transport
// error (doDaemonJSON returns status 0) or a decode error on a 2xx body (a
// truncated success) recovers on the next idempotent re-redeem, as does a 5xx
// (the workspace-runner mint flaking) or a 429 rate-limit. Every other non-200 —
// a 4xx client/terminal error (stale, cancelled, expired, daemon-mismatch,
// consumed-by-another-key) or a malformed 200 with no runner — is terminal and
// must be acked 'failed' so the request leaves the 'redeemed' state (which the
// owner DO's sweep never expires) instead of being re-redeemed forever.
func teamInstallRedeemRetryable(status int, err error) bool {
	if err != nil {
		return true
	}
	return status >= 500 || status == http.StatusTooManyRequests
}

func (w *botletsTeamInstallWorker) ack(ctx context.Context, auth commentbus.DaemonAuth, requestID, state, code, message string) {
	ackURL := strings.TrimRight(auth.BaseURL, "/") + "/daemon/team-installs/" + url.PathEscape(requestID) + "/ack"
	payload := map[string]any{"state": state}
	if code != "" {
		payload["failure_code"] = code
	}
	if message != "" {
		payload["failure_message"] = message
	}
	if _, err := w.doDaemonJSON(ctx, auth, http.MethodPost, ackURL, payload, nil); err != nil {
		w.logWarn("team_install.ack_failed", map[string]any{"request_id": requestID, "state": state, "error": err.Error()})
	}
}

// doDaemonJSON makes an ldt_-authenticated daemon call advertising team_install:v1
// (so the server stores the capability). Decodes a 2xx JSON body into out when
// non-nil. Returns the HTTP status; a transport/encode error is the error.
func (w *botletsTeamInstallWorker) doDaemonJSON(ctx context.Context, auth commentbus.DaemonAuth, method, endpoint string, payload any, out any) (int, error) {
	var req *http.Request
	var err error
	if payload != nil {
		raw, mErr := json.Marshal(payload)
		if mErr != nil {
			return 0, mErr
		}
		req, err = http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(string(raw)))
	} else {
		req, err = http.NewRequestWithContext(ctx, method, endpoint, nil)
	}
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("X-Comment-CLI-Version", version)
	req.Header.Set(runtimeRequestCapabilityHeader, teamInstallCapability)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := teamInstallHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer drainAndClose(resp)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && out != nil {
		if decErr := json.NewDecoder(resp.Body).Decode(out); decErr != nil {
			return resp.StatusCode, decErr
		}
	}
	return resp.StatusCode, nil
}
