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
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

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
	ID             string   `json:"id"`
	Kind           string   `json:"kind"`
	Capabilities   []string `json:"capabilities"`
	AuthConfigured bool     `json:"auth_configured"`
}

type Options struct {
	Runtime *agoboardruntime.Runtime
	Store   *agoboardstore.Store
	// Providers is the non-secret capability roster reported to clients.
	Providers []Provider
	// PollInterval bounds how long an idle event stream waits before it
	// re-reads SQLite. The in-memory notifier only makes delivery prompt; it is
	// never the authority for what a subscriber receives.
	PollInterval time.Duration
}

type Server struct {
	runtime      *agoboardruntime.Runtime
	store        *agoboardstore.Store
	providers    []Provider
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
		pollInterval: options.PollInterval,
		waiters:      make(map[string]chan struct{}),
	}, nil
}

func (server *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/goals", server.createGoal)
	mux.HandleFunc("GET /api/v1/boards/{boardID}", server.boardSnapshot)
	mux.HandleFunc("GET /api/v1/boards/{boardID}/tasks/{taskID}", server.taskDetail)
	mux.HandleFunc("POST /api/v1/boards/{boardID}/advance", server.advance)
	mux.HandleFunc("GET /api/v1/boards/{boardID}/events", server.events)
	mux.HandleFunc("GET /api/v1/providers", server.listProviders)
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

type advanceRequest struct {
	CommandID string `json:"command_id"`
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
	ID         string                        `json:"id"`
	State      agoboardprotocol.AttemptState `json:"state"`
	WorkerID   string                        `json:"worker_id"`
	EvidenceID string                        `json:"evidence_id"`
}

type TaskLease struct {
	ID        string                      `json:"id"`
	AttemptID string                      `json:"attempt_id"`
	WorkerID  string                      `json:"worker_id"`
	State     agoboardprotocol.LeaseState `json:"state"`
}

type TaskEvidence struct {
	ID        string                         `json:"id"`
	AttemptID string                         `json:"attempt_id"`
	WorkerID  string                         `json:"worker_id"`
	Artifact  string                         `json:"artifact"`
	Summary   string                         `json:"summary"`
	State     agoboardprotocol.EvidenceState `json:"state"`
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
	Attempts           []TaskAttempt                     `json:"attempts"`
	Leases             []TaskLease                       `json:"leases"`
	Evidence           []TaskEvidence                    `json:"evidence"`
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

func (server *Server) advance(writer http.ResponseWriter, request *http.Request) {
	var input advanceRequest
	if err := decodeRequest(writer, request, &input); err != nil {
		writeError(writer, err)
		return
	}
	if strings.TrimSpace(input.CommandID) == "" {
		writeError(writer, statusError{http.StatusBadRequest, "invalid_request", "推进请求需要 command_id。", nil})
		return
	}
	boardID := request.PathValue("boardID")
	if _, err := server.store.Board(request.Context(), boardID); err != nil {
		writeError(writer, err)
		return
	}
	// The client command ID is the durable claim identity: replaying it returns
	// current state without claiming a second task, and reusing it for another
	// board is a conflict.
	if _, err := server.runtime.TickOnce(request.Context(), boardID, input.CommandID); err != nil {
		if errors.Is(err, agoboardstore.ErrCommandConflict) {
			writeError(writer, statusError{http.StatusConflict, "command_conflict", "该命令 ID 已用于推进另一个看板，请换一个命令 ID。", err})
			return
		}
		writeError(writer, statusError{http.StatusBadRequest, "advance_failed", "推进看板失败。", err})
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
			detail.Attempts = append(detail.Attempts, TaskAttempt{ID: attempt.ID, State: attempt.State, WorkerID: attempt.WorkerID, EvidenceID: attempt.EvidenceID})
		}
	}
	for _, lease := range board.Leases {
		if lease.TaskID == task.ID {
			detail.Leases = append(detail.Leases, TaskLease{ID: lease.ID, AttemptID: lease.AttemptID, WorkerID: lease.WorkerID, State: lease.State})
		}
	}
	for _, evidence := range board.Evidence {
		if evidence.TaskID == task.ID {
			detail.Evidence = append(detail.Evidence, TaskEvidence{ID: evidence.ID, AttemptID: evidence.AttemptID, WorkerID: evidence.WorkerID, Artifact: evidence.Artifact, Summary: evidence.Summary, State: evidence.State})
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
		// Pause control is owned by the D3 scheduler increment; until it exists
		// no board can be paused, and reporting a fixed false is honest rather
		// than pretending a control is wired.
		Paused:    false,
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
