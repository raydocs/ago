package agothreadstore

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"claudexflow/internal/agoprotocol"
)

const MaxGitProjectionBytes = 2 << 20
const MaxGitMutationArtifactBytes = 64 << 20
const maxGitCommentBodyBytes = 64 << 10

// GitBinding is the durable store representation of agogit's private binding
// identity. It deliberately contains no executable Git object or provider data.
type GitBinding struct {
	ThreadID           string `json:"thread_id"`
	EnvironmentID      string `json:"environment_id"`
	ExecutorGeneration uint64 `json:"executor_generation"`
	WorktreeDir        string `json:"worktree_dir"`
	GitDir             string `json:"git_dir"`
	CommonDir          string `json:"common_dir"`
	RepositoryID       string `json:"repository_id"`
	WorktreeID         string `json:"worktree_id"`
	ObjectFormat       string `json:"object_format"`
	BaseIdentity       string `json:"base_identity"`
}

type GitSnapshotInput struct {
	ThreadID           string
	EnvironmentID      string
	ExecutorGeneration uint64
	RepositoryID       string
	WorktreeID         string
	IdempotencyKey     string
	Digest             string
	HeadOID            string
	IndexDigest        string
	Projection         json.RawMessage
	Artifact           json.RawMessage
}
type GitSnapshot struct {
	ThreadID           string          `json:"thread_id"`
	EnvironmentID      string          `json:"environment_id"`
	ExecutorGeneration uint64          `json:"executor_generation"`
	RepositoryID       string          `json:"repository_id"`
	WorktreeID         string          `json:"worktree_id"`
	Revision           uint64          `json:"revision"`
	Digest             string          `json:"digest"`
	HeadOID            string          `json:"head_oid"`
	IndexDigest        string          `json:"index_digest"`
	Projection         json.RawMessage `json:"projection"`
	CreatedSequence    uint64          `json:"created_sequence"`
	CreatedAt          string          `json:"created_at"`
}
type GitCommentInput struct {
	ThreadID, CommentID                         string
	ExpectedSequence                            *uint64
	SnapshotGeneration                          uint64
	SnapshotRevision                            uint64
	SnapshotDigest, FileID, HunkID, Actor, Body string
}
type GitCommentConflictError struct{ Reason string }

func (err GitCommentConflictError) Error() string { return "git comment conflict: " + err.Reason }

type GitComment struct {
	ThreadID           string `json:"thread_id"`
	CommentID          string `json:"comment_id"`
	SnapshotGeneration uint64 `json:"snapshot_generation"`
	SnapshotRevision   uint64 `json:"snapshot_revision"`
	SnapshotDigest     string `json:"snapshot_digest"`
	FileID             string `json:"file_id"`
	HunkID             string `json:"hunk_id,omitempty"`
	Actor              string `json:"actor"`
	Body               string `json:"body"`
	CreatedSequence    uint64 `json:"created_sequence"`
	CreatedAt          string `json:"created_at"`
}
type GitDiffProjection struct {
	Snapshot *GitSnapshot `json:"snapshot"`
	Comments []GitComment `json:"comments"`
}

