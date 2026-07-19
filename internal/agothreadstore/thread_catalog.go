package agothreadstore

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"claudexflow/internal/agoprotocol"
)

const (
	CommandThreadArchive   = agoprotocol.CommandThreadArchive
	CommandThreadUnarchive = agoprotocol.CommandThreadUnarchive
	EventThreadArchived    = agoprotocol.EventThreadArchived
	EventThreadUnarchived  = agoprotocol.EventThreadUnarchived

	ThreadCatalogSchemaVersion  = 1
	MaxThreadCatalogPageSize    = 100
	MaxThreadCatalogSearchBytes = 256
	maxThreadCatalogCursorBytes = 2_048
)

type ArchiveFilter string

const (
	ArchiveActive   ArchiveFilter = "active"
	ArchiveArchived ArchiveFilter = "archived"
	ArchiveAll      ArchiveFilter = "all"
)

type ThreadCatalogQuery struct {
	ProjectID string        `json:"project_id"`
	Search    string        `json:"search,omitempty"`
	Archive   ArchiveFilter `json:"archive"`
	Limit     int           `json:"limit"`
	Cursor    string        `json:"cursor,omitempty"`
}

func (query *ThreadCatalogQuery) validate() error {
	query.ProjectID = strings.TrimSpace(query.ProjectID)
	query.Search = strings.TrimSpace(query.Search)
	if query.ProjectID == "" {
		return fmt.Errorf("project_id is required")
	}
	if len(query.Search) > MaxThreadCatalogSearchBytes {
		return fmt.Errorf("search exceeds %d bytes", MaxThreadCatalogSearchBytes)
	}
	switch query.Archive {
	case ArchiveActive, ArchiveArchived, ArchiveAll:
	default:
		return fmt.Errorf("archive must be %q, %q, or %q", ArchiveActive, ArchiveArchived, ArchiveAll)
	}
	if query.Limit < 1 || query.Limit > MaxThreadCatalogPageSize {
		return fmt.Errorf("limit must be between 1 and %d", MaxThreadCatalogPageSize)
	}
	if len(query.Cursor) > maxThreadCatalogCursorBytes {
		return fmt.Errorf("cursor exceeds %d bytes", maxThreadCatalogCursorBytes)
	}
	return nil
}

type ThreadCatalogEntry struct {
	ThreadID     string               `json:"thread_id"`
	ProjectID    string               `json:"project_id"`
	Title        string               `json:"title"`
	Workspace    string               `json:"workspace"`
	LastSequence uint64               `json:"last_sequence"`
	Activity     agoprotocol.Activity `json:"activity"`
	CreatedAt    string               `json:"created_at"`
	UpdatedAt    string               `json:"updated_at"`
	Archived     bool                 `json:"archived"`
	ArchivedAt   string               `json:"archived_at"`
}

type ThreadCatalogPage struct {
	SchemaVersion int                  `json:"schema_version"`
	Threads       []ThreadCatalogEntry `json:"threads"`
	NextCursor    string               `json:"next_cursor,omitempty"`
}

type threadCatalogCursor struct {
	SchemaVersion int           `json:"schema_version"`
	ProjectID     string        `json:"project_id"`
	Search        string        `json:"search"`
	Archive       ArchiveFilter `json:"archive"`
	UpdatedAt     string        `json:"updated_at"`
	ThreadID      string        `json:"thread_id"`
}

