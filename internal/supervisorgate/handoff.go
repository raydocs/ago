package supervisorgate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HandoffCapsule is the T2 recovery artifact written atomically.
type HandoffCapsule struct {
	Schema          string         `json:"schema"`
	FromRootSession string         `json:"from_root_session_id"`
	WrittenAt       string         `json:"written_at"`
	Objective       string         `json:"objective,omitempty"`
	CurrentGate     *GateRecord    `json:"current_gate,omitempty"`
	Acceptance      []string       `json:"acceptance,omitempty"`
	CompactCount    int            `json:"compact_count"`
	PlaywrightCalls int            `json:"playwright_calls"`
	ScreenshotCalls int            `json:"screenshot_calls"`
	HighCostCalls   int            `json:"high_cost_calls"`
	StickyPending   bool           `json:"sticky_reroute_pending"`
	HandoffReason   string         `json:"handoff_reason"`
	OverflowLatched bool           `json:"overflow_latched,omitempty"`
	LatestPromptTok int64          `json:"latest_prompt_tokens,omitempty"`
	TokenSource     string         `json:"token_source,omitempty"`
	ToolResultBytes int64          `json:"tool_result_bytes_window,omitempty"`
	NextAction      string         `json:"next_action"`
	PathsHint       []string       `json:"path_hints,omitempty"`
	TranscriptPath  string         `json:"transcript_path,omitempty"`
	Verification    []string       `json:"verification_hints,omitempty"`
	WorkerIDs       []string       `json:"worker_ids,omitempty"`
	WorkflowSnapshot map[string]any `json:"workflow_snapshot,omitempty"`
	GateCounters    map[string]int `json:"gate_counters,omitempty"`
	ResidualRisks   []string       `json:"residual_risks,omitempty"`
	JSONPath        string         `json:"json_path,omitempty"`
	MarkdownPath    string         `json:"markdown_path,omitempty"`
}

func DefaultHandoffDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claudex", "handoffs")
}

// writeHandoffCapsule writes .json + .md under handoff dir. Safe to call under session lock.
func writeHandoffCapsule(st state, handoffDir string, now time.Time) (HandoffCapsule, error) {
	if handoffDir == "" {
		handoffDir = DefaultHandoffDir()
	}
	if err := os.MkdirAll(handoffDir, 0o700); err != nil {
		return HandoffCapsule{}, err
	}
	cap := HandoffCapsule{
		Schema:          "claudex-handoff.v1.4.5",
		FromRootSession: st.SessionID,
		WrittenAt:       now.UTC().Format(time.RFC3339Nano),
		CurrentGate:     st.OpenGate,
		CompactCount:    st.CompactCount,
		PlaywrightCalls: st.PlaywrightCalls,
		ScreenshotCalls: st.ScreenshotCalls,
		HighCostCalls:   st.HighCostCalls,
		StickyPending:   st.StickyReroutePending,
		HandoffReason:   st.HandoffReason,
		OverflowLatched: st.OverflowLatched,
		LatestPromptTok: st.LatestPromptTokens,
		TokenSource:     st.TokenSource,
		ToolResultBytes: st.ToolResultBytesWindow,
		TranscriptPath:  st.TranscriptPath,
		PathsHint:       append([]string(nil), st.PathHints...),
		NextAction:      "Start a new Root with: claudex --from-handoff <markdown_path> (user explicit only; never combine with --resume/--continue; hooks never spawn Claude).",
		ResidualRisks: []string{
			"Capsule is zero-model; re-anchor from disk before trusting narrative fields.",
			"Token samples may be partial (transcript tail) until gateway accounting is wired.",
		},
		GateCounters: map[string]int{
			"root_playwright": st.PlaywrightCalls,
			"root_high_cost":  st.HighCostCalls,
			"compacts":        st.CompactCount,
			"deploy_calls":    st.DeployCalls,
			"test_calls":      st.TestCalls,
		},
		WorkflowSnapshot: map[string]any{
			"sticky_reroute_pending": st.StickyReroutePending,
			"sticky_reason":          st.StickyReason,
			"handoff_required":       st.HandoffRequired,
			"context_pressure":       st.ContextPressure,
			"overflow_latched":       st.OverflowLatched,
			"used_gate_ids":          append([]string(nil), st.UsedGateIDs...),
			"last_tool_name":         st.LastToolName,
			"last_verify_key":        st.LastVerifyKey,
		},
	}
	if st.OpenGate != nil {
		cap.Acceptance = append([]string{}, st.OpenGate.Acceptance...)
		cap.Verification = append([]string{}, st.OpenGate.Acceptance...)
		if st.OpenGate.StopCondition != "" {
			cap.Verification = append(cap.Verification, "stop:"+st.OpenGate.StopCondition)
		}
	}
	// Worker IDs are not owned by the gate process; leave empty unless gate history encodes them.
	base := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, st.SessionID)
	if base == "" {
		base = "unknown"
	}
	jsonPath := filepath.Join(handoffDir, base+".json")
	mdPath := filepath.Join(handoffDir, base+".md")
	cap.JSONPath = jsonPath
	cap.MarkdownPath = mdPath

	raw, err := json.MarshalIndent(cap, "", "  ")
	if err != nil {
		return HandoffCapsule{}, err
	}
	if err := atomicWrite(jsonPath, append(raw, '\n')); err != nil {
		return HandoffCapsule{}, err
	}
	md := formatHandoffMarkdown(cap)
	if err := atomicWrite(mdPath, []byte(md)); err != nil {
		return HandoffCapsule{}, err
	}
	return cap, nil
}

