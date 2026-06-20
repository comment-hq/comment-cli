//go:build !darwin && !linux

package main

import "os"

// On non-Unix platforms (Windows) the POSIX uid ownership model does not apply;
// rely on the permission check and ACLs instead.
func ephemeralOwnedByUs(os.FileInfo) bool { return true }

// ephemeralPidAlive can't cheaply/portably check process liveness on Windows;
// assume alive so lock recovery falls back to age-based expiry.
func ephemeralPidAlive(int) bool { return true }
