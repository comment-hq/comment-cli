package commentbus

import (
	"errors"
	"os"
	"runtime"
	"strings"
)

// ErrTmuxNotInstalled marks the specific failure where the tmux binary cannot
// be located at all — it is not installed on a standard path and no usable
// COMMENT_IO_TMUX_BIN pin is set. It is distinct from "a configured tmux pin is
// unusable" so callers can surface a clear, OS-appropriate install message
// instead of a generic launch failure. Detect it with errors.Is.
var ErrTmuxNotInstalled = errors.New("tmux is not installed")

// SocketErrorCodeTmuxNotInstalled is the local-bus socket error code the daemon
// returns when it cannot launch a runtime because tmux is missing. The CLI maps
// it to a dedicated, human-readable exit status. Additive: older clients that
// don't recognize it still print the accompanying message and exit non-zero.
const SocketErrorCodeTmuxNotInstalled = "TMUX_NOT_INSTALLED"

// errTrustedExecutableNotFound is wrapped by the trusted-directory resolver when
// a bare executable name is not present in any trusted directory. The tmux layer
// translates it into ErrTmuxNotInstalled; other callers (e.g. runtime binary
// resolution) can keep treating it as a plain not-found error.
var errTrustedExecutableNotFound = errors.New("executable not found in trusted directories")

// TmuxNotInstalledMessage builds the full, human-readable error explaining that
// tmux is required and exactly how to install it on the current platform. Used
// for both the daemon's socket error message and the CLI's direct failures so
// the wording is identical wherever the user hits it.
func TmuxNotInstalledMessage() string {
	return "tmux is required to host agent runtimes, but it could not be found.\n\n" +
		TmuxInstallHint() + "\n\n" +
		"Already have tmux installed somewhere non-standard? Pin it for Comment.io with:\n" +
		"  export " + TmuxBinaryEnv + "=/absolute/path/to/tmux\n" +
		"then re-run your command. If the Comment bus runs as a background service\n" +
		"(launchd/systemd) it won't see your shell's environment — reinstall it so the\n" +
		"daemon picks up the pin:\n" +
		"  comment bus install\n" +
		"Run `comment doctor` to confirm tmux is detected."
}

// TmuxInstallHint returns a short, platform-appropriate instruction for
// installing tmux. It is robust across macOS and the common Linux distribution
// families, with a package-manager-agnostic fallback when the distro can't be
// identified.
func TmuxInstallHint() string {
	return tmuxInstallHintFor(runtime.GOOS, readOSRelease())
}

// TmuxInstallHintShort returns just the install command for the current platform
// as a single line (e.g. "brew install tmux", "sudo apt install tmux"), suitable
// for compact contexts like `comment doctor` JSON output where the full
// multi-line guidance from TmuxInstallHint would be unwieldy.
func TmuxInstallHintShort() string {
	return tmuxInstallHintShortFor(runtime.GOOS, readOSRelease())
}

// tmuxInstallHintShortFor is the pure core of TmuxInstallHintShort.
func tmuxInstallHintShortFor(goos string, osRelease string) string {
	switch goos {
	case "darwin":
		return "brew install tmux"
	case "windows":
		return "install tmux inside WSL (e.g. sudo apt install tmux)"
	case "linux":
		if cmd := linuxTmuxInstallCommand(osRelease); cmd != "" {
			return cmd
		}
		return "use your package manager (apt/dnf/pacman/zypper/apk) to install tmux"
	default:
		return "install tmux via your operating system's package manager"
	}
}

// readOSRelease returns the contents of /etc/os-release on Linux (best-effort;
// empty string when unreadable or on non-Linux platforms, which lets the hint
// mappers fall through to their generic guidance).
func readOSRelease() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		return string(data)
	}
	return ""
}

// tmuxInstallHintFor is the pure core of TmuxInstallHint: it maps a GOOS value
// plus the contents of /etc/os-release to an install instruction. Separated out
// so the per-distro mapping can be table-tested without touching the real
// filesystem.
func tmuxInstallHintFor(goos string, osRelease string) string {
	switch goos {
	case "darwin":
		// Homebrew installs into a trusted search dir (/opt/homebrew/bin or
		// /usr/local/bin); MacPorts' /opt/local/bin is not scanned, so we don't
		// suggest it as a one-step fix — those users should pin COMMENT_IO_TMUX_BIN.
		return "Install it with Homebrew:\n  brew install tmux\n" +
			"(no Homebrew? see https://brew.sh)"
	case "windows":
		// tmux has no native Windows build; it runs under WSL.
		return "tmux does not run natively on Windows. Use WSL (a Linux shell) and\n" +
			"install tmux there, e.g. `sudo apt install tmux` on Ubuntu."
	case "linux":
		if cmd := linuxTmuxInstallCommand(osRelease); cmd != "" {
			return "Install it with your package manager:\n  " + cmd
		}
		return "Install it with your distribution's package manager, for example:\n" +
			"  Debian/Ubuntu:  sudo apt install tmux\n" +
			"  Fedora/RHEL:    sudo dnf install tmux\n" +
			"  Arch:           sudo pacman -S tmux\n" +
			"  openSUSE:       sudo zypper install tmux\n" +
			"  Alpine:         sudo apk add tmux"
	default:
		return "Install tmux using your operating system's package manager."
	}
}

// linuxTmuxInstallCommand picks the install command for a Linux distribution by
// inspecting the ID and ID_LIKE fields of /etc/os-release. Returns "" when the
// distro family is unrecognized so the caller can show a multi-distro fallback.
func linuxTmuxInstallCommand(osRelease string) string {
	ids := osReleaseIDs(osRelease)
	for _, id := range ids {
		switch id {
		case "debian", "ubuntu", "linuxmint", "pop", "raspbian", "elementary", "kali", "neon":
			return "sudo apt install tmux"
		case "fedora", "rhel", "centos", "rocky", "almalinux", "ol", "oracle", "amzn", "amazon":
			return "sudo dnf install tmux"
		case "arch", "manjaro", "endeavouros", "garuda", "arcolinux":
			return "sudo pacman -S tmux"
		case "opensuse", "opensuse-leap", "opensuse-tumbleweed", "sles", "suse":
			return "sudo zypper install tmux"
		case "alpine":
			return "sudo apk add tmux"
		case "gentoo":
			return "sudo emerge app-misc/tmux"
		case "void":
			return "sudo xbps-install -S tmux"
		case "nixos":
			return "nix-env -iA nixpkgs.tmux"
		}
	}
	return ""
}

// osReleaseIDs parses the ID and ID_LIKE fields out of /etc/os-release contents
// into a lowercased, ordered list (ID first, then each ID_LIKE token). Quotes
// around values are stripped. Matching ID first means a derivative that sets a
// specific ID still wins over its ID_LIKE family.
func osReleaseIDs(osRelease string) []string {
	var primary string
	var like []string
	for _, line := range strings.Split(osRelease, "\n") {
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.ToLower(strings.Trim(strings.TrimSpace(value), `"'`))
		switch strings.TrimSpace(key) {
		case "ID":
			primary = value
		case "ID_LIKE":
			like = strings.Fields(value)
		}
	}
	ids := make([]string, 0, 1+len(like))
	if primary != "" {
		ids = append(ids, primary)
	}
	ids = append(ids, like...)
	return ids
}
