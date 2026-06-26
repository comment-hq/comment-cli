//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// withAutoUpdateSeams swaps the package-level auto-update seams (version, fetch,
// npm install, bus install, health check, clock) and restores them on cleanup.
// busInstall is always stubbed so a test never restarts a real service.
func withAutoUpdateSeams(t *testing.T, ver string,
	fetch func(context.Context, string, string, string, string) (versionCheckResponse, error),
	npm func(context.Context, string) error,
	bus func(context.Context, commentbus.Paths) error,
	health func(context.Context, commentbus.Paths) error,
) {
	t.Helper()
	origVersion := version
	origFetch := autoUpdateFetch
	origNpm := autoUpdateRunNpmInstall
	origBus := autoUpdateRunBusInstall
	origHealth := autoUpdateHealthCheck
	origFallbackPaths := autoUpdateNpmFallbackPaths
	origNow := autoUpdateNow
	version = ver
	autoUpdateFetch = fetch
	autoUpdateRunNpmInstall = npm
	// The seams take the daemon's botlets home; most tests don't care, so the
	// helper adapts the 2-arg stub (TestAutoUpdateBusInstallCarriesBotletsHome
	// assigns the seam directly to assert the threading).
	autoUpdateRunBusInstall = func(ctx context.Context, paths commentbus.Paths, _ string) error { return bus(ctx, paths) }
	// Like busInstall, the health seam takes the daemon's botlets home; adapt the
	// 2-arg stub (TestAutoUpdateHealthCheckCarriesBotletsHome assigns the seam
	// directly to assert the threading).
	autoUpdateHealthCheck = func(ctx context.Context, paths commentbus.Paths, _ string) error { return health(ctx, paths) }
	autoUpdateNpmFallbackPaths = origFallbackPaths
	autoUpdateNow = func() time.Time { return time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() {
		version = origVersion
		autoUpdateFetch = origFetch
		autoUpdateRunNpmInstall = origNpm
		autoUpdateRunBusInstall = origBus
		autoUpdateHealthCheck = origHealth
		autoUpdateNpmFallbackPaths = origFallbackPaths
		autoUpdateNow = origNow
	})
	t.Setenv("COMMENT_IO_HOME", t.TempDir())
}

func testAutoUpdatePaths(t *testing.T) commentbus.Paths {
	t.Helper()
	paths, err := commentbus.ResolvePaths(filepath.Join(t.TempDir(), "comment"))
	if err != nil {
		t.Fatal(err)
	}
	if err := commentbus.EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	return paths
}

func okFetch(latest string, minimum string) func(context.Context, string, string, string, string) (versionCheckResponse, error) {
	return func(context.Context, string, string, string, string) (versionCheckResponse, error) {
		return versionCheckResponse{Latest: latest, Minimum: minimum}, nil
	}
}

