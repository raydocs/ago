package agoverify_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoverify"
)

// recordingJudge answers whatever it is scripted to, and records whether it was
// asked at all. Whether the judge was consulted is the point of several tests:
// a deterministic failure must end the decision before it costs anything.
type recordingJudge struct {
	verdict agoverify.JudgeVerdict
	err     error
	calls   int
	seen    agoverify.JudgeInput
}

func (judge *recordingJudge) Judge(_ context.Context, input agoverify.JudgeInput) (agoverify.JudgeVerdict, error) {
	judge.calls++
	judge.seen = input
	return judge.verdict, judge.err
}

func acceptAll(criteria []string) agoverify.JudgeVerdict {
	outcomes := make([]agoverify.CriterionOutcome, 0, len(criteria))
	for _, criterion := range criteria {
		outcomes = append(outcomes, agoverify.CriterionOutcome{Criterion: criterion, Passed: true, Reason: "ok"})
	}
	return agoverify.JudgeVerdict{Decision: agoverify.DecisionAccept, Summary: "通过", Criteria: outcomes}
}

func newInput(result agoboardprotocol.EvidenceResult, scopes ...string) agoverify.Input {
	if len(scopes) == 0 {
		scopes = []string{"README.md"}
	}
	return agoverify.Input{
		Objective: "为 README 增加快速开始章节", TaskID: "update-readme", AttemptNumber: 1,
		TaskTitle: "更新 README", AcceptanceCriteria: []string{"README 包含快速开始章节"},
		AllowedPathScopes: scopes,
		Evidence:          agoboardprotocol.Evidence{ID: "evidence-1", Result: result},
		EvidenceID:        "evidence-1",
	}
}

func goodResult() agoboardprotocol.EvidenceResult {
	return agoboardprotocol.EvidenceResult{
		Summary:      "已增加快速开始章节",
		ChangedFiles: []agoboardprotocol.ChangedFile{{Path: "README.md", BeforeHash: "a", AfterHash: "b"}},
		Tests: []agoboardprotocol.TestRecord{{
			Name: "验收测试", Command: "go test ./...", Passed: true, ExitCode: 0, Required: true,
		}},
	}
}

