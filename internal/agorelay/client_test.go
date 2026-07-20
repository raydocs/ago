package agorelay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newRelayClient builds a Client wired to a fake httptest server, using env
// as the whole environment the client can see.
func newRelayClient(t *testing.T, baseURL, apiKeyEnv string, env map[string]string, mutate func(*Profile)) *Client {
	t.Helper()
	profile := Profile{
		ID:             "test-relay",
		BaseURL:        baseURL,
		Model:          "claude-sonnet-5",
		APIKeyEnv:      apiKeyEnv,
		Timeout:        5 * time.Second,
		MaxOutputBytes: 1 << 20,
	}
	if mutate != nil {
		mutate(&profile)
	}
	lookup := func(name string) string { return env[name] }
	client, err := New(profile, nil, lookup)
	if err != nil {
		t.Fatalf("New(%+v) error = %v, want nil", profile, err)
	}
	return client
}

func chatCompletionBody(t *testing.T, content string) []byte {
	t.Helper()
	payload := map[string]any{
		"model": "claude-sonnet-5",
		"choices": []map[string]any{
			{"message": map[string]any{"content": content}},
		},
		"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 22},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal(chat completion fixture) error = %v", err)
	}
	return encoded
}

// TestClientCompleteReturnsContentModelAndUsage exercises the happy path: a
// well-formed chat-completion response is turned into a Response.
func TestClientCompleteReturnsContentModelAndUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(chatCompletionBody(t, "hello from the relay"))
	}))
	defer server.Close()

	client := newRelayClient(t, server.URL, "REL_KEY", map[string]string{"REL_KEY": "irrelevant-value-12345"}, nil)

	response, err := client.Complete(context.Background(), Request{System: "sys", User: "usr"})
	if err != nil {
		t.Fatalf("Complete() error = %v, want nil", err)
	}
	if response.Content != "hello from the relay" {
		t.Fatalf("Complete() Content = %q, want %q", response.Content, "hello from the relay")
	}
	if response.Model != "claude-sonnet-5" {
		t.Fatalf("Complete() Model = %q, want %q", response.Model, "claude-sonnet-5")
	}
	if response.PromptTokens != 11 || response.OutputTokens != 22 {
		t.Fatalf("Complete() tokens = (%d, %d), want (11, 22)", response.PromptTokens, response.OutputTokens)
	}
}

// TestClientCompleteSendsModelSystemAndUserContent verifies the actual wire
// request, not just the response handling.
func TestClientCompleteSendsModelSystemAndUserContent(t *testing.T) {
	type sentBody struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}

	var captured sentBody
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(chatCompletionBody(t, "ok"))
	}))
	defer server.Close()

	client := newRelayClient(t, server.URL, "REL_KEY", map[string]string{"REL_KEY": "irrelevant-value-12345"}, nil)

	if _, err := client.Complete(context.Background(), Request{System: "you are helpful", User: "say hi"}); err != nil {
		t.Fatalf("Complete() error = %v, want nil", err)
	}

	if captured.Model != "claude-sonnet-5" {
		t.Fatalf("request body model = %q, want %q", captured.Model, "claude-sonnet-5")
	}
	var sawSystem, sawUser bool
	for _, message := range captured.Messages {
		if message.Role == "system" && message.Content == "you are helpful" {
			sawSystem = true
		}
		// The instructions must survive a relay that drops the system role, so
		// the user message carries them as well as the request itself.
		if message.Role == "user" && strings.Contains(message.Content, "say hi") &&
			strings.Contains(message.Content, "you are helpful") {
			sawUser = true
		}
	}
	if !sawSystem {
		t.Fatalf("request body messages = %+v, want a system message with the given content", captured.Messages)
	}
	if !sawUser {
		t.Fatalf("request body messages = %+v, want a user message with the given content", captured.Messages)
	}
}

