package agothreadstore

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"claudexflow/internal/agoprotocol"
)

func TestSubmitStartsIdleThreadAndQueuesBusyThread(t *testing.T) {
	store := openTestStore(t)
	thread := mustCreateThread(t, store, "create-mailbox")

	started, err := store.Submit(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandMessageSubmit, "submit-first"), MessageInput{
		Content: json.RawMessage(`{"text":"first"}`),
		Class:   agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatalf("Submit(idle) error = %v", err)
	}
	if started.Activity != agoprotocol.ActivityRunning || started.ActiveTurnID == "" {
		t.Fatalf("Submit(idle) state = %#v, want running with an active turn", started)
	}
	if len(started.Queue) != 0 {
		t.Fatalf("Submit(idle) queue = %#v, want empty queue", started.Queue)
	}

	queued, err := store.Submit(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandMessageSubmit, "submit-second"), MessageInput{
		Content: json.RawMessage(`{"text":"second"}`),
		Class:   agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatalf("Submit(busy) error = %v", err)
	}
	if queued.ActiveTurnID != started.ActiveTurnID || len(queued.Queue) != 1 {
		t.Fatalf("Submit(busy) state = %#v, want original turn and one queue item", queued)
	}
	if queued.Queue[0].Class != agoprotocol.QueueNormal || queued.Queue[0].State != agoprotocol.QueueItemPending {
		t.Fatalf("queued item = %#v, want normal pending", queued.Queue[0])
	}

	retry, err := store.Submit(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandMessageSubmit, "submit-second"), MessageInput{
		Content: json.RawMessage(`{"text":"second"}`),
		Class:   agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatalf("Submit() retry error = %v", err)
	}
	if !reflect.DeepEqual(retry, queued) {
		t.Fatalf("Submit() retry = %#v, want original result %#v", retry, queued)
	}
}

func TestPromoteToSteerAndConsumeAtSafePointExactlyOnce(t *testing.T) {
	store := openTestStore(t)
	thread := mustCreateThread(t, store, "create-steer")
	started := mustSubmit(t, store, thread.ThreadID, "first", agoprotocol.QueueNormal)
	firstQueued := mustSubmit(t, store, thread.ThreadID, "second", agoprotocol.QueueNormal)
	secondQueued := mustSubmit(t, store, thread.ThreadID, "third", agoprotocol.QueueNormal)

	promoted, err := store.Steer(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandMessageSteer, "steer-second"), firstQueued.Queue[0].QueueItemID, started.ActiveTurnID)
	if err != nil {
		t.Fatalf("Steer() error = %v", err)
	}
	if len(promoted.Queue) != 2 || promoted.Queue[0].Class != agoprotocol.QueueSteer {
		t.Fatalf("Steer() queue = %#v, want first item promoted in place", promoted.Queue)
	}

	consumed, err := store.SafePoint(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandSafePoint, "safe-point"), started.ActiveTurnID)
	if err != nil {
		t.Fatalf("SafePoint() error = %v", err)
	}
	if consumed.ActiveTurnID != started.ActiveTurnID || len(consumed.Queue) != 1 {
		t.Fatalf("SafePoint() state = %#v, want same turn and one remaining item", consumed)
	}
	if consumed.Queue[0].QueueItemID != secondQueued.Queue[1].QueueItemID {
		t.Fatalf("SafePoint() remaining queue = %#v, want third message", consumed.Queue)
	}

	retry, err := store.SafePoint(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandSafePoint, "safe-point"), started.ActiveTurnID)
	if err != nil {
		t.Fatalf("SafePoint() retry error = %v", err)
	}
	if !reflect.DeepEqual(retry, consumed) {
		t.Fatalf("SafePoint() retry = %#v, want original %#v", retry, consumed)
	}
}

