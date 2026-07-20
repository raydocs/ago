package agoboardstore

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"claudexflow/internal/agoboardprotocol"
)

func coordinatorActor() agoboardprotocol.Actor {
	return agoboardprotocol.Actor{ID: "scheduler", Role: agoboardprotocol.RoleCoordinator}
}

// buildClaimBoard creates a board whose tasks are all immediately ready, with
// the requested access modes, so slot policy can be exercised directly.
func buildClaimBoard(t *testing.T, store *Store, boardID, repository string, modes map[string]agoboardprotocol.AccessMode) agoboardprotocol.Board {
	t.Helper()
	ctx := context.Background()
	commands := []agoboardprotocol.Command{{
		SchemaVersion: agoboardprotocol.SchemaVersion,
		ID:            boardID + ":create", Actor: coordinatorActor(),
		Type:  agoboardprotocol.CommandBoardCreate,
		Board: &agoboardprotocol.BoardSpec{ID: boardID, Title: boardID, Repository: repository},
	}}
	version := uint64(1)
	names := make([]string, 0, len(modes))
	for name := range modes {
		names = append(names, name)
	}
	// Deterministic order so claim selection is predictable.
	for index := 0; index < len(names); index++ {
		for other := index + 1; other < len(names); other++ {
			if names[other] < names[index] {
				names[index], names[other] = names[other], names[index]
			}
		}
	}
	for _, name := range names {
		commands = append(commands, agoboardprotocol.Command{
			SchemaVersion: agoboardprotocol.SchemaVersion,
			ID:            boardID + ":add:" + name, ExpectedVersion: version, Actor: coordinatorActor(),
			Type: agoboardprotocol.CommandTaskAdd,
			Task: &agoboardprotocol.TaskSpec{
				ID: name, Title: name, AccessMode: modes[name],
				TerminalContract: agoboardprotocol.TerminalContract{Outcome: "done", AcceptanceCriteria: []string{"verified"}},
			},
		})
		version++
	}
	for _, name := range names {
		commands = append(commands, agoboardprotocol.Command{
			SchemaVersion: agoboardprotocol.SchemaVersion,
			ID:            boardID + ":activate:" + name, ExpectedVersion: version, Actor: coordinatorActor(),
			Type: agoboardprotocol.CommandTaskActivate, TaskID: name,
		})
		version++
	}
	result, err := store.CreateGraph(ctx, commands, []byte(`{"fixture":true}`))
	if err != nil {
		t.Fatalf("build claim board: %v", err)
	}
	return result.Board
}

func claimRequest(board agoboardprotocol.Board, commandID string, limits SlotLimits) ClaimRequest {
	return ClaimRequest{
		BoardID: board.ID, CommandID: commandID, Actor: coordinatorActor(),
		WorkerID: "worker", Now: testClock, LeaseDuration: time.Minute, Limits: limits,
	}
}

// Two schedulers racing at the same board version issue the same claim command.
// Exactly one may own fresh work; the other must be told it replayed.
func TestConcurrentClaimsElectExactlyOneFreshOwner(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "race.db"))
	defer store.Close()
	board := buildClaimBoard(t, store, "race", "repo", map[string]agoboardprotocol.AccessMode{
		"a": agoboardprotocol.AccessRead, "b": agoboardprotocol.AccessRead,
	})

	const racers = 8
	results := make(chan ClaimResult, racers)
	errs := make(chan error, racers)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for range racers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			// Identical command ID: this is the same scheduling decision being
			// made concurrently, not two different ones.
			result, err := store.Claim(context.Background(), claimRequest(board, "claim:race:same", SlotLimits{GlobalRunning: 4}))
			results <- result
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
	}
	owners, replays := 0, 0
	for result := range results {
		switch {
		case result.Dispatchable():
			owners++
		case result.Outcome == ClaimReplayed:
			replays++
		default:
			t.Fatalf("unexpected claim outcome %q", result.Outcome)
		}
	}
	if owners != 1 || replays != racers-1 {
		t.Fatalf("fresh owners = %d, replays = %d; want 1 and %d", owners, replays, racers-1)
	}
	var active int
	if err := store.db.QueryRow(`SELECT count(*) FROM leases WHERE state='active'`).Scan(&active); err != nil || active != 1 {
		t.Fatalf("active leases = %d, %v; want 1", active, err)
	}
}

