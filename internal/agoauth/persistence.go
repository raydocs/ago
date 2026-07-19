package agoauth

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/sys/unix"
)

var (
	ErrPersistenceUnavailable = errors.New("agoauth: credential persistence unavailable")
	ErrPersistenceCorrupt     = errors.New("agoauth: credential persistence corrupt")
	ErrPersistenceUnsafe      = errors.New("agoauth: credential persistence path is unsafe")
)

// CredentialPersistence durably owns registered credentials and their replay
// counters. AdvanceSignCount must be an atomic compare-and-swap across all
// processes using the same backing state.
type CredentialPersistence interface {
	LoadCredentials() ([]Credential, error)
	StoreCredential(Credential) error
	AdvanceSignCount(expected Credential, next uint32) error
}

// FileCredentialPersistence stores credentials in a private, atomically
// replaced JSON file. A sibling lock file serializes readers and writers across
// processes. The parent directory must already exist and have mode 0700.
type FileCredentialPersistence struct {
	mu        sync.RWMutex
	directory *os.File
	baseName  string
	lockName  string
}

const credentialFileVersion = 1

type credentialFile struct {
	Version     int          `json:"version"`
	Credentials []Credential `json:"credentials"`
	Checksum    string       `json:"checksum"`
}

type credentialFilePayload struct {
	Version     int          `json:"version"`
	Credentials []Credential `json:"credentials"`
}

func NewFileCredentialPersistence(path string) (*FileCredentialPersistence, error) {
	if path == "" {
		return nil, ErrPersistenceUnsafe
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPersistenceUnsafe, err)
	}
	dirPath := filepath.Dir(absolute)
	baseName := filepath.Base(absolute)
	fd, err := unix.Open(dirPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open parent: %v", ErrPersistenceUnsafe, err)
	}
	directory := os.NewFile(uintptr(fd), dirPath)
	if err := validateDescriptor(directory, unix.S_IFDIR, 0o700); err != nil {
		directory.Close()
		return nil, fmt.Errorf("%w: parent directory must be owned by the effective user and mode 0700", ErrPersistenceUnsafe)
	}
	return &FileCredentialPersistence{directory: directory, baseName: baseName, lockName: baseName + ".lock"}, nil
}

// Close releases the pinned parent-directory descriptor. It must not race with
// calls using the persistence object.
func (persistence *FileCredentialPersistence) Close() error {
	persistence.mu.Lock()
	defer persistence.mu.Unlock()
	if persistence.directory == nil {
		return nil
	}
	err := persistence.directory.Close()
	persistence.directory = nil
	return err
}

func (persistence *FileCredentialPersistence) LoadCredentials() ([]Credential, error) {
	var credentials []Credential
	err := persistence.withLock(func() error {
		state, err := persistence.readLocked()
		if err != nil {
			return err
		}
		credentials = cloneCredentials(state.Credentials)
		return nil
	})
	return credentials, err
}

func (persistence *FileCredentialPersistence) StoreCredential(credential Credential) error {
	return persistence.withLock(func() error {
		state, err := persistence.readLocked()
		if err != nil {
			return err
		}
		found := false
		for index := range state.Credentials {
			existing := state.Credentials[index]
			if existing.RPID != credential.RPID || existing.ID != credential.ID {
				continue
			}
			if credential.SignCount < existing.SignCount {
				return ErrSignCountReplay
			}
			state.Credentials[index] = cloneCredential(credential)
			found = true
			break
		}
		if !found {
			state.Credentials = append(state.Credentials, cloneCredential(credential))
		}
		return persistence.writeLocked(state.Credentials)
	})
}

func (persistence *FileCredentialPersistence) AdvanceSignCount(expected Credential, next uint32) error {
	if next == 0 || next <= expected.SignCount {
		return ErrSignCountReplay
	}
	return persistence.withLock(func() error {
		state, err := persistence.readLocked()
		if err != nil {
			return err
		}
		for index := range state.Credentials {
			credential := &state.Credentials[index]
			if credential.RPID != expected.RPID || credential.ID != expected.ID {
				continue
			}
			if credential.ActorID != expected.ActorID || credential.DeviceID != expected.DeviceID || !bytes.Equal(credential.PublicKey, expected.PublicKey) {
				return ErrCredentialInvalid
			}
			if next <= credential.SignCount {
				return ErrSignCountReplay
			}
			credential.SignCount = next
			return persistence.writeLocked(state.Credentials)
		}
		return ErrCredentialInvalid
	})
}

func (persistence *FileCredentialPersistence) withLock(operation func() error) error {
	persistence.mu.RLock()
	defer persistence.mu.RUnlock()
	if persistence.directory == nil {
		return ErrPersistenceUnavailable
	}
	file, err := openPrivateAt(persistence.directory, persistence.lockName, unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("%w: lock: %v", ErrPersistenceUnavailable, err)
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN)
	return operation()
}

