//go:build !darwin && !linux

package commentbus

// isTransientAcceptError is a no-op on non-unix platforms: the daemon only runs
// on darwin/linux, and this stub keeps the package cross-compilable (e.g. the
// Windows test-binary build) without referencing unix-only errnos.
func isTransientAcceptError(err error) bool {
	return false
}
