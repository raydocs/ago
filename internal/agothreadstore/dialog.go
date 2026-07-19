package agothreadstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"claudexflow/internal/agoprotocol"
)

const (
	DialogPending  DialogState = "pending"
	DialogResolved DialogState = "resolved"
)

type DialogState string

type PluginDialog struct {
	DialogID          string          `json:"dialog_id"`
	ThreadID          string          `json:"thread_id"`
	TurnID            string          `json:"turn_id"`
	PluginID          string          `json:"plugin_id"`
	Generation        uint64          `json:"generation"`
	InvocationID      string          `json:"invocation_id"`
	Deadline          time.Time       `json:"deadline"`
	RequestType       string          `json:"request_type"`
	Request           json.RawMessage `json:"request"`
	State             DialogState     `json:"state"`
	Revision          uint64          `json:"revision"`
	RequestedSequence uint64          `json:"requested_sequence"`
	ResolvedSequence  uint64          `json:"resolved_sequence,omitempty"`
	ResolverID        string          `json:"resolver_id,omitempty"`
	Response          json.RawMessage `json:"response,omitempty"`
}

type CreateDialogInput struct {
	ThreadID         string
	TurnID           string
	PluginID         string
	Generation       uint64
	InvocationID     string
	Deadline         time.Time
	RequestType      string
	Request          json.RawMessage
	ExpectedSequence *uint64
}

type ResolveDialogInput struct {
	DialogID         string
	ResolverID       string
	ExpectedRevision uint64
	ExpectedSequence *uint64
	Response         json.RawMessage
}

