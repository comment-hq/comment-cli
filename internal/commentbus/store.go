package commentbus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const SQLiteSchemaVersion = 5

var ErrStoreNotInitialized = errors.New("comment bus sqlite history is not initialized")

var SQLiteSchemaV1 = []string{
	`CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		source TEXT NOT NULL,
		kind TEXT NOT NULL,
		thread_id TEXT,
		sender TEXT NOT NULL,
		profile TEXT NOT NULL,
		bot_name TEXT NOT NULL,
		body_format TEXT NOT NULL,
		body_content TEXT NOT NULL,
		refs_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		retention_bucket TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS message_recipients (
		message_id TEXT NOT NULL,
		profile TEXT NOT NULL,
		handle TEXT NOT NULL,
		delivery_state TEXT NOT NULL,
		claim_holder TEXT,
		lease_expires_at TEXT,
		session_id TEXT,
		session_scope_type TEXT,
		session_scope_id TEXT,
		session_generation TEXT,
		read_at TEXT,
		PRIMARY KEY (message_id, profile)
	)`,
	`CREATE TABLE IF NOT EXISTS events (
		id TEXT PRIMARY KEY,
		message_id TEXT,
		profile TEXT,
		event_type TEXT NOT NULL,
		redacted_json TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS sessions (
		session_id TEXT PRIMARY KEY,
		profile TEXT NOT NULL,
		bot_name TEXT NOT NULL,
		scope_type TEXT NOT NULL,
		scope_id TEXT NOT NULL,
		tmux_session TEXT NOT NULL,
		pane_target TEXT NOT NULL,
		generation TEXT NOT NULL,
		runtime TEXT NOT NULL,
		state TEXT NOT NULL,
		last_nudge_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS outbox (
		idempotency_key TEXT PRIMARY KEY,
		sender_profile TEXT NOT NULL,
		recipient_profiles_json TEXT NOT NULL,
		state TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS transient_runtimes (
		run_id TEXT PRIMARY KEY,
		profile TEXT NOT NULL,
		role TEXT NOT NULL,
		bot_name TEXT NOT NULL,
		session_name TEXT NOT NULL,
		pane_target TEXT NOT NULL,
		runtime TEXT NOT NULL,
		runtime_command_json TEXT NOT NULL,
		runtime_command_path TEXT NOT NULL,
		comment_command_path TEXT NOT NULL,
		output_log_path TEXT NOT NULL,
		runtime_path TEXT NOT NULL,
		cwd TEXT NOT NULL,
		state TEXT NOT NULL,
		started_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_transient_runtimes_profile_role ON transient_runtimes(profile, role)`,
}

var SQLiteSchemaV2 = []string{
	`CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		source TEXT NOT NULL,
		kind TEXT NOT NULL,
		thread_id TEXT,
		sender TEXT NOT NULL,
		profile TEXT NOT NULL,
		bot_name TEXT NOT NULL,
		body_format TEXT NOT NULL,
		body_content TEXT NOT NULL,
		refs_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		retention_bucket TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS message_recipients (
		message_id TEXT NOT NULL,
		profile TEXT NOT NULL,
		handle TEXT NOT NULL,
		delivery_state TEXT NOT NULL,
		claim_holder TEXT,
		lease_expires_at TEXT,
		session_id TEXT,
		session_scope_type TEXT,
		session_scope_id TEXT,
		session_generation TEXT,
		read_at TEXT,
		PRIMARY KEY (message_id, profile)
	)`,
	`CREATE TABLE IF NOT EXISTS events (
		id TEXT PRIMARY KEY,
		message_id TEXT,
		profile TEXT,
		event_type TEXT NOT NULL,
		redacted_json TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS sessions (
		session_id TEXT PRIMARY KEY,
		profile TEXT NOT NULL,
		bot_name TEXT NOT NULL,
		scope_type TEXT NOT NULL,
		scope_id TEXT NOT NULL,
		tmux_session TEXT NOT NULL,
		pane_target TEXT NOT NULL,
		generation TEXT NOT NULL,
		runtime TEXT NOT NULL,
		state TEXT NOT NULL,
		last_nudge_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS outbox (
		idempotency_key TEXT PRIMARY KEY,
		sender_profile TEXT NOT NULL,
		recipient_profiles_json TEXT NOT NULL,
		state TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS transient_runtimes (
		run_id TEXT PRIMARY KEY,
		host TEXT NOT NULL DEFAULT 'tmux',
		profile TEXT NOT NULL,
		role TEXT NOT NULL,
		bot_name TEXT NOT NULL,
		session_name TEXT NOT NULL,
		pane_target TEXT NOT NULL,
		runtime TEXT NOT NULL,
		runtime_command_json TEXT NOT NULL,
		runtime_command_path TEXT NOT NULL,
		comment_command_path TEXT NOT NULL,
		output_log_path TEXT NOT NULL,
		runtime_path TEXT NOT NULL,
		cwd TEXT NOT NULL,
		state TEXT NOT NULL,
		started_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_transient_runtimes_profile_role ON transient_runtimes(profile, role)`,
}

