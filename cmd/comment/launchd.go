package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

const launchdLabelPrefix = "io.comment.commentd"

var launchdSupported = func() bool {
	return runtime.GOOS == "darwin"
}

var systemdSupported = func() bool {
	return runtime.GOOS == "linux"
}

var launchctlCombinedOutput = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "launchctl", args...)
	return cmd.CombinedOutput()
}

var systemctlCombinedOutput = func(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	return cmd.CombinedOutput()
}

type launchAgentConfig struct {
	Label            string
	Home             string
	Environment      string
	BaseURL          string
	BotletsHome      string
	BinaryPath       string
	TmuxBinary       string
	BmuxBinary       string
	PlistPath        string
	StdoutPath       string
	StderrPath       string
	ProgramArguments []string
}

type systemdServiceConfig struct {
	Label            string
	UnitName         string
	Home             string
	Environment      string
	BaseURL          string
	BotletsHome      string
	BinaryPath       string
	TmuxBinary       string
	BmuxBinary       string
	UnitPath         string
	StdoutPath       string
	StderrPath       string
	ProgramArguments []string
}

// stagingServiceEnvironment returns the environment name to bake into a daemon
// service definition, or "" for production. Production service files are left
// untouched so existing installs do not churn; only staging needs the explicit
// COMMENT_IO_ENV so the installed daemon resolves staging defaults (base URL and
// synced-docs root) even though its --home is already staging-specific.
func stagingServiceEnvironment() string {
	if env := commentbus.CurrentEnvironment(); env.IsStaging() {
		return env.Name
	}
	return ""
}

// installTmuxBinaryPin captures an explicit tmux pin (COMMENT_IO_TMUX_BIN) at
// install time so it can be baked into the generated service's environment. A
// shell-level COMMENT_IO_TMUX_BIN is not inherited by launchd/systemd services,
// so persisting it here is the only way an installed daemon can honor the pin.
// Only an absolute path is persisted; the bare "tmux" default is left to the
// daemon's trusted-directory auto-discovery.
func installTmuxBinaryPin() string {
	return installBinaryPin(commentbus.ResolveConfiguredTmuxBinary(""), "tmux")
}

func installBmuxBinaryPin() string {
	return installBinaryPin(commentbus.ResolveConfiguredBmuxBinary(""), "bmux")
}

func installBinaryPin(pin string, defaultName string) string {
	if pin == defaultName || !filepath.IsAbs(pin) {
		return ""
	}
	return pin
}

