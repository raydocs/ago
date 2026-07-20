package agoboardstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"

	"claudexflow/internal/agoboardprotocol"
)

// SlotLimits bounds how much work may be in flight at once. Zero means the
// limit is not enforced.
type SlotLimits struct {
	// GlobalRunning caps active leases across every board.
	GlobalRunning int
	// BoardRunning caps active leases within one board.
	BoardRunning int
	// RepositoryWriters caps concurrent writers on one repository. A writer is
	// exclusive: it may not overlap any other lease on the same repository.
	RepositoryWriters int
	// RepositoryReaders caps concurrent readers on one repository.
	RepositoryReaders int
}

// ClaimRequest describes one attempt to take work. CommandID is the durable
// claim identity: replaying it can never claim a second task.
type ClaimRequest struct {
	BoardID       string
	CommandID     string
	Actor         agoboardprotocol.Actor
	WorkerID      string
	Now           time.Time
	LeaseDuration time.Duration
	Limits        SlotLimits
}

// ClaimOutcome explains why a cycle did or did not take work.
type ClaimOutcome string

const (
	ClaimAcquired ClaimOutcome = "acquired"
	// ClaimReplayed means this command already claimed work. The caller must
	// not dispatch: the original claimant owns the attempt.
	ClaimReplayed  ClaimOutcome = "replayed"
	ClaimNoWork    ClaimOutcome = "no-work"
	ClaimPaused    ClaimOutcome = "paused"
	ClaimNoSlot    ClaimOutcome = "no-slot"
	ClaimBackedOff ClaimOutcome = "backing-off"
)

type ClaimResult struct {
	Outcome ClaimOutcome
	Board   agoboardprotocol.Board
	Events  []agoboardprotocol.Event
	TaskID  string
	// AttemptID and FencingToken are set only for ClaimAcquired. They are the
	// credentials the dispatched executor must present.
	AttemptID string
	LeaseID   string
	// Generation orders this attempt against every other one on the board.
	Generation   uint64
	FencingToken string
	Reason       string
}

// Dispatchable reports whether this caller owns fresh work and may run it.
// Only a claim this call created returns true, so a replay never re-executes.
func (result ClaimResult) Dispatchable() bool {
	return result.Outcome == ClaimAcquired && result.AttemptID != ""
}

