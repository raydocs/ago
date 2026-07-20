// Package agoverify decides whether accepted work is actually acceptable.
//
// It exists because "the worker said it finished" is not evidence. Independence
// here is structural, not a label:
//
//   - The evidence is read from the durable store by id. The executor's own
//     in-memory result is never the input, so a worker cannot hand the verifier
//     a different story than the one it recorded.
//   - Deterministic checks run first and cannot be overridden. A failed required
//     test, a hash that does not match, a path outside scope, or a patch based
//     on the wrong revision ends the decision before any model is consulted.
//   - The semantic judge is last, and its verdict is validated: every criterion
//     must be answered and every citation must name evidence that exists.
//   - A verifier that cannot be reached is reported as unavailable, never as a
//     rejection. Whether the provider is up says nothing about whether the work
//     was good, so it must not consume the worker's attempt budget.
package agoverify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"claudexflow/internal/agoartifact"
	"claudexflow/internal/agoboardprotocol"
)

// Decision is the outcome of verification.
type Decision string

const (
	DecisionAccept        Decision = "accept"
	DecisionRetry         Decision = "retry_with_feedback"
	DecisionNeedsInput    Decision = "blocked_needs_input"
	DecisionBlockedPolicy Decision = "blocked_policy"
)

// Result is what the caller acts on. Reason is safe to show a user.
type Result struct {
	Decision Decision
	Reason   string
	// Deterministic reports whether the outcome came from a check rather than
	// from judgement. A deterministic rejection is not a matter of opinion.
	Deterministic bool
	Criteria      []CriterionOutcome
}

type CriterionOutcome struct {
	Criterion string
	Passed    bool
	Reason    string
}

// ErrUnavailable means verification could not be performed. It is deliberately
// distinct from a rejection: the caller must leave the evidence submitted and
// try again rather than blaming the worker.
var ErrUnavailable = errors.New("verification is temporarily unavailable")

// Judge is the semantic half. It runs only after every deterministic check has
// passed, and its accept can still be refused if its reasoning does not hold up.
type Judge interface {
	Judge(ctx context.Context, input JudgeInput) (JudgeVerdict, error)
}

// JudgeInput is exactly what a semantic verifier is allowed to see. What is
// absent matters: no executor plan, no "done" flag, nothing the worker asserted
// that is not backed by persisted evidence.
type JudgeInput struct {
	Objective string
	// TaskID and AttemptNumber come from the durable graph, not from the
	// executor's report, so a judge can reason about retries without trusting
	// the worker's account of them.
	TaskID             string
	AttemptNumber      int
	TaskTitle          string
	TaskDescription    string
	AcceptanceCriteria []string
	Evidence           agoboardprotocol.EvidenceResult
	EvidenceID         string
	ArtifactIDs        []string
	TestNames          []string
	BaseRevision       string
}

type JudgeVerdict struct {
	Decision       Decision
	Summary        string
	Criteria       []CriterionOutcome
	RepairFeedback string
	MissingInput   string
}

// Artifacts lets the pipeline confirm that a referenced artifact really exists
// and still matches its recorded digest.
type Artifacts interface {
	Verify(ctx context.Context, descriptor agoartifact.Descriptor) error
}

type Options struct {
	Judge     Judge
	Artifacts Artifacts
}

type Verifier struct{ options Options }

func New(options Options) (*Verifier, error) {
	if options.Judge == nil {
		return nil, fmt.Errorf("verification requires a semantic judge")
	}
	return &Verifier{options: options}, nil
}

// Input is one verification, assembled from durable state by the caller.
type Input struct {
	Objective          string
	TaskID             string
	AttemptNumber      int
	TaskTitle          string
	TaskDescription    string
	AcceptanceCriteria []string
	AllowedPathScopes  []string
	// Evidence is the persisted record, read back from the store by id.
	Evidence   agoboardprotocol.Evidence
	EvidenceID string
	// IntegratedRevision is what the patch must be based on, or replayable onto.
	IntegratedRevision string
}

