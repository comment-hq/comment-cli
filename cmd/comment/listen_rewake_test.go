//go:build unix

// Unix-only: the launcher liveness probe (and the dead-pid expectations below)
// rely on the POSIX signal-0 implementation in listen_liveness_unix.go; the
// non-unix stub treats any positive pid as alive.

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

func TestStillWantsToListen(t *testing.T) {
	home := t.TempDir()
	paths := commentbus.Paths{Home: home}
	rewakeDir := filepath.Join(home, "rewake")
	if err := os.MkdirAll(rewakeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const profile = "max.alice"

	// Empty session: never wants to listen.
	if stillWantsToListen(paths, profile, "") {
		t.Fatal("empty listen session should not want to listen")
	}

	// Impromptu: gated on the binding file present AND naming this profile.
	const ccSession = "cc-session-abc"
	bind := filepath.Join(rewakeDir, "bind-"+ccSession)
	if stillWantsToListen(paths, profile, ccSession) {
		t.Fatal("impromptu session without a binding file should not want to listen")
	}
	if err := os.WriteFile(bind, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	if !stillWantsToListen(paths, profile, ccSession) {
		t.Fatal("impromptu session with a matching binding file should want to listen")
	}
	// Detach-A-then-attach-B: the binding now names a different handle, so the old
	// handle's listener must NOT re-claim.
	if err := os.WriteFile(bind, []byte("max.bob"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stillWantsToListen(paths, profile, ccSession) {
		t.Fatal("a binding that now names another handle should not re-claim the old one")
	}
	if err := os.Remove(bind); err != nil {
		t.Fatal(err)
	}
	if stillWantsToListen(paths, profile, ccSession) {
		t.Fatal("impromptu session after binding removal (detach) should not want to listen")
	}

	// Launcher: gated on the launcher process being alive.
	aliveTok := "launch-" + strconv.Itoa(os.Getpid())
	if !stillWantsToListen(paths, profile, aliveTok) {
		t.Fatalf("launcher token for a live pid (%s) should want to listen", aliveTok)
	}
	// A pid that cannot be running.
	if stillWantsToListen(paths, profile, "launch-2147483646") {
		t.Fatal("launcher token for a dead pid should not want to listen")
	}
	if stillWantsToListen(paths, profile, "launch-notanumber") {
		t.Fatal("launcher token with a non-numeric pid should not want to listen")
	}
}

func TestProcessIsAlive(t *testing.T) {
	if !processIsAlive(os.Getpid()) {
		t.Fatal("own pid should be alive")
	}
	if processIsAlive(0) || processIsAlive(-1) {
		t.Fatal("non-positive pids are never alive")
	}
	if processIsAlive(2147483646) {
		t.Fatal("an impossible pid should not be alive")
	}
}
