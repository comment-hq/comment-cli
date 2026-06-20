package commentbus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrBmuxNotInstalled marks the specific failure where the bmux binary cannot be
// located at all — it is not present in any trusted directory and no usable
// COMMENT_IO_BMUX_BIN pin is set. It is distinct from "a configured bmux pin is
// unusable" so callers can surface a clear install message (and the daemon can
// auto-install) instead of an opaque launch failure. Detect it with errors.Is.
var ErrBmuxNotInstalled = errors.New("bmux is not installed")

// SocketErrorCodeBmuxNotInstalled is the local-bus socket error code the daemon
// returns when it cannot launch a runtime because bmux is missing. The CLI maps
// it to a dedicated, human-readable exit status. Additive: older clients that
// don't recognize it still print the accompanying message and exit non-zero.
const SocketErrorCodeBmuxNotInstalled = "BMUX_NOT_INSTALLED"

const (
	// bmuxDistRepo is the public, binaries-only repo that hosts prebuilt bmux
	// releases. Overridable via BMUX_REPO for mirrors / forks / tests.
	bmuxDistRepo = "comment-hq/bmux"
	// BmuxInstallScriptURL is the one-line installer users run by hand to install
	// bmux, the explicit opt-in runtime host. bmux is no longer auto-installed by
	// `comment bus install` / `comment doctor --fix`, so this is the install path.
	BmuxInstallScriptURL = "https://raw.githubusercontent.com/" + bmuxDistRepo + "/main/install.sh"

	// bmuxStagingTag is the moving prerelease tag the staging channel publishes to
	// (see .github/workflows/bmux-publish.yml). Production fetches the "latest"
	// release; staging fetches this tag so the two channels stay isolated.
	bmuxStagingTag = "staging"
)

// cliReleaseVersion is the CLI build version (main.version), published once at
// process start via SetCLIReleaseVersion. It lets bmux channel selection follow
// the binary's own npm release channel rather than COMMENT_IO_ENV alone: the
// `@comment-io/cli@staging` line carries a SemVer prerelease version (e.g.
// "0.1.9-alpha.251") and tracks `main`, while the npm `latest` line is a clean
// release (e.g. "0.1.8"). A prerelease build must therefore fetch bmux from the
// staging channel even when it is pointed at the staging server purely by a
// COMMENT_IO_BASE_URL override — the team-install flow installs the staging CLI
// for any non-comment.io base URL but leaves COMMENT_IO_ENV at its production
// default, so without this the staging CLI reaches for the production `latest`
// bmux release that does not exist and 404s. Empty/"dev" (local source builds)
// is treated as production; those build bmux from source or pin BMUX_VERSION.
var cliReleaseVersion string

// SetCLIReleaseVersion records the running CLI's build version so bmux channel
// selection can detect a prerelease (staging) build. Call once from the command
// entry point before any bmux install runs: it is a process-global set at
// startup, not a per-request value.
func SetCLIReleaseVersion(v string) {
	cliReleaseVersion = strings.TrimSpace(v)
}

// cliReleaseIsPrerelease reports whether the running CLI build is a SemVer
// prerelease such as "0.1.9-alpha.251". Clean releases ("0.1.8") and the "dev"
// default return false.
func cliReleaseIsPrerelease() bool {
	return semverHasPrerelease(cliReleaseVersion)
}

