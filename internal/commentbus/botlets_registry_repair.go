package commentbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type botletsRepairAgentProfileWrite struct {
	path    string
	data    []byte
	profile AgentProfile
}

type botletsRepairAgentProfileAliasWrite struct {
	path string
	data []byte
}

type botletsRepairCredentialProfileRename struct {
	entry          BotRegistryEntry
	profileWrite   botletsRepairAgentProfileWrite
	writeCanonical bool
	aliasWrites    []botletsRepairAgentProfileAliasWrite
}

func repairBotletsRegistryFromCloudTask(ctx context.Context, paths Paths, botletsHome string, current BotRegistryEntry, task *CloudBotletsTaskNotification) (BotRegistryEntry, bool, error) {
	if !cloudBotletsTaskSuggestsRegistryRename(current, task) {
		return BotRegistryEntry{}, false, nil
	}
	ref := *current.BrainRef
	ref.OwnerAgentID = task.OwnerAgentID
	ref.BotAgentID = task.BotAgentID
	ref.SetupGeneration = task.SetupGeneration
	candidate := current
	candidate.Name = task.BotSlug
	candidate.DisplayName = task.BotName
	candidate.BotID = firstNonEmptyForRepair(task.BotID, current.BotID)
	candidate.Handle = task.BotHandle
	candidate.CredentialProfile = current.CredentialProfile
	candidate.SlugAliases = mergeBotletsRegistryAliasesForRepair(task.BotSlug, append([]string{current.Name}, current.SlugAliases...), nil)
	candidate.HandleAliases = mergeBotletsRegistryAliasesForRepair(task.BotHandle, append([]string{current.Handle}, current.HandleAliases...), nil)
	candidate.BrainRef = &ref
	entry, err := upsertBotletsRegistryEntryForRepair(ctx, paths, botletsHome, candidate)
	if err != nil {
		return BotRegistryEntry{}, false, err
	}
	return entry, true, nil
}

func cloudBotletsTaskSuggestsRegistryRename(current BotRegistryEntry, task *CloudBotletsTaskNotification) bool {
	if task == nil || current.BrainRef == nil || !validateCloudBotletsTaskNotification(task) {
		return false
	}
	if !isBotName(task.BotSlug) || !ProfileRE.MatchString(task.BotHandle) {
		return false
	}
	if !current.MatchesStableIdentity(task.BotID, task.BotAgentID) {
		return false
	}
	if current.BotID != "" && task.BotID != "" && current.BotID != task.BotID {
		return false
	}
	if current.BrainRef.OwnerAgentID != "" && current.BrainRef.OwnerAgentID != task.OwnerAgentID {
		return false
	}
	if current.BrainRef.BotAgentID == "" || current.BrainRef.BotAgentID != task.BotAgentID {
		return false
	}
	if current.BrainRef.SetupGeneration > 0 && current.BrainRef.SetupGeneration != task.SetupGeneration {
		return false
	}
	return !strings.EqualFold(current.Name, task.BotSlug) || !strings.EqualFold(current.Handle, task.BotHandle) || !current.MatchesSlug(task.BotSlug) || !current.MatchesProfile(task.BotHandle)
}

