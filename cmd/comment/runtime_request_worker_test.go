//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

const testRuntimeRequestDaemonToken = "ldt_runtime-test-token"

// newRuntimeRequestTestServer serves the two daemon endpoints the worker calls:
// GET /daemon/runtime-requests (returns the supplied pending list) and
// POST /daemon/runtime-requests/{id}/ack (records the ack body + path).
func newRuntimeRequestTestServer(t *testing.T, pending []agentRuntimeRequestListItem) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var mu sync.Mutex
	acks := []map[string]any{}
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/runtime-requests", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get(runtimeRequestCapabilityHeader); got != runtimeRequestCapability {
			t.Errorf("list %s = %q, want %q", runtimeRequestCapabilityHeader, got, runtimeRequestCapability)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"runtime_requests": pending})
	})
	mux.HandleFunc("/daemon/runtime-requests/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/ack") {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get(runtimeRequestCapabilityHeader); got != runtimeRequestCapability {
			t.Errorf("ack %s = %q, want %q", runtimeRequestCapabilityHeader, got, runtimeRequestCapability)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body == nil {
			body = map[string]any{}
		}
		body["__path"] = r.URL.Path
		mu.Lock()
		acks = append(acks, body)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, &acks
}

func TestRuntimeRequestWorkerLaunchesAndAcksStarted(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	var launched []string
	prev := runtimeRequestLaunch
	runtimeRequestLaunch = func(_ context.Context, _ commentbus.Paths, handle string) error {
		launched = append(launched, handle)
		return nil
	}
	t.Cleanup(func() { runtimeRequestLaunch = prev })

	pending := []agentRuntimeRequestListItem{{RequestID: "rtr_abc", State: "pending", AgentID: "ag_1", Handle: "max.reviewer", DaemonID: "ld_worker-test"}}
	server, acks := newRuntimeRequestTestServer(t, pending)
	writeEnrollmentDaemonAuth(t, paths, server.URL, testRuntimeRequestDaemonToken)
	worker := &agentRuntimeRequestWorker{paths: paths}

	if wait := worker.runOnce(context.Background()); wait != runtimeRequestPollInterval {
		t.Fatalf("wait = %v, want %v", wait, runtimeRequestPollInterval)
	}
	if len(launched) != 1 || launched[0] != "max.reviewer" {
		t.Fatalf("launched = %v, want [max.reviewer]", launched)
	}
	if len(*acks) != 1 {
		t.Fatalf("acks = %v, want 1", *acks)
	}
	if (*acks)[0]["state"] != "started" {
		t.Fatalf("ack state = %#v, want started", (*acks)[0])
	}
	if path, _ := (*acks)[0]["__path"].(string); !strings.HasSuffix(path, "/rtr_abc/ack") {
		t.Fatalf("ack path = %q, want .../rtr_abc/ack", path)
	}
}

func TestRuntimeRequestWorkerAcksFailedOnLaunchError(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	prev := runtimeRequestLaunch
	runtimeRequestLaunch = func(_ context.Context, _ commentbus.Paths, _ string) error {
		return errors.New("runtime not found")
	}
	t.Cleanup(func() { runtimeRequestLaunch = prev })

	pending := []agentRuntimeRequestListItem{{RequestID: "rtr_def", State: "pending", Handle: "max.flaky", DaemonID: "ld_worker-test"}}
	server, acks := newRuntimeRequestTestServer(t, pending)
	writeEnrollmentDaemonAuth(t, paths, server.URL, testRuntimeRequestDaemonToken)
	worker := &agentRuntimeRequestWorker{paths: paths}

	worker.runOnce(context.Background())
	if len(*acks) != 1 || (*acks)[0]["state"] != "failed" {
		t.Fatalf("acks = %#v, want one failed", *acks)
	}
	if (*acks)[0]["failure_message"] != "runtime not found" {
		t.Fatalf("failure_message = %#v, want \"runtime not found\"", (*acks)[0])
	}
}

func TestRuntimeRequestWorkerDoesNotRelaunchAfterAckFailure(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	launchCount := 0
	prev := runtimeRequestLaunch
	runtimeRequestLaunch = func(_ context.Context, _ commentbus.Paths, _ string) error {
		launchCount++
		return nil
	}
	t.Cleanup(func() { runtimeRequestLaunch = prev })

	// The server keeps listing the same pending request and 500s every ack, so
	// without the launched-marker guard the worker would relaunch each pass.
	ackCalls := 0
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/runtime-requests", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"runtime_requests": []agentRuntimeRequestListItem{
			{RequestID: "rtr_stuck", State: "pending", Handle: "max.stuck", DaemonID: "ld_worker-test"},
		}})
	})
	mux.HandleFunc("/daemon/runtime-requests/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ackCalls++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	writeEnrollmentDaemonAuth(t, paths, server.URL, testRuntimeRequestDaemonToken)
	worker := &agentRuntimeRequestWorker{paths: paths}

	worker.runOnce(context.Background()) // launch + ack(500)
	worker.runOnce(context.Background()) // ack retried, must NOT relaunch
	worker.runOnce(context.Background())
	if launchCount != 1 {
		t.Fatalf("launchCount = %d, want 1 (a running session must not be relaunched after an ack failure)", launchCount)
	}
	if ackCalls < 3 {
		t.Fatalf("ackCalls = %d, want >= 3 (the ack must keep being retried)", ackCalls)
	}
}

