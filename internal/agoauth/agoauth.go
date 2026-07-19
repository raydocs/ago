// Package agoauth provides an in-memory, transport-independent recent-passkey
// authorization core. It validates WebAuthn assertions after credential lookup
// and response decoding have been performed by the caller.
package agoauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidConfiguration     = errors.New("agoauth: invalid configuration")
	ErrInvalidBinding           = errors.New("agoauth: invalid binding")
	ErrRandomUnavailable        = errors.New("agoauth: secure random unavailable")
	ErrChallengeInvalid         = errors.New("agoauth: challenge invalid or expired")
	ErrBindingMismatch          = errors.New("agoauth: binding mismatch")
	ErrClientDataInvalid        = errors.New("agoauth: client data invalid")
	ErrOriginInvalid            = errors.New("agoauth: origin invalid")
	ErrRPIDInvalid              = errors.New("agoauth: relying party ID invalid")
	ErrAuthenticatorDataInvalid = errors.New("agoauth: authenticator data invalid")
	ErrCredentialInvalid        = errors.New("agoauth: credential invalid")
	ErrSignatureInvalid         = errors.New("agoauth: signature invalid")
	ErrSignCountReplay          = errors.New("agoauth: authenticator sign count did not increase")
	ErrCapacityExceeded         = errors.New("agoauth: authorization store capacity exceeded")
)

// Clock is the time source used for challenge and grant expiry.
type Clock interface {
	Now() time.Time
}

// SignatureVerifier verifies a WebAuthn assertion signature. signedData is
// authenticatorData || SHA-256(clientDataJSON), and publicKey is opaque data
// supplied when the credential is registered.
type SignatureVerifier interface {
	Verify(publicKey, signedData, signature []byte) error
}

type Config struct {
	// RelyingParties maps an exact RP ID to its exact allowed HTTPS origins.
	RelyingParties          map[string][]string
	ChallengeTTL            time.Duration
	GrantTTL                time.Duration
	ChallengeBytes          int
	MaxChallenges           int
	MaxGrants               int
	RequireUserVerification bool
}

type Dependencies struct {
	Clock       Clock
	Random      io.Reader
	Verifier    SignatureVerifier
	Persistence CredentialPersistence
}

// Binding scopes a challenge and resulting grant to one mutation authority.
type Binding struct {
	ThreadID  string
	ProjectID string
	DeviceID  string
	ActorID   string
}

type Credential struct {
	ID        string
	RPID      string
	ActorID   string
	DeviceID  string
	PublicKey []byte
	SignCount uint32
}

type Challenge struct {
	Value     string
	ExpiresAt time.Time
}

// Assertion contains decoded WebAuthn assertion response fields. The core
// parses ClientDataJSON and the fixed authenticator-data prefix itself.
type Assertion struct {
	CredentialID      string
	Binding           Binding
	RPID              string
	ClientDataJSON    []byte
	AuthenticatorData []byte
	Signature         []byte
}

type Grant struct {
	Token        string
	ExpiresAt    time.Time
	CredentialID string
	RPID         string
	SignCount    uint32
}

type MutationAuthorization struct {
	Binding              Binding
	RequireRecentPasskey bool
	GrantToken           string
}

type DecisionReason string

const (
	DecisionNotRequired           DecisionReason = "recent_passkey_not_required"
	DecisionRecentPasskeyValid    DecisionReason = "recent_passkey_valid"
	DecisionRecentPasskeyRequired DecisionReason = "recent_passkey_required"
	DecisionRecentPasskeyExpired  DecisionReason = "recent_passkey_expired"
	DecisionBindingMismatch       DecisionReason = "binding_mismatch"
)

type Decision struct {
	Allowed bool
	Reason  DecisionReason
}

type challengeRecord struct {
	binding   Binding
	rpID      string
	expiresAt time.Time
}

type grantRecord struct {
	binding   Binding
	expiresAt time.Time
}

type credentialRecord struct {
	actorID   string
	deviceID  string
	publicKey []byte
	signCount uint32
	version   uint64
}

type credentialKey struct {
	rpID string
	id   string
}

type Core struct {
	clock                   Clock
	random                  io.Reader
	verifier                SignatureVerifier
	persistence             CredentialPersistence
	rpOrigins               map[string]map[string]struct{}
	challengeTTL            time.Duration
	grantTTL                time.Duration
	challengeBytes          int
	maxChallenges           int
	maxGrants               int
	requireUserVerification bool

	randomMu              sync.Mutex
	verifierMu            sync.Mutex
	mu                    sync.Mutex
	nextCredentialVersion uint64
	challenges            map[string]challengeRecord
	grants                map[string]grantRecord
	credentials           map[credentialKey]credentialRecord
}

