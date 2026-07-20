// Package agoboardapi exposes the local Goal and Board HTTP boundary over the
// durable Work Graph.
//
// The boundary is deliberately thin: handlers decode product input, issue
// protocol commands through the runtime and store, and project durable state
// back out. No handler writes a task, attempt, lease, or evidence row, so the
// SQLite Work Graph stays the single source of scheduling truth.
//
// Provider credentials never enter this package. Provider descriptors are
// supplied by the process that owns configuration and carry only a boolean
// stating whether authentication is present.
package agoboardapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"claudexflow/internal/agoartifact"
	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agoplanner"
)

const (
	maxRequestBytes = 1 << 20

	// ExecutionModeFake is the deterministic offline executor family. It is the
	// only mode admitted until the Claude Code provider lands.
	ExecutionModeFake = "fake"

	defaultRevision     = "HEAD"
	defaultPollInterval = 250 * time.Millisecond
)

// Provider describes one configured capability provider without exposing any
// credential value.
type Provider struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Model is the mapping this role uses. It is non-secret metadata; the base
	// URL is deliberately absent because it can itself carry a credential.
	Model          string   `json:"model,omitempty"`
	Capabilities   []string `json:"capabilities"`
	AuthConfigured bool     `json:"auth_configured"`
}

type Options struct {
	Runtime *agoboardruntime.Runtime
	Store   *agoboardstore.Store
	// Providers is the non-secret capability roster reported to clients.
	Providers []Provider
	// Decisions reports what the autonomous supervisor could not decide alone.
	// Nil means no supervisor is running, which is reported as an empty queue
	// rather than an error: a board with no supervisor simply needs nothing.
	Decisions DecisionSource
	// Integration establishes the Ago-owned ref a board's accepted work is
	// promoted onto. Without it a write task can never complete, so a board
	// created with no integration setup can only run read-only work.
	Integration IntegrationSetup
	// Artifacts serves managed executor output. When nil, the download route
	// reports that artifact storage is unavailable rather than guessing a path.
	Artifacts *agoartifact.Store
	// PollInterval bounds how long an idle event stream waits before it
	// re-reads SQLite. The in-memory notifier only makes delivery prompt; it is
	// never the authority for what a subscriber receives.
	PollInterval time.Duration
}

type Server struct {
	runtime      *agoboardruntime.Runtime
	store        *agoboardstore.Store
	providers    []Provider
	decisions    DecisionSource
	integration  IntegrationSetup
	artifacts    *agoartifact.Store
	pollInterval time.Duration

	mu      sync.Mutex
	waiters map[string]chan struct{}
}

func New(options Options) (*Server, error) {
	if options.Runtime == nil || options.Store == nil {
		return nil, fmt.Errorf("board runtime and store are required")
	}
	if options.PollInterval <= 0 {
		options.PollInterval = defaultPollInterval
	}
	return &Server{
		runtime:      options.Runtime,
		store:        options.Store,
		providers:    append([]Provider(nil), options.Providers...),
		decisions:    options.Decisions,
		integration:  options.Integration,
		artifacts:    options.Artifacts,
		pollInterval: options.PollInterval,
		waiters:      make(map[string]chan struct{}),
	}, nil
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/goals", server.createGoal)
	mux.HandleFunc("GET /api/v1/boards/{boardID}", server.boardSnapshot)
	mux.HandleFunc("GET /api/v1/boards/{boardID}/tasks/{taskID}", server.taskDetail)
	mux.HandleFunc("POST /api/v1/boards/{boardID}/pause", server.setPaused(true))
	mux.HandleFunc("POST /api/v1/boards/{boardID}/resume", server.setPaused(false))
	mux.HandleFunc("GET /api/v1/boards/{boardID}/events", server.events)
	mux.HandleFunc("GET /api/v1/providers", server.listProviders)
	mux.HandleFunc("GET /api/v1/boards/{boardID}/artifacts/{artifactID}", server.downloadArtifact)
	mux.HandleFunc("POST /api/v1/boards/{boardID}/tasks/{taskID}/retry", server.retryTask)
	mux.HandleFunc("GET /api/v1/boards/{boardID}/decisions", server.listDecisions)
	return mux
}

