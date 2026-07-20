package agorelayplanner_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agoredact"
	"claudexflow/internal/agorelay"
	"claudexflow/internal/agorelayplanner"
)

const sentinel = "sk-ant-SENTINELsecretVALUE0123456789"

// scriptedResponse is one canned reply for a fake Model call: either a plan
// to encode, raw (possibly malformed) JSON, or an error to return directly.
type scriptedResponse struct {
	plan *agoplanner.Plan
	raw  string
	err  error
}

// fakeModel scripts one response per call, in order, and records every
// prompt it was given so tests can assert what the planner actually sent —
// including that it never sends a credential.
type fakeModel struct {
	responses []scriptedResponse
	calls     int
	prompts   []agorelay.Request
}

func (f *fakeModel) CompleteJSON(_ context.Context, request agorelay.Request, target any) error {
	f.prompts = append(f.prompts, request)
	index := f.calls
	f.calls++
	if index >= len(f.responses) {
		return errors.New("fakeModel: no scripted response for this call")
	}
	response := f.responses[index]
	if response.err != nil {
		return response.err
	}
	raw := response.raw
	if raw == "" {
		encoded, err := json.Marshal(response.plan)
		if err != nil {
			return err
		}
		raw = string(encoded)
	}
	return json.Unmarshal([]byte(raw), target)
}

// baseRequest is a valid, otherwise-unremarkable planner request that every
// test starts from and mutates as needed.
func baseRequest(objectiveSummary string) agoplanner.Request {
	return agoplanner.Request{
		Repository: agoplanner.Repository{ID: "repo-1", Revision: "rev-abc123"},
		Objective:  agoplanner.Objective{ID: "obj-1", Summary: objectiveSummary},
		ProjectGates: []agoplanner.ProjectGate{
			{
				ID:                 "gate-1",
				Title:              "质量关卡",
				AcceptanceCriteria: []string{"测试全部通过"},
				VerifierIDs:        []string{"tests"},
			},
		},
		Constraints: agoplanner.Constraints{
			PathScopes:     []string{"src", "docs"},
			CapabilityTags: []string{"repo-read", "repo-write", "tests", "report"},
			VerifierIDs:    []string{"tests", "lint"},
		},
	}
}

// twoTaskChainPlan is a minimal, valid, request-shaped plan: a read task
// feeding a write task. titleSuffix lets a test prove the plan reflects the
// objective it was asked to plan for rather than a fixed template.
func twoTaskChainPlan(request agoplanner.Request, titleSuffix string) agoplanner.Plan {
	return agoplanner.Plan{
		SchemaVersion: agoplanner.SchemaVersion,
		Repository:    request.Repository,
		Objective:     request.Objective,
		ProjectGates:  request.ProjectGates,
		Tasks: []agoplanner.TaskProposal{
			{
				ID: "inspect", Title: "阅读仓库 " + titleSuffix, Description: "阅读仓库结构并记录关键事实。",
				PathScopes: []string{"src"}, AcceptanceCriteria: []string{"记录仓库结构"},
				VerifierIDs: []string{"lint"}, CapabilityTags: []string{"repo-read"},
			},
			{
				ID: "implement", Title: "实现改动 " + titleSuffix, Description: "根据阅读结果实现所需改动。",
				PathScopes: []string{"src"}, AcceptanceCriteria: []string{"改动通过测试"},
				VerifierIDs: []string{"tests"}, CapabilityTags: []string{"repo-write"},
			},
		},
		Dependencies: []agoplanner.DependencyProposal{
			{TaskID: "implement", DependsOn: "inspect"},
		},
	}
}

func mustPlanner(t *testing.T, options agorelayplanner.Options) *agorelayplanner.Planner {
	t.Helper()
	planner, err := agorelayplanner.New(options)
	if err != nil {
		t.Fatalf("New(%+v) returned unexpected error: %v", options, err)
	}
	return planner
}

