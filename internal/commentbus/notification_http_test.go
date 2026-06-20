package commentbus

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const testNotificationLeaseOpID = "op_httplease1234567890a"

func TestHTTPNotificationClientLeaseNotification(t *testing.T) {
	var seenPath, seenAuth, seenIdempotencyKey, seenHolder, seenUserAgent string
	var seenLeaseTTL int64
	var seenKinds []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenIdempotencyKey = r.Header.Get("Idempotency-Key")
		seenUserAgent = r.Header.Get("User-Agent")
		var body struct {
			LeaseHolder string   `json:"lease_holder"`
			LeaseTTLMS  int64    `json:"lease_ttl_ms"`
			Kinds       []string `json:"kinds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("request body decode failed: %v", err)
		}
		seenHolder = body.LeaseHolder
		seenLeaseTTL = body.LeaseTTLMS
		seenKinds = body.Kinds
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"ok":true,
			"lease":{
				"claim_id":"clm_httpclaim1234567890",
				"notification_id":"ntf_httpnotification1234567890",
				"claimed_at":"2026-05-07T00:00:00Z",
				"lease_expires_at":"2026-05-07T00:10:00Z",
				"notification":{
					"id":"ntf_httpnotification1234567890",
					"type":"mention",
					"doc_slug":"abc123",
					"doc_title":"Design Review",
					"context":"Please review.",
					"from_handle":"max.sender",
					"from_name":"Max",
					"comment_id":null,
					"suggestion_id":null,
					"created_at":"2026-05-07T00:00:00Z",
					"read":false,
					"access_token":"550e8400-e29b-41d4-a716-446655440000"
				}
			},
			"leases":[]
		}`))
	}))
	defer server.Close()

	client := NewVersionedHTTPNotificationClient("test-version", server.Client())
	lease, err := client.LeaseNotification(context.Background(), AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     server.URL + "/",
	}, 2*time.Minute, "comment-bus:max.reviewer", testNotificationLeaseOpID, "botlets.task")
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil || lease.ClaimID != "clm_httpclaim1234567890" || lease.Notification.AccessToken == "" {
		t.Fatalf("lease = %+v", lease)
	}
	if seenPath != "/agents/me/notifications/lease" || seenLeaseTTL != 120000 {
		t.Fatalf("request path/body = %q lease=%d", seenPath, seenLeaseTTL)
	}
	if seenAuth != "Bearer as_http_secret" || seenIdempotencyKey != testNotificationLeaseOpID || seenHolder != "comment-bus:max.reviewer" || seenUserAgent != "comment-bus/test-version" {
		t.Fatalf("request headers/body auth=%q op=%q holder=%q ua=%q", seenAuth, seenIdempotencyKey, seenHolder, seenUserAgent)
	}
	if len(seenKinds) != 1 || seenKinds[0] != "botlets.task" {
		t.Fatalf("request kinds = %+v, want botlets.task", seenKinds)
	}
}

