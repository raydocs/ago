package agorelayverifier_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoredact"
	"claudexflow/internal/agorelay"
	"claudexflow/internal/agorelayverifier"
)

// sentinel is a credential-shaped literal a hostile or careless caller might
// leave sitting in durable evidence. It must never reach the fake model's
// prompt, and it must never reach the text of any returned error.
const sentinel = "sk-ant-SENTINELsecretVALUE0123456789"

// scriptedResponse is one canned reply for a fake Model call: either a
// verdict to encode, raw (possibly malformed) JSON, or an error to return
// directly.
type scriptedResponse struct {
	verdict *agorelayverifier.Verdict
	raw     string
	err     error
}

// fakeModel scripts one response per call and records every prompt it was
// given, so tests can assert what the verifier actually sent — including
// that it never sends a credential.
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
		verdict, ok := target.(*agorelayverifier.Verdict)
		if !ok {
			return errors.New("fakeModel: target is not *agorelayverifier.Verdict")
		}
		*verdict = *response.verdict
		return nil
	}
	return json.Unmarshal([]byte(raw), target)
}

func mustVerifier(t *testing.T, options agorelayverifier.Options) *agorelayverifier.Verifier {
	t.Helper()
	verifier, err := agorelayverifier.New(options)
	if err != nil {
		t.Fatalf("New(%+v) returned unexpected error: %v", options, err)
	}
	return verifier
}

// baseRequest is a valid, otherwise-unremarkable verifier request that every
// test starts from and mutates as needed.
func baseRequest() agorelayverifier.Request {
	return agorelayverifier.Request{
		Objective:          "Add a health check endpoint",
		TaskTitle:          "Implement /healthz",
		TaskDescription:    "Add a GET /healthz endpoint that returns 200 when the service is up.",
		AcceptanceCriteria: []string{"GET /healthz returns 200", "unit tests pass"},
		Evidence: agoboardprotocol.EvidenceResult{
			Summary: "Added handler and tests.",
			Tests: []agoboardprotocol.TestRecord{
				{Name: "TestHealthz", Command: "go test ./...", Passed: true, ExitCode: 0, Required: true},
			},
			Artifacts: []agoboardprotocol.ArtifactRef{
				{ID: "artifact-1", Type: "log", DisplayName: "test-output.log", Bytes: 128, SHA256: "abc123"},
			},
		},
		EvidenceID:   "evidence-1",
		ArtifactIDs:  []string{"artifact-1"},
		TestNames:    []string{"TestHealthz"},
		BaseRevision: "rev-abc123",
	}
}

func acceptAllVerdict(criteria []string) agorelayverifier.Verdict {
	criterionVerdicts := make([]agorelayverifier.CriterionVerdict, 0, len(criteria))
	for _, criterion := range criteria {
		criterionVerdicts = append(criterionVerdicts, agorelayverifier.CriterionVerdict{
			Criterion:    criterion,
			Passed:       true,
			EvidenceRefs: []string{"evidence-1"},
			Reason:       "confirmed by evidence",
		})
	}
	return agorelayverifier.Verdict{
		Verdict:  "accept",
		Summary:  "All criteria satisfied.",
		Criteria: criterionVerdicts,
	}
}

func TestWellFormedAcceptWithAllCriteriaPassedAndValidCitationsIsReturned(t *testing.T) {
	request := baseRequest()
	verdict := acceptAllVerdict(request.AcceptanceCriteria)
	model := &fakeModel{responses: []scriptedResponse{{verdict: &verdict}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})

	got, err := verifier.Judge(context.Background(), request)
	if err != nil {
		t.Fatalf("Judge returned unexpected error: %v", err)
	}
	if got.Verdict != "accept" {
		t.Fatalf("expected verdict %q, got %q", "accept", got.Verdict)
	}
	if len(got.Criteria) != len(request.AcceptanceCriteria) {
		t.Fatalf("expected %d criteria, got %d", len(request.AcceptanceCriteria), len(got.Criteria))
	}
	for _, criterionVerdict := range got.Criteria {
		if !criterionVerdict.Passed {
			t.Fatalf("expected every criterion passed, got %+v", criterionVerdict)
		}
	}
}

func TestAcceptWithACriterionFailedIsRejectedAsContradiction(t *testing.T) {
	request := baseRequest()
	verdict := acceptAllVerdict(request.AcceptanceCriteria)
	verdict.Criteria[1].Passed = false
	verdict.Criteria[1].EvidenceRefs = nil
	model := &fakeModel{responses: []scriptedResponse{{verdict: &verdict}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})

	_, err := verifier.Judge(context.Background(), request)
	assertInvalidVerdictError(t, err)
}

func TestMissingACriterionThatWasAskedAboutIsRejected(t *testing.T) {
	request := baseRequest()
	verdict := acceptAllVerdict(request.AcceptanceCriteria)
	verdict.Criteria = verdict.Criteria[:1] // drop coverage of the second criterion
	verdict.Verdict = "retry_with_feedback"
	verdict.RepairFeedback = "second criterion not addressed"
	model := &fakeModel{responses: []scriptedResponse{{verdict: &verdict}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})

	_, err := verifier.Judge(context.Background(), request)
	assertInvalidVerdictError(t, err)
}

