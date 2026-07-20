// Package agoboardstore persists Work Graph protocol aggregates in SQLite.
package agoboardstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"claudexflow/internal/agoboardprotocol"
	_ "modernc.org/sqlite"
)

const CurrentSchemaVersion = 2

//go:embed migrations/001_initial.sql
var initialSchema string

type Store struct{ db *sql.DB }

type Result struct {
	Board  agoboardprotocol.Board   `json:"board"`
	Events []agoboardprotocol.Event `json:"events"`
}

type Binding struct {
	BoardID    string `json:"board_id"`
	AttemptID  string `json:"attempt_id"`
	ThreadID   string `json:"thread_id"`
	ExecutorID string `json:"executor_id,omitempty"`
}

type CompletionStatus string

const (
	CompletionInProgress CompletionStatus = "in-progress"
	CompletionPassed     CompletionStatus = "passed"
	CompletionFailed     CompletionStatus = "failed"
)

type Completion struct {
	Status    CompletionStatus `json:"status"`
	Passed    int              `json:"passed"`
	Failed    int              `json:"failed"`
	Remaining int              `json:"remaining"`
}

type LeaseCommand struct {
	ID              string                 `json:"id"`
	ExpectedVersion uint64                 `json:"expected_version"`
	Actor           agoboardprotocol.Actor `json:"actor"`
	BoardID         string                 `json:"board_id"`
	LeaseID         string                 `json:"lease_id"`
	ExpiresAt       time.Time              `json:"expires_at,omitempty"`
	Reason          string                 `json:"reason,omitempty"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, fmt.Errorf("open board store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func sqliteDSN(path string) string {
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	pragmas := url.Values{}
	pragmas.Add("_pragma", "foreign_keys(ON)")
	pragmas.Add("_pragma", "busy_timeout(5000)")
	pragmas.Add("_pragma", "synchronous(FULL)")
	return path + separator + pragmas.Encode()
}

func (s *Store) initialize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;`); err != nil {
		return fmt.Errorf("configure board store: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin board store migration: %w", err)
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > CurrentSchemaVersion {
		return fmt.Errorf("board store schema version %d is newer than supported version %d", version, CurrentSchemaVersion)
	}
	if version == 0 {
		if _, err := tx.ExecContext(ctx, initialSchema); err != nil {
			return fmt.Errorf("initialize board store schema: %w", err)
		}
		version = CurrentSchemaVersion
	}
	if version == 1 {
		if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS board_definitions (
board_id TEXT PRIMARY KEY REFERENCES boards(board_id) ON DELETE CASCADE,
definition_json BLOB NOT NULL
)`); err != nil {
			return fmt.Errorf("migrate board definitions: %w", err)
		}
		version = 2
	}
	if version == CurrentSchemaVersion {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, version)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit board store migration: %w", err)
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v)
	return v, err
}

// Apply atomically applies a protocol command, appends its events, and stores
// the exact result for command retries. Lease acquisition without a deadline is
// supported for callers that do not require expiry.
func (s *Store) Apply(ctx context.Context, command agoboardprotocol.Command) (Result, error) {
	return s.apply(ctx, "", command, time.Time{}, nil, nil)
}

// ApplyBoard applies a command to the named board. It is required when a store
// contains multiple boards because protocol commands use board-local entity IDs.
func (s *Store) ApplyBoard(ctx context.Context, boardID string, command agoboardprotocol.Command) (Result, error) {
	return s.apply(ctx, boardID, command, time.Time{}, nil, nil)
}

// Create is the explicit board.create form of Apply.
func (s *Store) Create(ctx context.Context, command agoboardprotocol.Command) (Result, error) {
	if command.Type != agoboardprotocol.CommandBoardCreate {
		return Result{}, fmt.Errorf("Create requires board.create command")
	}
	return s.Apply(ctx, command)
}

// CreateGraph validates and persists a complete initial graph and its planner
// definition in one transaction. If any protocol command fails, no board,
// projection, definition, event, or command receipt is committed.
func (s *Store) CreateGraph(ctx context.Context, commands []agoboardprotocol.Command, definition json.RawMessage) (Result, error) {
	if len(commands) == 0 || commands[0].Type != agoboardprotocol.CommandBoardCreate || commands[0].Board == nil {
		return Result{}, fmt.Errorf("CreateGraph requires board.create followed by graph commands")
	}
	if len(definition) == 0 || !json.Valid(definition) {
		return Result{}, fmt.Errorf("valid graph definition is required")
	}
	actorID := commands[0].Actor.ID
	commandIDs := make(map[string]struct{}, len(commands))
	for _, command := range commands {
		if command.Actor.ID != actorID || command.Actor.Role != agoboardprotocol.RoleCoordinator {
			return Result{}, fmt.Errorf("graph admission requires one coordinator actor")
		}
		if command.ID == "" {
			return Result{}, fmt.Errorf("graph command id is required")
		}
		if _, exists := commandIDs[command.ID]; exists {
			return Result{}, fmt.Errorf("duplicate graph command id %q", command.ID)
		}
		commandIDs[command.ID] = struct{}{}
	}
	boardID := commands[0].Board.ID
	hash, err := requestHash(struct {
		BoardID    string                     `json:"board_id"`
		Commands   []agoboardprotocol.Command `json:"commands"`
		Definition json.RawMessage            `json:"definition"`
	}{boardID, commands, definition})
	if err != nil {
		return Result{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin graph admission: %w", err)
	}
	defer tx.Rollback()
	if result, found, err := storedResult(ctx, tx, actorID, commands[0].ID, hash); err != nil {
		return Result{}, err
	} else if found {
		return result, nil
	}
	type innerReceipt struct {
		command agoboardprotocol.Command
		hash    []byte
		result  Result
	}
	receipts := make([]innerReceipt, 0, len(commands)-1)
	for _, command := range commands[1:] {
		commandHash, err := commandRequestHash(boardID, command, time.Time{}, nil)
		if err != nil {
			return Result{}, err
		}
		if _, found, err := storedResult(ctx, tx, actorID, command.ID, commandHash); err != nil {
			return Result{}, err
		} else if found {
			return Result{}, fmt.Errorf("graph command %q was already used", command.ID)
		}
		receipts = append(receipts, innerReceipt{command: command, hash: commandHash})
	}
	current := agoboardprotocol.Board{}
	events := make([]agoboardprotocol.Event, 0, len(commands))
	for index, command := range commands {
		next, emitted, err := agoboardprotocol.Apply(current, command)
		if err != nil {
			return Result{}, fmt.Errorf("apply graph command %q: %w", command.ID, err)
		}
		if next.ID != boardID {
			return Result{}, fmt.Errorf("graph command %q changed board identity", command.ID)
		}
		current = next
		events = append(events, emitted...)
		if index > 0 {
			receipts[index-1].result = Result{Board: next, Events: emitted}
		}
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		return Result{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO boards(board_id,version,title,board_json) VALUES(?,?,?,?)`, current.ID, current.Version, current.Title, encoded); err != nil {
		return Result{}, fmt.Errorf("insert admitted board: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO board_definitions(board_id,definition_json) VALUES(?,?)`, current.ID, []byte(definition)); err != nil {
		return Result{}, fmt.Errorf("insert graph definition: %w", err)
	}
	if err := appendEvents(ctx, tx, events); err != nil {
		return Result{}, err
	}
	if err := syncProjection(ctx, tx, current); err != nil {
		return Result{}, err
	}
	for _, receipt := range receipts {
		if err := insertResult(ctx, tx, actorID, receipt.command.ID, receipt.hash, current.ID, receipt.result); err != nil {
			return Result{}, err
		}
	}
	result := Result{Board: current, Events: events}
	if err := insertResult(ctx, tx, actorID, commands[0].ID, hash, current.ID, result); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit graph admission: %w", err)
	}
	return result, nil
}

