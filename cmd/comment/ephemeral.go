package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"crypto/sha256"
	"encoding/hex"

	"github.com/comment-hq/comment-cli/internal/commentbus"
	"github.com/comment-hq/comment-cli/internal/commentsync"
)

// `comment ephemeral ensure` idempotently gives THIS agent session a named,
// session-scoped "ethereal" identity: it reuses an existing live credential
// bound to the session, or mints one via POST /agents/ephemeral (ark_ auth) and
// persists it. It is the reliable, single-command equivalent of the shell
// `ensure-session-identity` helper shipped with the comment-identity skill, and
// uses the SAME on-disk layout (ethereal/<handle>.json + rewake/bind-<session>)
// so the Claude asyncRewake wake-hook and the shell fallback interoperate.
//
// Exit codes mirror the shell helper:
//
//	0  identity ready    — prints "OK <handle> <credfile>" on stdout (with --json, a JSON object). Never prints the secret.
//	2  no ark key        — caller should fall back to anonymous. Not an error.
//	3  no session key     — no stable per-session id; refuses to mint.
//	1  mint/other error   — see stderr.

const (
	ephemeralMintTimeout = 60 * time.Second // bound the mint so a hung holder can't wedge the lock
	ephemeralLockWait    = 90 * time.Second // wait >= mint timeout so a concurrent mint is awaited, not aborted
	ephemeralTTL         = 30 * 24 * time.Hour
	ephemeralRefreshUndr = 7 * 24 * time.Hour
)

var (
	ephemeralEtherealHandleRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*[.]e-[0-9a-f]{8}$`)
	ephemeralSecretRE         = regexp.MustCompile(`^as_[A-Za-z0-9._-]+$`)
	ephemeralSafeKey          = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// ephemeralCred is the on-disk shape under ~/.comment-io/ethereal/<handle>.json.
// It matches what the shell helper and the /listen skill write.
type ephemeralCred struct {
	Handle        string `json:"handle"`
	AgentSecret   string `json:"agent_secret"`
	IdentityClass string `json:"identity_class,omitempty"`
	DisplayName   string `json:"display_name,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	BaseURL       string `json:"base_url,omitempty"`
	Owner         string `json:"owner,omitempty"`
	Session       string `json:"session,omitempty"`
}

func runEphemeral(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Println(ephemeralUsage())
		return nil
	}
	switch args[0] {
	case "ensure":
		return runEphemeralEnsure(args[1:])
	default:
		return fmt.Errorf("unknown ephemeral command %q\n\n%s", args[0], ephemeralUsage())
	}
}

func ephemeralUsage() string {
	return strings.Join([]string{
		"Give this agent session a named, session-scoped ethereal identity.",
		"",
		"Usage:",
		"  comment ephemeral ensure [flags]",
		"",
		"Flags:",
		"  --session KEY    stable per-session id (else COMMENT_IO_SESSION_ID,",
		"                   CODEX_THREAD_ID/CODEX_SESSION_ID, CLAUDE_CODE_SESSION_ID)",
		"  --name NAME      mint with this display name (else the server assigns one)",
		"  --base-url URL   API base (else COMMENT_IO_ENV cascade); must be a Comment.io host",
		"  --home DIR       Comment.io home (else COMMENT_IO_HOME / env cascade)",
		"  --json           print a JSON object instead of the OK line",
		"  --print-secret   also print the agent_secret (trusted non-interactive callers only)",
		"",
		"Exit codes: 0 ready · 2 no ark key (stay anonymous) · 3 no stable session key · 1 error",
	}, "\n")
}

