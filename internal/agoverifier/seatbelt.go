package agoverifier

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"claudexflow/internal/agolocalexec"
)

type brokerRunner func(context.Context, string, agolocalexec.LaunchPlan) (agolocalexec.BrokerResult, error)

type SeatbeltExecutor struct {
	Supervisor  string
	ReadRoots   []string
	Environment map[string]string
	broker      brokerRunner
}

func (executor SeatbeltExecutor) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	if !canonicalPath(executor.Supervisor) {
		return ExecutionResult{}, fmt.Errorf("verification supervisor must be canonical and absolute")
	}
	if !canonicalPath(request.Workspace) || !canonicalPath(request.Executable) || request.MaxOutputBytes < 2 {
		return ExecutionResult{}, fmt.Errorf("canonical verification workspace, executable, and bounded output are required")
	}
	for _, root := range executor.ReadRoots {
		if !canonicalPath(root) {
			return ExecutionResult{}, fmt.Errorf("verification read root must be canonical and absolute")
		}
	}
	jobRoot, err := os.MkdirTemp("", "ago-verification-")
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("create verification job root: %w", err)
	}
	rawJobRoot := jobRoot
	jobRoot, err = filepath.EvalSymlinks(rawJobRoot)
	if err != nil {
		_ = os.RemoveAll(rawJobRoot)
		return ExecutionResult{}, fmt.Errorf("resolve verification job root: %w", err)
	}
	defer os.RemoveAll(jobRoot)

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return ExecutionResult{}, fmt.Errorf("create verification approval nonce: %w", err)
	}
	deadline := 30 * time.Second
	if at, ok := ctx.Deadline(); ok {
		deadline = time.Until(at)
	}
	if deadline <= 0 {
		return ExecutionResult{}, ctx.Err()
	}
	environment := make(map[string]string, len(executor.Environment)+5)
	for key, value := range executor.Environment {
		environment[key] = value
	}
	environment["PATH"] = "/usr/bin:/bin"
	environment["LANG"] = "C.UTF-8"
	environment["GOCACHE"] = filepath.Join(jobRoot, "tmp", "go-build")
	environment["GOTMPDIR"] = filepath.Join(jobRoot, "tmp")
	readRoots := []string{
		request.Workspace, request.Executable, filepath.Dir(request.Executable), filepath.Dir(filepath.Dir(request.Executable)),
		"/System", "/usr/lib", "/usr/bin", "/bin", "/usr/share", "/private/var/db/dyld", "/dev/null", "/dev/urandom",
	}
	readRoots = append(readRoots, executor.ReadRoots...)
	halfOutput := request.MaxOutputBytes / 2
	plan := agolocalexec.LaunchPlan{
		Origin:        "server:verification/" + request.ThreadID + "/" + request.TurnID + "/" + request.ToolCallID,
		Executable:    request.Executable,
		Arguments:     append([]string(nil), request.Args...),
		WorkingDir:    request.Workspace,
		Environment:   environment,
		ReadRoots:     readRoots,
		WriteRoots:    nil,
		SyntheticHome: filepath.Join(jobRoot, "home"),
		SyntheticTemp: filepath.Join(jobRoot, "tmp"),
		ProfileID:     "ago.model.v1",
		Network:       agolocalexec.NetworkDisabled,
		Deadline:      deadline,
		Output:        agolocalexec.OutputBudget{HeadBytes: halfOutput, TailBytes: request.MaxOutputBytes - halfOutput},
		ApprovalNonce: hex.EncodeToString(nonce),
	}
	plan, err = agolocalexec.BindSeatbeltProfile(plan)
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("bind verification sandbox: %w", err)
	}
	run := executor.broker
	if run == nil {
		run = agolocalexec.ExecuteBroker
	}
	result, err := run(ctx, executor.Supervisor, plan)
	if err != nil {
		return ExecutionResult{}, err
	}
	return ExecutionResult{ExitCode: result.ExitCode, Output: joinedBrokerOutput(result)}, nil
}

func canonicalPath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}

func joinedBrokerOutput(result agolocalexec.BrokerResult) []byte {
	var output bytes.Buffer
	appendStream := func(name string, stream agolocalexec.CollectedOutput) {
		if stream.TotalBytes == 0 && len(stream.Head) == 0 && len(stream.Tail) == 0 {
			return
		}
		if output.Len() > 0 {
			output.WriteByte('\n')
		}
		fmt.Fprintf(&output, "%s:\n", name)
		output.Write(stream.Head)
		if stream.DroppedBytes > 0 {
			fmt.Fprintf(&output, "\n[%d bytes omitted]\n", stream.DroppedBytes)
		}
		output.Write(stream.Tail)
	}
	appendStream("stdout", result.Stdout)
	appendStream("stderr", result.Stderr)
	return []byte(strings.TrimSuffix(output.String(), "\n"))
}
