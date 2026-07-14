package router

import (
	"strings"
	"testing"
)

func TestAutomaticRoutesMatchClaudeXTopology(t *testing.T) {
	tests := []struct {
		task   string
		action Action
		model  string
		tool   string
	}{
		{"fix a parser bug", ActionDirect, "gpt-5.6-sol", ""},
		{"implement a cross-module architecture migration", ActionDirect, "gpt-5.6-sol", ""},
		{"locate repository dependencies", ActionCapability, "gpt-5.6-terra", "explore_repository"},
		{"research and compare current sources", ActionCapability, "grok-4.5", "search_external"},
		{"find today's latest X news", ActionCapability, "grok-4.5", "search_external"},
		{"digest https://example.com/spec", ActionCapability, "gemini-3.5-flash", "digest_urls"},
		{"find thread that changed src/server/index.ts", ActionCapability, "gpt-5.6-sol", "find_thread"},
		{"read https://claudex-threads.ppop.workers.dev/#/thread/thread-123 and extract the decision", ActionCapability, "glm-5.2", "read_thread"},
		{"ordinary request", ActionDirect, "gpt-5.6-sol", ""},
	}
	for _, tt := range tests {
		plan, err := PlanRoute(RouteRequest{Objective: tt.task})
		if err != nil {
			t.Fatal(err)
		}
		if plan.Action != tt.action || plan.SelectedLane.Model != tt.model || plan.SelectedLane.Tool != tt.tool {
			t.Errorf("task %q route = %s %s %s; want %s %s %s", tt.task, plan.Action, plan.SelectedLane.Model, plan.SelectedLane.Tool, tt.action, tt.model, tt.tool)
		}
		if plan.Supervisor.Model != "gpt-5.6-sol" || plan.Supervisor.Effort != "xhigh" {
			t.Fatalf("supervisor drifted: %#v", plan.Supervisor)
		}
	}
}

