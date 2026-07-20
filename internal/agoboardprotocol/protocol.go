// Package agoboardprotocol defines the versioned, persistence-neutral Work
// Graph contract. Storage and transport implementations must preserve these
// types and must apply commands through Apply rather than writing states.
package agoboardprotocol

import (
	"fmt"
	"time"
)

// SchemaVersion 3 adds structured evidence: changed files with before and after
// hashes, command and test records, artifact references, warnings, and the
// verifier's verdict. Boards persisted at an earlier version are upgraded by
// the store's migration, which is the only supported path forward.
const SchemaVersion = 3

// MaxAttempts bounds automatic retry. The state machine enforces it so no
// scheduler, however buggy, can create an extra attempt.
const MaxAttempts = 3

// MaxRetryDelay caps the backoff so a bounded retry stays observable.
const MaxRetryDelay = 30 * time.Second

// RetryDelay is the bounded exponential backoff before the given attempt number
// may start: min(2^attempt x 2s, 30s). It lives beside the attempt bound so the
// scheduler and the state machine cannot disagree about the policy.
func RetryDelay(attemptNumber int) time.Duration {
	if attemptNumber < 0 {
		attemptNumber = 0
	}
	// Shifting past the cap is pointless and would overflow on absurd input.
	if attemptNumber > 16 {
		return MaxRetryDelay
	}
	delay := time.Duration(1<<uint(attemptNumber)) * 2 * time.Second
	if delay > MaxRetryDelay {
		return MaxRetryDelay
	}
	return delay
}

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
	// TaskRetryWait is a bounded, durable backoff stop: the previous attempt
	// failed retryably and the task becomes eligible again only at
	// NextEligibleAt. It projects onto the Blocked column with a countdown.
	TaskRetryWait TaskState = "retry-wait"
)

// AccessMode declares whether a task needs to write the repository. The
// scheduler uses it to keep repository writers serialized.
type AccessMode string

const (
	AccessRead  AccessMode = "read"
	AccessWrite AccessMode = "write"
)

// FailureClass records why an attempt ended, and therefore whether the task may
// be retried. Classification is durable so a restarted scheduler reaches the
// same decision.
type FailureClass string

const (
	FailureNone FailureClass = ""
	// Retryable classes.
	FailureTransient        FailureClass = "transient"
	FailureVerifierFeedback FailureClass = "verifier-feedback"
	// Terminal classes: retrying cannot fix them without new user input.
	FailureAuth       FailureClass = "auth"
	FailurePolicy     FailureClass = "policy"
	FailureNeedsInput FailureClass = "needs-input"
	FailureRepository FailureClass = "repository"
	FailurePermanent  FailureClass = "permanent"
	FailureExhausted  FailureClass = "exhausted"
)

// Retryable reports whether a failure class may produce a further attempt.
// Unknown classes are treated as terminal so an unclassified failure fails
// closed rather than retrying forever.
func (class FailureClass) Retryable() bool {
	switch class {
	case FailureTransient, FailureVerifierFeedback:
		return true
	default:
		return false
	}
}

func validFailureClass(class FailureClass) bool {
	switch class {
	case FailureNone, FailureTransient, FailureVerifierFeedback,
		FailureAuth, FailurePolicy, FailureNeedsInput, FailureRepository, FailurePermanent, FailureExhausted:
		return true
	default:
		return false
	}
}

func validAccessMode(mode AccessMode) bool {
	return mode == AccessRead || mode == AccessWrite
}

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
	// Repository identifies the single local repository this board's work
	// touches. The scheduler serializes writers per repository.
	Repository string `json:"repository,omitempty"`
	// NextGeneration is the board's monotonic fencing counter. It only ever
	// increases, so a token minted for a superseded attempt can never be
	// reissued.
	NextGeneration uint64 `json:"next_generation"`
	// Paused stops new claims without cancelling running attempts.
	Paused      bool   `json:"paused"`
	PauseReason string `json:"pause_reason,omitempty"`
}

