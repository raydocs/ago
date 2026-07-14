package supervisorgate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPlaywrightBudgetDeniesAfterCap(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir, MaxPlaywright: 2, Now: fixedNow(t)}
	session := "sess-play"
	for i := 0; i < 2; i++ {
		in := hookJSON(t, HookInput{
			SessionID: session, HookEventName: "PostToolUse",
			ToolName:  "mcp__plugin_playwright_playwright__browser_click",
			ToolInput: json.RawMessage(`{"selector":"#x"}`),
		})
		if _, _, err := Run(bytes.NewReader(in), cfg); err != nil {
			t.Fatal(err)
		}
	}
	in := hookJSON(t, HookInput{
		SessionID: session, HookEventName: "PreToolUse",
		ToolName:  "mcp__plugin_playwright_playwright__browser_click",
		ToolInput: json.RawMessage(`{"selector":"#y"}`),
	})
	dec, out, err := Run(bytes.NewReader(in), cfg)
	if err != nil {
		t.Fatal(err)
	}
	// After soft sticky, high-cost may deny as sticky first if soft was crossed.
	if dec.Permission != "deny" {
		t.Fatalf("expected deny, got %#v", dec)
	}
	if !strings.Contains(dec.Reason, "PLAYWRIGHT") && !strings.Contains(dec.Reason, "STICKY") {
		t.Fatalf("unexpected reason: %#v out=%s", dec, out)
	}
}

func TestSameVerifyBudget(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir, MaxSameVerify: 2, HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	session := "sess-verify"
	payload := json.RawMessage(`{"command":"go test ./internal/threadgraph"}`)
	for i := 0; i < 2; i++ {
		in := hookJSON(t, HookInput{SessionID: session, HookEventName: "PostToolUse", ToolName: "Bash", ToolInput: payload})
		if _, _, err := Run(bytes.NewReader(in), cfg); err != nil {
			t.Fatal(err)
		}
	}
	in := hookJSON(t, HookInput{SessionID: session, HookEventName: "PreToolUse", ToolName: "Bash", ToolInput: payload})
	dec, _, err := Run(bytes.NewReader(in), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Permission != "deny" || !strings.Contains(dec.Reason, "VERIFY_BUDGET") {
		t.Fatalf("expected verify budget deny, got %#v", dec)
	}
}

func TestRootHandoffAfterCompacts(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir, MaxCompacts: 2, Now: fixedNow(t)}
	session := "sess-life"
	for i := 0; i < 2; i++ {
		if _, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{SessionID: session, HookEventName: "PostCompact"})), cfg); err != nil {
			t.Fatal(err)
		}
	}
	dec, out, err := Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Edit", ToolInput: json.RawMessage(`{"file_path":"a.go"}`),
	})), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Permission != "deny" || !strings.Contains(dec.Reason, "ROOT_HANDOFF") {
		t.Fatalf("expected handoff deny, got %#v", dec)
	}
	if !strings.Contains(string(out), "CLAUDEX_ROOT_HANDOFF_REQUIRED") {
		t.Fatalf("missing handoff context: %s", out)
	}
	dec, _, err = Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Read", ToolInput: json.RawMessage(`{"file_path":"a.go"}`),
	})), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Permission != "allow" {
		t.Fatalf("Read should remain allowed during handoff, got %#v", dec)
	}
}

