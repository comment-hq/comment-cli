package commentbus

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"
)

// defaultSocketWatchdogInterval is how often the running daemon re-checks that
// its Unix listening socket still exists on disk and is the file it bound. An
// Lstat every 15s is negligible overhead and bounds how long a daemon can sit
// alive-but-socketless (the wedged state that used to be terminal: a live
// process holding daemon.lock with no daemon.sock, which no client could reach
// and no fresh StartDaemon could replace because the lock was held).
const defaultSocketWatchdogInterval = 15 * time.Second

// defaultStartupInitTimeout bounds the store/capability init that runs after the
// singleton lock is acquired but before the socket is bound. If that init hangs
// (e.g. a wedged bind-mount filesystem), the daemon would otherwise hold the
// lock forever with no socket. On timeout StartDaemon returns an error and its
// deferred cleanup releases the lock, so a fresh start can take over.
const defaultStartupInitTimeout = 60 * time.Second

// defaultSocketRebindTimeout bounds a watchdog re-bind. Because Close joins the
// watchdog goroutine, an unbounded re-bind on a wedged filesystem would hang
// shutdown; on timeout the watchdog gives up (releasing the lock) so Close can
// always complete.
const defaultSocketRebindTimeout = 15 * time.Second

// socketRebindProbe, when non-nil, runs at the start of a watchdog re-bind.
// Test-only seam (guarded by the same mutex as startupInitProbe) to simulate a
// bind that hangs on a wedged filesystem.
func runSocketRebindProbe() {
	startupInitProbeMu.Lock()
	probe := socketRebindProbe
	startupInitProbeMu.Unlock()
	if probe != nil {
		probe()
	}
}

// swapSocketRebindProbe atomically installs fn as the re-bind probe and returns
// the previous one. Test-only.
func swapSocketRebindProbe(fn func()) func() {
	startupInitProbeMu.Lock()
	prev := socketRebindProbe
	socketRebindProbe = fn
	startupInitProbeMu.Unlock()
	return prev
}

var socketRebindProbe func()

// startupInitProbe, when non-nil, runs at the very start of the timeout-bounded
// startup init. Test-only seam to simulate a hang between lock acquisition and
// socket bind. Guarded by startupInitProbeMu because on timeout the init runs in
// a goroutine that can outlive the test that set it (which then restores it in
// cleanup), so the read and the restore must synchronize.
var (
	startupInitProbeMu sync.Mutex
	startupInitProbe   func()
)

func runStartupInitProbe() {
	startupInitProbeMu.Lock()
	probe := startupInitProbe
	startupInitProbeMu.Unlock()
	if probe != nil {
		probe()
	}
}

// swapStartupInitProbe atomically installs fn as the startup-init probe and
// returns the previous one, so a test can set it and restore it in cleanup
// without racing the timeout goroutine that reads it. Test-only.
func swapStartupInitProbe(fn func()) func() {
	startupInitProbeMu.Lock()
	prev := startupInitProbe
	startupInitProbe = fn
	startupInitProbeMu.Unlock()
	return prev
}

// bindUnixSocket creates the daemon's Unix listener at socketPath, tightens its
// permissions, and captures the bound socket's file identity so cleanup and the
// watchdog can tell "our socket" from a replacement. Shared by StartDaemon's
// initial bind and the watchdog's re-bind so both stay in lockstep.
func bindUnixSocket(socketPath string) (*net.UnixListener, os.FileInfo, error) {
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		// Release the fd WITHOUT unlinking by path. bindUnixSocket also runs in the
		// async re-bind path where the singleton lock may already be gone and this
		// path may belong to a fresh daemon; a path-based unlink here would delete
		// that daemon's live socket. Any file this failed bind left is a stale
		// socket the next daemon's prepareSocketPath cleans up under its own lock.
		closeListenerNoUnlink(l)
		return nil, nil, err
	}
	info, err := os.Lstat(socketPath)
	if err != nil {
		closeListenerNoUnlink(l)
		return nil, nil, err
	}
	return l, info, nil
}

// closeListenerNoUnlink closes a Unix listener's fd without unlinking its socket
// file by path (net.UnixListener.Close unlinks by path by default, which is
// unsafe when the path may since have been rebound by another daemon).
func closeListenerNoUnlink(l *net.UnixListener) {
	l.SetUnlinkOnClose(false)
	_ = l.Close()
}

// runWithStartupTimeout runs fn, returning its error, unless it fails to finish
// within timeout — in which case it returns a timeout error immediately but keeps
// waiting on fn in the background. If fn ultimately succeeds after the timeout,
// onLateSuccess is invoked so any resources fn opened (which nothing upstream will
// use, since StartDaemon already returned the timeout error) get cleaned up
// instead of leaked. Used to bound the post-lock / pre-listen init so a hang there
// can't strand the singleton lock with no socket. onLateSuccess reads whatever fn
// assigned, which is safe: the background <-done receive happens-after fn's writes.
func runWithStartupTimeout(timeout time.Duration, fn func() error, onLateSuccess func()) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		go func() {
			if err := <-done; err == nil && onLateSuccess != nil {
				onLateSuccess()
			}
		}()
		return fmt.Errorf("daemon startup init did not complete within %s (storage/capability init hung); releasing the singleton lock for a fresh start", timeout)
	}
}