func New(config Config, dependencies Dependencies) (*Core, error) {
	if dependencies.Clock == nil || dependencies.Random == nil || dependencies.Verifier == nil ||
		config.ChallengeTTL <= 0 || config.GrantTTL <= 0 || config.ChallengeBytes < 16 ||
		config.MaxChallenges <= 0 || config.MaxGrants <= 0 || len(config.RelyingParties) == 0 {
		return nil, ErrInvalidConfiguration
	}
	rpOrigins := make(map[string]map[string]struct{}, len(config.RelyingParties))
	for rpID, origins := range config.RelyingParties {
		if !validRPID(rpID) || len(origins) == 0 {
			return nil, ErrInvalidConfiguration
		}
		allowed := make(map[string]struct{}, len(origins))
		for _, origin := range origins {
			if !validOrigin(origin) || !originMatchesRPID(origin, rpID) {
				return nil, ErrInvalidConfiguration
			}
			allowed[origin] = struct{}{}
		}
		rpOrigins[rpID] = allowed
	}
	core := &Core{
		clock: dependencies.Clock, random: dependencies.Random, verifier: dependencies.Verifier,
		persistence: dependencies.Persistence,
		rpOrigins:   rpOrigins, challengeTTL: config.ChallengeTTL, grantTTL: config.GrantTTL,
		challengeBytes: config.ChallengeBytes, maxChallenges: config.MaxChallenges, maxGrants: config.MaxGrants,
		requireUserVerification: config.RequireUserVerification,
		challenges:              make(map[string]challengeRecord), grants: make(map[string]grantRecord),
		credentials: make(map[credentialKey]credentialRecord),
	}
	if dependencies.Persistence != nil {
		credentials, err := dependencies.Persistence.LoadCredentials()
		if err != nil {
			return nil, err
		}
		for _, credential := range credentials {
			if err := core.installCredential(credential); err != nil {
				return nil, fmt.Errorf("load credential: %w", err)
			}
		}
	}
	return core, nil
}

// RegisterCredential installs or replaces trusted credential state. Replacing a
// credential must not lower its sign count.
func (core *Core) RegisterCredential(credential Credential) error {
	core.mu.Lock()
	defer core.mu.Unlock()
	return core.storeCredentialLocked(credential)
}

func (core *Core) installCredential(credential Credential) error {
	core.mu.Lock()
	defer core.mu.Unlock()
	return core.installCredentialLocked(credential, false)
}

func (core *Core) storeCredentialLocked(credential Credential) error {
	if err := core.validateCredentialLocked(credential); err != nil {
		return err
	}
	if core.nextCredentialVersion == ^uint64(0) {
		return ErrCredentialInvalid
	}
	if core.persistence != nil {
		if err := core.persistence.StoreCredential(credential); err != nil {
			return fmt.Errorf("%w: %w", ErrPersistenceUnavailable, err)
		}
	}
	return core.installCredentialLocked(credential, true)
}

func (core *Core) installCredentialLocked(credential Credential, allowReplace bool) error {
	if err := core.validateCredentialLocked(credential); err != nil {
		return err
	}
	key := credentialKey{rpID: credential.RPID, id: credential.ID}
	if _, exists := core.credentials[key]; exists && !allowReplace {
		// Persistence loading must contain each RP/credential key only once.
		return ErrPersistenceCorrupt
	}
	core.nextCredentialVersion++
	if core.nextCredentialVersion == 0 {
		return ErrCredentialInvalid
	}
	core.credentials[key] = credentialRecord{
		actorID: credential.ActorID, deviceID: credential.DeviceID,
		publicKey: append([]byte(nil), credential.PublicKey...), signCount: credential.SignCount,
		version: core.nextCredentialVersion,
	}
	return nil
}

func (core *Core) validateCredentialLocked(credential Credential) error {
	if credential.ID == "" || credential.ActorID == "" || credential.DeviceID == "" || len(credential.PublicKey) == 0 {
		return ErrCredentialInvalid
	}
	if _, ok := core.rpOrigins[credential.RPID]; !ok {
		return ErrRPIDInvalid
	}
	key := credentialKey{rpID: credential.RPID, id: credential.ID}
	if existing, ok := core.credentials[key]; ok && credential.SignCount < existing.signCount {
		return ErrSignCountReplay
	}
	return nil
}

