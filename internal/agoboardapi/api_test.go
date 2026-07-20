package agoboardapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"claudexflow/internal/agoboardapi"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agoscheduler"
)

const (
	chineseDemoObjective = "分析当前仓库，为 README 增加一个快速开始章节，运行相关测试，并生成完成报告。"

	apiCoordinatorID         = "ago-scheduler"
	apiWorkerID              = "ago-demo-worker"
	apiVerifierID            = "ago-verifier"
	apiMaxAdvanceIterations  = 48
	providerSecretSentinel   = "super-secret-sentinel-value"
	providerSecretEnvVarName = "AGO_PROVIDER_API_KEY"
)

// newBoardTestServer builds a temp-dir-backed SQLite store, a runtime wired to
// the deterministic demo planner and in-package fake executor/verifier, and
// an agoboardapi server on top of it. Callers own the returned store's
// lifecycle (Close it, possibly early for restart tests) so it is not
// registered with t.Cleanup here.
func newBoardTestServer(t *testing.T, dbPath string, providers []agoboardapi.Provider) (http.Handler, *agoboardstore.Store) {
	handler, store, _ := newBoardTestServerWithScheduler(t, dbPath, providers)
	return handler, store
}

// newBoardTestServerWithScheduler also returns the scheduler that advances the
// board. There is no manual advance endpoint: progress comes from scheduling.
func newBoardTestServerWithScheduler(t *testing.T, dbPath string, providers []agoboardapi.Provider) (http.Handler, *agoboardstore.Store, *agoscheduler.Scheduler) {
	t.Helper()
	store, err := agoboardstore.Open(dbPath)
	if err != nil {
		t.Fatalf("agoboardstore.Open(%q): %v", dbPath, err)
	}
	runtime := agoboardruntime.New(store, agoplanner.DemoPlanner{}, agoboardruntime.Options{
		CoordinatorID: apiCoordinatorID,
		WorkerID:      apiWorkerID,
		VerifierID:    apiVerifierID,
		LeaseDuration: time.Minute,
		Now:           func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) },
	})
	if providers == nil {
		providers = []agoboardapi.Provider{{ID: "ago-demo-planner", Kind: "planner", Capabilities: []string{"planning"}, AuthConfigured: false}}
	}
	server, err := agoboardapi.New(agoboardapi.Options{
		Runtime:      runtime,
		Store:        store,
		Providers:    providers,
		PollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("agoboardapi.New: %v", err)
	}
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime, Executor: &fakeAPIExecutor{}, Verifier: &fakeAPIVerifier{},
		CoordinatorID: apiCoordinatorID, WorkerID: apiWorkerID, VerifierID: apiVerifierID,
		LeaseDuration: time.Minute,
		Now:           func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("agoscheduler.New: %v", err)
	}
	return server.Handler(), store, scheduler
}

// driveWithScheduler advances a board the way production does, by running
// scheduler cycles until the graph stops changing.
func driveWithScheduler(t *testing.T, store *agoboardstore.Store, scheduler *agoscheduler.Scheduler, boardID string) {
	t.Helper()
	ctx := context.Background()
	previous := uint64(0)
	for range apiMaxAdvanceIterations {
		if _, err := scheduler.RunOnce(ctx); err != nil {
			t.Fatalf("scheduler cycle: %v", err)
		}
		board, err := store.Board(ctx, boardID)
		if err != nil {
			t.Fatal(err)
		}
		if board.Version == previous {
			return
		}
		previous = board.Version
	}
}

// fakeAPIExecutor deterministically "executes" every dispatched task so the
// board can be driven to completion without any real work.
type fakeAPIExecutor struct{}

func (*fakeAPIExecutor) Execute(_ context.Context, dispatch agoboardruntime.Dispatch) (agoboardruntime.ExecutionResult, error) {
	return agoboardruntime.ExecutionResult{
		Artifact: "artifact://ago-demo/" + dispatch.AttemptID,
		Summary:  "演示执行器已完成任务",
	}, nil
}

