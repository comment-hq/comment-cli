package main

import (
	"strings"
	"testing"
)

func TestRuntimeAuthHeaderValue(t *testing.T) {
	// Login state only — deliberately independent of install/PATH (the daemon's
	// restricted PATH can't see user-local installs). Isolate from the
	// developer's real config by pointing HOME + CODEX_HOME at an empty temp dir,
	// then drive auth via env keys.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CODEX_HOME", tmp)

	cases := []struct {
		name      string
		anthropic string
		openai    string
		want      string
	}{
		{"none logged in", "", "", ""},
		{"claude only", "sk-ant", "", "claude"},
		{"codex only", "", "sk-openai", "codex"},
		{"both, canonical order", "sk-ant", "sk-openai", "claude,codex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_API_KEY", tc.anthropic)
			t.Setenv("OPENAI_API_KEY", tc.openai)
			if got := runtimeAuthHeaderValue(); got != tc.want {
				t.Fatalf("runtimeAuthHeaderValue() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReadinessFocusRuntime(t *testing.T) {
	// `comment run --runtime codex` while only claude is ready: the panel must
	// stay scoped to codex (pending), never claim readiness about claude.
	pending := readinessState{paired: true, daemonRunning: true, authedRuntimes: []string{"claude"}, claudeInstalled: true, codexInstalled: true, focusRuntime: "codex"}
	if pending.loggedIn() {
		t.Fatal("focus=codex with only claude authed must be not-logged-in")
	}
	hints := loginHints(pending)
	if len(hints) != 1 || !strings.Contains(hints[0], "codex login") {
		t.Fatalf("focus=codex hints = %v, want only the codex hint", hints)
	}
	var b strings.Builder
	renderReadinessBox(&b, pending, false)
	if out := b.String(); strings.Contains(out, "You're ready") || strings.Contains(out, "Coding agent ready") {
		t.Fatalf("focus-pending panel must not claim readiness:\n%s", out)
	}

	// focus=codex AND codex authed → logged in; summary names Codex (not claude).
	ready := readinessState{paired: true, authedRuntimes: []string{"claude", "codex"}, focusRuntime: "codex"}
	if !ready.loggedIn() {
		t.Fatal("focus=codex with codex authed must be logged in")
	}
	if got := loggedInSummary(ready); got != "Codex" {
		t.Fatalf("focus summary = %q, want Codex", got)
	}
}

func TestRenderReadinessBoxPending(t *testing.T) {
	var b strings.Builder
	renderReadinessBox(&b, readinessState{paired: true, daemonRunning: true, pairedLabel: "Test Box"}, false)
	out := b.String()
	for _, want := range []string{
		"SETUP READINESS",
		"this computer paired",
		"Log in to a coding agent",
		"LOG IN NOW",
		"codex login",
		"Not ready yet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pending panel missing %q.\n%s", want, out)
		}
	}
	if strings.Contains(out, "You're ready") {
		t.Errorf("pending panel must not claim readiness.\n%s", out)
	}
}

func TestRenderReadinessBoxReady(t *testing.T) {
	var b strings.Builder
	renderReadinessBox(&b, readinessState{paired: true, daemonRunning: true, pairedLabel: "Test Box", authedRuntimes: []string{"claude"}, claudeInstalled: true, setupBaseURL: "https://example.comt.dev/"}, false)
	out := b.String()
	for _, want := range []string{
		"Coding agent ready",
		"Claude logged in",
		"You're ready",
		// The ready CTA points at the PAIRED deployment, not hard-coded prod.
		"https://example.comt.dev/setup/handle",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ready panel missing %q.\n%s", want, out)
		}
	}
	if strings.Contains(out, "LOG IN NOW") {
		t.Errorf("ready panel must not show the glow badge.\n%s", out)
	}

	// Unknown base URL falls back to production.
	var b2 strings.Builder
	renderReadinessBox(&b2, readinessState{paired: true, daemonRunning: true, authedRuntimes: []string{"codex"}}, false)
	if !strings.Contains(b2.String(), "https://comment.io/setup/handle") {
		t.Errorf("ready panel without a base URL must fall back to comment.io.\n%s", b2.String())
	}
}

