package supervisorgate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	stateFileMode = 0o600
	stateDirMode  = 0o700
	schemaVersion = 2
)

// Root-level budgets (never reset by declare_gate).
const (
	rootMaxPlaywright  = 12
	rootMaxScreenshot  = 8
	rootMaxSameVerify  = 3
	rootHighCostSoft   = 8
	rootHighCostHard   = 24
	maxCompactsHandoff = 3
	maxTranscriptBytes = 8 * 1024 * 1024
	maxRootAge         = 4 * time.Hour
	maxGatesPerRoot    = 8
	rootMaxDeploy      = 3
	rootMaxTestBash    = 12
	// Context pressure uses window-relative ratios against CLAUDE_CODE_AUTO_COMPACT_WINDOW
	// (default 272000). Soft ≈ 78%, hard ≈ 90% of that window.
	defaultContextWindow = int64(272_000)
	promptTokenSoftRatio = 0.78
	promptTokenHardRatio = 0.90
	// Rolling tool-result byte window (not lifetime cumulative).
	toolBytesSoft   = int64(512 * 1024)
	toolBytesHard   = int64(2 * 1024 * 1024)
	toolBytesMaxWin = int64(4 * 1024 * 1024)
)

// Per-gate budgets (reset only when a new gate opens).
const (
	gateMaxPlaywright = 4
	gateMaxScreenshot = 3
	gateMaxSameVerify = 3
	gateHighCostSoft  = 4
	gateHighCostHard  = 8
)

// Override lease defaults (UserPromptSubmit /gate-override only).
const (
	overrideTTL     = 10 * time.Minute
	overrideMaxActs = 3
)

type GateRecord struct {
	GateID          string         `json:"gate_id"`
	Acceptance      []string       `json:"acceptance,omitempty"`
	StopCondition   string         `json:"stop_condition,omitempty"`
	AcceptanceHash  string         `json:"acceptance_hash,omitempty"`
	Status          string         `json:"status"` // open | closed | abandoned
	OpenedAt        string         `json:"opened_at,omitempty"`
	ClosedAt        string         `json:"closed_at,omitempty"`
	PlaywrightCalls int            `json:"playwright_calls"`
	ScreenshotCalls int            `json:"screenshot_calls"`
	HighCostCalls   int            `json:"high_cost_calls"`
	VerifyCounts    map[string]int `json:"verify_counts,omitempty"`
}

type OverrideLease struct {
	ExpiresAt        string   `json:"expires_at"`
	RemainingActions int      `json:"remaining_actions"`
	ToolClasses      []string `json:"tool_classes,omitempty"`
	PathScope        []string `json:"path_scope,omitempty"`
	Reason           string   `json:"reason,omitempty"`
	IssuedAt         string   `json:"issued_at,omitempty"`
}

type state struct {
	SessionID     string `json:"session_id"`
	StartedAt     string `json:"started_at"`
	UpdatedAt     string `json:"updated_at"`
	SchemaVersion int    `json:"schema_version"`

	// Root totals (never reset on declare_gate).
	CompactCount    int            `json:"compact_count"`
	PlaywrightCalls int            `json:"playwright_calls"`
	ScreenshotCalls int            `json:"screenshot_calls"`
	HighCostCalls   int            `json:"high_cost_calls"`
	VerifyCounts    map[string]int `json:"verify_counts"`

	// Sticky re-route (T1 / v1.4.2).
	StickyReroutePending bool   `json:"sticky_reroute_pending"`
	StickyReason         string `json:"sticky_reason,omitempty"`

	OpenGate    *GateRecord  `json:"open_gate,omitempty"`
	GateHistory []GateRecord `json:"gate_history,omitempty"`
	UsedGateIDs []string     `json:"used_gate_ids,omitempty"`

	Override *OverrideLease `json:"override,omitempty"`

	HandoffRequired bool   `json:"handoff_required"`
	HandoffReason   string `json:"handoff_reason,omitempty"`
	HandoffJSONPath string `json:"handoff_json_path,omitempty"`
	HandoffMDPath   string `json:"handoff_md_path,omitempty"`
	LastToolName    string `json:"last_tool_name,omitempty"`
	LastVerifyKey   string `json:"last_verify_key,omitempty"`

	// T3 context telemetry / latch (current samples; reset/decay on PostCompact).
	LatestPromptTokens    int64  `json:"latest_prompt_tokens,omitempty"` // last sample, not max-ever
	ToolResultBytesWindow int64  `json:"tool_result_bytes_window,omitempty"`
	ContextPressure       string `json:"context_pressure,omitempty"` // "" | soft | hard
	OverflowLatched       bool   `json:"overflow_latched,omitempty"`
	DeployCalls           int    `json:"deploy_calls,omitempty"`
	TestCalls             int    `json:"test_calls,omitempty"`
}

type gateEvent struct {
	Event      string `json:"event"`
	Observed   string `json:"observed_at"`
	SessionID  string `json:"session_id,omitempty"`
	HookEvent  string `json:"hook_event,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	State      string `json:"state,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Permission string `json:"permission,omitempty"`
}

func DefaultStateDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claudex", "supervisor-gate")
}

func withSessionLock(dir, sessionID string, fn func() error) error {
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return err
	}
	lockPath := statePath(dir, sessionID) + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, stateFileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("session lock: %w", err)
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn()
}

func loadStateUnlocked(dir, sessionID string) (state, error) {
	path := statePath(dir, sessionID)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return state{}, nil
		}
		return state{}, err
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return state{}, err
	}
	if st.VerifyCounts == nil {
		st.VerifyCounts = map[string]int{}
	}
	if st.OpenGate != nil && st.OpenGate.VerifyCounts == nil {
		st.OpenGate.VerifyCounts = map[string]int{}
	}
	return st, nil
}

func saveStateUnlocked(dir string, st state) error {
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return err
	}
	st.SchemaVersion = schemaVersion
	path := statePath(dir, st.SessionID)
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".gate-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(stateFileMode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func appendEvent(dir string, event gateEvent) error {
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		return err
	}
	path := filepath.Join(dir, "events.jsonl")
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, stateFileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

func statePath(dir, sessionID string) string {
	safe := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, sessionID)
	if safe == "" {
		safe = "unknown"
	}
	return filepath.Join(dir, safe+".json")
}

func newRootState(sessionID string, now time.Time) state {
	return state{
		SessionID:     sessionID,
		StartedAt:     now.UTC().Format(time.RFC3339Nano),
		SchemaVersion: schemaVersion,
		VerifyCounts:  map[string]int{},
	}
}

func gateIDUsed(st state, id string) bool {
	for _, g := range st.UsedGateIDs {
		if g == id {
			return true
		}
	}
	return false
}

func lastClosedHash(st state) string {
	for i := len(st.GateHistory) - 1; i >= 0; i-- {
		if st.GateHistory[i].AcceptanceHash != "" {
			return st.GateHistory[i].AcceptanceHash
		}
	}
	return ""
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}
