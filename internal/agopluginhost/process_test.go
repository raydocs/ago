package agopluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"claudexflow/internal/agopluginprotocol"
)

func TestBunProcessRuntimeInitializesDefaultPolicyAndInvokesHook(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "main.ts"))
	if err != nil {
		t.Fatal(err)
	}
	factory := NewProcessFactory(bun, script, ProcessOptions{MaxMessageBytes: 1 << 20, ExitGrace: time.Second})
	runtimeValue, err := factory.Start(context.Background(), 1)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	runtime := runtimeValue.(*ProcessRuntime)
	t.Cleanup(func() { _ = runtime.Terminate() })
	result, err := runtime.Initialize(context.Background(), agopluginprotocol.InitializeParams{
		SupportedProtocolVersions: []int{1}, Generation: 1,
		Capabilities: agopluginprotocol.Capabilities{UI: []agopluginprotocol.UIKind{}, RenderMode: "headless"},
		Limits:       agopluginprotocol.Limits{MaxMessageBytes: 1 << 20, MaxInflight: 8},
	})
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if len(result.Plugins) != 1 || result.Plugins[0].PluginID != DefaultPermissionPluginID {
		t.Fatalf("plugins = %#v", result.Plugins)
	}
	payload := json.RawMessage(`{"hook":"tool.call","payload":{"tool":"shell","input":{}}}`)
	raw, err := runtime.Invoke(context.Background(), agopluginprotocol.MethodHookInvoke, agopluginprotocol.InvocationParams{
		Generation: 1, InvocationID: "policy-1", ThreadID: "thread-1", TurnID: "turn-1", DeadlineUnixMs: time.Now().Add(time.Second).UnixMilli(), Payload: payload,
	})
	if err != nil {
		t.Fatalf("Invoke() error = %v", err)
	}
	if string(raw) != `[{"action":"allow"}]` {
		t.Fatalf("policy result = %s", raw)
	}
}

func TestBunProcessManagerReloadReplacesGeneration(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	script, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "main.ts"))
	manager := NewManager(NewProcessFactory(bun, script, ProcessOptions{MaxMessageBytes: 1 << 20, ExitGrace: time.Second}), time.Second)
	config := ReloadConfig{Capabilities: agopluginprotocol.Capabilities{RenderMode: "headless"}, Limits: agopluginprotocol.Limits{MaxMessageBytes: 1 << 20, MaxInflight: 8}}
	first, err := manager.Reload(context.Background(), config, "startup")
	if err != nil {
		t.Fatalf("initial reload: %v", err)
	}
	second, err := manager.Reload(context.Background(), config, "reload")
	if err != nil {
		t.Fatalf("replacement reload: %v", err)
	}
	if first.Generation != 1 || second.Generation != 2 || manager.Current().Generation != 2 {
		t.Fatalf("generations: first=%#v second=%#v current=%#v", first, second, manager.Current())
	}
	if err := manager.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
}