type Task struct {
	ID                 string           `json:"id"`
	Title              string           `json:"title"`
	State              TaskState        `json:"state"`
	TerminalContract   TerminalContract `json:"terminal_contract"`
	ActiveAttemptID    string           `json:"active_attempt_id,omitempty"`
	ActiveLeaseID      string           `json:"active_lease_id,omitempty"`
	AcceptedEvidenceID string           `json:"accepted_evidence_id,omitempty"`
	// AccessMode decides which repository concurrency slot this task consumes.
	AccessMode AccessMode `json:"access_mode"`
	// AttemptCount counts attempts in the current retry budget. It is the
	// durable bound the state machine enforces against automatic retry.
	AttemptCount int `json:"attempt_count"`
	// UserRetries counts how many times a person restarted this task after it
	// stopped. Attempt history is never discarded, so a reader can always see
	// what happened before each decision.
	UserRetries int `json:"user_retries,omitempty"`
	// NextEligibleAt gates a retry-wait task. A zero value means "no wait".
	NextEligibleAt time.Time    `json:"next_eligible_at,omitempty"`
	FailureClass   FailureClass `json:"failure_class,omitempty"`
	BlockedReason  string       `json:"blocked_reason,omitempty"`
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
	// Number is this attempt's 1-based position for its task.
	Number int `json:"number"`
	// Generation and FencingToken bind every later message from the executor
	// to exactly this attempt. A superseded attempt keeps its values so late
	// traffic can be recognised and rejected rather than silently applied.
	Generation    uint64       `json:"generation"`
	FencingToken  string       `json:"fencing_token,omitempty"`
	FailureClass  FailureClass `json:"failure_class,omitempty"`
	FailureReason string       `json:"failure_reason,omitempty"`
}

type Lease struct {
	ID        string     `json:"id"`
	TaskID    string     `json:"task_id"`
	AttemptID string     `json:"attempt_id"`
	WorkerID  string     `json:"worker_id"`
	State     LeaseState `json:"state"`
	// Generation and FencingToken mirror the attempt they were acquired for.
	Generation   uint64 `json:"generation"`
	FencingToken string `json:"fencing_token,omitempty"`
	// AcquiredAt and ExpiresAt are durable so a restarted scheduler reconciles
	// from SQLite rather than from process memory. A zero ExpiresAt means the
	// lease has no deadline.
	AcquiredAt time.Time `json:"acquired_at,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
}

// ChangedFile records one repository modification. Paths are
// repository-relative; an absolute path or a parent reference is rejected.
type ChangedFile struct {
	Path       string `json:"path"`
	BeforeHash string `json:"before_hash,omitempty"`
	AfterHash  string `json:"after_hash,omitempty"`
}

// CommandRecord is one command an executor ran. Output is not inlined: it is
// referenced as a bounded artifact so evidence cannot grow without limit.
type CommandRecord struct {
	Display          string `json:"display"`
	ExitCode         int    `json:"exit_code"`
	DurationMS       int64  `json:"duration_ms"`
	OutputArtifactID string `json:"output_artifact_id,omitempty"`
}

// TestRecord is a deterministic check. A required test that did not pass is a
// hard stop: no model verdict may accept over it.
type TestRecord struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
	Required bool   `json:"required"`
}

// ArtifactRef points at bytes held in the managed artifact store. The size and
// digest are recorded so a later read can prove the bytes did not change.
type ArtifactRef struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	Bytes       int64  `json:"bytes"`
	SHA256      string `json:"sha256"`
}

// EvidenceResult is what an attempt actually produced. It carries no
// credential: the fencing token that authorized the attempt is referenced only
// by generation, never by value.
type EvidenceResult struct {
	Summary      string          `json:"summary"`
	Generation   uint64          `json:"generation"`
	ChangedFiles []ChangedFile   `json:"changed_files,omitempty"`
	Commands     []CommandRecord `json:"commands,omitempty"`
	Tests        []TestRecord    `json:"tests,omitempty"`
	Artifacts    []ArtifactRef   `json:"artifacts,omitempty"`
	Warnings     []string        `json:"warnings,omitempty"`
}

// RequiredTestsPassed reports whether every required check succeeded. It is the
// deterministic gate that outranks a model's judgement.
func (result EvidenceResult) RequiredTestsPassed() bool {
	for _, test := range result.Tests {
		if test.Required && !test.Passed {
			return false
		}
	}
	return true
}

// FailedRequiredTests names the checks blocking acceptance.
func (result EvidenceResult) FailedRequiredTests() []string {
	var failed []string
	for _, test := range result.Tests {
		if test.Required && !test.Passed {
			failed = append(failed, test.Name)
		}
	}
	return failed
}

type Evidence struct {
	ID        string        `json:"id"`
	TaskID    string        `json:"task_id"`
	AttemptID string        `json:"attempt_id"`
	WorkerID  string        `json:"worker_id"`
	Artifact  string        `json:"artifact"`
	Summary   string        `json:"summary"`
	State     EvidenceState `json:"state"`
	// Result is the structured record a user inspects to see why work was
	// accepted. Verdict and VerdictReason are the independent decision.
	Result        EvidenceResult `json:"result,omitempty"`
	Verdict       string         `json:"verdict,omitempty"`
	VerdictReason string         `json:"verdict_reason,omitempty"`
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
	// CommandLeaseExpire is the coordinator's reconciliation of a lease whose
	// deadline elapsed or whose worker is unreachable. It supersedes the
	// attempt so late traffic from it can no longer authenticate.
	CommandLeaseExpire CommandType = "lease.expire"
	// CommandLeaseRenew extends an active lease's deadline. Only the current
	// generation may renew, so a superseded worker cannot keep a lease alive.
	CommandLeaseRenew  CommandType = "lease.renew"
	CommandBoardPause  CommandType = "board.pause"
	CommandBoardResume CommandType = "board.resume"
	// CommandTaskRetry is a human decision to try a stopped task again. The
	// automatic attempt bound protects against machine retry loops; a person
	// choosing to retry is a different act, and it is audited as one.
	CommandTaskRetry CommandType = "task.retry"
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
	// FencingToken authenticates a worker or verifier against the exact attempt
	// it is acting on. Commands from a superseded attempt cannot match.
	FencingToken string `json:"fencing_token,omitempty"`
	// FailureClass and NextEligibleAt carry the scheduler's durable retry
	// decision. The state machine still enforces the attempt bound itself.
	FailureClass   FailureClass `json:"failure_class,omitempty"`
	NextEligibleAt time.Time    `json:"next_eligible_at,omitempty"`
}

type BoardSpec struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Repository string `json:"repository,omitempty"`
}

type TaskSpec struct {
	ID               string           `json:"id"`
	Title            string           `json:"title"`
	TerminalContract TerminalContract `json:"terminal_contract"`
	AccessMode       AccessMode       `json:"access_mode"`
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
	// FencingToken must be unpredictable and never reused. The state machine
	// rejects a token already present on the board.
	FencingToken string `json:"fencing_token"`
	// AcquiredAt is the scheduler's clock reading for this claim. It is the
	// value the state machine compares against a retry-wait deadline, so
	// backoff is enforced by the protocol rather than by scheduler discipline.
	AcquiredAt time.Time `json:"acquired_at"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
}

