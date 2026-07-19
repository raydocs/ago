package agothreadstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"claudexflow/internal/agoprotocol"
)

type MessageInput struct {
	Content json.RawMessage        `json:"content"`
	Class   agoprotocol.QueueClass `json:"class"`
}

type QueueItem struct {
	QueueItemID string                     `json:"queue_item_id"`
	Position    uint64                     `json:"position"`
	Class       agoprotocol.QueueClass     `json:"class"`
	State       agoprotocol.QueueItemState `json:"state"`
	Content     json.RawMessage            `json:"content"`
}

type MailboxState struct {
	ThreadID        string               `json:"thread_id"`
	LastSequence    uint64               `json:"last_sequence"`
	Activity        agoprotocol.Activity `json:"activity"`
	ActiveTurnID    string               `json:"active_turn_id,omitempty"`
	CancelRequested bool                 `json:"cancel_requested"`
	Queue           []QueueItem          `json:"queue"`
	Events          []agoprotocol.Event  `json:"events,omitempty"`
}

type ConflictError struct {
	CurrentSequence  uint64
	ExpectedSequence uint64
}

func (conflict ConflictError) Error() string {
	return fmt.Sprintf("thread sequence is %d, expected %d", conflict.CurrentSequence, conflict.ExpectedSequence)
}

type StaleTurnError struct {
	ActiveTurnID   string
	ExpectedTurnID string
}

func (stale StaleTurnError) Error() string {
	return fmt.Sprintf("active turn is %q, expected %q", stale.ActiveTurnID, stale.ExpectedTurnID)
}

func (store *Store) Submit(ctx context.Context, command agoprotocol.Command, input MessageInput) (MailboxState, error) {
	if err := validateMailboxCommand(command, agoprotocol.CommandMessageSubmit); err != nil {
		return MailboxState{}, err
	}
	if err := input.Class.Validate(); err != nil {
		return MailboxState{}, err
	}
	return store.mutateMailbox(ctx, command, input, func(ctx context.Context, tx *sql.Tx, state *MailboxState) ([]EventDraft, error) {
		queueItemID, err := randomID("Q-")
		if err != nil {
			return nil, err
		}
		if state.Activity == agoprotocol.ActivityIdle {
			turnID, err := randomID("R-")
			if err != nil {
				return nil, err
			}
			state.Activity = agoprotocol.ActivityRunning
			state.ActiveTurnID = turnID
			return []EventDraft{
				mailboxEvent(agoprotocol.EventMessageAccepted, queueItemID, turnID, input.Content),
				mailboxEvent(agoprotocol.EventTurnStarted, queueItemID, turnID, nil),
			}, nil
		}
		position, err := nextQueuePosition(ctx, tx, command.ThreadID)
		if err != nil {
			return nil, err
		}
		item := QueueItem{QueueItemID: queueItemID, Position: position, Class: input.Class, State: agoprotocol.QueueItemPending, Content: cloneRawMessage(input.Content)}
		if err := insertQueueItem(ctx, tx, command.ThreadID, item); err != nil {
			return nil, err
		}
		return []EventDraft{mailboxEvent(agoprotocol.EventMessageQueued, queueItemID, state.ActiveTurnID, input.Content)}, nil
	})
}

func (store *Store) Steer(ctx context.Context, command agoprotocol.Command, queueItemID, expectedTurnID string) (MailboxState, error) {
	if err := validateMailboxCommand(command, agoprotocol.CommandMessageSteer); err != nil {
		return MailboxState{}, err
	}
	request := struct{ QueueItemID, ExpectedTurnID string }{queueItemID, expectedTurnID}
	return store.mutateMailbox(ctx, command, request, func(ctx context.Context, tx *sql.Tx, state *MailboxState) ([]EventDraft, error) {
		if err := requireActiveTurn(*state, expectedTurnID); err != nil {
			return nil, err
		}
		result, err := tx.ExecContext(ctx, `UPDATE pending_inputs SET class = ? WHERE thread_id = ? AND queue_item_id = ? AND state = ?`, agoprotocol.QueueSteer, command.ThreadID, queueItemID, agoprotocol.QueueItemPending)
		if err != nil {
			return nil, fmt.Errorf("promote queued message: %w", err)
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return nil, fmt.Errorf("queue item %q is not pending", queueItemID)
		}
		return []EventDraft{mailboxEvent(agoprotocol.EventMessageSteered, queueItemID, expectedTurnID, nil)}, nil
	})
}

