package agoboardruntime

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agoplanner"
)

func TestChineseGoalRunsThroughBoardClaimEvidenceAndIndependentReview(t *testing.T) {
	runtime, store, executor, verifier := newFixtureRuntime(t)
	goal, plan := fixtureGoalAndPlan()
	runtime.planner = agoplanner.FixturePlanner{Proposal: plan}

	created, err := runtime.Create(context.Background(), goal)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	assertOnlyTaskInColumn(t, created, ColumnReady)

	completed, err := runtime.Tick(context.Background(), goal.BoardID)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	assertOnlyTaskInColumn(t, completed, ColumnDone)
	if executor.dispatch.Goal.Objective.Summary != goal.Objective.Summary || verifier.dispatch.Goal.Objective.Summary != goal.Objective.Summary {
		t.Fatalf("Chinese objective changed: executor=%q verifier=%q", executor.dispatch.Goal.Objective.Summary, verifier.dispatch.Goal.Objective.Summary)
	}
	if !reflect.DeepEqual(executor.dispatch.Task, plan.Tasks[0]) {
		t.Fatalf("executor task = %#v, want full proposal %#v", executor.dispatch.Task, plan.Tasks[0])
	}
	completion, err := runtime.Completion(context.Background(), goal.BoardID)
	if err != nil || completion.Status != agoboardstore.CompletionPassed {
		t.Fatalf("Completion = %#v, %v", completion, err)
	}
	events, err := store.Replay(context.Background(), goal.BoardID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []agoboardprotocol.EventType{
		agoboardprotocol.EventBoardCreated,
		agoboardprotocol.EventTaskAdded,
		agoboardprotocol.EventTaskStateChanged,
		agoboardprotocol.EventLeaseAcquired,
		agoboardprotocol.EventAttemptStateChanged,
		agoboardprotocol.EventEvidenceSubmitted,
		agoboardprotocol.EventEvidenceAccepted,
	}
	gotTypes := make([]agoboardprotocol.EventType, len(events))
	for index := range events {
		gotTypes[index] = events[index].Type
	}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Fatalf("event order = %#v, want %#v", gotTypes, wantTypes)
	}
}

func TestVerifierRejectionProjectsBlockedAndFailedCompletion(t *testing.T) {
	runtime, _, _, verifier := newFixtureRuntime(t)
	goal, plan := fixtureGoalAndPlan()
	runtime.planner = agoplanner.FixturePlanner{Proposal: plan}
	verifier.review = Review{Accepted: false, Reason: "验收标准未满足"}
	if _, err := runtime.Create(context.Background(), goal); err != nil {
		t.Fatal(err)
	}
	view, err := runtime.Tick(context.Background(), goal.BoardID)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	assertOnlyTaskInColumn(t, view, ColumnBlocked)
	completion, err := runtime.Completion(context.Background(), goal.BoardID)
	if err != nil || completion.Status != agoboardstore.CompletionFailed {
		t.Fatalf("Completion = %#v, %v", completion, err)
	}
}

func TestExecutorFailureProjectsBlockedWithoutCallingVerifier(t *testing.T) {
	runtime, _, executor, verifier := newFixtureRuntime(t)
	goal, plan := fixtureGoalAndPlan()
	runtime.planner = agoplanner.FixturePlanner{Proposal: plan}
	executor.err = errors.New("fake executor failed")
	if _, err := runtime.Create(context.Background(), goal); err != nil {
		t.Fatal(err)
	}
	view, err := runtime.Tick(context.Background(), goal.BoardID)
	if err != nil {
		t.Fatalf("Tick persists the failure instead of returning it: %v", err)
	}
	assertOnlyTaskInColumn(t, view, ColumnBlocked)
	if verifier.calls != 0 {
		t.Fatalf("verifier calls = %d, want 0", verifier.calls)
	}
}

