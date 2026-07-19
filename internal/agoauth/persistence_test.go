package agoauth

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestFilePersistenceSurvivesRestartAndRejectsReusedSignCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	first := newFilePersistentCore(t, path, 0x11)
	if err := first.RegisterCredential(testCredential()); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	verify(t, first, issueChallenge(t, first, testBinding), testBinding, 7)

	second := newFilePersistentCore(t, path, 0x22)
	replayed := issueChallenge(t, second, testBinding)
	if _, err := second.VerifyAssertion(validAssertion(t, replayed, testBinding, 7)); !errors.Is(err, ErrSignCountReplay) {
		t.Fatalf("replayed VerifyAssertion() error = %v, want ErrSignCountReplay", err)
	}
	verify(t, second, issueChallenge(t, second, testBinding), testBinding, 8)
}

func TestPersistenceFailureDoesNotIssueGrantOrAdvanceMemoryState(t *testing.T) {
	persistence := &failingPersistence{}
	core := newCoreWithPersistence(t, persistence, 0x31)
	if err := core.RegisterCredential(testCredential()); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	persistence.advanceErr = errors.New("disk unavailable")
	challenge := issueChallenge(t, core, testBinding)
	grant, err := core.VerifyAssertion(validAssertion(t, challenge, testBinding, 1))
	if !errors.Is(err, ErrPersistenceUnavailable) || grant.Token != "" {
		t.Fatalf("VerifyAssertion() = %#v, %v; want no grant and ErrPersistenceUnavailable", grant, err)
	}
	if decision := core.AuthorizeMutation(MutationAuthorization{
		Binding: testBinding, RequireRecentPasskey: true, GrantToken: grant.Token,
	}); decision.Allowed {
		t.Fatalf("AuthorizeMutation() = %#v, want denied", decision)
	}

	persistence.advanceErr = nil
	verify(t, core, issueChallenge(t, core, testBinding), testBinding, 1)
}

func TestConcurrentCoresCannotCommitTheSameSignCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	first := newFilePersistentCore(t, path, 0x41)
	if err := first.RegisterCredential(testCredential()); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	second := newFilePersistentCore(t, path, 0x51)
	assertions := []struct {
		core      *Core
		assertion Assertion
	}{
		{core: first, assertion: validAssertion(t, issueChallenge(t, first, testBinding), testBinding, 1)},
		{core: second, assertion: validAssertion(t, issueChallenge(t, second, testBinding), testBinding, 1)},
	}

	start := make(chan struct{})
	var successes atomic.Int32
	var replays atomic.Int32
	var wg sync.WaitGroup
	for _, attempt := range assertions {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := attempt.core.VerifyAssertion(attempt.assertion)
			switch {
			case err == nil:
				successes.Add(1)
			case errors.Is(err, ErrSignCountReplay):
				replays.Add(1)
			default:
				t.Errorf("VerifyAssertion() error = %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if successes.Load() != 1 || replays.Load() != 1 {
		t.Fatalf("successes = %d, replays = %d; want 1 each", successes.Load(), replays.Load())
	}

	reopened := newFilePersistentCore(t, path, 0x61)
	if _, err := reopened.VerifyAssertion(validAssertion(t, issueChallenge(t, reopened, testBinding), testBinding, 1)); !errors.Is(err, ErrSignCountReplay) {
		t.Fatalf("reopened replay error = %v, want ErrSignCountReplay", err)
	}
}

func TestStaleCoreCanAdvancePastDurableCountButCannotReuseIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	first := newFilePersistentCore(t, path, 0x62)
	if err := first.RegisterCredential(testCredential()); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	stale := newFilePersistentCore(t, path, 0x63)
	verify(t, first, issueChallenge(t, first, testBinding), testBinding, 1)
	verify(t, stale, issueChallenge(t, stale, testBinding), testBinding, 2)
	if _, err := first.VerifyAssertion(validAssertion(t, issueChallenge(t, first, testBinding), testBinding, 2)); !errors.Is(err, ErrSignCountReplay) {
		t.Fatalf("reused count error = %v, want ErrSignCountReplay", err)
	}
}

func TestCrossCoreCredentialReplacementInvalidatesInFlightAssertion(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "credentials.json")
	persistence := openFilePersistence(t, path)
	verifier := &blockingVerifier{started: make(chan struct{}), release: make(chan struct{})}
	first, err := New(testConfig(), Dependencies{
		Clock: &fakeClock{now: time.Unix(1_700_000_000, 0)}, Random: &incrementingReader{value: 0x64},
		Verifier: verifier, Persistence: persistence,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := first.RegisterCredential(testCredential()); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	second := newFilePersistentCore(t, path, 0x65)
	assertion := validAssertion(t, issueChallenge(t, first, testBinding), testBinding, 1)
	result := make(chan error, 1)
	go func() {
		_, err := first.VerifyAssertion(assertion)
		result <- err
	}()
	<-verifier.started
	replacement := testCredential()
	replacement.PublicKey = []byte("replacement-key")
	if err := second.RegisterCredential(replacement); err != nil {
		t.Fatalf("replacement RegisterCredential() error = %v", err)
	}
	close(verifier.release)
	if err := <-result; !errors.Is(err, ErrCredentialInvalid) {
		t.Fatalf("VerifyAssertion() error = %v, want ErrCredentialInvalid", err)
	}
}

func TestRestartDropsChallengesAndGrants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	first := newFilePersistentCore(t, path, 0x71)
	if err := first.RegisterCredential(testCredential()); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	staleChallenge := issueChallenge(t, first, testBinding)
	grant := verify(t, first, issueChallenge(t, first, testBinding), testBinding, 1)

	restarted := newFilePersistentCore(t, path, 0x72)
	if _, err := restarted.VerifyAssertion(validAssertion(t, staleChallenge, testBinding, 2)); !errors.Is(err, ErrChallengeInvalid) {
		t.Fatalf("pre-restart challenge error = %v, want ErrChallengeInvalid", err)
	}
	decision := restarted.AuthorizeMutation(MutationAuthorization{
		Binding: testBinding, RequireRecentPasskey: true, GrantToken: grant.Token,
	})
	if decision.Allowed || decision.Reason != DecisionRecentPasskeyRequired {
		t.Fatalf("pre-restart grant decision = %#v, want required", decision)
	}
}

