package agoserve_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"claudexflow/internal/agoboardstore"
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

// claimAndCreate is how Ago itself establishes ownership: claim the directory,
// then create each of its own directories through the call that records having
// created them. Tests go through the same sequence, because a marker alone no
// longer authorises deleting anything.
func claimAndCreate(t *testing.T, state string, owned ...string) string {
	t.Helper()
	resolved, err := agoserve.ClaimState(state)
	if err != nil {
		t.Fatalf("ClaimState(%s): %v", state, err)
	}
	for _, name := range owned {
		created, err := agoserve.CreateOwnedDirectory(resolved, name)
		if err != nil {
			t.Fatalf("CreateOwnedDirectory(%s): %v", name, err)
		}
		if !created {
			t.Fatalf("CreateOwnedDirectory(%s) did not create it", name)
		}
	}
	return resolved
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
		claimAndCreate(t, state, "artifacts")
		plantSentinel(t, state)
		// A sibling of the real state directory, to prove the delete did not
		// wander up through the resolved parent.
		sibling := filepath.Join(real, "inner", "not-ago")
		plantSentinel(t, sibling)
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
	resolved := claimAndCreate(t, state, "artifacts")
	// Ago's real directory is swapped for a link pointing at the user's data.
	if err := os.RemoveAll(filepath.Join(resolved, "artifacts")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(resolved, "artifacts")); err != nil {
		t.Fatal(err)
	}

	if err := agoserve.ResetState(state, home); err != nil {
		t.Fatalf("ResetState: %v", err)
	}
	// The link carries no sentinel, so it is not Ago's and is left alone —
	// and above all what it points at is untouched.
	assertSentinelIntact(t, outside)
	if _, err := os.Lstat(filepath.Join(outside, sentinelName)); err != nil {
		t.Fatalf("the link was followed and the target emptied: %v", err)
	}
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
	claimAndCreate(t, state, "greeter", "artifacts", "worktrees", "integration")
	// Things the user put there afterwards, which Ago never recorded.
	plantSentinel(t, state)
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
	if _, err := agoserve.ClaimState(state); err != nil {
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

// The hole, in both the shapes it took.
//
// Ownership was first granted by writing a marker whenever the sample
// repository was absent — true of any directory --state can name. The second
// version narrowed that to directories whose entries all carried Ago's names,
// which is the same hole with a longer condition: a directory holding nothing
// but artifacts/report.pdf is an ordinary project directory. A name is not
// provenance.
//
// Every case below stands on its own. None of them contains a file outside
// Ago's own names, because leaning on such a file is exactly what made the
// earlier test insufficient — it passed while the hole was open.
func TestClaimRefusesEveryNonEmptyDirectoryWithoutAMarker(t *testing.T) {
	// A real SQLite header, so no case can be dismissed as "that was not a
	// plausible database anyway". Being a genuine database file must not
	// authorise anything either; provenance is not a file format.
	sqliteHeader := append([]byte("SQLite format 3\x00"), make([]byte, 84)...)

	cases := map[string]map[string][]byte{
		"artifacts holding a report":  {"artifacts/report.pdf": []byte("%PDF-1.7 user data\n")},
		"integration holding a patch": {"integration/user.patch": []byte("--- a\n+++ b\n")},
		"worktrees holding a note":    {"worktrees/note.txt": []byte("my notes\n")},
		"greeter holding a readme":    {"greeter/README.md": []byte("# my project\n")},
		"a plain file named ago.db":   {"ago.db": []byte("not actually a database\n")},
		"a real SQLite file":          {"ago.db": sqliteHeader},
		"a database and an artifacts": {"ago.db": sqliteHeader, "artifacts/report.pdf": []byte("user data\n")},
		"every reserved name at once": {
			"ago.db": sqliteHeader, "artifacts/a": []byte("a\n"), "integration/b": []byte("b\n"),
			"worktrees/c": []byte("c\n"), "greeter/d": []byte("d\n"),
		},
	}

	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "myproject")
			for relative, content := range files {
				path := filepath.Join(state, filepath.FromSlash(relative))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotTree(t, state)

			if _, err := agoserve.ClaimState(state); err == nil {
				t.Fatal("Ago claimed a directory it did not create")
			}
			if agoserve.OwnsState(state) {
				t.Fatal("a refused claim still marked the directory as Ago's")
			}
			if _, err := os.Lstat(filepath.Join(state, ".ago-demo-state")); err == nil {
				t.Fatal("a refused claim wrote an ownership marker")
			}
			// With no marker, reset cannot run either — now or ever.
			if err := agoserve.ResetState(state, t.TempDir()); err == nil {
				t.Fatal("reset ran on a directory Ago never claimed")
			}
			assertTreeUnchanged(t, state, before)
		})
	}
}

