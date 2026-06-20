//go:build darwin || linux

package main

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

func TestCheckDaemonRunningTCPModeSkipsNativeFix(t *testing.T) {
	// In TCP/caged mode with no daemon reachable, --fix must NOT run the native
	// busInstall (which can't configure the TCP transport and could start a host
	// daemon against the cage's state). It should warn and point at the external
	// runtime instead. 127.0.0.1:1 has nothing listening -> daemonHealthy false.
	paths := commentbus.Paths{
		Socket:     filepath.Join(t.TempDir(), "daemon.sock"),
		BusTCPAddr: "127.0.0.1:1",
	}
	got := checkDaemonRunning(paths, true)
	if got.Status != "warn" {
		t.Fatalf("expected warn in TCP mode, got %q: %s", got.Status, got.Message)
	}
	if !strings.Contains(got.Message, "external runtime") {
		t.Fatalf("expected external-runtime guidance, got %q", got.Message)
	}
}

func TestResolveCLIPathsScopesTCPToEnvHome(t *testing.T) {
	t.Setenv("COMMENT_IO_HOME", "/tmp/cage-home")
	t.Setenv("COMMENT_IO_BUS_TCP_ADDR", "127.0.0.1:7700")

	// No --home → uses COMMENT_IO_HOME → TCP applies.
	if p, err := resolveCLIPaths(""); err != nil {
		t.Fatal(err)
	} else if p.BusTCPAddr != "127.0.0.1:7700" {
		t.Fatalf("env home: want TCP set, got %q", p.BusTCPAddr)
	}
	// --home == COMMENT_IO_HOME → TCP applies.
	if p, err := resolveCLIPaths("/tmp/cage-home"); err != nil {
		t.Fatal(err)
	} else if p.BusTCPAddr != "127.0.0.1:7700" {
		t.Fatalf("matching --home: want TCP set, got %q", p.BusTCPAddr)
	}
	// --home that resolves to COMMENT_IO_HOME but isn't byte-identical (trailing
	// slash) → still matches after cleaning → TCP applies.
	if p, err := resolveCLIPaths("/tmp/cage-home/"); err != nil {
		t.Fatal(err)
	} else if p.BusTCPAddr != "127.0.0.1:7700" {
		t.Fatalf("trailing-slash --home: want TCP set, got %q", p.BusTCPAddr)
	}
	// --home pointing at a DIFFERENT (native) daemon → no TCP override.
	if p, err := resolveCLIPaths("/tmp/other-home"); err != nil {
		t.Fatal(err)
	} else if p.BusTCPAddr != "" {
		t.Fatalf("different --home: TCP must NOT apply, got %q", p.BusTCPAddr)
	}
}

func TestDaemonExternallyManagedScopedToHome(t *testing.T) {
	t.Setenv("COMMENT_IO_DAEMON_EXTERNAL", "1")
	t.Setenv("COMMENT_IO_HOME", "/tmp/cage-home")
	if !daemonExternallyManaged(commentbus.Paths{Home: "/tmp/cage-home"}) {
		t.Fatal("marker should apply to the env-configured home")
	}
	if daemonExternallyManaged(commentbus.Paths{Home: "/tmp/other-home"}) {
		t.Fatal("marker must NOT apply to a different --home (native daemon)")
	}
	// A scoped TCP addr is itself sufficient (resolveCLIPaths only sets it for the
	// env home).
	if !daemonExternallyManaged(commentbus.Paths{Home: "/tmp/other-home", BusTCPAddr: "127.0.0.1:7700"}) {
		t.Fatal("a set BusTCPAddr should mark external")
	}
}

func TestCheckDaemonExternalMarkerSkipsNativeInstall(t *testing.T) {
	// Linux caged Unix-socket mode: no TCP addr, but COMMENT_IO_DAEMON_EXTERNAL=1
	// (scoped to COMMENT_IO_HOME) marks the daemon as container-managed. doctor
	// --fix must NOT run a native install/start for either the installed or
	// running check.
	home := filepath.Join(t.TempDir(), ".comment-io")
	t.Setenv("COMMENT_IO_DAEMON_EXTERNAL", "1")
	t.Setenv("COMMENT_IO_HOME", home)
	paths := commentbus.Paths{
		Home:   home,
		Socket: filepath.Join(t.TempDir(), "daemon.sock"), // nothing listening
	}
	installed := checkDaemonInstalled(paths, true)
	if installed.Status != "ok" || !strings.Contains(installed.Message, "external runtime") {
		t.Fatalf("installed: expected external-runtime ok, got %q: %s", installed.Status, installed.Message)
	}
	running := checkDaemonRunning(paths, true)
	if running.Status != "warn" || !strings.Contains(running.Message, "external runtime") {
		t.Fatalf("running: expected external-runtime warn, got %q: %s", running.Status, running.Message)
	}
}