// semverHasPrerelease reports whether v is a SemVer with a prerelease segment: a
// '-' that follows a dotted-numeric version core. Build metadata ("+sha") is
// ignored and a leading "v" is tolerated. Requiring a numeric core ensures
// non-version strings ("dev", a branch name) never read as a prerelease.
func semverHasPrerelease(v string) bool {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexByte(v, '+'); i >= 0 {
		v = v[:i]
	}
	dash := strings.IndexByte(v, '-')
	if dash <= 0 {
		return false
	}
	for _, r := range v[:dash] {
		if r != '.' && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// BmuxInstallHintShort returns the single-line command that installs bmux,
// suitable for compact contexts like `comment doctor` JSON output. bmux is a
// first-party binary we publish, so unlike tmux there is one canonical installer
// rather than a per-distro package name.
func BmuxInstallHintShort() string {
	if bmuxInstallChannelIsStaging() {
		return "curl -fsSL " + BmuxInstallScriptURL + " | COMMENT_IO_ENV=staging sh"
	}
	return "curl -fsSL " + BmuxInstallScriptURL + " | sh"
}

// BmuxNotInstalledMessage builds the full, human-readable error explaining that
// bmux is required and exactly how to get it. Used for both the daemon's socket
// error message and the CLI's direct failures so the wording is identical
// wherever the user hits it.
func BmuxNotInstalledMessage() string {
	return "bmux is required for this session's runtime host, but it could not be found.\n\n" +
		"tmux is the default host; bmux is only used when explicitly selected\n" +
		"(a host=bmux session, or " + BmuxBinaryEnv + "). bmux is not auto-installed.\n" +
		"Install it by hand:\n" +
		"  " + BmuxInstallHintShort() + "\n\n" +
		"Already have bmux installed somewhere non-standard? Pin it for Comment.io with:\n" +
		"  export " + BmuxBinaryEnv + "=/absolute/path/to/bmux\n" +
		"then re-run `comment bus install` so the background daemon (which does not\n" +
		"inherit your shell environment) bakes the pin into its service definition.\n" +
		"Then start the bmux session again. (`comment doctor` verifies the default\n" +
		"tmux host, not bmux, so it will not confirm this opt-in pin.)"
}

func bmuxInstallChannelIsStaging() bool {
	return CurrentEnvironment().IsStaging() || cliReleaseIsPrerelease()
}

// BmuxInstallOptions controls EnsureBmuxInstalled. The zero value installs the
// latest published bmux for the current platform into the default trusted
// directory (~/.local/bin) from the public GitHub release.
type BmuxInstallOptions struct {
	// BaseURL overrides the release-asset base entirely (mirrors / air-gapped /
	// tests). When set, assets are fetched from "<BaseURL>/<asset>". Falls back to
	// the BMUX_BASE_URL environment variable, then to the public GitHub release.
	BaseURL string
	// InstallDir is the directory the binary is written to. Defaults to
	// BMUX_INSTALL_DIR, then ~/.local/bin (a trusted search dir).
	InstallDir string
	// Version is the release tag to install ("latest" by default, or BMUX_VERSION).
	Version string
	// Force reinstalls even when a usable bmux already resolves.
	Force bool
}

// BmuxInstallResult reports what EnsureBmuxInstalled did.
type BmuxInstallResult struct {
	// Path is the absolute path to the usable bmux binary.
	Path string
	// Installed is true when this call downloaded and placed the binary.
	Installed bool
	// AlreadyPresent is true when a usable bmux already resolved and the install
	// was skipped (Force was false).
	AlreadyPresent bool
	// Discoverable is true when an unpinned launchd/systemd daemon — which
	// resolves bare "bmux" from the trusted directories and does NOT inherit the
	// shell's COMMENT_IO_BMUX_BIN — can find a bmux. When false, the caller must
	// pin COMMENT_IO_BMUX_BIN to Path (or the daemon will still fail to launch).
	Discoverable bool
}

// BmuxDaemonDiscoverable reports whether an unpinned launchd/systemd daemon would
// resolve bmux. Passing the bare name explicitly bypasses the env pin that
// TrustedBmuxBinaryPath("") honors, so this reflects the daemon's real view
// (trusted directories only, no shell environment).
func BmuxDaemonDiscoverable() bool {
	_, err := TrustedBmuxBinaryPath("bmux")
	return err == nil
}

func bmuxDaemonDiscoversPath(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	resolved, err := TrustedBmuxBinaryPath("bmux")
	if err != nil {
		return false
	}
	return sameExecutablePath(resolved, path)
}

func sameExecutablePath(a, b string) bool {
	a = filepath.Clean(a)
	b = filepath.Clean(b)
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = filepath.Clean(resolved)
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = filepath.Clean(resolved)
	}
	return a == b
}

// bmuxHTTPClient is the client used to fetch release assets. A generous timeout
// covers a slow connection without hanging an install indefinitely.
var bmuxHTTPClient = &http.Client{Timeout: 60 * time.Second}

// EnsureBmuxInstalled makes the bmux binary available in a trusted directory so
// the daemon can launch managed runtimes. If bmux already resolves it is a no-op
// (unless opts.Force). Otherwise it downloads the prebuilt binary for the current
// platform from the public release, verifies its checksum when SHA256SUMS is
// published, and installs it to ~/.local/bin/bmux. It returns ErrBmuxNotInstalled
// (wrapped) when the current platform has no prebuilt binary.
func EnsureBmuxInstalled(opts BmuxInstallOptions) (BmuxInstallResult, error) {
	base := bmuxReleaseBase(opts.BaseURL, opts.Version)

	if !opts.Force {
		usesExplicitPin := strings.TrimSpace(os.Getenv(BmuxBinaryEnv)) != ""
		if path, err := TrustedBmuxBinaryPath(""); err == nil && bmuxInstalledChannelMatches(path, base, usesExplicitPin) {
			// A usable bmux already resolves AND it was installed from the same
			// release channel we'd fetch now. Skipping the download keeps install
			// fast. When the recorded channel differs (e.g. a production binary on
			// disk while installing staging), fall through and reinstall so the
			// daemon launches a binary whose protocol matches this client; a stale
			// cross-channel binary can fail the bmux handshake and leave `comment
			// run` broken even though install "succeeded".
			return BmuxInstallResult{Path: path, AlreadyPresent: true, Discoverable: bmuxDaemonDiscoversPath(path)}, nil
		}
	}

	asset, err := bmuxAssetName()
	if err != nil {
		return BmuxInstallResult{}, err
	}

	installDir, err := bmuxInstallDir(opts.InstallDir)
	if err != nil {
		return BmuxInstallResult{}, err
	}
	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return BmuxInstallResult{}, fmt.Errorf("create bmux install dir %s: %w", installDir, err)
	}

	var expectedChecksum string
	if sums, serr := bmuxFetch(base + "/SHA256SUMS"); serr == nil {
		expectedChecksum = bmuxChecksumFor(string(sums), asset)
	}

	binData, assetMeta, err := bmuxFetchAsset(base + "/" + asset)
	if err != nil {
		return BmuxInstallResult{}, fmt.Errorf("download bmux (%s): %w", asset, err)
	}
	if len(binData) == 0 {
		return BmuxInstallResult{}, fmt.Errorf("download bmux (%s): empty response", asset)
	}

	// Verify the checksum when SHA256SUMS is published (best effort, mirroring the
	// shell installer: a missing checksum file is not fatal, a mismatch is).
	if expectedChecksum != "" {
		got := sha256.Sum256(binData)
		if !strings.EqualFold(hex.EncodeToString(got[:]), expectedChecksum) {
			return BmuxInstallResult{}, fmt.Errorf("bmux checksum mismatch for %s (got %x, want %s)", asset, got, expectedChecksum)
		}
	}

	dest := filepath.Join(installDir, "bmux")
	if err := bmuxWriteExecutable(dest, binData); err != nil {
		return BmuxInstallResult{}, err
	}

	// Record which release channel this binary came from so a later install for a
	// different channel (e.g. switching production <-> staging) reinstalls instead
	// of short-circuiting on the AlreadyPresent precheck. Best-effort: a missing or
	// unreadable marker simply means the next install refreshes bmux once and
	// writes a fresh marker.
	fingerprint := bmuxChecksumFingerprint(expectedChecksum)
	if fingerprint == "" {
		fingerprint = bmuxHTTPFingerprint(assetMeta)
	}
	writeBmuxChannelMarker(installDir, bmuxInstallMarker{
		BaseURL:     base,
		Asset:       asset,
		Fingerprint: fingerprint,
	})

	// Report whether the daemon (bare "bmux", trusted dirs, no shell env) can now
	// discover the install. When it can't (e.g. a custom non-trusted install dir),
	// the caller must pin COMMENT_IO_BMUX_BIN to Path or the daemon still fails.
	return BmuxInstallResult{Path: dest, Installed: true, Discoverable: bmuxDaemonDiscoversPath(dest)}, nil
}

