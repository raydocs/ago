package agoboardstore

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"claudexflow/internal/agoboardprotocol"
)

// testClock keeps lease timing deterministic; no store test may sleep.
var testClock = time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

func TestReopenPreservesGraphLeaseBindingAndReplay(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "board.db")
	store := openStore(t, path)
	board := createReadyBoard(t, store)
	expires := time.Date(2026, 7, 20, 1, 2, 3, 0, time.UTC)
	command := leaseCommand(board, "lease-1", "attempt-1", "worker-1", "acquire-1")
	binding := &Binding{BoardID: board.ID, AttemptID: "attempt-1", ThreadID: "thread-1", ExecutorID: "runner-1"}
	first, err := store.AcquireLease(ctx, command, expires, binding)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openStore(t, path)
	defer store.Close()
	reopened, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatalf("Board after reopen: %v", err)
	}
	if !reflect.DeepEqual(reopened, first.Board) {
		t.Fatalf("reopened board = %#v, want %#v", reopened, first.Board)
	}
	gotBinding, err := store.Binding(ctx, board.ID, "attempt-1")
	if err != nil || !reflect.DeepEqual(gotBinding, *binding) {
		t.Fatalf("reopened binding = %#v, %v", gotBinding, err)
	}
	var gotExpiry int64
	if err := store.db.QueryRow(`SELECT expires_at FROM leases WHERE board_id=? AND lease_id='lease-1'`, board.ID).Scan(&gotExpiry); err != nil || gotExpiry != expires.UnixNano() {
		t.Fatalf("reopened lease expiry = %d, %v", gotExpiry, err)
	}
	events, err := store.Replay(ctx, board.ID, 0, 0)
	if err != nil || len(events) != int(first.Board.Version) {
		t.Fatalf("Replay = %d events, %v; want %d", len(events), err, first.Board.Version)
	}
	retry, err := store.AcquireLease(ctx, command, expires, binding)
	if err != nil || !reflect.DeepEqual(retry, first) {
		t.Fatalf("duplicate lease result = %#v, %v; want original %#v", retry, err, first)
	}
}

func TestConcurrentLeaseCompetitionHasOneWinner(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "competition.db"))
	defer store.Close()
	board := createReadyBoard(t, store)
	expires := time.Now().UTC().Add(time.Hour)
	commands := []agoboardprotocol.Command{
		leaseCommand(board, "lease-a", "attempt-a", "worker-a", "acquire-a"),
		leaseCommand(board, "lease-b", "attempt-b", "worker-b", "acquire-b"),
	}
	start := make(chan struct{})
	errors := make(chan error, len(commands))
	var wait sync.WaitGroup
	for _, command := range commands {
		command := command
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := store.AcquireLease(context.Background(), command, expires, nil)
			errors <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errors)
	winners := 0
	for err := range errors {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("lease winners = %d, want exactly 1", winners)
	}
	var active int
	if err := store.db.QueryRow(`SELECT count(*) FROM leases WHERE board_id=? AND state='active'`, board.ID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active leases = %d, %v; want 1", active, err)
	}
}

func TestExactConcurrentLeaseReplayHasOneFreshOwner(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "exact-replay.db"))
	defer store.Close()
	board := createReadyBoard(t, store)
	command := leaseCommand(board, "lease", "attempt", "worker", "same-acquire")
	expires := time.Now().UTC().Add(time.Hour)
	start := make(chan struct{})
	freshResults := make(chan bool, 2)
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, fresh, err := store.AcquireLeaseBoardOnce(context.Background(), board.ID, command, expires, nil)
			freshResults <- fresh
			errors <- err
		}()
	}
	close(start)
	wait.Wait()
	close(freshResults)
	close(errors)
	freshCount := 0
	for fresh := range freshResults {
		if fresh {
			freshCount++
		}
	}
	for err := range errors {
		if err != nil {
			t.Fatalf("AcquireLeaseBoardOnce: %v", err)
		}
	}
	if freshCount != 1 {
		t.Fatalf("fresh lease owners = %d, want 1", freshCount)
	}
}

