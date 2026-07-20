// Package agoboardruntime connects objective planning, the durable Work Graph,
// replaceable execution, and independent evidence review.
package agoboardruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agoplanner"
)

type Goal struct {
	BoardID      string                   `json:"board_id"`
	Repository   agoplanner.Repository    `json:"repository"`
	Objective    agoplanner.Objective     `json:"objective"`
	ProjectGates []agoplanner.ProjectGate `json:"project_gates"`
	Constraints  agoplanner.Constraints   `json:"constraints"`
	// ExecutionMode records which replaceable executor family the goal was
	// admitted for. It is part of the durable definition so a restarted process
	// projects the same goal without re-asking the user.
	ExecutionMode string `json:"execution_mode,omitempty"`
}

type Dispatch struct {
	Goal      Goal                    `json:"goal"`
	Task      agoplanner.TaskProposal `json:"task"`
	AttemptID string                  `json:"attempt_id"`
	WorkerID  string                  `json:"worker_id"`
}

type ExecutionResult struct {
	Artifact string `json:"artifact"`
	Summary  string `json:"summary"`
}

type Review struct {
	Accepted bool   `json:"accepted"`
	Reason   string `json:"reason"`
	// FailureClass lets a verifier distinguish actionable feedback from a
	// terminal stop. An empty value on a rejection means retryable feedback.
	FailureClass agoboardprotocol.FailureClass `json:"failure_class,omitempty"`
}

type Executor interface {
	Execute(context.Context, Dispatch) (ExecutionResult, error)
}

type Verifier interface {
	Verify(context.Context, Dispatch, ExecutionResult) (Review, error)
}

type Options struct {
	CoordinatorID string
	WorkerID      string
	VerifierID    string
	LeaseDuration time.Duration
	Now           func() time.Time
}

type Column string

const (
	ColumnBacklog Column = "Backlog"
	ColumnReady   Column = "Ready"
	ColumnClaimed Column = "Claimed"
	ColumnRunning Column = "Running"
	ColumnReview  Column = "Review"
	ColumnBlocked Column = "Blocked"
	ColumnDone    Column = "Done"
)

var boardColumns = []Column{
	ColumnBacklog, ColumnReady, ColumnClaimed, ColumnRunning, ColumnReview, ColumnBlocked, ColumnDone,
}

type BoardTask struct {
	ID    string                     `json:"id"`
	Title string                     `json:"title"`
	State agoboardprotocol.TaskState `json:"state"`
}

type BoardColumn struct {
	Name  Column      `json:"name"`
	Tasks []BoardTask `json:"tasks"`
}

type BoardView struct {
	BoardID string        `json:"board_id"`
	Version uint64        `json:"version"`
	Columns []BoardColumn `json:"columns"`
}

type project struct {
	Goal Goal            `json:"goal"`
	Plan agoplanner.Plan `json:"plan"`
}

type Runtime struct {
	store   *agoboardstore.Store
	planner agoplanner.Planner
	options Options

	mu       sync.RWMutex
	projects map[string]project
}

// New builds the goal-admission runtime. It takes no executor or verifier:
// running work belongs to internal/agoscheduler, and holding them here would
// invite a second dispatch path.
func New(store *agoboardstore.Store, planner agoplanner.Planner, options Options) *Runtime {
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Runtime{store: store, planner: planner, options: options, projects: make(map[string]project)}
}

