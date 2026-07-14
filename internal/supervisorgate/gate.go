// Package supervisorgate enforces zero-model Supervisor tool budgets, sticky
// re-route, dual Root/gate budgets, and Root lifecycle handoff.
package supervisorgate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const maxHookInputBytes = 4 * 1024 * 1024

// Config limits can be overridden in tests (root budgets).
type Config struct {
	StateDir           string
	MaxPlaywright      int
	MaxScreenshot      int
	MaxSameVerify      int
	HighCostSoft       int
	HighCostHard       int
	MaxCompacts        int
	MaxTranscriptBytes int64
	MaxAge             time.Duration
	// Gate-local (optional test overrides).
	GateMaxPlaywright int
	GateMaxScreenshot int
	GateMaxSameVerify int
	GateHighCostSoft  int
	GateHighCostHard  int
	Now               func() time.Time
}

type HookInput struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	ToolUseID      string          `json:"tool_use_id"`
	Cwd            string          `json:"cwd"`
	Prompt         string          `json:"prompt"`
	// StopFailure / API error surfaces (best-effort).
	Error            string `json:"error"`
	ErrorDetails     string `json:"error_details"`
	LastAssistantMsg string `json:"last_assistant_message"`
	// Optional usage fields if present on hook payloads.
	InputTokens             int64 `json:"input_tokens"`
	CacheReadInputTokens    int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

type Decision struct {
	Permission string
	Reason     string
	Context    string
	State      string
}

type hookOutput struct {
	HookSpecificOutput specificOutput `json:"hookSpecificOutput"`
}

type specificOutput struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
	AdditionalContext        string `json:"additionalContext,omitempty"`
}

// Run handles one Claude Code hook event from stdin and may emit JSON on stdout.
func Run(input io.Reader, cfg Config) (Decision, []byte, error) {
	cfg = normalizeConfig(cfg)
	raw, err := io.ReadAll(io.LimitReader(input, maxHookInputBytes+1))
	if err != nil {
		return Decision{}, nil, err
	}
	if len(raw) > maxHookInputBytes {
		return Decision{}, nil, fmt.Errorf("supervisor-gate hook input exceeds %d bytes", maxHookInputBytes)
	}
	if os.Getenv("CLAUDE_CODE_CHILD_SESSION") == "1" {
		return Decision{State: "child_skip"}, nil, nil
	}
	var hook HookInput
	if err := json.Unmarshal(raw, &hook); err != nil {
		return Decision{}, nil, fmt.Errorf("decode supervisor-gate hook: %w", err)
	}
	if strings.TrimSpace(hook.SessionID) == "" {
		return Decision{State: "no_session"}, nil, nil
	}

	var decision Decision
	err = withSessionLock(cfg.StateDir, hook.SessionID, func() error {
		st, loadErr := loadStateUnlocked(cfg.StateDir, hook.SessionID)
		if loadErr != nil {
			return loadErr
		}
		if st.SessionID == "" {
			st = newRootState(hook.SessionID, cfg.Now())
		}
		if st.VerifyCounts == nil {
			st.VerifyCounts = map[string]int{}
		}

		refreshLifecycle(&st, hook, cfg)
		expireOverride(&st, cfg.Now())
		ingestUsageHint(&st, hook)

		switch hook.HookEventName {
		case "SessionStart":
			decision = Decision{State: "session_start"}
			if st.OverflowLatched || st.HandoffRequired {
				decision.Context = lifecycleContext(st)
			}
		case "UserPromptSubmit":
			decision = handleUserPrompt(&st, hook, cfg)
			if st.OverflowLatched || st.HandoffRequired {
				if decision.Context == "" {
					decision.Context = lifecycleContext(st)
				}
			}
		case "PreCompact", "PostCompact":
			if hook.HookEventName == "PostCompact" {
				st.CompactCount++
				// T3: after compact, pressure samples must decay/reset (not max-ever).
				st.LatestPromptTokens = 0
				st.ToolResultBytesWindow = 0
				if st.ContextPressure == "soft" {
					st.ContextPressure = ""
				}
			}
			refreshLifecycle(&st, hook, cfg)
			decision = Decision{State: "compact_recorded"}
		case "PostToolUse", "PostToolUseFailure":
			decision = recordTool(&st, hook, cfg)
		case "StopFailure", "Stop":
			decision = handleStopFailure(&st, hook, cfg)
		case "PreToolUse":
			decision = evaluatePre(&st, hook, cfg)
		default:
			decision = Decision{State: "ignored"}
		}

		// T2: when handoff first latches, write capsule atomically (failure is observable).
		if st.HandoffRequired && st.HandoffJSONPath == "" {
			if cap, capErr := writeHandoffCapsule(st, DefaultHandoffDir(), cfg.Now()); capErr == nil {
				st.HandoffJSONPath = cap.JSONPath
				st.HandoffMDPath = cap.MarkdownPath
			} else {
				_ = appendEvent(cfg.StateDir, gateEvent{
					Event: "handoff_capsule_failure", Observed: cfg.Now().UTC().Format(time.RFC3339Nano),
					SessionID: st.SessionID, Detail: capErr.Error(),
				})
				if decision.Context == "" {
					decision.Context = "CLAUDEX_HANDOFF_CAPSULE_FAILURE: " + capErr.Error()
				} else {
					decision.Context += " | CLAUDEX_HANDOFF_CAPSULE_FAILURE: " + capErr.Error()
				}
			}
		}

		st.UpdatedAt = cfg.Now().UTC().Format(time.RFC3339Nano)
		if saveErr := saveStateUnlocked(cfg.StateDir, st); saveErr != nil {
			return saveErr
		}
		if decision.Permission == "deny" {
			_ = appendEvent(cfg.StateDir, gateEvent{
				Event: "gate_deny", Observed: st.UpdatedAt, SessionID: st.SessionID,
				HookEvent: hook.HookEventName, ToolName: hook.ToolName,
				State: decision.State, Detail: decision.Reason, Permission: "deny",
			})
		}
		return nil
	})
	if err != nil {
		_ = appendEvent(cfg.StateDir, gateEvent{
			Event: "gate_internal_failure", Observed: cfg.Now().UTC().Format(time.RFC3339Nano),
			SessionID: hook.SessionID, HookEvent: hook.HookEventName, ToolName: hook.ToolName,
			Detail: err.Error(),
		})
		return Decision{}, nil, err
	}
	out, encErr := encodeDecision(hook.HookEventName, decision)
	return decision, out, encErr
}