// Definition decodes the immutable planner definition admitted with a board.
func (s *Store) Definition(ctx context.Context, boardID string, target any) error {
	if strings.TrimSpace(boardID) == "" || target == nil {
		return fmt.Errorf("board id and definition target are required")
	}
	var encoded []byte
	if err := s.db.QueryRowContext(ctx, `SELECT definition_json FROM board_definitions WHERE board_id=?`, boardID).Scan(&encoded); err != nil {
		return fmt.Errorf("read board definition: %w", err)
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return fmt.Errorf("decode board definition: %w", err)
	}
	return nil
}

// AcquireLease atomically competes for a ready task and records its deadline
// and optional attempt/thread binding in the same transaction.
func (s *Store) AcquireLease(ctx context.Context, command agoboardprotocol.Command, expiresAt time.Time, binding *Binding) (Result, error) {
	if command.Type != agoboardprotocol.CommandLeaseAcquire {
		return Result{}, fmt.Errorf("AcquireLease requires lease.acquire command")
	}
	if expiresAt.IsZero() {
		return Result{}, fmt.Errorf("lease expiry is required")
	}
	boardID := ""
	if binding != nil {
		boardID = binding.BoardID
	}
	return s.apply(ctx, boardID, command, expiresAt.UTC(), binding, nil)
}

