package agothreadstore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"claudexflow/internal/agoprotocol"
)

func TestCompactionProjectionPreservesOriginalHistoryAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ago.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	created := mustCreateThread(t, store, "compaction-create")
	for index, text := range []string{"first", "second"} {
		_, err := store.Append(context.Background(), appendCommand(created.ThreadID, "compaction-message-"+text, "compaction-message-"+text), EventDraft{
			Type:       agoprotocol.EventMessageAccepted,
			Visibility: agoprotocol.VisibilityUser,
			Payload:    json.RawMessage(`{"text":"` + text + `"}`),
		})
		if err != nil {
			t.Fatalf("Append(%d) error = %v", index, err)
		}
	}
	command := appendCommand(created.ThreadID, "compact", "compact")
	command.Type = agoprotocol.CommandThreadCompact
	recorded, err := store.RecordCompaction(context.Background(), command, CompactionInput{
		ThroughSequence: 2,
		Summary:         "objective and first accepted message",
	})
	if err != nil {
		t.Fatalf("RecordCompaction() error = %v", err)
	}
	if recorded.EventSequence != 4 || recorded.ThroughSequence != 2 {
		t.Fatalf("compaction = %#v", recorded)
	}
	retry, err := store.RecordCompaction(context.Background(), command, CompactionInput{ThroughSequence: 2, Summary: "objective and first accepted message"})
	if err != nil {
		t.Fatalf("idempotent RecordCompaction() error = %v", err)
	}
	if retry != recorded {
		t.Fatalf("idempotent compaction = %#v, want %#v", retry, recorded)
	}
	if _, err := store.RecordCompaction(context.Background(), command, CompactionInput{ThroughSequence: 2, Summary: "changed retry"}); err == nil {
		t.Fatal("changed compaction retry was accepted")
	}
	projection, err := store.ContextProjection(context.Background(), created.ThreadID)
	if err != nil {
		t.Fatalf("ContextProjection() error = %v", err)
	}
	if projection.Compaction == nil || projection.Compaction.Summary != "objective and first accepted message" {
		t.Fatalf("projection compaction = %#v", projection.Compaction)
	}
	if len(projection.Tail) != 1 || projection.Tail[0].Sequence != 3 {
		t.Fatalf("projection tail = %#v, want original event 3 only", projection.Tail)
	}
	originals, err := store.Replay(context.Background(), created.ThreadID, 0, 0)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if len(originals) != 4 || originals[0].Sequence != 1 || originals[3].Type != agoprotocol.EventCompactionRecorded {
		t.Fatalf("original history = %#v", originals)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted, err := reopened.ContextProjection(context.Background(), created.ThreadID)
	if err != nil {
		t.Fatalf("ContextProjection() after restart error = %v", err)
	}
	if restarted.Compaction == nil || restarted.Compaction.ThroughSequence != 2 || len(restarted.Tail) != 1 || restarted.Tail[0].Sequence != 3 {
		t.Fatalf("restarted projection = %#v", restarted)
	}
}

func TestCompactionRejectsFutureOrRegressingBoundary(t *testing.T) {
	store := openTestStore(t)
	created := mustCreateThread(t, store, "compaction-boundaries")
	command := appendCommand(created.ThreadID, "compact-future", "compact-future")
	command.Type = agoprotocol.CommandThreadCompact
	if _, err := store.RecordCompaction(context.Background(), command, CompactionInput{ThroughSequence: 2, Summary: "future"}); err == nil {
		t.Fatal("future compaction boundary was accepted")
	}
	command = appendCommand(created.ThreadID, "compact-first", "compact-first")
	command.Type = agoprotocol.CommandThreadCompact
	if _, err := store.RecordCompaction(context.Background(), command, CompactionInput{ThroughSequence: 1, Summary: "first"}); err != nil {
		t.Fatalf("first compaction: %v", err)
	}
	command = appendCommand(created.ThreadID, "compact-regress", "compact-regress")
	command.Type = agoprotocol.CommandThreadCompact
	if _, err := store.RecordCompaction(context.Background(), command, CompactionInput{ThroughSequence: 1, Summary: "regress"}); err == nil {
		t.Fatal("regressing compaction boundary was accepted")
	}
}

func TestCompactionRejectsOversizedSummaryBeforePersistence(t *testing.T) {
	store := openTestStore(t)
	created := mustCreateThread(t, store, "compaction-oversized")
	command := appendCommand(created.ThreadID, "compact-oversized", "compact-oversized")
	command.Type = agoprotocol.CommandThreadCompact
	if _, err := store.RecordCompaction(context.Background(), command, CompactionInput{ThroughSequence: 1, Summary: strings.Repeat("x", MaxCompactionSummaryBytes+1)}); err == nil {
		t.Fatal("oversized compaction summary was accepted")
	}
	events, err := store.Replay(context.Background(), created.ThreadID, 0, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("oversized compaction mutated history: %#v, %v", events, err)
	}
}