var SQLiteSchemaV3 = []string{
	`CREATE TABLE IF NOT EXISTS messages (
		id TEXT PRIMARY KEY,
		source TEXT NOT NULL,
		kind TEXT NOT NULL,
		thread_id TEXT,
		sender TEXT NOT NULL,
		profile TEXT NOT NULL,
		bot_name TEXT NOT NULL,
		bot_id TEXT NOT NULL DEFAULT '',
		bot_agent_id TEXT NOT NULL DEFAULT '',
		body_format TEXT NOT NULL,
		body_content TEXT NOT NULL,
		refs_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		retention_bucket TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS message_recipients (
		message_id TEXT NOT NULL,
		profile TEXT NOT NULL,
		handle TEXT NOT NULL,
		bot_id TEXT NOT NULL DEFAULT '',
		bot_agent_id TEXT NOT NULL DEFAULT '',
		delivery_state TEXT NOT NULL,
		claim_holder TEXT,
		lease_expires_at TEXT,
		session_id TEXT,
		session_scope_type TEXT,
		session_scope_id TEXT,
		session_generation TEXT,
		read_at TEXT,
		PRIMARY KEY (message_id, profile)
	)`,
	`CREATE TABLE IF NOT EXISTS events (
		id TEXT PRIMARY KEY,
		message_id TEXT,
		profile TEXT,
		event_type TEXT NOT NULL,
		redacted_json TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS sessions (
		session_id TEXT PRIMARY KEY,
		profile TEXT NOT NULL,
		bot_name TEXT NOT NULL,
		bot_id TEXT NOT NULL DEFAULT '',
		bot_agent_id TEXT NOT NULL DEFAULT '',
		scope_type TEXT NOT NULL,
		scope_id TEXT NOT NULL,
		tmux_session TEXT NOT NULL,
		pane_target TEXT NOT NULL,
		generation TEXT NOT NULL,
		runtime TEXT NOT NULL,
		state TEXT NOT NULL,
		last_nudge_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS outbox (
		idempotency_key TEXT PRIMARY KEY,
		sender_profile TEXT NOT NULL,
		sender_bot_id TEXT NOT NULL DEFAULT '',
		sender_bot_agent_id TEXT NOT NULL DEFAULT '',
		recipient_profiles_json TEXT NOT NULL,
		state TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS transient_runtimes (
		run_id TEXT PRIMARY KEY,
		host TEXT NOT NULL DEFAULT 'tmux',
		bmux_binary TEXT NOT NULL DEFAULT '',
		profile TEXT NOT NULL,
		role TEXT NOT NULL,
		bot_name TEXT NOT NULL,
		bot_id TEXT NOT NULL DEFAULT '',
		bot_agent_id TEXT NOT NULL DEFAULT '',
		session_name TEXT NOT NULL,
		pane_target TEXT NOT NULL,
		runtime TEXT NOT NULL,
		runtime_command_json TEXT NOT NULL,
		runtime_command_path TEXT NOT NULL,
		comment_command_path TEXT NOT NULL,
		output_log_path TEXT NOT NULL,
		runtime_path TEXT NOT NULL,
		cwd TEXT NOT NULL,
		state TEXT NOT NULL,
		started_at TEXT NOT NULL,
		runtime_launch_mode TEXT NOT NULL DEFAULT 'path'
	)`,
	`CREATE INDEX IF NOT EXISTS idx_transient_runtimes_profile_role ON transient_runtimes(profile, role)`,
	`CREATE INDEX IF NOT EXISTS idx_messages_bot_identity ON messages(bot_id, bot_agent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_message_recipients_bot_identity ON message_recipients(bot_id, bot_agent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_sessions_bot_identity ON sessions(bot_id, bot_agent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_outbox_sender_bot_identity ON outbox(sender_bot_id, sender_bot_agent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_transient_runtimes_bot_identity ON transient_runtimes(bot_id, bot_agent_id)`,
}

