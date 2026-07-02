package commentbus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const defaultCommentBaseURL = "https://comment.io"

var errBotletsBrainProjectionPathMissing = errors.New("brain projection path must exist")

type AgentProfile struct {
	Handle      string
	AgentSecret string
	BaseURL     string
	Runtime     string
	Model       string
	Path        string
}

type ProfileAlias struct {
	Alias              string
	AliasOf            string
	BotID              string
	BotAgentID         string
	DisabledForPolling bool
	Path               string
}

type BotRegistryEntry struct {
	Name              string                `json:"name"`
	DisplayName       string                `json:"display_name,omitempty"`
	BotID             string                `json:"bot_id,omitempty"`
	Handle            string                `json:"handle"`
	SlugAliases       []string              `json:"slug_aliases,omitempty"`
	HandleAliases     []string              `json:"handle_aliases,omitempty"`
	CredentialProfile string                `json:"credential_profile"`
	CredentialPath    string                `json:"-"`
	RegistryRuntime   string                `json:"-"`
	BrainRef          *BotBrainRef          `json:"brain_ref,omitempty"`
	ManagedSession    ManagedSessionSetting `json:"managed_session"`
	// RespondsToMentions mirrors the bot's "Responds to @mentions" opt-in from
	// the server (the owned-agents manifest / enrollment hint). When true, the
	// daemon auto-launches the bot's runtime on an incoming doc @mention if no
	// session is already running — the same launch the web "Start your agent"
	// button triggers. omitempty so existing registry files stay byte-stable
	// until the flag is set.
	RespondsToMentions bool `json:"responds_to_mentions,omitempty"`
}

type BotBrainRef struct {
	WorkspaceID     string `json:"workspace_id"`
	OwnerAgentID    string `json:"owner_agent_id,omitempty"`
	BotAgentID      string `json:"bot_agent_id,omitempty"`
	ContainerID     string `json:"container_id"`
	RootFolderID    string `json:"root_folder_id"`
	RelativePath    string `json:"relative_path"`
	SetupGeneration int    `json:"setup_generation,omitempty"`
}

type BotletsRepairHint struct {
	Code             string `json:"code"`
	CanContinue      bool   `json:"can_continue"`
	CanonicalProfile string `json:"canonical_profile,omitempty"`
	CanonicalBotName string `json:"canonical_bot_name,omitempty"`
	SuggestedPath    string `json:"suggested_path,omitempty"`
}

const (
	BotletsRepairHintPathLabelMismatch     = "PATH_LABEL_MISMATCH"
	BotletsRepairHintCanonicalProfileMoved = "CANONICAL_PROFILE_RENAMED"
	BotletsRepairHintSyncPathMovePending   = "SYNC_PATH_MOVE_PENDING"
)

