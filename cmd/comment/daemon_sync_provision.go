package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/comment-hq/comment-cli/internal/commentbus"
	"github.com/comment-hq/comment-cli/internal/commentsync"
)

// errDaemonSyncCapabilityDisabled is returned when /daemon/sync-credential
// reports the pairing's sync capability is off (409 CAPABILITY_DISABLED). It is
// a definitive, actionable state — distinct from a transient mint failure — so
// callers can surface "re-enable sync" rather than retrying or falling back.
var errDaemonSyncCapabilityDisabled = errors.New("sync is disabled on this paired computer")

// One-approval trust (Phase 12): a PAIRED daemon self-provisions the
// read-scoped library-sync credential it needs to project Botlets brains —
// POST /daemon/sync-credential over the pairing token (ldt_). The human
// already approved this exact machine for the strictly more powerful
// agent-install capability when they approved the pairing; a second browser
// approval for the weaker read-only sync grant was pure friction (it also
// lived in an awkward Settings-buried UI). The minted key appears under
// Settings -> Local sync devices, named after the paired computer, and is
// revocable there like any other.

// daemonSyncProvisionMu serializes provisioning: the enrollment worker and
// the team resync worker can both discover the missing config in the same
// window, and two concurrent Logins would race the config/credential writes.
var daemonSyncProvisionMu sync.Mutex

// daemonSyncProvisionHTTPClient is a seam for tests.
var daemonSyncProvisionHTTPClient = &http.Client{Timeout: ownedAgentsRequestTimeout}

// ensureSyncConfiguredViaDaemonSeam lets tests stub the provisioning step.
var ensureSyncConfiguredViaDaemonSeam = ensureSyncConfiguredViaDaemon

// daemonSyncMintedKey caches a minted-but-not-yet-persisted usk_ key (and the
// base URL + daemon TOKEN it was minted against) across provisioning attempts
// in this daemon process. Every POST /daemon/sync-credential mints a NEW live
// key server-side and no daemon-token revoke endpoint exists, so when
// commentsync.Login fails for a persistent LOCAL reason (untrusted root,
// unwritable config) the 30s/4s worker retries would otherwise accumulate one
// fresh credential per pass. Login failures are local-only — the key is not
// consumed server-side — so reusing it on the next attempt is safe. Guarded by
// daemonSyncProvisionMu; cleared once Login persists the key.
//
// The token is part of the cache identity, not just the base URL: a usk_ is
// scoped to the daemon/account that minted it. After a same-origin re-pair or
// `comment bus pair --force` WITHOUT restarting the daemon process, the new
// pairing carries a different token (and possibly a different owner) while the
// old pairing's token may now be revoked — its cached key is dead. Keying only
// by origin would reuse that stale key, Login would persist a dead credential,
// and future provisioning would see sync as "configured" and never mint a fresh
// one. Requiring the token to match forces a fresh mint when the pairing
// identity changes.
var (
	daemonSyncMintedKey      string
	daemonSyncMintedKeyBase  string
	daemonSyncMintedKeyToken string
)