func normalizeConfig(cfg Config) Config {
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir()
	}
	if cfg.MaxPlaywright <= 0 {
		cfg.MaxPlaywright = rootMaxPlaywright
	}
	if cfg.MaxScreenshot <= 0 {
		cfg.MaxScreenshot = rootMaxScreenshot
	}
	if cfg.MaxSameVerify <= 0 {
		cfg.MaxSameVerify = rootMaxSameVerify
	}
	if cfg.HighCostSoft <= 0 {
		cfg.HighCostSoft = rootHighCostSoft
	}
	if cfg.HighCostHard <= 0 {
		cfg.HighCostHard = rootHighCostHard
	}
	if cfg.MaxCompacts <= 0 {
		cfg.MaxCompacts = maxCompactsHandoff
	}
	if cfg.MaxTranscriptBytes <= 0 {
		cfg.MaxTranscriptBytes = maxTranscriptBytes
	}
	if cfg.MaxAge <= 0 {
		cfg.MaxAge = maxRootAge
	}
	if cfg.GateMaxPlaywright <= 0 {
		cfg.GateMaxPlaywright = gateMaxPlaywright
	}
	if cfg.GateMaxScreenshot <= 0 {
		cfg.GateMaxScreenshot = gateMaxScreenshot
	}
	if cfg.GateMaxSameVerify <= 0 {
		cfg.GateMaxSameVerify = gateMaxSameVerify
	}
	if cfg.GateHighCostSoft <= 0 {
		cfg.GateHighCostSoft = gateHighCostSoft
	}
	if cfg.GateHighCostHard <= 0 {
		cfg.GateHighCostHard = gateHighCostHard
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return cfg
}

