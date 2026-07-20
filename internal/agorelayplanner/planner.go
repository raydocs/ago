// Package agorelayplanner is the real, model-backed implementation of
// agoplanner.Planner. It turns a Chinese objective plus the caller's
// repository, path-scope, capability, verifier, and project-gate constraints
// into a concrete task DAG by asking a model for structured JSON output.
//
// agoplanner.DemoPlanner remains in the tree, unchanged, as the fixed offline
// fixture used for demos and environments without a reachable relay; this
// package is what production planning uses when a model is available.
//
// Every model response — first attempt or corrected — is admitted only after
// passing agoplanner.Plan.Validate against the original request, plus this
// package's own bounds (task/edge/depth ceilings and a write-path-scope
// check that Validate does not perform). The request, never the model, owns
// SchemaVersion, Repository, Objective, and ProjectGates: a model cannot talk
// its way into rewriting what goal it was asked to plan for. Credentials
// never reach this package (it holds a Model, not a key), and every prompt
// and every returned error is passed through a Redactor before it leaves.
package agorelayplanner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agoredact"
	"claudexflow/internal/agorelay"
)

// Safe defaults used when Options leaves a bound at zero. These are
// deliberately tighter than agoplanner's own ceilings (32 tasks / 128
// dependencies): a model-proposed graph should stay small enough for a
// human to review, not merely small enough to be technically admissible.
const (
	defaultMaxTasks        = 8
	defaultMaxDependencies = 24
	defaultMaxDepth        = 5
)

const schemaName = "ago_plan"

// planJSONSchema is a permissive JSON Schema describing the shape we ask the
// model to emit. It mirrors agoplanner.Plan's JSON tags so the response can
// be unmarshaled directly into that type.
const planJSONSchema = `{
  "type": "object",
  "properties": {
    "schema_version": {"type": "integer"},
    "repository": {
      "type": "object",
      "properties": {"id": {"type": "string"}, "revision": {"type": "string"}}
    },
    "objective": {
      "type": "object",
      "properties": {"id": {"type": "string"}, "summary": {"type": "string"}}
    },
    "tasks": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string"},
          "title": {"type": "string"},
          "description": {"type": "string"},
          "path_scopes": {"type": "array", "items": {"type": "string"}},
          "acceptance_criteria": {"type": "array", "items": {"type": "string"}},
          "verifier_ids": {"type": "array", "items": {"type": "string"}},
          "capability_tags": {"type": "array", "items": {"type": "string"}}
        },
        "required": ["id", "title", "description"]
      }
    },
    "dependencies": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {"task_id": {"type": "string"}, "depends_on": {"type": "string"}}
      }
    },
    "project_gates": {"type": "array"}
  },
  "required": ["tasks"]
}`

// writeCapabilities are capability tags that mean a task will mutate the
// repository. A task carrying any of these must name a concrete, narrower
// path scope than the full allowed set — see checkWriteScopes.
var writeCapabilities = map[string]struct{}{
	"repo-write": {},
	"write":      {},
	"shell":      {},
}

// Model is the narrow transport slice agorelayplanner needs. It is an
// interface so tests can script responses without a network.
type Model interface {
	CompleteJSON(ctx context.Context, request agorelay.Request, target any) error
}

// Options configures a Planner. Model is required; the bounds and Redactor
// are optional.
type Options struct {
	Model Model

	// MaxTasks, MaxDependencies, MaxDepth bound the proposed graph beyond
	// whatever agoplanner.Plan.Validate already enforces. Zero uses the
	// package defaults (8, 24, 5).
	MaxTasks        int
	MaxDependencies int
	MaxDepth        int

	// Redactor scrubs every prompt sent to Model and every error returned to
	// the caller. If nil, a Redactor with no extra literals is used — it
	// still strips recognizable credential shapes.
	Redactor *agoredact.Redactor
}

// Planner is the model-backed agoplanner.Planner implementation.
type Planner struct {
	model           Model
	maxTasks        int
	maxDependencies int
	maxDepth        int
	redactor        *agoredact.Redactor
}

