package router

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"claudexflow/internal/catalog"
)

type Kind string

const (
	KindAuto             Kind = "auto"
	KindGeneral          Kind = "general"
	KindQuick            Kind = "quick"
	KindHard             Kind = "hard"
	KindImplement        Kind = "implement"
	KindComplexImplement Kind = "complex-implement"
	KindExplore          Kind = "explore"
	KindFastResearch     Kind = "fast-research"
	KindDeepResearch     Kind = "deep-research"
	KindRealtime         Kind = "realtime"
	KindChinese          Kind = "chinese"
	KindLongContext      Kind = "long-context"
	KindFindThread       Kind = "find-thread"
	KindReadThread       Kind = "read-thread"
)

type Action string

const (
	ActionDirect     Action = "supervisor_direct"
	ActionCapability Action = "specialist_capability"
	ActionWorker     Action = "bounded_worker"
	ActionBlocked    Action = "capability_blocked"
)

type RouteRequest struct {
	Objective                  string       `json:"objective" jsonschema:"Root objective to route; never a hidden or reconstructed task."`
	Kind                       Kind         `json:"kind,omitempty" jsonschema:"Optional explicit kind; auto, general, quick, hard, implement, complex-implement, explore, fast-research, deep-research, realtime, chinese, long-context, find-thread, or read-thread."`
	Risk                       string       `json:"risk,omitempty" jsonschema:"normal or high; defaults to normal."`
	ExplicitURLs               []string     `json:"explicit_urls,omitempty" jsonschema:"Already-known URLs. Their presence qualifies URL digestion without discovery."`
	AcceptanceCriteria         []string     `json:"acceptance_criteria,omitempty" jsonschema:"Observable acceptance criteria already fixed for the parent task. Required before automatic or mandatory Worker dispatch."`
	VerificationTarget         string       `json:"verification_target,omitempty" jsonschema:"Exact deterministic command, probe, artifact check, or bounded semantic review that will evaluate the Worker slice."`
	WorkerMarginalContribution string       `json:"worker_marginal_contribution,omitempty" jsonschema:"Concrete parent work or critical-path time one independent Worker would avoid duplicating. Required before automatic or mandatory Worker dispatch."`
	EstimatedWorkerSeconds     int          `json:"estimated_worker_seconds,omitempty" jsonschema:"Evidence-based useful work duration for each candidate Worker slice. Worker admission requires at least 90 seconds; use 0 when unknown and stay direct."`
	EstimatedParallelSavings   int          `json:"estimated_parallel_savings_seconds,omitempty" jsonschema:"Evidence-based critical-path savings after coordination and integration. Worker admission requires at least 45 seconds; use 0 when unknown and stay direct."`
	EstimateBasis              string       `json:"estimate_basis,omitempty" jsonschema:"Short task-local evidence for the duration and savings estimates; never invent timing merely to obtain a Worker route."`
	LaneHealth                 []LaneHealth `json:"lane_health,omitempty" jsonschema:"Observed runtime lane health only. Do not invent failures. The MCP server injects current-session evidence."`
	IndependentSlices          int          `json:"independent_slices,omitempty" jsonschema:"Count of genuinely independent bounded slices, 0 to 3. Do not invent slices."`
	SharedMutableState         bool         `json:"shared_mutable_state,omitempty" jsonschema:"Whether candidate workstreams would edit or coordinate through shared mutable state."`
	Checkability               string       `json:"checkability,omitempty" jsonschema:"auto, objective, partial, or semantic."`
	Topology                   string       `json:"topology,omitempty" jsonschema:"auto, direct, or worker. Worker is a user-mandated topology and still must be admissible."`
	// RuntimeVerifierStatus is injected by claudex-flow after a zero-model
	// executable/environment preflight. It is not caller-controlled.
	RuntimeVerifierStatus string `json:"-"`
}

type Lane struct {
	Model  string `json:"model"`
	Effort string `json:"effort,omitempty"`
	Role   string `json:"role"`
	Tool   string `json:"tool,omitempty"`
}

