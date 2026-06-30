//go:build darwin || linux

package commentbus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #1420 Bug 1: managed Claude sessions must not be launched with the
// unknown `--agent <bot>` flag. Botlets are not Claude subagents; Claude Code
// >= 2.1.x hard-errors with `--agent '<bot>' not found`, which killed the
// runtime ~1s after launch and drove the daemon into an infinite relaunch loop.
func TestManagedSessionRuntimeCommandForLaunchOmitsAgentFlag(t *testing.T) {
	const ref = "123e4567-e89b-42d3-a456-426614174000"
	cases := []struct {
		name   string
		ref    string
		launch string
		want   []string
	}{
		{"fresh without ref", "", managedSessionLaunchFresh, []string{"claude", "--dangerously-skip-permissions"}},
		{"fresh with ref", ref, managedSessionLaunchFresh, []string{"claude", "--session-id", ref, "--dangerously-skip-permissions"}},
		{"resume", ref, managedSessionLaunchResume, []string{"claude", "--resume", ref, "--dangerously-skip-permissions"}},
	}
	for _, tc := range cases {
		got := managedSessionRuntimeCommandForLaunch("claude", "landing", tc.ref, tc.launch)
		for _, arg := range got {
			if arg == "--agent" {
				t.Fatalf("%s: command still passes --agent: %#v", tc.name, got)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("%s: got %#v, want %#v", tc.name, got, tc.want)
		}
	}
}

// The launch-shape validator must accept the new agentless Claude commands on
// both the default (tmux) host and the bmux host (which always pins a runtime
// session ref).
func TestManagedSessionRuntimeCommandMatchesAcceptsAgentlessClaude(t *testing.T) {
	const ref = "123e4567-e89b-42d3-a456-426614174000"
	cases := []struct {
		name   string
		record SessionRecord
	}{
		{"tmux fresh", SessionRecord{
			Runtime:        "claude",
			BotName:        "landing",
			RuntimeCommand: []string{"claude", "--dangerously-skip-permissions"},
		}},
		{"bmux pinned", SessionRecord{
			Host:              SessionHostBmux,
			Runtime:           "claude",
			BotName:           "landing",
			RuntimeSessionRef: ref,
			RuntimeCommand:    []string{"claude", "--session-id", ref, "--dangerously-skip-permissions"},
		}},
		{"bmux resume", SessionRecord{
			Host:              SessionHostBmux,
			Runtime:           "claude",
			BotName:           "landing",
			RuntimeSessionRef: ref,
			RuntimeCommand:    []string{"claude", "--resume", ref, "--dangerously-skip-permissions"},
		}},
	}
	for _, tc := range cases {
		if !managedSessionRuntimeCommandMatches(tc.record) {
			t.Fatalf("%s: agentless Claude command did not validate: %#v", tc.name, tc.record.RuntimeCommand)
		}
	}
}

// Existing on-disk records written before this fix still carry `--agent`. The
// validator must keep accepting the legacy shapes so they read cleanly (and get
// regenerated agentless on relaunch) rather than poisoning the session read.
func TestManagedSessionRuntimeCommandMatchesStillAcceptsLegacyAgentShapes(t *testing.T) {
	const ref = "123e4567-e89b-42d3-a456-426614174000"
	cases := []struct {
		name   string
		record SessionRecord
	}{
		{"tmux legacy --agent", SessionRecord{
			Runtime:        "claude",
			BotName:        "reviewer",
			RuntimeCommand: []string{"claude", "--agent", "reviewer", "--dangerously-skip-permissions"},
		}},
		{"bmux pinned --agent", SessionRecord{
			Host:              SessionHostBmux,
			Runtime:           "claude",
			BotName:           "reviewer",
			RuntimeSessionRef: ref,
			RuntimeCommand:    []string{"claude", "--session-id", ref, "--agent", "reviewer", "--dangerously-skip-permissions"},
		}},
	}
	for _, tc := range cases {
		if !managedSessionRuntimeCommandMatches(tc.record) {
			t.Fatalf("%s: legacy --agent command should still validate: %#v", tc.name, tc.record.RuntimeCommand)
		}
	}
}

func TestRunSessionExecNormalizesLegacyClaudeAgentCommand(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh") // deterministic for shell launch mode.

	cases := []struct {
		name       string
		launchMode string
		command    []string
		want       []string
	}{
		{
			name:       "path mode preserves legacy bypass",
			launchMode: RuntimeLaunchModePath,
			command:    []string{"claude", "--agent", "reviewer", "--dangerously-skip-permissions"},
			want:       []string{"claude", "--dangerously-skip-permissions"},
		},
		{
			name:       "shell mode preserves legacy bypass",
			launchMode: RuntimeLaunchModeShell,
			command:    []string{"claude", "--agent", "reviewer", "--dangerously-skip-permissions"},
			want:       []string{"claude", "--dangerously-skip-permissions"},
		},
		{
			name:       "path mode does not add bypass",
			launchMode: RuntimeLaunchModePath,
			command:    []string{"claude", "--agent", "reviewer"},
			want:       []string{"claude"},
		},
		{
			name:       "shell mode does not add bypass",
			launchMode: RuntimeLaunchModeShell,
			command:    []string{"claude", "--agent", "reviewer"},
			want:       []string{"claude"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			paths := testDaemonPaths(t)
			record, err := RegisterSession(RegisterSessionOptions{
				Paths:       paths,
				Profile:     "max.reviewer",
				BotName:     "reviewer",
				ScopeType:   "profile",
				ScopeID:     "max.reviewer",
				BotletsHome: filepath.Join(paths.Home, "botlets"),
				SessionName: "comment-reviewer-abc123",
				PaneTarget:  "comment-reviewer-abc123:0.0",
				State:       "starting",
			})
			if err != nil {
				t.Fatal(err)
			}
			record.RuntimeCommand = append([]string(nil), tc.command...)
			record.RuntimeLaunchMode = tc.launchMode

			resolvedClaudePath := ""
			if tc.launchMode == RuntimeLaunchModePath {
				binDir := filepath.Join(paths.Home, "bin")
				if err := os.MkdirAll(binDir, 0o700); err != nil {
					t.Fatal(err)
				}
				claudePath := filepath.Join(binDir, "claude")
				if err := os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
				resolvedClaudePath, err = filepath.EvalSymlinks(claudePath)
				if err != nil {
					t.Fatal(err)
				}
				record.RuntimePath = resolvedClaudePath
				record.RuntimeCommandPath = claudePath
			}

			if err := WriteSessionRecord(paths, record); err != nil {
				t.Fatal(err)
			}
			rawBefore, err := os.ReadFile(sessionRecordPath(paths, record.SessionID))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(rawBefore), "--agent") {
				t.Fatalf("test fixture did not persist legacy --agent command:\n%s", rawBefore)
			}

			loaded, err := ReadSessionRecord(paths, record.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(loaded.RuntimeCommand) != fmt.Sprint(tc.want) {
				t.Fatalf("loaded command = %#v, want %#v", loaded.RuntimeCommand, tc.want)
			}
			rawAfter, err := os.ReadFile(sessionRecordPath(paths, record.SessionID))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(rawAfter), "--agent") {
				t.Fatalf("ReadSessionRecord rewrote the legacy command on disk:\n%s", rawAfter)
			}

			var execPath string
			var execArgv []string
			sentinel := errors.New("exec sentinel")
			err = RunSessionExec(SessionExecOptions{
				Paths:      paths,
				SessionID:  record.SessionID,
				Generation: record.Generation,
				Environ:    []string{"PATH=/usr/bin:/bin"},
				LookPath:   func(name string) (string, error) { return "", errors.New("lookpath should not be called") },
				Exec: func(path string, argv []string, env []string) error {
					execPath = path
					execArgv = append([]string{}, argv...)
					return sentinel
				},
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("RunSessionExec error = %v", err)
			}
			if strings.Contains(strings.Join(execArgv, " "), "--agent") {
				t.Fatalf("RunSessionExec executed legacy --agent argv: path=%q argv=%#v", execPath, execArgv)
			}
			if tc.launchMode == RuntimeLaunchModePath {
				wantArgv := append([]string{resolvedClaudePath}, tc.want[1:]...)
				if execPath != resolvedClaudePath || fmt.Sprint(execArgv) != fmt.Sprint(wantArgv) {
					t.Fatalf("path-mode exec = %q %#v, want %q %#v", execPath, execArgv, resolvedClaudePath, wantArgv)
				}
				return
			}
			wantShellArgvSuffix := tc.want
			if execPath != "/bin/sh" || len(execArgv) < 4 || fmt.Sprint(execArgv[3:]) != fmt.Sprint(wantShellArgvSuffix) {
				t.Fatalf("shell-mode exec = %q %#v, want /bin/sh launcher with normalized Claude args", execPath, execArgv)
			}
		})
	}
}

// Issue #1420 Bug 2: a single malformed session record must be skipped by the
// lenient reader the daemon's status/liveness paths use, not abort the entire
// read. The relaunch loop produced thousands of dangling records; one of them
// previously failed the read for the whole batch, surfacing to clients as
// `UPSTREAM_ERROR: could not read sessions` and taking down `comment run` /
// `comment sessions status`.
func TestListSessionRecordsLenientSkipsInvalidRecord(t *testing.T) {
	paths := testDaemonPaths(t)
	good, err := RegisterSession(RegisterSessionOptions{
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
	// A well-formed filename whose JSON is missing required fields: it passes
	// file-trust checks but fails validateSessionRecord, exactly the kind of
	// poisoned record the relaunch loop leaked.
	badID := "sess_bad000000000000000000"
	if !LocalSessionIDRE.MatchString(badID) {
		t.Fatalf("test fixture id %q is not a valid session id", badID)
	}
	badJSON := `{"session_id":"` + badID + `","profile":"` + good.Profile + `","bot_name":"` + good.BotName + `"}`
	if err := WritePrivateFileAtomic(sessionRecordPath(paths, badID), []byte(badJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	records, skipped, err := ListSessionRecordsLenient(paths)
	if err != nil {
		t.Fatalf("ListSessionRecordsLenient aborted on a single bad record: %v", err)
	}
	if len(records) != 1 || records[0].SessionID != good.SessionID {
		t.Fatalf("records = %+v, want only the valid record %s", records, good.SessionID)
	}
	if len(skipped) != 1 || skipped[0] != badID {
		t.Fatalf("skipped = %+v, want [%s]", skipped, badID)
	}

	// The strict reader (used by safety-sensitive callers like uninstall) must
	// still fail closed on the same poisoned record.
	if _, err := ListSessionRecords(paths); err == nil {
		t.Fatal("ListSessionRecords should still fail closed on an invalid record")
	}
}

func TestSelectSessionForMutationFailsClosedOnPoisonedRecord(t *testing.T) {
	paths := testDaemonPaths(t)
	good, err := RegisterSession(RegisterSessionOptions{
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
	badID := "sess_badmut00000000000000"
	if !LocalSessionIDRE.MatchString(badID) {
		t.Fatalf("test fixture id %q is not a valid session id", badID)
	}
	badJSON := `{"session_id":"` + badID + `","profile":"` + good.Profile + `","bot_name":"` + good.BotName + `"}`
	if err := WritePrivateFileAtomic(sessionRecordPath(paths, badID), []byte(badJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	if records, skipped, err := ListSessionRecordsLenient(paths); err != nil || len(records) != 1 || records[0].SessionID != good.SessionID || len(skipped) != 1 || skipped[0] != badID {
		t.Fatalf("lenient read fixture = records:%+v skipped:%+v err:%v, want good record and skipped poisoned record", records, skipped, err)
	}

	profile := good.Profile
	daemon := &Daemon{paths: paths}
	_, sockErr := daemon.selectSessionForMutation(SocketRequest{
		ID:     "req_stop_poisoned",
		Op:     "sessions.stop",
		Auth:   &SocketAuth{Mode: "owner", Capability: "owner-capability", Profile: &profile},
		Params: map[string]any{"profile": good.Profile, "bot": good.BotName},
	})
	if sockErr == nil || sockErr.Code != "UPSTREAM_ERROR" {
		t.Fatalf("selectSessionForMutation error = %+v, want UPSTREAM_ERROR", sockErr)
	}
}
