//go:build darwin || linux

package agobridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	fileStateVersion     = 1
	maxStateFileBytes    = 64 << 20
	maxStoredEvidence    = defaultMaxRemembered
	privateDirectoryMode = 0o700
	privateFileMode      = 0o600
)

type FileStateStore struct {
	rootPath string
	root     *os.File
}

type diskState struct {
	Version  int            `json:"version"`
	Identity BridgeIdentity `json:"identity"`
	State    State          `json:"state"`
	Checksum string         `json:"checksum"`
}

// NewFileStateStore opens or creates an absolute, canonical, owner-private
// directory used exclusively for durable bridge state.
func NewFileStateStore(rootPath string) (*FileStateStore, error) {
	if !filepath.IsAbs(rootPath) || filepath.Clean(rootPath) != rootPath {
		return nil, errors.New("agobridge: state directory must be an absolute canonical path")
	}
	parent := filepath.Dir(rootPath)
	canonicalParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return nil, errors.New("agobridge: state directory parent must resolve canonically")
	}
	rootPath = filepath.Join(canonicalParent, filepath.Base(rootPath))
	info, err := os.Lstat(rootPath)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(rootPath, privateDirectoryMode); err != nil {
			return nil, fmt.Errorf("agobridge: create state directory: %w", err)
		}
		if err := os.Chmod(rootPath, privateDirectoryMode); err != nil {
			return nil, fmt.Errorf("agobridge: secure state directory: %w", err)
		}
		info, err = os.Lstat(rootPath)
	}
	if err != nil {
		return nil, fmt.Errorf("agobridge: inspect state directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != privateDirectoryMode {
		return nil, errors.New("agobridge: state directory must be a non-symlink directory with mode 0700")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return nil, errors.New("agobridge: state directory must be owned by the current user")
	}
	canonicalRoot, err := filepath.EvalSymlinks(rootPath)
	if err != nil || canonicalRoot != rootPath {
		return nil, errors.New("agobridge: state directory must be canonical and symlink-free")
	}
	fd, err := syscall.Open(rootPath, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("agobridge: open state directory without following links: %w", err)
	}
	root := os.NewFile(uintptr(fd), rootPath)
	if root == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("agobridge: open state directory")
	}
	return &FileStateStore{rootPath: rootPath, root: root}, nil
}

func (store *FileStateStore) Close() error {
	if store == nil || store.root == nil {
		return nil
	}
	err := store.root.Close()
	store.root = nil
	return err
}

func (store *FileStateStore) Load(ctx context.Context, identity BridgeIdentity) (State, error) {
	if err := validateBridgeIdentity(identity); err != nil {
		return State{}, err
	}
	lock, err := store.lock(ctx, identity)
	if err != nil {
		return State{}, err
	}
	defer unlockAndClose(lock)
	return store.loadLocked(identity)
}

func (store *FileStateStore) Commit(ctx context.Context, identity BridgeIdentity, expectedRevision uint64, state State) (uint64, error) {
	if err := validateBridgeIdentity(identity); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	lock, err := store.lock(ctx, identity)
	if err != nil {
		return 0, err
	}
	defer unlockAndClose(lock)
	current, err := store.loadLocked(identity)
	if err != nil {
		return 0, err
	}
	if current.Revision != expectedRevision {
		return current.Revision, ErrStateConflict
	}
	if err := ctx.Err(); err != nil {
		return current.Revision, err
	}
	state = cloneState(state)
	state.Revision = expectedRevision + 1
	if state.Revision == 0 {
		return current.Revision, errors.New("agobridge: state revision overflow")
	}
	if err := validateFileState(identity, state); err != nil {
		return current.Revision, err
	}
	encoded, err := encodeDiskState(identity, state)
	if err != nil {
		return current.Revision, err
	}
	if len(encoded) > maxStateFileBytes {
		return current.Revision, errors.New("agobridge: durable state exceeds file size bound")
	}
	if err := store.atomicReplace(stateKey(identity)+".state", encoded); err != nil {
		return current.Revision, err
	}
	return state.Revision, nil
}