// New validates options and builds a Planner.
func New(options Options) (*Planner, error) {
	if options.Model == nil {
		return nil, fmt.Errorf("agorelayplanner: Model is required")
	}
	if options.MaxTasks < 0 || options.MaxDependencies < 0 || options.MaxDepth < 0 {
		return nil, fmt.Errorf("agorelayplanner: bounds must not be negative")
	}
	maxTasks := options.MaxTasks
	if maxTasks == 0 {
		maxTasks = defaultMaxTasks
	}
	maxDependencies := options.MaxDependencies
	if maxDependencies == 0 {
		maxDependencies = defaultMaxDependencies
	}
	maxDepth := options.MaxDepth
	if maxDepth == 0 {
		maxDepth = defaultMaxDepth
	}
	redactor := options.Redactor
	if redactor == nil {
		redactor = agoredact.New()
	}
	return &Planner{
		model:           options.Model,
		maxTasks:        maxTasks,
		maxDependencies: maxDependencies,
		maxDepth:        maxDepth,
		redactor:        redactor,
	}, nil
}

// PlanError reports that the model never produced a usable plan within the
// correction budget. It is distinct from an error returned directly by the
// context or the transport (context.Canceled, context.DeadlineExceeded, an
// agorelay.StatusError, and similar), which a caller may choose to retry;
// PlanError means the content itself — not the transport — was the problem,
// and retrying with the same objective is not expected to help.
type PlanError struct {
	Reason   string
	Attempts int
}

func (e PlanError) Error() string {
	return fmt.Sprintf("agorelayplanner: model did not produce an admissible plan after %d attempt(s): %s", e.Attempts, e.Reason)
}

// Plan satisfies agoplanner.Planner. It asks the model once, validates the
// result, and — only if that fails — sends exactly one corrected request
// carrying the validation error before giving up.
func (p *Planner) Plan(ctx context.Context, request agoplanner.Request) (agoplanner.Plan, error) {
	if err := ctx.Err(); err != nil {
		return agoplanner.Plan{}, err
	}

	system := p.redactor.String(systemPrompt())
	initialUser := p.redactor.String(p.buildUserPrompt(request))

	plan, err := p.callAndValidate(ctx, request, agorelay.Request{
		System:     system,
		User:       initialUser,
		SchemaName: schemaName,
		Schema:     json.RawMessage(planJSONSchema),
	})
	if err == nil {
		return plan, nil
	}

	// A context failure is the environment giving up, not the model
	// producing bad content; let it through unwrapped so a caller can tell
	// it apart from a terminal PlanError.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return agoplanner.Plan{}, ctxErr
	}

	correctionUser := p.buildCorrectionPrompt(initialUser, err)

	plan, err2 := p.callAndValidate(ctx, request, agorelay.Request{
		System:     system,
		User:       correctionUser,
		SchemaName: schemaName,
		Schema:     json.RawMessage(planJSONSchema),
	})
	if err2 != nil {
		return agoplanner.Plan{}, PlanError{
			Reason:   p.redactor.String(err2.Error()),
			Attempts: 2,
		}
	}
	return plan, nil
}

// callAndValidate performs one model call, pins the request-owned fields,
// and runs every admissibility check. It never returns a plan that failed
// any check.
func (p *Planner) callAndValidate(ctx context.Context, request agoplanner.Request, wire agorelay.Request) (agoplanner.Plan, error) {
	if err := ctx.Err(); err != nil {
		return agoplanner.Plan{}, err
	}

	// Only the fields the model actually owns are parsed. Decoding into the
	// full Plan meant a model writing "schema_version": "1.0" or a repository
	// as a bare string broke the whole response, and it invited the idea that
	// a model has any say over which goal is being planned. It does not: those
	// fields come from the request below.
	var proposal struct {
		Tasks        []agoplanner.TaskProposal       `json:"tasks"`
		Dependencies []agoplanner.DependencyProposal `json:"dependencies"`
	}
	if err := p.model.CompleteJSON(ctx, wire, &proposal); err != nil {
		return agoplanner.Plan{}, fmt.Errorf("model call failed: %w", err)
	}

	// The request — not the model — owns what goal is being planned for.
	// A model that echoes back a different repository, objective, or gate
	// set must not be able to make that stick.
	plan := agoplanner.Plan{
		SchemaVersion: agoplanner.SchemaVersion,
		Repository:    request.Repository,
		Objective:     request.Objective,
		Tasks:         proposal.Tasks,
		Dependencies:  proposal.Dependencies,
	}
	plan.ProjectGates = request.ProjectGates

	if err := plan.Validate(request); err != nil {
		return agoplanner.Plan{}, fmt.Errorf("plan failed validation: %w", err)
	}
	if err := p.checkBounds(plan); err != nil {
		return agoplanner.Plan{}, err
	}
	if err := checkWriteScopes(plan.Tasks, request.Constraints.PathScopes); err != nil {
		return agoplanner.Plan{}, err
	}
	return plan, nil
}