type ManagedSessionSetting struct {
	Enabled  bool   `json:"enabled"`
	Runtime  string `json:"runtime"`
	Model    string `json:"model,omitempty"`
	Host     string `json:"host,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

type ProfileState struct {
	AgentProfiles  map[string]AgentProfile
	ProfileAliases map[string]ProfileAlias
	BotRegistry    map[string]BotRegistryEntry
	BotletsHome    string
}

type ProfileReloadError struct {
	Code    string              `json:"code"`
	Message string              `json:"message"`
	Profile string              `json:"profile,omitempty"`
	Bot     string              `json:"bot,omitempty"`
	Hints   []BotletsRepairHint `json:"hints,omitempty"`
}

type ProfileReloadResult struct {
	Added          []string             `json:"added"`
	Removed        []string             `json:"removed"`
	Restarted      []string             `json:"restarted"`
	Errors         []ProfileReloadError `json:"errors"`
	ProfilesLoaded int                  `json:"profiles_loaded"`
	BotsLoaded     int                  `json:"bots_loaded"`
}

type ProfileLoadOptions struct {
	Paths          Paths
	BotletsHome    string
	DefaultBaseURL string
}

func LoadProfileState(ctx context.Context, options ProfileLoadOptions) (ProfileState, []ProfileReloadError) {
	options.DefaultBaseURL = resolveDefaultBaseURL(options.DefaultBaseURL)
	if options.BotletsHome == "" {
		options.BotletsHome = defaultBotletsHome()
	}
	botletsHome, err := ResolveBotletsHome(options.BotletsHome)
	if err != nil {
		return EmptyProfileState(""), []ProfileReloadError{{Code: "INVALID_BOTLETS_HOME", Message: "invalid botlets home"}}
	}
	parent := normalizeTrustedBotletsParentPath(filepath.Dir(botletsHome))
	if err := validateTrustedSearchPathDir(parent, "botlets home parent"); err != nil {
		return EmptyProfileState(botletsHome), []ProfileReloadError{{Code: "INVALID_BOTLETS_HOME", Message: err.Error()}}
	}
	if err := validateBotletsHomeTrust(botletsHome); err != nil {
		return EmptyProfileState(botletsHome), []ProfileReloadError{{Code: "INVALID_BOTLETS_HOME", Message: err.Error()}}
	}

	profiles, aliases, profileErrors := LoadAgentProfilesWithAliases(ctx, options.Paths, options.DefaultBaseURL)
	bots, registryErrors := LoadBotletsRegistry(ctx, botletsHome, profiles)
	errorsOut := append(profileErrors, registryErrors...)
	return ProfileState{
		AgentProfiles:  profiles,
		ProfileAliases: aliases,
		BotRegistry:    bots,
		BotletsHome:    botletsHome,
	}, errorsOut
}

func EmptyProfileState(botletsHome string) ProfileState {
	return ProfileState{
		AgentProfiles:  map[string]AgentProfile{},
		ProfileAliases: map[string]ProfileAlias{},
		BotRegistry:    map[string]BotRegistryEntry{},
		BotletsHome:    botletsHome,
	}
}

func LoadAgentProfiles(_ context.Context, paths Paths, defaultBaseURL string) (map[string]AgentProfile, []ProfileReloadError) {
	profiles, _, errorsOut := LoadAgentProfilesWithAliases(context.Background(), paths, defaultBaseURL)
	return profiles, errorsOut
}

func LoadAgentProfilesWithAliases(_ context.Context, paths Paths, defaultBaseURL string) (map[string]AgentProfile, map[string]ProfileAlias, []ProfileReloadError) {
	defaultBaseURL = resolveDefaultBaseURL(defaultBaseURL)
	profiles := map[string]AgentProfile{}
	aliases := map[string]ProfileAlias{}
	var errorsOut []ProfileReloadError
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := validateAgentDirectoryTrust(agentsDir); errors.Is(err, os.ErrNotExist) {
		return profiles, aliases, errorsOut
	} else if err != nil {
		return profiles, aliases, []ProfileReloadError{{Code: "UNTRUSTED_AGENTS_DIR", Message: err.Error()}}
	}
	entries, err := os.ReadDir(agentsDir)
	if errors.Is(err, os.ErrNotExist) {
		return profiles, aliases, errorsOut
	}
	if err != nil {
		return profiles, aliases, []ProfileReloadError{{Code: "READ_AGENTS_DIR_FAILED", Message: "could not read agent profiles"}}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	seen := map[string]string{}
	seenAliases := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		handle := strings.TrimSuffix(name, ".json")
		profile, alias, err := readAgentProfileOrAliasFile(filepath.Join(agentsDir, name), handle, defaultBaseURL)
		if err != nil {
			errorsOut = append(errorsOut, agentProfileLoadError("INVALID_AGENT_PROFILE", err.Error(), handle))
			continue
		}
		if alias != nil {
			key := strings.ToLower(alias.Alias)
			if previous := seen[key]; previous != "" || seenAliases[key] != "" {
				errorsOut = append(errorsOut, agentProfileLoadError("DUPLICATE_AGENT_PROFILE", "duplicate agent profile", alias.Alias))
				continue
			}
			seenAliases[key] = alias.Path
			aliases[alias.Alias] = *alias
			continue
		}
		key := strings.ToLower(profile.Handle)
		if previous := seen[key]; previous != "" || seenAliases[key] != "" {
			errorsOut = append(errorsOut, agentProfileLoadError("DUPLICATE_AGENT_PROFILE", "duplicate agent profile", profile.Handle))
			continue
		}
		seen[key] = profile.Path
		profiles[profile.Handle] = profile
	}
	for aliasHandle, alias := range aliases {
		if _, ok := profiles[alias.AliasOf]; !ok {
			errorsOut = append(errorsOut, agentProfileLoadError("INVALID_AGENT_ALIAS", "profile alias target not loaded", aliasHandle))
			delete(aliases, aliasHandle)
		}
	}
	return profiles, aliases, errorsOut
}

func readAgentProfileFile(path string, handle string, defaultBaseURL string) (AgentProfile, error) {
	profile, alias, err := readAgentProfileOrAliasFile(path, handle, defaultBaseURL)
	if err != nil {
		return AgentProfile{}, err
	}
	if alias != nil {
		return AgentProfile{}, errors.New("profile is an alias")
	}
	return profile, nil
}

func readAgentProfileOrAliasFile(path string, handle string, defaultBaseURL string) (AgentProfile, *ProfileAlias, error) {
	if !ProfileRE.MatchString(handle) {
		return AgentProfile{}, nil, errors.New("invalid profile handle")
	}
	if err := validateAgentProfileFileTrust(path); err != nil {
		return AgentProfile{}, nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return AgentProfile{}, nil, errors.New("could not read profile")
	}
	var raw struct {
		ProfileKind        string `json:"profile_kind"`
		Handle             string `json:"handle"`
		AgentSecret        string `json:"agent_secret"`
		BaseURL            string `json:"base_url"`
		Runtime            string `json:"runtime"`
		Model              string `json:"model"`
		AliasOf            string `json:"alias_of"`
		BotID              string `json:"bot_id"`
		BotAgentID         string `json:"bot_agent_id"`
		DisabledForPolling bool   `json:"disabled_for_polling"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return AgentProfile{}, nil, errors.New("invalid profile json")
	}
	cleanPath, err := filepath.Abs(path)
	if err != nil {
		return AgentProfile{}, nil, errors.New("invalid profile path")
	}
	cleanPath = filepath.Clean(cleanPath)
	if raw.ProfileKind == "alias" {
		if !ProfileRE.MatchString(raw.AliasOf) || raw.AliasOf == handle {
			return AgentProfile{}, nil, errors.New("invalid profile alias target")
		}
		if !raw.DisabledForPolling {
			return AgentProfile{}, nil, errors.New("profile alias must be disabled for polling")
		}
		if raw.BotID != "" && !isSafeRegistryIdentity(raw.BotID) {
			return AgentProfile{}, nil, errors.New("invalid profile alias bot id")
		}
		if raw.BotAgentID != "" && !isSafeRegistryIdentity(raw.BotAgentID) {
			return AgentProfile{}, nil, errors.New("invalid profile alias bot agent id")
		}
		return AgentProfile{}, &ProfileAlias{
			Alias:              handle,
			AliasOf:            raw.AliasOf,
			BotID:              raw.BotID,
			BotAgentID:         raw.BotAgentID,
			DisabledForPolling: true,
			Path:               cleanPath,
		}, nil
	}
	if raw.Handle != "" && raw.Handle != handle {
		return AgentProfile{}, nil, errors.New("profile handle mismatch")
	}
	if !strings.HasPrefix(raw.AgentSecret, "as_") {
		return AgentProfile{}, nil, errors.New("missing agent secret")
	}
	baseURL := defaultBaseURL
	if raw.BaseURL != "" {
		baseURL = normalizeBaseURL(raw.BaseURL)
	}
	if baseURL == "" {
		baseURL = CurrentEnvironment().DefaultBaseURL()
	}
	profileRuntime := strings.TrimSpace(raw.Runtime)
	if profileRuntime != "" && profileRuntime != "claude" && profileRuntime != "codex" {
		return AgentProfile{}, nil, errors.New("invalid profile runtime")
	}
	profileModel, ok := normalizeAgentModel(raw.Model)
	if !ok {
		return AgentProfile{}, nil, errors.New("invalid profile model")
	}
	return AgentProfile{
		Handle:      handle,
		AgentSecret: raw.AgentSecret,
		BaseURL:     baseURL,
		Runtime:     profileRuntime,
		Model:       profileModel,
		Path:        cleanPath,
	}, nil, nil
}

