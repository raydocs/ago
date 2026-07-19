package agorelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"claudexflow/internal/agobridge"
)

func TestRelayEnqueuePollResultAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	store := openRelayStore(t, path)
	provisionPair(t, store)
	server := httptest.NewTLSServer(NewServer(store, ServerConfig{MaxBodyBytes: 64 << 10}).Handler())
	t.Cleanup(server.Close)

	enqueued := browserJSON(t, server, http.MethodPost, "/v1/relay/requests", "browser-token", map[string]any{
		"nonce": "nonce-1", "project_id": "project-1", "thread_id": "T-one", "action": ActionProjection,
		"payload": map[string]any{"after_sequence": 0, "limit": 10},
	})
	if enqueued.StatusCode != http.StatusAccepted {
		t.Fatalf("enqueue status = %d", enqueued.StatusCode)
	}
	var accepted EnqueueResult
	decodeBody(t, enqueued, &accepted)
	if accepted.Sequence != 1 {
		t.Fatalf("accepted = %#v", accepted)
	}

	poll := daemonPoll(t, server, agobridge.PollEnvelope{AccountID: "account-1", DeviceID: "device-1", Cursor: 0})
	if len(poll.Requests) != 1 || poll.Requests[0].Sequence != 1 || poll.Requests[0].Action != ActionProjection {
		t.Fatalf("poll = %#v", poll)
	}
	response := agobridge.ResponseEnvelope{Sequence: 1, Nonce: "nonce-1", AccountID: "account-1", DeviceID: "device-1", Payload: json.RawMessage(`{"ok":true}`)}
	ack := daemonPoll(t, server, agobridge.PollEnvelope{AccountID: "account-1", DeviceID: "device-1", Cursor: 1, Responses: []agobridge.ResponseEnvelope{response}})
	if ack.AcknowledgedThrough != 1 {
		t.Fatalf("ack = %#v", ack)
	}
	result := browserJSON(t, server, http.MethodGet, "/v1/relay/results?sequence=1", "browser-token", nil)
	if result.StatusCode != http.StatusOK {
		t.Fatalf("result status = %d", result.StatusCode)
	}
	var got agobridge.ResponseEnvelope
	decodeBody(t, result, &got)
	if string(got.Payload) != `{"ok":true}` {
		t.Fatalf("result = %#v", got)
	}

	server.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openRelayStore(t, path)
	defer reopened.Close()
	browser, err := reopened.Authenticate(context.Background(), "browser-token", RoleBrowser)
	if err != nil {
		t.Fatal(err)
	}
	state, err := reopened.Result(context.Background(), browser, 1)
	if err != nil || state.Response == nil || string(state.Response.Payload) != `{"ok":true}` {
		t.Fatalf("reopened result = %#v, %v", state, err)
	}
}

func TestRelayConcurrentEnqueueHasMonotonicUniqueSequences(t *testing.T) {
	store := openRelayStore(t, filepath.Join(t.TempDir(), "relay.db"))
	defer store.Close()
	provisionPair(t, store)
	const count = 32
	results := make(chan uint64, count)
	var group sync.WaitGroup
	for index := range count {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			result, err := store.Enqueue(context.Background(), Principal{AccountID: "account-1", DeviceID: "device-1", Role: RoleBrowser, Projects: map[string]struct{}{"project-1": {}}}, EnqueueRequest{
				Nonce: fmt.Sprintf("nonce-%d", index), ProjectID: "project-1", ThreadID: "T-one", Action: ActionProjection, Payload: json.RawMessage(`{}`),
			})
			if err != nil {
				t.Errorf("Enqueue() error = %v", err)
				return
			}
			results <- result.Sequence
		}(index)
	}
	group.Wait()
	close(results)
	seen := make(map[uint64]bool)
	for sequence := range results {
		seen[sequence] = true
	}
	for sequence := uint64(1); sequence <= count; sequence++ {
		if !seen[sequence] {
			t.Fatalf("missing sequence %d in %#v", sequence, seen)
		}
	}
}

