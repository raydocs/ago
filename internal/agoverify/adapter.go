package agoverify

import (
	"context"
	"errors"
	"fmt"

	"claudexflow/internal/agorelayverifier"
)

// RelayJudge adapts the model-backed verifier to the pipeline's Judge.
//
// The adapter exists so the pipeline never depends on a transport, and so the
// one translation that matters is explicit and testable: an unreachable
// provider becomes ErrUnavailable, which the caller must treat as "try again"
// rather than "the work was bad".
type RelayJudge struct {
	Verifier *agorelayverifier.Verifier
}

func (judge RelayJudge) Judge(ctx context.Context, input JudgeInput) (JudgeVerdict, error) {
	verdict, err := judge.Verifier.Judge(ctx, agorelayverifier.Request{
		Objective: input.Objective, TaskTitle: input.TaskTitle,
		TaskDescription: input.TaskDescription, AcceptanceCriteria: input.AcceptanceCriteria,
		Evidence: input.Evidence, EvidenceID: input.EvidenceID,
		ArtifactIDs: input.ArtifactIDs, TestNames: input.TestNames,
		BaseRevision: input.BaseRevision,
	})
	if err != nil {
		var unavailable agorelayverifier.UnavailableError
		if errors.As(err, &unavailable) {
			// Not a rejection. The caller leaves the evidence submitted.
			return JudgeVerdict{}, fmt.Errorf("%w: %v", ErrUnavailable, err)
		}
		return JudgeVerdict{}, err
	}
	criteria := make([]CriterionOutcome, 0, len(verdict.Criteria))
	for _, criterion := range verdict.Criteria {
		criteria = append(criteria, CriterionOutcome{
			Criterion: criterion.Criterion, Passed: criterion.Passed, Reason: criterion.Reason,
		})
	}
	return JudgeVerdict{
		Decision: Decision(verdict.Verdict), Summary: verdict.Summary, Criteria: criteria,
		RepairFeedback: verdict.RepairFeedback, MissingInput: verdict.MissingInput,
	}, nil
}
