package agodaemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"claudexflow/internal/agoattachments"
	"claudexflow/internal/agocoordinator"
	"claudexflow/internal/agogit"
	"claudexflow/internal/agopluginhost"
	"claudexflow/internal/agopluginprotocol"
	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
)

func TestBearerAuthenticationRejectsBeforeReadingBody(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef"
	var handled atomic.Int32
	handler, err := RequireBearerToken(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		handled.Add(1)
		_, _ = io.ReadAll(request.Body)
		writer.WriteHeader(http.StatusNoContent)
	}), token)
	if err != nil {
		t.Fatal(err)
	}
	for name, authorization := range map[string]string{"missing": "", "wrong": "Bearer 0123456789abcdef0123456789abcdeg", "wrong scheme": "Basic " + token} {
		t.Run(name, func(t *testing.T) {
			body := &trackingRequestBody{content: []byte("must remain unread")}
			request := httptest.NewRequest(http.MethodPost, "/v1/threads", body)
			request.Header.Set("Authorization", authorization)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized || body.read.Load() != 0 || handled.Load() != 0 {
				t.Fatalf("status=%d reads=%d handled=%d", response.Code, body.read.Load(), handled.Load())
			}
		})
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || handled.Load() != 1 {
		t.Fatalf("authorized status=%d handled=%d", response.Code, handled.Load())
	}
}

func TestBearerAuthenticationRejectsWeakSecrets(t *testing.T) {
	for _, token := range []string{"", strings.Repeat("a", 31), strings.Repeat("a", 32), "contains whitespace 0123456789abcdef0123456789abcdef"} {
		if _, err := RequireBearerToken(http.NotFoundHandler(), token); err == nil {
			t.Fatalf("accepted weak bearer token %q", token)
		}
	}
}

type trackingRequestBody struct {
	content []byte
	read    atomic.Int32
}

func (body *trackingRequestBody) Read(target []byte) (int, error) {
	body.read.Add(1)
	if len(body.content) == 0 {
		return 0, io.EOF
	}
	count := copy(target, body.content)
	body.content = body.content[count:]
	return count, nil
}

func (*trackingRequestBody) Close() error { return nil }