type RouteFactors struct {
	Ambiguity          string `json:"ambiguity"`
	Coupling           string `json:"coupling"`
	StateDepth         string `json:"state_depth"`
	SemanticRisk       string `json:"semantic_risk"`
	Checkability       string `json:"checkability"`
	ExpectedRework     string `json:"expected_rework"`
	ContextDuplication string `json:"context_duplication"`
}

type AcceptanceContract struct {
	Ready              bool     `json:"ready"`
	Criteria           []string `json:"criteria,omitempty"`
	VerificationTarget string   `json:"verification_target,omitempty"`
	Boundary           string   `json:"boundary"`
}

type LaneHealth struct {
	Tool         string `json:"tool"`
	Status       string `json:"status"`
	FailureClass string `json:"failure_class,omitempty"`
	Reason       string `json:"reason,omitempty"`
	// ObservedAt is RFC3339Nano; used to merge session vs durable health by freshness (T12).
	ObservedAt string `json:"observed_at,omitempty"`
}

type SurfaceSnapshot struct {
	Client                  string       `json:"client"`
	LeadLane                string       `json:"lead_lane"`
	WorkerLane              string       `json:"worker_lane"`
	WorkerOverrideEvidence  string       `json:"worker_override_evidence"`
	MaximumConcurrentRuns   int          `json:"maximum_concurrent_runs"`
	AccountingComparability string       `json:"accounting_comparability"`
	LaneHealth              []LaneHealth `json:"lane_health,omitempty"`
}

type Candidate struct {
	Name              string `json:"name"`
	Lane              Lane   `json:"lane"`
	Qualified         bool   `json:"qualified"`
	RelativeIntensity string `json:"relative_resource_intensity"`
	Reason            string `json:"reason"`
}

type EscalationStep struct {
	Trigger string `json:"trigger"`
	Change  string `json:"change_one_dimension"`
	Action  string `json:"action"`
}

type Plan struct {
	RouteID                string             `json:"route_id,omitempty"`
	Kind                   Kind               `json:"kind"`
	Risk                   string             `json:"risk"`
	Action                 Action             `json:"action"`
	Supervisor             Lane               `json:"supervisor"`
	SelectedLane           Lane               `json:"selected_lane"`
	Reason                 string             `json:"reason"`
	Surface                SurfaceSnapshot    `json:"surface"`
	Factors                RouteFactors       `json:"factors"`
	Acceptance             AcceptanceContract `json:"acceptance_contract"`
	Candidates             []Candidate        `json:"candidate_comparison"`
	EvidenceBasis          string             `json:"evidence_basis"`
	AccountingUnit         string             `json:"accounting_unit"`
	DurableDefault         bool               `json:"durable_default"`
	AdoptionRule           string             `json:"adoption_rule"`
	WorkerAdmissible       bool               `json:"worker_admissible"`
	WorkerRejectionReasons []string           `json:"worker_rejection_reasons,omitempty"`
	WorkerPolicy           WorkerPolicy       `json:"worker_policy"`
	Verification           string             `json:"verification"`
	ResidualReview         string             `json:"residual_review"`
	Escalation             []EscalationStep   `json:"escalation"`
	StopCondition          string             `json:"stop_condition"`
	BlockedCapability      string             `json:"blocked_capability,omitempty"`
}

// WorkerPolicy is the runtime-enforced downside bound for delegation. Caller
// timing estimates remain advisory; they never authorize unbounded fan-out.
// Automatic routing always keeps one independent slice with the Supervisor.
type WorkerPolicy struct {
	Mode                     string `json:"mode"`
	EstimateTrust            string `json:"estimate_trust"`
	EstimatedWorkerSeconds   int    `json:"estimated_worker_seconds,omitempty"`
	EstimatedParallelSavings int    `json:"estimated_parallel_savings_seconds,omitempty"`
	IndependentSlices        int    `json:"independent_slices"`
	RootOwnedSlices          int    `json:"root_owned_slices"`
	MaxWorkerStarts          int    `json:"max_worker_starts"`
	MaxWorkerDeadlineMS      int64  `json:"max_worker_deadline_ms"`
}

