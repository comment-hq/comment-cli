package commentbus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOpenStoreInitializesSQLiteHistory(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != SQLiteSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, SQLiteSchemaVersion)
	}
	tables, err := store.TableNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"messages", "message_recipients", "events", "sessions", "outbox", "transient_runtimes"} {
		if !slices.Contains(tables, table) {
			t.Fatalf("missing table %s in %v", table, tables)
		}
	}
	info, err := os.Stat(paths.History)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("history sqlite mode = %o, want 0600", got)
	}
	actions, err := store.RepairDryRun(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("fresh store repair actions = %v, want none", actions)
	}
}

func TestWaitMessageSummariesSourceFilterFindsCloudBeyondLocalWindow(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	for i := 0; i < 55; i++ {
		if _, err := store.InsertLocalMessages(ctx, LocalMessageSend{
			SenderProfile: "max.sender",
			SenderBotName: "sender",
			Recipients:    []LocalMessageRecipient{{Profile: "max.reviewer", BotName: "reviewer"}},
			Body:          MessageBody{Format: "markdown", Content: fmt.Sprintf("local %d", i)},
			Now:           now.Add(time.Duration(i) * time.Millisecond),
		}); err != nil {
			t.Fatal(err)
		}
	}
	cloudID, err := GenerateLocalID("msg", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.InsertCloudNotificationMessage(ctx, CloudNotificationMessage{
		ID:             cloudID,
		Profile:        "max.reviewer",
		BotName:        "reviewer",
		Kind:           "doc.mention",
		From:           "@max.sender",
		Body:           MessageBody{Format: "markdown", Content: "cloud"},
		Refs:           map[string]any{"doc_slug": "abc123"},
		NotificationID: "ntf_sourcefilter1234567890",
		CreatedAt:      busTime(now.Add(time.Hour)),
		LeaseExpiresAt: busTime(now.Add(time.Hour + time.Minute)),
		Now:            now,
	}); err != nil {
		t.Fatal(err)
	}

	unfiltered, err := store.WaitMessageSummaries(ctx, MessageListFilter{Profile: "max.reviewer", BotName: "reviewer"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfiltered) != 50 {
		t.Fatalf("unfiltered summaries = %d, want 50", len(unfiltered))
	}
	for _, summary := range unfiltered {
		if summary.Source == "comment.io" {
			t.Fatalf("unfiltered window unexpectedly included cloud summary: %+v", summary)
		}
	}
	cloudOnly, err := store.WaitMessageSummaries(ctx, MessageListFilter{Profile: "max.reviewer", BotName: "reviewer", Source: "comment.io"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(cloudOnly) != 1 || cloudOnly[0].MessageID != cloudID || cloudOnly[0].Source != "comment.io" {
		t.Fatalf("cloud summaries = %+v, want only %s", cloudOnly, cloudID)
	}
}

func TestStorePersistsTransientRuntimes(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	record := TransientRuntimeRecord{
		RunID:              "sess_transientruntime1234",
		BmuxBinary:         "/usr/local/bin/bmux",
		Profile:            "max.reviewer",
		Role:               RuntimeRoleMain,
		BotName:            "reviewer",
		BotID:              "ag_bot",
		BotAgentID:         "ag_bot_agent",
		SessionName:        "comment-run-reviewer-abc123",
		PaneTarget:         "%1",
		Runtime:            "claude",
		RuntimeCommand:     []string{"claude", "--model", "test"},
		RuntimeCommandPath: "/usr/local/bin/claude",
		CommentCommandPath: "/usr/local/bin/comment",
		OutputLogPath:      "/tmp/runtime.log",
		RuntimePath:        "/usr/local/bin/claude",
		CWD:                "/tmp",
		State:              "alive",
		StartedAt:          "2026-05-19T00:00:00Z",
	}
	if err := store.PutTransientRuntime(ctx, record); err != nil {
		t.Fatal(err)
	}
	records, err := store.ListTransientRuntimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || !slices.Equal(records[0].RuntimeCommand, record.RuntimeCommand) || records[0].RunID != record.RunID || records[0].Role != RuntimeRoleMain || records[0].BotID != record.BotID || records[0].BotAgentID != record.BotAgentID || records[0].BmuxBinary != record.BmuxBinary {
		t.Fatalf("transient runtime records = %+v", records)
	}
	if err := store.DeleteTransientRuntime(ctx, record.RunID); err != nil {
		t.Fatal(err)
	}
	records, err = store.ListTransientRuntimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("transient runtime records after delete = %+v", records)
	}
}

func TestOpenStoreDoesNotDowngradeFutureSchema(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.History), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", paths.History)
	if err != nil {
		t.Fatal(err)
	}
	futureVersion := SQLiteSchemaVersion + 1
	if _, err := db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", futureVersion)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(ctx, paths)
	if err == nil {
		_ = store.Close()
		t.Fatal("OpenStore succeeded for future schema version")
	}
	if !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("OpenStore error = %q, want newer-than-supported error", err)
	}

	db, err = sql.Open("sqlite", paths.History)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != futureVersion {
		t.Fatalf("schema version after failed open = %d, want %d", version, futureVersion)
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(journalMode) != "delete" {
		t.Fatalf("journal mode after failed open = %q, want delete", journalMode)
	}
}

func TestOpenExistingStoreMigratesV1TransientRuntimeHost(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.History), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", paths.History)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range SQLiteSchemaV1 {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO transient_runtimes (
		run_id,
		profile,
		role,
		bot_name,
		session_name,
		pane_target,
		runtime,
		runtime_command_json,
		runtime_command_path,
		comment_command_path,
		output_log_path,
		runtime_path,
		cwd,
		state,
		started_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"sess_transientruntime1234",
		"max.reviewer",
		RuntimeRoleMain,
		"reviewer",
		"comment-run-reviewer-abc123",
		"%1",
		"claude",
		`["claude","--model","test"]`,
		"/usr/local/bin/claude",
		"/usr/local/bin/comment",
		"/tmp/runtime.log",
		"/usr/local/bin/claude",
		"/tmp",
		"alive",
		"2026-05-19T00:00:00Z",
	); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExistingStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != SQLiteSchemaVersion {
		t.Fatalf("schema version after migration = %d, want %d", version, SQLiteSchemaVersion)
	}
	records, err := store.ListTransientRuntimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("transient runtime records after migration = %+v, want one record", records)
	}
	if records[0].Host != SessionHostTmux {
		t.Fatalf("transient runtime host after migration = %q, want %q", records[0].Host, SessionHostTmux)
	}
	if records[0].RuntimeLaunchMode != RuntimeLaunchModePath {
		t.Fatalf("transient runtime launch mode after migration = %q, want %q", records[0].RuntimeLaunchMode, RuntimeLaunchModePath)
	}
	if records[0].BmuxBinary != "" {
		t.Fatalf("transient runtime bmux binary after migration = %q, want empty", records[0].BmuxBinary)
	}
}

