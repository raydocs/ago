package agolocalexec

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDecodeLaunchEnvelopeRejectsUnknownTrailingVersionAndDigest(t *testing.T) {
	p := brokerTestPlan(t)
	envelope, err := NewLaunchEnvelope(p)
	if err != nil {
		t.Fatal(err)
	}
	valid, _ := json.Marshal(envelope)
	tests := map[string][]byte{
		"unknown":  append(valid[:len(valid)-1], []byte(`,"surprise":true}`)...),
		"trailing": append(append([]byte{}, valid...), []byte(` {}`)...),
		"version":  bytes.Replace(valid, []byte(`"version":2`), []byte(`"version":1`), 1),
		"digest":   bytes.Replace(valid, []byte(envelope.Digest), []byte(strings.Repeat("0", 64)), 1),
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeLaunchEnvelope(bytes.NewReader(input)); err == nil {
				t.Fatal("accepted invalid envelope")
			}
		})
	}
}

func TestDecodeLaunchEnvelopeIsBounded(t *testing.T) {
	if _, err := DecodeLaunchEnvelope(strings.NewReader(strings.Repeat(" ", maxLaunchEnvelopeBytes+1))); err == nil {
		t.Fatal("accepted oversized envelope")
	}
}

func TestSupervisorRejectsProfileHashMismatch(t *testing.T) {
	p := brokerTestPlan(t)
	p.ProfileHash = strings.Repeat("0", 64)
	envelope, err := NewLaunchEnvelope(p)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(envelope)
	if err := RunSupervisor(bytes.NewReader(input), io.Discard, strings.NewReader("")); err == nil || !strings.Contains(err.Error(), "profile hash mismatch") {
		t.Fatalf("RunSupervisor error = %v", err)
	}
}

func TestSupervisorCommandSuccessAndBoundedOutput(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	p := brokerTestPlan(t)
	p.Executable = "/usr/bin/printf"
	p.Arguments = []string{"0123456789"}
	p.ReadRoots = []string{p.WorkingDir}
	p.Output = OutputBudget{HeadBytes: 3, TailBytes: 2}
	p.Deadline = 2 * time.Second
	p, err := BindSeatbeltProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	envelope, _ := NewLaunchEnvelope(p)
	input, _ := json.Marshal(envelope)
	var output bytes.Buffer
	liveR, liveW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer liveR.Close()
	defer liveW.Close()
	if err := RunSupervisor(bytes.NewReader(input), &output, liveR); err != nil {
		t.Fatal(err)
	}
	var result BrokerResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Stdout.Head) != "012" || string(result.Stdout.Tail) != "89" || result.Stdout.DroppedBytes != 5 {
		t.Fatalf("result: %+v", result)
	}
}

func TestSupervisorPassesOnlyDigestBoundStdin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	p := brokerTestPlan(t)
	p.Executable = "/bin/cat"
	p.Arguments = nil
	p.Stdin = []byte("bound input")
	p.ReadRoots = []string{p.WorkingDir}
	p.Output = OutputBudget{HeadBytes: 32, TailBytes: 32}
	p, err := BindSeatbeltProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	envelope, _ := NewLaunchEnvelope(p)
	input, _ := json.Marshal(envelope)
	var output bytes.Buffer
	liveR, liveW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer liveR.Close()
	defer liveW.Close()
	if err := RunSupervisor(bytes.NewReader(input), &output, liveR); err != nil {
		t.Fatal(err)
	}
	var result BrokerResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || string(result.Stdout.Head) != "bound input" {
		t.Fatalf("result: %+v", result)
	}
}

func TestSupervisorDeniesReadsOutsideDeclaredRoots(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	p := brokerTestPlan(t)
	external := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(external, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	p.Executable = "/bin/cat"
	p.Arguments = []string{external}
	p.ReadRoots = []string{p.WorkingDir}
	p, err := BindSeatbeltProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	result := runSupervisorPlan(t, p)
	if result.ExitCode == 0 || strings.Contains(string(result.Stdout.Head), "secret") {
		t.Fatalf("external read escaped sandbox: %+v", result)
	}
}

func TestSupervisorAllowsReadsInsideDeclaredWorkspace(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	p := brokerTestPlan(t)
	inside := filepath.Join(p.WorkingDir, "allowed.txt")
	if err := os.WriteFile(inside, []byte("allowed"), 0600); err != nil {
		t.Fatal(err)
	}
	p.Executable = "/bin/cat"
	p.Arguments = []string{inside}
	p.ReadRoots = []string{p.WorkingDir}
	p.Output = OutputBudget{HeadBytes: 32, TailBytes: 32}
	p, err := BindSeatbeltProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	result := runSupervisorPlan(t, p)
	if result.ExitCode != 0 || string(result.Stdout.Head) != "allowed" {
		t.Fatalf("declared workspace read failed: %+v", result)
	}
}

func TestSupervisorDeniesNetwork(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	p := brokerTestPlan(t)
	p.Executable = "/usr/bin/nc"
	p.Arguments = []string{"-w", "1", "127.0.0.1", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)}
	p.ReadRoots = []string{p.WorkingDir}
	p, err = BindSeatbeltProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	result := runSupervisorPlan(t, p)
	if result.ExitCode == 0 {
		t.Fatal("network connection escaped sandbox")
	}
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Millisecond))
	if connection, err := listener.Accept(); err == nil {
		_ = connection.Close()
		t.Fatal("sandboxed process reached listener")
	}
}

func runSupervisorPlan(t *testing.T, plan LaunchPlan) BrokerResult {
	t.Helper()
	envelope, err := NewLaunchEnvelope(plan)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(envelope)
	var output bytes.Buffer
	liveR, liveW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer liveR.Close()
	defer liveW.Close()
	if err := RunSupervisor(bytes.NewReader(input), &output, liveR); err != nil {
		t.Fatal(err)
	}
	var result BrokerResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestSupervisorLivenessClosureCancelsCommand(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	p := brokerTestPlan(t)
	p.Executable = "/bin/sleep"
	p.Arguments = []string{"30"}
	p.ReadRoots = []string{p.WorkingDir}
	p.Deadline = time.Minute
	p, err := BindSeatbeltProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	envelope, _ := NewLaunchEnvelope(p)
	input, _ := json.Marshal(envelope)
	var output bytes.Buffer
	liveR, liveW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer liveR.Close()
	time.AfterFunc(100*time.Millisecond, func() { _ = liveW.Close() })
	started := time.Now()
	if err := RunSupervisor(bytes.NewReader(input), &output, liveR); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("liveness cleanup took %v", elapsed)
	}
	var result BrokerResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Fatal("canceled command reported success")
	}
}

func TestSupervisorDeadlineCancelsCommand(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	p := brokerTestPlan(t)
	p.Executable = "/bin/sleep"
	p.Arguments = []string{"30"}
	p.ReadRoots = []string{p.WorkingDir}
	p.Deadline = 100 * time.Millisecond
	p, err := BindSeatbeltProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result := runSupervisorPlan(t, p)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("deadline cleanup took %v", elapsed)
	}
	if result.ExitCode == 0 {
		t.Fatal("deadline-canceled command reported success")
	}
}
