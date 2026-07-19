package agopluginprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestFrozenWireLiterals(t *testing.T) {
	p := InitializeParams{SupportedProtocolVersions: []int{1}, Generation: 7, WorkspaceURI: nil,
		Plugins:      []PluginConfig{{PluginID: "demo", EntryURI: "file:///demo", Config: json.RawMessage(`{"x":1}`)}},
		Capabilities: Capabilities{UI: []UIKind{UINotify}, RenderMode: "headless"}, Limits: Limits{MaxMessageBytes: 1024, MaxInflight: 4}}
	assertJSON(t, p, `{"supportedProtocolVersions":[1],"generation":7,"workspaceUri":null,"plugins":[{"pluginId":"demo","entryUri":"file:///demo","config":{"x":1}}],"capabilities":{"ui":["notify"],"renderMode":"headless"},"limits":{"maxMessageBytes":1024,"maxInflight":4}}`)
	r := InitializeResult{ProtocolVersion: 1, Generation: 7, Plugins: []PluginRegistration{{PluginID: "demo", Tools: []ToolRegistration{{Name: "search", Description: "Search", InputSchema: json.RawMessage(`{"type":"object"}`)}}, Commands: []CommandRegistration{{ID: "run", Title: "Run"}}, Hooks: []string{"before"}}}}
	assertJSON(t, r, `{"protocolVersion":1,"generation":7,"plugins":[{"pluginId":"demo","tools":[{"name":"search","description":"Search","inputSchema":{"type":"object"}}],"commands":[{"id":"run","title":"Run"}],"hooks":["before"]}]}`)
	assertJSON(t, InvocationParams{Generation: 7, InvocationID: "i", ThreadID: "thread-1", TurnID: "turn-1", DeadlineUnixMs: 123}, `{"generation":7,"invocationId":"i","threadId":"thread-1","turnId":"turn-1","deadlineUnixMs":123}`)
	assertJSON(t, CancellationParams{Generation: 7, InvocationID: "i", Reason: "shutdown"}, `{"generation":7,"invocationId":"i","reason":"shutdown"}`)
	assertJSON(t, UIRequestParams{Generation: 7, PluginID: "demo", InvocationID: "i", DeadlineUnixMs: 123, Request: UIRequest{Kind: UIConfirm, Title: "Continue?"}}, `{"generation":7,"pluginId":"demo","invocationId":"i","deadlineUnixMs":123,"request":{"kind":"confirm","title":"Continue?"}}`)
	assertJSON(t, UIResult{Status: UIStatusOK, Value: json.RawMessage(`false`)}, `{"status":"ok","value":false}`)
	assertJSON(t, AIAskParams{Question: "Proceed?", Context: "risk", Options: []string{"a"}, Generation: 7, PluginID: "demo", InvocationID: "i", ThreadID: "th", TurnID: "tu", DeadlineUnixMs: 123}, `{"question":"Proceed?","context":"risk","options":["a"],"generation":7,"pluginId":"demo","invocationId":"i","threadId":"th","turnId":"tu","deadlineUnixMs":123}`)
	assertJSON(t, AIAskResult{Answer: AIAnswerYes, Probability: .9, Reason: "supported"}, `{"answer":"yes","probability":0.9,"reason":"supported"}`)
}

func TestAIAskValidationIsStrictAndBounded(t *testing.T) {
	valid := []byte(`{"question":"Proceed?","context":"risk","options":["a","b"],"generation":7,"pluginId":"p","invocationId":"i","threadId":"th","turnId":"tu","deadlineUnixMs":123}`)
	if _, err := DecodeAIAskParams(valid); err != nil {
		t.Fatalf("valid params: %v", err)
	}
	for _, raw := range [][]byte{
		[]byte(`{"question":"Proceed?","generation":7,"pluginId":"p","invocationId":"i","threadId":"th","turnId":"tu","deadlineUnixMs":123,"provider":"secret"}`),
		[]byte(`{"question":"","generation":7,"pluginId":"p","invocationId":"i","threadId":"th","turnId":"tu","deadlineUnixMs":123}`),
		[]byte(`{"question":"Proceed?","context":"` + strings.Repeat("x", MaxAIAskContextBytes+1) + `","generation":7,"pluginId":"p","invocationId":"i","threadId":"th","turnId":"tu","deadlineUnixMs":123}`),
	} {
		if _, err := DecodeAIAskParams(raw); err == nil {
			t.Fatalf("accepted invalid params: %.80s", raw)
		}
	}
	for _, raw := range [][]byte{
		[]byte(`{"answer":"yes","probability":0.5,"reason":"ok","model":"hidden"}`),
		[]byte(`{"answer":"maybe","probability":0.5,"reason":"ok"}`),
		[]byte(`{"answer":"no","probability":1.1,"reason":"ok"}`),
		[]byte(`{"answer":"uncertain","probability":0.5,"reason":""}`),
	} {
		if _, err := DecodeAIAskResult(raw); err == nil {
			t.Fatalf("accepted invalid result: %s", raw)
		}
	}
}

