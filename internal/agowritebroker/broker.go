// Package agowritebroker implements the only receipted production-tool file
// write. It accepts declared bytes, not shell commands or opaque dirty state.
package agowritebroker

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"strconv"
	"strings"
	"sync"

	"claudexflow/internal/agogit"
	"claudexflow/internal/agothreadstore"
	"golang.org/x/sys/unix"
)

const ToolNameWriteFile = "write_file"
const maxIdentityBytes = 1024

type WriteFileRequest struct {
	ThreadID       string
	Path           string
	Content        []byte
	Mode           *uint32
	OperationID    string
	ToolCallID     string
	ToolName       string
	IdempotencyKey string
}

type WriteFileResult struct {
	ReceiptID string `json:"receipt_id"`
}

type ConflictError struct{ Reason string }

func (err ConflictError) Error() string { return "write_file conflict: " + err.Reason }

var ErrOutcomeUnknown = errors.New("write_file outcome is unknown")

type Broker struct{ store *agothreadstore.Store }

func New(store *agothreadstore.Store) *Broker { return &Broker{store: store} }

var workspaceLocks sync.Map

func workspaceLock(workspace string) *sync.Mutex {
	value, _ := workspaceLocks.LoadOrStore(workspace, new(sync.Mutex))
	return value.(*sync.Mutex)
}

func (broker *Broker) WriteFile(ctx context.Context, request WriteFileRequest) (WriteFileResult, error) {
	if broker == nil || broker.store == nil {
		return WriteFileResult{}, fmt.Errorf("write_file broker store is required")
	}
	if err := validateRequest(request); err != nil {
		return WriteFileResult{}, err
	}
	workspace, scope, err := broker.resolveScope(ctx, request.ThreadID)
	if err != nil {
		return WriteFileResult{}, err
	}
	lock := workspaceLock(workspace)
	lock.Lock()
	defer lock.Unlock()

	if prior, found, err := broker.priorRetry(ctx, scope, request); err != nil {
		return WriteFileResult{}, err
	} else if found {
		return WriteFileResult{ReceiptID: prior.ReceiptID}, nil
	}

	rootFD, err := unix.Open(workspace, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return WriteFileResult{}, fmt.Errorf("open authoritative workspace: %w", err)
	}
	defer unix.Close(rootFD)
	parentFD, leaf, err := openParentNoFollow(rootFD, request.Path)
	if err != nil {
		return WriteFileResult{}, err
	}
	defer unix.Close(parentFD)

	before, err := captureRegularOrAbsent(parentFD, leaf)
	if err != nil {
		return WriteFileResult{}, fmt.Errorf("capture before %q: %w", request.Path, err)
	}
	mode := uint32(0o644)
	if before.Kind == agothreadstore.GitReceiptFileRegular {
		mode = before.Mode
	}
	if request.Mode != nil {
		mode = *request.Mode
	}
	exactContent := make([]byte, len(request.Content))
	copy(exactContent, request.Content)
	after := agothreadstore.GitReceiptFileIdentity{Kind: agothreadstore.GitReceiptFileRegular, Mode: mode, Content: exactContent}
	if identitiesEqual(before, after) {
		return WriteFileResult{}, fmt.Errorf("write_file %q would not change exact bytes or mode", request.Path)
	}
	publication, err := atomicWrite(parentFD, leaf, request.Content, mode, before.Kind == agothreadstore.GitReceiptFileAbsent)
	if err != nil {
		return WriteFileResult{}, fmt.Errorf("write %q: %w", request.Path, err)
	}
	defer publication.close()
	observed, err := captureRegularOrAbsent(parentFD, leaf)
	if err != nil || !identitiesEqual(observed, after) {
		if rollbackErr := publication.rollback(); rollbackErr != nil {
			return WriteFileResult{}, fmt.Errorf("%w: verify %q: %v; rollback: %v", ErrOutcomeUnknown, request.Path, err, rollbackErr)
		}
		return WriteFileResult{}, fmt.Errorf("capture after %q did not match declared write: %w", request.Path, err)
	}
	receipt, err := broker.store.RecordGitWriteReceipt(context.WithoutCancel(ctx), agothreadstore.GitWriteReceiptInput{
		GitReceiptScope: scope, IdempotencyKey: request.IdempotencyKey,
		OperationID: request.OperationID, ToolCallID: request.ToolCallID, ToolName: ToolNameWriteFile,
		Changes: []agothreadstore.GitReceiptPathChange{{Path: request.Path, Before: before, After: after}},
	})
	if err != nil {
		prior, found, lookupErr := broker.priorRetry(context.WithoutCancel(ctx), scope, request)
		if lookupErr != nil {
			return WriteFileResult{}, fmt.Errorf("%w: receipt commit returned %v and durable outcome cannot be queried: %v", ErrOutcomeUnknown, err, lookupErr)
		}
		if found {
			publication.commit()
			return WriteFileResult{ReceiptID: prior.ReceiptID}, nil
		}
		if rollbackErr := publication.rollback(); rollbackErr != nil {
			return WriteFileResult{}, fmt.Errorf("%w: receipt failed: %v; rollback: %v", ErrOutcomeUnknown, err, rollbackErr)
		}
		return WriteFileResult{}, fmt.Errorf("record write_file receipt and rolled back publication: %w", err)
	}
	publication.commit()
	return WriteFileResult{ReceiptID: receipt.ReceiptID}, nil
}

