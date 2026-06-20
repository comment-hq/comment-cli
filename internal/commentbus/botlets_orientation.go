package commentbus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BotletsTaskStartupFileNames are the bot brain files the agent should orient
// from at the start of scheduled/task Botlets runs. Order matches OpenClaw
// cron startup behavior and intentionally excludes MEMORY.md by default.
var BotletsTaskStartupFileNames = []struct {
	Name        string
	Description string
}{
	{"AGENTS.md", "workspace rules and operating guidance"},
	{"TOOLS.md", "local tool/setup notes"},
	{"SOUL.md", "persona, tone, and behavioral guidance"},
	{"IDENTITY.md", "bot identity"},
	{"USER.md", "owner/user profile"},
	{"HEARTBEAT.md", "recurring heartbeat and cron task instructions"},
}

// BotletsSetupStartupFileNames are loaded for the owner-facing setup/main
// session. This includes MEMORY.md so durable bot memory stays in the
// Comment.io brain rather than the runtime's private memory feature.
var BotletsSetupStartupFileNames = []struct {
	Name        string
	Description string
}{
	{"AGENTS.md", "workspace rules and operating guidance"},
	{"TOOLS.md", "local tool/setup notes"},
	{"SOUL.md", "persona, tone, and behavioral guidance"},
	{"IDENTITY.md", "bot identity"},
	{"USER.md", "owner/user profile"},
	{"MEMORY.md", "curated long-term bot memory"},
	{"HEARTBEAT.md", "recurring heartbeat and cron task instructions"},
}

// BotletsStartupFileNames is retained for existing callers that expect the
// task-oriented startup list.
var BotletsStartupFileNames = BotletsTaskStartupFileNames

// BotletsBootstrapFileName is the brain file used only during setup
// orientation. Its presence is what gates whether the agent runs bootstrap;
// there is no separate server-side "already bootstrapped" flag.
const BotletsBootstrapFileName = "BOOTSTRAP.md"

// BotletsTaskOrientationInput is the data needed to expand a scheduled or
// manual botlets.task message body. All fields are required.
type BotletsTaskOrientationInput struct {
	Kind         string
	BotName      string
	BotHandle    string
	RunID        string
	ScheduledFor string
	Cron         string
	Timezone     string
	BrainRoot    string
}

// BotletsSetupOrientationInput is the data needed to build the setup
// orientation prompt that the managed runtime injects after
// `comment botlets setup` finishes.
type BotletsSetupOrientationInput struct {
	BotName             string
	BotDisplayName      string
	BotHandle           string
	BrainRoot           string
	BaseURL             string
	DocsRoot            string
	HasBootstrap        bool
	BootstrapProbeError string
}

// BuildBotletsTaskOrientation returns the local message body for a leased
// botlets.task notification. The body lists file names and exact paths only
// and never inlines brain file contents.
func BuildBotletsTaskOrientation(input BotletsTaskOrientationInput) (string, error) {
	if err := validateOrientationInput(input.BotName, input.BotHandle, input.BrainRoot); err != nil {
		return "", err
	}
	if input.RunID == "" || input.ScheduledFor == "" || input.Cron == "" || input.Timezone == "" {
		return "", errors.New("invalid botlets task orientation input")
	}
	label := "Scheduled Botlets run"
	if input.Kind == "manual" {
		label = "Manual Botlets run"
	} else if input.Kind != "scheduled" {
		return "", errors.New("invalid botlets task kind")
	}
	handle := strings.TrimPrefix(input.BotHandle, "@")
	var b strings.Builder
	b.WriteString("# Botlets Task\n\n")
	fmt.Fprintf(&b, "%s for %s (@%s).\n\n", label, input.BotName, handle)
	fmt.Fprintf(&b, "Run ID: %s\n", input.RunID)
	fmt.Fprintf(&b, "Scheduled window: %s\n", input.ScheduledFor)
	fmt.Fprintf(&b, "Schedule: %s (%s)\n", input.Cron, input.Timezone)
	fmt.Fprintf(&b, "Brain root: `%s`\n\n", input.BrainRoot)
	b.WriteString("## Startup Context\n\n")
	b.WriteString("Before doing the task, orient from the bot brain files below, matching OpenClaw cron startup behavior. Read them from the brain root when present, in this order:\n\n")
	for i, file := range BotletsTaskStartupFileNames {
		fmt.Fprintf(&b, "%d. `%s` - %s\n", i+1, file.Name, file.Description)
	}
	b.WriteString("\nDo not run `BOOTSTRAP.md` as part of a scheduled run. A separate setup/orientation command handles bootstrap when `BOOTSTRAP.md` exists in the brain, matching OpenClaw's file-existence style rather than a server-side \"already bootstrapped\" flag.\n\n")
	b.WriteString("Do not load `MEMORY.md` by default for this scheduled run. If the task itself requires durable memory work, update the appropriate brain files deliberately.\n\n")
	b.WriteString("## Run Instructions\n\n")
	b.WriteString("Read the current brain docs, perform the configured task, and write durable notes/results back into the brain through the Comment.io API or web UI. Local brain files are read-only projections. Use each projection header's `slug:` and `revision:` fields for API writes: `GET /docs/{slug}`, then `PATCH /docs/{slug}` with `base_revision` and `edits`. Do not use Claude Code/Codex runtime memory for bot memory.\n\n")
	b.WriteString("Ack this message when complete. Release it to stop this run without retrying the same task.\n")
	return b.String(), nil
}

