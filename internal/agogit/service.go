package agogit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
)

type UnsupportedExecutorError struct {
	Target agoprotocol.ExecutorType
}

func (e *UnsupportedExecutorError) Error() string {
	return fmt.Sprintf("git refresh does not support %q executor", e.Target)
}

type RefreshInput struct {
	ThreadID           string
	Workspace          string
	Executor           agoprotocol.ExecutorTarget
	EnvironmentID      string
	ExecutorGeneration uint64
	IdempotencyKey     string
}

type Service struct{ store *agothreadstore.Store }

func NewService(store *agothreadstore.Store) *Service { return &Service{store: store} }

type MutationInput struct {
	ThreadID, Workspace, EnvironmentID, ActorID, IdempotencyKey, CommandID string
	Executor                                                               agoprotocol.ExecutorTarget
	ExecutorGeneration                                                     uint64
	ExpectedSequence                                                       uint64
	ExpectedSnapshotRevision                                               uint64
	ExpectedSnapshotDigest                                                 string
	Kind                                                                   MutationKind
	SelectedUnitIDs                                                        []string
}

type MutationResult struct {
	Operation agothreadstore.GitOperation `json:"operation"`
	Snapshot  agothreadstore.GitSnapshot  `json:"snapshot"`
}

type RevertInput struct {
	ThreadID, Workspace, EnvironmentID, ActorID, IdempotencyKey, CommandID string
	Executor                                                               agoprotocol.ExecutorTarget
	ExecutorGeneration, ExpectedSequence                                   uint64
	ExpectedSnapshotRevision                                               uint64
	ExpectedSnapshotDigest, ReceiptID                                      string
}

func (s *Service) Refresh(ctx context.Context, in RefreshInput) (agothreadstore.GitSnapshot, error) {
	if in.Executor.Type != agoprotocol.ExecutorLocal {
		return agothreadstore.GitSnapshot{}, &UnsupportedExecutorError{Target: in.Executor.Type}
	}
	if err := in.Executor.Validate(); err != nil {
		return agothreadstore.GitSnapshot{}, err
	}
	if s == nil || s.store == nil || in.ThreadID == "" || in.Workspace == "" || in.EnvironmentID == "" || in.ExecutorGeneration == 0 || in.IdempotencyKey == "" {
		return agothreadstore.GitSnapshot{}, fmt.Errorf("git refresh identity fields are required")
	}
	executor := ExecutorIdentity{Generation: strconv.FormatUint(in.ExecutorGeneration, 10), Environment: in.EnvironmentID}
	binding, err := Bind(ctx, in.Workspace, executor)
	if err != nil {
		return agothreadstore.GitSnapshot{}, err
	}
	snapshot, err := binding.Snapshot(ctx, SnapshotOptions{CurrentExecutor: &executor})
	if err != nil {
		return agothreadstore.GitSnapshot{}, err
	}
	artifact, err := marshalMutationArtifact(snapshot)
	if err != nil {
		return agothreadstore.GitSnapshot{}, err
	}
	repositoryID, worktreeID := binding.RepositoryID(), binding.WorktreeID()
	raw, err := marshalProjection(repositoryID, worktreeID, snapshot)
	if err != nil {
		return agothreadstore.GitSnapshot{}, err
	}
	if len(raw) > agothreadstore.MaxGitProjectionBytes {
		return agothreadstore.GitSnapshot{}, fmt.Errorf("git snapshot projection exceeds %d bytes", agothreadstore.MaxGitProjectionBytes)
	}
	if err := s.store.RecordGitBinding(ctx, agothreadstore.GitBinding{
		ThreadID: in.ThreadID, EnvironmentID: in.EnvironmentID, ExecutorGeneration: in.ExecutorGeneration,
		WorktreeDir: binding.Workspace, GitDir: binding.GitDir, CommonDir: binding.CommonGitDir,
		RepositoryID: repositoryID, WorktreeID: worktreeID, ObjectFormat: binding.ObjectFormat, BaseIdentity: binding.BaseIdentity(),
	}); err != nil {
		return agothreadstore.GitSnapshot{}, err
	}
	return s.store.RecordGitSnapshot(ctx, agothreadstore.GitSnapshotInput{
		ThreadID: in.ThreadID, EnvironmentID: in.EnvironmentID, ExecutorGeneration: in.ExecutorGeneration,
		RepositoryID: repositoryID, WorktreeID: worktreeID, IdempotencyKey: in.IdempotencyKey,
		Digest: snapshot.Digest, HeadOID: snapshot.HeadOID, IndexDigest: snapshot.IndexDigest, Projection: raw, Artifact: artifact,
	})
}

