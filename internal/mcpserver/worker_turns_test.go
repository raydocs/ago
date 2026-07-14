package mcpserver

import "testing"

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