// -- wire types --------------------------------------------------------------

type repositoryRequest struct {
	Root     string `json:"root"`
	Revision string `json:"revision"`
}

type goalRequest struct {
	CommandID     string            `json:"command_id"`
	Objective     string            `json:"objective"`
	Repository    repositoryRequest `json:"repository"`
	ExecutionMode string            `json:"execution_mode"`
}

type goalResponse struct {
	Replayed  bool     `json:"replayed"`
	CommandID string   `json:"command_id"`
	Board     Snapshot `json:"board"`
}

type controlRequest struct {
	CommandID string `json:"command_id"`
	Reason    string `json:"reason"`
}

type SnapshotGoal struct {
	Objective     string            `json:"objective"`
	Repository    repositoryRequest `json:"repository"`
	ExecutionMode string            `json:"execution_mode"`
}

type SnapshotTask struct {
	ID        string                     `json:"id"`
	Title     string                     `json:"title"`
	State     agoboardprotocol.TaskState `json:"state"`
	DependsOn []string                   `json:"depends_on"`
}

type SnapshotColumn struct {
	Name  agoboardruntime.Column `json:"name"`
	Tasks []SnapshotTask         `json:"tasks"`
}

type SnapshotDependency struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	DependsOn string `json:"depends_on"`
}

type SnapshotProgress struct {
	Status    agoboardstore.CompletionStatus `json:"status"`
	Passed    int                            `json:"passed"`
	Failed    int                            `json:"failed"`
	Remaining int                            `json:"remaining"`
	Total     int                            `json:"total"`
}

type Snapshot struct {
	BoardID             string               `json:"board_id"`
	Title               string               `json:"title"`
	Version             uint64               `json:"version"`
	GraphVersion        uint64               `json:"graph_version"`
	LatestEventSequence uint64               `json:"latest_event_sequence"`
	Goal                SnapshotGoal         `json:"goal"`
	Columns             []SnapshotColumn     `json:"columns"`
	Dependencies        []SnapshotDependency `json:"dependencies"`
	Progress            SnapshotProgress     `json:"progress"`
	Paused              bool                 `json:"paused"`
	Completed           bool                 `json:"completed"`
}

type TaskAttempt struct {
	ID       string                        `json:"id"`
	Number   int                           `json:"number"`
	State    agoboardprotocol.AttemptState `json:"state"`
	WorkerID string                        `json:"worker_id"`
	// Generation orders attempts; the fencing token itself is never exposed,
	// because it is the credential that authorizes changing this task.
	Generation    uint64                        `json:"generation"`
	EvidenceID    string                        `json:"evidence_id"`
	FailureClass  agoboardprotocol.FailureClass `json:"failure_class,omitempty"`
	FailureReason string                        `json:"failure_reason,omitempty"`
}

type TaskLease struct {
	ID         string                      `json:"id"`
	AttemptID  string                      `json:"attempt_id"`
	WorkerID   string                      `json:"worker_id"`
	State      agoboardprotocol.LeaseState `json:"state"`
	Generation uint64                      `json:"generation"`
	AcquiredAt time.Time                   `json:"acquired_at,omitempty"`
	ExpiresAt  time.Time                   `json:"expires_at,omitempty"`
}

type TaskEvidence struct {
	ID        string                         `json:"id"`
	AttemptID string                         `json:"attempt_id"`
	WorkerID  string                         `json:"worker_id"`
	Artifact  string                         `json:"artifact"`
	Summary   string                         `json:"summary"`
	State     agoboardprotocol.EvidenceState `json:"state"`
	// Result is the structured record a user inspects: which files changed,
	// what ran, which checks were required, and what was produced.
	Result        agoboardprotocol.EvidenceResult `json:"result"`
	Verdict       string                          `json:"verdict,omitempty"`
	VerdictReason string                          `json:"verdict_reason,omitempty"`
}