func (store *Store) EditQueued(ctx context.Context, command agoprotocol.Command, queueItemID string, content json.RawMessage) (MailboxState, error) {
	if err := validateMailboxCommand(command, agoprotocol.CommandMessageEditQueued); err != nil {
		return MailboxState{}, err
	}
	request := struct {
		QueueItemID string          `json:"queue_item_id"`
		Content     json.RawMessage `json:"content"`
	}{queueItemID, content}
	return store.mutateMailbox(ctx, command, request, func(ctx context.Context, tx *sql.Tx, state *MailboxState) ([]EventDraft, error) {
		result, err := tx.ExecContext(ctx, `UPDATE pending_inputs SET content_json = ? WHERE thread_id = ? AND queue_item_id = ? AND state = ?`, []byte(content), command.ThreadID, queueItemID, agoprotocol.QueueItemPending)
		if err != nil {
			return nil, fmt.Errorf("edit queued message: %w", err)
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return nil, fmt.Errorf("queue item %q is not pending", queueItemID)
		}
		return []EventDraft{mailboxEvent(agoprotocol.EventMessageQueueEdited, queueItemID, state.ActiveTurnID, content)}, nil
	})
}

func (store *Store) SafePoint(ctx context.Context, command agoprotocol.Command, expectedTurnID string) (MailboxState, error) {
	if err := validateMailboxCommand(command, agoprotocol.CommandSafePoint); err != nil {
		return MailboxState{}, err
	}
	return store.mutateMailbox(ctx, command, expectedTurnID, func(ctx context.Context, tx *sql.Tx, state *MailboxState) ([]EventDraft, error) {
		if err := requireActiveTurn(*state, expectedTurnID); err != nil {
			return nil, err
		}
		item, found, err := firstQueueItem(ctx, tx, command.ThreadID, `class = ? AND state = ?`, agoprotocol.QueueSteer, agoprotocol.QueueItemPending)
		if err != nil || !found {
			return nil, err
		}
		if err := deleteQueueItem(ctx, tx, command.ThreadID, item.QueueItemID); err != nil {
			return nil, err
		}
		return []EventDraft{mailboxEvent(agoprotocol.EventMessageAccepted, item.QueueItemID, expectedTurnID, item.Content)}, nil
	})
}

func (store *Store) Dequeue(ctx context.Context, command agoprotocol.Command, queueItemID string) (MailboxState, error) {
	if err := validateMailboxCommand(command, agoprotocol.CommandMessageDequeue); err != nil {
		return MailboxState{}, err
	}
	return store.mutateMailbox(ctx, command, queueItemID, func(ctx context.Context, tx *sql.Tx, state *MailboxState) ([]EventDraft, error) {
		if err := deleteQueueItem(ctx, tx, command.ThreadID, queueItemID); err != nil {
			return nil, err
		}
		return []EventDraft{mailboxEvent(agoprotocol.EventMessageDequeued, queueItemID, state.ActiveTurnID, nil)}, nil
	})
}