// Decision preserves the single-lane CLI surface while embedding the route
// plan that the Claude X supervisor uses. CostClass is intentionally relative:
// the active surface mixes subscriptions and gateway accounting, so exact
// cheapest-route claims would be false precision.
type Decision struct {
	Kind          Kind            `json:"kind"`
	Risk          string          `json:"risk"`
	Profile       catalog.Profile `json:"profile"`
	Reason        string          `json:"reason"`
	CostClass     string          `json:"cost_class"`
	Escalation    string          `json:"escalation"`
	RouteAction   Action          `json:"route_action"`
	Tool          string          `json:"tool,omitempty"`
	EvidenceBasis string          `json:"evidence_basis"`
	Plan          Plan            `json:"route_plan"`
}

var urlPattern = regexp.MustCompile(`https?://[^\s<>()\[\]{}"']+`)

func Decide(task string, kind Kind, risk, modelOverride, effortOverride string) (Decision, error) {
	plan, err := PlanRoute(RouteRequest{Objective: task, Kind: kind, Risk: risk})
	if err != nil {
		return Decision{}, err
	}
	selected := plan.SelectedLane
	reason := plan.Reason
	evidenceBasis := plan.EvidenceBasis
	if modelOverride != "" {
		selected.Model = modelOverride
		selected.Effort = ""
		selected.Role = "explicit_single_lane"
		selected.Tool = ""
		reason = "explicit model override; automatic route comparison is advisory only"
		evidenceBasis = "user_override"
	}
	p, ok := catalog.Get(selected.Model)
	if !ok {
		return Decision{}, fmt.Errorf("unknown model %q", selected.Model)
	}
	if selected.Effort != "" {
		p.DefaultEffort = selected.Effort
	}
	if effortOverride != "" {
		if err := catalog.ValidateEffort(p, effortOverride); err != nil {
			return Decision{}, err
		}
		p.DefaultEffort = effortOverride
	}
	firstEscalation := plan.Escalation[0].Action
	return Decision{
		Kind: plan.Kind, Risk: plan.Risk, Profile: p, Reason: reason,
		CostClass: "relative:" + candidateIntensity(plan.Candidates, plan.Action), Escalation: firstEscalation,
		RouteAction: plan.Action, Tool: selected.Tool, EvidenceBasis: evidenceBasis, Plan: plan,
	}, nil
}