// bmuxChannelMarkerName is the sidecar file written next to an installed bmux
// recording the release base it was fetched from. It lets a later install detect
// a channel switch and reinstall rather than keeping a cross-channel binary.
const bmuxChannelMarkerName = ".bmux-channel"

type bmuxInstallMarker struct {
	BaseURL     string
	Asset       string
	Fingerprint string
}

// writeBmuxChannelMarker records channel next to a bmux installed in installDir.
// Best-effort: the binary install is what matters, so a write failure is ignored
// (it only disables cross-channel reinstall detection for this binary).
func writeBmuxChannelMarker(installDir string, marker bmuxInstallMarker) {
	if installDir == "" || marker.BaseURL == "" {
		return
	}
	_ = os.WriteFile(filepath.Join(installDir, bmuxChannelMarkerName), []byte(formatBmuxInstallMarker(marker)), 0o644)
}

func formatBmuxInstallMarker(marker bmuxInstallMarker) string {
	var b strings.Builder
	b.WriteString("base_url=")
	b.WriteString(strings.TrimSpace(marker.BaseURL))
	b.WriteByte('\n')
	if marker.Asset != "" {
		b.WriteString("asset=")
		b.WriteString(strings.TrimSpace(marker.Asset))
		b.WriteByte('\n')
	}
	if marker.Fingerprint != "" {
		b.WriteString("fingerprint=")
		b.WriteString(strings.TrimSpace(marker.Fingerprint))
		b.WriteByte('\n')
	}
	return b.String()
}