// An exact claim replay must never hand a second caller something to dispatch.
func TestExactClaimReplayIsNotDispatchable(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "replay.db"))
	defer store.Close()
	board := buildClaimBoard(t, store, "replay", "repo", map[string]agoboardprotocol.AccessMode{
		"a": agoboardprotocol.AccessRead, "b": agoboardprotocol.AccessRead,
	})
	ctx := context.Background()

	first, err := store.Claim(ctx, claimRequest(board, "claim:one", SlotLimits{GlobalRunning: 4}))
	if err != nil || !first.Dispatchable() {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	second, err := store.Claim(ctx, claimRequest(board, "claim:one", SlotLimits{GlobalRunning: 4}))
	if err != nil {
		t.Fatalf("replayed claim: %v", err)
	}
	if second.Dispatchable() || second.Outcome != ClaimReplayed {
		t.Fatalf("replayed claim = %#v, want a non-dispatchable replay", second)
	}
	if second.FencingToken != "" {
		t.Fatal("a replayed claim handed out a fencing token")
	}
	var attempts int
	if err := store.db.QueryRow(`SELECT count(*) FROM attempts`).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("attempts after replay = %d, %v; want 1", attempts, err)
	}
}

func TestGlobalRunningLimitIsEnforcedAcrossBoards(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "global.db"))
	defer store.Close()
	ctx := context.Background()
	boards := []agoboardprotocol.Board{
		buildClaimBoard(t, store, "board-1", "repo-1", map[string]agoboardprotocol.AccessMode{"a": agoboardprotocol.AccessRead, "b": agoboardprotocol.AccessRead}),
		buildClaimBoard(t, store, "board-2", "repo-2", map[string]agoboardprotocol.AccessMode{"a": agoboardprotocol.AccessRead, "b": agoboardprotocol.AccessRead}),
	}
	limits := SlotLimits{GlobalRunning: 3, RepositoryReaders: 4}
	acquired := 0
	for round := range 6 {
		board := boards[round%2]
		current, err := store.Board(ctx, board.ID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.Claim(ctx, claimRequest(current, fmt.Sprintf("claim:global:%d", round), limits))
		if err != nil {
			t.Fatal(err)
		}
		if result.Dispatchable() {
			acquired++
		}
	}
	if acquired != 3 {
		t.Fatalf("claims acquired = %d, want the global limit of 3", acquired)
	}
	var active int
	if err := store.db.QueryRow(`SELECT count(*) FROM leases WHERE state='active'`).Scan(&active); err != nil || active != 3 {
		t.Fatalf("active leases = %d, %v; want 3", active, err)
	}
}

func TestPerBoardRunningLimitIsEnforced(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "board-limit.db"))
	defer store.Close()
	ctx := context.Background()
	board := buildClaimBoard(t, store, "busy", "repo", map[string]agoboardprotocol.AccessMode{
		"a": agoboardprotocol.AccessRead, "b": agoboardprotocol.AccessRead,
		"c": agoboardprotocol.AccessRead, "d": agoboardprotocol.AccessRead,
	})
	limits := SlotLimits{GlobalRunning: 10, BoardRunning: 2, RepositoryReaders: 10}
	acquired := 0
	for round := range 4 {
		current, err := store.Board(ctx, board.ID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.Claim(ctx, claimRequest(current, fmt.Sprintf("claim:board:%d", round), limits))
		if err != nil {
			t.Fatal(err)
		}
		if result.Dispatchable() {
			acquired++
		}
	}
	if acquired != 2 {
		t.Fatalf("claims acquired = %d, want the per-board limit of 2", acquired)
	}
}

// A repository writer is exclusive. It must not overlap another writer, and it
// must not start while readers hold the repository.
func TestRepositoryWriterIsExclusive(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "writers.db"))
	defer store.Close()
	ctx := context.Background()
	board := buildClaimBoard(t, store, "writers", "repo", map[string]agoboardprotocol.AccessMode{
		"w1": agoboardprotocol.AccessWrite, "w2": agoboardprotocol.AccessWrite,
	})
	limits := SlotLimits{GlobalRunning: 10, BoardRunning: 10, RepositoryWriters: 1, RepositoryReaders: 10}

	first, err := store.Claim(ctx, claimRequest(board, "claim:w1", limits))
	if err != nil || !first.Dispatchable() {
		t.Fatalf("first writer claim = %#v, %v", first, err)
	}
	current, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Claim(ctx, claimRequest(current, "claim:w2", limits))
	if err != nil {
		t.Fatal(err)
	}
	if second.Dispatchable() {
		t.Fatal("two writers ran concurrently on one repository")
	}
	if second.Outcome != ClaimNoSlot {
		t.Fatalf("second writer outcome = %q, want %q", second.Outcome, ClaimNoSlot)
	}
}

