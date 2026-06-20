//go:build windows

package commentsync

import (
	"io/fs"
	"os"

	"golang.org/x/sys/windows"
)

const windowsAllBytes = ^uint32(0)

func lockSyncFile(file *os.File) error {
	overlapped := new(windows.Overlapped)
	if err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK,
		0,
		windowsAllBytes,
		windowsAllBytes,
		overlapped,
	); err != nil {
		return &fs.PathError{Op: "LockFileEx", Path: file.Name(), Err: err}
	}
	return nil
}

func unlockSyncFile(file *os.File) error {
	overlapped := new(windows.Overlapped)
	if err := windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		windowsAllBytes,
		windowsAllBytes,
		overlapped,
	); err != nil {
		return &fs.PathError{Op: "UnlockFileEx", Path: file.Name(), Err: err}
	}
	return nil
}

func fileOwnedByCurrentUser(os.FileInfo) bool {
	return true
}

func fileHasSingleLink(os.FileInfo) bool {
	return true
}