func TestHTTPNotificationClientLeaseNotificationEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"lease":null,"leases":[]}`))
	}))
	defer server.Close()

	client := NewHTTPNotificationClient(nil)
	lease, err := client.LeaseNotification(context.Background(), AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     server.URL,
	}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
	if err != nil {
		t.Fatal(err)
	}
	if lease != nil {
		t.Fatalf("lease = %+v, want nil", lease)
	}
}

func TestHTTPNotificationClientWaitNotificationWake(t *testing.T) {
	var seenPath, seenClient, seenAuth, seenUserAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenClient = r.URL.Query().Get("client")
		seenAuth = r.Header.Get("Authorization")
		seenUserAgent = r.Header.Get("User-Agent")
		if r.Header.Get("Upgrade") != "websocket" {
			t.Fatalf("missing websocket upgrade: %s", r.Header.Get("Upgrade"))
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server does not support hijacking")
		}
		conn, rw, err := hijacker.Hijack()
		if err != nil {
			t.Fatalf("hijack failed: %v", err)
		}
		defer conn.Close()
		key := r.Header.Get("Sec-WebSocket-Key")
		hash := sha1.Sum([]byte(key + websocketAcceptGUID))
		accept := base64.StdEncoding.EncodeToString(hash[:])
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
		_, _ = rw.WriteString("Upgrade: websocket\r\n")
		_, _ = rw.WriteString("Connection: Upgrade\r\n")
		_, _ = rw.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
		if err := rw.Flush(); err != nil {
			t.Fatalf("handshake flush failed: %v", err)
		}
		opcode, payload, err := readWebSocketServerFrame(rw.Reader)
		if err != nil {
			t.Fatalf("client ping read failed: %v", err)
		}
		if opcode != 0x1 || string(payload) != `{"type":"ping"}` {
			t.Fatalf("client wake ping opcode=%d payload=%s", opcode, payload)
		}
		if err := writeTestWebSocketServerTextFrame(rw, []byte(`{"type":"notification_wake","wake_id":"wake_empty","unread_count":0}`)); err != nil {
			t.Fatalf("empty wake write failed: %v", err)
		}
		if err := writeTestWebSocketServerTextFrame(rw, []byte(`{"type":"notification_wake","wake_id":"wake_ready","unread_count":2,"newest_notification_id":"ntf_ready123"}`)); err != nil {
			t.Fatalf("ready wake write failed: %v", err)
		}
	}))
	defer server.Close()

	client := NewVersionedHTTPNotificationClient("test-version", server.Client())
	wake, err := client.WaitNotificationWake(context.Background(), AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     server.URL + "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if wake == nil || wake.WakeID != "wake_ready" || wake.UnreadCount != 2 || wake.NewestNotificationID != "ntf_ready123" {
		t.Fatalf("wake = %+v", wake)
	}
	if seenPath != "/agents/me/notifications/connect" || seenClient != "daemon" || seenAuth != "Bearer as_http_secret" || seenUserAgent != "comment-bus/test-version" {
		t.Fatalf("wake request path=%q client=%q auth=%q ua=%q", seenPath, seenClient, seenAuth, seenUserAgent)
	}
}

func TestHTTPNotificationClientWaitNotificationWakeSendsProtocolPing(t *testing.T) {
	withNotificationWakeTimings(t, 15*time.Millisecond, time.Second)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw := writeTestNotificationWakeHandshake(t, w, r)
		defer conn.Close()
		opcode, payload, err := readWebSocketServerFrame(rw.Reader)
		if err != nil {
			t.Fatalf("client text ping read failed: %v", err)
		}
		if opcode != 0x1 || string(payload) != `{"type":"ping"}` {
			t.Fatalf("client wake ping opcode=%d payload=%s", opcode, payload)
		}
		opcode, payload, err = readWebSocketServerFrame(rw.Reader)
		if err != nil {
			t.Fatalf("client protocol ping read failed: %v", err)
		}
		if opcode != 0x9 || len(payload) != 0 {
			t.Fatalf("client protocol ping opcode=%d payload=%q", opcode, payload)
		}
		if err := writeTestWebSocketServerTextFrame(rw, []byte(`{"type":"notification_wake","wake_id":"wake_protocol_ping","unread_count":1}`)); err != nil {
			t.Fatalf("ready wake write failed: %v", err)
		}
	}))
	defer server.Close()

	client := NewHTTPNotificationClient(server.Client())
	wake, err := client.WaitNotificationWake(context.Background(), AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     server.URL + "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if wake == nil || wake.WakeID != "wake_protocol_ping" {
		t.Fatalf("wake = %+v", wake)
	}
}

func TestHTTPNotificationClientWaitNotificationWakeRepliesToServerPing(t *testing.T) {
	withNotificationWakeTimings(t, time.Hour, time.Second)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw := writeTestNotificationWakeHandshake(t, w, r)
		defer conn.Close()
		opcode, payload, err := readWebSocketServerFrame(rw.Reader)
		if err != nil {
			t.Fatalf("client text ping read failed: %v", err)
		}
		if opcode != 0x1 || string(payload) != `{"type":"ping"}` {
			t.Fatalf("client wake ping opcode=%d payload=%s", opcode, payload)
		}
		if err := writeTestWebSocketServerFrame(rw, 0x9, []byte("server-ping")); err != nil {
			t.Fatalf("server ping write failed: %v", err)
		}
		opcode, payload, err = readWebSocketServerFrame(rw.Reader)
		if err != nil {
			t.Fatalf("client pong read failed: %v", err)
		}
		if opcode != 0xA || string(payload) != "server-ping" {
			t.Fatalf("client pong opcode=%d payload=%q", opcode, payload)
		}
		if err := writeTestWebSocketServerTextFrame(rw, []byte(`{"type":"notification_wake","wake_id":"wake_server_ping","unread_count":1}`)); err != nil {
			t.Fatalf("ready wake write failed: %v", err)
		}
	}))
	defer server.Close()

	client := NewHTTPNotificationClient(server.Client())
	wake, err := client.WaitNotificationWake(context.Background(), AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     server.URL + "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if wake == nil || wake.WakeID != "wake_server_ping" {
		t.Fatalf("wake = %+v", wake)
	}
}

func TestHTTPNotificationClientWaitNotificationWakeReadDeadlineReconnects(t *testing.T) {
	withNotificationWakeTimings(t, time.Hour, 25*time.Millisecond)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw := writeTestNotificationWakeHandshake(t, w, r)
		defer conn.Close()
		opcode, payload, err := readWebSocketServerFrame(rw.Reader)
		if err != nil {
			t.Fatalf("client text ping read failed: %v", err)
		}
		if opcode != 0x1 || string(payload) != `{"type":"ping"}` {
			t.Fatalf("client wake ping opcode=%d payload=%s", opcode, payload)
		}
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}))
	defer server.Close()
	defer close(done)

	client := NewHTTPNotificationClient(server.Client())
	start := time.Now()
	wake, err := client.WaitNotificationWake(context.Background(), AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     server.URL + "/",
	})
	if wake != nil || !errors.Is(err, errNotificationWakeAmbiguous) {
		t.Fatalf("wake=%+v err=%v", wake, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("read deadline took %s", elapsed)
	}
}

func TestHTTPNotificationClientWaitNotificationWakeReturnsAgentAuthRevokedOnClose4431(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw := writeTestNotificationWakeHandshake(t, w, r)
		defer conn.Close()
		opcode, _, err := readWebSocketServerFrame(rw.Reader)
		if err != nil {
			t.Fatalf("client ping read failed: %v", err)
		}
		if opcode != 0x1 {
			t.Fatalf("expected client ping opcode 0x1, got %d", opcode)
		}
		// Server-side close frame carrying code 4431 (WS_AGENT_AUTH_REVOKED_CLOSE_CODE).
		closePayload := []byte{0x11, 0x4F, 'a', 'g', 'e', 'n', 't', '_', 'a', 'u', 't', 'h', '_', 'r', 'e', 'v', 'o', 'k', 'e', 'd'}
		if err := writeTestWebSocketServerFrame(rw, 0x8, closePayload); err != nil {
			t.Fatalf("close frame write failed: %v", err)
		}
	}))
	defer server.Close()

	client := NewHTTPNotificationClient(server.Client())
	wake, err := client.WaitNotificationWake(context.Background(), AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     server.URL + "/",
	})
	if wake != nil {
		t.Fatalf("expected nil wake, got %+v", wake)
	}
	if !errors.Is(err, ErrAgentAuthRevoked) {
		t.Fatalf("expected ErrAgentAuthRevoked, got %v", err)
	}
	// The ambiguous error must NOT be returned — callers distinguish revoked
	// auth from transient socket close to avoid an infinite reconnect loop.
	if errors.Is(err, errNotificationWakeAmbiguous) {
		t.Fatalf("ErrAgentAuthRevoked must not wrap errNotificationWakeAmbiguous: %v", err)
	}
}

func TestHTTPNotificationClientWaitNotificationWakeUnrecognizedCloseCodeIsAmbiguous(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw := writeTestNotificationWakeHandshake(t, w, r)
		defer conn.Close()
		opcode, _, err := readWebSocketServerFrame(rw.Reader)
		if err != nil {
			t.Fatalf("client ping read failed: %v", err)
		}
		if opcode != 0x1 {
			t.Fatalf("expected client ping opcode 0x1, got %d", opcode)
		}
		// 1011 = internal server error; unrelated to agent auth.
		if err := writeTestWebSocketServerFrame(rw, 0x8, []byte{0x03, 0xF3}); err != nil {
			t.Fatalf("close frame write failed: %v", err)
		}
	}))
	defer server.Close()

	client := NewHTTPNotificationClient(server.Client())
	wake, err := client.WaitNotificationWake(context.Background(), AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     server.URL + "/",
	})
	if wake != nil {
		t.Fatalf("expected nil wake, got %+v", wake)
	}
	if errors.Is(err, ErrAgentAuthRevoked) {
		t.Fatalf("non-4431 close must not surface as ErrAgentAuthRevoked: %v", err)
	}
	if !errors.Is(err, errNotificationWakeAmbiguous) {
		t.Fatalf("expected errNotificationWakeAmbiguous, got %v", err)
	}
}

func TestHTTPNotificationClientWaitNotificationWakeCancelUnblocksBlockedWrite(t *testing.T) {
	withNotificationWakeTimings(t, time.Hour, time.Hour)
	withNotificationWakeWriteTimeout(t, time.Hour)
	conn := newBlockingWakeConn()
	client := NewHTTPNotificationClient(&http.Client{
		Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return conn, nil
			},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	wake, err := client.WaitNotificationWake(ctx, AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     "http://127.0.0.1:1/",
	})
	if wake != nil || !errors.Is(err, errNotificationWakeDeadline) {
		t.Fatalf("wake=%+v err=%v", wake, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("blocked write cancellation took %s", elapsed)
	}
	if blocked := conn.blockedWriteCount(); blocked == 0 {
		t.Fatalf("blocked write count = %d, want > 0", blocked)
	}
}

func TestHTTPNotificationClientWaitNotificationWakeCancelAfterReadDeadlineRefresh(t *testing.T) {
	withNotificationWakeTimings(t, time.Hour, time.Hour)
	withNotificationWakeWriteTimeout(t, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := newCancelAfterReadDeadlineWakeConn(cancel)
	client := NewHTTPNotificationClient(&http.Client{
		Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return conn, nil
			},
		},
	})
	start := time.Now()
	wake, err := client.WaitNotificationWake(ctx, AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     "http://127.0.0.1:1/",
	})
	if wake != nil || !errors.Is(err, errNotificationWakeDeadline) {
		t.Fatalf("wake=%+v err=%v", wake, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("post-refresh cancellation took %s", elapsed)
	}
	if !conn.cancelRaceObserved() {
		t.Fatal("cancel/read-deadline race was not exercised")
	}
}

func TestHTTPNotificationClientWaitNotificationWakeTickerWriteFailureIsSticky(t *testing.T) {
	withNotificationWakeTimings(t, 10*time.Millisecond, time.Hour)
	withNotificationWakeWriteTimeout(t, time.Hour)
	conn := newTickerFailureWakeConn()
	client := NewHTTPNotificationClient(&http.Client{
		Transport: &http.Transport{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return conn, nil
			},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	wake, err := client.WaitNotificationWake(ctx, AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     "http://127.0.0.1:1/",
	})
	if wake != nil || !errors.Is(err, errNotificationWakeAmbiguous) {
		t.Fatalf("wake=%+v err=%v", wake, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("ticker write failure took %s", elapsed)
	}
	if failures := conn.protocolPingFailureCount(); failures == 0 {
		t.Fatalf("protocol ping failures = %d, want > 0", failures)
	}
}

func TestLockedConnSerializesFrameWrites(t *testing.T) {
	conn := &concurrentWriteDetectConn{}
	lc := &lockedConn{conn: conn}
	start := make(chan struct{})
	errs := make(chan error, 16)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs <- lc.writeFrame(byte(0x1+(i%2)), []byte("payload"))
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("writeFrame failed: %v", err)
		}
	}
	if maxActive := conn.maxActiveWrites(); maxActive != 1 {
		t.Fatalf("max concurrent writes = %d, want 1", maxActive)
	}
}

func TestLockedConnWriteFrameUsesWriteDeadline(t *testing.T) {
	withNotificationWakeWriteTimeout(t, 25*time.Millisecond)
	conn := newBlockingWakeConn()
	lc := &lockedConn{conn: conn}
	start := time.Now()
	err := lc.writeFrame(0x9, nil)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("writeFrame err = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("write deadline took %s", elapsed)
	}
	if blocked := conn.blockedWriteCount(); blocked == 0 {
		t.Fatalf("blocked write count = %d, want > 0", blocked)
	}
}

func TestHTTPNotificationClientWaitNotificationWakeUsesTransportTLSConfig(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTestNotificationWakeUpgrade(t, w, r, []byte(`{"type":"notification_wake","wake_id":"wake_tls","unread_count":1}`))
	}))
	defer server.Close()

	client := NewHTTPNotificationClient(server.Client())
	wake, err := client.WaitNotificationWake(context.Background(), AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     server.URL + "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if wake == nil || wake.WakeID != "wake_tls" || wake.UnreadCount != 1 {
		t.Fatalf("wake = %+v", wake)
	}
}

func TestNotificationWakeTLSConfigForcesHTTP1(t *testing.T) {
	cfg := notificationWakeTLSConfig(&http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			NextProtos: []string{
				"h2",
				"http/1.1",
			},
		},
	}, "comment.example")
	if cfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("min version = %x, want %x", cfg.MinVersion, tls.VersionTLS13)
	}
	if cfg.ServerName != "comment.example" {
		t.Fatalf("server name = %q", cfg.ServerName)
	}
	if len(cfg.NextProtos) != 1 || cfg.NextProtos[0] != "http/1.1" {
		t.Fatalf("next protos = %#v, want http/1.1 only", cfg.NextProtos)
	}
}

func TestHTTPNotificationClientWaitNotificationWakeUsesConfiguredProxy(t *testing.T) {
	var seenAbsoluteURL, seenHost, seenProxyAuth string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAbsoluteURL = r.URL.String()
		seenHost = r.Host
		seenProxyAuth = r.Header.Get("Proxy-Authorization")
		writeTestNotificationWakeUpgrade(t, w, r, []byte(`{"type":"notification_wake","wake_id":"wake_proxy","unread_count":1}`))
	}))
	defer proxy.Close()
	proxyURL, err := url.Parse(strings.Replace(proxy.URL, "http://", "http://proxy-user:proxy-pass@", 1))
	if err != nil {
		t.Fatal(err)
	}
	client := NewHTTPNotificationClient(&http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}})
	wake, err := client.WaitNotificationWake(context.Background(), AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if wake == nil || wake.WakeID != "wake_proxy" {
		t.Fatalf("wake = %+v", wake)
	}
	if !strings.HasPrefix(seenAbsoluteURL, "http://127.0.0.1:1/agents/me/notifications/connect?") || seenHost != "127.0.0.1:1" || seenProxyAuth == "" {
		t.Fatalf("proxy request url=%q host=%q proxyAuth=%q", seenAbsoluteURL, seenHost, seenProxyAuth)
	}
}

func TestHTTPNotificationClientWaitNotificationWakeCancelsStalledHandshake(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
	}()

	client := NewHTTPNotificationClient(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	wake, err := client.WaitNotificationWake(ctx, AgentProfile{
		Handle:      "max.reviewer",
		AgentSecret: "as_http_secret",
		BaseURL:     "http://" + ln.Addr().String(),
	})
	if wake != nil || !errors.Is(err, errNotificationWakeDeadline) {
		t.Fatalf("wake=%+v err=%v", wake, err)
	}
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("test server did not accept wake connection")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stalled wake connection was not closed after context cancellation")
	}
}

func TestHTTPNotificationClientClaimMutations(t *testing.T) {
	var seen []struct {
		Path           string
		Auth           string
		IdempotencyKey string
		Body           map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("request body decode failed: %v", err)
		}
		seen = append(seen, struct {
			Path           string
			Auth           string
			IdempotencyKey string
			Body           map[string]any
		}{
			Path:           r.URL.Path,
			Auth:           r.Header.Get("Authorization"),
			IdempotencyKey: r.Header.Get("Idempotency-Key"),
			Body:           body,
		})
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/renew"):
			_, _ = w.Write([]byte(`{
				"ok":true,
				"claim_id":"clm_httpclaim1234567890",
				"notification_id":"ntf_httpnotification1234567890",
				"claimed_at":"2026-05-07T00:00:00Z",
				"lease_expires_at":"2026-05-07T00:20:00Z",
				"notification":{
					"id":"ntf_httpnotification1234567890",
					"type":"mention",
					"doc_slug":"abc123",
					"doc_title":"Design Review",
					"context":"Please review.",
					"from_handle":"max.sender",
					"from_name":"Max",
					"comment_id":null,
					"suggestion_id":null,
					"created_at":"2026-05-07T00:00:00Z",
					"read":false
					}
				}`))
		case strings.HasSuffix(r.URL.Path, "/activity/handling"):
			_, _ = w.Write([]byte(`{"ok":true,"activity":{"activity_id":"ah_http123","status":"completed"}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true,"claim_id":"clm_httpclaim1234567890","notification_id":"ntf_httpnotification1234567890"}`))
		}
	}))
	defer server.Close()

	client := NewVersionedHTTPNotificationClient("test-version", server.Client())
	profile := AgentProfile{Handle: "max.reviewer", AgentSecret: "as_http_secret", BaseURL: server.URL + "/"}
	renew, err := client.RenewNotification(context.Background(), profile, "clm_httpclaim1234567890", 2*time.Minute, "op_httprenew1234567890aa")
	if err != nil {
		t.Fatal(err)
	}
	if renew.LeaseExpiresAt != "2026-05-07T00:20:00Z" {
		t.Fatalf("renew = %+v", renew)
	}
	if _, err := client.AckNotification(context.Background(), profile, "clm_httpclaim1234567890", "op_httpack1234567890aaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ReleaseNotification(context.Background(), profile, "clm_httpclaim1234567890", "op_httprelease1234567890"); err != nil {
		t.Fatal(err)
	}
	handling, err := client.PublishNotificationHandlingActivity(context.Background(), profile, "clm_httpclaim1234567890", CloudNotificationHandlingRequest{
		Action:  "complete",
		Outcome: "no_response",
		TTLMS:   45000,
	}, "op_httphandling123456789")
	if err != nil {
		t.Fatal(err)
	}
	if handling.Activity["status"] != "completed" {
		t.Fatalf("handling response = %#v", handling)
	}
	if len(seen) != 4 {
		t.Fatalf("seen requests = %#v", seen)
	}
	if seen[0].Path != "/agents/me/notifications/claim/clm_httpclaim1234567890/renew" || seen[0].IdempotencyKey != "op_httprenew1234567890aa" || seen[0].Body["op_id"] != "op_httprenew1234567890aa" || seen[0].Body["lease_ttl_ms"] != float64(120000) {
		t.Fatalf("renew request = %#v", seen[0])
	}
	if seen[1].Path != "/agents/me/notifications/claim/clm_httpclaim1234567890/ack" || seen[1].IdempotencyKey != "op_httpack1234567890aaaa" || seen[1].Auth != "Bearer as_http_secret" {
		t.Fatalf("ack request = %#v", seen[1])
	}
	if seen[2].Path != "/agents/me/notifications/claim/clm_httpclaim1234567890/release" || seen[2].IdempotencyKey != "op_httprelease1234567890" {
		t.Fatalf("release request = %#v", seen[2])
	}
	if seen[3].Path != "/agents/me/notifications/claim/clm_httpclaim1234567890/activity/handling" || seen[3].IdempotencyKey != "op_httphandling123456789" || seen[3].Body["action"] != "complete" || seen[3].Body["outcome"] != "no_response" || seen[3].Body["ttl_ms"] != float64(45000) {
		t.Fatalf("handling request = %#v", seen[3])
	}
}

func TestHTTPNotificationClientClaimMutationFailures(t *testing.T) {
	t.Run("retryable_status_is_ambiguous", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"code":"TEMPORARY"}`, http.StatusServiceUnavailable)
		}))
		defer server.Close()
		client := NewHTTPNotificationClient(server.Client())
		_, err := client.AckNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     server.URL,
		}, "clm_httpclaim1234567890", "op_httpackambiguous1234")
		if !errors.Is(err, errNotificationMutationAmbiguous) {
			t.Fatalf("retryable mutation err = %v", err)
		}
	})

	t.Run("status_error_redacts_secret", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"as_http_secret"}`))
		}))
		defer server.Close()
		client := NewHTTPNotificationClient(server.Client())
		_, err := client.ReleaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     server.URL,
		}, "clm_httpclaim1234567890", "op_httpreleasesecret123")
		if err == nil || strings.Contains(err.Error(), "as_http_secret") || !strings.Contains(err.Error(), "status 401") {
			t.Fatalf("status err = %v", err)
		}
	})

	t.Run("invalid_claim_and_op", func(t *testing.T) {
		client := NewHTTPNotificationClient(nil)
		_, err := client.AckNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example",
		}, "bad_claim", "op_httpack1234567890aaaa")
		if err == nil || !strings.Contains(err.Error(), "invalid notification claim id") {
			t.Fatalf("claim err = %v", err)
		}
		_, err = client.AckNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example",
		}, "clm_httpclaim1234567890", "bad-op")
		if err == nil || !strings.Contains(err.Error(), "invalid notification operation") {
			t.Fatalf("op err = %v", err)
		}
	})
}

func TestHTTPNotificationClientRejectsUnsafeResponsesAndURLs(t *testing.T) {
	t.Run("deadline_returns_error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()

		client := NewHTTPNotificationClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})})
		lease, err := client.LeaseNotification(ctx, AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example",
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || lease != nil || !strings.Contains(err.Error(), "ended before response") {
			t.Fatalf("deadline lease=%+v err=%v", lease, err)
		}
	})

	t.Run("status_error_redacts_secret", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "as_http_secret", http.StatusUnauthorized)
		}))
		defer server.Close()

		client := NewHTTPNotificationClient(nil)
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     server.URL,
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || strings.Contains(err.Error(), "as_http_secret") || !strings.Contains(err.Error(), "status 401") {
			t.Fatalf("status err = %v", err)
		}
	})

	t.Run("long_parent_deadline_is_capped", func(t *testing.T) {
		client := NewHTTPNotificationClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			deadline, ok := req.Context().Deadline()
			if !ok {
				t.Fatal("request context has no deadline")
			}
			if remaining := time.Until(deadline); remaining <= 0 || remaining > notificationMutationTimeout {
				t.Fatalf("request deadline remaining = %s, want bounded near request timeout", remaining)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader(`{"ok":true,"lease":null,"leases":[]}`)),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})})
		ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
		defer cancel()
		lease, err := client.LeaseNotification(ctx, AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example",
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err != nil || lease != nil {
			t.Fatalf("long parent deadline lease=%+v err=%v", lease, err)
		}
	})

	t.Run("transport_error_is_ambiguous", func(t *testing.T) {
		client := NewHTTPNotificationClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New("connection reset by peer")
		})})
		lease, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example",
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || lease != nil || !errors.Is(err, errNotificationLeaseAmbiguous) {
			t.Fatalf("transport lease=%+v err=%v", lease, err)
		}
	})

	t.Run("transport_error_redacts_request_url", func(t *testing.T) {
		client := NewHTTPNotificationClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: errors.New("dial tcp comment.example:443: connection reset")}
		})})
		lease, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example/private-base",
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || lease != nil || !errors.Is(err, errNotificationLeaseAmbiguous) {
			t.Fatalf("transport lease=%+v err=%v", lease, err)
		}
		errText := err.Error()
		for _, forbidden := range []string{"comment.example", "private-base", "notifications/lease", "as_http_secret"} {
			if strings.Contains(errText, forbidden) {
				t.Fatalf("transport error leaked %q in %q", forbidden, errText)
			}
		}
		if !strings.Contains(errText, "notification transport error") {
			t.Fatalf("transport error text = %q", errText)
		}
	})

	t.Run("mutation_transport_error_redacts_request_url", func(t *testing.T) {
		client := NewHTTPNotificationClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, &url.Error{Op: "Post", URL: req.URL.String(), Err: errors.New("dial tcp comment.example:443: connection reset")}
		})})
		mutation, err := client.AckNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example/private-base",
		}, "clm_httpclaim1234567890", "op_httpackabcdefghijklmn")
		if err == nil || mutation != nil || !errors.Is(err, errNotificationMutationAmbiguous) {
			t.Fatalf("mutation=%+v err=%v", mutation, err)
		}
		errText := err.Error()
		for _, forbidden := range []string{"comment.example", "private-base", "clm_httpclaim1234567890", "notifications/claim", "op_httpackabcdefghijklmn", "as_http_secret"} {
			if strings.Contains(errText, forbidden) {
				t.Fatalf("mutation transport error leaked %q in %q", forbidden, errText)
			}
		}
		if !strings.Contains(errText, "notification transport error") {
			t.Fatalf("mutation transport error text = %q", errText)
		}
	})

	t.Run("retryable_status_is_ambiguous", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "temporary", http.StatusInternalServerError)
		}))
		defer server.Close()

		client := NewHTTPNotificationClient(nil)
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     server.URL,
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if !errors.Is(err, errNotificationLeaseAmbiguous) {
			t.Fatalf("retryable status err = %v", err)
		}
	})

	t.Run("ok_decode_failure_is_ambiguous", func(t *testing.T) {
		client := NewHTTPNotificationClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(strings.NewReader("{")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})})
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example",
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if !errors.Is(err, errNotificationLeaseAmbiguous) {
			t.Fatalf("decode err = %v", err)
		}
	})

	t.Run("ok_body_read_error_is_ambiguous", func(t *testing.T) {
		client := NewHTTPNotificationClient(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       errReadCloser{err: errors.New("connection reset while reading body")},
				Header:     make(http.Header),
				Request:    req,
			}, nil
		})})
		lease, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example",
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || lease != nil || !errors.Is(err, errNotificationLeaseAmbiguous) {
			t.Fatalf("body-read lease=%+v err=%v", lease, err)
		}
	})

	t.Run("server_error_is_ambiguous", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "temporary failure", http.StatusInternalServerError)
		}))
		defer server.Close()

		client := NewHTTPNotificationClient(nil)
		lease, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     server.URL,
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || lease != nil || !errors.Is(err, errNotificationLeaseAmbiguous) {
			t.Fatalf("server-error lease=%+v err=%v", lease, err)
		}
	})

	t.Run("redirect_to_non_loopback_http_url", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://comment.example/agents/me/notifications/lease", http.StatusFound)
		}))
		defer server.Close()

		client := NewHTTPNotificationClient(nil)
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     server.URL,
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || !strings.Contains(err.Error(), "invalid notification base url") {
			t.Fatalf("redirect err = %v", err)
		}
	})

	t.Run("redirect_to_cross_origin_https_url", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://attacker.example/agents/me/notifications/lease", http.StatusFound)
		}))
		defer server.Close()

		client := NewHTTPNotificationClient(server.Client())
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     server.URL,
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || !strings.Contains(err.Error(), "invalid notification base url") {
			t.Fatalf("cross-origin redirect err = %v", err)
		}
	})

	t.Run("same_origin_redirect_is_not_followed", func(t *testing.T) {
		var redirectedAuth string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/redirected" {
				redirectedAuth = r.Header.Get("Authorization")
				w.WriteHeader(http.StatusNoContent)
				return
			}
			http.Redirect(w, r, "/redirected", http.StatusFound)
		}))
		defer server.Close()

		client := NewHTTPNotificationClient(nil)
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     server.URL,
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || !strings.Contains(err.Error(), "invalid notification base url") {
			t.Fatalf("same-origin redirect err = %v", err)
		}
		if redirectedAuth != "" {
			t.Fatalf("redirect target received auth header %q", redirectedAuth)
		}
	})

	t.Run("oversized_response", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", maxNotificationHTTPResponseBody+1)))
		}))
		defer server.Close()

		client := NewHTTPNotificationClient(nil)
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     server.URL,
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("oversized err = %v", err)
		}
	})

	t.Run("invalid_base_url", func(t *testing.T) {
		client := NewHTTPNotificationClient(nil)
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "file:///tmp/comment",
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || !strings.Contains(err.Error(), "invalid notification base url") {
			t.Fatalf("base url err = %v", err)
		}
	})

	t.Run("base_url_with_query", func(t *testing.T) {
		client := NewHTTPNotificationClient(nil)
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example?token=as_http_secret",
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || !strings.Contains(err.Error(), "invalid notification base url") || strings.Contains(err.Error(), "as_http_secret") {
			t.Fatalf("query base url err = %v", err)
		}
	})

	t.Run("base_url_with_secret_path", func(t *testing.T) {
		client := NewHTTPNotificationClient(nil)
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example/as_path_secret",
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || !strings.Contains(err.Error(), "invalid notification base url") || strings.Contains(err.Error(), "as_path_secret") {
			t.Fatalf("secret path base url err = %v", err)
		}
	})

	t.Run("non_loopback_http_url", func(t *testing.T) {
		client := NewHTTPNotificationClient(nil)
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "http://comment.example",
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || !strings.Contains(err.Error(), "invalid notification base url") {
			t.Fatalf("non-loopback http err = %v", err)
		}
	})

	t.Run("invalid_profile", func(t *testing.T) {
		client := NewHTTPNotificationClient(nil)
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "bad profile",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example",
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", testNotificationLeaseOpID)
		if err == nil || !strings.Contains(err.Error(), "invalid notification profile") {
			t.Fatalf("profile err = %v", err)
		}
	})

	t.Run("invalid_lease_holder", func(t *testing.T) {
		client := NewHTTPNotificationClient(nil)
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example",
		}, defaultLocalLeaseTTL, "bad\nholder", testNotificationLeaseOpID)
		if err == nil || !strings.Contains(err.Error(), "invalid notification lease holder") {
			t.Fatalf("lease holder err = %v", err)
		}
	})

	t.Run("invalid_idempotency_key", func(t *testing.T) {
		client := NewHTTPNotificationClient(nil)
		_, err := client.LeaseNotification(context.Background(), AgentProfile{
			Handle:      "max.reviewer",
			AgentSecret: "as_http_secret",
			BaseURL:     "https://comment.example",
		}, defaultLocalLeaseTTL, "comment-bus:max.reviewer", "bad-op")
		if err == nil || !strings.Contains(err.Error(), "invalid notification lease operation") {
			t.Fatalf("idempotency err = %v", err)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type errReadCloser struct {
	err error
}

func (r errReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (r errReadCloser) Close() error {
	return nil
}

func writeTestNotificationWakeUpgrade(t *testing.T, w http.ResponseWriter, r *http.Request, payload []byte) {
	t.Helper()
	conn, rw := writeTestNotificationWakeHandshake(t, w, r)
	defer conn.Close()
	opcode, clientPayload, err := readWebSocketServerFrame(rw.Reader)
	if err != nil {
		t.Fatalf("client ping read failed: %v", err)
	}
	if opcode != 0x1 || string(clientPayload) != `{"type":"ping"}` {
		t.Fatalf("client wake ping opcode=%d payload=%s", opcode, clientPayload)
	}
	if err := writeTestWebSocketServerTextFrame(rw, payload); err != nil {
		t.Fatalf("wake write failed: %v", err)
	}
}

func writeTestNotificationWakeHandshake(t *testing.T, w http.ResponseWriter, r *http.Request) (net.Conn, *bufio.ReadWriter) {
	t.Helper()
	if r.Header.Get("Upgrade") != "websocket" {
		t.Fatalf("missing websocket upgrade: %s", r.Header.Get("Upgrade"))
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("test server does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		t.Fatalf("hijack failed: %v", err)
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	hash := sha1.Sum([]byte(key + websocketAcceptGUID))
	accept := base64.StdEncoding.EncodeToString(hash[:])
	_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	_, _ = rw.WriteString("Upgrade: websocket\r\n")
	_, _ = rw.WriteString("Connection: Upgrade\r\n")
	_, _ = rw.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		t.Fatalf("handshake flush failed: %v", err)
	}
	return conn, rw
}

func writeTestWebSocketServerTextFrame(rw *bufio.ReadWriter, payload []byte) error {
	return writeTestWebSocketServerFrame(rw, 0x1, payload)
}

func writeTestWebSocketServerFrame(rw *bufio.ReadWriter, opcode byte, payload []byte) error {
	if len(payload) >= 126 {
		return errors.New("test websocket payload too large")
	}
	if _, err := rw.Write([]byte{0x80 | (opcode & 0x0F), byte(len(payload))}); err != nil {
		return err
	}
	if _, err := rw.Write(payload); err != nil {
		return err
	}
	return rw.Flush()
}

func withNotificationWakeTimings(t *testing.T, pingInterval time.Duration, readTimeout time.Duration) {
	t.Helper()
	oldPingInterval := notificationWakePingInterval
	oldReadTimeout := notificationWakeReadTimeout
	notificationWakePingInterval = pingInterval
	notificationWakeReadTimeout = readTimeout
	t.Cleanup(func() {
		notificationWakePingInterval = oldPingInterval
		notificationWakeReadTimeout = oldReadTimeout
	})
}

func withNotificationWakeWriteTimeout(t *testing.T, writeTimeout time.Duration) {
	t.Helper()
	oldWriteTimeout := notificationWakeWriteTimeout
	notificationWakeWriteTimeout = writeTimeout
	t.Cleanup(func() {
		notificationWakeWriteTimeout = oldWriteTimeout
	})
}

type concurrentWriteDetectConn struct {
	mu        sync.Mutex
	active    int
	maxActive int
}

func (c *concurrentWriteDetectConn) Read([]byte) (int, error) {
	return 0, io.EOF
}

func (c *concurrentWriteDetectConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.active++
	if c.active > c.maxActive {
		c.maxActive = c.active
	}
	c.mu.Unlock()
	time.Sleep(time.Millisecond)
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	return len(p), nil
}

func (c *concurrentWriteDetectConn) Close() error {
	return nil
}

func (c *concurrentWriteDetectConn) LocalAddr() net.Addr {
	return testNetAddr("local")
}

func (c *concurrentWriteDetectConn) RemoteAddr() net.Addr {
	return testNetAddr("remote")
}

func (c *concurrentWriteDetectConn) SetDeadline(time.Time) error {
	return nil
}

func (c *concurrentWriteDetectConn) SetReadDeadline(time.Time) error {
	return nil
}

func (c *concurrentWriteDetectConn) SetWriteDeadline(time.Time) error {
	return nil
}

func (c *concurrentWriteDetectConn) maxActiveWrites() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxActive
}

type testNetAddr string

func (a testNetAddr) Network() string {
	return string(a)
}

func (a testNetAddr) String() string {
	return string(a)
}

type blockingWakeConn struct {
	mu            sync.Mutex
	readBuf       []byte
	closed        bool
	readDeadline  time.Time
	writeDeadline time.Time
	blockedWrites int
	wake          chan struct{}
}

func newBlockingWakeConn() *blockingWakeConn {
	return &blockingWakeConn{wake: make(chan struct{})}
}

func (c *blockingWakeConn) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if len(c.readBuf) > 0 {
			n := copy(p, c.readBuf)
			c.readBuf = c.readBuf[n:]
			c.mu.Unlock()
			return n, nil
		}
		if c.closed {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		deadline := c.readDeadline
		wake := c.wake
		c.mu.Unlock()
		if err := waitForBlockingConnSignal(wake, deadline); err != nil {
			return 0, err
		}
	}
}

func (c *blockingWakeConn) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "GET ") {
		c.mu.Lock()
		c.readBuf = append(c.readBuf, []byte(testWakeUpgradeResponse(string(p)))...)
		c.signalLocked()
		c.mu.Unlock()
		return len(p), nil
	}
	c.mu.Lock()
	c.blockedWrites++
	c.mu.Unlock()
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		deadline := c.writeDeadline
		wake := c.wake
		c.mu.Unlock()
		if err := waitForBlockingConnSignal(wake, deadline); err != nil {
			return 0, err
		}
	}
}

func (c *blockingWakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.signalLocked()
	return nil
}

func (c *blockingWakeConn) LocalAddr() net.Addr {
	return testNetAddr("local")
}

func (c *blockingWakeConn) RemoteAddr() net.Addr {
	return testNetAddr("remote")
}

func (c *blockingWakeConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.writeDeadline = t
	c.signalLocked()
	return nil
}

func (c *blockingWakeConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.signalLocked()
	return nil
}

func (c *blockingWakeConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	c.signalLocked()
	return nil
}

func (c *blockingWakeConn) blockedWriteCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blockedWrites
}

func (c *blockingWakeConn) signalLocked() {
	close(c.wake)
	c.wake = make(chan struct{})
}

type cancelAfterReadDeadlineWakeConn struct {
	mu                      sync.Mutex
	readBuf                 []byte
	closed                  bool
	readDeadline            time.Time
	writeDeadline           time.Time
	cancel                  context.CancelFunc
	cancelTriggered         bool
	ctxReadDeadlineSeen     bool
	ctxReadDeadlineObserved chan struct{}
	ctxReadDeadlineOnce     sync.Once
	wake                    chan struct{}
}

func newCancelAfterReadDeadlineWakeConn(cancel context.CancelFunc) *cancelAfterReadDeadlineWakeConn {
	return &cancelAfterReadDeadlineWakeConn{
		cancel:                  cancel,
		ctxReadDeadlineObserved: make(chan struct{}),
		wake:                    make(chan struct{}),
	}
}

func (c *cancelAfterReadDeadlineWakeConn) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if len(c.readBuf) > 0 {
			n := copy(p, c.readBuf)
			c.readBuf = c.readBuf[n:]
			c.mu.Unlock()
			return n, nil
		}
		if c.closed {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		deadline := c.readDeadline
		wake := c.wake
		c.mu.Unlock()
		if err := waitForBlockingConnSignal(wake, deadline); err != nil {
			return 0, err
		}
	}
}

func (c *cancelAfterReadDeadlineWakeConn) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "GET ") {
		c.mu.Lock()
		c.readBuf = append(c.readBuf, []byte(testWakeUpgradeResponse(string(p)))...)
		c.signalLocked()
		c.mu.Unlock()
		return len(p), nil
	}
	return len(p), nil
}

func (c *cancelAfterReadDeadlineWakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.signalLocked()
	return nil
}

func (c *cancelAfterReadDeadlineWakeConn) LocalAddr() net.Addr {
	return testNetAddr("local")
}

func (c *cancelAfterReadDeadlineWakeConn) RemoteAddr() net.Addr {
	return testNetAddr("remote")
}

func (c *cancelAfterReadDeadlineWakeConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.writeDeadline = t
	c.signalLocked()
	return nil
}

func (c *cancelAfterReadDeadlineWakeConn) SetReadDeadline(t time.Time) error {
	shouldCancel := false
	shouldWaitForCtxDeadline := false
	c.mu.Lock()
	if !t.IsZero() && time.Until(t) > time.Second && !c.cancelTriggered {
		c.cancelTriggered = true
		shouldCancel = true
		shouldWaitForCtxDeadline = true
	} else if !t.IsZero() && time.Until(t) <= 0 {
		c.ctxReadDeadlineSeen = true
		c.ctxReadDeadlineOnce.Do(func() {
			close(c.ctxReadDeadlineObserved)
		})
	}
	c.mu.Unlock()
	if shouldCancel {
		c.cancel()
	}
	if shouldWaitForCtxDeadline {
		select {
		case <-c.ctxReadDeadlineObserved:
		case <-time.After(time.Second):
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.signalLocked()
	return nil
}

func (c *cancelAfterReadDeadlineWakeConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	c.signalLocked()
	return nil
}

func (c *cancelAfterReadDeadlineWakeConn) cancelRaceObserved() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cancelTriggered && c.ctxReadDeadlineSeen
}

func (c *cancelAfterReadDeadlineWakeConn) signalLocked() {
	close(c.wake)
	c.wake = make(chan struct{})
}

func waitForBlockingConnSignal(wake <-chan struct{}, deadline time.Time) error {
	if deadline.IsZero() {
		<-wake
		return nil
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		return os.ErrDeadlineExceeded
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-wake:
		return nil
	case <-timer.C:
		return os.ErrDeadlineExceeded
	}
}

func testWakeUpgradeResponse(handshake string) string {
	key := ""
	for _, line := range strings.Split(handshake, "\r\n") {
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Sec-WebSocket-Key") {
			key = strings.TrimSpace(value)
			break
		}
	}
	hash := sha1.Sum([]byte(key + websocketAcceptGUID))
	accept := base64.StdEncoding.EncodeToString(hash[:])
	return "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
}

type tickerFailureWakeConn struct {
	mu                   sync.Mutex
	readBuf              []byte
	writeBuf             []byte
	closed               bool
	readDeadline         time.Time
	writeDeadline        time.Time
	protocolPingFailures int
	pongHeaders          int
	wake                 chan struct{}
}

func newTickerFailureWakeConn() *tickerFailureWakeConn {
	return &tickerFailureWakeConn{wake: make(chan struct{})}
}

func (c *tickerFailureWakeConn) Read(p []byte) (int, error) {
	for {
		c.mu.Lock()
		if len(c.readBuf) > 0 {
			n := copy(p, c.readBuf)
			c.readBuf = c.readBuf[n:]
			c.mu.Unlock()
			return n, nil
		}
		if c.closed {
			c.mu.Unlock()
			return 0, net.ErrClosed
		}
		deadline := c.readDeadline
		wake := c.wake
		c.mu.Unlock()
		if err := waitForBlockingConnSignal(wake, deadline); err != nil {
			return 0, err
		}
	}
}

func (c *tickerFailureWakeConn) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "GET ") {
		c.mu.Lock()
		c.readBuf = append(c.readBuf, []byte(testWakeUpgradeResponse(string(p)))...)
		c.signalLocked()
		c.mu.Unlock()
		return len(p), nil
	}
	c.mu.Lock()
	c.writeBuf = append(c.writeBuf, p...)
	err := c.processClientFramesLocked()
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *tickerFailureWakeConn) processClientFramesLocked() error {
	for {
		if len(c.writeBuf) < 2 {
			return nil
		}
		payloadLen := int(c.writeBuf[1] & 0x7F)
		if payloadLen >= 126 {
			return errors.New("test websocket extended payload unsupported")
		}
		maskLen := 0
		if c.writeBuf[1]&0x80 != 0 {
			maskLen = 4
		}
		frameLen := 2 + maskLen + payloadLen
		if len(c.writeBuf) < frameLen {
			return nil
		}
		opcode := c.writeBuf[0] & 0x0F
		c.writeBuf = c.writeBuf[frameLen:]
		switch opcode {
		case 0x9:
			c.protocolPingFailures++
			c.readBuf = append(c.readBuf, testWebSocketServerFrameBytes(0x9, []byte("server-ping"))...)
			c.signalLocked()
			return os.ErrDeadlineExceeded
		case 0xA:
			c.pongHeaders++
		}
	}
}

func (c *tickerFailureWakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.signalLocked()
	return nil
}

func (c *tickerFailureWakeConn) LocalAddr() net.Addr {
	return testNetAddr("local")
}

func (c *tickerFailureWakeConn) RemoteAddr() net.Addr {
	return testNetAddr("remote")
}

func (c *tickerFailureWakeConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.writeDeadline = t
	c.signalLocked()
	return nil
}

func (c *tickerFailureWakeConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.signalLocked()
	return nil
}

func (c *tickerFailureWakeConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	c.signalLocked()
	return nil
}

func (c *tickerFailureWakeConn) protocolPingFailureCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.protocolPingFailures
}

func (c *tickerFailureWakeConn) pongHeaderCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pongHeaders
}

func (c *tickerFailureWakeConn) signalLocked() {
	close(c.wake)
	c.wake = make(chan struct{})
}

func testWebSocketServerFrameBytes(opcode byte, payload []byte) []byte {
	if len(payload) >= 126 {
		panic("test websocket payload too large")
	}
	frame := []byte{0x80 | (opcode & 0x0F), byte(len(payload))}
	frame = append(frame, payload...)
	return frame
}
