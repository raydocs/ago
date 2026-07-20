package agoboardapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"claudexflow/internal/agoboardprotocol"
)

const (
	// maxEventBatch bounds how many durable events one SQLite read may return.
	// A slow client therefore holds at most one batch in flight rather than an
	// unbounded in-process queue; backpressure comes from the socket itself.
	maxEventBatch = 256

	// heartbeatInterval keeps idle connections and intermediaries alive without
	// inventing events. Heartbeats are SSE comments and carry no data.
	heartbeatInterval = 15 * time.Second

	// clientRetryMillis tells EventSource how soon to reconnect. Reconnection
	// is safe because the cursor is durable.
	clientRetryMillis = 2000
)

// streamedEvent is the durable protocol event plus the sequence a client echoes
// back in Last-Event-ID. Sequence equals the event's board version, which is the
// primary key of the append-only events table.
type streamedEvent struct {
	agoboardprotocol.Event
	Sequence uint64 `json:"sequence"`
}

// events streams a board's append-only history as server-sent events.
//
// Every delivered frame is read from SQLite. The in-memory notifier only wakes
// a waiting subscriber sooner than the poll interval would; it is never the
// authority for what gets delivered, so a subscriber that reconnects to a
// different process — or to a restarted one — sees the same sequence.
func (server *Server) events(writer http.ResponseWriter, request *http.Request) {
	boardID := request.PathValue("boardID")
	cursor, err := resumeCursor(request)
	if err != nil {
		writeError(writer, err)
		return
	}
	if _, err := server.store.Board(request.Context(), boardID); err != nil {
		writeError(writer, err)
		return
	}

	header := writer.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)

	controller := http.NewResponseController(writer)
	if _, err := fmt.Fprintf(writer, "retry: %d\n\n", clientRetryMillis); err != nil {
		return
	}
	_ = controller.Flush()

	ctx := request.Context()
	lastWrite := time.Now()
	for {
		// Register interest before reading so an event committed between the
		// read and the wait still wakes this subscriber.
		waiter := server.waiter(boardID)

		events, err := server.store.Replay(ctx, boardID, cursor, maxEventBatch)
		if err != nil {
			return
		}
		for _, event := range events {
			encoded, err := json.Marshal(streamedEvent{Event: event, Sequence: event.Version})
			if err != nil {
				return
			}
			// No "event:" field: browsers deliver unnamed frames to onmessage,
			// and the semantic type travels inside the payload.
			if _, err := fmt.Fprintf(writer, "id: %d\ndata: %s\n\n", event.Version, encoded); err != nil {
				return
			}
			cursor = event.Version
		}
		if len(events) > 0 {
			if err := controller.Flush(); err != nil {
				return
			}
			lastWrite = time.Now()
		}
		if len(events) == maxEventBatch {
			// A full batch means more durable history is already available;
			// drain it before waiting.
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-waiter:
		case <-time.After(server.pollInterval):
		}
		if time.Since(lastWrite) >= heartbeatInterval {
			if _, err := fmt.Fprint(writer, ": heartbeat\n\n"); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
			lastWrite = time.Now()
		}
	}
}

// resumeCursor reads the resume position from Last-Event-ID, falling back to
// the ?after= query parameter. The header wins so a browser's automatic
// reconnect cannot be overridden by a stale URL.
func resumeCursor(request *http.Request) (uint64, error) {
	raw := strings.TrimSpace(request.Header.Get("Last-Event-ID"))
	source := "Last-Event-ID"
	if raw == "" {
		raw = strings.TrimSpace(request.URL.Query().Get("after"))
		source = "after"
	}
	if raw == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, statusError{http.StatusBadRequest, "invalid_cursor", fmt.Sprintf("%s 必须是非负整数事件序号。", source), err}
	}
	return cursor, nil
}

// waiter returns a channel closed the next time this board commits an event.
// One channel serves every subscriber of a board, so no per-subscriber state
// can leak when a client disconnects.
func (server *Server) waiter(boardID string) <-chan struct{} {
	server.mu.Lock()
	defer server.mu.Unlock()
	channel, found := server.waiters[boardID]
	if !found {
		channel = make(chan struct{})
		server.waiters[boardID] = channel
	}
	return channel
}

// notify wakes every subscriber of a board after a command has been committed.
// Losing a notification only delays delivery until the next poll; it can never
// lose an event, because events are always re-read from SQLite.
func (server *Server) notify(boardID string) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if channel, found := server.waiters[boardID]; found {
		close(channel)
		delete(server.waiters, boardID)
	}
}
