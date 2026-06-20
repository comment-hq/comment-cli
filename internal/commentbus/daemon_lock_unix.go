//go:build darwin || linux

package commentbus

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquireDaemonLock takes an exclusive, non-blocking advisory lock (flock) on a
// lock file in the home dir. The returned *os.File must be kept open for the
// daemon's lifetime; closing it releases the lock. This is the transport-
// independent singleton guard: a second daemon (Unix or TCP-only, any port)
// against the same home fails to acquire it. On a single kernel this is reliable;
// across a virtiofs bind-mount boundary (macOS Docker Desktop / Colima) it is
// best-effort.
func acquireDaemonLock(paths Paths) (*os.File, error) {
	lockPath := filepath.Join(paths.Home, "daemon.lock")
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("another comment daemon appears to be running for home %s (could not acquire %s): %w", paths.Home, lockPath, err)
	}
	return f, nil
}
