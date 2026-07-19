package agocoordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
)

func TestCoordinatorRunsAndDrainsThreadsWithoutAUIClient(t *testing.T) {
	store := openStore(t)
	executor := newFakeExecutor()
	coordinator := New(store, executor)
	threadID := createThread(t, store, "create-headless")

	first, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-first"), agothreadstore.MessageInput{
		Content: json.RawMessage(`{"text":"first"}`),
		Class:   agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatalf("Submit(first) error = %v", err)
	}
	firstRun := receiveRun(t, executor.started)
	if firstRun.ThreadID != threadID || firstRun.TurnID != first.ActiveTurnID || string(firstRun.Content) != `{"text":"first"}` || firstRun.Workspace == "" || firstRun.Mode != agoprotocol.AgentModeMedium || firstRun.Executor.Type != agoprotocol.ExecutorLocal {
		t.Fatalf("first run = %#v", firstRun)
	}
	if len(firstRun.Context.Tail) != 3 || firstRun.Context.Tail[1].Type != agoprotocol.EventMessageAccepted || firstRun.Context.Tail[2].Type != agoprotocol.EventTurnStarted {
		t.Fatalf("first run context = %#v", firstRun.Context)
	}

	queued, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-second"), agothreadstore.MessageInput{
		Content: json.RawMessage(`{"text":"second"}`),
		Class:   agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatalf("Submit(second) error = %v", err)
	}
	if len(queued.Queue) != 1 {
		t.Fatalf("queued state = %#v, want one pending message", queued)
	}

	executor.finish <- nil
	secondRun := receiveRun(t, executor.started)
	if secondRun.ThreadID != threadID || secondRun.TurnID == firstRun.TurnID || string(secondRun.Content) != `{"text":"second"}` {
		t.Fatalf("second run = %#v", secondRun)
	}
	executor.finish <- nil
	waitFor(t, func() bool {
		state, err := store.Mailbox(context.Background(), threadID)
		return err == nil && state.Activity == agoprotocol.ActivityIdle && state.ActiveTurnID == ""
	})
}