func (s *Service) Mutate(ctx context.Context, in MutationInput) (MutationResult, error) {
	if in.Executor.Type != agoprotocol.ExecutorLocal {
		return MutationResult{}, &UnsupportedExecutorError{Target: in.Executor.Type}
	}
	if err := in.Executor.Validate(); err != nil {
		return MutationResult{}, err
	}
	if s == nil || s.store == nil || in.ThreadID == "" || in.Workspace == "" || in.EnvironmentID == "" || in.ExecutorGeneration == 0 || in.ActorID == "" || in.IdempotencyKey == "" || in.CommandID == "" || in.ExpectedSnapshotRevision == 0 || in.ExpectedSnapshotDigest == "" || len(in.SelectedUnitIDs) == 0 {
		return MutationResult{}, fmt.Errorf("git mutation identity fields are required")
	}
	executor := ExecutorIdentity{Generation: strconv.FormatUint(in.ExecutorGeneration, 10), Environment: in.EnvironmentID}
	binding, err := Bind(ctx, in.Workspace, executor)
	if err != nil {
		return MutationResult{}, err
	}
	operationKind, err := storeMutationKind(in.Kind)
	if err != nil {
		return MutationResult{}, err
	}
	request := agothreadstore.GitOperationSemanticRequest{
		CommandDomain: agothreadstore.GitOperationCommandDomain, CommandVersion: agothreadstore.GitOperationCommandVersion,
		ThreadID: in.ThreadID, EnvironmentID: in.EnvironmentID, ExecutorGeneration: in.ExecutorGeneration,
		RepositoryID: binding.RepositoryID(), WorktreeID: binding.WorktreeID(), BaseIdentity: binding.BaseIdentity(),
		Kind: operationKind, ExpectedSnapshotRevision: in.ExpectedSnapshotRevision, ExpectedSnapshotDigest: in.ExpectedSnapshotDigest,
		SelectedUnitIDs: append([]string(nil), in.SelectedUnitIDs...), CommandID: in.CommandID,
	}
	if existing, found, lookupErr := s.store.LookupGitOperationRetry(ctx, in.ActorID, in.IdempotencyKey, request); lookupErr != nil {
		return MutationResult{}, lookupErr
	} else if found {
		if existing.State == agothreadstore.GitOperationCompleted || existing.State == agothreadstore.GitOperationConflicted {
			return s.mutationResult(ctx, existing)
		}
		return s.reconcileMutationRetry(ctx, binding, existing)
	}
	thread, err := s.store.Thread(ctx, in.ThreadID)
	if err != nil {
		return MutationResult{}, err
	}
	if in.ExpectedSequence == 0 || thread.LastSequence != in.ExpectedSequence {
		return MutationResult{}, agothreadstore.GitOperationConflictError{Reason: "thread sequence changed before Git mutation"}
	}

	artifactRaw, err := s.store.GitSnapshotArtifact(ctx, in.ThreadID, in.ExecutorGeneration, in.ExpectedSnapshotRevision, in.ExpectedSnapshotDigest)
	if err != nil {
		return MutationResult{}, err
	}
	expected, err := unmarshalMutationArtifact(artifactRaw)
	if err != nil {
		return MutationResult{}, err
	}
	if expected.Digest != strings.ToLower(in.ExpectedSnapshotDigest) {
		return MutationResult{}, fmt.Errorf("git mutation artifact digest mismatch")
	}
	plan, err := binding.PlanIndexMutation(ctx, expected, in.Kind, in.SelectedUnitIDs)
	if err != nil {
		return MutationResult{}, err
	}
	ownedPlan := true
	defer func() {
		if ownedPlan {
			DiscardIndexMutationPlan(plan)
		}
	}()
	head, err := binding.gitHeadIdentity(ctx, expected.HeadOID)
	if err != nil {
		return MutationResult{}, err
	}
	worktree := gitWorktreeIdentity(plan.AffectedWorktree)
	before := agothreadstore.GitStateIdentity{
		Version: agothreadstore.GitOperationIdentityV1, Head: head,
		Index: gitIndexIdentity(plan.Before, expected.IndexDigest), Worktree: worktree,
	}
	intended := before
	intended.Index = gitIndexIdentity(plan.Intended, plan.IntendedSemanticDigest)
	prepared, err := s.store.PrepareGitOperation(ctx, agothreadstore.GitOperationPrepareInput{
		ActorID: in.ActorID, IdempotencyKey: in.IdempotencyKey, Request: request, ObjectFormat: binding.ObjectFormat,
		Before: before, IntendedAfter: intended,
	})
	if err != nil {
		return MutationResult{}, err
	}
	if prepared.Disposition != agothreadstore.GitOperationCreated {
		return s.mutationResult(ctx, prepared.Operation)
	}
	publishErr := binding.PublishIndexMutation(ctx, plan)
	ownedPlan = false
	observed, observeErr := binding.captureGitStateIdentity(ctx, plan.AffectedWorktree)
	if observeErr != nil {
		return MutationResult{}, fmt.Errorf("capture post-mutation identity: %w", observeErr)
	}
	resultJSON, _ := json.Marshal(map[string]any{"publication_attempted": true, "publication_error": errorString(publishErr)})
	operation, reconcileErr := s.store.ReconcileGitOperation(ctx, agothreadstore.GitOperationReconcileInput{
		OperationID: prepared.Operation.OperationID,
		Observed:    observed,
		Evidence:    agothreadstore.GitOperationEvidencePostAttempt, NoFutureWrite: errors.Is(publishErr, ErrIndexMutationConflict), Result: resultJSON,
	})
	if reconcileErr != nil {
		return MutationResult{}, reconcileErr
	}
	if publishErr != nil {
		return MutationResult{Operation: operation}, publishErr
	}
	refreshed, err := s.Refresh(ctx, RefreshInput{
		ThreadID: in.ThreadID, Workspace: in.Workspace, Executor: in.Executor, EnvironmentID: in.EnvironmentID,
		ExecutorGeneration: in.ExecutorGeneration, IdempotencyKey: "mutation:" + operation.OperationID,
	})
	if err != nil {
		return MutationResult{Operation: operation}, err
	}
	return MutationResult{Operation: operation, Snapshot: refreshed}, nil
}

