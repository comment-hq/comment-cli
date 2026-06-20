package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

type doctorCheck struct {
	Name       string `json:"name"`
	Status     string `json:"status"` // "ok", "warn", "error", "fixed"
	Message    string `json:"message,omitempty"`
	FixApplied bool   `json:"fix_applied,omitempty"`
	FixError   string `json:"fix_error,omitempty"`
	Detail     any    `json:"detail,omitempty"`
}

type doctorResult struct {
	OK     bool          `json:"ok"`
	Home   string        `json:"home"`
	Checks []doctorCheck `json:"checks"`
}

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("comment doctor", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	fix := fs.Bool("fix", false, "Apply safe repairs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("doctor does not accept positional arguments")
	}

	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}

	result := doctorResult{Home: paths.Home}
	if daemonExternallyManaged(paths) {
		// Caged/external daemon: the managed-runtime host binaries (claude, tmux)
		// are the container's concern, not the host's — the host doesn't launch
		// runtimes — so skip those checks rather than fail a sync/message-only
		// caged install (or, with --fix, install host binaries it can't use).
		result.Checks = append(result.Checks, okCheck("host_runtime", "skipped: daemon is externally managed (container); host claude/tmux are not used"))
	} else {
		result.Checks = append(result.Checks, checkRuntimeOnPATH())
		result.Checks = append(result.Checks, checkRuntimeAuth())
		// Managed runtimes default to tmux, so doctor verifies tmux (the real
		// default host). bmux is no longer auto-provisioned; it stays an explicit
		// opt-in via COMMENT_IO_BMUX_BIN and is not surfaced as a default check.
		result.Checks = append(result.Checks, checkTmux(paths))
	}
	result.Checks = append(result.Checks, checkAgentsDir(paths, *fix))
	result.Checks = append(result.Checks, checkAgentProfileFiles(paths, *fix))
	installedCheck := checkDaemonInstalled(paths, *fix)
	result.Checks = append(result.Checks, installedCheck)
	daemonInstalled := installedCheck.Status == "ok" || installedCheck.Status == "fixed"
	result.Checks = append(result.Checks, checkDaemonPaired(paths, *fix, daemonInstalled))
	result.Checks = append(result.Checks, checkDaemonRunning(paths, *fix))
	result.Checks = append(result.Checks, checkDaemonProfiles(paths, *fix))

	result.OK = allDoctorChecksOK(result.Checks)
	if err := printJSON(result); err != nil {
		return err
	}
	if !result.OK {
		return cliExitError{Code: 2}
	}
	return nil
}

func allDoctorChecksOK(checks []doctorCheck) bool {
	for _, c := range checks {
		if c.Status == "error" || c.Status == "warn" {
			return false
		}
	}
	return true
}

func okCheck(name, msg string) doctorCheck {
	return doctorCheck{Name: name, Status: "ok", Message: msg}
}

func warnCheck(name, msg string) doctorCheck {
	return doctorCheck{Name: name, Status: "warn", Message: msg}
}

func errorCheck(name, msg string) doctorCheck {
	return doctorCheck{Name: name, Status: "error", Message: msg}
}

func fixedCheck(name, msg string) doctorCheck {
	return doctorCheck{Name: name, Status: "fixed", Message: msg, FixApplied: true}
}

// checkRuntimeOnPATH — a coding CLI (Claude Code or Codex) is present to host
// agents. `comment run` prefers Claude Code and falls back to Codex, so either
// one satisfies this; only neither is an error.
func checkRuntimeOnPATH() doctorCheck {
	claudePath, claudeErr := runtimeLookPath("claude")
	codexPath, codexErr := runtimeLookPath("codex")
	if claudeErr != nil && codexErr != nil {
		return errorCheck(
			"runtime_on_path",
			"no coding CLI found on PATH; install Claude Code (https://docs.claude.com/en/docs/claude-code) or Codex so `comment run` can host your agents",
		)
	}
	detail := map[string]any{}
	if claudeErr == nil {
		detail["claude"] = claudePath
	}
	if codexErr == nil {
		detail["codex"] = codexPath
	}
	return doctorCheck{
		Name:    "runtime_on_path",
		Status:  "ok",
		Message: "a coding CLI is on PATH (claude and/or codex)",
		Detail:  detail,
	}
}

// checkRuntimeAuth — warn if an installed coding CLI isn't logged in; it can't
// host an agent until it is. Only consulted for runtimes that are installed, so
// a user with only one CLI isn't nagged about the other.
func checkRuntimeAuth() doctorCheck {
	var problems []string
	if _, err := runtimeLookPath("claude"); err == nil {
		if ok, hint := runtimeAuthState("claude"); !ok {
			problems = append(problems, hint)
		}
	}
	if _, err := runtimeLookPath("codex"); err == nil {
		if ok, hint := runtimeAuthState("codex"); !ok {
			problems = append(problems, hint)
		}
	}
	if len(problems) == 0 {
		return okCheck("runtime_auth", "your coding CLI is authenticated")
	}
	return warnCheck("runtime_auth", strings.Join(problems, " "))
}