// TestClientCompleteJSONParsesRawJSON covers the simplest structured-output
// case: the model returns exactly a JSON object.
func TestClientCompleteJSONParsesRawJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(chatCompletionBody(t, `{"answer":"forty-two"}`))
	}))
	defer server.Close()

	client := newRelayClient(t, server.URL, "REL_KEY", map[string]string{"REL_KEY": "irrelevant-value-12345"}, nil)

	var target struct {
		Answer string `json:"answer"`
	}
	if err := client.CompleteJSON(context.Background(), Request{System: "s", User: "u"}, &target); err != nil {
		t.Fatalf("CompleteJSON() error = %v, want nil", err)
	}
	if target.Answer != "forty-two" {
		t.Fatalf("CompleteJSON() Answer = %q, want %q", target.Answer, "forty-two")
	}
}

// TestClientCompleteJSONParsesFencedJSON covers a model that wraps its JSON
// in a ```json fence, which is common chat-model behavior.
func TestClientCompleteJSONParsesFencedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(chatCompletionBody(t, "Sure thing:\n```json\n{\"answer\":\"fenced\"}\n```\nHope that helps."))
	}))
	defer server.Close()

	client := newRelayClient(t, server.URL, "REL_KEY", map[string]string{"REL_KEY": "irrelevant-value-12345"}, nil)

	var target struct {
		Answer string `json:"answer"`
	}
	if err := client.CompleteJSON(context.Background(), Request{System: "s", User: "u"}, &target); err != nil {
		t.Fatalf("CompleteJSON() error = %v, want nil", err)
	}
	if target.Answer != "fenced" {
		t.Fatalf("CompleteJSON() Answer = %q, want %q", target.Answer, "fenced")
	}
}

// TestClientCompleteJSONParsesJSONSurroundedByProse covers a model that
// answers in prose with a JSON object embedded in the middle.
func TestClientCompleteJSONParsesJSONSurroundedByProse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(chatCompletionBody(t, `Here is the result: {"answer":"embedded","nested":{"a":1}} — let me know if you need more.`))
	}))
	defer server.Close()

	client := newRelayClient(t, server.URL, "REL_KEY", map[string]string{"REL_KEY": "irrelevant-value-12345"}, nil)

	var target struct {
		Answer string `json:"answer"`
	}
	if err := client.CompleteJSON(context.Background(), Request{System: "s", User: "u"}, &target); err != nil {
		t.Fatalf("CompleteJSON() error = %v, want nil", err)
	}
	if target.Answer != "embedded" {
		t.Fatalf("CompleteJSON() Answer = %q, want %q", target.Answer, "embedded")
	}
}

// TestClientCompleteJSONErrorsOnUnparseableOutputWithBoundedExcerpt asserts
// that when nothing parses, the caller gets an informative but bounded
// error rather than a silent zero value or an unbounded dump.
func TestClientCompleteJSONErrorsOnUnparseableOutputWithBoundedExcerpt(t *testing.T) {
	longNonsense := strings.Repeat("not json whatsoever. ", 100)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(chatCompletionBody(t, longNonsense))
	}))
	defer server.Close()

	client := newRelayClient(t, server.URL, "REL_KEY", map[string]string{"REL_KEY": "irrelevant-value-12345"}, nil)

	var target struct {
		Answer string `json:"answer"`
	}
	err := client.CompleteJSON(context.Background(), Request{System: "s", User: "u"}, &target)
	if err == nil {
		t.Fatalf("CompleteJSON() error = nil, want an error for unparseable output")
	}
	if len(err.Error()) >= 600 {
		t.Fatalf("CompleteJSON() error message length = %d, want < 600", len(err.Error()))
	}
}

// TestClientCompleteNeverLeaksAPIKeyOnErrorResponse is the core credential
// safety guarantee: even when the relay echoes the key back in an error
// body, the client's returned error must not contain it.
func TestClientCompleteNeverLeaksAPIKeyOnErrorResponse(t *testing.T) {
	const sentinel = "sk-test-sentinel-value-should-never-leak-999"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":"invalid credential, saw header %q, key was %s"}`, authorization, sentinel)
	}))
	defer server.Close()

	client := newRelayClient(t, server.URL, "REL_KEY", map[string]string{"REL_KEY": sentinel}, nil)

	_, err := client.Complete(context.Background(), Request{System: "s", User: "u"})
	if err == nil {
		t.Fatalf("Complete() error = nil, want a StatusError for the 401 response")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Fatalf("Complete() error = %q, must not contain the API key %q", err.Error(), sentinel)
	}

	var statusErr StatusError
	if !asStatusError(err, &statusErr) {
		t.Fatalf("Complete() error type = %T, want StatusError", err)
	}
	if statusErr.Code != http.StatusUnauthorized {
		t.Fatalf("StatusError.Code = %d, want %d", statusErr.Code, http.StatusUnauthorized)
	}
}

