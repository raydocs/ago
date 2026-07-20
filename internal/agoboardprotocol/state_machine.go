package agoboardprotocol

import (
	"fmt"
	"strings"
	"time"
)

// Apply validates and atomically applies one command. The input is never
// mutated. Each returned event advances the aggregate version by one.
func Apply(current Board, command Command) (Board, []Event, error) {
	if command.SchemaVersion != SchemaVersion {
		return current, nil, fmt.Errorf("unsupported command schema version %d", command.SchemaVersion)
	}
	if command.ID == "" || command.Actor.ID == "" {
		return current, nil, fmt.Errorf("command id and actor id are required")
	}
	if command.ExpectedVersion != current.Version {
		return current, nil, fmt.Errorf("expected board version %d, got %d", command.ExpectedVersion, current.Version)
	}
	if current.ID != "" {
		if err := current.Validate(); err != nil {
			return current, nil, fmt.Errorf("invalid current board: %w", err)
		}
	}
	next := cloneBoard(current)
	events := make([]Event, 0, 2)
	emit := func(event Event) {
		next.Version++
		event.SchemaVersion = SchemaVersion
		event.ID = fmt.Sprintf("%s:%d", command.ID, len(events)+1)
		event.CommandID = command.ID
		event.BoardID = next.ID
		event.Version = next.Version
		event.Actor = command.Actor
		events = append(events, event)
	}
	requireRole := func(role ActorRole) error {
		if command.Actor.Role != role {
			return fmt.Errorf("command %s requires %s actor, got %s", command.Type, role, command.Actor.Role)
		}
		return nil
	}

	switch command.Type {
	case CommandBoardCreate:
		if err := requireRole(RoleCoordinator); err != nil {
			return current, nil, err
		}
		if current.ID != "" || command.Board == nil || command.Board.ID == "" || command.Board.Title == "" {
			return current, nil, fmt.Errorf("board.create requires an uninitialized aggregate and board id and title")
		}
		next = Board{
			SchemaVersion: SchemaVersion, ID: command.Board.ID, Title: command.Board.Title,
			Tasks: []Task{}, Dependencies: []Dependency{}, Attempts: []Attempt{}, Leases: []Lease{}, Evidence: []Evidence{},
			Repository: command.Board.Repository,
			// Generations start at 1 so zero can mean "no fencing authority",
			// which is what migrated schema-1 attempts carry.
			NextGeneration: 1,
		}
		emit(Event{Type: EventBoardCreated})
	case CommandTaskAdd:
		if err := requireInitialized(next); err != nil {
			return current, nil, err
		}
		if err := requireRole(RoleCoordinator); err != nil {
			return current, nil, err
		}
		if command.Task == nil || command.Task.ID == "" || command.Task.Title == "" {
			return current, nil, fmt.Errorf("task.add requires task id and title")
		}
		if err := command.Task.TerminalContract.validate(); err != nil {
			return current, nil, err
		}
		if _, _, found := findTask(next, command.Task.ID); found {
			return current, nil, fmt.Errorf("duplicate task id %q", command.Task.ID)
		}
		if !validAccessMode(command.Task.AccessMode) {
			return current, nil, fmt.Errorf("task.add requires access mode %q or %q, got %q", AccessRead, AccessWrite, command.Task.AccessMode)
		}
		task := Task{ID: command.Task.ID, Title: command.Task.Title, State: TaskPlanned, TerminalContract: cloneContract(command.Task.TerminalContract), AccessMode: command.Task.AccessMode}
		next.Tasks = append(next.Tasks, task)
		emit(Event{Type: EventTaskAdded, Task: taskPointer(task)})
	case CommandDependencyAdd:
		if err := requireInitialized(next); err != nil {
			return current, nil, err
		}
		if err := requireRole(RoleCoordinator); err != nil {
			return current, nil, err
		}
		if command.Dependency == nil || command.Dependency.ID == "" {
			return current, nil, fmt.Errorf("dependency.add requires a dependency id")
		}
		if command.Dependency.TaskID == command.Dependency.DependsOn {
			return current, nil, fmt.Errorf("dependency %q is a self-loop", command.Dependency.ID)
		}
		_, task, found := findTask(next, command.Dependency.TaskID)
		if !found {
			return current, nil, fmt.Errorf("task %q not found", command.Dependency.TaskID)
		}
		if task.State != TaskPlanned {
			return current, nil, illegalTransition(task.State, "add dependency")
		}
		if _, _, found := findTask(next, command.Dependency.DependsOn); !found {
			return current, nil, fmt.Errorf("prerequisite task %q not found", command.Dependency.DependsOn)
		}
		for _, existing := range next.Dependencies {
			if existing.ID == command.Dependency.ID {
				return current, nil, fmt.Errorf("duplicate dependency id %q", existing.ID)
			}
			if existing.TaskID == command.Dependency.TaskID && existing.DependsOn == command.Dependency.DependsOn {
				return current, nil, fmt.Errorf("duplicate dependency edge")
			}
		}
		dependency := Dependency(*command.Dependency)
		next.Dependencies = append(next.Dependencies, dependency)
		if err := next.Validate(); err != nil {
			return current, nil, err
		}
		emit(Event{Type: EventDependencyAdded, Dependency: dependencyPointer(dependency)})
	case CommandTaskActivate:
		if err := requireInitialized(next); err != nil {
			return current, nil, err
		}
		if err := requireRole(RoleCoordinator); err != nil {
			return current, nil, err
		}
		index, task, found := findTask(next, command.TaskID)
		if !found {
			return current, nil, fmt.Errorf("task %q not found", command.TaskID)
		}
		if task.State != TaskPlanned {
			return current, nil, illegalTransition(task.State, "activate")
		}
		state := TaskReady
		if !dependenciesPassed(next, task.ID) {
			state = TaskBlocked
		}
		next.Tasks[index].State = state
		emit(stateEvent(task, next.Tasks[index], command.Reason))
	case CommandLeaseAcquire:
		if err := requireInitialized(next); err != nil {
			return current, nil, err
		}
		if err := requireRole(RoleCoordinator); err != nil {
			return current, nil, err
		}
		if command.Lease == nil || command.Lease.ID == "" || command.Lease.AttemptID == "" || command.Lease.TaskID == "" || command.Lease.WorkerID == "" {
			return current, nil, fmt.Errorf("lease.acquire requires lease, attempt, task, and worker ids")
		}
		if command.Lease.FencingToken == "" || command.Lease.AcquiredAt.IsZero() {
			return current, nil, fmt.Errorf("lease.acquire requires a fencing token and an acquisition time")
		}
		// A paused board keeps running attempts but admits no new claims.
		if next.Paused {
			return current, nil, fmt.Errorf("board %q is paused: %s", next.ID, next.PauseReason)
		}
		index, task, found := findTask(next, command.Lease.TaskID)
		if !found {
			return current, nil, fmt.Errorf("task %q not found", command.Lease.TaskID)
		}
		if task.State != TaskReady && task.State != TaskRetryWait {
			return current, nil, illegalTransition(task.State, "lease")
		}
		// Backoff is enforced here, against the scheduler's declared clock
		// reading, so a task cannot be retried early even by a buggy caller.
		if task.State == TaskRetryWait && command.Lease.AcquiredAt.Before(task.NextEligibleAt) {
			return current, nil, fmt.Errorf("task %q is not eligible to retry until %s", task.ID, task.NextEligibleAt.UTC().Format(time.RFC3339Nano))
		}
		if task.AttemptCount >= MaxAttempts {
			return current, nil, fmt.Errorf("task %q exhausted its %d attempts", task.ID, MaxAttempts)
		}
		for _, lease := range next.Leases {
			if lease.ID == command.Lease.ID {
				return current, nil, fmt.Errorf("duplicate lease id %q", lease.ID)
			}
			if lease.FencingToken == command.Lease.FencingToken {
				return current, nil, fmt.Errorf("fencing token was already issued")
			}
		}
		for _, attempt := range next.Attempts {
			if attempt.ID == command.Lease.AttemptID {
				return current, nil, fmt.Errorf("duplicate attempt id %q", attempt.ID)
			}
			if attempt.FencingToken == command.Lease.FencingToken {
				return current, nil, fmt.Errorf("fencing token was already issued")
			}
		}
		// The generation is minted from the board's monotonic counter, so a
		// superseded attempt's token can never become valid again.
		generation := next.NextGeneration
		next.NextGeneration++
		attempt := Attempt{
			ID: command.Lease.AttemptID, TaskID: task.ID, WorkerID: command.Lease.WorkerID, State: AttemptLeased,
			Number: task.AttemptCount + 1, Generation: generation, FencingToken: command.Lease.FencingToken,
		}
		lease := Lease{
			ID: command.Lease.ID, TaskID: task.ID, AttemptID: attempt.ID, WorkerID: attempt.WorkerID, State: LeaseActive,
			Generation: generation, FencingToken: command.Lease.FencingToken,
			AcquiredAt: command.Lease.AcquiredAt.UTC(), ExpiresAt: expiryUTC(command.Lease.ExpiresAt),
		}
		next.Attempts = append(next.Attempts, attempt)
		next.Leases = append(next.Leases, lease)
		next.Tasks[index].State = TaskLeased
		next.Tasks[index].ActiveAttemptID, next.Tasks[index].ActiveLeaseID = attempt.ID, lease.ID
		next.Tasks[index].AttemptCount = attempt.Number
		next.Tasks[index].NextEligibleAt = time.Time{}
		next.Tasks[index].FailureClass, next.Tasks[index].BlockedReason = FailureNone, ""
		emit(Event{Type: EventLeaseAcquired, Task: taskPointer(next.Tasks[index]), Attempt: attemptPointer(attempt), Lease: leasePointer(lease)})
	case CommandAttemptStart:
		if err := requireRole(RoleWorker); err != nil {
			return current, nil, err
		}
		if err := requireFencingToken(next, command); err != nil {
			return current, nil, err
		}
		if err := transitionAttempt(&next, command, TaskLeased, TaskRunning, AttemptLeased, AttemptRunning); err != nil {
			return current, nil, err
		}
		task, attempt := taskAndAttempt(next, command.TaskID, command.AttemptID)
		emit(Event{Type: EventAttemptStateChanged, Task: taskPointer(task), Attempt: attemptPointer(attempt), PreviousState: TaskLeased, CurrentState: TaskRunning})
	case CommandEvidenceSubmit:
		if err := requireRole(RoleWorker); err != nil {
			return current, nil, err
		}
		if err := requireFencingToken(next, command); err != nil {
			return current, nil, err
		}
		if command.Evidence == nil || command.Evidence.ID == "" || command.Evidence.Artifact == "" || command.Evidence.Summary == "" {
			return current, nil, fmt.Errorf("evidence.submit requires evidence id, artifact, and summary")
		}
		if command.Evidence.TaskID != command.TaskID || command.Evidence.AttemptID != command.AttemptID {
			return current, nil, fmt.Errorf("evidence does not match command task and attempt")
		}
		if err := transitionAttempt(&next, command, TaskRunning, TaskVerifying, AttemptRunning, AttemptVerifying); err != nil {
			return current, nil, err
		}
		for _, item := range next.Evidence {
			if item.ID == command.Evidence.ID {
				return current, nil, fmt.Errorf("duplicate evidence id %q", item.ID)
			}
		}
		if err := validateEvidenceResult(command.Evidence.Result); err != nil {
			return current, nil, err
		}
		result := cloneEvidenceResult(command.Evidence.Result)
		// The generation is recorded so a reader can tell which attempt
		// produced this evidence without ever seeing the fencing token.
		if _, attempt, found := findAttempt(next, command.AttemptID); found {
			result.Generation = attempt.Generation
		}
		evidence := Evidence{ID: command.Evidence.ID, TaskID: command.TaskID, AttemptID: command.AttemptID, WorkerID: command.Actor.ID, Artifact: command.Evidence.Artifact, Summary: command.Evidence.Summary, State: EvidenceSubmitted, Result: result}
		next.Evidence = append(next.Evidence, evidence)
		attemptIndex, _, _ := findAttempt(next, command.AttemptID)
		next.Attempts[attemptIndex].EvidenceID = evidence.ID
		task, attempt := taskAndAttempt(next, command.TaskID, command.AttemptID)
		emit(Event{Type: EventEvidenceSubmitted, Task: taskPointer(task), Attempt: attemptPointer(attempt), Evidence: evidencePointer(evidence), PreviousState: TaskRunning, CurrentState: TaskVerifying})
	case CommandEvidenceAccept, CommandEvidenceReject:
		if err := requireRole(RoleVerifier); err != nil {
			return current, nil, err
		}
		// The verifier's decision is bound to the exact attempt it reviewed, so
		// a decision about superseded work cannot land on a newer attempt.
		if err := requireFencingToken(next, command); err != nil {
			return current, nil, err
		}
		if command.Evidence == nil || command.Evidence.ID == "" {
			return current, nil, fmt.Errorf("evidence decision requires evidence id")
		}
		taskIndex, task, found := findTask(next, command.TaskID)
		if !found || task.State != TaskVerifying || task.ActiveAttemptID != command.AttemptID {
			return current, nil, illegalTransition(task.State, "verify")
		}
		attemptIndex, attempt, found := findAttempt(next, command.AttemptID)
		if !found || attempt.State != AttemptVerifying || attempt.EvidenceID != command.Evidence.ID {
			return current, nil, fmt.Errorf("attempt is not verifying the specified evidence")
		}
		if command.Actor.ID == attempt.WorkerID {
			return current, nil, fmt.Errorf("evidence reviewer must be independent from the attempt worker")
		}
		evidenceIndex, evidence, found := findEvidence(next, command.Evidence.ID)
		if !found || evidence.State != EvidenceSubmitted {
			return current, nil, fmt.Errorf("evidence %q is not submitted", command.Evidence.ID)
		}
		leaseIndex, lease, found := findLease(next, task.ActiveLeaseID)
		if !found || lease.State != LeaseActive {
			return current, nil, fmt.Errorf("active lease not found")
		}
		if command.Type == CommandEvidenceAccept {
			// Deterministic checks outrank judgement. A verifier may not accept
			// work whose own recorded required tests did not pass, however
			// confident its reasoning is.
			if failed := evidence.Result.FailedRequiredTests(); len(failed) > 0 {
				return current, nil, fmt.Errorf("evidence %q cannot be accepted: required tests failed: %v", evidence.ID, failed)
			}
			next.Evidence[evidenceIndex].Verdict, next.Evidence[evidenceIndex].VerdictReason = "accept", command.Reason
			next.Tasks[taskIndex].State, next.Tasks[taskIndex].AcceptedEvidenceID = TaskPassed, evidence.ID
			next.Attempts[attemptIndex].State, next.Evidence[evidenceIndex].State = AttemptPassed, EvidenceAccepted
			next.Leases[leaseIndex].State = LeaseCompleted
			emit(Event{Type: EventEvidenceAccepted, Task: taskPointer(next.Tasks[taskIndex]), Attempt: attemptPointer(next.Attempts[attemptIndex]), Evidence: evidencePointer(next.Evidence[evidenceIndex]), Lease: leasePointer(next.Leases[leaseIndex]), PreviousState: TaskVerifying, CurrentState: TaskPassed, Reason: command.Reason})
			unblockReadyTasks(&next, emit)
		} else {
			// A rejection is a failure like any other: it either earns a
			// bounded retry or stops the task, and that decision is durable.
			class := command.FailureClass
			if class == FailureNone {
				class = FailureVerifierFeedback
			}
			if err := recordAttemptFailure(&next, taskIndex, attemptIndex, leaseIndex, class, command.Reason, command.NextEligibleAt); err != nil {
				return current, nil, err
			}
			next.Evidence[evidenceIndex].State = EvidenceRejected
			next.Evidence[evidenceIndex].Verdict, next.Evidence[evidenceIndex].VerdictReason = "reject", command.Reason
			emit(Event{Type: EventEvidenceRejected, Task: taskPointer(next.Tasks[taskIndex]), Attempt: attemptPointer(next.Attempts[attemptIndex]), Evidence: evidencePointer(next.Evidence[evidenceIndex]), Lease: leasePointer(next.Leases[leaseIndex]), PreviousState: TaskVerifying, CurrentState: next.Tasks[taskIndex].State, Reason: command.Reason})
		}
	case CommandAttemptFail:
		if err := requireRole(RoleWorker); err != nil {
			return current, nil, err
		}
		if err := requireFencingToken(next, command); err != nil {
			return current, nil, err
		}
		taskIndex, task, found := findTask(next, command.TaskID)
		if !found || (task.State != TaskLeased && task.State != TaskRunning) {
			return current, nil, illegalTransition(task.State, "fail")
		}
		attemptIndex, attempt, found := findAttempt(next, command.AttemptID)
		if !found || attempt.WorkerID != command.Actor.ID || task.ActiveAttemptID != attempt.ID {
			return current, nil, fmt.Errorf("worker does not own active attempt")
		}
		leaseIndex, lease, found := findLease(next, task.ActiveLeaseID)
		if !found || lease.State != LeaseActive || lease.WorkerID != command.Actor.ID {
			return current, nil, fmt.Errorf("worker does not own active lease")
		}
		// An unclassified failure is not assumed retryable: callers must say
		// what happened, so an unknown fault stops rather than loops.
		if command.FailureClass == FailureNone {
			return current, nil, fmt.Errorf("attempt.fail requires a failure class")
		}
		previous := task.State
		if err := recordAttemptFailure(&next, taskIndex, attemptIndex, leaseIndex, command.FailureClass, command.Reason, command.NextEligibleAt); err != nil {
			return current, nil, err
		}
		emit(Event{Type: EventAttemptStateChanged, Task: taskPointer(next.Tasks[taskIndex]), Attempt: attemptPointer(next.Attempts[attemptIndex]), Lease: leasePointer(next.Leases[leaseIndex]), PreviousState: previous, CurrentState: next.Tasks[taskIndex].State, Reason: command.Reason})
	case CommandLeaseExpire:
		if err := requireRole(RoleCoordinator); err != nil {
			return current, nil, err
		}
		if command.Lease == nil || command.Lease.ID == "" {
			return current, nil, fmt.Errorf("lease.expire requires a lease id")
		}
		leaseIndex, lease, found := findLease(next, command.Lease.ID)
		if !found || lease.State != LeaseActive {
			return current, nil, fmt.Errorf("active lease %q not found", command.Lease.ID)
		}
		taskIndex, task, found := findTask(next, lease.TaskID)
		if !found {
			return current, nil, fmt.Errorf("task %q not found", lease.TaskID)
		}
		attemptIndex, _, found := findAttempt(next, lease.AttemptID)
		if !found {
			return current, nil, fmt.Errorf("attempt %q not found", lease.AttemptID)
		}
		// Reclaiming a lease is transient by nature: the work may simply not
		// have finished. Whether a retry actually happens is still bounded by
		// the attempt count.
		class := command.FailureClass
		if class == FailureNone {
			class = FailureTransient
		}
		previous := task.State
		if err := recordAttemptFailure(&next, taskIndex, attemptIndex, leaseIndex, class, command.Reason, command.NextEligibleAt); err != nil {
			return current, nil, err
		}
		emit(Event{Type: EventLeaseExpired, Task: taskPointer(next.Tasks[taskIndex]), Attempt: attemptPointer(next.Attempts[attemptIndex]), Lease: leasePointer(next.Leases[leaseIndex]), PreviousState: previous, CurrentState: next.Tasks[taskIndex].State, Reason: command.Reason})
	case CommandLeaseRenew:
		if err := requireRole(RoleCoordinator); err != nil {
			return current, nil, err
		}
		if command.Lease == nil || command.Lease.ID == "" || command.Lease.ExpiresAt.IsZero() {
			return current, nil, fmt.Errorf("lease.renew requires a lease id and a new deadline")
		}
		leaseIndex, lease, found := findLease(next, command.Lease.ID)
		if !found || lease.State != LeaseActive {
			return current, nil, fmt.Errorf("active lease %q not found", command.Lease.ID)
		}
		// Renewal is fenced: only the current generation may keep a lease alive.
		// A lease migrated from schema 1 has no token and so cannot be renewed
		// at all, which is the conservative outcome for work whose executor
		// predates fencing.
		if lease.FencingToken == "" || command.Lease.FencingToken != lease.FencingToken {
			return current, nil, fmt.Errorf("fencing token does not authorize lease %q", command.Lease.ID)
		}
		if !command.Lease.ExpiresAt.After(lease.ExpiresAt) && !lease.ExpiresAt.IsZero() {
			return current, nil, fmt.Errorf("lease renewal must extend the deadline")
		}
		next.Leases[leaseIndex].ExpiresAt = command.Lease.ExpiresAt.UTC()
		taskIndex, _, _ := findTask(next, lease.TaskID)
		attemptIndex, _, _ := findAttempt(next, lease.AttemptID)
		emit(Event{Type: EventLeaseRenewed, Task: taskPointer(next.Tasks[taskIndex]), Attempt: attemptPointer(next.Attempts[attemptIndex]), Lease: leasePointer(next.Leases[leaseIndex]), Reason: command.Reason})
	case CommandTaskRetry:
		if err := requireInitialized(next); err != nil {
			return current, nil, err
		}
		if err := requireRole(RoleCoordinator); err != nil {
			return current, nil, err
		}
		if command.Reason == "" {
			return current, nil, fmt.Errorf("task.retry requires a reason recording the decision")
		}
		index, task, found := findTask(next, command.TaskID)
		if !found {
			return current, nil, fmt.Errorf("task %q not found", command.TaskID)
		}
		// Only a stopped task may be restarted. Retrying running or accepted
		// work would either duplicate a live attempt or discard a decision.
		if task.State != TaskFailed && task.State != TaskRetryWait {
			return current, nil, illegalTransition(task.State, "retry")
		}
		previous := task
		// The budget resets because a person decided to spend another one. The
		// attempts themselves stay in history; nothing is erased.
		next.Tasks[index].AttemptCount = 0
		next.Tasks[index].UserRetries = task.UserRetries + 1
		next.Tasks[index].NextEligibleAt = time.Time{}
		next.Tasks[index].FailureClass, next.Tasks[index].BlockedReason = FailureNone, ""
		next.Tasks[index].ActiveAttemptID, next.Tasks[index].ActiveLeaseID = "", ""
		state := TaskReady
		if !dependenciesPassed(next, task.ID) {
			state = TaskBlocked
		}
		next.Tasks[index].State = state
		emit(Event{Type: EventTaskRetryRequested, Task: taskPointer(next.Tasks[index]), PreviousState: previous.State, CurrentState: state, Reason: command.Reason})
	case CommandPlanPatch:
		if err := requireInitialized(next); err != nil {
			return current, nil, err
		}
		if err := requireRole(RoleCoordinator); err != nil {
			return current, nil, err
		}
		if command.Patch == nil || command.Patch.ID == "" || len(command.Patch.Steps) == 0 {
			return current, nil, fmt.Errorf("plan.patch requires an id and at least one step")
		}
		if command.Patch.Reason == "" {
			return current, nil, fmt.Errorf("plan.patch requires a reason recording why the plan changed")
		}
		if len(command.Patch.Steps) > maxPatchSteps {
			return current, nil, fmt.Errorf("plan.patch exceeds %d steps", maxPatchSteps)
		}
		// Every step is applied to one working copy. A failure anywhere returns
		// the original board, so a patch is all-or-nothing and a partially
		// applied plan can never be observed.
		for index, step := range command.Patch.Steps {
			if err := applyPatchStep(&next, command.Patch.ID, step); err != nil {
				return current, nil, fmt.Errorf("plan patch %q step %d (%s): %w", command.Patch.ID, index+1, step.Operation, err)
			}
		}
		if err := next.Validate(); err != nil {
			return current, nil, fmt.Errorf("plan patch %q produced an invalid graph: %w", command.Patch.ID, err)
		}
		emit(Event{Type: EventPlanPatched, Reason: command.Patch.Reason})
		// A patch can satisfy the last dependency a blocked task was waiting on.
		unblockReadyTasks(&next, emit)
	case CommandBoardPause, CommandBoardResume:
		if err := requireInitialized(next); err != nil {
			return current, nil, err
		}
		if err := requireRole(RoleCoordinator); err != nil {
			return current, nil, err
		}
		pause := command.Type == CommandBoardPause
		if pause && command.Reason == "" {
			return current, nil, fmt.Errorf("board.pause requires a reason")
		}
		// Repeating a transition is an error rather than a silent success, so a
		// caller cannot mistake a no-op for having taken control.
		if next.Paused == pause {
			state := "running"
			if next.Paused {
				state = "paused"
			}
			return current, nil, fmt.Errorf("board %q is already %s", next.ID, state)
		}
		next.Paused = pause
		next.PauseReason = ""
		eventType := EventBoardResumed
		if pause {
			next.PauseReason, eventType = command.Reason, EventBoardPaused
		}
		emit(Event{Type: eventType, Reason: command.Reason})
	default:
		return current, nil, fmt.Errorf("unsupported command type %q", command.Type)
	}
	if err := next.Validate(); err != nil {
		return current, nil, fmt.Errorf("command produced invalid board: %w", err)
	}
	return next, events, nil
}

