package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// auto_update_worker.go is the long-running daemon auto-updater (Phase 7,
// policy: AUTO-APPLY EVERYTHING with a post-update health check and automatic
// rollback).
//
// The per-command version gate (version_check.go / enforceCLIVersion) only
// upgrades a foreground command when it is below the server MINIMUM. A
// persistent daemon, by contrast, may run untouched for days while new releases
// ship. This worker closes that gap: on start and every ~24h it asks the backend
// for the current {latest, minimum}, and when the running binary is behind the
// target it reinstalls the CLI and restarts itself onto the fresh binary.
//
// Safety comes from a rollback journal written BEFORE acting (see
// commentbus.AutoUpdateJournal). The bus-install restart kills this process, so
// the NEXT daemon start reconciles the journal in TWO phases:
//
//   - reconcileAutoUpdatePreStart (before commentbus.StartDaemon) counts a boot
//     attempt and, at the cap, rolls back. This catches a binary so broken it
//     crashes DURING StartDaemon and would never reach the post-start check.
//   - reconcileAutoUpdatePostStart (after StartDaemon succeeds) runs the real
//     health self-check: it commits a healthy upgrade, or rolls back an
//     unhealthy one immediately (a real bad signal needs no attempt cap).
//
// The service manager keeps restarting a crash-looping binary; each restart
// bumps the pre-start counter until the rollback fires, so a bad release
// self-heals without human intervention. After a rollback the worker remembers
// the bad toVersion (LastRolledBackVersion) and refuses to re-upgrade into it,
// preventing upgrade/rollback thrash on a still-`latest` bad release.
//
// This is purely additive: enforceCLIVersion is untouched, and every external
// effect (fetch, npm install, bus install, health check, sleep) is behind a
// package var so tests run hermetically without shelling out or sleeping.

const (
	// autoUpdateInterval is the steady-state check cadence. The daemon is
	// long-lived, so a daily check is plenty; a fresh release reaches an idle
	// daemon within a day without hammering the backend.
	autoUpdateInterval = 24 * time.Hour
	// autoUpdateMaxAttempts is the reconciliation attempt cap for the CRASH path
	// (a binary that never reaches a healthy post-start). Each boot is one
	// attempt; at the cap we roll back. 3 tolerates two transient first-boot
	// crashes (OS SIGKILL under memory pressure, a one-off port conflict) of an
	// otherwise-good release before giving up on it — important because the
	// post-rollback cooldown would then pin us OFF that version. A definitively
	// unhealthy-but-running binary is rolled back immediately by post-start and
	// does not wait for this cap.
	autoUpdateMaxAttempts    = 3
	autoUpdateRequestTimeout = 30 * time.Second
	autoUpdateNpmBinaryEnv   = "COMMENT_IO_NPM_BIN"
)

// Seams for tests. Production wiring lives in the defaults below.
var (
	autoUpdateNow   = time.Now
	autoUpdateFetch = fetchCLIVersion

	// autoUpdateRunNpmInstall installs an exact npm package spec globally
	// (`npm install -g <pkg>@<version>`). Stubbed in tests so they never shell
	// out.
	autoUpdateRunNpmInstall = func(ctx context.Context, spec string) error {
		npm, err := resolveAutoUpdateNpmBinary()
		if err != nil {
			return err
		}
		_, runErr := upgradeCombinedOutput(ctx, autoUpdateNpmCommandEnv(npm), npm, "install", "-g", spec)
		return runErr
	}

	// autoUpdateRunBusInstall reinstalls + restarts the daemon service pinned to
	// the freshly installed binary (the same work `comment bus install` does).
	// In production this restart kills the current daemon process; that is the
	// expected, desired outcome. Stubbed in tests so the process survives and the
	// post-action bookkeeping can be asserted inline.
	autoUpdateRunBusInstall = func(ctx context.Context, paths commentbus.Paths, botletsHome string) error {
		// pair=false: this is an unattended background reinstall, so the chained
		// device-pair flow must be skipped — it would block the restart waiting
		// for browser approval (service stdin is often /dev/null, a character
		// device, which the interactivity heuristic can misread as a human).
		//
		// botletsHome is the running daemon's --botlets-home value (the same
		// hint the other workers receive). Reinstalling with "" would drop a
		// runtime-selected home that is not persisted in bus config — the
		// restarted service would resolve the persisted/default home instead of
		// the one whose profiles/team runtime this daemon was actually using.
		_, err := busInstall(paths.Home, botletsHome, "", false, false)
		return err
	}

	// autoUpdateHealthCheck is the cheap, real post-update self-check run by
	// reconciliation. The default ensures the bus base dirs are present/writable
	// and that the agent profiles load — i.e. the daemon can actually function on
	// the new binary. Stubbed in tests to force healthy/unhealthy outcomes.
	autoUpdateHealthCheck = defaultAutoUpdateHealthCheck

	autoUpdateNpmFallbackPaths = defaultAutoUpdateNpmFallbackPaths
)

