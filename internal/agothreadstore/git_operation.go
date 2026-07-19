package agothreadstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"claudexflow/internal/agoprotocol"
)

const (
	MaxGitOperationJSONBytes   = 64 << 10
	maxGitOperationIDs         = 4096
	maxGitOperationIDBytes     = 1024
	GitOperationIdentityV1     = 1
	GitOperationCommandDomain  = "ago.git-operation"
	GitOperationCommandVersion = 1
)

type GitHeadKind string

const (
	GitHeadCommit GitHeadKind = "commit"
	GitHeadUnborn GitHeadKind = "unborn"
)

type GitHeadIdentity struct {
	Kind        GitHeadKind `json:"kind"`
	OID         string      `json:"oid,omitempty"`
	SymbolicRef string      `json:"symbolic_ref,omitempty"`
}

type GitIndexIdentity struct {
	Exists           bool   `json:"exists"`
	SerializedDigest string `json:"serialized_digest"`
	Size             int64  `json:"size"`
	SemanticDigest   string `json:"semantic_digest"`
}

type GitWorktreeIdentity struct {
	Scope          string `json:"scope"`
	ManifestDigest string `json:"manifest_digest"`
}

type GitStateIdentity struct {
	Version  int                 `json:"version"`
	Head     GitHeadIdentity     `json:"head"`
	Index    GitIndexIdentity    `json:"index"`
	Worktree GitWorktreeIdentity `json:"worktree"`
}

type GitOperationState string

const (
	GitOperationPrepared       GitOperationState = "prepared"
	GitOperationCompleted      GitOperationState = "completed"
	GitOperationConflicted     GitOperationState = "conflicted"
	GitOperationOutcomeUnknown GitOperationState = "outcome-unknown"
)

type GitOperationKind string

const (
	GitOperationStage   GitOperationKind = "stage"
	GitOperationUnstage GitOperationKind = "unstage"
	GitOperationRevert  GitOperationKind = "revert"
)

type GitOperationSemanticRequest struct {
	CommandDomain            string           `json:"command_domain"`
	CommandVersion           uint64           `json:"command_version"`
	ThreadID                 string           `json:"thread_id"`
	EnvironmentID            string           `json:"environment_id"`
	ExecutorGeneration       uint64           `json:"executor_generation"`
	RepositoryID             string           `json:"repository_id"`
	WorktreeID               string           `json:"worktree_id"`
	BaseIdentity             string           `json:"base_identity"`
	Kind                     GitOperationKind `json:"operation_kind"`
	ExpectedSnapshotRevision uint64           `json:"expected_snapshot_revision"`
	ExpectedSnapshotDigest   string           `json:"expected_snapshot_digest"`
	SelectedUnitIDs          []string         `json:"selected_unit_ids"`
	CommandID                string           `json:"command_id"`
}

type GitOperationPrepareInput struct {
	ActorID        string
	IdempotencyKey string
	Request        GitOperationSemanticRequest
	ObjectFormat   string
	Before         GitStateIdentity
	IntendedAfter  GitStateIdentity
}

type GitOperationDisposition string

const (
	GitOperationCreated GitOperationDisposition = "created"
	GitOperationReplay  GitOperationDisposition = "replay"
)

type GitOperationPrepareResult struct {
	Operation   GitOperation            `json:"operation"`
	Disposition GitOperationDisposition `json:"disposition"`
}

type GitOperation struct {
	OperationID            string                      `json:"operation_id"`
	ActorID                string                      `json:"actor_id"`
	IdempotencyKey         string                      `json:"-"`
	Request                GitOperationSemanticRequest `json:"request"`
	RequestHash            string                      `json:"request_hash"`
	PlanHash               string                      `json:"plan_hash"`
	ObjectFormat           string                      `json:"object_format"`
	Before                 GitStateIdentity            `json:"before"`
	IntendedAfter          GitStateIdentity            `json:"intended_after"`
	LatestObserved         *GitStateIdentity           `json:"latest_observed,omitempty"`
	State                  GitOperationState           `json:"state"`
	Evidence               GitOperationEvidence        `json:"evidence,omitempty"`
	NoFutureWrite          bool                        `json:"no_future_write,omitempty"`
	Result                 json.RawMessage             `json:"result,omitempty"`
	PreparedSequence       uint64                      `json:"prepared_sequence"`
	LastTransitionSequence uint64                      `json:"last_transition_sequence"`
	ResolvedSequence       uint64                      `json:"resolved_sequence,omitempty"`
}