func (store *Store) CreatePendingDialog(ctx context.Context, input CreateDialogInput) (PluginDialog, error) {
	if input.ThreadID == "" || input.TurnID == "" || input.PluginID == "" || input.InvocationID == "" || input.RequestType == "" || input.Deadline.IsZero() {
		return PluginDialog{}, fmt.Errorf("thread, turn, plugin, invocation, deadline, and request type are required")
	}
	if !json.Valid(input.Request) {
		return PluginDialog{}, fmt.Errorf("request must be valid JSON")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return PluginDialog{}, fmt.Errorf("begin dialog create: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	existing, found, err := findDialogByCorrelation(ctx, tx, input)
	if err != nil {
		return PluginDialog{}, err
	}
	if found {
		if existing.Deadline.Equal(input.Deadline) && existing.RequestType == input.RequestType && bytes.Equal(existing.Request, input.Request) {
			return existing, nil
		}
		return PluginDialog{}, fmt.Errorf("plugin invocation already has a different dialog")
	}
	var sequence uint64
	var activeTurnID string
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence, active_turn_id FROM threads WHERE thread_id = ?`, input.ThreadID).Scan(&sequence, &activeTurnID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PluginDialog{}, fmt.Errorf("thread %q does not exist", input.ThreadID)
		}
		return PluginDialog{}, fmt.Errorf("read thread sequence: %w", err)
	}
	if activeTurnID != input.TurnID {
		return PluginDialog{}, fmt.Errorf("dialog turn %q is not the active turn %q", input.TurnID, activeTurnID)
	}
	if input.ExpectedSequence != nil && *input.ExpectedSequence != sequence {
		return PluginDialog{}, ConflictError{CurrentSequence: sequence, ExpectedSequence: *input.ExpectedSequence}
	}
	id, err := randomID("D-")
	if err != nil {
		return PluginDialog{}, err
	}
	dialog := PluginDialog{DialogID: id, ThreadID: input.ThreadID, TurnID: input.TurnID, PluginID: input.PluginID, Generation: input.Generation, InvocationID: input.InvocationID, Deadline: input.Deadline.UTC(), RequestType: input.RequestType, Request: cloneRawMessage(input.Request), State: DialogPending, Revision: 1, RequestedSequence: sequence + 1}
	payload, _ := json.Marshal(dialog)
	event, err := dialogEvent(ctx, tx, dialog.ThreadID, dialog.RequestedSequence, agoprotocol.EventPluginDialogRequested, payload)
	if err != nil {
		return PluginDialog{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO plugin_dialogs (dialog_id,thread_id,turn_id,plugin_id,generation,invocation_id,deadline,request_type,request_json,state,revision,requested_sequence) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, id, input.ThreadID, input.TurnID, input.PluginID, input.Generation, input.InvocationID, dialog.Deadline.Format(time.RFC3339Nano), input.RequestType, []byte(input.Request), DialogPending, 1, event.Sequence); err != nil {
		return PluginDialog{}, fmt.Errorf("insert plugin dialog: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE threads SET last_sequence=?, activity=? WHERE thread_id=?`, event.Sequence, agoprotocol.ActivityAwaitingApproval, input.ThreadID); err != nil {
		return PluginDialog{}, err
	}
	if err := tx.Commit(); err != nil {
		return PluginDialog{}, fmt.Errorf("commit dialog create: %w", err)
	}
	return dialog, nil
}

func (store *Store) ResolveDialog(ctx context.Context, input ResolveDialogInput) (PluginDialog, error) {
	if input.DialogID == "" || input.ResolverID == "" || input.ExpectedRevision == 0 || !json.Valid(input.Response) {
		return PluginDialog{}, fmt.Errorf("dialog, resolver, expected revision, and valid response JSON are required")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return PluginDialog{}, err
	}
	defer func() { _ = tx.Rollback() }()
	d, err := scanDialog(tx.QueryRowContext(ctx, dialogSelect+` WHERE dialog_id=?`, input.DialogID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PluginDialog{}, fmt.Errorf("dialog %q does not exist", input.DialogID)
		}
		return PluginDialog{}, err
	}
	if d.State == DialogResolved {
		if d.ResolverID == input.ResolverID && bytes.Equal(d.Response, input.Response) {
			return d, nil
		}
		return PluginDialog{}, fmt.Errorf("dialog %q is already resolved", input.DialogID)
	}
	if d.Revision != input.ExpectedRevision {
		return PluginDialog{}, fmt.Errorf("dialog revision is %d, expected %d", d.Revision, input.ExpectedRevision)
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence FROM threads WHERE thread_id=?`, d.ThreadID).Scan(&sequence); err != nil {
		return PluginDialog{}, err
	}
	if input.ExpectedSequence != nil && *input.ExpectedSequence != sequence {
		return PluginDialog{}, ConflictError{CurrentSequence: sequence, ExpectedSequence: *input.ExpectedSequence}
	}
	d.State = DialogResolved
	d.Revision++
	d.ResolverID = input.ResolverID
	d.Response = cloneRawMessage(input.Response)
	d.ResolvedSequence = sequence + 1
	payload, _ := json.Marshal(struct {
		DialogID   string          `json:"dialog_id"`
		Revision   uint64          `json:"revision"`
		ResolverID string          `json:"resolver_id"`
		Response   json.RawMessage `json:"response"`
	}{d.DialogID, d.Revision, d.ResolverID, d.Response})
	if _, err := dialogEvent(ctx, tx, d.ThreadID, d.ResolvedSequence, agoprotocol.EventPluginDialogResolved, payload); err != nil {
		return PluginDialog{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE plugin_dialogs SET state=?,revision=?,resolved_sequence=?,resolver_id=?,response_json=? WHERE dialog_id=? AND state=? AND revision=?`, DialogResolved, d.Revision, d.ResolvedSequence, d.ResolverID, []byte(d.Response), d.DialogID, DialogPending, input.ExpectedRevision)
	if err != nil {
		return PluginDialog{}, err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return PluginDialog{}, fmt.Errorf("dialog resolution lost revision race")
	}
	var pendingCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_dialogs WHERE thread_id=? AND state=?`, d.ThreadID, DialogPending).Scan(&pendingCount); err != nil {
		return PluginDialog{}, err
	}
	if pendingCount == 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE threads SET last_sequence=?, activity=CASE WHEN active_turn_id=? AND activity=? THEN ? ELSE activity END WHERE thread_id=?`, d.ResolvedSequence, d.TurnID, agoprotocol.ActivityAwaitingApproval, agoprotocol.ActivityRunning, d.ThreadID); err != nil {
			return PluginDialog{}, err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE threads SET last_sequence=? WHERE thread_id=?`, d.ResolvedSequence, d.ThreadID); err != nil {
		return PluginDialog{}, err
	}
	if err := tx.Commit(); err != nil {
		return PluginDialog{}, err
	}
	return d, nil
}

func (store *Store) ListPendingDialogs(ctx context.Context, threadID string) ([]PluginDialog, error) {
	return store.listDialogs(ctx, threadID, true)
}

func (store *Store) ListPendingDialogsByGeneration(ctx context.Context, generation uint64) ([]PluginDialog, error) {
	return store.listPendingDialogsWhere(ctx, `generation=?`, generation)
}

func (store *Store) ListAllPendingDialogs(ctx context.Context) ([]PluginDialog, error) {
	return store.listPendingDialogsWhere(ctx, `1=1`)
}

func (store *Store) listPendingDialogsWhere(ctx context.Context, predicate string, args ...any) ([]PluginDialog, error) {
	rows, err := store.db.QueryContext(ctx, dialogSelect+` WHERE `+predicate+` AND state='pending' ORDER BY thread_id,requested_sequence,dialog_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PluginDialog{}
	for rows.Next() {
		dialog, err := scanDialog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dialog)
	}
	return out, rows.Err()
}

