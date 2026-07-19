// Package agoattachments persists immutable, thread-owned attachment bytes.
package agoattachments

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
	"regexp"
	"sync"

	"claudexflow/internal/agoprotocol"
	"golang.org/x/sys/unix"
)

var (
	ErrConflict     = errors.New("attachment conflicts with immutable stored attachment")
	ErrNotFound     = errors.New("attachment not found")
	ErrUnauthorized = errors.New("attachment owner does not match")
	ErrCorrupt      = errors.New("attachment storage is corrupt")
)

const maxMetadataBytes = 2048

// StorageDirectory is the private directory created below the injected root.
const StorageDirectory = "attachments"

var ownerIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Owner is the exact project and thread pair authorized to use an attachment.
type Owner struct {
	ProjectID string `json:"project_id"`
	ThreadID  string `json:"thread_id"`
}

func (owner Owner) Validate() error {
	if !ownerIDPattern.MatchString(owner.ProjectID) {
		return fmt.Errorf("project_id must be a 1-128 byte ASCII identifier")
	}
	if !ownerIDPattern.MatchString(owner.ThreadID) {
		return fmt.Errorf("thread_id must be a 1-128 byte ASCII identifier")
	}
	return nil
}

// Store owns an open descriptor for a private injected storage directory.
type Store struct {
	mu     sync.RWMutex
	rootFD int
	closed bool
}

type metadata struct {
	Owner Owner                     `json:"owner"`
	Ref   agoprotocol.AttachmentRef `json:"ref"`
}

// Open roots a store below an existing injected directory. The injected root
// is opened without following symlinks; storage is kept in a private child.
func Open(root string) (*Store, error) {
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("open attachment root: %w", err)
	}
	storageFD, err := ensureDirectory(fd, StorageDirectory)
	_ = unix.Close(fd)
	if err != nil {
		return nil, fmt.Errorf("open private attachment directory: %w", err)
	}
	store := &Store{rootFD: storageFD}
	for _, name := range []string{"blobs", "refs"} {
		dirFD, err := ensureDirectory(storageFD, name)
		if err != nil {
			_ = store.Close()
			return nil, err
		}
		_ = unix.Close(dirFD)
	}
	return store, nil
}

func (store *Store) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed {
		return nil
	}
	store.closed = true
	return unix.Close(store.rootFD)
}

// Upload verifies bytes against ref and atomically publishes both the
// content-addressed blob and its immutable ownership metadata. An exact retry
// succeeds; reusing an attachment ID with any changed metadata is a conflict.
func (store *Store) Upload(ctx context.Context, owner Owner, ref agoprotocol.AttachmentRef, source io.Reader) error {
	if err := owner.Validate(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if source == nil {
		return fmt.Errorf("attachment source is required")
	}
	content, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: source}, int64(agoprotocol.MaxAttachmentBytes)+1))
	if err != nil {
		return fmt.Errorf("read attachment: %w", err)
	}
	if err := agoprotocol.ValidateAttachmentBytes(ref, content); err != nil {
		return err
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return fmt.Errorf("attachment store is closed")
	}
	encoded, err := json.Marshal(metadata{Owner: owner, Ref: ref})
	if err != nil {
		return fmt.Errorf("encode attachment metadata: %w", err)
	}
	if len(encoded) > maxMetadataBytes {
		return fmt.Errorf("attachment metadata exceeds %d bytes", maxMetadataBytes)
	}
	return store.uploadWithMetadataLock(owner, ref, content, encoded)
}

// Get authorizes the exact owner and immutable reference, then returns bytes
// only after re-verifying their size and digest.
func (store *Store) Get(ctx context.Context, owner Owner, ref agoprotocol.AttachmentRef) ([]byte, error) {
	opened, err := store.OpenAttachment(ctx, owner, ref)
	if err != nil {
		return nil, err
	}
	defer opened.Close()
	return io.ReadAll(opened)
}