func TestCompleteTurnStartsNextQueuedMessageThenBecomesIdle(t *testing.T) {
	store := openTestStore(t)
	thread := mustCreateThread(t, store, "create-complete")
	started := mustSubmit(t, store, thread.ThreadID, "first", agoprotocol.QueueNormal)
	mustSubmit(t, store, thread.ThreadID, "second", agoprotocol.QueueNormal)

	next, err := store.CompleteTurn(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandTurnComplete, "complete-first"), started.ActiveTurnID)
	if err != nil {
		t.Fatalf("CompleteTurn(first) error = %v", err)
	}
	if next.Activity != agoprotocol.ActivityRunning || next.ActiveTurnID == "" || next.ActiveTurnID == started.ActiveTurnID || len(next.Queue) != 0 {
		t.Fatalf("CompleteTurn(first) state = %#v, want next message running", next)
	}

	idle, err := store.CompleteTurn(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandTurnComplete, "complete-second"), next.ActiveTurnID)
	if err != nil {
		t.Fatalf("CompleteTurn(second) error = %v", err)
	}
	if idle.Activity != agoprotocol.ActivityIdle || idle.ActiveTurnID != "" || len(idle.Queue) != 0 {
		t.Fatalf("CompleteTurn(second) state = %#v, want idle", idle)
	}

	events, err := store.Replay(context.Background(), thread.ThreadID, 0, 0)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	wantTypes := []agoprotocol.EventType{
		agoprotocol.EventThreadCreated,
		agoprotocol.EventMessageAccepted,
		agoprotocol.EventTurnStarted,
		agoprotocol.EventMessageQueued,
		agoprotocol.EventTurnCompleted,
		agoprotocol.EventMessageAccepted,
		agoprotocol.EventTurnStarted,
		agoprotocol.EventTurnCompleted,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("Replay() returned %d events, want %d: %#v", len(events), len(wantTypes), events)
	}
	for index, event := range events {
		if event.Sequence != uint64(index+1) || event.Type != wantTypes[index] {
			t.Fatalf("event %d = (%d, %q), want (%d, %q)", index, event.Sequence, event.Type, index+1, wantTypes[index])
		}
	}
}

func TestDequeueAndStaleTurnCommandsAreDeterministic(t *testing.T) {
	store := openTestStore(t)
	thread := mustCreateThread(t, store, "create-dequeue")
	started := mustSubmit(t, store, thread.ThreadID, "first", agoprotocol.QueueNormal)
	queued := mustSubmit(t, store, thread.ThreadID, "second", agoprotocol.QueueNormal)

	if _, err := store.Steer(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandMessageSteer, "stale-steer"), queued.Queue[0].QueueItemID, "turn-stale"); err == nil {
		t.Fatal("Steer() accepted a stale expected turn")
	}
	afterStale, err := store.Mailbox(context.Background(), thread.ThreadID)
	if err != nil {
		t.Fatalf("Mailbox() error = %v", err)
	}
	if !reflect.DeepEqual(afterStale.Queue, queued.Queue) {
		t.Fatalf("stale Steer() mutated queue to %#v, want %#v", afterStale.Queue, queued.Queue)
	}

	dequeued, err := store.Dequeue(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandMessageDequeue, "dequeue-second"), queued.Queue[0].QueueItemID)
	if err != nil {
		t.Fatalf("Dequeue() error = %v", err)
	}
	if dequeued.ActiveTurnID != started.ActiveTurnID || len(dequeued.Queue) != 0 {
		t.Fatalf("Dequeue() state = %#v, want active turn and empty queue", dequeued)
	}
}

func TestEditQueuedMessageIsRevisionCheckedAndDurable(t *testing.T) {
	store := openTestStore(t)
	thread := mustCreateThread(t, store, "create-edit")
	mustSubmit(t, store, thread.ThreadID, "first", agoprotocol.QueueNormal)
	queued := mustSubmit(t, store, thread.ThreadID, "second", agoprotocol.QueueNormal)

	edit := mailboxCommand(thread.ThreadID, agoprotocol.CommandMessageEditQueued, "edit-second")
	edit.ExpectedSequence = sequencePointer(queued.LastSequence)
	edited, err := store.EditQueued(context.Background(), edit, queued.Queue[0].QueueItemID, json.RawMessage(`{"text":"edited"}`))
	if err != nil {
		t.Fatalf("EditQueued() error = %v", err)
	}
	if len(edited.Queue) != 1 || string(edited.Queue[0].Content) != `{"text":"edited"}` {
		t.Fatalf("EditQueued() queue = %#v, want durable edited content", edited.Queue)
	}

	stale := mailboxCommand(thread.ThreadID, agoprotocol.CommandMessageEditQueued, "stale-edit")
	stale.ExpectedSequence = sequencePointer(queued.LastSequence)
	_, err = store.EditQueued(context.Background(), stale, queued.Queue[0].QueueItemID, json.RawMessage(`{"text":"stale"}`))
	var conflict ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale EditQueued() error = %v, want ConflictError", err)
	}
	got, err := store.Mailbox(context.Background(), thread.ThreadID)
	if err != nil {
		t.Fatalf("Mailbox() error = %v", err)
	}
	if string(got.Queue[0].Content) != `{"text":"edited"}` {
		t.Fatalf("stale edit changed content to %s", got.Queue[0].Content)
	}
}

