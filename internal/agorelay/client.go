// Package agorelay is the transport and structured-output contract for an
// OpenAI-compatible chat completions endpoint — for example a local relay
// (cli-proxy-api) sitting in front of Claude, GPT, or Grok models. It knows
// nothing about boards, tasks, or scheduling; it only sends one bounded
// request and returns one parsed response.
//
// Credentials never live in this package's data structures. Profile stores
// only the NAME of the environment variable that holds the key; Client reads
// the value from the environment at call time via a caller-supplied lookup
// function, uses it for exactly one request, and never stores, logs, or
// returns it. Every error and Health value is passed through agoredact so a
// credential echoed back by a misbehaving server still cannot leak out.
package agorelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"claudexflow/internal/agoredact"
)

// defaultClientTimeout is used when Profile.Timeout is unset.
const defaultClientTimeout = 120 * time.Second

// defaultClientMaxOutputBytes is used when Profile.MaxOutputBytes is unset.
const defaultClientMaxOutputBytes int64 = 1 << 20

// maxParseErrorExcerpt bounds how much of an unparseable response is quoted
// back in an error, so a pathological response cannot blow up a log line.
const maxParseErrorExcerpt = 500

// Profile is non-secret provider configuration. The credential is NEVER
// stored here; it is read from the environment at call time via the lookup
// function passed to New.
type Profile struct {
	ID             string        // e.g. "local-relay"
	BaseURL        string        // e.g. "http://127.0.0.1:8317/v1"
	Model          string        // e.g. "claude-sonnet-5"
	APIKeyEnv      string        // name of the env var holding the credential
	Timeout        time.Duration // per-request wall clock
	MaxOutputBytes int64         // hard cap on response body read
}

// AuthConfigured reports whether the credential is present, WITHOUT
// returning it.
func (p Profile) AuthConfigured(lookup func(string) string) bool {
	if lookup == nil {
		return false
	}
	return strings.TrimSpace(lookup(p.APIKeyEnv)) != ""
}

// Client sends bounded chat-completion requests to one relay profile.
type Client struct {
	profile    Profile
	httpClient *http.Client
	lookupEnv  func(string) string
}

// New validates profile and builds a Client. httpClient may be nil (a
// plain http.Client is used); lookupEnv may be nil (os.Getenv is used).
func New(profile Profile, httpClient *http.Client, lookupEnv func(string) string) (*Client, error) {
	if strings.TrimSpace(profile.BaseURL) == "" {
		return nil, errors.New("agorelay: profile BaseURL is required")
	}
	parsed, err := url.Parse(profile.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("agorelay: profile BaseURL must be an http or https URL, got %q", profile.BaseURL)
	}
	if strings.TrimSpace(profile.Model) == "" {
		return nil, errors.New("agorelay: profile Model is required")
	}
	if profile.Timeout <= 0 {
		profile.Timeout = defaultClientTimeout
	}
	if profile.MaxOutputBytes <= 0 {
		profile.MaxOutputBytes = defaultClientMaxOutputBytes
	}
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if lookupEnv == nil {
		lookupEnv = func(string) string { return "" }
	}
	return &Client{profile: profile, httpClient: httpClient, lookupEnv: lookupEnv}, nil
}

// Request is one chat-completion turn. SchemaName/Schema are optional; when
// set, structured JSON output is requested from the relay.
type Request struct {
	System string
	User   string

	// JSONSchemaName/JSONSchema optional: when set, ask for structured output.
	SchemaName string
	Schema     json.RawMessage
}

// Response is the parsed result of one chat-completion turn.
type Response struct {
	Content      string
	Model        string
	PromptTokens int
	OutputTokens int
}

// StatusError reports a non-2xx HTTP response, letting a caller classify it
// as retryable or terminal without string-matching an error message.
type StatusError struct {
	Code    int
	Message string
}

func (e StatusError) Error() string {
	return fmt.Sprintf("agorelay: relay responded with status %d: %s", e.Code, e.Message)
}

// Retryable reports whether the status is worth retrying: request-timeout,
// conflict, too-early, rate-limited, or any server error.
func (e StatusError) Retryable() bool {
	switch e.Code {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly, http.StatusTooManyRequests:
		return true
	}
	return e.Code >= 500 && e.Code <= 599
}

