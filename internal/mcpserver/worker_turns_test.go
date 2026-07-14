package mcpserver

import (
	"testing"

	"claudexflow/internal/claude"
)

func TestWorkerInvokeMaxTurnsRemaining(t *testing.T) {
	if got := workerInvokeMaxTurns(0); got != 10 {
		t.Fatalf("got %d", got)
	}
	if got := workerInvokeMaxTurns(20); got != 4 {
		t.Fatalf("cumulative 20 must request MaxTurns=4, got %d", got)
	}
	if got := workerInvokeMaxTurns(24); got != 0 {
		t.Fatalf("exhausted must be 0, got %d", got)
	}
	if got := workerInvokeMaxTurns(23); got != 1 {
		t.Fatalf("got %d", got)
	}
}

func TestTurnAccountingQualityDoesNotUpgrade(t *testing.T) {
	// First invoke missing num_turns → upper_bound 10; second exact=2 → cumulative 12 quality stays upper_bound.
	w := &workerState{}
	s := &Server{}
	s.applyResult(w, claude.Result{}, 10)
	if w.cumulativeModelTurns != 10 || w.turnAccountingQuality != "upper_bound" {
		t.Fatalf("after upper_bound: turns=%d quality=%q", w.cumulativeModelTurns, w.turnAccountingQuality)
	}
	s.applyResult(w, claude.Result{NumTurns: 2, TurnAccountingQuality: "exact"}, 10)
	if w.cumulativeModelTurns != 12 {
		t.Fatalf("cumulative=%d want 12", w.cumulativeModelTurns)
	}
	if w.turnAccountingQuality != "upper_bound" {
		t.Fatalf("quality upgraded to %q; must stay upper_bound", w.turnAccountingQuality)
	}
}

func TestWorseTurnQualityOrder(t *testing.T) {
	if got := worseTurnQuality("exact", "upper_bound"); got != "upper_bound" {
		t.Fatalf("got %s", got)
	}
	if got := worseTurnQuality("upper_bound", "exact"); got != "upper_bound" {
		t.Fatalf("got %s", got)
	}
	if got := worseTurnQuality("upper_bound", "unknown"); got != "unknown" {
		t.Fatalf("got %s", got)
	}
}
