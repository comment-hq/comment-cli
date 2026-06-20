//go:build darwin || linux

package main

import (
	"errors"
	"os"
	"syscall"
)

// validateOwnerIsCurrentUser checks that the given file is owned by the
// effective user. Returns nil if owned by the current user; an error
// otherwise.
func validateOwnerIsCurrentUser(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("could not inspect file owner")
	}
	if uint32(os.Geteuid()) != stat.Uid {
		return errors.New("owner mismatch")
	}
	return nil
}