// BuildBotletsSetupOrientation returns the setup prompt that the daemon
// injects into the bot runtime on startup. The prompt either directs the agent
// to follow BOOTSTRAP.md or notes that bootstrap is skipped, based on whether
// `BOOTSTRAP.md` exists in the brain root right now.
func BuildBotletsSetupOrientation(input BotletsSetupOrientationInput) (string, error) {
	if err := validateOrientationInput(input.BotName, input.BotHandle, input.BrainRoot); err != nil {
		return "", err
	}
	displayName := strings.TrimSpace(input.BotDisplayName)
	if displayName == "" {
		displayName = input.BotName
	}
	if err := validateOrientationDisplayName(displayName); err != nil {
		return "", err
	}
	if input.DocsRoot != "" && (strings.ContainsAny(input.DocsRoot, "\r\n\x00") || containsSecretValue(input.DocsRoot)) {
		return "", errors.New("invalid docs root")
	}
	if input.BootstrapProbeError != "" && (strings.ContainsAny(input.BootstrapProbeError, "\r\n\x00") || containsSecretValue(input.BootstrapProbeError)) {
		return "", errors.New("invalid bootstrap probe error")
	}
	handle := strings.TrimPrefix(input.BotHandle, "@")
	var b strings.Builder
	b.WriteString("# Botlets Setup Orientation\n\n")
	fmt.Fprintf(&b, "You are %s (@%s), a Botlets bot. The owner just finished local setup.\n\n", displayName, handle)
	if displayName != input.BotName {
		fmt.Fprintf(&b, "Local bot slug: `%s`.\n\n", input.BotName)
	}
	fmt.Fprintf(&b, "Brain root: `%s`\n\n", input.BrainRoot)
	b.WriteString("## Startup Context\n\n")
	b.WriteString("Orient from the bot brain files below in this order, when present:\n\n")
	for i, file := range BotletsSetupStartupFileNames {
		fmt.Fprintf(&b, "%d. `%s` - %s\n", i+1, file.Name, file.Description)
	}
	b.WriteString("\n## Comment.io and Brain Writes\n\n")
	b.WriteString("Before editing, archiving, or remembering anything in the brain, load the Comment.io API instructions now. Use the Comment.io skill if it is listed in this runtime. If it is not listed, read ")
	b.WriteString(formatBotletsDocsReference(input.BaseURL, input.DocsRoot))
	b.WriteString(". Do not continue to brain writes until you have done this.\n\n")
	b.WriteString("Brain files under the local sync root are read-only projections. Do not use filesystem Edit/Write tools on them, including `MEMORY.md`. Write changes through the Comment.io API or web UI only.\n\n")
	b.WriteString("Concrete write path for existing docs: use the local projection header's `slug:` and `revision:` fields, `GET /docs/{slug}` if you need the current body, then `PATCH /docs/{slug}` with `base_revision` and `edits`. Archive finished bootstrap docs with `POST /docs/{slug}/archive`. To create a new durable brain doc, call `POST /docs` with `library_target: {\"kind\":\"bot\"}` while authenticated as this bot. Do not create new private memory files for durable bot facts.\n\n")
	b.WriteString("Do not use Claude Code/Codex built-in memory for this bot. Durable memory belongs in Comment.io brain docs such as `MEMORY.md` and `USER.md`. If the owner asks you to remember or update your brain, do it through the API without asking for a second confirmation.\n")
	if input.BootstrapProbeError != "" {
		b.WriteString("\n## Bootstrap\n\n")
		fmt.Fprintf(&b, "Could not inspect `%s` because the local brain projection is not currently readable: %s.\n\n", BotletsBootstrapFileName, input.BootstrapProbeError)
		b.WriteString("Run `comment sync once` if the brain has not appeared locally, then inspect the brain docs again. Do not assume bootstrap is complete until you have checked whether `BOOTSTRAP.md` exists.\n")
	} else if input.HasBootstrap {
		b.WriteString("\n## Bootstrap\n\n")
		fmt.Fprintf(&b, "`%s` is present in the brain. Read and follow it now to figure out who you are, then remove it through the Comment.io API or web UI when you're done. After removing it, run `comment sync once` and verify the local projection is gone before treating bootstrap as complete. If the API says the doc is archived/removed but the local file still exists, treat the local file as stale and sync; do not rerun bootstrap.\n", BotletsBootstrapFileName)
	} else {
		b.WriteString("\nFirst-run setup is already complete or not needed. Continue from the startup context above.\n")
	}
	b.WriteString("\nThis is an owner-facing orientation pass, not a scheduled task. After orienting, wait for the owner's next message.\n")
	return b.String(), nil
}

