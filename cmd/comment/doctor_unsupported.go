//go:build !darwin && !linux

package main

import "os"

// validateOwnerIsCurrentUser is a no-op on platforms where doctor checks
// don't run end-to-end. The bus install paths refuse to run on
// non-darwin/linux systems, so doctor's persistence checks short-circuit
// before this is reached.
func validateOwnerIsCurrentUser(_ os.FileInfo) error {
	return nil
}
