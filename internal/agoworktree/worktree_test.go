package agoworktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func gitStatus(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "-C", dir, "status", "--porcelain")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git status: %v: %s", err, out)
	}
	return string(out)
}

// testRepo builds a real throwaway git repository with a README.md, a docs/
// directory, and one commit — the fixture every test in this file starts
// from.
func testRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "test@example.invalid")
	git(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "guide.md"), []byte("guide\n"), 0o644); err != nil {
		t.Fatalf("write docs/guide.md: %v", err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-qm", "base")
	return dir
}

func newManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(Options{Root: filepath.Join(t.TempDir(), "worktrees")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestNewCreatesManagedRootWithRestrictedPermissions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "worktrees")
	if _, err := New(Options{Root: root}); err != nil {
		t.Fatalf("New: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("stat root: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("root perm = %o, want 0700", perm)
	}
}

func TestCreateChecksOutBaseRevisionWithoutTouchingCanonicalTree(t *testing.T) {
	requireGit(t)
	repoDir := testRepo(t)
	m := newManager(t)
	canonicalBefore := gitStatus(t, repoDir)

	lease, err := m.Create(context.Background(), repoDir, "attempt-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if lease.Path == "" || lease.Repository == "" || lease.BaseRevision == "" || lease.ID == "" {
		t.Fatalf("incomplete lease: %+v", lease)
	}

	data, err := os.ReadFile(filepath.Join(lease.Path, "README.md"))
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("worktree README.md = %q, err = %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(lease.Path, "docs", "guide.md")); err != nil {
		t.Fatalf("docs/guide.md missing in worktree: %v", err)
	}

	info, err := os.Stat(lease.Path)
	if err != nil {
		t.Fatalf("stat worktree: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("worktree perm = %o, want 0700", perm)
	}

	canonicalAfter := gitStatus(t, repoDir)
	if canonicalBefore != canonicalAfter {
		t.Fatalf("canonical repo status changed: before=%q after=%q", canonicalBefore, canonicalAfter)
	}
}

func TestWritingInWorktreeDoesNotAppearInCanonicalTree(t *testing.T) {
	requireGit(t)
	repoDir := testRepo(t)
	m := newManager(t)

	lease, err := m.Create(context.Background(), repoDir, "attempt")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "new.txt"), []byte("scratch\n"), 0o644); err != nil {
		t.Fatalf("write in worktree: %v", err)
	}

	if _, err := os.Stat(filepath.Join(repoDir, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected new.txt absent from canonical repo, stat err = %v", err)
	}
	if status := gitStatus(t, repoDir); status != "" {
		t.Fatalf("canonical repo dirtied: %q", status)
	}
}

func TestChangesReportsModifiedNewAndDeletedFiles(t *testing.T) {
	requireGit(t)
	repoDir := testRepo(t)
	m := newManager(t)

	lease, err := m.Create(context.Background(), repoDir, "attempt")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := os.WriteFile(filepath.Join(lease.Path, "README.md"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatalf("modify README.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "docs", "new.md"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatalf("write docs/new.md: %v", err)
	}
	if err := os.Remove(filepath.Join(lease.Path, "docs", "guide.md")); err != nil {
		t.Fatalf("remove docs/guide.md: %v", err)
	}

	changes, err := m.Changes(context.Background(), lease)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	byPath := map[string]Change{}
	for _, c := range changes {
		byPath[c.Path] = c
	}
	if len(byPath) != 3 {
		t.Fatalf("changes = %+v", changes)
	}

	modified, ok := byPath["README.md"]
	if !ok || modified.BeforeHash != hashString("hello\n") || modified.AfterHash != hashString("hello\nworld\n") || modified.Deleted {
		t.Fatalf("README.md change = %+v", modified)
	}

	added, ok := byPath["docs/new.md"]
	if !ok || added.BeforeHash != "" || added.AfterHash != hashString("fresh\n") || added.Deleted {
		t.Fatalf("docs/new.md change = %+v", added)
	}

	deleted, ok := byPath["docs/guide.md"]
	if !ok || deleted.BeforeHash != hashString("guide\n") || deleted.AfterHash != "" || !deleted.Deleted {
		t.Fatalf("docs/guide.md change = %+v", deleted)
	}
}

func TestViolatesScope(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		scopes  []string
		outside bool
	}{
		{"inside directory scope", "docs/a.md", []string{"docs"}, false},
		{"nested inside directory scope", "docs/sub/deep.md", []string{"docs"}, false},
		{"sibling directory sharing a prefix is outside", "docs2/a.md", []string{"docs"}, true},
		{"exact file scope matches", "README.md", []string{"README.md"}, false},
		{"file scope does not match unrelated file sharing a prefix", "README.md.bak", []string{"README.md"}, true},
		{"absolute path always violates regardless of scope", "/etc/passwd", []string{"etc"}, true},
		{"dot-dot segment always violates regardless of scope", "docs/../secret.txt", []string{"docs"}, true},
		{"no scopes means everything is outside", "docs/a.md", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ViolatesScope([]Change{{Path: tt.path}}, tt.scopes)
			isViolation := len(got) == 1
			if isViolation != tt.outside {
				t.Fatalf("ViolatesScope(%q, %v) = %v, want outside=%v", tt.path, tt.scopes, got, tt.outside)
			}
		})
	}
}

func TestRemoveDeletesWorktreeAndLeavesCanonicalIntact(t *testing.T) {
	requireGit(t)
	repoDir := testRepo(t)
	m := newManager(t)

	lease, err := m.Create(context.Background(), repoDir, "attempt")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := m.Remove(context.Background(), lease); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(lease.Path); !os.IsNotExist(err) {
		t.Fatalf("worktree still present after Remove, stat err = %v", err)
	}
	if status := gitStatus(t, repoDir); status != "" {
		t.Fatalf("canonical repo dirtied: %q", status)
	}
	if _, err := os.Stat(filepath.Join(repoDir, "README.md")); err != nil {
		t.Fatalf("canonical README.md missing: %v", err)
	}
}

// TestRemoveRefusesLeasePathOutsideManagedRoot is the most important test in
// this file: it proves a doctored Lease cannot turn Remove into an arbitrary
// filesystem delete.
func TestRemoveRefusesLeasePathOutsideManagedRoot(t *testing.T) {
	requireGit(t)
	repoDir := testRepo(t)
	m := newManager(t)

	lease, err := m.Create(context.Background(), repoDir, "attempt")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatalf("mkdir victim: %v", err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keepme.txt"), []byte("do not delete\n"), 0o600); err != nil {
		t.Fatalf("write victim file: %v", err)
	}

	doctored := lease
	doctored.Path = victim
	if err := m.Remove(context.Background(), doctored); err == nil {
		t.Fatal("Remove accepted a Lease.Path outside the managed root")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("victim directory did not survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(victim, "keepme.txt")); err != nil {
		t.Fatalf("victim file did not survive: %v", err)
	}

	// The rejected doctored call must not have corrupted anything: the real
	// worktree is still there and still removable through the real lease.
	if err := m.Remove(context.Background(), lease); err != nil {
		t.Fatalf("Remove of the real lease after a rejected doctored attempt: %v", err)
	}
}

func TestRemoveRefusesPathInsideRootThatGitDoesNotKnowAbout(t *testing.T) {
	requireGit(t)
	repoDir := testRepo(t)
	root := filepath.Join(t.TempDir(), "worktrees")
	m, err := New(Options{Root: root})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	bogus := filepath.Join(root, "not-a-real-worktree")
	if err := os.MkdirAll(bogus, 0o700); err != nil {
		t.Fatalf("mkdir bogus: %v", err)
	}
	lease := Lease{ID: "not-a-real-worktree", Path: bogus, Repository: repoDir}
	if err := m.Remove(context.Background(), lease); err == nil {
		t.Fatal("Remove accepted a path inside Root that git does not know about")
	}
	if _, err := os.Stat(bogus); err != nil {
		t.Fatalf("bogus directory did not survive: %v", err)
	}
}

func TestReconcileRemovesUnreferencedWorktreesButKeepsListed(t *testing.T) {
	requireGit(t)
	repoDir := testRepo(t)
	m := newManager(t)

	keepLease, err := m.Create(context.Background(), repoDir, "keep")
	if err != nil {
		t.Fatalf("Create keep: %v", err)
	}
	dropLease, err := m.Create(context.Background(), repoDir, "drop")
	if err != nil {
		t.Fatalf("Create drop: %v", err)
	}
	resolvedDropPath, err := filepath.EvalSymlinks(dropLease.Path)
	if err != nil {
		t.Fatalf("resolve drop path: %v", err)
	}

	removed, err := m.Reconcile(context.Background(), map[string]bool{keepLease.ID: true})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := os.Stat(keepLease.Path); err != nil {
		t.Fatalf("kept worktree missing: %v", err)
	}
	if _, err := os.Stat(dropLease.Path); !os.IsNotExist(err) {
		t.Fatalf("dropped worktree still present, stat err = %v", err)
	}

	// The dropped worktree's administrative entry must also be gone from
	// git's perspective, or a later `git worktree add` in this repository
	// could collide with it.
	registered, err := registeredWorktrees(context.Background(), repoDir)
	if err != nil {
		t.Fatalf("registeredWorktrees: %v", err)
	}
	if registered[resolvedDropPath] {
		t.Fatalf("git still lists removed worktree %q", resolvedDropPath)
	}
}

func TestConcurrentCreateForSameRepositoryProducesDistinctWorktrees(t *testing.T) {
	requireGit(t)
	repoDir := testRepo(t)
	m := newManager(t)

	const n = 8
	leases := make([]Lease, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			lease, err := m.Create(context.Background(), repoDir, fmt.Sprintf("attempt-%d", i))
			leases[i] = lease
			errs[i] = err
		}(i)
	}
	wg.Wait()

	seenPaths := map[string]bool{}
	seenIDs := map[string]bool{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Create[%d]: %v", i, err)
		}
		if seenPaths[leases[i].Path] {
			t.Fatalf("duplicate worktree path %q", leases[i].Path)
		}
		if seenIDs[leases[i].ID] {
			t.Fatalf("duplicate worktree id %q", leases[i].ID)
		}
		seenPaths[leases[i].Path] = true
		seenIDs[leases[i].ID] = true
	}
	if len(seenPaths) != n {
		t.Fatalf("expected %d distinct worktree paths, got %d", n, len(seenPaths))
	}
}