func resolveAutoUpdateNpmBinary() (string, error) {
	if pinned := strings.TrimSpace(os.Getenv(autoUpdateNpmBinaryEnv)); pinned != "" {
		if filepath.IsAbs(pinned) || strings.Contains(pinned, string(os.PathSeparator)) {
			clean := pinned
			if !filepath.IsAbs(clean) {
				abs, err := filepath.Abs(clean)
				if err != nil {
					return "", err
				}
				clean = abs
			}
			clean = filepath.Clean(clean)
			if err := validateUpgradeExecutable(clean, "npm binary"); err != nil {
				return "", err
			}
			return clean, nil
		}
		resolved, err := upgradeLookPath(pinned)
		if err != nil {
			return "", fmt.Errorf("%s=%q not found on PATH: %w", autoUpdateNpmBinaryEnv, pinned, err)
		}
		return resolved, nil
	}
	resolved, err := upgradeLookPath("npm")
	if err != nil || strings.TrimSpace(resolved) == "" {
		for _, candidate := range autoUpdateNpmFallbackPaths() {
			clean := filepath.Clean(strings.TrimSpace(candidate))
			if clean == "." || !filepath.IsAbs(clean) {
				continue
			}
			if validateUpgradeExecutable(clean, "npm binary") == nil {
				return clean, nil
			}
		}
		return "", fmt.Errorf("npm not found on PATH; reinstall the daemon from an interactive shell with npm available, or set %s to an absolute npm path", autoUpdateNpmBinaryEnv)
	}
	return resolved, nil
}

func defaultAutoUpdateNpmFallbackPaths() []string {
	paths := []string{
		"/opt/homebrew/bin/npm",
		"/usr/local/bin/npm",
		"/usr/bin/npm",
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		paths = append(paths,
			filepath.Join(home, ".local", "bin", "npm"),
			filepath.Join(home, ".npm-global", "bin", "npm"),
		)
	}
	return paths
}

func autoUpdateNpmCommandEnv(npm string) []string {
	dir := filepath.Dir(strings.TrimSpace(npm))
	if dir == "." || dir == "" {
		return nil
	}
	return envWithPrependedPath(os.Environ(), filepath.Clean(dir))
}

func envWithPrependedPath(env []string, dir string) []string {
	dir = strings.TrimSpace(dir)
	if dir == "" || dir == "." {
		return env
	}
	prefix := "PATH="
	sep := string(os.PathListSeparator)
	next := make([]string, 0, len(env)+1)
	replaced := false
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if !replaced {
				old := strings.TrimPrefix(entry, prefix)
				if old != "" {
					next = append(next, prefix+dir+sep+old)
				} else {
					next = append(next, prefix+dir)
				}
				replaced = true
			}
			continue
		}
		next = append(next, entry)
	}
	if !replaced {
		next = append(next, prefix+dir)
	}
	return next
}

func startAutoUpdateWorker(ctx context.Context, paths commentbus.Paths, botletsHome string) {
	go runAutoUpdateWorker(ctx, paths, botletsHome, autoUpdateInterval)
}

