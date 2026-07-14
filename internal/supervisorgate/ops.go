package supervisorgate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var gateIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// DeclareGateInput opens one gate with frozen acceptance (MCP zero-model).
type DeclareGateInput struct {
	SessionID     string   `json:"session_id,omitempty"`
	GateID        string   `json:"gate_id"`
	Acceptance    []string `json:"acceptance"`
	StopCondition string   `json:"stop_condition,omitempty"`
}

// CloseGateInput closes or abandons the open gate.
type CloseGateInput struct {
	SessionID string `json:"session_id,omitempty"`
	GateID    string `json:"gate_id"`
	Status    string `json:"status,omitempty"` // closed | abandoned
}

// AckRerouteInput clears sticky re-route after restating gate state.
type AckRerouteInput struct {
	SessionID            string   `json:"session_id,omitempty"`
	GateID               string   `json:"gate_id"`
	RemainingAcceptance  []string `json:"remaining_acceptance"`
	WorkerDecision       string   `json:"worker_decision"` // none | start | resume
	HypothesisChange     string   `json:"hypothesis_change,omitempty"`
}

// Status is a read-only snapshot.
type Status struct {
	SessionID            string         `json:"session_id"`
	StartedAt            string         `json:"started_at,omitempty"`
	UpdatedAt            string         `json:"updated_at,omitempty"`
	CompactCount         int            `json:"compact_count"`
	PlaywrightCalls      int            `json:"playwright_calls"`
	ScreenshotCalls      int            `json:"screenshot_calls"`
	HighCostCalls        int            `json:"high_cost_calls"`
	VerifyCounts         map[string]int `json:"verify_counts,omitempty"`
	HandoffRequired      bool           `json:"handoff_required"`
	HandoffReason        string         `json:"handoff_reason,omitempty"`
	StickyReroutePending bool           `json:"sticky_reroute_pending"`
	StickyReason         string         `json:"sticky_reason,omitempty"`
	OpenGate             *GateRecord    `json:"open_gate,omitempty"`
	GateCount            int            `json:"gate_count"`
	Override             *OverrideLease `json:"override,omitempty"`
	HandoffJSONPath      string         `json:"handoff_json_path,omitempty"`
	HandoffMDPath        string         `json:"handoff_md_path,omitempty"`
	OverflowLatched       bool   `json:"overflow_latched,omitempty"`
	LatestPromptTokens    int64  `json:"latest_prompt_tokens,omitempty"`
	ToolResultBytesWindow int64  `json:"tool_result_bytes_window,omitempty"`
	ContextPressure       string `json:"context_pressure,omitempty"`
	DeployCalls           int    `json:"deploy_calls,omitempty"`
	TestCalls             int    `json:"test_calls,omitempty"`
	StatePath            string         `json:"state_path"`
	Exists               bool           `json:"exists"`
	RootBudgets          map[string]int `json:"root_budgets"`
	GateBudgets          map[string]int `json:"gate_budgets"`
}