type Store struct {
	db   *sql.DB
	path string
}

type RepairAction struct {
	Action    string `json:"action"`
	MessageID string `json:"message_id,omitempty"`
	OpID      string `json:"op_id,omitempty"`
	FromState string `json:"from_state,omitempty"`
	ToState   string `json:"to_state,omitempty"`
	Reason    string `json:"reason"`
}

func OpenStore(ctx context.Context, paths Paths) (*Store, error) {
	if err := EnsureBaseDirs(paths); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(paths.History), 0o700); err != nil {
		return nil, err
	}
	db, err := openSQLite(paths.History)
	if err != nil {
		return nil, err
	}
	store := &Store{db: db, path: paths.History}
	if err := store.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(paths.History, 0o600); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func OpenExistingStore(ctx context.Context, paths Paths) (*Store, error) {
	if _, err := os.Stat(paths.History); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrStoreNotInitialized
		}
		return nil, err
	}
	db, err := openSQLite(paths.History)
	if err != nil {
		return nil, err
	}
	store := &Store{db: db, path: paths.History}
	if err := store.migrateExisting(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

func (s *Store) TableNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

func (s *Store) RepairDryRun(ctx context.Context) ([]RepairAction, error) {
	if _, err := s.SchemaVersion(ctx); err != nil {
		return nil, err
	}
	return []RepairAction{}, nil
}

func (s *Store) BackfillBotIdentityColumns(ctx context.Context, bots map[string]BotRegistryEntry) error {
	if len(bots) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, bot := range bots {
		botID := bot.BotID
		botAgentID := botAgentID(bot)
		if botID == "" && botAgentID == "" {
			continue
		}
		for _, profile := range botRegistryHandleLabels(bot) {
			if profile == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE outbox
				SET sender_bot_id = CASE WHEN sender_bot_id = '' THEN ? ELSE sender_bot_id END,
					sender_bot_agent_id = CASE WHEN sender_bot_agent_id = '' THEN ? ELSE sender_bot_agent_id END
				WHERE sender_profile = ? AND (sender_bot_id = '' OR sender_bot_agent_id = '')`,
				botID,
				botAgentID,
				profile,
			); err != nil {
				return err
			}
			for _, name := range botRegistryNameLabels(bot) {
				if name == "" {
					continue
				}
				if _, err := tx.ExecContext(ctx, `UPDATE messages
					SET bot_id = CASE WHEN bot_id = '' THEN ? ELSE bot_id END,
						bot_agent_id = CASE WHEN bot_agent_id = '' THEN ? ELSE bot_agent_id END
					WHERE profile = ? AND bot_name = ? AND (bot_id = '' OR bot_agent_id = '')`,
					botID,
					botAgentID,
					profile,
					name,
				); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE message_recipients
					SET bot_id = CASE WHEN bot_id = '' THEN ? ELSE bot_id END,
						bot_agent_id = CASE WHEN bot_agent_id = '' THEN ? ELSE bot_agent_id END
					WHERE profile = ?
						AND message_id IN (SELECT id FROM messages WHERE profile = ? AND bot_name = ?)
						AND (bot_id = '' OR bot_agent_id = '')`,
					botID,
					botAgentID,
					profile,
					profile,
					name,
				); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE sessions
					SET bot_id = CASE WHEN bot_id = '' THEN ? ELSE bot_id END,
						bot_agent_id = CASE WHEN bot_agent_id = '' THEN ? ELSE bot_agent_id END
					WHERE profile = ? AND bot_name = ? AND (bot_id = '' OR bot_agent_id = '')`,
					botID,
					botAgentID,
					profile,
					name,
				); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE transient_runtimes
					SET bot_id = CASE WHEN bot_id = '' THEN ? ELSE bot_id END,
						bot_agent_id = CASE WHEN bot_agent_id = '' THEN ? ELSE bot_agent_id END
					WHERE profile = ? AND bot_name = ? AND (bot_id = '' OR bot_agent_id = '')`,
					botID,
					botAgentID,
					profile,
					name,
				); err != nil {
					return err
				}
			}
		}
	}
	return tx.Commit()
}

