package agogit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	ErrInvalidMutationSelection    = errors.New("invalid index mutation selection")
	ErrIndexMutationConflict       = errors.New("index mutation conflicted without publication")
	ErrIndexMutationOutcomeUnknown = errors.New("index mutation publication outcome unknown")
	ErrIndexMutationPlanUsed       = errors.New("index mutation plan already used")
)

type MutationKind string

const (
	MutationStage   MutationKind = "stage"
	MutationUnstage MutationKind = "unstage"
)

// IndexMutationPlan contains only journal-safe identities. The alternate index,
// intended bytes, and publication checks remain operation-private.
type IndexMutationPlan struct {
	Kind                   MutationKind
	Before                 SerializedIndexIdentity
	Intended               SerializedIndexIdentity
	IntendedSemanticDigest string
	AffectedWorktree       []WorktreeEntryIdentity

	binding       *Binding
	headOID       string
	before        SerializedIndexIdentity
	intended      SerializedIndexIdentity
	affected      []WorktreeEntryIdentity
	alternateDir  string
	alternatePath string
	alternateID   fsIdentity
	auxiliary     map[string]fsIdentity
	intendedBytes []byte
	used          *atomic.Bool
}

type mutationSelection struct {
	patches  [][]byte
	affected []WorktreeEntryIdentity
}