func TestRelayRejectsUnauthorizedBindingsActionsReplayAndChangedResponse(t *testing.T) {
	store := openRelayStore(t, filepath.Join(t.TempDir(), "relay.db"))
	defer store.Close()
	provisionPair(t, store)
	principal := Principal{AccountID: "account-1", DeviceID: "device-1", Role: RoleBrowser, Projects: map[string]struct{}{"project-1": {}}}
	base := EnqueueRequest{Nonce: "nonce", ProjectID: "project-1", ThreadID: "T-one", Action: ActionSubmit, Payload: json.RawMessage(`{}`)}
	first, err := store.Enqueue(context.Background(), principal, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Enqueue(context.Background(), principal, base); err != nil {
		t.Fatalf("exact enqueue retry = %v", err)
	}
	changed := base
	changed.Payload = json.RawMessage(`{"changed":true}`)
	if _, err := store.Enqueue(context.Background(), principal, changed); err == nil {
		t.Fatal("changed nonce replay was accepted")
	}
	badAction := base
	badAction.Nonce = "other"
	badAction.Action = "shell"
	if _, err := store.Enqueue(context.Background(), principal, badAction); err == nil {
		t.Fatal("arbitrary action was accepted")
	}
	response := agobridge.ResponseEnvelope{Sequence: first.Sequence, Nonce: base.Nonce, AccountID: "account-1", DeviceID: "device-1", Payload: json.RawMessage(`{}`)}
	if _, err := store.Poll(context.Background(), Principal{AccountID: "account-1", DeviceID: "device-1", Role: RoleDaemon}, agobridge.PollEnvelope{AccountID: "account-1", DeviceID: "device-1", Cursor: 1, Responses: []agobridge.ResponseEnvelope{response}}); err != nil {
		t.Fatal(err)
	}
	response.Payload = json.RawMessage(`{"changed":true}`)
	if _, err := store.Poll(context.Background(), Principal{AccountID: "account-1", DeviceID: "device-1", Role: RoleDaemon}, agobridge.PollEnvelope{AccountID: "account-1", DeviceID: "device-1", Cursor: 1, Responses: []agobridge.ResponseEnvelope{response}}); err == nil {
		t.Fatal("changed duplicate response was accepted")
	}
}

func TestCredentialRotationInvalidatesOldGeneration(t *testing.T) {
	store := openRelayStore(t, filepath.Join(t.TempDir(), "relay.db"))
	defer store.Close()
	if err := store.RotateCredential(context.Background(), Credential{AccountID: "a", DeviceID: "d", Role: RoleDaemon, Generation: 1, Token: "old", Projects: []string{"p"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(context.Background(), "old", RoleDaemon); err != nil {
		t.Fatal(err)
	}
	if err := store.RotateCredential(context.Background(), Credential{AccountID: "a", DeviceID: "d", Role: RoleDaemon, Generation: 2, Token: "new", Projects: []string{"p"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(context.Background(), "old", RoleDaemon); err == nil {
		t.Fatal("old credential generation remained active")
	}
	if principal, err := store.Authenticate(context.Background(), "new", RoleDaemon); err != nil || principal.Generation != 2 {
		t.Fatalf("new credential = %#v, %v", principal, err)
	}
}

func TestRelayAllowsOnlyDocumentedBridgeAuthenticationActions(t *testing.T) {
	store := openRelayStore(t, filepath.Join(t.TempDir(), "relay.db"))
	defer store.Close()
	provisionPair(t, store)
	principal, err := store.Authenticate(context.Background(), "browser-token", RoleBrowser)
	if err != nil {
		t.Fatal(err)
	}
	for index, action := range []string{"auth.challenge", "auth.assertion"} {
		_, err := store.Enqueue(context.Background(), principal, EnqueueRequest{
			Nonce: fmt.Sprintf("auth-%d", index), ProjectID: "project-1", ThreadID: "T-one", Action: action, Payload: json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatalf("action %q: %v", action, err)
		}
	}
	if _, err := store.Enqueue(context.Background(), principal, EnqueueRequest{
		Nonce: "registration", ProjectID: "project-1", ThreadID: "T-one", Action: "auth.registration", Payload: json.RawMessage(`{}`),
	}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("registration action = %v", err)
	}
}

func TestRelayRestartRetransmitsUnacknowledgedAndResponseRetryIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	store := openRelayStore(t, path)
	provisionPair(t, store)
	browser, _ := store.Authenticate(context.Background(), "browser-token", RoleBrowser)
	request := EnqueueRequest{Nonce: "durable-nonce", ProjectID: "project-1", ThreadID: "T-one", Action: ActionArchive, Payload: json.RawMessage(`{}`)}
	if _, err := store.Enqueue(context.Background(), browser, request); err != nil {
		t.Fatal(err)
	}
	daemon, _ := store.Authenticate(context.Background(), "daemon-token", RoleDaemon)
	first, err := store.Poll(context.Background(), daemon, agobridge.PollEnvelope{AccountID: "account-1", DeviceID: "device-1"})
	if err != nil || len(first.Requests) != 1 {
		t.Fatalf("first poll = %#v, %v", first, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openRelayStore(t, path)
	defer store.Close()
	daemon, _ = store.Authenticate(context.Background(), "daemon-token", RoleDaemon)
	retried, err := store.Poll(context.Background(), daemon, agobridge.PollEnvelope{AccountID: "account-1", DeviceID: "device-1"})
	if err != nil || len(retried.Requests) != 1 || retried.Requests[0].Nonce != request.Nonce {
		t.Fatalf("restart poll = %#v, %v", retried, err)
	}
	response := agobridge.ResponseEnvelope{Sequence: 1, Nonce: request.Nonce, AccountID: "account-1", DeviceID: "device-1", Payload: json.RawMessage(`{"archived":true}`)}
	poll := agobridge.PollEnvelope{AccountID: "account-1", DeviceID: "device-1", Cursor: 1, Responses: []agobridge.ResponseEnvelope{response}}
	if _, err := store.Poll(context.Background(), daemon, poll); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Poll(context.Background(), daemon, poll); err != nil {
		t.Fatalf("exact response retry: %v", err)
	}
	poll.Responses[0].Payload = json.RawMessage(`{"archived":false}`)
	if _, err := store.Poll(context.Background(), daemon, poll); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed response retry = %v", err)
	}
}

func TestRelayConcurrentStoresAssignMonotonicSequences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	first := openRelayStore(t, path)
	provisionPair(t, first)
	second := openRelayStore(t, path)
	defer first.Close()
	defer second.Close()
	principal, _ := first.Authenticate(context.Background(), "browser-token", RoleBrowser)
	const count = 20
	sequences := make(chan uint64, count)
	var group sync.WaitGroup
	for index := range count {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			store := first
			if index%2 != 0 {
				store = second
			}
			result, err := store.Enqueue(context.Background(), principal, EnqueueRequest{Nonce: fmt.Sprintf("cross-%d", index), ProjectID: "project-1", ThreadID: "T-one", Action: ActionProjection, Payload: json.RawMessage(`{}`)})
			if err != nil {
				t.Errorf("enqueue %d: %v", index, err)
				return
			}
			sequences <- result.Sequence
		}(index)
	}
	group.Wait()
	close(sequences)
	seen := map[uint64]bool{}
	for sequence := range sequences {
		seen[sequence] = true
	}
	for sequence := uint64(1); sequence <= count; sequence++ {
		if !seen[sequence] {
			t.Fatalf("missing sequence %d", sequence)
		}
	}
}

func TestRelayHTTPEnforcesTLSAuthenticationBindingsActionsAndBodyLimit(t *testing.T) {
	store := openRelayStore(t, filepath.Join(t.TempDir(), "relay.db"))
	defer store.Close()
	provisionPair(t, store)
	server := httptest.NewTLSServer(NewServer(store, ServerConfig{MaxBodyBytes: 4 << 10}).Handler())
	defer server.Close()

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/relay/requests", bytes.NewBufferString(`{"nonce":"n","project_id":"project-1","thread_id":"T","action":"thread.projection","payload":{}}`))
	response, err := server.Client().Do(request)
	if err != nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing auth = %v, %v", status(response), err)
	}
	response.Body.Close()

	for name, test := range map[string]struct {
		token    string
		body     string
		expected int
	}{
		"wrong role": {"daemon-token", `{"nonce":"n","project_id":"project-1","thread_id":"T","action":"thread.projection","payload":{}}`, http.StatusUnauthorized},
		"project":    {"browser-token", `{"nonce":"n","project_id":"other","thread_id":"T","action":"thread.projection","payload":{}}`, http.StatusForbidden},
		"action":     {"browser-token", `{"nonce":"n","project_id":"project-1","thread_id":"T","action":"shell","payload":{}}`, http.StatusBadRequest},
		"oversized":  {"browser-token", `{"padding":"` + strings.Repeat("x", 5000) + `"}`, http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/relay/requests", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+test.token)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != test.expected {
				content, _ := io.ReadAll(response.Body)
				t.Fatalf("status = %d, body=%s", response.StatusCode, content)
			}
		})
	}
}

func TestRelayHTTPAcceptsForwardedHTTPSOnlyFromTrustedProxy(t *testing.T) {
	_, network, _ := net.ParseCIDR("127.0.0.1/32")
	store := openRelayStore(t, filepath.Join(t.TempDir(), "relay.db"))
	defer store.Close()
	handler := NewServer(store, ServerConfig{TrustedProxies: []*net.IPNet{network}}).Handler()

	request := httptest.NewRequest(http.MethodGet, "http://relay/v1/relay/results?sequence=1", nil)
	request.Header.Set("X-Forwarded-Proto", "https")
	request.RemoteAddr = "127.0.0.1:1234"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("trusted forwarded HTTPS = %d", recorder.Code)
	}
	request.RemoteAddr = "192.0.2.1:1234"
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("untrusted forwarded HTTPS = %d", recorder.Code)
	}
	request.RemoteAddr = "127.0.0.1:1234"
	request.Header.Add("X-Forwarded-Proto", "http")
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUpgradeRequired {
		t.Fatalf("ambiguous forwarded HTTPS = %d", recorder.Code)
	}
}

func TestRelayStoreRejectsSymlinkAndPublicDatabase(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "link.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("Open accepted a symlink database")
	}
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(target); err == nil {
		t.Fatal("Open accepted a group/world-readable database")
	}
}

