package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// `comment status` renders a human-facing setup-readiness panel: a big, hard-to-
// miss checklist whose load-bearing step — logging in to a coding agent (Claude
// or Codex) — glows while it is still pending. It deliberately complements
// `comment doctor` (machine-readable JSON) by being the friendly terminal view,
// reusing the same underlying checks (daemon pairing + runtime auth) so the two
// never drift.

// readinessState is the resolved setup state the panel renders.
type readinessState struct {
	paired          bool
	pairedLabel     string
	daemonRunning   bool // the bus daemon answers on its socket (it actually services work)
	authedRuntimes  []string // subset of {claude, codex}: installed AND logged in, sorted
	claudeInstalled bool
	codexInstalled  bool
	// setupBaseURL is the deployment this computer is paired to (from
	// daemon-auth.json). The ready CTA points here so a staging/personal install
	// doesn't send users to production /setup (where the daemon they just paired
	// wouldn't receive the agents they create). Empty when unpaired/unknown.
	setupBaseURL string
	// focusRuntime scopes the login step to a single runtime (set by
	// `comment run --runtime X`): the step is "done" only when THAT runtime is
	// ready, so the panel never says "ready" about a different runtime than the
	// one about to launch. Empty = the global "any coding agent" view.
	focusRuntime string
}

func (s readinessState) loggedIn() bool {
	if s.focusRuntime != "" {
		for _, r := range s.authedRuntimes {
			if r == s.focusRuntime {
				return true
			}
		}
		return false
	}
	return len(s.authedRuntimes) > 0
}

// ready requires the daemon to actually be RUNNING, not just paired: a
// paired-but-stopped daemon (stale auth file, or `bus install` that failed and
// fell through) would otherwise read as ready while no worker installs agents or
// services work.
func (s readinessState) ready() bool { return s.paired && s.daemonRunning && s.loggedIn() }

// gatherReadiness collects pairing + runtime-login state. Both signals reuse the
// exact helpers `comment doctor` uses (LoadDaemonAuth + runtimeAuthState), so
// the panel and doctor always agree.
func gatherReadiness(paths commentbus.Paths) readinessState {
	st := readinessState{}
	if auth, paired, err := commentbus.LoadDaemonAuth(paths); err == nil && paired {
		st.paired = true
		st.pairedLabel = auth.Label
		st.setupBaseURL = auth.BaseURL
		// "Paired" alone isn't enough — verify the daemon actually answers, the
		// same liveness check `comment doctor` uses (checkDaemonRunning).
		st.daemonRunning = daemonHealthy(paths)
	}
	if _, err := runtimeLookPath("claude"); err == nil {
		st.claudeInstalled = true
	}
	if _, err := runtimeLookPath("codex"); err == nil {
		st.codexInstalled = true
	}
	// runtimeAuthHeaderValue is login-only (the daemon can't reliably check PATH).
	// `comment status` runs in the FOREGROUND with the user's interactive PATH, so
	// here we CAN additionally require the runtime to be installed — only count a
	// runtime that can actually launch (installed AND logged in), so status never
	// reads "ready" for a stale config whose binary `comment run` can't find. (The
	// daemon's report to the web stays login-only by necessity.)
	for _, runtime := range strings.Split(runtimeAuthHeaderValue(), ",") {
		if runtime == "" {
			continue
		}
		if (runtime == "claude" && !st.claudeInstalled) || (runtime == "codex" && !st.codexInstalled) {
			continue
		}
		st.authedRuntimes = append(st.authedRuntimes, runtime)
	}
	return st
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("comment status", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	noColor := fs.Bool("no-color", false, "Disable ANSI color/formatting")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("status does not accept positional arguments")
	}
	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	st := gatherReadiness(paths)
	renderReadinessBox(os.Stdout, st, colorEnabled(os.Stdout) && !*noColor)
	return nil
}

// ---------------------------------------------------------------------------
// ANSI helpers (self-contained; no dependency). Color is enabled only on a real
// terminal and when NO_COLOR is unset and TERM is not "dumb".
// ---------------------------------------------------------------------------

const (
	ansiReset       = "\033[0m"
	ansiBold        = "\033[1m"
	ansiDim         = "\033[2m"
	ansiReverse     = "\033[7m"
	ansiGreen       = "\033[32m"
	ansiBrightGreen = "\033[92m"
	ansiYellow      = "\033[33m"
	ansiBrightYel   = "\033[93m"
	ansiCyan        = "\033[36m"
)

// colorEnabled reports whether ANSI styling should be emitted to w. A glow in
// the terminal is only meaningful on an interactive TTY; honor the NO_COLOR
// convention and dumb terminals.
func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if term := os.Getenv("TERM"); term == "dumb" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isTerminalFile(f)
}

// isTerminalFile reports whether f is a character device (a terminal). Note it
// checks the GIVEN file's fd — callers pass os.Stdout / os.Stderr — never stdin,
// which under `curl … | bash` is the pipe, not the terminal.
func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// paint wraps s in the given ANSI codes when color is on, else returns s.
func paint(color bool, s string, codes ...string) string {
	if !color || len(codes) == 0 {
		return s
	}
	return strings.Join(codes, "") + s + ansiReset
}

// ---------------------------------------------------------------------------
// Panel rendering. Left-bar style (no right border) so embedded ANSI escapes
// never throw off column alignment.
// ---------------------------------------------------------------------------

