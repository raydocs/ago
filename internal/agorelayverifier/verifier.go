// Package agorelayverifier is the SEMANTIC half of independent verification.
//
// Historically the "verifier" was the same object as the executor with a
// different ID string, so acceptance was self-certified: whatever the worker
// claimed to have done, it also got to judge. This package replaces that with
// a real second opinion — it asks a model to judge acceptance criteria
// against durable, persisted evidence, and it never trusts the worker's own
// "I think I'm done" assertion.
//
// It is deliberately the LAST step in acceptance. Deterministic gates (for
// example agoboardprotocol.EvidenceResult.RequiredTestsPassed) run first and
// outrank it: a model verdict can never override a failed required test, and
// this package does not attempt to. Its only job is the part a deterministic
// gate cannot do — judging prose acceptance criteria against evidence — and
// it does that job under a fail-closed contract: a malformed response, an
// incomplete one, or one that cites evidence that does not exist is an error,
// never a silent accept.
package agorelayverifier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoredact"
	"claudexflow/internal/agorelay"
)

// defaultMaxCriteria is used when Options.MaxCriteria is unset.
const defaultMaxCriteria = 64

// maxPromptItems bounds how many entries from any single evidence list
// (changed files, commands, tests, artifacts, warnings, artifact ids, test
// names) are rendered into the prompt, so a huge evidence result cannot
// produce an unbounded prompt.
const maxPromptItems = 50

// maxPromptFieldChars bounds how many characters of any single string field
// are rendered into the prompt, for the same reason.
const maxPromptFieldChars = 500

const schemaName = "ago_verifier_verdict"

// verdictJSONSchema mirrors Verdict's JSON tags so the response can be
// unmarshaled directly into that type.
const verdictJSONSchema = `{
  "type": "object",
  "properties": {
    "verdict": {"type": "string", "enum": ["accept", "retry_with_feedback", "blocked_needs_input", "blocked_policy"]},
    "summary": {"type": "string"},
    "criteria": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "criterion": {"type": "string"},
          "passed": {"type": "boolean"},
          "evidence_refs": {"type": "array", "items": {"type": "string"}},
          "reason": {"type": "string"}
        },
        "required": ["criterion", "passed"]
      }
    },
    "repair_feedback": {"type": "string"},
    "missing_input": {"type": "string"},
    "risks": {"type": "array", "items": {"type": "string"}}
  },
  "required": ["verdict", "criteria"]
}`

// Model is the narrow transport slice agorelayverifier needs. It is an
// interface so tests need no network.
type Model interface {
	CompleteJSON(ctx context.Context, request agorelay.Request, target any) error
}

// Request is what the verifier is allowed to see. Note what is ABSENT: the
// executor's internal plan, any "I think I'm done" flag, and anything the
// worker asserted that is not backed by durable evidence.
type Request struct {
	Objective          string
	TaskTitle          string
	TaskDescription    string
	AcceptanceCriteria []string
	// Evidence as persisted, not as the executor held it in memory.
	Evidence agoboardprotocol.EvidenceResult
	// EvidenceID, ArtifactIDs and TestNames are the set of references the model
	// is permitted to cite. A citation outside these is a fabrication.
	EvidenceID   string
	ArtifactIDs  []string
	TestNames    []string
	BaseRevision string
	// PatchText is the change itself, already checked against its recorded
	// digest by the caller. It is what lets a criterion written in prose — a
	// documented example, a report section — actually be judged.
	PatchText string
}

// CriterionVerdict is the model's judgement of one acceptance criterion.
type CriterionVerdict struct {
	Criterion    string   `json:"criterion"`
	Passed       bool     `json:"passed"`
	EvidenceRefs []string `json:"evidence_refs"`
	Reason       string   `json:"reason"`
}

// Verdict is the model's overall judgement. Verdict is one of "accept",
// "retry_with_feedback", "blocked_needs_input", or "blocked_policy".
type Verdict struct {
	Verdict        string             `json:"verdict"`
	Summary        string             `json:"summary"`
	Criteria       []CriterionVerdict `json:"criteria"`
	RepairFeedback string             `json:"repair_feedback"`
	MissingInput   string             `json:"missing_input"`
	Risks          []string           `json:"risks"`
}