// PlanIndexMutation creates a standalone alternate index but does not modify the
// real index or any worktree path. Callers may durably record the returned plan
// before attempting its single-use publication.
func (b *Binding) PlanIndexMutation(ctx context.Context, expected *Snapshot, kind MutationKind, selectedUnitIDs []string) (*IndexMutationPlan, error) {
	mu := worktreeLock(b.Workspace)
	mu.Lock()
	defer mu.Unlock()

	if expected == nil || expected.Digest == "" || len(selectedUnitIDs) == 0 || (kind != MutationStage && kind != MutationUnstage) {
		return nil, fmt.Errorf("%w: missing or invalid request", ErrInvalidMutationSelection)
	}
	if !b.validIdentity() {
		return nil, ErrIdentityChanged
	}
	current, err := b.capture(ctx)
	if err != nil {
		return nil, err
	}
	if current.Digest != expected.Digest {
		return nil, fmt.Errorf("%w: snapshot changed", ErrIndexMutationConflict)
	}
	selection, err := resolveMutationSelection(current, kind, selectedUnitIDs)
	if err != nil {
		return nil, err
	}

	indexPath := filepath.Join(b.GitDir, "index")
	beforeBytes, before, err := readExactIndex(indexPath)
	if err != nil {
		return nil, err
	}
	if before != current.SerializedIndex {
		return nil, fmt.Errorf("%w: serialized index changed", ErrIndexMutationConflict)
	}

	privateDir, err := os.MkdirTemp(b.GitDir, ".agogit-mutation-")
	if err != nil {
		return nil, fmt.Errorf("create private mutation directory: %w", err)
	}
	alternatePath := filepath.Join(privateDir, "index")
	cleanup := true
	defer func() {
		if cleanup {
			removePrivateAlternate(privateDir, alternatePath)
		}
	}()
	if err := setupMutationEnvironment(privateDir, b.ObjectFormat); err != nil {
		return nil, err
	}
	auxiliary, err := copySharedIndexes(b.GitDir, privateDir)
	if err != nil {
		return nil, err
	}
	if before.Exists {
		if err := writeNewFile(alternatePath, beforeBytes); err != nil {
			return nil, fmt.Errorf("seed alternate index: %w", err)
		}
	} else if err := b.runIndexGit(ctx, alternatePath, nil, "read-tree", "--empty"); err != nil {
		return nil, err
	}
	if err := b.runIndexGit(ctx, alternatePath, nil, "update-index", "--no-split-index"); err != nil {
		return nil, err
	}
	for _, patch := range selection.patches {
		args := []string{"apply", "--cached"}
		if kind == MutationUnstage {
			args = append(args, "-R")
		}
		if err := b.runIndexGit(ctx, alternatePath, patch, args...); err != nil {
			return nil, err
		}
	}
	intendedBytes, intended, alternateID, err := syncAndReadAlternate(alternatePath)
	if err != nil {
		return nil, err
	}
	semanticIndex, err := b.runIndexGitOutput(ctx, alternatePath, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	semanticSum := sha256.Sum256(semanticIndex)
	intendedSemanticDigest := hex.EncodeToString(semanticSum[:])

	affected := append([]WorktreeEntryIdentity(nil), selection.affected...)
	plan := &IndexMutationPlan{
		Kind: kind, Before: before, Intended: intended, IntendedSemanticDigest: intendedSemanticDigest, AffectedWorktree: append([]WorktreeEntryIdentity(nil), affected...),
		binding: b, headOID: current.HeadOID, before: before, intended: intended, affected: affected,
		alternateDir: privateDir, alternatePath: alternatePath, alternateID: alternateID, auxiliary: auxiliary, intendedBytes: intendedBytes,
		used: new(atomic.Bool),
	}
	cleanup = false
	return plan, nil
}

// DiscardIndexMutationPlan releases a plan that was not published, for example
// when durable journal preparation loses a concurrent ownership race.
func DiscardIndexMutationPlan(plan *IndexMutationPlan) {
	if plan == nil || plan.used == nil || !plan.used.CompareAndSwap(false, true) {
		return
	}
	removeOwnedAlternate(plan.alternateDir, plan.alternatePath, plan.alternateID, plan.auxiliary)
}

// PublishIndexMutation consumes plan exactly once. An error wrapping
// ErrIndexMutationConflict guarantees no publication. An error wrapping
// ErrIndexMutationOutcomeUnknown means the rename occurred and must not be replayed.
func (b *Binding) PublishIndexMutation(ctx context.Context, plan *IndexMutationPlan) error {
	if plan == nil || plan.used == nil || !plan.used.CompareAndSwap(false, true) {
		return ErrIndexMutationPlanUsed
	}
	defer removeOwnedAlternate(plan.alternateDir, plan.alternatePath, plan.alternateID, plan.auxiliary)
	if plan.binding != b {
		return fmt.Errorf("%w: plan belongs to another binding", ErrIndexMutationConflict)
	}

	mu := worktreeLock(b.Workspace)
	mu.Lock()
	defer mu.Unlock()

	lockPath := filepath.Join(b.GitDir, "index.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("%w: acquire real index lock: %v", ErrIndexMutationConflict, err)
	}
	published := false
	defer func() {
		if !published {
			removeOwnedLock(lockPath, lockFile)
		}
		_ = lockFile.Close()
	}()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %v", ErrIndexMutationConflict, err)
	}
	if !b.validIdentity() {
		return fmt.Errorf("%w: %v", ErrIndexMutationConflict, ErrIdentityChanged)
	}
	head, err := b.currentHead(ctx)
	if err != nil {
		return fmt.Errorf("%w: read HEAD: %v", ErrIndexMutationConflict, err)
	}
	if head != plan.headOID {
		return fmt.Errorf("%w: HEAD changed", ErrIndexMutationConflict)
	}
	_, realIndex, err := readExactIndex(filepath.Join(b.GitDir, "index"))
	if err != nil {
		return fmt.Errorf("%w: read real index: %v", ErrIndexMutationConflict, err)
	}
	if realIndex != plan.before {
		return fmt.Errorf("%w: real index changed", ErrIndexMutationConflict)
	}
	if err := b.verifyAffectedWorktree(plan.affected); err != nil {
		return fmt.Errorf("%w: worktree changed: %v", ErrIndexMutationConflict, err)
	}
	alternateBytes, alternateIdentity, alternateFSID, err := readRegularFile(plan.alternatePath)
	if err != nil || alternateIdentity != plan.intended || alternateFSID != plan.alternateID || !bytes.Equal(alternateBytes, plan.intendedBytes) {
		return fmt.Errorf("%w: alternate index changed", ErrIndexMutationConflict)
	}

	if err := writeAndVerifyDescriptor(lockFile, plan.intendedBytes); err != nil {
		return fmt.Errorf("%w: write owned index lock: %v", ErrIndexMutationConflict, err)
	}
	if !pathMatchesDescriptor(lockPath, lockFile) {
		return fmt.Errorf("%w: owned index lock path changed", ErrIndexMutationConflict)
	}
	// On ordinary macOS filesystems there is no universal pathname compare-and-swap
	// syscall. The conventional index.lock protocol protects cooperating Git writers;
	// the post-rename checks below turn detected final-syscall races into unknown outcome.
	if err := os.Rename(lockPath, filepath.Join(b.GitDir, "index")); err != nil {
		return fmt.Errorf("%w: publish index: %v", ErrIndexMutationConflict, err)
	}
	published = true
	if err := syncDirectory(b.GitDir); err != nil {
		return fmt.Errorf("%w: sync git directory: %v", ErrIndexMutationOutcomeUnknown, err)
	}
	publishedBytes, publishedIdentity, _, err := readRegularFile(filepath.Join(b.GitDir, "index"))
	if err != nil || publishedIdentity != plan.intended || !bytes.Equal(publishedBytes, plan.intendedBytes) {
		return fmt.Errorf("%w: published index verification failed", ErrIndexMutationOutcomeUnknown)
	}
	afterHead, err := b.currentHead(ctx)
	if err != nil || afterHead != plan.headOID {
		return fmt.Errorf("%w: HEAD changed after publication", ErrIndexMutationOutcomeUnknown)
	}
	if err := b.verifyAffectedWorktree(plan.affected); err != nil {
		return fmt.Errorf("%w: worktree changed after publication: %v", ErrIndexMutationOutcomeUnknown, err)
	}
	return nil
}