func TestStorePersistsShellModeTransientRuntime(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Shell-mode records carry no resolved path fields — the runtime name is
	// resolved through the user's login shell at launch. They must round-trip
	// through the store without being rejected by the validators.
	record := TransientRuntimeRecord{
		RunID:              "sess_shellmoderuntime1234",
		Profile:            "max.reviewer",
		Role:               RuntimeRoleMain,
		BotName:            "reviewer",
		SessionName:        "comment-run-reviewer-shell1",
		PaneTarget:         "%1",
		Runtime:            "codex",
		RuntimeCommand:     []string{"codex"},
		RuntimeCommandPath: "",
		CommentCommandPath: "/usr/local/bin/comment",
		OutputLogPath:      "/tmp/runtime.log",
		RuntimePath:        "",
		CWD:                "/tmp",
		State:              "alive",
		StartedAt:          "2026-05-19T00:00:00Z",
		RuntimeLaunchMode:  RuntimeLaunchModeShell,
	}
	if err := store.PutTransientRuntime(ctx, record); err != nil {
		t.Fatalf("PutTransientRuntime(shell mode) = %v, want nil", err)
	}
	records, err := store.ListTransientRuntimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("transient runtime records = %+v, want one", records)
	}
	if records[0].RuntimeLaunchMode != RuntimeLaunchModeShell {
		t.Fatalf("launch mode = %q, want %q", records[0].RuntimeLaunchMode, RuntimeLaunchModeShell)
	}
	if records[0].RuntimePath != "" || records[0].RuntimeCommandPath != "" {
		t.Fatalf("shell-mode record path fields = %q/%q, want empty", records[0].RuntimePath, records[0].RuntimeCommandPath)
	}

	// A shell-mode record that carries a path field is malformed and must be rejected.
	bad := record
	bad.RunID = "sess_shellmodebadpath12345"
	bad.RuntimePath = "/usr/local/bin/codex"
	if err := store.PutTransientRuntime(ctx, bad); err == nil {
		t.Fatal("PutTransientRuntime(shell mode with path) = nil, want rejection")
	}
}

