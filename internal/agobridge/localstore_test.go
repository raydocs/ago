package agobridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestFileStateStoreCommitReopen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bridge-state")
	store, err := NewFileStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	identity := BridgeIdentity{AccountID: "account", DeviceID: "device"}
	state := State{Cursor: 1, Evidence: []Evidence{{Sequence: 1, Nonce: "nonce", RequestDigest: testDigest("one"), Status: EvidencePrepared}}}
	revision, err := store.Commit(context.Background(), identity, 0, state)
	if err != nil || revision != 1 {
		t.Fatalf("Commit() = %d, %v", revision, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewFileStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	loaded, err := reopened.Load(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != 1 || loaded.Cursor != 1 || len(loaded.Evidence) != 1 || loaded.Evidence[0].Nonce != "nonce" {
		t.Fatalf("Load() = %#v", loaded)
	}
}

func TestFileStateStoreConcurrentCASHasOneWinner(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bridge-state")
	first, err := NewFileStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := NewFileStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	identity := BridgeIdentity{AccountID: "account", DeviceID: "device"}

	start := make(chan struct{})
	var winners atomic.Int32
	var conflicts atomic.Int32
	var group sync.WaitGroup
	for _, store := range []*FileStateStore{first, second} {
		group.Add(1)
		go func(store *FileStateStore) {
			defer group.Done()
			<-start
			_, commitErr := store.Commit(context.Background(), identity, 0, State{})
			switch {
			case commitErr == nil:
				winners.Add(1)
			case errors.Is(commitErr, ErrStateConflict):
				conflicts.Add(1)
			default:
				t.Errorf("Commit() error = %v", commitErr)
			}
		}(store)
	}
	close(start)
	group.Wait()
	if winners.Load() != 1 || conflicts.Load() != 1 {
		t.Fatalf("winners = %d, conflicts = %d", winners.Load(), conflicts.Load())
	}
}

func TestFileStateStoreRejectsSymlinkFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bridge-state")
	store, err := NewFileStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity := BridgeIdentity{AccountID: "account", DeviceID: "device"}
	key := stateKey(identity)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, key+".state")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), identity); err == nil {
		t.Fatal("Load accepted symlink state file")
	}
}

func TestFileStateStoreRejectsPermissiveRootAndState(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "bridge-state")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := NewFileStateStore(root); err == nil {
			t.Fatal("constructor accepted permissive root")
		}
	})
	t.Run("state", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "bridge-state")
		store, err := NewFileStateStore(root)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		identity := BridgeIdentity{AccountID: "account", DeviceID: "device"}
		if _, err := store.Commit(context.Background(), identity, 0, State{}); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(root, stateKey(identity)+".state"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(context.Background(), identity); err == nil {
			t.Fatal("Load accepted permissive state file")
		}
	})
}

func TestFileStateStoreRejectsCorruption(t *testing.T) {
	root := filepath.Join(t.TempDir(), "bridge-state")
	store, err := NewFileStateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	identity := BridgeIdentity{AccountID: "account", DeviceID: "device"}
	if _, err := store.Commit(context.Background(), identity, 0, State{}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, stateKey(identity)+".state")
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(context.Background(), identity); err == nil {
		t.Fatal("Load accepted corrupt state")
	}
}

func testDigest(value string) string {
	digest := sha256Sum([]byte(value))
	return digest
}