// snapshotTree records every path under root with its contents, so a test can
// assert that nothing at all changed rather than that one sentinel survived.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			tree[relative+"/"] = ""
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[relative] = string(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func assertTreeUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := snapshotTree(t, root)
	for path, content := range before {
		got, present := after[path]
		if !present {
			t.Errorf("%s was removed", path)
			continue
		}
		if got != content {
			t.Errorf("%s was modified: %q", path, got)
		}
	}
	for path := range after {
		if _, present := before[path]; !present {
			t.Errorf("%s was created in a directory Ago was refused", path)
		}
	}
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

// The v3 hole, found by a review that reproduced real data loss.
//
// A claim was correct at the moment it was made and then never re-decided.
// Point --state at a directory before it exists or while it is empty, let
// something else fill it, and reset removed what that something else made.
// This is the sharp version, because it needs no cleanup step at all:
//
//	cd ~/myrepo
//	ago demo --state ./build      # build/ absent -> created and claimed
//	make                          # fills build/artifacts with object files
//	ago demo --state ./build --reset
func TestResetLeavesEntriesAgoDidNotCreateEvenUnderItsOwnNames(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "build")

	// The claiming run: Ago creates the directory and its own artifacts.
	resolved := claimAndCreate(t, state, "artifacts")

	// Ago's own artifacts go away, and something else takes the name. This is
	// a different object: a new directory, a new inode.
	if err := os.RemoveAll(filepath.Join(resolved, "artifacts")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(resolved, "artifacts"), 0o700); err != nil {
		t.Fatal(err)
	}
	thesis := filepath.Join(resolved, "artifacts", "thesis.pdf")
	if err := os.WriteFile(thesis, []byte("IRREPLACEABLE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// And a name Ago never created at all.
	if err := os.MkdirAll(filepath.Join(resolved, "integration"), 0o700); err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(resolved, "integration", "prod.patch")
	if err := os.WriteFile(patch, []byte("--- a\n+++ b\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := agoserve.ResetState(state, home); err != nil {
		t.Fatalf("ResetState: %v", err)
	}
	for _, path := range []string{thesis, patch} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("reset destroyed something Ago never created: %s (%v)", path, err)
		}
	}
}

// The authority does not travel. A marker records the directory it was written
// for, by path and by device and inode, so a copy, a move, or a restore from a
// backup is not the thing that was claimed.
func TestOwnershipDoesNotSurviveBeingMovedOrCopied(t *testing.T) {
	home := t.TempDir()

	t.Run("moved", func(t *testing.T) {
		root := t.TempDir()
		state := filepath.Join(root, "demo")
		claimAndCreate(t, state, "artifacts")
		moved := filepath.Join(root, "myproject")
		if err := os.Rename(state, moved); err != nil {
			t.Fatal(err)
		}
		// The user repurposes it.
		mine := filepath.Join(moved, "artifacts", "user.pdf")
		if err := os.WriteFile(mine, []byte("mine\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if agoserve.OwnsState(moved) {
			t.Error("a moved directory still claims to be Ago's")
		}
		if err := agoserve.ResetState(moved, home); err == nil {
			t.Fatal("reset ran on a directory the marker no longer describes")
		}
		if _, err := os.Stat(mine); err != nil {
			t.Errorf("the user's file was destroyed: %v", err)
		}
	})

	t.Run("copied", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "demo")
		claimAndCreate(t, source)
		// A copy: same marker bytes, different directory.
		destination := filepath.Join(t.TempDir(), "myproject")
		if err := os.MkdirAll(destination, 0o700); err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(filepath.Join(source, ".ago-demo-state"))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, ".ago-demo-state"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		mine := filepath.Join(destination, "artifacts", "user.pdf")
		if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(mine, []byte("mine\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		if agoserve.OwnsState(destination) {
			t.Error("a copied marker claims a directory it was not written for")
		}
		if err := agoserve.ResetState(destination, home); err == nil {
			t.Fatal("a copied marker authorised a reset")
		}
		if _, err := os.Stat(mine); err != nil {
			t.Errorf("the user's file was destroyed: %v", err)
		}
	})
}

// A claim must land in the directory the user named. Claiming through a
// symlink is how the claiming side and the reset side came to disagree about
// which directory they were discussing.
func TestClaimRefusesASymlinkedState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	target := filepath.Join(t.TempDir(), "real")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := agoserve.CanClaim(link)
	if err == nil {
		t.Fatal("a symlinked --state was claimable")
	}
	if !strings.Contains(err.Error(), "符号链接") {
		t.Fatalf("the refusal does not name the reason: %v", err)
	}
	if _, err := agoserve.ClaimState(link); err == nil {
		t.Fatal("ClaimState followed a symlinked --state")
	}
	if _, err := os.Lstat(filepath.Join(target, ".ago-demo-state")); err == nil {
		t.Fatal("a marker was planted in a directory the user never named")
	}
}