func (persistence *FileCredentialPersistence) readLocked() (credentialFile, error) {
	file, err := openPrivateAt(persistence.directory, persistence.baseName, unix.O_RDONLY)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credentialFile{Version: credentialFileVersion, Credentials: []Credential{}}, nil
		}
		return credentialFile{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var state credentialFile
	if err := decoder.Decode(&state); err != nil {
		return credentialFile{}, fmt.Errorf("%w: decode: %v", ErrPersistenceCorrupt, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return credentialFile{}, err
	}
	if state.Version != credentialFileVersion || state.Credentials == nil || !validCredentialChecksum(state) {
		return credentialFile{}, ErrPersistenceCorrupt
	}
	seen := make(map[credentialKey]struct{}, len(state.Credentials))
	for _, credential := range state.Credentials {
		key := credentialKey{rpID: credential.RPID, id: credential.ID}
		if credential.ID == "" || credential.RPID == "" || credential.ActorID == "" || credential.DeviceID == "" || len(credential.PublicKey) == 0 {
			return credentialFile{}, ErrPersistenceCorrupt
		}
		if _, exists := seen[key]; exists {
			return credentialFile{}, ErrPersistenceCorrupt
		}
		seen[key] = struct{}{}
	}
	return state, nil
}

func (persistence *FileCredentialPersistence) writeLocked(credentials []Credential) error {
	state := credentialFile{Version: credentialFileVersion, Credentials: cloneCredentials(credentials)}
	state.Checksum = credentialChecksum(state.Version, state.Credentials)
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("%w: encode: %v", ErrPersistenceUnavailable, err)
	}
	data = append(data, '\n')
	temporaryName, temporary, err := persistence.createTemporaryLocked()
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = unix.Unlinkat(int(persistence.directory.Fd()), temporaryName, 0)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		return fmt.Errorf("%w: write temporary: %v", ErrPersistenceUnavailable, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("%w: sync temporary: %v", ErrPersistenceUnavailable, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("%w: close temporary: %v", ErrPersistenceUnavailable, err)
	}
	directoryFD := int(persistence.directory.Fd())
	if err := unix.Renameat(directoryFD, temporaryName, directoryFD, persistence.baseName); err != nil {
		return fmt.Errorf("%w: replace state: %v", ErrPersistenceUnavailable, err)
	}
	removeTemporary = false
	if err := unix.Fsync(directoryFD); err != nil {
		return fmt.Errorf("%w: sync parent: %v", ErrPersistenceUnavailable, err)
	}
	return nil
}

func (persistence *FileCredentialPersistence) createTemporaryLocked() (string, *os.File, error) {
	for range 8 {
		randomBytes := make([]byte, 16)
		if _, err := rand.Read(randomBytes); err != nil {
			return "", nil, fmt.Errorf("%w: temporary name entropy: %v", ErrPersistenceUnavailable, err)
		}
		name := ".agoauth-" + hex.EncodeToString(randomBytes)
		file, err := openPrivateAt(persistence.directory, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		return name, file, nil
	}
	return "", nil, fmt.Errorf("%w: temporary name collisions", ErrPersistenceUnavailable)
}

func openPrivateAt(directory *os.File, name string, flags int) (*os.File, error) {
	if name == "" || filepath.Base(name) != name {
		return nil, ErrPersistenceUnsafe
	}
	fd, err := unix.Openat(int(directory.Fd()), name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			return nil, ErrPersistenceUnsafe
		}
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		if errors.Is(err, unix.EEXIST) {
			return nil, os.ErrExist
		}
		return nil, fmt.Errorf("%w: open private file: %v", ErrPersistenceUnavailable, err)
	}
	file := os.NewFile(uintptr(fd), name)
	if err := validateDescriptor(file, unix.S_IFREG, 0o600); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

func validateDescriptor(file *os.File, expectedType uint32, expectedPermissions uint32) error {
	var status unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &status); err != nil {
		return fmt.Errorf("%w: stat descriptor: %v", ErrPersistenceUnavailable, err)
	}
	if uint32(status.Mode)&unix.S_IFMT != expectedType || uint32(status.Mode)&0o777 != expectedPermissions || status.Uid != uint32(os.Geteuid()) {
		return ErrPersistenceUnsafe
	}
	return nil
}

func validCredentialChecksum(state credentialFile) bool {
	expected, err := hex.DecodeString(state.Checksum)
	if err != nil || len(expected) != sha256.Size {
		return false
	}
	actual, err := hex.DecodeString(credentialChecksum(state.Version, state.Credentials))
	return err == nil && subtle.ConstantTimeCompare(expected, actual) == 1
}

func credentialChecksum(version int, credentials []Credential) string {
	payload, _ := json.Marshal(credentialFilePayload{Version: version, Credentials: credentials})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrPersistenceCorrupt
	}
	return nil
}

func cloneCredentials(credentials []Credential) []Credential {
	cloned := make([]Credential, len(credentials))
	for index, credential := range credentials {
		cloned[index] = cloneCredential(credential)
	}
	return cloned
}

func cloneCredential(credential Credential) Credential {
	credential.PublicKey = append([]byte(nil), credential.PublicKey...)
	return credential
}
