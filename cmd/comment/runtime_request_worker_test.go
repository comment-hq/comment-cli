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