func LoadBotletsRegistry(_ context.Context, botletsHome string, profiles map[string]AgentProfile) (map[string]BotRegistryEntry, []ProfileReloadError) {
	bots := map[string]BotRegistryEntry{}
	registryPath := filepath.Join(botletsHome, "registry.json")
	if err := validateRegistryFileTrust(registryPath); errors.Is(err, os.ErrNotExist) {
		return bots, nil
	} else if err != nil {
		return bots, []ProfileReloadError{{Code: "UNTRUSTED_BOTLETS_REGISTRY", Message: err.Error()}}
	}
	data, err := os.ReadFile(registryPath)
	if errors.Is(err, os.ErrNotExist) {
		return bots, nil
	}
	if err != nil {
		return bots, []ProfileReloadError{{Code: "READ_BOTLETS_REGISTRY_FAILED", Message: "could not read botlets registry"}}
	}
	var registry struct {
		Bots *[]BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		return bots, []ProfileReloadError{{Code: "INVALID_BOTLETS_REGISTRY", Message: "invalid botlets registry json"}}
	}
	if registry.Bots == nil {
		return bots, []ProfileReloadError{{Code: "INVALID_BOTLETS_REGISTRY", Message: "botlets registry bots must be an array"}}
	}
	return ValidateBotletsRegistryEntries(botletsHome, profiles, *registry.Bots)
}

func ValidateBotletsRegistryEntries(botletsHome string, profiles map[string]AgentProfile, registryBots []BotRegistryEntry) (map[string]BotRegistryEntry, []ProfileReloadError) {
	bots := map[string]BotRegistryEntry{}
	profilesByPath := profilesByCredentialPath(profiles)
	seenNames := map[string]string{}
	seenHandles := map[string]string{}
	seenBotIDs := map[string]struct{}{}
	seenBotAgentIDs := map[string]struct{}{}
	var errorsOut []ProfileReloadError
	for _, bot := range registryBots {
		if err := validateRegistryBotShape(bot); err != nil {
			errorsOut = append(errorsOut, registryProfileLoadError("INVALID_BOT", err.Error(), bot.Name, bot.Handle))
			continue
		}
		identityKey := registryBotIdentityKey(bot)
		if bot.BotID != "" {
			botIDKey := strings.ToLower(bot.BotID)
			if _, ok := seenBotIDs[botIDKey]; ok {
				errorsOut = append(errorsOut, registryProfileLoadError("DUPLICATE_BOT_ID", "duplicate bot id", bot.Name, bot.Handle))
				continue
			}
			seenBotIDs[botIDKey] = struct{}{}
		}
		if bot.BrainRef != nil && bot.BrainRef.BotAgentID != "" {
			botAgentIDKey := strings.ToLower(bot.BrainRef.BotAgentID)
			if _, ok := seenBotAgentIDs[botAgentIDKey]; ok {
				errorsOut = append(errorsOut, registryProfileLoadError("DUPLICATE_BOT_AGENT_ID", "duplicate bot agent id", bot.Name, bot.Handle))
				continue
			}
			seenBotAgentIDs[botAgentIDKey] = struct{}{}
		}
		if duplicateRegistryLabel(seenNames, botRegistryNameLabels(bot), identityKey) {
			errorsOut = append(errorsOut, registryProfileLoadError("DUPLICATE_BOT_NAME", "duplicate bot name", bot.Name, bot.Handle))
			continue
		}
		if duplicateRegistryLabel(seenHandles, botRegistryHandleLabels(bot), identityKey) {
			errorsOut = append(errorsOut, registryProfileLoadError("DUPLICATE_BOT_HANDLE", "duplicate bot handle", bot.Name, bot.Handle))
			continue
		}
		credentialPath, err := resolveRegistryPath(botletsHome, bot.CredentialProfile)
		if err != nil {
			errorsOut = append(errorsOut, registryProfileLoadError("INVALID_CREDENTIAL_PROFILE", "invalid credential profile", bot.Name, bot.Handle))
			continue
		}
		profile, ok := profilesByPath[credentialPath]
		if !ok {
			errorsOut = append(errorsOut, registryProfileLoadError("MISSING_CREDENTIAL_PROFILE", "credential profile not loaded", bot.Name, bot.Handle))
			continue
		}
		if profile.Handle != bot.Handle {
			errorsOut = append(errorsOut, registryProfileLoadError("HANDLE_MISMATCH", "credential profile handle mismatch", bot.Name, bot.Handle, credentialProfileHandleMismatchHints(bot, profile)...))
			continue
		}
		if bot.ManagedSession.Enabled {
			bot.RegistryRuntime = bot.ManagedSession.Runtime
			bot.ManagedSession.Runtime = managedSessionRuntimeForProfile(profile, bot.ManagedSession.Runtime)
			bot.ManagedSession.Model = managedSessionModelForProfile(profile, bot.ManagedSession.Model)
			bot.ManagedSession.Host = normalizeNewManagedSessionHost(bot.ManagedSession.Host)
		}
		storeRegistryLabels(seenNames, botRegistryNameLabels(bot), identityKey)
		storeRegistryLabels(seenHandles, botRegistryHandleLabels(bot), identityKey)
		bot.CredentialPath = credentialPath
		bots[bot.Name] = bot
	}
	return bots, errorsOut
}

