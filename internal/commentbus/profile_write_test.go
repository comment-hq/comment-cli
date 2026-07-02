//go:build darwin || linux

package commentbus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareAgentProfileWriteRejectsInvalidInputs(t *testing.T) {
	paths, err := ResolvePaths(filepath.Join(t.TempDir(), ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		handle  string
		secret  string
		runtime string
		want    error
	}{
		{"empty handle", "", "as_ag_secret", "", ErrMissingAgentCredential},
		{"empty secret", "max.reviewer", "", "", ErrMissingAgentCredential},
		{"invalid handle", "max.bad/../../escape", "as_ag_secret", "", ErrInvalidAgentHandle},
		{"invalid runtime", "max.reviewer", "as_ag_secret", "gpt", ErrInvalidAgentRuntime},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PrepareAgentProfileWrite(paths, tc.handle, tc.secret, "https://comment.io", tc.runtime)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(paths.Home, "escape.json")); !os.IsNotExist(err) {
		t.Fatalf("unsafe profile path was created, stat err = %v", err)
	}
}

func TestPrepareAgentProfileWriteAcceptsEmptyRuntime(t *testing.T) {
	paths, err := ResolvePaths(filepath.Join(t.TempDir(), ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	write, err := PrepareAgentProfileWrite(paths, "max.reviewer", "as_ag_secret", "https://comment.io/", "")
	if err != nil {
		t.Fatal(err)
	}
	var profile map[string]any
	if err := json.Unmarshal(write.Data, &profile); err != nil {
		t.Fatal(err)
	}
	if _, ok := profile["runtime"]; ok {
		t.Fatalf("empty runtime serialized into profile: %#v", profile)
	}
	if write.Profile.Runtime != "" {
		t.Fatalf("profile runtime = %q, want empty", write.Profile.Runtime)
	}
	if write.Profile.BaseURL != "https://comment.io" {
		t.Fatalf("profile base URL = %q, want trailing slash trimmed", write.Profile.BaseURL)
	}
}

func TestNormalizeAgentModelUsesServerLengthUnits(t *testing.T) {
	cases := []struct {
		name  string
		model string
		ok    bool
	}{
		{"ascii at limit", strings.Repeat("a", 120), true},
		{"ascii over limit", strings.Repeat("a", 121), false},
		{"multibyte BMP at limit", strings.Repeat("é", 120), true},
		{"multibyte BMP over limit", strings.Repeat("é", 121), false},
		{"astral at limit", strings.Repeat("🧠", 60), true},
		{"astral over limit", strings.Repeat("🧠", 61), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ok := NormalizeAgentModel(tc.model)
			if ok != tc.ok {
				t.Fatalf("NormalizeAgentModel(%q) ok = %v, want %v", tc.model, ok, tc.ok)
			}
		})
	}
}

func TestAgentProfileWriteWritesPrivateFileWithExpectedJSON(t *testing.T) {
	paths, err := ResolvePaths(filepath.Join(t.TempDir(), ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	write, err := PrepareAgentProfileWrite(paths, "max.runner", "as_ag_secret", "https://comment.io", "codex")
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(paths.Home, "agents", "max.runner.json")
	if write.Path != wantPath {
		t.Fatalf("write path = %q, want %q", write.Path, wantPath)
	}
	if err := write.Write(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(write.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("profile mode = %v, want 0600", info.Mode().Perm())
	}
	data, err := os.ReadFile(write.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatal("profile file missing trailing newline")
	}
	var profile map[string]string
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"handle":       "max.runner",
		"agent_secret": "as_ag_secret",
		"base_url":     "https://comment.io",
		"runtime":      "codex",
	}
	if len(profile) != len(want) {
		t.Fatalf("profile = %#v, want %#v", profile, want)
	}
	for key, value := range want {
		if profile[key] != value {
			t.Fatalf("profile[%q] = %q, want %q", key, profile[key], value)
		}
	}
}

func TestAgentProfileWriteOverwritesExistingProfile(t *testing.T) {
	paths, err := ResolvePaths(filepath.Join(t.TempDir(), ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := PrepareAgentProfileWrite(paths, "max.reviewer", "as_ag_old_secret", "https://comment.io", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Write(); err != nil {
		t.Fatal(err)
	}
	second, err := PrepareAgentProfileWrite(paths, "max.reviewer", "as_ag_new_secret", "https://comment.io", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if second.Path != first.Path {
		t.Fatalf("rewrite path = %q, want %q", second.Path, first.Path)
	}
	if err := second.Write(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(second.Path)
	if err != nil {
		t.Fatal(err)
	}
	var profile map[string]string
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}
	if profile["agent_secret"] != "as_ag_new_secret" || profile["runtime"] != "claude" {
		t.Fatalf("profile after overwrite = %#v", profile)
	}
	info, err := os.Stat(second.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("profile mode after overwrite = %v, want 0600", info.Mode().Perm())
	}
}

func TestPrepareAgentProfileWriteRejectsSymlinkedAgentsDir(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	realAgents := filepath.Join(root, "real-agents")
	if err := os.MkdirAll(realAgents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realAgents, filepath.Join(paths.Home, "agents")); err != nil {
		t.Skipf("symlink not available: %v", err)
	}

	_, err = PrepareAgentProfileWrite(paths, "max.reviewer", "as_ag_secret", "https://comment.io", "")
	if err == nil {
		t.Fatal("expected symlinked agents directory rejection")
	}
	if !strings.Contains(err.Error(), "agent profiles directory") {
		t.Fatalf("err = %v, want agent profiles directory trust failure", err)
	}
}

// A profile written by the generic helper must round-trip through the same
// loader the daemon uses on reload — this is the format-compatibility contract
// the enrollment worker depends on.
func TestAgentProfileWriteIsLoadableByProfileState(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	write, err := PrepareAgentProfileWrite(paths, "max.runner", "as_ag1234_secret", "https://comment.io/", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if err := write.Write(); err != nil {
		t.Fatal(err)
	}
	state, errs := LoadProfileState(context.Background(), ProfileLoadOptions{
		Paths:       paths,
		BotletsHome: filepath.Join(root, "botlets"),
	})
	for _, e := range errs {
		if isAgentProfileEntryError(e.Code) {
			t.Fatalf("profile reload error: %+v", e)
		}
	}
	profile, ok := state.AgentProfiles["max.runner"]
	if !ok {
		t.Fatalf("profile not loaded; got %+v", state.AgentProfiles)
	}
	if profile.AgentSecret != "as_ag1234_secret" || profile.Runtime != "codex" || profile.BaseURL != "https://comment.io" {
		t.Fatalf("profile = %+v", profile)
	}
}
