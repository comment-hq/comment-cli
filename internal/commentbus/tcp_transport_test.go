//go:build darwin || linux

package commentbus

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startTCPTestDaemon starts a daemon with the opt-in bus TCP listener bound to a
// loopback ephemeral port and returns its paths plus the resolved TCP address.
func startTCPTestDaemon(t *testing.T, ctx context.Context, bindAddr string, allowNonLoopback bool) (Paths, *Daemon) {
	t.Helper()
	paths := testDaemonPaths(t)
	daemon, err := startDaemonForTest(t, ctx, DaemonOptions{
		Paths:               paths,
		Version:             "test",
		BotletsHome:         filepath.Join(paths.Home, "botlets"),
		TCPListenAddr:       bindAddr,
		AllowNonLoopbackTCP: allowNonLoopback,
		Tmux:                newTestTmuxController(),
		Now: func() time.Time {
			return time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("start daemon with tcp listener: %v", err)
	}
	return paths, daemon
}

// requestTCPDaemon sends one newline-delimited JSON request over TCP and returns
// the decoded response, mirroring requestDaemon's unix-socket behavior.
func requestTCPDaemon(t *testing.T, addr string, request map[string]any) SocketResponse {
	t.Helper()
	payload, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial tcp daemon: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var response SocketResponse
	if err := json.Unmarshal(line, &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestDaemonTCPTransportRoundTripAndCapAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, daemon := startTCPTestDaemon(t, ctx, "127.0.0.1:0", false)
	defer daemon.Close()

	addr := daemon.TCPAddr()
	if addr == "" {
		t.Fatal("expected a non-empty TCP listener address")
	}

	// health over TCP returns the public subset: liveness + non-identifying
	// operational fields (so diagnostics degrade gracefully) but NONE of the
	// identifying metadata the Unix socket protects.
	health := requestTCPDaemon(t, addr, map[string]any{"id": "req_tcphealth", "op": "health", "params": map[string]any{}})
	if !health.OK {
		t.Fatalf("tcp health failed: %+v", health.Error)
	}
	healthResult, _ := health.Result.(map[string]any)
	for _, want := range []string{"version", "profiles_loaded", "features"} {
		if _, present := healthResult[want]; !present {
			t.Fatalf("public tcp health missing operational field %q: %#v", want, healthResult)
		}
	}
	// The reduced payload must be marked limited so consumers (doctor) don't
	// infer "all good" from counts alone.
	if limited, _ := healthResult["limited"].(bool); !limited {
		t.Fatalf("public tcp health must be marked limited:true, got %#v", healthResult)
	}
	for _, leaked := range []string{"socket_path", "agent_profile_handles", "daemon_id", "queued_mentions", "bus_tcp_addr", "daemon_paired", "pid", "notification_pollers"} {
		if _, present := healthResult[leaked]; present {
			t.Fatalf("public tcp health leaked identifying field %q: %#v", leaked, healthResult)
		}
	}

	capability, err := ReadCapability(daemon.paths.OwnerCapability)
	if err != nil {
		t.Fatal(err)
	}

	// A correct owner capability authorizes over TCP even though TCP has no
	// SO_PEERCRED — the capability token is the sole gate on the TCP transport.
	ok := requestTCPDaemon(t, addr, map[string]any{
		"id":     "req_tcpowner",
		"op":     "reload-profiles",
		"auth":   map[string]any{"mode": "owner", "capability": capability},
		"params": map[string]any{},
	})
	if !ok.OK {
		t.Fatalf("tcp owner-authed request failed: %+v", ok.Error)
	}

	// A non-health op with NO auth is rejected over TCP (capability is the sole
	// gate; the missing peer-cred does not open an unauthenticated hole).
	noAuth := requestTCPDaemon(t, addr, map[string]any{
		"id":     "req_tcpnoauth",
		"op":     "reload-profiles",
		"params": map[string]any{},
	})
	if noAuth.OK || noAuth.Error == nil || noAuth.Error.Code != "UNAUTHORIZED" {
		t.Fatalf("expected UNAUTHORIZED for missing auth over tcp, got %+v", noAuth)
	}

	// A wrong capability is rejected over TCP.
	bad := requestTCPDaemon(t, addr, map[string]any{
		"id":     "req_tcpbadcap",
		"op":     "reload-profiles",
		"auth":   map[string]any{"mode": "owner", "capability": "not-a-valid-capability-token-value"},
		"params": map[string]any{},
	})
	if bad.OK || bad.Error == nil || bad.Error.Code != "FORBIDDEN" {
		t.Fatalf("expected FORBIDDEN for bad capability over tcp, got %+v", bad)
	}
}

func TestCallSocketDialsTCPWhenConfigured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, daemon := startTCPTestDaemon(t, ctx, "127.0.0.1:0", false)
	defer daemon.Close()

	clientPaths := daemon.paths
	clientPaths.BusTCPAddr = daemon.TCPAddr() // the resolved ephemeral port

	resp, err := CallSocket(ctx, clientPaths, SocketRequest{
		ID:     "req_callsockettcp",
		Op:     "health",
		Params: map[string]any{},
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("CallSocket over tcp errored: %v", err)
	}
	if !resp.OK {
		t.Fatalf("CallSocket over tcp not ok: %+v", resp.Error)
	}
}

func TestDaemonTCPTransportRefusesNonLoopbackWithoutOptIn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths := testDaemonPaths(t)
	daemon, err := startDaemonForTest(t, ctx, DaemonOptions{
		Paths:         paths,
		Version:       "test",
		BotletsHome:   filepath.Join(paths.Home, "botlets"),
		TCPListenAddr: "0.0.0.0:0",
		Tmux:          newTestTmuxController(),
		Now: func() time.Time {
			return time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
		},
	})
	if err == nil {
		daemon.Close()
		t.Fatal("expected non-loopback TCP bind to be refused without AllowNonLoopbackTCP")
	}
}

func TestDaemonTCPTransportAllowsNonLoopbackWithOptIn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, daemon := startTCPTestDaemon(t, ctx, "0.0.0.0:0", true)
	defer daemon.Close()
	if daemon.TCPAddr() == "" {
		t.Fatal("expected a bound TCP listener with AllowNonLoopbackTCP")
	}
}

func TestCallSocketTCPOwnerHandshakeSucceeds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, daemon := startTCPTestDaemon(t, ctx, "127.0.0.1:0", false)
	defer daemon.Close()

	capability, err := ReadCapability(daemon.paths.OwnerCapability)
	if err != nil {
		t.Fatal(err)
	}
	clientPaths := daemon.paths
	clientPaths.BusTCPAddr = daemon.TCPAddr()

	// An owner-authed request over TCP performs the server-auth handshake against
	// the real daemon, then succeeds.
	resp, err := CallSocket(ctx, clientPaths, SocketRequest{
		ID:     "req_hsowner",
		Op:     "reload-profiles",
		Auth:   &SocketAuth{Mode: "owner", Capability: capability},
		Params: map[string]any{},
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("owner CallSocket over tcp errored: %v", err)
	}
	if !resp.OK {
		t.Fatalf("owner CallSocket over tcp not ok: %+v", resp.Error)
	}
}

func TestCallSocketTCPHandshakeRefusesImpostor(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A bogus listener that doesn't know the capability: it answers the handshake
	// with a wrong proof and records what the client sent.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	received := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		line, _ := br.ReadBytes('\n')
		received <- line
		_, _ = c.Write([]byte("{\"hs_proof\":\"deadbeefdeadbeef\"}\n"))
	}()

	clientPaths := Paths{Socket: "/nonexistent.sock", BusTCPAddr: ln.Addr().String()}
	_, err = CallSocket(ctx, clientPaths, SocketRequest{
		ID:     "req_impostor",
		Op:     "reload-profiles",
		Auth:   &SocketAuth{Mode: "owner", Capability: "as_realbutsecret_capability_value_abc123"},
		Params: map[string]any{},
	}, 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "handshake") {
		t.Fatalf("expected a handshake-auth refusal, got err=%v", err)
	}

	// The capability MUST NOT have been sent: the only thing the impostor received
	// is the handshake line, and that line carries no capability.
	select {
	case line := <-received:
		var got handshakeRequest
		if jsonErr := json.Unmarshal(line, &got); jsonErr != nil {
			t.Fatalf("impostor did not receive a handshake line: %v", jsonErr)
		}
		if got.Auth == nil || got.Auth.Capability != "" {
			t.Fatalf("capability leaked into the handshake: %s", string(line))
		}
		if strings.Contains(string(line), "as_realbutsecret") {
			t.Fatalf("capability value leaked to impostor: %s", string(line))
		}
	default:
		t.Fatal("impostor received nothing")
	}
}

