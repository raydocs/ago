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
	pathpkg "path"
	"strings"

	"claudexflow/internal/agoprotocol"
)

const GitWriteReceiptOwnerDomain = "ago.tool-write"

type GitReceiptFileKind string

const (
	GitReceiptFileAbsent  GitReceiptFileKind = "absent"
	GitReceiptFileRegular GitReceiptFileKind = "regular"
	GitReceiptFileSymlink GitReceiptFileKind = "symlink"
)

// GitReceiptFileIdentity is a no-follow leaf identity. For a regular file,
// Content is the file bytes; for a symlink, it is the link-target bytes.
type GitReceiptFileIdentity struct {
	Kind    GitReceiptFileKind `json:"kind"`
	Mode    uint32             `json:"mode"`
	Content []byte             `json:"content"`
}

type GitReceiptPathChange struct {
	Path   string                 `json:"path"`
	Before GitReceiptFileIdentity `json:"before"`
	After  GitReceiptFileIdentity `json:"after"`
}

type GitReceiptScope struct {
	ThreadID           string `json:"thread_id"`
	EnvironmentID      string `json:"environment_id"`
	ExecutorGeneration uint64 `json:"executor_generation"`
	RepositoryID       string `json:"repository_id"`
	WorktreeID         string `json:"worktree_id"`
	BaseIdentity       string `json:"base_identity"`
}

type GitWriteReceiptInput struct {
	GitReceiptScope
	IdempotencyKey string                 `json:"idempotency_key"`
	OperationID    string                 `json:"operation_id"`
	ToolCallID     string                 `json:"tool_call_id"`
	ToolName       string                 `json:"tool_name"`
	Changes        []GitReceiptPathChange `json:"changes"`
}

type GitWriteReceipt struct {
	ReceiptID       string `json:"receipt_id"`
	OwnerDomain     string `json:"owner_domain"`
	CreatedSequence uint64 `json:"created_sequence"`
	GitWriteReceiptInput
}

type GitWriteReceiptConflictError struct{ Reason string }

func (err GitWriteReceiptConflictError) Error() string {
	return "git write receipt conflict: " + err.Reason
}

