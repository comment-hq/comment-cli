//go:build darwin || linux

package commentbus

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSendBotletsMultilineOrientationPastesRichOrientation verifies that
// when a botlets bot has a valid brain projection, the daemon picks the
// multi-line BuildBotletsSetupOrientation path and pastes it via
// PasteText (newlines preserved), then sends Enter to submit.
func TestSendBotletsMultilineOrientationPastesRichOrientation(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	brainRoot := writeLocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")

	tmux := newTestTmuxController()
	if err := tmux.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName: "comment-orient-sess",
		WorkingDir:  root,
		Command:     "true",
	}); err != nil {
		t.Fatal(err)
	}
	paneTarget, err := tmux.PaneTarget(context.Background(), "comment-orient-sess")
	if err != nil {
		t.Fatal(err)
	}
	logger, err := newStructuredLogger(paths, &bytes.Buffer{}, func() time.Time { return time.Unix(0, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{paths: paths, tmux: tmux, logger: logger}

	bot := BotRegistryEntry{
		Name:        "reviewer",
		DisplayName: "Review Captain",
		Handle:      "max.reviewer",
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
	sent, sendErr := daemon.sendBotletsMultilineOrientation(context.Background(), paneTarget, "max.reviewer", bot)
	if !sent {
		t.Fatalf("expected sent=true for valid botlets bot, got sent=false sendErr=%v", sendErr)
	}
	if sendErr != nil {
		t.Fatalf("expected sendErr=nil after paste+enter succeed, got %v", sendErr)
	}
	if len(tmux.sends) < 2 {
		t.Fatalf("expected at least two tmux sends (paste + enter), got %d: %v", len(tmux.sends), tmux.sends)
	}
	pasteRecord := tmux.sends[len(tmux.sends)-2]
	enterRecord := tmux.sends[len(tmux.sends)-1]
	if !strings.HasPrefix(pasteRecord, "paste "+paneTarget+" ") {
		t.Fatalf("expected paste send, got %q", pasteRecord)
	}
	if !strings.HasPrefix(enterRecord, "enter "+paneTarget) {
		t.Fatalf("expected enter send, got %q", enterRecord)
	}
	body := strings.TrimPrefix(pasteRecord, "paste "+paneTarget+" ")
	for _, expected := range []string{
		"# Botlets Setup Orientation",
		"You are Review Captain (@max.reviewer), a Botlets bot.",
		"Local bot slug: `reviewer`.",
		"Brain root:",
		"`AGENTS.md`",
		"`TOOLS.md`",
		"`SOUL.md`",
		"`IDENTITY.md`",
		"`USER.md`",
		"`MEMORY.md`",
		"`HEARTBEAT.md`",
		"Before editing, archiving, or remembering anything in the brain, load the Comment.io API instructions now.",
		"Brain files under the local sync root are read-only projections",
		"Do not use Claude Code/Codex built-in memory",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("pasted body missing %q. body=%q", expected, body)
		}
	}
	if strings.Contains(body, "BOOTSTRAP.md") {
		t.Fatalf("pasted body should not mention bootstrap when absent. body=%q", body)
	}
	if !strings.Contains(body, brainRoot) {
		t.Fatalf("pasted body should include resolved brain root %q. body=%q", brainRoot, body)
	}
	if !strings.Contains(body, "\n") {
		t.Fatalf("pasted body should be multi-line; got single-line: %q", body)
	}
	if strings.HasSuffix(body, "\n") || strings.HasSuffix(body, "\r") {
		t.Fatalf("pasted body should not include a trailing newline before SendEnter: %q", body)
	}
}

func TestSendBotletsMultilineOrientationExplainsMissingBrainProjection(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	brainRoot := writeLocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")
	if err := os.RemoveAll(brainRoot); err != nil {
		t.Fatal(err)
	}

	tmux := newTestTmuxController()
	if err := tmux.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName: "comment-missing-brain-sess",
		WorkingDir:  root,
		Command:     "true",
	}); err != nil {
		t.Fatal(err)
	}
	paneTarget, err := tmux.PaneTarget(context.Background(), "comment-missing-brain-sess")
	if err != nil {
		t.Fatal(err)
	}
	logger, err := newStructuredLogger(paths, &bytes.Buffer{}, func() time.Time { return time.Unix(0, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{paths: paths, tmux: tmux, logger: logger}

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
	sent, sendErr := daemon.sendBotletsMultilineOrientation(context.Background(), paneTarget, "max.reviewer", bot)
	if !sent {
		t.Fatalf("expected sent=true for missing-but-known brain projection, got sent=false sendErr=%v", sendErr)
	}
	if sendErr != nil {
		t.Fatalf("expected sendErr=nil after paste+enter succeed, got %v", sendErr)
	}
	pasteRecord := tmux.sends[len(tmux.sends)-2]
	body := strings.TrimPrefix(pasteRecord, "paste "+paneTarget+" ")
	for _, expected := range []string{
		"# Botlets Setup Orientation",
		"You are reviewer (@max.reviewer), a Botlets bot.",
		"Brain root: `" + normalizeTrustedBotletsParentPath(brainRoot) + "`",
		"Could not inspect `BOOTSTRAP.md` because the local brain projection is not currently readable: brain projection path must exist.",
		"Run `comment sync once` if the brain has not appeared locally",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("pasted missing-brain body missing %q. body=%q", expected, body)
		}
	}
	if strings.Contains(body, "Skip bootstrap") {
		t.Fatalf("missing brain orientation should not skip bootstrap: %q", body)
	}
}

// TestSendBotletsMultilineOrientationFallsBackWhenPasteFails verifies
// that PasteText failures return sent=false so the caller can fall back
// to the single-line builder without orphan text in the pane.
func TestSendBotletsMultilineOrientationFallsBackWhenPasteFails(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	_ = writeLocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")

	tmux := newTestTmuxController()
	if err := tmux.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName: "comment-fallback-sess",
		WorkingDir:  root,
		Command:     "true",
	}); err != nil {
		t.Fatal(err)
	}
	paneTarget, err := tmux.PaneTarget(context.Background(), "comment-fallback-sess")
	if err != nil {
		t.Fatal(err)
	}
	// Mark the pane as paste-failing.
	tmux.mu.Lock()
	if tmux.failSend == nil {
		tmux.failSend = map[string]bool{}
	}
	tmux.failSend[paneTarget] = true
	tmux.mu.Unlock()

	logger, err := newStructuredLogger(paths, &bytes.Buffer{}, func() time.Time { return time.Unix(0, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{paths: paths, tmux: tmux, logger: logger}

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
	sent, sendErr := daemon.sendBotletsMultilineOrientation(context.Background(), paneTarget, "max.reviewer", bot)
	if sent {
		t.Fatalf("expected sent=false when PasteText fails (so caller can fall back), got sent=true sendErr=%v", sendErr)
	}
	for _, record := range tmux.sends {
		if strings.HasPrefix(record, "enter ") {
			t.Fatalf("Enter should not be sent after a failed paste, got record %q", record)
		}
	}
}

// TestSendBotletsMultilineOrientationHonorsEscapeHatch verifies that
// setting COMMENT_BUS_BOTLETS_MULTILINE_ORIENTATION=0 short-circuits the
// multi-line path so a misbehaving terminal can be worked around without a
// redeploy.
func TestSendBotletsMultilineOrientationHonorsEscapeHatch(t *testing.T) {
	t.Setenv("COMMENT_BUS_BOTLETS_MULTILINE_ORIENTATION", "0")
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	_ = writeLocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")
	tmux := newTestTmuxController()
	if err := tmux.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName: "comment-escape-sess",
		WorkingDir:  root,
		Command:     "true",
	}); err != nil {
		t.Fatal(err)
	}
	paneTarget, err := tmux.PaneTarget(context.Background(), "comment-escape-sess")
	if err != nil {
		t.Fatal(err)
	}
	logger, err := newStructuredLogger(paths, &bytes.Buffer{}, func() time.Time { return time.Unix(0, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{paths: paths, tmux: tmux, logger: logger}
	sent, sendErr := daemon.sendBotletsMultilineOrientation(context.Background(), paneTarget, "max.reviewer", BotRegistryEntry{
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
	if sent {
		t.Fatalf("escape hatch should force sent=false, got sent=true sendErr=%v", sendErr)
	}
	if len(tmux.sends) != 0 {
		t.Fatalf("escape hatch must not touch tmux, got sends=%v", tmux.sends)
	}
}

// TestSendBotletsMultilineOrientationSkipsBotsWithoutBrainRef makes sure
// non-botlets bots short-circuit before even attempting the multi-line
// path.
func TestSendBotletsMultilineOrientationSkipsBotsWithoutBrainRef(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	tmux := newTestTmuxController()
	logger, err := newStructuredLogger(paths, &bytes.Buffer{}, func() time.Time { return time.Unix(0, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{paths: paths, tmux: tmux, logger: logger}

	sent, sendErr := daemon.sendBotletsMultilineOrientation(context.Background(), "%1", "max.reviewer", BotRegistryEntry{
		Name:   "reviewer",
		Handle: "max.reviewer",
	})
	if sent {
		t.Fatalf("non-botlets bot must not trigger multi-line orientation, got sent=true sendErr=%v", sendErr)
	}
	if len(tmux.sends) != 0 {
		t.Fatalf("non-botlets bot must not touch tmux, got sends=%v", tmux.sends)
	}
}

func TestSendBotletsMultilineOrientationSubmitsInRealTmux(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	root := privateTestDir(t, "comment-bus-real-tmux-")
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	brainRoot := writeLocalSyncBrainProjectionForTest(t, paths, "Botlets/max/reviewer/brain")
	if err := os.WriteFile(filepath.Join(brainRoot, BotletsBootstrapFileName), []byte("bootstrap\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	outputPath := filepath.Join(root, "orientation.txt")
	scriptPath := filepath.Join(root, "read-orientation.sh")
	script := `#!/bin/sh
stty -echo 2>/dev/null || true
: > "$1"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "$1"
done
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	sessionName, err := randomTmuxBufferName()
	if err != nil {
		t.Fatal(err)
	}
	controller := ExecTmuxController{Binary: tmuxPath}
	if err := controller.NewSession(context.Background(), TmuxNewSessionOptions{
		SessionName:   sessionName,
		WorkingDir:    root,
		CommentHome:   root,
		BotletsHome: root,
		Command:       shellQuote(scriptPath) + " " + shellQuote(outputPath),
	}); err != nil {
		t.Fatalf("start real tmux session: %v", err)
	}
	t.Cleanup(func() {
		_ = controller.KillSession(context.Background(), sessionName)
	})
	paneTarget, err := controller.PaneTarget(context.Background(), sessionName)
	if err != nil {
		t.Fatal(err)
	}
	logger, err := newStructuredLogger(paths, &bytes.Buffer{}, func() time.Time { return time.Unix(0, 0).UTC() })
	if err != nil {
		t.Fatal(err)
	}
	daemon := &Daemon{paths: paths, tmux: controller, logger: logger}
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
	sent, sendErr := daemon.sendBotletsMultilineOrientation(context.Background(), paneTarget, "max.reviewer", bot)
	if !sent || sendErr != nil {
		t.Fatalf("send real tmux orientation sent=%v err=%v", sent, sendErr)
	}
	body := waitForFileContaining(t, outputPath, "This is an owner-facing orientation pass,", 2*time.Second)
	for _, expected := range []string{
		"# Botlets Setup Orientation",
		"You are reviewer (@max.reviewer), a Botlets bot.",
		"Brain root: `" + brainRoot + "`",
		"`BOOTSTRAP.md` is present in the brain.",
		"This is an owner-facing orientation pass, not a scheduled task.",
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("real tmux captured body missing %q:\n%s", expected, body)
		}
	}
}

func waitForFileContaining(t *testing.T, path string, needle string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			last = string(data)
			if strings.Contains(last, needle) {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q in %s; last content:\n%s", needle, path, last)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