func TestChineseObjectiveProducesValidMultiTaskDAGWithDependencies(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	plan := twoTaskChainPlan(request, "登录")
	model := &fakeModel{responses: []scriptedResponse{{plan: &plan}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	got, err := planner.Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan returned unexpected error: %v", err)
	}
	if err := got.Validate(request); err != nil {
		t.Fatalf("returned plan failed Validate: %v", err)
	}
	if len(got.Tasks) != 2 || len(got.Dependencies) != 1 {
		t.Fatalf("expected 2 tasks and 1 dependency, got %d tasks and %d dependencies", len(got.Tasks), len(got.Dependencies))
	}
	if model.calls != 1 {
		t.Fatalf("expected exactly 1 model call, got %d", model.calls)
	}
}

// This is the regression guard against a fixed template: two different
// objectives must produce two different graphs, and the objective text must
// actually have reached the model.
func TestDifferentObjectivesProduceDifferentGraphs(t *testing.T) {
	request1 := baseRequest("为仓库添加登录功能")
	plan1 := twoTaskChainPlan(request1, "登录")
	model1 := &fakeModel{responses: []scriptedResponse{{plan: &plan1}}}
	planner1 := mustPlanner(t, agorelayplanner.Options{Model: model1})
	got1, err := planner1.Plan(context.Background(), request1)
	if err != nil {
		t.Fatalf("Plan (objective 1) returned unexpected error: %v", err)
	}

	request2 := baseRequest("为仓库添加支付功能")
	plan2 := twoTaskChainPlan(request2, "支付")
	model2 := &fakeModel{responses: []scriptedResponse{{plan: &plan2}}}
	planner2 := mustPlanner(t, agorelayplanner.Options{Model: model2})
	got2, err := planner2.Plan(context.Background(), request2)
	if err != nil {
		t.Fatalf("Plan (objective 2) returned unexpected error: %v", err)
	}

	if got1.Tasks[0].Title == got2.Tasks[0].Title {
		t.Fatalf("two different objectives produced identical task titles %q; planner looks templated", got1.Tasks[0].Title)
	}
	if !strings.Contains(model1.prompts[0].User, request1.Objective.Summary) {
		t.Fatalf("prompt for objective 1 did not carry the objective: %q", model1.prompts[0].User)
	}
	if !strings.Contains(model2.prompts[0].User, request2.Objective.Summary) {
		t.Fatalf("prompt for objective 2 did not carry the objective: %q", model2.prompts[0].User)
	}
}

func TestChainDependencyShapeValidates(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	plan := agoplanner.Plan{
		SchemaVersion: agoplanner.SchemaVersion,
		Repository:    request.Repository,
		Objective:     request.Objective,
		ProjectGates:  request.ProjectGates,
		Tasks: []agoplanner.TaskProposal{
			{ID: "a", Title: "步骤一", Description: "第一步", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成第一步"}, VerifierIDs: []string{"lint"}, CapabilityTags: []string{"repo-read"}},
			{ID: "b", Title: "步骤二", Description: "第二步", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成第二步"}, VerifierIDs: []string{"lint"}, CapabilityTags: []string{"repo-read"}},
			{ID: "c", Title: "步骤三", Description: "第三步", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成第三步"}, VerifierIDs: []string{"tests"}, CapabilityTags: []string{"repo-write"}},
		},
		Dependencies: []agoplanner.DependencyProposal{
			{TaskID: "b", DependsOn: "a"},
			{TaskID: "c", DependsOn: "b"},
		},
	}
	model := &fakeModel{responses: []scriptedResponse{{plan: &plan}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	got, err := planner.Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan returned unexpected error for a valid 3-step chain: %v", err)
	}
	if err := got.Validate(request); err != nil {
		t.Fatalf("chain plan failed Validate: %v", err)
	}
}

func TestParallelDependencyShapeValidates(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	plan := agoplanner.Plan{
		SchemaVersion: agoplanner.SchemaVersion,
		Repository:    request.Repository,
		Objective:     request.Objective,
		ProjectGates:  request.ProjectGates,
		Tasks: []agoplanner.TaskProposal{
			{ID: "root", Title: "根任务", Description: "共同前置", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成前置"}, VerifierIDs: []string{"lint"}, CapabilityTags: []string{"repo-read"}},
			{ID: "left", Title: "左分支", Description: "并行分支一", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成左分支"}, VerifierIDs: []string{"tests"}, CapabilityTags: []string{"repo-write"}},
			{ID: "right", Title: "右分支", Description: "并行分支二", PathScopes: []string{"docs"}, AcceptanceCriteria: []string{"完成右分支"}, VerifierIDs: []string{"tests"}, CapabilityTags: []string{"repo-write"}},
		},
		Dependencies: []agoplanner.DependencyProposal{
			{TaskID: "left", DependsOn: "root"},
			{TaskID: "right", DependsOn: "root"},
		},
	}
	model := &fakeModel{responses: []scriptedResponse{{plan: &plan}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	got, err := planner.Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan returned unexpected error for a valid parallel shape: %v", err)
	}
	if err := got.Validate(request); err != nil {
		t.Fatalf("parallel plan failed Validate: %v", err)
	}
}

func TestMalformedJSONFailsAfterExactlyTwoCalls(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	model := &fakeModel{responses: []scriptedResponse{
		{raw: "{not valid json"},
		{raw: "{still not valid"},
	}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	_, err := planner.Plan(context.Background(), request)
	if err == nil {
		t.Fatalf("expected an error for malformed model output, got nil")
	}
	var planErr agorelayplanner.PlanError
	if !errors.As(err, &planErr) {
		t.Fatalf("expected a PlanError, got %T: %v", err, err)
	}
	if planErr.Attempts != 2 {
		t.Fatalf("expected PlanError.Attempts == 2, got %d", planErr.Attempts)
	}
	if model.calls != 2 {
		t.Fatalf("expected exactly 2 model calls, got %d", model.calls)
	}
}

func TestCyclicPlanRejected(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	cyclic := agoplanner.Plan{
		SchemaVersion: agoplanner.SchemaVersion,
		Repository:    request.Repository,
		Objective:     request.Objective,
		ProjectGates:  request.ProjectGates,
		Tasks: []agoplanner.TaskProposal{
			{ID: "a", Title: "任务甲", Description: "描述甲", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成甲"}, VerifierIDs: []string{"lint"}, CapabilityTags: []string{"repo-read"}},
			{ID: "b", Title: "任务乙", Description: "描述乙", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成乙"}, VerifierIDs: []string{"lint"}, CapabilityTags: []string{"repo-read"}},
		},
		Dependencies: []agoplanner.DependencyProposal{
			{TaskID: "a", DependsOn: "b"},
			{TaskID: "b", DependsOn: "a"},
		},
	}
	model := &fakeModel{responses: []scriptedResponse{{plan: &cyclic}, {plan: &cyclic}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	_, err := planner.Plan(context.Background(), request)
	if err == nil {
		t.Fatalf("expected a cyclic plan to be rejected")
	}
	var planErr agorelayplanner.PlanError
	if !errors.As(err, &planErr) || planErr.Attempts != 2 {
		t.Fatalf("expected PlanError with Attempts == 2, got %#v (err=%v)", planErr, err)
	}
	if model.calls != 2 {
		t.Fatalf("expected exactly 2 model calls, got %d", model.calls)
	}
}

func TestMoreTasksThanMaxTasksRejected(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	tooMany := agoplanner.Plan{
		SchemaVersion: agoplanner.SchemaVersion,
		Repository:    request.Repository,
		Objective:     request.Objective,
		ProjectGates:  request.ProjectGates,
		Tasks: []agoplanner.TaskProposal{
			{ID: "a", Title: "任务甲", Description: "描述甲", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成甲"}, VerifierIDs: []string{"lint"}, CapabilityTags: []string{"repo-read"}},
			{ID: "b", Title: "任务乙", Description: "描述乙", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成乙"}, VerifierIDs: []string{"lint"}, CapabilityTags: []string{"repo-read"}},
			{ID: "c", Title: "任务丙", Description: "描述丙", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成丙"}, VerifierIDs: []string{"lint"}, CapabilityTags: []string{"repo-read"}},
		},
	}
	model := &fakeModel{responses: []scriptedResponse{{plan: &tooMany}, {plan: &tooMany}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model, MaxTasks: 2})

	_, err := planner.Plan(context.Background(), request)
	if err == nil {
		t.Fatalf("expected a plan with more tasks than MaxTasks to be rejected")
	}
	var planErr agorelayplanner.PlanError
	if !errors.As(err, &planErr) || planErr.Attempts != 2 {
		t.Fatalf("expected PlanError with Attempts == 2, got %#v (err=%v)", planErr, err)
	}
}

func TestDepthBeyondMaxDepthRejected(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	chain := agoplanner.Plan{
		SchemaVersion: agoplanner.SchemaVersion,
		Repository:    request.Repository,
		Objective:     request.Objective,
		ProjectGates:  request.ProjectGates,
		Tasks: []agoplanner.TaskProposal{
			{ID: "a", Title: "步骤一", Description: "第一步", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成一"}, VerifierIDs: []string{"lint"}, CapabilityTags: []string{"repo-read"}},
			{ID: "b", Title: "步骤二", Description: "第二步", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成二"}, VerifierIDs: []string{"lint"}, CapabilityTags: []string{"repo-read"}},
			{ID: "c", Title: "步骤三", Description: "第三步", PathScopes: []string{"src"}, AcceptanceCriteria: []string{"完成三"}, VerifierIDs: []string{"lint"}, CapabilityTags: []string{"repo-read"}},
		},
		Dependencies: []agoplanner.DependencyProposal{
			{TaskID: "b", DependsOn: "a"},
			{TaskID: "c", DependsOn: "b"},
		},
	}
	model := &fakeModel{responses: []scriptedResponse{{plan: &chain}, {plan: &chain}}}
	// A 3-task chain has depth 3; MaxDepth: 2 must reject it.
	planner := mustPlanner(t, agorelayplanner.Options{Model: model, MaxDepth: 2})

	_, err := planner.Plan(context.Background(), request)
	if err == nil {
		t.Fatalf("expected a dependency chain deeper than MaxDepth to be rejected")
	}
	var planErr agorelayplanner.PlanError
	if !errors.As(err, &planErr) || planErr.Attempts != 2 {
		t.Fatalf("expected PlanError with Attempts == 2, got %#v (err=%v)", planErr, err)
	}
}

func TestPathScopeOutsideAllowedRejected(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	plan := twoTaskChainPlan(request, "登录")
	plan.Tasks[0].PathScopes = []string{"outside-of-allowed"}
	model := &fakeModel{responses: []scriptedResponse{{plan: &plan}, {plan: &plan}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	_, err := planner.Plan(context.Background(), request)
	if err == nil {
		t.Fatalf("expected a path scope outside the allowed set to be rejected")
	}
	var planErr agorelayplanner.PlanError
	if !errors.As(err, &planErr) {
		t.Fatalf("expected a PlanError, got %T: %v", err, err)
	}
}

func TestUnknownCapabilityTagRejected(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	plan := twoTaskChainPlan(request, "登录")
	plan.Tasks[0].CapabilityTags = []string{"network-access"}
	model := &fakeModel{responses: []scriptedResponse{{plan: &plan}, {plan: &plan}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	_, err := planner.Plan(context.Background(), request)
	if err == nil {
		t.Fatalf("expected an unknown capability tag to be rejected")
	}
	var planErr agorelayplanner.PlanError
	if !errors.As(err, &planErr) {
		t.Fatalf("expected a PlanError, got %T: %v", err, err)
	}
}

func TestUnknownVerifierIDRejected(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	plan := twoTaskChainPlan(request, "登录")
	plan.Tasks[0].VerifierIDs = []string{"unknown-verifier"}
	model := &fakeModel{responses: []scriptedResponse{{plan: &plan}, {plan: &plan}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	_, err := planner.Plan(context.Background(), request)
	if err == nil {
		t.Fatalf("expected an unknown verifier id to be rejected")
	}
	var planErr agorelayplanner.PlanError
	if !errors.As(err, &planErr) {
		t.Fatalf("expected a PlanError, got %T: %v", err, err)
	}
}

func TestEmptyAcceptanceCriteriaRejected(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	plan := twoTaskChainPlan(request, "登录")
	plan.Tasks[0].AcceptanceCriteria = nil
	model := &fakeModel{responses: []scriptedResponse{{plan: &plan}, {plan: &plan}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	_, err := planner.Plan(context.Background(), request)
	if err == nil {
		t.Fatalf("expected empty acceptance criteria to be rejected")
	}
	var planErr agorelayplanner.PlanError
	if !errors.As(err, &planErr) {
		t.Fatalf("expected a PlanError, got %T: %v", err, err)
	}
}

func TestWriteTaskWithNoPathScopeRejected(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	plan := twoTaskChainPlan(request, "登录")
	plan.Tasks[1].PathScopes = nil // "implement" carries repo-write
	model := &fakeModel{responses: []scriptedResponse{{plan: &plan}, {plan: &plan}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	_, err := planner.Plan(context.Background(), request)
	if err == nil {
		t.Fatalf("expected a write task with no path scope to be rejected")
	}
	var planErr agorelayplanner.PlanError
	if !errors.As(err, &planErr) {
		t.Fatalf("expected a PlanError, got %T: %v", err, err)
	}
}

// A write task that names the entire allowed path scope has not actually
// said what it will touch, so it must be rejected the same as an empty one.
func TestWriteTaskClaimingEntireAllowedScopeRejected(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	plan := twoTaskChainPlan(request, "登录")
	plan.Tasks[1].PathScopes = append([]string(nil), request.Constraints.PathScopes...) // full set: {"src","docs"}
	model := &fakeModel{responses: []scriptedResponse{{plan: &plan}, {plan: &plan}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	_, err := planner.Plan(context.Background(), request)
	if err == nil {
		t.Fatalf("expected a write task claiming the entire allowed scope to be rejected")
	}
	var planErr agorelayplanner.PlanError
	if !errors.As(err, &planErr) {
		t.Fatalf("expected a PlanError, got %T: %v", err, err)
	}
}

func TestCorrectionCallThatSucceedsReturnsPlanWithExactlyTwoCalls(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	invalid := twoTaskChainPlan(request, "登录")
	invalid.Tasks[0].AcceptanceCriteria = nil // first attempt fails validation
	valid := twoTaskChainPlan(request, "登录")

	model := &fakeModel{responses: []scriptedResponse{{plan: &invalid}, {plan: &valid}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	got, err := planner.Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("expected the correction round to produce a usable plan, got error: %v", err)
	}
	if err := got.Validate(request); err != nil {
		t.Fatalf("corrected plan failed Validate: %v", err)
	}
	if model.calls != 2 {
		t.Fatalf("expected exactly 2 model calls, got %d", model.calls)
	}
	// The correction prompt must carry the validation error so the model
	// has something concrete to fix.
	if !strings.Contains(model.prompts[1].User, "acceptance") && !strings.Contains(model.prompts[1].User, "验证") {
		t.Fatalf("correction prompt did not appear to carry the validation failure: %q", model.prompts[1].User)
	}
}

func TestSecondFailureAfterCorrectionReturnsPlanErrorWithNoThirdCall(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	invalid1 := twoTaskChainPlan(request, "登录")
	invalid1.Tasks[0].AcceptanceCriteria = nil
	invalid2 := twoTaskChainPlan(request, "登录")
	invalid2.Tasks[0].VerifierIDs = []string{"unknown-verifier"}

	model := &fakeModel{responses: []scriptedResponse{{plan: &invalid1}, {plan: &invalid2}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	_, err := planner.Plan(context.Background(), request)
	if err == nil {
		t.Fatalf("expected an error when the correction round also fails validation")
	}
	var planErr agorelayplanner.PlanError
	if !errors.As(err, &planErr) {
		t.Fatalf("expected a PlanError, got %T: %v", err, err)
	}
	if planErr.Attempts != 2 {
		t.Fatalf("expected PlanError.Attempts == 2, got %d", planErr.Attempts)
	}
	if model.calls != 2 {
		t.Fatalf("expected exactly 2 model calls and no third call, got %d", model.calls)
	}
}

func TestRequestRepositoryAndObjectiveWinOverModelOutput(t *testing.T) {
	request := baseRequest("为仓库添加登录功能")
	plan := twoTaskChainPlan(request, "登录")
	// The model tries to rewrite what it is planning for.
	plan.Repository = agoplanner.Repository{ID: "some-other-repo", Revision: "deadbeef"}
	plan.Objective = agoplanner.Objective{ID: "some-other-objective", Summary: "完全不同的目标"}

	model := &fakeModel{responses: []scriptedResponse{{plan: &plan}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model})

	got, err := planner.Plan(context.Background(), request)
	if err != nil {
		t.Fatalf("Plan returned unexpected error: %v", err)
	}
	if got.Repository != request.Repository {
		t.Fatalf("expected request repository %+v to win, got %+v", request.Repository, got.Repository)
	}
	if got.Objective != request.Objective {
		t.Fatalf("expected request objective %+v to win, got %+v", request.Objective, got.Objective)
	}
}

func TestSentinelSecretNeverReachesPrompt(t *testing.T) {
	request := baseRequest("为仓库添加登录功能 " + sentinel)
	plan := twoTaskChainPlan(request, "登录")

	model := &fakeModel{responses: []scriptedResponse{{plan: &plan}}}
	planner := mustPlanner(t, agorelayplanner.Options{Model: model, Redactor: agoredact.New(sentinel)})

	if _, err := planner.Plan(context.Background(), request); err != nil {
		t.Fatalf("Plan returned unexpected error: %v", err)
	}
	if len(model.prompts) == 0 {
		t.Fatalf("expected at least one prompt to have been sent")
	}
	for index, prompt := range model.prompts {
		if strings.Contains(prompt.System, sentinel) {
			t.Fatalf("prompt %d system message leaked the sentinel: %q", index, prompt.System)
		}
		if strings.Contains(prompt.User, sentinel) {
			t.Fatalf("prompt %d user message leaked the sentinel: %q", index, prompt.User)
		}
	}
}

func TestNewRejectsMissingModel(t *testing.T) {
	if _, err := agorelayplanner.New(agorelayplanner.Options{}); err == nil {
		t.Fatalf("expected New to reject Options with a nil Model")
	}
}
