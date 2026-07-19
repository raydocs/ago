package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestFreshBuiltDaemonResumesAfterCrashAndReconnectsWithoutDuplicateMutation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("real Seatbelt supervisor is macOS-only")
	}
	if testing.Short() {
		t.Skip("fresh binary integration test")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	temporary := t.TempDir()
	ago := filepath.Join(temporary, "ago")
	supervisor := filepath.Join(temporary, "ago-supervisor")
	helper := filepath.Join(temporary, "localexec.test")
	build := func(arguments ...string) {
		command := exec.Command("go", arguments...)
		command.Dir = root
		if output, buildErr := command.CombinedOutput(); buildErr != nil {
			t.Fatalf("%v: %v\n%s", arguments, buildErr, output)
		}
	}
	build("build", "-o", ago, "./cmd/ago")
	build("build", "-o", supervisor, "./cmd/ago-supervisor")
	build("test", "-c", "-o", helper, "./internal/agolocalexec")
	sidecar := filepath.Join(temporary, "deterministic-sidecar")
	script := "#!/bin/sh\nexec \"$1\" -test.run=^TestBrokerDuplexHelper$ -- sidecar-slow\n"
	if err := os.WriteFile(sidecar, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	runtimeDirectory, err := os.MkdirTemp("/tmp", "ago-restart-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDirectory) })
	database, socket := filepath.Join(runtimeDirectory, "ago.db"), filepath.Join(runtimeDirectory, "ago.sock")
	pluginRuntime := filepath.Join(root, "plugin-runtime", "main.ts")
	startDaemon := func() *exec.Cmd {
		command := exec.Command(ago, "daemon", "--db", database, "--socket", socket,
			"--executor-command", sidecar, "--executor-entry", helper,
			"--supervisor-command", supervisor, "--plugin-runtime", pluginRuntime,
			"--context-window-tokens", "100", "--reserved-output-tokens", "10")
		command.Dir = root
		command.Stdout = os.Stderr
		command.Stderr = os.Stderr
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		return command
	}
	var daemon *exec.Cmd
	t.Cleanup(func() {
		if daemon != nil && daemon.Process != nil {
			_ = daemon.Process.Kill()
			_ = daemon.Wait()
		}
	})
	runCLI := func(arguments ...string) []byte {
		arguments = append(arguments, "--socket", socket)
		command := exec.Command(ago, arguments...)
		command.Dir = root
		output, commandErr := command.CombinedOutput()
		if commandErr != nil {
			t.Fatalf("ago %v: %v\n%s", arguments, commandErr, output)
		}
		return output
	}
	waitReady := func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(socket); err == nil {
				command := exec.Command(ago, "list", "--socket", socket)
				if command.Run() == nil {
					return
				}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("daemon did not become ready")
	}
	daemon = startDaemon()
	waitReady()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, ".ago-capture-initialize"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var created struct {
		ThreadID string `json:"thread_id"`
	}
	if err := json.Unmarshal(runCLI("create", "--title", "restart-e2e", "--project", "restart-e2e", "--workspace", workspace, "--content", "hello"), &created); err != nil || created.ThreadID == "" {
		t.Fatalf("create response = %#v, %v", created, err)
	}
	type replayResponse struct {
		Events []struct {
			Type string `json:"type"`
		} `json:"events"`
	}
	replay := func() replayResponse {
		var response replayResponse
		if err := json.Unmarshal(runCLI("replay", "--thread", created.ThreadID), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	waitFor := func(eventType string) replayResponse {
		deadline := time.Now().Add(8 * time.Second)
		for time.Now().Before(deadline) {
			response := replay()
			for _, event := range response.Events {
				if event.Type == eventType {
					return response
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("event %q was not persisted", eventType)
		return replayResponse{}
	}
	waitFor("agent.started")
	if err := daemon.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = daemon.Wait()
	daemon = startDaemon()
	waitReady()
	finished := waitFor("turn.completed")
	counts := map[string]int{}
	for _, event := range finished.Events {
		counts[event.Type]++
	}
	if counts["message.accepted"] != 1 || counts["agent.started"] != 2 || counts["turn.completed"] != 1 || counts["turn.failed"] != 0 {
		t.Fatalf("restart event counts = %#v", counts)
	}
	runCLI("submit", "--thread", created.ThreadID, "--content", "hello", "--idempotency-key", "post-restart-submit")
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		finished = replay()
		counts = map[string]int{}
		for _, event := range finished.Events {
			counts[event.Type]++
		}
		if counts["turn.completed"] == 2 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if counts["message.accepted"] != 2 || counts["agent.started"] != 3 || counts["turn.completed"] != 2 || counts["compaction.recorded"] != 2 || counts["turn.failed"] != 0 {
		t.Fatalf("post-restart compaction event counts = %#v", counts)
	}
	captured, err := os.ReadFile(filepath.Join(workspace, ".ago-captured-initialize.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(captured), []byte{'\n'})
	if len(lines) < 3 {
		t.Fatalf("captured initialize attempts = %d, want crash, resume, and post-compaction", len(lines))
	}
	var initialize struct {
		Transcript []json.RawMessage `json:"transcript"`
	}
	if err := json.Unmarshal(lines[len(lines)-1], &initialize); err != nil {
		t.Fatal(err)
	}
	encodedTranscript, _ := json.Marshal(initialize.Transcript)
	if len(initialize.Transcript) == 0 || !bytes.Contains(encodedTranscript, []byte("AGO_RECOVERY_V2")) {
		t.Fatalf("post-restart sidecar did not receive persisted structured compaction transcript: %s", encodedTranscript)
	}
}