// fakeAPIVerifier deterministically accepts every attempt's evidence.
type fakeAPIVerifier struct{}

func (*fakeAPIVerifier) Verify(_ context.Context, _ agoboardruntime.Dispatch, _ agoboardruntime.ExecutionResult) (agoboardruntime.Review, error) {
	return agoboardruntime.Review{Accepted: true, Reason: "演示验收通过"}, nil
}

// -- wire-shape types matching the frozen agoboardapi contract --------------

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type snapshotTask struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	State     string   `json:"state"`
	DependsOn []string `json:"depends_on"`
}

type snapshotColumn struct {
	Name  string         `json:"name"`
	Tasks []snapshotTask `json:"tasks"`
}

type snapshotGoal struct {
	Objective  string `json:"objective"`
	Repository struct {
		Root     string `json:"root"`
		Revision string `json:"revision"`
	} `json:"repository"`
	ExecutionMode string `json:"execution_mode"`
}

type snapshotDependency struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	DependsOn string `json:"depends_on"`
}

type snapshotProgress struct {
	Status    string `json:"status"`
	Passed    int    `json:"passed"`
	Failed    int    `json:"failed"`
	Remaining int    `json:"remaining"`
	Total     int    `json:"total"`
}

type boardSnapshot struct {
	BoardID             string               `json:"board_id"`
	Title               string               `json:"title"`
	Version             uint64               `json:"version"`
	GraphVersion        uint64               `json:"graph_version"`
	LatestEventSequence uint64               `json:"latest_event_sequence"`
	Goal                snapshotGoal         `json:"goal"`
	Columns             []snapshotColumn     `json:"columns"`
	Dependencies        []snapshotDependency `json:"dependencies"`
	Progress            snapshotProgress     `json:"progress"`
	Paused              bool                 `json:"paused"`
	Completed           bool                 `json:"completed"`
}

type goalCreateResponse struct {
	Replayed  bool          `json:"replayed"`
	CommandID string        `json:"command_id"`
	Board     boardSnapshot `json:"board"`
}

type taskAttempt struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	WorkerID   string `json:"worker_id"`
	EvidenceID string `json:"evidence_id"`
}

type taskLease struct {
	ID        string `json:"id"`
	AttemptID string `json:"attempt_id"`
	WorkerID  string `json:"worker_id"`
	State     string `json:"state"`
}

type taskEvidence struct {
	ID        string `json:"id"`
	AttemptID string `json:"attempt_id"`
	WorkerID  string `json:"worker_id"`
	Artifact  string `json:"artifact"`
	Summary   string `json:"summary"`
	State     string `json:"state"`
}

type taskDetail struct {
	BoardID          string `json:"board_id"`
	TaskID           string `json:"task_id"`
	Title            string `json:"title"`
	State            string `json:"state"`
	Column           string `json:"column"`
	TerminalContract struct {
		Outcome            string   `json:"outcome"`
		AcceptanceCriteria []string `json:"acceptance_criteria"`
	} `json:"terminal_contract"`
	DependsOn          []string       `json:"depends_on"`
	RequiredBy         []string       `json:"required_by"`
	PathScopes         []string       `json:"path_scopes"`
	CapabilityTags     []string       `json:"capability_tags"`
	VerifierIDs        []string       `json:"verifier_ids"`
	ActiveAttemptID    string         `json:"active_attempt_id"`
	AcceptedEvidenceID string         `json:"accepted_evidence_id"`
	Attempts           []taskAttempt  `json:"attempts"`
	Leases             []taskLease    `json:"leases"`
	Evidence           []taskEvidence `json:"evidence"`
}

type providersResponse struct {
	Providers []agoboardapi.Provider `json:"providers"`
}

// -- request/response helpers -----------------------------------------------

func goalBody(commandID, objective, repoRoot string) map[string]any {
	return map[string]any{
		"command_id":     commandID,
		"objective":      objective,
		"repository":     map[string]any{"root": repoRoot, "revision": "HEAD"},
		"execution_mode": "fake",
	}
}

