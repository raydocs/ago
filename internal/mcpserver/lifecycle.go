package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"claudexflow/internal/claude"
	"claudexflow/internal/router"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) registerRoute(plan router.Plan, fingerprint string) router.Plan {
	rootSessionID, parentSessionID := s.threadBinding()
	s.mu.Lock()
	if s.routes == nil {
		s.routes = map[string]*RouteRecord{}
	}
	for _, existing := range s.routes {
		if existing.State == "open" && existing.RequestFingerprint == fingerprint && existing.RootSessionID == rootSessionID && existing.ParentSessionID == parentSessionID && existing.WorkDir == s.root {
			out := existing.Plan
			s.mu.Unlock()
			return out
		}
	}
	sequence := s.nextRoute.Add(1)
	plan.RouteID = fmt.Sprintf("route-%d-%d", time.Now().UnixMilli(), sequence)
	record := &RouteRecord{
		RouteID: plan.RouteID, State: "open", Plan: plan,
		CreatedAt:          time.Now().UTC().Format(time.RFC3339Nano),
		RequestFingerprint: fingerprint,
		RootSessionID:      rootSessionID,
		ParentSessionID:    parentSessionID,
		WorkDir:            s.root,
		Diagnostics: RouteDiagnostics{
			Coverage:       "child model calls observed by claudex-flow only; Claude Code Supervisor and tools outside claudex-flow are excluded",
			AccountingUnit: "relative_resource_intensity", SupervisorIncluded: false, ComparableSpend: false,
			RequestedModels: map[string]int{}, ResolvedModels: map[string]int{}, ToolUses: map[string]int{},
		},
		Integration:  RouteIntegrationState{Slices: map[string]RouteIntegrationSlice{}},
		LedgerStatus: "pending",
	}
	s.routes[record.RouteID] = record
	snapshot := cloneRouteRecord(*record)
	s.mu.Unlock()
	// Durable open index: survives MCP process restart (claudex --resume).
	s.persistOpenRoute(snapshot)
	return plan
}

// prepareWorkerRoute binds the launch packet to the zero-model route decision.
// It makes caller estimates advisory and prevents a start call from silently
// inflating ROI, fan-out, or deadline after route_task returned.
func (s *Server) prepareWorkerRoute(in *WorkerStartInput) error {
	if strings.TrimSpace(in.RouteID) == "" {
		if s.strictWorkerRoutes {
			return fmt.Errorf("route_id is required in strict workflow mode; call route_task once, then use its bounded worker policy")
		}
		return nil
	}
	if err := s.validateRouteTool(in.RouteID, "start_worker"); err != nil {
		return err
	}
	record := s.lookupRoute(in.RouteID)
	if record == nil {
		return fmt.Errorf("unknown route_id %q", in.RouteID)
	}
	policy := record.Plan.WorkerPolicy
	if policy.MaxWorkerStarts < 1 {
		return fmt.Errorf("route_id %q does not admit Worker starts", in.RouteID)
	}
	if in.EstimatedWorkerSeconds == 0 {
		in.EstimatedWorkerSeconds = policy.EstimatedWorkerSeconds
	}
	if in.EstimatedParallelSavings == 0 {
		in.EstimatedParallelSavings = policy.EstimatedParallelSavings
	}
	if in.EstimatedWorkerSeconds != policy.EstimatedWorkerSeconds || in.EstimatedParallelSavings != policy.EstimatedParallelSavings {
		return fmt.Errorf("Worker estimate packet must match route_task exactly; route worker=%ds savings=%ds, start worker=%ds savings=%ds", policy.EstimatedWorkerSeconds, policy.EstimatedParallelSavings, in.EstimatedWorkerSeconds, in.EstimatedParallelSavings)
	}
	if strings.TrimSpace(in.MarginalContribution) == "" {
		in.MarginalContribution = record.Plan.Reason
	}
	if strings.TrimSpace(in.EstimateBasis) == "" {
		in.EstimateBasis = "inherited from frozen route policy: " + record.Plan.EvidenceBasis
	}
	if strings.TrimSpace(in.OutputContract) == "" {
		in.OutputContract = "Return status, changed paths, verifier status, and residual risk in at most 12 non-empty lines."
	}
	if strings.TrimSpace(in.DoneCondition) == "" {
		// The route verifier evaluates the integrated parent result and is never
		// automatically inherited by one slice. Running it inside every Worker
		// duplicates cost and often fails while sibling modules are unfinished.
		// A caller may still provide a genuinely narrower exact slice verifier.
		in.DoneCondition = "Complete only the owned slice and report verification as Root-owned. The Supervisor retains the integrated route verifier."
	}
	if in.DeadlineMS == 0 {
		in.DeadlineMS = policy.MaxWorkerDeadlineMS
	}
	if in.DeadlineMS > policy.MaxWorkerDeadlineMS {
		return fmt.Errorf("deadline_ms exceeds route cap: got %d, max %d", in.DeadlineMS, policy.MaxWorkerDeadlineMS)
	}
	return nil
}

