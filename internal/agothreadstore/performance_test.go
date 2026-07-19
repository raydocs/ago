package agothreadstore

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"claudexflow/internal/agoprotocol"
)

const (
	fiveThousandEventCount = 5_000
	halfEventCount         = 2_500
	projectionPageSize     = 1_000
	pendingQueueCount      = 500

	// These budgets are intentionally generous for race-enabled macOS CI. They
	// are not microbenchmarks: each is large enough to tolerate shared-runner
	// noise while still rejecting accidental O(n²) replay/projection behavior.
	maxReplayWall           = 5 * time.Second
	maxFirstProjectionWall  = 3 * time.Second
	maxReconnectWall        = 1 * time.Second
	maxTotalPaginationWall  = 8 * time.Second
	maxMeasuredCPUWall      = 15 * time.Second
	maxReplayAllocations    = 150_000
	maxReplayAllocatedBytes = 64 << 20
	maxReplayGrowthRatio    = 4.0
)

var (
	performanceEventSink      []agoprotocol.Event
	performanceProjectionSink ClientProjection
)

func TestFiveThousandAuthoritativeStorePerformance(t *testing.T) {
	ctx := context.Background()
	store, threadID := seedPerformanceStore(t, fiveThousandEventCount, pendingQueueCount)
	halfStore, halfThreadID := seedPerformanceStore(t, halfEventCount, 0)

	replayStarted := time.Now()
	events, err := store.Replay(ctx, threadID, 0, 0)
	replayWall := time.Since(replayStarted)
	if err != nil {
		t.Fatalf("Replay(5,000) error = %v", err)
	}
	assertPerformanceEventOrder(t, events, fiveThousandEventCount)
	if replayWall > maxReplayWall {
		t.Fatalf("Replay(5,000) wall time = %v, budget = %v", replayWall, maxReplayWall)
	}

	firstStarted := time.Now()
	first, err := store.ClientProjection(ctx, threadID, 0, projectionPageSize)
	firstProjectionWall := time.Since(firstStarted)
	if err != nil {
		t.Fatalf("first ClientProjection() error = %v", err)
	}
	assertProjectionPage(t, first, 0, 1, projectionPageSize, true, fiveThousandEventCount)
	if len(first.Mailbox.Queue) != pendingQueueCount {
		t.Fatalf("first projection queue length = %d, want %d", len(first.Mailbox.Queue), pendingQueueCount)
	}
	if firstProjectionWall > maxFirstProjectionWall {
		t.Fatalf("first projection wall time = %v, budget = %v", firstProjectionWall, maxFirstProjectionWall)
	}

	reconnectStarted := time.Now()
	reconnect, err := store.ClientProjection(ctx, threadID, fiveThousandEventCount-1, projectionPageSize)
	reconnectWall := time.Since(reconnectStarted)
	if err != nil {
		t.Fatalf("reconnect ClientProjection() error = %v", err)
	}
	assertProjectionPage(t, reconnect, fiveThousandEventCount-1, fiveThousandEventCount, fiveThousandEventCount, false, fiveThousandEventCount)
	if reconnectWall > maxReconnectWall {
		t.Fatalf("reconnect wall time = %v, budget = %v", reconnectWall, maxReconnectWall)
	}

	paginationStarted := time.Now()
	after := uint64(0)
	for pageNumber := 0; pageNumber < fiveThousandEventCount/projectionPageSize; pageNumber++ {
		page, pageErr := store.ClientProjection(ctx, threadID, after, projectionPageSize)
		if pageErr != nil {
			t.Fatalf("projection page %d error = %v", pageNumber+1, pageErr)
		}
		firstSequence := pageNumber*projectionPageSize + 1
		lastSequence := firstSequence + projectionPageSize - 1
		assertProjectionPage(t, page, int(after), firstSequence, lastSequence, pageNumber < 4, fiveThousandEventCount)
		after = page.NextAfterSequence
	}
	totalPaginationWall := time.Since(paginationStarted)
	if after != fiveThousandEventCount {
		t.Fatalf("pagination cursor = %d, want %d", after, fiveThousandEventCount)
	}
	if totalPaginationWall > maxTotalPaginationWall {
		t.Fatalf("total pagination wall time = %v, budget = %v", totalPaginationWall, maxTotalPaginationWall)
	}

	assertProjectionEventQueryUsesBoundedIndex(t, store, threadID)

	allocs := testing.AllocsPerRun(3, func() {
		performanceEventSink, err = store.Replay(ctx, threadID, 0, 0)
	})
	if err != nil {
		t.Fatalf("allocation Replay() error = %v", err)
	}
	if allocs > maxReplayAllocations {
		t.Fatalf("Replay(5,000) allocations = %.0f, budget = %d", allocs, maxReplayAllocations)
	}

	runtime.GC()
	var before, afterMemory runtime.MemStats
	runtime.ReadMemStats(&before)
	performanceEventSink, err = store.Replay(ctx, threadID, 0, 0)
	runtime.ReadMemStats(&afterMemory)
	if err != nil {
		t.Fatalf("memory Replay() error = %v", err)
	}
	runtime.KeepAlive(performanceEventSink)
	replayAllocatedBytes := afterMemory.TotalAlloc - before.TotalAlloc
	if replayAllocatedBytes > maxReplayAllocatedBytes {
		t.Fatalf("Replay(5,000) allocated bytes = %d, budget = %d", replayAllocatedBytes, maxReplayAllocatedBytes)
	}

	halfReplayWall := medianReplayWall(t, halfStore, halfThreadID)
	fullReplayWall := medianReplayWall(t, store, threadID)
	// Skip only genuinely tiny baselines where scheduler/timer noise dominates.
	if halfReplayWall >= 5*time.Millisecond {
		ratio := float64(fullReplayWall) / float64(halfReplayWall)
		if ratio > maxReplayGrowthRatio {
			t.Fatalf("Replay growth ratio 2,500->5,000 = %.2fx (%v -> %v), budget = %.1fx", ratio, halfReplayWall, fullReplayWall, maxReplayGrowthRatio)
		}
	}

	measuredCPUWall := replayWall + firstProjectionWall + reconnectWall + totalPaginationWall
	if measuredCPUWall > maxMeasuredCPUWall {
		t.Fatalf("measured operation wall time = %v, budget = %v", measuredCPUWall, maxMeasuredCPUWall)
	}
	t.Logf("5,000 events: replay=%v first_projection=%v reconnect=%v pagination=%v allocations=%.0f allocated_bytes=%d replay_2500=%v replay_5000=%v", replayWall, firstProjectionWall, reconnectWall, totalPaginationWall, allocs, replayAllocatedBytes, halfReplayWall, fullReplayWall)
}

