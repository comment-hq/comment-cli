//go:build !unix && !windows

package main

import (
	"os"
	"os/exec"
)

func upgradeShutdownSignals() []os.Signal {
	return []os.Signal{os.Interrupt}
}

func configureUpgradeCommandCancel(_ *exec.Cmd) {}
