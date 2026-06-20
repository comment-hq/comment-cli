package commentsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/comment-hq/comment-cli/internal/commentbus"
	_ "modernc.org/sqlite"
)

const stateSchemaVersion = 6

type syncState struct {
	db    *sql.DB
	paths commentbus.Paths
}

type recoveryItem struct {
	ID           string    `json:"id"`
	VisibleID    string    `json:"visibleInstanceId"`
	Slug         string    `json:"slug,omitempty"`
	OriginalPath string    `json:"originalPath"`
	ArtifactPath string    `json:"artifactPath"`
	Reason       string    `json:"reason"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"createdAt"`
}

type syncOp struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	State      string    `json:"state"`
	VisibleID  string    `json:"visibleInstanceId,omitempty"`
	Slug       string    `json:"slug,omitempty"`
	Path       string    `json:"path,omitempty"`
	SnapshotID string    `json:"snapshotId,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func openSyncState(ctx context.Context, paths commentbus.Paths) (*syncState, error) {
	if err := ensurePrivateDirs(paths); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(paths.Home, "sync", "library.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	state := &syncState{db: db, paths: paths}
	if _, err := db.ExecContext(ctx, `PRAGMA busy_timeout = 5000`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := state.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return state, nil
}

func (s *syncState) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *syncState) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > stateSchemaVersion {
		return errors.New("sync database was written by a newer comment binary")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 3 {
		// Pre-rebrand sync DBs (created by the Botspring CLI — e.g. an existing
		// shared ~/.comment-io DB already at user_version 3) carry botspring_*
		// placement columns; the Botlets code reads botlets_* columns. Per the
		// rebrand we recreate local sync state rather than migrating it
		// (plan §1: recreate, no SQLite RENAME COLUMN): drop the data tables and
		// let the version==0 block below rebuild them at the current schema.
		// Placements re-sync on the next run.
		for _, statement := range []string{
			`DROP TABLE IF EXISTS placements`,
			`DROP TABLE IF EXISTS recoveries`,
			`DROP TABLE IF EXISTS sync_ops`,
			`DROP TABLE IF EXISTS snapshot_runs`,
			`DROP TABLE IF EXISTS retries`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		version = 0
	}
	if version == 0 {
		statements := []string{
			`CREATE TABLE IF NOT EXISTS placements (
			visible_id TEXT PRIMARY KEY,
			slug TEXT NOT NULL,
			section TEXT NOT NULL,
			path TEXT NOT NULL,
			canonical_path TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			body_content_hash TEXT NOT NULL DEFAULT '',
			rendered_projection_hash TEXT NOT NULL DEFAULT '',
			projection_format_version INTEGER NOT NULL DEFAULT 0,
			frontmatter_flavor TEXT NOT NULL DEFAULT '',
			links_flavor TEXT NOT NULL DEFAULT '',
			etag TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL,
			last_seen_snapshot TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			botlets_owner_handle TEXT NOT NULL DEFAULT '',
			botlets_bot_slug TEXT NOT NULL DEFAULT '',
			botlets_bot_local_name TEXT NOT NULL DEFAULT '',
			botlets_bot_id TEXT NOT NULL DEFAULT '',
			botlets_bot_handle TEXT NOT NULL DEFAULT '',
			botlets_bot_agent_id TEXT NOT NULL DEFAULT '',
			botlets_brain_container_id TEXT NOT NULL DEFAULT '',
			botlets_brain_root_folder_id TEXT NOT NULL DEFAULT '',
			botlets_brain_node_id TEXT NOT NULL DEFAULT ''
		)`,
			`CREATE TABLE IF NOT EXISTS recoveries (
			id TEXT PRIMARY KEY,
			visible_id TEXT NOT NULL,
			slug TEXT NOT NULL,
			original_path TEXT NOT NULL,
			artifact_path TEXT NOT NULL,
			reason TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
			`CREATE TABLE IF NOT EXISTS sync_ops (
			id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			state TEXT NOT NULL,
			visible_id TEXT NOT NULL DEFAULT '',
			slug TEXT NOT NULL DEFAULT '',
			path TEXT NOT NULL DEFAULT '',
			snapshot_id TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			started_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
			`CREATE TABLE IF NOT EXISTS snapshot_runs (
			snapshot_id TEXT PRIMARY KEY,
			scope_label TEXT NOT NULL,
			complete INTEGER NOT NULL,
			unsupported_json TEXT NOT NULL,
			started_at TEXT NOT NULL,
			completed_at TEXT
		)`,
			`CREATE TABLE IF NOT EXISTS retries (
			visible_id TEXT PRIMARY KEY,
			slug TEXT NOT NULL,
			op TEXT NOT NULL,
			snapshot_id TEXT NOT NULL,
			path TEXT NOT NULL,
			failure_class TEXT NOT NULL,
			error TEXT NOT NULL,
			retry_after TEXT,
			updated_at TEXT NOT NULL
		)`,
		}
		for _, statement := range statements {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		// Fresh DBs are created at the current schema (botlets_* placement
		// columns). Botspring->Botlets pre-release rebrand recreated local sync
		// state rather than migrating it (new ~/botlets home = fresh DB).
		version = stateSchemaVersion
	}
	if version == 1 {
		for _, statement := range []string{
			`ALTER TABLE placements ADD COLUMN body_content_hash TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE placements ADD COLUMN rendered_projection_hash TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE placements ADD COLUMN projection_format_version INTEGER NOT NULL DEFAULT 0`,
			`ALTER TABLE placements ADD COLUMN etag TEXT NOT NULL DEFAULT ''`,
			`UPDATE placements SET body_content_hash = content_hash WHERE body_content_hash = ''`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		version = 2
	}
	if version == 2 {
		for _, statement := range []string{
			`ALTER TABLE placements ADD COLUMN botlets_owner_handle TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE placements ADD COLUMN botlets_bot_slug TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE placements ADD COLUMN botlets_bot_local_name TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE placements ADD COLUMN botlets_bot_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE placements ADD COLUMN botlets_bot_handle TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE placements ADD COLUMN botlets_bot_agent_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE placements ADD COLUMN botlets_brain_container_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE placements ADD COLUMN botlets_brain_root_folder_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE placements ADD COLUMN botlets_brain_node_id TEXT NOT NULL DEFAULT ''`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		// Flow into the v5 flavor migration below instead of jumping straight to
		// the current version, so an older DB still gains the flavor columns.
		version = 5
	}
	if version == 4 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE placements ADD COLUMN botlets_bot_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
		version = 5
	}
	if version == 5 {
		for _, statement := range []string{
			`ALTER TABLE placements ADD COLUMN frontmatter_flavor TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE placements ADD COLUMN links_flavor TEXT NOT NULL DEFAULT ''`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		version = stateSchemaVersion
	}
	if version != stateSchemaVersion {
		return errors.New("sync database migration did not reach current schema")
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, stateSchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *syncState) replayIncompleteOps(ctx context.Context, root string) error {
	ops, err := s.listPendingOps(ctx)
	if err != nil {
		return err
	}
	for _, op := range ops {
		if op.Kind == "write_projection" && op.VisibleID != "" && op.Path != "" {
			if err := validateManagedPath(root, op.Path); err != nil {
				return err
			}
			data, readErr := os.ReadFile(op.Path)
			if readErr == nil {
				if err := preserveRecovery(ctx, s.paths, s, op.VisibleID, op.Slug, op.Path, "incomplete_write_replayed", data); err != nil {
					return err
				}
				if err := os.Remove(op.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			} else if !errors.Is(readErr, os.ErrNotExist) {
				return readErr
			}
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
		UPDATE sync_ops
		SET state = 'replay_checked', updated_at = ?
		WHERE state IN ('pending', 'started')
	`, now)
	return err
}