func TestFilePersistenceFailsClosedOnCorruptionSymlinksAndLoosePermissions(t *testing.T) {
	t.Run("corrupt", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "credentials.json")
		if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
			t.Fatal(err)
		}
		persistence := openFilePersistence(t, path)
		if _, err := New(testConfig(), Dependencies{
			Clock: &fakeClock{now: time.Unix(1_700_000_000, 0)}, Random: bytes.NewReader(bytes.Repeat([]byte{1}, 128)),
			Verifier: &fakeVerifier{}, Persistence: persistence,
		}); !errors.Is(err, ErrPersistenceCorrupt) {
			t.Fatalf("New() error = %v, want ErrPersistenceCorrupt", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte(`{"version":1,"credentials":[]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "credentials.json")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		persistence := openFilePersistence(t, path)
		if _, err := persistence.LoadCredentials(); !errors.Is(err, ErrPersistenceUnsafe) {
			t.Fatalf("LoadCredentials() error = %v, want ErrPersistenceUnsafe", err)
		}
	})

	t.Run("permissions", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "credentials.json")
		if err := os.WriteFile(path, []byte(`{"version":1,"credentials":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
		persistence := openFilePersistence(t, path)
		if _, err := persistence.LoadCredentials(); !errors.Is(err, ErrPersistenceUnsafe) {
			t.Fatalf("LoadCredentials() error = %v, want ErrPersistenceUnsafe", err)
		}
	})
}

func TestFilePersistenceCreatesPrivateAtomicState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	core := newFilePersistentCore(t, path, 0x81)
	if err := core.RegisterCredential(testCredential()); err != nil {
		t.Fatalf("RegisterCredential() error = %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential file mode = %v, want regular 0600", info.Mode())
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".agoauth-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain after atomic write: %v", matches)
	}
}

func TestFilePersistencePinsParentDirectory(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "private")
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(original, "credentials.json")
	persistence := openFilePersistence(t, path)
	pinned := filepath.Join(root, "pinned")
	if err := os.Rename(original, pinned); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := persistence.StoreCredential(testCredential()); err != nil {
		t.Fatalf("StoreCredential() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(pinned, "credentials.json")); err != nil {
		t.Fatalf("pinned credential state missing: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement directory received state: %v", err)
	}
}

func newFilePersistentCore(t *testing.T, path string, randomByte byte) *Core {
	t.Helper()
	return newCoreWithPersistence(t, openFilePersistence(t, path), randomByte)
}

func openFilePersistence(t *testing.T, path string) *FileCredentialPersistence {
	t.Helper()
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("chmod persistence directory: %v", err)
	}
	persistence, err := NewFileCredentialPersistence(path)
	if err != nil {
		t.Fatalf("NewFileCredentialPersistence() error = %v", err)
	}
	t.Cleanup(func() { _ = persistence.Close() })
	return persistence
}

func newCoreWithPersistence(t *testing.T, persistence CredentialPersistence, randomByte byte) *Core {
	t.Helper()
	core, err := New(testConfig(), Dependencies{
		Clock: &fakeClock{now: time.Unix(1_700_000_000, 0)}, Random: &incrementingReader{value: randomByte},
		Verifier: &fakeVerifier{}, Persistence: persistence,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return core
}

func testCredential() Credential {
	return Credential{
		ID: "credential-1", RPID: "example.com", ActorID: testBinding.ActorID,
		DeviceID: testBinding.DeviceID, PublicKey: []byte("public-key"),
	}
}

type failingPersistence struct {
	credentials []Credential
	advanceErr  error
}

type incrementingReader struct {
	value byte
}

func (reader *incrementingReader) Read(buffer []byte) (int, error) {
	for index := range buffer {
		buffer[index] = reader.value
	}
	reader.value++
	return len(buffer), nil
}

func (persistence *failingPersistence) LoadCredentials() ([]Credential, error) {
	return append([]Credential(nil), persistence.credentials...), nil
}

func (persistence *failingPersistence) StoreCredential(credential Credential) error {
	persistence.credentials = []Credential{credential}
	return nil
}

func (persistence *failingPersistence) AdvanceSignCount(_ Credential, _ uint32) error {
	return persistence.advanceErr
}
