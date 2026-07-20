// Package agoplanner defines the bounded objective-to-DAG planning contract.
// It is persistence and model neutral: planners propose work, and callers must
// validate the proposal against the request before admitting it to a work graph.
package agoplanner

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

const (
	SchemaVersion         = 1
	MaxTasks              = 32
	MaxDependencies       = 128
	MaxProjectGates       = 16
	MaxItemsPerTask       = 16
	MaxAcceptancePerGate  = 16
	MaxVerifierIDsPerGate = 8
	MaxEncodedPlanBytes   = 256 * 1024
)

type Repository struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}

type Objective struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

type ProjectGate struct {
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	VerifierIDs        []string `json:"verifier_ids"`
}

type Constraints struct {
	PathScopes      []string `json:"path_scopes"`
	CapabilityTags  []string `json:"capability_tags"`
	VerifierIDs     []string `json:"verifier_ids"`
	MaxTasks        int      `json:"max_tasks,omitempty"`
	MaxDependencies int      `json:"max_dependencies,omitempty"`
}

type Request struct {
	Repository   Repository    `json:"repository"`
	Objective    Objective     `json:"objective"`
	ProjectGates []ProjectGate `json:"project_gates"`
	Constraints  Constraints   `json:"constraints"`
}

type Plan struct {
	SchemaVersion int                  `json:"schema_version"`
	Repository    Repository           `json:"repository"`
	Objective     Objective            `json:"objective"`
	Tasks         []TaskProposal       `json:"tasks"`
	Dependencies  []DependencyProposal `json:"dependencies"`
	ProjectGates  []ProjectGate        `json:"project_gates"`
}

type TaskProposal struct {
	ID                 string   `json:"id"`
	Title              string   `json:"title"`
	Description        string   `json:"description"`
	PathScopes         []string `json:"path_scopes"`
	AcceptanceCriteria []string `json:"acceptance_criteria"`
	VerifierIDs        []string `json:"verifier_ids"`
	CapabilityTags     []string `json:"capability_tags"`
}

type DependencyProposal struct {
	TaskID    string `json:"task_id"`
	DependsOn string `json:"depends_on"`
}

type Planner interface {
	Plan(context.Context, Request) (Plan, error)
}

type PlannerFunc func(context.Context, Request) (Plan, error)

func (fn PlannerFunc) Plan(ctx context.Context, request Request) (Plan, error) {
	return fn(ctx, request)
}

// FixturePlanner returns a defensive copy of one deterministic proposal. It is
// intended for tests and local fixtures, while exercising the same validation
// boundary as a model-backed planner.
type FixturePlanner struct {
	Proposal Plan
}

func (planner FixturePlanner) Plan(ctx context.Context, request Request) (Plan, error) {
	if err := ctx.Err(); err != nil {
		return Plan{}, err
	}
	proposal := clonePlan(planner.Proposal)
	if err := proposal.Validate(request); err != nil {
		return Plan{}, err
	}
	return proposal, nil
}