// asStatusError is a small helper since the package deliberately has no
// third-party assertion library available.
func asStatusError(err error, target *StatusError) bool {
	statusErr, ok := err.(StatusError)
	if ok {
		*target = statusErr
	}
	return ok
}

// TestClientProbeNeverLeaksAPIKeyOnFailure exercises Probe's credential
// safety guarantee across every Health field.
func TestClientProbeNeverLeaksAPIKeyOnFailure(t *testing.T) {
	const sentinel = "sk-test-sentinel-value-should-never-leak-777"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":"key was %s"}`, sentinel)
	}))
	defer server.Close()

	client := newRelayClient(t, server.URL, "REL_KEY", map[string]string{"REL_KEY": sentinel}, nil)

	health := client.Probe(context.Background())
	if strings.Contains(health.Detail, sentinel) {
		t.Fatalf("Probe() Detail = %q, must not contain the API key %q", health.Detail, sentinel)
	}
	if !health.Reachable {
		t.Fatalf("Probe() Reachable = false, want true (server responded, even if with an error status)")
	}
	if health.ModelAvailable {
		t.Fatalf("Probe() ModelAvailable = true, want false (server did not return a model list)")
	}
}

// TestClientCompleteRejectsOversizedResponseBody asserts an oversized body
// is a hard error, never a silently truncated success.
func TestClientCompleteRejectsOversizedResponseBody(t *testing.T) {
	huge := chatCompletionBody(t, strings.Repeat("x", 4096))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(huge)
	}))
	defer server.Close()

	client := newRelayClient(t, server.URL, "REL_KEY", map[string]string{"REL_KEY": "irrelevant-value-12345"}, func(profile *Profile) {
		profile.MaxOutputBytes = 64
	})

	_, err := client.Complete(context.Background(), Request{System: "s", User: "u"})
	if err == nil {
		t.Fatalf("Complete() error = nil, want an error for a response over MaxOutputBytes")
	}
}

// TestClientCompleteTimesOutWhenServerSleepsPastTimeout uses a short,
// deterministic Timeout so the test itself never sleeps beyond it.
func TestClientCompleteTimesOutWhenServerSleepsPastTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	client := newRelayClient(t, server.URL, "REL_KEY", map[string]string{"REL_KEY": "irrelevant-value-12345"}, func(profile *Profile) {
		profile.Timeout = 100 * time.Millisecond
	})

	start := time.Now()
	_, err := client.Complete(context.Background(), Request{System: "s", User: "u"})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("Complete() error = nil, want a deadline-exceeded error")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("Complete() error = %q, want it to mention a deadline", err.Error())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Complete() took %v, want it to abort close to the 100ms Timeout", elapsed)
	}
}

// TestClientCompleteAbortsOnContextCancellationMidRequest confirms that
// cancelling the caller's context — not just Timeout expiring — aborts the
// in-flight request promptly.
func TestClientCompleteAbortsOnContextCancellationMidRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()

	client := newRelayClient(t, server.URL, "REL_KEY", map[string]string{"REL_KEY": "irrelevant-value-12345"}, func(profile *Profile) {
		profile.Timeout = 30 * time.Second
	})

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := client.Complete(ctx, Request{System: "s", User: "u"})
		errCh <- err
	}()

	<-started
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("Complete() error = nil, want an error after context cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Complete() did not return within 5s of context cancellation")
	}
}

// TestStatusErrorRetryableClassifiesByStatusCode locks down the exact
// retryable/terminal boundary callers depend on.
func TestStatusErrorRetryableClassifiesByStatusCode(t *testing.T) {
	cases := []struct {
		code      int
		retryable bool
	}{
		{http.StatusRequestTimeout, true},
		{http.StatusConflict, true},
		{http.StatusTooEarly, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusNotFound, false},
	}
	for _, testCase := range cases {
		err := StatusError{Code: testCase.code}
		if got := err.Retryable(); got != testCase.retryable {
			t.Errorf("StatusError{Code: %d}.Retryable() = %v, want %v", testCase.code, got, testCase.retryable)
		}
	}
}