func mustVerifier(t *testing.T, judge agoverify.Judge) *agoverify.Verifier {
	t.Helper()
	verifier, err := agoverify.New(agoverify.Options{Judge: judge})
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

// A worker reporting success with a failing required test cannot be accepted,
// and the judge is never even asked: a check that ran and failed is not a
// matter of opinion.
func TestFailingRequiredTestIsRejectedWithoutConsultingTheJudge(t *testing.T) {
	result := goodResult()
	result.Tests[0].Passed = false
	result.Tests[0].ExitCode = 1
	judge := &recordingJudge{verdict: acceptAll([]string{"README 包含快速开始章节"})}

	outcome, err := mustVerifier(t, judge).Verify(context.Background(), newInput(result))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Decision == agoverify.DecisionAccept {
		t.Fatal("work with a failing required test was accepted")
	}
	if !outcome.Deterministic {
		t.Fatal("the rejection was not recorded as deterministic")
	}
	if judge.calls != 0 {
		t.Fatalf("the judge was consulted %d times despite a failed check", judge.calls)
	}
	if !strings.Contains(outcome.Reason, "验收测试") {
		t.Fatalf("the reason does not name the failing check: %q", outcome.Reason)
	}
}

// A model that says accept while a required test failed cannot override it.
// This is the specific attack: an enthusiastic verifier talking past evidence.
func TestAModelCannotAcceptOverAFailedRequiredTest(t *testing.T) {
	result := goodResult()
	result.Tests[0].Passed = false
	judge := &recordingJudge{verdict: acceptAll([]string{"README 包含快速开始章节"})}
	outcome, err := mustVerifier(t, judge).Verify(context.Background(), newInput(result))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Decision == agoverify.DecisionAccept {
		t.Fatal("a model's accept overrode a deterministic failure")
	}
}

// A worker asserting completion with nothing to show is not evidence.
func TestEmptySummaryIsRejected(t *testing.T) {
	result := goodResult()
	result.Summary = "   "
	judge := &recordingJudge{verdict: acceptAll([]string{"README 包含快速开始章节"})}
	outcome, err := mustVerifier(t, judge).Verify(context.Background(), newInput(result))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Decision == agoverify.DecisionAccept || judge.calls != 0 {
		t.Fatalf("evidence with no summary was accepted (judge calls=%d)", judge.calls)
	}
}

// A change outside the declared scope is refused independently of the executor,
// which is the point: the executor's own check ran inside the thing being
// verified.
func TestChangeOutsideScopeIsRejectedIndependently(t *testing.T) {
	result := goodResult()
	result.ChangedFiles = append(result.ChangedFiles, agoboardprotocol.ChangedFile{Path: "secret.txt", AfterHash: "c"})
	judge := &recordingJudge{verdict: acceptAll([]string{"README 包含快速开始章节"})}

	outcome, err := mustVerifier(t, judge).Verify(context.Background(), newInput(result))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Decision != agoverify.DecisionBlockedPolicy {
		t.Fatalf("decision = %q, want a policy block", outcome.Decision)
	}
	if judge.calls != 0 {
		t.Fatal("the judge was consulted about work that was already out of scope")
	}
}

// A patch whose declared paths disagree with the reported changed files means
// the evidence describes work the patch does not contain.
func TestPatchDisagreeingWithTheEvidenceIsRejected(t *testing.T) {
	for name, patch := range map[string]*agoboardprotocol.PatchRecord{
		"missing digest": {ArtifactID: "a", BaseRevision: "r"},
		"missing base":   {ArtifactID: "a", SHA256: "d"},
		"missing id":     {SHA256: "d", BaseRevision: "r"},
		"path count":     {ArtifactID: "a", SHA256: "d", BaseRevision: "r", ChangedPaths: []string{"README.md", "docs/x.md"}},
		"out of scope":   {ArtifactID: "a", SHA256: "d", BaseRevision: "r", ChangedPaths: []string{"secret.txt"}},
	} {
		t.Run(name, func(t *testing.T) {
			result := goodResult()
			result.Patch = patch
			judge := &recordingJudge{verdict: acceptAll([]string{"README 包含快速开始章节"})}
			outcome, err := mustVerifier(t, judge).Verify(context.Background(), newInput(result))
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Decision == agoverify.DecisionAccept {
				t.Fatalf("%s was accepted", name)
			}
			if judge.calls != 0 {
				t.Fatalf("%s reached the judge", name)
			}
		})
	}
}

// A judge that contradicts itself — accept, with a criterion marked failed —
// is not honoured.
func TestSelfContradictoryAcceptIsRefused(t *testing.T) {
	judge := &recordingJudge{verdict: agoverify.JudgeVerdict{
		Decision: agoverify.DecisionAccept, Summary: "通过",
		Criteria: []agoverify.CriterionOutcome{{Criterion: "README 包含快速开始章节", Passed: false, Reason: "其实没做"}},
	}}
	outcome, err := mustVerifier(t, judge).Verify(context.Background(), newInput(goodResult()))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Decision == agoverify.DecisionAccept {
		t.Fatal("a self-contradictory accept was honoured")
	}
	if !outcome.Deterministic {
		t.Fatal("the contradiction was not treated as a deterministic refusal")
	}
}

// A judge that accepts without answering any criterion has not judged.
func TestAcceptWithoutPerCriterionOutcomesIsRefused(t *testing.T) {
	judge := &recordingJudge{verdict: agoverify.JudgeVerdict{
		Decision: agoverify.DecisionAccept, Summary: "看起来没问题",
	}}
	outcome, err := mustVerifier(t, judge).Verify(context.Background(), newInput(goodResult()))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Decision == agoverify.DecisionAccept {
		t.Fatal("an accept with no per-criterion reasoning was honoured")
	}
}

// A provider being unreachable is not a rejection. Conflating the two would
// punish a worker for someone else's outage.
func TestUnavailableJudgeIsNotARejection(t *testing.T) {
	judge := &recordingJudge{err: agoverify.ErrUnavailable}
	_, err := mustVerifier(t, judge).Verify(context.Background(), newInput(goodResult()))
	if !errors.Is(err, agoverify.ErrUnavailable) {
		t.Fatalf("Verify = %v, want it to report unavailability rather than decide", err)
	}
}

// Clean evidence reaches the judge, and the judge sees only durable facts.
func TestCleanEvidenceReachesTheJudgeWithDurableFactsOnly(t *testing.T) {
	result := goodResult()
	result.Artifacts = []agoboardprotocol.ArtifactRef{{ID: "artifact-1", SHA256: "d", Bytes: 4}}
	judge := &recordingJudge{verdict: acceptAll([]string{"README 包含快速开始章节"})}

	outcome, err := mustVerifier(t, judge).Verify(context.Background(), newInput(result))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Decision != agoverify.DecisionAccept {
		t.Fatalf("clean evidence was not accepted: %+v", outcome)
	}
	if judge.calls != 1 {
		t.Fatalf("judge calls = %d, want exactly one", judge.calls)
	}
	// The judge is told which references it may cite, so a fabricated citation
	// can be detected. It is told the durable attempt number, not anything the
	// executor claimed about itself.
	if len(judge.seen.ArtifactIDs) != 1 || judge.seen.ArtifactIDs[0] != "artifact-1" {
		t.Fatalf("artifact ids = %#v", judge.seen.ArtifactIDs)
	}
	if len(judge.seen.TestNames) != 1 || judge.seen.TestNames[0] != "验收测试" {
		t.Fatalf("test names = %#v", judge.seen.TestNames)
	}
	if judge.seen.EvidenceID != "evidence-1" || judge.seen.TaskID != "update-readme" {
		t.Fatalf("judge input identity = %#v", judge.seen)
	}
}

// A rejection carries the feedback a repair can act on.
func TestRejectionCarriesActionableFeedback(t *testing.T) {
	judge := &recordingJudge{verdict: agoverify.JudgeVerdict{
		Decision: agoverify.DecisionRetry, Summary: "不完整",
		RepairFeedback: "请补充快速开始章节中的安装步骤。",
		Criteria:       []agoverify.CriterionOutcome{{Criterion: "README 包含快速开始章节", Passed: false}},
	}}
	outcome, err := mustVerifier(t, judge).Verify(context.Background(), newInput(goodResult()))
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Decision != agoverify.DecisionRetry {
		t.Fatalf("decision = %q", outcome.Decision)
	}
	if outcome.Reason != "请补充快速开始章节中的安装步骤。" {
		t.Fatalf("reason = %q, want the repair feedback", outcome.Reason)
	}
}
