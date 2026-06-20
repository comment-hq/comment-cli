//go:build !darwin && !linux

package commentbus

import (
	"os"
	"path/filepath"
)

// acquireDaemonLock is a no-op stub on platforms where the daemon does not run
// (the daemon is unix-only — see peercred_unsupported.go). It opens the lock file
// so the caller's lifecycle (Close) works, but applies no advisory lock; flock is
// not available here. This exists only so the package cross-compiles (the CI
// compiles Windows test binaries).
func acquireDaemonLock(paths Paths) (*os.File, error) {
	lockPath := filepath.Join(paths.Home, "daemon.lock")
	return os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
}
