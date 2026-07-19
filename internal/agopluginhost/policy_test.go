package agopluginhost

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"claudexflow/internal/agopluginprotocol"
)

func TestFoldToolPolicyAppliesModificationsAndTreatsAllowAsAdvisory(t *testing.T) {
	input := map[string]any{"path": "before", "count": float64(1)}
	validated := []map[string]any{}
	decision, err := FoldToolPolicy(input, []ToolPolicyDecision{
		{Action: PolicyAllow},
		{Action: PolicyModify, Input: map[string]any{"path": "after", "count": float64(2)}},
		{Action: PolicyAbstain},
		{Action: PolicyAllow},
	}, func(candidate map[string]any) error {
		validated = append(validated, cloneInput(candidate))
		return nil
	})
	if err != nil {
		t.Fatalf("FoldToolPolicy() error = %v", err)
	}
	if decision.Action != PolicyAllow || !reflect.DeepEqual(decision.Input, map[string]any{"path": "after", "count": float64(2)}) {
		t.Fatalf("decision = %#v", decision)
	}
	if len(validated) != 2 || validated[0]["path"] != "after" || validated[1]["path"] != "after" {
		t.Fatalf("validated inputs = %#v", validated)
	}
}

func TestFoldToolPolicyTerminalAndFailureSemantics(t *testing.T) {
	result := ToolResult{Status: "done", Output: json.RawMessage(`{"cached":true}`)}
	for _, test := range []struct {
		name      string
		decisions []ToolPolicyDecision
		want      PolicyAction
		wantErr   bool
	}{
		{name: "deny", decisions: []ToolPolicyDecision{{Action: PolicyDeny, Message: "blocked"}, {Action: PolicyAllow}}, want: PolicyDeny},
		{name: "synthesize", decisions: []ToolPolicyDecision{{Action: PolicySynthesize, Result: &result}, {Action: PolicyDeny}}, want: PolicySynthesize},
		{name: "plugin error", decisions: []ToolPolicyDecision{{Action: PolicyError, Message: "broken"}}, wantErr: true},
		{name: "unknown", decisions: []ToolPolicyDecision{{Action: PolicyAction("future")}}, wantErr: true},
		{name: "malformed deny", decisions: []ToolPolicyDecision{{Action: PolicyDeny}}, wantErr: true},
		{name: "malformed synthesize", decisions: []ToolPolicyDecision{{Action: PolicySynthesize}}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			decision, err := FoldToolPolicy(map[string]any{"safe": true}, test.decisions, func(map[string]any) error { return nil })
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, wantErr %v", err, test.wantErr)
			}
			if !test.wantErr && decision.Action != test.want {
				t.Fatalf("action = %q, want %q", decision.Action, test.want)
			}
		})
	}
}

func TestFoldToolPolicyFailsClosedOnInvalidModifiedInput(t *testing.T) {
	_, err := FoldToolPolicy(map[string]any{"safe": true}, []ToolPolicyDecision{{Action: PolicyModify, Input: map[string]any{"safe": false}}}, func(map[string]any) error {
		return errors.New("schema rejected input")
	})
	if err == nil {
		t.Fatal("invalid modified input was allowed")
	}
}

func TestFoldToolResultsIgnoresMalformedReplacementAndKeepsLastValidResult(t *testing.T) {
	initial := ToolResult{Status: "done", Output: json.RawMessage(`"original"`)}
	logged := 0
	result := FoldToolResults(initial, []json.RawMessage{
		json.RawMessage(`null`),
		json.RawMessage(`{"status":"done","output":"replaced"}`),
		json.RawMessage(`{"status":"error"}`),
		json.RawMessage(`{"status":"future"}`),
	}, func(error) { logged++ })
	if result.Status != "done" || string(result.Output) != `"replaced"` {
		t.Fatalf("result = %#v", result)
	}
	if logged != 2 {
		t.Fatalf("logged malformed replacements = %d, want 2", logged)
	}
}