// requireFencingToken proves the caller is speaking for the exact attempt it
// names. An attempt without a token — which is how schema-1 attempts are
// migrated — can never be authenticated, so its executor holds no authority.
func requireFencingToken(board Board, command Command) error {
	if command.AttemptID == "" {
		return fmt.Errorf("command %s requires an attempt id", command.Type)
	}
	_, attempt, found := findAttempt(board, command.AttemptID)
	if !found {
		return fmt.Errorf("attempt %q not found", command.AttemptID)
	}
	if attempt.FencingToken == "" || command.FencingToken == "" || attempt.FencingToken != command.FencingToken {
		return fmt.Errorf("fencing token does not authorize attempt %q", command.AttemptID)
	}
	return nil
}

// recordAttemptFailure closes an attempt and applies the bounded retry policy.
//
// It is the single place that decides retry-wait versus a terminal stop, so
// executor failure, lease expiry, and verifier rejection cannot drift apart.
// The attempt bound is enforced here rather than trusted from the caller.
func recordAttemptFailure(board *Board, taskIndex, attemptIndex, leaseIndex int, class FailureClass, reason string, nextEligibleAt time.Time) error {
	if !validFailureClass(class) || class == FailureNone {
		return fmt.Errorf("invalid failure class %q", class)
	}
	task := &board.Tasks[taskIndex]
	retryable := class.Retryable() && task.AttemptCount < MaxAttempts
	if retryable && nextEligibleAt.IsZero() {
		return fmt.Errorf("a retryable failure requires the next eligible time")
	}
	board.Attempts[attemptIndex].State = AttemptFailed
	board.Attempts[attemptIndex].FailureClass = class
	board.Attempts[attemptIndex].FailureReason = reason
	board.Leases[leaseIndex].State = LeaseCompleted
	// Clearing the active pointers is what allows a later attempt to be
	// claimed, and what stops the superseded one from being addressed.
	task.ActiveAttemptID, task.ActiveLeaseID = "", ""
	if retryable {
		task.State, task.NextEligibleAt = TaskRetryWait, nextEligibleAt.UTC()
		task.FailureClass, task.BlockedReason = class, reason
		return nil
	}
	task.State, task.NextEligibleAt = TaskFailed, time.Time{}
	task.FailureClass, task.BlockedReason = class, reason
	if class.Retryable() {
		// The class was retryable but the budget is gone; record why it stopped.
		task.FailureClass = FailureExhausted
	}
	return nil
}