// The shallow-path deny-list is a backstop for a mistake in the marker logic,
// so it has to be exercised on paths that EXIST and by the message it emits.
// A version of this test that passed a non-existent path proved nothing: the
// refusal came from "that directory does not exist", and the rule it was
// supposed to cover could be deleted with every test still green.
func TestHighRiskPathRulesAreLoadBearing(t *testing.T) {
	home := t.TempDir()

	t.Run("the filesystem root", func(t *testing.T) {
		_, err := agoserve.CheckResetAllowed("/", home)
		if err == nil {
			t.Fatal("reset was allowed on the filesystem root")
		}
		// Both refusals mention 根目录, so the assertion has to name the one
		// under test. Matching the shared substring is how this test passed
		// while the rule it covers could be deleted.
		if !strings.Contains(err.Error(), "文件系统根目录") {
			t.Fatalf("the refusal came from some other rule: %v", err)
		}
	})

	t.Run("an existing top-level directory", func(t *testing.T) {
		// Something one segment below the root that really exists, so the
		// depth rule is what has to refuse it.
		var shallow string
		for _, candidate := range []string{"/Users", "/home", "/usr", "/var", "/tmp"} {
			if info, err := os.Lstat(candidate); err == nil && info.IsDir() {
				shallow = candidate
				break
			}
		}
		if shallow == "" {
			t.Skip("no one-segment directory to test with")
		}
		_, err := agoserve.CheckResetAllowed(shallow, home)
		if err == nil {
			t.Fatalf("reset was allowed on %s", shallow)
		}
		if !strings.Contains(err.Error(), "太靠近根目录") {
			t.Fatalf("the refusal came from some other rule: %v", err)
		}
	})
}