func status(response *http.Response) int {
	if response == nil {
		return 0
	}
	return response.StatusCode
}

func openRelayStore(t *testing.T, path string) *Store {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func provisionPair(t *testing.T, store *Store) {
	t.Helper()
	for _, credential := range []Credential{
		{AccountID: "account-1", DeviceID: "device-1", Role: RoleDaemon, Generation: 1, Token: "daemon-token", Projects: []string{"project-1"}},
		{AccountID: "account-1", DeviceID: "device-1", Role: RoleBrowser, Generation: 1, Token: "browser-token", Projects: []string{"project-1"}},
	} {
		if err := store.RotateCredential(context.Background(), credential); err != nil {
			t.Fatal(err)
		}
	}
}

func daemonPoll(t *testing.T, server *httptest.Server, poll agobridge.PollEnvelope) agobridge.PollResult {
	t.Helper()
	response := browserJSON(t, server, http.MethodPost, "/v1/bridge/poll", "daemon-token", poll)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("poll status = %d", response.StatusCode)
	}
	var result agobridge.PollResult
	decodeBody(t, response, &result)
	return result
}

func browserJSON(t *testing.T, server *httptest.Server, method, path, token string, body any) *http.Response {
	t.Helper()
	var encoded []byte
	if body != nil {
		encoded, _ = json.Marshal(body)
	}
	request, _ := http.NewRequest(method, server.URL+path, bytes.NewReader(encoded))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeBody(t *testing.T, response *http.Response, output any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(output); err != nil {
		t.Fatal(err)
	}
}