const tmuxVersionFloorMajor, tmuxVersionFloorMinor = 3, 2

var (
	tmuxVersionRE    = regexp.MustCompile(`(\d+)\.(\d+)`)
	plistTmuxPinRE   = regexp.MustCompile(`(?s)<key>COMMENT_IO_TMUX_BIN</key>\s*<string>([^<]*)</string>`)
	plistBmuxPinRE   = regexp.MustCompile(`(?s)<key>COMMENT_IO_BMUX_BIN</key>\s*<string>([^<]*)</string>`)
	systemdTmuxPinRE = regexp.MustCompile(`(?m)^Environment="COMMENT_IO_TMUX_BIN=((?:[^"\\]|\\.)*)"`)
	systemdBmuxPinRE = regexp.MustCompile(`(?m)^Environment="COMMENT_IO_BMUX_BIN=((?:[^"\\]|\\.)*)"`)
)

// unescapeSystemdEnvValue reverses systemdQuoteArg: it undoes backslash escapes
// (\\ -> \, \" -> ") and then the doubled percent (%% -> %). Without this a
// persisted pin like /opt/tmux%stable/bin/tmux (stored as %%stable) would be
// resolved with the wrong path.
func unescapeSystemdEnvValue(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(s[i])
	}
	return strings.ReplaceAll(b.String(), "%%", "%")
}

// installedServiceTmuxConfig inspects the installed launchd/systemd service
// definition for this home. It returns the COMMENT_IO_TMUX_BIN pin baked into
// the service (or "" if unpinned) and whether such a service exists at all.
// Callers need both to tell "installed service with no pin" (which
// auto-discovers and does NOT inherit the shell's COMMENT_IO_TMUX_BIN) apart
// from "no installed service" (foreground/shell context).
func installedServiceTmuxConfig(paths commentbus.Paths) (pin string, serviceExists bool) {
	return installedServiceBinaryConfig(paths, extractTmuxPinFromPlist, extractTmuxPinFromSystemd)
}

func installedServiceBmuxConfig(paths commentbus.Paths) (pin string, serviceExists bool) {
	return installedServiceBinaryConfig(paths, extractBmuxPinFromPlist, extractBmuxPinFromSystemd)
}

func installedServiceBinaryConfig(paths commentbus.Paths, extractPlist func([]byte) string, extractSystemd func([]byte) string) (pin string, serviceExists bool) {
	label := launchdLabelForHome(paths.Home)
	if dir, err := userLaunchAgentsDir(); err == nil {
		if data, err := os.ReadFile(filepath.Join(dir, label+".plist")); err == nil {
			serviceExists = true
			if v := extractPlist(data); v != "" {
				return v, true
			}
		}
	}
	if dir, err := userSystemdUnitDir(); err == nil {
		if data, err := os.ReadFile(filepath.Join(dir, label+".service")); err == nil {
			serviceExists = true
			if v := extractSystemd(data); v != "" {
				return v, true
			}
		}
	}
	return "", serviceExists
}

// effectiveTmuxResolveInput maps the installed-service state to the value passed
// to ResolveDaemonTmuxBinary so doctor mirrors the owning daemon: a pin wins; an
// unpinned-but-installed service forces trusted-dir auto-discovery ("tmux",
// bypassing the shell's COMMENT_IO_TMUX_BIN); no service defers to the shell.
func effectiveTmuxResolveInput(pin string, serviceExists bool) string {
	return effectiveServiceResolveInput(pin, serviceExists, "tmux")
}

func effectiveBmuxResolveInput(pin string, serviceExists bool) string {
	return effectiveServiceResolveInput(pin, serviceExists, "bmux")
}

func effectiveServiceResolveInput(pin string, serviceExists bool, defaultName string) string {
	if pin != "" {
		return pin
	}
	if serviceExists {
		return defaultName
	}
	return ""
}

func extractTmuxPinFromPlist(data []byte) string {
	return extractPinFromPlist(data, plistTmuxPinRE)
}

func extractBmuxPinFromPlist(data []byte) string {
	return extractPinFromPlist(data, plistBmuxPinRE)
}

func extractPinFromPlist(data []byte, re *regexp.Regexp) string {
	if m := re.FindSubmatch(data); m != nil {
		// The plist writer XML-escapes the value (xml.EscapeText); reverse it.
		return strings.TrimSpace(html.UnescapeString(string(m[1])))
	}
	return ""
}

func extractTmuxPinFromSystemd(data []byte) string {
	return extractPinFromSystemd(data, systemdTmuxPinRE)
}

func extractBmuxPinFromSystemd(data []byte) string {
	return extractPinFromSystemd(data, systemdBmuxPinRE)
}

func extractPinFromSystemd(data []byte, re *regexp.Regexp) string {
	if m := re.FindSubmatch(data); m != nil {
		return strings.TrimSpace(unescapeSystemdEnvValue(string(m[1])))
	}
	return ""
}

