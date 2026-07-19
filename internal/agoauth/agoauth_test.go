package agoauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testBinding = Binding{
	ThreadID:  "thread-1",
	ProjectID: "project-1",
	DeviceID:  "device-1",
	ActorID:   "actor-1",
}

func TestChallengeIsWebAuthnBufferSourceCompatible(t *testing.T) {
	core, _, _ := newTestCore(t)
	challenge := issueChallenge(t, core, testBinding)
	decoded, err := base64.RawURLEncoding.DecodeString(challenge)
	if err != nil {
		t.Fatalf("challenge is not unpadded base64url: %v", err)
	}
	if got := base64.RawURLEncoding.EncodeToString(decoded); got != challenge {
		t.Fatalf("challenge round trip = %q, want %q", got, challenge)
	}
}

func TestChallengeIsRandomBoundAndSingleUse(t *testing.T) {
	core, clock, verifier := newTestCore(t)
	challenge := issueChallenge(t, core, testBinding)
	assertion := validAssertion(t, challenge, testBinding, 1)

	grant, err := core.VerifyAssertion(assertion)
	if err != nil {
		t.Fatalf("VerifyAssertion() error = %v", err)
	}
	if grant.Token == "" || grant.CredentialID != "credential-1" || grant.RPID != "example.com" || grant.SignCount != 1 || verifier.calls != 1 {
		t.Fatalf("VerifyAssertion() = %#v, verifier calls = %d", grant, verifier.calls)
	}

	if _, err := core.VerifyAssertion(assertion); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("replayed VerifyAssertion() error = %v, want ErrChallengeInvalid", err)
	}

	other := issueChallenge(t, core, testBinding)
	wrongBinding := testBinding
	wrongBinding.ThreadID = "thread-2"
	if _, err := core.VerifyAssertion(validAssertion(t, other, wrongBinding, 2)); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("wrong-binding VerifyAssertion() error = %v, want ErrBindingMismatch", err)
	}
	if _, err := core.VerifyAssertion(validAssertion(t, other, testBinding, 2)); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("challenge after wrong-binding attempt error = %v, want consumed challenge", err)
	}

	expired := issueChallenge(t, core, testBinding)
	clock.Advance(6 * time.Minute)
	if _, err := core.VerifyAssertion(validAssertion(t, expired, testBinding, 2)); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("expired VerifyAssertion() error = %v, want ErrChallengeInvalid", err)
	}
}

func TestAssertionValidatesWebAuthnPolicyAndSignature(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Assertion, *fakeVerifier)
		want   error
	}{
		{name: "origin", mutate: func(a *Assertion, _ *fakeVerifier) {
			a.ClientDataJSON = clientData(t, assertionChallenge(t, *a), "https://evil.example", "webauthn.get", false)
		}, want: ErrOriginInvalid},
		{name: "type", mutate: func(a *Assertion, _ *fakeVerifier) {
			a.ClientDataJSON = clientData(t, assertionChallenge(t, *a), "https://app.example.com", "webauthn.create", false)
		}, want: ErrClientDataInvalid},
		{name: "cross origin", mutate: func(a *Assertion, _ *fakeVerifier) {
			a.ClientDataJSON = clientData(t, assertionChallenge(t, *a), "https://app.example.com", "webauthn.get", true)
		}, want: ErrOriginInvalid},
		{name: "rp id hash", mutate: func(a *Assertion, _ *fakeVerifier) { a.AuthenticatorData[0] ^= 0xff }, want: ErrRPIDInvalid},
		{name: "user presence", mutate: func(a *Assertion, _ *fakeVerifier) { a.AuthenticatorData[32] &^= 0x01 }, want: ErrAuthenticatorDataInvalid},
		{name: "user verification", mutate: func(a *Assertion, _ *fakeVerifier) { a.AuthenticatorData[32] &^= 0x04 }, want: ErrAuthenticatorDataInvalid},
		{name: "signature", mutate: func(_ *Assertion, v *fakeVerifier) { v.err = errors.New("bad signature") }, want: ErrSignatureInvalid},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			core, _, verifier := newTestCore(t)
			challenge := issueChallenge(t, core, testBinding)
			assertion := validAssertion(t, challenge, testBinding, 1)
			test.mutate(&assertion, verifier)
			if _, err := core.VerifyAssertion(assertion); !errors.Is(err, test.want) {
				t.Fatalf("VerifyAssertion() error = %v, want %v", err, test.want)
			}
			if _, err := core.VerifyAssertion(validAssertion(t, challenge, testBinding, 1)); !errors.Is(err, ErrChallengeInvalid) {
				t.Fatalf("failed assertion did not consume challenge: %v", err)
			}
		})
	}
}

