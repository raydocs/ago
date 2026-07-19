package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"claudexflow/internal/agodaemon"
	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
)

func TestCreateRequest(t *testing.T) {
	var got map[string]any
	socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/threads" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"thread_id":"t1"}`))
	}))
	defer stop()
	var out, stderr bytes.Buffer
	err := runClient(context.Background(), []string{"create", "--socket", socket, "--title", "T", "--workspace", "/w", "--project", "project-1", "--content", "Build it"}, &out, &stderr)
	if err != nil {
		t.Fatalf("runClient: %v stderr=%s", err, stderr.String())
	}
	spec := got["spec"].(map[string]any)
	executor := spec["executor"].(map[string]any)
	if spec["title"] != "T" || spec["workspace"] != "/w" || spec["mode"] != "medium" || executor["type"] != "local" {
		t.Fatalf("spec = %#v", spec)
	}
	if got["command_id"] == "" || got["command_id"] != got["idempotency_key"] {
		t.Fatalf("missing generated IDs: %#v", got)
	}
	if got["initial_message"].(map[string]any)["text"] != "Build it" || got["project"].(map[string]any)["project_id"] != "project-1" {
		t.Fatalf("atomic identity/message missing: %#v", got)
	}
	agent := got["agent"].(map[string]any)
	if agent["definition_id"] != "ago.default" || agent["default_mode"] != "medium" {
		t.Fatalf("immutable agent snapshot missing: %#v", agent)
	}
}

func TestListCLIUsesProjectScopedCatalogQuery(t *testing.T) {
	socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/threads" || r.URL.Query().Get("project_id") != "project one" || r.URL.Query().Get("search") != "needle" || r.URL.Query().Get("archive") != "all" || r.URL.Query().Get("limit") != "25" {
			t.Fatalf("request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"schema_version":1,"threads":[]}`))
	}))
	defer stop()
	var out, stderr bytes.Buffer
	if err := runClient(context.Background(), []string{"list", "--socket", socket, "--project", "project one", "--search", "needle", "--archive-filter", "all", "--limit", "25"}, &out, &stderr); err != nil {
		t.Fatalf("list: %v stderr=%s", err, stderr.String())
	}
}

func TestArchiveCLIRequiresSequenceFence(t *testing.T) {
	var got map[string]any
	socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/threads/thread-1/archive" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"thread_id":"thread-1","last_sequence":8}`))
	}))
	defer stop()
	var out, stderr bytes.Buffer
	if err := runClient(context.Background(), []string{"archive", "--socket", socket, "--thread", "thread-1", "--expected-sequence", "7"}, &out, &stderr); err != nil {
		t.Fatalf("archive: %v stderr=%s", err, stderr.String())
	}
	if got["expected_sequence"] != float64(7) || got["command_id"] == "" || got["command_id"] != got["idempotency_key"] {
		t.Fatalf("archive body = %#v", got)
	}
}

