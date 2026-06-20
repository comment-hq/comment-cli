package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// Daemon-local enrollment write journal.
//
// The server's `cleanups` list only proves an enrollment was redeemed and
// later went terminal — it cannot prove THIS daemon ever wrote the profile
// (the daemon may have crashed between redeem and write, and the same handle
// may since have been installed manually or by another enrollment). The
// journal is the daemon's own evidence: just before an enrollment writes a
// profile, it records WHICH file it is about to write and the hash of the
// credential it is writing into it. Cleanup paths then only restore-or-remove
// a profile they can attribute to the enrollment via that hash; without an
// entry (or with a mismatching hash) they leave the file alone and merely
// confirm server-side so the item drains.
//
// Entries are pruned when a terminal enrollment's cleanup is confirmed
// server-side. Entries for SUCCESSFULLY installed enrollments are deliberately
// kept: they are the record `comment bus unpair` uses to remove the profiles
// whose daemon-minted credentials its self-revoke just invalidated (manual
// installs have no entry and are never touched).

// enrollJournalFileName lives next to daemon-auth.json in the private bus
// directory — NOT under agents/ where the daemon's *.json profile glob (Go's
// filepath.Match `*` matches leading dots) would pick it up as a profile.
const enrollJournalFileName = "enroll-journal.json"

type enrollJournalEntry struct {
	EnrollmentID string `json:"enrollment_id"`
	DaemonID     string `json:"daemon_id,omitempty"`
	Handle       string `json:"handle"`
	ProfilePath  string `json:"profile_path"`
	// SecretSHA256 is the hex sha256 of the agent_secret this enrollment wrote
	// (about to write) into the profile — the attribution key. The plaintext
	// secret deliberately never rests in the journal.
	SecretSHA256 string `json:"secret_sha256"`
	// PrevSecretSHA256 carries the hashes of credentials EARLIER passes of THIS
	// enrollment already wrote to disk. On a retry the same redeemed enrollment
	// can mint a fresh credential (a re-redeem swaps credential ids); the
	// pre-write journal upsert records the new hash BEFORE write.Write() lands,
	// so a crash in that window would otherwise leave the journal pointing only
	// at the new hash while disk still holds the prior (now-revoked) credential
	// — terminal cleanup would then mis-attribute that profile as unowned and
	// abandon it. Keeping the prior hashes as additional attribution keys
	// (preserve-old-until-replacement-lands) means cleanup still owns the file
	// whichever of this enrollment's credentials is actually on disk; a credential
	// from a DIFFERENT install matches none of them and is correctly left alone.
	PrevSecretSHA256 []string `json:"prev_secret_sha256,omitempty"`
	// BotletsHandle/BotletsHome are set for Botlets installs so an attributed
	// REMOVAL can also roll back the registry entry the install upserted.
	BotletsHandle string `json:"botlets_handle,omitempty"`
	BotletsHome   string `json:"botlets_home,omitempty"`
	// PrevBotletsRegistry snapshots the pre-existing registry entry for this
	// handle (serialized commentbus.BotRegistryEntry) when a Botlets install
	// OVERWRITES a working install — i.e. the same case that takes a profile
	// backup. registerBotletsBotLocally upserts the handle's entry with the
	// enrollment's brain/runtime/timezone; if the enrollment then goes terminal
	// and cleanup RESTORES the profile backup, the entry must be rolled back to
	// this snapshot too, or the restored credential would run with the dead
	// enrollment's registry state (wrong brain, reset timezone). Absent when the
	// install created the handle fresh (cleanup then removes the entry outright).
	PrevBotletsRegistry json.RawMessage `json:"prev_botlets_registry,omitempty"`
	WrittenAt           string          `json:"written_at"`
}

func enrollJournalPath(paths commentbus.Paths) string {
	return filepath.Join(paths.Bus, enrollJournalFileName)
}

func enrollSecretSHA256(agentSecret string) string {
	sum := sha256.Sum256([]byte(agentSecret))
	return hex.EncodeToString(sum[:])
}

