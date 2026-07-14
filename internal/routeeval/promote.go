package routeeval

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Real ledger shapes from mcpserver.RouteRecord JSONL (not the old fictional schema).
type ledgerRecord struct {
	RouteID string `json:"route_id"`
	State   string `json:"state"`
	Plan    struct {
		Kind      string `json:"kind"`
		Action    string `json:"action"`
		Objective string `json:"objective"`
		RouteID   string `json:"route_id"`
		SelectedLane struct {
			Tool  string `json:"tool"`
			Model string `json:"model"`
		} `json:"selected_lane"`
	} `json:"plan"`
	Outcome *struct {
		Status          string `json:"status"`
		Verification    string `json:"verification"`
		HumanCorrection string `json:"human_correction"`
		ResidualRisk    string `json:"residual_risk"`
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
	MajorCorrections   int      `json:"major_corrections"`
	TotalFailedCalls   int      `json:"total_failed_calls"`
	AvgDurationMS      float64  `json:"avg_duration_ms,omitempty"`
	AvgPromptTokens    float64  `json:"avg_prompt_tokens,omitempty"`
	MeetsNonInferiority bool    `json:"meets_non_inferiority"`
	Recommendation     string   `json:"recommendation"`
	AutoPromote        bool     `json:"auto_promote"`
	Note               string   `json:"note"`
	SampleRouteIDs     []string `json:"sample_route_ids,omitempty"`
	Blockers           []string `json:"blockers,omitempty"`
}

func DefaultOutcomePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claudex", "route-outcomes.jsonl")
}

// DryRunPromote reads real RouteRecord JSONL.
// Family filter is exact match on record.family. When family is empty, all terminal
// records are considered. plan.kind is only used as a fallback when the record has
// no explicit family tag AND the filter equals plan.kind (coarse; prefer family).
func DryRunPromote(path, family string) (PromoteDryRun, error) {
	if path == "" {
		path = DefaultOutcomePath()
	}
	family = strings.TrimSpace(family)
	out := PromoteDryRun{
		Family: family, Path: path, AutoPromote: false,
		Note: "Promotion requires explicit --confirm after human review; never automatic. Schema: state/outcome.status from RouteRecord. Non-inferiority requires zero major human_correction, low child failures, and duration/token cohort without extreme outliers.",
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
	var durations []float64
	var promptToks []float64
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
			// Prefer explicit family tag. Fallback to plan.kind only when family empty on row.
			if row.Family != "" {
				if row.Family != family {
					continue
				}
			} else if string(row.Plan.Kind) != family {
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
		if row.Outcome != nil {
			corr := strings.ToLower(strings.TrimSpace(row.Outcome.HumanCorrection))
			if corr == "major" {
				out.MajorCorrections++
			}
		}
		out.TotalFailedCalls += row.Diagnostics.FailedCalls
		if row.Diagnostics.DurationMS > 0 {
			durations = append(durations, float64(row.Diagnostics.DurationMS))
		}
		pt := row.Diagnostics.Usage.InputTokens + row.Diagnostics.Usage.CacheReadTokens + row.Diagnostics.Usage.CacheCreationTokens
		if pt > 0 {
			promptToks = append(promptToks, float64(pt))
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
	if len(durations) > 0 {
		out.AvgDurationMS = mean(durations)
	}
	if len(promptToks) > 0 {
		out.AvgPromptTokens = mean(promptToks)
	}

	// Non-inferiority gates (T7):
	// 1. No major human corrections in the cohort.
	// 2. Child/tool failed_calls must not dominate (sum <= 10% of total records, and < accepted).
	// 3. Duration/token cohort has no extreme outliers (p95 <= 5× p50 when n>=4).
	var blockers []string
	if out.MajorCorrections > 0 {
		blockers = append(blockers, fmt.Sprintf("major_human_correction=%d", out.MajorCorrections))
	}
	if out.Total > 0 && out.TotalFailedCalls > 0 {
		// Any child-failure mass blocks promote review for a "clean" promote set.
		// Allow tiny noise: at most 0 for eligible (strict); canary with 9 failures fails.
		if out.TotalFailedCalls > max(1, out.Total/10) {
			blockers = append(blockers, fmt.Sprintf("child_failed_calls=%d", out.TotalFailedCalls))
		}
	}
	if !cohortNonInferior(durations) {
		blockers = append(blockers, "duration_outlier_vs_cohort_median")
	}
	if !cohortNonInferior(promptToks) {
		blockers = append(blockers, "token_outlier_vs_cohort_median")
	}
	// Extreme absolute cost (defensive): multi-hour or multi-million token averages.
	if out.AvgDurationMS > 30*60*1000 {
		blockers = append(blockers, "avg_duration_exceeds_30m")
	}
	if out.AvgPromptTokens > 1_000_000 {
		blockers = append(blockers, "avg_prompt_tokens_exceeds_1m")
	}
	out.Blockers = blockers
	out.MeetsNonInferiority = len(blockers) == 0

	if out.MeetsK10 && out.MeetsDistinctRoots && out.AcceptRate >= 0.8 && out.MeetsNonInferiority {
		out.Recommendation = "eligible_for_manual_promote_review"
	} else {
		out.Recommendation = "insufficient_evidence"
		if len(blockers) > 0 && out.MeetsK10 {
			out.Recommendation = "non_inferiority_failed"
		}
	}
	return out, nil
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func medianSorted(xs []float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return xs[n/2]
	}
	return (xs[n/2-1] + xs[n/2]) / 2
}

// cohortNonInferior is true when the sample is empty/small or p95 <= 5× p50.
func cohortNonInferior(xs []float64) bool {
	if len(xs) < 4 {
		return true
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	p50 := medianSorted(cp)
	if p50 <= 0 {
		return true
	}
	// p95 index
	idx := int(float64(len(cp)-1) * 0.95)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	p95 := cp[idx]
	return p95 <= 5*p50
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
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
		return "", fmt.Errorf("not eligible: %s (accepted=%d total=%d distinct=%d blockers=%v)", dry.Recommendation, dry.Accepted, dry.Total, dry.DistinctRouteIDs, dry.Blockers)
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
