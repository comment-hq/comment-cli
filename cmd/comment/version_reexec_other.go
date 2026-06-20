//go:build !unix

package main

import "errors"

// reexecComment is unsupported off unix; the caller falls back to asking the
// user to re-run their command after the update installs.
func reexecComment(bin string, argv []string, env []string) error {
	return errors.New("re-exec is not supported on this platform")
}
