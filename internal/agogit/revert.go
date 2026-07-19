package agogit

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	pathpkg "path"
	"strconv"
	"strings"
	"sync/atomic"

	"claudexflow/internal/agothreadstore"
	"golang.org/x/sys/unix"
)

var (
	ErrInvalidReceiptRevert        = errors.New("invalid receipt revert")
	ErrReceiptRevertConflict       = errors.New("receipt revert conflicted without publication")
	ErrReceiptRevertPlanUsed       = errors.New("receipt revert plan already used")
	ErrReceiptRevertOutcomeUnknown = errors.New("receipt revert outcome is unknown")
)

// ReceiptRevertPlan is an operation-private, single-use plan derived from one
// durable write receipt. It does not expose bytes or filesystem identities.
type ReceiptRevertPlan struct {
	ReceiptID string
	Paths     []string

	binding *Binding
	entries []receiptRevertEntry
	used    *atomic.Bool
}

type receiptRevertEntry struct {
	path    string
	current receiptFileIdentity
	desired agothreadstore.GitReceiptFileIdentity
}

type receiptFileIdentity struct {
	kind    agothreadstore.GitReceiptFileKind
	mode    uint32
	content []byte
	dev     uint64
	ino     uint64
}

// PlanReceiptRevert validates the receipt's durable markers and immutable
// binding, rejects staged overlap, then computes every desired worktree image.
// It never writes a worktree path.
func (b *Binding) PlanReceiptRevert(ctx context.Context, receipt agothreadstore.GitWriteReceipt) (*ReceiptRevertPlan, error) {
	mu := worktreeLock(b.Workspace)
	mu.Lock()
	defer mu.Unlock()

	if err := b.validateReceiptForRevert(receipt); err != nil {
		return nil, err
	}
	paths := make([]string, len(receipt.Changes))
	seen := make(map[string]struct{}, len(receipt.Changes))
	for i, change := range receipt.Changes {
		if err := validateRevertPath(change.Path); err != nil {
			return nil, err
		}
		if isProtectedRevertPath(change.Path) {
			return nil, fmt.Errorf("%w: protected path %q", ErrInvalidReceiptRevert, change.Path)
		}
		if _, exists := seen[change.Path]; exists {
			return nil, fmt.Errorf("%w: duplicate path %q", ErrInvalidReceiptRevert, change.Path)
		}
		seen[change.Path] = struct{}{}
		if err := validateReceiptIdentity(change.Before); err != nil {
			return nil, fmt.Errorf("%w: invalid before image for %q: %v", ErrInvalidReceiptRevert, change.Path, err)
		}
		if err := validateReceiptIdentity(change.After); err != nil {
			return nil, fmt.Errorf("%w: invalid after image for %q: %v", ErrInvalidReceiptRevert, change.Path, err)
		}
		if receiptIdentitiesEqual(change.Before, change.After) {
			return nil, fmt.Errorf("%w: path %q records no change", ErrInvalidReceiptRevert, change.Path)
		}
		paths[i] = change.Path
	}
	if err := b.rejectStagedOverlap(ctx, paths); err != nil {
		return nil, err
	}

	entries := make([]receiptRevertEntry, 0, len(receipt.Changes))
	for _, change := range receipt.Changes {
		current, err := b.readReceiptFile(change.Path)
		if err != nil {
			return nil, fmt.Errorf("%w: inspect %q: %v", ErrReceiptRevertConflict, change.Path, err)
		}
		desired, err := planReceiptPath(change, current)
		if err != nil {
			return nil, err
		}
		entries = append(entries, receiptRevertEntry{path: change.Path, current: current, desired: desired})
	}
	return &ReceiptRevertPlan{
		ReceiptID: receipt.ReceiptID, Paths: append([]string(nil), paths...),
		binding: b, entries: entries, used: new(atomic.Bool),
	}, nil
}