func upsertBotletsRegistryEntryForRepair(ctx context.Context, paths Paths, botletsHome string, entry BotRegistryEntry) (BotRegistryEntry, error) {
	var err error
	botletsHome, err = ValidateBotletsRegistryWriteTarget(botletsHome)
	if err != nil {
		return BotRegistryEntry{}, err
	}
	if err := os.Mkdir(botletsHome, 0o700); err != nil && !os.IsExist(err) {
		return BotRegistryEntry{}, err
	}
	if botletsHome, err = ValidateBotletsRegistryWriteTarget(botletsHome); err != nil {
		return BotRegistryEntry{}, err
	}
	if err := os.Chmod(botletsHome, 0o700); err != nil {
		return BotRegistryEntry{}, err
	}
	if botletsHome, err = ValidateBotletsRegistryWriteTarget(botletsHome); err != nil {
		return BotRegistryEntry{}, err
	}
	lockFile, err := openBotletsRegistryRepairLock(filepath.Join(botletsHome, ".registry.lock"))
	if err != nil {
		return BotRegistryEntry{}, err
	}
	defer lockFile.Close()
	if err := lockFile.Chmod(0o600); err != nil {
		return BotRegistryEntry{}, err
	}
	if err := lockBotletsRegistryRepairFile(lockFile); err != nil {
		return BotRegistryEntry{}, err
	}
	defer unlockBotletsRegistryRepairFile(lockFile)
	if _, err := ValidateBotletsRegistryWriteTarget(botletsHome); err != nil {
		return BotRegistryEntry{}, err
	}
	registryPath := filepath.Join(botletsHome, "registry.json")
	var registry struct {
		Bots []BotRegistryEntry `json:"bots"`
	}
	var previousRegistryData []byte
	previousRegistryExists := false
	if data, err := os.ReadFile(registryPath); err == nil {
		previousRegistryData = append([]byte(nil), data...)
		previousRegistryExists = true
		if err := json.Unmarshal(data, &registry); err != nil {
			return BotRegistryEntry{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return BotRegistryEntry{}, err
	}
	nextBots := make([]BotRegistryEntry, 0, len(registry.Bots))
	var previousEntry *BotRegistryEntry
	for i := range registry.Bots {
		overlap, err := botletsRegistryEntriesOverlapForRepair(registry.Bots[i], entry)
		if err != nil {
			return BotRegistryEntry{}, err
		}
		if overlap {
			existing := registry.Bots[i]
			previousEntry = &existing
			entry = mergeBotletsRegistryEntryForRepair(existing, entry)
			continue
		}
		nextBots = append(nextBots, registry.Bots[i])
	}
	if previousEntry == nil {
		return BotRegistryEntry{}, errors.New("Botlets registry repair failed: bot was not found")
	}
	var extraProfiles map[string]AgentProfile
	var afterCommit func(BotRegistryEntry) error
	repair, ok, err := prepareBotletsCredentialProfileRenameRepair(paths, botletsHome, *previousEntry, entry)
	if err != nil {
		return BotRegistryEntry{}, err
	}
	if ok {
		entry = repair.entry
		if repair.writeCanonical {
			extraProfiles = map[string]AgentProfile{repair.profileWrite.profile.Handle: repair.profileWrite.profile}
		}
		afterCommit = func(BotRegistryEntry) error {
			if repair.writeCanonical {
				return writePreparedBotletsAgentProfileSetRollbackableForRepair(repair.profileWrite, repair.aliasWrites)
			}
			return writePreparedBotletsAgentProfileAliasesForRepair(repair.aliasWrites)
		}
	}
	registry.Bots = append(nextBots, entry)
	sort.Slice(registry.Bots, func(i, j int) bool {
		return registry.Bots[i].Name < registry.Bots[j].Name
	})
	profiles, _, profileErrors := LoadAgentProfilesWithAliases(ctx, paths, "")
	for handle, profile := range extraProfiles {
		profiles[handle] = profile
	}
	if HasFatalProfileReloadError(profileErrors) {
		return BotRegistryEntry{}, fmt.Errorf("Botlets registry validation failed: %+v", profileErrors)
	}
	stateBots, registryErrors := ValidateBotletsRegistryEntries(botletsHome, profiles, registry.Bots)
	errorsOut := append(profileErrors, registryErrors...)
	if HasFatalProfileReloadError(errorsOut) {
		return BotRegistryEntry{}, fmt.Errorf("Botlets registry validation failed: %+v", errorsOut)
	}
	if _, ok := stateBots[entry.Name]; !ok {
		return BotRegistryEntry{}, errors.New("Botlets registry validation failed: bot was not loaded")
	}
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return BotRegistryEntry{}, err
	}
	if err := WritePrivateFileAtomic(registryPath, append(data, '\n'), 0o600); err != nil {
		return BotRegistryEntry{}, err
	}
	if afterCommit != nil {
		if err := afterCommit(entry); err != nil {
			if rollbackErr := restoreBotletsRegistryFileForRepair(registryPath, previousRegistryData, previousRegistryExists); rollbackErr != nil {
				return BotRegistryEntry{}, fmt.Errorf("%w (registry rollback failed: %v)", err, rollbackErr)
			}
			return BotRegistryEntry{}, err
		}
	}
	return entry, nil
}

func prepareBotletsCredentialProfileRenameRepair(paths Paths, botletsHome string, previous BotRegistryEntry, candidate BotRegistryEntry) (botletsRepairCredentialProfileRename, bool, error) {
	if strings.EqualFold(previous.Handle, candidate.Handle) {
		return botletsRepairCredentialProfileRename{}, false, nil
	}
	botID := firstNonEmptyForRepair(candidate.BotID, previous.BotID)
	botAgentID := firstNonEmptyForRepair(botletsRegistryEntryBotAgentIDForRepair(candidate), botletsRegistryEntryBotAgentIDForRepair(previous))
	if !botletsRegistryEntryHasIdentityForRepair(previous, botID, botAgentID) || !botletsRegistryEntryHasIdentityForRepair(candidate, botID, botAgentID) {
		return botletsRepairCredentialProfileRename{}, false, nil
	}
	oldCredentialPath, ok, err := resolveBotletsRegistryCredentialPathForRepair(botletsHome, previous.CredentialProfile)
	if err != nil || !ok {
		return botletsRepairCredentialProfileRename{}, false, err
	}
	oldProfile, ok, err := loadBotletsAgentProfileByPathForRepair(paths, oldCredentialPath)
	if err != nil || !ok {
		return botletsRepairCredentialProfileRename{}, false, err
	}
	if !strings.EqualFold(oldProfile.Handle, previous.Handle) {
		return botletsRepairCredentialProfileRename{}, false, nil
	}
	profileWrite, err := prepareBotletsAgentProfileForRepair(paths, candidate.Handle, oldProfile.AgentSecret, oldProfile.BaseURL, firstNonEmptyForRepair(oldProfile.Runtime, previous.ManagedSession.Runtime))
	if err != nil {
		return botletsRepairCredentialProfileRename{}, false, err
	}
	targetCredentialPath, targetOK, err := resolveBotletsRegistryCredentialPathForRepair(botletsHome, candidate.CredentialProfile)
	if err != nil {
		return botletsRepairCredentialProfileRename{}, false, err
	}
	if targetOK && targetCredentialPath != oldCredentialPath && targetCredentialPath != profileWrite.path {
		return botletsRepairCredentialProfileRename{}, false, nil
	}
	writeCanonical, err := shouldWriteBotletsCanonicalProfileRepairTarget(profileWrite.path, candidate.Handle)
	if err != nil {
		return botletsRepairCredentialProfileRename{}, false, err
	}
	candidate.CredentialProfile = profileWrite.path
	aliasWrites, err := prepareBotletsAgentProfileAliasesForRepair(paths, botletsHome, candidate.Handle, candidate.HandleAliases, botID, botAgentID)
	if err != nil {
		return botletsRepairCredentialProfileRename{}, false, err
	}
	return botletsRepairCredentialProfileRename{entry: candidate, profileWrite: profileWrite, writeCanonical: writeCanonical, aliasWrites: aliasWrites}, true, nil
}

func prepareBotletsAgentProfileForRepair(paths Paths, handle string, agentSecret string, baseURL string, runtime string) (botletsRepairAgentProfileWrite, error) {
	if handle == "" || agentSecret == "" {
		return botletsRepairAgentProfileWrite{}, errors.New("missing Botlets agent credential")
	}
	if !ProfileRE.MatchString(handle) {
		return botletsRepairAgentProfileWrite{}, errors.New("invalid Botlets agent handle")
	}
	runtime = strings.TrimSpace(runtime)
	if runtime != "" && runtime != "claude" && runtime != "codex" {
		return botletsRepairAgentProfileWrite{}, errors.New("invalid Botlets runtime")
	}
	profilePath, err := ValidateAgentProfileWriteTarget(paths, handle)
	if err != nil {
		return botletsRepairAgentProfileWrite{}, err
	}
	profileData := map[string]string{
		"handle":       handle,
		"agent_secret": agentSecret,
		"base_url":     baseURL,
	}
	if runtime != "" {
		profileData["runtime"] = runtime
	}
	data, err := json.MarshalIndent(profileData, "", "  ")
	if err != nil {
		return botletsRepairAgentProfileWrite{}, err
	}
	if _, err := ValidateAgentProfileWriteTarget(paths, handle); err != nil {
		return botletsRepairAgentProfileWrite{}, err
	}
	return botletsRepairAgentProfileWrite{
		path: profilePath,
		data: append(data, '\n'),
		profile: AgentProfile{
			Handle:      handle,
			AgentSecret: agentSecret,
			BaseURL:     strings.TrimSuffix(baseURL, "/"),
			Runtime:     runtime,
			Path:        profilePath,
		},
	}, nil
}

func resolveBotletsRegistryCredentialPathForRepair(botletsHome string, value string) (string, bool, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false, nil
	}
	if strings.ContainsAny(value, "\r\n\x00") || strings.Contains(value, "://") {
		return "", false, nil
	}
	resolved := value
	if strings.HasPrefix(value, "~/") || value == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false, err
		}
		if value == "~" {
			resolved = home
		} else {
			resolved = filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
	} else if !filepath.IsAbs(value) {
		cleaned := filepath.Clean(value)
		if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
			return "", false, nil
		}
		resolved = filepath.Join(botletsHome, cleaned)
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", false, err
	}
	return filepath.Clean(absolute), true, nil
}