func validateRequest(request WriteFileRequest) error {
	if strings.TrimSpace(request.ThreadID) == "" || strings.TrimSpace(request.OperationID) == "" ||
		strings.TrimSpace(request.ToolCallID) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return fmt.Errorf("thread, operation, tool call, and idempotency identities are required")
	}
	if len(request.ThreadID) > maxIdentityBytes || len(request.OperationID) > maxIdentityBytes ||
		len(request.ToolCallID) > maxIdentityBytes || len(request.IdempotencyKey) > maxIdentityBytes {
		return fmt.Errorf("thread, operation, tool call, and idempotency identities must not exceed %d bytes", maxIdentityBytes)
	}
	if request.ToolName != ToolNameWriteFile {
		return fmt.Errorf("only the %q production tool may use the write broker", ToolNameWriteFile)
	}
	if request.Path == "" || strings.IndexByte(request.Path, 0) >= 0 || pathpkg.IsAbs(request.Path) ||
		pathpkg.Clean(request.Path) != request.Path || request.Path == "." || strings.HasPrefix(request.Path, "../") {
		return fmt.Errorf("path %q must be a clean repository-relative path", request.Path)
	}
	for _, component := range strings.Split(request.Path, "/") {
		if component == ".git" {
			return fmt.Errorf("protected path %q cannot be written", request.Path)
		}
	}
	if request.Path == "thread-app/src/index.ts" || request.Path == "thread-app/test/thread-api.test.mjs" {
		return fmt.Errorf("protected path %q cannot be written", request.Path)
	}
	if request.Mode != nil && (*request.Mode == 0 || *request.Mode > 0o777) {
		return fmt.Errorf("mode must be an explicit nonzero permission mode")
	}
	return nil
}

func (broker *Broker) resolveScope(ctx context.Context, threadID string) (string, agothreadstore.GitReceiptScope, error) {
	projection, err := broker.store.ClientProjection(ctx, threadID, 0, 1)
	if err != nil {
		return "", agothreadstore.GitReceiptScope{}, err
	}
	if projection.Diff.Snapshot == nil {
		return "", agothreadstore.GitReceiptScope{}, fmt.Errorf("thread has no authoritative git binding snapshot")
	}
	snapshot := projection.Diff.Snapshot
	executor := agogit.ExecutorIdentity{Generation: strconv.FormatUint(snapshot.ExecutorGeneration, 10), Environment: snapshot.EnvironmentID}
	binding, err := agogit.Bind(ctx, projection.Thread.Workspace, executor)
	if err != nil {
		return "", agothreadstore.GitReceiptScope{}, fmt.Errorf("bind authoritative workspace: %w", err)
	}
	if binding.RepositoryID() != snapshot.RepositoryID || binding.WorktreeID() != snapshot.WorktreeID {
		return "", agothreadstore.GitReceiptScope{}, fmt.Errorf("authoritative repository binding changed")
	}
	return binding.Workspace, agothreadstore.GitReceiptScope{
		ThreadID: threadID, EnvironmentID: snapshot.EnvironmentID, ExecutorGeneration: snapshot.ExecutorGeneration,
		RepositoryID: snapshot.RepositoryID, WorktreeID: snapshot.WorktreeID, BaseIdentity: binding.BaseIdentity(),
	}, nil
}

func (broker *Broker) priorRetry(ctx context.Context, scope agothreadstore.GitReceiptScope, request WriteFileRequest) (agothreadstore.GitWriteReceipt, bool, error) {
	receipt, found, err := broker.store.GitWriteReceiptRetry(ctx, scope.ThreadID, scope.ExecutorGeneration, request.IdempotencyKey)
	if err != nil {
		return agothreadstore.GitWriteReceipt{}, false, err
	}
	if !found {
		return agothreadstore.GitWriteReceipt{}, false, nil
	}
	exact := receipt.EnvironmentID == scope.EnvironmentID && receipt.RepositoryID == scope.RepositoryID && receipt.WorktreeID == scope.WorktreeID && receipt.BaseIdentity == scope.BaseIdentity &&
		receipt.OperationID == request.OperationID && receipt.ToolCallID == request.ToolCallID && receipt.ToolName == ToolNameWriteFile &&
		len(receipt.Changes) == 1 && receipt.Changes[0].Path == request.Path && bytes.Equal(receipt.Changes[0].After.Content, request.Content)
	if request.Mode != nil {
		exact = exact && receipt.Changes[0].After.Mode == *request.Mode
	}
	if !exact {
		return agothreadstore.GitWriteReceipt{}, false, ConflictError{Reason: "idempotency key was already used for a different write or owner"}
	}
	return receipt, true, nil
}

