//go:build darwin || linux

package commentbus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBotletsBootstrapPresent(t *testing.T) {
	dir := t.TempDir()
	present, err := BotletsBootstrapPresent(dir)
	if err != nil {
		t.Fatalf("BotletsBootstrapPresent returned error: %v", err)
	}
	if present {
		t.Fatalf("expected BOOTSTRAP.md absent in fresh dir")
	}
	if err := os.WriteFile(filepath.Join(dir, BotletsBootstrapFileName), []byte("hello"), 0o600); err != nil {
		t.Fatalf("write BOOTSTRAP.md: %v", err)
	}
	present, err = BotletsBootstrapPresent(dir)
	if err != nil {
		t.Fatalf("BotletsBootstrapPresent returned error: %v", err)
	}
	if !present {
		t.Fatalf("expected BOOTSTRAP.md present after write")
	}
	dirAsFile := filepath.Join(dir, "subdir")
	if err := os.Mkdir(filepath.Join(dirAsFile), 0o700); err != nil {
		t.Fatalf("mkdir subdir: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dirAsFile, BotletsBootstrapFileName), 0o700); err != nil {
		t.Fatalf("mkdir BOOTSTRAP.md dir: %v", err)
	}
	present, err = BotletsBootstrapPresent(dirAsFile)
	if err != nil {
		t.Fatalf("BotletsBootstrapPresent returned error: %v", err)
	}
	if present {
		t.Fatalf("non-regular BOOTSTRAP.md should not count as present")
	}
	symlinkDir := filepath.Join(dir, "symlink-brain")
	if err := os.Mkdir(symlinkDir, 0o700); err != nil {
		t.Fatalf("mkdir symlink-brain: %v", err)
	}
	target := filepath.Join(dir, "target.md")
	if err := os.WriteFile(target, []byte("anything"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(symlinkDir, BotletsBootstrapFileName)); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	present, err = BotletsBootstrapPresent(symlinkDir)
	if err != nil {
		t.Fatalf("BotletsBootstrapPresent returned error for symlink: %v", err)
	}
	if present {
		t.Fatalf("symlinked BOOTSTRAP.md must not count as present")
	}
}

func TestExpandBotletsTaskMessageBodySwapsBody(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	brainRoot := writeLocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")
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
	message := CloudNotificationMessage{
		Kind: "botlets.task",
		Body: MessageBody{Format: "markdown", Content: "Scheduled Botlets run for reviewer (@max.reviewer).\n\nRun ID: blr_abc. Scheduled window: 2026-05-22T09:00:00Z.\n\nRead the bot brain..."},
	}
	notification := CloudNotification{
		Type: "botlets_task",
		BotletsTask: &CloudBotletsTaskNotification{
			RunID:               "blr_abc",
			Kind:                "scheduled",
			OwnerAgentID:        "ag_owner",
			BotAgentID:          "ag_bot",
			BotSlug:             "reviewer",
			BotName:             "reviewer",
			BotHandle:           "max.reviewer",
			ScheduledFor:        "2026-05-22T09:00:00Z",
			EnqueuedAt:          "2026-05-22T08:59:00Z",
			ScheduleVersion:     1,
			ExecutionGeneration: 1,
			SetupGeneration:     5,
			Cron:                "0 9 * * 1-5",
			Timezone:            "America/Los_Angeles",
		},
	}
	out, err := ExpandBotletsTaskMessageBody(paths, bot, message, notification)
	if err != nil {
		t.Fatalf("ExpandBotletsTaskMessageBody returned error: %v", err)
	}
	_ = brainRoot
	requireSubstrings(t, out.Body.Content, []string{
		"# Botlets Task",
		"Botlets/max/reviewer/brain`",
		"`AGENTS.md` - workspace rules and operating guidance",
		"`HEARTBEAT.md` - recurring heartbeat and cron task instructions",
		"Schedule: 0 9 * * 1-5 (America/Los_Angeles)",
	})
	if strings.Index(out.Body.Content, "`HEARTBEAT.md`") < strings.Index(out.Body.Content, "`USER.md`") {
		t.Fatalf("expanded task body should list HEARTBEAT.md after USER.md: %q", out.Body.Content)
	}
	if strings.Contains(out.Body.Content, "follow `BOOTSTRAP.md`") || strings.Contains(out.Body.Content, "read and follow `BOOTSTRAP.md`") {
		t.Fatalf("scheduled task body must not instruct to follow BOOTSTRAP.md: %q", out.Body.Content)
	}
	if out.Body.Format != "markdown" {
		t.Fatalf("expanded body format = %q, want markdown", out.Body.Format)
	}
}

func TestExpandBotletsTaskMessageBodyRejectsControlChars(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	writeLocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")
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
	original := CloudNotificationMessage{
		Kind: "botlets.task",
		Body: MessageBody{Format: "markdown", Content: "fallback body"},
	}
	notification := CloudNotification{
		Type: "botlets_task",
		BotletsTask: &CloudBotletsTaskNotification{
			RunID:               "blr_abc",
			Kind:                "scheduled",
			OwnerAgentID:        "ag_owner",
			BotAgentID:          "ag_bot",
			BotSlug:             "reviewer",
			BotName:             "reviewer",
			BotHandle:           "max.reviewer",
			ScheduledFor:        "2026-05-22T09:00:00Z",
			EnqueuedAt:          "2026-05-22T08:59:00Z",
			ScheduleVersion:     1,
			ExecutionGeneration: 1,
			SetupGeneration:     5,
			Cron:                "0 9 * * 1-5\nrm -rf /",
			Timezone:            "America/Los_Angeles",
		},
	}
	out, err := ExpandBotletsTaskMessageBody(paths, bot, original, notification)
	if err == nil {
		t.Fatalf("expected error for control chars in cron")
	}
	if out.Body.Content != "fallback body" {
		t.Fatalf("body should be unchanged on validation failure, got %q", out.Body.Content)
	}
}
