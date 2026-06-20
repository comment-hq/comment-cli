//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// TestMain defaults the bmux auto-install hook to a no-op so the bmux-wiring
// tests that still exercise ensureBmuxInstalledFn directly don't reach the
// network. (`comment bus install` / `comment doctor` no longer call this hook —
// bmux is an explicit opt-in host and is not auto-installed — but the hook and
// these tests are retained so opt-in bmux re-wires trivially.) Tests override
// the hook locally and restore it via t.Cleanup.
//
// It also defaults busInstallStdinIsInteractive to non-interactive: `go test`
// hands the binary /dev/null as stdin, which IS a character device, so the real
// stdinIsInteractive() reports true — and a `bus install` test on an unpaired
// temp home would then chain straight into the real network device-pair flow
// and block until the device code expires (~10m), tripping the package test
// timeout. Pairing tests (launchd_pair_chain_test.go) override this hook locally
// and restore it.
func TestMain(m *testing.M) {
	ensureBmuxInstalledFn = func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		return commentbus.BmuxInstallResult{Path: "/stub/bmux", AlreadyPresent: true}, nil
	}
	busInstallStdinIsInteractive = func() bool { return false }
	os.Exit(m.Run())
}

func stubEnsureBmux(t *testing.T, fn func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error)) {
	t.Helper()
	prev := ensureBmuxInstalledFn
	ensureBmuxInstalledFn = fn
	t.Cleanup(func() { ensureBmuxInstalledFn = prev })
}

func TestEnsureBmuxForInstallDiscoverableNeedsNoPin(t *testing.T) {
	t.Setenv(commentbus.BmuxBinaryEnv, "")
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		return commentbus.BmuxInstallResult{Path: "/home/u/.local/bin/bmux", Installed: true, Discoverable: true}, nil
	})
	got, pin := ensureBmuxForInstall()
	if got["ok"] != true || got["installed"] != true || got["path"] != "/home/u/.local/bin/bmux" {
		t.Fatalf("summary = %#v", got)
	}
	if pin != "" {
		t.Fatalf("pin = %q, want empty for a daemon-discoverable install", pin)
	}
	if _, hasPin := got["service_pin"]; hasPin {
		t.Fatalf("discoverable install should not record a service_pin: %#v", got)
	}
}

func TestEnsureBmuxForInstallNonDiscoverablePins(t *testing.T) {
	t.Setenv(commentbus.BmuxBinaryEnv, "") // no operator pin already set
	// A real, resolver-acceptable absolute path: non-discoverable but pinnable.
	dir := privateTempHome(t, "comment-bus-bmux-pinnable-")
	installed := filepath.Join(dir, "bmux")
	writeExecutableScript(t, installed, "#!/bin/sh\nexit 0\n")
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		return commentbus.BmuxInstallResult{Path: installed, Installed: true, Discoverable: false}, nil
	})
	got, pin := ensureBmuxForInstall()
	if pin != installed {
		t.Fatalf("pin = %q, want %q (non-discoverable but pinnable install must be pinned)", pin, installed)
	}
	if got["service_pin"] != installed {
		t.Fatalf("summary service_pin = %#v", got["service_pin"])
	}
}

func TestEnsureBmuxForInstallRefusesUnusablePin(t *testing.T) {
	t.Setenv(commentbus.BmuxBinaryEnv, "")
	// A path the daemon resolver rejects (does not exist) must NOT be pinned; the
	// summary reports the install as not daemon-usable instead of baking a bad pin.
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		return commentbus.BmuxInstallResult{Path: "/nonexistent/dir/bmux", Installed: true, Discoverable: false}, nil
	})
	got, pin := ensureBmuxForInstall()
	if pin != "" {
		t.Fatalf("pin = %q, want empty (unusable path must not be pinned)", pin)
	}
	if got["ok"] != false {
		t.Fatalf("ok = %#v, want false for an unusable install", got["ok"])
	}
	if _, hasPin := got["service_pin"]; hasPin {
		t.Fatalf("unusable install must not record service_pin: %#v", got)
	}
}

func TestEnsureBmuxForInstallHonorsUsableEnvPin(t *testing.T) {
	// A daemon-usable COMMENT_IO_BMUX_BIN is left to be baked verbatim by
	// newLaunchAgentConfig: ensureBmuxForInstall must not override it.
	dir := privateTempHome(t, "comment-bus-bmux-envpin-ok-")
	envPin := filepath.Join(dir, "bmux")
	writeExecutableScript(t, envPin, "#!/bin/sh\nexit 0\n")
	t.Setenv(commentbus.BmuxBinaryEnv, envPin)
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		return commentbus.BmuxInstallResult{Path: envPin, AlreadyPresent: true, Discoverable: false}, nil
	})
	got, pin := ensureBmuxForInstall()
	if pin != "" {
		t.Fatalf("pin = %q, want empty (usable env pin is honored, not overridden)", pin)
	}
	if got["ok"] != true {
		t.Fatalf("ok = %#v, want true", got["ok"])
	}
	if _, has := got["service_pin"]; has {
		t.Fatalf("usable env pin must not record a service_pin: %#v", got)
	}
	if _, has := got["replaced_env_pin"]; has {
		t.Fatalf("usable env pin must not be replaced: %#v", got)
	}
}