func openParentNoFollow(rootFD int, path string) (int, string, error) {
	parts := strings.Split(path, "/")
	current, err := unix.Dup(rootFD)
	if err != nil {
		return -1, "", err
	}
	for _, component := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(current, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		unix.Close(current)
		if openErr != nil {
			return -1, "", fmt.Errorf("open path component %q without following links: %w", component, openErr)
		}
		current = next
	}
	return current, parts[len(parts)-1], nil
}

func captureRegularOrAbsent(parentFD int, leaf string) (agothreadstore.GitReceiptFileIdentity, error) {
	fd, err := unix.Openat(parentFD, leaf, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return agothreadstore.GitReceiptFileIdentity{Kind: agothreadstore.GitReceiptFileAbsent}, nil
	}
	if err != nil {
		return agothreadstore.GitReceiptFileIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), leaf)
	if file == nil {
		_ = unix.Close(fd)
		return agothreadstore.GitReceiptFileIdentity{}, fmt.Errorf("open leaf file")
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return agothreadstore.GitReceiptFileIdentity{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return agothreadstore.GitReceiptFileIdentity{}, fmt.Errorf("leaf is not a regular file")
	}
	content, err := io.ReadAll(file)
	if err != nil {
		return agothreadstore.GitReceiptFileIdentity{}, err
	}
	return agothreadstore.GitReceiptFileIdentity{Kind: agothreadstore.GitReceiptFileRegular, Mode: uint32(stat.Mode) & 0o777, Content: content}, nil
}

type writePublication struct {
	parentFD     int
	leaf, backup string
	committed    bool
}

func atomicWrite(parentFD int, leaf string, content []byte, mode uint32, previouslyAbsent bool) (*writePublication, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	temp := fmt.Sprintf(".ago-write-%x.tmp", random[:])
	fd, err := unix.Openat(parentFD, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), temp)
	if file == nil {
		_ = unix.Close(fd)
		_ = unix.Unlinkat(parentFD, temp, 0)
		return nil, fmt.Errorf("open private temporary file")
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = unix.Unlinkat(parentFD, temp, 0)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return nil, err
	}
	if err := unix.Fchmod(fd, mode); err != nil {
		return nil, err
	}
	if err := unix.Fsync(fd); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	publication := &writePublication{parentFD: parentFD, leaf: leaf}
	if !previouslyAbsent {
		if _, err := rand.Read(random[:]); err != nil {
			return nil, err
		}
		publication.backup = fmt.Sprintf(".ago-write-%x.backup", random[:])
		if err := unix.Renameat(parentFD, leaf, parentFD, publication.backup); err != nil {
			return nil, err
		}
	}
	if err := unix.Renameat(parentFD, temp, parentFD, leaf); err != nil {
		if publication.backup != "" {
			_ = unix.Renameat(parentFD, publication.backup, parentFD, leaf)
		}
		return nil, err
	}
	cleanup = false
	if err := unix.Fsync(parentFD); err != nil {
		if rollbackErr := publication.rollback(); rollbackErr != nil {
			return nil, fmt.Errorf("%w: fsync: %v; rollback: %v", ErrOutcomeUnknown, err, rollbackErr)
		}
		return nil, err
	}
	return publication, nil
}

func (publication *writePublication) rollback() error {
	if publication == nil || publication.committed {
		return nil
	}
	if err := unix.Unlinkat(publication.parentFD, publication.leaf, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	if publication.backup != "" {
		if err := unix.Renameat(publication.parentFD, publication.backup, publication.parentFD, publication.leaf); err != nil {
			return err
		}
		publication.backup = ""
	}
	publication.committed = true
	return unix.Fsync(publication.parentFD)
}

func (publication *writePublication) commit() {
	if publication == nil || publication.committed {
		return
	}
	if publication.backup != "" {
		_ = unix.Unlinkat(publication.parentFD, publication.backup, 0)
		publication.backup = ""
		_ = unix.Fsync(publication.parentFD)
	}
	publication.committed = true
}

func (publication *writePublication) close() {
	if publication != nil && !publication.committed {
		_ = publication.rollback()
	}
}

func identitiesEqual(left, right agothreadstore.GitReceiptFileIdentity) bool {
	return left.Kind == right.Kind && left.Mode == right.Mode && bytes.Equal(left.Content, right.Content)
}
