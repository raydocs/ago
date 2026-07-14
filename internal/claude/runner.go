package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Request struct {
	ClaudeBinary    string
	SettingsPath    string
	AuthMode        AuthMode
	WorkDir         string
	Prompt          string
	Model           string
	Effort          string
	Role            string
	RootSessionID   string
	ParentSessionID string
	ResumeSession   string
	JSONSchema      string
	Tools           []string
	MaxTurns        int
	Timeout         time.Duration
}

type AuthMode string

const (
	AuthGateway            AuthMode = "gateway"
	AuthNativeSubscription AuthMode = "native_subscription"
)

type Result struct {
	Model          string          `json:"model"`
	ResolvedModel  string          `json:"resolved_model,omitempty"`
	AuthSource     AuthMode        `json:"auth_source"`
	Effort         string          `json:"effort,omitempty"`
	SessionID      string          `json:"session_id,omitempty"`
	Subtype        string          `json:"subtype,omitempty"`
	IsError        bool            `json:"is_error,omitempty"`
	TerminalReason string          `json:"terminal_reason,omitempty"`
	Text           string          `json:"text"`
	Structured     json.RawMessage `json:"structured_output,omitempty"`
	ToolUses       map[string]int  `json:"tool_uses"`
	ChangedPaths   []string        `json:"changed_paths,omitempty"`
	Usage          Usage           `json:"usage"`
	DurationMS     int64           `json:"duration_ms"`
	RawJSONL       string          `json:"-"`
	Stderr         string          `json:"stderr,omitempty"`
	ExitError      string          `json:"exit_error,omitempty"`
	Success        bool            `json:"success"`
}

type Usage struct {
	InputTokens         int64 `json:"input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
}

func Run(parent context.Context, req Request) Result {
	started := time.Now()
	if req.ClaudeBinary == "" {
		req.ClaudeBinary = "claude"
	}
	if req.MaxTurns <= 0 {
		req.MaxTurns = 8
	}
	if req.Timeout <= 0 {
		req.Timeout = 10 * time.Minute
	}
	if req.AuthMode == "" {
		req.AuthMode = AuthGateway
	}
	executionModel := modelForAuth(req.Model, req.AuthMode)
	ctx, cancel := context.WithTimeout(parent, req.Timeout)
	defer cancel()

	args := commandArgs(req, executionModel)

	cmd := exec.CommandContext(ctx, req.ClaudeBinary, args...)
	cmd.Dir = req.WorkDir
	// Claude Code sets CLAUDECODE in interactive sessions and refuses a normal
	// nested launch. This binary is an explicit bounded child executor, so mark
	// it as a child session and remove only the recursion sentinel. Provider and
	// gateway credentials continue to come from the dedicated settings overlay.
	if req.AuthMode == AuthNativeSubscription {
		cmd.Env = nativeChildEnvironment(os.Environ())
	} else {
		configDir, configErr := childConfigDir(req.SettingsPath)
		if configErr != nil {
			return Result{Model: req.Model, AuthSource: req.AuthMode, Effort: req.Effort, ToolUses: map[string]int{}, DurationMS: time.Since(started).Milliseconds(), ExitError: configErr.Error()}
		}
		cmd.Env = childEnvironment(os.Environ(), configDir)
	}
	cmd.Env = append(cmd.Env,
		"CLAUDEX_THREAD_MODEL="+req.Model,
		"CLAUDEX_THREAD_EFFORT="+req.Effort,
		"CLAUDEX_THREAD_ROLE="+req.Role,
	)
	cmd.Env = appendThreadContext(cmd.Env, req.RootSessionID, req.ParentSessionID)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	result := Result{Model: req.Model, AuthSource: req.AuthMode, Effort: req.Effort, ToolUses: map[string]int{}, DurationMS: time.Since(started).Milliseconds(), RawJSONL: stdout.String(), Stderr: stderr.String()}
	parseJSONL(stdout.Bytes(), &result)
	if err != nil {
		result.ExitError = err.Error()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.ExitError = "timeout: " + req.Timeout.String()
		}
	}
	result.Success = err == nil && !result.IsError && (strings.TrimSpace(result.Text) != "" || len(result.Structured) != 0)
	return result
}

func commandArgs(req Request, executionModel string) []string {
	args := []string{}
	// Native Claude must read the user's ordinary Claude subscription config.
	// Loading the Claude X settings overlay here would re-introduce
	// ANTHROPIC_BASE_URL/AUTH_TOKEN and disable claude.ai connectors.
	if req.SettingsPath != "" && req.AuthMode != AuthNativeSubscription {
		args = append(args, "--settings", req.SettingsPath)
	}
	// A delegated model is a leaf executor. Ignore user/project MCP servers and
	// expose an explicitly empty MCP registry so a specialist cannot recursively
	// delegate or inherit unrelated open-world tools.
	args = append(args, "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`)
	args = append(args, "-p", req.Prompt)
	if req.ResumeSession != "" {
		args = append(args, "--resume", req.ResumeSession)
	}
	args = append(args, "--model", executionModel)
	if req.Effort != "" {
		args = append(args, "--effort", req.Effort)
	}
	if req.JSONSchema != "" {
		args = append(args, "--json-schema", req.JSONSchema)
	}
	if len(req.Tools) > 0 {
		joined := strings.Join(req.Tools, ",")
		args = append(args, "--tools", joined, "--allowedTools", joined, "--permission-mode", "bypassPermissions")
	}
	args = append(args, "--max-turns", fmt.Sprint(req.MaxTurns), "--output-format", "stream-json", "--verbose")
	return args
}

