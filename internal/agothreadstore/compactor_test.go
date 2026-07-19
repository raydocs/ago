package agothreadstore

import (
	"context"
	"encoding/json"
	"testing"

	"claudexflow/internal/agoprotocol"
)

func TestCompactionBudgetUsesModelWindowOutputReserveAndNinetyPercentThreshold(t *testing.T) {
	budget := CompactionBudget{ContextWindowTokens: 100_000, ReservedOutputTokens: 10_000, TriggerRatio: 0.90}
	if got := budget.TriggerTokens(); got != 81_000 {
		t.Fatalf("TriggerTokens() = %d, want 81000", got)
	}
	if budget.ShouldCompact(80_999) {
		t.Fatal("compaction triggered below threshold")
	}
	if !budget.ShouldCompact(81_000) {
		t.Fatal("compaction did not trigger at threshold")
	}
}

func TestCompactorAutomaticallyRecordsSummaryAtThreshold(t *testing.T) {
	store := openTestStore(t)
	created := mustCreateThread(t, store, "automatic-compaction")
	_, err := store.Append(context.Background(), appendCommand(created.ThreadID, "automatic-message", "automatic-message"), EventDraft{
		Type: agoprotocol.EventMessageAccepted, Visibility: agoprotocol.VisibilityUser, Payload: json.RawMessage(`{"text":"retain original"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	compactor := NewCompactor(store, CompactionBudget{ContextWindowTokens: 100, ReservedOutputTokens: 10, TriggerRatio: 0.9})
	called := 0
	record, compacted, err := compactor.MaybeCompact(context.Background(), created.ThreadID, 81, func(_ ContextProjection) (string, error) {
		called++
		return "objective; decisions; verification; next action", nil
	})
	if err != nil {
		t.Fatalf("MaybeCompact() error = %v", err)
	}
	if !compacted || called != 1 || record.ThroughSequence != 2 {
		t.Fatalf("automatic compaction = %#v, compacted=%v called=%d", record, compacted, called)
	}
	before, err := store.Replay(context.Background(), created.ThreadID, 0, 0)
	if err != nil || len(before) != 3 || before[1].Type != agoprotocol.EventMessageAccepted {
		t.Fatalf("history after compaction = %#v, %v", before, err)
	}
	record, compacted, err = compactor.MaybeCompact(context.Background(), created.ThreadID, 80, func(ContextProjection) (string, error) {
		called++
		return "must not run", nil
	})
	if err != nil || compacted || record.CompactionID != "" || called != 1 {
		t.Fatalf("below threshold = %#v compacted=%v called=%d err=%v", record, compacted, called, err)
	}
}

func TestCompactorExplicitBoundaryExcludesCurrentPromptAndRunsOnce(t *testing.T) {
	store := openTestStore(t)
	created := mustCreateThread(t, store, "bounded-compaction")
	for _, key := range []string{"historical", "current"} {
		if _, err := store.Append(context.Background(), appendCommand(created.ThreadID, key, key), EventDraft{Type: agoprotocol.EventMessageAccepted, Visibility: agoprotocol.VisibilityUser, Payload: json.RawMessage(`{"text":"` + key + `"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	compactor := NewCompactor(store, CompactionBudget{ContextWindowTokens: 100, ReservedOutputTokens: 10, TriggerRatio: 0.9})
	called := 0
	record, compacted, err := compactor.MaybeCompactThrough(context.Background(), created.ThreadID, 2, 81, func(projection ContextProjection) (string, error) {
		called++
		if len(projection.Tail) != 2 || projection.Tail[1].Sequence != 2 {
			t.Fatalf("summary projection crossed boundary: %#v", projection)
		}
		return "historical only", nil
	})
	if err != nil || !compacted || called != 1 || record.ThroughSequence != 2 {
		t.Fatalf("compaction = %#v compacted=%v called=%d err=%v", record, compacted, called, err)
	}
	projection, err := store.ContextProjection(context.Background(), created.ThreadID)
	if err != nil || projection.Compaction == nil || len(projection.Tail) != 1 || projection.Tail[0].Sequence != 3 {
		t.Fatalf("post-compaction projection = %#v, %v", projection, err)
	}
	_, compacted, err = compactor.MaybeCompactThrough(context.Background(), created.ThreadID, 2, 81, func(ContextProjection) (string, error) {
		called++
		return "duplicate", nil
	})
	if err != nil || compacted || called != 1 {
		t.Fatalf("same boundary repeated: compacted=%v called=%d err=%v", compacted, called, err)
	}
}
