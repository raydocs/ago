package agogit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
)

func TestServiceRejectsUnsupportedPlacement(t *testing.T) {
	s, thread, workspace, _ := serviceFixture(t)
	for _, target := range []agoprotocol.ExecutorTarget{{Type: agoprotocol.ExecutorOrb}, {Type: agoprotocol.ExecutorRunner, RunnerID: "runner-1"}} {
		_, err := NewService(s).Refresh(context.Background(), RefreshInput{ThreadID: thread, Workspace: workspace, Executor: target, EnvironmentID: "env", ExecutorGeneration: 1, IdempotencyKey: "key"})
		var unsupported *UnsupportedExecutorError
		if !errors.As(err, &unsupported) || unsupported.Target != target.Type {
			t.Fatalf("Refresh(%s) error = %T %v", target.Type, err, err)
		}
	}
	projection, err := s.ClientProjection(context.Background(), thread, 0, 100)
	if err != nil || projection.Diff.Snapshot != nil {
		t.Fatalf("unsupported placement persisted snapshot: %#v, %v", projection.Diff, err)
	}
}

func TestServiceRefreshIsDurableAndTracksLiveState(t *testing.T) {
	s, thread, workspace, path := serviceFixture(t)
	svc := NewService(s)
	in := RefreshInput{ThreadID: thread, Workspace: workspace, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}, EnvironmentID: "env", ExecutorGeneration: 7, IdempotencyKey: "first"}
	first, err := svc.Refresh(context.Background(), in)
	if err != nil || first.Revision != 1 {
		t.Fatalf("first refresh = %#v, %v", first, err)
	}
	var projection map[string]any
	if err := json.Unmarshal(first.Projection, &projection); err != nil {
		t.Fatal(err)
	}
	if projection["schema_version"] != float64(1) || projection["repository_id"] == "" || projection["worktree_id"] == "" {
		t.Fatalf("projection identity = %#v", projection)
	}
	serialized, ok := projection["serialized_index"].(map[string]any)
	if !ok || serialized["exists"] != true || serialized["digest"] == "" || serialized["size"].(float64) <= 0 {
		t.Fatalf("projection serialized index identity = %#v", projection["serialized_index"])
	}
	for _, forbidden := range []string{"workspace", "worktree_dir", "git_dir", "common_dir", "provider", "model"} {
		if _, found := projection[forbidden]; found {
			t.Fatalf("projection exposes %q: %s", forbidden, first.Projection)
		}
	}
	retry := in
	retry.IdempotencyKey = "same-state-new-key"
	same, err := svc.Refresh(context.Background(), retry)
	if err != nil || same.Revision != first.Revision {
		t.Fatalf("same-state refresh = %#v, %v", same, err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "same.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retry.IdempotencyKey = "changed"
	changed, err := svc.Refresh(context.Background(), retry)
	if err != nil || changed.Revision != 2 || changed.Digest == first.Digest {
		t.Fatalf("changed refresh = %#v, %v", changed, err)
	}
	artifact, err := s.GitSnapshotArtifact(context.Background(), changed.ThreadID, changed.ExecutorGeneration, changed.Revision, changed.Digest)
	if err != nil {
		t.Fatal(err)
	}
	privateSnapshot, err := unmarshalMutationArtifact(artifact)
	if err != nil || privateSnapshot.Digest != changed.Digest || len(privateSnapshot.Unstaged) != 1 || len(privateSnapshot.Unstaged[0].Patch) == 0 || len(privateSnapshot.Unstaged[0].Worktree) == 0 {
		t.Fatalf("private mutation artifact = %#v, %v", privateSnapshot, err)
	}
	if bytes.Contains(changed.Projection, privateSnapshot.Unstaged[0].Patch) {
		t.Fatal("client projection leaked the complete private mutation patch")
	}
	var publicSnapshot snapshotProjection
	if err := json.Unmarshal(changed.Projection, &publicSnapshot); err != nil {
		t.Fatal(err)
	}
	if len(publicSnapshot.Unstaged) != 1 || len(publicSnapshot.Unstaged[0].Hunks) != 1 {
		t.Fatalf("public textual projection = %#v", publicSnapshot.Unstaged)
	}
	publicHunk := publicSnapshot.Unstaged[0].Hunks[0]
	privateHunk := privateSnapshot.Unstaged[0].Hunks[0]
	if publicHunk.ID != privateHunk.ID || publicHunk.Header != privateHunk.Header || !strings.Contains(publicHunk.Patch, "-one\n+changed\n") {
		t.Fatalf("public hunk does not preserve review text and mutation identity: public=%#v private=%#v", publicHunk, privateHunk)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = agothreadstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p, err := s.ClientProjection(context.Background(), thread, 0, 100)
	if err != nil || p.Diff.Snapshot == nil || p.Diff.Snapshot.Revision != changed.Revision {
		t.Fatalf("reopened projection = %#v, %v", p.Diff, err)
	}
	artifact, err = s.GitSnapshotArtifact(context.Background(), changed.ThreadID, changed.ExecutorGeneration, changed.Revision, changed.Digest)
	if err != nil || len(artifact) == 0 {
		t.Fatalf("reopened private artifact = %d bytes, %v", len(artifact), err)
	}
}

func TestMarshalProjectionIncludesDuplicateAwareTextHunks(t *testing.T) {
	header := "@@ -10,3 +10,3 @@ repeated"
	snapshot := &Snapshot{
		Digest: "snapshot", HeadOID: "head", IndexDigest: "index",
		Unstaged: []FileChange{{
			ID: "file-id", Path: "review.txt", Status: StatusModified, ContentDigest: "content", MutationSupported: true,
			Hunks: []Hunk{
				{ID: "stable-hunk-one", Header: header, Patch: []byte("diff --git a/review.txt b/review.txt\n--- a/review.txt\n+++ b/review.txt\n" + header + "\n first-context\n-old-one\n+new-one\n trailing-one\n")},
				{ID: "stable-hunk-two", Header: header, Patch: []byte("diff --git a/review.txt b/review.txt\n--- a/review.txt\n+++ b/review.txt\n" + header + "\n second-context\n-old-two\n+new-two\n trailing-two\n")},
			},
		}},
	}
	raw, err := marshalProjection("repository", "worktree", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var projection snapshotProjection
	if err := json.Unmarshal(raw, &projection); err != nil {
		t.Fatal(err)
	}
	if len(projection.Unstaged) != 1 || len(projection.Unstaged[0].Hunks) != 2 {
		t.Fatalf("projected hunks = %#v", projection.Unstaged)
	}
	first, second := projection.Unstaged[0].Hunks[0], projection.Unstaged[0].Hunks[1]
	if first.ID != "stable-hunk-one" || second.ID != "stable-hunk-two" {
		t.Fatalf("mutation IDs changed: %#v", projection.Unstaged[0].Hunks)
	}
	if first.Header != header || first.OldStart != 10 || first.OldLines != 3 || first.NewStart != 10 || first.NewLines != 3 {
		t.Fatalf("first hunk metadata = %#v", first)
	}
	if first.Occurrence != 1 || second.Occurrence != 2 {
		t.Fatalf("duplicate occurrences = %d, %d", first.Occurrence, second.Occurrence)
	}
	if !strings.Contains(first.Patch, "first-context") || strings.Contains(first.Patch, "second-context") || !strings.Contains(second.Patch, "second-context") || strings.Contains(second.Patch, "first-context") {
		t.Fatalf("hunk contexts are not distinct: first=%q second=%q", first.Patch, second.Patch)
	}
	if strings.Contains(first.Patch, "diff --git") || !strings.HasPrefix(first.Patch, header+"\n") {
		t.Fatalf("projection did not isolate exact textual hunk: %q", first.Patch)
	}
}

func TestMarshalProjectionRejectsMalformedAndOversizedText(t *testing.T) {
	validHeader := "@@ -1 +1 @@"
	for _, tc := range []struct {
		name  string
		hunk  Hunk
		large bool
	}{
		{name: "malformed leading hunk", hunk: Hunk{ID: "hunk", Header: validHeader, Patch: []byte("@@ broken @@\n-secret\n" + validHeader + "\n-old\n+new\n")}},
		{name: "range mismatch", hunk: Hunk{ID: "hunk", Header: "@@ -1,2 +1,2 @@", Patch: []byte("@@ -1,2 +1,2 @@\n-old\n+new\n")}},
		{name: "invalid UTF-8", hunk: Hunk{ID: "hunk", Header: validHeader, Patch: append([]byte(validHeader+"\n-old\n+"), 0xff, '\n')}},
		{name: "oversized", large: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hunk := tc.hunk
			if tc.large {
				hunk = Hunk{ID: "hunk", Header: "@@ -0,0 +1 @@", Patch: []byte("@@ -0,0 +1 @@\n+" + strings.Repeat("x", agothreadstore.MaxGitProjectionBytes) + "\n")}
			}
			snapshot := &Snapshot{Digest: "snapshot", HeadOID: "head", IndexDigest: "index", Unstaged: []FileChange{{ID: "file", Path: "unsafe.txt", Hunks: []Hunk{hunk}}}}
			if raw, err := marshalProjection("repository", "worktree", snapshot); err == nil {
				t.Fatalf("unsafe projection succeeded with %d bytes", len(raw))
			}
		})
	}
}

func TestMarshalProjectionNeverExposesBinaryHunks(t *testing.T) {
	secret := "SECRET-BINARY-CONTENT"
	snapshot := &Snapshot{Digest: "snapshot", HeadOID: "head", IndexDigest: "index", Unstaged: []FileChange{{
		ID: "binary-file", Path: "image.dat", Binary: true,
		Hunks: []Hunk{{ID: "binary-hunk", Header: "@@ -1 +1 @@", Patch: []byte("@@ -1 +1 @@\n-" + secret + "\n+replacement\n")}},
	}}}
	raw, err := marshalProjection("repository", "worktree", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(secret)) || bytes.Contains(raw, []byte("binary-hunk")) {
		t.Fatalf("binary hunk leaked into projection: %s", raw)
	}
	var projection snapshotProjection
	if err := json.Unmarshal(raw, &projection); err != nil {
		t.Fatal(err)
	}
	if len(projection.Unstaged) != 1 || !projection.Unstaged[0].Binary || len(projection.Unstaged[0].Hunks) != 0 {
		t.Fatalf("binary projection = %#v", projection.Unstaged)
	}
}

func TestServiceMutationDurablyStagesAndExactRetryDoesNotRepublish(t *testing.T) {
	s, thread, workspace, _ := serviceFixture(t)
	if err := os.WriteFile(filepath.Join(workspace, "same.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := NewService(s)
	snapshot, err := svc.Refresh(context.Background(), RefreshInput{
		ThreadID: thread, Workspace: workspace, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal},
		EnvironmentID: "env", ExecutorGeneration: 7, IdempotencyKey: "mutation-refresh",
	})
	if err != nil {
		t.Fatal(err)
	}
	private, err := s.GitSnapshotArtifact(context.Background(), thread, 7, snapshot.Revision, snapshot.Digest)
	if err != nil {
		t.Fatal(err)
	}
	captured, err := unmarshalMutationArtifact(private)
	if err != nil || len(captured.Unstaged) != 1 {
		t.Fatalf("mutation artifact = %#v, %v", captured, err)
	}
	worktreeBefore, err := os.ReadFile(filepath.Join(workspace, "same.txt"))
	if err != nil {
		t.Fatal(err)
	}
	input := MutationInput{
		ThreadID: thread, Workspace: workspace, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal},
		EnvironmentID: "env", ExecutorGeneration: 7, ActorID: "test", IdempotencyKey: "stage-once", CommandID: "git:stage-once",
		ExpectedSequence:         snapshot.CreatedSequence,
		ExpectedSnapshotRevision: snapshot.Revision, ExpectedSnapshotDigest: snapshot.Digest,
		Kind: MutationStage, SelectedUnitIDs: []string{captured.Unstaged[0].ID},
	}
	result, err := svc.Mutate(context.Background(), input)
	if err != nil || result.Operation.State != agothreadstore.GitOperationCompleted || result.Snapshot.Revision != snapshot.Revision+1 {
		t.Fatalf("Mutate() = %#v, %v", result, err)
	}
	indexAfter, err := os.ReadFile(filepath.Join(workspace, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	retry, err := svc.Mutate(context.Background(), input)
	if err != nil || retry.Operation.OperationID != result.Operation.OperationID || retry.Operation.ResolvedSequence != result.Operation.ResolvedSequence {
		t.Fatalf("retry Mutate() = %#v, %v; want original operation", retry, err)
	}
	indexAfterRetry, _ := os.ReadFile(filepath.Join(workspace, ".git", "index"))
	worktreeAfter, _ := os.ReadFile(filepath.Join(workspace, "same.txt"))
	if !bytes.Equal(indexAfterRetry, indexAfter) || !bytes.Equal(worktreeAfter, worktreeBefore) {
		t.Fatal("exact retry republished the index or changed worktree bytes")
	}
	changed := input
	changed.SelectedUnitIDs = []string{"changed-unit"}
	if _, err := svc.Mutate(context.Background(), changed); err == nil {
		t.Fatal("changed retry did not conflict")
	}
}

func TestServiceMutationPreparedCrashRetryReconcilesWithoutPublication(t *testing.T) {
	s, thread, workspace, _ := serviceFixture(t)
	if err := os.WriteFile(filepath.Join(workspace, "same.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := NewService(s)
	snapshot, err := svc.Refresh(context.Background(), RefreshInput{ThreadID: thread, Workspace: workspace, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}, EnvironmentID: "env", ExecutorGeneration: 7, IdempotencyKey: "crash-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := s.GitSnapshotArtifact(context.Background(), thread, 7, snapshot.Revision, snapshot.Digest)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := unmarshalMutationArtifact(artifact)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := Bind(context.Background(), workspace, ExecutorIdentity{Generation: "7", Environment: "env"})
	if err != nil {
		t.Fatal(err)
	}
	unitID := expected.Unstaged[0].ID
	plan, err := binding.PlanIndexMutation(context.Background(), expected, MutationStage, []string{unitID})
	if err != nil {
		t.Fatal(err)
	}
	head, _ := binding.gitHeadIdentity(context.Background(), expected.HeadOID)
	worktree := gitWorktreeIdentity(plan.AffectedWorktree)
	before := agothreadstore.GitStateIdentity{Version: 1, Head: head, Index: gitIndexIdentity(plan.Before, expected.IndexDigest), Worktree: worktree}
	after := before
	after.Index = gitIndexIdentity(plan.Intended, plan.IntendedSemanticDigest)
	request := agothreadstore.GitOperationSemanticRequest{
		CommandDomain: agothreadstore.GitOperationCommandDomain, CommandVersion: agothreadstore.GitOperationCommandVersion,
		ThreadID: thread, EnvironmentID: "env", ExecutorGeneration: 7, RepositoryID: binding.RepositoryID(), WorktreeID: binding.WorktreeID(), BaseIdentity: binding.BaseIdentity(),
		Kind: agothreadstore.GitOperationStage, ExpectedSnapshotRevision: snapshot.Revision, ExpectedSnapshotDigest: snapshot.Digest,
		SelectedUnitIDs: []string{unitID}, CommandID: "git:prepared-crash",
	}
	prepared, err := s.PrepareGitOperation(context.Background(), agothreadstore.GitOperationPrepareInput{ActorID: "test", IdempotencyKey: "prepared-crash", Request: request, ObjectFormat: binding.ObjectFormat, Before: before, IntendedAfter: after})
	if err != nil {
		t.Fatal(err)
	}
	DiscardIndexMutationPlan(plan) // simulate process death before publication
	indexBefore, _ := os.ReadFile(filepath.Join(workspace, ".git", "index"))
	result, err := svc.Mutate(context.Background(), MutationInput{
		ThreadID: thread, Workspace: workspace, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}, EnvironmentID: "env", ExecutorGeneration: 7,
		ActorID: "test", IdempotencyKey: "prepared-crash", CommandID: "git:prepared-crash", ExpectedSnapshotRevision: snapshot.Revision, ExpectedSnapshotDigest: snapshot.Digest,
		ExpectedSequence: snapshot.CreatedSequence,
		Kind:             MutationStage, SelectedUnitIDs: []string{unitID},
	})
	if err != nil || result.Operation.OperationID != prepared.Operation.OperationID || result.Operation.State != agothreadstore.GitOperationConflicted {
		t.Fatalf("prepared retry = %#v, %v", result, err)
	}
	indexAfter, _ := os.ReadFile(filepath.Join(workspace, ".git", "index"))
	if !bytes.Equal(indexAfter, indexBefore) {
		t.Fatal("prepared crash retry replayed publication")
	}
}

func TestServiceReceiptRevertIsJournaledAndExactRetryDoesNotRepublish(t *testing.T) {
	s, thread, workspace, _ := serviceFixture(t)
	path := filepath.Join(workspace, "same.txt")
	if err := os.WriteFile(path, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := NewService(s)
	snapshot, err := svc.Refresh(context.Background(), RefreshInput{ThreadID: thread, Workspace: workspace, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}, EnvironmentID: "env", ExecutorGeneration: 7, IdempotencyKey: "revert-refresh"})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := Bind(context.Background(), workspace, ExecutorIdentity{Generation: "7", Environment: "env"})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := s.RecordGitWriteReceipt(context.Background(), agothreadstore.GitWriteReceiptInput{
		GitReceiptScope: agothreadstore.GitReceiptScope{ThreadID: thread, EnvironmentID: "env", ExecutorGeneration: 7, RepositoryID: binding.RepositoryID(), WorktreeID: binding.WorktreeID(), BaseIdentity: binding.BaseIdentity()},
		IdempotencyKey:  "write-receipt", OperationID: "tool-operation", ToolCallID: "tool-call", ToolName: "write_file",
		Changes: []agothreadstore.GitReceiptPathChange{{Path: "same.txt", Before: regular("one\n"), After: regular("after\n")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	input := RevertInput{
		ThreadID: thread, Workspace: workspace, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}, EnvironmentID: "env", ExecutorGeneration: 7,
		ActorID: "test", IdempotencyKey: "revert-once", CommandID: "git:revert-once", ExpectedSequence: receipt.CreatedSequence,
		ExpectedSnapshotRevision: snapshot.Revision, ExpectedSnapshotDigest: snapshot.Digest, ReceiptID: receipt.ReceiptID,
	}
	result, err := svc.Revert(context.Background(), input)
	if err != nil || result.Operation.State != agothreadstore.GitOperationCompleted {
		t.Fatalf("Revert() = %#v, %v", result, err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "one\n" {
		t.Fatalf("reverted bytes = %q", got)
	}
	if err := os.WriteFile(path, []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retry, err := svc.Revert(context.Background(), input)
	if err != nil || retry.Operation.OperationID != result.Operation.OperationID || retry.Operation.ResolvedSequence != result.Operation.ResolvedSequence {
		t.Fatalf("retry Revert() = %#v, %v", retry, err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "later\n" {
		t.Fatalf("exact retry replayed revert: %q", got)
	}
}

func TestServiceFailsClosedWithoutPersistence(t *testing.T) {
	s, thread, workspace, _ := serviceFixture(t)
	in := RefreshInput{ThreadID: thread, Workspace: filepath.Join(workspace, "missing"), Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}, EnvironmentID: "env", ExecutorGeneration: 3, IdempotencyKey: "failed"}
	if _, err := NewService(s).Refresh(context.Background(), in); err == nil {
		t.Fatal("Refresh accepted missing repository")
	}
	p, err := s.ClientProjection(context.Background(), thread, 0, 100)
	if err != nil || p.Diff.Snapshot != nil || p.Thread.LastSequence != 1 {
		t.Fatalf("failed capture persisted state: %#v, %v", p, err)
	}
	in.Workspace = workspace
	in.IdempotencyKey = "successful"
	if got, err := NewService(s).Refresh(context.Background(), in); err != nil || got.Revision != 1 {
		t.Fatalf("refresh after failed capture = %#v, %v", got, err)
	}
	retarget := in
	retarget.ExecutorGeneration = 3
	retarget.EnvironmentID = "other-env"
	retarget.IdempotencyKey = "retarget"
	if _, err := NewService(s).Refresh(context.Background(), retarget); err == nil {
		t.Fatal("accepted environment retarget for generation")
	}
}

func serviceFixture(t *testing.T) (*agothreadstore.Store, string, string, string) {
	t.Helper()
	workspace := repo(t)
	path := filepath.Join(t.TempDir(), "threads.db")
	s, err := agothreadstore.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	created, err := s.CreateConfiguredThread(context.Background(), agoprotocol.Command{SchemaVersion: 1, CommandID: "create", IdempotencyKey: "create", ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.ThreadSpec{Workspace: workspace, Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	return s, created.ThreadID, workspace, path
}