func (runtime *Runtime) Create(ctx context.Context, goal Goal) (BoardView, error) {
	if err := runtime.validate(); err != nil {
		return BoardView{}, err
	}
	if strings.TrimSpace(goal.BoardID) == "" || strings.TrimSpace(goal.Objective.Summary) == "" {
		return BoardView{}, fmt.Errorf("board id and natural-language objective are required")
	}
	request := agoplanner.Request{
		Repository: goal.Repository, Objective: goal.Objective,
		ProjectGates: goal.ProjectGates, Constraints: goal.Constraints,
	}
	plan, err := runtime.planner.Plan(ctx, request)
	if err != nil {
		return BoardView{}, fmt.Errorf("plan objective: %w", err)
	}
	if err := plan.Validate(request); err != nil {
		return BoardView{}, fmt.Errorf("validate objective plan: %w", err)
	}
	actor := runtime.coordinator()
	commands := []agoboardprotocol.Command{{
		SchemaVersion: agoboardprotocol.SchemaVersion,
		ID:            stableID("command", goal.BoardID, "create"), Actor: actor,
		Type:  agoboardprotocol.CommandBoardCreate,
		Board: &agoboardprotocol.BoardSpec{ID: goal.BoardID, Title: goal.Objective.Summary, Repository: goal.Repository.ID},
	}}
	version := uint64(1)
	for _, proposal := range plan.Tasks {
		commands = append(commands, agoboardprotocol.Command{
			SchemaVersion: agoboardprotocol.SchemaVersion,
			ID:            stableID("command", goal.BoardID, "add-task", proposal.ID), ExpectedVersion: version,
			Actor: actor, Type: agoboardprotocol.CommandTaskAdd,
			Task: &agoboardprotocol.TaskSpec{
				ID: proposal.ID, Title: proposal.Title, AccessMode: accessModeFor(proposal),
				TerminalContract: agoboardprotocol.TerminalContract{Outcome: proposal.Description, AcceptanceCriteria: append([]string(nil), proposal.AcceptanceCriteria...)},
			},
		})
		version++
	}
	for _, dependency := range plan.Dependencies {
		commands = append(commands, agoboardprotocol.Command{
			SchemaVersion: agoboardprotocol.SchemaVersion,
			ID:            stableID("command", goal.BoardID, "add-dependency", dependency.TaskID, dependency.DependsOn), ExpectedVersion: version,
			Actor: actor, Type: agoboardprotocol.CommandDependencyAdd,
			Dependency: &agoboardprotocol.DependencySpec{
				ID:     stableID("dependency", goal.BoardID, dependency.TaskID, dependency.DependsOn),
				TaskID: dependency.TaskID, DependsOn: dependency.DependsOn,
			},
		})
		version++
	}
	for _, proposal := range plan.Tasks {
		commands = append(commands, agoboardprotocol.Command{
			SchemaVersion: agoboardprotocol.SchemaVersion,
			ID:            stableID("command", goal.BoardID, "activate", proposal.ID), ExpectedVersion: version,
			Actor: actor, Type: agoboardprotocol.CommandTaskActivate, TaskID: proposal.ID,
		})
		version++
	}
	definition := project{Goal: cloneGoal(goal), Plan: clonePlan(plan)}
	encodedDefinition, err := json.Marshal(definition)
	if err != nil {
		return BoardView{}, fmt.Errorf("encode graph definition: %w", err)
	}
	result, err := runtime.store.CreateGraph(ctx, commands, encodedDefinition)
	if err != nil {
		return BoardView{}, fmt.Errorf("admit graph: %w", err)
	}
	runtime.mu.Lock()
	runtime.projects[goal.BoardID] = definition
	runtime.mu.Unlock()
	return projectBoard(result.Board), nil
}

// Scheduling lives in internal/agoscheduler. The runtime deliberately exposes
// no claim or dispatch entry point: a second scheduling authority here is how
// duplicate dispatch and unbounded retry get reintroduced.

// Definition returns the immutable goal and plan admitted with a board,
// recovering them from the durable store when this process did not create it.
// Callers receive defensive copies and cannot mutate scheduling state through
// the returned values.
func (runtime *Runtime) Definition(ctx context.Context, boardID string) (Goal, agoplanner.Plan, error) {
	definition, ok := runtime.project(boardID)
	if !ok {
		if err := runtime.store.Definition(ctx, boardID, &definition); err != nil {
			return Goal{}, agoplanner.Plan{}, fmt.Errorf("recover board definition: %w", err)
		}
		runtime.mu.Lock()
		runtime.projects[boardID] = definition
		runtime.mu.Unlock()
	}
	return cloneGoal(definition.Goal), clonePlan(definition.Plan), nil
}

func (runtime *Runtime) View(ctx context.Context, boardID string) (BoardView, error) {
	board, err := runtime.store.Board(ctx, boardID)
	if err != nil {
		return BoardView{}, err
	}
	return projectBoard(board), nil
}

func (runtime *Runtime) Completion(ctx context.Context, boardID string) (agoboardstore.Completion, error) {
	return runtime.store.Completion(ctx, boardID)
}