func TestSignCountMustIncrease(t *testing.T) {
	core, _, _ := newTestCore(t)
	first := issueChallenge(t, core, testBinding)
	if _, err := core.VerifyAssertion(validAssertion(t, first, testBinding, 7)); err != nil {
		t.Fatalf("first VerifyAssertion() error = %v", err)
	}

	for _, count := range []uint32{7, 6, 0} {
		challenge := issueChallenge(t, core, testBinding)
		if _, err := core.VerifyAssertion(validAssertion(t, challenge, testBinding, count)); !errors.Is(err, ErrSignCountReplay) {
			t.Fatalf("VerifyAssertion(signCount=%d) error = %v, want ErrSignCountReplay", count, err)
		}
	}
}

func TestCredentialIsBoundToRelyingParty(t *testing.T) {
	config := testConfig()
	config.RelyingParties["other.com"] = []string{"https://app.other.com"}
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	core, err := New(config, Dependencies{
		Clock: clock, Random: bytes.NewReader(bytes.Repeat([]byte{0x42}, 128)), Verifier: &fakeVerifier{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := core.RegisterCredential(Credential{
		ID: "credential-1", RPID: "example.com", ActorID: testBinding.ActorID,
		DeviceID: testBinding.DeviceID, PublicKey: []byte("public-key"),
	}); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	challenge, err := core.IssueChallenge(testBinding, "other.com")
	if err != nil {
		t.Fatalf("IssueChallenge() error = %v", err)
	}
	assertion := validAssertion(t, challenge.Value, testBinding, 1)
	assertion.RPID = "other.com"
	assertion.ClientDataJSON = clientData(t, challenge.Value, "https://app.other.com", "webauthn.get", false)
	assertion.AuthenticatorData = authenticatorData("other.com", 0x05, 1)
	if _, err := core.VerifyAssertion(assertion); !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("cross-RP VerifyAssertion() error = %v, want ErrCredentialInvalid", err)
	}
}

func TestRecentGrantAuthorizesOnlyMatchingMutationUntilExpiry(t *testing.T) {
	core, clock, _ := newTestCore(t)

	optional := core.AuthorizeMutation(MutationAuthorization{Binding: testBinding})
	if !optional.Allowed || optional.Reason != DecisionNotRequired {
		t.Fatalf("optional AuthorizeMutation() = %#v", optional)
	}

	missing := core.AuthorizeMutation(MutationAuthorization{Binding: testBinding, RequireRecentPasskey: true})
	if missing.Allowed || missing.Reason != DecisionRecentPasskeyRequired {
		t.Fatalf("missing-grant AuthorizeMutation() = %#v", missing)
	}

	grant := verify(t, core, issueChallenge(t, core, testBinding), testBinding, 1)
	allowed := core.AuthorizeMutation(MutationAuthorization{Binding: testBinding, RequireRecentPasskey: true, GrantToken: grant.Token})
	if !allowed.Allowed || allowed.Reason != DecisionRecentPasskeyValid {
		t.Fatalf("AuthorizeMutation() = %#v", allowed)
	}

	other := testBinding
	other.ProjectID = "project-2"
	denied := core.AuthorizeMutation(MutationAuthorization{Binding: other, RequireRecentPasskey: true, GrantToken: grant.Token})
	if denied.Allowed || denied.Reason != DecisionBindingMismatch {
		t.Fatalf("wrong-binding AuthorizeMutation() = %#v", denied)
	}

	clock.Advance(3 * time.Minute)
	expired := core.AuthorizeMutation(MutationAuthorization{Binding: testBinding, RequireRecentPasskey: true, GrantToken: grant.Token})
	if expired.Allowed || expired.Reason != DecisionRecentPasskeyExpired {
		t.Fatalf("expired AuthorizeMutation() = %#v", expired)
	}
}

func TestChallengeSingleUseIsConcurrencySafe(t *testing.T) {
	core, _, verifier := newTestCore(t)
	challenge := issueChallenge(t, core, testBinding)
	assertion := validAssertion(t, challenge, testBinding, 1)

	const attempts = 32
	start := make(chan struct{})
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := core.VerifyAssertion(assertion); err == nil {
				successes.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := successes.Load(); got != 1 {
		t.Fatalf("successful concurrent assertions = %d, want 1", got)
	}
	if verifier.calls != 1 {
		t.Fatalf("signature verifier calls = %d, want 1", verifier.calls)
	}
}

func TestChallengeCollisionFailsClosed(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	core, err := New(testConfig(), Dependencies{
		Clock: clock, Random: bytes.NewReader(bytes.Repeat([]byte{0x42}, 64)), Verifier: &fakeVerifier{},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := core.IssueChallenge(testBinding, "example.com"); err != nil {
		t.Fatalf("first IssueChallenge() error = %v", err)
	}
	if _, err := core.IssueChallenge(testBinding, "example.com"); !errors.Is(err, ErrRandomUnavailable) {
		t.Fatalf("colliding IssueChallenge() error = %v, want ErrRandomUnavailable", err)
	}
}

func TestGrantCollisionFailsWithoutAdvancingSignCount(t *testing.T) {
	random := bytes.NewReader(bytes.Join([][]byte{
		bytes.Repeat([]byte{0x01}, 32), // first challenge
		bytes.Repeat([]byte{0x09}, 32), // first grant
		bytes.Repeat([]byte{0x02}, 32), // second challenge
		bytes.Repeat([]byte{0x09}, 32), // colliding second grant
		bytes.Repeat([]byte{0x03}, 32), // third challenge
		bytes.Repeat([]byte{0x08}, 32), // third grant
	}, nil))
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	core, err := New(testConfig(), Dependencies{Clock: clock, Random: random, Verifier: &fakeVerifier{}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := core.RegisterCredential(Credential{
		ID: "credential-1", RPID: "example.com", ActorID: testBinding.ActorID,
		DeviceID: testBinding.DeviceID, PublicKey: []byte("public-key"),
	}); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	first := verify(t, core, issueChallenge(t, core, testBinding), testBinding, 1)
	second := issueChallenge(t, core, testBinding)
	if _, err := core.VerifyAssertion(validAssertion(t, second, testBinding, 2)); !errors.Is(err, ErrRandomUnavailable) {
		t.Fatalf("colliding grant VerifyAssertion() error = %v, want ErrRandomUnavailable", err)
	}
	if decision := core.AuthorizeMutation(MutationAuthorization{
		Binding: testBinding, RequireRecentPasskey: true, GrantToken: first.Token,
	}); !decision.Allowed {
		t.Fatalf("first grant was overwritten: %#v", decision)
	}
	verify(t, core, issueChallenge(t, core, testBinding), testBinding, 2)
}

func TestChallengeCapacityIsBoundedAndExpiredEntriesArePruned(t *testing.T) {
	config := testConfig()
	config.MaxChallenges = 2
	random := bytes.NewReader(bytes.Join([][]byte{
		bytes.Repeat([]byte{0x01}, 32), bytes.Repeat([]byte{0x02}, 32),
		bytes.Repeat([]byte{0x03}, 32), bytes.Repeat([]byte{0x04}, 32),
	}, nil))
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	core, err := New(config, Dependencies{Clock: clock, Random: random, Verifier: &fakeVerifier{}})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	for range 2 {
		if _, err := core.IssueChallenge(testBinding, "example.com"); err != nil {
			t.Fatalf("IssueChallenge() error = %v", err)
		}
	}
	if _, err := core.IssueChallenge(testBinding, "example.com"); !errors.Is(err, ErrCapacityExceeded) {
		t.Fatalf("capacity IssueChallenge() error = %v, want ErrCapacityExceeded", err)
	}
	clock.Advance(6 * time.Minute)
	if _, err := core.IssueChallenge(testBinding, "example.com"); err != nil {
		t.Fatalf("IssueChallenge() after expiry error = %v", err)
	}
}

func TestCredentialReplacementDuringVerificationFailsClosed(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	verifier := &blockingVerifier{started: make(chan struct{}), release: make(chan struct{})}
	core, err := New(testConfig(), Dependencies{
		Clock: clock, Random: bytes.NewReader(bytes.Repeat([]byte{0x42}, 128)), Verifier: verifier,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	credential := Credential{ID: "credential-1", RPID: "example.com", ActorID: testBinding.ActorID, DeviceID: testBinding.DeviceID, PublicKey: []byte("public-key")}
	if err := core.RegisterCredential(credential); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	challenge := issueChallenge(t, core, testBinding)
	result := make(chan error, 1)
	go func() {
		_, err := core.VerifyAssertion(validAssertion(t, challenge, testBinding, 1))
		result <- err
	}()
	<-verifier.started
	credential.PublicKey = []byte("replacement-key")
	if err := core.RegisterCredential(credential); err != nil {
		t.Fatalf("replacement RegisterCredential() error = %v", err)
	}
	close(verifier.release)
	if err := <-result; !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("VerifyAssertion() error = %v, want ErrCredentialInvalid", err)
	}
}

func TestFailsClosedOnInvalidConfigurationAndRandomFailure(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	verifier := &fakeVerifier{}
	if _, err := New(Config{}, Dependencies{Clock: clock, Random: bytes.NewReader(nil), Verifier: verifier}); err == nil {
		t.Fatal("New() accepted an empty configuration")
	}
	invalidRPOrigin := testConfig()
	invalidRPOrigin.RelyingParties = map[string][]string{"example.com": {"https://unrelated.example"}}
	if _, err := New(invalidRPOrigin, Dependencies{Clock: clock, Random: bytes.NewReader(nil), Verifier: verifier}); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("New() with unrelated RP ID and origin error = %v, want ErrInvalidConfiguration", err)
	}
	core, err := New(testConfig(), Dependencies{Clock: clock, Random: bytes.NewReader(nil), Verifier: verifier})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if _, err := core.IssueChallenge(testBinding, "example.com"); !errors.Is(err, ErrRandomUnavailable) {
		t.Fatalf("IssueChallenge() error = %v, want ErrRandomUnavailable", err)
	}
	decision := core.AuthorizeMutation(MutationAuthorization{Binding: testBinding, RequireRecentPasskey: true})
	if decision.Allowed {
		t.Fatalf("AuthorizeMutation() = %#v, want denied", decision)
	}
}

func newTestCore(t *testing.T) (*Core, *fakeClock, *fakeVerifier) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_700_000_000, 0)}
	verifier := &fakeVerifier{}
	random := bytes.NewReader(bytes.Repeat([]byte{0x42}, 4096))
	core, err := New(testConfig(), Dependencies{Clock: clock, Random: random, Verifier: verifier})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := core.RegisterCredential(Credential{
		ID: "credential-1", RPID: "example.com", ActorID: testBinding.ActorID, DeviceID: testBinding.DeviceID, PublicKey: []byte("public-key"),
	}); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	return core, clock, verifier
}

func testConfig() Config {
	return Config{
		RelyingParties:          map[string][]string{"example.com": {"https://app.example.com"}},
		ChallengeTTL:            5 * time.Minute,
		GrantTTL:                2 * time.Minute,
		ChallengeBytes:          32,
		MaxChallenges:           256,
		MaxGrants:               256,
		RequireUserVerification: true,
	}
}

func issueChallenge(t *testing.T, core *Core, binding Binding) string {
	t.Helper()
	challenge, err := core.IssueChallenge(binding, "example.com")
	if err != nil {
		t.Fatalf("IssueChallenge() error = %v", err)
	}
	return challenge.Value
}

func verify(t *testing.T, core *Core, challenge string, binding Binding, count uint32) Grant {
	t.Helper()
	grant, err := core.VerifyAssertion(validAssertion(t, challenge, binding, count))
	if err != nil {
		t.Fatalf("VerifyAssertion() error = %v", err)
	}
	return grant
}

func validAssertion(t *testing.T, challenge string, binding Binding, count uint32) Assertion {
	t.Helper()
	return Assertion{
		CredentialID:      "credential-1",
		Binding:           binding,
		RPID:              "example.com",
		ClientDataJSON:    clientData(t, challenge, "https://app.example.com", "webauthn.get", false),
		AuthenticatorData: authenticatorData("example.com", 0x05, count),
		Signature:         []byte("signature"),
	}
}

func assertionChallenge(t *testing.T, assertion Assertion) string {
	t.Helper()
	var data struct {
		Challenge string `json:"challenge"`
	}
	if err := json.Unmarshal(assertion.ClientDataJSON, &data); err != nil {
		t.Fatal(err)
	}
	return data.Challenge
}

func clientData(t *testing.T, challenge, origin, typ string, crossOrigin bool) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": crossOrigin,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func authenticatorData(rpID string, flags byte, count uint32) []byte {
	data := make([]byte, 37)
	hash := sha256.Sum256([]byte(rpID))
	copy(data, hash[:])
	data[32] = flags
	binary.BigEndian.PutUint32(data[33:37], count)
	return data
}

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (clock *fakeClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.now
}

func (clock *fakeClock) Advance(duration time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.now = clock.now.Add(duration)
}

type fakeVerifier struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (verifier *fakeVerifier) Verify(publicKey, signedData, signature []byte) error {
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	verifier.calls++
	if !bytes.Equal(publicKey, []byte("public-key")) || len(signedData) != 69 || !bytes.Equal(signature, []byte("signature")) {
		return errors.New("unexpected verification input")
	}
	return verifier.err
}

type blockingVerifier struct {
	started chan struct{}
	release chan struct{}
}

func (verifier *blockingVerifier) Verify(_, _, _ []byte) error {
	close(verifier.started)
	<-verifier.release
	return nil
}