func TestCoordinatorRunsDistinctThreadsConcurrently(t *testing.T) {
	store := openStore(t)
	executor := newFakeExecutor()
	coordinator := New(store, executor)
	firstThread := createThread(t, store, "create-concurrent-first")
	secondThread := createThread(t, store, "create-concurrent-second")
	for _, item := range []struct{ threadID, key string }{{firstThread, "first"}, {secondThread, "second"}} {
		if _, err := coordinator.Submit(context.Background(), command(item.threadID, agoprotocol.CommandMessageSubmit, "submit-concurrent-"+item.key), agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"run"}`), Class: agoprotocol.QueueNormal}); err != nil {
			t.Fatal(err)
		}
	}
	started := map[string]bool{}
	started[receiveRun(t, executor.started).ThreadID] = true
	started[receiveRun(t, executor.started).ThreadID] = true
	if !started[firstThread] || !started[secondThread] || len(started) != 2 {
		t.Fatalf("concurrent starts = %#v", started)
	}
	executor.finish <- nil
	executor.finish <- nil
	waitFor(t, func() bool {
		first, firstErr := store.Mailbox(context.Background(), firstThread)
		second, secondErr := store.Mailbox(context.Background(), secondThread)
		return firstErr == nil && secondErr == nil && first.Activity == agoprotocol.ActivityIdle && second.Activity == agoprotocol.ActivityIdle
	})
}

func TestCoordinatorInterruptCancelsOldRunBeforeReplacement(t *testing.T) {
	store := openStore(t)
	executor := newFakeExecutor()
	coordinator := New(store, executor)
	threadID := createThread(t, store, "create-interrupt")

	started, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-old"), agothreadstore.MessageInput{
		Content: json.RawMessage(`{"text":"old"}`),
		Class:   agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	oldRun := receiveRun(t, executor.started)

	interrupting, err := coordinator.InterruptAndSubmit(context.Background(), command(threadID, agoprotocol.CommandTurnInterrupt, "interrupt"), started.ActiveTurnID, agothreadstore.MessageInput{
		Content: json.RawMessage(`{"text":"replacement"}`),
		Class:   agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatalf("InterruptAndSubmit() error = %v", err)
	}
	if !interrupting.CancelRequested || len(interrupting.Queue) != 1 {
		t.Fatalf("interrupting state = %#v", interrupting)
	}

	replacement := receiveRun(t, executor.started)
	if replacement.TurnID == oldRun.TurnID || string(replacement.Content) != `{"text":"replacement"}` {
		t.Fatalf("replacement run = %#v", replacement)
	}
	select {
	case cancelledTurn := <-executor.cancelled:
		if cancelledTurn != oldRun.TurnID {
			t.Fatalf("cancelled turn = %q, want %q", cancelledTurn, oldRun.TurnID)
		}
	default:
		t.Fatal("old executor run was not cancelled")
	}
	executor.finish <- nil
}

func TestCoordinatorPersistsExecutorFailure(t *testing.T) {
	store := openStore(t)
	executor := newFakeExecutor()
	coordinator := New(store, executor)
	threadID := createThread(t, store, "create-failure")

	_, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-failure"), agothreadstore.MessageInput{
		Content: json.RawMessage(`{"text":"fail"}`),
		Class:   agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	receiveRun(t, executor.started)
	executor.finish <- errors.New("executor crashed")
	waitFor(t, func() bool {
		state, err := store.Mailbox(context.Background(), threadID)
		return err == nil && state.Activity == agoprotocol.ActivityError && state.ActiveTurnID == ""
	})
}

func TestCoordinatorPersistsStreamingEventsBeforeTurnCompletion(t *testing.T) {
	store := openStore(t)
	execution := &fakeExecution{events: make(chan ExecutionEvent, 4), wait: make(chan error, 1)}
	executor := &fakeSessionExecutor{started: make(chan TurnRequest, 1), execution: execution}
	observer := &lifecycleRuntime{}
	coordinator := NewWithToolRuntime(store, executor, observer)
	threadID := createThread(t, store, "create-streaming")

	_, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-streaming"), agothreadstore.MessageInput{
		Content: json.RawMessage(`{"text":"stream"}`), Class: agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	run := receiveRun(t, executor.started)
	execution.events <- ExecutionEvent{Index: 1, Type: "started"}
	execution.events <- ExecutionEvent{Index: 2, Type: "text", Payload: json.RawMessage(`{"delta":"hello"}`)}
	execution.events <- ExecutionEvent{Index: 3, Type: "stopped", Payload: json.RawMessage(`{"reason":"stop"}`)}
	execution.events <- ExecutionEvent{Index: 4, Type: "settled"}
	waitFor(t, func() bool {
		events, err := store.Replay(context.Background(), threadID, 0, 0)
		return err == nil && len(events) == 7 && events[3].Type == agoprotocol.EventAgentStarted && events[4].Type == agoprotocol.EventAssistantTextDelta && events[6].Type == agoprotocol.EventAgentSettled
	})
	state, err := store.Mailbox(context.Background(), threadID)
	if err != nil || state.ActiveTurnID != run.TurnID {
		t.Fatalf("streaming mailbox settled before executor: %#v, %v", state, err)
	}
	close(execution.events)
	execution.wait <- nil
	waitFor(t, func() bool {
		events, err := store.Replay(context.Background(), threadID, 0, 0)
		return err == nil && len(events) == 8 && events[7].Type == agoprotocol.EventTurnCompleted
	})
	observer.mu.Lock()
	hooks := append([]string(nil), observer.hooks...)
	observer.mu.Unlock()
	if fmt.Sprint(hooks) != fmt.Sprint([]string{"session.start", "agent.start", "agent.end"}) {
		t.Fatalf("successful lifecycle hooks = %v", hooks)
	}
}

func TestCoordinatorRecordsProviderUsageAtDurableAssistantBoundary(t *testing.T) {
	store := openStore(t)
	created, err := store.CreateAtomicThread(context.Background(), agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: "create-usage", IdempotencyKey: "create-usage", ActorID: "test", Type: agoprotocol.CommandThreadCreate,
	}, agothreadstore.AtomicCreateInput{
		Spec:    agothreadstore.ThreadSpec{Title: "usage", Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}},
		Project: agothreadstore.ProjectIdentity{ProjectID: "project"}, Agent: agothreadstore.AgentDefinitionSnapshot{DefinitionID: "ago.default", Version: "1", DisplayName: "Ago", SystemInstructionsDigest: "sha256:test", DefaultMode: agoprotocol.AgentModeMedium},
		InitialMessage: json.RawMessage(`{"text":"measure"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := New(store, nil)
	event := ExecutionEvent{Index: 1, Type: "assistant_completed", Payload: json.RawMessage(`{"type":"assistant_completed","message":{"role":"assistant"},"provider_usage":{"provider":"openai","model":"gpt-test","request_id":"provider-request","status":"final","usage":{"input_tokens":101,"output_tokens":23,"cache_read_tokens":47,"cache_write_tokens":11,"total_tokens":997,"cache_write_1h_tokens":3,"reasoning_tokens":7},"cost":{"input":0.00101,"output":0.00046,"cache_read":0.000047,"cache_write":0.00011,"total":9.99}}}`)}
	request := TurnRequest{ThreadID: created.ThreadID, TurnID: created.ActiveTurnID, Attempt: 1}
	if err := coordinator.appendExecutionEvent(context.Background(), request, event); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.appendExecutionEvent(context.Background(), request, event); err != nil {
		t.Fatalf("exact durable retry: %v", err)
	}
	records, err := store.ProviderUsageLedger(context.Background(), created.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Provider != "openai" || records[0].RequestID != "provider-request" || records[0].Usage.TotalTokens != "997" || records[0].Usage.CacheWrite1HTokens != "3" || records[0].Usage.ReasoningTokens != "7" || records[0].Cost.Total != "9.99" {
		t.Fatalf("provider usage records = %#v", records)
	}
}

func TestCoordinatorEmitsAgentEndAndCleansExecutionOnStreamFailure(t *testing.T) {
	store := openStore(t)
	execution := &fakeExecution{events: make(chan ExecutionEvent, 2), wait: make(chan error, 1)}
	executor := &fakeSessionExecutor{started: make(chan TurnRequest, 1), execution: execution}
	observer := &lifecycleRuntime{}
	coordinator := NewWithToolRuntime(store, executor, observer)
	threadID := createThread(t, store, "create-lifecycle-failure")
	if _, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-lifecycle-failure"), agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"fail stream"}`), Class: agoprotocol.QueueNormal}); err != nil {
		t.Fatal(err)
	}
	receiveRun(t, executor.started)
	execution.events <- ExecutionEvent{Index: 1, Type: "started", Payload: json.RawMessage(`{"type":"started"}`)}
	execution.events <- ExecutionEvent{Index: 3, Type: "text", Payload: json.RawMessage(`{"delta":"invalid index"}`)}
	execution.wait <- nil
	waitFor(t, func() bool {
		state, err := store.Mailbox(context.Background(), threadID)
		return err == nil && state.Activity == agoprotocol.ActivityError
	})
	observer.mu.Lock()
	hooks := append([]string(nil), observer.hooks...)
	observer.mu.Unlock()
	if fmt.Sprint(hooks) != fmt.Sprint([]string{"session.start", "agent.start", "agent.end"}) {
		t.Fatalf("failure lifecycle hooks = %v", hooks)
	}
}

func TestCoordinatorEmitsSessionStartOnceAcrossTwoTurns(t *testing.T) {
	store := openStore(t)
	firstExecution := &fakeExecution{events: make(chan ExecutionEvent, 3), wait: make(chan error, 1)}
	executor := &sequencedSessionExecutor{started: make(chan TurnRequest, 2), executions: chanOfExecutions(firstExecution)}
	observer := &lifecycleRuntime{}
	coordinator := NewWithToolRuntime(store, executor, observer)
	threadID := createThread(t, store, "create-session-lifecycle")
	if _, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-session-first"), agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"first"}`), Class: agoprotocol.QueueNormal}); err != nil {
		t.Fatal(err)
	}
	receiveRun(t, executor.started)
	finishSessionExecution(firstExecution)
	waitFor(t, func() bool {
		state, err := store.Mailbox(context.Background(), threadID)
		return err == nil && state.Activity == agoprotocol.ActivityIdle
	})
	secondExecution := &fakeExecution{events: make(chan ExecutionEvent, 3), wait: make(chan error, 1)}
	executor.executions <- secondExecution
	if _, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-session-second"), agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"second"}`), Class: agoprotocol.QueueNormal}); err != nil {
		t.Fatal(err)
	}
	receiveRun(t, executor.started)
	finishSessionExecution(secondExecution)
	waitFor(t, func() bool {
		state, err := store.Mailbox(context.Background(), threadID)
		return err == nil && state.Activity == agoprotocol.ActivityIdle
	})
	observer.mu.Lock()
	hooks := append([]string(nil), observer.hooks...)
	observer.mu.Unlock()
	if fmt.Sprint(hooks) != fmt.Sprint([]string{"session.start", "agent.start", "agent.end", "agent.start", "agent.end"}) {
		t.Fatalf("two-turn lifecycle hooks = %v", hooks)
	}
}

