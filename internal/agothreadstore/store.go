package agothreadstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"claudexflow/internal/agoprotocol"
	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

const CurrentStoreSchemaVersion = 10

type ThreadSpec struct {
	Title     string                     `json:"title,omitempty"`
	Workspace string                     `json:"workspace,omitempty"`
	Mode      agoprotocol.AgentMode      `json:"mode"`
	Executor  agoprotocol.ExecutorTarget `json:"executor"`
}

func (spec ThreadSpec) Validate() error {
	if err := spec.Mode.Validate(); err != nil {
		return err
	}
	if err := spec.Executor.Validate(); err != nil {
		return err
	}
	if spec.Executor.Type == agoprotocol.ExecutorLocal {
		if spec.Workspace == "" || !filepath.IsAbs(spec.Workspace) {
			return fmt.Errorf("local executor workspace must be an absolute path")
		}
	}
	if spec.Executor.Type == agoprotocol.ExecutorRunner && spec.Workspace == "" {
		return fmt.Errorf("runner executor workspace is required")
	}
	return nil
}

type ThreadRecord struct {
	ThreadID     string                     `json:"thread_id"`
	LastSequence uint64                     `json:"last_sequence"`
	Title        string                     `json:"title"`
	Workspace    string                     `json:"workspace"`
	Mode         agoprotocol.AgentMode      `json:"mode"`
	Executor     agoprotocol.ExecutorTarget `json:"executor"`
	Project      ProjectIdentity            `json:"project"`
	Agent        AgentDefinitionSnapshot    `json:"agent"`
	Provenance   agoprotocol.Provenance     `json:"provenance,omitempty"`
}

type EventDraft struct {
	Type       agoprotocol.EventType
	Visibility agoprotocol.Visibility
	Provenance agoprotocol.Provenance
	Payload    json.RawMessage
}

