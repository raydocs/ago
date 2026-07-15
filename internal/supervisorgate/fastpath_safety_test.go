package supervisorgate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

const fastPathPrompt = "fix internal/catalog/catalog.go and run go test ./internal/catalog"

func runFastPathHook(t *testing.T, cfg Config, hook HookInput) Decision {
	t.Helper()
	decision, _, err := Run(bytes.NewReader(hookJSON(t, hook)), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

func startFastPath(t *testing.T, cfg Config, session string) {
	t.Helper()
	decision := runFastPathHook(t, cfg, HookInput{SessionID: session, HookEventName: "UserPromptSubmit", Prompt: fastPathPrompt})
	if decision.State != "fast_path" {
		t.Fatalf("expected fast path, got %#v", decision)
	}
}

func TestFastPathSafetyHardDeniesOrchestration(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	startFastPath(t, cfg, "fast-deny-route")
	decision := runFastPathHook(t, cfg, HookInput{
		SessionID: "fast-deny-route", HookEventName: "PreToolUse",
		ToolName: "mcp__claudex-flow__route_task", ToolInput: json.RawMessage(`{"objective":"tiny change"}`),
	})
	if decision.Permission != "deny" || decision.State != "fast_path_deny" {
		t.Fatalf("Fast Path must deny route_task, got %#v", decision)
	}
}

func TestFastPathSafetyRequiresEditBeforeVerifier(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	startFastPath(t, cfg, "fast-edit-first")
	decision := runFastPathHook(t, cfg, HookInput{
		SessionID: "fast-edit-first", HookEventName: "PreToolUse", ToolName: "Bash",
		ToolInput: json.RawMessage(`{"command":"go test ./internal/catalog"}`),
	})
	if decision.Permission != "deny" || !strings.Contains(decision.Reason, "edited successfully") {
		t.Fatalf("verifier before edit must be denied, got %#v", decision)
	}
}

func TestFastPathSafetyRejectsWrongWriteScope(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	startFastPath(t, cfg, "fast-wrong-path")
	decision := runFastPathHook(t, cfg, HookInput{
		SessionID: "fast-wrong-path", HookEventName: "PreToolUse", ToolName: "Edit",
		ToolInput: json.RawMessage(`{"file_path":"internal/catalog/other.go"}`),
	})
	if decision.Permission != "deny" || !strings.Contains(decision.Reason, "write scope") {
		t.Fatalf("wrong write scope must be denied, got %#v", decision)
	}
}

func TestFastPathSafetyDoesNotAdmitMaskedVerifier(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	decision := runFastPathHook(t, cfg, HookInput{
		SessionID: "fast-masked", HookEventName: "UserPromptSubmit",
		Prompt: "fix internal/catalog/catalog.go and run go test ./internal/catalog || true",
	})
	if decision.State == "fast_path" {
		t.Fatalf("masked verifier must not enter Fast Path: %#v", decision)
	}
}

func TestFastPathSafetyDoesNotAdmitWithOpenGate(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir, HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	if _, err := DeclareGate(dir, DeclareGateInput{SessionID: "fast-open-gate", GateID: "existing", Acceptance: []string{"existing task passes"}}, fixedNow(t)()); err != nil {
		t.Fatal(err)
	}
	decision := runFastPathHook(t, cfg, HookInput{SessionID: "fast-open-gate", HookEventName: "UserPromptSubmit", Prompt: fastPathPrompt})
	if decision.State == "fast_path" {
		t.Fatalf("open lifecycle must prevent Fast Path admission: %#v", decision)
	}
}

func TestFastPathSafetyFailureEventDoesNotLatch(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	session := "fast-failed-test"
	startFastPath(t, cfg, session)
	runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PostToolUse", ToolName: "Edit",
		ToolInput: json.RawMessage(`{"file_path":"internal/catalog/catalog.go"}`),
	})
	runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PostToolUseFailure", ToolName: "Bash",
		ToolInput: json.RawMessage(`{"command":"go test ./internal/catalog"}`), Error: "exit status 1",
	})
	decision := runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Read",
		ToolInput: json.RawMessage(`{"file_path":"internal/catalog/catalog.go"}`),
	})
	if decision.Permission == "deny" {
		t.Fatalf("failed verifier must not latch VerifiedStop: %#v", decision)
	}
}

func TestFastPathSafetyInterruptedVerifierDoesNotLatch(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	session := "fast-interrupted"
	startFastPath(t, cfg, session)
	runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PostToolUse", ToolName: "Edit",
		ToolInput: json.RawMessage(`{"file_path":"internal/catalog/catalog.go"}`),
	})
	runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PostToolUse", ToolName: "Bash",
		ToolInput:    json.RawMessage(`{"command":"go test ./internal/catalog"}`),
		ToolResponse: json.RawMessage(`{"stdout":"ok","interrupted":true}`),
	})
	decision := runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Read",
		ToolInput: json.RawMessage(`{"file_path":"internal/catalog/catalog.go"}`),
	})
	if decision.Permission == "deny" {
		t.Fatalf("interrupted verifier must not latch VerifiedStop: %#v", decision)
	}
}