// lookupRoute returns an open/terminal route from memory, hydrating from durable
// open-routes index once if missing (resume across MCP processes).
func (s *Server) lookupRoute(routeID string) *RouteRecord {
	routeID = strings.TrimSpace(routeID)
	if routeID == "" {
		return nil
	}
	s.mu.Lock()
	rec := s.routes[routeID]
	s.mu.Unlock()
	if rec != nil {
		return rec
	}
	s.loadOpenRoutesIntoMemory()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.routes[routeID]
}

func (s *Server) recordRouteModelCall(routeID, role, requestedModel, requestedEffort string, result claude.Result, sameLaneRetry bool) {
	routeID = strings.TrimSpace(routeID)
	if routeID == "" {
		return
	}
	identity := executionIdentity(requestedModel, requestedEffort, result)
	s.mu.Lock()
	defer s.mu.Unlock()
	record := s.routes[routeID]
	if record == nil {
		return
	}
	d := &record.Diagnostics
	if d.RequestedModels == nil {
		d.RequestedModels = map[string]int{}
	}
	if d.ResolvedModels == nil {
		d.ResolvedModels = map[string]int{}
	}
	if d.ToolUses == nil {
		d.ToolUses = map[string]int{}
	}
	d.Calls++
	if !result.Success || identity.ModelVerification == "mismatch" {
		d.FailedCalls++
	}
	switch role {
	case "worker_start":
		d.WorkerStarts++
	case "worker_resume":
		d.WorkerResumes++
	default:
		d.SpecialistCalls++
	}
	if sameLaneRetry {
		d.Retries++
	}
	d.RequestedModels[requestedModel]++
	if result.ResolvedModel != "" {
		d.ResolvedModels[result.ResolvedModel]++
	}
	for name, count := range result.ToolUses {
		d.ToolUses[name] += count
	}
	d.Usage = addTokenUsage(d.Usage, tokenUsage(result.Usage))
	d.DurationMS += result.DurationMS
}

func (s *Server) validateRouteTool(routeID, tool string) error {
	routeID = strings.TrimSpace(routeID)
	if routeID == "" {
		return nil
	}
	record := s.lookupRoute(routeID)
	if record == nil {
		return fmt.Errorf("unknown route_id %q", routeID)
	}
	s.mu.Lock()
	state := record.State
	plan := record.Plan
	s.mu.Unlock()
	if state != "open" {
		return fmt.Errorf("route_id %q is %s and cannot launch another selected capability", routeID, state)
	}
	if plan.Action == router.ActionBlocked {
		return fmt.Errorf("route_id %q is capability_blocked; repair the lane instead of executing or substituting it", routeID)
	}
	if plan.SelectedLane.Tool != tool {
		return fmt.Errorf("route_id %q selected %q, not %q", routeID, plan.SelectedLane.Tool, tool)
	}
	return nil
}