type CommandResult struct {
	ThreadID     string              `json:"thread_id"`
	LastSequence uint64              `json:"last_sequence"`
	Events       []agoprotocol.Event `json:"events"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open thread store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{db: db}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := store.ensureThreadCatalog(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (store *Store) initialize(ctx context.Context) error {
	if _, err := store.db.ExecContext(ctx, `
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA busy_timeout = 5000;
`); err != nil {
		return fmt.Errorf("configure thread store: %w", err)
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin thread store migration: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read thread store schema version: %w", err)
	}
	if version > CurrentStoreSchemaVersion {
		return fmt.Errorf("thread store schema version %d is newer than supported version %d", version, CurrentStoreSchemaVersion)
	}
	if version == CurrentStoreSchemaVersion {
		return tx.Commit()
	}

	const baseSchema = `

CREATE TABLE IF NOT EXISTS threads (
    thread_id TEXT PRIMARY KEY,
	last_sequence INTEGER NOT NULL CHECK (last_sequence >= 1)
);

CREATE TABLE IF NOT EXISTS events (
    thread_id TEXT NOT NULL REFERENCES threads(thread_id),
    sequence INTEGER NOT NULL CHECK (sequence >= 1),
    event_json BLOB NOT NULL,
    PRIMARY KEY (thread_id, sequence)
);

CREATE TABLE IF NOT EXISTS commands (
    actor_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    command_id TEXT NOT NULL,
    request_hash BLOB NOT NULL,
    thread_id TEXT NOT NULL REFERENCES threads(thread_id),
    result_json BLOB NOT NULL,
    PRIMARY KEY (actor_id, idempotency_key)
);
`
	if _, err := tx.ExecContext(ctx, baseSchema); err != nil {
		return fmt.Errorf("initialize base thread store schema: %w", err)
	}

	columns, err := tableColumns(ctx, tx, "threads")
	if err != nil {
		return err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "activity", definition: `TEXT NOT NULL DEFAULT 'idle'`},
		{name: "active_turn_id", definition: `TEXT NOT NULL DEFAULT ''`},
		{name: "cancel_requested", definition: `INTEGER NOT NULL DEFAULT 0`},
		{name: "title", definition: `TEXT NOT NULL DEFAULT ''`},
		{name: "workspace", definition: `TEXT NOT NULL DEFAULT ''`},
		{name: "mode", definition: `TEXT NOT NULL DEFAULT ''`},
		{name: "executor_type", definition: `TEXT NOT NULL DEFAULT ''`},
		{name: "runner_id", definition: `TEXT NOT NULL DEFAULT ''`},
		{name: "project_json", definition: `BLOB NOT NULL DEFAULT '{}'`},
		{name: "agent_snapshot_json", definition: `BLOB NOT NULL DEFAULT '{}'`},
		{name: "provenance_json", definition: `BLOB NOT NULL DEFAULT '{}'`},
	} {
		if columns[column.name] {
			continue
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE threads ADD COLUMN %s %s`, column.name, column.definition)); err != nil {
			return fmt.Errorf("add threads.%s: %w", column.name, err)
		}
	}

	const mailboxSchema = `
CREATE TABLE IF NOT EXISTS pending_inputs (
    thread_id TEXT NOT NULL REFERENCES threads(thread_id),
    queue_item_id TEXT NOT NULL,
    position INTEGER NOT NULL,
    class TEXT NOT NULL,
    state TEXT NOT NULL,
    content_json BLOB NOT NULL,
    PRIMARY KEY (thread_id, queue_item_id),
    UNIQUE (thread_id, position)
);

CREATE TABLE IF NOT EXISTS compactions (
    thread_id TEXT NOT NULL REFERENCES threads(thread_id),
    compaction_id TEXT NOT NULL,
    event_sequence INTEGER NOT NULL,
    through_sequence INTEGER NOT NULL,
    summary TEXT NOT NULL,
    PRIMARY KEY (thread_id, compaction_id),
    UNIQUE (thread_id, event_sequence)
);
`
	if _, err := tx.ExecContext(ctx, mailboxSchema); err != nil {
		return fmt.Errorf("initialize mailbox schema: %w", err)
	}
	const dialogSchema = `
CREATE TABLE IF NOT EXISTS plugin_dialogs (
    dialog_id TEXT PRIMARY KEY,
    thread_id TEXT NOT NULL REFERENCES threads(thread_id),
    turn_id TEXT NOT NULL,
    plugin_id TEXT NOT NULL,
    generation INTEGER NOT NULL CHECK (generation >= 0),
    invocation_id TEXT NOT NULL,
    deadline TEXT NOT NULL,
    request_type TEXT NOT NULL,
    request_json BLOB NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending', 'resolved')),
    revision INTEGER NOT NULL CHECK (revision >= 1),
    requested_sequence INTEGER NOT NULL,
    resolved_sequence INTEGER NOT NULL DEFAULT 0,
    resolver_id TEXT NOT NULL DEFAULT '',
    response_json BLOB,
    UNIQUE (thread_id, turn_id, plugin_id, generation, invocation_id)
);
CREATE INDEX IF NOT EXISTS plugin_dialogs_thread_state_order
    ON plugin_dialogs (thread_id, state, requested_sequence, dialog_id);
`
	if _, err := tx.ExecContext(ctx, dialogSchema); err != nil {
		return fmt.Errorf("initialize plugin dialog schema: %w", err)
	}
	const gitSchema = `
CREATE TABLE IF NOT EXISTS git_bindings (
 thread_id TEXT NOT NULL REFERENCES threads(thread_id), environment_id TEXT NOT NULL,
 executor_generation INTEGER NOT NULL CHECK(executor_generation >= 0), worktree_dir TEXT NOT NULL,
 git_dir TEXT NOT NULL, common_dir TEXT NOT NULL, repository_id TEXT NOT NULL, worktree_id TEXT NOT NULL,
 object_format TEXT NOT NULL, base_identity TEXT NOT NULL,
 PRIMARY KEY(thread_id, executor_generation)
);
CREATE TABLE IF NOT EXISTS git_snapshots (
 thread_id TEXT NOT NULL, executor_generation INTEGER NOT NULL, revision INTEGER NOT NULL CHECK(revision >= 1),
 environment_id TEXT NOT NULL, repository_id TEXT NOT NULL, worktree_id TEXT NOT NULL, idempotency_key TEXT NOT NULL,
 digest TEXT NOT NULL, head_oid TEXT NOT NULL, index_digest TEXT NOT NULL, projection_json BLOB NOT NULL,
 created_sequence INTEGER NOT NULL, created_at TEXT NOT NULL,
 PRIMARY KEY(thread_id, executor_generation, revision),
 UNIQUE(thread_id, executor_generation, digest), UNIQUE(thread_id, executor_generation, idempotency_key),
 FOREIGN KEY(thread_id, executor_generation) REFERENCES git_bindings(thread_id, executor_generation)
);
CREATE TABLE IF NOT EXISTS git_snapshot_artifacts (
 thread_id TEXT NOT NULL, executor_generation INTEGER NOT NULL, revision INTEGER NOT NULL,
 snapshot_digest TEXT NOT NULL, artifact_json BLOB NOT NULL CHECK(length(artifact_json) > 0 AND length(artifact_json) <= 67108864),
 PRIMARY KEY(thread_id,executor_generation,revision),
 FOREIGN KEY(thread_id,executor_generation,revision) REFERENCES git_snapshots(thread_id,executor_generation,revision)
);
CREATE TABLE IF NOT EXISTS git_comments (
 thread_id TEXT NOT NULL, comment_id TEXT NOT NULL, snapshot_generation INTEGER NOT NULL, snapshot_revision INTEGER NOT NULL,
 snapshot_digest TEXT NOT NULL, file_id TEXT NOT NULL, hunk_id TEXT NOT NULL, actor TEXT NOT NULL, body TEXT NOT NULL,
 created_sequence INTEGER NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(thread_id, comment_id),
 FOREIGN KEY(thread_id, snapshot_generation, snapshot_revision) REFERENCES git_snapshots(thread_id, executor_generation, revision)
);
CREATE INDEX IF NOT EXISTS git_snapshots_latest ON git_snapshots(thread_id, created_sequence DESC);
CREATE INDEX IF NOT EXISTS git_comments_snapshot ON git_comments(thread_id, snapshot_generation, snapshot_revision, created_sequence);
CREATE TABLE IF NOT EXISTS git_operations (
 operation_id TEXT PRIMARY KEY,
 actor_id TEXT NOT NULL, idempotency_domain TEXT NOT NULL CHECK(idempotency_domain = 'ago.git-operation'), idempotency_key TEXT NOT NULL,
 command_domain TEXT NOT NULL CHECK(command_domain = 'ago.git-operation'), command_version INTEGER NOT NULL CHECK(command_version = 1), command_id TEXT NOT NULL CHECK(command_id LIKE 'git:%'),
 thread_id TEXT NOT NULL, environment_id TEXT NOT NULL, executor_generation INTEGER NOT NULL CHECK(executor_generation >= 0),
 repository_id TEXT NOT NULL, worktree_id TEXT NOT NULL, base_identity TEXT NOT NULL, object_format TEXT NOT NULL CHECK(object_format IN ('sha1','sha256')),
 operation_kind TEXT NOT NULL CHECK(operation_kind IN ('stage','unstage','revert')),
 request_hash TEXT NOT NULL CHECK(length(request_hash) = 64), plan_hash TEXT NOT NULL CHECK(length(plan_hash) = 64), request_json BLOB NOT NULL,
 expected_snapshot_revision INTEGER NOT NULL CHECK(expected_snapshot_revision >= 1), expected_snapshot_digest TEXT NOT NULL,
 selected_unit_ids_json BLOB NOT NULL,
 before_json BLOB NOT NULL, intended_after_json BLOB NOT NULL, latest_observed_json BLOB,
 state TEXT NOT NULL CHECK(state IN ('prepared','completed','conflicted','outcome-unknown')),
 evidence TEXT NOT NULL DEFAULT '' CHECK(evidence IN ('','post-attempt','owner-fenced')),
 no_future_write INTEGER NOT NULL DEFAULT 0 CHECK(no_future_write IN (0,1)), result_json BLOB,
 prepared_sequence INTEGER NOT NULL CHECK(prepared_sequence >= 1),
 last_transition_sequence INTEGER NOT NULL CHECK(last_transition_sequence >= prepared_sequence),
 resolved_sequence INTEGER NOT NULL DEFAULT 0 CHECK(resolved_sequence >= 0),
 CHECK((state='prepared' AND latest_observed_json IS NULL AND evidence='' AND result_json IS NULL AND resolved_sequence=0 AND last_transition_sequence=prepared_sequence)
    OR (state='outcome-unknown' AND latest_observed_json IS NOT NULL AND evidence<>'' AND result_json IS NOT NULL AND resolved_sequence=0 AND last_transition_sequence>prepared_sequence)
    OR (state IN ('completed','conflicted') AND latest_observed_json IS NOT NULL AND evidence<>'' AND result_json IS NOT NULL AND resolved_sequence=last_transition_sequence AND resolved_sequence>prepared_sequence)),
 UNIQUE(idempotency_domain, actor_id, idempotency_key), UNIQUE(command_domain, command_version, command_id),
 FOREIGN KEY(thread_id, executor_generation, expected_snapshot_revision) REFERENCES git_snapshots(thread_id, executor_generation, revision)
);
CREATE INDEX IF NOT EXISTS git_operations_unresolved_scope
 ON git_operations(thread_id, environment_id, executor_generation, state, prepared_sequence);
CREATE UNIQUE INDEX IF NOT EXISTS git_operations_unresolved_worktree
 ON git_operations(repository_id,worktree_id) WHERE state IN ('prepared','outcome-unknown');
CREATE TABLE IF NOT EXISTS git_write_receipts (
 receipt_id TEXT PRIMARY KEY,
 owner_domain TEXT NOT NULL CHECK(owner_domain = 'ago.tool-write'),
 operation_id TEXT NOT NULL, tool_call_id TEXT NOT NULL, tool_name TEXT NOT NULL,
 idempotency_key TEXT NOT NULL, request_hash TEXT NOT NULL CHECK(length(request_hash) = 64),
 thread_id TEXT NOT NULL, environment_id TEXT NOT NULL, executor_generation INTEGER NOT NULL CHECK(executor_generation >= 0),
 repository_id TEXT NOT NULL, worktree_id TEXT NOT NULL, base_identity TEXT NOT NULL,
 created_sequence INTEGER NOT NULL CHECK(created_sequence >= 1),
 UNIQUE(thread_id, executor_generation, idempotency_key),
 FOREIGN KEY(thread_id, executor_generation) REFERENCES git_bindings(thread_id, executor_generation)
);
CREATE TABLE IF NOT EXISTS git_write_receipt_paths (
 receipt_id TEXT NOT NULL REFERENCES git_write_receipts(receipt_id),
 ordinal INTEGER NOT NULL CHECK(ordinal >= 0), path TEXT NOT NULL,
 before_json BLOB NOT NULL, after_json BLOB NOT NULL,
 PRIMARY KEY(receipt_id, ordinal), UNIQUE(receipt_id, path)
);
CREATE INDEX IF NOT EXISTS git_write_receipt_paths_path ON git_write_receipt_paths(path, receipt_id);
CREATE INDEX IF NOT EXISTS git_write_receipts_scope
 ON git_write_receipts(thread_id, environment_id, executor_generation, repository_id, worktree_id, base_identity, created_sequence);
`
	if _, err := tx.ExecContext(ctx, gitSchema); err != nil {
		return fmt.Errorf("initialize git snapshot schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, CurrentStoreSchemaVersion)); err != nil {
		return fmt.Errorf("record thread store schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit thread store migration: %w", err)
	}
	return nil
}

