package agoboardprotocol

import (
	"testing"
	"time"
)

// testClock is a fixed clock: no protocol test may depend on wall time.
var testClock = time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

// fenced fills in the fencing token the board issued for an attempt, which is
// what a legitimate worker or verifier receives at dispatch. Negative tests use
// it too, so they keep failing for the reason they are testing.
func fenced(board Board, command Command) Command {
	if _, attempt, found := findAttempt(board, command.AttemptID); found {
		command.FencingToken = attempt.FencingToken
	}
	return command
}

func TestBlockedTaskCannotLeaseAndBecomesReadyWhenDependencyPasses(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "first")
	board = addTask(t, board, "second")
	board = addDependency(t, board, "depends", "second", "first")
	board = activateTask(t, board, "first")
	board = activateTask(t, board, "second")
	assertTaskState(t, board, "second", TaskBlocked)

	_, _, err := Apply(board, leaseCommand(board, "second", "blocked-lease", "blocked-attempt", "worker-1"))
	if err == nil {
		t.Fatal("blocked task was leased")
	}

	board = completeTask(t, board, "first", "worker-1")
	assertTaskState(t, board, "first", TaskPassed)
	assertTaskState(t, board, "second", TaskReady)
	board = mustApply(t, board, leaseCommand(board, "second", "lease-2", "attempt-2", "worker-2"))
	assertTaskState(t, board, "second", TaskLeased)
}

func TestOnlyLeaseOwnerCanRunAndOnlyVerifierCanAccept(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "task")
	board = activateTask(t, board, "task")
	board = mustApply(t, board, leaseCommand(board, "task", "lease", "attempt", "worker-1"))

	wrongWorker := command(board, RoleWorker, "worker-2", CommandAttemptStart, "wrong-worker")
	wrongWorker.TaskID, wrongWorker.AttemptID = "task", "attempt"
	if _, _, err := Apply(board, fenced(board, wrongWorker)); err == nil {
		t.Fatal("worker without the lease started the attempt")
	}

	start := command(board, RoleWorker, "worker-1", CommandAttemptStart, "start")
	start.TaskID, start.AttemptID = "task", "attempt"
	board = mustApply(t, board, fenced(board, start))

	submit := command(board, RoleWorker, "worker-1", CommandEvidenceSubmit, "submit")
	submit.TaskID, submit.AttemptID = "task", "attempt"
	submit.Evidence = &EvidenceSpec{ID: "evidence", TaskID: "task", AttemptID: "attempt", Artifact: "commit:abc", Summary: "tests passed"}
	board = mustApply(t, board, fenced(board, submit))
	assertTaskState(t, board, "task", TaskVerifying)

	workerAccept := command(board, RoleWorker, "worker-1", CommandEvidenceAccept, "worker-accept")
	workerAccept.TaskID, workerAccept.AttemptID = "task", "attempt"
	workerAccept.Evidence = &EvidenceSpec{ID: "evidence"}
	if _, _, err := Apply(board, fenced(board, workerAccept)); err == nil {
		t.Fatal("worker accepted evidence and set passed")
	}
	assertTaskState(t, board, "task", TaskVerifying)

	accept := command(board, RoleVerifier, "verifier-1", CommandEvidenceAccept, "accept")
	accept.TaskID, accept.AttemptID = "task", "attempt"
	accept.Evidence = &EvidenceSpec{ID: "evidence"}
	board = mustApply(t, board, fenced(board, accept))
	assertTaskState(t, board, "task", TaskPassed)
	if board.Evidence[0].State != EvidenceAccepted {
		t.Fatalf("evidence state = %s, want accepted", board.Evidence[0].State)
	}
}

