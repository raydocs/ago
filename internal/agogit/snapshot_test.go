package agogit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func repo(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	git(t, d, "init", "-q")
	git(t, d, "config", "user.email", "test@example.invalid")
	git(t, d, "config", "user.name", "Test")
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("one\n"), 0644)
	git(t, d, "add", ".")
	git(t, d, "commit", "-qm", "base")
	return d
}

func bind(t *testing.T, d string) *Binding {
	t.Helper()
	b, err := Bind(context.Background(), d, ExecutorIdentity{Generation: "g1", Environment: "local"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCleanAndStableSnapshot(t *testing.T) {
	d := repo(t)
	b := bind(t, d)
	a, err := b.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := b.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Staged) != 0 || len(a.Unstaged) != 0 {
		t.Fatalf("not clean: %+v", a)
	}
	if a.Digest == "" || a.Digest != c.Digest {
		t.Fatalf("unstable digest %q %q", a.Digest, c.Digest)
	}
}

func TestUnbornHeadSnapshot(t *testing.T) {
	d := t.TempDir()
	git(t, d, "init", "-q")
	os.WriteFile(filepath.Join(d, "new.txt"), []byte("new\n"), 0644)
	snapshot, err := bind(t, d).Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.HeadOID != "unborn" || len(snapshot.Unstaged) != 1 {
		t.Fatalf("unexpected unborn snapshot: %+v", snapshot)
	}
}

func TestUntrackedSnapshotRetainsCompleteAddPatch(t *testing.T) {
	d := repo(t)
	if err := os.WriteFile(filepath.Join(d, "new.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := bind(t, d).Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var added FileChange
	for _, change := range snapshot.Unstaged {
		if change.Path == "new.txt" {
			added = change
		}
	}
	if added.Status != StatusAdded || len(added.Patch) == 0 || len(added.Hunks) != 1 {
		t.Fatalf("untracked mutation artifact = %+v", added)
	}
	if !bytes.Contains(added.Patch, []byte("--- /dev/null\n")) || !bytes.Contains(added.Patch, []byte("+++ b/new.txt\n")) {
		t.Fatalf("untracked patch is not a complete add: %q", added.Patch)
	}
}

func TestDigestTracksContentWithSameDiffShape(t *testing.T) {
	d := repo(t)
	b := bind(t, d)
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("two\n"), 0644)
	a, err := b.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("six\n"), 0644)
	c, err := b.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest == c.Digest {
		t.Fatalf("digest ignored changed content: %s", a.Digest)
	}
}

func TestDigestBindsHeadAndIndexEvenWhenWorktreeIsClean(t *testing.T) {
	d := repo(t)
	b := bind(t, d)
	first, err := b.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(d, "second.txt"), []byte("second\n"), 0644)
	git(t, d, "add", "second.txt")
	git(t, d, "commit", "-qm", "second")
	second, err := b.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.HeadOID == second.HeadOID || first.IndexDigest == second.IndexDigest || first.Digest == second.Digest {
		t.Fatalf("snapshot identity did not track clean commit: first=%+v second=%+v", first, second)
	}
}

func TestDigestTracksSerializedIndexFlags(t *testing.T) {
	d := repo(t)
	b := bind(t, d)
	before, err := b.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	git(t, d, "update-index", "--assume-unchanged", "same.txt")
	after, err := b.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if before.IndexDigest != after.IndexDigest {
		t.Fatalf("semantic index unexpectedly changed: %q != %q", before.IndexDigest, after.IndexDigest)
	}
	if before.Digest == after.Digest {
		t.Fatalf("snapshot ignored serialized index flag change: %q", before.Digest)
	}
}

func TestDuplicateHunkHeadersHaveDistinctOpaqueIDs(t *testing.T) {
	d := repo(t)
	original := "a\nsame\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nsame\nm\n"
	os.WriteFile(filepath.Join(d, "duplicates.txt"), []byte(original), 0644)
	git(t, d, "add", "duplicates.txt")
	git(t, d, "commit", "-qm", "duplicates")
	os.WriteFile(filepath.Join(d, "duplicates.txt"), []byte("a\nchanged\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nchanged\nm\n"), 0644)
	snapshot, err := bind(t, d).Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var hunks []Hunk
	for _, file := range snapshot.Unstaged {
		if file.Path == "duplicates.txt" {
			hunks = file.Hunks
		}
	}
	if len(hunks) != 2 || hunks[0].ID == hunks[1].ID {
		t.Fatalf("hunks are not independently addressable: %+v", hunks)
	}
}

func TestSnapshotRetainsCompleteContextPatchesForServerMutation(t *testing.T) {
	d := repo(t)
	lines := make([]byte, 0)
	for i := 0; i < 20; i++ {
		lines = append(lines, []byte(fmt.Sprintf("line-%02d\n", i))...)
	}
	if err := os.WriteFile(filepath.Join(d, "patch.txt"), lines, 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, d, "add", "patch.txt")
	git(t, d, "commit", "-qm", "patch fixture")
	changed := bytes.Replace(lines, []byte("line-00\n"), []byte("first\n"), 1)
	changed = bytes.Replace(changed, []byte("line-19\n"), []byte("last\n"), 1)
	if err := os.WriteFile(filepath.Join(d, "patch.txt"), changed, 0o644); err != nil {
		t.Fatal(err)
	}

	snapshot, err := bind(t, d).Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Unstaged) != 1 {
		t.Fatalf("unstaged = %+v", snapshot.Unstaged)
	}
	file := snapshot.Unstaged[0]
	if len(file.Patch) == 0 || len(file.Hunks) != 2 {
		t.Fatalf("private mutation material missing: %+v", file)
	}
	if len(completeHunkPatches(file.Patch)) != 2 {
		t.Fatalf("whole patch does not contain complete hunks: %q", file.Patch)
	}
	for _, hunk := range file.Hunks {
		if len(hunk.Patch) == 0 || len(completeHunkPatches(hunk.Patch)) != 1 {
			t.Fatalf("hunk patch is not independently applicable: %+v", hunk)
		}
		if !bytes.Contains(hunk.Patch, []byte(" line-")) {
			t.Fatalf("hunk patch has no normal context: %q", hunk.Patch)
		}
	}
}

