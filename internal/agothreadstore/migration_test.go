package agothreadstore

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"claudexflow/internal/agoprotocol"
)

func TestOpenMigratesLegacyStoreWithoutLosingThread(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	_, err = db.Exec(`
PRAGMA foreign_keys = ON;
CREATE TABLE threads (
    thread_id TEXT PRIMARY KEY,
    last_sequence INTEGER NOT NULL CHECK (last_sequence >= 1)
);
CREATE TABLE events (
    thread_id TEXT NOT NULL REFERENCES threads(thread_id),
    sequence INTEGER NOT NULL CHECK (sequence >= 1),
    event_json BLOB NOT NULL,
    PRIMARY KEY (thread_id, sequence)
);
CREATE TABLE commands (
    actor_id TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    command_id TEXT NOT NULL,
    request_hash BLOB NOT NULL,
    thread_id TEXT NOT NULL REFERENCES threads(thread_id),
    result_json BLOB NOT NULL,
    PRIMARY KEY (actor_id, idempotency_key)
);
INSERT INTO threads (thread_id, last_sequence) VALUES ('T-legacy', 1);
INSERT INTO events (thread_id, sequence, event_json) VALUES (
    'T-legacy', 1,
    '{"schema_version":1,"event_id":"E-legacy","thread_id":"T-legacy","sequence":1,"type":"thread.created","visibility":"user"}'
);
`)
	if err != nil {
		t.Fatalf("create legacy database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy database: %v", err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("Open(legacy) error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	version, err := store.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion() error = %v", err)
	}
	if version != CurrentStoreSchemaVersion {
		t.Fatalf("schema version = %d, want %d", version, CurrentStoreSchemaVersion)
	}
	mailbox, err := store.Mailbox(context.Background(), "T-legacy")
	if err != nil {
		t.Fatalf("Mailbox(legacy) error = %v", err)
	}
	if mailbox.Activity != agoprotocol.ActivityIdle || mailbox.LastSequence != 1 {
		t.Fatalf("migrated mailbox = %#v, want idle at original sequence", mailbox)
	}
	started, err := store.Submit(context.Background(), mailboxCommand("T-legacy", agoprotocol.CommandMessageSubmit, "legacy-submit"), MessageInput{Content: []byte(`{"text":"continue"}`), Class: agoprotocol.QueueNormal})
	if err != nil {
		t.Fatalf("Submit() after migration error = %v", err)
	}
	if started.Activity != agoprotocol.ActivityRunning || started.LastSequence != 3 {
		t.Fatalf("Submit() after migration = %#v, want running at sequence 3", started)
	}
}

func TestOpenRejectsNewerUnknownSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open future database: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatalf("set future schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close future database: %v", err)
	}

	_, err = Open(path)
	if err == nil || !strings.Contains(err.Error(), "newer than supported") {
		t.Fatalf("Open(future) error = %v, want unsupported-newer-schema error", err)
	}
}

