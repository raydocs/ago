package agothreadstore

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"claudexflow/internal/agoprotocol"
)

func TestThreadCatalogSearchIsProjectScopedAndCursorDeterministic(t *testing.T) {
	store := openTestStore(t)
	for index, fixture := range []struct {
		project string
		title   string
	}{
		{"project-a", "Alpha migration"},
		{"project-a", "Alpha release"},
		{"project-a", "Unrelated"},
		{"project-b", "Alpha private"},
	} {
		input := atomicCreateFixture(t)
		input.Project = ProjectIdentity{ProjectID: fixture.project}
		input.Spec.Title = fixture.title
		if _, err := store.CreateAtomicThread(context.Background(), createCommand("catalog-create-"+string(rune('a'+index)), "catalog-create-"+string(rune('a'+index))), input); err != nil {
			t.Fatalf("CreateAtomicThread(%q) error = %v", fixture.title, err)
		}
	}
	if _, err := store.db.ExecContext(context.Background(), `UPDATE thread_catalog SET updated_at='2026-07-19T00:00:00Z' WHERE project_id='project-a'`); err != nil {
		t.Fatalf("force catalog timestamp tie: %v", err)
	}

	query := ThreadCatalogQuery{ProjectID: "project-a", Search: "alpha", Archive: ArchiveActive, Limit: 1}
	first, err := store.SearchThreadCatalog(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchThreadCatalog(first) error = %v", err)
	}
	if len(first.Threads) != 1 || first.Threads[0].ProjectID != "project-a" || first.Threads[0].Archived || first.NextCursor == "" {
		t.Fatalf("first page = %#v", first)
	}
	query.Cursor = first.NextCursor
	second, err := store.SearchThreadCatalog(context.Background(), query)
	if err != nil {
		t.Fatalf("SearchThreadCatalog(second) error = %v", err)
	}
	if len(second.Threads) != 1 || second.Threads[0].ThreadID == first.Threads[0].ThreadID || second.NextCursor != "" {
		t.Fatalf("second page = %#v after %#v", second, first)
	}
	if first.Threads[0].ThreadID < second.Threads[0].ThreadID {
		t.Fatalf("catalog order = %q then %q, want descending deterministic tie-break", first.Threads[0].ThreadID, second.Threads[0].ThreadID)
	}

	query.ProjectID = "project-b"
	if _, err := store.SearchThreadCatalog(context.Background(), query); err == nil {
		t.Fatal("SearchThreadCatalog accepted a cursor from another project")
	}
}

func TestThreadCatalogQueryRejectsImplicitScopeAndUnboundedPages(t *testing.T) {
	store := openTestStore(t)
	for _, query := range []ThreadCatalogQuery{
		{Archive: ArchiveActive, Limit: 10},
		{ProjectID: "project-a", Archive: "", Limit: 10},
		{ProjectID: "project-a", Archive: ArchiveActive, Limit: 0},
		{ProjectID: "project-a", Archive: ArchiveActive, Limit: MaxThreadCatalogPageSize + 1},
		{ProjectID: "project-a", Archive: ArchiveActive, Limit: 10, Cursor: "not-base64!"},
	} {
		if _, err := store.SearchThreadCatalog(context.Background(), query); err == nil {
			t.Fatalf("SearchThreadCatalog(%#v) succeeded, want strict DTO validation error", query)
		}
	}
}

