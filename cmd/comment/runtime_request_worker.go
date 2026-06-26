package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// Agent runtime-start request worker — the daemon half of the SuccessCard
// "Start @handle" button. The browser enqueues a pending runtime request on the
// owner's account; this worker fast-polls GET /daemon/runtime-requests with the
// paired daemon token from `<home>/bus/daemon-auth.json`, launches the
// already-installed agent's runtime via `comment run <handle>` (with
// COMMENT_IO_SKIP_ATTACH so it starts the daemon-managed session WITHOUT a tty
// attach), and acks started|failed.
//
// Unlike the enrollment worker it mints no credential and writes no profile:
// the agent is already installed, so this only launches its runtime. The launch
// itself is a short-lived subprocess — `comment run --skip-attach` tells the bus
// daemon to start the session and returns; the session then persists under the
// daemon's management, not as a child of this process.
const (
	// runtimeRequestPollInterval is the fast poll cadence — a human is watching
	// a browser spinner after clicking Start.
	runtimeRequestPollInterval = 4 * time.Second
	// runtimeRequestPairingRecheckInterval is how often the worker re-reads
	// daemon-auth.json while unpaired or after its token was revoked.
	runtimeRequestPairingRecheckInterval = 30 * time.Second
	// runtimeRequestBackoffCap bounds exponential backoff on repeated poll errors.
	runtimeRequestBackoffCap     = 60 * time.Second
	runtimeRequestRequestTimeout = 30 * time.Second
	// runtimeRequestLaunchTimeout bounds the detached `comment run` (skip-attach):
	// a clean exit means started, a non-zero/timeout exit means failed.
	runtimeRequestLaunchTimeout = 60 * time.Second
	runtimeRequestCapabilityHeader = "X-Comment-Daemon-Capabilities"
	runtimeRequestCapability       = "agent_runtime_start:v1"
)

var (
	runtimeRequestHTTPClient = &http.Client{Timeout: runtimeRequestRequestTimeout}
	// runtimeRequestLaunch starts the agent's runtime detached and returns nil on
	// success or an error describing the failure. Stubbed in tests.
	runtimeRequestLaunch = launchAgentRuntimeDetached
)

func startAgentRuntimeRequestWorker(ctx context.Context, paths commentbus.Paths) {
	go runAgentRuntimeRequestWorker(ctx, paths)
}

func runAgentRuntimeRequestWorker(ctx context.Context, paths commentbus.Paths) {
	worker := &agentRuntimeRequestWorker{paths: paths}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(worker.runOnce(ctx))
		}
	}
}

type agentRuntimeRequestWorker struct {
	paths             commentbus.Paths
	pollFailures      int
	revokedToken      string
	loggedAuthBroken  bool
	loggedAuthRevoked bool
	// launched marks request IDs whose runtime we already started but whose ack
	// has not yet landed (network/5xx). The server still lists them as pending,
	// so without this we would relaunch an already-running session on the next
	// poll; instead we skip the launch and only retry the ack. Pruned each pass
	// to the still-pending set, and dropped on a 200 ack.
	launched map[string]bool
}

type agentRuntimeRequestListItem struct {
	RequestID string `json:"request_id"`
	State     string `json:"state"`
	AgentID   string `json:"agent_id"`
	Handle    string `json:"handle"`
	DaemonID  string `json:"daemon_id"`
}

