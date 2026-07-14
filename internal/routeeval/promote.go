package routeeval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Real ledger shapes from mcpserver.RouteRecord JSONL (not the old fictional schema).
type ledgerRecord struct {
	RouteID string `json:"route_id"`
	State   string `json:"state"`
	Plan    struct {
		Kind       string `json:"kind"`
		Action     string `json:"action"`
		Objective  string `json:"objective"`
		RouteID    string `json:"route_id"`
		SelectedLane struct {
			Tool  string `json:"tool"`
			Model string `json:"model"`
		} `json:"selected_lane"`
	} `json:"plan"`
	Outcome *struct {
		Status           string `json:"status"`
		Verification     string `json:"verification"`
		HumanCorrection  string `json:"human_correction"`
		ResidualRisk     string `json:"residual_risk"`
	} `json:"outcome"`
	Diagnostics struct {
		DurationMS int64 `json:"duration_ms"`
		Usage      struct {
			InputTokens         int64 `json:"input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
		} `json:"usage"`
		Calls        int `json:"calls"`
		FailedCalls  int `json:"failed_calls"`
		WorkerStarts int `json:"worker_starts"`
	} `json:"diagnostics"`
	// Optional explicit family tag if present on the record.
	Family string `json:"family"`
}

// PromoteDryRun summarizes outcomes for a family; never mutates defaults (T7).
type PromoteDryRun struct {
	Family             string   `json:"family"`
	Path               string   `json:"path"`
	Total              int      `json:"total"`
	Accepted           int      `json:"accepted"`
	Failed             int      `json:"failed"`
	DistinctRouteIDs   int      `json:"distinct_route_ids"`
	MeetsK10           bool     `json:"meets_k10"`
	MeetsDistinctRoots bool     `json:"meets_distinct_routes_3"`
	AcceptRate         float64  `json:"accept_rate"`
	Recommendation     string   `json:"recommendation"`
	AutoPromote        bool     `json:"auto_promote"`
	Note               string   `json:"note"`
	SampleRouteIDs     []string `json:"sample_route_ids,omitempty"`
}

func DefaultOutcomePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claudex", "route-outcomes.jsonl")
}

// DryRunPromote reads real RouteRecord JSONL.
// Family filter is exact match on record.family, or plan.kind when family flag matches kind.
func DryRunPromote(path, family string) (PromoteDryRun, error) {
	if path == "" {
		path = DefaultOutcomePath()
	}
	family = strings.TrimSpace(family)
	out := PromoteDryRun{
		Family: family, Path: path, AutoPromote: false,
		Note: "Promotion requires explicit --confirm after human review; never automatic. Schema: state/outcome.status from RouteRecord.",
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			out.Recommendation = "no_outcome_file"
			return out, nil
		}
		return out, err
	}
	defer f.Close()

	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row ledgerRecord
		if json.Unmarshal([]byte(line), &row) != nil {
			continue
		}
		if family != "" {
			if row.Family != family && string(row.Plan.Kind) != family {
				// Explicit fields only — no whole-line substring matching.
				continue
			}
		}
		// Only closed terminal records count.
		status := row.State
		if row.Outcome != nil && row.Outcome.Status != "" {
			status = row.Outcome.Status
		}
		if status != "accepted" && status != "failed" && status != "abandoned" {
			continue
		}
		out.Total++
		if row.RouteID != "" && !seen[row.RouteID] {
			seen[row.RouteID] = true
			if len(out.SampleRouteIDs) < 5 {
				out.SampleRouteIDs = append(out.SampleRouteIDs, row.RouteID)
			}
		}
		if status == "accepted" {
			out.Accepted++
		} else {
			out.Failed++
		}
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	out.DistinctRouteIDs = len(seen)
	out.MeetsDistinctRoots = out.DistinctRouteIDs >= 3
	out.MeetsK10 = out.Accepted >= 10 && out.Total >= 10
	if out.Total > 0 {
		out.AcceptRate = float64(out.Accepted) / float64(out.Total)
	}
	if out.MeetsK10 && out.MeetsDistinctRoots && out.AcceptRate >= 0.8 {
		out.Recommendation = "eligible_for_manual_promote_review"
	} else {
		out.Recommendation = "insufficient_evidence"
	}
	return out, nil
}

// ConfirmPromote writes a pending marker only; never merges router defaults.
func ConfirmPromote(path, family string, confirm bool) (string, error) {
	if !confirm {
		return "", fmt.Errorf("pass --confirm after reviewing dry-run; auto promotion is forbidden")
	}
	dry, err := DryRunPromote(path, family)
	if err != nil {
		return "", err
	}
	if dry.Recommendation != "eligible_for_manual_promote_review" {
		return "", fmt.Errorf("not eligible: %s (accepted=%d total=%d distinct=%d)", dry.Recommendation, dry.Accepted, dry.Total, dry.DistinctRouteIDs)
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".config", "claudex")
	_ = os.MkdirAll(dir, 0o700)
	marker := filepath.Join(dir, "route-defaults-pending.json")
	raw, _ := json.MarshalIndent(map[string]any{
		"family": family, "status": "pending_human_merge", "dry_run": dry,
		"note": "Operator must manually edit router policy; this file is only a marker.",
	}, "", "  ")
	if err := os.WriteFile(marker, append(raw, '\n'), 0o600); err != nil {
		return "", err
	}
	return marker, nil
}
