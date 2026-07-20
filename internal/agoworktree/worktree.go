// Package agoworktree gives each write attempt its own isolated git
// worktree, so a real LLM-driven executor never writes directly into the
// user's canonical working tree.
//
// Every attempt gets a private directory under a dedicated, Ago-owned root
// and a throwaway branch checked out at the repository's current revision.
// The canonical repository is only ever touched with `git worktree add` (to
// create) and `git worktree remove` / `git worktree list` (to inspect and
// tear down) — this package never runs reset, clean, checkout, restore, or
// stash against it. Removal additionally refuses to act on anything that is
// not both physically inside the managed root and known to git as a
// worktree it registered, so a Lease whose Path has been altered (by
// accident or by a compromised executor) can never cause a deletion outside
// the sandbox.
package agoworktree

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Options configures a Manager.
type Options struct {
	// Root is where Ago-owned worktrees live. Created 0700 if absent.
	Root string
	// Now is the clock used to seed worktree IDs. Defaults to time.Now.
	Now func() time.Time
}

// Manager creates and tears down isolated worktrees under a single Root.
type Manager struct {
	root string
	now  func() time.Time

	// mu guards repoLocks. git's own worktree administrative files (under
	// <repo>/.git/worktrees/) are not safe for truly concurrent `worktree
	// add`/`worktree remove` on the same repository — it can corrupt its
	// own bookkeeping under real concurrency. repoLocks serializes those
	// calls per canonical repository path without serializing unrelated
	// repositories against each other.
	mu        sync.Mutex
	repoLocks map[string]*sync.Mutex
}

func (m *Manager) lockFor(repo string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.repoLocks == nil {
		m.repoLocks = map[string]*sync.Mutex{}
	}
	lock, ok := m.repoLocks[repo]
	if !ok {
		lock = &sync.Mutex{}
		m.repoLocks[repo] = lock
	}
	return lock
}