func PlanRoute(in RouteRequest) (Plan, error) {
	in.Objective = strings.TrimSpace(in.Objective)
	if in.Objective == "" {
		return Plan{}, fmt.Errorf("objective is required")
	}
	if len(in.Objective) > 8000 {
		return Plan{}, fmt.Errorf("objective exceeds 8000 bytes")
	}
	if in.Risk == "" {
		in.Risk = "normal"
	}
	if in.Risk != "normal" && in.Risk != "high" {
		return Plan{}, fmt.Errorf("risk must be normal or high")
	}
	if in.Kind == "" || in.Kind == KindAuto {
		in.Kind = infer(in.Objective)
	}
	if !validKind(in.Kind) {
		return Plan{}, fmt.Errorf("unsupported kind %q", in.Kind)
	}
	if in.Checkability == "" || in.Checkability == "auto" {
		in.Checkability = inferCheckability(in.Objective, in.Kind)
	}
	if in.Checkability != "objective" && in.Checkability != "partial" && in.Checkability != "semantic" {
		return Plan{}, fmt.Errorf("checkability must be auto, objective, partial, or semantic")
	}
	if in.Topology == "" {
		in.Topology = "auto"
	}
	if in.Topology != "auto" && in.Topology != "direct" && in.Topology != "worker" {
		return Plan{}, fmt.Errorf("topology must be auto, direct, or worker")
	}
	if in.IndependentSlices < 0 || in.IndependentSlices > 3 {
		return Plan{}, fmt.Errorf("independent_slices must be between 0 and 3")
	}
	if in.EstimatedWorkerSeconds < 0 || in.EstimatedWorkerSeconds > 86_400 {
		return Plan{}, fmt.Errorf("estimated_worker_seconds must be between 0 and 86400")
	}
	if in.EstimatedParallelSavings < 0 || in.EstimatedParallelSavings > 86_400 {
		return Plan{}, fmt.Errorf("estimated_parallel_savings_seconds must be between 0 and 86400")
	}
	in.EstimateBasis = strings.TrimSpace(in.EstimateBasis)
	if len(in.EstimateBasis) > 2000 {
		return Plan{}, fmt.Errorf("estimate_basis exceeds 2000 bytes")
	}
	urls := append([]string(nil), in.ExplicitURLs...)
	if len(urls) == 0 {
		urls = urlPattern.FindAllString(in.Objective, 8)
	}
	if len(urls) > 8 {
		return Plan{}, fmt.Errorf("explicit URLs are capped at 8")
	}
	criteria, err := normalizeCriteria(in.AcceptanceCriteria)
	if err != nil {
		return Plan{}, err
	}
	in.AcceptanceCriteria = criteria
	in.VerificationTarget = strings.TrimSpace(in.VerificationTarget)
	in.WorkerMarginalContribution = strings.TrimSpace(in.WorkerMarginalContribution)
	if len(in.VerificationTarget) > 4000 {
		return Plan{}, fmt.Errorf("verification_target exceeds 4000 bytes")
	}
	if len(in.WorkerMarginalContribution) > 4000 {
		return Plan{}, fmt.Errorf("worker_marginal_contribution exceeds 4000 bytes")
	}
	health, err := normalizeLaneHealth(in.LaneHealth)
	if err != nil {
		return Plan{}, err
	}
	in.LaneHealth = health

	factors := analyzeFactors(in)
	workerReasons := workerRejectionReasons(in, factors)
	workerAdmissible := len(workerReasons) == 0
	if in.Topology == "worker" && !workerAdmissible {
		return Plan{}, fmt.Errorf("mandatory worker topology is not admissible: %s", strings.Join(workerReasons, "; "))
	}

	supervisor := Lane{Model: "gpt-5.6-sol", Effort: supervisorEffort(), Role: "supervisor"}
	action, selected, reason := chooseAction(in, urls, workerAdmissible, supervisor)
	workerPolicy := buildWorkerPolicy(in, action)
	blockedCapability := ""
	if selected.Tool != "" && laneUnavailable(in.LaneHealth, selected.Tool) {
		blockedCapability = selected.Tool
		action = ActionBlocked
		reason = fmt.Sprintf("required capability %s is quarantined by observed runtime evidence; do not fallback or repeat it until the lane is repaired and a health canary passes", selected.Tool)
	}
	candidates := compareCandidates(action, selected, supervisor, workerAdmissible, workerReasons)
	residual := "review only acceptance properties not encoded by the deterministic verifier"
	if in.Risk == "high" || factors.SemanticRisk == "high" {
		residual = "after deterministic PASS, use one fresh-context review only for material unencoded semantic risk"
	}
	evidenceBasis := "one_off_task_shape_heuristic"
	if in.Topology == "worker" || in.Topology == "direct" {
		evidenceBasis = "user_mandated_topology"
	}
	return Plan{
		Kind: in.Kind, Risk: in.Risk, Action: action, Supervisor: supervisor, SelectedLane: selected,
		Reason: reason,
		Surface: SurfaceSnapshot{
			Client: "Claude Code via Claude X", LeadLane: "gpt-5.6-sol/" + supervisor.Effort, WorkerLane: "grok-4.5/high",
			WorkerOverrideEvidence: "pinned request plus resolved-model verification", MaximumConcurrentRuns: 3,
			AccountingComparability: "mixed subscription and gateway signals; no single comparable spend unit",
			LaneHealth:              append([]LaneHealth(nil), in.LaneHealth...),
		},
		Factors: factors,
		Acceptance: AcceptanceContract{
			Ready:    len(in.AcceptanceCriteria) > 0 && in.VerificationTarget != "",
			Criteria: append([]string(nil), in.AcceptanceCriteria...), VerificationTarget: in.VerificationTarget,
			Boundary: "criteria and verification are frozen for this route; a real scope replacement requires a new route",
		},
		Candidates: candidates, EvidenceBasis: evidenceBasis,
		AccountingUnit: "relative_resource_intensity", DurableDefault: false,
		AdoptionRule:     "promote a route only after predeclared representative runs meet the frozen acceptance contract and non-inferiority threshold",
		WorkerAdmissible: workerAdmissible, WorkerRejectionReasons: workerReasons, WorkerPolicy: workerPolicy,
		Verification:   verificationInstruction(in.VerificationTarget),
		ResidualReview: residual,
		Escalation: []EscalationStep{
			{Trigger: "missing_or_conflicting_context", Change: "prompt_context", Action: "repair only the missing context; keep lane, tools, and topology fixed"},
			{Trigger: "transient_start_or_tool_failure", Change: "retry", Action: "retry the same lane once only when runtime marks it retryable and no side effect occurred"},
			{Trigger: "clear_task_but_insufficient_checking", Change: "effort", Action: "raise effort one supported level for only the failing workstream"},
			{Trigger: "reasoning_ambiguity_or_state_tracking_mismatch", Change: "lane", Action: "escalate only the unresolved slice, then return to the cheaper qualified lane"},
		},
		StopCondition: stopCondition(blockedCapability), BlockedCapability: blockedCapability,
	}, nil
}

