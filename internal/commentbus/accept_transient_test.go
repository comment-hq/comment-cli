//go:build darwin || linux

package commentbus

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestIsTransientAcceptError(t *testing.T) {
	transient := []error{
		&net.OpError{Op: "accept", Err: syscall.EMFILE},
		&net.OpError{Op: "accept", Err: syscall.ENFILE},
		&net.OpError{Op: "accept", Err: syscall.ECONNABORTED},
		syscall.EINTR,
	}
	for _, err := range transient {
		if !isTransientAcceptError(err) {
			t.Errorf("isTransientAcceptError(%v) = false, want true", err)
		}
	}
	permanent := []error{
		net.ErrClosed,
		errors.New("boom"),
		&net.OpError{Op: "accept", Err: syscall.EINVAL},
	}
	for _, err := range permanent {
		if isTransientAcceptError(err) {
			t.Errorf("isTransientAcceptError(%v) = true, want false", err)
		}
	}
}

// scriptedListener returns the queued Accept results in order; once exhausted it
// returns net.ErrClosed (a permanent error) so serveListener exits.
type scriptedListener struct {
	mu    sync.Mutex
	calls int
	errs  []error
}

func (s *scriptedListener) Accept() (net.Conn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i < len(s.errs) {
		return nil, s.errs[i]
	}
	return nil, net.ErrClosed
}

func (s *scriptedListener) Close() error   { return nil }
func (s *scriptedListener) Addr() net.Addr { return &net.UnixAddr{Name: "scripted", Net: "unix"} }

func (s *scriptedListener) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// TestServeListenerRetriesTransientAcceptError proves serveListener does not
// abandon its accept loop on a transient error (which would strand the socket
// unserved-but-present) — it backs off, retries, and only exits on a permanent
// (listener-closed) error.
func TestServeListenerRetriesTransientAcceptError(t *testing.T) {
	d := &Daemon{baseCtx: context.Background()} // nil logger is nil-safe
	l := &scriptedListener{errs: []error{
		&net.OpError{Op: "accept", Err: syscall.EMFILE},
		&net.OpError{Op: "accept", Err: syscall.EMFILE},
	}}

	done := make(chan struct{})
	go func() {
		d.serveListener(l)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serveListener did not exit after the permanent error")
	}

	// 2 transient retries + the final ErrClosed = at least 3 Accept calls. If it
	// had exited on the first transient error (the bug), callCount would be 1.
	if got := l.callCount(); got < 3 {
		t.Fatalf("serveListener made %d Accept calls; expected it to retry transient errors (>=3)", got)
	}
}

// TestServeListenerExitsOnContextCancelDuringBackoff ensures a daemon shutdown
// during accept backoff doesn't keep the loop alive.
func TestServeListenerExitsOnContextCancelDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	d := &Daemon{baseCtx: ctx}
	// Endless transient errors: only ctx cancellation should end the loop.
	l := &scriptedListener{errs: make([]error, 1_000_000)}
	for i := range l.errs {
		l.errs[i] = &net.OpError{Op: "accept", Err: syscall.EMFILE}
	}

	done := make(chan struct{})
	go func() {
		d.serveListener(l)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("serveListener did not exit on context cancellation during backoff")
	}
}
