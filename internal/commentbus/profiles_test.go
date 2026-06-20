//go:build darwin || linux

package commentbus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadProfileStateLoadsAgentProfilesAndBotletsRegistry(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"handle":       "max.reviewer",
		"agent_secret": "as_agent_profile_secret",
		"base_url":     "https://comment.example/",
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{{
		"name":               "reviewer",
		"handle":             "max.reviewer",
		"credential_profile": "~/.comment-io/agents/max.reviewer.json",
		"managed_session": map[string]any{
			"enabled": true,
		},
		"brain_ref": map[string]any{
			"workspace_id":     "ws_brain",
			"owner_agent_id":   "ag_owner",
			"bot_agent_id":     "ag_bot",
			"container_id":     "lc_brain",
			"root_folder_id":   "lf_brain",
			"relative_path":    "Botlets/max/reviewer/brain",
			"setup_generation": 5,
		},
	}})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:          paths,
		BotletsHome:    botletsHome,
		DefaultBaseURL: "https://fallback.example/",
	})
	if len(errorsOut) != 0 {
		t.Fatalf("errors = %+v", errorsOut)
	}
	profile := state.AgentProfiles["max.reviewer"]
	if profile.Handle != "max.reviewer" || profile.AgentSecret != "as_agent_profile_secret" || profile.BaseURL != "https://comment.example" {
		t.Fatalf("profile = %+v", profile)
	}
	if profile.Runtime != "" {
		t.Fatalf("profile runtime = %q, want empty legacy default", profile.Runtime)
	}
	if profile.Path != filepath.Clean(profilePath) {
		t.Fatalf("profile path = %q, want %q", profile.Path, filepath.Clean(profilePath))
	}
	bot := state.BotRegistry["reviewer"]
	if bot.Name != "reviewer" || bot.Handle != "max.reviewer" || bot.CredentialPath != profile.Path {
		t.Fatalf("bot = %+v", bot)
	}
	if !bot.ManagedSession.Enabled || bot.ManagedSession.Runtime != "claude" || bot.ManagedSession.Host != SessionHostTmux {
		t.Fatalf("managed session = %+v", bot.ManagedSession)
	}
	if bot.BrainRef == nil || bot.BrainRef.WorkspaceID != "ws_brain" || bot.BrainRef.OwnerAgentID != "ag_owner" || bot.BrainRef.BotAgentID != "ag_bot" || bot.BrainRef.SetupGeneration != 5 || bot.BrainRef.RelativePath != "Botlets/max/reviewer/brain" {
		t.Fatalf("brain ref = %+v", bot.BrainRef)
	}
}

func TestLoadProfileStateLoadsAgentProfileRuntime(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	writeAgentProfile(t, paths, "max.codex", map[string]any{
		"agent_secret": "as_agent_profile_secret",
		"runtime":      "codex",
	})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: filepath.Join(root, "botlets"),
	})
	if len(errorsOut) != 0 {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if got := state.AgentProfiles["max.codex"].Runtime; got != "codex" {
		t.Fatalf("profile runtime = %q, want codex", got)
	}
}

func TestLoadProfileStateUsesAgentProfileRuntimeForBotlets(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeAgentProfile(t, paths, "max.codex", map[string]any{
		"agent_secret": "as_agent_profile_secret",
		"runtime":      "codex",
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{{
		"name":               "codex-bot",
		"handle":             "max.codex",
		"credential_profile": profilePath,
		"managed_session": map[string]any{
			"enabled": true,
		},
	}})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 0 {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if got := state.BotRegistry["codex-bot"].ManagedSession.Runtime; got != "codex" {
		t.Fatalf("managed runtime = %q, want codex", got)
	}
}

func TestLoadProfileStateRejectsInvalidAgentProfileRuntime(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	writeAgentProfile(t, paths, "max.bad", map[string]any{
		"agent_secret": "as_agent_profile_secret",
		"runtime":      "gpt",
	})

	_, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: filepath.Join(root, "botlets"),
	})
	if len(errorsOut) == 0 || errorsOut[0].Code != "INVALID_AGENT_PROFILE" {
		t.Fatalf("errors = %+v, want invalid profile", errorsOut)
	}
}