// Claim atomically takes at most one eligible task.
//
// The receipt check, readiness re-confirmation, slot accounting, generation
// allocation, fencing-token minting, attempt and lease creation, and the
// durable receipt all happen inside one immediate transaction. Nothing about
// the decision is computed outside it, so two schedulers racing on the same
// task cannot both observe a free slot, and a crash-retry of the same command
// ID cannot mint a second token or claim a second task.
//
// The caller may dispatch only when Dispatchable reports true, and only after
// this call has returned, which is after the transaction committed.
func (s *Store) Claim(ctx context.Context, request ClaimRequest) (ClaimResult, error) {
	if request.BoardID == "" || request.CommandID == "" || request.WorkerID == "" {
		return ClaimResult{}, fmt.Errorf("claim requires board id, command id, and worker id")
	}
	if request.Actor.Role != agoboardprotocol.RoleCoordinator {
		return ClaimResult{}, fmt.Errorf("only a coordinator may claim work")
	}
	if request.Now.IsZero() || request.LeaseDuration <= 0 {
		return ClaimResult{}, fmt.Errorf("claim requires a clock reading and a positive lease duration")
	}
	// The hash deliberately excludes the fencing token: the token is minted
	// inside the transaction, so a retry of the same command hashes identically
	// and hits the stored receipt instead of colliding with it.
	hash, err := requestHash(struct {
		BoardID  string                 `json:"board_id"`
		Actor    agoboardprotocol.Actor `json:"actor"`
		WorkerID string                 `json:"worker_id"`
		Limits   SlotLimits             `json:"limits"`
	}{request.BoardID, request.Actor, request.WorkerID, request.Limits})
	if err != nil {
		return ClaimResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("begin claim: %w", err)
	}
	defer tx.Rollback()

	if stored, found, err := storedResult(ctx, tx, request.Actor.ID, request.CommandID, hash); err != nil {
		return ClaimResult{}, err
	} else if found {
		if stored.Board.ID != request.BoardID {
			return ClaimResult{}, fmt.Errorf("claim command %q already claimed on board %q: %w", request.CommandID, stored.Board.ID, ErrCommandConflict)
		}
		return ClaimResult{Outcome: ClaimReplayed, Board: stored.Board, Reason: "claim command was already honoured"}, nil
	}

	board, err := boardTx(ctx, tx, request.BoardID)
	if err != nil {
		return ClaimResult{}, err
	}
	if board.Paused {
		return ClaimResult{Outcome: ClaimPaused, Board: board, Reason: board.PauseReason}, nil
	}

	task, eligible, reason := eligibleTask(board, request.Now)
	if !eligible {
		return ClaimResult{Outcome: ClaimNoWork, Board: board, Reason: reason}, nil
	}
	if backingOff(board, request.Now) && task.ID == "" {
		return ClaimResult{Outcome: ClaimBackedOff, Board: board}, nil
	}

	// Slots are counted from committed rows under the write lock, never from
	// process memory, so a second scheduler in a second process sees the same
	// numbers this one does.
	if allowed, slotReason, err := s.slotAvailable(ctx, tx, board, task, request.Limits); err != nil {
		return ClaimResult{}, err
	} else if !allowed {
		return ClaimResult{Outcome: ClaimNoSlot, Board: board, TaskID: task.ID, Reason: slotReason}, nil
	}

	token, err := newFencingToken()
	if err != nil {
		return ClaimResult{}, err
	}
	attemptNumber := task.AttemptCount + 1
	attemptID := derivedClaimID("attempt", board.ID, task.ID, attemptNumber)
	leaseID := derivedClaimID("lease", board.ID, task.ID, attemptNumber)
	next, events, err := agoboardprotocol.Apply(board, agoboardprotocol.Command{
		SchemaVersion:   agoboardprotocol.SchemaVersion,
		ID:              request.CommandID,
		ExpectedVersion: board.Version,
		Actor:           request.Actor,
		Type:            agoboardprotocol.CommandLeaseAcquire,
		Lease: &agoboardprotocol.LeaseSpec{
			ID: leaseID, TaskID: task.ID, AttemptID: attemptID, WorkerID: request.WorkerID,
			FencingToken: token,
			AcquiredAt:   request.Now.UTC(),
			ExpiresAt:    request.Now.UTC().Add(request.LeaseDuration),
		},
	})
	if err != nil {
		return ClaimResult{}, fmt.Errorf("claim task %q: %w", task.ID, err)
	}
	if err := updateBoard(ctx, tx, next); err != nil {
		return ClaimResult{}, err
	}
	if err := appendEvents(ctx, tx, events); err != nil {
		return ClaimResult{}, err
	}
	if err := syncProjection(ctx, tx, next); err != nil {
		return ClaimResult{}, err
	}
	if err := insertResult(ctx, tx, request.Actor.ID, request.CommandID, hash, next.ID, Result{Board: next, Events: events}); err != nil {
		return ClaimResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ClaimResult{}, fmt.Errorf("commit claim: %w", err)
	}
	return ClaimResult{
		Outcome: ClaimAcquired, Board: next, Events: events, TaskID: task.ID,
		AttemptID: attemptID, LeaseID: leaseID, Generation: generationOf(next, attemptID), FencingToken: token,
	}, nil
}

// eligibleTask picks the first task that may be claimed now. A retry-wait task
// only becomes eligible once its durable backoff deadline has passed.
func eligibleTask(board agoboardprotocol.Board, now time.Time) (agoboardprotocol.Task, bool, string) {
	waiting := false
	for _, task := range board.Tasks {
		switch task.State {
		case agoboardprotocol.TaskReady:
			if task.AttemptCount < agoboardprotocol.MaxAttempts {
				return task, true, ""
			}
		case agoboardprotocol.TaskRetryWait:
			if task.AttemptCount >= agoboardprotocol.MaxAttempts {
				continue
			}
			if !now.Before(task.NextEligibleAt) {
				return task, true, ""
			}
			waiting = true
		}
	}
	if waiting {
		return agoboardprotocol.Task{}, false, "all eligible work is waiting out its retry backoff"
	}
	return agoboardprotocol.Task{}, false, "no ready task"
}

func backingOff(board agoboardprotocol.Board, now time.Time) bool {
	for _, task := range board.Tasks {
		if task.State == agoboardprotocol.TaskRetryWait && now.Before(task.NextEligibleAt) {
			return true
		}
	}
	return false
}

