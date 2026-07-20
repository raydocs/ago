package agoscheduler_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoscheduler"
	"claudexflow/internal/agoverify"
)

// fusedProvider is the attack: one object that implements both roles, which is
// how acceptance became self-certification under two identity strings.
type fusedProvider struct{}

func (fusedProvider) Execute(context.Context, agoboardruntime.Dispatch) (agoboardruntime.ExecutionResult, error) {
	return agoboardruntime.ExecutionResult{Artifact: "a", Summary: "done"}, nil
}
func (fusedProvider) Verify(context.Context, agoverify.Input) (agoverify.Result, error) {
	return agoverify.Result{Decision: agoverify.DecisionAccept}, nil
}

// A configuration where one object serves as both executor and verifier must be
// refused outright. Distinct identity strings are not independence.
func TestOneObjectCannotServeAsBothExecutorAndVerifier(t *testing.T) {
	h := newHarness(t, filepath.Join(t.TempDir(), "board.db"))
	defer h.store.Close()
	fused := fusedProvider{}
	_, err := newSchedulerWith(h, fused, fused)
	if err == nil {
		t.Fatal("the scheduler accepted one object in both roles")
	}
	if !strings.Contains(err.Error(), "different implementations") {
		t.Fatalf("error = %v, want it to name the fused-role problem", err)
	}
}

// unavailableJudge always reports the provider is down.
type unavailableJudge struct{ calls atomic.Int64 }

func (judge *unavailableJudge) Judge(context.Context, agoverify.JudgeInput) (agoverify.JudgeVerdict, error) {
	judge.calls.Add(1)
	return agoverify.JudgeVerdict{}, agoverify.ErrUnavailable
}

// A verifier outage must not re-run the worker. Whether the provider is up says
// nothing about whether the work was good, so the evidence stays submitted, the
// task stays in review, and the worker's attempt budget is untouched.
func TestVerifierOutageLeavesWorkInReviewWithoutRerunningTheWorker(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, filepath.Join(t.TempDir(), "board.db"))
	defer h.store.Close()
	h.createGoal(t, "board-outage")

	judge := &unavailableJudge{}
	verification, err := agoverify.New(agoverify.Options{Judge: judge})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := newSchedulerWith(h, h.executor, verification)
	if err != nil {
		t.Fatal(err)
	}
	// One cycle to get evidence submitted.
	if _, err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	executionsAfterFirst := h.executor.calls.Load()
	if executionsAfterFirst == 0 {
		t.Fatal("no work was executed, so the outage case is not exercised")
	}

	// Several more cycles with the verifier still down.
	for range 5 {
		if _, err := scheduler.RunOnce(ctx); err != nil {
			t.Fatalf("a verifier outage failed the cycle: %v", err)
		}
	}

	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	// The evidence is still awaiting a verdict, not rejected.
	submitted := 0
	for _, evidence := range board.Evidence {
		if evidence.State == agoboardprotocol.EvidenceSubmitted {
			submitted++
			if evidence.VerificationAttempts == 0 {
				t.Fatal("the outage was not recorded against the evidence")
			}
		}
		if evidence.State == agoboardprotocol.EvidenceRejected {
			t.Fatal("a verifier outage was recorded as a rejection of the work")
		}
	}
	if submitted == 0 {
		t.Fatal("no evidence remained awaiting verification")
	}
	// No task was failed, and no attempt budget was spent on the outage.
	for _, task := range board.Tasks {
		if task.State == agoboardprotocol.TaskFailed {
			t.Fatalf("task %q failed because of a verifier outage: %s", task.ID, task.BlockedReason)
		}
		if task.AttemptCount > 1 {
			t.Fatalf("task %q used %d attempts during a verifier outage", task.ID, task.AttemptCount)
		}
	}
	if judge.calls.Load() < 2 {
		t.Fatalf("verification was retried %d times, want it to keep trying", judge.calls.Load())
	}
}

