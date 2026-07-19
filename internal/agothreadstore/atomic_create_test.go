package agothreadstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"claudexflow/internal/agoprotocol"
)

func TestCreateAtomicThreadCommitsCompleteIdentityAndRunningTurn(t *testing.T) {
	store := openTestStore(t)
	realWorkspace := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(realWorkspace, 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(filepath.Dir(realWorkspace), "workspace-link")
	if err := os.Symlink(realWorkspace, workspace); err != nil {
		t.Fatal(err)
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(realWorkspace)
	if err != nil {
		t.Fatal(err)
	}
	in := AtomicCreateInput{
		Spec:           ThreadSpec{Title: "Atomic", Workspace: workspace, Mode: agoprotocol.AgentModeHigh, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}},
		Project:        ProjectIdentity{ProjectID: "project-1", DisplayName: "Project One"},
		Agent:          AgentDefinitionSnapshot{DefinitionID: "agent-1", Version: "v7", DisplayName: "Builder", SystemInstructions: "Build carefully.", SystemInstructionsDigest: "sha256:abc", Capabilities: []string{"files", "shell"}, DefaultMode: agoprotocol.AgentModeMedium, Provenance: "registry://agents/agent-1@v7"},
		Provenance:     agoprotocol.Provenance{RootThreadID: "T-root", ParentThreadID: "T-parent", SourceThreadID: "T-source", ReplyToThreadID: "T-parent"},
		InitialMessage: json.RawMessage(`{"text":"start"}`),
	}
	created, err := store.CreateAtomicThread(context.Background(), createCommand("atomic-command", "atomic-request"), in)
	if err != nil {
		t.Fatalf("CreateAtomicThread() error = %v", err)
	}
	if created.Activity != agoprotocol.ActivityRunning || created.ActiveTurnID == "" || len(created.Queue) != 0 || created.LastSequence != 3 {
		t.Fatalf("result = %#v, want running active turn at sequence 3", created)
	}
	if len(created.Events) != 3 || created.Events[0].Type != agoprotocol.EventThreadCreated || created.Events[1].Type != agoprotocol.EventMessageAccepted || created.Events[2].Type != agoprotocol.EventTurnStarted {
		t.Fatalf("events = %#v", created.Events)
	}
	if created.Events[1].Provenance != in.Provenance {
		t.Fatalf("message provenance = %#v", created.Events[1].Provenance)
	}
	record, err := store.Thread(context.Background(), created.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Workspace != canonicalWorkspace || record.Project != in.Project || !reflect.DeepEqual(record.Agent, in.Agent) {
		t.Fatalf("thread = %#v, input = %#v", record, in)
	}
	mailbox, err := store.Mailbox(context.Background(), created.ThreadID)
	if err != nil || mailbox.Activity != agoprotocol.ActivityRunning || mailbox.ActiveTurnID != created.ActiveTurnID || mailbox.LastSequence != 3 {
		t.Fatalf("readback = %#v, %v", mailbox, err)
	}
}

func TestCreateAtomicThreadIdempotencyReopenAndConcurrentDuplicate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ago.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	in := atomicCreateFixture(t)
	command := createCommand("atomic-duplicate", "atomic-duplicate")
	var wg sync.WaitGroup
	results := make(chan MailboxState, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := store.CreateAtomicThread(context.Background(), command, in)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent create: %v", err)
		}
	}
	var first MailboxState
	for result := range results {
		if first.ThreadID == "" {
			first = result
		} else if !reflect.DeepEqual(first, result) {
			t.Fatalf("duplicate results differ: %#v %#v", first, result)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	retry, err := reopened.CreateAtomicThread(context.Background(), command, in)
	if err != nil || !reflect.DeepEqual(retry, first) {
		t.Fatalf("reopen retry = %#v, %v", retry, err)
	}
	changed := in
	changed.InitialMessage = json.RawMessage(`{"text":"changed"}`)
	if _, err := reopened.CreateAtomicThread(context.Background(), command, changed); err == nil {
		t.Fatal("changed retry was accepted")
	}
}

func TestCreateAtomicThreadRollsBackEveryWriteOnLateEventFailure(t *testing.T) {
	store := openTestStore(t)
	in := atomicCreateFixture(t)
	// Event validation occurs after the thread row is inserted, exercising the
	// transaction rollback rather than only request validation.
	in.Provenance = agoprotocol.Provenance{ParentThreadID: "T-parent"}
	if _, err := store.CreateAtomicThread(context.Background(), createCommand("rollback", "rollback"), in); err == nil {
		t.Fatal("CreateAtomicThread() accepted incomplete inter-thread provenance")
	}
	threads, err := store.ListThreads(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) != 0 {
		t.Fatalf("rollback left threads behind: %#v", threads)
	}
	in.Provenance = agoprotocol.Provenance{}
	created, err := store.CreateAtomicThread(context.Background(), createCommand("rollback", "rollback"), in)
	if err != nil || created.LastSequence != 3 {
		t.Fatalf("idempotency key was consumed by rolled-back create: %#v, %v", created, err)
	}
}

func atomicCreateFixture(t *testing.T) AtomicCreateInput {
	t.Helper()
	return AtomicCreateInput{Spec: ThreadSpec{Title: "Atomic", Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}}, Project: ProjectIdentity{ProjectID: "p"}, Agent: AgentDefinitionSnapshot{DefinitionID: "a", Version: "1", DisplayName: "Agent", SystemInstructionsDigest: "sha256:x", DefaultMode: agoprotocol.AgentModeMedium}, InitialMessage: json.RawMessage(`{"text":"start"}`)}
}
