package agoscheduler_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agoscheduler"
	"claudexflow/internal/agoverify"
)

const chineseObjective = "分析当前仓库，为 README 增加一个快速开始章节，运行相关测试，并生成完成报告。"

// clock is an injected, manually advanced clock. No test in this package
// sleeps: time only moves when a test says so.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
}
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type recordingExecutor struct {
	calls atomic.Int64
	err   error
}

func (e *recordingExecutor) Execute(_ context.Context, dispatch agoboardruntime.Dispatch) (agoboardruntime.ExecutionResult, error) {
	e.calls.Add(1)
	if e.err != nil {
		return agoboardruntime.ExecutionResult{}, e.err
	}
	return agoboardruntime.ExecutionResult{
		Artifact: "artifact://scheduled/" + dispatch.AttemptID,
		Summary:  "调度器派发的任务已完成",
	}, nil
}

// countingJudge records how often verification actually ran, which is what
// lets a test prove the executor and the verifier are two separate calls.
type countingJudge struct{ calls atomic.Int64 }

func (j *countingJudge) Judge(_ context.Context, input agoverify.JudgeInput) (agoverify.JudgeVerdict, error) {
	j.calls.Add(1)
	outcomes := make([]agoverify.CriterionOutcome, 0, len(input.AcceptanceCriteria))
	for _, criterion := range input.AcceptanceCriteria {
		outcomes = append(outcomes, agoverify.CriterionOutcome{Criterion: criterion, Passed: true, Reason: "证据支持"})
	}
	return agoverify.JudgeVerdict{Decision: agoverify.DecisionAccept, Summary: "证据满足验收标准", Criteria: outcomes}, nil
}

type harness struct {
	store     *agoboardstore.Store
	runtime   *agoboardruntime.Runtime
	scheduler *agoscheduler.Scheduler
	executor  *recordingExecutor
	verifier  *countingJudge
	clock     *clock
	boardID   string
	// gateCommands is what the next createGoal establishes as the project
	// gate. Empty means the goal has no goal-level proof, which is the
	// default everywhere else.
	gateCommands []string
	baseRevision string
}

func newHarness(t *testing.T, dbPath string) *harness {
	t.Helper()
	store, err := agoboardstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	testClock := newClock()
	executor := &recordingExecutor{}
	verifier := &countingJudge{}
	runtime := agoboardruntime.New(store, agoplanner.DemoPlanner{}, agoboardruntime.Options{
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: testClock.Now,
	})
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime, Executor: executor, Verification: mustSchedulerVerification(t, verifier),
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: testClock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{store: store, runtime: runtime, scheduler: scheduler, executor: executor, verifier: verifier, clock: testClock}
}

// withGate rebuilds the harness scheduler with a project gate attached, so a
// test can drive the goal-level proof without a toolchain.
func (h *harness) withGate(t *testing.T, gate agoscheduler.ProjectGate, _ []string) {
	t.Helper()
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: h.store, Runtime: h.runtime, Executor: h.executor,
		Verification:  mustSchedulerVerification(t, h.verifier),
		Gate:          gate,
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: h.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.scheduler = scheduler
}

func (h *harness) createGoalWithGate(t *testing.T, boardID string, commands []string) {
	t.Helper()
	// A gate proves an INTEGRATED revision, so the goal needs an integration
	// chain for there to be anything to prove.
	h.gateCommands, h.baseRevision = commands, "base-revision"
	h.createGoal(t, boardID)
	h.gateCommands, h.baseRevision = nil, ""
}

func (h *harness) createGoal(t *testing.T, boardID string) {
	t.Helper()
	_, err := h.runtime.Create(context.Background(), agoboardruntime.Goal{
		BoardID:    boardID,
		Repository: agoplanner.Repository{ID: t.TempDir(), Revision: "HEAD"},
		Objective:  agoplanner.Objective{ID: "objective", Summary: chineseObjective},
		ProjectGates: []agoplanner.ProjectGate{{
			ID: "gate", Title: "目标验收",
			AcceptanceCriteria: []string{"所有任务通过独立验收"}, VerifierIDs: []string{"ago-verifier"},
		}},
		Constraints: agoplanner.Constraints{
			PathScopes:     []string{"README.md", "docs"},
			CapabilityTags: []string{"repo-read", "repo-write", "tests", "report"},
			VerifierIDs:    []string{"ago-verifier"},
		},
		ExecutionMode: "fake",
		GateCommands:  h.gateCommands,
		BaseRevision:  h.baseRevision,
	})
	if err != nil {
		t.Fatalf("create goal: %v", err)
	}
	h.boardID = boardID
}