func runEphemeralEnsure(args []string) error {
	fs := flag.NewFlagSet("comment ephemeral ensure", flag.ContinueOnError)
	session := fs.String("session", "", "stable per-session id")
	name := fs.String("name", "", "display name to mint with")
	baseURLFlag := fs.String("base-url", "", "API base URL")
	home := fs.String("home", "", "Comment.io home directory")
	jsonOut := fs.Bool("json", false, "print JSON")
	printSecret := fs.Bool("print-secret", false, "also print the agent_secret")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return fmt.Errorf("ephemeral ensure does not accept positional arguments\n\n%s", ephemeralUsage())
	}

	paths, err := resolveCLIPaths(*home)
	if err != nil {
		return err
	}
	// Resolve the ark key up front (env wins, else config.env). Decides exit 2 vs
	// 3, and also whether the paired-daemon base may be used (no-ark only).
	ark := ephemeralResolveArkKey(paths.Home)

	// Host precedence: explicit --base-url, then — for a NO-ARK session with no
	// explicit base env override — the paired daemon's own host (so it mints
	// against the RIGHT deployment and the reuse base-check matches; the
	// comment.io-vs-comt.dev trap), then the COMMENT_IO_ENV / COMMENT_IO_BASE_URL
	// cascade. The daemon base is deliberately NOT used on the ark path, so an
	// ark_ key is never redirected to a stale paired host.
	rawBase := firstNonEmpty(*baseURLFlag, ephemeralResolveDaemonBase(ark, paths), commentsync.DefaultBaseURL())
	base, baseOK := ephemeralNormalizeBase(rawBase)

	// Resolve a STABLE session key. An unstable key would re-mint every turn.
	sess := firstNonEmpty(*session,
		os.Getenv("COMMENT_IO_SESSION_ID"),
		os.Getenv("CODEX_THREAD_ID"),
		os.Getenv("CODEX_SESSION_ID"),
		os.Getenv("CLAUDE_CODE_SESSION_ID"))
	if sess == "" {
		if ark == "" {
			return cliExitError{Code: 2, Message: "comment ephemeral ensure: no ark key and no session key — staying anonymous (set COMMENT_IO_ARK_KEY and a stable session id for a named identity)."}
		}
		return cliExitError{Code: 3, Message: "comment ephemeral ensure: no stable session key. Set COMMENT_IO_SESSION_ID (or CODEX_THREAD_ID / CLAUDE_CODE_SESSION_ID), or pass --session <stable-id>; do not generate a random one per call."}
	}

	// Refuse to send the ark key (or reuse a credential) for a non-Comment.io
	// base — a mistyped/injected --base-url (or a pasted share URL carrying a
	// token in its path/query) must not exfiltrate the capability.
	if !baseOK {
		// Redact: rawBase may be a pasted share URL whose path/query carries a
		// token — print only the host, never the full URL.
		redacted := "<unparseable>"
		if u, perr := url.Parse(rawBase); perr == nil && u.Hostname() != "" {
			redacted = u.Scheme + "://" + u.Hostname()
		}
		return fmt.Errorf("refusing unapproved/invalid base (host %s) — would expose a credential (approved origins: comment.io, *.comt.dev, *.toofs.us, localhost)", redacted)
	}

	rewakeDir := filepath.Join(paths.Home, "rewake")
	etherealDir := filepath.Join(paths.Home, "ethereal")
	safe := ephemeralSanitizeKey(sess)
	bindFile := filepath.Join(rewakeDir, "bind-"+safe)
	lockFile := filepath.Join(rewakeDir, ".ensure-"+safe+".lock")

	// Harden directory PERMISSIONS before trusting any existing credential: a
	// group/world-writable store lets a local user swap the bind/cred for an
	// attacker token. (Also creates the dirs.) Writability is checked separately,
	// only before minting, so a no-ark read-only store can still go anonymous.
	storeErr := ephemeralHardenDir(rewakeDir)
	if storeErr == nil {
		storeErr = ephemeralHardenDir(etherealDir)
	}
	// Harden PURGES a previously-tamperable store, so a secured store is safe to
	// reuse from. "secure" gates all reuse below.
	secure := storeErr == nil

	// Fast path: reuse a live credential bound to this session — only from a
	// secured store.
	if secure {
		if cred, credPath, ok := ephemeralTryReuse(etherealDir, bindFile, sess, base); ok {
			ephemeralArmClaudeBind(rewakeDir, cred.Handle)
			return ephemeralPrint(*jsonOut, *printSecret, cred, credPath)
		}
	}

	// Minting — via EITHER the ark_ key or the paired daemon — requires a secure
	// (perms) AND writable store. Without one, a NO-ARK session degrades to
	// anonymous (exit 2) rather than hard-failing, while an ark session surfaces
	// the store error.
	mintBlock := storeErr
	if mintBlock == nil {
		mintBlock = ephemeralVerifyWritable(rewakeDir)
	}
	if mintBlock == nil {
		mintBlock = ephemeralVerifyWritable(etherealDir)
	}
	if mintBlock != nil {
		if ark == "" {
			return ephemeralNoMintAnonymous(paths, base)
		}
		return mintBlock
	}

	// Acquire a per-session lock so concurrent invocations don't double-mint —
	// shared by BOTH the ark and daemon mint paths. The server mint is not
	// idempotent per session, so two unlocked first-runs would create duplicate
	// handles and burn the owner's 20/hour mint quota. The reuse callback covers a
	// racer that minted (incl. the daemon writing the session-stamped cred) while
	// we waited; it's gated on `secure` — never reuse from an unsecured store.
	acquired := ephemeralAcquireLock(lockFile, func() (ephemeralCred, string, bool) {
		if !secure {
			return ephemeralCred{}, "", false
		}
		return ephemeralTryReuse(etherealDir, bindFile, sess, base)
	})
	if reuse, reusePath, ok := acquired.reuse, acquired.reusePath, acquired.reused; ok {
		ephemeralArmClaudeBind(rewakeDir, reuse.Handle)
		return ephemeralPrint(*jsonOut, *printSecret, reuse, reusePath)
	}
	if !acquired.held {
		if ark == "" {
			// Contended/stuck lock and no ark key: don't hard-fail a bootstrap —
			// stay anonymous this turn.
			return ephemeralNoMintAnonymous(paths, base)
		}
		return fmt.Errorf("could not acquire mint lock (%s); another mint may be stuck — remove it if stale", lockFile)
	}
	defer os.Remove(lockFile) // we hold it; release on exit (in-process ownership, no PID file needed)

	// Double-check after acquiring: a racer may have minted between checks (only
	// from a secured store — same guard as the fast path).
	if secure {
		if cred, credPath, ok := ephemeralTryReuse(etherealDir, bindFile, sess, base); ok {
			ephemeralArmClaudeBind(rewakeDir, cred.Handle)
			return ephemeralPrint(*jsonOut, *printSecret, cred, credPath)
		}
	}

	// Mint under the lock. No ark key ⇒ the paired daemon mints with its OWN
	// pairing token (no ark_ key on disk); otherwise mint directly with the key.
	if ark == "" {
		if cred, credPath, ok := ephemeralTryDaemonMint(paths, sess, *name, base, etherealDir, bindFile); ok {
			ephemeralArmClaudeBind(rewakeDir, cred.Handle)
			return ephemeralPrint(*jsonOut, *printSecret, cred, credPath)
		}
		// No host pairing reachable — but in the "Sandboxed + CLI" topology the
		// daemon (and its socket + ldt_) live in a Docker container the host can't
		// reach directly. Mint by exec-ing into that container; only the short-lived
		// ephemeral secret crosses back to the host store (the socket/token never do).
		if cred, credPath, ok := ephemeralTryDockerExecMint(sess, *name, base, etherealDir, bindFile); ok {
			ephemeralArmClaudeBind(rewakeDir, cred.Handle)
			return ephemeralPrint(*jsonOut, *printSecret, cred, credPath)
		}
		return ephemeralNoMintAnonymous(paths, base)
	}

	cred, credPath, err := ephemeralMint(base, ark, *name, sess, etherealDir, bindFile)
	if err != nil {
		return err
	}
	ephemeralArmClaudeBind(rewakeDir, cred.Handle)
	return ephemeralPrint(*jsonOut, *printSecret, cred, credPath)
}