func TestCoordinatorDurablyAcceptsAndDeliversSteerExactlyOnce(t *testing.T) {
	store := openStore(t)
	execution := &fakeExecution{events: make(chan ExecutionEvent, 4), wait: make(chan error, 1), controls: make(chan ExecutionControl, 2)}
	executor := &fakeSessionExecutor{started: make(chan TurnRequest, 1), execution: execution}
	coordinator := New(store, executor)
	threadID := createThread(t, store, "create-steer-delivery")

	started, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "steer-active"), agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"active"}`), Class: agoprotocol.QueueNormal})
	if err != nil {
		t.Fatal(err)
	}
	receiveRun(t, executor.started)
	queued, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "steer-queued"), agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"urgent"}`), Class: agoprotocol.QueueNormal})
	if err != nil || len(queued.Queue) != 1 {
		t.Fatalf("queue steer input: %#v, %v", queued, err)
	}
	steerCommand := command(threadID, agoprotocol.CommandMessageSteer, "deliver-steer")
	if _, err := coordinator.Steer(context.Background(), steerCommand, queued.Queue[0].QueueItemID, started.ActiveTurnID); err != nil {
		t.Fatal(err)
	}
	select {
	case control := <-execution.controls:
		if control.Type != "steer" || string(control.Payload) != `{"text":"urgent"}` {
			t.Fatalf("control = %#v", control)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("durable steer was not delivered")
	}
	if _, err := coordinator.Steer(context.Background(), steerCommand, queued.Queue[0].QueueItemID, started.ActiveTurnID); err != nil {
		t.Fatal(err)
	}
	select {
	case duplicate := <-execution.controls:
		t.Fatalf("retry duplicated steer delivery: %#v", duplicate)
	case <-time.After(30 * time.Millisecond):
	}
	execution.events <- ExecutionEvent{Index: 1, Type: "started"}
	execution.events <- ExecutionEvent{Index: 2, Type: "settled"}
	close(execution.events)
	execution.wait <- nil
}