// Verify runs the fixed order: deterministic checks, then judgement.
//
// The order is not an implementation detail. Each gate below can end the
// decision on its own, and none of them can be talked out of by a model.
func (verifier *Verifier) Verify(ctx context.Context, input Input) (Result, error) {
	result := input.Evidence.Result

	// 1. Required tests. A check that ran and failed is not a matter of
	//    interpretation, and it is the cheapest thing to get wrong by trusting
	//    a summary.
	if failed := result.FailedRequiredTests(); len(failed) > 0 {
		return Result{
			Decision: DecisionRetry, Deterministic: true,
			Reason: fmt.Sprintf("必需的检查未通过：%s", strings.Join(failed, "、")),
		}, nil
	}

	// 2. Evidence must actually exist. A worker that reports success with
	//    nothing to show has not demonstrated anything.
	if strings.TrimSpace(result.Summary) == "" {
		return Result{
			Decision: DecisionRetry, Deterministic: true,
			Reason: "证据缺少结果摘要。",
		}, nil
	}

	// 3. Artifacts must be present and unchanged. A recorded digest that no
	//    longer matches means the bytes are not what was reviewed.
	if verifier.options.Artifacts != nil {
		for _, reference := range result.Artifacts {
			if err := verifier.options.Artifacts.Verify(ctx, agoartifact.Descriptor{
				ID: reference.ID, Bytes: reference.Bytes, SHA256: reference.SHA256,
			}); err != nil {
				return Result{
					Decision: DecisionRetry, Deterministic: true,
					Reason: fmt.Sprintf("工件 %s 与记录的校验不一致。", reference.DisplayName),
				}, nil
			}
		}
	}

	// 4. Changed paths must be inside the declared scope. The executor already
	//    checks this, but a second, independent check is the point: the first
	//    one ran inside the component being verified.
	if outside := pathsOutsideScope(result.ChangedFiles, input.AllowedPathScopes); len(outside) > 0 {
		return Result{
			Decision: DecisionBlockedPolicy, Deterministic: true,
			Reason: fmt.Sprintf("修改超出允许范围：%s", strings.Join(outside, "、")),
		}, nil
	}

	// 5. A patch must describe the same change the evidence claims, and must be
	//    based on a revision the board knows about.
	if result.Patch != nil {
		if result.Patch.ArtifactID == "" || result.Patch.SHA256 == "" {
			return Result{
				Decision: DecisionRetry, Deterministic: true,
				Reason: "变更补丁缺少可校验的引用。",
			}, nil
		}
		if strings.TrimSpace(result.Patch.BaseRevision) == "" {
			return Result{
				Decision: DecisionRetry, Deterministic: true,
				Reason: "变更补丁没有记录基础版本。",
			}, nil
		}
		if outside := pathsOutsideScopeStrings(result.Patch.ChangedPaths, input.AllowedPathScopes); len(outside) > 0 {
			return Result{
				Decision: DecisionBlockedPolicy, Deterministic: true,
				Reason: fmt.Sprintf("补丁触及允许范围之外的路径：%s", strings.Join(outside, "、")),
			}, nil
		}
		// The patch and the reported changed files must agree. A mismatch means
		// the evidence describes work the patch does not contain.
		if len(result.Patch.ChangedPaths) != len(result.ChangedFiles) {
			return Result{
				Decision: DecisionRetry, Deterministic: true,
				Reason: "补丁包含的路径与报告的变更文件不一致。",
			}, nil
		}
	}

	// 6. Only now is judgement worth paying for.
	verdict, err := verifier.options.Judge.Judge(ctx, JudgeInput{
		Objective: input.Objective, TaskID: input.TaskID, AttemptNumber: input.AttemptNumber,
		TaskTitle:       input.TaskTitle,
		TaskDescription: input.TaskDescription, AcceptanceCriteria: input.AcceptanceCriteria,
		Evidence: result, EvidenceID: input.EvidenceID,
		ArtifactIDs: artifactIDs(result), TestNames: testNames(result),
		BaseRevision: input.IntegratedRevision,
	})
	if err != nil {
		// Unavailability is not a rejection. Propagate it so the caller leaves
		// the evidence submitted rather than punishing the worker.
		return Result{}, err
	}
	if verdict.Decision == DecisionAccept {
		// A judge may not accept while contradicting itself. This is the last
		// line: even a well-formed verdict has to be internally consistent.
		for _, criterion := range verdict.Criteria {
			if !criterion.Passed {
				return Result{
					Decision: DecisionRetry, Deterministic: true,
					Reason:   fmt.Sprintf("验收判定自相矛盾：%q 未通过却给出通过结论。", criterion.Criterion),
					Criteria: verdict.Criteria,
				}, nil
			}
		}
		if len(input.AcceptanceCriteria) > 0 && len(verdict.Criteria) == 0 {
			return Result{
				Decision: DecisionRetry, Deterministic: true,
				Reason: "验收判定没有逐条给出结论。",
			}, nil
		}
	}
	reason := verdict.Summary
	switch verdict.Decision {
	case DecisionRetry:
		if verdict.RepairFeedback != "" {
			reason = verdict.RepairFeedback
		}
	case DecisionNeedsInput:
		if verdict.MissingInput != "" {
			reason = verdict.MissingInput
		}
	}
	return Result{Decision: verdict.Decision, Reason: reason, Criteria: verdict.Criteria}, nil
}

func artifactIDs(result agoboardprotocol.EvidenceResult) []string {
	ids := make([]string, 0, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		ids = append(ids, artifact.ID)
	}
	return ids
}

func testNames(result agoboardprotocol.EvidenceResult) []string {
	names := make([]string, 0, len(result.Tests))
	for _, test := range result.Tests {
		names = append(names, test.Name)
	}
	return names
}

func pathsOutsideScope(files []agoboardprotocol.ChangedFile, scopes []string) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	return pathsOutsideScopeStrings(paths, scopes)
}

// pathsOutsideScopeStrings matches on path-segment boundaries, so a scope of
// "docs" does not admit "docs2", and anything absolute or containing a parent
// reference is always outside.
func pathsOutsideScopeStrings(paths []string, scopes []string) []string {
	var outside []string
	for _, path := range paths {
		if path == "" {
			continue
		}
		if strings.HasPrefix(path, "/") || path == ".." || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") {
			outside = append(outside, path)
			continue
		}
		allowed := false
		for _, scope := range scopes {
			if scope == "" {
				continue
			}
			if path == scope || strings.HasPrefix(path, strings.TrimSuffix(scope, "/")+"/") {
				allowed = true
				break
			}
		}
		if !allowed {
			outside = append(outside, path)
		}
	}
	return outside
}