func TestHandoffFailClosedUnknownMCPAndBash(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir, MaxCompacts: 1, Now: fixedNow(t)}
	session := "sess-bypass"
	if _, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{SessionID: session, HookEventName: "PostCompact"})), cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, tool string
		input      json.RawMessage
	}{
		{"bash-any", "Bash", json.RawMessage(`{"command":"ls -la"}`)},
		{"bash-bypass-and", "Bash", json.RawMessage(`{"command":"pwd & touch /tmp/x"}`)},
		{"bash-procsub", "Bash", json.RawMessage(`{"command":"ls <(touch /tmp/x)"}`)},
		{"unknown-mcp-write", "mcp__filesystem__write_file", json.RawMessage(`{"path":"/tmp/x"}`)},
		{"task", "Task", json.RawMessage(`{"description":"x"}`)},
		{"skill", "Skill", json.RawMessage(`{"skill":"x"}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dec, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{
				SessionID: session, HookEventName: "PreToolUse", ToolName: tc.tool, ToolInput: tc.input,
			})), cfg)
			if err != nil {
				t.Fatal(err)
			}
			if dec.Permission != "deny" {
				t.Fatalf("expected deny for %s, got %#v", tc.tool, dec)
			}
		})
	}
	// Control + Read still allowed.
	dec, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Read",
		ToolInput: json.RawMessage(`{"file_path":"a.go"}`),
	})), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Permission != "allow" {
		t.Fatalf("Read should allow, got %#v", dec)
	}
}

func TestDestructiveGitDeniedOnNormalRoot(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir, HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	for _, cmd := range []string{"git reset --hard HEAD", "git push --force origin main"} {
		dec, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{
			SessionID: "g", HookEventName: "PreToolUse", ToolName: "Bash",
			ToolInput: json.RawMessage(`{"command":` + jsonString(cmd) + `}`),
		})), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if dec.Permission != "deny" || !strings.Contains(dec.Reason, "GIT_DESTRUCTIVE") {
			t.Fatalf("cmd %q => %#v", cmd, dec)
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestParallelPostToolUseCountsExactly(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir, MaxPlaywright: 1000, HighCostHard: 10000, HighCostSoft: 10000, Now: fixedNow(t)}
	session := "sess-parallel"
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := hookJSON(t, HookInput{
				SessionID: session, HookEventName: "PostToolUse",
				ToolName: "Agent", ToolInput: json.RawMessage(`{"prompt":"x"}`),
			})
			if _, _, err := Run(bytes.NewReader(in), cfg); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	st, err := LoadStatus(dir, session)
	if err != nil {
		t.Fatal(err)
	}
	if st.HighCostCalls != n {
		t.Fatalf("parallel high-cost count = %d, want %d", st.HighCostCalls, n)
	}
}

func TestStickyRerouteDeniesUntilAck(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir, HighCostSoft: 2, HighCostHard: 50, MaxPlaywright: 100, Now: fixedNow(t)}
	session := "sess-sticky"
	if _, err := DeclareGate(dir, DeclareGateInput{
		SessionID: session, GateID: "g1", Acceptance: []string{"tests pass"},
	}, fixedNow(t)()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{
			SessionID: session, HookEventName: "PostToolUse", ToolName: "Agent",
			ToolInput: json.RawMessage(`{"prompt":"x"}`),
		})), cfg); err != nil {
			t.Fatal(err)
		}
	}
	dec, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Agent",
		ToolInput: json.RawMessage(`{"prompt":"y"}`),
	})), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Permission != "deny" || !strings.Contains(dec.Reason, "STICKY") {
		t.Fatalf("expected sticky deny, got %#v", dec)
	}
	// Control tool still allowed.
	dec, _, err = Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: session, HookEventName: "PreToolUse",
		ToolName: "mcp__claudex-flow__gate_status", ToolInput: json.RawMessage(`{}`),
	})), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Permission != "allow" {
		t.Fatalf("control tool should allow, got %#v", dec)
	}
	if _, err := AckReroute(dir, AckRerouteInput{
		SessionID: session, GateID: "g1", RemainingAcceptance: []string{"tests pass"}, WorkerDecision: "none",
	}, fixedNow(t)()); err != nil {
		t.Fatal(err)
	}
	dec, _, err = Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Agent",
		ToolInput: json.RawMessage(`{"prompt":"z"}`),
	})), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Permission != "allow" {
		t.Fatalf("after ack, Agent should allow, got %#v", dec)
	}
}

func TestDeclareGateResetsGateBudgetNotRoot(t *testing.T) {
	dir := t.TempDir()
	now := fixedNow(t)()
	session := "sess-dual"
	if _, err := DeclareGate(dir, DeclareGateInput{
		SessionID: session, GateID: "a", Acceptance: []string{"a"},
	}, now); err != nil {
		t.Fatal(err)
	}
	cfg := Config{StateDir: dir, GateMaxPlaywright: 2, MaxPlaywright: 12, HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	for i := 0; i < 2; i++ {
		if _, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{
			SessionID: session, HookEventName: "PostToolUse",
			ToolName: "mcp__plugin_playwright_playwright__browser_click", ToolInput: json.RawMessage(`{}`),
		})), cfg); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := LoadStatus(dir, session)
	if st.PlaywrightCalls != 2 || st.OpenGate == nil || st.OpenGate.PlaywrightCalls != 2 {
		t.Fatalf("pre-close counts: %#v", st)
	}
	if _, err := CloseGate(dir, CloseGateInput{SessionID: session, GateID: "a", Status: "closed"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := DeclareGate(dir, DeclareGateInput{
		SessionID: session, GateID: "b", Acceptance: []string{"b different"},
	}, now); err != nil {
		t.Fatal(err)
	}
	st, _ = LoadStatus(dir, session)
	if st.PlaywrightCalls != 2 {
		t.Fatalf("Root playwright must not reset, got %d", st.PlaywrightCalls)
	}
	if st.OpenGate == nil || st.OpenGate.PlaywrightCalls != 0 {
		t.Fatalf("new gate counters must start at 0: %#v", st.OpenGate)
	}
}

func TestGateSpamBlockedBySameAcceptanceHash(t *testing.T) {
	dir := t.TempDir()
	now := fixedNow(t)()
	session := "sess-spam"
	if _, err := DeclareGate(dir, DeclareGateInput{
		SessionID: session, GateID: "g1", Acceptance: []string{"same"},
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := CloseGate(dir, CloseGateInput{SessionID: session, GateID: "g1"}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := DeclareGate(dir, DeclareGateInput{
		SessionID: session, GateID: "g2", Acceptance: []string{"same"},
	}, now); err == nil {
		t.Fatal("expected same acceptance hash rejection")
	}
}

func TestUserGateOverride(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir, MaxCompacts: 99, HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	session := "sess-ov"
	// Arm sticky without override first is separate; here test override path after budget-like deny setup.
	// Issue override via UserPromptSubmit.
	dec, out, err := Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: session, HookEventName: "UserPromptSubmit",
		Prompt: "/gate-override reason=hotfix paths=internal/foo.go classes=Edit",
	})), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec.State != "override_issued" || !strings.Contains(string(out), "GATE_OVERRIDE") {
		t.Fatalf("override issue failed: %#v %s", dec, out)
	}
	// Edit on scoped path allowed.
	dec, _, err = Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: session, HookEventName: "PreToolUse", ToolName: "Edit",
		ToolInput: json.RawMessage(`{"file_path":"internal/foo.go"}`),
	})), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Permission != "allow" || dec.State != "override_allow" {
		t.Fatalf("override should allow Edit, got %#v", dec)
	}
}

func TestChildSessionSkipped(t *testing.T) {
	t.Setenv("CLAUDE_CODE_CHILD_SESSION", "1")
	dir := t.TempDir()
	dec, out, err := Run(bytes.NewReader(hookJSON(t, HookInput{SessionID: "child", HookEventName: "PreToolUse", ToolName: "Edit"})), Config{StateDir: dir, Now: fixedNow(t)})
	if err != nil {
		t.Fatal(err)
	}
	if dec.State != "child_skip" || out != nil {
		t.Fatalf("child should skip, got %#v out=%s", dec, out)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("child session wrote state: %v", entries)
	}
}

func TestTranscriptSizeTriggersHandoff(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "big.jsonl")
	if err := os.WriteFile(transcript, bytes.Repeat([]byte("x"), 100), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{StateDir: dir, MaxTranscriptBytes: 50, MaxCompacts: 99, MaxAge: 24 * time.Hour, Now: fixedNow(t)}
	dec, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: "sess-bytes", HookEventName: "PreToolUse", ToolName: "Write",
		TranscriptPath: transcript, ToolInput: json.RawMessage(`{"file_path":"a"}`),
	})), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Permission != "deny" || !strings.Contains(dec.Reason, "ROOT_HANDOFF") {
		t.Fatalf("expected transcript handoff deny, got %#v", dec)
	}
}

func TestBashReadOnlyWhitelist(t *testing.T) {
	if !bashReadOnly(json.RawMessage(`{"command":"ls"}`)) {
		t.Fatal("ls should be readonly")
	}
	if bashReadOnly(json.RawMessage(`{"command":"printf x > f"}`)) {
		t.Fatal("redirect must not be readonly")
	}
}

func TestHandoffWritesCapsule(t *testing.T) {
	dir := t.TempDir()
	// Redirect handoff dir via HOME
	t.Setenv("HOME", dir)
	cfg := Config{StateDir: filepath.Join(dir, "gate"), MaxCompacts: 1, Now: fixedNow(t)}
	session := "sess-cap"
	if _, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{SessionID: session, HookEventName: "PostCompact"})), cfg); err != nil {
		t.Fatal(err)
	}
	st, err := LoadStatus(cfg.StateDir, session)
	if err != nil {
		t.Fatal(err)
	}
	if !st.HandoffRequired || st.HandoffMDPath == "" || st.HandoffJSONPath == "" {
		t.Fatalf("expected capsule paths, got %#v", st)
	}
	if _, err := os.Stat(st.HandoffMDPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.HandoffJSONPath); err != nil {
		t.Fatal(err)
	}
}

func TestStopFailureLatchesOverflow(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{StateDir: dir, MaxCompacts: 99, Now: fixedNow(t)}
	session := "sess-ovf"
	if _, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: session, HookEventName: "StopFailure",
		Error: "invalid_request", LastAssistantMsg: "Prompt is too long",
	})), cfg); err != nil {
		t.Fatal(err)
	}
	st, err := LoadStatus(dir, session)
	if err != nil {
		t.Fatal(err)
	}
	if !st.OverflowLatched || !st.HandoffRequired {
		t.Fatalf("expected overflow latch, got %#v", st)
	}
}

func TestStrictAgentDeny(t *testing.T) {
	t.Setenv("CLAUDEX_WORKFLOW_STRICT", "1")
	dir := t.TempDir()
	cfg := Config{StateDir: dir, HighCostSoft: 100, HighCostHard: 100, Now: fixedNow(t)}
	dec, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: "s", HookEventName: "PreToolUse", ToolName: "Agent", ToolInput: json.RawMessage(`{}`),
	})), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if dec.Permission != "deny" || !strings.Contains(dec.Reason, "STRICT") {
		t.Fatalf("expected strict deny, got %#v", dec)
	}
}

func TestContextPressureSoftDoesNotHandoffAndResetsOnCompact(t *testing.T) {
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "272000")
	dir := t.TempDir()
	cfg := Config{StateDir: dir, MaxCompacts: 99, HighCostSoft: 1000, HighCostHard: 1000, Now: fixedNow(t)}
	session := "ctx"
	// Soft: ~78% of 272k ≈ 212k; inject 220000 tokens without huge tool bytes hard.
	dec, out, err := Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: session, HookEventName: "PostToolUse", ToolName: "Read",
		ToolInput: json.RawMessage(`{}`), ToolResponse: json.RawMessage(`{"ok":true}`),
		InputTokens: 220000,
	})), cfg)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := LoadStatus(dir, session)
	if st.HandoffRequired {
		t.Fatalf("soft must not handoff: %#v dec=%#v out=%s", st, dec, out)
	}
	if st.ContextPressure != "soft" && !strings.Contains(string(out)+dec.Context, "CONTEXT_PRESSURE") {
		// soft context may only appear on next PreToolUse
		if st.LatestPromptTokens != 220000 {
			t.Fatalf("expected current sample 220000, got %#v", st)
		}
	}
	// PostCompact resets samples.
	if _, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{SessionID: session, HookEventName: "PostCompact"})), cfg); err != nil {
		t.Fatal(err)
	}
	st, _ = LoadStatus(dir, session)
	if st.LatestPromptTokens != 0 || st.ToolResultBytesWindow != 0 {
		t.Fatalf("PostCompact must reset samples, got %#v", st)
	}
}

func TestContextPressureHardLatchesHandoff(t *testing.T) {
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "272000")
	dir := t.TempDir()
	cfg := Config{StateDir: dir, MaxCompacts: 99, Now: fixedNow(t)}
	session := "ctxh"
	// hard ≈ 90% of 272k = 244800
	if _, _, err := Run(bytes.NewReader(hookJSON(t, HookInput{
		SessionID: session, HookEventName: "PostToolUse", ToolName: "Read",
		ToolInput: json.RawMessage(`{}`), InputTokens: 250000,
	})), cfg); err != nil {
		t.Fatal(err)
	}
	st, _ := LoadStatus(dir, session)
	if !st.HandoffRequired || st.ContextPressure != "hard" {
		t.Fatalf("hard must handoff: %#v", st)
	}
}

func fixedNow(t *testing.T) func() time.Time {
	t.Helper()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return now }
}

func hookJSON(t *testing.T, in HookInput) []byte {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