func (plan Plan) Validate(request Request) error {
	if err := request.validate(); err != nil {
		return fmt.Errorf("invalid planner request: %w", err)
	}
	if plan.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported planner schema version %d", plan.SchemaVersion)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return fmt.Errorf("encode plan: %w", err)
	}
	if len(encoded) > MaxEncodedPlanBytes {
		return fmt.Errorf("encoded plan exceeds maximum of %d bytes", MaxEncodedPlanBytes)
	}
	if plan.Repository != request.Repository || plan.Objective != request.Objective {
		return fmt.Errorf("plan repository and objective must match the request")
	}
	maxTasks := boundedLimit(request.Constraints.MaxTasks, MaxTasks)
	maxDependencies := boundedLimit(request.Constraints.MaxDependencies, MaxDependencies)
	if len(plan.Tasks) == 0 || len(plan.Tasks) > maxTasks {
		return fmt.Errorf("plan must contain 1..%d tasks", maxTasks)
	}
	if len(plan.Dependencies) > maxDependencies {
		return fmt.Errorf("plan exceeds maximum of %d dependencies", maxDependencies)
	}
	if len(plan.ProjectGates) == 0 || len(plan.ProjectGates) > MaxProjectGates {
		return fmt.Errorf("plan must contain 1..%d project gates", MaxProjectGates)
	}
	if !sameGates(plan.ProjectGates, request.ProjectGates) {
		return fmt.Errorf("plan project gates must match the request")
	}

	tasks := make(map[string]struct{}, len(plan.Tasks))
	for _, task := range plan.Tasks {
		if blank(task.ID) || blank(task.Title) || blank(task.Description) {
			return fmt.Errorf("task id, title, and description are required")
		}
		if _, exists := tasks[task.ID]; exists {
			return fmt.Errorf("duplicate task id %q", task.ID)
		}
		tasks[task.ID] = struct{}{}
		if err := requiredBoundedStrings("task acceptance criteria", task.AcceptanceCriteria, MaxItemsPerTask); err != nil {
			return fmt.Errorf("task %q: %w", task.ID, err)
		}
		if err := requiredBoundedStrings("task verifier ids", task.VerifierIDs, MaxItemsPerTask); err != nil {
			return fmt.Errorf("task %q: %w", task.ID, err)
		}
		if err := requiredBoundedStrings("task path scopes", task.PathScopes, MaxItemsPerTask); err != nil {
			return fmt.Errorf("task %q: %w", task.ID, err)
		}
		if err := requiredBoundedStrings("task capability tags", task.CapabilityTags, MaxItemsPerTask); err != nil {
			return fmt.Errorf("task %q: %w", task.ID, err)
		}
		for _, scope := range task.PathScopes {
			if !validScope(scope) || !scopeAllowed(scope, request.Constraints.PathScopes) {
				return fmt.Errorf("task %q path scope %q is not allowed", task.ID, scope)
			}
		}
		if err := valuesAllowed("verifier", task.VerifierIDs, request.Constraints.VerifierIDs); err != nil {
			return fmt.Errorf("task %q: %w", task.ID, err)
		}
		if err := valuesAllowed("capability", task.CapabilityTags, request.Constraints.CapabilityTags); err != nil {
			return fmt.Errorf("task %q: %w", task.ID, err)
		}
	}

	edges := make(map[string]struct{}, len(plan.Dependencies))
	adjacency := make(map[string][]string, len(plan.Tasks))
	for _, dependency := range plan.Dependencies {
		if _, ok := tasks[dependency.TaskID]; !ok {
			return fmt.Errorf("dependency task %q not found", dependency.TaskID)
		}
		if _, ok := tasks[dependency.DependsOn]; !ok {
			return fmt.Errorf("dependency prerequisite %q not found", dependency.DependsOn)
		}
		if dependency.TaskID == dependency.DependsOn {
			return fmt.Errorf("task %q cannot depend on itself", dependency.TaskID)
		}
		key := dependency.TaskID + "\x00" + dependency.DependsOn
		if _, exists := edges[key]; exists {
			return fmt.Errorf("duplicate dependency %q -> %q", dependency.TaskID, dependency.DependsOn)
		}
		edges[key] = struct{}{}
		adjacency[dependency.TaskID] = append(adjacency[dependency.TaskID], dependency.DependsOn)
	}
	if hasCycle(tasks, adjacency) {
		return fmt.Errorf("plan dependencies contain a cycle")
	}
	return nil
}

func (request Request) validate() error {
	if blank(request.Repository.ID) || blank(request.Repository.Revision) {
		return fmt.Errorf("repository id and revision are required")
	}
	if blank(request.Objective.ID) || blank(request.Objective.Summary) {
		return fmt.Errorf("objective id and summary are required")
	}
	if len(request.ProjectGates) == 0 || len(request.ProjectGates) > MaxProjectGates {
		return fmt.Errorf("request must contain 1..%d project gates", MaxProjectGates)
	}
	if err := requiredBoundedStrings("allowed path scopes", request.Constraints.PathScopes, MaxItemsPerTask); err != nil {
		return err
	}
	for _, scope := range request.Constraints.PathScopes {
		if !validScope(scope) {
			return fmt.Errorf("invalid allowed path scope %q", scope)
		}
	}
	if err := requiredBoundedStrings("allowed verifier ids", request.Constraints.VerifierIDs, MaxItemsPerTask); err != nil {
		return err
	}
	if err := requiredBoundedStrings("allowed capability tags", request.Constraints.CapabilityTags, MaxItemsPerTask); err != nil {
		return err
	}
	if request.Constraints.MaxTasks < 0 || request.Constraints.MaxTasks > MaxTasks || request.Constraints.MaxDependencies < 0 || request.Constraints.MaxDependencies > MaxDependencies {
		return fmt.Errorf("requested limits exceed planner bounds")
	}
	seen := make(map[string]struct{}, len(request.ProjectGates))
	for _, gate := range request.ProjectGates {
		if blank(gate.ID) || blank(gate.Title) {
			return fmt.Errorf("project gate id and title are required")
		}
		if _, exists := seen[gate.ID]; exists {
			return fmt.Errorf("duplicate project gate id %q", gate.ID)
		}
		seen[gate.ID] = struct{}{}
		if err := requiredBoundedStrings("project gate acceptance criteria", gate.AcceptanceCriteria, MaxAcceptancePerGate); err != nil {
			return fmt.Errorf("project gate %q: %w", gate.ID, err)
		}
		if err := requiredBoundedStrings("project gate verifier ids", gate.VerifierIDs, MaxVerifierIDsPerGate); err != nil {
			return fmt.Errorf("project gate %q: %w", gate.ID, err)
		}
		if err := valuesAllowed("verifier", gate.VerifierIDs, request.Constraints.VerifierIDs); err != nil {
			return fmt.Errorf("project gate %q: %w", gate.ID, err)
		}
	}
	return nil
}