const readinessRuleWidth = 62

func topRule(w io.Writer, title string, color bool) {
	const prefix = "  ╭─ "
	dashes := readinessRuleWidth - len([]rune(prefix+title+" "))
	if dashes < 3 {
		dashes = 3
	}
	line := paint(color, prefix, ansiCyan) +
		paint(color, title, ansiBold, ansiCyan) +
		paint(color, " "+strings.Repeat("─", dashes), ansiCyan)
	fmt.Fprintln(w, line)
}

func bottomRule(w io.Writer, color bool) {
	line := "  ╰" + strings.Repeat("─", readinessRuleWidth-3)
	fmt.Fprintln(w, paint(color, line, ansiCyan))
}

// renderReadinessBox draws the setup-readiness panel. The pending login step is
// the glow: a bright bold marker plus a reverse-video badge, so its urgency is
// conveyed by color, glyph, AND text (never color alone).
func renderReadinessBox(w io.Writer, st readinessState, color bool) {
	fmt.Fprintln(w)
	topRule(w, "COMMENT.IO · SETUP READINESS", color)
	fmt.Fprintln(w, bar(color))

	// Step 1 — install + pair + daemon running.
	if st.paired && st.daemonRunning {
		label := st.pairedLabel
		suffix := ""
		if label != "" {
			suffix = paint(color, fmt.Sprintf("  (as %q)", label), ansiDim)
		}
		fmt.Fprintf(w, "%s   %s  %s%s\n", bar(color), doneMark(color), "CLI installed & this computer paired", suffix)
	} else if st.paired {
		// Paired but the daemon isn't answering — it won't install agents or
		// service work until it's started.
		fmt.Fprintf(w, "%s   %s  %s\n", bar(color), pendingMark(color), paint(color, "Paired, but the daemon isn't running — run: comment bus start", ansiBold, ansiBrightYel))
	} else {
		fmt.Fprintf(w, "%s   %s  %s\n", bar(color), pendingMark(color), paint(color, "Pair this computer — run: comment bus pair", ansiBold, ansiBrightYel))
	}

	// Step 2 — log in to a coding agent. This is the glowing step.
	if st.loggedIn() {
		fmt.Fprintf(w, "%s   %s  %s\n", bar(color), doneMark(color), "Coding agent ready — "+loggedInSummary(st)+" logged in")
	} else {
		badge := paint(color, " ◀ LOG IN NOW ", ansiReverse, ansiBold, ansiBrightYel)
		fmt.Fprintf(w, "%s   %s  %s   %s\n",
			bar(color),
			pendingMark(color),
			paint(color, "Log in to a coding agent — Claude or Codex", ansiBold, ansiBrightYel),
			badge,
		)
		for _, hint := range loginHints(st) {
			fmt.Fprintf(w, "%s        %s %s\n", bar(color), paint(color, "↳", ansiDim), hint)
		}
	}

	fmt.Fprintln(w, bar(color))
	if st.ready() {
		setupURL := strings.TrimRight(st.setupBaseURL, "/")
		if setupURL == "" {
			setupURL = "https://comment.io"
		}
		fmt.Fprintf(w, "%s   %s\n", bar(color), paint(color, "You're ready! Create agents at "+setupURL+"/setup", ansiBold, ansiBrightGreen))
	} else if !st.loggedIn() {
		fmt.Fprintf(w, "%s   %s\n", bar(color), paint(color, "Not ready yet — finish the glowing step above.", ansiBold, ansiYellow))
	} else {
		fmt.Fprintf(w, "%s   %s\n", bar(color), paint(color, "Almost there — finish the glowing step above.", ansiBold, ansiYellow))
	}
	bottomRule(w, color)
	fmt.Fprintln(w)
}

func bar(color bool) string { return paint(color, "  │", ansiCyan) }

func doneMark(color bool) string { return paint(color, "✓", ansiBrightGreen, ansiBold) }

// pendingMark is the glow: a bright bold filled circle.
func pendingMark(color bool) string { return paint(color, "●", ansiBrightYel, ansiBold) }

func loggedInSummary(st readinessState) string {
	runtimes := st.authedRuntimes
	if st.focusRuntime != "" {
		// The login step is scoped to one runtime; name that one.
		runtimes = []string{st.focusRuntime}
	}
	names := make([]string, 0, len(runtimes))
	for _, r := range runtimes {
		switch r {
		case "claude":
			names = append(names, "Claude")
		case "codex":
			names = append(names, "Codex")
		default:
			names = append(names, r)
		}
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " & " + names[len(names)-1]
	}
}

// loginHints returns the concrete next commands. When a runtime isn't installed
// it says so, since "log in" presupposes it's present. When the state is scoped
// to a focus runtime (comment run --runtime X), only that runtime's hint shows.
func loginHints(st readinessState) []string {
	claude := "run:  claude        (or  claude setup-token)"
	if !st.claudeInstalled {
		claude = "install Claude Code, then run:  claude"
	}
	codex := "run:  codex login"
	if !st.codexInstalled {
		codex = "install Codex, then run:  codex login"
	}
	switch st.focusRuntime {
	case "claude":
		return []string{claude}
	case "codex":
		return []string{codex}
	default:
		return []string{claude, codex}
	}
}
