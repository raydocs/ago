package configure

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureRouteHintPreservesSettingsAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := map[string]any{
		"env": map[string]any{"ANTHROPIC_AUTH_TOKEN": "secret", "MCP_TOOL_TIMEOUT": "120000"},
		"hooks": map[string]any{"UserPromptSubmit": []any{map[string]any{
			"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": "/bin/thread", "args": []any{"thread-hook"}}},
		}}},
	}
	raw, _ := json.Marshal(original)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	changed, err := EnsureRouteHint(path, "/bin/claudex-flow")
	if err != nil || !changed {
		t.Fatalf("first install = %t, %v", changed, err)
	}
	changed, err = EnsureRouteHint(path, "/bin/claudex-flow")
	if err != nil || changed {
		t.Fatalf("second install = %t, %v", changed, err)
	}
	var got map[string]any
	data, _ := os.ReadFile(path)
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["env"].(map[string]any)["ANTHROPIC_AUTH_TOKEN"] != "secret" {
		t.Fatal("unrelated secret setting changed")
	}
	groups := got["hooks"].(map[string]any)["UserPromptSubmit"].([]any)
	if !hasHandler(groups, "/bin/claudex-flow", "route-hint") || !hasHandler(groups, "/bin/thread", "thread-hook") {
		t.Fatalf("handlers missing: %#v", groups)
	}
}