// AcquireLeaseBoard is the multi-board form of AcquireLease.
func (s *Store) AcquireLeaseBoard(ctx context.Context, boardID string, command agoboardprotocol.Command, expiresAt time.Time, binding *Binding) (Result, error) {
	if command.Type != agoboardprotocol.CommandLeaseAcquire {
		return Result{}, fmt.Errorf("AcquireLeaseBoard requires lease.acquire command")
	}
	if boardID == "" || expiresAt.IsZero() {
		return Result{}, fmt.Errorf("board id and lease expiry are required")
	}
	return s.apply(ctx, boardID, command, expiresAt.UTC(), binding, nil)
}

// AcquireLeaseBoardOnce reports whether this caller created the durable lease
// receipt. Exact command replays receive the stored result with fresh=false so
// callers do not repeat executor side effects.
func (s *Store) AcquireLeaseBoardOnce(ctx context.Context, boardID string, command agoboardprotocol.Command, expiresAt time.Time, binding *Binding) (Result, bool, error) {
	if command.Type != agoboardprotocol.CommandLeaseAcquire {
		return Result{}, false, fmt.Errorf("AcquireLeaseBoardOnce requires lease.acquire command")
	}
	if boardID == "" || expiresAt.IsZero() {
		return Result{}, false, fmt.Errorf("board id and lease expiry are required")
	}
	fresh := false
	result, err := s.apply(ctx, boardID, command, expiresAt.UTC(), binding, &fresh)
	return result, fresh, err
}

