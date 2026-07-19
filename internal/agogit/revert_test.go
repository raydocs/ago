package agogit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"claudexflow/internal/agothreadstore"
	"golang.org/x/sys/unix"
)

func revertBinding(t *testing.T) (string, *Binding) {
	t.Helper()
	d := repo(t)
	b, err := Bind(context.Background(), d, ExecutorIdentity{Generation: "7", Environment: "env"})
	if err != nil {
		t.Fatal(err)
	}
	return d, b
}

func regular(content string) agothreadstore.GitReceiptFileIdentity {
	return agothreadstore.GitReceiptFileIdentity{Kind: agothreadstore.GitReceiptFileRegular, Mode: 0o644, Content: []byte(content)}
}

func absent() agothreadstore.GitReceiptFileIdentity {
	return agothreadstore.GitReceiptFileIdentity{Kind: agothreadstore.GitReceiptFileAbsent}
}

func revertReceipt(b *Binding, changes ...agothreadstore.GitReceiptPathChange) agothreadstore.GitWriteReceipt {
	return agothreadstore.GitWriteReceipt{
		ReceiptID: "W-durable", OwnerDomain: agothreadstore.GitWriteReceiptOwnerDomain, CreatedSequence: 11,
		GitWriteReceiptInput: agothreadstore.GitWriteReceiptInput{
			GitReceiptScope: agothreadstore.GitReceiptScope{
				ThreadID: "T-1", EnvironmentID: "env", ExecutorGeneration: 7,
				RepositoryID: b.RepositoryID(), WorktreeID: b.WorktreeID(), BaseIdentity: b.BaseIdentity(),
			},
			IdempotencyKey: "write-1", OperationID: "O-1", ToolCallID: "C-1", ToolName: "write",
			Changes: changes,
		},
	}
}

