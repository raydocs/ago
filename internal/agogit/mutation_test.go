package agogit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func mutationSnapshot(t *testing.T, b *Binding) *Snapshot {
	t.Helper()
	s, err := b.Snapshot(context.Background(), SnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func findMutationChange(t *testing.T, changes []FileChange, path string) FileChange {
	t.Helper()
	for _, change := range changes {
		if change.Path == path {
			return change
		}
	}
	t.Fatalf("change %q not found in %+v", path, changes)
	return FileChange{}
}

func TestIndexMutationStagesOneOfTwoHunksWithoutTouchingWorktree(t *testing.T) {
	d := repo(t)
	original := []byte("00\n01\n02\n03\n04\n05\n06\n07\n08\n09\n10\n11\n12\n13\n14\n15\n16\n17\n18\n19\n")
	if err := os.WriteFile(filepath.Join(d, "two.txt"), original, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, d, "add", "two.txt")
	git(t, d, "commit", "-qm", "two hunks")
	changed := bytes.Replace(original, []byte("01\n"), []byte("first\n"), 1)
	changed = bytes.Replace(changed, []byte("18\n"), []byte("second\n"), 1)
	if err := os.WriteFile(filepath.Join(d, "two.txt"), changed, 0o755); err != nil {
		t.Fatal(err)
	}
	b := bind(t, d)
	before := mutationSnapshot(t, b)
	change := findMutationChange(t, before.Unstaged, "two.txt")
	if len(change.Hunks) != 2 {
		t.Fatalf("hunks = %+v", change.Hunks)
	}
	plan, err := b.PlanIndexMutation(context.Background(), before, MutationStage, []string{change.Hunks[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Before != before.SerializedIndex || !plan.Intended.Exists || len(plan.AffectedWorktree) != 1 {
		t.Fatalf("plan identities = %+v", plan)
	}
	if err := b.PublishIndexMutation(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	after := mutationSnapshot(t, b)
	if after.SerializedIndex != plan.Intended {
		t.Fatalf("post-publication snapshot index = %+v, intended = %+v", after.SerializedIndex, plan.Intended)
	}
	staged := findMutationChange(t, after.Staged, "two.txt")
	unstaged := findMutationChange(t, after.Unstaged, "two.txt")
	if len(staged.Hunks) != 1 || len(unstaged.Hunks) != 1 {
		t.Fatalf("staged=%+v unstaged=%+v", staged.Hunks, unstaged.Hunks)
	}
	data, infoErr := os.Stat(filepath.Join(d, "two.txt"))
	if infoErr != nil || data.Mode().Perm() != 0o755 {
		t.Fatalf("worktree mode changed: %v %v", data, infoErr)
	}
	got, err := os.ReadFile(filepath.Join(d, "two.txt"))
	if err != nil || !bytes.Equal(got, changed) {
		t.Fatalf("worktree bytes changed: %v %q", err, got)
	}
}

func TestIndexMutationFileIDStagesAllTextHunks(t *testing.T) {
	d := repo(t)
	original := []byte("00\n01\n02\n03\n04\n05\n06\n07\n08\n09\n10\n11\n12\n13\n14\n15\n16\n17\n18\n19\n")
	os.WriteFile(filepath.Join(d, "two.txt"), original, 0o644)
	git(t, d, "add", "two.txt")
	git(t, d, "commit", "-qm", "two hunks")
	changed := bytes.Replace(original, []byte("01\n"), []byte("first\n"), 1)
	changed = bytes.Replace(changed, []byte("18\n"), []byte("second\n"), 1)
	os.WriteFile(filepath.Join(d, "two.txt"), changed, 0o644)
	b := bind(t, d)
	before := mutationSnapshot(t, b)
	change := findMutationChange(t, before.Unstaged, "two.txt")
	if len(change.Hunks) != 2 {
		t.Fatalf("hunks = %+v", change.Hunks)
	}
	plan, err := b.PlanIndexMutation(context.Background(), before, MutationStage, []string{change.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PublishIndexMutation(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	after := mutationSnapshot(t, b)
	if len(findMutationChange(t, after.Staged, "two.txt").Hunks) != 2 || len(after.Unstaged) != 0 {
		t.Fatalf("file selection did not stage every hunk: %+v", after)
	}
}

func TestIndexMutationUnstagesOneHunkAndPublishesExactIntendedIndex(t *testing.T) {
	d := repo(t)
	original := []byte("00\n01\n02\n03\n04\n05\n06\n07\n08\n09\n10\n11\n12\n13\n14\n15\n16\n17\n18\n19\n")
	os.WriteFile(filepath.Join(d, "two.txt"), original, 0o644)
	git(t, d, "add", "two.txt")
	git(t, d, "commit", "-qm", "two hunks")
	changed := bytes.Replace(original, []byte("01\n"), []byte("first\n"), 1)
	changed = bytes.Replace(changed, []byte("18\n"), []byte("second\n"), 1)
	os.WriteFile(filepath.Join(d, "two.txt"), changed, 0o644)
	git(t, d, "add", "two.txt")
	b := bind(t, d)
	before := mutationSnapshot(t, b)
	change := findMutationChange(t, before.Staged, "two.txt")
	plan, err := b.PlanIndexMutation(context.Background(), before, MutationUnstage, []string{change.Hunks[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	intended := plan.Intended
	if err := b.PublishIndexMutation(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	actual, err := readSerializedIndex(filepath.Join(b.GitDir, "index"))
	if err != nil || actual != intended {
		t.Fatalf("published index = %+v, intended = %+v, err = %v", actual, intended, err)
	}
	after := mutationSnapshot(t, b)
	if len(findMutationChange(t, after.Staged, "two.txt").Hunks) != 1 || len(findMutationChange(t, after.Unstaged, "two.txt").Hunks) != 1 {
		t.Fatalf("unexpected partial unstage: %+v", after)
	}
}

func TestIndexMutationRejectsInvalidSelections(t *testing.T) {
	d := repo(t)
	os.WriteFile(filepath.Join(d, "type.txt"), []byte("regular\n"), 0o644)
	git(t, d, "add", "type.txt")
	git(t, d, "commit", "-qm", "type fixture")
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0o644)
	os.MkdirAll(filepath.Join(d, "thread-app", "src"), 0o755)
	os.WriteFile(filepath.Join(d, "thread-app", "src", "index.ts"), []byte("protected\n"), 0o644)
	os.Symlink("same.txt", filepath.Join(d, "link"))
	os.Remove(filepath.Join(d, "type.txt"))
	os.Symlink("same.txt", filepath.Join(d, "type.txt"))
	b := bind(t, d)
	s := mutationSnapshot(t, b)
	regular := findMutationChange(t, s.Unstaged, "same.txt")
	protected := findMutationChange(t, s.Unstaged, "thread-app/src/index.ts")
	symlink := findMutationChange(t, s.Unstaged, "link")
	typeChange := findMutationChange(t, s.Unstaged, "type.txt")
	tests := []struct {
		name string
		kind MutationKind
		ids  []string
	}{
		{"unknown", MutationStage, []string{"unknown"}},
		{"duplicate", MutationStage, []string{regular.ID, regular.ID}},
		{"wrong side", MutationUnstage, []string{regular.ID}},
		{"mixed whole and hunk", MutationStage, []string{regular.ID, regular.Hunks[0].ID}},
		{"protected", MutationStage, []string{protected.ID}},
		{"symlink", MutationStage, []string{symlink.ID}},
		{"type change", MutationStage, []string{typeChange.ID}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := b.PlanIndexMutation(context.Background(), s, tt.kind, tt.ids); !errors.Is(err, ErrInvalidMutationSelection) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestIndexMutationExistingLockAndStaleInputsNeverPublish(t *testing.T) {
	t.Run("existing lock", func(t *testing.T) {
		d := repo(t)
		os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0o644)
		b := bind(t, d)
		s := mutationSnapshot(t, b)
		plan, err := b.PlanIndexMutation(context.Background(), s, MutationStage, []string{s.Unstaged[0].ID})
		if err != nil {
			t.Fatal(err)
		}
		indexBefore, _ := os.ReadFile(filepath.Join(b.GitDir, "index"))
		lockPath := filepath.Join(b.GitDir, "index.lock")
		sentinel := []byte("do-not-touch")
		os.WriteFile(lockPath, sentinel, 0o600)
		if err := b.PublishIndexMutation(context.Background(), plan); !errors.Is(err, ErrIndexMutationConflict) {
			t.Fatalf("err = %v", err)
		}
		lockAfter, _ := os.ReadFile(lockPath)
		indexAfter, _ := os.ReadFile(filepath.Join(b.GitDir, "index"))
		if !bytes.Equal(lockAfter, sentinel) || !bytes.Equal(indexAfter, indexBefore) {
			t.Fatal("existing lock or index changed")
		}
	})

	for _, stale := range []string{"head", "index", "worktree"} {
		t.Run("stale "+stale, func(t *testing.T) {
			d := repo(t)
			os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0o644)
			b := bind(t, d)
			s := mutationSnapshot(t, b)
			plan, err := b.PlanIndexMutation(context.Background(), s, MutationStage, []string{s.Unstaged[0].ID})
			if err != nil {
				t.Fatal(err)
			}
			switch stale {
			case "head":
				git(t, d, "commit", "--allow-empty", "-qm", "advance")
			case "index":
				git(t, d, "update-index", "--assume-unchanged", "same.txt")
			case "worktree":
				os.WriteFile(filepath.Join(d, "same.txt"), []byte("different\n"), 0o644)
			}
			indexBefore, _ := os.ReadFile(filepath.Join(b.GitDir, "index"))
			if err := b.PublishIndexMutation(context.Background(), plan); !errors.Is(err, ErrIndexMutationConflict) {
				t.Fatalf("err = %v", err)
			}
			indexAfter, _ := os.ReadFile(filepath.Join(b.GitDir, "index"))
			if !bytes.Equal(indexAfter, indexBefore) {
				t.Fatal("stale plan published")
			}
		})
	}
}

func TestIndexMutationPlanIsSingleUse(t *testing.T) {
	d := repo(t)
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0o644)
	b := bind(t, d)
	s := mutationSnapshot(t, b)
	plan, err := b.PlanIndexMutation(context.Background(), s, MutationStage, []string{s.Unstaged[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	planCopy := *plan
	if err := b.PublishIndexMutation(context.Background(), &planCopy); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishIndexMutation(context.Background(), plan); !errors.Is(err, ErrIndexMutationPlanUsed) {
		t.Fatalf("second publication err = %v", err)
	}
}

func TestIndexMutationRequestNeedsOnlySnapshotDigestAndUnitIDs(t *testing.T) {
	d := repo(t)
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0o644)
	b := bind(t, d)
	current := mutationSnapshot(t, b)
	requestIdentity := &Snapshot{Digest: current.Digest}
	plan, err := b.PlanIndexMutation(context.Background(), requestIdentity, MutationStage, []string{current.Unstaged[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PublishIndexMutation(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
}

func TestIndexMutationStagesWholeEntities(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		setup func(*testing.T, string)
	}{
		{"binary", "same.txt", func(t *testing.T, d string) { os.WriteFile(filepath.Join(d, "same.txt"), []byte{0, 1, 2, 3}, 0o644) }},
		{"delete", "same.txt", func(t *testing.T, d string) { os.Remove(filepath.Join(d, "same.txt")) }},
		{"untracked text add", "new.txt", func(t *testing.T, d string) { os.WriteFile(filepath.Join(d, "new.txt"), []byte("new\n"), 0o644) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := repo(t)
			tt.setup(t, d)
			b := bind(t, d)
			before := mutationSnapshot(t, b)
			change := findMutationChange(t, before.Unstaged, tt.path)
			plan, err := b.PlanIndexMutation(context.Background(), before, MutationStage, []string{change.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := b.PublishIndexMutation(context.Background(), plan); err != nil {
				t.Fatal(err)
			}
			after := mutationSnapshot(t, b)
			staged := findMutationChange(t, after.Staged, tt.path)
			if staged.Status != change.Status || len(after.Unstaged) != 0 {
				t.Fatalf("whole entity not staged: before=%+v after=%+v", change, after)
			}
		})
	}
}

func TestIndexMutationUnstagesWholeRename(t *testing.T) {
	d := repo(t)
	git(t, d, "mv", "same.txt", "renamed.txt")
	b := bind(t, d)
	before := mutationSnapshot(t, b)
	renamed := findMutationChange(t, before.Staged, "renamed.txt")
	if renamed.Status != StatusRenamed || renamed.OldPath != "same.txt" {
		t.Fatalf("fixture is not a staged rename: %+v", renamed)
	}
	plan, err := b.PlanIndexMutation(context.Background(), before, MutationUnstage, []string{renamed.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PublishIndexMutation(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	after := mutationSnapshot(t, b)
	if len(after.Staged) != 0 {
		t.Fatalf("rename remained staged: %+v", after.Staged)
	}
	indexListing, err := runGit(context.Background(), d, "ls-files")
	if err != nil || string(indexListing) != "same.txt\n" {
		t.Fatalf("rename index was not reversed: %q %v", indexListing, err)
	}
}

func TestIndexMutationUnstagesWholeAddedEntity(t *testing.T) {
	d := repo(t)
	os.WriteFile(filepath.Join(d, "new.txt"), []byte("new\n"), 0o644)
	git(t, d, "add", "new.txt")
	b := bind(t, d)
	before := mutationSnapshot(t, b)
	added := findMutationChange(t, before.Staged, "new.txt")
	plan, err := b.PlanIndexMutation(context.Background(), before, MutationUnstage, []string{added.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PublishIndexMutation(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	after := mutationSnapshot(t, b)
	if len(after.Staged) != 0 || findMutationChange(t, after.Unstaged, "new.txt").Status != StatusAdded {
		t.Fatalf("added entity not unstaged: %+v", after)
	}
}

func TestIndexMutationSupportsUnbornHeadAndAbsentIndex(t *testing.T) {
	d := t.TempDir()
	git(t, d, "init", "-q")
	os.WriteFile(filepath.Join(d, "new.txt"), []byte("new\n"), 0o644)
	b := bind(t, d)
	before := mutationSnapshot(t, b)
	if before.HeadOID != "unborn" || before.SerializedIndex.Exists {
		t.Fatalf("fixture is not unborn with absent index: %+v", before)
	}
	plan, err := b.PlanIndexMutation(context.Background(), before, MutationStage, []string{before.Unstaged[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PublishIndexMutation(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	after := mutationSnapshot(t, b)
	if after.HeadOID != "unborn" || !after.SerializedIndex.Exists || len(after.Staged) != 1 {
		t.Fatalf("unborn publication failed: %+v", after)
	}
}

func TestIndexMutationFailsClosedOnIndexSymlink(t *testing.T) {
	d := repo(t)
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0o644)
	b := bind(t, d)
	before := mutationSnapshot(t, b)
	indexPath := filepath.Join(b.GitDir, "index")
	realIndex := filepath.Join(b.GitDir, "saved-index")
	if err := os.Rename(indexPath, realIndex); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realIndex, indexPath); err != nil {
		t.Fatal(err)
	}
	if _, err := b.PlanIndexMutation(context.Background(), before, MutationStage, []string{before.Unstaged[0].ID}); err == nil {
		t.Fatal("index symlink accepted")
	}
}

func TestIndexMutationIgnoresMutablePublicPlanProjection(t *testing.T) {
	d := repo(t)
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0o644)
	b := bind(t, d)
	before := mutationSnapshot(t, b)
	plan, err := b.PlanIndexMutation(context.Background(), before, MutationStage, []string{before.Unstaged[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	intended := plan.Intended
	plan.Before = SerializedIndexIdentity{}
	plan.Intended = SerializedIndexIdentity{}
	plan.AffectedWorktree = nil
	if err := b.PublishIndexMutation(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	actual, err := readSerializedIndex(filepath.Join(b.GitDir, "index"))
	if err != nil || actual != intended {
		t.Fatalf("private plan identity was not authoritative: %+v %v", actual, err)
	}
}

func TestIndexMutationGitInvocationHasNoForbiddenFlagsOrSelectedPath(t *testing.T) {
	d := repo(t)
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0o644)
	b := bind(t, d)
	before := mutationSnapshot(t, b)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "argv")
	script := "#!/bin/sh\nprintf 'literal=%s\\n' \"$GIT_LITERAL_PATHSPECS\" >> " + logPath + "\nprintf '%s\\n' \"$@\" >> " + logPath + "\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	plan, err := b.PlanIndexMutation(context.Background(), before, MutationStage, []string{before.Unstaged[0].Hunks[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PublishIndexMutation(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	argv := string(logged)
	for _, forbidden := range []string{"--3way", "--reject", "--unidiff-zero"} {
		if strings.Contains(argv, forbidden) {
			t.Fatalf("forbidden unsafe argument %q in:\n%s", forbidden, argv)
		}
	}
	for _, invocation := range strings.Split(argv, "literal=") {
		if strings.HasPrefix(invocation, "1\n") && strings.Contains(invocation, "same.txt") {
			t.Fatalf("selected path reached alternate-index argv:\n%s", invocation)
		}
	}
	if !strings.Contains(argv, "apply\n--cached\n") || !strings.Contains(argv, "literal=1") {
		t.Fatalf("expected cached apply invocation, got:\n%s", argv)
	}
}

func TestIndexMutationRejectsUnmergedAndSubmoduleEntities(t *testing.T) {
	t.Run("unmerged", func(t *testing.T) {
		d := repo(t)
		git(t, d, "checkout", "-qb", "other")
		os.WriteFile(filepath.Join(d, "same.txt"), []byte("other\n"), 0o644)
		git(t, d, "commit", "-qam", "other")
		git(t, d, "checkout", "-q", "master")
		os.WriteFile(filepath.Join(d, "same.txt"), []byte("master\n"), 0o644)
		git(t, d, "commit", "-qam", "master")
		cmd := exec.Command("git", "-C", d, "merge", "other")
		if err := cmd.Run(); err == nil {
			t.Fatal("fixture merge unexpectedly succeeded")
		}
		b := bind(t, d)
		s := mutationSnapshot(t, b)
		change := findMutationChange(t, s.Unstaged, "same.txt")
		if change.Status != StatusUnmerged {
			t.Fatalf("fixture is not unmerged: %+v", change)
		}
		if _, err := b.PlanIndexMutation(context.Background(), s, MutationStage, []string{change.ID}); !errors.Is(err, ErrInvalidMutationSelection) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("submodule", func(t *testing.T) {
		d := repo(t)
		head, err := runGit(context.Background(), d, "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		git(t, d, "update-index", "--add", "--cacheinfo", "160000,"+strings.TrimSpace(string(head))+",sub")
		b := bind(t, d)
		s := mutationSnapshot(t, b)
		change := findMutationChange(t, s.Staged, "sub")
		if change.NewMode != "160000" {
			t.Fatalf("fixture is not a submodule: %+v", change)
		}
		if _, err := b.PlanIndexMutation(context.Background(), s, MutationUnstage, []string{change.ID}); !errors.Is(err, ErrInvalidMutationSelection) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestIndexMutationRejectsChangedAlternateBytes(t *testing.T) {
	d := repo(t)
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0o644)
	b := bind(t, d)
	s := mutationSnapshot(t, b)
	plan, err := b.PlanIndexMutation(context.Background(), s, MutationStage, []string{s.Unstaged[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	indexBefore, _ := os.ReadFile(filepath.Join(b.GitDir, "index"))
	if err := os.WriteFile(plan.alternatePath, []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishIndexMutation(context.Background(), plan); !errors.Is(err, ErrIndexMutationConflict) {
		t.Fatalf("err = %v", err)
	}
	indexAfter, _ := os.ReadFile(filepath.Join(b.GitDir, "index"))
	if !bytes.Equal(indexAfter, indexBefore) {
		t.Fatal("tampered alternate published")
	}
}

func TestIndexMutationPublishesStandaloneIndexFromSplitIndex(t *testing.T) {
	d := repo(t)
	git(t, d, "update-index", "--split-index")
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0o644)
	b := bind(t, d)
	s := mutationSnapshot(t, b)
	plan, err := b.PlanIndexMutation(context.Background(), s, MutationStage, []string{s.Unstaged[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	shared, err := filepath.Glob(filepath.Join(b.GitDir, "sharedindex.*"))
	if err != nil || len(shared) == 0 {
		t.Fatalf("split-index fixture missing: %v %v", shared, err)
	}
	for _, path := range shared {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.PublishIndexMutation(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	listing, err := runGit(context.Background(), d, "ls-files")
	if err != nil || string(listing) != "same.txt\n" {
		t.Fatalf("published index still depends on shared index: %q %v", listing, err)
	}
}

func TestIndexMutationDoesNotRunConfiguredFilters(t *testing.T) {
	d := repo(t)
	marker := filepath.Join(t.TempDir(), "filter-ran")
	script := filepath.Join(t.TempDir(), "filter")
	filterScript := "#!/bin/sh\ncase \"$GIT_DIR\" in *'.agogit-mutation-'*'/repo') touch " + marker + ";; esac\ncat\n"
	if err := os.WriteFile(script, []byte(filterScript), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, d, "config", "filter.evil.clean", script)
	git(t, d, "config", "filter.evil.smudge", script)
	git(t, d, "config", "diff.evil.textconv", script)
	os.WriteFile(filepath.Join(d, ".gitattributes"), []byte("same.txt filter=evil diff=evil\n"), 0o644)
	git(t, d, "add", ".gitattributes")
	git(t, d, "commit", "-qm", "attributes")
	os.Remove(marker)
	os.WriteFile(filepath.Join(d, "same.txt"), []byte("changed\n"), 0o644)
	b := bind(t, d)
	s := mutationSnapshot(t, b)
	plan, err := b.PlanIndexMutation(context.Background(), s, MutationStage, []string{s.Unstaged[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PublishIndexMutation(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configured filter executed: %v", err)
	}
}