func (s *Store) apply(ctx context.Context, boardID string, command agoboardprotocol.Command, expiresAt time.Time, binding *Binding, fresh *bool) (Result, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin board command: %w", err)
	}
	defer tx.Rollback()
	if command.Type == agoboardprotocol.CommandBoardCreate {
		if command.Board != nil {
			boardID = command.Board.ID
		}
	} else if boardID == "" {
		boardID, err = soleBoardID(ctx, tx)
		if err != nil {
			return Result{}, err
		}
	}
	hash, err := commandRequestHash(boardID, command, expiresAt, binding)
	if err != nil {
		return Result{}, err
	}
	if result, found, err := storedResult(ctx, tx, command.Actor.ID, command.ID, hash); err != nil {
		return Result{}, err
	} else if found {
		if fresh != nil {
			*fresh = false
		}
		return result, nil
	}

	var current agoboardprotocol.Board
	if command.Type != agoboardprotocol.CommandBoardCreate {
		current, err = boardTx(ctx, tx, boardID)
		if err != nil {
			return Result{}, err
		}
	}
	next, events, err := agoboardprotocol.Apply(current, command)
	if err != nil {
		return Result{}, err
	}
	if command.Type == agoboardprotocol.CommandBoardCreate {
		encoded, _ := json.Marshal(next)
		if _, err := tx.ExecContext(ctx, `INSERT INTO boards(board_id,version,title,board_json) VALUES(?,?,?,?)`, next.ID, next.Version, next.Title, encoded); err != nil {
			return Result{}, fmt.Errorf("insert board: %w", err)
		}
	} else if err := updateBoard(ctx, tx, next); err != nil {
		return Result{}, err
	}
	if err := appendEvents(ctx, tx, events); err != nil {
		return Result{}, err
	}
	if err := syncProjection(ctx, tx, next); err != nil {
		return Result{}, err
	}
	if command.Lease != nil && !expiresAt.IsZero() {
		if _, err := tx.ExecContext(ctx, `UPDATE leases SET expires_at=? WHERE board_id=? AND lease_id=?`, expiresAt.UnixNano(), next.ID, command.Lease.ID); err != nil {
			return Result{}, err
		}
	}
	if binding != nil {
		if command.Lease == nil || binding.BoardID != next.ID || binding.AttemptID != command.Lease.AttemptID || binding.ThreadID == "" {
			return Result{}, fmt.Errorf("binding does not match acquired attempt")
		}
		encoded, _ := json.Marshal(binding)
		if _, err := tx.ExecContext(ctx, `INSERT INTO bindings(board_id,attempt_id,thread_id,executor_id,binding_json) VALUES(?,?,?,?,?)`, binding.BoardID, binding.AttemptID, binding.ThreadID, binding.ExecutorID, encoded); err != nil {
			return Result{}, fmt.Errorf("insert binding: %w", err)
		}
	}
	result := Result{Board: next, Events: events}
	if err := insertResult(ctx, tx, command.Actor.ID, command.ID, hash, next.ID, result); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit board command: %w", err)
	}
	if fresh != nil {
		*fresh = true
	}
	return result, nil
}