func parseBmuxInstallMarker(data []byte) bmuxInstallMarker {
	text := strings.TrimSpace(string(data))
	text = strings.TrimPrefix(text, "\ufeff")
	if text == "" {
		return bmuxInstallMarker{}
	}
	firstLine := text
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		firstLine = text[:i]
	}
	if !strings.Contains(firstLine, "=") {
		// Legacy marker format from #613: a bare release base URL.
		return bmuxInstallMarker{BaseURL: firstLine}
	}
	var marker bmuxInstallMarker
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimPrefix(strings.TrimSpace(key), "\ufeff") {
		case "base_url":
			marker.BaseURL = strings.TrimSpace(value)
		case "asset":
			marker.Asset = strings.TrimSpace(value)
		case "fingerprint":
			marker.Fingerprint = strings.TrimSpace(value)
		}
	}
	return marker
}

// bmuxInstalledChannelMatches reports whether the bmux resolved at binaryPath may
// be reused for an install targeting wantChannel. Auto-discovered unmarked
// binaries have unknown provenance, so refresh them once and write our channel
// marker; otherwise a stale pre-channel binary can keep satisfying "bmux exists"
// while speaking an older control protocol than the freshly installed Comment
// CLI expects. Explicit operator pins are allowed to be unmarked because manual
// installs are the supported fallback for platforms without prebuilt bmux assets.
func bmuxInstalledChannelMatches(binaryPath, wantChannel string, allowUnmarked bool) bool {
	if binaryPath == "" || wantChannel == "" {
		return true
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(binaryPath), bmuxChannelMarkerName))
	if err != nil {
		return allowUnmarked
	}
	marker := parseBmuxInstallMarker(data)
	if strings.TrimSpace(marker.BaseURL) != strings.TrimSpace(wantChannel) {
		return false
	}
	asset, err := bmuxAssetName()
	if err != nil {
		return allowUnmarked
	}
	if marker.Asset != "" && marker.Asset != asset {
		return false
	}
	remoteFingerprint, err := bmuxRemoteFingerprint(wantChannel, asset)
	if err != nil || remoteFingerprint == "" {
		// If GitHub or a mirror is temporarily unreachable, keep the existing
		// same-channel binary usable. A later successful probe will refresh it if
		// the moving channel has advanced.
		return true
	}
	if marker.Asset == "" || marker.Fingerprint == "" {
		return false
	}
	return remoteFingerprint == marker.Fingerprint
}

