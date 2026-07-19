package agothreadstore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"claudexflow/internal/agoprotocol"
)

func TestPluginDialogCreateListResolveAndRecover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dialogs.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	created := mustCreateThread(t, store, "dialog-thread")
	active, err := store.Submit(context.Background(), mailboxCommand(created.ThreadID, agoprotocol.CommandMessageSubmit, "dialog-active"), MessageInput{Content: json.RawMessage(`{"text":"active"}`), Class: agoprotocol.QueueNormal})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Date(2026, 7, 20, 1, 2, 3, 0, time.UTC)
	dialog, err := store.CreatePendingDialog(context.Background(), CreateDialogInput{
		ThreadID: created.ThreadID, TurnID: active.ActiveTurnID, PluginID: "approval", Generation: 4,
		InvocationID: "invoke-1", Deadline: deadline, RequestType: "confirmation",
		Request: json.RawMessage(`{"prompt":"continue?"}`), ExpectedSequence: pointer(active.LastSequence),
	})
	if err != nil {
		t.Fatalf("CreatePendingDialog() error = %v", err)
	}
	if dialog.DialogID == "" || dialog.State != DialogPending || dialog.Revision != 1 || dialog.RequestType != "confirmation" {
		t.Fatalf("dialog = %#v", dialog)
	}
	waiting, err := store.Mailbox(context.Background(), created.ThreadID)
	if err != nil || waiting.Activity != agoprotocol.ActivityAwaitingApproval || waiting.ActiveTurnID != active.ActiveTurnID {
		t.Fatalf("mailbox while dialog pending = %#v, %v", waiting, err)
	}
	pending, err := store.ListPendingDialogs(context.Background(), created.ThreadID)
	if err != nil || !reflect.DeepEqual(pending, []PluginDialog{dialog}) {
		t.Fatalf("pending = %#v, %v", pending, err)
	}

	resolved, err := store.ResolveDialog(context.Background(), ResolveDialogInput{
		DialogID: dialog.DialogID, ResolverID: "client-1", ExpectedRevision: 1,
		ExpectedSequence: pointer(dialog.RequestedSequence), Response: json.RawMessage(`{"confirmed":true}`),
	})
	if err != nil {
		t.Fatalf("ResolveDialog() error = %v", err)
	}
	if resolved.State != DialogResolved || resolved.Revision != 2 || resolved.ResolvedSequence != dialog.RequestedSequence+1 {
		t.Fatalf("resolved = %#v", resolved)
	}
	running, err := store.Mailbox(context.Background(), created.ThreadID)
	if err != nil || running.Activity != agoprotocol.ActivityRunning || running.ActiveTurnID != active.ActiveTurnID {
		t.Fatalf("mailbox after dialog resolution = %#v, %v", running, err)
	}
	retry, err := store.ResolveDialog(context.Background(), ResolveDialogInput{
		DialogID: dialog.DialogID, ResolverID: "client-1", ExpectedRevision: 1,
		ExpectedSequence: pointer(dialog.RequestedSequence), Response: json.RawMessage(`{"confirmed":true}`),
	})
	if err != nil || !reflect.DeepEqual(retry, resolved) {
		t.Fatalf("retry = %#v, %v", retry, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	all, err := reopened.ListDialogs(context.Background(), created.ThreadID)
	if err != nil || !reflect.DeepEqual(all, []PluginDialog{resolved}) {
		t.Fatalf("reopened dialogs = %#v, %v", all, err)
	}
	events, err := reopened.Replay(context.Background(), created.ThreadID, active.LastSequence, 0)
	if err != nil || len(events) != 2 || events[0].Type != agoprotocol.EventPluginDialogRequested || events[1].Type != agoprotocol.EventPluginDialogResolved {
		t.Fatalf("dialog events = %#v, %v", events, err)
	}
}

func TestPluginDialogResolveHasExactlyOneWinner(t *testing.T) {
	store := openTestStore(t)
	thread := mustCreateThread(t, store, "resolve-race-thread")
	active, err := store.Submit(context.Background(), mailboxCommand(thread.ThreadID, agoprotocol.CommandMessageSubmit, "dialog-race-active"), MessageInput{Content: json.RawMessage(`{"text":"active"}`), Class: agoprotocol.QueueNormal})
	if err != nil {
		t.Fatal(err)
	}
	dialog, err := store.CreatePendingDialog(context.Background(), CreateDialogInput{ThreadID: thread.ThreadID, TurnID: active.ActiveTurnID, PluginID: "p", Generation: 1, InvocationID: "i", Deadline: time.Now().Add(time.Hour), RequestType: "choice", Request: json.RawMessage(`{"choices":[1,2]}`)})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, answer := range []string{`{"choice":1}`, `{"choice":2}`} {
		wg.Add(1)
		go func(answer string) {
			defer wg.Done()
			_, err := store.ResolveDialog(context.Background(), ResolveDialogInput{DialogID: dialog.DialogID, ResolverID: answer, ExpectedRevision: 1, Response: json.RawMessage(answer)})
			results <- err
		}(answer)
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("successful resolves = %d, want 1", winners)
	}
}

func pointer(value uint64) *uint64 { return &value }