func TestLoadProfileStateLoadsBotIdentityAliases(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeAgentProfile(t, paths, "max.research-reader", map[string]any{
		"handle":       "max.research-reader",
		"agent_secret": "as_agent_profile_secret",
		"base_url":     "https://comment.example/",
	})
	writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"profile_kind":         "alias",
		"alias_of":             "max.research-reader",
		"bot_id":               "ag_bot",
		"bot_agent_id":         "ag_bot",
		"disabled_for_polling": true,
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{{
		"name":               "research-reader",
		"bot_id":             "ag_bot",
		"handle":             "max.research-reader",
		"slug_aliases":       []string{"reviewer"},
		"handle_aliases":     []string{"max.reviewer"},
		"credential_profile": profilePath,
		"managed_session": map[string]any{
			"enabled": true,
		},
		"brain_ref": map[string]any{
			"workspace_id":     "ws_brain",
			"owner_agent_id":   "ag_owner",
			"bot_agent_id":     "ag_bot",
			"container_id":     "lc_brain",
			"root_folder_id":   "lf_brain",
			"relative_path":    "Botlets/max/reviewer/brain",
			"setup_generation": 5,
		},
	}})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 0 {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if _, ok := state.AgentProfiles["max.reviewer"]; ok {
		t.Fatalf("alias profile was loaded as a polling profile: %+v", state.AgentProfiles)
	}
	alias := state.ProfileAliases["max.reviewer"]
	if alias.AliasOf != "max.research-reader" || alias.BotID != "ag_bot" || !alias.DisabledForPolling {
		t.Fatalf("alias = %+v", alias)
	}
	bot := state.BotRegistry["research-reader"]
	if bot.BotID != "ag_bot" || !bot.MatchesSelector("reviewer") || !bot.MatchesSelector("max.reviewer") || !bot.MatchesSlug("reviewer") || bot.MatchesSlug("max.reviewer") || !bot.MatchesProfile("max.reviewer") {
		t.Fatalf("bot aliases not loaded: %+v", bot)
	}
	if bot.BrainRef == nil || bot.BrainRef.RelativePath != "Botlets/max/reviewer/brain" {
		t.Fatalf("brain ref = %+v", bot.BrainRef)
	}
}

func TestLoadProfileStateRejectsDuplicateBotAgentID(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	firstProfilePath := writeAgentProfile(t, paths, "max.first", map[string]any{
		"handle":       "max.first",
		"agent_secret": "as_first_secret",
	})
	secondProfilePath := writeAgentProfile(t, paths, "max.second", map[string]any{
		"handle":       "max.second",
		"agent_secret": "as_second_secret",
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{
		{
			"name": "first", "bot_id": "ag_first_bot", "handle": "max.first", "credential_profile": firstProfilePath,
			"brain_ref":       map[string]any{"workspace_id": "ws_brain", "owner_agent_id": "ag_owner", "bot_agent_id": "ag_same_agent", "container_id": "lc_brain", "root_folder_id": "lf_brain", "relative_path": "Botlets/max/first/brain", "setup_generation": 1},
			"managed_session": map[string]any{"enabled": true},
		},
		{
			"name": "second", "bot_id": "ag_second_bot", "handle": "max.second", "credential_profile": secondProfilePath,
			"brain_ref":       map[string]any{"workspace_id": "ws_brain", "owner_agent_id": "ag_owner", "bot_agent_id": "ag_same_agent", "container_id": "lc_brain", "root_folder_id": "lf_brain", "relative_path": "Botlets/max/second/brain", "setup_generation": 1},
			"managed_session": map[string]any{"enabled": true},
		},
	})

	_, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) == 0 {
		t.Fatal("LoadProfileState accepted duplicate bot agent ids")
	}
	found := false
	for _, err := range errorsOut {
		if err.Code == "DUPLICATE_BOT_AGENT_ID" {
			found = true
		}
	}
	if !found {
		t.Fatalf("errors = %+v, want DUPLICATE_BOT_AGENT_ID", errorsOut)
	}
}