func (store *Store) CompleteTurn(ctx context.Context, command agoprotocol.Command, expectedTurnID string) (MailboxState, error) {
	if err := validateMailboxCommand(command, agoprotocol.CommandTurnComplete); err != nil {
		return MailboxState{}, err
	}
	return store.mutateMailbox(ctx, command, expectedTurnID, func(ctx context.Context, tx *sql.Tx, state *MailboxState) ([]EventDraft, error) {
		if err := requireActiveTurn(*state, expectedTurnID); err != nil {
			return nil, err
		}
		if state.CancelRequested {
			return nil, fmt.Errorf("turn %q is settling cancellation", expectedTurnID)
		}
		events := []EventDraft{mailboxEvent(agoprotocol.EventTurnCompleted, "", expectedTurnID, nil)}
		item, found, err := nextPendingInput(ctx, tx, command.ThreadID)
		if err != nil {
			return nil, err
		}
		if !found {
			state.Activity = agoprotocol.ActivityIdle
			state.ActiveTurnID = ""
			return events, nil
		}
		if err := deleteQueueItem(ctx, tx, command.ThreadID, item.QueueItemID); err != nil {
			return nil, err
		}
		turnID, err := randomID("R-")
		if err != nil {
			return nil, err
		}
		state.Activity = agoprotocol.ActivityRunning
		state.ActiveTurnID = turnID
		events = append(events,
			mailboxEvent(agoprotocol.EventMessageAccepted, item.QueueItemID, turnID, item.Content),
			mailboxEvent(agoprotocol.EventTurnStarted, item.QueueItemID, turnID, nil),
		)
		return events, nil
	})
}

func (store *Store) FailTurn(ctx context.Context, command agoprotocol.Command, expectedTurnID, detail string) (MailboxState, error) {
	if err := validateMailboxCommand(command, agoprotocol.CommandTurnFail); err != nil {
		return MailboxState{}, err
	}
	request := struct {
		ExpectedTurnID string `json:"expected_turn_id"`
		Detail         string `json:"detail"`
	}{expectedTurnID, detail}
	return store.mutateMailbox(ctx, command, request, func(_ context.Context, _ *sql.Tx, state *MailboxState) ([]EventDraft, error) {
		if err := requireActiveTurn(*state, expectedTurnID); err != nil {
			return nil, err
		}
		state.Activity = agoprotocol.ActivityError
		state.ActiveTurnID = ""
		state.CancelRequested = false
		payload, err := json.Marshal(struct {
			TurnID string `json:"turn_id"`
			Detail string `json:"detail"`
		}{expectedTurnID, detail})
		if err != nil {
			return nil, err
		}
		return []EventDraft{{Type: agoprotocol.EventTurnFailed, Visibility: agoprotocol.VisibilityUser, Payload: payload}}, nil
	})
}

func (store *Store) InterruptAndSubmit(ctx context.Context, command agoprotocol.Command, expectedTurnID string, input MessageInput) (MailboxState, error) {
	if err := validateMailboxCommand(command, agoprotocol.CommandTurnInterrupt); err != nil {
		return MailboxState{}, err
	}
	request := struct {
		ExpectedTurnID string       `json:"expected_turn_id"`
		Input          MessageInput `json:"input"`
	}{expectedTurnID, input}
	return store.mutateMailbox(ctx, command, request, func(ctx context.Context, tx *sql.Tx, state *MailboxState) ([]EventDraft, error) {
		if err := requireActiveTurn(*state, expectedTurnID); err != nil {
			return nil, err
		}
		if state.CancelRequested {
			return nil, fmt.Errorf("turn %q already has cancellation requested", expectedTurnID)
		}
		queueItemID, err := randomID("Q-")
		if err != nil {
			return nil, err
		}
		position, err := nextQueuePosition(ctx, tx, command.ThreadID)
		if err != nil {
			return nil, err
		}
		item := QueueItem{QueueItemID: queueItemID, Position: position, Class: agoprotocol.QueueSteer, State: agoprotocol.QueueItemInterruptPending, Content: cloneRawMessage(input.Content)}
		if err := insertQueueItem(ctx, tx, command.ThreadID, item); err != nil {
			return nil, err
		}
		state.CancelRequested = true
		return []EventDraft{
			mailboxEvent(agoprotocol.EventTurnCancelRequested, queueItemID, expectedTurnID, nil),
			mailboxEvent(agoprotocol.EventMessageQueued, queueItemID, expectedTurnID, input.Content),
		}, nil
	})
}

