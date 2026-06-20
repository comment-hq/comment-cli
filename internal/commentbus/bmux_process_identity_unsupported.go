//go:build !darwin && !linux

package commentbus

func bmuxProcessIdentity(uint32) (string, error) {
	return "", ErrTmuxSessionMissing
}