func (s *Service) Revert(ctx context.Context, in RevertInput) (MutationResult, error) {
	if in.Executor.Type != agoprotocol.ExecutorLocal {
		return MutationResult{}, &UnsupportedExecutorError{Target: in.Executor.Type}
	}
	if err := in.Executor.Validate(); err != nil {
		return MutationResult{}, err
	}
	if s == nil || s.store == nil || in.ThreadID == "" || in.Workspace == "" || in.EnvironmentID == "" || in.ExecutorGeneration == 0 || in.ExpectedSequence == 0 || in.ActorID == "" || in.IdempotencyKey == "" || in.CommandID == "" || in.ExpectedSnapshotRevision == 0 || in.ExpectedSnapshotDigest == "" || in.ReceiptID == "" {
		return MutationResult{}, fmt.Errorf("receipt revert identity fields are required")
	}
	executor := ExecutorIdentity{Generation: strconv.FormatUint(in.ExecutorGeneration, 10), Environment: in.EnvironmentID}
	binding, err := Bind(ctx, in.Workspace, executor)
	if err != nil {
		return MutationResult{}, err
	}
	request := agothreadstore.GitOperationSemanticRequest{
		CommandDomain: agothreadstore.GitOperationCommandDomain, CommandVersion: agothreadstore.GitOperationCommandVersion,
		ThreadID: in.ThreadID, EnvironmentID: in.EnvironmentID, ExecutorGeneration: in.ExecutorGeneration,
		RepositoryID: binding.RepositoryID(), WorktreeID: binding.WorktreeID(), BaseIdentity: binding.BaseIdentity(),
		Kind: agothreadstore.GitOperationRevert, ExpectedSnapshotRevision: in.ExpectedSnapshotRevision, ExpectedSnapshotDigest: in.ExpectedSnapshotDigest,
		SelectedUnitIDs: []string{in.ReceiptID}, CommandID: in.CommandID,
	}
	if existing, found, lookupErr := s.store.LookupGitOperationRetry(ctx, in.ActorID, in.IdempotencyKey, request); lookupErr != nil {
		return MutationResult{}, lookupErr
	} else if found {
		if existing.State == agothreadstore.GitOperationCompleted || existing.State == agothreadstore.GitOperationConflicted {
			return s.mutationResult(ctx, existing)
		}
		return s.reconcileRevertRetry(ctx, binding, existing, in.ReceiptID)
	}
	thread, err := s.store.Thread(ctx, in.ThreadID)
	if err != nil {
		return MutationResult{}, err
	}
	if thread.LastSequence != in.ExpectedSequence {
		return MutationResult{}, agothreadstore.GitOperationConflictError{Reason: "thread sequence changed before receipt revert"}
	}
	receipt, err := s.store.GitWriteReceipt(ctx, in.ReceiptID)
	if err != nil {
		return MutationResult{}, err
	}
	plan, err := binding.PlanReceiptRevert(ctx, receipt)
	if err != nil {
		return MutationResult{}, err
	}
	before, err := binding.captureReceiptStateIdentity(ctx, plan.entries, false)
	if err != nil {
		return MutationResult{}, err
	}
	intended := before
	intended.Worktree = receiptPlanWorktreeIdentity(plan.entries, true)
	prepared, err := s.store.PrepareGitOperation(ctx, agothreadstore.GitOperationPrepareInput{
		ActorID: in.ActorID, IdempotencyKey: in.IdempotencyKey, Request: request, ObjectFormat: binding.ObjectFormat,
		Before: before, IntendedAfter: intended,
	})
	if err != nil {
		return MutationResult{}, err
	}
	if prepared.Disposition != agothreadstore.GitOperationCreated {
		return s.mutationResult(ctx, prepared.Operation)
	}
	publishErr := binding.PublishReceiptRevert(ctx, plan)
	observed, observeErr := binding.captureReceiptPathsStateIdentity(ctx, receipt.Changes)
	if observeErr != nil {
		return MutationResult{}, fmt.Errorf("capture receipt revert outcome: %w", observeErr)
	}
	resultJSON, _ := json.Marshal(map[string]any{"receipt_id": receipt.ReceiptID, "publication_attempted": true, "publication_error": errorString(publishErr)})
	operation, err := s.store.ReconcileGitOperation(ctx, agothreadstore.GitOperationReconcileInput{
		OperationID: prepared.Operation.OperationID, Observed: observed, Evidence: agothreadstore.GitOperationEvidencePostAttempt,
		NoFutureWrite: errors.Is(publishErr, ErrReceiptRevertConflict), Result: resultJSON,
	})
	if err != nil {
		return MutationResult{}, err
	}
	if publishErr != nil {
		return MutationResult{Operation: operation}, publishErr
	}
	refreshed, err := s.Refresh(ctx, RefreshInput{ThreadID: in.ThreadID, Workspace: in.Workspace, Executor: in.Executor, EnvironmentID: in.EnvironmentID, ExecutorGeneration: in.ExecutorGeneration, IdempotencyKey: "revert:" + operation.OperationID})
	if err != nil {
		return MutationResult{Operation: operation}, err
	}
	return MutationResult{Operation: operation, Snapshot: refreshed}, nil
}

