package agoboardstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"claudexflow/internal/agoboardprotocol"
)

// legacyBoardJSON is a schema-1 board aggregate exactly as the previous release
// persisted it: no repository, generation, access mode, attempt accounting,
// fencing token, or lease deadline. One lease is still active, which is the
// case the backfill has to be conservative about.
const legacyBoardJSON = `{
  "schema_version": 1,
  "id": "legacy-board",
  "version": 6,
  "title": "旧版看板",
  "tasks": [
    {"id":"done-task","title":"已完成任务","state":"passed",
     "terminal_contract":{"outcome":"完成","acceptance_criteria":["通过"]},
     "accepted_evidence_id":"evidence-done"},
    {"id":"running-task","title":"运行中任务","state":"running",
     "terminal_contract":{"outcome":"运行","acceptance_criteria":["通过"]},
     "active_attempt_id":"attempt-running","active_lease_id":"lease-running"}
  ],
  "dependencies": [{"id":"dep-1","task_id":"running-task","depends_on":"done-task"}],
  "attempts": [
    {"id":"attempt-done","task_id":"done-task","worker_id":"worker-1","state":"passed","evidence_id":"evidence-done"},
    {"id":"attempt-running","task_id":"running-task","worker_id":"worker-2","state":"running"}
  ],
  "leases": [
    {"id":"lease-done","task_id":"done-task","attempt_id":"attempt-done","worker_id":"worker-1","state":"completed"},
    {"id":"lease-running","task_id":"running-task","attempt_id":"attempt-running","worker_id":"worker-2","state":"active"}
  ],
  "evidence": [
    {"id":"evidence-done","task_id":"done-task","attempt_id":"attempt-done","worker_id":"worker-1",
     "artifact":"artifact://legacy","summary":"旧版证据","state":"accepted"}
  ]
}`

const legacyLeaseDeadline = int64(1_800_000_000_000_000_000)

// writeLegacyDatabase builds a genuine schema-2 database without going through
// the current store, so the migration is exercised against real old data rather
// than against something the new code produced.
func writeLegacyDatabase(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(initialSchema); err != nil {
		t.Fatalf("apply schema-2 baseline: %v", err)
	}
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO boards(board_id,version,title,board_json) VALUES(?,?,?,?)`,
			[]any{"legacy-board", 6, "旧版看板", []byte(legacyBoardJSON)}},
		{`INSERT INTO board_definitions(board_id,definition_json) VALUES(?,?)`,
			[]any{"legacy-board", []byte(`{"goal":{"objective":{"summary":"旧版目标"}}}`)}},
		{`INSERT INTO tasks VALUES(?,?,?,?)`, []any{"legacy-board", "done-task", "passed", []byte(`{"id":"done-task"}`)}},
		{`INSERT INTO tasks VALUES(?,?,?,?)`, []any{"legacy-board", "running-task", "running", []byte(`{"id":"running-task"}`)}},
		{`INSERT INTO dependencies VALUES(?,?,?,?,?)`, []any{"legacy-board", "dep-1", "running-task", "done-task", []byte(`{"id":"dep-1"}`)}},
		{`INSERT INTO attempts VALUES(?,?,?,?,?,?)`, []any{"legacy-board", "attempt-done", "done-task", "worker-1", "passed", []byte(`{"id":"attempt-done"}`)}},
		{`INSERT INTO attempts VALUES(?,?,?,?,?,?)`, []any{"legacy-board", "attempt-running", "running-task", "worker-2", "running", []byte(`{"id":"attempt-running"}`)}},
		{`INSERT INTO leases VALUES(?,?,?,?,?,?,?,?)`, []any{"legacy-board", "lease-done", "done-task", "attempt-done", "worker-1", "completed", 0, []byte(`{"id":"lease-done"}`)}},
		{`INSERT INTO leases VALUES(?,?,?,?,?,?,?,?)`, []any{"legacy-board", "lease-running", "running-task", "attempt-running", "worker-2", "active", legacyLeaseDeadline, []byte(`{"id":"lease-running"}`)}},
		{`INSERT INTO evidence VALUES(?,?,?,?,?,?)`, []any{"legacy-board", "evidence-done", "done-task", "attempt-done", "accepted", []byte(`{"id":"evidence-done"}`)}},
		{`INSERT INTO commands VALUES(?,?,?,?,?)`, []any{"coordinator", "legacy-command", []byte("hash"), "legacy-board", []byte(`{"board":{"id":"legacy-board"}}`)}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.query, err)
		}
	}
	for version := 1; version <= 6; version++ {
		if _, err := db.Exec(`INSERT INTO events(board_id,version,event_id,event_json) VALUES(?,?,?,?)`,
			"legacy-board", version, fmt.Sprintf("legacy-event-%d", version),
			[]byte(fmt.Sprintf(`{"schema_version":1,"id":"legacy-event-%d","board_id":"legacy-board","version":%d}`, version, version))); err != nil {
			t.Fatalf("seed event %d: %v", version, err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=2`); err != nil {
		t.Fatal(err)
	}
}