func runBusInstall(args []string) error {
	fs := flag.NewFlagSet("comment bus install", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	botletsHome := fs.String("botlets-home", "", "Botlets home directory")
	bin := fs.String("bin", "", "comment binary path")
	dryRun := fs.Bool("dry-run", false, "print the service definition without installing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("bus install does not accept positional arguments")
	}
	result, err := busInstall(*home, *botletsHome, *bin, *dryRun, true)
	if err != nil {
		return err
	}
	return printJSON(result)
}

// busInstall performs the install work without writing to stdout. The
// returned map matches what `comment bus install` would have printed.
// Used by `comment doctor --fix` so doctor can emit a single JSON
// document for the whole run. Exception: on an interactive session with no
// daemon pairing credentials, the chained pair flow (busInstallPairChain)
// prints the verification URL and code — the same output `comment bus pair`
// produces.
//
// pair gates that chained device-pair flow. Human-driven callers
// (`comment bus install`, `comment doctor --fix`) pass true; unattended
// callers (the auto-update reinstall) MUST pass false: stdin in a service
// context is commonly /dev/null, which IS a character device, so the
// stdinIsInteractive() heuristic can report true and an unpaired background
// reinstall would block forever waiting for browser approval.
func busInstall(home string, botletsHome string, bin string, dryRun bool, pair bool) (map[string]any, error) {
	installBotletsHome := botletsHome
	if installBotletsHome == "" {
		installBotletsHome = os.Getenv("BOTLETS_HOME")
	}
	// Managed runtimes default to tmux, so install no longer auto-provisions the
	// in-house bmux multiplexer. bmux remains an explicit opt-in: when the
	// operator pins COMMENT_IO_BMUX_BIN, newLaunchAgentConfig still bakes that
	// pin into the plist/unit so a host=bmux session can resolve it. We pass an
	// empty override pin here (no fresh install path to bake).
	var (
		result map[string]any
		err    error
	)
	switch {
	case systemdSupported() && !launchdSupported():
		result, err = busInstallSystemdResult(home, installBotletsHome, bin, dryRun, "")
	case !launchdSupported():
		return nil, unsupportedPersistentServiceError("install")
	default:
		result, err = busInstallLaunchdResult(home, installBotletsHome, bin, dryRun, "")
	}
	if err != nil {
		return nil, err
	}
	// Surface a non-fatal advisory when the daemon won't be able to resolve tmux
	// (it's an OS package we can't auto-install). The runtime launch path itself
	// returns the dedicated, friendly TMUX_NOT_INSTALLED error; this just lets the
	// user fix it at install time rather than at first `comment run`. Resolve the
	// way the installed service will: an absolute COMMENT_IO_TMUX_BIN is baked into
	// the unit (installTmuxBinaryPin), otherwise the background daemon resolves bare
	// "tmux" from trusted dirs and does NOT inherit the caller's shell env — so a
	// non-persisted COMMENT_IO_TMUX_BIN must not silence this advisory.
	tmuxResolveInput := installTmuxBinaryPin()
	if tmuxResolveInput == "" {
		tmuxResolveInput = "tmux"
	}
	if _, tmuxErr := commentbus.ResolveDaemonTmuxBinary(tmuxResolveInput); tmuxErr != nil {
		result["tmux"] = map[string]any{"ok": false, "hint": commentbus.TmuxInstallHintShort()}
	}
	// Pairing is part of install (daemon-mediated agent enrollment, Phase 2):
	// after the service install succeeds, chain straight into the device pair
	// flow when unpaired. Skipped on dry-run, which must not touch the
	// filesystem or the network, and when the caller disabled pairing
	// (unattended auto-update reinstalls).
	if !dryRun && pair {
		if paths, pathsErr := resolveCLIPaths(home); pathsErr == nil {
			busInstallPairChain(result, paths, busInstallStdinIsInteractive(), func() error {
				return busInstallRunPair(paths.Home)
			})
		}
	}
	return result, nil
}

// Pair-chain hooks. Package vars so tests can drive busInstall hermetically:
// `go test` gives test binaries /dev/null as stdin, which IS a character
// device, so stdinIsInteractive() reports true there — an unstubbed test on an
// unpaired temp home would enter the real network pair flow.
var (
	busInstallStdinIsInteractive = stdinIsInteractive
	busInstallRunPair            = func(home string) error {
		return runBusPair([]string{"--home", home})
	}
)

// busInstallPairFollowup is the non-interactive nudge surfaced when the daemon
// installed fine but this computer holds no pairing credentials.
const busInstallPairFollowup = "run 'comment bus pair' to pair this computer with your Comment.io account (one time)"

// busInstallPairChain implements the plan's "pairing is part of install"
// contract (docs/plans/daemon-mediated-agent-enrollment.md): after a
// successful daemon service install, an unpaired interactive session enters
// the pair flow directly; an unpaired non-interactive session surfaces the
// one-time `comment bus pair` follow-up instead (automation must never block
// waiting for browser approval). A pair failure is recorded as pair_warning —
// never an install failure. Already paired adds nothing.
//
// The hosted install scripts (agent-docs.ts) also chain `comment bus pair`
// after `comment bus install`; that second pair is a no-op because `bus pair`
// exits early when daemon-auth.json already exists (no --force), so chaining
// here cannot double-pair.
func busInstallPairChain(result map[string]any, paths commentbus.Paths, interactive bool, pairFn func() error) {
	_, paired, err := commentbus.LoadDaemonAuth(paths)
	if err != nil {
		// Present-but-unusable credentials: `comment bus pair` without --force
		// refuses to overwrite them, so don't enter the pair flow — report.
		result["pair_warning"] = "daemon pairing credentials are unreadable: " + err.Error() + "; run `comment bus pair --force` to replace them"
		return
	}
	if paired {
		return
	}
	if !interactive {
		result["pair_followup"] = busInstallPairFollowup
		return
	}
	if pairErr := pairFn(); pairErr != nil {
		result["pair_warning"] = pairErr.Error()
	}
}

// ensureBmuxInstalledFn is the bmux auto-install hook. It is a package var so
// tests can exercise `comment bus install` / `comment doctor --fix` without
// reaching the network (the real installer is tested directly in commentbus).
var ensureBmuxInstalledFn = commentbus.EnsureBmuxInstalled

// ensureBmuxForInstall runs the bmux auto-install and returns a JSON-friendly
// summary plus the COMMENT_IO_BMUX_BIN pin (if any) the service must bake so the
// daemon can resolve the binary. It never returns an error: a failed bmux
// install is reported, not fatal, so the daemon still installs.
func ensureBmuxForInstall() (summary map[string]any, pin string) {
	res, err := ensureBmuxInstalledFn(commentbus.BmuxInstallOptions{})
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}, ""
	}
	summary = map[string]any{
		"ok":              true,
		"path":            res.Path,
		"installed":       res.Installed,
		"already_present": res.AlreadyPresent,
		"discoverable":    res.Discoverable,
	}
	// installBmuxBinaryPin() reads COMMENT_IO_BMUX_BIN; whatever it returns is the
	// pin newLaunchAgentConfig() bakes into the service unless we override it here.
	// The daemon does NOT inherit the shell environment and resolves bare "bmux"
	// only from trusted directories, so decide what (if anything) the service must
	// pin so the daemon can actually launch the binary.
	if envPin := installBmuxBinaryPin(); envPin != "" {
		// An operator-provided COMMENT_IO_BMUX_BIN is normally baked verbatim. Two
		// cases force us to replace it instead:
		//   1. The pin is stale/rejected by the daemon's resolver — baking it would
		//      report bmux.ok=true while the daemon launches a bad path and
		//      `comment run` stays broken.
		//   2. A fresh download just happened (res.Installed) for a different binary.
		//      EnsureBmuxInstalled only falls through its AlreadyPresent precheck
		//      onto a download when the pinned binary is a *different release
		//      channel* than the one we just installed (see its channel-marker
		//      logic); re-baking that stale cross-channel pin would reintroduce the
		//      protocol mismatch the reinstall was meant to fix.
		_, envPinErr := commentbus.TrustedBmuxBinaryPath(envPin)
		envPinUsable := envPinErr == nil
		crossChannelReinstall := res.Installed && filepath.IsAbs(res.Path) && res.Path != envPin
		if envPinUsable && !crossChannelReinstall {
			return summary, "" // honor the usable operator pin verbatim
		}
		if filepath.IsAbs(res.Path) {
			if _, perr := commentbus.TrustedBmuxBinaryPath(res.Path); perr == nil {
				pin = res.Path
				summary["service_pin"] = pin
				summary["replaced_env_pin"] = envPin
				return summary, pin
			}
		}
		if envPinUsable {
			// The freshly installed binary isn't daemon-usable but the existing pin
			// is; keep the pin rather than break a working setup.
			return summary, ""
		}
		summary["ok"] = false
		summary["error"] = "COMMENT_IO_BMUX_BIN=" + envPin + " is not usable by the background daemon, and bmux installed at " + res.Path + " cannot be used either; install bmux under ~/.local/bin or set COMMENT_IO_BMUX_BIN to an absolute path the daemon can resolve"
		return summary, ""
	}
	// No operator pin. When the daemon can't auto-discover the binary, pin the
	// service to the exact path we installed — but only if the daemon's resolver
	// would actually accept that path. The resolver rejects unsafe locations (e.g.
	// a group-/world-writable install dir); baking such a pin would report success
	// while `comment run` still fails, so we surface it as unusable instead.
	if !res.Discoverable && filepath.IsAbs(res.Path) {
		if _, perr := commentbus.TrustedBmuxBinaryPath(res.Path); perr == nil {
			pin = res.Path
			summary["service_pin"] = pin
		} else {
			summary["ok"] = false
			summary["error"] = "bmux installed at " + res.Path + " but the daemon cannot use it (" + perr.Error() + "); install it under ~/.local/bin or another trusted directory"
		}
	}
	return summary, pin
}

