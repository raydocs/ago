package configure

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// EnsureHooks idempotently installs root-only zero-model control hooks:
// route-hint on UserPromptSubmit and supervisor-gate on Pre/Post tool + compact.
// It also removes blocking stall-watch PostToolUse handlers (self-deadlock with
// Claude Code's hook lifecycle; stall-watch now no-ops and is not reinstalled).
func EnsureHooks(settingsPath, command string) (map[string]bool, error) {
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return nil, err
	}
	var settings map[string]any
	if err := json.Unmarshal(raw, &settings); err != nil {
		return nil, fmt.Errorf("decode settings: %w", err)
	}
	hooks, ok := settings["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		settings["hooks"] = hooks
	}

	changed := map[string]bool{}
	if removeHandlers(hooks, command, "stall-watch") {
		changed["remove:stall-watch"] = true
	}
	// route-hint first on UserPromptSubmit so routing context is available early.
	if ensureHandler(hooks, "UserPromptSubmit", "", command, []any{"route-hint"}, map[string]any{
		"type": "command", "command": command, "args": []any{"route-hint"}, "timeout": float64(2),
	}, true) {
		changed["route-hint"] = true
	}

	gateSpecs := []struct {
		event   string
		matcher string
		timeout float64
		front   bool
	}{
		{event: "PreToolUse", matcher: "*", timeout: 5, front: true},
		{event: "PostToolUse", matcher: "*", timeout: 5, front: false},
		{event: "PostToolUseFailure", matcher: "*", timeout: 5, front: false},
		{event: "PostCompact", matcher: "", timeout: 5, front: true},
		{event: "SessionStart", matcher: "", timeout: 5, front: false},
		// UserPromptSubmit: /gate-override leases (user-only; never MCP self-auth).
		{event: "UserPromptSubmit", matcher: "", timeout: 5, front: false},
		// T3: StopFailure overflow latch (stdout ignored by CC; side-effect state only).
		{event: "StopFailure", matcher: "*", timeout: 5, front: false},
	}
	for _, spec := range gateSpecs {
		handler := map[string]any{
			"type": "command", "command": command, "args": []any{"supervisor-gate"}, "timeout": spec.timeout,
		}
		if ensureHandler(hooks, spec.event, spec.matcher, command, []any{"supervisor-gate"}, handler, spec.front) {
			changed["supervisor-gate:"+spec.event] = true
		}
	}

	if len(changed) == 0 {
		return changed, nil
	}
	output, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, err
	}
	output = append(output, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(settingsPath), ".settings-hooks-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(output); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if err := os.Rename(tmpPath, settingsPath); err != nil {
		return nil, err
	}
	return changed, nil
}

// EnsureRouteHint remains for callers that only need the prompt router.
func EnsureRouteHint(settingsPath, command string) (bool, error) {
	changed, err := EnsureHooks(settingsPath, command)
	if err != nil {
		return false, err
	}
	return len(changed) > 0, nil
}

func ensureHandler(hooks map[string]any, event, matcher, command string, args []any, handler map[string]any, front bool) bool {
	groups, _ := hooks[event].([]any)
	if hasHandler(groups, command, fmt.Sprint(args[0])) {
		return false
	}
	if len(groups) == 0 {
		hooks[event] = []any{map[string]any{"matcher": matcher, "hooks": []any{handler}}}
		return true
	}
	// Prefer the first matching matcher group; otherwise append a new group.
	for i, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			continue
		}
		if fmt.Sprint(group["matcher"]) != matcher && !(matcher == "" && group["matcher"] == nil) {
			// allow empty matcher match when stored as missing
			if matcher != "" || fmt.Sprint(group["matcher"]) != "" {
				continue
			}
		}
		entries, _ := group["hooks"].([]any)
		if front {
			group["hooks"] = append([]any{handler}, entries...)
		} else {
			group["hooks"] = append(entries, handler)
		}
		groups[i] = group
		hooks[event] = groups
		return true
	}
	hooks[event] = append(groups, map[string]any{"matcher": matcher, "hooks": []any{handler}})
	return true
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

// removeHandlers strips every hook entry matching command+arg across all events.
func removeHandlers(hooks map[string]any, command, arg string) bool {
	removed := false
	for event, rawGroups := range hooks {
		groups, ok := rawGroups.([]any)
		if !ok {
			continue
		}
		nextGroups := make([]any, 0, len(groups))
		for _, rawGroup := range groups {
			group, ok := rawGroup.(map[string]any)
			if !ok {
				nextGroups = append(nextGroups, rawGroup)
				continue
			}
			entries, _ := group["hooks"].([]any)
			kept := make([]any, 0, len(entries))
			for _, rawEntry := range entries {
				entry, ok := rawEntry.(map[string]any)
				if !ok {
					kept = append(kept, rawEntry)
					continue
				}
				if fmt.Sprint(entry["command"]) == command {
					args, _ := entry["args"].([]any)
					if len(args) == 1 && fmt.Sprint(args[0]) == arg {
						removed = true
						continue
					}
				}
				kept = append(kept, rawEntry)
			}
			if len(kept) == 0 {
				// Drop empty matcher groups.
				removed = true
				continue
			}
			group["hooks"] = kept
			nextGroups = append(nextGroups, group)
		}
		hooks[event] = nextGroups
	}
	return removed
}
