//go:build darwin || linux

package main

import (
	"errors"
	"os"
	"syscall"
)

// ephemeralOwnedByUs reports whether the file is owned by the current uid. A
// store owned by another local user is insecure even at mode 0700 — its owner
// can plant or replace the bind/credential we would otherwise trust.
func ephemeralOwnedByUs(info os.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false // cannot determine ownership → treat as not ours
	}
	return st.Uid == uint32(os.Getuid())
}

// ephemeralPidAlive reports whether pid is a live process (signal 0). Used to
// recover a mint lock from a dead holder immediately (the shell helper does the
// same), so a mixed CLI/helper deployment agrees on liveness.
func ephemeralPidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM) // EPERM = exists but not signalable
}