// The residual sharp edge, stated as a test so nobody has to discover it.
//
// A directory Ago created belongs to Ago as a whole. A file the user drops
// INSIDE artifacts/ goes when artifacts/ goes. What survives is anything at the
// top level of the state directory that Ago never created — that is the
// boundary, and it is worth being explicit about which side of it a file is on.
func TestResetRemovesAgosOwnDirectoriesWholeButNeverTouchesTheTopLevel(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "demo")
	resolved := claimAndCreate(t, state, "artifacts")
	// Inside a directory Ago created: goes with it.
	inside := filepath.Join(resolved, "artifacts", "my-file.txt")
	if err := os.WriteFile(inside, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// At the top level, under a name Ago does not use: survives.
	plantSentinel(t, resolved)

	if err := agoserve.ResetState(state, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(inside); err == nil {
		t.Error("a file inside Ago's own directory survived, so the reset was incomplete")
	}
	assertSentinelIntact(t, resolved)
}

// A crash between writing the marker's temporary file and renaming it into
// place must not strand the directory. Without this, the leftover made the
// directory non-empty and unmarked — permanently unclaimable, and the user
// would have to find and delete a hidden file they never knew existed.
func TestAnInterruptedClaimDoesNotStrandTheDirectory(t *testing.T) {
	state := filepath.Join(t.TempDir(), "demo")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	debris := filepath.Join(state, ".ago-demo-state-1234567")
	if err := os.WriteFile(debris, []byte("half a marker"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := agoserve.CanClaim(state); err != nil {
		t.Fatalf("a directory holding only Ago's own debris was refused: %v", err)
	}
	resolved, err := agoserve.ClaimState(state)
	if err != nil {
		t.Fatalf("ClaimState: %v", err)
	}
	if !agoserve.OwnsState(resolved) {
		t.Fatal("the claim did not take")
	}
	if _, err := os.Lstat(debris); !os.IsNotExist(err) {
		t.Errorf("the debris was left behind: %v", err)
	}
}

// But debris does not make a directory full of a user's work claimable.
func TestDebrisDoesNotExcuseAUsersContents(t *testing.T) {
	state := filepath.Join(t.TempDir(), "myproject")
	plantSentinel(t, state)
	if err := os.WriteFile(filepath.Join(state, ".ago-demo-state-1234567"), []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := agoserve.CanClaim(state); err == nil {
		t.Fatal("debris made a directory full of a user's work claimable")
	}
	assertSentinelIntact(t, state)
}

// The binding is checked field by field, and each field has to matter on its
// own. A marker that gets the directory right but the magic wrong is still not
// Ago's — otherwise anything that happened to serialise a path and an inode
// into that filename would authorise a delete.
func TestAMarkerWithTheRightBindingButTheWrongMagicIsRefused(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "demo")
	// A genuine claim first, so path and inode are exactly right.
	resolved := claimAndCreate(t, state, "artifacts")
	path := filepath.Join(resolved, ".ago-demo-state")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["magic"] = "some-other-tool-v1"
	rewritten, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}

	if agoserve.OwnsState(resolved) {
		t.Fatal("a marker with the wrong magic still claimed the directory")
	}
	if err := agoserve.ResetState(state, home); err == nil {
		t.Fatal("a marker with the wrong magic authorised a reset")
	}
	if _, err := os.Stat(filepath.Join(resolved, "artifacts")); err != nil {
		t.Errorf("the refused reset still deleted something: %v", err)
	}
}

// Reset only ever removes names Ago itself uses. The recorded entries are the
// authority, but the reserved list is the ceiling: a marker that names
// something else — hand-edited, or written by a future version with a
// different set — cannot be used to delete it.
func TestResetIgnoresRecordedEntriesOutsideItsOwnNames(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "demo")
	resolved := claimAndCreate(t, state)

	// The user's own directory, and a marker that has been edited to name it.
	notes := filepath.Join(resolved, "my-notes")
	if err := os.MkdirAll(notes, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(notes, "keep.txt"), []byte("keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(notes)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no filesystem identity on this platform")
	}
	path := filepath.Join(resolved, ".ago-demo-state")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(content, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["entries"] = map[string]any{
		"my-notes": map[string]any{"device": uint64(stat.Dev), "inode": uint64(stat.Ino)},
	}
	rewritten, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rewritten, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := agoserve.ResetState(state, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(notes, "keep.txt")); err != nil {
		t.Errorf("reset removed a name that is not one of Ago's: %v", err)
	}
}

// Attribution had no coverage at all, which is how the previous version's
// central mechanism could be deleted with the whole suite green.
//
// The scenario every earlier test missed: a user's own directory under one of
// Ago's names, ALREADY PRESENT when a later run starts. Ago must never come to
// own it — not by finding it, not by re-recording it, not ever.
func TestAUsersDirectoryPresentAtStartupNeverBecomesAgos(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "demo")
	resolved := claimAndCreate(t, state, "artifacts")

	// Ago's own goes away and the user's takes the name, before a later run.
	if err := os.RemoveAll(filepath.Join(resolved, "artifacts")); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(resolved, "artifacts", "thesis.pdf")
	if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("IRREPLACEABLE\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A later run: Ago tries to create its directories again and finds one
	// there. It must not adopt it.
	created, err := agoserve.CreateOwnedDirectory(resolved, "artifacts")
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("CreateOwnedDirectory claimed to have created an existing directory")
	}
	if _, err := os.Lstat(filepath.Join(resolved, "artifacts", ".ago-created")); err == nil {
		t.Fatal("a directory Ago did not create was marked as its own")
	}

	if err := agoserve.ResetState(state, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("reset destroyed a directory Ago never created: %v", err)
	}
}

// Inode reuse must not be able to restore that data loss. On a filesystem that
// reissues inode numbers — ext4 does, straight after a delete in the same block
// group — the previous version would delete a user's directory that merely
// inherited the number. Provenance is a nonce Ago wrote, which a filesystem
// does not hand out, so the same-inode case is simulated directly.
func TestASameInodeDirectoryWithoutTheSentinelIsNotAgos(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "demo")
	resolved := claimAndCreate(t, state, "artifacts")

	// Keep Ago's directory — same inode, same name — but remove the record of
	// who made it. That is exactly what an inode the kernel reissued looks
	// like to any identity-based check.
	if err := os.Remove(filepath.Join(resolved, "artifacts", ".ago-created")); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(resolved, "artifacts", "thesis.pdf")
	if err := os.WriteFile(mine, []byte("IRREPLACEABLE\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := agoserve.ResetState(state, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("a directory with no proof of authorship was deleted: %v", err)
	}
}

// The board database is identified by its contents, not its name.
func TestOnlyAgosOwnDatabaseIsRemoved(t *testing.T) {
	home := t.TempDir()

	t.Run("a user's file called ago.db", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "demo")
		resolved := claimAndCreate(t, state)
		// A real SQLite file, but not Ago's board.
		content := append([]byte("SQLite format 3\x00"), make([]byte, 4096)...)
		copy(content[100:], []byte("CREATE TABLE my_own_table (id INTEGER)"))
		if err := os.WriteFile(filepath.Join(resolved, "ago.db"), content, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := agoserve.ResetState(state, home); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(resolved, "ago.db")); err != nil {
			t.Fatalf("a database that is not Ago's board was removed: %v", err)
		}
	})

	t.Run("Ago's own board", func(t *testing.T) {
		state := filepath.Join(t.TempDir(), "demo")
		resolved := claimAndCreate(t, state)
		store, err := agoboardstore.Open(filepath.Join(resolved, "ago.db"))
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if err := agoserve.ResetState(state, home); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(resolved, "ago.db")); !os.IsNotExist(err) {
			t.Fatalf("Ago's own board database survived a reset: %v", err)
		}
	})
}