func expiryUTC(value time.Time) time.Time {
	if value.IsZero() {
		return time.Time{}
	}
	return value.UTC()
}

func transitionAttempt(board *Board, command Command, fromTask, toTask TaskState, fromAttempt, toAttempt AttemptState) error {
	taskIndex, task, found := findTask(*board, command.TaskID)
	if !found || task.State != fromTask || task.ActiveAttemptID != command.AttemptID {
		return illegalTransition(task.State, string(toTask))
	}
	attemptIndex, attempt, found := findAttempt(*board, command.AttemptID)
	if !found || attempt.State != fromAttempt || attempt.WorkerID != command.Actor.ID {
		return fmt.Errorf("worker does not own active attempt in state %s", fromAttempt)
	}
	_, lease, found := findLease(*board, task.ActiveLeaseID)
	if !found || lease.State != LeaseActive || lease.WorkerID != command.Actor.ID {
		return fmt.Errorf("worker does not own active lease")
	}
	board.Tasks[taskIndex].State, board.Attempts[attemptIndex].State = toTask, toAttempt
	return nil
}

func unblockReadyTasks(board *Board, emit func(Event)) {
	for index := range board.Tasks {
		if board.Tasks[index].State == TaskBlocked && dependenciesPassed(*board, board.Tasks[index].ID) {
			previous := board.Tasks[index]
			board.Tasks[index].State = TaskReady
			emit(stateEvent(previous, board.Tasks[index], "dependencies passed"))
		}
	}
}

