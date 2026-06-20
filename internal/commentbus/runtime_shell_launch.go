package commentbus

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// Shell-native runtime launch (§4 of docs/plans/runtime-resolution-shell-native.md).
//
// Instead of resolving a runtime (claude/codex) to a trusted absolute binary
// path and exec'ing it, "shell" launch mode hands the runtime *name* to the
// user's interactive login shell and lets the shell resolve it — PATH binary,
// alias, or function. This file holds the pure builders + login-shell discovery;
// wiring into the daemon/session-exec paths happens separately.

// shellFamily classifies a login shell so the -c script can be tailored
// (bash needs `shopt -s expand_aliases` and a PS1 guard bypass; zsh does not).
type shellFamily int

const (
	shellFamilyOther shellFamily = iota
	shellFamilyZsh
	shellFamilyBash
)

// shellRuntimeNameRE is a defense-in-depth gate on any runtime name that gets
// interpolated literally into a -c script string. The primary gate remains
// isManagedSessionRuntime (claude/codex); this guarantees no shell metacharacter
// can ever reach the script even if the allowlist is later widened.
var shellRuntimeNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

var shellEnvKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isShellSafeRuntimeName reports whether name is safe to interpolate into a -c
// script. Callers MUST still gate on isManagedSessionRuntime; this is the
// belt-and-suspenders character check.
func isShellSafeRuntimeName(name string) bool {
	return shellRuntimeNameRE.MatchString(name)
}

func classifyShellFamily(shellPath string) shellFamily {
	base := strings.ToLower(filepath.Base(shellPath))
	switch {
	case strings.Contains(base, "zsh"):
		return shellFamilyZsh
	case strings.Contains(base, "bash"):
		return shellFamilyBash
	default:
		return shellFamilyOther
	}
}

// isExecutableFile reports whether path is an existing, regular-ish, executable
// file. This is a "does it even work" check, NOT a trust check — shell-native
// mode deliberately drops the perms/owner walk.
func isExecutableFile(path string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}

// resolveLoginShell returns the absolute path of the current user's login shell
// and its family. Resolution order (§4.2):
//  1. $SHELL, if set and a valid executable absolute path.
//  2. The passwd DB, keyed by os/user.Current().Username (NOT $USER, which
//     launchd may not propagate): `dscl` on darwin, `getent passwd` on linux.
//  3. Per-OS default (/bin/zsh on darwin, /bin/bash on linux).
//
// Every candidate is TrimSpace'd and existence/exec-checked before use.
func resolveLoginShell() (string, shellFamily, error) {
	if shell := strings.TrimSpace(os.Getenv("SHELL")); isExecutableFile(shell) {
		return shell, classifyShellFamily(shell), nil
	}
	if shell := loginShellFromPasswd(); isExecutableFile(shell) {
		return shell, classifyShellFamily(shell), nil
	}
	def := defaultLoginShell()
	if isExecutableFile(def) {
		return def, classifyShellFamily(def), nil
	}
	return "", shellFamilyOther, errors.New("could not resolve a usable login shell; set $SHELL to an absolute path")
}

func defaultLoginShell() string {
	if runtime.GOOS == "darwin" {
		return "/bin/zsh"
	}
	return "/bin/bash"
}

// loginShellFromPasswd reads the current user's shell from the passwd database,
// keyed by the username from os/user.Current() (which reads getpwuid(geteuid())
// directly, bypassing the possibly-unset $USER env var). It uses separate-argv
// exec (never a shell -c string) so a username containing `.`/`@`/`\`/spaces
// cannot break the call or inject.
func loginShellFromPasswd() string {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return ""
	}
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.Command("dscl", ".", "-read", "/Users/"+u.Username, "UserShell").Output()
		if err != nil {
			return ""
		}
		// Output looks like "UserShell: /bin/zsh"; take the first line, strip the
		// label, and use the whole remainder (a shell path may contain spaces, so
		// do NOT split on whitespace and keep only the last token).
		line := string(out)
		if idx := strings.IndexByte(line, '\n'); idx >= 0 {
			line = line[:idx]
		}
		return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "UserShell:"))
	case "linux":
		out, err := exec.Command("getent", "passwd", u.Username).Output()
		if err != nil {
			return ""
		}
		line := strings.TrimSpace(string(out))
		if idx := strings.IndexByte(line, '\n'); idx >= 0 {
			line = line[:idx]
		}
		parts := strings.Split(line, ":")
		if len(parts) < 7 {
			return ""
		}
		return strings.TrimSpace(parts[6])
	default:
		return ""
	}
}