func buildWorkerPolicy(in RouteRequest, action Action) WorkerPolicy {
	policy := WorkerPolicy{
		Mode: "direct", EstimateTrust: "caller_advisory_unattested",
		EstimatedWorkerSeconds:   in.EstimatedWorkerSeconds,
		EstimatedParallelSavings: in.EstimatedParallelSavings,
		IndependentSlices:        in.IndependentSlices,
	}
	if action != ActionWorker {
		return policy
	}
	policy.Mode = "bounded_speculation"
	policy.MaxWorkerDeadlineMS = 180_000
	policy.RootOwnedSlices = 1
	policy.MaxWorkerStarts = in.IndependentSlices - policy.RootOwnedSlices
	if policy.MaxWorkerStarts > 2 {
		policy.MaxWorkerStarts = 2
	}
	if in.Topology == "worker" {
		// A user-mandated topology may assign every declared slice, but remains
		// bounded by the normal runtime concurrency and a five-minute deadline.
		policy.Mode = "user_mandated_bounded"
		policy.RootOwnedSlices = 0
		policy.MaxWorkerStarts = in.IndependentSlices
		policy.MaxWorkerDeadlineMS = 300_000
	}
	if policy.MaxWorkerStarts < 0 {
		policy.MaxWorkerStarts = 0
	}
	return policy
}

func supervisorEffort() string {
	switch effort := strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDEX_THREAD_EFFORT"))); effort {
	case "medium", "high", "xhigh":
		return effort
	default:
		return "high"
	}
}

func chooseAction(in RouteRequest, urls []string, workerAdmissible bool, supervisor Lane) (Action, Lane, string) {
	if in.Topology == "direct" {
		return ActionDirect, supervisor, "direct topology was explicitly requested and preserves the acceptance contract"
	}
	if in.Kind == KindFindThread {
		return ActionCapability, Lane{Model: supervisor.Model, Effort: supervisor.Effort, Role: "local_thread_index", Tool: "find_thread"}, "the missing context is a prior Thread identity; use the zero-model local index before spending tokens on read_thread"
	}
	if in.Kind == KindReadThread {
		return ActionCapability, Lane{Model: "glm-5.2", Role: "read_thread", Tool: "read_thread"}, "a prior local Thread is the concrete missing context; use one bounded GLM extraction instead of copying the transcript into the Supervisor"
	}
	if len(urls) > 0 || in.Kind == KindFastResearch {
		return ActionCapability, Lane{Model: "gemini-3.5-flash", Effort: "medium", Role: "url_digest", Tool: "digest_urls"}, "explicit URLs need bounded extraction, not search or Worker fan-out"
	}
	switch in.Kind {
	case KindRealtime, KindDeepResearch:
		return ActionCapability, Lane{Model: "grok-4.5", Effort: "high", Role: "external_search", Tool: "search_external"}, "external/current evidence is the concrete missing capability; the Sol supervisor retains synthesis"
	case KindExplore:
		return ActionCapability, Lane{Model: "gpt-5.6-terra", Effort: "high", Role: "repo_explore", Tool: "explore_repository"}, "repository location and dependency evidence is the concrete missing capability"
	case KindLongContext:
		return ActionCapability, Lane{Model: "sonnet[1m]", Effort: "high", Role: "native_claude", Tool: "consult_native_claude"}, "the context requirement exceeds the root lane; use one bounded native 1M consultation and return evidence"
	}
	if in.Topology == "worker" || (in.Topology == "auto" && workerAdmissible) {
		return ActionWorker, Lane{Model: "grok-4.5", Effort: "high", Role: "worker", Tool: "start_worker"}, "an explicit independent, checkable slice can avoid duplicating Supervisor implementation"
	}
	return ActionDirect, supervisor, "no qualified capability gap or net-useful independent Worker slice was established"
}