func TestExpectedSequenceConflictPropagates(t *testing.T) {
	socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["expected_sequence"] != float64(7) {
			t.Fatalf("body = %#v", body)
		}
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"sequence conflict"}`))
	}))
	defer stop()
	var out, stderr bytes.Buffer
	err := runClient(context.Background(), []string{"submit", "--socket", socket, "--thread", "t", "--content", "hello", "--expected-sequence", "7"}, &out, &stderr)
	if err == nil || out.Len() != 0 || !bytes.Contains(stderr.Bytes(), []byte("sequence conflict")) {
		t.Fatalf("err=%v out=%q stderr=%q", err, out.String(), stderr.String())
	}
}

func TestSubmitUploadsAttachmentsThenSendsCanonicalMessageReferences(t *testing.T) {
	attachmentPath := filepath.Join(t.TempDir(), "note.txt")
	content := []byte("attachment body")
	if err := os.WriteFile(attachmentPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	var uploaded agoprotocol.AttachmentRef
	var submitted agoprotocol.MessageInput
	calls := 0
	socket, stop := testUnixServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		switch calls {
		case 1:
			if request.Method != http.MethodPost || request.URL.Path != "/v1/threads/thread-1/attachments" {
				t.Fatalf("upload request = %s %s", request.Method, request.URL.Path)
			}
			if err := json.Unmarshal([]byte(request.Header.Get("X-Ago-Attachment-Ref")), &uploaded); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(request.Body)
			if err != nil || !bytes.Equal(body, content) {
				t.Fatalf("upload body = %q, %v", body, err)
			}
			_ = json.NewEncoder(writer).Encode(uploaded)
		case 2:
			if request.Method != http.MethodPost || request.URL.Path != "/v1/threads/thread-1/messages" {
				t.Fatalf("submit request = %s %s", request.Method, request.URL.Path)
			}
			var envelope struct {
				Content json.RawMessage `json:"content"`
			}
			if err := json.NewDecoder(request.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(envelope.Content, &submitted); err != nil {
				t.Fatal(err)
			}
			_, _ = writer.Write([]byte(`{"thread_id":"thread-1"}`))
		default:
			t.Fatalf("unexpected request %d", calls)
		}
	}))
	defer stop()
	var out, stderr bytes.Buffer
	err := runClient(context.Background(), []string{"submit", "--socket", socket, "--thread", "thread-1", "--content", "review", "--attachment", attachmentPath, "--file-mention", "src/main.go", "--idempotency-key", "retry-1"}, &out, &stderr)
	if err != nil {
		t.Fatalf("submit: %v stderr=%s", err, stderr.String())
	}
	digest := sha256.Sum256(content)
	if uploaded.AttachmentID == "" || uploaded.SHA256 != fmt.Sprintf("%x", digest) || uploaded.SizeBytes != uint64(len(content)) || uploaded.MediaType != "text/plain" || uploaded.Filename != "note.txt" {
		t.Fatalf("uploaded ref = %#v", uploaded)
	}
	if submitted.Text != "review" || len(submitted.Attachments) != 1 || submitted.Attachments[0] != uploaded || len(submitted.FileMentions) != 1 || submitted.FileMentions[0].Path != "src/main.go" {
		t.Fatalf("submitted message = %#v", submitted)
	}
	encodedSubmitted, _ := json.Marshal(submitted)
	if bytes.Contains(encodedSubmitted, []byte(attachmentPath)) || bytes.Contains(encodedSubmitted, []byte(`"workspace"`)) {
		t.Fatalf("submitted message leaked local path/workspace: %s", encodedSubmitted)
	}
}

func TestSubmitRejectsOversizedAttachmentBeforeNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(int64(agoprotocol.MaxAttachmentBytes) + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	var calls atomic.Int32
	socket, stop := testUnixServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer stop()
	var out, stderr bytes.Buffer
	err = runClient(context.Background(), []string{"submit", "--socket", socket, "--thread", "thread-1", "--attachment", path}, &out, &stderr)
	if err == nil || calls.Load() != 0 {
		t.Fatalf("oversized attachment err=%v calls=%d", err, calls.Load())
	}
}

func TestSubmitRejectsSymlinkAttachmentBeforeNetwork(t *testing.T) {
	realPath := filepath.Join(t.TempDir(), "real.txt")
	if err := os.WriteFile(realPath, []byte("real"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(t.TempDir(), "link.txt")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	socket, stop := testUnixServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer stop()
	var out, stderr bytes.Buffer
	err := runClient(context.Background(), []string{"submit", "--socket", socket, "--thread", "thread-1", "--attachment", linkPath}, &out, &stderr)
	if err == nil || calls.Load() != 0 {
		t.Fatalf("symlink attachment err=%v calls=%d", err, calls.Load())
	}
}

func TestSubmitDoesNotSendMessageWhenAttachmentUploadFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	socket, stop := testUnixServer(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls.Add(1) != 1 || !strings.HasSuffix(request.URL.Path, "/attachments") {
			t.Fatalf("unexpected request %s", request.URL.Path)
		}
		writer.WriteHeader(http.StatusConflict)
		_, _ = writer.Write([]byte(`{"error":"immutable conflict"}`))
	}))
	defer stop()
	var out, stderr bytes.Buffer
	err := runClient(context.Background(), []string{"submit", "--socket", socket, "--thread", "thread-1", "--content", "do not send", "--attachment", path}, &out, &stderr)
	if err == nil || calls.Load() != 1 || !strings.Contains(stderr.String(), "immutable conflict") {
		t.Fatalf("upload failure err=%v calls=%d stderr=%q", err, calls.Load(), stderr.String())
	}
}

func TestCreateFailsClosedForAttachmentAndFileMentionFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, extra := range [][]string{{"--attachment", path}, {"--file-mention", "README.md"}} {
		args := []string{"create", "--socket", "/tmp/unused", "--title", "T", "--workspace", "/w", "--project", "p", "--content", "start"}
		args = append(args, extra...)
		var out, stderr bytes.Buffer
		if err := runClient(context.Background(), args, &out, &stderr); err == nil {
			t.Fatalf("create accepted %v", extra)
		}
	}
}

func TestStageCLIUsesOpaqueUnitsAndSnapshotFences(t *testing.T) {
	var got map[string]any
	socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/threads/thread-1/diff/stage" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"operation":{"state":"completed"}}`))
	}))
	defer stop()
	var out, stderr bytes.Buffer
	err := runClient(context.Background(), []string{"stage", "--socket", socket, "--thread", "thread-1", "--expected-sequence", "7", "--snapshot-revision", "3", "--snapshot-digest", strings.Repeat("a", 64), "--units", "unit-1,unit-2"}, &out, &stderr)
	if err != nil {
		t.Fatalf("stage: %v stderr=%s", err, stderr.String())
	}
	if got["expected_sequence"] != float64(7) || got["expected_snapshot_revision"] != float64(3) || got["expected_snapshot_digest"] != strings.Repeat("a", 64) {
		t.Fatalf("stage fences = %#v", got)
	}
	units := got["selected_unit_ids"].([]any)
	if len(units) != 2 || units[0] != "unit-1" || got["command_id"] != got["idempotency_key"] || !strings.HasPrefix(got["command_id"].(string), "git:") {
		t.Fatalf("stage identity = %#v", got)
	}
	for _, forbidden := range []string{"path", "patch", "workspace", "executor_generation"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("stage body exposed %s: %#v", forbidden, got)
		}
	}
}

