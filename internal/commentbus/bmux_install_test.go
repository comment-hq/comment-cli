//go:build darwin || linux

package commentbus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bmuxReleaseServer serves a fake bmux asset (and optionally SHA256SUMS) so the
// installer can be exercised without reaching GitHub.
func bmuxReleaseServer(t *testing.T, asset string, payload []byte, sumsBody string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/"+asset, func(w http.ResponseWriter, _ *http.Request) {
		sum := sha256.Sum256(payload)
		w.Header().Set("ETag", `"`+hex.EncodeToString(sum[:])+`"`)
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(payload)))
		_, _ = w.Write(payload)
	})
	if sumsBody != "" {
		mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(sumsBody))
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// isolateBmuxEnv points the installer at an empty home + explicit install dir so
// no real bmux on the host resolves during the AlreadyPresent precheck.
func isolateBmuxEnv(t *testing.T) (installDir string) {
	t.Helper()
	home := t.TempDir()
	installDir = filepath.Join(t.TempDir(), "bin")
	t.Setenv("HOME", home)
	t.Setenv(BmuxBinaryEnv, "")
	t.Setenv("BMUX_REPO", "")
	t.Setenv("BMUX_VERSION", "")
	t.Setenv("BMUX_INSTALL_DIR", installDir)
	return installDir
}

func TestEnsureBmuxInstalledDownloadsAndVerifies(t *testing.T) {
	installDir := isolateBmuxEnv(t)
	asset, err := bmuxAssetName()
	if err != nil {
		t.Skipf("no prebuilt asset for this platform: %v", err)
	}
	payload := []byte("#!/bin/sh\necho bmux-fake\n")
	sum := sha256.Sum256(payload)
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), asset)
	srv := bmuxReleaseServer(t, asset, payload, sums)
	t.Setenv("BMUX_BASE_URL", srv.URL)

	res, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("EnsureBmuxInstalled: %v", err)
	}
	if !res.Installed {
		t.Fatalf("Installed = false, want true (result %#v)", res)
	}
	dest := filepath.Join(installDir, "bmux")
	got, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatalf("read installed bmux: %v", rerr)
	}
	if string(got) != string(payload) {
		t.Fatalf("installed bytes mismatch")
	}
	info, _ := os.Stat(dest)
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("installed bmux is not executable: mode %v", info.Mode())
	}
	// installDir here is a custom, non-trusted dir, so the unpinned daemon cannot
	// discover it — the caller is expected to pin it.
	if res.Discoverable {
		t.Fatalf("Discoverable = true for a non-trusted install dir; want false")
	}
}

func TestEnsureBmuxInstalledDiscoverableInTrustedDir(t *testing.T) {
	asset, err := bmuxAssetName()
	if err != nil {
		t.Skipf("no prebuilt asset for this platform: %v", err)
	}
	// Install into ~/.local/bin, which IS a trusted search dir, so the daemon can
	// discover it without a pin.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(BmuxBinaryEnv, "")
	t.Setenv("BMUX_INSTALL_DIR", "") // default -> ~/.local/bin
	payload := []byte("trusted bmux")
	srv := bmuxReleaseServer(t, asset, payload, "")
	t.Setenv("BMUX_BASE_URL", srv.URL)

	res, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("EnsureBmuxInstalled: %v", err)
	}
	if !res.Installed || !res.Discoverable {
		t.Fatalf("result = %#v, want Installed+Discoverable", res)
	}
	if res.Path != filepath.Join(home, ".local", "bin", "bmux") {
		t.Fatalf("Path = %q, want ~/.local/bin/bmux", res.Path)
	}
}

func TestEnsureBmuxInstalledRejectsChecksumMismatch(t *testing.T) {
	installDir := isolateBmuxEnv(t)
	asset, err := bmuxAssetName()
	if err != nil {
		t.Skipf("no prebuilt asset for this platform: %v", err)
	}
	payload := []byte("real bmux bytes")
	// Publish a checksum for different content -> must be rejected.
	wrong := sha256.Sum256([]byte("tampered"))
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(wrong[:]), asset)
	srv := bmuxReleaseServer(t, asset, payload, sums)
	t.Setenv("BMUX_BASE_URL", srv.URL)

	_, err = EnsureBmuxInstalled(BmuxInstallOptions{})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want checksum mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(installDir, "bmux")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("bmux should not be installed on checksum mismatch")
	}
}

