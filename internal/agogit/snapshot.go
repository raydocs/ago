// Package agogit produces read-only, worktree-bound Git change snapshots.
package agogit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

var (
	ErrNotRepository   = errors.New("not a non-bare git repository")
	ErrIdentityChanged = errors.New("bound repository identity changed")
	ErrUnstable        = errors.New("git snapshot remained unstable")
)

type ExecutorIdentity struct{ Generation, Environment string }
type fsIdentity struct{ Dev, Ino uint64 }

type Binding struct {
	Workspace, GitDir, CommonGitDir, ObjectFormat string
	Executor                                      ExecutorIdentity
	workspaceID, gitDirID, commonGitDirID         fsIdentity
}

type Status string

const (
	StatusAdded       Status = "added"
	StatusModified    Status = "modified"
	StatusDeleted     Status = "deleted"
	StatusRenamed     Status = "renamed"
	StatusCopied      Status = "copied"
	StatusTypeChanged Status = "type-changed"
	StatusUnmerged    Status = "unmerged"
)

type Hunk struct {
	ID, Header string
	Patch      []byte `json:"-"`
}
type WorktreeEntryIdentity struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
	Digest string `json:"digest,omitempty"`
	Dev    uint64 `json:"dev"`
	Ino    uint64 `json:"ino"`
}
type FileChange struct {
	ID                string
	Path              string
	OldPath           string `json:",omitempty"`
	ContentDigest     string
	Status            Status
	OldMode           string `json:",omitempty"`
	NewMode           string `json:",omitempty"`
	Binary            bool
	Protected         bool
	MutationSupported bool
	Hunks             []Hunk                  `json:",omitempty"`
	Patch             []byte                  `json:"-"`
	Worktree          []WorktreeEntryIdentity `json:"-"`
}
type SerializedIndexIdentity struct {
	Exists bool   `json:"exists"`
	Digest string `json:"digest,omitempty"`
	Size   int64  `json:"size"`
}
type Snapshot struct {
	Digest           string
	HeadOID          string
	IndexDigest      string
	SerializedIndex  SerializedIndexIdentity
	Staged, Unstaged []FileChange
}
type SnapshotOptions struct {
	MaxCaptures int
	// AfterCapture is an optional observation/barrier callback. It runs while the
	// per-worktree read lock is held and must not call Snapshot recursively.
	AfterCapture    func(captureNumber int)
	CurrentExecutor *ExecutorIdentity
}

var locks sync.Map

func worktreeLock(path string) *sync.Mutex {
	v, _ := locks.LoadOrStore(path, new(sync.Mutex))
	return v.(*sync.Mutex)
}

func Bind(ctx context.Context, workspace string, executor ExecutorIdentity) (*Binding, error) {
	real, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return nil, fmt.Errorf("canonical workspace: %w", err)
	}
	real, err = filepath.Abs(real)
	if err != nil {
		return nil, err
	}
	run := func(args ...string) ([]byte, error) { return runGit(ctx, real, args...) }
	bare, err := run("rev-parse", "--is-bare-repository")
	if err != nil || strings.TrimSpace(string(bare)) != "false" {
		return nil, ErrNotRepository
	}
	top, err := run("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, ErrNotRepository
	}
	topReal, err := filepath.EvalSymlinks(strings.TrimSpace(string(top)))
	if err != nil {
		return nil, err
	}
	gitDir, err := gitPath(real, run, "--git-dir")
	if err != nil {
		return nil, err
	}
	common, err := gitPath(real, run, "--git-common-dir")
	if err != nil {
		return nil, err
	}
	format, err := run("rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	b := &Binding{Workspace: topReal, GitDir: gitDir, CommonGitDir: common, ObjectFormat: strings.TrimSpace(string(format)), Executor: executor}
	if b.workspaceID, err = identity(topReal); err != nil {
		return nil, err
	}
	if b.gitDirID, err = identity(gitDir); err != nil {
		return nil, err
	}
	if b.commonGitDirID, err = identity(common); err != nil {
		return nil, err
	}
	return b, nil
}

