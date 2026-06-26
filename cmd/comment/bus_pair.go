package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
	"github.com/comment-hq/comment-cli/internal/commentsync"
)

// Device-code pairing for the local daemon (daemon-mediated agent enrollment,
// Phase 2). `comment bus pair` starts a pairing exchange against
// POST /daemon/pair/start, prints the verification URL + user code for the
// signed-in owner to approve in the browser, then polls
// POST /daemon/pair/redeem until the daemon token is minted and persists it to
// `<home>/bus/daemon-auth.json` (0600). The token is secret-equivalent and is
// never printed or logged.

const busPairDefaultInterval = 3 * time.Second

// busPairSlowDownPadding is added on top of the server-requested interval when
// the server answers `slow_down`, mirroring the OAuth device-flow convention
// of backing off beyond the advertised cadence.
const busPairSlowDownPadding = 2 * time.Second

// busPairMaxConsecutiveTransportFailures bounds how many back-to-back
// network-level redeem failures the poll loop tolerates before giving up.
// A transport failure is AMBIGUOUS — the redeem may have reached the Worker
// and committed the pairing even though the response was lost — so a single
// failure must never abort the poll (see busPairPollRedeem).
const busPairMaxConsecutiveTransportFailures = 10

// busPairHeartbeatInterval is how often the poll loop reprints a "still
// waiting" progress line while it waits for the user to approve the pairing in
// their browser. Without it the loop polls silently for up to the full
// expiry window (~10m), which reads as a hung install rather than a wait for a
// human action.
const busPairHeartbeatInterval = 30 * time.Second

var (
	busPairHTTPClient = &http.Client{Timeout: 30 * time.Second}
	// busPairSleep is stubbed by tests so polling does not sleep for real.
	busPairSleep = time.Sleep
)

type busPairStartResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

type busPairRedeemResponse struct {
	DaemonToken  string   `json:"daemon_token"`
	DaemonID     string   `json:"daemon_id"`
	OwnerHandle  string   `json:"owner_handle"`
	Label        string   `json:"label"`
	Capabilities []string `json:"capabilities"`
	Error        string   `json:"error"`
	Code         string   `json:"code"`
	Interval     int      `json:"interval"`
}

func runBusPair(args []string) error {
	fs := flag.NewFlagSet("comment bus pair", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	label := fs.String("label", "", "computer name shown in the web app (default: this computer's hostname)")
	baseURL := fs.String("base-url", commentsync.DefaultBaseURL(), "Comment.io base URL")
	force := fs.Bool("force", false, "pair again even if this computer already has daemon credentials")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("bus pair does not accept positional arguments")
	}
	return busPair(os.Stdout, *home, *label, *baseURL, *force)
}