func refreshLifecycle(st *state, hook HookInput, cfg Config) {
	if st.HandoffRequired {
		return
	}
	if st.CompactCount >= cfg.MaxCompacts {
		st.HandoffRequired = true
		st.HandoffReason = fmt.Sprintf("compact_count=%d exceeds budget %d", st.CompactCount, cfg.MaxCompacts)
		return
	}
	if started, err := time.Parse(time.RFC3339Nano, st.StartedAt); err == nil {
		if cfg.Now().Sub(started) >= cfg.MaxAge {
			st.HandoffRequired = true
			st.HandoffReason = fmt.Sprintf("root_age exceeds %s", cfg.MaxAge)
			return
		}
	}
	if path := strings.TrimSpace(hook.TranscriptPath); path != "" {
		st.TranscriptPath = expandHome(path)
		if info, err := os.Stat(st.TranscriptPath); err == nil && info.Size() >= cfg.MaxTranscriptBytes {
			st.HandoffRequired = true
			st.HandoffReason = fmt.Sprintf("transcript_bytes=%d exceeds budget %d", info.Size(), cfg.MaxTranscriptBytes)
		}
	}
}

func expireOverride(st *state, now time.Time) {
	if st.Override == nil {
		return
	}
	exp, err := time.Parse(time.RFC3339Nano, st.Override.ExpiresAt)
	if err != nil || !now.Before(exp) || st.Override.RemainingActions <= 0 {
		st.Override = nil
	}
}

func handleUserPrompt(st *state, hook HookInput, cfg Config) Decision {
	// Only real UserPromptSubmit may issue overrides (not model/MCP self-auth).
	if lease, ok, msg := parseGateOverride(hook.Prompt, cfg.Now()); ok {
		if st.HandoffRequired {
			return Decision{
				State:   "override_blocked_handoff",
				Context: "CLAUDEX_GATE: override refused while handoff_required; finish capsule and start a new Root.",
			}
		}
		st.Override = &lease
		_ = appendEvent(cfg.StateDir, gateEvent{
			Event: "gate_override_issued", Observed: cfg.Now().UTC().Format(time.RFC3339Nano),
			SessionID: st.SessionID, Detail: lease.Reason,
		})
		return Decision{
			State:   "override_issued",
			Context: fmt.Sprintf("CLAUDEX_GATE_OVERRIDE v1: lease active until %s, remaining_actions=%d, paths=%v. %s", lease.ExpiresAt, lease.RemainingActions, lease.PathScope, msg),
		}
	}
	return Decision{State: "user_prompt"}
}

func contextWindowTokens() int64 {
	if raw := strings.TrimSpace(os.Getenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW")); raw != "" {
		var n int64
		for _, ch := range raw {
			if ch < '0' || ch > '9' {
				return defaultContextWindow
			}
			n = n*10 + int64(ch-'0')
		}
		if n > 0 {
			return n
		}
	}
	return defaultContextWindow
}

func promptSoftHard() (soft, hard int64) {
	w := contextWindowTokens()
	return int64(float64(w) * promptTokenSoftRatio), int64(float64(w) * promptTokenHardRatio)
}

func ingestUsageHint(st *state, hook HookInput) {
	// Only trust token fields when the hook actually provided them (not all zeros by default).
	promptSide := hook.InputTokens + hook.CacheReadInputTokens + hook.CacheCreationInputTokens
	if promptSide > 0 {
		st.LatestPromptTokens = promptSide // current sample, not max-ever
		st.TokenSource = "hook"
		return
	}
	// Official Claude Code PostToolUse payloads do not include input_tokens/cache_*.
	// Fall back to sampling the latest assistant usage from the session transcript (T3 partial).
	if path := strings.TrimSpace(hook.TranscriptPath); path != "" {
		if n, ok := sampleTranscriptPromptTokens(path); ok && n > 0 {
			st.LatestPromptTokens = n
			st.TokenSource = "transcript"
		}
	}
}

func applyContextPressure(st *state) string {
	soft, hard := promptSoftHard()
	// Soft: warning only. Hard: handoff.
	if st.LatestPromptTokens >= hard || st.ToolResultBytesWindow >= toolBytesHard {
		st.ContextPressure = "hard"
		if !st.HandoffRequired {
			st.HandoffRequired = true
			st.HandoffReason = fmt.Sprintf("context hard pressure: prompt_tokens=%d (hard>=%d) tool_bytes=%d", st.LatestPromptTokens, hard, st.ToolResultBytesWindow)
		}
		return "hard"
	}
	if st.LatestPromptTokens >= soft || st.ToolResultBytesWindow >= toolBytesSoft {
		st.ContextPressure = "soft"
		return "soft"
	}
	st.ContextPressure = ""
	return ""
}

