package agoverifier

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"claudexflow/internal/agolocalexec"
)

func TestSeatbeltExecutorBuildsBoundedReadOnlyWorkspacePlan(t *testing.T) {
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var captured agolocalexec.LaunchPlan
	executor := SeatbeltExecutor{
		Supervisor: "/usr/bin/true",
		ReadRoots:  []string{"/usr/share"},
		Environment: map[string]string{
			"GOTOOLCHAIN": "local",
		},
		broker: func(_ context.Context, supervisor string, plan agolocalexec.LaunchPlan) (agolocalexec.BrokerResult, error) {
			if supervisor != "/usr/bin/true" {
				t.Fatalf("supervisor = %q", supervisor)
			}
			captured = plan
			return agolocalexec.BrokerResult{
				ExitCode: 2,
				Stdout:   agolocalexec.CollectedOutput{Head: []byte("out"), TotalBytes: 3},
				Stderr:   agolocalexec.CollectedOutput{Head: []byte("err"), TotalBytes: 3},
			}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := executor.Execute(ctx, ExecutionRequest{
		Workspace: workspace, Executable: "/usr/bin/true", Args: []string{"arg"}, MaxOutputBytes: 8,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if result.ExitCode != 2 || string(result.Output) != "stdout:\nout\nstderr:\nerr" {
		t.Fatalf("Execute() = %#v", result)
	}
	if captured.WorkingDir != workspace || captured.Executable != "/usr/bin/true" || !reflect.DeepEqual(captured.Arguments, []string{"arg"}) {
		t.Fatalf("launch target = %#v", captured)
	}
	if captured.Network != agolocalexec.NetworkDisabled || captured.TTY || len(captured.WriteRoots) != 0 || captured.Output.HeadBytes != 4 || captured.Output.TailBytes != 4 {
		t.Fatalf("sandbox bounds = %#v", captured)
	}
	if !contains(captured.ReadRoots, workspace) || !contains(captured.ReadRoots, "/usr/share") || captured.ProfileHash == "" || captured.ApprovalNonce == "" {
		t.Fatalf("sandbox identity/read roots = %#v", captured)
	}
	if captured.Environment["GOTOOLCHAIN"] != "local" || captured.Environment["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("sandbox environment = %#v", captured.Environment)
	}
	if !strings.HasPrefix(captured.Environment["GOCACHE"], captured.SyntheticTemp+string(filepath.Separator)) {
		t.Fatalf("GOCACHE is outside writable synthetic temp: %#v", captured.Environment)
	}
	jobRoot := filepath.Dir(captured.SyntheticHome)
	canonicalTemp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(jobRoot, canonicalTemp+string(filepath.Separator)) {
		t.Fatalf("synthetic job root = %q", jobRoot)
	}
	if _, err := os.Stat(jobRoot); !os.IsNotExist(err) {
		t.Fatalf("job root was not cleaned up: %v", err)
	}
}

func TestSeatbeltExecutorFailsClosedWithoutCanonicalSupervisor(t *testing.T) {
	executor := SeatbeltExecutor{Supervisor: "ago-supervisor"}
	_, err := executor.Execute(context.Background(), ExecutionRequest{
		Workspace: t.TempDir(), Executable: "/usr/bin/true", MaxOutputBytes: 8,
	})
	if err == nil {
		t.Fatal("Execute() accepted a relative supervisor")
	}
}

func TestSeatbeltExecutorRunsHarmlessCommandThroughRealSupervisor(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	repository, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	supervisor := filepath.Join(t.TempDir(), "ago-supervisor")
	build := exec.Command("go", "build", "-o", supervisor, "./cmd/ago-supervisor")
	build.Dir = repository
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build supervisor: %v\n%s", err, output)
	}
	supervisor, err = filepath.EvalSymlinks(supervisor)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := (SeatbeltExecutor{Supervisor: supervisor}).Execute(ctx, ExecutionRequest{
		ThreadID: "thread", TurnID: "turn", ToolCallID: "tool", Workspace: workspace,
		Executable: "/usr/bin/true", MaxOutputBytes: 128,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Execute() = %#v, %v", result, err)
	}
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