func TestLoadProfileStateAllowsBmuxCodexManagedSession(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{{
		"name":               "reviewer",
		"handle":             "max.reviewer",
		"credential_profile": profilePath,
		"managed_session": map[string]any{
			"enabled": true,
			"runtime": "codex",
			"host":    "bmux",
		},
	}})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 0 {
		t.Fatalf("errors = %+v", errorsOut)
	}
	bot := state.BotRegistry["reviewer"]
	if bot.ManagedSession.Runtime != "codex" || bot.ManagedSession.Host != SessionHostBmux {
		t.Fatalf("managed session = %+v, want bmux Codex", bot.ManagedSession)
	}
}

func TestLoadProfileStateAllowsExistingReadableAgentDirectoryWithPrivateFiles(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	if err := os.Chmod(filepath.Dir(profilePath), 0o755); err != nil {
		t.Fatal(err)
	}

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: filepath.Join(root, "botlets"),
	})
	if len(errorsOut) != 0 {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if _, ok := state.AgentProfiles["max.reviewer"]; !ok {
		t.Fatalf("profile was not loaded from readable agents directory: %+v", state.AgentProfiles)
	}
}

func TestLoadProfileStateUsesDefaultBaseURLFromEnv(t *testing.T) {
	root := t.TempDir()
	t.Setenv("COMMENT_IO_BASE_URL", "https://env.comment.local/")
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: filepath.Join(root, "botlets"),
	})
	if len(errorsOut) != 0 {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if got := state.AgentProfiles["max.reviewer"].BaseURL; got != "https://env.comment.local" {
		t.Fatalf("base URL = %q", got)
	}
}

func TestLoadProfileStateReportsRegistryErrorsWithoutSecrets(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	writeJSONFile(t, filepath.Join(paths.Home, "agents", "as_secret_filename.json"), map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{
		{
			"name":               "reviewer",
			"handle":             "max.writer",
			"credential_profile": "~/.comment-io/agents/max.reviewer.json",
		},
		{
			"name":               "second-reviewer",
			"handle":             "max.reviewer",
			"credential_profile": "../outside.json",
		},
		{
			"name":               "cap_secret_bot",
			"handle":             "as_secret_profile",
			"credential_profile": "~/.comment-io/agents/max.reviewer.json",
		},
	})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 4 {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if len(state.BotRegistry) != 0 {
		t.Fatalf("bot registry loaded despite errors: %+v", state.BotRegistry)
	}
	encoded := mustMarshalJSON(t, errorsOut)
	if containsSecretValue(encoded) {
		t.Fatalf("profile errors leaked secret-shaped value: %s", encoded)
	}
	if errorsOut[0].Code != "INVALID_AGENT_PROFILE" || errorsOut[1].Code != "HANDLE_MISMATCH" || errorsOut[2].Code != "INVALID_CREDENTIAL_PROFILE" || errorsOut[3].Code != "INVALID_BOT" {
		t.Fatalf("error codes = %+v", errorsOut)
	}
}

func TestLoadProfileStateRejectsUnsafeBotletsBrainRef(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{{
		"name":               "reviewer",
		"handle":             "max.reviewer",
		"credential_profile": "~/.comment-io/agents/max.reviewer.json",
		"brain_ref": map[string]any{
			"workspace_id":   "ws_brain",
			"container_id":   "lc_brain",
			"root_folder_id": "lf_brain",
			"relative_path":  "../outside",
		},
	}})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 1 || errorsOut[0].Code != "INVALID_BOT" {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if len(state.BotRegistry) != 0 {
		t.Fatalf("bot registry loaded despite invalid brain ref: %+v", state.BotRegistry)
	}
}

func TestLoadProfileStateHintsStaleCanonicalCredentialProfile(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{{
		"name":               "research-reader",
		"handle":             "max.research-reader",
		"bot_id":             "bot_reviewer_stable",
		"handle_aliases":     []string{"max.reviewer"},
		"credential_profile": profilePath,
		"brain_ref": map[string]any{
			"workspace_id":     "ws_brain",
			"owner_agent_id":   "ag_owner",
			"bot_agent_id":     "ag_reviewer_stable",
			"container_id":     "lc_brain",
			"root_folder_id":   "lf_brain",
			"relative_path":    "Botlets/max/research-reader/brain",
			"setup_generation": 5,
		},
	}})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(state.BotRegistry) != 0 {
		t.Fatalf("bot registry loaded despite stale credential profile: %+v", state.BotRegistry)
	}
	if len(errorsOut) != 1 || errorsOut[0].Code != "HANDLE_MISMATCH" {
		t.Fatalf("errors = %+v, want one HANDLE_MISMATCH", errorsOut)
	}
	if len(errorsOut[0].Hints) != 1 {
		t.Fatalf("hints = %+v, want one canonical-profile hint", errorsOut[0].Hints)
	}
	hint := errorsOut[0].Hints[0]
	if hint.Code != BotletsRepairHintCanonicalProfileMoved || hint.CanContinue || hint.CanonicalProfile != "max.research-reader" || hint.CanonicalBotName != "research-reader" {
		t.Fatalf("hint = %+v", hint)
	}
	encoded := mustMarshalJSON(t, errorsOut)
	if !strings.Contains(encoded, BotletsRepairHintCanonicalProfileMoved) {
		t.Fatalf("encoded errors missing repair hint: %s", encoded)
	}
	if containsSecretValue(encoded) {
		t.Fatalf("profile errors leaked secret-shaped value: %s", encoded)
	}
}

