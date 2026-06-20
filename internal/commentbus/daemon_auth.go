package commentbus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// DaemonAuthFileName is the daemon pairing credential file kept under the
// private bus directory (`<home>/bus/daemon-auth.json`).
const DaemonAuthFileName = "daemon-auth.json"

// DaemonAuth is this computer's paired-daemon credential, written by
// `comment bus pair` and read by the daemon and CLI. The Token is
// secret-equivalent: it must never be logged, printed, or included in
// diagnostics.
type DaemonAuth struct {
	DaemonID     string   `json:"daemon_id"`
	Token        string   `json:"daemon_token"`
	BaseURL      string   `json:"base_url"`
	Label        string   `json:"label"`
	Capabilities []string `json:"capabilities"`
	PairedAt     string   `json:"paired_at"`
}

// DaemonAuthPath returns the location of the daemon pairing credential file.
func DaemonAuthPath(paths Paths) string {
	return filepath.Join(paths.Bus, DaemonAuthFileName)
}

// LoadDaemonAuth reads the daemon pairing credential. A missing file is not an
// error: it returns the zero value with ok=false. A present-but-unusable file
// (symlink, unparseable, missing required fields) returns an error so callers
// can tell "unpaired" apart from "broken".
func LoadDaemonAuth(paths Paths) (DaemonAuth, bool, error) {
	path := DaemonAuthPath(paths)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return DaemonAuth{}, false, nil
	}
	if err != nil {
		return DaemonAuth{}, false, errors.New("could not inspect daemon auth file")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return DaemonAuth{}, false, errors.New("daemon auth file must not be a symlink")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return DaemonAuth{}, false, errors.New("could not read daemon auth file")
	}
	var auth DaemonAuth
	if err := json.Unmarshal(data, &auth); err != nil {
		return DaemonAuth{}, false, errors.New("daemon auth file is not valid JSON")
	}
	if strings.TrimSpace(auth.DaemonID) == "" || strings.TrimSpace(auth.Token) == "" {
		return DaemonAuth{}, false, errors.New("daemon auth file is missing required fields")
	}
	return auth, true, nil
}

// SaveDaemonAuth persists the daemon pairing credential with owner-only
// permissions (0600) under a trust-validated 0700 bus directory, creating the
// home and bus directories with the same trust conventions the agent-profile
// writer uses.
func SaveDaemonAuth(paths Paths, auth DaemonAuth) error {
	if strings.TrimSpace(auth.DaemonID) == "" || strings.TrimSpace(auth.Token) == "" {
		return errors.New("daemon auth requires daemon_id and daemon_token")
	}
	home := filepath.Clean(paths.Home)
	if err := ensureTrustedPrivateDir(home, "comment home"); err != nil {
		return err
	}
	busDir := filepath.Join(home, "bus")
	if err := ensureTrustedPrivateDir(busDir, "bus directory"); err != nil {
		return err
	}
	data, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivateFileAtomicExistingDir(filepath.Join(busDir, DaemonAuthFileName), append(data, '\n'), 0o600)
}

// DeleteDaemonAuth removes the daemon pairing credential. A missing file is
// not an error.
func DeleteDaemonAuth(paths Paths) error {
	if err := os.Remove(DaemonAuthPath(paths)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