func TestEvidenceReviewerMustBeIndependentFromAttemptWorker(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "task")
	board = activateTask(t, board, "task")
	board = mustApply(t, board, leaseCommand(board, "task", "lease", "attempt", "worker-1"))
	start := command(board, RoleWorker, "worker-1", CommandAttemptStart, "start-independent")
	start.TaskID, start.AttemptID = "task", "attempt"
	board = mustApply(t, board, fenced(board, start))
	submit := command(board, RoleWorker, "worker-1", CommandEvidenceSubmit, "submit-independent")
	submit.TaskID, submit.AttemptID = "task", "attempt"
	submit.Evidence = &EvidenceSpec{ID: "evidence", TaskID: "task", AttemptID: "attempt", Artifact: "artifact", Summary: "summary"}
	board = mustApply(t, board, fenced(board, submit))

	selfReview := command(board, RoleVerifier, "worker-1", CommandEvidenceAccept, "self-review")
	selfReview.TaskID, selfReview.AttemptID = "task", "attempt"
	selfReview.Evidence = &EvidenceSpec{ID: "evidence"}
	if _, _, err := Apply(board, fenced(board, selfReview)); err == nil {
		t.Fatal("attempt worker accepted its own evidence by changing actor role")
	}
	assertTaskState(t, board, "task", TaskVerifying)
}

func TestVerifierAttemptFailRequiresActiveLease(t *testing.T) {
	board := createBoard(t)
	board = addTask(t, board, "task")
	board = activateTask(t, board, "task")
	board = mustApply(t, board, leaseCommand(board, "task", "lease", "attempt", "worker-1"))
	start := command(board, RoleWorker, "worker-1", CommandAttemptStart, "start")
	start.TaskID, start.AttemptID = "task", "attempt"
	board = mustApply(t, board, fenced(board, start))
	board.Leases[0].State = LeaseCompleted
	if err := board.Validate(); err != nil {
		t.Fatalf("fixture must remain valid: %v", err)
	}

	fail := command(board, RoleWorker, "worker-1", CommandAttemptFail, "fail")
	fail.TaskID, fail.AttemptID = "task", "attempt"
	fail.FailureClass = FailureTransient
	if _, _, err := Apply(board, fenced(board, fail)); err == nil {
		t.Fatal("worker failed attempt with an inactive lease")
	}
}

func TestIllegalTransitionsAndTerminalStatesAreRejected(t *testing.T) {
	board := createBoard(t)
	contract := TerminalContract{Outcome: "immutable outcome", AcceptanceCriteria: []string{"immutable criterion"}}
	spec := &TaskSpec{ID: "task", Title: "Task", AccessMode: AccessRead, TerminalContract: contract}
	add := coordinatorCommand(board, CommandTaskAdd, "add", func(command *Command) { command.Task = spec })
	board = mustApply(t, board, add)
	spec.TerminalContract.Outcome = "changed"
	spec.TerminalContract.AcceptanceCriteria[0] = "changed"
	if board.Tasks[0].TerminalContract.Outcome != "immutable outcome" || board.Tasks[0].TerminalContract.AcceptanceCriteria[0] != "immutable criterion" {
		t.Fatal("task terminal contract retained mutable command storage")
	}

	startPlanned := command(board, RoleWorker, "worker", CommandAttemptStart, "start-planned")
	startPlanned.TaskID, startPlanned.AttemptID = "task", "missing"
	if _, _, err := Apply(board, startPlanned); err == nil {
		t.Fatal("planned task transitioned directly to running")
	}

	board = activateTask(t, board, "task")
	board = completeTask(t, board, "task", "worker")
	terminal := cloneBoard(board)
	if _, _, err := Apply(board, leaseCommand(board, "task", "another-lease", "another-attempt", "worker")); err == nil {
		t.Fatal("passed task was leased again")
	}
	if board.Tasks[0].State != terminal.Tasks[0].State || board.Tasks[0].TerminalContract.Outcome != terminal.Tasks[0].TerminalContract.Outcome {
		t.Fatal("rejected terminal transition mutated task")
	}
}

func createBoard(t *testing.T) Board {
	t.Helper()
	return mustApply(t, Board{}, Command{
		SchemaVersion: SchemaVersion,
		ID:            "create",
		Actor:         Actor{ID: "coordinator", Role: RoleCoordinator},
		Type:          CommandBoardCreate,
		Board:         &BoardSpec{ID: "board", Title: "Board"},
	})
}