func TestRuntimeRequestWorkerNoopsWhenUnpaired(t *testing.T) {
	paths := testAgentEnrollmentPaths(t)
	called := false
	prev := runtimeRequestLaunch
	runtimeRequestLaunch = func(_ context.Context, _ commentbus.Paths, _ string) error {
		called = true
		return nil
	}
	t.Cleanup(func() { runtimeRequestLaunch = prev })

	worker := &agentRuntimeRequestWorker{paths: paths}
	if wait := worker.runOnce(context.Background()); wait != runtimeRequestPairingRecheckInterval {
		t.Fatalf("wait = %v, want pairing recheck", wait)
	}
	if called {
		t.Fatal("launch must not be called while unpaired")
	}
}

func envCount(env []string, key string) int {
	prefix := key + "="
	n := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			n++
		}
	}
	return n
}

// The detached `comment run` must carry the daemon's resolved home (finding 2):
// a daemon started with `comment bus run --home /custom` has paths.Home=/custom
// but no COMMENT_IO_HOME in its inherited env, so the launch must pin
// COMMENT_IO_HOME=/custom or the launched runtime resolves the DEFAULT home and
// never finds the bot's profile/socket. This guards BOTH callers
// (mention-auto-launch and the runtime-request worker), which both pass the
// daemon's Paths through launchAgentRuntimeDetached -> agentRuntimeLaunchEnv.
func TestAgentRuntimeLaunchEnvPinsDaemonHome(t *testing.T) {
	t.Setenv("COMMENT_IO_HOME", "/inherited/home")
	t.Setenv("COMMENT_IO_SKIP_ATTACH", "")

	env := agentRuntimeLaunchEnv(commentbus.Paths{Home: "/custom/home"})

	if got := envValue(env, "COMMENT_IO_HOME"); got != "/custom/home" {
		t.Fatalf("COMMENT_IO_HOME = %q, want /custom/home (the daemon's resolved home)", got)
	}
	if n := envCount(env, "COMMENT_IO_HOME"); n != 1 {
		t.Fatalf("COMMENT_IO_HOME appears %d times, want exactly 1 (inherited value must be dropped)", n)
	}
	if got := envValue(env, "COMMENT_IO_SKIP_ATTACH"); got != "1" {
		t.Fatalf("COMMENT_IO_SKIP_ATTACH = %q, want 1", got)
	}
	if n := envCount(env, "COMMENT_IO_SKIP_ATTACH"); n != 1 {
		t.Fatalf("COMMENT_IO_SKIP_ATTACH appears %d times, want exactly 1", n)
	}
}

// With no home to pin (zero-value Paths, e.g. the runtime-request worker before
// this change relied on inherited env), the inherited COMMENT_IO_HOME is left
// untouched so the launched runtime keeps the daemon's default cascade. This
// proves the change is ADDITIVE and does not regress the runtime-request path.
func TestAgentRuntimeLaunchEnvPreservesInheritedHomeWhenUnset(t *testing.T) {
	t.Setenv("COMMENT_IO_HOME", "/inherited/home")

	env := agentRuntimeLaunchEnv(commentbus.Paths{})

	if got := envValue(env, "COMMENT_IO_HOME"); got != "/inherited/home" {
		t.Fatalf("COMMENT_IO_HOME = %q, want inherited /inherited/home", got)
	}
	if n := envCount(env, "COMMENT_IO_HOME"); n != 1 {
		t.Fatalf("COMMENT_IO_HOME appears %d times, want exactly 1", n)
	}
	if got := envValue(env, "COMMENT_IO_SKIP_ATTACH"); got != "1" {
		t.Fatalf("COMMENT_IO_SKIP_ATTACH = %q, want 1", got)
	}
}

