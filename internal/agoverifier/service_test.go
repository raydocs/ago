package agoverifier

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
)

func TestServiceRunsCatalogCheckAndRecordsTruth(t *testing.T) {
	store, threadID, workspace := testStore(t)
	executor := &fakeExecutor{result: ExecutionResult{ExitCode: 0, Output: []byte("ok\n")}}
	service := New(store, store, StaticCatalog{
		"unit": {Executable: "/usr/bin/true", Args: []string{"--unit"}, Timeout: time.Second},
	}, executor, Limits{})

	result, err := service.Run(context.Background(), Request{
		ThreadID: threadID, TurnID: "turn-1", ToolCallID: "tool-1", IdempotencyKey: "verify-1", CheckID: "unit",
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Status != agothreadstore.VerificationPassed || result.IdempotencyKey != "verify-1:final" {
		t.Fatalf("Run() = %#v, want passed final record", result)
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if executor.calls != 1 || executor.request.Workspace != canonicalWorkspace || executor.request.Executable != "/usr/bin/true" || !reflect.DeepEqual(executor.request.Args, []string{"--unit"}) {
		t.Fatalf("executor request = %#v, calls = %d", executor.request, executor.calls)
	}
	checks, err := store.VerificationChecks(context.Background(), threadID)
	if err != nil || len(checks) != 2 {
		t.Fatalf("VerificationChecks() = %#v, %v", checks, err)
	}
	if checks[0].Status != agothreadstore.VerificationUnknown || checks[0].OutputSummary != "running; turn=turn-1; tool_call=tool-1" {
		t.Fatalf("running record = %#v", checks[0])
	}
	if checks[1].Status != agothreadstore.VerificationPassed || checks[1].OutputSummary != "exit 0; output: ok\n" {
		t.Fatalf("final record = %#v", checks[1])
	}
}

func TestServiceRetryAndRaceExecuteOnlyOnce(t *testing.T) {
	store, threadID, _ := testStore(t)
	executor := &fakeExecutor{result: ExecutionResult{Output: []byte("done")}, entered: make(chan struct{}), release: make(chan struct{})}
	service := New(store, store, StaticCatalog{"unit": {Executable: "/usr/bin/true"}}, executor, Limits{})
	secondService := New(store, store, StaticCatalog{"unit": {Executable: "/usr/bin/true"}}, executor, Limits{})
	request := Request{ThreadID: threadID, TurnID: "turn", ToolCallID: "tool", IdempotencyKey: "same", CheckID: "unit"}

	results := make(chan agothreadstore.VerificationCheck, 2)
	errors := make(chan error, 2)
	for _, runner := range []*Service{service, secondService} {
		go func() {
			result, err := runner.Run(context.Background(), request)
			results <- result
			errors <- err
		}()
	}
	<-executor.entered
	close(executor.release)
	first, second := <-results, <-results
	if err := <-errors; err != nil {
		t.Fatalf("first Run() error = %v", err)
	}
	if err := <-errors; err != nil {
		t.Fatalf("second Run() error = %v", err)
	}
	if executor.calls != 1 || !reflect.DeepEqual(first, second) {
		t.Fatalf("calls = %d, results = %#v / %#v", executor.calls, first, second)
	}

	changed := request
	changed.ToolCallID = "other-tool"
	if _, err := service.Run(context.Background(), changed); !IsConflict(err) {
		t.Fatalf("changed retry error = %v, want conflict", err)
	}
	changed = request
	changed.CheckID = "client-selected-executable"
	if _, err := service.Run(context.Background(), changed); !IsConflict(err) {
		t.Fatalf("changed check retry error = %v, want conflict", err)
	}
	if executor.calls != 1 {
		t.Fatalf("changed retry executed check; calls = %d", executor.calls)
	}
}

func TestServiceRecordsFailureCancellationAndBoundedOutput(t *testing.T) {
	tests := []struct {
		name       string
		result     ExecutionResult
		err        error
		cancel     bool
		wantOutput string
	}{
		{name: "exit failure", result: ExecutionResult{ExitCode: 2, Output: []byte("bad")}, wantOutput: "exit 2; output: bad"},
		{name: "executor failure", err: errors.New("sandbox unavailable"), wantOutput: "executor failure: sandbox unavailable"},
		{name: "cancellation", cancel: true, err: context.Canceled, wantOutput: "canceled: context canceled"},
		{name: "bounded output", result: ExecutionResult{ExitCode: 1, Output: []byte("0123456789")}, wantOutput: "exit 1; output: 01234 [truncated]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, threadID, _ := testStore(t)
			executor := &fakeExecutor{result: test.result, err: test.err}
			service := New(store, store, StaticCatalog{"unit": {Executable: "/usr/bin/false"}}, executor, Limits{MaxOutputBytes: 5})
			ctx := context.Background()
			if test.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			result, err := service.Run(ctx, Request{ThreadID: threadID, TurnID: "turn", ToolCallID: "tool", IdempotencyKey: "failure", CheckID: "unit"})
			if err == nil {
				t.Fatal("Run() error = nil, want execution error")
			}
			if result.Status != agothreadstore.VerificationFailed || result.OutputSummary != test.wantOutput {
				t.Fatalf("Run() = %#v, want failed summary %q", result, test.wantOutput)
			}
		})
	}
}

func TestServiceTimeoutIsBoundedAndRecorded(t *testing.T) {
	store, threadID, _ := testStore(t)
	executor := &fakeExecutor{release: make(chan struct{})}
	service := New(store, store, StaticCatalog{
		"slow": {Executable: "/usr/bin/true", Timeout: time.Hour},
	}, executor, Limits{MaxTimeout: 10 * time.Millisecond})

	result, err := service.Run(context.Background(), Request{
		ThreadID: threadID, TurnID: "turn", ToolCallID: "tool", IdempotencyKey: "timeout", CheckID: "slow",
	})
	if err == nil {
		t.Fatal("Run() error = nil, want timeout error")
	}
	if result.Status != agothreadstore.VerificationFailed || result.OutputSummary != "timed out: context deadline exceeded" {
		t.Fatalf("Run() = %#v, want recorded timeout", result)
	}
	checks, ledgerErr := store.VerificationChecks(context.Background(), threadID)
	if ledgerErr != nil || len(checks) != 2 || checks[1] != result {
		t.Fatalf("VerificationChecks() = %#v, %v; want terminal timeout", checks, ledgerErr)
	}
}

func TestServiceLostResponseRetrySurvivesStoreReopen(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "threads.db")
	workspace := t.TempDir()
	store, err := agothreadstore.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	threadID := createThread(t, store, workspace)
	firstExecutor := &fakeExecutor{result: ExecutionResult{Output: []byte("persisted")}}
	request := Request{ThreadID: threadID, TurnID: "turn", ToolCallID: "tool", IdempotencyKey: "lost", CheckID: "unit"}
	first, err := New(store, store, StaticCatalog{"unit": {Executable: "/usr/bin/true"}}, firstExecutor, Limits{}).Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := agothreadstore.Open(database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	secondExecutor := &fakeExecutor{}
	retry, err := New(reopened, reopened, StaticCatalog{"unit": {Executable: "/usr/bin/true"}}, secondExecutor, Limits{}).Run(context.Background(), request)
	if err != nil || !reflect.DeepEqual(retry, first) || secondExecutor.calls != 0 {
		t.Fatalf("reopened retry = %#v, %v; first = %#v, calls = %d", retry, err, first, secondExecutor.calls)
	}
}

type fakeExecutor struct {
	mu      sync.Mutex
	calls   int
	request ExecutionRequest
	result  ExecutionResult
	err     error
	entered chan struct{}
	release chan struct{}
}

func (executor *fakeExecutor) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	executor.mu.Lock()
	executor.calls++
	executor.request = request
	if executor.entered != nil && executor.calls == 1 {
		close(executor.entered)
	}
	executor.mu.Unlock()
	if executor.release != nil {
		select {
		case <-executor.release:
		case <-ctx.Done():
			return ExecutionResult{}, ctx.Err()
		}
	}
	return executor.result, executor.err
}

func testStore(t *testing.T) (*agothreadstore.Store, string, string) {
	t.Helper()
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "threads.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	return store, createThread(t, store, workspace), workspace
}

func createThread(t *testing.T, store *agothreadstore.Store, workspace string) string {
	t.Helper()
	result, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: "create", IdempotencyKey: "create", ActorID: "test", Type: agoprotocol.CommandThreadCreate,
	}, agothreadstore.ThreadSpec{Workspace: workspace, Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	return result.ThreadID
}