func TestRevertCLIUsesOnlyReceiptAndSnapshotFences(t *testing.T) {
	var got map[string]any
	socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/threads/thread-1/diff/revert" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"operation":{"state":"completed"}}`))
	}))
	defer stop()
	var out, stderr bytes.Buffer
	err := runClient(context.Background(), []string{"revert", "--socket", socket, "--thread", "thread-1", "--expected-sequence", "7", "--snapshot-revision", "3", "--snapshot-digest", strings.Repeat("a", 64), "--receipt", "R-one"}, &out, &stderr)
	if err != nil {
		t.Fatalf("revert: %v stderr=%s", err, stderr.String())
	}
	if got["receipt_id"] != "R-one" || got["expected_sequence"] != float64(7) || got["command_id"] != got["idempotency_key"] {
		t.Fatalf("revert body = %#v", got)
	}
	for _, forbidden := range []string{"path", "patch", "workspace", "selected_unit_ids"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("revert body exposed %s: %#v", forbidden, got)
		}
	}
}

func TestCommentCLIUsesOpaqueSnapshotAnchors(t *testing.T) {
	var got map[string]any
	socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/threads/thread-1/diff/comments" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"comment_id":"comment-1"}`))
	}))
	defer stop()
	var out, stderr bytes.Buffer
	err := runClient(context.Background(), []string{"comment", "--socket", socket, "--thread", "thread-1", "--expected-sequence", "9", "--snapshot-revision", "3", "--snapshot-digest", strings.Repeat("a", 64), "--file-id", "file:opaque", "--hunk-id", "hunk:opaque", "--content", "Change this section"}, &out, &stderr)
	if err != nil {
		t.Fatalf("comment: %v stderr=%s", err, stderr.String())
	}
	if got["expected_sequence"] != float64(9) || got["snapshot_revision"] != float64(3) || got["snapshot_digest"] != strings.Repeat("a", 64) || got["file_id"] != "file:opaque" || got["hunk_id"] != "hunk:opaque" || got["body"] != "Change this section" || got["actor_id"] != "ago-cli" {
		t.Fatalf("comment body = %#v", got)
	}
	for _, forbidden := range []string{"path", "patch", "workspace"} {
		if _, ok := got[forbidden]; ok {
			t.Fatalf("comment body exposed %s: %#v", forbidden, got)
		}
	}
}

func TestOpenUsesAuthoritativeProjectionCursorAndLimit(t *testing.T) {
	socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/threads/thread-1/projection" || r.URL.Query().Get("after") != "9" || r.URL.Query().Get("limit") != "50" {
			t.Fatalf("request = %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"schema_version":1,"events":[],"dialogs":[]}`))
	}))
	defer stop()
	var out, stderr bytes.Buffer
	if err := runClient(context.Background(), []string{"open", "--socket", socket, "--thread", "thread-1", "--after", "9", "--limit", "50"}, &out, &stderr); err != nil {
		t.Fatalf("open projection: %v stderr=%s", err, stderr.String())
	}
}

