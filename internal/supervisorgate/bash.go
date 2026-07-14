package supervisorgate

import (
	"encoding/json"
	"strings"
)

// bashReadOnly allows a narrow whitelist of read-only shell forms during handoff.
// Fail closed: unknown shape → not read-only.
func bashReadOnly(raw json.RawMessage) bool {
	cmd := bashCommand(raw)
	if cmd == "" {
		return false
	}
	lower := strings.ToLower(cmd)
	for _, bad := range []string{
		">", ">>", "<<", "$(", "`", "&&", "||", ";", "|",
		"\n", "\r", "tee ", " wrangler", "npm ", "npx ", "pnpm ", "yarn ",
		"git commit", "git push", "git reset", "rm ", "mv ", "cp ", "chmod ",
		"curl ", "wget ", "python ", "python3 ", "node ", "go run", "go test",
		"make ", "deploy", "dd ", "truncate",
	} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "ls", "pwd", "true":
		return true
	case "cat", "head", "tail", "wc", "file", "stat", "rg", "grep":
		return len(fields) >= 2
	default:
		return false
	}
}

// bashIsDestructiveGit detects hard-denied git operations on normal Roots.
func bashIsDestructiveGit(raw json.RawMessage) bool {
	cmd := strings.ToLower(bashCommand(raw))
	if cmd == "" {
		return false
	}
	// Conservative substring match: these must never be automated thrash.
	if strings.Contains(cmd, "git reset --hard") || strings.Contains(cmd, "git reset -hard") {
		return true
	}
	if strings.Contains(cmd, "git push --force") || strings.Contains(cmd, "git push -f") ||
		strings.Contains(cmd, "git push --force-with-lease") {
		return true
	}
	return false
}

// bashClass classifies Bash for high-cost / budget counting (T11 partial).
// Returns: readonly | test | deploy | mutate | other
func bashClass(raw json.RawMessage) string {
	cmd := strings.ToLower(bashCommand(raw))
	if cmd == "" {
		return "other"
	}
	if bashReadOnly(raw) {
		return "readonly"
	}
	if strings.Contains(cmd, "wrangler deploy") || strings.Contains(cmd, "npm run deploy") ||
		strings.Contains(cmd, "cloudflare deploy") || strings.Contains(cmd, " production deploy") {
		return "deploy"
	}
	if strings.Contains(cmd, "go test") || strings.Contains(cmd, "npm test") ||
		strings.Contains(cmd, "npm run test") || strings.Contains(cmd, "npx vitest") ||
		strings.Contains(cmd, "pytest") {
		return "test"
	}
	if strings.Contains(cmd, ">") || strings.Contains(cmd, ">>") || strings.Contains(cmd, "git commit") ||
		strings.Contains(cmd, "rm ") || strings.Contains(cmd, "mv ") {
		return "mutate"
	}
	return "other"
}

func bashCommand(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, key := range []string{"command", "cmd"} {
			if v, ok := obj[key].(string); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func toolPathFromInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for _, key := range []string{"file_path", "path", "filePath"} {
		if v, ok := obj[key].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
