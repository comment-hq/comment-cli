package commentbus

import (
	"errors"
	"path/filepath"
)

const (
	SessionHostTmux = "tmux"
	SessionHostBmux = "bmux"
)

func normalizeSessionHost(host string) string {
	if host == "" {
		return SessionHostTmux
	}
	return host
}

// defaultNewSessionHost is the single source of truth for which multiplexer a
// brand-new managed/transient runtime is created under when the caller does not
// pin a host explicitly. We default to tmux (the standard, externally-maintained
// multiplexer) and keep bmux a deliberate opt-in via host="bmux" /
// COMMENT_IO_BMUX_BIN. Flipping the default is a one-line change here.
func defaultNewSessionHost() string {
	return SessionHostTmux
}

func normalizeNewManagedSessionHost(host string) string {
	if host == "" {
		return defaultNewSessionHost()
	}
	return normalizeSessionHost(host)
}

func isSessionHost(host string) bool {
	switch normalizeSessionHost(host) {
	case SessionHostTmux, SessionHostBmux:
		return true
	default:
		return false
	}
}

func validateSessionNameForHost(host string, sessionName string) error {
	switch normalizeSessionHost(host) {
	case SessionHostTmux, SessionHostBmux:
		return validateTmuxSessionName(sessionName)
	default:
		return errors.New("invalid session host")
	}
}

func validatePaneTargetForHost(host string, paneTarget string) error {
	switch normalizeSessionHost(host) {
	case SessionHostTmux:
		return validateTmuxPaneTarget(paneTarget)
	case SessionHostBmux:
		if !isSafeAbsoluteLocalPath(paneTarget) || filepath.Ext(paneTarget) != ".sock" {
			return errors.New("invalid bmux socket path")
		}
		return nil
	default:
		return errors.New("invalid session host")
	}
}

func validateBmuxSessionPaneTarget(paths Paths, sessionName string, paneTarget string) error {
	if sessionName == "" {
		return errors.New("invalid bmux session name")
	}
	expected, err := BmuxSocketPathForSession(paths, sessionName)
	if err != nil {
		return err
	}
	if paneTarget != filepath.Clean(paneTarget) || paneTarget != expected {
		return errors.New("invalid bmux socket path")
	}
	return nil
}