func (s *syncState) beginOp(ctx context.Context, kind, visibleID, slug, path, snapshotID string) (syncOp, error) {
	now := time.Now().UTC()
	op := syncOp{
		ID:         "op_" + shortStableSuffix(fmt.Sprintf("%s:%s:%s:%d", kind, visibleID, path, now.UnixNano())),
		Kind:       kind,
		State:      "started",
		VisibleID:  visibleID,
		Slug:       slug,
		Path:       path,
		SnapshotID: snapshotID,
		StartedAt:  now,
		UpdatedAt:  now,
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_ops (id, kind, state, visible_id, slug, path, snapshot_id, started_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, op.ID, op.Kind, op.State, op.VisibleID, op.Slug, op.Path, op.SnapshotID, formatTime(op.StartedAt), formatTime(op.UpdatedAt)); err != nil {
		return syncOp{}, err
	}
	if err := writeJSON0600(filepath.Join(s.paths.Home, "sync", "ops", op.ID+".json"), op); err != nil {
		return syncOp{}, err
	}
	return op, nil
}

func (s *syncState) finishOp(ctx context.Context, op syncOp, state string, opErr error) error {
	now := time.Now().UTC().Format(time.RFC3339)
	errText := ""
	if opErr != nil {
		errText = opErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `
		UPDATE sync_ops SET state = ?, error = ?, updated_at = ? WHERE id = ?
	`, state, errText, now, op.ID)
	if err != nil {
		return err
	}
	op.State = state
	op.UpdatedAt = time.Now().UTC()
	if errText != "" {
		op.Path = op.Path + " error=" + errText
	}
	return writeJSON0600(filepath.Join(s.paths.Home, "sync", "ops", op.ID+".json"), op)
}

func (s *syncState) listPendingOps(ctx context.Context) ([]syncOp, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, state, visible_id, slug, path, snapshot_id, started_at, updated_at
		FROM sync_ops
		WHERE state IN ('pending', 'started')
		ORDER BY started_at, id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ops []syncOp
	for rows.Next() {
		var op syncOp
		var startedAt string
		var updatedAt string
		if err := rows.Scan(&op.ID, &op.Kind, &op.State, &op.VisibleID, &op.Slug, &op.Path, &op.SnapshotID, &startedAt, &updatedAt); err != nil {
			return nil, err
		}
		if parsed, err := time.Parse(time.RFC3339, startedAt); err == nil {
			op.StartedAt = parsed
		}
		if parsed, err := time.Parse(time.RFC3339, updatedAt); err == nil {
			op.UpdatedAt = parsed
		}
		ops = append(ops, op)
	}
	return ops, rows.Err()
}

func (s *syncState) recordSnapshotStart(ctx context.Context, snapshotID, scopeLabel string, unsupported []string) error {
	data, err := json.Marshal(unsupported)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO snapshot_runs (snapshot_id, scope_label, complete, unsupported_json, started_at)
		VALUES (?, ?, 0, ?, ?)
		ON CONFLICT(snapshot_id) DO UPDATE SET scope_label = excluded.scope_label, unsupported_json = excluded.unsupported_json
	`, snapshotID, scopeLabel, string(data), now)
	return err
}

func (s *syncState) recordSnapshotComplete(ctx context.Context, snapshotID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `
		UPDATE snapshot_runs SET complete = 1, completed_at = ? WHERE snapshot_id = ?
	`, now, snapshotID)
	return err
}

func (s *syncState) getPlacement(ctx context.Context, visibleID string) (placementMeta, bool, error) {
	var meta placementMeta
	var updated string
	err := s.db.QueryRowContext(ctx, `
		SELECT visible_id, slug, section, path, canonical_path, content_hash, body_content_hash, rendered_projection_hash, projection_format_version, frontmatter_flavor, links_flavor, etag, revision, last_seen_snapshot, updated_at,
			botlets_owner_handle, botlets_bot_slug, botlets_bot_local_name, botlets_bot_id, botlets_bot_handle, botlets_bot_agent_id, botlets_brain_container_id, botlets_brain_root_folder_id, botlets_brain_node_id
		FROM placements WHERE visible_id = ?
	`, visibleID).Scan(
		&meta.VisibleInstanceID,
		&meta.Slug,
		&meta.Section,
		&meta.Path,
		&meta.CanonicalPath,
		&meta.ContentHash,
		&meta.BodyContentHash,
		&meta.RenderedProjectionHash,
		&meta.ProjectionFormatVersion,
		&meta.FrontmatterFlavor,
		&meta.LinksFlavor,
		&meta.ETag,
		&meta.Revision,
		&meta.LastSeenSnapshot,
		&updated,
		&meta.BotletsOwnerHandle,
		&meta.BotletsBotSlug,
		&meta.BotletsBotLocalName,
		&meta.BotletsBotID,
		&meta.BotletsBotHandle,
		&meta.BotletsBotAgentID,
		&meta.BotletsBrainContainerID,
		&meta.BotletsBrainRootFolderID,
		&meta.BotletsBrainNodeID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return placementMeta{}, false, nil
	}
	if err != nil {
		return placementMeta{}, false, err
	}
	if parsed, err := time.Parse(time.RFC3339, updated); err == nil {
		meta.UpdatedAt = parsed
	}
	return meta, true, nil
}

func (s *syncState) upsertPlacement(ctx context.Context, meta placementMeta) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO placements (
			visible_id, slug, section, path, canonical_path, content_hash, body_content_hash, rendered_projection_hash, projection_format_version, frontmatter_flavor, links_flavor, etag, revision, last_seen_snapshot, updated_at,
			botlets_owner_handle, botlets_bot_slug, botlets_bot_local_name, botlets_bot_id, botlets_bot_handle, botlets_bot_agent_id, botlets_brain_container_id, botlets_brain_root_folder_id, botlets_brain_node_id
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(visible_id) DO UPDATE SET
			slug = excluded.slug,
			section = excluded.section,
			path = excluded.path,
			canonical_path = excluded.canonical_path,
			content_hash = excluded.content_hash,
			body_content_hash = excluded.body_content_hash,
			rendered_projection_hash = excluded.rendered_projection_hash,
			projection_format_version = excluded.projection_format_version,
			frontmatter_flavor = excluded.frontmatter_flavor,
			links_flavor = excluded.links_flavor,
			etag = excluded.etag,
			revision = excluded.revision,
			last_seen_snapshot = excluded.last_seen_snapshot,
			updated_at = excluded.updated_at,
			botlets_owner_handle = excluded.botlets_owner_handle,
			botlets_bot_slug = excluded.botlets_bot_slug,
			botlets_bot_local_name = excluded.botlets_bot_local_name,
			botlets_bot_id = excluded.botlets_bot_id,
			botlets_bot_handle = excluded.botlets_bot_handle,
			botlets_bot_agent_id = excluded.botlets_bot_agent_id,
			botlets_brain_container_id = excluded.botlets_brain_container_id,
			botlets_brain_root_folder_id = excluded.botlets_brain_root_folder_id,
			botlets_brain_node_id = excluded.botlets_brain_node_id
	`, meta.VisibleInstanceID, meta.Slug, meta.Section, meta.Path, meta.CanonicalPath, meta.ContentHash, meta.BodyContentHash, meta.RenderedProjectionHash, meta.ProjectionFormatVersion, meta.FrontmatterFlavor, meta.LinksFlavor, meta.ETag, meta.Revision, meta.LastSeenSnapshot, formatTime(meta.UpdatedAt), meta.BotletsOwnerHandle, meta.BotletsBotSlug, meta.BotletsBotLocalName, meta.BotletsBotID, meta.BotletsBotHandle, meta.BotletsBotAgentID, meta.BotletsBrainContainerID, meta.BotletsBrainRootFolderID, meta.BotletsBrainNodeID)
	return err
}

