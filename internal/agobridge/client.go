// Package agobridge provides the outbound-only authenticated relay transport for
// explicitly published operations on already-running local threads.
package agobridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	defaultMaxBodyBytes     int64 = 1 << 20
	defaultPollTimeout            = 30 * time.Second
	defaultRequestTimeout         = 35 * time.Second
	defaultExecutionTimeout       = 30 * time.Second
	defaultMinBackoff             = 250 * time.Millisecond
	defaultMaxBackoff             = 10 * time.Second
	defaultMaxRemembered          = 4096
	maxIdentityBytes              = 512
	maxActionBytes                = 256
	maxNonceBytes                 = 512
)

var (
	ErrCertificatePin = errors.New("agobridge: relay certificate pin mismatch")
	ErrSequence       = errors.New("agobridge: invalid relay sequence")
	ErrStateConflict  = errors.New("agobridge: durable state revision conflict")
	ErrStateCapacity  = errors.New("agobridge: durable replay evidence capacity reached")
)

const (
	ErrorUnauthorized          = "unauthorized"
	ErrorReplay                = "replay"
	ErrorConflict              = "conflict"
	ErrorAuthorizationRequired = "authorization_required"
	ErrorInvalidRequest        = "invalid_request"
	ErrorExecutionFailed       = "execution_failed"
	ErrorCapacity              = "capacity"
	ErrorUnknownOutcome        = "unknown_outcome"
	ErrorStateUnavailable      = "state_unavailable"
)

// Operation is a locally classified action on one explicitly published thread.
// Mutation must be determined locally; it is never accepted from the relay.
type Operation interface {
	Mutation() bool
	Execute(context.Context, ExecutionRequest) (json.RawMessage, error)
}

// ExecutionRequest carries the stable identity required to derive a durable
// downstream idempotency key (account/device/sequence, with nonce as binding).
type ExecutionRequest struct {
	AccountID          string
	DeviceID           string
	ProjectID          string
	ThreadID           string
	Action             string
	Nonce              string
	Sequence           uint64
	AuthorizationToken string
	Payload            json.RawMessage
}

// Publications resolves only operations belonging to explicitly published,
// already-running local threads. Returning false denies the request.
type Publications interface {
	ResolvePublished(context.Context, string, string, string) (Operation, bool)
}

// Authorization is injected by the daemon's recent-passkey authority.
type Authorization interface {
	AuthorizeMutation(context.Context, MutationAuthorization) error
}

type BridgeIdentity struct {
	AccountID string `json:"account_id"`
	DeviceID  string `json:"device_id"`
}

// StateStore must durably and atomically compare-and-swap the complete state.
// Commit must not return success until the replacement survives process restart.
type StateStore interface {
	Load(context.Context, BridgeIdentity) (State, error)
	Commit(context.Context, BridgeIdentity, uint64, State) (uint64, error)
}

const (
	EvidencePrepared  = "prepared"
	EvidenceCompleted = "completed"
)

type Evidence struct {
	Sequence      uint64            `json:"sequence"`
	Nonce         string            `json:"nonce"`
	RequestDigest string            `json:"request_digest"`
	Status        string            `json:"status"`
	Response      *ResponseEnvelope `json:"response,omitempty"`
}

type PendingResponse struct {
	RequestDigest string           `json:"request_digest"`
	Response      ResponseEnvelope `json:"response"`
}

type State struct {
	Revision uint64           `json:"revision"`
	Cursor   uint64           `json:"cursor"`
	Pending  *PendingResponse `json:"pending,omitempty"`
	Evidence []Evidence       `json:"evidence,omitempty"`
}

type MutationAuthorization struct {
	AccountID          string
	DeviceID           string
	ProjectID          string
	ThreadID           string
	Action             string
	Nonce              string
	Sequence           uint64
	AuthorizationToken string
}

type Config struct {
	RelayURL       string
	CertificatePin string
	BearerToken    string
	AccountID      string
	DeviceID       string

	AllowedProjects map[string]struct{}
	Publications    Publications
	Authorization   Authorization
	StateStore      StateStore

	PollTimeout      time.Duration
	RequestTimeout   time.Duration
	ExecutionTimeout time.Duration
	MinBackoff       time.Duration
	MaxBackoff       time.Duration
	MaxBodyBytes     int64
	MaxRemembered    int
}

