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
var shellAssignmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=.*$`)

const (
	verifierRootOnly  = "root_only"
	verifierExactOnce = "worker_exact_once"
)

var workerVerifierExecutables = map[string]bool{
	"bazel": true, "bundle": true, "bun": true, "cargo": true, "cmake": true,
	"ctest": true, "deno": true, "dotnet": true, "go": true, "gradle": true,
	"jest": true, "make": true, "mvn": true, "node": true, "nox": true,
	"npm": true, "pnpm": true, "pytest": true, "python": true, "python3": true,
	"rake": true, "ruby": true, "swift": true, "tox": true, "uv": true,
	"xcodebuild": true, "yarn": true,
}

// exactWorkerVerifier recognizes one literal repository-local verifier. It is
// intentionally conservative: descriptive acceptance prose still admits the
// slice, but the Worker receives no Bash and Root owns final verification.
func exactWorkerVerifier(value string) (string, bool) {
	command := strings.TrimSpace(value)
	if command == "" || strings.ContainsAny(command, "\r\n;&|`<>") || strings.Contains(command, "$(") {
		return "", false
	}
	lower := " " + strings.ToLower(command) + " "
	for _, install := range []string{
		" -m pip install ", " pip install ", " npm install ", " pnpm install ",
		" yarn install ", " bundle install ", " cargo install ", " go install ",
		" uv pip install ",
	} {
		if strings.Contains(lower, install) {
			return "", false
		}
	}
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", false
	}
	i := 0
	for i < len(fields) && shellAssignmentPattern.MatchString(fields[i]) {
		i++
	}
	if i == len(fields) {
		return "", false
	}
	executable := fields[i]
	base := executable
	if slash := strings.LastIndexAny(base, `/\\`); slash >= 0 {
		base = base[slash+1:]
	}
	if workerVerifierExecutables[base] {
		return command, true
	}
	// A checked-in relative runner is an exact verifier without requiring a
	// language-specific allowlist (for example ./scripts/test.sh).
	if strings.HasPrefix(executable, "./") && len(executable) > 2 && !strings.Contains(executable[2:], "..") {
		return command, true
	}
	return "", false
}

// evaluateWorkerAdmission is pure: rejection consumes no model call, budget,
// concurrency slot, or write lease.
func evaluateWorkerAdmission(in WorkerStartInput) WorkerAdmission {
	deadline := in.DeadlineMS
	if deadline == 0 {
		deadline = defaultDeadlineMS
	}
	verifierMode := verifierRootOnly
	verifierCommand := ""
	if command, ok := exactWorkerVerifier(in.DoneCondition); ok {
		verifierMode, verifierCommand = verifierExactOnce, command
	}
	admission := WorkerAdmission{
		Route:                routeParallelWorker,
		RouteID:              strings.TrimSpace(in.RouteID),
		SliceID:              strings.TrimSpace(in.SliceID),
		MarginalContribution: strings.TrimSpace(in.MarginalContribution),
		DeadlineMS:           deadline,
		VerifierMode:         verifierMode,
		VerifierCommand:      verifierCommand,
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
	if in.EstimatedWorkerSeconds < 90 {
		admission.RejectionReasons = append(admission.RejectionReasons, "estimated_worker_seconds must be at least 90; keep smaller or unknown slices Supervisor-direct")
	}
	if in.EstimatedParallelSavings < 45 {
		admission.RejectionReasons = append(admission.RejectionReasons, "estimated_parallel_savings_seconds must be at least 45 after coordination and integration")
	}
	if strings.TrimSpace(in.EstimateBasis) == "" {
		admission.RejectionReasons = append(admission.RejectionReasons, "estimate_basis is required; do not invent timing to obtain Worker admission")
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
	if in.EstimatedWorkerSeconds > 86_400 || in.EstimatedParallelSavings > 86_400 {
		admission.RejectionReasons = append(admission.RejectionReasons, "worker estimates are capped at 86400 seconds")
	}
	if len(in.RouteID) > 128 || len(in.Objective) > 8000 || len(in.Context) > 24000 || len(in.OutputContract) > 4000 || len(in.DoneCondition) > 4000 || len(in.MarginalContribution) > 4000 || len(in.EstimateBasis) > 2000 || len(in.RetryReason) > 2000 {
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
	if reason := compositeSliceReasonForInput(in); reason != "" {
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
			Paths:   paths, DoneCondition: done,
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