func (s *Service) reconcileRevertRetry(ctx context.Context, binding *Binding, operation agothreadstore.GitOperation, receiptID string) (MutationResult, error) {
	receipt, err := s.store.GitWriteReceipt(ctx, receiptID)
	if err != nil {
		return MutationResult{}, err
	}
	observed, err := binding.captureReceiptPathsStateIdentity(ctx, receipt.Changes)
	if err != nil {
		return MutationResult{}, err
	}
	result := json.RawMessage(`{"publication_attempted":false,"recovered_without_replay":true}`)
	reconciled, err := s.store.ReconcileGitOperation(ctx, agothreadstore.GitOperationReconcileInput{OperationID: operation.OperationID, Observed: observed, Evidence: agothreadstore.GitOperationEvidenceOwnerFenced, NoFutureWrite: true, Result: result})
	if err != nil {
		return MutationResult{}, err
	}
	return s.mutationResult(ctx, reconciled)
}

type receiptIdentityProjection struct {
	Path, Kind, ContentDigest string
	Mode                      uint32
}

func receiptWorktreeIdentity(paths []string, identities []receiptIdentityProjection) agothreadstore.GitWorktreeIdentity {
	sortedPaths := append([]string(nil), paths...)
	sort.Strings(sortedPaths)
	sort.Slice(identities, func(i, j int) bool { return identities[i].Path < identities[j].Path })
	scopeRaw, _ := json.Marshal(sortedPaths)
	manifestRaw, _ := json.Marshal(identities)
	scopeSum, manifestSum := sha256.Sum256(scopeRaw), sha256.Sum256(manifestRaw)
	return agothreadstore.GitWorktreeIdentity{Scope: hex.EncodeToString(scopeSum[:]), ManifestDigest: hex.EncodeToString(manifestSum[:])}
}

func receiptPlanWorktreeIdentity(entries []receiptRevertEntry, desired bool) agothreadstore.GitWorktreeIdentity {
	paths := make([]string, 0, len(entries))
	identities := make([]receiptIdentityProjection, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.path)
		if desired {
			identities = append(identities, projectReceiptIdentity(entry.path, entry.desired.Kind, entry.desired.Mode, entry.desired.Content))
		} else {
			identities = append(identities, projectReceiptIdentity(entry.path, entry.current.kind, entry.current.mode, entry.current.content))
		}
	}
	return receiptWorktreeIdentity(paths, identities)
}

func projectReceiptIdentity(path string, kind agothreadstore.GitReceiptFileKind, mode uint32, content []byte) receiptIdentityProjection {
	sum := sha256.Sum256(content)
	return receiptIdentityProjection{Path: path, Kind: string(kind), Mode: mode, ContentDigest: hex.EncodeToString(sum[:])}
}