func (core *Core) IssueChallenge(binding Binding, rpID string) (Challenge, error) {
	if !binding.valid() {
		return Challenge{}, ErrInvalidBinding
	}
	if _, ok := core.rpOrigins[rpID]; !ok {
		return Challenge{}, ErrRPIDInvalid
	}
	value, err := core.randomToken("")
	if err != nil {
		return Challenge{}, err
	}
	now := core.clock.Now()
	challenge := Challenge{Value: value, ExpiresAt: now.Add(core.challengeTTL)}
	core.mu.Lock()
	core.pruneChallengesLocked(now)
	if len(core.challenges) >= core.maxChallenges {
		core.mu.Unlock()
		return Challenge{}, ErrCapacityExceeded
	}
	if _, exists := core.challenges[value]; exists {
		core.mu.Unlock()
		return Challenge{}, fmt.Errorf("%w: challenge collision", ErrRandomUnavailable)
	}
	core.challenges[value] = challengeRecord{binding: binding, rpID: rpID, expiresAt: challenge.ExpiresAt}
	core.mu.Unlock()
	return challenge, nil
}

func (core *Core) VerifyAssertion(assertion Assertion) (Grant, error) {
	clientData, err := parseClientData(assertion.ClientDataJSON)
	if err != nil {
		return Grant{}, err
	}

	now := core.clock.Now()
	core.mu.Lock()
	challenge, ok := core.challenges[clientData.Challenge]
	if ok {
		delete(core.challenges, clientData.Challenge)
	}
	core.mu.Unlock()
	if !ok || !now.Before(challenge.expiresAt) {
		return Grant{}, ErrChallengeInvalid
	}
	if !assertion.Binding.valid() || assertion.Binding != challenge.binding {
		return Grant{}, ErrBindingMismatch
	}
	if assertion.RPID != challenge.rpID {
		return Grant{}, ErrRPIDInvalid
	}
	allowedOrigins, ok := core.rpOrigins[assertion.RPID]
	if !ok {
		return Grant{}, ErrRPIDInvalid
	}
	if clientData.Type != "webauthn.get" || clientData.Challenge == "" {
		return Grant{}, ErrClientDataInvalid
	}
	if clientData.CrossOrigin {
		return Grant{}, ErrOriginInvalid
	}
	if _, ok := allowedOrigins[clientData.Origin]; !ok {
		return Grant{}, ErrOriginInvalid
	}

	signCount, err := core.validateAuthenticatorData(assertion.RPID, assertion.AuthenticatorData)
	if err != nil {
		return Grant{}, err
	}
	if assertion.CredentialID == "" || len(assertion.Signature) == 0 {
		return Grant{}, ErrCredentialInvalid
	}
	key := credentialKey{rpID: assertion.RPID, id: assertion.CredentialID}
	core.mu.Lock()
	credential, ok := core.credentials[key]
	publicKey := append([]byte(nil), credential.publicKey...)
	core.mu.Unlock()
	if !ok || credential.actorID != assertion.Binding.ActorID || credential.deviceID != assertion.Binding.DeviceID {
		return Grant{}, ErrCredentialInvalid
	}
	credentialVersion := credential.version

	clientHash := sha256.Sum256(assertion.ClientDataJSON)
	signedData := make([]byte, 0, len(assertion.AuthenticatorData)+len(clientHash))
	signedData = append(signedData, assertion.AuthenticatorData...)
	signedData = append(signedData, clientHash[:]...)
	core.verifierMu.Lock()
	verifyErr := core.verifier.Verify(publicKey, signedData, assertion.Signature)
	core.verifierMu.Unlock()
	if verifyErr != nil {
		return Grant{}, fmt.Errorf("%w: %v", ErrSignatureInvalid, verifyErr)
	}
	grantToken, err := core.randomToken("g.")
	if err != nil {
		return Grant{}, err
	}

	core.mu.Lock()
	defer core.mu.Unlock()
	credential, ok = core.credentials[key]
	if !ok || credential.actorID != assertion.Binding.ActorID || credential.deviceID != assertion.Binding.DeviceID || credential.version != credentialVersion {
		return Grant{}, ErrCredentialInvalid
	}
	if signCount == 0 || signCount <= credential.signCount {
		return Grant{}, ErrSignCountReplay
	}
	core.pruneGrantsLocked(now)
	if len(core.grants) >= core.maxGrants {
		return Grant{}, ErrCapacityExceeded
	}
	if _, exists := core.grants[grantToken]; exists {
		return Grant{}, fmt.Errorf("%w: grant collision", ErrRandomUnavailable)
	}
	if core.persistence != nil {
		expected := Credential{
			ID: assertion.CredentialID, RPID: assertion.RPID, ActorID: credential.actorID,
			DeviceID: credential.deviceID, PublicKey: append([]byte(nil), credential.publicKey...), SignCount: credential.signCount,
		}
		if err := core.persistence.AdvanceSignCount(expected, signCount); err != nil {
			if errors.Is(err, ErrSignCountReplay) {
				return Grant{}, err
			}
			return Grant{}, fmt.Errorf("%w: %w", ErrPersistenceUnavailable, err)
		}
	}
	credential.signCount = signCount
	core.credentials[key] = credential
	grant := Grant{
		Token: grantToken, ExpiresAt: now.Add(core.grantTTL), CredentialID: assertion.CredentialID,
		RPID: assertion.RPID, SignCount: signCount,
	}
	core.grants[grantToken] = grantRecord{binding: assertion.Binding, expiresAt: grant.ExpiresAt}
	return grant, nil
}

