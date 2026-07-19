package agothreadstore

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestRecordGitWriteReceiptPersistsExactOwnedChanges(t *testing.T) {
	store, threadID := receiptTestStore(t)
	input := receiptTestInput(threadID)

	receipt, err := store.RecordGitWriteReceipt(context.Background(), input)
	if err != nil {
		t.Fatalf("RecordGitWriteReceipt() error = %v", err)
	}
	if receipt.ReceiptID == "" || receipt.CreatedSequence == 0 {
		t.Fatalf("receipt identity = %#v", receipt)
	}
	if receipt.ThreadID != input.ThreadID || receipt.EnvironmentID != input.EnvironmentID || receipt.ExecutorGeneration != input.ExecutorGeneration ||
		receipt.RepositoryID != input.RepositoryID || receipt.WorktreeID != input.WorktreeID || receipt.BaseIdentity != input.BaseIdentity ||
		receipt.OperationID != input.OperationID || receipt.ToolCallID != input.ToolCallID || receipt.ToolName != input.ToolName {
		t.Fatalf("receipt lost binding or ownership: %#v", receipt)
	}
	if !reflect.DeepEqual(receipt.Changes, input.Changes) {
		t.Fatalf("receipt changes = %#v, want %#v", receipt.Changes, input.Changes)
	}

	loaded, err := store.GitWriteReceipt(context.Background(), receipt.ReceiptID)
	if err != nil || !reflect.DeepEqual(loaded, receipt) {
		t.Fatalf("GitWriteReceipt() = %#v, %v; want %#v", loaded, err, receipt)
	}
	byRetry, found, err := store.GitWriteReceiptRetry(context.Background(), input.ThreadID, input.ExecutorGeneration, input.IdempotencyKey)
	if err != nil || !found || !reflect.DeepEqual(byRetry, receipt) {
		t.Fatalf("GitWriteReceiptRetry() = %#v, %v, %v; want %#v", byRetry, found, err, receipt)
	}

	retry, err := store.RecordGitWriteReceipt(context.Background(), input)
	if err != nil || !reflect.DeepEqual(retry, receipt) {
		t.Fatalf("exact retry = %#v, %v; want %#v", retry, err, receipt)
	}
	changed := input
	changed.Changes = append([]GitReceiptPathChange(nil), input.Changes...)
	changed.Changes[0].After.Content = []byte("different\n")
	if _, err := store.RecordGitWriteReceipt(context.Background(), changed); !isGitReceiptConflict(err) {
		t.Fatalf("changed retry error = %v, want GitWriteReceiptConflictError", err)
	}
	immutableOwner := input
	immutableOwner.ToolCallID = "tool-call-other"
	if _, err := store.RecordGitWriteReceipt(context.Background(), immutableOwner); !isGitReceiptConflict(err) {
		t.Fatalf("changed owner retry error = %v, want GitWriteReceiptConflictError", err)
	}
}

func TestRecordGitWriteReceiptRejectsUnownedDirtyOrUnsafeChanges(t *testing.T) {
	store, threadID := receiptTestStore(t)
	base := receiptTestInput(threadID)
	tests := map[string]func(*GitWriteReceiptInput){
		"missing operation owner": func(in *GitWriteReceiptInput) { in.OperationID = "" },
		"missing tool owner":      func(in *GitWriteReceiptInput) { in.ToolCallID = "" },
		"empty affected paths":    func(in *GitWriteReceiptInput) { in.Changes = nil },
		"duplicate path": func(in *GitWriteReceiptInput) {
			in.Changes = append(in.Changes, in.Changes[0])
		},
		"unclean path":  func(in *GitWriteReceiptInput) { in.Changes[0].Path = "src/../dirty.txt" },
		"absolute path": func(in *GitWriteReceiptInput) { in.Changes[0].Path = "/tmp/dirty.txt" },
		"protected source": func(in *GitWriteReceiptInput) {
			in.Changes[0].Path = "thread-app/src/index.ts"
		},
		"protected test": func(in *GitWriteReceiptInput) {
			in.Changes[0].Path = "thread-app/test/thread-api.test.mjs"
		},
		"untyped identity": func(in *GitWriteReceiptInput) { in.Changes[0].After.Kind = "" },
		"absent with content": func(in *GitWriteReceiptInput) {
			in.Changes[0].After = GitReceiptFileIdentity{Kind: GitReceiptFileAbsent, Content: []byte("dirty")}
		},
		"regular without content": func(in *GitWriteReceiptInput) {
			in.Changes[0].After = GitReceiptFileIdentity{Kind: GitReceiptFileRegular, Mode: 0o644}
		},
		"no change": func(in *GitWriteReceiptInput) { in.Changes[0].After = in.Changes[0].Before },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			in := base
			in.IdempotencyKey = "receipt-" + name
			in.Changes = append([]GitReceiptPathChange(nil), base.Changes...)
			mutate(&in)
			if _, err := store.RecordGitWriteReceipt(context.Background(), in); err == nil {
				t.Fatal("RecordGitWriteReceipt() accepted unsafe or unowned state")
			}
		})
	}
}