func TestArchiveAndUnarchiveAreDurableIdempotentCatalogMutations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	input := atomicCreateFixture(t)
	input.Project = ProjectIdentity{ProjectID: "project-a"}
	created, err := store.CreateAtomicThread(context.Background(), createCommand("archive-create", "archive-create"), input)
	if err != nil {
		t.Fatal(err)
	}

	archive := catalogCommand(created.ThreadID, CommandThreadArchive, "archive", created.LastSequence)
	archived, err := store.ArchiveThread(context.Background(), archive)
	if err != nil {
		t.Fatalf("ArchiveThread() error = %v", err)
	}
	retry, err := store.ArchiveThread(context.Background(), archive)
	if err != nil || !reflect.DeepEqual(retry, archived) {
		t.Fatalf("idempotent ArchiveThread() = %#v, %v; want %#v", retry, err, archived)
	}
	if len(archived.Events) != 1 || archived.Events[0].Type != EventThreadArchived || archived.LastSequence != created.LastSequence+1 {
		t.Fatalf("archive result = %#v", archived)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	page, err := store.SearchThreadCatalog(context.Background(), ThreadCatalogQuery{ProjectID: "project-a", Archive: ArchiveArchived, Limit: 10})
	if err != nil || len(page.Threads) != 1 || !page.Threads[0].Archived || page.Threads[0].ArchivedAt == "" {
		t.Fatalf("reopened archived catalog = %#v, %v", page, err)
	}

	unarchive := catalogCommand(created.ThreadID, CommandThreadUnarchive, "unarchive", archived.LastSequence)
	unarchived, err := store.UnarchiveThread(context.Background(), unarchive)
	if err != nil {
		t.Fatalf("UnarchiveThread() error = %v", err)
	}
	if len(unarchived.Events) != 1 || unarchived.Events[0].Type != EventThreadUnarchived {
		t.Fatalf("unarchive result = %#v", unarchived)
	}
	active, err := store.SearchThreadCatalog(context.Background(), ThreadCatalogQuery{ProjectID: "project-a", Archive: ArchiveActive, Limit: 10})
	if err != nil || len(active.Threads) != 1 || active.Threads[0].Archived || active.Threads[0].ArchivedAt != "" {
		t.Fatalf("active catalog after unarchive = %#v, %v", active, err)
	}
}

func TestConcurrentArchiveCommandsHaveOneSequenceWinner(t *testing.T) {
	store := openTestStore(t)
	input := atomicCreateFixture(t)
	input.Project = ProjectIdentity{ProjectID: "project-race"}
	created, err := store.CreateAtomicThread(context.Background(), createCommand("archive-race-create", "archive-race-create"), input)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			_, err := store.ArchiveThread(context.Background(), catalogCommand(created.ThreadID, CommandThreadArchive, "archive-race-"+string(rune('a'+index)), created.LastSequence))
			errs <- err
		}(index)
	}
	close(start)
	wait.Wait()
	close(errs)
	successes, conflicts := 0, 0
	for err := range errs {
		if err == nil {
			successes++
			continue
		}
		var conflict ConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("archive race loser error = %T %v, want ConflictError", err, err)
		}
		conflicts++
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("archive race successes=%d conflicts=%d, want 1/1", successes, conflicts)
	}
	events, err := store.Replay(context.Background(), created.ThreadID, created.LastSequence, 0)
	if err != nil || len(events) != 1 || events[0].Type != EventThreadArchived {
		t.Fatalf("archive race events = %#v, %v", events, err)
	}
}

func TestThreadCatalogLazilyMigratesCurrentStoreWithoutCatalogTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog-migration.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	input := atomicCreateFixture(t)
	input.Project = ProjectIdentity{ProjectID: "legacy-project"}
	if _, err := store.CreateAtomicThread(context.Background(), createCommand("legacy-catalog-create", "legacy-catalog-create"), input); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	page, err := store.SearchThreadCatalog(context.Background(), ThreadCatalogQuery{ProjectID: "legacy-project", Archive: ArchiveAll, Limit: 10})
	if err != nil || len(page.Threads) != 1 {
		t.Fatalf("lazy migration page = %#v, %v", page, err)
	}
}

func catalogCommand(threadID string, commandType agoprotocol.CommandType, key string, sequence uint64) agoprotocol.Command {
	return agoprotocol.Command{SchemaVersion: agoprotocol.SchemaVersion, CommandID: key, IdempotencyKey: key, ActorID: "catalog-test", Type: commandType, ThreadID: threadID, ExpectedSequence: &sequence}
}
