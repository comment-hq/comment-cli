package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentsync"
)

// version_check.go enforces a server-controlled minimum CLI version.
//
// On startup, user-facing commands call enforceCLIVersion, which asks the
// backend (GET /cli/version) for the minimum supported version. When the
// running binary is older, it auto-runs `comment upgrade` and re-execs the
// original command on the fresh binary. The check fails OPEN on any network or
// parsing error so an offline machine or a backend outage never bricks the CLI,
// and it is fully skipped for local `dev` builds.
//
// Environment escape hatches:
//   - COMMENT_IO_SKIP_VERSION_CHECK=1   bypass the gate entirely
//   - COMMENT_IO_BASE_URL=<url>         point the check at a non-default backend
//   - COMMENT_IO_VERSION_CHECK_TTL=<d>  override the on-disk cache TTL (default 1h)
//
// COMMENT_IO_VERSION_REEXEC is set internally on the post-upgrade re-exec to
// prevent an upgrade loop; it is not a user-facing knob.

const (
	versionCheckReexecEnv  = "COMMENT_IO_VERSION_REEXEC"
	versionCheckSkipEnv    = "COMMENT_IO_SKIP_VERSION_CHECK"
	versionCheckTTLEnv     = "COMMENT_IO_VERSION_CHECK_TTL"
	versionCheckCacheFile  = "version-check.json"
	versionCheckTimeout    = 2 * time.Second
	defaultVersionCheckTTL = time.Hour
)

// cliVersionGatedCommands are the user-facing entrypoints subject to the gate.
// Plumbing and recovery commands (bus, daemon, upgrade, uninstall, doctor,
// diagnose, help, version, internal execs) are intentionally never gated so the
// daemon, the upgrade path itself, and diagnostics keep working on stale builds.
// `mcp` is deliberately excluded too: it is a machine-to-machine stdio handshake
// where a long auto-upgrade mid-startup would hang the MCP client.
var cliVersionGatedCommands = map[string]bool{
	"run":      true,
	"docs":     true,
	"sync":     true,
	"botlets":  true,
	"messages": true,
	"secrets":  true,
	"activity": true,
	"runtime":  true,
	"sessions": true,
	"plugin":   true,
	// listen talks to the daemon over newly-added socket ops (listen.claim/release/
	// handles, messages.wait --rewake); gate it like run/messages so a stale CLI is
	// upgraded before it speaks the new protocol to a newer daemon.
	"listen": true,
}

type versionCheckResponse struct {
	Minimum     string `json:"minimum"`
	Latest      string `json:"latest"`
	Environment string `json:"environment"`
}

type versionCheckCacheState struct {
	CheckedAt time.Time `json:"checked_at"`
	// BaseURL records which backend produced this decision. The cache is only
	// reused when it matches the current backend, so switching
	// COMMENT_IO_BASE_URL (prod <-> staging) within the TTL re-checks the new
	// backend instead of serving the other environment's minimum.
	BaseURL string `json:"base_url"`
	Minimum string `json:"minimum"`
	Latest  string `json:"latest"`
}

// Seams for tests.
var (
	versionCheckNow     = time.Now
	versionCheckFetch   = fetchCLIVersion
	versionCheckUpgrade = autoUpgradeAndReexec
)

var versionCheckHTTPClient = &http.Client{Timeout: versionCheckTimeout}

// enforceCLIVersion blocks an outdated CLI from running a gated command,
// auto-upgrading and re-execing when possible. It returns nil (allow) in every
// case where it cannot positively confirm the binary is outdated.
func enforceCLIVersion(args []string) error {
	if version == "dev" {
		return nil
	}
	if os.Getenv(versionCheckReexecEnv) != "" {
		return nil
	}
	if isTruthyEnv(os.Getenv(versionCheckSkipEnv)) {
		return nil
	}
	if _, gated := cliVersionGatedCommand(args); !gated {
		return nil
	}

	home := versionCheckHomeDir()
	baseURL := versionCheckBaseURL()
	ttl := versionCheckTTL()
	now := versionCheckNow()

	// Fast path: a fresh cached decision avoids a network round trip on every
	// command. The cache is scoped to the backend that produced it, so a base-URL
	// switch (prod <-> staging) within the TTL re-checks the new backend rather
	// than reusing the other environment's minimum. Only act on the cache; never
	// block solely because it is stale.
	if state, ok := readVersionCheckCache(home); ok && state.BaseURL == baseURL && cliVersionCacheFresh(state, now, ttl) {
		if cliVersionOutdated(version, state.Minimum) {
			return versionCheckUpgrade(version, state.Minimum)
		}
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), versionCheckTimeout)
	defer cancel()
	resp, err := versionCheckFetch(ctx, baseURL, version, cliInstanceID(), cliOSArch())
	if err != nil {
		return nil // fail open — offline or backend down must not brick the CLI
	}
	minimum := strings.TrimSpace(resp.Minimum)
	if minimum == "" {
		return nil
	}
	writeVersionCheckCache(home, versionCheckCacheState{
		CheckedAt: now,
		BaseURL:   baseURL,
		Minimum:   minimum,
		Latest:    strings.TrimSpace(resp.Latest),
	})
	if cliVersionOutdated(version, minimum) {
		return versionCheckUpgrade(version, minimum)
	}
	return nil
}

