//go:build darwin || linux

package commentbus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeEnvironmentVars(t *testing.T) {
	t.Run("production returns nil", func(t *testing.T) {
		t.Setenv(EnvVar, "")
		t.Setenv(StagingBaseURLEnvVar, "")
		t.Setenv(BaseURLEnvVar, "")
		if got := RuntimeEnvironmentVars(); got != nil {
			t.Fatalf("production RuntimeEnvironmentVars() = %#v, want nil", got)
		}
	})

	t.Run("staging forwards env and overrides", func(t *testing.T) {
		t.Setenv(EnvVar, EnvStaging)
		t.Setenv(StagingBaseURLEnvVar, "https://staging.example")
		t.Setenv(BaseURLEnvVar, "https://generic.example")
		got := strings.Join(RuntimeEnvironmentVars(), "\n")
		for _, want := range []string{
			EnvVar + "=" + EnvStaging,
			StagingBaseURLEnvVar + "=https://staging.example",
			BaseURLEnvVar + "=https://generic.example",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("RuntimeEnvironmentVars() missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("staging without overrides forwards only the env name", func(t *testing.T) {
		t.Setenv(EnvVar, EnvStaging)
		t.Setenv(StagingBaseURLEnvVar, "")
		t.Setenv(BaseURLEnvVar, "")
		got := RuntimeEnvironmentVars()
		if len(got) != 1 || got[0] != EnvVar+"="+EnvStaging {
			t.Fatalf("RuntimeEnvironmentVars() = %#v, want [%q]", got, EnvVar+"="+EnvStaging)
		}
	})
}

// TestManagedSessionEnvForwardsStagingEnv proves the env reaches the runtime
// end-to-end through RunSessionExec/managedSessionEnv when the daemon is staging,
// addressing the Codex review point that managed runtimes otherwise fall back to
// production defaults.
func TestManagedSessionEnvForwardsStagingEnv(t *testing.T) {
	t.Setenv(EnvVar, EnvStaging)
	t.Setenv(StagingBaseURLEnvVar, "https://staging.example")

	paths := testDaemonPaths(t)
	binDir := filepath.Join(paths.Home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(binDir, "claude")
	if err := os.WriteFile(claudePath, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	record, err := RegisterSession(RegisterSessionOptions{
		Paths:       paths,
		Profile:     "max.reviewer",
		BotName:     "reviewer",
		ScopeType:   "profile",
		ScopeID:     "max.reviewer",
		BotletsHome: filepath.Join(paths.Home, "botlets"),
		SessionName: "comment-reviewer-abc123",
		PaneTarget:  "comment-reviewer-abc123:0.0",
		State:       "starting",
	})
	if err != nil {
		t.Fatal(err)
	}
	resolvedClaudePath, err := filepath.EvalSymlinks(claudePath)
	if err != nil {
		t.Fatal(err)
	}
	record.RuntimePath = resolvedClaudePath
	record.RuntimeCommandPath = claudePath
	record.RuntimeLaunchMode = RuntimeLaunchModePath
	if err := WriteSessionRecord(paths, record); err != nil {
		t.Fatal(err)
	}

	var execEnv []string
	sentinel := errors.New("exec sentinel")
	err = RunSessionExec(SessionExecOptions{
		Paths:      paths,
		SessionID:  record.SessionID,
		Generation: record.Generation,
		LookPath:   func(name string) (string, error) { return "", errors.New("lookpath should not be called") },
		Exec: func(path string, argv []string, env []string) error {
			execEnv = append([]string{}, env...)
			return sentinel
		},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("RunSessionExec error = %v", err)
	}
	env := strings.Join(execEnv, "\n")
	for _, want := range []string{
		EnvVar + "=" + EnvStaging,
		StagingBaseURLEnvVar + "=https://staging.example",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("managed runtime env missing %q:\n%s", want, env)
		}
	}
}