func handleStopFailure(st *state, hook HookInput, cfg Config) Decision {
	blob := strings.ToLower(hook.Error + " " + hook.ErrorDetails + " " + hook.LastAssistantMsg)
	if strings.Contains(blob, "prompt is too long") || strings.Contains(blob, "context limit") ||
		strings.Contains(blob, "context window") || strings.Contains(blob, "maximum context") {
		// Reactive latch only — not a preventive mechanism (CC ignores StopFailure stdout).
		st.OverflowLatched = true
		st.ContextPressure = "hard"
		if !st.HandoffRequired {
			st.HandoffRequired = true
			st.HandoffReason = "StopFailure: prompt/context overflow latched (reactive)"
		}
		return Decision{State: "overflow_latched"}
	}
	return Decision{State: "stop_observed"}
}

func recordTool(st *state, hook HookInput, cfg Config) Decision {
	name := hook.ToolName
	st.LastToolName = name
	// Capture write-path hints for handoff recovery (bounded).
	if p := toolPathFromInput(hook.ToolInput); p != "" && (name == "Write" || name == "Edit" || name == "MultiEdit") {
		st.PathHints = appendPathHint(st.PathHints, p, 32)
	}
	// T3: rolling tool_response bytes (bounded); not lifetime cumulative.
	if len(hook.ToolResponse) > 0 {
		st.ToolResultBytesWindow += int64(len(hook.ToolResponse))
		if st.ToolResultBytesWindow > toolBytesMaxWin {
			st.ToolResultBytesWindow = toolBytesMaxWin
		}
	}
	pressure := applyContextPressure(st)
	if isControlTool(name) {
		if pressure == "soft" {
			return Decision{State: "recorded_control", Context: contextPressureContext(*st)}
		}
		return Decision{State: "recorded_control"}
	}
	incHigh := false
	if isPlaywright(name) {
		st.PlaywrightCalls++
		if st.OpenGate != nil {
			st.OpenGate.PlaywrightCalls++
		}
		incHigh = true
	}
	if isScreenshot(name) {
		st.ScreenshotCalls++
		if st.OpenGate != nil {
			st.OpenGate.ScreenshotCalls++
		}
		incHigh = true
	}
	if name == "Bash" {
		switch bashClass(hook.ToolInput) {
		case "test":
			st.TestCalls++
			incHigh = true
		case "deploy":
			st.DeployCalls++
			incHigh = true
		case "mutate":
			incHigh = true
		}
	} else if isHighCost(name) && !isPlaywright(name) && !isScreenshot(name) {
		incHigh = true
	}
	if incHigh {
		st.HighCostCalls++
		if st.OpenGate != nil {
			st.OpenGate.HighCostCalls++
		}
	}
	if key := verifyKey(name, hook.ToolInput); key != "" {
		st.VerifyCounts[key]++
		if st.OpenGate != nil {
			if st.OpenGate.VerifyCounts == nil {
				st.OpenGate.VerifyCounts = map[string]int{}
			}
			st.OpenGate.VerifyCounts[key]++
		}
		st.LastVerifyKey = key
	}
	// Arm sticky when Root or gate soft high-cost crossed.
	rootSoft := st.HighCostCalls >= cfg.HighCostSoft
	gateSoft := st.OpenGate != nil && st.OpenGate.HighCostCalls >= cfg.GateHighCostSoft
	if (rootSoft || gateSoft) && !st.StickyReroutePending {
		st.StickyReroutePending = true
		st.StickyReason = fmt.Sprintf("high_cost root=%d gate=%d", st.HighCostCalls, gateHigh(*st))
	}
	refreshLifecycle(st, hook, cfg)
	return Decision{State: "recorded"}
}