func managedSessionRuntimeForProfile(profile AgentProfile, fallback string) string {
	if profile.Runtime != "" {
		return profile.Runtime
	}
	if fallback == "claude" || fallback == "codex" {
		return fallback
	}
	return "claude"
}

func managedSessionModelForProfile(profile AgentProfile, fallback string) string {
	if profile.Model != "" {
		return profile.Model
	}
	model, ok := normalizeAgentModel(fallback)
	if !ok {
		return ""
	}
	return model
}

func profilesByCredentialPath(profiles map[string]AgentProfile) map[string]AgentProfile {
	out := map[string]AgentProfile{}
	for _, profile := range profiles {
		out[profile.Path] = profile
	}
	return out
}

// HasFatalProfileReloadError reports whether the errors include any failure
// that should prevent applying a newly loaded ProfileState. Per-agent-profile
// entry errors (a single bad profile file) are not fatal — they are reported
// but should let the other valid profiles load. Directory-level errors and
// bot-registry errors remain fatal (preserve previous state) to avoid
// dropping working bots on a transient registry edit.
func HasFatalProfileReloadError(errs []ProfileReloadError) bool {
	for _, e := range errs {
		if !isAgentProfileEntryError(e.Code) {
			return true
		}
	}
	return false
}

func isAgentProfileEntryError(code string) bool {
	switch code {
	case "INVALID_AGENT_PROFILE", "INVALID_AGENT_ALIAS", "DUPLICATE_AGENT_PROFILE":
		return true
	}
	return false
}

func agentProfileLoadError(code string, message string, profile string) ProfileReloadError {
	out := ProfileReloadError{Code: code, Message: message}
	if ProfileRE.MatchString(profile) {
		out.Profile = profile
	}
	return out
}

func registryProfileLoadError(code string, message string, bot string, profile string, hints ...BotletsRepairHint) ProfileReloadError {
	out := ProfileReloadError{Code: code, Message: message}
	if isBotName(bot) {
		out.Bot = bot
	}
	if ProfileRE.MatchString(profile) {
		out.Profile = profile
	}
	if len(hints) > 0 {
		out.Hints = append([]BotletsRepairHint{}, hints...)
	}
	return out
}

func credentialProfileHandleMismatchHints(bot BotRegistryEntry, profile AgentProfile) []BotletsRepairHint {
	if bot.Handle == "" || profile.Handle == "" || strings.EqualFold(bot.Handle, profile.Handle) {
		return nil
	}
	if !bot.MatchesProfile(profile.Handle) {
		return nil
	}
	return []BotletsRepairHint{{
		Code:             BotletsRepairHintCanonicalProfileMoved,
		CanContinue:      false,
		CanonicalProfile: bot.Handle,
		CanonicalBotName: bot.Name,
	}}
}

func validateRegistryBotShape(bot BotRegistryEntry) error {
	if !isBotName(bot.Name) {
		return errors.New("invalid bot name")
	}
	if bot.BotID != "" && !isSafeRegistryIdentity(bot.BotID) {
		return errors.New("invalid bot id")
	}
	if strings.TrimSpace(bot.DisplayName) != "" {
		if !validBotDisplayNameLength(bot.DisplayName) || strings.ContainsAny(bot.DisplayName, "\r\n\x00") || containsSecretValue(bot.DisplayName) {
			return errors.New("invalid bot display name")
		}
	}
	if !ProfileRE.MatchString(bot.Handle) {
		return errors.New("invalid bot handle")
	}
	if err := validateBotSlugAliases(bot.Name, bot.SlugAliases); err != nil {
		return err
	}
	if err := validateBotHandleAliases(bot.Handle, bot.HandleAliases); err != nil {
		return err
	}
	if bot.BrainRef != nil {
		if err := validateBotBrainRef(*bot.BrainRef, bot); err != nil {
			return err
		}
	}
	if bot.ManagedSession.Enabled && bot.ManagedSession.Runtime != "" && bot.ManagedSession.Runtime != "claude" && bot.ManagedSession.Runtime != "codex" {
		return errors.New("invalid managed session runtime")
	}
	if bot.ManagedSession.Enabled {
		if _, ok := normalizeAgentModel(bot.ManagedSession.Model); !ok {
			return errors.New("invalid managed session model")
		}
	}
	if bot.ManagedSession.Enabled && !isSessionHost(bot.ManagedSession.Host) {
		return errors.New("invalid managed session host")
	}
	if bot.ManagedSession.Enabled && strings.TrimSpace(bot.ManagedSession.Timezone) != "" {
		if strings.ContainsAny(bot.ManagedSession.Timezone, "\r\n\x00") || containsSecretValue(bot.ManagedSession.Timezone) {
			return errors.New("invalid managed session timezone")
		}
		if _, err := time.LoadLocation(bot.ManagedSession.Timezone); err != nil {
			return errors.New("invalid managed session timezone")
		}
	}
	return nil
}