func (b *Binding) captureReceiptStateIdentity(ctx context.Context, entries []receiptRevertEntry, desired bool) (agothreadstore.GitStateIdentity, error) {
	base, err := b.captureGitStateIdentity(ctx, nil)
	if err != nil {
		return agothreadstore.GitStateIdentity{}, err
	}
	base.Worktree = receiptPlanWorktreeIdentity(entries, desired)
	return base, nil
}

func (b *Binding) captureReceiptPathsStateIdentity(ctx context.Context, changes []agothreadstore.GitReceiptPathChange) (agothreadstore.GitStateIdentity, error) {
	paths := make([]string, 0, len(changes))
	identities := make([]receiptIdentityProjection, 0, len(changes))
	for _, change := range changes {
		current, err := b.readReceiptFile(change.Path)
		if err != nil {
			return agothreadstore.GitStateIdentity{}, err
		}
		paths = append(paths, change.Path)
		identities = append(identities, projectReceiptIdentity(change.Path, current.kind, current.mode, current.content))
	}
	base, err := b.captureGitStateIdentity(ctx, nil)
	if err != nil {
		return agothreadstore.GitStateIdentity{}, err
	}
	base.Worktree = receiptWorktreeIdentity(paths, identities)
	return base, nil
}

func (s *Service) reconcileMutationRetry(ctx context.Context, binding *Binding, operation agothreadstore.GitOperation) (MutationResult, error) {
	artifactRaw, err := s.store.GitSnapshotArtifact(ctx, operation.Request.ThreadID, operation.Request.ExecutorGeneration, operation.Request.ExpectedSnapshotRevision, operation.Request.ExpectedSnapshotDigest)
	if err != nil {
		return MutationResult{}, err
	}
	expected, err := unmarshalMutationArtifact(artifactRaw)
	if err != nil {
		return MutationResult{}, err
	}
	kind := MutationKind(operation.Request.Kind)
	selection, err := resolveMutationSelection(expected, kind, operation.Request.SelectedUnitIDs)
	if err != nil {
		return MutationResult{}, fmt.Errorf("resolve crash reconciliation scope: %w", err)
	}
	observed, err := binding.captureGitStateIdentity(ctx, selection.affected)
	if err != nil {
		return MutationResult{}, err
	}
	resultJSON := json.RawMessage(`{"publication_attempted":false,"recovered_without_replay":true}`)
	reconciled, err := s.store.ReconcileGitOperation(ctx, agothreadstore.GitOperationReconcileInput{
		OperationID: operation.OperationID, Observed: observed, Evidence: agothreadstore.GitOperationEvidenceOwnerFenced,
		NoFutureWrite: true, Result: resultJSON,
	})
	if err != nil {
		return MutationResult{}, err
	}
	if reconciled.State == agothreadstore.GitOperationCompleted {
		thread, err := s.store.Thread(ctx, operation.Request.ThreadID)
		if err != nil {
			return MutationResult{}, err
		}
		refreshed, err := s.Refresh(ctx, RefreshInput{
			ThreadID: thread.ThreadID, Workspace: thread.Workspace, Executor: thread.Executor,
			EnvironmentID: operation.Request.EnvironmentID, ExecutorGeneration: operation.Request.ExecutorGeneration,
			IdempotencyKey: "mutation-recovery:" + operation.OperationID,
		})
		if err != nil {
			return MutationResult{Operation: reconciled}, err
		}
		return MutationResult{Operation: reconciled, Snapshot: refreshed}, nil
	}
	return s.mutationResult(ctx, reconciled)
}

func (s *Service) mutationResult(ctx context.Context, operation agothreadstore.GitOperation) (MutationResult, error) {
	projection, err := s.store.ClientProjection(ctx, operation.Request.ThreadID, 0, 1)
	if err != nil {
		return MutationResult{}, err
	}
	result := MutationResult{Operation: operation}
	if projection.Diff.Snapshot != nil {
		result.Snapshot = *projection.Diff.Snapshot
	}
	return result, nil
}

func storeMutationKind(kind MutationKind) (agothreadstore.GitOperationKind, error) {
	switch kind {
	case MutationStage:
		return agothreadstore.GitOperationStage, nil
	case MutationUnstage:
		return agothreadstore.GitOperationUnstage, nil
	default:
		return "", fmt.Errorf("unsupported git mutation kind %q", kind)
	}
}

func gitIndexIdentity(identity SerializedIndexIdentity, semanticDigest string) agothreadstore.GitIndexIdentity {
	return agothreadstore.GitIndexIdentity{Exists: identity.Exists, SerializedDigest: identity.Digest, Size: identity.Size, SemanticDigest: semanticDigest}
}