func (b *Binding) validateReceiptForRevert(receipt agothreadstore.GitWriteReceipt) error {
	if b == nil || !b.validIdentity() {
		return fmt.Errorf("%w: %v", ErrInvalidReceiptRevert, ErrIdentityChanged)
	}
	if receipt.ReceiptID == "" || receipt.OwnerDomain != agothreadstore.GitWriteReceiptOwnerDomain || receipt.CreatedSequence == 0 {
		return fmt.Errorf("%w: a durable tool-write receipt is required", ErrInvalidReceiptRevert)
	}
	if receipt.ThreadID == "" || receipt.EnvironmentID == "" || receipt.ExecutorGeneration == 0 ||
		receipt.RepositoryID == "" || receipt.WorktreeID == "" || receipt.BaseIdentity == "" ||
		receipt.IdempotencyKey == "" || receipt.OperationID == "" || receipt.ToolCallID == "" || receipt.ToolName == "" || len(receipt.Changes) == 0 {
		return fmt.Errorf("%w: incomplete receipt ownership or scope", ErrInvalidReceiptRevert)
	}
	generation, err := strconv.ParseUint(b.Executor.Generation, 10, 64)
	if err != nil || receipt.EnvironmentID != b.Executor.Environment || receipt.ExecutorGeneration != generation ||
		receipt.RepositoryID != b.RepositoryID() || receipt.WorktreeID != b.WorktreeID() || receipt.BaseIdentity != b.BaseIdentity() {
		return fmt.Errorf("%w: receipt does not match the bound worktree", ErrInvalidReceiptRevert)
	}
	return nil
}

func validateRevertPath(value string) error {
	if value == "" || strings.IndexByte(value, 0) >= 0 || pathpkg.IsAbs(value) || pathpkg.Clean(value) != value || value == "." || strings.HasPrefix(value, "../") {
		return fmt.Errorf("%w: path %q must be clean and repository-relative", ErrInvalidReceiptRevert, value)
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("%w: invalid path component in %q", ErrInvalidReceiptRevert, value)
		}
	}
	return nil
}

func isProtectedRevertPath(value string) bool {
	return value == "thread-app/src/index.ts" || value == "thread-app/test/thread-api.test.mjs"
}

func validateReceiptIdentity(identity agothreadstore.GitReceiptFileIdentity) error {
	switch identity.Kind {
	case agothreadstore.GitReceiptFileAbsent:
		if identity.Mode != 0 || identity.Content != nil {
			return errors.New("absent image has mode or content")
		}
	case agothreadstore.GitReceiptFileRegular, agothreadstore.GitReceiptFileSymlink:
		if identity.Mode == 0 || identity.Mode > 0o777 || identity.Content == nil {
			return errors.New("present image lacks exact mode or content")
		}
		if identity.Kind == agothreadstore.GitReceiptFileSymlink && identity.Mode != 0o777 {
			return errors.New("symlink image has an unrestorable mode")
		}
	default:
		return fmt.Errorf("unsupported kind %q", identity.Kind)
	}
	return nil
}

func receiptIdentitiesEqual(a, b agothreadstore.GitReceiptFileIdentity) bool {
	return a.Kind == b.Kind && a.Mode == b.Mode && bytes.Equal(a.Content, b.Content)
}

func planReceiptPath(change agothreadstore.GitReceiptPathChange, current receiptFileIdentity) (agothreadstore.GitReceiptFileIdentity, error) {
	if current.matches(change.After) {
		return cloneReceiptIdentity(change.Before), nil
	}
	textOnly := change.Before.Kind == agothreadstore.GitReceiptFileRegular &&
		change.After.Kind == agothreadstore.GitReceiptFileRegular && current.kind == agothreadstore.GitReceiptFileRegular &&
		change.Before.Mode == change.After.Mode && current.mode == change.After.Mode &&
		!bytes.ContainsRune(change.Before.Content, 0) && !bytes.ContainsRune(change.After.Content, 0) && !bytes.ContainsRune(current.content, 0)
	if !textOnly {
		return agothreadstore.GitReceiptFileIdentity{}, fmt.Errorf("%w: %q no longer has its exact postimage", ErrReceiptRevertConflict, change.Path)
	}
	merged, err := mergeReceiptText(current.content, change.After.Content, change.Before.Content)
	if err != nil {
		return agothreadstore.GitReceiptFileIdentity{}, fmt.Errorf("%w: overlapping or ambiguous edits in %q", ErrReceiptRevertConflict, change.Path)
	}
	return agothreadstore.GitReceiptFileIdentity{Kind: agothreadstore.GitReceiptFileRegular, Mode: change.Before.Mode, Content: merged}, nil
}