func runAutoUpdateWorker(ctx context.Context, paths commentbus.Paths, botletsHome string, interval time.Duration) {
	if interval <= 0 {
		interval = autoUpdateInterval
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			runAutoUpdateWorkerOnce(ctx, paths, botletsHome)
			timer.Reset(interval)
		}
	}
}

// runAutoUpdateWorkerOnce performs one check. It fetches {latest, minimum},
// refreshes the cached health state, and — when the running binary is behind the
// target — performs the upgrade (which normally does not return, because the bus
// install restarts the daemon). Returns true when an upgrade was initiated.
func runAutoUpdateWorkerOnce(ctx context.Context, paths commentbus.Paths, botletsHome string) bool {
	if ctx.Err() != nil {
		return false
	}
	fetchCtx, cancel := context.WithTimeout(ctx, autoUpdateRequestTimeout)
	defer cancel()
	resp, err := autoUpdateFetch(fetchCtx, versionCheckBaseURL(), version, cliInstanceID(), cliOSArch())
	if err != nil {
		// Offline or backend down: never act on missing data. Leave the cached
		// health state untouched and try again next tick.
		writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.fetch_failed", map[string]any{
			"error": err.Error(),
		})
		return false
	}

	target := autoUpdateTargetVersion(resp.Latest, resp.Minimum)
	outdated := target != "" && cliVersionOutdated(version, target)
	// Refresh the cached health view every tick so `bus health` reflects the
	// latest fetch even when no upgrade is needed.
	recordAutoUpdateLatest(paths, strings.TrimSpace(target), outdated)
	if !outdated {
		return false
	}
	// Anti-thrash: if we previously rolled back FROM exactly this target, do not
	// re-upgrade into the same known-bad release. Otherwise every 24h tick would
	// re-install the bad `latest`, fail its health check, and roll back again.
	state := commentbus.ReadAutoUpdateState(paths)
	if state.LastUpdateResult == commentbus.AutoUpdateResultRolledBack &&
		strings.TrimSpace(state.LastRolledBackVersion) == strings.TrimSpace(target) {
		writeDaemonWorkerLog(paths, "auto.update", "info", "auto_update.skip_rolled_back", map[string]any{
			"target": strings.TrimSpace(target),
		})
		return false
	}
	performAutoUpdate(ctx, paths, botletsHome, version, target)
	return true
}

// autoUpdateTargetVersion is the version the daemon should be on: the server's
// latest, but never below the required minimum. A misconfigured backend whose
// latest is below its minimum must still pull the daemon up to the minimum, not
// down to a stale latest.
func autoUpdateTargetVersion(latest string, minimum string) string {
	latest = strings.TrimSpace(latest)
	minimum = strings.TrimSpace(minimum)
	switch {
	case latest == "":
		return minimum
	case minimum == "":
		return latest
	case cliVersionOutdated(latest, minimum): // latest < minimum
		return minimum
	default:
		return latest
	}
}

// performAutoUpdate writes the rollback journal BEFORE acting, then installs the
// target and restarts the daemon. In production the bus install restarts the
// service and this process never returns; the next start reconciles the journal.
func performAutoUpdate(ctx context.Context, paths commentbus.Paths, botletsHome string, fromVersion string, toVersion string) {
	packageName := autoUpdatePackageName()
	journal := commentbus.AutoUpdateJournal{
		FromVersion: fromVersion,
		ToVersion:   toVersion,
		PackageName: packageName,
		Attempts:    0,
		StartedAt:   autoUpdateNow().UTC(),
	}
	if err := commentbus.WriteAutoUpdateJournal(paths, journal); err != nil {
		writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.journal_write_failed", map[string]any{
			"error": err.Error(),
		})
		return
	}
	writeDaemonWorkerLog(paths, "auto.update", "info", "auto_update.upgrade_started", map[string]any{
		"from_version": fromVersion,
		"to_version":   toVersion,
		"package":      packageName,
	})

	spec := packageName + "@" + toVersion
	if err := autoUpdateRunNpmInstall(ctx, spec); err != nil {
		// Install failed: the binary is unchanged. Leave the journal in place so
		// the next start (still on fromVersion) reconciles it as "not applied".
		writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.npm_install_failed", map[string]any{
			"spec":  spec,
			"error": err.Error(),
		})
		return
	}
	if err := autoUpdateRunBusInstall(ctx, paths, botletsHome); err != nil {
		writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.bus_install_failed", map[string]any{
			"error": err.Error(),
		})
		return
	}
	// Unreachable in production: bus install restarted the service and killed us.
	writeDaemonWorkerLog(paths, "auto.update", "info", "auto_update.bus_install_returned", map[string]any{
		"to_version": toVersion,
	})
}

