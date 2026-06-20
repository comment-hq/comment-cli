//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

func TestBotletsLogsOpensClaudeTranscriptForRun(t *testing.T) {
	fixture := setupBotletsRunLogFixture(t)
	var opened []string
	oldOpen := botletsOpenDefaultFile
	botletsOpenDefaultFile = func(path string) error {
		opened = append(opened, path)
		return nil
	}
	t.Cleanup(func() {
		botletsOpenDefaultFile = oldOpen
	})

	output, err := captureRun(t, []string{
		"botlets", "logs",
		"--home", fixture.home,
		"--botlets-home", fixture.botletsHome,
		"--bot", "max.runner",
		"--run", fixture.runID,
		"--claude-home", fixture.claudeHome,
		"--codex-home", fixture.codexHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || opened[0] != fixture.transcriptPath {
		t.Fatalf("opened = %v, want %s", opened, fixture.transcriptPath)
	}
	for _, want := range []string{
		"run: " + fixture.runID,
		"message: " + fixture.messageID,
		"transcript: " + fixture.transcriptPath + ":2",
		"runtime: claude",
		"runtime_session_id: claude-session-123",
		"cwd: /tmp/botlets",
		"opened: " + fixture.transcriptPath,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestBotletsLogsJSONDoesNotOpenTranscript(t *testing.T) {
	fixture := setupBotletsRunLogFixture(t)
	oldOpen := botletsOpenDefaultFile
	var opened []string
	botletsOpenDefaultFile = func(path string) error {
		opened = append(opened, path)
		return nil
	}
	t.Cleanup(func() {
		botletsOpenDefaultFile = oldOpen
	})

	output, err := captureRun(t, []string{
		"botlets", "logs",
		"--home", fixture.home,
		"--botlets-home", fixture.botletsHome,
		"--bot", "runner",
		"--message", fixture.messageID,
		"--claude-home", fixture.claudeHome,
		"--codex-home", fixture.codexHome,
		"--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 0 {
		t.Fatalf("json mode opened transcript: %v", opened)
	}
	var decoded struct {
		Opened bool `json:"opened"`
		Run    struct {
			MessageID  string `json:"message_id"`
			Transcript struct {
				Runtime          string `json:"runtime"`
				Path             string `json:"path"`
				Line             int    `json:"line"`
				RuntimeSessionID string `json:"runtime_session_id"`
			} `json:"transcript"`
		} `json:"run"`
	}
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Opened || decoded.Run.MessageID != fixture.messageID || decoded.Run.Transcript.Path != fixture.transcriptPath || decoded.Run.Transcript.Line != 2 || decoded.Run.Transcript.Runtime != "claude" || decoded.Run.Transcript.RuntimeSessionID != "claude-session-123" {
		t.Fatalf("decoded output = %+v", decoded)
	}
}

func TestBotletsLogsListsRecentRunCommands(t *testing.T) {
	fixture := setupBotletsRunLogFixture(t)
	oldOpen := botletsOpenDefaultFile
	var opened []string
	botletsOpenDefaultFile = func(path string) error {
		opened = append(opened, path)
		return nil
	}
	t.Cleanup(func() {
		botletsOpenDefaultFile = oldOpen
	})

	output, err := captureRun(t, []string{
		"botlets", "logs",
		"--home", fixture.home,
		"--botlets-home", fixture.botletsHome,
		"--bot", "max.runner",
		"--limit", "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(opened) != 0 {
		t.Fatalf("list mode opened transcript: %v", opened)
	}
	for _, want := range []string{
		"bot: runner (max.runner)",
		fixture.runID,
		fixture.messageID,
		"open: comment botlets logs --bot max.runner --run " + fixture.runID,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestBotletsLogsFindsPreRenameRunByStableBotIdentity(t *testing.T) {
	fixture := setupBotletsRunLogFixture(t)
	paths, err := commentbus.ResolvePaths(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := commentbus.OpenExistingStore(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 5, 26, 15, 0, 0, 0, time.UTC)
	oldRunID := "blr_prerename0123456789abcdef"
	oldMessageID := "msg_PRERENAMEBOTLETSRUN01"
	if _, err := store.InsertCloudNotificationMessage(context.Background(), commentbus.CloudNotificationMessage{
		ID:             oldMessageID,
		Profile:        "max.old-runner",
		BotName:        "old-runner",
		BotID:          fixture.botID,
		BotAgentID:     fixture.botAgentID,
		Kind:           "botlets.task",
		From:           "@botlets.ai",
		Body:           commentbus.MessageBody{Format: "markdown", Content: "Botlets Task\nRun ID: " + oldRunID},
		Refs:           map[string]any{"run_id": oldRunID, "task_kind": "scheduled", "scheduled_for": now.Format(time.RFC3339)},
		NotificationID: "ntf_prerenamebotletsrunlog",
		CreatedAt:      now.Format(time.RFC3339Nano),
		LeaseExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano),
		Now:            now,
	}); err != nil {
		t.Fatal(err)
	}
	transcriptLine := fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"# comment.io message for max.old-runner\nBotlets Task\nRun ID: %s\nReceive: comment messages receive %s"},"sessionId":"claude-session-123","cwd":"/tmp/botlets","timestamp":"%s"}`, oldRunID, oldMessageID, now.Format(time.RFC3339Nano))
	file, err := os.OpenFile(fixture.transcriptPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(file, transcriptLine); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	output, err := captureRun(t, []string{
		"botlets", "logs",
		"--home", fixture.home,
		"--botlets-home", fixture.botletsHome,
		"--bot", "runner",
		"--run", oldRunID,
		"--claude-home", fixture.claudeHome,
		"--codex-home", fixture.codexHome,
		"--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Run struct {
			RunID      string `json:"run_id"`
			MessageID  string `json:"message_id"`
			Command    string `json:"command"`
			Transcript struct {
				Path string `json:"path"`
				Line int    `json:"line"`
			} `json:"transcript"`
		} `json:"run"`
	}
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Run.RunID != oldRunID || decoded.Run.MessageID != oldMessageID || decoded.Run.Transcript.Path != fixture.transcriptPath || decoded.Run.Transcript.Line != 4 {
		t.Fatalf("decoded output = %+v", decoded)
	}
	if decoded.Run.Command != "comment botlets logs --bot max.runner --run "+oldRunID {
		t.Fatalf("command = %q", decoded.Run.Command)
	}

	output, err = captureRun(t, []string{
		"botlets", "logs",
		"--home", fixture.home,
		"--botlets-home", fixture.botletsHome,
		"--bot", "runner",
		"--message", oldMessageID,
		"--claude-home", fixture.claudeHome,
		"--codex-home", fixture.codexHome,
		"--json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(output), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Run.RunID != oldRunID || decoded.Run.MessageID != oldMessageID {
		t.Fatalf("message lookup decoded output = %+v", decoded)
	}
}

func TestBotletsLogsFindsRunBeyondFirstMessagePage(t *testing.T) {
	fixture := setupBotletsRunLogFixture(t)
	paths, err := commentbus.ResolvePaths(fixture.home)
	if err != nil {
		t.Fatal(err)
	}
	store, err := commentbus.OpenExistingStore(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	targetCreatedAt := time.Date(2026, 5, 26, 14, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		createdAt := targetCreatedAt.Add(-time.Duration(200-i) * time.Minute)
		runID := fmt.Sprintf("blr_%032x", i+1)
		messageID := fmt.Sprintf("msg_%020d", i)
		if _, err := store.InsertCloudNotificationMessage(context.Background(), commentbus.CloudNotificationMessage{
			ID:             messageID,
			Profile:        "max.runner",
			BotName:        "runner",
			Kind:           "botlets.task",
			From:           "@botlets.ai",
			Body:           commentbus.MessageBody{Format: "markdown", Content: "Botlets Task\nRun ID: " + runID},
			Refs:           map[string]any{"run_id": runID, "task_kind": "scheduled", "scheduled_for": createdAt.Format(time.RFC3339)},
			NotificationID: fmt.Sprintf("ntf_oldbotletsrunlog%03d", i),
			CreatedAt:      createdAt.Format(time.RFC3339Nano),
			LeaseExpiresAt: createdAt.Add(time.Hour).Format(time.RFC3339Nano),
			Now:            createdAt,
		}); err != nil {
			t.Fatal(err)
		}
	}

	oldOpen := botletsOpenDefaultFile
	botletsOpenDefaultFile = func(path string) error { return nil }
	t.Cleanup(func() {
		botletsOpenDefaultFile = oldOpen
	})

	output, err := captureRun(t, []string{
		"botlets", "logs",
		"--home", fixture.home,
		"--botlets-home", fixture.botletsHome,
		"--bot", "max.runner",
		"--run", fixture.runID,
		"--claude-home", fixture.claudeHome,
		"--codex-home", fixture.codexHome,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "transcript: "+fixture.transcriptPath+":2") {
		t.Fatalf("output did not find paginated target:\n%s", output)
	}
}

func TestBotletsLogsClassifiesCodexUserTranscriptLine(t *testing.T) {
	runID := "blr_0123456789abcdef0123456789abcdef"
	messageID := "msg_ABCDEFGHIJKLMNOPQRST"
	line := []byte(fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# comment.io message\nBotlets Task\nRun ID: %s\ncomment messages receive %s"}]}}`, runID, messageID))

	candidate, sessionID, cwd := classifyBotletsTranscriptLine(line, "codex", "codex", "/tmp/codex/session.jsonl", 7, messageID, runID)
	if sessionID != "" || cwd != "" {
		t.Fatalf("metadata = %q %q, want empty", sessionID, cwd)
	}
	if candidate == nil || candidate.Runtime != "codex" || candidate.Line != 7 || candidate.Path != "/tmp/codex/session.jsonl" {
		t.Fatalf("candidate = %+v", candidate)
	}
}

type botletsRunLogFixture struct {
	home           string
	botletsHome    string
	claudeHome     string
	codexHome      string
	runID          string
	messageID      string
	botID          string
	botAgentID     string
	transcriptPath string
}

func setupBotletsRunLogFixture(t *testing.T) botletsRunLogFixture {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, ".comment-io")
	botletsHome := filepath.Join(root, "botlets")
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	profilePath, err := writeBotletsAgentProfile(paths, "max.runner", "as_ag_test_secret", "https://comment.example")
	if err != nil {
		t.Fatal(err)
	}
	botID := "ag_bot_runner"
	botAgentID := "ag_bot_runner"
	if err := upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "runner",
		BotID:             botID,
		Handle:            "max.runner",
		SlugAliases:       []string{"old-runner"},
		HandleAliases:     []string{"max.old-runner"},
		CredentialProfile: profilePath,
		BrainRef:          &commentbus.BotBrainRef{WorkspaceID: "ws_brain", OwnerAgentID: "ag_owner", BotAgentID: botAgentID, ContainerID: "lc_brain", RootFolderID: "lf_brain", RelativePath: "Botlets/max/runner/brain", SetupGeneration: 1},
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	}); err != nil {
		t.Fatal(err)
	}
	store, err := commentbus.OpenStore(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 5, 26, 14, 0, 0, 0, time.UTC)
	runID := "blr_0123456789abcdef0123456789abcdef"
	messageID := "msg_ABCDEFGHIJKLMNOPQRST"
	if _, err := store.InsertCloudNotificationMessage(context.Background(), commentbus.CloudNotificationMessage{
		ID:             messageID,
		Profile:        "max.runner",
		BotName:        "runner",
		BotID:          botID,
		BotAgentID:     botAgentID,
		Kind:           "botlets.task",
		From:           "@botlets.ai",
		Body:           commentbus.MessageBody{Format: "markdown", Content: "Botlets Task\nRun ID: " + runID},
		Refs:           map[string]any{"run_id": runID, "task_kind": "scheduled", "scheduled_for": now.Format(time.RFC3339)},
		NotificationID: "ntf_testbotletsrunlog",
		CreatedAt:      now.Format(time.RFC3339Nano),
		LeaseExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano),
		Now:            now,
	}); err != nil {
		t.Fatal(err)
	}

	claudeHome := filepath.Join(root, "claude")
	projectDir := filepath.Join(claudeHome, "projects", "-tmp-botlets")
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(projectDir, "session.jsonl")
	transcript := strings.Join([]string{
		`{"type":"permission-mode","sessionId":"claude-session-123","cwd":"/tmp/botlets"}`,
		fmt.Sprintf(`{"type":"user","message":{"role":"user","content":"# comment.io message for max.runner\nBotlets Task\nRun ID: %s\nReceive: comment messages receive %s"},"sessionId":"claude-session-123","cwd":"/tmp/botlets","timestamp":"%s"}`, runID, messageID, now.Format(time.RFC3339Nano)),
		`{"type":"assistant","message":{"role":"assistant","content":"working"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(root, "codex")
	if err := os.MkdirAll(filepath.Join(codexHome, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	return botletsRunLogFixture{
		home:           home,
		botletsHome:    botletsHome,
		claudeHome:     claudeHome,
		codexHome:      codexHome,
		runID:          runID,
		messageID:      messageID,
		botID:          botID,
		botAgentID:     botAgentID,
		transcriptPath: transcriptPath,
	}
}

func TestWriteBotletsAgentProfileValidatesHandleBeforePathJoin(t *testing.T) {
	paths, err := commentbus.ResolvePaths(filepath.Join(t.TempDir(), "comment"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeBotletsAgentProfile(paths, "max.bad/../../escape", "as_ag_test_secret", "https://comment.io"); err == nil {
		t.Fatal("writeBotletsAgentProfile accepted unsafe handle")
	}
	if _, err := os.Stat(filepath.Join(paths.Home, "escape.json")); !os.IsNotExist(err) {
		t.Fatalf("unsafe profile path was created, stat err = %v", err)
	}
}

func TestPrepareBotletsAgentProfileWithRuntimeWritesRuntime(t *testing.T) {
	paths, err := commentbus.ResolvePaths(filepath.Join(t.TempDir(), "comment"))
	if err != nil {
		t.Fatal(err)
	}
	write, err := prepareBotletsAgentProfileWithRuntime(paths, "max.runner", "as_ag_test_secret", "https://comment.io", "codex")
	if err != nil {
		t.Fatal(err)
	}
	var profile map[string]any
	if err := json.Unmarshal(write.data, &profile); err != nil {
		t.Fatal(err)
	}
	if profile["runtime"] != "codex" || write.profile.Runtime != "codex" {
		t.Fatalf("profile runtime = %#v / %q, want codex", profile["runtime"], write.profile.Runtime)
	}
}

func TestWriteBotletsAgentProfileAliasesSuppressesOldPollingProfile(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	if _, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io"); err != nil {
		t.Fatal(err)
	}
	aliasPath, err := writeBotletsAgentProfile(paths, "max.reviewer", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upsertBotletsRegistryReturningEntry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "reviewer",
		BotID:             "ag_bot",
		Handle:            "max.reviewer",
		HandleAliases:     []string{"max.research-reader"},
		CredentialProfile: aliasPath,
		BrainRef:          &commentbus.BotBrainRef{WorkspaceID: "ws_brain", OwnerAgentID: "ag_owner", BotAgentID: "ag_bot", ContainerID: "lc_brain", RootFolderID: "lf_brain", RelativePath: "Botlets/max/reviewer/brain", SetupGeneration: 1},
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := writeBotletsAgentProfileAliases(paths, botletsHome, "max.research-reader", []string{"max.reviewer"}, "ag_bot", "ag_bot"); err != nil {
		t.Fatal(err)
	}
	state, errorsOut := commentbus.LoadProfileState(context.Background(), commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: filepath.Join(t.TempDir(), "botlets"),
	})
	if len(errorsOut) != 0 {
		t.Fatalf("errors = %+v", errorsOut)
	}
	if _, ok := state.AgentProfiles["max.reviewer"]; ok {
		t.Fatalf("old alias profile still loaded for polling: %+v", state.AgentProfiles)
	}
	alias := state.ProfileAliases["max.reviewer"]
	if alias.AliasOf != "max.research-reader" || alias.BotID != "ag_bot" || !alias.DisabledForPolling {
		t.Fatalf("alias = %+v", alias)
	}
}

func TestPrepareBotletsAgentProfileAliasesRejectsUnrelatedCredentialOverwrite(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	if _, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_new_secret", "https://comment.io"); err != nil {
		t.Fatal(err)
	}
	aliasPath, err := writeBotletsAgentProfile(paths, "max.reviewer", "as_ag_other_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := prepareBotletsAgentProfileAliases(paths, botletsHome, "max.research-reader", []string{"max.reviewer"}, "ag_bot", "ag_bot"); err == nil || !strings.Contains(err.Error(), "already belongs to another bot") {
		t.Fatalf("prepare alias err = %v, want unrelated credential rejection", err)
	}
	data, readErr := os.ReadFile(aliasPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var profile map[string]any
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}
	if profile["agent_secret"] != "as_ag_other_secret" || profile["profile_kind"] == "alias" {
		t.Fatalf("alias profile was unexpectedly overwritten: %+v", profile)
	}
}

func TestRegisterBotletsBotLocallyDoesNotOverwriteAliasesBeforeProjection(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_new_secret", "https://comment.io"); err != nil {
		t.Fatal(err)
	}
	aliasPath, err := writeBotletsAgentProfile(paths, "max.reviewer", "as_ag_old_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}

	_, err = registerBotletsBotLocally(context.Background(), botletsRegisterInput{
		Paths:            paths,
		BotletsHome:      filepath.Join(root, "botlets"),
		BaseURL:          "https://comment.io",
		BotHandle:        "max.research-reader",
		AgentSecret:      "as_ag_new_secret",
		BotSlug:          "research-reader",
		BotID:            "ag_bot",
		SlugAliases:      []string{"reviewer"},
		HandleAliases:    []string{"max.reviewer"},
		OwnerAgentID:     "ag_owner",
		BotAgentID:       "ag_bot",
		WorkspaceID:      "ws_brain",
		ContainerID:      "lc_brain",
		RootFolderID:     "lf_brain",
		SetupGeneration:  1,
		ScheduleTimezone: "UTC",
		Runtime:          "claude",
	})
	if err == nil {
		t.Fatal("register unexpectedly succeeded without a local brain projection")
	}
	data, readErr := os.ReadFile(aliasPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var aliasProfile map[string]any
	if err := json.Unmarshal(data, &aliasProfile); err != nil {
		t.Fatal(err)
	}
	if aliasProfile["agent_secret"] != "as_ag_old_secret" || aliasProfile["profile_kind"] == "alias" {
		t.Fatalf("alias profile was overwritten before projection validation: %+v", aliasProfile)
	}
}

func TestRegisterBotletsBotLocallyDoesNotOverwriteCanonicalProfileBeforeRegistryValidation(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	canonicalProfilePath, err := writeBotletsAgentProfile(paths, "max.reviewer", "as_ag_old_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	otherProfilePath, err := writeBotletsAgentProfile(paths, "max.other", "as_ag_other_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	staleRegistry := map[string]any{
		"bots": []map[string]any{
			{"name": "other", "bot_id": "ag_other", "handle": "max.other", "credential_profile": otherProfilePath, "managed_session": map[string]any{"enabled": true, "runtime": "claude"}},
		},
	}
	staleData, err := json.Marshal(staleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	writeCLILocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")

	_, err = registerBotletsBotLocally(context.Background(), botletsRegisterInput{
		Paths:            paths,
		BotletsHome:      botletsHome,
		BaseURL:          "https://comment.io",
		BotHandle:        "max.reviewer",
		AgentSecret:      "as_ag_new_secret",
		BotSlug:          "reviewer",
		BotID:            "ag_bot",
		SlugAliases:      []string{"other"},
		OwnerAgentID:     "ag_owner",
		BotAgentID:       "ag_bot",
		WorkspaceID:      "ws_brain",
		ContainerID:      "lc_brain",
		RootFolderID:     "lf_brain",
		SetupGeneration:  1,
		ScheduleTimezone: "UTC",
		Runtime:          "claude",
	})
	if err == nil || !strings.Contains(err.Error(), "another bot") {
		t.Fatalf("register err = %v, want registry conflict", err)
	}
	data, readErr := os.ReadFile(canonicalProfilePath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var profile map[string]any
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}
	if profile["agent_secret"] != "as_ag_old_secret" {
		t.Fatalf("canonical profile was overwritten before registry validation: %+v", profile)
	}
}

func TestRegisterBotletsBotLocallyDoesNotWriteProfilesWhenBusConfigFails(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	writeCLILocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")
	if err := os.WriteFile(paths.Bus, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = registerBotletsBotLocally(context.Background(), botletsRegisterInput{
		Paths:            paths,
		BotletsHome:      botletsHome,
		BaseURL:          "https://comment.io",
		BotHandle:        "max.reviewer",
		AgentSecret:      "as_ag_new_secret",
		BotSlug:          "reviewer",
		BotID:            "ag_bot",
		OwnerAgentID:     "ag_owner",
		BotAgentID:       "ag_bot",
		WorkspaceID:      "ws_brain",
		ContainerID:      "lc_brain",
		RootFolderID:     "lf_brain",
		SetupGeneration:  1,
		ScheduleTimezone: "UTC",
		Runtime:          "claude",
	})
	if err == nil {
		t.Fatal("register unexpectedly succeeded after bus config write failed")
	}
	if _, statErr := os.Stat(filepath.Join(paths.Home, "agents", "max.reviewer.json")); !os.IsNotExist(statErr) {
		t.Fatalf("profile was written after bus config failure, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(botletsHome, "registry.json")); !os.IsNotExist(statErr) {
		t.Fatalf("registry was written after bus config failure, stat err = %v", statErr)
	}
}

func TestRegisterBotletsBotLocallyRollsBackBusConfigWhenRegistryFails(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	previousHome := filepath.Join(root, "botlets-old")
	botletsHome := filepath.Join(root, "botlets-new")
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: previousHome}); err != nil {
		t.Fatal(err)
	}
	if _, err := writeBotletsAgentProfile(paths, "max.reviewer", "as_ag_old_secret", "https://comment.io"); err != nil {
		t.Fatal(err)
	}
	otherProfilePath, err := writeBotletsAgentProfile(paths, "max.other", "as_ag_other_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	staleRegistry := map[string]any{
		"bots": []map[string]any{
			{"name": "other", "bot_id": "ag_other", "handle": "max.other", "credential_profile": otherProfilePath, "managed_session": map[string]any{"enabled": true, "runtime": "claude"}},
		},
	}
	staleData, err := json.Marshal(staleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	writeCLILocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")

	_, err = registerBotletsBotLocally(context.Background(), botletsRegisterInput{
		Paths:            paths,
		BotletsHome:      botletsHome,
		BaseURL:          "https://comment.io",
		BotHandle:        "max.reviewer",
		AgentSecret:      "as_ag_new_secret",
		BotSlug:          "reviewer",
		BotID:            "ag_bot",
		SlugAliases:      []string{"other"},
		OwnerAgentID:     "ag_owner",
		BotAgentID:       "ag_bot",
		WorkspaceID:      "ws_brain",
		ContainerID:      "lc_brain",
		RootFolderID:     "lf_brain",
		SetupGeneration:  1,
		ScheduleTimezone: "UTC",
		Runtime:          "claude",
	})
	if err == nil || !strings.Contains(err.Error(), "another bot") {
		t.Fatalf("register err = %v, want registry conflict", err)
	}
	config, ok, err := commentbus.ReadBusConfig(paths)
	if err != nil {
		t.Fatal(err)
	}
	resolvedPreviousHome, err := commentbus.ResolveBotletsHome(previousHome)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || config.BotletsHome != resolvedPreviousHome {
		t.Fatalf("bus config = %+v ok=%v, want previous home %q", config, ok, resolvedPreviousHome)
	}
}

func TestWriteBotletsAgentProfileRejectsSymlinkedAgentsDirBeforeWrite(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	realAgents := filepath.Join(root, "real-agents")
	if err := os.MkdirAll(realAgents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realAgents, filepath.Join(paths.Home, "agents")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := writeBotletsAgentProfile(paths, "max.reader2", "as_ag_test_secret", "https://comment.io"); err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("writeBotletsAgentProfile error = %v, want symlink rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(realAgents, "max.reader2.json")); !os.IsNotExist(statErr) {
		t.Fatalf("profile was written through symlinked agents dir, stat err = %v", statErr)
	}
}

func TestUpsertBotletsRegistryRejectsSymlinkedHomeBeforeWrite(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	profilePath, err := writeBotletsAgentProfile(paths, "max.reader2", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	realHome := filepath.Join(root, "real-botlets")
	if err := os.MkdirAll(realHome, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkHome := filepath.Join(root, "symlink-botlets")
	if err := os.Symlink(realHome, symlinkHome); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	err = upsertBotletsRegistry(paths, symlinkHome, commentbus.BotRegistryEntry{
		Name:              "reader-2",
		Handle:            "max.reader2",
		CredentialProfile: profilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("upsert error = %v, want symlink rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(realHome, "registry.json")); !os.IsNotExist(statErr) {
		t.Fatalf("registry was written through symlink, stat err = %v", statErr)
	}
}

func TestUpsertBotletsRegistryRejectsSymlinkedParentBeforeWrite(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	profilePath, err := writeBotletsAgentProfile(paths, "max.reader2", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	realParent := filepath.Join(root, "real-parent")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkParent := filepath.Join(root, "link-parent")
	if err := os.Symlink(realParent, symlinkParent); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	err = upsertBotletsRegistry(paths, filepath.Join(symlinkParent, "botlets"), commentbus.BotRegistryEntry{
		Name:              "reader-2",
		Handle:            "max.reader2",
		CredentialProfile: profilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("upsert error = %v, want parent symlink rejection", err)
	}
	if _, statErr := os.Stat(filepath.Join(realParent, "botlets", "registry.json")); !os.IsNotExist(statErr) {
		t.Fatalf("registry was written through symlinked parent, stat err = %v", statErr)
	}
}

func TestUpsertBotletsRegistryRejectsSymlinkedLockFile(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	profilePath, err := writeBotletsAgentProfile(paths, "max.reader2", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	lockTarget := filepath.Join(root, "lock-target")
	if err := os.WriteFile(lockTarget, []byte("do not touch"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(lockTarget, filepath.Join(botletsHome, ".registry.lock")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	err = upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "reader-2",
		DisplayName:       "Reader Two",
		Handle:            "max.reader2",
		CredentialProfile: profilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err == nil {
		t.Fatal("upsert accepted symlinked registry lock")
	}
	if _, statErr := os.Stat(filepath.Join(botletsHome, "registry.json")); !os.IsNotExist(statErr) {
		t.Fatalf("registry was written despite symlinked lock, stat err = %v", statErr)
	}
}

func TestUpsertBotletsRegistryCoalescesNameAndHandleMatches(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.reader2", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	staleRegistry := map[string]any{
		"bots": []map[string]any{
			{"name": "reader-2", "handle": "max.oldreader", "credential_profile": filepath.Join(root, "missing-old.json"), "managed_session": map[string]any{"enabled": true, "runtime": "claude"}},
			{"name": "legacy-reader", "handle": "max.reader2", "credential_profile": filepath.Join(root, "missing-reader.json"), "managed_session": map[string]any{"enabled": true, "runtime": "claude"}},
		},
	}
	staleData, err := json.Marshal(staleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	displayName := strings.Repeat("é", 60)
	err = upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "reader-2",
		DisplayName:       displayName,
		Handle:            "max.reader2",
		CredentialProfile: profilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(botletsHome, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bots) != 1 {
		t.Fatalf("registry bot count = %d, want 1: %+v", len(registry.Bots), registry.Bots)
	}
	if registry.Bots[0].Name != "reader-2" || registry.Bots[0].DisplayName != displayName || registry.Bots[0].Handle != "max.reader2" {
		t.Fatalf("registry bot = %+v", registry.Bots[0])
	}
}

func TestUpsertBotletsRegistryCoalescesBotIDAndAliasMatches(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	oldProfilePath, err := writeBotletsAgentProfile(paths, "max.reviewer", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	newProfilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	staleRegistry := map[string]any{
		"bots": []map[string]any{
			{"name": "reviewer", "bot_id": "ag_bot", "handle": "max.reviewer", "credential_profile": oldProfilePath, "managed_session": map[string]any{"enabled": true, "runtime": "claude"}},
		},
	}
	staleData, err := json.Marshal(staleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	err = upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		SlugAliases:       []string{"reviewer"},
		HandleAliases:     []string{"max.reviewer"},
		CredentialProfile: newProfilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(botletsHome, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bots) != 1 {
		t.Fatalf("registry bot count = %d, want 1: %+v", len(registry.Bots), registry.Bots)
	}
	if bot := registry.Bots[0]; bot.Name != "research-reader" || bot.BotID != "ag_bot" || !bot.MatchesSelector("reviewer") || !bot.MatchesProfile("max.reviewer") {
		t.Fatalf("registry bot = %+v", bot)
	}
}

func TestUpsertBotletsRegistryPreservesPreviousCanonicalLabelsAsAliases(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	oldProfilePath, err := writeBotletsAgentProfile(paths, "max.reviewer", "as_ag_old_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	newProfilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_new_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	staleRegistry := map[string]any{
		"bots": []map[string]any{
			{"name": "reviewer", "bot_id": "ag_bot", "handle": "max.reviewer", "credential_profile": oldProfilePath, "managed_session": map[string]any{"enabled": true, "runtime": "claude"}},
		},
	}
	staleData, err := json.Marshal(staleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	merged, err := upsertBotletsRegistryReturningEntry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		CredentialProfile: newProfilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !merged.MatchesSelector("reviewer") || !merged.MatchesProfile("max.reviewer") {
		t.Fatalf("merged entry lost previous canonical labels: %+v", merged)
	}
	data, err := os.ReadFile(filepath.Join(botletsHome, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bots) != 1 {
		t.Fatalf("registry bot count = %d, want 1: %+v", len(registry.Bots), registry.Bots)
	}
	if bot := registry.Bots[0]; bot.Name != "research-reader" || !bot.MatchesSelector("reviewer") || !bot.MatchesProfile("max.reviewer") {
		t.Fatalf("registry bot lost previous canonical labels: %+v", bot)
	}
}

func TestUpsertBotletsRegistryRepairsRenamedCredentialProfile(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	oldProfilePath, err := writeBotletsAgentProfile(paths, "max.reviewer", "as_ag_old_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	staleRegistry := map[string]any{
		"bots": []map[string]any{
			{"name": "reviewer", "bot_id": "bot_perm_123", "handle": "max.reviewer", "credential_profile": oldProfilePath, "managed_session": map[string]any{"enabled": true, "runtime": "claude"}},
		},
	}
	staleData, err := json.Marshal(staleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(botletsHome, "registry.json")
	if err := os.WriteFile(registryPath, append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	newProfilePath, err := commentbus.ValidateAgentProfileWriteTarget(paths, "max.research-reader")
	if err != nil {
		t.Fatal(err)
	}

	merged, err := upsertBotletsRegistryReturningEntry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "bot_perm_123",
		Handle:            "max.research-reader",
		CredentialProfile: oldProfilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if merged.CredentialProfile != newProfilePath || !merged.MatchesSelector("reviewer") || !merged.MatchesProfile("max.reviewer") {
		t.Fatalf("merged entry = %+v, want repaired canonical profile and aliases", merged)
	}
	state, errorsOut := commentbus.LoadProfileState(context.Background(), commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 0 {
		t.Fatalf("profile reload errors = %+v", errorsOut)
	}
	profile := state.AgentProfiles["max.research-reader"]
	if profile.Handle != "max.research-reader" || profile.AgentSecret != "as_ag_old_secret" || profile.Path != newProfilePath {
		t.Fatalf("canonical profile = %+v", profile)
	}
	alias := state.ProfileAliases["max.reviewer"]
	if alias.AliasOf != "max.research-reader" || alias.BotID != "bot_perm_123" || !alias.DisabledForPolling {
		t.Fatalf("alias profile = %+v", alias)
	}
	bot := state.BotRegistry["research-reader"]
	if bot.CredentialPath != newProfilePath || bot.Handle != "max.research-reader" || !bot.MatchesProfile("max.reviewer") {
		t.Fatalf("loaded bot = %+v", bot)
	}
}

func TestUpsertBotletsRegistryRejectsRenamedCredentialProfileOverwrite(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	oldProfilePath, err := writeBotletsAgentProfile(paths, "max.reviewer", "as_ag_old_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	newProfilePath, err := commentbus.ValidateAgentProfileWriteTarget(paths, "max.research-reader")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newProfilePath, []byte("{\"handle\":\"max.other\",\"agent_secret\":\"as_ag_other_secret\",\"base_url\":\"https://comment.io\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	staleRegistry := map[string]any{
		"bots": []map[string]any{
			{"name": "reviewer", "bot_id": "bot_perm_123", "handle": "max.reviewer", "credential_profile": oldProfilePath, "managed_session": map[string]any{"enabled": true, "runtime": "claude"}},
		},
	}
	staleData, err := json.Marshal(staleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(botletsHome, "registry.json")
	if err := os.WriteFile(registryPath, append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = upsertBotletsRegistryReturningEntry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "bot_perm_123",
		Handle:            "max.research-reader",
		CredentialProfile: oldProfilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err == nil || !strings.Contains(err.Error(), "already belongs to another credential") {
		t.Fatalf("upsert err = %v, want canonical overwrite rejection", err)
	}
	data, err := os.ReadFile(newProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "max.other") || !strings.Contains(string(data), "as_ag_other_secret") {
		t.Fatalf("new profile was overwritten: %s", data)
	}
	oldData, err := os.ReadFile(oldProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(oldData), "\"profile_kind\"") {
		t.Fatalf("old profile was demoted despite rejected repair: %s", oldData)
	}
	data, err = os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bots) != 1 || registry.Bots[0].Handle != "max.reviewer" || registry.Bots[0].CredentialProfile != oldProfilePath {
		t.Fatalf("registry changed after rejected repair: %+v", registry.Bots)
	}
}

func TestUpsertBotletsRegistryRejectsAliasOverlapForDifferentBotID(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	staleRegistry := map[string]any{
		"bots": []map[string]any{
			{"name": "reviewer", "bot_id": "ag_other", "handle": "max.reviewer", "credential_profile": profilePath, "managed_session": map[string]any{"enabled": true, "runtime": "claude"}},
		},
	}
	staleData, err := json.Marshal(staleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(botletsHome, "registry.json")
	if err := os.WriteFile(registryPath, append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	err = upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		SlugAliases:       []string{"reviewer"},
		HandleAliases:     []string{"max.reviewer"},
		CredentialProfile: profilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err == nil || !strings.Contains(err.Error(), "another bot") {
		t.Fatalf("upsert err = %v, want cross-bot alias conflict", err)
	}
	data, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bots) != 1 || registry.Bots[0].BotID != "ag_other" {
		t.Fatalf("registry was changed after rejected conflict: %+v", registry.Bots)
	}
}

func TestUpsertBotletsRegistryRejectsAliasOverlapWithLegacyBot(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	staleRegistry := map[string]any{
		"bots": []map[string]any{
			{"name": "reviewer", "handle": "max.reviewer", "credential_profile": profilePath, "managed_session": map[string]any{"enabled": true, "runtime": "claude"}},
		},
	}
	staleData, err := json.Marshal(staleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(botletsHome, "registry.json")
	if err := os.WriteFile(registryPath, append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	err = upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		SlugAliases:       []string{"reviewer"},
		HandleAliases:     []string{"max.reviewer"},
		CredentialProfile: profilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err == nil || !strings.Contains(err.Error(), "another bot") {
		t.Fatalf("upsert err = %v, want legacy alias conflict", err)
	}
	data, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bots) != 1 || registry.Bots[0].Name != "reviewer" {
		t.Fatalf("registry was changed after rejected legacy conflict: %+v", registry.Bots)
	}
}

func TestUpsertBotletsRegistryRejectsConflictingBotIDForSameBotAgent(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	staleRegistry := map[string]any{
		"bots": []map[string]any{
			{
				"name": "research-reader", "bot_id": "ag_other", "handle": "max.research-reader", "credential_profile": profilePath,
				"brain_ref":       map[string]any{"workspace_id": "ws_brain", "owner_agent_id": "ag_owner", "bot_agent_id": "ag_bot", "container_id": "lc_brain", "root_folder_id": "lf_brain", "relative_path": "Botlets/max/research-reader/brain", "setup_generation": 1},
				"managed_session": map[string]any{"enabled": true, "runtime": "claude"},
			},
		},
	}
	staleData, err := json.Marshal(staleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(botletsHome, "registry.json")
	if err := os.WriteFile(registryPath, append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	err = upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		CredentialProfile: profilePath,
		BrainRef:          &commentbus.BotBrainRef{WorkspaceID: "ws_brain", OwnerAgentID: "ag_owner", BotAgentID: "ag_bot", ContainerID: "lc_brain", RootFolderID: "lf_brain", RelativePath: "Botlets/max/research-reader/brain", SetupGeneration: 1},
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err == nil || !strings.Contains(err.Error(), "another bot") {
		t.Fatalf("upsert err = %v, want bot id conflict", err)
	}
	data, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bots) != 1 || registry.Bots[0].BotID != "ag_other" {
		t.Fatalf("registry was changed after rejected bot id conflict: %+v", registry.Bots)
	}
}

func TestUpsertBotletsRegistryRejectsConflictingBotAgentForSameBotID(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	staleRegistry := map[string]any{
		"bots": []map[string]any{
			{
				"name": "research-reader", "bot_id": "ag_bot", "handle": "max.research-reader", "credential_profile": profilePath,
				"brain_ref":       map[string]any{"workspace_id": "ws_brain", "owner_agent_id": "ag_owner", "bot_agent_id": "ag_bot", "container_id": "lc_brain", "root_folder_id": "lf_brain", "relative_path": "Botlets/max/research-reader/brain", "setup_generation": 1},
				"managed_session": map[string]any{"enabled": true, "runtime": "claude"},
			},
		},
	}
	staleData, err := json.Marshal(staleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(botletsHome, "registry.json")
	if err := os.WriteFile(registryPath, append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	err = upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		CredentialProfile: profilePath,
		BrainRef:          &commentbus.BotBrainRef{WorkspaceID: "ws_brain", OwnerAgentID: "ag_owner", BotAgentID: "ag_other_bot", ContainerID: "lc_brain", RootFolderID: "lf_brain", RelativePath: "Botlets/max/research-reader/brain", SetupGeneration: 1},
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err == nil || !strings.Contains(err.Error(), "another bot agent") {
		t.Fatalf("upsert err = %v, want bot agent conflict", err)
	}
	data, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bots) != 1 || registry.Bots[0].BrainRef == nil || registry.Bots[0].BrainRef.BotAgentID != "ag_bot" {
		t.Fatalf("registry was changed after rejected bot agent conflict: %+v", registry.Bots)
	}
}

func TestUpsertBotletsRegistryPreservesStableIdentityForLegacyRegister(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	staleRegistry := map[string]any{
		"bots": []map[string]any{
			{
				"name": "research-reader", "bot_id": "ag_bot", "handle": "max.research-reader", "slug_aliases": []string{"reviewer"}, "handle_aliases": []string{"max.reviewer"}, "credential_profile": profilePath,
				"brain_ref":       map[string]any{"workspace_id": "ws_brain", "owner_agent_id": "ag_owner", "bot_agent_id": "ag_bot", "container_id": "lc_brain", "root_folder_id": "lf_brain", "relative_path": "Botlets/max/research-reader/brain", "setup_generation": 1},
				"managed_session": map[string]any{"enabled": true, "runtime": "claude"},
			},
		},
	}
	staleData, err := json.Marshal(staleRegistry)
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(botletsHome, "registry.json")
	if err := os.WriteFile(registryPath, append(staleData, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	err = upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		Handle:            "max.research-reader",
		CredentialProfile: profilePath,
		BrainRef:          &commentbus.BotBrainRef{WorkspaceID: "ws_brain", OwnerAgentID: "ag_owner", BotAgentID: "ag_bot", ContainerID: "lc_brain", RootFolderID: "lf_brain", RelativePath: "Botlets/max/research-reader/brain", SetupGeneration: 1},
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bots) != 1 {
		t.Fatalf("registry bot count = %d, want 1: %+v", len(registry.Bots), registry.Bots)
	}
	bot := registry.Bots[0]
	if bot.BotID != "ag_bot" || !bot.MatchesSelector("reviewer") || !bot.MatchesProfile("max.reviewer") {
		t.Fatalf("registry bot lost stable identity or aliases: %+v", bot)
	}
}

func TestUpsertBotletsRegistryRejectsInvalidAliasBeforeWrite(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}

	err = upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		SlugAliases:       []string{"bad slug"},
		CredentialProfile: profilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid bot slug alias") {
		t.Fatalf("upsert err = %v, want invalid slug alias", err)
	}
	if _, statErr := os.Stat(filepath.Join(botletsHome, "registry.json")); !os.IsNotExist(statErr) {
		t.Fatalf("registry was written after invalid alias, stat err = %v", statErr)
	}
}

func TestUpsertBotletsRegistryRunsPreflightBeforeWrite(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	preflightErr := errors.New("alias profile preflight failed")
	_, err = upsertBotletsRegistryReturningEntryWithPreflight(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		HandleAliases:     []string{"max.reviewer"},
		CredentialProfile: profilePath,
		BrainRef:          &commentbus.BotBrainRef{WorkspaceID: "ws_brain", OwnerAgentID: "ag_owner", BotAgentID: "ag_bot", ContainerID: "lc_brain", RootFolderID: "lf_brain", RelativePath: "Botlets/max/research-reader/brain", SetupGeneration: 1},
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	}, nil, func(commentbus.BotRegistryEntry) error {
		return preflightErr
	}, nil)
	if !errors.Is(err, preflightErr) {
		t.Fatalf("upsert err = %v, want preflight error", err)
	}
	if _, statErr := os.Stat(filepath.Join(botletsHome, "registry.json")); !os.IsNotExist(statErr) {
		t.Fatalf("registry was written after preflight failure, stat err = %v", statErr)
	}
}

func TestUpsertBotletsRegistryRollsBackWhenAfterCommitFails(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	beforeCommitErr := errors.New("profile write failed")
	_, err = upsertBotletsRegistryReturningEntryWithPreflight(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		CredentialProfile: profilePath,
		BrainRef:          &commentbus.BotBrainRef{WorkspaceID: "ws_brain", OwnerAgentID: "ag_owner", BotAgentID: "ag_bot", ContainerID: "lc_brain", RootFolderID: "lf_brain", RelativePath: "Botlets/max/research-reader/brain", SetupGeneration: 1},
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	}, nil, nil, func(commentbus.BotRegistryEntry) error {
		return beforeCommitErr
	})
	if !errors.Is(err, beforeCommitErr) {
		t.Fatalf("upsert err = %v, want after-commit error", err)
	}
	if _, statErr := os.Stat(filepath.Join(botletsHome, "registry.json")); !os.IsNotExist(statErr) {
		t.Fatalf("registry was not rolled back after after-commit failure, stat err = %v", statErr)
	}
}

func TestWritePreparedBotletsAgentProfileSetRollsBackAliasOverwrite(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := prepareBotletsAgentProfile(paths, "max.research-reader", "as_ag_new_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	aliasPath, err := writeBotletsAgentProfile(paths, "max.reviewer", "as_ag_old_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	badAlias := botletsAgentProfileAliasWrite{
		path: filepath.Join(paths.Home, "missing-agents-dir", "max.bad.json"),
		data: []byte("{}\n"),
	}
	err = writePreparedBotletsAgentProfileSetRollbackable(canonical, []botletsAgentProfileAliasWrite{
		{path: aliasPath, data: []byte("{\"profile_kind\":\"alias\",\"alias_of\":\"max.research-reader\"}\n")},
		badAlias,
	})
	if err == nil {
		t.Fatal("profile write unexpectedly succeeded")
	}
	data, readErr := os.ReadFile(aliasPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var aliasProfile map[string]any
	if err := json.Unmarshal(data, &aliasProfile); err != nil {
		t.Fatal(err)
	}
	if aliasProfile["agent_secret"] != "as_ag_old_secret" || aliasProfile["profile_kind"] == "alias" {
		t.Fatalf("alias profile was not restored after failed profile set write: %+v", aliasProfile)
	}
	if _, statErr := os.Stat(canonical.path); !os.IsNotExist(statErr) {
		t.Fatalf("new canonical profile was not removed after rollback, stat err = %v", statErr)
	}
}

func TestReconcileBotletsRegistryAfterCompletionUpdatesSetupGeneration(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := upsertBotletsRegistryReturningEntry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		CredentialProfile: profilePath,
		BrainRef:          &commentbus.BotBrainRef{WorkspaceID: "ws_brain", OwnerAgentID: "ag_owner", BotAgentID: "ag_bot", ContainerID: "lc_brain", RootFolderID: "lf_brain", RelativePath: "Botlets/max/research-reader/brain", SetupGeneration: 1},
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err = reconcileBotletsRegistryAfterCompletion(paths, botletsHome, "https://comment.io", "as_ag_test_secret", entry, map[string]any{
		"metadata": map[string]any{
			"botId":         "ag_bot",
			"botSlug":       "research-reader-renamed",
			"slugAliases":   []any{"research-reader"},
			"handleAliases": []any{"max.research-reader-old"},
			"localSetup": map[string]any{
				"setupGeneration": float64(2),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "research-reader-renamed" || entry.BrainRef == nil || entry.BrainRef.SetupGeneration != 2 || !entry.MatchesSlug("research-reader") || !entry.MatchesProfile("max.research-reader-old") {
		t.Fatalf("reconciled entry = %+v", entry)
	}
}

func TestReconcileBotletsRegistryAfterCompletionUpdatesRenamedHandle(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := upsertBotletsRegistryReturningEntry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "bot_perm_123",
		Handle:            "max.research-reader",
		CredentialProfile: profilePath,
		BrainRef:          &commentbus.BotBrainRef{WorkspaceID: "ws_brain", OwnerAgentID: "ag_owner", BotAgentID: "ag_bot", ContainerID: "lc_brain", RootFolderID: "lf_brain", RelativePath: "Botlets/max/research-reader/brain", SetupGeneration: 1},
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	entry, err = reconcileBotletsRegistryAfterCompletion(paths, botletsHome, "https://comment.io", "as_ag_test_secret", entry, map[string]any{
		"bot_handle": "max.research-reader-renamed",
		"metadata": map[string]any{
			"botId":         "bot_perm_123",
			"botSlug":       "research-reader",
			"handleAliases": []any{"max.research-reader"},
			"localSetup": map[string]any{
				"setupGeneration": float64(2),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if entry.Handle != "max.research-reader-renamed" || entry.CredentialProfile == profilePath || !entry.MatchesProfile("max.research-reader") || entry.BrainRef == nil || entry.BrainRef.SetupGeneration != 2 {
		t.Fatalf("reconciled entry = %+v", entry)
	}
	state, errorsOut := commentbus.LoadProfileState(context.Background(), commentbus.ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: botletsHome,
	})
	if len(errorsOut) != 0 {
		t.Fatalf("errors = %+v", errorsOut)
	}
	profile := state.AgentProfiles["max.research-reader-renamed"]
	if profile.Handle != "max.research-reader-renamed" || profile.AgentSecret != "as_ag_test_secret" {
		t.Fatalf("profile = %+v", profile)
	}
	alias := state.ProfileAliases["max.research-reader"]
	if alias.AliasOf != "max.research-reader-renamed" || !alias.DisabledForPolling {
		t.Fatalf("alias = %+v", alias)
	}
	leaseNow := time.Date(2026, 5, 22, 13, 0, 0, 0, time.UTC)
	_, _, err = commentbus.CloudMessageFromLease(
		"msg_abcdefghijklmnopqrst",
		entry.Handle,
		entry,
		profile,
		commentbus.CloudNotificationLease{
			ClaimID:        "clm_cloudclaim1234567890",
			NotificationID: "ntf_cloudnotification1234567890",
			ClaimedAt:      "2026-05-22T13:00:00Z",
			LeaseExpiresAt: "2026-05-22T13:10:00Z",
			Notification: commentbus.CloudNotification{
				ID:         "ntf_cloudnotification1234567890",
				Type:       "botlets_task",
				FromHandle: "comment.io",
				CreatedAt:  "2026-05-22T13:00:00Z",
				BotletsTask: &commentbus.CloudBotletsTaskNotification{
					RunID:               "blr_0123456789abcdef0123456789abcdef",
					Kind:                "scheduled",
					OwnerAgentID:        "ag_owner",
					BotAgentID:          "ag_bot",
					BotSlug:             "research-reader",
					BotName:             "Research Reader",
					BotHandle:           "max.research-reader-renamed",
					ScheduledFor:        "2026-05-22T13:05:00Z",
					EnqueuedAt:          "2026-05-22T13:00:00Z",
					ScheduleVersion:     1,
					ExecutionGeneration: 1,
					SetupGeneration:     2,
					Cron:                "0 9 * * *",
					Timezone:            "UTC",
				},
			},
		},
		leaseNow,
	)
	if err != nil {
		t.Fatalf("renamed handle task validation failed: %v", err)
	}
}

func TestSelectBotletsStatusBotPrefersExactSlugOverHandleSuffix(t *testing.T) {
	state := commentbus.ProfileState{
		AgentProfiles: map[string]commentbus.AgentProfile{
			"max.status": {Handle: "max.status"},
			"max.other":  {Handle: "max.other"},
		},
		BotRegistry: map[string]commentbus.BotRegistryEntry{
			"status": {Name: "status", BotID: "ag_status", Handle: "max.status"},
			"other":  {Name: "other", BotID: "ag_other", Handle: "max.other", HandleAliases: []string{"max.status"}},
		},
	}
	entry, profile, ok, err := selectBotletsStatusBot(state, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || entry.BotID != "ag_status" || profile.Handle != "max.status" {
		t.Fatalf("selected entry=%+v profile=%+v ok=%v", entry, profile, ok)
	}
}

func TestSelectBotletsStatusBotRejectsAmbiguousHandleSuffix(t *testing.T) {
	state := commentbus.ProfileState{
		AgentProfiles: map[string]commentbus.AgentProfile{
			"max.alpha": {Handle: "max.alpha"},
			"sam.alpha": {Handle: "sam.alpha"},
		},
		BotRegistry: map[string]commentbus.BotRegistryEntry{
			"first-alpha":  {Name: "first-alpha", BotID: "ag_alpha", Handle: "max.alpha"},
			"second-alpha": {Name: "second-alpha", BotID: "ag_other_alpha", Handle: "sam.alpha"},
		},
	}
	_, _, ok, err := selectBotletsStatusBot(state, "alpha")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("select err = %v, want ambiguous", err)
	}
	if ok {
		t.Fatal("ambiguous selector returned ok")
	}
}

func TestResolveBotletsRunShortcutAcceptsAliasAndCanonicalizesRuntimeArgs(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		SlugAliases:       []string{"reviewer"},
		HandleAliases:     []string{"max.reviewer"},
		CredentialProfile: profilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: botletsHome}); err != nil {
		t.Fatal(err)
	}
	entry, err := resolveBotletsRunShortcut(paths, "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "research-reader" || entry.Handle != "max.research-reader" {
		t.Fatalf("entry = %+v", entry)
	}
	args := botletsShortcutRuntimeArgs(entry.ManagedSession.Runtime, entry.Name)
	if !slices.Contains(args, "research-reader") {
		t.Fatalf("runtime args = %+v, want canonical bot name", args)
	}
}

func TestSelectManagedDefaultBotletsRunShortcutPrefersSingleConfiguredDefault(t *testing.T) {
	entry, ok, err := selectManagedDefaultBotletsRunShortcut(map[string]commentbus.BotRegistryEntry{
		"default": {
			Name:           "default",
			BotID:          "ag_default",
			Handle:         "max.default",
			ManagedSession: commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
		},
		"other-default": {
			Name:           "other-default",
			BotID:          "ag_other_default",
			Handle:         "sam.renamed",
			SlugAliases:    []string{"default"},
			HandleAliases:  []string{"sam.default"},
			ManagedSession: commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || entry.Handle != "max.default" {
		t.Fatalf("entry=%+v ok=%v, want max.default", entry, ok)
	}
}

func TestSelectManagedDefaultBotletsRunShortcutRejectsMultipleConfiguredDefaults(t *testing.T) {
	_, ok, err := selectManagedDefaultBotletsRunShortcut(map[string]commentbus.BotRegistryEntry{
		"default": {
			Name:           "default",
			BotID:          "ag_default",
			Handle:         "max.default",
			ManagedSession: commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
		},
		"other-default": {
			Name:           "other-default",
			BotID:          "ag_other_default",
			Handle:         "sam.default",
			ManagedSession: commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want ambiguous", err)
	}
	if ok {
		t.Fatal("ambiguous default returned ok")
	}
}

func TestResolveCLIProfileUsesBotAliases(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		SlugAliases:       []string{"reviewer"},
		HandleAliases:     []string{"max.reviewer"},
		CredentialProfile: profilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := resolveCLIProfile(context.Background(), paths, "", "reviewer", botletsHome)
	if err != nil {
		t.Fatal(err)
	}
	if got != "max.research-reader" {
		t.Fatalf("profile = %q, want canonical handle", got)
	}
	got, err = resolveCLIProfile(context.Background(), paths, "max.reviewer", "reviewer", botletsHome)
	if err != nil {
		t.Fatal(err)
	}
	if got != "max.research-reader" {
		t.Fatalf("profile with alias = %q, want canonical handle", got)
	}
}

func TestResolveBotletsRunShortcutPrefersExactSlugOverHandleSuffix(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	statusProfilePath, err := writeBotletsAgentProfile(paths, "max.runner", "as_ag_status_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	otherProfilePath, err := writeBotletsAgentProfile(paths, "max.status", "as_ag_other_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "status",
		BotID:             "ag_status",
		Handle:            "max.runner",
		CredentialProfile: statusProfilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "other",
		BotID:             "ag_other",
		Handle:            "max.status",
		CredentialProfile: otherProfilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: botletsHome}); err != nil {
		t.Fatal(err)
	}
	entry, err := resolveBotletsRunShortcut(paths, "status")
	if err != nil {
		t.Fatal(err)
	}
	if entry.BotID != "ag_status" {
		t.Fatalf("entry = %+v, want exact slug bot", entry)
	}
}

func TestBotletsRegistryDisplayNameNormalizationMatchesProfileValidation(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.reader2", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	displayName := commentbus.NormalizeBotDisplayNameForRegistry("  Launch\nReader\r\n\tBot  ")
	if displayName != "Launch Reader Bot" {
		t.Fatalf("normalized display name = %q", displayName)
	}
	if unsafe := commentbus.NormalizeBotDisplayNameForRegistry("Launch\x00Reader"); unsafe != "" {
		t.Fatalf("unsafe display name = %q, want omitted", unsafe)
	}
	err = upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "reader-2",
		DisplayName:       displayName,
		Handle:            "max.reader2",
		CredentialProfile: profilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(botletsHome, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bots) != 1 || registry.Bots[0].DisplayName != displayName {
		t.Fatalf("registry bots = %+v, want normalized display name %q", registry.Bots, displayName)
	}
}

func TestUpsertBotletsRegistryToleratesNonFatalAgentProfileErrors(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.reader2", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	// A companion sidecar file whose stem is not a valid profile handle (two
	// dots) sits in the agents dir. It yields a non-fatal INVALID_AGENT_PROFILE
	// reload error that must not abort registering an otherwise-valid bot.
	sidecar := filepath.Join(paths.Home, "agents", "max.reader2.slack.json")
	if err := os.WriteFile(sidecar, []byte(`{"channel":"#x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err = upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "reader-2",
		Handle:            "max.reader2",
		CredentialProfile: profilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	})
	if err != nil {
		t.Fatalf("upsert failed on non-fatal sidecar agent file: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(botletsHome, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bots) != 1 || registry.Bots[0].Handle != "max.reader2" {
		t.Fatalf("registry bots = %+v, want single max.reader2", registry.Bots)
	}
}

func TestUpsertBotletsRegistrySerializesConcurrentWriters(t *testing.T) {
	root := t.TempDir()
	paths, err := commentbus.ResolvePaths(filepath.Join(root, "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(root, "botlets")
	const botCount = 12
	var wg sync.WaitGroup
	errs := make(chan error, botCount)
	for i := 0; i < botCount; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := "reader-" + string(rune('a'+i))
			handle := "max.reader-" + string(rune('a'+i))
			profilePath, err := writeBotletsAgentProfile(paths, handle, "as_ag_test_secret", "https://comment.io")
			if err != nil {
				errs <- err
				return
			}
			errs <- upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
				Name:              name,
				Handle:            handle,
				CredentialProfile: profilePath,
				ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(botletsHome, "registry.json"))
	if err != nil {
		t.Fatal(err)
	}
	var registry struct {
		Bots []commentbus.BotRegistryEntry `json:"bots"`
	}
	if err := json.Unmarshal(data, &registry); err != nil {
		t.Fatal(err)
	}
	if len(registry.Bots) != botCount {
		t.Fatalf("registry bot count = %d, want %d: %+v", len(registry.Bots), botCount, registry.Bots)
	}
}

func TestReloadBotletsProfilesReturnsResultErrors(t *testing.T) {
	home, botletsHome, stop := startCLITestDaemon(t)
	defer stop()
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), []byte("{not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, reloadErr := reloadBotletsProfiles(context.Background(), paths, botletsHome)
	if reloadErr == "" {
		t.Fatal("reloadBotletsProfiles did not surface result errors")
	}
	if !strings.Contains(reloadErr, "INVALID_BOTLETS_REGISTRY") {
		t.Fatalf("reload error = %q", reloadErr)
	}
}

func TestBotletsSetupFailureTelemetryIncludesAccountAndShape(t *testing.T) {
	oldEmitter := emitBotletsSetupTelemetryForSetup
	var events []struct {
		level string
		msg   string
		data  map[string]any
	}
	emitBotletsSetupTelemetryForSetup = func(_ context.Context, _ *http.Client, _ string, level string, msg string, data map[string]any) {
		copied := map[string]any{}
		for key, value := range data {
			copied[key] = value
		}
		events = append(events, struct {
			level string
			msg   string
			data  map[string]any
		}{level: level, msg: msg, data: copied})
	}
	t.Cleanup(func() { emitBotletsSetupTelemetryForSetup = oldEmitter })

	home := privateTempHome(t, "comment-cli-botlets-setup-telemetry-")
	output, err := captureRun(t, []string{
		"botlets", "setup",
		"--home", home,
		"--bot", "max.error-debugger",
		"--runtime", "codex",
		"--setup-attempt-id", "bla_1234567890abc",
		"--base-url", "https://comment.io",
	})
	if err == nil {
		t.Fatalf("expected setup failure, output=%s", output)
	}

	var failure map[string]any
	for _, event := range events {
		if event.level == "error" && event.msg == "botlets_cli_setup_failed" {
			failure = event.data
			break
		}
	}
	if failure == nil {
		t.Fatalf("missing setup failure telemetry: %#v", events)
	}
	if failure["owner_handle"] != "max" || failure["bot_slug"] != "error-debugger" || failure["runtime"] != "codex" || failure["setup_attempt_id"] != "bla_1234567890abc" {
		t.Fatalf("failure identity fields = %#v", failure)
	}
	if failure["failure_code"] != "library_sync_not_configured" || failure["outcome"] != "failed" {
		t.Fatalf("failure shape fields = %#v", failure)
	}
	if summary, _ := failure["reason_summary"].(string); !strings.Contains(summary, "sign in so this computer can read") {
		t.Fatalf("reason summary = %q", summary)
	}
	if hash, _ := failure["reason_hash"].(string); len(hash) != 24 {
		t.Fatalf("reason hash = %q", hash)
	}
}

func TestBotletsSetupDoesNotReportHelpAsFailure(t *testing.T) {
	oldEmitter := emitBotletsSetupTelemetryForSetup
	var events int
	emitBotletsSetupTelemetryForSetup = func(_ context.Context, _ *http.Client, _ string, _ string, _ string, _ map[string]any) {
		events++
	}
	t.Cleanup(func() { emitBotletsSetupTelemetryForSetup = oldEmitter })

	output, err := captureRun(t, []string{"botlets", "setup", "--help"})
	if err == nil {
		t.Fatalf("expected flag help error, output=%s", output)
	}
	if events != 0 {
		t.Fatalf("help emitted %d telemetry event(s)", events)
	}
}

func TestBotletsSetupTelemetryReasonSummaryRedactsSensitiveDetails(t *testing.T) {
	err := errors.New("failed at /home/user/Secret Project/setup.log with as_ag_testsecret and usk_v2.agent.key.supersecret from https://comment.io/d/demo?token=secret for max@example.com bla_1234567890abc")
	summary := botletsTelemetryReasonSummary(err)
	for _, forbidden := range []string{"/home/user", "Secret", "Project", "setup.log", "as_ag_testsecret", "supersecret", "https://comment.io", "max@example.com", "bla_1234567890abc"} {
		if strings.Contains(summary, forbidden) {
			t.Fatalf("summary leaked %q: %s", forbidden, summary)
		}
	}
	for _, want := range []string{"[path]", "as_[redacted]", "usk_v2.[redacted]", "[url]", "[email]", "bla_[redacted]"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q: %s", want, summary)
		}
	}
	if hash := botletsTelemetryReasonHash(err); len(hash) != 24 {
		t.Fatalf("reason hash = %q", hash)
	}
}

func TestBotletsSetupWarningTelemetryUsesWarnLevel(t *testing.T) {
	oldEmitter := emitBotletsSetupTelemetryForSetup
	var level string
	var msg string
	var data map[string]any
	emitBotletsSetupTelemetryForSetup = func(_ context.Context, _ *http.Client, _ string, eventLevel string, eventMsg string, eventData map[string]any) {
		level = eventLevel
		msg = eventMsg
		data = eventData
	}
	t.Cleanup(func() { emitBotletsSetupTelemetryForSetup = oldEmitter })

	emitBotletsSetupWarningTelemetry(context.Background(), &http.Client{}, botletsSetupTelemetryState{
		BaseURL:        "https://comment.io",
		OwnerHandle:    "max",
		BotSlug:        "reviewer",
		BotHandle:      "max.reviewer",
		Runtime:        "claude",
		SetupAttemptID: "bla_1234567890abc",
	}, "daemon_orientation_warning", "daemon did not advertise botlets_setup_orientation")

	if level != "warn" || msg != "botlets_cli_setup_warning" {
		t.Fatalf("warning event = %q/%q data=%#v", level, msg, data)
	}
	if data["warning_code"] != "daemon_orientation_warning" || data["owner_handle"] != "max" || data["bot_handle"] != "max.reviewer" {
		t.Fatalf("warning data = %#v", data)
	}
}

func TestBotletsSetupReloadFailureHandling(t *testing.T) {
	err := botletsSetupReloadFailure("after reconciling local bot identity", "INVALID_BOTLETS_REGISTRY")
	if err == nil || !strings.Contains(err.Error(), "after reconciling local bot identity") || !strings.Contains(err.Error(), "INVALID_BOTLETS_REGISTRY") {
		t.Fatalf("reload failure err = %v", err)
	}
	if err := botletsSetupReloadFailure("after reconciling local bot identity", ""); err != nil {
		t.Fatalf("empty reload error should not fail: %v", err)
	}

	oldEmitter := emitBotletsSetupTelemetryForSetup
	var level string
	var msg string
	var data map[string]any
	emitBotletsSetupTelemetryForSetup = func(_ context.Context, _ *http.Client, _ string, eventLevel string, eventMsg string, eventData map[string]any) {
		level = eventLevel
		msg = eventMsg
		data = eventData
	}
	t.Cleanup(func() { emitBotletsSetupTelemetryForSetup = oldEmitter })

	err = handleBotletsSetupReloadFailure(context.Background(), &http.Client{}, botletsSetupTelemetryState{
		BaseURL:        "https://comment.io",
		OwnerHandle:    "jon-gordner",
		BotSlug:        "growth-hacker",
		BotHandle:      "jon-gordner.growth-hacker",
		Runtime:        "claude",
		SetupAttemptID: "bla_1234567890abc",
	}, "after writing local files", "owner-only command is not allowed from a managed session")
	if err != nil {
		t.Fatalf("managed-session reload should be a warning, got %v", err)
	}
	if level != "warn" || msg != "botlets_cli_setup_warning" {
		t.Fatalf("warning event = %q/%q data=%#v", level, msg, data)
	}
	if data["warning_code"] != "daemon_reload_managed_session" || data["outcome"] != "warning" {
		t.Fatalf("warning data = %#v", data)
	}
	if summary, _ := data["reason_summary"].(string); !strings.Contains(summary, "Botlets daemon profile reload failed after writing local files") {
		t.Fatalf("warning summary = %q", summary)
	}

	if err := handleBotletsSetupReloadFailure(context.Background(), &http.Client{}, botletsSetupTelemetryState{
		BaseURL: "https://comment.io",
	}, "after reconciling local bot identity", "INVALID_BOTLETS_REGISTRY"); err == nil {
		t.Fatal("non-managed reload failures should remain fatal")
	}
}

func TestReloadBotletsProfilesAuthUsesManagedSessionCapability(t *testing.T) {
	home := privateTempHome(t, "comment-cli-reload-session-")
	paths := mustResolveCLIPaths(t, home)
	if _, err := commentbus.EnsureOwnerCapability(paths); err != nil {
		t.Fatal(err)
	}

	ownerAuth, err := reloadBotletsProfilesAuth(paths)
	if err != nil {
		t.Fatal(err)
	}
	if ownerAuth.Mode != "owner" {
		t.Fatalf("auth mode without managed session = %q, want owner", ownerAuth.Mode)
	}

	record := installManagedSessionEnv(t, paths, "max.reviewer", "reviewer")
	sessionAuth, err := reloadBotletsProfilesAuth(paths)
	if err != nil {
		t.Fatal(err)
	}
	if sessionAuth.Mode != "session" {
		t.Fatalf("auth mode in managed session = %q, want session", sessionAuth.Mode)
	}
	if sessionAuth.Profile == nil || *sessionAuth.Profile != record.Profile {
		t.Fatalf("session auth profile = %#v, want %q", sessionAuth.Profile, record.Profile)
	}
	if sessionAuth.SessionID == nil || *sessionAuth.SessionID != record.SessionID {
		t.Fatalf("session auth id = %#v, want %q", sessionAuth.SessionID, record.SessionID)
	}
	if sessionAuth.SessionGeneration == nil || *sessionAuth.SessionGeneration != record.Generation {
		t.Fatalf("session auth generation = %#v, want %q", sessionAuth.SessionGeneration, record.Generation)
	}
}

func TestFormatBotletsSetupHumanOutputDoesNotAskOwnerToPasteOrientation(t *testing.T) {
	output := formatBotletsSetupHumanOutput(botletsSetupHumanOutput{
		BotName:          "staging-test",
		BotHandle:        "max.staging-test",
		Runtime:          "claude",
		ProfilePath:      "/tmp/comment/agents/max.staging-test.json",
		RegistryPath:     "/tmp/botlets/registry.json",
		BrainPath:        "/tmp/Comment Docs/Botlets/max/staging-test/brain",
		SetupOrientation: "# Botlets Setup Orientation\n\nDo setup work.\n",
		SetupAttemptID:   "bla_1234567890abc",
	})
	if strings.Contains(output, "Paste this orientation") || strings.Contains(output, "# Botlets Setup Orientation") {
		t.Fatalf("human setup output should not print a manual paste block:\n%s", output)
	}
	if !strings.Contains(output, "Connected bot: staging-test (max.staging-test)") {
		t.Fatalf("human setup output should describe this computer as connected:\n%s", output)
	}
	if !strings.Contains(output, "startup note: queued for automatic delivery") {
		t.Fatalf("human setup output should explain automatic delivery:\n%s", output)
	}
	if strings.Contains(output, "keep that terminal open") {
		t.Fatalf("human setup output should not tell users to keep a terminal open:\n%s", output)
	}
	if !strings.Contains(output, "start: comment run staging-test") {
		t.Fatalf("human setup output should include the runtime start command:\n%s", output)
	}
}

func TestFormatBotletsSetupHumanOutputKeepsAutomaticOrientationWhenBootstrapUnknown(t *testing.T) {
	output := formatBotletsSetupHumanOutput(botletsSetupHumanOutput{
		BotName:             "staging-test",
		BotHandle:           "max.staging-test",
		Runtime:             "claude",
		ProfilePath:         "/tmp/comment/agents/max.staging-test.json",
		RegistryPath:        "/tmp/botlets/registry.json",
		BrainPath:           "/tmp/Comment Docs/Botlets/max/staging-test/brain",
		BootstrapProbeError: "brain projection path must exist",
		SetupOrientation:    "# Botlets Setup Orientation\n\nDo setup work.\n",
	})
	if strings.Contains(output, "setup_orientation: skipped") {
		t.Fatalf("human setup output should not skip automatic orientation when bootstrap is unknown:\n%s", output)
	}
	if !strings.Contains(output, "bootstrap check: brain projection path must exist") {
		t.Fatalf("human setup output should report bootstrap probe error:\n%s", output)
	}
	if !strings.Contains(output, "startup note: queued for automatic delivery") {
		t.Fatalf("human setup output should keep automatic delivery:\n%s", output)
	}
	if !strings.Contains(output, "start: comment run staging-test") {
		t.Fatalf("human setup output should include the runtime start command:\n%s", output)
	}
}

func TestFormatBotletsSetupHumanOutputWarnsWhenAutomaticOrientationUnsupported(t *testing.T) {
	output := formatBotletsSetupHumanOutput(botletsSetupHumanOutput{
		BotName:                 "staging-test",
		BotHandle:               "max.staging-test",
		Runtime:                 "claude",
		ProfilePath:             "/tmp/comment/agents/max.staging-test.json",
		RegistryPath:            "/tmp/botlets/registry.json",
		BrainPath:               "/tmp/Comment Docs/Botlets/max/staging-test/brain",
		SetupOrientationWarning: "automatic tmux setup orientation is not confirmed",
		SetupOrientation:        "# Botlets Setup Orientation\n\nDo setup work.\n",
	})
	if strings.Contains(output, "startup note: queued for automatic delivery") {
		t.Fatalf("human setup output should not promise automatic delivery when daemon support is unconfirmed:\n%s", output)
	}
	if !strings.Contains(output, "startup note: automatic tmux setup orientation is not confirmed") {
		t.Fatalf("human setup output should report daemon orientation warning:\n%s", output)
	}
	if !strings.Contains(output, "start: comment run staging-test") {
		t.Fatalf("human setup output should still include the runtime start command:\n%s", output)
	}
}

func TestBotletsDaemonSupportsSetupOrientation(t *testing.T) {
	if !botletsDaemonSupportsSetupOrientation(map[string]any{
		"features": map[string]any{
			commentbus.FeatureBotletsSetupOrientation: commentbus.FeatureBotletsSetupOrientationVersion,
		},
	}) {
		t.Fatal("expected feature string to be supported")
	}
	if !botletsDaemonSupportsSetupOrientation(map[string]any{
		"features": map[string]any{
			commentbus.FeatureBotletsSetupOrientation: true,
		},
	}) {
		t.Fatal("expected feature bool to be supported")
	}
	for _, health := range []any{
		nil,
		map[string]any{},
		map[string]any{"features": map[string]any{}},
		map[string]any{"features": map[string]any{commentbus.FeatureBotletsSetupOrientation: false}},
		map[string]any{"features": map[string]any{commentbus.FeatureBotletsSetupOrientation: ""}},
	} {
		if botletsDaemonSupportsSetupOrientation(health) {
			t.Fatalf("health should not support orientation: %#v", health)
		}
	}
}

func TestEnsureBotletsSetupOrientationDaemonRefreshesStaleDaemon(t *testing.T) {
	oldHealth := botletsSetupDaemonHealth
	oldInstall := botletsSetupBusInstall
	t.Cleanup(func() {
		botletsSetupDaemonHealth = oldHealth
		botletsSetupBusInstall = oldInstall
	})
	paths, err := commentbus.ResolvePaths(filepath.Join(t.TempDir(), "comment"))
	if err != nil {
		t.Fatal(err)
	}
	healthCalls := 0
	botletsSetupDaemonHealth = func(context.Context, commentbus.Paths) (bool, any, string) {
		healthCalls++
		if healthCalls == 1 {
			return true, map[string]any{"features": map[string]any{}}, ""
		}
		return true, map[string]any{
			"features": map[string]any{
				commentbus.FeatureBotletsSetupOrientation: commentbus.FeatureBotletsSetupOrientationVersion,
			},
		}, ""
	}
	installCalled := false
	botletsSetupBusInstall = func(home string, botletsHome string, bin string, dryRun bool, pair bool) (map[string]any, error) {
		installCalled = true
		if home != paths.Home || botletsHome == "" || bin != "" || dryRun {
			t.Fatalf("bus install args = home %q botletsHome %q bin %q dryRun %v", home, botletsHome, bin, dryRun)
		}
		if pair {
			t.Fatal("orientation daemon refresh must not enter the chained pair flow (it can run from background workers)")
		}
		return map[string]any{"installed": true}, nil
	}
	status, err := ensureBotletsSetupOrientationDaemon(context.Background(), paths, filepath.Join(t.TempDir(), "botlets"))
	if err != nil {
		t.Fatal(err)
	}
	if !installCalled || !status.Refreshed || !status.Supported {
		t.Fatalf("status = %+v installCalled=%v", status, installCalled)
	}
	if status.InstallResult["installed"] != true {
		t.Fatalf("install result = %#v", status.InstallResult)
	}
}

func TestEnsureBotletsSetupOrientationDaemonWarnsWhenRefreshFails(t *testing.T) {
	oldHealth := botletsSetupDaemonHealth
	oldInstall := botletsSetupBusInstall
	t.Cleanup(func() {
		botletsSetupDaemonHealth = oldHealth
		botletsSetupBusInstall = oldInstall
	})
	paths, err := commentbus.ResolvePaths(filepath.Join(t.TempDir(), "comment"))
	if err != nil {
		t.Fatal(err)
	}
	botletsSetupDaemonHealth = func(context.Context, commentbus.Paths) (bool, any, string) {
		return true, map[string]any{"features": map[string]any{}}, ""
	}
	botletsSetupBusInstall = func(string, string, string, bool, bool) (map[string]any, error) {
		return nil, errors.New("launchd denied bootstrap")
	}
	status, err := ensureBotletsSetupOrientationDaemon(context.Background(), paths, filepath.Join(t.TempDir(), "botlets"))
	if err != nil {
		t.Fatalf("stale daemon refresh should be non-fatal: %v", err)
	}
	if status.Supported {
		t.Fatalf("status should not report daemon support after refresh failure: %+v", status)
	}
	if status.Refreshed {
		t.Fatalf("status should not report a successful daemon refresh after install failure: %+v", status)
	}
	if !strings.Contains(status.Warning, "automatic daemon refresh failed") || !strings.Contains(status.Warning, "comment bus install") || !strings.Contains(status.Warning, "launchd denied bootstrap") {
		t.Fatalf("warning = %q", status.Warning)
	}
}

func TestParseBotletsBotSelectorRequiresOwner(t *testing.T) {
	owner, bot, err := parseBotletsBotSelector("", "max.reader2")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "max" || bot != "reader2" {
		t.Fatalf("selector = %q %q", owner, bot)
	}
	owner, bot, err = parseBotletsBotSelector("max", "reader2")
	if err != nil {
		t.Fatal(err)
	}
	if owner != "max" || bot != "reader2" {
		t.Fatalf("selector with owner = %q %q", owner, bot)
	}
	if _, _, err := parseBotletsBotSelector("", "reader2"); err == nil || !strings.Contains(err.Error(), "owner handle is required") {
		t.Fatalf("bare selector err = %v", err)
	}
}

func TestExpectedBotletsBrainRelativePathBindsOwnerAndSlug(t *testing.T) {
	path, err := expectedBotletsBrainRelativePath("max.reader2", "reader2")
	if err != nil {
		t.Fatal(err)
	}
	if path != "Botlets/max/reader2/brain" {
		t.Fatalf("path = %q", path)
	}
	if _, err := expectedBotletsBrainRelativePath("reader2", "reader2"); err == nil {
		t.Fatal("bare handle accepted")
	}
}

func TestRunBotletsRegisterValidatesFlags(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"missing handle", []string{"--bot-slug", "reader"}, "requires --handle"},
		{"missing bot-slug", []string{"--handle", "max.reader"}, "requires --bot-slug"},
		{"invalid runtime", []string{"--handle", "max.reader", "--bot-slug", "reader", "--runtime", "gpt"}, "invalid Botlets runtime"},
		{"setup generation too low", []string{"--handle", "max.reader", "--bot-slug", "reader", "--setup-generation", "0"}, "--setup-generation >= 1"},
		{"missing brain ids", []string{"--handle", "max.reader", "--bot-slug", "reader", "--setup-generation", "1"}, "requires --bot-agent-id"},
		{
			"missing secret",
			[]string{
				"--handle", "max.reader", "--bot-slug", "reader", "--setup-generation", "1",
				"--bot-agent-id", "ag_bot", "--owner-agent-id", "ag_owner",
				"--workspace-id", "ws_1", "--container-id", "lc_bs_1", "--root-folder-id", "lf_bs_1",
			},
			"requires an agent secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runBotletsRegister(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("runBotletsRegister(%v) error = %v, want containing %q", tc.args, err, tc.wantErr)
			}
		})
	}
}