// checkTmux — the daemon's tmux resolves to a real, recent-enough binary, and
// no wrapper shim shadows it. The daemon scans trusted directories (never
// $PATH), so a "smart tmux" shim on PATH does not break the daemon — but it
// breaks interactive use and any other PATH-based consumer, so we surface it.
func checkTmux(paths commentbus.Paths) doctorCheck {
	name := "tmux_runtime"
	// Resolve the way the *installed* daemon will: an explicit pin baked into
	// the service definition (COMMENT_IO_TMUX_BIN) takes precedence, since a
	// shell-level value is not what the launchd/systemd daemon sees. Falls back
	// to the current environment + trusted-dir auto-discovery for a foreground
	// daemon.
	pin, serviceExists := installedServiceTmuxConfig(paths)
	// Resolve exactly how the daemon that owns this home will:
	//   - service has a pin            -> use it
	//   - service exists but no pin    -> it auto-discovers; force "tmux" so we
	//                                     bypass the invoking shell's
	//                                     COMMENT_IO_TMUX_BIN, which the service
	//                                     does not inherit
	//   - no installed service         -> foreground/shell context; honor the
	//                                     shell's COMMENT_IO_TMUX_BIN
	resolveInput := effectiveTmuxResolveInput(pin, serviceExists)
	resolved, err := commentbus.ResolveDaemonTmuxBinary(resolveInput)
	if err != nil {
		msg := "tmux not found in trusted directories; " + commentbus.TmuxInstallHintShort() + " or set COMMENT_IO_TMUX_BIN"
		if pin != "" {
			msg = "installed daemon's pinned tmux is unusable (" + pin + "): " + err.Error()
		}
		return errorCheck(name, msg)
	}
	detail := map[string]any{"path": resolved}
	if pin != "" {
		detail["service_pin"] = pin
	}
	if commentbus.IsTmuxWrapperScript(resolved) {
		detail["wrapper_script"] = true
		return doctorCheck{Name: name, Status: "warn", Message: "resolved tmux is a shell-script wrapper, not a real binary: " + resolved, Detail: detail}
	}
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelProbe()
	out, verr := exec.CommandContext(probeCtx, resolved, "-V").Output()
	if probeCtx.Err() != nil {
		return doctorCheck{Name: name, Status: "error", Message: "tmux -V timed out for " + resolved + " (broken or hanging binary)", Detail: detail}
	}
	version := strings.TrimSpace(string(out))
	if verr != nil || version == "" {
		msg := "tmux -V failed for " + resolved
		if verr != nil {
			msg += ": " + verr.Error()
		}
		return doctorCheck{Name: name, Status: "error", Message: msg, Detail: detail}
	}
	detail["version"] = version
	if p, e := exec.LookPath("tmux"); e == nil && p != resolved && commentbus.IsTmuxWrapperScript(p) {
		detail["path_shadow"] = p
		return doctorCheck{Name: name, Status: "warn", Message: "a tmux wrapper shim shadows the real binary on PATH at " + p + "; the daemon uses " + resolved + ", but other tools that call tmux via PATH will hit the shim", Detail: detail}
	}
	if maj, min, ok := parseTmuxMajorMinor(version); ok && (maj < tmuxVersionFloorMajor || (maj == tmuxVersionFloorMajor && min < tmuxVersionFloorMinor)) {
		return doctorCheck{Name: name, Status: "warn", Message: fmt.Sprintf("tmux %d.%d is older than the recommended %d.%d; bracketed-paste orientation may be unreliable", maj, min, tmuxVersionFloorMajor, tmuxVersionFloorMinor), Detail: detail}
	}
	return doctorCheck{Name: name, Status: "ok", Message: version + " at " + resolved, Detail: detail}
}

func checkBmux(paths commentbus.Paths, fix bool) doctorCheck {
	name := "bmux_runtime"
	pin, serviceExists := installedServiceBmuxConfig(paths)
	resolveInput := effectiveBmuxResolveInput(pin, serviceExists)
	resolved, err := commentbus.TrustedBmuxBinaryPath(resolveInput)
	if err != nil {
		// A pinned-but-unusable bmux is an operator misconfiguration we must not
		// silently overwrite; only auto-install when bmux is simply absent.
		if pin != "" {
			return errorCheck(name, "installed daemon's pinned bmux is unusable ("+pin+"): "+err.Error())
		}
		if fix {
			if _, ierr := ensureBmuxInstalledFn(commentbus.BmuxInstallOptions{}); ierr != nil {
				return doctorCheck{Name: name, Status: "error", Message: "bmux not found and auto-install failed; " + commentbus.BmuxInstallHintShort(), FixError: ierr.Error()}
			}
			// Verify the DAEMON — not the shell — can now resolve bmux: re-resolve
			// with the same effective input the installed service uses (bare "bmux"
			// → trusted dirs for an unpinned service). EnsureBmuxInstalled honors
			// shell overrides (COMMENT_IO_BMUX_BIN / BMUX_INSTALL_DIR) that the
			// background daemon does not inherit, so an install outside the trusted
			// directories must not be reported as "fixed".
			daemonResolved, derr := commentbus.TrustedBmuxBinaryPath(resolveInput)
			if derr != nil {
				return errorCheck(name, "bmux was installed but the daemon cannot resolve it (installed outside the trusted directories); run `comment bus install` to pin it, or "+commentbus.BmuxInstallHintShort())
			}
			return bmuxProbeCheck(name, daemonResolved, pin, true)
		}
		return errorCheck(name, "bmux not found in trusted directories; run with --fix to auto-install, or "+commentbus.BmuxInstallHintShort()+", or set "+commentbus.BmuxBinaryEnv)
	}
	return bmuxProbeCheck(name, resolved, pin, false)
}

