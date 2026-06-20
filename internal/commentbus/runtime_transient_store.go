package commentbus

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
)

type transientRuntimeStoreRow struct {
	Record TransientRuntimeRecord
	RunID  string
	Valid  bool
}

func (s *Store) PutTransientRuntime(ctx context.Context, record TransientRuntimeRecord) error {
	if err := validateTransientRuntimeRecord(record); err != nil {
		return err
	}
	commandJSON, err := json.Marshal(record.RuntimeCommand)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO transient_runtimes (
		run_id, host, bmux_binary, profile, role, bot_name, bot_id, bot_agent_id, session_name, pane_target, runtime, runtime_command_json,
		runtime_command_path, comment_command_path, output_log_path, runtime_path, cwd, state, started_at,
		runtime_launch_mode
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(run_id) DO UPDATE SET
		host = excluded.host,
		bmux_binary = excluded.bmux_binary,
		profile = excluded.profile,
		role = excluded.role,
		bot_name = excluded.bot_name,
		bot_id = excluded.bot_id,
		bot_agent_id = excluded.bot_agent_id,
		session_name = excluded.session_name,
		pane_target = excluded.pane_target,
		runtime = excluded.runtime,
		runtime_command_json = excluded.runtime_command_json,
		runtime_command_path = excluded.runtime_command_path,
		comment_command_path = excluded.comment_command_path,
		output_log_path = excluded.output_log_path,
		runtime_path = excluded.runtime_path,
		cwd = excluded.cwd,
		state = excluded.state,
		started_at = excluded.started_at,
		runtime_launch_mode = excluded.runtime_launch_mode`,
		record.RunID,
		normalizeSessionHost(record.Host),
		record.BmuxBinary,
		record.Profile,
		record.Role,
		record.BotName,
		record.BotID,
		record.BotAgentID,
		record.SessionName,
		record.PaneTarget,
		record.Runtime,
		string(commandJSON),
		record.RuntimeCommandPath,
		record.CommentCommandPath,
		record.OutputLogPath,
		record.RuntimePath,
		record.CWD,
		record.State,
		record.StartedAt,
		normalizeRuntimeLaunchMode(record.RuntimeLaunchMode),
	)
	return err
}

func (s *Store) DeleteTransientRuntime(ctx context.Context, runID string) error {
	if !LocalSessionIDRE.MatchString(runID) {
		return errors.New("invalid transient runtime id")
	}
	return s.deleteTransientRuntimeRow(ctx, runID)
}

func (s *Store) deleteTransientRuntimeRow(ctx context.Context, runID string) error {
	if runID == "" {
		return errors.New("invalid transient runtime id")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM transient_runtimes WHERE run_id = ?`, runID)
	return err
}