func TestCoordinatorPersistsToolRequestBeforeDispatchAndResultBeforeDelivery(t *testing.T) {
	store := openStore(t)
	execution := &fakeExecution{events: make(chan ExecutionEvent, 8), wait: make(chan error, 1), controls: make(chan ExecutionControl, 1)}
	executor := &fakeSessionExecutor{started: make(chan TurnRequest, 1), execution: execution}
	var threadID string
	tools := toolRuntimeFunc(func(_ context.Context, call ToolCall) (ToolResult, error) {
		events, err := store.Replay(context.Background(), threadID, 0, 0)
		if err != nil || events[len(events)-1].Type != agoprotocol.EventToolRequested {
			t.Fatalf("tool dispatched before durable request: %#v, %v", events, err)
		}
		if call.CallID != "call-1" || call.Name != "ago_echo" || call.Input["text"] != "value" {
			t.Fatalf("tool call = %#v", call)
		}
		return ToolResult{Output: "AGO:value"}, nil
	})
	coordinator := NewWithToolRuntime(store, executor, tools)
	threadID = createThread(t, store, "create-tool-boundary")
	_, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "tool-boundary"), agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"tool"}`), Class: agoprotocol.QueueNormal})
	if err != nil {
		t.Fatal(err)
	}
	receiveRun(t, executor.started)
	execution.events <- ExecutionEvent{Index: 1, Type: "started", Payload: json.RawMessage(`{"type":"started"}`)}
	execution.events <- ExecutionEvent{Index: 2, Type: "tool_invocation", Payload: json.RawMessage(`{"type":"tool_invocation","callId":"call-1","name":"ago_echo","input":{"text":"value"}}`)}
	select {
	case control := <-execution.controls:
		events, replayErr := store.Replay(context.Background(), threadID, 0, 0)
		if replayErr != nil || len(events) < 2 || events[len(events)-2].Type != agoprotocol.EventToolCompleted || events[len(events)-1].Type != agoprotocol.EventToolResultPrepared {
			t.Fatalf("tool result delivered before durable completion: %#v, %v", events, replayErr)
		}
		if control.Type != "tool_result" || !bytes.Contains(control.Payload, []byte(`"output":"AGO:value"`)) {
			t.Fatalf("tool result control = %#v", control)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("tool result was not delivered")
	}
	execution.events <- ExecutionEvent{Index: 3, Type: "tool_finished", Payload: json.RawMessage(`{"type":"tool_finished","callId":"call-1","error":false}`)}
	execution.events <- ExecutionEvent{Index: 4, Type: "stopped", Payload: json.RawMessage(`{"type":"stopped","reason":"stop"}`)}
	execution.events <- ExecutionEvent{Index: 5, Type: "settled", Payload: json.RawMessage(`{"type":"settled"}`)}
	close(execution.events)
	execution.wait <- nil
}

func TestCoordinatorRecoveryResumesOrphanedTurnFromSafeInferenceBoundary(t *testing.T) {
	store := openStore(t)
	threadID := createThread(t, store, "create-orphan")
	started, err := store.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-orphan"), agothreadstore.MessageInput{
		Content: json.RawMessage(`{"text":"resume safely"}`),
		Class:   agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatalf("seed orphaned turn: %v", err)
	}
	executor := newFakeExecutor()
	coordinator := New(store, executor)

	if err := coordinator.Recover(context.Background()); err != nil {
		t.Fatalf("Recover() error = %v", err)
	}
	select {
	case run := <-executor.started:
		if run.TurnID != started.ActiveTurnID || run.Attempt != 1 {
			t.Fatalf("recovered run = %#v", run)
		}
	case <-time.After(time.Second):
		t.Fatal("safe orphaned turn was not resumed")
	}
	executor.finish <- nil
}

func TestCoordinatorRecoveryDoesNotReplayUnresolvedToolSideEffect(t *testing.T) {
	store := openStore(t)
	threadID := createThread(t, store, "create-unsafe-orphan")
	started, err := store.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-unsafe-orphan"), agothreadstore.MessageInput{
		Content: json.RawMessage(`{"text":"run a side effect"}`), Class: agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := "seed-unsafe-tool"
	_, err = store.AppendTurnEvents(context.Background(), agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: key, IdempotencyKey: key, ActorID: "test",
		Type: agoprotocol.CommandTurnEventAppend, ThreadID: threadID,
	}, started.ActiveTurnID, agothreadstore.EventDraft{Type: agoprotocol.EventToolRequested, Visibility: agoprotocol.VisibilityUser, Payload: mustJSON(map[string]any{
		"turn_id": started.ActiveTurnID, "executor_event_index": 2,
		"event": map[string]any{"type": "tool_invocation", "callId": "side-effect-1", "name": "write", "input": map[string]any{"path": "x"}},
	})})
	if err != nil {
		t.Fatal(err)
	}
	executor := newFakeExecutor()
	coordinator := New(store, executor)
	if err := coordinator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case run := <-executor.started:
		t.Fatalf("unsafe side effect was replayed: %#v", run)
	default:
	}
	state, err := store.Mailbox(context.Background(), threadID)
	if err != nil || state.Activity != agoprotocol.ActivityError || state.ActiveTurnID != "" {
		t.Fatalf("unsafe recovered mailbox = %#v, %v", state, err)
	}
	events, err := store.Replay(context.Background(), threadID, started.LastSequence, 0)
	if err != nil || len(events) != 2 || events[1].Type != agoprotocol.EventTurnFailed || !bytes.Contains(events[1].Payload, []byte("unresolved tool side-effect boundary")) {
		t.Fatalf("unsafe recovery events = %#v, %v", events, err)
	}
}

func TestCoordinatorRecoveryAfterPreparedToolResultUsesSecondExecutionAttemptWithoutRedispatch(t *testing.T) {
	store := openStore(t)
	threadID := createThread(t, store, "create-prepared-orphan")
	started, err := store.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-prepared-orphan"), agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"continue after prepared result"}`), Class: agoprotocol.QueueNormal})
	if err != nil {
		t.Fatal(err)
	}
	drafts := []agothreadstore.EventDraft{
		{Type: agoprotocol.EventAgentStarted, Visibility: agoprotocol.VisibilityInternal, Payload: mustJSON(map[string]any{"turn_id": started.ActiveTurnID, "executor_event_index": 1, "event": map[string]any{"type": "started"}})},
		{Type: agoprotocol.EventToolRequested, Visibility: agoprotocol.VisibilityUser, Payload: mustJSON(map[string]any{"turn_id": started.ActiveTurnID, "executor_event_index": 2, "event": map[string]any{"type": "tool_invocation", "callId": "prepared-1", "name": "write", "input": map[string]any{"path": "x"}}})},
		{Type: agoprotocol.EventToolCompleted, Visibility: agoprotocol.VisibilityUser, Payload: mustJSON(map[string]any{"turn_id": started.ActiveTurnID, "call_id": "prepared-1", "name": "write", "output": "ok", "error": false})},
		{Type: agoprotocol.EventToolResultPrepared, Visibility: agoprotocol.VisibilityInternal, Payload: mustJSON(map[string]any{"turn_id": started.ActiveTurnID, "call_id": "prepared-1", "name": "write", "output": "ok", "error": false})},
	}
	for index, draft := range drafts {
		key := fmt.Sprintf("seed-prepared-%d", index)
		if _, err := store.AppendTurnEvents(context.Background(), agoprotocol.Command{SchemaVersion: agoprotocol.SchemaVersion, CommandID: key, IdempotencyKey: key, ActorID: "test", Type: agoprotocol.CommandTurnEventAppend, ThreadID: threadID}, started.ActiveTurnID, draft); err != nil {
			t.Fatal(err)
		}
	}
	execution := &fakeExecution{events: make(chan ExecutionEvent, 4), wait: make(chan error, 1), controls: make(chan ExecutionControl, 1)}
	executor := &fakeSessionExecutor{started: make(chan TurnRequest, 1), execution: execution}
	toolCalls := 0
	coordinator := NewWithToolRuntime(store, executor, toolRuntimeFunc(func(context.Context, ToolCall) (ToolResult, error) {
		toolCalls++
		return ToolResult{}, errors.New("prepared tool must not be redispatched")
	}))
	if err := coordinator.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	run := receiveRun(t, executor.started)
	if run.Attempt != 2 || run.TurnID != started.ActiveTurnID {
		t.Fatalf("recovery request = %#v", run)
	}
	execution.events <- ExecutionEvent{Index: 1, Type: "started", Payload: json.RawMessage(`{"type":"started"}`)}
	execution.events <- ExecutionEvent{Index: 2, Type: "stopped", Payload: json.RawMessage(`{"type":"stopped","reason":"done"}`)}
	execution.events <- ExecutionEvent{Index: 3, Type: "settled", Payload: json.RawMessage(`{"type":"settled"}`)}
	close(execution.events)
	execution.wait <- nil
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		state, mailboxErr := store.Mailbox(context.Background(), threadID)
		if mailboxErr == nil && state.Activity == agoprotocol.ActivityIdle {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	events, err := store.Replay(context.Background(), threadID, 0, 0)
	startedCount := 0
	for _, event := range events {
		if event.Type == agoprotocol.EventAgentStarted {
			startedCount++
		}
	}
	if err != nil || toolCalls != 0 || startedCount != 2 || events[len(events)-1].Type != agoprotocol.EventTurnCompleted {
		t.Fatalf("prepared recovery events=%#v toolCalls=%d started=%d err=%v", events, toolCalls, startedCount, err)
	}
}