func tableColumns(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, fmt.Errorf("read %s columns: %w", table, err)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var position int
		var name, dataType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&position, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, fmt.Errorf("scan %s columns: %w", table, err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s columns: %w", table, err)
	}
	return columns, nil
}

func (store *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	if err := store.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read thread store schema version: %w", err)
	}
	return version, nil
}

func (store *Store) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	return store.db.Close()
}

func (store *Store) CreateThread(ctx context.Context, command agoprotocol.Command, payload json.RawMessage) (CommandResult, error) {
	return store.createThread(ctx, command, payload, ThreadSpec{})
}

func (store *Store) CreateConfiguredThread(ctx context.Context, command agoprotocol.Command, spec ThreadSpec) (CommandResult, error) {
	if err := spec.Validate(); err != nil {
		return CommandResult{}, err
	}
	payload, err := json.Marshal(spec)
	if err != nil {
		return CommandResult{}, fmt.Errorf("encode thread configuration: %w", err)
	}
	return store.createThread(ctx, command, payload, spec)
}

func (store *Store) createThread(ctx context.Context, command agoprotocol.Command, payload json.RawMessage, spec ThreadSpec) (CommandResult, error) {
	if err := command.Validate(); err != nil {
		return CommandResult{}, err
	}
	if command.Type != agoprotocol.CommandThreadCreate {
		return CommandResult{}, fmt.Errorf("CreateThread requires a %q command", agoprotocol.CommandThreadCreate)
	}
	requestHash, err := hashRequest(command, []EventDraft{{
		Type:       agoprotocol.EventThreadCreated,
		Visibility: agoprotocol.VisibilityUser,
		Payload:    payload,
	}})
	if err != nil {
		return CommandResult{}, err
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return CommandResult{}, fmt.Errorf("begin create thread: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if existing, found, err := commandResult(ctx, tx, command, requestHash); err != nil {
		return CommandResult{}, err
	} else if found {
		return existing, nil
	}

	threadID, err := randomID("T-")
	if err != nil {
		return CommandResult{}, err
	}
	eventID, err := randomID("E-")
	if err != nil {
		return CommandResult{}, err
	}
	event := agoprotocol.Event{
		SchemaVersion: agoprotocol.SchemaVersion,
		EventID:       eventID,
		ThreadID:      threadID,
		Sequence:      1,
		Type:          agoprotocol.EventThreadCreated,
		Visibility:    agoprotocol.VisibilityUser,
		Payload:       cloneRawMessage(payload),
	}
	if err := event.Validate(); err != nil {
		return CommandResult{}, err
	}
	result := CommandResult{ThreadID: threadID, LastSequence: 1, Events: []agoprotocol.Event{event}}

	if _, err := tx.ExecContext(ctx, `
INSERT INTO threads (thread_id, last_sequence, title, workspace, mode, executor_type, runner_id)
VALUES (?, 1, ?, ?, ?, ?, ?)`, threadID, spec.Title, spec.Workspace, spec.Mode, spec.Executor.Type, spec.Executor.RunnerID); err != nil {
		return CommandResult{}, fmt.Errorf("insert thread: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO thread_catalog (thread_id,project_id,created_at,updated_at) VALUES (?, '', '', '')`, threadID); err != nil {
		return CommandResult{}, fmt.Errorf("insert legacy thread catalog entry: %w", err)
	}
	if err := insertEvent(ctx, tx, event); err != nil {
		return CommandResult{}, err
	}
	if err := insertCommand(ctx, tx, command, requestHash, result); err != nil {
		return CommandResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CommandResult{}, fmt.Errorf("commit create thread: %w", err)
	}
	return result, nil
}

func (store *Store) Thread(ctx context.Context, threadID string) (ThreadRecord, error) {
	var record ThreadRecord
	var executorType agoprotocol.ExecutorType
	var projectJSON, agentJSON, provenanceJSON []byte
	err := store.db.QueryRowContext(ctx, `
SELECT thread_id, last_sequence, title, workspace, mode, executor_type, runner_id, project_json, agent_snapshot_json, provenance_json
FROM threads WHERE thread_id = ?`, threadID).Scan(
		&record.ThreadID,
		&record.LastSequence,
		&record.Title,
		&record.Workspace,
		&record.Mode,
		&executorType,
		&record.Executor.RunnerID,
		&projectJSON, &agentJSON, &provenanceJSON,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ThreadRecord{}, fmt.Errorf("thread %q does not exist", threadID)
	}
	if err != nil {
		return ThreadRecord{}, fmt.Errorf("read thread: %w", err)
	}
	record.Executor.Type = executorType
	if err := json.Unmarshal(projectJSON, &record.Project); err != nil {
		return ThreadRecord{}, fmt.Errorf("decode project identity: %w", err)
	}
	if err := json.Unmarshal(agentJSON, &record.Agent); err != nil {
		return ThreadRecord{}, fmt.Errorf("decode agent snapshot: %w", err)
	}
	if err := json.Unmarshal(provenanceJSON, &record.Provenance); err != nil {
		return ThreadRecord{}, fmt.Errorf("decode thread provenance: %w", err)
	}
	return record, nil
}

func (store *Store) ListThreads(ctx context.Context) ([]ThreadRecord, error) {
	rows, err := store.db.QueryContext(ctx, `
SELECT thread_id, last_sequence, title, workspace, mode, executor_type, runner_id, project_json, agent_snapshot_json, provenance_json
FROM threads ORDER BY thread_id`)
	if err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	defer rows.Close()
	records := make([]ThreadRecord, 0)
	for rows.Next() {
		var record ThreadRecord
		var executorType agoprotocol.ExecutorType
		var projectJSON, agentJSON, provenanceJSON []byte
		if err := rows.Scan(&record.ThreadID, &record.LastSequence, &record.Title, &record.Workspace, &record.Mode, &executorType, &record.Executor.RunnerID, &projectJSON, &agentJSON, &provenanceJSON); err != nil {
			return nil, fmt.Errorf("scan thread: %w", err)
		}
		record.Executor.Type = executorType
		if err := json.Unmarshal(projectJSON, &record.Project); err != nil {
			return nil, fmt.Errorf("decode project identity: %w", err)
		}
		if err := json.Unmarshal(agentJSON, &record.Agent); err != nil {
			return nil, fmt.Errorf("decode agent snapshot: %w", err)
		}
		if err := json.Unmarshal(provenanceJSON, &record.Provenance); err != nil {
			return nil, fmt.Errorf("decode thread provenance: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list threads: %w", err)
	}
	return records, nil
}

func (store *Store) Append(ctx context.Context, command agoprotocol.Command, drafts ...EventDraft) (CommandResult, error) {
	if err := command.Validate(); err != nil {
		return CommandResult{}, err
	}
	if command.Type != agoprotocol.CommandMessageAppend {
		return CommandResult{}, fmt.Errorf("Append requires a %q command", agoprotocol.CommandMessageAppend)
	}
	if len(drafts) == 0 {
		return CommandResult{}, fmt.Errorf("at least one event is required")
	}
	requestHash, err := hashRequest(command, drafts)
	if err != nil {
		return CommandResult{}, err
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return CommandResult{}, fmt.Errorf("begin append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if existing, found, err := commandResult(ctx, tx, command, requestHash); err != nil {
		return CommandResult{}, err
	} else if found {
		return existing, nil
	}

	var lastSequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence FROM threads WHERE thread_id = ?`, command.ThreadID).Scan(&lastSequence); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CommandResult{}, fmt.Errorf("thread %q does not exist", command.ThreadID)
		}
		return CommandResult{}, fmt.Errorf("read thread sequence: %w", err)
	}
	if command.ExpectedSequence != nil && *command.ExpectedSequence != lastSequence {
		return CommandResult{}, fmt.Errorf("thread sequence is %d, expected %d", lastSequence, *command.ExpectedSequence)
	}

	events := make([]agoprotocol.Event, 0, len(drafts))
	for index, draft := range drafts {
		eventID, err := randomID("E-")
		if err != nil {
			return CommandResult{}, err
		}
		event := agoprotocol.Event{
			SchemaVersion: agoprotocol.SchemaVersion,
			EventID:       eventID,
			ThreadID:      command.ThreadID,
			Sequence:      lastSequence + uint64(index) + 1,
			Type:          draft.Type,
			Visibility:    draft.Visibility,
			Provenance:    draft.Provenance,
			Payload:       cloneRawMessage(draft.Payload),
		}
		if err := event.Validate(); err != nil {
			return CommandResult{}, err
		}
		if err := insertEvent(ctx, tx, event); err != nil {
			return CommandResult{}, err
		}
		events = append(events, event)
	}
	lastSequence += uint64(len(events))
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET last_sequence = ? WHERE thread_id = ?`, lastSequence, command.ThreadID); err != nil {
		return CommandResult{}, fmt.Errorf("update thread sequence: %w", err)
	}
	result := CommandResult{ThreadID: command.ThreadID, LastSequence: lastSequence, Events: events}
	if err := insertCommand(ctx, tx, command, requestHash, result); err != nil {
		return CommandResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CommandResult{}, fmt.Errorf("commit append: %w", err)
	}
	return result, nil
}

func (store *Store) AppendTurnEvents(ctx context.Context, command agoprotocol.Command, expectedTurnID string, drafts ...EventDraft) (CommandResult, error) {
	if err := command.Validate(); err != nil {
		return CommandResult{}, err
	}
	if command.Type != agoprotocol.CommandTurnEventAppend {
		return CommandResult{}, fmt.Errorf("AppendTurnEvents requires a %q command", agoprotocol.CommandTurnEventAppend)
	}
	if expectedTurnID == "" {
		return CommandResult{}, fmt.Errorf("expected turn ID is required")
	}
	if len(drafts) == 0 {
		return CommandResult{}, fmt.Errorf("at least one event is required")
	}
	requestHash, err := hashTurnEventRequest(command, expectedTurnID, drafts)
	if err != nil {
		return CommandResult{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return CommandResult{}, fmt.Errorf("begin turn event append: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, err := commandResult(ctx, tx, command, requestHash); err != nil {
		return CommandResult{}, err
	} else if found {
		return existing, nil
	}
	var lastSequence uint64
	var activity agoprotocol.Activity
	var activeTurnID string
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence, activity, active_turn_id FROM threads WHERE thread_id = ?`, command.ThreadID).Scan(&lastSequence, &activity, &activeTurnID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CommandResult{}, fmt.Errorf("thread %q does not exist", command.ThreadID)
		}
		return CommandResult{}, fmt.Errorf("read active turn: %w", err)
	}
	if !isActiveActivity(activity) || activeTurnID != expectedTurnID {
		return CommandResult{}, StaleTurnError{ActiveTurnID: activeTurnID, ExpectedTurnID: expectedTurnID}
	}
	if command.ExpectedSequence != nil && *command.ExpectedSequence != lastSequence {
		return CommandResult{}, ConflictError{CurrentSequence: lastSequence, ExpectedSequence: *command.ExpectedSequence}
	}
	events := make([]agoprotocol.Event, 0, len(drafts))
	for index, draft := range drafts {
		eventID, err := randomID("E-")
		if err != nil {
			return CommandResult{}, err
		}
		event := agoprotocol.Event{
			SchemaVersion: agoprotocol.SchemaVersion,
			EventID:       eventID,
			ThreadID:      command.ThreadID,
			Sequence:      lastSequence + uint64(index) + 1,
			Type:          draft.Type,
			Visibility:    draft.Visibility,
			Provenance:    draft.Provenance,
			Payload:       cloneRawMessage(draft.Payload),
		}
		if err := event.Validate(); err != nil {
			return CommandResult{}, err
		}
		if err := insertEvent(ctx, tx, event); err != nil {
			return CommandResult{}, err
		}
		events = append(events, event)
	}
	lastSequence += uint64(len(events))
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET last_sequence = ? WHERE thread_id = ?`, lastSequence, command.ThreadID); err != nil {
		return CommandResult{}, fmt.Errorf("update thread sequence: %w", err)
	}
	result := CommandResult{ThreadID: command.ThreadID, LastSequence: lastSequence, Events: events}
	if err := insertCommand(ctx, tx, command, requestHash, result); err != nil {
		return CommandResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CommandResult{}, fmt.Errorf("commit turn event append: %w", err)
	}
	return result, nil
}

func (store *Store) Replay(ctx context.Context, threadID string, afterSequence uint64, limit int) ([]agoprotocol.Event, error) {
	if threadID == "" {
		return nil, fmt.Errorf("thread_id is required")
	}
	if limit < 0 {
		return nil, fmt.Errorf("limit cannot be negative")
	}
	query := `SELECT event_json FROM events WHERE thread_id = ? AND sequence > ? ORDER BY sequence`
	args := []any{threadID, afterSequence}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("replay events: %w", err)
	}
	defer rows.Close()

	events := make([]agoprotocol.Event, 0)
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		var event agoprotocol.Event
		if err := json.Unmarshal(encoded, &event); err != nil {
			return nil, fmt.Errorf("decode event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("replay events: %w", err)
	}
	return events, nil
}

func commandResult(ctx context.Context, tx *sql.Tx, command agoprotocol.Command, requestHash []byte) (CommandResult, bool, error) {
	var storedHash []byte
	var encoded []byte
	err := tx.QueryRowContext(ctx, `SELECT request_hash, result_json FROM commands WHERE actor_id = ? AND idempotency_key = ?`, command.ActorID, command.IdempotencyKey).Scan(&storedHash, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return CommandResult{}, false, nil
	}
	if err != nil {
		return CommandResult{}, false, fmt.Errorf("read command result: %w", err)
	}
	if !bytes.Equal(storedHash, requestHash) {
		return CommandResult{}, false, fmt.Errorf("idempotency key %q was already used for different content", command.IdempotencyKey)
	}
	var result CommandResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		return CommandResult{}, false, fmt.Errorf("decode command result: %w", err)
	}
	return result, true, nil
}

func insertCommand(ctx context.Context, tx *sql.Tx, command agoprotocol.Command, requestHash []byte, result CommandResult) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode command result: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO commands (actor_id, idempotency_key, command_id, request_hash, thread_id, result_json) VALUES (?, ?, ?, ?, ?, ?)`, command.ActorID, command.IdempotencyKey, command.CommandID, requestHash, result.ThreadID, encoded); err != nil {
		return fmt.Errorf("insert command: %w", err)
	}
	return nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, event agoprotocol.Event) error {
	encoded, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO events (thread_id, sequence, event_json) VALUES (?, ?, ?)`, event.ThreadID, event.Sequence, encoded); err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

func hashRequest(command agoprotocol.Command, drafts []EventDraft) ([]byte, error) {
	encoded, err := json.Marshal(struct {
		Command agoprotocol.Command `json:"command"`
		Drafts  []EventDraft        `json:"drafts"`
	}{Command: command, Drafts: drafts})
	if err != nil {
		return nil, fmt.Errorf("encode command request: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return digest[:], nil
}

func hashTurnEventRequest(command agoprotocol.Command, expectedTurnID string, drafts []EventDraft) ([]byte, error) {
	encoded, err := json.Marshal(struct {
		Command        agoprotocol.Command `json:"command"`
		ExpectedTurnID string              `json:"expected_turn_id"`
		Drafts         []EventDraft        `json:"drafts"`
	}{Command: command, ExpectedTurnID: expectedTurnID, Drafts: drafts})
	if err != nil {
		return nil, fmt.Errorf("encode turn event request: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return digest[:], nil
}

func randomID(prefix string) (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate ID: %w", err)
	}
	return prefix + hex.EncodeToString(bytes), nil
}

func cloneRawMessage(message json.RawMessage) json.RawMessage {
	if message == nil {
		return nil
	}
	return append(json.RawMessage(nil), message...)
}