func dependenciesPassed(board Board, taskID string) bool {
	for _, dependency := range board.Dependencies {
		if dependency.TaskID != taskID {
			continue
		}
		_, prerequisite, found := findTask(board, dependency.DependsOn)
		if !found || prerequisite.State != TaskPassed {
			return false
		}
	}
	return true
}

func requireInitialized(board Board) error {
	if board.ID == "" {
		return fmt.Errorf("board is not initialized")
	}
	return nil
}
func illegalTransition(state TaskState, action string) error {
	return fmt.Errorf("cannot %s task in %s state", action, state)
}
func stateEvent(previous, current Task, reason string) Event {
	return Event{Type: EventTaskStateChanged, Task: taskPointer(current), PreviousState: previous.State, CurrentState: current.State, Reason: reason}
}

func findTask(board Board, id string) (int, Task, bool) {
	for i, value := range board.Tasks {
		if value.ID == id {
			return i, value, true
		}
	}
	return -1, Task{}, false
}
func findAttempt(board Board, id string) (int, Attempt, bool) {
	for i, value := range board.Attempts {
		if value.ID == id {
			return i, value, true
		}
	}
	return -1, Attempt{}, false
}
func findLease(board Board, id string) (int, Lease, bool) {
	for i, value := range board.Leases {
		if value.ID == id {
			return i, value, true
		}
	}
	return -1, Lease{}, false
}
func findEvidence(board Board, id string) (int, Evidence, bool) {
	for i, value := range board.Evidence {
		if value.ID == id {
			return i, value, true
		}
	}
	return -1, Evidence{}, false
}
func taskAndAttempt(board Board, taskID, attemptID string) (Task, Attempt) {
	_, task, _ := findTask(board, taskID)
	_, attempt, _ := findAttempt(board, attemptID)
	return task, attempt
}