func AuthModeForProvider(provider string) AuthMode {
	if strings.EqualFold(strings.TrimSpace(provider), "anthropic") {
		return AuthNativeSubscription
	}
	return AuthGateway
}

func modelForAuth(model string, mode AuthMode) string {
	if mode == AuthNativeSubscription && model == "claude-fable-5" {
		return "fable"
	}
	return model
}

func childConfigDir(settingsPath string) (string, error) {
	if configured := strings.TrimSpace(os.Getenv("CLAUDEX_WORKER_CONFIG_DIR")); configured != "" {
		if err := os.MkdirAll(configured, 0o700); err != nil {
			return "", fmt.Errorf("create worker config dir: %w", err)
		}
		return configured, nil
	}
	if strings.TrimSpace(settingsPath) == "" {
		return "", nil
	}
	dir := filepath.Join(filepath.Dir(settingsPath), "worker-runtime")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create worker config dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", fmt.Errorf("secure worker config dir: %w", err)
	}
	return dir, nil
}

func childEnvironment(env []string, configDir string) []string {
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if strings.HasPrefix(item, "CLAUDECODE=") || strings.HasPrefix(item, "CLAUDE_CODE_CHILD_SESSION=") || strings.HasPrefix(item, "CLAUDE_CONFIG_DIR=") || strings.HasPrefix(item, "CLAUDEX_THREAD_MODEL=") || strings.HasPrefix(item, "CLAUDEX_THREAD_EFFORT=") || strings.HasPrefix(item, "CLAUDEX_THREAD_ROLE=") || strings.HasPrefix(item, "CLAUDEX_THREAD_ROOT_SESSION_ID=") || strings.HasPrefix(item, "CLAUDEX_THREAD_PARENT_SESSION_ID=") {
			continue
		}
		out = append(out, item)
	}
	out = append(out, "CLAUDE_CODE_CHILD_SESSION=1")
	if configDir != "" {
		out = append(out, "CLAUDE_CONFIG_DIR="+configDir)
	}
	return out
}

