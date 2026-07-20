package agoboardprotocol

import "testing"

func patchCommand(board Board, id string, steps ...PatchStep) Command {
	return coordinatorCommand(board, CommandPlanPatch, "patch-"+id, func(command *Command) {
		command.Patch = &PatchSpec{ID: id, Reason: "计划需要调整", Steps: steps}
	})
}

func newTaskSpec(id string) *TaskSpec {
	return &TaskSpec{
		ID: id, Title: id, AccessMode: AccessRead,
		TerminalContract: TerminalContract{Outcome: "complete " + id, AcceptanceCriteria: []string{"tests pass"}},
	}
}

// A task added by a patch joins the running graph immediately, with readiness
// computed from its dependencies rather than left stranded in planned.
func TestPatchAddsATaskThatJoinsTheRunningGraph(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "first")
	board = activateTask(t, board, "first")

	board = mustApply(t, board, patchCommand(board, "p1",
		PatchStep{Operation: PatchAddTask, Task: newTaskSpec("repair")},
	))
	assertTaskState(t, board, "repair", TaskReady)

	_, task, _ := findTask(board, "repair")
	if task.Origin != "p1" {
		t.Fatalf("added task origin = %q, want the patch id so its provenance is visible", task.Origin)
	}
}

// A task added with an unmet dependency waits rather than becoming claimable.
func TestPatchAddedTaskWithAnUnmetDependencyIsBlocked(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "first")
	board = activateTask(t, board, "first")

	board = mustApply(t, board, patchCommand(board, "p1",
		PatchStep{Operation: PatchAddTask, Task: newTaskSpec("later")},
		PatchStep{Operation: PatchAddDependency, TaskID: "later", DependsOn: "first"},
	))
	assertTaskState(t, board, "later", TaskBlocked)

	// Completing the prerequisite releases it, which proves the patch produced
	// a real dependency and not a decoration.
	board = completeTask(t, board, "first", "worker-1")
	assertTaskState(t, board, "later", TaskReady)
}

// A patch is atomic: an illegal step anywhere rejects the whole patch.
func TestAnIllegalStepRejectsTheEntirePatch(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "first")
	board = activateTask(t, board, "first")
	before := cloneBoard(board)

	_, _, err := Apply(board, patchCommand(board, "p1",
		PatchStep{Operation: PatchAddTask, Task: newTaskSpec("good")},
		PatchStep{Operation: PatchAddDependency, TaskID: "good", DependsOn: "does-not-exist"},
	))
	if err == nil {
		t.Fatal("a patch with a missing prerequisite was applied")
	}
	if len(board.Tasks) != len(before.Tasks) {
		t.Fatalf("a rejected patch added %d tasks", len(board.Tasks)-len(before.Tasks))
	}
	if _, _, found := findTask(board, "good"); found {
		t.Fatal("the first step of a rejected patch was left applied")
	}
}

// A patch that would create a cycle is rejected by whole-board validation.
func TestPatchCannotIntroduceACycle(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "a")
	board = addTask(t, board, "b")
	board = addDependency(t, board, "dep", "b", "a")
	board = activateTask(t, board, "a")
	board = activateTask(t, board, "b")

	if _, _, err := Apply(board, patchCommand(board, "p1",
		PatchStep{Operation: PatchAddDependency, TaskID: "a", DependsOn: "b"},
	)); err == nil {
		t.Fatal("a patch introduced a dependency cycle")
	}
}

// Accepted work is immutable: its meaning cannot be changed after the fact.
func TestPatchCannotRewriteAcceptedWork(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "done")
	board = activateTask(t, board, "done")
	board = completeTask(t, board, "done", "worker-1")
	assertTaskState(t, board, "done", TaskPassed)

	for name, step := range map[string]PatchStep{
		"change acceptance": {Operation: PatchUpdateAcceptance, TaskID: "done",
			Acceptance: &TerminalContract{Outcome: "changed", AcceptanceCriteria: []string{"different"}}},
		"supersede":      {Operation: PatchSupersedeTask, TaskID: "done", SupersededBy: "done"},
		"cancel":         {Operation: PatchCancelTask, TaskID: "done"},
		"add dependency": {Operation: PatchAddDependency, TaskID: "done", DependsOn: "done"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Apply(board, patchCommand(board, "p-"+name, step)); err == nil {
				t.Fatalf("%s was allowed on accepted work", name)
			}
		})
	}
}

