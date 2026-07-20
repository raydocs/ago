package agoworktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// An attempt must leave nothing durable in the user's repository. A branch
// created by `worktree add -b` survives `worktree remove`, so every attempt
// would permanently pollute the ref namespace.
func TestAttemptsLeaveNoRefsInTheCanonicalRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		command := exec.Command("git", args...)
		command.Dir = repo
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-b", "main")
	run("config", "user.email", "u@e.com")
	run("config", "user.name", "U")
	run("add", "-A")
	run("commit", "-m", "init")

	before := run("for-each-ref", "--format=%(refname)")
	manager, err := New(Options{Root: filepath.Join(base, "worktrees")})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		lease, err := manager.Create(context.Background(), repo, "attempt-"+id)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.Remove(context.Background(), lease); err != nil {
			t.Fatal(err)
		}
	}
	after := run("for-each-ref", "--format=%(refname)")
	if after != before {
		t.Fatalf("attempts changed the repository's refs:\nbefore %q\nafter  %q", before, after)
	}
	if strings.Contains(after, "ago/") {
		t.Fatalf("attempts left branches behind: %q", after)
	}
	if listing := run("worktree", "list"); strings.Count(listing, "\n") != 0 {
		t.Fatalf("worktrees survived: %q", listing)
	}
}