func busPair(out io.Writer, home string, label string, baseURL string, force bool) error {
	paths, err := resolveCLIPaths(home)
	if err != nil {
		return err
	}
	auth, paired, loadErr := commentbus.LoadDaemonAuth(paths)
	if loadErr != nil && !force {
		return fmt.Errorf("existing daemon credentials are unreadable (%s); re-run with --force to replace them", loadErr.Error())
	}
	if paired && !force {
		fmt.Fprintf(out, "already paired as %q (daemon %s)\n", auth.Label, auth.DaemonID)
		fmt.Fprintln(out, "Re-run with --force to pair this computer again.")
		return nil
	}

	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		return errors.New("bus pair requires a base URL")
	}
	if strings.TrimSpace(label) == "" {
		hostname, _ := os.Hostname()
		label = strings.TrimSpace(hostname)
	}

	ctx := context.Background()
	start, err := busPairStart(ctx, base, label)
	if err != nil {
		return err
	}
	expiresIn := time.Duration(start.ExpiresIn) * time.Second
	renderPairingPrompt(out, base, start)
	// The wait status streams as plain prose BELOW the closed panel — the panel
	// is the "what to do" card; this line and the periodic heartbeats from the
	// poll loop are ongoing status, so they read as a consistent stream instead
	// of dangling after the box's bottom rule.
	fmt.Fprintf(out, "Waiting for you to approve it… (the code expires in %s)\n", expiresIn)

	redeemed, err := busPairPollRedeem(ctx, base, start, out)
	if err != nil {
		return err
	}
	savedLabel := redeemed.Label
	if savedLabel == "" {
		savedLabel = label
	}
	// --force over an existing pairing is a local re-pair/reinitialization
	// operation, not an implicit unpair. Do NOT revoke the previous daemon here:
	// self-revoke kills the as_ credentials that daemon minted, and cleaning the
	// enroll-journal-attributed profiles removes the credential files running
	// agent sessions may still need. Instead, after the new daemon auth is
	// saved, mark the old daemon as "replaced" server-side so it leaves
	// auto-install / Botlets-home selection without credential revocation.
	replacesPreviousDaemon := force && paired && loadErr == nil && auth.DaemonID != redeemed.DaemonID
	canMarkPreviousDaemonReplaced := replacesPreviousDaemon && sameBusPairBaseURL(auth.BaseURL, base)
	if replacesPreviousDaemon {
		if err := enrollJournalStampMissingDaemonID(paths, auth.DaemonID); err != nil {
			fmt.Fprintf(out, "Warning: could not mark existing agent profiles for automatic refresh after re-pair (%s). Existing profiles were left in place; if you revoke the previous computer, delete stale profiles manually before relying on auto-install.\n", err.Error())
		}
	}
	if err := commentbus.SaveDaemonAuth(paths, commentbus.DaemonAuth{
		DaemonID:     redeemed.DaemonID,
		Token:        redeemed.DaemonToken,
		BaseURL:      base,
		Label:        savedLabel,
		Capabilities: redeemed.Capabilities,
		PairedAt:     time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		// The token was delivered exactly once and cannot be persisted, so this
		// computer will never be able to use OR revoke it later. Self-revoke NOW
		// with the in-memory token (best-effort) so the server-side daemon does
		// not linger paired-but-unusable in Settings -> Paired computers.
		status, revokeErr := busUnpairSelfRevoke(ctx, commentbus.DaemonAuth{BaseURL: base, Token: redeemed.DaemonToken})
		if revokeErr == nil && status == http.StatusOK {
			return fmt.Errorf("pairing was approved but the daemon credentials could not be saved (%w); the pairing was revoked on the server — fix the problem and run `comment bus pair` again", err)
		}
		return fmt.Errorf("pairing was approved but the daemon credentials could not be saved (%w); revoke this computer in the web app under Settings -> Paired computers, then run `comment bus pair` again", err)
	}
	if canMarkPreviousDaemonReplaced {
		status, replaceErr := busPairReplacePreviousDaemon(ctx, base, commentbus.DaemonAuth{
			DaemonID: redeemed.DaemonID,
			Token:    redeemed.DaemonToken,
		}, auth.DaemonID, auth.Token)
		if replaceErr != nil {
			fmt.Fprintf(out, "Warning: replaced the local daemon pairing for %q (daemon %s) without revoking it, but could not mark it replaced on the server (%s). Existing agent profiles were left in place; Botlets profiles may not refresh through the new daemon until you revoke the previous computer in the web app under Settings -> Paired computers.\n", auth.Label, auth.DaemonID, replaceErr.Error())
		} else if status == http.StatusOK {
			fmt.Fprintf(out, "Marked previous daemon %s (%q) as replaced on the server without revoking it. Existing agent profiles were left in place so running agents keep their credentials; journaled profiles will refresh through the new daemon. Revoke the previous computer in the web app under Settings -> Paired computers after those sessions finish.\n", auth.DaemonID, auth.Label)
		} else {
			fmt.Fprintf(out, "Warning: replaced the local daemon pairing for %q (daemon %s) without revoking it, but the server could not mark it replaced (HTTP %d). Existing agent profiles were left in place; Botlets profiles may not refresh through the new daemon until you revoke the previous computer in the web app under Settings -> Paired computers.\n", auth.Label, auth.DaemonID, status)
		}
	} else if replacesPreviousDaemon {
		previousBase := strings.TrimRight(strings.TrimSpace(auth.BaseURL), "/")
		if previousBase == "" {
			previousBase = "an unknown base URL"
		}
		fmt.Fprintf(out, "Skipped marking previous daemon %s (%q) as replaced because it was paired with %s, but this pairing is for %s. The previous daemon token was not sent to %s; revoke the old computer in its original web app after those sessions finish.\n", auth.DaemonID, auth.Label, previousBase, base, base)
	}
	fmt.Fprintf(out, "\nPaired this computer as %q (daemon %s).\n", savedLabel, redeemed.DaemonID)
	fmt.Fprintln(out, "Manage or revoke paired computers in the web app under Settings -> Paired computers.")
	// The welcome doc is the friendly human next-step. When pairing is driven as
	// one step of a larger installer (e.g. the docker `--with-cli` flow, which
	// then prints ~20 more lines of host CLI + skill install), this line would be
	// buried mid-stream, so the installer sets COMMENT_IO_PAIR_NO_WELCOME=1 and
	// prints the welcome link itself as the final closing banner. Standalone
	// `comment bus pair` leaves the env unset and prints it here. Unknown env is
	// silently ignored by older CLIs, so this stays graceful across version skew.
	if os.Getenv("COMMENT_IO_PAIR_NO_WELCOME") == "" {
		fmt.Fprintf(out, "\nNew to Comment.io? Open your welcome doc and meet Guy, your guide: %s/welcome\n", strings.TrimRight(base, "/"))
	}
	return nil
}