func gitPath(workspace string, run func(...string) ([]byte, error), arg string) (string, error) {
	v, err := run("rev-parse", arg)
	if err != nil {
		return "", err
	}
	p := strings.TrimSpace(string(v))
	if !filepath.IsAbs(p) {
		p = filepath.Join(workspace, p)
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	return filepath.Clean(p), nil
}
func identity(path string) (fsIdentity, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return fsIdentity{}, err
	}
	return fsIdentity{uint64(st.Dev), uint64(st.Ino)}, nil
}

// RepositoryID and WorktreeID are opaque, stable public identities. They bind
// canonical paths to filesystem objects without disclosing either to clients.
func (b *Binding) RepositoryID() string {
	return opaqueID("repository", b.CommonGitDir, b.ObjectFormat, formatIdentity(b.commonGitDirID))
}

func (b *Binding) WorktreeID() string {
	return opaqueID("worktree", b.RepositoryID(), b.Workspace, b.GitDir, formatIdentity(b.workspaceID), formatIdentity(b.gitDirID))
}

func (b *Binding) BaseIdentity() string {
	return opaqueID("binding", b.RepositoryID(), b.WorktreeID(), b.Executor.Environment, b.Executor.Generation)
}

func formatIdentity(id fsIdentity) string { return fmt.Sprintf("%d:%d", id.Dev, id.Ino) }

func opaqueID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(strconv.Itoa(len(part))))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (b *Binding) validIdentity() bool {
	a, e1 := identity(b.Workspace)
	c, e2 := identity(b.GitDir)
	d, e3 := identity(b.CommonGitDir)
	return e1 == nil && e2 == nil && e3 == nil && a == b.workspaceID && c == b.gitDirID && d == b.commonGitDirID
}

func (b *Binding) Snapshot(ctx context.Context, opts SnapshotOptions) (*Snapshot, error) {
	mu := worktreeLock(b.Workspace)
	mu.Lock()
	defer mu.Unlock()
	if !b.validIdentity() || (opts.CurrentExecutor != nil && *opts.CurrentExecutor != b.Executor) {
		return nil, ErrIdentityChanged
	}
	max := opts.MaxCaptures
	if max < 2 {
		max = 3
	}
	var previous *Snapshot
	for n := 1; n <= max; n++ {
		current, err := b.capture(ctx)
		if err != nil {
			return nil, err
		}
		if opts.AfterCapture != nil {
			opts.AfterCapture(n)
		}
		if previous != nil && previous.Digest == current.Digest {
			return current, nil
		}
		previous = current
	}
	return nil, ErrUnstable
}

