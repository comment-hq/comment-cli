//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

type dockerRuntimeStubCalls struct {
	combined [][]string
	run      []string
}

func resetDockerRuntimeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("COMMENT_IO_HOME", "")
	t.Setenv("COMMENT_IO_ENV", "")
	t.Setenv("COMMENT_IO_BASE_URL", "")
	t.Setenv("COMMENT_IO_STAGING_BASE_URL", "")
	t.Setenv("COMMENT_IO_SKIP_ATTACH", "")
	t.Setenv("COMMENT_IO_AGENT_SANDBOX", "")
	t.Setenv("TERM", "")
}

func stubDockerRuntime(t *testing.T, containerBase string) *dockerRuntimeStubCalls {
	t.Helper()
	calls := &dockerRuntimeStubCalls{}
	oldLookPath := dockerRuntimeLookPath
	oldCombined := dockerRuntimeCombinedOutput
	oldRun := dockerRuntimeRunCommand
	oldHost := dockerRuntimeHostDaemonUnavailable
	oldStdin := dockerRuntimeStdinIsTerminal
	oldStdout := dockerRuntimeStdoutIsTerminal
	dockerRuntimeLookPath = func(name string) (string, error) {
		if name != "docker" {
			return "", errors.New("not found")
		}
		return "/fake/docker", nil
	}
	dockerRuntimeCombinedOutput = func(_ context.Context, command string, args ...string) ([]byte, error) {
		calls.combined = append(calls.combined, append([]string{command}, args...))
		if command != "/fake/docker" {
			return nil, errors.New("unexpected command")
		}
		if len(args) >= 1 && args[0] == "inspect" {
			return []byte("true\n"), nil
		}
		if len(args) >= 4 && args[0] == "exec" && args[2] == "cat" {
			return []byte(`{"daemon_id":"ld_test","daemon_token":"ldt_test","base_url":"` + containerBase + `"}`), nil
		}
		return nil, errors.New("unexpected docker args")
	}
	dockerRuntimeRunCommand = func(_ context.Context, command string, args ...string) error {
		calls.run = append([]string{command}, args...)
		return nil
	}
	dockerRuntimeHostDaemonUnavailable = func(context.Context, commentbus.Paths) bool { return true }
	dockerRuntimeStdinIsTerminal = func() bool { return true }
	dockerRuntimeStdoutIsTerminal = func() bool { return true }
	t.Cleanup(func() {
		dockerRuntimeLookPath = oldLookPath
		dockerRuntimeCombinedOutput = oldCombined
		dockerRuntimeRunCommand = oldRun
		dockerRuntimeHostDaemonUnavailable = oldHost
		dockerRuntimeStdinIsTerminal = oldStdin
		dockerRuntimeStdoutIsTerminal = oldStdout
	})
	return calls
}

func disableDockerRuntimePrivateFileChecksForTest(t *testing.T) {
	t.Helper()
	old := dockerRuntimePrivateFileChecksOK
	dockerRuntimePrivateFileChecksOK = func() bool { return false }
	t.Cleanup(func() { dockerRuntimePrivateFileChecksOK = old })
}