type RequestEnvelope struct {
	Sequence           uint64          `json:"sequence"`
	Nonce              string          `json:"nonce"`
	AccountID          string          `json:"account_id"`
	DeviceID           string          `json:"device_id"`
	ProjectID          string          `json:"project_id"`
	ThreadID           string          `json:"thread_id"`
	Action             string          `json:"action"`
	AuthorizationToken string          `json:"authorization_token,omitempty"`
	Payload            json.RawMessage `json:"payload"`
}

type ResponseEnvelope struct {
	Sequence  uint64          `json:"sequence"`
	Nonce     string          `json:"nonce"`
	AccountID string          `json:"account_id"`
	DeviceID  string          `json:"device_id"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Error     *ResponseError  `json:"error,omitempty"`
}

type ResponseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type PollEnvelope struct {
	AccountID string             `json:"account_id"`
	DeviceID  string             `json:"device_id"`
	Cursor    uint64             `json:"cursor"`
	Responses []ResponseEnvelope `json:"responses,omitempty"`
}

type PollResult struct {
	AccountID           string            `json:"account_id"`
	DeviceID            string            `json:"device_id"`
	AcknowledgedThrough uint64            `json:"acknowledged_through"`
	Requests            []RequestEnvelope `json:"requests,omitempty"`
}

type Client struct {
	config     Config
	endpoint   string
	httpClient *http.Client

	mu         sync.Mutex
	identity   BridgeIdentity
	state      State
	bySequence map[uint64]int
	nonces     map[string]uint64
}

func New(ctx context.Context, config Config) (*Client, error) {
	relayURL, err := url.Parse(config.RelayURL)
	if err != nil || relayURL.Scheme != "https" || relayURL.Host == "" || relayURL.User != nil || relayURL.RawQuery != "" || relayURL.Fragment != "" {
		return nil, errors.New("agobridge: relay URL must be an HTTPS origin")
	}
	if relayURL.Path != "" && relayURL.Path != "/" {
		return nil, errors.New("agobridge: relay URL must not contain a path")
	}
	pin, err := hex.DecodeString(config.CertificatePin)
	if err != nil || len(pin) != sha256.Size {
		return nil, errors.New("agobridge: certificate pin must be a SHA-256 hex digest")
	}
	if len(config.BearerToken) == 0 || len(config.BearerToken) > 8<<10 || strings.ContainsAny(config.BearerToken, "\r\n") ||
		!validIdentity(config.AccountID, maxIdentityBytes) || !validIdentity(config.DeviceID, maxIdentityBytes) {
		return nil, errors.New("agobridge: relay credentials and account/device identity are required")
	}
	if len(config.AllowedProjects) == 0 || config.Publications == nil || config.StateStore == nil {
		return nil, errors.New("agobridge: project ACL, publications, and durable state store are required")
	}
	for projectID := range config.AllowedProjects {
		if !validIdentity(projectID, maxIdentityBytes) {
			return nil, errors.New("agobridge: invalid project ACL entry")
		}
	}
	applyDefaults(&config)
	if config.MinBackoff > config.MaxBackoff || config.RequestTimeout <= config.PollTimeout || config.ExecutionTimeout <= 0 || config.MaxBodyBytes <= 0 || config.MaxRemembered <= 0 {
		return nil, errors.New("agobridge: invalid timeout, backoff, or body limit")
	}
	emptyPoll, err := json.Marshal(PollEnvelope{AccountID: config.AccountID, DeviceID: config.DeviceID})
	if err != nil || int64(len(emptyPoll)) > config.MaxBodyBytes {
		return nil, errors.New("agobridge: body limit cannot contain the minimum poll envelope")
	}

	pinned := append([]byte(nil), pin...)
	tlsConfig := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // The exact certificate is authenticated below.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return ErrCertificatePin
			}
			digest := sha256.Sum256(state.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(digest[:], pinned) != 1 {
				return ErrCertificatePin
			}
			return nil
		},
	}
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSClientConfig:       tlsConfig,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: config.RequestTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          2,
		MaxIdleConnsPerHost:   1,
	}
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("agobridge: relay redirects are forbidden")
		},
	}
	identity := BridgeIdentity{AccountID: config.AccountID, DeviceID: config.DeviceID}
	state, err := config.StateStore.Load(ctx, identity)
	if err != nil {
		return nil, fmt.Errorf("agobridge: load durable state: %w", err)
	}
	clientResult := &Client{
		config: config, endpoint: strings.TrimSuffix(relayURL.String(), "/") + "/v1/bridge/poll", httpClient: client,
		identity: identity, state: cloneState(state), bySequence: make(map[uint64]int), nonces: make(map[string]uint64),
	}
	if err := clientResult.indexState(); err != nil {
		return nil, err
	}
	return clientResult, nil
}

func applyDefaults(config *Config) {
	if config.PollTimeout <= 0 {
		config.PollTimeout = defaultPollTimeout
	}
	if config.RequestTimeout <= 0 {
		config.RequestTimeout = defaultRequestTimeout
	}
	if config.ExecutionTimeout <= 0 {
		config.ExecutionTimeout = defaultExecutionTimeout
	}
	if config.MinBackoff <= 0 {
		config.MinBackoff = defaultMinBackoff
	}
	if config.MaxBackoff <= 0 {
		config.MaxBackoff = defaultMaxBackoff
	}
	if config.MaxBodyBytes <= 0 {
		config.MaxBodyBytes = defaultMaxBodyBytes
	}
	if config.MaxRemembered <= 0 {
		config.MaxRemembered = defaultMaxRemembered
	}
}

// Run connects to the relay and long-polls until ctx is canceled. Transient
// transport failures are retried with bounded exponential backoff.
func (client *Client) Run(ctx context.Context) error {
	backoff := client.config.MinBackoff
	for {
		result, err := client.poll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, ErrCertificatePin) {
				return err
			}
			if err := sleep(ctx, backoff); err != nil {
				return err
			}
			backoff *= 2
			if backoff > client.config.MaxBackoff {
				backoff = client.config.MaxBackoff
			}
			continue
		}
		backoff = client.config.MinBackoff
		if err := client.accept(ctx, result); err != nil {
			return err
		}
	}
}

func (client *Client) poll(ctx context.Context) (PollResult, error) {
	client.mu.Lock()
	poll := PollEnvelope{AccountID: client.config.AccountID, DeviceID: client.config.DeviceID, Cursor: client.state.Cursor}
	if client.state.Pending != nil {
		poll.Responses = []ResponseEnvelope{cloneResponse(client.state.Pending.Response)}
	}
	client.mu.Unlock()
	body, err := json.Marshal(poll)
	if err != nil {
		return PollResult{}, err
	}
	if int64(len(body)) > client.config.MaxBodyBytes {
		return PollResult{}, errors.New("agobridge: outbound poll body exceeds limit")
	}
	requestCtx, cancel := context.WithTimeout(ctx, client.config.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, client.endpoint, bytes.NewReader(body))
	if err != nil {
		return PollResult{}, err
	}
	request.Header.Set("Authorization", "Bearer "+client.config.BearerToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Bridge-Poll-Timeout", client.config.PollTimeout.String())
	response, err := client.httpClient.Do(request)
	if err != nil {
		return PollResult{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
		return PollResult{}, fmt.Errorf("agobridge: relay returned HTTP %d", response.StatusCode)
	}
	var result PollResult
	decoder := json.NewDecoder(io.LimitReader(response.Body, client.config.MaxBodyBytes+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return PollResult{}, fmt.Errorf("agobridge: decode relay response: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return PollResult{}, err
	}
	return result, nil
}

func (client *Client) accept(ctx context.Context, result PollResult) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if result.AccountID != client.config.AccountID || result.DeviceID != client.config.DeviceID {
		return errors.New("agobridge: relay response identity mismatch")
	}
	if result.AcknowledgedThrough > client.state.Cursor || len(result.Requests) > 1 {
		return ErrSequence
	}
	if client.state.Pending != nil && result.AcknowledgedThrough >= client.state.Pending.Response.Sequence {
		previous := cloneState(client.state)
		client.state.Pending = nil
		if err := client.commitLocked(ctx, previous.Revision); err != nil {
			client.state = previous
			return err
		}
	}
	if len(result.Requests) == 0 {
		return nil
	}
	request := result.Requests[0]
	if request.Sequence == client.state.Cursor && client.state.Cursor != 0 {
		_, err := client.handleLocked(ctx, request)
		return err
	}
	if client.state.Pending != nil || request.Sequence != client.state.Cursor+1 {
		return ErrSequence
	}
	_, err := client.handleLocked(ctx, request)
	return err
}

func (client *Client) handle(ctx context.Context, request RequestEnvelope) ResponseEnvelope {
	client.mu.Lock()
	defer client.mu.Unlock()
	response, err := client.handleLocked(ctx, request)
	if err != nil {
		return client.failure(request, ErrorStateUnavailable, "durable bridge state is unavailable")
	}
	return response
}

func (client *Client) handleLocked(ctx context.Context, request RequestEnvelope) (ResponseEnvelope, error) {
	digest := requestDigest(request)
	digestText := hex.EncodeToString(digest[:])
	if index, ok := client.bySequence[request.Sequence]; ok {
		evidence := &client.state.Evidence[index]
		if evidence.RequestDigest != digestText {
			response := client.failure(request, ErrorConflict, "sequence already used by a different request")
			if evidence.Status == EvidencePrepared {
				if err := client.completePreparedConflictLocked(ctx, evidence, digestText, response); err != nil {
					return ResponseEnvelope{}, err
				}
				return response, nil
			}
			if client.state.Pending != nil {
				return response, nil
			}
			if err := client.persistPendingLocked(ctx, digestText, response); err != nil {
				return ResponseEnvelope{}, err
			}
			return response, nil
		}
		if evidence.Status == EvidencePrepared {
			response := client.failure(request, ErrorUnknownOutcome, "prepared request outcome is unknown")
			if err := client.completeLocked(ctx, evidence, digestText, response); err != nil {
				return ResponseEnvelope{}, err
			}
			return response, nil
		}
		response := cloneResponse(*evidence.Response)
		if client.state.Pending == nil {
			if err := client.persistPendingLocked(ctx, digestText, response); err != nil {
				return ResponseEnvelope{}, err
			}
		}
		return response, nil
	}
	if request.Sequence != client.state.Cursor+1 || !validIdentity(request.Nonce, maxNonceBytes) {
		return ResponseEnvelope{}, ErrSequence
	}
	// Fail closed rather than evicting replay/idempotency evidence. This keeps
	// memory bounded without allowing an old nonce or sequence to become valid.
	if len(client.state.Evidence) >= client.config.MaxRemembered {
		return ResponseEnvelope{}, ErrStateCapacity
	}
	previous := cloneState(client.state)
	client.state.Evidence = append(client.state.Evidence, Evidence{
		Sequence: request.Sequence, Nonce: request.Nonce, RequestDigest: digestText, Status: EvidencePrepared,
	})
	index := len(client.state.Evidence) - 1
	client.bySequence[request.Sequence] = index
	if _, exists := client.nonces[request.Nonce]; !exists {
		client.nonces[request.Nonce] = request.Sequence
	}
	if err := client.commitLocked(ctx, previous.Revision); err != nil {
		client.state = previous
		client.rebuildIndexes()
		return ResponseEnvelope{}, err
	}
	response := client.validateAndExecute(ctx, request)
	response = client.boundResponse(response)
	evidence := &client.state.Evidence[index]
	if err := client.completeLocked(ctx, evidence, digestText, response); err != nil {
		return ResponseEnvelope{}, err
	}
	return response, nil
}

func (client *Client) completePreparedConflictLocked(ctx context.Context, evidence *Evidence, digest string, conflict ResponseEnvelope) error {
	previous := cloneState(client.state)
	unknown := client.failure(RequestEnvelope{Sequence: evidence.Sequence, Nonce: evidence.Nonce}, ErrorUnknownOutcome, "prepared request outcome is unknown")
	unknown = client.boundResponse(unknown)
	evidence.Status = EvidenceCompleted
	evidence.Response = &unknown
	client.state.Cursor = evidence.Sequence
	conflict = client.boundResponse(conflict)
	client.state.Pending = &PendingResponse{RequestDigest: digest, Response: conflict}
	if err := client.commitLocked(ctx, previous.Revision); err != nil {
		client.state = previous
		client.rebuildIndexes()
		return err
	}
	return nil
}

func (client *Client) completeLocked(ctx context.Context, evidence *Evidence, digest string, response ResponseEnvelope) error {
	response = client.boundResponse(response)
	if !client.responseFitsPoll(response) {
		return errors.New("agobridge: response envelope exceeds configured body limit")
	}
	previous := cloneState(client.state)
	evidence.Status = EvidenceCompleted
	cloned := cloneResponse(response)
	evidence.Response = &cloned
	if response.Sequence == client.state.Cursor+1 {
		client.state.Cursor = response.Sequence
	}
	client.state.Pending = &PendingResponse{RequestDigest: digest, Response: cloneResponse(response)}
	if err := client.commitLocked(ctx, previous.Revision); err != nil {
		client.state = previous
		client.rebuildIndexes()
		return err
	}
	return nil
}

func (client *Client) persistPendingLocked(ctx context.Context, digest string, response ResponseEnvelope) error {
	if client.state.Pending != nil {
		return nil
	}
	previous := cloneState(client.state)
	if response.Sequence == client.state.Cursor+1 {
		client.state.Cursor = response.Sequence
	}
	client.state.Pending = &PendingResponse{RequestDigest: digest, Response: cloneResponse(response)}
	if err := client.commitLocked(ctx, previous.Revision); err != nil {
		client.state = previous
		return err
	}
	return nil
}

func (client *Client) validateAndExecute(ctx context.Context, request RequestEnvelope) ResponseEnvelope {
	if !validRequestEnvelope(request, client.config.MaxBodyBytes) {
		return client.failure(request, ErrorInvalidRequest, "invalid request envelope")
	}
	if request.AccountID != client.config.AccountID || request.DeviceID != client.config.DeviceID {
		return client.failure(request, ErrorUnauthorized, "account or device is not authorized")
	}
	if previous, ok := client.nonces[request.Nonce]; ok && previous != request.Sequence {
		return client.failure(request, ErrorReplay, "nonce has already been used")
	}
	client.nonces[request.Nonce] = request.Sequence
	if _, ok := client.config.AllowedProjects[request.ProjectID]; !ok {
		return client.failure(request, ErrorUnauthorized, "project or thread is not published")
	}
	operationCtx, cancel := context.WithTimeout(ctx, client.config.ExecutionTimeout)
	defer cancel()
	operation, ok := client.config.Publications.ResolvePublished(operationCtx, request.ProjectID, request.ThreadID, request.Action)
	if !ok || operation == nil {
		return client.failure(request, ErrorUnauthorized, "project or thread is not published")
	}
	if operation.Mutation() {
		if client.config.Authorization == nil {
			return client.failure(request, ErrorAuthorizationRequired, "recent passkey authorization is required")
		}
		authorization := MutationAuthorization{
			AccountID: request.AccountID, DeviceID: request.DeviceID, ProjectID: request.ProjectID,
			ThreadID: request.ThreadID, Action: request.Action, Nonce: request.Nonce, Sequence: request.Sequence,
			AuthorizationToken: request.AuthorizationToken,
		}
		if err := client.config.Authorization.AuthorizeMutation(operationCtx, authorization); err != nil {
			return client.failure(request, ErrorAuthorizationRequired, "recent passkey authorization is required")
		}
	}
	execution := ExecutionRequest{
		AccountID: request.AccountID, DeviceID: request.DeviceID, ProjectID: request.ProjectID,
		ThreadID: request.ThreadID, Action: request.Action, Nonce: request.Nonce, Sequence: request.Sequence,
		AuthorizationToken: request.AuthorizationToken,
		Payload:            append(json.RawMessage(nil), request.Payload...),
	}
	payload, err := operation.Execute(operationCtx, execution)
	if err != nil {
		return client.failure(request, ErrorExecutionFailed, "published operation failed")
	}
	if int64(len(payload)) > client.config.MaxBodyBytes || (len(payload) > 0 && !json.Valid(payload)) {
		return client.failure(request, ErrorExecutionFailed, "published operation returned an invalid response")
	}
	return ResponseEnvelope{
		Sequence: request.Sequence, Nonce: request.Nonce, AccountID: client.config.AccountID,
		DeviceID: client.config.DeviceID, Payload: append(json.RawMessage(nil), payload...),
	}
}

func validRequestEnvelope(request RequestEnvelope, maxBodyBytes int64) bool {
	return request.Sequence != 0 && validIdentity(request.Nonce, maxNonceBytes) &&
		validIdentity(request.ProjectID, maxIdentityBytes) && validIdentity(request.ThreadID, maxIdentityBytes) &&
		validIdentity(request.Action, maxActionBytes) && len(request.Payload) > 0 &&
		int64(len(request.Payload)) <= maxBodyBytes && json.Valid(request.Payload)
}

func (client *Client) boundResponse(response ResponseEnvelope) ResponseEnvelope {
	if client.responseFitsPoll(response) {
		return response
	}
	return client.failure(RequestEnvelope{Sequence: response.Sequence, Nonce: response.Nonce}, ErrorExecutionFailed, "published operation response exceeds transport limit")
}

func (client *Client) responseFitsPoll(response ResponseEnvelope) bool {
	poll := PollEnvelope{
		AccountID: client.config.AccountID,
		DeviceID:  client.config.DeviceID,
		Cursor:    response.Sequence,
		Responses: []ResponseEnvelope{response},
	}
	encoded, err := json.Marshal(poll)
	return err == nil && int64(len(encoded)) <= client.config.MaxBodyBytes
}

func (client *Client) failure(request RequestEnvelope, code, message string) ResponseEnvelope {
	return ResponseEnvelope{
		Sequence: request.Sequence, Nonce: request.Nonce, AccountID: client.config.AccountID,
		DeviceID: client.config.DeviceID, Error: &ResponseError{Code: code, Message: message},
	}
}

func requestDigest(request RequestEnvelope) [sha256.Size]byte {
	encoded, _ := json.Marshal(request)
	return sha256.Sum256(encoded)
}

func cloneResponse(response ResponseEnvelope) ResponseEnvelope {
	response.Payload = append(json.RawMessage(nil), response.Payload...)
	if response.Error != nil {
		cloned := *response.Error
		response.Error = &cloned
	}
	return response
}

func responsesEqual(first, second ResponseEnvelope) bool {
	firstJSON, firstErr := json.Marshal(first)
	secondJSON, secondErr := json.Marshal(second)
	return firstErr == nil && secondErr == nil && bytes.Equal(firstJSON, secondJSON)
}

func cloneState(state State) State {
	cloned := state
	cloned.Evidence = make([]Evidence, len(state.Evidence))
	for index, evidence := range state.Evidence {
		cloned.Evidence[index] = evidence
		if evidence.Response != nil {
			response := cloneResponse(*evidence.Response)
			cloned.Evidence[index].Response = &response
		}
	}
	if state.Pending != nil {
		pending := *state.Pending
		pending.Response = cloneResponse(pending.Response)
		cloned.Pending = &pending
	}
	return cloned
}

func (client *Client) indexState() error {
	if len(client.state.Evidence) > client.config.MaxRemembered {
		return errors.New("agobridge: durable evidence exceeds configured bound")
	}
	if client.state.Cursor > uint64(len(client.state.Evidence)) ||
		(uint64(len(client.state.Evidence)) > client.state.Cursor+1) {
		return errors.New("agobridge: invalid durable cursor")
	}
	client.rebuildIndexes()
	for index, evidence := range client.state.Evidence {
		digest, err := hex.DecodeString(evidence.RequestDigest)
		if evidence.Sequence != uint64(index+1) || !validIdentity(evidence.Nonce, maxNonceBytes) || err != nil ||
			len(digest) != sha256.Size || hex.EncodeToString(digest) != evidence.RequestDigest {
			return errors.New("agobridge: invalid durable evidence")
		}
		if client.bySequence[evidence.Sequence] != index {
			return errors.New("agobridge: duplicate durable sequence evidence")
		}
		if evidence.Status != EvidencePrepared && evidence.Status != EvidenceCompleted {
			return errors.New("agobridge: invalid durable evidence status")
		}
		if evidence.Status == EvidenceCompleted {
			if evidence.Sequence > client.state.Cursor {
				return errors.New("agobridge: completed evidence is ahead of cursor")
			}
			if evidence.Response == nil || evidence.Response.Sequence != evidence.Sequence || evidence.Response.Nonce != evidence.Nonce {
				return errors.New("agobridge: invalid durable completed response")
			}
			if evidence.Response.AccountID != client.identity.AccountID || evidence.Response.DeviceID != client.identity.DeviceID ||
				int64(len(evidence.Response.Payload)) > client.config.MaxBodyBytes ||
				(len(evidence.Response.Payload) > 0 && !json.Valid(evidence.Response.Payload)) {
				return errors.New("agobridge: invalid durable completed response")
			}
		} else if evidence.Response != nil || index != len(client.state.Evidence)-1 ||
			evidence.Sequence < client.state.Cursor || evidence.Sequence > client.state.Cursor+1 {
			return errors.New("agobridge: invalid prepared evidence")
		}
	}
	if client.state.Pending != nil {
		pending := client.state.Pending
		if pending.Response.Sequence != client.state.Cursor || pending.Response.AccountID != client.identity.AccountID ||
			pending.Response.DeviceID != client.identity.DeviceID || !client.responseFitsPoll(pending.Response) {
			return errors.New("agobridge: invalid durable pending response")
		}
		index, ok := client.bySequence[pending.Response.Sequence]
		if !ok {
			return errors.New("agobridge: invalid durable pending response")
		}
		evidence := client.state.Evidence[index]
		digestBytes, digestErr := hex.DecodeString(pending.RequestDigest)
		digestMatches := evidence.Status == EvidenceCompleted && evidence.RequestDigest == pending.RequestDigest &&
			evidence.Response != nil && responsesEqual(*evidence.Response, pending.Response)
		conflictMatches := evidence.Status == EvidenceCompleted && evidence.RequestDigest != pending.RequestDigest &&
			digestErr == nil && len(digestBytes) == sha256.Size && validIdentity(pending.Response.Nonce, maxNonceBytes) &&
			len(pending.Response.Payload) == 0 && pending.Response.Error != nil &&
			pending.Response.Error.Code == ErrorConflict && pending.Response.Error.Message == "sequence already used by a different request"
		if !digestMatches && !conflictMatches {
			return errors.New("agobridge: invalid durable pending response")
		}
	}
	return nil
}

func (client *Client) rebuildIndexes() {
	client.bySequence = make(map[uint64]int, len(client.state.Evidence))
	client.nonces = make(map[string]uint64, len(client.state.Evidence))
	for index, evidence := range client.state.Evidence {
		client.bySequence[evidence.Sequence] = index
		if _, exists := client.nonces[evidence.Nonce]; !exists {
			client.nonces[evidence.Nonce] = evidence.Sequence
		}
	}
}

func (client *Client) commitLocked(ctx context.Context, expectedRevision uint64) error {
	next := cloneState(client.state)
	revision, err := client.config.StateStore.Commit(ctx, client.identity, expectedRevision, next)
	if err != nil {
		return fmt.Errorf("agobridge: commit durable state: %w", err)
	}
	if revision <= expectedRevision {
		return errors.New("agobridge: state store returned a non-monotonic revision")
	}
	client.state.Revision = revision
	return nil
}

func validIdentity(value string, max int) bool {
	return value != "" && len(value) <= max && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}

func ensureEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("agobridge: relay returned multiple JSON values")
		}
		return fmt.Errorf("agobridge: response exceeds limit or is malformed: %w", err)
	}
	return nil
}

func sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