// SearchThreadCatalog returns a stable keyset page. Every query is explicitly
// scoped to one immutable project identity; cursors cannot be reused with a
// different project, search term, or archive projection.
func (store *Store) SearchThreadCatalog(ctx context.Context, query ThreadCatalogQuery) (ThreadCatalogPage, error) {
	if err := query.validate(); err != nil {
		return ThreadCatalogPage{}, err
	}
	if err := store.ensureThreadCatalog(ctx); err != nil {
		return ThreadCatalogPage{}, err
	}

	cursor, err := decodeThreadCatalogCursor(query)
	if err != nil {
		return ThreadCatalogPage{}, err
	}
	where := []string{"c.project_id = ?", "instr(lower(t.title), lower(?)) > 0"}
	args := []any{query.ProjectID, query.Search}
	switch query.Archive {
	case ArchiveActive:
		where = append(where, "c.archived_at IS NULL")
	case ArchiveArchived:
		where = append(where, "c.archived_at IS NOT NULL")
	}
	if cursor != nil {
		where = append(where, "(c.updated_at < ? OR (c.updated_at = ? AND c.thread_id < ?))")
		args = append(args, cursor.UpdatedAt, cursor.UpdatedAt, cursor.ThreadID)
	}
	args = append(args, query.Limit+1)
	rows, err := store.db.QueryContext(ctx, `
SELECT c.thread_id,c.project_id,t.title,t.workspace,t.last_sequence,t.activity,
       c.created_at,c.updated_at,c.archived_at
FROM thread_catalog c
JOIN threads t ON t.thread_id = c.thread_id
WHERE `+strings.Join(where, " AND ")+`
ORDER BY c.updated_at DESC, c.thread_id DESC
LIMIT ?`, args...)
	if err != nil {
		return ThreadCatalogPage{}, fmt.Errorf("search thread catalog: %w", err)
	}
	defer rows.Close()

	entries := make([]ThreadCatalogEntry, 0, query.Limit+1)
	for rows.Next() {
		var entry ThreadCatalogEntry
		var archivedAt sql.NullString
		if err := rows.Scan(&entry.ThreadID, &entry.ProjectID, &entry.Title, &entry.Workspace, &entry.LastSequence, &entry.Activity, &entry.CreatedAt, &entry.UpdatedAt, &archivedAt); err != nil {
			return ThreadCatalogPage{}, fmt.Errorf("scan thread catalog: %w", err)
		}
		entry.Archived = archivedAt.Valid
		if archivedAt.Valid {
			entry.ArchivedAt = archivedAt.String
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return ThreadCatalogPage{}, fmt.Errorf("read thread catalog: %w", err)
	}

	page := ThreadCatalogPage{SchemaVersion: ThreadCatalogSchemaVersion, Threads: entries}
	if len(entries) > query.Limit {
		page.Threads = entries[:query.Limit]
		last := page.Threads[len(page.Threads)-1]
		page.NextCursor, err = encodeThreadCatalogCursor(query, last)
		if err != nil {
			return ThreadCatalogPage{}, err
		}
	}
	return page, nil
}

func (store *Store) ArchiveThread(ctx context.Context, command agoprotocol.Command) (CommandResult, error) {
	return store.setThreadArchived(ctx, command, true)
}

func (store *Store) UnarchiveThread(ctx context.Context, command agoprotocol.Command) (CommandResult, error) {
	return store.setThreadArchived(ctx, command, false)
}

func (store *Store) setThreadArchived(ctx context.Context, command agoprotocol.Command, archived bool) (CommandResult, error) {
	wantCommand, eventType := CommandThreadArchive, EventThreadArchived
	if !archived {
		wantCommand, eventType = CommandThreadUnarchive, EventThreadUnarchived
	}
	if err := command.Validate(); err != nil {
		return CommandResult{}, err
	}
	if command.Type != wantCommand {
		return CommandResult{}, fmt.Errorf("archive mutation requires a %q command", wantCommand)
	}
	if command.ExpectedSequence == nil {
		return CommandResult{}, fmt.Errorf("expected_sequence is required for %s", wantCommand)
	}
	if err := store.ensureThreadCatalog(ctx); err != nil {
		return CommandResult{}, err
	}
	requestPayload, _ := json.Marshal(struct {
		Archived bool `json:"archived"`
	}{archived})
	requestHash, err := hashRequest(command, []EventDraft{{Type: eventType, Visibility: agoprotocol.VisibilityUser, Payload: requestPayload}})
	if err != nil {
		return CommandResult{}, err
	}

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return CommandResult{}, fmt.Errorf("begin archive mutation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, err := commandResult(ctx, tx, command, requestHash); err != nil {
		return CommandResult{}, err
	} else if found {
		return existing, nil
	}

	var lastSequence uint64
	var archivedAt sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT t.last_sequence,c.archived_at
FROM threads t JOIN thread_catalog c ON c.thread_id=t.thread_id
WHERE t.thread_id=?`, command.ThreadID).Scan(&lastSequence, &archivedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CommandResult{}, fmt.Errorf("thread %q does not exist", command.ThreadID)
		}
		return CommandResult{}, fmt.Errorf("read archive state: %w", err)
	}
	if *command.ExpectedSequence != lastSequence {
		return CommandResult{}, ConflictError{CurrentSequence: lastSequence, ExpectedSequence: *command.ExpectedSequence}
	}
	if archivedAt.Valid == archived {
		return CommandResult{}, fmt.Errorf("thread %q is already %s", command.ThreadID, map[bool]string{true: "archived", false: "active"}[archived])
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	payload, _ := json.Marshal(struct {
		Archived  bool   `json:"archived"`
		ChangedAt string `json:"changed_at"`
	}{archived, now})
	eventID, err := randomID("E-")
	if err != nil {
		return CommandResult{}, err
	}
	event := agoprotocol.Event{SchemaVersion: agoprotocol.SchemaVersion, EventID: eventID, ThreadID: command.ThreadID, Sequence: lastSequence + 1, Type: eventType, Visibility: agoprotocol.VisibilityUser, Payload: payload}
	if err := event.Validate(); err != nil {
		return CommandResult{}, err
	}
	if err := insertEvent(ctx, tx, event); err != nil {
		return CommandResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET last_sequence=? WHERE thread_id=?`, event.Sequence, command.ThreadID); err != nil {
		return CommandResult{}, fmt.Errorf("advance archive sequence: %w", err)
	}
	var catalogResult sql.Result
	if archived {
		catalogResult, err = tx.ExecContext(ctx, `UPDATE thread_catalog SET archived_at=?,updated_at=?,revision=revision+1 WHERE thread_id=? AND archived_at IS NULL`, now, now, command.ThreadID)
	} else {
		catalogResult, err = tx.ExecContext(ctx, `UPDATE thread_catalog SET archived_at=NULL,updated_at=?,revision=revision+1 WHERE thread_id=? AND archived_at IS NOT NULL`, now, command.ThreadID)
	}
	if err != nil {
		return CommandResult{}, fmt.Errorf("update archive state: %w", err)
	}
	if changed, _ := catalogResult.RowsAffected(); changed != 1 {
		return CommandResult{}, fmt.Errorf("archive state changed concurrently")
	}
	result := CommandResult{ThreadID: command.ThreadID, LastSequence: event.Sequence, Events: []agoprotocol.Event{event}}
	if err := insertCommand(ctx, tx, command, requestHash, result); err != nil {
		return CommandResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return CommandResult{}, fmt.Errorf("commit archive mutation: %w", err)
	}
	return result, nil
}