// runSocketWatchdog periodically verifies the daemon's listening socket still
// exists on disk and is the file it bound. If the socket vanishes (or is
// replaced), it re-binds a fresh listener in place; if it cannot, it shuts the
// daemon down so the singleton lock releases and a fresh StartDaemon can win.
// No-op for a TCP-only daemon (no Unix socket to watch).
func (d *Daemon) runSocketWatchdog(ctx context.Context) {
	// Signal Close (which joins this) on every exit path, including the disabled
	// early-return below, so teardown never blocks waiting for a watchdog that
	// already stopped.
	if d.socketWatchdogDone != nil {
		defer close(d.socketWatchdogDone)
	}
	interval := d.socketWatchdogInterval
	if interval < 0 {
		// Explicitly disabled (tests use this to demonstrate the pre-fix wedge).
		return
	}
	if interval == 0 {
		interval = defaultSocketWatchdogInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				return
			}
			if d.socketHealthy() {
				continue
			}
			d.recoverLostSocket(ctx)
		}
	}
}

// socketHealthy reports whether the on-disk socket still matches the listener we
// bound. A TCP-only daemon (nil socketFileInfo) is always "healthy" here.
func (d *Daemon) socketHealthy() bool {
	d.socketMu.Lock()
	expected := d.socketFileInfo
	d.socketMu.Unlock()
	if expected == nil {
		return true
	}
	current, err := os.Lstat(d.paths.Socket)
	if err != nil {
		return false
	}
	return sameFileUnchanged(current, expected)
}

// recoverLostSocket re-binds the Unix listener after the socket file went
// missing. On success it swaps in the new listener (updating socketFileInfo so
// Close still removes the right file) and resumes serving; the old listener is
// closed with unlink-on-close disabled so it can't delete the freshly bound
// socket. On failure it cancels the daemon so the lock releases.
//
// The bind is bounded by a timeout: net.ListenUnix/os.Chmod/os.Lstat aren't
// context-cancellable, so on a wedged filesystem they could block forever — and
// because Close joins the watchdog goroutine, an unbounded bind here would hang
// the entire shutdown. On timeout we treat it like a bind failure (release the
// lock) and, if the bind finishes late, close the orphaned listener rather than
// installing it (which would be a torn write of d.listener after teardown).
func (d *Daemon) recoverLostSocket(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	timeout := d.socketRebindTimeout
	if timeout <= 0 {
		timeout = defaultSocketRebindTimeout
	}
	var newListener *net.UnixListener
	var newInfo os.FileInfo
	if err := runWithStartupTimeout(timeout, func() error {
		runSocketRebindProbe()
		l, info, err := bindUnixSocket(d.paths.Socket)
		if err != nil {
			return err
		}
		newListener = l
		newInfo = info
		return nil
	}, func() {
		// Late success after the timeout: shutdown already proceeded, so this
		// listener will never be installed — drop it. Safe to read newListener/
		// newInfo: the done-channel receive in runWithStartupTimeout happens-after
		// the assignments above. removeFile=false: the singleton lock may already be
		// released, so leave file cleanup to the next daemon's prepareSocketPath.
		//
		// Known accepted edge: if this stale file materializes in the narrow window
		// after a fresh daemon's prepareSocketPath probe but before its own bind,
		// that daemon's StartDaemon fails once with "socket already exists" and must
		// be retried (the retry's prepareSocketPath then clears it). This requires a
		// still-recovering filesystem during another daemon's startup — rare, and
		// one-shot/self-healing on retry — so we accept it rather than re-probing
		// immediately before every bind.
		d.closeOrphanListener(newListener, newInfo, false)
	}); err != nil {
		d.logger.warn("bus.socket_watchdog_rebind_failed", map[string]any{
			"socket": d.paths.Socket,
			"error":  err.Error(),
		})
		// Can't restore the socket in bounded time; release the singleton lock so a
		// fresh daemon (with a clean socket) can take over instead of wedging.
		if d.shutdownCancel != nil {
			d.shutdownCancel()
		}
		return
	}

	d.socketMu.Lock()
	if ctx.Err() != nil {
		// Shutting down concurrently — don't install a socket the teardown path
		// won't track. Drop the one we just bound. removeFile=true is safe here:
		// this runs synchronously in the watchdog goroutine, and Close is parked on
		// the watchdog join (it releases the singleton lock only after we return),
		// so no other daemon can own the path during this removal.
		d.socketMu.Unlock()
		d.closeOrphanListener(newListener, newInfo, true)
		return
	}
	old := d.listener
	d.listener = newListener
	d.socketFileInfo = newInfo
	d.socketMu.Unlock()

	if old != nil {
		// The old listener's path now resolves to the NEW socket; disable its
		// unlink-on-close so closing it can't delete the file we just bound.
		old.SetUnlinkOnClose(false)
		_ = old.Close()
	}
	go d.serveListener(newListener)
	d.logger.info("bus.socket_watchdog_rebound", map[string]any{"socket": d.paths.Socket})
}

// closeOrphanListener releases a listener we bound but won't install (a late
// re-bind after its timeout, or a bind aborted by concurrent shutdown). It closes
// the fd with unlink-on-close disabled — a bare Close unlinks by path and, in a
// restart race, could delete a *different* daemon's socket.
//
// removeFile must be true ONLY when the caller still provably holds the singleton
// lock (the synchronous shutdown-abort branch, where Close is parked on the
// watchdog join and hasn't released the lock yet). Then the identity-checked
// removeIfSameFile is race-free. In the async late-success path the lock may
// already be released and a fresh daemon may own the path, so removeFile is false
// and cleanup is left to that daemon's prepareSocketPath — an identity check
// there is atomic with its own bind because it runs under its own lock. (A
// non-atomic Lstat-then-Remove here, with the lock gone, could unlink the new
// daemon's socket.)
func (d *Daemon) closeOrphanListener(l *net.UnixListener, info os.FileInfo, removeFile bool) {
	if l == nil {
		return
	}
	closeListenerNoUnlink(l)
	if removeFile {
		_ = removeIfSameFile(d.paths.Socket, info)
	}
}
