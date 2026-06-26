//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// findUninstallAction returns the first action with the given name, or nil.
func findUninstallAction(actions []uninstallAction, name string) *uninstallAction {
	for i := range actions {
		if actions[i].Name == name {
			return &actions[i]
		}
	}
	return nil
}

func TestUnpairDaemonForUninstallRevokesServerSide(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	var sawAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, r *http.Request) {
		calls++
		sawAuth = r.Header.Get("Authorization")
		if r.Method != http.MethodPost {
			t.Errorf("self-revoke method = %s, want POST", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_live",
		Token:    "ldt_live_secret",
		BaseURL:  server.URL,
		Label:    "Live Mac",
	}); err != nil {
		t.Fatal(err)
	}

	action := unpairDaemonForUninstall(context.Background(), paths)
	if action.Status != "removed" {
		t.Fatalf("status = %q, want removed (action: %+v)", action.Status, action)
	}
	if calls != 1 {
		t.Fatalf("self-revoke calls = %d, want 1", calls)
	}
	if sawAuth != "Bearer ldt_live_secret" {
		t.Fatalf("self-revoke Authorization = %q", sawAuth)
	}
	// A confirmed revoke must drop the now-dead local daemon token immediately
	// (not defer to home removal), so a later early-return can't leave the
	// computer looking locally paired against a revoked server record.
	if _, statErr := os.Stat(commentbus.DaemonAuthPath(paths)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-auth.json still exists after confirmed revoke, stat err = %v", statErr)
	}
}

