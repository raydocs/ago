package agothreadstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"claudexflow/internal/agoprotocol"
)

const MaxCompactionSummaryBytes = 64 << 10

type CompactionInput struct {
	ThroughSequence uint64 `json:"through_sequence"`
	Summary         string `json:"summary"`
}

type CompactionRecord struct {
	CompactionID    string `json:"compaction_id"`
	ThreadID        string `json:"thread_id"`
	EventSequence   uint64 `json:"event_sequence"`
	ThroughSequence uint64 `json:"through_sequence"`
	Summary         string `json:"summary"`
}

type ContextProjection struct {
	Compaction *CompactionRecord   `json:"compaction,omitempty"`
	Tail       []agoprotocol.Event `json:"tail"`
}

func (store *Store) RecordCompaction(ctx context.Context, command agoprotocol.Command, input CompactionInput) (CompactionRecord, error) {
	if err := command.Validate(); err != nil {
		return CompactionRecord{}, err
	}
	if command.Type != agoprotocol.CommandThreadCompact {
		return CompactionRecord{}, fmt.Errorf("RecordCompaction requires a %q command", agoprotocol.CommandThreadCompact)
	}
	if input.ThroughSequence == 0 || strings.TrimSpace(input.Summary) == "" {
		return CompactionRecord{}, fmt.Errorf("compaction boundary and summary are required")
	}
	if len(input.Summary) > MaxCompactionSummaryBytes {
		return CompactionRecord{}, fmt.Errorf("compaction summary exceeds %d bytes", MaxCompactionSummaryBytes)
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return CompactionRecord{}, fmt.Errorf("encode compaction: %w", err)
	}
	draft := EventDraft{Type: agoprotocol.EventCompactionRecorded, Visibility: agoprotocol.VisibilityAudit, Payload: payload}
	requestHash, err := hashRequest(command, []EventDraft{draft})
	if err != nil {
		return CompactionRecord{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return CompactionRecord{}, fmt.Errorf("begin compaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, err := commandResult(ctx, tx, command, requestHash); err != nil {
		return CompactionRecord{}, err
	} else if found {
		if len(existing.Events) != 1 {
			return CompactionRecord{}, fmt.Errorf("stored compaction result is malformed")
		}
		return CompactionRecord{CompactionID: existing.Events[0].EventID, ThreadID: command.ThreadID, EventSequence: existing.Events[0].Sequence, ThroughSequence: input.ThroughSequence, Summary: input.Summary}, nil
	}
	var lastSequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence FROM threads WHERE thread_id = ?`, command.ThreadID).Scan(&lastSequence); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return CompactionRecord{}, fmt.Errorf("thread %q does not exist", command.ThreadID)
		}
		return CompactionRecord{}, fmt.Errorf("read thread sequence: %w", err)
	}
	if input.ThroughSequence > lastSequence {
		return CompactionRecord{}, fmt.Errorf("compaction boundary %d exceeds thread sequence %d", input.ThroughSequence, lastSequence)
	}
	var previous uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(through_sequence), 0) FROM compactions WHERE thread_id = ?`, command.ThreadID).Scan(&previous); err != nil {
		return CompactionRecord{}, fmt.Errorf("read previous compaction: %w", err)
	}
	if input.ThroughSequence <= previous {
		return CompactionRecord{}, fmt.Errorf("compaction boundary %d does not advance past %d", input.ThroughSequence, previous)
	}
	eventID, err := randomID("E-")
	if err != nil {
		return CompactionRecord{}, err
	}
	event := agoprotocol.Event{SchemaVersion: agoprotocol.SchemaVersion, EventID: eventID, ThreadID: command.ThreadID, Sequence: lastSequence + 1, Type: draft.Type, Visibility: draft.Visibility, Payload: payload}
	if err := event.Validate(); err != nil {
		return CompactionRecord{}, err
	}
	if err := insertEvent(ctx, tx, event); err != nil {
		return CompactionRecord{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO compactions (thread_id, compaction_id, event_sequence, through_sequence, summary) VALUES (?, ?, ?, ?, ?)`, command.ThreadID, eventID, event.Sequence, input.ThroughSequence, input.Summary); err != nil {
		return CompactionRecord{}, fmt.Errorf("insert compaction: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET last_sequence = ? WHERE thread_id = ?`, event.Sequence, command.ThreadID); err != nil {
		return CompactionRecord{}, fmt.Errorf("update thread sequence: %w", err)
	}
	result := CommandResult{ThreadID: command.ThreadID, LastSequence: event.Sequence, Events: []agoprotocol.Event{event}}
	if err := insertCommand(ctx, tx, command, requestHash, result); err != nil {
		return CompactionRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return CompactionRecord{}, fmt.Errorf("commit compaction: %w", err)
	}
	return CompactionRecord{CompactionID: eventID, ThreadID: command.ThreadID, EventSequence: event.Sequence, ThroughSequence: input.ThroughSequence, Summary: input.Summary}, nil
}

func (store *Store) ContextProjection(ctx context.Context, threadID string) (ContextProjection, error) {
	var record CompactionRecord
	err := store.db.QueryRowContext(ctx, `SELECT compaction_id, event_sequence, through_sequence, summary FROM compactions WHERE thread_id = ? ORDER BY event_sequence DESC LIMIT 1`, threadID).Scan(&record.CompactionID, &record.EventSequence, &record.ThroughSequence, &record.Summary)
	after := uint64(0)
	var latest *CompactionRecord
	if err == nil {
		record.ThreadID = threadID
		after = record.ThroughSequence
		latest = &record
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ContextProjection{}, fmt.Errorf("read latest compaction: %w", err)
	}
	events, err := store.Replay(ctx, threadID, after, 0)
	if err != nil {
		return ContextProjection{}, err
	}
	tail := make([]agoprotocol.Event, 0, len(events))
	for _, event := range events {
		if event.Type != agoprotocol.EventCompactionRecorded {
			tail = append(tail, event)
		}
	}
	return ContextProjection{Compaction: latest, Tail: tail}, nil
}