func TestCallSocketTCPHandshakeLargeResponseNotTruncated(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const capability = "as_testcap_value_for_handshake_abc123"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	big := strings.Repeat("x", 2<<20) // 2 MiB, spans many bufio fills after the handshake

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		hsLine, _ := br.ReadBytes('\n')
		var hs handshakeRequest
		if json.Unmarshal(hsLine, &hs) != nil {
			return
		}
		// Prove daemon identity correctly so the client proceeds.
		_, _ = c.Write([]byte("{\"hs_proof\":\"" + handshakeProof(capability, hs.HSNonce) + "\"}\n"))
		_, _ = br.ReadBytes('\n') // the real request
		enc, _ := json.Marshal(SocketResponse{ID: "req_big", OK: true, Result: map[string]any{"blob": big}})
		_, _ = c.Write(append(enc, '\n'))
	}()

	clientPaths := Paths{Socket: "/nonexistent.sock", BusTCPAddr: ln.Addr().String()}
	resp, err := CallSocket(ctx, clientPaths, SocketRequest{
		ID:     "req_big",
		Op:     "messages.list",
		Auth:   &SocketAuth{Mode: "owner", Capability: capability},
		Params: map[string]any{},
	}, 10*time.Second)
	if err != nil {
		t.Fatalf("large response over handshake errored: %v", err)
	}
	if !resp.OK {
		t.Fatalf("large response not ok: %+v", resp.Error)
	}
	result, _ := resp.Result.(map[string]any)
	if s, _ := result["blob"].(string); len(s) != len(big) {
		t.Fatalf("response truncated by handshake budget: got %d bytes, want %d", len(s), len(big))
	}
}