func TestEnsureBmuxForInstallReplacesUnusableEnvPin(t *testing.T) {
	// A stale/unusable COMMENT_IO_BMUX_BIN must NOT be baked when the fresh install
	// is daemon-usable: ensureBmuxForInstall overrides it with the installed path.
	t.Setenv(commentbus.BmuxBinaryEnv, "/nonexistent/stale/bmux")
	dir := privateTempHome(t, "comment-bus-bmux-envpin-replace-")
	installed := filepath.Join(dir, "bmux")
	writeExecutableScript(t, installed, "#!/bin/sh\nexit 0\n")
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		return commentbus.BmuxInstallResult{Path: installed, Installed: true, Discoverable: false}, nil
	})
	got, pin := ensureBmuxForInstall()
	if pin != installed {
		t.Fatalf("pin = %q, want %q (unusable env pin replaced by installed path)", pin, installed)
	}
	if got["service_pin"] != installed {
		t.Fatalf("service_pin = %#v, want %q", got["service_pin"], installed)
	}
	if got["replaced_env_pin"] != "/nonexistent/stale/bmux" {
		t.Fatalf("replaced_env_pin = %#v, want the stale env pin", got["replaced_env_pin"])
	}
	if got["ok"] != true {
		t.Fatalf("ok = %#v, want true", got["ok"])
	}
}

func TestEnsureBmuxForInstallOverridesCrossChannelEnvPin(t *testing.T) {
	// A usable but cross-channel env pin: EnsureBmuxInstalled fell through its
	// AlreadyPresent precheck and downloaded a fresh binary (res.Installed), so the
	// stale pin must be replaced with the freshly installed path rather than baked.
	dir := privateTempHome(t, "comment-bus-bmux-crosschan-")
	envPin := filepath.Join(dir, "old-channel-bmux")
	writeExecutableScript(t, envPin, "#!/bin/sh\nexit 0\n")
	installed := filepath.Join(dir, "bmux")
	writeExecutableScript(t, installed, "#!/bin/sh\nexit 0\n")
	t.Setenv(commentbus.BmuxBinaryEnv, envPin)
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		// Installed=true + a path different from the (usable) env pin models the
		// channel-marker mismatch reinstall.
		return commentbus.BmuxInstallResult{Path: installed, Installed: true, Discoverable: false}, nil
	})
	got, pin := ensureBmuxForInstall()
	if pin != installed {
		t.Fatalf("pin = %q, want %q (cross-channel reinstall must override the env pin)", pin, installed)
	}
	if got["service_pin"] != installed {
		t.Fatalf("service_pin = %#v, want %q", got["service_pin"], installed)
	}
	if got["replaced_env_pin"] != envPin {
		t.Fatalf("replaced_env_pin = %#v, want %q", got["replaced_env_pin"], envPin)
	}
	if got["ok"] != true {
		t.Fatalf("ok = %#v, want true", got["ok"])
	}
}

func TestEnsureBmuxForInstallUnusableEnvPinAndUnusableInstall(t *testing.T) {
	// Both the env pin and the freshly installed path are daemon-unusable: surface
	// the install as not usable rather than baking a broken pin.
	t.Setenv(commentbus.BmuxBinaryEnv, "/nonexistent/stale/bmux")
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		return commentbus.BmuxInstallResult{Path: "/nonexistent/dir/bmux", Installed: true, Discoverable: false}, nil
	})
	got, pin := ensureBmuxForInstall()
	if pin != "" {
		t.Fatalf("pin = %q, want empty (no usable path to pin)", pin)
	}
	if got["ok"] != false {
		t.Fatalf("ok = %#v, want false", got["ok"])
	}
	if _, has := got["service_pin"]; has {
		t.Fatalf("unusable install must not record service_pin: %#v", got)
	}
}

func TestEnsureBmuxForInstallReportsFailureNonFatally(t *testing.T) {
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		return commentbus.BmuxInstallResult{}, errStub
	})
	got, pin := ensureBmuxForInstall()
	if got["ok"] != false {
		t.Fatalf("ok = %#v, want false", got["ok"])
	}
	if _, hasErr := got["error"]; !hasErr {
		t.Fatalf("expected error key in %#v", got)
	}
	if pin != "" {
		t.Fatalf("pin = %q, want empty on failure", pin)
	}
}

func TestBusInstallLaunchdResultBakesBmuxPin(t *testing.T) {
	if !launchdSupported() {
		t.Skip("launchd-only test")
	}
	home := privateTempHome(t, "comment-bus-bmux-pin-")
	t.Setenv("HOME", home)
	// Dry-run builds the plist without touching launchctl or the filesystem.
	res, err := busInstallLaunchdResult(filepath.Join(home, ".comment-io"), home, "", true, "/opt/custom/bmux")
	if err != nil {
		t.Fatalf("busInstallLaunchdResult: %v", err)
	}
	plist, _ := res["plist"].(string)
	if !strings.Contains(plist, commentbus.BmuxBinaryEnv) || !strings.Contains(plist, "/opt/custom/bmux") {
		t.Fatalf("plist missing baked bmux pin:\n%s", plist)
	}
}

