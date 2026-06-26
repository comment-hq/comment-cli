//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

// stubUninstallDocker fakes the `docker` CLI for uninstall tests: `docker ps`
// returns the given container names each tab-joined with image (mirroring the
// "{{.Names}}\t{{.Image}}" format), `docker volume ls` returns the volume names,
// and every other docker invocation succeeds unless listed in fail (keyed by
// "<command> <args...>"). It returns a pointer to the recorded docker invocation
// strings.
func stubUninstallDocker(t *testing.T, containers []string, image string, volumes []string, fail map[string]bool) *[]string {
	t.Helper()
	var calls []string
	oldCombined := uninstallCombinedOutput
	oldLookPath := uninstallLookPath
	uninstallCombinedOutput = func(ctx context.Context, command string, args ...string) ([]byte, error) {
		joined := command + " " + strings.Join(args, " ")
		if filepath.Base(command) == "docker" {
			calls = append(calls, joined)
			if len(args) >= 1 && args[0] == "ps" {
				var lines []string
				for _, name := range containers {
					lines = append(lines, name+"\t"+image)
				}
				return []byte(strings.Join(lines, "\n") + "\n"), nil
			}
			if len(args) >= 2 && args[0] == "volume" && args[1] == "ls" {
				return []byte(strings.Join(volumes, "\n") + "\n"), nil
			}
			if fail != nil && fail[joined] {
				return []byte("docker error\n"), errors.New("exit status 1")
			}
			return []byte("ok\n"), nil
		}
		return []byte("ok\n"), nil
	}
	uninstallLookPath = func(command string) (string, error) {
		if command == "docker" {
			return "/fake/docker", nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() {
		uninstallCombinedOutput = oldCombined
		uninstallLookPath = oldLookPath
	})
	return &calls
}

func dockerUninstallHomes(t *testing.T) (home, botletsHome string) {
	t.Helper()
	userHome := t.TempDir()
	t.Setenv("HOME", userHome)
	t.Setenv("COMMENT_IO_AGENT_IMAGE", "")
	// Pin the active origin so the per-origin Docker resource slug is
	// deterministic ("comment.io" -> "comment-io"), independent of the shell's
	// ambient COMMENT_IO_* env.
	t.Setenv("COMMENT_IO_ENV", "")
	t.Setenv("COMMENT_IO_BASE_URL", "")
	t.Setenv("COMMENT_IO_STAGING_BASE_URL", "")
	home = filepath.Join(userHome, ".comment-io")
	botletsHome = filepath.Join(userHome, "botlets")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(botletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	return home, botletsHome
}

func writeDockerRuntimeMarkerForHome(t *testing.T, home string, target dockerRuntimeTarget) string {
	t.Helper()
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDockerRuntimeInstallMarker(paths, target); err != nil {
		t.Fatal(err)
	}
	return dockerRuntimeInstallMarkerPath(paths)
}

func writeInvalidDockerRuntimeMarkerForHome(t *testing.T, home string, content string, mode os.FileMode) string {
	t.Helper()
	paths, err := commentbus.ResolvePaths(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.Bus, 0o700); err != nil {
		t.Fatal(err)
	}
	path := dockerRuntimeInstallMarkerPath(paths)
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeDockerProjectedManifestForHome(t *testing.T, home string, files []string) string {
	t.Helper()
	return writeDockerProjectedManifestForDir(t, filepath.Join(home, "agents"), files)
}

func writeDockerProjectedManifestForDir(t *testing.T, agentsDir string, files []string) string {
	t.Helper()
	if err := os.MkdirAll(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(agentsDir, ".comment-agent-projected.manifest")
	content, err := json.Marshal(map[string][]string{"files": files})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(content, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	return manifestPath
}

func TestUninstallRemovesDockerAgentContainersAndVolumes(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	// comment-agent-example-comt-dev is ANOTHER origin's (staging) agent sharing this
	// Docker daemon; uninstalling the comment.io origin must leave it running.
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-comment-io", "comment-agent-example-comt-dev", "some-other-container"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io", "comment-agent-home-comment-io", "comment-agent-projected-comment-io", "comment-agent-state-example-comt-dev", "unrelated-volume"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != true {
		t.Fatalf("unexpected result = %#v", result)
	}

	containerAction, ok := findTestAction(result, "remove_docker_containers")
	if !ok || containerAction["status"] != "removed" {
		t.Fatalf("remove_docker_containers action = %#v", containerAction)
	}
	volumeAction, ok := findTestAction(result, "remove_docker_volumes")
	if !ok || volumeAction["status"] != "removed" {
		t.Fatalf("remove_docker_volumes action = %#v", volumeAction)
	}

	for _, want := range []string{
		"/fake/docker rm -f comment-agent-comment-io",
		"/fake/docker volume rm comment-agent-state-comment-io",
		"/fake/docker volume rm comment-agent-home-comment-io",
		"/fake/docker volume rm comment-agent-projected-comment-io",
	} {
		if !slices.Contains(*calls, want) {
			t.Fatalf("expected docker call %q in %#v", want, *calls)
		}
	}
	// Another origin's agent + volumes, and non-comment-agent resources, must be
	// left alone.
	for _, forbidden := range []string{
		"/fake/docker rm -f comment-agent-example-comt-dev",
		"/fake/docker volume rm comment-agent-state-example-comt-dev",
		"/fake/docker rm -f some-other-container",
		"/fake/docker volume rm unrelated-volume",
	} {
		if slices.Contains(*calls, forbidden) {
			t.Fatalf("must not run cross-origin/unrelated removal %q: %#v", forbidden, *calls)
		}
	}
	// The image must not be removed without --remove-image.
	for _, call := range *calls {
		if strings.HasPrefix(call, "/fake/docker rmi") {
			t.Fatalf("image should not be removed by default: %#v", *calls)
		}
	}
	if _, ok := findTestAction(result, "remove_docker_image"); ok {
		t.Fatalf("remove_docker_image action should be absent without --remove-image")
	}
}

func TestUninstallRemovesDockerImageWithFlag(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	// A custom image (e.g. installed via COMMENT_IO_AGENT_IMAGE): --remove-image
	// must drop the ref the container actually used, not the :latest default.
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-comment-io"},
		"ghcr.io/comment-hq/comment-agent:v1.2.3",
		[]string{"comment-agent-state-comment-io"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins", "--remove-image",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	imageAction, ok := findTestAction(result, "remove_docker_image")
	if !ok || imageAction["status"] != "removed" {
		t.Fatalf("remove_docker_image action = %#v", imageAction)
	}
	if !slices.Contains(*calls, "/fake/docker rmi ghcr.io/comment-hq/comment-agent:v1.2.3") {
		t.Fatalf("expected docker rmi of the container's image in %#v", *calls)
	}
	if slices.Contains(*calls, "/fake/docker rmi ghcr.io/comment-hq/comment-agent:latest") {
		t.Fatalf("should not remove the :latest default when a container pins another tag: %#v", *calls)
	}
}

func TestUninstallImageRemovalWarningStillRemovesRuntimeMarker(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	marker := writeDockerRuntimeMarkerForHome(t, home, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	})
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-comment-io"},
		"ghcr.io/comment-hq/comment-agent:v1.2.3",
		[]string{"comment-agent-state-comment-io"},
		map[string]bool{"/fake/docker rmi ghcr.io/comment-hq/comment-agent:v1.2.3": true},
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins", "--remove-image",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	imageAction, ok := findTestAction(result, "remove_docker_image")
	if !ok || imageAction["status"] != "warning" {
		t.Fatalf("remove_docker_image action = %#v", imageAction)
	}
	markerAction, ok := findTestAction(result, "remove_docker_runtime_markers")
	if !ok || markerAction["status"] != "removed" {
		t.Fatalf("remove_docker_runtime_markers action = %#v", markerAction)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime marker should be removed after container/volume cleanup despite image warning, stat err = %v", err)
	}
	if !slices.Contains(*calls, "/fake/docker rm -f comment-agent-comment-io") {
		t.Fatalf("expected container removal in %#v", *calls)
	}
	if !slices.Contains(*calls, "/fake/docker volume rm comment-agent-state-comment-io") {
		t.Fatalf("expected volume removal in %#v", *calls)
	}
}

func TestUninstallRemoveImageFallsBackToDefaultWhenNoContainers(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	// No containers (already removed) but the user still wants the image gone:
	// fall back to the configured/default ref.
	calls := stubUninstallDocker(t, nil, "", nil, nil)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins", "--remove-image",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	imageAction, ok := findTestAction(result, "remove_docker_image")
	if !ok || imageAction["status"] != "removed" {
		t.Fatalf("remove_docker_image action = %#v", imageAction)
	}
	if !slices.Contains(*calls, "/fake/docker rmi ghcr.io/comment-hq/comment-agent:latest") {
		t.Fatalf("expected docker rmi of the default image in %#v", *calls)
	}
}

// A staging home's --remove-image fallback (no container left to read the image
// from) must target the STAGING package, mirroring the install script's
// agentImageRefForBaseUrl. With no container metadata we can't be sure whether the
// staging install fell back to the production image, so BOTH safe defaults are
// removed — but the staging one (what a staging install pulls) must be among them.
func TestUninstallRemoveImageFallsBackToStagingImageForStagingHome(t *testing.T) {
	home, _ := dockerUninstallHomes(t) // process env = production
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	// No containers anywhere (already removed), but the user wants the image gone.
	calls := stubUninstallDocker(t, nil, "", nil, nil)

	output, err := captureRun(t, []string{
		"uninstall", "--home", stagingHome, "--botlets-home", filepath.Join(userHome, "botlets-staging"),
		"--yes", "--skip-cli", "--skip-plugins", "--remove-image",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	imageAction, ok := findTestAction(result, "remove_docker_image")
	if !ok || imageAction["status"] != "removed" {
		t.Fatalf("remove_docker_image action = %#v", imageAction)
	}
	if !slices.Contains(*calls, "/fake/docker rmi ghcr.io/comment-hq/comment-agent-staging:latest") {
		t.Fatalf("expected docker rmi of the staging image in %#v", *calls)
	}
	// Both safe defaults are removed when there's no container metadata (the staging
	// install may have fallen back to the production image).
	if !slices.Contains(*calls, "/fake/docker rmi ghcr.io/comment-hq/comment-agent:latest") {
		t.Fatalf("expected docker rmi of the production fallback image too in %#v", *calls)
	}
}

func TestUninstallSkipDockerLeavesContainers(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-comment-io"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins", "--skip-docker",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	action, ok := findTestAction(result, "remove_docker_containers")
	if !ok || action["status"] != "skipped" {
		t.Fatalf("remove_docker_containers action = %#v", action)
	}
	if len(*calls) != 0 {
		t.Fatalf("--skip-docker should not invoke docker: %#v", *calls)
	}
}

func TestUninstallDockerNotInstalledSkipsGracefully(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	oldLookPath := uninstallLookPath
	uninstallLookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { uninstallLookPath = oldLookPath })

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall must succeed when docker is absent: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != true {
		t.Fatalf("unexpected result = %#v", result)
	}
	action, ok := findTestAction(result, "remove_docker_containers")
	if !ok || action["status"] != "skipped" {
		t.Fatalf("remove_docker_containers action = %#v", action)
	}
}

func TestUninstallDockerRemovalFailureIsWarningNotError(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	stubUninstallDocker(t,
		[]string{"comment-agent-comment-io"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io"},
		map[string]bool{"/fake/docker rm -f comment-agent-comment-io": true},
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall must not fail on a docker warning: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != true {
		t.Fatalf("ok should remain true on docker warning: %#v", result)
	}
	action, ok := findTestAction(result, "remove_docker_containers")
	if !ok || action["status"] != "warning" {
		t.Fatalf("remove_docker_containers action = %#v", action)
	}
}

func TestUninstallDryRunListsDockerArtifacts(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	stubUninstallDocker(t,
		[]string{"comment-agent-comment-io"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome, "--dry-run",
	})
	if err != nil {
		t.Fatalf("uninstall dry-run failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != true || result["dry_run"] != true {
		t.Fatalf("unexpected dry-run result = %#v", result)
	}
	action, ok := findTestAction(result, "remove_docker_containers")
	if !ok || action["status"] != "planned" {
		t.Fatalf("remove_docker_containers plan action = %#v", action)
	}
	volumeAction, ok := findTestAction(result, "remove_docker_volumes")
	if !ok || volumeAction["status"] != "planned" {
		t.Fatalf("remove_docker_volumes plan action = %#v", volumeAction)
	}
}

func TestUninstallDryRunListsProjectedProfileCleanup(t *testing.T) {
	home, _ := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	stagingBotletsHome := filepath.Join(userHome, "botlets-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stagingBotletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	target := dockerRuntimeTarget{
		BaseURL:   "https://example.comt.dev",
		Container: "comment-agent-example-comt-dev",
	}
	writeDockerRuntimeMarkerForHome(t, home, target)
	writeDockerRuntimeMarkerForHome(t, stagingHome, target)
	manifest := writeDockerProjectedManifestForHome(t, home, []string{"max.agent.json", "prod.agent.json"})
	maxProfile := filepath.Join(home, "agents", "max.agent.json")
	prodProfile := filepath.Join(home, "agents", "prod.agent.json")
	if err := os.WriteFile(maxProfile, []byte(`{"handle":"max.agent","base_url":"https://example.comt.dev"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prodProfile, []byte(`{"handle":"prod.agent","base_url":"https://comment.io"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stubUninstallDocker(t,
		[]string{"comment-agent-example-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-example-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", stagingHome, "--botlets-home", stagingBotletsHome, "--dry-run",
	})
	if err != nil {
		t.Fatalf("uninstall dry-run failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	action, ok := findTestAction(result, "remove_docker_projected_profiles")
	if !ok || action["status"] != "planned" {
		t.Fatalf("remove_docker_projected_profiles action = %#v", action)
	}
	paths, ok := action["paths"].([]any)
	if !ok || !slices.Contains(paths, any(maxProfile)) {
		t.Fatalf("remove_docker_projected_profiles paths = %#v, want %s", action["paths"], maxProfile)
	}
	detail, ok := action["detail"].(map[string]any)
	if !ok {
		t.Fatalf("remove_docker_projected_profiles detail = %#v", action["detail"])
	}
	rewrites, ok := detail["rewrite_manifests"].([]any)
	if !ok || !slices.Contains(rewrites, any(manifest)) {
		t.Fatalf("rewrite_manifests = %#v, want %s", detail["rewrite_manifests"], manifest)
	}
	for _, path := range []string{manifest, maxProfile, prodProfile} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("dry-run should not remove %s, stat err = %v\n%s", path, err, output)
		}
	}
}

func TestUninstallDryRunSkipDockerLeavesRuntimeMarkersPlannedSkipped(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	writeDockerRuntimeMarkerForHome(t, home, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	})
	oldLookPath := uninstallLookPath
	uninstallLookPath = func(command string) (string, error) {
		if command == "docker" {
			t.Fatal("dry-run --skip-docker should not inspect Docker")
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() { uninstallLookPath = oldLookPath })

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome, "--dry-run", "--skip-docker",
	})
	if err != nil {
		t.Fatalf("uninstall dry-run failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	action, ok := findTestAction(result, "remove_docker_runtime_markers")
	if !ok || action["status"] != "skipped" {
		t.Fatalf("remove_docker_runtime_markers action = %#v", action)
	}
}

func TestUninstallDryRunInvalidSelectedMarkerPreservesHomePlannedSkipped(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	writeInvalidDockerRuntimeMarkerForHome(t, home, "{not-json", 0o600)
	stubUninstallDocker(t,
		[]string{"comment-agent-comment-io"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome, "--dry-run",
	})
	if err != nil {
		t.Fatalf("uninstall dry-run failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	markerAction, ok := findTestAction(result, "remove_docker_runtime_markers")
	if !ok || markerAction["status"] != "warning" {
		t.Fatalf("remove_docker_runtime_markers action = %#v", markerAction)
	}
	homeAction, ok := findTestAction(result, "remove_comment_home")
	if !ok || homeAction["status"] != "skipped" {
		t.Fatalf("remove_comment_home action = %#v", homeAction)
	}
}

func TestUninstallDryRunDockerUnavailableWithSelectedMarkerPreservesHomePlannedSkipped(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	writeDockerRuntimeMarkerForHome(t, home, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	})
	oldLookPath := uninstallLookPath
	uninstallLookPath = func(string) (string, error) { return "", errors.New("not found") }
	t.Cleanup(func() { uninstallLookPath = oldLookPath })

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome, "--dry-run",
	})
	if err != nil {
		t.Fatalf("uninstall dry-run failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	markerAction, ok := findTestAction(result, "remove_docker_runtime_markers")
	if !ok || markerAction["status"] != "planned" {
		t.Fatalf("remove_docker_runtime_markers action = %#v", markerAction)
	}
	homeAction, ok := findTestAction(result, "remove_comment_home")
	if !ok || homeAction["status"] != "skipped" {
		t.Fatalf("remove_comment_home action = %#v", homeAction)
	}
}

func TestUninstallRemovesSingleNonDefaultOriginAgent(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t) // active env slug = "comment-io"
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	// The only agent present was installed from a personal-staging origin whose
	// slug ("example-comt-dev") does not match the active env default. With exactly one
	// agent present, uninstall must still remove it (slug read from the container).
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-example-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-example-comt-dev", "comment-agent-home-example-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	action, ok := findTestAction(result, "remove_docker_containers")
	if !ok || action["status"] != "removed" {
		t.Fatalf("remove_docker_containers action = %#v", action)
	}
	for _, want := range []string{
		"/fake/docker rm -f comment-agent-example-comt-dev",
		"/fake/docker volume rm comment-agent-state-example-comt-dev",
		"/fake/docker volume rm comment-agent-home-example-comt-dev",
	} {
		if !slices.Contains(*calls, want) {
			t.Fatalf("expected docker call %q in %#v", want, *calls)
		}
	}
}

func TestUninstallRevokesSoleNonDefaultOriginBeforeRemovingStateVolume(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-example-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-example-comt-dev", "comment-agent-home-example-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	revokeIndex := slices.IndexFunc(*calls, func(call string) bool {
		return strings.Contains(call, "docker run --rm --user 0 --entrypoint sh -v comment-agent-state-example-comt-dev:/state")
	})
	removeIndex := slices.Index(*calls, "/fake/docker volume rm comment-agent-state-example-comt-dev")
	if revokeIndex < 0 {
		t.Fatalf("expected Docker daemon revoke probe for non-default state volume in %#v", *calls)
	}
	if removeIndex < 0 {
		t.Fatalf("expected state volume removal in %#v", *calls)
	}
	if revokeIndex > removeIndex {
		t.Fatalf("state volume was removed before Docker daemon revoke probe: %#v", *calls)
	}
}

func TestUninstallUnrelatedCorruptFallbackMarkerDoesNotBlockProductionDockerCleanup(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	writeInvalidDockerRuntimeMarkerForHome(t, stagingHome, "{not-json", 0o600)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-comment-io"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	action, ok := findTestAction(result, "remove_docker_containers")
	if !ok || action["status"] != "removed" {
		t.Fatalf("remove_docker_containers action = %#v\n%s", action, output)
	}
	if !slices.Contains(*calls, "/fake/docker rm -f comment-agent-comment-io") {
		t.Fatalf("expected production container removal in %#v", *calls)
	}
}

func TestUninstallAmbiguousMultiOriginLeavesAllInPlace(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t) // active env slug = "comment-io"
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	// Two origins' agents share the daemon and neither matches the active env, so
	// uninstall cannot attribute one to this environment: remove nothing, report
	// both as left in place.
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-foo", "comment-agent-bar"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-foo", "comment-agent-state-bar"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall must not fail on ambiguous Docker state: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	if result["ok"] != true {
		t.Fatalf("ok should remain true: %#v", result)
	}
	containerAction, ok := findTestAction(result, "remove_docker_containers")
	if !ok || containerAction["status"] != "skipped" {
		t.Fatalf("remove_docker_containers action = %#v", containerAction)
	}
	other, ok := findTestAction(result, "docker_other_origins")
	if !ok || other["status"] != "skipped" {
		t.Fatalf("docker_other_origins action = %#v", other)
	}
	// The leftover credential-bearing volumes must be disclosed, not hidden.
	paths, _ := other["paths"].([]any)
	for _, want := range []string{"comment-agent-state-foo", "comment-agent-state-bar", "comment-agent-foo", "comment-agent-bar"} {
		if !slices.ContainsFunc(paths, func(p any) bool { return p == want }) {
			t.Fatalf("docker_other_origins must disclose %q; paths = %#v", want, paths)
		}
	}
	for _, call := range *calls {
		if strings.Contains(call, "docker rm -f") || strings.Contains(call, "docker volume rm") {
			t.Fatalf("ambiguous multi-origin state must remove nothing: %#v", *calls)
		}
	}
}

func TestUninstallPrefersActiveOriginVolumesOverForeignSingleton(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t) // active env slug = "comment-io"
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	// The active origin's container was already removed but its credential volumes
	// remain; another origin owns the only surviving container. Uninstall must
	// finish the active origin (its volumes) and NOT touch the foreign container.
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-foo"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io", "comment-agent-home-comment-io", "comment-agent-state-foo"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	volumeAction, ok := findTestAction(result, "remove_docker_volumes")
	if !ok || volumeAction["status"] != "removed" {
		t.Fatalf("remove_docker_volumes action = %#v", volumeAction)
	}
	for _, want := range []string{
		"/fake/docker volume rm comment-agent-state-comment-io",
		"/fake/docker volume rm comment-agent-home-comment-io",
	} {
		if !slices.Contains(*calls, want) {
			t.Fatalf("expected active-origin volume removal %q in %#v", want, *calls)
		}
	}
	// The foreign origin's container and volume must be left alone and reported.
	for _, forbidden := range []string{
		"/fake/docker rm -f comment-agent-foo",
		"/fake/docker volume rm comment-agent-state-foo",
	} {
		if slices.Contains(*calls, forbidden) {
			t.Fatalf("must not touch the foreign origin %q: %#v", forbidden, *calls)
		}
	}
	if other, ok := findTestAction(result, "docker_other_origins"); !ok || other["status"] != "skipped" {
		t.Fatalf("docker_other_origins action = %#v", other)
	}
}

func TestUninstallConfirmPromptDisclosesDocker(t *testing.T) {
	with := uninstallConfirmPrompt("/h/.comment-io", "/h/botlets", true)
	if !strings.Contains(with, "Docker") {
		t.Fatalf("interactive confirmation must disclose Docker removal: %q", with)
	}
	without := uninstallConfirmPrompt("/h/.comment-io", "/h/botlets", false)
	if strings.Contains(without, "Docker") {
		t.Fatalf("--skip-docker confirmation must not mention Docker removal: %q", without)
	}
}

func TestUninstallHomeFlagScopesDockerToThatEnvironment(t *testing.T) {
	home, _ := dockerUninstallHomes(t) // process env = production (slug "comment-io")
	userHome := filepath.Dir(home)     // dockerUninstallHomes put home at $HOME/.comment-io
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	// Both origins' agents share the daemon. Uninstalling the STAGING home (via
	// --home, without --staging) must remove the staging origin (comt.dev →
	// comt-dev), NOT production — even though the process env is production.
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-comment-io", "comment-agent-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io", "comment-agent-state-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", stagingHome, "--botlets-home", filepath.Join(userHome, "botlets-staging"),
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	if !slices.Contains(*calls, "/fake/docker rm -f comment-agent-comt-dev") {
		t.Fatalf("expected staging container removal in %#v", *calls)
	}
	if slices.Contains(*calls, "/fake/docker rm -f comment-agent-comment-io") {
		t.Fatalf("must NOT remove the production container when uninstalling the staging home: %#v", *calls)
	}
	if slices.Contains(*calls, "/fake/docker volume rm comment-agent-state-comment-io") {
		t.Fatalf("must NOT remove production volumes when uninstalling the staging home: %#v", *calls)
	}
}

func TestUninstallStagingFallbackMarkerIgnoresProductionBaseOverride(t *testing.T) {
	home, _ := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_BASE_URL", "https://example.comt.dev")
	t.Setenv("COMMENT_IO_STAGING_BASE_URL", "https://comt.dev")
	writeDockerRuntimeMarkerForHome(t, home, dockerRuntimeTarget{
		BaseURL:   "https://example.comt.dev",
		Container: "comment-agent-example-comt-dev",
	})
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-comt-dev", "comment-agent-example-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comt-dev", "comment-agent-state-example-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", stagingHome, "--botlets-home", filepath.Join(userHome, "botlets-staging"),
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	if !slices.Contains(*calls, "/fake/docker rm -f comment-agent-example-comt-dev") {
		t.Fatalf("expected personal-staging marker target removal in %#v", *calls)
	}
	if slices.Contains(*calls, "/fake/docker rm -f comment-agent-comt-dev") {
		t.Fatalf("must not fall back to the staging env slug when a personal-staging marker exists: %#v", *calls)
	}
}

func TestUninstallStagingFallbackMarkerDoesNotTreatProductionAsPersonalStagingUnderBaseOverride(t *testing.T) {
	home, _ := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_BASE_URL", "https://example.comt.dev")
	writeDockerRuntimeMarkerForHome(t, home, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	})
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-comment-io", "comment-agent-other-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io", "comment-agent-state-other-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", stagingHome, "--botlets-home", filepath.Join(userHome, "botlets-staging"),
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	if slices.Contains(*calls, "/fake/docker rm -f comment-agent-comment-io") {
		t.Fatalf("must not select a production marker as the staging fallback under COMMENT_IO_BASE_URL: %#v", *calls)
	}
}

func TestUninstallStagingFallbackMarkerDoesNotTreatProductionAliasAsPersonalStaging(t *testing.T) {
	home, _ := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("COMMENT_IO_BASE_URL", "https://example.comt.dev")
	writeDockerRuntimeMarkerForHome(t, home, dockerRuntimeTarget{
		BaseURL:   "https://www.comment.io",
		Container: "comment-agent-www-comment-io",
	})
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-www-comment-io", "comment-agent-other-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-www-comment-io", "comment-agent-state-other-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", stagingHome, "--botlets-home", filepath.Join(userHome, "botlets-staging"),
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	if slices.Contains(*calls, "/fake/docker rm -f comment-agent-www-comment-io") {
		t.Fatalf("must not select a production alias marker as the staging fallback: %#v", *calls)
	}
}

func TestUninstallRemovesMatchingDockerRuntimeMarkersFromHybridHomes(t *testing.T) {
	home, _ := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	target := dockerRuntimeTarget{
		BaseURL:   "https://example.comt.dev",
		Container: "comment-agent-example-comt-dev",
	}
	defaultMarker := writeDockerRuntimeMarkerForHome(t, home, target)
	stagingMarker := writeDockerRuntimeMarkerForHome(t, stagingHome, target)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	stubUninstallDocker(t,
		[]string{"comment-agent-example-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-example-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", stagingHome, "--botlets-home", filepath.Join(userHome, "botlets-staging"),
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	action, ok := findTestAction(result, "remove_docker_runtime_markers")
	if !ok || action["status"] != "removed" {
		t.Fatalf("remove_docker_runtime_markers action = %#v", action)
	}
	if _, err := os.Stat(defaultMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default-home marker should be removed, stat err = %v", err)
	}
	if _, err := os.Stat(stagingMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected-home marker should be removed, stat err = %v", err)
	}
}

func TestUninstallStagingHomeRemovesMatchingDefaultProjectedProfiles(t *testing.T) {
	home, _ := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	target := dockerRuntimeTarget{
		BaseURL:   "https://example.comt.dev",
		Container: "comment-agent-example-comt-dev",
	}
	defaultMarker := writeDockerRuntimeMarkerForHome(t, home, target)
	stagingMarker := writeDockerRuntimeMarkerForHome(t, stagingHome, target)
	manifest := writeDockerProjectedManifestForHome(t, home, []string{"max.agent.json"})
	projectedProfile := filepath.Join(home, "agents", "max.agent.json")
	nativeProfile := filepath.Join(home, "agents", "native.agent.json")
	if err := os.WriteFile(projectedProfile, []byte(`{"handle":"max.agent","base_url":"https://example.comt.dev"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nativeProfile, []byte(`{"handle":"native.agent"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	stubUninstallDocker(t,
		[]string{"comment-agent-example-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-example-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", stagingHome, "--botlets-home", filepath.Join(userHome, "botlets-staging"),
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	action, ok := findTestAction(result, "remove_docker_projected_profiles")
	if !ok || action["status"] != "removed" {
		t.Fatalf("remove_docker_projected_profiles action = %#v", action)
	}
	for _, path := range []string{defaultMarker, stagingMarker, manifest, projectedProfile} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should be removed, stat err = %v\n%s", path, err, output)
		}
	}
	if _, err := os.Stat(nativeProfile); err != nil {
		t.Fatalf("native host profile should remain, stat err = %v", err)
	}
}

func TestUninstallRemovesCustomProjectedProfilesFromRuntimeMarker(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	customAgentsDir := filepath.Join(userHome, "custom agents")
	target := dockerRuntimeTarget{
		BaseURL:            "https://comment.io",
		Container:          "comment-agent-comment-io",
		ProjectedAgentsDir: customAgentsDir,
	}
	marker := writeDockerRuntimeMarkerForHome(t, home, target)
	manifest := writeDockerProjectedManifestForDir(t, customAgentsDir, []string{"max.agent.json"})
	projectedProfile := filepath.Join(customAgentsDir, "max.agent.json")
	if err := os.WriteFile(projectedProfile, []byte(`{"handle":"max.agent","base_url":"https://comment.io"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	stubUninstallDocker(t,
		[]string{"comment-agent-comment-io"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	action, ok := findTestAction(result, "remove_docker_projected_profiles")
	if !ok || action["status"] != "removed" {
		t.Fatalf("remove_docker_projected_profiles action = %#v", action)
	}
	for _, path := range []string{marker, manifest, projectedProfile} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s should be removed, stat err = %v\n%s", path, err, output)
		}
	}
}

func TestUninstallProjectedProfileCleanupKeepsOtherOrigins(t *testing.T) {
	home, _ := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	stagingBotletsHome := filepath.Join(userHome, "botlets-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stagingBotletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	target := dockerRuntimeTarget{
		BaseURL:   "https://example.comt.dev",
		Container: "comment-agent-example-comt-dev",
	}
	marker := writeDockerRuntimeMarkerForHome(t, stagingHome, target)
	defaultMarker := writeDockerRuntimeMarkerForHome(t, home, target)
	manifest := writeDockerProjectedManifestForHome(t, home, []string{"max.agent.json", "prod.agent.json"})
	maxProfile := filepath.Join(home, "agents", "max.agent.json")
	prodProfile := filepath.Join(home, "agents", "prod.agent.json")
	if err := os.WriteFile(maxProfile, []byte(`{"handle":"max.agent","base_url":"https://example.comt.dev"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prodProfile, []byte(`{"handle":"prod.agent","base_url":"https://comment.io"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	stubUninstallDocker(t,
		[]string{"comment-agent-example-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-example-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", stagingHome, "--botlets-home", stagingBotletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	action, ok := findTestAction(result, "remove_docker_projected_profiles")
	if !ok || action["status"] != "removed" {
		t.Fatalf("remove_docker_projected_profiles action = %#v", action)
	}
	if _, err := os.Stat(maxProfile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target profile should be removed, stat err = %v\n%s", err, output)
	}
	if _, err := os.Stat(prodProfile); err != nil {
		t.Fatalf("foreign profile should remain, stat err = %v\n%s", err, output)
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatalf("manifest should remain for foreign origin, err = %v\n%s", err, output)
	}
	if !strings.Contains(string(data), "prod.agent.json") || strings.Contains(string(data), "max.agent.json") {
		t.Fatalf("manifest = %s, want only foreign profile", data)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("selected runtime marker should be removed after scoped profile cleanup, stat err = %v\n%s", err, output)
	}
	if _, err := os.Stat(defaultMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default runtime marker should be removed after scoped profile cleanup, stat err = %v\n%s", err, output)
	}
}

func TestUninstallProjectedProfileWarningLeavesRuntimeMarkersForRetry(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	marker := writeDockerRuntimeMarkerForHome(t, home, dockerRuntimeTarget{
		BaseURL:   "https://comment.io",
		Container: "comment-agent-comment-io",
	})
	manifest := writeDockerProjectedManifestForHome(t, home, []string{"../escape.json"})
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	stubUninstallDocker(t,
		[]string{"comment-agent-comment-io"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	result := decodeTestJSONMap(t, output)
	projectedAction, ok := findTestAction(result, "remove_docker_projected_profiles")
	if !ok || projectedAction["status"] != "warning" {
		t.Fatalf("remove_docker_projected_profiles action = %#v", projectedAction)
	}
	markerAction, ok := findTestAction(result, "remove_docker_runtime_markers")
	if !ok || markerAction["status"] != "skipped" {
		t.Fatalf("remove_docker_runtime_markers action = %#v", markerAction)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("runtime marker should remain for retry, stat err = %v\n%s", err, output)
	}
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("projected manifest should remain for retry, stat err = %v\n%s", err, output)
	}
}

func TestUninstallLeavesForeignDockerRuntimeMarkerInDefaultHome(t *testing.T) {
	home, _ := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	foreignMarker := writeDockerRuntimeMarkerForHome(t, home, dockerRuntimeTarget{
		BaseURL:   "https://other.example",
		Container: "comment-agent-other-example",
	})
	writeDockerRuntimeMarkerForHome(t, stagingHome, dockerRuntimeTarget{
		BaseURL:   "https://example.comt.dev",
		Container: "comment-agent-example-comt-dev",
	})
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	stubUninstallDocker(t,
		[]string{"comment-agent-example-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-example-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", stagingHome, "--botlets-home", filepath.Join(userHome, "botlets-staging"),
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(foreignMarker); err != nil {
		t.Fatalf("foreign default-home marker should remain, stat err = %v\n%s", err, output)
	}
}

func TestUninstallDefaultHomeMarkerScopesDockerToPersonalStaging(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	target := dockerRuntimeTarget{
		BaseURL:   "https://example.comt.dev",
		Container: "comment-agent-example-comt-dev",
	}
	writeDockerRuntimeMarkerForHome(t, home, target)
	stagingMarker := writeDockerRuntimeMarkerForHome(t, stagingHome, target)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-comment-io", "comment-agent-example-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io", "comment-agent-state-example-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	if !slices.Contains(*calls, "/fake/docker rm -f comment-agent-example-comt-dev") {
		t.Fatalf("expected personal-staging container removal from marker target in %#v", *calls)
	}
	if slices.Contains(*calls, "/fake/docker rm -f comment-agent-comment-io") {
		t.Fatalf("must not remove production container when marker targets personal staging: %#v", *calls)
	}
	if slices.Contains(*calls, "/fake/docker volume rm comment-agent-state-comment-io") {
		t.Fatalf("must not remove production volume when marker targets personal staging: %#v", *calls)
	}
	if _, err := os.Stat(stagingMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staging marker should be removed by plain uninstall, stat err = %v", err)
	}
}

func TestUninstallTrustedMarkerDoesNotRemoveSoleForeignDockerOrigin(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	marker := writeDockerRuntimeMarkerForHome(t, home, dockerRuntimeTarget{
		BaseURL:   "https://example.comt.dev",
		Container: "comment-agent-example-comt-dev",
	})
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-comment-io"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	if slices.Contains(*calls, "/fake/docker rm -f comment-agent-comment-io") {
		t.Fatalf("trusted marker target is absent; must not remove sole foreign origin: %#v", *calls)
	}
	if slices.Contains(*calls, "/fake/docker volume rm comment-agent-state-comment-io") {
		t.Fatalf("trusted marker target is absent; must not remove sole foreign volume: %#v", *calls)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale marker for absent trusted target should be removed after successful Docker inspection, stat err = %v", err)
	}
}

func TestUninstallSkipDockerLeavesCrossHomeRuntimeMarker(t *testing.T) {
	home, _ := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	target := dockerRuntimeTarget{
		BaseURL:   "https://example.comt.dev",
		Container: "comment-agent-example-comt-dev",
	}
	defaultMarker := writeDockerRuntimeMarkerForHome(t, home, target)
	stagingMarker := writeDockerRuntimeMarkerForHome(t, stagingHome, target)
	stagingBotletsHome := filepath.Join(userHome, "botlets-staging")
	if err := os.MkdirAll(stagingBotletsHome, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := installFakeLaunchctl(t)
	fake.loaded = true

	output, err := captureRun(t, []string{
		"uninstall", "--home", stagingHome, "--botlets-home", stagingBotletsHome,
		"--yes", "--skip-cli", "--skip-plugins", "--skip-docker",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(defaultMarker); err != nil {
		t.Fatalf("default-home marker should remain when --skip-docker is set, stat err = %v\n%s", err, output)
	}
	if _, err := os.Stat(stagingMarker); err != nil {
		t.Fatalf("selected-home marker should remain when --skip-docker is set, stat err = %v\n%s", err, output)
	}
	if _, err := os.Stat(stagingBotletsHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("separate botlets home should be removed even when comment home is preserved, stat err = %v\n%s", err, output)
	}
}

func TestUninstallDockerQueryFailureLeavesCrossHomeRuntimeMarker(t *testing.T) {
	home, _ := dockerUninstallHomes(t)
	userHome := filepath.Dir(home)
	stagingHome := filepath.Join(userHome, ".comment-io-staging")
	if err := os.MkdirAll(stagingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	target := dockerRuntimeTarget{
		BaseURL:   "https://example.comt.dev",
		Container: "comment-agent-example-comt-dev",
	}
	defaultMarker := writeDockerRuntimeMarkerForHome(t, home, target)
	stagingMarker := writeDockerRuntimeMarkerForHome(t, stagingHome, target)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	oldCombined := uninstallCombinedOutput
	oldLookPath := uninstallLookPath
	uninstallCombinedOutput = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("docker unavailable\n"), errors.New("exit status 1")
	}
	uninstallLookPath = func(command string) (string, error) {
		if command == "docker" {
			return "/fake/docker", nil
		}
		return "", errors.New("not found")
	}
	t.Cleanup(func() {
		uninstallCombinedOutput = oldCombined
		uninstallLookPath = oldLookPath
	})

	output, err := captureRun(t, []string{
		"uninstall", "--home", stagingHome, "--botlets-home", filepath.Join(userHome, "botlets-staging"),
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(defaultMarker); err != nil {
		t.Fatalf("default-home marker should remain after Docker query failure, stat err = %v\n%s", err, output)
	}
	if _, err := os.Stat(stagingMarker); err != nil {
		t.Fatalf("selected-home marker should remain after Docker query failure, stat err = %v\n%s", err, output)
	}
}

func TestUninstallInvalidDockerRuntimeMarkerDoesNotSelectEnvDefaultDocker(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	writeInvalidDockerRuntimeMarkerForHome(t, home, "{not-json", 0o600)
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-comment-io", "comment-agent-example-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io", "comment-agent-state-example-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	action, ok := findTestAction(decodeTestJSONMap(t, output), "remove_docker_containers")
	if !ok || action["status"] != "warning" {
		t.Fatalf("remove_docker_containers action = %#v", action)
	}
	for _, call := range *calls {
		if strings.Contains(call, "docker rm -f") || strings.Contains(call, "docker volume rm") {
			t.Fatalf("unsafe marker must not fall back to env default Docker teardown: %#v", *calls)
		}
	}
}

func TestUninstallPermissiveDockerRuntimeMarkerDoesNotSelectEnvDefaultDocker(t *testing.T) {
	home, botletsHome := dockerUninstallHomes(t)
	path := writeDockerRuntimeMarkerForHome(t, home, dockerRuntimeTarget{
		BaseURL:   "https://example.comt.dev",
		Container: "comment-agent-example-comt-dev",
	})
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	fake := installFakeLaunchctl(t)
	fake.loaded = true
	calls := stubUninstallDocker(t,
		[]string{"comment-agent-comment-io", "comment-agent-example-comt-dev"},
		"ghcr.io/comment-hq/comment-agent:latest",
		[]string{"comment-agent-state-comment-io", "comment-agent-state-example-comt-dev"},
		nil,
	)

	output, err := captureRun(t, []string{
		"uninstall", "--home", home, "--botlets-home", botletsHome,
		"--yes", "--skip-cli", "--skip-plugins",
	})
	if err != nil {
		t.Fatalf("uninstall failed: %v\n%s", err, output)
	}
	for _, call := range *calls {
		if strings.Contains(call, "docker rm -f") || strings.Contains(call, "docker volume rm") {
			t.Fatalf("permissive marker must not fall back to env default Docker teardown: %#v\n%s", *calls, output)
		}
	}
}

// TestDockerAgentSlug pins parity with the slug derivation in
// dockerAgentInstallLines (packages/shared/src/agent-docs.ts): if these drift,
// uninstall scopes to the wrong container/volume names and silently leaves the
// real agent behind.
func TestDockerAgentSlug(t *testing.T) {
	cases := map[string]string{
		"https://comment.io":    "comment-io",
		"https://example.comt.dev":  "example-comt-dev",
		"http://localhost:8787": "localhost-8787",
		"https://comt.dev/":     "comt-dev",
		"https://":              "default",
		"https://Comment.IO":    "comment-io",
	}
	for input, want := range cases {
		if got := dockerAgentSlug(input); got != want {
			t.Errorf("dockerAgentSlug(%q) = %q, want %q", input, got, want)
		}
	}
}