func (core *Core) AuthorizeMutation(request MutationAuthorization) Decision {
	if !request.RequireRecentPasskey {
		return Decision{Allowed: true, Reason: DecisionNotRequired}
	}
	if !request.Binding.valid() || request.GrantToken == "" {
		return Decision{Reason: DecisionRecentPasskeyRequired}
	}
	now := core.clock.Now()
	core.mu.Lock()
	grant, ok := core.grants[request.GrantToken]
	if ok && !now.Before(grant.expiresAt) {
		delete(core.grants, request.GrantToken)
	}
	core.mu.Unlock()
	if !ok {
		return Decision{Reason: DecisionRecentPasskeyRequired}
	}
	if !now.Before(grant.expiresAt) {
		return Decision{Reason: DecisionRecentPasskeyExpired}
	}
	if grant.binding != request.Binding {
		return Decision{Reason: DecisionBindingMismatch}
	}
	return Decision{Allowed: true, Reason: DecisionRecentPasskeyValid}
}

func (core *Core) validateAuthenticatorData(rpID string, data []byte) (uint32, error) {
	if len(data) < 37 {
		return 0, ErrAuthenticatorDataInvalid
	}
	expectedHash := sha256.Sum256([]byte(rpID))
	if subtle.ConstantTimeCompare(data[:32], expectedHash[:]) != 1 {
		return 0, ErrRPIDInvalid
	}
	const userPresent = 0x01
	const userVerified = 0x04
	if data[32]&userPresent == 0 || core.requireUserVerification && data[32]&userVerified == 0 {
		return 0, ErrAuthenticatorDataInvalid
	}
	return binary.BigEndian.Uint32(data[33:37]), nil
}

func (core *Core) randomToken(prefix string) (string, error) {
	buffer := make([]byte, core.challengeBytes)
	core.randomMu.Lock()
	defer core.randomMu.Unlock()
	if _, err := io.ReadFull(core.random, buffer); err != nil {
		return "", fmt.Errorf("%w: %v", ErrRandomUnavailable, err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buffer), nil
}

func (core *Core) pruneChallengesLocked(now time.Time) {
	for token, challenge := range core.challenges {
		if !now.Before(challenge.expiresAt) {
			delete(core.challenges, token)
		}
	}
}

func (core *Core) pruneGrantsLocked(now time.Time) {
	for token, grant := range core.grants {
		if !now.Before(grant.expiresAt) {
			delete(core.grants, token)
		}
	}
}

type collectedClientData struct {
	Type        string `json:"type"`
	Challenge   string `json:"challenge"`
	Origin      string `json:"origin"`
	CrossOrigin bool   `json:"crossOrigin"`
}

func parseClientData(data []byte) (collectedClientData, error) {
	var parsed collectedClientData
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := decoder.Decode(&parsed); err != nil || parsed.Challenge == "" || parsed.Origin == "" {
		return collectedClientData{}, ErrClientDataInvalid
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return collectedClientData{}, ErrClientDataInvalid
	}
	return parsed, nil
}

func (binding Binding) valid() bool {
	return binding.ThreadID != "" && binding.ProjectID != "" && binding.DeviceID != "" && binding.ActorID != ""
}

func validRPID(rpID string) bool {
	return rpID != "" && rpID == strings.ToLower(rpID) && !strings.ContainsAny(rpID, "/:@ ") && !strings.HasPrefix(rpID, ".") && !strings.HasSuffix(rpID, ".")
}

func validOrigin(origin string) bool {
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == ""
}

func originMatchesRPID(origin, rpID string) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	hostname := strings.ToLower(parsed.Hostname())
	return hostname == rpID || strings.HasSuffix(hostname, "."+rpID)
}
