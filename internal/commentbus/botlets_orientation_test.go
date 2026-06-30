package commentbus

import (
	"strings"
	"testing"
)

func TestBuildBotletsTaskOrientationScheduled(t *testing.T) {
	body, err := BuildBotletsTaskOrientation(BotletsTaskOrientationInput{
		Kind:         "scheduled",
		BotName:      "pmf-tracker",
		BotHandle:    "max.pmf-tracker",
		RunID:        "blr_abc",
		ScheduledFor: "2026-05-22T09:00:00Z",
		Cron:         "0 9 * * 1-5",
		Timezone:     "America/Los_Angeles",
		BrainRoot:    "/home/user/CommentSync/Botlets/max/pmf-tracker/brain",
	})
	if err != nil {
		t.Fatalf("BuildBotletsTaskOrientation returned error: %v", err)
	}
	requireSubstrings(t, body, []string{
		"Scheduled Botlets run for pmf-tracker (@max.pmf-tracker).",
		"Run ID: blr_abc",
		"Scheduled window: 2026-05-22T09:00:00Z",
		"Schedule: 0 9 * * 1-5 (America/Los_Angeles)",
		"Brain root: `/home/user/CommentSync/Botlets/max/pmf-tracker/brain`",
		"`AGENTS.md` - workspace rules and operating guidance",
		"`TOOLS.md` - local tool/setup notes",
		"`SOUL.md` - persona, tone, and behavioral guidance",
		"`IDENTITY.md` - bot identity",
		"`USER.md` - owner/user profile",
		"`HEARTBEAT.md` - recurring heartbeat and cron task instructions",
		"Do not run `BOOTSTRAP.md` as part of a scheduled run.",
		"Do not load `MEMORY.md` by default",
	})
	if strings.Index(body, "`HEARTBEAT.md`") < strings.Index(body, "`USER.md`") {
		t.Fatalf("task orientation should list HEARTBEAT.md after USER.md: %q", body)
	}
	// Scheduled/cron runs are isolated from session history by design: they must
	// NOT pull the today/yesterday daily notes that the interactive main session
	// reads. Guard the boundary so the daily-note read doesn't leak into cron.
	if strings.Contains(body, "daily notes") {
		t.Fatalf("scheduled task orientation must not read daily notes (cron is isolated): %q", body)
	}
}

func TestBuildBotletsTaskOrientationManualLabel(t *testing.T) {
	body, err := BuildBotletsTaskOrientation(BotletsTaskOrientationInput{
		Kind:         "manual",
		BotName:      "pmf-tracker",
		BotHandle:    "max.pmf-tracker",
		RunID:        "blr_abc",
		ScheduledFor: "2026-05-22T09:00:00Z",
		Cron:         "0 9 * * 1-5",
		Timezone:     "UTC",
		BrainRoot:    "/srv/brain",
	})
	if err != nil {
		t.Fatalf("BuildBotletsTaskOrientation returned error: %v", err)
	}
	if !strings.Contains(body, "Manual Botlets run for pmf-tracker (@max.pmf-tracker).") {
		t.Fatalf("manual task orientation missing manual label: %q", body)
	}
}