// runUntilSettled drives cycles until nothing changes, bounded so a stalled
// scheduler fails the test rather than looping forever.
func (h *harness) runUntilSettled(t *testing.T) agoboardstore.Completion {
	t.Helper()
	ctx := context.Background()
	previous := uint64(0)
	for range 64 {
		if _, err := h.scheduler.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		board, err := h.store.Board(ctx, h.boardID)
		if err != nil {
			t.Fatal(err)
		}
		if board.Version == previous {
			break
		}
		previous = board.Version
		// Let any backoff elapse so a retrying board can settle.
		h.clock.Advance(time.Minute)
	}
	completion, err := h.store.Completion(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	return completion
}

// The headline D3 outcome: a Chinese goal reaches a terminal state with no
// manual advance call anywhere.
func TestChineseGoalReachesDoneWithoutManualAdvance(t *testing.T) {
	h := newHarness(t, filepath.Join(t.TempDir(), "board.db"))
	defer h.store.Close()
	h.createGoal(t, "board-auto")

	completion := h.runUntilSettled(t)
	if completion.Status != agoboardstore.CompletionPassed {
		t.Fatalf("completion = %#v, want every task passed", completion)
	}
	board, err := h.store.Board(context.Background(), h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	if len(board.Tasks) != 5 {
		t.Fatalf("demo graph has %d tasks, want 5", len(board.Tasks))
	}
	for _, task := range board.Tasks {
		if task.State != agoboardprotocol.TaskPassed {
			t.Fatalf("task %q state = %q, want passed", task.ID, task.State)
		}
	}
	// The objective is untouched by scheduling.
	goal, _, err := h.runtime.Definition(context.Background(), h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	if goal.Objective.Summary != chineseObjective {
		t.Fatalf("objective = %q, want it unchanged", goal.Objective.Summary)
	}
	if h.executor.calls.Load() != 5 || h.verifier.calls.Load() != 5 {
		t.Fatalf("executor calls = %d, verifier calls = %d; want 5 and 5", h.executor.calls.Load(), h.verifier.calls.Load())
	}
}

// Every dispatched attempt must be fenced, and the verifier must never be the
// worker.
func TestEveryDispatchedAttemptIsFencedAndIndependentlyVerified(t *testing.T) {
	h := newHarness(t, filepath.Join(t.TempDir(), "fenced.db"))
	defer h.store.Close()
	h.createGoal(t, "board-fenced")
	h.runUntilSettled(t)

	board, err := h.store.Board(context.Background(), h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	tokens := make(map[string]string, len(board.Attempts))
	for _, attempt := range board.Attempts {
		if attempt.FencingToken == "" || attempt.Generation == 0 {
			t.Fatalf("attempt %q was dispatched without fencing identity: %#v", attempt.ID, attempt)
		}
		if previous, seen := tokens[attempt.FencingToken]; seen {
			t.Fatalf("attempts %q and %q share a fencing token", previous, attempt.ID)
		}
		tokens[attempt.FencingToken] = attempt.ID
	}
	for _, evidence := range board.Evidence {
		if evidence.WorkerID != "ago-worker" {
			t.Fatalf("evidence %q was not submitted by the worker: %#v", evidence.ID, evidence)
		}
	}
	if uint64(len(board.Attempts)) >= board.NextGeneration {
		t.Fatalf("generation counter %d did not stay ahead of %d attempts", board.NextGeneration, len(board.Attempts))
	}
}

// A paused board admits no new claims, keeps that state across a restart, and
// resumes cleanly.
func TestPauseSurvivesRestartAndResumeContinuesScheduling(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "paused.db")
	h := newHarness(t, path)
	h.createGoal(t, "board-paused")

	// One cycle so some work is genuinely in flight before pausing.
	if _, err := h.scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ApplyBoard(ctx, h.boardID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "pause-1", ExpectedVersion: board.Version,
		Actor: agoboardprotocol.Actor{ID: "ago-operator", Role: agoboardprotocol.RoleCoordinator},
		Type:  agoboardprotocol.CommandBoardPause, Reason: "用户暂停",
	}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	pausedVersion, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	callsWhilePaused := h.executor.calls.Load()
	for range 3 {
		if _, err := h.scheduler.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if h.executor.calls.Load() != callsWhilePaused {
		t.Fatalf("a paused board dispatched %d new attempts", h.executor.calls.Load()-callsWhilePaused)
	}
	after, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	// Pause stops work being STARTED, not work being finished: an attempt
	// already in flight runs through its own verification and integration
	// rather than being abandoned halfway. So the invariant is that no new
	// attempt appears, not that the graph freezes.
	if len(after.Attempts) != len(pausedVersion.Attempts) {
		t.Fatalf("a paused board created %d new attempts", len(after.Attempts)-len(pausedVersion.Attempts))
	}
	for _, task := range after.Tasks {
		if task.State == agoboardprotocol.TaskLeased && !contains(pausedVersion.Tasks, task.ID, agoboardprotocol.TaskLeased) {
			t.Fatalf("a paused board claimed task %q", task.ID)
		}
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}

	// Restart against the same database: the pause must still hold.
	restarted := newHarness(t, path)
	defer restarted.store.Close()
	restarted.boardID = h.boardID
	reopened, err := restarted.store.Board(ctx, restarted.boardID)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Paused || reopened.PauseReason != "用户暂停" {
		t.Fatalf("pause did not survive restart: paused=%v reason=%q", reopened.Paused, reopened.PauseReason)
	}
	if _, err := restarted.scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if restarted.executor.calls.Load() != 0 {
		t.Fatal("a restarted scheduler dispatched work on a paused board")
	}

	if _, err := restarted.store.ApplyBoard(ctx, restarted.boardID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "resume-1", ExpectedVersion: reopened.Version,
		Actor: agoboardprotocol.Actor{ID: "ago-operator", Role: agoboardprotocol.RoleCoordinator},
		Type:  agoboardprotocol.CommandBoardResume,
	}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	completion := restarted.runUntilSettled(t)
	if completion.Status != agoboardstore.CompletionPassed {
		t.Fatalf("completion after resume = %#v, want passed", completion)
	}
}

// Repeating a pause is an error rather than a silent success.
func TestRepeatedPauseIsRejected(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, filepath.Join(t.TempDir(), "double-pause.db"))
	defer h.store.Close()
	h.createGoal(t, "board-double")
	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	pause := func(id string, version uint64) error {
		_, err := h.store.ApplyBoard(ctx, h.boardID, agoboardprotocol.Command{
			SchemaVersion: agoboardprotocol.SchemaVersion, ID: id, ExpectedVersion: version,
			Actor: agoboardprotocol.Actor{ID: "ago-operator", Role: agoboardprotocol.RoleCoordinator},
			Type:  agoboardprotocol.CommandBoardPause, Reason: "用户暂停",
		})
		return err
	}
	if err := pause("pause-a", board.Version); err != nil {
		t.Fatal(err)
	}
	paused, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	if err := pause("pause-b", paused.Version); err == nil {
		t.Fatal("pausing an already-paused board silently succeeded")
	}
}

// Nothing is dispatched unless a claim was committed to this caller.
func TestNoDispatchWithoutACommittedClaim(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, filepath.Join(t.TempDir(), "no-dispatch.db"))
	defer h.store.Close()
	h.createGoal(t, "board-nodispatch")

	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ApplyBoard(ctx, h.boardID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "pause", ExpectedVersion: board.Version,
		Actor: agoboardprotocol.Actor{ID: "ago-operator", Role: agoboardprotocol.RoleCoordinator},
		Type:  agoboardprotocol.CommandBoardPause, Reason: "用户暂停",
	}); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := h.scheduler.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if h.executor.calls.Load() != 0 || h.verifier.calls.Load() != 0 {
		t.Fatalf("executor ran %d times and verifier %d times without a committed claim", h.executor.calls.Load(), h.verifier.calls.Load())
	}
	attempts, err := h.store.CountAttempts(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Fatalf("attempts created without dispatch = %d, want 0", attempts)
	}
}

// Run must exit cleanly on cancellation and must not leave an ownerless attempt
// behind: every attempt it created has a lease.
func TestRunExitsCleanlyOnContextCancel(t *testing.T) {
	h := newHarness(t, filepath.Join(t.TempDir(), "cancel.db"))
	defer h.store.Close()
	h.createGoal(t, "board-cancel")

	ticks := make(chan time.Time)
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: h.store, Runtime: h.runtime, Executor: h.executor, Verification: mustSchedulerVerification(t, h.verifier),
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: h.clock.Now, Ticker: ticks,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx) }()

	ticks <- h.clock.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want a clean exit", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after its context was cancelled")
	}

	board, err := h.store.Board(context.Background(), h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range board.Attempts {
		found := false
		for _, lease := range board.Leases {
			if lease.AttemptID == attempt.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("cancellation left attempt %q without a lease", attempt.ID)
		}
	}
}