// renderPairingPrompt draws the "approve this in your browser" step as a framed
// action card so it interrupts the install log stream — the structure is what
// makes the eye stop on the one moment that needs a human. The card holds only
// the action and the two things to act on (link + code); the wait status streams
// as plain prose below it (see busPair) so the box closes cleanly on the code.
// Emphasis is deliberately restrained: bold "ACTION REQUIRED" and a bold code,
// no glyph markers or reverse-video badges. The only color is the cyan box rule
// (a border, not text), matching the `comment status` readiness panel. Color
// auto-disables on non-TTY / NO_COLOR / dumb terminals via colorEnabled, so
// piped installs and tests get clean plain text.
func renderPairingPrompt(out io.Writer, base string, start busPairStartResponse) {
	color := colorEnabled(out)
	fmt.Fprintln(out)
	topRule(out, "COMMENT.IO · PAIR THIS COMPUTER", color)
	fmt.Fprintln(out, bar(color))
	fmt.Fprintf(out, "%s   %s — approve this computer in your browser\n", bar(color), paint(color, "ACTION REQUIRED", ansiBold))
	fmt.Fprintf(out, "%s   to link it to %s\n", bar(color), strings.TrimRight(base, "/"))
	fmt.Fprintln(out, bar(color))
	fmt.Fprintf(out, "%s   Open:  %s\n", bar(color), start.VerificationURIComplete)
	fmt.Fprintf(out, "%s   Code:  %s\n", bar(color), paint(color, start.UserCode, ansiBold))
	bottomRule(out, color)
	fmt.Fprintln(out)
}

func sameBusPairBaseURL(a string, b string) bool {
	return strings.TrimRight(strings.TrimSpace(a), "/") == strings.TrimRight(strings.TrimSpace(b), "/")
}

