package commentbus

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeSessionFilePathUsesPinnedUUIDAndCWDKey(t *testing.T) {
	ref := "123e4567-e89b-42d3-a456-426614174000"
	got, err := ClaudeSessionFilePath("/home/user", "/home/user/work/project", ref)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/home/user", ".claude", "projects", "-home-user-work-project", ref+".jsonl")
	if got != want {
		t.Fatalf("ClaudeSessionFilePath() = %q, want %q", got, want)
	}
}

func TestClaudeSessionFilePathHyphenatesSpacesInCWDKey(t *testing.T) {
	ref := "123e4567-e89b-42d3-a456-426614174000"
	got, err := ClaudeSessionFilePath("/home/user", "/home/user/Comment Docs/Botlets/max/feedback/brain", ref)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/home/user", ".claude", "projects", "-home-user-Comment-Docs-Botlets-max-feedback-brain", ref+".jsonl")
	if got != want {
		t.Fatalf("ClaudeSessionFilePath() = %q, want %q", got, want)
	}
}

func TestClaudeSessionFilePathMatchesHiddenDirectoryKey(t *testing.T) {
	ref := "123e4567-e89b-42d3-a456-426614174000"
	got, err := ClaudeSessionFilePath("/home/user", "/home/user/.codex/tmp/bmux-smoke/botlets", ref)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/home/user", ".claude", "projects", "-home-user--codex-tmp-bmux-smoke-botlets", ref+".jsonl")
	if got != want {
		t.Fatalf("ClaudeSessionFilePath() = %q, want %q", got, want)
	}
}

func TestClaudeSessionFilePathRejectsNonUUIDSessionID(t *testing.T) {
	_, err := ClaudeSessionFilePath("/home/user", "/home/user/work/project", "sess_abc")
	if err == nil {
		t.Fatal("ClaudeSessionFilePath() succeeded for non-UUID session id")
	}
}

func TestFindCodexRolloutCorrelationRequiresNonceAndCWD(t *testing.T) {
	userHome := t.TempDir()
	cwd := filepath.Join(userHome, "work", "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	sessionID := "019c81bf-8e4d-7881-905e-dad0194779ab"
	nonce := "op_abcdefghijklmnopqrst"
	path := writeCodexRolloutForTest(t, userHome, now, sessionID, cwd, nonce)

	match, ok, err := FindCodexRolloutCorrelation(userHome, cwd, nonce, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("FindCodexRolloutCorrelation() did not match nonce-bearing rollout")
	}
	if match.Path != path || match.SessionID != sessionID {
		t.Fatalf("match = %+v, want path %q session %q", match, path, sessionID)
	}
}

func TestFindCodexRolloutCorrelationDoesNotFallBackToNewest(t *testing.T) {
	userHome := t.TempDir()
	cwd := filepath.Join(userHome, "work", "project")
	otherCWD := filepath.Join(userHome, "other")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(otherCWD, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	nonce := "op_abcdefghijklmnopqrst"
	writeCodexRolloutForTest(t, userHome, now, "019c81bf-8e4d-7881-905e-dad0194779ab", cwd, "")
	writeCodexRolloutForTest(t, userHome, now, "019c81bf-8e4d-7881-905e-dad0194779ac", otherCWD, nonce)

	match, ok, err := FindCodexRolloutCorrelation(userHome, cwd, nonce, now)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("FindCodexRolloutCorrelation() matched %+v, want no match", match)
	}
}

func TestFindCodexRolloutCorrelationAcceptsResponseItemUserMessage(t *testing.T) {
	userHome := t.TempDir()
	cwd := filepath.Join(userHome, "work", "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	nonce := "op_abcdefghijklmnopqrst"
	sessionID := "019c81bf-8e4d-7881-905e-dad0194779ad"
	path := codexRolloutPathForTest(t, userHome, now, sessionID)
	body := fmt.Sprintf(
		"{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q}}\n{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"hello %s\"}]}}\n",
		sessionID,
		cwd,
		nonce,
	)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	match, ok, err := FindCodexRolloutCorrelation(userHome, cwd, nonce, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || match.SessionID != sessionID {
		t.Fatalf("match = %+v ok=%v, want response_item user message match", match, ok)
	}
}

func TestFindCodexRolloutCorrelationSkipsOversizedUnrelatedLines(t *testing.T) {
	userHome := t.TempDir()
	cwd := filepath.Join(userHome, "work", "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	nonce := "op_abcdefghijklmnopqrst"
	unrelatedPath := codexRolloutPathForTest(t, userHome, now, "019c81bf-8e4d-7881-905e-dad0194779ae")
	if err := os.WriteFile(unrelatedPath, []byte(strings.Repeat("x", codexRolloutMaxLineBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionID := "019c81bf-8e4d-7881-905e-dad0194779af"
	writeCodexRolloutForTest(t, userHome, now, sessionID, cwd, nonce)

	match, ok, err := FindCodexRolloutCorrelation(userHome, cwd, nonce, now)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || match.SessionID != sessionID {
		t.Fatalf("match = %+v ok=%v, want oversized unrelated line skipped", match, ok)
	}
}

func TestFindCodexRolloutCorrelationRejectsSessionIDMismatch(t *testing.T) {
	userHome := t.TempDir()
	cwd := filepath.Join(userHome, "work", "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	nonce := "op_abcdefghijklmnopqrst"
	pathSessionID := "019c81bf-8e4d-7881-905e-dad0194779aa"
	jsonSessionID := "019c81bf-8e4d-7881-905e-dad0194779bb"
	path := codexRolloutPathForTest(t, userHome, now, pathSessionID)
	body := fmt.Sprintf(
		"{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":%q}}\n",
		jsonSessionID,
		cwd,
		nonce,
	)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	match, ok, err := FindCodexRolloutCorrelation(userHome, cwd, nonce, now)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("FindCodexRolloutCorrelation() matched mismatched ids: %+v", match)
	}
}

func TestFindCodexRolloutCorrelationSkipsSymlinkCandidates(t *testing.T) {
	userHome := t.TempDir()
	cwd := filepath.Join(userHome, "work", "project")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	nonce := "op_abcdefghijklmnopqrst"
	target := filepath.Join(userHome, "target.jsonl")
	body := fmt.Sprintf(
		"{\"type\":\"session_meta\",\"payload\":{\"id\":\"019c81bf-8e4d-7881-905e-dad0194779ac\",\"cwd\":%q}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":%q}}\n",
		cwd,
		nonce,
	)
	if err := os.WriteFile(target, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	link := codexRolloutPathForTest(t, userHome, now, "019c81bf-8e4d-7881-905e-dad0194779ac")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	match, ok, err := FindCodexRolloutCorrelation(userHome, cwd, nonce, now)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("FindCodexRolloutCorrelation() matched symlink candidate: %+v", match)
	}
}

func writeCodexRolloutForTest(t *testing.T, userHome string, now time.Time, sessionID string, cwd string, nonce string) string {
	t.Helper()
	path := codexRolloutPathForTest(t, userHome, now, sessionID)
	message := "hello"
	if nonce != "" {
		message += " " + nonce
	}
	body := fmt.Sprintf(
		"{\"type\":\"session_meta\",\"payload\":{\"id\":%q,\"cwd\":%q}}\n{\"type\":\"event_msg\",\"payload\":{\"type\":\"user_message\",\"message\":%q}}\n",
		sessionID,
		cwd,
		message,
	)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func codexRolloutPathForTest(t *testing.T, userHome string, now time.Time, sessionID string) string {
	t.Helper()
	dir := filepath.Join(userHome, ".codex", "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+sessionID+".jsonl")
}
