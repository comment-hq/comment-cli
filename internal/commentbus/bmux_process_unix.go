//go:build darwin || linux

package commentbus

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func configureDetachedBmuxCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateDetachedBmuxCommand(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

func terminatePersistedBmuxChildProcessGroup(pid uint32, pgid uint32, identity string) error {
	if pid == 0 || pgid == 0 || identity == "" {
		return ErrTmuxSessionMissing
	}
	currentIdentity, err := bmuxProcessIdentityFn(pid)
	if err != nil {
		if errors.Is(err, ErrTmuxSessionMissing) {
			return ErrTmuxSessionMissing
		}
		return err
	}
	if currentIdentity != identity {
		return ErrTmuxSessionMissing
	}
	currentPGID, err := syscall.Getpgid(int(pid))
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return ErrTmuxSessionMissing
		}
		return err
	}
	if currentPGID != int(pgid) {
		return ErrTmuxSessionMissing
	}
	signaled := false
	for i, signal := range []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM, syscall.SIGKILL} {
		err := syscall.Kill(-int(pgid), signal)
		if err == nil {
			signaled = true
			if i < 2 {
				time.Sleep(100 * time.Millisecond)
			}
			continue
		}
		if errors.Is(err, syscall.ESRCH) {
			if signaled {
				return nil
			}
			return ErrTmuxSessionMissing
		}
		return err
	}
	return nil
}