func TestRepositoryReadersRunConcurrentlyUpToTheirLimit(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "readers.db"))
	defer store.Close()
	ctx := context.Background()
	board := buildClaimBoard(t, store, "readers", "repo", map[string]agoboardprotocol.AccessMode{
		"r1": agoboardprotocol.AccessRead, "r2": agoboardprotocol.AccessRead, "r3": agoboardprotocol.AccessRead,
	})
	limits := SlotLimits{GlobalRunning: 10, BoardRunning: 10, RepositoryWriters: 1, RepositoryReaders: 2}
	acquired := 0
	for round := range 3 {
		current, err := store.Board(ctx, board.ID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.Claim(ctx, claimRequest(current, fmt.Sprintf("claim:r:%d", round), limits))
		if err != nil {
			t.Fatal(err)
		}
		if result.Dispatchable() {
			acquired++
		}
	}
	if acquired != 2 {
		t.Fatalf("concurrent readers = %d, want the reader limit of 2", acquired)
	}
}

// The writer/reader conflict policy runs in both directions.
func TestWriterAndReaderDoNotOverlapOnOneRepository(t *testing.T) {
	ctx := context.Background()
	limits := SlotLimits{GlobalRunning: 10, BoardRunning: 10, RepositoryWriters: 1, RepositoryReaders: 4}

	t.Run("reader blocks a writer", func(t *testing.T) {
		store := openStore(t, filepath.Join(t.TempDir(), "rw.db"))
		defer store.Close()
		// "a" sorts first and is the reader, so it is claimed first.
		board := buildClaimBoard(t, store, "rw", "repo", map[string]agoboardprotocol.AccessMode{
			"a-read": agoboardprotocol.AccessRead, "b-write": agoboardprotocol.AccessWrite,
		})
		first, err := store.Claim(ctx, claimRequest(board, "claim:read", limits))
		if err != nil || !first.Dispatchable() || first.TaskID != "a-read" {
			t.Fatalf("reader claim = %#v, %v", first, err)
		}
		current, err := store.Board(ctx, board.ID)
		if err != nil {
			t.Fatal(err)
		}
		second, err := store.Claim(ctx, claimRequest(current, "claim:write", limits))
		if err != nil {
			t.Fatal(err)
		}
		if second.Dispatchable() {
			t.Fatal("a writer started while a reader held the repository")
		}
	})

	t.Run("writer blocks a reader", func(t *testing.T) {
		store := openStore(t, filepath.Join(t.TempDir(), "wr.db"))
		defer store.Close()
		board := buildClaimBoard(t, store, "wr", "repo", map[string]agoboardprotocol.AccessMode{
			"a-write": agoboardprotocol.AccessWrite, "b-read": agoboardprotocol.AccessRead,
		})
		first, err := store.Claim(ctx, claimRequest(board, "claim:w", limits))
		if err != nil || !first.Dispatchable() || first.TaskID != "a-write" {
			t.Fatalf("writer claim = %#v, %v", first, err)
		}
		current, err := store.Board(ctx, board.ID)
		if err != nil {
			t.Fatal(err)
		}
		second, err := store.Claim(ctx, claimRequest(current, "claim:r", limits))
		if err != nil {
			t.Fatal(err)
		}
		if second.Dispatchable() {
			t.Fatal("a reader started while a writer held the repository exclusively")
		}
	})
}

// Slot limits must come from committed rows, not process memory: a second store
// handle on the same file represents a second scheduler process.
func TestSlotLimitsHoldAcrossSeparateStoreHandles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "processes.db")
	first := openStore(t, path)
	defer first.Close()
	board := buildClaimBoard(t, first, "shared", "repo", map[string]agoboardprotocol.AccessMode{
		"a": agoboardprotocol.AccessRead, "b": agoboardprotocol.AccessRead, "c": agoboardprotocol.AccessRead,
	})
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	limits := SlotLimits{GlobalRunning: 2, BoardRunning: 2, RepositoryReaders: 4}
	stores := []*Store{first, second, first}
	acquired := 0
	for round, store := range stores {
		current, err := store.Board(ctx, board.ID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := store.Claim(ctx, claimRequest(current, fmt.Sprintf("claim:proc:%d", round), limits))
		if err != nil {
			t.Fatal(err)
		}
		if result.Dispatchable() {
			acquired++
		}
	}
	if acquired != 2 {
		t.Fatalf("claims across two store handles = %d, want the limit of 2", acquired)
	}
}

