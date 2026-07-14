package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"claudexflow/internal/claude"
	"claudexflow/internal/router"
	"claudexflow/internal/sessionbind"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	// Isolate durable lane-health + open-routes so tests do not pollute ~/.config.
	t.Setenv("CLAUDEX_LANE_HEALTH_PATH", filepath.Join(root, "lane-health.json"))
	return &Server{
		root:                   root,
		workers:                map[string]*workerState{},
		leases:                 map[string]string{},
		attemptedSlices:        map[string]int{},
		preparingSlices:        map[string]bool{},
		sliceInputs:            map[string]WorkerStartInput{},
		routes:                 map[string]*RouteRecord{},
		routeLedgerPath:        filepath.Join(root, "route-outcomes.jsonl"),
		openRoutesPathOverride: filepath.Join(root, "open-routes.json"),
		slots:                  make(chan struct{}, maxConcurrentRuns),
	}
}

func qualifiedInput() WorkerStartInput {
	return WorkerStartInput{
		SliceID:              "parser-contract",
		Objective:            "Implement the parser contract.",
		MarginalContribution: "Own the isolated parser change so the supervisor can verify instead of duplicating implementation.",
		Context:              "The parser package is already isolated.",
		OutputContract:       "Return changed paths, exact test output, and residual risk.",
		DoneCondition:        "go test ./parser passes.",
		DeadlineMS:           60_000,
	}
}

func reportResult(status string) claude.Result {
	needs := `[]`
	if status == "needs_capability" {
		needs = `[{"kind":"external_search","question":"current price?","why":"blocks the decision","urls":[]}]`
	}
	return claude.Result{
		Success:       true,
		SessionID:     "session-1",
		ResolvedModel: "grok-4.5-build-20260713",
		AuthSource:    claude.AuthGateway,
		Structured:    json.RawMessage(`{"status":"` + status + `","summary":"done","evidence":[],"changed_paths":[],"verification":["test passed"],"needs":` + needs + `}`),
		ToolUses:      map[string]int{"Read": 1},
		DurationMS:    12,
		Usage:         claude.Usage{InputTokens: 10, CacheReadTokens: 3, OutputTokens: 4},
	}
}

