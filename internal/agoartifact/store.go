// Package agoartifact stores untrusted executor output inside a directory Ago
// owns.
//
// The boundary is a byte stream, not a path. An executor hands over content;
// it never names a location. That removes traversal, symlink, and hard-link
// escape as input vectors entirely rather than trying to validate them away.
// Identifiers are generated, so a caller cannot influence where bytes land, and
// a display name is metadata only — it never becomes part of a path.
//
// Size limits are enforced against bytes actually read, never against a stat
// result a caller could lie about.
package agoartifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	ErrNotFound  = errors.New("artifact does not exist")
	ErrTooLarge  = errors.New("artifact exceeds the configured byte limit")
	ErrCorrupt   = errors.New("artifact bytes do not match their recorded metadata")
	ErrBadID     = errors.New("artifact id is not well formed")
	ErrDuplicate = errors.New("artifact id already exists")
)

// DefaultMaxBytes bounds a single artifact.
const DefaultMaxBytes int64 = 8 << 20

const (
	objectsDir = "objects"
	tempDir    = "tmp"
	// idBytes is the length of a generated identifier. 32 bytes of entropy
	// makes an identifier unguessable as well as unique.
	idBytes = 32
)

// Descriptor is the durable, non-secret metadata for one stored artifact.
type Descriptor struct {
	ID string `json:"id"`
	// Type is a caller-declared media type, recorded for display only. It is
	// never used to choose a path or to execute anything.
	Type string `json:"type"`
	// DisplayName is shown to a user. It is sanitized and is never a path.
	DisplayName string    `json:"display_name"`
	Bytes       int64     `json:"bytes"`
	SHA256      string    `json:"sha256"`
	CreatedAt   time.Time `json:"created_at,omitempty"`
}

type Options struct {
	// Root is the directory Ago owns. It is created with 0700 if absent and
	// its permissions are corrected if they are wider.
	Root string
	// MaxBytes bounds one artifact. Zero uses DefaultMaxBytes.
	MaxBytes int64
	Now      func() time.Time
}

type Store struct {
	root     string
	maxBytes int64
	now      func() time.Time
}

func Open(options Options) (*Store, error) {
	if strings.TrimSpace(options.Root) == "" {
		return nil, fmt.Errorf("artifact root is required")
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return nil, err
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = DefaultMaxBytes
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	for _, dir := range []string{root, filepath.Join(root, objectsDir), filepath.Join(root, tempDir)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("prepare artifact directory: %w", err)
		}
		// MkdirAll leaves an existing directory's mode alone, so a root that
		// was created loosely before is tightened here rather than trusted.
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("secure artifact directory: %w", err)
		}
	}
	// The root itself must be a real directory, not a symlink pointing
	// somewhere else.
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("artifact root must be a real directory")
	}
	return &Store{root: root, maxBytes: options.MaxBytes, now: options.Now}, nil
}

// Root reports the managed directory. It is exposed for diagnostics and for
// reconciliation, never for a caller to compose paths from.
func (store *Store) Root() string { return store.root }

// PutInput carries display metadata. It cannot influence the storage location.
type PutInput struct {
	Type        string
	DisplayName string
}

// Put copies a stream into the managed root and returns its descriptor.
//
// The bytes are hashed and counted while they are written, so the recorded size
// and digest describe what was actually stored. The temporary file is created
// exclusively, fsynced, and atomically renamed, so a crash can leave a
// discardable temp file but never a half-written artifact a descriptor points
// at. Exceeding the limit stops the copy immediately and removes the temp.
func (store *Store) Put(ctx context.Context, input PutInput, source io.Reader) (Descriptor, error) {
	if source == nil {
		return Descriptor{}, fmt.Errorf("artifact content is required")
	}
	id, err := newID()
	if err != nil {
		return Descriptor{}, err
	}
	return store.putWithID(ctx, id, input, source)
}

