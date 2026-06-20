package commentsync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
)

const (
	DefaultRootName = "Comment Docs"
	configVersion   = 1
)

// DefaultBaseURL returns the default Comment.io base URL for the active
// environment (production or staging), honoring the COMMENT_IO_BASE_URL and
// COMMENT_IO_STAGING_BASE_URL overrides.
func DefaultBaseURL() string {
	return commentbus.CurrentEnvironment().DefaultBaseURL()
}

var unsafeFilenameChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

var errLibrarySyncKeyRejected = errors.New("library sync key rejected")

// errLibrarySyncTimeout is a sentinel for transport-level timeouts (context
// deadline exceeded / http.Client.Timeout exceeded) during library-sync HTTP
// calls. A timeout means the server never delivered a response, so the API key
// was never evaluated — callers must NOT report it as a rejected or
// unvalidatable key (bug #523).
var errLibrarySyncTimeout = errors.New("library sync request timed out")

var errLibrarySyncSnapshotIncomplete = errors.New("library sync snapshot incomplete")

type librarySyncKeyRejectedError struct {
	message string
}

func (e librarySyncKeyRejectedError) Error() string {
	return e.message
}

func (e librarySyncKeyRejectedError) Unwrap() error {
	return errLibrarySyncKeyRejected
}

type librarySyncTimeoutError struct {
	message string
}

func (e librarySyncTimeoutError) Error() string {
	return e.message
}

func (e librarySyncTimeoutError) Unwrap() error {
	return errLibrarySyncTimeout
}

type librarySyncSnapshotIncompleteError struct {
	message string
}

func (e librarySyncSnapshotIncompleteError) Error() string {
	return e.message
}

func (e librarySyncSnapshotIncompleteError) Unwrap() error {
	return errLibrarySyncSnapshotIncomplete
}

// classifyLibrarySyncTransportError converts an error returned by
// (*http.Client).Do into a librarySyncTimeoutError when it represents a
// transport timeout (context deadline exceeded or the client's own timeout),
// preserving the original error for unwrapping. Non-timeout transport errors
// are returned unchanged.
func classifyLibrarySyncTransportError(label string, err error) error {
	if err == nil {
		return nil
	}
	if isTransportTimeout(err) {
		return librarySyncTimeoutError{message: fmt.Sprintf("%s timed out before the server responded; check your network connection and retry setup in a moment: %v", label, err)}
	}
	return err
}

// defaultLoginHTTPTimeout is the http.Client timeout for the "Connect this
// computer" / comment sync login snapshot + activation calls. The snapshot
// export can take longer than the previous 30s on cold caches, which surfaced
// as misleading "could not validate the API key" failures (bug #523), so the
// default is raised and made overridable via COMMENT_IO_LOGIN_TIMEOUT.
const defaultLoginHTTPTimeout = 60 * time.Second

// loginHTTPTimeout returns the configured login HTTP client timeout. Set
// COMMENT_IO_LOGIN_TIMEOUT to a Go duration (e.g. "90s", "2m") to override the
// default. Invalid or non-positive values fall back to the default.
func loginHTTPTimeout() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("COMMENT_IO_LOGIN_TIMEOUT")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return defaultLoginHTTPTimeout
}

// Right after a brand-new bot brain is created, its export snapshot may still
// be generating server-side (botletsBrains.partial == true), which makes the
// snapshot's snapshotComplete flag false for a few seconds. That is a known
// transient state — the rejection text itself says "retry setup in a moment" —
// so the login path polls through it with backoff instead of hard-failing on
// the first incomplete snapshot, which used to abort the installer (bug #564).
// Package-level so tests can shrink the timings; a max wait of 0 disables the
// poll and restores the old one-shot behavior.
var (
	librarySyncSnapshotRetryInterval = 2 * time.Second
	librarySyncSnapshotRetryMaxWait  = 60 * time.Second
)

// isTransportTimeout reports whether err represents a transport-level timeout
// rather than a server response. It covers context.DeadlineExceeded,
// os.ErrDeadlineExceeded (net.Conn deadlines surfaced by http.Client.Timeout),
// and any error implementing the net.Error Timeout() interface.
func isTransportTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

type Options struct {
	Home        string
	Root        string
	BaseURL     string
	APIKey      string
	PurgeLocal  bool
	Client      *http.Client
	ValidateKey bool
	// FrontmatterFlavor / LinksFlavor select the sync-root presentation flavor
	// at Login. Empty preserves the existing root's flavor (or plain for a new
	// root). See flavor.go.
	FrontmatterFlavor string
	LinksFlavor       string
}

type Config struct {
	Version          int       `json:"version"`
	BaseURL          string    `json:"base_url"`
	Root             string    `json:"root"`
	Scope            string    `json:"scope"`
	ScopeLabel       string    `json:"scope_label"`
	HumanID          string    `json:"human_id,omitempty"`
	KeyID            string    `json:"key_id,omitempty"`
	ConfigGeneration int       `json:"config_generation"`
	ConfiguredAt     time.Time `json:"configured_at"`
	BackgroundSync   bool      `json:"background_sync"`
	ManualOnly       bool      `json:"manual_only"`
	LiveSyncEnabled  bool      `json:"live_sync_enabled"`
	// FrontmatterFlavor / LinksFlavor select how projected files are
	// presented on disk for this sync root. Empty == "plain" (the legacy
	// HTML-comment header + canonical Markdown body). See flavor.go.
	FrontmatterFlavor string `json:"frontmatter_flavor,omitempty"`
	LinksFlavor       string `json:"links_flavor,omitempty"`
}

type Credentials struct {
	Version   int       `json:"version"`
	APIKey    string    `json:"api_key"`
	CreatedAt time.Time `json:"created_at"`
}

type Status struct {
	Configured            bool     `json:"configured"`
	BackgroundSync        bool     `json:"backgroundSync"`
	ManualOnly            bool     `json:"manualOnly"`
	Home                  string   `json:"home"`
	Root                  string   `json:"root,omitempty"`
	BaseURL               string   `json:"baseUrl,omitempty"`
	ScopeLabel            string   `json:"scopeLabel,omitempty"`
	LastSyncAt            string   `json:"lastSyncAt,omitempty"`
	Documents             int      `json:"documents"`
	Conflicts             int      `json:"conflicts"`
	Unsupported           []string `json:"unsupportedSections,omitempty"`
	SyncCapabilityVersion int      `json:"syncCapabilityVersion"`
	ConfigGeneration      int      `json:"configGeneration,omitempty"`
	LiveSyncEnabled       bool     `json:"liveSyncEnabled"`
	ReloadRequired        bool     `json:"reloadRequired"`
	FrontmatterFlavor     string   `json:"frontmatterFlavor,omitempty"`
	LinksFlavor           string   `json:"linksFlavor,omitempty"`
}

type LogoutResult struct {
	Removed             bool `json:"removed"`
	ServerRevoked       bool `json:"serverRevoked"`
	PurgedLocal         bool `json:"purgedLocal,omitempty"`
	ProjectionsRemoved  int  `json:"projectionsRemoved,omitempty"`
	RecoveriesPreserved int  `json:"recoveriesPreserved,omitempty"`
}

type OnceResult struct {
	Root                string   `json:"root"`
	SnapshotID          string   `json:"snapshotId"`
	ScopeLabel          string   `json:"scopeLabel"`
	DocumentsWritten    int      `json:"documentsWritten"`
	DocumentsRemoved    int      `json:"documentsRemoved"`
	RecoveriesPreserved int      `json:"recoveriesPreserved"`
	UnsupportedSections []string `json:"unsupportedSections"`
}

type RecoveryScanResult struct {
	Checked             int `json:"checked"`
	ProjectionRefreshes int `json:"projectionRefreshes"`
	SnapshotRefreshes   int `json:"snapshotRefreshes"`
	RecoveriesPreserved int `json:"recoveriesPreserved"`
	NotModified         int `json:"notModified"`
}

type RecoveryItem struct {
	ID           string `json:"id"`
	VisibleID    string `json:"visibleInstanceId"`
	Slug         string `json:"slug,omitempty"`
	OriginalPath string `json:"originalPath"`
	ArtifactPath string `json:"artifactPath"`
	Reason       string `json:"reason"`
	Status       string `json:"status"`
	CreatedAt    string `json:"createdAt"`
}

type RecoverAction string

const (
	RecoverActionShow               RecoverAction = "show"
	RecoverActionDiff               RecoverAction = "diff"
	RecoverActionCopyNextToOriginal RecoverAction = "copy-next-to-original"
	RecoverActionDiscard            RecoverAction = "discard"
)

type RecoverResult struct {
	ID           string `json:"id"`
	ArtifactPath string `json:"artifactPath,omitempty"`
	OutputPath   string `json:"outputPath,omitempty"`
	Diff         string `json:"diff,omitempty"`
	Discarded    bool   `json:"discarded,omitempty"`
}

type snapshotResponse struct {
	SnapshotID          string            `json:"snapshotId"`
	ScopeLabel          string            `json:"scopeLabel"`
	CoveredSections     []snapshotSection `json:"coveredSections"`
	UnsupportedSections []snapshotSection `json:"unsupportedSections"`
	SnapshotComplete    bool              `json:"snapshotComplete"`
	Rows                []snapshotRow     `json:"rows"`
	PageInfo            struct {
		NextCursor *string `json:"nextCursor"`
		Partial    bool    `json:"partial"`
	} `json:"pageInfo"`
}

type snapshotSection struct {
	ID            string `json:"id"`
	Label         string `json:"label"`
	Covered       bool   `json:"covered"`
	Authoritative bool   `json:"authoritative"`
	Count         int    `json:"count"`
	Reason        string `json:"reason,omitempty"`
}

type snapshotRow struct {
	VisibleInstanceID        string  `json:"visibleInstanceId"`
	Section                  string  `json:"section"`
	Kind                     string  `json:"kind"`
	Name                     string  `json:"name"`
	ParentVisibleInstance    *string `json:"parentVisibleInstanceId"`
	DocSlug                  string  `json:"docSlug,omitempty"`
	SourceLabel              *string `json:"sourceLabel,omitempty"`
	Revision                 *int    `json:"revision,omitempty"`
	ContentHash              string  `json:"contentHash,omitempty"`
	LastContentModifiedAt    string  `json:"lastContentModifiedAt,omitempty"`
	BotletsOwnerHandle       string  `json:"botletsOwnerHandle,omitempty"`
	BotletsBotSlug           string  `json:"botletsBotSlug,omitempty"`
	BotletsBotLocalName      string  `json:"botletsBotLocalName,omitempty"`
	BotletsBotID             string  `json:"botletsBotId,omitempty"`
	BotletsBotHandle         string  `json:"botletsBotHandle,omitempty"`
	BotletsBotAgentID        string  `json:"botletsBotAgentId,omitempty"`
	BotletsBrainContainerID  string  `json:"botletsBrainContainerId,omitempty"`
	BotletsBrainRootFolderID string  `json:"botletsBrainRootFolderId,omitempty"`
	BotletsBrainNodeID       string  `json:"botletsBrainNodeId,omitempty"`
}

type projectionResponse struct {
	Slug         string  `json:"slug"`
	Title        *string `json:"title"`
	Markdown     string  `json:"markdown"`
	Revision     int     `json:"revision"`
	ContentHash  string  `json:"content_hash"`
	ETag         string  `json:"etag"`
	CanonicalURL string  `json:"canonical_url"`
}

type DeviceLoginSession struct {
	UserCode        string `json:"userCode"`
	DeviceCode      string `json:"deviceCode"`
	VerificationURI string `json:"verificationUri"`
	ExpiresAt       string `json:"expiresAt"`
	Interval        int    `json:"interval"`
	ScopeLabel      string `json:"scopeLabel"`
	BaseURL         string `json:"baseUrl"`
}

type devicePollResponse struct {
	Status    string `json:"status"`
	Interval  int    `json:"interval,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"`
	APIKey    string `json:"api_key,omitempty"`
}

