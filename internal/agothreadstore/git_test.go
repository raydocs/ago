package agothreadstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

func testDigest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func TestGitSnapshotIsDurableIdempotentAndProjectedWithHistoricalComments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	thread := mustCreateThread(t, s, "git-create").ThreadID
	b := GitBinding{ThreadID: thread, EnvironmentID: "env", ExecutorGeneration: 4, WorktreeDir: "/repo", GitDir: "/repo/.git", CommonDir: "/repo/.git", RepositoryID: "repo-id", WorktreeID: "wt-id", ObjectFormat: "sha256", BaseIdentity: "base"}
	if err := s.RecordGitBinding(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	one := GitSnapshotInput{ThreadID: thread, EnvironmentID: "env", ExecutorGeneration: 4, RepositoryID: "repo-id", WorktreeID: "wt-id", IdempotencyKey: "capture-1", Digest: testDigest("one"), HeadOID: "head-1", IndexDigest: testDigest("index-1"), Projection: json.RawMessage(`{"staged":[],"unstaged":[{"id":"file:opaque","hunks":[{"id":"hunk:opaque"}]}]}`)}
	r1, err := s.RecordGitSnapshot(context.Background(), one)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := s.RecordGitSnapshot(context.Background(), one)
	if err != nil || retry.Revision != r1.Revision {
		t.Fatalf("retry = %#v, %v", retry, err)
	}
	sameDigest := one
	sameDigest.IdempotencyKey = "capture-1-again"
	if got, err := s.RecordGitSnapshot(context.Background(), sameDigest); err != nil || got.Revision != r1.Revision {
		t.Fatalf("same digest retry = %#v, %v", got, err)
	}
	changedRetry := one
	changedRetry.Digest = testDigest("changed")
	if _, err := s.RecordGitSnapshot(context.Background(), changedRetry); err == nil {
		t.Fatal("accepted changed retry")
	}
	comment, err := s.AddGitComment(context.Background(), GitCommentInput{ThreadID: thread, CommentID: "comment-1", SnapshotGeneration: r1.ExecutorGeneration, SnapshotRevision: r1.Revision, SnapshotDigest: r1.Digest, FileID: "file:opaque", HunkID: "hunk:opaque", Actor: "alice", Body: "remember this"})
	if err != nil {
		t.Fatal(err)
	}
	retryComment, err := s.AddGitComment(context.Background(), GitCommentInput{ThreadID: thread, CommentID: "comment-1", SnapshotGeneration: r1.ExecutorGeneration, SnapshotRevision: r1.Revision, SnapshotDigest: r1.Digest, FileID: "file:opaque", HunkID: "hunk:opaque", Actor: "alice", Body: "remember this"})
	if err != nil || retryComment.CreatedSequence != comment.CreatedSequence {
		t.Fatalf("comment retry = %#v, %v", retryComment, err)
	}
	if _, err := s.AddGitComment(context.Background(), GitCommentInput{ThreadID: thread, CommentID: "comment-1", SnapshotGeneration: r1.ExecutorGeneration, SnapshotRevision: r1.Revision, SnapshotDigest: r1.Digest, FileID: "file:opaque", HunkID: "hunk:opaque", Actor: "alice", Body: "changed"}); err == nil {
		t.Fatal("accepted changed comment retry")
	}
	two := one
	two.IdempotencyKey = "capture-2"
	two.Digest = testDigest("two")
	two.HeadOID = "head-2"
	r2, err := s.RecordGitSnapshot(context.Background(), two)
	if err != nil || r2.Revision != r1.Revision+1 {
		t.Fatalf("second = %#v, %v", r2, err)
	}
	p, err := s.ClientProjection(context.Background(), thread, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if p.Diff.Snapshot == nil || p.Diff.Snapshot.Revision != r2.Revision || len(p.Diff.Comments) != 0 {
		t.Fatalf("latest diff = %#v", p.Diff)
	}
	if comment.SnapshotRevision != r1.Revision {
		t.Fatalf("comment moved: %#v", comment)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	p, err = s.ClientProjection(context.Background(), thread, 0, 100)
	if err != nil || p.Diff.Snapshot.Revision != 2 {
		t.Fatalf("reopen projection = %#v, %v", p.Diff, err)
	}
}

func TestGitCommentRejectsTargetOutsideExactSnapshot(t *testing.T) {
	s := openTestStore(t)
	thread := mustCreateThread(t, s, "git-comment-target-create").ThreadID
	binding := GitBinding{ThreadID: thread, EnvironmentID: "env", ExecutorGeneration: 1, WorktreeDir: "/w", GitDir: "/g", CommonDir: "/c", RepositoryID: "r", WorktreeID: "w", ObjectFormat: "sha1", BaseIdentity: "b"}
	if err := s.RecordGitBinding(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.RecordGitSnapshot(context.Background(), GitSnapshotInput{ThreadID: thread, EnvironmentID: "env", ExecutorGeneration: 1, RepositoryID: "r", WorktreeID: "w", IdempotencyKey: "snapshot", Digest: testDigest("snapshot"), HeadOID: "h", IndexDigest: testDigest("i"), Projection: json.RawMessage(`{"staged":[{"id":"file-one","hunks":[{"id":"hunk-one"}]}],"unstaged":[]}`)})
	if err != nil {
		t.Fatal(err)
	}
	base := GitCommentInput{ThreadID: thread, CommentID: "comment", SnapshotGeneration: 1, SnapshotRevision: snapshot.Revision, SnapshotDigest: snapshot.Digest, FileID: "file-one", HunkID: "hunk-one", Actor: "alice", Body: "change this"}
	for name, mutate := range map[string]func(*GitCommentInput){
		"unknown file": func(in *GitCommentInput) { in.FileID = "file-missing" },
		"unknown hunk": func(in *GitCommentInput) { in.HunkID = "hunk-missing" },
	} {
		t.Run(name, func(t *testing.T) {
			input := base
			input.CommentID += "-" + name
			mutate(&input)
			if _, err := s.AddGitComment(context.Background(), input); err == nil {
				t.Fatal("accepted comment target outside snapshot")
			}
		})
	}
}

func TestGitCommentRequiresExactSnapshotGeneration(t *testing.T) {
	s := openTestStore(t)
	thread := mustCreateThread(t, s, "git-comment-generation-create").ThreadID
	for _, generation := range []uint64{1, 2} {
		binding := GitBinding{ThreadID: thread, EnvironmentID: "env", ExecutorGeneration: generation, WorktreeDir: "/w", GitDir: "/g", CommonDir: "/c", RepositoryID: "r", WorktreeID: "w", ObjectFormat: "sha1", BaseIdentity: "b"}
		if err := s.RecordGitBinding(context.Background(), binding); err != nil {
			t.Fatal(err)
		}
		input := GitSnapshotInput{ThreadID: thread, EnvironmentID: "env", ExecutorGeneration: generation, RepositoryID: "r", WorktreeID: "w", IdempotencyKey: fmt.Sprintf("capture-%d", generation), Digest: testDigest(fmt.Sprintf("snapshot-%d", generation)), HeadOID: "h", IndexDigest: testDigest("i"), Projection: json.RawMessage(`{}`)}
		if _, err := s.RecordGitSnapshot(context.Background(), input); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddGitComment(context.Background(), GitCommentInput{ThreadID: thread, CommentID: "wrong-generation", SnapshotGeneration: 3, SnapshotRevision: 1, SnapshotDigest: testDigest("snapshot-2"), FileID: "file", Actor: "alice", Body: "no"}); err == nil {
		t.Fatal("accepted comment for an unbound snapshot generation")
	}
}

func TestGitSnapshotValidationAndBindingMismatchRollBack(t *testing.T) {
	s := openTestStore(t)
	thread := mustCreateThread(t, s, "git-invalid-create").ThreadID
	b := GitBinding{ThreadID: thread, EnvironmentID: "env", ExecutorGeneration: 1, WorktreeDir: "/w", GitDir: "/g", CommonDir: "/c", RepositoryID: "r", WorktreeID: "w", ObjectFormat: "sha1", BaseIdentity: "b"}
	if err := s.RecordGitBinding(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	in := GitSnapshotInput{ThreadID: thread, EnvironmentID: "wrong", ExecutorGeneration: 1, RepositoryID: "r", WorktreeID: "w", IdempotencyKey: "x", Digest: testDigest("x"), HeadOID: "h", IndexDigest: testDigest("i"), Projection: json.RawMessage(`{}`)}
	if _, err := s.RecordGitSnapshot(context.Background(), in); err == nil {
		t.Fatal("accepted mismatch")
	}
	in.EnvironmentID = "env"
	in.Digest = "bad"
	if _, err := s.RecordGitSnapshot(context.Background(), in); err == nil {
		t.Fatal("accepted bad digest")
	}
	p, err := s.ClientProjection(context.Background(), thread, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if p.Diff.Snapshot != nil || len(p.Diff.Comments) != 0 || p.Thread.LastSequence != 1 {
		t.Fatalf("rollback failed: %#v", p)
	}
}

func TestLatestGitBindingReturnsHighestDurableGeneration(t *testing.T) {
	s := openTestStore(t)
	thread := mustCreateThread(t, s, "latest-binding-create").ThreadID
	for _, generation := range []uint64{3, 1, 2} {
		binding := GitBinding{
			ThreadID: thread, EnvironmentID: fmt.Sprintf("env-%d", generation), ExecutorGeneration: generation,
			WorktreeDir: fmt.Sprintf("/worktree-%d", generation), GitDir: "/git", CommonDir: "/common",
			RepositoryID: "repository", WorktreeID: fmt.Sprintf("worktree-%d", generation), ObjectFormat: "sha1", BaseIdentity: "base",
		}
		if err := s.RecordGitBinding(context.Background(), binding); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.LatestGitBinding(context.Background(), thread)
	if err != nil {
		t.Fatal(err)
	}
	if got.ExecutorGeneration != 3 || got.EnvironmentID != "env-3" || got.WorktreeDir != "/worktree-3" {
		t.Fatalf("LatestGitBinding() = %#v", got)
	}
	if _, err := s.LatestGitBinding(context.Background(), "T-missing"); err == nil {
		t.Fatal("LatestGitBinding() accepted a thread without a binding")
	}
}
