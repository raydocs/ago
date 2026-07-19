package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"claudexflow/internal/agocoordinator"
	"claudexflow/internal/agodaemon"
	"claudexflow/internal/agogit"
	"claudexflow/internal/agolocalexec"
	"claudexflow/internal/agopluginhost"
	"claudexflow/internal/agopluginprotocol"
	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
	"claudexflow/internal/agoverifier"
)

func TestRealBunUIRequestIsDurableAcrossClientReconnectAndResolvedExactly(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	thread, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: "create-dialog-e2e", IdempotencyKey: "create-dialog-e2e",
		ActorID: "test", Type: agoprotocol.CommandThreadCreate,
	}, agothreadstore.ThreadSpec{Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.Submit(context.Background(), agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: "submit-dialog-e2e", IdempotencyKey: "submit-dialog-e2e",
		ActorID: "test", Type: agoprotocol.CommandMessageSubmit, ThreadID: thread.ThreadID,
	}, agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"test"}`), Class: agoprotocol.QueueNormal})
	if err != nil {
		t.Fatal(err)
	}

	runtimeScript, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "main.ts"))
	pluginScript, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "test-plugin.ts"))
	broker := newDialogBroker(store)
	var plugins *agopluginhost.Manager
	plugins = agopluginhost.NewManager(agopluginhost.NewProcessFactory(bun, runtimeScript, agopluginhost.ProcessOptions{
		MaxMessageBytes: 1 << 20, ExitGrace: time.Second,
		UI: func(ctx context.Context, params agopluginprotocol.UIRequestParams) agopluginprotocol.UIResult {
			return broker.Request(ctx, plugins, params)
		},
	}), time.Second)
	t.Cleanup(func() { _ = plugins.Shutdown(context.Background()) })
	pluginConfig := agopluginhost.ReloadConfig{
		Plugins:      []agopluginprotocol.PluginConfig{{PluginID: "test.plugin", EntryURI: "file://" + pluginScript, Config: json.RawMessage(`{}`)}},
		Capabilities: agopluginprotocol.Capabilities{UI: []agopluginprotocol.UIKind{agopluginprotocol.UIConfirm}, RenderMode: "headless"},
		Limits:       agopluginprotocol.Limits{MaxMessageBytes: 1 << 20, MaxInflight: 8},
	}
	plugins.SetGenerationRetirer(broker.RetireGeneration)
	if _, err := plugins.Reload(context.Background(), pluginConfig, "test"); err != nil {
		t.Fatal(err)
	}

	coordinator := agocoordinator.New(store, dialogNoopExecutor{})
	httpServer := httptest.NewServer(agodaemon.NewWithDialogs(store, coordinator, broker).Handler())
	t.Cleanup(httpServer.Close)
	result := make(chan json.RawMessage, 1)
	go func() {
		raw, _ := plugins.ExecuteCommandFor(context.Background(), "test.plugin:confirm", nil, agopluginhost.InvocationContext{ThreadID: thread.ThreadID, TurnID: active.ActiveTurnID})
		result <- raw
	}()

	var dialog agothreadstore.PluginDialog
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		response, requestErr := http.Get(httpServer.URL + "/v1/threads/" + thread.ThreadID + "/dialogs")
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		var listed struct {
			Dialogs []agothreadstore.PluginDialog `json:"dialogs"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&listed)
		_ = response.Body.Close() // The UI client disconnects while the plugin invocation keeps waiting.
		if decodeErr == nil && len(listed.Dialogs) == 1 {
			dialog = listed.Dialogs[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if dialog.DialogID == "" || dialog.State != agothreadstore.DialogPending || dialog.RequestType != "confirm" {
		t.Fatalf("pending durable dialog = %#v", dialog)
	}
	// A new client connection sees and resolves the same durable revision.
	body, _ := json.Marshal(map[string]any{"resolver_id": "reconnected-client", "expected_revision": dialog.Revision, "expected_sequence": dialog.RequestedSequence, "response": map[string]any{"status": "ok", "value": true}})
	response, err := http.Post(httpServer.URL+"/v1/threads/"+thread.ThreadID+"/dialogs/"+dialog.DialogID+"/resolve", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("resolve status = %d", response.StatusCode)
	}
	select {
	case raw := <-result:
		if string(raw) != `true` {
			t.Fatalf("plugin received UI result %s", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("plugin did not resume after durable dialog resolution")
	}
	events, err := store.Replay(context.Background(), thread.ThreadID, active.LastSequence, 0)
	if err != nil || len(events) != 2 || events[0].Type != agoprotocol.EventPluginDialogRequested || events[1].Type != agoprotocol.EventPluginDialogResolved {
		t.Fatalf("durable dialog events = %#v, %v", events, err)
	}

	type commandResult struct {
		raw json.RawMessage
		err error
	}
	secondResult := make(chan commandResult, 1)
	go func() {
		raw, commandErr := plugins.ExecuteCommandFor(context.Background(), "test.plugin:confirm", nil, agopluginhost.InvocationContext{ThreadID: thread.ThreadID, TurnID: active.ActiveTurnID})
		secondResult <- commandResult{raw: raw, err: commandErr}
	}()
	var retiring agothreadstore.PluginDialog
	for deadline = time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		pending, listErr := store.ListPendingDialogs(context.Background(), thread.ThreadID)
		if listErr == nil && len(pending) == 1 {
			retiring = pending[0]
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if retiring.DialogID == "" {
		t.Fatal("second plugin dialog was not persisted")
	}
	if _, err := plugins.Reload(context.Background(), pluginConfig, "test-reload"); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-secondResult:
		if result.err == nil || len(result.raw) != 0 {
			t.Fatalf("retired plugin received result %s, %v", result.raw, result.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("retiring plugin invocation remained blocked on UI")
	}
	retired, err := store.Dialog(context.Background(), retiring.DialogID)
	if err != nil || retired.State != agothreadstore.DialogResolved || !bytes.Contains(retired.Response, []byte(`"cancelled"`)) {
		t.Fatalf("retired durable dialog = %#v, %v", retired, err)
	}
}

func TestProductionAttachmentStoreDefaultsBesideDatabaseAndIsPrivate(t *testing.T) {
	root := t.TempDir()
	databasePath := filepath.Join(root, "state", "ago.db")
	attachments, attachmentRoot, err := openProductionAttachmentStore(databasePath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer attachments.Close()
	if attachmentRoot != filepath.Dir(databasePath) {
		t.Fatalf("attachment root = %q, want %q", attachmentRoot, filepath.Dir(databasePath))
	}
	info, err := os.Stat(filepath.Join(attachmentRoot, "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("attachment directory mode = %v", info.Mode())
	}
}

func TestOptionalBridgeConfigurationIsDisabledOrComplete(t *testing.T) {
	if config, err := loadOptionalBridgeStartupConfig(bridgeStartupFlags{}); err != nil || config != nil {
		t.Fatalf("disabled config = %#v, %v", config, err)
	}
	if _, err := loadOptionalBridgeStartupConfig(bridgeStartupFlags{RelayURL: "https://relay.example"}); err == nil {
		t.Fatal("partial bridge configuration was accepted")
	}
	root := t.TempDir()
	publicationsPath := filepath.Join(root, "publications.json")
	if err := os.WriteFile(publicationsPath, []byte(`{"publications":[{"project_id":"project-1","thread_id":"T-one","actions":["thread.projection","thread.submit","thread.archive"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadOptionalBridgeStartupConfig(bridgeStartupFlags{
		RelayURL: "https://relay.example", CertificatePin: strings.Repeat("a", 64), BearerToken: "token",
		AccountID: "account", DeviceID: "device", AllowedProjects: "project-1", PublicationsPath: publicationsPath,
		StateRoot: filepath.Join(root, "state"), PasskeyCredentials: filepath.Join(root, "credentials.json"),
		PasskeyRPID: "example.com", PasskeyOrigins: "https://example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	if config == nil || len(config.publications.Publications) != 1 || len(config.client.AllowedProjects) != 1 {
		t.Fatalf("config = %#v", config)
	}
}

func TestProductionPluginServiceDiscoversAndIsolatesWorkspaceCommands(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	pluginRuntime, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "main.ts"))
	pluginEntry, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "test-plugin.ts"))
	workspaceA, workspaceB := t.TempDir(), t.TempDir()
	for _, workspace := range []string{workspaceA, workspaceB} {
		for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Ago Test"}, {"config", "user.email", "ago@example.invalid"}, {"commit", "--allow-empty", "-qm", "base"}} {
			command := exec.Command("git", args...)
			command.Dir = workspace
			if output, runErr := command.CombinedOutput(); runErr != nil {
				t.Fatalf("git %v: %v: %s", args, runErr, output)
			}
		}
	}
	if err := os.Mkdir(filepath.Join(workspaceA, ".ago"), 0o700); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal([]agopluginprotocol.PluginConfig{{PluginID: "test.plugin", EntryURI: "file://" + pluginEntry, Config: json.RawMessage(`{"codeChange":true}`)}})
	if err := os.WriteFile(filepath.Join(workspaceA, ".ago", "plugins.json"), config, 0o600); err != nil {
		t.Fatal(err)
	}
	dialogs := newDialogBroker(store)
	registry := agopluginhost.NewWorkspaceRegistry(func(workspace string) *agopluginhost.Manager {
		var manager *agopluginhost.Manager
		manager = agopluginhost.NewManager(agopluginhost.NewProcessFactory(bun, pluginRuntime, agopluginhost.ProcessOptions{MaxMessageBytes: 1 << 20, ExitGrace: time.Second, UI: func(ctx context.Context, params agopluginprotocol.UIRequestParams) agopluginprotocol.UIResult {
			return dialogs.Request(ctx, manager, params)
		}}), time.Second)
		manager.SetGenerationRetirer(func(generation int64, reason string) {
			dialogs.RetireWorkspaceGeneration(workspace, generation, reason)
		})
		return manager
	}, agopluginhost.ReloadConfig{Capabilities: agopluginprotocol.Capabilities{RenderMode: "client-neutral"}, Limits: agopluginprotocol.Limits{MaxMessageBytes: 1 << 20, MaxInflight: 8}})
	t.Cleanup(func() { _ = registry.Shutdown(context.Background()) })
	service := productionTools{store: store, plugins: registry}
	createActive := func(id, workspace string) (string, string) {
		thread, createErr := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{SchemaVersion: agoprotocol.SchemaVersion, CommandID: "create-" + id, IdempotencyKey: "create-" + id, ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.ThreadSpec{Workspace: workspace, Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
		if createErr != nil {
			t.Fatal(createErr)
		}
		active, submitErr := store.Submit(context.Background(), agoprotocol.Command{SchemaVersion: agoprotocol.SchemaVersion, CommandID: "submit-" + id, IdempotencyKey: "submit-" + id, ActorID: "test", Type: agoprotocol.CommandMessageSubmit, ThreadID: thread.ThreadID}, agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"active"}`), Class: agoprotocol.QueueNormal})
		if submitErr != nil {
			t.Fatal(submitErr)
		}
		return thread.ThreadID, active.ActiveTurnID
	}
	threadA, turnA := createActive("workspace-a", workspaceA)
	threadB, turnB := createActive("workspace-b", workspaceB)
	if _, err := agogit.NewService(store).Refresh(context.Background(), agogit.RefreshInput{ThreadID: threadA, Workspace: workspaceA, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}, EnvironmentID: "thread:" + threadA, ExecutorGeneration: 1, IdempotencyKey: "write-snapshot"}); err != nil {
		t.Fatal(err)
	}
	writeResult, err := service.ExecuteTool(context.Background(), agocoordinator.ToolCall{ThreadID: threadA, TurnID: turnA, CallID: "write-call", Name: "write_file", Input: map[string]any{"path": "declared.txt", "content_base64": "AGJpbmFyeQ==", "mode": float64(0o600)}})
	if err != nil || writeResult.Error || !strings.Contains(writeResult.Output, `"receipt_id":"W-`) {
		t.Fatalf("declared write result = %#v, %v", writeResult, err)
	}
	if bytes, readErr := os.ReadFile(filepath.Join(workspaceA, "declared.txt")); readErr != nil || !reflect.DeepEqual(bytes, []byte("\x00binary")) {
		t.Fatalf("declared write bytes = %q, %v", bytes, readErr)
	}
	if _, err := service.ExecuteTool(context.Background(), agocoordinator.ToolCall{ThreadID: threadA, TurnID: turnA, CallID: "protected-call", Name: "write_file", Input: map[string]any{"path": "thread-app/src/index.ts", "content": "forbidden"}}); err == nil {
		t.Fatal("production write_file accepted protected path")
	}
	snapshotA, err := service.PluginRegistrations(context.Background(), threadA)
	if err != nil || len(snapshotA.Registrations) != 2 || snapshotA.Registrations[0].PluginID != "test.plugin" {
		t.Fatalf("workspace A plugins = %#v, %v", snapshotA, err)
	}
	snapshotB, err := service.PluginRegistrations(context.Background(), threadB)
	if err != nil || len(snapshotB.Registrations) != 1 || snapshotB.Registrations[0].PluginID != agopluginhost.DefaultPermissionPluginID {
		t.Fatalf("workspace B plugins = %#v, %v", snapshotB, err)
	}
	result, err := service.ExecutePluginCommand(context.Background(), threadA, turnA, "test.plugin:run", map[string]any{"source": "test"})
	if err != nil || string(result) != `"ran"` {
		t.Fatalf("workspace A command = %s, %v", result, err)
	}
	if _, err := service.ExecutePluginCommand(context.Background(), threadB, turnB, "test.plugin:run", nil); err == nil {
		t.Fatal("workspace B executed workspace A plugin command")
	}

	// Exercise the production coordinator catalog and durable tool boundary with
	// the real discovered Bun plugin, rather than stopping at manager dispatch.
	executor := &registeredToolSessionExecutor{started: make(chan agocoordinator.TurnRequest, 1)}
	coordinator := agocoordinator.NewWithToolRuntime(store, executor, service)
	threadC, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{SchemaVersion: agoprotocol.SchemaVersion, CommandID: "create-workspace-tool", IdempotencyKey: "create-workspace-tool", ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.ThreadSpec{Workspace: workspaceA, Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coordinator.Submit(context.Background(), agoprotocol.Command{SchemaVersion: agoprotocol.SchemaVersion, CommandID: "submit-workspace-tool", IdempotencyKey: "submit-workspace-tool", ActorID: "test", Type: agoprotocol.CommandMessageSubmit, ThreadID: threadC.ThreadID}, agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"use plugin"}`), Class: agoprotocol.QueueNormal}); err != nil {
		t.Fatal(err)
	}
	select {
	case request := <-executor.started:
		if len(request.Tools) != 4 || request.Tools[0].Name != "ago_echo" || request.Tools[1].Name != "write_file" || request.Tools[2].Name != "echo" || request.Tools[3].Name != "write_test_file" {
			t.Fatalf("workspace tool catalog = %#v", request.Tools)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("coordinator did not launch registered plugin tool turn")
	}
	var replay []agoprotocol.Event
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		replay, err = store.Replay(context.Background(), threadC.ThreadID, 0, 0)
		if err == nil && len(replay) > 0 && replay[len(replay)-1].Type == agoprotocol.EventTurnCompleted {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	counts := map[agoprotocol.EventType]int{}
	for _, event := range replay {
		counts[event.Type]++
	}
	for _, eventType := range []agoprotocol.EventType{agoprotocol.EventToolRequested, agoprotocol.EventToolCompleted, agoprotocol.EventToolResultPrepared, agoprotocol.EventTurnCompleted} {
		if counts[eventType] != 1 {
			t.Fatalf("durable plugin tool event %s count = %d; replay = %#v", eventType, counts[eventType], replay)
		}
	}
	proof, err := os.ReadFile(filepath.Join(workspaceA, "ago-proof.txt"))
	if err != nil || string(proof) != "phase-1 deterministic proof" {
		t.Fatalf("representative code-change artifact = %q, %v", proof, err)
	}
}

type registeredToolSessionExecutor struct {
	started chan agocoordinator.TurnRequest
}

func (*registeredToolSessionExecutor) Run(context.Context, agocoordinator.TurnRequest) error {
	return fmt.Errorf("legacy executor path must not be used")
}

func (executor *registeredToolSessionExecutor) Start(_ context.Context, request agocoordinator.TurnRequest) (agocoordinator.Execution, error) {
	execution := &registeredToolExecution{events: make(chan agocoordinator.ExecutionEvent, 8), controls: make(chan agocoordinator.ExecutionControl, 1), wait: make(chan error, 1)}
	executor.started <- request
	go func() {
		execution.events <- agocoordinator.ExecutionEvent{Index: 1, Type: "started", Payload: json.RawMessage(`{"type":"started"}`)}
		execution.events <- agocoordinator.ExecutionEvent{Index: 2, Type: "assistant_completed", Payload: json.RawMessage(`{"type":"assistant_completed","message":{"role":"assistant","api":"faux","provider":"internal-provider","model":"internal-model","stopReason":"toolUse","content":[{"type":"toolCall","callId":"plugin-call","name":"write_test_file","input":{"text":"phase-1 deterministic proof"}}],"at":1}}`)}
		execution.events <- agocoordinator.ExecutionEvent{Index: 3, Type: "tool_invocation", Payload: json.RawMessage(`{"type":"tool_invocation","callId":"plugin-call","name":"write_test_file","input":{"text":"phase-1 deterministic proof"}}`)}
		control := <-execution.controls
		if control.Type != "tool_result" || !bytes.Contains(control.Payload, []byte(`"name":"write_test_file"`)) || !bytes.Contains(control.Payload, []byte(`wrote ago-proof.txt`)) {
			execution.wait <- fmt.Errorf("plugin result control = %#v", control)
			close(execution.events)
			return
		}
		execution.events <- agocoordinator.ExecutionEvent{Index: 4, Type: "tool_finished", Payload: json.RawMessage(`{"type":"tool_finished","callId":"plugin-call","error":false}`)}
		execution.events <- agocoordinator.ExecutionEvent{Index: 5, Type: "stopped", Payload: json.RawMessage(`{"type":"stopped","reason":"stop"}`)}
		execution.events <- agocoordinator.ExecutionEvent{Index: 6, Type: "settled", Payload: json.RawMessage(`{"type":"settled"}`)}
		close(execution.events)
		execution.wait <- nil
	}()
	return execution, nil
}

type registeredToolExecution struct {
	events   chan agocoordinator.ExecutionEvent
	controls chan agocoordinator.ExecutionControl
	wait     chan error
}

func (execution *registeredToolExecution) Events() <-chan agocoordinator.ExecutionEvent {
	return execution.events
}
func (execution *registeredToolExecution) Send(_ context.Context, control agocoordinator.ExecutionControl) error {
	execution.controls <- control
	return nil
}
func (*registeredToolExecution) CloseInput() error { return nil }
func (execution *registeredToolExecution) Wait() error {
	return <-execution.wait
}

type dialogNoopExecutor struct{}

func (dialogNoopExecutor) Run(context.Context, agocoordinator.TurnRequest) error { return nil }

func TestProductionAIClassifierUsesDedicatedPiSessionAndStrictTypedResult(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	thread, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: "classifier-thread", IdempotencyKey: "classifier-thread", ActorID: "test", Type: agoprotocol.CommandThreadCreate,
	}, agothreadstore.ThreadSpec{Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	executor := &classifierFauxExecutor{}
	classifier := piAIClassifier{store: store, executor: executor}
	result, err := classifier.Ask(context.Background(), agopluginprotocol.AIAskParams{
		Question: "Is this safe?", Context: "No writes", Options: []string{"strict"}, ThreadID: thread.ThreadID, TurnID: "turn-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != agopluginprotocol.AIAnswerYes || result.Probability != 0.9 || result.Reason != "bounded" {
		t.Fatalf("classifier result = %#v", result)
	}
	if executor.request.ThreadID != thread.ThreadID || executor.request.TurnID == "turn-1" || executor.request.Workspace == "" || executor.request.Mode != agoprotocol.AgentModeMedium || len(executor.request.Tools) != 0 {
		t.Fatalf("dedicated classifier request = %#v", executor.request)
	}
	if bytes.Contains(executor.request.Content, []byte(`"provider"`)) || bytes.Contains(executor.request.Content, []byte(`"model"`)) {
		t.Fatalf("classifier prompt exposed route: %s", executor.request.Content)
	}
}

func TestTrustedProviderProcessBridgesBoundedTypedFrames(t *testing.T) {
	bridge := trustedProviderProcess{Command: os.Args[0], Arguments: []string{"-test.run=^TestTrustedProviderProcessHelper$"}, Provider: "openai", Model: "gpt-5.4", Environment: []string{"AGO_PROVIDER_HELPER=1"}}
	var responses []agolocalexec.ProviderResponse
	err := bridge.Callback(context.Background(), agolocalexec.ProviderRequest{
		Type: "inference_request", ID: "request-1", Provider: "openai", Model: "gpt-5.4", Context: json.RawMessage(`{"messages":[]}`), Options: json.RawMessage(`{"maxTokens":32}`),
	}, func(response agolocalexec.ProviderResponse) error {
		responses = append(responses, response)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(responses) != 2 || responses[0].Type != "delta" || responses[0].Delta != "yes" || responses[1].Type != "result" || !bytes.Contains(responses[1].Message, []byte(`"role":"assistant"`)) {
		t.Fatalf("trusted provider responses = %#v", responses)
	}
	if err := bridge.Callback(context.Background(), agolocalexec.ProviderRequest{Type: "inference_request", ID: "wrong-route", Provider: "other", Model: "gpt-5.4", Context: json.RawMessage(`{}`), Options: json.RawMessage(`{}`)}, func(agolocalexec.ProviderResponse) error { return nil }); err == nil {
		t.Fatal("trusted provider accepted a child-selected route")
	}
}

func TestExecutorReadRootUsesPackageRootForPiProviderEntrypoint(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{
		filepath.Join(root, "package.json"):              `{"private":true}`,
		filepath.Join(source, "main.ts"):                 `import "dependency"`,
		filepath.Join(source, "provider-process.ts"):     `export {}`,
		filepath.Join(root, "node_modules", "marker.js"): `export {}`,
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	entry := filepath.Join(source, "main.ts")
	readRoot, err := executorReadRoot(entry)
	if err != nil {
		t.Fatal(err)
	}
	if readRoot != root {
		t.Fatalf("executor read root = %q, want package root %q", readRoot, root)
	}
	arguments := executorArguments(entry, readRoot)
	want := []string{"run", "--cwd", root, entry}
	if fmt.Sprint(arguments) != fmt.Sprint(want) {
		t.Fatalf("executor arguments = %q, want %q", arguments, want)
	}
}

func TestTrustedProviderProcessHelper(t *testing.T) {
	if os.Getenv("AGO_PROVIDER_HELPER") != "1" {
		return
	}
	var request agolocalexec.ProviderRequest
	if err := json.NewDecoder(os.Stdin).Decode(&request); err != nil {
		os.Exit(2)
	}
	fmt.Printf("{\"type\":\"delta\",\"id\":%q,\"delta\":\"yes\"}\n", request.ID)
	fmt.Printf("{\"type\":\"result\",\"id\":%q,\"message\":{\"role\":\"assistant\",\"api\":\"faux\",\"provider\":\"openai\",\"model\":\"gpt-5.4\",\"content\":[{\"type\":\"text\",\"text\":\"yes\"}],\"usage\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0,\"totalTokens\":0,\"cost\":{\"input\":0,\"output\":0,\"cacheRead\":0,\"cacheWrite\":0,\"total\":0}},\"stopReason\":\"stop\",\"timestamp\":1}}\n", request.ID)
	os.Exit(0)
}

type classifierFauxExecutor struct{ request agocoordinator.TurnRequest }

func (executor *classifierFauxExecutor) Start(_ context.Context, request agocoordinator.TurnRequest) (agocoordinator.Execution, error) {
	executor.request = request
	execution := &registeredToolExecution{events: make(chan agocoordinator.ExecutionEvent, 3), controls: make(chan agocoordinator.ExecutionControl, 1), wait: make(chan error, 1)}
	execution.events <- agocoordinator.ExecutionEvent{Index: 1, Type: "text", Payload: json.RawMessage(`{"type":"text","delta":"{\"answer\":\"yes\",\"probability\":0.9,"}`)}
	execution.events <- agocoordinator.ExecutionEvent{Index: 2, Type: "text", Payload: json.RawMessage(`{"type":"text","delta":"\"reason\":\"bounded\"}"}`)}
	close(execution.events)
	execution.wait <- nil
	return execution, nil
}

func TestValidateUIValueRequiresTheRequestKindResultType(t *testing.T) {
	tests := []struct {
		kind  string
		value string
		ok    bool
	}{
		{"notify", "", true}, {"notify", `true`, false},
		{"confirm", `true`, true}, {"confirm", ` false `, true}, {"confirm", `null`, false}, {"confirm", `"yes"`, false},
		{"input", `"answer"`, true}, {"select", `"second"`, true}, {"select", `2`, false},
		{"unknown", `null`, false},
	}
	for _, test := range tests {
		err := validateUIValue(test.kind, json.RawMessage(test.value))
		if (err == nil) != test.ok {
			t.Errorf("validateUIValue(%q, %q) error = %v, want success=%v", test.kind, test.value, err, test.ok)
		}
	}
}

func TestDeterministicRecoverySummaryIsBoundedStructuredAndTruthPreserving(t *testing.T) {
	event := func(sequence uint64, eventType agoprotocol.EventType, payload string) agoprotocol.Event {
		return agoprotocol.Event{SchemaVersion: agoprotocol.SchemaVersion, EventID: fmt.Sprintf("E-%d", sequence), ThreadID: "T-1", Sequence: sequence, Type: eventType, Visibility: agoprotocol.VisibilityUser, Payload: json.RawMessage(payload)}
	}
	projection := agothreadstore.ContextProjection{Tail: []agoprotocol.Event{
		event(1, agoprotocol.EventMessageAccepted, `{"turn_id":"turn-1","content":{"text":"finish the acceptance criteria"}}`),
		event(2, agoprotocol.EventAssistantCompleted, `{"turn_id":"turn-1","event":{"type":"assistant_completed"}}`),
		event(3, agoprotocol.EventToolRequested, `{"turn_id":"turn-1","event":{"type":"tool_invocation","callId":"call-1","name":"write","input":{"path":"a.go"}}}`),
		event(4, agoprotocol.EventToolCompleted, `{"turn_id":"turn-1","call_id":"call-1","output":"ok","error":false}`),
	}}
	first, err := deterministicRecoverySummary(projection)
	if err != nil {
		t.Fatal(err)
	}
	second, err := deterministicRecoverySummary(projection)
	if err != nil || first != second {
		t.Fatalf("recovery summary is not deterministic: err=%v equal=%v", err, first == second)
	}
	const prefix = "AGO_RECOVERY_V2\n"
	if !strings.HasPrefix(first, prefix) || len(first) > 64<<10 {
		t.Fatalf("recovery summary prefix/size = %q/%d", first[:min(len(first), len(prefix))], len(first))
	}
	var capsule recoveryCapsule
	decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(first, prefix)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&capsule); err != nil {
		t.Fatal(err)
	}
	if capsule.Version != 2 || len(capsule.SourceSHA256) != 64 || len(capsule.Objective.Evidence) != 1 || len(capsule.AcceptanceCriteria.Evidence) != 1 || len(capsule.Decisions.Evidence) != 1 || len(capsule.ChangedPaths.Evidence) != 1 || len(capsule.Verification.Evidence) != 1 || len(capsule.ActiveWork.Evidence) != 4 || len(capsule.NextAction.Evidence) != 1 {
		t.Fatalf("structured recovery capsule = %#v", capsule)
	}
	if capsule.UnresolvedIssues.Status != "unprepared-tool-requests-present" || len(capsule.UnresolvedIssues.Evidence) != 1 {
		t.Fatalf("unresolved issues = %#v", capsule.UnresolvedIssues)
	}
	projection.Tail = append(projection.Tail, event(5, agoprotocol.EventToolResultPrepared, `{"turn_id":"turn-1","call_id":"call-1","output":"ok","error":false}`))
	prepared, err := deterministicRecoverySummary(projection)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(prepared, prefix)), &capsule); err != nil || capsule.UnresolvedIssues.Status != "no-unprepared-tool-request-observed" || len(capsule.UnresolvedIssues.Evidence) != 0 {
		t.Fatalf("prepared recovery unresolved issues = %#v, %v", capsule.UnresolvedIssues, err)
	}
}

func TestLoadPluginConfigsIsStrictAndBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugins.json")
	if err := os.WriteFile(path, []byte(`[{"pluginId":"acme","entryUri":"file:///tmp/acme.ts","config":{"enabled":true}}]`), 0600); err != nil {
		t.Fatal(err)
	}
	plugins, err := loadPluginConfigs(path)
	if err != nil || len(plugins) != 1 || plugins[0].PluginID != "acme" || !json.Valid(plugins[0].Config) {
		t.Fatalf("plugins = %#v, %v", plugins, err)
	}
	if err := os.WriteFile(path, []byte(`[{"pluginId":"acme","entryUri":"file:///tmp/acme.ts","unknown":true}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPluginConfigs(path); err == nil {
		t.Fatal("accepted unknown plugin config field")
	}
}

func TestUnixListenerReplacesStaleSocket(t *testing.T) {
	path := socketTestPath(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("create stale socket: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close stale socket: %v", err)
	}

	replacement, err := unixListener(path)
	if err != nil {
		t.Fatalf("unixListener() error = %v", err)
	}
	t.Cleanup(func() { _ = replacement.Close() })
}

func TestUnixListenerDoesNotStealLiveSocket(t *testing.T) {
	path := socketTestPath(t)
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	if replacement, err := unixListener(path); err == nil {
		_ = replacement.Close()
		t.Fatal("unixListener() succeeded for a live daemon socket")
	}
	connection, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("live socket was stolen: %v", err)
	}
	_ = connection.Close()
}

func TestUnixListenerRefusesNonSocketPath(t *testing.T) {
	path := socketTestPath(t)
	if err := os.WriteFile(path, []byte("user data"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}

	if listener, err := unixListener(path); err == nil {
		_ = listener.Close()
		t.Fatal("unixListener() succeeded for a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("regular file was removed: %v", err)
	}
	if string(data) != "user data" {
		t.Fatalf("regular file changed to %q", data)
	}
}

func TestOptionalTCPStartupConfigIsAllOrNothingLoopbackAndStrong(t *testing.T) {
	private := t.TempDir()
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	private, _ = filepath.EvalSymlinks(private)
	endpoint := filepath.Join(private, "endpoint.json")
	token := "0123456789abcdef0123456789abcdef"
	valid, err := loadOptionalTCPStartupConfig(tcpStartupFlags{Listen: "127.0.0.1:0", EndpointFile: endpoint, BearerToken: token})
	if err != nil || valid == nil || valid.listen != "127.0.0.1:0" || valid.endpointFile != endpoint || valid.bearerToken != token {
		t.Fatalf("valid TCP config = %#v, %v", valid, err)
	}
	if disabled, err := loadOptionalTCPStartupConfig(tcpStartupFlags{}); err != nil || disabled != nil {
		t.Fatalf("disabled TCP config = %#v, %v", disabled, err)
	}
	for name, flags := range map[string]tcpStartupFlags{
		"partial listen":    {Listen: "127.0.0.1:0"},
		"partial endpoint":  {EndpointFile: endpoint},
		"partial token":     {BearerToken: token},
		"wildcard":          {Listen: "0.0.0.0:0", EndpointFile: endpoint, BearerToken: token},
		"nonloopback":       {Listen: "192.0.2.1:0", EndpointFile: endpoint, BearerToken: token},
		"hostname":          {Listen: "localhost:0", EndpointFile: endpoint, BearerToken: token},
		"weak token":        {Listen: "127.0.0.1:0", EndpointFile: endpoint, BearerToken: strings.Repeat("a", 32)},
		"relative endpoint": {Listen: "127.0.0.1:0", EndpointFile: "endpoint.json", BearerToken: token},
	} {
		t.Run(name, func(t *testing.T) {
			if config, err := loadOptionalTCPStartupConfig(flags); err == nil || config != nil {
				t.Fatalf("config = %#v, error = %v", config, err)
			}
		})
	}
}

func TestLoopbackTCPEndpointIsPrivateAuthenticatedAndRemoved(t *testing.T) {
	private := t.TempDir()
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	private, _ = filepath.EvalSymlinks(private)
	token := "0123456789abcdef0123456789abcdef"
	endpointPath := filepath.Join(private, "endpoint.json")
	config, err := loadOptionalTCPStartupConfig(tcpStartupFlags{Listen: "127.0.0.1:0", EndpointFile: endpointPath, BearerToken: token})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := tcpListener(config.listen)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	baseHandler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"status":"ok"}`))
	})
	unixPath := socketTestPath(t)
	unixNetListener, err := unixListener(unixPath)
	if err != nil {
		t.Fatal(err)
	}
	defer unixNetListener.Close()
	unixServer := &http.Server{Handler: baseHandler}
	unixResult := make(chan error, 1)
	go func() { unixResult <- unixServer.Serve(unixNetListener) }()
	authenticated, err := agodaemon.RequireBearerToken(baseHandler, config.bearerToken)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: authenticated}
	result := make(chan error, 1)
	go func() { result <- server.Serve(listener) }()
	baseURL, err := writeTCPEndpoint(config.endpointFile, listener.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer removeTCPEndpoint(config.endpointFile)

	raw, err := os.ReadFile(endpointPath)
	if err != nil {
		t.Fatal(err)
	}
	var endpoint struct {
		BaseURL string `json:"base_url"`
	}
	if err := json.Unmarshal(raw, &endpoint); err != nil || endpoint.BaseURL != baseURL || bytes.Contains(raw, []byte(token)) {
		t.Fatalf("endpoint = %s, decoded=%#v, err=%v", raw, endpoint, err)
	}
	info, err := os.Stat(endpointPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("endpoint mode = %v, err=%v", info.Mode(), err)
	}
	unixResponse, err := unixHTTPClient(unixPath).Get("http://ago/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = unixResponse.Body.Close()
	if unixResponse.StatusCode != http.StatusOK {
		t.Fatalf("unauthenticated Unix status = %d", unixResponse.StatusCode)
	}
	for name, authorization := range map[string]string{"missing": "", "wrong": "Bearer 0123456789abcdef0123456789abcdeg", "correct": "Bearer " + token} {
		t.Run(name, func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodGet, baseURL+"/v1/health", nil)
			request.Header.Set("Authorization", authorization)
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			want := http.StatusUnauthorized
			if name == "correct" {
				want = http.StatusOK
			}
			if response.StatusCode != want {
				t.Fatalf("status = %d, want %d", response.StatusCode, want)
			}
		})
	}
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	if err := unixServer.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	if err := <-result; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve() error = %v", err)
	}
	if err := <-unixResult; !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Unix Serve() error = %v", err)
	}
	if err := removeTCPEndpoint(endpointPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(endpointPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("endpoint remained after shutdown: %v", err)
	}
}

func TestResolveExecutableReturnsCanonicalAbsolutePath(t *testing.T) {
	path, err := resolveExecutable("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		t.Fatalf("resolved executable = %q", path)
	}
	if resolved, err := filepath.EvalSymlinks(path); err != nil || resolved != path {
		t.Fatalf("executable is not canonical: resolved=%q err=%v", resolved, err)
	}
}

func TestProductionVerificationCatalogAndToolAreServerOwned(t *testing.T) {
	catalog := productionVerificationCatalog("/usr/bin/go")
	check, err := catalog.Resolve(context.Background(), productionGoTestCheckID)
	if err != nil {
		t.Fatal(err)
	}
	if check.Executable != "/usr/bin/go" || !reflect.DeepEqual(check.Args, []string{"test", "./..."}) || check.Timeout != 2*time.Minute {
		t.Fatalf("production check = %#v", check)
	}
	if _, err := catalog.Resolve(context.Background(), "model-command"); err == nil {
		t.Fatal("catalog accepted an unregistered check")
	}
	tool := productionVerificationTool()
	if tool.Name != productionVerificationToolName || string(tool.InputSchema) != `{"type":"object","properties":{"check_id":{"type":"string","enum":["go-test"]}},"required":["check_id"],"additionalProperties":false}` {
		t.Fatalf("verification tool = %#v", tool)
	}
}

func TestProductionVerificationToolUsesDurableToolCallIdentityAndStrictInput(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	created, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: "create-verification", IdempotencyKey: "create-verification", ActorID: "test", Type: agoprotocol.CommandThreadCreate,
	}, agothreadstore.ThreadSpec{Workspace: workspace, Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	executor := &productionVerifierFake{result: agoverifier.ExecutionResult{Output: []byte("ok")}}
	verifier := agoverifier.New(store, store, agoverifier.StaticCatalog{
		productionGoTestCheckID: {Executable: "/usr/bin/true"},
	}, executor, agoverifier.Limits{})
	tools := productionTools{store: store, verifier: verifier}
	call := agocoordinator.ToolCall{
		ThreadID: created.ThreadID, TurnID: "turn-7", CallID: "call-9", Name: productionVerificationToolName,
		Input: map[string]any{"check_id": productionGoTestCheckID},
	}
	result, err := tools.ExecuteTool(context.Background(), call)
	if err != nil || result.Error {
		t.Fatalf("ExecuteTool() = %#v, %v", result, err)
	}
	var record agothreadstore.VerificationCheck
	if err := json.Unmarshal([]byte(result.Output), &record); err != nil || record.IdempotencyKey != "verify:call-9:final" || record.Status != agothreadstore.VerificationPassed {
		t.Fatalf("verification output = %q, %v", result.Output, err)
	}
	executed, calls := executor.snapshot()
	if calls != 1 || executed.ThreadID != created.ThreadID || executed.TurnID != "turn-7" || executed.ToolCallID != "call-9" {
		t.Fatalf("executor request = %#v, calls = %d", executed, calls)
	}

	for name, input := range map[string]map[string]any{
		"executable": {"check_id": productionGoTestCheckID, "executable": "/bin/sh"},
		"arguments":  {"check_id": productionGoTestCheckID, "args": []any{"-c", "id"}},
		"workspace":  {"check_id": productionGoTestCheckID, "workspace": "/tmp"},
		"missing":    {},
	} {
		t.Run(name, func(t *testing.T) {
			changed := call
			changed.CallID = "invalid-" + name
			changed.Input = input
			if _, err := tools.ExecuteTool(context.Background(), changed); err == nil {
				t.Fatal("ExecuteTool() accepted non-check_id input")
			}
		})
	}
	if _, calls = executor.snapshot(); calls != 1 {
		t.Fatalf("invalid input reached executor; calls = %d", calls)
	}
	if _, err := (productionTools{}).ExecuteTool(context.Background(), call); err == nil {
		t.Fatal("ExecuteTool() did not fail closed without verifier")
	}
}

type productionVerifierFake struct {
	mu      sync.Mutex
	calls   int
	request agoverifier.ExecutionRequest
	result  agoverifier.ExecutionResult
}

func (executor *productionVerifierFake) Execute(_ context.Context, request agoverifier.ExecutionRequest) (agoverifier.ExecutionResult, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.calls++
	executor.request = request
	return executor.result, nil
}

func (executor *productionVerifierFake) snapshot() (agoverifier.ExecutionRequest, int) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return executor.request, executor.calls
}

func socketTestPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("/tmp", "ago-socket-")
	if err != nil {
		t.Fatalf("create short socket directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return filepath.Join(directory, "ago.sock")
}
