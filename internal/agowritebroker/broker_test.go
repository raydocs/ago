package agowritebroker

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"claudexflow/internal/agogit"
	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
)

func TestWriteFilePersistsExactBytesAndReceipt(t *testing.T) {
	store, threadID, workspace := testBrokerStore(t)
	path := "nested/file.txt"
	if err := os.Mkdir(filepath.Join(workspace, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, path), []byte("before\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	mode := uint32(0o600)

	result, err := New(store).WriteFile(context.Background(), WriteFileRequest{
		ThreadID: threadID, Path: path, Content: []byte("after\x00exact\n"), Mode: &mode,
		OperationID: "operation-1", ToolCallID: "tool-call-1", ToolName: "write_file", IdempotencyKey: "write-1",
	})
	if err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if result.ReceiptID == "" {
		t.Fatal("WriteFile() returned an empty receipt ID")
	}
	got, err := os.ReadFile(filepath.Join(workspace, path))
	if err != nil || !reflect.DeepEqual(got, []byte("after\x00exact\n")) {
		t.Fatalf("written bytes = %q, %v", got, err)
	}
	info, err := os.Stat(filepath.Join(workspace, path))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("written mode = %v, %v", info.Mode().Perm(), err)
	}
	receipt, err := store.GitWriteReceipt(context.Background(), result.ReceiptID)
	if err != nil {
		t.Fatalf("GitWriteReceipt() error = %v", err)
	}
	wantChanges := []agothreadstore.GitReceiptPathChange{{
		Path:   path,
		Before: agothreadstore.GitReceiptFileIdentity{Kind: agothreadstore.GitReceiptFileRegular, Mode: 0o640, Content: []byte("before\n")},
		After:  agothreadstore.GitReceiptFileIdentity{Kind: agothreadstore.GitReceiptFileRegular, Mode: 0o600, Content: []byte("after\x00exact\n")},
	}}
	if receipt.OperationID != "operation-1" || receipt.ToolCallID != "tool-call-1" || receipt.ToolName != "write_file" || !reflect.DeepEqual(receipt.Changes, wantChanges) {
		t.Fatalf("receipt = %#v, want ownership and exact identities %#v", receipt, wantChanges)
	}
}

func TestWriteFileExactRetryReturnsReceiptWithoutWritingAgain(t *testing.T) {
	store, threadID, workspace := testBrokerStore(t)
	request := writeRequest(threadID, "retry.txt", "first", "retry-1")
	first, err := New(store).WriteFile(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, request.Path), []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	retry, err := New(store).WriteFile(context.Background(), request)
	if err != nil || retry.ReceiptID != first.ReceiptID {
		t.Fatalf("exact retry = %#v, %v; want receipt %q", retry, err, first.ReceiptID)
	}
	got, _ := os.ReadFile(filepath.Join(workspace, request.Path))
	if string(got) != "external" {
		t.Fatalf("exact retry rewrote current workspace bytes: %q", got)
	}
}

func TestWriteFileChangedRetryConflictsBeforeWriting(t *testing.T) {
	store, threadID, workspace := testBrokerStore(t)
	request := writeRequest(threadID, "retry.txt", "first", "retry-changed")
	if _, err := New(store).WriteFile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Content = []byte("changed")
	_, err := New(store).WriteFile(context.Background(), request)
	var conflict ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("changed retry error = %v, want ConflictError", err)
	}
	got, _ := os.ReadFile(filepath.Join(workspace, request.Path))
	if string(got) != "first" {
		t.Fatalf("changed retry wrote bytes: %q", got)
	}
}

func TestWriteFileChangedRetryPathConflictsBeforeWriting(t *testing.T) {
	store, threadID, workspace := testBrokerStore(t)
	request := writeRequest(threadID, "first.txt", "first", "retry-cross-path")
	if _, err := New(store).WriteFile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Path = "second.txt"
	request.Content = []byte("second")
	_, err := New(store).WriteFile(context.Background(), request)
	var conflict ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("cross-path retry error = %v, want ConflictError", err)
	}
	if _, statErr := os.Lstat(filepath.Join(workspace, "second.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("cross-path retry performed a write before conflict: %v", statErr)
	}
}