func evaluatePre(st *state, hook HookInput, cfg Config) Decision {
	name := hook.ToolName
	refreshLifecycle(st, hook, cfg)
	expireOverride(st, cfg.Now())

	// Control tools always allowed (scheduler must not lock itself out).
	if isControlTool(name) {
		return Decision{Permission: "allow", State: "control_allow"}
	}

	// T5: opt-in workflow strict mode denies native Agent fan-out.
	if os.Getenv("CLAUDEX_WORKFLOW_STRICT") == "1" && name == "Agent" {
		return Decision{
			Permission: "deny",
			Reason:      "CLAUDEX_WORKFLOW_STRICT: native Agent denied; use mcp__claudex-flow__start_worker or explore_repository",
			Context:    "CLAUDEX_STRICT: prefer MCP Worker/specialist lanes for unattended canaries.",
			State:      "strict_agent_deny",
		}
	}

	// User override lease (construction tools only, path-scoped).
	if st.Override != nil && !st.HandoffRequired && overrideCovers(st.Override, name, hook.ToolInput) {
		st.Override.RemainingActions--
		if st.Override.RemainingActions <= 0 {
			st.Override = nil
		}
		return Decision{Permission: "allow", State: "override_allow", Context: "CLAUDEX_GATE_OVERRIDE: consumed one lease action."}
	}

	if st.HandoffRequired || st.OverflowLatched {
		if blocked, why := handoffBlocks(name, hook.ToolInput); blocked {
			return Decision{
				Permission: "deny",
				Reason:     "CLAUDEX_ROOT_HANDOFF_REQUIRED: " + st.HandoffReason + "; " + why,
				Context:    lifecycleContext(*st),
				State:      "handoff_deny",
			}
		}
		return Decision{Permission: "allow", Context: lifecycleContext(*st), State: "handoff_allow_readonly"}
	}

	// Destructive git always denied on normal Roots unless user override covers it
	// (override requires explicit path scope and does not cover bare git force).
	if name == "Bash" && bashIsDestructiveGit(hook.ToolInput) {
		return Decision{
			Permission: "deny",
			Reason:     "CLAUDEX_GIT_DESTRUCTIVE: git reset --hard / push --force blocked; use explicit user approval outside automated thrash",
			State:      "git_destructive_deny",
		}
	}

	// T11: deploy/test bash budgets at Root.
	if name == "Bash" {
		switch bashClass(hook.ToolInput) {
		case "deploy":
			if st.DeployCalls >= rootMaxDeploy {
				return Decision{
					Permission: "deny",
					Reason:     fmt.Sprintf("CLAUDEX_DEPLOY_BUDGET: %d deploys already used (cap %d)", st.DeployCalls, rootMaxDeploy),
					State:      "deploy_budget_deny",
				}
			}
		case "test":
			if st.TestCalls >= rootMaxTestBash {
				return Decision{
					Permission: "deny",
					Reason:     fmt.Sprintf("CLAUDEX_TEST_BASH_BUDGET: %d test bash calls (cap %d)", st.TestCalls, rootMaxTestBash),
					State:      "test_budget_deny",
				}
			}
		}
	}

	// Sticky re-route: deny further high-cost until ack_reroute.
	if st.StickyReroutePending && isHighCost(name) {
		return Decision{
			Permission: "deny",
			Reason:      "CLAUDEX_STICKY_REROUTE: call mcp__claudex-flow__ack_reroute with open gate_id, remaining_acceptance, and worker_decision before more high-cost tools",
			Context:    stickyContext(*st),
			State:      "sticky_deny",
		}
	}
	if st.StickyReroutePending && name == "Bash" {
		switch bashClass(hook.ToolInput) {
		case "test", "deploy", "mutate":
			return Decision{
				Permission: "deny",
				Reason:     "CLAUDEX_STICKY_REROUTE: mutating/test/deploy Bash blocked until ack_reroute",
				Context:    stickyContext(*st),
				State:      "sticky_deny",
			}
		}
	}

	// Same verification fingerprint (Root + gate).
	if key := verifyKey(name, hook.ToolInput); key != "" {
		rootN := st.VerifyCounts[key]
		gateN := 0
		if st.OpenGate != nil && st.OpenGate.VerifyCounts != nil {
			gateN = st.OpenGate.VerifyCounts[key]
		}
		if rootN >= cfg.MaxSameVerify || gateN >= cfg.GateMaxSameVerify {
			return Decision{
				Permission: "deny",
				Reason:     fmt.Sprintf("CLAUDEX_VERIFY_BUDGET: fingerprint ran root=%d gate=%d (caps %d/%d)", rootN, gateN, cfg.MaxSameVerify, cfg.GateMaxSameVerify),
				Context:    "CLAUDEX_GATE: stop repeating the same check. Change one variable or state a new hypothesis.",
				State:      "verify_budget_deny",
			}
		}
	}

	if isPlaywright(name) {
		if st.PlaywrightCalls >= cfg.MaxPlaywright || (st.OpenGate != nil && st.OpenGate.PlaywrightCalls >= cfg.GateMaxPlaywright) {
			return Decision{
				Permission: "deny",
				Reason:     fmt.Sprintf("CLAUDEX_PLAYWRIGHT_BUDGET: root=%d/%d gate=%d/%d", st.PlaywrightCalls, cfg.MaxPlaywright, gatePW(*st), cfg.GateMaxPlaywright),
				Context:    "CLAUDEX_GATE: Playwright budget exhausted (Root and/or current gate).",
				State:      "playwright_budget_deny",
			}
		}
	}
	if isScreenshot(name) {
		if st.ScreenshotCalls >= cfg.MaxScreenshot || (st.OpenGate != nil && st.OpenGate.ScreenshotCalls >= cfg.GateMaxScreenshot) {
			return Decision{
				Permission: "deny",
				Reason:     fmt.Sprintf("CLAUDEX_SCREENSHOT_BUDGET: root=%d/%d gate=%d/%d", st.ScreenshotCalls, cfg.MaxScreenshot, gateShot(*st), cfg.GateMaxScreenshot),
				Context:    "CLAUDEX_GATE: screenshot budget exhausted.",
				State:      "screenshot_budget_deny",
			}
		}
	}
	if isHighCost(name) {
		if st.HighCostCalls >= cfg.HighCostHard || (st.OpenGate != nil && st.OpenGate.HighCostCalls >= cfg.GateHighCostHard) {
			return Decision{
				Permission: "deny",
				Reason:     fmt.Sprintf("CLAUDEX_HIGH_COST_BUDGET: root=%d/%d gate=%d/%d", st.HighCostCalls, cfg.HighCostHard, gateHigh(*st), cfg.GateHighCostHard),
				Context:    stickyContext(*st),
				State:      "high_cost_budget_deny",
			}
		}
	}

	if st.ContextPressure == "soft" {
		return Decision{Permission: "allow", Context: contextPressureContext(*st), State: "context_soft"}
	}
	if st.ContextPressure == "hard" && (isPlaywright(name) || isScreenshot(name) || name == "Bash") {
		return Decision{
			Permission: "deny",
			Reason:     "CLAUDEX_CONTEXT_HARD: large-output tools blocked; compact or hand off",
			Context:    contextPressureContext(*st),
			State:      "context_hard_deny",
		}
	}

	return Decision{Permission: "allow", State: "allow"}
}

