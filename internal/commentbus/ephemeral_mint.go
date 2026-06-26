package commentbus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const daemonEphemeralMintTimeout = 60 * time.Second

var (
	daemonEphemeralEtherealHandleRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*[.]e-[0-9a-f]{8}$`)
	daemonEphemeralSecretRE         = regexp.MustCompile(`^as_[A-Za-z0-9._-]+$`)
)

// daemonEphemeralCred mirrors the on-disk shape cmd/comment writes to
// ethereal/<handle>.json, so the CLI's `comment ephemeral ensure` reuse path
// reads a daemon-minted credential identically to a CLI-minted one.
type daemonEphemeralCred struct {
	Handle        string `json:"handle"`
	AgentSecret   string `json:"agent_secret"`
	IdentityClass string `json:"identity_class,omitempty"`
	DisplayName   string `json:"display_name,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	BaseURL       string `json:"base_url,omitempty"`
	Owner         string `json:"owner,omitempty"`
	Session       string `json:"session,omitempty"`
}

// mintEphemeralViaPairing mints a session-scoped ethereal handle for the paired
// owner using ONLY the daemon's own pairing token, by proxying to the backend
// POST /daemon/agents/ephemeral. The owner's ark_ key is never involved, so a
// bootstrap session with no on-disk key still gets a named identity.
//
// The minted agent_secret is written straight to the ethereal store (0600) and
// is NEVER returned over the socket — the caller receives only the handle, the
// cred file path, and other non-secret fields, preserving the invariant that
// agent secrets do not cross the daemon socket. The mint targets the daemon's
// own paired BaseURL, so it can never go to the wrong deployment (this is also
// what fixes the CLI's comment.io-vs-comt.dev host default for no-key sessions).
// ephemeralPrepareStore creates the ethereal store, hardens its mode, rejects a
// symlinked store, and probes writability — so the caller can fail BEFORE
// consuming a one-time mint when the store is unusable (bad perms, symlink,
// full disk, or a read-only/mismatched container volume).
func ephemeralPrepareStore(dir string) *SocketError {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return socketError("INTERNAL_ERROR", "could not create ethereal store", false)
	}
	_ = os.Chmod(dir, 0o700)
	// Checked AFTER chmod so a swap between MkdirAll and now is still caught
	// (mirrors the CLI's ephemeralHardenDir).
	if lst, lerr := os.Lstat(dir); lerr != nil || lst.Mode()&os.ModeSymlink != 0 {
		return socketError("INTERNAL_ERROR", "ethereal store is not a regular directory; refusing to write", false)
	}
	probe, perr := os.CreateTemp(dir, ".probe-*")
	if perr != nil {
		return socketError("INTERNAL_ERROR", "ethereal store is not writable; refusing to mint", false)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(name)
	return nil
}

func (d *Daemon) mintEphemeralViaPairing(ctx context.Context, req SocketRequest) (map[string]any, *SocketError) {
	session, _ := req.Params["session"].(string)
	session = strings.TrimSpace(session)
	if session == "" {
		return nil, socketError("VALIDATION_ERROR", "session is required", false)
	}
	displayName, _ := req.Params["display_name"].(string)

	auth, ok, err := LoadDaemonAuth(d.paths)
	if err != nil {
		return nil, socketError("NOT_PAIRED", "daemon pairing credential is unreadable", false)
	}
	if !ok {
		return nil, socketError("NOT_PAIRED", "this computer is not paired; run `comment bus pair`", false)
	}
	base := strings.TrimRight(auth.BaseURL, "/")
	if base == "" {
		return nil, socketError("NOT_PAIRED", "daemon pairing has no base URL", false)
	}

	// Prepare AND probe the credential store BEFORE minting: the mint returns a
	// one-time agent_secret, so a store that can't be created/written afterward
	// would discard that secret and orphan the handle (burning the owner's mint
	// quota). Fail here, before any mint is consumed.
	etherealDir := filepath.Join(d.paths.Home, "ethereal")
	if sockErr := ephemeralPrepareStore(etherealDir); sockErr != nil {
		return nil, sockErr
	}

	body := map[string]any{}
	if strings.TrimSpace(displayName) != "" {
		body["display_name"] = displayName
	}
	payload, _ := json.Marshal(body)

	reqCtx, cancel := context.WithTimeout(ctx, daemonEphemeralMintTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, base+"/daemon/agents/ephemeral", bytes.NewReader(payload))
	if err != nil {
		return nil, socketError("INTERNAL_ERROR", "could not build mint request", false)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+auth.Token)
	httpReq.Header.Set("X-Comment-CLI-Version", d.version)

	client := &http.Client{Timeout: daemonEphemeralMintTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, socketError("UPSTREAM_UNAVAILABLE", "mint request failed", true)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		// Never echo the body — it carries no secret, but stay conservative.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, socketError("UPSTREAM_ERROR", fmt.Sprintf("mint failed (HTTP %d)", resp.StatusCode), resp.StatusCode >= 500)
	}

	var minted struct {
		Handle      string `json:"handle"`
		AgentSecret string `json:"agent_secret"`
		DisplayName string `json:"display_name"`
		ExpiresAt   string `json:"expires_at"`
		Owner       string `json:"owner"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&minted); err != nil {
		return nil, socketError("UPSTREAM_ERROR", "mint response parse failed", false)
	}
	// Validate before trusting: the handle becomes a filename and the secret
	// becomes an Authorization header value downstream.
	if !daemonEphemeralSecretRE.MatchString(minted.AgentSecret) ||
		!daemonEphemeralEtherealHandleRE.MatchString(minted.Handle) ||
		strings.Contains(minted.Handle, "..") {
		return nil, socketError("UPSTREAM_ERROR", "mint returned a malformed handle/secret", false)
	}

	cred := daemonEphemeralCred{
		Handle:        minted.Handle,
		AgentSecret:   minted.AgentSecret,
		IdentityClass: "ethereal",
		DisplayName:   minted.DisplayName,
		ExpiresAt:     minted.ExpiresAt,
		BaseURL:       base,
		Owner:         minted.Owner,
		Session:       session,
	}
	data, err := json.Marshal(cred)
	if err != nil {
		return nil, socketError("INTERNAL_ERROR", "could not encode credential", false)
	}
	credPath := filepath.Join(etherealDir, minted.Handle+".json")
	if err := WritePrivateFileAtomic(credPath, data, 0o600); err != nil {
		return nil, socketError("INTERNAL_ERROR", "could not persist credential", false)
	}

	// Return ONLY non-secret fields; the CLI reads the secret back from the
	// 0600 cred file and writes the session->handle bind itself.
	return map[string]any{
		"handle":       minted.Handle,
		"cred_file":    credPath,
		"base_url":     base,
		"expires_at":   minted.ExpiresAt,
		"display_name": minted.DisplayName,
		"owner":        minted.Owner,
	}, nil
}
