package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
	"github.com/comment-hq/comment-cli/internal/commentsync"
)

const syncWorkerInterval = 30 * time.Second
const syncLiveReconnectMin = 2 * time.Second
const syncLiveReconnectMax = 60 * time.Second
const syncLiveBackoffResetAfter = 1 * time.Minute

var syncFileWatchInterval = 2 * time.Second
var syncLiveConfigPollInterval = 5 * time.Second

func startSyncWorker(ctx context.Context, paths commentbus.Paths) {
	go runSyncLiveWorker(ctx, paths)
	go runSyncFileWatchWorker(ctx, paths)
	go func() {
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				runSyncWorkerOnce(ctx, paths)
				timer.Reset(syncWorkerInterval)
			}
		}
	}()
}

func runSyncFileWatchWorker(ctx context.Context, paths commentbus.Paths) {
	timer := time.NewTimer(syncFileWatchInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			status, err := commentsync.ReadStatus(commentsync.Options{Home: paths.Home})
			if err != nil {
				writeSyncWorkerLog(paths, "warn", "sync.watch.status_failed", map[string]any{"error": err.Error()})
			} else if status.Configured && status.BackgroundSync {
				result, err := commentsync.RecoverDirtyProjections(ctx, commentsync.Options{Home: paths.Home})
				if err != nil {
					writeSyncWorkerLog(paths, "warn", "sync.watch.recovery_failed", map[string]any{"error": err.Error()})
				} else if result.ProjectionRefreshes > 0 || result.SnapshotRefreshes > 0 || result.RecoveriesPreserved > 0 {
					writeSyncWorkerLog(paths, "info", "sync.watch.recovery_complete", map[string]any{
						"checked":                 result.Checked,
						"projection_refreshes":    result.ProjectionRefreshes,
						"snapshot_refreshes":      result.SnapshotRefreshes,
						"recoveries_preserved":    result.RecoveriesPreserved,
						"not_modified":            result.NotModified,
						"scan_interval_millisecs": syncFileWatchInterval.Milliseconds(),
					})
				}
			}
			timer.Reset(syncFileWatchInterval)
		}
	}
}

func runSyncLiveWorker(ctx context.Context, paths commentbus.Paths) {
	backoff := syncLiveReconnectMin
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		status, err := commentsync.ReadStatus(commentsync.Options{Home: paths.Home})
		if err != nil || !status.Configured || !status.BackgroundSync || !status.LiveSyncEnabled {
			if err != nil {
				writeSyncWorkerLog(paths, "warn", "sync.live.status_failed", map[string]any{"error": err.Error()})
			}
			if !sleepContext(ctx, syncWorkerInterval) {
				return
			}
			continue
		}
		startedAt := time.Now()
		runCtx, cancelRun := context.WithCancel(ctx)
		stopMonitor := monitorLiveSyncConfig(runCtx, paths, status.ConfigGeneration, cancelRun)
		result, err := commentsync.RunLiveSync(runCtx, commentsync.Options{Home: paths.Home})
		stopMonitor()
		cancelRun()
		if time.Since(startedAt) >= syncLiveBackoffResetAfter || result.EventsProcessed > 0 || result.ProjectionRefreshes > 0 || result.SnapshotRefreshes > 0 || result.NotModified > 0 {
			backoff = syncLiveReconnectMin
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			writeSyncWorkerLog(paths, "warn", "sync.live.disconnected", map[string]any{
				"error":                err.Error(),
				"reconnect_after_ms":   backoff.Milliseconds(),
				"events_processed":     result.EventsProcessed,
				"projection_refreshes": result.ProjectionRefreshes,
				"snapshot_refreshes":   result.SnapshotRefreshes,
				"not_modified":         result.NotModified,
			})
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff *= 2
			if backoff > syncLiveReconnectMax {
				backoff = syncLiveReconnectMax
			}
			continue
		}
		if err == nil {
			writeSyncWorkerLog(paths, "info", "sync.live.complete", map[string]any{
				"events_processed":     result.EventsProcessed,
				"projection_refreshes": result.ProjectionRefreshes,
				"snapshot_refreshes":   result.SnapshotRefreshes,
				"not_modified":         result.NotModified,
			})
		}
		backoff = syncLiveReconnectMin
	}
}

func monitorLiveSyncConfig(ctx context.Context, paths commentbus.Paths, generation int, cancel context.CancelFunc) func() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(syncLiveConfigPollInterval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				status, err := commentsync.ReadStatus(commentsync.Options{Home: paths.Home})
				if err != nil || !status.Configured || !status.BackgroundSync || !status.LiveSyncEnabled || status.ConfigGeneration != generation {
					cancel()
					return
				}
				timer.Reset(syncLiveConfigPollInterval)
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func sleepContext(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func runSyncWorkerOnce(ctx context.Context, paths commentbus.Paths) {
	status, err := commentsync.ReadStatus(commentsync.Options{Home: paths.Home})
	if err != nil || !status.Configured || !status.BackgroundSync {
		if err != nil {
			writeSyncWorkerLog(paths, "warn", "sync.status_failed", map[string]any{"error": err.Error()})
		}
		return
	}
	result, err := commentsync.Once(ctx, commentsync.Options{Home: paths.Home})
	if err != nil {
		writeSyncWorkerLog(paths, "error", "sync.once_failed", map[string]any{"error": err.Error()})
		return
	}
	writeSyncWorkerLog(paths, "info", "sync.once_complete", map[string]any{
		"snapshot_id":          result.SnapshotID,
		"documents_written":    result.DocumentsWritten,
		"documents_removed":    result.DocumentsRemoved,
		"recoveries_preserved": result.RecoveriesPreserved,
	})
}

func writeSyncWorkerLog(paths commentbus.Paths, level string, msg string, data map[string]any) {
	writeDaemonWorkerLog(paths, "sync.library", level, msg, data)
}

func writeDaemonWorkerLog(paths commentbus.Paths, component string, level string, msg string, data map[string]any) {
	if err := os.MkdirAll(paths.Logs, 0o700); err != nil {
		return
	}
	file, err := os.OpenFile(filepath.Join(paths.Logs, "commentd.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_ = json.NewEncoder(file).Encode(map[string]any{
		"ts":        time.Now().UTC().Format(time.RFC3339),
		"level":     level,
		"component": component,
		"msg":       msg,
		"data":      data,
	})
}