// checkBounds enforces the planner's own task/edge/depth ceilings, which sit
// on top of (and are normally tighter than) whatever agoplanner.Plan.Validate
// already allows.
func (p *Planner) checkBounds(plan agoplanner.Plan) error {
	if len(plan.Tasks) > p.maxTasks {
		return fmt.Errorf("plan contains %d tasks, exceeding planner limit of %d", len(plan.Tasks), p.maxTasks)
	}
	if len(plan.Dependencies) > p.maxDependencies {
		return fmt.Errorf("plan contains %d dependencies, exceeding planner limit of %d", len(plan.Dependencies), p.maxDependencies)
	}
	// Validate already rejected cycles, so this longest-path walk is safe.
	if depth := longestChain(plan.Tasks, plan.Dependencies); depth > p.maxDepth {
		return fmt.Errorf("plan dependency chain depth %d exceeds planner limit of %d", depth, p.maxDepth)
	}
	return nil
}

// longestChain returns the length, in tasks, of the longest dependency chain
// in the DAG. Callers must ensure the graph is acyclic first.
func longestChain(tasks []agoplanner.TaskProposal, dependencies []agoplanner.DependencyProposal) int {
	adjacency := make(map[string][]string, len(tasks))
	for _, dependency := range dependencies {
		adjacency[dependency.TaskID] = append(adjacency[dependency.TaskID], dependency.DependsOn)
	}
	memo := make(map[string]int, len(tasks))
	var depthOf func(string) int
	depthOf = func(id string) int {
		if cached, ok := memo[id]; ok {
			return cached
		}
		best := 0
		for _, prerequisite := range adjacency[id] {
			if d := depthOf(prerequisite); d > best {
				best = d
			}
		}
		memo[id] = best + 1
		return best + 1
	}
	max := 0
	for _, task := range tasks {
		if d := depthOf(task.ID); d > max {
			max = d
		}
	}
	return max
}

// checkWriteScopes rejects a task whose capability tags include a writing
// capability but whose path scopes are empty or exactly the full allowed
// set. Validate already forbids empty path scopes for every task (it
// requires 1..N values), so in practice this only catches the "claims the
// whole allowed surface" case — but a writer that names nothing narrower
// than everything it was ever allowed to touch has not actually said what
// it will change.
func checkWriteScopes(tasks []agoplanner.TaskProposal, allowedScopes []string) error {
	allowed := scopeSet(allowedScopes)
	for _, task := range tasks {
		if !hasWriteCapability(task.CapabilityTags) {
			continue
		}
		if len(task.PathScopes) == 0 {
			return fmt.Errorf("task %q carries a write capability but declares no path scope", task.ID)
		}
		if scopeSetsEqual(scopeSet(task.PathScopes), allowed) {
			return fmt.Errorf("task %q carries a write capability but claims the entire allowed path scope instead of a concrete subset", task.ID)
		}
	}
	return nil
}

func hasWriteCapability(tags []string) bool {
	for _, tag := range tags {
		if _, ok := writeCapabilities[tag]; ok {
			return true
		}
	}
	return false
}