func formatHandoffMarkdown(c HandoffCapsule) string {
	var b strings.Builder
	b.WriteString("# Claude X Root handoff capsule\n\n")
	b.WriteString(fmt.Sprintf("- schema: `%s`\n", c.Schema))
	b.WriteString(fmt.Sprintf("- from_root: `%s`\n", c.FromRootSession))
	b.WriteString(fmt.Sprintf("- written_at: `%s`\n", c.WrittenAt))
	b.WriteString(fmt.Sprintf("- handoff_reason: %s\n", c.HandoffReason))
	b.WriteString(fmt.Sprintf("- compact_count: %d\n", c.CompactCount))
	b.WriteString(fmt.Sprintf("- root_high_cost: %d playwright: %d screenshots: %d\n", c.HighCostCalls, c.PlaywrightCalls, c.ScreenshotCalls))
	b.WriteString(fmt.Sprintf("- sticky_pending: %v overflow_latched: %v\n", c.StickyPending, c.OverflowLatched))
	if c.LatestPromptTok > 0 {
		b.WriteString(fmt.Sprintf("- latest_prompt_tokens: %d (source=%s)\n", c.LatestPromptTok, c.TokenSource))
	}
	if c.TranscriptPath != "" {
		b.WriteString(fmt.Sprintf("- transcript_path: `%s`\n", c.TranscriptPath))
	}
	if len(c.PathsHint) > 0 {
		b.WriteString("- path_hints:\n")
		for _, p := range c.PathsHint {
			b.WriteString(fmt.Sprintf("  - `%s`\n", p))
		}
	}
	if len(c.Verification) > 0 {
		b.WriteString("- verification_hints:\n")
		for _, v := range c.Verification {
			b.WriteString(fmt.Sprintf("  - %s\n", v))
		}
	}
	if c.CurrentGate != nil {
		b.WriteString(fmt.Sprintf("- open_gate: `%s` status=%s\n", c.CurrentGate.GateID, c.CurrentGate.Status))
		for _, a := range c.CurrentGate.Acceptance {
			b.WriteString(fmt.Sprintf("  - acceptance: %s\n", a))
		}
	}
	b.WriteString("\n## Next action\n\n")
	b.WriteString(c.NextAction + "\n\n")
	b.WriteString("## Recovery rules\n\n")
	b.WriteString("1. Do not resume construction on this Root (`--resume` / `--continue` forbidden with `--from-handoff`).\n")
	b.WriteString("2. Re-anchor from current files, transcript_path, and path_hints before trusting narrative.\n")
	b.WriteString("3. User starts a new Root explicitly; hooks never spawn Claude.\n")
	return b.String()
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".handoff-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