// ephemeralNoMintAnonymous is the exit-2 "stay anonymous" result when there is
// no ark key and the paired daemon could not mint (absent/unpaired/unreachable).
func ephemeralNoMintAnonymous(paths commentbus.Paths, base string) error {
	return cliExitError{Code: 2, Message: "comment ephemeral ensure: no ark key (set COMMENT_IO_ARK_KEY or add it to " + filepath.Join(paths.Home, "config.env") + "; reveal one at " + base + "/settings) and no paired daemon to mint via. Staying anonymous."}
}

// ephemeralResolveArkKey returns the owner ark_ registration key from the
// environment, else from <home>/config.env. Never logged.
func ephemeralResolveArkKey(home string) string {
	if key := strings.TrimSpace(os.Getenv("COMMENT_IO_ARK_KEY")); key != "" {
		// Drop it from our own environment so it is not visible via
		// /proc/<pid>/environ for the rest of this (possibly long) run.
		_ = os.Unsetenv("COMMENT_IO_ARK_KEY")
		return key
	}
	data, err := os.ReadFile(filepath.Join(home, "config.env"))
	if err != nil {
		return ""
	}
	var last string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "COMMENT_IO_ARK_KEY="); ok {
			last = strings.TrimSpace(v)
		}
	}
	return last
}

// ephemeralResolveDaemonBase returns the paired daemon's backend host so a
// NO-ARK session mints against the right deployment instead of the comment.io
// default (the daemon mints on its own paired host, so the CLI's base must match
// for the reuse base-check to hold). It backs off in three cases so it never
// overrides an explicit choice:
//   - an ark_ key is present (the ark path must keep using the configured base;
//     redirecting it to a stale paired host would send the key to the wrong host);
//   - COMMENT_IO_BASE_URL / COMMENT_IO_STAGING_BASE_URL is set (a documented base
//     override, honored by commentsync.DefaultBaseURL()).
//
// NOTE: it does NOT gate on COMMENT_IO_ENV — `applyEnvironment` always sets that
// (to "production" by default), so it can't distinguish an explicit choice.
// The result is still narrowed to the approved host set by ephemeralNormalizeBase.
func ephemeralResolveDaemonBase(ark string, paths commentbus.Paths) string {
	if ark != "" {
		return ""
	}
	if strings.TrimSpace(os.Getenv("COMMENT_IO_BASE_URL")) != "" ||
		strings.TrimSpace(os.Getenv("COMMENT_IO_STAGING_BASE_URL")) != "" {
		return ""
	}
	auth, ok, err := commentbus.LoadDaemonAuth(paths)
	if err != nil || !ok {
		return ""
	}
	return strings.TrimSpace(auth.BaseURL)
}

