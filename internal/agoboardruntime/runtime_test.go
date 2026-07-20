package agoboardruntime

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agoplanner"
)

// The runtime owns goal admission and durable definition recovery. Claiming,
// dispatch, retry, and verification moved to internal/agoscheduler, which is
// where those behaviours are now tested; keeping a second scheduling entry
// point here is what this package deliberately no longer has.

func TestChineseGoalIsAdmittedAsADurableGraph(t *testing.T) {
	runtime, store := newFixtureRuntime(t)
	goal, plan := fixtureGoalAndPlan()
	runtime.planner = agoplanner.FixturePlanner{Proposal: plan}

	view, err := runtime.Create(context.Background(), goal)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	assertOnlyTaskInColumn(t, view, ColumnReady)

	board, err := store.Board(context.Background(), goal.BoardID)
	if err != nil {
		t.Fatal(err)
	}
	if board.Title != goal.Objective.Summary {
		t.Fatalf("board title = %q, want the Chinese objective unchanged", board.Title)
	}
	if board.Repository != goal.Repository.ID {
		t.Fatalf("board repository = %q, want %q", board.Repository, goal.Repository.ID)
	}
	if board.NextGeneration != 1 {
		t.Fatalf("a new board must start its fencing counter at 1, got %d", board.NextGeneration)
	}
	if board.Paused {
		t.Fatal("a new board must not start paused")
	}
	for _, task := range board.Tasks {
		if task.AccessMode == "" {
			t.Fatalf("task %q was admitted without an access mode", task.ID)
		}
		if task.AttemptCount != 0 {
			t.Fatalf("task %q was admitted with %d attempts", task.ID, task.AttemptCount)
		}
	}
}

func TestGraphAdmissionIsIdempotentForTheSameGoal(t *testing.T) {
	runtime, store := newFixtureRuntime(t)
	goal, plan := fixtureGoalAndPlan()
	runtime.planner = agoplanner.FixturePlanner{Proposal: plan}
	ctx := context.Background()

	if _, err := runtime.Create(ctx, goal); err != nil {
		t.Fatal(err)
	}
	first, err := store.Board(ctx, goal.BoardID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Create(ctx, goal); err != nil {
		t.Fatalf("re-admitting the same goal: %v", err)
	}
	second, err := store.Board(ctx, goal.BoardID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("re-admitting an identical goal changed the durable graph")
	}
	boards, err := store.BoardIDs(ctx)
	if err != nil || len(boards) != 1 {
		t.Fatalf("boards = %#v, %v; want exactly one", boards, err)
	}
}

// A restarted process must recover the goal and plan from SQLite, since the
// scheduler needs them to build a dispatch.
func TestRestartRecoversDurableGoalAndPlannerDefinition(t *testing.T) {
	runtime, store := newFixtureRuntime(t)
	goal, plan := fixtureGoalAndPlan()
	runtime.planner = agoplanner.FixturePlanner{Proposal: plan}
	if _, err := runtime.Create(context.Background(), goal); err != nil {
		t.Fatal(err)
	}

	restarted := New(store, agoplanner.FixturePlanner{Proposal: plan}, runtime.options)
	recoveredGoal, recoveredPlan, err := restarted.Definition(context.Background(), goal.BoardID)
	if err != nil {
		t.Fatalf("Definition after restart: %v", err)
	}
	if recoveredGoal.Objective.Summary != goal.Objective.Summary {
		t.Fatalf("recovered objective = %q, want %q", recoveredGoal.Objective.Summary, goal.Objective.Summary)
	}
	if recoveredGoal.ExecutionMode != goal.ExecutionMode {
		t.Fatalf("recovered execution mode = %q, want %q", recoveredGoal.ExecutionMode, goal.ExecutionMode)
	}
	if !reflect.DeepEqual(recoveredPlan.Tasks, plan.Tasks) {
		t.Fatalf("recovered plan tasks = %#v, want %#v", recoveredPlan.Tasks, plan.Tasks)
	}
}

// The returned definition must be a copy: a caller cannot reach into the
// runtime's cache and change what the next dispatch sees.
func TestDefinitionReturnsDefensiveCopies(t *testing.T) {
	runtime, _ := newFixtureRuntime(t)
	goal, plan := fixtureGoalAndPlan()
	runtime.planner = agoplanner.FixturePlanner{Proposal: plan}
	if _, err := runtime.Create(context.Background(), goal); err != nil {
		t.Fatal(err)
	}
	first, firstPlan, err := runtime.Definition(context.Background(), goal.BoardID)
	if err != nil {
		t.Fatal(err)
	}
	first.Objective.Summary = "被篡改的目标"
	firstPlan.Tasks[0].Title = "被篡改的任务"

	second, secondPlan, err := runtime.Definition(context.Background(), goal.BoardID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Objective.Summary != goal.Objective.Summary {
		t.Fatalf("mutating a returned goal changed the runtime's copy: %q", second.Objective.Summary)
	}
	if secondPlan.Tasks[0].Title != plan.Tasks[0].Title {
		t.Fatalf("mutating a returned plan changed the runtime's copy: %q", secondPlan.Tasks[0].Title)
	}
}

func TestCreateRejectsAnEmptyObjective(t *testing.T) {
	runtime, _ := newFixtureRuntime(t)
	goal, plan := fixtureGoalAndPlan()
	runtime.planner = agoplanner.FixturePlanner{Proposal: plan}
	goal.Objective.Summary = "   "
	if _, err := runtime.Create(context.Background(), goal); err == nil {
		t.Fatal("a goal with a blank objective was admitted")
	}
}

func newFixtureRuntime(t *testing.T) (*Runtime, *agoboardstore.Store) {
	t.Helper()
	store, err := agoboardstore.Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runtime := New(store, nil, Options{
		CoordinatorID: "scheduler", WorkerID: "fake-local-worker", VerifierID: "independent-verifier",
		LeaseDuration: time.Minute, Now: func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) },
	})
	return runtime, store
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
		ExecutionMode: "fake",
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

// The runtime must not expose a second way to move task state.
func TestRuntimeExposesNoSchedulingEntryPoint(t *testing.T) {
	runtime, _ := newFixtureRuntime(t)
	value := reflect.TypeOf(runtime)
	for index := range value.NumMethod() {
		switch name := value.Method(index).Name; name {
		case "Tick", "TickOnce", "Claim", "Dispatch", "Advance":
			t.Fatalf("the runtime exposes %q, reintroducing a second scheduling authority", name)
		}
	}
	_ = agoboardprotocol.SchemaVersion
}