func TestRuntimeRestartRecoversDurableGoalAndPlannerMetadata(t *testing.T) {
	runtime, store, executor, verifier := newFixtureRuntime(t)
	goal, plan := fixtureGoalAndPlan()
	runtime.planner = agoplanner.FixturePlanner{Proposal: plan}
	if _, err := runtime.Create(context.Background(), goal); err != nil {
		t.Fatal(err)
	}

	restarted := New(store, agoplanner.FixturePlanner{Proposal: plan}, executor, verifier, runtime.options)
	view, err := restarted.Tick(context.Background(), goal.BoardID)
	if err != nil {
		t.Fatalf("Tick after runtime restart: %v", err)
	}
	assertOnlyTaskInColumn(t, view, ColumnDone)
	if executor.dispatch.Goal.Objective.Summary != goal.Objective.Summary || !reflect.DeepEqual(executor.dispatch.Task, plan.Tasks[0]) {
		t.Fatalf("recovered dispatch = %#v", executor.dispatch)
	}
}

type fakeExecutor struct {
	dispatch Dispatch
	result   ExecutionResult
	err      error
}

func (executor *fakeExecutor) Execute(_ context.Context, dispatch Dispatch) (ExecutionResult, error) {
	executor.dispatch = dispatch
	return executor.result, executor.err
}

type fakeVerifier struct {
	dispatch Dispatch
	evidence ExecutionResult
	review   Review
	calls    int
}

func (verifier *fakeVerifier) Verify(_ context.Context, dispatch Dispatch, evidence ExecutionResult) (Review, error) {
	verifier.calls++
	verifier.dispatch, verifier.evidence = dispatch, evidence
	return verifier.review, nil
}

func newFixtureRuntime(t *testing.T) (*Runtime, *agoboardstore.Store, *fakeExecutor, *fakeVerifier) {
	t.Helper()
	store, err := agoboardstore.Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	executor := &fakeExecutor{result: ExecutionResult{Artifact: "artifact://fake/result", Summary: "测试通过"}}
	verifier := &fakeVerifier{review: Review{Accepted: true, Reason: "独立验收通过"}}
	runtime := New(store, nil, executor, verifier, Options{
		CoordinatorID: "scheduler", WorkerID: "fake-local-worker", VerifierID: "independent-verifier",
		LeaseDuration: time.Minute, Now: func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) },
	})
	return runtime, store, executor, verifier
}

func fixtureGoalAndPlan() (Goal, agoplanner.Plan) {
	gate := agoplanner.ProjectGate{ID: "gate", Title: "项目验收", AcceptanceCriteria: []string{"测试通过"}, VerifierIDs: []string{"go-test"}}
	goal := Goal{
		BoardID:      "board-cn",
		Repository:   agoplanner.Repository{ID: "raydocs/ago", Revision: "fixture-revision"},
		Objective:    agoplanner.Objective{ID: "goal-cn", Summary: "实现中文目标到可验证任务完成的最小闭环"},
		ProjectGates: []agoplanner.ProjectGate{gate},
		Constraints: agoplanner.Constraints{
			PathScopes: []string{"internal/agoboardruntime"}, CapabilityTags: []string{"go", "tests"}, VerifierIDs: []string{"go-test"},
		},
	}
	plan := agoplanner.Plan{
		SchemaVersion: agoplanner.SchemaVersion, Repository: goal.Repository, Objective: goal.Objective, ProjectGates: goal.ProjectGates,
		Tasks: []agoplanner.TaskProposal{{
			ID: "slice", Title: "完成纵向切片", Description: "实现并验证最小编排闭环。",
			PathScopes: []string{"internal/agoboardruntime"}, AcceptanceCriteria: []string{"集成测试通过"}, VerifierIDs: []string{"go-test"}, CapabilityTags: []string{"go", "tests"},
		}},
		Dependencies: []agoplanner.DependencyProposal{},
	}
	return goal, plan
}

func assertOnlyTaskInColumn(t *testing.T, view BoardView, column Column) {
	t.Helper()
	for _, item := range view.Columns {
		if item.Name == column {
			if len(item.Tasks) != 1 || item.Tasks[0].ID != "slice" {
				t.Fatalf("column %s tasks = %#v", column, item.Tasks)
			}
			continue
		}
		if len(item.Tasks) != 0 {
			t.Fatalf("unexpected tasks in column %s: %#v", item.Name, item.Tasks)
		}
	}
}