func TestScopedDir(t *testing.T) {
	s := newTestServer(t)
	child := filepath.Join(s.root, "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	expected, err := filepath.EvalSymlinks(child)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.scopedDir("child"); err != nil || got != expected {
		t.Fatalf("scopedDir child = %q, %v", got, err)
	}
	if _, err := s.scopedDir(".."); err == nil {
		t.Fatal("expected outside-root rejection")
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(s.root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.scopedDir("escape"); err == nil {
		t.Fatal("expected symlink escape rejection")
	}
}

func TestWriteScopeLeaseRejectsOverlap(t *testing.T) {
	s := newTestServer(t)
	w1 := &workerState{id: "worker-1", write: true, paths: []string{"internal"}}
	w2 := &workerState{id: "worker-2", write: true, paths: []string{"internal/mcpserver"}}
	if err := s.acquireLease(w1); err != nil {
		t.Fatal(err)
	}
	if err := s.acquireLease(w2); err == nil {
		t.Fatal("expected overlapping scope rejection")
	}
	s.releaseLease(w1)
	if err := s.acquireLease(w2); err != nil {
		t.Fatalf("scope should be available after release: %v", err)
	}
}

func TestContractFieldsAndGuard(t *testing.T) {
	contract := Contract()
	if contract.Version != ContractVersion || contract.WorkerProfile != "grok-4.5/high" {
		t.Fatalf("unexpected contract: %#v", contract)
	}
	wantFields := []string{"context", "deadline_ms", "done_condition", "marginal_contribution", "objective", "output_contract", "paths", "retry_reason", "route_id", "slice_id", "workdir", "write"}
	if !reflect.DeepEqual(contract.WorkerStartFields, wantFields) {
		t.Fatalf("worker fields = %v; want %v", contract.WorkerStartFields, wantFields)
	}
	path := filepath.Join(t.TempDir(), "orchestrator.md")
	validPrompt := "Contract: `" + ContractVersion + "`\n" + workerFieldMarker() + "\n"
	if err := os.WriteFile(path, []byte(validPrompt), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOrchestrator(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("Contract: `stale`\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOrchestrator(path); err == nil {
		t.Fatal("expected stale orchestrator rejection")
	}
	if err := os.WriteFile(path, []byte("Contract: `"+ContractVersion+"`\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateOrchestrator(path); err == nil {
		t.Fatal("expected missing worker-schema marker rejection")
	}
}

func TestAdmissionRejectsBeforeModelBudgetOrLease(t *testing.T) {
	s := newTestServer(t)
	calls := 0
	s.runModel = func(context.Context, claude.Request) claude.Result {
		calls++
		return reportResult("completed")
	}
	in := qualifiedInput()
	in.MarginalContribution = ""
	in.OutputContract = ""
	in.Write = true
	in.Paths = nil
	if _, _, err := s.startWorker(context.Background(), nil, in); err == nil {
		t.Fatal("expected admission rejection")
	}
	if calls != 0 || s.workerStarts.Load() != 0 || s.workerTurns.Load() != 0 || len(s.leases) != 0 || len(s.workers) != 0 {
		t.Fatalf("rejection consumed runtime resources: calls=%d starts=%d turns=%d leases=%d workers=%d", calls, s.workerStarts.Load(), s.workerTurns.Load(), len(s.leases), len(s.workers))
	}
	_, status, err := s.workflowStatus(context.Background(), nil, EmptyInput{})
	if err != nil || len(status.Slices) != 1 || status.Slices[0].State != "rejected" {
		t.Fatalf("rejection not observable: status=%#v err=%v", status, err)
	}
}

func TestQualifiedWorkerRecordsIdentityUsageAndAdmission(t *testing.T) {
	s := newTestServer(t)
	s.runModel = func(_ context.Context, req claude.Request) claude.Result {
		if req.Model != workerModel || req.Effort != workerEffort || req.AuthMode != claude.AuthGateway {
			t.Fatalf("unexpected worker request: %#v", req)
		}
		return reportResult("completed")
	}
	_, out, err := s.startWorker(context.Background(), nil, qualifiedInput())
	if err != nil {
		t.Fatal(err)
	}
	if out.Admission.Result != admissionAdmitted || out.State != "completed" || out.StartAttempts != 1 {
		t.Fatalf("unexpected worker output: %#v", out)
	}
	if out.Identity.ModelVerification != "verified" || out.Identity.RequestedModel != workerModel || out.Identity.ResolvedModel == "" || out.Identity.EffortVerification != "cli_argument_only" {
		t.Fatalf("identity was not reported honestly: %#v", out.Identity)
	}
	if out.Usage.InputTokens != 10 || out.Usage.CacheReadTokens != 3 || out.DurationMS != 12 {
		t.Fatalf("usage was not preserved: %#v", out)
	}
}

func TestWorkerRequestInheritsRootAndParentThreadBinding(t *testing.T) {
	s := newTestServer(t)
	s.parentPID = 4242
	t.Setenv("CLAUDEX_SESSION_BINDING_DIR", t.TempDir())
	if err := sessionbind.Record(s.parentPID, "root-session", s.root); err != nil {
		t.Fatal(err)
	}
	var captured claude.Request
	s.runModel = func(_ context.Context, req claude.Request) claude.Result {
		captured = req
		return reportResult("completed")
	}
	if _, _, err := s.startWorker(context.Background(), nil, qualifiedInput()); err != nil {
		t.Fatal(err)
	}
	if captured.RootSessionID != "root-session" || captured.ParentSessionID != "root-session" {
		t.Fatalf("worker lost Root/Parent binding: %#v", captured)
	}
}

func TestRouteTaskUsesLiveLaneQuarantineWithoutFallback(t *testing.T) {
	s := newTestServer(t)
	s.recordLaneFailure("search_external", failureInfo{Class: failureAuthConfiguration, Detail: "gateway auth failed"})
	_, plan, err := s.routeTask(context.Background(), nil, router.RouteRequest{Objective: "Research today's current vendor announcement.", Kind: router.KindRealtime})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != router.ActionBlocked || plan.BlockedCapability != "search_external" || plan.SelectedLane.Model != "grok-4.5" {
		t.Fatalf("live quarantine did not block the exact lane: %#v", plan)
	}
	if len(plan.Surface.LaneHealth) != 1 || plan.Surface.LaneHealth[0].FailureClass != failureAuthConfiguration {
		t.Fatalf("health evidence not exposed: %#v", plan.Surface.LaneHealth)
	}

	// Same-session healthy record restores the lane. Durable quarantine requires
	// explicit lane-health clear --canary-pass outside this process.
	s.recordLaneHealthy("search_external")
	_, recovered, err := s.routeTask(context.Background(), nil, router.RouteRequest{Objective: "Research today's current vendor announcement.", Kind: router.KindRealtime})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Action != router.ActionCapability || recovered.SelectedLane.Tool != "search_external" {
		t.Fatalf("healthy lane was not restored: %#v", recovered)
	}
}

func TestRouteLifecycleRecordsAcceptedResultOnce(t *testing.T) {
	s := newTestServer(t)
	_, plan, err := s.routeTask(context.Background(), nil, router.RouteRequest{
		Objective: "Implement isolated parser.", AcceptanceCriteria: []string{"Parser passes."},
		VerificationTarget: "go test ./parser", WorkerMarginalContribution: "Own the isolated implementation so the supervisor only verifies.", IndependentSlices: 1, Checkability: "objective",
	})
	if err != nil || plan.RouteID == "" {
		t.Fatalf("route_id missing: plan=%#v err=%v", plan, err)
	}
	if _, _, err := s.recordRouteOutcome(context.Background(), nil, RouteOutcomeInput{RouteID: plan.RouteID, Status: "accepted"}); err == nil {
		t.Fatal("accepted route without verification was recorded")
	}
	_, record, err := s.recordRouteOutcome(context.Background(), nil, RouteOutcomeInput{
		RouteID: plan.RouteID, Status: "accepted", Verification: "go test ./parser: PASS", HumanCorrection: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.State != "accepted" || record.Outcome == nil || record.Outcome.HumanCorrection != "none" {
		t.Fatalf("route outcome missing: %#v", record)
	}
	if record.LedgerStatus != "persisted" {
		t.Fatalf("terminal route was not persisted: %#v", record)
	}
	ledger, err := os.ReadFile(s.routeLedgerPath)
	if err != nil || !strings.Contains(string(ledger), plan.RouteID) {
		t.Fatalf("route ledger missing terminal record: %q err=%v", ledger, err)
	}
	if _, _, err := s.recordRouteOutcome(context.Background(), nil, RouteOutcomeInput{RouteID: plan.RouteID, Status: "failed"}); err == nil {
		t.Fatal("terminal route was closed twice")
	}
	_, status, err := s.workflowStatus(context.Background(), nil, EmptyInput{})
	if err != nil || len(status.Routes) != 1 || status.Routes[0].State != "accepted" {
		t.Fatalf("route lifecycle not observable: status=%#v err=%v", status, err)
	}
}

func TestWorkerRouteIDMustSelectWorkerTool(t *testing.T) {
	s := newTestServer(t)
	calls := 0
	s.runModel = func(context.Context, claude.Request) claude.Result {
		calls++
		return reportResult("completed")
	}
	_, direct, err := s.routeTask(context.Background(), nil, router.RouteRequest{Objective: "Fix a localized parser bug."})
	if err != nil {
		t.Fatal(err)
	}
	in := qualifiedInput()
	in.RouteID = direct.RouteID
	if _, _, err := s.startWorker(context.Background(), nil, in); err == nil || !strings.Contains(err.Error(), "selected") {
		t.Fatalf("mismatched route/tool binding was not rejected: %v", err)
	}
	if calls != 0 {
		t.Fatalf("route mismatch invoked Worker model %d time(s)", calls)
	}
}

func TestRuntimeLaneEvidenceOverridesCallerClaim(t *testing.T) {
	s := newTestServer(t)
	s.recordLaneFailure("start_worker", failureInfo{Class: failureModelMismatch, Detail: "resolved another model"})
	in := router.RouteRequest{
		Objective: "Implement isolated parser and run go test.", AcceptanceCriteria: []string{"Parser passes."},
		VerificationTarget: "go test ./parser", WorkerMarginalContribution: "Own the isolated implementation so the supervisor only verifies.", IndependentSlices: 1, Checkability: "objective",
		LaneHealth: []router.LaneHealth{{Tool: "start_worker", Status: "healthy"}},
	}
	_, plan, err := s.routeTask(context.Background(), nil, in)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != router.ActionDirect || plan.WorkerAdmissible {
		t.Fatalf("caller overrode runtime quarantine: %#v", plan)
	}
	_, status, err := s.workflowStatus(context.Background(), nil, EmptyInput{})
	if err != nil || len(status.LaneHealth) != 1 || status.LaneHealth[0].Status != "unavailable" {
		t.Fatalf("workflow status omitted lane health: status=%#v err=%v", status, err)
	}
}

func TestRetryableAuthFailureAllowsOneIdenticalSameLaneRetry(t *testing.T) {
	s := newTestServer(t)
	calls := 0
	s.runModel = func(context.Context, claude.Request) claude.Result {
		calls++
		if calls == 1 {
			return claude.Result{AuthSource: claude.AuthGateway, ToolUses: map[string]int{}, ExitError: "authentication failed", Stderr: "connectors are disabled"}
		}
		return reportResult("completed")
	}
	in := qualifiedInput()
	if _, _, err := s.startWorker(context.Background(), nil, in); err == nil || !strings.Contains(err.Error(), "retry_eligible=true") {
		t.Fatalf("expected classified retryable failure, got %v", err)
	}
	_, status, _ := s.workflowStatus(context.Background(), nil, EmptyInput{})
	if len(status.Workers) != 1 || !status.Workers[0].RetryEligible || status.Workers[0].FailureClass != failureAuthConfiguration {
		t.Fatalf("first failure not observable: %#v", status.Workers)
	}
	in.RetryReason = "Removed the conflicting API-key environment from the worker runtime."
	_, out, err := s.startWorker(context.Background(), nil, in)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "completed" || out.StartAttempts != 2 || calls != 2 || s.workerStarts.Load() != 2 {
		t.Fatalf("retry did not complete as one same-lane attempt: out=%#v calls=%d", out, calls)
	}
	if _, _, err := s.startWorker(context.Background(), nil, in); err == nil {
		t.Fatal("expected third start rejection")
	}
	if calls != 2 {
		t.Fatalf("rejected third start invoked model: %d calls", calls)
	}
}

func TestRetryRejectsChangedSliceWithoutModelCall(t *testing.T) {
	s := newTestServer(t)
	calls := 0
	s.runModel = func(context.Context, claude.Request) claude.Result {
		calls++
		return claude.Result{AuthSource: claude.AuthGateway, ToolUses: map[string]int{}, ExitError: "authentication failed"}
	}
	in := qualifiedInput()
	_, _, _ = s.startWorker(context.Background(), nil, in)
	in.RetryReason = "Auth repaired."
	in.Objective = "Broadened replacement objective."
	if _, _, err := s.startWorker(context.Background(), nil, in); err == nil || !strings.Contains(err.Error(), "retry must keep") {
		t.Fatalf("expected changed-slice retry rejection, got %v", err)
	}
	if calls != 1 || s.workerStarts.Load() != 1 {
		t.Fatalf("changed retry consumed a model call: calls=%d starts=%d", calls, s.workerStarts.Load())
	}
}

func TestNonRetryableFailureCannotRestart(t *testing.T) {
	s := newTestServer(t)
	calls := 0
	s.runModel = func(context.Context, claude.Request) claude.Result {
		calls++
		return claude.Result{AuthSource: claude.AuthGateway, ToolUses: map[string]int{"Read": 1}, ExitError: "exit status 1", Stderr: "compile failed"}
	}
	in := qualifiedInput()
	if _, _, err := s.startWorker(context.Background(), nil, in); err == nil || strings.Contains(err.Error(), "retry_eligible=true") {
		t.Fatalf("expected non-retryable failure, got %v", err)
	}
	in.RetryReason = "Try again."
	if _, _, err := s.startWorker(context.Background(), nil, in); err == nil {
		t.Fatal("expected non-retryable slice restart rejection")
	}
	if calls != 1 {
		t.Fatalf("non-retryable restart invoked model: %d calls", calls)
	}
}

func TestResumeUsesOriginalWorkerAndSession(t *testing.T) {
	s := newTestServer(t)
	calls := 0
	s.runModel = func(_ context.Context, req claude.Request) claude.Result {
		calls++
		if calls == 1 {
			return reportResult("needs_capability")
		}
		if req.ResumeSession != "session-1" {
			t.Fatalf("resume session = %q", req.ResumeSession)
		}
		return reportResult("completed")
	}
	_, first, err := s.startWorker(context.Background(), nil, qualifiedInput())
	if err != nil {
		t.Fatal(err)
	}
	_, resumed, err := s.resumeWorker(context.Background(), nil, WorkerResumeInput{WorkerID: first.WorkerID, EvidencePacket: "source: https://example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.WorkerID != first.WorkerID || resumed.SessionID != first.SessionID || resumed.Turn != 2 || resumed.State != "completed" {
		t.Fatalf("resume lost worker continuity: first=%#v resumed=%#v", first, resumed)
	}
	if calls != 2 || s.workerStarts.Load() != 1 || s.workerTurns.Load() != 2 {
		t.Fatalf("resume accounting wrong: calls=%d starts=%d turns=%d", calls, s.workerStarts.Load(), s.workerTurns.Load())
	}
	if resumed.Usage.InputTokens != 20 || resumed.Usage.CacheReadTokens != 6 || resumed.Usage.OutputTokens != 8 || resumed.DurationMS != 24 || resumed.ToolUses["Read"] != 2 {
		t.Fatalf("worker turn usage was not accumulated exactly once: %#v", resumed)
	}
}

func TestRouteDiagnosticsCountChildCallsWithoutSupervisorEstimates(t *testing.T) {
	s := newTestServer(t)
	s.runModel = func(context.Context, claude.Request) claude.Result { return reportResult("completed") }
	_, plan, err := s.routeTask(context.Background(), nil, router.RouteRequest{
		Objective: "Implement isolated parser.", AcceptanceCriteria: []string{"Parser passes."},
		VerificationTarget: "go test ./parser", WorkerMarginalContribution: "Own the isolated implementation so the supervisor only verifies.", IndependentSlices: 1, Checkability: "objective",
	})
	if err != nil {
		t.Fatal(err)
	}
	in := qualifiedInput()
	in.RouteID = plan.RouteID
	if _, _, err := s.startWorker(context.Background(), nil, in); err != nil {
		t.Fatal(err)
	}
	_, record, err := s.recordRouteOutcome(context.Background(), nil, RouteOutcomeInput{RouteID: plan.RouteID, Status: "accepted", Verification: "go test ./parser: PASS", HumanCorrection: "none"})
	if err != nil {
		t.Fatal(err)
	}
	d := record.Diagnostics
	if d.Calls != 1 || d.WorkerStarts != 1 || d.WorkerResumes != 0 || d.SpecialistCalls != 0 || d.Usage.InputTokens != 10 || d.DurationMS != 12 {
		t.Fatalf("route child accounting is wrong: %#v", d)
	}
	if d.SupervisorIncluded || d.ComparableSpend || d.RequestedModels[workerModel] != 1 || d.ResolvedModels["grok-4.5-build-20260713"] != 1 {
		t.Fatalf("route accounting boundary is dishonest or incomplete: %#v", d)
	}
}

func TestReadThreadUsesSanitizedLocalContextAndGLM(t *testing.T) {
	s := newTestServer(t)
	s.transcriptRoot = filepath.Join(s.root, "transcripts")
	project := filepath.Join(s.transcriptRoot, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","sessionId":"thread-123","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"api_key=00000000000000000000000000000000.TESTFIXTURE00000000 parser requirement"}}`,
		`{"type":"assistant","uuid":"a1","sessionId":"thread-123","timestamp":"2026-01-01T00:01:00Z","message":{"role":"assistant","model":"gpt-5.6-sol","content":[{"type":"text","text":"Verified with go test ./parser"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(project, "thread-123.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	s.runModel = func(_ context.Context, req claude.Request) claude.Result {
		if req.Model != threadReaderModel || req.Effort != "" || req.Role != "read_thread" || !reflect.DeepEqual(req.Tools, []string{"StructuredOutput"}) {
			t.Fatalf("unexpected Read Thread request: %#v", req)
		}
		if strings.Contains(req.Prompt, "505ba8") || !strings.Contains(req.Prompt, "[REDACTED]") || !strings.Contains(req.Prompt, "go test ./parser") || !strings.Contains(req.Prompt, "under 2500 characters") || !strings.Contains(req.Prompt, "tool call records an attempted action") || !strings.Contains(req.Prompt, "revise, supersede, revert, or contradict") {
			t.Fatalf("Read Thread prompt leaked a secret or lost exact evidence: %s", req.Prompt)
		}
		return claude.Result{
			Success: true, SessionID: "glm-thread-session", ResolvedModel: "glm-5.2", AuthSource: claude.AuthGateway,
			Structured: json.RawMessage(`{"status":"completed","summary":"found verification","items":[{"claim":"parser verified","source":"thread://thread-123#a1","detail":"go test ./parser"}],"open_questions":[]}`),
			ToolUses:   map[string]int{"StructuredOutput": 1}, DurationMS: 20, Usage: claude.Usage{InputTokens: 100, OutputTokens: 20},
		}
	}
	_, plan, err := s.routeTask(context.Background(), nil, router.RouteRequest{Objective: "Read prior thread and extract verification.", Kind: router.KindReadThread})
	if err != nil {
		t.Fatal(err)
	}
	_, out, err := s.readThread(context.Background(), nil, ThreadReadInput{RouteID: plan.RouteID, ThreadID: "thread-123", Question: "What exact command verified the parser?", MaxSourceBytes: 16 * 1024})
	if err != nil {
		t.Fatal(err)
	}
	if out.Identity.ModelVerification != "verified" || out.ThreadSource == nil || out.ThreadSource.ThreadID != "thread-123" || out.ThreadSource.SelectedEvents != 2 || out.Usage.InputTokens != 100 {
		t.Fatalf("Read Thread output is incomplete: %#v", out)
	}
	_, status, err := s.workflowStatus(context.Background(), nil, EmptyInput{})
	if err != nil || status.ThreadReadCalls != 1 || status.Routes[0].Diagnostics.SpecialistCalls != 1 {
		t.Fatalf("Read Thread accounting is missing: status=%#v err=%v", status, err)
	}
}

func TestFindThreadUsesLocalSanitizedIndexWithoutModelCall(t *testing.T) {
	s := newTestServer(t)
	s.transcriptRoot = filepath.Join(s.root, "transcripts")
	project := filepath.Join(s.transcriptRoot, "-Users-test-project-x")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	transcript := strings.Join([]string{
		`{"type":"user","uuid":"u1","sessionId":"thread-find","timestamp":"2026-07-13T00:00:00Z","cwd":"/Users/test/project/x","message":{"role":"user","content":"api_key=00000000000000000000000000000000.TESTFIXTURE00000000 implement usage ledger"}}`,
		`{"type":"assistant","uuid":"a1","sessionId":"thread-find","timestamp":"2026-07-13T00:01:00Z","cwd":"/Users/test/project/x","message":{"role":"assistant","model":"gpt-5.6-sol","content":[{"type":"tool_use","id":"tool-1","name":"Edit","input":{"file_path":"thread-app/src/usage.ts","new_string":"done"}}]}}`,
		`{"type":"ai-title","aiTitle":"Usage ledger implementation","sessionId":"thread-find"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(project, "thread-find.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	s.runModel = func(context.Context, claude.Request) claude.Result {
		modelCalls++
		return claude.Result{}
	}
	_, plan, err := s.routeTask(context.Background(), nil, router.RouteRequest{Objective: "Find thread that changed the usage ledger.", Kind: router.KindFindThread})
	if err != nil {
		t.Fatal(err)
	}
	_, out, err := s.findThread(context.Background(), nil, ThreadFindInput{RouteID: plan.RouteID, Query: `"usage ledger" file:thread-app/src/usage.ts project:x`})
	if err != nil {
		t.Fatal(err)
	}
	if modelCalls != 0 || len(out.Result.Matches) != 1 || out.Result.Matches[0].ThreadID != "thread-find" || !strings.Contains(out.NextAction, "read_thread") {
		t.Fatalf("Find Thread output/model boundary wrong: calls=%d out=%#v", modelCalls, out)
	}
	encoded, _ := json.Marshal(out)
	if strings.Contains(string(encoded), "505ba8") || !strings.Contains(string(encoded), "[REDACTED]") {
		t.Fatalf("Find Thread leaked raw transcript data: %s", encoded)
	}
	_, status, err := s.workflowStatus(context.Background(), nil, EmptyInput{})
	if err != nil || status.ThreadFindCalls != 1 || status.Routes[0].Diagnostics.Calls != 0 {
		t.Fatalf("Find Thread accounting is wrong: status=%#v err=%v", status, err)
	}
}

func TestInvalidFindThreadQueryConsumesNoScanBudget(t *testing.T) {
	s := newTestServer(t)
	_, plan, err := s.routeTask(context.Background(), nil, router.RouteRequest{Objective: "Find thread for prior work.", Kind: router.KindFindThread})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.findThread(context.Background(), nil, ThreadFindInput{RouteID: plan.RouteID}); err == nil {
		t.Fatal("expected empty query rejection")
	}
	if s.threadFindCalls.Load() != 0 {
		t.Fatalf("invalid query consumed scan budget: %d", s.threadFindCalls.Load())
	}
}

func TestReportBoundsRejectContextBloatAndMultiNeedFanout(t *testing.T) {
	tooManyNeeds := reportResult("needs_capability")
	tooManyNeeds.Structured = json.RawMessage(`{"status":"needs_capability","summary":"need two things","evidence":[],"changed_paths":[],"verification":[],"needs":[{"kind":"external_search","question":"q1","why":"w1","urls":[]},{"kind":"repo_explore","question":"q2","why":"w2","urls":[]}]}`)
	if _, err := decodeWorkerReport(tooManyNeeds); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("multi-need worker fan-out was not rejected: %v", err)
	}
	oversized := reportResult("completed")
	oversized.Structured = json.RawMessage(`{"status":"completed","summary":"` + strings.Repeat("x", maxWorkerReportBytes) + `","evidence":[],"changed_paths":[],"verification":[],"needs":[]}`)
	if _, err := decodeWorkerReport(oversized); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized worker report was not rejected: %v", err)
	}
}

func TestModelMismatchAndScopeViolationAreObservableAndNotRetryable(t *testing.T) {
	t.Run("model mismatch", func(t *testing.T) {
		s := newTestServer(t)
		s.runModel = func(context.Context, claude.Request) claude.Result {
			result := reportResult("completed")
			result.ResolvedModel = "claude-opus-4-8"
			return result
		}
		_, out, err := s.startWorker(context.Background(), nil, qualifiedInput())
		if err != nil {
			t.Fatal(err)
		}
		if out.State != "model_mismatch" || out.FailureClass != failureModelMismatch || out.RetryEligible {
			t.Fatalf("model mismatch not enforced: %#v", out)
		}
		if _, _, err := s.resumeWorker(context.Background(), nil, WorkerResumeInput{WorkerID: out.WorkerID, EvidencePacket: "continue"}); err == nil {
			t.Fatal("expected mismatched model resume rejection")
		}
	})
	t.Run("scope violation", func(t *testing.T) {
		s := newTestServer(t)
		s.runModel = func(context.Context, claude.Request) claude.Result {
			result := reportResult("completed")
			result.ChangedPaths = []string{"outside/file.go"}
			return result
		}
		in := qualifiedInput()
		in.Write = true
		in.Paths = []string{"internal/threadusage/parse.go"}
		_, out, err := s.startWorker(context.Background(), nil, in)
		if err != nil {
			t.Fatal(err)
		}
		if out.State != "blocked" || out.FailureClass != failureScopeViolation || !strings.Contains(out.Error, "outside owned scopes") {
			t.Fatalf("scope violation not enforced: %#v", out)
		}
	})
}

func TestWorkerBriefDefinesCompactCapabilityPacket(t *testing.T) {
	in := qualifiedInput()
	in.Write = true
	in.Paths = []string{"parser"}
	got := workerStartBrief(in)
	for _, want := range []string{"Grok 4.5 high worker", "Slice ID: parser-contract", "Marginal contribution", "Output contract", "external_search", "url_digest", "repo_explore", "StructuredOutput", "go test ./parser"} {
		if !strings.Contains(got, want) {
			t.Fatalf("brief missing %q", want)
		}
	}
}

func TestProfilesMatchWorkflow(t *testing.T) {
	if workerModel != "grok-4.5" || workerEffort != "high" {
		t.Fatalf("unexpected worker profile %s/%s", workerModel, workerEffort)
	}
	if grokExternal.model != "grok-4.5" || grokExternal.effort != "high" {
		t.Fatalf("unexpected Grok profile %#v", grokExternal)
	}
	if geminiURLs.model != "gemini-3.5-flash" || len(geminiURLs.tools) != 1 || geminiURLs.tools[0] != "WebFetch" {
		t.Fatalf("unexpected Gemini profile %#v", geminiURLs)
	}
	if terraRepo.model != "gpt-5.6-terra" {
		t.Fatalf("unexpected Terra profile %#v", terraRepo)
	}
}

func TestNativeClaudeModels(t *testing.T) {
	tests := map[string]string{"": "opus", "opus": "opus", "sonnet": "sonnet", "sonnet[1m]": "sonnet[1m]", "fable": "fable", "claude-fable-5": "fable"}
	for input, want := range tests {
		got, err := nativeClaudeModel(input)
		if err != nil || got != want {
			t.Fatalf("nativeClaudeModel(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := nativeClaudeModel("grok-4.5"); err == nil {
		t.Fatal("expected non-Claude model rejection")
	}
}

func TestDecodeWorkerReport(t *testing.T) {
	raw := json.RawMessage(`{"status":"needs_capability","summary":"need current vendor data","evidence":[],"changed_paths":[],"verification":[],"needs":[{"kind":"external_search","question":"current price?","why":"blocks decision","urls":[]}]}`)
	report, err := decodeWorkerReport(claude.Result{Structured: raw})
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "needs_capability" || len(report.Needs) != 1 || report.Needs[0].Kind != "external_search" {
		t.Fatalf("unexpected report %#v", report)
	}
}

func TestSchemasAreValidJSON(t *testing.T) {
	if !json.Valid([]byte(workerJSONSchema)) || !json.Valid([]byte(evidenceJSONSchema)) {
		t.Fatal("structured output schemas must be valid JSON")
	}
}
