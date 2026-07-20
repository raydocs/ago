package agoboardprotocol

import "fmt"

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
		next = Board{SchemaVersion: SchemaVersion, ID: command.Board.ID, Title: command.Board.Title, Tasks: []Task{}, Dependencies: []Dependency{}, Attempts: []Attempt{}, Leases: []Lease{}, Evidence: []Evidence{}}
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
		task := Task{ID: command.Task.ID, Title: command.Task.Title, State: TaskPlanned, TerminalContract: cloneContract(command.Task.TerminalContract)}
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
		index, task, found := findTask(next, command.Lease.TaskID)
		if !found {
			return current, nil, fmt.Errorf("task %q not found", command.Lease.TaskID)
		}
		if task.State != TaskReady {
			return current, nil, illegalTransition(task.State, "lease")
		}
		for _, lease := range next.Leases {
			if lease.ID == command.Lease.ID {
				return current, nil, fmt.Errorf("duplicate lease id %q", lease.ID)
			}
		}
		for _, attempt := range next.Attempts {
			if attempt.ID == command.Lease.AttemptID {
				return current, nil, fmt.Errorf("duplicate attempt id %q", attempt.ID)
			}
		}
		attempt := Attempt{ID: command.Lease.AttemptID, TaskID: task.ID, WorkerID: command.Lease.WorkerID, State: AttemptLeased}
		lease := Lease{ID: command.Lease.ID, TaskID: task.ID, AttemptID: attempt.ID, WorkerID: attempt.WorkerID, State: LeaseActive}
		next.Attempts = append(next.Attempts, attempt)
		next.Leases = append(next.Leases, lease)
		next.Tasks[index].State, next.Tasks[index].ActiveAttemptID, next.Tasks[index].ActiveLeaseID = TaskLeased, attempt.ID, lease.ID
		emit(Event{Type: EventLeaseAcquired, Task: taskPointer(next.Tasks[index]), Attempt: attemptPointer(attempt), Lease: leasePointer(lease)})
	case CommandAttemptStart:
		if err := requireRole(RoleWorker); err != nil {
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
		evidence := Evidence{ID: command.Evidence.ID, TaskID: command.TaskID, AttemptID: command.AttemptID, WorkerID: command.Actor.ID, Artifact: command.Evidence.Artifact, Summary: command.Evidence.Summary, State: EvidenceSubmitted}
		next.Evidence = append(next.Evidence, evidence)
		attemptIndex, _, _ := findAttempt(next, command.AttemptID)
		next.Attempts[attemptIndex].EvidenceID = evidence.ID
		task, attempt := taskAndAttempt(next, command.TaskID, command.AttemptID)
		emit(Event{Type: EventEvidenceSubmitted, Task: taskPointer(task), Attempt: attemptPointer(attempt), Evidence: evidencePointer(evidence), PreviousState: TaskRunning, CurrentState: TaskVerifying})
	case CommandEvidenceAccept, CommandEvidenceReject:
		if err := requireRole(RoleVerifier); err != nil {
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
			next.Tasks[taskIndex].State, next.Tasks[taskIndex].AcceptedEvidenceID = TaskPassed, evidence.ID
			next.Attempts[attemptIndex].State, next.Evidence[evidenceIndex].State = AttemptPassed, EvidenceAccepted
			next.Leases[leaseIndex].State = LeaseCompleted
			emit(Event{Type: EventEvidenceAccepted, Task: taskPointer(next.Tasks[taskIndex]), Attempt: attemptPointer(next.Attempts[attemptIndex]), Evidence: evidencePointer(next.Evidence[evidenceIndex]), Lease: leasePointer(next.Leases[leaseIndex]), PreviousState: TaskVerifying, CurrentState: TaskPassed, Reason: command.Reason})
			unblockReadyTasks(&next, emit)
		} else {
			next.Tasks[taskIndex].State = TaskFailed
			next.Attempts[attemptIndex].State, next.Evidence[evidenceIndex].State = AttemptFailed, EvidenceRejected
			next.Leases[leaseIndex].State = LeaseCompleted
			emit(Event{Type: EventEvidenceRejected, Task: taskPointer(next.Tasks[taskIndex]), Attempt: attemptPointer(next.Attempts[attemptIndex]), Evidence: evidencePointer(next.Evidence[evidenceIndex]), Lease: leasePointer(next.Leases[leaseIndex]), PreviousState: TaskVerifying, CurrentState: TaskFailed, Reason: command.Reason})
		}
	case CommandAttemptFail:
		if err := requireRole(RoleWorker); err != nil {
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
		previous := task.State
		next.Tasks[taskIndex].State, next.Attempts[attemptIndex].State, next.Leases[leaseIndex].State = TaskFailed, AttemptFailed, LeaseCompleted
		emit(Event{Type: EventAttemptStateChanged, Task: taskPointer(next.Tasks[taskIndex]), Attempt: attemptPointer(next.Attempts[attemptIndex]), Lease: leasePointer(next.Leases[leaseIndex]), PreviousState: previous, CurrentState: TaskFailed, Reason: command.Reason})
	default:
		return current, nil, fmt.Errorf("unsupported command type %q", command.Type)
	}
	if err := next.Validate(); err != nil {
		return current, nil, fmt.Errorf("command produced invalid board: %w", err)
	}
	return next, events, nil
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