func scopeSet(values []string) map[string]struct{} {
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func scopeSetsEqual(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for value := range left {
		if _, ok := right[value]; !ok {
			return false
		}
	}
	return true
}

// systemPrompt is static: it carries no request data, so it needs no
// redaction of its own, but Plan still passes it through the Redactor for
// uniformity.
func systemPrompt() string {
	return "You are a task-decomposition planner for an autonomous coding system. " +
		"Given an objective and its constraints, respond with ONLY one JSON object describing a directed acyclic graph of tasks that accomplishes the objective. " +
		"Every task must draw path_scopes only from the allowed path scopes, capability_tags only from the allowed capability tags, and verifier_ids only from the allowed verifier ids. " +
		"A task whose capability_tags include a writing capability (repo-write, write, or shell) must declare a concrete, narrower subset of the allowed path scopes — never the entire allowed set. " +
		"Every task needs a non-empty title, description, and acceptance_criteria. " +
		"Do not include any explanation outside the JSON object."
}

// buildUserPrompt renders the request into the prompt the model plans
// against. Everything the model needs to make the plan depend on the actual
// objective lives here: the Chinese objective itself, the repository, the
// allowed scopes/capabilities/verifiers, the project gates, and the graph
// limits.
func (p *Planner) buildUserPrompt(request agoplanner.Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "目标 (objective)\nid: %s\n概述: %s\n\n", request.Objective.ID, request.Objective.Summary)
	fmt.Fprintf(&b, "仓库 (repository)\nid: %s\nrevision: %s\n\n", request.Repository.ID, request.Repository.Revision)
	fmt.Fprintf(&b, "允许的路径范围 (allowed path scopes): %s\n", strings.Join(request.Constraints.PathScopes, ", "))
	fmt.Fprintf(&b, "允许的能力标签 (allowed capability tags): %s\n", strings.Join(request.Constraints.CapabilityTags, ", "))
	fmt.Fprintf(&b, "允许的验证器 id (allowed verifier ids): %s\n\n", strings.Join(request.Constraints.VerifierIDs, ", "))
	b.WriteString("项目关卡 (project gates):\n")
	for _, gate := range request.ProjectGates {
		fmt.Fprintf(&b, "- %s (%s); acceptance=%s; verifiers=%s\n",
			gate.ID, gate.Title,
			strings.Join(gate.AcceptanceCriteria, "; "),
			strings.Join(gate.VerifierIDs, ", "))
	}
	fmt.Fprintf(&b, "\n图限制: 最多 %d 个任务, 最多 %d 条依赖, 最大依赖链深度 %d。\n\n", p.maxTasks, p.maxDependencies, p.maxDepth)

	// A worked example is worth more than a field list: a model omitting a
	// required array is the single most common way a plan fails validation,
	// and showing one filled-in task fixes it far more reliably than
	// describing the schema again.
	b.WriteString("每个任务的所有字段都是必填的，数组都不能为空。示例（只是格式示范，不要照抄内容）：\n")
	fmt.Fprintf(&b, `{
  "tasks": [
    {
      "id": "inspect-readme",
      "title": "检查 README 现状",
      "description": "读取 README，确认快速开始章节需要补充哪些内容。",
      "path_scopes": [%q],
      "acceptance_criteria": ["已记录 README 缺少的章节内容"],
      "verifier_ids": [%q],
      "capability_tags": [%q]
    }
  ],
  "dependencies": [{"task_id": "write-readme", "depends_on": "inspect-readme"}]
}
`, firstOr(request.Constraints.PathScopes, "README.md"),
		firstOr(request.Constraints.VerifierIDs, "ago-verifier"),
		firstOr(request.Constraints.CapabilityTags, "repo-read"))
	b.WriteString("\n只返回 tasks 和 dependencies 两个字段。不要返回 schema_version、repository、objective 或 project_gates：这些由系统提供，你写了也会被忽略。")
	b.WriteString("写文件的任务必须在 path_scopes 中列出它要修改的具体文件，不能列出全部允许范围。")
	return b.String()
}

// buildCorrectionPrompt appends the (redacted) validation failure to the
// original prompt and asks for a single corrected plan. This is the one
// follow-up the planner is allowed to make.
func (p *Planner) buildCorrectionPrompt(previousUser string, validationErr error) string {
	var b strings.Builder
	b.WriteString(previousUser)
	b.WriteString("\n\n上一次返回的 JSON 未通过验证，错误如下:\n")
	b.WriteString(p.redactor.String(validationErr.Error()))
	b.WriteString("\n请修正后重新返回一个满足以上所有约束的 JSON 对象。")
	return p.redactor.String(b.String())
}

// firstOr returns the first value or a fallback, for building an example the
// model can copy the shape of.
func firstOr(values []string, fallback string) string {
	if len(values) > 0 {
		return values[0]
	}
	return fallback
}