func nativeChildEnvironment(env []string) []string {
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if strings.HasPrefix(item, "CLAUDECODE=") || strings.HasPrefix(item, "CLAUDE_CODE_CHILD_SESSION=") || strings.HasPrefix(item, "CLAUDE_CONFIG_DIR=") || strings.HasPrefix(item, "CLAUDEX_THREAD_MODEL=") || strings.HasPrefix(item, "CLAUDEX_THREAD_EFFORT=") || strings.HasPrefix(item, "CLAUDEX_THREAD_ROLE=") || strings.HasPrefix(item, "CLAUDEX_THREAD_ROOT_SESSION_ID=") || strings.HasPrefix(item, "CLAUDEX_THREAD_PARENT_SESSION_ID=") || strings.HasPrefix(item, "ANTHROPIC_API_KEY=") || strings.HasPrefix(item, "ANTHROPIC_AUTH_TOKEN=") || strings.HasPrefix(item, "ANTHROPIC_BASE_URL=") {
			continue
		}
		out = append(out, item)
	}
	return append(out, "CLAUDE_CODE_CHILD_SESSION=1")
}

func appendThreadContext(env []string, rootSessionID, parentSessionID string) []string {
	if rootSessionID != "" {
		env = append(env, "CLAUDEX_THREAD_ROOT_SESSION_ID="+rootSessionID)
	}
	if parentSessionID != "" {
		env = append(env, "CLAUDEX_THREAD_PARENT_SESSION_ID="+parentSessionID)
	}
	return env
}

func parseJSONL(raw []byte, result *Result) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 4*1024*1024)
	for scanner.Scan() {
		var event map[string]any
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		if event["type"] == "result" {
			result.Subtype, _ = event["subtype"].(string)
			result.IsError, _ = event["is_error"].(bool)
			result.TerminalReason, _ = event["terminal_reason"].(string)
			if text, ok := event["result"].(string); ok {
				result.Text = text
			}
			if structured, ok := event["structured_output"]; ok {
				if raw, err := json.Marshal(structured); err == nil {
					result.Structured = raw
				}
			}
			if usage, ok := event["usage"].(map[string]any); ok {
				result.Usage = usageFromMap(usage)
			}
		}
		if sessionID, ok := event["session_id"].(string); ok && sessionID != "" {
			result.SessionID = sessionID
		}
		message, _ := event["message"].(map[string]any)
		if model, _ := message["model"].(string); model != "" {
			result.ResolvedModel = model
		}
		content, _ := message["content"].([]any)
		for _, item := range content {
			block, _ := item.(map[string]any)
			if block["type"] == "tool_use" {
				name, _ := block["name"].(string)
				if name == "" {
					name = "unknown"
				}
				result.ToolUses[name]++
				if name == "Write" || name == "Edit" {
					input, _ := block["input"].(map[string]any)
					if path, _ := input["file_path"].(string); path != "" {
						result.ChangedPaths = append(result.ChangedPaths, path)
					}
				}
			}
		}
	}
	result.ChangedPaths = uniqueSorted(result.ChangedPaths)
}

func usageFromMap(usage map[string]any) Usage {
	return Usage{
		InputTokens:         int64FromAny(usage["input_tokens"]),
		CacheCreationTokens: int64FromAny(usage["cache_creation_input_tokens"]),
		CacheReadTokens:     int64FromAny(usage["cache_read_input_tokens"]),
		OutputTokens:        int64FromAny(usage["output_tokens"]),
	}
}

func int64FromAny(value any) int64 {
	switch n := value.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func (r Result) FailureDetail() string {
	parts := make([]string, 0, 5)
	if r.ExitError != "" {
		parts = append(parts, r.ExitError)
	}
	if r.Subtype != "" {
		parts = append(parts, "result subtype: "+r.Subtype)
	}
	if r.TerminalReason != "" {
		parts = append(parts, "terminal reason: "+r.TerminalReason)
	}
	if r.IsError && strings.TrimSpace(r.Text) != "" {
		parts = append(parts, strings.TrimSpace(r.Text))
	}
	if strings.TrimSpace(r.Stderr) != "" {
		parts = append(parts, strings.TrimSpace(r.Stderr))
	}
	if len(parts) == 0 {
		return "empty or invalid model result"
	}
	return strings.Join(parts, "\n")
}
