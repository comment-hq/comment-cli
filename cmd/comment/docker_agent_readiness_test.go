package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

func TestDockerAgentMountPointsParsesMountinfo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mountinfo")
	data := "36 25 0:32 / /state rw,relatime - ext4 /dev/sda rw\n" +
		"37 25 0:33 / /home/agent\\040with\\040space rw,relatime - ext4 /dev/sdb rw\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}

	mounts := dockerAgentMountPoints(path)
	if mounts["/state"].FSType != "ext4" || mounts["/state"].Source != "/dev/sda" {
		t.Fatalf("mountinfo did not include /state: %#v", mounts)
	}
	if mounts["/home/agent with space"].FSType != "ext4" {
		t.Fatalf("mountinfo did not unescape path with spaces: %#v", mounts)
	}
}

func TestDockerAgentMountStatusRejectsEphemeralMount(t *testing.T) {
	mounts := map[string]dockerAgentMountPoint{
		"/state": {
			Path:   "/state",
			FSType: "tmpfs",
			Source: "tmpfs",
		},
	}
	status := dockerAgentMountStatus("/state", mounts)
	if !status.Mounted {
		t.Fatalf("tmpfs /state should still be reported mounted: %#v", status)
	}
	if status.Persistent {
		t.Fatalf("tmpfs /state must not be reported persistent: %#v", status)
	}
}

func TestAddDockerAgentHealthOnlyInsideSandbox(t *testing.T) {
	t.Setenv(dockerRuntimeSandboxEnv, "")
	payload := addDockerAgentHealth(map[string]any{"ok": true}, commentbus.Paths{Home: "/state"})
	if _, ok := payload["agent_sandbox"]; ok {
		t.Fatalf("agent_sandbox health should be absent outside sandbox: %#v", payload)
	}
	if _, ok := payload["docker_agent_readiness"]; ok {
		t.Fatalf("docker_agent_readiness health should be absent outside sandbox: %#v", payload)
	}

	t.Setenv(dockerRuntimeSandboxEnv, "1")
	t.Setenv("HOME", "/home/agent")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "codex"))
	mountinfo := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(mountinfo, []byte("36 25 0:32 / /state rw,relatime - ext4 /dev/sda rw\n37 25 0:33 / /home/agent rw,relatime - ext4 /dev/sdb rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prevMountInfo := dockerAgentMountInfoPath
	dockerAgentMountInfoPath = mountinfo
	t.Cleanup(func() { dockerAgentMountInfoPath = prevMountInfo })
	withRuntimeLookPath(t, lookPathSet())

	payload = addDockerAgentHealth(map[string]any{"ok": true}, commentbus.Paths{Home: "/state"})
	if sandbox, ok := payload["agent_sandbox"].(bool); !ok || !sandbox {
		t.Fatalf("agent_sandbox health missing: %#v", payload)
	}
	readiness, ok := payload["docker_agent_readiness"].(*dockerAgentReadiness)
	if !ok || readiness == nil {
		t.Fatalf("docker_agent_readiness health missing or wrong type: %#v", payload["docker_agent_readiness"])
	}
	if !readiness.State.Persistent || !readiness.Home.Persistent {
		t.Fatalf("docker_agent mounts not marked persistent: %#v", readiness)
	}
}

func TestDockerAgentRuntimeStatusesUseFileBackedAuth(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	customCodexHome := filepath.Join(t.TempDir(), "custom-codex")
	t.Setenv("CODEX_HOME", customCodexHome)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	if err := os.MkdirAll(customCodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(customCodexHome, "auth.json"), []byte(`{"tokens":{"access_token":"abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	withRuntimeLookPath(t, lookPathSet("claude", "codex"))

	statuses := dockerAgentRuntimeStatuses()
	for _, status := range statuses {
		if status.Authenticated {
			t.Fatalf("%s authenticated from env key or CODEX_HOME in Docker readiness: %#v", status.Name, statuses)
		}
	}

	defaultCodexHome := filepath.Join(tmp, ".codex")
	if err := os.MkdirAll(defaultCodexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultCodexHome, "auth.json"), []byte(`{"tokens":{"access_token":"abc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	statuses = dockerAgentRuntimeStatuses()
	foundCodex := false
	for _, status := range statuses {
		if status.Name == "codex" {
			foundCodex = true
			if !status.Authenticated {
				t.Fatalf("codex auth in default persisted home should count: %#v", statuses)
			}
		}
	}
	if !foundCodex {
		t.Fatalf("codex runtime status missing: %#v", statuses)
	}
}