func (s *Store) init(ctx context.Context) error {
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if version > SQLiteSchemaVersion {
		return validateSupportedSchemaVersion(version)
	}
	if version > 0 {
		if err := s.migrate(ctx, version); err != nil {
			return err
		}
		version = SQLiteSchemaVersion
	}
	if err := validateSupportedSchemaVersion(version); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA synchronous = NORMAL"); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, statement := range SQLiteSchemaV3 {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if version == 0 {
		if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 5`); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) migrateExisting(ctx context.Context) error {
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if version == 0 {
		return ErrStoreNotInitialized
	}
	if version > SQLiteSchemaVersion {
		return validateSupportedSchemaVersion(version)
	}
	return s.migrate(ctx, version)
}

func (s *Store) migrate(ctx context.Context, version int) error {
	if version == SQLiteSchemaVersion {
		return nil
	}
	if version != 1 && version != 2 && version != 3 && version != 4 {
		return validateSupportedSchemaVersion(version)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Migrations are cumulative and step the local `version` forward so a v1
	// store applies 1->2->3->4->5 in one transaction.
	if version == 1 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE transient_runtimes ADD COLUMN host TEXT NOT NULL DEFAULT 'tmux'`); err != nil {
			return err
		}
		version = 2
	}
	if version == 2 {
		for _, statement := range []string{
			`ALTER TABLE messages ADD COLUMN bot_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE messages ADD COLUMN bot_agent_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE message_recipients ADD COLUMN bot_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE message_recipients ADD COLUMN bot_agent_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE sessions ADD COLUMN bot_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE sessions ADD COLUMN bot_agent_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE outbox ADD COLUMN sender_bot_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE outbox ADD COLUMN sender_bot_agent_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE transient_runtimes ADD COLUMN bot_id TEXT NOT NULL DEFAULT ''`,
			`ALTER TABLE transient_runtimes ADD COLUMN bot_agent_id TEXT NOT NULL DEFAULT ''`,
			`CREATE INDEX IF NOT EXISTS idx_messages_bot_identity ON messages(bot_id, bot_agent_id)`,
			`CREATE INDEX IF NOT EXISTS idx_message_recipients_bot_identity ON message_recipients(bot_id, bot_agent_id)`,
			`CREATE INDEX IF NOT EXISTS idx_sessions_bot_identity ON sessions(bot_id, bot_agent_id)`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_sender_bot_identity ON outbox(sender_bot_id, sender_bot_agent_id)`,
			`CREATE INDEX IF NOT EXISTS idx_transient_runtimes_bot_identity ON transient_runtimes(bot_id, bot_agent_id)`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		version = 3
	}
	if version == 3 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE transient_runtimes ADD COLUMN runtime_launch_mode TEXT NOT NULL DEFAULT 'path'`); err != nil {
			return err
		}
		version = 4
	}
	if version == 4 {
		if _, err := tx.ExecContext(ctx, `ALTER TABLE transient_runtimes ADD COLUMN bmux_binary TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
		version = 5
	}
	if version != SQLiteSchemaVersion {
		return validateSupportedSchemaVersion(version)
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 5`); err != nil {
		return err
	}
	return tx.Commit()
}

func validateSupportedSchemaVersion(version int) error {
	if version > SQLiteSchemaVersion {
		return fmt.Errorf("comment bus sqlite schema version %d is newer than supported version %d", version, SQLiteSchemaVersion)
	}
	if version > 0 && version < SQLiteSchemaVersion {
		return fmt.Errorf("comment bus sqlite schema version %d requires migration to version %d", version, SQLiteSchemaVersion)
	}
	return nil
}
