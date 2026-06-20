package commentbus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// auto_update_state.go owns the on-disk state the daemon auto-updater (Phase 7)
// reads and writes under the bus home. Two small files live here:
//
//   - pending-update.json (the rollback journal): written BEFORE an upgrade is
//     attempted so a crash-looping bad release can self-heal. It records where we
//     came from, where we are going, the npm package, an attempts counter, and
//     when the upgrade started. Startup reconciliation reads it to decide whether
//     to commit, retry, roll back, or give up.
//   - auto-update-state.json (health cache): the last fetched latest version,
//     whether an update is available, and the last reconciliation outcome. This is
//     the source of truth for the auto-update fields surfaced by `comment bus
//     health` and the daemon socket health op.
//
// These live in package commentbus (not package main with the worker) so the
// daemon socket health handler can read them without importing the CLI command
// package or duplicating the worker's version-comparison logic — the worker
// computes update_available and persists it as a bool here.

const (
	autoUpdateJournalFile = "pending-update.json"
	autoUpdateStateFile   = "auto-update-state.json"

	// AutoUpdateResult* are the terminal outcomes recorded by startup
	// reconciliation and surfaced as last_update_result in health.
	AutoUpdateResultNone       = "none"
	AutoUpdateResultSuccess    = "success"
	AutoUpdateResultRolledBack = "rolled_back"
)

// AutoUpdateJournal is the rollback journal written before an auto-upgrade acts.
type AutoUpdateJournal struct {
	FromVersion string    `json:"from_version"`
	ToVersion   string    `json:"to_version"`
	PackageName string    `json:"package_name"`
	Attempts    int       `json:"attempts"`
	StartedAt   time.Time `json:"started_at"`
}

// AutoUpdateState is the cached health view of the auto-updater.
type AutoUpdateState struct {
	LatestVersion    string `json:"latest_version,omitempty"`
	UpdateAvailable  bool   `json:"update_available"`
	LastUpdateAt     string `json:"last_update_at,omitempty"`
	LastUpdateResult string `json:"last_update_result,omitempty"`
	// LastRolledBackVersion is the toVersion the daemon last rolled back FROM.
	// While LastUpdateResult is rolled_back and this matches the fetched target,
	// the worker skips re-upgrading to the same known-bad release (anti-thrash).
	LastRolledBackVersion string `json:"last_rolled_back_version,omitempty"`
}

func autoUpdateJournalPath(paths Paths) string {
	return filepath.Join(paths.Bus, autoUpdateJournalFile)
}

func autoUpdateStatePath(paths Paths) string {
	return filepath.Join(paths.Bus, autoUpdateStateFile)
}

// ReadAutoUpdateJournal returns the pending-update journal and whether it
// exists. A missing file reports (zero, false); an unreadable/corrupt file is
// treated the same so a garbage journal never wedges startup — it is simply
// re-evaluated as "no pending update".
func ReadAutoUpdateJournal(paths Paths) (AutoUpdateJournal, bool) {
	data, err := os.ReadFile(autoUpdateJournalPath(paths))
	if err != nil {
		return AutoUpdateJournal{}, false
	}
	var journal AutoUpdateJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return AutoUpdateJournal{}, false
	}
	if strings.TrimSpace(journal.ToVersion) == "" {
		return AutoUpdateJournal{}, false
	}
	return journal, true
}

// WriteAutoUpdateJournal persists the rollback journal atomically.
func WriteAutoUpdateJournal(paths Paths, journal AutoUpdateJournal) error {
	if err := os.MkdirAll(paths.Bus, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return writeFileAtomic(autoUpdateJournalPath(paths), data, 0o600)
}

// DeleteAutoUpdateJournal removes the journal. A missing file is not an error.
func DeleteAutoUpdateJournal(paths Paths) error {
	if err := os.Remove(autoUpdateJournalPath(paths)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ReadAutoUpdateState returns the cached health state. A missing or corrupt file
// reports the zero value (treated as "none").
func ReadAutoUpdateState(paths Paths) AutoUpdateState {
	data, err := os.ReadFile(autoUpdateStatePath(paths))
	if err != nil {
		return AutoUpdateState{}
	}
	var state AutoUpdateState
	if err := json.Unmarshal(data, &state); err != nil {
		return AutoUpdateState{}
	}
	return state
}

// WriteAutoUpdateState persists the cached health state atomically.
func WriteAutoUpdateState(paths Paths, state AutoUpdateState) error {
	if err := os.MkdirAll(paths.Bus, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return writeFileAtomic(autoUpdateStatePath(paths), data, 0o600)
}

// AutoUpdateHealth builds the auto-update fields for the health surfaces. It
// reads only the cached state file — the worker (which owns version parsing)
// has already computed update_available — so this stays free of any version
// comparison logic.
func AutoUpdateHealth(paths Paths, currentVersion string) map[string]any {
	state := ReadAutoUpdateState(paths)
	result := strings.TrimSpace(state.LastUpdateResult)
	if result == "" {
		result = AutoUpdateResultNone
	}
	return map[string]any{
		"current_version":    currentVersion,
		"latest_version":     emptyStringToNil(state.LatestVersion),
		"update_available":   state.UpdateAvailable,
		"last_update_at":     emptyStringToNil(state.LastUpdateAt),
		"last_update_result": result,
	}
}

func emptyStringToNil(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
