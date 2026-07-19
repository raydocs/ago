package agodaemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"claudexflow/internal/agoauth"
	"claudexflow/internal/agobridge"
	"claudexflow/internal/agocoordinator"
	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agorelay"
	"claudexflow/internal/agothreadstore"
)

type bridgeNoopExecutor struct{}

func (bridgeNoopExecutor) Run(context.Context, agocoordinator.TurnRequest) error { return nil }

type bridgeBlockingExecutor struct{ release <-chan struct{} }

func (executor bridgeBlockingExecutor) Run(ctx context.Context, _ agocoordinator.TurnRequest) error {
	select {
	case <-executor.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestBridgePublicationsRequireExplicitRunningThreadAndAction(t *testing.T) {
	store, coordinator, threadID := bridgeFixture(t)
	publications, err := NewBridgePublications(store, coordinator, BridgePublicationConfig{Publications: []BridgePublication{{
		ProjectID: "project-1", ThreadID: threadID, Actions: []string{BridgeActionProjection},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if operation, ok := publications.ResolvePublished(context.Background(), "project-1", threadID, BridgeActionProjection); !ok || operation.Mutation() {
		t.Fatalf("projection operation = %#v, %v", operation, ok)
	}
	for _, denied := range []struct{ project, thread, action string }{
		{"other", threadID, BridgeActionProjection},
		{"project-1", "T-other", BridgeActionProjection},
		{"project-1", threadID, BridgeActionSubmit},
		{"project-1", threadID, "shell"},
	} {
		if _, ok := publications.ResolvePublished(context.Background(), denied.project, denied.thread, denied.action); ok {
			t.Fatalf("resolved unauthorized publication %#v", denied)
		}
	}
}

func TestBridgePublicationsAcceptAuthenticationActions(t *testing.T) {
	store, coordinator, threadID := bridgeFixture(t)
	core, _, _ := bridgeAuthCore(t, time.Minute, time.Minute)
	_, err := newBridgePublications(store, coordinator, BridgePublicationConfig{Publications: []BridgePublication{{
		ProjectID: "project-1", ThreadID: threadID, Actions: []string{"auth.challenge", "auth.assertion"},
	}}}, newBridgeAuthTransport(core))
	if err != nil {
		t.Fatalf("authentication publications: %v", err)
	}
}

func TestBridgeAuthenticationActionsFailStartupWithoutAuthority(t *testing.T) {
	store, coordinator, threadID := bridgeFixture(t)
	_, err := NewBridgePublications(store, coordinator, BridgePublicationConfig{Publications: []BridgePublication{{
		ProjectID: "project-1", ThreadID: threadID, Actions: []string{BridgeActionAuthChallenge},
	}}})
	if err == nil {
		t.Fatal("authentication publication started without a recent-passkey authority")
	}
}

func TestBridgeAuthenticationAssertionReturnsSingleUseBoundGrant(t *testing.T) {
	store, coordinator, threadID := bridgeFixture(t)
	core, clock, credentialID := bridgeAuthCore(t, time.Minute, time.Minute)
	transport := newBridgeAuthTransport(core)
	publications, err := newBridgePublications(store, coordinator, BridgePublicationConfig{Publications: []BridgePublication{{
		ProjectID: "project-1", ThreadID: threadID, Actions: []string{BridgeActionAuthChallenge, BridgeActionAuthAssertion, BridgeActionSubmit},
	}}}, transport)
	if err != nil {
		t.Fatal(err)
	}
	binding := agobridge.ExecutionRequest{AccountID: "account-1", DeviceID: "device-1", ProjectID: "project-1", ThreadID: threadID}
	challenge := executeBridgeChallenge(t, publications, binding)
	assertion := bridgeAssertionPayload(t, challenge.Challenge, credentialID, 1)
	assertionOperation, ok := publications.ResolvePublished(context.Background(), "project-1", threadID, BridgeActionAuthAssertion)
	if !ok || assertionOperation.Mutation() {
		t.Fatal("assertion operation was not a published non-mutation")
	}
	binding.Action, binding.Payload = BridgeActionAuthAssertion, assertion
	payload, err := assertionOperation.Execute(context.Background(), binding)
	if err != nil {
		t.Fatal(err)
	}
	var grant BridgeAuthAssertionResponse
	if err := json.Unmarshal(payload, &grant); err != nil || grant.AuthorizationToken == "" {
		t.Fatalf("grant = %#v, %v", grant, err)
	}
	authorization := RecentPasskeyAuthorization{Core: core, transport: transport}
	mutation := agobridge.MutationAuthorization{
		AccountID: "account-1", DeviceID: "device-1", ProjectID: "project-1", ThreadID: threadID,
		Action: BridgeActionSubmit, AuthorizationToken: grant.AuthorizationToken,
	}
	if err := authorization.AuthorizeMutation(context.Background(), mutation); err != nil {
		t.Fatalf("valid grant: %v", err)
	}
	if err := authorization.AuthorizeMutation(context.Background(), mutation); err == nil {
		t.Fatal("single-use grant was reused")
	}
	if _, err := assertionOperation.Execute(context.Background(), binding); err == nil {
		t.Fatal("assertion challenge replay succeeded")
	}

	expiring := executeBridgeChallenge(t, publications, binding)
	clock.Advance(2 * time.Minute)
	binding.Payload = bridgeAssertionPayload(t, expiring.Challenge, credentialID, 2)
	if _, err := assertionOperation.Execute(context.Background(), binding); err == nil {
		t.Fatal("expired challenge assertion succeeded")
	}
}

func TestBridgeAuthenticationAssertionRejectsWrongBinding(t *testing.T) {
	store, coordinator, threadID := bridgeFixture(t)
	core, _, credentialID := bridgeAuthCore(t, time.Minute, time.Minute)
	transport := newBridgeAuthTransport(core)
	publications, err := newBridgePublications(store, coordinator, BridgePublicationConfig{Publications: []BridgePublication{{
		ProjectID: "project-1", ThreadID: threadID, Actions: []string{BridgeActionAuthChallenge, BridgeActionAuthAssertion},
	}}}, transport)
	if err != nil {
		t.Fatal(err)
	}
	request := agobridge.ExecutionRequest{AccountID: "account-1", DeviceID: "device-1", ProjectID: "project-1", ThreadID: threadID}
	challenge := executeBridgeChallenge(t, publications, request)
	request.DeviceID = "other-device"
	request.Action = BridgeActionAuthAssertion
	request.Payload = bridgeAssertionPayload(t, challenge.Challenge, credentialID, 1)
	operation, _ := publications.ResolvePublished(context.Background(), "project-1", threadID, BridgeActionAuthAssertion)
	if _, err := operation.Execute(context.Background(), request); err == nil {
		t.Fatal("wrong-binding assertion succeeded")
	}
}

func TestOutboundBridgeTLSRecentPasskeyGrantGatesMutation(t *testing.T) {
	releaseExecutor := make(chan struct{})
	defer close(releaseExecutor)
	store, coordinator, threadID := bridgeFixtureWithExecutor(t, bridgeBlockingExecutor{release: releaseExecutor})
	relayStore, err := agorelay.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer relayStore.Close()
	for _, credential := range []agorelay.Credential{
		{AccountID: "account-1", DeviceID: "device-1", Role: agorelay.RoleDaemon, Generation: 1, Token: "daemon-token", Projects: []string{"project-1"}},
		{AccountID: "account-1", DeviceID: "device-1", Role: agorelay.RoleBrowser, Generation: 1, Token: "browser-token", Projects: []string{"project-1"}},
	} {
		if err := relayStore.RotateCredential(context.Background(), credential); err != nil {
			t.Fatal(err)
		}
	}
	relayServer := httptest.NewTLSServer(agorelay.NewServer(relayStore, agorelay.ServerConfig{MaxPollTimeout: 20 * time.Millisecond}).Handler())
	defer relayServer.Close()
	pin := sha256.Sum256(relayServer.Certificate().Raw)

	clock := &bridgeTestClock{now: time.Unix(1_700_000_000, 0)}
	credentialDirectory := t.TempDir()
	if err := os.Chmod(credentialDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	credentialStore, err := agoauth.NewFileCredentialPersistence(filepath.Join(credentialDirectory, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer credentialStore.Close()
	core, err := agoauth.New(agoauth.Config{
		RelyingParties: map[string][]string{"example.com": {"https://app.example.com"}}, ChallengeTTL: time.Minute,
		GrantTTL: time.Minute, ChallengeBytes: 32, MaxChallenges: 32, MaxGrants: 32, RequireUserVerification: true,
	}, agoauth.Dependencies{Clock: clock, Random: rand.Reader, Verifier: acceptingBridgeVerifier{}, Persistence: credentialStore})
	if err != nil {
		t.Fatal(err)
	}
	credentialID := base64.RawURLEncoding.EncodeToString([]byte("credential-1"))
	if err := core.RegisterCredential(agoauth.Credential{
		ID: credentialID, RPID: "example.com", ActorID: "account-1", DeviceID: "device-1", PublicKey: []byte("operator-key"),
	}); err != nil {
		t.Fatal(err)
	}
	stateStore, err := agobridge.NewFileStateStore(filepath.Join(t.TempDir(), "bridge-state"))
	if err != nil {
		t.Fatal(err)
	}
	bridge, err := StartOutboundBridge(context.Background(), OutboundBridgeConfig{
		Client: agobridge.Config{
			RelayURL: relayServer.URL, CertificatePin: hex.EncodeToString(pin[:]), BearerToken: "daemon-token",
			AccountID: "account-1", DeviceID: "device-1", AllowedProjects: map[string]struct{}{"project-1": {}}, StateStore: stateStore,
			PollTimeout: 20 * time.Millisecond, RequestTimeout: time.Second, ExecutionTimeout: time.Second,
			MinBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond, MaxBodyBytes: 1 << 20,
		},
		Publications: BridgePublicationConfig{Publications: []BridgePublication{{
			ProjectID: "project-1", ThreadID: threadID,
			Actions: []string{BridgeActionAuthChallenge, BridgeActionAuthAssertion, BridgeActionSubmit},
		}}},
		Store: store, Coordinator: coordinator, Authorization: RecentPasskeyAuthorization{Core: core}, Closer: stateStore,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := bridge.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	browser, err := relayStore.Authenticate(context.Background(), "browser-token", agorelay.RoleBrowser)
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := store.Mailbox(context.Background(), threadID)
	if err != nil {
		t.Fatal(err)
	}
	submitPayload, _ := json.Marshal(map[string]any{"expected_sequence": mailbox.LastSequence, "content": map[string]any{"text": "remote"}, "class": "normal"})
	missing := relayBridgeRequest(t, relayStore, browser, threadID, agorelay.ActionSubmit, "tls-missing", "", submitPayload)
	if missing.Error == nil || missing.Error.Code != agobridge.ErrorAuthorizationRequired {
		t.Fatalf("missing grant response = %#v", missing.Error)
	}

	challengeResponse := relayBridgeRequest(t, relayStore, browser, threadID, agorelay.ActionAuthChallenge, "tls-challenge", "", json.RawMessage(`{"rp_id":"example.com"}`))
	if challengeResponse.Error != nil {
		t.Fatalf("challenge response = %#v", challengeResponse.Error)
	}
	var challenge BridgeAuthChallengeResponse
	if err := json.Unmarshal(challengeResponse.Payload, &challenge); err != nil {
		t.Fatal(err)
	}
	assertionResponse := relayBridgeRequest(t, relayStore, browser, threadID, agorelay.ActionAuthAssertion, "tls-assertion", "", bridgeAssertionPayload(t, challenge.Challenge, credentialID, 1))
	if assertionResponse.Error != nil {
		t.Fatalf("assertion response = %#v", assertionResponse.Error)
	}
	var grant BridgeAuthAssertionResponse
	if err := json.Unmarshal(assertionResponse.Payload, &grant); err != nil {
		t.Fatal(err)
	}
	replayed := relayBridgeRequest(t, relayStore, browser, threadID, agorelay.ActionAuthAssertion, "tls-assertion-replay", "", bridgeAssertionPayload(t, challenge.Challenge, credentialID, 1))
	if replayed.Error == nil || replayed.Error.Code != agobridge.ErrorExecutionFailed {
		t.Fatalf("replayed assertion response = %#v", replayed.Error)
	}
	clock.Advance(2 * time.Minute)
	expired := relayBridgeRequest(t, relayStore, browser, threadID, agorelay.ActionSubmit, "tls-expired", grant.AuthorizationToken, submitPayload)
	if expired.Error == nil || expired.Error.Code != agobridge.ErrorAuthorizationRequired {
		t.Fatalf("expired grant response = %#v", expired.Error)
	}
	secondChallengeResponse := relayBridgeRequest(t, relayStore, browser, threadID, agorelay.ActionAuthChallenge, "tls-challenge-2", "", json.RawMessage(`{"rp_id":"example.com"}`))
	var secondChallenge BridgeAuthChallengeResponse
	if secondChallengeResponse.Error != nil || json.Unmarshal(secondChallengeResponse.Payload, &secondChallenge) != nil {
		t.Fatalf("second challenge response = %#v", secondChallengeResponse)
	}
	secondAssertion := relayBridgeRequest(t, relayStore, browser, threadID, agorelay.ActionAuthAssertion, "tls-assertion-2", "", bridgeAssertionPayload(t, secondChallenge.Challenge, credentialID, 2))
	if secondAssertion.Error != nil || json.Unmarshal(secondAssertion.Payload, &grant) != nil {
		t.Fatalf("second assertion response = %#v", secondAssertion)
	}
	mutation := relayBridgeRequest(t, relayStore, browser, threadID, agorelay.ActionSubmit, "tls-submit", grant.AuthorizationToken, submitPayload)
	if mutation.Error != nil {
		t.Fatalf("authorized mutation = %#v", mutation.Error)
	}
	reused := relayBridgeRequest(t, relayStore, browser, threadID, agorelay.ActionSubmit, "tls-reused", grant.AuthorizationToken, submitPayload)
	if reused.Error == nil || reused.Error.Code != agobridge.ErrorAuthorizationRequired {
		t.Fatalf("reused grant response = %#v", reused.Error)
	}
}

func relayBridgeRequest(t *testing.T, store *agorelay.Store, principal agorelay.Principal, threadID, action, nonce, token string, payload json.RawMessage) agobridge.ResponseEnvelope {
	t.Helper()
	enqueued, err := store.Enqueue(context.Background(), principal, agorelay.EnqueueRequest{
		Nonce: nonce, ProjectID: "project-1", ThreadID: threadID, Action: action, AuthorizationToken: token, Payload: payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		result, err := store.Result(context.Background(), principal, enqueued.Sequence)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Pending && result.Response != nil {
			return *result.Response
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("relay result %d remained pending", enqueued.Sequence)
	return agobridge.ResponseEnvelope{}
}

type bridgeTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *bridgeTestClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *bridgeTestClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	clock.now = clock.now.Add(duration)
	clock.mu.Unlock()
}

type acceptingBridgeVerifier struct{}

func (acceptingBridgeVerifier) Verify(_, _, _ []byte) error { return nil }

func bridgeAuthCore(t *testing.T, challengeTTL, grantTTL time.Duration) (*agoauth.Core, *bridgeTestClock, string) {
	t.Helper()
	clock := &bridgeTestClock{now: time.Unix(1_700_000_000, 0)}
	core, err := agoauth.New(agoauth.Config{
		RelyingParties: map[string][]string{"example.com": {"https://app.example.com"}}, ChallengeTTL: challengeTTL,
		GrantTTL: grantTTL, ChallengeBytes: 32, MaxChallenges: 32, MaxGrants: 32, RequireUserVerification: true,
	}, agoauth.Dependencies{Clock: clock, Random: rand.Reader, Verifier: acceptingBridgeVerifier{}})
	if err != nil {
		t.Fatal(err)
	}
	credentialID := base64.RawURLEncoding.EncodeToString([]byte("credential-1"))
	if err := core.RegisterCredential(agoauth.Credential{
		ID: credentialID, RPID: "example.com", ActorID: "account-1", DeviceID: "device-1", PublicKey: []byte("operator-key"),
	}); err != nil {
		t.Fatal(err)
	}
	return core, clock, credentialID
}

func executeBridgeChallenge(t *testing.T, publications agobridge.Publications, request agobridge.ExecutionRequest) BridgeAuthChallengeResponse {
	t.Helper()
	operation, ok := publications.ResolvePublished(context.Background(), request.ProjectID, request.ThreadID, BridgeActionAuthChallenge)
	if !ok || operation.Mutation() {
		t.Fatal("challenge operation was not a published non-mutation")
	}
	request.Action = BridgeActionAuthChallenge
	request.Payload = json.RawMessage(`{"rp_id":"example.com"}`)
	payload, err := operation.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	var response BridgeAuthChallengeResponse
	if err := json.Unmarshal(payload, &response); err != nil || response.Challenge == "" || response.RPID != "example.com" {
		t.Fatalf("challenge = %#v, %v", response, err)
	}
	challengeBytes, err := base64.RawURLEncoding.DecodeString(response.Challenge)
	if err != nil || base64.RawURLEncoding.EncodeToString(challengeBytes) != response.Challenge {
		t.Fatalf("challenge is not canonical unpadded base64url: %q, %v", response.Challenge, err)
	}
	return response
}

func bridgeAssertionPayload(t *testing.T, challenge, credentialID string, signCount uint32) json.RawMessage {
	t.Helper()
	clientData, _ := json.Marshal(map[string]any{"type": "webauthn.get", "challenge": challenge, "origin": "https://app.example.com", "crossOrigin": false})
	authenticatorData := make([]byte, 37)
	rpHash := sha256.Sum256([]byte("example.com"))
	copy(authenticatorData, rpHash[:])
	authenticatorData[32] = 0x05
	binary.BigEndian.PutUint32(authenticatorData[33:], signCount)
	encoded, err := json.Marshal(BridgeAuthAssertionRequest{
		CredentialID: credentialID, RPID: "example.com",
		ClientDataJSON: base64.RawURLEncoding.EncodeToString(clientData), AuthenticatorData: base64.RawURLEncoding.EncodeToString(authenticatorData),
		Signature: base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{1}, 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestOutboundBridgeTLSCompositionReopensDurableCursor(t *testing.T) {
	store, coordinator, threadID := bridgeFixture(t)
	relay := newBridgeRelay(t, agobridge.RequestEnvelope{
		Sequence: 1, Nonce: "nonce-1", AccountID: "account-1", DeviceID: "device-1",
		ProjectID: "project-1", ThreadID: threadID, Action: BridgeActionProjection,
		Payload: json.RawMessage(`{"after_sequence":0,"limit":10}`),
	})
	stateRoot := filepath.Join(t.TempDir(), "bridge-state")
	start := func() *OutboundBridge {
		stateStore, err := agobridge.NewFileStateStore(stateRoot)
		if err != nil {
			t.Fatal(err)
		}
		bridge, err := StartOutboundBridge(context.Background(), OutboundBridgeConfig{
			Client: agobridge.Config{
				RelayURL: relay.server.URL, CertificatePin: relay.pin(), BearerToken: "token", AccountID: "account-1", DeviceID: "device-1",
				AllowedProjects: map[string]struct{}{"project-1": {}}, StateStore: stateStore,
				PollTimeout: 20 * time.Millisecond, RequestTimeout: 100 * time.Millisecond, ExecutionTimeout: time.Second,
				MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond, MaxBodyBytes: 1 << 20,
			},
			Publications: BridgePublicationConfig{Publications: []BridgePublication{{
				ProjectID: "project-1", ThreadID: threadID, Actions: []string{BridgeActionProjection},
			}}},
			Store: store, Coordinator: coordinator, Closer: stateStore,
		})
		if err != nil {
			t.Fatal(err)
		}
		return bridge
	}

	first := start()
	relay.waitResponses(t, 1)
	shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := first.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	cancel()
	relay.requestDuplicate()
	second := start()
	relay.waitResponses(t, 1)
	shutdown, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := second.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
}

type bridgeRelay struct {
	server  *httptest.Server
	request agobridge.RequestEnvelope
	mu      sync.Mutex
	count   int
	resend  bool
}

func newBridgeRelay(t *testing.T, request agobridge.RequestEnvelope) *bridgeRelay {
	relay := &bridgeRelay{request: request}
	relay.server = httptest.NewTLSServer(http.HandlerFunc(relay.serveHTTP))
	t.Cleanup(relay.server.Close)
	return relay
}

func (relay *bridgeRelay) pin() string {
	digest := sha256.Sum256(relay.server.Certificate().Raw)
	return hex.EncodeToString(digest[:])
}

func (relay *bridgeRelay) requestDuplicate() {
	relay.mu.Lock()
	relay.resend = true
	relay.count = 0
	relay.mu.Unlock()
}

func (relay *bridgeRelay) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer token" {
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return
	}
	var poll agobridge.PollEnvelope
	if err := json.NewDecoder(request.Body).Decode(&poll); err != nil {
		http.Error(writer, "bad poll", http.StatusBadRequest)
		return
	}
	relay.mu.Lock()
	for range poll.Responses {
		relay.count++
	}
	result := agobridge.PollResult{AccountID: poll.AccountID, DeviceID: poll.DeviceID, AcknowledgedThrough: poll.Cursor}
	if poll.Cursor == 0 || relay.resend {
		result.Requests = []agobridge.RequestEnvelope{relay.request}
		relay.resend = false
	}
	relay.mu.Unlock()
	_ = json.NewEncoder(writer).Encode(result)
}

func (relay *bridgeRelay) waitResponses(t *testing.T, wanted int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		relay.mu.Lock()
		count := relay.count
		relay.mu.Unlock()
		if count >= wanted {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("relay responses = %d, want %d", relay.count, wanted)
}

func bridgeFixture(t *testing.T) (*agothreadstore.Store, *agocoordinator.Coordinator, string) {
	return bridgeFixtureWithExecutor(t, bridgeNoopExecutor{})
}

func bridgeFixtureWithExecutor(t *testing.T, executor agocoordinator.Executor) (*agothreadstore.Store, *agocoordinator.Coordinator, string) {
	t.Helper()
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.CreateAtomicThread(context.Background(), agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: "bridge-create", IdempotencyKey: "bridge-create", ActorID: "test", Type: agoprotocol.CommandThreadCreate,
	}, agothreadstore.AtomicCreateInput{
		Spec:           agothreadstore.ThreadSpec{Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}},
		Project:        agothreadstore.ProjectIdentity{ProjectID: "project-1"},
		Agent:          agothreadstore.AgentDefinitionSnapshot{DefinitionID: "ago", Version: "1", DisplayName: "Ago", SystemInstructionsDigest: "sha256:test", DefaultMode: agoprotocol.AgentModeMedium},
		InitialMessage: json.RawMessage(`{"text":"running"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, agocoordinator.New(store, executor), created.ThreadID
}