func runBusInstallLaunchd(home string, botletsHome string, bin string, dryRun bool) error {
	result, err := busInstallLaunchdResult(home, botletsHome, bin, dryRun, "")
	if err != nil {
		return err
	}
	return printJSON(result)
}

// busInstallLaunchdResult installs the launchd service. bmuxPin, when non-empty,
// overrides the COMMENT_IO_BMUX_BIN baked into the plist (used when the bmux
// auto-install landed somewhere the unpinned daemon cannot discover).
func busInstallLaunchdResult(home string, botletsHome string, bin string, dryRun bool, bmuxPin string) (map[string]any, error) {
	paths, cfg, err := newLaunchAgentConfig(home, botletsHome, bin)
	if err != nil {
		return nil, err
	}
	if bmuxPin != "" {
		cfg.BmuxBinary = bmuxPin
	}
	plist := buildLaunchAgentPlist(cfg)
	if dryRun {
		return launchdResult(paths, cfg, map[string]any{
			"dry_run": true,
			"plist":   plist,
		}), nil
	}
	ctx := context.Background()
	store, err := commentbus.OpenStore(ctx, paths)
	if err != nil {
		return nil, err
	}
	capability, err := commentbus.EnsureOwnerCapability(paths)
	closeErr := store.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: cfg.BotletsHome}); err != nil {
		return nil, err
	}
	if err := writeLaunchAgentPlist(cfg.PlistPath, []byte(plist)); err != nil {
		return nil, err
	}
	if err := launchdBootoutIfLoaded(cfg.Label); err != nil {
		return nil, err
	}
	if err := reclaimDaemonSocket(paths.Socket); err != nil {
		return nil, err
	}
	if err := launchdBootstrapWithRetry(launchdDomainTarget(), cfg.PlistPath); err != nil {
		return nil, err
	}
	if err := runLaunchctl("kickstart", launchdServiceTarget(cfg.Label)); err != nil {
		return nil, err
	}
	return launchdResult(paths, cfg, map[string]any{
		"installed":             true,
		"loaded":                true,
		"owner_capability_path": capability.Path,
		"owner_capability_new":  capability.Created,
	}), nil
}

func runBusStart(args []string) error {
	fs := flag.NewFlagSet("comment bus start", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("bus start does not accept positional arguments")
	}
	if systemdSupported() && !launchdSupported() {
		return runBusStartSystemd(*home)
	}
	if !launchdSupported() {
		return unsupportedPersistentServiceError("start")
	}
	paths, cfg, err := newLaunchAgentConfig(*home, "", "")
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.PlistPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("launchd service is not installed; run `comment bus install` first")
		}
		return err
	}
	loaded, err := launchdServiceLoaded(cfg.Label)
	if err != nil {
		return err
	}
	if !loaded {
		if err := launchdBootstrapWithRetry(launchdDomainTarget(), cfg.PlistPath); err != nil {
			return err
		}
	}
	if err := runLaunchctl("kickstart", launchdServiceTarget(cfg.Label)); err != nil {
		return err
	}
	return printJSON(launchdResult(paths, cfg, map[string]any{
		"installed": true,
		"loaded":    true,
	}))
}

func runBusStop(args []string) error {
	fs := flag.NewFlagSet("comment bus stop", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("bus stop does not accept positional arguments")
	}
	if systemdSupported() && !launchdSupported() {
		return runBusStopSystemd(*home)
	}
	if !launchdSupported() {
		return unsupportedPersistentServiceError("stop")
	}
	paths, cfg, err := newLaunchAgentConfig(*home, "", "")
	if err != nil {
		return err
	}
	if err := launchdBootoutIfLoaded(cfg.Label); err != nil {
		return err
	}
	return printJSON(launchdResult(paths, cfg, map[string]any{
		"installed": fileExists(cfg.PlistPath),
		"loaded":    false,
	}))
}