// cliVersionGatedCommand maps args to the effective command and whether it is
// subject to the version gate. A bare `--runtime ...` root invocation is an
// implicit `run`.
func cliVersionGatedCommand(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	if hasRootRuntimeFlag(args) {
		return "run", true
	}
	cmd := args[0]
	return cmd, cliVersionGatedCommands[cmd]
}

func autoUpgradeAndReexec(current string, minimum string) error {
	fmt.Fprintf(os.Stderr, "\nA required Comment.io CLI update is available: %s → %s (or newer).\n", current, minimum)
	fmt.Fprintln(os.Stderr, "This version is no longer supported. Installing the update automatically…")
	fmt.Fprintf(os.Stderr, "(Set %s=1 to bypass this check.)\n\n", versionCheckSkipEnv)

	ctx, stop := signal.NotifyContext(context.Background(), upgradeShutdownSignals()...)
	defer stop()

	// Upgrade only the CLI binary here, not the persistent daemon. The gate's
	// job is to get the running command onto a supported binary; forcing a
	// daemon reinstall would block simple commands (e.g. `comment docs`/`sync`)
	// on machines where the CLI works but user services are broken or absent.
	// SkipDaemon also means Home/BotletsHome are unused, so the re-exec below
	// still honors the original command's --home/--botlets-home. The daemon is
	// refreshed by the full `comment upgrade` flow.
	result, err := performUpgrade(ctx, upgradeOptions{
		PackageSpec: defaultUpgradePackage(),
		NPM:         "npm",
		SkipDaemon:  true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Automatic update failed: %v\n\n", err)
		fmt.Fprintf(os.Stderr, "Update manually, then re-run your command:\n  npm install -g %s\n", defaultUpgradePackage())
		return cliExitError{Code: 1}
	}

	freshBin := freshBinFromUpgradeResult(result)
	if freshBin == "" {
		fmt.Fprintln(os.Stderr, "Update installed. Re-run your command to continue.")
		return cliExitError{Code: 1}
	}

	fmt.Fprintln(os.Stderr, "Update installed. Continuing with your command…")
	env := append(os.Environ(), versionCheckReexecEnv+"=1")
	if err := reexecComment(freshBin, os.Args, env); err != nil {
		fmt.Fprintf(os.Stderr, "Could not continue automatically: %v\n", err)
		fmt.Fprintln(os.Stderr, "Re-run your command to continue.")
		return cliExitError{Code: 1}
	}
	return nil // unreachable when reexecComment replaces the process
}

func freshBinFromUpgradeResult(result map[string]any) string {
	cli, ok := result["cli"].(map[string]any)
	if !ok {
		return ""
	}
	paths, ok := cli["paths"].(upgradeInstalledPaths)
	if !ok {
		return ""
	}
	if paths.ServiceBin != "" {
		return paths.ServiceBin
	}
	return paths.CommentBin
}

func fetchCLIVersion(ctx context.Context, baseURL string, current string, instanceID string, osArch string) (versionCheckResponse, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/cli/version?current=" + url.QueryEscape(current)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return versionCheckResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Comment-CLI-Version", current)
	if instanceID != "" {
		req.Header.Set("X-Comment-CLI-Id", instanceID)
	}
	if osArch != "" {
		req.Header.Set("X-Comment-CLI-OS-Arch", osArch)
	}
	resp, err := versionCheckHTTPClient.Do(req)
	if err != nil {
		return versionCheckResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return versionCheckResponse{}, fmt.Errorf("cli version check returned status %d", resp.StatusCode)
	}
	var out versionCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return versionCheckResponse{}, err
	}
	return out, nil
}

func versionCheckBaseURL() string {
	return commentsync.DefaultBaseURL()
}

func versionCheckTTL() time.Duration {
	if value := strings.TrimSpace(os.Getenv(versionCheckTTLEnv)); value != "" {
		if d, err := time.ParseDuration(value); err == nil && d >= 0 {
			return d
		}
	}
	return defaultVersionCheckTTL
}

func versionCheckHomeDir() string {
	paths, err := resolveCLIPaths("")
	if err != nil {
		return ""
	}
	return paths.Home
}

// cliInstanceID is a stable, anonymous identifier for this install, derived
// from the hostname and home directory. It carries no PII and lets us count
// distinct installs per version in Axiom without identifying anyone.
func cliInstanceID() string {
	hostname, _ := os.Hostname()
	home := os.Getenv("COMMENT_IO_HOME")
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = userHome
		}
	}
	sum := sha256.Sum256([]byte(hostname + "|" + home))
	return hex.EncodeToString(sum[:8])
}

