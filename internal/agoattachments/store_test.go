package agoattachments

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"claudexflow/internal/agoprotocol"
)

func TestUploadGetAndOpen(t *testing.T) {
	store := openTestStore(t)
	owner := Owner{ProjectID: "project-1", ThreadID: "T-thread-1"}
	content := []byte("immutable attachment")
	ref := attachmentRef("att-1", "note.txt", "text/plain", content)

	if err := store.Upload(context.Background(), owner, ref, bytes.NewReader(content)); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}
	got, err := store.Get(context.Background(), owner, ref)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("Get() = %q, want %q", got, content)
	}

	opened, err := store.Open(context.Background(), owner, ref)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer opened.Close()
	openedBytes, err := io.ReadAll(opened)
	if err != nil {
		t.Fatalf("read Open(): %v", err)
	}
	if !bytes.Equal(openedBytes, content) {
		t.Fatalf("Open() = %q, want %q", openedBytes, content)
	}
}

func TestUploadExactRetrySurvivesReopen(t *testing.T) {
	root := t.TempDir()
	owner := Owner{ProjectID: "project-1", ThreadID: "T-thread-1"}
	content := []byte("same request")
	ref := attachmentRef("att-retry", "retry.txt", "text/plain", content)
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upload(context.Background(), owner, ref, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Upload(context.Background(), owner, ref, bytes.NewReader(content)); err != nil {
		t.Fatalf("exact retry failed: %v", err)
	}
}

func TestStorageIsContentAddressedAndPrivate(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner := Owner{ProjectID: "project-1", ThreadID: "T-thread-1"}
	content := []byte("shared content")
	for _, id := range []string{"att-shared-1", "att-shared-2"} {
		ref := attachmentRef(id, id+".txt", "text/plain", content)
		if err := store.Upload(context.Background(), owner, ref, bytes.NewReader(content)); err != nil {
			t.Fatal(err)
		}
	}
	digest := sha256.Sum256(content)
	digestName := hex.EncodeToString(digest[:])
	blobPath := filepath.Join(root, StorageDirectory, "blobs", digestName[:2], digestName)
	if _, err := os.Stat(blobPath); err != nil {
		t.Fatalf("content-addressed blob missing: %v", err)
	}
	if err := filepath.Walk(filepath.Join(root, StorageDirectory), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("storage path %q permissions = %04o, want private", path, info.Mode().Perm())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestUploadChangedRetryConflicts(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	owner := Owner{ProjectID: "project-1", ThreadID: "T-thread-1"}
	original := []byte("original")
	ref := attachmentRef("att-conflict", "original.txt", "text/plain", original)
	if err := store.Upload(context.Background(), owner, ref, bytes.NewReader(original)); err != nil {
		t.Fatal(err)
	}

	changed := []byte("changed")
	changedRef := attachmentRef(ref.AttachmentID, "changed.txt", "text/plain", changed)
	if err := store.Upload(context.Background(), owner, changedRef, bytes.NewReader(changed)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry error = %v, want ErrConflict", err)
	}
	changedBlob := filepath.Join(root, StorageDirectory, "blobs", changedRef.SHA256[:2], changedRef.SHA256)
	if _, err := os.Stat(changedBlob); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("changed retry persisted an unreferenced blob: %v", err)
	}
	otherOwner := Owner{ProjectID: owner.ProjectID, ThreadID: "T-other"}
	if err := store.Upload(context.Background(), otherOwner, ref, bytes.NewReader(original)); !errors.Is(err, ErrConflict) {
		t.Fatalf("owner-changing retry error = %v, want ErrConflict", err)
	}
}

func TestCorruptMetadataFailsGetAndRetry(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner := Owner{ProjectID: "project-1", ThreadID: "T-thread-1"}
	content := []byte("metadata")
	ref := attachmentRef("att-corrupt-metadata", "metadata.txt", "text/plain", content)
	if err := store.Upload(context.Background(), owner, ref, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	metadataPath := storedMetadataPath(root, ref.AttachmentID)
	if err := os.WriteFile(metadataPath, []byte(`{"owner":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), owner, ref); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Get() error = %v, want ErrCorrupt", err)
	}
	if err := store.Upload(context.Background(), owner, ref, bytes.NewReader(content)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("exact retry error = %v, want ErrCorrupt", err)
	}
}

func TestGetRequiresExactOwnerAndReference(t *testing.T) {
	store := openTestStore(t)
	owner := Owner{ProjectID: "project-1", ThreadID: "T-thread-1"}
	content := []byte("private")
	ref := attachmentRef("att-private", "private.txt", "text/plain", content)
	if err := store.Upload(context.Background(), owner, ref, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	for _, unauthorized := range []Owner{
		{ProjectID: "project-2", ThreadID: owner.ThreadID},
		{ProjectID: owner.ProjectID, ThreadID: "T-thread-2"},
	} {
		if _, err := store.Get(context.Background(), unauthorized, ref); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("Get(%#v) error = %v, want ErrUnauthorized", unauthorized, err)
		}
	}
	changedRef := ref
	changedRef.Filename = "other.txt"
	if _, err := store.Get(context.Background(), owner, changedRef); !errors.Is(err, ErrConflict) {
		t.Fatalf("Get(changed ref) error = %v, want ErrConflict", err)
	}
}

func TestUploadValidatesBytesAndBoundsReader(t *testing.T) {
	store := openTestStore(t)
	owner := Owner{ProjectID: "project-1", ThreadID: "T-thread-1"}
	content := []byte("expected")
	ref := attachmentRef("att-invalid", "invalid.bin", "application/octet-stream", content)
	if err := store.Upload(context.Background(), owner, ref, bytes.NewReader([]byte("tampered"))); err == nil {
		t.Fatal("Upload() accepted bytes with a different digest")
	}

	reader := &countingByteReader{remaining: int(agoprotocol.MaxAttachmentBytes) + 2}
	if err := store.Upload(context.Background(), owner, ref, reader); err == nil {
		t.Fatal("Upload() accepted an oversized body")
	}
	if reader.read != int(agoprotocol.MaxAttachmentBytes)+1 {
		t.Fatalf("Upload() read %d bytes, want bounded read of %d", reader.read, agoprotocol.MaxAttachmentBytes+1)
	}
}

func TestOpenRejectsSymlinkRootsAndStorageDirectories(t *testing.T) {
	realRoot := t.TempDir()
	linkRoot := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(linkRoot); err == nil {
		_ = store.Close()
		t.Fatal("Open() followed a symlink root")
	}

	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, StorageDirectory)); err != nil {
		t.Fatal(err)
	}
	if store, err := Open(root); err == nil {
		_ = store.Close()
		t.Fatal("Open() followed a symlink storage directory")
	}
}

func TestGetRejectsSymlinkAndCorruptBlob(t *testing.T) {
	for _, test := range []struct {
		name   string
		tamper func(string) error
	}{
		{name: "symlink", tamper: func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			return os.Symlink(filepath.Join(t.TempDir(), "outside"), path)
		}},
		{name: "digest", tamper: func(path string) error {
			return os.WriteFile(path, []byte("Tamper me"), 0o600)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			owner := Owner{ProjectID: "project-1", ThreadID: "T-thread-1"}
			content := []byte("tamper me")
			ref := attachmentRef("att-tamper", "tamper.txt", "text/plain", content)
			if err := store.Upload(context.Background(), owner, ref, bytes.NewReader(content)); err != nil {
				t.Fatal(err)
			}
			blobPath := filepath.Join(root, StorageDirectory, "blobs", ref.SHA256[:2], ref.SHA256)
			if err := test.tamper(blobPath); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Get(context.Background(), owner, ref); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Get() error = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestConcurrentExactUploadIsIdempotent(t *testing.T) {
	store := openTestStore(t)
	owner := Owner{ProjectID: "project-1", ThreadID: "T-thread-1"}
	content := bytes.Repeat([]byte("race"), 1024)
	ref := attachmentRef("att-race-exact", "race.bin", "application/octet-stream", content)

	start := make(chan struct{})
	errs := make(chan error, 32)
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			errs <- store.Upload(context.Background(), owner, ref, bytes.NewReader(content))
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent exact Upload() error = %v", err)
		}
	}
}

func TestConcurrentChangedUploadHasOneImmutableWinner(t *testing.T) {
	root := t.TempDir()
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	secondStore, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStore.Close()
	stores := []*Store{store, secondStore}
	owner := Owner{ProjectID: "project-1", ThreadID: "T-thread-1"}
	contents := [][]byte{[]byte("winner one"), []byte("winner two")}
	refs := []agoprotocol.AttachmentRef{
		attachmentRef("att-race-conflict", "one.txt", "text/plain", contents[0]),
		attachmentRef("att-race-conflict", "two.txt", "text/plain", contents[1]),
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for index := range refs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			errs <- stores[index].Upload(context.Background(), owner, refs[index], bytes.NewReader(contents[index]))
		}(index)
	}
	close(start)
	wait.Wait()
	close(errs)
	var successes, conflicts int
	for err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrConflict):
			conflicts++
		default:
			t.Fatalf("concurrent changed Upload() error = %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes = %d, conflicts = %d; want 1 each", successes, conflicts)
	}
}

func TestIdentifiersCannotTraverseStorage(t *testing.T) {
	store := openTestStore(t)
	content := []byte("x")
	ref := attachmentRef("att-safe", "x.txt", "text/plain", content)
	invalidOwners := []Owner{
		{ProjectID: "../project", ThreadID: "T-thread"},
		{ProjectID: "project", ThreadID: "../thread"},
		{ProjectID: "/project", ThreadID: "T-thread"},
	}
	for _, owner := range invalidOwners {
		if err := store.Upload(context.Background(), owner, ref, bytes.NewReader(content)); err == nil {
			t.Fatalf("Upload(%#v) accepted traversal-like owner", owner)
		}
	}
	ref.AttachmentID = "../attachment"
	if err := store.Upload(context.Background(), Owner{ProjectID: "project", ThreadID: "T-thread"}, ref, bytes.NewReader(content)); err == nil {
		t.Fatal("Upload() accepted traversal-like attachment ID")
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func attachmentRef(id, filename, mediaType string, content []byte) agoprotocol.AttachmentRef {
	digest := sha256.Sum256(content)
	return agoprotocol.AttachmentRef{
		AttachmentID: id,
		SHA256:       hex.EncodeToString(digest[:]),
		SizeBytes:    uint64(len(content)),
		MediaType:    mediaType,
		Filename:     filename,
	}
}

func storedMetadataPath(root, attachmentID string) string {
	identity := sha256.Sum256([]byte(attachmentID))
	name := hex.EncodeToString(identity[:])
	return filepath.Join(root, StorageDirectory, "refs", name[:2], name)
}

type countingByteReader struct {
	remaining int
	read      int
}

func (reader *countingByteReader) Read(target []byte) (int, error) {
	if reader.remaining == 0 {
		return 0, io.EOF
	}
	count := min(len(target), reader.remaining)
	for index := range count {
		target[index] = 'x'
	}
	reader.remaining -= count
	reader.read += count
	return count, nil
}