func addTask(t *testing.T, board Board, id string) Board {
	t.Helper()
	return mustApply(t, board, coordinatorCommand(board, CommandTaskAdd, "add-"+id, func(command *Command) {
		command.Task = &TaskSpec{ID: id, Title: id, AccessMode: AccessRead, TerminalContract: TerminalContract{Outcome: "complete " + id, AcceptanceCriteria: []string{"tests pass"}}}
	}))
}

func addDependency(t *testing.T, board Board, id, taskID, dependsOn string) Board {
	t.Helper()
	return mustApply(t, board, coordinatorCommand(board, CommandDependencyAdd, "add-"+id, func(command *Command) {
		command.Dependency = &DependencySpec{ID: id, TaskID: taskID, DependsOn: dependsOn}
	}))
}

func activateTask(t *testing.T, board Board, taskID string) Board {
	t.Helper()
	return mustApply(t, board, coordinatorCommand(board, CommandTaskActivate, "activate-"+taskID, func(command *Command) {
		command.TaskID = taskID
	}))
}

func completeTask(t *testing.T, board Board, taskID, workerID string) Board {
	t.Helper()
	attemptID, leaseID, evidenceID := "attempt-"+taskID, "lease-"+taskID, "evidence-"+taskID
	board = mustApply(t, board, leaseCommand(board, taskID, leaseID, attemptID, workerID))
	start := command(board, RoleWorker, workerID, CommandAttemptStart, "start-"+taskID)
	start.TaskID, start.AttemptID = taskID, attemptID
	board = mustApply(t, board, fenced(board, start))
	submit := command(board, RoleWorker, workerID, CommandEvidenceSubmit, "submit-"+taskID)
	submit.TaskID, submit.AttemptID = taskID, attemptID
	submit.Evidence = &EvidenceSpec{ID: evidenceID, TaskID: taskID, AttemptID: attemptID, Artifact: "commit:" + taskID, Summary: "verified"}
	board = mustApply(t, board, fenced(board, submit))
	accept := command(board, RoleVerifier, "verifier", CommandEvidenceAccept, "accept-"+taskID)
	accept.TaskID, accept.AttemptID = taskID, attemptID
	accept.Evidence = &EvidenceSpec{ID: evidenceID}
	return mustApply(t, board, fenced(board, accept))
}

func leaseCommand(board Board, taskID, leaseID, attemptID, workerID string) Command {
	return coordinatorCommand(board, CommandLeaseAcquire, "acquire-"+leaseID, func(command *Command) {
		command.Lease = &LeaseSpec{ID: leaseID, TaskID: taskID, AttemptID: attemptID, WorkerID: workerID, FencingToken: "token-" + attemptID, AcquiredAt: testClock}
	})
}

func coordinatorCommand(board Board, commandType CommandType, id string, configure func(*Command)) Command {
	command := command(board, RoleCoordinator, "coordinator", commandType, id)
	configure(&command)
	return command
}

func command(board Board, role ActorRole, actorID string, commandType CommandType, id string) Command {
	return Command{SchemaVersion: SchemaVersion, ID: id, ExpectedVersion: board.Version, Actor: Actor{ID: actorID, Role: role}, Type: commandType}
}

func mustApply(t *testing.T, board Board, command Command) Board {
	t.Helper()
	next, events, err := Apply(board, command)
	if err != nil {
		t.Fatalf("apply %s: %v", command.Type, err)
	}
	if len(events) == 0 {
		t.Fatalf("apply %s emitted no events", command.Type)
	}
	return next
}

func assertTaskState(t *testing.T, board Board, taskID string, want TaskState) {
	t.Helper()
	_, task, found := findTask(board, taskID)
	if !found || task.State != want {
		t.Fatalf("task %q state = %q (found %v), want %q", taskID, task.State, found, want)
	}
}