func cliOSArch() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

func readVersionCheckCache(home string) (versionCheckCacheState, bool) {
	if home == "" {
		return versionCheckCacheState{}, false
	}
	data, err := os.ReadFile(filepath.Join(home, versionCheckCacheFile))
	if err != nil {
		return versionCheckCacheState{}, false
	}
	var state versionCheckCacheState
	if err := json.Unmarshal(data, &state); err != nil {
		return versionCheckCacheState{}, false
	}
	return state, true
}

func writeVersionCheckCache(home string, state versionCheckCacheState) {
	if home == "" {
		return
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(home, versionCheckCacheFile), data, 0o600)
}

func cliVersionCacheFresh(state versionCheckCacheState, now time.Time, ttl time.Duration) bool {
	if state.CheckedAt.IsZero() || strings.TrimSpace(state.Minimum) == "" {
		return false
	}
	if ttl <= 0 {
		return false
	}
	age := now.Sub(state.CheckedAt)
	return age >= 0 && age < ttl
}

// cliVersionOutdated reports whether current is strictly older than minimum.
// Unparseable versions fail open (not outdated) so we never block on garbage.
//
// When the numeric cores match, a prerelease build is treated as older than the
// stable release of the same core, per SemVer precedence: a staging client on
// 0.1.9-alpha.7 pointed at a backend whose minimum is the stable 0.1.9 must
// upgrade. Without this, the stripped cores compare equal and the prerelease
// runs forever against a stricter stable backend.
func cliVersionOutdated(current string, minimum string) bool {
	cur, ok := parseCLIVersion(current)
	if !ok {
		return false
	}
	min, ok := parseCLIVersion(minimum)
	if !ok {
		return false
	}
	if cmp := compareCLIVersionParts(cur, min); cmp != 0 {
		return cmp < 0
	}
	return comparePrerelease(cliVersionPrerelease(current), cliVersionPrerelease(minimum)) < 0
}

// cliVersionPrerelease extracts the SemVer prerelease identifier string (the
// part after the first '-', with any '+build' metadata removed). A stable
// release returns "".
func cliVersionPrerelease(value string) string {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "v")
	trimmed = strings.SplitN(trimmed, "+", 2)[0]
	dash := strings.IndexByte(trimmed, '-')
	if dash < 0 {
		return ""
	}
	return trimmed[dash+1:]
}

// comparePrerelease orders two SemVer prerelease strings per SemVer 2.0.0
// §11.3-11.4. The empty string denotes a stable release, which has higher
// precedence than any prerelease. Returns -1/0/1 for a<b / a==b / a>b.
func comparePrerelease(a string, b string) int {
	if a == b {
		return 0
	}
	if a == "" { // stable release outranks any prerelease
		return 1
	}
	if b == "" {
		return -1
	}
	aIDs := strings.Split(a, ".")
	bIDs := strings.Split(b, ".")
	for i := 0; i < len(aIDs) && i < len(bIDs); i++ {
		if c := comparePrereleaseIdent(aIDs[i], bIDs[i]); c != 0 {
			return c
		}
	}
	// All shared identifiers equal: the longer identifier set wins.
	switch {
	case len(aIDs) < len(bIDs):
		return -1
	case len(aIDs) > len(bIDs):
		return 1
	default:
		return 0
	}
}

// comparePrereleaseIdent compares one dot-separated prerelease identifier.
// Numeric identifiers compare numerically and rank below any alphanumeric
// identifier; alphanumeric identifiers compare lexically in ASCII order.
func comparePrereleaseIdent(a string, b string) int {
	aNum, aIsNum := prereleaseInt(a)
	bNum, bIsNum := prereleaseInt(b)
	switch {
	case aIsNum && bIsNum:
		switch {
		case aNum < bNum:
			return -1
		case aNum > bNum:
			return 1
		default:
			return 0
		}
	case aIsNum:
		return -1 // numeric < alphanumeric
	case bIsNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func prereleaseInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func parseCLIVersion(value string) ([3]int, bool) {
	trimmed := strings.TrimSpace(value)
	trimmed = strings.TrimPrefix(trimmed, "v")
	if trimmed == "" || trimmed == "dev" {
		return [3]int{}, false
	}
	core := strings.SplitN(trimmed, "-", 2)[0]
	core = strings.SplitN(core, "+", 2)[0]
	segments := strings.Split(core, ".")
	var parts [3]int
	for i := 0; i < 3 && i < len(segments); i++ {
		n, err := strconv.Atoi(segments[i])
		if err != nil || n < 0 {
			return [3]int{}, false
		}
		parts[i] = n
	}
	return parts, true
}

func compareCLIVersionParts(a [3]int, b [3]int) int {
	for i := 0; i < 3; i++ {
		if a[i] < b[i] {
			return -1
		}
		if a[i] > b[i] {
			return 1
		}
	}
	return 0
}

func isTruthyEnv(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
