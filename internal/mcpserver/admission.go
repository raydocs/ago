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
	if len(admission.RejectionReasons) > 0 {
		admission.Result = admissionRejected
	}
	return admission
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
	return fmt.Errorf("worker slice %q admission rejected: %s", admission.SliceID, strings.Join(admission.RejectionReasons, "; "))
}