func doRequest(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func decodeInto(t *testing.T, recorder *httptest.ResponseRecorder, target any) {
	t.Helper()
	if err := json.Unmarshal(recorder.Body.Bytes(), target); err != nil {
		t.Fatalf("decode response body %q: %v", recorder.Body.String(), err)
	}
}

// snapshotOf reads the current board projection over HTTP.
func snapshotOf(t *testing.T, handler http.Handler, boardID string) boardSnapshot {
	t.Helper()
	response := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+boardID, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d, body = %s", response.Code, response.Body.String())
	}
	var snapshot boardSnapshot
	decodeInto(t, response, &snapshot)
	return snapshot
}

func taskIDSet(snapshot boardSnapshot) map[string]string {
	result := make(map[string]string)
	for _, column := range snapshot.Columns {
		for _, task := range column.Tasks {
			result[task.ID] = task.State
		}
	}
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsChineseCharacter(value string) bool {
	for _, r := range value {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return strings.ContainsAny(value, "，。")
}

func assertNoSentinelLeak(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(recorder.Body.String(), providerSecretSentinel) {
		t.Fatalf("response body leaked provider secret: %s", recorder.Body.String())
	}
	for key, values := range recorder.Header() {
		for _, value := range values {
			if strings.Contains(value, providerSecretSentinel) {
				t.Fatalf("response header %q leaked provider secret: %q", key, value)
			}
		}
	}
}

// -- required test cases ------------------------------------------------------

func TestChineseObjectiveRoundTripsThroughCreateAndIsDurable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store := newBoardTestServer(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })
	repoRoot := t.TempDir()

	created := doRequest(t, handler, http.MethodPost, "/api/v1/goals", goalBody("cmd-cn-roundtrip", chineseDemoObjective, repoRoot))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var createdResponse goalCreateResponse
	decodeInto(t, created, &createdResponse)
	if createdResponse.Replayed {
		t.Fatalf("first create reported replayed = true")
	}
	if createdResponse.Board.Goal.Objective != chineseDemoObjective {
		t.Fatalf("objective = %q, want %q", createdResponse.Board.Goal.Objective, chineseDemoObjective)
	}

	fetched := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+createdResponse.Board.BoardID, nil)
	if fetched.Code != http.StatusOK {
		t.Fatalf("get snapshot status = %d, body = %s", fetched.Code, fetched.Body.String())
	}
	var snapshot boardSnapshot
	decodeInto(t, fetched, &snapshot)
	if snapshot.BoardID != createdResponse.Board.BoardID || snapshot.Goal.Objective != chineseDemoObjective {
		t.Fatalf("durable snapshot = %#v", snapshot)
	}
}

func TestExactCommandReplayCreatesExactlyOneBoard(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store := newBoardTestServer(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })
	body := goalBody("cmd-replay", chineseDemoObjective, t.TempDir())

	first := doRequest(t, handler, http.MethodPost, "/api/v1/goals", body)
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, body = %s", first.Code, first.Body.String())
	}
	var firstResponse goalCreateResponse
	decodeInto(t, first, &firstResponse)
	if firstResponse.Replayed {
		t.Fatalf("first create reported replayed = true")
	}

	second := doRequest(t, handler, http.MethodPost, "/api/v1/goals", body)
	if second.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body = %s", second.Code, second.Body.String())
	}
	var secondResponse goalCreateResponse
	decodeInto(t, second, &secondResponse)
	if !secondResponse.Replayed {
		t.Fatalf("replay reported replayed = false")
	}
	if secondResponse.Board.BoardID != firstResponse.Board.BoardID {
		t.Fatalf("replay board id = %q, want %q", secondResponse.Board.BoardID, firstResponse.Board.BoardID)
	}
}