func columnNames(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var index int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&index, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestFreshDatabaseIsCreatedAtCurrentSchema(t *testing.T) {
	store := openStore(t, filepath.Join(t.TempDir(), "fresh.db"))
	defer store.Close()
	version, err := store.SchemaVersion(context.Background())
	if err != nil || version != CurrentSchemaVersion {
		t.Fatalf("fresh schema version = %d, %v; want %d", version, err, CurrentSchemaVersion)
	}
	for table, required := range map[string][]string{
		"leases":   {"generation", "fencing_token", "acquired_at", "access_mode", "repository_id"},
		"attempts": {"attempt_number", "generation", "fencing_token", "failure_class"},
		"tasks":    {"access_mode", "attempt_count", "next_eligible_at", "failure_class"},
		"boards":   {"repository_id", "paused", "next_generation"},
	} {
		present := columnNames(t, store.db, table)
		for _, column := range required {
			found := false
			for _, name := range present {
				if name == column {
					found = true
				}
			}
			if !found {
				t.Fatalf("fresh %s table is missing %q; has %v", table, column, present)
			}
		}
	}
}

// A migrated database must be structurally identical to a fresh one, otherwise
// the two paths would drift and only one of them would be tested.
func TestMigratedDatabaseSchemaMatchesFreshDatabase(t *testing.T) {
	legacyPath := filepath.Join(t.TempDir(), "legacy.db")
	writeLegacyDatabase(t, legacyPath)
	migrated := openStore(t, legacyPath)
	defer migrated.Close()
	fresh := openStore(t, filepath.Join(t.TempDir(), "fresh.db"))
	defer fresh.Close()

	for _, table := range []string{"boards", "tasks", "dependencies", "attempts", "leases", "evidence", "commands", "bindings", "board_definitions", "events"} {
		migratedColumns := columnNames(t, migrated.db, table)
		freshColumns := columnNames(t, fresh.db, table)
		if !reflect.DeepEqual(migratedColumns, freshColumns) {
			t.Fatalf("table %s: migrated columns %v, fresh columns %v", table, migratedColumns, freshColumns)
		}
	}
}

func TestMigrationPreservesBoardsEventsCommandsAndDefinitions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	writeLegacyDatabase(t, path)
	store := openStore(t, path)
	defer store.Close()

	version, err := store.SchemaVersion(ctx)
	if err != nil || version != CurrentSchemaVersion {
		t.Fatalf("schema version after migration = %d, %v", version, err)
	}
	board, err := store.Board(ctx, "legacy-board")
	if err != nil {
		t.Fatalf("migrated board did not load: %v", err)
	}
	if board.SchemaVersion != agoboardprotocol.SchemaVersion {
		t.Fatalf("board schema version = %d, want %d", board.SchemaVersion, agoboardprotocol.SchemaVersion)
	}
	if board.Title != "旧版看板" || board.Version != 6 {
		t.Fatalf("migration altered board identity: %#v", board)
	}
	if len(board.Tasks) != 2 || len(board.Attempts) != 2 || len(board.Leases) != 2 || len(board.Evidence) != 1 || len(board.Dependencies) != 1 {
		t.Fatalf("migration lost graph content: %#v", board)
	}

	events, err := store.Replay(ctx, "legacy-board", 0, 0)
	if err != nil || len(events) != 6 {
		t.Fatalf("events after migration = %d, %v; want 6", len(events), err)
	}
	var commandCount int
	if err := store.db.QueryRow(`SELECT count(*) FROM commands`).Scan(&commandCount); err != nil || commandCount != 1 {
		t.Fatalf("commands after migration = %d, %v; want 1", commandCount, err)
	}
	var definition map[string]any
	if err := store.Definition(ctx, "legacy-board", &definition); err != nil {
		t.Fatalf("definition after migration: %v", err)
	}
	if len(definition) == 0 {
		t.Fatal("migration lost the planner definition")
	}
}

