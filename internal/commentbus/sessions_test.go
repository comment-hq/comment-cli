//go:build darwin || linux

package commentbus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRegisterSessionWritesRecordAndCapability(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		BotID:       "ag_bot",
		BotAgentID:  "ag_bot_agent",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		Now:         time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !LocalSessionIDRE.MatchString(record.SessionID) || !LocalSessionGenerationIDRE.MatchString(record.Generation) {
		t.Fatalf("invalid generated ids: %+v", record)
	}
	if record.Runtime != "claude" || len(record.RuntimeCommand) != 2 || record.RuntimeCommand[0] != "claude" || record.RuntimeCommand[1] != "--dangerously-skip-permissions" {
		t.Fatalf("unexpected runtime command: %+v", record.RuntimeCommand)
	}
	if token, err := ReadCapability(record.CapabilityFile); err != nil || !CapabilityTokenRE.MatchString(token) {
		t.Fatalf("capability token invalid: token=%q err=%v", token, err)
	}
	recordInfo, err := os.Stat(sessionRecordPath(paths, record.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	if recordInfo.Mode().Perm() != 0o600 {
		t.Fatalf("session record mode = %o", recordInfo.Mode().Perm())
	}
	capInfo, err := os.Stat(record.CapabilityFile)
	if err != nil {
		t.Fatal(err)
	}
	if capInfo.Mode().Perm() != 0o600 {
		t.Fatalf("session capability mode = %o", capInfo.Mode().Perm())
	}

	loaded, err := ReadSessionRecord(paths, record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.SessionID != record.SessionID || loaded.CapabilityFile != record.CapabilityFile || loaded.BotID != "ag_bot" || loaded.BotAgentID != "ag_bot_agent" {
		t.Fatalf("loaded record = %+v, want %+v", loaded, record)
	}
}

func TestRegisterSessionAcceptsCodexRuntime(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		Runtime:     "codex",
		Now:         time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Runtime != "codex" || len(record.RuntimeCommand) != 2 || record.RuntimeCommand[0] != "codex" || record.RuntimeCommand[1] != "--yolo" {
		t.Fatalf("unexpected codex runtime command: %+v", record)
	}
}

func TestRegisterSessionBmuxCodexAllowsEmptyRuntimeSessionRef(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Host:        SessionHostBmux,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		SessionName: "comment-reviewer-abc123",
		Runtime:     "codex",
		Now:         time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.RuntimeSessionRef != "" {
		t.Fatalf("RuntimeSessionRef = %q, want empty until nonce correlation", record.RuntimeSessionRef)
	}
	wantCommand := []string{"codex", "--yolo"}
	if fmt.Sprint(record.RuntimeCommand) != fmt.Sprint(wantCommand) {
		t.Fatalf("runtime command = %#v, want %#v", record.RuntimeCommand, wantCommand)
	}
	if record.OutputLogPath == "" {
		t.Fatal("bmux Codex record should still allocate an output log")
	}
}

func TestRegisterSessionBmuxCodexResumeUsesRuntimeSessionRef(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:             paths,
		Host:              SessionHostBmux,
		Profile:           "max.reviewer",
		BotName:           "reviewer",
		ScopeType:         "profile",
		ScopeID:           "max.reviewer",
		BotletsHome:       filepath.Join(paths.Home, "botlets"),
		SessionName:       "comment-reviewer-abc123",
		Runtime:           "codex",
		RuntimeSessionRef: "019c81bf-8e4d-7881-905e-dad0194779ab",
		LaunchMode:        managedSessionLaunchResume,
		Now:               time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := []string{"codex", "resume", record.RuntimeSessionRef, "--yolo"}
	if fmt.Sprint(record.RuntimeCommand) != fmt.Sprint(wantCommand) {
		t.Fatalf("runtime command = %#v, want %#v", record.RuntimeCommand, wantCommand)
	}
	if !managedSessionRuntimeCommandMatches(record) {
		t.Fatalf("resume runtime command did not validate: %#v", record.RuntimeCommand)
	}
}

func TestManagedCodexRuntimeCommandRejectsEphemeral(t *testing.T) {
	record := SessionRecord{
		Host:              SessionHostBmux,
		Profile:           "max.reviewer",
		BotName:           "reviewer",
		ScopeType:         "profile",
		ScopeID:           "max.reviewer",
		BotletsHome:       "/tmp/botlets",
		SessionID:         "sess_abcdefghijklmnopqrst",
		SessionName:       "comment-reviewer-abc123",
		Generation:        "gen_abcdefghijklmnop",
		CapabilityFile:    "/tmp/capability",
		Runtime:           "codex",
		RuntimeSessionRef: "019c81bf-8e4d-7881-905e-dad0194779ab",
		RuntimeCommand:    []string{"codex", "--yolo", "--ephemeral"},
		OutputLogPath:     "/tmp/runtime.log",
		CreatedAt:         time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		State:             "alive",
	}
	if managedSessionRuntimeCommandMatches(record) {
		t.Fatal("managed Codex command with --ephemeral matched")
	}
}

func TestManagedBmuxCodexRuntimeCommandRejectsLegacyNoYolo(t *testing.T) {
	record := SessionRecord{
		Host:           SessionHostBmux,
		Profile:        "max.reviewer",
		BotName:        "reviewer",
		ScopeType:      "profile",
		ScopeID:        "max.reviewer",
		BotletsHome:    "/tmp/botlets",
		SessionID:      "sess_abcdefghijklmnopqrst",
		SessionName:    "comment-reviewer-abc123",
		Generation:     "gen_abcdefghijklmnop",
		CapabilityFile: "/tmp/capability",
		Runtime:        "codex",
		RuntimeCommand: []string{"codex"},
		OutputLogPath:  "/tmp/runtime.log",
		CreatedAt:      time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		State:          "alive",
	}
	if managedSessionRuntimeCommandMatches(record) {
		t.Fatal("bmux Codex command without --yolo matched")
	}
	record.Host = SessionHostTmux
	if !managedSessionRuntimeCommandMatches(record) {
		t.Fatal("tmux legacy Codex command should remain accepted for old records")
	}
}

func TestManagedBmuxCodexRuntimeCommandValidatesResumeTupleExactly(t *testing.T) {
	record := SessionRecord{
		Host:              SessionHostBmux,
		Profile:           "max.reviewer",
		BotName:           "reviewer",
		ScopeType:         "profile",
		ScopeID:           "max.reviewer",
		BotletsHome:       "/tmp/botlets",
		SessionID:         "sess_abcdefghijklmnopqrst",
		SessionName:       "comment-reviewer-abc123",
		Generation:        "gen_abcdefghijklmnop",
		CapabilityFile:    "/tmp/capability",
		Runtime:           "codex",
		RuntimeSessionRef: "019c81bf-8e4d-7881-905e-dad0194779ab",
		RuntimeCommand:    []string{"codex", "resume", "019c81bf-8e4d-7881-905e-dad0194779ab", "--yolo"},
		OutputLogPath:     "/tmp/runtime.log",
		CreatedAt:         time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		State:             "alive",
	}
	if !managedSessionRuntimeCommandMatches(record) {
		t.Fatal("bmux Codex resume command should validate")
	}
	record.RuntimeCommand = append(record.RuntimeCommand, "--ephemeral")
	if managedSessionRuntimeCommandMatches(record) {
		t.Fatal("bmux Codex resume command with extra args matched")
	}
	record.RuntimeCommand = []string{"codex", "resume", "019c81bf-8e4d-7881-905e-dad0194779ac", "--yolo"}
	if managedSessionRuntimeCommandMatches(record) {
		t.Fatal("bmux Codex resume command with mismatched ref matched")
	}
	record.RuntimeSessionRef = "--last"
	record.RuntimeCommand = []string{"codex", "resume", "--last", "--yolo"}
	if managedSessionRuntimeCommandMatches(record) {
		t.Fatal("bmux Codex resume command with option-shaped ref matched")
	}
}

func TestRegisterSessionBmuxClaudePinsRuntimeSessionRefAndOutputLog(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Host:        SessionHostBmux,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		SessionName: "comment-reviewer-abc123",
		Runtime:     "claude",
		Now:         time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Host != SessionHostBmux {
		t.Fatalf("host = %q, want %q", record.Host, SessionHostBmux)
	}
	if !UUIDRE.MatchString(record.RuntimeSessionRef) {
		t.Fatalf("runtime session ref = %q, want UUID", record.RuntimeSessionRef)
	}
	wantCommand := []string{"claude", "--session-id", record.RuntimeSessionRef, "--dangerously-skip-permissions"}
	if fmt.Sprint(record.RuntimeCommand) != fmt.Sprint(wantCommand) {
		t.Fatalf("runtime command = %#v, want %#v", record.RuntimeCommand, wantCommand)
	}
	if record.OutputLogPath == "" || !filepath.IsAbs(record.OutputLogPath) || filepath.Base(record.OutputLogPath) != record.SessionID+".log" {
		t.Fatalf("output log path = %q, want managed session log path", record.OutputLogPath)
	}
	loaded, err := ReadSessionRecord(paths, record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RuntimeSessionRef != record.RuntimeSessionRef || loaded.OutputLogPath != record.OutputLogPath {
		t.Fatalf("loaded bmux record = %+v, want ref/log from %+v", loaded, record)
	}
}

func TestRegisterSessionBmuxClaudeResumeUsesRuntimeSessionRef(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:             paths,
		Host:              SessionHostBmux,
		Profile:           "max.reviewer",
		BotName:           "reviewer",
		ScopeType:         "profile",
		ScopeID:           "max.reviewer",
		BotletsHome:       filepath.Join(paths.Home, "botlets"),
		SessionName:       "comment-reviewer-abc123",
		Runtime:           "claude",
		RuntimeSessionRef: "123e4567-e89b-42d3-a456-426614174000",
		LaunchMode:        managedSessionLaunchResume,
		Now:               time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantCommand := []string{"claude", "--resume", record.RuntimeSessionRef, "--dangerously-skip-permissions"}
	if fmt.Sprint(record.RuntimeCommand) != fmt.Sprint(wantCommand) {
		t.Fatalf("runtime command = %#v, want %#v", record.RuntimeCommand, wantCommand)
	}
	if !managedSessionRuntimeCommandMatches(record) {
		t.Fatalf("resume runtime command did not validate: %#v", record.RuntimeCommand)
	}
}

func TestManagedBmuxClaudeRuntimeCommandValidatesResumeTupleExactly(t *testing.T) {
	record := SessionRecord{
		Host:              SessionHostBmux,
		Profile:           "max.reviewer",
		BotName:           "reviewer",
		ScopeType:         "profile",
		ScopeID:           "max.reviewer",
		BotletsHome:       "/tmp/botlets",
		SessionID:         "sess_abcdefghijklmnopqrst",
		SessionName:       "comment-reviewer-abc123",
		Generation:        "gen_abcdefghijklmnop",
		CapabilityFile:    "/tmp/capability",
		Runtime:           "claude",
		RuntimeSessionRef: "123e4567-e89b-42d3-a456-426614174000",
		RuntimeCommand:    []string{"claude", "--resume", "123e4567-e89b-42d3-a456-426614174000", "--agent", "reviewer", "--dangerously-skip-permissions"},
		OutputLogPath:     "/tmp/runtime.log",
		CreatedAt:         time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
		State:             "alive",
	}
	if !managedSessionRuntimeCommandMatches(record) {
		t.Fatal("bmux Claude resume command should validate")
	}
	record.RuntimeCommand = append(record.RuntimeCommand, "--print")
	if managedSessionRuntimeCommandMatches(record) {
		t.Fatal("bmux Claude resume command with extra args matched")
	}
	record.RuntimeCommand = []string{"claude", "--resume", "023e4567-e89b-42d3-a456-426614174000", "--agent", "reviewer", "--dangerously-skip-permissions"}
	if managedSessionRuntimeCommandMatches(record) {
		t.Fatal("bmux Claude resume command with mismatched ref matched")
	}
}

func TestRegisterSessionBmuxClaudeRejectsInvalidRuntimeSessionRef(t *testing.T) {
	paths := testDaemonPaths(t)
	_, err := RegisterSession(RegisterSessionOptions{
		Paths:             paths,
		Host:              SessionHostBmux,
		Profile:           "max.reviewer",
		BotName:           "reviewer",
		ScopeType:         "profile",
		ScopeID:           "max.reviewer",
		BotletsHome:       filepath.Join(paths.Home, "botlets"),
		SessionName:       "comment-reviewer-abc123",
		Runtime:           "claude",
		RuntimeSessionRef: "sess_not-a-uuid",
		Now:               time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
	})
	if !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("RegisterSession() error = %v, want ErrInvalidSession", err)
	}
}

func TestRegisterSessionBmuxRejectsNonCanonicalPaneTarget(t *testing.T) {
	paths := testDaemonPaths(t)
	paneTarget, err := BmuxSocketPathForSession(paths, "comment-other-abc123")
	if err != nil {
		t.Fatal(err)
	}
	_, err = RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Host:        SessionHostBmux,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		SessionName: "comment-reviewer-abc123",
		PaneTarget:  paneTarget,
		Runtime:     "claude",
		Now:         time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
	})
	if !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("RegisterSession() error = %v, want ErrInvalidSession", err)
	}
}