func validateBotSlugAliases(current string, aliases []string) error {
	seen := map[string]struct{}{strings.ToLower(current): {}}
	for _, alias := range aliases {
		if !isBotName(alias) {
			return errors.New("invalid bot slug alias")
		}
		key := strings.ToLower(alias)
		if _, ok := seen[key]; ok {
			return errors.New("duplicate bot slug alias")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func validateBotHandleAliases(current string, aliases []string) error {
	seen := map[string]struct{}{strings.ToLower(current): {}}
	for _, alias := range aliases {
		if !ProfileRE.MatchString(alias) {
			return errors.New("invalid bot handle alias")
		}
		key := strings.ToLower(alias)
		if _, ok := seen[key]; ok {
			return errors.New("duplicate bot handle alias")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func isSafeRegistryIdentity(value string) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= 128 && !strings.ContainsAny(value, "\r\n\x00") && !containsSecretValue(value)
}

func registryBotIdentityKey(bot BotRegistryEntry) string {
	if bot.BotID != "" {
		return "bot:" + strings.ToLower(bot.BotID)
	}
	if bot.BrainRef != nil && bot.BrainRef.BotAgentID != "" {
		return "agent:" + strings.ToLower(bot.BrainRef.BotAgentID)
	}
	return "handle:" + strings.ToLower(bot.Handle)
}

func botAgentID(bot BotRegistryEntry) string {
	if bot.BrainRef == nil {
		return ""
	}
	return bot.BrainRef.BotAgentID
}

func (bot BotRegistryEntry) StableBotAgentID() string {
	return botAgentID(bot)
}

func (bot BotRegistryEntry) MatchesStableIdentity(botID string, botAgentID string) bool {
	if bot.BotID != "" && botID != "" && bot.BotID == botID {
		return true
	}
	stableBotAgentID := bot.StableBotAgentID()
	return stableBotAgentID != "" && botAgentID != "" && stableBotAgentID == botAgentID
}

func botRegistryNameLabels(bot BotRegistryEntry) []string {
	return append([]string{bot.Name}, bot.SlugAliases...)
}

func botRegistryHandleLabels(bot BotRegistryEntry) []string {
	return append([]string{bot.Handle}, bot.HandleAliases...)
}

func duplicateRegistryLabel(seen map[string]string, labels []string, identityKey string) bool {
	for _, label := range labels {
		key := strings.ToLower(label)
		if previous := seen[key]; previous != "" && previous != identityKey {
			return true
		}
	}
	return false
}

func storeRegistryLabels(seen map[string]string, labels []string, identityKey string) {
	for _, label := range labels {
		seen[strings.ToLower(label)] = identityKey
	}
}

func botRegistryBrainPathLabelsMatch(bot BotRegistryEntry, ownerLabel string, slugLabel string) bool {
	owners := map[string]struct{}{}
	for _, handle := range botRegistryHandleLabels(bot) {
		owner, _, ok := strings.Cut(handle, ".")
		if ok && owner != "" {
			owners[strings.ToLower(owner)] = struct{}{}
		}
	}
	slugs := map[string]struct{}{}
	for _, slug := range botRegistryNameLabels(bot) {
		slugs[strings.ToLower(slug)] = struct{}{}
	}
	_, ownerOK := owners[strings.ToLower(ownerLabel)]
	_, slugOK := slugs[strings.ToLower(slugLabel)]
	return ownerOK && slugOK
}

// MatchesSelector reports whether selector names this bot by current slug,
// current handle, an alias slug/handle, or the handle suffix used by
// `comment run <bot>`.
func (bot BotRegistryEntry) MatchesSelector(selector string) bool {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return false
	}
	key := strings.ToLower(selector)
	if bot.MatchesSlug(selector) {
		return true
	}
	for _, handle := range botRegistryHandleLabels(bot) {
		if strings.ToLower(handle) == key {
			return true
		}
		_, suffix, ok := strings.Cut(handle, ".")
		if ok && strings.ToLower(suffix) == key {
			return true
		}
	}
	return false
}

// MatchesDaemonSelector is for daemon-scoped ownership checks. It intentionally
// avoids the bare handle suffix convenience accepted by `comment run <bot>`
// because multiple bots can share the same credential profile.
func (bot BotRegistryEntry) MatchesDaemonSelector(selector string) bool {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return false
	}
	return bot.MatchesSlug(selector) || bot.MatchesProfile(selector)
}

func (bot BotRegistryEntry) MatchesSlug(slug string) bool {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return false
	}
	key := strings.ToLower(slug)
	for _, label := range botRegistryNameLabels(bot) {
		if strings.ToLower(label) == key {
			return true
		}
	}
	return false
}

func (bot BotRegistryEntry) MatchesProfile(profile string) bool {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return false
	}
	key := strings.ToLower(profile)
	for _, handle := range botRegistryHandleLabels(bot) {
		if strings.ToLower(handle) == key {
			return true
		}
	}
	return false
}

func validBotDisplayNameLength(displayName string) bool {
	return utf8.RuneCountInString(displayName) <= 100
}

// NormalizeBotDisplayNameForRegistry returns a registry-safe display name or
// an empty string when the input cannot be safely represented.
func NormalizeBotDisplayNameForRegistry(displayName string) string {
	normalized := strings.Join(strings.Fields(displayName), " ")
	if normalized == "" {
		return ""
	}
	if !validBotDisplayNameLength(normalized) || strings.ContainsAny(normalized, "\x00") || containsSecretValue(normalized) {
		return ""
	}
	return normalized
}

func validateBotBrainRef(ref BotBrainRef, bot BotRegistryEntry) error {
	if strings.TrimSpace(ref.WorkspaceID) == "" || strings.TrimSpace(ref.ContainerID) == "" || strings.TrimSpace(ref.RootFolderID) == "" {
		return errors.New("invalid brain reference")
	}
	for _, id := range []string{ref.OwnerAgentID, ref.BotAgentID} {
		if strings.TrimSpace(id) == "" || len(id) > 128 || strings.ContainsAny(id, "\r\n\x00") || containsSecretValue(id) {
			return errors.New("invalid brain reference")
		}
	}
	if ref.SetupGeneration < 1 {
		return errors.New("invalid brain reference")
	}
	rawRelative := strings.TrimSpace(ref.RelativePath)
	relative := filepath.Clean(rawRelative)
	if relative == "." || filepath.IsAbs(relative) || strings.HasPrefix(relative, "..") || strings.Contains(relative, "://") {
		return errors.New("invalid brain reference path")
	}
	if strings.Contains(relative, "\x00") || strings.ContainsAny(relative, "\r\n") {
		return errors.New("invalid brain reference path")
	}
	canonicalRelative := filepath.ToSlash(relative)
	if rawRelative != canonicalRelative {
		return errors.New("invalid brain reference path")
	}
	parts := strings.Split(canonicalRelative, "/")
	if len(parts) != 4 || parts[0] != "Botlets" || parts[1] == "" || parts[2] == "" || parts[3] != "brain" {
		return errors.New("invalid brain reference path")
	}
	if !botRegistryBrainPathLabelsMatch(bot, parts[1], parts[2]) {
		return errors.New("brain reference path does not match bot identity")
	}
	return nil
}

func ValidateBotletsBrainProjection(paths Paths, bot BotRegistryEntry) (string, error) {
	root, _, err := ValidateBotletsBrainProjectionWithRepairHints(paths, bot)
	return root, err
}

func ValidateBotletsBrainProjectionWithRepairHints(paths Paths, bot BotRegistryEntry) (string, []BotletsRepairHint, error) {
	if bot.BrainRef == nil {
		return "", nil, errors.New("missing brain reference")
	}
	if err := validateBotBrainRef(*bot.BrainRef, bot); err != nil {
		return "", nil, err
	}
	rawRoot, err := readLocalSyncRoot(paths)
	if err != nil {
		return "", nil, err
	}
	root := normalizeTrustedBotletsParentPath(rawRoot)
	if root == "" || !filepath.IsAbs(root) {
		return "", nil, errors.New("local sync root is not configured")
	}
	if err := validateTrustedSearchPathDir(root, "local sync root"); err != nil {
		return "", nil, err
	}
	brainRoot := filepath.Join(root, filepath.FromSlash(bot.BrainRef.RelativePath))
	rel, err := filepath.Rel(root, brainRoot)
	if err != nil {
		return "", nil, err
	}
	rel = filepath.Clean(rel)
	if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", nil, errors.New("brain projection is outside local sync root")
	}
	if filepath.ToSlash(rel) != bot.BrainRef.RelativePath {
		return "", nil, errors.New("brain projection path does not match brain reference")
	}
	placement, err := resolveBotletsBrainProjectionPlacement(paths, rawRoot, root, bot)
	if err != nil {
		return "", nil, err
	}
	if placement.Root != "" {
		brainRoot = placement.Root
	}
	if err := validateBotletsBrainProjectionPath(root, brainRoot); err != nil {
		return "", placement.Hints, err
	}
	return brainRoot, placement.Hints, nil
}