func resolveMutationSelection(snapshot *Snapshot, kind MutationKind, ids []string) (mutationSelection, error) {
	changes := snapshot.Unstaged
	if kind == MutationUnstage {
		changes = snapshot.Staged
	}
	type unit struct {
		file  *FileChange
		hunk  *Hunk
		patch []byte
	}
	units := make(map[string]unit)
	for fileIndex := range changes {
		file := &changes[fileIndex]
		units[file.ID] = unit{file: file, patch: file.Patch}
		for hunkIndex := range file.Hunks {
			hunk := &file.Hunks[hunkIndex]
			units[hunk.ID] = unit{file: file, hunk: hunk, patch: hunk.Patch}
		}
	}
	seen := make(map[string]bool, len(ids))
	selectedFiles := make(map[string]bool)
	selectedHunks := make(map[string]bool)
	whole, partial := false, false
	result := mutationSelection{}
	affectedPaths := make(map[string]bool)
	for _, id := range ids {
		if id == "" || seen[id] {
			return mutationSelection{}, fmt.Errorf("%w: empty or duplicate unit ID", ErrInvalidMutationSelection)
		}
		seen[id] = true
		selected, ok := units[id]
		if !ok {
			return mutationSelection{}, fmt.Errorf("%w: unknown or wrong-side unit ID", ErrInvalidMutationSelection)
		}
		file := selected.file
		if !file.MutationSupported || file.Protected || file.Status == StatusTypeChanged || file.Status == StatusUnmerged || file.OldMode == "120000" || file.NewMode == "120000" || file.OldMode == "160000" || file.NewMode == "160000" {
			return mutationSelection{}, fmt.Errorf("%w: unsupported file entity", ErrInvalidMutationSelection)
		}
		requiresWhole := file.Binary || file.Status == StatusAdded || file.Status == StatusDeleted || file.Status == StatusRenamed || file.Status == StatusCopied
		if selected.hunk != nil {
			partial = true
			if requiresWhole || len(selected.patch) == 0 || selectedFiles[file.ID] {
				return mutationSelection{}, fmt.Errorf("%w: file requires whole-entity selection", ErrInvalidMutationSelection)
			}
			selectedHunks[file.ID] = true
		} else {
			whole = true
			if len(selected.patch) == 0 || selectedHunks[file.ID] {
				return mutationSelection{}, fmt.Errorf("%w: mixed whole-file and hunk selection", ErrInvalidMutationSelection)
			}
			selectedFiles[file.ID] = true
		}
		result.patches = append(result.patches, append([]byte(nil), selected.patch...))
		for _, entry := range file.Worktree {
			if !affectedPaths[entry.Path] {
				affectedPaths[entry.Path] = true
				result.affected = append(result.affected, entry)
			}
		}
	}
	if whole && partial {
		return mutationSelection{}, fmt.Errorf("%w: mixed whole-file and hunk IDs", ErrInvalidMutationSelection)
	}
	return result, nil
}

func (b *Binding) runIndexGit(ctx context.Context, indexPath string, stdin []byte, args ...string) error {
	_, err := b.runIndexGitOutput(ctx, indexPath, stdin, args...)
	return err
}