func TestSameCommandIDWithDifferentObjectiveConflicts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store := newBoardTestServer(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })
	repoRoot := t.TempDir()

	first := doRequest(t, handler, http.MethodPost, "/api/v1/goals", goalBody("cmd-conflict", chineseDemoObjective, repoRoot))
	if first.Code != http.StatusCreated {
		t.Fatalf("first create status = %d, body = %s", first.Code, first.Body.String())
	}
	changed := goalBody("cmd-conflict", chineseDemoObjective+"（已修改）", repoRoot)
	second := doRequest(t, handler, http.MethodPost, "/api/v1/goals", changed)
	if second.Code != http.StatusConflict {
		t.Fatalf("conflicting create status = %d, body = %s", second.Code, second.Body.String())
	}
}

func TestNonexistentRepositoryRootRejectedWithChineseMessage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store := newBoardTestServer(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })
	missingRoot := filepath.Join(t.TempDir(), "does-not-exist")

	response := doRequest(t, handler, http.MethodPost, "/api/v1/goals", goalBody("cmd-missing-repo", chineseDemoObjective, missingRoot))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var errorBody apiError
	decodeInto(t, response, &errorBody)
	if errorBody.Error.Code == "" || !containsChineseCharacter(errorBody.Error.Message) {
		t.Fatalf("error body = %#v, want a Chinese-language message", errorBody)
	}
}

func TestSnapshotHasCanonicalColumnsAndDependencyEdgesReflectedInTasks(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store := newBoardTestServer(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })

	created := doRequest(t, handler, http.MethodPost, "/api/v1/goals", goalBody("cmd-columns", chineseDemoObjective, t.TempDir()))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var createdResponse goalCreateResponse
	decodeInto(t, created, &createdResponse)
	snapshot := createdResponse.Board

	wantOrder := []string{"Backlog", "Ready", "Claimed", "Running", "Review", "Blocked", "Done"}
	if len(snapshot.Columns) != len(wantOrder) {
		t.Fatalf("columns = %#v, want %d columns", snapshot.Columns, len(wantOrder))
	}
	dependsOnByTaskID := make(map[string][]string, len(snapshot.Columns))
	for index, column := range snapshot.Columns {
		if column.Name != wantOrder[index] {
			t.Fatalf("column[%d] = %q, want %q", index, column.Name, wantOrder[index])
		}
		if column.Tasks == nil {
			t.Fatalf("column %q has a null task list", column.Name)
		}
		for _, task := range column.Tasks {
			dependsOnByTaskID[task.ID] = task.DependsOn
		}
	}
	if len(snapshot.Dependencies) == 0 {
		t.Fatalf("demo plan produced no dependency edges")
	}
	for _, dependency := range snapshot.Dependencies {
		dependsOn, found := dependsOnByTaskID[dependency.TaskID]
		if !found {
			t.Fatalf("dependency %#v references a task missing from any column", dependency)
		}
		if !containsString(dependsOn, dependency.DependsOn) {
			t.Fatalf("task %q depends_on = %#v, want it to contain %q", dependency.TaskID, dependsOn, dependency.DependsOn)
		}
	}
}

