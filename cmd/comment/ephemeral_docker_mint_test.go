//go:build darwin || linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubContainerMint replaces the docker-exec seam with a canned response and
// records the args it was called with. Restores the real seam on cleanup.
func stubContainerMint(t *testing.T, out string, gotContainer, gotBase, gotSess *string) {
	t.Helper()
	prev := ephemeralContainerMint
	ephemeralContainerMint = func(_ context.Context, container, sess, _, base string) ([]byte, error) {
		if gotContainer != nil {
			*gotContainer = container
		}
		if gotBase != nil {
			*gotBase = base
		}
		if gotSess != nil {
			*gotSess = sess
		}
		return []byte(out), nil
	}
	t.Cleanup(func() { ephemeralContainerMint = prev })
}

// The docker-exec mint persists the container's ephemeral cred to the HOST
// store (cred file + session bind) and targets the deterministic
// comment-agent-<slug> container for the origin.
func TestEphemeralDockerExecMintPersistsToHostStore(t *testing.T) {
	dir := t.TempDir()
	ephemeralDir := filepath.Join(dir, "ephemeral")
	bindFile := filepath.Join(dir, "rewake", "bind-sess1")
	if err := os.MkdirAll(ephemeralDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bindFile), 0o700); err != nil {
		t.Fatal(err)
	}
	var gotContainer, gotBase string
	stubContainerMint(t, `{"handle":"max.e-abcd1234","agent_secret":"as_ag_t_secret","identity_class":"ephemeral","actor_id":"ai:max.e-abcd1234","base_url":"https://comt.dev","expires_at":"2999-01-01T00:00:00.000Z"}`, &gotContainer, &gotBase, nil)

	base := "https://comt.dev"
	cred, credPath, ok := ephemeralTryDockerExecMint("sess1", "Pat (Pairing)", base, ephemeralDir, bindFile)
	if !ok {
		t.Fatalf("ephemeralTryDockerExecMint ok = false, want true")
	}
	if cred.Handle != "max.e-abcd1234" || cred.AgentSecret != "as_ag_t_secret" {
		t.Fatalf("cred = %+v, want handle/secret from the container", cred)
	}
	// The container's reported base_url + expires_at are threaded into the host cred.
	if cred.BaseURL != "https://comt.dev" || cred.Session != "sess1" {
		t.Fatalf("cred base/session = (%q,%q), want (https://comt.dev,sess1)", cred.BaseURL, cred.Session)
	}
	if cred.IdentityClass != "ephemeral" {
		t.Fatalf("cred identity_class = %q, want ephemeral", cred.IdentityClass)
	}
	if cred.ExpiresAt != "2999-01-01T00:00:00.000Z" {
		t.Fatalf("cred expires_at = %q, want the container's value", cred.ExpiresAt)
	}
	// Targeted the deterministic container for the origin.
	if want := dockerAgentContainerName(dockerAgentSlug(base)); gotContainer != want {
		t.Fatalf("docker exec container = %q, want %q", gotContainer, want)
	}
	if gotBase != base {
		t.Fatalf("in-container --base-url = %q, want %q", gotBase, base)
	}
	// Persisted to the HOST store + bind pointer.
	if credPath != filepath.Join(ephemeralDir, "max.e-abcd1234.json") {
		t.Fatalf("credPath = %q", credPath)
	}
	if _, err := os.Stat(credPath); err != nil {
		t.Fatalf("cred not written to host store: %v", err)
	}
	if b, _ := os.ReadFile(bindFile); strings.TrimSpace(string(b)) != "max.e-abcd1234" {
		t.Fatalf("bind = %q, want max.e-abcd1234", strings.TrimSpace(string(b)))
	}
}

