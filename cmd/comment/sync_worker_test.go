//go:build darwin || linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
	"github.com/comment-hq/comment-cli/internal/commentsync"
)

func TestMonitorLiveSyncConfigCancelsWhenLiveSyncDisabled(t *testing.T) {
	home := privateTempHome(t, "comment-cli-sync-monitor-")
	root := privateTempHome(t, "comment-cli-sync-root-")
	const key = "usk_v2.ag_test.key.secret-secret-secret"

	if _, err := commentsync.Login(context.Background(), commentsync.Options{
		Home:    home,
		Root:    root,
		BaseURL: "https://comment.io",
		APIKey:  key,
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := commentsync.SetBackgroundSync(context.Background(), commentsync.Options{Home: home}, true); err != nil {
		t.Fatalf("enable background sync: %v", err)
	}
	if err := commentsync.SetLiveSync(context.Background(), commentsync.Options{Home: home}, true); err != nil {
		t.Fatalf("enable live sync: %v", err)
	}
	status, err := commentsync.ReadStatus(commentsync.Options{Home: home})
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	previousPollInterval := syncLiveConfigPollInterval
	syncLiveConfigPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { syncLiveConfigPollInterval = previousPollInterval })

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()
	canceled := make(chan struct{})
	var closeCanceled sync.Once
	cancelRun := func() {
		closeCanceled.Do(func() {
			close(canceled)
			cancelCtx()
		})
	}
	stopMonitor := monitorLiveSyncConfig(ctx, mustResolveCLIPaths(t, home), status.ConfigGeneration, cancelRun)
	defer stopMonitor()

	if err := commentsync.SetLiveSync(context.Background(), commentsync.Options{Home: home}, false); err != nil {
		t.Fatalf("disable live sync: %v", err)
	}

	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not cancel after live sync was disabled")
	}
}

func TestSyncFileWatchWorkerRestoresDirtyProjection(t *testing.T) {
	home := privateTempHome(t, "comment-cli-sync-watch-")
	root := privateTempHome(t, "comment-cli-sync-watch-root-")
	const key = "usk_v2.ag_test.key.secret-secret-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/auth/library-sync/snapshot":
			writeSyncWorkerTestJSON(t, w, map[string]any{
				"snapshotId": "watch_test",
				"scopeLabel": "My Files and Shared With Me",
				"coveredSections": []map[string]any{
					{"id": "my-files", "label": "My Files", "covered": true, "authoritative": true, "count": 1},
				},
				"unsupportedSections": []map[string]any{},
				"snapshotComplete":    true,
				"rows": []map[string]any{
					{"visibleInstanceId": "doc-1", "section": "my-files", "kind": "document", "name": "Launch", "parentVisibleInstanceId": nil, "docSlug": "abc123"},
				},
				"pageInfo": map[string]any{"nextCursor": nil, "partial": false},
			})
		case "/docs/abc123":
			writeSyncWorkerTestJSON(t, w, map[string]any{
				"slug":         "abc123",
				"title":        "Launch",
				"markdown":     "# Launch\n",
				"revision":     1,
				"content_hash": syncWorkerTestSHA256("# Launch\n"),
				"etag":         "test",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := commentsync.Login(context.Background(), commentsync.Options{
		Home:    home,
		Root:    root,
		BaseURL: server.URL,
		APIKey:  key,
	}); err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := commentsync.SetBackgroundSync(context.Background(), commentsync.Options{Home: home}, true); err != nil {
		t.Fatalf("enable background sync: %v", err)
	}
	if _, err := commentsync.Once(context.Background(), commentsync.Options{Home: home}); err != nil {
		t.Fatalf("once: %v", err)
	}
	target := filepath.Join(root, "My Files", "Launch.md")
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod projection: %v", err)
	}
	if err := os.WriteFile(target, []byte("# Local edit\n"), 0o644); err != nil {
		t.Fatalf("dirty projection: %v", err)
	}

	previousWatchInterval := syncFileWatchInterval
	syncFileWatchInterval = 10 * time.Millisecond
	t.Cleanup(func() { syncFileWatchInterval = previousWatchInterval })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runSyncFileWatchWorker(ctx, mustResolveCLIPaths(t, home))

	deadline := time.Now().Add(2 * time.Second)
	for {
		data, err := os.ReadFile(target)
		if err == nil && string(data) != "# Local edit\n" && string(data) != "" {
			if string(data) == "# Launch\n" || containsProjectionBody(data, "# Launch\n") {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for file watch recovery; read err=%v body=%q", err, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	recoveries, err := commentsync.ListRecoveries(context.Background(), commentsync.Options{Home: home})
	if err != nil {
		t.Fatalf("list recoveries: %v", err)
	}
	if len(recoveries) != 1 || recoveries[0].Reason != "local_dirty_before_overwrite" {
		t.Fatalf("recoveries = %+v", recoveries)
	}
}

func mustResolveCLIPaths(t *testing.T, home string) commentbus.Paths {
	t.Helper()
	paths, err := resolveCLIPaths(home)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func writeSyncWorkerTestJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatalf("write json: %v", err)
	}
}

func syncWorkerTestSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func containsProjectionBody(data []byte, body string) bool {
	return len(data) >= len(body) && string(data[len(data)-len(body):]) == body
}