func TestOpenExistingStoreMigratesV2StableBotIdentityColumns(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.History), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", paths.History)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range SQLiteSchemaV2 {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO messages (
		id, source, kind, thread_id, sender, profile, bot_name, body_format, body_content, refs_json, created_at, retention_bucket
	) VALUES (?, 'local', 'message', NULL, '@max.sender', 'max.reviewer', 'reviewer', 'markdown', 'hello', '{}', '2026-05-19T00:00:00.000000000Z', 'default')`, "msg_legacyidentity123456"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO message_recipients (
		message_id, profile, handle, delivery_state
	) VALUES (?, 'max.reviewer', 'max.reviewer', 'unclaimed')`, "msg_legacyidentity123456"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sessions (
		session_id, profile, bot_name, scope_type, scope_id, tmux_session, pane_target, generation, runtime, state, last_nudge_json, created_at, updated_at
	) VALUES ('sess_legacyidentity123456', 'max.reviewer', 'reviewer', 'profile', 'max.reviewer', 'comment-reviewer-abc123', '%1', 'gen_legacyidentity1234', 'claude', 'alive', '{}', '2026-05-19T00:00:00.000000000Z', '2026-05-19T00:00:00.000000000Z')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO outbox (
		idempotency_key, sender_profile, recipient_profiles_json, state, created_at, updated_at
	) VALUES ('op_legacyidentity123456', 'max.reviewer', '{}', 'done', '2026-05-19T00:00:00.000000000Z', '2026-05-19T00:00:00.000000000Z')`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO transient_runtimes (
		run_id, host, profile, role, bot_name, session_name, pane_target, runtime, runtime_command_json,
		runtime_command_path, comment_command_path, output_log_path, runtime_path, cwd, state, started_at
	) VALUES (
		'sess_legacyruntime1234567', 'tmux', 'max.reviewer', ?, 'reviewer', 'comment-run-reviewer-abc123', '%1', 'claude',
		'["claude","--model","test"]', '/usr/local/bin/claude', '/usr/local/bin/comment', '/tmp/runtime.log',
		'/usr/local/bin/claude', '/tmp', 'alive', '2026-05-19T00:00:00Z'
	)`, RuntimeRoleMain); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExistingStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != SQLiteSchemaVersion {
		t.Fatalf("schema version after migration = %d, want %d", version, SQLiteSchemaVersion)
	}
	assertEmpty := func(query string, args ...any) {
		t.Helper()
		var first, second string
		if err := store.db.QueryRowContext(ctx, query, args...).Scan(&first, &second); err != nil {
			t.Fatal(err)
		}
		if first != "" || second != "" {
			t.Fatalf("migrated identity = %q %q, want empty compatibility defaults", first, second)
		}
	}
	assertEmpty(`SELECT bot_id, bot_agent_id FROM messages WHERE id = ?`, "msg_legacyidentity123456")
	assertEmpty(`SELECT bot_id, bot_agent_id FROM message_recipients WHERE message_id = ?`, "msg_legacyidentity123456")
	assertEmpty(`SELECT bot_id, bot_agent_id FROM sessions WHERE session_id = ?`, "sess_legacyidentity123456")
	assertEmpty(`SELECT sender_bot_id, sender_bot_agent_id FROM outbox WHERE idempotency_key = ?`, "op_legacyidentity123456")
	records, err := store.ListTransientRuntimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].BotID != "" || records[0].BotAgentID != "" || records[0].BmuxBinary != "" {
		t.Fatalf("transient runtime records after migration = %+v", records)
	}
}