func TestConformanceUsesProjectionPaginationAndPrintsCanonicalDigests(t *testing.T) {
	var calls atomic.Int32
	socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.EscapedPath() != "/v1/threads/thread%2Fone/projection" || r.URL.Query().Get("limit") != "200" {
			t.Fatalf("request = %s %s?%s", r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
		}
		switch calls.Add(1) {
		case 1:
			if r.URL.Query().Get("after") != "0" {
				t.Fatalf("first cursor = %q", r.URL.Query().Get("after"))
			}
			_, _ = w.Write([]byte(conformanceProjectionPage(0, 1, true, `[{"schema_version":1,"event_id":"event-1","thread_id":"thread/one","sequence":1,"type":"message.accepted","visibility":"shared","payload":{"content":{"text":"hello"}}}]`)))
		case 2:
			if r.URL.Query().Get("after") != "1" {
				t.Fatalf("second cursor = %q", r.URL.Query().Get("after"))
			}
			_, _ = w.Write([]byte(conformanceProjectionPage(1, 2, false, `[{"schema_version":1,"event_id":"event-2","thread_id":"thread/one","sequence":2,"type":"provider.usage-recorded","visibility":"shared","payload":{"input_tokens":4}}]`)))
		default:
			t.Fatal("unexpected projection request")
		}
	}))
	defer stop()

	var out, stderr bytes.Buffer
	if err := runClient(context.Background(), []string{"conformance", "--socket", socket, "--thread", "thread/one"}, &out, &stderr); err != nil {
		t.Fatalf("conformance: %v stderr=%s", err, stderr.String())
	}
	const expected = `{"dialogs":{"count":1,"digest":"25e352de99223b5697e334f60479c1b7162d2bebc176ddfe2fc7a0b8986fcdea"},"diff":{"comment_count":0,"digest":"0a1d0e25af44dd164b83f8e5a044bec5bc183d02565de8cbd98651f5faf85b89","has_snapshot":false},"digest":"228ef58de50caaa74cd104469d94e1d23fa48bf0ae9dfe94ce741926d89eca64","events":{"count":2,"digest":"2591a11ef686d9eb4a779fcec1b26fb7d31c95f2baac7e5bcd136a5286ffe920","first_sequence":1,"last_sequence":2},"mailbox":{"activity":"idle","cancel_requested":false,"digest":"ac38697c4eb90a389331e3b8d918db795f96abeb7524fc40aa7c25fa01cecf11","last_sequence":2,"thread_id":"thread/one"},"queue":{"count":1,"digest":"9ad80307b2fd50738ea27d2af044beede9c7df2a646b1c5890cea42f866e2155"},"snapshot_sequence":2}` + "\n"
	if out.String() != expected {
		t.Fatalf("output mismatch\n got: %s\nwant: %s", out.String(), expected)
	}
	if calls.Load() != 2 {
		t.Fatalf("projection requests = %d", calls.Load())
	}
}

func TestConformanceRejectsMalformedProjectionWithoutOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string) string
	}{
		{name: "snapshot contradiction", mutate: func(page string) string {
			return strings.Replace(page, `"snapshot_sequence":2`, `"snapshot_sequence":99`, 1)
		}},
		{name: "unknown field", mutate: func(page string) string {
			return strings.Replace(page, `"schema_version":1`, `"unknown":true,"schema_version":1`, 1)
		}},
		{name: "wrong collection type", mutate: func(page string) string { return strings.Replace(page, `"comments":[]`, `"comments":{}`, 1) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			page := test.mutate(conformanceProjectionPage(0, 2, false, `[{"schema_version":1,"event_id":"event-1","thread_id":"thread/one","sequence":1,"type":"message.accepted","visibility":"shared"},{"schema_version":1,"event_id":"event-2","thread_id":"thread/one","sequence":2,"type":"done","visibility":"shared"}]`))
			socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(page)) }))
			defer stop()
			var out, stderr bytes.Buffer
			err := runClient(context.Background(), []string{"conformance", "--socket", socket, "--thread", "thread/one"}, &out, &stderr)
			if err == nil || out.Len() != 0 {
				t.Fatalf("err=%v output=%q", err, out.String())
			}
		})
	}
}