func (store *FileStateStore) lock(ctx context.Context, identity BridgeIdentity) (*os.File, error) {
	if store == nil || store.root == nil {
		return nil, errors.New("agobridge: state store is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := filepath.Join(store.rootPath, stateKey(identity)+".lock")
	flags := syscall.O_RDWR | syscall.O_CREAT | syscall.O_EXCL | syscall.O_NOFOLLOW
	fd, err := syscall.Open(path, flags, privateFileMode)
	created := err == nil
	if errors.Is(err, syscall.EEXIST) {
		fd, err = syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("agobridge: open identity lock without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return nil, errors.New("agobridge: open identity lock")
	}
	if created {
		if err := file.Chmod(privateFileMode); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("agobridge: secure identity lock: %w", err)
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("agobridge: sync identity lock: %w", err)
		}
		if err := store.root.Sync(); err != nil {
			_ = file.Close()
			return nil, fmt.Errorf("agobridge: sync state directory: %w", err)
		}
	}
	if err := requirePrivateRegular(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, fmt.Errorf("agobridge: lock durable state: %w", err)
		}
		if err := sleep(ctx, 10*time.Millisecond); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		unlockAndClose(file)
		return nil, err
	}
	return file, nil
}

func (store *FileStateStore) loadLocked(identity BridgeIdentity) (State, error) {
	path := filepath.Join(store.rootPath, stateKey(identity)+".state")
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, syscall.ENOENT) {
		return State{}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("agobridge: open durable state without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = syscall.Close(fd)
		return State{}, errors.New("agobridge: open durable state")
	}
	defer file.Close()
	if err := requirePrivateRegular(file); err != nil {
		return State{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxStateFileBytes+1))
	if err != nil {
		return State{}, fmt.Errorf("agobridge: read durable state: %w", err)
	}
	if len(data) == 0 || len(data) > maxStateFileBytes {
		return State{}, errors.New("agobridge: durable state is empty or exceeds size bound")
	}
	return decodeDiskState(identity, data)
}

func (store *FileStateStore) atomicReplace(name string, data []byte) error {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return fmt.Errorf("agobridge: generate state temp name: %w", err)
	}
	tempName := ".tmp-" + hex.EncodeToString(random)
	tempPath := filepath.Join(store.rootPath, tempName)
	fd, err := syscall.Open(tempPath, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW, privateFileMode)
	if err != nil {
		return fmt.Errorf("agobridge: create exclusive state temp file: %w", err)
	}
	file := os.NewFile(uintptr(fd), tempPath)
	if file == nil {
		_ = syscall.Close(fd)
		_ = os.Remove(tempPath)
		return errors.New("agobridge: create state temp file")
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(privateFileMode); err != nil {
		return fmt.Errorf("agobridge: secure state temp file: %w", err)
	}
	if _, err := io.Copy(file, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("agobridge: write durable state: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("agobridge: sync durable state file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("agobridge: close durable state file: %w", err)
	}
	if err := os.Rename(tempPath, filepath.Join(store.rootPath, name)); err != nil {
		return fmt.Errorf("agobridge: atomically replace durable state: %w", err)
	}
	keep = true
	if err := store.root.Sync(); err != nil {
		return fmt.Errorf("agobridge: sync durable state directory: %w", err)
	}
	return nil
}

func encodeDiskState(identity BridgeIdentity, state State) ([]byte, error) {
	envelope := diskState{Version: fileStateVersion, Identity: identity, State: cloneState(state)}
	checksum, err := diskStateChecksum(envelope)
	if err != nil {
		return nil, err
	}
	envelope.Checksum = checksum
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("agobridge: encode durable state: %w", err)
	}
	return append(encoded, '\n'), nil
}

func decodeDiskState(identity BridgeIdentity, data []byte) (State, error) {
	var envelope diskState
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return State{}, fmt.Errorf("agobridge: decode durable state: %w", err)
	}
	if err := ensureEOF(decoder); err != nil {
		return State{}, err
	}
	if envelope.Version != fileStateVersion || envelope.Identity != identity {
		return State{}, errors.New("agobridge: durable state version or identity mismatch")
	}
	want, err := diskStateChecksum(envelope)
	if err != nil || envelope.Checksum == "" || envelope.Checksum != want {
		return State{}, errors.New("agobridge: durable state checksum mismatch")
	}
	if err := validateFileState(identity, envelope.State); err != nil {
		return State{}, err
	}
	return cloneState(envelope.State), nil
}