func TestEnsureBmuxInstalledMissingChecksumStillInstalls(t *testing.T) {
	installDir := isolateBmuxEnv(t)
	asset, err := bmuxAssetName()
	if err != nil {
		t.Skipf("no prebuilt asset for this platform: %v", err)
	}
	payload := []byte("bmux without sums")
	srv := bmuxReleaseServer(t, asset, payload, "") // no SHA256SUMS route -> 404
	t.Setenv("BMUX_BASE_URL", srv.URL)

	res, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("EnsureBmuxInstalled (no sums): %v", err)
	}
	if !res.Installed {
		t.Fatalf("Installed = false, want true")
	}
	if _, statErr := os.Stat(filepath.Join(installDir, "bmux")); statErr != nil {
		t.Fatalf("bmux not installed: %v", statErr)
	}
}

func TestEnsureBmuxInstalledSkipsWhenPresent(t *testing.T) {
	// Make bmux already resolvable via an explicit absolute pin with a matching
	// channel marker, then point the base URL at a server with no routes: a
	// download attempt would fail, proving the precheck short-circuits before any
	// network access.
	dir := t.TempDir()
	existing := filepath.Join(dir, "bmux")
	if err := os.WriteFile(existing, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	asset, err := bmuxAssetName()
	if err != nil {
		t.Skipf("no prebuilt asset for this platform: %v", err)
	}
	baseURL := "http://127.0.0.1:0"
	marker := bmuxInstallMarker{BaseURL: baseURL, Asset: asset, Fingerprint: "sha256:present"}
	if err := os.WriteFile(filepath.Join(dir, bmuxChannelMarkerName), []byte(formatBmuxInstallMarker(marker)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BmuxBinaryEnv, existing)
	t.Setenv("BMUX_BASE_URL", baseURL) // unusable on purpose

	res, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("EnsureBmuxInstalled (present): %v", err)
	}
	if !res.AlreadyPresent || res.Installed {
		t.Fatalf("result = %#v, want AlreadyPresent only", res)
	}
}

func TestEnsureBmuxInstalledPinnedPresentIsNotDaemonDiscoverable(t *testing.T) {
	// A shell pin can point at a perfectly usable bmux outside the daemon's bare
	// trusted search path. In that case the install may be "already present", but
	// the launchd/systemd service still needs COMMENT_IO_BMUX_BIN baked in.
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	existing := filepath.Join(dir, "bmux")
	if err := os.WriteFile(existing, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	asset, err := bmuxAssetName()
	if err != nil {
		t.Skipf("no prebuilt asset for this platform: %v", err)
	}
	baseURL := "http://127.0.0.1:0"
	marker := bmuxInstallMarker{BaseURL: baseURL, Asset: asset, Fingerprint: "sha256:present"}
	if err := os.WriteFile(filepath.Join(dir, bmuxChannelMarkerName), []byte(formatBmuxInstallMarker(marker)), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BmuxBinaryEnv, existing)
	t.Setenv("BMUX_BASE_URL", baseURL) // unusable on purpose

	res, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("EnsureBmuxInstalled (present): %v", err)
	}
	if !res.AlreadyPresent || res.Installed {
		t.Fatalf("result = %#v, want AlreadyPresent only", res)
	}
	if res.Discoverable {
		t.Fatalf("Discoverable = true for shell-only pin %q; want false", existing)
	}
}

func TestEnsureBmuxInstalledKeepsExplicitUnmarkedPin(t *testing.T) {
	// Manual bmux installs are allowed to be unmarked when the operator pins the
	// exact binary; this is the advertised fallback on platforms without a
	// published bmux asset.
	dir := t.TempDir()
	existing := filepath.Join(dir, "bmux")
	if err := os.WriteFile(existing, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BmuxBinaryEnv, existing)
	t.Setenv("BMUX_BASE_URL", "http://127.0.0.1:0") // unusable on purpose

	res, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("EnsureBmuxInstalled (manual pin): %v", err)
	}
	if !res.AlreadyPresent || res.Installed {
		t.Fatalf("result = %#v, want AlreadyPresent only", res)
	}
	if res.Path != existing {
		t.Fatalf("Path = %q, want explicit pin %q", res.Path, existing)
	}
}

