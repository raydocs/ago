package agolocalexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"claudexflow/internal/agocoordinator"
	"claudexflow/internal/agoprotocol"
)

type helperObservation struct {
	Request agocoordinator.TurnRequest `json:"request"`
	CWD     string                     `json:"cwd"`
	Secret  string                     `json:"secret"`
}

func TestProcessExecutorSendsRequestAndLimitedEnvironment(t *testing.T) {
	t.Setenv("AGO_TEST_SECRET", "must-not-leak")
	workspace := t.TempDir()
	observationPath := filepath.Join(t.TempDir(), "observation.json")
	executor := ProcessExecutor{
		Command: os.Args[0],
		Args:    []string{"-test.run=^TestProcessHelper$", "ago-helper", "observe", observationPath},
	}
	request := agocoordinator.TurnRequest{
		ThreadID:  "thread-1",
		TurnID:    "turn-1",
		Content:   json.RawMessage(`{"text":"hello"}`),
		Workspace: workspace,
		Mode:      agoprotocol.AgentModeHigh,
		Executor:  agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal},
	}

	if err := executor.Run(context.Background(), request); err != nil {
		t.Fatalf("run executor: %v", err)
	}
	data, err := os.ReadFile(observationPath)
	if err != nil {
		t.Fatalf("read helper observation: %v", err)
	}
	var observation helperObservation
	if err := json.Unmarshal(data, &observation); err != nil {
		t.Fatalf("decode helper observation: %v", err)
	}
	if observation.Request.ThreadID != request.ThreadID || observation.Request.TurnID != request.TurnID {
		t.Fatalf("helper got wrong request: %+v", observation.Request)
	}
	if string(observation.Request.Content) != string(request.Content) {
		t.Fatalf("helper content = %s, want %s", observation.Request.Content, request.Content)
	}
	if observation.Request.Mode != request.Mode || observation.Request.Executor != request.Executor {
		t.Fatalf("helper execution metadata = mode %q executor %+v", observation.Request.Mode, observation.Request.Executor)
	}
	workspaceInfo, err := os.Stat(workspace)
	if err != nil {
		t.Fatalf("stat workspace: %v", err)
	}
	observedInfo, err := os.Stat(observation.CWD)
	if err != nil {
		t.Fatalf("stat helper cwd: %v", err)
	}
	if !os.SameFile(workspaceInfo, observedInfo) {
		t.Fatalf("helper cwd = %q, want same directory as %q", observation.CWD, workspace)
	}
	if observation.Secret != "" {
		t.Fatal("ambient secret leaked into executor environment")
	}
}

func TestProcessExecutorCancellationDoesNotWaitForKillGraceAfterExit(t *testing.T) {
	readyPath := filepath.Join(t.TempDir(), "ready")
	executor := ProcessExecutor{
		Command:   os.Args[0],
		Args:      []string{"-test.run=^TestProcessHelper$", "ago-helper", "wait", readyPath},
		KillGrace: 2 * time.Second,
	}
	request := agocoordinator.TurnRequest{
		ThreadID:  "thread-1",
		TurnID:    "turn-1",
		Workspace: t.TempDir(),
		Mode:      agoprotocol.AgentModeMedium,
		Executor:  agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- executor.Run(ctx, request) }()
	waitForFile(t, readyPath)

	started := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation error = %v", err)
		}
		if elapsed := time.Since(started); elapsed >= executor.KillGrace {
			t.Fatalf("cancellation waited for full kill grace: %v", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("executor did not stop after cancellation")
	}
}

func TestProcessExecutorReturnsChildFailure(t *testing.T) {
	executor := ProcessExecutor{Command: os.Args[0], Args: []string{"-test.run=^TestProcessHelper$", "ago-helper", "fail"}}
	err := executor.Run(context.Background(), agocoordinator.TurnRequest{Workspace: t.TempDir()})
	if err == nil {
		t.Fatal("expected child failure")
	}
}

func TestProcessHelper(t *testing.T) {
	marker := -1
	for index, argument := range os.Args {
		if argument == "ago-helper" {
			marker = index
			break
		}
	}
	if marker < 0 {
		t.Skip("helper process only")
	}
	mode := os.Args[marker+1]
	switch mode {
	case "observe":
		var request agocoordinator.TurnRequest
		if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		cwd, err := os.Getwd()
		if err != nil {
			t.Fatalf("get cwd: %v", err)
		}
		data, err := json.Marshal(helperObservation{
			Request: request,
			CWD:     cwd,
			Secret:  os.Getenv("AGO_TEST_SECRET"),
		})
		if err != nil {
			t.Fatalf("encode observation: %v", err)
		}
		if err := os.WriteFile(os.Args[marker+2], data, 0o600); err != nil {
			t.Fatalf("write observation: %v", err)
		}
	case "wait":
		if err := os.WriteFile(os.Args[marker+2], nil, 0o600); err != nil {
			t.Fatalf("write ready file: %v", err)
		}
		for {
			time.Sleep(time.Hour)
		}
	case "fail":
		os.Exit(23)
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper did not create %s", path)
}
