package agobridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type testOperation struct {
	mutation bool
	calls    atomic.Int32
	mu       sync.Mutex
	last     ExecutionRequest
}

func (operation *testOperation) Mutation() bool { return operation.mutation }
func (operation *testOperation) Execute(_ context.Context, request ExecutionRequest) (json.RawMessage, error) {
	operation.calls.Add(1)
	operation.mu.Lock()
	operation.last = request
	operation.mu.Unlock()
	return append(json.RawMessage(nil), request.Payload...), nil
}

func (operation *testOperation) lastRequest() ExecutionRequest {
	operation.mu.Lock()
	defer operation.mu.Unlock()
	return operation.last
}

type memoryStateStore struct {
	mu          sync.Mutex
	revision    uint64
	state       State
	commitCalls int
	failAt      int
}

func (store *memoryStateStore) Load(context.Context, BridgeIdentity) (State, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return cloneState(store.state), nil
}

func (store *memoryStateStore) Commit(_ context.Context, _ BridgeIdentity, expectedRevision uint64, state State) (uint64, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.commitCalls++
	if store.commitCalls == store.failAt {
		return store.revision, errors.New("injected durable commit failure")
	}
	if expectedRevision != store.revision {
		return store.revision, ErrStateConflict
	}
	store.revision++
	state.Revision = store.revision
	store.state = cloneState(state)
	return store.revision, nil
}

type testPublications struct {
	mu         sync.RWMutex
	operations map[string]Operation
}

func (publications *testPublications) ResolvePublished(_ context.Context, projectID, threadID, action string) (Operation, bool) {
	publications.mu.RLock()
	defer publications.mu.RUnlock()
	operation, ok := publications.operations[projectID+"\x00"+threadID+"\x00"+action]
	return operation, ok
}

type testAuthorizer struct {
	err   error
	calls atomic.Int32
}

func (authorizer *testAuthorizer) AuthorizeMutation(context.Context, MutationAuthorization) error {
	authorizer.calls.Add(1)
	return authorizer.err
}

type memoryRelay struct {
	server *httptest.Server

	mu               sync.Mutex
	requests         []RequestEnvelope
	responses        map[uint64]ResponseEnvelope
	disconnectPolls  int
	disconnectAccept int
}

func newMemoryRelay(t *testing.T) *memoryRelay {
	t.Helper()
	relay := &memoryRelay{responses: make(map[uint64]ResponseEnvelope)}
	relay.server = httptest.NewTLSServer(http.HandlerFunc(relay.serveHTTP))
	t.Cleanup(relay.server.Close)
	return relay
}

func (relay *memoryRelay) URL() string { return relay.server.URL }

func (relay *memoryRelay) Pin(t *testing.T) string {
	t.Helper()
	digest := sha256.Sum256(relay.server.Certificate().Raw)
	return hex.EncodeToString(digest[:])
}

func (relay *memoryRelay) enqueue(request RequestEnvelope) {
	relay.mu.Lock()
	relay.requests = append(relay.requests, request)
	relay.mu.Unlock()
}

func (relay *memoryRelay) response(sequence uint64) (ResponseEnvelope, bool) {
	relay.mu.Lock()
	defer relay.mu.Unlock()
	response, ok := relay.responses[sequence]
	return response, ok
}

func (relay *memoryRelay) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || request.URL.Path != "/v1/bridge/poll" || request.Header.Get("Authorization") != "Bearer relay-token" {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	var poll PollEnvelope
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&poll); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}
	relay.mu.Lock()
	if relay.disconnectPolls > 0 {
		relay.disconnectPolls--
		relay.mu.Unlock()
		panic(http.ErrAbortHandler)
	}
	for _, response := range poll.Responses {
		if relay.disconnectAccept > 0 {
			relay.disconnectAccept--
			relay.mu.Unlock()
			panic(http.ErrAbortHandler)
		}
		relay.responses[response.Sequence] = response
	}
	result := PollResult{AccountID: poll.AccountID, DeviceID: poll.DeviceID, AcknowledgedThrough: poll.Cursor}
	for _, candidate := range relay.requests {
		if candidate.Sequence > poll.Cursor {
			result.Requests = append(result.Requests, candidate)
			break
		}
	}
	relay.mu.Unlock()
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(result)
}