func (store *Store) Cancel(ctx context.Context, command agoprotocol.Command, expectedTurnID string) (MailboxState, error) {
	if err := validateMailboxCommand(command, agoprotocol.CommandTurnCancel); err != nil {
		return MailboxState{}, err
	}
	return store.mutateMailbox(ctx, command, expectedTurnID, func(_ context.Context, _ *sql.Tx, state *MailboxState) ([]EventDraft, error) {
		if err := requireActiveTurn(*state, expectedTurnID); err != nil {
			return nil, err
		}
		if state.CancelRequested {
			return nil, fmt.Errorf("turn %q already has cancellation requested", expectedTurnID)
		}
		state.CancelRequested = true
		return []EventDraft{mailboxEvent(agoprotocol.EventTurnCancelRequested, "", expectedTurnID, nil)}, nil
	})
}

func (store *Store) SettleCancellation(ctx context.Context, command agoprotocol.Command, expectedTurnID string) (MailboxState, error) {
	if err := validateMailboxCommand(command, agoprotocol.CommandTurnSettleCancelled); err != nil {
		return MailboxState{}, err
	}
	return store.mutateMailbox(ctx, command, expectedTurnID, func(ctx context.Context, tx *sql.Tx, state *MailboxState) ([]EventDraft, error) {
		if err := requireActiveTurn(*state, expectedTurnID); err != nil {
			return nil, err
		}
		if !state.CancelRequested {
			return nil, fmt.Errorf("turn %q has no cancellation request", expectedTurnID)
		}
		events := []EventDraft{mailboxEvent(agoprotocol.EventTurnCancelled, "", expectedTurnID, nil)}
		state.CancelRequested = false
		item, found, err := firstQueueItem(ctx, tx, command.ThreadID, `state = ?`, agoprotocol.QueueItemInterruptPending)
		if err != nil {
			return nil, err
		}
		if !found {
			state.Activity = agoprotocol.ActivityIdle
			state.ActiveTurnID = ""
			return events, nil
		}
		if err := deleteQueueItem(ctx, tx, command.ThreadID, item.QueueItemID); err != nil {
			return nil, err
		}
		turnID, err := randomID("R-")
		if err != nil {
			return nil, err
		}
		state.Activity = agoprotocol.ActivityRunning
		state.ActiveTurnID = turnID
		events = append(events,
			mailboxEvent(agoprotocol.EventMessageAccepted, item.QueueItemID, turnID, item.Content),
			mailboxEvent(agoprotocol.EventTurnStarted, item.QueueItemID, turnID, nil),
		)
		return events, nil
	})
}

func (store *Store) Mailbox(ctx context.Context, threadID string) (MailboxState, error) {
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MailboxState{}, err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := loadMailboxState(ctx, tx, threadID)
	if err != nil {
		return MailboxState{}, err
	}
	if err := tx.Commit(); err != nil {
		return MailboxState{}, err
	}
	return state, nil
}

func (store *Store) ActiveMailboxes(ctx context.Context) ([]MailboxState, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT thread_id FROM threads WHERE active_turn_id <> '' ORDER BY thread_id`)
	if err != nil {
		return nil, err
	}
	var threadIDs []string
	for rows.Next() {
		var threadID string
		if err := rows.Scan(&threadID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		threadIDs = append(threadIDs, threadID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	states := make([]MailboxState, 0, len(threadIDs))
	for _, threadID := range threadIDs {
		state, err := store.Mailbox(ctx, threadID)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	return states, nil
}

func (store *Store) mutateMailbox(ctx context.Context, command agoprotocol.Command, request any, mutate func(context.Context, *sql.Tx, *MailboxState) ([]EventDraft, error)) (MailboxState, error) {
	requestHash, err := hashMailboxRequest(command, request)
	if err != nil {
		return MailboxState{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return MailboxState{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, err := storedMailboxResult(ctx, tx, command, requestHash); err != nil {
		return MailboxState{}, err
	} else if found {
		return existing, nil
	}
	state, err := loadMailboxState(ctx, tx, command.ThreadID)
	if err != nil {
		return MailboxState{}, err
	}
	if command.ExpectedSequence != nil && *command.ExpectedSequence != state.LastSequence {
		return MailboxState{}, ConflictError{CurrentSequence: state.LastSequence, ExpectedSequence: *command.ExpectedSequence}
	}
	drafts, err := mutate(ctx, tx, &state)
	if err != nil {
		return MailboxState{}, err
	}
	events, err := appendMailboxEvents(ctx, tx, command.ThreadID, state.LastSequence, drafts)
	if err != nil {
		return MailboxState{}, err
	}
	state.LastSequence += uint64(len(events))
	state.Events = events
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET last_sequence = ?, activity = ?, active_turn_id = ?, cancel_requested = ? WHERE thread_id = ?`, state.LastSequence, state.Activity, state.ActiveTurnID, state.CancelRequested, command.ThreadID); err != nil {
		return MailboxState{}, err
	}
	state.Queue, err = loadQueue(ctx, tx, command.ThreadID)
	if err != nil {
		return MailboxState{}, err
	}
	if err := insertMailboxCommand(ctx, tx, command, requestHash, state); err != nil {
		return MailboxState{}, err
	}
	if err := tx.Commit(); err != nil {
		return MailboxState{}, err
	}
	return state, nil
}

