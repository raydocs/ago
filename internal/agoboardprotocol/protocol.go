// Package agoboardprotocol defines the versioned, persistence-neutral Work
// Graph contract. Storage and transport implementations must preserve these
// types and must apply commands through Apply rather than writing states.
package agoboardprotocol

import "fmt"

const SchemaVersion = 1

type TaskState string

const (
	TaskPlanned   TaskState = "planned"
	TaskBlocked   TaskState = "blocked"
	TaskReady     TaskState = "ready"
	TaskLeased    TaskState = "leased"
	TaskRunning   TaskState = "running"
	TaskVerifying TaskState = "verifying"
	TaskPassed    TaskState = "passed"
	TaskFailed    TaskState = "failed"
)

type AttemptState string

const (
	AttemptLeased    AttemptState = "leased"
	AttemptRunning   AttemptState = "running"
	AttemptVerifying AttemptState = "verifying"
	AttemptPassed    AttemptState = "passed"
	AttemptFailed    AttemptState = "failed"
)

type LeaseState string

const (
	LeaseActive    LeaseState = "active"
	LeaseCompleted LeaseState = "completed"
)

type EvidenceState string

const (
	EvidenceSubmitted EvidenceState = "submitted"
	EvidenceAccepted  EvidenceState = "accepted"
	EvidenceRejected  EvidenceState = "rejected"
)

type ActorRole string

const (
	RoleCoordinator ActorRole = "coordinator"
	RoleWorker      ActorRole = "worker"
	RoleVerifier    ActorRole = "verifier"
)

type Actor struct {
	ID   string    `json:"id"`
	Role ActorRole `json:"role"`
}

