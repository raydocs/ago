package agobridge

import (
	"context"
	"encoding/json"
	"testing"
)

type largeResponseOperation struct{}

func (largeResponseOperation) Mutation() bool { return false }
func (largeResponseOperation) Execute(context.Context, ExecutionRequest) (json.RawMessage, error) {
	payload := append(json.RawMessage{'"'}, make([]byte, 430)...)
	for index := 1; index < len(payload); index++ {
		payload[index] = 'x'
	}
	payload = append(payload, '"')
	return payload, nil
}

func TestPreparedChangedDuplicateCompletesUnknownBeforeProgress(t *testing.T) {
	relay := newMemoryRelay(t)
	store := &memoryStateStore{failAt: 2}
	operation := &testOperation{mutation: true}
	publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
	config := testConfigWithStore(t, relay, publications, &testAuthorizer{}, store)
	original := request(1, "nonce-1", "project-1", "thread-1", `{}`)
	first, _ := New(context.Background(), config)
	if response := first.handle(context.Background(), original); response.Error == nil || response.Error.Code != ErrorStateUnavailable {
		t.Fatalf("initial response = %#v", response)
	}

	changed := original
	changed.Payload = json.RawMessage(`{"changed":true}`)
	reopened, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	conflict := reopened.handle(context.Background(), changed)
	if conflict.Error == nil || conflict.Error.Code != ErrorConflict || operation.calls.Load() != 1 {
		t.Fatalf("conflict = %#v, calls = %d", conflict, operation.calls.Load())
	}
	state, err := store.Load(context.Background(), BridgeIdentity{AccountID: "account-1", DeviceID: "device-1"})
	if err != nil {
		t.Fatal(err)
	}
	if state.Cursor != 1 || len(state.Evidence) != 1 || state.Evidence[0].Status != EvidenceCompleted ||
		state.Evidence[0].Response == nil || state.Evidence[0].Response.Error == nil || state.Evidence[0].Response.Error.Code != ErrorUnknownOutcome ||
		state.Pending == nil || state.Pending.Response.Error == nil || state.Pending.Response.Error.Code != ErrorConflict {
		t.Fatalf("durable state = %#v", state)
	}
	if err := reopened.accept(context.Background(), PollResult{AccountID: "account-1", DeviceID: "device-1", AcknowledgedThrough: 1}); err != nil {
		t.Fatal(err)
	}
	next := request(2, "nonce-2", "project-1", "thread-1", `{}`)
	if response := reopened.handle(context.Background(), next); response.Error != nil || operation.calls.Load() != 2 {
		t.Fatalf("next response = %#v, calls = %d", response, operation.calls.Load())
	}
}

func TestPendingResponseIsNotOverwrittenBeforeAcknowledgement(t *testing.T) {
	relay := newMemoryRelay(t)
	store := &memoryStateStore{}
	operation := &testOperation{}
	publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
	config := testConfigWithStore(t, relay, publications, nil, store)
	original := request(1, "nonce", "project-1", "thread-1", `{"original":true}`)
	client, _ := New(context.Background(), config)
	first := client.handle(context.Background(), original)
	changed := original
	changed.Payload = json.RawMessage(`{"changed":true}`)
	conflict := client.handle(context.Background(), changed)
	if conflict.Error == nil || conflict.Error.Code != ErrorConflict {
		t.Fatalf("conflict = %#v", conflict)
	}
	state, err := store.Load(context.Background(), BridgeIdentity{AccountID: "account-1", DeviceID: "device-1"})
	if err != nil {
		t.Fatal(err)
	}
	originalDigest := requestDigest(original)
	if state.Pending == nil || state.Pending.RequestDigest != hexDigest(originalDigest) || string(state.Pending.Response.Payload) != string(first.Payload) {
		t.Fatalf("pending was overwritten: %#v", state.Pending)
	}
}

func TestOversizedPollEnvelopeBecomesCompactFailure(t *testing.T) {
	relay := newMemoryRelay(t)
	store := &memoryStateStore{}
	operation := largeResponseOperation{}
	publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
	config := testConfigWithStore(t, relay, publications, nil, store)
	config.MaxBodyBytes = 512
	client, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	original := request(1, "nonce", "project-1", "thread-1", `{}`)
	response := client.handle(context.Background(), original)
	if response.Error == nil || response.Error.Code != ErrorExecutionFailed {
		t.Fatalf("response = %#v", response)
	}
	client.mu.Lock()
	poll := PollEnvelope{AccountID: "account-1", DeviceID: "device-1", Cursor: client.state.Cursor, Responses: []ResponseEnvelope{client.state.Pending.Response}}
	client.mu.Unlock()
	encoded, err := json.Marshal(poll)
	if err != nil || int64(len(encoded)) > config.MaxBodyBytes {
		t.Fatalf("poll size = %d, err = %v", len(encoded), err)
	}
}

func TestInvalidRequestIsDurablyProgressed(t *testing.T) {
	relay := newMemoryRelay(t)
	store := &memoryStateStore{}
	publications := &testPublications{operations: map[string]Operation{}}
	config := testConfigWithStore(t, relay, publications, nil, store)
	client, _ := New(context.Background(), config)
	invalid := request(1, "nonce", "project-1", "thread-1", `{}`)
	invalid.Payload = nil
	response := client.handle(context.Background(), invalid)
	if response.Error == nil || response.Error.Code != ErrorInvalidRequest {
		t.Fatalf("response = %#v", response)
	}
	state, err := store.Load(context.Background(), BridgeIdentity{AccountID: "account-1", DeviceID: "device-1"})
	if err != nil {
		t.Fatal(err)
	}
	if state.Cursor != 1 || state.Pending == nil || len(state.Evidence) != 1 || state.Evidence[0].Status != EvidenceCompleted {
		t.Fatalf("state = %#v", state)
	}
}

func TestNewRejectsPendingThatDiffersFromCompletedEvidence(t *testing.T) {
	relay := newMemoryRelay(t)
	store := &memoryStateStore{}
	publications := &testPublications{operations: map[string]Operation{}}
	original := request(1, "nonce", "project-1", "thread-1", `{}`)
	digest := requestDigest(original)
	completed := ResponseEnvelope{Sequence: 1, Nonce: "nonce", AccountID: "account-1", DeviceID: "device-1", Payload: json.RawMessage(`{"ok":true}`)}
	different := completed
	different.Payload = json.RawMessage(`{"forged":true}`)
	state := State{
		Cursor:   1,
		Evidence: []Evidence{{Sequence: 1, Nonce: "nonce", RequestDigest: hexDigest(digest), Status: EvidenceCompleted, Response: &completed}},
		Pending:  &PendingResponse{RequestDigest: hexDigest(digest), Response: different},
	}
	if _, err := store.Commit(context.Background(), BridgeIdentity{AccountID: "account-1", DeviceID: "device-1"}, 0, state); err != nil {
		t.Fatal(err)
	}
	if _, err := New(context.Background(), testConfigWithStore(t, relay, publications, nil, store)); err == nil {
		t.Fatal("New accepted pending response different from completed evidence")
	}
}

func hexDigest(digest [32]byte) string {
	const digits = "0123456789abcdef"
	encoded := make([]byte, 64)
	for index, value := range digest {
		encoded[index*2] = digits[value>>4]
		encoded[index*2+1] = digits[value&15]
	}
	return string(encoded)
}
