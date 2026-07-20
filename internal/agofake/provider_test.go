package agofake_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claudexflow/internal/agoartifact"
	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agofake"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agoscheduler"
	"claudexflow/internal/agoverify"
)

const chineseObjective = "分析当前仓库，为 README 增加一个快速开始章节，运行相关测试，并生成完成报告。"

// The demo graph's first root task; scripting it is enough to observe each
// outcome without depending on the whole plan.
const firstTask = "identify-commands"

type clock struct{ now time.Time }

func (c *clock) Now() time.Time          { return c.now }
func (c *clock) Advance(d time.Duration) { c.now = c.now.Add(d) }

type fixture struct {
	store     *agoboardstore.Store
	scheduler *agoscheduler.Scheduler
	clock     *clock
	boardID   string
}

func newFixture(t *testing.T, script agofake.Script) *fixture {
	t.Helper()
	store, err := agoboardstore.Open(filepath.Join(t.TempDir(), "fake.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	artifacts, err := agoartifact.Open(agoartifact.Options{Root: filepath.Join(t.TempDir(), "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := agofake.New(script)
	if err != nil {
		t.Fatal(err)
	}
	// Two separate objects: the executor cannot certify its own work.
	judge, err := agofake.NewVerifier(script)
	if err != nil {
		t.Fatal(err)
	}
	verification, err := agoverify.New(agoverify.Options{Judge: judge, Artifacts: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	testClock := &clock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
	runtime := agoboardruntime.New(store, agoplanner.DemoPlanner{}, agoboardruntime.Options{
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: testClock.Now,
	})
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime, Executor: provider.WithArtifacts(artifacts), Verification: verification,
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: testClock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	boardID := "board-fake"
	if _, err := runtime.Create(context.Background(), agoboardruntime.Goal{
		BoardID:    boardID,
		Repository: agoplanner.Repository{ID: t.TempDir(), Revision: "HEAD"},
		Objective:  agoplanner.Objective{ID: "objective", Summary: chineseObjective},
		ProjectGates: []agoplanner.ProjectGate{{
			ID: "gate", Title: "目标验收",
			AcceptanceCriteria: []string{"所有任务通过独立验收"}, VerifierIDs: []string{"ago-verifier"},
		}},
		Constraints: agoplanner.Constraints{
			PathScopes:     []string{"README.md", "docs"},
			CapabilityTags: []string{"repo-read", "repo-write", "tests", "report"},
			VerifierIDs:    []string{"ago-verifier"},
		},
		ExecutionMode: "fake",
	}); err != nil {
		t.Fatal(err)
	}
	return &fixture{store: store, scheduler: scheduler, clock: testClock, boardID: boardID}
}

// settle drives scheduler cycles until the graph stops changing, advancing the
// injected clock so backoff elapses. Nothing sleeps.
func (f *fixture) settle(t *testing.T) agoboardprotocol.Board {
	t.Helper()
	ctx := context.Background()
	previous := uint64(0)
	for range 96 {
		if _, err := f.scheduler.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		board, err := f.store.Board(ctx, f.boardID)
		if err != nil {
			t.Fatal(err)
		}
		if board.Version == previous {
			return board
		}
		previous = board.Version
		f.clock.Advance(time.Minute)
	}
	t.Fatal("the board never settled")
	return agoboardprotocol.Board{}
}

func taskOf(t *testing.T, board agoboardprotocol.Board, id string) agoboardprotocol.Task {
	t.Helper()
	for _, task := range board.Tasks {
		if task.ID == id {
			return task
		}
	}
	t.Fatalf("task %q not found", id)
	return agoboardprotocol.Task{}
}

func attemptsOf(board agoboardprotocol.Board, taskID string) []agoboardprotocol.Attempt {
	var attempts []agoboardprotocol.Attempt
	for _, attempt := range board.Attempts {
		if attempt.TaskID == taskID {
			attempts = append(attempts, attempt)
		}
	}
	return attempts
}

// A fully successful Chinese goal reaches Done through the fake path.
func TestSuccessDrivesTheChineseGoalToDone(t *testing.T) {
	f := newFixture(t, agofake.Script{Default: agofake.OutcomeSuccess})
	board := f.settle(t)
	for _, task := range board.Tasks {
		if task.State != agoboardprotocol.TaskPassed {
			t.Fatalf("task %q = %q, want passed", task.ID, task.State)
		}
	}
	completion, err := f.store.Completion(context.Background(), f.boardID)
	if err != nil || completion.Status != agoboardstore.CompletionPassed {
		t.Fatalf("completion = %#v, %v", completion, err)
	}
	// Every accepted task has exactly one attempt and one accepted evidence.
	for _, task := range board.Tasks {
		if got := len(attemptsOf(board, task.ID)); got != 1 {
			t.Fatalf("task %q ran %d attempts, want 1", task.ID, got)
		}
		if task.AcceptedEvidenceID == "" {
			t.Fatalf("task %q passed without accepted evidence", task.ID)
		}
	}
}

func TestTemporaryFailureThenSuccessRetriesExactlyOnce(t *testing.T) {
	f := newFixture(t, agofake.Script{
		Default: agofake.OutcomeSuccess,
		ByTask:  map[string]agofake.Outcome{firstTask: agofake.OutcomeTemporaryFailureThenSuccess},
	})
	board := f.settle(t)
	task := taskOf(t, board, firstTask)
	if task.State != agoboardprotocol.TaskPassed {
		t.Fatalf("task = %q, want it to pass on its second attempt", task.State)
	}
	attempts := attemptsOf(board, firstTask)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want exactly 2", len(attempts))
	}
	if attempts[0].State != agoboardprotocol.AttemptFailed || attempts[0].FailureClass != agoboardprotocol.FailureTransient {
		t.Fatalf("first attempt = %#v, want a transient failure", attempts[0])
	}
	if attempts[1].State != agoboardprotocol.AttemptPassed {
		t.Fatalf("second attempt = %#v, want passed", attempts[1])
	}
	// The failure stays in the durable history rather than being erased.
	if attempts[0].FailureReason == "" {
		t.Fatal("the first failure lost its recorded reason")
	}
	if attempts[1].Generation <= attempts[0].Generation {
		t.Fatalf("retry generation %d did not advance past %d", attempts[1].Generation, attempts[0].Generation)
	}
}

func TestTerminalOutcomesBlockWithoutExhaustingRetries(t *testing.T) {
	for outcome, wantClass := range map[agofake.Outcome]agoboardprotocol.FailureClass{
		agofake.OutcomePermanentFailure:  agoboardprotocol.FailurePermanent,
		agofake.OutcomeBlockedNeedsInput: agoboardprotocol.FailureNeedsInput,
		agofake.OutcomeBlockedPolicy:     agoboardprotocol.FailurePolicy,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			f := newFixture(t, agofake.Script{
				Default: agofake.OutcomeSuccess,
				ByTask:  map[string]agofake.Outcome{firstTask: outcome},
			})
			board := f.settle(t)
			task := taskOf(t, board, firstTask)
			if task.State != agoboardprotocol.TaskFailed {
				t.Fatalf("task = %q, want failed", task.State)
			}
			if task.FailureClass != wantClass {
				t.Fatalf("failure class = %q, want %q", task.FailureClass, wantClass)
			}
			if task.BlockedReason == "" {
				t.Fatal("a blocked task must carry an actionable reason")
			}
			// A terminal fault stops after one attempt; it does not burn the
			// retry budget.
			if got := len(attemptsOf(board, firstTask)); got != 1 {
				t.Fatalf("attempts = %d, want 1 for a terminal fault", got)
			}
			if !task.NextEligibleAt.IsZero() {
				t.Fatal("a terminal fault scheduled a retry")
			}
		})
	}
}

// A timeout is retryable, so it consumes the bounded budget and then stops.
func TestTimeoutRetriesToTheBoundThenStops(t *testing.T) {
	f := newFixture(t, agofake.Script{
		Default: agofake.OutcomeSuccess,
		ByTask:  map[string]agofake.Outcome{firstTask: agofake.OutcomeTimeout},
	})
	board := f.settle(t)
	task := taskOf(t, board, firstTask)
	if task.State != agoboardprotocol.TaskFailed {
		t.Fatalf("task = %q, want failed after exhausting retries", task.State)
	}
	if task.FailureClass != agoboardprotocol.FailureExhausted {
		t.Fatalf("failure class = %q, want %q", task.FailureClass, agoboardprotocol.FailureExhausted)
	}
	attempts := attemptsOf(board, firstTask)
	if len(attempts) != agoboardprotocol.MaxAttempts {
		t.Fatalf("attempts = %d, want the bound of %d", len(attempts), agoboardprotocol.MaxAttempts)
	}
	for index, attempt := range attempts {
		if attempt.Number != index+1 {
			t.Fatalf("attempt %d has number %d", index, attempt.Number)
		}
		if attempt.FailureClass != agoboardprotocol.FailureTransient {
			t.Fatalf("attempt %d class = %q, want transient", index, attempt.FailureClass)
		}
	}
}

// A verifier rejection produces a legal later attempt carrying the feedback.
func TestVerifierRetryWithFeedbackCreatesALegalLaterAttempt(t *testing.T) {
	f := newFixture(t, agofake.Script{
		Default: agofake.OutcomeSuccess,
		ByTask:  map[string]agofake.Outcome{firstTask: agofake.OutcomeVerifierRetryWithFeedback},
	})
	board := f.settle(t)
	task := taskOf(t, board, firstTask)
	if task.State != agoboardprotocol.TaskPassed {
		t.Fatalf("task = %q, want passed after acting on feedback", task.State)
	}
	attempts := attemptsOf(board, firstTask)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(attempts))
	}
	if attempts[0].FailureClass != agoboardprotocol.FailureVerifierFeedback {
		t.Fatalf("first attempt class = %q, want verifier feedback", attempts[0].FailureClass)
	}
	// The rejected evidence stays inspectable alongside the accepted one.
	var rejected, accepted int
	for _, evidence := range board.Evidence {
		if evidence.TaskID != firstTask {
			continue
		}
		switch evidence.State {
		case agoboardprotocol.EvidenceRejected:
			rejected++
		case agoboardprotocol.EvidenceAccepted:
			accepted++
		}
	}
	if rejected != 1 || accepted != 1 {
		t.Fatalf("evidence history = %d rejected, %d accepted; want 1 and 1", rejected, accepted)
	}
}