func TestExtraCriterionNeverAskedAboutIsRejected(t *testing.T) {
	request := baseRequest()
	verdict := acceptAllVerdict(request.AcceptanceCriteria)
	verdict.Criteria = append(verdict.Criteria, agorelayverifier.CriterionVerdict{
		Criterion:    "an acceptance criterion nobody asked about",
		Passed:       true,
		EvidenceRefs: []string{"evidence-1"},
	})
	model := &fakeModel{responses: []scriptedResponse{{verdict: &verdict}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})

	_, err := verifier.Judge(context.Background(), request)
	assertInvalidVerdictError(t, err)
}

// TestFabricatedCitationIsRejected is the fabrication guard: a citation
// naming an artifact/test/evidence id that does not exist must be rejected.
func TestFabricatedCitationIsRejected(t *testing.T) {
	request := baseRequest()
	verdict := acceptAllVerdict(request.AcceptanceCriteria)
	verdict.Criteria[0].EvidenceRefs = []string{"artifact-that-does-not-exist"}
	model := &fakeModel{responses: []scriptedResponse{{verdict: &verdict}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})

	_, err := verifier.Judge(context.Background(), request)
	assertInvalidVerdictError(t, err)
}

func TestCriterionPassedTrueWithEmptyEvidenceRefsIsRejected(t *testing.T) {
	request := baseRequest()
	verdict := acceptAllVerdict(request.AcceptanceCriteria)
	verdict.Criteria[0].EvidenceRefs = nil
	model := &fakeModel{responses: []scriptedResponse{{verdict: &verdict}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})

	_, err := verifier.Judge(context.Background(), request)
	assertInvalidVerdictError(t, err)
}

func TestUnknownVerdictValueIsRejected(t *testing.T) {
	request := baseRequest()
	verdict := acceptAllVerdict(request.AcceptanceCriteria)
	verdict.Verdict = "looks_fine"
	model := &fakeModel{responses: []scriptedResponse{{verdict: &verdict}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})

	_, err := verifier.Judge(context.Background(), request)
	assertInvalidVerdictError(t, err)
}

func TestMalformedJSONIsRejected(t *testing.T) {
	request := baseRequest()
	model := &fakeModel{responses: []scriptedResponse{{raw: "{not valid json"}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})

	_, err := verifier.Judge(context.Background(), request)
	assertInvalidVerdictError(t, err)
}

func TestMoreThanMaxCriteriaIsRejected(t *testing.T) {
	request := baseRequest()
	request.AcceptanceCriteria = []string{"only one criterion"}
	verdict := acceptAllVerdict(request.AcceptanceCriteria)
	// Duplicate the one legitimate entry so the response has 2 criteria
	// entries while MaxCriteria is 1 — this must trip the raw count bound,
	// independent of whether the content would otherwise be coherent.
	verdict.Criteria = append(verdict.Criteria, verdict.Criteria[0])
	model := &fakeModel{responses: []scriptedResponse{{verdict: &verdict}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model, MaxCriteria: 1})

	_, err := verifier.Judge(context.Background(), request)
	assertInvalidVerdictError(t, err)
}

func TestRetryWithFeedbackIsReturnedAsIs(t *testing.T) {
	request := baseRequest()
	verdict := acceptAllVerdict(request.AcceptanceCriteria)
	verdict.Verdict = "retry_with_feedback"
	verdict.Criteria[1].Passed = false
	verdict.Criteria[1].EvidenceRefs = []string{"TestHealthz"}
	verdict.RepairFeedback = "unit tests were not actually run"
	model := &fakeModel{responses: []scriptedResponse{{verdict: &verdict}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})

	got, err := verifier.Judge(context.Background(), request)
	if err != nil {
		t.Fatalf("Judge returned unexpected error for a legitimate rejection: %v", err)
	}
	if got.Verdict != "retry_with_feedback" {
		t.Fatalf("expected verdict %q, got %q", "retry_with_feedback", got.Verdict)
	}
	if got.RepairFeedback != "unit tests were not actually run" {
		t.Fatalf("expected repair feedback to be preserved, got %q", got.RepairFeedback)
	}
}

func TestBlockedNeedsInputIsReturnedAsIs(t *testing.T) {
	request := baseRequest()
	verdict := acceptAllVerdict(request.AcceptanceCriteria)
	verdict.Verdict = "blocked_needs_input"
	verdict.Criteria[0].Passed = false
	verdict.Criteria[0].EvidenceRefs = nil
	verdict.MissingInput = "need the target error rate from the user"
	model := &fakeModel{responses: []scriptedResponse{{verdict: &verdict}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})

	got, err := verifier.Judge(context.Background(), request)
	if err != nil {
		t.Fatalf("Judge returned unexpected error for a legitimate block: %v", err)
	}
	if got.Verdict != "blocked_needs_input" {
		t.Fatalf("expected verdict %q, got %q", "blocked_needs_input", got.Verdict)
	}
	if got.MissingInput != "need the target error rate from the user" {
		t.Fatalf("expected missing input to be preserved, got %q", got.MissingInput)
	}
}