func diskStateChecksum(envelope diskState) (string, error) {
	envelope.Checksum = ""
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return "", fmt.Errorf("agobridge: checksum durable state: %w", err)
	}
	return sha256Sum(encoded), nil
}

func validateFileState(identity BridgeIdentity, state State) error {
	if len(state.Evidence) > maxStoredEvidence || state.Cursor > uint64(len(state.Evidence)) ||
		uint64(len(state.Evidence)) > state.Cursor+1 {
		return errors.New("agobridge: durable state evidence exceeds bound")
	}
	for index, evidence := range state.Evidence {
		if evidence.Sequence != uint64(index+1) || !validIdentity(evidence.Nonce, maxNonceBytes) ||
			len(evidence.RequestDigest) != sha256.Size*2 ||
			(evidence.Status != EvidencePrepared && evidence.Status != EvidenceCompleted) {
			return errors.New("agobridge: invalid durable state evidence")
		}
		digest, err := hex.DecodeString(evidence.RequestDigest)
		if err != nil || hex.EncodeToString(digest) != evidence.RequestDigest {
			return errors.New("agobridge: invalid durable state digest")
		}
		if evidence.Status == EvidencePrepared {
			if evidence.Response != nil || index != len(state.Evidence)-1 || evidence.Sequence < state.Cursor || evidence.Sequence > state.Cursor+1 {
				return errors.New("agobridge: invalid prepared durable evidence")
			}
			continue
		}
		if evidence.Sequence > state.Cursor || evidence.Response == nil || evidence.Response.Sequence != evidence.Sequence ||
			evidence.Response.Nonce != evidence.Nonce || evidence.Response.AccountID != identity.AccountID ||
			evidence.Response.DeviceID != identity.DeviceID || (len(evidence.Response.Payload) > 0 && !json.Valid(evidence.Response.Payload)) {
			return errors.New("agobridge: invalid completed durable evidence")
		}
	}
	if state.Pending != nil {
		pending := state.Pending
		if pending.Response.Sequence == 0 || pending.Response.Sequence > uint64(len(state.Evidence)) || pending.Response.Sequence != state.Cursor ||
			pending.Response.AccountID != identity.AccountID || pending.Response.DeviceID != identity.DeviceID {
			return errors.New("agobridge: invalid durable pending response")
		}
		evidence := state.Evidence[pending.Response.Sequence-1]
		exact := evidence.Status == EvidenceCompleted && evidence.RequestDigest == pending.RequestDigest &&
			evidence.Response != nil && responsesEqual(*evidence.Response, pending.Response)
		digest, digestErr := hex.DecodeString(pending.RequestDigest)
		conflict := evidence.Status == EvidenceCompleted && evidence.RequestDigest != pending.RequestDigest &&
			digestErr == nil && len(digest) == sha256.Size && hex.EncodeToString(digest) == pending.RequestDigest &&
			len(pending.Response.Payload) == 0 && validIdentity(pending.Response.Nonce, maxNonceBytes) && pending.Response.Error != nil &&
			pending.Response.Error.Code == ErrorConflict && pending.Response.Error.Message == "sequence already used by a different request"
		if !exact && !conflict {
			return errors.New("agobridge: durable pending response does not match evidence")
		}
	}
	return nil
}

func validateBridgeIdentity(identity BridgeIdentity) error {
	if !validIdentity(identity.AccountID, maxIdentityBytes) || !validIdentity(identity.DeviceID, maxIdentityBytes) {
		return errors.New("agobridge: invalid bridge identity")
	}
	return nil
}

func stateKey(identity BridgeIdentity) string {
	return sha256Sum([]byte("agobridge-state-v1\x00" + identity.AccountID + "\x00" + identity.DeviceID))
}

func sha256Sum(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func requirePrivateRegular(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("agobridge: inspect private state file: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != privateFileMode {
		return errors.New("agobridge: state files must be regular files with mode 0600")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Geteuid() {
		return errors.New("agobridge: state files must be owned by the current user")
	}
	return nil
}

func unlockAndClose(file *os.File) {
	if file == nil {
		return
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}