func TestGitWriteReceiptsOverlappingPathsUsesPathComponentsAndScope(t *testing.T) {
	store, threadID := receiptTestStore(t)
	first := receiptTestInput(threadID)
	first.Changes = []GitReceiptPathChange{{Path: "src/a/file.txt", Before: absentReceiptFile(), After: regularReceiptFile("one\n")}}
	receipt, err := store.RecordGitWriteReceipt(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}

	for name, paths := range map[string][]string{
		"exact":      {"src/a/file.txt"},
		"ancestor":   {"src"},
		"descendant": {"src/a/file.txt/child"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := store.GitWriteReceiptsOverlappingPaths(context.Background(), GitReceiptScope{
				ThreadID: threadID, EnvironmentID: "env", ExecutorGeneration: 3, RepositoryID: "repo", WorktreeID: "worktree", BaseIdentity: "base:v1",
			}, paths)
			if err != nil || len(got) != 1 || got[0].ReceiptID != receipt.ReceiptID {
				t.Fatalf("overlap = %#v, %v; want receipt %q", got, err, receipt.ReceiptID)
			}
		})
	}
	none, err := store.GitWriteReceiptsOverlappingPaths(context.Background(), GitReceiptScope{
		ThreadID: threadID, EnvironmentID: "env", ExecutorGeneration: 3, RepositoryID: "repo", WorktreeID: "worktree", BaseIdentity: "base:v1",
	}, []string{"src/ab"})
	if err != nil || len(none) != 0 {
		t.Fatalf("component-neighbor overlap = %#v, %v; want none", none, err)
	}
}

func receiptTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	store := openTestStore(t)
	threadID := mustCreateThread(t, store, "receipt-"+t.Name()).ThreadID
	err := store.RecordGitBinding(context.Background(), GitBinding{
		ThreadID: threadID, EnvironmentID: "env", ExecutorGeneration: 3, WorktreeDir: "/repo", GitDir: "/repo/.git", CommonDir: "/repo/.git",
		RepositoryID: "repo", WorktreeID: "worktree", ObjectFormat: "sha256", BaseIdentity: "base:v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, threadID
}

func receiptTestInput(threadID string) GitWriteReceiptInput {
	return GitWriteReceiptInput{
		GitReceiptScope: GitReceiptScope{ThreadID: threadID, EnvironmentID: "env", ExecutorGeneration: 3, RepositoryID: "repo", WorktreeID: "worktree", BaseIdentity: "base:v1"},
		IdempotencyKey:  "receipt-key", OperationID: "operation-1", ToolCallID: "tool-call-1", ToolName: "write_file",
		Changes: []GitReceiptPathChange{
			{Path: "src/new.txt", Before: absentReceiptFile(), After: regularReceiptFile("new\n")},
			{Path: "link", Before: GitReceiptFileIdentity{Kind: GitReceiptFileSymlink, Mode: 0o777, Content: []byte("old-target")}, After: GitReceiptFileIdentity{Kind: GitReceiptFileSymlink, Mode: 0o777, Content: []byte("new-target")}},
		},
	}
}

func absentReceiptFile() GitReceiptFileIdentity {
	return GitReceiptFileIdentity{Kind: GitReceiptFileAbsent}
}
func regularReceiptFile(content string) GitReceiptFileIdentity {
	return GitReceiptFileIdentity{Kind: GitReceiptFileRegular, Mode: 0o644, Content: []byte(content)}
}
func isGitReceiptConflict(err error) bool {
	var conflict GitWriteReceiptConflictError
	return errors.As(err, &conflict)
}