func TestLoadProfileStateRejectsIncompleteBotletsBrainRef(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{{
		"name":               "reviewer",
		"handle":             "max.reviewer",
		"credential_profile": "~/.comment-io/agents/max.reviewer.json",
		"brain_ref": map[string]any{
			"workspace_id":   "ws_brain",
			"container_id":   "lc_brain",
			"root_folder_id": "lf_brain",
			"relative_path":  "Botlets/max/reviewer/brain",
		},
	}})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 1 || errorsOut[0].Code != "INVALID_BOT" {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if len(state.BotRegistry) != 0 {
		t.Fatalf("bot registry loaded despite incomplete brain ref: %+v", state.BotRegistry)
	}
}

func TestLoadProfileStateRejectsBotletsBrainRefOutsideBrainRootShape(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{{
		"name":               "reviewer",
		"handle":             "max.reviewer",
		"credential_profile": "~/.comment-io/agents/max.reviewer.json",
		"brain_ref": map[string]any{
			"workspace_id":     "ws_brain",
			"owner_agent_id":   "ag_owner",
			"bot_agent_id":     "ag_bot",
			"container_id":     "lc_brain",
			"root_folder_id":   "lf_brain",
			"relative_path":    "Botlets",
			"setup_generation": 5,
		},
	}})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 1 || errorsOut[0].Code != "INVALID_BOT" {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if len(state.BotRegistry) != 0 {
		t.Fatalf("bot registry loaded despite invalid brain ref shape: %+v", state.BotRegistry)
	}
}

func TestLoadProfileStateRejectsBotletsBrainRefForWrongOwnerOrBotPath(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{{
		"name":               "reviewer",
		"handle":             "max.reviewer",
		"credential_profile": "~/.comment-io/agents/max.reviewer.json",
		"brain_ref": map[string]any{
			"workspace_id":     "ws_brain",
			"owner_agent_id":   "ag_owner",
			"bot_agent_id":     "ag_bot",
			"container_id":     "lc_brain",
			"root_folder_id":   "lf_brain",
			"relative_path":    "Botlets/max/other-bot/brain",
			"setup_generation": 5,
		},
	}})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 1 || errorsOut[0].Code != "INVALID_BOT" {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if len(state.BotRegistry) != 0 {
		t.Fatalf("bot registry loaded despite mismatched brain ref path: %+v", state.BotRegistry)
	}
}

func TestLoadProfileStateRejectsNonCanonicalBotletsBrainRefPath(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{{
		"name":               "reviewer",
		"handle":             "max.reviewer",
		"credential_profile": "~/.comment-io/agents/max.reviewer.json",
		"brain_ref": map[string]any{
			"workspace_id":     "ws_brain",
			"owner_agent_id":   "ag_owner",
			"bot_agent_id":     "ag_bot",
			"container_id":     "lc_brain",
			"root_folder_id":   "lf_brain",
			"relative_path":    "Botlets/max/reviewer/brain/.",
			"setup_generation": 5,
		},
	}})

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 1 || errorsOut[0].Code != "INVALID_BOT" {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if len(state.BotRegistry) != 0 {
		t.Fatalf("bot registry loaded despite non-canonical brain ref path: %+v", state.BotRegistry)
	}
}

