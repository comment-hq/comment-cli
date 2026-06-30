//go:build darwin || linux

package commentbus

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// pairTestDaemon writes a daemon-auth.json pointing at baseURL and returns a
// Daemon whose home is a fresh temp dir.
func pairTestDaemon(t *testing.T, baseURL string) *Daemon {
	t.Helper()
	paths, err := ResolvePaths(filepath.Join(t.TempDir(), ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveDaemonAuth(paths, DaemonAuth{
		DaemonID:     "ld_11111111-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222",
		Token:        "ldt_ag_owner_ld_x_secret-token-value",
		BaseURL:      baseURL,
		Label:        "Test Box",
		Capabilities: []string{"agent_enrollment:v1"},
		PairedAt:     "2026-06-10T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	return &Daemon{paths: paths, version: "test"}
}

func TestMintEphemeralViaPairing_HappyPath(t *testing.T) {
	var gotAuth, gotBody string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/daemon/agents/ephemeral" || r.Method != http.MethodPost {
			http.Error(w, "unexpected route", http.StatusNotFound)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent_id":     "ag_abc123",
			"agent_secret": "as_ag_abc123_secret",
			"handle":       "owner.e-deadbeef",
			"actor_id":     "ai:owner.e-deadbeef",
			"display_name": "Sam (Sync)",
			"expires_at":   "2026-07-22T00:00:00.000Z",
			"owner":        "owner",
		})
	}))
	defer backend.Close()

	d := pairTestDaemon(t, backend.URL)
	result, sockErr := d.mintEphemeralViaPairing(context.Background(), SocketRequest{
		Params: map[string]any{"session": "sess-abc", "display_name": "Sam (Sync)"},
	})
	if sockErr != nil {
		t.Fatalf("mint err = %+v", sockErr)
	}

	// The daemon forwarded its pairing token + the display_name.
	if gotAuth != "Bearer ldt_ag_owner_ld_x_secret-token-value" {
		t.Fatalf("forwarded auth = %q", gotAuth)
	}
	if gotBody != `{"display_name":"Sam (Sync)"}` {
		t.Fatalf("forwarded body = %q", gotBody)
	}

	// The socket result carries NO secret — only non-secret fields.
	if _, leaked := result["agent_secret"]; leaked {
		t.Fatal("agent_secret leaked into socket result")
	}
	if result["handle"] != "owner.e-deadbeef" {
		t.Fatalf("result handle = %v", result["handle"])
	}
	if result["base_url"] != backend.URL {
		t.Fatalf("result base_url = %v, want %v", result["base_url"], backend.URL)
	}
	credPath, _ := result["cred_file"].(string)
	if credPath != filepath.Join(d.paths.Home, "ephemeral", "owner.e-deadbeef.json") {
		t.Fatalf("result cred_file = %v", credPath)
	}

	// The secret-bearing cred file is written 0600 with the full shape.
	info, err := os.Stat(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("cred file mode = %v, want 0600", info.Mode().Perm())
	}
	var cred daemonEphemeralCred
	data, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cred); err != nil {
		t.Fatal(err)
	}
	if cred.AgentSecret != "as_ag_abc123_secret" {
		t.Fatalf("cred secret = %q", cred.AgentSecret)
	}
	if cred.Session != "sess-abc" {
		t.Fatalf("cred session = %q, want sess-abc", cred.Session)
	}
	if cred.BaseURL != backend.URL {
		t.Fatalf("cred base_url = %q, want %q", cred.BaseURL, backend.URL)
	}
	if cred.Handle != "owner.e-deadbeef" {
		t.Fatalf("cred handle = %q", cred.Handle)
	}
	if cred.IdentityClass != "ephemeral" {
		t.Fatalf("cred identity_class = %q, want ephemeral", cred.IdentityClass)
	}
	legacyPath := filepath.Join(d.paths.Home, "ethereal", "owner.e-deadbeef.json")
	legacyInfo, err := os.Stat(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if legacyInfo.Mode().Perm() != 0o600 {
		t.Fatalf("legacy cred file mode = %v, want 0600", legacyInfo.Mode().Perm())
	}
	var legacyCred daemonEphemeralCred
	legacyData, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(legacyData, &legacyCred); err != nil {
		t.Fatal(err)
	}
	if legacyCred.AgentSecret != "as_ag_abc123_secret" {
		t.Fatalf("legacy cred secret = %q", legacyCred.AgentSecret)
	}
	if legacyCred.IdentityClass != "ethereal" {
		t.Fatalf("legacy cred identity_class = %q, want ethereal", legacyCred.IdentityClass)
	}
}

