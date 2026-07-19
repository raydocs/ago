package agothreadstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestGitOperationPrepareRetryLookupAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	input := prepareGitOperationFixture(t, store, GitOperationStage)
	input.Request.SelectedUnitIDs = []string{"unstaged-hunk", "unstaged-file"}
	result, err := store.PrepareGitOperation(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Disposition != GitOperationCreated || result.Operation.OperationID == "" || result.Operation.State != GitOperationPrepared {
		t.Fatalf("prepare = %#v", result)
	}
	if got := result.Operation.Request.SelectedUnitIDs; !reflect.DeepEqual(got, []string{"unstaged-file", "unstaged-hunk"}) {
		t.Fatalf("canonical IDs = %#v", got)
	}
	if result.Operation.RequestHash == result.Operation.PlanHash || len(result.Operation.PlanHash) != 64 {
		t.Fatalf("hashes = %#v", result.Operation)
	}

	lookup, found, err := store.LookupGitOperationRetry(context.Background(), input.ActorID, input.IdempotencyKey, input.Request)
	if err != nil || !found || lookup.OperationID != result.Operation.OperationID {
		t.Fatalf("early lookup = %#v, %t, %v", lookup, found, err)
	}
	volatile := input
	volatile.IntendedAfter.Index.SerializedDigest = testDigest("another-plan")
	replay, err := store.PrepareGitOperation(context.Background(), volatile)
	if err != nil || replay.Disposition != GitOperationReplay || replay.Operation.PlanHash != result.Operation.PlanHash {
		t.Fatalf("volatile replay = %#v, %v", replay, err)
	}
	changed := input.Request
	changed.SelectedUnitIDs = []string{"unstaged-file"}
	if _, _, err := store.LookupGitOperationRetry(context.Background(), input.ActorID, input.IdempotencyKey, changed); !isGitOperationConflict(err) {
		t.Fatalf("changed lookup = %v", err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	got, err := store.GitOperation(context.Background(), result.Operation.OperationID)
	if err != nil || !reflect.DeepEqual(got, result.Operation) {
		t.Fatalf("reopen = %#v, %v", got, err)
	}
}

func TestGitOperationConcurrentPrepareHasOneSideEffectOwner(t *testing.T) {
	store := openTestStore(t)
	input := prepareGitOperationFixture(t, store, GitOperationStage)
	const count = 12
	results := make(chan GitOperationPrepareResult, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := store.PrepareGitOperation(context.Background(), input)
			results <- r
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	created := 0
	operationID := ""
	for result := range results {
		if result.Disposition == GitOperationCreated {
			created++
		}
		if operationID == "" {
			operationID = result.Operation.OperationID
		}
		if result.Operation.OperationID != operationID {
			t.Fatalf("operation IDs differ")
		}
	}
	if created != 1 {
		t.Fatalf("created dispositions = %d, want 1", created)
	}
}

func TestGitOperationBindingSnapshotProjectionAndKindValidation(t *testing.T) {
	unborn := GitStateIdentity{Version: GitOperationIdentityV1, Head: GitHeadIdentity{Kind: GitHeadUnborn, OID: "unborn", SymbolicRef: "refs/heads/main"}, Index: GitIndexIdentity{SemanticDigest: testDigest("empty-index")}, Worktree: GitWorktreeIdentity{Scope: "repository-worktree", ManifestDigest: testDigest("empty-worktree")}}
	if err := unborn.validate("sha256"); err != nil {
		t.Fatalf("valid unborn identity rejected: %v", err)
	}
	for _, kind := range []GitOperationKind{GitOperationStage, GitOperationUnstage, GitOperationRevert} {
		t.Run(string(kind), func(t *testing.T) {
			store := openTestStore(t)
			input := prepareGitOperationFixture(t, store, kind)
			if _, err := store.PrepareGitOperation(context.Background(), input); err != nil {
				t.Fatal(err)
			}
		})
	}
	store := openTestStore(t)
	base := prepareGitOperationFixture(t, store, GitOperationStage)
	cases := map[string]func(*GitOperationPrepareInput){
		"base":   func(in *GitOperationPrepareInput) { in.Request.BaseIdentity = "wrong" },
		"format": func(in *GitOperationPrepareInput) { in.ObjectFormat = "sha1" },
		"snapshot-head": func(in *GitOperationPrepareInput) {
			in.Before.Head.OID = strings.Repeat("b", 64)
			in.IntendedAfter.Head = in.Before.Head
		},
		"snapshot-index": func(in *GitOperationPrepareInput) {
			in.Before.Index.SemanticDigest = testDigest("wrong")
			in.IntendedAfter.Index.SemanticDigest = testDigest("after-wrong")
		},
		"wrong-side":     func(in *GitOperationPrepareInput) { in.Request.SelectedUnitIDs = []string{"staged-file"} },
		"protected":      func(in *GitOperationPrepareInput) { in.Request.SelectedUnitIDs = []string{"protected-file"} },
		"unsupported":    func(in *GitOperationPrepareInput) { in.Request.SelectedUnitIDs = []string{"unsupported-file"} },
		"noop":           func(in *GitOperationPrepareInput) { in.IntendedAfter = in.Before },
		"stage-head":     func(in *GitOperationPrepareInput) { in.IntendedAfter.Head.OID = strings.Repeat("c", 64) },
		"stage-worktree": func(in *GitOperationPrepareInput) { in.IntendedAfter.Worktree.ManifestDigest = testDigest("changed") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := base
			in.IdempotencyKey += name
			in.Request.CommandID += name
			mutate(&in)
			if _, err := store.PrepareGitOperation(context.Background(), in); err == nil {
				t.Fatal("invalid prepare accepted")
			}
		})
	}
	revert := base
	revert.Request.Kind = GitOperationRevert
	revert.Request.CommandID += "-revert"
	revert.IdempotencyKey += "-revert"
	revert.IntendedAfter = revert.Before
	revert.IntendedAfter.Worktree.ManifestDigest = testDigest("reverted")
	revert.IntendedAfter.Index.SerializedDigest = testDigest("bad-index")
	if _, err := store.PrepareGitOperation(context.Background(), revert); err == nil {
		t.Fatal("revert changed index")
	}
}

func TestGitOperationUnresolvedExclusionReconciliationAndEvents(t *testing.T) {
	store := openTestStore(t)
	input := prepareGitOperationFixture(t, store, GitOperationStage)
	prepared, err := store.PrepareGitOperation(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	other := input
	other.ActorID = "other"
	other.IdempotencyKey = "other"
	other.Request.CommandID = "git:other"
	if _, err := store.PrepareGitOperation(context.Background(), other); !isGitOperationConflict(err) {
		t.Fatalf("unresolved conflict = %v", err)
	}
	observed := input.Before
	observed.Worktree.ManifestDigest = testDigest("uncertain")
	unknownInput := GitOperationReconcileInput{OperationID: prepared.Operation.OperationID, Observed: observed, Evidence: GitOperationEvidencePostAttempt, Result: json.RawMessage(`{"note":"uncertain"}`)}
	unknown, err := store.ReconcileGitOperation(context.Background(), unknownInput)
	if err != nil || unknown.State != GitOperationOutcomeUnknown || unknown.ResolvedSequence != 0 || unknown.LastTransitionSequence != prepared.Operation.PreparedSequence+1 {
		t.Fatalf("unknown = %#v, %v", unknown, err)
	}
	again, err := store.ReconcileGitOperation(context.Background(), unknownInput)
	if err != nil || !reflect.DeepEqual(again, unknown) {
		t.Fatalf("unknown retry = %#v, %v", again, err)
	}
	thread, _ := store.Thread(context.Background(), input.Request.ThreadID)
	if thread.LastSequence != unknown.LastTransitionSequence {
		t.Fatalf("retry emitted event: sequence %d", thread.LastSequence)
	}
	unresolved, err := store.ListUnresolvedGitOperations(context.Background(), input.Request.ThreadID, input.Request.EnvironmentID, input.Request.ExecutorGeneration)
	if err != nil || len(unresolved) != 1 || unresolved[0].State != GitOperationOutcomeUnknown {
		t.Fatalf("unresolved = %#v, %v", unresolved, err)
	}

	resolve := unknownInput
	resolve.Observed = input.IntendedAfter
	resolve.NoFutureWrite = true
	resolve.Evidence = GitOperationEvidenceOwnerFenced
	resolve.Result = json.RawMessage(`{"applied":true}`)
	completed, err := store.ReconcileGitOperation(context.Background(), resolve)
	if err != nil || completed.State != GitOperationCompleted || completed.ResolvedSequence != completed.LastTransitionSequence {
		t.Fatalf("completed = %#v, %v", completed, err)
	}
	if _, err := store.ReconcileGitOperation(context.Background(), unknownInput); !isGitOperationConflict(err) {
		t.Fatalf("terminal mutation = %v", err)
	}
	unresolved, _ = store.ListUnresolvedGitOperations(context.Background(), input.Request.ThreadID, input.Request.EnvironmentID, input.Request.ExecutorGeneration)
	if len(unresolved) != 0 {
		t.Fatalf("resolved still listed: %#v", unresolved)
	}
	second, err := store.PrepareGitOperation(context.Background(), other)
	if err != nil {
		t.Fatalf("resolved lock not released: %v", err)
	}
	secondObserved := other.Before
	secondObserved.Worktree.ManifestDigest = testDigest("second-uncertain")
	if got, err := store.ReconcileGitOperation(context.Background(), GitOperationReconcileInput{OperationID: second.Operation.OperationID, Observed: secondObserved, Evidence: GitOperationEvidencePostAttempt, Result: json.RawMessage(`{"step":1}`)}); err != nil || got.State != GitOperationOutcomeUnknown {
		t.Fatalf("second unknown = %#v, %v", got, err)
	}
	if got, err := store.ReconcileGitOperation(context.Background(), GitOperationReconcileInput{OperationID: second.Operation.OperationID, Observed: other.Before, Evidence: GitOperationEvidenceOwnerFenced, NoFutureWrite: true, Result: json.RawMessage(`{"step":2}`)}); err != nil || got.State != GitOperationConflicted {
		t.Fatalf("unknown to conflicted = %#v, %v", got, err)
	}

	events, err := store.Replay(context.Background(), input.Request.ThreadID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(string(event.Payload), input.IdempotencyKey) {
			t.Fatalf("event leaked idempotency key: %s", event.Payload)
		}
	}
}

func TestGitOperationPreparedCanReconcileDirectlyAndTerminalIsImmutable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		observed func(GitOperationPrepareInput) GitStateIdentity
		noFuture bool
		want     GitOperationState
	}{
		{"completed", func(in GitOperationPrepareInput) GitStateIdentity { return in.IntendedAfter }, false, GitOperationCompleted},
		{"conflicted", func(in GitOperationPrepareInput) GitStateIdentity { return in.Before }, true, GitOperationConflicted},
		{"unknown-before-not-fenced", func(in GitOperationPrepareInput) GitStateIdentity { return in.Before }, false, GitOperationOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openTestStore(t)
			input := prepareGitOperationFixture(t, store, GitOperationStage)
			prepared, err := store.PrepareGitOperation(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			reconcile := GitOperationReconcileInput{OperationID: prepared.Operation.OperationID, Observed: tc.observed(input), Evidence: GitOperationEvidencePostAttempt, NoFutureWrite: tc.noFuture, Result: json.RawMessage(`{"ok":true}`)}
			got, err := store.ReconcileGitOperation(context.Background(), reconcile)
			if err != nil || got.State != tc.want {
				t.Fatalf("reconcile = %#v, %v", got, err)
			}
			again, err := store.ReconcileGitOperation(context.Background(), reconcile)
			if err != nil || !reflect.DeepEqual(again, got) {
				t.Fatalf("exact retry = %#v, %v", again, err)
			}
		})
	}
}

func TestGitOperationBoundsNamespaceAndRollback(t *testing.T) {
	store := openTestStore(t)
	input := prepareGitOperationFixture(t, store, GitOperationStage)
	invalid := []GitOperationPrepareInput{input, input, input, input}
	invalid[0].Request.SelectedUnitIDs = nil
	invalid[1].Request.SelectedUnitIDs = []string{"x", "x"}
	invalid[2].Request.CommandID = "ordinary-command"
	invalid[3].Before.Index.SerializedDigest = "bad"
	for i, in := range invalid {
		in.IdempotencyKey += fmt.Sprint(i)
		if _, err := store.PrepareGitOperation(context.Background(), in); err == nil {
			t.Fatalf("invalid %d accepted", i)
		}
	}
	tooMany := input
	tooMany.Request.SelectedUnitIDs = make([]string, maxGitOperationIDs+1)
	if _, err := store.PrepareGitOperation(context.Background(), tooMany); err == nil {
		t.Fatal("too many IDs accepted")
	}
	if _, err := store.db.Exec(`INSERT INTO commands(actor_id,idempotency_key,command_id,request_hash,thread_id,result_json) VALUES(?,?,?,?,?,?)`, input.ActorID, input.IdempotencyKey, input.Request.CommandID, []byte("ordinary"), input.Request.ThreadID, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	gitPrepared, err := store.PrepareGitOperation(context.Background(), input)
	if err != nil || gitPrepared.Disposition != GitOperationCreated {
		t.Fatalf("ordinary command domain aliased Git operation: %#v, %v", gitPrepared, err)
	}
	oversizedResult := json.RawMessage(`{"value":"` + strings.Repeat("x", MaxGitOperationJSONBytes) + `"}`)
	if _, err := store.ReconcileGitOperation(context.Background(), GitOperationReconcileInput{OperationID: gitPrepared.Operation.OperationID, Observed: input.Before, Evidence: GitOperationEvidenceOwnerFenced, NoFutureWrite: true, Result: oversizedResult}); err == nil {
		t.Fatal("oversized reconciliation result accepted")
	}
	if _, err := store.ReconcileGitOperation(context.Background(), GitOperationReconcileInput{OperationID: gitPrepared.Operation.OperationID, Observed: input.Before, Evidence: GitOperationEvidenceOwnerFenced, NoFutureWrite: true, Result: json.RawMessage(`{"fenced":true}`)}); err != nil {
		t.Fatal(err)
	}
	rollbackInput := input
	rollbackInput.ActorID = "rollback-actor"
	rollbackInput.IdempotencyKey = "rollback-key"
	rollbackInput.Request.CommandID = "git:rollback-command"
	before, _ := store.Thread(context.Background(), input.Request.ThreadID)
	if _, err := store.db.Exec(`CREATE TRIGGER reject_git_operation BEFORE INSERT ON git_operations BEGIN SELECT RAISE(ABORT, 'test rejection'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PrepareGitOperation(context.Background(), rollbackInput); err == nil {
		t.Fatal("trigger did not reject")
	}
	after, _ := store.Thread(context.Background(), input.Request.ThreadID)
	if after.LastSequence != before.LastSequence {
		t.Fatalf("rollback sequence = %d, want %d", after.LastSequence, before.LastSequence)
	}
}

func prepareGitOperationFixture(t *testing.T, store *Store, kind GitOperationKind) GitOperationPrepareInput {
	t.Helper()
	threadID := mustCreateThread(t, store, "git-operation-"+t.Name()).ThreadID
	head := strings.Repeat("a", 64)
	binding := GitBinding{ThreadID: threadID, EnvironmentID: "env", ExecutorGeneration: 3, WorktreeDir: "/repo", GitDir: "/repo/.git", CommonDir: "/repo/.git", RepositoryID: "repo", WorktreeID: "worktree", ObjectFormat: "sha256", BaseIdentity: "base:v1"}
	if err := store.RecordGitBinding(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	projection := json.RawMessage(`{"schema_version":1,"staged":[{"id":"staged-file","protected":false,"mutation_supported":true,"hunks":[{"id":"staged-hunk"}]}],"unstaged":[{"id":"unstaged-file","protected":false,"mutation_supported":true,"hunks":[{"id":"unstaged-hunk"}]},{"id":"protected-file","protected":true,"mutation_supported":true},{"id":"unsupported-file","protected":false,"mutation_supported":false}]}`)
	snapshot, err := store.RecordGitSnapshot(context.Background(), GitSnapshotInput{ThreadID: threadID, EnvironmentID: "env", ExecutorGeneration: 3, RepositoryID: "repo", WorktreeID: "worktree", IdempotencyKey: "snapshot-" + t.Name(), Digest: testDigest("snapshot-" + t.Name()), HeadOID: head, IndexDigest: testDigest("index-semantic"), Projection: projection})
	if err != nil {
		t.Fatal(err)
	}
	before := GitStateIdentity{Version: GitOperationIdentityV1, Head: GitHeadIdentity{Kind: GitHeadCommit, OID: head, SymbolicRef: "refs/heads/main"}, Index: GitIndexIdentity{Exists: true, SerializedDigest: testDigest("index-exact-before"), Size: 42, SemanticDigest: snapshot.IndexDigest}, Worktree: GitWorktreeIdentity{Scope: "repository-worktree", ManifestDigest: testDigest("worktree-before")}}
	after := before
	selected := []string{"unstaged-file"}
	if kind == GitOperationRevert {
		after.Worktree.ManifestDigest = testDigest("worktree-after")
	} else {
		after.Index.SerializedDigest = testDigest("index-exact-after")
		after.Index.Size = 43
		after.Index.SemanticDigest = testDigest("index-semantic-after")
		if kind == GitOperationUnstage {
			selected = []string{"staged-file"}
		}
	}
	return GitOperationPrepareInput{ActorID: "actor", IdempotencyKey: "git-key-" + t.Name(), ObjectFormat: "sha256", Before: before, IntendedAfter: after, Request: GitOperationSemanticRequest{CommandDomain: GitOperationCommandDomain, CommandVersion: GitOperationCommandVersion, ThreadID: threadID, EnvironmentID: "env", ExecutorGeneration: 3, RepositoryID: "repo", WorktreeID: "worktree", BaseIdentity: "base:v1", Kind: kind, ExpectedSnapshotRevision: snapshot.Revision, ExpectedSnapshotDigest: snapshot.Digest, SelectedUnitIDs: selected, CommandID: "git:command-" + t.Name()}}
}

func isGitOperationConflict(err error) bool {
	var conflict GitOperationConflictError
	return errors.As(err, &conflict)
}
