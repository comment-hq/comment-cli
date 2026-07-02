//go:build darwin || linux

package commentbus

import (
	"errors"
	"syscall"
)

// isTransientAcceptError reports whether a listener Accept error is a transient,
// recoverable condition rather than the listener being closed. These are the
// fd/interrupt errors the accept loop should back off and retry on instead of
// abandoning the socket (a dead accept loop leaves the socket file present but
// unserved — a wedge the file-identity watchdog can't detect).
func isTransientAcceptError(err error) bool {
	return errors.Is(err, syscall.EMFILE) ||
		errors.Is(err, syscall.ENFILE) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EINTR)
}