func (s *Server) recordRouteOutcome(_ context.Context, _ *mcp.CallToolRequest, in RouteOutcomeInput) (*mcp.CallToolResult, RouteOutcomeReceipt, error) {
	in.RouteID = strings.TrimSpace(in.RouteID)
	in.Status = strings.TrimSpace(in.Status)
	in.Verification = strings.TrimSpace(in.Verification)
	in.HumanCorrection = strings.TrimSpace(in.HumanCorrection)
	in.ResidualRisk = strings.TrimSpace(in.ResidualRisk)
	if in.RouteID == "" {
		return nil, RouteOutcomeReceipt{}, fmt.Errorf("route_id is required")
	}
	if in.Status != "accepted" && in.Status != "failed" && in.Status != "abandoned" {
		return nil, RouteOutcomeReceipt{}, fmt.Errorf("status must be accepted, failed, or abandoned")
	}
	if in.Status == "accepted" && in.Verification == "" {
		return nil, RouteOutcomeReceipt{}, fmt.Errorf("accepted outcome requires concrete verification evidence")
	}
	if in.HumanCorrection == "" {
		in.HumanCorrection = "unknown"
	}
	if in.HumanCorrection != "unknown" && in.HumanCorrection != "none" && in.HumanCorrection != "minor" && in.HumanCorrection != "major" {
		return nil, RouteOutcomeReceipt{}, fmt.Errorf("human_correction must be unknown, none, minor, or major")
	}
	if len(in.Verification) > 8000 || len(in.ResidualRisk) > 4000 {
		return nil, RouteOutcomeReceipt{}, fmt.Errorf("route outcome evidence exceeds bounded field limits")
	}

	record := s.lookupRoute(in.RouteID)
	if record == nil {
		return nil, RouteOutcomeReceipt{}, fmt.Errorf("unknown route_id %q", in.RouteID)
	}
	s.mu.Lock()
	if record.State != "open" {
		s.mu.Unlock()
		return nil, RouteOutcomeReceipt{}, fmt.Errorf("route_id %q already has terminal state %s", in.RouteID, record.State)
	}
	if in.Status == "accepted" && record.Plan.Action == router.ActionBlocked {
		s.mu.Unlock()
		return nil, RouteOutcomeReceipt{}, fmt.Errorf("capability_blocked route cannot be accepted without a new repaired route")
	}
	if in.Status == "accepted" && record.Plan.Action == router.ActionWorker {
		integration := record.Integration
		switch {
		case integration.Required == 0:
			s.mu.Unlock()
			return nil, RouteOutcomeReceipt{}, fmt.Errorf("bounded_worker route cannot be accepted before an admitted Worker integration is recorded")
		case integration.Completed != integration.Required:
			s.mu.Unlock()
			return nil, RouteOutcomeReceipt{}, fmt.Errorf("bounded_worker route integration incomplete: completed=%d required=%d; collect every background Worker exactly once", integration.Completed, integration.Required)
		case integration.Passed != integration.Required:
			s.mu.Unlock()
			return nil, RouteOutcomeReceipt{}, fmt.Errorf("bounded_worker route integration failed hygiene checks: passed=%d required=%d; repair only failing slices and re-run their scoped diff check", integration.Passed, integration.Required)
		}
	}
	record.State = in.Status
	record.Outcome = &RouteOutcome{
		Status: in.Status, Verification: in.Verification, HumanCorrection: in.HumanCorrection,
		ResidualRisk: in.ResidualRisk, RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	snapshot := cloneRouteRecord(*record)
	s.mu.Unlock()

	ledgerStatus, ledgerError := s.appendRouteRecord(snapshot)
	s.mu.Lock()
	if current := s.routes[in.RouteID]; current != nil {
		current.LedgerStatus = ledgerStatus
		current.LedgerError = ledgerError
		snapshot = cloneRouteRecord(*current)
	}
	s.mu.Unlock()
	// Terminal: drop from open durable index (ledger keeps the history).
	s.dropOpenRoute(in.RouteID)
	return nil, compactRouteOutcomeReceipt(snapshot), nil
}

func (s *Server) recordRouteIntegrationStart(routeID, sliceID string) {
	routeID, sliceID = strings.TrimSpace(routeID), strings.TrimSpace(sliceID)
	if routeID == "" || sliceID == "" {
		return
	}
	s.mu.Lock()
	record := s.routes[routeID]
	if record == nil || record.State != "open" {
		s.mu.Unlock()
		return
	}
	if record.Integration.Slices == nil {
		record.Integration.Slices = map[string]RouteIntegrationSlice{}
	}
	if _, exists := record.Integration.Slices[sliceID]; !exists {
		record.Integration.Slices[sliceID] = RouteIntegrationSlice{State: "pending"}
		recalculateRouteIntegration(&record.Integration)
	}
	snapshot := cloneRouteRecord(*record)
	s.mu.Unlock()
	s.persistOpenRoute(snapshot)
}

func (s *Server) recordRouteIntegrationResult(routeID, sliceID string, digest *IntegrationDigest) {
	routeID, sliceID = strings.TrimSpace(routeID), strings.TrimSpace(sliceID)
	if routeID == "" || sliceID == "" || digest == nil {
		return
	}
	s.mu.Lock()
	record := s.routes[routeID]
	if record == nil || record.State != "open" {
		s.mu.Unlock()
		return
	}
	if record.Integration.Slices == nil {
		record.Integration.Slices = map[string]RouteIntegrationSlice{}
	}
	state := "failed"
	if digest.DiffCheck == "pass" {
		state = "passed"
	}
	record.Integration.Slices[sliceID] = RouteIntegrationSlice{
		State: state, DiffCheck: digest.DiffCheck, AutoFixed: digest.AutoFixed, ArtifactPath: digest.ArtifactPath,
	}
	recalculateRouteIntegration(&record.Integration)
	snapshot := cloneRouteRecord(*record)
	s.mu.Unlock()
	s.persistOpenRoute(snapshot)
}

func recalculateRouteIntegration(integration *RouteIntegrationState) {
	integration.Required, integration.Completed, integration.Passed, integration.AutoFixed = 0, 0, 0, 0
	for _, slice := range integration.Slices {
		integration.Required++
		if slice.State != "pending" {
			integration.Completed++
		}
		if slice.State == "passed" {
			integration.Passed++
		}
		if slice.AutoFixed {
			integration.AutoFixed++
		}
	}
}

func compactRouteOutcomeReceipt(record RouteRecord) RouteOutcomeReceipt {
	return RouteOutcomeReceipt{
		RouteID: record.RouteID, State: record.State, Outcome: record.Outcome,
		Integration: record.Integration, Diagnostics: record.Diagnostics,
		LedgerStatus: record.LedgerStatus, LedgerError: record.LedgerError,
	}
}

func defaultRouteLedgerPath() string {
	if value := strings.TrimSpace(os.Getenv("CLAUDEX_ROUTE_LEDGER_PATH")); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "claudex", "route-outcomes.jsonl")
}