func TestBunProcessRuntimeUsesHeadlessUIAndPropagatesCancellation(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	script, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "main.ts"))
	plugin, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "test-plugin.ts"))
	factory := NewProcessFactory(bun, script, ProcessOptions{MaxMessageBytes: 1 << 20, ExitGrace: time.Second})
	runtimeValue, err := factory.Start(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	runtime := runtimeValue.(*ProcessRuntime)
	t.Cleanup(func() { _ = runtime.Terminate() })
	_, err = runtime.Initialize(context.Background(), agopluginprotocol.InitializeParams{
		SupportedProtocolVersions: []int{1}, Generation: 1,
		Plugins:      []agopluginprotocol.PluginConfig{{PluginID: "test.plugin", EntryURI: "file://" + plugin, Config: json.RawMessage(`{}`)}},
		Capabilities: agopluginprotocol.Capabilities{RenderMode: "headless"},
		Limits:       agopluginprotocol.Limits{MaxMessageBytes: 1 << 20, MaxInflight: 8},
	})
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	uiResult, err := runtime.Invoke(context.Background(), agopluginprotocol.MethodHookInvoke, agopluginprotocol.InvocationParams{
		Generation: 1, InvocationID: "ui-1", ThreadID: "thread-1", TurnID: "turn-1", DeadlineUnixMs: time.Now().Add(time.Second).UnixMilli(), Payload: json.RawMessage(`{"hook":"ui"}`),
	})
	if err != nil || string(uiResult) != `[false]` {
		t.Fatalf("headless UI result = %s, %v", uiResult, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runtime.Invoke(ctx, agopluginprotocol.MethodHookInvoke, agopluginprotocol.InvocationParams{
			Generation: 1, InvocationID: "cancel-1", ThreadID: "thread-1", TurnID: "turn-1", DeadlineUnixMs: time.Now().Add(time.Second).UnixMilli(), Payload: json.RawMessage(`{"hook":"cancel"}`),
		})
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled invocation error = %v", err)
	}
}

func TestBunProcessRuntimeRoutesCorrelatedAIAskToAgoCallback(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	script, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "main.ts"))
	plugin, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "test-plugin.ts"))
	seen := make(chan agopluginprotocol.AIAskParams, 1)
	factory := NewProcessFactory(bun, script, ProcessOptions{MaxMessageBytes: 1 << 20, ExitGrace: time.Second, AIAsk: func(_ context.Context, p agopluginprotocol.AIAskParams) (agopluginprotocol.AIAskResult, error) {
		seen <- p
		return agopluginprotocol.AIAskResult{Answer: agopluginprotocol.AIAnswerYes, Probability: .8, Reason: "safe"}, nil
	}})
	rv, err := factory.Start(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	runtime := rv.(*ProcessRuntime)
	t.Cleanup(func() { _ = runtime.Terminate() })
	_, err = runtime.Initialize(context.Background(), agopluginprotocol.InitializeParams{SupportedProtocolVersions: []int{1}, Generation: 3, Plugins: []agopluginprotocol.PluginConfig{{PluginID: "test.plugin", EntryURI: "file://" + plugin, Config: json.RawMessage(`{}`)}}, Capabilities: agopluginprotocol.Capabilities{RenderMode: "headless"}, Limits: agopluginprotocol.Limits{MaxMessageBytes: 1 << 20, MaxInflight: 8}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := runtime.Invoke(context.Background(), agopluginprotocol.MethodHookInvoke, agopluginprotocol.InvocationParams{Generation: 3, InvocationID: "ask-1", ThreadID: "thread-authoritative", TurnID: "turn-authoritative", DeadlineUnixMs: time.Now().Add(time.Second).UnixMilli(), Payload: json.RawMessage(`{"hook":"ask","payload":{"question":"Proceed?"}}`)})
	if err != nil || string(raw) != `[{"answer":"yes","probability":0.8,"reason":"safe"}]` {
		t.Fatalf("ask result = %s, %v", raw, err)
	}
	p := <-seen
	if p.Generation != 3 || p.PluginID != "test.plugin" || p.InvocationID != "ask-1" || p.ThreadID != "thread-authoritative" || p.TurnID != "turn-authoritative" {
		t.Fatalf("correlation = %#v", p)
	}
}

func TestBunProcessRuntimeCancelsUnawaitedAIAskWhenInvocationCompletes(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	script, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "main.ts"))
	plugin, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "test-plugin.ts"))
	cancelled := make(chan struct{}, 1)
	factory := NewProcessFactory(bun, script, ProcessOptions{MaxMessageBytes: 1 << 20, ExitGrace: time.Second, AIAsk: func(ctx context.Context, _ agopluginprotocol.AIAskParams) (agopluginprotocol.AIAskResult, error) {
		<-ctx.Done()
		cancelled <- struct{}{}
		return agopluginprotocol.AIAskResult{}, ctx.Err()
	}})
	rv, err := factory.Start(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	runtime := rv.(*ProcessRuntime)
	t.Cleanup(func() { _ = runtime.Terminate() })
	_, err = runtime.Initialize(context.Background(), agopluginprotocol.InitializeParams{SupportedProtocolVersions: []int{1}, Generation: 3, Plugins: []agopluginprotocol.PluginConfig{{PluginID: "test.plugin", EntryURI: "file://" + plugin, Config: json.RawMessage(`{}`)}}, Capabilities: agopluginprotocol.Capabilities{RenderMode: "headless"}, Limits: agopluginprotocol.Limits{MaxMessageBytes: 1 << 20, MaxInflight: 8}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := runtime.Invoke(context.Background(), agopluginprotocol.MethodHookInvoke, agopluginprotocol.InvocationParams{Generation: 3, InvocationID: "unawaited", ThreadID: "thread-authoritative", TurnID: "turn-authoritative", DeadlineUnixMs: time.Now().Add(time.Second).UnixMilli(), Payload: json.RawMessage(`{"hook":"ask-unawaited"}`)})
	if err != nil || string(raw) != `["done"]` {
		t.Fatalf("unawaited hook result = %s, %v", raw, err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("unawaited ai.ask outlived its invocation")
	}
}

func TestProcessRuntimeRejectsChildAuthoredAIAskCorrelation(t *testing.T) {
	for name, mutate := range map[string]func(*agopluginprotocol.AIAskParams){
		"thread":   func(params *agopluginprotocol.AIAskParams) { params.ThreadID = "thread-forged" },
		"turn":     func(params *agopluginprotocol.AIAskParams) { params.TurnID = "turn-forged" },
		"deadline": func(params *agopluginprotocol.AIAskParams) { params.DeadlineUnixMs++ },
		"plugin":   func(params *agopluginprotocol.AIAskParams) { params.PluginID = "unknown.plugin" },
	} {
		t.Run(name, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			defer writer.Close()
			invocationCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			authoritative := agopluginprotocol.InvocationParams{Generation: 3, InvocationID: "invoke", ThreadID: "thread-1", TurnID: "turn-1", DeadlineUnixMs: time.Now().Add(time.Second).UnixMilli()}
			called := false
			runtime := &ProcessRuntime{generation: 3, stdin: writer, active: map[string]activeInvocation{"invoke": {ctx: invocationCtx, cancel: cancel, params: authoritative}}, plugins: map[string]struct{}{"test.plugin": {}}, options: ProcessOptions{AIAsk: func(context.Context, agopluginprotocol.AIAskParams) (agopluginprotocol.AIAskResult, error) {
				called = true
				return agopluginprotocol.AIAskResult{}, nil
			}}}
			params := agopluginprotocol.AIAskParams{Question: "safe?", Generation: 3, PluginID: "test.plugin", InvocationID: "invoke", ThreadID: authoritative.ThreadID, TurnID: authoritative.TurnID, DeadlineUnixMs: authoritative.DeadlineUnixMs}
			mutate(&params)
			runtime.handleAIAsk(agopluginprotocol.Envelope{Type: agopluginprotocol.MessageRequest, ID: "ask", Method: agopluginprotocol.MethodAIAsk, Params: mustJSON(params)})
			_ = writer.Close()
			var response agopluginprotocol.Envelope
			if err := json.NewDecoder(reader).Decode(&response); err != nil {
				t.Fatal(err)
			}
			if called || response.OK == nil || *response.OK || response.Error == nil || response.Error.Code != agopluginprotocol.CodeInvalidRequest {
				t.Fatalf("mismatch response = %#v, callback called=%v", response, called)
			}
		})
	}
}