func (s *Store) RecordGitBinding(ctx context.Context, b GitBinding) error {
	if err := validateGitBinding(b); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO git_bindings VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(thread_id,executor_generation) DO UPDATE SET environment_id=excluded.environment_id WHERE environment_id=excluded.environment_id AND worktree_dir=excluded.worktree_dir AND git_dir=excluded.git_dir AND common_dir=excluded.common_dir AND repository_id=excluded.repository_id AND worktree_id=excluded.worktree_id AND object_format=excluded.object_format AND base_identity=excluded.base_identity`, b.ThreadID, b.EnvironmentID, b.ExecutorGeneration, b.WorktreeDir, b.GitDir, b.CommonDir, b.RepositoryID, b.WorktreeID, b.ObjectFormat, b.BaseIdentity)
	if err != nil {
		return fmt.Errorf("record git binding: %w", err)
	}
	var count int
	_ = s.db.QueryRowContext(ctx, `SELECT count(*) FROM git_bindings WHERE thread_id=? AND executor_generation=? AND environment_id=? AND worktree_dir=? AND git_dir=? AND common_dir=? AND repository_id=? AND worktree_id=? AND object_format=? AND base_identity=?`, b.ThreadID, b.ExecutorGeneration, b.EnvironmentID, b.WorktreeDir, b.GitDir, b.CommonDir, b.RepositoryID, b.WorktreeID, b.ObjectFormat, b.BaseIdentity).Scan(&count)
	if count != 1 {
		return fmt.Errorf("git binding generation is immutable")
	}
	return nil
}

// LatestGitBinding returns the highest durable executor generation for a
// thread. Callers must still fence it against the authoritative thread record.
func (s *Store) LatestGitBinding(ctx context.Context, threadID string) (GitBinding, error) {
	if strings.TrimSpace(threadID) == "" {
		return GitBinding{}, fmt.Errorf("thread_id is required")
	}
	var binding GitBinding
	err := s.db.QueryRowContext(ctx, `SELECT thread_id,environment_id,executor_generation,worktree_dir,git_dir,common_dir,repository_id,worktree_id,object_format,base_identity FROM git_bindings WHERE thread_id=? ORDER BY executor_generation DESC LIMIT 1`, threadID).Scan(
		&binding.ThreadID, &binding.EnvironmentID, &binding.ExecutorGeneration, &binding.WorktreeDir, &binding.GitDir, &binding.CommonDir,
		&binding.RepositoryID, &binding.WorktreeID, &binding.ObjectFormat, &binding.BaseIdentity,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return GitBinding{}, fmt.Errorf("thread %q has no durable git binding", threadID)
	}
	if err != nil {
		return GitBinding{}, fmt.Errorf("read latest git binding: %w", err)
	}
	if err := validateGitBinding(binding); err != nil {
		return GitBinding{}, fmt.Errorf("stored git binding: %w", err)
	}
	return binding, nil
}

func validateGitBinding(binding GitBinding) error {
	if binding.ThreadID == "" || binding.EnvironmentID == "" || binding.ExecutorGeneration == 0 || binding.WorktreeDir == "" || binding.GitDir == "" || binding.CommonDir == "" || binding.RepositoryID == "" || binding.WorktreeID == "" || binding.ObjectFormat == "" || binding.BaseIdentity == "" {
		return fmt.Errorf("git binding fields and executor generation are required")
	}
	return nil
}

func validDigest(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}
func validateProjection(raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > MaxGitProjectionBytes || !json.Valid(raw) {
		return fmt.Errorf("projection_json must be valid JSON no larger than %d bytes", MaxGitProjectionBytes)
	}
	var x any
	if json.Unmarshal(raw, &x) != nil {
		return fmt.Errorf("malformed projection_json")
	}
	if _, ok := x.(map[string]any); !ok {
		return fmt.Errorf("projection_json must be an object")
	}
	return nil
}

func validateMutationArtifact(raw json.RawMessage) error {
	if len(raw) == 0 || len(raw) > MaxGitMutationArtifactBytes || !json.Valid(raw) {
		return fmt.Errorf("mutation artifact must be valid JSON no larger than %d bytes", MaxGitMutationArtifactBytes)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return fmt.Errorf("mutation artifact must be a JSON object")
	}
	return nil
}

func (s *Store) RecordGitSnapshot(ctx context.Context, in GitSnapshotInput) (GitSnapshot, error) {
	if in.ThreadID == "" || in.EnvironmentID == "" || in.RepositoryID == "" || in.WorktreeID == "" || in.IdempotencyKey == "" || in.HeadOID == "" || !validDigest(in.Digest) || !validDigest(in.IndexDigest) {
		return GitSnapshot{}, fmt.Errorf("invalid git snapshot identity")
	}
	if err := validateProjection(in.Projection); err != nil {
		return GitSnapshot{}, err
	}
	if len(in.Artifact) != 0 {
		if err := validateMutationArtifact(in.Artifact); err != nil {
			return GitSnapshot{}, err
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GitSnapshot{}, err
	}
	defer tx.Rollback()
	var env, repo, wt string
	err = tx.QueryRowContext(ctx, `SELECT environment_id,repository_id,worktree_id FROM git_bindings WHERE thread_id=? AND executor_generation=?`, in.ThreadID, in.ExecutorGeneration).Scan(&env, &repo, &wt)
	if err != nil {
		return GitSnapshot{}, fmt.Errorf("read git binding: %w", err)
	}
	if env != in.EnvironmentID || repo != in.RepositoryID || wt != in.WorktreeID {
		return GitSnapshot{}, fmt.Errorf("snapshot does not match git binding")
	}
	var existing GitSnapshot
	err = tx.QueryRowContext(ctx, `SELECT revision,digest,head_oid,index_digest,projection_json,created_sequence,created_at FROM git_snapshots WHERE thread_id=? AND executor_generation=? AND (digest=? OR idempotency_key=?)`, in.ThreadID, in.ExecutorGeneration, in.Digest, in.IdempotencyKey).Scan(&existing.Revision, &existing.Digest, &existing.HeadOID, &existing.IndexDigest, &existing.Projection, &existing.CreatedSequence, &existing.CreatedAt)
	if err == nil {
		// Digest is the primary capture identity, so callers may safely retry a
		// capture under a fresh request key. A reused key with changed content,
		// or inconsistent content claiming the same digest, remains an error.
		if existing.Digest != in.Digest || existing.HeadOID != in.HeadOID || existing.IndexDigest != in.IndexDigest || string(existing.Projection) != string(in.Projection) {
			return GitSnapshot{}, fmt.Errorf("changed git snapshot retry")
		}
		var existingArtifact []byte
		artifactErr := tx.QueryRowContext(ctx, `SELECT artifact_json FROM git_snapshot_artifacts WHERE thread_id=? AND executor_generation=? AND revision=?`, in.ThreadID, in.ExecutorGeneration, existing.Revision).Scan(&existingArtifact)
		if len(in.Artifact) != 0 && (artifactErr != nil || !bytes.Equal(existingArtifact, in.Artifact)) {
			return GitSnapshot{}, fmt.Errorf("changed git snapshot artifact retry")
		}
		existing.ThreadID = in.ThreadID
		existing.EnvironmentID = env
		existing.ExecutorGeneration = in.ExecutorGeneration
		existing.RepositoryID = repo
		existing.WorktreeID = wt
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return GitSnapshot{}, err
	}
	var rev, seq uint64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM git_snapshots WHERE thread_id=? AND executor_generation=?`, in.ThreadID, in.ExecutorGeneration).Scan(&rev); err != nil {
		return GitSnapshot{}, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT last_sequence+1 FROM threads WHERE thread_id=?`, in.ThreadID).Scan(&seq); err != nil {
		return GitSnapshot{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO git_snapshots VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, in.ThreadID, in.ExecutorGeneration, rev, in.EnvironmentID, in.RepositoryID, in.WorktreeID, in.IdempotencyKey, in.Digest, in.HeadOID, in.IndexDigest, []byte(in.Projection), seq, now); err != nil {
		return GitSnapshot{}, err
	}
	if len(in.Artifact) != 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO git_snapshot_artifacts VALUES(?,?,?,?,?)`, in.ThreadID, in.ExecutorGeneration, rev, in.Digest, []byte(in.Artifact)); err != nil {
			return GitSnapshot{}, fmt.Errorf("record git mutation artifact: %w", err)
		}
	}
	if err = appendGitEvent(ctx, tx, in.ThreadID, seq, agoprotocol.EventType("git.snapshot-recorded"), agoprotocol.VisibilityInternal, map[string]any{"generation": in.ExecutorGeneration, "revision": rev, "digest": in.Digest}); err != nil {
		return GitSnapshot{}, err
	}
	if err = tx.Commit(); err != nil {
		return GitSnapshot{}, err
	}
	return GitSnapshot{in.ThreadID, in.EnvironmentID, in.ExecutorGeneration, in.RepositoryID, in.WorktreeID, rev, in.Digest, in.HeadOID, in.IndexDigest, in.Projection, seq, now}, nil
}