// enrollJournalLoad reads the journal; a missing file is an empty journal.
func enrollJournalLoad(paths commentbus.Paths) (map[string]enrollJournalEntry, error) {
	data, err := os.ReadFile(enrollJournalPath(paths))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]enrollJournalEntry{}, nil
	}
	if err != nil {
		return nil, err
	}
	entries := map[string]enrollJournalEntry{}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func enrollJournalSave(paths commentbus.Paths, entries map[string]enrollJournalEntry) error {
	if err := os.MkdirAll(paths.Bus, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return commentbus.WritePrivateFileAtomicExistingDir(enrollJournalPath(paths), append(data, '\n'), 0o600)
}

// enrollJournalStampMissingDaemonID backfills current-daemon attribution onto
// older successful-install journal entries before a force re-pair replaces the
// local daemon token. The entries then let the new daemon recognize preserved
// old-daemon credentials and atomically refresh them through normal enrollment
// instead of deleting profile files during re-pair. For cross-base re-pairs,
// the same attribution prevents a later unpair of the new daemon from treating
// old-base legacy entries as belonging to the new daemon.
func enrollJournalStampMissingDaemonID(paths commentbus.Paths, daemonID string) error {
	daemonID = strings.TrimSpace(daemonID)
	if daemonID == "" {
		return nil
	}
	entries, err := enrollJournalLoad(paths)
	if err != nil {
		return err
	}
	changed := false
	for id, entry := range entries {
		if strings.TrimSpace(entry.DaemonID) != "" {
			continue
		}
		entry.DaemonID = daemonID
		entries[id] = entry
		changed = true
	}
	if !changed {
		return nil
	}
	return enrollJournalSave(paths, entries)
}

// enrollJournalRecord upserts one entry keyed by enrollment id. Called just
// BEFORE the profile write so a crash mid-write still leaves the evidence the
// later cleanup needs (acting on an entry whose write never landed is safe:
// restore-or-remove on a missing file is a no-op, and the hash check refuses
// to touch anyone else's file).
func enrollJournalRecord(paths commentbus.Paths, entry enrollJournalEntry) error {
	if entry.WrittenAt == "" {
		entry.WrittenAt = time.Now().UTC().Format(time.RFC3339)
	}
	entries, err := enrollJournalLoad(paths)
	if err != nil {
		return err
	}
	if existing, ok := entries[entry.EnrollmentID]; ok {
		// Preserve every credential an earlier pass of this enrollment already
		// wrote (the prior hash, plus any it had itself preserved) as additional
		// attribution keys, so a crash between this upsert and the replacement
		// write cannot orphan the still-on-disk prior credential.
		prior := append([]string{existing.SecretSHA256}, existing.PrevSecretSHA256...)
		prior = append(prior, entry.PrevSecretSHA256...)
		entry.PrevSecretSHA256 = distinctEnrollSecretHashes(prior, entry.SecretSHA256)
		// Preserve the FIRST pass's pre-existing registry snapshot (first writer
		// wins, mirroring the profile .enroll-backup): a retried pass sees the
		// now-enrollment-owned profile, takes no fresh backup, and sets no
		// snapshot — but the original working entry must survive so a restore-
		// time rollback can put it back.
		if len(entry.PrevBotletsRegistry) == 0 {
			entry.PrevBotletsRegistry = existing.PrevBotletsRegistry
		}
	}
	entries[entry.EnrollmentID] = entry
	return enrollJournalSave(paths, entries)
}

// distinctEnrollSecretHashes returns the distinct, non-empty hashes from in,
// excluding the enrollment's current hash, so PrevSecretSHA256 stays the set of
// EARLIER credentials this enrollment wrote.
func distinctEnrollSecretHashes(in []string, current string) []string {
	seen := map[string]bool{}
	var out []string
	for _, h := range in {
		if h == "" || h == current || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// enrollJournalLookup reports (entry, journaled, indeterminate). indeterminate
// is true when the journal file exists but cannot be read or parsed (a missing
// journal is a clean not-journaled, NOT indeterminate, since enrollJournalLoad
// maps ErrNotExist to an empty map). A caller must not confirm-drain a cleanup
// on indeterminate — the journal may attribute the very profile being cleaned.
func enrollJournalLookup(paths commentbus.Paths, enrollmentID string) (entry enrollJournalEntry, journaled bool, indeterminate bool) {
	entries, err := enrollJournalLoad(paths)
	if err != nil {
		return enrollJournalEntry{}, false, true
	}
	e, ok := entries[enrollmentID]
	return e, ok, false
}

// enrollJournalRemove drops one entry; best-effort (a journal that cannot be
// rewritten only means a stale entry whose hash no longer matches anything).
func enrollJournalRemove(paths commentbus.Paths, enrollmentID string) {
	entries, err := enrollJournalLoad(paths)
	if err != nil {
		return
	}
	if _, ok := entries[enrollmentID]; !ok {
		return
	}
	delete(entries, enrollmentID)
	_ = enrollJournalSave(paths, entries)
}

// enrollJournalProfileSecretMatches reports whether the profile file at the
// entry's path currently holds a credential this enrollment wrote (its current
// hash or any preserved prior hash). A missing file reports (false, false,
// false): nothing to attribute. A non-not-exist read error (permissions/
// transient) reports indeterminate=true: ownership cannot be determined, so the
// caller must retry rather than treat it as a definitive foreign-credential
// mismatch and prune the journal / abandon a possibly-ours revoked profile.
func enrollJournalProfileSecretMatches(entry enrollJournalEntry) (matches bool, fileExists bool, indeterminate bool) {
	data, err := os.ReadFile(entry.ProfilePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, false
		}
		return false, true, true
	}
	var profile struct {
		AgentSecret string `json:"agent_secret"`
	}
	if err := json.Unmarshal(data, &profile); err != nil {
		return false, true, false
	}
	if profile.AgentSecret == "" {
		return false, true, false
	}
	sum := enrollSecretSHA256(profile.AgentSecret)
	if sum == entry.SecretSHA256 {
		return true, true, false
	}
	for _, prev := range entry.PrevSecretSHA256 {
		if sum == prev {
			return true, true, false
		}
	}
	return false, true, false
}

// enrollJournalProfileNeedsDaemonRefresh reports whether a local profile is a
// journal-attributed credential minted by a different daemon than the caller.
// This is the force re-pair bridge: preserved files keep running sessions alive
// immediately, but the new daemon still self-enrolls replacements instead of
// forever treating old-daemon credentials as installed.
func enrollJournalProfileNeedsDaemonRefresh(paths commentbus.Paths, handle string, profilePath string, currentDaemonID string) bool {
	currentDaemonID = strings.TrimSpace(currentDaemonID)
	if currentDaemonID == "" {
		return false
	}
	entries, err := enrollJournalLoad(paths)
	if err != nil {
		return false
	}
	cleanProfilePath := filepath.Clean(profilePath)
	for _, entry := range entries {
		entryDaemonID := strings.TrimSpace(entry.DaemonID)
		if entryDaemonID == "" || entryDaemonID == currentDaemonID {
			continue
		}
		if strings.TrimSpace(entry.Handle) != handle || filepath.Clean(entry.ProfilePath) != cleanProfilePath {
			continue
		}
		matches, _, indeterminate := enrollJournalProfileSecretMatches(entry)
		if matches && !indeterminate {
			return true
		}
	}
	return false
}

// enrollJournalEntryOwnsProfile reports whether a terminal-enrollment cleanup
// may run restore-or-remove for this entry: the file is missing (our write
// never landed or was already cleaned — restore-or-remove is then a safe
// backup-restore or no-op) or it still holds a credential this enrollment
// wrote. A file holding ANY other credential belongs to a later install
// (manual or another enrollment) and must not be touched. indeterminate=true
// means the file could not be read to attribute it (permissions/transient): the
// caller must retry rather than prune or confirm-drain the entry.
func enrollJournalEntryOwnsProfile(entry enrollJournalEntry) (owns bool, indeterminate bool) {
	matches, fileExists, indeterminate := enrollJournalProfileSecretMatches(entry)
	if indeterminate {
		return false, true
	}
	return matches || !fileExists, false
}