// The whole fake path must be usable with no credentials in the environment.
func TestFakeProviderNeedsNoCredentials(t *testing.T) {
	for _, name := range []string{"AGO_PROVIDER_API_KEY", "AGO_PROVIDER_BASE_URL", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	f := newFixture(t, agofake.Script{Default: agofake.OutcomeSuccess})
	board := f.settle(t)
	for _, task := range board.Tasks {
		if task.State != agoboardprotocol.TaskPassed {
			t.Fatalf("task %q = %q; the offline path must not need credentials", task.ID, task.State)
		}
	}
	// Nothing the fake produced may look like a secret.
	for _, evidence := range board.Evidence {
		if strings.Contains(strings.ToLower(evidence.Artifact+evidence.Summary), "key") {
			t.Fatalf("fake evidence mentions a key: %#v", evidence)
		}
	}
}

func TestUnknownScriptedOutcomeIsRejected(t *testing.T) {
	if _, err := agofake.New(agofake.Script{Default: "teleport"}); err == nil {
		t.Fatal("an unknown default outcome was accepted")
	}
	if _, err := agofake.New(agofake.Script{ByTask: map[string]agofake.Outcome{"a": "teleport"}}); err == nil {
		t.Fatal("an unknown per-task outcome was accepted")
	}
}

// The worker must never be able to accept its own evidence, whatever the fake
// is scripted to do.
func TestFakeWorkerCannotReviewItsOwnEvidence(t *testing.T) {
	f := newFixture(t, agofake.Script{Default: agofake.OutcomeSuccess})
	board := f.settle(t)
	for _, evidence := range board.Evidence {
		if evidence.WorkerID == "ago-verifier" {
			t.Fatalf("evidence %q was submitted under the verifier identity", evidence.ID)
		}
	}
}