var errStub = stubError("boom")

type stubError string

func (e stubError) Error() string { return string(e) }

func TestCheckBmuxFixInstallsWhenMissing(t *testing.T) {
	dir := privateTempHome(t, "comment-doctor-bmux-fix-")
	// Isolate HOME so bmux does not resolve from the real ~/.local/bin, forcing
	// the --fix path; the fix hook then drops a working fake bmux into a trusted
	// dir (~/.local/bin) so the daemon-accurate re-resolution succeeds.
	t.Setenv("HOME", dir)
	t.Setenv(commentbus.BmuxBinaryEnv, "")
	installed := filepath.Join(dir, ".local", "bin", "bmux")
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
			t.Fatal(err)
		}
		// Must be a real (non-`#!`) executable: the trusted-dir resolver skips
		// shell-script shims. Symlink to /bin/echo — the resolver EvalSymlinks to
		// the real (code-signed) binary, which answers `-V` with non-empty stdout,
		// satisfying the version probe. (Copying it would strip the signature and
		// get SIGKILLed on Apple Silicon.)
		linkExecutable(t, "/bin/echo", installed)
		return commentbus.BmuxInstallResult{Path: installed, Installed: true, Discoverable: true}, nil
	})

	got := checkBmux(commentbus.Paths{Home: filepath.Join(dir, ".comment-io")}, true)
	if got.Status != "fixed" {
		t.Fatalf("checkBmux(fix) status = %#v, want fixed", got)
	}
	if !strings.Contains(got.Message, "installed bmux") {
		t.Fatalf("checkBmux(fix) message = %q, want it to report the install", got.Message)
	}
}

func linkExecutable(t *testing.T, target, link string) {
	t.Helper()
	if _, err := os.Stat(target); err != nil {
		t.Skipf("fixture %s unavailable: %v", target, err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func TestCheckBmuxFixRejectsNonDiscoverableInstall(t *testing.T) {
	dir := privateTempHome(t, "comment-doctor-bmux-fix-bad-")
	t.Setenv("HOME", dir)
	t.Setenv(commentbus.BmuxBinaryEnv, "")
	// The hook "installs" into a non-trusted dir the daemon can't resolve. doctor
	// must NOT report fixed — it must surface the pin guidance.
	installed := filepath.Join(dir, "untrusted", "bmux")
	stubEnsureBmux(t, func(commentbus.BmuxInstallOptions) (commentbus.BmuxInstallResult, error) {
		if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
			t.Fatal(err)
		}
		writeExecutableScript(t, installed, "#!/bin/sh\nexit 0\n")
		return commentbus.BmuxInstallResult{Path: installed, Installed: true, Discoverable: false}, nil
	})

	got := checkBmux(commentbus.Paths{Home: filepath.Join(dir, ".comment-io")}, true)
	if got.Status != "error" {
		t.Fatalf("checkBmux(fix) status = %#v, want error for non-discoverable install", got)
	}
	if !strings.Contains(got.Message, "comment bus install") {
		t.Fatalf("checkBmux(fix) message = %q, want pin guidance", got.Message)
	}
}

func TestExitForErrorMapsMissingBmux(t *testing.T) {
	// A daemon-originated BMUX_NOT_INSTALLED socket error remaps to the dedicated
	// exit code and the CLI's own clear install message.
	sockErr := cliSocketError{Code: commentbus.SocketErrorCodeBmuxNotInstalled, Message: "ignored; CLI builds its own message"}
	code, stderr := exitForError(sockErr)
	if code != exitBmuxMissing {
		t.Fatalf("socket BMUX_NOT_INSTALLED exit code = %d; want %d", code, exitBmuxMissing)
	}
	if !strings.Contains(stderr, "bmux is required") {
		t.Fatalf("socket BMUX_NOT_INSTALLED stderr = %q; want install guidance", stderr)
	}
}

func TestCheckBmuxNoFixReportsInstallHint(t *testing.T) {
	dir := privateTempHome(t, "comment-doctor-bmux-nofix-")
	// Pin to a bare name that won't resolve in the empty temp home's trusted dirs.
	t.Setenv("HOME", dir)
	t.Setenv(commentbus.BmuxBinaryEnv, "")

	got := checkBmux(commentbus.Paths{Home: filepath.Join(dir, ".comment-io")}, false)
	if got.Status != "error" {
		t.Fatalf("checkBmux(no fix) status = %#v, want error", got)
	}
	if !strings.Contains(got.Message, "--fix") || !strings.Contains(got.Message, "install.sh") {
		t.Fatalf("checkBmux(no fix) message = %q, want --fix + install hint", got.Message)
	}
}
