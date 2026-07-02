//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// TestBusStatusReportsRunningFromSocketProbe is the bug-3 regression: a daemon
// that is actually running must read as running from `comment bus status` even
// when the service manager can't confirm it (no launchctl/systemctl-managed unit
// — the systemd-less container case). Pre-fix, status derived liveness solely
// from the service manager, so a live daemon reported as down; the status JSON
// had no `running`/`socket_live` keys at all.
func TestBusStatusReportsRunningFromSocketProbe(t *testing.T) {
	home := privateTempHome(t, "comment-bus-live-")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}

	// With no daemon, the socket probe is negative and status is not running.
	if daemonSocketLive(paths) {
		t.Fatal("socket reported live with no daemon running")
	}
	assertBusStatusRunning(t, home, false)

	ctx, cancel := context.WithCancel(context.Background())
	daemon, err := commentbus.StartDaemon(ctx, commentbus.DaemonOptions{Paths: paths, Version: "test"})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = daemon.Close()
	}()

	// The daemon is live on the socket; the service manager still doesn't know it
	// (nothing was installed via launchctl/systemctl), yet status must say running.
	if !daemonSocketLive(paths) {
		t.Fatal("socket reported dead with a live daemon")
	}
	assertBusStatusRunning(t, home, true)
}

func assertBusStatusRunning(t *testing.T, home string, want bool) {
	t.Helper()
	out, err := captureRun(t, []string{"bus", "status", "--home", home})
	if err != nil {
		t.Fatalf("bus status: %v (out=%s)", err, out)
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatalf("parse bus status JSON: %v (out=%s)", err, out)
	}
	if status["socket_live"] != want {
		t.Fatalf("socket_live = %v, want %v (status=%s)", status["socket_live"], want, out)
	}
	if status["running"] != want {
		t.Fatalf("running = %v, want %v — status liveness must reflect the socket probe, not just the service manager (status=%s)", status["running"], want, out)
	}
	// When the daemon is running only because the socket probe reached it (the
	// service manager isn't tracking a manually-started daemon), every status path
	// — systemd, launchd, and the no-service-manager fallback — must explain why
	// in `message`. This guards the launchd path against silently dropping it.
	if want && status["loaded"] == false {
		msg, _ := status["message"].(string)
		if !strings.Contains(msg, "socket probe") {
			t.Fatalf("message = %q, want it to mention the socket probe explaining running:true (status=%s)", msg, out)
		}
	}
}