func TestRenderReadinessBoxDaemonNotRunning(t *testing.T) {
	// Paired + logged in but the daemon is stopped: must NOT read as ready.
	var b strings.Builder
	renderReadinessBox(&b, readinessState{paired: true, daemonRunning: false, pairedLabel: "Test Box", authedRuntimes: []string{"claude"}, claudeInstalled: true}, false)
	out := b.String()
	if !strings.Contains(out, "daemon isn't running") {
		t.Errorf("paired-but-stopped panel must flag the daemon.\n%s", out)
	}
	if strings.Contains(out, "You're ready") {
		t.Errorf("paired-but-stopped panel must not claim readiness.\n%s", out)
	}
}

func TestRenderReadinessBoxShowsDockerPersistence(t *testing.T) {
	var b strings.Builder
	renderReadinessBox(&b, readinessState{
		paired:          true,
		daemonRunning:   true,
		authedRuntimes:  []string{"claude"},
		claudeInstalled: true,
		dockerAgent: &dockerAgentReadiness{
			State: dockerAgentMountReadiness{
				Path:       "/state",
				Mounted:    true,
				Persistent: true,
			},
			Home: dockerAgentMountReadiness{
				Path:       "/home/agent",
				Mounted:    true,
				Persistent: true,
			},
			Runtimes: []dockerAgentRuntimeReadiness{
				{Name: "claude", Installed: true, Authenticated: true},
				{Name: "codex", Installed: true, Authenticated: false},
			},
		},
	}, false)
	out := b.String()
	for _, want := range []string{
		"Docker storage",
		"State has a restart-safe mount at /state",
		"Agent home has a restart-safe mount at /home/agent",
		"Claude installed & logged in",
		"Codex installed, not logged in",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Docker readiness panel missing %q.\n%s", want, out)
		}
	}
}

func TestReadinessRequiresDockerPersistence(t *testing.T) {
	st := readinessState{
		paired:         true,
		daemonRunning:  true,
		authedRuntimes: []string{"claude"},
		dockerAgent: &dockerAgentReadiness{
			State:    dockerAgentMountReadiness{Path: "/state", Mounted: false, Persistent: false},
			Home:     dockerAgentMountReadiness{Path: "/home/agent", Mounted: true, Persistent: true},
			Runtimes: []dockerAgentRuntimeReadiness{{Name: "claude", Installed: true, Authenticated: true}},
		},
	}
	if st.ready() {
		t.Fatal("Docker sandbox readiness must require persistent /state and home mounts")
	}
	var b strings.Builder
	renderReadinessBox(&b, st, false)
	out := b.String()
	if strings.Contains(out, "You're ready") {
		t.Fatalf("missing Docker persistence must not claim ready:\n%s", out)
	}
	if !strings.Contains(out, "Docker storage mounts are not ready") {
		t.Fatalf("missing Docker persistence should explain the blocker:\n%s", out)
	}
}

func TestDockerReadinessClaudeHintUsesPersistentLogin(t *testing.T) {
	hints := loginHints(readinessState{
		claudeInstalled: true,
		dockerAgent: &dockerAgentReadiness{
			State: dockerAgentMountReadiness{Path: "/state", Mounted: true, Persistent: true},
			Home:  dockerAgentMountReadiness{Path: "/home/agent", Mounted: true, Persistent: true},
		},
	})
	if len(hints) == 0 || !strings.Contains(hints[0], "/login") {
		t.Fatalf("Docker Claude hint = %v, want /login", hints)
	}
	if strings.Contains(hints[0], "setup-token") {
		t.Fatalf("Docker Claude hint must not mention setup-token: %v", hints)
	}
}

func TestLoggedInSummary(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"claude"}, "Claude"},
		{[]string{"codex"}, "Codex"},
		{[]string{"claude", "codex"}, "Claude & Codex"},
	}
	for _, tc := range cases {
		if got := loggedInSummary(readinessState{authedRuntimes: tc.in}); got != tc.want {
			t.Errorf("loggedInSummary(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestColorEnabledRespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	// os.Stdout is typically not a TTY under `go test`, but NO_COLOR must force
	// it off regardless.
	if colorEnabled(nil) {
		t.Errorf("colorEnabled must be false when NO_COLOR is set")
	}
}