func (b *Binding) runIndexGitOutput(ctx context.Context, indexPath string, stdin []byte, args ...string) ([]byte, error) {
	privateDir := filepath.Dir(indexPath)
	base := []string{
		"-c", "core.pager=cat", "-c", "color.ui=false", "-c", "diff.external=", "-c", "core.fsmonitor=false",
		"-c", "core.untrackedCache=false", "-c", "core.hooksPath=/dev/null", "-c", "core.attributesFile=/dev/null", "-C", b.Workspace,
	}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_PAGER=cat", "PAGER=cat", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "GIT_LITERAL_PATHSPECS=1",
		"GIT_ATTR_NOSYSTEM=1", "GIT_INDEX_FILE=" + indexPath, "GIT_WORK_TREE=" + filepath.Join(privateDir, "worktree"),
		"GIT_DIR=" + filepath.Join(privateDir, "repo"), "GIT_OBJECT_DIRECTORY=" + filepath.Join(b.CommonGitDir, "objects"),
	}
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, output)
	} else {
		return output, nil
	}
}

func (b *Binding) currentHead(ctx context.Context) (string, error) {
	head, err := runGit(ctx, b.Workspace, "rev-parse", "--verify", "HEAD^{commit}")
	if err == nil {
		return string(bytes.TrimSpace(head)), nil
	}
	if _, symbolicErr := runGit(ctx, b.Workspace, "symbolic-ref", "-q", "HEAD"); symbolicErr == nil {
		return "unborn", nil
	}
	return "", err
}

func (b *Binding) verifyAffectedWorktree(expected []WorktreeEntryIdentity) error {
	for _, identity := range expected {
		actual, _, err := b.readWorktreeEntry(identity.Path)
		if err != nil {
			return err
		}
		if actual != identity {
			return ErrUnstable
		}
	}
	return nil
}

func readExactIndex(path string) ([]byte, SerializedIndexIdentity, error) {
	data, identity, _, err := readRegularFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, SerializedIndexIdentity{}, nil
	}
	return data, identity, err
}