func TestReadSessionRecordRejectsMismatchedBmuxPaneTarget(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Host:        SessionHostBmux,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		SessionName: "comment-reviewer-abc123",
		Runtime:     "claude",
		Now:         time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	record.PaneTarget, err = BmuxSocketPathForSession(paths, "comment-other-abc123")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionRecord(paths, record); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSessionRecord(paths, record.SessionID); err == nil {
		t.Fatal("expected mismatched bmux pane target to be rejected")
	}
}

func TestRegisterSessionRejectsReservedOrMismatchedScopes(t *testing.T) {
	cases := []RegisterSessionOptions{
		{ScopeType: "profile", ScopeID: "max.other"},
		{ScopeType: "doc", ScopeID: "abc123"},
		{ScopeType: "message", ScopeID: "msg_abcdefghijklmnopqrst"},
	}
	for _, tc := range cases {
		t.Run(tc.ScopeType+"-"+tc.ScopeID, func(t *testing.T) {
			paths := testDaemonPaths(t)
			tc.Paths = paths
			tc.Profile = "max.reviewer"
			tc.BotName = "reviewer"
			tc.BotletsHome = filepath.Join(paths.Home, "botlets")
			if _, err := RegisterSession(tc); !errors.Is(err, ErrInvalidSession) {
				t.Fatalf("RegisterSession() error = %v, want ErrInvalidSession", err)
			}
		})
	}
}