// TestClientProbeReportsUnreachableForDeadEndpointWithoutPanicking exercises
// the connection-refused path: Probe must degrade gracefully.
func TestClientProbeReportsUnreachableForDeadEndpointWithoutPanicking(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := server.URL
	server.Close() // nothing is listening at deadURL anymore

	client := newRelayClient(t, deadURL, "REL_KEY", map[string]string{"REL_KEY": "irrelevant-value-12345"}, func(profile *Profile) {
		profile.Timeout = 2 * time.Second
	})

	health := client.Probe(context.Background())
	if health.Reachable {
		t.Fatalf("Probe() Reachable = true, want false for a dead endpoint")
	}
	if health.ModelAvailable {
		t.Fatalf("Probe() ModelAvailable = true, want false for a dead endpoint")
	}
}

// TestClientProbeReportsModelAvailableFromModelsListing exercises the
// success path of Probe against a /models listing.
func TestClientProbeReportsModelAvailableFromModelsListing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			t.Fatalf("Probe() requested path = %q, want it to end in /models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"claude-opus-4-8"},{"id":"claude-sonnet-5"}]}`)
	}))
	defer server.Close()

	client := newRelayClient(t, server.URL, "REL_KEY", map[string]string{"REL_KEY": "irrelevant-value-12345"}, nil)

	health := client.Probe(context.Background())
	if !health.Reachable {
		t.Fatalf("Probe() Reachable = false, want true")
	}
	if !health.ModelAvailable {
		t.Fatalf("Probe() ModelAvailable = false, want true (claude-sonnet-5 is listed)")
	}
	if !health.AuthConfigured {
		t.Fatalf("Probe() AuthConfigured = false, want true (env var is set)")
	}
}

// TestProfileAuthConfiguredReflectsEnvironmentPresenceOnly verifies
// AuthConfigured is a presence check, not a value check, and that the raw
// value never needs to be inspected to compute it.
func TestProfileAuthConfiguredReflectsEnvironmentPresenceOnly(t *testing.T) {
	profile := Profile{APIKeyEnv: "REL_KEY"}

	empty := func(string) string { return "" }
	if profile.AuthConfigured(empty) {
		t.Fatalf("AuthConfigured() = true, want false when the env var is empty")
	}

	set := func(name string) string {
		if name == "REL_KEY" {
			return "sentinel-value-12345"
		}
		return ""
	}
	if !profile.AuthConfigured(set) {
		t.Fatalf("AuthConfigured() = false, want true when the env var is set")
	}
}

// TestNewValidatesProfileAndAppliesDefaults locks down New's validation and
// default-filling behavior, since callers depend on Timeout/MaxOutputBytes
// never being zero after construction.
func TestNewValidatesProfileAndAppliesDefaults(t *testing.T) {
	if _, err := New(Profile{Model: "m"}, nil, nil); err == nil {
		t.Fatalf("New() error = nil, want an error for a missing BaseURL")
	}
	if _, err := New(Profile{BaseURL: "not-a-url", Model: "m"}, nil, nil); err == nil {
		t.Fatalf("New() error = nil, want an error for a non-http(s) BaseURL")
	}
	if _, err := New(Profile{BaseURL: "http://127.0.0.1:1"}, nil, nil); err == nil {
		t.Fatalf("New() error = nil, want an error for a missing Model")
	}

	client, err := New(Profile{BaseURL: "http://127.0.0.1:1", Model: "m"}, nil, nil)
	if err != nil {
		t.Fatalf("New() error = %v, want nil", err)
	}
	if client.profile.Timeout != defaultClientTimeout {
		t.Fatalf("New() Timeout = %v, want default %v", client.profile.Timeout, defaultClientTimeout)
	}
	if client.profile.MaxOutputBytes != defaultClientMaxOutputBytes {
		t.Fatalf("New() MaxOutputBytes = %d, want default %d", client.profile.MaxOutputBytes, defaultClientMaxOutputBytes)
	}
}