func TestActualCLIWebAndAppleConformanceAgreeOnOneDaemon(t *testing.T) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		t.Skip("bun is not installed")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	swift, err := exec.LookPath("swift")
	if err != nil {
		t.Skip("swift is not installed")
	}
	applePackage := filepath.Join(root, "ago-clients", "apple")
	binPath, err := exec.Command(swift, "build", "--package-path", applePackage, "--show-bin-path").Output()
	if err != nil {
		t.Skipf("cannot resolve AgoDesktop build directory: %v", err)
	}
	apple := filepath.Join(strings.TrimSpace(string(binPath)), "AgoDesktop")
	if info, err := os.Stat(apple); err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		t.Skip("AgoDesktop debug executable is not built")
	}

	store, err := agothreadstore.Open(filepath.Join(t.TempDir(), "ago.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	created, err := store.CreateAtomicThread(context.Background(), agoprotocol.Command{
		SchemaVersion: agoprotocol.SchemaVersion, CommandID: "actual-client-conformance", IdempotencyKey: "actual-client-conformance",
		ActorID: "conformance", Type: agoprotocol.CommandThreadCreate,
	}, agothreadstore.AtomicCreateInput{
		Spec:           agothreadstore.ThreadSpec{Workspace: t.TempDir(), Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}},
		Project:        agothreadstore.ProjectIdentity{ProjectID: "conformance-project"},
		Agent:          agothreadstore.AgentDefinitionSnapshot{DefinitionID: "conformance-agent", Version: "1", DisplayName: "Conformance", SystemInstructionsDigest: "sha256:conformance", DefaultMode: agoprotocol.AgentModeMedium},
		InitialMessage: json.RawMessage(`{"text":"compare actual clients"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := agodaemon.New(store, nil).Handler()
	socket, stopUnix := testUnixServer(t, handler)
	defer stopUnix()
	tcp := httptest.NewServer(handler)
	defer tcp.Close()

	var cli, stderr bytes.Buffer
	if err := runClient(context.Background(), []string{"conformance", "--socket", socket, "--thread", created.ThreadID}, &cli, &stderr); err != nil {
		t.Fatalf("CLI conformance: %v stderr=%s", err, stderr.String())
	}
	projectionURL := tcp.URL + "/v1/threads/" + created.ThreadID + "/projection"
	webCommand := exec.Command(bun, filepath.Join(root, "ago-clients", "web", "src", "conformance.ts"), projectionURL)
	web, err := webCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("Web conformance: %v: %s", err, web)
	}
	appleCommand := exec.Command(apple, "--conformance", projectionURL)
	appleOutput, err := appleCommand.CombinedOutput()
	if err != nil {
		t.Fatalf("Apple conformance: %v: %s", err, appleOutput)
	}
	if cli.String() != string(web) || cli.String() != string(appleOutput) {
		t.Fatalf("actual client conformance diverged\nCLI:   %s\nWeb:   %s\nApple: %s", cli.String(), web, appleOutput)
	}
}

func conformanceProjectionPage(requested, next int, hasMore bool, events string) string {
	return fmt.Sprintf(`{"schema_version":1,"thread":{"thread_id":"thread/one","last_sequence":2,"title":"Thread","workspace":"/x","mode":"default","executor":{"type":"local"},"project":{"project_id":"project"},"agent":{"definition_id":"agent","version":"1","display_name":"Agent","default_mode":"default"}},"mailbox":{"thread_id":"thread/one","last_sequence":2,"activity":"idle","cancel_requested":false,"queue":[{"queue_item_id":"queue-1","position":0,"class":"normal","state":"pending","content":{"text":"next"}}]},"events":%s,"dialogs":[{"dialog_id":"dialog-1","thread_id":"thread/one","turn_id":"turn-1","plugin_id":"plugin","generation":1,"invocation_id":"invocation","deadline":"2026-07-19T00:00:00Z","request_type":"confirm","request":{"prompt":"Continue?"},"state":"pending","revision":1,"requested_sequence":2}],"diff":{"snapshot":null,"comments":[]},"requested_after_sequence":%d,"next_after_sequence":%d,"snapshot_sequence":2,"has_more":%t,"plugins":{"available":false,"generation":0,"registrations":[]},"executor":{"target":{"type":"local"},"activity":"idle"}}`, events, requested, next, hasMore)
}

func TestPluginCommandCLIUsesCanonicalThreadRouteAndTypedJSONInput(t *testing.T) {
	var got map[string]any
	socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/threads/thread-1/plugin-commands/acme:run" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"result":{"ok":true}}`))
	}))
	defer stop()
	var out, stderr bytes.Buffer
	if err := runClient(context.Background(), []string{"plugin-command", "--socket", socket, "--thread", "thread-1", "--turn", "turn-1", "--command", "acme:run", "--input", `{"count":2}`}, &out, &stderr); err != nil {
		t.Fatalf("plugin command: %v stderr=%s", err, stderr.String())
	}
	input, ok := got["input"].(map[string]any)
	if got["turn_id"] != "turn-1" || !ok || input["count"] != float64(2) {
		t.Fatalf("plugin command body = %#v", got)
	}
}