// bmuxAssetName maps the current platform to the published release asset name.
func bmuxAssetName() (string, error) {
	return bmuxAssetNameFor(runtime.GOOS, runtime.GOARCH)
}

// bmuxAssetNameFor is the pure core of bmuxAssetName: it maps a GOOS/GOARCH pair
// to the published release asset name, matching bmux/packaging/install.sh.
// Returns ErrBmuxNotInstalled (wrapped) for platforms with no prebuilt binary.
func bmuxAssetNameFor(goos, goarch string) (string, error) {
	switch goos {
	case "darwin":
		return "bmux-macos-universal", nil // universal: arm64 + x86_64
	case "linux":
		switch goarch {
		case "amd64":
			return "bmux-linux-x86_64", nil
		case "arm64":
			return "bmux-linux-arm64", nil
		default:
			return "", fmt.Errorf("%w: no prebuilt bmux for linux/%s; build from source (see %s)", ErrBmuxNotInstalled, goarch, bmuxDistRepo)
		}
	default:
		return "", fmt.Errorf("%w: no prebuilt bmux for %s; install it manually", ErrBmuxNotInstalled, goos)
	}
}

// bmuxInstallDir resolves the directory the binary is written to: an explicit
// argument wins, then BMUX_INSTALL_DIR, then ~/.local/bin (a trusted,
// daemon-discoverable dir). Both environments install here — the staging vs
// production split is in which release *channel* is fetched (see
// bmuxReleaseBase), not the on-disk location.
func bmuxInstallDir(explicit string) (string, error) {
	// A relative dir would write the binary relative to the install process's
	// working directory and could never be a usable absolute COMMENT_IO_BMUX_BIN
	// pin for the background daemon — reject it rather than install somewhere the
	// daemon can't resolve while reporting success.
	if dir := strings.TrimSpace(explicit); dir != "" {
		return requireAbsoluteInstallDir(dir)
	}
	if dir := strings.TrimSpace(os.Getenv("BMUX_INSTALL_DIR")); dir != "" {
		return requireAbsoluteInstallDir(dir)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("could not resolve home directory for bmux install: %w", err)
	}
	return filepath.Join(home, ".local", "bin"), nil
}

func requireAbsoluteInstallDir(dir string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("bmux install dir must be an absolute path: %q", dir)
	}
	return dir, nil
}

// bmuxReleaseBase computes the base URL the asset is fetched from. An explicit
// BaseURL (or BMUX_BASE_URL) overrides everything; otherwise it resolves the
// GitHub release-download URL for the requested version ("latest" by default).
func bmuxReleaseBase(explicitBase, version string) string {
	if base := strings.TrimSpace(explicitBase); base != "" {
		return strings.TrimRight(base, "/")
	}
	if base := strings.TrimSpace(os.Getenv("BMUX_BASE_URL")); base != "" {
		return strings.TrimRight(base, "/")
	}
	repo := strings.TrimSpace(os.Getenv("BMUX_REPO"))
	if repo == "" {
		repo = bmuxDistRepo
	}
	ver := strings.TrimSpace(version)
	if ver == "" {
		ver = strings.TrimSpace(os.Getenv("BMUX_VERSION"))
	}
	if ver != "" && ver != "latest" {
		return "https://github.com/" + repo + "/releases/download/" + ver
	}
	// No explicit version: pick the channel for this build. Staging fetches the
	// moving prerelease tag; production fetches the latest stable release
	// (`releases/latest/download` excludes prereleases, so staging builds never
	// leak into production installs). Select staging when either COMMENT_IO_ENV
	// chooses it OR this is a prerelease CLI build (the `@comment-io/cli@staging`
	// line): such a build tracks `main` and ships alongside the staging bmux
	// release, so it must not reach for the production `latest` release, which may
	// not exist yet and would be the wrong protocol.
	if CurrentEnvironment().IsStaging() || cliReleaseIsPrerelease() {
		return "https://github.com/" + repo + "/releases/download/" + bmuxStagingTag
	}
	return "https://github.com/" + repo + "/releases/latest/download"
}