func TestCoordinatorShutdownCancelsAndSettlesActiveTurn(t *testing.T) {
	store := openStore(t)
	executor := newFakeExecutor()
	coordinator := New(store, executor)
	threadID := createThread(t, store, "create-shutdown")
	started, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-shutdown"), agothreadstore.MessageInput{
		Content: json.RawMessage(`{"text":"long run"}`),
		Class:   agoprotocol.QueueNormal,
	})
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	receiveRun(t, executor.started)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := coordinator.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	state, err := store.Mailbox(context.Background(), threadID)
	if err != nil {
		t.Fatalf("Mailbox() error = %v", err)
	}
	if state.Activity != agoprotocol.ActivityIdle || state.ActiveTurnID != "" || state.CancelRequested {
		t.Fatalf("shutdown mailbox = %#v, want settled idle", state)
	}
	events, err := store.Replay(context.Background(), threadID, started.LastSequence, 0)
	if err != nil {
		t.Fatalf("Replay() error = %v", err)
	}
	if len(events) != 2 || events[0].Type != agoprotocol.EventTurnCancelRequested || events[1].Type != agoprotocol.EventTurnCancelled {
		t.Fatalf("shutdown events = %#v", events)
	}
}

func TestCoordinatorEmitsCompleteLifecycleWhenSessionIsCancelled(t *testing.T) {
	store := openStore(t)
	execution := &fakeExecution{events: make(chan ExecutionEvent, 1), controls: make(chan ExecutionControl, 1), wait: make(chan error, 1)}
	executor := &fakeSessionExecutor{started: make(chan TurnRequest, 1), execution: execution}
	observer := &lifecycleRuntime{}
	coordinator := NewWithToolRuntime(store, executor, observer)
	threadID := createThread(t, store, "create-session-cancel-lifecycle")
	if _, err := coordinator.Submit(context.Background(), command(threadID, agoprotocol.CommandMessageSubmit, "submit-session-cancel-lifecycle"), agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"cancel"}`), Class: agoprotocol.QueueNormal}); err != nil {
		t.Fatal(err)
	}
	receiveRun(t, executor.started)
	execution.events <- ExecutionEvent{Index: 1, Type: "started", Payload: json.RawMessage(`{"type":"started"}`)}
	waitFor(t, func() bool {
		observer.mu.Lock()
		defer observer.mu.Unlock()
		return len(observer.hooks) == 2
	})
	execution.wait <- nil
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := coordinator.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	observer.mu.Lock()
	hooks := append([]string(nil), observer.hooks...)
	observer.mu.Unlock()
	if fmt.Sprint(hooks) != fmt.Sprint([]string{"session.start", "agent.start", "agent.end"}) {
		t.Fatalf("cancelled lifecycle hooks = %v", hooks)
	}
	select {
	case control := <-execution.controls:
		if control.Type != "abort" {
			t.Fatalf("cancel control = %#v", control)
		}
	default:
		t.Fatal("session cancellation did not send abort")
	}
}

type fakeExecutor struct {
	started   chan TurnRequest
	finish    chan error
	cancelled chan string
}

type fakeSessionExecutor struct {
	started   chan TurnRequest
	execution *fakeExecution
}

type sequencedSessionExecutor struct {
	started    chan TurnRequest
	executions chan *fakeExecution
}

func (executor *fakeSessionExecutor) Run(context.Context, TurnRequest) error {
	return errors.New("legacy Run must not be called for a session executor")
}

func (executor *fakeSessionExecutor) Start(_ context.Context, request TurnRequest) (Execution, error) {
	executor.started <- request
	return executor.execution, nil
}

func (*sequencedSessionExecutor) Run(context.Context, TurnRequest) error {
	return errors.New("legacy Run must not be called for a session executor")
}

func (executor *sequencedSessionExecutor) Start(_ context.Context, request TurnRequest) (Execution, error) {
	executor.started <- request
	return <-executor.executions, nil
}

func chanOfExecutions(executions ...*fakeExecution) chan *fakeExecution {
	result := make(chan *fakeExecution, len(executions)+1)
	for _, execution := range executions {
		result <- execution
	}
	return result
}

func finishSessionExecution(execution *fakeExecution) {
	execution.events <- ExecutionEvent{Index: 1, Type: "started", Payload: json.RawMessage(`{"type":"started"}`)}
	execution.events <- ExecutionEvent{Index: 2, Type: "stopped", Payload: json.RawMessage(`{"type":"stopped","reason":"stop"}`)}
	execution.events <- ExecutionEvent{Index: 3, Type: "settled", Payload: json.RawMessage(`{"type":"settled"}`)}
	close(execution.events)
	execution.wait <- nil
}

type fakeExecution struct {
	events   chan ExecutionEvent
	wait     chan error
	controls chan ExecutionControl
}

type toolRuntimeFunc func(context.Context, ToolCall) (ToolResult, error)

func (function toolRuntimeFunc) ExecuteTool(ctx context.Context, call ToolCall) (ToolResult, error) {
	return function(ctx, call)
}

type lifecycleRuntime struct {
	mu    sync.Mutex
	hooks []string
}

func (*lifecycleRuntime) ExecuteTool(context.Context, ToolCall) (ToolResult, error) {
	return ToolResult{}, nil
}
func (runtime *lifecycleRuntime) ObserveLifecycle(_ context.Context, hook string, _ any) {
	runtime.mu.Lock()
	runtime.hooks = append(runtime.hooks, hook)
	runtime.mu.Unlock()
}

func (execution *fakeExecution) Events() <-chan ExecutionEvent { return execution.events }
func (execution *fakeExecution) Send(_ context.Context, control ExecutionControl) error {
	if execution.controls != nil {
		execution.controls <- control
	}
	return nil
}
func (execution *fakeExecution) CloseInput() error { return nil }
func (execution *fakeExecution) Wait() error       { return <-execution.wait }

func newFakeExecutor() *fakeExecutor {
	return &fakeExecutor{
		started:   make(chan TurnRequest, 8),
		finish:    make(chan error, 8),
		cancelled: make(chan string, 8),
	}
}

func (executor *fakeExecutor) Run(ctx context.Context, request TurnRequest) error {
	executor.started <- request
	select {
	case err := <-executor.finish:
		return err
	case <-ctx.Done():
		executor.cancelled <- request.TurnID
		return ctx.Err()
	}
}

func openStore(t *testing.T) *agothreadstore.Store {
	t.Helper()
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createThread(t *testing.T, store *agothreadstore.Store, key string) string {
	t.Helper()
	result, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{
		SchemaVersion:  agoprotocol.SchemaVersion,
		CommandID:      "cmd-" + key,
		IdempotencyKey: "request-" + key,
		ActorID:        "user-1",
		Type:           agoprotocol.CommandThreadCreate,
	}, agothreadstore.ThreadSpec{
		Title:     key,
		Workspace: filepath.Join(t.TempDir(), "workspace"),
		Mode:      agoprotocol.AgentModeMedium,
		Executor:  agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal},
	})
	if err != nil {
		t.Fatalf("CreateThread() error = %v", err)
	}
	return result.ThreadID
}

func command(threadID string, commandType agoprotocol.CommandType, key string) agoprotocol.Command {
	return agoprotocol.Command{
		SchemaVersion:  agoprotocol.SchemaVersion,
		CommandID:      "cmd-" + key,
		IdempotencyKey: "request-" + key,
		ActorID:        "user-1",
		Type:           commandType,
		ThreadID:       threadID,
	}
}

func receiveRun(t *testing.T, runs <-chan TurnRequest) TurnRequest {
	t.Helper()
	select {
	case run := <-runs:
		return run
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for executor run")
		return TurnRequest{}
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}