func TestMethodsAndErrorCodeDiscriminants(t *testing.T) {
	wantMethods := []string{"initialize", "hook.invoke", "tool.execute", "command.execute", "host.dispose", "invocation.cancel", "ui.request", "ai.ask", "log"}
	gotMethods := []string{MethodInitialize, MethodHookInvoke, MethodToolExecute, MethodCommandExecute, MethodHostDispose, MethodInvocationCancel, MethodUIRequest, MethodAIAsk, MethodLog}
	for i := range wantMethods {
		if gotMethods[i] != wantMethods[i] {
			t.Errorf("method %d = %q", i, gotMethods[i])
		}
	}
	wantCodes := []string{"INVALID_REQUEST", "INCOMPATIBLE_VERSION", "NOT_FOUND", "INVALID_RESULT", "CANCELLED", "TIMEOUT", "UNAVAILABLE", "OVERLOADED", "PLUGIN_ERROR", "STALE_GENERATION"}
	gotCodes := []string{CodeInvalidRequest, CodeIncompatibleVersion, CodeNotFound, CodeInvalidResult, CodeCancelled, CodeTimeout, CodeUnavailable, CodeOverloaded, CodePluginError, CodeStaleGeneration}
	for i := range wantCodes {
		if gotCodes[i] != wantCodes[i] {
			t.Errorf("code %d = %q", i, gotCodes[i])
		}
	}
}

