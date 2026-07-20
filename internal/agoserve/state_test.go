package agoserve_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"claudexflow/internal/agoserve"
)

// The thing being defended against: `demo --state <anything> --reset` used to
// call os.RemoveAll on a user-supplied path. Every test here places a hostile
// sentinel — a file Ago did not create and must never remove — and asserts it
// is byte-identical afterwards.

const sentinelName = "IMPORTANT-USER-FILE.txt"
const sentinelBody = "这是用户自己的文件，Ago 绝对不能删除它。\n"

func plantSentinel(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sentinelName), []byte(sentinelBody), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertSentinelIntact(t *testing.T, dir string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(dir, sentinelName))
	if err != nil {
		t.Fatalf("the sentinel was destroyed: %v", err)
	}
	if string(content) != sentinelBody {
		t.Fatalf("the sentinel was modified: %q", content)
	}
}

// An ordinary directory a user pointed --state at is not Ago's to empty, no
// matter what is in it.
func TestResetRefusesADirectoryAgoDidNotCreate(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "my-important-work")
	plantSentinel(t, state)
	// Something that looks like demo state, to prove the refusal is about
	// ownership rather than about the contents.
	if err := os.WriteFile(filepath.Join(state, "ago.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := agoserve.ResetState(state, home)
	if err == nil {
		t.Fatal("reset emptied a directory Ago never created")
	}
	if !strings.Contains(err.Error(), "归属标记") {
		t.Fatalf("the refusal does not explain ownership: %v", err)
	}
	assertSentinelIntact(t, state)
	if _, err := os.Stat(filepath.Join(state, "ago.db")); err != nil {
		t.Fatalf("a refused reset still deleted something: %v", err)
	}
}

// A file named like the marker is not a marker. Ownership is decided by
// contents, so a user cannot accidentally authorise a delete by creating an
// empty file with the right name.
func TestResetRefusesAForgedMarker(t *testing.T) {
	home := t.TempDir()
	for name, content := range map[string]string{
		"empty":          "",
		"wrong magic":    `{"magic":"something-else"}`,
		"not json":       "ago-demo-state-v1",
		"json but plain": `{"created_at":"2026-01-01T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			plantSentinel(t, state)
			if err := os.WriteFile(filepath.Join(state, ".ago-demo-state"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := agoserve.ResetState(state, home); err == nil {
				t.Fatal("a forged marker authorised a reset")
			}
			assertSentinelIntact(t, state)
		})
	}
}

// A symlink named like the marker cannot vouch for a directory by pointing at a
// real marker somewhere else.
func TestResetRefusesASymlinkedMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	home := t.TempDir()
	real := t.TempDir()
	if err := agoserve.WriteMarker(real); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(t.TempDir(), "state")
	plantSentinel(t, state)
	if err := os.Symlink(filepath.Join(real, ".ago-demo-state"), filepath.Join(state, ".ago-demo-state")); err != nil {
		t.Fatal(err)
	}
	if err := agoserve.ResetState(state, home); err == nil {
		t.Fatal("a symlinked marker authorised a reset")
	}
	assertSentinelIntact(t, state)
}

// A symlinked --state is a mismatch between what the user named and what would
// be deleted, so it is refused. A symlinked ancestor is not a mismatch, so it
// is resolved — and the delete must still land only inside the real directory.
func TestResetRefusesASymlinkedPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	home := t.TempDir()

	t.Run("state itself is a link", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "real")
		plantSentinel(t, target)
		if err := agoserve.WriteMarker(target); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := agoserve.ResetState(link, home); err == nil {
			t.Fatal("reset followed a symlinked --state")
		}
		assertSentinelIntact(t, target)
	})

	// A symlinked ANCESTOR is resolved, not refused — refusing it would refuse
	// most real machines, since /var is a link to /private/var on macOS and a
	// home directory is often reached through one. What must hold is that the
	// delete lands inside the real directory and nowhere else.
	t.Run("a parent is a link", func(t *testing.T) {
		real := t.TempDir()
		state := filepath.Join(real, "inner", "state")
		plantSentinel(t, state)
		if err := os.MkdirAll(filepath.Join(state, "artifacts"), 0o700); err != nil {
			t.Fatal(err)
		}
		// A sibling of the real state directory, to prove the delete did not
		// wander up through the resolved parent.
		sibling := filepath.Join(real, "inner", "not-ago")
		plantSentinel(t, sibling)
		if err := agoserve.WriteMarker(state); err != nil {
			t.Fatal(err)
		}
		linkedParent := filepath.Join(t.TempDir(), "parent-link")
		if err := os.Symlink(filepath.Join(real, "inner"), linkedParent); err != nil {
			t.Fatal(err)
		}
		if err := agoserve.ResetState(filepath.Join(linkedParent, "state"), home); err != nil {
			t.Fatalf("ResetState through a symlinked parent: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(state, "artifacts")); !os.IsNotExist(err) {
			t.Errorf("the reset did not reach the real directory: %v", err)
		}
		assertSentinelIntact(t, state)
		assertSentinelIntact(t, sibling)
	})
}

// A symlink at one of the entries Ago removes is unlinked, never followed. A
// hostile link cannot turn "delete artifacts" into "delete the user's photos".
func TestResetUnlinksRatherThanFollowingAHostileEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	home := t.TempDir()
	outside := t.TempDir()
	plantSentinel(t, outside)

	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := agoserve.WriteMarker(state); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(state, "artifacts")); err != nil {
		t.Fatal(err)
	}

	if err := agoserve.ResetState(state, home); err != nil {
		t.Fatalf("ResetState: %v", err)
	}
	// The link is gone; what it pointed at is untouched.
	if _, err := os.Lstat(filepath.Join(state, "artifacts")); !os.IsNotExist(err) {
		t.Fatalf("the hostile link survived: %v", err)
	}
	assertSentinelIntact(t, outside)
}

// Locations that are never a demo directory are refused even if a marker was
// copied into them.
func TestResetRefusesHighRiskLocations(t *testing.T) {
	t.Run("home itself", func(t *testing.T) {
		home := t.TempDir()
		plantSentinel(t, home)
		if err := agoserve.WriteMarker(home); err != nil {
			t.Fatal(err)
		}
		err := agoserve.ResetState(home, home)
		if err == nil {
			t.Fatal("reset targeted the user's home directory")
		}
		if !strings.Contains(err.Error(), "主目录") {
			t.Fatalf("the refusal does not name the reason: %v", err)
		}
		assertSentinelIntact(t, home)
	})

	t.Run("an ancestor of home", func(t *testing.T) {
		parent := t.TempDir()
		home := filepath.Join(parent, "user")
		plantSentinel(t, parent)
		if err := os.MkdirAll(home, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := agoserve.WriteMarker(parent); err != nil {
			t.Fatal(err)
		}
		if err := agoserve.ResetState(parent, home); err == nil {
			t.Fatal("reset targeted a directory containing the user's home")
		}
		assertSentinelIntact(t, parent)
	})

	t.Run("a git repository", func(t *testing.T) {
		home := t.TempDir()
		repo := filepath.Join(t.TempDir(), "checkout")
		plantSentinel(t, repo)
		if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := agoserve.WriteMarker(repo); err != nil {
			t.Fatal(err)
		}
		err := agoserve.ResetState(repo, home)
		if err == nil {
			t.Fatal("reset targeted a git repository")
		}
		if !strings.Contains(err.Error(), "git") {
			t.Fatalf("the refusal does not name the reason: %v", err)
		}
		assertSentinelIntact(t, repo)
	})

	t.Run("the filesystem root", func(t *testing.T) {
		if err := agoserve.ResetState("/", t.TempDir()); err == nil {
			t.Fatal("reset targeted the filesystem root")
		}
	})

	t.Run("a top-level directory", func(t *testing.T) {
		// One segment deep is never a demo directory. This is checked without
		// creating it: the path does not exist, and the refusal must not
		// depend on Ago having looked inside it.
		if err := agoserve.ResetState("/ago-demo-state-test", t.TempDir()); err == nil {
			t.Fatal("reset targeted a top-level directory")
		}
	})

	t.Run("nothing at all", func(t *testing.T) {
		if err := agoserve.ResetState("", t.TempDir()); err == nil {
			t.Fatal("reset accepted an empty path")
		}
	})
}

// The success path removes what Ago made and nothing else. A user who kept a
// note in the demo directory keeps the note.
func TestResetRemovesOnlyAgoOwnedEntries(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "demo")
	plantSentinel(t, state)
	if err := agoserve.WriteMarker(state); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"ago.db", "ago.db-wal"} {
		if err := os.WriteFile(filepath.Join(state, entry), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []string{"greeter", "artifacts", "worktrees", "integration"} {
		if err := os.MkdirAll(filepath.Join(state, entry, "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A user's own subdirectory, which is not on Ago's list.
	if err := os.MkdirAll(filepath.Join(state, "my-notes"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := agoserve.ResetState(state, home); err != nil {
		t.Fatalf("ResetState: %v", err)
	}
	for _, entry := range []string{"ago.db", "ago.db-wal", "greeter", "artifacts", "worktrees", "integration"} {
		if _, err := os.Lstat(filepath.Join(state, entry)); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset", entry)
		}
	}
	assertSentinelIntact(t, state)
	if _, err := os.Stat(filepath.Join(state, "my-notes")); err != nil {
		t.Errorf("a user's own directory was removed: %v", err)
	}
	// The directory is still Ago's: it was before the reset, and re-deciding
	// that on the next run would refuse a directory that also holds a user's
	// file — turning --reset into a way to make the demo unusable.
	if !agoserve.OwnsState(state) {
		t.Error("a reset gave away ownership of a directory Ago owns")
	}
	if err := agoserve.ClaimState(state); err != nil {
		t.Errorf("the directory could not be reused after a reset: %v", err)
	}
}

// OwnsState is the predicate the demo uses, so it is checked directly too.
func TestOwnsStateOnlyAfterAgoWroteTheMarker(t *testing.T) {
	state := t.TempDir()
	if agoserve.OwnsState(state) {
		t.Fatal("an untouched directory claims to be Ago's")
	}
	if err := agoserve.WriteMarker(state); err != nil {
		t.Fatal(err)
	}
	if !agoserve.OwnsState(state) {
		t.Fatal("a directory Ago marked is not recognised")
	}
}

// The hole the first version of this fix left open.
//
// Ownership was granted by writing a marker whenever the sample repository was
// absent — which is true of any directory a user points --state at. Two
// ordinary commands then destroyed their data:
//
//	ago demo --state ~/myproject          # plants a marker
//	ago demo --state ~/myproject --reset  # deletes myproject/artifacts
//
// The names Ago removes are ordinary directory names. Narrowing the delete to
// them is not enough; the authorisation itself has to be earned.
func TestClaimRefusesADirectoryWithSomeoneElsesContents(t *testing.T) {
	state := filepath.Join(t.TempDir(), "myproject")
	plantSentinel(t, state)
	// The names that made this dangerous: ordinary words a real project uses.
	for _, entry := range []string{"artifacts", "integration"} {
		if err := os.MkdirAll(filepath.Join(state, entry), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(state, "artifacts", "report.pdf"), []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := agoserve.ClaimState(state)
	if err == nil {
		t.Fatal("Ago claimed a directory full of someone else's work")
	}
	if agoserve.OwnsState(state) {
		t.Fatal("a refused claim still wrote the ownership marker")
	}
	// And with no marker, reset cannot touch it.
	if err := agoserve.ResetState(state, t.TempDir()); err == nil {
		t.Fatal("reset ran on a directory Ago failed to claim")
	}
	if _, err := os.Stat(filepath.Join(state, "artifacts", "report.pdf")); err != nil {
		t.Fatalf("the user's file was destroyed: %v", err)
	}
	assertSentinelIntact(t, state)
}

func TestClaimAcceptsOnlyDirectoriesAgoCouldHaveMade(t *testing.T) {
	t.Run("a path that does not exist yet", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "new", "demo")
		if err := agoserve.ClaimState(state); err != nil {
			t.Fatalf("ClaimState: %v", err)
		}
		if !agoserve.OwnsState(state) {
			t.Fatal("a directory Ago created is not claimed")
		}
	})

	t.Run("an empty directory", func(t *testing.T) {
		state := t.TempDir()
		if err := agoserve.ClaimState(state); err != nil {
			t.Fatalf("ClaimState: %v", err)
		}
		if !agoserve.OwnsState(state) {
			t.Fatal("an empty directory was not claimed")
		}
	})

	// The upgrade path: state written by a build from before markers existed.
	// Its contents are recognisably Ago's, so it is adopted rather than left
	// permanently un-resettable.
	t.Run("a directory holding only Ago's own entries", func(t *testing.T) {
		state := t.TempDir()
		if err := os.MkdirAll(filepath.Join(state, "greeter"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "ago.db"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := agoserve.ClaimState(state); err != nil {
			t.Fatalf("ClaimState on pre-marker state: %v", err)
		}
		if !agoserve.OwnsState(state) {
			t.Fatal("pre-marker demo state was not adopted")
		}
	})

	t.Run("a directory Ago already owns", func(t *testing.T) {
		state := t.TempDir()
		if err := agoserve.ClaimState(state); err != nil {
			t.Fatal(err)
		}
		// A user's own file appearing later must not un-claim it, and must not
		// be at risk either: reset only removes Ago's own entries.
		plantSentinel(t, state)
		if err := agoserve.ClaimState(state); err != nil {
			t.Fatalf("a directory Ago owns was not re-claimable: %v", err)
		}
		if err := agoserve.ResetState(state, t.TempDir()); err != nil {
			t.Fatal(err)
		}
		assertSentinelIntact(t, state)
	})

	t.Run("blank", func(t *testing.T) {
		for _, state := range []string{"", "   "} {
			if err := agoserve.ClaimState(state); err == nil {
				t.Fatalf("ClaimState accepted %q", state)
			}
		}
	})
}

// A git worktree and a submodule have a .git FILE, not a directory. Reading
// only directories let the deny-list miss both.
func TestResetRefusesAGitWorktreeWhoseDotGitIsAFile(t *testing.T) {
	home := t.TempDir()
	worktree := filepath.Join(t.TempDir(), "wt")
	plantSentinel(t, worktree)
	if err := os.WriteFile(filepath.Join(worktree, ".git"),
		[]byte("gitdir: /somewhere/.git/worktrees/wt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agoserve.WriteMarker(worktree); err != nil {
		t.Fatal(err)
	}
	err := agoserve.ResetState(worktree, home)
	if err == nil {
		t.Fatal("reset targeted a git worktree")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Fatalf("the refusal does not name the reason: %v", err)
	}
	assertSentinelIntact(t, worktree)
}