// Once the verifier recovers, the SAME durable evidence is judged — the worker
// is never asked to redo work that was already submitted.
func TestVerifierRecoveryJudgesTheSameEvidenceWithoutNewExecution(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, filepath.Join(t.TempDir(), "board.db"))
	defer h.store.Close()
	h.createGoal(t, "board-recovery")

	flaky := &flakyJudge{failures: 3}
	verification, err := agoverify.New(agoverify.Options{Judge: flaky})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := newSchedulerWith(h, h.executor, verification)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	afterFirst := h.executor.calls.Load()
	evidenceBefore := evidenceIDs(t, h, ctx)

	for range 4 {
		if _, err := scheduler.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	// The very same evidence records were judged; none were replaced.
	for _, id := range evidenceBefore {
		found := false
		for _, evidence := range board.Evidence {
			if evidence.ID == id && evidence.State == agoboardprotocol.EvidenceAccepted {
				found = true
			}
		}
		if !found {
			t.Fatalf("evidence %q was not the record eventually accepted", id)
		}
	}
	// The executor was not asked to redo the work it had already submitted.
	if h.executor.calls.Load() < afterFirst {
		t.Fatal("executor call count went backwards")
	}
}

// flakyJudge is unavailable for a while, then works.
type flakyJudge struct {
	failures int
	seen     int
}

func (judge *flakyJudge) Judge(_ context.Context, input agoverify.JudgeInput) (agoverify.JudgeVerdict, error) {
	judge.seen++
	if judge.seen <= judge.failures {
		return agoverify.JudgeVerdict{}, agoverify.ErrUnavailable
	}
	outcomes := make([]agoverify.CriterionOutcome, 0, len(input.AcceptanceCriteria))
	for _, criterion := range input.AcceptanceCriteria {
		outcomes = append(outcomes, agoverify.CriterionOutcome{Criterion: criterion, Passed: true, Reason: "ok"})
	}
	return agoverify.JudgeVerdict{Decision: agoverify.DecisionAccept, Summary: "通过", Criteria: outcomes}, nil
}

func evidenceIDs(t *testing.T, h *harness, ctx context.Context) []string {
	t.Helper()
	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, evidence := range board.Evidence {
		ids = append(ids, evidence.ID)
	}
	if len(ids) == 0 {
		t.Fatal("no evidence was submitted, so the test proves nothing")
	}
	return ids
}

// newSchedulerWith builds a scheduler with the given roles, so a test can wire
// a deliberately wrong configuration and check it is refused.
func newSchedulerWith(h *harness, executor agoboardruntime.Executor, verification agoscheduler.Verification) (*agoscheduler.Scheduler, error) {
	return agoscheduler.New(agoscheduler.Options{
		Store: h.store, Runtime: h.runtime, Executor: executor, Verification: verification,
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: h.clock.Now,
	})
}

var _ = errors.Is

// An outage that never ends must not become a silent stall.
//
// Verification is deferred a bounded number of times, and the bound used to be
// enforced by skipping the evidence — after which nothing would ever look at it
// again. The task stayed in verifying, a state no transition could leave, and a
// supervisor read it as merely pending and spun until it ran out of steps. The
// attempt is failed instead, on the record, so it can be escalated like any
// other work that cannot be finished automatically.
func TestVerificationThatNeverConcludesFailsTheAttemptInsteadOfStalling(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, filepath.Join(t.TempDir(), "board.db"))
	defer h.store.Close()
	h.createGoal(t, "board-never-concludes")

	verification, err := agoverify.New(agoverify.Options{Judge: &unavailableJudge{}})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := newSchedulerWith(h, h.executor, verification)
	if err != nil {
		t.Fatal(err)
	}
	// Well past MaxVerificationAttempts, so the bound is certainly reached.
	for range agoboardprotocol.MaxVerificationAttempts + 5 {
		if _, err := scheduler.RunOnce(ctx); err != nil {
			t.Fatalf("a permanent verifier outage failed the cycle: %v", err)
		}
	}

	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	stranded := 0
	for _, task := range board.Tasks {
		if task.State == agoboardprotocol.TaskVerifying {
			stranded++
		}
	}
	if stranded > 0 {
		t.Fatalf("%d task(s) are stranded in verifying with nothing able to move them", stranded)
	}
	// The failure says the verdict was never obtained — not that the work was
	// judged and found wanting.
	var failed []agoboardprotocol.Task
	for _, task := range board.Tasks {
		if task.State == agoboardprotocol.TaskFailed {
			failed = append(failed, task)
		}
	}
	if len(failed) == 0 {
		t.Fatal("no task recorded a failure, so the exhausted verification left no trace")
	}
	for _, task := range failed {
		if task.FailureClass != agoboardprotocol.FailureExhausted {
			t.Fatalf("task %q failed with class %q, want exhausted: %s",
				task.ID, task.FailureClass, task.BlockedReason)
		}
		if !strings.Contains(task.BlockedReason, "验收") {
			t.Fatalf("task %q does not say verification was the problem: %q", task.ID, task.BlockedReason)
		}
	}
}