func TestEnsureBmuxInstalledReinstallsOnChannelSwitch(t *testing.T) {
	asset, err := bmuxAssetName()
	if err != nil {
		t.Skipf("no prebuilt asset for this platform: %v", err)
	}
	home := t.TempDir()
	dest := filepath.Join(home, ".local", "bin", "bmux")
	marker := filepath.Join(home, ".local", "bin", bmuxChannelMarkerName)
	t.Setenv("HOME", home)
	t.Setenv(BmuxBinaryEnv, dest)
	t.Setenv("BMUX_INSTALL_DIR", filepath.Dir(dest))

	// First install from "channel A".
	srvA := bmuxReleaseServer(t, asset, []byte("bmux channel A"), "")
	t.Setenv("BMUX_BASE_URL", srvA.URL)
	resA, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("install A: %v", err)
	}
	if !resA.Installed {
		t.Fatalf("install A result = %#v, want Installed", resA)
	}
	if got, _ := os.ReadFile(marker); parseBmuxInstallMarker(got).BaseURL != srvA.URL {
		t.Fatalf("channel marker = %q, want base %q", got, srvA.URL)
	}

	// Re-install from the SAME channel: precheck must short-circuit (no download),
	// proven by pointing the base URL at an unusable server.
	t.Setenv("BMUX_BASE_URL", srvA.URL)
	resSame, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("install same channel: %v", err)
	}
	if !resSame.AlreadyPresent || resSame.Installed {
		t.Fatalf("same-channel result = %#v, want AlreadyPresent only", resSame)
	}

	// Switch to "channel B": the recorded marker differs, so the precheck must
	// fall through and reinstall the channel-B binary.
	srvB := bmuxReleaseServer(t, asset, []byte("bmux channel B"), "")
	t.Setenv("BMUX_BASE_URL", srvB.URL)
	resB, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("install B: %v", err)
	}
	if !resB.Installed {
		t.Fatalf("channel switch result = %#v, want a fresh Installed", resB)
	}
	if got, _ := os.ReadFile(dest); string(got) != "bmux channel B" {
		t.Fatalf("after channel switch binary = %q, want channel B", got)
	}
	if got, _ := os.ReadFile(marker); parseBmuxInstallMarker(got).BaseURL != srvB.URL {
		t.Fatalf("channel marker after switch = %q, want base %q", got, srvB.URL)
	}
}

func TestEnsureBmuxInstalledRefreshesSameChannelWhenChecksumChanges(t *testing.T) {
	installDir := isolateBmuxEnv(t)
	asset, err := bmuxAssetName()
	if err != nil {
		t.Skipf("no prebuilt asset for this platform: %v", err)
	}
	dest := filepath.Join(installDir, "bmux")
	t.Setenv(BmuxBinaryEnv, dest)
	payload := []byte("bmux v1")
	assetDownloads := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/"+asset, func(w http.ResponseWriter, _ *http.Request) {
		assetDownloads++
		_, _ = w.Write(payload)
	})
	mux.HandleFunc("/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) {
		sum := sha256.Sum256(payload)
		_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("BMUX_BASE_URL", srv.URL)

	res1, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("first install: %v", err)
	}
	if !res1.Installed || assetDownloads != 1 {
		t.Fatalf("first result = %#v, assetDownloads=%d; want install and one asset fetch", res1, assetDownloads)
	}
	resSame, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("same fingerprint install: %v", err)
	}
	if !resSame.AlreadyPresent || resSame.Installed || assetDownloads != 1 {
		t.Fatalf("same fingerprint result = %#v, assetDownloads=%d; want reuse with no asset fetch", resSame, assetDownloads)
	}

	payload = []byte("bmux v2")
	res2, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("changed fingerprint install: %v", err)
	}
	if !res2.Installed || res2.AlreadyPresent || assetDownloads != 2 {
		t.Fatalf("changed fingerprint result = %#v, assetDownloads=%d; want reinstall and second asset fetch", res2, assetDownloads)
	}
	if got, _ := os.ReadFile(dest); string(got) != "bmux v2" {
		t.Fatalf("installed binary = %q, want bmux v2", got)
	}
}