// BotletsBrainRootForProfile returns the validated local brain projection for
// the first registered Botlets bot owned by profile. Callers use this as a
// best-effort working directory hint; if local sync is missing or stale, the
// caller should keep its existing fallback so setup/recovery prompts can still
// run.
func BotletsBrainRootForProfile(paths Paths, state ProfileState, profile string) (string, bool) {
	if profile == "" {
		return "", false
	}
	names := make([]string, 0, len(state.BotRegistry))
	for name := range state.BotRegistry {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		bot := state.BotRegistry[name]
		if !bot.MatchesProfile(profile) || bot.BrainRef == nil {
			continue
		}
		brainRoot, err := ValidateBotletsBrainProjection(paths, bot)
		if err == nil && brainRoot != "" {
			return brainRoot, true
		}
	}
	return "", false
}

// ResolveBotletsBrainProjectionHint returns the expected brain projection
// root for startup orientation. It keeps the registry and placement checks, but
// allows the final on-disk brain directory to be missing so the runtime can
// explain the sync/setup problem to the bot instead of falling back to a generic
// Comment.io prompt.
func ResolveBotletsBrainProjectionHint(paths Paths, bot BotRegistryEntry) (string, error) {
	if bot.BrainRef == nil {
		return "", errors.New("missing brain reference")
	}
	if err := validateBotBrainRef(*bot.BrainRef, bot); err != nil {
		return "", err
	}
	rawRoot, err := readLocalSyncRoot(paths)
	if err != nil {
		return "", err
	}
	root := normalizeTrustedBotletsParentPath(rawRoot)
	if root == "" || !filepath.IsAbs(root) {
		return "", errors.New("local sync root is not configured")
	}
	if err := validateTrustedSearchPathDir(root, "local sync root"); err != nil {
		return "", err
	}
	brainRoot := filepath.Join(root, filepath.FromSlash(bot.BrainRef.RelativePath))
	rel, err := filepath.Rel(root, brainRoot)
	if err != nil {
		return "", err
	}
	rel = filepath.Clean(rel)
	if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return "", errors.New("brain projection is outside local sync root")
	}
	if filepath.ToSlash(rel) != bot.BrainRef.RelativePath {
		return "", errors.New("brain projection path does not match brain reference")
	}
	placement, err := resolveBotletsBrainProjectionPlacement(paths, rawRoot, root, bot)
	if err != nil {
		return "", err
	}
	if placement.Root != "" {
		brainRoot = placement.Root
	}
	if err := validateBotletsBrainProjectionPath(root, brainRoot); err != nil && !errors.Is(err, errBotletsBrainProjectionPathMissing) {
		return "", err
	}
	return brainRoot, nil
}

func readLocalSyncRoot(paths Paths) (string, error) {
	var cfg struct {
		Root string `json:"root"`
	}
	data, err := os.ReadFile(filepath.Join(paths.Home, "sync", "config.json"))
	if err != nil {
		return "", errors.New("local sync root is not configured")
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", errors.New("local sync root is not configured")
	}
	return cfg.Root, nil
}