func loadMailboxState(ctx context.Context, tx *sql.Tx, threadID string) (MailboxState, error) {
	state := MailboxState{ThreadID: threadID}
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence, activity, active_turn_id, cancel_requested FROM threads WHERE thread_id = ?`, threadID).Scan(&state.LastSequence, &state.Activity, &state.ActiveTurnID, &state.CancelRequested); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MailboxState{}, fmt.Errorf("thread %q does not exist", threadID)
		}
		return MailboxState{}, err
	}
	queue, err := loadQueue(ctx, tx, threadID)
	state.Queue = queue
	return state, err
}

func loadQueue(ctx context.Context, tx *sql.Tx, threadID string) ([]QueueItem, error) {
	rows, err := tx.QueryContext(ctx, `SELECT queue_item_id, position, class, state, content_json FROM pending_inputs WHERE thread_id = ? ORDER BY position`, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]QueueItem, 0)
	for rows.Next() {
		var item QueueItem
		if err := rows.Scan(&item.QueueItemID, &item.Position, &item.Class, &item.State, &item.Content); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func firstQueueItem(ctx context.Context, tx *sql.Tx, threadID, condition string, args ...any) (QueueItem, bool, error) {
	query := `SELECT queue_item_id, position, class, state, content_json FROM pending_inputs WHERE thread_id = ? AND ` + condition + ` ORDER BY position LIMIT 1`
	rowArgs := append([]any{threadID}, args...)
	var item QueueItem
	err := tx.QueryRowContext(ctx, query, rowArgs...).Scan(&item.QueueItemID, &item.Position, &item.Class, &item.State, &item.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return QueueItem{}, false, nil
	}
	return item, err == nil, err
}

func nextPendingInput(ctx context.Context, tx *sql.Tx, threadID string) (QueueItem, bool, error) {
	var item QueueItem
	err := tx.QueryRowContext(ctx, `
SELECT queue_item_id, position, class, state, content_json
FROM pending_inputs
WHERE thread_id = ? AND state = ?
ORDER BY CASE class WHEN 'steer' THEN 0 ELSE 1 END, position
LIMIT 1`, threadID, agoprotocol.QueueItemPending).Scan(&item.QueueItemID, &item.Position, &item.Class, &item.State, &item.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return QueueItem{}, false, nil
	}
	return item, err == nil, err
}

func nextQueuePosition(ctx context.Context, tx *sql.Tx, threadID string) (uint64, error) {
	var position uint64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position), 0) + 1 FROM pending_inputs WHERE thread_id = ?`, threadID).Scan(&position); err != nil {
		return 0, err
	}
	return position, nil
}