// bmuxProbeCheck runs `bmux -V` against a resolved binary and builds the doctor
// result. fixed marks the check as a successful --fix repair (status "fixed")
// rather than a plain "ok".
func bmuxProbeCheck(name, resolved, pin string, fixed bool) doctorCheck {
	detail := map[string]any{"path": resolved}
	if pin != "" {
		detail["service_pin"] = pin
	}
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelProbe()
	out, verr := exec.CommandContext(probeCtx, resolved, "-V").Output()
	if probeCtx.Err() != nil {
		return doctorCheck{Name: name, Status: "error", Message: "bmux -V timed out for " + resolved + " (broken or hanging binary)", Detail: detail}
	}
	version := strings.TrimSpace(string(out))
	if verr != nil || version == "" {
		msg := "bmux -V failed for " + resolved
		if verr != nil {
			msg += ": " + verr.Error()
		}
		return doctorCheck{Name: name, Status: "error", Message: msg, Detail: detail}
	}
	detail["version"] = version
	if fixed {
		return doctorCheck{Name: name, Status: "fixed", Message: "installed bmux (" + version + ") at " + resolved, FixApplied: true, Detail: detail}
	}
	return doctorCheck{Name: name, Status: "ok", Message: version + " at " + resolved, Detail: detail}
}

// parseTmuxMajorMinor extracts major/minor from `tmux -V` output such as
// "tmux 3.6a", "tmux 3.2", or "tmux next-3.4".
func parseTmuxMajorMinor(version string) (int, int, bool) {
	m := tmuxVersionRE.FindStringSubmatch(version)
	if m == nil {
		return 0, 0, false
	}
	maj, err1 := strconv.Atoi(m[1])
	min, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

// checkAgentsDir — ~/.comment-io/agents exists, owned by user, not
// group/world writable.
func checkAgentsDir(paths commentbus.Paths, fix bool) doctorCheck {
	name := "agents_dir"
	agentsDir := filepath.Join(paths.Home, "agents")
	info, err := os.Lstat(agentsDir)
	if errors.Is(err, os.ErrNotExist) {
		if !fix {
			return warnCheck(name, fmt.Sprintf("agents directory %s does not exist; run with --fix to create it", agentsDir))
		}
		if err := os.MkdirAll(agentsDir, 0o700); err != nil {
			return doctorCheck{Name: name, Status: "error", Message: "could not create agents directory", FixError: err.Error()}
		}
		return fixedCheck(name, fmt.Sprintf("created %s (0700)", agentsDir))
	}
	if err != nil {
		return errorCheck(name, "could not inspect agents directory: "+err.Error())
	}
	if !info.IsDir() {
		return errorCheck(name, agentsDir+" exists but is not a directory")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errorCheck(name, agentsDir+" must not be a symlink")
	}
	if err := validateOwnerIsCurrentUser(info); err != nil {
		return errorCheck(name, "agents directory is not owned by the current user; cannot auto-fix")
	}
	if info.Mode().Perm()&0o022 != 0 {
		if !fix {
			return warnCheck(name, fmt.Sprintf("agents directory %s is group- or world-writable (mode %#o); run with --fix to chmod 0700", agentsDir, info.Mode().Perm()))
		}
		if err := os.Chmod(agentsDir, 0o700); err != nil {
			return doctorCheck{Name: name, Status: "error", Message: "could not chmod agents directory", FixError: err.Error()}
		}
		return fixedCheck(name, fmt.Sprintf("chmod 0700 %s", agentsDir))
	}
	return okCheck(name, fmt.Sprintf("%s mode %#o", agentsDir, info.Mode().Perm()))
}

// checkAgentProfileFiles — each ~/.comment-io/agents/*.json is owner=user
// and 0600. The legacy 0.1.0 CLI shipped some profile files with 0644;
// the new daemon rejects them, blocking sibling profiles from loading.
func checkAgentProfileFiles(paths commentbus.Paths, fix bool) doctorCheck {
	name := "agent_profile_files"
	agentsDir := filepath.Join(paths.Home, "agents")
	entries, err := os.ReadDir(agentsDir)
	if errors.Is(err, os.ErrNotExist) {
		return okCheck(name, "no agent profiles to check")
	}
	if err != nil {
		return errorCheck(name, "could not read agents directory: "+err.Error())
	}
	type profileFinding struct {
		Path   string `json:"path"`
		Mode   string `json:"mode"`
		Action string `json:"action"`
	}
	var bad []profileFinding
	var fixed []profileFinding
	var unfixable []profileFinding
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		profilePath := filepath.Join(agentsDir, entry.Name())
		info, statErr := os.Lstat(profilePath)
		if statErr != nil {
			unfixable = append(unfixable, profileFinding{Path: profilePath, Action: "stat_failed:" + statErr.Error()})
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			unfixable = append(unfixable, profileFinding{Path: profilePath, Action: "not_regular_file"})
			continue
		}
		if err := validateOwnerIsCurrentUser(info); err != nil {
			unfixable = append(unfixable, profileFinding{Path: profilePath, Action: "wrong_owner"})
			continue
		}
		perm := info.Mode().Perm()
		if perm&0o077 == 0 {
			continue
		}
		modeStr := fmt.Sprintf("%#o", perm)
		if !fix {
			bad = append(bad, profileFinding{Path: profilePath, Mode: modeStr, Action: "needs_chmod_0600"})
			continue
		}
		if err := os.Chmod(profilePath, 0o600); err != nil {
			unfixable = append(unfixable, profileFinding{Path: profilePath, Mode: modeStr, Action: "chmod_failed:" + err.Error()})
			continue
		}
		fixed = append(fixed, profileFinding{Path: profilePath, Mode: modeStr, Action: "chmod_0600"})
	}
	switch {
	case len(unfixable) > 0:
		return doctorCheck{
			Name:    name,
			Status:  "error",
			Message: fmt.Sprintf("%d profile file(s) cannot be auto-repaired", len(unfixable)),
			Detail:  map[string]any{"unfixable": unfixable, "bad": bad, "fixed": fixed},
		}
	case len(fixed) > 0:
		return doctorCheck{
			Name:       name,
			Status:     "fixed",
			Message:    fmt.Sprintf("chmod 0600 on %d profile file(s)", len(fixed)),
			FixApplied: true,
			Detail:     map[string]any{"fixed": fixed},
		}
	case len(bad) > 0:
		return doctorCheck{
			Name:    name,
			Status:  "warn",
			Message: fmt.Sprintf("%d profile file(s) have group/world-readable permissions; run with --fix to chmod 0600", len(bad)),
			Detail:  map[string]any{"bad": bad},
		}
	default:
		return okCheck(name, "all agent profile files have 0600 permissions")
	}
}