func (s *syncState) listPlacements(ctx context.Context) ([]placementMeta, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT visible_id, slug, section, path, canonical_path, content_hash, body_content_hash, rendered_projection_hash, projection_format_version, frontmatter_flavor, links_flavor, etag, revision, last_seen_snapshot, updated_at,
			botlets_owner_handle, botlets_bot_slug, botlets_bot_local_name, botlets_bot_id, botlets_bot_handle, botlets_bot_agent_id, botlets_brain_container_id, botlets_brain_root_folder_id, botlets_brain_node_id
		FROM placements ORDER BY visible_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []placementMeta
	for rows.Next() {
		var meta placementMeta
		var updated string
		if err := rows.Scan(
			&meta.VisibleInstanceID,
			&meta.Slug,
			&meta.Section,
			&meta.Path,
			&meta.CanonicalPath,
			&meta.ContentHash,
			&meta.BodyContentHash,
			&meta.RenderedProjectionHash,
			&meta.ProjectionFormatVersion,
			&meta.FrontmatterFlavor,
			&meta.LinksFlavor,
			&meta.ETag,
			&meta.Revision,
			&meta.LastSeenSnapshot,
			&updated,
			&meta.BotletsOwnerHandle,
			&meta.BotletsBotSlug,
			&meta.BotletsBotLocalName,
			&meta.BotletsBotID,
			&meta.BotletsBotHandle,
			&meta.BotletsBotAgentID,
			&meta.BotletsBrainContainerID,
			&meta.BotletsBrainRootFolderID,
			&meta.BotletsBrainNodeID,
		); err != nil {
			return nil, err
		}
		if parsed, err := time.Parse(time.RFC3339, updated); err == nil {
			meta.UpdatedAt = parsed
		}
		out = append(out, meta)
	}
	return out, rows.Err()
}

