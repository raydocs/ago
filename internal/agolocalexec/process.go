package agolocalexec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"claudexflow/internal/agocoordinator"
)

type ProcessExecutor struct {
	Command   string
	Args      []string
	KillGrace time.Duration
}

func (executor ProcessExecutor) Run(ctx context.Context, request agocoordinator.TurnRequest) error {
	if strings.TrimSpace(executor.Command) == "" {
		return fmt.Errorf("local executor command is not configured")
	}
	if executor.KillGrace <= 0 {
		executor.KillGrace = 2 * time.Second
	}
	command := exec.Command(executor.Command, executor.Args...)
	command.Dir = request.Workspace
	command.Env = executorEnvironment(request)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := command.StdinPipe()
	if err != nil {
		return fmt.Errorf("open executor stdin: %w", err)
	}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start local executor: %w", err)
	}
	if err := json.NewEncoder(stdin).Encode(request); err != nil {
		_ = stdin.Close()
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
		return fmt.Errorf("send turn to local executor: %w", err)
	}
	_ = stdin.Close()

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("local executor: %w", err)
		}
		return nil
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(executor.KillGrace)
		defer timer.Stop()
		select {
		case <-done:
			return ctx.Err()
		case <-timer.C:
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-done
			return ctx.Err()
		}
	}
}

func executorEnvironment(request agocoordinator.TurnRequest) []string {
	allowed := []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "SHELL"}
	environment := make([]string, 0, len(allowed)+3)
	for _, key := range allowed {
		if value, ok := os.LookupEnv(key); ok {
			environment = append(environment, key+"="+value)
		}
	}
	return append(environment,
		"AGO_THREAD_ID="+request.ThreadID,
		"AGO_TURN_ID="+request.TurnID,
		"AGO_AGENT_MODE="+string(request.Mode),
	)
}