func TestTaskDetailReturnsContractScopesAndAttemptsAfterAdvance(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store, scheduler := newBoardTestServerWithScheduler(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })

	created := doRequest(t, handler, http.MethodPost, "/api/v1/goals", goalBody("cmd-task-detail", chineseDemoObjective, t.TempDir()))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var createdResponse goalCreateResponse
	decodeInto(t, created, &createdResponse)
	boardID := createdResponse.Board.BoardID

	driveWithScheduler(t, store, scheduler, boardID)
	final := snapshotOf(t, handler, boardID)
	if !final.Completed || final.Progress.Failed != 0 {
		t.Fatalf("board did not complete cleanly: %#v", final.Progress)
	}

	var doneTaskID string
	for _, column := range final.Columns {
		if column.Name == "Done" && len(column.Tasks) > 0 {
			doneTaskID = column.Tasks[0].ID
			break
		}
	}
	if doneTaskID == "" {
		t.Fatalf("no task reached Done in final snapshot %#v", final)
	}

	detail := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+boardID+"/tasks/"+doneTaskID, nil)
	if detail.Code != http.StatusOK {
		t.Fatalf("task detail status = %d, body = %s", detail.Code, detail.Body.String())
	}
	var body taskDetail
	decodeInto(t, detail, &body)
	if len(body.TerminalContract.AcceptanceCriteria) == 0 {
		t.Fatalf("task detail has no acceptance criteria: %#v", body)
	}
	if len(body.PathScopes) == 0 || len(body.CapabilityTags) == 0 || len(body.VerifierIDs) == 0 {
		t.Fatalf("task detail missing scopes/tags/verifiers: %#v", body)
	}
	if len(body.Attempts) != 1 || body.Attempts[0].State != "passed" || body.Attempts[0].WorkerID != apiWorkerID {
		t.Fatalf("attempts = %#v", body.Attempts)
	}
	if len(body.Leases) != 1 || body.Leases[0].State != "completed" {
		t.Fatalf("leases = %#v", body.Leases)
	}
	if len(body.Evidence) != 1 || body.Evidence[0].State != "accepted" || body.Evidence[0].WorkerID != apiWorkerID {
		t.Fatalf("evidence = %#v", body.Evidence)
	}
	for _, dependency := range final.Dependencies {
		if dependency.DependsOn == doneTaskID && !containsString(body.RequiredBy, dependency.TaskID) {
			t.Fatalf("task %q required_by = %#v, want it to contain %q", doneTaskID, body.RequiredBy, dependency.TaskID)
		}
	}

	missing := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+boardID+"/tasks/does-not-exist", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown task status = %d", missing.Code)
	}
}

func TestUnknownBoardReturns404ForSnapshotAndTaskDetail(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store := newBoardTestServer(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })

	snapshotResponse := doRequest(t, handler, http.MethodGet, "/api/v1/boards/board:does-not-exist", nil)
	if snapshotResponse.Code != http.StatusNotFound {
		t.Fatalf("unknown board snapshot status = %d", snapshotResponse.Code)
	}
	taskResponse := doRequest(t, handler, http.MethodGet, "/api/v1/boards/board:does-not-exist/tasks/task:whatever", nil)
	if taskResponse.Code != http.StatusNotFound {
		t.Fatalf("unknown board task detail status = %d", taskResponse.Code)
	}
}

func TestBoardSurvivesAPIProcessRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handlerA, storeA := newBoardTestServer(t, dbPath, nil)

	created := doRequest(t, handlerA, http.MethodPost, "/api/v1/goals", goalBody("cmd-restart", chineseDemoObjective, t.TempDir()))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var createdResponse goalCreateResponse
	decodeInto(t, created, &createdResponse)

	before := doRequest(t, handlerA, http.MethodGet, "/api/v1/boards/"+createdResponse.Board.BoardID, nil)
	var beforeSnapshot boardSnapshot
	decodeInto(t, before, &beforeSnapshot)
	if err := storeA.Close(); err != nil {
		t.Fatalf("close store A: %v", err)
	}

	handlerB, storeB := newBoardTestServer(t, dbPath, nil)
	t.Cleanup(func() { _ = storeB.Close() })

	after := doRequest(t, handlerB, http.MethodGet, "/api/v1/boards/"+createdResponse.Board.BoardID, nil)
	if after.Code != http.StatusOK {
		t.Fatalf("reopened snapshot status = %d, body = %s", after.Code, after.Body.String())
	}
	var afterSnapshot boardSnapshot
	decodeInto(t, after, &afterSnapshot)
	if afterSnapshot.BoardID != beforeSnapshot.BoardID || afterSnapshot.Version != beforeSnapshot.Version || afterSnapshot.Goal.Objective != beforeSnapshot.Goal.Objective {
		t.Fatalf("snapshot after restart = %#v, before = %#v", afterSnapshot, beforeSnapshot)
	}
	if !reflect.DeepEqual(taskIDSet(afterSnapshot), taskIDSet(beforeSnapshot)) {
		t.Fatalf("task set changed across restart: before=%#v after=%#v", taskIDSet(beforeSnapshot), taskIDSet(afterSnapshot))
	}
}