func runBusStatus(args []string) error {
	fs := flag.NewFlagSet("comment bus status", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("bus status does not accept positional arguments")
	}
	if systemdSupported() && !launchdSupported() {
		return runBusStatusSystemd(*home)
	}
	if !launchdSupported() {
		paths, err := resolveCLIPaths(*home)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{
			"ok":           true,
			"supported":    false,
			"installed":    false,
			"loaded":       false,
			"home":         paths.Home,
			"socket_path":  paths.Socket,
			"history_path": paths.History,
			"message":      unsupportedPersistentServiceMessage("status"),
		})
	}
	paths, cfg, err := newLaunchAgentConfig(*home, "", "")
	if err != nil {
		return err
	}
	installed := fileExists(cfg.PlistPath)
	loaded := false
	if launchdSupported() {
		loaded, err = launchdServiceLoaded(cfg.Label)
		if err != nil {
			return err
		}
	}
	return printJSON(launchdResult(paths, cfg, map[string]any{
		"supported": launchdSupported(),
		"installed": installed,
		"loaded":    loaded,
	}))
}

func runBusUninstall(args []string) error {
	fs := flag.NewFlagSet("comment bus uninstall", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("bus uninstall does not accept positional arguments")
	}
	result, err := busUninstall(*home)
	if err != nil {
		return err
	}
	return printJSON(result)
}

func busUninstall(home string) (map[string]any, error) {
	if systemdSupported() && !launchdSupported() {
		return busUninstallSystemdResult(home)
	}
	if !launchdSupported() {
		return nil, unsupportedPersistentServiceError("uninstall")
	}
	return busUninstallLaunchdResult(home)
}

func busUninstallLaunchdResult(home string) (map[string]any, error) {
	paths, cfg, err := newLaunchAgentUninstallConfig(home)
	if err != nil {
		return nil, err
	}
	if err := launchdBootoutIfLoaded(cfg.Label); err != nil {
		return nil, err
	}
	if err := os.Remove(cfg.PlistPath); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return launchdResult(paths, cfg, map[string]any{
		"installed": false,
		"loaded":    false,
	}), nil
}

func newLaunchAgentUninstallConfig(home string) (commentbus.Paths, launchAgentConfig, error) {
	paths, err := resolveCLIPaths(home)
	if err != nil {
		return commentbus.Paths{}, launchAgentConfig{}, err
	}
	launchAgents, err := userLaunchAgentsDir()
	if err != nil {
		return commentbus.Paths{}, launchAgentConfig{}, err
	}
	label := launchdLabelForHome(paths.Home)
	cfg := launchAgentConfig{
		Label:     label,
		Home:      paths.Home,
		PlistPath: filepath.Join(launchAgents, label+".plist"),
	}
	return paths, cfg, nil
}

func newLaunchAgentConfig(home string, botletsHome string, bin string) (commentbus.Paths, launchAgentConfig, error) {
	paths, err := resolveCLIPaths(home)
	if err != nil {
		return commentbus.Paths{}, launchAgentConfig{}, err
	}
	if botletsHome == "" {
		if config, ok, err := commentbus.ReadBusConfig(paths); err == nil && ok && config.BotletsHome != "" {
			botletsHome = config.BotletsHome
		}
	}
	resolvedBotletsHome, err := commentbus.ResolveBotletsHome(botletsHome)
	if err != nil {
		return commentbus.Paths{}, launchAgentConfig{}, err
	}
	binaryPath, err := resolveLaunchdBinaryPath(bin)
	if err != nil {
		return commentbus.Paths{}, launchAgentConfig{}, err
	}
	launchAgents, err := userLaunchAgentsDir()
	if err != nil {
		return commentbus.Paths{}, launchAgentConfig{}, err
	}
	label := launchdLabelForHome(paths.Home)
	cfg := launchAgentConfig{
		Label:            label,
		Home:             paths.Home,
		Environment:      stagingServiceEnvironment(),
		BaseURL:          commentbus.StagingServiceBaseURLOverride(),
		BotletsHome:      resolvedBotletsHome,
		BinaryPath:       binaryPath,
		TmuxBinary:       installTmuxBinaryPin(),
		BmuxBinary:       installBmuxBinaryPin(),
		PlistPath:        filepath.Join(launchAgents, label+".plist"),
		StdoutPath:       filepath.Join(paths.Logs, "commentd.out.log"),
		StderrPath:       filepath.Join(paths.Logs, "commentd.err.log"),
		ProgramArguments: []string{binaryPath, "bus", "run", "--home", paths.Home},
	}
	return paths, cfg, nil
}

func launchdLabelForHome(home string) string {
	sum := sha256.Sum256([]byte(canonicalLaunchdHome(home)))
	return launchdLabelPrefix + "." + hex.EncodeToString(sum[:])[:12]
}

func canonicalLaunchdHome(home string) string {
	clean := filepath.Clean(home)
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return filepath.Clean(resolved)
	}
	for cursor := clean; ; cursor = filepath.Dir(cursor) {
		resolved, err := filepath.EvalSymlinks(cursor)
		if err == nil {
			rel, relErr := filepath.Rel(cursor, clean)
			if relErr != nil || rel == "." {
				return filepath.Clean(resolved)
			}
			return filepath.Clean(filepath.Join(resolved, rel))
		}
		parent := filepath.Dir(cursor)
		if parent == cursor {
			return clean
		}
	}
}