func TestExternalAmpThreadURLDoesNotMisrouteToLocalReadThread(t *testing.T) {
	plan, err := PlanRoute(RouteRequest{Objective: "Read thread https://ampcode.com/threads/T-123 and summarize the decision"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Kind != KindFastResearch || plan.SelectedLane.Tool != "digest_urls" {
		t.Fatalf("external Amp URL was treated as a local Claude X Thread: %#v", plan)
	}

	local, err := PlanRoute(RouteRequest{Objective: "Read https://claudex-threads.ppop.workers.dev/#/thread/thread-123 and extract the verifier"})
	if err != nil {
		t.Fatal(err)
	}
	if local.Kind != KindReadThread || local.SelectedLane.Tool != "read_thread" {
		t.Fatalf("local Claude X Thread was not routed to read_thread: %#v", local)
	}
}

func TestWorkerRequiresIndependentCheckableNonSharedSlice(t *testing.T) {
	plan, err := PlanRoute(RouteRequest{
		Objective:                  "Implement the isolated parser package and run go test ./parser.",
		AcceptanceCriteria:         []string{"Parser behavior matches the fixture."},
		VerificationTarget:         "go test ./parser",
		WorkerMarginalContribution: "Own the isolated parser implementation so the supervisor only verifies it.",
		IndependentSlices:          1,
		Checkability:               "objective",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != ActionWorker || !plan.WorkerAdmissible || plan.SelectedLane.Tool != "start_worker" {
		t.Fatalf("qualified Worker was not selected: %#v", plan)
	}
	if plan.EvidenceBasis != "one_off_task_shape_heuristic" || plan.DurableDefault {
		t.Fatalf("one-off route was promoted beyond its evidence: %#v", plan)
	}

	shared, err := PlanRoute(RouteRequest{
		Objective:          "Implement two changes with a deterministic test.",
		AcceptanceCriteria: []string{"Existing behavior remains green."},
		VerificationTarget: "go test ./...",
		IndependentSlices:  2,
		SharedMutableState: true,
		Checkability:       "objective",
	})
	if err != nil {
		t.Fatal(err)
	}
	if shared.Action != ActionDirect || shared.WorkerAdmissible || len(shared.WorkerRejectionReasons) == 0 {
		t.Fatalf("shared state should keep one agent: %#v", shared)
	}
}

func TestMandatoryWorkerTopologyCannotWeakenContract(t *testing.T) {
	_, err := PlanRoute(RouteRequest{Objective: "Make a judgment call.", Topology: "worker", Checkability: "semantic"})
	if err == nil || !strings.Contains(err.Error(), "not admissible") {
		t.Fatalf("expected mandatory topology rejection, got %v", err)
	}
	plan, err := PlanRoute(RouteRequest{Objective: "Implement isolated parser and run go test.", AcceptanceCriteria: []string{"Parser tests pass."}, VerificationTarget: "go test ./parser", WorkerMarginalContribution: "Own the isolated implementation so the supervisor only verifies.", Topology: "worker", IndependentSlices: 1, Checkability: "objective"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != ActionWorker || plan.EvidenceBasis != "user_mandated_topology" {
		t.Fatalf("valid mandatory topology not preserved: %#v", plan)
	}
}

func TestHighRiskStaysWithSolAndAddsResidualReview(t *testing.T) {
	plan, err := PlanRoute(RouteRequest{Objective: "Make an irreversible architecture choice", Kind: KindHard, Risk: "high"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != ActionDirect || plan.SelectedLane.Model != "gpt-5.6-sol" {
		t.Fatalf("high risk should not fan out automatically: %#v", plan)
	}
	if !strings.Contains(plan.ResidualReview, "fresh-context") {
		t.Fatalf("missing residual semantic review: %q", plan.ResidualReview)
	}
}

func TestRouteComparisonHasNoFalseExactCostClaim(t *testing.T) {
	plan, err := PlanRoute(RouteRequest{Objective: "Implement an isolated testable parser.", AcceptanceCriteria: []string{"Parser accepts the fixture."}, VerificationTarget: "go test ./parser", WorkerMarginalContribution: "Own the isolated implementation so the supervisor only verifies.", IndependentSlices: 1, Checkability: "objective"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.AccountingUnit != "relative_resource_intensity" || len(plan.Candidates) != 3 {
		t.Fatalf("invalid route accounting: %#v", plan)
	}
	if plan.Candidates[1].Name != "cheapest_plausible_single_agent" || plan.Candidates[1].Lane.Role != "supervisor" || plan.Candidates[2].Name != "routed_route" || plan.Candidates[2].Lane.Tool != "start_worker" {
		t.Fatalf("three-route comparison conflated a Worker with the single-agent candidate: %#v", plan.Candidates)
	}
	if plan.Surface.WorkerOverrideEvidence == "" || plan.AdoptionRule == "" || plan.Factors.ExpectedRework == "" {
		t.Fatalf("surface/rework/adoption evidence missing: %#v", plan)
	}
	encoded := strings.ToLower(plan.AccountingUnit + plan.Reason)
	if strings.Contains(encoded, "$") || strings.Contains(encoded, "1x") || strings.Contains(encoded, "cheapest exact") {
		t.Fatalf("route made unsupported exact-cost claim: %s", encoded)
	}
	if len(plan.Escalation) != 4 {
		t.Fatalf("missing one-dimension escalation ladder: %#v", plan.Escalation)
	}
	for _, step := range plan.Escalation {
		if step.Trigger == "" || step.Change == "" || step.Action == "" {
			t.Fatalf("incomplete escalation step: %#v", step)
		}
	}
}

func TestCapabilityIsRepresentedAsRoutedCandidateNotSingleAgent(t *testing.T) {
	plan, err := PlanRoute(RouteRequest{Objective: "Research today's vendor announcement.", Kind: KindRealtime})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Candidates[1].Lane.Role != "supervisor" || plan.Candidates[2].Lane.Tool != "search_external" || !plan.Candidates[2].Qualified {
		t.Fatalf("capability route comparison is structurally wrong: %#v", plan.Candidates)
	}
}

func TestReadThreadWinsOverGenericURLDigestAndReportsRoutedIntensity(t *testing.T) {
	decision, err := Decide("Read https://claudex-threads.ppop.workers.dev/#/thread/thread-123 and extract the exact verification.", KindAuto, "normal", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if decision.RouteAction != ActionCapability || decision.Tool != "read_thread" || decision.Profile.ID != "glm-5.2" || decision.CostClass != "relative:lower_for_bounded_gap" {
		t.Fatalf("thread reference was misrouted or cost class regressed: %#v", decision)
	}
}

func TestFindThreadUsesZeroModelLocalCapabilityBeforeReadThread(t *testing.T) {
	decision, err := Decide("Find thread that modified src/server/index.ts in project x after:7d.", KindAuto, "normal", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if decision.Kind != KindFindThread || decision.RouteAction != ActionCapability || decision.Tool != "find_thread" || decision.Profile.ID != "gpt-5.6-sol" || decision.CostClass != "relative:minimal_zero_model_local_scan" {
		t.Fatalf("Find Thread was not routed to the local zero-model capability: %#v", decision)
	}
	if !strings.Contains(decision.Reason, "zero-model") {
		t.Fatalf("route did not expose its no-model boundary: %q", decision.Reason)
	}
}

func TestWorkerRouteRequiresFrozenAcceptanceContract(t *testing.T) {
	plan, err := PlanRoute(RouteRequest{
		Objective:         "Implement isolated parser and run go test.",
		IndependentSlices: 1,
		Checkability:      "objective",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != ActionDirect || plan.WorkerAdmissible || plan.Acceptance.Ready {
		t.Fatalf("worker route should remain direct without a frozen contract: %#v", plan)
	}
	joined := strings.Join(plan.WorkerRejectionReasons, "; ")
	if !strings.Contains(joined, "acceptance criteria") || !strings.Contains(joined, "verification target") {
		t.Fatalf("missing contract rejection evidence: %s", joined)
	}

	ready, err := PlanRoute(RouteRequest{
		Objective:                  "Implement isolated parser and run go test.",
		AcceptanceCriteria:         []string{"Parser test fixture passes.", "No unrelated files change."},
		VerificationTarget:         "go test ./parser",
		WorkerMarginalContribution: "Own the isolated implementation so the supervisor only verifies.",
		IndependentSlices:          1,
		Checkability:               "objective",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ready.Action != ActionWorker || !ready.WorkerAdmissible || !ready.Acceptance.Ready || ready.Verification == "" {
		t.Fatalf("frozen contract did not admit worker: %#v", ready)
	}
}

func TestObservedLaneFailureQuarantinesWithoutFallback(t *testing.T) {
	health := []LaneHealth{{Tool: "search_external", Status: "unavailable", FailureClass: "auth_configuration", Reason: "gateway auth failed"}}
	plan, err := PlanRoute(RouteRequest{Objective: "Research today's current vendor announcement.", Kind: KindRealtime, LaneHealth: health})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != ActionBlocked || plan.BlockedCapability != "search_external" {
		t.Fatalf("unavailable capability was not blocked: %#v", plan)
	}
	if plan.SelectedLane.Tool != "search_external" || plan.SelectedLane.Model != "grok-4.5" {
		t.Fatalf("router silently substituted the required lane: %#v", plan.SelectedLane)
	}
	if !strings.Contains(plan.StopCondition, "do not silently fallback") || len(plan.Surface.LaneHealth) != 1 {
		t.Fatalf("quarantine evidence was not exposed: %#v", plan)
	}
}

func TestUnavailableOptionalWorkerFallsBackToQualifiedSupervisorTopology(t *testing.T) {
	in := RouteRequest{
		Objective: "Implement isolated parser and run go test.", AcceptanceCriteria: []string{"Parser tests pass."},
		VerificationTarget: "go test ./parser", WorkerMarginalContribution: "Own the isolated implementation so the supervisor only verifies.", IndependentSlices: 1, Checkability: "objective",
		LaneHealth: []LaneHealth{{Tool: "start_worker", Status: "unavailable", FailureClass: "model_mismatch", Reason: "resolved wrong model"}},
	}
	plan, err := PlanRoute(in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != ActionDirect || plan.WorkerAdmissible || plan.SelectedLane.Model != "gpt-5.6-sol" {
		t.Fatalf("optional unavailable worker should keep qualified supervisor: %#v", plan)
	}
	if !strings.Contains(strings.Join(plan.WorkerRejectionReasons, "; "), "quarantined") {
		t.Fatalf("worker quarantine reason missing: %#v", plan.WorkerRejectionReasons)
	}

	in.Topology = "worker"
	if _, err := PlanRoute(in); err == nil || !strings.Contains(err.Error(), "quarantined") {
		t.Fatalf("mandatory unavailable worker should fail without substitution, got %v", err)
	}
}

func TestLegacyDecisionUsesSelectedLaneAndExplicitOverride(t *testing.T) {
	d, err := Decide("ordinary request", KindAuto, "normal", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Profile.ID != "gpt-5.6-sol" || d.RouteAction != ActionDirect || !strings.HasPrefix(d.CostClass, "relative:") {
		t.Fatalf("unexpected direct decision: %#v", d)
	}
	override, err := Decide("ordinary request", KindAuto, "normal", "glm-5.2", "")
	if err != nil {
		t.Fatal(err)
	}
	if override.Profile.ID != "glm-5.2" || override.EvidenceBasis != "user_override" {
		t.Fatalf("explicit override was not preserved: %#v", override)
	}
}

func TestRouteInputValidation(t *testing.T) {
	for _, input := range []RouteRequest{
		{},
		{Objective: "x", Risk: "critical"},
		{Objective: "x", Checkability: "objective-ish"},
		{Objective: "x", Topology: "fanout"},
		{Objective: "x", IndependentSlices: 4},
		{Objective: "x", AcceptanceCriteria: make([]string, 13)},
		{Objective: "x", VerificationTarget: strings.Repeat("x", 4001)},
		{Objective: "x", LaneHealth: []LaneHealth{{Tool: "unknown", Status: "unavailable"}}},
		{Objective: "x", LaneHealth: []LaneHealth{{Tool: "start_worker", Status: "broken"}}},
	} {
		if _, err := PlanRoute(input); err == nil {
			t.Fatalf("expected validation failure for %#v", input)
		}
	}
}
