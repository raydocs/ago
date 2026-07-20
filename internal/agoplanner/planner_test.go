package agoplanner

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestFixturePlannerGoldenSupportsChainAndParallelLeaves(t *testing.T) {
	request, proposal := fixture()
	planner := FixturePlanner{Proposal: proposal}
	got, err := planner.Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/chain_parallel.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(encoded)) != strings.TrimSpace(string(want)) {
		t.Fatalf("plan JSON differs from golden:\n%s", encoded)
	}

	got.Tasks[0].PathScopes[0] = "mutated"
	again, err := planner.Plan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, proposal) {
		t.Fatal("fixture planner did not return a defensive deterministic copy")
	}
}

func TestPlanRejectsInvalidProposals(t *testing.T) {
	request, valid := fixture()
	tests := []struct {
		name   string
		mutate func(*Request, *Plan)
		match  string
	}{
		{name: "missing acceptance", mutate: func(_ *Request, plan *Plan) { plan.Tasks[0].AcceptanceCriteria = nil }, match: "acceptance"},
		{name: "missing verifier", mutate: func(_ *Request, plan *Plan) { plan.Tasks[0].VerifierIDs = nil }, match: "verifier"},
		{name: "oversized", mutate: func(request *Request, plan *Plan) {
			request.Constraints.MaxTasks = len(plan.Tasks) - 1
		}, match: "1..3 tasks"},
		{name: "oversized encoding", mutate: func(_ *Request, plan *Plan) {
			plan.Tasks[0].Description = strings.Repeat("x", MaxEncodedPlanBytes)
		}, match: "encoded plan exceeds"},
		{name: "cycle", mutate: func(_ *Request, plan *Plan) {
			plan.Dependencies = append(plan.Dependencies, DependencyProposal{TaskID: "inspect", DependsOn: "test"})
		}, match: "cycle"},
		{name: "missing project gates", mutate: func(_ *Request, plan *Plan) { plan.ProjectGates = nil }, match: "project gates"},
		{name: "changed project gate", mutate: func(_ *Request, plan *Plan) { plan.ProjectGates[0].Title = "weaker" }, match: "must match"},
		{name: "outside path scope", mutate: func(_ *Request, plan *Plan) { plan.Tasks[0].PathScopes = []string{"cmd/ago"} }, match: "not allowed"},
		{name: "parent path scope", mutate: func(_ *Request, plan *Plan) { plan.Tasks[0].PathScopes = []string{"../secret"} }, match: "not allowed"},
		{name: "unsupported capability", mutate: func(_ *Request, plan *Plan) { plan.Tasks[0].CapabilityTags = []string{"network"} }, match: "not allowed"},
		{name: "unsupported verifier", mutate: func(_ *Request, plan *Plan) { plan.Tasks[0].VerifierIDs = []string{"shell"} }, match: "not allowed"},
		{name: "unknown dependency", mutate: func(_ *Request, plan *Plan) { plan.Dependencies[0].DependsOn = "missing" }, match: "not found"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidateRequest := request
			candidateRequest.ProjectGates = clonePlan(Plan{ProjectGates: request.ProjectGates}).ProjectGates
			candidate := clonePlan(valid)
			test.mutate(&candidateRequest, &candidate)
			err := candidate.Validate(candidateRequest)
			if err == nil || !strings.Contains(err.Error(), test.match) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.match)
			}
		})
	}
}

func TestRequestRejectsMissingOrInvalidProjectGate(t *testing.T) {
	request, plan := fixture()
	request.ProjectGates[0].AcceptanceCriteria = nil
	plan.ProjectGates[0].AcceptanceCriteria = nil
	if err := plan.Validate(request); err == nil || !strings.Contains(err.Error(), "project gate acceptance") {
		t.Fatalf("Validate() error = %v, want project gate acceptance error", err)
	}
}

func TestFixturePlannerHonorsCancellation(t *testing.T) {
	request, plan := fixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (FixturePlanner{Proposal: plan}).Plan(ctx, request); err != context.Canceled {
		t.Fatalf("Plan() error = %v, want context canceled", err)
	}
}

func fixture() (Request, Plan) {
	gate := ProjectGate{
		ID: "project-tests", Title: "Project tests pass",
		AcceptanceCriteria: []string{"The planner package test suite passes."},
		VerifierIDs:        []string{"go-test"},
	}
	request := Request{
		Repository:   Repository{ID: "raydocs/ago", Revision: "d79c720"},
		Objective:    Objective{ID: "A-21", Summary: "Implement a bounded objective-to-DAG planner contract."},
		ProjectGates: []ProjectGate{gate},
		Constraints: Constraints{
			PathScopes: []string{"internal/agoplanner"}, CapabilityTags: []string{"go", "tests"}, VerifierIDs: []string{"go-test", "go-vet"},
			MaxTasks: 8, MaxDependencies: 12,
		},
	}
	plan := Plan{
		SchemaVersion: SchemaVersion, Repository: request.Repository, Objective: request.Objective,
		Tasks: []TaskProposal{
			{ID: "inspect", Title: "Inspect protocol", Description: "Confirm repository protocol conventions.", PathScopes: []string{"internal/agoplanner"}, AcceptanceCriteria: []string{"Contract conventions are identified."}, VerifierIDs: []string{"go-test"}, CapabilityTags: []string{"go"}},
			{ID: "contract", Title: "Implement contract", Description: "Define and validate bounded planner proposals.", PathScopes: []string{"internal/agoplanner/planner.go"}, AcceptanceCriteria: []string{"Invalid plans fail closed."}, VerifierIDs: []string{"go-test", "go-vet"}, CapabilityTags: []string{"go"}},
			{ID: "test", Title: "Test contract", Description: "Cover valid and invalid DAG proposals.", PathScopes: []string{"internal/agoplanner/planner_test.go"}, AcceptanceCriteria: []string{"Golden and invalid fixtures pass."}, VerifierIDs: []string{"go-test"}, CapabilityTags: []string{"go", "tests"}},
			{ID: "vet", Title: "Vet contract", Description: "Statically check the planner package.", PathScopes: []string{"internal/agoplanner"}, AcceptanceCriteria: []string{"Go vet reports no findings."}, VerifierIDs: []string{"go-vet"}, CapabilityTags: []string{"go"}},
		},
		Dependencies: []DependencyProposal{
			{TaskID: "contract", DependsOn: "inspect"},
			{TaskID: "test", DependsOn: "contract"},
			{TaskID: "vet", DependsOn: "contract"},
		},
		ProjectGates: []ProjectGate{gate},
	}
	return request, plan
}