func TestVerifySessionCapability(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
	})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := ReadCapability(record.CapabilityFile)
	if err != nil {
		t.Fatal(err)
	}
	auth := SocketAuth{
		Mode:              "session",
		Capability:        capability,
		Profile:           &record.Profile,
		SessionID:         &record.SessionID,
		SessionGeneration: &record.Generation,
	}
	if _, err := VerifySessionCapability(paths, auth); err != nil {
		t.Fatal(err)
	}
	auth.Capability = "cap_wrongcapabilitytoken"
	if _, err := VerifySessionCapability(paths, auth); err == nil {
		t.Fatal("expected invalid capability")
	}
}

func TestVerifySessionCapabilityAcceptsCanonicalHomeForSymlinkedParent(t *testing.T) {
	linkedPaths, canonicalPaths := symlinkedSessionHomePaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       linkedPaths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(canonicalPaths.Home, "botlets"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.CapabilityFile == sessionCapabilityPath(canonicalPaths, record.Profile, record.SessionID, record.Generation) {
		t.Fatal("test did not create distinct linked and canonical capability paths")
	}
	capability, err := ReadCapability(record.CapabilityFile)
	if err != nil {
		t.Fatal(err)
	}
	auth := SocketAuth{
		Mode:              "session",
		Capability:        capability,
		Profile:           &record.Profile,
		SessionID:         &record.SessionID,
		SessionGeneration: &record.Generation,
	}
	loaded, err := ReadSessionRecord(canonicalPaths, record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CapabilityFile != record.CapabilityFile {
		t.Fatalf("canonical read rewrote capability path: %+v", loaded)
	}
	if _, err := VerifySessionCapability(canonicalPaths, auth); err != nil {
		t.Fatalf("canonical home should verify linked-parent capability: %v", err)
	}
}

func TestVerifySessionCapabilityAcceptsStartingState(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		State:       "starting",
	})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := ReadCapability(record.CapabilityFile)
	if err != nil {
		t.Fatal(err)
	}
	auth := SocketAuth{
		Mode:              "session",
		Capability:        capability,
		Profile:           &record.Profile,
		SessionID:         &record.SessionID,
		SessionGeneration: &record.Generation,
	}
	if _, err := VerifySessionCapability(paths, auth); err != nil {
		t.Fatal(err)
	}
}