func soleBoardID(ctx context.Context, tx *sql.Tx) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT board_id FROM boards ORDER BY board_id LIMIT 2`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("board id is required when store contains %d boards", len(ids))
	}
	return ids[0], nil
}

func (s *Store) Board(ctx context.Context, boardID string) (agoboardprotocol.Board, error) {
	return boardQuery(ctx, s.db, boardID)
}

func boardQuery(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id string) (agoboardprotocol.Board, error) {
	var encoded []byte
	if err := q.QueryRowContext(ctx, `SELECT board_json FROM boards WHERE board_id=?`, id).Scan(&encoded); errors.Is(err, sql.ErrNoRows) {
		return agoboardprotocol.Board{}, fmt.Errorf("board %q does not exist", id)
	} else if err != nil {
		return agoboardprotocol.Board{}, err
	}
	var board agoboardprotocol.Board
	if err := json.Unmarshal(encoded, &board); err != nil {
		return board, fmt.Errorf("decode board: %w", err)
	}
	return board, board.Validate()
}
func boardTx(ctx context.Context, tx *sql.Tx, id string) (agoboardprotocol.Board, error) {
	return boardQuery(ctx, tx, id)
}

func (s *Store) Replay(ctx context.Context, boardID string, afterVersion uint64, limit int) ([]agoboardprotocol.Event, error) {
	if limit < 0 {
		return nil, fmt.Errorf("limit cannot be negative")
	}
	query, args := `SELECT event_json FROM events WHERE board_id=? AND version>? ORDER BY version`, []any{boardID, afterVersion}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []agoboardprotocol.Event{}
	for rows.Next() {
		var b []byte
		var e agoboardprotocol.Event
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &e); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func (s *Store) Ready(ctx context.Context, boardID string) ([]agoboardprotocol.Task, error) {
	board, err := s.Board(ctx, boardID)
	if err != nil {
		return nil, err
	}
	ready := []agoboardprotocol.Task{}
	for _, task := range board.Tasks {
		if task.State == agoboardprotocol.TaskReady {
			ready = append(ready, task)
		}
	}
	return ready, nil
}

func (s *Store) Completion(ctx context.Context, boardID string) (Completion, error) {
	board, err := s.Board(ctx, boardID)
	if err != nil {
		return Completion{}, err
	}
	c := Completion{Status: CompletionInProgress}
	for _, task := range board.Tasks {
		switch task.State {
		case agoboardprotocol.TaskPassed:
			c.Passed++
		case agoboardprotocol.TaskFailed:
			c.Failed++
		default:
			c.Remaining++
		}
	}
	if c.Failed > 0 {
		c.Status = CompletionFailed
	} else if len(board.Tasks) > 0 && c.Remaining == 0 {
		c.Status = CompletionPassed
	}
	return c, nil
}

func (s *Store) Binding(ctx context.Context, boardID, attemptID string) (Binding, error) {
	var encoded []byte
	if err := s.db.QueryRowContext(ctx, `SELECT binding_json FROM bindings WHERE board_id=? AND attempt_id=?`, boardID, attemptID).Scan(&encoded); err != nil {
		return Binding{}, err
	}
	var value Binding
	err := json.Unmarshal(encoded, &value)
	return value, err
}

func (s *Store) RenewLease(ctx context.Context, command LeaseCommand) (Result, error) {
	if command.ExpiresAt.IsZero() {
		return Result{}, fmt.Errorf("lease expiry is required")
	}
	result, _, err := s.mutateLease(ctx, command, false, nil)
	return result, err
}

func (s *Store) ExpireLease(ctx context.Context, command LeaseCommand) (Result, error) {
	result, _, err := s.mutateLease(ctx, command, true, nil)
	return result, err
}

func (s *Store) mutateLease(ctx context.Context, command LeaseCommand, expire bool, expectedExpiry *int64) (Result, bool, error) {
	if command.ID == "" || command.BoardID == "" || command.LeaseID == "" || command.Actor.ID == "" || command.Actor.Role != agoboardprotocol.RoleCoordinator {
		return Result{}, false, fmt.Errorf("valid coordinator lease command is required")
	}
	hash, _ := requestHash(struct {
		Command LeaseCommand `json:"command"`
		Expire  bool         `json:"expire"`
	}{command, expire})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, false, err
	}
	defer tx.Rollback()
	if result, found, err := storedResult(ctx, tx, command.Actor.ID, command.ID, hash); err != nil {
		return Result{}, false, err
	} else if found {
		return result, true, nil
	}
	board, err := boardTx(ctx, tx, command.BoardID)
	if err != nil {
		return Result{}, false, err
	}
	if expectedExpiry != nil {
		var state string
		var currentExpiry int64
		err := tx.QueryRowContext(ctx, `SELECT state,expires_at FROM leases WHERE board_id=? AND lease_id=?`, command.BoardID, command.LeaseID).Scan(&state, &currentExpiry)
		if errors.Is(err, sql.ErrNoRows) {
			return Result{}, false, nil
		}
		if err != nil {
			return Result{}, false, err
		}
		if state != string(agoboardprotocol.LeaseActive) || currentExpiry != *expectedExpiry {
			return Result{}, false, nil
		}
	}
	if board.Version != command.ExpectedVersion {
		if expectedExpiry != nil {
			return Result{}, false, nil
		}
		return Result{}, false, fmt.Errorf("expected board version %d, got %d", command.ExpectedVersion, board.Version)
	}
	leaseIndex, taskIndex, attemptIndex := -1, -1, -1
	for i := range board.Leases {
		if board.Leases[i].ID == command.LeaseID {
			leaseIndex = i
		}
	}
	if leaseIndex < 0 || board.Leases[leaseIndex].State != agoboardprotocol.LeaseActive {
		return Result{}, false, fmt.Errorf("active lease %q not found", command.LeaseID)
	}
	lease := &board.Leases[leaseIndex]
	for i := range board.Tasks {
		if board.Tasks[i].ID == lease.TaskID {
			taskIndex = i
		}
	}
	for i := range board.Attempts {
		if board.Attempts[i].ID == lease.AttemptID {
			attemptIndex = i
		}
	}
	if taskIndex < 0 || attemptIndex < 0 {
		return Result{}, false, fmt.Errorf("lease graph is incomplete")
	}
	eventType := agoboardprotocol.EventType("lease.renewed")
	if expire {
		eventType = agoboardprotocol.EventType("lease.expired")
		lease.State = agoboardprotocol.LeaseCompleted
		board.Attempts[attemptIndex].State = agoboardprotocol.AttemptFailed
		board.Tasks[taskIndex].State = agoboardprotocol.TaskReady
		board.Tasks[taskIndex].ActiveAttemptID, board.Tasks[taskIndex].ActiveLeaseID = "", ""
	}
	board.Version++
	event := agoboardprotocol.Event{SchemaVersion: agoboardprotocol.SchemaVersion, ID: fmt.Sprintf("%s:1", command.ID), CommandID: command.ID, BoardID: board.ID, Version: board.Version, Actor: command.Actor, Type: eventType, Task: &board.Tasks[taskIndex], Attempt: &board.Attempts[attemptIndex], Lease: lease, Reason: command.Reason}
	if err := updateBoard(ctx, tx, board); err != nil {
		return Result{}, false, err
	}
	if err := appendEvents(ctx, tx, []agoboardprotocol.Event{event}); err != nil {
		return Result{}, false, err
	}
	expires := command.ExpiresAt.UTC()
	if expire {
		expires = time.Time{}
	}
	if err := syncProjection(ctx, tx, board); err != nil {
		return Result{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE leases SET expires_at=? WHERE board_id=? AND lease_id=?`, timeNanos(expires), board.ID, command.LeaseID); err != nil {
		return Result{}, false, err
	}
	result := Result{Board: board, Events: []agoboardprotocol.Event{event}}
	if err := insertResult(ctx, tx, command.Actor.ID, command.ID, hash, board.ID, result); err != nil {
		return Result{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Result{}, false, err
	}
	return result, true, nil
}

