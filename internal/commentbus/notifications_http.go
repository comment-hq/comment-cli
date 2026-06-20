package commentbus

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	notificationLeaseEndpoint       = "/agents/me/notifications/lease"
	notificationWakeEndpoint        = "/agents/me/notifications/connect"
	maxNotificationHTTPResponseBody = 1 << 20
	maxNotificationWakeFrameBytes   = 64 * 1024
	notificationMutationTimeout     = 10 * time.Second
)

var (
	errNotificationLeaseDeadline      = errors.New("notification lease ended before response")
	errNotificationLeaseAmbiguous     = errors.New("notification lease response uncertain")
	errNotificationWakeDeadline       = errors.New("notification wake ended before response")
	errNotificationWakeAmbiguous      = errors.New("notification wake response uncertain")
	errNotificationMutationDeadline   = errors.New("notification mutation ended before response")
	errNotificationMutationAmbiguous  = errors.New("notification mutation response uncertain")
	errInvalidNotificationRedirectURL = errors.New("invalid notification redirect url")

	// ErrAgentAuthRevoked is returned when the notification wake socket is
	// closed by the server with close code 4431 (WS_AGENT_AUTH_REVOKED_CLOSE_CODE
	// in packages/shared/src/protocol.ts). Reconnecting with the same agent
	// credentials will be rejected again; callers should back off and prompt
	// the operator to re-issue credentials.
	ErrAgentAuthRevoked = errors.New("notification wake socket closed: agent auth revoked")
)

// wsCloseCodeAgentAuthRevoked mirrors WS_AGENT_AUTH_REVOKED_CLOSE_CODE in
// packages/shared/src/protocol.ts.
const wsCloseCodeAgentAuthRevoked = 4431

// parseWebSocketCloseCode reads a control-frame close payload (RFC 6455 §5.5.1):
// the first two bytes are the big-endian status code; everything after is an
// optional UTF-8 reason. Returns (code, true) when the payload carries a code.
func parseWebSocketCloseCode(payload []byte) (uint16, bool) {
	if len(payload) < 2 {
		return 0, false
	}
	return binary.BigEndian.Uint16(payload[:2]), true
}

var (
	notificationWakePingInterval = 30 * time.Second
	notificationWakeReadTimeout  = 70 * time.Second
	notificationWakeWriteTimeout = 5 * time.Second
)

const websocketAcceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type HTTPNotificationClient struct {
	Client    *http.Client
	userAgent string
}

type NotificationHTTPError struct {
	Status int
	Code   string
}

