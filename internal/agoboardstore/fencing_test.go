package agoboardstore

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"claudexflow/internal/agoboardprotocol"
)

func workerActor() agoboardprotocol.Actor {
	return agoboardprotocol.Actor{ID: "worker", Role: agoboardprotocol.RoleWorker}
}
func verifierActor() agoboardprotocol.Actor {
	return agoboardprotocol.Actor{ID: "verifier", Role: agoboardprotocol.RoleVerifier}
}

// startAttempt claims a task and moves it to running, returning the claim so a
// test can act as that attempt's executor.
func startAttempt(t *testing.T, store *Store, board agoboardprotocol.Board, commandID string, now time.Time) ClaimResult {
	t.Helper()
	ctx := context.Background()
	request := claimRequest(board, commandID, SlotLimits{GlobalRunning: 4, RepositoryReaders: 4})
	request.Now = now
	claim, err := store.Claim(ctx, request)
	if err != nil || !claim.Dispatchable() {
		t.Fatalf("claim %q = %#v, %v", commandID, claim, err)
	}
	if _, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "start:" + claim.AttemptID,
		ExpectedVersion: claim.Board.Version, Actor: workerActor(),
		Type: agoboardprotocol.CommandAttemptStart, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
		FencingToken: claim.FencingToken,
	}); err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	return claim
}

// An executor whose lease was reclaimed must not be able to submit evidence,
// even though it still holds a token that was once valid.
func TestStaleExecutorEvidenceIsRejectedAfterItsLeaseExpires(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "stale-evidence.db"))
	defer store.Close()
	board := buildClaimBoard(t, store, "stale", "repo", map[string]agoboardprotocol.AccessMode{
		"only": agoboardprotocol.AccessRead,
	})
	stale := startAttempt(t, store, board, "claim:1", testClock)

	// The scheduler reclaims the lease while the executor is still working.
	expired, err := store.ExpireDueLeases(ctx, testClock.Add(2*time.Minute), coordinatorActor())
	if err != nil || len(expired) != 1 {
		t.Fatalf("ExpireDueLeases = %#v, %v; want one expiry", expired, err)
	}
	current, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The old executor finishes and reports success with its original token.
	_, err = store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "late-evidence",
		ExpectedVersion: current.Version, Actor: workerActor(),
		Type: agoboardprotocol.CommandEvidenceSubmit, TaskID: stale.TaskID, AttemptID: stale.AttemptID,
		FencingToken: stale.FencingToken,
		Evidence: &agoboardprotocol.EvidenceSpec{
			ID: "late", TaskID: stale.TaskID, AttemptID: stale.AttemptID,
			Artifact: "artifact://late", Summary: "迟到的证据",
		},
	})
	if err == nil {
		t.Fatal("a superseded executor submitted evidence after its lease was reclaimed")
	}
	after, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Evidence) != 0 {
		t.Fatalf("late evidence was recorded: %#v", after.Evidence)
	}
	if after.Version != current.Version {
		t.Fatalf("a rejected late submission changed the graph from version %d to %d", current.Version, after.Version)
	}
}

// A token from a superseded attempt must not authorize the attempt that
// replaced it.
func TestSupersededTokenCannotActOnTheReplacementAttempt(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "superseded.db"))
	defer store.Close()
	board := buildClaimBoard(t, store, "superseded", "repo", map[string]agoboardprotocol.AccessMode{
		"only": agoboardprotocol.AccessRead,
	})
	first := startAttempt(t, store, board, "claim:1", testClock)
	if _, err := store.ExpireDueLeases(ctx, testClock.Add(2*time.Minute), coordinatorActor()); err != nil {
		t.Fatal(err)
	}
	current, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, task, _ := taskByID(current, "only")
	second := startAttempt(t, store, current, "claim:2", task.NextEligibleAt)

	if second.FencingToken == first.FencingToken {
		t.Fatal("the replacement attempt reused the superseded token")
	}
	if second.Generation <= first.Generation {
		t.Fatalf("generation did not advance: first=%d second=%d", first.Generation, second.Generation)
	}
	running, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The old executor aims its stale token at the new attempt.
	if _, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "forged",
		ExpectedVersion: running.Version, Actor: workerActor(),
		Type: agoboardprotocol.CommandEvidenceSubmit, TaskID: second.TaskID, AttemptID: second.AttemptID,
		FencingToken: first.FencingToken,
		Evidence: &agoboardprotocol.EvidenceSpec{
			ID: "forged", TaskID: second.TaskID, AttemptID: second.AttemptID,
			Artifact: "artifact://forged", Summary: "伪造的证据",
		},
	}); err == nil {
		t.Fatal("a superseded token authorized the attempt that replaced it")
	}
}