type TaskDetail struct {
	BoardID            string                            `json:"board_id"`
	TaskID             string                            `json:"task_id"`
	Title              string                            `json:"title"`
	State              agoboardprotocol.TaskState        `json:"state"`
	Column             agoboardruntime.Column            `json:"column"`
	TerminalContract   agoboardprotocol.TerminalContract `json:"terminal_contract"`
	DependsOn          []string                          `json:"depends_on"`
	RequiredBy         []string                          `json:"required_by"`
	PathScopes         []string                          `json:"path_scopes"`
	CapabilityTags     []string                          `json:"capability_tags"`
	VerifierIDs        []string                          `json:"verifier_ids"`
	ActiveAttemptID    string                            `json:"active_attempt_id"`
	AcceptedEvidenceID string                            `json:"accepted_evidence_id"`
	AccessMode         agoboardprotocol.AccessMode       `json:"access_mode"`
	// Retry accounting, so a user can see why work is waiting and for how long.
	AttemptCount   int                           `json:"attempt_count"`
	MaxAttempts    int                           `json:"max_attempts"`
	NextEligibleAt time.Time                     `json:"next_eligible_at,omitempty"`
	FailureClass   agoboardprotocol.FailureClass `json:"failure_class,omitempty"`
	BlockedReason  string                        `json:"blocked_reason,omitempty"`
	Attempts       []TaskAttempt                 `json:"attempts"`
	Leases         []TaskLease                   `json:"leases"`
	Evidence       []TaskEvidence                `json:"evidence"`
}

// -- handlers ----------------------------------------------------------------

func (server *Server) createGoal(writer http.ResponseWriter, request *http.Request) {
	var input goalRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	goal, err := server.buildGoal(input)
	if err != nil {
		writeError(writer, err)
		return
	}
	// Establish where this board's accepted work will be promoted, before any
	// task can produce a change. Ago writes only its own ref; the user's branch
	// is never involved.
	if server.integration != nil {
		ref := server.integration.RefName(goal.BoardID)
		base, refErr := server.integration.EnsureRef(request.Context(), goal.Repository.ID, ref, "")
		if refErr != nil {
			writeError(writer, statusError{http.StatusBadRequest, "repository_unavailable",
				"无法在该仓库中建立 Ago 集成分支，请确认它是一个 git 仓库。", refErr})
			return
		}
		goal.BaseRevision, goal.IntegrationRef = base, ref
	}
	// The board identity is derived from the command ID, so an already-present
	// board means this exact command was admitted before. Two concurrent
	// identical requests may both report 201; the durable store still admits
	// exactly one board, which is the invariant that matters.
	_, existing := server.store.Board(request.Context(), goal.BoardID)
	replayed := existing == nil
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	if _, createErr := server.runtime.Create(request.Context(), goal); createErr != nil {
		// The response stays generic so nothing internal leaks, but an operator
		// needs to know why a goal was refused.
		log.Printf("goal %q rejected: %v", input.CommandID, createErr)
		// An exact replay is absorbed by the store's command receipt, so a
		// conflict here can only mean the command ID was reused with different
		// content.
		if errors.Is(createErr, agoboardstore.ErrCommandConflict) {
			writeError(writer, statusError{http.StatusConflict, "command_conflict", "该命令 ID 已用于不同的目标内容，请换一个命令 ID。", createErr})
			return
		}
		writeError(writer, statusError{http.StatusBadRequest, "goal_rejected", "无法为该目标创建工作图。", createErr})
		return
	}
	snapshot, err := server.snapshot(request.Context(), goal.BoardID)
	if err != nil {
		writeError(writer, err)
		return
	}
	server.notify(goal.BoardID)
	writeJSON(writer, status, goalResponse{Replayed: replayed, CommandID: input.CommandID, Board: snapshot})
}

func (server *Server) boardSnapshot(writer http.ResponseWriter, request *http.Request) {
	snapshot, err := server.snapshot(request.Context(), request.PathValue("boardID"))
	if err != nil {
		writeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, snapshot)
}