func (w *agentRuntimeRequestWorker) runOnce(ctx context.Context) time.Duration {
	if ctx.Err() != nil {
		return runtimeRequestPairingRecheckInterval
	}
	auth, paired, err := commentbus.LoadDaemonAuth(w.paths)
	if err != nil {
		if !w.loggedAuthBroken {
			w.loggedAuthBroken = true
			w.logWarn("agent_runtime_request.daemon_auth_unreadable", map[string]any{"error": err.Error()})
		}
		return runtimeRequestPairingRecheckInterval
	}
	if !paired {
		w.loggedAuthBroken = false
		w.revokedToken = ""
		w.loggedAuthRevoked = false
		return runtimeRequestPairingRecheckInterval
	}
	if strings.TrimSpace(auth.BaseURL) == "" {
		if !w.loggedAuthBroken {
			w.loggedAuthBroken = true
			w.logWarn("agent_runtime_request.daemon_auth_missing_base_url", map[string]any{"daemon_id": auth.DaemonID})
		}
		return runtimeRequestPairingRecheckInterval
	}
	w.loggedAuthBroken = false
	if w.revokedToken != "" && auth.Token == w.revokedToken {
		return runtimeRequestPairingRecheckInterval
	}
	w.revokedToken = ""
	w.loggedAuthRevoked = false

	items, status, listErr := w.listRequests(ctx, auth)
	switch {
	case listErr != nil:
		w.pollFailures++
		w.logWarn("agent_runtime_request.poll_failed", map[string]any{"daemon_id": auth.DaemonID, "error": listErr.Error()})
		return w.backoffWait()
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		w.revokedToken = auth.Token
		w.pollFailures = 0
		if !w.loggedAuthRevoked {
			w.loggedAuthRevoked = true
			w.logWarn("agent_runtime_request.daemon_token_revoked", map[string]any{"daemon_id": auth.DaemonID, "status": status})
		}
		return runtimeRequestPairingRecheckInterval
	case status != http.StatusOK:
		w.pollFailures++
		w.logWarn("agent_runtime_request.poll_failed", map[string]any{"daemon_id": auth.DaemonID, "status": status})
		return w.backoffWait()
	}
	w.pollFailures = 0
	for _, item := range items {
		if ctx.Err() != nil {
			return runtimeRequestPollInterval
		}
		if item.State != "pending" {
			continue
		}
		w.processRequest(ctx, auth, item)
	}
	// Prune launched markers for requests that left the pending list (acked or
	// expired), so the map can't grow unbounded.
	if len(w.launched) > 0 {
		pending := make(map[string]bool, len(items))
		for _, item := range items {
			pending[item.RequestID] = true
		}
		for id := range w.launched {
			if !pending[id] {
				delete(w.launched, id)
			}
		}
	}
	return runtimeRequestPollInterval
}

func (w *agentRuntimeRequestWorker) backoffWait() time.Duration {
	wait := runtimeRequestPollInterval
	for i := 1; i < w.pollFailures; i++ {
		wait *= 2
		if wait >= runtimeRequestBackoffCap {
			return runtimeRequestBackoffCap
		}
	}
	if wait > runtimeRequestBackoffCap {
		return runtimeRequestBackoffCap
	}
	return wait
}

