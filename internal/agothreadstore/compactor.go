package agothreadstore

import (
	"context"
	"fmt"
	"strconv"

	"claudexflow/internal/agoprotocol"
)

type CompactionBudget struct {
	ContextWindowTokens  int64
	ReservedOutputTokens int64
	TriggerRatio         float64
}

func (budget CompactionBudget) TriggerTokens() int64 {
	ratio := budget.TriggerRatio
	if ratio == 0 {
		ratio = 0.90
	}
	available := budget.ContextWindowTokens - budget.ReservedOutputTokens
	if available <= 0 || ratio <= 0 || ratio > 1 {
		return 0
	}
	return int64(float64(available) * ratio)
}

func (budget CompactionBudget) ShouldCompact(estimatedInputTokens int64) bool {
	trigger := budget.TriggerTokens()
	return trigger > 0 && estimatedInputTokens >= trigger
}

type Compactor struct {
	store  *Store
	budget CompactionBudget
}

func NewCompactor(store *Store, budget CompactionBudget) *Compactor {
	return &Compactor{store: store, budget: budget}
}

func (compactor *Compactor) MaybeCompact(ctx context.Context, threadID string, estimatedInputTokens int64, summarize func(ContextProjection) (string, error)) (CompactionRecord, bool, error) {
	if compactor == nil || compactor.store == nil {
		return CompactionRecord{}, false, fmt.Errorf("compactor store is required")
	}
	if !compactor.budget.ShouldCompact(estimatedInputTokens) {
		return CompactionRecord{}, false, nil
	}
	if summarize == nil {
		return CompactionRecord{}, false, fmt.Errorf("compaction summarizer is required")
	}
	thread, err := compactor.store.Thread(ctx, threadID)
	if err != nil {
		return CompactionRecord{}, false, err
	}
	return compactor.MaybeCompactThrough(ctx, threadID, thread.LastSequence, estimatedInputTokens, summarize)
}

func (compactor *Compactor) MaybeCompactThrough(ctx context.Context, threadID string, throughSequence uint64, estimatedInputTokens int64, summarize func(ContextProjection) (string, error)) (CompactionRecord, bool, error) {
	if compactor == nil || compactor.store == nil {
		return CompactionRecord{}, false, fmt.Errorf("compactor store is required")
	}
	if !compactor.budget.ShouldCompact(estimatedInputTokens) {
		return CompactionRecord{}, false, nil
	}
	if summarize == nil {
		return CompactionRecord{}, false, fmt.Errorf("compaction summarizer is required")
	}
	projection, err := compactor.store.ContextProjection(ctx, threadID)
	if err != nil {
		return CompactionRecord{}, false, err
	}
	if projection.Compaction != nil && projection.Compaction.ThroughSequence >= throughSequence {
		return CompactionRecord{}, false, nil
	}
	filtered := projection.Tail[:0]
	for _, event := range projection.Tail {
		if event.Sequence <= throughSequence {
			filtered = append(filtered, event)
		}
	}
	projection.Tail = filtered
	summary, err := summarize(projection)
	if err != nil {
		return CompactionRecord{}, false, fmt.Errorf("summarize thread for compaction: %w", err)
	}
	boundary := throughSequence
	key := threadID + ":" + strconv.FormatUint(boundary, 10)
	record, err := compactor.store.RecordCompaction(ctx, agoprotocol.Command{
		SchemaVersion:  agoprotocol.SchemaVersion,
		CommandID:      "compactor:" + key,
		IdempotencyKey: "compactor:" + key,
		ActorID:        "ago-compactor",
		Type:           agoprotocol.CommandThreadCompact,
		ThreadID:       threadID,
	}, CompactionInput{ThroughSequence: boundary, Summary: summary})
	if err != nil {
		return CompactionRecord{}, false, err
	}
	return record, true, nil
}