func resolveLaunchdBinaryPath(bin string) (string, error) {
	if bin == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", err
		}
		bin = executable
	}
	if !filepath.IsAbs(bin) {
		abs, err := filepath.Abs(bin)
		if err != nil {
			return "", err
		}
		bin = abs
	}
	clean := filepath.Clean(bin)
	info, err := os.Stat(clean)
	if err != nil {
		return "", fmt.Errorf("comment binary must exist: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("comment binary must be a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("comment binary must be executable")
	}
	return clean, nil
}

func userLaunchAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

func buildLaunchAgentPlist(cfg launchAgentConfig) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString(`<plist version="1.0">` + "\n")
	b.WriteString("<dict>\n")
	writePlistString(&b, "Label", cfg.Label)
	b.WriteString("\t<key>ProgramArguments</key>\n")
	b.WriteString("\t<array>\n")
	for _, arg := range cfg.ProgramArguments {
		b.WriteString("\t\t<string>")
		b.WriteString(xmlText(arg))
		b.WriteString("</string>\n")
	}
	b.WriteString("\t</array>\n")
	b.WriteString("\t<key>EnvironmentVariables</key>\n")
	b.WriteString("\t<dict>\n")
	writePlistStringIndented(&b, "COMMENT_IO_HOME", cfg.Home, 2)
	if cfg.Environment != "" {
		writePlistStringIndented(&b, "COMMENT_IO_ENV", cfg.Environment, 2)
	}
	if cfg.BaseURL != "" {
		writePlistStringIndented(&b, "COMMENT_IO_BASE_URL", cfg.BaseURL, 2)
	}
	if cfg.TmuxBinary != "" {
		writePlistStringIndented(&b, "COMMENT_IO_TMUX_BIN", cfg.TmuxBinary, 2)
	}
	if cfg.BmuxBinary != "" {
		writePlistStringIndented(&b, "COMMENT_IO_BMUX_BIN", cfg.BmuxBinary, 2)
	}
	b.WriteString("\t</dict>\n")
	writePlistString(&b, "WorkingDirectory", cfg.Home)
	writePlistString(&b, "StandardOutPath", cfg.StdoutPath)
	writePlistString(&b, "StandardErrorPath", cfg.StderrPath)
	writePlistString(&b, "ProcessType", "Background")
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n")
	b.WriteString("\t<key>KeepAlive</key>\n\t<true/>\n")
	b.WriteString("</dict>\n")
	b.WriteString("</plist>\n")
	return b.String()
}

func writePlistString(b *strings.Builder, key string, value string) {
	writePlistStringIndented(b, key, value, 1)
}

func writePlistStringIndented(b *strings.Builder, key string, value string, depth int) {
	indent := strings.Repeat("\t", depth)
	b.WriteString(indent)
	b.WriteString("<key>")
	b.WriteString(xmlText(key))
	b.WriteString("</key>\n")
	b.WriteString(indent)
	b.WriteString("<string>")
	b.WriteString(xmlText(value))
	b.WriteString("</string>\n")
}

func xmlText(value string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(value))
	return b.String()
}

func writeLaunchAgentPlist(path string, data []byte) error {
	return writeServiceFile(path, data)
}

func writeServiceFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	ok = true
	return os.Chmod(path, 0o644)
}

func launchdBootoutIfLoaded(label string) error {
	loaded, err := launchdServiceLoaded(label)
	if err != nil {
		return err
	}
	if !loaded {
		return nil
	}
	return runLaunchctl("bootout", launchdServiceTarget(label))
}

func launchdServiceLoaded(label string) (bool, error) {
	output, err := runLaunchctlOutput("print", launchdServiceTarget(label))
	if err == nil {
		return true, nil
	}
	text := strings.ToLower(string(output))
	for _, marker := range []string{"could not find service", "no such process", "not found", "does not exist"} {
		if strings.Contains(text, marker) {
			return false, nil
		}
	}
	return false, err
}

func runLaunchctl(args ...string) error {
	_, err := runLaunchctlOutput(args...)
	return err
}

func runLaunchctlOutput(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := launchctlCombinedOutput(ctx, args...)
	if err == nil {
		return output, nil
	}
	if len(output) == 0 {
		return output, fmt.Errorf("launchctl %s failed: %w", strings.Join(args, " "), err)
	}
	return output, fmt.Errorf("launchctl %s failed: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
}

func launchdDomainTarget() string {
	guiDomain := launchdGUIDomainTarget()
	if launchdDomainUnsupported(guiDomain) {
		return launchdUserDomainTarget()
	}
	return guiDomain
}

func launchdGUIDomainTarget() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

func launchdUserDomainTarget() string {
	return "user/" + strconv.Itoa(os.Getuid())
}

func launchdServiceTarget(label string) string {
	return launchdDomainTarget() + "/" + label
}

func launchdDomainUnsupported(domain string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := launchctlCombinedOutput(ctx, "print", domain)
	if err == nil {
		return false
	}
	return isUnsupportedLaunchdDomainText(string(output)) || isUnsupportedLaunchdDomainText(err.Error())
}

func isUnsupportedLaunchdDomainText(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "domain does not support specified action") ||
		(strings.Contains(lower, "could not print domain") && strings.Contains(lower, "125"))
}