// knownVerdicts is the closed set of verdict values Judge accepts. An
// unrecognised value fails closed rather than being coerced into one of
// these.
var knownVerdicts = map[string]bool{
	"accept":              true,
	"retry_with_feedback": true,
	"blocked_needs_input": true,
	"blocked_policy":      true,
}

// Options configures a Verifier. Model is required; Redactor and MaxCriteria
// are optional.
type Options struct {
	Model Model
	// Redactor scrubs every prompt sent to Model and every error returned to
	// the caller. If nil, a Redactor with no extra literals is used — it
	// still strips recognizable credential shapes.
	Redactor *agoredact.Redactor
	// MaxCriteria bounds the response. Zero uses 64.
	MaxCriteria int
}

// Verifier judges acceptance criteria against durable evidence using its own
// model call.
type Verifier struct {
	model       Model
	redactor    *agoredact.Redactor
	maxCriteria int
}

// New validates options and builds a Verifier.
func New(options Options) (*Verifier, error) {
	if options.Model == nil {
		return nil, fmt.Errorf("agorelayverifier: Model is required")
	}
	if options.MaxCriteria < 0 {
		return nil, fmt.Errorf("agorelayverifier: MaxCriteria must not be negative")
	}
	maxCriteria := options.MaxCriteria
	if maxCriteria == 0 {
		maxCriteria = defaultMaxCriteria
	}
	redactor := options.Redactor
	if redactor == nil {
		redactor = agoredact.New()
	}
	return &Verifier{model: options.Model, redactor: redactor, maxCriteria: maxCriteria}, nil
}

// UnavailableError reports that the model provider itself could not be
// reached or is refusing load — a transport failure, a retryable HTTP
// status, or a deadline. A caller should retry verification later; it must
// NOT re-run the worker, because the worker's evidence was never judged.
type UnavailableError struct{ Err error }

func (e UnavailableError) Error() string {
	return fmt.Sprintf("agorelayverifier: verifier provider unavailable: %s", e.Err.Error())
}

func (e UnavailableError) Unwrap() error { return e.Err }

// InvalidVerdictError reports that the model produced a response that cannot
// be honoured: malformed JSON, a missing or unknown verdict, incomplete or
// mismatched criteria coverage, a fabricated citation, an unevidenced pass,
// or an accept that contradicts its own per-criterion verdicts. This is a
// content problem, not a transport problem: retrying the SAME verification
// call may help, but the caller must not treat it like an UnavailableError.
type InvalidVerdictError struct{ Reason string }

func (e InvalidVerdictError) Error() string {
	return fmt.Sprintf("agorelayverifier: invalid verdict: %s", e.Reason)
}

// Judge returns the model's verdict after validating its SHAPE and its
// CITATIONS. It never returns an accept the response did not earn.
func (v *Verifier) Judge(ctx context.Context, request Request) (Verdict, error) {
	if err := ctx.Err(); err != nil {
		return Verdict{}, UnavailableError{Err: err}
	}

	wire := agorelay.Request{
		System:     v.redactor.String(systemPrompt()),
		User:       v.redactor.String(v.buildUserPrompt(request)),
		SchemaName: schemaName,
		Schema:     json.RawMessage(verdictJSONSchema),
	}

	var verdict Verdict
	if err := v.model.CompleteJSON(ctx, wire, &verdict); err != nil {
		return Verdict{}, v.classifyModelError(err)
	}

	if err := v.validateShape(verdict); err != nil {
		return Verdict{}, err
	}
	if err := v.validateCoverage(verdict, request.AcceptanceCriteria); err != nil {
		return Verdict{}, err
	}
	if err := v.validateCitations(verdict, request); err != nil {
		return Verdict{}, err
	}
	if err := v.validateAcceptContradiction(verdict); err != nil {
		return Verdict{}, err
	}
	return verdict, nil
}