func (s *syncState) deletePlacement(ctx context.Context, visibleID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM placements WHERE visible_id = ?`, visibleID)
	return err
}

func (s *syncState) resetProjectionState(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DELETE FROM placements`,
		`DELETE FROM retries`,
		`DELETE FROM sync_ops`,
		`DELETE FROM snapshot_runs`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *syncState) countPlacements(ctx context.Context) int {
	var count int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM placements`).Scan(&count)
	return count
}

func (s *syncState) insertRecovery(ctx context.Context, item recoveryItem) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO recoveries (id, visible_id, slug, original_path, artifact_path, reason, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, item.ID, item.VisibleID, item.Slug, item.OriginalPath, item.ArtifactPath, item.Reason, item.Status, formatTime(item.CreatedAt))
	return err
}

func (s *syncState) listRecoveries(ctx context.Context, includeDiscarded bool) ([]recoveryItem, error) {
	query := `
		SELECT id, visible_id, slug, original_path, artifact_path, reason, status, created_at
		FROM recoveries
	`
	if !includeDiscarded {
		query += ` WHERE status = 'pending'`
	}
	query += ` ORDER BY created_at, id`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recoveryItem
	for rows.Next() {
		item, err := scanRecovery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *syncState) findRecovery(ctx context.Context, target string) (recoveryItem, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, visible_id, slug, original_path, artifact_path, reason, status, created_at
		FROM recoveries
		WHERE id = ? OR artifact_path = ? OR original_path = ?
		ORDER BY created_at DESC
		LIMIT 1
	`, target, target, target)
	return scanRecovery(row)
}

func (s *syncState) setRecoveryStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recoveries SET status = ? WHERE id = ?`, status, id)
	return err
}

type recoveryScanner interface {
	Scan(dest ...any) error
}

func scanRecovery(row recoveryScanner) (recoveryItem, error) {
	var item recoveryItem
	var created string
	if err := row.Scan(&item.ID, &item.VisibleID, &item.Slug, &item.OriginalPath, &item.ArtifactPath, &item.Reason, &item.Status, &created); err != nil {
		return recoveryItem{}, err
	}
	if parsed, err := time.Parse(time.RFC3339, created); err == nil {
		item.CreatedAt = parsed
	}
	return item, nil
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		value = time.Now().UTC()
	}
	return value.UTC().Format(time.RFC3339)
}