func TestNoProviderSecretLeaksAcrossEndpoints(t *testing.T) {
	t.Setenv(providerSecretEnvVarName, providerSecretSentinel)
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store := newBoardTestServer(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })

	created := doRequest(t, handler, http.MethodPost, "/api/v1/goals", goalBody("cmd-secret", chineseDemoObjective, t.TempDir()))
	assertNoSentinelLeak(t, created)
	var createdResponse goalCreateResponse
	decodeInto(t, created, &createdResponse)
	boardID := createdResponse.Board.BoardID

	snapshotResponse := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+boardID, nil)
	assertNoSentinelLeak(t, snapshotResponse)

	var probeTaskID string
	for _, column := range createdResponse.Board.Columns {
		if len(column.Tasks) > 0 {
			probeTaskID = column.Tasks[0].ID
			break
		}
	}
	if probeTaskID == "" {
		t.Fatalf("no task available on created board %#v", createdResponse.Board)
	}
	taskResponse := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+boardID+"/tasks/"+probeTaskID, nil)
	assertNoSentinelLeak(t, taskResponse)

	pauseResponse := doRequest(t, handler, http.MethodPost, "/api/v1/boards/"+boardID+"/pause", map[string]any{"command_id": "pause-secret", "reason": "用户暂停"})
	assertNoSentinelLeak(t, pauseResponse)

	providersResponseRecorder := doRequest(t, handler, http.MethodGet, "/api/v1/providers", nil)
	assertNoSentinelLeak(t, providersResponseRecorder)

	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/v1/boards/"+boardID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		for _, value := range values {
			if strings.Contains(value, providerSecretSentinel) {
				t.Fatalf("SSE response header %q leaked provider secret: %q", key, value)
			}
		}
	}
	buffer := make([]byte, 4096)
	n, _ := response.Body.Read(buffer)
	if strings.Contains(string(buffer[:n]), providerSecretSentinel) {
		t.Fatalf("SSE body leaked provider secret: %q", buffer[:n])
	}
}

func TestProvidersEndpointReturnsConfiguredProviders(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	wantProviders := []agoboardapi.Provider{{ID: "ago-demo-planner", Kind: "planner", Capabilities: []string{"planning"}, AuthConfigured: false}}
	handler, store := newBoardTestServer(t, dbPath, wantProviders)
	t.Cleanup(func() { _ = store.Close() })

	response := doRequest(t, handler, http.MethodGet, "/api/v1/providers", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("providers status = %d, body = %s", response.Code, response.Body.String())
	}
	var body providersResponse
	decodeInto(t, response, &body)
	if !reflect.DeepEqual(body.Providers, wantProviders) {
		t.Fatalf("providers = %#v, want %#v", body.Providers, wantProviders)
	}
}