func (server *Server) taskDetail(writer http.ResponseWriter, request *http.Request) {
	boardID, taskID := request.PathValue("boardID"), request.PathValue("taskID")
	board, err := server.store.Board(request.Context(), boardID)
	if err != nil {
		writeError(writer, err)
		return
	}
	var task agoboardprotocol.Task
	found := false
	for _, candidate := range board.Tasks {
		if candidate.ID == taskID {
			task, found = candidate, true
			break
		}
	}
	if !found {
		writeError(writer, statusError{http.StatusNotFound, "task_not_found", fmt.Sprintf("看板中不存在任务 %q。", taskID), nil})
		return
	}
	_, plan, err := server.runtime.Definition(request.Context(), boardID)
	if err != nil {
		writeError(writer, err)
		return
	}
	view, err := server.runtime.View(request.Context(), boardID)
	if err != nil {
		writeError(writer, err)
		return
	}
	detail := TaskDetail{
		BoardID: boardID, TaskID: task.ID, Title: task.Title, State: task.State,
		Column:             columnOf(view, task.ID),
		TerminalContract:   task.TerminalContract,
		DependsOn:          []string{},
		RequiredBy:         []string{},
		PathScopes:         []string{},
		CapabilityTags:     []string{},
		VerifierIDs:        []string{},
		ActiveAttemptID:    task.ActiveAttemptID,
		AcceptedEvidenceID: task.AcceptedEvidenceID,
		AccessMode:         task.AccessMode,
		AttemptCount:       task.AttemptCount,
		MaxAttempts:        agoboardprotocol.MaxAttempts,
		NextEligibleAt:     task.NextEligibleAt,
		FailureClass:       task.FailureClass,
		BlockedReason:      task.BlockedReason,
		Attempts:           []TaskAttempt{},
		Leases:             []TaskLease{},
		Evidence:           []TaskEvidence{},
	}
	for _, dependency := range board.Dependencies {
		if dependency.TaskID == task.ID {
			detail.DependsOn = append(detail.DependsOn, dependency.DependsOn)
		}
		if dependency.DependsOn == task.ID {
			detail.RequiredBy = append(detail.RequiredBy, dependency.TaskID)
		}
	}
	for _, proposal := range plan.Tasks {
		if proposal.ID == task.ID {
			detail.PathScopes = append(detail.PathScopes, proposal.PathScopes...)
			detail.CapabilityTags = append(detail.CapabilityTags, proposal.CapabilityTags...)
			detail.VerifierIDs = append(detail.VerifierIDs, proposal.VerifierIDs...)
			break
		}
	}
	for _, attempt := range board.Attempts {
		if attempt.TaskID == task.ID {
			detail.Attempts = append(detail.Attempts, TaskAttempt{
				ID: attempt.ID, Number: attempt.Number, State: attempt.State, WorkerID: attempt.WorkerID,
				Generation: attempt.Generation, EvidenceID: attempt.EvidenceID,
				FailureClass: attempt.FailureClass, FailureReason: attempt.FailureReason,
			})
		}
	}
	for _, lease := range board.Leases {
		if lease.TaskID == task.ID {
			detail.Leases = append(detail.Leases, TaskLease{
				ID: lease.ID, AttemptID: lease.AttemptID, WorkerID: lease.WorkerID, State: lease.State,
				Generation: lease.Generation, AcquiredAt: lease.AcquiredAt, ExpiresAt: lease.ExpiresAt,
			})
		}
	}
	for _, evidence := range board.Evidence {
		if evidence.TaskID == task.ID {
			detail.Evidence = append(detail.Evidence, TaskEvidence{
				ID: evidence.ID, AttemptID: evidence.AttemptID, WorkerID: evidence.WorkerID,
				Artifact: evidence.Artifact, Summary: evidence.Summary, State: evidence.State,
				Result: evidence.Result, Verdict: evidence.Verdict, VerdictReason: evidence.VerdictReason,
			})
		}
	}
	writeJSON(writer, http.StatusOK, detail)
}

