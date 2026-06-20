//go:build !windows

package commentbus

import (
	"io/fs"
	"os"

	"golang.org/x/sys/unix"
)

func openBotletsRegistryRepairLock(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func lockBotletsRegistryRepairFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX)
}

func unlockBotletsRegistryRepairFile(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