func gitWorktreeIdentity(entries []WorktreeEntryIdentity) agothreadstore.GitWorktreeIdentity {
	copyEntries := append([]WorktreeEntryIdentity(nil), entries...)
	sort.Slice(copyEntries, func(i, j int) bool { return copyEntries[i].Path < copyEntries[j].Path })
	paths := make([]string, 0, len(copyEntries))
	for _, entry := range copyEntries {
		paths = append(paths, entry.Path)
	}
	scopeRaw, _ := json.Marshal(paths)
	manifestRaw, _ := json.Marshal(copyEntries)
	scopeSum, manifestSum := sha256.Sum256(scopeRaw), sha256.Sum256(manifestRaw)
	return agothreadstore.GitWorktreeIdentity{Scope: hex.EncodeToString(scopeSum[:]), ManifestDigest: hex.EncodeToString(manifestSum[:])}
}

func (b *Binding) captureWorktreeEntries(expected []WorktreeEntryIdentity) ([]WorktreeEntryIdentity, error) {
	actual := make([]WorktreeEntryIdentity, 0, len(expected))
	for _, entry := range expected {
		identity, _, err := b.readWorktreeEntry(entry.Path)
		if err != nil {
			return nil, err
		}
		actual = append(actual, identity)
	}
	return actual, nil
}

func (b *Binding) captureGitStateIdentity(ctx context.Context, affected []WorktreeEntryIdentity) (agothreadstore.GitStateIdentity, error) {
	for attempt := 0; attempt < 3; attempt++ {
		_, before, err := readExactIndex(filepath.Join(b.GitDir, "index"))
		if err != nil {
			return agothreadstore.GitStateIdentity{}, err
		}
		oid, err := b.currentHead(ctx)
		if err != nil {
			return agothreadstore.GitStateIdentity{}, err
		}
		head, err := b.gitHeadIdentity(ctx, oid)
		if err != nil {
			return agothreadstore.GitStateIdentity{}, err
		}
		semantic, err := runGit(ctx, b.Workspace, "ls-files", "--stage", "-z")
		if err != nil {
			return agothreadstore.GitStateIdentity{}, err
		}
		entries, err := b.captureWorktreeEntries(affected)
		if err != nil {
			return agothreadstore.GitStateIdentity{}, err
		}
		_, after, err := readExactIndex(filepath.Join(b.GitDir, "index"))
		if err != nil {
			return agothreadstore.GitStateIdentity{}, err
		}
		if before != after {
			continue
		}
		semanticSum := sha256.Sum256(semantic)
		return agothreadstore.GitStateIdentity{
			Version: agothreadstore.GitOperationIdentityV1, Head: head,
			Index: gitIndexIdentity(after, hex.EncodeToString(semanticSum[:])), Worktree: gitWorktreeIdentity(entries),
		}, nil
	}
	return agothreadstore.GitStateIdentity{}, ErrUnstable
}