// TestMintEphemeralOverSocketNoAuth exercises the FULL socket path (dial →
// HandleRequest → authorize → dispatch → handler), not just the handler, so it
// catches the auth-gate regression where authorize() rejected the auth-optional
// op before dispatch. The request carries no "auth" field at all.
func TestMintEphemeralOverSocketNoAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"agent_id":     "ag_abc123",
			"agent_secret": "as_ag_abc123_secret",
			"handle":       "owner.e-deadbeef",
			"display_name": "Sam",
			"expires_at":   "2026-07-22T00:00:00.000Z",
			"owner":        "owner",
		})
	}))
	defer backend.Close()

	paths, daemon := startTestDaemon(t, ctx, nil)
	defer daemon.Close()
	if err := SaveDaemonAuth(paths, DaemonAuth{
		DaemonID: "ld_11111111-1111-4111-8111-111111111111_22222222-2222-4222-8222-222222222222",
		Token:    "ldt_ag_owner_ld_x_secret-token-value",
		BaseURL:  backend.URL,
	}); err != nil {
		t.Fatal(err)
	}

	resp := requestDaemon(t, paths, map[string]any{
		"id":     "req_mintephemeral",
		"op":     "agents.mint-ephemeral",
		"params": map[string]any{"session": "sess-xyz"},
	})
	if !resp.OK {
		t.Fatalf("no-auth mint over socket failed (authorize regression?): %+v", resp.Error)
	}
	result, _ := resp.Result.(map[string]any)
	if result["handle"] != "owner.e-deadbeef" {
		t.Fatalf("handle = %v", result["handle"])
	}
	// The secret must never appear in the socket response.
	if enc := mustJSON(t, resp); containsSecretValue(enc) {
		t.Fatalf("socket response leaked a secret-shaped value: %s", enc)
	}
	// The daemon persisted the secret-bearing cred file at 0600.
	credPath := filepath.Join(paths.Home, "ephemeral", "owner.e-deadbeef.json")
	info, err := os.Stat(credPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cred file stat = %v / err %v", info, err)
	}
}

func TestMintEphemeralViaPairing_StoreCheckedBeforeMint(t *testing.T) {
	var calls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"handle": "owner.e-deadbeef", "agent_secret": "as_x_y", "expires_at": "2026-07-22T00:00:00.000Z",
		})
	}))
	defer backend.Close()

	d := pairTestDaemon(t, backend.URL)
	// Make the ephemeral store impossible to create: a regular file sits where the
	// directory should go, so MkdirAll fails. The mint must NOT be consumed.
	if err := os.WriteFile(filepath.Join(d.paths.Home, "ephemeral"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, sockErr := d.mintEphemeralViaPairing(context.Background(), SocketRequest{
		Params: map[string]any{"session": "sess-abc"},
	})
	if sockErr == nil || sockErr.Code != "INTERNAL_ERROR" {
		t.Fatalf("err = %+v, want INTERNAL_ERROR", sockErr)
	}
	if calls != 0 {
		t.Fatalf("backend called %d times; the store must be validated BEFORE minting", calls)
	}
}

func TestMintEphemeralViaPairing_LegacyStoreCheckedBeforeMint(t *testing.T) {
	var calls int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"handle": "owner.e-deadbeef", "agent_secret": "as_x_y", "expires_at": "2026-07-22T00:00:00.000Z",
		})
	}))
	defer backend.Close()

	d := pairTestDaemon(t, backend.URL)
	if err := os.WriteFile(filepath.Join(d.paths.Home, "ethereal"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, sockErr := d.mintEphemeralViaPairing(context.Background(), SocketRequest{
		Params: map[string]any{"session": "sess-abc"},
	})
	if sockErr == nil || sockErr.Code != "INTERNAL_ERROR" {
		t.Fatalf("err = %+v, want INTERNAL_ERROR", sockErr)
	}
	if calls != 0 {
		t.Fatalf("backend called %d times; the legacy store must be validated BEFORE minting", calls)
	}
}

func TestMintEphemeralViaPairing_NotPaired(t *testing.T) {
	paths, err := ResolvePaths(filepath.Join(t.TempDir(), ".comment-io"))
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{paths: paths, version: "test"}
	_, sockErr := d.mintEphemeralViaPairing(context.Background(), SocketRequest{
		Params: map[string]any{"session": "sess-abc"},
	})
	if sockErr == nil || sockErr.Code != "NOT_PAIRED" {
		t.Fatalf("err = %+v, want NOT_PAIRED", sockErr)
	}
}

func TestMintEphemeralViaPairing_MissingSession(t *testing.T) {
	d := pairTestDaemon(t, "https://comt.dev")
	_, sockErr := d.mintEphemeralViaPairing(context.Background(), SocketRequest{
		Params: map[string]any{},
	})
	if sockErr == nil || sockErr.Code != "VALIDATION_ERROR" {
		t.Fatalf("err = %+v, want VALIDATION_ERROR", sockErr)
	}
}

func TestMintEphemeralViaPairing_BackendError(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusUnauthorized)
	}))
	defer backend.Close()
	d := pairTestDaemon(t, backend.URL)
	_, sockErr := d.mintEphemeralViaPairing(context.Background(), SocketRequest{
		Params: map[string]any{"session": "sess-abc"},
	})
	if sockErr == nil || sockErr.Code != "UPSTREAM_ERROR" {
		t.Fatalf("err = %+v, want UPSTREAM_ERROR", sockErr)
	}
}