func contextPressureContext(st state) string {
	soft, hard := promptSoftHard()
	return fmt.Sprintf("CLAUDEX_CONTEXT_PRESSURE v1 level=%s prompt_tokens=%d soft>=%d hard>=%d tool_bytes_window=%d. Soft is warning only; hard latches handoff / blocks large-output tools. PostCompact resets samples. StopFailure latch is reactive only.", st.ContextPressure, st.LatestPromptTokens, soft, hard, st.ToolResultBytesWindow)
}

func encodeDecision(event string, d Decision) ([]byte, error) {
	if event == "PreToolUse" {
		if d.Permission != "deny" && d.Context == "" {
			return nil, nil
		}
		out := specificOutput{HookEventName: event, AdditionalContext: d.Context}
		if d.Permission != "" {
			out.PermissionDecision = d.Permission
			out.PermissionDecisionReason = d.Reason
		}
		return json.Marshal(hookOutput{HookSpecificOutput: out})
	}
	if event == "UserPromptSubmit" && d.Context != "" {
		return json.Marshal(hookOutput{HookSpecificOutput: specificOutput{
			HookEventName: event, AdditionalContext: d.Context,
		}})
	}
	// PostCompact has no decision/context control — never emit.
	if d.Context == "" || event == "PostCompact" || event == "PreCompact" {
		return nil, nil
	}
	return json.Marshal(hookOutput{HookSpecificOutput: specificOutput{
		HookEventName: event, AdditionalContext: d.Context,
	}})
}

func isPlaywright(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "playwright") || strings.Contains(n, "browser_")
}

func isScreenshot(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "screenshot") || strings.Contains(n, "take_screenshot")
}

func isHighCost(name string) bool {
	if isPlaywright(name) || isScreenshot(name) {
		return true
	}
	switch name {
	case "Agent", "Task", "TaskCreate", "TaskUpdate", "TaskOutput", "Skill", "Write", "Edit":
		return true
	}
	if strings.HasPrefix(name, "mcp__claudex-flow__start_worker") {
		return true
	}
	if strings.HasPrefix(name, "mcp__plugin_playwright") {
		return true
	}
	return false
}