func (e *NotificationHTTPError) Error() string {
	if e == nil {
		return "notification request failed"
	}
	if e.Code != "" {
		return fmt.Sprintf("notification request failed with status %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("notification request failed with status %d", e.Status)
}

func NewHTTPNotificationClient(client *http.Client) *HTTPNotificationClient {
	return NewVersionedHTTPNotificationClient("", client)
}

func NewVersionedHTTPNotificationClient(version string, client *http.Client) *HTTPNotificationClient {
	if client == nil {
		client = http.DefaultClient
	}
	copied := *client
	copied.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return errInvalidNotificationRedirectURL
	}
	return &HTTPNotificationClient{Client: &copied, userAgent: notificationUserAgent(version)}
}

func (c *HTTPNotificationClient) LeaseNotification(ctx context.Context, profile AgentProfile, leaseTTL time.Duration, leaseHolder string, idempotencyKey string, kinds ...string) (*CloudNotificationLease, error) {
	if c == nil {
		return nil, errors.New("notification client is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateNotificationProfile(profile); err != nil {
		return nil, err
	}
	if !isSafeNotificationLeaseHolder(leaseHolder) {
		return nil, errors.New("invalid notification lease holder")
	}
	if !LocalOperationIDRE.MatchString(idempotencyKey) {
		return nil, errors.New("invalid notification lease operation")
	}
	leaseURL, err := buildNotificationLeaseURL(profile.BaseURL)
	if err != nil {
		return nil, err
	}
	bodyValues := map[string]any{
		"lease_holder": leaseHolder,
		"lease_ttl_ms": durationMilliseconds(leaseTTL),
	}
	if len(kinds) > 0 {
		bodyValues["kinds"] = kinds
	}
	body, err := json.Marshal(bodyValues)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, notificationMutationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, leaseURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+profile.AgentSecret)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		if errors.Is(err, errInvalidNotificationRedirectURL) {
			return nil, errors.New("invalid notification base url")
		}
		if reqCtx.Err() != nil || ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", errNotificationLeaseDeadline, sanitizedNotificationTransportError(err))
		}
		return nil, fmt.Errorf("%w: %w", errNotificationLeaseAmbiguous, sanitizedNotificationTransportError(err))
	}
	defer resp.Body.Close()
	data, err := readNotificationHTTPBody(resp.Body)
	if err != nil {
		if resp.StatusCode == http.StatusOK || isAmbiguousNotificationMutationStatus(resp.StatusCode) {
			return nil, fmt.Errorf("%w: %w", errNotificationLeaseAmbiguous, err)
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		statusErr := notificationHTTPStatusError(resp.StatusCode, data)
		if isAmbiguousNotificationMutationStatus(resp.StatusCode) {
			return nil, fmt.Errorf("%w: %w", errNotificationLeaseAmbiguous, statusErr)
		}
		return nil, statusErr
	}
	var response struct {
		OK     bool                     `json:"ok"`
		Lease  *CloudNotificationLease  `json:"lease"`
		Leases []CloudNotificationLease `json:"leases"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		return nil, fmt.Errorf("%w: %w", errNotificationLeaseAmbiguous, err)
	}
	if !response.OK {
		return nil, fmt.Errorf("%w: invalid notification lease response", errNotificationLeaseAmbiguous)
	}
	if response.Lease == nil {
		return nil, nil
	}
	return response.Lease, nil
}

func (c *HTTPNotificationClient) WaitNotificationWake(ctx context.Context, profile AgentProfile) (*CloudNotificationWake, error) {
	if c == nil {
		return nil, errors.New("notification client is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateNotificationProfile(profile); err != nil {
		return nil, err
	}
	wakeURL, err := buildNotificationWakeURL(profile.BaseURL)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(wakeURL)
	if err != nil {
		return nil, errors.New("invalid notification base url")
	}
	conn, reader, err := c.openNotificationWakeSocket(ctx, parsed, profile.AgentSecret)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", errNotificationWakeDeadline, sanitizedNotificationTransportError(err))
		}
		return nil, fmt.Errorf("%w: %w", errNotificationWakeAmbiguous, sanitizedNotificationTransportError(err))
	}
	defer conn.Close()
	lc := &lockedConn{conn: conn}
	var writerWG sync.WaitGroup
	defer writerWG.Wait()
	stopClose := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			now := time.Now()
			_ = conn.SetReadDeadline(now)
			_ = conn.SetWriteDeadline(now)
		case <-stopClose:
		}
	}()
	defer close(stopClose)
	if err := lc.writeFrame(0x1, []byte(`{"type":"ping"}`)); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", errNotificationWakeDeadline, sanitizedNotificationTransportError(err))
		}
		return nil, fmt.Errorf("%w: %w", errNotificationWakeAmbiguous, sanitizedNotificationTransportError(err))
	}
	writerErr := make(chan error, 1)
	recordWriterErr := func(err error) {
		select {
		case writerErr <- err:
		default:
		}
		_ = conn.SetReadDeadline(time.Now())
	}
	readWriterErr := func() error {
		select {
		case err := <-writerErr:
			return err
		default:
			return nil
		}
	}
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		ticker := time.NewTicker(notificationWakePingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopClose:
				return
			case <-ticker.C:
				if err := lc.writeFrame(0x9, nil); err != nil {
					recordWriterErr(err)
					return
				}
			}
		}
	}()
	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", errNotificationWakeDeadline, ctx.Err())
		}
		if err := readWriterErr(); err != nil {
			return nil, fmt.Errorf("%w: %w", errNotificationWakeAmbiguous, sanitizedNotificationTransportError(err))
		}
		_ = conn.SetReadDeadline(time.Now().Add(notificationWakeReadTimeout))
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", errNotificationWakeDeadline, ctx.Err())
		}
		if err := readWriterErr(); err != nil {
			return nil, fmt.Errorf("%w: %w", errNotificationWakeAmbiguous, sanitizedNotificationTransportError(err))
		}
		opcode, payload, err := readWebSocketServerFrame(reader)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("%w: %w", errNotificationWakeDeadline, sanitizedNotificationTransportError(err))
			}
			if isNotificationWakeReadDeadline(err) {
				return nil, fmt.Errorf("%w: %w", errNotificationWakeAmbiguous, sanitizedNotificationTransportError(err))
			}
			return nil, fmt.Errorf("%w: %w", errNotificationWakeAmbiguous, sanitizedNotificationTransportError(err))
		}
		switch opcode {
		case 0x1:
			var wake CloudNotificationWake
			if err := json.Unmarshal(payload, &wake); err != nil {
				continue
			}
			if wake.Type != "notification_wake" {
				continue
			}
			if wake.UnreadCount <= 0 {
				continue
			}
			return &wake, nil
		case 0x8:
			if code, ok := parseWebSocketCloseCode(payload); ok && code == wsCloseCodeAgentAuthRevoked {
				return nil, ErrAgentAuthRevoked
			}
			return nil, errNotificationWakeAmbiguous
		case 0x9:
			if err := lc.writeFrame(0xA, payload); err != nil {
				if ctx.Err() != nil {
					return nil, fmt.Errorf("%w: %w", errNotificationWakeDeadline, sanitizedNotificationTransportError(err))
				}
				return nil, fmt.Errorf("%w: %w", errNotificationWakeAmbiguous, sanitizedNotificationTransportError(err))
			}
		case 0xA:
			continue
		default:
			continue
		}
	}
}

func (c *HTTPNotificationClient) RenewNotification(ctx context.Context, profile AgentProfile, claimID string, leaseTTL time.Duration, idempotencyKey string) (*CloudNotificationLease, error) {
	body := map[string]any{
		"op_id": idempotencyKey,
	}
	if leaseTTL > 0 {
		body["lease_ttl_ms"] = durationMilliseconds(leaseTTL)
	}
	data, err := c.doNotificationClaimMutation(ctx, profile, claimID, "renew", idempotencyKey, body)
	if err != nil {
		return nil, err
	}
	var lease CloudNotificationLease
	if err := json.Unmarshal(data, &lease); err != nil {
		return nil, fmt.Errorf("%w: %w", errNotificationMutationAmbiguous, err)
	}
	if lease.ClaimID == "" || lease.NotificationID == "" || lease.LeaseExpiresAt == "" {
		return nil, fmt.Errorf("%w: invalid notification renew response", errNotificationMutationAmbiguous)
	}
	return &lease, nil
}

func (c *HTTPNotificationClient) AckNotification(ctx context.Context, profile AgentProfile, claimID string, idempotencyKey string) (*CloudNotificationClaimMutation, error) {
	return c.finishNotificationClaim(ctx, profile, claimID, "ack", idempotencyKey)
}

func (c *HTTPNotificationClient) ReleaseNotification(ctx context.Context, profile AgentProfile, claimID string, idempotencyKey string) (*CloudNotificationClaimMutation, error) {
	return c.finishNotificationClaim(ctx, profile, claimID, "release", idempotencyKey)
}

func (c *HTTPNotificationClient) PublishNotificationHandlingActivity(ctx context.Context, profile AgentProfile, claimID string, request CloudNotificationHandlingRequest, idempotencyKey string) (*CloudNotificationHandlingResult, error) {
	if request.Action != "start" && request.Action != "complete" && request.Action != "clear" {
		return nil, errors.New("invalid notification handling action")
	}
	body := map[string]any{"action": request.Action}
	if request.TTLMS > 0 {
		body["ttl_ms"] = request.TTLMS
	}
	if request.Outcome != "" {
		body["outcome"] = request.Outcome
	}
	if request.ClaimGeneration != "" {
		body["claim_generation"] = request.ClaimGeneration
	}
	if request.ProgressAt != "" {
		body["progress_at"] = request.ProgressAt
	}
	data, err := c.doNotificationClaimMutation(ctx, profile, claimID, "activity/handling", idempotencyKey, body)
	if err != nil {
		return nil, err
	}
	var result CloudNotificationHandlingResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("%w: %w", errNotificationMutationAmbiguous, err)
	}
	if !result.OK {
		return nil, fmt.Errorf("%w: invalid notification handling response", errNotificationMutationAmbiguous)
	}
	return &result, nil
}

func (c *HTTPNotificationClient) finishNotificationClaim(ctx context.Context, profile AgentProfile, claimID string, action string, idempotencyKey string) (*CloudNotificationClaimMutation, error) {
	data, err := c.doNotificationClaimMutation(ctx, profile, claimID, action, idempotencyKey, map[string]any{"op_id": idempotencyKey})
	if err != nil {
		return nil, err
	}
	var result CloudNotificationClaimMutation
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("%w: %w", errNotificationMutationAmbiguous, err)
	}
	if !result.OK || result.ClaimID == "" || result.NotificationID == "" {
		return nil, fmt.Errorf("%w: invalid notification %s response", errNotificationMutationAmbiguous, action)
	}
	return &result, nil
}

func (c *HTTPNotificationClient) doNotificationClaimMutation(ctx context.Context, profile AgentProfile, claimID string, action string, idempotencyKey string, body map[string]any) ([]byte, error) {
	if c == nil {
		return nil, errors.New("notification client is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := validateNotificationProfile(profile); err != nil {
		return nil, err
	}
	if !isSafeCloudID("claim", claimID) {
		return nil, errors.New("invalid notification claim id")
	}
	if !LocalOperationIDRE.MatchString(idempotencyKey) {
		return nil, errors.New("invalid notification operation")
	}
	if action != "renew" && action != "ack" && action != "release" && action != "activity/handling" {
		return nil, errors.New("invalid notification operation")
	}
	mutationURL, err := buildNotificationClaimURL(profile.BaseURL, claimID, action)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, notificationMutationTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, mutationURL, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+profile.AgentSecret)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.httpClient().Do(req)
	if err != nil {
		if errors.Is(err, errInvalidNotificationRedirectURL) {
			return nil, errors.New("invalid notification base url")
		}
		if reqCtx.Err() != nil || ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %w", errNotificationMutationDeadline, sanitizedNotificationTransportError(err))
		}
		return nil, fmt.Errorf("%w: %w", errNotificationMutationAmbiguous, sanitizedNotificationTransportError(err))
	}
	defer resp.Body.Close()
	data, err := readNotificationHTTPBody(resp.Body)
	if err != nil {
		if resp.StatusCode == http.StatusOK || isAmbiguousNotificationMutationStatus(resp.StatusCode) {
			return nil, fmt.Errorf("%w: %w", errNotificationMutationAmbiguous, err)
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		statusErr := notificationHTTPStatusError(resp.StatusCode, data)
		if isAmbiguousNotificationMutationStatus(resp.StatusCode) {
			return nil, fmt.Errorf("%w: %w", errNotificationMutationAmbiguous, statusErr)
		}
		return nil, statusErr
	}
	return data, nil
}

func (c *HTTPNotificationClient) httpClient() *http.Client {
	if c == nil || c.Client == nil {
		return http.DefaultClient
	}
	return c.Client
}

func (c *HTTPNotificationClient) openNotificationWakeSocket(ctx context.Context, parsed *url.URL, agentSecret string) (net.Conn, *bufio.Reader, error) {
	transport, err := c.notificationHTTPTransport()
	if err != nil {
		return nil, nil, err
	}
	conn, requestURI, proxyAuth, err := c.openNotificationWakeConnection(ctx, transport, parsed)
	if err != nil {
		return nil, nil, err
	}
	stopClose := closeNotificationConnOnContextDone(ctx, conn)
	defer stopClose()
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	if err := writeNotificationWakeHandshake(conn, requestURI, parsed.Host, key, agentSecret, c.userAgent, proxyAuth); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		return nil, nil, &NotificationHTTPError{Status: resp.StatusCode}
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") {
		_ = conn.Close()
		return nil, nil, errors.New("invalid websocket upgrade response")
	}
	if !headerContainsToken(resp.Header.Values("Connection"), "upgrade") {
		_ = conn.Close()
		return nil, nil, errors.New("invalid websocket connection response")
	}
	acceptHash := sha1.Sum([]byte(key + websocketAcceptGUID))
	expectedAccept := base64.StdEncoding.EncodeToString(acceptHash[:])
	if resp.Header.Get("Sec-WebSocket-Accept") != expectedAccept {
		_ = conn.Close()
		return nil, nil, errors.New("invalid websocket accept response")
	}
	return conn, reader, nil
}

func (c *HTTPNotificationClient) notificationHTTPTransport() (*http.Transport, error) {
	rt := http.DefaultTransport
	if c != nil && c.Client != nil && c.Client.Transport != nil {
		rt = c.Client.Transport
	}
	transport, ok := rt.(*http.Transport)
	if !ok {
		return nil, errors.New("notification wake transport does not support websocket dial")
	}
	return transport.Clone(), nil
}

func (c *HTTPNotificationClient) openNotificationWakeConnection(ctx context.Context, transport *http.Transport, parsed *url.URL) (net.Conn, string, string, error) {
	targetAddr, err := notificationWakeDialAddress(parsed)
	if err != nil {
		return nil, "", "", err
	}
	requestURI := parsed.RequestURI()
	if requestURI == "" {
		requestURI = "/"
	}
	proxyReqURL, err := notificationWakeProxyRequestURL(parsed)
	if err != nil {
		return nil, "", "", err
	}
	var proxyURL *url.URL
	if transport.Proxy != nil {
		proxyReq := &http.Request{Method: http.MethodGet, URL: proxyReqURL}
		proxyURL, err = transport.Proxy(proxyReq)
		if err != nil {
			return nil, "", "", err
		}
	}
	if proxyURL == nil {
		conn, err := c.openDirectNotificationWakeConnection(ctx, transport, parsed, targetAddr)
		return conn, requestURI, "", err
	}
	proxyConn, err := c.openNotificationProxyConnection(ctx, transport, proxyURL)
	if err != nil {
		return nil, "", "", err
	}
	proxyAuth := notificationProxyAuthorization(proxyURL)
	if parsed.Scheme == "ws" {
		return proxyConn, proxyReqURL.String(), proxyAuth, nil
	}
	if err := c.connectNotificationWakeProxy(ctx, proxyConn, proxyURL, targetAddr, proxyAuth, transport.ProxyConnectHeader); err != nil {
		_ = proxyConn.Close()
		return nil, "", "", err
	}
	tlsConn, err := c.wrapNotificationWakeTLS(ctx, transport, proxyConn, parsed.Hostname())
	if err != nil {
		_ = proxyConn.Close()
		return nil, "", "", err
	}
	return tlsConn, requestURI, "", nil
}

func (c *HTTPNotificationClient) openDirectNotificationWakeConnection(ctx context.Context, transport *http.Transport, parsed *url.URL, targetAddr string) (net.Conn, error) {
	if parsed.Scheme == "wss" {
		if transport.DialTLSContext != nil {
			return transport.DialTLSContext(ctx, "tcp", targetAddr)
		}
		if transport.DialTLS != nil {
			return dialNotificationWakeLegacyTLS(ctx, transport.DialTLS, targetAddr)
		}
	}
	conn, err := dialNotificationWakeTCP(ctx, transport, targetAddr)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "wss" {
		return conn, nil
	}
	tlsConn, err := c.wrapNotificationWakeTLS(ctx, transport, conn, parsed.Hostname())
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func (c *HTTPNotificationClient) openNotificationProxyConnection(ctx context.Context, transport *http.Transport, proxyURL *url.URL) (net.Conn, error) {
	switch proxyURL.Scheme {
	case "http", "https":
	default:
		return nil, errors.New("unsupported notification proxy scheme")
	}
	addr, err := notificationURLDialAddress(proxyURL, proxyURL.Scheme)
	if err != nil {
		return nil, err
	}
	conn, err := dialNotificationWakeTCP(ctx, transport, addr)
	if err != nil {
		return nil, err
	}
	if proxyURL.Scheme != "https" {
		return conn, nil
	}
	tlsConn, err := c.wrapNotificationWakeTLS(ctx, transport, conn, proxyURL.Hostname())
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func (c *HTTPNotificationClient) connectNotificationWakeProxy(ctx context.Context, conn net.Conn, proxyURL *url.URL, targetAddr string, proxyAuth string, proxyConnectHeader http.Header) error {
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: targetAddr},
		Host:   targetAddr,
		Header: make(http.Header),
	}
	for key, values := range proxyConnectHeader {
		req.Header[key] = append([]string(nil), values...)
	}
	if proxyAuth != "" {
		req.Header.Set("Proxy-Authorization", proxyAuth)
	}
	stopClose := closeNotificationConnOnContextDone(ctx, conn)
	defer stopClose()
	if err := req.Write(conn); err != nil {
		return err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &NotificationHTTPError{Status: resp.StatusCode}
	}
	return nil
}

func (c *HTTPNotificationClient) wrapNotificationWakeTLS(ctx context.Context, transport *http.Transport, conn net.Conn, serverName string) (net.Conn, error) {
	tlsConn := tls.Client(conn, notificationWakeTLSConfig(transport, serverName))
	handshakeCtx := ctx
	cancel := func() {}
	if transport.TLSHandshakeTimeout > 0 {
		handshakeCtx, cancel = context.WithTimeout(ctx, transport.TLSHandshakeTimeout)
	}
	defer cancel()
	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		return nil, err
	}
	return tlsConn, nil
}

func closeNotificationConnOnContextDone(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	return func() {
		close(done)
	}
}

func writeNotificationWakeHandshake(conn net.Conn, requestURI string, host string, key string, agentSecret string, userAgent string, proxyAuth string) error {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b,
		"GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nAuthorization: Bearer %s\r\nUser-Agent: %s\r\n",
		requestURI,
		host,
		key,
		agentSecret,
		userAgent,
	)
	if proxyAuth != "" {
		_, _ = fmt.Fprintf(&b, "Proxy-Authorization: %s\r\n", proxyAuth)
	}
	b.WriteString("\r\n")
	_, err := io.WriteString(conn, b.String())
	return err
}

func validateNotificationProfile(profile AgentProfile) error {
	if !ProfileRE.MatchString(profile.Handle) || !strings.HasPrefix(profile.AgentSecret, "as_") || strings.ContainsAny(profile.AgentSecret, "\r\n\x00") {
		return errors.New("invalid notification profile")
	}
	return nil
}

func sanitizedNotificationTransportError(err error) error {
	if err == nil {
		return errors.New("notification transport error")
	}
	return errors.New("notification transport error")
}

func isAmbiguousNotificationMutationStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500
}

func buildNotificationLeaseURL(baseURL string) (string, error) {
	return buildNotificationURL(baseURL, notificationLeaseEndpoint, nil)
}

func buildNotificationWakeURL(baseURL string) (string, error) {
	query := url.Values{}
	query.Set("client", "daemon")
	raw, err := buildNotificationURL(baseURL, notificationWakeEndpoint, query)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", errors.New("invalid notification base url")
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", errors.New("invalid notification base url")
	}
	return parsed.String(), nil
}

func buildNotificationClaimURL(baseURL string, claimID string, action string) (string, error) {
	if !isSafeCloudID("claim", claimID) {
		return "", errors.New("invalid notification claim id")
	}
	return buildNotificationURL(baseURL, "/agents/me/notifications/claim/"+url.PathEscape(claimID)+"/"+action, nil)
}

func buildNotificationURL(baseURL string, endpoint string, query url.Values) (string, error) {
	if strings.TrimSpace(baseURL) == "" || strings.ContainsAny(baseURL, "\r\n\x00") || containsSecretValue(baseURL) {
		return "", errors.New("invalid notification base url")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", errors.New("invalid notification base url")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || !isAllowedNotificationURLScheme(parsed) {
		return "", errors.New("invalid notification base url")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + endpoint
	parsed.Fragment = ""
	if query != nil {
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

func isAllowedNotificationURLScheme(parsed *url.URL) bool {
	if parsed.Scheme == "https" {
		return true
	}
	if parsed.Scheme != "http" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.IsLoopback()
}

func isSafeNotificationLeaseHolder(value string) bool {
	return value != "" && len(value) <= 200 && !strings.ContainsAny(value, "\r\n\x00") && !containsSecretValue(value)
}

func notificationUserAgent(version string) string {
	version = strings.TrimSpace(version)
	if version == "" || len(version) > 128 || strings.ContainsAny(version, "\r\n\x00") || containsSecretValue(version) {
		version = "dev"
	}
	return "comment-bus/" + version
}

func durationMilliseconds(value time.Duration) int64 {
	if value <= 0 {
		return 0
	}
	return value.Milliseconds()
}

func readNotificationHTTPBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxNotificationHTTPResponseBody+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxNotificationHTTPResponseBody {
		return nil, errors.New("notification response is too large")
	}
	return data, nil
}

func notificationHTTPStatusError(status int, body []byte) error {
	var payload struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && payload.Code != "" && !containsSecretValue(payload.Code) && !strings.ContainsAny(payload.Code, "\r\n\x00") {
		return &NotificationHTTPError{Status: status, Code: payload.Code}
	}
	return &NotificationHTTPError{Status: status}
}

func notificationWakeDialAddress(parsed *url.URL) (string, error) {
	return notificationURLDialAddress(parsed, parsed.Scheme)
}

func notificationURLDialAddress(parsed *url.URL, scheme string) (string, error) {
	host := parsed.Hostname()
	if host == "" {
		return "", errors.New("invalid notification base url")
	}
	port := parsed.Port()
	if port == "" {
		switch scheme {
		case "wss", "https":
			port = "443"
		case "ws", "http":
			port = "80"
		default:
			return "", errors.New("invalid notification base url")
		}
	}
	return net.JoinHostPort(host, port), nil
}

func notificationWakeProxyRequestURL(parsed *url.URL) (*url.URL, error) {
	proxyURL := *parsed
	switch parsed.Scheme {
	case "wss":
		proxyURL.Scheme = "https"
	case "ws":
		proxyURL.Scheme = "http"
	default:
		return nil, errors.New("invalid notification base url")
	}
	return &proxyURL, nil
}

func notificationProxyAuthorization(proxyURL *url.URL) string {
	if proxyURL == nil || proxyURL.User == nil {
		return ""
	}
	password, _ := proxyURL.User.Password()
	raw := proxyURL.User.Username() + ":" + password
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
}

func notificationWakeTLSConfig(transport *http.Transport, serverName string) *tls.Config {
	cfg := &tls.Config{}
	if transport != nil && transport.TLSClientConfig != nil {
		cfg = transport.TLSClientConfig.Clone()
	}
	if cfg.MinVersion == 0 || cfg.MinVersion < tls.VersionTLS12 {
		cfg.MinVersion = tls.VersionTLS12
	}
	if cfg.ServerName == "" {
		cfg.ServerName = serverName
	}
	cfg.NextProtos = []string{"http/1.1"}
	return cfg
}

func dialNotificationWakeTCP(ctx context.Context, transport *http.Transport, addr string) (net.Conn, error) {
	if transport != nil && transport.DialContext != nil {
		return transport.DialContext(ctx, "tcp", addr)
	}
	dialer := net.Dialer{}
	return dialer.DialContext(ctx, "tcp", addr)
}

func dialNotificationWakeLegacyTLS(ctx context.Context, dialTLS func(network string, addr string) (net.Conn, error), addr string) (net.Conn, error) {
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	go func() {
		conn, err := dialTLS("tcp", addr)
		done <- result{conn: conn, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-done:
		return result.conn, result.err
	}
}

func headerContainsToken(values []string, token string) bool {
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

type lockedConn struct {
	conn net.Conn
	mu   sync.Mutex
}

func (l *lockedConn) writeFrame(opcode byte, payload []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if notificationWakeWriteTimeout > 0 {
		if err := l.conn.SetWriteDeadline(time.Now().Add(notificationWakeWriteTimeout)); err != nil {
			return err
		}
		defer func() {
			_ = l.conn.SetWriteDeadline(time.Time{})
		}()
	}
	return writeWebSocketClientFrame(l.conn, opcode, payload)
}

func isNotificationWakeReadDeadline(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func writeWebSocketClientFrame(w io.Writer, opcode byte, payload []byte) error {
	if len(payload) > maxNotificationWakeFrameBytes {
		return errors.New("websocket payload is too large")
	}
	header := []byte{0x80 | (opcode & 0x0F)}
	switch {
	case len(payload) < 126:
		header = append(header, 0x80|byte(len(payload)))
	case len(payload) <= 0xFFFF:
		header = append(header, 0x80|126, byte(len(payload)>>8), byte(len(payload)))
	default:
		return errors.New("websocket payload is too large")
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i%4]
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	if _, err := w.Write(mask); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

func readWebSocketServerFrame(r *bufio.Reader) (byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return 0, nil, err
	}
	opcode := header[0] & 0x0F
	length := int64(header[1] & 0x7F)
	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(r, extended); err != nil {
			return 0, nil, err
		}
		length = int64(extended[0])<<8 | int64(extended[1])
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(r, extended); err != nil {
			return 0, nil, err
		}
		length = 0
		for _, b := range extended {
			length = (length << 8) | int64(b)
		}
	}
	if length < 0 || length > maxNotificationWakeFrameBytes {
		return 0, nil, errors.New("websocket frame is too large")
	}
	var mask []byte
	if header[1]&0x80 != 0 {
		mask = make([]byte, 4)
		if _, err := io.ReadFull(r, mask); err != nil {
			return 0, nil, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	if mask != nil {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return opcode, payload, nil
}
