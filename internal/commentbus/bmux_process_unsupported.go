//go:build !darwin && !linux

package commentbus

import "os/exec"

func configureDetachedBmuxCommand(cmd *exec.Cmd) {}

func terminateDetachedBmuxCommand(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func terminatePersistedBmuxChildProcessGroup(uint32, uint32, string) error {
	return ErrTmuxSessionMissing
}