// Control / scheduler tools must remain usable under sticky and soft blocks.
func isControlTool(name string) bool {
	switch name {
	case "Read", "Grep", "Glob", "TaskList", "TaskGet":
		return true
	}
	if !strings.HasPrefix(name, "mcp__claudex-flow__") {
		return false
	}
	switch {
	case strings.HasSuffix(name, "__declare_gate"),
		strings.HasSuffix(name, "__close_gate"),
		strings.HasSuffix(name, "__ack_reroute"),
		strings.HasSuffix(name, "__gate_status"),
		strings.HasSuffix(name, "__workflow_status"),
		strings.HasSuffix(name, "__runtime_contract"),
		strings.HasSuffix(name, "__find_thread"),
		strings.HasSuffix(name, "__read_thread"),
		strings.HasSuffix(name, "__route_task"),
		strings.HasSuffix(name, "__record_route_outcome"):
		return true
	default:
		return false
	}
}

// handoffBlocks is fail-closed: unknown tools are blocked. Only an explicit
// read/control allowlist may proceed during Root handoff.
func handoffBlocks(name string, raw json.RawMessage) (bool, string) {
	// Explicit allowlist only (no default-allow for unknown MCP).
	switch name {
	case "Read", "Grep", "Glob":
		return false, ""
	}
	if isHandoffControlTool(name) {
		return false, ""
	}
	// Bash is fully denied during handoff: string allowlists are bypassable
	// (e.g. pwd & touch, process substitution). Use Read/Grep instead.
	if name == "Bash" {
		return true, "Bash is fully denied during handoff (bypass-resistant); use Read/Grep"
	}
	if isPlaywright(name) || isScreenshot(name) {
		return true, "browser/screenshot tools are blocked during handoff"
	}
	if strings.HasPrefix(name, "mcp__claudex-flow__start_worker") || strings.HasPrefix(name, "mcp__claudex-flow__resume_worker") {
		return true, "Worker tools are blocked during handoff"
	}
	return true, "unknown or construction tool denied during handoff (fail-closed allowlist)"
}

// isHandoffControlTool is the narrow MCP allowlist usable while handoff_required.
func isHandoffControlTool(name string) bool {
	if !strings.HasPrefix(name, "mcp__claudex-flow__") {
		return false
	}
	switch {
	case strings.HasSuffix(name, "__declare_gate"),
		strings.HasSuffix(name, "__close_gate"),
		strings.HasSuffix(name, "__ack_reroute"),
		strings.HasSuffix(name, "__gate_status"),
		strings.HasSuffix(name, "__workflow_status"),
		strings.HasSuffix(name, "__runtime_contract"),
		strings.HasSuffix(name, "__find_thread"),
		strings.HasSuffix(name, "__read_thread"):
		return true
	default:
		return false
	}
}

func verifyKey(name string, raw json.RawMessage) string {
	if name == "" {
		return ""
	}
	if !(name == "Bash" || isPlaywright(name) || isScreenshot(name) || name == "TaskGet") {
		return ""
	}
	body := strings.TrimSpace(string(raw))
	if body == "" || body == "null" {
		return name
	}
	if len(body) > 240 {
		body = body[:240]
	}
	sum := sha256.Sum256([]byte(name + "\x00" + body))
	return name + ":" + hex.EncodeToString(sum[:8])
}

func stickyContext(st state) string {
	gate := ""
	if st.OpenGate != nil {
		gate = st.OpenGate.GateID
	}
	return fmt.Sprintf("CLAUDEX_STICKY_REROUTE v1: pending=%v reason=%q open_gate=%q root_high_cost=%d. Call mcp__claudex-flow__ack_reroute with gate_id, remaining_acceptance[], worker_decision (none|start|resume), optional hypothesis_change. Control tools (declare_gate/close_gate/ack_reroute/gate_status/Read/Grep) remain allowed.", st.StickyReroutePending, st.StickyReason, gate, st.HighCostCalls)
}

