package agothreadstore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claudexflow/internal/agoprotocol"
)

func TestClientProjectionPagesAndSnapshotAgreement(t *testing.T) {
	store := openTestStore(t)
	created := mustCreateThread(t, store, "projection-create")
	for i := 0; i < 4; i++ {
		_, err := store.Append(context.Background(), appendCommand(created.ThreadID, "projection-command-"+string(rune('a'+i)), "projection-key-"+string(rune('a'+i))), EventDraft{Type: agoprotocol.EventMessageAccepted, Visibility: agoprotocol.VisibilityUser, Payload: json.RawMessage(`{"original":true}`)})
		if err != nil {
			t.Fatal(err)
		}
	}

	first, err := store.ClientProjection(context.Background(), created.ThreadID, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.SchemaVersion != 1 || len(first.Events) != 2 || first.Events[0].Sequence != 1 || first.NextAfterSequence != 2 || !first.HasMore {
		t.Fatalf("first page = %#v", first)
	}
	middle, err := store.ClientProjection(context.Background(), created.ThreadID, first.NextAfterSequence, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(middle.Events) != 2 || middle.Events[0].Sequence != 3 || middle.NextAfterSequence != 4 || !middle.HasMore {
		t.Fatalf("middle page = %#v", middle)
	}
	final, err := store.ClientProjection(context.Background(), created.ThreadID, middle.NextAfterSequence, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Events) != 1 || final.Events[0].Sequence != 5 || final.NextAfterSequence != 5 || final.HasMore {
		t.Fatalf("final page = %#v", final)
	}
	if final.SnapshotSequence != final.Thread.LastSequence || final.SnapshotSequence != final.Mailbox.LastSequence {
		t.Fatalf("snapshot disagreement: %#v", final)
	}
	empty, err := store.ClientProjection(context.Background(), created.ThreadID, final.NextAfterSequence, 2)
	if err != nil || len(empty.Events) != 0 || empty.NextAfterSequence != final.NextAfterSequence {
		t.Fatalf("empty page = %#v, %v", empty, err)
	}
	encoded, _ := json.Marshal(empty)
	if strings.Contains(string(encoded), `"events":null`) || strings.Contains(string(encoded), `"dialogs":null`) || strings.Contains(string(encoded), `"queue":null`) {
		t.Fatalf("non-deterministic arrays: %s", encoded)
	}
}

func TestClientProjectionRejectsInvalidArguments(t *testing.T) {
	store := openTestStore(t)
	created := mustCreateThread(t, store, "projection-invalid")
	for _, tc := range []struct {
		id    string
		after uint64
		limit int
	}{{"", 0, 1}, {created.ThreadID, 0, 0}, {created.ThreadID, 0, 1001}, {created.ThreadID, 2, 1}} {
		if _, err := store.ClientProjection(context.Background(), tc.id, tc.after, tc.limit); err == nil {
			t.Fatalf("ClientProjection(%q,%d,%d) succeeded", tc.id, tc.after, tc.limit)
		}
	}
}

func TestClientProjectionIncludesAllDialogStatesAndSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "projection.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	created := mustCreateThread(t, store, "projection-dialog")
	active, err := store.Submit(context.Background(), mailboxCommand(created.ThreadID, agoprotocol.CommandMessageSubmit, "projection-submit"), MessageInput{Content: json.RawMessage(`{"text":"go"}`), Class: agoprotocol.QueueNormal})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := store.CreatePendingDialog(context.Background(), CreateDialogInput{ThreadID: created.ThreadID, TurnID: active.ActiveTurnID, PluginID: "plugin", InvocationID: "one", Deadline: time.Now().Add(time.Hour), RequestType: "confirm", Request: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ResolveDialog(context.Background(), ResolveDialogInput{DialogID: resolved.DialogID, ResolverID: "client", ExpectedRevision: 1, Response: json.RawMessage(`true`)}); err != nil {
		t.Fatal(err)
	}
	if _, err = store.CreatePendingDialog(context.Background(), CreateDialogInput{ThreadID: created.ThreadID, TurnID: active.ActiveTurnID, PluginID: "plugin", InvocationID: "two", Deadline: time.Now().Add(time.Hour), RequestType: "confirm", Request: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	projection, err := store.ClientProjection(context.Background(), created.ThreadID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(projection.Dialogs) != 2 || projection.Dialogs[0].State != DialogResolved || projection.Dialogs[1].State != DialogPending {
		t.Fatalf("dialogs = %#v", projection.Dialogs)
	}
}