func (s *Server) appendRouteRecord(record RouteRecord) (string, string) {
	path := strings.TrimSpace(s.routeLedgerPath)
	if path == "" {
		return "disabled", ""
	}
	record.LedgerStatus = "persisted"
	record.LedgerError = ""
	raw, err := json.Marshal(record)
	if err != nil {
		return "failed", err.Error()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "failed", err.Error()
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "failed", err.Error()
	}
	defer file.Close()
	if _, err := file.Write(append(raw, '\n')); err != nil {
		return "failed", err.Error()
	}
	if err := file.Sync(); err != nil {
		return "failed", err.Error()
	}
	return "persisted", ""
}

func cloneRouteRecord(record RouteRecord) RouteRecord {
	if record.Outcome != nil {
		outcome := *record.Outcome
		record.Outcome = &outcome
	}
	record.Diagnostics.RequestedModels = cloneStringIntMap(record.Diagnostics.RequestedModels)
	record.Diagnostics.ResolvedModels = cloneStringIntMap(record.Diagnostics.ResolvedModels)
	record.Diagnostics.ToolUses = cloneStringIntMap(record.Diagnostics.ToolUses)
	if len(record.Integration.Slices) > 0 {
		slices := make(map[string]RouteIntegrationSlice, len(record.Integration.Slices))
		for key, value := range record.Integration.Slices {
			slices[key] = value
		}
		record.Integration.Slices = slices
	}
	return record
}

func cloneStringIntMap(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