// ephemeralTryDaemonMint asks the locally-paired daemon to mint a session-scoped
// ethereal handle. The daemon mints with its OWN pairing token (no ark_ key on
// disk), persists the secret-bearing cred file itself, and returns only the
// handle + cred path — so the secret never crosses the socket. We read that cred
// back, best-effort write the session bind, and hand it to the caller. Returns
// false (→ caller degrades to anonymous) when there is no reachable/paired
// daemon or the mint failed.
func ephemeralTryDaemonMint(paths commentbus.Paths, sess, name, base, etherealDir, bindFile string) (ephemeralCred, string, bool) {
	// Need a reachable daemon: a live Unix socket, OR the opt-in TCP transport
	// (COMMENT_IO_BUS_TCP_ADDR — the caged-daemon path, where paths.Socket may be
	// empty/absent but callSocket dials BusTCPAddr).
	if strings.TrimSpace(paths.BusTCPAddr) == "" {
		if strings.TrimSpace(paths.Socket) == "" {
			return ephemeralCred{}, "", false
		}
		if _, err := os.Stat(paths.Socket); err != nil {
			return ephemeralCred{}, "", false // no daemon listening
		}
	}
	// The daemon mints on its OWN paired host. Only proceed when that matches the
	// base resolved for this session, so we never burn a mint producing a cred the
	// reuse base-check would then reject (e.g. an explicit --base-url elsewhere).
	dauth, paired, derr := commentbus.LoadDaemonAuth(paths)
	if derr != nil || !paired || !ephemeralSameBase(ephemeralCred{BaseURL: dauth.BaseURL}, base) {
		return ephemeralCred{}, "", false
	}
	// Auth: prefer the owner capability when present — it authorizes the mint over
	// BOTH the Unix socket and the opt-in TCP transport (where nil auth is
	// rejected). Without it, nil auth still works on the UID-gated Unix socket.
	var auth *commentbus.SocketAuth
	if a, aerr := ownerAuth(paths, ""); aerr == nil {
		auth = a
	}
	params := map[string]any{"session": sess}
	if strings.TrimSpace(name) != "" {
		params["display_name"] = name
	}
	timeout := ephemeralMintTimeout + 10*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := callSocket(ctx, paths, "agents.mint-ephemeral", auth, params, timeout)
	if err != nil || !resp.OK {
		return ephemeralCred{}, "", false
	}
	result, _ := resp.Result.(map[string]any)
	handle, _ := result["handle"].(string)
	return ephemeralAcceptDaemonMintedCred(etherealDir, bindFile, handle, sess, base)
}

func ephemeralAcceptDaemonMintedCred(etherealDir, bindFile, handle, sess, base string) (ephemeralCred, string, bool) {
	// Validate before using as a filename (it came over the socket).
	if !ephemeralEtherealHandleOK(handle) {
		return ephemeralCred{}, "", false
	}
	// Read the cred back from OUR OWN store, not the daemon's reported cred_file:
	// in the Docker cage the daemon's home (e.g. /state) differs from the host
	// CLI's bind-mounted COMMENT_IO_HOME, so its absolute path is not valid here.
	credPath := filepath.Join(etherealDir, handle+".json")
	cred, credOK := ephemeralReadCred(credPath)
	if !credOK {
		return ephemeralCred{}, "", false
	}
	if cred.IdentityClass == "" {
		if cred.Handle != handle ||
			sess == "" ||
			cred.Session != sess ||
			!ephemeralEtherealHandleOK(cred.Handle) ||
			!ephemeralSecretRE.MatchString(cred.AgentSecret) ||
			!ephemeralCredLive(cred) ||
			!ephemeralSameBase(cred, base) {
			return ephemeralCred{}, "", false
		}
		cred.IdentityClass = "ethereal"
		data, err := json.Marshal(cred)
		if err != nil || commentbus.WritePrivateFileAtomic(credPath, data, 0o600) != nil {
			return ephemeralCred{}, "", false
		}
	}
	if !ephemeralReusableCred(cred, sess, base, handle) {
		return ephemeralCred{}, "", false
	}
	// Best-effort session bind; reuse also works via the session-stamped cred
	// scan if this never lands.
	_ = commentbus.WritePrivateFileAtomic(bindFile, []byte(cred.Handle), 0o600)
	return cred, credPath, true
}