type GitOperationEvidence string

const (
	GitOperationEvidencePostAttempt GitOperationEvidence = "post-attempt"
	GitOperationEvidenceOwnerFenced GitOperationEvidence = "owner-fenced"
)

type GitOperationReconcileInput struct {
	OperationID   string
	Observed      GitStateIdentity
	Evidence      GitOperationEvidence
	NoFutureWrite bool
	Result        json.RawMessage
}

type GitOperationConflictError struct{ Reason string }

func (err GitOperationConflictError) Error() string { return "git operation conflict: " + err.Reason }

func (store *Store) LookupGitOperationRetry(ctx context.Context, actorID, idempotencyKey string, request GitOperationSemanticRequest) (GitOperation, bool, error) {
	canonical, requestHash, err := canonicalSemanticRequest(request)
	if err != nil || !boundedRequired(actorID) || !boundedRequired(idempotencyKey) {
		if err == nil {
			err = fmt.Errorf("valid actor and idempotency key are required")
		}
		return GitOperation{}, false, err
	}
	operation, found, err := gitOperationByRetryIdentity(ctx, store.db, actorID, idempotencyKey)
	if err != nil || !found {
		return operation, found, err
	}
	if operation.RequestHash != requestHash || !sameSemanticRequest(operation.Request, canonical) {
		return GitOperation{}, false, GitOperationConflictError{Reason: "idempotency key was already used for a different semantic request"}
	}
	return operation, true, nil
}

