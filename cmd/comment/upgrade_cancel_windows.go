//go:build windows

package main

import (
	"os"
	"os/exec"
	"strconv"
)

func upgradeShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func configureUpgradeCommandCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run()
		_ = cmd.Process.Kill()
		return nil
	}
}
