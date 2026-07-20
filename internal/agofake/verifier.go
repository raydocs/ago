package agofake

import (
	"context"
	"fmt"

	"claudexflow/internal/agoverify"
)

// Verifier is the offline semantic judge.
//
// It is a separate type from Provider on purpose. When one object served as
// both executor and verifier, acceptance was self-certification wearing a
// second identity: the "verifier" accepted because the worker's own summary was
// non-empty. Splitting them means the offline demo exercises the same shape as
// the real one — evidence in, verdict out, with no access to whatever the
// executor was thinking.
//
// It may share a read-only script with the executor, because a demo needs
// predictable outcomes. It may not share state: everything it decides comes
// from the evidence it is handed.
type Verifier struct {
	script Script
}

// NewVerifier builds the offline judge.
func NewVerifier(script Script) (*Verifier, error) {
	if script.Default != "" && !script.Default.Valid() {
		return nil, fmt.Errorf("unknown default outcome %q", script.Default)
	}
	for taskID, outcome := range script.ByTask {
		if !outcome.Valid() {
			return nil, fmt.Errorf("task %q has unknown outcome %q", taskID, outcome)
		}
	}
	return &Verifier{script: script}, nil
}

// Judge decides from the evidence it was given.
//
// Note what it does NOT do: accept because a summary or an artifact is present.
// That was the old behaviour, and it meant a worker could pass by asserting it
// had finished. Every criterion is answered, and a criterion is only marked
// passed when something in the evidence supports it.
func (verifier *Verifier) Judge(ctx context.Context, input agoverify.JudgeInput) (agoverify.JudgeVerdict, error) {
	if err := ctx.Err(); err != nil {
		return agoverify.JudgeVerdict{}, err
	}
	// The scripted rejection: the first attempt is sent back with feedback so a
	// demo can show repair happening.
	if verifier.script.outcomeFor(input.TaskID) == OutcomeVerifierRetryWithFeedback && firstAttempt(input) {
		return agoverify.JudgeVerdict{
			Decision:       agoverify.DecisionRetry,
			Summary:        fmt.Sprintf("任务《%s》的证据未覆盖全部验收标准。", input.TaskTitle),
			RepairFeedback: "请补充证明每条验收标准的证据，特别是测试结果。",
			Criteria:       criteriaOutcomes(input, false, "首次提交的证据不足"),
		}, nil
	}

	// A criterion passes only when the evidence contains something that could
	// support it: a passing test, a recorded command, or a changed file.
	supported := len(input.Evidence.Tests) > 0 || len(input.Evidence.Commands) > 0 || len(input.Evidence.ChangedFiles) > 0
	if !supported {
		return agoverify.JudgeVerdict{
			Decision:       agoverify.DecisionRetry,
			Summary:        "证据中没有任何可核对的记录。",
			RepairFeedback: "请提供变更文件、命令或测试结果。",
			Criteria:       criteriaOutcomes(input, false, "没有可核对的证据"),
		}, nil
	}
	return agoverify.JudgeVerdict{
		Decision: agoverify.DecisionAccept,
		Summary:  fmt.Sprintf("任务《%s》的证据满足全部验收标准。", input.TaskTitle),
		Criteria: criteriaOutcomes(input, true, "证据支持该标准"),
	}, nil
}

// criteriaOutcomes answers every criterion, which is what the pipeline requires
// before it will honour an accept.
func criteriaOutcomes(input agoverify.JudgeInput, passed bool, reason string) []agoverify.CriterionOutcome {
	outcomes := make([]agoverify.CriterionOutcome, 0, len(input.AcceptanceCriteria))
	for _, criterion := range input.AcceptanceCriteria {
		outcomes = append(outcomes, agoverify.CriterionOutcome{
			Criterion: criterion, Passed: passed, Reason: reason,
		})
	}
	return outcomes
}

// firstAttempt reports whether this is the first attempt for its task, which is
// what the scripted rejection keys on. It reads the durable attempt number, not
// anything the executor said about itself.
func firstAttempt(input agoverify.JudgeInput) bool {
	return input.AttemptNumber <= 1
}