func TestDequeueVersusCompletionHasOneRevisionWinner(t *testing.T) {
	store := openTestStore(t)
	thread := mustCreateThread(t, store, "create-race")
	started := mustSubmit(t, store, thread.ThreadID, "first", agoprotocol.QueueNormal)
	queued := mustSubmit(t, store, thread.ThreadID, "second", agoprotocol.QueueNormal)
	targetQueueItemID := queued.Queue[0].QueueItemID

	dequeue := mailboxCommand(thread.ThreadID, agoprotocol.CommandMessageDequeue, "race-dequeue")
	dequeue.ExpectedSequence = sequencePointer(queued.LastSequence)
	complete := mailboxCommand(thread.ThreadID, agoprotocol.CommandTurnComplete, "race-complete")
	complete.ExpectedSequence = sequencePointer(queued.LastSequence)

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var dequeueErr, completeErr error
	go func() {
		defer wait.Done()
		<-start
		_, dequeueErr = store.Dequeue(context.Background(), dequeue, targetQueueItemID)
	}()
	go func() {
		defer wait.Done()
		<-start
		_, completeErr = store.CompleteTurn(context.Background(), complete, started.ActiveTurnID)
	}()
	close(start)
	wait.Wait()

	if (dequeueErr == nil) == (completeErr == nil) {
		t.Fatalf("race errors = dequeue %v, complete %v; want exactly one success", dequeueErr, completeErr)
	}
	loser := dequeueErr
	if loser == nil {
		loser = completeErr
	}
	var conflict ConflictError
	if !errors.As(loser, &conflict) {
		t.Fatalf("race loser error = %v, want ConflictError", loser)
	}

	events, err := store.Replay(context.Background(), thread.ThreadID, 0, 0)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	accepted := 0
	for _, event := range events {
		if event.Type != agoprotocol.EventMessageAccepted {
			continue
		}
		var payload struct {
			QueueItemID string `json:"queue_item_id"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatalf("decode accepted event: %v", err)
		}
		if payload.QueueItemID == targetQueueItemID {
			accepted++
		}
	}
	wantAccepted := 0
	if completeErr == nil {
		wantAccepted = 1
	}
	if accepted != wantAccepted {
		t.Fatalf("target message accepted %d times, want %d", accepted, wantAccepted)
	}
}

func TestInterruptAndSubmitWaitsForCancellationSettlement(t *testing.T) {
	store := openTestStore(t)
	thread := mustCreateThread(t, store, "create-interrupt")
	started := mustSubmit(t, store, thread.ThreadID, "first", agoprotocol.QueueNormal)

	interrupting, err := store.InterruptAndSubmit(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandTurnInterrupt, "interrupt"), started.ActiveTurnID, MessageInput{
		Content: json.RawMessage(`{"text":"replacement"}`),
		Class:   agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatalf("InterruptAndSubmit() error = %v", err)
	}
	if interrupting.Activity != agoprotocol.ActivityRunning || interrupting.ActiveTurnID != started.ActiveTurnID || len(interrupting.Queue) != 1 {
		t.Fatalf("InterruptAndSubmit() state = %#v, want old turn settling with one replacement", interrupting)
	}
	if interrupting.Queue[0].State != agoprotocol.QueueItemInterruptPending {
		t.Fatalf("replacement state = %q, want interrupt-pending", interrupting.Queue[0].State)
	}

	settled, err := store.SettleCancellation(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandTurnSettleCancelled, "settle-interrupt"), started.ActiveTurnID)
	if err != nil {
		t.Fatalf("SettleCancellation() error = %v", err)
	}
	if settled.Activity != agoprotocol.ActivityRunning || settled.ActiveTurnID == "" || settled.ActiveTurnID == started.ActiveTurnID || len(settled.Queue) != 0 {
		t.Fatalf("SettleCancellation() state = %#v, want replacement started exactly once", settled)
	}
}

func TestCancelWithoutReplacementSettlesToIdle(t *testing.T) {
	store := openTestStore(t)
	thread := mustCreateThread(t, store, "create-cancel")
	started := mustSubmit(t, store, thread.ThreadID, "first", agoprotocol.QueueNormal)

	cancelling, err := store.Cancel(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandTurnCancel, "cancel"), started.ActiveTurnID)
	if err != nil {
		t.Fatalf("Cancel() error = %v", err)
	}
	if cancelling.ActiveTurnID != started.ActiveTurnID || !cancelling.CancelRequested {
		t.Fatalf("Cancel() state = %#v, want cancellation requested on active turn", cancelling)
	}

	settled, err := store.SettleCancellation(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandTurnSettleCancelled, "settle-cancel"), started.ActiveTurnID)
	if err != nil {
		t.Fatalf("SettleCancellation() error = %v", err)
	}
	if settled.Activity != agoprotocol.ActivityIdle || settled.ActiveTurnID != "" || settled.CancelRequested {
		t.Fatalf("SettleCancellation() state = %#v, want idle", settled)
	}
}

func TestSteerVersusCompletionHasOneSequenceWinner(t *testing.T) {
	store := openTestStore(t)
	thread := mustCreateThread(t, store, "steer-complete-race")
	active := mustSubmit(t, store, thread.ThreadID, "race-active", agoprotocol.QueueNormal)
	queued := mustSubmit(t, store, thread.ThreadID, "race-queued", agoprotocol.QueueNormal)
	steerCommand := mailboxCommand(thread.ThreadID, agoprotocol.CommandMessageSteer, "race-steer")
	steerCommand.ExpectedSequence = sequencePointer(queued.LastSequence)
	completeCommand := mailboxCommand(thread.ThreadID, agoprotocol.CommandTurnComplete, "race-complete")
	completeCommand.ExpectedSequence = sequencePointer(queued.LastSequence)
	errors := make(chan error, 2)
	start := make(chan struct{})
	go func() {
		<-start
		_, err := store.Steer(context.Background(), steerCommand, queued.Queue[0].QueueItemID, active.ActiveTurnID)
		errors <- err
	}()
	go func() {
		<-start
		_, err := store.CompleteTurn(context.Background(), completeCommand, active.ActiveTurnID)
		errors <- err
	}()
	close(start)
	successes := 0
	for range 2 {
		if err := <-errors; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("steer/completion successes = %d, want exactly one", successes)
	}
	state, err := store.Mailbox(context.Background(), thread.ThreadID)
	if err != nil || (state.LastSequence != queued.LastSequence+1 && state.LastSequence != queued.LastSequence+3) {
		t.Fatalf("steer/completion final state = %#v, %v", state, err)
	}
}

func TestInterruptVersusCompletionHasOneSequenceWinner(t *testing.T) {
	store := openTestStore(t)
	thread := mustCreateThread(t, store, "interrupt-complete-race")
	active := mustSubmit(t, store, thread.ThreadID, "interrupt-race-active", agoprotocol.QueueNormal)
	interruptCommand := mailboxCommand(thread.ThreadID, agoprotocol.CommandTurnInterrupt, "race-interrupt")
	interruptCommand.ExpectedSequence = sequencePointer(active.LastSequence)
	completeCommand := mailboxCommand(thread.ThreadID, agoprotocol.CommandTurnComplete, "interrupt-race-complete")
	completeCommand.ExpectedSequence = sequencePointer(active.LastSequence)
	errors := make(chan error, 2)
	start := make(chan struct{})
	go func() {
		<-start
		_, err := store.InterruptAndSubmit(context.Background(), interruptCommand, active.ActiveTurnID, MessageInput{Content: json.RawMessage(`{"text":"replacement"}`), Class: agoprotocol.QueueNormal})
		errors <- err
	}()
	go func() {
		<-start
		_, err := store.CompleteTurn(context.Background(), completeCommand, active.ActiveTurnID)
		errors <- err
	}()
	close(start)
	successes := 0
	for range 2 {
		if err := <-errors; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("interrupt/completion successes = %d, want exactly one", successes)
	}
	state, err := store.Mailbox(context.Background(), thread.ThreadID)
	if err != nil || (state.LastSequence != active.LastSequence+1 && state.LastSequence != active.LastSequence+2) {
		t.Fatalf("interrupt/completion final state = %#v, %v", state, err)
	}
}

func TestMailboxQueueAndActiveTurnSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	thread := mustCreateThread(t, store, "create-reopen-mailbox")
	mustSubmit(t, store, thread.ThreadID, "first", agoprotocol.QueueNormal)
	want := mustSubmit(t, store, thread.ThreadID, "second", agoprotocol.QueueNormal)
	want.Events = nil
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.Mailbox(context.Background(), thread.ThreadID)
	if err != nil {
		t.Fatalf("Mailbox() after reopen error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Mailbox() after reopen = %#v, want %#v", got, want)
	}
}

func mustSubmit(t *testing.T, store *Store, threadID, key string, class agoprotocol.QueueClass) MailboxState {
	t.Helper()
	state, err := store.Submit(context.Background(), mailboxCommand(threadID, agoprotocol.CommandMessageSubmit, "submit-"+key), MessageInput{
		Content: json.RawMessage(`{"text":"` + key + `"}`),
		Class:   class,
	})
	if err != nil {
		t.Fatalf("Submit(%q) error = %v", key, err)
	}
	return state
}

func mailboxCommand(threadID string, commandType agoprotocol.CommandType, key string) agoprotocol.Command {
	return agoprotocol.Command{
		SchemaVersion:  agoprotocol.SchemaVersion,
		CommandID:      "cmd-" + key,
		IdempotencyKey: "request-" + key,
		ActorID:        "user-1",
		Type:           commandType,
		ThreadID:       threadID,
	}
}

func sequencePointer(sequence uint64) *uint64 {
	return &sequence
}
