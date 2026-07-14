package mcpserver

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	routeParallelWorker = "parallel_worker"
	admissionAdmitted   = "admitted"
	admissionRejected   = "rejected"
)

var sliceIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

// evaluateWorkerAdmission is pure: rejection consumes no model call, budget,
// concurrency slot, or write lease.
func evaluateWorkerAdmission(in WorkerStartInput) WorkerAdmission {
	deadline := in.DeadlineMS
	if deadline == 0 {
		deadline = defaultDeadlineMS
	}
	admission := WorkerAdmission{
		Route:                routeParallelWorker,
		RouteID:              strings.TrimSpace(in.RouteID),
		SliceID:              strings.TrimSpace(in.SliceID),
		MarginalContribution: strings.TrimSpace(in.MarginalContribution),
		DeadlineMS:           deadline,
		Result:               admissionAdmitted,
		RejectionReasons:     []string{},
	}
	if admission.SliceID == "" {
		admission.RejectionReasons = append(admission.RejectionReasons, "slice_id is required")
	} else if !sliceIDPattern.MatchString(admission.SliceID) {
		admission.RejectionReasons = append(admission.RejectionReasons, "slice_id must be 1-128 characters using letters, numbers, dot, underscore, or dash")
	}
	if strings.TrimSpace(in.Objective) == "" {
		admission.RejectionReasons = append(admission.RejectionReasons, "objective is required")
	}
	if admission.MarginalContribution == "" {
		admission.RejectionReasons = append(admission.RejectionReasons, "marginal_contribution is required")
	}
	if strings.TrimSpace(in.OutputContract) == "" {
		admission.RejectionReasons = append(admission.RejectionReasons, "output_contract is required")
	}
	if strings.TrimSpace(in.DoneCondition) == "" {
		admission.RejectionReasons = append(admission.RejectionReasons, "done_condition is required")
	}
	if deadline < minDeadlineMS || deadline > maxDeadlineMS {
		admission.RejectionReasons = append(admission.RejectionReasons, fmt.Sprintf("deadline_ms must be between %d and %d", minDeadlineMS, maxDeadlineMS))
	}
	if len(in.RouteID) > 128 || len(in.Objective) > 8000 || len(in.Context) > 24000 || len(in.OutputContract) > 4000 || len(in.DoneCondition) > 4000 || len(in.MarginalContribution) > 4000 || len(in.RetryReason) > 2000 {
		admission.RejectionReasons = append(admission.RejectionReasons, "worker packet exceeds bounded field limits")
	}
	if len(in.Paths) > 32 {
		admission.RejectionReasons = append(admission.RejectionReasons, "worker paths are capped at 32 entries")
	}
	for _, path := range in.Paths {
		if len(path) > 1024 {
			admission.RejectionReasons = append(admission.RejectionReasons, "each worker path must be at most 1024 bytes")
			break
		}
	}
	if in.Write && !hasExplicitPath(in.Paths) {
		admission.RejectionReasons = append(admission.RejectionReasons, "write workers require explicit non-overlapping paths")
	}
	if reason := compositeSliceReason(detectSliceDomains(in)); reason != "" {
		admission.RejectionReasons = append(admission.RejectionReasons, reason)
		admission.SuggestedSlices = suggestSlices(in)
	}
	// T4: write workers should prefer a single verifier command.
	if in.Write && multiCommandDone(in.DoneCondition) {
		admission.RejectionReasons = append(admission.RejectionReasons, "done_condition chains multiple commands (&&, ||, ;, |, or newlines); use one primary verifier per slice")
	}
	if len(admission.RejectionReasons) > 0 {
		admission.Result = admissionRejected
	}
	return admission
}

func suggestSlices(in WorkerStartInput) []SuggestedSlice {
	byDomain := map[string][]string{}
	for _, p := range in.Paths {
		d := domainFromPath(p)
		if d == "" {
			d = "other"
		}
		byDomain[d] = append(byDomain[d], p)
	}
	if len(byDomain) == 0 {
		return []SuggestedSlice{{
			SliceID: in.SliceID + "-a", DoneCondition: "narrow one independent verifier",
			Note: "template only: restate objective for a single domain",
		}}
	}
	out := make([]SuggestedSlice, 0, len(byDomain))
	i := 0
	for d, paths := range byDomain {
		i++
		done := "go test ./..."
		switch d {
		case domainUI:
			done = "node --test thread-app/test/frontend-contract.test.mjs"
		case domainAPI:
			done = "cd thread-app && npm run typecheck"
		case domainSchema:
			done = "wrangler d1 migrations list (dry review) or schema unit test"
		case domainUsage:
			done = "go test ./internal/threadusage"
		case domainParsing:
			done = "go test ./internal/threadgraph"
		case domainDeploy:
			done = "wrangler deploy --dry-run if supported"
		}
		out = append(out, SuggestedSlice{
			SliceID: fmt.Sprintf("%s-%s-%d", sanitizeID(in.SliceID), shortDomain(d), i),
			Paths: paths, DoneCondition: done,
			Note: "zero-model template; re-admit with one domain only",
		})
	}
	return out
}

func shortDomain(d string) string {
	switch d {
	case domainSchema:
		return "schema"
	case domainAPI:
		return "api"
	case domainUI:
		return "ui"
	case domainUsage:
		return "usage"
	case domainDeploy:
		return "deploy"
	case domainParsing:
		return "parse"
	default:
		return "other"
	}
}

func sanitizeID(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "slice"
	}
	return s
}

func multiCommandDone(done string) bool {
	d := strings.TrimSpace(done)
	if d == "" {
		return false
	}
	if strings.Count(d, "&&") > 0 || strings.Count(d, "||") > 0 {
		return true
	}
	if strings.Contains(d, ";") || strings.Contains(d, "|") || strings.Contains(d, "\n") {
		return true
	}
	return false
}

func hasExplicitPath(paths []string) bool {
	for _, path := range paths {
		if strings.TrimSpace(path) != "" {
			return true
		}
	}
	return false
}

func admissionError(admission WorkerAdmission) error {
	msg := fmt.Sprintf("worker slice %q admission rejected: %s", admission.SliceID, strings.Join(admission.RejectionReasons, "; "))
	if len(admission.SuggestedSlices) > 0 {
		parts := make([]string, 0, len(admission.SuggestedSlices))
		for _, s := range admission.SuggestedSlices {
			parts = append(parts, s.SliceID+":"+strings.Join(s.Paths, ","))
		}
		msg += " | suggested_slices=" + strings.Join(parts, "; ")
	}
	return fmt.Errorf("%s", msg)
}