type placementMeta struct {
	VisibleInstanceID        string    `json:"visibleInstanceId"`
	Slug                     string    `json:"slug"`
	Section                  string    `json:"section,omitempty"`
	Path                     string    `json:"path"`
	CanonicalPath            string    `json:"canonicalPath,omitempty"`
	ContentHash              string    `json:"contentHash"`
	BodyContentHash          string    `json:"bodyContentHash,omitempty"`
	RenderedProjectionHash   string    `json:"renderedProjectionHash,omitempty"`
	ProjectionFormatVersion  int       `json:"projectionFormatVersion,omitempty"`
	FrontmatterFlavor        string    `json:"frontmatterFlavor,omitempty"`
	LinksFlavor              string    `json:"linksFlavor,omitempty"`
	ETag                     string    `json:"etag,omitempty"`
	Revision                 int       `json:"revision"`
	LastSeenSnapshot         string    `json:"lastSeenSnapshot,omitempty"`
	UpdatedAt                time.Time `json:"updatedAt"`
	BotletsOwnerHandle       string    `json:"botletsOwnerHandle,omitempty"`
	BotletsBotSlug           string    `json:"botletsBotSlug,omitempty"`
	BotletsBotLocalName      string    `json:"botletsBotLocalName,omitempty"`
	BotletsBotID             string    `json:"botletsBotId,omitempty"`
	BotletsBotHandle         string    `json:"botletsBotHandle,omitempty"`
	BotletsBotAgentID        string    `json:"botletsBotAgentId,omitempty"`
	BotletsBrainContainerID  string    `json:"botletsBrainContainerId,omitempty"`
	BotletsBrainRootFolderID string    `json:"botletsBrainRootFolderId,omitempty"`
	BotletsBrainNodeID       string    `json:"botletsBrainNodeId,omitempty"`
}

func Login(ctx context.Context, opts Options) (Config, error) {
	paths, err := resolvePaths(opts.Home)
	if err != nil {
		return Config{}, err
	}
	root, err := resolveRoot(opts.Root)
	if err != nil {
		return Config{}, err
	}
	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL()
	}
	apiKey := strings.TrimSpace(opts.APIKey)
	if apiKey == "" {
		return Config{}, errors.New("missing library sync API key")
	}
	if !strings.HasPrefix(apiKey, "usk_v2.") {
		return Config{}, errors.New("library sync requires a scoped usk_v2 key; copy a fresh setup command or run comment sync login again")
	}
	keyInfo, err := parseScopedKey(apiKey)
	if err != nil {
		return Config{}, fmt.Errorf("%w; copy a fresh setup command or run comment sync login again", err)
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: loginHTTPTimeout()}
	}
	if opts.ValidateKey {
		if err := validateLibrarySyncAPIKey(ctx, client, baseURL, apiKey); err != nil {
			return Config{}, err
		}
	}
	lock, err := acquireSyncLock(paths)
	if err != nil {
		return Config{}, err
	}
	defer lock.Close()
	if err := ensurePrivateDirs(paths); err != nil {
		return Config{}, err
	}
	if err := ensureSafeRoot(root); err != nil {
		return Config{}, err
	}
	previous, previousErr := readConfig(paths)
	if previousErr != nil && !errors.Is(previousErr, os.ErrNotExist) {
		return Config{}, previousErr
	}
	generation := 1
	if previousErr == nil {
		generation = previous.ConfigGeneration + 1
	}
	flavor := resolveLoginFlavor(opts, previous, previousErr == nil)
	cfg := Config{
		Version:           configVersion,
		BaseURL:           baseURL,
		Root:              root,
		Scope:             "library-sync:read:botlets-brains",
		ScopeLabel:        "My Files, Shared With Me, Team Wiki, and Botlets brains",
		HumanID:           keyInfo.agentID,
		KeyID:             keyInfo.keyID,
		ConfigGeneration:  generation,
		ConfiguredAt:      time.Now().UTC(),
		BackgroundSync:    false,
		ManualOnly:        true,
		LiveSyncEnabled:   false,
		FrontmatterFlavor: nonPlainFlavorValue(flavor.Frontmatter, flavorFrontmatterPlain),
		LinksFlavor:       nonPlainFlavorValue(flavor.Links, flavorLinksPlain),
	}
	creds := Credentials{Version: configVersion, APIKey: apiKey, CreatedAt: cfg.ConfiguredAt}
	if err := validateRootOwnership(root, cfg, paths); err != nil {
		return Config{}, err
	}
	if reset, err := projectionStateNeedsReset(ctx, paths, root, keyInfo.agentID, previous, previousErr); err != nil {
		return Config{}, err
	} else if reset {
		if err := resetProjectionState(ctx, paths); err != nil {
			return Config{}, err
		}
	}
	if err := writeJSON0600(filepath.Join(paths.Home, "sync", "config.json"), cfg); err != nil {
		return Config{}, err
	}
	if err := writeJSON0600(filepath.Join(paths.Home, "sync", "credentials.json"), creds); err != nil {
		return Config{}, err
	}
	if opts.ValidateKey {
		if err := activateLibrarySyncAPIKey(ctx, client, baseURL, apiKey); err != nil {
			return Config{}, err
		}
	}
	return cfg, nil
}