// checkDaemonInstalled — the launchd plist or systemd user unit exists.
// daemonExternallyManaged reports whether the daemon is provided by an external
// runtime (e.g. a container) rather than a native launchd/systemd service. True
// when the opt-in TCP transport is configured OR the caller marks it explicitly
// with COMMENT_IO_DAEMON_EXTERNAL=1 — the latter covers the Linux caged setup
// that uses the bind-mounted Unix socket (no TCP address). In that case doctor
// must never run a native install/start, which could start a host daemon against
// the container-managed state dir.
func daemonExternallyManaged(paths commentbus.Paths) bool {
	// paths.BusTCPAddr is already scoped to COMMENT_IO_HOME by resolveCLIPaths.
	if paths.BusTCPAddr != "" {
		return true
	}
	if strings.TrimSpace(os.Getenv("COMMENT_IO_DAEMON_EXTERNAL")) != "1" {
		return false
	}
	// Scope the COMMENT_IO_DAEMON_EXTERNAL marker to COMMENT_IO_HOME, mirroring the
	// TCP-addr scoping: an explicit --home for a different (native) daemon still
	// gets normal doctor behavior (including native install/start repairs).
	envHome := strings.TrimSpace(os.Getenv("COMMENT_IO_HOME"))
	if envHome == "" {
		return false
	}
	cleanedEnvHome, err := commentbus.ExpandHome(envHome)
	return err == nil && paths.Home == cleanedEnvHome
}

func checkDaemonInstalled(paths commentbus.Paths, fix bool) doctorCheck {
	name := "daemon_installed"
	if daemonExternallyManaged(paths) {
		return okCheck(name, "daemon is managed by an external runtime (container); native launchd/systemd install is not used")
	}
	switch {
	case launchdSupported():
		_, cfg, err := newLaunchAgentConfig(paths.Home, "", "")
		if err != nil {
			return errorCheck(name, "could not derive launchd config: "+err.Error())
		}
		if !fileExists(cfg.PlistPath) {
			return doctorDaemonInstallOrSuggest(name, fix, "launchd plist missing")
		}
		return okCheck(name, "launchd plist installed at "+cfg.PlistPath)
	case systemdSupported():
		_, cfg, err := newSystemdServiceConfig(paths.Home, "", "")
		if err != nil {
			return errorCheck(name, "could not derive systemd config: "+err.Error())
		}
		if !fileExists(cfg.UnitPath) {
			return doctorDaemonInstallOrSuggest(name, fix, "systemd user unit missing")
		}
		return okCheck(name, "systemd user unit installed at "+cfg.UnitPath)
	default:
		return warnCheck(name, "no persistent daemon backend on this platform; run `comment bus run` under your own service manager")
	}
}