// wire types for the OpenAI-compatible chat completions endpoint.

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type jsonSchemaSpec struct {
	Name   string          `json:"name"`
	Schema json.RawMessage `json:"schema"`
	Strict bool            `json:"strict,omitempty"`
}

type responseFormatSpec struct {
	Type       string          `json:"type"`
	JSONSchema *jsonSchemaSpec `json:"json_schema,omitempty"`
}

type chatCompletionRequest struct {
	Model          string              `json:"model"`
	Messages       []chatMessage       `json:"messages"`
	ResponseFormat *responseFormatSpec `json:"response_format,omitempty"`
}

type chatCompletionResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type modelListResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
}

// mergeInstructions repeats the system instructions inside the user message.
//
// Not every OpenAI-compatible endpoint delivers the system role. A local
// agent-style proxy in particular replaces it with its own, and the caller's
// instructions vanish silently: against one such relay the verifier's output
// contract was ignored on every call — the model invented its own field names
// and omitted the verdict entirely — so verification could never conclude.
// The user role is the only one every endpoint passes through, so the
// instructions go there too. The system message is still sent, because
// providers that do honour it give it higher priority than user text.
func mergeInstructions(system, user string) string {
	if strings.TrimSpace(system) == "" {
		return user
	}
	return system + "\n\n" + user
}

// Complete performs one bounded chat completion.
func (c *Client) Complete(ctx context.Context, request Request) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, c.profile.Timeout)
	defer cancel()

	key := c.lookupEnv(c.profile.APIKeyEnv)

	wireRequest := chatCompletionRequest{
		Model: c.profile.Model,
		Messages: []chatMessage{
			{Role: "system", Content: request.System},
			{Role: "user", Content: mergeInstructions(request.System, request.User)},
		},
	}
	if request.SchemaName != "" && len(request.Schema) > 0 {
		wireRequest.ResponseFormat = &responseFormatSpec{
			Type: "json_schema",
			JSONSchema: &jsonSchemaSpec{
				Name:   request.SchemaName,
				Schema: request.Schema,
				Strict: true,
			},
		}
	}

	encoded, err := json.Marshal(wireRequest)
	if err != nil {
		return Response{}, fmt.Errorf("agorelay: encode request: %w", err)
	}

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint("/chat/completions"), bytes.NewReader(encoded))
	if err != nil {
		return Response{}, fmt.Errorf("agorelay: build request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if key != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+key)
	}

	httpResponse, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return Response{}, errors.New(c.redact(key, fmt.Sprintf("agorelay: request failed: %s", err.Error())))
	}
	defer httpResponse.Body.Close()

	raw, err := readCapped(httpResponse.Body, c.profile.MaxOutputBytes)
	if err != nil {
		return Response{}, errors.New(c.redact(key, err.Error()))
	}

	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return Response{}, StatusError{
			Code:    httpResponse.StatusCode,
			Message: c.redact(key, strings.TrimSpace(string(raw))),
		}
	}

	var decoded chatCompletionResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Response{}, errors.New(c.redact(key, fmt.Sprintf("agorelay: malformed response body: %s", err.Error())))
	}
	if len(decoded.Choices) == 0 {
		return Response{}, errors.New("agorelay: response contained no choices")
	}
	content := decoded.Choices[0].Message.Content
	if content == "" {
		return Response{}, errors.New("agorelay: response choice had empty content")
	}

	return Response{
		Content:      content,
		Model:        decoded.Model,
		PromptTokens: decoded.Usage.PromptTokens,
		OutputTokens: decoded.Usage.CompletionTokens,
	}, nil
}

var fencedJSONPattern = regexp.MustCompile("(?s)```(?:json)?\\s*(.*?)```")