// OpenAttachment is the streaming-shaped counterpart to Get. Verification is
// completed before return, so the reader is backed by a bounded memory copy.
func (store *Store) OpenAttachment(ctx context.Context, owner Owner, ref agoprotocol.AttachmentRef) (io.ReadCloser, error) {
	if err := owner.Validate(); err != nil {
		return nil, err
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed {
		return nil, fmt.Errorf("attachment store is closed")
	}
	stored, err := store.readMetadata(ref.AttachmentID)
	if err != nil {
		return nil, err
	}
	if stored.Owner != owner {
		return nil, ErrUnauthorized
	}
	if stored.Ref != ref {
		return nil, ErrConflict
	}
	content, err := store.readBlob(ctx, stored.Ref)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

// Open is an alias for OpenAttachment retained as the concise integration API.
func (store *Store) Open(ctx context.Context, owner Owner, ref agoprotocol.AttachmentRef) (io.ReadCloser, error) {
	return store.OpenAttachment(ctx, owner, ref)
}

func (store *Store) publishBlob(ref agoprotocol.AttachmentRef, content []byte) error {
	dirFD, err := store.openShard("blobs", ref.SHA256[:2])
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	return publishImmutable(dirFD, ref.SHA256, content, func(existing []byte) error {
		if err := agoprotocol.ValidateAttachmentBytes(ref, existing); err != nil {
			return fmt.Errorf("%w: blob %s: %v", ErrCorrupt, ref.SHA256, err)
		}
		return nil
	}, int(agoprotocol.MaxAttachmentBytes))
}

func (store *Store) uploadWithMetadataLock(owner Owner, ref agoprotocol.AttachmentRef, content, encoded []byte) error {
	identity := sha256.Sum256([]byte(ref.AttachmentID))
	name := hex.EncodeToString(identity[:])
	dirFD, err := store.openShard("refs", name[:2])
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	lockFD, err := lockAt(dirFD, ".lock-"+name)
	if err != nil {
		return fmt.Errorf("lock attachment metadata: %w", err)
	}
	defer unlockAndClose(lockFD)

	existing, err := readFileAt(dirFD, name, maxMetadataBytes)
	if err == nil {
		stored, decodeErr := decodeMetadata(existing, ref.AttachmentID)
		if decodeErr != nil {
			return decodeErr
		}
		if err := unix.Fsync(dirFD); err != nil {
			return fmt.Errorf("fsync existing attachment metadata: %w", err)
		}
		if stored.Owner != owner || stored.Ref != ref || !bytes.Equal(existing, encoded) {
			return ErrConflict
		}
		return store.publishBlob(ref, content)
	}
	if !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("%w: read existing metadata: %v", ErrCorrupt, err)
	}
	if err := store.publishBlob(ref, content); err != nil {
		return err
	}
	return publishImmutableLocked(dirFD, name, encoded, maxMetadataBytes)
}

func (store *Store) readMetadata(attachmentID string) (metadata, error) {
	identity := sha256.Sum256([]byte(attachmentID))
	name := hex.EncodeToString(identity[:])
	dirFD, err := store.openExistingShard("refs", name[:2])
	if errors.Is(err, unix.ENOENT) {
		return metadata{}, ErrNotFound
	}
	if err != nil {
		return metadata{}, fmt.Errorf("%w: open metadata shard: %v", ErrCorrupt, err)
	}
	defer unix.Close(dirFD)
	raw, err := readFileAt(dirFD, name, maxMetadataBytes)
	if errors.Is(err, unix.ENOENT) {
		return metadata{}, ErrNotFound
	}
	if err != nil {
		return metadata{}, fmt.Errorf("%w: read attachment metadata: %v", ErrCorrupt, err)
	}
	return decodeMetadata(raw, attachmentID)
}

func decodeMetadata(raw []byte, attachmentID string) (metadata, error) {
	var stored metadata
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return metadata{}, fmt.Errorf("%w: decode metadata: %v", ErrCorrupt, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return metadata{}, fmt.Errorf("%w: decode metadata: %v", ErrCorrupt, err)
	}
	if err := stored.Owner.Validate(); err != nil {
		return metadata{}, fmt.Errorf("%w: metadata owner: %v", ErrCorrupt, err)
	}
	if err := stored.Ref.Validate(); err != nil || stored.Ref.AttachmentID != attachmentID {
		return metadata{}, fmt.Errorf("%w: invalid metadata reference", ErrCorrupt)
	}
	return stored, nil
}

func (store *Store) readBlob(ctx context.Context, ref agoprotocol.AttachmentRef) ([]byte, error) {
	dirFD, err := store.openExistingShard("blobs", ref.SHA256[:2])
	if err != nil {
		return nil, fmt.Errorf("%w: open blob shard: %v", ErrCorrupt, err)
	}
	defer unix.Close(dirFD)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content, err := readFileAt(dirFD, ref.SHA256, int(agoprotocol.MaxAttachmentBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: read blob: %v", ErrCorrupt, err)
	}
	if err := agoprotocol.ValidateAttachmentBytes(ref, content); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	return content, nil
}

func (store *Store) openShard(base, shard string) (int, error) {
	baseFD, err := openDirectoryAt(store.rootFD, base)
	if err != nil {
		return -1, err
	}
	defer unix.Close(baseFD)
	return ensureDirectory(baseFD, shard)
}

func (store *Store) openExistingShard(base, shard string) (int, error) {
	baseFD, err := openDirectoryAt(store.rootFD, base)
	if err != nil {
		return -1, err
	}
	defer unix.Close(baseFD)
	return openDirectoryAt(baseFD, shard)
}

func ensureDirectory(parentFD int, name string) (int, error) {
	if err := unix.Mkdirat(parentFD, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, fmt.Errorf("create attachment directory %q: %w", name, err)
	} else if err == nil {
		if err := unix.Fsync(parentFD); err != nil {
			return -1, fmt.Errorf("fsync attachment parent: %w", err)
		}
	}
	return openDirectoryAt(parentFD, name)
}

func openDirectoryAt(parentFD int, name string) (int, error) {
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	if err := requirePrivateDirectory(fd); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func requirePrivateDirectory(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&0o077 != 0 {
		return fmt.Errorf("directory permissions %04o are not private", stat.Mode&0o777)
	}
	return nil
}

func publishImmutable(dirFD int, name string, content []byte, verifyExisting func([]byte) error, maxBytes int) error {
	lockFD, err := lockAt(dirFD, ".lock-"+name)
	if err != nil {
		return fmt.Errorf("open attachment lock: %w", err)
	}
	defer unlockAndClose(lockFD)

	if existing, err := readFileAt(dirFD, name, maxBytes); err == nil {
		verificationErr := verifyExisting(existing)
		if syncErr := unix.Fsync(dirFD); syncErr != nil {
			return fmt.Errorf("fsync existing attachment directory: %w", syncErr)
		}
		return verificationErr
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	return publishImmutableLocked(dirFD, name, content, maxBytes)
}

func publishImmutableLocked(dirFD int, name string, content []byte, maxBytes int) error {
	if len(content) > maxBytes {
		return fmt.Errorf("attachment file exceeds %d byte bound", maxBytes)
	}
	temp, err := randomTempName()
	if err != nil {
		return err
	}
	tempFD, err := unix.Openat(dirFD, temp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create attachment temporary file: %w", err)
	}
	removeTemp := true
	defer func() {
		_ = unix.Close(tempFD)
		if removeTemp {
			_ = unix.Unlinkat(dirFD, temp, 0)
		}
	}()
	if err := writeAll(tempFD, content); err != nil {
		return err
	}
	if err := unix.Fsync(tempFD); err != nil {
		return fmt.Errorf("fsync attachment temporary file: %w", err)
	}
	if err := unix.Close(tempFD); err != nil {
		return fmt.Errorf("close attachment temporary file: %w", err)
	}
	tempFD = -1
	if err := unix.Renameat(dirFD, temp, dirFD, name); err != nil {
		return fmt.Errorf("publish attachment: %w", err)
	}
	removeTemp = false
	if err := unix.Fsync(dirFD); err != nil {
		return fmt.Errorf("fsync attachment directory: %w", err)
	}
	return nil
}

func lockAt(dirFD int, name string) (int, error) {
	fd, err := openLockAt(dirFD, name)
	if err != nil {
		return -1, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func unlockAndClose(fd int) {
	_ = unix.Flock(fd, unix.LOCK_UN)
	_ = unix.Close(fd)
}

func openLockAt(dirFD int, name string) (int, error) {
	for {
		fd, err := unix.Openat(dirFD, name, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err == nil {
			if statErr := requirePrivateRegularFile(fd); statErr != nil {
				_ = unix.Close(fd)
				return -1, statErr
			}
			return fd, nil
		}
		if !errors.Is(err, unix.ENOENT) {
			return -1, err
		}
		fd, err = unix.Openat(dirFD, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
		if err == nil {
			if syncErr := unix.Fsync(dirFD); syncErr != nil {
				_ = unix.Close(fd)
				return -1, syncErr
			}
			return fd, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return -1, err
		}
	}
}

func readFileAt(dirFD int, name string, maxBytes int) ([]byte, error) {
	fd, err := unix.Openat(dirFD, name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	if err := requirePrivateRegularFile(fd); err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	file := newFDReader(fd)
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(content) > maxBytes {
		return nil, fmt.Errorf("stored file exceeds read bound")
	}
	return content, nil
}

func requirePrivateRegularFile(fd int) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return fmt.Errorf("stored path is not a regular file")
	}
	if stat.Mode&0o077 != 0 {
		return fmt.Errorf("stored file permissions %04o are not private", stat.Mode&0o777)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("metadata contains multiple JSON values")
		}
		return err
	}
	return nil
}

type fdReader struct{ fd int }

func newFDReader(fd int) *fdReader { return &fdReader{fd: fd} }
func (reader *fdReader) Read(p []byte) (int, error) {
	read, err := unix.Read(reader.fd, p)
	if err == nil && read == 0 {
		return 0, io.EOF
	}
	return read, err
}
func (reader *fdReader) Close() error {
	if reader.fd < 0 {
		return nil
	}
	err := unix.Close(reader.fd)
	reader.fd = -1
	return err
}

func writeAll(fd int, content []byte) error {
	for len(content) > 0 {
		written, err := unix.Write(fd, content)
		if err != nil {
			return fmt.Errorf("write attachment temporary file: %w", err)
		}
		content = content[written:]
	}
	return nil
}

func randomTempName() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate attachment temporary name: %w", err)
	}
	return ".tmp-" + hex.EncodeToString(value[:]), nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (reader *contextReader) Read(p []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(p)
}