// Two schedulers sharing a database must not execute the same task twice.
func TestTwoSchedulersDoNotDuplicateWork(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "two.db")
	first := newHarness(t, path)
	defer first.store.Close()
	first.createGoal(t, "board-shared")

	second := newHarness(t, path)
	defer second.store.Close()
	second.boardID = first.boardID

	var wait sync.WaitGroup
	for _, h := range []*harness{first, second} {
		h := h
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 12 {
				if _, err := h.scheduler.RunOnce(ctx); err != nil {
					return
				}
			}
		}()
	}
	wait.Wait()

	board, err := first.store.Board(ctx, first.boardID)
	if err != nil {
		t.Fatal(err)
	}
	// One accepted attempt per task, and never two attempts for the same task
	// in flight at once.
	perTask := make(map[string]int, len(board.Tasks))
	for _, attempt := range board.Attempts {
		if attempt.State == agoboardprotocol.AttemptPassed {
			perTask[attempt.TaskID]++
		}
	}
	for taskID, passed := range perTask {
		if passed != 1 {
			t.Fatalf("task %q has %d accepted attempts, want exactly 1", taskID, passed)
		}
	}
	active, err := first.store.CountActiveLeases(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if active > 1 {
		t.Fatalf("active leases = %d, want at most 1 for this single-writer repository", active)
	}
}