// The backfill must not hand a pre-upgrade executor any authority it never had.
func TestMigrationGrantsNoFencingAuthorityToHistoricalLeases(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	writeLegacyDatabase(t, path)
	store := openStore(t, path)
	defer store.Close()

	board, err := store.Board(ctx, "legacy-board")
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range board.Attempts {
		if attempt.FencingToken != "" || attempt.Generation != 0 {
			t.Fatalf("migrated attempt %q was granted fencing authority: %#v", attempt.ID, attempt)
		}
	}
	for _, lease := range board.Leases {
		if lease.FencingToken != "" || lease.Generation != 0 {
			t.Fatalf("migrated lease %q was granted fencing authority: %#v", lease.ID, lease)
		}
	}
	// The still-active lease keeps its state and, critically, its deadline, so
	// the reconciler can find and supersede it.
	var activeState string
	var activeExpiry int64
	if err := store.db.QueryRow(`SELECT state,expires_at FROM leases WHERE lease_id='lease-running'`).Scan(&activeState, &activeExpiry); err != nil {
		t.Fatal(err)
	}
	if activeState != string(agoboardprotocol.LeaseActive) || activeExpiry != legacyLeaseDeadline {
		t.Fatalf("active lease after migration = %s/%d, want active/%d", activeState, activeExpiry, legacyLeaseDeadline)
	}
	for _, lease := range board.Leases {
		if lease.ID == "lease-running" && lease.ExpiresAt.UTC() != time.Unix(0, legacyLeaseDeadline).UTC() {
			t.Fatalf("lease deadline was not carried into the aggregate: %#v", lease)
		}
	}

	// Without a token, no worker command can authenticate against the attempt
	// that was in flight during the upgrade.
	_, _, err = agoboardprotocol.Apply(board, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "stale-worker", ExpectedVersion: board.Version,
		Actor: agoboardprotocol.Actor{ID: "worker-2", Role: agoboardprotocol.RoleWorker},
		Type:  agoboardprotocol.CommandEvidenceSubmit, TaskID: "running-task", AttemptID: "attempt-running",
		Evidence: &agoboardprotocol.EvidenceSpec{ID: "late", TaskID: "running-task", AttemptID: "attempt-running", Artifact: "a", Summary: "s"},
	})
	if err == nil {
		t.Fatal("an executor from before the upgrade was able to submit evidence")
	}
}

// Conservative backfill still has to produce a usable graph: legacy tasks get a
// write access mode and an attempt count derived from real history.
func TestMigrationBackfillsAccessModeAndAttemptAccounting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	writeLegacyDatabase(t, path)
	store := openStore(t, path)
	defer store.Close()

	board, err := store.Board(context.Background(), "legacy-board")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range board.Tasks {
		if task.AccessMode != agoboardprotocol.AccessWrite {
			t.Fatalf("legacy task %q access mode = %q, want the cautious %q", task.ID, task.AccessMode, agoboardprotocol.AccessWrite)
		}
		if task.AttemptCount != 1 {
			t.Fatalf("legacy task %q attempt count = %d, want 1 from its history", task.ID, task.AttemptCount)
		}
	}
	if board.NextGeneration != 1 {
		t.Fatalf("migrated board next generation = %d, want 1", board.NextGeneration)
	}
	// Projections must agree with the aggregate the migration wrote.
	var mode string
	var attemptCount int
	if err := store.db.QueryRow(`SELECT access_mode,attempt_count FROM tasks WHERE task_id='running-task'`).Scan(&mode, &attemptCount); err != nil {
		t.Fatal(err)
	}
	if mode != string(agoboardprotocol.AccessWrite) || attemptCount != 1 {
		t.Fatalf("task projection = %s/%d, disagrees with the aggregate", mode, attemptCount)
	}
}

// A migration that fails partway must leave the database exactly as it was,
// including its recorded version, so the next open retries cleanly.
func TestFailedMigrationDoesNotAdvanceSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.db")
	writeLegacyDatabase(t, path)
	corrupt, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corrupt.Exec(`UPDATE boards SET board_json=? WHERE board_id='legacy-board'`, []byte(`{ this is not json`)); err != nil {
		t.Fatal(err)
	}
	if err := corrupt.Close(); err != nil {
		t.Fatal(err)
	}

	if store, err := Open(path); err == nil {
		store.Close()
		t.Fatal("Open accepted a database whose backfill could not succeed")
	}

	check, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	var version int
	if err := check.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("user_version after a failed migration = %d, want it left at 2", version)
	}
	// The rolled-back transaction must not have left the new columns behind.
	var columnCount int
	if err := check.QueryRow(`SELECT count(*) FROM pragma_table_info('leases') WHERE name='fencing_token'`).Scan(&columnCount); err != nil {
		t.Fatal(err)
	}
	if columnCount != 0 {
		t.Fatal("a failed migration left its schema changes applied")
	}
}