func TestPausedBoardAdmitsNoNewClaims(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "paused.db"))
	defer store.Close()
	ctx := context.Background()
	board := buildClaimBoard(t, store, "paused", "repo", map[string]agoboardprotocol.AccessMode{
		"a": agoboardprotocol.AccessRead, "b": agoboardprotocol.AccessRead,
	})
	if _, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "pause", ExpectedVersion: board.Version,
		Actor: coordinatorActor(), Type: agoboardprotocol.CommandBoardPause, Reason: "用户暂停",
	}); err != nil {
		t.Fatalf("pause: %v", err)
	}
	current, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Claim(ctx, claimRequest(current, "claim:while-paused", SlotLimits{GlobalRunning: 4}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Dispatchable() || result.Outcome != ClaimPaused {
		t.Fatalf("claim on a paused board = %#v, want a paused outcome", result)
	}
}

// A task waiting out its retry backoff must not be claimable until the injected
// clock passes its durable deadline.
func TestRetryBackoffGatesClaimUntilTheClockAdvances(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "backoff.db"))
	defer store.Close()
	ctx := context.Background()
	board := buildClaimBoard(t, store, "backoff", "repo", map[string]agoboardprotocol.AccessMode{
		"only": agoboardprotocol.AccessRead,
	})
	limits := SlotLimits{GlobalRunning: 4}
	claim, err := store.Claim(ctx, claimRequest(board, "claim:first", limits))
	if err != nil || !claim.Dispatchable() {
		t.Fatalf("first claim = %#v, %v", claim, err)
	}
	eligibleAt := testClock.Add(4 * time.Second)
	if _, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "fail-1", ExpectedVersion: claim.Board.Version,
		Actor: agoboardprotocol.Actor{ID: "worker", Role: agoboardprotocol.RoleWorker},
		Type:  agoboardprotocol.CommandAttemptFail, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
		FencingToken: claim.FencingToken, FailureClass: agoboardprotocol.FailureTransient,
		Reason: "临时失败", NextEligibleAt: eligibleAt,
	}); err != nil {
		t.Fatalf("record failure: %v", err)
	}

	current, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	early := claimRequest(current, "claim:too-early", limits)
	early.Now = eligibleAt.Add(-time.Nanosecond)
	tooEarly, err := store.Claim(ctx, early)
	if err != nil {
		t.Fatal(err)
	}
	if tooEarly.Dispatchable() {
		t.Fatal("a task was retried before its durable backoff deadline")
	}

	late := claimRequest(current, "claim:eligible", limits)
	late.Now = eligibleAt
	retried, err := store.Claim(ctx, late)
	if err != nil {
		t.Fatal(err)
	}
	if !retried.Dispatchable() {
		t.Fatalf("task was not retried after its deadline: %#v", retried)
	}
	if retried.FencingToken == claim.FencingToken {
		t.Fatal("the retry reused the superseded attempt's fencing token")
	}
}

// The attempt ceiling is enforced by the state machine, so no claim path can
// produce a fourth attempt.
func TestExhaustedTaskIsNeverClaimedAgain(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "exhausted.db"))
	defer store.Close()
	ctx := context.Background()
	board := buildClaimBoard(t, store, "exhausted", "repo", map[string]agoboardprotocol.AccessMode{
		"only": agoboardprotocol.AccessRead,
	})
	limits := SlotLimits{GlobalRunning: 4}
	now := testClock
	for attempt := 1; attempt <= agoboardprotocol.MaxAttempts; attempt++ {
		current, err := store.Board(ctx, board.ID)
		if err != nil {
			t.Fatal(err)
		}
		request := claimRequest(current, fmt.Sprintf("claim:%d", attempt), limits)
		request.Now = now
		claim, err := store.Claim(ctx, request)
		if err != nil || !claim.Dispatchable() {
			t.Fatalf("attempt %d claim = %#v, %v", attempt, claim, err)
		}
		now = now.Add(time.Minute)
		if _, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
			SchemaVersion: agoboardprotocol.SchemaVersion, ID: fmt.Sprintf("fail:%d", attempt), ExpectedVersion: claim.Board.Version,
			Actor: agoboardprotocol.Actor{ID: "worker", Role: agoboardprotocol.RoleWorker},
			Type:  agoboardprotocol.CommandAttemptFail, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
			FencingToken: claim.FencingToken, FailureClass: agoboardprotocol.FailureTransient,
			Reason: "临时失败", NextEligibleAt: now,
		}); err != nil {
			t.Fatalf("attempt %d failure: %v", attempt, err)
		}
	}
	final, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, task, _ := taskByID(final, "only")
	if task.State != agoboardprotocol.TaskFailed || task.FailureClass != agoboardprotocol.FailureExhausted {
		t.Fatalf("exhausted task = %#v, want failed/exhausted", task)
	}
	request := claimRequest(final, "claim:fourth", limits)
	request.Now = now.Add(time.Hour)
	fourth, err := store.Claim(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Dispatchable() {
		t.Fatal("a fourth attempt was created for an exhausted task")
	}
	var attempts int
	if err := store.db.QueryRow(`SELECT count(*) FROM attempts`).Scan(&attempts); err != nil || attempts != agoboardprotocol.MaxAttempts {
		t.Fatalf("attempts = %d, %v; want %d", attempts, err, agoboardprotocol.MaxAttempts)
	}
}