func TestMintEphemeralViaPairing_MalformedMint(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		// Secret does not match the as_ shape — must be rejected, not persisted.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"handle":       "owner.e-deadbeef",
			"agent_secret": "not-a-secret",
			"expires_at":   "2026-07-22T00:00:00.000Z",
		})
	}))
	defer backend.Close()
	d := pairTestDaemon(t, backend.URL)
	_, sockErr := d.mintEphemeralViaPairing(context.Background(), SocketRequest{
		Params: map[string]any{"session": "sess-abc"},
	})
	if sockErr == nil || sockErr.Code != "UPSTREAM_ERROR" {
		t.Fatalf("err = %+v, want UPSTREAM_ERROR", sockErr)
	}
	if _, err := os.Stat(filepath.Join(d.paths.Home, "ephemeral", "owner.e-deadbeef.json")); !os.IsNotExist(err) {
		t.Fatal("a malformed mint must not persist a cred file")
	}
}

func TestValidateSocketRequest_MintEphemeralAuthOptional(t *testing.T) {
	// A no-auth mint request is accepted (bootstrap path) when params are valid.
	if err := ValidateSocketRequest(SocketRequest{
		ID: "req_abc", Op: "agents.mint-ephemeral", Params: map[string]any{"session": "sess-1"},
	}); err != nil {
		t.Fatalf("no-auth mint envelope rejected: %v", err)
	}
	// Param validation still applies (missing session fails).
	if err := ValidateSocketRequest(SocketRequest{
		ID: "req_abc", Op: "agents.mint-ephemeral", Params: map[string]any{},
	}); err == nil {
		t.Fatal("missing session should fail param validation")
	}
}

func TestValidateMintEphemeralParams(t *testing.T) {
	cases := []struct {
		name   string
		params map[string]any
		wantOK bool
	}{
		{"valid session only", map[string]any{"session": "d8dd151b-dc4b-4b99"}, true},
		{"valid with display_name", map[string]any{"session": "sess1", "display_name": "Sam"}, true},
		{"missing session", map[string]any{}, false},
		{"empty session", map[string]any{"session": ""}, false},
		{"unknown param", map[string]any{"session": "s", "nope": "x"}, false},
		{"secret in display_name", map[string]any{"session": "s", "display_name": "as_ag_x_secret"}, false},
		{"non-string display_name", map[string]any{"session": "s", "display_name": 5}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMintEphemeralParams(tc.params)
			if tc.wantOK && err != nil {
				t.Fatalf("got err %v, want ok", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatal("got ok, want err")
			}
		})
	}
}
