package agosupervisor_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agofake"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agosupervisor"
)

// A failing gate must produce work or a decision. Never silence.
//
// The previous version could produce neither: it aimed the repair at the task
// that produced the rejected revision, which is accepted, and both
// update_acceptance and task.retry refuse accepted work. So the first real
// gate failure did nothing at all — no repair, no decision, no budget spent —
// and the runner swallowed the error. That is the exact state this sprint
// exists to make impossible.
func TestAFailingGateAlwaysProducesWorkOrADecision(t *testing.T) {
	ctx := context.Background()
	h := newSupervisorHarness(t)
	h.completeEveryTask(t)
	h.failGate(t, "集成结果未通过项目门禁：go test ./...", "--- FAIL: TestSomething")

	status, err := h.supervisor.Step(ctx)
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	if status.Complete {
		t.Fatal("a goal completed with a failing gate")
	}

	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	runnable := 0
	for _, task := range board.Tasks {
		switch task.State {
		case agoboardprotocol.TaskPassed, agoboardprotocol.TaskFailed:
		default:
			if !task.Cancelled && task.SupersededBy == "" {
				runnable++
			}
		}
	}
	// The invariant, stated as the test's whole purpose.
	if runnable == 0 && len(status.Decisions) == 0 {
		t.Fatal("not done, nothing runnable, and nothing in the attention queue")
	}
	if runnable == 0 {
		return // escalated, which is a legitimate outcome
	}

	// If it made work, that work must be REAL: on the board and in the plan,
	// with scopes it can actually write. A task in one and not the other is
	// claimable and undispatchable.
	var repair agoboardprotocol.Task
	for _, task := range board.Tasks {
		if strings.HasPrefix(task.ID, "gate-repair") {
			repair = task
		}
	}
	if repair.ID == "" {
		t.Fatal("the gate failure produced work that is not a repair task")
	}
	if repair.AccessMode != agoboardprotocol.AccessWrite {
		t.Errorf("the repair task cannot write: %q", repair.AccessMode)
	}
	var definition struct {
		Plan agoplanner.Plan `json:"plan"`
	}
	if err := h.store.Definition(ctx, h.boardID, &definition); err != nil {
		t.Fatal(err)
	}
	var proposal *agoplanner.TaskProposal
	for index, task := range definition.Plan.Tasks {
		if task.ID == repair.ID {
			proposal = &definition.Plan.Tasks[index]
		}
	}
	if proposal == nil {
		t.Fatal("the repair task is on the board but not in the plan: it would fail at dispatch with 'no planner proposal'")
	}
	if len(proposal.PathScopes) == 0 {
		t.Error("the repair task has no path scopes, so it could edit nothing")
	}
	if !strings.Contains(proposal.Description, "TestSomething") {
		t.Errorf("the repair was not told what failed: %q", proposal.Description)
	}
}

// Repair is bounded, and the bound is spent on real attempts rather than on
// calls that could never have worked.
func TestGateRepairIsBoundedAndThenEscalates(t *testing.T) {
	ctx := context.Background()
	h := newSupervisorHarness(t)

	for round := 1; round <= agoboardprotocol.MaxGateRepairs+1; round++ {
		h.completeEveryTask(t)
		h.failGate(t, "未通过", "FAIL")
		if _, err := h.supervisor.Step(ctx); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	status, err := h.supervisor.Step(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Complete {
		t.Fatal("a goal with a permanently failing gate completed")
	}
	if len(status.Decisions) == 0 {
		t.Fatal("the repair budget was spent and nothing was escalated")
	}
	found := false
	for _, decision := range status.Decisions {
		if decision.TaskID == "project-gate" {
			found = true
			if !strings.Contains(decision.Reason, "未通过") {
				t.Errorf("the decision does not carry the failure: %q", decision.Reason)
			}
		}
	}
	if !found {
		t.Fatalf("no project-gate decision among %d", len(status.Decisions))
	}
}

// newSupervisorHarness builds a goal that has a project gate and an
// integration chain, so there is something for the gate to prove.
func newSupervisorHarness(t *testing.T) *gateHarness {
	t.Helper()
	base := t.TempDir()
	h := openHarness(t, filepath.Join(base, "board.db"), base,
		agofake.Script{Default: agofake.OutcomeSuccess},
		agosupervisor.Authorization{LocalFileWrites: true, LocalCommits: true},
		"board-gate")
	runtime := agoboardruntime.New(h.store, agoplanner.DemoPlanner{}, agoboardruntime.Options{
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
		BaseRevision:  "base-revision",
		GateCommands:  []string{"go test ./..."},
	}); err != nil {
		t.Fatal(err)
	}
	return &gateHarness{harness: h}
}

type gateHarness struct{ *harness }

// completeEveryTask drives the scheduler until nothing is outstanding, so the
// gate has a settled board to answer about.
func (h *gateHarness) completeEveryTask(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for range 64 {
		if _, err := h.scheduler.RunOnceForBoard(ctx, h.boardID); err != nil {
			t.Fatalf("RunOnceForBoard: %v", err)
		}
		board, err := h.store.Board(ctx, h.boardID)
		if err != nil {
			t.Fatal(err)
		}
		outstanding := 0
		for _, task := range board.Tasks {
			if task.State != agoboardprotocol.TaskPassed && !task.Cancelled && task.SupersededBy == "" {
				outstanding++
			}
		}
		if outstanding == 0 {
			return
		}
	}
	t.Fatal("tasks never settled")
}

// failGate records a gate failure the way the scheduler would.
func (h *gateHarness) failGate(t *testing.T, summary, output string) {
	t.Helper()
	ctx := context.Background()
	board, err := h.store.Board(ctx, h.boardID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ApplyBoard(ctx, h.boardID, agoboardprotocol.Command{
		SchemaVersion:   agoboardprotocol.SchemaVersion,
		ID:              fmt.Sprintf("gate-fail:%s:%d", board.IntegratedRevision, board.Gate.Failures),
		ExpectedVersion: board.Version,
		Actor:           agoboardprotocol.Actor{ID: "ago-supervisor", Role: agoboardprotocol.RoleCoordinator},
		Type:            agoboardprotocol.CommandGateFail,
		Revision:        board.IntegratedRevision,
		Gate: &agoboardprotocol.GateSpec{
			Summary: summary, FailureOutput: output,
			RanAt: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC),
		},
	}); err != nil {
		t.Fatalf("record gate failure: %v", err)
	}
}
