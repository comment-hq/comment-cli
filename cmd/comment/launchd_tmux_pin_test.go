//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// The interactive tmux attach must resolve the tmux binary the daemon for the
// SELECTED home was pinned to (a service-baked COMMENT_IO_TMUX_BIN), not the
// default home's service config. Now that tmux is the default runtime host, a
// `comment run --home <custom>` attach that read the default home's (unpinned)
// service would exec bare tmux and fail even though the daemon created the
// session fine. (Regression for Codex review of the tmux-default change.)
func TestClientTmuxBinaryHonorsSelectedHomeServicePin(t *testing.T) {
	if !launchdSupported() {
		t.Skip("launchd-only test")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(commentbus.TmuxBinaryEnv, "") // no shell override; the service pin must win
	// A non-default selected home, as `comment run --home <custom>` would use. Its
	// daemon baked a tmux pin; the default home has no installed service.
	selected, err := resolveCLIPaths(filepath.Join(home, "custom-home"))
	if err != nil {
		t.Fatal(err)
	}
	dir, err := userLaunchAgentsDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The pin must be a real, resolver-acceptable executable (the daemon resolver
	// rejects shell-script shims and non-existent paths), so symlink a real binary.
	const pinTarget = "/bin/echo"
	if _, statErr := os.Stat(pinTarget); statErr != nil {
		t.Skipf("fixture %s unavailable: %v", pinTarget, statErr)
	}
	pin := filepath.Join(home, "pinned-tmux")
	if err := os.Symlink(pinTarget, pin); err != nil {
		t.Fatal(err)
	}
	label := launchdLabelForHome(selected.Home)
	plist := buildLaunchAgentPlist(launchAgentConfig{
		Label:            label,
		Home:             selected.Home,
		TmuxBinary:       pin,
		BinaryPath:       "/usr/local/bin/comment",
		ProgramArguments: []string{"/usr/local/bin/comment", "bus", "run", "--home", selected.Home},
	})
	if err := os.WriteFile(filepath.Join(dir, label+".plist"), []byte(plist), 0o644); err != nil {
		t.Fatal(err)
	}
	got := clientTmuxBinary(selected)
	if got == "tmux" {
		t.Fatalf("clientTmuxBinary(selected home) returned bare tmux; the selected home's service pin was not honored")
	}
	gotInfo, err := os.Stat(got)
	if err != nil {
		t.Fatalf("resolved tmux %q is not usable: %v", got, err)
	}
	pinInfo, err := os.Stat(pin)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(gotInfo, pinInfo) {
		t.Fatalf("clientTmuxBinary(selected) = %q, want the pinned tmux (%s -> %s)", got, pin, pinTarget)
	}
}

// comment uninstall's fallback session cleanup must use the daemon's pinned
// tmux (read from the service definition), not just the shell env, or it can
// fail to kill sessions when tmux lives only at the persisted pin. (Regression
// for Codex review of #540.)
func TestUninstallControllerHonorsTmuxPin(t *testing.T) {
	c := newUninstallTmuxController("/custom/tmux")
	ec, ok := c.(commentbus.ExecTmuxController)
	if !ok {
		t.Fatalf("unexpected controller type %T", c)
	}
	if ec.Binary != "/custom/tmux" {
		t.Fatalf("uninstall controller Binary = %q, want /custom/tmux", ec.Binary)
	}
}

// An explicit COMMENT_IO_TMUX_BIN pin set at install time must be baked into the
// generated service environment, since a shell-level value is not inherited by
// launchd/systemd services. (Regression for Codex review of #540.)
func TestInstalledServicePersistsTmuxPin(t *testing.T) {
	t.Setenv("COMMENT_IO_TMUX_BIN", "/opt/homebrew/bin/tmux")
	t.Setenv("COMMENT_IO_BMUX_BIN", "/opt/homebrew/bin/bmux")
	pin := installTmuxBinaryPin()
	if pin != "/opt/homebrew/bin/tmux" {
		t.Fatalf("installTmuxBinaryPin() = %q, want /opt/homebrew/bin/tmux", pin)
	}
	bmuxPin := installBmuxBinaryPin()
	if bmuxPin != "/opt/homebrew/bin/bmux" {
		t.Fatalf("installBmuxBinaryPin() = %q, want /opt/homebrew/bin/bmux", bmuxPin)
	}

	plist := buildLaunchAgentPlist(launchAgentConfig{
		Label:            "io.comment.test",
		Home:             "/home/.comment-io",
		TmuxBinary:       pin,
		BmuxBinary:       bmuxPin,
		ProgramArguments: []string{"/usr/local/bin/comment", "bus", "run", "--home", "/home/.comment-io"},
		StdoutPath:       "/home/.comment-io/logs/out.log",
		StderrPath:       "/home/.comment-io/logs/err.log",
	})
	if !strings.Contains(plist, "COMMENT_IO_TMUX_BIN") || !strings.Contains(plist, "/opt/homebrew/bin/tmux") {
		t.Fatalf("launchd plist missing tmux pin env:\n%s", plist)
	}
	if !strings.Contains(plist, "COMMENT_IO_BMUX_BIN") || !strings.Contains(plist, "/opt/homebrew/bin/bmux") {
		t.Fatalf("launchd plist missing bmux pin env:\n%s", plist)
	}

	unit := buildSystemdUnit(systemdServiceConfig{
		Label:            "io.comment.test",
		UnitName:         "io.comment.test.service",
		Home:             "/home/.comment-io",
		TmuxBinary:       pin,
		BmuxBinary:       bmuxPin,
		BinaryPath:       "/usr/local/bin/comment",
		UnitPath:         "/home/.config/systemd/user/io.comment.test.service",
		StdoutPath:       "/home/.comment-io/logs/out.log",
		StderrPath:       "/home/.comment-io/logs/err.log",
		ProgramArguments: []string{"/usr/local/bin/comment", "bus", "run", "--home", "/home/.comment-io"},
	})
	if !strings.Contains(unit, "COMMENT_IO_TMUX_BIN=/opt/homebrew/bin/tmux") {
		t.Fatalf("systemd unit missing tmux pin env:\n%s", unit)
	}
	if !strings.Contains(unit, "COMMENT_IO_BMUX_BIN=/opt/homebrew/bin/bmux") {
		t.Fatalf("systemd unit missing bmux pin env:\n%s", unit)
	}
}

// Without an explicit absolute pin, the service must not write a tmux env var —
// the daemon falls back to trusted-directory auto-discovery. Bare names and the
// "tmux" default are not persisted.
func TestInstalledServiceOmitsTmuxPinWhenUnset(t *testing.T) {
	t.Setenv("COMMENT_IO_TMUX_BIN", "")
	t.Setenv("COMMENT_IO_BMUX_BIN", "")
	if pin := installTmuxBinaryPin(); pin != "" {
		t.Fatalf("installTmuxBinaryPin() = %q, want empty when unset", pin)
	}
	if pin := installBmuxBinaryPin(); pin != "" {
		t.Fatalf("installBmuxBinaryPin() = %q, want empty when unset", pin)
	}
	// A non-absolute value is also not persisted.
	t.Setenv("COMMENT_IO_TMUX_BIN", "tmux")
	t.Setenv("COMMENT_IO_BMUX_BIN", "bmux")
	if pin := installTmuxBinaryPin(); pin != "" {
		t.Fatalf("installTmuxBinaryPin() = %q, want empty for bare name", pin)
	}
	if pin := installBmuxBinaryPin(); pin != "" {
		t.Fatalf("installBmuxBinaryPin() = %q, want empty for bare name", pin)
	}

	plist := buildLaunchAgentPlist(launchAgentConfig{
		Label:            "io.comment.test",
		Home:             "/home/.comment-io",
		TmuxBinary:       installTmuxBinaryPin(),
		ProgramArguments: []string{"/usr/local/bin/comment", "bus", "run"},
	})
	if strings.Contains(plist, "COMMENT_IO_TMUX_BIN") {
		t.Fatalf("launchd plist should omit tmux pin when unset:\n%s", plist)
	}
	if strings.Contains(plist, "COMMENT_IO_BMUX_BIN") {
		t.Fatalf("launchd plist should omit bmux pin when unset:\n%s", plist)
	}
}

// doctor must read the pin from the installed service definition (not the
// invoking shell). Round-trip: what the installer writes, the extractor reads.
// (Regression for Codex review of #540.)
func TestExtractTmuxPinFromServiceDefinitions(t *testing.T) {
	const pin = "/opt/homebrew/bin/tmux"
	const bmuxPin = "/opt/homebrew/bin/bmux"
	plist := buildLaunchAgentPlist(launchAgentConfig{
		Home:             "/home/.comment-io",
		TmuxBinary:       pin,
		BmuxBinary:       bmuxPin,
		ProgramArguments: []string{"/usr/local/bin/comment", "bus", "run"},
	})
	if got := extractTmuxPinFromPlist([]byte(plist)); got != pin {
		t.Fatalf("plist pin = %q, want %q", got, pin)
	}
	if got := extractBmuxPinFromPlist([]byte(plist)); got != bmuxPin {
		t.Fatalf("plist bmux pin = %q, want %q", got, bmuxPin)
	}
	unit := buildSystemdUnit(systemdServiceConfig{
		Home:             "/home/.comment-io",
		TmuxBinary:       pin,
		BmuxBinary:       bmuxPin,
		ProgramArguments: []string{"/usr/local/bin/comment", "bus", "run"},
	})
	if got := extractTmuxPinFromSystemd([]byte(unit)); got != pin {
		t.Fatalf("systemd pin = %q, want %q", got, pin)
	}
	if got := extractBmuxPinFromSystemd([]byte(unit)); got != bmuxPin {
		t.Fatalf("systemd bmux pin = %q, want %q", got, bmuxPin)
	}
	// No pin baked in → extractors return empty.
	plistNoPin := buildLaunchAgentPlist(launchAgentConfig{
		Home:             "/home/.comment-io",
		ProgramArguments: []string{"/usr/local/bin/comment", "bus", "run"},
	})
	if got := extractTmuxPinFromPlist([]byte(plistNoPin)); got != "" {
		t.Fatalf("plist with no pin = %q, want empty", got)
	}
	if got := extractBmuxPinFromPlist([]byte(plistNoPin)); got != "" {
		t.Fatalf("plist with no bmux pin = %q, want empty", got)
	}
	unitNoPin := buildSystemdUnit(systemdServiceConfig{
		Home:             "/home/.comment-io",
		ProgramArguments: []string{"/usr/local/bin/comment", "bus", "run"},
	})
	if got := extractTmuxPinFromSystemd([]byte(unitNoPin)); got != "" {
		t.Fatalf("systemd with no pin = %q, want empty", got)
	}
	if got := extractBmuxPinFromSystemd([]byte(unitNoPin)); got != "" {
		t.Fatalf("systemd with no bmux pin = %q, want empty", got)
	}
}

// Pins with characters the service writers escape (XML & in plists, %% in
// systemd) must round-trip exactly, or doctor/uninstall resolve the wrong path.
// (Regression for Codex review of #540.)
func TestExtractTmuxPinRoundTripsEscapedCharacters(t *testing.T) {
	const plistPin = "/tmp/a&b/tmux" // xml.EscapeText -> a&amp;b
	plist := buildLaunchAgentPlist(launchAgentConfig{
		Home:             "/home/.comment-io",
		TmuxBinary:       plistPin,
		ProgramArguments: []string{"/usr/local/bin/comment", "bus", "run"},
	})
	if got := extractTmuxPinFromPlist([]byte(plist)); got != plistPin {
		t.Fatalf("plist round-trip = %q, want %q", got, plistPin)
	}

	const systemdPin = "/opt/tmux%stable/bin/tmux" // systemdQuoteArg -> %%stable
	unit := buildSystemdUnit(systemdServiceConfig{
		Home:             "/home/.comment-io",
		TmuxBinary:       systemdPin,
		ProgramArguments: []string{"/usr/local/bin/comment", "bus", "run"},
	})
	if got := extractTmuxPinFromSystemd([]byte(unit)); got != systemdPin {
		t.Fatalf("systemd round-trip = %q, want %q", got, systemdPin)
	}
}

// doctor must mirror the owning daemon: a pinned service uses its pin; an
// unpinned-but-installed service auto-discovers (ignoring the shell's
// COMMENT_IO_TMUX_BIN, which it does not inherit); only a foreground/no-service
// context defers to the shell env. (Regression for Codex review of #540.)
func TestEffectiveTmuxResolveInput(t *testing.T) {
	cases := []struct {
		pin       string
		exists    bool
		wantInput string
	}{
		{"/custom/tmux", true, "/custom/tmux"}, // pinned service
		{"/custom/tmux", false, "/custom/tmux"},
		{"", true, "tmux"}, // installed, unpinned -> force auto-discovery, bypass shell env
		{"", false, ""},    // no service -> defer to shell COMMENT_IO_TMUX_BIN
	}
	for _, c := range cases {
		if got := effectiveTmuxResolveInput(c.pin, c.exists); got != c.wantInput {
			t.Errorf("effectiveTmuxResolveInput(%q,%v) = %q, want %q", c.pin, c.exists, got, c.wantInput)
		}
	}
}

func TestEffectiveBmuxResolveInput(t *testing.T) {
	cases := []struct {
		pin       string
		exists    bool
		wantInput string
	}{
		{"/custom/bmux", true, "/custom/bmux"},
		{"/custom/bmux", false, "/custom/bmux"},
		{"", true, "bmux"},
		{"", false, ""},
	}
	for _, c := range cases {
		if got := effectiveBmuxResolveInput(c.pin, c.exists); got != c.wantInput {
			t.Errorf("effectiveBmuxResolveInput(%q,%v) = %q, want %q", c.pin, c.exists, got, c.wantInput)
		}
	}
}