func TestDaemonTCPOnlyDisablesUnixListener(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths := testDaemonPaths(t)
	daemon, err := startDaemonForTest(t, ctx, DaemonOptions{
		Paths:               paths,
		Version:             "test",
		BotletsHome:         filepath.Join(paths.Home, "botlets"),
		TCPListenAddr:       "127.0.0.1:0",
		DisableUnixListener: true,
		Tmux:                newTestTmuxController(),
		Now: func() time.Time {
			return time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
		},
	})
	if err != nil {
		t.Fatalf("start TCP-only daemon: %v", err)
	}
	defer daemon.Close()

	// No Unix socket file should be created when the Unix listener is disabled.
	if _, statErr := os.Stat(paths.Socket); !os.IsNotExist(statErr) {
		t.Fatalf("expected no socket file with DisableUnixListener, stat err = %v", statErr)
	}
	// The daemon is still reachable over TCP.
	health := requestTCPDaemon(t, daemon.TCPAddr(), map[string]any{"id": "req_tcponlyhealth", "op": "health", "params": map[string]any{}})
	if !health.OK {
		t.Fatalf("tcp-only health failed: %+v", health.Error)
	}
}

func TestDaemonNoListenerConfiguredIsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths := testDaemonPaths(t) // BusTCPAddr cleared by helper
	daemon, err := startDaemonForTest(t, ctx, DaemonOptions{
		Paths:               paths,
		Version:             "test",
		BotletsHome:         filepath.Join(paths.Home, "botlets"),
		DisableUnixListener: true, // and no TCP addr -> no listeners at all
		Tmux:                newTestTmuxController(),
		Now: func() time.Time {
			return time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
		},
	})
	if err == nil {
		daemon.Close()
		t.Fatal("expected an error when no listener is configured")
	}
}

func TestDaemonSingletonLockAcrossTransports(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paths := testDaemonPaths(t)
	opts := func(p Paths) DaemonOptions {
		return DaemonOptions{
			Paths:               p,
			Version:             "test",
			BotletsHome:         filepath.Join(p.Home, "botlets"),
			TCPListenAddr:       "127.0.0.1:0", // ephemeral — each gets a different port
			DisableUnixListener: true,
			Tmux:                newTestTmuxController(),
			Now: func() time.Time {
				return time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
			},
		}
	}
	d1, err := startDaemonForTest(t, ctx, opts(paths))
	if err != nil {
		t.Fatalf("first daemon failed to start: %v", err)
	}

	// A second TCP-only daemon on a DIFFERENT port but the SAME home must be
	// refused by the cross-transport lock (the TCP port bind alone wouldn't catch
	// a different port).
	d2, err2 := startDaemonForTest(t, ctx, opts(paths))
	if err2 == nil {
		d2.Close()
		d1.Close()
		t.Fatal("expected the singleton lock to refuse a second daemon on the same home")
	}

	// After the first releases the lock, a new daemon on the same home can start.
	d1.Close()
	d3, err3 := startDaemonForTest(t, ctx, opts(paths))
	if err3 != nil {
		t.Fatalf("expected a daemon to start after the first released the lock: %v", err3)
	}
	d3.Close()
}

func TestIsLoopbackTCPAddr(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:7700": true,
		"localhost:7700": true,
		"[::1]:7700":     true,
		"0.0.0.0:7700":   false,
		":7700":          false,
		"192.168.1.5:80": false,
	}
	for addr, want := range cases {
		if got := isLoopbackTCPAddr(addr); got != want {
			t.Errorf("isLoopbackTCPAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}
