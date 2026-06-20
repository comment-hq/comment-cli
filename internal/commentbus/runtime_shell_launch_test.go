//go:build darwin || linux

package commentbus

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsShellSafeRuntimeName(t *testing.T) {
	for _, ok := range []string{"codex", "claude", "claude-code", "a_b1"} {
		if !isShellSafeRuntimeName(ok) {
			t.Errorf("isShellSafeRuntimeName(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "co dex", "codex;rm", "$(x)", "co`x`", "-codex", "co|x", "co\nx"} {
		if isShellSafeRuntimeName(bad) {
			t.Errorf("isShellSafeRuntimeName(%q) = true, want false", bad)
		}
	}
}

func TestClassifyShellFamily(t *testing.T) {
	cases := map[string]shellFamily{
		"/bin/zsh":               shellFamilyZsh,
		"/opt/homebrew/bin/bash": shellFamilyBash,
		"/usr/bin/fish":          shellFamilyOther,
		"/bin/sh":                shellFamilyOther,
	}
	for path, want := range cases {
		if got := classifyShellFamily(path); got != want {
			t.Errorf("classifyShellFamily(%q) = %d, want %d", path, got, want)
		}
	}
}

func TestShellExportPrefixQuotesAndRejectsControlChars(t *testing.T) {
	prefix, err := shellExportPrefix([]string{
		"COMMENT_IO_ENV=staging",
		"COMMENT_IO_BASE_URL=https://x/$(touch /tmp/pwned)",
		"BAD KEY=ignored",
		"COMMENT_IO_QUOTE=a'b",
	})
	if err != nil {
		t.Fatalf("shellExportPrefix = %v, want nil", err)
	}
	if !strings.Contains(prefix, "export COMMENT_IO_ENV='staging'; ") {
		t.Errorf("missing exported env selector: %q", prefix)
	}
	// The command-substitution payload must be single-quoted, not executable.
	if !strings.Contains(prefix, `export COMMENT_IO_BASE_URL='https://x/$(touch /tmp/pwned)'; `) {
		t.Errorf("base url not safely single-quoted: %q", prefix)
	}
	if strings.Contains(prefix, "BAD KEY") {
		t.Errorf("malformed key was not skipped: %q", prefix)
	}
	// Embedded single quote must be escaped as '\'' .
	if !strings.Contains(prefix, `export COMMENT_IO_QUOTE='a'\''b'; `) {
		t.Errorf("embedded quote not POSIX-escaped: %q", prefix)
	}

	if _, err := shellExportPrefix([]string{"COMMENT_IO_ENV=line1\nline2"}); err == nil {
		t.Error("shellExportPrefix accepted a newline value, want rejection")
	}
}

func TestBuildShellLaunchArgvStructure(t *testing.T) {
	argv := buildShellLaunchArgv("/bin/zsh", shellFamilyZsh, "codex", "export COMMENT_IO_ENV='x'; ", []string{"--agent", "rev"})
	want := []string{"/bin/zsh", "-ilc", `export COMMENT_IO_ENV='x'; codex "$@"`, "codex", "--agent", "rev"}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("zsh argv = %#v, want %#v", argv, want)
	}
	bashArgv := buildShellLaunchArgv("/bin/bash", shellFamilyBash, "codex", "", nil)
	if !strings.Contains(bashArgv[2], "shopt -s expand_aliases;") || !strings.Contains(bashArgv[2], `codex "$@"`) {
		t.Fatalf("bash script missing expand_aliases or command word: %q", bashArgv[2])
	}
}

func TestAppendShellLaunchEnvSetsPS1ForBashOnly(t *testing.T) {
	if got := appendShellLaunchEnv([]string{"HOME=/x"}, shellFamilyZsh); len(got) != 1 {
		t.Errorf("zsh env mutated: %v", got)
	}
	got := appendShellLaunchEnv([]string{"HOME=/x"}, shellFamilyBash)
	if !containsEnv(got, "PS1=x") {
		t.Errorf("bash env missing PS1: %v", got)
	}
	// Idempotent when PS1 already present.
	pre := []string{"PS1=custom", "HOME=/x"}
	if got := appendShellLaunchEnv(pre, shellFamilyBash); len(got) != 2 {
		t.Errorf("PS1 duplicated: %v", got)
	}
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// --- Real-shell integration: prove name/alias/function resolution + env export ---

type shellLaunchHarness struct {
	shellPath string
	family    shellFamily
	home      string
	binDir    string
	markerOut string
}

// writeMarker installs an executable marker script (named `name`) that records
// how it was invoked and the runtime env it saw, into MARKER_OUT.
func (h *shellLaunchHarness) writeMarker(t *testing.T, name string) {
	t.Helper()
	// One field per line so values that contain spaces (e.g. expanded alias
	// args) survive parsing.
	script := "#!/bin/sh\n" +
		"{\n" +
		`printf 'first=%s\n' "${1:-}"` + "\n" +
		`printf 'all=%s\n' "$*"` + "\n" +
		`printf 'env=%s\n' "${COMMENT_IO_ENV:-}"` + "\n" +
		`printf 'term=%s\n' "${TERM:-}"` + "\n" +
		`printf 'colorterm=%s\n' "${COLORTERM:-}"` + "\n" +
		`printf 'force=%s\n' "${FORCE_COLOR:-}"` + "\n" +
		`printf 'clicolor=%s\n' "${CLICOLOR_FORCE:-}"` + "\n" +
		`printf 'nocolor=%s\n' "${NO_COLOR:-}"` + "\n" +
		"} > \"$MARKER_OUT\"\n"
	p := filepath.Join(h.binDir, name)
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeRC writes the given body into the shell's rc/profile files so it is
// sourced for an interactive login shell of either family.
func (h *shellLaunchHarness) writeRC(t *testing.T, body string) {
	t.Helper()
	header := "export PATH=" + shellQuote(h.binDir) + ":$PATH\n"
	content := header + body + "\n"
	// zsh: .zshrc (interactive). bash: .bashrc (sourced by our launch script);
	// .bash_profile sources .bashrc to mirror a standard setup as well.
	for _, f := range []string{".zshrc", ".bashrc", ".zprofile", ".profile"} {
		if err := os.WriteFile(filepath.Join(h.home, f), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bp := "[ -r \"$HOME/.bashrc\" ] && . \"$HOME/.bashrc\"\n"
	if err := os.WriteFile(filepath.Join(h.home, ".bash_profile"), []byte(bp), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *shellLaunchHarness) run(t *testing.T, exportEnv []string, runtimeArgs []string) map[string]string {
	t.Helper()
	prefix, err := shellExportPrefix(exportEnv)
	if err != nil {
		t.Fatalf("shellExportPrefix: %v", err)
	}
	if containsRuntimeTerminalColorEnv(exportEnv) {
		prefix = "unset NO_COLOR; " + prefix
	}
	argv := buildShellLaunchArgv(h.shellPath, h.family, "codex", prefix, runtimeArgs)
	cmd := exec.Command(argv[0], argv[1:]...)
	env := []string{
		"HOME=" + h.home,
		"PATH=/usr/bin:/bin",
		"MARKER_OUT=" + h.markerOut,
		// Simulate an inherited deployment selector that rc will try to drop.
		"COMMENT_IO_ENV=inherited",
	}
	cmd.Env = appendShellLaunchEnv(env, h.family)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shell launch failed: %v\noutput: %s", err, out)
	}
	data, err := os.ReadFile(h.markerOut)
	if err != nil {
		t.Fatalf("marker not written (runtime never executed): %v", err)
	}
	result := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			result[k] = v
		}
	}
	return result
}

func containsRuntimeTerminalColorEnv(env []string) bool {
	for _, entry := range env {
		if isRuntimeTerminalColorKey(envKey(entry)) {
			return true
		}
	}
	return false
}

func newShellLaunchHarness(t *testing.T, shellPath string) *shellLaunchHarness {
	t.Helper()
	home := t.TempDir()
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return &shellLaunchHarness{
		shellPath: shellPath,
		family:    classifyShellFamily(shellPath),
		home:      home,
		binDir:    binDir,
		markerOut: filepath.Join(home, "marker.out"),
	}
}

func eachAvailableShell(t *testing.T, fn func(t *testing.T, shellPath string)) {
	t.Helper()
	for _, name := range []string{"zsh", "bash"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Logf("shell %q not available; skipping", name)
			continue
		}
		t.Run(name, func(t *testing.T) { fn(t, path) })
	}
}

func TestShellLaunchResolvesBinaryOnPath(t *testing.T) {
	eachAvailableShell(t, func(t *testing.T, shellPath string) {
		h := newShellLaunchHarness(t, shellPath)
		h.writeMarker(t, "codex") // a real `codex` binary on the rc-prepended PATH
		h.writeRC(t, "")
		got := h.run(t, []string{"COMMENT_IO_ENV=fromprefix"}, []string{"--flag"})
		if got["all"] != "--flag" {
			t.Errorf("binary args = %q, want --flag", got["all"])
		}
		if got["env"] != "fromprefix" {
			t.Errorf("binary COMMENT_IO_ENV = %q, want fromprefix", got["env"])
		}
	})
}

func TestShellLaunchResolvesAlias(t *testing.T) {
	eachAvailableShell(t, func(t *testing.T, shellPath string) {
		h := newShellLaunchHarness(t, shellPath)
		h.writeMarker(t, "realcodex")
		h.writeRC(t, "alias codex='realcodex aliased'")
		got := h.run(t, []string{"COMMENT_IO_ENV=fromprefix"}, []string{"--flag"})
		if got["first"] != "aliased" {
			t.Errorf("alias did not expand: marker=%v", got)
		}
		if !strings.Contains(got["all"], "--flag") {
			t.Errorf("alias args not forwarded: %q", got["all"])
		}
	})
}

// TestShellLaunchResolvesFunctionAndExportsEnv is the key §4.7 proof: a
// function-wrapped runtime must still see the exported COMMENT_IO_ENV even
// though (a) the function consumes the command, and (b) rc tried to drop the
// inherited value. `export` (not a `VAR=val cmd` prefix) is what makes this hold.
func TestShellLaunchResolvesFunctionAndExportsEnv(t *testing.T) {
	eachAvailableShell(t, func(t *testing.T, shellPath string) {
		h := newShellLaunchHarness(t, shellPath)
		h.writeMarker(t, "realcodex")
		// rc both defines the function AND (simulating `eval "$(mise env)"`)
		// unsets the inherited deployment selector. The export prefix runs after
		// rc and must win.
		h.writeRC(t, "unset COMMENT_IO_ENV\ncodex() { realcodex func \"$@\"; }")
		got := h.run(t, []string{"COMMENT_IO_ENV=fromprefix"}, []string{"--agent", "rev"})
		if got["first"] != "func" {
			t.Errorf("function did not run: marker=%v", got)
		}
		if got["env"] != "fromprefix" {
			t.Errorf("exported COMMENT_IO_ENV not seen by function-launched runtime = %q, want fromprefix", got["env"])
		}
	})
}

func TestShellLaunchReassertsTerminalColorEnvAfterRC(t *testing.T) {
	eachAvailableShell(t, func(t *testing.T, shellPath string) {
		h := newShellLaunchHarness(t, shellPath)
		h.writeMarker(t, "realcodex")
		h.writeRC(t, strings.Join([]string{
			"TERM=dumb",
			"COLORTERM=",
			"FORCE_COLOR=0",
			"CLICOLOR_FORCE=0",
			"NO_COLOR=1",
			"export TERM COLORTERM FORCE_COLOR CLICOLOR_FORCE NO_COLOR",
			"codex() { realcodex color \"$@\"; }",
		}, "\n"))
		prefixEnv := runtimeTerminalColorEnv([]string{
			"TERM=xterm-kitty",
			"COLORTERM=24bit",
			"FORCE_COLOR=0",
			"CLICOLOR_FORCE=0",
			"NO_COLOR=1",
		})
		got := h.run(t, prefixEnv, []string{"--agent", "rev"})
		if got["first"] != "color" {
			t.Errorf("function did not run: marker=%v", got)
		}
		for key, want := range map[string]string{
			"term":      "xterm-kitty",
			"colorterm": "24bit",
			"force":     "1",
			"clicolor":  "1",
			"nocolor":   "",
		} {
			if got[key] != want {
				t.Errorf("%s = %q, want %q (marker=%v)", key, got[key], want, got)
			}
		}
	})
}

// TestRunSessionExecShellModeLaunchesViaLoginShell is the core managed-path
// regression: a new managed session is shell mode, so RunSessionExec must launch
// the runtime *name* through the login shell (no client-side path resolution),
// re-assert injected COMMENT_IO_* via an export prefix, and still scrub secrets
// from the process env.
func TestRunSessionExecShellModeLaunchesViaLoginShell(t *testing.T) {
	t.Setenv("SHELL", "/bin/sh") // deterministic, always present
	paths := testDaemonPaths(t)
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		SessionName: "comment-reviewer-shell1",
		PaneTarget:  "comment-reviewer-shell1:0.0",
		Runtime:     "codex",
		State:       "starting",
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.RuntimeLaunchMode != RuntimeLaunchModeShell {
		t.Fatalf("new managed session launch mode = %q, want shell", record.RuntimeLaunchMode)
	}
	if record.RuntimePath != "" || record.RuntimeCommandPath != "" {
		t.Fatalf("shell-mode session pinned a path: %q/%q", record.RuntimePath, record.RuntimeCommandPath)
	}

	var execPath string
	var execArgv, execEnv []string
	sentinel := errors.New("exec sentinel")
	err = RunSessionExec(SessionExecOptions{
		Paths:      paths,
		SessionID:  record.SessionID,
		Generation: record.Generation,
		Environ:    []string{"COMMENT_IO_ENV=staging", "PATH=/usr/bin:/bin", "OPENAI_API_KEY=sk-test-secret", "AXIOM_TOKEN=secret"},
		LookPath:   func(string) (string, error) { return "", errors.New("lookpath must not be called in shell mode") },
		Exec: func(path string, argv []string, env []string) error {
			execPath = path
			execArgv = append([]string{}, argv...)
			execEnv = append([]string{}, env...)
			return sentinel
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunSessionExec error = %v", err)
	}
	if execPath != "/bin/sh" {
		t.Fatalf("exec path = %q, want /bin/sh (the login shell)", execPath)
	}
	if len(execArgv) < 4 || execArgv[0] != "/bin/sh" || execArgv[1] != "-ilc" || execArgv[3] != "codex" {
		t.Fatalf("argv = %#v, want [/bin/sh -ilc <script> codex ...]", execArgv)
	}
	script := execArgv[2]
	if !strings.Contains(script, `codex "$@"`) {
		t.Fatalf("script missing literal command word: %q", script)
	}
	// Injected session env re-asserted via the in-script export prefix (§4.7).
	if !strings.Contains(script, "export COMMENT_IO_SESSION_ID=") || !strings.Contains(script, "export COMMENT_IO_PROFILE=") {
		t.Fatalf("script missing injected export prefix: %q", script)
	}
	for _, want := range []string{
		"unset NO_COLOR;",
		"export TERM='xterm-256color';",
		"export COLORTERM='truecolor';",
		"export FORCE_COLOR='1';",
		"export CLICOLOR_FORCE='1';",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing color export %q: %q", want, script)
		}
	}
	// Runtime args forwarded after the $0 placeholder.
	if !strings.Contains(strings.Join(execArgv[4:], " "), "--yolo") {
		t.Fatalf("runtime args not forwarded: %#v", execArgv)
	}
	env := strings.Join(execEnv, "\n")
	if !strings.Contains(env, "COMMENT_IO_PROFILE=max.reviewer") || !strings.Contains(env, "COMMENT_IO_BOT_NAME=reviewer") {
		t.Fatalf("process env missing injected vars:\n%s", env)
	}
	if strings.Contains(env, "OPENAI_API_KEY") || strings.Contains(env, "AXIOM_TOKEN") {
		t.Fatalf("secrets leaked into shell-mode runtime env:\n%s", env)
	}
	for _, want := range []string{"TERM=xterm-256color", "COLORTERM=truecolor", "FORCE_COLOR=1", "CLICOLOR_FORCE=1"} {
		if !strings.Contains(env, want) {
			t.Fatalf("process env missing color var %q:\n%s", want, env)
		}
	}
	if strings.Contains(env, "NO_COLOR=") {
		t.Fatalf("process env kept NO_COLOR:\n%s", env)
	}
}

func TestRunSessionExecShellModeRuntimeEmitsAnsiColor(t *testing.T) {
	eachAvailableShell(t, func(t *testing.T, shellPath string) {
		t.Setenv("SHELL", shellPath)
		h := newShellLaunchHarness(t, shellPath)
		runtimePath := filepath.Join(h.binDir, "codex")
		script := "#!/bin/sh\n" +
			"printf '\\033[31mmanaged-color\\033[0m\\n'\n" +
			`printf 'term=%s colorterm=%s force=%s clicolor=%s nocolor=%s\n' "${TERM:-}" "${COLORTERM:-}" "${FORCE_COLOR:-}" "${CLICOLOR_FORCE:-}" "${NO_COLOR:-}"` + "\n"
		if err := os.WriteFile(runtimePath, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		h.writeRC(t, strings.Join([]string{
			"TERM=dumb",
			"COLORTERM=",
			"FORCE_COLOR=0",
			"CLICOLOR_FORCE=0",
			"NO_COLOR=1",
			"export TERM COLORTERM FORCE_COLOR CLICOLOR_FORCE NO_COLOR",
		}, "\n"))

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
			Runtime:     "codex",
			State:       "starting",
		})
		if err != nil {
			t.Fatal(err)
		}

		var output []byte
		sentinel := errors.New("exec sentinel")
		err = RunSessionExec(SessionExecOptions{
			Paths:      paths,
			SessionID:  record.SessionID,
			Generation: record.Generation,
			Environ: []string{
				"HOME=" + h.home,
				"PATH=/usr/bin:/bin",
				"TERM=dumb",
				"COLORTERM=bad\nvalue",
				"FORCE_COLOR=0",
				"CLICOLOR_FORCE=0",
				"NO_COLOR=1",
			},
			LookPath: func(string) (string, error) { return "", errors.New("lookpath must not be called in shell mode") },
			Exec: func(path string, argv []string, env []string) error {
				cmd := exec.Command(path, argv[1:]...)
				cmd.Env = env
				var runErr error
				output, runErr = cmd.CombinedOutput()
				if runErr != nil {
					t.Fatalf("managed shell command failed: %v\noutput: %s\nargv: %#v", runErr, output, argv)
				}
				return sentinel
			},
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("RunSessionExec error = %v", err)
		}
		got := string(output)
		if !strings.Contains(got, "\x1b[31mmanaged-color\x1b[0m") {
			t.Fatalf("managed runtime output missing exact ANSI bytes:\n%q", got)
		}
		for _, want := range []string{"term=xterm-256color", "colorterm=truecolor", "force=1", "clicolor=1", "nocolor="} {
			if !strings.Contains(got, want) {
				t.Fatalf("managed runtime output missing %q:\n%s", want, got)
			}
		}
	})
}

// TestSessionPaneRunsExpectedRuntimeShellMode locks the shell-mode health
// verdict: a non-shell pane foreground means the runtime is running; a bare
// login shell means "not yet / busy"; empty is never running.
func TestSessionPaneRunsExpectedRuntimeShellMode(t *testing.T) {
	record := SessionRecord{
		Runtime:           "claude",
		RuntimeCommand:    []string{"claude", "--agent", "reviewer"},
		RuntimeLaunchMode: RuntimeLaunchModeShell,
		BotName:           "reviewer",
		State:             "alive",
	}
	for _, running := range []string{"claude", "codex", "node", "python", "mycodex", "sleep", "vim"} {
		if !sessionPaneRunsExpectedRuntime(record, running) {
			t.Errorf("shell-mode pane %q = not-running, want running", running)
		}
	}
	for _, notReady := range []string{"zsh", "bash", "sh", "dash", "-zsh", "-bash", "ZSH", "fish", "ksh", "csh", "tcsh"} {
		if sessionPaneRunsExpectedRuntime(record, notReady) {
			t.Errorf("shell-mode pane %q = running, want not-ready (login shell)", notReady)
		}
	}
	if sessionPaneRunsExpectedRuntime(record, "") {
		t.Error("shell-mode empty pane command = running, want false")
	}

	// Path-mode parity: precise expected-names match still applies.
	pathRecord := SessionRecord{
		Runtime:            "claude",
		RuntimeCommand:     []string{"claude", "--agent", "reviewer"},
		RuntimeLaunchMode:  RuntimeLaunchModePath,
		RuntimePath:        "/usr/local/bin/claude",
		RuntimeCommandPath: "/usr/local/bin/claude",
	}
	if !sessionPaneRunsExpectedRuntime(pathRecord, "claude") {
		t.Error("path-mode pane claude = not-running, want running")
	}
	if sessionPaneRunsExpectedRuntime(pathRecord, "sleep") {
		t.Error("path-mode pane sleep = running, want busy (precise names)")
	}
}

// TestSessionRuntimeResolvableShellMode locks the trust gate the daemon health
// checks use: a shell-mode session with no pinned paths must pass (resolution
// happens via the login shell at exec); a bad runtime name must fail.
func TestSessionRuntimeResolvableShellMode(t *testing.T) {
	ok := SessionRecord{
		Runtime:           "codex",
		RuntimeCommand:    []string{"codex"},
		RuntimeLaunchMode: RuntimeLaunchModeShell,
	}
	if err := sessionRuntimeResolvable(ok); err != nil {
		t.Errorf("shell-mode resolvable = %v, want nil (no path to re-validate)", err)
	}
	bad := SessionRecord{
		Runtime:           "codex",
		RuntimeCommand:    []string{"not-a-runtime"},
		RuntimeLaunchMode: RuntimeLaunchModeShell,
	}
	if err := sessionRuntimeResolvable(bad); err == nil {
		t.Error("shell-mode with non-managed runtime name = nil, want error")
	}
	empty := SessionRecord{RuntimeLaunchMode: RuntimeLaunchModeShell}
	if err := sessionRuntimeResolvable(empty); err == nil {
		t.Error("shell-mode with empty command = nil, want error")
	}
}