func (b *Binding) gitHeadIdentity(ctx context.Context, oid string) (agothreadstore.GitHeadIdentity, error) {
	symbolic, err := runGit(ctx, b.Workspace, "symbolic-ref", "-q", "HEAD")
	ref := ""
	if err == nil {
		ref = strings.TrimSpace(string(symbolic))
	}
	if oid == "unborn" {
		if ref == "" {
			return agothreadstore.GitHeadIdentity{}, fmt.Errorf("unborn HEAD has no symbolic ref")
		}
		return agothreadstore.GitHeadIdentity{Kind: agothreadstore.GitHeadUnborn, OID: "unborn", SymbolicRef: ref}, nil
	}
	return agothreadstore.GitHeadIdentity{Kind: agothreadstore.GitHeadCommit, OID: oid, SymbolicRef: ref}, nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

type mutationArtifact struct {
	SchemaVersion   int                      `json:"schema_version"`
	Digest          string                   `json:"digest"`
	HeadOID         string                   `json:"head_oid"`
	IndexDigest     string                   `json:"index_digest"`
	SerializedIndex SerializedIndexIdentity  `json:"serialized_index"`
	Staged          []mutationArtifactChange `json:"staged"`
	Unstaged        []mutationArtifactChange `json:"unstaged"`
}

type mutationArtifactChange struct {
	ID, Path, OldPath, ContentDigest string
	Status                           Status
	OldMode, NewMode                 string
	Binary, Protected                bool
	MutationSupported                bool
	Patch                            []byte
	Worktree                         []WorktreeEntryIdentity
	Hunks                            []mutationArtifactHunk
}

type mutationArtifactHunk struct {
	ID, Header string
	Patch      []byte
}

func marshalMutationArtifact(snapshot *Snapshot) ([]byte, error) {
	if snapshot == nil {
		return nil, fmt.Errorf("git mutation artifact snapshot is required")
	}
	project := func(changes []FileChange) []mutationArtifactChange {
		out := make([]mutationArtifactChange, 0, len(changes))
		for _, change := range changes {
			hunks := make([]mutationArtifactHunk, 0, len(change.Hunks))
			for _, hunk := range change.Hunks {
				hunks = append(hunks, mutationArtifactHunk{ID: hunk.ID, Header: hunk.Header, Patch: append([]byte(nil), hunk.Patch...)})
			}
			out = append(out, mutationArtifactChange{
				ID: change.ID, Path: change.Path, OldPath: change.OldPath, ContentDigest: change.ContentDigest,
				Status: change.Status, OldMode: change.OldMode, NewMode: change.NewMode, Binary: change.Binary,
				Protected: change.Protected, MutationSupported: change.MutationSupported, Patch: append([]byte(nil), change.Patch...),
				Worktree: append([]WorktreeEntryIdentity(nil), change.Worktree...), Hunks: hunks,
			})
		}
		return out
	}
	return json.Marshal(mutationArtifact{1, snapshot.Digest, snapshot.HeadOID, snapshot.IndexDigest, snapshot.SerializedIndex, project(snapshot.Staged), project(snapshot.Unstaged)})
}

func unmarshalMutationArtifact(raw []byte) (*Snapshot, error) {
	var artifact mutationArtifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		return nil, fmt.Errorf("decode git mutation artifact: %w", err)
	}
	if artifact.SchemaVersion != 1 || artifact.Digest == "" || artifact.HeadOID == "" || artifact.IndexDigest == "" {
		return nil, fmt.Errorf("invalid git mutation artifact identity")
	}
	restore := func(changes []mutationArtifactChange) []FileChange {
		out := make([]FileChange, 0, len(changes))
		for _, change := range changes {
			hunks := make([]Hunk, 0, len(change.Hunks))
			for _, hunk := range change.Hunks {
				hunks = append(hunks, Hunk{ID: hunk.ID, Header: hunk.Header, Patch: append([]byte(nil), hunk.Patch...)})
			}
			out = append(out, FileChange{
				ID: change.ID, Path: change.Path, OldPath: change.OldPath, ContentDigest: change.ContentDigest,
				Status: change.Status, OldMode: change.OldMode, NewMode: change.NewMode, Binary: change.Binary,
				Protected: change.Protected, MutationSupported: change.MutationSupported, Patch: append([]byte(nil), change.Patch...),
				Worktree: append([]WorktreeEntryIdentity(nil), change.Worktree...), Hunks: hunks,
			})
		}
		return out
	}
	return &Snapshot{Digest: artifact.Digest, HeadOID: artifact.HeadOID, IndexDigest: artifact.IndexDigest, SerializedIndex: artifact.SerializedIndex, Staged: restore(artifact.Staged), Unstaged: restore(artifact.Unstaged)}, nil
}

type snapshotProjection struct {
	SchemaVersion   int                     `json:"schema_version"`
	RepositoryID    string                  `json:"repository_id"`
	WorktreeID      string                  `json:"worktree_id"`
	Digest          string                  `json:"digest"`
	HeadOID         string                  `json:"head_oid"`
	IndexDigest     string                  `json:"index_digest"`
	SerializedIndex SerializedIndexIdentity `json:"serialized_index"`
	Staged          []changeProjection      `json:"staged"`
	Unstaged        []changeProjection      `json:"unstaged"`
}

type changeProjection struct {
	ID                string           `json:"id"`
	Path              string           `json:"path"`
	OldPath           string           `json:"old_path,omitempty"`
	ContentDigest     string           `json:"content_digest"`
	Status            Status           `json:"status"`
	OldMode           string           `json:"old_mode,omitempty"`
	NewMode           string           `json:"new_mode,omitempty"`
	Binary            bool             `json:"binary"`
	Protected         bool             `json:"protected"`
	MutationSupported bool             `json:"mutation_supported"`
	Hunks             []hunkProjection `json:"hunks,omitempty"`
}

type hunkProjection struct {
	ID         string `json:"id"`
	Header     string `json:"header"`
	Patch      string `json:"patch"`
	OldStart   int    `json:"old_start"`
	OldLines   int    `json:"old_lines"`
	NewStart   int    `json:"new_start"`
	NewLines   int    `json:"new_lines"`
	Occurrence int    `json:"occurrence"`
}

