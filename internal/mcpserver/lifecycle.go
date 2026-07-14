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

func (s *Server) registerRoute(plan router.Plan) router.Plan {
	sequence := s.nextRoute.Add(1)
	plan.RouteID = fmt.Sprintf("route-%d-%d", time.Now().UnixMilli(), sequence)
	record := &RouteRecord{
		RouteID: plan.RouteID, State: "open", Plan: plan,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Diagnostics: RouteDiagnostics{
			Coverage:       "child model calls observed by claudex-flow only; Claude Code Supervisor and tools outside claudex-flow are excluded",
			AccountingUnit: "relative_resource_intensity", SupervisorIncluded: false, ComparableSpend: false,
			RequestedModels: map[string]int{}, ResolvedModels: map[string]int{}, ToolUses: map[string]int{},
		},
		LedgerStatus: "pending",
	}
	s.mu.Lock()
	if s.routes == nil {
		s.routes = map[string]*RouteRecord{}
	}
	s.routes[record.RouteID] = record
	s.mu.Unlock()
	return plan
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
	s.mu.Lock()
	record := s.routes[routeID]
	if record == nil {
		s.mu.Unlock()
		return fmt.Errorf("unknown route_id %q", routeID)
	}
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

func (s *Server) recordRouteOutcome(_ context.Context, _ *mcp.CallToolRequest, in RouteOutcomeInput) (*mcp.CallToolResult, RouteRecord, error) {
	in.RouteID = strings.TrimSpace(in.RouteID)
	in.Status = strings.TrimSpace(in.Status)
	in.Verification = strings.TrimSpace(in.Verification)
	in.HumanCorrection = strings.TrimSpace(in.HumanCorrection)
	in.ResidualRisk = strings.TrimSpace(in.ResidualRisk)
	if in.RouteID == "" {
		return nil, RouteRecord{}, fmt.Errorf("route_id is required")
	}
	if in.Status != "accepted" && in.Status != "failed" && in.Status != "abandoned" {
		return nil, RouteRecord{}, fmt.Errorf("status must be accepted, failed, or abandoned")
	}
	if in.Status == "accepted" && in.Verification == "" {
		return nil, RouteRecord{}, fmt.Errorf("accepted outcome requires concrete verification evidence")
	}
	if in.HumanCorrection == "" {
		in.HumanCorrection = "unknown"
	}
	if in.HumanCorrection != "unknown" && in.HumanCorrection != "none" && in.HumanCorrection != "minor" && in.HumanCorrection != "major" {
		return nil, RouteRecord{}, fmt.Errorf("human_correction must be unknown, none, minor, or major")
	}
	if len(in.Verification) > 8000 || len(in.ResidualRisk) > 4000 {
		return nil, RouteRecord{}, fmt.Errorf("route outcome evidence exceeds bounded field limits")
	}

	s.mu.Lock()
	record := s.routes[in.RouteID]
	if record == nil {
		s.mu.Unlock()
		return nil, RouteRecord{}, fmt.Errorf("unknown route_id %q", in.RouteID)
	}
	if record.State != "open" {
		s.mu.Unlock()
		return nil, RouteRecord{}, fmt.Errorf("route_id %q already has terminal state %s", in.RouteID, record.State)
	}
	if in.Status == "accepted" && record.Plan.Action == router.ActionBlocked {
		s.mu.Unlock()
		return nil, RouteRecord{}, fmt.Errorf("capability_blocked route cannot be accepted without a new repaired route")
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
	return nil, snapshot, nil
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
