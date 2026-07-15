package routehint

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"claudexflow/internal/fastpath"
	"claudexflow/internal/router"
)

const maxHookInputBytes = 128 * 1024

type event struct {
	HookEventName string `json:"hook_event_name"`
	Prompt        string `json:"prompt"`
}

type hookOutput struct {
	HookSpecificOutput specificOutput `json:"hookSpecificOutput"`
}

type specificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext"`
}

// Build emits a zero-model routing reminder only for prompts with one concrete
// specialist gap. Ordinary implementation prompts stay silent so the hook does
// not add permanent workflow prose to every turn.
func Build(reader io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, maxHookInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxHookInputBytes {
		return nil, fmt.Errorf("route hint hook input exceeds %d bytes", maxHookInputBytes)
	}
	var in event
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("decode route hint hook input: %w", err)
	}
	if in.HookEventName != "UserPromptSubmit" || strings.TrimSpace(in.Prompt) == "" {
		return nil, nil
	}
	if contract, ok := fastpath.Parse(in.Prompt); ok {
		return json.Marshal(hookOutput{HookSpecificOutput: specificOutput{
			HookEventName: "UserPromptSubmit", AdditionalContext: fastpath.Context(contract),
		}})
	}
	plan, err := router.PlanRoute(router.RouteRequest{Objective: in.Prompt})
	if err != nil || plan.Action != router.ActionCapability || plan.SelectedLane.Tool == "" {
		return nil, nil
	}

	hint := capabilityHint(plan.SelectedLane.Tool, plan.Reason)
	return json.Marshal(hookOutput{HookSpecificOutput: specificOutput{
		HookEventName: "UserPromptSubmit", AdditionalContext: hint,
	}})
}

func capabilityHint(tool, reason string) string {
	return fmt.Sprintf("CLAUDEX_ROUTE_HINT v1 (zero-model, current prompt only): detected a concrete %s gap. Before duplicating that work in the Sol Supervisor, call mcp__claudex-flow__%s once with the narrowest bounded question, then keep synthesis and implementation in the Supervisor. Runtime admission and lane health remain authoritative. If the required evidence is already present, ignore this hint and continue direct. Do not create a Worker merely because this hint exists. Basis: %s", tool, tool, reason)
}