func (server *Server) listProviders(writer http.ResponseWriter, request *http.Request) {
	providers := server.providers
	if providers == nil {
		providers = []Provider{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"providers": providers})
}

// -- projection --------------------------------------------------------------

func (server *Server) snapshot(ctx context.Context, boardID string) (Snapshot, error) {
	board, err := server.store.Board(ctx, boardID)
	if err != nil {
		return Snapshot{}, err
	}
	view, err := server.runtime.View(ctx, boardID)
	if err != nil {
		return Snapshot{}, err
	}
	goal, _, err := server.runtime.Definition(ctx, boardID)
	if err != nil {
		return Snapshot{}, err
	}
	completion, err := server.store.Completion(ctx, boardID)
	if err != nil {
		return Snapshot{}, err
	}
	sequence, err := server.store.LatestSequence(ctx, boardID)
	if err != nil {
		return Snapshot{}, err
	}
	dependsOn := make(map[string][]string, len(board.Tasks))
	dependencies := make([]SnapshotDependency, 0, len(board.Dependencies))
	for _, dependency := range board.Dependencies {
		dependsOn[dependency.TaskID] = append(dependsOn[dependency.TaskID], dependency.DependsOn)
		dependencies = append(dependencies, SnapshotDependency{ID: dependency.ID, TaskID: dependency.TaskID, DependsOn: dependency.DependsOn})
	}
	columns := make([]SnapshotColumn, 0, len(view.Columns))
	for _, column := range view.Columns {
		tasks := make([]SnapshotTask, 0, len(column.Tasks))
		for _, task := range column.Tasks {
			edges := dependsOn[task.ID]
			if edges == nil {
				edges = []string{}
			}
			tasks = append(tasks, SnapshotTask{ID: task.ID, Title: task.Title, State: task.State, DependsOn: edges})
		}
		columns = append(columns, SnapshotColumn{Name: column.Name, Tasks: tasks})
	}
	total := completion.Passed + completion.Failed + completion.Remaining
	return Snapshot{
		BoardID: board.ID, Title: board.Title, Version: board.Version, GraphVersion: board.Version,
		LatestEventSequence: sequence,
		Goal: SnapshotGoal{
			Objective:     goal.Objective.Summary,
			Repository:    repositoryRequest{Root: goal.Repository.ID, Revision: goal.Repository.Revision},
			ExecutionMode: goal.ExecutionMode,
		},
		Columns:      columns,
		Dependencies: dependencies,
		Progress: SnapshotProgress{
			Status: completion.Status, Passed: completion.Passed,
			Failed: completion.Failed, Remaining: completion.Remaining, Total: total,
		},
		Paused:    board.Paused,
		Completed: total > 0 && completion.Remaining == 0,
	}, nil
}

func columnOf(view agoboardruntime.BoardView, taskID string) agoboardruntime.Column {
	for _, column := range view.Columns {
		for _, task := range column.Tasks {
			if task.ID == taskID {
				return column.Name
			}
		}
	}
	return ""
}

// -- goal construction -------------------------------------------------------

