//go:build darwin || linux

package main

import (
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

func TestEphemeralResolveDaemonBase(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	paths, err := resolveCLIPaths(home)
	if err != nil {
		t.Fatal(err)
	}

	// Unpaired → empty (caller falls back to the env cascade default).
	if got := ephemeralResolveDaemonBase("", paths); got != "" {
		t.Fatalf("unpaired base = %q, want empty", got)
	}

	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_11111111-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222",
		Token:    "ldt_ag_owner_ld_x_secret-token-value",
		BaseURL:  "https://comt.dev",
	}); err != nil {
		t.Fatal(err)
	}

	// Paired, NO ark key, no explicit base override → the paired host (this is the
	// fix: it must NOT gate on COMMENT_IO_ENV, which applyEnvironment always sets).
	if got := ephemeralResolveDaemonBase("", paths); got != "https://comt.dev" {
		t.Fatalf("paired no-ark base = %q, want https://comt.dev", got)
	}

	// An ark key present → empty: the ark path keeps the configured base and is
	// never redirected to the paired host.
	if got := ephemeralResolveDaemonBase("ark_max_x", paths); got != "" {
		t.Fatalf("ark base = %q, want empty", got)
	}

	// An explicit COMMENT_IO_BASE_URL override → empty (honored via DefaultBaseURL).
	t.Setenv("COMMENT_IO_BASE_URL", "https://comment.io")
	if got := ephemeralResolveDaemonBase("", paths); got != "" {
		t.Fatalf("explicit-base-url base = %q, want empty", got)
	}
}