func testConfig(t *testing.T, relay *memoryRelay, publications Publications, authorizer Authorization) Config {
	return testConfigWithStore(t, relay, publications, authorizer, &memoryStateStore{})
}

func testConfigWithStore(t *testing.T, relay *memoryRelay, publications Publications, authorizer Authorization, store StateStore) Config {
	t.Helper()
	return Config{
		RelayURL:       relay.URL(),
		CertificatePin: relay.Pin(t),
		BearerToken:    "relay-token",
		AccountID:      "account-1",
		DeviceID:       "device-1",
		AllowedProjects: map[string]struct{}{
			"project-1": {},
		},
		Publications:   publications,
		Authorization:  authorizer,
		StateStore:     store,
		PollTimeout:    50 * time.Millisecond,
		RequestTimeout: 250 * time.Millisecond,
		MinBackoff:     time.Millisecond,
		MaxBackoff:     5 * time.Millisecond,
		MaxBodyBytes:   64 << 10,
	}
}

func request(sequence uint64, nonce, projectID, threadID string, payload string) RequestEnvelope {
	return RequestEnvelope{
		Sequence: sequence, Nonce: nonce, AccountID: "account-1", DeviceID: "device-1",
		ProjectID: projectID, ThreadID: threadID, Action: "read", Payload: json.RawMessage(payload),
	}
}

func runUntilResponse(t *testing.T, client *Client, relay *memoryRelay, sequence uint64) ResponseEnvelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	for {
		if response, ok := relay.response(sequence); ok {
			cancel()
			<-done
			return response
		}
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for response")
		case <-time.After(time.Millisecond):
		}
	}
}

func TestDisconnectReconnectResumesWithoutDuplicateExecution(t *testing.T) {
	relay := newMemoryRelay(t)
	operation := &testOperation{}
	publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
	relay.disconnectPolls = 1
	relay.disconnectAccept = 1
	relay.enqueue(request(1, "nonce-1", "project-1", "thread-1", `{"value":1}`))

	client, err := New(context.Background(), testConfig(t, relay, publications, nil))
	if err != nil {
		t.Fatal(err)
	}
	response := runUntilResponse(t, client, relay, 1)
	if response.Error != nil || operation.calls.Load() != 1 || response.Sequence != 1 {
		t.Fatalf("response = %#v, calls = %d", response, operation.calls.Load())
	}
}

func TestReplayNonceIsRejected(t *testing.T) {
	relay := newMemoryRelay(t)
	operation := &testOperation{}
	publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
	relay.enqueue(request(1, "same-nonce", "project-1", "thread-1", `{}`))
	relay.enqueue(request(2, "same-nonce", "project-1", "thread-1", `{}`))
	client, _ := New(context.Background(), testConfig(t, relay, publications, nil))
	first := runUntilResponse(t, client, relay, 1)
	if first.Error != nil {
		t.Fatalf("first response = %#v", first)
	}
	second := runUntilResponse(t, client, relay, 2)
	if second.Error == nil || second.Error.Code != ErrorReplay || operation.calls.Load() != 1 {
		t.Fatalf("second response = %#v, calls = %d", second, operation.calls.Load())
	}
}

func TestUnauthorizedThreadAndProjectAreRejected(t *testing.T) {
	for _, test := range []struct {
		name      string
		projectID string
		threadID  string
	}{
		{name: "project", projectID: "project-2", threadID: "thread-1"},
		{name: "thread", projectID: "project-1", threadID: "thread-2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			relay := newMemoryRelay(t)
			operation := &testOperation{}
			publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
			relay.enqueue(request(1, "nonce", test.projectID, test.threadID, `{}`))
			client, _ := New(context.Background(), testConfig(t, relay, publications, nil))
			response := runUntilResponse(t, client, relay, 1)
			if response.Error == nil || response.Error.Code != ErrorUnauthorized || operation.calls.Load() != 0 {
				t.Fatalf("response = %#v, calls = %d", response, operation.calls.Load())
			}
		})
	}
}