func validateLibrarySyncAPIKey(ctx context.Context, client *http.Client, baseURL, apiKey string) error {
	deadline := time.Now().Add(librarySyncSnapshotRetryMaxWait)
	for {
		snapshot, _, err := fetchSnapshotWithPages(ctx, client, baseURL, apiKey)
		if err != nil {
			if errors.Is(err, errLibrarySyncTimeout) {
				return fmt.Errorf("library sync login timed out before the server responded — the API key was not validated; check your connection and run comment sync login again to retry: %w", err)
			}
			if errors.Is(err, errLibrarySyncKeyRejected) {
				return fmt.Errorf("library sync login rejected the API key: %w", err)
			}
			if errors.Is(err, errLibrarySyncSnapshotIncomplete) {
				return errors.New("library sync login rejected the API key because the export snapshot is not complete; retry setup in a moment")
			}
			return fmt.Errorf("library sync login could not validate the API key: %w", err)
		}
		if snapshot.SnapshotComplete {
			for _, section := range snapshot.CoveredSections {
				if section.Covered && !section.Authoritative {
					reason := strings.TrimSpace(section.Reason)
					if reason != "" {
						return fmt.Errorf("library sync login rejected the API key because section %q is not authoritative: %s", section.Label, reason)
					}
					return fmt.Errorf("library sync login rejected the API key because section %q is not authoritative", section.Label)
				}
			}
			return nil
		}
		// Snapshot not complete yet. This is the transient brain-creation race
		// (bug #564): the freshly-minted brain's export is still generating.
		// Poll with backoff until the deadline before giving up, rather than
		// hard-failing the installer on the first incomplete read.
		if !time.Now().Before(deadline) {
			return errors.New("library sync login rejected the API key because the export snapshot is not complete; retry setup in a moment")
		}
		timer := time.NewTimer(librarySyncSnapshotRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func activateLibrarySyncAPIKey(ctx context.Context, client *http.Client, baseURL, apiKey string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/auth/library-sync/current-device/activate", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := client.Do(req)
	if err != nil {
		if timeoutErr := classifyLibrarySyncTransportError("library sync activation request", err); errors.Is(timeoutErr, errLibrarySyncTimeout) {
			return fmt.Errorf("library sync login timed out before the server responded — the device was not activated; check your connection and run comment sync login again to retry: %w", timeoutErr)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		bodyText := strings.TrimSpace(string(body))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return librarySyncKeyRejectedError{message: fmt.Sprintf("library sync login could not activate the persisted API key (HTTP %d): %s. The key may be expired, revoked, or copied from an old setup command; copy a fresh setup command or run comment sync login again", resp.StatusCode, bodyText)}
		}
		return fmt.Errorf("library sync login could not activate the persisted API key: HTTP %d %s", resp.StatusCode, bodyText)
	}
	return nil
}

func StartDeviceLogin(ctx context.Context, opts Options) (DeviceLoginSession, error) {
	root, err := resolveRoot(opts.Root)
	if err != nil {
		return DeviceLoginSession{}, err
	}
	baseURL := strings.TrimRight(opts.BaseURL, "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL()
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	body := strings.NewReader(fmt.Sprintf(
		`{"device_id":"%s","device_label":"%s","root_label":"%s","sync_version":2,"client_capabilities":["botlets-brains"]}`,
		jsonEscape(librarySyncDeviceID(root)),
		jsonEscape(defaultDeviceLabel()),
		jsonEscape(root),
	))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/auth/library-sync/device-codes", body)
	if err != nil {
		return DeviceLoginSession{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return DeviceLoginSession{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return DeviceLoginSession{}, fmt.Errorf("device login start failed: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var session DeviceLoginSession
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return DeviceLoginSession{}, err
	}
	session.BaseURL = baseURL
	if strings.HasPrefix(session.VerificationURI, "/") {
		session.VerificationURI = baseURL + session.VerificationURI
	}
	if session.Interval <= 0 {
		session.Interval = 3
	}
	return session, nil
}

func PollDeviceLogin(ctx context.Context, opts Options, session DeviceLoginSession) (string, error) {
	baseURL := strings.TrimRight(firstNonEmpty(session.BaseURL, opts.BaseURL, DefaultBaseURL()), "/")
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	interval := time.Duration(session.Interval) * time.Second
	if interval <= 0 {
		interval = 3 * time.Second
	}
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		payload := strings.NewReader(fmt.Sprintf(`{"deviceCode":"%s"}`, jsonEscape(session.DeviceCode)))
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/auth/library-sync/device-codes/"+session.UserCode+"/poll", payload)
		if err != nil {
			return "", err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		var poll devicePollResponse
		decodeErr := json.NewDecoder(resp.Body).Decode(&poll)
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && decodeErr == nil && poll.Status == "approved" && poll.APIKey != "" {
			return poll.APIKey, nil
		}
		if resp.StatusCode == http.StatusGone {
			return "", errors.New("device login code expired")
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusTooManyRequests {
			return "", fmt.Errorf("device login poll failed: HTTP %d", resp.StatusCode)
		}
		if poll.Interval > 0 {
			interval = time.Duration(poll.Interval) * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
}

type scopedKeyParts struct {
	agentID string
	keyID   string
}

func parseScopedKey(key string) (scopedKeyParts, error) {
	parts := strings.Split(key, ".")
	if len(parts) != 4 || parts[0] != "usk_v2" || parts[1] == "" || parts[2] == "" || parts[3] == "" {
		return scopedKeyParts{}, errors.New("library sync API key must be usk_v2.<agentId>.<keyId>.<secret>")
	}
	return scopedKeyParts{agentID: parts[1], keyID: parts[2]}, nil
}

func Logout(ctx context.Context, opts Options) (LogoutResult, error) {
	paths, err := resolvePaths(opts.Home)
	if err != nil {
		return LogoutResult{}, err
	}
	lock, err := acquireSyncLock(paths)
	if err != nil {
		return LogoutResult{}, err
	}
	defer lock.Close()
	cfg, cfgErr := readConfig(paths)
	creds, credsErr := readCredentials(paths)
	result := LogoutResult{}
	if opts.PurgeLocal && cfgErr != nil {
		if errors.Is(cfgErr, os.ErrNotExist) {
			return result, errors.New("cannot purge local projections because library sync is not configured")
		}
		return result, cfgErr
	}
	if cfgErr == nil && credsErr == nil {
		client := opts.Client
		if client == nil {
			client = &http.Client{Timeout: 30 * time.Second}
		}
		if err := revokeCurrentDevice(ctx, client, cfg.BaseURL, creds.APIKey); err == nil {
			result.ServerRevoked = true
		}
	}
	if opts.PurgeLocal {
		purge, err := purgeLocalProjections(ctx, paths, cfg.Root)
		if err != nil {
			return result, err
		}
		result.PurgedLocal = true
		result.ProjectionsRemoved = purge.removed
		result.RecoveriesPreserved = purge.recovered
	}
	for _, path := range []string{
		filepath.Join(paths.Home, "sync", "credentials.json"),
		filepath.Join(paths.Home, "sync", "config.json"),
	} {
		if err := os.Remove(path); err == nil {
			result.Removed = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	}
	return result, nil
}

type localPurgeResult struct {
	removed   int
	recovered int
}

func purgeLocalProjections(ctx context.Context, paths commentbus.Paths, root string) (localPurgeResult, error) {
	if strings.TrimSpace(root) == "" {
		return localPurgeResult{}, errors.New("cannot purge local projections without a sync root")
	}
	root = filepath.Clean(root)
	if info, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return localPurgeResult{}, resetProjectionState(ctx, paths)
	} else if err != nil {
		return localPurgeResult{}, err
	} else if info.Mode()&os.ModeSymlink != 0 {
		return localPurgeResult{}, fmt.Errorf("sync root refuses symlink: %s", root)
	} else if !info.IsDir() {
		return localPurgeResult{}, fmt.Errorf("sync root is not a directory: %s", root)
	}
	state, err := openSyncState(ctx, paths)
	if err != nil {
		return localPurgeResult{}, err
	}
	defer state.Close()
	placements, err := state.listPlacements(ctx)
	if err != nil {
		return localPurgeResult{}, err
	}
	result := localPurgeResult{}
	for _, placement := range placements {
		if placement.Path == "" {
			continue
		}
		if err := validateManagedPath(root, placement.Path); err != nil {
			return result, err
		}
		bytes, readErr := os.ReadFile(placement.Path)
		if errors.Is(readErr, os.ErrNotExist) {
			continue
		}
		if readErr != nil {
			return result, readErr
		}
		if placementBodyContentHash(placement) == "" || placementLocallyDirty(bytes, placement) {
			if err := preserveRecovery(ctx, paths, state, placement.VisibleInstanceID, placement.Slug, placement.Path, "local_dirty_before_purge", bytes); err != nil {
				return result, err
			}
			result.recovered++
			continue
		}
		op, err := state.beginOp(ctx, "purge_projection", placement.VisibleInstanceID, placement.Slug, placement.Path, placement.LastSeenSnapshot)
		if err != nil {
			return result, err
		}
		if err := os.Remove(placement.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = state.finishOp(ctx, op, "failed", err)
			return result, err
		}
		if err := state.finishOp(ctx, op, "complete", nil); err != nil {
			return result, err
		}
		result.removed++
	}
	if err := state.resetProjectionState(ctx); err != nil {
		return result, err
	}
	for _, path := range []string{
		filepath.Join(paths.Home, "sync", "metadata", "placements"),
		filepath.Join(paths.Home, "sync", "ops"),
	} {
		if err := os.RemoveAll(path); err != nil {
			return result, err
		}
	}
	for _, path := range []string{
		filepath.Join(paths.Home, "sync", "last-sync-at"),
		filepath.Join(paths.Home, "sync", "unsupported-sections.json"),
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
	}
	if err := removeGeneratedRootFiles(root); err != nil {
		return result, err
	}
	removeEmptySyncDirs(root)
	return result, nil
}

func removeGeneratedRootFiles(root string) error {
	readmePath := filepath.Join(root, "README.md")
	if data, err := os.ReadFile(readmePath); err == nil {
		content := string(data)
		if content == rootReadmeContent() || content == rootReadmeContentWithRecover() {
			if err := os.Remove(readmePath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	markerPath := filepath.Join(root, ".comment-sync-root.json")
	var marker map[string]any
	if err := readJSON(markerPath, &marker); err == nil {
		if marker["managed_by"] == "comment sync" {
			if err := os.Remove(markerPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := removeGeneratedLocalAgentDocs(root); err != nil {
		return err
	}
	return nil
}

func removeGeneratedLocalAgentDocs(root string) error {
	markerPath := localAgentDocsMarkerPath(root)
	if err := validateManagedPath(root, markerPath); err != nil {
		return err
	}
	var marker map[string]any
	err := readJSON(markerPath, &marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if marker["managed_by"] != "comment sync" || marker["kind"] != "local-agent-docs" {
		return nil
	}
	for _, name := range append(generatedLocalAgentDocNames(), localAgentDocsMarkerName) {
		target := filepath.Join(root, localAgentDocsDirName, name)
		if err := validateManagedPath(root, target); err != nil {
			return err
		}
		if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	_ = os.Remove(filepath.Join(root, localAgentDocsDirName))
	return nil
}

func removeEmptySyncDirs(root string) {
	for _, top := range []string{
		filepath.Join(root, "My Files"),
		filepath.Join(root, "Shared With Me"),
		filepath.Join(root, "Team Wiki"),
		filepath.Join(root, "Botlets"),
	} {
		removeEmptyDirTree(top, root)
	}
}

func removeEmptyDirTree(dir, stop string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			removeEmptyDirTree(filepath.Join(dir, entry.Name()), stop)
		}
	}
	if filepath.Clean(dir) == filepath.Clean(stop) {
		return
	}
	_ = os.Remove(dir)
}

func projectionStateNeedsReset(ctx context.Context, paths commentbus.Paths, root string, agentID string, previous Config, previousErr error) (bool, error) {
	if previousErr == nil {
		return filepath.Clean(previous.Root) != filepath.Clean(root) || previous.HumanID != agentID, nil
	}
	if !errors.Is(previousErr, os.ErrNotExist) {
		return false, previousErr
	}
	escaped, err := projectionStateEscapesRoot(ctx, paths, root)
	if err != nil {
		return false, err
	}
	return escaped, nil
}

func projectionStateEscapesRoot(ctx context.Context, paths commentbus.Paths, root string) (bool, error) {
	state, err := openSyncState(ctx, paths)
	if err != nil {
		return false, err
	}
	defer state.Close()
	placements, err := state.listPlacements(ctx)
	if err != nil {
		return false, err
	}
	for _, placement := range placements {
		if placement.Path != "" && !pathWithinRoot(root, placement.Path) {
			return true, nil
		}
	}
	ops, err := state.listPendingOps(ctx)
	if err != nil {
		return false, err
	}
	for _, op := range ops {
		if op.Path != "" && !pathWithinRoot(root, op.Path) {
			return true, nil
		}
	}
	matches, err := filepath.Glob(filepath.Join(paths.Home, "sync", "metadata", "placements", "*.json"))
	if err != nil {
		return false, err
	}
	for _, path := range matches {
		var placement placementMeta
		if err := readJSON(path, &placement); err != nil {
			continue
		}
		if placement.Path != "" && !pathWithinRoot(root, placement.Path) {
			return true, nil
		}
	}
	return false, nil
}

func resetProjectionState(ctx context.Context, paths commentbus.Paths) error {
	state, err := openSyncState(ctx, paths)
	if err != nil {
		return err
	}
	if err := state.resetProjectionState(ctx); err != nil {
		_ = state.Close()
		return err
	}
	if err := state.Close(); err != nil {
		return err
	}
	for _, dir := range []string{
		filepath.Join(paths.Home, "sync", "metadata", "placements"),
		filepath.Join(paths.Home, "sync", "ops"),
	} {
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func ReadStatus(opts Options) (Status, error) {
	paths, err := resolvePaths(opts.Home)
	if err != nil {
		return Status{}, err
	}
	cfg, err := readConfig(paths)
	if errors.Is(err, os.ErrNotExist) {
		return Status{Configured: false, Home: paths.Home, ManualOnly: true, SyncCapabilityVersion: stateSchemaVersion}, nil
	}
	if err != nil {
		return Status{}, err
	}
	status := Status{
		Configured:            true,
		BackgroundSync:        cfg.BackgroundSync,
		ManualOnly:            cfg.ManualOnly,
		Home:                  paths.Home,
		Root:                  cfg.Root,
		BaseURL:               cfg.BaseURL,
		ScopeLabel:            cfg.ScopeLabel,
		SyncCapabilityVersion: stateSchemaVersion,
		ConfigGeneration:      cfg.ConfigGeneration,
		LiveSyncEnabled:       cfg.LiveSyncEnabled,
		FrontmatterFlavor:     configFlavor(cfg).Frontmatter,
		LinksFlavor:           configFlavor(cfg).Links,
	}
	if state, err := openSyncState(context.Background(), paths); err == nil {
		status.Documents = state.countPlacements(context.Background())
		if recoveries, err := state.listRecoveries(context.Background(), false); err == nil {
			status.Conflicts = len(recoveries)
		}
		_ = state.Close()
	} else {
		status.Documents = countMetadataFiles(filepath.Join(paths.Home, "sync", "metadata", "placements"))
		status.Conflicts = countMarkdownFiles(filepath.Join(paths.Home, "sync", "recovery"))
	}
	if last, err := os.ReadFile(filepath.Join(paths.Home, "sync", "last-sync-at")); err == nil {
		status.LastSyncAt = strings.TrimSpace(string(last))
	}
	if unsupported, err := os.ReadFile(filepath.Join(paths.Home, "sync", "unsupported-sections.json")); err == nil {
		_ = json.Unmarshal(unsupported, &status.Unsupported)
	}
	return status, nil
}

func Once(ctx context.Context, opts Options) (OnceResult, error) {
	paths, err := resolvePaths(opts.Home)
	if err != nil {
		return OnceResult{}, err
	}
	lock, err := acquireSyncLock(paths)
	if err != nil {
		return OnceResult{}, err
	}
	defer lock.Close()
	cfg, err := readConfig(paths)
	if err != nil {
		return OnceResult{}, err
	}
	creds, err := readCredentials(paths)
	if err != nil {
		return OnceResult{}, err
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	if err := ensurePrivateDirs(paths); err != nil {
		return OnceResult{}, err
	}
	state, err := openSyncState(ctx, paths)
	if err != nil {
		return OnceResult{}, err
	}
	defer state.Close()
	if err := ensureSafeRoot(cfg.Root); err != nil {
		return OnceResult{}, err
	}
	if err := validateRootOwnership(cfg.Root, cfg, paths); err != nil {
		return OnceResult{}, err
	}
	if err := state.replayIncompleteOps(ctx, cfg.Root); err != nil {
		return OnceResult{}, err
	}
	if err := writeRootFiles(ctx, state, cfg.Root, cfg, paths); err != nil {
		return OnceResult{}, err
	}
	if err := writeLocalAgentDocs(ctx, client, paths, cfg.Root, cfg.BaseURL); err != nil {
		return OnceResult{}, err
	}
	snapshot, allRows, err := fetchSnapshotWithPages(ctx, client, cfg.BaseURL, creds.APIKey)
	if err != nil {
		return OnceResult{}, err
	}
	if snapshot.PageInfo.Partial || snapshot.PageInfo.NextCursor != nil {
		return OnceResult{}, errors.New("library sync snapshot is still partial; refusing to mirror a partial generation")
	}
	if !snapshot.SnapshotComplete {
		return OnceResult{}, errors.New("library sync snapshot is not complete; refusing to mirror a partial generation")
	}
	for _, section := range snapshot.CoveredSections {
		if section.Covered && !section.Authoritative {
			reason := strings.TrimSpace(section.Reason)
			if reason != "" {
				return OnceResult{}, fmt.Errorf("library sync section %q is not authoritative: %s", section.Label, reason)
			}
			return OnceResult{}, fmt.Errorf("library sync section %q is not authoritative", section.Label)
		}
	}
	coveredSections := authoritativeCoveredSections(snapshot.CoveredSections)
	pathsByVisibleID := allocatePaths(cfg.Root, allRows)
	// Bug #531 defense (A)/(B): allocatePaths is expected to hand each
	// document a unique path, but if two snapshot entries ever resolve to the
	// same local path (e.g. duplicate Botlets agents sharing
	// owner-handle + bot-slug) one write would clobber the other and the loser
	// looks "absent", which historically swept the live brain doc to recovery.
	// Surface the collision loudly so it is diagnosable instead of silent.
	for _, collision := range detectAllocationCollisions(allRows, pathsByVisibleID) {
		fmt.Fprintf(os.Stderr, "comment sync: WARNING duplicate document mapping for path %q (documents %s) — synced content for duplicates may be incomplete; this usually means duplicate agents share an owner/slug (see bug #531)\n", collision.Path, strings.Join(collision.VisibleInstanceIDs, ", "))
		writeSyncRuntimeLog(paths, "error", "sync.reconcile.path_collision_detected", map[string]any{
			"path":               collision.Path,
			"visibleInstanceIds": collision.VisibleInstanceIDs,
		})
	}
	result := OnceResult{Root: cfg.Root, SnapshotID: snapshot.SnapshotID, ScopeLabel: snapshot.ScopeLabel}
	for _, section := range snapshot.UnsupportedSections {
		if section.Reason != "" {
			result.UnsupportedSections = append(result.UnsupportedSections, section.Label+": "+section.Reason)
		} else {
			result.UnsupportedSections = append(result.UnsupportedSections, section.Label)
		}
	}
	sort.Strings(result.UnsupportedSections)
	if err := state.recordSnapshotStart(ctx, snapshot.SnapshotID, snapshot.ScopeLabel, result.UnsupportedSections); err != nil {
		return OnceResult{}, err
	}
	flavor := configFlavor(cfg)
	// For links=obsidian, wikilinks resolve against every synced doc's on-disk
	// basename, so build the slug→path map once across the whole snapshot.
	resolve := slugBasenameResolver(snapshotSlugToPath(allRows, pathsByVisibleID))
	seenDocuments := map[string]snapshotRow{}
	for _, row := range allRows {
		if row.Kind != "document" || row.DocSlug == "" {
			continue
		}
		seenDocuments[row.VisibleInstanceID] = row
		target := pathsByVisibleID[row.VisibleInstanceID]
		if target == "" {
			continue
		}
		projection, err := fetchProjection(ctx, client, cfg.BaseURL, creds.APIKey, row.DocSlug)
		if err != nil {
			return OnceResult{}, err
		}
		if row.Revision != nil && projection.Revision != *row.Revision {
			return OnceResult{}, fmt.Errorf("projection fetch for %s returned revision %d, expected snapshot revision %d", row.DocSlug, projection.Revision, *row.Revision)
		}
		if row.ContentHash != "" && projection.ContentHash != row.ContentHash {
			return OnceResult{}, fmt.Errorf("projection fetch for %s returned content hash %s, expected snapshot hash %s", row.DocSlug, projection.ContentHash, row.ContentHash)
		}
		fr, err := buildFlavorRender(ctx, client, cfg.BaseURL, creds.APIKey, row.DocSlug, flavor, resolve)
		if err != nil {
			return OnceResult{}, err
		}
		recovered, err := writeProjection(ctx, paths, state, cfg.Root, cfg.BaseURL, target, row, projection, snapshot.SnapshotID, fr)
		if err != nil {
			return OnceResult{}, err
		}
		if recovered {
			result.RecoveriesPreserved++
		}
		result.DocumentsWritten++
	}
	removed, recovered, err := removeAbsentPlacements(ctx, paths, state, cfg.Root, snapshot.SnapshotID, seenDocuments, pathsByVisibleID, coveredSections)
	if err != nil {
		return OnceResult{}, err
	}
	result.DocumentsRemoved = removed
	result.RecoveriesPreserved += recovered
	if err := state.recordSnapshotComplete(ctx, snapshot.SnapshotID); err != nil {
		return OnceResult{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if err := os.WriteFile(filepath.Join(paths.Home, "sync", "last-sync-at"), []byte(now+"\n"), 0o600); err != nil {
		return OnceResult{}, err
	}
	if err := writeJSON0600(filepath.Join(paths.Home, "sync", "unsupported-sections.json"), result.UnsupportedSections); err != nil {
		return OnceResult{}, err
	}
	return result, nil
}

func RecoverDirtyProjections(ctx context.Context, opts Options) (RecoveryScanResult, error) {
	paths, err := resolvePaths(opts.Home)
	if err != nil {
		return RecoveryScanResult{}, err
	}
	cfg, err := readConfig(paths)
	if err != nil {
		return RecoveryScanResult{}, err
	}
	state, err := openSyncState(ctx, paths)
	if err != nil {
		return RecoveryScanResult{}, err
	}
	placements, err := state.listPlacements(ctx)
	recoveriesBefore := 0
	if err == nil {
		if recoveries, listErr := state.listRecoveries(ctx, false); listErr == nil {
			recoveriesBefore = len(recoveries)
		}
	}
	if closeErr := state.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		return RecoveryScanResult{}, err
	}
	result := RecoveryScanResult{Checked: len(placements)}
	needsBySlug := map[string]placementMeta{}
	for _, placement := range placements {
		if placement.Slug == "" {
			continue
		}
		needsRepair, err := placementNeedsLocalRepair(cfg.Root, placement, configFlavor(cfg))
		if err != nil {
			return RecoveryScanResult{}, err
		}
		if needsRepair {
			current, ok := needsBySlug[placement.Slug]
			if !ok || placement.Revision > current.Revision {
				needsBySlug[placement.Slug] = placement
			}
		}
	}
	for slug, placement := range needsBySlug {
		refresh, err := RefreshProjection(ctx, opts, LiveEvent{
			Type:     "local_projection_changed",
			Slug:     slug,
			Revision: placement.Revision,
		})
		if err != nil {
			return RecoveryScanResult{}, err
		}
		if refresh.NeedsSnapshot {
			if _, err := Once(ctx, opts); err != nil {
				return RecoveryScanResult{}, err
			}
			result.SnapshotRefreshes++
			continue
		}
		result.ProjectionRefreshes += refresh.Refreshed
		result.NotModified += refresh.NotModified
	}
	if state, err := openSyncState(ctx, paths); err == nil {
		if recoveries, listErr := state.listRecoveries(ctx, false); listErr == nil && len(recoveries) > recoveriesBefore {
			result.RecoveriesPreserved += len(recoveries) - recoveriesBefore
		}
		_ = state.Close()
	}
	return result, nil
}

func readConfig(paths commentbus.Paths) (Config, error) {
	var cfg Config
	if err := readJSON(filepath.Join(paths.Home, "sync", "config.json"), &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Version > configVersion {
		return Config{}, errors.New("sync config was written by a newer comment binary")
	}
	return cfg, nil
}

func readCredentials(paths commentbus.Paths) (Credentials, error) {
	var creds Credentials
	if err := readJSON(filepath.Join(paths.Home, "sync", "credentials.json"), &creds); err != nil {
		return Credentials{}, err
	}
	if creds.APIKey == "" {
		return Credentials{}, errors.New("sync credentials are missing api_key")
	}
	return creds, nil
}

func resolvePaths(home string) (commentbus.Paths, error) {
	if home == "" {
		home = os.Getenv("COMMENT_IO_HOME")
	}
	return commentbus.ResolvePaths(home)
}

func resolveRoot(root string) (string, error) {
	if root != "" {
		return commentbus.ExpandHome(root)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, commentbus.CurrentEnvironment().DefaultSyncRootName()), nil
}

func ensurePrivateDirs(paths commentbus.Paths) error {
	for _, dir := range []string{
		filepath.Join(paths.Home, "sync"),
		filepath.Join(paths.Home, "sync", "metadata"),
		filepath.Join(paths.Home, "sync", "metadata", "placements"),
		filepath.Join(paths.Home, "sync", "recovery"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func writeRootFiles(ctx context.Context, state *syncState, root string, cfg Config, paths commentbus.Paths) error {
	markerPath := filepath.Join(root, ".comment-sync-root.json")
	if err := validateManagedPath(root, markerPath); err != nil {
		return err
	}
	marker := map[string]any{}
	if err := readJSON(markerPath, &marker); err == nil {
		if marker["managed_by"] != "comment sync" {
			return errors.New("sync root marker exists but is not managed by comment sync")
		}
		if err := validateRootMarker(marker, cfg, paths); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	readmePath := filepath.Join(root, "README.md")
	if err := validateManagedPath(root, readmePath); err != nil {
		return err
	}
	readme := rootReadmeContent()
	if existing, err := os.ReadFile(readmePath); errors.Is(err, os.ErrNotExist) {
		op, err := state.beginOp(ctx, "write_root_readme", "", "", readmePath, "")
		if err != nil {
			return err
		}
		if err := atomicWriteFile(readmePath, []byte(readme), 0o644); err != nil {
			_ = state.finishOp(ctx, op, "failed", err)
			return err
		}
		if err := state.finishOp(ctx, op, "complete", nil); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else if string(existing) == rootReadmeContentWithRecover() || string(existing) == readme {
		op, err := state.beginOp(ctx, "write_root_readme", "", "", readmePath, "")
		if err != nil {
			return err
		}
		if err := atomicWriteFile(readmePath, []byte(readme), 0o644); err != nil {
			_ = state.finishOp(ctx, op, "failed", err)
			return err
		}
		if err := state.finishOp(ctx, op, "complete", nil); err != nil {
			return err
		}
	}

	if _, ok := marker["created_at"]; !ok {
		marker["created_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	marker["schema_version"] = 1
	marker["managed_by"] = "comment sync"
	if _, ok := marker["root_uuid"]; !ok {
		marker["root_uuid"] = "root_" + shortStableSuffix(fmt.Sprintf("%s:%d", root, time.Now().UnixNano()))
	}
	marker["base_url_hash"] = sha256Hex(cfg.BaseURL)
	marker["human_id_hash"] = sha256Hex(cfg.HumanID)
	marker["state_home_id"] = sha256Hex(paths.Home)
	marker["config_generation"] = cfg.ConfigGeneration
	flavor := configFlavor(cfg)
	marker["frontmatter_flavor"] = flavor.Frontmatter
	marker["links_flavor"] = flavor.Links
	marker["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	return writeJSON0600(markerPath, marker)
}

func writeLocalAgentDocs(ctx context.Context, client *http.Client, paths commentbus.Paths, root string, baseURL string) error {
	docsDir := filepath.Join(root, localAgentDocsDirName)
	readmeTarget := filepath.Join(docsDir, "README.md")
	if err := validateManagedPath(root, readmeTarget); err != nil {
		return err
	}
	llms, ok := fetchPublicAgentDoc(ctx, client, baseURL, "/llms.txt")
	if !ok {
		return nil
	}
	markerPath := localAgentDocsMarkerPath(root)
	if err := validateManagedPath(root, markerPath); err != nil {
		return err
	}
	var marker map[string]any
	if err := readJSON(markerPath, &marker); err == nil {
		if marker["managed_by"] != "comment sync" || marker["kind"] != "local-agent-docs" {
			return nil
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Lstat(docsDir); statErr == nil {
			return nil
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		marker = map[string]any{}
	} else {
		return err
	}
	if err := os.MkdirAll(docsDir, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(docsDir, 0o755); err != nil {
		return err
	}
	if _, ok := marker["created_at"]; !ok {
		marker["created_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	marker["schema_version"] = 1
	marker["managed_by"] = "comment sync"
	marker["kind"] = "local-agent-docs"
	marker["base_url_hash"] = sha256Hex(baseURL)
	marker["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	if err := writeJSON0600(markerPath, marker); err != nil {
		return err
	}
	docs := map[string]string{
		"README.md": localAgentDocsReadme(baseURL),
		"llms.txt":  llms,
	}
	for _, doc := range []struct {
		name string
		path string
	}{
		{name: "local-sync.txt", path: "/llms/local-sync.txt"},
		{name: "reference.txt", path: "/llms/reference.txt"},
		{name: "notifications.txt", path: "/llms/notifications.txt"},
		{name: "registration.txt", path: "/llms/registration.txt"},
		{name: "messages.txt", path: "/llms/messages.txt"},
	} {
		if text, ok := fetchPublicAgentDoc(ctx, client, baseURL, doc.path); ok {
			docs[doc.name] = text
		}
	}
	for name, text := range docs {
		target := filepath.Join(docsDir, name)
		if err := validateManagedPath(root, target); err != nil {
			return err
		}
		if err := atomicWriteFile(target, []byte(text), 0o444); err != nil {
			return err
		}
	}
	writeSyncRuntimeLog(paths, "info", "sync.docs.written", map[string]any{
		"count": len(docs),
		"root":  root,
	})
	return nil
}

func generatedLocalAgentDocNames() []string {
	return []string{
		"README.md",
		"llms.txt",
		"local-sync.txt",
		"reference.txt",
		"notifications.txt",
		"registration.txt",
		"messages.txt",
	}
}

func authoritativeCoveredSections(sections []snapshotSection) map[string]bool {
	covered := make(map[string]bool, len(sections))
	for _, section := range sections {
		if section.Covered && section.Authoritative {
			covered[section.ID] = true
		}
	}
	return covered
}

func localAgentDocsMarkerPath(root string) string {
	return filepath.Join(root, localAgentDocsDirName, localAgentDocsMarkerName)
}

func fetchPublicAgentDoc(ctx context.Context, client *http.Client, baseURL string, path string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return "", false
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", false
	}
	return string(data), true
}

func localAgentDocsReadme(baseURL string) string {
	return strings.Join([]string{
		"# Comment.io Local Agent Docs",
		"",
		"This generated folder is a read-only cache of public Comment.io agent documentation.",
		"Prefer these local files when they are present, especially when running under comment run.",
		"Write through the Comment.io REST API or web UI; synced Markdown files are projections only.",
		"For a projected Markdown file, use the header's slug/revision: GET /docs/{slug}, then PATCH /docs/{slug} with base_revision and edits.",
		"Do not use Claude Code/Codex runtime memory as the durable source of truth for Comment.io or Botlets brain docs.",
		"",
		"- llms.txt: main agent instructions",
		"- local-sync.txt: local mirror rules",
		"- reference.txt: full API reference",
		"- notifications.txt: notification workflow",
		"- registration.txt: persistent identity workflow",
		"- messages.txt: direct agent messages workflow",
		"",
		"Canonical remote docs: " + strings.TrimRight(baseURL, "/") + "/llms.txt",
		"",
	}, "\n")
}

func rootReadmeContent() string {
	return strings.Join([]string{
		"# Comment.io Local Sync",
		"",
		"These files are read-only projections from Comment.io.",
		"Each synced Markdown file starts with a hidden comment.io:projection header that names the canonical URL, slug, revision, body hash, and local agent docs folder.",
		"The canonical Markdown body starts after that header. Ignore the header when constructing API edit old_string values.",
		"Edit the canonical document in Comment.io. Local edits are preserved under ~/.comment-io/sync/recovery and then replaced with the server version.",
		"To update a projection through the API, use the header's slug and revision: GET /docs/{slug}, then PATCH /docs/{slug} with base_revision and edits.",
		"Deleting a local projection does not delete the remote document; the next sync recreates clean projections.",
		"",
		"Agents should read local files for search and context when this mirror is available, then write through the Comment.io API or web UI. Do not use runtime-local memory as the durable source of truth for mirrored docs or Botlets brain memory. Local agent docs are in _Comment.io Docs/.",
		"",
		"Useful commands: comment sync status, comment sync conflicts, comment sync recover <path>, comment sync logout.",
		"",
	}, "\n")
}

func rootReadmeContentWithRecover() string {
	return strings.Join([]string{
		"# Comment.io Local Sync",
		"",
		"These files are read-only projections from Comment.io.",
		"Edit the canonical document in Comment.io. Local edits are preserved under ~/.comment-io/sync/recovery and then replaced with the server version.",
		"Use the projection header's slug/revision for API writes: GET /docs/{slug}, then PATCH /docs/{slug} with base_revision and edits.",
		"Deleting a local projection does not delete the remote document; the next sync recreates clean projections.",
		"Do not use runtime-local memory as the durable source of truth for mirrored docs or Botlets brain memory.",
		"",
		"Useful commands: comment sync status, comment sync conflicts, comment sync recover <path>, comment sync logout.",
		"",
	}, "\n")
}

func fetchSnapshot(ctx context.Context, client *http.Client, baseURL, key string) (snapshotResponse, error) {
	return fetchSnapshotPath(ctx, client, baseURL, key, "/auth/library-sync/snapshot?v=2&materialization=incremental")
}

func fetchSnapshotPage(ctx context.Context, client *http.Client, baseURL, key, snapshotID, cursor string) (snapshotResponse, error) {
	return fetchSnapshotPath(ctx, client, baseURL, key, "/auth/library-sync/snapshot/"+snapshotID+"/pages/"+cursor+"?v=2")
}

func fetchSnapshotWithPages(ctx context.Context, client *http.Client, baseURL, key string) (snapshotResponse, []snapshotRow, error) {
	first, err := fetchSnapshot(ctx, client, baseURL, key)
	if err != nil {
		return snapshotResponse{}, nil, err
	}
	deadline := time.Now().Add(librarySyncSnapshotRetryMaxWait)
	for {
		final, rows, err := collectSnapshotPages(ctx, client, baseURL, key, first)
		if err != nil {
			return snapshotResponse{}, nil, err
		}
		if !final.PageInfo.Partial {
			return final, rows, nil
		}
		if !time.Now().Before(deadline) {
			return snapshotResponse{}, nil, librarySyncSnapshotIncompleteError{message: "library sync snapshot is still partial; refusing to mirror a partial generation"}
		}
		timer := time.NewTimer(librarySyncSnapshotRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			if err := ctx.Err(); isTransportTimeout(err) {
				return snapshotResponse{}, nil, librarySyncTimeoutError{message: fmt.Sprintf("library sync snapshot wait timed out before the server finished materializing; retry setup in a moment: %v", err)}
			}
			return snapshotResponse{}, nil, ctx.Err()
		case <-timer.C:
		}
		first, err = fetchSnapshotPage(ctx, client, baseURL, key, first.SnapshotID, "first")
		if err != nil {
			return snapshotResponse{}, nil, err
		}
		if first.SnapshotID == "" {
			return snapshotResponse{}, nil, errors.New("library sync snapshot page response omitted snapshot id")
		}
	}
}

func collectSnapshotPages(ctx context.Context, client *http.Client, baseURL, key string, first snapshotResponse) (snapshotResponse, []snapshotRow, error) {
	final := first
	allRows := append([]snapshotRow(nil), first.Rows...)
	seenCursors := map[string]bool{}
	for final.PageInfo.NextCursor != nil {
		cursor := strings.TrimSpace(*final.PageInfo.NextCursor)
		if cursor == "" || seenCursors[cursor] {
			return snapshotResponse{}, nil, fmt.Errorf("library sync snapshot %s returned invalid repeated cursor %q", first.SnapshotID, cursor)
		}
		seenCursors[cursor] = true
		next, err := fetchSnapshotPage(ctx, client, baseURL, key, first.SnapshotID, cursor)
		if err != nil {
			return snapshotResponse{}, nil, err
		}
		if next.SnapshotID != first.SnapshotID {
			return snapshotResponse{}, nil, fmt.Errorf("library sync snapshot page changed snapshot id from %s to %s", first.SnapshotID, next.SnapshotID)
		}
		allRows = append(allRows, next.Rows...)
		final = next
	}
	return final, allRows, nil
}

func fetchSnapshotPath(ctx context.Context, client *http.Client, baseURL, key, path string) (snapshotResponse, error) {
	var out snapshotResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+path, nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return out, classifyLibrarySyncTransportError("library sync snapshot request", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		bodyText := strings.TrimSpace(string(body))
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return out, librarySyncKeyRejectedError{message: fmt.Sprintf("library sync key was rejected (HTTP %d): %s. The key may be expired, revoked, or copied from an old setup command; copy a fresh setup command or run comment sync login again", resp.StatusCode, bodyText)}
		}
		return out, fmt.Errorf("library sync snapshot failed: HTTP %d %s", resp.StatusCode, bodyText)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	return out, nil
}

func revokeCurrentDevice(ctx context.Context, client *http.Client, baseURL, key string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, strings.TrimRight(baseURL, "/")+"/auth/library-sync/current-device", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("library sync logout revoke failed: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func fetchProjection(ctx context.Context, client *http.Client, baseURL, key, slug string) (projectionResponse, error) {
	var out projectionResponse
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/docs/"+slug+"?projection=library-sync", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return out, fmt.Errorf("projection fetch for %s failed: HTTP %d %s", slug, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	if out.ContentHash == "" {
		out.ContentHash = sha256Hex(out.Markdown)
	}
	return out, nil
}

// fetchEgressDocument fetches a server egress representation (GET
// /docs/:slug?format=<format>) as raw bytes. Used for format=okf (OKF
// frontmatter + plain body) and format=obsidian-slug (OKF frontmatter + a body
// whose cross-doc links the shared server tokenizer tagged [[slug|label]]) — the
// server owns the YAML merge + the link tokenizing so the daemon never
// re-implements them.
func fetchEgressDocument(ctx context.Context, client *http.Client, baseURL, key, slug, format string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/docs/"+slug+"?format="+format, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("%s fetch for %s failed: HTTP %d %s", format, slug, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// Read one byte past the cap so an oversized response is rejected rather than
	// silently truncated: a truncated body still has its frontmatter at the top,
	// so splitOkfFrontmatter would accept it and the mirror would present an
	// incomplete document with a "clean" BodyContentHash indefinitely.
	const maxEgressBytes = 25 << 20 // generous; the projection fetch has no byte cap
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxEgressBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxEgressBytes {
		return nil, fmt.Errorf("%s fetch for %s exceeded the %d-byte limit", format, slug, maxEgressBytes)
	}
	return data, nil
}

// buildFlavorRender assembles the per-document flavor materials for a write.
// When links=obsidian it fetches the server's format=obsidian-slug document
// (OKF frontmatter + a slug-tagged body) — one request that serves both the okf
// and plain frontmatter flavors (the frontmatter is used or the plain header
// substituted in projectionPrefixAndBody). When links=plain and frontmatter=okf
// it fetches format=okf. A configured server format that comes back without a
// recognizable x-comment frontmatter is a hard error so the root never silently
// writes a half-formed file.
//
// The egress fetch is a second request after the projection, so on a concurrent
// edit its body may be one revision ahead of projection.ContentHash. That is
// harmless: BodyContentHash (computed from the bytes actually written) is the
// dirty-check source of truth, and the next sync reconciles the recorded
// ContentHash. ContentHash stays the canonical server-change signal.
func buildFlavorRender(ctx context.Context, client *http.Client, baseURL, key, slug string, flavor syncFlavor, resolve slugNameResolver) (flavorRender, error) {
	fr := flavorRender{flavor: flavor}
	if flavor.Links == flavorLinksObsidian {
		fr.resolve = resolve
		// okf frontmatter → obsidian-slug-okf (OKF-framed, slug-tagged body);
		// plain frontmatter → obsidian-slug (CANONICAL-framed: the doc's own
		// leading frontmatter preserved verbatim, only body links tagged). Both
		// run the same server-side tokenizer; the framing is what differs.
		if flavor.Frontmatter == flavorFrontmatterOKF {
			doc, err := fetchEgressDocument(ctx, client, baseURL, key, slug, "obsidian-slug-okf")
			if err != nil {
				return flavorRender{}, err
			}
			if _, _, ok := splitOkfFrontmatter(doc); !ok {
				return flavorRender{}, fmt.Errorf("obsidian-slug-okf fetch for %s returned a document without a recognizable x-comment frontmatter", slug)
			}
			fr.obsidianSlugDocument = doc
			fr.obsidianSlugPresent = true
			return fr, nil
		}
		doc, err := fetchEgressDocument(ctx, client, baseURL, key, slug, "obsidian-slug")
		if err != nil {
			return flavorRender{}, err
		}
		// Canonical-framed: no required OKF block (the doc may legitimately have
		// no leading frontmatter — or be legitimately empty, which is a valid doc
		// the mirror reproduces as an empty body). fetchEgressDocument already
		// rejects non-200s and truncation (byte-cap over-read), so accept as-is.
		// Mark it PRESENT (not via byte length) so an empty body is honored as the
		// authoritative content, not mistaken for "no egress" → stale fallback.
		fr.obsidianSlugDocument = doc
		fr.obsidianSlugPresent = true
		return fr, nil
	}
	if flavor.Frontmatter == flavorFrontmatterOKF {
		doc, err := fetchEgressDocument(ctx, client, baseURL, key, slug, "okf")
		if err != nil {
			return flavorRender{}, err
		}
		if _, _, ok := splitOkfFrontmatter(doc); !ok {
			return flavorRender{}, fmt.Errorf("okf fetch for %s returned a document without a recognizable x-comment frontmatter", slug)
		}
		fr.okfDocument = doc
	}
	return fr, nil
}

// docBasename derives the Obsidian note name (file basename without .md) for a
// projected file path.
func docBasename(path string) string {
	name := filepath.Base(path)
	if ext := filepath.Ext(name); strings.EqualFold(ext, ".md") {
		return name[:len(name)-len(ext)]
	}
	return name
}

// placementsSlugToPath maps each placement's slug to its on-disk path (for
// vault wikilink resolution in the live path, which works from persisted
// placements rather than a fresh snapshot).
func placementsSlugToPath(placements []placementMeta) map[string]string {
	out := map[string]string{}
	for _, p := range placements {
		if p.Slug == "" {
			continue
		}
		path := p.Path
		if path == "" {
			path = p.CanonicalPath
		}
		if path != "" {
			out[p.Slug] = path
		}
	}
	return out
}

// snapshotSlugToPath maps each document row's slug to its allocated on-disk
// path for the current snapshot (for vault wikilink resolution).
func snapshotSlugToPath(rows []snapshotRow, pathsByVisibleID map[string]string) map[string]string {
	out := map[string]string{}
	for _, row := range rows {
		if row.Kind != "document" || row.DocSlug == "" {
			continue
		}
		if path := pathsByVisibleID[row.VisibleInstanceID]; path != "" {
			out[row.DocSlug] = path
		}
	}
	return out
}

// slugBasenameResolver builds a slug→note-name resolver from a slug→path map,
// for rewriting cross-doc links to [[wikilinks]] that point at the actual
// on-disk file names in the vault. When two docs in different folders share a
// basename (allocatePaths allows that — their full paths differ), a bare
// [[basename]] wikilink would be ambiguous, so colliding basenames resolve to ""
// (the link is left as a full URL, which still works in Obsidian — lossless).
func slugBasenameResolver(slugToPath map[string]string) slugNameResolver {
	slugBasename := make(map[string]string, len(slugToPath))
	counts := map[string]int{}
	for slug, path := range slugToPath {
		if path == "" {
			continue
		}
		b := docBasename(path)
		slugBasename[slug] = b
		counts[b]++
	}
	return func(slug string) string {
		b, ok := slugBasename[slug]
		if !ok || b == "" || counts[b] > 1 {
			return ""
		}
		return b
	}
}

func allocatePaths(root string, rows []snapshotRow) map[string]string {
	paths := map[string]string{}
	used := map[string]int{}
	used[canonicalKey(filepath.Join(root, "README.md"))] = 1
	used[canonicalKey(filepath.Join(root, ".comment-sync-root.json"))] = 1
	used[canonicalKey(filepath.Join(root, localAgentDocsDirName))] = 1

	sharedRows := make([]snapshotRow, 0)
	teamWikiChildren := map[string][]snapshotRow{}
	botletsRows := make([]snapshotRow, 0)
	personalChildren := map[string][]snapshotRow{}
	for _, row := range rows {
		if row.Section == "shared-with-me" {
			if row.Kind == "document" {
				sharedRows = append(sharedRows, row)
			}
			continue
		}
		if row.Section == "botlets-brains" {
			botletsRows = append(botletsRows, row)
			continue
		}
		parentID := ""
		if row.ParentVisibleInstance != nil {
			parentID = *row.ParentVisibleInstance
		}
		if row.Section == "team-wiki" {
			teamWikiChildren[parentID] = append(teamWikiChildren[parentID], row)
			continue
		}
		personalChildren[parentID] = append(personalChildren[parentID], row)
	}

	var walkTree func(childrenByParent map[string][]snapshotRow, parentID string, parentPath string)
	walkTree = func(childrenByParent map[string][]snapshotRow, parentID string, parentPath string) {
		children := sortedRowsForAllocation(childrenByParent[parentID])
		for _, row := range children {
			name := sanitizeComponent(row.Name)
			if row.Kind == "document" {
				name = ensureMarkdownName(row.Name)
			}
			paths[row.VisibleInstanceID] = uniquePath(parentPath, name, row.VisibleInstanceID, used)
			if row.Kind == "folder" {
				walkTree(childrenByParent, row.VisibleInstanceID, paths[row.VisibleInstanceID])
			}
		}
	}
	walkTree(personalChildren, "", filepath.Join(root, "My Files"))
	walkTree(teamWikiChildren, "", filepath.Join(root, "Team Wiki"))

	allocateOrphanTreeRows := func(childrenByParent map[string][]snapshotRow, defaultParent string) {
		for _, rows := range childrenByParent {
			for _, row := range sortedRowsForAllocation(rows) {
				if paths[row.VisibleInstanceID] != "" {
					continue
				}
				parent := defaultParent
				if row.ParentVisibleInstance != nil && paths[*row.ParentVisibleInstance] != "" {
					parent = paths[*row.ParentVisibleInstance]
				}
				name := sanitizeComponent(row.Name)
				if row.Kind == "document" {
					name = ensureMarkdownName(row.Name)
				}
				paths[row.VisibleInstanceID] = uniquePath(parent, name, row.VisibleInstanceID, used)
			}
		}
	}
	allocateOrphanTreeRows(personalChildren, filepath.Join(root, "My Files"))
	allocateOrphanTreeRows(teamWikiChildren, filepath.Join(root, "Team Wiki"))

	for _, row := range sortedRowsForAllocation(sharedRows) {
		parent := filepath.Join(root, "Shared With Me")
		if source, ok := sharedSourceFolder(row.SourceLabel); ok {
			parent = filepath.Join(parent, source)
		}
		paths[row.VisibleInstanceID] = uniquePath(parent, ensureMarkdownName(row.Name), row.VisibleInstanceID, used)
	}
	allocateBotletsBrainPaths(root, botletsRows, paths, used)
	return paths
}

// allocationCollision describes two or more document rows that resolved to the
// same local path during allocation (bug #531).
type allocationCollision struct {
	Path               string
	VisibleInstanceIDs []string
}

// detectAllocationCollisions reports document rows whose allocated paths share
// a canonical path. Only document rows are considered (folders are containers,
// not content). Results are deterministic for stable logging/testing.
func detectAllocationCollisions(rows []snapshotRow, pathsByVisibleID map[string]string) []allocationCollision {
	byCanonical := map[string][]string{}
	displayPath := map[string]string{}
	for _, row := range rows {
		if row.Kind != "document" {
			continue
		}
		target := pathsByVisibleID[row.VisibleInstanceID]
		if target == "" {
			continue
		}
		key := canonicalKey(target)
		byCanonical[key] = append(byCanonical[key], row.VisibleInstanceID)
		if _, ok := displayPath[key]; !ok {
			displayPath[key] = target
		}
	}
	var collisions []allocationCollision
	for key, ids := range byCanonical {
		if len(ids) < 2 {
			continue
		}
		sort.Strings(ids)
		collisions = append(collisions, allocationCollision{Path: displayPath[key], VisibleInstanceIDs: ids})
	}
	sort.Slice(collisions, func(i, j int) bool { return collisions[i].Path < collisions[j].Path })
	return collisions
}

func allocateBotletsBrainPaths(root string, rows []snapshotRow, paths map[string]string, used map[string]int) {
	if len(rows) == 0 {
		return
	}
	byBot := map[string][]snapshotRow{}
	for _, row := range rows {
		key := firstNonEmpty(row.BotletsBotID, row.BotletsBotAgentID, row.BotletsBotSlug, row.BotletsBotHandle, row.VisibleInstanceID)
		byBot[key] = append(byBot[key], row)
	}
	botKeys := make([]string, 0, len(byBot))
	for key := range byBot {
		botKeys = append(botKeys, key)
	}
	sort.Strings(botKeys)
	for _, key := range botKeys {
		botRows := byBot[key]
		sort.Slice(botRows, func(i, j int) bool {
			left := botletsBrainSortKey(botRows[i])
			right := botletsBrainSortKey(botRows[j])
			if left != right {
				return left < right
			}
			return botRows[i].VisibleInstanceID < botRows[j].VisibleInstanceID
		})
		seed := botRows[0]
		ownerName := sanitizeComponent(firstNonEmpty(seed.BotletsOwnerHandle, "Owner"))
		botName := sanitizeComponent(firstNonEmpty(seed.BotletsBotLocalName, seed.BotletsBotSlug, seed.BotletsBotHandle, seed.BotletsBotID, seed.BotletsBotAgentID, "Bot"))
		botRoot := uniquePath(filepath.Join(root, "Botlets", ownerName), botName, key, used)
		brainRoot := filepath.Join(botRoot, "brain")
		used[canonicalKey(brainRoot)] = 1
		children := map[string][]snapshotRow{}
		for _, row := range botRows {
			parentID := ""
			if row.ParentVisibleInstance != nil {
				parentID = *row.ParentVisibleInstance
			}
			children[parentID] = append(children[parentID], row)
		}
		var walk func(parentID string, parentPath string)
		walk = func(parentID string, parentPath string) {
			for _, row := range sortedRowsForAllocation(children[parentID]) {
				name := sanitizeComponent(row.Name)
				if row.Kind == "document" {
					name = ensureMarkdownName(row.Name)
				}
				paths[row.VisibleInstanceID] = uniquePath(parentPath, name, row.VisibleInstanceID, used)
				if row.Kind == "folder" {
					walk(row.VisibleInstanceID, paths[row.VisibleInstanceID])
				}
			}
		}
		walk("", brainRoot)
		for _, rows := range children {
			for _, row := range sortedRowsForAllocation(rows) {
				if paths[row.VisibleInstanceID] != "" {
					continue
				}
				parent := brainRoot
				if row.ParentVisibleInstance != nil && paths[*row.ParentVisibleInstance] != "" {
					parent = paths[*row.ParentVisibleInstance]
				}
				name := sanitizeComponent(row.Name)
				if row.Kind == "document" {
					name = ensureMarkdownName(row.Name)
				}
				paths[row.VisibleInstanceID] = uniquePath(parent, name, row.VisibleInstanceID, used)
			}
		}
	}
}

func botletsBrainSortKey(row snapshotRow) string {
	return strings.Join([]string{
		strings.ToLower(firstNonEmpty(row.BotletsOwnerHandle, "owner")),
		strings.ToLower(firstNonEmpty(row.BotletsBotLocalName, row.BotletsBotSlug, row.BotletsBotHandle, row.BotletsBotID, row.BotletsBotAgentID, "bot")),
		allocationSortKey(row),
	}, "\x00")
}

func sharedSourceFolder(sourceLabel *string) (string, bool) {
	if sourceLabel == nil {
		return "", false
	}
	source := sanitizeComponent(firstNonEmpty(*sourceLabel, "Shared"))
	if source == "Untitled" {
		return "Shared", true
	}
	return source, true
}

func sortedRowsForAllocation(rows []snapshotRow) []snapshotRow {
	out := append([]snapshotRow(nil), rows...)
	sort.Slice(out, func(i, j int) bool {
		left := allocationSortKey(out[i])
		right := allocationSortKey(out[j])
		if left != right {
			return left < right
		}
		return out[i].VisibleInstanceID < out[j].VisibleInstanceID
	})
	return out
}

func allocationSortKey(row snapshotRow) string {
	name := sanitizeComponent(row.Name)
	if row.Kind == "document" {
		name = ensureMarkdownName(row.Name)
	}
	return strings.ToLower(name)
}

func uniquePath(parent, name, stableID string, used map[string]int) string {
	candidate := filepath.Join(parent, name)
	key := canonicalKey(candidate)
	if used[key] == 0 {
		used[key] = 1
		return candidate
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	suffix := shortStableSuffix(stableID)
	for attempt := 0; ; attempt++ {
		nextName := fmt.Sprintf("%s-%s%s", stem, suffix, ext)
		if attempt > 0 {
			nextName = fmt.Sprintf("%s-%s-%d%s", stem, suffix, attempt+1, ext)
		}
		next := filepath.Join(parent, nextName)
		nextKey := canonicalKey(next)
		if used[nextKey] == 0 {
			used[nextKey] = 1
			return next
		}
	}
}

func canonicalKey(path string) string {
	return strings.ToLower(filepath.Clean(path))
}

func sanitizeComponent(name string) string {
	cleaned := unsafeFilenameChars.ReplaceAllString(strings.TrimSpace(name), "-")
	cleaned = strings.Trim(cleaned, ". ")
	if cleaned == "" {
		return "Untitled"
	}
	if len(cleaned) > 120 {
		cleaned = strings.TrimSpace(cleaned[:120])
	}
	switch strings.ToLower(cleaned) {
	case "con", "prn", "aux", "nul", "com1", "lpt1":
		return cleaned + "-"
	default:
		return cleaned
	}
}

func ensureMarkdownName(name string) string {
	cleaned := sanitizeComponent(name)
	if strings.EqualFold(filepath.Ext(cleaned), ".md") {
		return cleaned
	}
	return cleaned + ".md"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func placementBodyContentHash(meta placementMeta) string {
	if meta.BodyContentHash != "" {
		return meta.BodyContentHash
	}
	return meta.ContentHash
}

// placementFlavor returns the normalized presentation flavor a placement was
// last written in (empty fields == plain, so pre-flavor placements read plain).
func placementFlavor(meta placementMeta) syncFlavor {
	return normalizeFlavor(meta.FrontmatterFlavor, meta.LinksFlavor)
}

// placementLocallyDirty reports whether the on-disk bytes diverge from what the
// daemon last wrote for this placement, i.e. a local edit that must be preserved
// to recovery before an overwrite. It compares the presented-body hash (the
// normal dirty signal) and, for OKF files, ALSO the full rendered-file hash:
// the OKF frontmatter carries user-visible fields (title, type, tags) that a
// body-only hash can't see, so a frontmatter-only edit would otherwise be
// silently overwritten. RenderedProjectionHash is the hash of exactly what we
// last wrote, so comparing against it never false-positives on a server-side
// header regeneration (we always compare on-disk against our own prior write).
func placementLocallyDirty(localBytes []byte, previous placementMeta) bool {
	expected := placementBodyContentHash(previous)
	if expected == "" {
		return false
	}
	if !localProjectionMatchesHash(localBytes, expected) {
		return true
	}
	if placementFlavor(previous).Frontmatter == flavorFrontmatterOKF &&
		previous.RenderedProjectionHash != "" &&
		sha256Hex(string(localBytes)) != previous.RenderedProjectionHash {
		return true
	}
	return false
}

func localProjectionMatchesHash(data []byte, expectedHash string) bool {
	if expectedHash == "" {
		return false
	}
	return sha256Hex(string(projectionBodyForDirtyCheckWithExpectedHash(data, expectedHash))) == expectedHash
}

func writeProjection(
	ctx context.Context,
	paths commentbus.Paths,
	state *syncState,
	root string,
	baseURL string,
	target string,
	row snapshotRow,
	projection projectionResponse,
	snapshotID string,
	fr flavorRender,
) (bool, error) {
	visibleID := row.VisibleInstanceID
	metaPath := placementMetadataPath(paths, visibleID)
	var previous placementMeta
	if stored, ok, err := state.getPlacement(ctx, visibleID); err != nil {
		return false, err
	} else if ok {
		previous = stored
	} else {
		previous = readPlacementMetadata(paths, visibleID)
	}
	actualTarget, err := resolveProjectionTarget(target, visibleID, previous)
	if err != nil {
		return false, err
	}
	if err := validateManagedPath(root, actualTarget); err != nil {
		return false, err
	}
	localBytes, readErr := os.ReadFile(actualTarget)
	recovered := false
	if readErr == nil {
		if placementLocallyDirty(localBytes, previous) {
			if err := preserveRecovery(ctx, paths, state, visibleID, previous.Slug, actualTarget, "local_dirty_before_overwrite", localBytes); err != nil {
				return false, err
			}
			recovered = true
		}
	} else if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return false, readErr
	}
	removeOldClean := ""
	if previous.Path != "" && previous.Path != actualTarget {
		if err := validateManagedPath(root, previous.Path); err != nil {
			return false, err
		}
		oldBytes, oldErr := os.ReadFile(previous.Path)
		if oldErr == nil {
			if placementLocallyDirty(oldBytes, previous) {
				if err := preserveRecovery(ctx, paths, state, visibleID, previous.Slug, previous.Path, "local_dirty_before_move", oldBytes); err != nil {
					return false, err
				}
				recovered = true
			}
			removeOldClean = previous.Path
		} else if !errors.Is(oldErr, os.ErrNotExist) {
			return false, oldErr
		}
	}
	if err := os.MkdirAll(filepath.Dir(actualTarget), 0o755); err != nil {
		return false, err
	}
	op, err := state.beginOp(ctx, "write_projection", visibleID, projection.Slug, actualTarget, snapshotID)
	if err != nil {
		return false, err
	}
	renderedProjection := renderProjectionFile(projection, baseURL, row, fr)
	if err := atomicWriteFile(actualTarget, renderedProjection, 0o444); err != nil {
		_ = state.finishOp(ctx, op, "failed", err)
		return false, err
	}
	if removeOldClean != "" {
		_ = os.Remove(removeOldClean)
	}
	meta := placementMeta{
		VisibleInstanceID:        visibleID,
		Slug:                     projection.Slug,
		Section:                  row.Section,
		Path:                     actualTarget,
		CanonicalPath:            target,
		ContentHash:              projection.ContentHash,
		BodyContentHash:          sha256Hex(string(projectionBodyForDirtyCheck(renderedProjection))),
		RenderedProjectionHash:   sha256Hex(string(renderedProjection)),
		ProjectionFormatVersion:  projectionFormatVersion,
		FrontmatterFlavor:        nonPlainFlavorValue(fr.flavor.Frontmatter, flavorFrontmatterPlain),
		LinksFlavor:              nonPlainFlavorValue(fr.flavor.Links, flavorLinksPlain),
		ETag:                     projection.ETag,
		Revision:                 projection.Revision,
		LastSeenSnapshot:         snapshotID,
		UpdatedAt:                time.Now().UTC(),
		BotletsOwnerHandle:       row.BotletsOwnerHandle,
		BotletsBotSlug:           row.BotletsBotSlug,
		BotletsBotLocalName:      row.BotletsBotLocalName,
		BotletsBotID:             row.BotletsBotID,
		BotletsBotHandle:         row.BotletsBotHandle,
		BotletsBotAgentID:        row.BotletsBotAgentID,
		BotletsBrainContainerID:  row.BotletsBrainContainerID,
		BotletsBrainRootFolderID: row.BotletsBrainRootFolderID,
		BotletsBrainNodeID:       row.BotletsBrainNodeID,
	}
	if err := writeJSON0600(metaPath, meta); err != nil {
		_ = state.finishOp(ctx, op, "failed", err)
		return false, err
	}
	if err := state.upsertPlacement(ctx, meta); err != nil {
		_ = state.finishOp(ctx, op, "failed", err)
		return false, err
	}
	removeLegacyPlacementMetadata(paths, visibleID)
	if err := state.finishOp(ctx, op, "complete", nil); err != nil {
		return false, err
	}
	writeSyncRuntimeLog(paths, "info", "sync.projection.header_written", map[string]any{
		"slug":     projection.Slug,
		"revision": projection.Revision,
		"path":     actualTarget,
	})
	if recovered {
		writeSyncRuntimeLog(paths, "info", "sync.recovery.local_dirty_restored", map[string]any{
			"slug": projection.Slug,
			"path": actualTarget,
		})
	}
	return recovered, nil
}

func resolveProjectionTarget(target, visibleID string, previous placementMeta) (string, error) {
	if previous.Path != "" && previous.Path == target {
		return target, nil
	}
	if previous.Path != "" && previous.CanonicalPath == target {
		if _, err := os.Stat(previous.Path); err == nil {
			return previous.Path, nil
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if previous.Path != "" {
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			return target, nil
		} else if err != nil {
			return "", err
		}
	}
	if previous.ContentHash == "" {
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			return target, nil
		} else if err != nil {
			return "", err
		}
	}
	return firstAvailableSibling(target, visibleID)
}

func firstAvailableSibling(target, visibleID string) (string, error) {
	ext := filepath.Ext(target)
	stem := strings.TrimSuffix(target, ext)
	suffix := shortStableSuffix(visibleID)
	for attempt := 0; ; attempt++ {
		candidate := fmt.Sprintf("%s-%s%s", stem, suffix, ext)
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%s-%d%s", stem, suffix, attempt+1, ext)
		}
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate, nil
		} else if err != nil {
			return "", err
		}
	}
}

func removeAbsentPlacements(
	ctx context.Context,
	paths commentbus.Paths,
	state *syncState,
	root string,
	snapshotID string,
	seen map[string]snapshotRow,
	pathsByVisibleID map[string]string,
	coveredSections map[string]bool,
) (removed int, recovered int, err error) {
	placements, err := state.listPlacements(ctx)
	if err != nil {
		return 0, 0, err
	}
	// Bug #531 defense (A)/(C): build the set of local paths that a currently
	// present (seen) document occupies after this sync's write pass. When two
	// snapshot entries collide on the SAME local brain/projection path — e.g.
	// duplicate Botlets agents minted on a team retry that share
	// owner-handle + bot-slug — an absent placement from the old mapping can
	// still point at a path that a live document now owns. Sweeping it would
	// silently delete the freshly written brain doc, leaving the bot with an
	// empty persona. Detect that collision and refuse the destructive sweep.
	seenCanonicalPaths := map[string]string{}
	for _, placement := range placements {
		if _, ok := seen[placement.VisibleInstanceID]; !ok {
			continue
		}
		for _, candidate := range []string{placement.Path, placement.CanonicalPath} {
			if candidate == "" {
				continue
			}
			seenCanonicalPaths[canonicalKey(candidate)] = placement.VisibleInstanceID
		}
	}
	// Defense-in-depth: also seed from the freshly allocated path map so a live
	// document's canonical path is protected even when its placement DB record
	// is not yet present this pass (e.g. a row not captured in the placements
	// table). Never override an owner already established from a DB record.
	for visibleID := range seen {
		target := pathsByVisibleID[visibleID]
		if target == "" {
			continue
		}
		if _, exists := seenCanonicalPaths[canonicalKey(target)]; !exists {
			seenCanonicalPaths[canonicalKey(target)] = visibleID
		}
	}
	for _, placement := range placements {
		if _, ok := seen[placement.VisibleInstanceID]; ok {
			continue
		}
		if !coveredSections[placement.Section] {
			continue
		}
		// Collision guard: if this absent placement's path is now owned by a
		// live document written this sync, removing it would erase live
		// content. Log loudly, drop only the stale placement record, and leave
		// the on-disk file untouched.
		if collisionOwner, collides := absentPlacementCollidesWithSeen(placement, seenCanonicalPaths); collides {
			fmt.Fprintf(os.Stderr, "comment sync: refusing to remove %q for absent document %s — that path is now owned by live document %s; the duplicate would have erased synced content (see bug #531)\n", placement.Path, placement.VisibleInstanceID, collisionOwner)
			writeSyncRuntimeLog(paths, "error", "sync.reconcile.path_collision_skip_removal", map[string]any{
				"absentVisibleId": placement.VisibleInstanceID,
				"absentSlug":      placement.Slug,
				"path":            placement.Path,
				"canonicalPath":   placement.CanonicalPath,
				"liveVisibleId":   collisionOwner,
				"section":         placement.Section,
			})
			if err := state.deletePlacement(ctx, placement.VisibleInstanceID); err != nil {
				return removed, recovered, err
			}
			removePlacementMetadata(paths, placement.VisibleInstanceID)
			continue
		}
		if err := validateManagedPath(root, placement.Path); err != nil {
			return removed, recovered, err
		}
		bytes, readErr := os.ReadFile(placement.Path)
		if readErr == nil {
			if placementLocallyDirty(bytes, placement) {
				if err := preserveRecovery(ctx, paths, state, placement.VisibleInstanceID, placement.Slug, placement.Path, "local_dirty_before_absent_removal", bytes); err != nil {
					return removed, recovered, err
				}
				recovered++
			}
			op, err := state.beginOp(ctx, "remove_absent_projection", placement.VisibleInstanceID, placement.Slug, placement.Path, snapshotID)
			if err != nil {
				return removed, recovered, err
			}
			if err := os.Remove(placement.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
				_ = state.finishOp(ctx, op, "failed", err)
				return removed, recovered, err
			}
			if err := state.finishOp(ctx, op, "complete", nil); err != nil {
				return removed, recovered, err
			}
			removed++
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return removed, recovered, readErr
		}
		if err := state.deletePlacement(ctx, placement.VisibleInstanceID); err != nil {
			return removed, recovered, err
		}
		removePlacementMetadata(paths, placement.VisibleInstanceID)
	}
	return removed, recovered, nil
}

// absentPlacementCollidesWithSeen reports whether an absent placement's path is
// now owned by a live (seen) document written in this sync. When true, removing
// the absent placement would silently delete the live document's file (bug
// #531), so the caller must skip the destructive sweep. It returns the live
// owner's VisibleInstanceID for diagnostics.
func absentPlacementCollidesWithSeen(placement placementMeta, seenCanonicalPaths map[string]string) (string, bool) {
	for _, candidate := range []string{placement.Path, placement.CanonicalPath} {
		if candidate == "" {
			continue
		}
		if owner, ok := seenCanonicalPaths[canonicalKey(candidate)]; ok && owner != placement.VisibleInstanceID {
			return owner, true
		}
	}
	return "", false
}

func preserveRecovery(ctx context.Context, paths commentbus.Paths, state *syncState, visibleID, slug, originalPath, reason string, localBytes []byte) error {
	now := time.Now().UTC()
	prefix := hashedPathID(visibleID) + "-" + now.Format("20060102T150405Z")
	file, err := os.CreateTemp(filepath.Join(paths.Home, "sync", "recovery"), prefix+"-*.local.md")
	if err != nil {
		return err
	}
	path := file.Name()
	defer func() {
		_ = os.Remove(path)
	}()
	if _, err := file.Write(localBytes); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	item := recoveryItem{
		ID:           strings.TrimSuffix(filepath.Base(path), ".local.md"),
		VisibleID:    visibleID,
		Slug:         slug,
		OriginalPath: originalPath,
		ArtifactPath: path,
		Reason:       reason,
		Status:       "pending",
		CreatedAt:    now,
	}
	if err := state.insertRecovery(ctx, item); err != nil {
		return err
	}
	if err := writeJSON0600(strings.TrimSuffix(path, ".local.md")+".json", item); err != nil {
		return err
	}
	path = ""
	return nil
}

func ListRecoveries(ctx context.Context, opts Options) ([]RecoveryItem, error) {
	paths, err := resolvePaths(opts.Home)
	if err != nil {
		return nil, err
	}
	state, err := openSyncState(ctx, paths)
	if err != nil {
		return nil, err
	}
	defer state.Close()
	items, err := state.listRecoveries(ctx, false)
	if err != nil {
		return nil, err
	}
	out := make([]RecoveryItem, 0, len(items))
	for _, item := range items {
		out = append(out, publicRecoveryItem(item))
	}
	return out, nil
}

func Recover(ctx context.Context, opts Options, target string, action RecoverAction) (RecoverResult, error) {
	if strings.TrimSpace(target) == "" {
		return RecoverResult{}, errors.New("missing recovery id or path")
	}
	paths, err := resolvePaths(opts.Home)
	if err != nil {
		return RecoverResult{}, err
	}
	lock, err := acquireSyncLock(paths)
	if err != nil {
		return RecoverResult{}, err
	}
	defer lock.Close()
	state, err := openSyncState(ctx, paths)
	if err != nil {
		return RecoverResult{}, err
	}
	defer state.Close()
	item, err := state.findRecovery(ctx, target)
	if err != nil {
		return RecoverResult{}, err
	}
	result := RecoverResult{ID: item.ID, ArtifactPath: item.ArtifactPath}
	switch action {
	case RecoverActionShow:
		return result, nil
	case RecoverActionDiff:
		canonical, _ := os.ReadFile(item.OriginalPath)
		recovered, err := os.ReadFile(item.ArtifactPath)
		if err != nil {
			return RecoverResult{}, err
		}
		result.Diff = simpleRecoveryDiff(item.OriginalPath, item.ArtifactPath, canonical, recovered)
		return result, nil
	case RecoverActionCopyNextToOriginal:
		data, err := os.ReadFile(item.ArtifactPath)
		if err != nil {
			return RecoverResult{}, err
		}
		output := recoveryCopyPath(item.OriginalPath, item.ID)
		if err := writeFileNoReplace(output, data, 0o600); err != nil {
			return RecoverResult{}, err
		}
		result.OutputPath = output
		return result, nil
	case RecoverActionDiscard:
		if err := state.setRecoveryStatus(ctx, item.ID, "discarded"); err != nil {
			return RecoverResult{}, err
		}
		if err := os.Remove(item.ArtifactPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return RecoverResult{}, err
		}
		result.Discarded = true
		return result, nil
	default:
		return RecoverResult{}, fmt.Errorf("unknown recovery action %q", action)
	}
}

func SetBackgroundSync(ctx context.Context, opts Options, enabled bool) error {
	paths, err := resolvePaths(opts.Home)
	if err != nil {
		return err
	}
	lock, err := acquireSyncLock(paths)
	if err != nil {
		return err
	}
	defer lock.Close()
	cfg, err := readConfig(paths)
	if err != nil {
		return err
	}
	cfg.BackgroundSync = enabled
	cfg.ManualOnly = !enabled
	cfg.ConfigGeneration++
	return writeJSON0600(filepath.Join(paths.Home, "sync", "config.json"), cfg)
}

func SetLiveSync(ctx context.Context, opts Options, enabled bool) error {
	paths, err := resolvePaths(opts.Home)
	if err != nil {
		return err
	}
	lock, err := acquireSyncLock(paths)
	if err != nil {
		return err
	}
	defer lock.Close()
	cfg, err := readConfig(paths)
	if err != nil {
		return err
	}
	cfg.LiveSyncEnabled = enabled
	cfg.ConfigGeneration++
	return writeJSON0600(filepath.Join(paths.Home, "sync", "config.json"), cfg)
}

func publicRecoveryItem(item recoveryItem) RecoveryItem {
	return RecoveryItem{
		ID:           item.ID,
		VisibleID:    item.VisibleID,
		Slug:         item.Slug,
		OriginalPath: item.OriginalPath,
		ArtifactPath: item.ArtifactPath,
		Reason:       item.Reason,
		Status:       item.Status,
		CreatedAt:    formatTime(item.CreatedAt),
	}
}

func simpleRecoveryDiff(originalPath, artifactPath string, canonical, recovered []byte) string {
	canonicalBody := projectionBodyForDirtyCheck(canonical)
	recoveredBody := projectionBodyForDirtyCheck(recovered)
	// If the body is unchanged but the files differ — e.g. an OKF
	// frontmatter-only edit (title/tags) — show the full content so the
	// recovered change is visible in the diff, not just preserved in the
	// artifact. (The full edited file is always saved either way.)
	if bytes.Equal(canonicalBody, recoveredBody) && !bytes.Equal(canonical, recovered) {
		canonicalBody = canonical
		recoveredBody = recovered
	}
	return strings.Join([]string{
		"--- " + originalPath,
		"+++ " + artifactPath,
		"@@",
		string(canonicalBody),
		"@@ recovered",
		string(recoveredBody),
	}, "\n")
}

func recoveryCopyPath(originalPath, recoveryID string) string {
	ext := filepath.Ext(originalPath)
	stem := strings.TrimSuffix(originalPath, ext)
	if ext == "" {
		ext = ".md"
	}
	return stem + ".recovered-" + shortStableSuffix(recoveryID) + ext
}

func writeFileNoReplace(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return file.Chmod(mode)
}

type syncLock struct {
	file *os.File
}

func acquireSyncLock(paths commentbus.Paths) (*syncLock, error) {
	lockPath := filepath.Join(paths.Home, "sync", "lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockSyncFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &syncLock{file: file}, nil
}

func (l *syncLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unlockSyncFile(l.file)
	return l.file.Close()
}

func validateRootOwnership(root string, cfg Config, paths commentbus.Paths) error {
	markerPath := filepath.Join(root, ".comment-sync-root.json")
	marker := map[string]any{}
	if err := readJSON(markerPath, &marker); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	if marker["managed_by"] != "comment sync" {
		return errors.New("sync root marker exists but is not managed by comment sync")
	}
	return validateRootMarker(marker, cfg, paths)
}

func validateRootMarker(marker map[string]any, cfg Config, paths commentbus.Paths) error {
	checks := []struct {
		field string
		want  string
	}{
		{field: "base_url_hash", want: sha256Hex(cfg.BaseURL)},
		{field: "human_id_hash", want: sha256Hex(cfg.HumanID)},
		{field: "state_home_id", want: sha256Hex(paths.Home)},
	}
	for _, check := range checks {
		got, ok := marker[check.field].(string)
		if ok && got != "" && got != check.want {
			return fmt.Errorf("sync root belongs to a different account, server, or COMMENT_IO_HOME (%s mismatch)", check.field)
		}
	}
	return nil
}

func ensureSafeRoot(root string) error {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return err
		}
		info, err = os.Lstat(root)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("sync root refuses symlink: %s", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("sync root is not a directory: %s", root)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("sync root is group/world writable: %s", root)
	}
	if !fileOwnedByCurrentUser(info) {
		return fmt.Errorf("sync root is owned by another user: %s", root)
	}
	return nil
}

func validateManagedPath(root, target string) error {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if !pathWithinRoot(root, target) {
		return fmt.Errorf("managed path escapes sync root: %s", target)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return err
	}
	if err := validateExistingPathComponent(root, true); err != nil {
		return err
	}
	current := root
	parts := strings.Split(filepath.Dir(rel), string(filepath.Separator))
	for _, part := range parts {
		if part == "." || part == "" {
			continue
		}
		current = filepath.Join(current, part)
		if err := validateExistingPathComponent(current, true); err != nil {
			return err
		}
	}
	if err := validateExistingPathComponent(target, false); err != nil {
		return err
	}
	return nil
}

func pathWithinRoot(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".." && !filepath.IsAbs(rel)
}

func validateExistingPathComponent(path string, mustBeDir bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("managed path refuses symlink: %s", path)
	}
	if mustBeDir {
		if !info.IsDir() {
			return fmt.Errorf("managed path ancestor is not a directory: %s", path)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("managed path ancestor is group/world writable: %s", path)
		}
		return nil
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("managed path target is not a regular file: %s", path)
	}
	if !fileHasSingleLink(info) {
		return fmt.Errorf("managed path target has multiple hard links: %s", path)
	}
	if !fileOwnedByCurrentUser(info) {
		return fmt.Errorf("managed path target is owned by another user: %s", path)
	}
	return nil
}

func readJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func writeJSON0600(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteFile(path, data, 0o600)
}

func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "."+base+".*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	cleanup = false
	return os.Chmod(path, mode)
}

func countMetadataFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}
	return count
}

func countMarkdownFiles(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".md") {
			count++
		}
	}
	return count
}

func encodePathID(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func hashedPathID(value string) string {
	return "sha256-" + sha256Hex(value)
}

func placementMetadataPath(paths commentbus.Paths, visibleID string) string {
	return filepath.Join(paths.Home, "sync", "metadata", "placements", hashedPathID(visibleID)+".json")
}

func legacyPlacementMetadataPath(paths commentbus.Paths, visibleID string) string {
	return filepath.Join(paths.Home, "sync", "metadata", "placements", encodePathID(visibleID)+".json")
}

func placementMetadataPaths(paths commentbus.Paths, visibleID string) []string {
	current := placementMetadataPath(paths, visibleID)
	legacy := legacyPlacementMetadataPath(paths, visibleID)
	if current == legacy {
		return []string{current}
	}
	return []string{current, legacy}
}

func readPlacementMetadata(paths commentbus.Paths, visibleID string) placementMeta {
	var meta placementMeta
	for _, path := range placementMetadataPaths(paths, visibleID) {
		if err := readJSON(path, &meta); err == nil {
			return meta
		}
	}
	return placementMeta{}
}

func removeLegacyPlacementMetadata(paths commentbus.Paths, visibleID string) {
	legacy := legacyPlacementMetadataPath(paths, visibleID)
	if legacy != placementMetadataPath(paths, visibleID) {
		_ = os.Remove(legacy)
	}
}

func removePlacementMetadata(paths commentbus.Paths, visibleID string) {
	for _, path := range placementMetadataPaths(paths, visibleID) {
		_ = os.Remove(path)
	}
}

func shortStableSuffix(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:4])
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func jsonEscape(value string) string {
	data, _ := json.Marshal(value)
	if len(data) < 2 {
		return ""
	}
	return string(data[1 : len(data)-1])
}

func defaultDeviceLabel() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "Local sync device"
	}
	return "Local sync on " + host
}

func librarySyncDeviceID(root string) string {
	host, err := os.Hostname()
	if err != nil {
		host = ""
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		absolute = root
	}
	input := "library-sync-device-v1\n" + strings.TrimSpace(strings.ToLower(host)) + "\n" + filepath.Clean(absolute)
	return "dev:" + sha256Hex(input)
}