func loadBotletsAgentProfileByPathForRepair(paths Paths, credentialPath string) (AgentProfile, bool, error) {
	profiles, _, errorsOut := LoadAgentProfilesWithAliases(context.Background(), paths, "")
	if HasFatalProfileReloadError(errorsOut) {
		return AgentProfile{}, false, fmt.Errorf("Botlets credential profile repair failed: %+v", errorsOut)
	}
	for _, profile := range profiles {
		if profile.Path == credentialPath {
			return profile, true, nil
		}
	}
	return AgentProfile{}, false, nil
}

func shouldWriteBotletsCanonicalProfileRepairTarget(path string, targetHandle string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return false, fmt.Errorf("Botlets canonical profile %q cannot overwrite an unsafe profile path", targetHandle)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var raw struct {
		ProfileKind        string `json:"profile_kind"`
		Handle             string `json:"handle"`
		AgentSecret        string `json:"agent_secret"`
		AliasOf            string `json:"alias_of"`
		BotID              string `json:"bot_id"`
		BotAgentID         string `json:"bot_agent_id"`
		DisabledForPolling bool   `json:"disabled_for_polling"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, fmt.Errorf("Botlets canonical profile %q cannot overwrite an invalid profile file", targetHandle)
	}
	if raw.ProfileKind == "alias" {
		return false, fmt.Errorf("Botlets canonical profile %q cannot overwrite an alias profile", targetHandle)
	}
	if raw.ProfileKind != "" || (raw.Handle != "" && !strings.EqualFold(raw.Handle, targetHandle)) || raw.AgentSecret == "" {
		return false, fmt.Errorf("Botlets canonical profile %q already belongs to another credential", targetHandle)
	}
	return false, nil
}

func prepareBotletsAgentProfileAliasesForRepair(paths Paths, botletsHome string, canonicalHandle string, aliases []string, botID string, botAgentID string) ([]botletsRepairAgentProfileAliasWrite, error) {
	writes := make([]botletsRepairAgentProfileAliasWrite, 0, len(aliases))
	for _, alias := range aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" || strings.EqualFold(alias, canonicalHandle) {
			continue
		}
		if !ProfileRE.MatchString(alias) {
			return nil, errors.New("invalid Botlets alias handle")
		}
		profilePath, err := ValidateAgentProfileWriteTarget(paths, alias)
		if err != nil {
			return nil, err
		}
		if err := validateBotletsAgentProfileAliasTargetForRepair(botletsHome, canonicalHandle, alias, profilePath, botID, botAgentID); err != nil {
			return nil, err
		}
		data, err := json.MarshalIndent(map[string]any{
			"profile_kind":         "alias",
			"alias_of":             canonicalHandle,
			"bot_id":               botID,
			"bot_agent_id":         botAgentID,
			"disabled_for_polling": true,
		}, "", "  ")
		if err != nil {
			return nil, err
		}
		writes = append(writes, botletsRepairAgentProfileAliasWrite{path: profilePath, data: append(data, '\n')})
	}
	return writes, nil
}

func validateBotletsAgentProfileAliasTargetForRepair(botletsHome string, canonicalHandle string, alias string, profilePath string, botID string, botAgentID string) error {
	info, err := os.Lstat(profilePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("Botlets alias profile %q cannot overwrite an unsafe profile path", alias)
	}
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return err
	}
	var raw struct {
		ProfileKind        string `json:"profile_kind"`
		Handle             string `json:"handle"`
		AgentSecret        string `json:"agent_secret"`
		AliasOf            string `json:"alias_of"`
		BotID              string `json:"bot_id"`
		BotAgentID         string `json:"bot_agent_id"`
		DisabledForPolling bool   `json:"disabled_for_polling"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("Botlets alias profile %q cannot overwrite an invalid profile file", alias)
	}
	if raw.ProfileKind == "alias" {
		if !raw.DisabledForPolling {
			return fmt.Errorf("Botlets alias profile %q is not safe to replace", alias)
		}
		if strings.EqualFold(raw.AliasOf, canonicalHandle) || botletsProfileAliasIdentityMatchesForRepair(raw.BotID, raw.BotAgentID, botID, botAgentID) {
			return nil
		}
		return fmt.Errorf("Botlets alias profile %q already belongs to another bot", alias)
	}
	if raw.ProfileKind != "" || (raw.Handle != "" && !strings.EqualFold(raw.Handle, alias)) || raw.AgentSecret == "" {
		return fmt.Errorf("Botlets alias profile %q cannot overwrite an unrecognized profile file", alias)
	}
	ok, err := botletsRegistryHasProfileAliasForIdentityForRepair(botletsHome, alias, profilePath, botID, botAgentID)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}
	return fmt.Errorf("Botlets alias profile %q already belongs to another bot", alias)
}