// New validates and prepares Options.Root and returns a Manager bound to it.
func New(options Options) (*Manager, error) {
	root := strings.TrimSpace(options.Root)
	if root == "" {
		return nil, fmt.Errorf("agoworktree: Root is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("agoworktree: create root %q: %w", root, err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("agoworktree: stat root %q: %w", root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("agoworktree: root %q is not a directory", root)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Manager{root: root, now: now}, nil
}

// Lease describes one isolated worktree created for a single attempt.
type Lease struct {
	ID           string // generated, opaque
	Path         string // absolute path to the isolated worktree
	Repository   string // canonical repository root it was created from
	BaseRevision string // commit SHA the worktree started at
	// Branch is always empty: worktrees are detached so nothing is written to
	// the repository's ref namespace.
	Branch string // the throwaway branch name Ago created
}

// Change describes one repository-relative path modified in a worktree.
// BeforeHash is "" for a new file, AfterHash is "" for a delete.
type Change struct {
	Path       string
	BeforeHash string
	AfterHash  string
	Deleted    bool
}

// Create makes an isolated worktree for one attempt: a private directory
// under Root, checked out at repositoryRoot's current HEAD on a new
// throwaway branch. Nothing about repositoryRoot's own working tree, index,
// or HEAD is ever changed by this call.
func (m *Manager) Create(ctx context.Context, repositoryRoot, attemptID string) (Lease, error) {
	repo, err := resolveRepository(ctx, repositoryRoot)
	if err != nil {
		return Lease{}, err
	}

	baseOut, err := runGit(ctx, repo, "rev-parse", "HEAD")
	if err != nil {
		return Lease{}, fmt.Errorf("resolve base revision for %q: %w", repositoryRoot, err)
	}
	base := strings.TrimSpace(string(baseOut))
	if base == "" {
		return Lease{}, fmt.Errorf("repository %q has no base revision", repositoryRoot)
	}

	id, err := m.generateID()
	if err != nil {
		return Lease{}, err
	}
	path := filepath.Join(m.root, id)
	if _, statErr := os.Stat(path); statErr == nil {
		return Lease{}, fmt.Errorf("worktree path %q already exists", path)
	}

	lock := m.lockFor(repo)
	lock.Lock()
	// Detached, deliberately: `worktree add -b` writes a real branch into the
	// user's repository that `worktree remove` does not delete, so every
	// attempt would permanently pollute their ref namespace. Detaching gives
	// the same isolation and leaves nothing behind.
	_, addErr := runGit(ctx, repo, "worktree", "add", "--detach", path, base)
	lock.Unlock()
	if addErr != nil {
		return Lease{}, fmt.Errorf("create worktree: %w", addErr)
	}

	// git worktree add creates the directory using the process umask
	// (typically 0755). Lock it down immediately: nothing else on the
	// machine should be able to read or traverse an in-progress attempt.
	if err := os.Chmod(path, 0o700); err != nil {
		lock.Lock()
		_, _ = runGit(ctx, repo, "worktree", "remove", "--force", path)
		lock.Unlock()
		return Lease{}, fmt.Errorf("secure worktree %q: %w", path, err)
	}

	return Lease{
		ID:           id,
		Path:         path,
		Repository:   repo,
		BaseRevision: base,
	}, nil
}

// Changes reports repository-relative paths modified in the worktree, each
// with before/after SHA-256 content hashes computed from git: "before" comes
// from the lease's base revision, "after" from the current working file.
func (m *Manager) Changes(ctx context.Context, lease Lease) ([]Change, error) {
	if strings.TrimSpace(lease.Path) == "" {
		return nil, fmt.Errorf("lease has no path")
	}
	base := strings.TrimSpace(lease.BaseRevision)
	if base == "" {
		base = "HEAD"
	}

	out, err := runGit(ctx, lease.Path, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return nil, fmt.Errorf("git status: %w", err)
	}
	paths, err := statusPaths(out)
	if err != nil {
		return nil, err
	}

	changes := make([]Change, 0, len(paths))
	for _, path := range paths {
		change, err := describeChange(ctx, lease.Path, base, path)
		if err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes, nil
}

// describeChange hashes a path's content at base and in the working tree
// directly, rather than trying to interpret every git status letter
// combination — that keeps the modified/added/deleted classification correct
// regardless of exactly how git chose to report the change.
func describeChange(ctx context.Context, worktree, base, path string) (Change, error) {
	change := Change{Path: path}

	existsAtBase, err := pathExistsAtRevision(ctx, worktree, base, path)
	if err != nil {
		return Change{}, fmt.Errorf("check base content for %q: %w", path, err)
	}
	if existsAtBase {
		content, err := runGit(ctx, worktree, "show", base+":"+path)
		if err != nil {
			return Change{}, fmt.Errorf("read base content for %q: %w", path, err)
		}
		change.BeforeHash = hashBytes(content)
	}

	full := filepath.Join(worktree, filepath.FromSlash(path))
	info, statErr := os.Lstat(full)
	if statErr != nil || info.IsDir() {
		change.Deleted = true
		return change, nil
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return Change{}, fmt.Errorf("read working file %q: %w", path, err)
	}
	change.AfterHash = hashBytes(data)
	return change, nil
}

func pathExistsAtRevision(ctx context.Context, dir, revision, path string) (bool, error) {
	out, err := runGit(ctx, dir, "ls-tree", "-z", revision, "--", path)
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

func statusPaths(out []byte) ([]string, error) {
	raw := strings.TrimRight(string(out), "\x00")
	if raw == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var paths []string
	for _, entry := range strings.Split(raw, "\x00") {
		if entry == "" {
			continue
		}
		if len(entry) < 4 || entry[2] != ' ' {
			return nil, fmt.Errorf("unexpected git status entry %q", entry)
		}
		path := entry[3:]
		if path == "" || filepath.IsAbs(path) || hasDotDotSegment(path) {
			return nil, fmt.Errorf("git reported unsafe path %q", path)
		}
		if !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// ViolatesScope returns the changed paths that fall outside every allowed
// scope. A scope is a repository-relative file or directory prefix; the
// match is on path-segment boundaries, so scope "docs" matches "docs/a.md"
// but not "docs2/a.md". It is a pure function: absolute paths and any path
// containing a ".." segment are always violations, independent of scopes.
func ViolatesScope(changes []Change, allowedScopes []string) []string {
	var violations []string
	for _, change := range changes {
		if !withinAnyScope(change.Path, allowedScopes) {
			violations = append(violations, change.Path)
		}
	}
	return violations
}

func withinAnyScope(path string, scopes []string) bool {
	if isUnsafeRelativePath(path) {
		return false
	}
	for _, scope := range scopes {
		if isUnsafeRelativePath(scope) {
			continue
		}
		if pathHasScopePrefix(path, scope) {
			return true
		}
	}
	return false
}

func isUnsafeRelativePath(p string) bool {
	return p == "" || filepath.IsAbs(p) || hasDotDotSegment(p)
}

func hasDotDotSegment(path string) bool {
	for _, segment := range strings.Split(filepath.ToSlash(path), "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

func pathHasScopePrefix(path, scope string) bool {
	path = filepath.ToSlash(path)
	scope = strings.TrimSuffix(filepath.ToSlash(scope), "/")
	if path == scope {
		return true
	}
	return strings.HasPrefix(path, scope+"/")
}

// Remove deletes only a worktree this Manager created. It refuses to act
// unless the lease's path (after resolving symlinks) is both inside Root and
// currently registered as a worktree of Repository according to git itself —
// a caller passing a Lease with a doctored Path must get an error, not a
// deletion. The canonical repository is only ever asked `git worktree
// remove`, never touched with a mutating command against its own tree.
// CreateAt is Create against an explicit starting revision, so a downstream
// task can begin from work its dependencies already had integrated rather than
// from the repository's own HEAD.
func (m *Manager) CreateAt(ctx context.Context, repositoryRoot, attemptID, revision string) (Lease, error) {
	if strings.TrimSpace(revision) == "" {
		return m.Create(ctx, repositoryRoot, attemptID)
	}
	lease, err := m.Create(ctx, repositoryRoot, attemptID)
	if err != nil {
		return Lease{}, err
	}
	// Move the isolated copy onto the requested revision. This only ever
	// touches Ago's own worktree; the canonical tree is untouched.
	if _, err := runGit(ctx, lease.Path, "checkout", "--detach", revision); err != nil {
		_ = m.Remove(ctx, lease)
		return Lease{}, fmt.Errorf("start worktree at %q: %w", revision, err)
	}
	resolved, err := runGit(ctx, lease.Path, "rev-parse", "HEAD")
	if err != nil {
		_ = m.Remove(ctx, lease)
		return Lease{}, err
	}
	lease.BaseRevision = strings.TrimSpace(string(resolved))
	return lease, nil
}

// Patch renders everything the worktree changed as a durable binary-safe patch.
//
// It is produced before the worktree is removed and is the only thing that
// survives it. Binary safety matters because an executor may legitimately
// change a non-text file, and a patch that silently dropped it would make the
// evidence a lie.
func (m *Manager) Patch(ctx context.Context, lease Lease) ([]byte, error) {
	// Stage everything so new and deleted files appear in the diff too.
	if _, err := runGit(ctx, lease.Path, "add", "-A"); err != nil {
		return nil, fmt.Errorf("stage worktree changes: %w", err)
	}
	patch, err := runGit(ctx, lease.Path, "diff", "--cached", "--binary", "--no-color", lease.BaseRevision)
	if err != nil {
		return nil, fmt.Errorf("render patch: %w", err)
	}
	return patch, nil
}

func (m *Manager) Remove(ctx context.Context, lease Lease) error {
	if strings.TrimSpace(lease.Path) == "" {
		return fmt.Errorf("lease has no path")
	}
	realRoot, err := filepath.EvalSymlinks(m.root)
	if err != nil {
		return fmt.Errorf("resolve managed root: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(lease.Path)
	if err != nil {
		return fmt.Errorf("resolve worktree path %q: %w", lease.Path, err)
	}

	// This containment check is the whole point of the package: a Lease
	// whose Path has been altered to point outside our managed root must be
	// refused here, before we ever ask git about it or touch the disk.
	if !isWithinManagedRoot(realRoot, realPath) {
		return fmt.Errorf("refusing to remove %q: outside managed root %q", lease.Path, m.root)
	}

	registered, err := registeredWorktrees(ctx, lease.Repository)
	if err != nil {
		return fmt.Errorf("list worktrees for %q: %w", lease.Repository, err)
	}
	if !registered[realPath] {
		return fmt.Errorf("refusing to remove %q: not a worktree git knows about for %q", lease.Path, lease.Repository)
	}

	lock := m.lockFor(lease.Repository)
	lock.Lock()
	_, err = runGit(ctx, lease.Repository, "worktree", "remove", "--force", realPath)
	lock.Unlock()
	if err != nil {
		return fmt.Errorf("remove worktree %q: %w", realPath, err)
	}
	return nil
}

func isWithinManagedRoot(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path == root {
		return false
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

func registeredWorktrees(ctx context.Context, repository string) (map[string]bool, error) {
	if strings.TrimSpace(repository) == "" {
		return nil, fmt.Errorf("repository is required")
	}
	out, err := runGit(ctx, repository, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		p, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		set[real] = true
	}
	return set, nil
}

// Reconcile removes worktrees under Root that are not in the keep set (keyed
// by Lease.ID). Used at startup after a crash to clear out anything left
// behind by an attempt that never got to call Remove.
func (m *Manager) Reconcile(ctx context.Context, keep map[string]bool) (int, error) {
	realRoot, err := filepath.EvalSymlinks(m.root)
	if err != nil {
		return 0, fmt.Errorf("resolve managed root: %w", err)
	}
	entries, err := os.ReadDir(realRoot)
	if err != nil {
		return 0, fmt.Errorf("read managed root %q: %w", m.root, err)
	}
	removed := 0
	for _, entry := range entries {
		if !entry.IsDir() || keep[entry.Name()] {
			continue
		}
		path := filepath.Join(realRoot, entry.Name())
		if err := removeUnreferenced(ctx, path); err != nil {
			return removed, fmt.Errorf("reconcile %q: %w", entry.Name(), err)
		}
		removed++
	}
	return removed, nil
}

// removeUnreferenced deletes a directory that lives entirely inside our own
// managed root, so — unlike Remove — there is no containment question here.
// The only thing left to decide is whether git still considers it a live
// worktree: ask it from the directory's own path, so we never have to guess
// which repository originally owned it. If git no longer recognizes it at
// all (e.g. debris from a crash mid-Create), it is safe to remove outright.
func removeUnreferenced(ctx context.Context, path string) error {
	if _, err := runGit(ctx, path, "rev-parse", "--is-inside-work-tree"); err != nil {
		return os.RemoveAll(path)
	}
	if _, err := runGit(ctx, path, "worktree", "remove", "--force", path); err != nil {
		return fmt.Errorf("remove worktree: %w", err)
	}
	return nil
}

// resolveRepository confirms repositoryRoot exists, is a directory, and is
// itself the top level of a git working tree (not merely a subdirectory of
// one), returning its symlink-resolved canonical path.
func resolveRepository(ctx context.Context, repositoryRoot string) (string, error) {
	if strings.TrimSpace(repositoryRoot) == "" {
		return "", fmt.Errorf("repository root is required")
	}
	real, err := filepath.EvalSymlinks(repositoryRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repository root %q: %w", repositoryRoot, err)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", fmt.Errorf("stat repository root %q: %w", repositoryRoot, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repository root %q is not a directory", repositoryRoot)
	}

	if out, err := runGit(ctx, real, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(string(out)) != "true" {
		return "", fmt.Errorf("repository root %q is not a git working tree", repositoryRoot)
	}
	topOut, err := runGit(ctx, real, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("resolve git top-level for %q: %w", repositoryRoot, err)
	}
	top, err := filepath.EvalSymlinks(strings.TrimSpace(string(topOut)))
	if err != nil {
		return "", fmt.Errorf("resolve git top-level for %q: %w", repositoryRoot, err)
	}
	if top != real {
		return "", fmt.Errorf("repository root %q is not the git top-level (found %q)", repositoryRoot, top)
	}
	return real, nil
}

func (m *Manager) generateID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate worktree id: %w", err)
	}
	return fmt.Sprintf("%x%s", m.now().UnixNano(), hex.EncodeToString(buf)), nil
}

func sanitizeBranchComponent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	if out == "" {
		return "attempt"
	}
	return out
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

const (
	gitTimeout = 30 * time.Second
	killGrace  = 2 * time.Second
)

// runGit executes git under a bounded timeout, isolated into its own process
// group (Setpgid) so that cancellation can kill the whole group — including
// any child processes git spawns, such as hooks — instead of leaving
// orphans behind. Mirrors the cancellation pattern in
// internal/agolocalexec/process.go.
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start git %s: %w", strings.Join(args, " "), err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return stdout.Bytes(), nil
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		timer := time.NewTimer(killGrace)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
	}
}