func writeDockerRuntimeProjectionManifest(t *testing.T, paths commentbus.Paths, handles ...string) {
	t.Helper()
	files := make([]string, 0, len(handles))
	for _, handle := range handles {
		files = append(files, handle+".json")
	}
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := `{"files":[`
	for i, file := range files {
		if i > 0 {
			data += `,`
		}
		data += `"` + file + `"`
	}
	data += "]}\n"
	if err := os.WriteFile(filepath.Join(agentsDir, ".comment-agent-projected.manifest"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDockerRuntimeDelegatesShortcutWhenHostDaemonUnavailable(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubDockerRuntime(t, "https://comment.io")

	err = defaultRunRuntimeCommand(runtimeRunOptions{
		BotShortcut: "max.reviewer",
		Role:        commentbus.RuntimeRoleMain,
		Detach:      true,
	})
	if err != nil {
		t.Fatalf("defaultRunRuntimeCommand returned error: %v", err)
	}
	want := []string{
		"/fake/docker", "exec",
		"-e", "COMMENT_IO_AGENT_SANDBOX=1",
		"comment-agent-comment-io", "comment", "run", "--detach", "max.reviewer",
	}
	if !slices.Equal(calls.run, want) {
		t.Fatalf("docker run args = %#v, want %#v", calls.run, want)
	}
}

func TestDockerRuntimeDelegatesExplicitModelClear(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubDockerRuntime(t, "https://comment.io")

	err = defaultRunRuntimeCommand(runtimeRunOptions{
		BotShortcut: "max.reviewer",
		Role:        commentbus.RuntimeRoleMain,
		ModelSet:    true,
		Detach:      true,
	})
	if err != nil {
		t.Fatalf("defaultRunRuntimeCommand returned error: %v", err)
	}
	want := []string{
		"/fake/docker", "exec",
		"-e", "COMMENT_IO_AGENT_SANDBOX=1",
		"-e", "COMMENT_IO_RUNTIME_REQUEST_MODEL_EXPLICIT=1",
		"-e", "COMMENT_IO_RUNTIME_REQUEST_MODEL=",
		"comment-agent-comment-io", "comment", "run", "--detach", "max.reviewer",
	}
	if !slices.Equal(calls.run, want) {
		t.Fatalf("docker run args = %#v, want %#v", calls.run, want)
	}
}

func TestDockerRuntimeRootShorthandPreservesRuntimeArgs(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubDockerRuntime(t, "https://comment.io")

	output, err := captureRun(t, []string{
		"--runtime", "claude",
		"--profile", "max.reviewer",
		"--role", "task",
		"--setup-attempt-id", "bla_1234567890abc",
		"--detach",
		"--",
		"--model", "opus",
	})
	if err != nil {
		t.Fatalf("run returned error: %v\n%s", err, output)
	}
	wantSuffix := []string{
		"comment-agent-comment-io", "comment", "run",
		"--runtime", "claude",
		"--profile", "max.reviewer",
		"--role", "task",
		"--setup-attempt-id", "bla_1234567890abc",
		"--detach",
		"--",
		"--model", "opus",
	}
	if len(calls.run) < len(wantSuffix) || !slices.Equal(calls.run[len(calls.run)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("docker run args = %#v, want suffix %#v", calls.run, wantSuffix)
	}
}

func TestDockerRuntimeDelegatedArgvForwardsModel(t *testing.T) {
	got := dockerRuntimeDelegatedArgv(runtimeRunOptions{
		Runtime:     "claude",
		Profile:     "max.reviewer",
		Model:       "opus",
		RuntimeArgs: []string{"--name", "Claude Session"},
	})
	want := []string{
		"run",
		"--runtime", "claude",
		"--profile", "max.reviewer",
		"--model", "opus",
		"--",
		"--name", "Claude Session",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("delegated argv = %#v, want %#v", got, want)
	}
}

func TestDockerRuntimeDoesNotShadowReachableHostDaemon(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	oldHost := dockerRuntimeHostDaemonUnavailable
	oldLookPath := dockerRuntimeLookPath
	dockerRuntimeHostDaemonUnavailable = func(context.Context, commentbus.Paths) bool { return false }
	dockerRuntimeLookPath = func(string) (string, error) { t.Fatal("docker should not be probed"); return "", nil }
	t.Cleanup(func() {
		dockerRuntimeHostDaemonUnavailable = oldHost
		dockerRuntimeLookPath = oldLookPath
	})

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if err != nil || handled {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want false, nil", handled, err)
	}
}

func TestDockerRuntimeRejectsHostOnlyOptions(t *testing.T) {
	for _, options := range []runtimeRunOptions{
		{BotShortcut: "max.reviewer", Home: "/tmp/comment-home"},
		{BotShortcut: "max.reviewer", CWD: "/Users/example/project"},
		{Runtime: "/opt/homebrew/bin/claude", Profile: "max.reviewer"},
		{Runtime: "./claude", Profile: "max.reviewer"},
		{Runtime: `.\\claude`, Profile: "max.reviewer"},
		{Runtime: `C:\Users\example\bin\claude.exe`, Profile: "max.reviewer"},
		{Runtime: "C:/Users/example/bin/claude.exe", Profile: "max.reviewer"},
		{Runtime: "~/.local/bin/claude", Profile: "max.reviewer"},
	} {
		if dockerRuntimeInvocationSafe(options) {
			t.Fatalf("dockerRuntimeInvocationSafe(%+v) = true, want false", options)
		}
	}
}

func TestDockerRuntimeAllowsBareManagedRuntimeNames(t *testing.T) {
	for _, options := range []runtimeRunOptions{
		{Runtime: "claude", Profile: "max.reviewer"},
		{Runtime: "codex", Profile: "max.reviewer"},
	} {
		if !dockerRuntimeInvocationSafe(options) {
			t.Fatalf("dockerRuntimeInvocationSafe(%+v) = false, want true", options)
		}
	}
}

func TestDockerRuntimeAttachRequiresTTY(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	stubDockerRuntime(t, "https://comment.io")
	dockerRuntimeStdinIsTerminal = func() bool { return false }
	dockerRuntimeStdoutIsTerminal = func() bool { return false }

	err = defaultRunRuntimeCommand(runtimeRunOptions{
		BotShortcut: "max.reviewer",
		Role:        commentbus.RuntimeRoleMain,
	})
	if err == nil || !strings.Contains(err.Error(), "needs an interactive terminal") {
		t.Fatalf("error = %v, want interactive terminal guidance", err)
	}
}

func TestDockerRuntimeBaseMismatchIsFatal(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	stubDockerRuntime(t, "https://other.example")

	err = defaultRunRuntimeCommand(runtimeRunOptions{
		BotShortcut: "max.reviewer",
		Role:        commentbus.RuntimeRoleMain,
		Detach:      true,
	})
	if err == nil || !strings.Contains(err.Error(), "paired to https://other.example") {
		t.Fatalf("error = %v, want base mismatch", err)
	}
}

func TestDockerRuntimePropagatesChildExitCode(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	stubDockerRuntime(t, "https://comment.io")
	dockerRuntimeRunCommand = func(context.Context, string, ...string) error {
		return cliExitError{Code: 42}
	}

	err = defaultRunRuntimeCommand(runtimeRunOptions{
		BotShortcut: "max.reviewer",
		Role:        commentbus.RuntimeRoleMain,
		Detach:      true,
	})
	var exitErr cliExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 42 {
		t.Fatalf("error = %#v, want cliExitError code 42", err)
	}
}

func TestDockerRuntimeSkipsInsideSandbox(t *testing.T) {
	resetDockerRuntimeEnv(t)
	t.Setenv("COMMENT_IO_AGENT_SANDBOX", "1")
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	stubDockerRuntime(t, "https://comment.io")

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if err != nil || handled {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want false, nil", handled, err)
	}
}

func TestDockerRuntimeInstallMarkerSelectsCustomOrigin(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://example.comt.dev",
		Container: "comment-agent-example-comt-dev",
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubDockerRuntime(t, "https://example.comt.dev")

	err = defaultRunRuntimeCommand(runtimeRunOptions{
		BotShortcut: "max.reviewer",
		Role:        commentbus.RuntimeRoleMain,
		Detach:      true,
	})
	if err != nil {
		t.Fatalf("defaultRunRuntimeCommand returned error: %v", err)
	}
	if !slices.Contains(calls.run, "comment-agent-example-comt-dev") {
		t.Fatalf("docker args = %#v, want marker container", calls.run)
	}
}

func TestDockerRuntimeMissingMarkerUsesProjectedProfileBaseURL(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestAgentProfile(t, paths.Home, "max.reviewer", `{"agent_secret":"as_testsecret","base_url":"https://example.comt.dev","runtime":"claude"}`)
	writeDockerRuntimeProjectionManifest(t, paths, "max.reviewer")
	calls := stubDockerRuntime(t, "https://example.comt.dev")

	err = defaultRunRuntimeCommand(runtimeRunOptions{
		BotShortcut: "max.reviewer",
		Role:        commentbus.RuntimeRoleMain,
		Detach:      true,
	})
	if err != nil {
		t.Fatalf("defaultRunRuntimeCommand returned error: %v", err)
	}
	if !slices.Contains(calls.run, "comment-agent-example-comt-dev") {
		t.Fatalf("docker args = %#v, want projected profile container", calls.run)
	}
}

func TestDockerRuntimeMissingMarkerToleratesCorruptRegistryForProjectedProfileShortcut(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(filepath.Dir(paths.Home), "botlets")
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: botletsHome}); err != nil {
		t.Fatal(err)
	}
	writeTestAgentProfile(t, paths.Home, "max.reviewer", `{"agent_secret":"as_testsecret","base_url":"https://example.comt.dev","runtime":"claude"}`)
	writeDockerRuntimeProjectionManifest(t, paths, "max.reviewer")
	calls := stubDockerRuntime(t, "https://example.comt.dev")

	err = defaultRunRuntimeCommand(runtimeRunOptions{
		BotShortcut: "max.reviewer",
		Role:        commentbus.RuntimeRoleMain,
		Detach:      true,
	})
	if err != nil {
		t.Fatalf("defaultRunRuntimeCommand returned error: %v", err)
	}
	if !slices.Contains(calls.run, "comment-agent-example-comt-dev") {
		t.Fatalf("docker args = %#v, want projected profile delegation despite corrupt Botlets registry", calls.run)
	}
}

func TestDockerRuntimeMissingMarkerDoesNotCaptureUnprojectedProfile(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestAgentProfile(t, paths.Home, "max.reviewer", `{"agent_secret":"as_testsecret","base_url":"https://example.comt.dev","runtime":"claude"}`)
	oldHost := dockerRuntimeHostDaemonUnavailable
	oldLookPath := dockerRuntimeLookPath
	dockerRuntimeHostDaemonUnavailable = func(context.Context, commentbus.Paths) bool { return true }
	dockerRuntimeLookPath = func(string) (string, error) {
		t.Fatal("docker should not be probed for an unprojected local profile without marker")
		return "", nil
	}
	t.Cleanup(func() {
		dockerRuntimeHostDaemonUnavailable = oldHost
		dockerRuntimeLookPath = oldLookPath
	})

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if err != nil || handled {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want false, nil", handled, err)
	}
}

func TestDockerRuntimeMarkerDoesNotCaptureUnprojectedLocalProfile(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestAgentProfile(t, paths.Home, "max.reviewer", `{"agent_secret":"as_testsecret","base_url":"https://other.example","runtime":"claude"}`)
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	oldHost := dockerRuntimeHostDaemonUnavailable
	oldLookPath := dockerRuntimeLookPath
	dockerRuntimeHostDaemonUnavailable = func(context.Context, commentbus.Paths) bool { return true }
	dockerRuntimeLookPath = func(string) (string, error) {
		t.Fatal("docker should not be probed for an unprojected local profile")
		return "", nil
	}
	t.Cleanup(func() {
		dockerRuntimeHostDaemonUnavailable = oldHost
		dockerRuntimeLookPath = oldLookPath
	})

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if err != nil || handled {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want false, nil", handled, err)
	}
}

func TestDockerRuntimeMarkerDoesNotCaptureNativeBotletsHandleAlias(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(t.TempDir(), "botlets")
	profilePath, err := writeBotletsAgentProfile(paths, "max.research-reader", "as_ag_test_secret", "https://comment.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := upsertBotletsRegistry(paths, botletsHome, commentbus.BotRegistryEntry{
		Name:              "research-reader",
		BotID:             "ag_bot",
		Handle:            "max.research-reader",
		HandleAliases:     []string{"max.reviewer"},
		CredentialProfile: profilePath,
		ManagedSession:    commentbus.ManagedSessionSetting{Enabled: true, Runtime: "claude"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: botletsHome}); err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	oldHost := dockerRuntimeHostDaemonUnavailable
	oldLookPath := dockerRuntimeLookPath
	dockerRuntimeHostDaemonUnavailable = func(context.Context, commentbus.Paths) bool { return true }
	dockerRuntimeLookPath = func(string) (string, error) {
		t.Fatal("docker should not be probed for a native Botlets handle alias")
		return "", nil
	}
	t.Cleanup(func() {
		dockerRuntimeHostDaemonUnavailable = oldHost
		dockerRuntimeLookPath = oldLookPath
	})

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{
		BotShortcut: "max.reviewer",
		Role:        commentbus.RuntimeRoleMain,
	})
	if err != nil || handled {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want false, nil", handled, err)
	}
}

func TestDockerRuntimeProjectedProfileBaseOverridesStaleMarker(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestAgentProfile(t, paths.Home, "max.reviewer", `{"agent_secret":"as_testsecret","base_url":"https://example.comt.dev","runtime":"claude"}`)
	writeDockerRuntimeProjectionManifest(t, paths, "max.reviewer")
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubDockerRuntime(t, "https://example.comt.dev")

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if err != nil || !handled {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want delegated projected profile origin", handled, err)
	}
	if !slices.Contains(calls.run, "comment-agent-example-comt-dev") {
		t.Fatalf("docker args = %#v, want projected profile container", calls.run)
	}
}

func TestDockerRuntimeReadsInstallMarkerWithoutPrivateFileChecks(t *testing.T) {
	resetDockerRuntimeEnv(t)
	disableDockerRuntimePrivateFileChecksForTest(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}

	target, ok, err := readDockerRuntimeInstallMarker(paths)
	if err != nil || !ok {
		t.Fatalf("readDockerRuntimeInstallMarker = (%+v, %v, %v), want marker", target, ok, err)
	}
	if target.BaseURL != "https://comment.io" || target.Container != "comment-agent-comment-io" || !target.fromMarker {
		t.Fatalf("target = %+v, want marker target", target)
	}
}

func TestDockerRuntimeProjectedProfileBaseOverridesStaleMarkerWithoutPrivateFileChecks(t *testing.T) {
	resetDockerRuntimeEnv(t)
	disableDockerRuntimePrivateFileChecksForTest(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestAgentProfile(t, paths.Home, "max.reviewer", `{"agent_secret":"as_testsecret","base_url":"https://example.comt.dev","runtime":"claude"}`)
	writeDockerRuntimeProjectionManifest(t, paths, "max.reviewer")
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubDockerRuntime(t, "https://example.comt.dev")

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if err != nil || !handled {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want delegated projected profile origin", handled, err)
	}
	if !slices.Contains(calls.run, "comment-agent-example-comt-dev") {
		t.Fatalf("docker args = %#v, want projected profile container", calls.run)
	}
}

func TestDockerRuntimeInvalidInstallMarkerFailsClosed(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Bus, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dockerRuntimeInstallMarkerPath(paths), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldHost := dockerRuntimeHostDaemonUnavailable
	oldLookPath := dockerRuntimeLookPath
	dockerRuntimeHostDaemonUnavailable = func(context.Context, commentbus.Paths) bool { return true }
	dockerRuntimeLookPath = func(string) (string, error) {
		t.Fatal("docker should not be looked up when marker is invalid")
		return "", nil
	}
	t.Cleanup(func() {
		dockerRuntimeHostDaemonUnavailable = oldHost
		dockerRuntimeLookPath = oldLookPath
	})

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if !handled || err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want handled invalid marker error", handled, err)
	}
}

func TestDockerRuntimeSymlinkInstallMarkerFailsClosed(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Bus, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(paths.Home, "marker-target.json")
	if err := os.WriteFile(target, []byte(`{"base_url":"https://comment.io","container":"comment-agent-comment-io"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dockerRuntimeInstallMarkerPath(paths)); err != nil {
		t.Fatal(err)
	}
	oldHost := dockerRuntimeHostDaemonUnavailable
	oldLookPath := dockerRuntimeLookPath
	dockerRuntimeHostDaemonUnavailable = func(context.Context, commentbus.Paths) bool { return true }
	dockerRuntimeLookPath = func(string) (string, error) {
		t.Fatal("docker should not be looked up when marker is a symlink")
		return "", nil
	}
	t.Cleanup(func() {
		dockerRuntimeHostDaemonUnavailable = oldHost
		dockerRuntimeLookPath = oldLookPath
	})

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if !handled || err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want handled symlink marker error", handled, err)
	}
}

func TestDockerRuntimePermissiveInstallMarkerFailsClosed(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dockerRuntimeInstallMarkerPath(paths), 0o644); err != nil {
		t.Fatal(err)
	}
	oldHost := dockerRuntimeHostDaemonUnavailable
	oldLookPath := dockerRuntimeLookPath
	dockerRuntimeHostDaemonUnavailable = func(context.Context, commentbus.Paths) bool { return true }
	dockerRuntimeLookPath = func(string) (string, error) {
		t.Fatal("docker should not be looked up when marker is not private")
		return "", nil
	}
	t.Cleanup(func() {
		dockerRuntimeHostDaemonUnavailable = oldHost
		dockerRuntimeLookPath = oldLookPath
	})

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if !handled || err == nil || !strings.Contains(err.Error(), "must be private") {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want handled private marker error", handled, err)
	}
}

func TestDockerRuntimeCorruptSelectedProfileFailsClosed(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestAgentProfile(t, paths.Home, "max.reviewer", "{not-json")
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	oldHost := dockerRuntimeHostDaemonUnavailable
	oldLookPath := dockerRuntimeLookPath
	dockerRuntimeHostDaemonUnavailable = func(context.Context, commentbus.Paths) bool { return true }
	dockerRuntimeLookPath = func(string) (string, error) {
		t.Fatal("docker should not be looked up when selected local profile failed to load")
		return "", nil
	}
	t.Cleanup(func() {
		dockerRuntimeHostDaemonUnavailable = oldHost
		dockerRuntimeLookPath = oldLookPath
	})

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if !handled || err == nil || !strings.Contains(err.Error(), `agent profile "max.reviewer" could not be loaded`) {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want handled profile load error", handled, err)
	}
}

func TestDockerRuntimeMarkedInstallToleratesCorruptHostBotletsRegistry(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	botletsHome := filepath.Join(filepath.Dir(paths.Home), "botlets")
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(botletsHome, "registry.json"), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := commentbus.WriteBusConfig(paths, commentbus.BusConfig{BotletsHome: botletsHome}); err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	calls := stubDockerRuntime(t, "https://comment.io")

	err = defaultRunRuntimeCommand(runtimeRunOptions{
		BotShortcut: "reviewer",
		Role:        commentbus.RuntimeRoleMain,
		Detach:      true,
	})
	if err != nil {
		t.Fatalf("defaultRunRuntimeCommand returned error: %v", err)
	}
	if !slices.Contains(calls.run, "comment-agent-comment-io") {
		t.Fatalf("docker args = %#v, want marker delegation despite corrupt host registry", calls.run)
	}
}

func TestDockerRuntimeSymlinkProjectionManifestDoesNotOwnProfile(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestAgentProfile(t, paths.Home, "max.reviewer", `{"agent_secret":"as_testsecret","base_url":"https://comment.io","runtime":"claude"}`)
	agentsDir := filepath.Join(paths.Home, "agents")
	target := filepath.Join(paths.Home, "manifest-target.json")
	if err := os.WriteFile(target, []byte(`{"files":["max.reviewer.json"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(agentsDir, ".comment-agent-projected.manifest")); err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	oldHost := dockerRuntimeHostDaemonUnavailable
	oldLookPath := dockerRuntimeLookPath
	dockerRuntimeHostDaemonUnavailable = func(context.Context, commentbus.Paths) bool { return true }
	dockerRuntimeLookPath = func(string) (string, error) {
		t.Fatal("docker should not be looked up when projection manifest is untrusted")
		return "", nil
	}
	t.Cleanup(func() {
		dockerRuntimeHostDaemonUnavailable = oldHost
		dockerRuntimeLookPath = oldLookPath
	})

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if err != nil || handled {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want untrusted manifest treated as native profile", handled, err)
	}
}

func TestDockerRuntimePermissiveProjectionManifestDoesNotOwnProfile(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	writeTestAgentProfile(t, paths.Home, "max.reviewer", `{"agent_secret":"as_testsecret","base_url":"https://comment.io","runtime":"claude"}`)
	writeDockerRuntimeProjectionManifest(t, paths, "max.reviewer")
	if err := os.Chmod(filepath.Join(paths.Home, "agents", ".comment-agent-projected.manifest"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	oldHost := dockerRuntimeHostDaemonUnavailable
	oldLookPath := dockerRuntimeLookPath
	dockerRuntimeHostDaemonUnavailable = func(context.Context, commentbus.Paths) bool { return true }
	dockerRuntimeLookPath = func(string) (string, error) {
		t.Fatal("docker should not be looked up when projection manifest is not private")
		return "", nil
	}
	t.Cleanup(func() {
		dockerRuntimeHostDaemonUnavailable = oldHost
		dockerRuntimeLookPath = oldLookPath
	})

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if err != nil || handled {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want permissive manifest treated as native profile", handled, err)
	}
}

func TestDockerRuntimeMarkedInstallRequiresDockerOnPath(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	oldHost := dockerRuntimeHostDaemonUnavailable
	oldLookPath := dockerRuntimeLookPath
	dockerRuntimeHostDaemonUnavailable = func(context.Context, commentbus.Paths) bool { return true }
	dockerRuntimeLookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() {
		dockerRuntimeHostDaemonUnavailable = oldHost
		dockerRuntimeLookPath = oldLookPath
	})

	handled, err := maybeDelegateRuntimeToDocker(context.Background(), paths, runtimeRunOptions{BotShortcut: "max.reviewer"})
	if !handled || err == nil || !strings.Contains(err.Error(), "docker command is not on your PATH") {
		t.Fatalf("maybeDelegateRuntimeToDocker = (%v, %v), want handled docker PATH guidance", handled, err)
	}
}

func TestDockerRuntimeStoppedMarkedContainerReturnsActionableError(t *testing.T) {
	resetDockerRuntimeEnv(t)
	paths, err := resolveCLIPaths("")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	}); err != nil {
		t.Fatal(err)
	}
	stubDockerRuntime(t, "https://comment.io")
	dockerRuntimeCombinedOutput = func(_ context.Context, command string, args ...string) ([]byte, error) {
		if filepath.Base(command) != "docker" {
			return nil, errors.New("unexpected command")
		}
		if len(args) >= 1 && args[0] == "inspect" {
			return []byte("false\n"), nil
		}
		return nil, errors.New("unexpected docker args")
	}

	err = defaultRunRuntimeCommand(runtimeRunOptions{
		BotShortcut: "max.reviewer",
		Role:        commentbus.RuntimeRoleMain,
		Detach:      true,
	})
	if err == nil || !strings.Contains(err.Error(), "docker start comment-agent-comment-io") {
		t.Fatalf("error = %v, want docker start guidance", err)
	}
}