func TestValidateBotletsBrainProjectionRejectsSymlinkedAncestor(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	syncRoot := filepath.Join(root, "Comment Docs")
	realBotRoot := filepath.Join(root, "real-reviewer")
	if err := os.MkdirAll(filepath.Join(realBotRoot, "brain"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(syncRoot, "Botlets", "max"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realBotRoot, filepath.Join(syncRoot, "Botlets", "max", "reviewer")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	writeJSONFile(t, filepath.Join(paths.Home, "sync", "config.json"), map[string]any{
		"version":           1,
		"root":              syncRoot,
		"base_url":          "https://comt.dev",
		"scope":             "library-sync:read:botlets-brains",
		"scope_label":       "Botlets brains",
		"config_generation": 1,
		"configured_at":     "2026-05-22T00:00:00Z",
		"background_sync":   false,
		"manual_only":       true,
		"live_sync_enabled": false,
	})

	_, err = ValidateBotletsBrainProjection(paths, BotRegistryEntry{
		Name:   "reviewer",
		Handle: "max.reviewer",
		BrainRef: &BotBrainRef{
			WorkspaceID:     "ws_brain",
			OwnerAgentID:    "ag_owner",
			BotAgentID:      "ag_bot",
			ContainerID:     "lc_brain",
			RootFolderID:    "lf_brain",
			RelativePath:    "Botlets/max/reviewer/brain",
			SetupGeneration: 5,
		},
	})
	if err == nil {
		t.Fatal("expected symlinked brain ancestor rejection")
	}
}

func TestValidateBotletsBrainProjectionAcceptsCurrentProjection(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	brainRoot := writeLocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")

	got, err := ValidateBotletsBrainProjection(paths, BotRegistryEntry{
		Name:   "reviewer",
		Handle: "max.reviewer",
		BrainRef: &BotBrainRef{
			WorkspaceID:     "ws_brain",
			OwnerAgentID:    "ag_owner",
			BotAgentID:      "ag_bot",
			ContainerID:     "lc_brain",
			RootFolderID:    "lf_brain",
			RelativePath:    "Botlets/max/reviewer/brain",
			SetupGeneration: 5,
		},
	})
	if err != nil {
		t.Fatalf("validate projection: %v", err)
	}
	if got != normalizeTrustedBotletsParentPath(brainRoot) {
		t.Fatalf("projection path = %q, want %q", got, brainRoot)
	}
}

func TestValidateBotletsBrainProjectionAcceptsAliasProjectionLabels(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	brainRoot := writeLocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")

	got, err := ValidateBotletsBrainProjection(paths, BotRegistryEntry{
		Name:          "research-reader",
		Handle:        "max.research-reader",
		BotID:         "ag_bot",
		SlugAliases:   []string{"reviewer"},
		HandleAliases: []string{"max.reviewer"},
		BrainRef: &BotBrainRef{
			WorkspaceID:     "ws_brain",
			OwnerAgentID:    "ag_owner",
			BotAgentID:      "ag_bot",
			ContainerID:     "lc_brain",
			RootFolderID:    "lf_brain",
			RelativePath:    "Botlets/max/reviewer/brain",
			SetupGeneration: 5,
		},
	})
	if err != nil {
		t.Fatalf("validate alias projection: %v", err)
	}
	if got != normalizeTrustedBotletsParentPath(brainRoot) {
		t.Fatalf("projection path = %q, want %q", got, brainRoot)
	}
}

func TestValidateBotletsBrainProjectionReturnsRepairHintForStableIDsAtDifferentPath(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	_ = writeLocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")
	syncRoot := filepath.Join(paths.Home, "Comment Docs")
	renamedBrainRoot := filepath.Join(syncRoot, "Botlets", "max", "renamed-reviewer", "brain")
	if err := os.MkdirAll(renamedBrainRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for current := renamedBrainRoot; ; current = filepath.Dir(current) {
		if err := os.Chmod(current, 0o700); err != nil {
			t.Fatal(err)
		}
		if current == syncRoot {
			break
		}
	}
	db, err := sql.Open("sqlite", filepath.Join(paths.Home, "sync", "library.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE placements SET path = ? WHERE visible_id = ?`, filepath.Join(renamedBrainRoot, "Identity.md"), "brain-doc-1"); err != nil {
		t.Fatal(err)
	}

	got, hints, err := ValidateBotletsBrainProjectionWithRepairHints(paths, BotRegistryEntry{
		Name:          "reviewer",
		Handle:        "max.reviewer",
		BotID:         "ag_bot",
		SlugAliases:   []string{"renamed-reviewer"},
		HandleAliases: []string{"max.renamed-reviewer"},
		BrainRef: &BotBrainRef{
			WorkspaceID:     "ws_brain",
			OwnerAgentID:    "ag_owner",
			BotAgentID:      "ag_bot",
			ContainerID:     "lc_brain",
			RootFolderID:    "lf_brain",
			RelativePath:    "Botlets/max/reviewer/brain",
			SetupGeneration: 5,
		},
	})
	if err != nil {
		t.Fatalf("validate stale path with stable identity: %v", err)
	}
	if got != normalizeTrustedBotletsParentPath(renamedBrainRoot) {
		t.Fatalf("projection path = %q, want renamed placement %q", got, renamedBrainRoot)
	}
	if len(hints) != 1 {
		t.Fatalf("repair hints = %#v, want one hint", hints)
	}
	hint := hints[0]
	if hint.Code != BotletsRepairHintSyncPathMovePending || !hint.CanContinue || hint.CanonicalProfile != "max.reviewer" || hint.CanonicalBotName != "reviewer" || hint.SuggestedPath != normalizeTrustedBotletsParentPath(renamedBrainRoot) {
		t.Fatalf("repair hint = %#v", hint)
	}
}

func TestBotletsBrainRootForProfileReturnsValidatedProjection(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	brainRoot := writeLocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")
	state := ProfileState{
		BotRegistry: map[string]BotRegistryEntry{
			"reviewer": {
				Name:   "reviewer",
				Handle: "max.reviewer",
				BrainRef: &BotBrainRef{
					WorkspaceID:     "ws_brain",
					OwnerAgentID:    "ag_owner",
					BotAgentID:      "ag_bot",
					ContainerID:     "lc_brain",
					RootFolderID:    "lf_brain",
					RelativePath:    "Botlets/max/reviewer/brain",
					SetupGeneration: 5,
				},
			},
		},
	}
	got, ok := BotletsBrainRootForProfile(paths, state, "max.reviewer")
	if !ok || got != normalizeTrustedBotletsParentPath(brainRoot) {
		t.Fatalf("brain root = %q ok=%v, want %q true", got, ok, brainRoot)
	}
}

func TestResolveBotletsBrainProjectionHintAllowsMissingProjectionPath(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	brainRoot := writeLocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")
	if err := os.RemoveAll(brainRoot); err != nil {
		t.Fatal(err)
	}
	bot := BotRegistryEntry{
		Name:   "reviewer",
		Handle: "max.reviewer",
		BrainRef: &BotBrainRef{
			WorkspaceID:     "ws_brain",
			OwnerAgentID:    "ag_owner",
			BotAgentID:      "ag_bot",
			ContainerID:     "lc_brain",
			RootFolderID:    "lf_brain",
			RelativePath:    "Botlets/max/reviewer/brain",
			SetupGeneration: 5,
		},
	}
	if _, err := ValidateBotletsBrainProjection(paths, bot); !errors.Is(err, errBotletsBrainProjectionPathMissing) {
		t.Fatalf("strict validation err = %v, want missing path", err)
	}
	got, err := ResolveBotletsBrainProjectionHint(paths, bot)
	if err != nil {
		t.Fatalf("hint returned error: %v", err)
	}
	if got != normalizeTrustedBotletsParentPath(brainRoot) {
		t.Fatalf("hint path = %q, want %q", got, brainRoot)
	}
}

func TestValidateBotletsBrainProjectionPathPreservesNonMissingStatErrors(t *testing.T) {
	root := t.TempDir()
	tooLongPart := strings.Repeat("a", 300)
	err := validateBotletsBrainProjectionPath(root, filepath.Join(root, tooLongPart))
	if err == nil {
		t.Fatal("expected path validation error")
	}
	if errors.Is(err, errBotletsBrainProjectionPathMissing) {
		t.Fatalf("err = %v, want original non-missing filesystem error", err)
	}
}

func writeLocalSyncBrainProjectionForTest(t *testing.T, paths Paths, relativePath string) string {
	t.Helper()
	root := filepath.Join(paths.Home, "Comment Docs")
	brainRoot := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(brainRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for current := brainRoot; ; current = filepath.Dir(current) {
		if err := os.Chmod(current, 0o700); err != nil {
			t.Fatal(err)
		}
		if current == root {
			break
		}
	}
	writeJSONFile(t, filepath.Join(paths.Home, "sync", "config.json"), map[string]any{
		"version":           1,
		"root":              root,
		"base_url":          "https://comt.dev",
		"scope":             "library-sync:read:botlets-brains",
		"scope_label":       "Botlets brains",
		"config_generation": 1,
		"configured_at":     "2026-05-22T00:00:00Z",
		"background_sync":   false,
		"manual_only":       true,
		"live_sync_enabled": false,
	})
	dbPath := filepath.Join(paths.Home, "sync", "library.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS placements (
		visible_id TEXT PRIMARY KEY,
		section TEXT NOT NULL,
		path TEXT NOT NULL,
		botlets_bot_agent_id TEXT NOT NULL DEFAULT '',
		botlets_brain_container_id TEXT NOT NULL DEFAULT '',
		botlets_brain_root_folder_id TEXT NOT NULL DEFAULT '',
		botlets_bot_handle TEXT NOT NULL DEFAULT '',
		botlets_bot_slug TEXT NOT NULL DEFAULT '',
		botlets_bot_local_name TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT OR REPLACE INTO placements (
		visible_id,
		section,
		path,
		botlets_bot_agent_id,
		botlets_brain_container_id,
		botlets_brain_root_folder_id,
		botlets_bot_handle,
		botlets_bot_slug,
		botlets_bot_local_name
	) VALUES (?, 'botlets-brains', ?, 'ag_bot', 'lc_brain', 'lf_brain', 'max.reviewer', 'reviewer', 'reviewer')`, "brain-doc-1", filepath.Join(brainRoot, "Identity.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		t.Fatal(err)
	}
	return brainRoot
}

func TestLoadProfileStateRequiresExplicitRegistryBotsArray(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	for name, registry := range map[string]any{
		"missing": map[string]any{},
		"null":    map[string]any{"bots": nil},
	} {
		t.Run(name, func(t *testing.T) {
			writeJSONFile(t, filepath.Join(botletsHome, "registry.json"), registry)
			state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
				Paths:       paths,
				BotletsHome: botletsHome,
			})
			if len(errorsOut) != 1 || errorsOut[0].Code != "INVALID_BOTLETS_REGISTRY" {
				t.Fatalf("errors = %+v", errorsOut)
			}
			if len(state.BotRegistry) != 0 {
				t.Fatalf("bot registry loaded despite invalid registry: %+v", state.BotRegistry)
			}
		})
	}

	writeBotletsRegistry(t, botletsHome, []map[string]any{})
	_, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 0 {
		t.Fatalf("explicit empty registry errors = %+v", errorsOut)
	}
}

func TestLoadProfileStateRejectsUntrustedBotletsHome(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o720, 0o777} {
		if err := os.Chmod(botletsHome, mode); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(botletsHome, 0o700)
		})

		_, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
			Paths:       paths,
			BotletsHome: botletsHome,
		})
		if len(errorsOut) != 1 || errorsOut[0].Code != "INVALID_BOTLETS_HOME" {
			t.Fatalf("mode %o errors = %+v", mode, errorsOut)
		}
	}
}

func TestLoadProfileStateRejectsUntrustedAgentProfileFiles(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	if err := os.Chmod(profilePath, 0o644); err != nil {
		t.Fatal(err)
	}

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: filepath.Join(root, "botlets"),
	})
	if len(errorsOut) != 1 || errorsOut[0].Code != "INVALID_AGENT_PROFILE" {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if len(state.AgentProfiles) != 0 {
		t.Fatalf("profile loaded despite untrusted file: %+v", state.AgentProfiles)
	}
	encoded := mustMarshalJSON(t, errorsOut)
	if containsSecretValue(encoded) {
		t.Fatalf("profile trust error leaked secret-shaped value: %s", encoded)
	}
}

func TestLoadProfileStateRejectsSymlinkedAgentDirectory(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	realAgents := filepath.Join(root, "real-agents")
	writeJSONFile(t, filepath.Join(realAgents, "max.reviewer.json"), map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realAgents, filepath.Join(paths.Home, "agents")); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	_, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: filepath.Join(root, "botlets"),
	})
	if len(errorsOut) != 1 || errorsOut[0].Code != "UNTRUSTED_AGENTS_DIR" {
		t.Fatalf("errors = %+v", errorsOut)
	}
}

func TestLoadProfileStateRejectsUntrustedRegistryFile(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	botletsHome := filepath.Join(root, "botlets")
	writeBotletsRegistry(t, botletsHome, []map[string]any{{
		"name":               "reviewer",
		"handle":             "max.reviewer",
		"credential_profile": profilePath,
	}})
	if err := os.Chmod(filepath.Join(botletsHome, "registry.json"), 0o666); err != nil {
		t.Fatal(err)
	}

	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 1 || errorsOut[0].Code != "UNTRUSTED_BOTLETS_REGISTRY" {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if len(state.BotRegistry) != 0 {
		t.Fatalf("bot registry loaded despite untrusted registry file: %+v", state.BotRegistry)
	}
}

func TestLoadProfileStateRejectsSymlinkedRegistryFile(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	profilePath := writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	botletsHome := filepath.Join(root, "botlets")
	realRegistry := filepath.Join(root, "real-registry.json")
	writeJSONFile(t, realRegistry, map[string]any{"bots": []map[string]any{{
		"name":               "reviewer",
		"handle":             "max.reviewer",
		"credential_profile": profilePath,
	}}})
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realRegistry, filepath.Join(botletsHome, "registry.json")); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	_, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 1 || errorsOut[0].Code != "UNTRUSTED_BOTLETS_REGISTRY" {
		t.Fatalf("errors = %+v", errorsOut)
	}
}

func TestLoadProfileStateRejectsSymlinkedBotletsHomeParent(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	writeAgentProfile(t, paths, "max.reviewer", map[string]any{
		"agent_secret": "as_agent_profile_secret",
	})
	realParent := filepath.Join(root, "real-parent")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(root, "link-parent")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	state, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: filepath.Join(linkParent, "botlets"),
	})
	if len(errorsOut) != 1 || errorsOut[0].Code != "INVALID_BOTLETS_HOME" {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if len(state.BotRegistry) != 0 {
		t.Fatalf("bot registry loaded through symlinked parent: %+v", state.BotRegistry)
	}
}

func TestLoadProfileStateTrustErrorsDoNotEchoSecretShapedPaths(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, "cap_secretshapedvalue1234567890", ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.Home, "agents"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(botletsHome, "registry.json"), 0o700); err != nil {
		t.Fatal(err)
	}

	_, errorsOut := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 2 {
		t.Fatalf("errors = %+v", errorsOut)
	}
	encoded := mustMarshalJSON(t, errorsOut)
	if containsSecretValue(encoded) {
		t.Fatalf("trust errors leaked secret-shaped path component: %s", encoded)
	}
}

func writeAgentProfile(t *testing.T, paths Paths, handle string, value map[string]any) string {
	t.Helper()
	path := filepath.Join(paths.Home, "agents", handle+".json")
	writeJSONFile(t, path, value)
	return filepath.Clean(path)
}

func writeBotletsRegistry(t *testing.T, botletsHome string, bots []map[string]any) {
	t.Helper()
	writeJSONFile(t, filepath.Join(botletsHome, "registry.json"), map[string]any{"bots": bots})
}

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustMarshalJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