func botletsProfileAliasIdentityMatchesForRepair(existingBotID string, existingBotAgentID string, botID string, botAgentID string) bool {
	if botID != "" && existingBotID != "" && existingBotID == botID {
		return true
	}
	return botAgentID != "" && existingBotAgentID != "" && existingBotAgentID == botAgentID
}

func botletsRegistryHasProfileAliasForIdentityForRepair(botletsHome string, alias string, profilePath string, botID string, botAgentID string) (bool, error) {
	if strings.TrimSpace(botletsHome) == "" {
		return false, nil
	}
	botletsHome, err := ValidateBotletsRegistryWriteTarget(botletsHome)
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(botletsHome, "registry.json"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var registry struct {
		Bots []BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		return false, err
	}
	cleanProfilePath, err := filepath.Abs(profilePath)
	if err != nil {
		return false, err
	}
	cleanProfilePath = filepath.Clean(cleanProfilePath)
	for _, entry := range registry.Bots {
		if !botletsRegistryEntryHasIdentityForRepair(entry, botID, botAgentID) || !entry.MatchesProfile(alias) {
			continue
		}
		credentialPath := entry.CredentialProfile
		if credentialPath != "" && !filepath.IsAbs(credentialPath) {
			credentialPath = filepath.Join(botletsHome, credentialPath)
		}
		if credentialPath == "" {
			return true, nil
		}
		credentialPath, err = filepath.Abs(credentialPath)
		if err != nil {
			return false, err
		}
		if filepath.Clean(credentialPath) == cleanProfilePath || strings.EqualFold(entry.Handle, alias) {
			return true, nil
		}
	}
	return false, nil
}

func writePreparedBotletsAgentProfileAliasesForRepair(writes []botletsRepairAgentProfileAliasWrite) error {
	for _, write := range writes {
		if err := WritePrivateFileAtomicExistingDir(write.path, write.data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

type botletsRepairAgentProfileFileWrite struct {
	path string
	data []byte
}

type botletsRepairAgentProfileFileBackup struct {
	exists bool
	data   []byte
	mode   os.FileMode
}

func writePreparedBotletsAgentProfileSetRollbackableForRepair(primary botletsRepairAgentProfileWrite, aliases []botletsRepairAgentProfileAliasWrite) error {
	writes := []botletsRepairAgentProfileFileWrite{{path: primary.path, data: primary.data}}
	for _, alias := range aliases {
		writes = append(writes, botletsRepairAgentProfileFileWrite{path: alias.path, data: alias.data})
	}
	return writeBotletsAgentProfileFilesRollbackableForRepair(writes)
}

func writeBotletsAgentProfileFilesRollbackableForRepair(writes []botletsRepairAgentProfileFileWrite) error {
	backups := make(map[string]botletsRepairAgentProfileFileBackup, len(writes))
	written := make([]string, 0, len(writes))
	for _, write := range writes {
		if _, ok := backups[write.path]; !ok {
			info, statErr := os.Stat(write.path)
			if statErr == nil {
				data, readErr := os.ReadFile(write.path)
				if readErr != nil {
					return readErr
				}
				backups[write.path] = botletsRepairAgentProfileFileBackup{exists: true, data: data, mode: info.Mode().Perm()}
			} else if errors.Is(statErr, os.ErrNotExist) {
				backups[write.path] = botletsRepairAgentProfileFileBackup{}
			} else {
				return statErr
			}
		}
		if err := WritePrivateFileAtomicExistingDir(write.path, write.data, 0o600); err != nil {
			if rollbackErr := restoreBotletsAgentProfileFilesForRepair(backups, written); rollbackErr != nil {
				return fmt.Errorf("%w (profile rollback failed: %v)", err, rollbackErr)
			}
			return err
		}
		written = append(written, write.path)
	}
	return nil
}

func restoreBotletsAgentProfileFilesForRepair(backups map[string]botletsRepairAgentProfileFileBackup, paths []string) error {
	var rollbackErr error
	for i := len(paths) - 1; i >= 0; i-- {
		path := paths[i]
		backup := backups[path]
		if backup.exists {
			mode := backup.mode
			if mode == 0 {
				mode = 0o600
			}
			if err := WritePrivateFileAtomicExistingDir(path, backup.data, mode); err != nil && rollbackErr == nil {
				rollbackErr = err
			}
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && rollbackErr == nil {
			rollbackErr = err
		}
	}
	return rollbackErr
}

func restoreBotletsRegistryFileForRepair(path string, previous []byte, exists bool) error {
	if exists {
		return WritePrivateFileAtomic(path, previous, 0o600)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func mergeBotletsRegistryEntryForRepair(existing BotRegistryEntry, next BotRegistryEntry) BotRegistryEntry {
	if next.BotID == "" {
		next.BotID = existing.BotID
	}
	if next.DisplayName == "" {
		next.DisplayName = existing.DisplayName
	}
	preferredSlugAliases := append([]string{}, next.SlugAliases...)
	preferredSlugAliases = append(preferredSlugAliases, existing.Name)
	preferredHandleAliases := append([]string{}, next.HandleAliases...)
	preferredHandleAliases = append(preferredHandleAliases, existing.Handle)
	next.SlugAliases = mergeBotletsRegistryAliasesForRepair(next.Name, preferredSlugAliases, existing.SlugAliases)
	next.HandleAliases = mergeBotletsRegistryAliasesForRepair(next.Handle, preferredHandleAliases, existing.HandleAliases)
	return next
}

func mergeBotletsRegistryAliasesForRepair(current string, preferred []string, fallback []string) []string {
	out := make([]string, 0, len(preferred)+len(fallback))
	seen := map[string]struct{}{}
	if current = strings.TrimSpace(current); current != "" {
		seen[strings.ToLower(current)] = struct{}{}
	}
	for _, labels := range [][]string{preferred, fallback} {
		for _, label := range labels {
			label = strings.TrimSpace(label)
			if label == "" {
				continue
			}
			key := strings.ToLower(label)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, label)
		}
	}
	return out
}

func botletsRegistryEntriesOverlapForRepair(existing BotRegistryEntry, next BotRegistryEntry) (bool, error) {
	if next.BotID != "" && existing.BotID == next.BotID {
		existingBotAgentID := botletsRegistryEntryBotAgentIDForRepair(existing)
		nextBotAgentID := botletsRegistryEntryBotAgentIDForRepair(next)
		if existingBotAgentID != "" && nextBotAgentID != "" && existingBotAgentID != nextBotAgentID {
			return false, fmt.Errorf("Botlets registry bot id %q already belongs to another bot agent", next.BotID)
		}
		return true, nil
	}
	if existing.BrainRef != nil && next.BrainRef != nil && existing.BrainRef.BotAgentID != "" && existing.BrainRef.BotAgentID == next.BrainRef.BotAgentID {
		if existing.BotID != "" && next.BotID != "" && existing.BotID != next.BotID {
			return false, fmt.Errorf("Botlets registry bot agent %q already belongs to another bot", existing.BrainRef.BotAgentID)
		}
		return true, nil
	}
	existingLabels := map[string]bool{}
	for _, label := range []string{existing.Name, existing.Handle} {
		if strings.TrimSpace(label) == "" {
			continue
		}
		existingLabels[strings.ToLower(label)] = true
	}
	for _, label := range append(existing.SlugAliases, existing.HandleAliases...) {
		if strings.TrimSpace(label) == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := existingLabels[key]; !ok {
			existingLabels[key] = false
		}
	}
	nextLabels := map[string]bool{}
	for _, label := range []string{next.Name, next.Handle} {
		if strings.TrimSpace(label) == "" {
			continue
		}
		nextLabels[strings.ToLower(label)] = true
	}
	for _, label := range append(next.SlugAliases, next.HandleAliases...) {
		if strings.TrimSpace(label) == "" {
			continue
		}
		key := strings.ToLower(label)
		if _, ok := nextLabels[key]; !ok {
			nextLabels[key] = false
		}
	}
	for key, nextCanonical := range nextLabels {
		if existingCanonical, ok := existingLabels[key]; ok {
			existingIdentity := firstNonEmptyForRepair(existing.BotID, botletsRegistryEntryBotAgentIDForRepair(existing))
			nextIdentity := firstNonEmptyForRepair(next.BotID, botletsRegistryEntryBotAgentIDForRepair(next))
			if existingIdentity != "" && nextIdentity != "" && existingIdentity != nextIdentity {
				return false, fmt.Errorf("Botlets registry label %q already belongs to another bot", key)
			}
			if existingIdentity != "" && nextIdentity != "" {
				return true, nil
			}
			if !existingCanonical || !nextCanonical {
				return false, fmt.Errorf("Botlets registry label %q already belongs to another bot", key)
			}
			return true, nil
		}
	}
	return false, nil
}

func botletsRegistryEntryBotAgentIDForRepair(entry BotRegistryEntry) string {
	if entry.BrainRef == nil {
		return ""
	}
	return entry.BrainRef.BotAgentID
}

func botletsRegistryEntryHasIdentityForRepair(entry BotRegistryEntry, botID string, botAgentID string) bool {
	if botID != "" && entry.BotID != "" && entry.BotID == botID {
		return true
	}
	entryBotAgentID := botletsRegistryEntryBotAgentIDForRepair(entry)
	return botAgentID != "" && entryBotAgentID != "" && entryBotAgentID == botAgentID
}

func firstNonEmptyForRepair(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