func TestBuildBotletsTaskOrientationRejectsInvalidInput(t *testing.T) {
	base := BotletsTaskOrientationInput{
		Kind:         "scheduled",
		BotName:      "pmf-tracker",
		BotHandle:    "max.pmf-tracker",
		RunID:        "blr_abc",
		ScheduledFor: "2026-05-22T09:00:00Z",
		Cron:         "0 9 * * 1-5",
		Timezone:     "UTC",
		BrainRoot:    "/srv/brain",
	}
	cases := []struct {
		name string
		mut  func(*BotletsTaskOrientationInput)
	}{
		{"missing run id", func(i *BotletsTaskOrientationInput) { i.RunID = "" }},
		{"missing brain root", func(i *BotletsTaskOrientationInput) { i.BrainRoot = "" }},
		{"relative brain root", func(i *BotletsTaskOrientationInput) { i.BrainRoot = "relative/brain" }},
		{"newline in bot name", func(i *BotletsTaskOrientationInput) { i.BotName = "pmf\ntracker" }},
		{"invalid kind", func(i *BotletsTaskOrientationInput) { i.Kind = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := base
			tc.mut(&input)
			if _, err := BuildBotletsTaskOrientation(input); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}
}

func TestBuildBotletsSetupOrientationWithBootstrap(t *testing.T) {
	body, err := BuildBotletsSetupOrientation(BotletsSetupOrientationInput{
		BotName:        "pmf-tracker",
		BotDisplayName: "Penny the Pressure Tester",
		BotHandle:      "max.pmf-tracker",
		BrainRoot:      "/srv/brain",
		BaseURL:        "https://comt.dev",
		DocsRoot:       "/home/user/Comment Docs/_Comment.io Docs",
		HasBootstrap:   true,
	})
	if err != nil {
		t.Fatalf("BuildBotletsSetupOrientation returned error: %v", err)
	}
	requireSubstrings(t, body, []string{
		"# Botlets Setup Orientation",
		"You are Penny the Pressure Tester (@max.pmf-tracker), a Botlets bot.",
		"Local bot slug: `pmf-tracker`.",
		"Brain root: `/srv/brain`",
		"`AGENTS.md` - workspace rules and operating guidance",
		"`MEMORY.md` - curated long-term bot memory",
		"`HEARTBEAT.md` - recurring heartbeat and cron task instructions",
		"Also read today's and yesterday's daily notes (`memory/YYYY-MM-DD.md` for today and the previous day) when present.",
		"Before editing, archiving, or remembering anything in the brain, load the Comment.io API instructions now.",
		"`/home/user/Comment Docs/_Comment.io Docs/llms.txt`",
		"Do not use filesystem Edit/Write tools",
		"Do not use Claude Code/Codex built-in memory",
		"`BOOTSTRAP.md` is present in the brain. Read and follow it now",
		"remove it through the Comment.io API or web UI",
		"run `comment sync once`",
		"treat the local file as stale",
	})
}

func TestBuildBotletsSetupOrientationAllowsMultibyteDisplayName(t *testing.T) {
	displayName := strings.Repeat("é", 60)
	body, err := BuildBotletsSetupOrientation(BotletsSetupOrientationInput{
		BotName:        "pmf-tracker",
		BotDisplayName: displayName,
		BotHandle:      "max.pmf-tracker",
		BrainRoot:      "/srv/brain",
		HasBootstrap:   false,
	})
	if err != nil {
		t.Fatalf("BuildBotletsSetupOrientation returned error: %v", err)
	}
	if !strings.Contains(body, displayName) {
		t.Fatalf("setup orientation should include display name: %q", body)
	}
}

func TestBuildBotletsSetupOrientationWithoutBootstrap(t *testing.T) {
	body, err := BuildBotletsSetupOrientation(BotletsSetupOrientationInput{
		BotName:      "pmf-tracker",
		BotHandle:    "max.pmf-tracker",
		BrainRoot:    "/srv/brain",
		HasBootstrap: false,
	})
	if err != nil {
		t.Fatalf("BuildBotletsSetupOrientation returned error: %v", err)
	}
	requireSubstrings(t, body, []string{
		"First-run setup is already complete or not needed.",
	})
	if strings.Contains(body, "already bootstrapped") {
		t.Fatalf("setup orientation should not mention server-side already-bootstrapped flag: %q", body)
	}
	if strings.Contains(body, "BOOTSTRAP.md") {
		t.Fatalf("setup orientation should not mention BOOTSTRAP.md when the file is absent: %q", body)
	}
}

func TestBuildBotletsSetupOrientationWithUnknownBootstrap(t *testing.T) {
	body, err := BuildBotletsSetupOrientation(BotletsSetupOrientationInput{
		BotName:             "pmf-tracker",
		BotHandle:           "max.pmf-tracker",
		BrainRoot:           "/srv/brain",
		BootstrapProbeError: "brain projection path must exist",
	})
	if err != nil {
		t.Fatalf("BuildBotletsSetupOrientation returned error: %v", err)
	}
	requireSubstrings(t, body, []string{
		"Could not inspect `BOOTSTRAP.md` because the local brain projection is not currently readable: brain projection path must exist.",
		"Run `comment sync once` if the brain has not appeared locally",
		"Do not assume bootstrap is complete",
	})
	if strings.Contains(body, "Skip bootstrap") {
		t.Fatalf("unknown bootstrap state should not say bootstrap is skipped: %q", body)
	}
}

func TestExpandBotletsTaskMessageBodyLeavesOtherKindsUnchanged(t *testing.T) {
	original := CloudNotificationMessage{
		Kind: "doc.mention",
		Body: MessageBody{Format: "markdown", Content: "hi"},
	}
	out, err := ExpandBotletsTaskMessageBody(Paths{}, BotRegistryEntry{}, original, CloudNotification{Type: "mention"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Body.Content != "hi" {
		t.Fatalf("non-task body should be unchanged, got %q", out.Body.Content)
	}
}

func TestExpandBotletsTaskMessageBodyKeepsOriginalOnBrainFailure(t *testing.T) {
	bot := BotRegistryEntry{
		Name:   "pmf-tracker",
		Handle: "max.pmf-tracker",
		BrainRef: &BotBrainRef{
			WorkspaceID:     "ws_one",
			OwnerAgentID:    "ag_owner",
			BotAgentID:      "ag_bot",
			ContainerID:     "lc_one",
			RootFolderID:    "lf_one",
			RelativePath:    "Botlets/max/pmf-tracker/brain",
			SetupGeneration: 1,
		},
	}
	message := CloudNotificationMessage{
		Kind: "botlets.task",
		Body: MessageBody{Format: "markdown", Content: "fallback"},
	}
	notification := CloudNotification{
		Type: "botlets_task",
		BotletsTask: &CloudBotletsTaskNotification{
			RunID:        "blr_abc",
			Kind:         "scheduled",
			BotName:      "pmf-tracker",
			BotHandle:    "max.pmf-tracker",
			ScheduledFor: "2026-05-22T09:00:00Z",
			Cron:         "0 9 * * 1-5",
			Timezone:     "UTC",
		},
	}
	out, err := ExpandBotletsTaskMessageBody(Paths{Home: t.TempDir()}, bot, message, notification)
	if err == nil {
		t.Fatalf("expected error when sync root is missing")
	}
	if out.Body.Content != "fallback" {
		t.Fatalf("body should be unchanged when brain validation fails, got %q", out.Body.Content)
	}
}

func requireSubstrings(t *testing.T, haystack string, needles []string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			t.Fatalf("missing %q in:\n%s", needle, haystack)
		}
	}
}
