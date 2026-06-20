//go:build darwin || linux

package commentbus

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDaemonAuthSaveLoadRoundTrip(t *testing.T) {
	paths, err := ResolvePaths(filepath.Join(t.TempDir(), ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	auth := DaemonAuth{
		DaemonID:     "ld_11111111-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222",
		Token:        "ldt_ag_owner_ld_x_secret-token-value",
		BaseURL:      "https://comment.io",
		Label:        "Max's MacBook",
		Capabilities: []string{"agent_enrollment:v1"},
		PairedAt:     "2026-06-10T00:00:00Z",
	}
	if err := SaveDaemonAuth(paths, auth); err != nil {
		t.Fatal(err)
	}
	path := DaemonAuthPath(paths)
	if path != filepath.Join(paths.Home, "bus", DaemonAuthFileName) {
		t.Fatalf("daemon auth path = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("daemon auth mode = %v, want 0600", info.Mode().Perm())
	}
	busInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if busInfo.Mode().Perm() != 0o700 {
		t.Fatalf("bus dir mode = %v, want 0700", busInfo.Mode().Perm())
	}
	loaded, ok, err := LoadDaemonAuth(paths)
	if err != nil || !ok {
		t.Fatalf("LoadDaemonAuth = ok %v err %v", ok, err)
	}
	if loaded.DaemonID != auth.DaemonID || loaded.Token != auth.Token || loaded.BaseURL != auth.BaseURL ||
		loaded.Label != auth.Label || loaded.PairedAt != auth.PairedAt {
		t.Fatalf("loaded = %+v, want %+v", loaded, auth)
	}
	if len(loaded.Capabilities) != 1 || loaded.Capabilities[0] != "agent_enrollment:v1" {
		t.Fatalf("loaded capabilities = %#v", loaded.Capabilities)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["daemon_token"] != auth.Token {
		t.Fatalf("daemon-auth.json daemon_token field = %#v", raw["daemon_token"])
	}
}

func TestDaemonAuthLoadToleratesMissingFile(t *testing.T) {
	paths, err := ResolvePaths(filepath.Join(t.TempDir(), ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	auth, ok, err := LoadDaemonAuth(paths)
	if err != nil {
		t.Fatalf("LoadDaemonAuth err = %v, want nil", err)
	}
	if ok {
		t.Fatal("LoadDaemonAuth ok = true for missing file")
	}
	if auth.DaemonID != "" || auth.Token != "" {
		t.Fatalf("LoadDaemonAuth returned non-zero value: %+v", auth)
	}
}

func TestDaemonAuthLoadRejectsSymlinkAndBadContents(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Bus, 0o700); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "elsewhere.json")
	if err := os.WriteFile(real, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, DaemonAuthPath(paths)); err != nil {
		t.Skipf("symlink not available: %v", err)
	}
	if _, _, err := LoadDaemonAuth(paths); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked daemon auth err = %v, want symlink rejection", err)
	}
	if err := os.Remove(DaemonAuthPath(paths)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(DaemonAuthPath(paths), []byte(`{"daemon_id":"ld_x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadDaemonAuth(paths); err == nil {
		t.Fatal("daemon auth without token loaded without error")
	}
}

func TestDaemonAuthSaveRejectsSymlinkedBusDir(t *testing.T) {
	root := t.TempDir()
	paths, err := ResolvePaths(filepath.Join(root, ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	realBus := filepath.Join(root, "real-bus")
	if err := os.MkdirAll(realBus, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realBus, paths.Bus); err != nil {
		t.Skipf("symlink not available: %v", err)
	}
	err = SaveDaemonAuth(paths, DaemonAuth{DaemonID: "ld_x", Token: "ldt_x"})
	if err == nil || !strings.Contains(err.Error(), "bus directory") {
		t.Fatalf("SaveDaemonAuth err = %v, want bus directory trust failure", err)
	}
}

func TestDaemonAuthDeleteToleratesMissing(t *testing.T) {
	paths, err := ResolvePaths(filepath.Join(t.TempDir(), ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	if err := DeleteDaemonAuth(paths); err != nil {
		t.Fatalf("DeleteDaemonAuth on missing file err = %v", err)
	}
}

// Health must advertise the daemon_pairing feature bit and reflect the paired
// state from daemon-auth.json on every call, without ever exposing the token.
func TestDaemonHealthReportsPairingState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()

	health := requestDaemon(t, paths, map[string]any{"id": "req_health-pairing", "op": "health", "params": map[string]any{}})
	if !health.OK {
		t.Fatalf("health failed: %+v", health.Error)
	}
	result := health.Result.(map[string]any)
	features := result["features"].(map[string]any)
	if got, ok := features[FeatureDaemonPairing].(float64); !ok || got != float64(FeatureDaemonPairingVersion) {
		t.Fatalf("features[%s] = %#v, want %d", FeatureDaemonPairing, features[FeatureDaemonPairing], FeatureDaemonPairingVersion)
	}
	if got, ok := features[FeatureAgentEnrollment].(float64); !ok || got != float64(FeatureAgentEnrollmentVersion) {
		t.Fatalf("features[%s] = %#v, want %d", FeatureAgentEnrollment, features[FeatureAgentEnrollment], FeatureAgentEnrollmentVersion)
	}
	if result["daemon_paired"] != false {
		t.Fatalf("daemon_paired = %#v, want false", result["daemon_paired"])
	}
	if result["daemon_id"] != nil {
		t.Fatalf("daemon_id = %#v, want nil while unpaired", result["daemon_id"])
	}

	auth := DaemonAuth{
		DaemonID: "ld_33333333-3333-4333-8333-333333333333_44444444-4444-4444-8444-444444444444",
		Token:    "ldt_ag_owner_ld_x_health-secret-token",
		BaseURL:  "https://comment.io",
		Label:    "Health Test Mac",
		PairedAt: "2026-06-10T00:00:00Z",
	}
	if err := SaveDaemonAuth(paths, auth); err != nil {
		t.Fatal(err)
	}
	health = requestDaemon(t, paths, map[string]any{"id": "req_health-paired", "op": "health", "params": map[string]any{}})
	if !health.OK {
		t.Fatalf("health failed after pairing: %+v", health.Error)
	}
	result = health.Result.(map[string]any)
	if result["daemon_paired"] != true {
		t.Fatalf("daemon_paired = %#v, want true", result["daemon_paired"])
	}
	if result["daemon_id"] != auth.DaemonID {
		t.Fatalf("daemon_id = %#v, want %q", result["daemon_id"], auth.DaemonID)
	}
	if encoded := mustJSON(t, health); strings.Contains(encoded, auth.Token) {
		t.Fatalf("health response leaked the daemon token: %s", encoded)
	}
}