func TestResolveAutoUpdateNpmBinaryUsesPinnedServicePath(t *testing.T) {
	npm := filepath.Join(t.TempDir(), "npm")
	if err := os.WriteFile(npm, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(autoUpdateNpmBinaryEnv, npm)
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	got, err := resolveAutoUpdateNpmBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got != npm {
		t.Fatalf("resolved npm = %q, want pinned %q", got, npm)
	}
}

func TestResolveAutoUpdateNpmBinaryFailsClearlyWhenLaunchdPathCannotFindNpm(t *testing.T) {
	origLookPath := upgradeLookPath
	upgradeLookPath = func(string) (string, error) {
		return "", os.ErrNotExist
	}
	origFallbackPaths := autoUpdateNpmFallbackPaths
	autoUpdateNpmFallbackPaths = func() []string { return nil }
	t.Cleanup(func() {
		upgradeLookPath = origLookPath
		autoUpdateNpmFallbackPaths = origFallbackPaths
	})
	t.Setenv(autoUpdateNpmBinaryEnv, "")

	_, err := resolveAutoUpdateNpmBinary()
	if err == nil {
		t.Fatal("expected missing npm to return an error")
	}
	if !strings.Contains(err.Error(), autoUpdateNpmBinaryEnv) || !strings.Contains(err.Error(), "npm not found on PATH") {
		t.Fatalf("missing-npm error = %v, want actionable daemon reinstall guidance", err)
	}
}

func TestResolveAutoUpdateNpmBinaryFallsBackToCommonInstallPath(t *testing.T) {
	npm := filepath.Join(t.TempDir(), "npm")
	if err := os.WriteFile(npm, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	origLookPath := upgradeLookPath
	upgradeLookPath = func(string) (string, error) {
		return "", os.ErrNotExist
	}
	origFallbackPaths := autoUpdateNpmFallbackPaths
	autoUpdateNpmFallbackPaths = func() []string { return []string{filepath.Join(t.TempDir(), "missing-npm"), npm} }
	t.Cleanup(func() {
		upgradeLookPath = origLookPath
		autoUpdateNpmFallbackPaths = origFallbackPaths
	})
	t.Setenv(autoUpdateNpmBinaryEnv, "")

	got, err := resolveAutoUpdateNpmBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got != npm {
		t.Fatalf("resolved npm = %q, want fallback %q", got, npm)
	}
}

func TestAutoUpdateRunNpmInstallPrependsPinnedNpmDirToPath(t *testing.T) {
	dir := t.TempDir()
	npm := filepath.Join(dir, "npm")
	if err := os.WriteFile(npm, []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(autoUpdateNpmBinaryEnv, npm)
	t.Setenv("PATH", "/usr/bin")

	origCombined := upgradeCombinedOutput
	var capturedEnv []string
	var capturedCommand string
	var capturedArgs []string
	upgradeCombinedOutput = func(_ context.Context, env []string, command string, args ...string) ([]byte, error) {
		capturedEnv = append([]string(nil), env...)
		capturedCommand = command
		capturedArgs = append([]string(nil), args...)
		return nil, nil
	}
	t.Cleanup(func() { upgradeCombinedOutput = origCombined })

	if err := autoUpdateRunNpmInstall(context.Background(), "@comment-io/cli@1.2.3"); err != nil {
		t.Fatal(err)
	}
	if capturedCommand != npm {
		t.Fatalf("npm command = %q, want %q", capturedCommand, npm)
	}
	if strings.Join(capturedArgs, " ") != "install -g @comment-io/cli@1.2.3" {
		t.Fatalf("npm args = %v", capturedArgs)
	}
	pathValue := ""
	for _, entry := range capturedEnv {
		if strings.HasPrefix(entry, "PATH=") {
			pathValue = strings.TrimPrefix(entry, "PATH=")
			break
		}
	}
	wantPrefix := dir + string(os.PathListSeparator)
	if !strings.HasPrefix(pathValue, wantPrefix) {
		t.Fatalf("PATH = %q, want npm dir prefix %q", pathValue, wantPrefix)
	}
}

// TestAutoUpdateWorkerOutdatedWritesJournalBeforeInstall verifies the full
// behind-latest path: the rollback journal is written BEFORE npm install runs,
// and the journal records from/to/package.
func TestAutoUpdateWorkerOutdatedWritesJournalBeforeInstall(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	var journalAtInstall commentbus.AutoUpdateJournal
	var journalPresentAtInstall bool
	npmCalls := 0
	busCalls := 0
	withAutoUpdateSeams(t, "1.0.0",
		okFetch("1.2.0", "1.0.0"),
		func(_ context.Context, spec string) error {
			npmCalls++
			if spec != "@comment-io/cli@1.2.0" {
				t.Fatalf("npm spec = %q, want @comment-io/cli@1.2.0", spec)
			}
			// The journal MUST already exist when install runs.
			journalAtInstall, journalPresentAtInstall = commentbus.ReadAutoUpdateJournal(paths)
			return nil
		},
		func(context.Context, commentbus.Paths) error { busCalls++; return nil },
		func(context.Context, commentbus.Paths) error { return nil },
	)

	if !runAutoUpdateWorkerOnce(context.Background(), paths, "") {
		t.Fatal("worker should have initiated an upgrade")
	}
	if npmCalls != 1 || busCalls != 1 {
		t.Fatalf("npmCalls=%d busCalls=%d, want 1/1", npmCalls, busCalls)
	}
	if !journalPresentAtInstall {
		t.Fatal("journal was not written before npm install")
	}
	if journalAtInstall.FromVersion != "1.0.0" || journalAtInstall.ToVersion != "1.2.0" {
		t.Fatalf("journal = %+v, want from 1.0.0 to 1.2.0", journalAtInstall)
	}
	if journalAtInstall.PackageName != "@comment-io/cli" {
		t.Fatalf("journal package = %q", journalAtInstall.PackageName)
	}
	// Health state reflects the available update.
	state := commentbus.ReadAutoUpdateState(paths)
	if state.LatestVersion != "1.2.0" || !state.UpdateAvailable {
		t.Fatalf("state = %+v, want latest 1.2.0 + update available", state)
	}
}

// TestAutoUpdateWorkerCurrentEqualsLatestNoOps verifies the up-to-date path does
// not install or write a journal, but still refreshes the cached health state.
func TestAutoUpdateWorkerCurrentEqualsLatestNoOps(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	withAutoUpdateSeams(t, "1.2.0",
		okFetch("1.2.0", "1.0.0"),
		func(context.Context, string) error { t.Fatal("npm install must not run when up to date"); return nil },
		func(context.Context, commentbus.Paths) error {
			t.Fatal("bus install must not run when up to date")
			return nil
		},
		func(context.Context, commentbus.Paths) error { return nil },
	)

	if runAutoUpdateWorkerOnce(context.Background(), paths, "") {
		t.Fatal("worker should no-op when current == latest")
	}
	if _, ok := commentbus.ReadAutoUpdateJournal(paths); ok {
		t.Fatal("no journal should be written when up to date")
	}
	state := commentbus.ReadAutoUpdateState(paths)
	if state.LatestVersion != "1.2.0" || state.UpdateAvailable {
		t.Fatalf("state = %+v, want latest 1.2.0 + no update", state)
	}
}

// TestAutoUpdateWorkerNeverBelowMinimum verifies that when the backend's latest
// is (mis)reported below its minimum, the daemon upgrades to the minimum, never
// to the stale lower latest.
func TestAutoUpdateWorkerNeverBelowMinimum(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	var installedSpec string
	withAutoUpdateSeams(t, "1.5.0",
		okFetch("1.0.0", "2.0.0"), // latest < minimum
		func(_ context.Context, spec string) error { installedSpec = spec; return nil },
		func(context.Context, commentbus.Paths) error { return nil },
		func(context.Context, commentbus.Paths) error { return nil },
	)

	if !runAutoUpdateWorkerOnce(context.Background(), paths, "") {
		t.Fatal("worker should upgrade up to the minimum")
	}
	if installedSpec != "@comment-io/cli@2.0.0" {
		t.Fatalf("installed spec = %q, want @comment-io/cli@2.0.0 (the minimum)", installedSpec)
	}
	journal, ok := commentbus.ReadAutoUpdateJournal(paths)
	if !ok || journal.ToVersion != "2.0.0" {
		t.Fatalf("journal = %+v ok=%v, want toVersion 2.0.0", journal, ok)
	}
}

// TestAutoUpdatePostStartHealthyCommits verifies that when the running binary
// equals the journal's toVersion and the post-start health check passes, the
// journal is deleted, the result is recorded as success, and any stale
// LastRolledBackVersion (from an earlier rollback of this same version) is
// cleared so the anti-thrash skip no longer applies.
func TestAutoUpdatePostStartHealthyCommits(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	withAutoUpdateSeams(t, "1.2.0",
		okFetch("1.2.0", "1.0.0"),
		func(context.Context, string) error { t.Fatal("no install during a healthy commit"); return nil },
		func(context.Context, commentbus.Paths) error {
			t.Fatal("no bus install during a healthy commit")
			return nil
		},
		func(context.Context, commentbus.Paths) error { return nil }, // healthy
	)
	// Seed a stale rolled-back marker for this same version to prove it clears.
	if err := commentbus.WriteAutoUpdateState(paths, commentbus.AutoUpdateState{
		LastUpdateResult: commentbus.AutoUpdateResultRolledBack, LastRolledBackVersion: "1.2.0",
	}); err != nil {
		t.Fatal(err)
	}
	if err := commentbus.WriteAutoUpdateJournal(paths, commentbus.AutoUpdateJournal{
		FromVersion: "1.0.0", ToVersion: "1.2.0", PackageName: "@comment-io/cli", Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}

	reconcileAutoUpdatePostStart(context.Background(), paths, "")

	if _, ok := commentbus.ReadAutoUpdateJournal(paths); ok {
		t.Fatal("journal should be deleted after a healthy commit")
	}
	state := commentbus.ReadAutoUpdateState(paths)
	if state.LastUpdateResult != commentbus.AutoUpdateResultSuccess {
		t.Fatalf("last_update_result = %q, want success", state.LastUpdateResult)
	}
	if state.LastRolledBackVersion != "" {
		t.Fatalf("last_rolled_back_version = %q, want cleared on success", state.LastRolledBackVersion)
	}
}

// TestAutoUpdatePostStartUnhealthyRollsBackImmediately verifies that an
// unhealthy new binary rolls back to fromVersion right away after a successful
// StartDaemon — a real health failure is a definitive bad signal that needs no
// attempt cap. The rollback npm-installs the OLD version, restarts, clears the
// journal, and remembers the bad toVersion.
func TestAutoUpdatePostStartUnhealthyRollsBackImmediately(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	var rollbackSpec string
	busCalls := 0
	withAutoUpdateSeams(t, "1.2.0",
		okFetch("1.2.0", "1.0.0"),
		func(_ context.Context, spec string) error { rollbackSpec = spec; return nil },
		func(context.Context, commentbus.Paths) error { busCalls++; return nil },
		func(context.Context, commentbus.Paths) error { return errors.New("socket bind failed") }, // unhealthy
	)
	// Attempts at 0: no cap reached, yet a health failure still rolls back now.
	if err := commentbus.WriteAutoUpdateJournal(paths, commentbus.AutoUpdateJournal{
		FromVersion: "1.0.0", ToVersion: "1.2.0", PackageName: "@comment-io/cli", Attempts: 0,
	}); err != nil {
		t.Fatal(err)
	}

	reconcileAutoUpdatePostStart(context.Background(), paths, "")

	if rollbackSpec != "@comment-io/cli@1.0.0" {
		t.Fatalf("rollback spec = %q, want @comment-io/cli@1.0.0 (fromVersion)", rollbackSpec)
	}
	if busCalls != 1 {
		t.Fatalf("bus install calls = %d, want 1 for the rollback restart", busCalls)
	}
	if _, ok := commentbus.ReadAutoUpdateJournal(paths); ok {
		t.Fatal("journal should be deleted after rollback")
	}
	state := commentbus.ReadAutoUpdateState(paths)
	if state.LastUpdateResult != commentbus.AutoUpdateResultRolledBack {
		t.Fatalf("last_update_result = %q, want rolled_back", state.LastUpdateResult)
	}
	if state.LastRolledBackVersion != "1.2.0" {
		t.Fatalf("last_rolled_back_version = %q, want 1.2.0", state.LastRolledBackVersion)
	}
}

// TestAutoUpdatePostStartRollbackThenRestartedBinaryDoesNotThrash is the
// end-to-end regression for the round-3 finding: after a post-start health
// failure rolls back, the restarted (old) binary's worker tick must NOT
// re-upgrade into the same bad toVersion. Because rollbackAutoUpdate records the
// anti-thrash lock BEFORE the process-killing bus install, the lock is durable
// across the restart even when the journal only reached attempt 1.
func TestAutoUpdatePostStartRollbackThenRestartedBinaryDoesNotThrash(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	// Phase 1: we are the bad new binary (1.2.0); post-start health fails -> rollback.
	withAutoUpdateSeams(t, "1.2.0",
		okFetch("1.2.0", "1.0.0"),
		func(context.Context, string) error { return nil },           // npm rollback ok
		func(context.Context, commentbus.Paths) error { return nil }, // bus install (stubbed) "restarts"
		func(context.Context, commentbus.Paths) error { return errors.New("unhealthy") },
	)
	if err := commentbus.WriteAutoUpdateJournal(paths, commentbus.AutoUpdateJournal{
		FromVersion: "1.0.0", ToVersion: "1.2.0", PackageName: "@comment-io/cli", Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	reconcileAutoUpdatePostStart(context.Background(), paths, "")

	// Phase 2: the restarted OLD binary (1.0.0) runs a worker tick. The server's
	// latest is still the bad 1.2.0. The cooldown must suppress the re-upgrade.
	performCalled := false
	withAutoUpdateSeams(t, "1.0.0",
		okFetch("1.2.0", "1.0.0"),
		func(context.Context, string) error { performCalled = true; return nil },
		func(context.Context, commentbus.Paths) error { performCalled = true; return nil },
		func(context.Context, commentbus.Paths) error { return nil },
	)
	if did := runAutoUpdateWorkerOnce(context.Background(), paths, ""); did {
		t.Fatal("worker re-upgraded into a rolled-back version (thrash)")
	}
	if performCalled {
		t.Fatal("npm/bus install ran for a rolled-back version — cooldown failed")
	}
	if _, ok := commentbus.ReadAutoUpdateJournal(paths); ok {
		t.Fatal("no new journal should be written when the upgrade is skipped")
	}
}

// TestAutoUpdatePreStartMatchingBelowCapIncrements verifies that when we ARE the
// new binary (version == toVersion) but the attempt counter is below the cap,
// the pre-start phase only bumps + persists the attempt and does NOT roll back —
// StartDaemon is allowed to proceed and the post-start health check decides.
func TestAutoUpdatePreStartMatchingBelowCapIncrements(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	withAutoUpdateSeams(t, "1.2.0",
		okFetch("1.2.0", "1.0.0"),
		func(context.Context, string) error { t.Fatal("no npm below the boot-attempt cap"); return nil },
		func(context.Context, commentbus.Paths) error {
			t.Fatal("no bus install below the boot-attempt cap")
			return nil
		},
		func(context.Context, commentbus.Paths) error {
			t.Fatal("pre-start must not run the health check")
			return nil
		},
	)
	if err := commentbus.WriteAutoUpdateJournal(paths, commentbus.AutoUpdateJournal{
		FromVersion: "1.0.0", ToVersion: "1.2.0", PackageName: "@comment-io/cli", Attempts: 0,
	}); err != nil {
		t.Fatal(err)
	}

	reconcileAutoUpdatePreStart(context.Background(), paths, "")

	journal, ok := commentbus.ReadAutoUpdateJournal(paths)
	if !ok {
		t.Fatal("journal should persist below the boot-attempt cap")
	}
	if journal.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (persisted before StartDaemon)", journal.Attempts)
	}
}

// TestAutoUpdatePreStartMatchingAtCapRollsBack verifies that when we ARE the new
// binary at the boot-attempt cap (e.g. it keeps crashing inside StartDaemon),
// the pre-start phase rolls back to fromVersion without ever needing a health
// signal — the repeated crash IS the signal.
func TestAutoUpdatePreStartMatchingAtCapRollsBack(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	var rollbackSpec string
	busCalls := 0
	withAutoUpdateSeams(t, "1.2.0",
		okFetch("1.2.0", "1.0.0"),
		func(_ context.Context, spec string) error { rollbackSpec = spec; return nil },
		func(context.Context, commentbus.Paths) error { busCalls++; return nil },
		func(context.Context, commentbus.Paths) error {
			t.Fatal("pre-start rollback must not run the health check")
			return nil
		},
	)
	// One below the cap so this boot hits it.
	if err := commentbus.WriteAutoUpdateJournal(paths, commentbus.AutoUpdateJournal{
		FromVersion: "1.0.0", ToVersion: "1.2.0", PackageName: "@comment-io/cli", Attempts: autoUpdateMaxAttempts - 1,
	}); err != nil {
		t.Fatal(err)
	}

	reconcileAutoUpdatePreStart(context.Background(), paths, "")

	if rollbackSpec != "@comment-io/cli@1.0.0" {
		t.Fatalf("rollback spec = %q, want @comment-io/cli@1.0.0 (fromVersion)", rollbackSpec)
	}
	if busCalls != 1 {
		t.Fatalf("bus install calls = %d, want 1 for the rollback restart", busCalls)
	}
	if _, ok := commentbus.ReadAutoUpdateJournal(paths); ok {
		t.Fatal("journal should be deleted after rollback")
	}
	state := commentbus.ReadAutoUpdateState(paths)
	if state.LastRolledBackVersion != "1.2.0" {
		t.Fatalf("last_rolled_back_version = %q, want 1.2.0", state.LastRolledBackVersion)
	}
}

// TestAutoUpdatePreStartCrashLoopNpmFailDoesNotLoop is the regression test for
// bug #2: a binary that crashes inside StartDaemon is caught by repeated
// pre-start boots; at the cap it rolls back, and when the rollback's npm install
// FAILS there is NO restored binary to restart onto — so rollback must NOT call
// bus install (which would restart the still-broken binary forever). It must
// instead record rolled_back + DELETE the journal so a subsequent boot does not
// re-attempt the rollback, breaking the loop.
func TestAutoUpdatePreStartCrashLoopNpmFailDoesNotLoop(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	npmCalls := 0
	withAutoUpdateSeams(t, "1.2.0", // we are the (bad) new binary that crashes on start
		okFetch("1.2.0", "1.0.0"),
		func(_ context.Context, _ string) error {
			npmCalls++
			return errors.New("npm registry unreachable") // rollback install fails
		},
		func(context.Context, commentbus.Paths) error {
			t.Fatal("bug #2: bus install must NOT run when the rollback npm install failed")
			return nil
		},
		func(context.Context, commentbus.Paths) error {
			t.Fatal("pre-start must not run the health check")
			return nil
		},
	)
	if err := commentbus.WriteAutoUpdateJournal(paths, commentbus.AutoUpdateJournal{
		FromVersion: "1.0.0", ToVersion: "1.2.0", PackageName: "@comment-io/cli", Attempts: 0,
	}); err != nil {
		t.Fatal(err)
	}

	// Boots below the cap: each increments, no rollback yet.
	for i := 1; i < autoUpdateMaxAttempts; i++ {
		reconcileAutoUpdatePreStart(context.Background(), paths, "")
		if j, ok := commentbus.ReadAutoUpdateJournal(paths); !ok || j.Attempts != i {
			t.Fatalf("after boot %d: journal=%+v ok=%v, want attempts %d", i, j, ok, i)
		}
		if npmCalls != 0 {
			t.Fatalf("npm should not run before the cap, got %d calls after boot %d", npmCalls, i)
		}
	}

	// Cap boot: hits the cap -> rollback. npm install of fromVersion FAILS.
	reconcileAutoUpdatePreStart(context.Background(), paths, "")
	if npmCalls != 1 {
		t.Fatalf("npm calls = %d, want exactly 1 (the failed rollback install)", npmCalls)
	}
	if _, ok := commentbus.ReadAutoUpdateJournal(paths); ok {
		t.Fatal("journal must be deleted even when the rollback npm install failed, else the loop recurs")
	}
	state := commentbus.ReadAutoUpdateState(paths)
	if state.LastUpdateResult != commentbus.AutoUpdateResultRolledBack {
		t.Fatalf("last_update_result = %q, want rolled_back", state.LastUpdateResult)
	}
	if state.LastRolledBackVersion != "1.2.0" {
		t.Fatalf("last_rolled_back_version = %q, want 1.2.0", state.LastRolledBackVersion)
	}

	// Boot 3: journal is gone, so nothing re-attempts — the loop is broken.
	reconcileAutoUpdatePreStart(context.Background(), paths, "")
	if npmCalls != 1 {
		t.Fatalf("npm calls = %d after a third boot, want still 1 (no recurrence)", npmCalls)
	}
}

// TestAutoUpdatePreStartStillOldAtCapGivesUp verifies that when the install
// never applied (current still != toVersion) at the attempt cap, the pre-start
// phase gives up: it records rolled_back (remembering the bad toVersion) and
// clears the journal without an npm rollback — there is nothing to roll back to
// because the bad version never landed.
func TestAutoUpdatePreStartStillOldAtCapGivesUp(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	withAutoUpdateSeams(t, "1.0.0", // still the OLD version: install did not take
		okFetch("1.2.0", "1.0.0"),
		func(context.Context, string) error { t.Fatal("give-up must not run npm"); return nil },
		func(context.Context, commentbus.Paths) error { t.Fatal("give-up must not run bus install"); return nil },
		func(context.Context, commentbus.Paths) error {
			t.Fatal("pre-start must not run the health check")
			return nil
		},
	)
	if err := commentbus.WriteAutoUpdateJournal(paths, commentbus.AutoUpdateJournal{
		FromVersion: "1.0.0", ToVersion: "1.2.0", PackageName: "@comment-io/cli", Attempts: autoUpdateMaxAttempts - 1,
	}); err != nil {
		t.Fatal(err)
	}

	reconcileAutoUpdatePreStart(context.Background(), paths, "")

	if _, ok := commentbus.ReadAutoUpdateJournal(paths); ok {
		t.Fatal("journal should be deleted after giving up at the cap")
	}
	state := commentbus.ReadAutoUpdateState(paths)
	if state.LastUpdateResult != commentbus.AutoUpdateResultRolledBack {
		t.Fatalf("last_update_result = %q, want rolled_back", state.LastUpdateResult)
	}
	if state.LastRolledBackVersion != "1.2.0" {
		t.Fatalf("last_rolled_back_version = %q, want 1.2.0", state.LastRolledBackVersion)
	}
}

// TestAutoUpdateWorkerSkipsRolledBackTarget is the regression test for bug #3:
// after a rollback, the next 24h tick must NOT re-upgrade into the same bad
// `latest`. When the cached state says we rolled back FROM exactly the fetched
// target, the worker skips the upgrade entirely (no journal, no install) and
// returns false — preventing upgrade/rollback thrash.
func TestAutoUpdateWorkerSkipsRolledBackTarget(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	withAutoUpdateSeams(t, "1.0.0",
		okFetch("1.2.0", "1.0.0"), // backend still serves the bad version as latest
		func(context.Context, string) error {
			t.Fatal("bug #3: must not re-install a rolled-back target")
			return nil
		},
		func(context.Context, commentbus.Paths) error {
			t.Fatal("bug #3: must not restart into a rolled-back target")
			return nil
		},
		func(context.Context, commentbus.Paths) error { return nil },
	)
	if err := commentbus.WriteAutoUpdateState(paths, commentbus.AutoUpdateState{
		LastUpdateResult: commentbus.AutoUpdateResultRolledBack, LastRolledBackVersion: "1.2.0",
	}); err != nil {
		t.Fatal(err)
	}

	if runAutoUpdateWorkerOnce(context.Background(), paths, "") {
		t.Fatal("worker should skip (not initiate) an upgrade to a rolled-back target")
	}
	if _, ok := commentbus.ReadAutoUpdateJournal(paths); ok {
		t.Fatal("no journal should be written when skipping a rolled-back target")
	}
	// The cached health view still refreshes so `bus health` shows the available
	// (but skipped) update.
	state := commentbus.ReadAutoUpdateState(paths)
	if state.LatestVersion != "1.2.0" || !state.UpdateAvailable {
		t.Fatalf("state = %+v, want latest 1.2.0 + update available", state)
	}
}

// TestAutoUpdateReconcileNoJournalNoOp verifies a clean start with no pending
// update does nothing in either reconcile phase.
func TestAutoUpdateReconcileNoJournalNoOp(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	withAutoUpdateSeams(t, "1.2.0",
		okFetch("1.2.0", "1.0.0"),
		func(context.Context, string) error { t.Fatal("no npm without a journal"); return nil },
		func(context.Context, commentbus.Paths) error { t.Fatal("no bus install without a journal"); return nil },
		func(context.Context, commentbus.Paths) error {
			t.Fatal("no health check without a journal")
			return nil
		},
	)

	reconcileAutoUpdatePreStart(context.Background(), paths, "")
	reconcileAutoUpdatePostStart(context.Background(), paths, "")

	state := commentbus.ReadAutoUpdateState(paths)
	if state.LastUpdateResult != "" {
		t.Fatalf("last_update_result = %q, want empty (untouched)", state.LastUpdateResult)
	}
}

// TestAutoUpdateFetchFailureNoOp verifies that a fetch error never acts and
// never writes a journal — an offline daemon must not brick itself.
func TestAutoUpdateFetchFailureNoOp(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	withAutoUpdateSeams(t, "1.0.0",
		func(context.Context, string, string, string, string) (versionCheckResponse, error) {
			return versionCheckResponse{}, errors.New("network down")
		},
		func(context.Context, string) error { t.Fatal("no npm on fetch failure"); return nil },
		func(context.Context, commentbus.Paths) error { t.Fatal("no bus install on fetch failure"); return nil },
		func(context.Context, commentbus.Paths) error { return nil },
	)

	if runAutoUpdateWorkerOnce(context.Background(), paths, "") {
		t.Fatal("worker should no-op on fetch failure")
	}
	if _, ok := commentbus.ReadAutoUpdateJournal(paths); ok {
		t.Fatal("no journal should be written on fetch failure")
	}
}

// TestAutoUpdateHealthSurface verifies AutoUpdateHealth maps the cached state
// into the documented health fields.
func TestAutoUpdateHealthSurface(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	if err := commentbus.WriteAutoUpdateState(paths, commentbus.AutoUpdateState{
		LatestVersion: "1.2.0", UpdateAvailable: true, LastUpdateAt: "2026-06-10T12:00:00Z",
		LastUpdateResult: commentbus.AutoUpdateResultSuccess,
	}); err != nil {
		t.Fatal(err)
	}

	h := commentbus.AutoUpdateHealth(paths, "1.0.0")
	if h["current_version"] != "1.0.0" || h["latest_version"] != "1.2.0" {
		t.Fatalf("versions = %v / %v", h["current_version"], h["latest_version"])
	}
	if h["update_available"] != true {
		t.Fatalf("update_available = %v, want true", h["update_available"])
	}
	if h["last_update_result"] != commentbus.AutoUpdateResultSuccess {
		t.Fatalf("last_update_result = %v", h["last_update_result"])
	}

	// Empty state reports the documented defaults.
	empty := commentbus.AutoUpdateHealth(testAutoUpdatePaths(t), "1.0.0")
	if empty["latest_version"] != nil || empty["update_available"] != false {
		t.Fatalf("empty state = %v", empty)
	}
	if empty["last_update_result"] != commentbus.AutoUpdateResultNone {
		t.Fatalf("empty last_update_result = %v, want none", empty["last_update_result"])
	}
}

// TestDefaultAutoUpdateHealthCheckFailsOnProfileLoadErrors — LoadProfileState
// reports per-profile failures in its []ProfileReloadError return, not as an
// error. The post-update health check must fail (and trigger the rollback)
// when the new binary cannot load existing agent profiles, instead of
// committing an upgrade that stranded every installed agent.
func TestDefaultAutoUpdateHealthCheckFailsOnProfileLoadErrors(t *testing.T) {
	// Isolate $HOME: the health check resolves the default Botlets home under
	// it, and the developer machine's real ~/botlets must not leak in.
	t.Setenv("HOME", t.TempDir())
	paths := testAutoUpdatePaths(t)

	if err := defaultAutoUpdateHealthCheck(context.Background(), paths, ""); err != nil {
		t.Fatalf("health check with no profiles = %v, want nil", err)
	}

	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	brokenPath := filepath.Join(agentsDir, "max.broken.json")
	if err := os.WriteFile(brokenPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := defaultAutoUpdateHealthCheck(context.Background(), paths, "")
	if err == nil {
		t.Fatal("health check must fail when a profile cannot be loaded")
	}
	if !strings.Contains(err.Error(), "profile") {
		t.Fatalf("health check err = %v, want a profile-load failure", err)
	}

	if removeErr := os.Remove(brokenPath); removeErr != nil {
		t.Fatal(removeErr)
	}
	if err := defaultAutoUpdateHealthCheck(context.Background(), paths, ""); err != nil {
		t.Fatalf("health check after removing the broken profile = %v, want nil", err)
	}
}

// TestAutoUpdatePackageNameFromSpec covers the npm spec name parsing edge cases.
func TestAutoUpdatePackageNameFromSpec(t *testing.T) {
	cases := map[string]string{
		"@comment-io/cli@latest": "@comment-io/cli",
		"@comment-io/cli@1.2.3":  "@comment-io/cli",
		"@comment-io/cli":        "@comment-io/cli",
		"comment-io-cli@1.2.3":   "comment-io-cli",
		"comment-io-cli":         "comment-io-cli",
	}
	for spec, want := range cases {
		if got := npmPackageNameFromSpec(spec); got != want {
			t.Errorf("npmPackageNameFromSpec(%q) = %q, want %q", spec, got, want)
		}
	}
}

// TestAutoUpdateBusInstallCarriesBotletsHome verifies the unattended reinstall
// threads the running daemon's --botlets-home into busInstall: dropping it
// would restart the service against the persisted/default Botlets home instead
// of the one this daemon's profiles and team runtime actually live in
// (Codex round-5).
func TestAutoUpdateBusInstallCarriesBotletsHome(t *testing.T) {
	paths := testAutoUpdatePaths(t)
	withAutoUpdateSeams(t, "1.0.0",
		okFetch("1.2.0", "1.0.0"),
		func(context.Context, string) error { return nil },
		func(context.Context, commentbus.Paths) error { return nil },
		func(context.Context, commentbus.Paths) error { return nil },
	)
	gotHomes := []string{}
	autoUpdateRunBusInstall = func(_ context.Context, _ commentbus.Paths, botletsHome string) error {
		gotHomes = append(gotHomes, botletsHome)
		return nil
	}

	if !runAutoUpdateWorkerOnce(context.Background(), paths, "/custom/botlets-home") {
		t.Fatal("worker should have initiated an upgrade")
	}
	if len(gotHomes) != 1 || gotHomes[0] != "/custom/botlets-home" {
		t.Fatalf("bus install botlets homes = %v, want the daemon's runtime hint carried through", gotHomes)
	}
}
