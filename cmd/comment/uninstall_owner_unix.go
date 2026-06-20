//go:build darwin || linux

package main

import "os"

func validateUninstallRemovalOwner(info os.FileInfo) error {
	return validateOwnerIsCurrentUser(info)
}