func TestBotletsTeamSetupRequiresFlags(t *testing.T) {
	// Missing --workspace-id / --code fails before touching the filesystem.
	if _, err := captureRun(t, []string{"botlets", "team-setup"}); err == nil ||
		!strings.Contains(err.Error(), "requires --workspace-id and --code") {
		t.Fatalf("expected workspace-id/code required error, got %v", err)
	}
	// An invalid runtime is rejected.
	if _, err := captureRun(t, []string{"botlets", "team-setup", "--workspace-id", "bw_x", "--code", "sc_x", "--runtime", "bogus"}); err == nil ||
		!strings.Contains(err.Error(), "invalid Botlets runtime") {
		t.Fatalf("expected invalid runtime error, got %v", err)
	}
	// A missing setup token is rejected.
	if _, err := captureRun(t, []string{"botlets", "team-setup", "--workspace-id", "bw_x", "--code", "sc_x"}); err == nil ||
		!strings.Contains(err.Error(), "requires a setup token") {
		t.Fatalf("expected token required error, got %v", err)
	}
}

func TestPostBotletsJSONAcceptAllowsIdempotentStatus(t *testing.T) {
	// The team-runtime redeem returns 201 on the first call but 200 (rotated
	// secret) on an idempotent retry after a lost response. Both must succeed.
	for _, status := range []int{http.StatusCreated, http.StatusOK} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"runnerId":"br_x","runnerSecret":"brs_y"}`))
		}))
		var out struct {
			RunnerID     string `json:"runnerId"`
			RunnerSecret string `json:"runnerSecret"`
		}
		err := postBotletsJSONAccept(context.Background(), srv.Client(), srv.URL, map[string]any{}, &out, http.StatusCreated, http.StatusOK)
		srv.Close()
		if err != nil {
			t.Fatalf("status %d: unexpected error %v", status, err)
		}
		if out.RunnerID != "br_x" || out.RunnerSecret != "brs_y" {
			t.Fatalf("status %d: decoded = %+v", status, out)
		}
	}

	// A non-accepted status still fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer srv.Close()
	if err := postBotletsJSONAccept(context.Background(), srv.Client(), srv.URL, map[string]any{}, nil, http.StatusCreated, http.StatusOK); err == nil {
		t.Fatal("expected error for non-accepted status")
	}
}