func workerRejectionReasons(in RouteRequest, factors RouteFactors) []string {
	var reasons []string
	if len(in.AcceptanceCriteria) == 0 {
		reasons = append(reasons, "no observable acceptance criteria were fixed")
	}
	if strings.TrimSpace(in.VerificationTarget) == "" {
		reasons = append(reasons, "no concrete verification target was fixed")
	}
	if in.IndependentSlices == 0 {
		reasons = append(reasons, "no independent slice was declared")
	}
	if in.Topology == "auto" && in.IndependentSlices < 2 {
		reasons = append(reasons, "automatic delegation requires at least two independent slices so the Supervisor retains useful work")
	}
	if strings.TrimSpace(in.WorkerMarginalContribution) == "" {
		reasons = append(reasons, "no marginal contribution was declared for the Worker")
	}
	if in.EstimatedWorkerSeconds < 90 {
		reasons = append(reasons, "estimated useful work per Worker is below the 90-second delegation threshold")
	}
	if in.EstimatedParallelSavings < 45 {
		reasons = append(reasons, "estimated critical-path savings is below the 45-second delegation threshold")
	}
	if strings.TrimSpace(in.EstimateBasis) == "" {
		reasons = append(reasons, "no task-local estimate basis was supplied; unknown ROI stays Supervisor-direct")
	}
	if in.SharedMutableState {
		reasons = append(reasons, "candidate work shares mutable state")
	}
	if factors.Checkability == "semantic" {
		reasons = append(reasons, "the slice lacks an affordable objective or partial verifier")
	}
	if in.RuntimeVerifierStatus != "" && in.RuntimeVerifierStatus != "available" && in.RuntimeVerifierStatus != "available_fallback" {
		reasons = append(reasons, "the Root verifier is not executable in the current project environment (status="+in.RuntimeVerifierStatus+")")
	}
	if factors.ContextDuplication == "high" {
		reasons = append(reasons, "the slice would duplicate most lead context")
	}
	if laneUnavailable(in.LaneHealth, "start_worker") {
		reasons = append(reasons, "start_worker is quarantined by observed runtime evidence")
	}
	return reasons
}

func normalizeCriteria(values []string) ([]string, error) {
	if len(values) > 12 {
		return nil, fmt.Errorf("acceptance_criteria are capped at 12 entries")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len(value) > 1000 {
			return nil, fmt.Errorf("each acceptance criterion must be at most 1000 bytes")
		}
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out, nil
}

func normalizeLaneHealth(values []LaneHealth) ([]LaneHealth, error) {
	if len(values) > 8 {
		return nil, fmt.Errorf("lane_health is capped at 8 entries")
	}
	known := map[string]bool{
		"start_worker": true, "search_external": true, "digest_urls": true,
		"explore_repository": true, "consult_native_claude": true, "find_thread": true, "read_thread": true,
	}
	byTool := map[string]LaneHealth{}
	for _, value := range values {
		value.Tool = strings.TrimSpace(value.Tool)
		value.Status = strings.TrimSpace(value.Status)
		value.FailureClass = strings.TrimSpace(value.FailureClass)
		if !known[value.Tool] {
			return nil, fmt.Errorf("unsupported lane_health tool %q", value.Tool)
		}
		if value.Status != "healthy" && value.Status != "degraded" && value.Status != "unavailable" {
			return nil, fmt.Errorf("lane_health status for %s must be healthy, degraded, or unavailable", value.Tool)
		}
		if len(value.Reason) > 2000 || len(value.FailureClass) > 128 {
			return nil, fmt.Errorf("lane_health evidence exceeds bounded field limits")
		}
		byTool[value.Tool] = value
	}
	tools := make([]string, 0, len(byTool))
	for tool := range byTool {
		tools = append(tools, tool)
	}
	sort.Strings(tools)
	out := make([]LaneHealth, 0, len(tools))
	for _, tool := range tools {
		out = append(out, byTool[tool])
	}
	return out, nil
}

func laneUnavailable(values []LaneHealth, tool string) bool {
	for _, value := range values {
		if value.Tool == tool && value.Status == "unavailable" {
			return true
		}
	}
	return false
}

func stopCondition(blockedCapability string) string {
	if blockedCapability != "" {
		return "stop and report the quarantined capability plus repair evidence required; do not silently fallback"
	}
	return "stop immediately when the frozen acceptance contract and required residual review pass"
}

func verificationInstruction(target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return "no concrete verification target supplied; keep execution direct unless the route is only a read-only capability gap"
	}
	return "run the frozen verification target before residual semantic review: " + target
}