func formatBotletsDocsReference(baseURL string, docsRoot string) string {
	if strings.TrimSpace(docsRoot) != "" {
		return fmt.Sprintf("`%s/llms.txt`", strings.TrimRight(docsRoot, "/"))
	}
	if strings.TrimSpace(baseURL) != "" {
		if llmsURL, err := buildNotificationURL(baseURL, llmsInstructionsEndpoint, nil); err == nil {
			return fmt.Sprintf("`%s`", llmsURL)
		}
	}
	return "`/llms.txt`"
}

// BotletsBootstrapPresent reports whether `BOOTSTRAP.md` exists as a regular
// file at the top of the given brain root. Symlinks do not count as present;
// the file must be a real on-disk regular file the agent owns.
func BotletsBootstrapPresent(brainRoot string) (bool, error) {
	if brainRoot == "" {
		return false, errors.New("missing brain root")
	}
	info, err := os.Lstat(filepath.Join(brainRoot, BotletsBootstrapFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, nil
	}
	return true, nil
}

// ExpandBotletsTaskMessageBody replaces the markdown body of a leased
// botlets.task message with the Botlets-oriented task prompt. Returns the
// message unchanged for any other kind, when the notification has no
// botlets_task payload, or when brain projection validation fails. The
// caller is responsible for logging the returned error if present.
func ExpandBotletsTaskMessageBody(paths Paths, bot BotRegistryEntry, message CloudNotificationMessage, notification CloudNotification) (CloudNotificationMessage, error) {
	if message.Kind != "botlets.task" {
		return message, nil
	}
	task := notification.BotletsTask
	if task == nil {
		return message, nil
	}
	if err := validateBotletsTaskOrientationFields(task); err != nil {
		return message, err
	}
	brainRoot, err := ValidateBotletsBrainProjection(paths, bot)
	if err != nil {
		return message, err
	}
	body, err := BuildBotletsTaskOrientation(BotletsTaskOrientationInput{
		Kind:         task.Kind,
		BotName:      task.BotName,
		BotHandle:    task.BotHandle,
		RunID:        task.RunID,
		ScheduledFor: task.ScheduledFor,
		Cron:         task.Cron,
		Timezone:     task.Timezone,
		BrainRoot:    brainRoot,
	})
	if err != nil {
		return message, err
	}
	message.Body = MessageBody{Format: "markdown", Content: body}
	return message, nil
}

func validateBotletsTaskOrientationFields(task *CloudBotletsTaskNotification) error {
	fields := []string{task.RunID, task.ScheduledFor, task.Cron, task.Timezone, task.BotName, task.BotHandle}
	for _, value := range fields {
		if strings.ContainsAny(value, "\r\n\x00") {
			return errors.New("botlets task field contains control characters")
		}
		if containsSecretValue(value) {
			return errors.New("botlets task field contains a secret value")
		}
	}
	return nil
}

func validateOrientationInput(botName string, botHandle string, brainRoot string) error {
	if strings.TrimSpace(botName) == "" || strings.ContainsAny(botName, "\r\n\x00") {
		return errors.New("invalid botlets bot name")
	}
	if strings.TrimSpace(botHandle) == "" || strings.ContainsAny(botHandle, "\r\n\x00") {
		return errors.New("invalid botlets bot handle")
	}
	if brainRoot == "" || strings.ContainsAny(brainRoot, "\r\n\x00") {
		return errors.New("invalid botlets brain root")
	}
	if !filepath.IsAbs(brainRoot) {
		return errors.New("botlets brain root must be absolute")
	}
	return nil
}

func validateOrientationDisplayName(displayName string) error {
	if strings.TrimSpace(displayName) == "" || !validBotDisplayNameLength(displayName) || strings.ContainsAny(displayName, "\r\n\x00") {
		return errors.New("invalid botlets bot display name")
	}
	if containsSecretValue(displayName) {
		return errors.New("invalid botlets bot display name")
	}
	return nil
}
