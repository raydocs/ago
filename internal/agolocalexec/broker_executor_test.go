package agolocalexec

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"claudexflow/internal/agocoordinator"
	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
)

func TestBrokerExecutorBuildsRestrictedDigestBoundTurnPlan(t *testing.T) {
	workspace := t.TempDir()
	executable := "/bin/cat"
	executor := BrokerExecutor{Command: executable, Arguments: []string{"-u"}, DefaultDeadline: time.Minute}
	request := agocoordinator.TurnRequest{
		ThreadID: "thread-1", TurnID: "turn-1", Content: json.RawMessage(`{"text":"hello"}`),
		Workspace: workspace, Mode: agoprotocol.AgentModeHigh, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal},
	}
	plan, cleanup, err := executor.buildPlan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	var decoded agocoordinator.TurnRequest
	if err := json.Unmarshal(plan.Stdin, &decoded); err != nil {
		t.Fatalf("decode plan stdin: %v", err)
	}
	if decoded.ThreadID != request.ThreadID || decoded.TurnID != request.TurnID {
		t.Fatalf("stdin request = %+v", decoded)
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if plan.WorkingDir != canonicalWorkspace || plan.Executable != executable || plan.Network != NetworkDisabled || plan.TTY {
		t.Fatalf("unsafe plan: %+v", plan)
	}
	if plan.Environment["AGO_THREAD_ID"] != request.ThreadID || plan.Environment["HOME"] != "" || plan.Environment["PATH"] != "/usr/bin:/bin" {
		t.Fatalf("environment = %#v", plan.Environment)
	}
	if plan.ProfileHash == "" || plan.ApprovalNonce == "" {
		t.Fatal("plan is not bound to profile and nonce")
	}
	if _, err := os.Stat(filepath.Dir(plan.SyntheticHome)); err != nil {
		t.Fatalf("job root was not created: %v", err)
	}
	cleanup()
	if _, err := os.Stat(filepath.Dir(plan.SyntheticHome)); !os.IsNotExist(err) {
		t.Fatalf("job root survived cleanup: %v", err)
	}
}

func TestBrokerExecutorRejectsRelativeSupervisorOrCommand(t *testing.T) {
	request := agocoordinator.TurnRequest{ThreadID: "thread", TurnID: "turn", Workspace: t.TempDir()}
	for name, executor := range map[string]BrokerExecutor{
		"supervisor": {Supervisor: "ago-supervisor", Command: "/usr/bin/true"},
		"command":    {Supervisor: "/usr/bin/true", Command: "tool"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := executor.Run(context.Background(), request); err == nil {
				t.Fatal("accepted relative trusted executable")
			}
		})
	}
}

func TestBrokerExecutorStartRequiresSessionConfigurationAndStrictText(t *testing.T) {
	request := agocoordinator.TurnRequest{ThreadID: "thread", TurnID: "turn", Workspace: t.TempDir(), Content: json.RawMessage(`{"text":"hello"}`)}
	base := BrokerExecutor{Supervisor: "/bin/true", Command: "/bin/cat"}
	if _, err := base.Start(context.Background(), request); err == nil {
		t.Fatal("Start accepted missing provider/model/protocol")
	}
	base.Provider, base.Model, base.Protocol = "provider", "model", "pi-jsonl-v1"
	request.Content = json.RawMessage(`{"text":"ok","extra":true}`)
	if _, err := base.Start(context.Background(), request); err == nil {
		t.Fatal("Start accepted unknown content field")
	}
}

func TestBrokerExecutorImplementsSessionExecutor(t *testing.T) {
	var _ agocoordinator.SessionExecutor = BrokerExecutor{}
}