func TestFastPathSafetySuccessfulFrozenVerifierHardStops(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	session := "fast-success"
	startFastPath(t, cfg, session)
	decision := runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Read",
		ToolInput: json.RawMessage(`{"file_path":"internal/catalog/catalog.go"}`),
	})
	if decision.Permission != "allow" {
		t.Fatalf("target read should be allowed: %#v", decision)
	}
	runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PostToolUse", ToolName: "Edit",
		ToolInput: json.RawMessage(`{"file_path":"internal/catalog/catalog.go"}`),
	})
	decision = runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Bash",
		ToolInput: json.RawMessage(`{"command":"go test ./internal/catalog"}`),
	})
	if decision.Permission != "allow" {
		t.Fatalf("frozen verifier should be allowed after edit: %#v", decision)
	}
	decision = runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PostToolUse", ToolName: "Bash",
		ToolInput:    json.RawMessage(`{"command":"go test ./internal/catalog"}`),
		ToolResponse: json.RawMessage(`{"stdout":"ok","stderr":"","interrupted":false}`),
	})
	if decision.State != "verified_stop_latched" {
		t.Fatalf("successful frozen verifier should latch: %#v", decision)
	}
	decision = runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Read",
		ToolInput: json.RawMessage(`{"file_path":"internal/catalog/catalog.go"}`),
	})
	if decision.Permission != "deny" || decision.State != "verified_stop_deny" {
		t.Fatalf("post-verify tool must be denied: %#v", decision)
	}
}

func TestFastPathAllowsOneExactReadOnlyTargetDiff(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	session := "fast-target-diff"
	startFastPath(t, cfg, session)
	runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PostToolUse", ToolName: "Edit",
		ToolInput: json.RawMessage(`{"file_path":"internal/catalog/catalog.go"}`),
	})
	diff := json.RawMessage(`{"command":"git diff -- internal/catalog/catalog.go"}`)
	decision := runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Bash", ToolInput: diff,
	})
	if decision.Permission != "allow" || decision.State != "fast_path_diff_allow" {
		t.Fatalf("exact target diff must be allowed once: %#v", decision)
	}
	runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PostToolUse", ToolName: "Bash", ToolInput: diff,
	})
	decision = runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Bash", ToolInput: diff,
	})
	if decision.Permission != "deny" || !strings.Contains(decision.Reason, "already inspected") {
		t.Fatalf("duplicate target diff must be denied: %#v", decision)
	}
	decision = runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Bash",
		ToolInput: json.RawMessage(`{"command":"go test ./internal/catalog; git diff -- internal/catalog/catalog.go"}`),
	})
	if decision.Permission != "deny" {
		t.Fatalf("compound verifier/diff must remain denied: %#v", decision)
	}
	decision = runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Bash",
		ToolInput: json.RawMessage(`{"command":"go test ./internal/catalog"}`),
	})
	if decision.Permission != "allow" {
		t.Fatalf("diff inspection must not consume verifier: %#v", decision)
	}
}

func TestFastPathDiffRejectsWrongPathAndFlags(t *testing.T) {
	cfg := Config{StateDir: t.TempDir(), HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	session := "fast-diff-scope"
	startFastPath(t, cfg, session)
	runFastPathHook(t, cfg, HookInput{
		SessionID: session, HookEventName: "PostToolUse", ToolName: "Edit",
		ToolInput: json.RawMessage(`{"file_path":"internal/catalog/catalog.go"}`),
	})
	for _, command := range []string{
		"git diff -- internal/catalog/other.go",
		"git diff --stat -- internal/catalog/catalog.go",
		"git diff -- internal/catalog/catalog.go internal/catalog/other.go",
		"git diff -- internal/catalog/catalog.go | cat",
	} {
		decision := runFastPathHook(t, cfg, HookInput{
			SessionID: session, HookEventName: "PreToolUse", ToolName: "Bash",
			ToolInput: json.RawMessage(`{"command":` + jsonString(command) + `}`),
		})
		if decision.Permission != "deny" {
			t.Fatalf("unsafe diff %q admitted: %#v", command, decision)
		}
	}
}

func BenchmarkFastPathPromptAndRead(b *testing.B) {
	cfg := Config{StateDir: b.TempDir(), HighCostSoft: 100, HighCostHard: 100, Now: time.Now}
	for i := 0; i < b.N; i++ {
		session := fmt.Sprintf("bench-fast-%d", i)
		for _, hook := range []HookInput{
			{SessionID: session, HookEventName: "UserPromptSubmit", Prompt: fastPathPrompt},
			{SessionID: session, HookEventName: "PreToolUse", ToolName: "Read", ToolInput: json.RawMessage(`{"file_path":"internal/catalog/catalog.go"}`)},
		} {
			raw, err := json.Marshal(hook)
			if err != nil {
				b.Fatal(err)
			}
			if _, _, err := Run(bytes.NewReader(raw), cfg); err != nil {
				b.Fatal(err)
			}
		}
	}
}
