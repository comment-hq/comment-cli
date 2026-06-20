//go:build linux

package commentbus

import (
	"net"

	"golang.org/x/sys/unix"
)

type PeerCredential struct {
	UID uint32
	GID uint32
	PID int32
}

func PeerCredentialFor(conn *net.UnixConn) (PeerCredential, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return PeerCredential{}, err
	}
	var cred *unix.Ucred
	var getErr error
	controlErr := raw.Control(func(fd uintptr) {
		cred, getErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if controlErr != nil {
		return PeerCredential{}, controlErr
	}
	if getErr != nil {
		return PeerCredential{}, getErr
	}
	return PeerCredential{UID: cred.Uid, GID: cred.Gid, PID: cred.Pid}, nil
}