func (store *Store) putWithID(ctx context.Context, id string, input PutInput, source io.Reader) (Descriptor, error) {
	if err := ctx.Err(); err != nil {
		return Descriptor{}, err
	}
	finalPath, err := store.objectPath(id)
	if err != nil {
		return Descriptor{}, err
	}
	if _, err := os.Lstat(finalPath); err == nil {
		return Descriptor{}, fmt.Errorf("%q: %w", id, ErrDuplicate)
	}

	tempPath, file, err := store.createTemp()
	if err != nil {
		return Descriptor{}, err
	}
	// Any failure below must leave nothing behind but a discardable temp.
	committed := false
	defer func() {
		file.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()

	digest := sha256.New()
	// Read one byte past the limit so an oversized stream is detected from what
	// was actually read rather than from a size the caller claimed.
	limited := io.LimitReader(source, store.maxBytes+1)
	written, err := io.Copy(io.MultiWriter(file, digest), limited)
	if err != nil {
		return Descriptor{}, fmt.Errorf("copy artifact: %w", err)
	}
	if written > store.maxBytes {
		return Descriptor{}, fmt.Errorf("%d bytes: %w", written, ErrTooLarge)
	}
	if err := file.Sync(); err != nil {
		return Descriptor{}, fmt.Errorf("flush artifact: %w", err)
	}
	if err := file.Close(); err != nil {
		return Descriptor{}, fmt.Errorf("close artifact: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
		return Descriptor{}, err
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return Descriptor{}, fmt.Errorf("publish artifact: %w", err)
	}
	committed = true
	syncDir(filepath.Dir(finalPath))

	return Descriptor{
		ID: id, Type: safeType(input.Type), DisplayName: SafeDisplayName(input.DisplayName),
		Bytes: written, SHA256: hex.EncodeToString(digest.Sum(nil)), CreatedAt: store.now().UTC(),
	}, nil
}

// Open returns a reader for a stored artifact after re-verifying that the
// resolved path is still inside the managed root and is still a regular,
// unlinked file. The check is repeated at read time because the filesystem can
// change between writing and reading.
func (store *Store) Open(ctx context.Context, descriptor Descriptor) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := store.objectPath(descriptor.ID)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%q: %w", descriptor.ID, ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	// A symlink here would be an escape; a hard link would mean the bytes are
	// reachable and mutable from outside the managed root.
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%q is not a regular managed file: %w", descriptor.ID, ErrCorrupt)
	}
	if links := hardLinkCount(info); links > 1 {
		return nil, fmt.Errorf("%q has %d links: %w", descriptor.ID, links, ErrCorrupt)
	}
	if err := store.contained(path); err != nil {
		return nil, err
	}
	if descriptor.Bytes > 0 && info.Size() != descriptor.Bytes {
		return nil, fmt.Errorf("%q is %d bytes, metadata says %d: %w", descriptor.ID, info.Size(), descriptor.Bytes, ErrCorrupt)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return file, nil
}

// Verify re-reads an artifact and confirms it still matches its descriptor.
func (store *Store) Verify(ctx context.Context, descriptor Descriptor) error {
	reader, err := store.Open(ctx, descriptor)
	if err != nil {
		return err
	}
	defer reader.Close()
	digest := sha256.New()
	written, err := io.Copy(digest, reader)
	if err != nil {
		return err
	}
	if written != descriptor.Bytes || hex.EncodeToString(digest.Sum(nil)) != descriptor.SHA256 {
		return fmt.Errorf("%q: %w", descriptor.ID, ErrCorrupt)
	}
	return nil
}

// Reconcile removes temporary files and objects the durable store no longer
// references. It is safe to run at startup after a crash: an object is only
// removed when the caller states it is unreferenced.
func (store *Store) Reconcile(ctx context.Context, referenced map[string]bool) (int, error) {
	removed := 0
	temps, err := os.ReadDir(filepath.Join(store.root, tempDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return removed, err
	}
	for _, entry := range temps {
		if err := ctx.Err(); err != nil {
			return removed, err
		}
		// A temp file is never referenced by the database: a descriptor only
		// exists after the rename succeeded.
		if err := os.Remove(filepath.Join(store.root, tempDir, entry.Name())); err == nil {
			removed++
		}
	}
	shards, err := os.ReadDir(filepath.Join(store.root, objectsDir))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return removed, err
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		objects, err := os.ReadDir(filepath.Join(store.root, objectsDir, shard.Name()))
		if err != nil {
			return removed, err
		}
		for _, object := range objects {
			if err := ctx.Err(); err != nil {
				return removed, err
			}
			if referenced[object.Name()] {
				continue
			}
			if err := os.Remove(filepath.Join(store.root, objectsDir, shard.Name(), object.Name())); err == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// objectPath maps an identifier onto its managed location. The identifier is
// validated to be exactly the generated shape, so it cannot carry a separator,
// a parent reference, or an absolute path.
func (store *Store) objectPath(id string) (string, error) {
	if err := ValidID(id); err != nil {
		return "", err
	}
	return filepath.Join(store.root, objectsDir, id[:2], id), nil
}

// contained re-verifies that a path resolves inside the managed root even after
// symlinks are followed.
func (store *Store) contained(path string) error {
	resolvedRoot, err := filepath.EvalSymlinks(store.root)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil {
		return err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return fmt.Errorf("artifact resolved outside the managed root: %w", ErrCorrupt)
	}
	return nil
}

func (store *Store) createTemp() (string, *os.File, error) {
	for range 8 {
		name, err := newID()
		if err != nil {
			return "", nil, err
		}
		path := filepath.Join(store.root, tempDir, name)
		// O_EXCL means we never adopt a file someone else created, and
		// O_NOFOLLOW-equivalent safety comes from O_EXCL refusing an existing
		// symlink at this path.
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			return path, file, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return "", nil, fmt.Errorf("create artifact temp: %w", err)
		}
	}
	return "", nil, fmt.Errorf("could not create a unique artifact temp file")
}

// ValidID reports whether an identifier has exactly the generated shape.
func ValidID(id string) error {
	if len(id) != idBytes*2 {
		return fmt.Errorf("%q: %w", id, ErrBadID)
	}
	for index := 0; index < len(id); index++ {
		c := id[index]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("%q: %w", id, ErrBadID)
		}
	}
	return nil
}

func newID() (string, error) {
	buffer := make([]byte, idBytes)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("generate artifact id: %w", err)
	}
	return hex.EncodeToString(buffer), nil
}

// SafeDisplayName reduces a caller-supplied name to something safe to show and
// to put in a Content-Disposition header. It is never used as a path.
func SafeDisplayName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "artifact"
	}
	// Drop any directory structure a caller tried to smuggle in.
	name = filepath.Base(filepath.FromSlash(name))
	if name == "." || name == ".." || name == string(os.PathSeparator) {
		return "artifact"
	}
	var builder strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// Control characters would let a name forge header structure.
			continue
		case r == '"' || r == '\\' || r == '/' || r == ';' || r == '\n' || r == '\r':
			builder.WriteRune('_')
		default:
			builder.WriteRune(r)
		}
	}
	cleaned := strings.TrimSpace(builder.String())
	if cleaned == "" || cleaned == "." || cleaned == ".." {
		return "artifact"
	}
	if len(cleaned) > 128 {
		cleaned = cleaned[:128]
	}
	return cleaned
}

func safeType(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "application/octet-stream"
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f || r == '\n' || r == '\r' {
			return "application/octet-stream"
		}
	}
	if len(value) > 128 {
		return "application/octet-stream"
	}
	return value
}

func syncDir(path string) {
	dir, err := os.Open(path)
	if err != nil {
		return
	}
	defer dir.Close()
	_ = dir.Sync()
}
