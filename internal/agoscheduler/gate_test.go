package agoscheduler_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agogate"
	"claudexflow/internal/agoscheduler"
)

// scriptedGate answers however a test needs, and counts how often it was asked.
type scriptedGate struct {
	passes    bool
	calls     int
	revisions []string
}

func (gate *scriptedGate) Run(_ context.Context, repository, revision string, commands []string) (agogate.Result, error) {
	gate.calls++
	gate.revisions = append(gate.revisions, revision)
	if gate.passes {
		return agogate.Result{Revision: revision, Passed: true, Summary: "通过"}, nil
	}
	return agogate.Result{
		Revision: revision, Passed: false, Summary: "未通过：go test ./...",
		Checks: []agogate.Check{{
			Command: "go test ./...", ExitCode: 1,
			Output: "--- FAIL: TestSomething",
		}},
	}, nil
}

// Every task passing is a weaker claim than the integrated result holding
// together. Until the gate proves the integrated revision, the goal is not
// complete — this is the false-green the whole gate exists to prevent.
func TestAGoalIsNotCompleteUntilTheGateProvesTheIntegratedResult(t *testing.T) {
	ctx := context.Background()

	t.Run("a failing gate withholds completion", func(t *testing.T) {
		h := newGateHarness(t, &scriptedGate{passes: false})
		completion := h.runUntilSettled(t)
		if completion.Status == agoboardstore.CompletionPassed {
			t.Fatal("the goal completed while the integrated result failed its own checks")
		}
		board, err := h.store.Board(ctx, h.boardID)
		if err != nil {
			t.Fatal(err)
		}
		if board.Gate.State != agoboardprotocol.GateFailed {
			t.Fatalf("gate state = %q, want failed", board.Gate.State)
		}
		// A repair needs something to act on.
		if !strings.Contains(board.Gate.FailureOutput, "TestSomething") {
			t.Fatalf("the failure output kept nothing actionable: %q", board.Gate.FailureOutput)
		}
	})

	t.Run("a passing gate completes it", func(t *testing.T) {
		gate := &scriptedGate{passes: true}
		h := newGateHarness(t, gate)
		completion := h.runUntilSettled(t)
		if completion.Status != agoboardstore.CompletionPassed {
			t.Fatalf("completion = %q, want passed", completion.Status)
		}
		board, err := h.store.Board(ctx, h.boardID)
		if err != nil {
			t.Fatal(err)
		}
		if !board.Gate.SatisfiedAt(board.IntegratedRevision) {
			t.Fatalf("the gate did not prove the integrated revision: %+v", board.Gate)
		}
		if gate.calls == 0 {
			t.Fatal("the gate was never run")
		}
	})
}

// A gate answer is about one revision. Asking again for the same one would
// spend a toolchain run to learn nothing.
func TestTheGateIsNotRerunForARevisionItAlreadyAnswered(t *testing.T) {
	gate := &scriptedGate{passes: true}
	h := newGateHarness(t, gate)
	h.runUntilSettled(t)
	before := gate.calls

	for range 5 {
		if _, err := h.scheduler.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if gate.calls != before {
		t.Fatalf("the gate ran %d more times for a revision it had already answered", gate.calls-before)
	}
}

// A pass recorded against an earlier revision says nothing about a later one.
func TestAPassDoesNotCarryToADifferentRevision(t *testing.T) {
	board := agoboardprotocol.Board{
		IntegratedRevision: "newer",
		Gate: agoboardprotocol.ProjectGate{
			State: agoboardprotocol.GatePassed, Commands: []string{"go test ./..."},
			Revision: "older",
		},
	}
	if board.Gate.SatisfiedAt(board.IntegratedRevision) {
		t.Fatal("a pass recorded for an earlier revision satisfied a later one")
	}
	if !board.Gate.SatisfiedAt("older") {
		t.Fatal("the pass does not even satisfy the revision it was recorded for")
	}
}

// A goal whose repository offered no checks still completes — but on the
// weaker claim, and the board says so rather than pretending it was proven.
func TestAGoalWithNoGateCompletesButIsNotMarkedProven(t *testing.T) {
	h := newHarness(t, t.TempDir()+"/board.db")
	defer h.store.Close()
	h.createGoal(t, "board-no-gate")
	completion := h.runUntilSettled(t)
	if completion.Status != agoboardstore.CompletionPassed {
		t.Fatalf("completion = %q, want passed", completion.Status)
	}
	board, err := h.store.Board(context.Background(), h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	if board.Gate.Established() {
		t.Fatal("a goal with no checks reported an established gate")
	}
	if board.Gate.SatisfiedAt(board.IntegratedRevision) {
		t.Fatal("an absent gate reported itself satisfied")
	}
}

// newGateHarness is the ordinary harness with a project gate and a goal that
// establishes checks for it to run.
func newGateHarness(t *testing.T, gate agoscheduler.ProjectGate) *harness {
	t.Helper()
	h := newHarness(t, t.TempDir()+"/board.db")
	t.Cleanup(func() { h.store.Close() })
	if gate != nil {
		h.withGate(t, gate, nil)
	}
	h.createGoalWithGate(t, "board-gate", []string{"go test ./..."})
	return h
}

// brokenGate can never run. A permanently broken toolchain, an unreadable
// repository, a worktree that cannot be created.
type brokenGate struct{ calls int }

func (gate *brokenGate) Run(_ context.Context, _, _ string, _ []string) (agogate.Result, error) {
	gate.calls++
	return agogate.Result{}, errors.New("git worktree add: permission denied")
}

// A gate that cannot RUN must not be silent. Swallowing the error left a goal
// not done, with nothing runnable and nothing in the attention queue, forever
// — the one outcome this system must never produce.
func TestAGateThatCannotRunIsBoundedAndNeverSilent(t *testing.T) {
	ctx := context.Background()
	gate := &brokenGate{}
	h := newGateHarness(t, nil)
	h.withGate(t, gate, nil)
	h.runUntilSettled(t)

	for range agoboardprotocol.MaxGateUnavailable + 5 {
		if _, err := h.scheduler.RunOnceForBoard(ctx, h.boardID); err != nil {
			t.Fatalf("a broken gate failed the cycle: %v", err)
		}
	}
	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	// It is counted durably, so a restart does not begin the tally again.
	if board.Gate.Unavailable == 0 {
		t.Fatal("the failures to run were not recorded")
	}
	// And it stops trying rather than burning a toolchain run every cycle.
	if board.Gate.Unavailable > agoboardprotocol.MaxGateUnavailable {
		t.Fatalf("it kept trying past its bound: %d", board.Gate.Unavailable)
	}
	if !strings.Contains(board.Gate.LastError, "permission denied") {
		t.Fatalf("the reason was lost: %q", board.Gate.LastError)
	}
	// The repair budget is untouched: the work was never judged.
	if board.Gate.Failures != 0 {
		t.Fatalf("an outage spent the repair budget: %d", board.Gate.Failures)
	}
	// And it is not a pass.
	completion := agoboardprotocol.EvaluateCompletion(board)
	if completion.Done || completion.Proven {
		t.Fatalf("a goal whose gate never ran reported done=%v proven=%v", completion.Done, completion.Proven)
	}
}