func (store *Store) PrepareGitOperation(ctx context.Context, input GitOperationPrepareInput) (GitOperationPrepareResult, error) {
	operation, err := canonicalGitOperation(input)
	if err != nil {
		return GitOperationPrepareResult{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return GitOperationPrepareResult{}, fmt.Errorf("begin git operation prepare: %w", err)
	}
	defer tx.Rollback()

	if existing, found, err := gitOperationByRetryIdentity(ctx, tx, operation.ActorID, operation.IdempotencyKey); err != nil {
		return GitOperationPrepareResult{}, err
	} else if found {
		if existing.RequestHash != operation.RequestHash || !sameSemanticRequest(existing.Request, operation.Request) {
			return GitOperationPrepareResult{}, GitOperationConflictError{Reason: "idempotency key was already used for a different semantic request"}
		}
		return GitOperationPrepareResult{Operation: existing, Disposition: GitOperationReplay}, nil
	}
	var conflictCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM git_operations WHERE command_domain=? AND command_version=? AND command_id=?`, operation.Request.CommandDomain, operation.Request.CommandVersion, operation.Request.CommandID).Scan(&conflictCount); err != nil {
		return GitOperationPrepareResult{}, err
	}
	if conflictCount != 0 {
		return GitOperationPrepareResult{}, GitOperationConflictError{Reason: "Git command ID was already used"}
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM git_operations WHERE repository_id=? AND worktree_id=? AND state IN ('prepared','outcome-unknown')`, operation.Request.RepositoryID, operation.Request.WorktreeID).Scan(&conflictCount); err != nil {
		return GitOperationPrepareResult{}, err
	}
	if conflictCount != 0 {
		return GitOperationPrepareResult{}, GitOperationConflictError{Reason: "worktree already has an unresolved operation"}
	}

	var env, repo, worktree, objectFormat, baseIdentity string
	var snapshotEnv, snapshotRepo, snapshotWorktree, snapshotDigest, headOID, indexDigest string
	var projection []byte
	err = tx.QueryRowContext(ctx, `
SELECT b.environment_id,b.repository_id,b.worktree_id,b.object_format,b.base_identity,
       s.environment_id,s.repository_id,s.worktree_id,s.digest,s.head_oid,s.index_digest,s.projection_json
FROM git_bindings b JOIN git_snapshots s
 ON s.thread_id=b.thread_id AND s.executor_generation=b.executor_generation
WHERE b.thread_id=? AND b.executor_generation=? AND s.revision=?`,
		operation.Request.ThreadID, operation.Request.ExecutorGeneration, operation.Request.ExpectedSnapshotRevision,
	).Scan(&env, &repo, &worktree, &objectFormat, &baseIdentity, &snapshotEnv, &snapshotRepo, &snapshotWorktree, &snapshotDigest, &headOID, &indexDigest, &projection)
	if errors.Is(err, sql.ErrNoRows) {
		return GitOperationPrepareResult{}, GitOperationConflictError{Reason: "referenced binding and snapshot do not exist"}
	}
	if err != nil {
		return GitOperationPrepareResult{}, fmt.Errorf("read git operation binding: %w", err)
	}
	r := operation.Request
	if env != r.EnvironmentID || repo != r.RepositoryID || worktree != r.WorktreeID || snapshotEnv != r.EnvironmentID || snapshotRepo != r.RepositoryID || snapshotWorktree != r.WorktreeID ||
		objectFormat != operation.ObjectFormat || baseIdentity != r.BaseIdentity || snapshotDigest != r.ExpectedSnapshotDigest {
		return GitOperationPrepareResult{}, GitOperationConflictError{Reason: "binding, object format, base identity, or snapshot identity mismatch"}
	}
	if operation.Before.Head.OID != headOID || operation.Before.Index.SemanticDigest != indexDigest {
		return GitOperationPrepareResult{}, GitOperationConflictError{Reason: "before identity is not linked to the referenced snapshot"}
	}
	if err := validateSelectedProjection(r.Kind, r.SelectedUnitIDs, projection); err != nil {
		return GitOperationPrepareResult{}, err
	}

	operationID, err := randomID("G-")
	if err != nil {
		return GitOperationPrepareResult{}, err
	}
	operation.OperationID = operationID
	operation.State = GitOperationPrepared
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence+1 FROM threads WHERE thread_id=?`, r.ThreadID).Scan(&operation.PreparedSequence); err != nil {
		return GitOperationPrepareResult{}, err
	}
	operation.LastTransitionSequence = operation.PreparedSequence
	requestJSON, _ := json.Marshal(operation.Request)
	selectedJSON, _ := json.Marshal(operation.Request.SelectedUnitIDs)
	beforeJSON, _ := json.Marshal(operation.Before)
	afterJSON, _ := json.Marshal(operation.IntendedAfter)
	_, err = tx.ExecContext(ctx, `INSERT INTO git_operations (
 operation_id,actor_id,idempotency_domain,idempotency_key,command_domain,command_version,command_id,
 thread_id,environment_id,executor_generation,repository_id,worktree_id,base_identity,object_format,operation_kind,
 request_hash,plan_hash,request_json,expected_snapshot_revision,expected_snapshot_digest,selected_unit_ids_json,
 before_json,intended_after_json,latest_observed_json,state,evidence,no_future_write,result_json,
 prepared_sequence,last_transition_sequence,resolved_sequence
) VALUES (?,?,?, ?,?,?,?, ?,?,?,?,?,?,?,?, ?,?,?,?,?,?, ?,?,NULL,?,'',0,NULL, ?,?,0)`,
		operation.OperationID, operation.ActorID, GitOperationCommandDomain, operation.IdempotencyKey,
		r.CommandDomain, r.CommandVersion, r.CommandID, r.ThreadID, r.EnvironmentID, r.ExecutorGeneration, r.RepositoryID, r.WorktreeID, r.BaseIdentity, operation.ObjectFormat, r.Kind,
		operation.RequestHash, operation.PlanHash, requestJSON, r.ExpectedSnapshotRevision, r.ExpectedSnapshotDigest, selectedJSON,
		beforeJSON, afterJSON, operation.State, operation.PreparedSequence, operation.LastTransitionSequence)
	if err != nil {
		if strings.Contains(err.Error(), "git_operations_unresolved_worktree") {
			return GitOperationPrepareResult{}, GitOperationConflictError{Reason: "worktree already has an unresolved operation"}
		}
		return GitOperationPrepareResult{}, fmt.Errorf("insert git operation: %w", err)
	}
	if err := appendGitEvent(ctx, tx, r.ThreadID, operation.PreparedSequence, agoprotocol.EventType("git.operation-prepared"), agoprotocol.VisibilityAudit, gitOperationEventPayload(operation)); err != nil {
		return GitOperationPrepareResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return GitOperationPrepareResult{}, fmt.Errorf("commit git operation prepare: %w", err)
	}
	return GitOperationPrepareResult{Operation: operation, Disposition: GitOperationCreated}, nil
}

func (store *Store) ReconcileGitOperation(ctx context.Context, input GitOperationReconcileInput) (GitOperation, error) {
	if !boundedRequired(input.OperationID) || (input.Evidence != GitOperationEvidencePostAttempt && input.Evidence != GitOperationEvidenceOwnerFenced) {
		return GitOperation{}, fmt.Errorf("valid operation ID and reconciliation evidence are required")
	}
	result, err := canonicalJSONObject("result", input.Result)
	if err != nil {
		return GitOperation{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return GitOperation{}, err
	}
	defer tx.Rollback()
	operation, found, err := gitOperationByOperationID(ctx, tx, input.OperationID)
	if err != nil {
		return GitOperation{}, err
	}
	if !found {
		return GitOperation{}, fmt.Errorf("git operation %q does not exist", input.OperationID)
	}
	if err := input.Observed.validate(operation.ObjectFormat); err != nil {
		return GitOperation{}, fmt.Errorf("invalid observed identity: %w", err)
	}

	derived := GitOperationOutcomeUnknown
	if input.Observed.equal(operation.IntendedAfter) {
		derived = GitOperationCompleted
	} else if input.Observed.equal(operation.Before) && input.NoFutureWrite {
		derived = GitOperationConflicted
	}
	if operation.State == GitOperationCompleted || operation.State == GitOperationConflicted {
		if operation.State == derived && operation.LatestObserved != nil && operation.LatestObserved.equal(input.Observed) && operation.Evidence == input.Evidence && operation.NoFutureWrite == input.NoFutureWrite && bytes.Equal(operation.Result, result) {
			return operation, nil
		}
		return GitOperation{}, GitOperationConflictError{Reason: "terminal operation is immutable"}
	}
	if operation.State == GitOperationOutcomeUnknown && derived == GitOperationOutcomeUnknown {
		if operation.LatestObserved != nil && operation.LatestObserved.equal(input.Observed) && operation.Evidence == input.Evidence && operation.NoFutureWrite == input.NoFutureWrite && bytes.Equal(operation.Result, result) {
			return operation, nil
		}
		return GitOperation{}, GitOperationConflictError{Reason: "outcome-unknown reconciliation must be an exact retry or resolve the operation"}
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence+1 FROM threads WHERE thread_id=?`, operation.Request.ThreadID).Scan(&sequence); err != nil {
		return GitOperation{}, err
	}
	observedJSON, _ := json.Marshal(input.Observed)
	resolved := uint64(0)
	if derived == GitOperationCompleted || derived == GitOperationConflicted {
		resolved = sequence
	}
	res, err := tx.ExecContext(ctx, `UPDATE git_operations SET latest_observed_json=?,state=?,evidence=?,no_future_write=?,result_json=?,last_transition_sequence=?,resolved_sequence=? WHERE operation_id=? AND state=?`,
		observedJSON, derived, input.Evidence, input.NoFutureWrite, []byte(result), sequence, resolved, input.OperationID, operation.State)
	if err != nil {
		return GitOperation{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return GitOperation{}, GitOperationConflictError{Reason: "operation changed during reconciliation"}
	}
	operation.LatestObserved = &input.Observed
	operation.State, operation.Evidence, operation.NoFutureWrite, operation.Result = derived, input.Evidence, input.NoFutureWrite, result
	operation.LastTransitionSequence, operation.ResolvedSequence = sequence, resolved
	if err := appendGitEvent(ctx, tx, operation.Request.ThreadID, sequence, agoprotocol.EventType("git.operation-"+string(derived)), agoprotocol.VisibilityAudit, gitOperationEventPayload(operation)); err != nil {
		return GitOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return GitOperation{}, err
	}
	return operation, nil
}