func TestDuplicateExactRequestReturnsCachedResponse(t *testing.T) {
	relay := newMemoryRelay(t)
	operation := &testOperation{}
	publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
	client, _ := New(context.Background(), testConfig(t, relay, publications, nil))
	original := request(1, "nonce", "project-1", "thread-1", `{"one":1}`)
	first := client.handle(context.Background(), original)
	second := client.handle(context.Background(), original)
	if first.Error != nil || second.Error != nil || string(first.Payload) != string(second.Payload) || operation.calls.Load() != 1 {
		t.Fatalf("first = %#v, second = %#v, calls = %d", first, second, operation.calls.Load())
	}
}

func TestConcurrentDuplicateExecutesOnce(t *testing.T) {
	relay := newMemoryRelay(t)
	operation := &testOperation{}
	publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
	client, _ := New(context.Background(), testConfig(t, relay, publications, nil))
	original := request(1, "nonce", "project-1", "thread-1", `{"one":1}`)

	const callers = 16
	responses := make(chan ResponseEnvelope, callers)
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			responses <- client.handle(context.Background(), original)
		}()
	}
	group.Wait()
	close(responses)
	for response := range responses {
		if response.Error != nil || string(response.Payload) != `{"one":1}` {
			t.Fatalf("response = %#v", response)
		}
	}
	if operation.calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", operation.calls.Load())
	}
}

func TestChangedDuplicateConflicts(t *testing.T) {
	relay := newMemoryRelay(t)
	operation := &testOperation{}
	publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
	client, _ := New(context.Background(), testConfig(t, relay, publications, nil))
	original := request(1, "nonce", "project-1", "thread-1", `{"one":1}`)
	changed := original
	changed.Payload = json.RawMessage(`{"one":2}`)
	_ = client.handle(context.Background(), original)
	response := client.handle(context.Background(), changed)
	if response.Error == nil || response.Error.Code != ErrorConflict || operation.calls.Load() != 1 {
		t.Fatalf("response = %#v, calls = %d", response, operation.calls.Load())
	}
}

func TestReopenReturnsDurableResponseWithoutExecution(t *testing.T) {
	relay := newMemoryRelay(t)
	store := &memoryStateStore{}
	operation := &testOperation{}
	publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
	config := testConfigWithStore(t, relay, publications, nil, store)
	original := request(1, "nonce", "project-1", "thread-1", `{"one":1}`)

	firstClient, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	first := firstClient.handle(context.Background(), original)
	if first.Error != nil || operation.calls.Load() != 1 {
		t.Fatalf("first = %#v, calls = %d", first, operation.calls.Load())
	}
	secondClient, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	second := secondClient.handle(context.Background(), original)
	if second.Error != nil || string(second.Payload) != string(first.Payload) || operation.calls.Load() != 1 {
		t.Fatalf("second = %#v, calls = %d", second, operation.calls.Load())
	}
}

func TestReopenChangedDuplicateConflicts(t *testing.T) {
	relay := newMemoryRelay(t)
	store := &memoryStateStore{}
	operation := &testOperation{}
	publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
	config := testConfigWithStore(t, relay, publications, nil, store)
	original := request(1, "nonce", "project-1", "thread-1", `{"one":1}`)
	firstClient, _ := New(context.Background(), config)
	_ = firstClient.handle(context.Background(), original)

	changed := original
	changed.Payload = json.RawMessage(`{"one":2}`)
	secondClient, _ := New(context.Background(), config)
	response := secondClient.handle(context.Background(), changed)
	if response.Error == nil || response.Error.Code != ErrorConflict || operation.calls.Load() != 1 {
		t.Fatalf("response = %#v, calls = %d", response, operation.calls.Load())
	}
}