// Running work is never silently removed. Superseding it must be refused; only
// an explicit cancellation may end it.
func TestRunningWorkCannotBeSupersededSilently(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "running")
	board = addTask(t, board, "replacement")
	board = activateTask(t, board, "running")
	board = activateTask(t, board, "replacement")
	board = mustApply(t, board, leaseCommand(board, "running", "lease", "attempt", "worker-1"))
	start := command(board, RoleWorker, "worker-1", CommandAttemptStart, "start")
	start.TaskID, start.AttemptID = "running", "attempt"
	board = mustApply(t, board, fenced(board, start))
	assertTaskState(t, board, "running", TaskRunning)

	if _, _, err := Apply(board, patchCommand(board, "p1",
		PatchStep{Operation: PatchSupersedeTask, TaskID: "running", SupersededBy: "replacement"},
	)); err == nil {
		t.Fatal("running work was superseded without being cancelled")
	}

	// Cancelling is allowed, and it must also end the lease so the attempt is
	// not left running against work nobody is waiting for.
	board = mustApply(t, board, patchCommand(board, "p2",
		PatchStep{Operation: PatchCancelTask, TaskID: "running", Reason: "范围变更"},
	))
	assertTaskState(t, board, "running", TaskFailed)
	_, task, _ := findTask(board, "running")
	if !task.Cancelled || task.ActiveAttemptID != "" {
		t.Fatalf("cancelled task = %#v", task)
	}
	for _, lease := range board.Leases {
		if lease.TaskID == "running" && lease.State == LeaseActive {
			t.Fatal("cancelling left an active lease behind")
		}
	}
}

// A superseded task keeps everything that happened to it.
func TestSupersedingPreservesHistory(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "old")
	board = addTask(t, board, "new")
	board = activateTask(t, board, "old")
	board = activateTask(t, board, "new")

	// Give the old task a real failed attempt so there is history to preserve.
	board = mustApply(t, board, leaseCommand(board, "old", "lease", "attempt", "worker-1"))
	fail := command(board, RoleWorker, "worker-1", CommandAttemptFail, "fail")
	fail.TaskID, fail.AttemptID = "old", "attempt"
	fail.FailureClass = FailurePolicy
	fail.Reason = "策略拒绝"
	board = mustApply(t, board, fenced(board, fail))

	attemptsBefore := len(board.Attempts)
	board = mustApply(t, board, patchCommand(board, "p1",
		PatchStep{Operation: PatchSupersedeTask, TaskID: "old", SupersededBy: "new"},
	))

	_, task, _ := findTask(board, "old")
	if task.SupersededBy != "new" {
		t.Fatalf("superseded task = %#v, want it to name its replacement", task)
	}
	if len(board.Attempts) != attemptsBefore {
		t.Fatalf("attempts changed from %d to %d; history must be preserved", attemptsBefore, len(board.Attempts))
	}
	found := false
	for _, attempt := range board.Attempts {
		if attempt.TaskID == "old" && attempt.FailureReason == "策略拒绝" {
			found = true
		}
	}
	if !found {
		t.Fatal("the superseded task lost the record of why its attempt failed")
	}
}

// Removing a dependency can release work that was waiting on it.
func TestPatchCanRemoveADependencyAndReleaseWaitingWork(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "a")
	board = addTask(t, board, "b")
	board = addDependency(t, board, "dep", "b", "a")
	board = activateTask(t, board, "a")
	board = activateTask(t, board, "b")
	assertTaskState(t, board, "b", TaskBlocked)

	board = mustApply(t, board, patchCommand(board, "p1",
		PatchStep{Operation: PatchRemoveDependency, TaskID: "b", DependsOn: "a"},
	))
	assertTaskState(t, board, "b", TaskReady)
}

// A patch must say who changed the plan and why, and must stay bounded.
func TestPatchRequiresAReasonAndIsBounded(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "first")
	board = activateTask(t, board, "first")

	noReason := coordinatorCommand(board, CommandPlanPatch, "no-reason", func(command *Command) {
		command.Patch = &PatchSpec{ID: "p1", Steps: []PatchStep{{Operation: PatchAddTask, Task: newTaskSpec("x")}}}
	})
	if _, _, err := Apply(board, noReason); err == nil {
		t.Fatal("a plan patch was accepted without a recorded reason")
	}

	steps := make([]PatchStep, maxPatchSteps+1)
	for index := range steps {
		steps[index] = PatchStep{Operation: PatchAddTask, Task: newTaskSpec("task-" + string(rune('a'+index%26)) + string(rune('0'+index/26)))}
	}
	if _, _, err := Apply(board, patchCommand(board, "big", steps...)); err == nil {
		t.Fatalf("a patch with more than %d steps was accepted", maxPatchSteps)
	}

	if _, _, err := Apply(board, patchCommand(board, "unknown",
		PatchStep{Operation: PatchOperation("delete_everything"), TaskID: "first"},
	)); err == nil {
		t.Fatal("an unknown patch operation was accepted")
	}
}

// Only the coordinator may change the plan: a worker cannot rewrite the graph
// it is executing.
func TestOnlyTheCoordinatorMayPatchThePlan(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "first")
	board = activateTask(t, board, "first")

	workerPatch := command(board, RoleWorker, "worker-1", CommandPlanPatch, "worker-patch")
	workerPatch.Patch = &PatchSpec{ID: "p1", Reason: "自作主张", Steps: []PatchStep{{Operation: PatchAddTask, Task: newTaskSpec("x")}}}
	if _, _, err := Apply(board, workerPatch); err == nil {
		t.Fatal("a worker rewrote the plan it was executing")
	}
}