func validateBotletsBrainProjectionPath(syncRoot string, brainRoot string) error {
	current := filepath.Clean(syncRoot)
	rel, err := filepath.Rel(current, brainRoot)
	if err != nil {
		return err
	}
	rel = filepath.Clean(rel)
	if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return errors.New("brain projection is outside local sync root")
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return errBotletsBrainProjectionPathMissing
			}
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("brain projection path must not contain symlinks")
		}
		if !info.IsDir() {
			return errors.New("brain projection path must be a directory")
		}
		if err := validateCurrentUserOwner(info, "brain projection path"); err != nil {
			return err
		}
		if info.Mode().Perm()&0o022 != 0 {
			return errors.New("brain projection path must not be group- or world-writable")
		}
	}
	return nil
}

type botletsBrainProjectionPlacement struct {
	Root         string
	RelativePath string
	Hints        []BotletsRepairHint
}

func resolveBotletsBrainProjectionPlacement(paths Paths, rawSyncRoot string, trustedSyncRoot string, bot BotRegistryEntry) (botletsBrainProjectionPlacement, error) {
	if bot.BrainRef == nil {
		return botletsBrainProjectionPlacement{}, errors.New("missing brain reference")
	}
	dbPath := filepath.Join(paths.Home, "sync", "library.sqlite")
	info, err := os.Lstat(dbPath)
	if err != nil {
		return botletsBrainProjectionPlacement{}, errors.New("local sync placement state is not available")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return botletsBrainProjectionPlacement{}, errors.New("local sync placement state must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return botletsBrainProjectionPlacement{}, errors.New("local sync placement state is not a regular file")
	}
	if err := validateCurrentUserOwner(info, "local sync placement state"); err != nil {
		return botletsBrainProjectionPlacement{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return botletsBrainProjectionPlacement{}, errors.New("local sync placement state must be private")
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return botletsBrainProjectionPlacement{}, errors.New("could not open local sync placement state")
	}
	defer db.Close()
	hasBotIDColumn := sqliteTableHasColumn(db, "placements", "botlets_bot_id")
	selectFields := `path, botlets_bot_agent_id, botlets_brain_container_id, botlets_brain_root_folder_id`
	if hasBotIDColumn {
		selectFields = `path, botlets_bot_id, botlets_bot_agent_id, botlets_brain_container_id, botlets_brain_root_folder_id`
	}
	rows, err := db.Query(
		`SELECT `+selectFields+`
		FROM placements
		WHERE section = 'botlets-brains'
			AND botlets_bot_agent_id = ?
			AND botlets_brain_container_id = ?
			AND botlets_brain_root_folder_id = ?`,
		bot.BrainRef.BotAgentID,
		bot.BrainRef.ContainerID,
		bot.BrainRef.RootFolderID,
	)
	if err != nil {
		return botletsBrainProjectionPlacement{}, errors.New("could not read local sync placement state")
	}
	defer rows.Close()
	var repairable *botletsBrainProjectionPlacement
	for rows.Next() {
		var placementPath, botID, botAgentID, containerID, rootFolderID string
		if hasBotIDColumn {
			if err := rows.Scan(&placementPath, &botID, &botAgentID, &containerID, &rootFolderID); err != nil {
				return botletsBrainProjectionPlacement{}, errors.New("could not read local sync placement state")
			}
		} else {
			if err := rows.Scan(&placementPath, &botAgentID, &containerID, &rootFolderID); err != nil {
				return botletsBrainProjectionPlacement{}, errors.New("could not read local sync placement state")
			}
		}
		if botAgentID != bot.BrainRef.BotAgentID || containerID != bot.BrainRef.ContainerID || rootFolderID != bot.BrainRef.RootFolderID {
			continue
		}
		if bot.BotID != "" && botID != "" && botID != bot.BotID {
			continue
		}
		relative := botletsBrainRelativePathFromPlacement(trustedSyncRoot, placementPath)
		if relative == "" {
			relative = botletsBrainRelativePathFromPlacement(rawSyncRoot, placementPath)
		}
		if relative == "" {
			continue
		}
		resolution := botletsBrainProjectionPlacement{
			Root:         filepath.Join(trustedSyncRoot, filepath.FromSlash(relative)),
			RelativePath: relative,
		}
		if relative == bot.BrainRef.RelativePath {
			return resolution, nil
		}
		if repairable == nil {
			resolution.Hints = []BotletsRepairHint{{
				Code:             BotletsRepairHintSyncPathMovePending,
				CanContinue:      true,
				CanonicalProfile: bot.Handle,
				CanonicalBotName: bot.Name,
				SuggestedPath:    resolution.Root,
			}}
			repairable = &resolution
		}
	}
	if err := rows.Err(); err != nil {
		return botletsBrainProjectionPlacement{}, errors.New("could not read local sync placement state")
	}
	if repairable != nil {
		return *repairable, nil
	}
	return botletsBrainProjectionPlacement{}, errors.New("local sync placement metadata does not match brain reference")
}

func sqliteTableHasColumn(db *sql.DB, table string, column string) bool {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var typ string
		var notnull int
		var defaultValue sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			return false
		}
		if name == column {
			return true
		}
	}
	return false
}

func botletsBrainRelativePathFromPlacement(syncRoot string, placementPath string) string {
	rel, err := filepath.Rel(syncRoot, placementPath)
	if err != nil {
		return ""
	}
	rel = filepath.Clean(rel)
	if rel == "." || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) < 4 || parts[0] != "Botlets" || parts[1] == "" || parts[2] == "" || parts[3] != "brain" {
		return ""
	}
	return strings.Join(parts[:4], "/")
}