func compareCandidates(action Action, selected, supervisor Lane, workerAdmissible bool, workerReasons []string) []Candidate {
	singleReason := "no cheaper in-session single-agent lead has verified non-inferiority and an actionable switch; keep the current qualified Supervisor"
	routedLane := selected
	routedQualified := action == ActionCapability || (action == ActionWorker && workerAdmissible)
	routedIntensity := "not_qualified"
	routedReason := strings.Join(workerReasons, "; ")
	switch action {
	case ActionCapability:
		if selected.Tool == "find_thread" {
			routedIntensity = "minimal_zero_model_local_scan"
			routedReason = "one bounded local transcript scan returns candidate IDs without a child model or duplicated Supervisor context"
		} else {
			routedIntensity = "lower_for_bounded_gap"
			routedReason = "one specialist answers only the bounded capability gap; Supervisor context is not duplicated"
		}
	case ActionWorker:
		routedIntensity = "potentially_lower_if_first_pass_accepts"
		routedReason = "one independent Worker avoids declared Supervisor work; coordination, verification, retries, and rescue remain counted"
	case ActionBlocked:
		routedQualified = false
		routedIntensity = "unavailable"
		routedReason = "the required capability is quarantined; no silent substitute is qualified"
	case ActionDirect:
		routedLane = Lane{Model: "grok-4.5", Effort: "high", Role: "worker", Tool: "start_worker"}
		if routedReason == "" {
			routedReason = "no qualified independent routed slice was established"
		}
	}
	return []Candidate{
		{Name: "current_single_agent", Lane: supervisor, Qualified: true, RelativeIntensity: "baseline", Reason: "fixed Claude X supervisor lane"},
		{Name: "cheapest_plausible_single_agent", Lane: supervisor, Qualified: true, RelativeIntensity: "baseline", Reason: singleReason},
		{Name: "routed_route", Lane: routedLane, Qualified: routedQualified, RelativeIntensity: routedIntensity, Reason: routedReason},
	}
}

func analyzeFactors(in RouteRequest) RouteFactors {
	t := strings.ToLower(in.Objective)
	ambiguity := "medium"
	if containsAny(t, "exact", "only", "specific", "明确", "仅", "只需") {
		ambiguity = "low"
	} else if containsAny(t, "best", "improve", "optimize", "strategy", "better", "最好", "优化", "策略", "更好") {
		ambiguity = "high"
	}
	coupling := "medium"
	if in.SharedMutableState {
		coupling = "high"
	} else if in.IndependentSlices > 0 {
		coupling = "low"
	} else if in.Kind == KindComplexImplement {
		coupling = "high"
	}
	stateDepth := "medium"
	if in.Kind == KindQuick || in.Kind == KindFastResearch || in.Kind == KindRealtime || in.Kind == KindExplore || in.Kind == KindFindThread || in.Kind == KindReadThread {
		stateDepth = "low"
	} else if in.Kind == KindComplexImplement || in.Kind == KindHard || in.Kind == KindLongContext {
		stateDepth = "high"
	}
	semanticRisk := "normal"
	if in.Risk == "high" || in.Kind == KindHard || containsAny(t, "irreversible", "security", "payment", "production", "不可逆", "安全", "支付", "生产") {
		semanticRisk = "high"
	} else if in.Kind == KindQuick {
		semanticRisk = "low"
	}
	contextDuplication := "medium"
	if in.SharedMutableState || in.Kind == KindLongContext {
		contextDuplication = "high"
	} else if in.IndependentSlices > 0 {
		contextDuplication = "low"
	} else if in.Kind == KindComplexImplement {
		contextDuplication = "high"
	}
	expectedRework := "medium"
	if ambiguity == "high" || coupling == "high" || semanticRisk == "high" {
		expectedRework = "high"
	} else if ambiguity == "low" && in.Checkability == "objective" {
		expectedRework = "low"
	}
	return RouteFactors{Ambiguity: ambiguity, Coupling: coupling, StateDepth: stateDepth, SemanticRisk: semanticRisk, Checkability: in.Checkability, ExpectedRework: expectedRework, ContextDuplication: contextDuplication}
}