func insertQueueItem(ctx context.Context, tx *sql.Tx, threadID string, item QueueItem) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO pending_inputs (thread_id, queue_item_id, position, class, state, content_json) VALUES (?, ?, ?, ?, ?, ?)`, threadID, item.QueueItemID, item.Position, item.Class, item.State, []byte(item.Content))
	return err
}

func deleteQueueItem(ctx context.Context, tx *sql.Tx, threadID, queueItemID string) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM pending_inputs WHERE thread_id = ? AND queue_item_id = ?`, threadID, queueItemID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("queue item %q is not pending", queueItemID)
	}
	return nil
}

func appendMailboxEvents(ctx context.Context, tx *sql.Tx, threadID string, lastSequence uint64, drafts []EventDraft) ([]agoprotocol.Event, error) {
	events := make([]agoprotocol.Event, 0, len(drafts))
	for index, draft := range drafts {
		eventID, err := randomID("E-")
		if err != nil {
			return nil, err
		}
		event := agoprotocol.Event{SchemaVersion: agoprotocol.SchemaVersion, EventID: eventID, ThreadID: threadID, Sequence: lastSequence + uint64(index) + 1, Type: draft.Type, Visibility: draft.Visibility, Provenance: draft.Provenance, Payload: cloneRawMessage(draft.Payload)}
		if err := event.Validate(); err != nil {
			return nil, err
		}
		if err := insertEvent(ctx, tx, event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, nil
}

func mailboxEvent(eventType agoprotocol.EventType, queueItemID, turnID string, content json.RawMessage) EventDraft {
	payload, _ := json.Marshal(struct {
		QueueItemID string          `json:"queue_item_id,omitempty"`
		TurnID      string          `json:"turn_id,omitempty"`
		Content     json.RawMessage `json:"content,omitempty"`
	}{queueItemID, turnID, content})
	return EventDraft{Type: eventType, Visibility: agoprotocol.VisibilityUser, Payload: payload}
}

func validateMailboxCommand(command agoprotocol.Command, expected agoprotocol.CommandType) error {
	if err := command.Validate(); err != nil {
		return err
	}
	if command.Type != expected {
		return fmt.Errorf("operation requires a %q command", expected)
	}
	return nil
}

func requireActiveTurn(state MailboxState, expectedTurnID string) error {
	if expectedTurnID == "" || !isActiveActivity(state.Activity) || state.ActiveTurnID != expectedTurnID {
		return StaleTurnError{ActiveTurnID: state.ActiveTurnID, ExpectedTurnID: expectedTurnID}
	}
	return nil
}

func isActiveActivity(activity agoprotocol.Activity) bool {
	return activity == agoprotocol.ActivityRunning || activity == agoprotocol.ActivityAwaitingApproval
}

func hashMailboxRequest(command agoprotocol.Command, request any) ([]byte, error) {
	encoded, err := json.Marshal(struct {
		Command agoprotocol.Command `json:"command"`
		Request any                 `json:"request"`
	}{command, request})
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	return digest[:], nil
}

func storedMailboxResult(ctx context.Context, tx *sql.Tx, command agoprotocol.Command, requestHash []byte) (MailboxState, bool, error) {
	var storedHash, encoded []byte
	err := tx.QueryRowContext(ctx, `SELECT request_hash, result_json FROM commands WHERE actor_id = ? AND idempotency_key = ?`, command.ActorID, command.IdempotencyKey).Scan(&storedHash, &encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return MailboxState{}, false, nil
	}
	if err != nil {
		return MailboxState{}, false, err
	}
	if !bytes.Equal(storedHash, requestHash) {
		return MailboxState{}, false, fmt.Errorf("idempotency key %q was already used for different content", command.IdempotencyKey)
	}
	var state MailboxState
	if err := json.Unmarshal(encoded, &state); err != nil {
		return MailboxState{}, false, err
	}
	return state, true, nil
}

func insertMailboxCommand(ctx context.Context, tx *sql.Tx, command agoprotocol.Command, requestHash []byte, state MailboxState) error {
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO commands (actor_id, idempotency_key, command_id, request_hash, thread_id, result_json) VALUES (?, ?, ?, ?, ?, ?)`, command.ActorID, command.IdempotencyKey, command.CommandID, requestHash, command.ThreadID, encoded)
	return err
}
