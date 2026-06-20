//go:build windows

package commentbus

import (
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

const botletsRegistryRepairLockAllBytes = ^uint32(0)

func openBotletsRegistryRepairLock(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrInvalid}
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	statInfo, statErr := file.Stat()
	lstatInfo, lstatErr := os.Lstat(path)
	if statErr != nil || lstatErr != nil || lstatInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(statInfo, lstatInfo) {
		_ = file.Close()
		if statErr != nil {
			return nil, statErr
		}
		if lstatErr != nil {
			return nil, lstatErr
		}
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrInvalid}
	}
	return file, nil
}

func lockBotletsRegistryRepairFile(file *os.File) error {
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		botletsRegistryRepairLockAllBytes,
		botletsRegistryRepairLockAllBytes,
		overlapped,
	); err != nil {
		return &fs.PathError{Op: "LockFileEx", Path: file.Name(), Err: err}
	}
	return nil
}

func unlockBotletsRegistryRepairFile(file *os.File) error {
	overlapped := new(windows.Overlapped)
	if err := windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		botletsRegistryRepairLockAllBytes,
		botletsRegistryRepairLockAllBytes,
		overlapped,
	); err != nil {
		return &fs.PathError{Op: "UnlockFileEx", Path: file.Name(), Err: err}
	}
	return nil
}
