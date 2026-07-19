package agodaemon

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"claudexflow/internal/agoauth"
	"claudexflow/internal/agobridge"
	"claudexflow/internal/agocoordinator"
	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
)

const (
	BridgeActionProjection    = "thread.projection"
	BridgeActionSubmit        = "thread.submit"
	BridgeActionArchive       = "thread.archive"
	BridgeActionAuthChallenge = "auth.challenge"
	BridgeActionAuthAssertion = "auth.assertion"
)

type BridgePublication struct {
	ProjectID string   `json:"project_id"`
	ThreadID  string   `json:"thread_id"`
	Actions   []string `json:"actions"`
}

type BridgePublicationConfig struct {
	Publications []BridgePublication `json:"publications"`
}

type bridgePublications struct {
	store       *agothreadstore.Store
	coordinator *agocoordinator.Coordinator
	published   map[string]map[string]struct{}
	auth        *bridgeAuthTransport
}

func NewBridgePublications(store *agothreadstore.Store, coordinator *agocoordinator.Coordinator, config BridgePublicationConfig) (agobridge.Publications, error) {
	return newBridgePublications(store, coordinator, config, nil)
}

func newBridgePublications(store *agothreadstore.Store, coordinator *agocoordinator.Coordinator, config BridgePublicationConfig, auth *bridgeAuthTransport) (agobridge.Publications, error) {
	if store == nil || coordinator == nil || len(config.Publications) == 0 {
		return nil, errors.New("agodaemon: bridge requires store, coordinator, and explicit publications")
	}
	published := make(map[string]map[string]struct{}, len(config.Publications))
	for _, publication := range config.Publications {
		if strings.TrimSpace(publication.ProjectID) != publication.ProjectID || publication.ProjectID == "" ||
			strings.TrimSpace(publication.ThreadID) != publication.ThreadID || publication.ThreadID == "" || len(publication.Actions) == 0 {
			return nil, errors.New("agodaemon: invalid bridge publication")
		}
		key := publication.ProjectID + "\x00" + publication.ThreadID
		if _, exists := published[key]; exists {
			return nil, errors.New("agodaemon: duplicate bridge publication")
		}
		actions := make(map[string]struct{}, len(publication.Actions))
		for _, action := range publication.Actions {
			if action != BridgeActionProjection && action != BridgeActionSubmit && action != BridgeActionArchive &&
				action != BridgeActionAuthChallenge && action != BridgeActionAuthAssertion {
				return nil, fmt.Errorf("agodaemon: unsupported bridge action %q", action)
			}
			if _, exists := actions[action]; exists {
				return nil, errors.New("agodaemon: duplicate bridge action")
			}
			if (action == BridgeActionAuthChallenge || action == BridgeActionAuthAssertion) && auth == nil {
				return nil, errors.New("agodaemon: bridge authentication action requires recent-passkey authority")
			}
			actions[action] = struct{}{}
		}
		published[key] = actions
	}
	return &bridgePublications{store: store, coordinator: coordinator, published: published, auth: auth}, nil
}

func (publications *bridgePublications) ResolvePublished(ctx context.Context, projectID, threadID, action string) (agobridge.Operation, bool) {
	actions, ok := publications.published[projectID+"\x00"+threadID]
	if !ok {
		return nil, false
	}
	if _, ok := actions[action]; !ok {
		return nil, false
	}
	if (action == BridgeActionAuthChallenge || action == BridgeActionAuthAssertion) && publications.auth == nil {
		return nil, false
	}
	thread, err := publications.store.Thread(ctx, threadID)
	if err != nil || thread.Project.ProjectID != projectID {
		return nil, false
	}
	mailbox, err := publications.store.Mailbox(ctx, threadID)
	if err != nil || mailbox.Activity != agoprotocol.ActivityRunning || mailbox.ActiveTurnID == "" {
		return nil, false
	}
	return bridgeOperation{publications: publications, projectID: projectID, threadID: threadID, action: action}, true
}

type bridgeOperation struct {
	publications *bridgePublications
	projectID    string
	threadID     string
	action       string
}

func (operation bridgeOperation) Mutation() bool {
	return operation.action == BridgeActionSubmit || operation.action == BridgeActionArchive
}

// BridgeAuthChallengeRequest begins a WebAuthn assertion ceremony. RPID must
// exactly match an operator-configured relying party. Account, device, project,
// and thread scope are always derived from the authenticated bridge envelope.
type BridgeAuthChallengeRequest struct {
	RPID string `json:"rp_id"`
}

