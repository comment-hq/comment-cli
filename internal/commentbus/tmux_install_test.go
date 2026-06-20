package commentbus

import (
	"errors"
	"strings"
	"testing"
)

func TestTmuxInstallHintFor(t *testing.T) {
	debianRelease := "PRETTY_NAME=\"Ubuntu 22.04.3 LTS\"\nID=ubuntu\nID_LIKE=debian\n"
	cases := []struct {
		name       string
		goos       string
		osRelease  string
		wantSubstr string
	}{
		{"darwin", "darwin", "", "brew install tmux"},
		{"windows", "windows", "", "WSL"},
		{"ubuntu", "linux", debianRelease, "sudo apt install tmux"},
		{"debian", "linux", "ID=debian\n", "sudo apt install tmux"},
		{"mint via id_like", "linux", "ID=linuxmint\nID_LIKE=\"ubuntu debian\"\n", "sudo apt install tmux"},
		{"fedora", "linux", "ID=fedora\n", "sudo dnf install tmux"},
		{"rocky via id_like", "linux", "ID=rocky\nID_LIKE=\"rhel centos fedora\"\n", "sudo dnf install tmux"},
		{"arch", "linux", "ID=arch\n", "sudo pacman -S tmux"},
		{"manjaro", "linux", "ID=manjaro\nID_LIKE=arch\n", "sudo pacman -S tmux"},
		{"opensuse", "linux", "ID=\"opensuse-leap\"\nID_LIKE=\"suse opensuse\"\n", "sudo zypper install tmux"},
		{"alpine", "linux", "ID=alpine\n", "sudo apk add tmux"},
		{"gentoo", "linux", "ID=gentoo\n", "emerge"},
		{"void", "linux", "ID=void\n", "xbps-install"},
		{"nixos", "linux", "ID=nixos\n", "nixpkgs.tmux"},
		{"unknown linux falls back to multi-distro list", "linux", "ID=plan9\n", "Debian/Ubuntu"},
		{"empty linux falls back to multi-distro list", "linux", "", "sudo pacman -S tmux"},
		{"unknown os", "plan9", "", "package manager"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tmuxInstallHintFor(tc.goos, tc.osRelease)
			if !strings.Contains(got, tc.wantSubstr) {
				t.Fatalf("tmuxInstallHintFor(%q, ...) = %q; want substring %q", tc.goos, got, tc.wantSubstr)
			}
		})
	}
}

func TestTmuxInstallHintShortFor(t *testing.T) {
	cases := []struct {
		name       string
		goos       string
		osRelease  string
		want       string
	}{
		{"darwin", "darwin", "", "brew install tmux"},
		{"ubuntu", "linux", "ID=ubuntu\nID_LIKE=debian\n", "sudo apt install tmux"},
		{"fedora", "linux", "ID=fedora\n", "sudo dnf install tmux"},
		{"unknown linux", "linux", "ID=plan9\n", "use your package manager (apt/dnf/pacman/zypper/apk) to install tmux"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tmuxInstallHintShortFor(tc.goos, tc.osRelease); got != tc.want {
				t.Fatalf("tmuxInstallHintShortFor(%q, ...) = %q; want %q", tc.goos, got, tc.want)
			}
		})
	}
}

func TestOSReleaseIDsPrefersPrimaryID(t *testing.T) {
	// A derivative that sets its own ID should match that ID before its ID_LIKE
	// family, so the primary ID is ordered first.
	ids := osReleaseIDs("ID=fedora\nID_LIKE=\"rhel centos\"\n")
	if len(ids) == 0 || ids[0] != "fedora" {
		t.Fatalf("osReleaseIDs ordering = %v; want fedora first", ids)
	}
}

func TestTmuxNotInstalledMessageIncludesHintAndEnvVar(t *testing.T) {
	msg := TmuxNotInstalledMessage()
	if !strings.Contains(msg, "tmux is required") {
		t.Fatalf("message missing requirement framing: %q", msg)
	}
	if !strings.Contains(msg, TmuxBinaryEnv) {
		t.Fatalf("message should mention the %s override: %q", TmuxBinaryEnv, msg)
	}
	if !strings.Contains(msg, TmuxInstallHint()) {
		t.Fatalf("message should embed the platform install hint")
	}
	// A pin exported in the shell does not reach a launchd/systemd bus daemon;
	// the guidance must tell service users to reinstall the bus to persist it.
	if !strings.Contains(msg, "comment bus install") {
		t.Fatalf("message should tell service users to reinstall the bus to persist the pin: %q", msg)
	}
}

func TestTmuxInstallHintDarwinDropsUnscannedMacPorts(t *testing.T) {
	// MacPorts installs to /opt/local/bin, which the daemon does not scan, so the
	// macOS hint must not present `port install` as a one-step fix.
	hint := tmuxInstallHintFor("darwin", "")
	if !strings.Contains(hint, "brew install tmux") {
		t.Fatalf("darwin hint should recommend Homebrew: %q", hint)
	}
	if strings.Contains(hint, "port install") || strings.Contains(strings.ToLower(hint), "macports") {
		t.Fatalf("darwin hint should not suggest MacPorts (its bin dir is not scanned): %q", hint)
	}
}

func TestCommandWrapsErrTmuxNotInstalled(t *testing.T) {
	// A bare tmux name that resolves to nothing in any trusted directory must
	// surface as ErrTmuxNotInstalled so callers can show an install hint. Point
	// the controller at a name that cannot exist on a standard path.
	c := ExecTmuxController{Binary: "tmux-definitely-not-a-real-binary-xyz"}
	_, _, cancel, err := c.command(nil, "")
	if cancel != nil {
		cancel()
	}
	if err == nil {
		t.Fatal("expected resolution error for a non-existent tmux binary")
	}
	if !errors.Is(err, ErrTmuxNotInstalled) {
		t.Fatalf("error = %v; want errors.Is ErrTmuxNotInstalled", err)
	}
}
