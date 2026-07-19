package agocoordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
)

type TurnRequest struct {
	ThreadID  string
	TurnID    string
	Attempt   uint64
	Content   json.RawMessage
	Workspace string
	Mode      agoprotocol.AgentMode
	Executor  agoprotocol.ExecutorTarget
	Tools     []ExternalTool
	Context   agothreadstore.ContextProjection `json:"-"`
}

type ExternalTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type ToolCatalog interface {
	ExternalTools(context.Context, string) ([]ExternalTool, error)
}

type Executor interface {
	Run(context.Context, TurnRequest) error
}

type SessionExecutor interface {
	Start(context.Context, TurnRequest) (Execution, error)
}

type Execution interface {
	Events() <-chan ExecutionEvent
	Send(context.Context, ExecutionControl) error
	CloseInput() error
	Wait() error
}

type ExecutionEvent struct {
	Index   uint64
	Type    string
	Payload json.RawMessage
}

type ExecutionControl struct {
	Type    string
	Payload json.RawMessage
}

type ToolCall struct {
	ThreadID string
	TurnID   string
	CallID   string
	Name     string
	Input    map[string]any
}

type ToolResult struct {
	Output string
	Error  bool
}

type ToolRuntime interface {
	ExecuteTool(context.Context, ToolCall) (ToolResult, error)
}

type ToolResultObserver interface {
	ObserveToolResult(context.Context, ToolCall, ToolResult) ToolResult
}

type LifecycleObserver interface {
	ObserveLifecycle(context.Context, string, any)
}

type ContextPreparer interface {
	PrepareContext(context.Context, TurnRequest) (agothreadstore.ContextProjection, error)
}

type Coordinator struct {
	store    *agothreadstore.Store
	executor Executor
	tools    ToolRuntime
	preparer ContextPreparer

	mu       sync.Mutex
	running  map[string]*runningTurn
	sessions map[string]struct{}
	wait     sync.WaitGroup
}

type runningTurn struct {
	threadID  string
	cancel    context.CancelFunc
	execution Execution
	controlMu sync.Mutex
}

func New(store *agothreadstore.Store, executor Executor) *Coordinator {
	return &Coordinator{store: store, executor: executor, running: make(map[string]*runningTurn), sessions: make(map[string]struct{})}
}

func NewWithToolRuntime(store *agothreadstore.Store, executor Executor, tools ToolRuntime) *Coordinator {
	coordinator := New(store, executor)
	coordinator.tools = tools
	return coordinator
}

func NewRuntime(store *agothreadstore.Store, executor Executor, tools ToolRuntime, preparer ContextPreparer) *Coordinator {
	coordinator := NewWithToolRuntime(store, executor, tools)
	coordinator.preparer = preparer
	return coordinator
}

func (coordinator *Coordinator) Recover(ctx context.Context) error {
	mailboxes, err := coordinator.store.ActiveMailboxes(ctx)
	if err != nil {
		return fmt.Errorf("list active turns during recovery: %w", err)
	}
	for _, mailbox := range mailboxes {
		unsafe, err := coordinator.hasUnsafeToolBoundary(ctx, mailbox.ThreadID, mailbox.ActiveTurnID)
		if err != nil {
			return fmt.Errorf("inspect orphaned turn %q: %w", mailbox.ActiveTurnID, err)
		}
		if !unsafe {
			if err := coordinator.launchState(mailbox); err != nil {
				return fmt.Errorf("resume orphaned turn %q: %w", mailbox.ActiveTurnID, err)
			}
			continue
		}
		command := internalCommand(mailbox.ThreadID, mailbox.ActiveTurnID)
		command.Type = agoprotocol.CommandTurnFail
		command.CommandID += ":recovery-fail"
		command.IdempotencyKey += ":recovery-fail"
		if _, err := coordinator.store.FailTurn(ctx, command, mailbox.ActiveTurnID, "daemon restarted at an unresolved tool side-effect boundary"); err != nil {
			return fmt.Errorf("fail orphaned turn %q: %w", mailbox.ActiveTurnID, err)
		}
	}
	return nil
}