// classifyModelError maps a failure from the transport into UnavailableError
// when it is the provider's fault (a retryable HTTP status or a deadline),
// and into InvalidVerdictError otherwise — for example a response the
// transport could not parse as JSON at all. Either way the text is passed
// through the redactor before it can become a returned error.
func (v *Verifier) classifyModelError(err error) error {
	var statusErr agorelay.StatusError
	if errors.As(err, &statusErr) && statusErr.Retryable() {
		return UnavailableError{Err: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return UnavailableError{Err: err}
	}
	return v.invalidVerdictf("model call did not produce a usable verdict: %s", err.Error())
}

// invalidVerdictf builds an InvalidVerdictError whose reason has been passed
// through the redactor, so nothing the model echoed back — or nothing this
// verifier reflects from the request — can leak a credential into an error.
func (v *Verifier) invalidVerdictf(format string, args ...any) error {
	return InvalidVerdictError{Reason: v.redactor.String(fmt.Sprintf(format, args...))}
}

// validateShape enforces requirement 1: a malformed response, a missing or
// unknown verdict value, an empty criterion string, or more than MaxCriteria
// entries is an error — never a silent accept.
func (v *Verifier) validateShape(verdict Verdict) error {
	if !knownVerdicts[verdict.Verdict] {
		return v.invalidVerdictf("response has a missing or unknown verdict value %q", verdict.Verdict)
	}
	if len(verdict.Criteria) > v.maxCriteria {
		return v.invalidVerdictf("response contains %d criteria, exceeding the limit of %d", len(verdict.Criteria), v.maxCriteria)
	}
	for index, criterion := range verdict.Criteria {
		if strings.TrimSpace(criterion.Criterion) == "" {
			return v.invalidVerdictf("criterion at index %d has an empty criterion string", index)
		}
	}
	return nil
}

// validateCoverage enforces requirement 2: every acceptance criterion asked
// about must receive exactly one verdict entry, matched by exact string. A
// response covering fewer criteria, a duplicate, or one naming a criterion
// that was never asked about is rejected rather than accepted.
func (v *Verifier) validateCoverage(verdict Verdict, criteria []string) error {
	expected := make(map[string]bool, len(criteria))
	for _, criterion := range criteria {
		expected[criterion] = false
	}
	for _, criterionVerdict := range verdict.Criteria {
		alreadySeen, wasAsked := expected[criterionVerdict.Criterion]
		if !wasAsked {
			return v.invalidVerdictf("response includes a verdict for criterion %q, which was never asked about", criterionVerdict.Criterion)
		}
		if alreadySeen {
			return v.invalidVerdictf("response covers criterion %q more than once", criterionVerdict.Criterion)
		}
		expected[criterionVerdict.Criterion] = true
	}
	var missing []string
	for criterion, seen := range expected {
		if !seen {
			missing = append(missing, criterion)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return v.invalidVerdictf("response is missing a verdict for criteria: %s", strings.Join(missing, "; "))
	}
	return nil
}

// validateCitations enforces requirements 4: every evidence_refs entry must
// be exactly the EvidenceID, one of ArtifactIDs, or one of TestNames — a
// citation outside that set is a fabrication. A criterion marked passed:true
// with no evidence_refs is rejected too: passing must be evidenced.
func (v *Verifier) validateCitations(verdict Verdict, request Request) error {
	allowed := make(map[string]bool, 1+len(request.ArtifactIDs)+len(request.TestNames))
	if request.EvidenceID != "" {
		allowed[request.EvidenceID] = true
	}
	for _, artifactID := range request.ArtifactIDs {
		allowed[artifactID] = true
	}
	for _, testName := range request.TestNames {
		allowed[testName] = true
	}
	for _, criterionVerdict := range verdict.Criteria {
		if criterionVerdict.Passed && len(criterionVerdict.EvidenceRefs) == 0 {
			return v.invalidVerdictf("criterion %q is marked passed with no evidence_refs", criterionVerdict.Criterion)
		}
		for _, ref := range criterionVerdict.EvidenceRefs {
			if !allowed[ref] {
				return v.invalidVerdictf("criterion %q cites %q, which is not the evidence id, an artifact id, or a test name this verifier was given", criterionVerdict.Criterion, ref)
			}
		}
	}
	return nil
}

// validateAcceptContradiction enforces requirement 3: a response that says
// "verdict":"accept" while any criterion is passed:false is a contradiction.
func (v *Verifier) validateAcceptContradiction(verdict Verdict) error {
	if verdict.Verdict != "accept" {
		return nil
	}
	for _, criterionVerdict := range verdict.Criteria {
		if !criterionVerdict.Passed {
			return v.invalidVerdictf("verdict is accept but criterion %q is passed:false", criterionVerdict.Criterion)
		}
	}
	return nil
}

// systemPrompt is static: it carries no request data, so it needs no
// redaction of its own, but Judge still passes it through the Redactor for
// uniformity.
func systemPrompt() string {
	return "You are an INDEPENDENT verifier for an autonomous coding system. " +
		"You did not do the work, and you must not trust the worker's own claim that it is finished. " +
		"Judge ONLY against the durable evidence provided below — not against any plan, intention, or self-assessment the worker made. " +
		"For every acceptance criterion listed, return exactly one entry in \"criteria\" whose \"criterion\" field is an EXACT copy of that criterion string — do not paraphrase it, do not add criteria that were not listed, and do not omit any. " +
		"Every criterion you mark passed:true MUST cite at least one entry in evidence_refs. " +
		"Every evidence_refs entry MUST be exactly the evidence id, one of the listed artifact ids, or one of the listed test names given to you below — citing anything else is a fabrication and the whole verdict will be rejected. " +
		"Set \"verdict\" to \"accept\" only if every criterion is passed:true; otherwise use \"retry_with_feedback\" with repair_feedback describing what is missing, \"blocked_needs_input\" with missing_input describing what you need from a person, or \"blocked_policy\". " +
		"Respond with ONLY one JSON object. Do not include any explanation outside the JSON object.\n\n" +
		// The wire request also carries a JSON schema, but not every provider
		// honours response_format — one that ignores it invents its own field
		// names ("met", "explanation") and omits "verdict" entirely, which
		// fails closed and wastes the whole verification. Stating the contract
		// in the prompt costs nothing and makes the schema an optimisation
		// rather than a dependency.
		"The object MUST use exactly these keys and no others:\n" +
		"{\n" +
		"  \"verdict\": \"accept\" | \"retry_with_feedback\" | \"blocked_needs_input\" | \"blocked_policy\",\n" +
		"  \"summary\": \"<one sentence>\",\n" +
		"  \"criteria\": [{\"criterion\": \"<exact copy>\", \"passed\": true|false, \"evidence_refs\": [\"<permitted citation>\"], \"reason\": \"<why>\"}],\n" +
		"  \"repair_feedback\": \"<what to fix, when verdict is retry_with_feedback>\",\n" +
		"  \"missing_input\": \"<what a person must supply, when verdict is blocked_needs_input>\",\n" +
		"  \"risks\": [\"<optional>\"]\n" +
		"}\n" +
		"\"verdict\" is REQUIRED. Do not rename \"passed\" to \"met\", do not rename \"reason\" to \"explanation\", and do not nest the object under another key."
}

// buildUserPrompt renders the request into the prompt the model judges
// against. Every evidence list is bounded by maxPromptItems and every string
// field by maxPromptFieldChars before rendering, so a huge evidence result
// cannot produce an unbounded prompt.
func (v *Verifier) buildUserPrompt(request Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Objective: %s\n", boundedField(request.Objective))
	fmt.Fprintf(&b, "Task: %s\n", boundedField(request.TaskTitle))
	fmt.Fprintf(&b, "Task description: %s\n\n", boundedField(request.TaskDescription))

	b.WriteString("Acceptance criteria (every one of these MUST receive exactly one entry in \"criteria\"):\n")
	for index, criterion := range request.AcceptanceCriteria {
		fmt.Fprintf(&b, "%d. %s\n", index+1, boundedField(criterion))
	}

	b.WriteString("\nDurable evidence (this is what actually happened; the worker's own claims are not shown to you):\n")
	fmt.Fprintf(&b, "summary: %s\n", boundedField(request.Evidence.Summary))
	if patch := request.Evidence.Patch; patch != nil {
		fmt.Fprintf(&b, "patch: base_revision=%s bytes=%d sha256=%s changed_paths=%s\n",
			boundedField(patch.BaseRevision), patch.Bytes, boundedField(patch.SHA256),
			strings.Join(boundedStrings(patch.ChangedPaths), ", "))
	}

	b.WriteString("changed files:\n")
	for _, file := range boundedSlice(request.Evidence.ChangedFiles) {
		fmt.Fprintf(&b, "- %s (before=%s after=%s)\n", boundedField(file.Path), boundedField(file.BeforeHash), boundedField(file.AfterHash))
	}

	b.WriteString("commands:\n")
	for _, command := range boundedSlice(request.Evidence.Commands) {
		fmt.Fprintf(&b, "- %s exit=%d duration_ms=%d output_artifact=%s\n",
			boundedField(command.Display), command.ExitCode, command.DurationMS, boundedField(command.OutputArtifactID))
	}

	b.WriteString("tests:\n")
	for _, test := range boundedSlice(request.Evidence.Tests) {
		fmt.Fprintf(&b, "- name=%s command=%s passed=%t exit=%d required=%t\n",
			boundedField(test.Name), boundedField(test.Command), test.Passed, test.ExitCode, test.Required)
	}

	b.WriteString("artifacts:\n")
	for _, artifact := range boundedSlice(request.Evidence.Artifacts) {
		fmt.Fprintf(&b, "- id=%s type=%s name=%s bytes=%d sha256=%s\n",
			boundedField(artifact.ID), boundedField(artifact.Type), boundedField(artifact.DisplayName), artifact.Bytes, boundedField(artifact.SHA256))
	}

	b.WriteString("warnings:\n")
	for _, warning := range boundedStrings(request.Evidence.Warnings) {
		fmt.Fprintf(&b, "- %s\n", warning)
	}

	b.WriteString("\nPermitted citations — every evidence_refs entry MUST be exactly one of these strings:\n")
	fmt.Fprintf(&b, "evidence_id: %s\n", boundedField(request.EvidenceID))
	fmt.Fprintf(&b, "artifact_ids: %s\n", strings.Join(boundedStrings(request.ArtifactIDs), ", "))
	fmt.Fprintf(&b, "test_names: %s\n", strings.Join(boundedStrings(request.TestNames), ", "))
	fmt.Fprintf(&b, "base_revision: %s\n", boundedField(request.BaseRevision))

	if strings.TrimSpace(request.PatchText) != "" {
		b.WriteString("\nThe change itself, as a unified diff. This is the durable content that " +
			"will be applied; judge the criteria against it rather than asking for the file text:\n")
		b.WriteString(boundedPatch(request.PatchText))
		b.WriteString("\n")
	}
	return b.String()
}

// maxPromptPatchChars bounds the rendered diff. The caller already bounds it;
// this is the prompt builder refusing to trust that.
const maxPromptPatchChars = 32 * 1024

func boundedPatch(patch string) string {
	if len(patch) > maxPromptPatchChars {
		return patch[:maxPromptPatchChars] + "\n...[truncated]"
	}
	return patch
}

// boundedSlice caps items at maxPromptItems, so a huge evidence list cannot
// produce an unbounded prompt.
func boundedSlice[T any](items []T) []T {
	if len(items) > maxPromptItems {
		return items[:maxPromptItems]
	}
	return items
}

// boundedField caps a single string field at maxPromptFieldChars.
func boundedField(value string) string {
	if len(value) > maxPromptFieldChars {
		return value[:maxPromptFieldChars] + "...[truncated]"
	}
	return value
}

// boundedStrings caps both the number of strings and the length of each one.
func boundedStrings(values []string) []string {
	limited := boundedSlice(values)
	out := make([]string, len(limited))
	for index, value := range limited {
		out[index] = boundedField(value)
	}
	return out
}