func doctorDaemonInstallOrSuggest(name string, fix bool, reason string) doctorCheck {
	if !fix {
		return warnCheck(name, reason+"; run with --fix to invoke `comment bus install`")
	}
	if _, err := busInstall("", "", "", false, false); err != nil {
		return doctorCheck{Name: name, Status: "error", Message: reason, FixError: err.Error()}
	}
	return fixedCheck(name, "ran `comment bus install`")
}

// checkDaemonPaired — the daemon is installed AND this computer holds daemon
// pairing credentials (bus/daemon-auth.json). "Installed but unpaired" is a
// distinct, fixable finding from "daemon not installed": with --fix on a
// terminal it invokes the `comment bus pair` flow directly; without a
// terminal it prints the exact command to run.
func checkDaemonPaired(paths commentbus.Paths, fix bool, daemonInstalled bool) doctorCheck {
	name := "daemon_paired"
	auth, paired, err := commentbus.LoadDaemonAuth(paths)
	if err != nil {
		return errorCheck(name, "daemon pairing credentials are unreadable: "+err.Error()+"; run `comment bus pair --force` to replace them")
	}
	if paired {
		return doctorCheck{
			Name:    name,
			Status:  "ok",
			Message: fmt.Sprintf("this computer is paired as %q", auth.Label),
			Detail:  map[string]any{"daemon_id": auth.DaemonID, "label": auth.Label},
		}
	}
	if !daemonInstalled {
		return warnCheck(name, "skipped: daemon is not installed; install it first (`comment bus install`), then run `comment bus pair`")
	}
	// Pairing needs a human in a browser and prints the verification URL/code
	// to stdout — running it inside doctor would interleave that output with
	// doctor's JSON result and corrupt it for any parsing caller. Always
	// report it as the next step instead of doing it here.
	pairHint := "daemon is installed but this computer is not paired; run `comment bus pair` to pair it with your Comment.io account (one time)"
	return warnCheck(name, pairHint)
}