func (s *Store) ListTransientRuntimes(ctx context.Context) ([]TransientRuntimeRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		run_id, host, bmux_binary, profile, role, bot_name, bot_id, bot_agent_id, session_name, pane_target, runtime, runtime_command_json,
		runtime_command_path, comment_command_path, output_log_path, runtime_path, cwd, state, started_at,
		runtime_launch_mode
		FROM transient_runtimes
		ORDER BY started_at ASC, run_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []TransientRuntimeRecord
	for rows.Next() {
		record, err := scanTransientRuntimeRecord(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Store) listTransientRuntimeRows(ctx context.Context) ([]transientRuntimeStoreRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
		run_id, host, bmux_binary, profile, role, bot_name, bot_id, bot_agent_id, session_name, pane_target, runtime, runtime_command_json,
		runtime_command_path, comment_command_path, output_log_path, runtime_path, cwd, state, started_at,
		runtime_launch_mode
		FROM transient_runtimes
		ORDER BY started_at ASC, run_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []transientRuntimeStoreRow
	for rows.Next() {
		row, err := scanTransientRuntimeStoreRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func scanTransientRuntimeStoreRow(row *sql.Rows) (transientRuntimeStoreRow, error) {
	var record TransientRuntimeRecord
	var commandJSON string
	if err := row.Scan(
		&record.RunID,
		&record.Host,
		&record.BmuxBinary,
		&record.Profile,
		&record.Role,
		&record.BotName,
		&record.BotID,
		&record.BotAgentID,
		&record.SessionName,
		&record.PaneTarget,
		&record.Runtime,
		&commandJSON,
		&record.RuntimeCommandPath,
		&record.CommentCommandPath,
		&record.OutputLogPath,
		&record.RuntimePath,
		&record.CWD,
		&record.State,
		&record.StartedAt,
		&record.RuntimeLaunchMode,
	); err != nil {
		return transientRuntimeStoreRow{}, err
	}
	valid := true
	if err := json.Unmarshal([]byte(commandJSON), &record.RuntimeCommand); err != nil {
		valid = false
	}
	record.Role = normalizeTransientRuntimeRole(record.Role)
	record.Host = normalizeSessionHost(record.Host)
	record.RuntimeLaunchMode = normalizeRuntimeLaunchMode(record.RuntimeLaunchMode)
	if valid {
		if err := validateTransientRuntimeRecord(record); err != nil {
			valid = false
		}
	}
	return transientRuntimeStoreRow{Record: record, RunID: record.RunID, Valid: valid}, nil
}

func scanTransientRuntimeRecord(row *sql.Rows) (TransientRuntimeRecord, error) {
	record, err := scanRawTransientRuntimeRecord(row)
	if err != nil {
		return TransientRuntimeRecord{}, err
	}
	if err := validateTransientRuntimeRecord(record); err != nil {
		return TransientRuntimeRecord{}, err
	}
	return record, nil
}

func scanRawTransientRuntimeRecord(row *sql.Rows) (TransientRuntimeRecord, error) {
	var record TransientRuntimeRecord
	var commandJSON string
	if err := row.Scan(
		&record.RunID,
		&record.Host,
		&record.BmuxBinary,
		&record.Profile,
		&record.Role,
		&record.BotName,
		&record.BotID,
		&record.BotAgentID,
		&record.SessionName,
		&record.PaneTarget,
		&record.Runtime,
		&commandJSON,
		&record.RuntimeCommandPath,
		&record.CommentCommandPath,
		&record.OutputLogPath,
		&record.RuntimePath,
		&record.CWD,
		&record.State,
		&record.StartedAt,
		&record.RuntimeLaunchMode,
	); err != nil {
		return TransientRuntimeRecord{}, err
	}
	if err := json.Unmarshal([]byte(commandJSON), &record.RuntimeCommand); err != nil {
		return TransientRuntimeRecord{}, err
	}
	record.Role = normalizeTransientRuntimeRole(record.Role)
	record.Host = normalizeSessionHost(record.Host)
	record.RuntimeLaunchMode = normalizeRuntimeLaunchMode(record.RuntimeLaunchMode)
	return record, nil
}

func validateTransientRuntimeRecord(record TransientRuntimeRecord) error {
	record.Host = normalizeSessionHost(record.Host)
	mode := normalizeRuntimeLaunchMode(record.RuntimeLaunchMode)
	// In "shell" mode the runtime is resolved through the user's login shell at
	// launch, so the path fields carry no meaning and must be empty. In "path"
	// mode both must be present (the legacy trusted-binary contract).
	pathFieldsInvalid := false
	if mode == RuntimeLaunchModeShell {
		pathFieldsInvalid = record.RuntimeCommandPath != "" || record.RuntimePath != ""
	} else {
		pathFieldsInvalid = record.RuntimeCommandPath == "" || record.RuntimePath == ""
	}
	if !LocalSessionIDRE.MatchString(record.RunID) ||
		!isSessionHost(record.Host) ||
		(record.BmuxBinary != "" && (!isSafeAbsoluteLocalPath(record.BmuxBinary) || filepath.Clean(record.BmuxBinary) != record.BmuxBinary)) ||
		!ProfileRE.MatchString(record.Profile) ||
		!isTransientRuntimeRole(record.Role) ||
		!BotNameRE.MatchString(record.BotName) ||
		!safeOptionalRegistryIdentity(record.BotID) ||
		!safeOptionalRegistryIdentity(record.BotAgentID) ||
		record.Runtime == "" ||
		len(record.RuntimeCommand) == 0 ||
		record.RuntimeCommand[0] == "" ||
		pathFieldsInvalid ||
		record.CommentCommandPath == "" ||
		record.OutputLogPath == "" ||
		record.CWD == "" ||
		record.State != "alive" ||
		record.StartedAt == "" {
		return errors.New("invalid transient runtime record")
	}
	if err := validateSessionNameForHost(record.Host, record.SessionName); err != nil {
		return errors.New("invalid transient runtime record")
	}
	if err := validatePaneTargetForHost(record.Host, record.PaneTarget); err != nil {
		return errors.New("invalid transient runtime record")
	}
	return nil
}