func (b *Binding) capture(ctx context.Context) (*Snapshot, error) {
	head, err := runGit(ctx, b.Workspace, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		if _, symbolicErr := runGit(ctx, b.Workspace, "symbolic-ref", "-q", "HEAD"); symbolicErr != nil {
			return nil, err
		}
		head = []byte("unborn\n")
	}
	index, err := runGit(ctx, b.Workspace, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	headOID := strings.TrimSpace(string(head))
	indexSum := sha256.Sum256(index)
	indexDigest := hex.EncodeToString(indexSum[:])
	serializedIndex, err := readSerializedIndex(filepath.Join(b.GitDir, "index"))
	if err != nil {
		return nil, err
	}
	staged, err := b.diff(ctx, true)
	if err != nil {
		return nil, err
	}
	unstaged, err := b.diff(ctx, false)
	if err != nil {
		return nil, err
	}
	untracked, err := runGit(ctx, b.Workspace, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	for _, p := range split0(untracked) {
		if p == "" {
			continue
		}
		f, err := b.untracked(p)
		if err != nil {
			return nil, err
		}
		unstaged = append(unstaged, f)
	}
	sortChanges(staged)
	sortChanges(unstaged)
	canonical := struct {
		HeadOID, IndexDigest string
		SerializedIndex      SerializedIndexIdentity
		Staged, Unstaged     []FileChange
	}{headOID, indexDigest, serializedIndex, staged, unstaged}
	raw, _ := json.Marshal(canonical)
	sum := sha256.Sum256(raw)
	return &Snapshot{Digest: hex.EncodeToString(sum[:]), HeadOID: headOID, IndexDigest: indexDigest, SerializedIndex: serializedIndex, Staged: staged, Unstaged: unstaged}, nil
}

func readSerializedIndex(path string) (SerializedIndexIdentity, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return SerializedIndexIdentity{}, nil
	}
	if err != nil {
		return SerializedIndexIdentity{}, fmt.Errorf("open serialized index: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return SerializedIndexIdentity{}, fmt.Errorf("stat serialized index: %w", err)
	}
	if !info.Mode().IsRegular() {
		return SerializedIndexIdentity{}, fmt.Errorf("serialized index is not a regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return SerializedIndexIdentity{}, fmt.Errorf("hash serialized index: %w", err)
	}
	return SerializedIndexIdentity{Exists: true, Digest: hex.EncodeToString(h.Sum(nil)), Size: info.Size()}, nil
}

func (b *Binding) diff(ctx context.Context, cached bool) ([]FileChange, error) {
	args := []string{"diff", "--raw", "-z", "-M", "--no-ext-diff", "--no-textconv"}
	side := "unstaged"
	if cached {
		args = append(args, "--cached")
		side = "staged"
	}
	out, err := runGit(ctx, b.Workspace, args...)
	if err != nil {
		return nil, err
	}
	fields := split0(out)
	result := []FileChange{}
	for i := 0; i < len(fields) && fields[i] != ""; {
		header := fields[i]
		i++
		parts := strings.Fields(header)
		if len(parts) < 5 {
			return nil, fmt.Errorf("invalid git raw record %q", header)
		}
		code := parts[4]
		if i >= len(fields) {
			return nil, fmt.Errorf("missing diff path")
		}
		oldPath := fields[i]
		i++
		path := oldPath
		if code[0] == 'R' || code[0] == 'C' {
			if i >= len(fields) {
				return nil, fmt.Errorf("missing rename path")
			}
			path = fields[i]
			i++
		}
		f := FileChange{Path: path, OldMode: strings.TrimPrefix(parts[0], ":"), NewMode: parts[1], Status: status(code[0])}
		if path != oldPath {
			f.OldPath = oldPath
		}
		patchArgs := []string{"diff", "--binary", "--full-index", "--unified=3", "-M", "--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/"}
		if cached {
			patchArgs = append(patchArgs, "--cached")
		}
		patchArgs = append(patchArgs, "--")
		if f.OldPath != "" {
			patchArgs = append(patchArgs, f.OldPath)
		}
		patchArgs = append(patchArgs, path)
		patch, er := runGit(ctx, b.Workspace, patchArgs...)
		if er != nil {
			return nil, er
		}
		f.Patch = append([]byte(nil), patch...)
		patchSum := sha256.Sum256(patch)
		f.ContentDigest = hex.EncodeToString(patchSum[:])
		f.Binary = bytes.Contains(patch, []byte("GIT binary patch")) || bytes.Contains(patch, []byte("Binary files "))
		for occurrence, hunk := range completeHunkPatches(patch) {
			hunk.ID = opaque("hunk", side, path, f.ContentDigest, hunk.Header, strconv.Itoa(occurrence+1))
			f.Hunks = append(f.Hunks, hunk)
		}
		for _, worktreePath := range []string{f.OldPath, f.Path} {
			if worktreePath == "" || (len(f.Worktree) > 0 && f.Worktree[len(f.Worktree)-1].Path == worktreePath) {
				continue
			}
			entry, _, entryErr := b.readWorktreeEntry(worktreePath)
			if entryErr != nil {
				return nil, entryErr
			}
			f.Worktree = append(f.Worktree, entry)
		}
		decorate(&f, side)
		result = append(result, f)
	}
	return result, nil
}

func completeHunkPatches(patch []byte) []Hunk {
	starts := make([]int, 0)
	for offset := 0; offset < len(patch); {
		end := bytes.IndexByte(patch[offset:], '\n')
		if end < 0 {
			end = len(patch) - offset
		} else {
			end++
		}
		if bytes.HasPrefix(patch[offset:offset+end], []byte("@@ ")) {
			starts = append(starts, offset)
		}
		offset += end
	}
	if len(starts) == 0 {
		return nil
	}
	preamble := patch[:starts[0]]
	result := make([]Hunk, 0, len(starts))
	for index, start := range starts {
		end := len(patch)
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		headerEnd := bytes.IndexByte(patch[start:end], '\n')
		if headerEnd < 0 {
			headerEnd = end - start
		}
		result = append(result, Hunk{
			Header: string(patch[start : start+headerEnd]),
			Patch:  append(append([]byte(nil), preamble...), patch[start:end]...),
		})
	}
	return result
}

func (b *Binding) untracked(path string) (FileChange, error) {
	entry, data, err := b.readWorktreeEntry(path)
	if err != nil {
		return FileChange{}, err
	}
	mode := "100644"
	if entry.Kind == "symlink" {
		mode = "120000"
	} else if entry.Mode&0111 != 0 {
		mode = "100755"
	}
	binary := err == nil && bytes.IndexByte(data, 0) >= 0
	sum := sha256.Sum256(data)
	f := FileChange{Path: path, Status: StatusAdded, NewMode: mode, Binary: binary, ContentDigest: hex.EncodeToString(sum[:]), Worktree: []WorktreeEntryIdentity{entry}}
	if entry.Kind == "regular" {
		patch, patchErr := runGitDiff(b.Workspace, "diff", "--no-index", "--binary", "--full-index", "--unified=3", "--no-color", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", "--", "/dev/null", path)
		if patchErr != nil {
			return FileChange{}, patchErr
		}
		f.Patch = append([]byte(nil), patch...)
		patchSum := sha256.Sum256(patch)
		f.ContentDigest = hex.EncodeToString(patchSum[:])
		f.Binary = bytes.Contains(patch, []byte("GIT binary patch")) || bytes.Contains(patch, []byte("Binary files "))
		for occurrence, hunk := range completeHunkPatches(patch) {
			hunk.ID = opaque("hunk", "unstaged", path, f.ContentDigest, hunk.Header, strconv.Itoa(occurrence+1))
			f.Hunks = append(f.Hunks, hunk)
		}
	}
	decorate(&f, "unstaged")
	return f, nil
}

func (b *Binding) readWorktreeEntry(relative string) (WorktreeEntryIdentity, []byte, error) {
	if relative == "" || strings.IndexByte(relative, 0) >= 0 || pathpkg.IsAbs(relative) || pathpkg.Clean(relative) != relative {
		return WorktreeEntryIdentity{}, nil, fmt.Errorf("invalid git worktree path")
	}
	parts := strings.Split(relative, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return WorktreeEntryIdentity{}, nil, fmt.Errorf("invalid git worktree path")
		}
	}
	dirfd, err := unix.Open(b.Workspace, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return WorktreeEntryIdentity{}, nil, fmt.Errorf("open worktree root: %w", err)
	}
	defer func() { _ = unix.Close(dirfd) }()
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(dirfd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			return WorktreeEntryIdentity{}, nil, fmt.Errorf("open worktree parent: %w", openErr)
		}
		_ = unix.Close(dirfd)
		dirfd = next
	}
	leaf := parts[len(parts)-1]
	var stat unix.Stat_t
	if err := unix.Fstatat(dirfd, leaf, &stat, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return WorktreeEntryIdentity{Path: relative, Kind: "absent"}, nil, nil
	} else if err != nil {
		return WorktreeEntryIdentity{}, nil, fmt.Errorf("inspect worktree entry: %w", err)
	}
	entry := WorktreeEntryIdentity{Path: relative, Mode: uint32(stat.Mode), Size: stat.Size, Dev: uint64(stat.Dev), Ino: uint64(stat.Ino)}
	switch stat.Mode & unix.S_IFMT {
	case unix.S_IFLNK:
		entry.Kind = "symlink"
		buffer := make([]byte, 4096)
		n, err := unix.Readlinkat(dirfd, leaf, buffer)
		if err != nil {
			return WorktreeEntryIdentity{}, nil, fmt.Errorf("read worktree symlink: %w", err)
		}
		data := append([]byte(nil), buffer[:n]...)
		sum := sha256.Sum256(data)
		entry.Size = int64(len(data))
		entry.Digest = hex.EncodeToString(sum[:])
		return entry, data, nil
	case unix.S_IFREG:
		entry.Kind = "regular"
	default:
		entry.Kind = "other"
		return entry, nil, nil
	}
	fd, err := unix.Openat(dirfd, leaf, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return WorktreeEntryIdentity{}, nil, fmt.Errorf("open worktree file: %w", err)
	}
	file := os.NewFile(uintptr(fd), relative)
	if file == nil {
		_ = unix.Close(fd)
		return WorktreeEntryIdentity{}, nil, fmt.Errorf("open worktree file descriptor")
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return WorktreeEntryIdentity{}, nil, fmt.Errorf("read worktree file: %w", err)
	}
	var after unix.Stat_t
	if err := unix.Fstat(fd, &after); err != nil {
		return WorktreeEntryIdentity{}, nil, fmt.Errorf("revalidate worktree file: %w", err)
	}
	if stat.Dev != after.Dev || stat.Ino != after.Ino || stat.Mode != after.Mode || stat.Size != after.Size {
		return WorktreeEntryIdentity{}, nil, ErrUnstable
	}
	sum := sha256.Sum256(data)
	entry.Digest = hex.EncodeToString(sum[:])
	return entry, data, nil
}
func decorate(f *FileChange, side string) {
	f.Protected = f.Path == "thread-app/src/index.ts" || f.Path == "thread-app/test/thread-api.test.mjs"
	unsafe := f.OldMode == "120000" || f.NewMode == "120000" || f.OldMode == "160000" || f.NewMode == "160000" || f.Status == StatusUnmerged
	f.MutationSupported = !f.Protected && !unsafe
	f.ID = opaque("file", side, f.OldPath, f.Path, string(f.Status), f.ContentDigest)
}
func status(c byte) Status {
	switch c {
	case 'A':
		return StatusAdded
	case 'D':
		return StatusDeleted
	case 'R':
		return StatusRenamed
	case 'C':
		return StatusCopied
	case 'T':
		return StatusTypeChanged
	case 'U':
		return StatusUnmerged
	default:
		return StatusModified
	}
}
func split0(v []byte) []string {
	raw := bytes.Split(v, []byte{0})
	out := make([]string, len(raw))
	for i := range raw {
		out[i] = string(raw[i])
	}
	return out
}
func opaque(v ...string) string {
	h := sha256.New()
	for _, s := range v {
		h.Write([]byte(strconv.Itoa(len(s))))
		h.Write([]byte{':'})
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))[:24]
}
func sortChanges(v []FileChange) {
	sort.Slice(v, func(i, j int) bool {
		if v[i].Path == v[j].Path {
			return v[i].OldPath < v[j].OldPath
		}
		return v[i].Path < v[j].Path
	})
}

func runGit(ctx context.Context, workspace string, args ...string) ([]byte, error) {
	return runGitCommand(ctx, workspace, false, args...)
}

func runGitDiff(workspace string, args ...string) ([]byte, error) {
	return runGitCommand(context.Background(), workspace, true, args...)
}

func runGitCommand(ctx context.Context, workspace string, allowDiffExit bool, args ...string) ([]byte, error) {
	base := []string{"-c", "core.pager=cat", "-c", "color.ui=false", "-c", "diff.external=", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "core.hooksPath=/dev/null", "-C", workspace}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	path := os.Getenv("PATH")
	cmd.Env = []string{"PATH=" + path, "LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_PAGER=cat", "PAGER=cat", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0"}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if allowDiffExit && ee.ExitCode() == 1 {
				return out, nil
			}
			return nil, fmt.Errorf("git %s: %w: %s", args[0], err, ee.Stderr)
		}
		return nil, err
	}
	return out, nil
}