func TestDoctorCheckAgentsDirMissingWarnsWithoutFix(t *testing.T) {
	home := t.TempDir()
	paths := commentbus.Paths{Home: filepath.Join(home, ".comment-io")}

	got := checkAgentsDir(paths, false)
	if got.Status != "warn" {
		t.Fatalf("expected warn for missing dir, got %#v", got)
	}
}

func TestDoctorCheckAgentsDirMissingFixedWithFix(t *testing.T) {
	home := t.TempDir()
	paths := commentbus.Paths{Home: filepath.Join(home, ".comment-io")}

	got := checkAgentsDir(paths, true)
	if got.Status != "fixed" || !got.FixApplied {
		t.Fatalf("expected fixed status, got %#v", got)
	}
	info, err := os.Stat(filepath.Join(paths.Home, "agents"))
	if err != nil {
		t.Fatalf("agents dir not created: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("created agents dir mode = %o, want 0700", info.Mode().Perm())
	}
}

func TestDoctorCheckAgentsDirGroupWritableFixed(t *testing.T) {
	home := t.TempDir()
	paths := commentbus.Paths{Home: filepath.Join(home, ".comment-io")}
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(agentsDir, 0o775); err != nil {
		t.Fatal(err)
	}

	got := checkAgentsDir(paths, true)
	if got.Status != "fixed" {
		t.Fatalf("expected fixed for group-writable dir, got %#v", got)
	}
	info, err := os.Stat(agentsDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("agents dir mode after fix = %o, want 0700", info.Mode().Perm())
	}
}

func TestDoctorCheckAgentProfileFilesWorldReadableFixed(t *testing.T) {
	home := t.TempDir()
	paths := commentbus.Paths{Home: filepath.Join(home, ".comment-io")}
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(agentsDir, "alice.bot.json")
	bad := filepath.Join(agentsDir, "max.personal.json")
	for _, p := range []string{good, bad} {
		if err := os.WriteFile(p, []byte(`{"agent_secret":"as_x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(bad, 0o644); err != nil {
		t.Fatal(err)
	}

	// Without --fix: should warn and identify the bad file.
	got := checkAgentProfileFiles(paths, false)
	if got.Status != "warn" {
		t.Fatalf("expected warn, got %#v", got)
	}
	encoded, _ := json.Marshal(got.Detail)
	if !contains(string(encoded), "max.personal.json") {
		t.Fatalf("warn detail should name max.personal.json: %s", encoded)
	}

	// With --fix: chmod 0600 and report fixed.
	fixed := checkAgentProfileFiles(paths, true)
	if fixed.Status != "fixed" || !fixed.FixApplied {
		t.Fatalf("expected fixed, got %#v", fixed)
	}
	info, err := os.Stat(bad)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("max.personal mode after fix = %o, want 0600", info.Mode().Perm())
	}
}

func TestDoctorCheckAgentProfileFilesAllPrivateIsOK(t *testing.T) {
	home := t.TempDir()
	paths := commentbus.Paths{Home: filepath.Join(home, ".comment-io")}
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "alice.bot.json"), []byte(`{"agent_secret":"as_x"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := checkAgentProfileFiles(paths, false)
	if got.Status != "ok" {
		t.Fatalf("expected ok, got %#v", got)
	}
}

func TestDoctorCheckAgentProfileFilesNoDirIsOK(t *testing.T) {
	home := t.TempDir()
	paths := commentbus.Paths{Home: filepath.Join(home, ".comment-io")}

	got := checkAgentProfileFiles(paths, false)
	if got.Status != "ok" {
		t.Fatalf("expected ok when agents dir missing, got %#v", got)
	}
}

func TestDoctorCheckBmuxUsesConfiguredBinary(t *testing.T) {
	dir := privateTempHome(t, "comment-doctor-bmux-")
	bmuxPath := filepath.Join(dir, "bmux")
	writeExecutableScript(t, bmuxPath, "#!/bin/sh\nif [ \"$1\" = \"-V\" ]; then echo 'bmux test'; exit 0; fi\nexit 1\n")
	t.Setenv(commentbus.BmuxBinaryEnv, bmuxPath)

	got := checkBmux(commentbus.Paths{Home: filepath.Join(dir, ".comment-io")}, false)
	if got.Status != "ok" {
		t.Fatalf("checkBmux status = %#v, want ok", got)
	}
	if detail, ok := got.Detail.(map[string]any); !ok || detail["path"] != bmuxPath || detail["version"] != "bmux test" {
		t.Fatalf("checkBmux detail = %#v", got.Detail)
	}
}

func TestBusInstallSilentDoesNotWriteToStdout(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	installFakeLaunchctl(t)
	bin := writeFakeCommentBinary(t)
	// Non-interactive: the pair chain must only add the pair_followup result
	// key, never print. (Unstubbed, go test's /dev/null stdin reads as a char
	// device, i.e. "interactive", and would enter the real pair flow.)
	stubBusInstallPair(t, false, func(home string) error {
		t.Fatalf("non-interactive bus install must not call the pair flow (home=%s)", home)
		return nil
	})

	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	result, runErr := busInstall(filepath.Join(userHome, ".comment-io"), "", bin, false, true)

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = oldStdout
	data, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatal(readErr)
	}

	if runErr != nil {
		t.Fatalf("busInstall failed: %v", runErr)
	}
	if len(data) != 0 {
		t.Fatalf("busInstall must not write to stdout (would produce two JSON docs in `comment doctor --fix`); got %q", string(data))
	}
	if result["installed"] != true || result["loaded"] != true {
		t.Fatalf("busInstall result missing installed/loaded: %#v", result)
	}
}

func TestClassifyPostReloadProfilesReturnsFixedWhenClean(t *testing.T) {
	got := classifyPostReloadProfiles("daemon_profiles",
		[]string{"max.bot"},
		daemonProfileInspection{Handles: []string{"max.bot"}, ReportsHandles: true},
	)
	if got.Status != "fixed" || !got.FixApplied {
		t.Fatalf("expected fixed, got %#v", got)
	}
}

func TestClassifyPostReloadProfilesReturnsErrorWhenHandlesStillMissing(t *testing.T) {
	got := classifyPostReloadProfiles("daemon_profiles",
		[]string{"max.bot", "max.other"},
		daemonProfileInspection{Handles: []string{"max.bot"}, ReportsHandles: true},
	)
	if got.Status != "error" {
		t.Fatalf("expected error when handles missing, got %#v", got)
	}
}

func TestClassifyPostReloadProfilesReturnsWarnWhenErrorsRemainAfterReload(t *testing.T) {
	// Regression for the doctor-PR review: all handles are loaded but the
	// daemon still reports profile_load_errors (e.g. WRITE_BUS_CONFIG_FAILED).
	// That state must surface as warn — not be hidden behind a `fixed` status.
	got := classifyPostReloadProfiles("daemon_profiles",
		[]string{"max.bot"},
		daemonProfileInspection{
			Handles:        []string{"max.bot"},
			ReportsHandles: true,
			Errors:         []any{map[string]any{"code": "WRITE_BUS_CONFIG_FAILED", "message": "could not persist daemon profile config"}},
		},
	)
	if got.Status != "warn" {
		t.Fatalf("expected warn when daemon reports load errors after reload, got %#v", got)
	}
	if !got.FixApplied {
		t.Fatalf("fix_applied should still be true; reload did run: %#v", got)
	}
}

func contains(haystack, needle string) bool {
	return indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	n, h := len(needle), len(haystack)
	if n == 0 {
		return 0
	}
	for i := 0; i+n <= h; i++ {
		if haystack[i:i+n] == needle {
			return i
		}
	}
	return -1
}

func TestParseTmuxMajorMinor(t *testing.T) {
	cases := []struct {
		in       string
		maj, min int
		ok       bool
	}{
		{"tmux 3.6a", 3, 6, true},
		{"tmux 3.2", 3, 2, true},
		{"tmux next-3.4", 3, 4, true},
		{"tmux 2.8", 2, 8, true},
		{"tmux 3.10", 3, 10, true},
		{"garbage", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		maj, min, ok := parseTmuxMajorMinor(c.in)
		if ok != c.ok || (ok && (maj != c.maj || min != c.min)) {
			t.Errorf("parseTmuxMajorMinor(%q) = (%d,%d,%v), want (%d,%d,%v)", c.in, maj, min, ok, c.maj, c.min, c.ok)
		}
	}
}