func (s *Store) GitSnapshotArtifact(ctx context.Context, threadID string, generation, revision uint64, digest string) (json.RawMessage, error) {
	if threadID == "" || generation == 0 || revision == 0 || !validDigest(digest) {
		return nil, fmt.Errorf("exact git snapshot artifact identity is required")
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT artifact_json FROM git_snapshot_artifacts WHERE thread_id=? AND executor_generation=? AND revision=? AND snapshot_digest=?`, threadID, generation, revision, strings.ToLower(digest)).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("git snapshot mutation artifact does not exist")
	}
	if err != nil {
		return nil, fmt.Errorf("read git snapshot mutation artifact: %w", err)
	}
	if err := validateMutationArtifact(raw); err != nil {
		return nil, fmt.Errorf("stored git snapshot mutation artifact: %w", err)
	}
	return cloneRawMessage(raw), nil
}

func (s *Store) AddGitComment(ctx context.Context, in GitCommentInput) (GitComment, error) {
	if in.ThreadID == "" || in.CommentID == "" || len(in.CommentID) > 256 || in.SnapshotGeneration == 0 || in.SnapshotRevision == 0 || !validDigest(in.SnapshotDigest) || in.FileID == "" || len(in.FileID) > 1024 || len(in.HunkID) > 1024 || in.Actor == "" || len(in.Actor) > 256 || strings.TrimSpace(in.Body) == "" || len(in.Body) > maxGitCommentBodyBytes {
		return GitComment{}, fmt.Errorf("invalid git comment")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return GitComment{}, err
	}
	defer tx.Rollback()
	var existing GitComment
	existing.ThreadID = in.ThreadID
	err = tx.QueryRowContext(ctx, `SELECT snapshot_generation,snapshot_revision,snapshot_digest,file_id,hunk_id,actor,body,created_sequence,created_at FROM git_comments WHERE thread_id=? AND comment_id=?`, in.ThreadID, in.CommentID).Scan(
		&existing.SnapshotGeneration, &existing.SnapshotRevision, &existing.SnapshotDigest, &existing.FileID, &existing.HunkID, &existing.Actor, &existing.Body, &existing.CreatedSequence, &existing.CreatedAt,
	)
	if err == nil {
		existing.CommentID = in.CommentID
		if existing.SnapshotGeneration == in.SnapshotGeneration && existing.SnapshotRevision == in.SnapshotRevision && existing.SnapshotDigest == in.SnapshotDigest && existing.FileID == in.FileID && existing.HunkID == in.HunkID && existing.Actor == in.Actor && existing.Body == in.Body {
			return existing, nil
		}
		return GitComment{}, GitCommentConflictError{Reason: "comment_id already belongs to a different request"}
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return GitComment{}, fmt.Errorf("read git comment retry: %w", err)
	}
	var gen uint64
	var projection []byte
	if err = tx.QueryRowContext(ctx, `SELECT executor_generation,projection_json FROM git_snapshots WHERE thread_id=? AND executor_generation=? AND revision=? AND digest=?`, in.ThreadID, in.SnapshotGeneration, in.SnapshotRevision, in.SnapshotDigest).Scan(&gen, &projection); err != nil {
		return GitComment{}, fmt.Errorf("snapshot identity mismatch: %w", err)
	}
	if !gitCommentTargetExists(projection, in.FileID, in.HunkID) {
		return GitComment{}, fmt.Errorf("git comment target does not belong to exact snapshot")
	}
	var lastSequence uint64
	if err = tx.QueryRowContext(ctx, `SELECT last_sequence FROM threads WHERE thread_id=?`, in.ThreadID).Scan(&lastSequence); err != nil {
		return GitComment{}, err
	}
	if in.ExpectedSequence != nil && *in.ExpectedSequence != lastSequence {
		return GitComment{}, ConflictError{CurrentSequence: lastSequence, ExpectedSequence: *in.ExpectedSequence}
	}
	seq := lastSequence + 1
	now := time.Now().UTC().Format(time.RFC3339Nano)
	c := GitComment{in.ThreadID, in.CommentID, gen, in.SnapshotRevision, in.SnapshotDigest, in.FileID, in.HunkID, in.Actor, in.Body, seq, now}
	_, err = tx.ExecContext(ctx, `INSERT INTO git_comments VALUES(?,?,?,?,?,?,?,?,?,?,?)`, c.ThreadID, c.CommentID, c.SnapshotGeneration, c.SnapshotRevision, c.SnapshotDigest, c.FileID, c.HunkID, c.Actor, c.Body, c.CreatedSequence, c.CreatedAt)
	if err != nil {
		return GitComment{}, fmt.Errorf("record git comment: %w", err)
	}
	if err = appendGitEvent(ctx, tx, in.ThreadID, seq, agoprotocol.EventType("git.comment-added"), agoprotocol.VisibilityAudit, c); err != nil {
		return GitComment{}, err
	}
	if err = tx.Commit(); err != nil {
		return GitComment{}, err
	}
	return c, nil
}

func (s *Store) FindGitComment(ctx context.Context, threadID, commentID string) (GitComment, bool, error) {
	if threadID == "" || commentID == "" {
		return GitComment{}, false, fmt.Errorf("thread_id and comment_id are required")
	}
	comment := GitComment{ThreadID: threadID, CommentID: commentID}
	err := s.db.QueryRowContext(ctx, `SELECT snapshot_generation,snapshot_revision,snapshot_digest,file_id,hunk_id,actor,body,created_sequence,created_at FROM git_comments WHERE thread_id=? AND comment_id=?`, threadID, commentID).Scan(
		&comment.SnapshotGeneration, &comment.SnapshotRevision, &comment.SnapshotDigest, &comment.FileID, &comment.HunkID, &comment.Actor, &comment.Body, &comment.CreatedSequence, &comment.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return GitComment{}, false, nil
	}
	if err != nil {
		return GitComment{}, false, fmt.Errorf("find git comment: %w", err)
	}
	return comment, true, nil
}

func gitCommentTargetExists(raw []byte, fileID, hunkID string) bool {
	type hunk struct {
		ID string `json:"id"`
	}
	type file struct {
		ID    string `json:"id"`
		Hunks []hunk `json:"hunks"`
	}
	var projection struct {
		Staged   []file `json:"staged"`
		Unstaged []file `json:"unstaged"`
	}
	if json.Unmarshal(raw, &projection) != nil {
		return false
	}
	matches := 0
	for _, candidate := range append(projection.Staged, projection.Unstaged...) {
		if candidate.ID != fileID {
			continue
		}
		if hunkID == "" {
			matches++
			continue
		}
		for _, candidateHunk := range candidate.Hunks {
			if candidateHunk.ID == hunkID {
				matches++
			}
		}
	}
	return matches == 1
}

func appendGitEvent(ctx context.Context, tx *sql.Tx, thread string, seq uint64, typ agoprotocol.EventType, visibility agoprotocol.Visibility, payload any) error {
	raw, _ := json.Marshal(payload)
	id, err := randomID("E-")
	if err != nil {
		return err
	}
	e := agoprotocol.Event{SchemaVersion: agoprotocol.SchemaVersion, EventID: id, ThreadID: thread, Sequence: seq, Type: typ, Visibility: visibility, Payload: raw}
	if err = e.Validate(); err != nil {
		return err
	}
	if err = insertEvent(ctx, tx, e); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE threads SET last_sequence=? WHERE thread_id=?`, seq, thread)
	return err
}

