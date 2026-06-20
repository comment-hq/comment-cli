//go:build !unix

package main

// processIsAlive is unix-only (the launcher/hook flow is POSIX). On other
// platforms, conservatively assume a launcher is still alive so the rewake loop
// does not wrongly disarm a listener it cannot probe.
func processIsAlive(pid int) bool {
	return pid > 0
}