func infer(task string) Kind {
	t := strings.ToLower(task)
	switch {
	case containsAny(t, "find thread", "find-thread", "find_thread", "search threads", "which thread", "查找 thread", "搜索 thread", "找到 thread", "哪个 thread", "查找线程", "搜索线程", "找到哪个线程"):
		return KindFindThread
	case strings.Contains(t, "ampcode.com/threads/"):
		return KindFastResearch
	case containsAny(t, "claudex-threads.ppop.workers.dev/#/thread/", "#/thread/", "read thread", "read-thread", "读取 thread", "读取线程", "历史 thread", "previous claude x thread", "prior claude x thread"):
		return KindReadThread
	case urlPattern.MatchString(t):
		return KindFastResearch
	case containsAny(t, "latest", "today", "current news", "x.com", "twitter", "实时", "最新", "新闻"):
		return KindRealtime
	case containsAny(t, "explore", "locate", "where is", "dependency", "repository map", "探索", "定位", "依赖"):
		return KindExplore
	case containsAny(t, "research", "compare sources", "literature", "调研", "资料", "多源"):
		return KindDeepResearch
	case containsAny(t, "implement", "fix", "refactor", "test", "optimize", "upgrade", "实现", "修复", "重构", "测试", "优化", "升级"):
		if containsAny(t, "architecture", "migration", "cross-module", "complex", "workflow", "router", "架构", "迁移", "跨模块", "复杂", "工作流", "路由") {
			return KindComplexImplement
		}
		return KindImplement
	case containsAny(t, "huge context", "1m", "large repository", "超长上下文", "大型仓库"):
		return KindLongContext
	case containsAny(t, "hardest", "proof", "irreversible", "极难", "证明", "不可逆"):
		return KindHard
	default:
		return KindGeneral
	}
}

func inferCheckability(task string, kind Kind) string {
	t := strings.ToLower(task)
	if containsAny(t, "go test", "npm test", "pytest", "lint", "typecheck", "build", "schema", "exact", "测试通过", "构建通过", "精确") {
		return "objective"
	}
	if kind == KindImplement || kind == KindComplexImplement || kind == KindExplore || kind == KindFastResearch || kind == KindRealtime || kind == KindFindThread || kind == KindReadThread {
		return "partial"
	}
	return "semantic"
}

func validKind(kind Kind) bool {
	switch kind {
	case KindGeneral, KindQuick, KindHard, KindImplement, KindComplexImplement, KindExplore, KindFastResearch, KindDeepResearch, KindRealtime, KindChinese, KindLongContext, KindFindThread, KindReadThread:
		return true
	default:
		return false
	}
}

func candidateIntensity(candidates []Candidate, action Action) string {
	target := "routed_route"
	if action == ActionDirect {
		target = "cheapest_plausible_single_agent"
	}
	for _, candidate := range candidates {
		if candidate.Name == target {
			return candidate.RelativeIntensity
		}
	}
	return "unknown"
}

func containsAny(s string, values ...string) bool {
	for _, v := range values {
		if s == v || strings.Contains(s, v) {
			return true
		}
	}
	return false
}