// ensureSyncConfiguredViaDaemon makes local library sync usable, provisioning
// the credential over the daemon pairing token when it is not configured yet.
// No-op when sync is already configured for the daemon's own origin. Never
// prompts.
func ensureSyncConfiguredViaDaemon(ctx context.Context, paths commentbus.Paths, auth commentbus.DaemonAuth) error {
	daemonSyncProvisionMu.Lock()
	defer daemonSyncProvisionMu.Unlock()
	base := strings.TrimRight(strings.TrimSpace(auth.BaseURL), "/")
	if base == "" {
		return fmt.Errorf("daemon auth has no base URL")
	}
	if status, err := commentsync.ReadStatus(commentsync.Options{Home: paths.Home}); err == nil && status.Configured {
		configured := strings.TrimRight(strings.TrimSpace(status.BaseURL), "/")
		if configured == "" || strings.EqualFold(configured, base) {
			return nil
		}
		// Local sync in this Comment.io home mirrors a DIFFERENT origin than
		// the one this daemon is paired to. One home supports exactly one sync
		// origin, and silently re-provisioning would clobber the other
		// product's working sync config — so fail with an actionable error
		// instead of letting Botlets installs search the wrong mirror forever.
		// The error surfaces through the existing retryable failure paths
		// (SYNC_PROVISION_FAILED ack / resync error), so fixing the setup heals
		// without losing the enrollment.
		return fmt.Errorf("local sync in %s is configured for %s, but this daemon is paired to %s; one Comment.io home can sync only one origin — re-run `comment sync login` against %s, or pair/sync this origin under a separate COMMENT_IO_HOME", paths.Home, configured, base, base)
	}
	apiKey := daemonSyncMintedKey
	if apiKey == "" || daemonSyncMintedKeyBase != base || daemonSyncMintedKeyToken != auth.Token {
		minted, err := fetchDaemonSyncCredential(ctx, base, auth.Token)
		if err != nil {
			return err
		}
		apiKey = minted
		daemonSyncMintedKey = minted
		daemonSyncMintedKeyBase = base
		daemonSyncMintedKeyToken = auth.Token
	}
	var err error
	_, err = commentsync.Login(ctx, commentsync.Options{Home: paths.Home, BaseURL: base, APIKey: apiKey})
	if err != nil && (strings.Contains(err.Error(), "sync root belongs to a different account") || strings.Contains(err.Error(), "not managed by comment sync")) {
		// The default root (~/Comment Docs[ (staging)]) is owned by another
		// server, account, or COMMENT_IO_HOME — e.g. this machine also syncs
		// production. Fall back to a host-suffixed root instead of failing
		// the install forever.
		host := base
		if u, parseErr := url.Parse(base); parseErr == nil && u.Host != "" {
			host = u.Host
		}
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return err
		}
		_, err = commentsync.Login(ctx, commentsync.Options{
			Home:    paths.Home,
			Root:    filepath.Join(home, "Comment Docs ("+host+")"),
			BaseURL: base,
			APIKey:  apiKey,
		})
	}
	if err == nil {
		// The key is persisted in the sync config now; drop the in-memory copy.
		daemonSyncMintedKey = ""
		daemonSyncMintedKeyBase = ""
		daemonSyncMintedKeyToken = ""
	}
	return err
}

// daemonSyncUsableForOrigin reports whether local library sync is already
// configured for origin — the same "configured for this origin" predicate
// ensureSyncConfiguredViaDaemon uses for its no-op fast path (an empty
// persisted base URL counts as matching, mirroring that check). The team
// resync worker uses it to decide whether a provisioning failure actually
// blocks Botlets registration: sync configured for the TEAM runtime's origin
// is usable even when the pairing-token provisioning (which targets the
// daemon's own origin) failed.
func daemonSyncUsableForOrigin(home string, origin string) bool {
	status, err := commentsync.ReadStatus(commentsync.Options{Home: home})
	if err != nil || !status.Configured {
		return false
	}
	configured := strings.TrimRight(strings.TrimSpace(status.BaseURL), "/")
	return configured == "" || strings.EqualFold(configured, strings.TrimRight(strings.TrimSpace(origin), "/"))
}

// fetchDaemonSyncCredential mints a fresh scoped usk_ key over the pairing
// token. The key is returned once; the caller persists it via Login.
func fetchDaemonSyncCredential(ctx context.Context, base string, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/daemon/sync-credential", strings.NewReader("{}"))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := daemonSyncProvisionHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusConflict {
		// 409 covers two distinct states: CAPABILITY_DISABLED (sync turned off —
		// a definitive, actionable state the caller should surface) and
		// DAEMON_REPLACED (a stale pairing — inapplicable, the caller should fall
		// back to the browser device flow). Distinguish by the response code so a
		// replaced pairing isn't misreported as "sync is turned off".
		var conflict struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&conflict)
		if conflict.Code == "CAPABILITY_DISABLED" {
			return "", errDaemonSyncCapabilityDisabled
		}
		return "", fmt.Errorf("sync credential request returned status 409 (%s)", conflict.Code)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("sync credential request returned status %d", resp.StatusCode)
	}
	var body struct {
		APIKey string `json:"api_key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("sync credential response unreadable: %w", err)
	}
	if strings.TrimSpace(body.APIKey) == "" {
		return "", fmt.Errorf("sync credential response carried no key")
	}
	return strings.TrimSpace(body.APIKey), nil
}
