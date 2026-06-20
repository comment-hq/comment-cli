//go:build !darwin && !linux

package commentbus

import (
	"errors"
	"net"
)

type PeerCredential struct {
	UID uint32
	GID uint32
	PID int32
}

func PeerCredentialFor(conn *net.UnixConn) (PeerCredential, error) {
	return PeerCredential{}, errors.New("peer credentials are not supported on this platform")
}
