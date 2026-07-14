package mcpserver

import (
	"encoding/json"
	"fmt"
	"strings"

	"claudexflow/internal/claude"
)

const (
	maxWorkerReportBytes   = 32 * 1024
	maxEvidenceReportBytes = 24 * 1024
)

func decodeEvidenceReport(result claude.Result) (EvidenceReport, error) {
	raw := result.Structured
	if len(raw) == 0 {
		raw = json.RawMessage(result.Text)
	}
	if len(raw) > maxEvidenceReportBytes {
		return EvidenceReport{}, fmt.Errorf("evidence packet exceeds %d bytes", maxEvidenceReportBytes)
	}
	var report EvidenceReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return EvidenceReport{}, err
	}
	if report.Status != "completed" && report.Status != "blocked" {
		return EvidenceReport{}, fmt.Errorf("unsupported evidence status %q", report.Status)
	}
	if strings.TrimSpace(report.Summary) == "" || len(report.Summary) > 4000 {
		return EvidenceReport{}, fmt.Errorf("evidence summary must be 1-4000 bytes")
	}
	if len(report.Items) > 12 || len(report.OpenQuestions) > 8 {
		return EvidenceReport{}, fmt.Errorf("evidence packet exceeds item/open-question limits")
	}
	for _, item := range report.Items {
		if strings.TrimSpace(item.Claim) == "" || len(item.Claim) > 2000 || len(item.Source) > 2000 || len(item.Detail) > 4000 {
			return EvidenceReport{}, fmt.Errorf("evidence item exceeds bounded field limits")
		}
	}
	for _, question := range report.OpenQuestions {
		if len(question) > 2000 {
			return EvidenceReport{}, fmt.Errorf("open question exceeds 2000 bytes")
		}
	}
	return report, nil
}

func validateWorkerReport(raw json.RawMessage, report WorkerReport) error {
	if len(raw) > maxWorkerReportBytes {
		return fmt.Errorf("worker report exceeds %d bytes", maxWorkerReportBytes)
	}
	if report.Status != "completed" && report.Status != "needs_capability" && report.Status != "blocked" {
		return fmt.Errorf("unsupported worker status %q", report.Status)
	}
	if strings.TrimSpace(report.Summary) == "" || len(report.Summary) > 4000 {
		return fmt.Errorf("worker summary must be 1-4000 bytes")
	}
	if len(report.Evidence) > 12 || len(report.Verification) > 12 || len(report.ChangedPaths) > 64 {
		return fmt.Errorf("worker report exceeds evidence, verification, or changed-path limits")
	}
	for _, value := range append(append([]string(nil), report.Evidence...), report.Verification...) {
		if len(value) > 4000 {
			return fmt.Errorf("worker evidence or verification item exceeds 4000 bytes")
		}
	}
	for _, path := range report.ChangedPaths {
		if len(path) > 1024 {
			return fmt.Errorf("worker changed path exceeds 1024 bytes")
		}
	}
	if report.Status == "needs_capability" {
		if len(report.Needs) != 1 {
			return fmt.Errorf("needs_capability requires exactly one bounded need")
		}
	} else if len(report.Needs) != 0 {
		return fmt.Errorf("status %s must have an empty needs array", report.Status)
	}
	for _, need := range report.Needs {
		if need.Kind != "external_search" && need.Kind != "url_digest" && need.Kind != "repo_explore" && need.Kind != "find_thread" && need.Kind != "read_thread" {
			return fmt.Errorf("unsupported capability need %q", need.Kind)
		}
		if strings.TrimSpace(need.Question) == "" || len(need.Question) > 8000 || len(need.Why) > 4000 || len(need.URLs) > 8 {
			return fmt.Errorf("capability need exceeds bounded field limits")
		}
		for _, rawURL := range need.URLs {
			if len(rawURL) > 2048 {
				return fmt.Errorf("capability need URL exceeds 2048 bytes")
			}
		}
	}
	return nil
}
