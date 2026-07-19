package agothreadstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"claudexflow/internal/agoprotocol"
)

const ClientProjectionSchemaVersion = 1

// ClientProjection is the client-neutral, reconnectable view of one thread at
// a single durable store snapshot.
type ClientProjection struct {
	SchemaVersion          int                 `json:"schema_version"`
	Thread                 ThreadRecord        `json:"thread"`
	Mailbox                MailboxState        `json:"mailbox"`
	Events                 []agoprotocol.Event `json:"events"`
	Dialogs                []PluginDialog      `json:"dialogs"`
	Diff                   GitDiffProjection   `json:"diff"`
	RequestedAfterSequence uint64              `json:"requested_after_sequence"`
	NextAfterSequence      uint64              `json:"next_after_sequence"`
	SnapshotSequence       uint64              `json:"snapshot_sequence"`
	HasMore                bool                `json:"has_more"`
}

// ClientProjection reads all mutable projection state and a page of original
// immutable events from one read-only SQLite transaction.
func (store *Store) ClientProjection(ctx context.Context, threadID string, afterSequence uint64, limit int) (ClientProjection, error) {
	if threadID == "" {
		return ClientProjection{}, fmt.Errorf("thread_id is required")
	}
	if limit < 1 || limit > 1000 {
		return ClientProjection{}, fmt.Errorf("limit must be between 1 and 1000")
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ClientProjection{}, fmt.Errorf("begin client projection: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	thread, err := loadProjectionThread(ctx, tx, threadID)
	if err != nil {
		return ClientProjection{}, err
	}
	if afterSequence > thread.LastSequence {
		return ClientProjection{}, fmt.Errorf("after_sequence %d exceeds snapshot sequence %d", afterSequence, thread.LastSequence)
	}
	mailbox, err := loadMailboxState(ctx, tx, threadID)
	if err != nil {
		return ClientProjection{}, fmt.Errorf("read projection mailbox: %w", err)
	}
	dialogs, err := loadProjectionDialogs(ctx, tx, threadID)
	if err != nil {
		return ClientProjection{}, err
	}
	diff, err := loadGitDiff(ctx, tx, threadID)
	if err != nil {
		return ClientProjection{}, fmt.Errorf("read git diff projection: %w", err)
	}
	events, err := loadProjectionEvents(ctx, tx, threadID, afterSequence, limit+1)
	if err != nil {
		return ClientProjection{}, err
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	next := afterSequence
	if len(events) != 0 {
		next = events[len(events)-1].Sequence
	}
	projection := ClientProjection{SchemaVersion: ClientProjectionSchemaVersion, Thread: thread, Mailbox: mailbox, Events: events, Dialogs: dialogs, Diff: diff, RequestedAfterSequence: afterSequence, NextAfterSequence: next, SnapshotSequence: thread.LastSequence, HasMore: hasMore}
	if err := tx.Commit(); err != nil {
		return ClientProjection{}, fmt.Errorf("commit client projection: %w", err)
	}
	return projection, nil
}

func loadProjectionThread(ctx context.Context, tx *sql.Tx, threadID string) (ThreadRecord, error) {
	var r ThreadRecord
	var executorType agoprotocol.ExecutorType
	var projectJSON, agentJSON, provenanceJSON []byte
	err := tx.QueryRowContext(ctx, `SELECT thread_id,last_sequence,title,workspace,mode,executor_type,runner_id,project_json,agent_snapshot_json,provenance_json FROM threads WHERE thread_id=?`, threadID).Scan(&r.ThreadID, &r.LastSequence, &r.Title, &r.Workspace, &r.Mode, &executorType, &r.Executor.RunnerID, &projectJSON, &agentJSON, &provenanceJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("thread %q does not exist", threadID)
	}
	if err != nil {
		return r, fmt.Errorf("read projection thread: %w", err)
	}
	r.Executor.Type = executorType
	if err := json.Unmarshal(projectJSON, &r.Project); err != nil {
		return r, fmt.Errorf("decode project identity: %w", err)
	}
	if err := json.Unmarshal(agentJSON, &r.Agent); err != nil {
		return r, fmt.Errorf("decode agent snapshot: %w", err)
	}
	if err := json.Unmarshal(provenanceJSON, &r.Provenance); err != nil {
		return r, fmt.Errorf("decode thread provenance: %w", err)
	}
	return r, nil
}

func loadProjectionDialogs(ctx context.Context, tx *sql.Tx, threadID string) ([]PluginDialog, error) {
	rows, err := tx.QueryContext(ctx, dialogSelect+` WHERE thread_id=? ORDER BY requested_sequence,dialog_id`, threadID)
	if err != nil {
		return nil, fmt.Errorf("read projection dialogs: %w", err)
	}
	defer rows.Close()
	out := make([]PluginDialog, 0)
	for rows.Next() {
		d, err := scanDialog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read projection dialogs: %w", err)
	}
	return out, nil
}

func loadProjectionEvents(ctx context.Context, tx *sql.Tx, threadID string, after uint64, limit int) ([]agoprotocol.Event, error) {
	rows, err := tx.QueryContext(ctx, `SELECT event_json FROM events WHERE thread_id=? AND sequence>? ORDER BY sequence ASC LIMIT ?`, threadID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("read projection events: %w", err)
	}
	defer rows.Close()
	out := make([]agoprotocol.Event, 0, limit)
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var event agoprotocol.Event
		if err := json.Unmarshal(encoded, &event); err != nil {
			return nil, fmt.Errorf("decode projection event: %w", err)
		}
		out = append(out, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read projection events: %w", err)
	}
	return out, nil
}