// A verifier decision carrying a stale token must not land.
func TestStaleVerifierDecisionIsRejected(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "stale-verdict.db"))
	defer store.Close()
	board := buildClaimBoard(t, store, "verdict", "repo", map[string]agoboardprotocol.AccessMode{
		"only": agoboardprotocol.AccessRead,
	})
	claim := startAttempt(t, store, board, "claim:1", testClock)
	running, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "submit",
		ExpectedVersion: running.Version, Actor: workerActor(),
		Type: agoboardprotocol.CommandEvidenceSubmit, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
		FencingToken: claim.FencingToken,
		Evidence: &agoboardprotocol.EvidenceSpec{
			ID: "evidence", TaskID: claim.TaskID, AttemptID: claim.AttemptID,
			Artifact: "artifact://ok", Summary: "证据",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "stale-accept",
		ExpectedVersion: submitted.Board.Version, Actor: verifierActor(),
		Type: agoboardprotocol.CommandEvidenceAccept, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
		FencingToken: "a-token-this-verifier-never-received",
		Evidence:     &agoboardprotocol.EvidenceSpec{ID: "evidence"},
	}); err == nil {
		t.Fatal("a verifier decision with a stale token was accepted")
	}
	after, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, task, _ := taskByID(after, "only")
	if task.State != agoboardprotocol.TaskVerifying {
		t.Fatalf("task state = %q, want it still verifying after a rejected verdict", task.State)
	}
}

// Once a task is accepted, a late failure from any attempt must not undo it.
func TestAcceptedTaskIsNotRevertedByALateFailure(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "accepted.db"))
	defer store.Close()
	board := buildClaimBoard(t, store, "accepted", "repo", map[string]agoboardprotocol.AccessMode{
		"only": agoboardprotocol.AccessRead,
	})
	claim := startAttempt(t, store, board, "claim:1", testClock)
	running, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "submit",
		ExpectedVersion: running.Version, Actor: workerActor(),
		Type: agoboardprotocol.CommandEvidenceSubmit, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
		FencingToken: claim.FencingToken,
		Evidence: &agoboardprotocol.EvidenceSpec{
			ID: "evidence", TaskID: claim.TaskID, AttemptID: claim.AttemptID,
			Artifact: "artifact://ok", Summary: "证据",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "accept",
		ExpectedVersion: submitted.Board.Version, Actor: verifierActor(),
		Type: agoboardprotocol.CommandEvidenceAccept, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
		FencingToken: claim.FencingToken, Evidence: &agoboardprotocol.EvidenceSpec{ID: "evidence"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The same executor, unaware it already succeeded, now reports a failure.
	if _, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "late-failure",
		ExpectedVersion: accepted.Board.Version, Actor: workerActor(),
		Type: agoboardprotocol.CommandAttemptFail, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
		FencingToken: claim.FencingToken, FailureClass: agoboardprotocol.FailureTransient,
		Reason: "迟到的失败", NextEligibleAt: testClock.Add(time.Minute),
	}); err == nil {
		t.Fatal("a late failure reverted an accepted task")
	}
	final, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, task, _ := taskByID(final, "only")
	if task.State != agoboardprotocol.TaskPassed || task.AcceptedEvidenceID != "evidence" {
		t.Fatalf("accepted task was disturbed: %#v", task)
	}
	// And no further attempt may be claimed for it.
	request := claimRequest(final, "claim:after-accept", SlotLimits{GlobalRunning: 4})
	request.Now = testClock.Add(time.Hour)
	after, err := store.Claim(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if after.Dispatchable() {
		t.Fatal("a completed task was redispatched")
	}
}