// A stale/colliding container that minted against a DIFFERENT origin is rejected
// up front — never hand this session a credential for the wrong deployment.
func TestEphemeralDockerExecMintRejectsBaseMismatch(t *testing.T) {
	dir := t.TempDir()
	ephemeralDir := filepath.Join(dir, "ephemeral")
	_ = os.MkdirAll(ephemeralDir, 0o700)
	stubContainerMint(t, `{"handle":"max.e-abcd1234","agent_secret":"as_ag_x","identity_class":"ephemeral","base_url":"https://comment.io"}`, nil, nil, nil)
	if _, _, ok := ephemeralTryDockerExecMint("sess1", "", "https://comt.dev", ephemeralDir, filepath.Join(dir, "bind")); ok {
		t.Fatalf("ok = true, want false when the container minted against a different origin")
	}
}

// A malformed container response is rejected (caller falls back to anonymous).
func TestEphemeralDockerExecMintRejectsMalformed(t *testing.T) {
	dir := t.TempDir()
	ephemeralDir := filepath.Join(dir, "ephemeral")
	_ = os.MkdirAll(ephemeralDir, 0o700)
	stubContainerMint(t, `{"handle":"../escape","agent_secret":"nope"}`, nil, nil, nil)
	if _, _, ok := ephemeralTryDockerExecMint("sess1", "", "https://comt.dev", ephemeralDir, filepath.Join(dir, "bind")); ok {
		t.Fatalf("ok = true, want false for a malformed handle/secret")
	}
}

func TestEphemeralDockerExecMintAcceptsLegacyUnmarkedOutput(t *testing.T) {
	dir := t.TempDir()
	ephemeralDir := filepath.Join(dir, "ephemeral")
	_ = os.MkdirAll(ephemeralDir, 0o700)
	stubContainerMint(t, `{"handle":"max.e-abcd1234","agent_secret":"as_ag_x","base_url":"https://comt.dev"}`, nil, nil, nil)
	cred, _, ok := ephemeralTryDockerExecMint("sess1", "", "https://comt.dev", ephemeralDir, filepath.Join(dir, "bind"))
	if !ok {
		t.Fatalf("ok = false, want true for legacy unmarked container output")
	}
	if cred.IdentityClass != "ephemeral" {
		t.Fatalf("cred identity_class = %q, want ephemeral", cred.IdentityClass)
	}
}

func TestEphemeralDockerExecMintRejectsNonEphemeralMarker(t *testing.T) {
	dir := t.TempDir()
	ephemeralDir := filepath.Join(dir, "ephemeral")
	_ = os.MkdirAll(ephemeralDir, 0o700)
	stubContainerMint(t, `{"handle":"max.e-abcd1234","agent_secret":"as_ag_x","identity_class":"standard","base_url":"https://comt.dev"}`, nil, nil, nil)
	if _, _, ok := ephemeralTryDockerExecMint("sess1", "", "https://comt.dev", ephemeralDir, filepath.Join(dir, "bind")); ok {
		t.Fatalf("ok = true, want false for non-ephemeral container output")
	}
}

// End-to-end: no ark key, no host pairing — `comment ephemeral ensure` mints via
// the container (docker-exec seam), persists to the host store, and reuses on the
// next call without minting again.
func TestEphemeralEnsureDockerExecMintThenReuse(t *testing.T) {
	clearEphemeralEnv(t)
	home := t.TempDir()
	calls := 0
	prev := ephemeralContainerMint
	ephemeralContainerMint = func(context.Context, string, string, string, string) ([]byte, error) {
		calls++
		return []byte(`{"handle":"max.e-d0c00001","agent_secret":"as_ag_dock_secret","identity_class":"ephemeral","actor_id":"ai:max.e-d0c00001"}`), nil
	}
	t.Cleanup(func() { ephemeralContainerMint = prev })

	args := []string{"--home", home, "--base-url", "https://comt.dev", "--session", "sessd"}
	if err := runEphemeralEnsure(args); err != nil {
		t.Fatalf("docker-exec mint returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("container mint calls = %d, want 1", calls)
	}
	cred := filepath.Join(home, "ephemeral", "max.e-d0c00001.json")
	if _, err := os.Stat(cred); err != nil {
		t.Fatalf("cred not persisted to host store: %v", err)
	}
	// Second call reuses the host cred — no second container mint.
	if err := runEphemeralEnsure(args); err != nil {
		t.Fatalf("reuse returned error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("reuse re-minted: calls = %d, want 1", calls)
	}
}