// reconcileAutoUpdatePreStart runs at the very TOP of daemon startup, BEFORE
// commentbus.StartDaemon. Its job is to catch a binary that is so broken it
// crashes DURING StartDaemon: by counting a boot attempt before StartDaemon
// runs (and rolling back at the cap), a crash-on-start release self-heals even
// though it never reaches the post-start health check. It also handles the
// "install never applied" case (we are still on fromVersion / some other
// version) so a silently-failed npm install gives up at the cap.
func reconcileAutoUpdatePreStart(ctx context.Context, paths commentbus.Paths, botletsHome string) {
	journal, ok := commentbus.ReadAutoUpdateJournal(paths)
	if !ok {
		return // no pending update — nothing to reconcile
	}

	if version == strings.TrimSpace(journal.ToVersion) {
		// We ARE the new binary, but have not yet committed (post-start health
		// check hasn't passed). Count this boot. If StartDaemon keeps crashing,
		// each restart re-enters here and bumps the counter until the cap, at
		// which point we roll back to fromVersion without ever needing a health
		// signal — the crash IS the signal.
		journal.Attempts++
		if journal.Attempts >= autoUpdateMaxAttempts {
			rollbackAutoUpdate(ctx, paths, botletsHome, journal)
			return
		}
		// Persist the attempt BEFORE StartDaemon, so a crash during StartDaemon
		// still leaves the bumped counter on disk for the next boot.
		_ = commentbus.WriteAutoUpdateJournal(paths, journal)
		writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.boot_attempt", map[string]any{
			"to_version": journal.ToVersion,
			"attempts":   journal.Attempts,
		})
		return
	}

	// The binary is NOT the version we tried to install: the install never
	// applied (npm failed silently, a partial install, or a manual revert), or
	// we are back on fromVersion after a rollback restart. Count the attempt; at
	// the cap, give up and clear the journal — there is nothing to roll back TO
	// because the bad version never landed. Record rolled_back (with the bad
	// toVersion) so the worker won't re-upgrade into it.
	journal.Attempts++
	if journal.Attempts >= autoUpdateMaxAttempts {
		recordAutoUpdateRolledBack(paths, journal.ToVersion)
		_ = commentbus.DeleteAutoUpdateJournal(paths)
		writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.gave_up", map[string]any{
			"current_version": version,
			"to_version":      journal.ToVersion,
			"attempts":        journal.Attempts,
		})
		return
	}
	_ = commentbus.WriteAutoUpdateJournal(paths, journal)
	writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.install_not_applied", map[string]any{
		"current_version": version,
		"to_version":      journal.ToVersion,
		"attempts":        journal.Attempts,
	})
}