// A failure the retry policy cannot fix stops the task immediately, with no
// backoff and no further attempt.
func TestNonRetryableFailureBlocksImmediately(t *testing.T) {
	ctx := context.Background()
	for _, class := range []agoboardprotocol.FailureClass{
		agoboardprotocol.FailureAuth,
		agoboardprotocol.FailurePolicy,
		agoboardprotocol.FailureNeedsInput,
		agoboardprotocol.FailureRepository,
	} {
		t.Run(string(class), func(t *testing.T) {
			store := openStore(t, filepath.Join(t.TempDir(), "terminal.db"))
			defer store.Close()
			board := buildClaimBoard(t, store, "terminal", "repo", map[string]agoboardprotocol.AccessMode{
				"only": agoboardprotocol.AccessRead,
			})
			claim := startAttempt(t, store, board, "claim:1", testClock)
			running, err := store.Board(ctx, board.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
				SchemaVersion: agoboardprotocol.SchemaVersion, ID: "fail",
				ExpectedVersion: running.Version, Actor: workerActor(),
				Type: agoboardprotocol.CommandAttemptFail, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
				FencingToken: claim.FencingToken, FailureClass: class, Reason: "不可重试的失败",
			}); err != nil {
				t.Fatalf("record %s failure: %v", class, err)
			}
			after, err := store.Board(ctx, board.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, task, _ := taskByID(after, "only")
			if task.State != agoboardprotocol.TaskFailed {
				t.Fatalf("%s failure left the task in %q, want failed", class, task.State)
			}
			if task.FailureClass != class {
				t.Fatalf("failure class = %q, want %q preserved", task.FailureClass, class)
			}
			if !task.NextEligibleAt.IsZero() {
				t.Fatalf("%s failure scheduled a retry at %s", class, task.NextEligibleAt)
			}
			// Even with only one attempt spent, no retry may be claimed.
			request := claimRequest(after, "claim:2", SlotLimits{GlobalRunning: 4})
			request.Now = testClock.Add(time.Hour)
			retry, err := store.Claim(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if retry.Dispatchable() {
				t.Fatalf("%s failure was retried", class)
			}
		})
	}
}

// An unclassified failure must not be silently treated as retryable.
func TestUnclassifiedFailureIsRejected(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "unclassified.db"))
	defer store.Close()
	board := buildClaimBoard(t, store, "unclassified", "repo", map[string]agoboardprotocol.AccessMode{
		"only": agoboardprotocol.AccessRead,
	})
	claim := startAttempt(t, store, board, "claim:1", testClock)
	running, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "fail",
		ExpectedVersion: running.Version, Actor: workerActor(),
		Type: agoboardprotocol.CommandAttemptFail, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
		FencingToken: claim.FencingToken, Reason: "没有分类的失败",
	}); err == nil {
		t.Fatal("a failure with no class was accepted")
	}
}

// Backoff deadlines and reconciliation state live in SQLite, so a restart must
// not lose them or let a retry happen early.
func TestRetryDeadlineAndReconciliationSurviveRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart-retry.db")
	store := openStore(t, path)
	board := buildClaimBoard(t, store, "restart", "repo", map[string]agoboardprotocol.AccessMode{
		"only": agoboardprotocol.AccessRead,
	})
	claim := startAttempt(t, store, board, "claim:1", testClock)
	if _, err := store.ExpireDueLeases(ctx, testClock.Add(2*time.Minute), coordinatorActor()); err != nil {
		t.Fatal(err)
	}
	before, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, beforeTask, _ := taskByID(before, "only")
	if beforeTask.State != agoboardprotocol.TaskRetryWait || beforeTask.NextEligibleAt.IsZero() {
		t.Fatalf("task before restart = %#v", beforeTask)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, afterTask, _ := taskByID(after, "only")
	if !afterTask.NextEligibleAt.Equal(beforeTask.NextEligibleAt) {
		t.Fatalf("retry deadline changed across restart: %s then %s", beforeTask.NextEligibleAt, afterTask.NextEligibleAt)
	}
	if afterTask.AttemptCount != 1 {
		t.Fatalf("attempt count after restart = %d, want 1", afterTask.AttemptCount)
	}
	// Still too early to retry.
	early := claimRequest(after, "claim:early", SlotLimits{GlobalRunning: 4})
	early.Now = afterTask.NextEligibleAt.Add(-time.Nanosecond)
	if result, err := reopened.Claim(ctx, early); err != nil {
		t.Fatal(err)
	} else if result.Dispatchable() {
		t.Fatal("a restarted process retried before the durable deadline")
	}
	// The reclaimed lease stays completed; it is not resurrected.
	for _, lease := range after.Leases {
		if lease.ID == claim.LeaseID && lease.State != agoboardprotocol.LeaseCompleted {
			t.Fatalf("reclaimed lease state after restart = %q", lease.State)
		}
	}
	// And the retry itself is fenced with a fresh identity.
	late := claimRequest(after, "claim:late", SlotLimits{GlobalRunning: 4})
	late.Now = afterTask.NextEligibleAt
	retry, err := reopened.Claim(ctx, late)
	if err != nil || !retry.Dispatchable() {
		t.Fatalf("retry after restart = %#v, %v", retry, err)
	}
	if retry.FencingToken == claim.FencingToken || retry.AttemptID == claim.AttemptID {
		t.Fatal("the retry reused the superseded attempt identity")
	}
}