// The directory binding exists for a case the move/copy tests never reach: a
// directory deleted and restored to the SAME path, which a backup restore or
// `cp -a` back into place produces. Same path, different inode.
func TestOwnershipDoesNotSurviveBeingRestoredToTheSamePath(t *testing.T) {
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "demo")
	resolved := claimAndCreate(t, state, "artifacts")
	content, err := os.ReadFile(filepath.Join(resolved, ".ago-demo-state"))
	if err != nil {
		t.Fatal(err)
	}

	// The directory is destroyed and recreated at the same path, and the
	// marker is put back exactly as it was — a restore.
	if err := os.RemoveAll(resolved); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(resolved, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resolved, ".ago-demo-state"), content, 0o600); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(resolved, "artifacts", "user.pdf")
	if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if agoserve.OwnsState(resolved) {
		t.Error("a restored marker still claims a directory that is not the one it described")
	}
	if err := agoserve.ResetState(state, home); err == nil {
		t.Fatal("a restored marker authorised a reset")
	}
	if _, err := os.Stat(mine); err != nil {
		t.Errorf("the user's file was destroyed: %v", err)
	}
}

// The sentinel has to be a real file in the directory it speaks for. A symlink
// pointing at a file that happens to hold the right nonce would let a
// directory vouch for itself with someone else's evidence — the same trick the
// marker's own Lstat check refuses.
func TestASymlinkedSentinelDoesNotAuthoriseDeletion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "demo")
	resolved := claimAndCreate(t, state, "artifacts")

	// Take the real nonce, then rebuild artifacts/ as a directory that is not
	// Ago's but whose sentinel is a link to a file holding that nonce.
	nonce, err := os.ReadFile(filepath.Join(resolved, "artifacts", ".ago-created"))
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := filepath.Join(t.TempDir(), "borrowed-nonce")
	if err := os.WriteFile(elsewhere, nonce, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(resolved, "artifacts")); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(resolved, "artifacts", "thesis.pdf")
	if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("IRREPLACEABLE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(resolved, "artifacts", ".ago-created")); err != nil {
		t.Fatal(err)
	}

	if err := agoserve.ResetState(state, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("a symlinked sentinel authorised deleting a directory Ago never created: %v", err)
	}
}