func marshalProjection(repositoryID, worktreeID string, snapshot *Snapshot) ([]byte, error) {
	project := func(changes []FileChange) ([]changeProjection, error) {
		out := make([]changeProjection, 0, len(changes))
		for _, change := range changes {
			hunks := make([]hunkProjection, 0, len(change.Hunks))
			occurrences := make(map[string]int)
			if !change.Binary {
				for _, hunk := range change.Hunks {
					occurrences[hunk.Header]++
					projected, err := projectTextHunk(hunk, occurrences[hunk.Header])
					if err != nil {
						return nil, fmt.Errorf("project Git hunk %q: %w", hunk.ID, err)
					}
					hunks = append(hunks, projected)
				}
			}
			out = append(out, changeProjection{
				ID: change.ID, Path: change.Path, OldPath: change.OldPath, ContentDigest: change.ContentDigest,
				Status: change.Status, OldMode: change.OldMode, NewMode: change.NewMode, Binary: change.Binary,
				Protected: change.Protected, MutationSupported: change.MutationSupported, Hunks: hunks,
			})
		}
		return out, nil
	}
	staged, err := project(snapshot.Staged)
	if err != nil {
		return nil, err
	}
	unstaged, err := project(snapshot.Unstaged)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(snapshotProjection{
		SchemaVersion: 1, RepositoryID: repositoryID, WorktreeID: worktreeID,
		Digest: snapshot.Digest, HeadOID: snapshot.HeadOID, IndexDigest: snapshot.IndexDigest,
		SerializedIndex: snapshot.SerializedIndex, Staged: staged, Unstaged: unstaged,
	})
	if err != nil {
		return nil, err
	}
	if len(raw) > agothreadstore.MaxGitProjectionBytes {
		return nil, fmt.Errorf("git snapshot projection exceeds %d bytes", agothreadstore.MaxGitProjectionBytes)
	}
	return raw, nil
}

var unifiedHunkHeader = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@(?: .*)?$`)

func projectTextHunk(hunk Hunk, occurrence int) (hunkProjection, error) {
	if hunk.ID == "" || hunk.Header == "" || occurrence < 1 || !utf8.ValidString(hunk.Header) || strings.IndexByte(hunk.Header, 0) >= 0 {
		return hunkProjection{}, fmt.Errorf("invalid hunk identity or header")
	}
	match := unifiedHunkHeader.FindStringSubmatch(hunk.Header)
	if match == nil {
		return hunkProjection{}, fmt.Errorf("malformed unified hunk header")
	}
	values := [4]int{}
	for index, raw := range []string{match[1], match[2], match[3], match[4]} {
		if raw == "" {
			values[index] = 1
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil {
			return hunkProjection{}, fmt.Errorf("invalid unified hunk range")
		}
		values[index] = value
	}
	if !utf8.Valid(hunk.Patch) || bytes.IndexByte(hunk.Patch, 0) >= 0 {
		return hunkProjection{}, fmt.Errorf("hunk patch is not safe UTF-8 text")
	}
	headerLine := []byte(hunk.Header + "\n")
	start := -1
	for offset := 0; offset < len(hunk.Patch); {
		end := bytes.IndexByte(hunk.Patch[offset:], '\n')
		if end < 0 {
			end = len(hunk.Patch) - offset
		} else {
			end++
		}
		if bytes.HasPrefix(hunk.Patch[offset:offset+end], []byte("@@ ")) {
			start = offset
			break
		}
		offset += end
	}
	if start < 0 || !bytes.HasPrefix(hunk.Patch[start:], headerLine) {
		return hunkProjection{}, fmt.Errorf("hunk patch does not contain its exact header")
	}
	patch := hunk.Patch[start:]
	if next := bytes.Index(patch[len(headerLine):], []byte("\n@@ ")); next >= 0 {
		return hunkProjection{}, fmt.Errorf("hunk patch contains multiple hunks")
	}
	oldLines, newLines := 0, 0
	for _, line := range bytes.Split(patch[len(headerLine):], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case ' ':
			oldLines++
			newLines++
		case '-':
			oldLines++
		case '+':
			newLines++
		case '\\':
			if string(line) != `\ No newline at end of file` {
				return hunkProjection{}, fmt.Errorf("malformed no-newline marker")
			}
		default:
			return hunkProjection{}, fmt.Errorf("malformed unified hunk line")
		}
	}
	if oldLines != values[1] || newLines != values[3] {
		return hunkProjection{}, fmt.Errorf("hunk body does not match header ranges")
	}
	return hunkProjection{
		ID: hunk.ID, Header: hunk.Header, Patch: string(patch),
		OldStart: values[0], OldLines: values[1], NewStart: values[2], NewLines: values[3], Occurrence: occurrence,
	}, nil
}