// checkDaemonRunning — daemon process is loaded and responding on the
// unix socket.
func checkDaemonRunning(paths commentbus.Paths, fix bool) doctorCheck {
	name := "daemon_running"
	endpoint := paths.Socket
	if paths.BusTCPAddr != "" {
		endpoint = "tcp " + paths.BusTCPAddr
	}
	if daemonHealthy(paths) {
		return okCheck(name, "daemon responds to health on "+endpoint)
	}
	// When the daemon is externally managed (container) a native reinstall is
	// wrong: it can't serve the configured transport and could start a host daemon
	// against the cage's state dir, so never attempt it — point at that runtime.
	if daemonExternallyManaged(paths) {
		return warnCheck(name, "daemon is not responding on "+endpoint+"; it is managed by an external runtime (container) — check that runtime's status/logs rather than a native reinstall")
	}
	if !fix {
		return warnCheck(name, "daemon is not responding on "+endpoint+"; run with --fix to reinstall and kickstart")
	}
	if _, err := busInstall("", "", "", false, false); err != nil {
		return doctorCheck{Name: name, Status: "error", Message: "daemon not responding", FixError: err.Error()}
	}
	// Give launchd / systemd a moment to bring the daemon up.
	for i := 0; i < 30; i++ {
		if daemonHealthy(paths) {
			return fixedCheck(name, "daemon reinstalled and is responding on "+endpoint)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errorCheck(name, "daemon still not responding after reinstall; check `comment bus status` and ~/.comment-io/logs/commentd.err.log")
}

// checkDaemonProfiles — every agent profile file on disk is loaded in the
// daemon's profile state.
func checkDaemonProfiles(paths commentbus.Paths, fix bool) doctorCheck {
	name := "daemon_profiles"
	if !daemonHealthy(paths) {
		return warnCheck(name, "skipped: daemon is not running")
	}
	onDisk, err := agentProfileHandlesOnDisk(paths)
	if err != nil {
		return errorCheck(name, "could not enumerate profiles on disk: "+err.Error())
	}
	inspection, err := daemonAgentProfileHandles(paths)
	if err != nil {
		return errorCheck(name, "could not query daemon profile state: "+err.Error())
	}
	// Over a limited (TCP) transport the daemon withholds agent_profile_handles,
	// so we CANNOT verify which handles are loaded — only the count.
	if inspection.LimitedTransport && !inspection.ReportsHandles {
		// If the count proves staleness (fewer loaded than on disk) and --fix is
		// set, still repair: reload-profiles is supported over the authenticated
		// TCP transport. Then re-check the count and warn that handles remain
		// unverifiable over this transport.
		if fix && inspection.LoadedCount < len(onDisk) {
			if err := callDaemonReloadProfiles(paths); err != nil {
				return doctorCheck{Name: name, Status: "error", Message: "reload-profiles failed", FixError: err.Error()}
			}
			after, err := daemonAgentProfileHandles(paths)
			if err != nil {
				return errorCheck(name, "could not re-query daemon after reload: "+err.Error())
			}
			// Warning-only even on a post-reload count match: TCP health withholds
			// handles and load errors, so the count can't prove the RIGHT profiles
			// loaded (or that none errored). Don't report "fixed".
			return warnCheck(name, fmt.Sprintf("reloaded over the TCP transport; daemon reports %d of %d profile(s) loaded, but handles/load-errors aren't verifiable over TCP — run doctor against the Unix socket to confirm", after.LoadedCount, len(onDisk)))
		}
		// Count matches (or no --fix): a matching count doesn't prove the right
		// profiles are loaded, so don't report a misleading "ok".
		return warnCheck(
			name,
			fmt.Sprintf("daemon reachable over the TCP transport, which doesn't expose profile handles; cannot verify which of %d on-disk profile(s) are loaded (daemon reports %d loaded). Run doctor against the daemon's Unix socket for detailed inspection.", len(onDisk), inspection.LoadedCount),
		)
	}
	// Older daemons don't report agent_profile_handles. Fall back to the
	// counts: if profiles_loaded matches the file count, treat as ok and
	// suggest a daemon reinstall to enable detailed inspection.
	if !inspection.ReportsHandles {
		if inspection.LoadedCount >= len(onDisk) {
			return doctorCheck{
				Name:    name,
				Status:  "ok",
				Message: fmt.Sprintf("daemon reports %d profile(s) loaded; reinstall the daemon (`comment bus install`) for detailed inspection", inspection.LoadedCount),
				Detail:  map[string]any{"profiles_loaded": inspection.LoadedCount, "on_disk": onDisk},
			}
		}
		return warnCheck(
			name,
			fmt.Sprintf("daemon reports only %d profile(s) loaded but %d exist on disk; reinstall the daemon (`comment bus install`) and rerun doctor", inspection.LoadedCount, len(onDisk)),
		)
	}
	missing := diffHandles(onDisk, inspection.Handles)
	if len(missing) == 0 && len(inspection.Errors) == 0 {
		return doctorCheck{
			Name:    name,
			Status:  "ok",
			Message: fmt.Sprintf("daemon has %d agent profile(s) loaded", len(inspection.Handles)),
			Detail:  map[string]any{"loaded": inspection.Handles},
		}
	}
	detail := map[string]any{
		"on_disk":       onDisk,
		"loaded":        inspection.Handles,
		"missing":       missing,
		"loaded_errors": inspection.Errors,
	}
	if !fix {
		return doctorCheck{
			Name:    name,
			Status:  "warn",
			Message: fmt.Sprintf("%d profile(s) on disk are not loaded; run with --fix to reload", len(missing)),
			Detail:  detail,
		}
	}
	if err := callDaemonReloadProfiles(paths); err != nil {
		return doctorCheck{Name: name, Status: "error", Message: "reload-profiles failed", FixError: err.Error(), Detail: detail}
	}
	// Re-query after reload.
	after, err := daemonAgentProfileHandles(paths)
	if err != nil {
		return errorCheck(name, "could not re-query daemon after reload: "+err.Error())
	}
	return classifyPostReloadProfiles(name, onDisk, after)
}

// classifyPostReloadProfiles maps a post-reload daemon profile state to a
// doctor check status. Extracted so that the all-loaded-but-errors-remain
// case has unit coverage.
func classifyPostReloadProfiles(name string, onDisk []string, after daemonProfileInspection) doctorCheck {
	missingAfter := diffHandles(onDisk, after.Handles)
	if len(missingAfter) == 0 && len(after.Errors) == 0 {
		return fixedCheck(name, fmt.Sprintf("reloaded; daemon now has %d agent profile(s)", len(after.Handles)))
	}
	detail := map[string]any{
		"missing":       missingAfter,
		"loaded":        after.Handles,
		"loaded_errors": after.Errors,
	}
	if len(missingAfter) > 0 {
		return doctorCheck{
			Name:       name,
			Status:     "error",
			Message:    fmt.Sprintf("%d profile(s) still missing after reload", len(missingAfter)),
			FixApplied: true,
			Detail:     detail,
		}
	}
	// All handles loaded, but the daemon still reports profile errors
	// (e.g. a sibling profile that refuses to load, or a bus-config
	// write failure). Surface as warn so the operator notices.
	return doctorCheck{
		Name:       name,
		Status:     "warn",
		Message:    fmt.Sprintf("reload applied but %d profile-load error(s) remain; run `comment bus reload-profiles` for details", len(after.Errors)),
		FixApplied: true,
		Detail:     detail,
	}
}

func agentProfileHandlesOnDisk(paths commentbus.Paths) ([]string, error) {
	agentsDir := filepath.Join(paths.Home, "agents")
	entries, err := os.ReadDir(agentsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var handles []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		handles = append(handles, strings.TrimSuffix(entry.Name(), ".json"))
	}
	sort.Strings(handles)
	return handles, nil
}

type daemonProfileInspection struct {
	Handles          []string
	LoadedCount      int
	Errors           []any
	ReportsHandles   bool
	LimitedTransport bool
}

func daemonAgentProfileHandles(paths commentbus.Paths) (daemonProfileInspection, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := commentbus.GenerateSocketRequestID()
	if err != nil {
		return daemonProfileInspection{}, err
	}
	resp, err := commentbus.CallSocket(ctx, paths, commentbus.SocketRequest{
		ID:     id,
		Op:     "health",
		Params: map[string]any{},
	}, 5*time.Second)
	if err != nil {
		return daemonProfileInspection{}, err
	}
	if !resp.OK {
		return daemonProfileInspection{}, fmt.Errorf("daemon health returned error: %v", resp.Error)
	}
	resultMap, ok := resp.Result.(map[string]any)
	if !ok {
		return daemonProfileInspection{}, errors.New("daemon health result was not an object")
	}
	inspection := daemonProfileInspection{}
	if loaded, ok := resultMap["profiles_loaded"].(float64); ok {
		inspection.LoadedCount = int(loaded)
	}
	if limited, ok := resultMap["limited"].(bool); ok {
		inspection.LimitedTransport = limited
	}
	rawProfiles, present := resultMap["agent_profile_handles"].([]any)
	inspection.ReportsHandles = present
	for _, h := range rawProfiles {
		if s, ok := h.(string); ok {
			inspection.Handles = append(inspection.Handles, s)
		}
	}
	sort.Strings(inspection.Handles)
	if rawErrors, ok := resultMap["profile_load_errors"].([]any); ok {
		inspection.Errors = rawErrors
	}
	return inspection, nil
}

func callDaemonReloadProfiles(paths commentbus.Paths) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, err := commentbus.GenerateSocketRequestID()
	if err != nil {
		return err
	}
	resp, err := commentbus.CallSocket(ctx, paths, commentbus.SocketRequest{
		ID:     id,
		Op:     "reload-profiles",
		Auth:   ownerAuthFromCapability(paths),
		Params: map[string]any{},
	}, 10*time.Second)
	if err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("reload-profiles returned error: %v", resp.Error)
	}
	return nil
}