// The nonce is the whole point of the sentinel: it ties a directory to ONE
// claim. A sentinel carrying a different claim's nonce — copied along with the
// directory, restored from a backup, or written by hand — is evidence about
// some other directory and authorises nothing here.
func TestASentinelFromAnotherClaimAuthorisesNothing(t *testing.T) {
	home := t.TempDir()

	// One demo, whose artifacts directory Ago genuinely created.
	source := filepath.Join(t.TempDir(), "first")
	sourceResolved := claimAndCreate(t, source, "artifacts")
	borrowed, err := os.ReadFile(filepath.Join(sourceResolved, "artifacts", ".ago-created"))
	if err != nil {
		t.Fatal(err)
	}

	// A second demo, with its own claim and therefore its own nonce. The
	// user's directory here carries the FIRST demo's sentinel.
	state := filepath.Join(t.TempDir(), "second")
	resolved := claimAndCreate(t, state)
	mine := filepath.Join(resolved, "artifacts", "thesis.pdf")
	if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("IRREPLACEABLE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resolved, "artifacts", ".ago-created"), borrowed, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := agoserve.ResetState(state, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("a sentinel from another claim authorised deleting a directory Ago never created: %v", err)
	}
	// And a hand-written one is no better.
	if err := os.WriteFile(filepath.Join(resolved, "artifacts", ".ago-created"), []byte("guessed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := agoserve.ResetState(state, home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("a forged sentinel authorised deleting a directory Ago never created: %v", err)
	}
}

// ResetOwnedDirectory exists so an interrupted creation can be finished. It
// empties a directory, so it must refuse anything Ago cannot prove it made —
// otherwise "finish the half-built repository" becomes "empty whatever is at
// that path".
func TestResetOwnedDirectoryRefusesWhatAgoDidNotCreate(t *testing.T) {
	state := filepath.Join(t.TempDir(), "demo")
	resolved := claimAndCreate(t, state)

	// A directory at one of Ago's names that Ago did not create.
	mine := filepath.Join(resolved, "greeter", "my-work.txt")
	if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := agoserve.ResetOwnedDirectory(resolved, "greeter")
	if err == nil {
		t.Fatal("ResetOwnedDirectory emptied a directory Ago did not create")
	}
	if !strings.Contains(err.Error(), "无法证明") {
		t.Fatalf("the refusal does not explain itself: %v", err)
	}
	if _, statErr := os.Stat(mine); statErr != nil {
		t.Fatalf("the refused call still destroyed the contents: %v", statErr)
	}
}