func TestDialogCLIListsAndResolvesWithRevisionAndSequenceFences(t *testing.T) {
	var got map[string]any
	calls := 0
	socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			if r.Method != http.MethodGet || r.URL.Path != "/v1/threads/thread-1/dialogs" {
				t.Fatalf("list request = %s %s", r.Method, r.URL.Path)
			}
			_, _ = w.Write([]byte(`{"dialogs":[]}`))
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/threads/thread-1/dialogs/dialog-1/resolve" {
			t.Fatalf("resolve request = %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"dialog_id":"dialog-1","state":"resolved"}`))
	}))
	defer stop()
	var out, stderr bytes.Buffer
	if err := runClient(context.Background(), []string{"dialogs", "--socket", socket, "--thread", "thread-1"}, &out, &stderr); err != nil {
		t.Fatalf("dialogs: %v stderr=%s", err, stderr.String())
	}
	out.Reset()
	if err := runClient(context.Background(), []string{"resolve-dialog", "--socket", socket, "--thread", "thread-1", "--dialog", "dialog-1", "--resolver", "cli", "--expected-revision", "2", "--expected-sequence", "9", "--response", `{"status":"ok","value":true}`}, &out, &stderr); err != nil {
		t.Fatalf("resolve dialog: %v stderr=%s", err, stderr.String())
	}
	response, ok := got["response"].(map[string]any)
	if got["resolver_id"] != "cli" || got["expected_revision"] != float64(2) || got["expected_sequence"] != float64(9) || !ok || response["status"] != "ok" || response["value"] != true {
		t.Fatalf("resolve dialog body = %#v", got)
	}
}

func TestWatchReconnectCursorNoDuplicate(t *testing.T) {
	var calls atomic.Int32
	socket, stop := testUnixServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.URL.Path != "/v1/threads/t/projection" || r.URL.Query().Get("limit") != "200" {
			t.Fatalf("watch path = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		after, _ := strconv.Atoi(r.URL.Query().Get("after"))
		if n == 1 && after != 0 {
			t.Fatalf("first after=%d", after)
		}
		if n > 1 && after != 1 {
			t.Fatalf("reconnect after=%d", after)
		}
		if n == 1 {
			_, _ = w.Write([]byte(`{"events":[{"sequence":1,"type":"delta"}],"next_after_sequence":1,"has_more":false}`))
			return
		}
		_, _ = w.Write([]byte(`{"events":[{"sequence":2,"type":"done"}],"next_after_sequence":2,"has_more":false}`))
	}))
	defer stop()
	var out, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := runClient(ctx, []string{"watch", "--socket", socket, "--thread", "t", "--until", "done", "--poll", "1ms"}, &out, &stderr); err != nil {
		t.Fatalf("watch: %v stderr=%s", err, stderr.String())
	}
	s := bufio.NewScanner(&out)
	count := 0
	for s.Scan() {
		count++
	}
	if count != 2 {
		t.Fatalf("lines=%d output=%q", count, out.String())
	}
}

func TestDispatchBackwardCompatibility(t *testing.T) {
	for _, args := range [][]string{nil, {"--socket", "/tmp/x"}, {"daemon", "--socket", "/tmp/x"}} {
		mode, rest := dispatch(args)
		if mode != "daemon" {
			t.Fatalf("dispatch(%q) = %q %q", args, mode, rest)
		}
	}
	mode, _ := dispatch([]string{"list"})
	if mode != "client" {
		t.Fatalf("list mode=%q", mode)
	}
}

func testUnixServer(t *testing.T, handler http.Handler) (string, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ago-client-test-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	s := &http.Server{Handler: handler}
	go func() { _ = s.Serve(l) }()
	return socket, func() { _ = s.Close(); _ = l.Close(); _ = os.RemoveAll(dir) }
}
