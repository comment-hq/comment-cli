//go:build darwin || linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// stubBusInstallPair replaces the busInstall pair-chain hooks for the duration
// of the test. Required for any test that drives the real busInstall: `go test`
// gives test binaries /dev/null as stdin, which is a character device, so the
// unstubbed stdinIsInteractive() reports true and an unpaired temp home would
// enter the real network pair flow.
func stubBusInstallPair(t *testing.T, interactive bool, pairFn func(home string) error) {
	t.Helper()
	prevInteractive := busInstallStdinIsInteractive
	prevPair := busInstallRunPair
	busInstallStdinIsInteractive = func() bool { return interactive }
	busInstallRunPair = pairFn
	t.Cleanup(func() {
		busInstallStdinIsInteractive = prevInteractive
		busInstallRunPair = prevPair
	})
}

func pairChainTestPaths(t *testing.T) commentbus.Paths {
	t.Helper()
	home := filepath.Join(privateTempHome(t, "comment-pair-chain-"), ".comment-io")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	return commentbus.Paths{Home: home, Bus: filepath.Join(home, "bus")}
}

func TestBusInstallPairChainUnpairedNonInteractiveSetsFollowup(t *testing.T) {
	paths := pairChainTestPaths(t)
	result := map[string]any{}
	pairCalls := 0

	busInstallPairChain(result, paths, false, func() error {
		pairCalls++
		return nil
	})

	if pairCalls != 0 {
		t.Fatalf("non-interactive install must not enter the pair flow; pairFn called %d times", pairCalls)
	}
	if result["pair_followup"] != busInstallPairFollowup {
		t.Fatalf("pair_followup = %#v, want %q", result["pair_followup"], busInstallPairFollowup)
	}
	if _, ok := result["pair_warning"]; ok {
		t.Fatalf("unexpected pair_warning: %#v", result)
	}
}

func TestBusInstallPairChainUnpairedInteractiveRunsPairFlow(t *testing.T) {
	paths := pairChainTestPaths(t)
	result := map[string]any{}
	pairCalls := 0

	busInstallPairChain(result, paths, true, func() error {
		pairCalls++
		return nil
	})

	if pairCalls != 1 {
		t.Fatalf("interactive unpaired install must enter the pair flow once; pairFn called %d times", pairCalls)
	}
	if _, ok := result["pair_followup"]; ok {
		t.Fatalf("unexpected pair_followup after interactive pair: %#v", result)
	}
	if _, ok := result["pair_warning"]; ok {
		t.Fatalf("unexpected pair_warning after successful pair: %#v", result)
	}
}

func TestBusInstallPairChainInteractivePairErrorRecordsWarning(t *testing.T) {
	paths := pairChainTestPaths(t)
	result := map[string]any{}

	busInstallPairChain(result, paths, true, func() error {
		return errors.New("pairing code expired")
	})

	if result["pair_warning"] != "pairing code expired" {
		t.Fatalf("pair_warning = %#v, want the pair error (install itself must not fail)", result["pair_warning"])
	}
	if _, ok := result["pair_followup"]; ok {
		t.Fatalf("unexpected pair_followup alongside pair_warning: %#v", result)
	}
}

func TestBusInstallPairChainPairedAddsNothing(t *testing.T) {
	paths := pairChainTestPaths(t)
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "dmn_test",
		Token:    "secret-token",
		BaseURL:  "https://comment.example",
		Label:    "test computer",
	}); err != nil {
		t.Fatal(err)
	}

	for _, interactive := range []bool{true, false} {
		result := map[string]any{}
		busInstallPairChain(result, paths, interactive, func() error {
			t.Fatalf("paired install (interactive=%v) must not enter the pair flow", interactive)
			return nil
		})
		if len(result) != 0 {
			t.Fatalf("paired install (interactive=%v) must add nothing, got %#v", interactive, result)
		}
	}
}