func (w *agentRuntimeRequestWorker) listRequests(ctx context.Context, auth commentbus.DaemonAuth) ([]agentRuntimeRequestListItem, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(auth.BaseURL, "/")+"/daemon/runtime-requests", nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("X-Comment-CLI-Version", version)
	req.Header.Set(runtimeAuthHeaderName, runtimeAuthHeaderValue())
	req.Header.Set(runtimeRequestCapabilityHeader, runtimeRequestCapability)
	resp, err := runtimeRequestHTTPClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	var body struct {
		RuntimeRequests []agentRuntimeRequestListItem `json:"runtime_requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, resp.StatusCode, errors.New("runtime request list returned an unreadable response")
	}
	return body.RuntimeRequests, resp.StatusCode, nil
}

// processRequest launches one pending runtime request and acks the outcome. If
// a prior pass already launched it (ack didn't land), it skips the launch and
// retries the ack only — never relaunching a running session.
func (w *agentRuntimeRequestWorker) processRequest(ctx context.Context, auth commentbus.DaemonAuth, item agentRuntimeRequestListItem) {
	state := "started"
	failureMessage := ""
	if w.launched[item.RequestID] {
		// Already started on a previous pass; only the ack failed. Retry the ack.
	} else {
		w.logInfo("agent_runtime_request.launching", map[string]any{"request_id": item.RequestID, "handle": item.Handle})
		if launchErr := runtimeRequestLaunch(ctx, w.paths, item.Handle); launchErr != nil {
			state = "failed"
			failureMessage = launchErr.Error()
			w.logWarn("agent_runtime_request.launch_failed", map[string]any{"request_id": item.RequestID, "handle": item.Handle, "error": failureMessage})
		} else {
			if w.launched == nil {
				w.launched = map[string]bool{}
			}
			w.launched[item.RequestID] = true
			w.logInfo("agent_runtime_request.started", map[string]any{"request_id": item.RequestID, "handle": item.Handle})
		}
	}
	status, ackErr := w.postRuntimeRequestAck(ctx, auth, item.RequestID, state, failureMessage)
	if ackErr != nil {
		w.logWarn("agent_runtime_request.ack_failed", map[string]any{"request_id": item.RequestID, "error": ackErr.Error()})
		return
	}
	if status != http.StatusOK {
		w.logWarn("agent_runtime_request.ack_rejected", map[string]any{"request_id": item.RequestID, "status": status})
		return
	}
	// Ack landed (terminal on the server now) — forget the launched marker.
	delete(w.launched, item.RequestID)
}

func (w *agentRuntimeRequestWorker) postRuntimeRequestAck(ctx context.Context, auth commentbus.DaemonAuth, requestID, state, failureMessage string) (int, error) {
	payload := map[string]any{"state": state}
	if state == "failed" {
		payload["failure_message"] = failureMessage
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	url := strings.TrimRight(auth.BaseURL, "/") + "/daemon/runtime-requests/" + requestID + "/ack"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("X-Comment-CLI-Version", version)
	req.Header.Set(runtimeAuthHeaderName, runtimeAuthHeaderValue())
	req.Header.Set(runtimeRequestCapabilityHeader, runtimeRequestCapability)
	resp, err := runtimeRequestHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer drainAndClose(resp)
	return resp.StatusCode, nil
}

// launchAgentRuntimeDetached starts an already-installed agent's runtime without
// attaching a terminal: it re-invokes this same binary as
// `comment run <handle>` with COMMENT_IO_SKIP_ATTACH=1, which tells the bus
// daemon to start the managed session and returns promptly. A clean exit means
// the session started; a non-zero exit (captured output) is the failure detail.
//
// Both callers (the web "Start your agent" runtime-request worker and the
// mention-driven auto-launch) pass the daemon's resolved Paths; the launched
// `comment run` is pinned to paths.Home via COMMENT_IO_HOME so it resolves the
// SAME home/profiles/socket the daemon holds — even when the daemon was started
// as `comment bus run --home /custom` (a custom home set by flag, not via the
// COMMENT_IO_HOME env var the bare process inherits).
func launchAgentRuntimeDetached(ctx context.Context, paths commentbus.Paths, handle string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not resolve the comment binary: %w", err)
	}
	launchCtx, cancel := context.WithTimeout(ctx, runtimeRequestLaunchTimeout)
	defer cancel()
	cmd := exec.CommandContext(launchCtx, exe, "run", handle)
	cmd.Env = agentRuntimeLaunchEnv(paths)
	cmd.Stdin = nil
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if runErr := cmd.Run(); runErr != nil {
		msg := strings.TrimSpace(out.String())
		if msg == "" {
			msg = runErr.Error()
		}
		if len(msg) > 280 {
			msg = msg[:280]
		}
		return errors.New(msg)
	}
	return nil
}

// agentRuntimeLaunchEnv builds the environment for the detached `comment run`.
// It inherits the daemon's process environment, then forces COMMENT_IO_SKIP_ATTACH
// (no-attach start) and pins COMMENT_IO_HOME to the daemon's resolved home so a
// flag-set `--home` (which never lands in the inherited env) still reaches the
// launched runtime. A zero-value/empty home leaves the inherited COMMENT_IO_HOME
// (if any) untouched — the launched process falls back to the same default cascade
// the daemon used, so the runtime-request path with a bare Paths is unaffected.
//
// When we DO pin a home we also reconcile COMMENT_IO_BUS_TCP_ADDR to the daemon's
// OWN resolved dial address (paths.BusTCPAddr). That env var is the only bus-address
// override resolveCLIPaths keys off the home: it applies the dial address whenever
// the resolved COMMENT_IO_HOME matches the target home. We must keep the child
// dialing THIS daemon for the pinned home, and there are two cases:
//
//   - TCP daemon (paths.BusTCPAddr set, e.g. COMMENT_IO_BUS_DISABLE_UNIX=1 / a
//     caged deployment with no Unix socket): the TCP dial address is the ONLY way to
//     reach the bus. We SET COMMENT_IO_BUS_TCP_ADDR to paths.BusTCPAddr — the
//     daemon's actual dial address for the home it itself resolved — overriding any
//     stale inherited value from a DIFFERENT caged daemon. Unconditionally stripping
//     here would leave the child with no dial address, falling back to a nonexistent
//     Unix socket and breaking auto-start in TCP-only/caged deployments.
//   - Pure Unix daemon (paths.BusTCPAddr empty): the child reaches the pinned home
//     via its Unix socket, so we STRIP any inherited COMMENT_IO_BUS_TCP_ADDR — it
//     belongs to a parent's home and resolveCLIPaths would otherwise re-apply it to
//     the freshly pinned home, dialing the wrong daemon.
//
// With no home to pin we touch neither var, so the bare-Paths runtime-request case
// (the round-6 zero-value path) is unaffected.
func agentRuntimeLaunchEnv(paths commentbus.Paths) []string {
	home := strings.TrimSpace(paths.Home)
	busTCPAddr := strings.TrimSpace(paths.BusTCPAddr)
	base := os.Environ()
	out := make([]string, 0, len(base)+3)
	for _, entry := range base {
		key := entry
		if idx := strings.IndexRune(entry, '='); idx >= 0 {
			key = entry[:idx]
		}
		if key == "COMMENT_IO_SKIP_ATTACH" {
			continue
		}
		// Drop any inherited COMMENT_IO_HOME so the daemon's resolved home (set
		// below) is authoritative; keep it only when we have no home to pin.
		if key == "COMMENT_IO_HOME" && home != "" {
			continue
		}
		// When pinning a home, drop the inherited bus dial address so we can replace
		// it below with the daemon's own (TCP daemon) or leave it stripped (Unix
		// daemon). Either way the inherited value belongs to the parent's home and
		// resolveCLIPaths would otherwise re-apply it to the newly pinned home.
		if key == "COMMENT_IO_BUS_TCP_ADDR" && home != "" {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, "COMMENT_IO_SKIP_ATTACH=1")
	if home != "" {
		out = append(out, "COMMENT_IO_HOME="+home)
		// A TCP-only daemon (no Unix socket) is reachable ONLY via its dial address,
		// so pin the child to THIS daemon's address (paths.BusTCPAddr). For a pure
		// Unix daemon paths.BusTCPAddr is empty and the inherited address stays
		// stripped (above), so the child uses the pinned home's Unix socket.
		if busTCPAddr != "" {
			out = append(out, "COMMENT_IO_BUS_TCP_ADDR="+busTCPAddr)
		}
	}
	return out
}

func (w *agentRuntimeRequestWorker) logInfo(msg string, data map[string]any) {
	writeDaemonWorkerLog(w.paths, "agent.runtime_request", "info", msg, data)
}

func (w *agentRuntimeRequestWorker) logWarn(msg string, data map[string]any) {
	writeDaemonWorkerLog(w.paths, "agent.runtime_request", "warn", msg, data)
}
