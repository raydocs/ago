package agoboardapi_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sseFrame is one parsed "id: <sequence>\ndata: <json>\n\n" frame.
type sseFrame struct {
	ID   uint64
	Data map[string]any
}

// readSSEFrames connects to an SSE endpoint (optionally with Last-Event-ID
// set) and returns after collecting `want` frames or the context deadline,
// whichever comes first. Comment lines (starting with ":") are ignored.
func readSSEFrames(t *testing.T, ctx context.Context, url, lastEventID string, want int) []sseFrame {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build SSE request: %v", err)
	}
	if lastEventID != "" {
		request.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("SSE request: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("SSE status = %d, body = %s", response.StatusCode, body)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("SSE content-type = %q, want text/event-stream", contentType)
	}
	if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "no-cache" {
		t.Fatalf("SSE cache-control = %q, want no-cache", cacheControl)
	}

	reader := bufio.NewReader(response.Body)
	frames := make([]sseFrame, 0, want)
	var pendingID uint64
	haveID := false
	for len(frames) < want {
		line, readErr := reader.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, ":"):
			// heartbeat comment; ignore.
		case strings.HasPrefix(line, "id:"):
			value := strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			parsed, parseErr := strconv.ParseUint(value, 10, 64)
			if parseErr != nil {
				t.Fatalf("malformed SSE id line %q: %v", line, parseErr)
			}
			pendingID, haveID = parsed, true
		case strings.HasPrefix(line, "data:"):
			if !haveID {
				t.Fatalf("SSE data line without a preceding id line: %q", line)
			}
			value := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			var decoded map[string]any
			if err := json.Unmarshal([]byte(value), &decoded); err != nil {
				t.Fatalf("decode SSE data %q: %v", value, err)
			}
			frames = append(frames, sseFrame{ID: pendingID, Data: decoded})
			haveID = false
		}
		if readErr != nil {
			return frames
		}
	}
	return frames
}

func sequenceIDs(frames []sseFrame) []uint64 {
	ids := make([]uint64, len(frames))
	for index, frame := range frames {
		ids[index] = frame.ID
	}
	return ids
}

func sameSequenceIDs(a, b []sseFrame) bool {
	return reflect.DeepEqual(sequenceIDs(a), sequenceIDs(b))
}

func assertContainsInOrder(t *testing.T, got, want []string) {
	t.Helper()
	cursor := 0
	for _, wantType := range want {
		found := false
		for ; cursor < len(got); cursor++ {
			if got[cursor] == wantType {
				found = true
				cursor++
				break
			}
		}
		if !found {
			t.Fatalf("event types %#v do not contain %q as the next element of subsequence %#v", got, wantType, want)
		}
	}
}

// -- real-HTTP helpers (SSE tests need a live streaming connection) ---------

func realRequest(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return response
}

func decodeResponse(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatalf("decode response: %v", err)
	}
}

func createRealBoard(t *testing.T, baseURL, commandID, repoRoot string) goalCreateResponse {
	t.Helper()
	response := realRequest(t, http.MethodPost, baseURL+"/api/v1/goals", goalBody(commandID, chineseDemoObjective, repoRoot))
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("create status = %d, body = %s", response.StatusCode, body)
	}
	var created goalCreateResponse
	decodeResponse(t, response, &created)
	return created
}

// -- required test cases ------------------------------------------------------

