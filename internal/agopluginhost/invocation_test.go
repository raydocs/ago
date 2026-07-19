package agopluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"claudexflow/internal/agopluginprotocol"
)

func TestExecuteToolAndCanonicalCommandUseCurrentGeneration(t *testing.T) {
	registrations := []agopluginprotocol.PluginRegistration{
		{PluginID: "acme", Tools: []agopluginprotocol.ToolRegistration{{Name: "lookup"}}, Commands: []agopluginprotocol.CommandRegistration{{ID: "open"}}},
		{PluginID: DefaultPermissionPluginID},
	}
	first := &invocationRuntime{registrations: registrations, responses: []json.RawMessage{json.RawMessage(`{"old":true}`)}}
	second := &invocationRuntime{registrations: registrations, responses: []json.RawMessage{json.RawMessage(`{"ok":true}`), json.RawMessage(`null`)}}
	manager := NewManager(&invocationFactory{runtimes: []*invocationRuntime{first, second}}, time.Second)
	if _, err := manager.Reload(context.Background(), ReloadConfig{}, "start"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Reload(context.Background(), ReloadConfig{}, "reload"); err != nil {
		t.Fatal(err)
	}

	correlation := InvocationContext{ThreadID: "thread-1", TurnID: "turn-1"}
	if _, err := manager.ExecuteToolFor(context.Background(), "lookup", map[string]any{"q": "x"}, correlation); err != nil {
		t.Fatalf("ExecuteToolFor() error = %v", err)
	}
	if _, err := manager.ExecuteCommandFor(context.Background(), "acme:open", nil, correlation); err != nil {
		t.Fatalf("ExecuteCommandFor() error = %v", err)
	}
	if len(first.calls) != 0 || len(second.calls) != 2 {
		t.Fatalf("calls after reload: first=%d second=%d", len(first.calls), len(second.calls))
	}
	for _, call := range second.calls {
		if call.params.Generation != 2 || call.params.InvocationID == "" || call.params.ThreadID != "thread-1" || call.params.TurnID != "turn-1" || call.params.DeadlineUnixMs <= time.Now().UnixMilli() {
			t.Fatalf("unbound invocation params = %#v", call.params)
		}
	}
	var toolPayload map[string]any
	_ = json.Unmarshal(second.calls[0].params.Payload, &toolPayload)
	if second.calls[0].method != agopluginprotocol.MethodToolExecute || toolPayload["pluginId"] != "acme" || toolPayload["name"] != "lookup" {
		t.Fatalf("tool call = %#v, payload=%#v", second.calls[0], toolPayload)
	}
	if second.calls[1].method != agopluginprotocol.MethodCommandExecute || string(second.calls[1].params.Payload) != `{"commandId":"acme:open","pluginId":"acme"}` {
		t.Fatalf("command call = %#v", second.calls[1])
	}
	if _, err := manager.ExecuteCommandFor(context.Background(), "open", nil, correlation); err == nil {
		t.Fatal("non-canonical command was accepted")
	}
	if _, err := manager.ExecuteToolFor(context.Background(), "missing", nil, correlation); err == nil {
		t.Fatal("unregistered tool was accepted")
	}
}

func TestMalformedToolResultFailsAndInvokeDoesNotHoldManagerLock(t *testing.T) {
	runtime := &invocationRuntime{registrations: []agopluginprotocol.PluginRegistration{{PluginID: "acme", Tools: []agopluginprotocol.ToolRegistration{{Name: "bad"}}}, {PluginID: DefaultPermissionPluginID}}, responses: []json.RawMessage{json.RawMessage(`{`)}}
	manager := NewManager(&invocationFactory{runtimes: []*invocationRuntime{runtime}}, time.Second)
	if _, err := manager.Reload(context.Background(), ReloadConfig{}, "start"); err != nil {
		t.Fatal(err)
	}
	runtime.duringInvoke = func() { _ = manager.Current() }
	if _, err := manager.ExecuteToolFor(context.Background(), "bad", nil, InvocationContext{ThreadID: "thread-1", TurnID: "turn-1"}); err == nil {
		t.Fatal("malformed tool execution succeeded")
	}
}

func TestObserversFoldInOrderAndFailOpen(t *testing.T) {
	registrations := []agopluginprotocol.PluginRegistration{{PluginID: "acme", Hooks: []string{"tool.result", "agent.start"}}, {PluginID: DefaultPermissionPluginID}}
	runtime := &invocationRuntime{registrations: registrations, responses: []json.RawMessage{
		json.RawMessage(`[null,{"status":"done","output":"first"},{"status":"bogus"},{"status":"error","error":"last"}]`),
	}, invokeErrs: []error{nil, errors.New("observer broke")}}
	manager := NewManager(&invocationFactory{runtimes: []*invocationRuntime{runtime}}, time.Second)
	if _, err := manager.Reload(context.Background(), ReloadConfig{}, "start"); err != nil {
		t.Fatal(err)
	}
	var logged []string
	logFailure := func(err error) { logged = append(logged, err.Error()) }
	confirmed := ToolResult{Status: "done", Output: json.RawMessage(`"confirmed"`)}
	got := manager.ObserveToolResult(context.Background(), map[string]any{"id": "1"}, confirmed, logFailure)
	if got.Status != "error" || got.Error != "last" || len(logged) != 1 {
		t.Fatalf("folded=%#v logged=%#v", got, logged)
	}
	manager.ObserveLifecycle(context.Background(), "agent.start", map[string]any{"id": "a"}, logFailure)
	manager.ObserveLifecycle(context.Background(), "unregistered", nil, logFailure)
	if len(logged) != 2 || len(runtime.calls) != 2 {
		t.Fatalf("lifecycle fail-open logged=%#v calls=%d", logged, len(runtime.calls))
	}

	runtime.responses = append(runtime.responses, json.RawMessage(`not-json`))
	runtime.invokeErrs = append(runtime.invokeErrs, nil)
	got = manager.ObserveToolResult(context.Background(), nil, confirmed, logFailure)
	if !reflect.DeepEqual(got, confirmed) {
		t.Fatalf("malformed observer changed confirmed result: %#v", got)
	}
}

func TestLifecycleInvocationExposesThreadTurnCorrelationOnlyWhileActive(t *testing.T) {
	registrations := []agopluginprotocol.PluginRegistration{{PluginID: "acme", Hooks: []string{"agent.start"}}, {PluginID: DefaultPermissionPluginID}}
	runtime := &invocationRuntime{registrations: registrations, responses: []json.RawMessage{json.RawMessage(`[]`)}}
	manager := NewManager(&invocationFactory{runtimes: []*invocationRuntime{runtime}}, time.Second)
	if _, err := manager.Reload(context.Background(), ReloadConfig{}, "start"); err != nil {
		t.Fatal(err)
	}
	var invocationID string
	runtime.duringInvoke = func() {
		invocationID = runtime.calls[len(runtime.calls)-1].params.InvocationID
		correlation, found := manager.Invocation(invocationID)
		if !found || correlation.ThreadID != "thread-1" || correlation.TurnID != "turn-1" {
			t.Fatalf("active correlation = %#v found=%v", correlation, found)
		}
	}
	manager.ObserveLifecycle(context.Background(), "agent.start", map[string]any{"thread_id": "thread-1", "turn_id": "turn-1"}, nil)
	if _, found := manager.Invocation(invocationID); found {
		t.Fatal("completed invocation correlation leaked")
	}
}

type invocationCall struct {
	method string
	params agopluginprotocol.InvocationParams
}

type invocationRuntime struct {
	generation    int64
	registrations []agopluginprotocol.PluginRegistration
	responses     []json.RawMessage
	invokeErrs    []error
	calls         []invocationCall
	duringInvoke  func()
}

func (r *invocationRuntime) Initialize(_ context.Context, p agopluginprotocol.InitializeParams) (agopluginprotocol.InitializeResult, error) {
	r.generation = p.Generation
	return agopluginprotocol.InitializeResult{ProtocolVersion: 1, Generation: p.Generation, Plugins: r.registrations}, nil
}
func (r *invocationRuntime) Invoke(_ context.Context, method string, params agopluginprotocol.InvocationParams) (json.RawMessage, error) {
	r.calls = append(r.calls, invocationCall{method, params})
	if r.duringInvoke != nil {
		r.duringInvoke()
	}
	i := len(r.calls) - 1
	if i < len(r.invokeErrs) && r.invokeErrs[i] != nil {
		return nil, r.invokeErrs[i]
	}
	if i < len(r.responses) {
		return r.responses[i], nil
	}
	return json.RawMessage(`null`), nil
}
func (*invocationRuntime) CancelAll(string)                      {}
func (*invocationRuntime) Dispose(context.Context, string) error { return nil }
func (*invocationRuntime) Terminate() error                      { return nil }

type invocationFactory struct {
	runtimes []*invocationRuntime
	next     int
}

func (f *invocationFactory) Start(context.Context, int64) (Runtime, error) {
	r := f.runtimes[f.next]
	f.next++
	return r, nil
}