func TestReopenPreparedOutcomeFailsClosed(t *testing.T) {
	relay := newMemoryRelay(t)
	store := &memoryStateStore{failAt: 2}
	operation := &testOperation{mutation: true}
	publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
	config := testConfigWithStore(t, relay, publications, &testAuthorizer{}, store)
	original := request(1, "nonce", "project-1", "thread-1", `{}`)
	firstClient, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	failedCompletion := firstClient.handle(context.Background(), original)
	if failedCompletion.Error == nil || failedCompletion.Error.Code != ErrorStateUnavailable || operation.calls.Load() != 1 {
		t.Fatalf("failed completion = %#v, calls = %d", failedCompletion, operation.calls.Load())
	}

	client, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	response := client.handle(context.Background(), original)
	if response.Error == nil || response.Error.Code != ErrorUnknownOutcome || operation.calls.Load() != 1 {
		t.Fatalf("response = %#v, calls = %d", response, operation.calls.Load())
	}
	changed := original
	changed.Payload = json.RawMessage(`{"changed":true}`)
	conflict := client.handle(context.Background(), changed)
	if conflict.Error == nil || conflict.Error.Code != ErrorConflict || operation.calls.Load() != 1 {
		t.Fatalf("conflict = %#v, calls = %d", conflict, operation.calls.Load())
	}
}

func TestMutationAuthorizationFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name       string
		authorizer Authorization
	}{
		{name: "missing"},
		{name: "denied", authorizer: &testAuthorizer{err: errors.New("passkey required")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			relay := newMemoryRelay(t)
			operation := &testOperation{mutation: true}
			publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
			client, _ := New(context.Background(), testConfig(t, relay, publications, test.authorizer))
			response := client.handle(context.Background(), request(1, "nonce", "project-1", "thread-1", `{}`))
			if response.Error == nil || response.Error.Code != ErrorAuthorizationRequired || operation.calls.Load() != 0 {
				t.Fatalf("response = %#v, calls = %d", response, operation.calls.Load())
			}
		})
	}
}

func TestAuthorizedMutationExecutes(t *testing.T) {
	relay := newMemoryRelay(t)
	operation := &testOperation{mutation: true}
	authorizer := &testAuthorizer{}
	publications := &testPublications{operations: map[string]Operation{"project-1\x00thread-1\x00read": operation}}
	client, _ := New(context.Background(), testConfig(t, relay, publications, authorizer))
	response := client.handle(context.Background(), request(1, "nonce", "project-1", "thread-1", `{}`))
	if response.Error != nil || operation.calls.Load() != 1 || authorizer.calls.Load() != 1 {
		t.Fatalf("response = %#v, operation calls = %d, authorization calls = %d", response, operation.calls.Load(), authorizer.calls.Load())
	}
	execution := operation.lastRequest()
	if execution.AccountID != "account-1" || execution.DeviceID != "device-1" || execution.ProjectID != "project-1" ||
		execution.ThreadID != "thread-1" || execution.Action != "read" || execution.Nonce != "nonce" || execution.Sequence != 1 || string(execution.Payload) != `{}` {
		t.Fatalf("execution identity = %#v", execution)
	}
}

func TestNewRequiresHTTPSAndMatchingCertificatePin(t *testing.T) {
	relay := newMemoryRelay(t)
	config := testConfig(t, relay, &testPublications{}, nil)
	config.RelayURL = "http://relay.example"
	if _, err := New(context.Background(), config); err == nil {
		t.Fatal("New accepted plaintext relay URL")
	}
	config.RelayURL = relay.URL()
	config.CertificatePin = "0000000000000000000000000000000000000000000000000000000000000000"
	client, err := New(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := client.Run(ctx); err == nil {
		t.Fatal("Run accepted mismatched certificate pin")
	}
}