// buildGoal turns product input into a validated planner request. The board and
// objective identities are derived from the client command ID, so an exact
// replay addresses the same durable board and a reused command ID with changed
// content collides inside the store instead of silently creating a second board.
func (server *Server) buildGoal(input goalRequest) (agoboardruntime.Goal, error) {
	commandID := strings.TrimSpace(input.CommandID)
	if commandID == "" {
		return agoboardruntime.Goal{}, statusError{http.StatusBadRequest, "invalid_request", "缺少 command_id：每个变更请求都需要一个幂等命令 ID。", nil}
	}
	objective := strings.TrimSpace(input.Objective)
	if objective == "" {
		return agoboardruntime.Goal{}, statusError{http.StatusBadRequest, "invalid_request", "缺少 objective：请用自然语言描述要达成的目标。", nil}
	}
	mode := strings.TrimSpace(input.ExecutionMode)
	if mode == "" {
		mode = ExecutionModeFake
	}
	if mode != ExecutionModeFake {
		return agoboardruntime.Goal{}, statusError{http.StatusBadRequest, "invalid_request", fmt.Sprintf("不支持的执行模式 %q，当前仅支持 %q。", mode, ExecutionModeFake), nil}
	}
	root := strings.TrimSpace(input.Repository.Root)
	if root == "" {
		return agoboardruntime.Goal{}, statusError{http.StatusBadRequest, "invalid_request", "缺少 repository.root：请选择一个本地仓库目录。", nil}
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return agoboardruntime.Goal{}, statusError{http.StatusBadRequest, "repository_unavailable", fmt.Sprintf("仓库目录 %q 不存在或不可读，请选择一个有效的本地仓库目录。", root), err}
	}
	revision := strings.TrimSpace(input.Repository.Revision)
	if revision == "" {
		revision = defaultRevision
	}
	boardID := derivedID("board", commandID)
	return agoboardruntime.Goal{
		BoardID:    boardID,
		Repository: agoplanner.Repository{ID: root, Revision: revision},
		// The objective is stored exactly as the user wrote it; nothing
		// normalizes, translates, or truncates the Chinese text.
		Objective:     agoplanner.Objective{ID: derivedID("objective", commandID), Summary: objective},
		ProjectGates:  defaultProjectGates(),
		Constraints:   defaultConstraints(),
		ExecutionMode: mode,
	}, nil
}

func defaultProjectGates() []agoplanner.ProjectGate {
	return []agoplanner.ProjectGate{{
		ID: "gate-demo", Title: "目标验收",
		AcceptanceCriteria: []string{"所有任务通过独立验收"},
		VerifierIDs:        []string{"ago-verifier"},
	}}
}

func defaultConstraints() agoplanner.Constraints {
	return agoplanner.Constraints{
		PathScopes:     []string{"README.md", "docs"},
		CapabilityTags: []string{"repo-read", "repo-write", "tests", "report"},
		VerifierIDs:    []string{"ago-verifier"},
	}
}

func derivedID(namespace, commandID string) string {
	digest := sha256.Sum256([]byte(namespace + "\x00" + commandID))
	return namespace + ":" + hex.EncodeToString(digest[:16])
}

// -- error and encoding helpers ----------------------------------------------

type statusError struct {
	status  int
	code    string
	message string
	err     error
}

func (e statusError) Error() string {
	if e.err != nil {
		return e.message + ": " + e.err.Error()
	}
	return e.message
}
func (e statusError) Unwrap() error { return e.err }

func writeError(writer http.ResponseWriter, err error) {
	status, code, message := http.StatusBadRequest, "invalid_request", "请求无效。"
	var typed statusError
	switch {
	case errors.As(err, &typed):
		status, code, message = typed.status, typed.code, typed.message
	case errors.Is(err, agoboardstore.ErrBoardNotFound):
		status, code, message = http.StatusNotFound, "board_not_found", "看板不存在。"
	case errors.Is(err, agoboardstore.ErrCommandConflict):
		status, code, message = http.StatusConflict, "command_conflict", "该命令 ID 已用于不同的请求内容。"
	}
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func decodeRequest(writer http.ResponseWriter, request *http.Request, target any) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return statusError{http.StatusBadRequest, "invalid_request", "请求体不是合法的 JSON。", err}
	}
	return nil
}