func lifecycleContext(st state) string {
	reason := st.HandoffReason
	if reason == "" {
		reason = "root lifecycle budget reached"
	}
	paths := ""
	if st.HandoffMDPath != "" {
		paths = fmt.Sprintf(" capsule_md=%s capsule_json=%s", st.HandoffMDPath, st.HandoffJSONPath)
	}
	return fmt.Sprintf("CLAUDEX_ROOT_HANDOFF_REQUIRED v1: %s. compact_count=%d.%s Stop construction tools. User starts a new Root with claudex --from-handoff (explicit only). Use Read/Grep/find_thread/read_thread only (Bash denied during handoff). Enforcement is PreToolUse.", reason, st.CompactCount, paths)
}

func gateHigh(st state) int {
	if st.OpenGate == nil {
		return 0
	}
	return st.OpenGate.HighCostCalls
}
func gatePW(st state) int {
	if st.OpenGate == nil {
		return 0
	}
	return st.OpenGate.PlaywrightCalls
}
func gateShot(st state) int {
	if st.OpenGate == nil {
		return 0
	}
	return st.OpenGate.ScreenshotCalls
}

func overrideCovers(lease *OverrideLease, tool string, raw json.RawMessage) bool {
	if lease == nil {
		return false
	}
	class := overrideToolClass(tool, raw)
	if class == "" {
		return false
	}
	okClass := false
	for _, c := range lease.ToolClasses {
		if strings.EqualFold(c, class) {
			okClass = true
			break
		}
	}
	if !okClass {
		return false
	}
	path := toolPathFromInput(raw)
	if path == "" {
		// Bash without path: only if class is Test/Deploy and path_scope empty not allowed — require paths always.
		return false
	}
	for _, p := range lease.PathScope {
		p = strings.TrimSpace(p)
		if p != "" && (path == p || strings.HasPrefix(path, strings.TrimSuffix(p, "/")+"/")) {
			return true
		}
	}
	return false
}

func overrideToolClass(tool string, raw json.RawMessage) string {
	switch tool {
	case "Edit", "Write":
		return "Edit"
	case "Bash":
		switch bashClass(raw) {
		case "test":
			return "Test"
		case "deploy":
			return "Deploy"
		default:
			return ""
		}
	default:
		return ""
	}
}

// parseGateOverride parses: /gate-override reason=... paths=a,b [classes=Edit,Test]
func parseGateOverride(prompt string, now time.Time) (OverrideLease, bool, string) {
	p := strings.TrimSpace(prompt)
	if !strings.HasPrefix(p, "/gate-override") && !strings.Contains(p, "\n/gate-override") {
		// allow leading noise then command
		idx := strings.Index(p, "/gate-override")
		if idx < 0 {
			return OverrideLease{}, false, ""
		}
		p = strings.TrimSpace(p[idx:])
	} else if !strings.HasPrefix(p, "/gate-override") {
		idx := strings.Index(p, "/gate-override")
		p = strings.TrimSpace(p[idx:])
	}
	if !strings.HasPrefix(p, "/gate-override") {
		return OverrideLease{}, false, ""
	}
	rest := strings.TrimSpace(strings.TrimPrefix(p, "/gate-override"))
	fields := map[string]string{}
	for _, part := range strings.Fields(rest) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			fields[strings.ToLower(kv[0])] = kv[1]
		}
	}
	reason := strings.TrimSpace(fields["reason"])
	pathsRaw := strings.TrimSpace(fields["paths"])
	if reason == "" || pathsRaw == "" {
		return OverrideLease{}, false, ""
	}
	var paths []string
	for _, pth := range strings.Split(pathsRaw, ",") {
		pth = strings.TrimSpace(pth)
		if pth != "" {
			paths = append(paths, pth)
		}
	}
	if len(paths) == 0 {
		return OverrideLease{}, false, ""
	}
	classes := []string{"Edit", "Test", "Deploy"}
	if c := strings.TrimSpace(fields["classes"]); c != "" {
		classes = nil
		for _, x := range strings.Split(c, ",") {
			x = strings.TrimSpace(x)
			if x != "" {
				classes = append(classes, x)
			}
		}
	}
	lease := OverrideLease{
		ExpiresAt:        now.Add(overrideTTL).UTC().Format(time.RFC3339Nano),
		RemainingActions: overrideMaxActs,
		ToolClasses:      classes,
		PathScope:        paths,
		Reason:           reason,
		IssuedAt:         now.UTC().Format(time.RFC3339Nano),
	}
	return lease, true, "path_scope required; max 3 actions / 10 minutes"
}
