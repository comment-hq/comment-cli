package commentbus

import (
	"errors"
	"os"
	"path/filepath"
)

type Paths struct {
	Home            string
	Socket          string
	// BusTCPAddr, when non-empty, is the TCP address the CLIENT dials instead of
	// the Unix socket — an opt-in transport for environments where a bind-mounted
	// Unix socket can't be reached across a boundary (e.g. Docker Desktop on
	// macOS). It is CLIENT-only: the daemon's own bind address is
	// DaemonOptions.TCPListenAddr, kept separate so a host that exports the dial
	// address doesn't make a native daemon try to bind it. Injected at the
	// CLI/socket boundary (resolveCLIPaths) from COMMENT_IO_BUS_TCP_ADDR — not in
	// this generic resolver, so library/test Paths don't inherit an ambient value.
	BusTCPAddr      string
	PID             string
	Logs            string
	Bus             string
	History         string
	Ops             string
	OpsPending      string
	OpsDone         string
	Sessions        string
	Capabilities    string
	OwnerCapability string
	Private         string
	Spool           string
}

func DefaultHomeDir() (string, error) {
	return CurrentEnvironment().DefaultHomeDir()
}

func ResolvePaths(home string) (Paths, error) {
	if home == "" {
		defaultHome, err := DefaultHomeDir()
		if err != nil {
			return Paths{}, err
		}
		home = defaultHome
	}
	cleaned, err := expandHome(home)
	if err != nil {
		return Paths{}, err
	}
	bus := filepath.Join(cleaned, "bus")
	ops := filepath.Join(bus, "ops")
	caps := filepath.Join(bus, "capabilities")
	return Paths{
		Home:            cleaned,
		Socket:          filepath.Join(cleaned, "daemon.sock"),
		PID:             filepath.Join(cleaned, "daemon.pid"),
		Logs:            filepath.Join(cleaned, "logs"),
		Bus:             bus,
		History:         filepath.Join(bus, "history.sqlite"),
		Ops:             ops,
		OpsPending:      filepath.Join(ops, "pending"),
		OpsDone:         filepath.Join(ops, "done"),
		Sessions:        filepath.Join(bus, "sessions"),
		Capabilities:    caps,
		OwnerCapability: filepath.Join(caps, "owner.cap"),
		Private:         filepath.Join(bus, "private"),
		Spool:           filepath.Join(bus, "spool"),
	}, nil
}

func EnsureBaseDirs(paths Paths) error {
	for _, dir := range []string{
		paths.Home,
		paths.Logs,
		paths.Bus,
		paths.Ops,
		paths.OpsPending,
		paths.OpsDone,
		paths.Sessions,
		paths.Capabilities,
		paths.Private,
		paths.Spool,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// ExpandHome resolves a path that may start with `~` (the current user's
// home) or be relative to the current working directory. Returns a clean
// absolute path.
func ExpandHome(path string) (string, error) {
	return expandHome(path)
}

func expandHome(path string) (string, error) {
	if path == "" {
		return "", errors.New("empty path")
	}
	if path == "~" || len(path) > 2 && path[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		path = abs
	}
	return filepath.Clean(path), nil
}