func TestServerAddsSnapshotFencedDiffComment(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{SchemaVersion: 1, CommandID: "create-comment", IdempotencyKey: "create-comment", ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.ThreadSpec{Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	binding := agothreadstore.GitBinding{ThreadID: created.ThreadID, EnvironmentID: "env", ExecutorGeneration: 1, WorktreeDir: "/w", GitDir: "/g", CommonDir: "/c", RepositoryID: "repo", WorktreeID: "worktree", ObjectFormat: "sha1", BaseIdentity: "base"}
	if err := store.RecordGitBinding(context.Background(), binding); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("comment-snapshot")))
	indexDigest := fmt.Sprintf("%x", sha256.Sum256([]byte("comment-index")))
	snapshot, err := store.RecordGitSnapshot(context.Background(), agothreadstore.GitSnapshotInput{ThreadID: created.ThreadID, EnvironmentID: "env", ExecutorGeneration: 1, RepositoryID: "repo", WorktreeID: "worktree", IdempotencyKey: "snapshot-comment", Digest: digest, HeadOID: "head", IndexDigest: indexDigest, Projection: json.RawMessage(`{"staged":[],"unstaged":[{"id":"file:opaque","hunks":[{"id":"hunk:opaque"}]}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(New(store, nil).Handler())
	t.Cleanup(httpServer.Close)
	body := map[string]any{"comment_id": "comment-1", "expected_sequence": snapshot.CreatedSequence, "snapshot_revision": snapshot.Revision, "snapshot_digest": snapshot.Digest, "file_id": "file:opaque", "hunk_id": "hunk:opaque", "actor_id": "web", "body": "Please change only this section"}
	response := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/diff/comments", body)
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", response.StatusCode)
	}
	retry := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/diff/comments", body)
	_ = retry.Body.Close()
	if retry.StatusCode != http.StatusAccepted {
		t.Fatalf("exact lost-response retry status = %d", retry.StatusCode)
	}
	changedRetry := make(map[string]any, len(body))
	for key, value := range body {
		changedRetry[key] = value
	}
	changedRetry["body"] = "changed retry"
	changed := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/diff/comments", changedRetry)
	_ = changed.Body.Close()
	if changed.StatusCode != http.StatusConflict {
		t.Fatalf("changed retry status = %d", changed.StatusCode)
	}
	projection, err := store.ClientProjection(context.Background(), created.ThreadID, 0, 100)
	if err != nil || len(projection.Diff.Comments) != 1 || projection.Diff.Comments[0].Body != body["body"] {
		t.Fatalf("comments = %#v, err=%v", projection.Diff.Comments, err)
	}
	for _, forbidden := range []map[string]any{{"path": "a.go"}, {"patch": "forged"}, {"snapshot_generation": 1}, {"snapshot_revision": snapshot.Revision + 1}} {
		changed := make(map[string]any, len(body)+1)
		for key, value := range body {
			changed[key] = value
		}
		for key, value := range forbidden {
			changed[key] = value
		}
		bad := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/diff/comments", changed)
		_ = bad.Body.Close()
		if bad.StatusCode < 400 {
			t.Fatalf("accepted forbidden comment request %#v", forbidden)
		}
	}
}

func TestServerSearchesAndArchivesProjectScopedThreads(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	create := func(key, project, title string) agothreadstore.MailboxState {
		result, createErr := store.CreateAtomicThread(context.Background(), agoprotocol.Command{SchemaVersion: 1, CommandID: key, IdempotencyKey: key, ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.AtomicCreateInput{
			Spec:    agothreadstore.ThreadSpec{Title: title, Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}},
			Project: agothreadstore.ProjectIdentity{ProjectID: project}, Agent: agothreadstore.AgentDefinitionSnapshot{DefinitionID: "ago.default", Version: "1", DisplayName: "Ago", SystemInstructionsDigest: "sha256:test", DefaultMode: agoprotocol.AgentModeMedium}, InitialMessage: json.RawMessage(`{"text":"start"}`),
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		return result
	}
	wanted := create("create-wanted", "project-one", "Needle thread")
	_ = create("create-other", "project-two", "Needle other project")
	httpServer := httptest.NewServer(New(store, nil).Handler())
	t.Cleanup(httpServer.Close)

	search := requestJSON(t, http.MethodGet, httpServer.URL+"/v1/threads?project_id=project-one&search=needle&archive=active&limit=20", nil)
	defer search.Body.Close()
	if search.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d", search.StatusCode)
	}
	var page agothreadstore.ThreadCatalogPage
	if err := json.NewDecoder(search.Body).Decode(&page); err != nil || len(page.Threads) != 1 || page.Threads[0].ThreadID != wanted.ThreadID {
		t.Fatalf("search page = %#v, err=%v", page, err)
	}
	archive := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+wanted.ThreadID+"/archive", map[string]any{"command_id": "archive", "idempotency_key": "archive", "actor_id": "web", "expected_sequence": wanted.LastSequence})
	defer archive.Body.Close()
	if archive.StatusCode != http.StatusAccepted {
		t.Fatalf("archive status = %d", archive.StatusCode)
	}
	active := requestJSON(t, http.MethodGet, httpServer.URL+"/v1/threads?project_id=project-one&archive=active&limit=20", nil)
	defer active.Body.Close()
	var activePage agothreadstore.ThreadCatalogPage
	if err := json.NewDecoder(active.Body).Decode(&activePage); err != nil || len(activePage.Threads) != 0 {
		t.Fatalf("active page = %#v, err=%v", activePage, err)
	}
}

func TestServerRejectsNoncanonicalOrUnresolvedMessageReferences(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	httpServer := httptest.NewServer(New(store, nil).Handler())
	t.Cleanup(httpServer.Close)
	base := map[string]any{
		"command_id": "create-message-contract", "idempotency_key": "create-message-contract", "actor_id": "test",
		"spec":    map[string]any{"title": "Contract", "workspace": t.TempDir(), "mode": "medium", "executor": map[string]any{"type": "local"}},
		"project": map[string]any{"project_id": "project"}, "agent": map[string]any{"definition_id": "ago.default", "version": "1", "display_name": "Ago", "system_instructions_digest": "sha256:test", "default_mode": "medium"},
	}
	for name, message := range map[string]any{
		"unknown field":         map[string]any{"text": "hello", "workspace": "/forged"},
		"unresolved attachment": map[string]any{"attachments": []any{map[string]any{"attachment_id": "att-1", "sha256": strings.Repeat("a", 64), "size_bytes": 1, "media_type": "text/plain", "filename": "a.txt"}}},
		"unresolved mention":    map[string]any{"file_mentions": []any{map[string]any{"path": "README.md"}}},
	} {
		t.Run(name, func(t *testing.T) {
			body := make(map[string]any, len(base)+1)
			for key, value := range base {
				body[key] = value
			}
			body["command_id"], body["idempotency_key"], body["initial_message"] = "create-"+name, "create-"+name, message
			response := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads", body)
			_ = response.Body.Close()
			if response.StatusCode < 400 {
				t.Fatalf("accepted noncanonical or unresolved message: %#v", message)
			}
		})
	}
}

func TestServerUploadsAndAuthorizesImmutableAttachmentsOnSubmit(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	attachments, err := agoattachments.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer attachments.Close()
	workspace := t.TempDir()
	created, err := store.CreateAtomicThread(context.Background(), agoprotocol.Command{SchemaVersion: 1, CommandID: "attachment-create", IdempotencyKey: "attachment-create", ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.AtomicCreateInput{
		Spec:    agothreadstore.ThreadSpec{Workspace: workspace, Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}},
		Project: agothreadstore.ProjectIdentity{ProjectID: "project-1"}, Agent: agothreadstore.AgentDefinitionSnapshot{DefinitionID: "ago", Version: "1", DisplayName: "Ago", SystemInstructionsDigest: "sha256:test", DefaultMode: agoprotocol.AgentModeMedium}, InitialMessage: json.RawMessage(`{"text":"start"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := agocoordinator.New(store, &daemonExecutor{started: make(chan agocoordinator.TurnRequest, 1), finish: make(chan error, 1)})
	httpServer := httptest.NewServer(New(store, coordinator).WithAttachments(attachments).Handler())
	defer httpServer.Close()
	content := []byte("uploaded bytes")
	ref := daemonAttachmentRef("att-upload", "upload.txt", "text/plain", content)

	const racers = 16
	start := make(chan struct{})
	type uploadResult struct {
		status int
		err    error
	}
	results := make(chan uploadResult, racers)
	for range racers {
		go func() {
			<-start
			response, requestErr := doAttachmentRequest(httpServer.URL+"/v1/threads/"+created.ThreadID+"/attachments", ref, content)
			if requestErr != nil {
				results <- uploadResult{err: requestErr}
				return
			}
			results <- uploadResult{status: response.StatusCode}
			_ = response.Body.Close()
		}()
	}
	close(start)
	for range racers {
		result := <-results
		if result.err != nil || result.status != http.StatusCreated {
			t.Fatalf("concurrent exact upload = status %d, error %v", result.status, result.err)
		}
	}

	submit := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/messages", map[string]any{
		"command_id": "attachment-submit", "idempotency_key": "attachment-submit", "actor_id": "user", "class": "normal",
		"content": map[string]any{"text": "inspect", "attachments": []agoprotocol.AttachmentRef{ref}},
	})
	_ = submit.Body.Close()
	if submit.StatusCode != http.StatusAccepted {
		t.Fatalf("authorized attachment submit status = %d", submit.StatusCode)
	}

	changedContent := []byte("changed bytes")
	changedRef := daemonAttachmentRef(ref.AttachmentID, "changed.txt", "text/plain", changedContent)
	changed := requestAttachment(t, httpServer.URL+"/v1/threads/"+created.ThreadID+"/attachments", changedRef, changedContent)
	_ = changed.Body.Close()
	if changed.StatusCode != http.StatusConflict {
		t.Fatalf("changed upload status = %d", changed.StatusCode)
	}
	wrongRef := ref
	wrongRef.Filename = "wrong.txt"
	rejected := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/messages", map[string]any{
		"command_id": "attachment-wrong-ref", "idempotency_key": "attachment-wrong-ref", "actor_id": "user", "class": "normal",
		"content": map[string]any{"attachments": []agoprotocol.AttachmentRef{wrongRef}},
	})
	_ = rejected.Body.Close()
	if rejected.StatusCode != http.StatusConflict {
		t.Fatalf("changed attachment ref submit status = %d", rejected.StatusCode)
	}
	second, err := store.CreateAtomicThread(context.Background(), agoprotocol.Command{SchemaVersion: 1, CommandID: "attachment-create-second", IdempotencyKey: "attachment-create-second", ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.AtomicCreateInput{
		Spec:    agothreadstore.ThreadSpec{Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}},
		Project: agothreadstore.ProjectIdentity{ProjectID: "project-2"}, Agent: agothreadstore.AgentDefinitionSnapshot{DefinitionID: "ago", Version: "1", DisplayName: "Ago", SystemInstructionsDigest: "sha256:test", DefaultMode: agoprotocol.AgentModeMedium}, InitialMessage: json.RawMessage(`{"text":"start"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongOwner := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+second.ThreadID+"/messages", map[string]any{
		"command_id": "attachment-wrong-owner", "idempotency_key": "attachment-wrong-owner", "actor_id": "user", "class": "normal",
		"content": map[string]any{"attachments": []agoprotocol.AttachmentRef{ref}},
	})
	_ = wrongOwner.Body.Close()
	if wrongOwner.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong-owner attachment submit status = %d", wrongOwner.StatusCode)
	}
	secondMailbox, err := store.Mailbox(context.Background(), second.ThreadID)
	if err != nil || secondMailbox.LastSequence != 3 {
		t.Fatalf("wrong-owner submit changed mailbox = %#v, %v", secondMailbox, err)
	}
	missingRef := daemonAttachmentRef("att-missing", "missing.txt", "text/plain", []byte("missing"))
	beforeMixed, err := store.Mailbox(context.Background(), created.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	mixed := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/messages", map[string]any{
		"command_id": "attachment-mixed", "idempotency_key": "attachment-mixed", "actor_id": "user", "class": "normal",
		"content": map[string]any{"attachments": []agoprotocol.AttachmentRef{ref, missingRef}},
	})
	_ = mixed.Body.Close()
	if mixed.StatusCode != http.StatusNotFound {
		t.Fatalf("mixed attachment submit status = %d", mixed.StatusCode)
	}
	afterMixed, err := store.Mailbox(context.Background(), created.ThreadID)
	if err != nil || afterMixed.LastSequence != beforeMixed.LastSequence {
		t.Fatalf("mixed attachment submit changed mailbox: before=%#v after=%#v err=%v", beforeMixed, afterMixed, err)
	}

	unknownThread := requestAttachment(t, httpServer.URL+"/v1/threads/T-missing/attachments", ref, content)
	_ = unknownThread.Body.Close()
	if unknownThread.StatusCode < 400 {
		t.Fatalf("unknown-thread upload status = %d", unknownThread.StatusCode)
	}
	malformedRequest, err := http.NewRequest(http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/attachments", bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	malformedRequest.Header.Set("X-Ago-Attachment-Ref", `{"attachment_id":"att-upload","unknown":true}`)
	malformed, err := http.DefaultClient.Do(malformedRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = malformed.Body.Close()
	if malformed.StatusCode < 400 {
		t.Fatalf("unknown attachment metadata field status = %d", malformed.StatusCode)
	}
	boundedRef := daemonAttachmentRef("att-bounded", "bounded.bin", "application/octet-stream", []byte("bounded"))
	boundedRequest, err := http.NewRequest(http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/attachments", nil)
	if err != nil {
		t.Fatal(err)
	}
	encodedBoundedRef, _ := json.Marshal(boundedRef)
	boundedRequest.Header.Set("X-Ago-Attachment-Ref", string(encodedBoundedRef))
	boundedRequest.Body = io.NopCloser(bytes.NewReader(bytes.Repeat([]byte{'x'}, int(agoprotocol.MaxAttachmentBytes)+1)))
	boundedRequest.ContentLength = -1
	bounded, err := http.DefaultClient.Do(boundedRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = bounded.Body.Close()
	if bounded.StatusCode < 400 {
		t.Fatalf("chunked oversized upload status = %d", bounded.StatusCode)
	}
	partial := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/messages", map[string]any{
		"command_id": "attachment-partial", "idempotency_key": "attachment-partial", "actor_id": "user", "class": "normal",
		"content": map[string]any{"attachments": []agoprotocol.AttachmentRef{boundedRef}},
	})
	_ = partial.Body.Close()
	if partial.StatusCode != http.StatusNotFound {
		t.Fatalf("partial oversized attachment became usable, status = %d", partial.StatusCode)
	}

	withoutAttachments := httptest.NewServer(New(store, coordinator).Handler())
	defer withoutAttachments.Close()
	unavailable := requestAttachment(t, withoutAttachments.URL+"/v1/threads/"+created.ThreadID+"/attachments", ref, content)
	_ = unavailable.Body.Close()
	if unavailable.StatusCode < 400 {
		t.Fatalf("unconfigured attachment endpoint status = %d", unavailable.StatusCode)
	}
}

func TestServerResolvesFileMentionsOnlyAgainstLatestExactGitBinding(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", "file.txt"), []byte("bound"), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateAtomicThread(context.Background(), agoprotocol.Command{SchemaVersion: 1, CommandID: "mention-create", IdempotencyKey: "mention-create", ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.AtomicCreateInput{
		Spec:    agothreadstore.ThreadSpec{Workspace: workspace, Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}},
		Project: agothreadstore.ProjectIdentity{ProjectID: "project-1"}, Agent: agothreadstore.AgentDefinitionSnapshot{DefinitionID: "ago", Version: "1", DisplayName: "Ago", SystemInstructionsDigest: "sha256:test", DefaultMode: agoprotocol.AgentModeMedium}, InitialMessage: json.RawMessage(`{"text":"start"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	thread, err := store.Thread(context.Background(), created.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	workspace = thread.Workspace
	recordBinding := func(generation uint64, worktree string) {
		t.Helper()
		if err := store.RecordGitBinding(context.Background(), agothreadstore.GitBinding{ThreadID: created.ThreadID, EnvironmentID: "thread:" + created.ThreadID, ExecutorGeneration: generation, WorktreeDir: worktree, GitDir: filepath.Join(worktree, ".git"), CommonDir: filepath.Join(worktree, ".git"), RepositoryID: "repo", WorktreeID: fmt.Sprintf("worktree-%d", generation), ObjectFormat: "sha1", BaseIdentity: "base"}); err != nil {
			t.Fatal(err)
		}
	}
	recordBinding(1, workspace)
	recordBinding(2, workspace)
	coordinator := agocoordinator.New(store, &daemonExecutor{started: make(chan agocoordinator.TurnRequest, 1), finish: make(chan error, 1)})
	httpServer := httptest.NewServer(New(store, coordinator).Handler())
	defer httpServer.Close()
	submitMention := func(id, path string) int {
		t.Helper()
		response := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/messages", map[string]any{
			"command_id": id, "idempotency_key": id, "actor_id": "user", "class": "normal",
			"content": map[string]any{"file_mentions": []map[string]string{{"path": path}}},
		})
		defer response.Body.Close()
		return response.StatusCode
	}
	if status := submitMention("mention-ok", "nested/file.txt"); status != http.StatusAccepted {
		t.Fatalf("valid mention status = %d", status)
	}
	if err := os.Symlink(filepath.Join(workspace, "nested", "file.txt"), filepath.Join(workspace, "link.txt")); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": "missing.txt", "directory": "nested", "final symlink": "link.txt", "component symlink": "linked-dir/outside.txt"} {
		if status := submitMention("mention-"+name, path); status < 400 {
			t.Fatalf("%s mention status = %d", name, status)
		}
	}
	recordBinding(3, t.TempDir())
	if status := submitMention("mention-binding-mismatch", "nested/file.txt"); status < 400 {
		t.Fatalf("binding mismatch mention status = %d", status)
	}
}

func daemonAttachmentRef(id, filename, mediaType string, content []byte) agoprotocol.AttachmentRef {
	digest := sha256.Sum256(content)
	return agoprotocol.AttachmentRef{AttachmentID: id, SHA256: hex.EncodeToString(digest[:]), SizeBytes: uint64(len(content)), MediaType: mediaType, Filename: filename}
}

func requestAttachment(t *testing.T, url string, ref agoprotocol.AttachmentRef, content []byte) *http.Response {
	t.Helper()
	response, err := doAttachmentRequest(url, ref, content)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func doAttachmentRequest(url string, ref agoprotocol.AttachmentRef, content []byte) (*http.Response, error) {
	encoded, err := json.Marshal(ref)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Ago-Attachment-Ref", string(encoded))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	return response, nil
}

func TestServerRefreshesDiffFromAuthoritativeThreadConfiguration(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	created, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{SchemaVersion: agoprotocol.SchemaVersion, CommandID: "create", IdempotencyKey: "create", ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.ThreadSpec{Workspace: workspace, Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	want := agothreadstore.GitSnapshot{ThreadID: created.ThreadID, EnvironmentID: "thread:" + created.ThreadID, ExecutorGeneration: 1, RepositoryID: "repo", WorktreeID: "worktree", Revision: 2, Digest: "digest", HeadOID: "head", IndexDigest: "index", Projection: json.RawMessage(`{"staged":[]}`), CreatedSequence: 9, CreatedAt: "2026-01-02T03:04:05Z"}
	fake := &capturingGitRefresher{snapshot: want}
	httpServer := httptest.NewServer(New(store, nil).WithGitRefresher(fake).Handler())
	t.Cleanup(httpServer.Close)

	response := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/diff/refresh", map[string]any{"idempotency_key": "refresh-1"})
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var got agothreadstore.GitSnapshot
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("response = %#v, want exact snapshot %#v", got, want)
	}
	if fake.calls != 1 || fake.input.ThreadID != created.ThreadID || fake.input.Workspace != workspace || fake.input.Executor.Type != agoprotocol.ExecutorLocal || fake.input.EnvironmentID != "thread:"+created.ThreadID || fake.input.ExecutorGeneration != 1 || fake.input.IdempotencyKey != "refresh-1" {
		t.Fatalf("refresh input = %#v, calls=%d", fake.input, fake.calls)
	}
}

func TestServerRejectsInvalidDiffRefreshWithoutCallingService(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{SchemaVersion: agoprotocol.SchemaVersion, CommandID: "create", IdempotencyKey: "create", ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.ThreadSpec{Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &capturingGitRefresher{}
	httpServer := httptest.NewServer(New(store, nil).WithGitRefresher(fake).Handler())
	t.Cleanup(httpServer.Close)
	for name, test := range map[string]struct {
		threadID string
		body     any
	}{
		"unknown field":               {created.ThreadID, map[string]any{"idempotency_key": "x", "workspace": "/forged"}},
		"client executor override":    {created.ThreadID, map[string]any{"idempotency_key": "x", "executor": map[string]any{"type": "local"}}},
		"client provider override":    {created.ThreadID, map[string]any{"idempotency_key": "x", "provider": "forged"}},
		"client model override":       {created.ThreadID, map[string]any{"idempotency_key": "x", "model": "forged"}},
		"missing thread":              {"T-missing", map[string]any{"idempotency_key": "x"}},
		"client environment override": {created.ThreadID, map[string]any{"idempotency_key": "x", "environment_id": "forged"}},
	} {
		t.Run(name, func(t *testing.T) {
			before := fake.calls
			response := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+test.threadID+"/diff/refresh", test.body)
			defer response.Body.Close()
			if response.StatusCode == http.StatusAccepted || fake.calls != before {
				t.Fatalf("status=%d calls=%d", response.StatusCode, fake.calls)
			}
		})
	}
}

func TestServerDoesNotFallbackForUnsupportedDiffRefreshExecutor(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{SchemaVersion: agoprotocol.SchemaVersion, CommandID: "create", IdempotencyKey: "create", ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.ThreadSpec{Workspace: "/runner/work", Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorRunner, RunnerID: "runner-1"}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &capturingGitRefresher{err: &agogit.UnsupportedExecutorError{Target: agoprotocol.ExecutorRunner}}
	httpServer := httptest.NewServer(New(store, nil).WithGitRefresher(fake).Handler())
	t.Cleanup(httpServer.Close)
	response := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/diff/refresh", map[string]any{"idempotency_key": "x"})
	defer response.Body.Close()
	if response.StatusCode < 400 || fake.calls != 1 || fake.input.Executor.Type != agoprotocol.ExecutorRunner {
		t.Fatalf("status=%d calls=%d input=%#v", response.StatusCode, fake.calls, fake.input)
	}
}

type capturingGitRefresher struct {
	input          agogit.RefreshInput
	snapshot       agothreadstore.GitSnapshot
	err            error
	calls          int
	mutationInput  agogit.MutationInput
	mutationResult agogit.MutationResult
	mutationErr    error
	mutationCalls  int
	revertInput    agogit.RevertInput
	revertResult   agogit.MutationResult
	revertCalls    int
}

func (f *capturingGitRefresher) Refresh(_ context.Context, input agogit.RefreshInput) (agothreadstore.GitSnapshot, error) {
	f.calls++
	f.input = input
	return f.snapshot, f.err
}

func (f *capturingGitRefresher) Mutate(_ context.Context, input agogit.MutationInput) (agogit.MutationResult, error) {
	f.mutationCalls++
	f.mutationInput = input
	return f.mutationResult, f.mutationErr
}

func (f *capturingGitRefresher) Revert(_ context.Context, input agogit.RevertInput) (agogit.MutationResult, error) {
	f.revertCalls++
	f.revertInput = input
	return f.revertResult, nil
}

func TestServerDiffStageUsesOnlyDaemonOwnedWorkspaceAndExecutor(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{SchemaVersion: 1, CommandID: "create-stage", IdempotencyKey: "create-stage", ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.ThreadSpec{Workspace: workspace, Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &capturingGitRefresher{mutationResult: agogit.MutationResult{Operation: agothreadstore.GitOperation{OperationID: "G-one", State: agothreadstore.GitOperationCompleted}}}
	httpServer := httptest.NewServer(New(store, nil).WithGitRefresher(fake).Handler())
	t.Cleanup(httpServer.Close)
	response := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/diff/stage", map[string]any{
		"command_id": "git:stage", "idempotency_key": "stage", "actor_id": "alice",
		"expected_sequence":          1,
		"expected_snapshot_revision": 2, "expected_snapshot_digest": strings.Repeat("a", 64), "selected_unit_ids": []string{"unit-1"},
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || fake.mutationCalls != 1 {
		t.Fatalf("status=%d calls=%d", response.StatusCode, fake.mutationCalls)
	}
	if fake.mutationInput.Workspace != workspace || fake.mutationInput.Executor.Type != agoprotocol.ExecutorLocal || fake.mutationInput.EnvironmentID != "thread:"+created.ThreadID || fake.mutationInput.Kind != agogit.MutationStage {
		t.Fatalf("mutation input = %#v", fake.mutationInput)
	}
	for _, forbidden := range []map[string]any{{"workspace": "/forged"}, {"executor_generation": 99}, {"path": "same.txt"}, {"patch": "forged"}} {
		body := map[string]any{"command_id": "git:forged", "idempotency_key": "forged", "actor_id": "alice", "expected_sequence": 1, "expected_snapshot_revision": 2, "expected_snapshot_digest": strings.Repeat("a", 64), "selected_unit_ids": []string{"unit-1"}}
		for key, value := range forbidden {
			body[key] = value
		}
		before := fake.mutationCalls
		bad := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/diff/stage", body)
		_ = bad.Body.Close()
		if bad.StatusCode < 400 || fake.mutationCalls != before {
			t.Fatalf("forbidden request %#v status=%d calls=%d", forbidden, bad.StatusCode, fake.mutationCalls)
		}
	}
}

func TestServerReceiptRevertAcceptsOnlyReceiptAndDaemonOwnedScope(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	workspace := t.TempDir()
	created, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{SchemaVersion: 1, CommandID: "create-revert", IdempotencyKey: "create-revert", ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.ThreadSpec{Workspace: workspace, Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	fake := &capturingGitRefresher{revertResult: agogit.MutationResult{Operation: agothreadstore.GitOperation{OperationID: "G-revert", State: agothreadstore.GitOperationCompleted}}}
	httpServer := httptest.NewServer(New(store, nil).WithGitRefresher(fake).Handler())
	t.Cleanup(httpServer.Close)
	body := map[string]any{"command_id": "git:revert", "idempotency_key": "revert", "actor_id": "alice", "expected_sequence": 1, "expected_snapshot_revision": 2, "expected_snapshot_digest": strings.Repeat("a", 64), "receipt_id": "R-one"}
	response := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/diff/revert", body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || fake.revertCalls != 1 {
		t.Fatalf("status=%d calls=%d", response.StatusCode, fake.revertCalls)
	}
	if fake.revertInput.Workspace != workspace || fake.revertInput.ThreadID != created.ThreadID || fake.revertInput.ReceiptID != "R-one" || fake.revertInput.ExecutorGeneration != 1 {
		t.Fatalf("revert input = %#v", fake.revertInput)
	}
	for _, forbidden := range []map[string]any{{"path": "file.txt"}, {"patch": "forged"}, {"workspace": "/forged"}, {"executor_generation": 99}} {
		forged := make(map[string]any, len(body)+1)
		for key, value := range body {
			forged[key] = value
		}
		for key, value := range forbidden {
			forged[key] = value
		}
		before := fake.revertCalls
		bad := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/diff/revert", forged)
		_ = bad.Body.Close()
		if bad.StatusCode < 400 || fake.revertCalls != before {
			t.Fatalf("forbidden request %#v status=%d calls=%d", forbidden, bad.StatusCode, fake.revertCalls)
		}
	}
}

func TestServerCreatesAndRunsThreadWithoutRenderer(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	executor := &daemonExecutor{started: make(chan agocoordinator.TurnRequest, 1), finish: make(chan error, 1)}
	coordinator := agocoordinator.New(store, executor)
	httpServer := httptest.NewServer(New(store, coordinator).Handler())
	t.Cleanup(httpServer.Close)

	health := requestJSON(t, http.MethodGet, httpServer.URL+"/v1/health", nil)
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", health.StatusCode)
	}
	_ = health.Body.Close()

	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	createBody := map[string]any{
		"command_id":      "cmd-create",
		"idempotency_key": "request-create",
		"actor_id":        "user-1",
		"spec": map[string]any{
			"title":     "Headless thread",
			"workspace": workspace,
			"mode":      "medium",
			"executor":  map[string]any{"type": "local"},
		},
		"project":         map[string]any{"project_id": "project-1", "display_name": "Project"},
		"agent":           map[string]any{"definition_id": "agent-1", "version": "1", "display_name": "Agent", "system_instructions_digest": "sha256:test", "default_mode": "medium"},
		"initial_message": map[string]any{"text": "run headlessly"},
	}
	createdResponse := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads", createBody)
	if createdResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("create status = %d", createdResponse.StatusCode)
	}
	var created agothreadstore.MailboxState
	if err := json.NewDecoder(createdResponse.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	_ = createdResponse.Body.Close()

	select {
	case run := <-executor.started:
		if run.ThreadID != created.ThreadID || run.Workspace == "" || run.Mode != agoprotocol.AgentModeMedium || run.Executor.Type != agoprotocol.ExecutorLocal {
			t.Fatalf("executor run = %#v", run)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not dispatch the durable turn")
	}

	retryResponse := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads", createBody)
	var retry agothreadstore.MailboxState
	if retryResponse.StatusCode != http.StatusAccepted || json.NewDecoder(retryResponse.Body).Decode(&retry) != nil {
		t.Fatalf("atomic create retry status = %d", retryResponse.StatusCode)
	}
	_ = retryResponse.Body.Close()
	if retry.ThreadID != created.ThreadID || retry.LastSequence != 3 {
		t.Fatalf("atomic create retry = %#v, created = %#v", retry, created)
	}
	select {
	case duplicate := <-executor.started:
		t.Fatalf("atomic create retry launched duplicate turn: %#v", duplicate)
	case <-time.After(50 * time.Millisecond):
	}

	eventsResponse := requestJSON(t, http.MethodGet, httpServer.URL+"/v1/threads/"+created.ThreadID+"/events?after=1", nil)
	if eventsResponse.StatusCode != http.StatusOK {
		t.Fatalf("events status = %d", eventsResponse.StatusCode)
	}
	var replay struct {
		Events []agoprotocol.Event `json:"events"`
	}
	if err := json.NewDecoder(eventsResponse.Body).Decode(&replay); err != nil {
		t.Fatalf("decode events response: %v", err)
	}
	_ = eventsResponse.Body.Close()
	if len(replay.Events) != 2 || replay.Events[0].Type != agoprotocol.EventMessageAccepted || replay.Events[1].Type != agoprotocol.EventTurnStarted {
		t.Fatalf("events = %#v, want accepted and turn.started", replay.Events)
	}

	executor.finish <- nil
}

func TestServerRejectsClientAuthoredCreateProvenance(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	server := httptest.NewServer(New(store, nil).Handler())
	t.Cleanup(server.Close)
	response := requestJSON(t, http.MethodPost, server.URL+"/v1/threads", map[string]any{
		"command_id": "forged", "idempotency_key": "forged", "actor_id": "client",
		"spec":            map[string]any{"title": "forged", "workspace": t.TempDir(), "mode": "medium", "executor": map[string]any{"type": "local"}},
		"project":         map[string]any{"project_id": "p"},
		"agent":           map[string]any{"definition_id": "a", "version": "1", "display_name": "A", "system_instructions_digest": "sha256:test", "default_mode": "medium"},
		"initial_message": map[string]any{"text": "start"},
		"provenance":      map[string]any{"root_thread_id": "T-forged"},
	})
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("client-authored provenance status = %d", response.StatusCode)
	}
	threads, err := store.ListThreads(context.Background())
	if err != nil || len(threads) != 0 {
		t.Fatalf("forged create persisted: %#v, %v", threads, err)
	}
}

func TestServerClientProjectionCombinesOneDurableSnapshotWithPluginGeneration(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.CreateAtomicThread(context.Background(), agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: "projection-create", IdempotencyKey: "projection-create", ActorID: "test", Type: agoprotocol.CommandThreadCreate,
	}, agothreadstore.AtomicCreateInput{
		Spec:    agothreadstore.ThreadSpec{Title: "Projection", Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}},
		Project: agothreadstore.ProjectIdentity{ProjectID: "project"}, Agent: agothreadstore.AgentDefinitionSnapshot{DefinitionID: "ago.default", Version: "1", DisplayName: "Ago", SystemInstructionsDigest: "sha256:test", DefaultMode: agoprotocol.AgentModeMedium}, InitialMessage: json.RawMessage(`{"text":"start"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	plugins := &daemonPluginCommands{snapshot: agopluginhost.Snapshot{Generation: 7, Registrations: []agopluginprotocol.PluginRegistration{{PluginID: "acme"}}}}
	server := httptest.NewServer(NewWithRuntime(store, nil, nil, plugins).Handler())
	t.Cleanup(server.Close)

	response := requestJSON(t, http.MethodGet, server.URL+"/v1/threads/"+created.ThreadID+"/projection?after=0&limit=2", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("projection status = %d", response.StatusCode)
	}
	var projection struct {
		agothreadstore.ClientProjection
		Plugins struct {
			Available     bool                                   `json:"available"`
			Generation    int64                                  `json:"generation"`
			Registrations []agopluginprotocol.PluginRegistration `json:"registrations"`
		} `json:"plugins"`
		Executor struct {
			Activity     agoprotocol.Activity       `json:"activity"`
			ActiveTurnID string                     `json:"active_turn_id"`
			Target       agoprotocol.ExecutorTarget `json:"target"`
		} `json:"executor"`
	}
	if err := json.NewDecoder(response.Body).Decode(&projection); err != nil {
		t.Fatal(err)
	}
	if projection.SchemaVersion != 1 || len(projection.Events) != 2 || !projection.HasMore || projection.SnapshotSequence != 3 {
		t.Fatalf("durable projection = %#v", projection.ClientProjection)
	}
	if !projection.Plugins.Available || projection.Plugins.Generation != 7 || len(projection.Plugins.Registrations) != 1 {
		t.Fatalf("plugin projection = %#v", projection.Plugins)
	}
	if projection.Executor.Activity != agoprotocol.ActivityRunning || projection.Executor.ActiveTurnID != created.ActiveTurnID || projection.Executor.Target.Type != agoprotocol.ExecutorLocal {
		t.Fatalf("executor projection = %#v", projection.Executor)
	}
}

func TestServerExposesDurableMailboxControlsWithConflictFencing(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	executor := &daemonExecutor{started: make(chan agocoordinator.TurnRequest, 4), finish: make(chan error, 1)}
	coordinator := agocoordinator.New(store, executor)
	httpServer := httptest.NewServer(New(store, coordinator).Handler())
	t.Cleanup(httpServer.Close)

	createdResponse := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads", map[string]any{
		"command_id": "controls-create", "idempotency_key": "controls-create", "actor_id": "user-1",
		"spec":            map[string]any{"title": "Controls", "workspace": t.TempDir(), "mode": "medium", "executor": map[string]any{"type": "local"}},
		"project":         map[string]any{"project_id": "controls-project"},
		"agent":           map[string]any{"definition_id": "agent", "version": "1", "display_name": "Agent", "system_instructions_digest": "sha256:test", "default_mode": "medium"},
		"initial_message": map[string]any{"text": "first"},
	})
	var created agothreadstore.MailboxState
	if err := json.NewDecoder(createdResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	_ = createdResponse.Body.Close()

	run := receiveDaemonRun(t, executor.started)
	queued := submitMessage(t, httpServer.URL, created.ThreadID, "controls-queued", "queued")
	if len(queued.Queue) != 1 {
		t.Fatalf("queued state = %#v", queued)
	}
	queueItemID := queued.Queue[0].QueueItemID
	expected := queued.LastSequence

	editBody := map[string]any{
		"command_id": "controls-edit", "idempotency_key": "controls-edit", "actor_id": "user-1",
		"expected_sequence": expected, "content": map[string]any{"text": "edited"},
	}
	editURL := httpServer.URL + "/v1/threads/" + created.ThreadID + "/queue/" + queueItemID
	editedResponse := requestJSON(t, http.MethodPatch, editURL, editBody)
	if editedResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("edit status = %d", editedResponse.StatusCode)
	}
	var edited agothreadstore.MailboxState
	if err := json.NewDecoder(editedResponse.Body).Decode(&edited); err != nil {
		t.Fatal(err)
	}
	_ = editedResponse.Body.Close()
	if string(edited.Queue[0].Content) != `{"text":"edited"}` {
		t.Fatalf("edited queue = %#v", edited.Queue)
	}

	retryResponse := requestJSON(t, http.MethodPatch, editURL, editBody)
	var retry agothreadstore.MailboxState
	if err := json.NewDecoder(retryResponse.Body).Decode(&retry); err != nil {
		t.Fatal(err)
	}
	_ = retryResponse.Body.Close()
	if retry.LastSequence != edited.LastSequence || len(retry.Events) != len(edited.Events) || retry.Events[0].EventID != edited.Events[0].EventID {
		t.Fatalf("idempotent retry changed result: edited=%#v retry=%#v", edited, retry)
	}

	staleResponse := requestJSON(t, http.MethodPatch, editURL, map[string]any{
		"command_id": "controls-stale", "idempotency_key": "controls-stale", "actor_id": "user-1",
		"expected_sequence": expected, "content": map[string]any{"text": "stale"},
	})
	if staleResponse.StatusCode != http.StatusConflict {
		t.Fatalf("stale edit status = %d", staleResponse.StatusCode)
	}
	_ = staleResponse.Body.Close()

	steerResponse := requestJSON(t, http.MethodPost, editURL+"/steer", map[string]any{
		"command_id": "controls-steer", "idempotency_key": "controls-steer", "actor_id": "user-1", "expected_turn_id": run.TurnID,
	})
	if steerResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("steer status = %d", steerResponse.StatusCode)
	}
	_ = steerResponse.Body.Close()

	dequeueResponse := requestJSON(t, http.MethodDelete, editURL, map[string]any{
		"command_id": "controls-dequeue", "idempotency_key": "controls-dequeue", "actor_id": "user-1",
	})
	if dequeueResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("dequeue status = %d", dequeueResponse.StatusCode)
	}
	_ = dequeueResponse.Body.Close()

	interruptResponse := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/turns/"+run.TurnID+"/interrupt", map[string]any{
		"command_id": "controls-interrupt", "idempotency_key": "controls-interrupt", "actor_id": "user-1", "content": map[string]any{"text": "replacement"},
	})
	if interruptResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("interrupt status = %d", interruptResponse.StatusCode)
	}
	_ = interruptResponse.Body.Close()
	replacement := receiveDaemonRun(t, executor.started)

	cancelResponse := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/turns/"+replacement.TurnID+"/cancel", map[string]any{
		"command_id": "controls-cancel", "idempotency_key": "controls-cancel", "actor_id": "user-1",
	})
	if cancelResponse.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel status = %d", cancelResponse.StatusCode)
	}
	_ = cancelResponse.Body.Close()
}

func TestServerListsAndResolvesDurablePluginDialogs(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{SchemaVersion: agoprotocol.SchemaVersion, CommandID: "dialog-create", IdempotencyKey: "dialog-create", ActorID: "user", Type: agoprotocol.CommandThreadCreate}, agothreadstore.ThreadSpec{Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	active, err := store.Submit(context.Background(), agoprotocol.Command{SchemaVersion: agoprotocol.SchemaVersion, CommandID: "dialog-submit", IdempotencyKey: "dialog-submit", ActorID: "user", Type: agoprotocol.CommandMessageSubmit, ThreadID: created.ThreadID}, agothreadstore.MessageInput{Content: json.RawMessage(`{"text":"active"}`), Class: agoprotocol.QueueNormal})
	if err != nil {
		t.Fatal(err)
	}
	dialog, err := store.CreatePendingDialog(context.Background(), agothreadstore.CreateDialogInput{ThreadID: created.ThreadID, TurnID: active.ActiveTurnID, PluginID: "approval", Generation: 1, InvocationID: "invoke", Deadline: time.Now().Add(time.Minute), RequestType: "confirm", Request: json.RawMessage(`{"message":"continue?"}`)})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := agocoordinator.New(store, &daemonExecutor{started: make(chan agocoordinator.TurnRequest), finish: make(chan error)})
	httpServer := httptest.NewServer(NewWithDialogs(store, coordinator, store).Handler())
	t.Cleanup(httpServer.Close)

	listed := requestJSON(t, http.MethodGet, httpServer.URL+"/v1/threads/"+created.ThreadID+"/dialogs", nil)
	var pending struct {
		Dialogs []agothreadstore.PluginDialog `json:"dialogs"`
	}
	if listed.StatusCode != http.StatusOK || json.NewDecoder(listed.Body).Decode(&pending) != nil || len(pending.Dialogs) != 1 || pending.Dialogs[0].DialogID != dialog.DialogID {
		t.Fatalf("listed dialogs = %#v status=%d", pending, listed.StatusCode)
	}
	_ = listed.Body.Close()
	resolved := requestJSON(t, http.MethodPost, httpServer.URL+"/v1/threads/"+created.ThreadID+"/dialogs/"+dialog.DialogID+"/resolve", map[string]any{
		"resolver_id": "client", "expected_revision": 1, "expected_sequence": dialog.RequestedSequence,
		"response": map[string]any{"status": "ok", "value": true},
	})
	if resolved.StatusCode != http.StatusAccepted {
		t.Fatalf("resolve status = %d", resolved.StatusCode)
	}
	_ = resolved.Body.Close()
	pendingDialogs, err := store.ListPendingDialogs(context.Background(), created.ThreadID)
	if err != nil || len(pendingDialogs) != 0 {
		t.Fatalf("pending after resolve = %#v, %v", pendingDialogs, err)
	}
}

func TestServerListsAndExecutesCanonicalPluginCommandForThread(t *testing.T) {
	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{SchemaVersion: agoprotocol.SchemaVersion, CommandID: "plugin-create", IdempotencyKey: "plugin-create", ActorID: "user", Type: agoprotocol.CommandThreadCreate}, agothreadstore.ThreadSpec{Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	plugins := &daemonPluginCommands{snapshot: agopluginhost.Snapshot{Generation: 3, Registrations: []agopluginprotocol.PluginRegistration{{PluginID: "acme", Commands: []agopluginprotocol.CommandRegistration{{ID: "run", Title: "Run"}}}}}}
	server := httptest.NewServer(NewWithRuntime(store, agocoordinator.New(store, &daemonExecutor{started: make(chan agocoordinator.TurnRequest), finish: make(chan error)}), nil, plugins).Handler())
	t.Cleanup(server.Close)
	listed := requestJSON(t, http.MethodGet, server.URL+"/v1/threads/"+created.ThreadID+"/plugins", nil)
	if listed.StatusCode != http.StatusOK {
		t.Fatalf("plugin list status = %d", listed.StatusCode)
	}
	_ = listed.Body.Close()
	executed := requestJSON(t, http.MethodPost, server.URL+"/v1/threads/"+created.ThreadID+"/plugin-commands/acme:run", map[string]any{"turn_id": "turn-1", "input": map[string]any{"value": 7}})
	if executed.StatusCode != http.StatusOK || plugins.threadID != created.ThreadID || plugins.turnID != "turn-1" || plugins.commandID != "acme:run" {
		t.Fatalf("plugin command status=%d service=%#v", executed.StatusCode, plugins)
	}
	_ = executed.Body.Close()
}

type daemonPluginCommands struct {
	snapshot                    agopluginhost.Snapshot
	threadID, turnID, commandID string
}

func (plugins *daemonPluginCommands) PluginRegistrations(context.Context, string) (agopluginhost.Snapshot, error) {
	return plugins.snapshot, nil
}
func (plugins *daemonPluginCommands) ExecutePluginCommand(_ context.Context, threadID, turnID, commandID string, _ any) (json.RawMessage, error) {
	plugins.threadID, plugins.turnID, plugins.commandID = threadID, turnID, commandID
	return json.RawMessage(`{"ok":true}`), nil
}

func submitMessage(t *testing.T, baseURL, threadID, id, text string) agothreadstore.MailboxState {
	t.Helper()
	response := requestJSON(t, http.MethodPost, baseURL+"/v1/threads/"+threadID+"/messages", map[string]any{
		"command_id": id, "idempotency_key": id, "actor_id": "user-1", "class": "normal", "content": map[string]any{"text": text},
	})
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("submit status = %d", response.StatusCode)
	}
	var state agothreadstore.MailboxState
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return state
}

func receiveDaemonRun(t *testing.T, started <-chan agocoordinator.TurnRequest) agocoordinator.TurnRequest {
	t.Helper()
	select {
	case run := <-started:
		return run
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not dispatch turn")
		return agocoordinator.TurnRequest{}
	}
}

type daemonExecutor struct {
	started chan agocoordinator.TurnRequest
	finish  chan error
}

func (executor *daemonExecutor) Run(ctx context.Context, request agocoordinator.TurnRequest) error {
	executor.started <- request
	select {
	case err := <-executor.finish:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func requestJSON(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	var encoded bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&encoded).Encode(body); err != nil {
			t.Fatalf("encode request: %v", err)
		}
	}
	request, err := http.NewRequest(method, url, &encoded)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return response
}