// DeclareGate opens a new gate; resets gate-local counters only.
func DeclareGate(stateDir string, in DeclareGateInput, now time.Time) (Status, error) {
	if now.IsZero() {
		now = time.Now()
	}
	sessionID := strings.TrimSpace(in.SessionID)
	gateID := strings.TrimSpace(in.GateID)
	if sessionID == "" {
		return Status{}, fmt.Errorf("session_id is required")
	}
	if !gateIDPattern.MatchString(gateID) {
		return Status{}, fmt.Errorf("gate_id must be 1-64 chars [A-Za-z0-9._-]")
	}
	acceptance := cleanStrings(in.Acceptance)
	if len(acceptance) == 0 {
		return Status{}, fmt.Errorf("acceptance requires at least one criterion")
	}
	hash := acceptanceHash(acceptance, strings.TrimSpace(in.StopCondition))
	dir := stateDirOrDefault(stateDir)

	var out Status
	err := withSessionLock(dir, sessionID, func() error {
		st, err := loadOrInit(dir, sessionID, now)
		if err != nil {
			return err
		}
		if st.OpenGate != nil && st.OpenGate.Status == "open" {
			return fmt.Errorf("gate %q is still open; close_gate or abandon it first", st.OpenGate.GateID)
		}
		if gateIDUsed(st, gateID) {
			return fmt.Errorf("gate_id %q already used in this Root", gateID)
		}
		if len(st.UsedGateIDs) >= maxGatesPerRoot {
			return fmt.Errorf("Root gate budget exhausted: max %d gates", maxGatesPerRoot)
		}
		if prev := lastClosedHash(st); prev != "" && prev == hash {
			return fmt.Errorf("acceptance hash matches previous gate; change acceptance or stop_condition")
		}
		rec := GateRecord{
			GateID:         gateID,
			Acceptance:     acceptance,
			StopCondition:  strings.TrimSpace(in.StopCondition),
			AcceptanceHash: hash,
			Status:         "open",
			OpenedAt:       now.UTC().Format(time.RFC3339Nano),
			VerifyCounts:   map[string]int{},
		}
		st.OpenGate = &rec
		st.UsedGateIDs = append(st.UsedGateIDs, gateID)
		// Gate-local counters start at zero; Root totals untouched.
		st.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
		if err := saveStateUnlocked(dir, st); err != nil {
			return err
		}
		_ = appendEvent(dir, gateEvent{
			Event: "declare_gate", Observed: st.UpdatedAt, SessionID: sessionID,
			State: "open", Detail: gateID,
		})
		out = statusFrom(st, dir)
		return nil
	})
	return out, err
}

// CloseGate marks the open gate closed or abandoned.
func CloseGate(stateDir string, in CloseGateInput, now time.Time) (Status, error) {
	if now.IsZero() {
		now = time.Now()
	}
	sessionID := strings.TrimSpace(in.SessionID)
	gateID := strings.TrimSpace(in.GateID)
	status := strings.TrimSpace(in.Status)
	if status == "" {
		status = "closed"
	}
	if status != "closed" && status != "abandoned" {
		return Status{}, fmt.Errorf("status must be closed or abandoned")
	}
	if sessionID == "" || gateID == "" {
		return Status{}, fmt.Errorf("session_id and gate_id are required")
	}
	dir := stateDirOrDefault(stateDir)
	var out Status
	err := withSessionLock(dir, sessionID, func() error {
		st, err := loadOrInit(dir, sessionID, now)
		if err != nil {
			return err
		}
		if st.OpenGate == nil || st.OpenGate.GateID != gateID {
			return fmt.Errorf("no open gate %q", gateID)
		}
		rec := *st.OpenGate
		rec.Status = status
		rec.ClosedAt = now.UTC().Format(time.RFC3339Nano)
		st.GateHistory = append(st.GateHistory, rec)
		st.OpenGate = nil
		st.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
		if err := saveStateUnlocked(dir, st); err != nil {
			return err
		}
		_ = appendEvent(dir, gateEvent{
			Event: "close_gate", Observed: st.UpdatedAt, SessionID: sessionID,
			State: status, Detail: gateID,
		})
		out = statusFrom(st, dir)
		return nil
	})
	return out, err
}

// AckReroute clears sticky re-route after a structured restatement.
func AckReroute(stateDir string, in AckRerouteInput, now time.Time) (Status, error) {
	if now.IsZero() {
		now = time.Now()
	}
	sessionID := strings.TrimSpace(in.SessionID)
	gateID := strings.TrimSpace(in.GateID)
	if sessionID == "" || gateID == "" {
		return Status{}, fmt.Errorf("session_id and gate_id are required")
	}
	if len(cleanStrings(in.RemainingAcceptance)) == 0 {
		return Status{}, fmt.Errorf("remaining_acceptance requires at least one item")
	}
	wd := strings.TrimSpace(strings.ToLower(in.WorkerDecision))
	if wd != "none" && wd != "start" && wd != "resume" {
		return Status{}, fmt.Errorf("worker_decision must be none, start, or resume")
	}
	dir := stateDirOrDefault(stateDir)
	var out Status
	err := withSessionLock(dir, sessionID, func() error {
		st, err := loadOrInit(dir, sessionID, now)
		if err != nil {
			return err
		}
		if !st.StickyReroutePending {
			return fmt.Errorf("no sticky re-route pending")
		}
		if st.OpenGate == nil || st.OpenGate.GateID != gateID {
			return fmt.Errorf("ack_reroute requires open gate_id %q", gateID)
		}
		st.StickyReroutePending = false
		st.StickyReason = ""
		st.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
		if err := saveStateUnlocked(dir, st); err != nil {
			return err
		}
		_ = appendEvent(dir, gateEvent{
			Event: "ack_reroute", Observed: st.UpdatedAt, SessionID: sessionID,
			State: "cleared", Detail: gateID + " worker=" + wd,
		})
		out = statusFrom(st, dir)
		return nil
	})
	return out, err
}