func TestEnsureBmuxInstalledRefreshesLegacySameChannelMarker(t *testing.T) {
	installDir := isolateBmuxEnv(t)
	asset, err := bmuxAssetName()
	if err != nil {
		t.Skipf("no prebuilt asset for this platform: %v", err)
	}
	dest := filepath.Join(installDir, "bmux")
	t.Setenv(BmuxBinaryEnv, dest)
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("legacy same-channel bmux"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := bmuxReleaseServer(t, asset, []byte("fresh same-channel bmux"), "")
	t.Setenv("BMUX_BASE_URL", srv.URL)
	if err := os.WriteFile(filepath.Join(installDir, bmuxChannelMarkerName), []byte(srv.URL+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("legacy marker refresh: %v", err)
	}
	if !res.Installed || res.AlreadyPresent {
		t.Fatalf("legacy marker result = %#v, want Installed only", res)
	}
	if got, _ := os.ReadFile(dest); string(got) != "fresh same-channel bmux" {
		t.Fatalf("refreshed binary = %q, want fresh same-channel bmux", got)
	}
	marker := parseBmuxInstallMarker(mustReadFile(t, filepath.Join(installDir, bmuxChannelMarkerName)))
	if marker.BaseURL != srv.URL || marker.Asset != asset || marker.Fingerprint == "" {
		t.Fatalf("marker = %#v, want structured base/asset/fingerprint", marker)
	}
}

func TestEnsureBmuxInstalledRefreshesUnmarkedBinary(t *testing.T) {
	// A binary with no channel marker has unknown provenance. Refresh it once so
	// old pre-channel bmux installs cannot satisfy "already present" while using a
	// stale control protocol.
	home := t.TempDir()
	t.Setenv("HOME", home)
	dest := filepath.Join(home, ".local", "bin", "bmux")
	marker := filepath.Join(home, ".local", "bin", bmuxChannelMarkerName)
	t.Setenv(BmuxBinaryEnv, "")
	t.Setenv("BMUX_INSTALL_DIR", filepath.Dir(dest))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("preexisting bmux"), 0o755); err != nil {
		t.Fatal(err)
	}
	asset, err := bmuxAssetName()
	if err != nil {
		t.Skipf("no prebuilt asset for this platform: %v", err)
	}
	srv := bmuxReleaseServer(t, asset, []byte("fresh channel bmux"), "")
	t.Setenv("BMUX_BASE_URL", srv.URL)
	res, err := EnsureBmuxInstalled(BmuxInstallOptions{})
	if err != nil {
		t.Fatalf("EnsureBmuxInstalled (unmarked refresh): %v", err)
	}
	if !res.Installed || res.AlreadyPresent {
		t.Fatalf("result = %#v, want Installed only for an unmarked binary", res)
	}
	if got, _ := os.ReadFile(dest); string(got) != "fresh channel bmux" {
		t.Fatalf("refreshed binary = %q, want fresh channel bmux", got)
	}
	if got, _ := os.ReadFile(marker); parseBmuxInstallMarker(got).BaseURL != srv.URL {
		t.Fatalf("channel marker = %q, want base %q", got, srv.URL)
	}
}

func TestSameExecutablePathResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "bmux-real")
	link := filepath.Join(dir, "bmux")
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !sameExecutablePath(link, real) {
		t.Fatalf("sameExecutablePath(%q, %q) = false, want true", link, real)
	}
	if sameExecutablePath(link, other) {
		t.Fatalf("sameExecutablePath(%q, %q) = true, want false", link, other)
	}
}