func cloneContract(value TerminalContract) TerminalContract {
	value.AcceptanceCriteria = append([]string(nil), value.AcceptanceCriteria...)
	return value
}
func cloneBoard(value Board) Board {
	value.Tasks = append([]Task(nil), value.Tasks...)
	for index := range value.Tasks {
		value.Tasks[index].TerminalContract = cloneContract(value.Tasks[index].TerminalContract)
	}
	value.Dependencies = append([]Dependency(nil), value.Dependencies...)
	value.Attempts = append([]Attempt(nil), value.Attempts...)
	value.Leases = append([]Lease(nil), value.Leases...)
	value.Evidence = append([]Evidence(nil), value.Evidence...)
	for index := range value.Evidence {
		value.Evidence[index].Result = cloneEvidenceResult(value.Evidence[index].Result)
	}
	return value
}
func taskPointer(value Task) *Task {
	value.TerminalContract = cloneContract(value.TerminalContract)
	return &value
}
func dependencyPointer(value Dependency) *Dependency { return &value }
func attemptPointer(value Attempt) *Attempt          { return &value }
func leasePointer(value Lease) *Lease                { return &value }
func evidencePointer(value Evidence) *Evidence       { return &value }

// validateEvidenceResult keeps structured evidence bounded and free of paths
// that could describe work outside the repository.
func validateEvidenceResult(result EvidenceResult) error {
	if len(result.ChangedFiles) > maxEvidenceItems || len(result.Commands) > maxEvidenceItems ||
		len(result.Tests) > maxEvidenceItems || len(result.Artifacts) > maxEvidenceItems ||
		len(result.Warnings) > maxEvidenceItems {
		return fmt.Errorf("structured evidence exceeds %d items in a section", maxEvidenceItems)
	}
	for _, file := range result.ChangedFiles {
		if file.Path == "" {
			return fmt.Errorf("a changed file requires a path")
		}
		if !relativeRepositoryPath(file.Path) {
			return fmt.Errorf("changed file %q must be a repository-relative path", file.Path)
		}
	}
	for _, test := range result.Tests {
		if test.Name == "" {
			return fmt.Errorf("a test record requires a name")
		}
	}
	for _, artifact := range result.Artifacts {
		if artifact.ID == "" || artifact.SHA256 == "" {
			return fmt.Errorf("an artifact reference requires an id and a digest")
		}
		if artifact.Bytes < 0 {
			return fmt.Errorf("artifact %q has a negative size", artifact.ID)
		}
	}
	return nil
}