func TestWriteFileRejectsUnsafePathsAndNonWriteTools(t *testing.T) {
	store, threadID, workspace := testBrokerStore(t)
	if err := os.Symlink(t.TempDir(), filepath.Join(workspace, "linked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("tracked.txt", filepath.Join(workspace, "leaf-link")); err != nil {
		t.Fatal(err)
	}
	tests := map[string]WriteFileRequest{
		"absolute":         writeRequest(threadID, filepath.Join(workspace, "outside"), "x", "unsafe-absolute"),
		"unclean":          writeRequest(threadID, "a/../tracked.txt", "x", "unsafe-unclean"),
		"git metadata":     writeRequest(threadID, ".git/config", "x", "unsafe-git"),
		"protected source": writeRequest(threadID, "thread-app/src/index.ts", "x", "unsafe-protected"),
		"symlink parent":   writeRequest(threadID, "linked/file", "x", "unsafe-parent-link"),
		"symlink leaf":     writeRequest(threadID, "leaf-link", "x", "unsafe-leaf-link"),
		"shell tool":       writeRequest(threadID, "shell.txt", "x", "unsafe-shell"),
	}
	shell := tests["shell tool"]
	shell.ToolName = "shell"
	tests["shell tool"] = shell
	for name, request := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := New(store).WriteFile(context.Background(), request); err == nil {
				t.Fatal("WriteFile() accepted an unsafe request")
			}
		})
	}
}

func TestWriteFileFailureDoesNotRecordReceipt(t *testing.T) {
	store, threadID, workspace := testBrokerStore(t)
	if err := os.Mkdir(filepath.Join(workspace, "directory-target"), 0o755); err != nil {
		t.Fatal(err)
	}
	request := writeRequest(threadID, "directory-target", "cannot replace directory", "failed-write")
	if _, err := New(store).WriteFile(context.Background(), request); err == nil {
		t.Fatal("WriteFile() unexpectedly succeeded")
	}
	projection, err := store.ClientProjection(context.Background(), threadID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range projection.Events {
		if event.Type == agoprotocol.EventType("git.write-receipt-recorded") {
			t.Fatalf("failed write produced receipt event %#v", event)
		}
	}
}

func TestWriteFileCreatesEmptyRegularFile(t *testing.T) {
	store, threadID, workspace := testBrokerStore(t)
	request := writeRequest(threadID, "empty.txt", "", "empty-file")
	result, err := New(store).WriteFile(context.Background(), request)
	if err != nil {
		t.Fatalf("WriteFile(empty) error = %v", err)
	}
	receipt, err := store.GitWriteReceipt(context.Background(), result.ReceiptID)
	if err != nil || len(receipt.Changes) != 1 || receipt.Changes[0].After.Content == nil {
		t.Fatalf("empty-file receipt = %#v, %v", receipt, err)
	}
	info, err := os.Stat(filepath.Join(workspace, request.Path))
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Fatalf("empty-file identity = %#v, %v", info, err)
	}
}

func writeRequest(threadID, path, content, key string) WriteFileRequest {
	return WriteFileRequest{
		ThreadID: threadID, Path: path, Content: []byte(content), OperationID: "operation-" + key,
		ToolCallID: "tool-call-" + key, ToolName: ToolNameWriteFile, IdempotencyKey: key,
	}
}

func testBrokerStore(t *testing.T) (*agothreadstore.Store, string, string) {
	t.Helper()
	workspace := t.TempDir()
	runGit(t, workspace, "init", "-q")
	runGit(t, workspace, "config", "user.name", "Ago Test")
	runGit(t, workspace, "config", "user.email", "ago@example.invalid")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, workspace, "add", "tracked.txt")
	runGit(t, workspace, "commit", "-qm", "base")

	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: "create", IdempotencyKey: "create", ActorID: "test", Type: agoprotocol.CommandThreadCreate,
	}, agothreadstore.ThreadSpec{Workspace: workspace, Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = agogit.NewService(store).Refresh(context.Background(), agogit.RefreshInput{
		ThreadID: created.ThreadID, Workspace: workspace, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal},
		EnvironmentID: "env-1", ExecutorGeneration: 1, IdempotencyKey: "initial-snapshot",
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, created.ThreadID, workspace
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