func TestOpaqueIDsDistinguishStagedAndUnstagedVersions(t *testing.T) {
	d := repo(t)
	b := bind(t, d)
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("staged\n"), 0644)
	git(t, d, "add", "same.txt")
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("unstaged\n"), 0644)

	snapshot, err := b.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Staged) != 1 || len(snapshot.Unstaged) != 1 {
		t.Fatalf("unexpected changes: staged=%+v unstaged=%+v", snapshot.Staged, snapshot.Unstaged)
	}
	staged, unstaged := snapshot.Staged[0], snapshot.Unstaged[0]
	if staged.ID == unstaged.ID {
		t.Fatalf("file ID aliases staged and unstaged versions: %q", staged.ID)
	}
	if len(staged.Hunks) != 1 || len(unstaged.Hunks) != 1 {
		t.Fatalf("unexpected hunks: staged=%+v unstaged=%+v", staged.Hunks, unstaged.Hunks)
	}
	if staged.Hunks[0].ID == unstaged.Hunks[0].ID {
		t.Fatalf("hunk ID aliases staged and unstaged versions: %q", staged.Hunks[0].ID)
	}
}

func TestSnapshotKindsAndProtectedPaths(t *testing.T) {
	d := repo(t)
	b := bind(t, d)
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("two\n"), 0644)
	git(t, d, "add", "same.txt")
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("three\n"), 0644)
	os.WriteFile(filepath.Join(d, "new.txt"), []byte("new\n"), 0644)
	os.MkdirAll(filepath.Join(d, "thread-app", "src"), 0755)
	os.WriteFile(filepath.Join(d, "thread-app", "src", "index.ts"), []byte("x"), 0644)
	s, err := b.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Staged) != 1 {
		t.Fatalf("staged=%+v", s.Staged)
	}
	if len(s.Unstaged) != 3 {
		t.Fatalf("unstaged=%+v", s.Unstaged)
	}
	var protected bool
	for _, f := range s.Unstaged {
		if f.Path == "thread-app/src/index.ts" {
			protected = f.Protected
			if f.MutationSupported {
				t.Fatal("protected mutation supported")
			}
		}
	}
	if !protected {
		t.Fatal("protected path not marked")
	}
}