func cloneReceiptIdentity(identity agothreadstore.GitReceiptFileIdentity) agothreadstore.GitReceiptFileIdentity {
	identity.Content = bytes.Clone(identity.Content)
	return identity
}

func mergeReceiptText(ours, base, theirs []byte) ([]byte, error) {
	dir, err := os.MkdirTemp("", "agogit-revert-merge-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	paths := []string{dir + "/ours", dir + "/base", dir + "/theirs"}
	for i, data := range [][]byte{ours, base, theirs} {
		if err := os.WriteFile(paths[i], data, 0o600); err != nil {
			return nil, err
		}
	}
	command := exec.Command("git", "merge-file", "-p", paths[0], paths[1], paths[2])
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, ErrReceiptRevertConflict
	}
	return nil, fmt.Errorf("three-way merge: %w", err)
}

func (identity receiptFileIdentity) matches(expected agothreadstore.GitReceiptFileIdentity) bool {
	return identity.kind == expected.Kind && identity.mode == expected.Mode && bytes.Equal(identity.content, expected.Content)
}

func (identity receiptFileIdentity) equal(other receiptFileIdentity) bool {
	return identity.kind == other.kind && identity.mode == other.mode && identity.dev == other.dev && identity.ino == other.ino && bytes.Equal(identity.content, other.content)
}

func (b *Binding) rejectStagedOverlap(ctx context.Context, paths []string) error {
	output, err := runGit(ctx, b.Workspace, "diff", "--cached", "--name-only", "-z", "--")
	if err != nil {
		return fmt.Errorf("%w: inspect staged paths: %v", ErrReceiptRevertConflict, err)
	}
	for _, staged := range split0(output) {
		if staged == "" {
			continue
		}
		for _, path := range paths {
			if staged == path || strings.HasPrefix(staged, path+"/") || strings.HasPrefix(path, staged+"/") {
				return fmt.Errorf("%w: staged path %q overlaps the receipt", ErrReceiptRevertConflict, staged)
			}
		}
	}
	return nil
}

func (b *Binding) readReceiptFile(relative string) (receiptFileIdentity, error) {
	dirfd, leaf, err := b.openReceiptParent(relative)
	if err != nil {
		return receiptFileIdentity{}, err
	}
	defer unix.Close(dirfd)
	return readReceiptFileAt(dirfd, leaf)
}

func (b *Binding) openReceiptParent(relative string) (int, string, error) {
	if err := validateRevertPath(relative); err != nil {
		return -1, "", err
	}
	parts := strings.Split(relative, "/")
	dirfd, err := unix.Open(b.Workspace, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(dirfd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			_ = unix.Close(dirfd)
			return -1, "", openErr
		}
		_ = unix.Close(dirfd)
		dirfd = next
	}
	return dirfd, parts[len(parts)-1], nil
}

func readReceiptFileAt(dirfd int, leaf string) (receiptFileIdentity, error) {
	var before unix.Stat_t
	if err := unix.Fstatat(dirfd, leaf, &before, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return receiptFileIdentity{kind: agothreadstore.GitReceiptFileAbsent}, nil
	} else if err != nil {
		return receiptFileIdentity{}, err
	}
	identity := receiptFileIdentity{mode: uint32(before.Mode) & 0o777, dev: uint64(before.Dev), ino: uint64(before.Ino)}
	switch before.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		identity.kind = agothreadstore.GitReceiptFileSymlink
		buffer := make([]byte, 4096)
		n, err := unix.Readlinkat(dirfd, leaf, buffer)
		if err != nil {
			return receiptFileIdentity{}, err
		}
		identity.content = append([]byte(nil), buffer[:n]...)
		return identity, nil
	case unix.S_IFREG:
		identity.kind = agothreadstore.GitReceiptFileRegular
	default:
		return receiptFileIdentity{}, fmt.Errorf("unsupported worktree entry kind")
	}
	fd, err := unix.Openat(dirfd, leaf, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return receiptFileIdentity{}, err
	}
	file := os.NewFile(uintptr(fd), leaf)
	if file == nil {
		_ = unix.Close(fd)
		return receiptFileIdentity{}, errors.New("open file descriptor")
	}
	defer file.Close()
	identity.content, err = io.ReadAll(file)
	if err != nil {
		return receiptFileIdentity{}, err
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil || before.Dev != after.Dev || before.Ino != after.Ino || before.Mode != after.Mode || before.Size != after.Size {
		return receiptFileIdentity{}, ErrUnstable
	}
	return identity, nil
}