func TestEventsStreamInOrderAcrossCreateAndAdvance(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store, scheduler := newBoardTestServerWithScheduler(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	created := createRealBoard(t, httpServer.URL, "sse-order", t.TempDir())
	boardID := created.Board.BoardID
	driveWithScheduler(t, store, scheduler, boardID)
	final := snapshotOf(t, handler, boardID)
	if !final.Completed || final.Progress.Failed != 0 {
		t.Fatalf("board did not complete cleanly: %#v", final.Progress)
	}
	total := int(final.LatestEventSequence)
	if total == 0 {
		t.Fatalf("final snapshot has no events: %#v", final)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frames := readSSEFrames(t, ctx, httpServer.URL+"/api/v1/boards/"+boardID+"/events", "", total)
	if len(frames) != total {
		t.Fatalf("collected %d frames, want %d", len(frames), total)
	}

	var previous uint64
	gotTypes := make([]string, 0, total)
	for index, frame := range frames {
		if frame.ID <= previous {
			t.Fatalf("frame[%d] sequence %d is not strictly increasing after %d", index, frame.ID, previous)
		}
		previous = frame.ID
		sequenceField, ok := frame.Data["sequence"].(float64)
		if !ok || uint64(sequenceField) != frame.ID {
			t.Fatalf("frame[%d] data.sequence = %v, want %d", index, frame.Data["sequence"], frame.ID)
		}
		eventType, _ := frame.Data["type"].(string)
		gotTypes = append(gotTypes, eventType)
	}
	if frames[0].ID != 1 || frames[len(frames)-1].ID != uint64(total) {
		t.Fatalf("sequence range = [%d,%d], want [1,%d]", frames[0].ID, frames[len(frames)-1].ID, total)
	}
	wantSubsequence := []string{"board.created", "task.added", "lease.acquired", "evidence.submitted", "evidence.accepted"}
	assertContainsInOrder(t, gotTypes, wantSubsequence)
}

func TestReconnectingWithLastEventIDReturnsOnlyLaterEvents(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store, scheduler := newBoardTestServerWithScheduler(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	created := createRealBoard(t, httpServer.URL, "sse-resume", t.TempDir())
	boardID := created.Board.BoardID
	driveWithScheduler(t, store, scheduler, boardID)
	final := snapshotOf(t, handler, boardID)
	total := int(final.LatestEventSequence)
	if total < 2 {
		t.Fatalf("need at least 2 durable events to test resumption, got %d", total)
	}
	cursor := uint64(total / 2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frames := readSSEFrames(t, ctx, httpServer.URL+"/api/v1/boards/"+boardID+"/events", strconv.FormatUint(cursor, 10), total-int(cursor))
	if len(frames) != total-int(cursor) {
		t.Fatalf("collected %d frames, want %d", len(frames), total-int(cursor))
	}
	seen := make(map[uint64]bool, len(frames))
	for index, frame := range frames {
		if frame.ID <= cursor {
			t.Fatalf("frame[%d] sequence %d is not greater than cursor %d", index, frame.ID, cursor)
		}
		if seen[frame.ID] {
			t.Fatalf("duplicate sequence %d in resumed stream", frame.ID)
		}
		seen[frame.ID] = true
	}
	if frames[0].ID != cursor+1 || frames[len(frames)-1].ID != uint64(total) {
		t.Fatalf("resumed sequence range = [%d,%d], want [%d,%d]", frames[0].ID, frames[len(frames)-1].ID, cursor+1, total)
	}
}

func TestAfterQueryMatchesHeaderAndHeaderWinsOnConflict(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store, scheduler := newBoardTestServerWithScheduler(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	created := createRealBoard(t, httpServer.URL, "sse-after-param", t.TempDir())
	boardID := created.Board.BoardID
	driveWithScheduler(t, store, scheduler, boardID)
	final := snapshotOf(t, handler, boardID)
	total := int(final.LatestEventSequence)
	if total < 4 {
		t.Fatalf("need at least 4 durable events, got %d", total)
	}
	cursor := uint64(total / 2)
	eventsURL := httpServer.URL + "/api/v1/boards/" + boardID + "/events"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	viaHeader := readSSEFrames(t, ctx, eventsURL, strconv.FormatUint(cursor, 10), total-int(cursor))
	viaQuery := readSSEFrames(t, ctx, eventsURL+"?after="+strconv.FormatUint(cursor, 10), "", total-int(cursor))
	if !sameSequenceIDs(viaHeader, viaQuery) {
		t.Fatalf("header cursor frames = %v, query cursor frames = %v", sequenceIDs(viaHeader), sequenceIDs(viaQuery))
	}

	headerCursor, queryCursor := uint64(1), uint64(total-1)
	if headerCursor == queryCursor {
		t.Fatalf("test fixture needs distinct header/query cursors, got %d for both", headerCursor)
	}
	conflicting := readSSEFrames(t, ctx, eventsURL+"?after="+strconv.FormatUint(queryCursor, 10), strconv.FormatUint(headerCursor, 10), total-int(headerCursor))
	if len(conflicting) != total-int(headerCursor) || conflicting[0].ID != headerCursor+1 {
		t.Fatalf("header should win over conflicting query: got sequences %v, want %d frames starting at %d", sequenceIDs(conflicting), total-int(headerCursor), headerCursor+1)
	}
}

func TestReplayingCursorFromZeroDoesNotMutateBoard(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store, scheduler := newBoardTestServerWithScheduler(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	created := createRealBoard(t, httpServer.URL, "sse-replay-idempotent", t.TempDir())
	boardID := created.Board.BoardID
	driveWithScheduler(t, store, scheduler, boardID)
	final := snapshotOf(t, handler, boardID)
	total := int(final.LatestEventSequence)
	before := taskIDSet(final)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	eventsURL := httpServer.URL + "/api/v1/boards/" + boardID + "/events"
	_ = readSSEFrames(t, ctx, eventsURL, "", total)
	_ = readSSEFrames(t, ctx, eventsURL, "", total)

	afterResponse := realRequest(t, http.MethodGet, httpServer.URL+"/api/v1/boards/"+boardID, nil)
	var after boardSnapshot
	decodeResponse(t, afterResponse, &after)
	if after.Version != final.Version {
		t.Fatalf("version changed after replay: before=%d after=%d", final.Version, after.Version)
	}
	if !reflect.DeepEqual(before, taskIDSet(after)) {
		t.Fatalf("task states changed after replay: before=%#v after=%#v", before, taskIDSet(after))
	}
}

func TestServerRestartPreservesSSEResumability(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handlerA, storeA, schedulerA := newBoardTestServerWithScheduler(t, dbPath, nil)
	httpServerA := httptest.NewServer(handlerA)

	created := createRealBoard(t, httpServerA.URL, "sse-restart", t.TempDir())
	boardID := created.Board.BoardID
	driveWithScheduler(t, storeA, schedulerA, boardID)
	final := snapshotOf(t, handlerA, boardID)
	total := int(final.LatestEventSequence)
	if total < 2 {
		t.Fatalf("need at least 2 durable events, got %d", total)
	}
	httpServerA.Close()
	if err := storeA.Close(); err != nil {
		t.Fatalf("close store A: %v", err)
	}

	handlerB, storeB, _ := newBoardTestServerWithScheduler(t, dbPath, nil)
	t.Cleanup(func() { _ = storeB.Close() })
	httpServerB := httptest.NewServer(handlerB)
	t.Cleanup(httpServerB.Close)

	cursor := uint64(total / 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frames := readSSEFrames(t, ctx, httpServerB.URL+"/api/v1/boards/"+boardID+"/events", strconv.FormatUint(cursor, 10), total-int(cursor))
	if len(frames) != total-int(cursor) {
		t.Fatalf("resumed frames from restarted server = %d, want %d", len(frames), total-int(cursor))
	}
	if frames[0].ID != cursor+1 || frames[len(frames)-1].ID != uint64(total) {
		t.Fatalf("resumed sequence range = [%d,%d], want [%d,%d]", frames[0].ID, frames[len(frames)-1].ID, cursor+1, total)
	}
}

func TestMalformedCursorReturns400(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store, _ := newBoardTestServerWithScheduler(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	created := createRealBoard(t, httpServer.URL, "sse-malformed-cursor", t.TempDir())
	eventsURL := httpServer.URL + "/api/v1/boards/" + created.Board.BoardID + "/events"

	headerRequest, err := http.NewRequest(http.MethodGet, eventsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	headerRequest.Header.Set("Last-Event-ID", "not-a-number")
	headerResponse, err := http.DefaultClient.Do(headerRequest)
	if err != nil {
		t.Fatal(err)
	}
	assertBadCursorResponse(t, headerResponse)

	queryResponse, err := http.Get(eventsURL + "?after=not-a-number")
	if err != nil {
		t.Fatal(err)
	}
	assertBadCursorResponse(t, queryResponse)
}

func assertBadCursorResponse(t *testing.T, response *http.Response) {
	t.Helper()
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("malformed cursor status = %d, body = %s", response.StatusCode, body)
	}
	var errorBody apiError
	if err := json.NewDecoder(response.Body).Decode(&errorBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errorBody.Error.Code == "" || errorBody.Error.Message == "" {
		t.Fatalf("error body = %#v", errorBody)
	}
}

// The fencing token is the only credential authorizing an attempt. It must not
// reach a subscriber, who needs no credential at all to open this stream.
//
// This is a regression test: the stream previously embedded the protocol event
// verbatim and shipped every token in plaintext.
func TestEventStreamNeverDisclosesAFencingToken(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "board.db")
	handler, store, scheduler := newBoardTestServerWithScheduler(t, dbPath, nil)
	t.Cleanup(func() { _ = store.Close() })
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	created := createRealBoard(t, httpServer.URL, "sse-no-token", t.TempDir())
	boardID := created.Board.BoardID
	driveWithScheduler(t, store, scheduler, boardID)
	final := snapshotOf(t, handler, boardID)

	// Collect the real tokens straight from the durable aggregate, so the test
	// searches for the exact secrets rather than a guessed shape.
	board, err := store.Board(context.Background(), boardID)
	if err != nil {
		t.Fatal(err)
	}
	var tokens []string
	for _, attempt := range board.Attempts {
		if attempt.FencingToken != "" {
			tokens = append(tokens, attempt.FencingToken)
		}
	}
	if len(tokens) == 0 {
		t.Fatal("no attempt carried a fencing token; the test would prove nothing")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/v1/boards/"+boardID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	// Read the whole durable history, then stop.
	body := make([]byte, 0, 64*1024)
	buffer := make([]byte, 4096)
	for len(body) < 512*1024 {
		n, readErr := response.Body.Read(buffer)
		body = append(body, buffer[:n]...)
		if readErr != nil {
			break
		}
		if strings.Count(string(body), "\nid: ") >= int(final.LatestEventSequence)-1 {
			break
		}
	}
	stream := string(body)
	if !strings.Contains(stream, "lease.acquired") {
		t.Fatalf("the stream carried no lease.acquired frame, so the test would prove nothing: %d bytes", len(stream))
	}
	for _, token := range tokens {
		if strings.Contains(stream, token) {
			t.Fatalf("the event stream disclosed fencing token %q", token)
		}
	}
	if strings.Contains(stream, "fencing_token") {
		t.Fatal("the event stream carries a fencing_token field")
	}
	// The generation must still be published: it orders attempts without
	// authorizing anything.
	if !strings.Contains(stream, `"generation"`) {
		t.Fatal("the event stream dropped the attempt generation")
	}
}