func loadGitDiff(ctx context.Context, tx *sql.Tx, thread string) (GitDiffProjection, error) {
	d := GitDiffProjection{Comments: []GitComment{}}
	var s GitSnapshot
	err := tx.QueryRowContext(ctx, `SELECT environment_id,executor_generation,repository_id,worktree_id,revision,digest,head_oid,index_digest,projection_json,created_sequence,created_at FROM git_snapshots WHERE thread_id=? ORDER BY created_sequence DESC LIMIT 1`, thread).Scan(&s.EnvironmentID, &s.ExecutorGeneration, &s.RepositoryID, &s.WorktreeID, &s.Revision, &s.Digest, &s.HeadOID, &s.IndexDigest, &s.Projection, &s.CreatedSequence, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	s.ThreadID = thread
	d.Snapshot = &s
	rows, err := tx.QueryContext(ctx, `SELECT comment_id,snapshot_digest,file_id,hunk_id,actor,body,created_sequence,created_at FROM git_comments WHERE thread_id=? AND snapshot_generation=? AND snapshot_revision=? ORDER BY created_sequence,comment_id`, thread, s.ExecutorGeneration, s.Revision)
	if err != nil {
		return d, err
	}
	defer rows.Close()
	for rows.Next() {
		c := GitComment{ThreadID: thread, SnapshotGeneration: s.ExecutorGeneration, SnapshotRevision: s.Revision}
		if err = rows.Scan(&c.CommentID, &c.SnapshotDigest, &c.FileID, &c.HunkID, &c.Actor, &c.Body, &c.CreatedSequence, &c.CreatedAt); err != nil {
			return d, err
		}
		d.Comments = append(d.Comments, c)
	}
	return d, rows.Err()
}
