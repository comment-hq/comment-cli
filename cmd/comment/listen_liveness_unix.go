//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

// processIsAlive reports whether a process with the given pid currently exists,
// using the POSIX signal-0 liveness probe (no signal is delivered). A nil error
// means the process exists; EPERM means it exists but is owned by another user
// (still alive). Used to tell a live `comment listen <handle>` launcher (token
// launch-<pid>) apart from one that has exited, so the rewake loop only re-claims
// for a launcher that is still running.
func processIsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