func busPairStart(ctx context.Context, base string, label string) (busPairStartResponse, error) {
	payload := map[string]any{
		"label":       label,
		"platform":    runtime.GOOS,
		"cli_version": version,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return busPairStartResponse{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/daemon/pair/start", bytes.NewReader(body))
	if err != nil {
		return busPairStartResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Comment-CLI-Version", version)
	resp, err := busPairHTTPClient.Do(req)
	if err != nil {
		return busPairStartResponse{}, fmt.Errorf("could not reach %s to start pairing: %w", base, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusTooManyRequests {
		return busPairStartResponse{}, errors.New("too many pairing attempts; wait a minute and run `comment bus pair` again")
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return busPairStartResponse{}, fmt.Errorf("pairing start failed: HTTP %d", resp.StatusCode)
	}
	var start busPairStartResponse
	if err := json.NewDecoder(resp.Body).Decode(&start); err != nil {
		return busPairStartResponse{}, errors.New("pairing start returned an unreadable response")
	}
	if start.DeviceCode == "" || start.UserCode == "" || start.VerificationURIComplete == "" {
		return busPairStartResponse{}, errors.New("pairing start returned an incomplete response")
	}
	if start.Interval <= 0 {
		start.Interval = int(busPairDefaultInterval / time.Second)
	}
	if start.ExpiresIn <= 0 {
		start.ExpiresIn = 600
	}
	return start, nil
}

func busPairPollRedeem(ctx context.Context, base string, start busPairStartResponse, out io.Writer) (busPairRedeemResponse, error) {
	interval := time.Duration(start.Interval) * time.Second
	deadline := time.Now().Add(time.Duration(start.ExpiresIn) * time.Second)
	transportFailures := 0
	// Reprint a progress line every busPairHeartbeatInterval of polling so the
	// wait for the user's browser approval doesn't look like a hang. Driven by
	// accumulated poll-sleep (not wall clock) so it's deterministic under the
	// stubbed sleep in tests.
	sinceHeartbeat := time.Duration(0)
	for {
		if time.Now().After(deadline) {
			return busPairRedeemResponse{}, errors.New("pairing was not approved before the code expired; run `comment bus pair` again")
		}
		busPairSleep(interval)
		sinceHeartbeat += interval
		if sinceHeartbeat >= busPairHeartbeatInterval {
			sinceHeartbeat = 0
			if out != nil {
				if remaining := time.Until(deadline).Round(time.Second); remaining > 0 {
					fmt.Fprintf(out, "  …still waiting for you to approve this computer in your browser (%s left). Open %s and enter code %s.\n", remaining, start.VerificationURIComplete, start.UserCode)
				}
			}
		}
		redeem, status, err := busPairRedeemOnce(ctx, base, start.DeviceCode)
		if err != nil {
			// AMBIGUOUS: the redeem may have reached the Worker and committed the
			// pairing even though we never read the response, and the daemon token
			// is delivered exactly once. Treating this as fatal would drop that
			// token with no recovery, so keep polling: an uncommitted attempt
			// succeeds on a later poll (the server releases a stale redeem claim
			// after ~30s), and a committed one answers 409 below with explicit
			// recovery guidance.
			transportFailures++
			if transportFailures >= busPairMaxConsecutiveTransportFailures {
				return busPairRedeemResponse{}, fmt.Errorf("%w; if the web app already shows this computer under Settings -> Paired computers, revoke it there and run `comment bus pair` again", err)
			}
			continue
		}
		transportFailures = 0
		switch {
		case status == http.StatusOK:
			if redeem.DaemonToken == "" || redeem.DaemonID == "" {
				return busPairRedeemResponse{}, errors.New("pairing redeem returned an incomplete response")
			}
			return redeem, nil
		case status == http.StatusBadRequest && redeem.Error == "authorization_pending":
			if redeem.Interval > 0 {
				interval = time.Duration(redeem.Interval) * time.Second
			}
		case status == http.StatusTooManyRequests:
			// slow_down: back off beyond the server-requested cadence.
			serverInterval := time.Duration(redeem.Interval) * time.Second
			if serverInterval > interval {
				interval = serverInterval
			}
			interval += busPairSlowDownPadding
		case status == http.StatusGone:
			return busPairRedeemResponse{}, errors.New("the pairing code expired before it was approved; run `comment bus pair` again")
		case status == http.StatusConflict:
			// Either another terminal completed the pairing, or an earlier poll
			// from THIS terminal committed it but the response was lost in
			// transit (the token is delivered exactly once and cannot be
			// re-fetched). Name the orphaned-pairing recovery path explicitly.
			return busPairRedeemResponse{}, errors.New("the pairing code was already redeemed; if no other terminal completed this pairing, revoke this computer in the web app under Settings -> Paired computers and run `comment bus pair` again")
		case status == http.StatusNotFound:
			return busPairRedeemResponse{}, errors.New("the server no longer recognizes this pairing code; run `comment bus pair` again")
		case status == http.StatusServiceUnavailable:
			// Retryable server-side hiccup (e.g. daemon registration failed);
			// the code is still live, keep polling.
		default:
			return busPairRedeemResponse{}, fmt.Errorf("pairing failed: HTTP %d", status)
		}
	}
}

func busPairRedeemOnce(ctx context.Context, base string, deviceCode string) (busPairRedeemResponse, int, error) {
	body, err := json.Marshal(map[string]any{"device_code": deviceCode})
	if err != nil {
		return busPairRedeemResponse{}, 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/daemon/pair/redeem", bytes.NewReader(body))
	if err != nil {
		return busPairRedeemResponse{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Comment-CLI-Version", version)
	resp, err := busPairHTTPClient.Do(req)
	if err != nil {
		return busPairRedeemResponse{}, 0, fmt.Errorf("could not reach %s while waiting for pairing approval: %w", base, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	var redeem busPairRedeemResponse
	_ = json.NewDecoder(resp.Body).Decode(&redeem)
	return redeem, resp.StatusCode, nil
}

func busPairReplacePreviousDaemon(ctx context.Context, base string, auth commentbus.DaemonAuth, previousDaemonID string, previousDaemonToken string) (int, error) {
	body, err := json.Marshal(map[string]any{
		"previous_daemon_id":    previousDaemonID,
		"previous_daemon_token": previousDaemonToken,
	})
	if err != nil {
		return 0, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/daemon/replace-previous", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	resp, err := busPairHTTPClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("could not reach %s to mark the previous daemon replaced: %w", base, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode, nil
}

func runBusUnpair(args []string) error {
	fs := flag.NewFlagSet("comment bus unpair", flag.ContinueOnError)
	home := fs.String("home", "", "Comment.io home directory")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(fs.Args()) > 0 {
		return errors.New("bus unpair does not accept positional arguments")
	}
	return busUnpair(os.Stdout, *home, *yes)
}

// busUnpair revokes this computer's daemon server-side (best-effort, via
// POST /daemon/self-revoke with the daemon token — revoking the daemon also
// revokes every generic local credential it minted and closes its in-flight
// enrollments), then deletes the local daemon credentials file.
func busUnpair(out io.Writer, home string, yes bool) error {
	paths, err := resolveCLIPaths(home)
	if err != nil {
		return err
	}
	auth, paired, loadErr := commentbus.LoadDaemonAuth(paths)
	if loadErr == nil && !paired {
		fmt.Fprintln(out, "this computer is not paired - nothing to do")
		return nil
	}
	path := commentbus.DaemonAuthPath(paths)
	if loadErr != nil {
		fmt.Fprintf(out, "The daemon credentials file at %s is unreadable; it will be removed.\n", path)
	} else {
		fmt.Fprintf(out, "Unpairing removes this computer's daemon credentials (%s) for %q (daemon %s).\n", path, auth.Label, auth.DaemonID)
	}
	fmt.Fprintln(out, "Heads up: unpairing also revokes this daemon on the server, so agents enrolled through it stop working on this computer.")
	if !yes {
		if !stdinIsInteractive() {
			return errors.New("refusing to unpair without confirmation; re-run with --yes")
		}
		fmt.Fprint(out, "Unpair this computer? [y/N] ")
		scanner := bufio.NewScanner(os.Stdin)
		answer := ""
		if scanner.Scan() {
			answer = strings.ToLower(strings.TrimSpace(scanner.Text()))
		}
		if answer != "y" && answer != "yes" {
			fmt.Fprintln(out, "unpair cancelled")
			return nil
		}
	}
	// Best-effort server-side self-revoke BEFORE deleting the local file (the
	// token is the only credential that can do it). ONLY a 200 confirms the
	// server revoked anything. A 401/403 is AMBIGUOUS: cf's requireDaemonAuth
	// returns the SAME `INVALID_DAEMON_TOKEN` 401 for an already-revoked daemon
	// AND for a corrupted/stale token that never authenticated — there is no
	// distinct "already revoked" signal (see cf/src/routes/daemon.ts). A token
	// the server rejects is NOT proof the daemon record is gone: it may still be
	// live with credentials we can no longer reach. So treat 401/403 as
	// UNCONFIRMED — do NOT run the credential-revoking profile cleanup (it
	// assumes the server already killed those credentials) and tell the user to
	// revoke in the web app. Any other failure also leaves it unconfirmed.
	revokeConfirmed := false
	if loadErr == nil && strings.TrimSpace(auth.Token) != "" {
		switch status, revokeErr := busUnpairSelfRevoke(context.Background(), auth); {
		case revokeErr != nil:
			fmt.Fprintf(out, "Warning: could not reach the server to revoke this daemon (%s). Revoke it in the web app under Settings -> Paired computers.\n", revokeErr.Error())
		case status == http.StatusOK:
			revokeConfirmed = true
			fmt.Fprintln(out, "Revoked this daemon on the server.")
		case status == http.StatusUnauthorized || status == http.StatusForbidden:
			fmt.Fprintln(out, "Warning: the server rejected this daemon token, so revocation could NOT be confirmed (the token may be stale, or the daemon may already be revoked). If this computer still appears under Settings -> Paired computers, revoke it there to kill its credentials. Enrollment-installed agent profiles were left in place.")
		default:
			fmt.Fprintf(out, "Warning: server-side revocation failed (HTTP %d). Revoke it in the web app under Settings -> Paired computers.\n", status)
		}
	} else if loadErr != nil {
		fmt.Fprintln(out, "Warning: the credentials file is unreadable, so this daemon cannot be revoked from here. Revoke it in the web app under Settings -> Paired computers.")
	}
	// Once the revoke is CONFIRMED, every credential this daemon's enrollments
	// installed is dead — but the agents/*.json profiles holding those as_
	// tokens are still on disk. Left there, they wedge a later re-pair: the
	// owned-agents reconciler sees an existing profile, treats the agent as
	// installed, and never re-enrolls it for fresh credentials. Remove (or
	// restore the pre-enrollment backup of) exactly the profiles the enroll
	// journal attributes to this daemon's enrollments; manual installs have no
	// journal entry and are never touched. Skipped when the revoke was NOT
	// confirmed: those credentials may still be live and the profiles working.
	if revokeConfirmed {
		busUnpairCleanupEnrolledProfiles(out, paths, auth.DaemonID)
	}
	if err := commentbus.DeleteDaemonAuth(paths); err != nil {
		return err
	}
	fmt.Fprintf(out, "Unpaired: removed %s\n", path)
	return nil
}

// busUnpairCleanupEnrolledProfiles removes the agent profiles this daemon's
// enrollments installed (per the enroll journal), restoring pre-enrollment
// backups where they exist. Attribution is verified per file: a profile whose
// credential does not hash-match its journal entry belongs to a later manual
// or re-enrolled install and is left alone. Entries attributed to a different
// non-empty daemon id are also left alone: force re-pair preserves those
// credentials until the old daemon is explicitly revoked elsewhere. Legacy
// entries without daemon attribution are still eligible for cleanup. Entries
// are re-scanned in additional passes because a restored backup can itself be
// an older (journaled) enrollment's install whose credential the revoke also
// killed. Best-effort throughout: unpair must never fail on local cleanup
// trouble.
//
// Journal pruning is per-entry: an entry is dropped only once its cleanup
// succeeds (or it is no longer ours). An entry whose cleanup FAILED — a locked
// or unwritable profile, or a Botlets registry that could not be rewritten — is
// kept (and so is the journal file) so a retry or repair can still tie that
// stale profile to the revoked daemon credential, and the user is told which
// profiles still need cleanup.
func busUnpairCleanupEnrolledProfiles(out io.Writer, paths commentbus.Paths, revokedDaemonID string) {
	entries, err := enrollJournalLoad(paths)
	if err != nil {
		fmt.Fprintf(out, "Warning: could not read the enrollment journal (%s); enrollment-installed agent profiles were left in place. Delete stale files under %s manually before re-pairing.\n", err.Error(), filepath.Join(paths.Home, "agents"))
		return
	}
	revokedDaemonID = strings.TrimSpace(revokedDaemonID)
	removed, restored := 0, 0
	// Track each journal entry's fate so the journal is pruned per-entry rather
	// than wiped wholesale: cleaned entries are dropped, but an entry whose
	// cleanup FAILED (a locked/unwritable profile, or a Botlets registry that
	// could not be rewritten) is kept so a later retry or repair can still find
	// the stale profile tied to this now-revoked daemon credential. failed maps
	// the enrollment id to the profile path still needing attention.
	cleaned := map[string]bool{}
	failed := map[string]string{}
	skippedOtherDaemon := map[string]bool{}
	for pass := 0; pass <= len(entries); pass++ {
		progress := false
		for id, entry := range entries {
			if cleaned[id] {
				continue
			}
			if _, alreadyFailed := failed[id]; alreadyFailed {
				// A genuine cleanup error (locked/unwritable profile or
				// registry) won't be undone by another entry's restore in a
				// later pass, so don't re-attempt it (and don't re-warn).
				continue
			}
			if !enrollJournalEntryMatchesRevokedDaemon(entry, revokedDaemonID) {
				skippedOtherDaemon[id] = true
				continue
			}
			matches, fileExists, indeterminate := enrollJournalProfileSecretMatches(entry)
			if indeterminate {
				// The profile exists but could not be read to attribute it
				// (permissions/transient). Don't prune a possibly-ours revoked
				// install on an unreadable file: keep the entry for a later repair.
				failed[id] = entry.ProfilePath
				fmt.Fprintf(out, "Warning: could not read %s to attribute it; keeping the journal entry to retry\n", entry.ProfilePath)
				continue
			}
			if !fileExists || !matches {
				// EXCEPTION before pruning: an earlier pass (or a prior unpair
				// run) may have removed THIS Botlets install's profile and then
				// failed to rewrite the registry, leaving the entry registry-only
				// incomplete. With the profile gone the secret hash no longer
				// matches, so the plain `!fileExists` gate would misclassify it as
				// "not ours" and prune the only record of the stale registry row
				// (a permanent MISSING_CREDENTIAL_PROFILE). When the file is gone
				// AND the Botlets registry still lists this handle, it is
				// ours-and-incomplete: re-attempt just the registry removal.
				if !fileExists && entry.BotletsHandle != "" {
					exists, existsErr := botletsRegistryEntryExistsForHandle(entry.BotletsHome, entry.BotletsHandle)
					if existsErr != nil {
						failed[id] = entry.ProfilePath
						fmt.Fprintf(out, "Warning: could not check the Botlets registry for %s: %s\n", entry.BotletsHandle, existsErr.Error())
						continue
					}
					if exists {
						if err := removeBotletsRegistryEntryForHandle(entry.BotletsHome, entry.BotletsHandle); err != nil {
							failed[id] = entry.ProfilePath
							fmt.Fprintf(out, "Warning: could not remove the Botlets registry entry for %s: %s\n", entry.BotletsHandle, err.Error())
							continue
						}
						progress = true
						removed++
						delete(failed, id)
						cleaned[id] = true
						continue
					}
				}
				// Not ours anymore (already gone, or reassigned to a later
				// manual/re-enrolled install we must not touch). Nothing to
				// retry — leave it out of `failed` so it is pruned.
				continue
			}
			didRestore, err := restoreOrRemoveEnrollProfile(entry.ProfilePath)
			if err != nil {
				failed[id] = entry.ProfilePath
				fmt.Fprintf(out, "Warning: could not clean up %s: %s\n", entry.ProfilePath, err.Error())
				continue
			}
			if didRestore {
				progress = true
				restored++
				delete(failed, id)
				cleaned[id] = true
				continue
			}
			// Profile removed; roll back the registry entry it upserted. A
			// registry that cannot be rewritten (locked/unwritable) is a partial
			// failure: the profile is gone but the Botlets registry still lists
			// the bot, so keep the journal entry for a retry instead of pruning.
			progress = true
			if entry.BotletsHandle != "" {
				if err := removeBotletsRegistryEntryForHandle(entry.BotletsHome, entry.BotletsHandle); err != nil {
					failed[id] = entry.ProfilePath
					fmt.Fprintf(out, "Warning: could not remove the Botlets registry entry for %s: %s\n", entry.BotletsHandle, err.Error())
					continue
				}
			}
			removed++
			delete(failed, id)
			cleaned[id] = true
		}
		if !progress {
			break
		}
	}
	// Prune the journal to only the entries whose cleanup failed; remove the
	// file entirely when nothing remains. daemon-auth.json is deleted by the
	// caller regardless: the self-revoke already killed these credentials
	// server-side, so a dead token file has no value — the preserved journal
	// entries are the local record a follow-up repair uses to finish cleanup.
	if len(failed) > 0 || len(skippedOtherDaemon) > 0 {
		remaining := make(map[string]enrollJournalEntry, len(failed)+len(skippedOtherDaemon))
		pending := make([]string, 0, len(failed))
		for id, profilePath := range failed {
			remaining[id] = entries[id]
			pending = append(pending, profilePath)
		}
		for id := range skippedOtherDaemon {
			remaining[id] = entries[id]
		}
		sort.Strings(pending)
		if saveErr := enrollJournalSave(paths, remaining); saveErr != nil {
			fmt.Fprintf(out, "Warning: could not rewrite the enrollment journal to keep the %d retained entr(y/ies): %s\n", len(remaining), saveErr.Error())
		}
		if len(pending) > 0 {
			fmt.Fprintf(out, "Note: %d enrollment profile(s) still need cleanup and were kept in the journal for a later retry: %s\n", len(pending), strings.Join(pending, ", "))
		}
	} else if removeErr := os.Remove(enrollJournalPath(paths)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		fmt.Fprintf(out, "Warning: could not remove the enrollment journal: %s\n", removeErr.Error())
	}
	if removed+restored == 0 {
		return
	}
	if errText := agentEnrollmentReloadProfiles(context.Background(), paths, ""); errText != "" {
		fmt.Fprintf(out, "Warning: the daemon did not reload after profile cleanup: %s\n", errText)
	}
	fmt.Fprintf(out, "Cleaned up %d enrollment-installed agent profile(s): %d removed, %d restored to their pre-enrollment contents. Re-pairing will mint fresh credentials for agents that still auto-install here.\n", removed+restored, removed, restored)
}

func enrollJournalEntryMatchesRevokedDaemon(entry enrollJournalEntry, revokedDaemonID string) bool {
	entryDaemonID := strings.TrimSpace(entry.DaemonID)
	return entryDaemonID == "" || (revokedDaemonID != "" && entryDaemonID == revokedDaemonID)
}

// busUnpairSelfRevoke calls POST /daemon/self-revoke with the daemon token.
func busUnpairSelfRevoke(ctx context.Context, auth commentbus.DaemonAuth) (int, error) {
	base := strings.TrimRight(auth.BaseURL, "/")
	if base == "" {
		return 0, errors.New("no base URL in the daemon credentials")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/daemon/self-revoke", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("X-Comment-CLI-Version", version)
	resp, err := busPairHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()
	return resp.StatusCode, nil
}
