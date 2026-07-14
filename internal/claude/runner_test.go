package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseJSONL(t *testing.T) {
	raw := []byte("{\"type\":\"assistant\",\"session_id\":\"session-1\",\"message\":{\"model\":\"claude-opus-4-8[1m]\",\"content\":[{\"type\":\"tool_use\",\"name\":\"Write\",\"input\":{\"file_path\":\"/tmp/a\"}}]}}\n{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"terminal_reason\":\"completed\",\"session_id\":\"session-1\",\"result\":\"{\\\"status\\\":\\\"completed\\\"}\",\"structured_output\":{\"status\":\"completed\"},\"usage\":{\"input_tokens\":10,\"cache_creation_input_tokens\":2,\"cache_read_input_tokens\":3,\"output_tokens\":4}}\n")
	r := Result{ToolUses: map[string]int{}}
	parseJSONL(raw, &r)
	if r.SessionID != "session-1" || r.ResolvedModel != "claude-opus-4-8[1m]" || r.Text == "" || string(r.Structured) != `{"status":"completed"}` || r.ToolUses["Write"] != 1 || len(r.ChangedPaths) != 1 || r.Usage.InputTokens != 10 || r.Usage.CacheReadTokens != 3 || r.Subtype != "success" || r.IsError {
		t.Fatalf("parsed %#v", r)
	}
}

func TestNativeChildEnvironmentUsesLocalSubscription(t *testing.T) {
	got := nativeChildEnvironment([]string{"PATH=/bin", "CLAUDECODE=1", "CLAUDE_CONFIG_DIR=/gateway", "ANTHROPIC_API_KEY=key", "ANTHROPIC_AUTH_TOKEN=token", "ANTHROPIC_BASE_URL=http://gateway", "KEEP=yes"})
	joined := strings.Join(got, "\n")
	for _, forbidden := range []string{"CLAUDECODE=", "CLAUDE_CONFIG_DIR=", "ANTHROPIC_API_KEY=", "ANTHROPIC_AUTH_TOKEN=", "ANTHROPIC_BASE_URL="} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("native environment retained %s: %v", forbidden, got)
		}
	}
	if !strings.Contains(joined, "CLAUDE_CODE_CHILD_SESSION=1") || !strings.Contains(joined, "KEEP=yes") {
		t.Fatalf("unexpected native environment: %v", got)
	}
}

func TestAuthModeAndNativeModelAlias(t *testing.T) {
	if got := AuthModeForProvider("anthropic"); got != AuthNativeSubscription {
		t.Fatalf("got %q", got)
	}
	if got := AuthModeForProvider("xai"); got != AuthGateway {
		t.Fatalf("got %q", got)
	}
	if got := modelForAuth("claude-fable-5", AuthNativeSubscription); got != "fable" {
		t.Fatalf("got %q", got)
	}
	if got := modelForAuth("claude-fable-5", AuthGateway); got != "claude-fable-5" {
		t.Fatalf("got %q", got)
	}
}

func TestNativeCommandDoesNotLoadGatewaySettings(t *testing.T) {
	req := Request{
		SettingsPath: "/Users/test/.config/claudex/settings.json",
		AuthMode:     AuthNativeSubscription,
		Prompt:       "review",
		Model:        "claude-fable-5",
		Effort:       "high",
		MaxTurns:     1,
	}
	args := commandArgs(req, modelForAuth(req.Model, req.AuthMode))
	joined := strings.Join(args, "\n")
	if strings.Contains(joined, "--settings") || strings.Contains(joined, req.SettingsPath) {
		t.Fatalf("native command loaded gateway settings: %v", args)
	}
	if !strings.Contains(joined, "fable") || !strings.Contains(joined, "high") {
		t.Fatalf("native model or effort missing: %v", args)
	}

	req.AuthMode = AuthGateway
	args = commandArgs(req, req.Model)
	joined = strings.Join(args, "\n")
	if !strings.Contains(joined, "--settings") || !strings.Contains(joined, req.SettingsPath) {
		t.Fatalf("gateway command lost settings overlay: %v", args)
	}
}

func TestChildEnvironment(t *testing.T) {
	got := childEnvironment([]string{"PATH=/bin", "CLAUDECODE=1", "CLAUDE_CODE_CHILD_SESSION=old", "CLAUDE_CONFIG_DIR=/old", "CLAUDEX_THREAD_ROOT_SESSION_ID=stale", "CLAUDEX_THREAD_PARENT_SESSION_ID=stale", "KEEP=yes"}, "/isolated")
	got = appendThreadContext(got, "root-session", "parent-session")
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "CLAUDECODE=") || strings.Contains(joined, "CLAUDE_CONFIG_DIR=/old") || strings.Contains(joined, "CLAUDEX_THREAD_ROOT_SESSION_ID=stale") || !strings.Contains(joined, "CLAUDE_CONFIG_DIR=/isolated") || !strings.Contains(joined, "CLAUDE_CODE_CHILD_SESSION=1") || !strings.Contains(joined, "CLAUDEX_THREAD_ROOT_SESSION_ID=root-session") || !strings.Contains(joined, "CLAUDEX_THREAD_PARENT_SESSION_ID=parent-session") || !strings.Contains(joined, "KEEP=yes") {
		t.Fatalf("unexpected environment: %v", got)
	}
}

func TestChildConfigDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "claudex", "settings.json")
	got, err := childConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(filepath.Dir(dir), "worker-runtime") {
		t.Fatalf("got %q", got)
	}
	info, err := os.Stat(got)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestFailureDetailIncludesStructuredTerminalEvidence(t *testing.T) {
	r := Result{ExitError: "exit status 1", Subtype: "error_max_turns", IsError: true, Text: "Reached max turns", Stderr: "connector warning"}
	got := r.FailureDetail()
	for _, want := range []string{"exit status 1", "error_max_turns", "Reached max turns", "connector warning"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}
}