func TestOpenMigratesVersionSevenGitStoreToCurrentVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`DROP TABLE git_operations; PRAGMA user_version = 7`); err != nil {
		t.Fatalf("downgrade fixture to v7: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatalf("Open(v7) error = %v", err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != CurrentStoreSchemaVersion {
		t.Fatalf("migrated version = %d, %v; want %d", version, err, CurrentStoreSchemaVersion)
	}
	var table string
	if err := store.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='git_operations'`).Scan(&table); err != nil || table != "git_operations" {
		t.Fatalf("git_operations table = %q, %v", table, err)
	}
	columns, err := tableColumns(context.Background(), mustBeginTestTx(t, store), "git_operations")
	if err != nil || !columns["operation_id"] || !columns["before_json"] || !columns["latest_observed_json"] || columns["completed_sequence"] {
		t.Fatalf("migrated git operation columns = %#v, %v", columns, err)
	}
}

func TestOpenMigratesVersionEightJournalStoreToVersionNineWithoutLosingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
DROP TABLE git_snapshot_artifacts;
PRAGMA user_version = 8;
INSERT INTO threads (thread_id,last_sequence) VALUES ('T-v8',1);
INSERT INTO events (thread_id,sequence,event_json) VALUES (
 'T-v8',1,'{"schema_version":1,"event_id":"E-v8","thread_id":"T-v8","sequence":1,"type":"thread.created","visibility":"user"}'
);
INSERT INTO git_bindings VALUES ('T-v8','env',3,'/worktree','/git','/common','repo','worktree','sha1','base');
INSERT INTO git_snapshots VALUES (
 'T-v8',3,1,'env','repo','worktree','snapshot-v8',
 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
 'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',
 'cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc',
 '{}',1,'2026-07-19T00:00:00Z'
);
`); err != nil {
		t.Fatalf("create v8 fixture: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatalf("Open(v8) error = %v", err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != CurrentStoreSchemaVersion {
		t.Fatalf("migrated version = %d, %v; want %d", version, err, CurrentStoreSchemaVersion)
	}
	var snapshotCount, artifactTableCount int
	if err := store.db.QueryRow(`SELECT count(*) FROM git_snapshots WHERE thread_id='T-v8' AND executor_generation=3 AND revision=1`).Scan(&snapshotCount); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='git_snapshot_artifacts'`).Scan(&artifactTableCount); err != nil {
		t.Fatal(err)
	}
	if snapshotCount != 1 || artifactTableCount != 1 {
		t.Fatalf("migration preserved snapshots=%d, artifact tables=%d; want 1,1", snapshotCount, artifactTableCount)
	}
}

func TestOpenMigratesVersionNineStoreToCurrentWithoutLosingRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v9.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
DROP TABLE git_write_receipt_paths;
DROP TABLE git_write_receipts;
INSERT INTO threads (thread_id,last_sequence) VALUES ('T-v9',1);
INSERT INTO events (thread_id,sequence,event_json) VALUES (
 'T-v9',1,'{"schema_version":1,"event_id":"E-v9","thread_id":"T-v9","sequence":1,"type":"thread.created","visibility":"user"}'
);
INSERT INTO git_bindings VALUES ('T-v9','env',4,'/worktree','/git','/common','repo-v9','worktree-v9','sha256','base-v9');
PRAGMA user_version = 9;
`); err != nil {
		t.Fatalf("create v9 fixture: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatalf("Open(v9) error = %v", err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != CurrentStoreSchemaVersion {
		t.Fatalf("migrated version = %d, %v; want %d", version, err, CurrentStoreSchemaVersion)
	}
	var threadCount, eventCount, bindingCount, receiptTableCount int
	queries := []struct {
		query string
		out   *int
	}{
		{`SELECT count(*) FROM threads WHERE thread_id='T-v9' AND last_sequence=1`, &threadCount},
		{`SELECT count(*) FROM events WHERE thread_id='T-v9' AND sequence=1`, &eventCount},
		{`SELECT count(*) FROM git_bindings WHERE thread_id='T-v9' AND executor_generation=4 AND base_identity='base-v9'`, &bindingCount},
		{`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='git_write_receipts'`, &receiptTableCount},
	}
	for _, query := range queries {
		if err := store.db.QueryRow(query.query).Scan(query.out); err != nil {
			t.Fatal(err)
		}
	}
	if threadCount != 1 || eventCount != 1 || bindingCount != 1 || receiptTableCount != 1 {
		t.Fatalf("v9 migration counts thread=%d event=%d binding=%d receipt-table=%d; want all 1", threadCount, eventCount, bindingCount, receiptTableCount)
	}
}

func TestOpenConfiguresFullSynchronousDurability(t *testing.T) {
	store := openTestStore(t)
	var synchronous int
	if err := store.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if synchronous != 2 {
		t.Fatalf("PRAGMA synchronous = %d, want FULL (2)", synchronous)
	}
}

func mustBeginTestTx(t *testing.T, store *Store) *sql.Tx {
	t.Helper()
	tx, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return tx
}