// setPaused issues a durable board.pause or board.resume protocol command.
//
// Pausing stops new claims; attempts already running keep their leases and are
// allowed to finish, because cancelling live work is a separate, explicit act.
// The state is part of the board aggregate, so it survives a restart.
func (server *Server) setPaused(pause bool) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		var input controlRequest
		if err := decodeRequest(writer, request, &input); err != nil {
			writeError(writer, err)
			return
		}
		if strings.TrimSpace(input.CommandID) == "" {
			writeError(writer, statusError{http.StatusBadRequest, "invalid_request", "该请求需要 command_id。", nil})
			return
		}
		boardID := request.PathValue("boardID")
		board, err := server.store.Board(request.Context(), boardID)
		if err != nil {
			writeError(writer, err)
			return
		}
		commandType := agoboardprotocol.CommandBoardResume
		reason := strings.TrimSpace(input.Reason)
		if pause {
			commandType = agoboardprotocol.CommandBoardPause
			if reason == "" {
				reason = "用户暂停"
			}
		}
		_, err = server.store.ApplyBoard(request.Context(), boardID, agoboardprotocol.Command{
			SchemaVersion:   agoboardprotocol.SchemaVersion,
			ID:              input.CommandID,
			ExpectedVersion: board.Version,
			Actor:           agoboardprotocol.Actor{ID: "ago-operator", Role: agoboardprotocol.RoleCoordinator},
			Type:            commandType,
			Reason:          reason,
		})
		if err != nil {
			if errors.Is(err, agoboardstore.ErrCommandConflict) {
				writeError(writer, statusError{http.StatusConflict, "command_conflict", "该命令 ID 已用于不同的请求内容。", err})
				return
			}
			// An illegal transition, such as pausing an already-paused board, is
			// reported rather than silently treated as success.
			writeError(writer, statusError{http.StatusConflict, "illegal_transition", "看板当前状态不允许该操作。", err})
			return
		}
		snapshot, err := server.snapshot(request.Context(), boardID)
		if err != nil {
			writeError(writer, err)
			return
		}
		server.notify(boardID)
		writeJSON(writer, http.StatusOK, snapshot)
	}
}

// downloadArtifact streams managed executor output.
//
// The artifact must be referenced by this board's durable evidence: an
// identifier alone is not authority to read bytes. The descriptor recorded in
// evidence is what the artifact store re-verifies against, so a file whose
// bytes changed after the fact is refused rather than served.
func (server *Server) downloadArtifact(writer http.ResponseWriter, request *http.Request) {
	if server.artifacts == nil {
		writeError(writer, statusError{http.StatusNotFound, "artifacts_unavailable", "当前未启用工件存储。", nil})
		return
	}
	boardID, artifactID := request.PathValue("boardID"), request.PathValue("artifactID")
	board, err := server.store.Board(request.Context(), boardID)
	if err != nil {
		writeError(writer, err)
		return
	}
	reference, found := artifactReference(board, artifactID)
	if !found {
		// A board that does not reference the artifact is told the same thing
		// as one where it does not exist, and no local path is echoed back.
		writeError(writer, statusError{http.StatusNotFound, "artifact_not_found", "该看板没有引用此工件。", nil})
		return
	}
	descriptor := agoartifact.Descriptor{
		ID: reference.ID, Type: reference.Type, DisplayName: reference.DisplayName,
		Bytes: reference.Bytes, SHA256: reference.SHA256,
	}
	reader, err := server.artifacts.Open(request.Context(), descriptor)
	if err != nil {
		if errors.Is(err, agoartifact.ErrNotFound) || errors.Is(err, agoartifact.ErrBadID) {
			writeError(writer, statusError{http.StatusNotFound, "artifact_not_found", "工件不存在。", nil})
			return
		}
		// A containment or digest failure is a integrity problem, not a client
		// error, and the local path must not appear in the response.
		writeError(writer, statusError{http.StatusConflict, "artifact_corrupt", "工件内容与记录的校验不一致，已拒绝下载。", nil})
		return
	}
	defer reader.Close()
	writer.Header().Set("Content-Type", descriptor.Type)
	writer.Header().Set("Content-Length", strconv.FormatInt(descriptor.Bytes, 10))
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	// The display name is already sanitized by the artifact store; it is
	// encoded here so a non-ASCII name cannot alter the header's structure.
	writer.Header().Set("Content-Disposition", contentDisposition(descriptor.DisplayName))
	writer.WriteHeader(http.StatusOK)
	_, _ = io.Copy(writer, reader)
}

func artifactReference(board agoboardprotocol.Board, artifactID string) (agoboardprotocol.ArtifactRef, bool) {
	for _, evidence := range board.Evidence {
		for _, reference := range evidence.Result.Artifacts {
			if reference.ID == artifactID {
				return reference, true
			}
		}
	}
	return agoboardprotocol.ArtifactRef{}, false
}