func TestEnsureBmuxInstalledForceReinstalls(t *testing.T) {
	installDir := isolateBmuxEnv(t)
	// Even with a usable pin, Force must download and install fresh.
	existing := filepath.Join(t.TempDir(), "bmux")
	if err := os.WriteFile(existing, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(BmuxBinaryEnv, existing)
	asset, err := bmuxAssetName()
	if err != nil {
		t.Skipf("no prebuilt asset for this platform: %v", err)
	}
	payload := []byte("forced bmux")
	srv := bmuxReleaseServer(t, asset, payload, "")
	t.Setenv("BMUX_BASE_URL", srv.URL)

	res, err := EnsureBmuxInstalled(BmuxInstallOptions{Force: true})
	if err != nil {
		t.Fatalf("EnsureBmuxInstalled (force): %v", err)
	}
	if !res.Installed {
		t.Fatalf("Installed = false under Force")
	}
	if _, statErr := os.Stat(filepath.Join(installDir, "bmux")); statErr != nil {
		t.Fatalf("forced bmux not installed: %v", statErr)
	}
}

func TestBmuxChecksumFor(t *testing.T) {
	sums := "deadbeef  bmux-linux-x86_64\n" +
		"abc123  *bmux-macos-universal\n" +
		"garbage line\n"
	if got := bmuxChecksumFor(sums, "bmux-linux-x86_64"); got != "deadbeef" {
		t.Fatalf("linux checksum = %q", got)
	}
	if got := bmuxChecksumFor(sums, "bmux-macos-universal"); got != "abc123" {
		t.Fatalf("macos checksum (binary-mode '*') = %q", got)
	}
	if got := bmuxChecksumFor(sums, "bmux-windows-x86_64.exe"); got != "" {
		t.Fatalf("missing asset checksum = %q, want empty", got)
	}
}

func TestBmuxAssetNameFor(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
		wantErr            bool
	}{
		{"darwin", "arm64", "bmux-macos-universal", false},
		{"darwin", "amd64", "bmux-macos-universal", false},
		{"linux", "amd64", "bmux-linux-x86_64", false},
		{"linux", "arm64", "bmux-linux-arm64", false},
		{"windows", "amd64", "", true},
	}
	for _, c := range cases {
		got, err := bmuxAssetNameFor(c.goos, c.goarch)
		if c.wantErr {
			if err == nil || !errors.Is(err, ErrBmuxNotInstalled) {
				t.Fatalf("%s/%s err = %v, want ErrBmuxNotInstalled", c.goos, c.goarch, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Fatalf("%s/%s = %q, %v; want %q", c.goos, c.goarch, got, err, c.want)
		}
	}
}

func TestLaunchSessionSocketErrorMapsMissingBmux(t *testing.T) {
	err := fmt.Errorf("%w: bmux not found in trusted directories", ErrBmuxNotInstalled)
	se := launchSessionSocketError(SessionRecord{Host: SessionHostBmux}, err)
	if se == nil || se.Code != SocketErrorCodeBmuxNotInstalled {
		t.Fatalf("socket error = %#v, want code %q", se, SocketErrorCodeBmuxNotInstalled)
	}
	if !strings.Contains(se.Message, "bmux is required") {
		t.Fatalf("message = %q, want install guidance", se.Message)
	}
}

func TestBinaryPathTranslatesMissingBmux(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(BmuxBinaryEnv, "") // bare "bmux", not resolvable in empty home
	_, err := ExecBmuxController{Binary: "bmux"}.binaryPath()
	if err == nil || !errors.Is(err, ErrBmuxNotInstalled) {
		t.Fatalf("binaryPath err = %v, want ErrBmuxNotInstalled", err)
	}
}

func TestBmuxInstallDirRejectsRelative(t *testing.T) {
	if _, err := bmuxInstallDir("relative/bin"); err == nil {
		t.Fatal("expected error for a relative explicit install dir")
	}
	t.Setenv("BMUX_INSTALL_DIR", "also/relative")
	if _, err := bmuxInstallDir(""); err == nil {
		t.Fatal("expected error for a relative BMUX_INSTALL_DIR")
	}
	t.Setenv("BMUX_INSTALL_DIR", "/abs/ok")
	if dir, err := bmuxInstallDir(""); err != nil || dir != "/abs/ok" {
		t.Fatalf("absolute BMUX_INSTALL_DIR = (%q, %v), want (/abs/ok, nil)", dir, err)
	}
}

func TestBmuxReleaseBaseChannels(t *testing.T) {
	repo := "https://github.com/" + bmuxDistRepo + "/releases"
	t.Run("production default -> latest", func(t *testing.T) {
		t.Setenv(EnvVar, "")
		t.Setenv("BMUX_BASE_URL", "")
		t.Setenv("BMUX_VERSION", "")
		t.Setenv("BMUX_REPO", "")
		withCLIReleaseVersion(t, "0.1.8") // clean release build
		if got := bmuxReleaseBase("", ""); got != repo+"/latest/download" {
			t.Fatalf("prod base = %q", got)
		}
	})
	t.Run("staging env -> staging tag", func(t *testing.T) {
		t.Setenv(EnvVar, EnvStaging)
		t.Setenv("BMUX_BASE_URL", "")
		t.Setenv("BMUX_VERSION", "")
		t.Setenv("BMUX_REPO", "")
		withCLIReleaseVersion(t, "0.1.8") // clean build: staging must come from COMMENT_IO_ENV
		if got := bmuxReleaseBase("", ""); got != repo+"/download/"+bmuxStagingTag {
			t.Fatalf("staging base = %q", got)
		}
	})
	t.Run("explicit version wins over channel", func(t *testing.T) {
		t.Setenv(EnvVar, EnvStaging)
		t.Setenv("BMUX_BASE_URL", "")
		if got := bmuxReleaseBase("", "bmux-v1.2.3"); got != repo+"/download/bmux-v1.2.3" {
			t.Fatalf("explicit version base = %q", got)
		}
	})
	t.Run("BMUX_BASE_URL overrides everything", func(t *testing.T) {
		t.Setenv(EnvVar, EnvStaging)
		t.Setenv("BMUX_BASE_URL", "https://mirror.example/x/")
		if got := bmuxReleaseBase("", ""); got != "https://mirror.example/x" {
			t.Fatalf("override base = %q", got)
		}
	})
}

// withCLIReleaseVersion sets the process-global CLI build version for the
// duration of a test and restores it afterward, so channel-selection tests that
// depend on the build's npm release channel don't leak into one another.
func withCLIReleaseVersion(t *testing.T, v string) {
	t.Helper()
	prev := cliReleaseVersion
	SetCLIReleaseVersion(v)
	t.Cleanup(func() { cliReleaseVersion = prev })
}

// TestBmuxReleaseBasePrereleaseBuildSelectsStaging is the regression for the
// team-install bmux 404: a prerelease CLI build (the @comment-io/cli@staging
// line) pointed at staging via a COMMENT_IO_BASE_URL override leaves
// COMMENT_IO_ENV at its production default, yet must still fetch bmux from the
// staging channel rather than the nonexistent production `latest` release.
func TestBmuxReleaseBasePrereleaseBuildSelectsStaging(t *testing.T) {
	repo := "https://github.com/" + bmuxDistRepo + "/releases"
	t.Setenv(EnvVar, "") // production environment (no COMMENT_IO_ENV)
	t.Setenv("BMUX_BASE_URL", "")
	t.Setenv("BMUX_VERSION", "")
	t.Setenv("BMUX_REPO", "")

	t.Run("prerelease build -> staging tag", func(t *testing.T) {
		withCLIReleaseVersion(t, "0.1.9-alpha.251")
		if got := bmuxReleaseBase("", ""); got != repo+"/download/"+bmuxStagingTag {
			t.Fatalf("prerelease build base = %q, want staging tag", got)
		}
	})
	t.Run("clean release build -> latest", func(t *testing.T) {
		withCLIReleaseVersion(t, "0.1.8")
		if got := bmuxReleaseBase("", ""); got != repo+"/latest/download" {
			t.Fatalf("clean release build base = %q, want latest", got)
		}
	})
	t.Run("explicit BMUX_VERSION still wins over a prerelease build", func(t *testing.T) {
		withCLIReleaseVersion(t, "0.1.9-alpha.251")
		t.Setenv("BMUX_VERSION", "bmux-v1.2.3")
		if got := bmuxReleaseBase("", ""); got != repo+"/download/bmux-v1.2.3" {
			t.Fatalf("explicit version base = %q", got)
		}
	})
}

func TestSemverHasPrerelease(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"0.1.9-alpha.251", true}, // @comment-io/cli@staging line
		{"v0.1.9-alpha.251", true},
		{"0.1.3-alpha.15", true},
		{"1.0.0-rc.1+build.7", true},
		{"0.1.8", false}, // npm latest line
		{"0.1.4", false}, // prod-candidate line
		{"1.0.0+build.7", false},
		{"dev", false}, // local source build
		{"", false},
		{"feature-x", false}, // not a version: numeric core required
		{"-alpha", false},    // leading dash, empty core
	}
	for _, c := range cases {
		if got := semverHasPrerelease(c.in); got != c.want {
			t.Errorf("semverHasPrerelease(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestBmuxNotInstalledMessageMentionsKnobs(t *testing.T) {
	t.Setenv(EnvVar, "")
	withCLIReleaseVersion(t, "0.1.8")
	msg := BmuxNotInstalledMessage()
	// bmux is no longer auto-installed: the message must point at the manual
	// install hint and the COMMENT_IO_BMUX_BIN pin, not `comment doctor --fix`.
	for _, want := range []string{BmuxBinaryEnv, "comment bus install", "install.sh"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("BmuxNotInstalledMessage missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "comment doctor --fix") {
		t.Fatalf("BmuxNotInstalledMessage must not claim `comment doctor --fix` installs bmux:\n%s", msg)
	}
	if !strings.Contains(BmuxInstallHintShort(), "install.sh") {
		t.Fatalf("BmuxInstallHintShort = %q", BmuxInstallHintShort())
	}
}

func TestBmuxInstallHintShortMatchesChannel(t *testing.T) {
	t.Run("production", func(t *testing.T) {
		t.Setenv(EnvVar, "")
		withCLIReleaseVersion(t, "0.1.8")
		got := BmuxInstallHintShort()
		if !strings.HasSuffix(got, " | sh") || strings.Contains(got, "COMMENT_IO_ENV=staging") {
			t.Fatalf("production hint = %q", got)
		}
	})
	t.Run("staging env", func(t *testing.T) {
		t.Setenv(EnvVar, EnvStaging)
		withCLIReleaseVersion(t, "0.1.8")
		if got := BmuxInstallHintShort(); !strings.Contains(got, "| COMMENT_IO_ENV=staging sh") {
			t.Fatalf("staging hint = %q, want staging env piped to sh", got)
		}
	})
	t.Run("prerelease build", func(t *testing.T) {
		t.Setenv(EnvVar, "")
		withCLIReleaseVersion(t, "0.1.9-alpha.251")
		if got := BmuxInstallHintShort(); !strings.Contains(got, "| COMMENT_IO_ENV=staging sh") {
			t.Fatalf("prerelease hint = %q, want staging env piped to sh", got)
		}
	})
}

func TestBmuxInstallMarkerParsesLegacyAndStructured(t *testing.T) {
	legacy := parseBmuxInstallMarker([]byte("https://github.com/comment-hq/bmux/releases/latest/download\n"))
	if legacy.BaseURL != "https://github.com/comment-hq/bmux/releases/latest/download" || legacy.Asset != "" || legacy.Fingerprint != "" {
		t.Fatalf("legacy marker = %#v", legacy)
	}
	structured := parseBmuxInstallMarker([]byte("base_url=https://example.test/base\nasset=bmux-linux-arm64\nfingerprint=sha256:abc\n"))
	if structured.BaseURL != "https://example.test/base" || structured.Asset != "bmux-linux-arm64" || structured.Fingerprint != "sha256:abc" {
		t.Fatalf("structured marker = %#v", structured)
	}
	withBOM := parseBmuxInstallMarker([]byte("\xef\xbb\xbfbase_url=https://example.test/base\nasset=bmux-linux-arm64\n"))
	if withBOM.BaseURL != "https://example.test/base" || withBOM.Asset != "bmux-linux-arm64" {
		t.Fatalf("BOM marker = %#v", withBOM)
	}
}

func TestBmuxHTTPFingerprintIgnoresRedirectURL(t *testing.T) {
	a := bmuxHTTPFingerprint(bmuxHTTPMetadata{
		FinalURL:      "https://objects.example/download-a",
		ETag:          `"abc"`,
		ContentLength: 123,
	})
	b := bmuxHTTPFingerprint(bmuxHTTPMetadata{
		FinalURL:      "https://objects.example/download-b",
		ETag:          `"abc"`,
		ContentLength: 123,
	})
	if a == "" || a != b {
		t.Fatalf("fingerprints = %q and %q, want stable non-empty value independent of final URL", a, b)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