func TestVerifySessionCapabilityRejectsInactiveState(t *testing.T) {
	for _, state := range []string{"stale", "dead"} {
		t.Run(state, func(t *testing.T) {
			paths := testDaemonPaths(t)
			record, err := RegisterSession(RegisterSessionOptions{
				Paths:       paths,
				Profile:     "max.reviewer",
				BotName:     "reviewer",
				ScopeType:   "profile",
				ScopeID:     "max.reviewer",
				BotletsHome: filepath.Join(paths.Home, "botlets"),
			})
			if err != nil {
				t.Fatal(err)
			}
			capability, err := ReadCapability(record.CapabilityFile)
			if err != nil {
				t.Fatal(err)
			}
			record.State = state
			data, err := json.MarshalIndent(record, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(sessionRecordPath(paths, record.SessionID), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadSessionRecord(paths, record.SessionID); err != nil {
				t.Fatalf("expected %s record to be readable: %v", state, err)
			}
			auth := SocketAuth{
				Mode:              "session",
				Capability:        capability,
				Profile:           &record.Profile,
				SessionID:         &record.SessionID,
				SessionGeneration: &record.Generation,
			}
			if _, err := VerifySessionCapability(paths, auth); err == nil {
				t.Fatalf("expected %s session capability rejection", state)
			}
		})
	}
}

func TestVerifySessionCapabilityRejectsUntrustedCapabilityFile(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
	})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := ReadCapability(record.CapabilityFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(record.CapabilityFile, 0o644); err != nil {
		t.Fatal(err)
	}
	auth := SocketAuth{
		Mode:              "session",
		Capability:        capability,
		Profile:           &record.Profile,
		SessionID:         &record.SessionID,
		SessionGeneration: &record.Generation,
	}
	if _, err := VerifySessionCapability(paths, auth); err == nil {
		t.Fatal("expected untrusted capability file error")
	}
}

func TestVerifySessionCapabilityRejectsHardlinkedCapabilityFile(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
	})
	if err != nil {
		t.Fatal(err)
	}
	capability, err := ReadCapability(record.CapabilityFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(record.CapabilityFile, filepath.Join(paths.Home, "session-linked.cap")); err != nil {
		t.Fatal(err)
	}
	auth := SocketAuth{
		Mode:              "session",
		Capability:        capability,
		Profile:           &record.Profile,
		SessionID:         &record.SessionID,
		SessionGeneration: &record.Generation,
	}
	if _, err := VerifySessionCapability(paths, auth); err == nil {
		t.Fatal("expected hardlinked capability file rejection")
	}
}