// contentDisposition builds a header that is safe for any display name: an
// ASCII fallback plus an RFC 5987 encoding for the real value.
func contentDisposition(name string) string {
	ascii := make([]rune, 0, len(name))
	for _, r := range name {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			ascii = append(ascii, '_')
			continue
		}
		ascii = append(ascii, r)
	}
	fallback := string(ascii)
	if fallback == "" {
		fallback = "artifact"
	}
	return fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, fallback, url.PathEscape(name))
}

// retryTask records a person's decision to try a stopped task again.
//
// The automatic attempt bound exists to stop machine retry loops; a human
// choosing to spend another budget is a different act, so it is a distinct
// audited command rather than a way around the bound. Any note the user
// supplies becomes the recorded reason, which is how the blocked-task input
// form reaches the durable history.
func (server *Server) retryTask(writer http.ResponseWriter, request *http.Request) {
	var input controlRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	if strings.TrimSpace(input.CommandID) == "" {
		writeError(writer, statusError{http.StatusBadRequest, "invalid_request", "该请求需要 command_id。", nil})
		return
	}
	boardID, taskID := request.PathValue("boardID"), request.PathValue("taskID")
	board, err := server.store.Board(request.Context(), boardID)
	if err != nil {
		writeError(writer, err)
		return
	}
	reason := strings.TrimSpace(input.Reason)
	if reason == "" {
		reason = "用户请求重试"
	}
	if _, err := server.store.ApplyBoard(request.Context(), boardID, agoboardprotocol.Command{
		SchemaVersion:   agoboardprotocol.SchemaVersion,
		ID:              input.CommandID,
		ExpectedVersion: board.Version,
		Actor:           agoboardprotocol.Actor{ID: "ago-operator", Role: agoboardprotocol.RoleCoordinator},
		Type:            agoboardprotocol.CommandTaskRetry,
		TaskID:          taskID,
		Reason:          reason,
	}); err != nil {
		if errors.Is(err, agoboardstore.ErrCommandConflict) {
			writeError(writer, statusError{http.StatusConflict, "command_conflict", "该命令 ID 已用于不同的请求内容。", err})
			return
		}
		writeError(writer, statusError{http.StatusConflict, "illegal_transition", "该任务当前状态不允许重试。", err})
		return
	}
	snapshot, err := server.snapshot(request.Context(), boardID)
	if err != nil {
		writeError(writer, err)
		return
	}
	server.notify(boardID)
	writeJSON(writer, http.StatusOK, snapshot)
}

// DecisionSource reports the items an autonomous supervisor needs a person for.
// It is an interface so the API does not depend on the supervisor package, and
// so a board can be served with no supervisor at all.
type DecisionSource interface {
	PendingDecisions(boardID string) []PendingDecision
}

// PendingDecision is one item in the user's attention queue. It is deliberately
// self-contained: a user must never have to go find a worker to understand it.
type PendingDecision struct {
	Kind         string    `json:"kind"`
	TaskID       string    `json:"task_id,omitempty"`
	Title        string    `json:"title"`
	Reason       string    `json:"reason"`
	Suggestion   string    `json:"suggestion"`
	RaisedAt     time.Time `json:"raised_at"`
	AttemptsUsed int       `json:"attempts_used,omitempty"`
}

func (server *Server) listDecisions(writer http.ResponseWriter, request *http.Request) {
	boardID := request.PathValue("boardID")
	if _, err := server.store.Board(request.Context(), boardID); err != nil {
		writeError(writer, err)
		return
	}
	pending := []PendingDecision{}
	if server.decisions != nil {
		pending = server.decisions.PendingDecisions(boardID)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"decisions": pending})
}

// IntegrationSetup establishes a board's Ago-owned integration ref.
type IntegrationSetup interface {
	RefName(boardID string) string
	EnsureRef(ctx context.Context, repository, ref, baseRevision string) (string, error)
}
