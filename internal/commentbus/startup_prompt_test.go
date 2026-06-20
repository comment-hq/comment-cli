package commentbus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFormatTmuxStartupInstructionFallsBackToURLWhenPathsMissing(t *testing.T) {
	text, err := formatTmuxStartupInstruction("https://comment.example/base/", "max.reviewer", startupOrientationPaths{})
	if err != nil {
		t.Fatal(err)
	}
	want := "You are an agent running on comment.io with the handle max.reviewer. Before handling Comment.io work, fetch, read, and understand the full agent instructions at https://comment.example/base/llms.txt. Write through the Comment.io API or web UI."
	if text != want {
		t.Fatalf("startup instruction = %q, want %q", text, want)
	}
	if strings.ContainsAny(text, "\r\n\x00") {
		t.Fatalf("startup instruction contains unsafe control chars: %q", text)
	}
}

func TestFormatTmuxStartupInstructionInlinesResolvedPathsWhenSet(t *testing.T) {
	text, err := formatTmuxStartupInstruction(
		"https://comment.example/base/",
		"max.reviewer",
		startupOrientationPaths{
			DocsRoot: "/home/user/Comment Docs/_Comment.io Docs",
			SyncRoot: "/home/user/Comment Docs",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "COMMENT_IO_LOCAL_DOCS_ROOT") || strings.Contains(text, "COMMENT_IO_LOCAL_SYNC_ROOT") {
		t.Fatalf("startup instruction should not reference env-var names literally when paths are resolved: %q", text)
	}
	if !strings.Contains(text, "read /home/user/Comment Docs/_Comment.io Docs/llms.txt") {
		t.Fatalf("expected inlined docs-root in startup instruction: %q", text)
	}
	if !strings.Contains(text, "Prefer /home/user/Comment Docs for broad local reads") {
		t.Fatalf("expected inlined sync-root in startup instruction: %q", text)
	}
	if strings.Contains(text, "https://comment.example/base/llms.txt") {
		t.Fatalf("startup instruction should not fall back to URL when docs root is set: %q", text)
	}
}

func TestFormatBotletsTmuxStartupInstructionIncludesBrainRootAndStartupFiles(t *testing.T) {
	text, err := formatBotletsTmuxStartupInstruction(
		"https://comment.example/base/",
		"personal",
		"Personal Assistant",
		"max.personal",
		startupOrientationPaths{
			DocsRoot:  "/home/user/Comment Docs/_Comment.io Docs",
			SyncRoot:  "/home/user/Comment Docs",
			BrainRoot: "/home/user/Comment Docs/Botlets/max/personal/brain",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "You are Personal Assistant (@max.personal), a Botlets bot.") {
		t.Fatalf("startup instruction should name the bot and handle: %q", text)
	}
	if !strings.Contains(text, "Your brain is at /home/user/Comment Docs/Botlets/max/personal/brain.") {
		t.Fatalf("startup instruction should name the brain root: %q", text)
	}
	for _, name := range []string{"AGENTS.md", "TOOLS.md", "SOUL.md", "IDENTITY.md", "USER.md", "MEMORY.md", "HEARTBEAT.md", "BOOTSTRAP.md"} {
		if name == "BOOTSTRAP.md" {
			if strings.Contains(text, name) {
				t.Fatalf("startup instruction should not mention bootstrap when using compact fallback: %q", text)
			}
			continue
		}
		if !strings.Contains(text, name) {
			t.Fatalf("startup instruction should mention %s: %q", name, text)
		}
	}
	if !strings.Contains(text, "read /home/user/Comment Docs/_Comment.io Docs/llms.txt") {
		t.Fatalf("startup instruction should inline docs root: %q", text)
	}
	if !strings.Contains(text, "never runtime memory") {
		t.Fatalf("startup instruction should reject runtime-local memory: %q", text)
	}
	if !strings.Contains(text, `POST /docs with library_target {"kind":"bot"}`) {
		t.Fatalf("startup instruction should explain new bot brain doc creation: %q", text)
	}
	if strings.ContainsAny(text, "\r\n\x00") {
		t.Fatalf("startup instruction contains unsafe control chars: %q", text)
	}
	if len(text) > 512 {
		t.Fatalf("startup instruction exceeds 512 bytes: %d", len(text))
	}
}

func TestFormatBotletsTmuxStartupInstructionMentionsBootstrapWhenPresent(t *testing.T) {
	brainRoot := filepath.Join(t.TempDir(), "brain")
	if err := os.MkdirAll(brainRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(brainRoot, BotletsBootstrapFileName), []byte("# bootstrap\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, err := formatBotletsTmuxStartupInstruction(
		"https://comment.example/base/",
		"personal",
		"Personal Assistant",
		"max.personal",
		startupOrientationPaths{BrainRoot: brainRoot},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "BOOTSTRAP.md exists; follow it now") {
		t.Fatalf("startup instruction should mention present bootstrap: %q", text)
	}
	if !strings.Contains(text, "remove it via Comment.io") || !strings.Contains(text, "run comment sync once") {
		t.Fatalf("startup instruction should avoid local projection edits: %q", text)
	}
	if len(text) > 512 {
		t.Fatalf("startup instruction exceeds 512 bytes: %d", len(text))
	}
}

func TestFormatBotletsTmuxStartupInstructionRequiresBrainRoot(t *testing.T) {
	_, err := formatBotletsTmuxStartupInstruction(
		"https://comment.example/base/",
		"personal",
		"",
		"max.personal",
		startupOrientationPaths{},
	)
	if err == nil {
		t.Fatalf("expected error when brain root is empty")
	}
}

func TestFormatTmuxStartupInstructionFitsWithinChunkLimit(t *testing.T) {
	text, err := formatTmuxStartupInstruction(
		"https://comment.example/base/",
		"max.reviewer",
		startupOrientationPaths{
			DocsRoot: "/home/user/Comment Docs/_Comment.io Docs",
			SyncRoot: "/home/user/Comment Docs",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > 512 {
		t.Fatalf("startup instruction exceeds 512 bytes: %d", len(text))
	}
}

func TestFormatTmuxStartupInstructionFallsBackWhenPathsOverflowChunkLimit(t *testing.T) {
	longPath := "/home/user/Library/Mobile Documents/com~apple~CloudDocs/" + strings.Repeat("nested/", 50) + "sync-root"
	text, err := formatTmuxStartupInstruction(
		"https://comment.example/base/",
		"max.reviewer",
		startupOrientationPaths{
			DocsRoot: longPath + "/_Comment.io Docs",
			SyncRoot: longPath,
		},
	)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if len(text) > 512 {
		t.Fatalf("fallback startup instruction still exceeds 512 bytes: %d", len(text))
	}
	if strings.Contains(text, longPath) {
		t.Fatalf("fallback should have dropped the long path, but it is still present: %q", text)
	}
	if !strings.Contains(text, "https://comment.example/base/llms.txt") {
		t.Fatalf("fallback should reference the URL llms.txt: %q", text)
	}
}

func TestFormatBotletsTmuxStartupInstructionFallsBackToURLWhenDocsRootOverflows(t *testing.T) {
	longDocsRoot := "/home/user/Library/Mobile Documents/com~apple~CloudDocs/" + strings.Repeat("nested/", 30) + "_Comment.io Docs"
	brainRoot := "/home/user/Comment Docs/Botlets/max/personal/brain"
	text, err := formatBotletsTmuxStartupInstruction(
		"https://comment.example/base/",
		"personal",
		"",
		"max.personal",
		startupOrientationPaths{
			DocsRoot:  longDocsRoot,
			BrainRoot: brainRoot,
		},
	)
	if err != nil {
		t.Fatalf("expected fallback to succeed, got error: %v", err)
	}
	if len(text) > 512 {
		t.Fatalf("fallback startup instruction still exceeds 512 bytes: %d", len(text))
	}
	if !strings.Contains(text, brainRoot) {
		t.Fatalf("fallback must preserve brain root: %q", text)
	}
	if strings.Contains(text, longDocsRoot) {
		t.Fatalf("fallback should drop the overlong docs root: %q", text)
	}
	if !strings.Contains(text, "https://comment.example/base/llms.txt") {
		t.Fatalf("fallback should reference the URL llms.txt: %q", text)
	}
}