func TestRenewAndExpireAreDurableIdempotentAndScheduleBoundedRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "expiry.db")
	store := openStore(t, path)
	board := createReadyBoard(t, store)
	initialExpiry := time.Now().UTC().Add(time.Minute)
	acquired, err := store.AcquireLease(ctx, leaseCommand(board, "lease", "attempt", "worker", "acquire"), initialExpiry, nil)
	if err != nil {
		t.Fatal(err)
	}
	renewedExpiry := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Nanosecond)
	renew := LeaseCommand{ID: "renew", ExpectedVersion: acquired.Board.Version, Actor: coordinator(), BoardID: board.ID, LeaseID: "lease", ExpiresAt: renewedExpiry, FencingToken: "token-attempt"}
	renewed, err := store.RenewLease(ctx, renew)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.RenewLease(ctx, renew)
	if err != nil || !reflect.DeepEqual(retry, renewed) {
		t.Fatalf("renew retry = %#v, %v; want %#v", retry, err, renewed)
	}
	staleExpiry := initialExpiry.UnixNano()
	_, staleExpired, err := store.mutateLease(ctx, LeaseCommand{ID: "stale-sweep", ExpectedVersion: renewed.Board.Version, Actor: coordinator(), BoardID: board.ID, LeaseID: "lease"}, true, &staleExpiry)
	if err != nil || staleExpired {
		t.Fatalf("stale expiry race expired renewed lease: expired=%v err=%v", staleExpired, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStore(t, path)
	defer store.Close()
	expired, err := store.ExpireDueLeases(ctx, renewedExpiry.Add(time.Nanosecond), coordinator())
	if err != nil || len(expired) != 1 {
		t.Fatalf("ExpireDueLeases = %#v, %v", expired, err)
	}
	// Expiry no longer makes a task instantly ready again: it schedules a
	// bounded retry, so a flapping worker cannot be redispatched in a loop.
	expiredBoard, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, task, found := taskByID(expiredBoard, "task")
	if !found || task.State != agoboardprotocol.TaskRetryWait {
		t.Fatalf("task after expiry = %#v (found %v), want state %q", task, found, agoboardprotocol.TaskRetryWait)
	}
	if !task.NextEligibleAt.After(renewedExpiry) {
		t.Fatalf("next eligible time %s must be after the elapsed deadline %s", task.NextEligibleAt, renewedExpiry)
	}
	if task.ActiveAttemptID != "" || task.ActiveLeaseID != "" {
		t.Fatalf("expired task still points at an attempt or lease: %#v", task)
	}
	if ready, err := store.Ready(ctx, board.ID); err != nil || len(ready) != 0 {
		t.Fatalf("Ready during backoff = %#v, %v, want none", ready, err)
	}
	completion, err := store.Completion(ctx, board.ID)
	if err != nil || completion.Status != CompletionInProgress || completion.Remaining != 1 {
		t.Fatalf("Completion = %#v, %v", completion, err)
	}
}

func TestOpenUsesWALFullAndRejectsFutureSchema(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "schema.db"))
	defer store.Close()
	var journal string
	var synchronous int
	if err := store.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" || synchronous != 2 {
		t.Fatalf("durability mode = %s/%d, want wal/FULL(2)", journal, synchronous)
	}
	store.db.SetMaxIdleConns(0)
	if err := store.db.Ping(); err != nil {
		t.Fatal(err)
	}
	var foreignKeys int
	if err := store.db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || synchronous != 2 {
		t.Fatalf("replacement connection pragmas = foreign_keys:%d synchronous:%d", foreignKeys, synchronous)
	}
}

