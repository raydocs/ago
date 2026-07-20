package agosupervisor_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"claudexflow/internal/agoartifact"
	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agofake"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agoscheduler"
	"claudexflow/internal/agosupervisor"
	"claudexflow/internal/agoverify"
)

const chineseObjective = "分析当前仓库，为 README 增加一个快速开始章节，运行相关测试，并生成完成报告。"

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Time advances on every read so bounded backoff elapses without sleeping.
	c.now = c.now.Add(time.Second)
	return c.now
}

type harness struct {
	store      *agoboardstore.Store
	scheduler  *agoscheduler.Scheduler
	supervisor *agosupervisor.Supervisor
	boardID    string
	path       string
}

func newHarness(t *testing.T, script agofake.Script, authorize agosupervisor.Authorization) *harness {
	t.Helper()
	base := t.TempDir()
	path := filepath.Join(base, "board.db")
	return openHarness(t, path, base, script, authorize, "board-zero-relay")
}

func openHarness(t *testing.T, path, base string, script agofake.Script, authorize agosupervisor.Authorization, boardID string) *harness {
	t.Helper()
	store, err := agoboardstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	artifacts, err := agoartifact.Open(agoartifact.Options{Root: filepath.Join(base, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := agofake.New(script)
	if err != nil {
		t.Fatal(err)
	}
	// Two separate objects: the executor cannot certify its own work.
	judge, err := agofake.NewVerifier(script)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := agoverify.New(agoverify.Options{Judge: judge, Artifacts: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	testClock := &clock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
	runtime := agoboardruntime.New(store, agoplanner.DemoPlanner{}, agoboardruntime.Options{
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: testClock.Now,
	})
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime,
		Executor: provider.WithArtifacts(artifacts), Verification: verification,
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: testClock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := agosupervisor.New(agosupervisor.Options{
		Store: store, Scheduler: scheduler, BoardID: boardID,
		CoordinatorID: "ago-supervisor", Authorize: authorize, Now: testClock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{store: store, scheduler: scheduler, supervisor: supervisor, boardID: boardID, path: path}
}

func (h *harness) createGoal(t *testing.T, runtimeStore *agoboardstore.Store) {
	t.Helper()
	runtime := agoboardruntime.New(runtimeStore, agoplanner.DemoPlanner{}, agoboardruntime.Options{
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute,
		Now:           func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) },
	})
	if _, err := runtime.Create(context.Background(), agoboardruntime.Goal{
		BoardID:    h.boardID,
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
	}); err != nil {
		t.Fatal(err)
	}
}

// The product claim, stated as a test: after the goal is created, no human
// message is sent. The supervisor drives planning, dispatch, verification,
// repair, retry, and continuation entirely from durable state.
//
// The scenario deliberately includes work that fails transiently, work a
// verifier rejects, and a scheduler restart, because those are exactly the
// moments a person would previously have had to intervene.
func TestZeroRelayAutonomousGoalReachesCompletion(t *testing.T) {
	ctx := context.Background()
	script := agofake.Script{
		Default: agofake.OutcomeSuccess,
		ByTask: map[string]agofake.Outcome{
			// A transient fault the scheduler must retry on its own.
			"identify-commands": agofake.OutcomeTemporaryFailureThenSuccess,
			// A verifier rejection the supervisor must turn into a repair.
			"update-readme": agofake.OutcomeVerifierRetryWithFeedback,
		},
	}
	authorize := agosupervisor.Authorization{LocalFileWrites: true, LocalCommits: true}

	h := newHarness(t, script, authorize)
	h.createGoal(t, h.store)

	// Everything from here happens with no further human input.
	status, err := h.supervisor.Run(ctx, 200)
	if err != nil {
		t.Fatalf("supervisor could not drive the goal to a terminal state: %v", err)
	}
	if !status.Complete {
		t.Fatalf("goal did not complete: %+v decisions=%#v", status, status.Decisions)
	}
	if len(status.Decisions) != 0 {
		t.Fatalf("the goal needed %d human decisions, want zero: %#v", len(status.Decisions), status.Decisions)
	}

	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range board.Tasks {
		if task.State != agoboardprotocol.TaskPassed {
			t.Fatalf("task %q = %q, want passed", task.ID, task.State)
		}
		if task.AcceptedEvidenceID == "" {
			t.Fatalf("task %q passed without accepted evidence", task.ID)
		}
	}

	// The transient failure was retried automatically and its history kept.
	transient := attemptsFor(board, "identify-commands")
	if len(transient) != 2 {
		t.Fatalf("transient task ran %d attempts, want 2", len(transient))
	}
	if transient[0].State != agoboardprotocol.AttemptFailed || transient[0].FailureClass != agoboardprotocol.FailureTransient {
		t.Fatalf("first attempt = %#v, want a recorded transient failure", transient[0])
	}

	// The verifier rejection produced a repaired attempt, and the rejection is
	// still inspectable rather than erased by the eventual success.
	rejected := 0
	for _, evidence := range board.Evidence {
		if evidence.TaskID == "update-readme" && evidence.State == agoboardprotocol.EvidenceRejected {
			rejected++
		}
	}
	if rejected == 0 {
		t.Fatal("the verifier rejection left no durable record")
	}
}

// A stop only a person can resolve must stop the loop and surface one
// actionable item — not spin, and not silently give up.
func TestWorkNeedingAPersonSurfacesExactlyOneDecision(t *testing.T) {
	ctx := context.Background()
	for outcome, wantKind := range map[agofake.Outcome]agosupervisor.DecisionKind{
		agofake.OutcomeBlockedNeedsInput: agosupervisor.DecisionAmbiguous,
		agofake.OutcomeBlockedPolicy:     agosupervisor.DecisionDestructive,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			h := newHarness(t, agofake.Script{
				Default: agofake.OutcomeSuccess,
				ByTask:  map[string]agofake.Outcome{"identify-commands": outcome},
			}, agosupervisor.Authorization{LocalFileWrites: true})
			h.createGoal(t, h.store)

			status, err := h.supervisor.Run(ctx, 200)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if status.Complete {
				t.Fatal("a goal blocked on a human decision reported completion")
			}
			if len(status.Decisions) != 1 {
				t.Fatalf("decisions = %#v, want exactly one", status.Decisions)
			}
			decision := status.Decisions[0]
			if decision.Kind != wantKind {
				t.Fatalf("decision kind = %q, want %q", decision.Kind, wantKind)
			}
			if decision.TaskID != "identify-commands" || decision.Reason == "" || decision.Suggestion == "" {
				t.Fatalf("decision is not self-contained: %#v", decision)
			}
		})
	}
}

// A permanent fault is repaired a bounded number of times and then handed over,
// so automatic repair cannot loop forever.
func TestAutomaticRepairIsBoundedThenEscalates(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, agofake.Script{
		Default: agofake.OutcomeSuccess,
		ByTask:  map[string]agofake.Outcome{"identify-commands": agofake.OutcomePermanentFailure},
	}, agosupervisor.Authorization{LocalFileWrites: true, MaxRepairsPerTask: 2})
	h.createGoal(t, h.store)

	status, err := h.supervisor.Run(ctx, 300)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if status.Complete {
		t.Fatal("a permanently failing goal reported completion")
	}
	if len(status.Decisions) != 1 || status.Decisions[0].Kind != agosupervisor.DecisionExhausted {
		t.Fatalf("decisions = %#v, want one exhausted decision", status.Decisions)
	}

	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	// Two repairs were spent, each granting a fresh bounded attempt budget.
	attempts := attemptsFor(board, "identify-commands")
	if len(attempts) < 2 {
		t.Fatalf("repair produced %d attempts, want the repaired retries to be real", len(attempts))
	}
	// Every repair is on the record with its reason.
	patched := 0
	for _, event := range replay(t, h.store, h.boardID) {
		if event.Type == agoboardprotocol.EventPlanPatched {
			patched++
			if event.Reason == "" {
				t.Fatal("a plan patch was recorded without a reason")
			}
		}
	}
	if patched != 2 {
		t.Fatalf("plan patches = %d, want exactly the 2 authorized repairs", patched)
	}
}

// The supervisor must survive a restart mid-goal: its authority comes from the
// durable graph, not from anything it holds in memory.
func TestSupervisorResumesAfterRestartWithoutDuplicatingWork(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	path := filepath.Join(base, "board.db")
	script := agofake.Script{
		Default: agofake.OutcomeSuccess,
		ByTask:  map[string]agofake.Outcome{"identify-commands": agofake.OutcomeTemporaryFailureThenSuccess},
	}
	authorize := agosupervisor.Authorization{LocalFileWrites: true}

	first := openHarness(t, path, base, script, authorize, "board-restart")
	first.createGoal(t, first.store)
	// Advance partway, then abandon this supervisor entirely.
	for range 3 {
		if _, err := first.supervisor.Step(ctx); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	midway, err := first.store.Board(ctx, first.boardID)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.store.Close(); err != nil {
		t.Fatal(err)
	}

	// A brand new supervisor and scheduler, same database.
	second := openHarness(t, path, base, script, authorize, "board-restart")
	status, err := second.supervisor.Run(ctx, 200)
	if err != nil {
		t.Fatalf("Run after restart: %v", err)
	}
	if !status.Complete || len(status.Decisions) != 0 {
		t.Fatalf("goal did not complete cleanly after restart: %+v", status)
	}

	board, err := second.store.Board(ctx, second.boardID)
	if err != nil {
		t.Fatal(err)
	}
	if board.Version < midway.Version {
		t.Fatalf("the graph went backwards across restart: %d then %d", midway.Version, board.Version)
	}
	// No task was executed twice: exactly one accepted attempt each.
	for _, task := range board.Tasks {
		accepted := 0
		for _, attempt := range attemptsFor(board, task.ID) {
			if attempt.State == agoboardprotocol.AttemptPassed {
				accepted++
			}
		}
		if accepted != 1 {
			t.Fatalf("task %q has %d accepted attempts after restart, want 1", task.ID, accepted)
		}
	}
}

// The supervisor plans; it must never claim. If it ever acquired a lease
// directly it would bypass the slot limits and fencing the scheduler enforces.
func TestSupervisorNeverActsAsAWorkerOrClaimsWork(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, agofake.Script{Default: agofake.OutcomeSuccess}, agosupervisor.Authorization{LocalFileWrites: true})
	h.createGoal(t, h.store)
	if _, err := h.supervisor.Run(ctx, 200); err != nil {
		t.Fatal(err)
	}

	for _, event := range replay(t, h.store, h.boardID) {
		if event.Actor.ID != "ago-supervisor" {
			continue
		}
		if event.Actor.Role != agoboardprotocol.RoleCoordinator {
			t.Fatalf("the supervisor acted as %q on event %q", event.Actor.Role, event.Type)
		}
		switch event.Type {
		case agoboardprotocol.EventLeaseAcquired, agoboardprotocol.EventEvidenceSubmitted,
			agoboardprotocol.EventEvidenceAccepted, agoboardprotocol.EventEvidenceRejected:
			t.Fatalf("the supervisor emitted %q, which belongs to the scheduler or the verifier", event.Type)
		}
	}

	// And the work that did happen was claimed by the scheduler.
	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range board.Attempts {
		if attempt.WorkerID != "ago-worker" {
			t.Fatalf("attempt %q ran as %q, want the scheduler's worker", attempt.ID, attempt.WorkerID)
		}
	}
}

func attemptsFor(board agoboardprotocol.Board, taskID string) []agoboardprotocol.Attempt {
	var attempts []agoboardprotocol.Attempt
	for _, attempt := range board.Attempts {
		if attempt.TaskID == taskID {
			attempts = append(attempts, attempt)
		}
	}
	return attempts
}

func replay(t *testing.T, store *agoboardstore.Store, boardID string) []agoboardprotocol.Event {
	t.Helper()
	events, err := store.Replay(context.Background(), boardID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// A restarted supervisor must reach the same decisions as the one it replaced.
//
// The repair budget used to live in memory, so a restart re-issued repair
// commands whose durable receipts already existed; the resulting conflict was
// swallowed, the budget was burned on commands that did nothing, and the
// attention queue stayed empty while the goal quietly stalled.
func TestRepairBudgetSurvivesRestartAndStillEscalates(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	path := filepath.Join(base, "board.db")
	script := agofake.Script{
		Default: agofake.OutcomeSuccess,
		ByTask:  map[string]agofake.Outcome{"identify-commands": agofake.OutcomePermanentFailure},
	}
	authorize := agosupervisor.Authorization{LocalFileWrites: true, MaxRepairsPerTask: 2}

	first := openHarness(t, path, base, script, authorize, "board-restart-repair")
	first.createGoal(t, first.store)
	// Run far enough to spend at least one repair.
	for range 8 {
		if _, err := first.supervisor.Step(ctx); err != nil {
			t.Fatalf("Step: %v", err)
		}
	}
	beforeBoard, err := first.store.Board(ctx, first.boardID)
	if err != nil {
		t.Fatal(err)
	}
	_, beforeTask, _ := taskIn(beforeBoard, "identify-commands")
	if beforeTask.UserRetries == 0 {
		t.Fatal("no repair was spent, so the restart case would not be exercised")
	}
	if err := first.store.Close(); err != nil {
		t.Fatal(err)
	}

	// A brand new supervisor with an empty memory, same durable state.
	second := openHarness(t, path, base, script, authorize, "board-restart-repair")
	status, err := second.supervisor.Run(ctx, 200)
	if err != nil {
		t.Fatalf("Run after restart: %v", err)
	}
	if status.Complete {
		t.Fatal("a permanently failing goal reported completion")
	}
	// The goal must end up waiting on a person, with a real decision, rather
	// than stalling with an empty queue.
	if len(status.Decisions) == 0 {
		t.Fatal("the restarted supervisor stalled without raising any decision")
	}
	if !status.Blocked {
		t.Fatalf("status = %+v, want it blocked on a user decision", status)
	}

	afterBoard, err := second.store.Board(ctx, second.boardID)
	if err != nil {
		t.Fatal(err)
	}
	_, afterTask, _ := taskIn(afterBoard, "identify-commands")
	// The budget is durable, so a restart cannot grant a fresh allowance.
	if afterTask.UserRetries > authorize.MaxRepairsPerTask {
		t.Fatalf("restart granted extra repairs: %d used against a limit of %d",
			afterTask.UserRetries, authorize.MaxRepairsPerTask)
	}
}

func taskIn(board agoboardprotocol.Board, id string) (int, agoboardprotocol.Task, bool) {
	for index, task := range board.Tasks {
		if task.ID == id {
			return index, task, true
		}
	}
	return -1, agoboardprotocol.Task{}, false
}
