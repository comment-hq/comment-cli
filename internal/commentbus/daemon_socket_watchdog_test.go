//go:build darwin || linux

package commentbus

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// daemonHealthOK dials the Unix socket and issues a health op, returning whether
// the daemon answered OK. Unlike requestDaemon it never t.Fatals — the socket is
// expected to be absent at points in these tests.
func daemonHealthOK(t *testing.T, paths Paths) bool {
	t.Helper()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: paths.Socket, Net: "unix"})
	if err != nil {
		return false
	}
	defer conn.Close()
	payload, err := json.Marshal(map[string]any{"id": "req_wdhealth", "op": "health", "params": map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return false
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return false
	}
	var response SocketResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return false
	}
	return response.OK
}

// conditionHoldsWithin polls cond until it returns true or the timeout elapses,
// reporting whether it ever became true. Non-fatal (unlike waitForConditionWithin)
// so a test can assert a condition never becomes true.
func conditionHoldsWithin(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestDaemonSocketWatchdogRebindsVanishedSocket is the bug-1 regression: a live
// daemon whose listening socket disappears from disk must re-bind it in place
// and keep serving, instead of wedging alive-but-socketless (lock held, no
// socket) — the state that used to be unrecoverable without killing the process.
func TestDaemonSocketWatchdogRebindsVanishedSocket(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths := testDaemonPaths(t)
	daemon, err := startDaemonForTest(t, ctx, DaemonOptions{
		Paths:                  paths,
		Version:                "test",
		SocketWatchdogInterval: 15 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	if !daemonHealthOK(t, paths) {
		t.Fatal("expected a healthy socket at start")
	}
	// The socket file vanishes out from under the live daemon.
	if err := os.Remove(paths.Socket); err != nil {
		t.Fatal(err)
	}
	// The watchdog must re-bind and restore reachability.
	waitForConditionWithin(t, "watchdog re-binds the vanished socket", 3*time.Second, func() bool {
		return daemonHealthOK(t, paths)
	})
}

// TestDaemonWithoutSocketWatchdogStaysWedged characterizes the pre-fix bug: with
// the watchdog disabled, removing the socket leaves the daemon alive-but-mute and
// it never recovers on its own. This is the exact wedge observed in production
// (PID holding daemon.lock, no daemon.sock) and proves the watchdog is
// load-bearing, not incidental.
func TestDaemonWithoutSocketWatchdogStaysWedged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths := testDaemonPaths(t)
	daemon, err := startDaemonForTest(t, ctx, DaemonOptions{
		Paths:                  paths,
		Version:                "test",
		SocketWatchdogInterval: -1, // disabled
	})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	if !daemonHealthOK(t, paths) {
		t.Fatal("expected a healthy socket at start")
	}
	if err := os.Remove(paths.Socket); err != nil {
		t.Fatal(err)
	}
	// No watchdog: the socket stays gone. Give any (hypothetical) recovery ample
	// time; it must not come back.
	if conditionHoldsWithin(300*time.Millisecond, func() bool { return daemonHealthOK(t, paths) }) {
		t.Fatal("socket unexpectedly recovered with the watchdog disabled")
	}
}

// TestDaemonStartupInitTimeoutReleasesLock is the bug-2 regression: if the init
// between lock-acquire and socket-bind hangs, StartDaemon must time out and
// release the singleton lock rather than parking the daemon in the wedged state.
// TestDaemonCloseNotBlockedByHungRebind proves the Close→watchdog join can't hang
// forever if a re-bind blocks on a wedged filesystem: the re-bind is timeout-
// bounded, so the watchdog always returns and Close always completes.
func TestDaemonCloseNotBlockedByHungRebind(t *testing.T) {
	paths := testDaemonPaths(t)

	// Make every re-bind hang until we release it, simulating a wedged FS where
	// net.ListenUnix/chmod/lstat never return.
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	prev := swapSocketRebindProbe(func() { <-release })
	t.Cleanup(func() { swapSocketRebindProbe(prev); unblock() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	daemon, err := startDaemonForTest(t, ctx, DaemonOptions{
		Paths:                  paths,
		Version:                "test",
		SocketWatchdogInterval: 15 * time.Millisecond,
		SocketRebindTimeout:    60 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Trigger recovery: remove the socket so the watchdog enters the (hung) re-bind.
	if err := os.Remove(paths.Socket); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond) // let the watchdog get into the hung re-bind

	// Close must complete even though a re-bind is stuck — bounded by the rebind
	// timeout, not blocked forever.
	closed := make(chan error, 1)
	go func() { closed <- daemon.Close() }()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung waiting to join a watchdog stuck in a re-bind")
	}
	unblock() // let the leaked bind goroutine finish before the test ends
}

func TestDaemonStartupInitTimeoutReleasesLock(t *testing.T) {
	paths := testDaemonPaths(t)

	// A slow (not permanently wedged) init: it finishes shortly AFTER the startup
	// timeout, exercising the late-success cleanup path (the opened store/logger
	// must be closed, not leaked). The probe is read once when the init goroutine
	// starts, so clearing it after the timeout doesn't affect the in-flight run.
	prev := swapStartupInitProbe(func() { time.Sleep(120 * time.Millisecond) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := startDaemonForTest(t, ctx, DaemonOptions{
		Paths:              paths,
		Version:            "test",
		StartupInitTimeout: 20 * time.Millisecond,
	})
	swapStartupInitProbe(prev)
	if err == nil {
		t.Fatal("expected a startup-init timeout error")
	}
	if !strings.Contains(err.Error(), "startup init did not complete") {
		t.Fatalf("unexpected error: %v", err)
	}

	// The lock must have been released so a fresh daemon can take over.
	lock, lerr := acquireDaemonLock(paths)
	if lerr != nil {
		t.Fatalf("singleton lock not released after startup timeout: %v", lerr)
	}
	_ = lock.Close()

	// Let the slow init finish so its late-success cleanup runs (under -race this
	// also proves the store/logger hand-off to the cleanup is synchronized).
	time.Sleep(250 * time.Millisecond)

	// A fresh daemon then starts cleanly — paths are usable, nothing left wedged.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	daemon, err := startDaemonForTest(t, ctx2, DaemonOptions{Paths: paths, Version: "test"})
	if err != nil {
		t.Fatalf("fresh daemon start after startup timeout failed: %v", err)
	}
	defer daemon.Close()
	if !daemonHealthOK(t, paths) {
		t.Fatal("fresh daemon not healthy after a prior startup timeout")
	}
}