func ownerAuthFromCapability(paths commentbus.Paths) *commentbus.SocketAuth {
	capability, err := commentbus.ReadPrivateCapability(paths.Home, paths.OwnerCapability, "owner capability file")
	if err != nil {
		return nil
	}
	return &commentbus.SocketAuth{Mode: "owner", Capability: capability}
}

func diffHandles(onDisk, loaded []string) []string {
	loadedSet := map[string]struct{}{}
	for _, h := range loaded {
		loadedSet[h] = struct{}{}
	}
	var missing []string
	for _, h := range onDisk {
		if _, ok := loadedSet[h]; !ok {
			missing = append(missing, h)
		}
	}
	return missing
}

// daemonHealthy returns true if the daemon's control transport accepts a
// connection and answers a health op successfully. It honors the opt-in TCP
// transport (paths.BusTCPAddr) so the cross-boundary case (Unix socket
// unreachable, TCP reachable) reports the daemon as up rather than down.
func daemonHealthy(paths commentbus.Paths) bool {
	network, address := "unix", paths.Socket
	if paths.BusTCPAddr != "" {
		network, address = "tcp", paths.BusTCPAddr
	} else if _, err := os.Stat(paths.Socket); err != nil {
		return false
	}
	conn, err := net.DialTimeout(network, address, 1*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	id, err := commentbus.GenerateSocketRequestID()
	if err != nil {
		return false
	}
	req := commentbus.SocketRequest{ID: id, Op: "health", Params: map[string]any{}}
	encoded, err := json.Marshal(req)
	if err != nil {
		return false
	}
	if _, err := conn.Write(append(encoded, '\n')); err != nil {
		return false
	}
	// Read a single line response.
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if idx := indexOfByte(buf, '\n'); idx >= 0 {
				buf = buf[:idx]
				break
			}
		}
		if err != nil {
			return false
		}
		if len(buf) > 1<<20 {
			return false
		}
	}
	var resp commentbus.SocketResponse
	if err := json.Unmarshal(buf, &resp); err != nil {
		return false
	}
	return resp.OK
}

func indexOfByte(buf []byte, b byte) int {
	for i, c := range buf {
		if c == b {
			return i
		}
	}
	return -1
}

// validateOwnerIsCurrentUser is implemented per-platform in
// doctor_unix.go and doctor_unsupported.go.