// Renewal is fenced: only the lease's current token may extend it.
func TestLeaseRenewalRequiresTheCurrentFencingToken(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "renew-fence.db"))
	defer store.Close()
	ctx := context.Background()
	board := buildClaimBoard(t, store, "renew", "repo", map[string]agoboardprotocol.AccessMode{
		"only": agoboardprotocol.AccessRead,
	})
	claim, err := store.Claim(ctx, claimRequest(board, "claim", SlotLimits{GlobalRunning: 2}))
	if err != nil || !claim.Dispatchable() {
		t.Fatalf("claim = %#v, %v", claim, err)
	}
	extended := testClock.Add(time.Hour)
	if _, err := store.RenewLease(ctx, LeaseCommand{
		ID: "renew-wrong", ExpectedVersion: claim.Board.Version, Actor: coordinatorActor(),
		BoardID: board.ID, LeaseID: claim.LeaseID, ExpiresAt: extended,
		FencingToken: "a-token-from-somewhere-else",
	}); err == nil {
		t.Fatal("a lease was renewed with a token it was not issued")
	}
	if _, err := store.RenewLease(ctx, LeaseCommand{
		ID: "renew-right", ExpectedVersion: claim.Board.Version, Actor: coordinatorActor(),
		BoardID: board.ID, LeaseID: claim.LeaseID, ExpiresAt: extended,
		FencingToken: claim.FencingToken,
	}); err != nil {
		t.Fatalf("the current token could not renew its own lease: %v", err)
	}
}

// Two schedulers sweeping the same due leases must not fight: one applies the
// expiry, the other is absorbed by the receipt, and neither aborts the sweep.
func TestConcurrentReconcileSweepsAgreeAndCompleteEveryDueLease(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "reconcile-race.db")
	first := openStore(t, path)
	defer first.Close()
	board := buildClaimBoard(t, first, "sweep", "repo", map[string]agoboardprotocol.AccessMode{
		"a": agoboardprotocol.AccessRead, "b": agoboardprotocol.AccessRead, "c": agoboardprotocol.AccessRead,
	})
	limits := SlotLimits{GlobalRunning: 5, BoardRunning: 5, RepositoryReaders: 5}
	for round := range 3 {
		current, err := first.Board(ctx, board.ID)
		if err != nil {
			t.Fatal(err)
		}
		claim, err := first.Claim(ctx, claimRequest(current, fmt.Sprintf("claim:%d", round), limits))
		if err != nil || !claim.Dispatchable() {
			t.Fatalf("claim %d = %#v, %v", round, claim, err)
		}
	}
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	// Both sweeps run past every deadline, with deliberately different clock
	// readings: the reconciliation decision must not depend on either clock.
	deadline := testClock.Add(2 * time.Minute)
	type sweep struct {
		expired int
		err     error
	}
	results := make(chan sweep, 2)
	var wait sync.WaitGroup
	for index, store := range []*Store{first, second} {
		store := store
		skew := time.Duration(index) * 137 * time.Millisecond
		wait.Add(1)
		go func() {
			defer wait.Done()
			expired, err := store.ExpireDueLeases(ctx, deadline.Add(skew), coordinatorActor())
			results <- sweep{len(expired), err}
		}()
	}
	wait.Wait()
	close(results)
	applied := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("a concurrent reconcile sweep failed instead of being absorbed: %v", result.err)
		}
		applied += result.expired
	}
	// Every lease is expired exactly once across the two sweeps.
	if applied != 3 {
		t.Fatalf("expiries applied across both sweeps = %d, want 3", applied)
	}
	var active int
	if err := first.db.QueryRow(`SELECT count(*) FROM leases WHERE state='active'`).Scan(&active); err != nil || active != 0 {
		t.Fatalf("active leases after reconcile = %d, %v; want 0", active, err)
	}
	final, err := first.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range final.Tasks {
		if task.State != agoboardprotocol.TaskRetryWait {
			t.Fatalf("task %q after reconcile = %q, want retry-wait", task.ID, task.State)
		}
		if task.NextEligibleAt.IsZero() {
			t.Fatalf("task %q has no durable retry deadline", task.ID)
		}
	}
}
