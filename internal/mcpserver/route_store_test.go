package mcpserver

import (
	"context"
	"path/filepath"
	"testing"

	"claudexflow/internal/router"
)

func TestOpenRouteSurvivesProcessRestartForOutcome(t *testing.T) {
	dir := t.TempDir()
	openPath := filepath.Join(dir, "open-routes.json")
	ledger := filepath.Join(dir, "route-outcomes.jsonl")

	// Process A: plan a route (persists open index).
	a := newTestServer(t)
	a.openRoutesPathOverride = openPath
	a.routeLedgerPath = ledger
	_, plan, err := a.routeTask(context.Background(), nil, router.RouteRequest{
		Objective: "Implement isolated parser and run go test.", AcceptanceCriteria: []string{"Parser passes."},
		VerificationTarget: "go test ./parser", WorkerMarginalContribution: "Own the isolated implementation so the supervisor only verifies.", IndependentSlices: 1, Checkability: "objective",
	})
	if err != nil || plan.RouteID == "" {
		t.Fatalf("route: plan=%#v err=%v", plan, err)
	}

	// Process B: new MCP memory, load open routes, record outcome.
	b := newTestServer(t)
	b.openRoutesPathOverride = openPath
	b.routeLedgerPath = ledger
	b.loadOpenRoutesIntoMemory()
	_, rec, err := b.recordRouteOutcome(context.Background(), nil, RouteOutcomeInput{
		RouteID: plan.RouteID, Status: "accepted", Verification: "go test ./parser: PASS", HumanCorrection: "none",
	})
	if err != nil {
		t.Fatalf("resume outcome failed: %v", err)
	}
	if rec.State != "accepted" || rec.LedgerStatus != "persisted" {
		t.Fatalf("unexpected record: %#v", rec)
	}
}
