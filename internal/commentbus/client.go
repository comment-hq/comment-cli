package commentbus

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
)

const maxSocketResponseBytes = 256 << 20
const socketRequestAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-"

func GenerateSocketRequestID() (string, error) {
	bytes := make([]byte, 28)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	out := make([]byte, 0, len("req_")+len(bytes))
	out = append(out, "req_"...)
	for _, b := range bytes {
		out = append(out, socketRequestAlphabet[int(b)%len(socketRequestAlphabet)])
	}
	return string(out), nil
}

func CallSocket(ctx context.Context, paths Paths, req SocketRequest, responseTimeout time.Duration) (SocketResponse, error) {
	if responseTimeout <= 0 {
		responseTimeout = 10 * time.Second
	}
	dialer := net.Dialer{Timeout: socketReadTimeout}
	network, address := "unix", paths.Socket
	if paths.BusTCPAddr != "" {
		network, address = "tcp", paths.BusTCPAddr
	}
	conn, err := dialer.DialContext(ctx, network, address)
	if err != nil {
		return SocketResponse{}, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(responseTimeout))
	}
	// A resettable byte budget: the handshake read and the final response read
	// each get their own bound so a large valid response is never truncated by
	// bytes the handshake consumed.
	limited := &io.LimitedReader{R: conn, N: maxSocketResponseBytes + 1}
	reader := bufio.NewReader(limited)
	// Over TCP, a request that carries a capability must first verify the listener
	// is the genuine daemon (server-auth handshake) — otherwise an impostor at the
	// address could harvest the capability. No capability to protect (e.g. health)
	// or a Unix socket (peer-cred gated) → no handshake.
	if network == "tcp" && req.Auth != nil && req.Auth.Capability != "" {
		nonce, err := generateHandshakeNonce()
		if err != nil {
			return SocketResponse{}, err
		}
		hsAuth := *req.Auth
		hsAuth.Capability = "" // never send the capability in the handshake
		hsEncoded, err := json.Marshal(handshakeRequest{HSNonce: nonce, Auth: &hsAuth})
		if err != nil {
			return SocketResponse{}, err
		}
		if _, err := conn.Write(append(hsEncoded, '\n')); err != nil {
			return SocketResponse{}, err
		}
		limited.N = maxHandshakeResponseBytes // bound the handshake read
		hsLine, err := reader.ReadBytes('\n')
		if err != nil {
			return SocketResponse{}, errors.New("daemon did not complete the TCP handshake; refusing to send capability")
		}
		var hsResp handshakeResponse
		if err := json.Unmarshal(hsLine, &hsResp); err != nil {
			return SocketResponse{}, errors.New("invalid TCP handshake response; refusing to send capability")
		}
		expected := handshakeProof(req.Auth.Capability, nonce)
		if !hmac.Equal([]byte(expected), []byte(hsResp.HSProof)) {
			return SocketResponse{}, errors.New("daemon failed TCP handshake authentication; refusing to send capability")
		}
		// Restore a full budget for the real response.
		limited.N = maxSocketResponseBytes + 1
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return SocketResponse{}, err
	}
	if _, err := conn.Write(append(encoded, '\n')); err != nil {
		return SocketResponse{}, err
	}
	line, err := reader.ReadBytes('\n')
	if len(line) > maxSocketResponseBytes {
		return SocketResponse{}, errors.New("socket response too large")
	}
	if err != nil {
		return SocketResponse{}, err
	}
	var response SocketResponse
	if err := json.Unmarshal(line, &response); err != nil {
		return SocketResponse{}, err
	}
	return response, nil
}
