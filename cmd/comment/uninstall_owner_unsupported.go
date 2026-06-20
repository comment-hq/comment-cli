//go:build !darwin && !linux

package main

import (
	"errors"
	"os"
)

func validateUninstallRemovalOwner(_ os.FileInfo) error {
	return errors.New("cannot verify path ownership on this platform")
}