// maxEvidenceItems bounds each section so one attempt cannot make the aggregate
// unbounded.
const maxEvidenceItems = 256

func relativeRepositoryPath(value string) bool {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") {
		return false
	}
	if value == ".." || strings.HasPrefix(value, "../") || strings.Contains(value, "/../") || strings.HasSuffix(value, "/..") {
		return false
	}
	// A Windows-style drive letter is absolute too.
	if len(value) >= 2 && value[1] == ':' {
		return false
	}
	return true
}

func cloneEvidenceResult(result EvidenceResult) EvidenceResult {
	result.ChangedFiles = append([]ChangedFile(nil), result.ChangedFiles...)
	result.Commands = append([]CommandRecord(nil), result.Commands...)
	result.Tests = append([]TestRecord(nil), result.Tests...)
	result.Artifacts = append([]ArtifactRef(nil), result.Artifacts...)
	result.Warnings = append([]string(nil), result.Warnings...)
	return result
}

// maxPatchSteps bounds one patch so a single command cannot rewrite the whole
// graph in an unreviewable jump.
const maxPatchSteps = 32

// applyPatchStep mutates the working board for one operation.
//
// The rules exist to protect history: accepted work is immutable, running work
// is not silently removed, and a superseded task keeps its attempts and
// evidence rather than being deleted.
func applyPatchStep(board *Board, patchID string, step PatchStep) error {
	switch step.Operation {
	case PatchAddTask:
		if step.Task == nil || step.Task.ID == "" || step.Task.Title == "" {
			return fmt.Errorf("add_task requires task id and title")
		}
		if err := step.Task.TerminalContract.validate(); err != nil {
			return err
		}
		if !validAccessMode(step.Task.AccessMode) {
			return fmt.Errorf("add_task requires a valid access mode")
		}
		if _, _, found := findTask(*board, step.Task.ID); found {
			return fmt.Errorf("duplicate task id %q", step.Task.ID)
		}
		task := Task{
			ID: step.Task.ID, Title: step.Task.Title, State: TaskPlanned,
			TerminalContract: cloneContract(step.Task.TerminalContract),
			AccessMode:       step.Task.AccessMode,
			Origin:           patchID,
		}
		board.Tasks = append(board.Tasks, task)
		// A task added by a patch is activated immediately: the graph is
		// already running, so leaving it planned would strand it.
		index := len(board.Tasks) - 1
		if dependenciesPassed(*board, task.ID) {
			board.Tasks[index].State = TaskReady
		} else {
			board.Tasks[index].State = TaskBlocked
		}
	case PatchAddDependency:
		if step.TaskID == "" || step.DependsOn == "" || step.TaskID == step.DependsOn {
			return fmt.Errorf("add_dependency requires two distinct task ids")
		}
		index, task, found := findTask(*board, step.TaskID)
		if !found {
			return fmt.Errorf("task %q not found", step.TaskID)
		}
		if _, _, found := findTask(*board, step.DependsOn); !found {
			return fmt.Errorf("prerequisite %q not found", step.DependsOn)
		}
		if task.State == TaskPassed {
			return fmt.Errorf("cannot add a dependency to accepted task %q", step.TaskID)
		}
		id := patchID + ":" + step.TaskID + ":" + step.DependsOn
		for _, existing := range board.Dependencies {
			if existing.TaskID == step.TaskID && existing.DependsOn == step.DependsOn {
				return fmt.Errorf("dependency already exists")
			}
			if existing.ID == id {
				return fmt.Errorf("duplicate dependency id %q", id)
			}
		}
		board.Dependencies = append(board.Dependencies, Dependency{ID: id, TaskID: step.TaskID, DependsOn: step.DependsOn})
		// A new prerequisite can make ready work no longer ready.
		if board.Tasks[index].State == TaskReady && !dependenciesPassed(*board, step.TaskID) {
			board.Tasks[index].State = TaskBlocked
		}
	case PatchRemoveDependency:
		if step.TaskID == "" || step.DependsOn == "" {
			return fmt.Errorf("remove_dependency requires two task ids")
		}
		for position, existing := range board.Dependencies {
			if existing.TaskID == step.TaskID && existing.DependsOn == step.DependsOn {
				board.Dependencies = append(board.Dependencies[:position], board.Dependencies[position+1:]...)
				return nil
			}
		}
		return fmt.Errorf("dependency %q -> %q not found", step.TaskID, step.DependsOn)
	case PatchUpdateAcceptance:
		if step.TaskID == "" || step.Acceptance == nil {
			return fmt.Errorf("update_acceptance requires a task id and acceptance criteria")
		}
		if err := step.Acceptance.validate(); err != nil {
			return err
		}
		index, task, found := findTask(*board, step.TaskID)
		if !found {
			return fmt.Errorf("task %q not found", step.TaskID)
		}
		// Changing what "done" means after work was accepted would rewrite the
		// meaning of a decision already made.
		if task.State == TaskPassed {
			return fmt.Errorf("cannot change the acceptance criteria of accepted task %q", step.TaskID)
		}
		board.Tasks[index].TerminalContract = cloneContract(*step.Acceptance)
	case PatchSupersedeTask:
		if step.TaskID == "" || step.SupersededBy == "" || step.TaskID == step.SupersededBy {
			return fmt.Errorf("supersede_task requires a task and a distinct replacement")
		}
		index, task, found := findTask(*board, step.TaskID)
		if !found {
			return fmt.Errorf("task %q not found", step.TaskID)
		}
		if _, _, found := findTask(*board, step.SupersededBy); !found {
			return fmt.Errorf("replacement task %q not found", step.SupersededBy)
		}
		if task.State == TaskPassed {
			return fmt.Errorf("cannot supersede accepted task %q", step.TaskID)
		}
		if task.State == TaskLeased || task.State == TaskRunning || task.State == TaskVerifying {
			// Running work is never silently removed. It must be cancelled
			// explicitly, which is a separate, visible decision.
			return fmt.Errorf("task %q is still running; cancel it before superseding", step.TaskID)
		}
		board.Tasks[index].SupersededBy = step.SupersededBy
		board.Tasks[index].State = TaskFailed
		board.Tasks[index].FailureClass = FailureNone
		board.Tasks[index].BlockedReason = "superseded by " + step.SupersededBy
	case PatchCancelTask:
		if step.TaskID == "" {
			return fmt.Errorf("cancel_task requires a task id")
		}
		index, task, found := findTask(*board, step.TaskID)
		if !found {
			return fmt.Errorf("task %q not found", step.TaskID)
		}
		if task.State == TaskPassed {
			return fmt.Errorf("cannot cancel accepted task %q", step.TaskID)
		}
		if task.State == TaskLeased || task.State == TaskRunning || task.State == TaskVerifying {
			// Cancelling live work has to end its lease too, or the attempt
			// would keep running against a task nobody is waiting for.
			for position := range board.Leases {
				if board.Leases[position].TaskID == task.ID && board.Leases[position].State == LeaseActive {
					board.Leases[position].State = LeaseCompleted
				}
			}
			for position := range board.Attempts {
				if board.Attempts[position].ID == task.ActiveAttemptID {
					board.Attempts[position].State = AttemptFailed
					board.Attempts[position].FailureClass = FailurePolicy
					board.Attempts[position].FailureReason = "cancelled by plan patch"
				}
			}
		}
		board.Tasks[index].State = TaskFailed
		board.Tasks[index].Cancelled = true
		board.Tasks[index].ActiveAttemptID, board.Tasks[index].ActiveLeaseID = "", ""
		board.Tasks[index].FailureClass = FailurePolicy
		board.Tasks[index].BlockedReason = firstNonBlank(step.Reason, "cancelled by plan patch")
	default:
		return fmt.Errorf("unsupported patch operation %q", step.Operation)
	}
	return nil
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