// ExpireDueLeases durably expires all leases whose deadlines are at or before
// now. Each expiry has a deterministic command identity, making crash retries
// harmless. The returned results are ordered by lease ID.
func (s *Store) ExpireDueLeases(ctx context.Context, now time.Time, actor agoboardprotocol.Actor) ([]Result, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT board_id,lease_id,expires_at FROM leases WHERE state='active' AND expires_at>0 AND expires_at<=? ORDER BY board_id,lease_id`, now.UTC().UnixNano())
	if err != nil {
		return nil, err
	}
	type due struct {
		board, lease string
		expiry       int64
	}
	var values []due
	for rows.Next() {
		var d due
		if err := rows.Scan(&d.board, &d.lease, &d.expiry); err != nil {
			rows.Close()
			return nil, err
		}
		values = append(values, d)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(values))
	for _, d := range values {
		board, err := s.Board(ctx, d.board)
		if err != nil {
			return nil, err
		}
		identity := expiryCommandID(d.board, d.lease, d.expiry)
		result, expired, err := s.mutateLease(ctx, LeaseCommand{ID: identity, ExpectedVersion: board.Version, Actor: actor, BoardID: d.board, LeaseID: d.lease, Reason: "lease deadline elapsed"}, true, &d.expiry)
		if err != nil {
			return nil, err
		}
		if expired {
			results = append(results, result)
		}
	}
	return results, nil
}

func expiryCommandID(boardID, leaseID string, expiry int64) string {
	encoded, _ := json.Marshal(struct {
		BoardID string `json:"board_id"`
		LeaseID string `json:"lease_id"`
		Expiry  int64  `json:"expiry"`
	}{boardID, leaseID, expiry})
	digest := sha256.Sum256(encoded)
	return "expire:" + hex.EncodeToString(digest[:])
}

func updateBoard(ctx context.Context, tx *sql.Tx, board agoboardprotocol.Board) error {
	encoded, _ := json.Marshal(board)
	result, err := tx.ExecContext(ctx, `UPDATE boards SET version=?,title=?,board_json=? WHERE board_id=?`, board.Version, board.Title, encoded, board.ID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return fmt.Errorf("board %q does not exist", board.ID)
	}
	return nil
}
func appendEvents(ctx context.Context, tx *sql.Tx, events []agoboardprotocol.Event) error {
	for _, event := range events {
		encoded, _ := json.Marshal(event)
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(board_id,version,event_id,event_json) VALUES(?,?,?,?)`, event.BoardID, event.Version, event.ID, encoded); err != nil {
			return err
		}
	}
	return nil
}