func (store *Store) ListDialogs(ctx context.Context, threadID string) ([]PluginDialog, error) {
	return store.listDialogs(ctx, threadID, false)
}

func (store *Store) Dialog(ctx context.Context, dialogID string) (PluginDialog, error) {
	if dialogID == "" {
		return PluginDialog{}, fmt.Errorf("dialog_id is required")
	}
	dialog, err := scanDialog(store.db.QueryRowContext(ctx, dialogSelect+` WHERE dialog_id=?`, dialogID))
	if errors.Is(err, sql.ErrNoRows) {
		return PluginDialog{}, fmt.Errorf("dialog %q does not exist", dialogID)
	}
	return dialog, err
}

func (store *Store) listDialogs(ctx context.Context, threadID string, pending bool) ([]PluginDialog, error) {
	if threadID == "" {
		return nil, fmt.Errorf("thread_id is required")
	}
	q := dialogSelect + ` WHERE thread_id=?`
	if pending {
		q += ` AND state='pending'`
	}
	q += ` ORDER BY requested_sequence, dialog_id`
	rows, err := store.db.QueryContext(ctx, q, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PluginDialog{}
	for rows.Next() {
		d, err := scanDialog(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const dialogSelect = `SELECT dialog_id,thread_id,turn_id,plugin_id,generation,invocation_id,deadline,request_type,request_json,state,revision,requested_sequence,resolved_sequence,resolver_id,response_json FROM plugin_dialogs`

type rowScanner interface{ Scan(...any) error }

func scanDialog(row rowScanner) (PluginDialog, error) {
	var d PluginDialog
	var deadline string
	var request []byte
	var response []byte
	err := row.Scan(&d.DialogID, &d.ThreadID, &d.TurnID, &d.PluginID, &d.Generation, &d.InvocationID, &deadline, &d.RequestType, &request, &d.State, &d.Revision, &d.RequestedSequence, &d.ResolvedSequence, &d.ResolverID, &response)
	if err != nil {
		return d, err
	}
	d.Deadline, err = time.Parse(time.RFC3339Nano, deadline)
	d.Request = cloneRawMessage(request)
	if response != nil {
		d.Response = cloneRawMessage(response)
	}
	return d, err
}
func findDialogByCorrelation(ctx context.Context, tx *sql.Tx, in CreateDialogInput) (PluginDialog, bool, error) {
	d, err := scanDialog(tx.QueryRowContext(ctx, dialogSelect+` WHERE thread_id=? AND turn_id=? AND plugin_id=? AND generation=? AND invocation_id=?`, in.ThreadID, in.TurnID, in.PluginID, in.Generation, in.InvocationID))
	if errors.Is(err, sql.ErrNoRows) {
		return d, false, nil
	}
	return d, err == nil, err
}
func dialogEvent(ctx context.Context, tx *sql.Tx, threadID string, sequence uint64, eventType agoprotocol.EventType, payload []byte) (agoprotocol.Event, error) {
	id, err := randomID("E-")
	if err != nil {
		return agoprotocol.Event{}, err
	}
	event := agoprotocol.Event{SchemaVersion: agoprotocol.SchemaVersion, EventID: id, ThreadID: threadID, Sequence: sequence, Type: eventType, Visibility: agoprotocol.VisibilityInternal, Payload: payload}
	if err := event.Validate(); err != nil {
		return event, err
	}
	return event, insertEvent(ctx, tx, event)
}
