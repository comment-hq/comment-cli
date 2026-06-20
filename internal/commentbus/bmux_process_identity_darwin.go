//go:build darwin

package commentbus

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

func bmuxProcessIdentity(pid uint32) (string, error) {
	if pid == 0 {
		return "", ErrTmuxSessionMissing
	}
	info, err := unix.SysctlKinfoProc("kern.proc.pid", int(pid))
	if err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EIO) {
			return "", ErrTmuxSessionMissing
		}
		return "", err
	}
	if info == nil || info.Proc.P_pid != int32(pid) {
		return "", ErrTmuxSessionMissing
	}
	sec, nsec := info.Proc.P_starttime.Unix()
	if sec == 0 && nsec == 0 {
		return "", ErrTmuxSessionMissing
	}
	return fmt.Sprintf("darwin:%d:%d", sec, nsec), nil
}