// CompleteJSON performs Complete and unmarshals the content into target,
// tolerating a model that wraps JSON in ```json fences or prose.
func (c *Client) CompleteJSON(ctx context.Context, request Request, target any) error {
	response, err := c.Complete(ctx, request)
	if err != nil {
		return err
	}

	content := strings.TrimSpace(response.Content)

	if err := json.Unmarshal([]byte(content), target); err == nil {
		return nil
	}

	if fenced := extractFencedJSON(content); fenced != "" {
		if err := json.Unmarshal([]byte(fenced), target); err == nil {
			return nil
		}
	}

	if balanced := extractBalancedObject(content); balanced != "" {
		if err := json.Unmarshal([]byte(balanced), target); err == nil {
			return nil
		}
	}

	key := c.lookupEnv(c.profile.APIKeyEnv)
	excerpt := content
	if len(excerpt) > maxParseErrorExcerpt {
		excerpt = excerpt[:maxParseErrorExcerpt]
	}
	return fmt.Errorf("agorelay: could not parse structured output from response: %q", c.redact(key, excerpt))
}

// extractFencedJSON returns the content of the first ``` or ```json fenced
// block, or "" if there is none.
func extractFencedJSON(content string) string {
	matches := fencedJSONPattern.FindStringSubmatch(content)
	if len(matches) < 2 {
		return ""
	}
	return strings.TrimSpace(matches[1])
}

// extractBalancedObject returns the outermost balanced {...} object found in
// content, ignoring braces inside JSON string literals, or "" if the first
// "{" never closes.
func extractBalancedObject(content string) string {
	start := strings.IndexByte(content, '{')
	if start < 0 {
		return ""
	}

	depth := 0
	inString := false
	escaped := false
	for index := start; index < len(content); index++ {
		ch := content[index]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return content[start : index+1]
			}
		}
	}
	return ""
}

// Health reports relay reachability without ever including the credential.
type Health struct {
	Reachable      bool
	ModelAvailable bool
	AuthConfigured bool
	Detail         string // safe, redacted
}

// Probe checks the relay is reachable and the model is listed. It must
// never return the credential in any field.
func (c *Client) Probe(ctx context.Context) Health {
	health := Health{AuthConfigured: c.profile.AuthConfigured(c.lookupEnv)}

	ctx, cancel := context.WithTimeout(ctx, c.profile.Timeout)
	defer cancel()

	key := c.lookupEnv(c.profile.APIKeyEnv)

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint("/models"), nil)
	if err != nil {
		health.Detail = "agorelay: could not build probe request"
		return health
	}
	if key != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+key)
	}

	httpResponse, err := c.httpClient.Do(httpRequest)
	if err != nil {
		health.Detail = c.redact(key, fmt.Sprintf("relay unreachable: %s", err.Error()))
		return health
	}
	defer httpResponse.Body.Close()
	health.Reachable = true

	raw, err := readCapped(httpResponse.Body, c.profile.MaxOutputBytes)
	if err != nil {
		health.Detail = "agorelay: probe response exceeded size limit"
		return health
	}

	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		health.Detail = c.redact(key, fmt.Sprintf("probe returned status %d", httpResponse.StatusCode))
		return health
	}

	var models modelListResponse
	if err := json.Unmarshal(raw, &models); err != nil {
		health.Detail = "agorelay: probe response was not a valid model list"
		return health
	}
	for _, entry := range models.Data {
		if entry.ID == c.profile.Model {
			health.ModelAvailable = true
			break
		}
	}
	if !health.ModelAvailable {
		health.Detail = "model not listed by relay"
	}
	return health
}

// endpoint joins the profile's BaseURL with path, tolerating a trailing
// slash on BaseURL.
func (c *Client) endpoint(path string) string {
	return strings.TrimRight(c.profile.BaseURL, "/") + path
}

// redact removes the live credential (and anything else credential-shaped)
// from text before it is allowed into an error, Health field, or log.
func (c *Client) redact(key, text string) string {
	if key == "" {
		return text
	}
	return agoredact.New(key).String(text)
}

// readCapped reads at most maxBytes+1 from source so an oversized body is
// detected as an error rather than silently truncated into a success.
func readCapped(source io.Reader, maxBytes int64) ([]byte, error) {
	limited := io.LimitReader(source, maxBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("agorelay: read response body: %w", err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("agorelay: response body exceeded %d byte limit", maxBytes)
	}
	return raw, nil
}
