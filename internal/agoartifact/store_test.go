package agoartifact_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"claudexflow/internal/agoartifact"
)

func newStore(t *testing.T, maxBytes int64) *agoartifact.Store {
	t.Helper()
	store, err := agoartifact.Open(agoartifact.Options{
		Root: filepath.Join(t.TempDir(), "artifacts"), MaxBytes: maxBytes,
		Now: func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func put(t *testing.T, store *agoartifact.Store, content string) agoartifact.Descriptor {
	t.Helper()
	descriptor, err := store.Put(context.Background(), agoartifact.PutInput{Type: "text/plain", DisplayName: "log.txt"}, strings.NewReader(content))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return descriptor
}

func TestPutRecordsTrueSizeAndDigest(t *testing.T) {
	store := newStore(t, agoartifact.DefaultMaxBytes)
	content := "执行日志\nexit code: 0\n"
	descriptor := put(t, store, content)

	digest := sha256.Sum256([]byte(content))
	if descriptor.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("digest = %q, want the digest of what was written", descriptor.SHA256)
	}
	if descriptor.Bytes != int64(len(content)) {
		t.Fatalf("bytes = %d, want %d", descriptor.Bytes, len(content))
	}
	if err := agoartifact.ValidID(descriptor.ID); err != nil {
		t.Fatalf("generated id is not well formed: %v", err)
	}
	if err := store.Verify(context.Background(), descriptor); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	reader, err := store.Open(context.Background(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != content {
		t.Fatalf("round trip = %q, %v", got, err)
	}
}

// The limit is enforced against bytes actually read, so a stream that lies
// about its size cannot get past it.
func TestOversizedStreamIsRejectedAndLeavesNothingBehind(t *testing.T) {
	store := newStore(t, 64)
	oversized := strings.NewReader(strings.Repeat("A", 4096))
	if _, err := store.Put(context.Background(), agoartifact.PutInput{}, oversized); !errors.Is(err, agoartifact.ErrTooLarge) {
		t.Fatalf("Put = %v, want ErrTooLarge", err)
	}
	assertNoTempsLeft(t, store)
	assertNoObjects(t, store)
}

// lyingReader reports a small size through any interface a caller might trust,
// then streams far more bytes.
type lyingReader struct {
	remaining int
}

func (r *lyingReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for i := range n {
		p[i] = 'B'
	}
	r.remaining -= n
	return n, nil
}

// Size is the kind of hint a caller might be tempted to trust. It is a lie.
func (r *lyingReader) Size() int64 { return 8 }
func (r *lyingReader) Stat() (os.FileInfo, error) {
	return nil, errors.New("no stat")
}

func TestUnderstatedSizeHintDoesNotBypassTheLimit(t *testing.T) {
	store := newStore(t, 128)
	if _, err := store.Put(context.Background(), agoartifact.PutInput{}, &lyingReader{remaining: 100_000}); !errors.Is(err, agoartifact.ErrTooLarge) {
		t.Fatalf("Put = %v, want ErrTooLarge despite the small size hint", err)
	}
	assertNoTempsLeft(t, store)
	assertNoObjects(t, store)
}

// Identifiers are the only thing a caller supplies on retrieval, so every
// path-shaped identifier must be rejected before it can reach the filesystem.
func TestMalformedIdentifiersAreRejected(t *testing.T) {
	store := newStore(t, agoartifact.DefaultMaxBytes)
	for _, id := range []string{
		"", "..", "../../etc/passwd", "/etc/passwd",
		strings.Repeat("a", 63), strings.Repeat("a", 65),
		strings.Repeat("A", 64), // uppercase is not the generated shape
		"../" + strings.Repeat("a", 61),
		strings.Repeat("a", 32) + "/" + strings.Repeat("b", 31),
		"g" + strings.Repeat("a", 63),
	} {
		if err := agoartifact.ValidID(id); err == nil {
			t.Fatalf("ValidID(%q) accepted a malformed identifier", id)
		}
		if _, err := store.Open(context.Background(), agoartifact.Descriptor{ID: id}); !errors.Is(err, agoartifact.ErrBadID) {
			t.Fatalf("Open(%q) = %v, want ErrBadID", id, err)
		}
	}
}

func TestUnknownIdentifierIsNotFoundAndLeaksNoPath(t *testing.T) {
	store := newStore(t, agoartifact.DefaultMaxBytes)
	missing := strings.Repeat("ab", 32)
	_, err := store.Open(context.Background(), agoartifact.Descriptor{ID: missing})
	if !errors.Is(err, agoartifact.ErrNotFound) {
		t.Fatalf("Open = %v, want ErrNotFound", err)
	}
	if strings.Contains(err.Error(), store.Root()) {
		t.Fatalf("the not-found error leaked a local path: %v", err)
	}
}

// A symlink planted where an object belongs must not be followed.
func TestSymlinkInPlaceOfAnObjectIsRefused(t *testing.T) {
	store := newStore(t, agoartifact.DefaultMaxBytes)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("机密内容"), 0o600); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("cd", 32)
	shard := filepath.Join(store.Root(), "objects", id[:2])
	if err := os.MkdirAll(shard, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(shard, id)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := store.Open(context.Background(), agoartifact.Descriptor{ID: id})
	if err == nil {
		t.Fatal("a symlinked object was opened, escaping the managed root")
	}
	if !errors.Is(err, agoartifact.ErrCorrupt) {
		t.Fatalf("Open = %v, want ErrCorrupt", err)
	}
}

// A hard link means the bytes are mutable from outside the managed root, so the
// artifact is no longer trustworthy evidence.
func TestHardLinkedObjectIsRefused(t *testing.T) {
	store := newStore(t, agoartifact.DefaultMaxBytes)
	descriptor := put(t, store, "证据内容")
	object := filepath.Join(store.Root(), "objects", descriptor.ID[:2], descriptor.ID)
	elsewhere := filepath.Join(t.TempDir(), "shadow")
	if err := os.Link(object, elsewhere); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if _, err := store.Open(context.Background(), descriptor); !errors.Is(err, agoartifact.ErrCorrupt) {
		t.Fatalf("Open = %v, want ErrCorrupt for a hard-linked object", err)
	}
}

func TestNonRegularObjectIsRefused(t *testing.T) {
	store := newStore(t, agoartifact.DefaultMaxBytes)
	id := strings.Repeat("ef", 32)
	shard := filepath.Join(store.Root(), "objects", id[:2])
	if err := os.MkdirAll(filepath.Join(shard, id), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(context.Background(), agoartifact.Descriptor{ID: id}); !errors.Is(err, agoartifact.ErrCorrupt) {
		t.Fatalf("Open on a directory = %v, want ErrCorrupt", err)
	}
}

// Truncation or tampering after the fact must be detected rather than served.
func TestTamperedBytesAreDetected(t *testing.T) {
	store := newStore(t, agoartifact.DefaultMaxBytes)
	descriptor := put(t, store, "原始证据内容")
	object := filepath.Join(store.Root(), "objects", descriptor.ID[:2], descriptor.ID)

	if err := os.WriteFile(object, []byte("被篡改"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Open(context.Background(), descriptor); !errors.Is(err, agoartifact.ErrCorrupt) {
		t.Fatalf("Open after truncation = %v, want ErrCorrupt", err)
	}

	// Same length, different bytes: only the digest can catch this.
	same := bytes.Repeat([]byte("x"), int(descriptor.Bytes))
	if err := os.WriteFile(object, same, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(context.Background(), descriptor); !errors.Is(err, agoartifact.ErrCorrupt) {
		t.Fatalf("Verify after tampering = %v, want ErrCorrupt", err)
	}
}

func TestDisplayNameIsSanitizedAndNeverBecomesAPath(t *testing.T) {
	store := newStore(t, agoartifact.DefaultMaxBytes)
	for _, name := range []string{
		"../../etc/passwd", "/etc/shadow", `evil".txt`, "a\nb", "a\r\nContent-Length: 0", "..", ".", "",
		strings.Repeat("n", 500),
	} {
		descriptor, err := store.Put(context.Background(), agoartifact.PutInput{DisplayName: name}, strings.NewReader("x"))
		if err != nil {
			t.Fatalf("Put(%q): %v", name, err)
		}
		got := descriptor.DisplayName
		if strings.ContainsAny(got, "/\\\n\r\"") || got == ".." || got == "." || got == "" {
			t.Fatalf("display name %q was not sanitized: %q", name, got)
		}
		if len(got) > 128 {
			t.Fatalf("display name %q was not bounded: %d chars", name, len(got))
		}
		// The stored object is addressed by identifier, never by the name.
		object := filepath.Join(store.Root(), "objects", descriptor.ID[:2], descriptor.ID)
		if _, err := os.Stat(object); err != nil {
			t.Fatalf("object is not at its identifier path: %v", err)
		}
	}
}

func TestMediaTypeWithHeaderInjectionIsNeutralized(t *testing.T) {
	store := newStore(t, agoartifact.DefaultMaxBytes)
	descriptor, err := store.Put(context.Background(), agoartifact.PutInput{Type: "text/plain\r\nX-Injected: 1"}, strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(descriptor.Type, "\r\n") {
		t.Fatalf("media type kept header structure: %q", descriptor.Type)
	}
}

// Concurrent writers must each get their own artifact; identifiers are
// generated, so a collision would be a generator bug rather than a race.
func TestConcurrentPutsProduceDistinctArtifacts(t *testing.T) {
	store := newStore(t, agoartifact.DefaultMaxBytes)
	const writers = 16
	ids := make(chan string, writers)
	errs := make(chan error, writers)
	var wait sync.WaitGroup
	for index := range writers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			descriptor, err := store.Put(context.Background(), agoartifact.PutInput{},
				strings.NewReader(strings.Repeat("c", index+1)))
			ids <- descriptor.ID
			errs <- err
		}()
	}
	wait.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Put: %v", err)
		}
	}
	seen := make(map[string]bool, writers)
	for id := range ids {
		if seen[id] {
			t.Fatalf("two concurrent puts produced the same identifier %q", id)
		}
		seen[id] = true
	}
	if len(seen) != writers {
		t.Fatalf("distinct artifacts = %d, want %d", len(seen), writers)
	}
}

// Reconcile clears crash debris without touching referenced artifacts.
func TestReconcileRemovesOrphansAndTempsButKeepsReferenced(t *testing.T) {
	store := newStore(t, agoartifact.DefaultMaxBytes)
	kept := put(t, store, "保留的证据")
	orphan := put(t, store, "无人引用的证据")

	// Simulate a crash mid-write.
	tempPath := filepath.Join(store.Root(), "tmp", strings.Repeat("aa", 32))
	if err := os.WriteFile(tempPath, []byte("half written"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := store.Reconcile(context.Background(), map[string]bool{kept.ID: true})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("reconcile removed %d entries, want the temp and the orphan", removed)
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reconcile left a temp file behind")
	}
	if _, err := store.Open(context.Background(), orphan); !errors.Is(err, agoartifact.ErrNotFound) {
		t.Fatalf("orphan survived reconcile: %v", err)
	}
	if err := store.Verify(context.Background(), kept); err != nil {
		t.Fatalf("reconcile damaged a referenced artifact: %v", err)
	}
}

// Reopening the managed root must preserve metadata and content exactly.
func TestArtifactsSurviveStoreRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	first, err := agoartifact.Open(agoartifact.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := first.Put(context.Background(), agoartifact.PutInput{Type: "application/json", DisplayName: "evidence.json"}, strings.NewReader(`{"通过":true}`))
	if err != nil {
		t.Fatal(err)
	}

	second, err := agoartifact.Open(agoartifact.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Verify(context.Background(), descriptor); err != nil {
		t.Fatalf("artifact did not survive restart: %v", err)
	}
	reader, err := second.Open(context.Background(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	content, err := io.ReadAll(reader)
	if err != nil || string(content) != `{"通过":true}` {
		t.Fatalf("content after restart = %q, %v", content, err)
	}
}

// The managed root must not be world- or group-readable.
func TestManagedRootIsPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "artifacts")
	// Pre-create it loosely; Open must tighten it rather than accept it.
	if err := os.MkdirAll(root, 0o777); err != nil {
		t.Fatal(err)
	}
	store, err := agoartifact.Open(agoartifact.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{store.Root(), filepath.Join(store.Root(), "objects"), filepath.Join(store.Root(), "tmp")} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Fatalf("%s has mode %o, want 0700", dir, perm)
		}
	}
	descriptor := put(t, store, "内容")
	object := filepath.Join(store.Root(), "objects", descriptor.ID[:2], descriptor.ID)
	info, err := os.Stat(object)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("artifact has mode %o, want no group or other access", perm)
	}
}

// A root that is a symlink is refused: it would let the whole store be
// relocated outside Ago's control.
func TestSymlinkedRootIsRefused(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := agoartifact.Open(agoartifact.Options{Root: link}); err == nil {
		t.Fatal("a symlinked artifact root was accepted")
	}
}

func assertNoTempsLeft(t *testing.T, store *agoartifact.Store) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(store.Root(), "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temp files left behind: %d", len(entries))
	}
}

func assertNoObjects(t *testing.T, store *agoartifact.Store) {
	t.Helper()
	shards, err := os.ReadDir(filepath.Join(store.Root(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, shard := range shards {
		objects, err := os.ReadDir(filepath.Join(store.Root(), "objects", shard.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(objects) != 0 {
			t.Fatalf("a rejected upload left %d objects behind", len(objects))
		}
	}
}