func TestProviderBrokerRejectsUnknownAndOversizedRequests(t *testing.T) {
	callback := func(_ context.Context, _ ProviderRequest, _ func(ProviderResponse) error) error { return nil }
	for _, input := range []string{
		`{"type":"inference_request","id":"one","provider":"p","model":"m","context":{},"options":{},"extra":true}` + "\n",
		strings.Repeat("x", maxProviderFrameBytes+1) + "\n",
	} {
		if err := serveProviderBroker(context.Background(), strings.NewReader(input), io.Discard, callback); err == nil {
			t.Fatalf("accepted malformed provider input")
		}
	}
}

func TestFrameReadersRejectOversizedUnterminatedInputBeforeReadingPastBudget(t *testing.T) {
	tests := map[string]struct {
		max int
		run func(io.Reader) error
	}{
		"provider": {
			max: maxProviderFrameBytes,
			run: func(input io.Reader) error {
				return serveProviderBroker(context.Background(), input, io.Discard, func(_ context.Context, _ ProviderRequest, _ func(ProviderResponse) error) error { return nil })
			},
		},
		"supervisor events": {
			max: 32,
			run: func(input io.Reader) error {
				return proxyEventFrames(input, io.Discard, ProtocolBudget{MaxFrameBytes: 32, MaxEvents: 1, MaxEventBytes: 64})
			},
		},
		"broker events": {
			max: 32,
			run: func(input io.Reader) error {
				return scanFrames(input, ProtocolBudget{ID: "test", MaxFrameBytes: 32, MaxEvents: 1, MaxEventBytes: 64}, make(chan []byte))
			},
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			input := &unterminatedBudgetReader{remaining: test.max + 1}
			if err := test.run(input); err == nil || !strings.Contains(err.Error(), "exceeds budget") {
				t.Fatalf("oversized unterminated frame error = %v", err)
			}
			if input.readPastBudget {
				t.Fatal("reader requested input after the frame budget was exhausted")
			}
		})
	}
}

type unterminatedBudgetReader struct {
	remaining      int
	readPastBudget bool
}

func (reader *unterminatedBudgetReader) Read(target []byte) (int, error) {
	if reader.remaining == 0 {
		reader.readPastBudget = true
		return 0, fmt.Errorf("read past budget")
	}
	count := len(target)
	if count > reader.remaining {
		count = reader.remaining
	}
	for index := 0; index < count; index++ {
		target[index] = 'x'
	}
	reader.remaining -= count
	return count, nil
}

func TestPiWireMatchesFlatStrictSidecarProtocol(t *testing.T) {
	eventType, payload, err := decodePiEvent([]byte(`{"type":"text","delta":"hello"}`))
	if err != nil || eventType != "text" || !bytes.Equal(payload, []byte(`{"type":"text","delta":"hello"}`)) {
		t.Fatalf("decodePiEvent() = %q %s, %v", eventType, payload, err)
	}
	usage := `{"type":"assistant_completed","message":{"role":"assistant"},"provider_usage":{"provider":"openai","model":"gpt-test","request_id":"request-1","status":"final","usage":{"input_tokens":101,"output_tokens":23,"cache_read_tokens":47,"cache_write_tokens":11,"total_tokens":997,"cache_write_1h_tokens":3,"reasoning_tokens":7},"cost":{"input":0.00101,"output":0.00046,"cache_read":0.000047,"cache_write":0.00011,"total":9.99}}}`
	if eventType, payload, err := decodePiEvent([]byte(usage)); err != nil || eventType != "assistant_completed" || string(payload) != usage {
		t.Fatalf("decodePiEvent(provider usage) = %q %s, %v", eventType, payload, err)
	}
	for _, invalid := range []string{
		`{"type":"started","payload":{}}`,
		`{"type":"text"}`,
		`{"type":"settled","extra":true}`,
		`{"type":"assistant_completed","message":{"role":"assistant"}}`,
		`{"type":"assistant_completed","message":{"role":"assistant"},"provider_usage":{"provider":"openai"}}`,
	} {
		if _, _, err := decodePiEvent([]byte(invalid)); err == nil {
			t.Fatalf("decodePiEvent() accepted %s", invalid)
		}
	}
	abort, err := encodePiControl(agocoordinator.ExecutionControl{Type: "abort"})
	if err != nil || string(abort) != `{"type":"abort"}` {
		t.Fatalf("abort control = %s, %v", abort, err)
	}
	steer, err := encodePiControl(agocoordinator.ExecutionControl{Type: "steer", Payload: json.RawMessage(`{"text":"now"}`)})
	if err != nil || string(steer) != `{"text":"now","type":"steer"}` {
		t.Fatalf("steer control = %s, %v", steer, err)
	}
}