func syncProjection(ctx context.Context, tx *sql.Tx, board agoboardprotocol.Board) error {
	for _, value := range board.Tasks {
		encoded, _ := json.Marshal(value)
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks VALUES(?,?,?,?) ON CONFLICT(board_id,task_id) DO UPDATE SET state=excluded.state,task_json=excluded.task_json`, board.ID, value.ID, value.State, encoded); err != nil {
			return err
		}
	}
	for _, value := range board.Dependencies {
		encoded, _ := json.Marshal(value)
		if _, err := tx.ExecContext(ctx, `INSERT INTO dependencies VALUES(?,?,?,?,?) ON CONFLICT(board_id,dependency_id) DO UPDATE SET dependency_json=excluded.dependency_json`, board.ID, value.ID, value.TaskID, value.DependsOn, encoded); err != nil {
			return err
		}
	}
	for _, value := range board.Attempts {
		encoded, _ := json.Marshal(value)
		if _, err := tx.ExecContext(ctx, `INSERT INTO attempts VALUES(?,?,?,?,?,?) ON CONFLICT(board_id,attempt_id) DO UPDATE SET state=excluded.state,attempt_json=excluded.attempt_json`, board.ID, value.ID, value.TaskID, value.WorkerID, value.State, encoded); err != nil {
			return err
		}
	}
	for _, value := range board.Leases {
		encoded, _ := json.Marshal(value)
		if _, err := tx.ExecContext(ctx, `INSERT INTO leases VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(board_id,lease_id) DO UPDATE SET state=excluded.state,lease_json=excluded.lease_json`, board.ID, value.ID, value.TaskID, value.AttemptID, value.WorkerID, value.State, 0, encoded); err != nil {
			return err
		}
	}
	for _, value := range board.Evidence {
		encoded, _ := json.Marshal(value)
		if _, err := tx.ExecContext(ctx, `INSERT INTO evidence VALUES(?,?,?,?,?,?) ON CONFLICT(board_id,evidence_id) DO UPDATE SET state=excluded.state,evidence_json=excluded.evidence_json`, board.ID, value.ID, value.TaskID, value.AttemptID, value.State, encoded); err != nil {
			return err
		}
	}
	return nil
}

func timeNanos(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UnixNano()
}

func requestHash(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	return digest[:], nil
}

func commandRequestHash(boardID string, command agoboardprotocol.Command, expiresAt time.Time, binding *Binding) ([]byte, error) {
	return requestHash(struct {
		BoardID   string                   `json:"board_id"`
		Command   agoboardprotocol.Command `json:"command"`
		ExpiresAt time.Time                `json:"expires_at,omitempty"`
		Binding   *Binding                 `json:"binding,omitempty"`
	}{boardID, command, expiresAt, binding})
}

func storedResult(ctx context.Context, tx *sql.Tx, actor, id string, hash []byte) (Result, bool, error) {
	var stored, encoded []byte
	err := tx.QueryRowContext(ctx, `SELECT request_hash,result_json FROM commands WHERE actor_id=? AND command_id=?`, actor, id).Scan(&stored, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	if !bytes.Equal(stored, hash) {
		return Result{}, false, fmt.Errorf("command %q was already used for different content", id)
	}
	var result Result
	if err := json.Unmarshal(encoded, &result); err != nil {
		return Result{}, false, err
	}
	return result, true, nil
}
func insertResult(ctx context.Context, tx *sql.Tx, actor, id string, hash []byte, boardID string, result Result) error {
	encoded, _ := json.Marshal(result)
	_, err := tx.ExecContext(ctx, `INSERT INTO commands VALUES(?,?,?,?,?)`, actor, id, hash, boardID, encoded)
	return err
}