func TestManagerEvaluatesRealDefaultPermissionPlugin(t *testing.T) {
	manager, cleanup := realPluginManager(t)
	defer cleanup()
	decision, err := manager.EvaluateToolCall(context.Background(), ToolCallEvent{ThreadID: "thread-1", TurnID: "turn-1", Tool: "shell", Input: map[string]any{"command": "pwd"}}, func(map[string]any) error { return nil })
	if err != nil {
		t.Fatalf("EvaluateToolCall() error = %v", err)
	}
	if decision.Action != PolicyAllow || decision.Input["command"] != "pwd" {
		t.Fatalf("default decision = %#v", decision)
	}
}

func TestManagerFailsClosedOnMalformedRealBunPreToolDecision(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	runtimeScript, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "main.ts"))
	pluginScript, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "test-plugin.ts"))
	manager := NewManager(NewProcessFactory(bun, runtimeScript, ProcessOptions{MaxMessageBytes: 1 << 20, ExitGrace: time.Second}), time.Second)
	_, err = manager.Reload(context.Background(), ReloadConfig{
		Plugins:      []agopluginprotocol.PluginConfig{{PluginID: "test.plugin", EntryURI: "file://" + pluginScript, Config: json.RawMessage(`{"malformedPolicy":true}`)}},
		Capabilities: agopluginprotocol.Capabilities{RenderMode: "headless"},
		Limits:       agopluginprotocol.Limits{MaxMessageBytes: 1 << 20, MaxInflight: 8},
	}, "malformed-policy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	validated := false
	if _, err := manager.EvaluateToolCall(context.Background(), ToolCallEvent{ThreadID: "thread-1", TurnID: "turn-1", Tool: "echo", Input: map[string]any{"text": "must-not-run"}}, func(map[string]any) error {
		validated = true
		return nil
	}); err == nil {
		t.Fatal("malformed real Bun pre-tool decision was allowed")
	}
	if validated {
		t.Fatal("tool validation/dispatch boundary was reached after malformed pre-tool decision")
	}
}

func TestPolicyDeadlineRestartsChildThatIgnoresCancellation(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	script, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "main.ts"))
	plugin, _ := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "hanging-plugin.ts"))
	manager := NewManager(NewProcessFactory(bun, script, ProcessOptions{MaxMessageBytes: 1 << 20, ExitGrace: 100 * time.Millisecond}), 100*time.Millisecond)
	_, err = manager.Reload(context.Background(), ReloadConfig{
		Plugins:      []agopluginprotocol.PluginConfig{{PluginID: "hanging.plugin", EntryURI: "file://" + plugin, Config: json.RawMessage(`{}`)}},
		Capabilities: agopluginprotocol.Capabilities{RenderMode: "headless"},
		Limits:       agopluginprotocol.Limits{MaxMessageBytes: 1 << 20, MaxInflight: 8},
	}, "startup")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = manager.EvaluateToolCall(ctx, ToolCallEvent{ThreadID: "thread-1", TurnID: "turn-1", Tool: "shell", Input: map[string]any{}}, func(map[string]any) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("policy timeout error = %v", err)
	}
	if manager.Current().Generation != 2 {
		t.Fatalf("generation after uncooperative timeout = %d, want restarted generation 2", manager.Current().Generation)
	}
}

func realPluginManager(t *testing.T) (*Manager, func()) {
	t.Helper()
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "plugin-runtime", "main.ts"))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(NewProcessFactory(bun, script, ProcessOptions{MaxMessageBytes: 1 << 20, ExitGrace: time.Second}), time.Second)
	_, err = manager.Reload(context.Background(), ReloadConfig{
		Capabilities: agopluginprotocol.Capabilities{RenderMode: "headless"},
		Limits:       agopluginprotocol.Limits{MaxMessageBytes: 1 << 20, MaxInflight: 8},
	}, "startup")
	if err != nil {
		t.Fatalf("start real plugin manager: %v", err)
	}
	return manager, func() { _ = manager.Shutdown(context.Background()) }
}