func readRegularFile(path string) ([]byte, SerializedIndexIdentity, fsIdentity, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, SerializedIndexIdentity{}, fsIdentity{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, SerializedIndexIdentity{}, fsIdentity{}, err
	}
	if !before.Mode().IsRegular() {
		return nil, SerializedIndexIdentity{}, fsIdentity{}, fmt.Errorf("file is not regular")
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, SerializedIndexIdentity{}, fsIdentity{}, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() {
		return nil, SerializedIndexIdentity{}, fsIdentity{}, ErrUnstable
	}
	stat, ok := before.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, SerializedIndexIdentity{}, fsIdentity{}, fmt.Errorf("missing filesystem identity")
	}
	sum := sha256.Sum256(data)
	identity := SerializedIndexIdentity{Exists: true, Digest: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	return data, identity, fsIdentity{Dev: uint64(stat.Dev), Ino: uint64(stat.Ino)}, nil
}

func writeNewFile(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func syncAndReadAlternate(path string) ([]byte, SerializedIndexIdentity, fsIdentity, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, SerializedIndexIdentity{}, fsIdentity{}, err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return nil, SerializedIndexIdentity{}, fsIdentity{}, err
	}
	if err := f.Close(); err != nil {
		return nil, SerializedIndexIdentity{}, fsIdentity{}, err
	}
	return readRegularFile(path)
}

func writeAndVerifyDescriptor(file *os.File, intended []byte) error {
	if _, err := file.Write(intended); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	actual, err := io.ReadAll(file)
	if err != nil {
		return err
	}
	if !bytes.Equal(actual, intended) {
		return fmt.Errorf("descriptor bytes differ")
	}
	return nil
}

func syncDirectory(path string) error {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	return unix.Fsync(fd)
}

func removePrivateAlternate(dir, path string) {
	if dir == "" || path == "" || filepath.Dir(path) != dir {
		return
	}
	_ = os.Remove(path)
	_ = os.Remove(path + ".lock")
	for _, shared := range privateSharedIndexes(dir) {
		_ = os.Remove(shared)
	}
	removePrivateGitDirectory(filepath.Join(dir, "repo"))
	_ = os.Remove(filepath.Join(dir, "worktree"))
	_ = os.Remove(dir)
}

func removeOwnedAlternate(dir, path string, expected fsIdentity, auxiliary map[string]fsIdentity) {
	if dir == "" || path == "" || filepath.Dir(path) != dir {
		return
	}
	if actual, ok := pathIdentity(path); ok && actual == expected {
		_ = os.Remove(path)
	}
	for auxiliaryPath, auxiliaryID := range auxiliary {
		if filepath.Dir(auxiliaryPath) == dir {
			if actual, ok := pathIdentity(auxiliaryPath); ok && actual == auxiliaryID {
				_ = os.Remove(auxiliaryPath)
			}
		}
	}
	removePrivateGitDirectory(filepath.Join(dir, "repo"))
	_ = os.Remove(filepath.Join(dir, "worktree"))
	_ = os.Remove(dir)
}

func copySharedIndexes(gitDir, privateDir string) (map[string]fsIdentity, error) {
	result := make(map[string]fsIdentity)
	paths, err := filepath.Glob(filepath.Join(gitDir, "sharedindex.*"))
	if err != nil {
		return nil, fmt.Errorf("find shared indexes: %w", err)
	}
	for _, source := range paths {
		data, _, _, err := readRegularFile(source)
		if err != nil {
			return nil, fmt.Errorf("read shared index: %w", err)
		}
		destination := filepath.Join(privateDir, filepath.Base(source))
		if err := writeNewFile(destination, data); err != nil {
			return nil, fmt.Errorf("copy shared index: %w", err)
		}
		identity, ok := pathIdentity(destination)
		if !ok {
			return nil, fmt.Errorf("identify copied shared index")
		}
		result[destination] = identity
	}
	return result, nil
}

func privateSharedIndexes(dir string) []string {
	paths, _ := filepath.Glob(filepath.Join(dir, "sharedindex.*"))
	return paths
}

func setupMutationEnvironment(dir, objectFormat string) error {
	repoDir := filepath.Join(dir, "repo")
	worktreeDir := filepath.Join(dir, "worktree")
	if err := os.Mkdir(repoDir, 0o700); err != nil {
		return fmt.Errorf("create private git directory: %w", err)
	}
	if err := os.Mkdir(filepath.Join(repoDir, "objects"), 0o700); err != nil {
		return fmt.Errorf("create private object directory: %w", err)
	}
	if err := os.Mkdir(filepath.Join(repoDir, "refs"), 0o700); err != nil {
		return fmt.Errorf("create private refs directory: %w", err)
	}
	if err := os.Mkdir(worktreeDir, 0o700); err != nil {
		return fmt.Errorf("create private git worktree: %w", err)
	}
	config := []byte("[core]\n\trepositoryformatversion = 0\n\tbare = false\n")
	if objectFormat != "sha1" {
		config = []byte("[core]\n\trepositoryformatversion = 1\n\tbare = false\n[extensions]\n\tobjectformat = " + objectFormat + "\n")
	}
	if err := writeNewFile(filepath.Join(repoDir, "config"), config); err != nil {
		return fmt.Errorf("write private git config: %w", err)
	}
	if err := writeNewFile(filepath.Join(repoDir, "HEAD"), []byte("ref: refs/heads/agogit-private\n")); err != nil {
		return fmt.Errorf("write private HEAD: %w", err)
	}
	return nil
}

func removePrivateGitDirectory(repoDir string) {
	_ = os.Remove(filepath.Join(repoDir, "config"))
	_ = os.Remove(filepath.Join(repoDir, "HEAD"))
	_ = os.Remove(filepath.Join(repoDir, "objects"))
	_ = os.Remove(filepath.Join(repoDir, "refs"))
	_ = os.Remove(repoDir)
}

func pathIdentity(path string) (fsIdentity, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fsIdentity{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fsIdentity{}, false
	}
	return fsIdentity{Dev: uint64(stat.Dev), Ino: uint64(stat.Ino)}, true
}

func pathMatchesDescriptor(path string, file *os.File) bool {
	pathInfo, pathErr := os.Lstat(path)
	fileInfo, fileErr := file.Stat()
	return pathErr == nil && fileErr == nil && pathInfo.Mode().IsRegular() && os.SameFile(pathInfo, fileInfo)
}

func removeOwnedLock(path string, file *os.File) {
	if pathMatchesDescriptor(path, file) {
		_ = os.Remove(path)
	}
}