// PublishReceiptRevert consumes a plan once, revalidates every current identity,
// prepares same-directory replacements, then publishes without following links.
func (b *Binding) PublishReceiptRevert(ctx context.Context, plan *ReceiptRevertPlan) error {
	if plan == nil || plan.used == nil || !plan.used.CompareAndSwap(false, true) {
		return ErrReceiptRevertPlanUsed
	}
	if plan.binding != b {
		return fmt.Errorf("%w: plan belongs to another binding", ErrReceiptRevertConflict)
	}
	mu := worktreeLock(b.Workspace)
	mu.Lock()
	defer mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrReceiptRevertConflict, err)
	}
	if !b.validIdentity() {
		return fmt.Errorf("%w: %v", ErrReceiptRevertConflict, ErrIdentityChanged)
	}
	if err := b.rejectStagedOverlap(ctx, plan.Paths); err != nil {
		return err
	}
	for _, entry := range plan.entries {
		current, err := b.readReceiptFile(entry.path)
		if err != nil || !current.equal(entry.current) {
			return fmt.Errorf("%w: current identity changed for %q", ErrReceiptRevertConflict, entry.path)
		}
	}

	prepared := make([]preparedReceiptRevert, 0, len(plan.entries))
	defer func() {
		for i := range prepared {
			prepared[i].closeAndRemove()
		}
	}()
	for _, entry := range plan.entries {
		item, err := b.prepareReceiptRevert(entry)
		if err != nil {
			return fmt.Errorf("%w: prepare %q: %v", ErrReceiptRevertConflict, entry.path, err)
		}
		prepared = append(prepared, item)
	}
	for i := range prepared {
		actual, err := readReceiptFileAt(prepared[i].dirfd, prepared[i].leaf)
		if err != nil || !actual.equal(plan.entries[i].current) {
			return fmt.Errorf("%w: current identity changed for %q", ErrReceiptRevertConflict, plan.entries[i].path)
		}
	}
	if err := publishPreparedReceiptReverts(prepared, plan.Paths); err != nil {
		return err
	}
	return nil
}

func publishPreparedReceiptReverts(prepared []preparedReceiptRevert, paths []string) error {
	for i := range prepared {
		if err := prepared[i].publish(); err != nil {
			rollbackErr := error(nil)
			for restored := i; restored >= 0; restored-- {
				if prepared[restored].published || prepared[restored].backupLeaf != "" {
					if err := prepared[restored].rollback(); err != nil && rollbackErr == nil {
						rollbackErr = err
					}
				}
			}
			if rollbackErr != nil {
				return fmt.Errorf("%w: publish %q: %v; rollback: %v", ErrReceiptRevertOutcomeUnknown, paths[i], err, rollbackErr)
			}
			return fmt.Errorf("%w: publish %q was rolled back: %v", ErrReceiptRevertConflict, paths[i], err)
		}
	}
	for i := range prepared {
		prepared[i].discardBackup()
	}
	return nil
}

type preparedReceiptRevert struct {
	dirfd         int
	leaf          string
	tempLeaf      string
	backupLeaf    string
	absent        bool
	currentAbsent bool
	published     bool
}