func TestSessionValidation(t *testing.T) {
	s := NewSession(SessionConfig{SupportedProtocolVersions: []int{1}, Generation: 9})
	assertCode(t, s, request("x", MethodHookInvoke, InvocationParams{Generation: 9}), CodeInvalidRequest)
	init := InitializeParams{SupportedProtocolVersions: []int{1}, Generation: 9}
	if _, err := s.Accept(request("init", MethodInitialize, init)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(request("h", MethodHookInvoke, InvocationParams{Generation: 9, InvocationID: "i", ThreadID: "thread-1", TurnID: "turn-1", DeadlineUnixMs: 1})); err != nil {
		t.Fatal(err)
	}
	assertCode(t, s, request("h", MethodToolExecute, InvocationParams{Generation: 9}), CodeInvalidRequest)
	assertCode(t, s, request("s", MethodCommandExecute, InvocationParams{Generation: 8, InvocationID: "stale", ThreadID: "thread-1", TurnID: "turn-1", DeadlineUnixMs: 1}), CodeStaleGeneration)
	assertCode(t, s, request("u", "unknown", struct{}{}), CodeNotFound)
	assertCode(t, s, request("c", MethodInvocationCancel, CancellationParams{Generation: 9}), CodeInvalidRequest)
	if _, err := s.Accept(notification(MethodInvocationCancel, CancellationParams{Generation: 9, InvocationID: "i", Reason: "stop"})); err != nil {
		t.Fatal(err)
	}
	assertCode(t, s, notification(MethodUIRequest, UIRequestParams{Generation: 9}), CodeInvalidRequest)
	if _, err := s.Accept(request("ui", MethodUIRequest, UIRequestParams{Generation: 9, PluginID: "p", InvocationID: "i", DeadlineUnixMs: 1, Request: UIRequest{Kind: UIInput}})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Accept(notification(MethodLog, LogParams{Generation: 9, PluginID: "p", Level: "info", Message: "ok"})); err != nil {
		t.Fatal(err)
	}
	assertCode(t, s, request("log", MethodLog, LogParams{Generation: 9}), CodeInvalidRequest)
}

func TestInitializeRejectsIncompatibleAndUnknownDiscriminants(t *testing.T) {
	s := NewSession(SessionConfig{SupportedProtocolVersions: []int{1}, Generation: 1})
	assertCode(t, s, request("i", MethodInitialize, InitializeParams{SupportedProtocolVersions: []int{2}, Generation: 1}), CodeIncompatibleVersion)
	s = NewSession(SessionConfig{SupportedProtocolVersions: []int{1}, Generation: 1})
	if _, err := s.Accept(request("i", MethodInitialize, InitializeParams{SupportedProtocolVersions: []int{1}, Generation: 1})); err != nil {
		t.Fatal(err)
	}
	assertCode(t, s, request("ui", MethodUIRequest, UIRequestParams{Generation: 1, PluginID: "p", InvocationID: "i", DeadlineUnixMs: 1, Request: UIRequest{Kind: UIKind("bogus")}}), CodeInvalidRequest)
}

func TestDecoderOneBoundedJSONObjectPerLine(t *testing.T) {
	for _, input := range []string{"[]\n", "{\"type\":\"notification\",\"method\":\"log\"} {}\n", "{\"type\":\"notification\",\"method\":\"log\",\"extra\":1}\n"} {
		_, err := NewDecoder(strings.NewReader(input), 256).Decode()
		assertRPCCode(t, err, CodeInvalidRequest)
	}
	_, err := NewDecoder(strings.NewReader(strings.Repeat("x", 65)+"\n"), 64).Decode()
	assertRPCCode(t, err, CodeOverloaded)
}

func TestHeadlessUIResultsAreConservative(t *testing.T) {
	tests := []struct {
		kind UIKind
		want UIResult
	}{
		{UINotify, UIResult{Status: UIStatusOK}}, {UIConfirm, UIResult{Status: UIStatusOK, Value: json.RawMessage(`false`)}},
		{UIInput, UIResult{Status: UIStatusUnavailable}}, {UISelect, UIResult{Status: UIStatusUnavailable}},
	}
	for _, tt := range tests {
		got := HeadlessUIResult(tt.kind)
		if string(got.Value) != string(tt.want.Value) || got.Status != tt.want.Status {
			t.Errorf("%s: %#v", tt.kind, got)
		}
	}
}

func TestWireContractPreservesRegistrationAndNestedUIShapes(t *testing.T) {
	workspace := "file:///workspace"
	params := InitializeParams{
		SupportedProtocolVersions: []int{1},
		Generation:                4,
		WorkspaceURI:              &workspace,
		Plugins:                   []PluginConfig{{PluginID: "example.policy", EntryURI: "file:///plugin.ts", Config: json.RawMessage(`{"strict":true}`)}},
		Capabilities:              Capabilities{UI: []UIKind{UINotify, UIConfirm}, RenderMode: "headless"},
		Limits:                    Limits{MaxMessageBytes: 1024, MaxInflight: 8},
	}
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"capabilities":{"ui":["notify","confirm"],"renderMode":"headless"}`, `"workspaceUri":"file:///workspace"`, `"config":{"strict":true}`} {
		if !bytes.Contains(raw, []byte(fragment)) {
			t.Fatalf("initialize JSON %s missing %s", raw, fragment)
		}
	}

	registration := PluginRegistration{
		PluginID: "example.policy",
		Tools: []ToolRegistration{{
			Name: "example_check", Description: "Check input", InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
		Commands: []CommandRegistration{{ID: "configure", Title: "Configure", Category: "Plugin", Description: "Configure policy"}},
		Hooks:    []string{"tool.call", "tool.result"},
	}
	raw, err = json.Marshal(registration)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"inputSchema":{"type":"object"}`, `"id":"configure"`, `"hooks":["tool.call","tool.result"]`} {
		if !bytes.Contains(raw, []byte(fragment)) {
			t.Fatalf("registration JSON %s missing %s", raw, fragment)
		}
	}

	ui := UIRequestParams{
		Generation: 4, PluginID: "example.policy", InvocationID: "inv-1", DeadlineUnixMs: 123,
		Request: UIRequest{Kind: UISelect, Title: "Choose", Message: "Pick one", Options: []string{"a", "b"}, InitialValue: "b"},
	}
	raw, err = json.Marshal(ui)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"request":{"kind":"select"`, `"title":"Choose"`, `"options":["a","b"]`, `"initialValue":"b"`} {
		if !bytes.Contains(raw, []byte(fragment)) {
			t.Fatalf("UI request JSON %s missing %s", raw, fragment)
		}
	}
}

func TestSessionRejectsMalformedInvocationAndResponse(t *testing.T) {
	s := NewSession(SessionConfig{SupportedProtocolVersions: []int{1}, Generation: 3})
	if _, err := s.Accept(request("init", MethodInitialize, InitializeParams{SupportedProtocolVersions: []int{1}, Generation: 3})); err != nil {
		t.Fatal(err)
	}
	assertCode(t, s, request("missing-invocation", MethodToolExecute, InvocationParams{Generation: 3, DeadlineUnixMs: 100}), CodeInvalidRequest)
	assertCode(t, s, Envelope{Type: MessageResponse}, CodeInvalidRequest)
	ok := true
	assertCode(t, s, Envelope{Type: MessageResponse, ID: "response", OK: &ok, Error: &RPCError{Code: CodePluginError, Message: "both"}}, CodeInvalidRequest)
}

func request(id, method string, params any) Envelope {
	raw, _ := json.Marshal(params)
	return Envelope{Type: MessageRequest, ID: id, Method: method, Params: raw}
}
func notification(method string, params any) Envelope {
	raw, _ := json.Marshal(params)
	return Envelope{Type: MessageNotification, Method: method, Params: raw}
}
func assertJSON(t *testing.T, value any, want string) {
	t.Helper()
	got, err := json.Marshal(value)
	if err != nil || string(got) != want {
		t.Fatalf("JSON = %s (%v), want %s", got, err, want)
	}
}
func assertCode(t *testing.T, s *Session, e Envelope, want string) {
	t.Helper()
	_, err := s.Accept(e)
	assertRPCCode(t, err, want)
}
func assertRPCCode(t *testing.T, err error, want string) {
	t.Helper()
	var rpc *RPCError
	if !errors.As(err, &rpc) || rpc.Code != want {
		t.Fatalf("error = %v, want %s", err, want)
	}
}
