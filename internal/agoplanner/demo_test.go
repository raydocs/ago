package agoplanner_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"claudexflow/internal/agoplanner"
)

func demoRequest() agoplanner.Request {
	return agoplanner.Request{
		Repository: agoplanner.Repository{ID: "/tmp/fixture", Revision: "HEAD"},
		Objective: agoplanner.Objective{
			ID:      "objective:demo",
			Summary: "分析当前仓库，为 README 增加一个快速开始章节，运行相关测试，并生成完成报告。",
		},
		ProjectGates: []agoplanner.ProjectGate{{
			ID: "gate-demo", Title: "目标验收",
			AcceptanceCriteria: []string{"所有任务通过独立验收"},
			VerifierIDs:        []string{"ago-verifier"},
		}},
		Constraints: agoplanner.Constraints{
			PathScopes:     []string{"README.md", "docs"},
			CapabilityTags: []string{"repo-read", "repo-write", "tests", "report"},
			VerifierIDs:    []string{"ago-verifier"},
		},
	}
}

func TestDemoPlannerProducesValidatedChineseDAG(t *testing.T) {
	request := demoRequest()
	plan, err := agoplanner.DemoPlanner{}.Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if err := plan.Validate(request); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if plan.Objective != request.Objective || plan.Repository != request.Repository {
		t.Fatalf("plan must preserve the request objective and repository, got %#v", plan.Objective)
	}
	if len(plan.Tasks) < 2 || len(plan.Dependencies) == 0 {
		t.Fatalf("demo plan must be a DAG with dependencies, got %d tasks and %d edges", len(plan.Tasks), len(plan.Dependencies))
	}
	for _, task := range plan.Tasks {
		if !containsHan(task.Title) {
			t.Fatalf("task %q title is not Chinese: %q", task.ID, task.Title)
		}
	}
}

func TestDemoPlannerIsDeterministicForTheSameRequest(t *testing.T) {
	request := demoRequest()
	first, err := agoplanner.DemoPlanner{}.Plan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := agoplanner.DemoPlanner{}.Plan(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("demo planner must be deterministic so command replay stays exact")
	}
}

func TestDemoPlannerRejectsConstraintsItCannotSatisfy(t *testing.T) {
	request := demoRequest()
	request.Constraints.PathScopes = []string{"unrelated"}
	if _, err := (agoplanner.DemoPlanner{}).Plan(context.Background(), request); err == nil {
		t.Fatal("demo planner must fail closed when the requested path scopes cannot host its tasks")
	}
}

func containsHan(value string) bool {
	for _, r := range value {
		if r >= 0x4e00 && r <= 0x9fff {
			return true
		}
	}
	return strings.ContainsRune(value, '，')
}