func resolveRegistryPath(base string, value string) (string, error) {
	if !isSafeRegistryCredentialPath(value) {
		return "", errors.New("invalid path")
	}
	resolved := value
	var err error
	if isHomePath(value) {
		resolved, err = expandHome(value)
		if err != nil {
			return "", err
		}
	} else if !filepath.IsAbs(value) {
		resolved = filepath.Join(base, filepath.Clean(value))
	}
	absolute, err := filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func ResolveBotletsHome(home string) (string, error) {
	if home == "" {
		home = defaultBotletsHome()
	}
	if !isSafeHomeOrAbsolutePath(home) {
		return "", errors.New("invalid botlets home")
	}
	return expandHome(home)
}

func ValidateBotletsRegistryWriteTarget(home string) (string, error) {
	botletsHome, err := ResolveBotletsHome(home)
	if err != nil {
		return "", err
	}
	parent := normalizeTrustedBotletsParentPath(filepath.Dir(botletsHome))
	if err := validateTrustedSearchPathDir(parent, "botlets home parent"); err != nil {
		return "", err
	}
	info, err := os.Lstat(botletsHome)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("botlets home must not be a symlink")
	}
	if err == nil && !info.IsDir() {
		return "", errors.New("botlets home is not a directory")
	}
	if err == nil {
		if err := validateBotletsHomeTrust(botletsHome); err != nil {
			return "", err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("could not inspect botlets home")
	}
	registryPath := filepath.Join(botletsHome, "registry.json")
	if err := validateRegistryFileTrust(registryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return botletsHome, nil
}

func ValidateAgentProfileWriteTarget(paths Paths, handle string) (string, error) {
	if !ProfileRE.MatchString(handle) {
		return "", errors.New("invalid profile handle")
	}
	home := filepath.Clean(paths.Home)
	if err := ensureTrustedPrivateDir(home, "comment home"); err != nil {
		return "", err
	}
	agentsDir := filepath.Join(home, "agents")
	if err := ensureTrustedPrivateDir(agentsDir, "agent profiles directory"); err != nil {
		return "", err
	}
	return filepath.Join(agentsDir, handle+".json"), nil
}

func ensureTrustedPrivateDir(path string, label string) error {
	clean := filepath.Clean(path)
	parent := normalizeTrustedBotletsParentPath(filepath.Dir(clean))
	if err := validateTrustedSearchPathDir(parent, label+" parent"); err != nil {
		return err
	}
	info, err := os.Lstat(clean)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(clean, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := os.Chmod(clean, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(clean)
	}
	if err != nil {
		return errors.New("could not inspect " + label)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New(label + " must not be a symlink")
	}
	if !info.IsDir() {
		return errors.New(label + " is not a directory")
	}
	if err := validateCurrentUserOwner(info, label); err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New(label + " must not be group- or world-writable")
	}
	return nil
}

func normalizeTrustedBotletsParentPath(path string) string {
	clean := filepath.Clean(path)
	if runtime.GOOS == "darwin" && (clean == "/var" || strings.HasPrefix(clean, "/var/")) {
		return filepath.Clean("/private/var" + strings.TrimPrefix(clean, "/var"))
	}
	return clean
}

func validateBotletsHomeTrust(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("could not inspect botlets home")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("botlets home must not be a symlink")
	}
	if !info.IsDir() {
		return errors.New("botlets home is not a directory")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("botlets home must not be group- or world-writable")
	}
	if err := validateCurrentUserOwner(info, "botlets home"); err != nil {
		return err
	}
	return nil
}

func validateAgentDirectoryTrust(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return errors.New("could not inspect agent profiles directory")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("agent profiles directory must not be a symlink")
	}
	if !info.IsDir() {
		return errors.New("agent profiles path is not a directory")
	}
	if err := validateCurrentUserOwner(info, "agent profiles directory"); err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("agent profiles directory must not be group- or world-writable")
	}
	return nil
}

func validateAgentProfileFileTrust(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("could not inspect profile")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("profile file must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return errors.New("profile path is not a regular file")
	}
	if err := validateCurrentUserOwner(info, "profile file"); err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return errors.New("profile file must be private")
	}
	return nil
}

func validateRegistryFileTrust(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return os.ErrNotExist
		}
		return errors.New("could not inspect botlets registry")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("botlets registry must not be a symlink")
	}
	if !info.Mode().IsRegular() {
		return errors.New("botlets registry is not a regular file")
	}
	if err := validateCurrentUserOwner(info, "botlets registry"); err != nil {
		return err
	}
	if info.Mode().Perm()&0o022 != 0 {
		return errors.New("botlets registry must not be group- or world-writable")
	}
	return nil
}

func defaultBotletsHome() string {
	if value := os.Getenv("BOTLETS_HOME"); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "botlets")
}

func normalizeBaseURL(value string) string {
	return strings.TrimSuffix(value, "/")
}

func resolveDefaultBaseURL(value string) string {
	value = normalizeBaseURL(value)
	if value != "" {
		return value
	}
	return CurrentEnvironment().DefaultBaseURL()
}

// DefaultBaseURL returns the canonical Comment.io base URL after applying the
// COMMENT_IO_BASE_URL override. Callers outside the daemon (e.g. CLI
// subcommands that need a base URL without loading any profile) should use
// this rather than hardcoding the value.
func DefaultBaseURL() string {
	return resolveDefaultBaseURL("")
}

func isSafeRegistryCredentialPath(value string) bool {
	if !isSafePathString(value) || strings.Contains(value, "://") {
		return false
	}
	if filepath.IsAbs(value) || isHomePath(value) {
		return true
	}
	cleaned := filepath.Clean(value)
	return cleaned != "." && cleaned != ".." && !strings.HasPrefix(cleaned, ".."+string(filepath.Separator))
}

func isSafeHomeOrAbsolutePath(value string) bool {
	return isSafePathString(value) && (filepath.IsAbs(value) || isHomePath(value))
}

func isSafePathString(value string) bool {
	return value != "" && len(value) <= 4096 && !strings.ContainsAny(value, "\r\n\x00") && !containsSecretValue(value)
}

func isHomePath(value string) bool {
	return value == "~" || strings.HasPrefix(value, "~/")
}
