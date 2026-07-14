package configure

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureHooksInstallsRouteHintAndSupervisorGate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	initial := map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []any{map[string]any{
				"matcher": "", "hooks": []any{map[string]any{"type": "command", "command": "/bin/thread", "args": []any{"thread-hook"}}},
			}},
			// Legacy blocking stall-watch must be stripped on configure-hooks.
			"PostToolUse": []any{map[string]any{
				"matcher": "*", "hooks": []any{map[string]any{
					"type": "command", "command": "/bin/claudex-flow", "args": []any{"stall-watch"},
					"asyncRewake": true, "timeout": float64(360),
				}},
			}},
		},
	}
	raw, _ := json.MarshalIndent(initial, "", "  ")
	if err := os.WriteFile(path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	changed, err := EnsureHooks(path, "/bin/claudex-flow")
	if err != nil {
		t.Fatal(err)
	}
	if !changed["route-hint"] || !changed["supervisor-gate:PreToolUse"] || !changed["remove:stall-watch"] {
		t.Fatalf("expected installs + stall-watch removal, got %#v", changed)
	}

	// Second call is idempotent.
	changed2, err := EnsureHooks(path, "/bin/claudex-flow")
	if err != nil {
		t.Fatal(err)
	}
	if len(changed2) != 0 {
		t.Fatalf("expected no changes on second install, got %#v", changed2)
	}

	var got map[string]any
	body, _ := os.ReadFile(path)
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	hooks := got["hooks"].(map[string]any)
	ups := hooks["UserPromptSubmit"].([]any)
	if !hasHandler(ups, "/bin/claudex-flow", "route-hint") || !hasHandler(ups, "/bin/thread", "thread-hook") {
		t.Fatalf("UserPromptSubmit handlers missing: %#v", ups)
	}
	pre := hooks["PreToolUse"].([]any)
	if !hasHandler(pre, "/bin/claudex-flow", "supervisor-gate") {
		t.Fatalf("PreToolUse supervisor-gate missing: %#v", pre)
	}
	post := hooks["PostToolUse"].([]any)
	if !hasHandler(post, "/bin/claudex-flow", "supervisor-gate") {
		t.Fatalf("PostToolUse supervisor-gate missing: %#v", post)
	}
	if hasHandler(post, "/bin/claudex-flow", "stall-watch") {
		t.Fatalf("stall-watch must be removed from PostToolUse: %#v", post)
	}
}