// shellExportPrefix builds a sequence of `export VAR='value'; ` statements for
// the -c script (§4.7). Each value is POSIX single-quoted via shellQuote so it
// cannot inject. Values containing control characters that single-quoting cannot
// neutralize (\r \n \x00) abort the launch — a literal newline inside a '...'
// block in a -c string is a real newline in the shell source. env entries are
// "KEY=VALUE" strings; malformed or unsafe keys are skipped.
func shellExportPrefix(env []string) (string, error) {
	var b strings.Builder
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !shellEnvKeyRE.MatchString(key) {
			continue
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return "", fmt.Errorf("env value for %s contains control characters and cannot be shell-quoted", key)
		}
		b.WriteString("export ")
		b.WriteString(key)
		b.WriteString("=")
		b.WriteString(shellQuote(value))
		b.WriteString("; ")
	}
	return b.String(), nil
}

// buildShellLaunchScript builds the body passed to `<shell> -ilc <body>`.
// runtimeName MUST already be validated (isManagedSessionRuntime AND
// isShellSafeRuntimeName) — it is the only value interpolated as a command word.
// exportPrefix is the output of shellExportPrefix (already quoted). The runtime
// name appears literally as the command word (required for alias expansion);
// runtime args are forwarded via "$@" from argv, never interpolated here.
func buildShellLaunchScript(family shellFamily, runtimeName, exportPrefix string) string {
	body := exportPrefix + runtimeName + ` "$@"`
	if family == shellFamilyBash {
		// bash subtleties (§4.1):
		//   - login mode (-l) sources .bash_profile, NOT .bashrc, so aliases and
		//     functions defined in .bashrc must be sourced explicitly. We do so
		//     idempotently; combined with PS1=x in the env (appendShellLaunchEnv),
		//     a `[ -z "$PS1" ] && return` guard in .bashrc still passes.
		//   - alias expansion inside a -c string requires `shopt -s expand_aliases`
		//     as the first statement and the alias to be defined before the
		//     command word is parsed.
		return `shopt -s expand_aliases; [ -r "$HOME/.bashrc" ] && . "$HOME/.bashrc"; ` + body
	}
	return body
}

// buildShellLaunchArgv returns the full argv to exec for a shell-native launch:
//
//	[shellPath, "-ilc", <script>, runtimeName, runtimeArgs...]
//
// The runtimeName after the script is the shell's $0 (used in shell error
// messages); runtimeArgs become $1.. and are forwarded by the script's "$@".
// Only the validated runtimeName is interpolated into the script string.
func buildShellLaunchArgv(shellPath string, family shellFamily, runtimeName, exportPrefix string, runtimeArgs []string) []string {
	script := buildShellLaunchScript(family, runtimeName, exportPrefix)
	argv := make([]string, 0, 4+len(runtimeArgs))
	argv = append(argv, shellPath, "-ilc", script, runtimeName)
	argv = append(argv, runtimeArgs...)
	return argv
}

// appendShellLaunchEnv ensures the shell process environment carries PS1 so a
// .bashrc guarded on `[ -z "$PS1" ] && return` still sources its aliases (§4.1).
// Harmless for zsh. Returns env unchanged if PS1 is already present.
func appendShellLaunchEnv(env []string, family shellFamily) []string {
	if family != shellFamilyBash {
		return env
	}
	for _, entry := range env {
		if strings.HasPrefix(entry, "PS1=") {
			return env
		}
	}
	return append(env, "PS1=x")
}