func (coordinator *Coordinator) hasUnsafeToolBoundary(ctx context.Context, threadID, turnID string) (bool, error) {
	events, err := coordinator.store.Replay(ctx, threadID, 0, 0)
	if err != nil {
		return false, err
	}
	requested := make(map[string]bool)
	prepared := make(map[string]bool)
	for _, event := range events {
		switch event.Type {
		case agoprotocol.EventToolRequested:
			var payload struct {
				TurnID string `json:"turn_id"`
				Event  struct {
					CallID string `json:"callId"`
				} `json:"event"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || (payload.TurnID == turnID && payload.Event.CallID == "") {
				return true, nil
			}
			if payload.TurnID == turnID {
				requested[payload.Event.CallID] = true
			}
		case agoprotocol.EventToolResultPrepared:
			var payload struct {
				TurnID string `json:"turn_id"`
				CallID string `json:"call_id"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil || (payload.TurnID == turnID && payload.CallID == "") {
				return true, nil
			}
			if payload.TurnID == turnID {
				prepared[payload.CallID] = true
			}
		}
	}
	for callID := range requested {
		if !prepared[callID] {
			return true, nil
		}
	}
	return false, nil
}

func (coordinator *Coordinator) Shutdown(ctx context.Context) error {
	coordinator.mu.Lock()
	running := make(map[string]*runningTurn, len(coordinator.running))
	for turnID, turn := range coordinator.running {
		running[turnID] = turn
	}
	coordinator.mu.Unlock()
	for turnID, turn := range running {
		command := internalCommand(turn.threadID, turnID)
		command.Type = agoprotocol.CommandTurnCancel
		command.CommandID += ":shutdown-cancel"
		command.IdempotencyKey += ":shutdown-cancel"
		if _, err := coordinator.store.Cancel(ctx, command, turnID); err != nil {
			return fmt.Errorf("request cancellation for turn %q during shutdown: %w", turnID, err)
		}
		turn.cancel()
	}
	done := make(chan struct{})
	go func() {
		coordinator.wait.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (coordinator *Coordinator) Submit(ctx context.Context, command agoprotocol.Command, input agothreadstore.MessageInput) (agothreadstore.MailboxState, error) {
	state, err := coordinator.store.Submit(ctx, command, input)
	if err != nil {
		return agothreadstore.MailboxState{}, err
	}
	if err := coordinator.launchState(state); err != nil {
		return state, err
	}
	return state, nil
}

// LaunchCommitted attaches an executor only to a mailbox state that has already
// been committed by the authoritative store operation.
func (coordinator *Coordinator) LaunchCommitted(state agothreadstore.MailboxState) error {
	return coordinator.launchState(state)
}

func (coordinator *Coordinator) InterruptAndSubmit(ctx context.Context, command agoprotocol.Command, expectedTurnID string, input agothreadstore.MessageInput) (agothreadstore.MailboxState, error) {
	state, err := coordinator.store.InterruptAndSubmit(ctx, command, expectedTurnID, input)
	if err != nil {
		return agothreadstore.MailboxState{}, err
	}
	if !coordinator.cancel(expectedTurnID) {
		return state, fmt.Errorf("active executor turn %q is not attached", expectedTurnID)
	}
	return state, nil
}

func (coordinator *Coordinator) Cancel(ctx context.Context, command agoprotocol.Command, expectedTurnID string) (agothreadstore.MailboxState, error) {
	state, err := coordinator.store.Cancel(ctx, command, expectedTurnID)
	if err != nil {
		return agothreadstore.MailboxState{}, err
	}
	if !coordinator.cancel(expectedTurnID) {
		return state, fmt.Errorf("active executor turn %q is not attached", expectedTurnID)
	}
	return state, nil
}

func (coordinator *Coordinator) Steer(ctx context.Context, command agoprotocol.Command, queueItemID, expectedTurnID string) (agothreadstore.MailboxState, error) {
	state, err := coordinator.store.Steer(ctx, command, queueItemID, expectedTurnID)
	if err != nil {
		return agothreadstore.MailboxState{}, err
	}
	coordinator.mu.Lock()
	running := coordinator.running[expectedTurnID]
	coordinator.mu.Unlock()
	if running == nil {
		return state, fmt.Errorf("active executor turn %q is not attached", expectedTurnID)
	}
	if err := coordinator.deliverSteers(running); err != nil {
		return state, err
	}
	return coordinator.store.Mailbox(context.Background(), command.ThreadID)
}

func (coordinator *Coordinator) launchState(state agothreadstore.MailboxState) error {
	request, found, err := turnRequest(state)
	if err != nil {
		return err
	}
	if !found && state.Activity == agoprotocol.ActivityRunning && state.ActiveTurnID != "" {
		state.Events, err = coordinator.store.Replay(context.Background(), state.ThreadID, 0, 0)
		if err != nil {
			return fmt.Errorf("replay active turn input: %w", err)
		}
		request, found, err = turnRequest(state)
		if err != nil {
			return err
		}
	}
	if !found {
		return nil
	}
	thread, err := coordinator.store.Thread(context.Background(), state.ThreadID)
	if err != nil {
		return err
	}
	request.Workspace = thread.Workspace
	request.Mode = thread.Mode
	request.Executor = thread.Executor
	if catalog, ok := coordinator.tools.(ToolCatalog); ok {
		request.Tools, err = catalog.ExternalTools(context.Background(), state.ThreadID)
		if err != nil {
			return fmt.Errorf("load external tool catalog: %w", err)
		}
	}
	request.Context, err = coordinator.store.ContextProjection(context.Background(), state.ThreadID)
	if err != nil {
		return fmt.Errorf("project thread context: %w", err)
	}
	events, err := coordinator.store.Replay(context.Background(), state.ThreadID, 0, 0)
	if err != nil {
		return fmt.Errorf("count turn execution attempts: %w", err)
	}
	request.Attempt = 1
	for _, event := range events {
		if event.Type == agoprotocol.EventAgentStarted {
			var payload struct {
				TurnID string `json:"turn_id"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.TurnID == request.TurnID {
				request.Attempt++
			}
		}
	}
	coordinator.mu.Lock()
	if _, running := coordinator.running[request.TurnID]; running {
		coordinator.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	coordinator.running[request.TurnID] = &runningTurn{threadID: request.ThreadID, cancel: cancel}
	coordinator.wait.Add(1)
	coordinator.mu.Unlock()
	go coordinator.run(ctx, request)
	return nil
}

func (coordinator *Coordinator) run(ctx context.Context, request TurnRequest) {
	defer coordinator.wait.Done()
	var err error
	if coordinator.preparer != nil {
		request.Context, err = coordinator.preparer.PrepareContext(ctx, request)
	}
	if err != nil {
		// Preparation failures settle through the same durable turn-failure path.
	} else if executor, ok := coordinator.executor.(SessionExecutor); ok {
		err = coordinator.runSession(ctx, executor, request)
	} else {
		err = coordinator.executor.Run(ctx, request)
	}
	coordinator.mu.Lock()
	delete(coordinator.running, request.TurnID)
	coordinator.mu.Unlock()

	command := internalCommand(request.ThreadID, request.TurnID)
	var state agothreadstore.MailboxState
	if errors.Is(err, context.Canceled) {
		command.Type = agoprotocol.CommandTurnSettleCancelled
		command.CommandID += ":settle-cancelled"
		command.IdempotencyKey += ":settle-cancelled"
		state, err = coordinator.store.SettleCancellation(context.Background(), command, request.TurnID)
	} else if err != nil {
		command.Type = agoprotocol.CommandTurnFail
		command.CommandID += ":fail"
		command.IdempotencyKey += ":fail"
		state, err = coordinator.store.FailTurn(context.Background(), command, request.TurnID, err.Error())
	} else {
		command.Type = agoprotocol.CommandTurnComplete
		command.CommandID += ":complete"
		command.IdempotencyKey += ":complete"
		state, err = coordinator.store.CompleteTurn(context.Background(), command, request.TurnID)
	}
	if err == nil {
		_ = coordinator.launchState(state)
	}
}

func (coordinator *Coordinator) runSession(ctx context.Context, executor SessionExecutor, request TurnRequest) error {
	coordinator.mu.Lock()
	_, attached := coordinator.sessions[request.ThreadID]
	if !attached {
		coordinator.sessions[request.ThreadID] = struct{}{}
	}
	coordinator.mu.Unlock()
	if !attached {
		coordinator.observeLifecycle(ctx, "session.start", map[string]any{"thread_id": request.ThreadID, "turn_id": request.TurnID})
	}
	execution, err := executor.Start(ctx, request)
	if err != nil {
		return err
	}
	cleaned := false
	defer func() {
		if !cleaned {
			_ = execution.CloseInput()
			_ = execution.Wait()
		}
	}()
	coordinator.mu.Lock()
	running := coordinator.running[request.TurnID]
	coordinator.mu.Unlock()
	if running == nil {
		_ = execution.CloseInput()
		_ = execution.Wait()
		cleaned = true
		return fmt.Errorf("executor turn %q detached before session start", request.TurnID)
	}
	running.controlMu.Lock()
	running.execution = execution
	running.controlMu.Unlock()
	if err := coordinator.deliverSteers(running); err != nil {
		_ = execution.CloseInput()
		_ = execution.Wait()
		cleaned = true
		return err
	}
	expectedIndex := uint64(1)
	started := false
	defer func() {
		if started {
			coordinator.observeLifecycle(context.Background(), "agent.end", map[string]any{"thread_id": request.ThreadID, "turn_id": request.TurnID})
		}
	}()
	settled := false
	for {
		select {
		case <-ctx.Done():
			abortContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = execution.Send(abortContext, ExecutionControl{Type: "abort"})
			_ = execution.CloseInput()
			_ = execution.Wait()
			cleaned = true
			return ctx.Err()
		case event, open := <-execution.Events():
			if !open {
				if err := execution.CloseInput(); err != nil {
					return fmt.Errorf("close executor input: %w", err)
				}
				waitErr := execution.Wait()
				cleaned = true
				if waitErr != nil {
					return waitErr
				}
				if !started || !settled {
					return fmt.Errorf("executor exited without a complete started/settled lifecycle")
				}
				return nil
			}
			if settled {
				return fmt.Errorf("executor emitted %q after settlement", event.Type)
			}
			if event.Index != expectedIndex {
				return fmt.Errorf("executor event index is %d, expected %d", event.Index, expectedIndex)
			}
			expectedIndex++
			if event.Type == "started" {
				if started {
					return fmt.Errorf("executor emitted duplicate started event")
				}
				started = true
			} else if !started {
				return fmt.Errorf("executor event %q arrived before started", event.Type)
			}
			if event.Type == "settled" {
				settled = true
			}
			if err := coordinator.appendExecutionEvent(ctx, request, event); err != nil {
				return err
			}
			if event.Type == "started" {
				coordinator.observeLifecycle(ctx, "agent.start", map[string]any{"thread_id": request.ThreadID, "turn_id": request.TurnID})
			}
			if event.Type == "tool_invocation" {
				if err := coordinator.executeTool(ctx, request, execution, event); err != nil {
					return err
				}
			}
			if settled {
				if err := execution.CloseInput(); err != nil {
					return fmt.Errorf("close settled executor input: %w", err)
				}
			}
		}
	}
}

func (coordinator *Coordinator) observeLifecycle(ctx context.Context, hook string, payload any) {
	if observer, ok := coordinator.tools.(LifecycleObserver); ok {
		observer.ObserveLifecycle(ctx, hook, payload)
	}
}

func (coordinator *Coordinator) executeTool(ctx context.Context, request TurnRequest, execution Execution, event ExecutionEvent) error {
	decoder := json.NewDecoder(bytes.NewReader(event.Payload))
	decoder.DisallowUnknownFields()
	var frame struct {
		Type   string         `json:"type"`
		CallID string         `json:"callId"`
		Name   string         `json:"name"`
		Input  map[string]any `json:"input"`
	}
	if err := decoder.Decode(&frame); err != nil || frame.Type != "tool_invocation" || frame.CallID == "" || frame.Name == "" || frame.Input == nil {
		return fmt.Errorf("invalid tool invocation event")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return fmt.Errorf("tool invocation event contains trailing data")
	}
	result := ToolResult{Output: "tool unavailable", Error: true}
	if coordinator.tools != nil {
		resolved, err := coordinator.tools.ExecuteTool(ctx, ToolCall{ThreadID: request.ThreadID, TurnID: request.TurnID, CallID: frame.CallID, Name: frame.Name, Input: frame.Input})
		if err != nil {
			result = ToolResult{Output: err.Error(), Error: true}
		} else {
			result = resolved
		}
	}
	call := ToolCall{ThreadID: request.ThreadID, TurnID: request.TurnID, CallID: frame.CallID, Name: frame.Name, Input: frame.Input}
	payload := mustJSON(map[string]any{"turn_id": request.TurnID, "call_id": frame.CallID, "name": frame.Name, "output": result.Output, "error": result.Error})
	eventType := agoprotocol.EventToolCompleted
	if result.Error {
		eventType = agoprotocol.EventToolFailed
	}
	key := fmt.Sprintf("coordinator:%s:tool-result:%s", request.TurnID, frame.CallID)
	if _, err := coordinator.store.AppendTurnEvents(ctx, agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: key, IdempotencyKey: key,
		ActorID: "ago-coordinator", Type: agoprotocol.CommandTurnEventAppend, ThreadID: request.ThreadID,
	}, request.TurnID, agothreadstore.EventDraft{Type: eventType, Visibility: agoprotocol.VisibilityUser, Payload: payload}); err != nil {
		return fmt.Errorf("persist confirmed tool result: %w", err)
	}
	delivered := result
	if observer, ok := coordinator.tools.(ToolResultObserver); ok {
		delivered = observer.ObserveToolResult(ctx, call, result)
	}
	preparedPayload := mustJSON(map[string]any{"turn_id": request.TurnID, "call_id": frame.CallID, "name": frame.Name, "output": delivered.Output, "error": delivered.Error})
	preparedKey := key + ":prepared"
	if _, err := coordinator.store.AppendTurnEvents(ctx, agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: preparedKey, IdempotencyKey: preparedKey,
		ActorID: "ago-coordinator", Type: agoprotocol.CommandTurnEventAppend, ThreadID: request.ThreadID,
	}, request.TurnID, agothreadstore.EventDraft{Type: agoprotocol.EventToolResultPrepared, Visibility: agoprotocol.VisibilityInternal, Payload: preparedPayload}); err != nil {
		return fmt.Errorf("persist prepared tool result: %w", err)
	}
	control := ExecutionControl{Type: "tool_result", Payload: mustJSON(map[string]any{"callId": frame.CallID, "name": frame.Name, "output": delivered.Output, "error": delivered.Error})}
	if err := execution.Send(ctx, control); err != nil {
		return fmt.Errorf("deliver confirmed tool result: %w", err)
	}
	return nil
}

func (coordinator *Coordinator) deliverSteers(running *runningTurn) error {
	running.controlMu.Lock()
	defer running.controlMu.Unlock()
	if running.execution == nil {
		return nil
	}
	for {
		state, err := coordinator.store.Mailbox(context.Background(), running.threadID)
		if err != nil {
			return err
		}
		var steer *agothreadstore.QueueItem
		for index := range state.Queue {
			if state.Queue[index].Class == agoprotocol.QueueSteer {
				steer = &state.Queue[index]
				break
			}
		}
		if steer == nil {
			return nil
		}
		command := internalCommand(running.threadID, state.ActiveTurnID)
		command.Type = agoprotocol.CommandSafePoint
		command.CommandID += ":steer:" + steer.QueueItemID
		command.IdempotencyKey += ":steer:" + steer.QueueItemID
		safe, err := coordinator.store.SafePoint(context.Background(), command, state.ActiveTurnID)
		if err != nil {
			return fmt.Errorf("accept steer at safe point: %w", err)
		}
		if len(safe.Events) != 1 || safe.Events[0].Type != agoprotocol.EventMessageAccepted {
			return fmt.Errorf("safe point did not durably accept one steer")
		}
		var payload struct {
			Content struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(safe.Events[0].Payload, &payload); err != nil {
			return fmt.Errorf("decode durable steer: %w", err)
		}
		sendCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err = running.execution.Send(sendCtx, ExecutionControl{Type: "steer", Payload: mustJSON(map[string]any{"text": payload.Content.Text})})
		cancel()
		if err != nil {
			return fmt.Errorf("deliver durable steer: %w", err)
		}
	}
}

func mustJSON(value any) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func (coordinator *Coordinator) appendExecutionEvent(ctx context.Context, request TurnRequest, event ExecutionEvent) error {
	eventType, visibility, err := durableExecutionEventType(event.Type)
	if err != nil {
		return err
	}
	if len(event.Payload) > 0 && !json.Valid(event.Payload) {
		return fmt.Errorf("executor event %d has invalid JSON payload", event.Index)
	}
	payload, err := json.Marshal(struct {
		TurnID             string          `json:"turn_id"`
		ExecutorEventIndex uint64          `json:"executor_event_index"`
		Event              json.RawMessage `json:"event,omitempty"`
	}{TurnID: request.TurnID, ExecutorEventIndex: event.Index, Event: event.Payload})
	if err != nil {
		return fmt.Errorf("encode executor event: %w", err)
	}
	key := fmt.Sprintf("coordinator:%s:attempt:%d:event:%d", request.TurnID, request.Attempt, event.Index)
	_, err = coordinator.store.AppendTurnEvents(ctx, agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion,
		CommandID:     key, IdempotencyKey: key, ActorID: "ago-coordinator",
		Type: agoprotocol.CommandTurnEventAppend, ThreadID: request.ThreadID,
	}, request.TurnID, agothreadstore.EventDraft{Type: eventType, Visibility: visibility, Payload: payload})
	if err != nil {
		return fmt.Errorf("persist executor event %d: %w", event.Index, err)
	}
	if event.Type == "assistant_completed" {
		usage, found, err := providerUsageFromAssistantEvent(request.ThreadID, key, event.Payload)
		if err != nil {
			return fmt.Errorf("decode provider usage for executor event %d: %w", event.Index, err)
		}
		if found {
			if _, err := coordinator.store.RecordProviderUsage(ctx, usage); err != nil {
				return fmt.Errorf("persist provider usage for executor event %d: %w", event.Index, err)
			}
		}
	}
	return nil
}

func providerUsageFromAssistantEvent(threadID, idempotencyKey string, raw json.RawMessage) (agothreadstore.ProviderUsageInput, bool, error) {
	var frame struct {
		Type          string          `json:"type"`
		Message       json.RawMessage `json:"message"`
		ProviderUsage struct {
			Provider  string                            `json:"provider"`
			Model     string                            `json:"model"`
			RequestID *string                           `json:"request_id"`
			Status    agothreadstore.UsageRecordStatus  `json:"status"`
			Usage     agothreadstore.ProviderTokenUsage `json:"usage"`
			Cost      agothreadstore.ProviderCost       `json:"cost"`
		} `json:"provider_usage"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&frame); err != nil {
		return agothreadstore.ProviderUsageInput{}, false, err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return agothreadstore.ProviderUsageInput{}, false, fmt.Errorf("assistant event must contain exactly one JSON value")
	}
	if frame.Type != "assistant_completed" || len(frame.Message) == 0 {
		return agothreadstore.ProviderUsageInput{}, false, fmt.Errorf("assistant_completed frame identity is required")
	}
	if frame.ProviderUsage.RequestID == nil {
		return agothreadstore.ProviderUsageInput{}, false, nil
	}
	return agothreadstore.ProviderUsageInput{
		ThreadID: threadID, IdempotencyKey: idempotencyKey,
		Provider: frame.ProviderUsage.Provider, Model: frame.ProviderUsage.Model, RequestID: *frame.ProviderUsage.RequestID,
		Status: frame.ProviderUsage.Status, Usage: frame.ProviderUsage.Usage, Cost: frame.ProviderUsage.Cost,
	}, true, nil
}

func durableExecutionEventType(sidecarType string) (agoprotocol.EventType, agoprotocol.Visibility, error) {
	switch sidecarType {
	case "started":
		return agoprotocol.EventAgentStarted, agoprotocol.VisibilityInternal, nil
	case "text":
		return agoprotocol.EventAssistantTextDelta, agoprotocol.VisibilityUser, nil
	case "assistant_completed":
		return agoprotocol.EventAssistantCompleted, agoprotocol.VisibilityUser, nil
	case "tool_invocation":
		return agoprotocol.EventToolRequested, agoprotocol.VisibilityUser, nil
	case "tool_finished":
		return agoprotocol.EventToolAcknowledged, agoprotocol.VisibilityInternal, nil
	case "stopped":
		return agoprotocol.EventAgentStopped, agoprotocol.VisibilityInternal, nil
	case "settled":
		return agoprotocol.EventAgentSettled, agoprotocol.VisibilityInternal, nil
	case "queue":
		return agoprotocol.EventAgentQueueUpdated, agoprotocol.VisibilityInternal, nil
	case "compact_start", "compact_end":
		return "", "", fmt.Errorf("executor-side compaction is not authoritative")
	default:
		return "", "", fmt.Errorf("unsupported executor event type %q", sidecarType)
	}
}

func (coordinator *Coordinator) cancel(turnID string) bool {
	coordinator.mu.Lock()
	turn, found := coordinator.running[turnID]
	coordinator.mu.Unlock()
	if found {
		turn.cancel()
	}
	return found
}

func turnRequest(state agothreadstore.MailboxState) (TurnRequest, bool, error) {
	if state.Activity != agoprotocol.ActivityRunning || state.ActiveTurnID == "" {
		return TurnRequest{}, false, nil
	}
	for _, event := range state.Events {
		if event.Type != agoprotocol.EventMessageAccepted {
			continue
		}
		var payload struct {
			TurnID  string          `json:"turn_id"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return TurnRequest{}, false, fmt.Errorf("decode accepted message: %w", err)
		}
		if payload.TurnID == state.ActiveTurnID {
			return TurnRequest{ThreadID: state.ThreadID, TurnID: state.ActiveTurnID, Content: payload.Content}, true, nil
		}
	}
	return TurnRequest{}, false, nil
}

func internalCommand(threadID, turnID string) agoprotocol.Command {
	return agoprotocol.Command{
		SchemaVersion:  agoprotocol.SchemaVersion,
		CommandID:      "coordinator:" + turnID,
		IdempotencyKey: "coordinator:" + turnID,
		ActorID:        "ago-coordinator",
		ThreadID:       threadID,
	}
}