func (runtime *Runtime) validate() error {
	if runtime == nil || runtime.store == nil || runtime.planner == nil {
		return fmt.Errorf("board store and planner are required")
	}
	if strings.TrimSpace(runtime.options.CoordinatorID) == "" || strings.TrimSpace(runtime.options.WorkerID) == "" || strings.TrimSpace(runtime.options.VerifierID) == "" {
		return fmt.Errorf("coordinator, worker, and verifier identities are required")
	}
	if runtime.options.LeaseDuration <= 0 || runtime.options.Now == nil {
		return fmt.Errorf("positive lease duration and clock are required")
	}
	return nil
}

func (runtime *Runtime) project(boardID string) (project, bool) {
	runtime.mu.RLock()
	defer runtime.mu.RUnlock()
	value, ok := runtime.projects[boardID]
	return value, ok
}

func (runtime *Runtime) coordinator() agoboardprotocol.Actor {
	return agoboardprotocol.Actor{ID: runtime.options.CoordinatorID, Role: agoboardprotocol.RoleCoordinator}
}
func (runtime *Runtime) worker() agoboardprotocol.Actor {
	return agoboardprotocol.Actor{ID: runtime.options.WorkerID, Role: agoboardprotocol.RoleWorker}
}
func (runtime *Runtime) verifierActor() agoboardprotocol.Actor {
	return agoboardprotocol.Actor{ID: runtime.options.VerifierID, Role: agoboardprotocol.RoleVerifier}
}

func projectBoard(board agoboardprotocol.Board) BoardView {
	view := BoardView{BoardID: board.ID, Version: board.Version, Columns: make([]BoardColumn, len(boardColumns))}
	indexes := make(map[Column]int, len(boardColumns))
	for index, name := range boardColumns {
		view.Columns[index] = BoardColumn{Name: name, Tasks: []BoardTask{}}
		indexes[name] = index
	}
	for _, task := range board.Tasks {
		column := columnForState(task.State)
		index := indexes[column]
		view.Columns[index].Tasks = append(view.Columns[index].Tasks, BoardTask{ID: task.ID, Title: task.Title, State: task.State})
	}
	return view
}

func columnForState(state agoboardprotocol.TaskState) Column {
	switch state {
	case agoboardprotocol.TaskPlanned:
		return ColumnBacklog
	case agoboardprotocol.TaskReady:
		return ColumnReady
	case agoboardprotocol.TaskLeased:
		return ColumnClaimed
	case agoboardprotocol.TaskRunning:
		return ColumnRunning
	case agoboardprotocol.TaskVerifying:
		return ColumnReview
	case agoboardprotocol.TaskPassed:
		return ColumnDone
	case agoboardprotocol.TaskBlocked, agoboardprotocol.TaskFailed:
		return ColumnBlocked
	default:
		return ColumnBlocked
	}
}

func stableID(namespace string, parts ...string) string {
	encoded, _ := json.Marshal(parts)
	digest := sha256.Sum256(encoded)
	return namespace + ":" + hex.EncodeToString(digest[:16])
}

func cloneGoal(goal Goal) Goal {
	goal.ProjectGates = clonePlan(agoplanner.Plan{ProjectGates: goal.ProjectGates}).ProjectGates
	goal.Constraints.PathScopes = append([]string(nil), goal.Constraints.PathScopes...)
	goal.Constraints.CapabilityTags = append([]string(nil), goal.Constraints.CapabilityTags...)
	goal.Constraints.VerifierIDs = append([]string(nil), goal.Constraints.VerifierIDs...)
	return goal
}

func cloneTask(task agoplanner.TaskProposal) agoplanner.TaskProposal {
	plan := clonePlan(agoplanner.Plan{Tasks: []agoplanner.TaskProposal{task}})
	return plan.Tasks[0]
}

func clonePlan(plan agoplanner.Plan) agoplanner.Plan {
	encoded, _ := json.Marshal(plan)
	var cloned agoplanner.Plan
	_ = json.Unmarshal(encoded, &cloned)
	return cloned
}

// accessModeFor derives a task's repository access mode from its declared
// capabilities. Anything that can write the repository is serialized, so an
// unrecognised capability set is treated as a writer rather than assumed safe.
func accessModeFor(proposal agoplanner.TaskProposal) agoboardprotocol.AccessMode {
	for _, tag := range proposal.CapabilityTags {
		switch tag {
		case "repo-write", "write", "shell":
			return agoboardprotocol.AccessWrite
		}
	}
	return agoboardprotocol.AccessRead
}