func TestPiBootstrapIncludesExactInitializeAndPrompt(t *testing.T) {
	bootstrap, err := piBootstrap("/canonical/workspace", "internal-provider", "internal-model", nil, nil, "hello")
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(bootstrap, []byte{'\n'}), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("bootstrap lines = %q", bootstrap)
	}
	var initialize map[string]any
	if err := json.Unmarshal(lines[0], &initialize); err != nil {
		t.Fatal(err)
	}
	if initialize["type"] != "initialize" || initialize["cwd"] != "/canonical/workspace" || initialize["provider"] != "internal-provider" || initialize["model"] != "internal-model" {
		t.Fatalf("initialize = %#v", initialize)
	}
	if len(initialize) != 6 || len(initialize["tools"].([]any)) != 0 {
		t.Fatalf("initialize has unexpected fields: %#v", initialize)
	}
	if string(lines[1]) != `{"text":"hello","type":"prompt"}` {
		t.Fatalf("prompt = %s", lines[1])
	}
}

func TestPiTranscriptProjectsSummaryTailAndOmitsCurrentPrompt(t *testing.T) {
	accepted := func(sequence uint64, turnID, text string) agoprotocol.Event {
		payload, _ := json.Marshal(map[string]any{"queue_item_id": "queue-" + turnID, "turn_id": turnID, "content": map[string]any{"text": text}})
		return agoprotocol.Event{SchemaVersion: agoprotocol.SchemaVersion, EventID: fmt.Sprintf("event-%d", sequence), ThreadID: "thread", Sequence: sequence, Type: agoprotocol.EventMessageAccepted, Visibility: agoprotocol.VisibilityUser, Payload: payload}
	}
	assistantMessage := json.RawMessage(`{"role":"assistant","api":"ago-faux","provider":"internal","model":"model","stopReason":"stop","content":[{"type":"text","text":"answer"}],"at":3}`)
	assistantPayload, _ := json.Marshal(map[string]any{"turn_id": "old", "executor_event_index": 2, "event": map[string]any{"type": "assistant_completed", "message": assistantMessage}})
	request := agocoordinator.TurnRequest{
		ThreadID: "thread", TurnID: "current", Content: json.RawMessage(`{"text":"now"}`),
		Context: agothreadstore.ContextProjection{
			Compaction: &agothreadstore.CompactionRecord{ThreadID: "thread", ThroughSequence: 1, Summary: "durable summary"},
			Tail: []agoprotocol.Event{
				accepted(2, "old", "prior"),
				{SchemaVersion: agoprotocol.SchemaVersion, EventID: "event-3", ThreadID: "thread", Sequence: 3, Type: agoprotocol.EventAssistantCompleted, Visibility: agoprotocol.VisibilityUser, Payload: assistantPayload},
				accepted(4, "current", "now"),
			},
		},
	}
	transcript, err := piTranscript(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) != 3 || !bytes.Contains(transcript[0], []byte(`"role":"summary"`)) || !bytes.Contains(transcript[1], []byte(`"text":"prior"`)) || !bytes.Equal(transcript[2], assistantMessage) {
		t.Fatalf("transcript = %s", transcript)
	}
	encodedTranscript, _ := json.Marshal(transcript)
	if bytes.Contains(encodedTranscript, []byte(`"text":"now"`)) {
		t.Fatalf("current prompt was duplicated in transcript: %s", transcript)
	}
	request.Content = json.RawMessage(`{"text":"different"}`)
	if _, err := piTranscript(request); err == nil {
		t.Fatal("accepted mismatched current prompt")
	}
}