// A retrying task must expose its retry accounting so a user can see why work
// is waiting and for how long, without ever exposing the fencing credential.
func TestTaskDetailExposesRetryAccountingWithoutLeakingTheFencingToken(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store, scheduler := newBoardTestServerWithFailingExecutor(t, dbPath)
	t.Cleanup(func() { _ = store.Close() })

	created := doRequest(t, handler, http.MethodPost, "/api/v1/goals", goalBody("cmd-retry-detail", chineseDemoObjective, t.TempDir()))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var response goalCreateResponse
	decodeInto(t, created, &response)
	boardID := response.Board.BoardID

	if _, err := scheduler.RunOnce(context.Background()); err != nil {
		t.Fatalf("scheduler cycle: %v", err)
	}
	snapshot := snapshotOf(t, handler, boardID)

	var retryingTaskID string
	for _, column := range snapshot.Columns {
		if column.Name != "Blocked" {
			continue
		}
		for _, task := range column.Tasks {
			if task.State == "retry-wait" {
				retryingTaskID = task.ID
			}
		}
	}
	if retryingTaskID == "" {
		t.Fatalf("no task entered retry-wait after a transient failure: %#v", snapshot.Columns)
	}

	detail := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+boardID+"/tasks/"+retryingTaskID, nil)
	if detail.Code != http.StatusOK {
		t.Fatalf("task detail status = %d, body = %s", detail.Code, detail.Body.String())
	}
	var body struct {
		State          string `json:"state"`
		AttemptCount   int    `json:"attempt_count"`
		MaxAttempts    int    `json:"max_attempts"`
		NextEligibleAt string `json:"next_eligible_at"`
		FailureClass   string `json:"failure_class"`
		BlockedReason  string `json:"blocked_reason"`
		Attempts       []struct {
			Number        int    `json:"number"`
			State         string `json:"state"`
			Generation    uint64 `json:"generation"`
			FailureClass  string `json:"failure_class"`
			FailureReason string `json:"failure_reason"`
		} `json:"attempts"`
		Leases []struct {
			State      string `json:"state"`
			Generation uint64 `json:"generation"`
			ExpiresAt  string `json:"expires_at"`
		} `json:"leases"`
	}
	decodeInto(t, detail, &body)

	if body.State != "retry-wait" || body.AttemptCount != 1 || body.MaxAttempts != 3 {
		t.Fatalf("retry accounting = %#v", body)
	}
	if body.NextEligibleAt == "" {
		t.Fatal("task detail does not expose when the retry becomes eligible")
	}
	if body.FailureClass != "transient" || body.BlockedReason == "" {
		t.Fatalf("failure classification = %q / %q", body.FailureClass, body.BlockedReason)
	}
	if len(body.Attempts) != 1 || body.Attempts[0].Number != 1 || body.Attempts[0].State != "failed" {
		t.Fatalf("attempt history = %#v", body.Attempts)
	}
	if body.Attempts[0].FailureClass != "transient" || body.Attempts[0].FailureReason == "" {
		t.Fatalf("attempt failure detail = %#v", body.Attempts[0])
	}
	if body.Attempts[0].Generation == 0 {
		t.Fatal("attempt detail does not expose its generation")
	}
	if len(body.Leases) != 1 || body.Leases[0].ExpiresAt == "" {
		t.Fatalf("lease detail = %#v", body.Leases)
	}
	// The credential itself must never reach a client.
	if strings.Contains(detail.Body.String(), "fencing_token") {
		t.Fatalf("task detail exposed a fencing token: %s", detail.Body.String())
	}
}

// newBoardTestServerWithFailingExecutor wires an executor that always fails
// transiently, so retry behaviour is observable through the API.
func newBoardTestServerWithFailingExecutor(t *testing.T, dbPath string) (http.Handler, *agoboardstore.Store, *agoscheduler.Scheduler) {
	t.Helper()
	store, err := agoboardstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC) }
	runtime := agoboardruntime.New(store, agoplanner.DemoPlanner{}, agoboardruntime.Options{
		CoordinatorID: apiCoordinatorID, WorkerID: apiWorkerID, VerifierID: apiVerifierID,
		LeaseDuration: time.Minute, Now: clock,
	})
	server, err := agoboardapi.New(agoboardapi.Options{Runtime: runtime, Store: store, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime, Executor: failingExecutor{}, Verifier: &fakeAPIVerifier{},
		CoordinatorID: apiCoordinatorID, WorkerID: apiWorkerID, VerifierID: apiVerifierID,
		LeaseDuration: time.Minute, Now: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server.Handler(), store, scheduler
}

type failingExecutor struct{}

func (failingExecutor) Execute(_ context.Context, _ agoboardruntime.Dispatch) (agoboardruntime.ExecutionResult, error) {
	return agoboardruntime.ExecutionResult{}, errors.New("执行器临时失败")
}