type EvidenceSpec struct {
	ID        string         `json:"id"`
	TaskID    string         `json:"task_id"`
	AttemptID string         `json:"attempt_id"`
	Artifact  string         `json:"artifact"`
	Summary   string         `json:"summary"`
	Result    EvidenceResult `json:"result,omitempty"`
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
	EventLeaseExpired        EventType = "lease.expired"
	EventLeaseRenewed        EventType = "lease.renewed"
	EventBoardPaused         EventType = "board.paused"
	EventBoardResumed        EventType = "board.resumed"
	EventTaskRetryRequested  EventType = "task.retry-requested"
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
		if !validAccessMode(task.AccessMode) {
			return fmt.Errorf("task %q has invalid access mode %q", task.ID, task.AccessMode)
		}
		if !validFailureClass(task.FailureClass) {
			return fmt.Errorf("task %q has invalid failure class %q", task.ID, task.FailureClass)
		}
		// Only the lower bound is a structural invariant. The attempt ceiling is
		// enforced by Apply when a lease is acquired, so a board migrated from
		// schema 1 with more historical attempts stays readable instead of
		// forcing the migration to rewrite history to a legal-looking number.
		if task.AttemptCount < 0 {
			return fmt.Errorf("task %q has a negative attempt count", task.ID)
		}
		if task.State == TaskRetryWait && task.NextEligibleAt.IsZero() {
			return fmt.Errorf("task %q is waiting to retry without a next eligible time", task.ID)
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
	if board.Paused && board.PauseReason == "" {
		return fmt.Errorf("a paused board requires a reason")
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
	case TaskPlanned, TaskBlocked, TaskReady, TaskLeased, TaskRunning, TaskVerifying, TaskPassed, TaskFailed, TaskRetryWait:
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
		if !validFailureClass(attempt.FailureClass) {
			return fmt.Errorf("attempt %q has invalid failure class %q", attempt.ID, attempt.FailureClass)
		}
		// A token always implies a generation. The converse is not required:
		// attempts migrated from schema 1 carry neither, which is what denies
		// a historical executor any fencing authority.
		if attempt.FencingToken != "" && attempt.Generation == 0 {
			return fmt.Errorf("attempt %q has a fencing token without a generation", attempt.ID)
		}
		if attempt.Generation >= board.NextGeneration && attempt.Generation != 0 {
			return fmt.Errorf("attempt %q generation %d is not below the board's next generation %d", attempt.ID, attempt.Generation, board.NextGeneration)
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
		// The lease and its attempt must agree on fencing identity, otherwise a
		// message could authenticate against one and act on the other.
		if lease.Generation != attempt.Generation || lease.FencingToken != attempt.FencingToken {
			return fmt.Errorf("lease %q fencing identity does not match its attempt", lease.ID)
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