// ephemeralNormalizeBase parses the host (not glob-matched, so
// https://evil.com/.comt.dev cannot slip through), allows only known Comment.io
// origins over https (plus localhost for dev), rejects userinfo, and returns the
// bare ORIGIN (scheme://host[:port]) — dropping any path/query/fragment so a
// pasted share URL with a token is never sent to the mint endpoint.
func ephemeralNormalizeBase(base string) (string, bool) {
	u, err := url.Parse(base)
	if err != nil || u.User != nil {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	scheme := strings.ToLower(u.Scheme)
	approved := false
	switch {
	case host == "localhost" || host == "127.0.0.1" || host == "::1":
		approved = scheme == "http" || scheme == "https"
	case scheme == "https":
		switch {
		case host == "comment.io", host == "www.comment.io", host == "comt.dev", host == "toofs.us":
			approved = true
		case strings.HasSuffix(host, ".comt.dev"), strings.HasSuffix(host, ".toofs.us"):
			// Note: *.botlets.dev is deliberately NOT accepted — it is a legacy
			// alias that 301-redirects to comt.dev, so a mint POST would break.
			// Use the canonical comt.dev host instead.
			approved = true
		}
	}
	if !approved || u.Host == "" {
		return "", false
	}
	return scheme + "://" + u.Host, true
}

func ephemeralHardenDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}
	// Reject a symlinked store dir — it could redirect into attacker-owned space.
	if lst, lerr := os.Lstat(dir); lerr == nil && lst.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing to store secrets there", dir)
	}
	// POSIX mode bits are meaningful only on Unix; on Windows os.Stat reports
	// synthetic 0777 and Chmod only toggles read-only. The threat is group/world
	// WRITE (a local user planting/replacing the bind/cred), so key on the write
	// bits (0o022); we still chmod to 0700 as defense in depth. (Group/world READ
	// leaks only handle names; the cred JSON itself is 0600.)
	posix := runtime.GOOS != "windows"
	wasWritable := false
	if pre, statErr := os.Stat(dir); statErr == nil && posix {
		wasWritable = pre.Mode().Perm()&0o022 != 0
	}
	_ = os.Chmod(dir, 0o700)
	// Re-check for a symlink AFTER chmod (defence against a swap between the first
	// Lstat and now — mirrors the shell helper's second islink check).
	if lst, lerr := os.Lstat(dir); lerr == nil && lst.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s was swapped to a symlink; refusing to store secrets there", dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	// A dir owned by another local user is insecure at ANY mode — its owner can
	// plant/replace the bind/cred. (Also catches a chmod we couldn't apply.)
	if posix && !ephemeralOwnedByUs(info) {
		return fmt.Errorf("%s is owned by another user; refusing to trust/store secrets there", dir)
	}
	if posix && info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group/world-writable; refusing to store secrets there (chmod 700 it)", dir)
	}
	if wasWritable {
		// The dir was writable by others, so any bind/cred in it could have been
		// planted. PURGE those now that it is 0700, so a later run — seeing the
		// now-secure mode — can't trust a laundered attacker credential. (Locks/
		// temp files are not credential-bearing and are left alone.)
		if entries, e := os.ReadDir(dir); e == nil {
			for _, en := range entries {
				n := en.Name()
				if strings.HasPrefix(n, "bind-") || strings.HasSuffix(n, ".json") {
					_ = os.Remove(filepath.Join(dir, n))
				}
			}
		}
	}
	return nil
}

