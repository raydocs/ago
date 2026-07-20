package agoboardapi_test

import (
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
)

// claimedTaskCount counts tasks that have left the pre-dispatch columns, which
// is what a duplicate claim would inflate.
func claimedTaskCount(snapshot boardSnapshot) int {
	claimed := 0
	for _, column := range snapshot.Columns {
		switch column.Name {
		case "Claimed", "Running", "Review", "Done":
			claimed += len(column.Tasks)
		}
	}
	return claimed
}

func advance(t *testing.T, handler http.Handler, boardID, commandID string) (int, boardSnapshot) {
	t.Helper()
	response := doRequest(t, handler, http.MethodPost, "/api/v1/boards/"+boardID+"/advance", map[string]any{"command_id": commandID})
	var snapshot boardSnapshot
	if response.Code == http.StatusOK {
		decodeInto(t, response, &snapshot)
	}
	return response.Code, snapshot
}

func createBoard(t *testing.T, handler http.Handler, commandID string) boardSnapshot {
	t.Helper()
	response := doRequest(t, handler, http.MethodPost, "/api/v1/goals", goalBody(commandID, chineseDemoObjective, t.TempDir()))
	if response.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", response.Code, response.Body.String())
	}
	var created goalCreateResponse
	decodeInto(t, response, &created)
	return created.Board
}

// A retried advance must not consume a second ready task. The demo graph starts
// with two independent ready roots, so a broken replay would visibly claim the
// other one.
func TestAdvanceReplayDoesNotClaimASecondTask(t *testing.T) {
	handler, store := newBoardTestServer(t, filepath.Join(t.TempDir(), "board.db"), nil)
	t.Cleanup(func() { _ = store.Close() })
	board := createBoard(t, handler, "cmd-advance-replay")

	status, first := advance(t, handler, board.BoardID, "adv-key")
	if status != http.StatusOK {
		t.Fatalf("first advance status = %d", status)
	}
	if claimedTaskCount(first) != 1 {
		t.Fatalf("first advance claimed %d tasks, want 1: %#v", claimedTaskCount(first), first.Columns)
	}

	status, replayed := advance(t, handler, board.BoardID, "adv-key")
	if status != http.StatusOK {
		t.Fatalf("replayed advance status = %d", status)
	}
	if claimedTaskCount(replayed) != 1 {
		t.Fatalf("replayed advance claimed %d tasks, want 1: %#v", claimedTaskCount(replayed), replayed.Columns)
	}
	if replayed.Version != first.Version || replayed.LatestEventSequence != first.LatestEventSequence {
		t.Fatalf("replay advanced the graph: first=(v%d,seq%d) replay=(v%d,seq%d)",
			first.Version, first.LatestEventSequence, replayed.Version, replayed.LatestEventSequence)
	}
	if !reflect.DeepEqual(taskIDSet(replayed), taskIDSet(first)) {
		t.Fatalf("replay changed task states: first=%#v replay=%#v", taskIDSet(first), taskIDSet(replayed))
	}
}

// A distinct key must still make progress, otherwise the fix would have frozen
// the board instead of making replay safe.
func TestDistinctAdvanceKeysClaimDistinctTasks(t *testing.T) {
	handler, store := newBoardTestServer(t, filepath.Join(t.TempDir(), "board.db"), nil)
	t.Cleanup(func() { _ = store.Close() })
	board := createBoard(t, handler, "cmd-advance-distinct")

	if status, _ := advance(t, handler, board.BoardID, "adv-1"); status != http.StatusOK {
		t.Fatalf("first advance status = %d", status)
	}
	status, second := advance(t, handler, board.BoardID, "adv-2")
	if status != http.StatusOK {
		t.Fatalf("second advance status = %d", status)
	}
	if claimedTaskCount(second) != 2 {
		t.Fatalf("two distinct keys claimed %d tasks, want 2: %#v", claimedTaskCount(second), second.Columns)
	}
}

// The only content an advance carries besides its key is the board it targets,
// so reusing a key against another board must conflict rather than advance it.
func TestAdvanceCommandIDReusedForAnotherBoardConflicts(t *testing.T) {
	handler, store := newBoardTestServer(t, filepath.Join(t.TempDir(), "board.db"), nil)
	t.Cleanup(func() { _ = store.Close() })
	first := createBoard(t, handler, "cmd-board-one")
	second := createBoard(t, handler, "cmd-board-two")

	if status, _ := advance(t, handler, first.BoardID, "shared-key"); status != http.StatusOK {
		t.Fatalf("advance on first board status = %d", status)
	}
	status, _ := advance(t, handler, second.BoardID, "shared-key")
	if status != http.StatusConflict {
		t.Fatalf("reused key on another board status = %d, want %d", status, http.StatusConflict)
	}

	snapshot := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+second.BoardID, nil)
	var untouched boardSnapshot
	decodeInto(t, snapshot, &untouched)
	if claimedTaskCount(untouched) != 0 || untouched.Version != second.Version {
		t.Fatalf("rejected advance still mutated the second board: %#v", untouched)
	}
}

// The receipt lives in SQLite, so a replay after a process restart must still
// return the recorded outcome without claiming more work.
func TestAdvanceReplayIsDurableAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handlerA, storeA := newBoardTestServer(t, dbPath, nil)
	board := createBoard(t, handlerA, "cmd-advance-restart")

	status, before := advance(t, handlerA, board.BoardID, "durable-key")
	if status != http.StatusOK {
		t.Fatalf("advance status = %d", status)
	}
	if err := storeA.Close(); err != nil {
		t.Fatalf("close store A: %v", err)
	}

	handlerB, storeB := newBoardTestServer(t, dbPath, nil)
	t.Cleanup(func() { _ = storeB.Close() })

	status, after := advance(t, handlerB, board.BoardID, "durable-key")
	if status != http.StatusOK {
		t.Fatalf("replayed advance after restart status = %d", status)
	}
	if after.Version != before.Version || after.LatestEventSequence != before.LatestEventSequence {
		t.Fatalf("replay after restart advanced the graph: before=(v%d,seq%d) after=(v%d,seq%d)",
			before.Version, before.LatestEventSequence, after.Version, after.LatestEventSequence)
	}
	if claimedTaskCount(after) != 1 {
		t.Fatalf("replay after restart claimed %d tasks, want 1", claimedTaskCount(after))
	}
	if !reflect.DeepEqual(taskIDSet(after), taskIDSet(before)) {
		t.Fatalf("replay after restart changed task states: before=%#v after=%#v", taskIDSet(before), taskIDSet(after))
	}
}