func (store *Store) GitOperation(ctx context.Context, operationID string) (GitOperation, error) {
	operation, found, err := gitOperationByOperationID(ctx, store.db, operationID)
	if err != nil {
		return GitOperation{}, err
	}
	if !found {
		return GitOperation{}, fmt.Errorf("git operation %q does not exist", operationID)
	}
	return operation, nil
}

func (store *Store) ListUnresolvedGitOperations(ctx context.Context, threadID, environmentID string, generation uint64) ([]GitOperation, error) {
	if !boundedRequired(threadID) || !boundedRequired(environmentID) {
		return nil, fmt.Errorf("thread and environment IDs are required")
	}
	rows, err := store.db.QueryContext(ctx, gitOperationSelect+` WHERE thread_id=? AND environment_id=? AND executor_generation=? AND state IN ('prepared','outcome-unknown') ORDER BY prepared_sequence,operation_id`, threadID, environmentID, generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var operations []GitOperation
	for rows.Next() {
		op, err := scanGitOperation(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, op)
	}
	return operations, rows.Err()
}

func canonicalGitOperation(input GitOperationPrepareInput) (GitOperation, error) {
	request, requestHash, err := canonicalSemanticRequest(input.Request)
	if err != nil {
		return GitOperation{}, err
	}
	if !boundedRequired(input.ActorID) || !boundedRequired(input.IdempotencyKey) || !boundedRequired(input.ObjectFormat) {
		return GitOperation{}, fmt.Errorf("invalid git operation authority or object format")
	}
	if err := input.Before.validate(input.ObjectFormat); err != nil {
		return GitOperation{}, fmt.Errorf("invalid before identity: %w", err)
	}
	if err := input.IntendedAfter.validate(input.ObjectFormat); err != nil {
		return GitOperation{}, fmt.Errorf("invalid intended-after identity: %w", err)
	}
	if input.Before.equal(input.IntendedAfter) {
		return GitOperation{}, fmt.Errorf("git operation plan is a no-op")
	}
	sameHead := input.Before.Head == input.IntendedAfter.Head
	sameIndex := input.Before.Index == input.IntendedAfter.Index
	sameWorktree := input.Before.Worktree == input.IntendedAfter.Worktree
	switch request.Kind {
	case GitOperationStage, GitOperationUnstage:
		if !sameHead || !sameWorktree || sameIndex {
			return GitOperation{}, fmt.Errorf("stage/unstage must change only the exact index identity")
		}
	case GitOperationRevert:
		if !sameHead || !sameIndex || sameWorktree {
			return GitOperation{}, fmt.Errorf("revert must change only the worktree identity")
		}
	}
	plan, _ := json.Marshal(struct {
		Before        GitStateIdentity `json:"before"`
		IntendedAfter GitStateIdentity `json:"intended_after"`
	}{input.Before, input.IntendedAfter})
	digest := sha256.Sum256(plan)
	return GitOperation{ActorID: input.ActorID, IdempotencyKey: input.IdempotencyKey, Request: request, RequestHash: requestHash, PlanHash: hex.EncodeToString(digest[:]), ObjectFormat: input.ObjectFormat, Before: input.Before, IntendedAfter: input.IntendedAfter}, nil
}

func canonicalSemanticRequest(request GitOperationSemanticRequest) (GitOperationSemanticRequest, string, error) {
	if request.CommandDomain != GitOperationCommandDomain || request.CommandVersion != GitOperationCommandVersion || !boundedRequired(request.CommandID) || !strings.HasPrefix(request.CommandID, "git:") ||
		!boundedRequired(request.ThreadID) || !boundedRequired(request.EnvironmentID) || !boundedRequired(request.RepositoryID) || !boundedRequired(request.WorktreeID) || !boundedRequired(request.BaseIdentity) || request.ExpectedSnapshotRevision == 0 || !validDigest(request.ExpectedSnapshotDigest) || !request.Kind.valid() {
		return request, "", fmt.Errorf("invalid or non-namespaced git operation semantic request")
	}
	selected, err := canonicalSelectedUnitIDs(request.SelectedUnitIDs)
	if err != nil {
		return request, "", err
	}
	request.SelectedUnitIDs = selected
	request.ExpectedSnapshotDigest = strings.ToLower(request.ExpectedSnapshotDigest)
	raw, _ := json.Marshal(request)
	digest := sha256.Sum256(raw)
	return request, hex.EncodeToString(digest[:]), nil
}

func (identity GitStateIdentity) validate(objectFormat string) error {
	if objectFormat != "sha1" && objectFormat != "sha256" {
		return fmt.Errorf("unsupported object format")
	}
	if identity.Version != GitOperationIdentityV1 || !validDigest(identity.Index.SemanticDigest) || !boundedRequired(identity.Worktree.Scope) || !validDigest(identity.Worktree.ManifestDigest) {
		return fmt.Errorf("identity version or digest is invalid")
	}
	if identity.Index.Exists {
		if !validDigest(identity.Index.SerializedDigest) || identity.Index.Size < 0 {
			return fmt.Errorf("existing index requires an exact serialized digest and non-negative size")
		}
	} else if identity.Index.SerializedDigest != "" || identity.Index.Size != 0 {
		return fmt.Errorf("absent index cannot have serialized bytes")
	}
	switch identity.Head.Kind {
	case GitHeadCommit:
		if identity.Head.OID == "" {
			return fmt.Errorf("commit HEAD requires an OID")
		}
		if objectFormat == "sha1" && len(identity.Head.OID) != 40 || objectFormat == "sha256" && len(identity.Head.OID) != 64 {
			return fmt.Errorf("HEAD OID does not match object format")
		}
	case GitHeadUnborn:
		if identity.Head.OID != "unborn" || identity.Head.SymbolicRef == "" {
			return fmt.Errorf("unborn HEAD requires sentinel OID and symbolic ref")
		}
	default:
		return fmt.Errorf("invalid HEAD kind")
	}
	return nil
}

func (identity GitStateIdentity) equal(other GitStateIdentity) bool { return identity == other }

type projectionUnit struct {
	ID                string `json:"id"`
	Protected         bool   `json:"protected"`
	MutationSupported bool   `json:"mutation_supported"`
	Hunks             []struct {
		ID string `json:"id"`
	} `json:"hunks"`
}

func validateSelectedProjection(kind GitOperationKind, selected []string, raw []byte) error {
	if kind == GitOperationRevert {
		return nil
	}
	var projection struct{ Staged, Unstaged []projectionUnit }
	if err := json.Unmarshal(raw, &projection); err != nil {
		return fmt.Errorf("decode snapshot projection: %w", err)
	}
	units := projection.Unstaged
	if kind == GitOperationUnstage {
		units = projection.Staged
	}
	counts := make(map[string]int)
	blocked := make(map[string]bool)
	for _, unit := range units {
		counts[unit.ID]++
		blocked[unit.ID] = unit.Protected || !unit.MutationSupported
		for _, hunk := range unit.Hunks {
			counts[hunk.ID]++
			blocked[hunk.ID] = unit.Protected || !unit.MutationSupported
		}
	}
	for _, id := range selected {
		if counts[id] != 1 {
			return GitOperationConflictError{Reason: "selected ID is absent or ambiguous on the required projection side"}
		}
		if blocked[id] {
			return GitOperationConflictError{Reason: "selected ID is protected or mutation-unsupported"}
		}
	}
	return nil
}

func canonicalJSONObject(name string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || len(raw) > MaxGitOperationJSONBytes || !json.Valid(raw) {
		return nil, fmt.Errorf("%s must be valid JSON no larger than %d bytes", name, MaxGitOperationJSONBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, fmt.Errorf("%s must be a JSON object", name)
	}
	canonical, err := json.Marshal(object)
	if err != nil || len(canonical) > MaxGitOperationJSONBytes {
		return nil, fmt.Errorf("canonical %s exceeds %d bytes", name, MaxGitOperationJSONBytes)
	}
	return canonical, nil
}

func canonicalSelectedUnitIDs(ids []string) ([]string, error) {
	if len(ids) == 0 || len(ids) > maxGitOperationIDs {
		return nil, fmt.Errorf("selected unit IDs must contain between 1 and %d entries", maxGitOperationIDs)
	}
	result := append([]string(nil), ids...)
	for _, id := range result {
		if !boundedRequired(id) {
			return nil, fmt.Errorf("invalid selected unit ID")
		}
	}
	sort.Strings(result)
	for i := 1; i < len(result); i++ {
		if result[i] == result[i-1] {
			return nil, fmt.Errorf("selected unit IDs must be unique")
		}
	}
	return result, nil
}

func boundedRequired(value string) bool { return value != "" && len(value) <= maxGitOperationIDBytes }
func (kind GitOperationKind) valid() bool {
	return kind == GitOperationStage || kind == GitOperationUnstage || kind == GitOperationRevert
}
func sameSemanticRequest(a, b GitOperationSemanticRequest) bool {
	aa, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(aa, bb)
}

type gitOperationScanner interface{ Scan(...any) error }
type gitOperationQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const gitOperationSelect = `SELECT operation_id,actor_id,idempotency_key,request_json,request_hash,plan_hash,object_format,before_json,intended_after_json,latest_observed_json,state,evidence,no_future_write,result_json,prepared_sequence,last_transition_sequence,resolved_sequence FROM git_operations`

func gitOperationByOperationID(ctx context.Context, q gitOperationQueryer, id string) (GitOperation, bool, error) {
	return queryGitOperation(q.QueryRowContext(ctx, gitOperationSelect+` WHERE operation_id=?`, id))
}
func gitOperationByRetryIdentity(ctx context.Context, q gitOperationQueryer, actor, key string) (GitOperation, bool, error) {
	return queryGitOperation(q.QueryRowContext(ctx, gitOperationSelect+` WHERE idempotency_domain=? AND actor_id=? AND idempotency_key=?`, GitOperationCommandDomain, actor, key))
}
func queryGitOperation(row *sql.Row) (GitOperation, bool, error) {
	op, err := scanGitOperation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return GitOperation{}, false, nil
	}
	return op, err == nil, err
}

func scanGitOperation(scanner gitOperationScanner) (GitOperation, error) {
	var op GitOperation
	var request, before, after, observed, result []byte
	var evidence string
	if err := scanner.Scan(&op.OperationID, &op.ActorID, &op.IdempotencyKey, &request, &op.RequestHash, &op.PlanHash, &op.ObjectFormat, &before, &after, &observed, &op.State, &evidence, &op.NoFutureWrite, &result, &op.PreparedSequence, &op.LastTransitionSequence, &op.ResolvedSequence); err != nil {
		return GitOperation{}, err
	}
	if err := json.Unmarshal(request, &op.Request); err != nil {
		return GitOperation{}, err
	}
	if err := json.Unmarshal(before, &op.Before); err != nil {
		return GitOperation{}, err
	}
	if err := json.Unmarshal(after, &op.IntendedAfter); err != nil {
		return GitOperation{}, err
	}
	if len(observed) > 0 {
		var identity GitStateIdentity
		if err := json.Unmarshal(observed, &identity); err != nil {
			return GitOperation{}, err
		}
		op.LatestObserved = &identity
	}
	op.Evidence = GitOperationEvidence(evidence)
	op.Result = cloneRawMessage(result)
	return op, nil
}

func gitOperationEventPayload(op GitOperation) map[string]any {
	return map[string]any{"operation_id": op.OperationID, "command_domain": op.Request.CommandDomain, "command_version": op.Request.CommandVersion, "command_id": op.Request.CommandID, "actor_id": op.ActorID, "operation_kind": op.Request.Kind, "request_hash": op.RequestHash, "plan_hash": op.PlanHash, "state": op.State, "prepared_sequence": op.PreparedSequence, "last_transition_sequence": op.LastTransitionSequence, "resolved_sequence": op.ResolvedSequence}
}