func (store *Store) ensureThreadCatalog(ctx context.Context) error {
	if _, err := store.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS thread_catalog (
    thread_id TEXT PRIMARY KEY REFERENCES threads(thread_id),
    project_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    archived_at TEXT,
    revision INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1)
);
CREATE INDEX IF NOT EXISTS thread_catalog_project_archive_order
    ON thread_catalog (project_id, archived_at, updated_at DESC, thread_id DESC);
INSERT OR IGNORE INTO thread_catalog (thread_id,project_id,created_at,updated_at)
SELECT thread_id,COALESCE(json_extract(project_json, '$.project_id'),''),'',''
FROM threads;
`); err != nil {
		return fmt.Errorf("initialize thread catalog: %w", err)
	}
	return nil
}

func decodeThreadCatalogCursor(query ThreadCatalogQuery) (*threadCatalogCursor, error) {
	if query.Cursor == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(query.Cursor)
	if err != nil {
		return nil, fmt.Errorf("decode thread catalog cursor: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var cursor threadCatalogCursor
	if err := decoder.Decode(&cursor); err != nil {
		return nil, fmt.Errorf("decode thread catalog cursor: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode thread catalog cursor: %w", err)
	}
	if cursor.SchemaVersion != ThreadCatalogSchemaVersion || cursor.ProjectID != query.ProjectID || cursor.Search != strings.ToLower(query.Search) || cursor.Archive != query.Archive || cursor.ThreadID == "" {
		return nil, fmt.Errorf("thread catalog cursor does not match query")
	}
	return &cursor, nil
}

func encodeThreadCatalogCursor(query ThreadCatalogQuery, entry ThreadCatalogEntry) (string, error) {
	raw, err := json.Marshal(threadCatalogCursor{SchemaVersion: ThreadCatalogSchemaVersion, ProjectID: query.ProjectID, Search: strings.ToLower(query.Search), Archive: query.Archive, UpdatedAt: entry.UpdatedAt, ThreadID: entry.ThreadID})
	if err != nil {
		return "", fmt.Errorf("encode thread catalog cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}
