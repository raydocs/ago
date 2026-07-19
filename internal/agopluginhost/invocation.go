package agopluginhost

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"claudexflow/internal/agopluginprotocol"
)

const defaultInvocationTimeout = 120 * time.Second

var hostInvocationSequence atomic.Uint64

// ExecuteToolFor executes the exactly registered tool name in the current
// generation with Ago-authored thread/turn correlation.
func (manager *Manager) ExecuteToolFor(ctx context.Context, name string, input map[string]any, correlation InvocationContext) (json.RawMessage, error) {
	current := manager.activeSnapshot()
	owner := ""
	for _, plugin := range current.snapshot.Registrations {
		for _, tool := range plugin.Tools {
			if tool.Name == name {
				owner = plugin.PluginID
				break
			}
		}
	}
	if owner == "" {
		return nil, fmt.Errorf("plugin tool %q is not registered", name)
	}
	payload, err := json.Marshal(struct {
		Name     string         `json:"name"`
		PluginID string         `json:"pluginId"`
		Input    map[string]any `json:"input"`
	}{name, owner, input})
	if err != nil {
		return nil, fmt.Errorf("encode plugin tool %q input: %w", name, err)
	}
	raw, err := manager.invokeCurrent(ctx, current, agopluginprotocol.MethodToolExecute, "tool", payload, correlation)
	if err != nil {
		return nil, fmt.Errorf("execute plugin tool %q: %w", name, err)
	}
	if len(raw) == 0 || !json.Valid(raw) {
		return nil, fmt.Errorf("execute plugin tool %q: malformed result", name)
	}
	return raw, nil
}

func (manager *Manager) ExecuteCommandFor(ctx context.Context, canonicalID string, input any, correlation InvocationContext) (json.RawMessage, error) {
	current := manager.activeSnapshot()
	owner, found := commandOwner(current.snapshot, canonicalID)
	if !found {
		return nil, fmt.Errorf("plugin command %q is not registered", canonicalID)
	}
	payload, _ := json.Marshal(struct {
		CommandID string `json:"commandId"`
		PluginID  string `json:"pluginId"`
		Input     any    `json:"input,omitempty"`
	}{canonicalID, owner, input})
	return manager.invokeCurrent(ctx, current, agopluginprotocol.MethodCommandExecute, "command", payload, correlation)
}

// ObserveToolResult invokes registered tool.result observers in registration
// order. Invocation, decoding, and observer failures are fail-open: the last
// valid replacement wins and the confirmed result is otherwise preserved.
func (manager *Manager) ObserveToolResult(ctx context.Context, payload any, confirmed ToolResult, logFailure func(error)) ToolResult {
	raw, ok := manager.observe(ctx, "tool.result", payload, "result", logFailure)
	if !ok {
		return confirmed
	}
	var replacements []json.RawMessage
	if err := json.Unmarshal(raw, &replacements); err != nil {
		logObserverFailure(logFailure, fmt.Errorf("decode tool.result observers: %w", err))
		return confirmed
	}
	return FoldToolResults(confirmed, replacements, logFailure)
}

// ObserveLifecycle invokes observers for a registered lifecycle hook. It is
// deliberately fail-open; failures are reported only through logFailure.
func (manager *Manager) ObserveLifecycle(ctx context.Context, hook string, payload any, logFailure func(error)) {
	manager.observe(ctx, hook, payload, "lifecycle", logFailure)
}

func (manager *Manager) observe(ctx context.Context, hook string, value any, kind string, logFailure func(error)) (json.RawMessage, bool) {
	current := manager.activeSnapshot()
	if !snapshotHasHook(current.snapshot, hook) {
		return nil, false
	}
	payload, err := json.Marshal(struct {
		Hook    string `json:"hook"`
		Payload any    `json:"payload"`
	}{hook, value})
	if err != nil {
		logObserverFailure(logFailure, fmt.Errorf("encode %s observer payload: %w", hook, err))
		return nil, false
	}
	correlation := invocationContext(value)
	raw, err := manager.invokeCurrent(ctx, current, agopluginprotocol.MethodHookInvoke, kind, payload, correlation)
	if err != nil {
		logObserverFailure(logFailure, fmt.Errorf("observe %s: %w", hook, err))
		return nil, false
	}
	return raw, true
}

func (manager *Manager) activeSnapshot() activeGeneration {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.current
}

func (manager *Manager) invokeCurrent(ctx context.Context, current activeGeneration, method, prefix string, payload json.RawMessage, correlation InvocationContext) (json.RawMessage, error) {
	if current.runtime == nil || current.snapshot.Generation == 0 {
		return nil, fmt.Errorf("plugin runtime is unavailable")
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultInvocationTimeout)
		defer cancel()
		deadline, _ = ctx.Deadline()
	}
	invocationID := fmt.Sprintf("%s-%d", prefix, hostInvocationSequence.Add(1))
	untrack := manager.trackInvocation(invocationID, correlation)
	defer untrack()
	return current.runtime.Invoke(ctx, method, agopluginprotocol.InvocationParams{
		Generation: current.snapshot.Generation, InvocationID: invocationID,
		ThreadID: correlation.ThreadID, TurnID: correlation.TurnID,
		DeadlineUnixMs: deadline.UnixMilli(), Payload: payload,
	})
}

func invocationContext(value any) InvocationContext {
	encoded, err := json.Marshal(value)
	if err != nil {
		return InvocationContext{}
	}
	var fields struct {
		ThreadIDSnake string `json:"thread_id"`
		TurnIDSnake   string `json:"turn_id"`
		ThreadIDCamel string `json:"threadId"`
		TurnIDCamel   string `json:"turnId"`
	}
	if json.Unmarshal(encoded, &fields) != nil {
		return InvocationContext{}
	}
	if fields.ThreadIDSnake != "" {
		return InvocationContext{ThreadID: fields.ThreadIDSnake, TurnID: fields.TurnIDSnake}
	}
	return InvocationContext{ThreadID: fields.ThreadIDCamel, TurnID: fields.TurnIDCamel}
}

func commandOwner(snapshot Snapshot, canonical string) (string, bool) {
	if strings.Count(canonical, ":") < 1 {
		return "", false
	}
	for _, plugin := range snapshot.Registrations {
		for _, command := range plugin.Commands {
			if plugin.PluginID+":"+command.ID == canonical {
				return plugin.PluginID, true
			}
		}
	}
	return "", false
}

func snapshotHasHook(snapshot Snapshot, hook string) bool {
	for _, plugin := range snapshot.Registrations {
		for _, registered := range plugin.Hooks {
			if registered == hook {
				return true
			}
		}
	}
	return false
}

func logObserverFailure(logFailure func(error), err error) {
	if logFailure != nil {
		logFailure(err)
	}
}