// LoadStatus returns a read-only snapshot.
func LoadStatus(stateDir, sessionID string) (Status, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Status{}, fmt.Errorf("session_id is required")
	}
	dir := stateDirOrDefault(stateDir)
	path := statePath(dir, sessionID)
	out := Status{
		SessionID: sessionID, StatePath: path,
		RootBudgets: map[string]int{
			"playwright": rootMaxPlaywright, "screenshot": rootMaxScreenshot,
			"same_verify": rootMaxSameVerify, "high_cost_soft": rootHighCostSoft, "high_cost_hard": rootHighCostHard,
		},
		GateBudgets: map[string]int{
			"playwright": gateMaxPlaywright, "screenshot": gateMaxScreenshot,
			"same_verify": gateMaxSameVerify, "high_cost_soft": gateHighCostSoft, "high_cost_hard": gateHighCostHard,
		},
	}
	err := withSessionLock(dir, sessionID, func() error {
		st, err := loadStateUnlocked(dir, sessionID)
		if err != nil {
			return err
		}
		if st.SessionID == "" {
			return nil
		}
		out = statusFrom(st, dir)
		return nil
	})
	return out, err
}

func loadOrInit(dir, sessionID string, now time.Time) (state, error) {
	st, err := loadStateUnlocked(dir, sessionID)
	if err != nil {
		return state{}, err
	}
	if st.SessionID == "" {
		st = newRootState(sessionID, now)
	}
	return st, nil
}

func statusFrom(st state, dir string) Status {
	return Status{
		SessionID: st.SessionID, StartedAt: st.StartedAt, UpdatedAt: st.UpdatedAt,
		CompactCount: st.CompactCount, PlaywrightCalls: st.PlaywrightCalls,
		ScreenshotCalls: st.ScreenshotCalls, HighCostCalls: st.HighCostCalls,
		VerifyCounts: st.VerifyCounts, HandoffRequired: st.HandoffRequired,
		HandoffReason: st.HandoffReason, StickyReroutePending: st.StickyReroutePending,
		StickyReason: st.StickyReason, OpenGate: st.OpenGate,
		GateCount: len(st.UsedGateIDs), Override: st.Override,
		HandoffJSONPath: st.HandoffJSONPath, HandoffMDPath: st.HandoffMDPath,
		OverflowLatched: st.OverflowLatched, LatestPromptTokens: st.LatestPromptTokens,
		ToolResultBytesWindow: st.ToolResultBytesWindow, ContextPressure: st.ContextPressure,
		DeployCalls: st.DeployCalls, TestCalls: st.TestCalls,
		StatePath: statePath(dir, st.SessionID), Exists: true,
		RootBudgets: map[string]int{
			"playwright": rootMaxPlaywright, "screenshot": rootMaxScreenshot,
			"same_verify": rootMaxSameVerify, "high_cost_soft": rootHighCostSoft, "high_cost_hard": rootHighCostHard,
			"deploy": rootMaxDeploy, "test_bash": rootMaxTestBash,
		},
		GateBudgets: map[string]int{
			"playwright": gateMaxPlaywright, "screenshot": gateMaxScreenshot,
			"same_verify": gateMaxSameVerify, "high_cost_soft": gateHighCostSoft, "high_cost_hard": gateHighCostHard,
		},
	}
}

func acceptanceHash(acceptance []string, stop string) string {
	sum := sha256.Sum256([]byte(strings.Join(acceptance, "\n") + "\x00" + stop))
	return hex.EncodeToString(sum[:8])
}

func cleanStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func stateDirOrDefault(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return DefaultStateDir()
	}
	return dir
}