func planAndPublishRevert(t *testing.T, b *Binding, receipt agothreadstore.GitWriteReceipt) {
	t.Helper()
	plan, err := b.PlanReceiptRevert(context.Background(), receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.PublishReceiptRevert(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
}

func TestReceiptRevertRestoresExactBeforeImage(t *testing.T) {
	d, b := revertBinding(t)
	path := filepath.Join(d, "same.txt")
	if err := os.WriteFile(path, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	planAndPublishRevert(t, b, revertReceipt(b, agothreadstore.GitReceiptPathChange{
		Path: "same.txt", Before: regular("one\n"), After: regular("after\n"),
	}))
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, []byte("one\n")) {
		t.Fatalf("reverted bytes = %q, %v", got, err)
	}
}

func TestReceiptRevertPreservesLaterNonOverlappingTextEdit(t *testing.T) {
	d, b := revertBinding(t)
	path := filepath.Join(d, "same.txt")
	before := "alpha\nbravo\ncharlie\ndelta\necho\n"
	after := "alpha\nBRAVO\ncharlie\ndelta\necho\n"
	current := "alpha\nBRAVO\ncharlie\ndelta\nECHO\n"
	if err := os.WriteFile(path, []byte(current), 0o644); err != nil {
		t.Fatal(err)
	}
	planAndPublishRevert(t, b, revertReceipt(b, agothreadstore.GitReceiptPathChange{
		Path: "same.txt", Before: regular(before), After: regular(after),
	}))
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "alpha\nbravo\ncharlie\ndelta\nECHO\n" {
		t.Fatalf("merged revert = %q, %v", got, err)
	}
}

func TestReceiptRevertOverlappingTextEditConflictsWithoutWrite(t *testing.T) {
	d, b := revertBinding(t)
	path := filepath.Join(d, "same.txt")
	current := []byte("alpha\nLATER\ncharlie\n")
	if err := os.WriteFile(path, current, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := b.PlanReceiptRevert(context.Background(), revertReceipt(b, agothreadstore.GitReceiptPathChange{
		Path: "same.txt", Before: regular("alpha\nbravo\ncharlie\n"), After: regular("alpha\nWRITTEN\ncharlie\n"),
	}))
	if !errors.Is(err, ErrReceiptRevertConflict) {
		t.Fatalf("PlanReceiptRevert() error = %v, want conflict", err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, current) {
		t.Fatalf("conflicted plan wrote %q", got)
	}
}

func TestReceiptRevertRejectsProtectedPath(t *testing.T) {
	_, b := revertBinding(t)
	_, err := b.PlanReceiptRevert(context.Background(), revertReceipt(b, agothreadstore.GitReceiptPathChange{
		Path: "thread-app/src/index.ts", Before: regular("before\n"), After: regular("after\n"),
	}))
	if !errors.Is(err, ErrInvalidReceiptRevert) {
		t.Fatalf("protected receipt error = %v", err)
	}
}

func TestReceiptRevertNeverFollowsWorktreeSymlink(t *testing.T) {
	d, b := revertBinding(t)
	victim := filepath.Join(d, "victim.txt")
	if err := os.WriteFile(victim, []byte("victim\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(d, "same.txt")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("victim.txt", path); err != nil {
		t.Fatal(err)
	}
	_, err := b.PlanReceiptRevert(context.Background(), revertReceipt(b, agothreadstore.GitReceiptPathChange{
		Path: "same.txt", Before: regular("one\n"), After: regular("after\n"),
	}))
	if !errors.Is(err, ErrReceiptRevertConflict) {
		t.Fatalf("symlink error = %v, want conflict", err)
	}
	got, _ := os.ReadFile(victim)
	if string(got) != "victim\n" {
		t.Fatalf("symlink target changed to %q", got)
	}
}

func TestReceiptRevertRejectsUnrestorableSymlinkMode(t *testing.T) {
	d, b := revertBinding(t)
	if err := os.WriteFile(filepath.Join(d, "same.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := agothreadstore.GitReceiptFileIdentity{
		Kind: agothreadstore.GitReceiptFileSymlink, Mode: 0o644, Content: []byte("target.txt"),
	}
	_, err := b.PlanReceiptRevert(context.Background(), revertReceipt(b, agothreadstore.GitReceiptPathChange{
		Path: "same.txt", Before: before, After: regular("after\n"),
	}))
	if !errors.Is(err, ErrInvalidReceiptRevert) {
		t.Fatalf("unrestorable symlink mode error = %v", err)
	}
}

func TestReceiptRevertPlansEveryPathBeforePublishingAny(t *testing.T) {
	d, b := revertBinding(t)
	firstPath := filepath.Join(d, "first.txt")
	secondPath := filepath.Join(d, "second.txt")
	if err := os.WriteFile(firstPath, []byte("first-after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secondPath, []byte("second-later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	receipt := revertReceipt(b,
		agothreadstore.GitReceiptPathChange{Path: "first.txt", Before: regular("first-before\n"), After: regular("first-after\n")},
		agothreadstore.GitReceiptPathChange{Path: "second.txt", Before: regular("second-before\n"), After: regular("second-after\n")},
	)
	if _, err := b.PlanReceiptRevert(context.Background(), receipt); !errors.Is(err, ErrReceiptRevertConflict) {
		t.Fatalf("multi-path plan error = %v, want conflict", err)
	}
	first, _ := os.ReadFile(firstPath)
	second, _ := os.ReadFile(secondPath)
	if string(first) != "first-after\n" || string(second) != "second-later\n" {
		t.Fatalf("failed planning wrote first=%q second=%q", first, second)
	}
}

func TestReceiptRevertCreateDeleteAndModeTransitionsRequireExactPostimage(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		d, b := revertBinding(t)
		path := filepath.Join(d, "created.txt")
		if err := os.WriteFile(path, []byte("created\x00bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		planAndPublishRevert(t, b, revertReceipt(b, agothreadstore.GitReceiptPathChange{
			Path: "created.txt", Before: absent(), After: regular("created\x00bytes"),
		}))
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created path still exists: %v", err)
		}
	})

	t.Run("delete", func(t *testing.T) {
		d, b := revertBinding(t)
		path := filepath.Join(d, "deleted.txt")
		planAndPublishRevert(t, b, revertReceipt(b, agothreadstore.GitReceiptPathChange{
			Path: "deleted.txt", Before: regular("restored\n"), After: absent(),
		}))
		got, err := os.ReadFile(path)
		if err != nil || string(got) != "restored\n" {
			t.Fatalf("restored deletion = %q, %v", got, err)
		}
	})

	t.Run("mode", func(t *testing.T) {
		d, b := revertBinding(t)
		path := filepath.Join(d, "same.txt")
		if err := os.WriteFile(path, []byte("after\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
		before := regular("one\n")
		after := regular("after\n")
		after.Mode = 0o755
		planAndPublishRevert(t, b, revertReceipt(b, agothreadstore.GitReceiptPathChange{Path: "same.txt", Before: before, After: after}))
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o644 {
			t.Fatalf("restored mode = %v, %v", info, err)
		}
	})

	t.Run("binary mismatch", func(t *testing.T) {
		d, b := revertBinding(t)
		path := filepath.Join(d, "same.txt")
		current := []byte("after\x00later")
		if err := os.WriteFile(path, current, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := b.PlanReceiptRevert(context.Background(), revertReceipt(b, agothreadstore.GitReceiptPathChange{
			Path: "same.txt", Before: regular("before\x00bytes"), After: regular("after\x00bytes"),
		}))
		if !errors.Is(err, ErrReceiptRevertConflict) {
			t.Fatalf("binary mismatch error = %v", err)
		}
	})
}

func TestReceiptRevertFailsClosedOnStagedOverlap(t *testing.T) {
	d, b := revertBinding(t)
	if err := os.WriteFile(filepath.Join(d, "same.txt"), []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, d, "add", "same.txt")
	_, err := b.PlanReceiptRevert(context.Background(), revertReceipt(b, agothreadstore.GitReceiptPathChange{
		Path: "same.txt", Before: regular("one\n"), After: regular("after\n"),
	}))
	if !errors.Is(err, ErrReceiptRevertConflict) {
		t.Fatalf("staged overlap error = %v", err)
	}
}

func TestReceiptRevertRejectsNonDurableOrMismatchedReceipt(t *testing.T) {
	_, b := revertBinding(t)
	change := agothreadstore.GitReceiptPathChange{Path: "same.txt", Before: regular("one\n"), After: regular("after\n")}
	for name, mutate := range map[string]func(*agothreadstore.GitWriteReceipt){
		"missing durable ID": func(receipt *agothreadstore.GitWriteReceipt) { receipt.ReceiptID = "" },
		"wrong owner":        func(receipt *agothreadstore.GitWriteReceipt) { receipt.OwnerDomain = "other" },
		"wrong binding":      func(receipt *agothreadstore.GitWriteReceipt) { receipt.BaseIdentity = "other" },
		"unclean path":       func(receipt *agothreadstore.GitWriteReceipt) { receipt.Changes[0].Path = "a/../same.txt" },
	} {
		t.Run(name, func(t *testing.T) {
			receipt := revertReceipt(b, change)
			mutate(&receipt)
			if _, err := b.PlanReceiptRevert(context.Background(), receipt); !errors.Is(err, ErrInvalidReceiptRevert) {
				t.Fatalf("invalid receipt error = %v", err)
			}
		})
	}
}

func TestReceiptRevertPublicationRevalidatesBeforeWriting(t *testing.T) {
	d, b := revertBinding(t)
	path := filepath.Join(d, "same.txt")
	if err := os.WriteFile(path, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := b.PlanReceiptRevert(context.Background(), revertReceipt(b, agothreadstore.GitReceiptPathChange{
		Path: "same.txt", Before: regular("one\n"), After: regular("after\n"),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.PublishReceiptRevert(context.Background(), plan); !errors.Is(err, ErrReceiptRevertConflict) {
		t.Fatalf("stale publication error = %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "later\n" {
		t.Fatalf("stale publication wrote %q", got)
	}
}

func TestReceiptRevertRollsBackEarlierPathsWhenLaterPublicationFails(t *testing.T) {
	dir := t.TempDir()
	dirfd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(dirfd)
	for name, content := range map[string]string{"one": "before-one", "two": "before-two", "temp-one": "after-one"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prepared := []preparedReceiptRevert{
		{dirfd: dirfd, leaf: "one", tempLeaf: "temp-one"},
		{dirfd: dirfd, leaf: "two", tempLeaf: "missing-temp"},
	}
	err = publishPreparedReceiptReverts(prepared, []string{"one", "two"})
	if !errors.Is(err, ErrReceiptRevertConflict) || errors.Is(err, ErrReceiptRevertOutcomeUnknown) {
		t.Fatalf("publish error = %v", err)
	}
	for name, want := range map[string]string{"one": "before-one", "two": "before-two"} {
		got, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil || string(got) != want {
			t.Fatalf("%s after rollback = %q, %v; want %q", name, got, readErr, want)
		}
	}
}
