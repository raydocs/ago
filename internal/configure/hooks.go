package configure

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureRouteHint adds the root-only zero-model prompt router without
// rebuilding the user's settings file or exposing its credential values.
func EnsureRouteHint(settingsPath, command string) (bool, error) {
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return false, err
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return false, fmt.Errorf("decode settings: %w", err)
	}
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}
	groups, _ := hooks["UserPromptSubmit"].([]any)
	if hasHandler(groups, command, "route-hint") {
		return false, nil
	}
	handler := map[string]any{
		"type": "command", "command": command, "args": []any{"route-hint"}, "timeout": float64(2),
	}
	if len(groups) == 0 {
		groups = []any{map[string]any{"matcher": "", "hooks": []any{handler}}}
	} else if group, ok := groups[0].(map[string]any); ok {
		entries, _ := group["hooks"].([]any)
		group["hooks"] = append([]any{handler}, entries...)
		groups[0] = group
	} else {
		groups = append([]any{map[string]any{"matcher": "", "hooks": []any{handler}}}, groups...)
	}
	hooks["UserPromptSubmit"] = groups
	output, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return false, err
	}
	output = append(output, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(settingsPath), ".settings-route-hint-*")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := tmp.Write(output); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpPath, settingsPath); err != nil {
		return false, err
	}
	return true, nil
}

func hasHandler(groups []any, command, arg string) bool {
	for _, rawGroup := range groups {
		group, _ := rawGroup.(map[string]any)
		entries, _ := group["hooks"].([]any)
		for _, rawEntry := range entries {
			entry, _ := rawEntry.(map[string]any)
			if fmt.Sprint(entry["command"]) != command {
				continue
			}
			args, _ := entry["args"].([]any)
			if len(args) == 1 && fmt.Sprint(args[0]) == arg {
				return true
			}
		}
	}
	return false
}