func TestOpenExistingStoreMigratesV4TransientRuntimeBmuxBinary(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := EnsureBaseDirs(paths); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.History), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", paths.History)
	if err != nil {
		t.Fatal(err)
	}
	removedBmuxColumn := false
	for _, statement := range SQLiteSchemaV3 {
		if strings.Contains(statement, "bmux_binary") {
			withoutBmux := strings.Replace(statement, "\n\t\tbmux_binary TEXT NOT NULL DEFAULT '',", "", 1)
			if withoutBmux == statement {
				_ = db.Close()
				t.Fatal("test failed to remove bmux_binary from v4 schema fixture")
			}
			statement = withoutBmux
			removedBmuxColumn = true
		}
		if _, err := db.ExecContext(ctx, statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if !removedBmuxColumn {
		_ = db.Close()
		t.Fatal("test fixture did not see bmux_binary in latest schema")
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO transient_runtimes (
		run_id, host, profile, role, bot_name, bot_id, bot_agent_id, session_name, pane_target, runtime, runtime_command_json,
		runtime_command_path, comment_command_path, output_log_path, runtime_path, cwd, state, started_at, runtime_launch_mode
	) VALUES (
		'sess_legacybmuxpin1234567', 'tmux', 'max.reviewer', ?, 'reviewer', '', '', 'comment-run-reviewer-abc123', '%1', 'claude',
		'["claude","--model","test"]', '/usr/local/bin/claude', '/usr/local/bin/comment', '/tmp/runtime.log',
		'/usr/local/bin/claude', '/tmp', 'alive', '2026-05-19T00:00:00Z', 'path'
	)`, RuntimeRoleMain); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA user_version = 4"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExistingStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if version != SQLiteSchemaVersion {
		t.Fatalf("schema version after migration = %d, want %d", version, SQLiteSchemaVersion)
	}
	records, err := store.ListTransientRuntimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].BmuxBinary != "" {
		t.Fatalf("transient runtime records after v4 migration = %+v", records)
	}
}

func TestOpenExistingStoreTreatsVersionZeroAsNotInitialized(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.History), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", paths.History)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenExistingStore(ctx, paths)
	if err == nil {
		_ = store.Close()
		t.Fatal("OpenExistingStore succeeded for user_version 0")
	}
	if !errors.Is(err, ErrStoreNotInitialized) {
		t.Fatalf("OpenExistingStore error = %v, want ErrStoreNotInitialized", err)
	}
}

func TestStoreBackfillsBotIdentityColumnsFromRegistry(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	send, err := store.InsertLocalMessages(ctx, LocalMessageSend{
		SenderProfile:  "max.old-reviewer",
		SenderBotName:  "old-reviewer",
		Recipients:     []LocalMessageRecipient{{Profile: "max.old-reviewer", BotName: "old-reviewer"}},
		Body:           MessageBody{Format: "markdown", Content: "ping"},
		IdempotencyKey: "op_backfillidentity1234",
		Now:            time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutTransientRuntime(ctx, TransientRuntimeRecord{
		RunID:              "sess_backfillidentity1234",
		Profile:            "max.old-reviewer",
		Role:               RuntimeRoleMain,
		BotName:            "old-reviewer",
		SessionName:        "comment-run-old-reviewer-abc123",
		PaneTarget:         "%1",
		Runtime:            "claude",
		RuntimeCommand:     []string{"claude", "--model", "test"},
		RuntimeCommandPath: "/usr/local/bin/claude",
		CommentCommandPath: "/usr/local/bin/comment",
		OutputLogPath:      "/tmp/runtime.log",
		RuntimePath:        "/usr/local/bin/claude",
		CWD:                "/tmp",
		State:              "alive",
		StartedAt:          "2026-05-19T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.BackfillBotIdentityColumns(ctx, map[string]BotRegistryEntry{
		"reviewer": {
			Name:          "reviewer",
			BotID:         "ag_bot",
			Handle:        "max.reviewer",
			SlugAliases:   []string{"old-reviewer"},
			HandleAliases: []string{"max.old-reviewer"},
			BrainRef:      &BotBrainRef{BotAgentID: "ag_bot_agent"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	assertIdentity := func(query string, args ...any) {
		t.Helper()
		var botID, botAgentID string
		if err := store.db.QueryRowContext(ctx, query, args...).Scan(&botID, &botAgentID); err != nil {
			t.Fatal(err)
		}
		if botID != "ag_bot" || botAgentID != "ag_bot_agent" {
			t.Fatalf("identity = %q %q, want ag_bot/ag_bot_agent", botID, botAgentID)
		}
	}
	assertIdentity(`SELECT bot_id, bot_agent_id FROM messages WHERE id = ?`, send.Messages[0].ID)
	assertIdentity(`SELECT bot_id, bot_agent_id FROM message_recipients WHERE message_id = ?`, send.Messages[0].ID)
	assertIdentity(`SELECT sender_bot_id, sender_bot_agent_id FROM outbox WHERE idempotency_key = ?`, "op_backfillidentity1234")
	records, err := store.ListTransientRuntimes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].BotID != "ag_bot" || records[0].BotAgentID != "ag_bot_agent" {
		t.Fatalf("transient runtime after backfill = %+v", records)
	}
}

func TestInsertLocalMessagesReplaysOutboxByStableSenderIdentity(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	input := LocalMessageSend{
		SenderProfile:    "max.old-reviewer",
		SenderBotName:    "old-reviewer",
		SenderBotID:      "ag_sender_bot",
		SenderBotAgentID: "ag_sender_agent",
		Recipients: []LocalMessageRecipient{{
			Profile:    "max.old-runner",
			BotName:    "old-runner",
			BotID:      "ag_recipient_bot",
			BotAgentID: "ag_recipient_agent",
		}},
		Body:           MessageBody{Format: "markdown", Content: "same body"},
		Refs:           map[string]any{"kind": "test"},
		IdempotencyKey: "op_stableidentityreplay",
		Now:            time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC),
	}
	first, err := store.InsertLocalMessages(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	input.SenderProfile = "max.reviewer"
	input.SenderBotName = "reviewer"
	input.Recipients[0].Profile = "max.runner"
	input.Recipients[0].BotName = "runner"
	second, err := store.InsertLocalMessages(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || len(second.Messages) != 1 || second.Messages[0].ID != first.Messages[0].ID {
		t.Fatalf("stable replay result = %+v, first = %+v", second, first)
	}
}

func TestInsertLocalMessagesRejectsOutboxReplayForReusedSenderProfile(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	input := LocalMessageSend{
		SenderProfile:    "max.reviewer",
		SenderBotName:    "reviewer",
		SenderBotID:      "ag_original_bot",
		SenderBotAgentID: "ag_original_agent",
		Recipients: []LocalMessageRecipient{{
			Profile: "max.runner",
			BotName: "runner",
		}},
		Body:           MessageBody{Format: "markdown", Content: "same body"},
		IdempotencyKey: "op_reusedprofileconflict",
		Now:            time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	if _, err := store.InsertLocalMessages(ctx, input); err != nil {
		t.Fatal(err)
	}
	input.SenderBotID = "ag_recreated_bot"
	input.SenderBotAgentID = "ag_recreated_agent"
	_, err = store.InsertLocalMessages(ctx, input)
	if !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("reused profile replay error = %v, want ErrMessageConflict", err)
	}
}

// TestUnclaimedCountsByProfile is the regression test for bug #95: `daemon
// health` must be able to surface a per-profile backlog of queued (unclaimed)
// messages, so an operator can tell that mentions are piling up on a profile
// that nothing is draining — not just that the daemon is "connected".
func TestUnclaimedCountsByProfile(t *testing.T) {
	ctx := context.Background()
	paths, err := ResolvePaths(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(ctx, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	send := func(recipient, key string) string {
		res, err := store.InsertLocalMessages(ctx, LocalMessageSend{
			SenderProfile:  "max.sender",
			SenderBotName:  "sender",
			Recipients:     []LocalMessageRecipient{{Profile: recipient, BotName: "bot"}},
			Body:           MessageBody{Format: "markdown", Content: "ping"},
			IdempotencyKey: key,
			Now:            time.Now().UTC(),
		})
		if err != nil {
			t.Fatalf("insert for %s: %v", recipient, err)
		}
		return res.Messages[0].ID
	}

	// Two queued for max.bar, one for max.baz which we then claim (drained).
	send("max.bar", "op_bar_1")
	send("max.bar", "op_bar_2")
	bazID := send("max.baz", "op_baz_1")
	if _, err := store.ClaimMessage(ctx, MessageClaimOptions{
		Profile:     "max.baz",
		MessageID:   bazID,
		ClaimHolder: "owner:max.baz",
		LeaseTTL:    time.Minute,
		Now:         time.Now().UTC(),
	}); err != nil {
		t.Fatalf("claim baz: %v", err)
	}

	counts, err := store.UnclaimedCountsByProfile(ctx)
	if err != nil {
		t.Fatalf("UnclaimedCountsByProfile: %v", err)
	}
	if counts["max.bar"] != 2 {
		t.Fatalf("expected 2 unclaimed for max.bar, got %d", counts["max.bar"])
	}
	if got, ok := counts["max.baz"]; ok {
		t.Fatalf("max.baz was claimed, expected it omitted from queued counts, got %d", got)
	}
	if _, ok := counts["max.sender"]; ok {
		t.Fatalf("sender should not appear as a recipient with queued mentions")
	}
}