func TestPiTranscriptStrictlyCorrelatesRegisteredPluginToolResult(t *testing.T) {
	request := agocoordinator.TurnRequest{
		ThreadID: "thread", TurnID: "current", Content: json.RawMessage(`{"text":"now"}`),
		Tools:   []agocoordinator.ExternalTool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		Context: currentPromptProjection("thread", "current", "now"),
	}
	assistant := json.RawMessage(`{"role":"assistant","api":"faux","provider":"internal","model":"model","stopReason":"toolUse","content":[{"type":"toolCall","callId":"plugin-call","name":"echo","input":{"text":"plugin"}}],"at":1}`)
	assistantPayload, _ := json.Marshal(map[string]any{"turn_id": "old", "executor_event_index": 2, "event": map[string]any{"type": "assistant_completed", "message": assistant}})
	preparedPayload, _ := json.Marshal(map[string]any{"turn_id": "old", "call_id": "plugin-call", "name": "echo", "output": `{"text":"plugin"}`, "error": false})
	request.Context.Tail = []agoprotocol.Event{
		{SchemaVersion: agoprotocol.SchemaVersion, EventID: "accepted-old", ThreadID: "thread", Sequence: 1, Type: agoprotocol.EventMessageAccepted, Visibility: agoprotocol.VisibilityUser, Payload: json.RawMessage(`{"queue_item_id":"old-queue","turn_id":"old","content":{"text":"prior"}}`)},
		{SchemaVersion: agoprotocol.SchemaVersion, EventID: "assistant", ThreadID: "thread", Sequence: 2, Type: agoprotocol.EventAssistantCompleted, Visibility: agoprotocol.VisibilityUser, Payload: assistantPayload},
		{SchemaVersion: agoprotocol.SchemaVersion, EventID: "prepared", ThreadID: "thread", Sequence: 3, Type: agoprotocol.EventToolResultPrepared, Visibility: agoprotocol.VisibilityUser, Payload: preparedPayload},
		{SchemaVersion: agoprotocol.SchemaVersion, EventID: "accepted-current", ThreadID: "thread", Sequence: 4, Type: agoprotocol.EventMessageAccepted, Visibility: agoprotocol.VisibilityUser, Payload: json.RawMessage(`{"queue_item_id":"current-queue","turn_id":"current","content":{"text":"now"}}`)},
	}
	transcript, err := piTranscript(request)
	if err != nil || len(transcript) != 3 || !bytes.Equal(transcript[1], assistant) || !bytes.Contains(transcript[2], []byte(`"callId":"plugin-call"`)) || !bytes.Contains(transcript[2], []byte(`"name":"echo"`)) {
		t.Fatalf("registered plugin transcript = %s, %v", transcript, err)
	}

	request.Tools = nil
	if _, err := piTranscript(request); err == nil {
		t.Fatal("accepted transcript for an unregistered plugin tool")
	}
	request.Tools = []agocoordinator.ExternalTool{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	request.Context.Tail[2].Payload = json.RawMessage(`{"turn_id":"old","call_id":"different","name":"echo","output":"x","error":false}`)
	if _, err := piTranscript(request); err == nil {
		t.Fatal("accepted mismatched prepared plugin tool call ID")
	}
}

func TestBrokerExecutorStreamsFlatPiEventsThroughRealSupervisor(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	testBinary, err := filepath.EvalSymlinks(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	supervisor := filepath.Join(t.TempDir(), "supervisor")
	if err := os.WriteFile(supervisor, []byte("#!/bin/sh\nexec \""+testBinary+"\" -test.run=^TestBrokerDuplexHelper$ -- supervisor\n"), 0700); err != nil {
		t.Fatal(err)
	}
	supervisor, err = filepath.EvalSymlinks(supervisor)
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	executor := BrokerExecutor{
		Supervisor: supervisor, Command: testBinary,
		Arguments: []string{"-test.run=^TestBrokerDuplexHelper$", "--", "sidecar"},
		Protocol:  "pi-jsonl-v1", Provider: "internal-provider", Model: "internal-model",
	}
	execution, err := executor.Start(context.Background(), agocoordinator.TurnRequest{
		ThreadID: "thread", TurnID: "turn", Workspace: workspace, Content: json.RawMessage(`{"text":"hello"}`), Context: currentPromptProjection("thread", "turn", "hello"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for event := range execution.Events() {
		types = append(types, event.Type)
		if event.Type == "settled" {
			if err := execution.CloseInput(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := execution.Wait(); err != nil {
		t.Fatal(err)
	}
	want := []string{"started", "text", "assistant_completed", "stopped", "settled"}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
}

func TestBrokerExecutorRealBunPiThroughCompiledSupervisorAndSeatbelt(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	supervisor := filepath.Join(t.TempDir(), "ago-supervisor")
	build := exec.Command("go", "build", "-o", supervisor, "./cmd/ago-supervisor")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real supervisor: %v\n%s", err, output)
	}
	supervisor, _ = filepath.EvalSymlinks(supervisor)
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("Bun is required for real Pi E2E")
	}
	bun, err = filepath.EvalSymlinks(bun)
	if err != nil {
		t.Fatal(err)
	}
	adapter := filepath.Join(root, "pi-adapter")
	secret := "must-not-enter-seatbelt-" + t.Name()
	t.Setenv("OPENAI_API_KEY", secret)
	callbackCalls := 0
	callback := func(_ context.Context, request ProviderRequest, emit func(ProviderResponse) error) error {
		callbackCalls++
		encoded, _ := json.Marshal(request)
		if bytes.Contains(encoded, []byte(secret)) {
			return fmt.Errorf("credential environment leaked into sandbox provider request")
		}
		if request.Provider != "trusted-faux" || request.Model != "scripted" {
			return fmt.Errorf("unexpected route %s/%s", request.Provider, request.Model)
		}
		if err := emit(ProviderResponse{Type: "delta", Delta: "REAL_PI"}); err != nil {
			return err
		}
		message := json.RawMessage(`{"role":"assistant","api":"ago-pipe","provider":"trusted-faux","model":"scripted","content":[{"type":"text","text":"REAL_PI"}],"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0,"cost":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"total":0}},"stopReason":"stop","timestamp":1}`)
		return emit(ProviderResponse{Type: "result", Message: message})
	}
	executor := BrokerExecutor{
		Supervisor: supervisor, Command: bun, Arguments: []string{"run", "--cwd", adapter, filepath.Join(adapter, "src/main.ts")},
		ReadRoots: []string{adapter, bun, filepath.Dir(bun)}, DefaultDeadline: 30 * time.Second, Protocol: "pi-jsonl-v1", Provider: "trusted-faux", Model: "scripted", ProviderCallback: callback,
	}
	execution, err := executor.Start(context.Background(), agocoordinator.TurnRequest{ThreadID: "real", TurnID: "turn", Workspace: t.TempDir(), Content: json.RawMessage(`{"text":"credential-free"}`), Context: currentPromptProjection("real", "turn", "credential-free")})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	var text string
	for event := range execution.Events() {
		types = append(types, event.Type)
		if event.Type == "text" {
			var frame struct {
				Delta string `json:"delta"`
			}
			_ = json.Unmarshal(event.Payload, &frame)
			text += frame.Delta
		}
		if event.Type == "settled" {
			if err := execution.CloseInput(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := execution.Wait(); err != nil {
		t.Fatal(err)
	}
	if callbackCalls != 1 || text != "REAL_PI" {
		t.Fatalf("callback calls=%d text=%q", callbackCalls, text)
	}
	want := []string{"started", "text", "assistant_completed", "stopped", "settled"}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		t.Fatalf("real Pi events=%v want=%v", types, want)
	}
	plan, cleanup, err := executor.buildPlan(context.Background(), agocoordinator.TurnRequest{ThreadID: "x", TurnID: "y", Workspace: adapter})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if plan.Network != NetworkDisabled {
		t.Fatal("child network was not disabled")
	}
	for _, value := range plan.Environment {
		if strings.Contains(value, secret) {
			t.Fatal("credential entered sandbox environment")
		}
	}
}

func TestBrokerExecutorAdvertisesAndCorrelatesRegisteredToolThroughRealSupervisor(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	testBinary, err := filepath.EvalSymlinks(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	supervisor := filepath.Join(t.TempDir(), "supervisor")
	if err := os.WriteFile(supervisor, []byte("#!/bin/sh\nexec \""+testBinary+"\" -test.run=^TestBrokerDuplexHelper$ -- supervisor\n"), 0700); err != nil {
		t.Fatal(err)
	}
	supervisor, err = filepath.EvalSymlinks(supervisor)
	if err != nil {
		t.Fatal(err)
	}
	executor := BrokerExecutor{
		Supervisor: supervisor, Command: testBinary,
		Arguments: []string{"-test.run=^TestBrokerDuplexHelper$", "--", "sidecar"},
		Protocol:  "pi-jsonl-v1", Provider: "internal-provider", Model: "internal-model",
	}
	execution, err := executor.Start(context.Background(), agocoordinator.TurnRequest{
		ThreadID: "thread", TurnID: "turn", Workspace: t.TempDir(), Content: json.RawMessage(`{"text":"hello"}`), Context: currentPromptProjection("thread", "turn", "hello"),
		Tools: []agocoordinator.ExternalTool{{Name: "echo", Description: "Echo", InputSchema: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for event := range execution.Events() {
		types = append(types, event.Type)
		if event.Type == "tool_invocation" {
			if !bytes.Contains(event.Payload, []byte(`"name":"echo"`)) {
				t.Fatalf("registered tool event = %s", event.Payload)
			}
			if err := execution.Send(context.Background(), agocoordinator.ExecutionControl{Type: "tool_result", Payload: json.RawMessage(`{"callId":"plugin-call","name":"echo","output":"{\"text\":\"plugin\"}","error":false}`)}); err != nil {
				t.Fatal(err)
			}
		}
		if event.Type == "settled" {
			if err := execution.CloseInput(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := execution.Wait(); err != nil {
		t.Fatal(err)
	}
	want := []string{"started", "assistant_completed", "tool_invocation", "tool_finished", "text", "assistant_completed", "stopped", "settled"}
	if fmt.Sprint(types) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
}

func TestBrokerExecutorSendsFlatControlsThroughRealSupervisor(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt is macOS-only")
	}
	testBinary, err := filepath.EvalSymlinks(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	supervisor := filepath.Join(t.TempDir(), "supervisor")
	if err := os.WriteFile(supervisor, []byte("#!/bin/sh\nexec \""+testBinary+"\" -test.run=^TestBrokerDuplexHelper$ -- supervisor\n"), 0700); err != nil {
		t.Fatal(err)
	}
	supervisor, err = filepath.EvalSymlinks(supervisor)
	if err != nil {
		t.Fatal(err)
	}
	executor := BrokerExecutor{
		Supervisor: supervisor, Command: testBinary,
		Arguments: []string{"-test.run=^TestBrokerDuplexHelper$", "--", "sidecar-controls"},
		Protocol:  "pi-jsonl-v1", Provider: "internal-provider", Model: "internal-model",
	}
	execution, err := executor.Start(context.Background(), agocoordinator.TurnRequest{
		ThreadID: "thread", TurnID: "turn", Workspace: t.TempDir(), Content: json.RawMessage(`{"text":"hello"}`), Context: currentPromptProjection("thread", "turn", "hello"),
	})
	if err != nil {
		t.Fatal(err)
	}
	events := execution.Events()
	if event := <-events; event.Type != "started" {
		t.Fatalf("first event = %q", event.Type)
	}
	if err := execution.Send(context.Background(), agocoordinator.ExecutionControl{Type: "steer", Payload: json.RawMessage(`{"text":"now"}`)}); err != nil {
		t.Fatal(err)
	}
	if err := execution.Send(context.Background(), agocoordinator.ExecutionControl{Type: "abort"}); err != nil {
		t.Fatal(err)
	}
	var types []string
	for event := range events {
		types = append(types, event.Type)
	}
	if err := execution.Wait(); err != nil {
		t.Fatal(err)
	}
	if want := []string{"text", "stopped", "settled"}; fmt.Sprint(types) != fmt.Sprint(want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
}

func currentPromptProjection(threadID, turnID, text string) agothreadstore.ContextProjection {
	payload, _ := json.Marshal(map[string]any{"queue_item_id": "queue", "turn_id": turnID, "content": map[string]any{"text": text}})
	return agothreadstore.ContextProjection{Tail: []agoprotocol.Event{{
		SchemaVersion: agoprotocol.SchemaVersion, EventID: "event", ThreadID: threadID, Sequence: 1,
		Type: agoprotocol.EventMessageAccepted, Visibility: agoprotocol.VisibilityUser, Payload: payload,
	}}}
}

func TestBrokerDuplexHelper(t *testing.T) {
	if len(os.Args) < 2 {
		t.Skip("helper process only")
	}
	mode := os.Args[len(os.Args)-1]
	switch mode {
	case "supervisor":
		live := os.NewFile(3, "liveness")
		controls := os.NewFile(4, "controls")
		events := os.NewFile(5, "events")
		if err := RunSupervisorSession(os.Stdin, os.Stdout, live, controls, events); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	case "sidecar", "sidecar-slow":
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			os.Exit(2)
		}
		var initialize map[string]any
		initializeLine := append([]byte(nil), scanner.Bytes()...)
		if json.Unmarshal(initializeLine, &initialize) != nil || initialize["type"] != "initialize" || initialize["cwd"] == "" || initialize["provider"] == "" || initialize["model"] == "" || len(initialize) != 6 {
			os.Exit(3)
		}
		if _, err := os.Stat(".ago-capture-initialize"); err == nil {
			capture, openErr := os.OpenFile(".ago-captured-initialize.jsonl", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if openErr != nil {
				os.Exit(12)
			}
			_, writeErr := capture.Write(append(initializeLine, '\n'))
			closeErr := capture.Close()
			if writeErr != nil || closeErr != nil {
				os.Exit(13)
			}
		}
		if transcript, ok := initialize["transcript"].([]any); !ok {
			os.Exit(8)
		} else if len(transcript) > 0 {
			if summary, ok := transcript[0].(map[string]any); ok && summary["role"] == "summary" {
				text, _ := summary["text"].(string)
				parts := strings.SplitN(text, "\n", 2)
				var capsule struct {
					Version            int `json:"version"`
					Objective          any `json:"objective"`
					AcceptanceCriteria any `json:"acceptance_criteria"`
					Decisions          any `json:"decisions"`
					ChangedPaths       any `json:"changed_paths"`
					Verification       any `json:"verification"`
					ActiveWork         any `json:"active_work"`
					UnresolvedIssues   any `json:"unresolved_issues"`
					NextAction         any `json:"next_action"`
				}
				if len(parts) != 2 || parts[0] != "AGO_RECOVERY_V2" || json.Unmarshal([]byte(parts[1]), &capsule) != nil || capsule.Version != 2 || capsule.Objective == nil || capsule.AcceptanceCriteria == nil || capsule.Decisions == nil || capsule.ChangedPaths == nil || capsule.Verification == nil || capsule.ActiveWork == nil || capsule.UnresolvedIssues == nil || capsule.NextAction == nil {
					os.Exit(9)
				}
			}
		}
		hasPluginEcho := false
		for _, raw := range initialize["tools"].([]any) {
			if tool, ok := raw.(map[string]any); ok && tool["name"] == "echo" {
				hasPluginEcho = true
			}
		}
		if !scanner.Scan() || string(scanner.Bytes()) != `{"text":"hello","type":"prompt"}` {
			os.Exit(4)
		}
		fmt.Fprintln(os.Stdout, `{"type":"started"}`)
		if mode == "sidecar-slow" {
			time.Sleep(750 * time.Millisecond)
		}
		if hasPluginEcho {
			fmt.Fprintln(os.Stdout, `{"type":"assistant_completed","message":{"role":"assistant","api":"faux","provider":"internal-provider","model":"internal-model","stopReason":"toolUse","content":[{"type":"toolCall","callId":"plugin-call","name":"echo","input":{"text":"plugin"}}],"at":1},"provider_usage":{"provider":"internal-provider","model":"internal-model","request_id":null,"status":"final","usage":{"input_tokens":0,"output_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0,"total_tokens":0},"cost":{"input":0,"output":0,"cache_read":0,"cache_write":0,"total":0}}}`)
			fmt.Fprintln(os.Stdout, `{"type":"tool_invocation","callId":"plugin-call","name":"echo","input":{"text":"plugin"}}`)
			if !scanner.Scan() {
				os.Exit(10)
			}
			var result struct {
				Type   string `json:"type"`
				CallID string `json:"callId"`
				Name   string `json:"name"`
				Output string `json:"output"`
				Error  bool   `json:"error"`
			}
			if json.Unmarshal(scanner.Bytes(), &result) != nil || result.Type != "tool_result" || result.CallID != "plugin-call" || result.Name != "echo" || result.Error || !strings.Contains(result.Output, "plugin") {
				os.Exit(11)
			}
			fmt.Fprintln(os.Stdout, `{"type":"tool_finished","callId":"plugin-call","error":false}`)
		}
		for _, event := range []string{
			`{"type":"text","delta":"hello"}`,
			`{"type":"assistant_completed","message":{"role":"assistant","api":"faux","provider":"internal-provider","model":"internal-model","stopReason":"stop","content":[{"type":"text","text":"hello"}],"at":1},"provider_usage":{"provider":"internal-provider","model":"internal-model","request_id":null,"status":"final","usage":{"input_tokens":0,"output_tokens":0,"cache_read_tokens":0,"cache_write_tokens":0,"total_tokens":0},"cost":{"input":0,"output":0,"cache_read":0,"cache_write":0,"total":0}}}`,
			`{"type":"stopped","reason":"stop"}`,
			`{"type":"settled"}`,
		} {
			fmt.Fprintln(os.Stdout, event)
		}
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	case "sidecar-controls":
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() || !scanner.Scan() {
			os.Exit(5)
		}
		fmt.Fprintln(os.Stdout, `{"type":"started"}`)
		if !scanner.Scan() || string(scanner.Bytes()) != `{"text":"now","type":"steer"}` {
			os.Exit(6)
		}
		if !scanner.Scan() || string(scanner.Bytes()) != `{"type":"abort"}` {
			os.Exit(7)
		}
		fmt.Fprintln(os.Stdout, `{"type":"text","delta":"controlled"}`)
		fmt.Fprintln(os.Stdout, `{"type":"stopped","reason":"aborted"}`)
		fmt.Fprintln(os.Stdout, `{"type":"settled"}`)
		os.Exit(0)
	default:
		t.Skip("helper process only")
	}
}