// Opening an already-migrated database repeatedly must be a no-op.
func TestReopeningAMigratedDatabaseIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	writeLegacyDatabase(t, path)

	var snapshot []byte
	for round := range 3 {
		store, err := Open(path)
		if err != nil {
			t.Fatalf("open round %d: %v", round, err)
		}
		board, err := store.Board(ctx, "legacy-board")
		if err != nil {
			t.Fatalf("round %d board: %v", round, err)
		}
		encoded, err := json.Marshal(board)
		if err != nil {
			t.Fatal(err)
		}
		if round == 0 {
			snapshot = encoded
		} else if string(encoded) != string(snapshot) {
			t.Fatalf("round %d changed the migrated board:\n%s\nwant\n%s", round, encoded, snapshot)
		}
		events, err := store.Replay(ctx, "legacy-board", 0, 0)
		if err != nil || len(events) != 6 {
			t.Fatalf("round %d events = %d, %v; want 6", round, len(events), err)
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

// The schema-4 upgrade must reach a database that is already at schema 3,
// exercising the second migration step on its own rather than only as part of a
// full 2-to-4 run.
func TestSchemaThreeDatabaseUpgradesToCurrent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v3.db")
	writeLegacyDatabase(t, path)

	// First open migrates 2 -> 4. Rewind the marker to 3 and drop the schema-4
	// additions so the next open exercises exactly the 3 -> 4 step.
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP TABLE artifacts`,
		`ALTER TABLE evidence DROP COLUMN required_tests_passed`,
		`ALTER TABLE evidence DROP COLUMN verdict`,
		`PRAGMA user_version=3`,
	} {
		if _, err := raw.Exec(statement); err != nil {
			t.Fatalf("rewind %q: %v", statement, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade from schema 3: %v", err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(ctx)
	if err != nil || version != CurrentSchemaVersion {
		t.Fatalf("version = %d, %v; want %d", version, err, CurrentSchemaVersion)
	}
	board, err := store.Board(ctx, "legacy-board")
	if err != nil {
		t.Fatalf("board after 3 -> 4: %v", err)
	}
	if board.SchemaVersion != agoboardprotocol.SchemaVersion {
		t.Fatalf("board schema version = %d, want %d", board.SchemaVersion, agoboardprotocol.SchemaVersion)
	}
	// The historical acceptance keeps its verdict but claims no deterministic
	// backing it never had.
	for _, evidence := range board.Evidence {
		if evidence.State == agoboardprotocol.EvidenceAccepted && evidence.Verdict != "accept" {
			t.Fatalf("migrated evidence %q verdict = %q", evidence.ID, evidence.Verdict)
		}
		if len(evidence.Result.Tests) != 0 {
			t.Fatalf("migration invented test records: %#v", evidence.Result.Tests)
		}
	}
	events, err := store.Replay(ctx, "legacy-board", 0, 0)
	if err != nil || len(events) != 6 {
		t.Fatalf("events after 3 -> 4 = %d, %v; want 6", len(events), err)
	}
}

// Artifact references are projected so the artifact store can be reconciled.
func TestArtifactReferencesAreProjectedForReconciliation(t *testing.T) {
	ctx := context.Background()
	store := openStore(t, filepath.Join(t.TempDir(), "artifacts.db"))
	defer store.Close()
	board := buildClaimBoard(t, store, "art", "repo", map[string]agoboardprotocol.AccessMode{
		"only": agoboardprotocol.AccessRead,
	})
	claim := startAttempt(t, store, board, "claim:1", testClock)
	running, err := store.Board(ctx, board.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyBoard(ctx, board.ID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion, ID: "submit",
		ExpectedVersion: running.Version, Actor: workerActor(),
		Type: agoboardprotocol.CommandEvidenceSubmit, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
		FencingToken: claim.FencingToken,
		Evidence: &agoboardprotocol.EvidenceSpec{
			ID: "evidence", TaskID: claim.TaskID, AttemptID: claim.AttemptID,
			Artifact: "artifact://managed", Summary: "证据",
			Result: agoboardprotocol.EvidenceResult{
				Summary: "证据",
				Artifacts: []agoboardprotocol.ArtifactRef{
					{ID: "aaaa1111", Type: "text/plain", DisplayName: "log.txt", Bytes: 12, SHA256: "deadbeef"},
					{ID: "bbbb2222", Type: "application/json", DisplayName: "report.json", Bytes: 34, SHA256: "cafebabe"},
				},
			},
		},
	}); err != nil {
		t.Fatalf("submit structured evidence: %v", err)
	}
	referenced, err := store.ReferencedArtifacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !referenced["aaaa1111"] || !referenced["bbbb2222"] || len(referenced) != 2 {
		t.Fatalf("referenced artifacts = %#v", referenced)
	}
}