// An executor error is classified and retried within the bound rather than
// stopping the task on its first failure.
func TestTransientExecutorFailureIsRetriedWithinTheBound(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, filepath.Join(t.TempDir(), "retry.db"))
	defer h.store.Close()
	h.executor.err = errors.New("执行器临时失败")
	h.createGoal(t, "board-retry")

	if _, err := h.scheduler.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	var retrying int
	for _, task := range board.Tasks {
		if task.State == agoboardprotocol.TaskRetryWait {
			retrying++
			if task.AttemptCount != 1 || task.NextEligibleAt.IsZero() {
				t.Fatalf("retry accounting for %q = %#v", task.ID, task)
			}
		}
	}
	if retrying == 0 {
		t.Fatal("a transient executor failure did not schedule a retry")
	}
	// Drive to exhaustion; the bound must hold.
	for range 40 {
		h.clock.Advance(time.Minute)
		if _, err := h.scheduler.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	final, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	perTask := make(map[string]int, len(final.Tasks))
	for _, attempt := range final.Attempts {
		perTask[attempt.TaskID]++
	}
	for taskID, attempts := range perTask {
		if attempts > agoboardprotocol.MaxAttempts {
			t.Fatalf("task %q accumulated %d attempts, above the bound of %d", taskID, attempts, agoboardprotocol.MaxAttempts)
		}
	}
}

func mustSchedulerVerification(t *testing.T, judge agoverify.Judge) *agoverify.Verifier {
	t.Helper()
	verification, err := agoverify.New(agoverify.Options{Judge: judge})
	if err != nil {
		t.Fatal(err)
	}
	return verification
}

func contains(tasks []agoboardprotocol.Task, id string, state agoboardprotocol.TaskState) bool {
	for _, task := range tasks {
		if task.ID == id && task.State == state {
			return true
		}
	}
	return false
}
