package commentbus

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"time"
)

// Server-authentication handshake for the opt-in TCP transport.
//
// Over the Unix socket the client connects to a 0600 file it owns and the daemon
// verifies the peer UID, so each side implicitly trusts the other. Over TCP there
// is no such guarantee: if the daemon is down and another process squats the
// address, a naive client would hand its capability token to that impostor. To
// prevent that, a TCP client first makes the daemon PROVE it holds the same
// capability — HMAC(capability, nonce) — and only sends the real (capability-
// bearing) request once the proof verifies. The capability itself never appears
// in the handshake. The daemon answers a handshake on any transport (harmless:
// the proof reveals nothing about the high-entropy capability), but only the TCP
// client initiates one.
const handshakeContext = "comment-bus-handshake-v1"

// maxHandshakeResponseBytes bounds the handshake reply read so a hostile listener
// can't exhaust memory before the client verifies it. The real reply is ~80 bytes.
const maxHandshakeResponseBytes = 64 << 10

// handshakeRequest is the client's opening line on a TCP connection. It carries a
// fresh nonce and the auth envelope WITHOUT the capability value (mode + session
// identifiers only) so the daemon knows which capability to prove knowledge of.
type handshakeRequest struct {
	HSNonce string      `json:"hs_nonce"`
	Auth    *SocketAuth `json:"auth,omitempty"`
}

// handshakeResponse is the daemon's proof that it holds the expected capability.
type handshakeResponse struct {
	HSProof string `json:"hs_proof"`
}

// handshakeProof = HMAC-SHA256(capability, context:nonce), hex-encoded. The
// capability is high-entropy, so emitting this proof leaks nothing usable.
func handshakeProof(capability string, nonce string) string {
	mac := hmac.New(sha256.New, []byte(capability))
	mac.Write([]byte(handshakeContext + ":" + nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

func generateHandshakeNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// isHandshakeLine reports whether a received line is a handshake opener (has a
// nonce and no op) rather than a normal request.
func isHandshakeLine(raw []byte) (handshakeRequest, bool) {
	var probe struct {
		HSNonce string `json:"hs_nonce"`
		Op      string `json:"op"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return handshakeRequest{}, false
	}
	if probe.HSNonce == "" || probe.Op != "" {
		return handshakeRequest{}, false
	}
	var hs handshakeRequest
	if err := json.Unmarshal(raw, &hs); err != nil {
		return handshakeRequest{}, false
	}
	return hs, true
}

func writeHandshakeResponse(conn net.Conn, resp handshakeResponse) error {
	encoded, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(socketWriteTimeout)); err != nil {
		return err
	}
	defer func() { _ = conn.SetWriteDeadline(time.Time{}) }()
	_, err = conn.Write(append(encoded, '\n'))
	return err
}
