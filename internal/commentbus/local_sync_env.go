package commentbus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type localSyncRuntimeConfig struct {
	Root            string `json:"root"`
	KeyID           string `json:"key_id,omitempty"`
	BackgroundSync  bool   `json:"background_sync"`
	LiveSyncEnabled bool   `json:"live_sync_enabled"`
}

type localSyncLiveStatus struct {
	State     string `json:"state"`
	Root      string `json:"root"`
	KeyID     string `json:"key_id,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

const localSyncAgentDocsMarkerName = ".comment-agent-docs.json"
const localSyncLiveStatusFreshFor = 2 * time.Minute

// localSyncOrientationPaths returns the resolved sync-root and docs-root
// values the daemon would otherwise export as COMMENT_IO_LOCAL_SYNC_ROOT and
// COMMENT_IO_LOCAL_DOCS_ROOT into the runtime shell. Empty strings mean that
// variable would not have been exported (sync not configured, marker file
// missing, etc.).
func localSyncOrientationPaths(paths Paths) (docsRoot string, syncRoot string) {
	for _, entry := range localSyncRuntimeEnv(paths) {
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		key, value := entry[:eq], entry[eq+1:]
		switch key {
		case "COMMENT_IO_LOCAL_SYNC_ROOT":
			syncRoot = value
		case "COMMENT_IO_LOCAL_DOCS_ROOT":
			docsRoot = value
		}
	}
	return docsRoot, syncRoot
}

// LocalSyncOrientationPaths returns the local docs/sync roots used in startup
// orientation. CLI setup code uses this to build the same preview prompt the
// daemon will paste into managed runtimes.
func LocalSyncOrientationPaths(paths Paths) (docsRoot string, syncRoot string) {
	return localSyncOrientationPaths(paths)
}

func localSyncRuntimeEnv(paths Paths) []string {
	data, err := os.ReadFile(filepath.Join(paths.Home, "sync", "config.json"))
	if err != nil {
		return nil
	}
	var cfg localSyncRuntimeConfig
	if err := json.Unmarshal(data, &cfg); err != nil || strings.TrimSpace(cfg.Root) == "" {
		return nil
	}
	root := filepath.Clean(cfg.Root)
	freshness := "periodic"
	if cfg.BackgroundSync && cfg.LiveSyncEnabled && localSyncLiveStatusOK(paths, root, cfg.KeyID, time.Now()) {
		freshness = "live"
	}
	env := []string{
		"COMMENT_IO_LOCAL_SYNC=1",
		"COMMENT_IO_LOCAL_SYNC_ROOT=" + root,
		"COMMENT_IO_LOCAL_SYNC_MODE=read-only-projection",
		"COMMENT_IO_LOCAL_SYNC_FRESHNESS=" + freshness,
	}
	docsRoot := filepath.Join(root, "_Comment.io Docs")
	markerPath := filepath.Join(docsRoot, localSyncAgentDocsMarkerName)
	if markerOK := localSyncAgentDocsMarkerOK(markerPath); markerOK {
		info, err := os.Stat(filepath.Join(docsRoot, "llms.txt"))
		if err == nil && info.Mode().IsRegular() {
			env = append(env, "COMMENT_IO_LOCAL_DOCS_ROOT="+docsRoot)
		}
	}
	return env
}

func localSyncLiveStatusOK(paths Paths, root string, keyID string, now time.Time) bool {
	data, err := os.ReadFile(filepath.Join(paths.Home, "sync", "live-status.json"))
	if err != nil {
		return false
	}
	var status localSyncLiveStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return false
	}
	if status.State != "connected" || filepath.Clean(status.Root) != root {
		return false
	}
	if keyID != "" && status.KeyID != "" && status.KeyID != keyID {
		return false
	}
	updatedAt, err := time.Parse(time.RFC3339, status.UpdatedAt)
	if err != nil {
		return false
	}
	return !updatedAt.After(now.Add(30*time.Second)) && now.Sub(updatedAt) <= localSyncLiveStatusFreshFor
}

func localSyncAgentDocsMarkerOK(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var marker map[string]any
	if err := json.Unmarshal(data, &marker); err != nil {
		return false
	}
	if marker["managed_by"] != "comment sync" || marker["kind"] != "local-agent-docs" {
		return false
	}
	if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
		return false
	}
	return true
}