func Test429And503AreUnavailableErrorAnd400IsNot(t *testing.T) {
	request := baseRequest()

	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		model := &fakeModel{responses: []scriptedResponse{{err: agorelay.StatusError{Code: code, Message: "retry later"}}}}
		verifier := mustVerifier(t, agorelayverifier.Options{Model: model})
		_, err := verifier.Judge(context.Background(), request)
		var unavailable agorelayverifier.UnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("status %d: expected UnavailableError, got %T: %v", code, err, err)
		}
	}

	model := &fakeModel{responses: []scriptedResponse{{err: agorelay.StatusError{Code: http.StatusBadRequest, Message: "bad request"}}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})
	_, err := verifier.Judge(context.Background(), request)
	var unavailable agorelayverifier.UnavailableError
	if errors.As(err, &unavailable) {
		t.Fatalf("status 400: expected NOT UnavailableError, got %v", err)
	}
	var invalid agorelayverifier.InvalidVerdictError
	if !errors.As(err, &invalid) {
		t.Fatalf("status 400: expected InvalidVerdictError, got %T: %v", err, err)
	}
}

func TestContextDeadlineIsUnavailableError(t *testing.T) {
	request := baseRequest()
	model := &fakeModel{responses: []scriptedResponse{{err: context.DeadlineExceeded}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})

	_, err := verifier.Judge(context.Background(), request)
	var unavailable agorelayverifier.UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("expected UnavailableError, got %T: %v", err, err)
	}
}

func TestContextAlreadyExpiredBeforeCallIsUnavailableError(t *testing.T) {
	request := baseRequest()
	model := &fakeModel{}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model})

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	_, err := verifier.Judge(ctx, request)
	var unavailable agorelayverifier.UnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("expected UnavailableError, got %T: %v", err, err)
	}
	if model.calls != 0 {
		t.Fatalf("expected no model call when context is already expired, got %d calls", model.calls)
	}
}

// TestSentinelSecretNeverReachesPromptOrError is the prompt-hygiene and
// error-hygiene guarantee: a credential-shaped literal sitting in durable
// evidence must never appear in what the fake model receives, and must never
// appear in any returned error.
func TestSentinelSecretNeverReachesPromptOrError(t *testing.T) {
	request := baseRequest()
	request.Evidence.Summary = "Ran with token " + sentinel + " during setup."
	request.Evidence.Warnings = []string{"leaked during a retry: " + sentinel}
	request.Evidence.Commands = []agoboardprotocol.CommandRecord{
		{Display: "curl -H 'Authorization: Bearer " + sentinel + "'", ExitCode: 0},
	}

	redactor := agoredact.New(sentinel)

	// Case 1: a well-formed accept — no error, but the prompt must be clean.
	verdict := acceptAllVerdict(request.AcceptanceCriteria)
	model := &fakeModel{responses: []scriptedResponse{{verdict: &verdict}}}
	verifier := mustVerifier(t, agorelayverifier.Options{Model: model, Redactor: redactor})

	if _, err := verifier.Judge(context.Background(), request); err != nil {
		t.Fatalf("Judge returned unexpected error: %v", err)
	}
	if len(model.prompts) != 1 {
		t.Fatalf("expected exactly 1 prompt, got %d", len(model.prompts))
	}
	if strings.Contains(model.prompts[0].System, sentinel) {
		t.Fatalf("system prompt leaked the sentinel secret: %q", model.prompts[0].System)
	}
	if strings.Contains(model.prompts[0].User, sentinel) {
		t.Fatalf("user prompt leaked the sentinel secret: %q", model.prompts[0].User)
	}

	// Case 2: a rejected verdict (fabricated citation) — the error text must
	// also stay clean, even though it echoes the model's own (attacker
	// controlled) citation string.
	badVerdict := acceptAllVerdict(request.AcceptanceCriteria)
	badVerdict.Criteria[0].EvidenceRefs = []string{"see the token " + sentinel}
	model2 := &fakeModel{responses: []scriptedResponse{{verdict: &badVerdict}}}
	verifier2 := mustVerifier(t, agorelayverifier.Options{Model: model2, Redactor: redactor})

	_, err := verifier2.Judge(context.Background(), request)
	if err == nil {
		t.Fatalf("expected an InvalidVerdictError for a fabricated citation")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("returned error leaked the sentinel secret: %v", err)
	}
}

func assertInvalidVerdictError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an InvalidVerdictError, got nil")
	}
	var invalid agorelayverifier.InvalidVerdictError
	if !errors.As(err, &invalid) {
		t.Fatalf("expected InvalidVerdictError, got %T: %v", err, err)
	}
}