// ephemeralVerifyWritable confirms we can ACTUALLY persist a one-time secret
// here before minting: a 0700 dir we do not own (e.g. created by sudo) passes
// the perm check but fails the write, consuming an ethereal slot and losing the
// credential. A temp-file probe is the portable way to test writability.
func ephemeralVerifyWritable(dir string) error {
	probe, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return fmt.Errorf("%s is not writable; cannot persist credentials there: %w", dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

// ephemeralSanitizeKey returns a filesystem-safe key. An already-safe key is
// used verbatim (so a Claude UUID matches what the wake hook reads: bind-<id>);
// anything else is hashed (a lossy substitution would collide distinct keys).
func ephemeralSanitizeKey(sess string) string {
	if ephemeralSafeKey.MatchString(sess) {
		return sess
	}
	sum := sha256.Sum256([]byte(sess))
	return "h-" + hex.EncodeToString(sum[:])[:40]
}

func ephemeralCredLive(c ephemeralCred) bool {
	if c.AgentSecret == "" || c.Handle == "" {
		return false
	}
	if c.ExpiresAt == "" {
		return true // server is source of truth
	}
	exp, err := time.Parse(time.RFC3339, strings.Replace(c.ExpiresAt, "Z", "+00:00", 1))
	if err != nil {
		// Try the common JS ISO format with milliseconds.
		exp, err = time.Parse("2006-01-02T15:04:05.000Z07:00", c.ExpiresAt)
		if err != nil {
			return true // unparseable → treat as live
		}
	}
	return exp.After(time.Now())
}

// ephemeralSameBase reports whether a stored credential belongs to the target
// deployment. Trailing slashes are normalized on both sides; a missing base_url
// is anomalous and treated as non-reusable.
func ephemeralSameBase(c ephemeralCred, base string) bool {
	b := strings.TrimRight(c.BaseURL, "/")
	return b != "" && b == strings.TrimRight(base, "/")
}

func ephemeralEtherealHandleOK(handle string) bool {
	return ephemeralEtherealHandleRE.MatchString(handle) && !strings.Contains(handle, "..")
}

func ephemeralReusableCred(c ephemeralCred, sess, base, expectedHandle string) bool {
	if expectedHandle != "" && c.Handle != expectedHandle {
		return false
	}
	if sess == "" || c.Session != sess {
		return false
	}
	if c.IdentityClass != "ethereal" {
		return false
	}
	return ephemeralEtherealHandleOK(c.Handle) &&
		ephemeralSecretRE.MatchString(c.AgentSecret) &&
		ephemeralCredLive(c) &&
		ephemeralSameBase(c, base)
}

func ephemeralReadCred(path string) (ephemeralCred, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ephemeralCred{}, false
	}
	var c ephemeralCred
	if err := json.Unmarshal(data, &c); err != nil {
		return ephemeralCred{}, false
	}
	return c, true
}

// ephemeralTryReuse returns a live credential for this session: the fast path
// follows the bind pointer; the reclaim path scans for a session-stamped cred
// when the bind pointer is missing/stale (so a lost bind never orphans a handle)
// and rebuilds the bind. Refreshes a near-expiry local hint to mirror the server.
func ephemeralTryReuse(etherealDir, bindFile, sess, base string) (ephemeralCred, string, bool) {
	if handle, err := os.ReadFile(bindFile); err == nil {
		h := strings.TrimSpace(string(handle))
		if ephemeralEtherealHandleOK(h) {
			credPath := filepath.Join(etherealDir, h+".json")
			if c, ok := ephemeralReadCred(credPath); ok && ephemeralReusableCred(c, sess, base, h) {
				ephemeralRefreshExpiry(credPath, c)
				return c, credPath, true
			}
		}
	}
	// Reclaim: a cred stamped with this session, still live, but no usable bind.
	matches, _ := filepath.Glob(filepath.Join(etherealDir, "*.json"))
	for _, p := range matches {
		c, ok := ephemeralReadCred(p)
		expected := strings.TrimSuffix(filepath.Base(p), ".json")
		if !ok || !ephemeralReusableCred(c, sess, base, expected) {
			continue
		}
		if err := commentbus.WritePrivateFileAtomic(bindFile, []byte(c.Handle), 0o600); err != nil {
			continue // couldn't rebuild bind; don't claim reuse
		}
		ephemeralRefreshExpiry(p, c)
		return c, p, true
	}
	return ephemeralCred{}, "", false
}

// ephemeralRefreshExpiry pushes a near-expiry local hint out, mirroring the
// server's TTL-extend-on-use (reuse implies the session is active).
func ephemeralRefreshExpiry(path string, c ephemeralCred) {
	if c.ExpiresAt == "" {
		return
	}
	exp, err := time.Parse("2006-01-02T15:04:05.000Z07:00", c.ExpiresAt)
	if err != nil {
		if exp, err = time.Parse(time.RFC3339, c.ExpiresAt); err != nil {
			return
		}
	}
	if time.Until(exp) > ephemeralRefreshUndr {
		return
	}
	c.ExpiresAt = time.Now().UTC().Add(ephemeralTTL).Format("2006-01-02T15:04:05.000Z")
	if data, err := json.Marshal(c); err == nil {
		_ = commentbus.WritePrivateFileAtomic(path, data, 0o600)
	}
}

type ephemeralLockResult struct {
	held      bool
	reused    bool
	reuse     ephemeralCred
	reusePath string
}

// ephemeralAcquireLock takes a per-session lock via an atomic exclusive create
// (WritePrivateFileAtomicNoReplace — os.Link based, cross-platform). On
// contention it steals a STALE lock (older than the wait window — safe because a
// legitimate mint is bounded by ephemeralMintTimeout) race-free via rename, and
// re-checks reuse while waiting so a loser returns the winner's credential. The
// caller releases the lock on exit (in-process ownership; SIGKILL recovery comes
// from age-based stealing, not a PID file).
func ephemeralAcquireLock(lockFile string, reuse func() (ephemeralCred, string, bool)) ephemeralLockResult {
	deadline := time.Now().Add(ephemeralLockWait)
	for {
		// Lock payload "<pid> <epoch>" is the SAME format the shell helper writes,
		// so a mixed CLI/helper rollout interoperates: the helper does pid-liveness
		// + age on our lock (never sees it as malformed), and we age-expire either.
		lockBody := fmt.Sprintf("%d %d", os.Getpid(), time.Now().Unix())
		err := commentbus.WritePrivateFileAtomicNoReplace(lockFile, []byte(lockBody), 0o600)
		if err == nil {
			return ephemeralLockResult{held: true}
		}
		// Steal a stale lock race-free: only the winning rename owns it.
		if ephemeralLockStale(lockFile) {
			steal := lockFile + ".steal." + ephemeralSanitizeKey(fmt.Sprintf("%d", os.Getpid()))
			if os.Rename(lockFile, steal) == nil {
				if ephemeralLockStale(steal) {
					_ = os.Remove(steal) // confirmed stale: drop it
				} else {
					// Turned out live. Restore it ONLY if no new owner has appeared
					// (os.Link fails if lockFile exists) — never clobber a fresh lock,
					// which would let two processes both think they hold it.
					_ = os.Link(steal, lockFile)
					_ = os.Remove(steal)
				}
			}
			continue
		}
		if c, p, ok := reuse(); ok {
			return ephemeralLockResult{reused: true, reuse: c, reusePath: p}
		}
		if time.Now().After(deadline) {
			return ephemeralLockResult{}
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// ephemeralLockStale reports whether a lock is older than the wait window. A
// legitimate mint cannot run that long (bounded by ephemeralMintTimeout), so an
// older lock is a crashed/abandoned holder, safe to steal. Age uses the lock's
// stored timestamp, falling back to its modtime.
func ephemeralLockStale(lockFile string) bool {
	info, err := os.Stat(lockFile)
	if err != nil {
		return false // gone already; not our concern
	}
	// Lock payload is "<pid> <epoch>" (shared with the shell helper). A dead
	// holder is stale immediately (matches the shell, so a mixed deployment
	// agrees); otherwise use the epoch for age (a legitimate mint is bounded by
	// ephemeralMintTimeout < ephemeralLockWait). Fall back to modtime if garbled.
	if data, err := os.ReadFile(lockFile); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 1 {
			if pid, perr := strconv.Atoi(fields[0]); perr == nil && runtime.GOOS != "windows" && !ephemeralPidAlive(pid) {
				return true // holder is dead
			}
		}
		if len(fields) >= 2 {
			if epoch, perr := strconv.ParseInt(fields[1], 10, 64); perr == nil {
				return time.Since(time.Unix(epoch, 0)) > ephemeralLockWait
			}
		}
	}
	return time.Since(info.ModTime()) > ephemeralLockWait
}

// ephemeralContainerMint runs `comment ephemeral ensure --json --print-secret`
// inside the paired daemon's Docker container and returns its stdout. Seam for
// tests (the real impl shells out to `docker exec`). Stderr is discarded so
// container noise never reaches the host's output and no secret can leak there.
var ephemeralContainerMint = func(ctx context.Context, container, sess, name, base string) ([]byte, error) {
	dockerBin, err := exec.LookPath("docker")
	if err != nil {
		return nil, err
	}
	args := []string{"exec", container, "comment", "ephemeral", "ensure",
		"--session", sess, "--base-url", base, "--json", "--print-secret"}
	if strings.TrimSpace(name) != "" {
		args = append(args, "--name", name)
	}
	cmd := exec.CommandContext(ctx, dockerBin, args...)
	cmd.Stderr = io.Discard
	return cmd.Output()
}

// ephemeralTryDockerExecMint mints an identity for the owner by exec-ing into the
// paired daemon's Docker container — the "Sandboxed + CLI" topology, where the
// daemon (and its Unix socket + ldt_ pairing token) live in the container and the
// HOST has no pairing of its own, so ephemeralTryDaemonMint can't reach a socket.
// `docker exec` runs `comment ephemeral ensure` INSIDE the container (which mints
// over its own daemon token, exactly like the native path), and only the
// resulting short-lived ephemeral secret crosses back over stdout to the host
// ethereal store. The daemon socket and pairing token never leave the container.
// The container is resolved deterministically from the origin (comment-agent-<slug>,
// the same name the installer/uninstaller use). Returns ok=false (caller stays
// anonymous) when docker is absent, the container isn't running, or the mint fails.
func ephemeralTryDockerExecMint(sess, name, base, etherealDir, bindFile string) (ephemeralCred, string, bool) {
	container := dockerAgentContainerName(dockerAgentSlug(base))
	ctx, cancel := context.WithTimeout(context.Background(), ephemeralMintTimeout+15*time.Second)
	defer cancel()
	out, err := ephemeralContainerMint(ctx, container, sess, name, base)
	if err != nil {
		return ephemeralCred{}, "", false
	}
	var parsed struct {
		Handle        string `json:"handle"`
		AgentSecret   string `json:"agent_secret"`
		IdentityClass string `json:"identity_class"`
		BaseURL       string `json:"base_url"`
		ExpiresAt     string `json:"expires_at"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &parsed); err != nil {
		return ephemeralCred{}, "", false
	}
	// Validate before trusting: handle becomes a filename, secret a bearer token.
	if !ephemeralSecretRE.MatchString(parsed.AgentSecret) ||
		(parsed.IdentityClass != "" && parsed.IdentityClass != "ethereal") ||
		!ephemeralEtherealHandleOK(parsed.Handle) {
		return ephemeralCred{}, "", false
	}
	// Reject a cred minted against a DIFFERENT origin than requested — a stale or
	// colliding comment-agent-<slug> container must never hand this session a
	// credential for the wrong deployment (e.g. a comment.io as_ to a comt.dev
	// session). Mirrors the daemon path's base guard. An empty base_url (older
	// in-container CLI that doesn't emit it) is trusted to the requested base.
	if strings.TrimSpace(parsed.BaseURL) != "" && !ephemeralSameBase(ephemeralCred{BaseURL: strings.TrimSpace(parsed.BaseURL)}, base) {
		return ephemeralCred{}, "", false
	}
	// Use the origin the container minted against; fall back to the base we
	// targeted. Carry expires_at so the local near-expiry refresh hint works.
	c := ephemeralCred{
		Handle:        parsed.Handle,
		AgentSecret:   parsed.AgentSecret,
		IdentityClass: "ethereal",
		DisplayName:   strings.TrimSpace(name),
		BaseURL:       firstNonEmpty(strings.TrimSpace(parsed.BaseURL), base),
		ExpiresAt:     strings.TrimSpace(parsed.ExpiresAt),
		Session:       sess,
	}
	credPath := filepath.Join(etherealDir, c.Handle+".json")
	data, err := json.Marshal(c)
	if err != nil {
		return ephemeralCred{}, "", false
	}
	if err := commentbus.WritePrivateFileAtomic(credPath, data, 0o600); err != nil {
		return ephemeralCred{}, "", false
	}
	// Bind the session only after the cred exists, mirroring ephemeralMint.
	_ = commentbus.WritePrivateFileAtomic(bindFile, []byte(c.Handle), 0o600)
	return c, credPath, true
}

func ephemeralMint(base, ark, name, sess, etherealDir, bindFile string) (ephemeralCred, string, error) {
	body := map[string]any{}
	if name != "" {
		body["display_name"] = name
	}
	payload, _ := json.Marshal(body)

	ctx, cancel := context.WithTimeout(context.Background(), ephemeralMintTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/agents/ephemeral", bytes.NewReader(payload))
	if err != nil {
		return ephemeralCred{}, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ark)
	req.Header.Set("X-Comment-CLI-Version", version)

	client := &http.Client{Timeout: ephemeralMintTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return ephemeralCred{}, "", fmt.Errorf("mint request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		// Do NOT echo the body — it never contains the secret on error, but be safe.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return ephemeralCred{}, "", fmt.Errorf("mint failed (HTTP %d); check the ark key at %s/settings", resp.StatusCode, base)
	}
	var c ephemeralCred
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return ephemeralCred{}, "", fmt.Errorf("mint response parse failed: %w", err)
	}
	// Validate shapes before trusting them: handle becomes a filename and the
	// secret becomes an Authorization header value.
	if !ephemeralSecretRE.MatchString(c.AgentSecret) || !ephemeralEtherealHandleOK(c.Handle) {
		return ephemeralCred{}, "", errors.New("mint returned a malformed handle/secret")
	}
	c.IdentityClass = "ethereal"
	if c.BaseURL == "" {
		c.BaseURL = base
	}
	c.Session = sess

	credPath := filepath.Join(etherealDir, c.Handle+".json")
	data, err := json.Marshal(c)
	if err != nil {
		return ephemeralCred{}, "", err
	}
	if err := commentbus.WritePrivateFileAtomic(credPath, data, 0o600); err != nil {
		return ephemeralCred{}, "", fmt.Errorf("could not persist credential: %w", err)
	}
	// Only AFTER the cred exists, write the session->handle bind pointer. If this
	// fails, the next call reclaims via the session-stamped scan (no orphan).
	_ = commentbus.WritePrivateFileAtomic(bindFile, []byte(c.Handle), 0o600)
	return c, credPath, nil
}

// ephemeralArmClaudeBind also writes bind-$CLAUDE_CODE_SESSION_ID, which the
// Claude asyncRewake hook reads, so @mentions wake the session even when the
// reuse/lock key was a higher-precedence id (daemon/Codex). No-op off Claude.
func ephemeralArmClaudeBind(rewakeDir, handle string) {
	cc := strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID"))
	if cc == "" {
		return
	}
	// Sanitize before using cc in a filename — an injected CLAUDE_CODE_SESSION_ID
	// like "../../x" would otherwise write outside rewakeDir. A real Claude UUID
	// is all-safe chars so it passes through verbatim (and still matches what the
	// asyncRewake hook reads, bind-<id>).
	cc = ephemeralSanitizeKey(cc)
	bind := filepath.Join(rewakeDir, "bind-"+cc)
	if data, err := os.ReadFile(bind); err == nil && strings.TrimSpace(string(data)) == handle {
		return
	}
	_ = commentbus.WritePrivateFileAtomic(bind, []byte(handle), 0o600)
}

func ephemeralPrint(jsonOut, printSecret bool, c ephemeralCred, credPath string) error {
	if jsonOut {
		out := map[string]any{"handle": c.Handle, "cred_file": credPath, "actor_id": "ai:" + c.Handle}
		if c.IdentityClass != "" {
			out["identity_class"] = c.IdentityClass
		}
		// Emit base_url + expires_at so a programmatic caller (e.g. the host-side
		// docker-exec mint) can persist the authoritative origin + TTL the container
		// minted against, not just re-derive them.
		if c.BaseURL != "" {
			out["base_url"] = c.BaseURL
		}
		if c.ExpiresAt != "" {
			out["expires_at"] = c.ExpiresAt
		}
		if printSecret {
			out["agent_secret"] = c.AgentSecret
		}
		data, _ := json.Marshal(out)
		fmt.Println(string(data))
		return nil
	}
	fmt.Printf("OK %s %s\n", c.Handle, credPath)
	if printSecret {
		fmt.Printf("SECRET %s\n", c.AgentSecret)
	}
	return nil
}
