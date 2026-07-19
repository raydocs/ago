package agopluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"claudexflow/internal/agopluginprotocol"
)

type PolicyAction string

const (
	PolicyAbstain    PolicyAction = "abstain"
	PolicyAllow      PolicyAction = "allow"
	PolicyDeny       PolicyAction = "deny"
	PolicyModify     PolicyAction = "modify"
	PolicySynthesize PolicyAction = "synthesize"
	PolicyError      PolicyAction = "error"
)

type ToolResult struct {
	Status string          `json:"status"`
	Output json.RawMessage `json:"output,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type ToolPolicyDecision struct {
	Action  PolicyAction   `json:"action"`
	Message string         `json:"message,omitempty"`
	Input   map[string]any `json:"input,omitempty"`
	Result  *ToolResult    `json:"result,omitempty"`
}

type ToolCallEvent struct {
	ThreadID   string         `json:"threadId,omitempty"`
	TurnID     string         `json:"turnId,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	Tool       string         `json:"tool"`
	Input      map[string]any `json:"input"`
}

type PolicyOutcome struct {
	Action  PolicyAction
	Message string
	Input   map[string]any
	Result  *ToolResult
}

func FoldToolPolicy(initial map[string]any, decisions []ToolPolicyDecision, validate func(map[string]any) error) (PolicyOutcome, error) {
	current := cloneInput(initial)
	if validate == nil {
		return PolicyOutcome{}, fmt.Errorf("tool input validator is required")
	}
	for _, decision := range decisions {
		switch decision.Action {
		case PolicyAbstain, PolicyAllow:
			continue
		case PolicyModify:
			if decision.Input == nil {
				return PolicyOutcome{}, fmt.Errorf("plugin returned modify without input")
			}
			current = cloneInput(decision.Input)
			if err := validate(current); err != nil {
				return PolicyOutcome{}, fmt.Errorf("plugin-modified tool input is invalid: %w", err)
			}
		case PolicyDeny:
			if decision.Message == "" {
				return PolicyOutcome{}, fmt.Errorf("plugin returned deny without a message")
			}
			return PolicyOutcome{Action: PolicyDeny, Message: decision.Message, Input: current}, nil
		case PolicySynthesize:
			if decision.Result == nil || !validToolResult(*decision.Result) {
				return PolicyOutcome{}, fmt.Errorf("plugin returned malformed synthesized result")
			}
			return PolicyOutcome{Action: PolicySynthesize, Input: current, Result: decision.Result}, nil
		case PolicyError:
			return PolicyOutcome{}, fmt.Errorf("tool policy plugin failed: %s", decision.Message)
		default:
			return PolicyOutcome{}, fmt.Errorf("plugin returned unknown policy action %q", decision.Action)
		}
	}
	if err := validate(current); err != nil {
		return PolicyOutcome{}, fmt.Errorf("final tool input is invalid: %w", err)
	}
	return PolicyOutcome{Action: PolicyAllow, Input: current}, nil
}

var policyInvocationSequence atomic.Uint64

func (manager *Manager) EvaluateToolCall(ctx context.Context, event ToolCallEvent, validate func(map[string]any) error) (PolicyOutcome, error) {
	manager.mu.RLock()
	current := manager.current
	manager.mu.RUnlock()
	invoker, ok := current.runtime.(interface {
		Invoke(context.Context, string, agopluginprotocol.InvocationParams) (json.RawMessage, error)
	})
	if !ok || current.snapshot.Generation == 0 {
		return PolicyOutcome{}, fmt.Errorf("plugin runtime is unavailable")
	}
	deadline, hasDeadline := ctx.Deadline()
	if !hasDeadline {
		deadline = time.Now().Add(120 * time.Second)
	}
	payload, err := json.Marshal(struct {
		Hook    string        `json:"hook"`
		Payload ToolCallEvent `json:"payload"`
	}{Hook: "tool.call", Payload: event})
	if err != nil {
		return PolicyOutcome{}, err
	}
	invocationID := "policy-" + strconv.FormatUint(policyInvocationSequence.Add(1), 10)
	untrack := manager.trackInvocation(invocationID, InvocationContext{ThreadID: event.ThreadID, TurnID: event.TurnID})
	defer untrack()
	raw, err := invoker.Invoke(ctx, agopluginprotocol.MethodHookInvoke, agopluginprotocol.InvocationParams{
		Generation:     current.snapshot.Generation,
		InvocationID:   invocationID,
		ThreadID:       event.ThreadID,
		TurnID:         event.TurnID,
		DeadlineUnixMs: deadline.UnixMilli(),
		Payload:        payload,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			manager.mu.RLock()
			config := cloneReloadConfig(manager.config)
			manager.mu.RUnlock()
			restartCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, restartErr := manager.Reload(restartCtx, config, "deadline")
			cancel()
			if restartErr != nil {
				return PolicyOutcome{}, fmt.Errorf("invoke tool policy: %w; restart plugin generation: %v", err, restartErr)
			}
		}
		return PolicyOutcome{}, fmt.Errorf("invoke tool policy: %w", err)
	}
	var decisions []ToolPolicyDecision
	if err := json.Unmarshal(raw, &decisions); err != nil {
		return PolicyOutcome{}, fmt.Errorf("decode tool policy decisions: %w", err)
	}
	if len(decisions) == 0 {
		return PolicyOutcome{}, fmt.Errorf("tool policy chain returned no decisions")
	}
	return FoldToolPolicy(event.Input, decisions, validate)
}

func validToolResult(result ToolResult) bool {
	switch result.Status {
	case "done":
		return result.Error == ""
	case "error":
		return result.Error != ""
	case "cancelled":
		return true
	default:
		return false
	}
}

func FoldToolResults(initial ToolResult, replacements []json.RawMessage, logInvalid func(error)) ToolResult {
	current := initial
	for _, raw := range replacements {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var replacement ToolResult
		if err := json.Unmarshal(raw, &replacement); err != nil || !validToolResult(replacement) {
			if logInvalid != nil {
				if err != nil {
					logInvalid(fmt.Errorf("decode tool result replacement: %w", err))
				} else {
					logInvalid(fmt.Errorf("invalid tool result replacement status %q", replacement.Status))
				}
			}
			continue
		}
		current = replacement
	}
	return current
}

func cloneInput(input map[string]any) map[string]any {
	if input == nil {
		return map[string]any{}
	}
	encoded, _ := json.Marshal(input)
	var clone map[string]any
	_ = json.Unmarshal(encoded, &clone)
	return clone
}