type bmuxHTTPMetadata struct {
	FinalURL      string
	ETag          string
	LastModified  string
	ContentLength int64
}

func bmuxMetadataFromResponse(resp *http.Response) bmuxHTTPMetadata {
	finalURL := ""
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return bmuxHTTPMetadata{
		FinalURL:      finalURL,
		ETag:          strings.TrimSpace(resp.Header.Get("ETag")),
		LastModified:  strings.TrimSpace(resp.Header.Get("Last-Modified")),
		ContentLength: resp.ContentLength,
	}
}

func bmuxChecksumFingerprint(checksum string) string {
	checksum = strings.ToLower(strings.TrimSpace(checksum))
	if checksum == "" {
		return ""
	}
	return "sha256:" + checksum
}

func bmuxHTTPFingerprint(meta bmuxHTTPMetadata) string {
	signals := 0
	parts := []string{}
	if meta.ETag != "" {
		signals++
		parts = append(parts, "etag="+meta.ETag)
	}
	if meta.LastModified != "" {
		signals++
		parts = append(parts, "last_modified="+meta.LastModified)
	}
	if meta.ContentLength >= 0 {
		signals++
		parts = append(parts, fmt.Sprintf("content_length=%d", meta.ContentLength))
	}
	if signals == 0 {
		return ""
	}
	return "http:" + strings.Join(parts, ";")
}

func bmuxRemoteFingerprint(base, asset string) (string, error) {
	if sums, err := bmuxFetch(base + "/SHA256SUMS"); err == nil {
		if want := bmuxChecksumFor(string(sums), asset); want != "" {
			return bmuxChecksumFingerprint(want), nil
		}
	}
	meta, err := bmuxHead(base + "/" + asset)
	if err != nil {
		return "", err
	}
	return bmuxHTTPFingerprint(meta), nil
}

// bmuxFetch performs a GET and returns the body, following redirects (GitHub's
// latest/download redirects to the actual asset).
func bmuxFetch(url string) ([]byte, error) {
	data, _, err := bmuxFetchAsset(url)
	return data, err
}

func bmuxFetchAsset(url string) ([]byte, bmuxHTTPMetadata, error) {
	resp, err := bmuxHTTPClient.Get(url)
	if err != nil {
		return nil, bmuxHTTPMetadata{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, bmuxHTTPMetadata{}, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	// Cap the read so a misbehaving mirror can't exhaust memory; bmux binaries are
	// a few MB, 128 MiB is comfortably above that.
	data, err := io.ReadAll(io.LimitReader(resp.Body, 128*1024*1024))
	return data, bmuxMetadataFromResponse(resp), err
}

func bmuxHead(url string) (bmuxHTTPMetadata, error) {
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return bmuxHTTPMetadata{}, err
	}
	resp, err := bmuxHTTPClient.Do(req)
	if err != nil {
		return bmuxHTTPMetadata{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return bmuxHTTPMetadata{}, fmt.Errorf("HEAD %s: HTTP %d", url, resp.StatusCode)
	}
	return bmuxMetadataFromResponse(resp), nil
}

// bmuxChecksumFor extracts the expected hex digest for asset from SHA256SUMS
// contents (lines of "<hex>␠␠<name>", where the name may be prefixed with '*'
// for binary mode). Returns "" when the asset is not listed.
func bmuxChecksumFor(sums, asset string) string {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == asset {
			return fields[0]
		}
	}
	return ""
}

// bmuxWriteExecutable atomically writes data to dest with mode 0755 via a
// temp file + rename in the same directory.
func bmuxWriteExecutable(dest string, data []byte) error {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".bmux-*")
	if err != nil {
		return fmt.Errorf("create temp bmux in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write bmux: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write bmux: %w", err)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("chmod bmux: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("install bmux to %s: %w", dest, err)
	}
	cleanup = false
	return nil
}