type TerminalContract struct {
	Outcome            string   `json:"outcome"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
}

type Board struct {
	SchemaVersion int          `json:"schema_version"`
	ID            string       `json:"id"`
	Version       uint64       `json:"version"`
	Title         string       `json:"title"`
	Tasks         []Task       `json:"tasks"`
	Dependencies  []Dependency `json:"dependencies"`
	Attempts      []Attempt    `json:"attempts"`
	Leases        []Lease      `json:"leases"`
	Evidence      []Evidence   `json:"evidence"`
}

type Task struct {
	ID                 string           `json:"id"`
	Title              string           `json:"title"`
	State              TaskState        `json:"state"`
	TerminalContract   TerminalContract `json:"terminal_contract"`
	ActiveAttemptID    string           `json:"active_attempt_id,omitempty"`
	ActiveLeaseID      string           `json:"active_lease_id,omitempty"`
	AcceptedEvidenceID string           `json:"accepted_evidence_id,omitempty"`
}

type Dependency struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	DependsOn string `json:"depends_on"`
}

type Attempt struct {
	ID         string       `json:"id"`
	TaskID     string       `json:"task_id"`
	WorkerID   string       `json:"worker_id"`
	State      AttemptState `json:"state"`
	EvidenceID string       `json:"evidence_id,omitempty"`
}

type Lease struct {
	ID        string     `json:"id"`
	TaskID    string     `json:"task_id"`
	AttemptID string     `json:"attempt_id"`
	WorkerID  string     `json:"worker_id"`
	State     LeaseState `json:"state"`
}

type Evidence struct {
	ID        string        `json:"id"`
	TaskID    string        `json:"task_id"`
	AttemptID string        `json:"attempt_id"`
	WorkerID  string        `json:"worker_id"`
	Artifact  string        `json:"artifact"`
	Summary   string        `json:"summary"`
	State     EvidenceState `json:"state"`
}

type CommandType string

const (
	CommandBoardCreate    CommandType = "board.create"
	CommandTaskAdd        CommandType = "task.add"
	CommandDependencyAdd  CommandType = "dependency.add"
	CommandTaskActivate   CommandType = "task.activate"
	CommandLeaseAcquire   CommandType = "lease.acquire"
	CommandAttemptStart   CommandType = "attempt.start"
	CommandEvidenceSubmit CommandType = "evidence.submit"
	CommandEvidenceAccept CommandType = "evidence.accept"
	CommandEvidenceReject CommandType = "evidence.reject"
	CommandAttemptFail    CommandType = "attempt.fail"
)

type Command struct {
	SchemaVersion   int             `json:"schema_version"`
	ID              string          `json:"id"`
	ExpectedVersion uint64          `json:"expected_version"`
	Actor           Actor           `json:"actor"`
	Type            CommandType     `json:"type"`
	Board           *BoardSpec      `json:"board,omitempty"`
	Task            *TaskSpec       `json:"task,omitempty"`
	Dependency      *DependencySpec `json:"dependency,omitempty"`
	Lease           *LeaseSpec      `json:"lease,omitempty"`
	TaskID          string          `json:"task_id,omitempty"`
	AttemptID       string          `json:"attempt_id,omitempty"`
	Evidence        *EvidenceSpec   `json:"evidence,omitempty"`
	Reason          string          `json:"reason,omitempty"`
}

type BoardSpec struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type TaskSpec struct {
	ID               string           `json:"id"`
	Title            string           `json:"title"`
	TerminalContract TerminalContract `json:"terminal_contract"`
}

type DependencySpec struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	DependsOn string `json:"depends_on"`
}

type LeaseSpec struct {
	ID        string `json:"id"`
	AttemptID string `json:"attempt_id"`
	TaskID    string `json:"task_id"`
	WorkerID  string `json:"worker_id"`
}

type EvidenceSpec struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	AttemptID string `json:"attempt_id"`
	Artifact  string `json:"artifact"`
	Summary   string `json:"summary"`
}

type EventType string

const (
	EventBoardCreated        EventType = "board.created"
	EventTaskAdded           EventType = "task.added"
	EventDependencyAdded     EventType = "dependency.added"
	EventTaskStateChanged    EventType = "task.state-changed"
	EventLeaseAcquired       EventType = "lease.acquired"
	EventAttemptStateChanged EventType = "attempt.state-changed"
	EventEvidenceSubmitted   EventType = "evidence.submitted"
	EventEvidenceAccepted    EventType = "evidence.accepted"
	EventEvidenceRejected    EventType = "evidence.rejected"
)

type Event struct {
	SchemaVersion int         `json:"schema_version"`
	ID            string      `json:"id"`
	CommandID     string      `json:"command_id"`
	BoardID       string      `json:"board_id"`
	Version       uint64      `json:"version"`
	Actor         Actor       `json:"actor"`
	Type          EventType   `json:"type"`
	Task          *Task       `json:"task,omitempty"`
	Dependency    *Dependency `json:"dependency,omitempty"`
	Attempt       *Attempt    `json:"attempt,omitempty"`
	Lease         *Lease      `json:"lease,omitempty"`
	Evidence      *Evidence   `json:"evidence,omitempty"`
	PreviousState TaskState   `json:"previous_state,omitempty"`
	CurrentState  TaskState   `json:"current_state,omitempty"`
	Reason        string      `json:"reason,omitempty"`
}

func (board Board) Validate() error {
	if board.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported board schema version %d", board.SchemaVersion)
	}
	if board.ID == "" {
		return fmt.Errorf("board id is required")
	}
	if board.Title == "" {
		return fmt.Errorf("board title is required")
	}
	tasks := make(map[string]Task, len(board.Tasks))
	for _, task := range board.Tasks {
		if task.ID == "" || task.Title == "" {
			return fmt.Errorf("task id and title are required")
		}
		if _, exists := tasks[task.ID]; exists {
			return fmt.Errorf("duplicate task id %q", task.ID)
		}
		if err := task.TerminalContract.validate(); err != nil {
			return fmt.Errorf("task %q: %w", task.ID, err)
		}
		if !validTaskState(task.State) {
			return fmt.Errorf("task %q has invalid state %q", task.ID, task.State)
		}
		tasks[task.ID] = task
	}
	dependencies := make(map[string]struct{}, len(board.Dependencies))
	edges := make(map[string]struct{}, len(board.Dependencies))
	for _, dependency := range board.Dependencies {
		if dependency.ID == "" {
			return fmt.Errorf("dependency id is required")
		}
		if _, exists := dependencies[dependency.ID]; exists {
			return fmt.Errorf("duplicate dependency id %q", dependency.ID)
		}
		dependencies[dependency.ID] = struct{}{}
		if dependency.TaskID == dependency.DependsOn {
			return fmt.Errorf("dependency %q is a self-loop", dependency.ID)
		}
		if _, exists := tasks[dependency.TaskID]; !exists {
			return fmt.Errorf("dependency %q references missing task %q", dependency.ID, dependency.TaskID)
		}
		if _, exists := tasks[dependency.DependsOn]; !exists {
			return fmt.Errorf("dependency %q references missing prerequisite %q", dependency.ID, dependency.DependsOn)
		}
		edge := dependency.TaskID + "\x00" + dependency.DependsOn
		if _, exists := edges[edge]; exists {
			return fmt.Errorf("duplicate dependency edge from %q to %q", dependency.TaskID, dependency.DependsOn)
		}
		edges[edge] = struct{}{}
	}
	if err := validateAcyclic(tasks, board.Dependencies); err != nil {
		return err
	}
	if err := validateAttemptsLeasesEvidence(board, tasks); err != nil {
		return err
	}
	return nil
}

func (contract TerminalContract) validate() error {
	if contract.Outcome == "" || len(contract.AcceptanceCriteria) == 0 {
		return fmt.Errorf("terminal contract outcome and acceptance criteria are required")
	}
	for _, criterion := range contract.AcceptanceCriteria {
		if criterion == "" {
			return fmt.Errorf("terminal contract acceptance criteria cannot be empty")
		}
	}
	return nil
}

func validTaskState(state TaskState) bool {
	switch state {
	case TaskPlanned, TaskBlocked, TaskReady, TaskLeased, TaskRunning, TaskVerifying, TaskPassed, TaskFailed:
		return true
	default:
		return false
	}
}

func validateAcyclic(tasks map[string]Task, dependencies []Dependency) error {
	adjacent := make(map[string][]string, len(tasks))
	for _, dependency := range dependencies {
		adjacent[dependency.TaskID] = append(adjacent[dependency.TaskID], dependency.DependsOn)
	}
	visiting := make(map[string]bool, len(tasks))
	visited := make(map[string]bool, len(tasks))
	var visit func(string) error
	visit = func(taskID string) error {
		if visiting[taskID] {
			return fmt.Errorf("dependency graph contains a cycle at task %q", taskID)
		}
		if visited[taskID] {
			return nil
		}
		visiting[taskID] = true
		for _, prerequisite := range adjacent[taskID] {
			if err := visit(prerequisite); err != nil {
				return err
			}
		}
		visiting[taskID] = false
		visited[taskID] = true
		return nil
	}
	for taskID := range tasks {
		if err := visit(taskID); err != nil {
			return err
		}
	}
	return nil
}

func validateAttemptsLeasesEvidence(board Board, tasks map[string]Task) error {
	attempts := make(map[string]Attempt, len(board.Attempts))
	for _, attempt := range board.Attempts {
		if attempt.ID == "" || attempt.WorkerID == "" {
			return fmt.Errorf("attempt id and worker id are required")
		}
		if _, exists := attempts[attempt.ID]; exists {
			return fmt.Errorf("duplicate attempt id %q", attempt.ID)
		}
		if _, exists := tasks[attempt.TaskID]; !exists {
			return fmt.Errorf("attempt %q references missing task %q", attempt.ID, attempt.TaskID)
		}
		switch attempt.State {
		case AttemptLeased, AttemptRunning, AttemptVerifying, AttemptPassed, AttemptFailed:
		default:
			return fmt.Errorf("attempt %q has invalid state %q", attempt.ID, attempt.State)
		}
		attempts[attempt.ID] = attempt
	}
	leases := make(map[string]struct{}, len(board.Leases))
	for _, lease := range board.Leases {
		if lease.ID == "" || lease.WorkerID == "" {
			return fmt.Errorf("lease id and worker id are required")
		}
		if _, exists := leases[lease.ID]; exists {
			return fmt.Errorf("duplicate lease id %q", lease.ID)
		}
		leases[lease.ID] = struct{}{}
		attempt, exists := attempts[lease.AttemptID]
		if !exists || attempt.TaskID != lease.TaskID || attempt.WorkerID != lease.WorkerID {
			return fmt.Errorf("lease %q does not match its attempt", lease.ID)
		}
		if lease.State != LeaseActive && lease.State != LeaseCompleted {
			return fmt.Errorf("lease %q has invalid state %q", lease.ID, lease.State)
		}
	}
	evidence := make(map[string]struct{}, len(board.Evidence))
	for _, item := range board.Evidence {
		if item.ID == "" || item.Artifact == "" || item.Summary == "" {
			return fmt.Errorf("evidence id, artifact, and summary are required")
		}
		if _, exists := evidence[item.ID]; exists {
			return fmt.Errorf("duplicate evidence id %q", item.ID)
		}
		evidence[item.ID] = struct{}{}
		attempt, exists := attempts[item.AttemptID]
		if !exists || attempt.TaskID != item.TaskID || attempt.WorkerID != item.WorkerID {
			return fmt.Errorf("evidence %q does not match its attempt", item.ID)
		}
		if item.State != EvidenceSubmitted && item.State != EvidenceAccepted && item.State != EvidenceRejected {
			return fmt.Errorf("evidence %q has invalid state %q", item.ID, item.State)
		}
	}
	return nil
}