func BenchmarkFiveThousandAuthoritativeStore(b *testing.B) {
	ctx := context.Background()
	store, threadID := seedPerformanceStore(b, fiveThousandEventCount, pendingQueueCount)

	b.Run("Replay", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var err error
			performanceEventSink, err = store.Replay(ctx, threadID, 0, 0)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("FirstProjection", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var err error
			performanceProjectionSink, err = store.ClientProjection(ctx, threadID, 0, projectionPageSize)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("ReconnectFrom4999", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			var err error
			performanceProjectionSink, err = store.ClientProjection(ctx, threadID, fiveThousandEventCount-1, projectionPageSize)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("PaginateAll", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			after := uint64(0)
			for after < fiveThousandEventCount {
				var err error
				performanceProjectionSink, err = store.ClientProjection(ctx, threadID, after, projectionPageSize)
				if err != nil {
					b.Fatal(err)
				}
				after = performanceProjectionSink.NextAfterSequence
			}
		}
	})
}

func seedPerformanceStore(tb testing.TB, eventCount, queueCount int) (*Store, string) {
	tb.Helper()
	store := openPerformanceStore(tb)
	created, err := store.CreateThread(context.Background(), createCommand(fmt.Sprintf("performance-create-%d", eventCount), fmt.Sprintf("performance-create-%d", eventCount)), nil)
	if err != nil {
		tb.Fatalf("CreateThread() error = %v", err)
	}
	tx, err := store.db.Begin()
	if err != nil {
		tb.Fatalf("begin performance seed = %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	eventStatement, err := tx.Prepare(`INSERT INTO events (thread_id, sequence, event_json) VALUES (?, ?, ?)`)
	if err != nil {
		tb.Fatalf("prepare event seed = %v", err)
	}
	defer eventStatement.Close()
	eventTypes := [...]agoprotocol.EventType{
		agoprotocol.EventMessageAccepted,
		agoprotocol.EventAgentStarted,
		agoprotocol.EventAssistantTextDelta,
		agoprotocol.EventToolCompleted,
		agoprotocol.EventAssistantCompleted,
	}
	for sequence := 2; sequence <= eventCount; sequence++ {
		event := agoprotocol.Event{
			SchemaVersion: agoprotocol.SchemaVersion,
			EventID:       fmt.Sprintf("E-performance-%05d", sequence),
			ThreadID:      created.ThreadID,
			Sequence:      uint64(sequence),
			Type:          eventTypes[sequence%len(eventTypes)],
			Visibility:    agoprotocol.VisibilityUser,
			Payload:       json.RawMessage(fmt.Sprintf(`{"index":%d,"text":"representative immutable performance event payload"}`, sequence)),
		}
		if err := event.Validate(); err != nil {
			tb.Fatalf("validate seed event %d = %v", sequence, err)
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			tb.Fatalf("encode seed event %d = %v", sequence, err)
		}
		if _, err := eventStatement.Exec(event.ThreadID, event.Sequence, encoded); err != nil {
			tb.Fatalf("insert seed event %d = %v", sequence, err)
		}
	}
	if _, err := tx.Exec(`UPDATE threads SET last_sequence=? WHERE thread_id=?`, eventCount, created.ThreadID); err != nil {
		tb.Fatalf("update seed sequence = %v", err)
	}
	queueStatement, err := tx.Prepare(`INSERT INTO pending_inputs (thread_id, queue_item_id, position, class, state, content_json) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		tb.Fatalf("prepare queue seed = %v", err)
	}
	defer queueStatement.Close()
	for index := 1; index <= queueCount; index++ {
		content := json.RawMessage(fmt.Sprintf(`{"text":"queued performance message %04d"}`, index))
		if !json.Valid(content) {
			tb.Fatalf("queue item %d content is invalid JSON", index)
		}
		if _, err := queueStatement.Exec(created.ThreadID, fmt.Sprintf("Q-performance-%04d", index), index, agoprotocol.QueueNormal, agoprotocol.QueueItemPending, []byte(content)); err != nil {
			tb.Fatalf("insert queue item %d = %v", index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		tb.Fatalf("commit performance seed = %v", err)
	}
	return store, created.ThreadID
}

func openPerformanceStore(tb testing.TB) *Store {
	tb.Helper()
	store, err := Open(tb.TempDir() + "/performance.db")
	if err != nil {
		tb.Fatalf("Open() error = %v", err)
	}
	tb.Cleanup(func() { _ = store.Close() })
	return store
}

func assertPerformanceEventOrder(t *testing.T, events []agoprotocol.Event, count int) {
	t.Helper()
	if len(events) != count {
		t.Fatalf("Replay() returned %d events, want %d", len(events), count)
	}
	for index, event := range events {
		wantSequence := uint64(index + 1)
		if event.Sequence != wantSequence {
			t.Fatalf("Replay() event %d sequence = %d, want %d", index, event.Sequence, wantSequence)
		}
		if index > 0 && event.EventID != fmt.Sprintf("E-performance-%05d", wantSequence) {
			t.Fatalf("Replay() event %d ID = %q", index, event.EventID)
		}
	}
}

func assertProjectionPage(t *testing.T, page ClientProjection, requestedAfter, firstSequence, lastSequence int, hasMore bool, snapshotSequence int) {
	t.Helper()
	wantCount := lastSequence - firstSequence + 1
	if len(page.Events) != wantCount {
		t.Fatalf("projection after %d returned %d events, want %d", requestedAfter, len(page.Events), wantCount)
	}
	if page.RequestedAfterSequence != uint64(requestedAfter) || page.Events[0].Sequence != uint64(firstSequence) || page.NextAfterSequence != uint64(lastSequence) {
		t.Fatalf("projection cursor after %d = requested:%d first:%d next:%d", requestedAfter, page.RequestedAfterSequence, page.Events[0].Sequence, page.NextAfterSequence)
	}
	if page.HasMore != hasMore || page.SnapshotSequence != uint64(snapshotSequence) || page.Thread.LastSequence != page.SnapshotSequence || page.Mailbox.LastSequence != page.SnapshotSequence {
		t.Fatalf("projection metadata after %d = has_more:%v snapshot:%d thread:%d mailbox:%d", requestedAfter, page.HasMore, page.SnapshotSequence, page.Thread.LastSequence, page.Mailbox.LastSequence)
	}
	for index, event := range page.Events {
		if event.Sequence != uint64(firstSequence+index) {
			t.Fatalf("projection after %d event %d sequence = %d, want %d", requestedAfter, index, event.Sequence, firstSequence+index)
		}
	}
}

func assertProjectionEventQueryUsesBoundedIndex(t *testing.T, store *Store, threadID string) {
	t.Helper()
	rows, err := store.db.Query(`EXPLAIN QUERY PLAN SELECT event_json FROM events WHERE thread_id=? AND sequence>? ORDER BY sequence ASC LIMIT ?`, threadID, 0, projectionPageSize+1)
	if err != nil {
		t.Fatalf("EXPLAIN projection event query = %v", err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan projection query plan = %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read projection query plan = %v", err)
	}
	plan := strings.Join(details, "; ")
	if !strings.Contains(plan, "SEARCH events USING INDEX") || strings.Contains(plan, "SCAN events") || strings.Contains(plan, "USE TEMP B-TREE") {
		t.Fatalf("projection event query is not a bounded index seek: %s", plan)
	}
}

func medianReplayWall(t *testing.T, store *Store, threadID string) time.Duration {
	t.Helper()
	durations := make([]time.Duration, 5)
	for index := range durations {
		started := time.Now()
		events, err := store.Replay(context.Background(), threadID, 0, 0)
		durations[index] = time.Since(started)
		if err != nil {
			t.Fatalf("Replay() growth measurement error = %v", err)
		}
		performanceEventSink = events
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return durations[len(durations)/2]
}