func TestUnpairDaemonForUninstallCleansEnrolledProfiles(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	agentsDir := filepath.Join(paths.Home, "agents")
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// A journaled enrollment-installed profile holds an as_ token the confirmed
	// revoke just killed; uninstall must clean it now so a later re-pair isn't
	// wedged by a stale "already installed" profile.
	enrolledPath := filepath.Join(agentsDir, "max.enrolled.json")
	if err := os.WriteFile(enrolledPath, []byte(`{"handle":"max.enrolled","agent_secret":"as_enrolled_1","base_url":"https://comment.io"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := enrollJournalRecord(paths, enrollJournalEntry{
		EnrollmentID: "enr_aaaaaaaa-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222",
		Handle:       "max.enrolled",
		ProfilePath:  enrolledPath,
		SecretSHA256: enrollSecretSHA256("as_enrolled_1"),
	}); err != nil {
		t.Fatal(err)
	}
	// A removed profile triggers a daemon reload — stub it so the test does not
	// reach a real socket.
	stubAgentEnrollmentReload(t, &callRecorder{}, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_enr",
		Token:    "ldt_enr_secret",
		BaseURL:  server.URL,
		Label:    "Enroll Mac",
	}); err != nil {
		t.Fatal(err)
	}

	action := unpairDaemonForUninstall(context.Background(), paths)
	if action.Status != "removed" {
		t.Fatalf("status = %q, want removed (action: %+v)", action.Status, action)
	}
	if _, statErr := os.Stat(enrolledPath); !os.IsNotExist(statErr) {
		t.Fatalf("enrollment-installed profile still on disk after revoke, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(commentbus.DaemonAuthPath(paths)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-auth.json still exists after confirmed revoke, stat err = %v", statErr)
	}
}

func TestUnpairDaemonForUninstallSkipsWhenNotPaired(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	action := unpairDaemonForUninstall(context.Background(), paths)
	if action.Status != "skipped" {
		t.Fatalf("status = %q, want skipped (action: %+v)", action.Status, action)
	}
	if !strings.Contains(action.Message, "not paired") {
		t.Fatalf("message = %q, want a not-paired note", action.Message)
	}
}

func TestUnpairDaemonForUninstallWarnsOnRejectedToken(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "INVALID_DAEMON_TOKEN"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_stale",
		Token:    "ldt_stale_secret",
		BaseURL:  server.URL,
		Label:    "Stale Mac",
	}); err != nil {
		t.Fatal(err)
	}

	action := unpairDaemonForUninstall(context.Background(), paths)
	// A rejected token is ambiguous (already-revoked vs stale), so it must NOT
	// claim success — a warning that still lets uninstall proceed (result.OK).
	if action.Status != "warning" {
		t.Fatalf("status = %q, want warning (action: %+v)", action.Status, action)
	}
	if !strings.Contains(action.Message, "Paired computers") {
		t.Fatalf("message = %q, want a web-app revocation pointer", action.Message)
	}
	if strings.Contains(action.Message, "ldt_stale_secret") || strings.Contains(action.Error, "ldt_stale_secret") {
		t.Fatalf("action leaked the daemon token: %+v", action)
	}
	// Even on a rejected (ambiguous) token, the now-unusable local auth must be
	// dropped so a later early-return can't leave the machine looking paired and
	// block a clean re-pair (mirrors `bus unpair`).
	if _, statErr := os.Stat(commentbus.DaemonAuthPath(paths)); !os.IsNotExist(statErr) {
		t.Fatalf("daemon-auth.json still exists after rejected revoke, stat err = %v", statErr)
	}
}

func TestUnpairDaemonForUninstallDeletesUnreadableAuth(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	authPath := commentbus.DaemonAuthPath(paths)
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// Malformed JSON → LoadDaemonAuth errors. The broken file still reads as
	// "paired", so uninstall must drop it instead of leaving it to abort behind.
	if err := os.WriteFile(authPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	action := unpairDaemonForUninstall(context.Background(), paths)
	if action.Status != "warning" {
		t.Fatalf("status = %q, want warning (action: %+v)", action.Status, action)
	}
	if _, statErr := os.Stat(authPath); !os.IsNotExist(statErr) {
		t.Fatalf("unreadable daemon-auth.json still exists, stat err = %v", statErr)
	}
}

// A warning-status unpair must not flip the overall result to not-OK: an
// unreachable server can't block local teardown.
func TestUnpairDaemonWarningKeepsResultOK(t *testing.T) {
	result := &uninstallResult{OK: true}
	appendUninstallAction(result, uninstallAction{Name: "unpair_daemon", Status: "warning", Message: "could not reach the server"})
	if !result.OK {
		t.Fatal("a warning-status unpair flipped result.OK to false")
	}
	if a := findUninstallAction(result.Actions, "unpair_daemon"); a == nil {
		t.Fatal("unpair_daemon action not recorded")
	}
}

func TestUninstallPlanIncludesUnpairWhenPaired(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID: "ld_plan",
		Token:    "ldt_plan_secret",
		BaseURL:  "https://example.invalid",
		Label:    "Plan Mac",
	}); err != nil {
		t.Fatal(err)
	}
	result := &uninstallResult{OK: true, Home: home}
	addUninstallPlanActions(result, uninstallOptions{Home: home, NPM: "npm"})
	action := findUninstallAction(result.Actions, "unpair_daemon")
	if action == nil {
		t.Fatal("plan missing unpair_daemon action")
	}
	if action.Status != "planned" {
		t.Fatalf("plan unpair_daemon status = %q, want planned", action.Status)
	}
	// unpair must be planned BEFORE the home is removed (it needs the token).
	var unpairIdx, removeHomeIdx = -1, -1
	for i, a := range result.Actions {
		switch a.Name {
		case "unpair_daemon":
			unpairIdx = i
		case "remove_comment_home":
			removeHomeIdx = i
		}
	}
	if unpairIdx == -1 || removeHomeIdx == -1 || unpairIdx >= removeHomeIdx {
		t.Fatalf("unpair (idx %d) must precede remove_comment_home (idx %d)", unpairIdx, removeHomeIdx)
	}
}

// stubDockerCatDaemonAuth fakes the `docker run --entrypoint cat ... daemon-auth.json`
// read so the Docker revoke path can run without a real engine. Non-`run`
// docker calls and every other command succeed with "ok". Returns a pointer to
// the recorded docker `run` invocations.
func stubDockerCatDaemonAuth(t *testing.T, authJSON string, runErr error) *[]string {
	t.Helper()
	var runs []string
	old := uninstallCombinedOutput
	uninstallCombinedOutput = func(_ context.Context, command string, args ...string) ([]byte, error) {
		if filepath.Base(command) == "docker" && len(args) >= 1 && args[0] == "run" {
			runs = append(runs, command+" "+strings.Join(args, " "))
			if runErr != nil {
				return []byte("docker error\n"), runErr
			}
			return []byte(authJSON), nil
		}
		return []byte("ok\n"), nil
	}
	t.Cleanup(func() { uninstallCombinedOutput = old })
	return &runs
}

func TestRevokeDockerDaemonForUninstallRevokesServerSide(t *testing.T) {
	var calls int
	var sawAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, r *http.Request) {
		calls++
		sawAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	authJSON := `{"daemon_id":"ld_docker","daemon_token":"ldt_docker_secret","base_url":"` + server.URL + `"}`
	runs := stubDockerCatDaemonAuth(t, authJSON, nil)

	artifacts := dockerAgentArtifacts{
		Volumes: []string{"comment-agent-state-foo", "comment-agent-home-foo"},
		Images:  []string{"ghcr.io/comment-hq/comment-agent:latest"},
	}
	action := revokeDockerDaemonForUninstall(context.Background(), "/fake/docker", artifacts, "")
	if action.Status != "removed" {
		t.Fatalf("status = %q, want removed (action: %+v)", action.Status, action)
	}
	if calls != 1 {
		t.Fatalf("self-revoke calls = %d, want 1", calls)
	}
	if sawAuth != "Bearer ldt_docker_secret" {
		t.Fatalf("self-revoke Authorization = %q", sawAuth)
	}
	// Two docker runs: the --entrypoint cat read, then the --entrypoint sh clear
	// of the now-dead token (mirroring the host path's DeleteDaemonAuth).
	if len(*runs) != 2 {
		t.Fatalf("docker run calls = %d, want 2: %v", len(*runs), *runs)
	}
	read := (*runs)[0]
	for _, want := range []string{"--user 0", "--entrypoint sh", "comment-agent-state-foo:/state", "cat /state/bus/daemon-auth.json"} {
		if !strings.Contains(read, want) {
			t.Fatalf("docker read run = %q, missing %q", read, want)
		}
	}
	clear := (*runs)[1]
	for _, want := range []string{"--user 0", "--entrypoint sh", "comment-agent-state-foo:/state", "rm -rf /state/bus/daemon-auth.json", "/state/bus/enroll-journal.json", "/state/agents", "/state/botlets"} {
		if !strings.Contains(clear, want) {
			t.Fatalf("docker clear run = %q, missing %q", clear, want)
		}
	}
}

func TestRevokeDockerDaemonForUninstallSkipsWhenNeverPaired(t *testing.T) {
	// The read probe prints the sentinel when daemon-auth.json is absent: a clean
	// skip, not a false "could not revoke" alarm.
	stubDockerCatDaemonAuth(t, dockerNoDaemonAuthSentinel+"\n", nil)
	artifacts := dockerAgentArtifacts{Volumes: []string{"comment-agent-state-foo"}}
	action := revokeDockerDaemonForUninstall(context.Background(), "/fake/docker", artifacts, "")
	if action.Status != "skipped" {
		t.Fatalf("status = %q, want skipped (action: %+v)", action.Status, action)
	}
}

func TestRevokeDockerDaemonForUninstallWarnsOnMalformedAuth(t *testing.T) {
	// A present-but-unparseable file means a daemon that may still be live; the
	// volume is about to be deleted, so the user must be told to revoke manually.
	stubDockerCatDaemonAuth(t, "{not valid json", nil)
	artifacts := dockerAgentArtifacts{Volumes: []string{"comment-agent-state-foo"}}
	action := revokeDockerDaemonForUninstall(context.Background(), "/fake/docker", artifacts, "")
	if action.Status != "warning" {
		t.Fatalf("status = %q, want warning (action: %+v)", action.Status, action)
	}
	if !strings.Contains(action.Message, "Paired computers") {
		t.Fatalf("message = %q, want a web-app revocation pointer", action.Message)
	}
}

func TestRevokeDockerDaemonForUninstallClearsAuthOnRejectedToken(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": "INVALID_DAEMON_TOKEN"})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	authJSON := `{"daemon_id":"ld_d","daemon_token":"ldt_d","base_url":"` + server.URL + `"}`
	runs := stubDockerCatDaemonAuth(t, authJSON, nil)

	artifacts := dockerAgentArtifacts{Volumes: []string{"comment-agent-state-foo"}}
	action := revokeDockerDaemonForUninstall(context.Background(), "/fake/docker", artifacts, "")
	if action.Status != "warning" {
		t.Fatalf("status = %q, want warning (action: %+v)", action.Status, action)
	}
	// Read + a clear that drops ONLY the rejected token (profiles may still be
	// live), so a surviving volume can't look paired — but enrolled profiles stay.
	if len(*runs) != 2 {
		t.Fatalf("docker run calls = %d, want 2: %v", len(*runs), *runs)
	}
	clear := (*runs)[1]
	if !strings.Contains(clear, "/state/bus/daemon-auth.json") {
		t.Fatalf("clear run = %q, want it to drop daemon-auth.json", clear)
	}
	if strings.Contains(clear, "/state/agents") || strings.Contains(clear, "/state/botlets") {
		t.Fatalf("clear run = %q, must NOT delete profiles on an ambiguous token", clear)
	}
}

func TestRevokeDockerDaemonForUninstallSkipsWithoutStateVolume(t *testing.T) {
	artifacts := dockerAgentArtifacts{Volumes: []string{"comment-agent-home-foo"}}
	action := revokeDockerDaemonForUninstall(context.Background(), "/fake/docker", artifacts, "")
	if action.Status != "skipped" {
		t.Fatalf("status = %q, want skipped (action: %+v)", action.Status, action)
	}
}

func TestRevokeDockerDaemonForUninstallWarnsWhenVolumeUnreadable(t *testing.T) {
	stubDockerCatDaemonAuth(t, "", errors.New("exit status 1"))
	artifacts := dockerAgentArtifacts{Volumes: []string{"comment-agent-state-foo"}}
	action := revokeDockerDaemonForUninstall(context.Background(), "/fake/docker", artifacts, "")
	if action.Status != "warning" {
		t.Fatalf("status = %q, want warning (action: %+v)", action.Status, action)
	}
	if !strings.Contains(action.Message, "Paired computers") {
		t.Fatalf("message = %q, want a web-app revocation pointer", action.Message)
	}
}

func TestUninstallPlanWarnsWhenAuthUnreadable(t *testing.T) {
	home := testBusPairHome(t)
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	authPath := commentbus.DaemonAuthPath(paths)
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := &uninstallResult{OK: true, Home: home}
	addUninstallPlanActions(result, uninstallOptions{Home: home, NPM: "npm"})
	action := findUninstallAction(result.Actions, "unpair_daemon")
	if action == nil {
		t.Fatal("plan missing unpair_daemon action")
	}
	// Must NOT report "skipped/not paired" — the real run warns + deletes it.
	if action.Status != "planned" {
		t.Fatalf("status = %q, want planned (action: %+v)", action.Status, action)
	}
	if !strings.Contains(action.Message, "Paired computers") {
		t.Fatalf("message = %q, want a web-app revocation pointer", action.Message)
	}
}

func TestUninstallPlanSkipsUnpairWhenNotPaired(t *testing.T) {
	home := testBusPairHome(t)
	result := &uninstallResult{OK: true, Home: home}
	addUninstallPlanActions(result, uninstallOptions{Home: home, NPM: "npm"})
	action := findUninstallAction(result.Actions, "unpair_daemon")
	if action == nil {
		t.Fatal("plan missing unpair_daemon action")
	}
	if action.Status != "skipped" {
		t.Fatalf("plan unpair_daemon status = %q, want skipped", action.Status)
	}
}

// A staging install that FELL BACK to the production image at install time: when
// only the state volume remains (no container to read the actual image from) and
// the staging image isn't present/pullable, the revoke must try the production
// fallback image rather than warning and orphaning the server-side daemon. Pins
// that dockerDaemonReadImages returns the whole candidate list and the revoke loops
// past a failing candidate.
func TestRevokeDockerDaemonTriesProductionFallbackWhenStagingImageMissing(t *testing.T) {
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("COMMENT_IO_AGENT_IMAGE", "")
	t.Setenv("COMMENT_IO_ENV", "")
	t.Setenv("COMMENT_IO_BASE_URL", "")
	t.Setenv("COMMENT_IO_STAGING_BASE_URL", "")
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}

	var revokeCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/daemon/self-revoke", func(w http.ResponseWriter, _ *http.Request) {
		revokeCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	authJSON := `{"daemon_id":"ld_docker","daemon_token":"ldt_docker_secret","base_url":"` + server.URL + `"}`

	const stagingImg = "ghcr.io/comment-hq/comment-agent-staging:latest"
	const prodImg = "ghcr.io/comment-hq/comment-agent:latest"
	var ranProdImage bool
	old := uninstallCombinedOutput
	uninstallCombinedOutput = func(_ context.Context, command string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if filepath.Base(command) == "docker" && len(args) >= 1 && args[0] == "run" {
			// Staging home tries the staging image FIRST; it isn't present → fail, so
			// the loop must fall through to the production image (actually used).
			if strings.Contains(joined, stagingImg) {
				return []byte("Unable to find image '" + stagingImg + "' locally\n"), errors.New("exit status 125")
			}
			if strings.Contains(joined, prodImg) {
				ranProdImage = true
			}
			return []byte(authJSON), nil
		}
		return []byte("ok\n"), nil
	}
	t.Cleanup(func() { uninstallCombinedOutput = old })

	artifacts := dockerAgentArtifacts{Volumes: []string{"comment-agent-state-foo"}}
	action := revokeDockerDaemonForUninstall(context.Background(), "/fake/docker", artifacts, stagingHome)
	if action.Status != "removed" {
		t.Fatalf("status = %q, want removed (staging image missing → production fallback): %+v", action.Status, action)
	}
	if revokeCalls != 1 {
		t.Fatalf("self-revoke calls = %d, want 1", revokeCalls)
	}
	if !ranProdImage {
		t.Fatal("expected the revoke read/clear to run on the production fallback image after the staging image failed")
	}
}