func TestBusInstallPairChainUnreadableAuthRecordsWarning(t *testing.T) {
	paths := pairChainTestPaths(t)
	if err := os.MkdirAll(paths.Bus, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(commentbus.DaemonAuthPath(paths), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	result := map[string]any{}
	busInstallPairChain(result, paths, true, func() error {
		t.Fatal("unreadable credentials must not enter the pair flow (bus pair refuses without --force)")
		return nil
	})

	warning, _ := result["pair_warning"].(string)
	if warning == "" || !containsAll(warning, "unreadable", "--force") {
		t.Fatalf("pair_warning = %#v, want unreadable-credentials message with --force hint", result["pair_warning"])
	}
	if _, ok := result["pair_followup"]; ok {
		t.Fatalf("unexpected pair_followup for unreadable credentials: %#v", result)
	}
}

// TestBusInstallNonInteractiveIncludesPairFollowup drives the real busInstall
// (fake launchctl) on a fresh, unpaired home in non-interactive mode: the
// install must succeed, must not enter the pair flow, and must surface the
// `comment bus pair` follow-up.
func TestBusInstallNonInteractiveIncludesPairFollowup(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv(commentbus.BmuxBinaryEnv, "")
	installFakeLaunchctl(t)
	bin := writeFakeCommentBinary(t)
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		return commentbus.BmuxInstallResult{Path: "bmux", AlreadyPresent: true, Discoverable: true}, nil
	})
	stubBusInstallPair(t, false, func(home string) error {
		t.Fatalf("non-interactive bus install must not call the pair flow (home=%s)", home)
		return nil
	})

	result, err := busInstall(filepath.Join(userHome, ".comment-io"), "", bin, false, true)
	if err != nil {
		t.Fatalf("busInstall failed: %v", err)
	}
	if result["pair_followup"] != busInstallPairFollowup {
		t.Fatalf("pair_followup = %#v, want %q", result["pair_followup"], busInstallPairFollowup)
	}
}

// TestBusInstallInteractiveChainsPairFlow drives the real busInstall on a
// fresh, unpaired home in interactive mode: the pair flow runs once with the
// resolved home.
func TestBusInstallInteractiveChainsPairFlow(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv(commentbus.BmuxBinaryEnv, "")
	installFakeLaunchctl(t)
	bin := writeFakeCommentBinary(t)
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		return commentbus.BmuxInstallResult{Path: "bmux", AlreadyPresent: true, Discoverable: true}, nil
	})
	commentHome := filepath.Join(userHome, ".comment-io")
	var pairedHomes []string
	stubBusInstallPair(t, true, func(home string) error {
		pairedHomes = append(pairedHomes, home)
		return nil
	})

	result, err := busInstall(commentHome, "", bin, false, true)
	if err != nil {
		t.Fatalf("busInstall failed: %v", err)
	}
	if len(pairedHomes) != 1 || pairedHomes[0] != commentHome {
		t.Fatalf("pair flow homes = %#v, want exactly one call with %q", pairedHomes, commentHome)
	}
	if _, ok := result["pair_followup"]; ok {
		t.Fatalf("unexpected pair_followup after interactive pair: %#v", result)
	}
	if _, ok := result["pair_warning"]; ok {
		t.Fatalf("unexpected pair_warning after successful pair: %#v", result)
	}
}

// TestBusInstallNoPairSkipsPairChain — pair=false (the unattended auto-update
// reinstall path) must skip the pair chain entirely, even when the
// interactivity heuristic reports true (service stdin is often /dev/null, a
// character device it misreads as a human).
func TestBusInstallNoPairSkipsPairChain(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv(commentbus.BmuxBinaryEnv, "")
	installFakeLaunchctl(t)
	bin := writeFakeCommentBinary(t)
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		return commentbus.BmuxInstallResult{Path: "bmux", AlreadyPresent: true, Discoverable: true}, nil
	})
	stubBusInstallPair(t, true, func(home string) error {
		t.Fatalf("pair=false bus install must not call the pair flow (home=%s)", home)
		return nil
	})

	result, err := busInstall(filepath.Join(userHome, ".comment-io"), "", bin, false, false)
	if err != nil {
		t.Fatalf("busInstall failed: %v", err)
	}
	if _, ok := result["pair_followup"]; ok {
		t.Fatalf("pair=false install must not surface pair_followup: %#v", result)
	}
	if _, ok := result["pair_warning"]; ok {
		t.Fatalf("pair=false install must not surface pair_warning: %#v", result)
	}
}

// TestBusInstallDryRunSkipsPairChain — dry-run must not touch the filesystem
// or network, so the pair chain is skipped entirely even when interactive.
func TestBusInstallDryRunSkipsPairChain(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	installFakeLaunchctl(t)
	bin := writeFakeCommentBinary(t)
	stubBusInstallPair(t, true, func(home string) error {
		t.Fatalf("dry-run bus install must not call the pair flow (home=%s)", home)
		return nil
	})

	result, err := busInstall(filepath.Join(userHome, ".comment-io"), "", bin, true, true)
	if err != nil {
		t.Fatalf("busInstall dry-run failed: %v", err)
	}
	if _, ok := result["pair_followup"]; ok {
		t.Fatalf("dry-run must not surface pair_followup: %#v", result)
	}
	if _, ok := result["pair_warning"]; ok {
		t.Fatalf("dry-run must not surface pair_warning: %#v", result)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