// reconcileAutoUpdatePostStart runs AFTER commentbus.StartDaemon succeeds. By
// this point the new binary has proven it can at least boot the daemon, so we
// run the real post-update health check and either commit (healthy) or roll
// back immediately (unhealthy — we have a definitive bad signal, no need to
// wait for the attempt cap).
func reconcileAutoUpdatePostStart(ctx context.Context, paths commentbus.Paths, botletsHome string) {
	journal, ok := commentbus.ReadAutoUpdateJournal(paths)
	if !ok {
		return // no pending update — nothing to reconcile
	}
	if version != strings.TrimSpace(journal.ToVersion) {
		return // not our pending update (pre-start already handled non-applied)
	}

	if err := autoUpdateHealthCheck(ctx, paths, botletsHome); err == nil {
		// Healthy: commit and forget where we came from.
		_ = commentbus.DeleteAutoUpdateJournal(paths)
		recordAutoUpdateResult(paths, commentbus.AutoUpdateResultSuccess)
		writeDaemonWorkerLog(paths, "auto.update", "info", "auto_update.committed", map[string]any{
			"from_version": journal.FromVersion,
			"to_version":   journal.ToVersion,
		})
		return
	}

	// Started but definitively unhealthy: roll back right now.
	writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.health_failed", map[string]any{
		"to_version": journal.ToVersion,
		"attempts":   journal.Attempts,
	})
	rollbackAutoUpdate(ctx, paths, botletsHome, journal)
}

// rollbackAutoUpdate reinstalls the prior version and restarts the daemon.
//
// Critically, if the npm install of the OLD version FAILS, there is no restored
// binary to restart onto — calling bus install would just restart the SAME bad
// binary and loop forever. So on npm failure we record rolled_back (so the
// worker won't re-attempt this toVersion), clear the journal, and RETURN without
// restarting. Only a SUCCESSFUL npm install proceeds to bus install.
//
// The rolled_back outcome (and journal deletion) is persisted BEFORE the
// process-killing bus install — otherwise, in the post-start health-fail path
// where the journal is only at attempt 1, the restarted old binary would never
// see the anti-thrash lock, its worker tick would re-upgrade into the same bad
// toVersion, and the daemon would thrash on every restart cycle. Recording
// first makes the lock durable regardless of when bus install kills us.
func rollbackAutoUpdate(ctx context.Context, paths commentbus.Paths, botletsHome string, journal commentbus.AutoUpdateJournal) {
	writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.rollback", map[string]any{
		"from_version": journal.FromVersion,
		"to_version":   journal.ToVersion,
		"attempts":     journal.Attempts,
	})
	// Durable anti-thrash lock first: the worker on the restarted binary will
	// skip re-upgrading into toVersion, and pre-start finds no journal to retry.
	recordAutoUpdateRolledBack(paths, journal.ToVersion)
	_ = commentbus.DeleteAutoUpdateJournal(paths)

	spec := strings.TrimSpace(journal.PackageName) + "@" + strings.TrimSpace(journal.FromVersion)
	if err := autoUpdateRunNpmInstall(ctx, spec); err != nil {
		// No restored binary exists — do NOT restart onto the still-broken one.
		// The lock is already recorded above, so the daemon stays on the bad
		// binary but does not thrash.
		writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.rollback_npm_failed", map[string]any{
			"spec":  spec,
			"error": err.Error(),
		})
		return
	}
	if err := autoUpdateRunBusInstall(ctx, paths, botletsHome); err != nil {
		writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.rollback_bus_install_failed", map[string]any{
			"error": err.Error(),
		})
	}
	// In production bus install already killed us here; the restarted fromVersion
	// binary boots with the lock recorded and no journal.
}

// recordAutoUpdateLatest refreshes the cached latest-version / update-available
// fields without clobbering the last reconciliation result.
func recordAutoUpdateLatest(paths commentbus.Paths, latest string, updateAvailable bool) {
	state := commentbus.ReadAutoUpdateState(paths)
	state.LatestVersion = latest
	state.UpdateAvailable = updateAvailable
	if err := commentbus.WriteAutoUpdateState(paths, state); err != nil {
		writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.state_write_failed", map[string]any{
			"error": err.Error(),
		})
	}
}