func launchdResult(paths commentbus.Paths, cfg launchAgentConfig, extra map[string]any) map[string]any {
	result := map[string]any{
		"ok":                true,
		"service_manager":   "launchd",
		"label":             cfg.Label,
		"home":              paths.Home,
		"botlets_home":      cfg.BotletsHome,
		"socket_path":       paths.Socket,
		"history_path":      paths.History,
		"plist_path":        cfg.PlistPath,
		"program_arguments": cfg.ProgramArguments,
	}
	for key, value := range extra {
		result[key] = value
	}
	return result
}

func runBusInstallSystemd(home string, botletsHome string, bin string, dryRun bool) error {
	result, err := busInstallSystemdResult(home, botletsHome, bin, dryRun, "")
	if err != nil {
		return err
	}
	return printJSON(result)
}

// busInstallSystemdResult installs the systemd user service. bmuxPin, when
// non-empty, overrides the COMMENT_IO_BMUX_BIN baked into the unit (used when the
// bmux auto-install landed somewhere the unpinned daemon cannot discover).
func busInstallSystemdResult(home string, botletsHome string, bin string, dryRun bool, bmuxPin string) (map[string]any, error) {
	paths, cfg, err := newSystemdServiceConfig(home, botletsHome, bin)
	if err != nil {
		return nil, err
	}
	if bmuxPin != "" {
		cfg.BmuxBinary = bmuxPin
	}
	unit := buildSystemdUnit(cfg)
	if dryRun {
		return systemdResult(paths, cfg, map[string]any{
			"dry_run": true,
			"unit":    unit,
		}), nil
	}
	ctx := context.Background()
	store, err := commentbus.OpenStore(ctx, paths)
	if err != nil {
		return nil, err
	}
	capability, err := commentbus.EnsureOwnerCapability(paths)
	closeErr := store.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: cfg.BotletsHome}); err != nil {
		return nil, err
	}
	if err := writeServiceFile(cfg.UnitPath, []byte(unit)); err != nil {
		return nil, err
	}
	if err := runSystemctlUser("daemon-reload"); err != nil {
		return nil, err
	}
	if err := runSystemctlUser("stop", cfg.UnitName); err != nil && !isSystemdMissingServiceError(err) {
		return nil, err
	}
	if err := reclaimDaemonSocket(paths.Socket); err != nil {
		return nil, err
	}
	if err := runSystemctlUser("enable", "--now", cfg.UnitName); err != nil {
		return nil, err
	}
	if err := runSystemctlUser("restart", cfg.UnitName); err != nil {
		return nil, err
	}
	return systemdResult(paths, cfg, map[string]any{
		"installed":             true,
		"loaded":                true,
		"owner_capability_path": capability.Path,
		"owner_capability_new":  capability.Created,
	}), nil
}

func runBusStartSystemd(home string) error {
	paths, cfg, err := newSystemdServiceConfig(home, "", "")
	if err != nil {
		return err
	}
	if _, err := os.Stat(cfg.UnitPath); err != nil {
		if os.IsNotExist(err) {
			return errors.New("systemd user service is not installed; run `comment bus install` first")
		}
		return err
	}
	if err := runSystemctlUser("daemon-reload"); err != nil {
		return err
	}
	if err := runSystemctlUser("start", cfg.UnitName); err != nil {
		return err
	}
	return printJSON(systemdResult(paths, cfg, map[string]any{
		"installed": true,
		"loaded":    true,
	}))
}

func runBusStopSystemd(home string) error {
	paths, cfg, err := newSystemdServiceConfig(home, "", "")
	if err != nil {
		return err
	}
	if active, err := systemdServiceActive(cfg.UnitName); err != nil {
		return err
	} else if active {
		if err := runSystemctlUser("stop", cfg.UnitName); err != nil {
			return err
		}
	}
	return printJSON(systemdResult(paths, cfg, map[string]any{
		"installed": fileExists(cfg.UnitPath),
		"loaded":    false,
	}))
}

func runBusStatusSystemd(home string) error {
	paths, cfg, err := newSystemdServiceConfig(home, "", "")
	if err != nil {
		return err
	}
	loaded := false
	supported := true
	message := ""
	if active, err := systemdServiceActive(cfg.UnitName); err == nil {
		loaded = active
	} else {
		supported = false
		message = err.Error()
	}
	return printJSON(systemdResult(paths, cfg, map[string]any{
		"supported": supported,
		"installed": fileExists(cfg.UnitPath),
		"loaded":    loaded,
		"message":   message,
	}))
}

func runBusUninstallSystemd(home string) error {
	result, err := busUninstallSystemdResult(home)
	if err != nil {
		return err
	}
	return printJSON(result)
}

func busUninstallSystemdResult(home string) (map[string]any, error) {
	paths, cfg, err := newSystemdUninstallConfig(home)
	if err != nil {
		return nil, err
	}
	if err := runSystemctlUser("disable", "--now", cfg.UnitName); err != nil && !isSystemdMissingServiceError(err) {
		return nil, err
	}
	if err := os.Remove(cfg.UnitPath); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := runSystemctlUser("daemon-reload"); err != nil {
		return nil, err
	}
	return systemdResult(paths, cfg, map[string]any{
		"installed": false,
		"loaded":    false,
	}), nil
}

func newSystemdUninstallConfig(home string) (commentbus.Paths, systemdServiceConfig, error) {
	paths, err := resolveCLIPaths(home)
	if err != nil {
		return commentbus.Paths{}, systemdServiceConfig{}, err
	}
	unitDir, err := userSystemdUnitDir()
	if err != nil {
		return commentbus.Paths{}, systemdServiceConfig{}, err
	}
	label := launchdLabelForHome(paths.Home)
	unitName := label + ".service"
	cfg := systemdServiceConfig{
		Label:    label,
		UnitName: unitName,
		Home:     paths.Home,
		UnitPath: filepath.Join(unitDir, unitName),
	}
	return paths, cfg, nil
}

