package agothreadstore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"claudexflow/internal/agoprotocol"
)

func TestCreateConfiguredThreadPersistsExplicitExecutionIdentity(t *testing.T) {
	store := openTestStore(t)
	spec := ThreadSpec{
		Title:     "Implement durable runtime",
		Workspace: filepath.Join(t.TempDir(), "project"),
		Mode:      agoprotocol.AgentModeMedium,
		Executor:  agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal},
	}
	created, err := store.CreateConfiguredThread(context.Background(), createCommand("cmd-configured", "request-configured"), spec)
	if err != nil {
		t.Fatalf("CreateConfiguredThread() error = %v", err)
	}
	thread, err := store.Thread(context.Background(), created.ThreadID)
	if err != nil {
		t.Fatalf("Thread() error = %v", err)
	}
	if thread.Title != spec.Title || thread.Workspace != spec.Workspace || thread.Mode != spec.Mode || thread.Executor != spec.Executor {
		t.Fatalf("Thread() = %#v, want spec %#v", thread, spec)
	}
}

func TestListThreadsReturnsEveryConfiguredThreadDeterministically(t *testing.T) {
	store := openTestStore(t)
	for index, title := range []string{"First", "Second"} {
		_, err := store.CreateConfiguredThread(context.Background(), createCommand("list-command-"+title, "list-request-"+title), ThreadSpec{
			Title:     title,
			Workspace: filepath.Join(t.TempDir(), "workspace"),
			Mode:      agoprotocol.AgentModeMedium,
			Executor:  agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal},
		})
		if err != nil {
			t.Fatalf("CreateConfiguredThread(%d) error = %v", index, err)
		}
	}
	threads, err := store.ListThreads(context.Background())
	if err != nil {
		t.Fatalf("ListThreads() error = %v", err)
	}
	if len(threads) != 2 || threads[0].ThreadID >= threads[1].ThreadID {
		t.Fatalf("ListThreads() = %#v, want two records ordered by thread ID", threads)
	}
	if threads[0].Title == threads[1].Title || threads[0].Workspace == "" || threads[1].Executor.Type != agoprotocol.ExecutorLocal {
		t.Fatalf("ListThreads() lost configured identity: %#v", threads)
	}
}

func TestCreateConfiguredThreadRejectsImplicitExecutor(t *testing.T) {
	store := openTestStore(t)
	_, err := store.CreateConfiguredThread(context.Background(), createCommand("cmd-invalid-config", "request-invalid-config"), ThreadSpec{
		Workspace: filepath.Join(t.TempDir(), "project"),
		Mode:      agoprotocol.AgentModeMedium,
	})
	if err == nil {
		t.Fatal("CreateConfiguredThread() accepted a missing executor target")
	}
}

func TestCreateThreadIsAtomicAndIdempotent(t *testing.T) {
	store := openTestStore(t)
	command := createCommand("cmd-create", "request-create")

	first, err := store.CreateThread(context.Background(), command, json.RawMessage(`{"title":"First task"}`))
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	if first.ThreadID == "" {
		t.Fatal("CreateThread() returned an empty thread ID")
	}
	if len(first.Events) != 1 || first.Events[0].Type != agoprotocol.EventThreadCreated {
		t.Fatalf("CreateThread() events = %#v, want one thread.created event", first.Events)
	}
	if first.Events[0].ThreadID != first.ThreadID || first.Events[0].Sequence != 1 {
		t.Fatalf("initial event = %#v, want thread ID %q at sequence 1", first.Events[0], first.ThreadID)
	}

	retry, err := store.CreateThread(context.Background(), command, json.RawMessage(`{"title":"First task"}`))
	if err != nil {
		t.Fatalf("idempotent CreateThread() error = %v", err)
	}
	if !reflect.DeepEqual(retry, first) {
		t.Fatalf("idempotent result = %#v, want original %#v", retry, first)
	}

	events, err := store.Replay(context.Background(), first.ThreadID, 0, 0)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Replay() returned %d events after retry, want 1", len(events))
	}
}

func TestAppendPersistsCommandBeforeAcknowledgementAndRejectsChangedRetry(t *testing.T) {
	store := openTestStore(t)
	created := mustCreateThread(t, store, "request-create")
	command := appendCommand(created.ThreadID, "cmd-append", "request-append")
	draft := EventDraft{
		Type:       agoprotocol.EventMessageAccepted,
		Visibility: agoprotocol.VisibilityUser,
		Payload:    json.RawMessage(`{"text":"hello"}`),
	}

	result, err := store.Append(context.Background(), command, draft)
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	retry, err := store.Append(context.Background(), command, draft)
	if err != nil {
		t.Fatalf("idempotent Append() error = %v", err)
	}
	if !reflect.DeepEqual(retry, result) {
		t.Fatalf("idempotent append result = %#v, want %#v", retry, result)
	}

	changed := draft
	changed.Payload = json.RawMessage(`{"text":"different"}`)
	if _, err := store.Append(context.Background(), command, changed); err == nil {
		t.Fatal("Append() accepted an idempotency-key retry with different content")
	}
}