// recordAutoUpdateResult stamps the terminal reconciliation outcome and time
// without clobbering the cached latest-version fields. On a successful commit it
// also clears LastRolledBackVersion: a release that previously rolled back may
// later become installable (e.g. a re-published fix), and once we successfully
// land it the anti-thrash skip must no longer apply.
func recordAutoUpdateResult(paths commentbus.Paths, result string) {
	state := commentbus.ReadAutoUpdateState(paths)
	state.LastUpdateResult = result
	state.LastUpdateAt = autoUpdateNow().UTC().Format(time.RFC3339)
	if result == commentbus.AutoUpdateResultSuccess {
		state.LastRolledBackVersion = ""
	}
	if err := commentbus.WriteAutoUpdateState(paths, state); err != nil {
		writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.state_write_failed", map[string]any{
			"error": err.Error(),
		})
	}
}

// recordAutoUpdateRolledBack stamps a rolled-back outcome and remembers the
// toVersion we rolled back FROM, without clobbering the cached latest-version
// fields. The worker reads LastRolledBackVersion to avoid re-upgrading into the
// same known-bad release (anti-thrash).
func recordAutoUpdateRolledBack(paths commentbus.Paths, rolledBackVersion string) {
	state := commentbus.ReadAutoUpdateState(paths)
	state.LastUpdateResult = commentbus.AutoUpdateResultRolledBack
	state.LastRolledBackVersion = strings.TrimSpace(rolledBackVersion)
	state.LastUpdateAt = autoUpdateNow().UTC().Format(time.RFC3339)
	if err := commentbus.WriteAutoUpdateState(paths, state); err != nil {
		writeDaemonWorkerLog(paths, "auto.update", "warn", "auto_update.state_write_failed", map[string]any{
			"error": err.Error(),
		})
	}
}

// autoUpdatePackageName is the npm package name (no version suffix) the daemon
// installs, derived from the same spec the foreground upgrade path uses so the
// COMMENT_IO_CLI_PACKAGE override is honored.
func autoUpdatePackageName() string {
	return npmPackageNameFromSpec(defaultUpgradePackage())
}

// npmPackageNameFromSpec strips a trailing `@version` from an npm package spec,
// preserving a leading scope. "@comment-io/cli@latest" -> "@comment-io/cli";
// "comment-io-cli@1.2.3" -> "comment-io-cli"; an already-bare name is returned
// unchanged.
func npmPackageNameFromSpec(spec string) string {
	spec = strings.TrimSpace(spec)
	if at := strings.LastIndex(spec, "@"); at > 0 {
		return spec[:at]
	}
	return spec
}

// defaultAutoUpdateHealthCheck is the production post-update self-check: the bus
// base dirs are present/writable and the agent profiles load. It is intentionally
// cheap and dependency-free so it cannot itself wedge a restart.
func defaultAutoUpdateHealthCheck(ctx context.Context, paths commentbus.Paths, botletsHome string) error {
	if err := commentbus.EnsureBaseDirs(paths); err != nil {
		return err
	}
	// LoadProfileState reports per-profile failures in its second return, not
	// as an error. A new binary that cannot load the existing agent/Botlets
	// profiles (profile schema regression, trust-path change, unreadable
	// registry) is exactly the breakage this check exists to catch — committing
	// such an upgrade would strand every installed agent — so any reload error
	// fails the check and triggers the rollback.
	// Resolve the home the RUNNING daemon actually uses: an explicit
	// --botlets-home passed to the daemon is not persisted in bus config, so
	// falling back to persistedCLIBotletsHome(paths, "") would validate the
	// wrong home and could commit an upgrade whose new binary cannot load the
	// profiles/registry the daemon is really serving. botletsHome takes
	// priority; an empty value falls back to the persisted/default home.
	_, loadErrors := commentbus.LoadProfileState(ctx, commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: persistedCLIBotletsHome(paths, botletsHome),
	})
	if len(loadErrors) > 0 {
		first := loadErrors[0]
		detail := first.Message
		if first.Profile != "" {
			detail = first.Profile + ": " + detail
		}
		return fmt.Errorf("%d profile(s) failed to load on the new binary (%s)", len(loadErrors), detail)
	}
	return nil
}