func TestVerifySessionCapabilityRejectsMalformedCapabilityFile(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
	})
	if err != nil {
		t.Fatal(err)
	}
	malformedCapability := "not-a-capability-token-with-length"
	if err := os.WriteFile(record.CapabilityFile, []byte(malformedCapability+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	auth := SocketAuth{
		Mode:              "session",
		Capability:        malformedCapability,
		Profile:           &record.Profile,
		SessionID:         &record.SessionID,
		SessionGeneration: &record.Generation,
	}
	if _, err := VerifySessionCapability(paths, auth); err == nil {
		t.Fatal("expected malformed capability file rejection")
	}
}

func TestReadSessionRecordRejectsMismatchedSessionID(t *testing.T) {
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
	})
	if err != nil {
		t.Fatal(err)
	}
	mismatchedID := "sess_abcdefghijklmnopqrst"
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionRecordPath(paths, mismatchedID), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSessionRecord(paths, mismatchedID); err == nil {
		t.Fatal("expected mismatched session id rejection")
	}
}

func TestReadSessionRecordRejectsUntrustedFile(t *testing.T) {
	paths := testDaemonPaths(t)
	sessionID := "sess_abcdefghijklmnopqrst"
	path := sessionRecordPath(paths, sessionID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"session_id":"sess_abcdefghijklmnopqrst"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSessionRecord(paths, sessionID); err == nil {
		t.Fatal("expected untrusted session file error")
	}
}

func TestRegisterSessionRejectsConcurrentDuplicateSessionID(t *testing.T) {
	paths := testDaemonPaths(t)
	sessionID := "sess_abcdefghijklmnopqrst"
	const attempts = 12
	start := make(chan struct{})
	var wg sync.WaitGroup
	successes := make(chan SessionRecord, attempts)
	failures := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			record, err := RegisterSession(RegisterSessionOptions{
				Paths:       paths,
				Profile:     "max.reviewer",
				BotName:     "reviewer",
				ScopeType:   "profile",
				ScopeID:     "max.reviewer",
				SessionID:   sessionID,
				Generation:  fmt.Sprintf("gen_%016d", i),
				BotletsHome: filepath.Join(paths.Home, "botlets"),
			})
			if err != nil {
				failures <- err
				return
			}
			successes <- record
		}(i)
	}
	close(start)
	wg.Wait()
	close(successes)
	close(failures)

	if len(successes) != 1 {
		t.Fatalf("successful duplicate registrations = %d, want 1", len(successes))
	}
	if len(failures) != attempts-1 {
		t.Fatalf("failed duplicate registrations = %d, want %d", len(failures), attempts-1)
	}
	records, err := ListSessionRecords(paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].SessionID != sessionID {
		t.Fatalf("records = %+v, want one record for %s", records, sessionID)
	}
}

func symlinkedSessionHomePaths(t *testing.T) (Paths, Paths) {
	t.Helper()
	root := privateTestDir(t, "comment-bus-symlink-home-")
	realParent := filepath.Join(root, "real-parent")
	linkParent := filepath.Join(root, "linked-parent")
	if err := os.MkdirAll(filepath.Join(realParent, "comment-home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(realParent, "comment-home"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Fatal(err)
	}
	linkedPaths, err := ResolvePaths(filepath.Join(linkParent, "comment-home"))
	if err != nil {
		t.Fatal(err)
	}
	canonicalHome, err := filepath.EvalSymlinks(linkedPaths.Home)
	if err != nil {
		t.Fatal(err)
	}
	canonicalPaths, err := ResolvePaths(canonicalHome)
	if err != nil {
		t.Fatal(err)
	}
	if linkedPaths.Home == canonicalPaths.Home {
		t.Fatalf("linked home did not differ from canonical home: %s", linkedPaths.Home)
	}
	return linkedPaths, canonicalPaths
}