func TestAppendTurnEventsIsIdempotentAndRejectsLateExecutorOutput(t *testing.T) {
	store := openTestStore(t)
	created := mustCreateThread(t, store, "turn-events-create")
	started, err := store.Submit(context.Background(), agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: "turn-events-submit", IdempotencyKey: "turn-events-submit",
		ActorID: "user-1", Type: agoprotocol.CommandMessageSubmit, ThreadID: created.ThreadID,
	}, MessageInput{Class: agoprotocol.QueueNormal, Content: json.RawMessage(`{"text":"run"}`)})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	command := agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: "turn-events-1", IdempotencyKey: "turn-events-1",
		ActorID: "ago-coordinator", Type: agoprotocol.CommandTurnEventAppend, ThreadID: created.ThreadID,
	}
	draft := EventDraft{Type: agoprotocol.EventAgentStarted, Visibility: agoprotocol.VisibilityInternal, Payload: json.RawMessage(`{"executor_event_index":1}`)}
	appended, err := store.AppendTurnEvents(context.Background(), command, started.ActiveTurnID, draft)
	if err != nil {
		t.Fatalf("AppendTurnEvents() error = %v", err)
	}
	retry, err := store.AppendTurnEvents(context.Background(), command, started.ActiveTurnID, draft)
	if err != nil || !reflect.DeepEqual(retry, appended) {
		t.Fatalf("idempotent AppendTurnEvents() = %#v, %v; want %#v", retry, err, appended)
	}

	complete := command
	complete.Type = agoprotocol.CommandTurnComplete
	complete.CommandID = "turn-events-complete"
	complete.IdempotencyKey = "turn-events-complete"
	if _, err := store.CompleteTurn(context.Background(), complete, started.ActiveTurnID); err != nil {
		t.Fatalf("CompleteTurn() error = %v", err)
	}
	late := command
	late.CommandID = "turn-events-late"
	late.IdempotencyKey = "turn-events-late"
	if _, err := store.AppendTurnEvents(context.Background(), late, started.ActiveTurnID, EventDraft{Type: agoprotocol.EventAgentSettled, Visibility: agoprotocol.VisibilityInternal}); err == nil {
		t.Fatal("AppendTurnEvents() accepted output after terminal settlement")
	}
	events, err := store.Replay(context.Background(), created.ThreadID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 || events[3].Type != agoprotocol.EventAgentStarted || events[4].Type != agoprotocol.EventTurnCompleted {
		t.Fatalf("durable events = %#v", events)
	}
}

func TestSequencesAndReplayAreIndependentPerThread(t *testing.T) {
	store := openTestStore(t)
	first := mustCreateThread(t, store, "create-first")
	second := mustCreateThread(t, store, "create-second")

	var wait sync.WaitGroup
	errors := make(chan error, 4)
	for index := 0; index < 2; index++ {
		for _, threadID := range []string{first.ThreadID, second.ThreadID} {
			wait.Add(1)
			go func(threadID string, index int) {
				defer wait.Done()
				_, err := store.Append(context.Background(), appendCommand(threadID, threadID+"-cmd-"+string(rune('a'+index)), threadID+"-request-"+string(rune('a'+index))), EventDraft{
					Type:       agoprotocol.EventMessageAccepted,
					Visibility: agoprotocol.VisibilityUser,
					Payload:    json.RawMessage(`{"text":"message"}`),
				})
				errors <- err
			}(threadID, index)
		}
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatalf("concurrent Append() error = %v", err)
		}
	}

	for _, threadID := range []string{first.ThreadID, second.ThreadID} {
		events, err := store.Replay(context.Background(), threadID, 1, 0)
		if err != nil {
			t.Fatalf("Replay(%q) error = %v", threadID, err)
		}
		if len(events) != 2 || events[0].Sequence != 2 || events[1].Sequence != 3 {
			t.Fatalf("Replay(%q, after 1) = %#v, want sequences 2 and 3", threadID, events)
		}
	}
}

func TestCloseAndReopenRestoresAuthoritativeStateAndImmutableHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ago.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	created := mustCreateThread(t, store, "request-create")
	appended, err := store.Append(context.Background(), appendCommand(created.ThreadID, "cmd-append", "request-append"), EventDraft{
		Type:       agoprotocol.EventMessageAccepted,
		Visibility: agoprotocol.VisibilityUser,
		Payload:    json.RawMessage(`{"text":"durable"}`),
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	want, err := store.Replay(context.Background(), created.ThreadID, 0, 0)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := reopened.Replay(context.Background(), created.ThreadID, 0, 0)
	if err != nil {
		t.Fatalf("Replay() after reopen error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reopened events = %#v, want %#v", got, want)
	}

	retry, err := reopened.Append(context.Background(), appendCommand(created.ThreadID, "cmd-append", "request-append"), EventDraft{
		Type:       agoprotocol.EventMessageAccepted,
		Visibility: agoprotocol.VisibilityUser,
		Payload:    json.RawMessage(`{"text":"durable"}`),
	})
	if err != nil {
		t.Fatalf("idempotent Append() after reopen error = %v", err)
	}
	if !reflect.DeepEqual(retry, appended) {
		t.Fatalf("reopened idempotent result = %#v, want %#v", retry, appended)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func mustCreateThread(t *testing.T, store *Store, idempotencyKey string) CommandResult {
	t.Helper()
	result, err := store.CreateThread(context.Background(), createCommand("cmd-"+idempotencyKey, idempotencyKey), nil)
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	return result
}

func createCommand(commandID, idempotencyKey string) agoprotocol.Command {
	return agoprotocol.Command{
		SchemaVersion:  agoprotocol.SchemaVersion,
		CommandID:      commandID,
		IdempotencyKey: idempotencyKey,
		ActorID:        "user-1",
		Type:           agoprotocol.CommandThreadCreate,
	}
}

func appendCommand(threadID, commandID, idempotencyKey string) agoprotocol.Command {
	return agoprotocol.Command{
		SchemaVersion:  agoprotocol.SchemaVersion,
		CommandID:      commandID,
		IdempotencyKey: idempotencyKey,
		ActorID:        "user-1",
		Type:           agoprotocol.CommandMessageAppend,
		ThreadID:       threadID,
	}
}