func TestSnapshotBindsAffectedWorktreeBytesWithoutFollowingSymlinks(t *testing.T) {
	d := repo(t)
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0o644)
	snapshot, err := bind(t, d).Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Unstaged) != 1 || len(snapshot.Unstaged[0].Worktree) != 1 {
		t.Fatalf("worktree identities = %+v", snapshot.Unstaged)
	}
	identity := snapshot.Unstaged[0].Worktree[0]
	if identity.Path != "same.txt" || identity.Kind != "regular" || identity.Digest == "" || identity.Size != int64(len("changed\n")) {
		t.Fatalf("worktree identity = %+v", identity)
	}

	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(d, "link")); err != nil {
		t.Fatal(err)
	}
	snapshot, err = bind(t, d).Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range snapshot.Unstaged {
		if change.Path == "link" {
			if len(change.Worktree) != 1 || change.Worktree[0].Kind != "symlink" || change.Worktree[0].Size == int64(len("secret")) {
				t.Fatalf("symlink identity followed external target: %+v", change.Worktree)
			}
		}
	}
}

func TestAddDeleteRenameAndBinary(t *testing.T) {
	d := repo(t)
	os.WriteFile(filepath.Join(d, "delete.txt"), []byte("d\n"), 0644)
	os.WriteFile(filepath.Join(d, "rename.txt"), []byte("r\n"), 0644)
	git(t, d, "add", ".")
	git(t, d, "commit", "-qm", "fixtures")
	os.Remove(filepath.Join(d, "delete.txt"))
	git(t, d, "mv", "rename.txt", "renamed.txt")
	os.WriteFile(filepath.Join(d, "add.txt"), []byte("a\n"), 0644)
	os.WriteFile(filepath.Join(d, "binary.dat"), []byte{0, 1, 2}, 0644)
	git(t, d, "add", "-A")
	s, err := bind(t, d).Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[Status]bool{}
	binary := false
	for _, f := range s.Staged {
		seen[f.Status] = true
		binary = binary || f.Binary
	}
	if !seen[StatusAdded] || !seen[StatusDeleted] || !seen[StatusRenamed] || !binary {
		t.Fatalf("files=%+v", s.Staged)
	}
}

func TestCanonicalWorkspaceAndLinkedWorktreeIdentity(t *testing.T) {
	d := repo(t)
	link := filepath.Join(t.TempDir(), "repo")
	if err := os.Symlink(d, link); err != nil {
		t.Fatal(err)
	}
	b := bind(t, link)
	real, _ := filepath.EvalSymlinks(d)
	if b.Workspace != real {
		t.Fatalf("workspace=%q", b.Workspace)
	}
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, d, "worktree", "add", "-q", wt)
	b2 := bind(t, wt)
	if b.GitDir == b2.GitDir || b.CommonGitDir != b2.CommonGitDir {
		t.Fatalf("bad worktree identities: %+v %+v", b, b2)
	}
}

func TestIdentityReplacementRejected(t *testing.T) {
	d := repo(t)
	b := bind(t, d)
	old := d + "-old"
	if err := os.Rename(d, old); err != nil {
		t.Fatal(err)
	}
	os.Mkdir(d, 0755)
	_, err := b.Snapshot(context.Background(), SnapshotOptions{})
	if !errors.Is(err, ErrIdentityChanged) {
		t.Fatalf("err=%v", err)
	}
}

func TestMaliciousExternalDiffDoesNotRun(t *testing.T) {
	d := repo(t)
	marker := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "evil")
	os.WriteFile(script, []byte("#!/bin/sh\ntouch \"$MARKER\"\n"), 0755)
	git(t, d, "config", "diff.external", script)
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0644)
	os.Setenv("MARKER", marker)
	t.Cleanup(func() { os.Unsetenv("MARKER") })
	if _, err := bind(t, d).Snapshot(context.Background(), SnapshotOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("external diff executed")
	}
}

func TestChangeDuringScanReturnsUnstable(t *testing.T) {
	d := repo(t)
	b := bind(t, d)
	_, err := b.Snapshot(context.Background(), SnapshotOptions{AfterCapture: func(n int) {
		if n == 1 {
			os.WriteFile(filepath.Join(d, "new.txt"), []byte("changed"), 0644)
		}
	}, MaxCaptures: 2})
	if !errors.Is(err, ErrUnstable) {
		t.Fatalf("err=%v", err)
	}
}

func TestUntrackedSymlinkFailsClosedForMutation(t *testing.T) {
	d := repo(t)
	if err := os.Symlink("same.txt", filepath.Join(d, "link")); err != nil {
		t.Fatal(err)
	}
	s, err := bind(t, d).Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range s.Unstaged {
		if f.Path == "link" && (f.NewMode != "120000" || f.MutationSupported) {
			t.Fatalf("unsafe symlink: %+v", f)
		}
	}
}