func newSystemdServiceConfig(home string, botletsHome string, bin string) (commentbus.Paths, systemdServiceConfig, error) {
	paths, err := resolveCLIPaths(home)
	if err != nil {
		return commentbus.Paths{}, systemdServiceConfig{}, err
	}
	if botletsHome == "" {
		if config, ok, err := commentbus.ReadBusConfig(paths); err == nil && ok && config.BotletsHome != "" {
			botletsHome = config.BotletsHome
		}
	}
	resolvedBotletsHome, err := commentbus.ResolveBotletsHome(botletsHome)
	if err != nil {
		return commentbus.Paths{}, systemdServiceConfig{}, err
	}
	binaryPath, err := resolveLaunchdBinaryPath(bin)
	if err != nil {
		return commentbus.Paths{}, systemdServiceConfig{}, err
	}
	unitDir, err := userSystemdUnitDir()
	if err != nil {
		return commentbus.Paths{}, systemdServiceConfig{}, err
	}
	label := launchdLabelForHome(paths.Home)
	unitName := label + ".service"
	cfg := systemdServiceConfig{
		Label:            label,
		UnitName:         unitName,
		Home:             paths.Home,
		Environment:      stagingServiceEnvironment(),
		BaseURL:          commentbus.StagingServiceBaseURLOverride(),
		BotletsHome:      resolvedBotletsHome,
		BinaryPath:       binaryPath,
		TmuxBinary:       installTmuxBinaryPin(),
		BmuxBinary:       installBmuxBinaryPin(),
		UnitPath:         filepath.Join(unitDir, unitName),
		StdoutPath:       filepath.Join(paths.Logs, "commentd.out.log"),
		StderrPath:       filepath.Join(paths.Logs, "commentd.err.log"),
		ProgramArguments: []string{binaryPath, "bus", "run", "--home", paths.Home},
	}
	if err := validateSystemdServiceConfig(cfg); err != nil {
		return commentbus.Paths{}, systemdServiceConfig{}, err
	}
	return paths, cfg, nil
}