// Unix-mode pin (paths.BusTCPAddr empty): the daemon has a Unix socket for the
// pinned home, so the inherited COMMENT_IO_BUS_TCP_ADDR must be DROPPED. That env
// var is the only bus dial address resolveCLIPaths keys off the home: it re-applies
// the dial address whenever the resolved COMMENT_IO_HOME matches the target home.
// Leaving a parent's address in place would make the child `comment run` resolve
// the freshly pinned home yet still DIAL the stale TCP address of a DIFFERENT caged
// daemon. Dropping it lets the child reach the pinned home's Unix socket.
func TestAgentRuntimeLaunchEnvDropsInheritedBusTCPAddrWhenPinningUnixHome(t *testing.T) {
	t.Setenv("COMMENT_IO_HOME", "/inherited/home")
	t.Setenv("COMMENT_IO_BUS_TCP_ADDR", "127.0.0.1:9999")

	// No BusTCPAddr on the resolved Paths == a pure Unix daemon.
	env := agentRuntimeLaunchEnv(commentbus.Paths{Home: "/custom/home"})

	if got := envValue(env, "COMMENT_IO_HOME"); got != "/custom/home" {
		t.Fatalf("COMMENT_IO_HOME = %q, want /custom/home", got)
	}
	if n := envCount(env, "COMMENT_IO_BUS_TCP_ADDR"); n != 0 {
		t.Fatalf("COMMENT_IO_BUS_TCP_ADDR appears %d times, want 0 (inherited stale dial address must be dropped for a Unix daemon)", n)
	}
	if got := envValue(env, "COMMENT_IO_SKIP_ATTACH"); got != "1" {
		t.Fatalf("COMMENT_IO_SKIP_ATTACH = %q, want 1", got)
	}
}

// TCP-only pin (paths.BusTCPAddr set, e.g. COMMENT_IO_BUS_DISABLE_UNIX=1 — the
// caged daemon deliberately created NO Unix socket): the TCP dial address is the
// ONLY way to reach the bus, so the child MUST carry COMMENT_IO_BUS_TCP_ADDR set to
// the daemon's own resolved address (paths.BusTCPAddr), OVERRIDING any stale
// inherited value from a different caged daemon. Stripping it (as the Unix case
// does) would leave the child with no dial address — falling back to a nonexistent
// Unix socket — and break web "Start your agent" + mention auto-start in TCP-only
// deployments.
func TestAgentRuntimeLaunchEnvPinsDaemonBusTCPAddrWhenPinningTCPHome(t *testing.T) {
	t.Setenv("COMMENT_IO_HOME", "/inherited/home")
	t.Setenv("COMMENT_IO_BUS_TCP_ADDR", "127.0.0.1:9999") // stale inherited address

	env := agentRuntimeLaunchEnv(commentbus.Paths{Home: "/custom/home", BusTCPAddr: "127.0.0.1:7700"})

	if got := envValue(env, "COMMENT_IO_HOME"); got != "/custom/home" {
		t.Fatalf("COMMENT_IO_HOME = %q, want /custom/home", got)
	}
	if got := envValue(env, "COMMENT_IO_BUS_TCP_ADDR"); got != "127.0.0.1:7700" {
		t.Fatalf("COMMENT_IO_BUS_TCP_ADDR = %q, want 127.0.0.1:7700 (the daemon's own dial address, overriding the stale inherited 127.0.0.1:9999)", got)
	}
	if n := envCount(env, "COMMENT_IO_BUS_TCP_ADDR"); n != 1 {
		t.Fatalf("COMMENT_IO_BUS_TCP_ADDR appears %d times, want exactly 1 (the stale inherited value must be replaced, not duplicated)", n)
	}
	if got := envValue(env, "COMMENT_IO_SKIP_ATTACH"); got != "1" {
		t.Fatalf("COMMENT_IO_SKIP_ATTACH = %q, want 1", got)
	}
}

// With no home to pin (zero-value Paths — the round-6 runtime-request path that
// relies on the inherited env), the inherited COMMENT_IO_BUS_TCP_ADDR is left
// untouched: nothing is being re-homed, so the launched runtime keeps the parent's
// dial address. This proves the bus-address strip is gated on actually pinning a
// home and stays additive/safe for the bare-Paths case.
func TestAgentRuntimeLaunchEnvPreservesInheritedBusTCPAddrWhenHomeUnset(t *testing.T) {
	t.Setenv("COMMENT_IO_HOME", "/inherited/home")
	t.Setenv("COMMENT_IO_BUS_TCP_ADDR", "127.0.0.1:9999")

	env := agentRuntimeLaunchEnv(commentbus.Paths{})

	if got := envValue(env, "COMMENT_IO_BUS_TCP_ADDR"); got != "127.0.0.1:9999" {
		t.Fatalf("COMMENT_IO_BUS_TCP_ADDR = %q, want inherited 127.0.0.1:9999 (nothing to re-home)", got)
	}
	if n := envCount(env, "COMMENT_IO_BUS_TCP_ADDR"); n != 1 {
		t.Fatalf("COMMENT_IO_BUS_TCP_ADDR appears %d times, want exactly 1", n)
	}
}