func (store *Store) RecordGitWriteReceipt(ctx context.Context, input GitWriteReceiptInput) (GitWriteReceipt, error) {
	requestHash, err := validateGitWriteReceiptInput(input)
	if err != nil {
		return GitWriteReceipt{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return GitWriteReceipt{}, fmt.Errorf("begin git write receipt: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, found, err := queryGitWriteReceiptByRetry(ctx, tx, input.ThreadID, input.ExecutorGeneration, input.IdempotencyKey)
	if err != nil {
		return GitWriteReceipt{}, err
	}
	if found {
		if existingHash, hashErr := hashGitWriteReceiptInput(existing.GitWriteReceiptInput); hashErr != nil || existingHash != requestHash {
			return GitWriteReceipt{}, GitWriteReceiptConflictError{Reason: "idempotency key was already used for different content or ownership"}
		}
		return existing, nil
	}

	var environmentID, repositoryID, worktreeID, baseIdentity string
	err = tx.QueryRowContext(ctx, `SELECT environment_id,repository_id,worktree_id,base_identity FROM git_bindings WHERE thread_id=? AND executor_generation=?`, input.ThreadID, input.ExecutorGeneration).
		Scan(&environmentID, &repositoryID, &worktreeID, &baseIdentity)
	if errors.Is(err, sql.ErrNoRows) {
		return GitWriteReceipt{}, fmt.Errorf("git write receipt binding does not exist")
	}
	if err != nil {
		return GitWriteReceipt{}, fmt.Errorf("read git write receipt binding: %w", err)
	}
	if environmentID != input.EnvironmentID || repositoryID != input.RepositoryID || worktreeID != input.WorktreeID || baseIdentity != input.BaseIdentity {
		return GitWriteReceipt{}, GitWriteReceiptConflictError{Reason: "receipt does not match immutable git binding"}
	}

	receiptID, err := randomID("W-")
	if err != nil {
		return GitWriteReceipt{}, err
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, `SELECT last_sequence+1 FROM threads WHERE thread_id=?`, input.ThreadID).Scan(&sequence); err != nil {
		return GitWriteReceipt{}, fmt.Errorf("read git write receipt sequence: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO git_write_receipts (
 receipt_id,owner_domain,operation_id,tool_call_id,tool_name,idempotency_key,request_hash,
 thread_id,environment_id,executor_generation,repository_id,worktree_id,base_identity,created_sequence
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, receiptID, GitWriteReceiptOwnerDomain, input.OperationID, input.ToolCallID, input.ToolName,
		input.IdempotencyKey, requestHash, input.ThreadID, input.EnvironmentID, input.ExecutorGeneration, input.RepositoryID, input.WorktreeID, input.BaseIdentity, sequence)
	if err != nil {
		return GitWriteReceipt{}, fmt.Errorf("insert git write receipt: %w", err)
	}
	for ordinal, change := range input.Changes {
		beforeJSON, _ := json.Marshal(change.Before)
		afterJSON, _ := json.Marshal(change.After)
		if _, err := tx.ExecContext(ctx, `INSERT INTO git_write_receipt_paths(receipt_id,ordinal,path,before_json,after_json) VALUES(?,?,?,?,?)`, receiptID, ordinal, change.Path, beforeJSON, afterJSON); err != nil {
			return GitWriteReceipt{}, fmt.Errorf("insert git write receipt path: %w", err)
		}
	}
	receipt := GitWriteReceipt{ReceiptID: receiptID, OwnerDomain: GitWriteReceiptOwnerDomain, CreatedSequence: sequence, GitWriteReceiptInput: cloneGitWriteReceiptInput(input)}
	if err := appendGitEvent(ctx, tx, input.ThreadID, sequence, agoprotocol.EventType("git.write-receipt-recorded"), agoprotocol.VisibilityAudit, map[string]any{
		"receipt_id": receiptID, "owner_domain": GitWriteReceiptOwnerDomain, "operation_id": input.OperationID,
		"tool_call_id": input.ToolCallID, "tool_name": input.ToolName, "affected_paths": receiptPaths(input.Changes),
	}); err != nil {
		return GitWriteReceipt{}, err
	}
	if err := tx.Commit(); err != nil {
		return GitWriteReceipt{}, fmt.Errorf("commit git write receipt: %w", err)
	}
	return receipt, nil
}

func (store *Store) GitWriteReceipt(ctx context.Context, receiptID string) (GitWriteReceipt, error) {
	if !boundedRequired(receiptID) {
		return GitWriteReceipt{}, fmt.Errorf("receipt ID is required")
	}
	receipt, found, err := queryGitWriteReceipt(ctx, store.db, `r.receipt_id=?`, receiptID)
	if err != nil {
		return GitWriteReceipt{}, err
	}
	if !found {
		return GitWriteReceipt{}, fmt.Errorf("git write receipt %q does not exist", receiptID)
	}
	return receipt, nil
}

// GitWriteReceiptRetry returns the durable receipt owned by one thread,
// executor generation, and idempotency key. Callers use it before filesystem
// publication so a changed retry cannot perform side effects first.
func (store *Store) GitWriteReceiptRetry(ctx context.Context, threadID string, generation uint64, idempotencyKey string) (GitWriteReceipt, bool, error) {
	if !boundedRequired(threadID) || generation == 0 || !boundedRequired(idempotencyKey) {
		return GitWriteReceipt{}, false, fmt.Errorf("receipt retry identity is required")
	}
	return queryGitWriteReceiptByRetry(ctx, store.db, threadID, generation, idempotencyKey)
}

func (store *Store) GitWriteReceiptsOverlappingPaths(ctx context.Context, scope GitReceiptScope, paths []string) ([]GitWriteReceipt, error) {
	if err := validateGitReceiptScope(scope); err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("overlap paths are required")
	}
	for _, path := range paths {
		if err := validateReceiptPath(path); err != nil {
			return nil, err
		}
	}
	rows, err := store.db.QueryContext(ctx, `SELECT DISTINCT r.receipt_id
FROM git_write_receipts r JOIN git_write_receipt_paths p ON p.receipt_id=r.receipt_id
WHERE r.thread_id=? AND r.environment_id=? AND r.executor_generation=? AND r.repository_id=? AND r.worktree_id=? AND r.base_identity=?
ORDER BY r.created_sequence,r.receipt_id`, scope.ThreadID, scope.EnvironmentID, scope.ExecutorGeneration, scope.RepositoryID, scope.WorktreeID, scope.BaseIdentity)
	if err != nil {
		return nil, fmt.Errorf("query git write receipt scope: %w", err)
	}
	var receiptIDs []string
	for rows.Next() {
		var receiptID string
		if err := rows.Scan(&receiptID); err != nil {
			return nil, err
		}
		receiptIDs = append(receiptIDs, receiptID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	result := make([]GitWriteReceipt, 0, len(receiptIDs))
	for _, receiptID := range receiptIDs {
		receipt, err := store.GitWriteReceipt(ctx, receiptID)
		if err != nil {
			return nil, err
		}
		if receiptOverlaps(receipt.Changes, paths) {
			result = append(result, receipt)
		}
	}
	return result, nil
}

func validateGitWriteReceiptInput(input GitWriteReceiptInput) (string, error) {
	if err := validateGitReceiptScope(input.GitReceiptScope); err != nil {
		return "", err
	}
	if !boundedRequired(input.IdempotencyKey) || !boundedRequired(input.OperationID) || !boundedRequired(input.ToolCallID) || !boundedRequired(input.ToolName) {
		return "", fmt.Errorf("receipt idempotency key, operation, and tool ownership are required")
	}
	if len(input.Changes) == 0 {
		return "", fmt.Errorf("exact affected paths are required")
	}
	seen := make(map[string]struct{}, len(input.Changes))
	for _, change := range input.Changes {
		if err := validateReceiptPath(change.Path); err != nil {
			return "", err
		}
		if isProtectedReceiptPath(change.Path) {
			return "", fmt.Errorf("protected path %q cannot be receipted", change.Path)
		}
		if _, exists := seen[change.Path]; exists {
			return "", fmt.Errorf("duplicate affected path %q", change.Path)
		}
		seen[change.Path] = struct{}{}
		if err := change.Before.validate(); err != nil {
			return "", fmt.Errorf("invalid before identity for %q: %w", change.Path, err)
		}
		if err := change.After.validate(); err != nil {
			return "", fmt.Errorf("invalid after identity for %q: %w", change.Path, err)
		}
		if change.Before.equal(change.After) {
			return "", fmt.Errorf("affected path %q has no write", change.Path)
		}
	}
	return hashGitWriteReceiptInput(input)
}

func validateGitReceiptScope(scope GitReceiptScope) error {
	if !boundedRequired(scope.ThreadID) || !boundedRequired(scope.EnvironmentID) || !boundedRequired(scope.RepositoryID) || !boundedRequired(scope.WorktreeID) || !boundedRequired(scope.BaseIdentity) {
		return fmt.Errorf("exact git receipt scope is required")
	}
	return nil
}

func validateReceiptPath(value string) error {
	if value == "" || strings.IndexByte(value, 0) >= 0 || pathpkg.IsAbs(value) || pathpkg.Clean(value) != value || strings.HasPrefix(value, "../") || value == "." {
		return fmt.Errorf("receipt path %q must be a clean repository-relative path", value)
	}
	return nil
}

func isProtectedReceiptPath(value string) bool {
	return value == "thread-app/src/index.ts" || value == "thread-app/test/thread-api.test.mjs"
}

func (identity GitReceiptFileIdentity) validate() error {
	switch identity.Kind {
	case GitReceiptFileAbsent:
		if identity.Mode != 0 || identity.Content != nil {
			return fmt.Errorf("absent identity cannot have mode or content")
		}
	case GitReceiptFileRegular, GitReceiptFileSymlink:
		if identity.Mode == 0 || identity.Mode > 0o777 || identity.Content == nil {
			return fmt.Errorf("present identity requires permission mode and exact content")
		}
	default:
		return fmt.Errorf("unsupported file kind %q", identity.Kind)
	}
	return nil
}

func (identity GitReceiptFileIdentity) equal(other GitReceiptFileIdentity) bool {
	return identity.Kind == other.Kind && identity.Mode == other.Mode && bytes.Equal(identity.Content, other.Content)
}

func hashGitWriteReceiptInput(input GitWriteReceiptInput) (string, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return "", fmt.Errorf("encode git write receipt request: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

type receiptQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryGitWriteReceiptByRetry(ctx context.Context, queryer receiptQueryer, threadID string, generation uint64, key string) (GitWriteReceipt, bool, error) {
	return queryGitWriteReceipt(ctx, queryer, `r.thread_id=? AND r.executor_generation=? AND r.idempotency_key=?`, threadID, generation, key)
}

func queryGitWriteReceipt(ctx context.Context, queryer receiptQueryer, predicate string, args ...any) (GitWriteReceipt, bool, error) {
	var receipt GitWriteReceipt
	var requestHash string
	err := queryer.QueryRowContext(ctx, `SELECT r.receipt_id,r.owner_domain,r.operation_id,r.tool_call_id,r.tool_name,r.idempotency_key,r.request_hash,
r.thread_id,r.environment_id,r.executor_generation,r.repository_id,r.worktree_id,r.base_identity,r.created_sequence
FROM git_write_receipts r WHERE `+predicate, args...).Scan(
		&receipt.ReceiptID, &receipt.OwnerDomain, &receipt.OperationID, &receipt.ToolCallID, &receipt.ToolName, &receipt.IdempotencyKey, &requestHash,
		&receipt.ThreadID, &receipt.EnvironmentID, &receipt.ExecutorGeneration, &receipt.RepositoryID, &receipt.WorktreeID, &receipt.BaseIdentity, &receipt.CreatedSequence)
	if errors.Is(err, sql.ErrNoRows) {
		return GitWriteReceipt{}, false, nil
	}
	if err != nil {
		return GitWriteReceipt{}, false, fmt.Errorf("read git write receipt: %w", err)
	}
	rows, err := queryer.QueryContext(ctx, `SELECT path,before_json,after_json FROM git_write_receipt_paths WHERE receipt_id=? ORDER BY ordinal`, receipt.ReceiptID)
	if err != nil {
		return GitWriteReceipt{}, false, fmt.Errorf("read git write receipt paths: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var change GitReceiptPathChange
		var beforeJSON, afterJSON []byte
		if err := rows.Scan(&change.Path, &beforeJSON, &afterJSON); err != nil {
			return GitWriteReceipt{}, false, err
		}
		if err := json.Unmarshal(beforeJSON, &change.Before); err != nil {
			return GitWriteReceipt{}, false, fmt.Errorf("decode before identity: %w", err)
		}
		if err := json.Unmarshal(afterJSON, &change.After); err != nil {
			return GitWriteReceipt{}, false, fmt.Errorf("decode after identity: %w", err)
		}
		receipt.Changes = append(receipt.Changes, change)
	}
	if err := rows.Err(); err != nil {
		return GitWriteReceipt{}, false, err
	}
	actualHash, err := hashGitWriteReceiptInput(receipt.GitWriteReceiptInput)
	if err != nil || actualHash != requestHash {
		return GitWriteReceipt{}, false, fmt.Errorf("git write receipt integrity check failed")
	}
	return receipt, true, nil
}

func cloneGitWriteReceiptInput(input GitWriteReceiptInput) GitWriteReceiptInput {
	clone := input
	clone.Changes = make([]GitReceiptPathChange, len(input.Changes))
	for i, change := range input.Changes {
		clone.Changes[i] = change
		clone.Changes[i].Before.Content = bytes.Clone(change.Before.Content)
		clone.Changes[i].After.Content = bytes.Clone(change.After.Content)
	}
	return clone
}

func receiptPaths(changes []GitReceiptPathChange) []string {
	paths := make([]string, len(changes))
	for i, change := range changes {
		paths[i] = change.Path
	}
	return paths
}

func receiptOverlaps(changes []GitReceiptPathChange, paths []string) bool {
	for _, change := range changes {
		for _, candidate := range paths {
			if change.Path == candidate || strings.HasPrefix(change.Path, candidate+"/") || strings.HasPrefix(candidate, change.Path+"/") {
				return true
			}
		}
	}
	return false
}