// slotAvailable enforces the concurrency policy from committed rows.
//
// A repository writer is exclusive: it may not overlap any other lease on the
// same repository, and no other lease may start while it holds one. Readers run
// concurrently up to their own limit.
func (s *Store) slotAvailable(ctx context.Context, tx *sql.Tx, board agoboardprotocol.Board, task agoboardprotocol.Task, limits SlotLimits) (bool, string, error) {
	count := func(query string, args ...any) (int, error) {
		var value int
		err := tx.QueryRowContext(ctx, query, args...).Scan(&value)
		return value, err
	}
	if limits.GlobalRunning > 0 {
		total, err := count(`SELECT count(*) FROM leases WHERE state='active'`)
		if err != nil {
			return false, "", err
		}
		if total >= limits.GlobalRunning {
			return false, fmt.Sprintf("global running limit %d reached", limits.GlobalRunning), nil
		}
	}
	if limits.BoardRunning > 0 {
		total, err := count(`SELECT count(*) FROM leases WHERE state='active' AND board_id=?`, board.ID)
		if err != nil {
			return false, "", err
		}
		if total >= limits.BoardRunning {
			return false, fmt.Sprintf("board running limit %d reached", limits.BoardRunning), nil
		}
	}
	repository := board.Repository
	writers, err := count(`SELECT count(*) FROM leases WHERE state='active' AND repository_id=? AND access_mode='write'`, repository)
	if err != nil {
		return false, "", err
	}
	readers, err := count(`SELECT count(*) FROM leases WHERE state='active' AND repository_id=? AND access_mode='read'`, repository)
	if err != nil {
		return false, "", err
	}
	if task.AccessMode == agoboardprotocol.AccessWrite {
		// Exclusive: a writer waits for the repository to be completely quiet.
		if writers+readers > 0 {
			return false, "repository is busy; a writer needs exclusive access", nil
		}
		if limits.RepositoryWriters > 0 && writers >= limits.RepositoryWriters {
			return false, fmt.Sprintf("repository writer limit %d reached", limits.RepositoryWriters), nil
		}
		return true, "", nil
	}
	if writers > 0 {
		return false, "repository is held exclusively by a writer", nil
	}
	if limits.RepositoryReaders > 0 && readers >= limits.RepositoryReaders {
		return false, fmt.Sprintf("repository reader limit %d reached", limits.RepositoryReaders), nil
	}
	return true, "", nil
}

// newFencingToken returns an unpredictable single-use credential. It is minted
// inside the claim transaction so a replayed command never produces a new one.
func newFencingToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate fencing token: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

func derivedClaimID(namespace, boardID, taskID string, attemptNumber int) string {
	digest, _ := requestHash([]string{namespace, boardID, taskID, strconv.Itoa(attemptNumber)})
	return namespace + ":" + hex.EncodeToString(digest[:16])
}

// BoardIDs lists every board in creation-stable order.
func (s *Store) BoardIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT board_id FROM boards ORDER BY board_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CountActiveLeases reports how many leases are currently held, optionally
// scoped to one board. It is a read-only diagnostic for schedulers and tests;
// admission decisions are made inside Claim's transaction, never from this.
func (s *Store) CountActiveLeases(ctx context.Context, boardID string) (int, error) {
	query, args := `SELECT count(*) FROM leases WHERE state='active'`, []any{}
	if boardID != "" {
		query += ` AND board_id=?`
		args = append(args, boardID)
	}
	var value int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&value)
	return value, err
}

// CountAttempts reports how many attempts exist, optionally scoped to a board.
func (s *Store) CountAttempts(ctx context.Context, boardID string) (int, error) {
	query, args := `SELECT count(*) FROM attempts`, []any{}
	if boardID != "" {
		query += ` WHERE board_id=?`
		args = append(args, boardID)
	}
	var value int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&value)
	return value, err
}

// generationOf reads back the generation the state machine minted for an
// attempt. The counter lives in the aggregate, so this is the only place that
// learns it rather than assuming a value.
func generationOf(board agoboardprotocol.Board, attemptID string) uint64 {
	for _, attempt := range board.Attempts {
		if attempt.ID == attemptID {
			return attempt.Generation
		}
	}
	return 0
}