func (b *Binding) prepareReceiptRevert(entry receiptRevertEntry) (preparedReceiptRevert, error) {
	dirfd, leaf, err := b.openReceiptParent(entry.path)
	if err != nil {
		return preparedReceiptRevert{}, err
	}
	item := preparedReceiptRevert{dirfd: dirfd, leaf: leaf, absent: entry.desired.Kind == agothreadstore.GitReceiptFileAbsent, currentAbsent: entry.current.kind == agothreadstore.GitReceiptFileAbsent}
	if item.absent {
		return item, nil
	}
	tempLeaf, err := newReceiptTempName()
	if err != nil {
		_ = unix.Close(dirfd)
		return preparedReceiptRevert{}, err
	}
	item.tempLeaf = tempLeaf
	switch entry.desired.Kind {
	case agothreadstore.GitReceiptFileRegular:
		fd, err := unix.Openat(dirfd, tempLeaf, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, entry.desired.Mode)
		if err != nil {
			item.closeAndRemove()
			return preparedReceiptRevert{}, err
		}
		file := os.NewFile(uintptr(fd), tempLeaf)
		if file == nil {
			_ = unix.Close(fd)
			item.closeAndRemove()
			return preparedReceiptRevert{}, errors.New("open temporary descriptor")
		}
		if _, err = file.Write(entry.desired.Content); err == nil {
			err = file.Chmod(os.FileMode(entry.desired.Mode))
		}
		if err == nil {
			err = file.Sync()
		}
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			item.closeAndRemove()
			return preparedReceiptRevert{}, err
		}
	case agothreadstore.GitReceiptFileSymlink:
		if err := unix.Symlinkat(string(entry.desired.Content), dirfd, tempLeaf); err != nil {
			item.closeAndRemove()
			return preparedReceiptRevert{}, err
		}
	default:
		item.closeAndRemove()
		return preparedReceiptRevert{}, errors.New("unsupported desired kind")
	}
	if err := unix.Fsync(dirfd); err != nil {
		item.closeAndRemove()
		return preparedReceiptRevert{}, err
	}
	temporary, err := readReceiptFileAt(dirfd, tempLeaf)
	if err != nil || !temporary.matches(entry.desired) {
		item.closeAndRemove()
		return preparedReceiptRevert{}, errors.New("temporary image differs from desired identity")
	}
	return item, nil
}

func newReceiptTempName() (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf(".agogit-revert-%x.tmp", random[:]), nil
}

func (item *preparedReceiptRevert) publish() error {
	if !item.currentAbsent {
		backup, err := newReceiptTempName()
		if err != nil {
			return err
		}
		if err := unix.Renameat(item.dirfd, item.leaf, item.dirfd, backup); err != nil {
			return err
		}
		item.backupLeaf = backup
	}
	if !item.absent {
		if err := unix.Renameat(item.dirfd, item.tempLeaf, item.dirfd, item.leaf); err != nil {
			return err
		}
		item.tempLeaf = ""
	}
	item.published = true
	if err := unix.Fsync(item.dirfd); err != nil {
		return err
	}
	return nil
}

func (item *preparedReceiptRevert) rollback() error {
	if item.published && !item.absent {
		if err := unix.Unlinkat(item.dirfd, item.leaf, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
	}
	if item.backupLeaf != "" {
		if err := unix.Renameat(item.dirfd, item.backupLeaf, item.dirfd, item.leaf); err != nil {
			return err
		}
		item.backupLeaf = ""
	}
	item.published = false
	return unix.Fsync(item.dirfd)
}

func (item *preparedReceiptRevert) discardBackup() {
	if item.backupLeaf != "" {
		_ = unix.Unlinkat(item.dirfd, item.backupLeaf, 0)
		item.backupLeaf = ""
		_ = unix.Fsync(item.dirfd)
	}
}

func (item *preparedReceiptRevert) closeAndRemove() {
	if item.dirfd < 0 {
		return
	}
	if item.tempLeaf != "" {
		_ = unix.Unlinkat(item.dirfd, item.tempLeaf, 0)
	}
	if item.backupLeaf != "" {
		_ = unix.Unlinkat(item.dirfd, item.backupLeaf, 0)
	}
	_ = unix.Close(item.dirfd)
	item.dirfd = -1
}