func TestMultiBoardTargetingAndBoardLocalEventIDs(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "multi.db"))
	defer store.Close()
	actors := []agoboardprotocol.Actor{{ID: "one", Role: agoboardprotocol.RoleCoordinator}, {ID: "two", Role: agoboardprotocol.RoleCoordinator}}
	for index, boardID := range []string{"board-one", "board-two"} {
		created, err := store.Create(ctx, agoboardprotocol.Command{SchemaVersion: agoboardprotocol.SchemaVersion, ID: "same-create-id", Actor: actors[index], Type: agoboardprotocol.CommandBoardCreate, Board: &agoboardprotocol.BoardSpec{ID: boardID, Title: boardID}})
		if err != nil {
			t.Fatal(err)
		}
		command := agoboardprotocol.Command{SchemaVersion: agoboardprotocol.SchemaVersion, ID: "same-add-id", ExpectedVersion: created.Board.Version, Actor: actors[index], Type: agoboardprotocol.CommandTaskAdd, Task: &agoboardprotocol.TaskSpec{ID: "task", AccessMode: agoboardprotocol.AccessRead, Title: "Task", TerminalContract: agoboardprotocol.TerminalContract{Outcome: "done", AcceptanceCriteria: []string{"verified"}}}}
		result, err := store.ApplyBoard(ctx, boardID, command)
		if err != nil || result.Board.ID != boardID {
			t.Fatalf("ApplyBoard(%s) = %#v, %v", boardID, result, err)
		}
	}
	sharedActor := agoboardprotocol.Actor{ID: "shared", Role: agoboardprotocol.RoleCoordinator}
	activate := agoboardprotocol.Command{SchemaVersion: agoboardprotocol.SchemaVersion, ID: "same-targeted-command", ExpectedVersion: 2, Actor: sharedActor, Type: agoboardprotocol.CommandTaskActivate, TaskID: "task"}
	if _, err := store.ApplyBoard(ctx, "board-one", activate); err != nil {
		t.Fatal(err)
	}
	if result, err := store.ApplyBoard(ctx, "board-two", activate); err == nil || result.Board.ID == "board-one" {
		t.Fatalf("cross-board command aliased first result: %#v, %v", result, err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT count(*) FROM events`).Scan(&count); err != nil || count != 5 {
		t.Fatalf("board-local events = %d, %v; want 5", count, err)
	}
}

func TestExpiryCommandIdentityDoesNotAliasDelimitedIDs(t *testing.T) {
	first := expiryCommandID("a", "b:c", 42)
	second := expiryCommandID("a:b", "c", 42)
	if first == second {
		t.Fatalf("expiry identities collided: %q", first)
	}
}

func TestCreateGraphCommitsDefinitionAndWholeGraphAtomically(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "graph.db"))
	defer store.Close()
	definition := json.RawMessage(`{"goal":"实现闭环"}`)
	commands := graphCommands("board-atomic", false)
	created, err := store.CreateGraph(ctx, commands, definition)
	if err != nil || len(created.Board.Tasks) != 1 || created.Board.Tasks[0].State != agoboardprotocol.TaskReady {
		t.Fatalf("CreateGraph = %#v, %v", created, err)
	}
	var decoded map[string]string
	if err := store.Definition(ctx, "board-atomic", &decoded); err != nil || decoded["goal"] != "实现闭环" {
		t.Fatalf("Definition = %#v, %v", decoded, err)
	}
	retry, err := store.CreateGraph(ctx, commands, definition)
	if err != nil || !reflect.DeepEqual(retry, created) {
		t.Fatalf("CreateGraph exact retry = %#v, %v; want %#v", retry, err, created)
	}
	innerRetry, err := store.ApplyBoard(ctx, "board-atomic", commands[1])
	if err != nil || innerRetry.Board.Version != 2 || len(innerRetry.Events) != 1 || innerRetry.Events[0].CommandID != commands[1].ID {
		t.Fatalf("inner command retry = %#v, %v", innerRetry, err)
	}
	if _, err := store.CreateGraph(ctx, commands, json.RawMessage(`{"goal":"changed"}`)); err == nil {
		t.Fatal("CreateGraph accepted changed definition under the same command identity")
	}

	invalid := graphCommands("board-rollback", true)
	if _, err := store.CreateGraph(ctx, invalid, definition); err == nil {
		t.Fatal("CreateGraph accepted an invalid command in the graph batch")
	}
	if _, err := store.Board(ctx, "board-rollback"); err == nil {
		t.Fatal("failed CreateGraph left a partial board")
	}
	duplicateIDs := graphCommands("board-duplicate-command", false)
	duplicateIDs[2].ID = duplicateIDs[1].ID
	if _, err := store.CreateGraph(ctx, duplicateIDs, definition); err == nil {
		t.Fatal("CreateGraph accepted duplicate inner command IDs")
	}
	if _, err := store.Board(ctx, "board-duplicate-command"); err == nil {
		t.Fatal("duplicate command IDs left a partial board")
	}
}

func graphCommands(boardID string, invalid bool) []agoboardprotocol.Command {
	actor := coordinator()
	commands := []agoboardprotocol.Command{
		{SchemaVersion: agoboardprotocol.SchemaVersion, ID: boardID + ":create", Actor: actor, Type: agoboardprotocol.CommandBoardCreate, Board: &agoboardprotocol.BoardSpec{ID: boardID, Title: "Board"}},
		{SchemaVersion: agoboardprotocol.SchemaVersion, ID: boardID + ":add", ExpectedVersion: 1, Actor: actor, Type: agoboardprotocol.CommandTaskAdd, Task: &agoboardprotocol.TaskSpec{ID: "task", AccessMode: agoboardprotocol.AccessRead, Title: "Task", TerminalContract: agoboardprotocol.TerminalContract{Outcome: "done", AcceptanceCriteria: []string{"verified"}}}},
		{SchemaVersion: agoboardprotocol.SchemaVersion, ID: boardID + ":activate", ExpectedVersion: 2, Actor: actor, Type: agoboardprotocol.CommandTaskActivate, TaskID: "task"},
	}
	if invalid {
		commands[1].Task.TerminalContract.AcceptanceCriteria = nil
	}
	return commands
}

func openStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return store
}

func createReadyBoard(t *testing.T, store *Store) agoboardprotocol.Board {
	t.Helper()
	ctx := context.Background()
	result, err := store.Create(ctx, agoboardprotocol.Command{SchemaVersion: agoboardprotocol.SchemaVersion, ID: "create", Actor: coordinator(), Type: agoboardprotocol.CommandBoardCreate, Board: &agoboardprotocol.BoardSpec{ID: "board", Title: "Board"}})
	if err != nil {
		t.Fatal(err)
	}
	result, err = store.Apply(ctx, agoboardprotocol.Command{SchemaVersion: agoboardprotocol.SchemaVersion, ID: "add", ExpectedVersion: result.Board.Version, Actor: coordinator(), Type: agoboardprotocol.CommandTaskAdd, Task: &agoboardprotocol.TaskSpec{ID: "task", AccessMode: agoboardprotocol.AccessRead, Title: "Task", TerminalContract: agoboardprotocol.TerminalContract{Outcome: "done", AcceptanceCriteria: []string{"tests pass"}}}})
	if err != nil {
		t.Fatal(err)
	}
	result, err = store.Apply(ctx, agoboardprotocol.Command{SchemaVersion: agoboardprotocol.SchemaVersion, ID: "activate", ExpectedVersion: result.Board.Version, Actor: coordinator(), Type: agoboardprotocol.CommandTaskActivate, TaskID: "task"})
	if err != nil {
		t.Fatal(err)
	}
	return result.Board
}

func leaseCommand(board agoboardprotocol.Board, leaseID, attemptID, workerID, commandID string) agoboardprotocol.Command {
	return agoboardprotocol.Command{SchemaVersion: agoboardprotocol.SchemaVersion, ID: commandID, ExpectedVersion: board.Version, Actor: coordinator(), Type: agoboardprotocol.CommandLeaseAcquire, Lease: &agoboardprotocol.LeaseSpec{ID: leaseID, TaskID: "task", AttemptID: attemptID, WorkerID: workerID, FencingToken: "token-" + attemptID, AcquiredAt: testClock}}
}

func coordinator() agoboardprotocol.Actor {
	return agoboardprotocol.Actor{ID: "coordinator", Role: agoboardprotocol.RoleCoordinator}
}

// taskByID finds a task in a board aggregate.
func taskByID(board agoboardprotocol.Board, id string) (int, agoboardprotocol.Task, bool) {
	for index, task := range board.Tasks {
		if task.ID == id {
			return index, task, true
		}
	}
	return -1, agoboardprotocol.Task{}, false
}