func boundedLimit(value, maximum int) int {
	if value == 0 {
		return maximum
	}
	return value
}

func requiredBoundedStrings(name string, values []string, maximum int) error {
	if len(values) == 0 || len(values) > maximum {
		return fmt.Errorf("%s must contain 1..%d values", name, maximum)
	}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if blank(value) {
			return fmt.Errorf("%s cannot contain blank values", name)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%s contains duplicate %q", name, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func valuesAllowed(kind string, values, allowed []string) error {
	set := make(map[string]struct{}, len(allowed))
	for _, value := range allowed {
		set[value] = struct{}{}
	}
	for _, value := range values {
		if blank(value) {
			return fmt.Errorf("%s cannot be blank", kind)
		}
		if _, ok := set[value]; !ok {
			return fmt.Errorf("%s %q is not allowed", kind, value)
		}
	}
	return nil
}

func validScope(scope string) bool {
	return scope != "" && scope != "." && !strings.HasPrefix(scope, "/") && path.Clean(scope) == scope && scope != ".." && !strings.HasPrefix(scope, "../")
}

func scopeAllowed(scope string, allowed []string) bool {
	for _, root := range allowed {
		if scope == root || strings.HasPrefix(scope, root+"/") {
			return true
		}
	}
	return false
}

func hasCycle(tasks map[string]struct{}, adjacency map[string][]string) bool {
	state := make(map[string]uint8, len(tasks))
	var visit func(string) bool
	visit = func(task string) bool {
		if state[task] == 1 {
			return true
		}
		if state[task] == 2 {
			return false
		}
		state[task] = 1
		for _, dependency := range adjacency[task] {
			if visit(dependency) {
				return true
			}
		}
		state[task] = 2
		return false
	}
	for task := range tasks {
		if visit(task) {
			return true
		}
	}
	return false
}

func sameGates(left, right []ProjectGate) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].ID != right[index].ID || left[index].Title != right[index].Title || !sameStrings(left[index].AcceptanceCriteria, right[index].AcceptanceCriteria) || !sameStrings(left[index].VerifierIDs, right[index].VerifierIDs) {
			return false
		}
	}
	return true
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func blank(value string) bool { return strings.TrimSpace(value) == "" }

func clonePlan(plan Plan) Plan {
	clone := plan
	clone.Tasks = append([]TaskProposal(nil), plan.Tasks...)
	for index := range clone.Tasks {
		clone.Tasks[index].PathScopes = append([]string(nil), plan.Tasks[index].PathScopes...)
		clone.Tasks[index].AcceptanceCriteria = append([]string(nil), plan.Tasks[index].AcceptanceCriteria...)
		clone.Tasks[index].VerifierIDs = append([]string(nil), plan.Tasks[index].VerifierIDs...)
		clone.Tasks[index].CapabilityTags = append([]string(nil), plan.Tasks[index].CapabilityTags...)
	}
	clone.Dependencies = append([]DependencyProposal(nil), plan.Dependencies...)
	clone.ProjectGates = append([]ProjectGate(nil), plan.ProjectGates...)
	for index := range clone.ProjectGates {
		clone.ProjectGates[index].AcceptanceCriteria = append([]string(nil), plan.ProjectGates[index].AcceptanceCriteria...)
		clone.ProjectGates[index].VerifierIDs = append([]string(nil), plan.ProjectGates[index].VerifierIDs...)
	}
	return clone
}