func userSystemdUnitDir() (string, error) {
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome != "" {
		if !filepath.IsAbs(configHome) {
			abs, err := filepath.Abs(configHome)
			if err != nil {
				return "", err
			}
			configHome = abs
		}
		return filepath.Join(filepath.Clean(configHome), "systemd", "user"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "systemd", "user"), nil
}

func buildSystemdUnit(cfg systemdServiceConfig) string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Comment.io local bus daemon\n")
	b.WriteString("After=network-online.target\n")
	b.WriteString("Wants=network-online.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("ExecStart=")
	for i, arg := range cfg.ProgramArguments {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(systemdQuoteArg(arg))
	}
	b.WriteByte('\n')
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("Environment=")
	b.WriteString(systemdQuoteArg("COMMENT_IO_HOME=" + cfg.Home))
	b.WriteByte('\n')
	if cfg.Environment != "" {
		b.WriteString("Environment=")
		b.WriteString(systemdQuoteArg("COMMENT_IO_ENV=" + cfg.Environment))
		b.WriteByte('\n')
	}
	if cfg.BaseURL != "" {
		b.WriteString("Environment=")
		b.WriteString(systemdQuoteArg("COMMENT_IO_BASE_URL=" + cfg.BaseURL))
		b.WriteByte('\n')
	}
	if cfg.TmuxBinary != "" {
		b.WriteString("Environment=")
		b.WriteString(systemdQuoteArg("COMMENT_IO_TMUX_BIN=" + cfg.TmuxBinary))
		b.WriteByte('\n')
	}
	if cfg.BmuxBinary != "" {
		b.WriteString("Environment=")
		b.WriteString(systemdQuoteArg("COMMENT_IO_BMUX_BIN=" + cfg.BmuxBinary))
		b.WriteByte('\n')
	}
	b.WriteString("StandardOutput=append:")
	b.WriteString(systemdUnitPathValue(cfg.StdoutPath))
	b.WriteByte('\n')
	b.WriteString("StandardError=append:")
	b.WriteString(systemdUnitPathValue(cfg.StderrPath))
	b.WriteString("\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.String()
}

func validateSystemdServiceConfig(cfg systemdServiceConfig) error {
	values := map[string]string{
		"binary path": cfg.BinaryPath,
		"home":        cfg.Home,
		"environment": cfg.Environment,
		"tmux binary": cfg.TmuxBinary,
		"bmux binary": cfg.BmuxBinary,
		"stdout path": cfg.StdoutPath,
		"stderr path": cfg.StderrPath,
		"unit name":   cfg.UnitName,
		"unit path":   cfg.UnitPath,
	}
	for i, arg := range cfg.ProgramArguments {
		values[fmt.Sprintf("program argument %d", i)] = arg
	}
	for label, value := range values {
		if containsSystemdControlChar(value) {
			return fmt.Errorf("invalid systemd unit value for %s: control characters are not allowed", label)
		}
	}
	return nil
}

func containsSystemdControlChar(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func systemdQuoteArg(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '%':
			b.WriteString("%%")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func systemdUnitPathValue(value string) string {
	return strings.ReplaceAll(value, "%", "%%")
}

func systemdServiceActive(unitName string) (bool, error) {
	output, err := runSystemctlUserOutput("is-active", unitName)
	state := strings.TrimSpace(string(output))
	if err == nil {
		return state == "active", nil
	}
	if state == "inactive" || state == "failed" || state == "unknown" || isSystemdMissingServiceText(string(output)) {
		return false, nil
	}
	return false, err
}

func runSystemctlUser(args ...string) error {
	_, err := runSystemctlUserOutput(args...)
	return err
}

func runSystemctlUserOutput(args ...string) ([]byte, error) {
	fullArgs := append([]string{"--user"}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	output, err := systemctlCombinedOutput(ctx, fullArgs...)
	if err == nil {
		return output, nil
	}
	message := unsupportedPersistentServiceMessage(strings.Join(args, " "))
	if len(output) == 0 {
		return output, fmt.Errorf("systemctl %s failed: %w. %s", strings.Join(fullArgs, " "), err, message)
	}
	return output, fmt.Errorf("systemctl %s failed: %w: %s. %s", strings.Join(fullArgs, " "), err, strings.TrimSpace(string(output)), message)
}

func isSystemdMissingServiceError(err error) bool {
	if err == nil {
		return false
	}
	return isSystemdMissingServiceText(err.Error())
}

func isSystemdMissingServiceText(text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range []string{"not loaded", "not found", "could not be found", "does not exist", "no such file"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func systemdResult(paths commentbus.Paths, cfg systemdServiceConfig, extra map[string]any) map[string]any {
	result := map[string]any{
		"ok":                true,
		"service_manager":   "systemd",
		"label":             cfg.Label,
		"unit_name":         cfg.UnitName,
		"home":              paths.Home,
		"botlets_home":      cfg.BotletsHome,
		"socket_path":       paths.Socket,
		"history_path":      paths.History,
		"unit_path":         cfg.UnitPath,
		"program_arguments": cfg.ProgramArguments,
	}
	for key, value := range extra {
		result[key] = value
	}
	return result
}

func unsupportedPersistentServiceError(action string) error {
	return errors.New(unsupportedPersistentServiceMessage(action))
}

func unsupportedPersistentServiceMessage(action string) string {
	return fmt.Sprintf("comment bus %s supports macOS launchd and Linux systemd --user; run `comment bus run` under your user service manager as a foreground fallback", action)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// socketHolderPIDsFn returns PIDs of processes holding a unix-socket file
// open. Indirected as a var so tests can fake out process discovery.
var socketHolderPIDsFn = func(socketPath string) []int {
	out, err := exec.Command("lsof", "-t", "--", socketPath).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(field); err == nil && pid > 0 {
			pids = append(pids, pid)
		}
	}
	return pids
}

// killProcessFn signals a process. Indirected for tests.
var killProcessFn = func(pid int, sig os.Signal) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(sig)
}

// socketReachableFn reports whether a process is currently accepting on the
// unix socket at socketPath. Indirected for tests.
var socketReachableFn = func(socketPath string) bool {
	if _, err := os.Stat(socketPath); err != nil {
		return false
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// launchdBootstrapRetryDelay returns the delay before the next bootstrap
// retry. Indirected for tests so they don't sleep.
var launchdBootstrapRetryDelay = func(attempt int) time.Duration {
	return time.Duration(500*attempt) * time.Millisecond
}

const launchdBootstrapMaxAttempts = 3

// launchdBootstrapWithRetry runs `launchctl bootstrap` and retries on
// transient I/O errors that launchd occasionally returns right after a
// preceding bootout — the previous load hasn't fully torn down before the
// next bootstrap tries to register the same label, surfacing as
// `Bootstrap failed: 5: Input/output error`.
func launchdBootstrapWithRetry(domain string, plistPath string) error {
	var lastErr error
	for attempt := 1; attempt <= launchdBootstrapMaxAttempts; attempt++ {
		_, err := runLaunchctlOutput("bootstrap", domain, plistPath)
		if err == nil {
			return nil
		}
		if !isTransientLaunchctlError(err) {
			return err
		}
		lastErr = err
		if attempt < launchdBootstrapMaxAttempts {
			time.Sleep(launchdBootstrapRetryDelay(attempt))
		}
	}
	return lastErr
}

func isTransientLaunchctlError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Input/output error") ||
		strings.Contains(msg, "Bootstrap failed: 5")
}

// reclaimDaemonSocket releases the unix socket at socketPath so a freshly
// bootstrapped daemon can bind cleanly. Callers must boot out the daemon we
// manage first; anything still bound to the socket is therefore a legacy
// daemon (typically the 0.1.0 node CLI that ran outside launchd/systemd) and
// would otherwise wedge the new bind with "daemon socket already exists".
func reclaimDaemonSocket(socketPath string) error {
	if socketPath == "" {
		return nil
	}
	self := os.Getpid()
	initial := socketHolderPIDsFn(socketPath)
	for _, pid := range initial {
		if pid <= 0 || pid == self {
			continue
		}
		_ = killProcessFn(pid, syscall.SIGTERM)
	}
	if len(initial) > 0 {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if !socketReachableFn(socketPath) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if socketReachableFn(socketPath) {
			for _, pid := range socketHolderPIDsFn(socketPath) {
				if pid <= 0 || pid == self {
					continue
				}
				_ = killProcessFn(pid, syscall.SIGKILL)
			}
		}
	}
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