// BridgeAuthChallengeResponse is the assertion challenge and its expiry.
// Challenge is unpadded base64url; Web and Apple clients decode it to bytes for
// the platform WebAuthn challenge BufferSource/Data value.
type BridgeAuthChallengeResponse struct {
	Challenge string    `json:"challenge"`
	RPID      string    `json:"rp_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// BridgeAuthAssertionRequest carries browser/iOS WebAuthn assertion response
// fields. Every binary field uses unpadded base64url. CredentialID is the
// operator-provisioned credential's unpadded base64url identifier. Registration
// and attestation are intentionally not exposed by the bridge.
type BridgeAuthAssertionRequest struct {
	CredentialID      string `json:"credential_id"`
	RPID              string `json:"rp_id"`
	ClientDataJSON    string `json:"client_data_json"`
	AuthenticatorData string `json:"authenticator_data"`
	Signature         string `json:"signature"`
}

// BridgeAuthAssertionResponse returns a scope-bound, short-lived, single-use
// token. Supply it as authorization_token on one thread.submit or thread.archive.
type BridgeAuthAssertionResponse struct {
	AuthorizationToken string    `json:"authorization_token"`
	ExpiresAt          time.Time `json:"expires_at"`
}

func (operation bridgeOperation) Execute(ctx context.Context, request agobridge.ExecutionRequest) (json.RawMessage, error) {
	if request.ProjectID != operation.projectID || request.ThreadID != operation.threadID || request.Action != operation.action || request.AccountID == "" || request.DeviceID == "" {
		return nil, errors.New("bridge request identity does not match the published operation")
	}
	if _, ok := operation.publications.ResolvePublished(ctx, operation.projectID, operation.threadID, operation.action); !ok {
		return nil, errors.New("published thread is no longer running")
	}
	switch operation.action {
	case BridgeActionAuthChallenge:
		var input BridgeAuthChallengeRequest
		if err := decodeBridgePayload(request.Payload, &input); err != nil || input.RPID == "" {
			return nil, errors.New("invalid authentication challenge request")
		}
		challenge, err := operation.publications.auth.core.IssueChallenge(bridgeAuthBinding(request), input.RPID)
		if err != nil {
			return nil, errors.New("authentication challenge denied")
		}
		return json.Marshal(BridgeAuthChallengeResponse{Challenge: challenge.Value, RPID: input.RPID, ExpiresAt: challenge.ExpiresAt})
	case BridgeActionAuthAssertion:
		var input BridgeAuthAssertionRequest
		if err := decodeBridgePayload(request.Payload, &input); err != nil {
			return nil, errors.New("invalid authentication assertion request")
		}
		clientData, err := decodeBase64URL(input.ClientDataJSON, 64<<10)
		if err != nil {
			return nil, errors.New("invalid authentication assertion request")
		}
		authenticatorData, err := decodeBase64URL(input.AuthenticatorData, 64<<10)
		if err != nil {
			return nil, errors.New("invalid authentication assertion request")
		}
		signature, err := decodeBase64URL(input.Signature, 64<<10)
		if err != nil {
			return nil, errors.New("invalid authentication assertion request")
		}
		credentialID, err := decodeBase64URL(input.CredentialID, 1024)
		if err != nil || len(credentialID) == 0 || input.RPID == "" {
			return nil, errors.New("invalid authentication assertion request")
		}
		grant, err := operation.publications.auth.core.VerifyAssertion(agoauth.Assertion{
			CredentialID: input.CredentialID, Binding: bridgeAuthBinding(request), RPID: input.RPID,
			ClientDataJSON: clientData, AuthenticatorData: authenticatorData, Signature: signature,
		})
		if err != nil {
			return nil, errors.New("authentication assertion denied")
		}
		operation.publications.auth.register(grant, bridgeAuthBinding(request))
		return json.Marshal(BridgeAuthAssertionResponse{AuthorizationToken: grant.Token, ExpiresAt: grant.ExpiresAt})
	case BridgeActionProjection:
		var input struct {
			AfterSequence uint64 `json:"after_sequence"`
			Limit         int    `json:"limit"`
		}
		if err := decodeBridgePayload(request.Payload, &input); err != nil || input.Limit < 1 || input.Limit > 1000 {
			return nil, errors.New("invalid projection request")
		}
		projection, err := operation.publications.store.ClientProjection(ctx, operation.threadID, input.AfterSequence, input.Limit)
		if err != nil {
			return nil, err
		}
		return json.Marshal(projection)
	case BridgeActionSubmit:
		var input struct {
			ExpectedSequence uint64                 `json:"expected_sequence"`
			Content          json.RawMessage        `json:"content"`
			Class            agoprotocol.QueueClass `json:"class"`
		}
		if err := decodeBridgePayload(request.Payload, &input); err != nil || input.ExpectedSequence == 0 || !json.Valid(input.Content) ||
			(input.Class != agoprotocol.QueueNormal && input.Class != agoprotocol.QueueSteer) {
			return nil, errors.New("invalid submit request")
		}
		commandID := bridgeCommandID(request)
		expected := input.ExpectedSequence
		state, err := operation.publications.coordinator.Submit(ctx, agoprotocol.Command{
			SchemaVersion: agoprotocol.SchemaVersion, CommandID: commandID, IdempotencyKey: commandID,
			ActorID: request.AccountID, Type: agoprotocol.CommandMessageSubmit, ThreadID: operation.threadID,
			ExpectedSequence: &expected,
		}, agothreadstore.MessageInput{Content: append(json.RawMessage(nil), input.Content...), Class: input.Class})
		if err != nil {
			return nil, err
		}
		return json.Marshal(state)
	case BridgeActionArchive:
		var input struct {
			ExpectedSequence uint64 `json:"expected_sequence"`
		}
		if err := decodeBridgePayload(request.Payload, &input); err != nil || input.ExpectedSequence == 0 {
			return nil, errors.New("invalid archive request")
		}
		commandID := bridgeCommandID(request)
		expected := input.ExpectedSequence
		result, err := operation.publications.store.ArchiveThread(ctx, agoprotocol.Command{
			SchemaVersion: agoprotocol.SchemaVersion, CommandID: commandID, IdempotencyKey: commandID,
			ActorID: request.AccountID, Type: agothreadstore.CommandThreadArchive, ThreadID: operation.threadID,
			ExpectedSequence: &expected,
		})
		if err != nil {
			return nil, err
		}
		return json.Marshal(result)
	default:
		return nil, errors.New("unsupported bridge action")
	}
}

func bridgeAuthBinding(request agobridge.ExecutionRequest) agoauth.Binding {
	return agoauth.Binding{ThreadID: request.ThreadID, ProjectID: request.ProjectID, DeviceID: request.DeviceID, ActorID: request.AccountID}
}

func decodeBase64URL(value string, maximum int) ([]byte, error) {
	if value == "" || strings.Contains(value, "=") {
		return nil, errors.New("base64url value is required and must be unpadded")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) > maximum || base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, errors.New("invalid base64url value")
	}
	return decoded, nil
}

func bridgeCommandID(request agobridge.ExecutionRequest) string {
	encoded := fmt.Sprintf("%s\x00%s\x00%d\x00%s", request.AccountID, request.DeviceID, request.Sequence, request.Nonce)
	digest := sha256.Sum256([]byte(encoded))
	return "bridge-" + hex.EncodeToString(digest[:])
}

func decodeBridgePayload(payload json.RawMessage, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("multiple JSON values")
	}
	return nil
}

type RecentPasskeyAuthorization struct {
	Core      *agoauth.Core
	transport *bridgeAuthTransport
}

type bridgeAuthTransport struct {
	core   *agoauth.Core
	mu     sync.Mutex
	grants map[string]bridgeGrantRecord
}

type bridgeGrantRecord struct {
	binding   agoauth.Binding
	expiresAt time.Time
}

const maxBridgeGrantRecords = 1024

func newBridgeAuthTransport(core *agoauth.Core) *bridgeAuthTransport {
	if core == nil {
		return nil
	}
	return &bridgeAuthTransport{core: core, grants: make(map[string]bridgeGrantRecord)}
}

func (transport *bridgeAuthTransport) register(grant agoauth.Grant, binding agoauth.Binding) {
	transport.mu.Lock()
	if len(transport.grants) >= maxBridgeGrantRecords {
		var oldestToken string
		var oldestExpiry time.Time
		for token, record := range transport.grants {
			if oldestToken == "" || record.expiresAt.Before(oldestExpiry) {
				oldestToken, oldestExpiry = token, record.expiresAt
			}
		}
		delete(transport.grants, oldestToken)
	}
	transport.grants[grant.Token] = bridgeGrantRecord{binding: binding, expiresAt: grant.ExpiresAt}
	transport.mu.Unlock()
}

func (transport *bridgeAuthTransport) authorize(token string, binding agoauth.Binding) error {
	transport.mu.Lock()
	registered, ok := transport.grants[token]
	if ok {
		delete(transport.grants, token)
	}
	transport.mu.Unlock()
	if !ok || registered.binding != binding {
		return errors.New("recent passkey grant is unavailable or has already been used")
	}
	decision := transport.core.AuthorizeMutation(agoauth.MutationAuthorization{
		Binding: binding, RequireRecentPasskey: true, GrantToken: token,
	})
	if !decision.Allowed {
		return fmt.Errorf("recent passkey denied: %s", decision.Reason)
	}
	return nil
}

// StandardPasskeyVerifier verifies PKIX-encoded ECDSA P-256, RSA, and Ed25519
// credential keys using the WebAuthn signed-data conventions supported by Ago.
type StandardPasskeyVerifier struct{}

func (StandardPasskeyVerifier) Verify(publicKey, signedData, signature []byte) error {
	parsed, err := x509.ParsePKIXPublicKey(publicKey)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(signedData)
	switch key := parsed.(type) {
	case *ecdsa.PublicKey:
		if !ecdsa.VerifyASN1(key, digest[:], signature) {
			return errors.New("invalid ECDSA signature")
		}
	case *rsa.PublicKey:
		if err := rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature); err != nil {
			return err
		}
	case ed25519.PublicKey:
		if !ed25519.Verify(key, signedData, signature) {
			return errors.New("invalid Ed25519 signature")
		}
	default:
		return errors.New("unsupported passkey public key")
	}
	return nil
}

func (authorization RecentPasskeyAuthorization) AuthorizeMutation(_ context.Context, request agobridge.MutationAuthorization) error {
	if authorization.Core == nil {
		return errors.New("recent passkey authority is unavailable")
	}
	binding := agoauth.Binding{
		ThreadID: request.ThreadID, ProjectID: request.ProjectID, DeviceID: request.DeviceID, ActorID: request.AccountID,
	}
	if authorization.transport != nil {
		return authorization.transport.authorize(request.AuthorizationToken, binding)
	}
	decision := authorization.Core.AuthorizeMutation(agoauth.MutationAuthorization{
		Binding:              binding,
		RequireRecentPasskey: true,
		GrantToken:           request.AuthorizationToken,
	})
	if !decision.Allowed {
		return fmt.Errorf("recent passkey denied: %s", decision.Reason)
	}
	return nil
}

type OutboundBridgeConfig struct {
	Client        agobridge.Config
	Publications  BridgePublicationConfig
	Store         *agothreadstore.Store
	Coordinator   *agocoordinator.Coordinator
	Authorization agobridge.Authorization
	Closer        io.Closer
}

type OutboundBridge struct {
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

func StartOutboundBridge(parent context.Context, config OutboundBridgeConfig) (*OutboundBridge, error) {
	var authTransport *bridgeAuthTransport
	switch authorization := config.Authorization.(type) {
	case RecentPasskeyAuthorization:
		authTransport = newBridgeAuthTransport(authorization.Core)
		authorization.transport = authTransport
		config.Authorization = authorization
	case *RecentPasskeyAuthorization:
		if authorization != nil {
			authTransport = newBridgeAuthTransport(authorization.Core)
			authorization.transport = authTransport
		}
	}
	publications, err := newBridgePublications(config.Store, config.Coordinator, config.Publications, authTransport)
	if err != nil {
		return nil, err
	}
	config.Client.Publications = publications
	config.Client.Authorization = config.Authorization
	client, err := agobridge.New(parent, config.Client)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	bridge := &OutboundBridge{cancel: cancel, done: make(chan error, 1)}
	go func() {
		err := client.Run(ctx)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		if config.Closer != nil {
			if closeErr := config.Closer.Close(); err == nil {
				err = closeErr
			}
		}
		bridge.done <- err
		close(bridge.done)
	}()
	return bridge, nil
}

func (bridge *OutboundBridge) Errors() <-chan error { return bridge.done }

func (bridge *OutboundBridge) Shutdown(ctx context.Context) error {
	bridge.once.Do(bridge.cancel)
	select {
	case err := <-bridge.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func AllowedBridgeProjects(config BridgePublicationConfig) map[string]struct{} {
	projects := make(map[string]struct{})
	for _, publication := range config.Publications {
		projects[publication.ProjectID] = struct{}{}
	}
	return projects
}

func SortedBridgeProjects(config BridgePublicationConfig) []string {
	projects := AllowedBridgeProjects(config)
	result := make([]string, 0, len(projects))
	for project := range projects {
		result = append(result, project)
	}
	sort.Strings(result)
	return result
}
