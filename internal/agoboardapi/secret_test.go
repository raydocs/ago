package agoboardapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claudexflow/internal/agoartifact"
	"claudexflow/internal/agoboardapi"
	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agofake"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agoredact"
	"claudexflow/internal/agoscheduler"
)

// secretSentinel is planted everywhere an executor could leak it. It must not
// survive into any durable or user-visible surface.
const secretSentinel = "sk-ant-SENTINEL-do-not-persist-0123456789"

// leakyExecutor is a hostile executor: it puts the sentinel into every field it
// controls, including the artifact it writes.
type leakyExecutor struct {
	artifacts *agoartifact.Store
}

func (e leakyExecutor) Execute(ctx context.Context, dispatch agoboardruntime.Dispatch) (agoboardruntime.ExecutionResult, error) {
	descriptor, err := e.artifacts.Put(ctx, agoartifact.PutInput{
		Type: "text/plain", DisplayName: "output-" + secretSentinel + ".log",
	}, agoredact.New(secretSentinel).Reader(strings.NewReader(
		"运行日志\nAuthorization: Bearer "+secretSentinel+"\napi_key="+secretSentinel+"\n")))
	if err != nil {
		return agoboardruntime.ExecutionResult{}, err
	}
	return agoboardruntime.ExecutionResult{
		Artifact: "artifact://leak/" + secretSentinel,
		Summary:  "任务完成，密钥是 " + secretSentinel,
		Result: agoboardprotocol.EvidenceResult{
			Summary:  "任务完成，密钥是 " + secretSentinel,
			Warnings: []string{"警告：api_key=" + secretSentinel},
			ChangedFiles: []agoboardprotocol.ChangedFile{{
				Path: "README.md", BeforeHash: "aaa", AfterHash: "bbb",
			}},
			Commands: []agoboardprotocol.CommandRecord{{
				Display:  "curl -H 'Authorization: Bearer " + secretSentinel + "' https://example.com",
				ExitCode: 0, DurationMS: 10, OutputArtifactID: descriptor.ID,
			}},
			Tests: []agoboardprotocol.TestRecord{{
				Name: "验收测试", Command: "go test -token=" + secretSentinel,
				Passed: true, ExitCode: 0, Required: true,
			}},
			Artifacts: []agoboardprotocol.ArtifactRef{{
				ID: descriptor.ID, Type: descriptor.Type, DisplayName: descriptor.DisplayName,
				Bytes: descriptor.Bytes, SHA256: descriptor.SHA256,
			}},
		},
	}, nil
}

type leakyVerifier struct{}

func (leakyVerifier) Verify(context.Context, agoboardruntime.Dispatch, agoboardruntime.ExecutionResult) (agoboardruntime.Review, error) {
	return agoboardruntime.Review{Accepted: true, Reason: "已核对密钥 " + secretSentinel}, nil
}

// A sentinel planted by a hostile executor must not reach SQLite bytes,
// artifact files, API JSON, the SSE stream, or task detail.
func TestSecretSentinelNeverReachesAnyDurableOrVisibleSurface(t *testing.T) {
	t.Setenv("AGO_PROVIDER_API_KEY", secretSentinel)
	base := t.TempDir()
	dbPath := filepath.Join(base, "board.db")
	store, err := agoboardstore.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	artifacts, err := agoartifact.Open(agoartifact.Options{Root: filepath.Join(base, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) }
	runtime := agoboardruntime.New(store, agoplanner.DemoPlanner{}, agoboardruntime.Options{
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: clock,
	})
	server, err := agoboardapi.New(agoboardapi.Options{
		Runtime: runtime, Store: store, Artifacts: artifacts, PollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime,
		Executor: leakyExecutor{artifacts: artifacts}, Verifier: leakyVerifier{},
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: clock,
		Redactor: agoredact.New(secretSentinel),
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	created := doRequest(t, handler, http.MethodPost, "/api/v1/goals", goalBody("cmd-secret-chain", chineseDemoObjective, t.TempDir()))
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var response goalCreateResponse
	decodeInto(t, created, &response)
	boardID := response.Board.BoardID
	driveWithScheduler(t, store, scheduler, boardID)

	// 1. SQLite bytes, read raw so no decoding can hide the sentinel.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		content, err := os.ReadFile(dbPath + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), secretSentinel) {
			t.Fatalf("the sentinel reached the SQLite file %q", dbPath+suffix)
		}
	}

	// 2. Artifact files on disk.
	root := artifacts.Root()
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if strings.Contains(string(content), secretSentinel) {
			t.Fatalf("the sentinel reached artifact bytes at %s", path)
		}
		if strings.Contains(info.Name(), secretSentinel) {
			t.Fatalf("the sentinel reached an artifact file name: %s", info.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// 3. Board snapshot JSON.
	snapshot := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+boardID, nil)
	assertNoSentinel(t, "board snapshot", snapshot.Body.String())

	// 4. Task detail for every task, including evidence and artifacts.
	board, err := store.Board(context.Background(), boardID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range board.Tasks {
		detail := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+boardID+"/tasks/"+task.ID, nil)
		assertNoSentinel(t, "task detail "+task.ID, detail.Body.String())
		if strings.Contains(detail.Body.String(), "fencing_token") {
			t.Fatalf("task detail %s exposed a fencing token field", task.ID)
		}
	}

	// 5. Providers.
	assertNoSentinel(t, "providers", doRequest(t, handler, http.MethodGet, "/api/v1/providers", nil).Body.String())

	// 6. SSE payload.
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/v1/boards/"+boardID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	streamResponse, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer streamResponse.Body.Close()
	stream := make([]byte, 0, 128*1024)
	buffer := make([]byte, 8192)
	for len(stream) < 256*1024 {
		n, readErr := streamResponse.Body.Read(buffer)
		stream = append(stream, buffer[:n]...)
		if readErr != nil || n == 0 {
			break
		}
		if strings.Contains(string(stream), "evidence.accepted") {
			break
		}
	}
	assertNoSentinel(t, "sse stream", string(stream))

	// 7. Artifact download, including its headers.
	var artifactID string
	for _, evidence := range board.Evidence {
		for _, reference := range evidence.Result.Artifacts {
			artifactID = reference.ID
		}
	}
	if artifactID == "" {
		t.Fatal("no artifact was produced, so the download path would not be exercised")
	}
	download := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+boardID+"/artifacts/"+artifactID, nil)
	if download.Code != http.StatusOK {
		t.Fatalf("artifact download status = %d, body = %s", download.Code, download.Body.String())
	}
	assertNoSentinel(t, "artifact download body", download.Body.String())
	for key, values := range download.Header() {
		for _, value := range values {
			if strings.Contains(value, secretSentinel) {
				t.Fatalf("artifact download header %q leaked the sentinel", key)
			}
		}
	}
	if !strings.Contains(download.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("artifact download is not served as an attachment: %q", download.Header().Get("Content-Disposition"))
	}
}

// An artifact this board does not reference must not be readable through it.
func TestArtifactDownloadRequiresABoardReference(t *testing.T) {
	base := t.TempDir()
	store, err := agoboardstore.Open(filepath.Join(base, "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	artifacts, err := agoartifact.Open(agoartifact.Options{Root: filepath.Join(base, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	runtime := agoboardruntime.New(store, agoplanner.DemoPlanner{}, agoboardruntime.Options{
		CoordinatorID: "c", WorkerID: "w", VerifierID: "v", LeaseDuration: time.Minute,
		Now: func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) },
	})
	server, err := agoboardapi.New(agoboardapi.Options{Runtime: runtime, Store: store, Artifacts: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	created := doRequest(t, handler, http.MethodPost, "/api/v1/goals", goalBody("cmd-unreferenced", chineseDemoObjective, t.TempDir()))
	var response goalCreateResponse
	decodeInto(t, created, &response)

	// An artifact exists in the managed store but no evidence references it.
	descriptor, err := artifacts.Put(context.Background(), agoartifact.PutInput{DisplayName: "orphan.log"}, strings.NewReader("私密内容"))
	if err != nil {
		t.Fatal(err)
	}
	unreferenced := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+response.Board.BoardID+"/artifacts/"+descriptor.ID, nil)
	if unreferenced.Code != http.StatusNotFound {
		t.Fatalf("unreferenced artifact status = %d, want 404", unreferenced.Code)
	}
	if strings.Contains(unreferenced.Body.String(), "私密内容") {
		t.Fatal("an unreferenced artifact's content was served")
	}
	if strings.Contains(unreferenced.Body.String(), artifacts.Root()) {
		t.Fatal("the 404 leaked a local artifact path")
	}

	// A malformed identifier must never yield artifact bytes. A traversal
	// attempt is path-cleaned by the router into a different route before the
	// handler sees it, which is why the acceptable outcomes are a redirect or a
	// not-found — never a served artifact.
	for _, id := range []string{"..", "../../etc/passwd", "not-hex", strings.Repeat("z", 64)} {
		attempt := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+response.Board.BoardID+"/artifacts/"+id, nil)
		switch {
		case attempt.Code == http.StatusNotFound:
		case attempt.Code >= 300 && attempt.Code < 400:
			// Path cleaning redirected it away from the artifact route.
			if strings.Contains(attempt.Header().Get("Location"), "/artifacts/") {
				t.Fatalf("artifact id %q redirected back into the artifact route: %q", id, attempt.Header().Get("Location"))
			}
		default:
			t.Fatalf("artifact id %q status = %d, want a refusal", id, attempt.Code)
		}
		if strings.Contains(attempt.Body.String(), "私密内容") {
			t.Fatalf("artifact id %q served content", id)
		}
	}
}

func assertNoSentinel(t *testing.T, surface, content string) {
	t.Helper()
	if strings.Contains(content, secretSentinel) {
		excerpt := content
		if len(excerpt) > 800 {
			excerpt = excerpt[:800]
		}
		t.Fatalf("the sentinel reached %s: %s", surface, excerpt)
	}
}

// Task detail must carry the structured evidence, otherwise everything D6
// records is invisible to the person meant to inspect it.
func TestTaskDetailExposesStructuredEvidence(t *testing.T) {
	base := t.TempDir()
	store, err := agoboardstore.Open(filepath.Join(base, "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	artifacts, err := agoartifact.Open(agoartifact.Options{Root: filepath.Join(base, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) }
	runtime := agoboardruntime.New(store, agoplanner.DemoPlanner{}, agoboardruntime.Options{
		CoordinatorID: "c", WorkerID: "w", VerifierID: "v", LeaseDuration: time.Minute, Now: clock,
	})
	server, err := agoboardapi.New(agoboardapi.Options{Runtime: runtime, Store: store, Artifacts: artifacts})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := agofake.New(agofake.Script{Default: agofake.OutcomeSuccess})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime,
		Executor: provider.WithArtifacts(artifacts), Verifier: provider,
		CoordinatorID: "c", WorkerID: "w", VerifierID: "v", LeaseDuration: time.Minute, Now: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()
	created := doRequest(t, handler, http.MethodPost, "/api/v1/goals", goalBody("cmd-structured", chineseDemoObjective, t.TempDir()))
	var response goalCreateResponse
	decodeInto(t, created, &response)
	boardID := response.Board.BoardID
	driveWithScheduler(t, store, scheduler, boardID)

	board, err := store.Board(context.Background(), boardID)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, task := range board.Tasks {
		if task.State != agoboardprotocol.TaskPassed {
			continue
		}
		detail := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+boardID+"/tasks/"+task.ID, nil)
		var body struct {
			Evidence []struct {
				Verdict string `json:"verdict"`
				Result  struct {
					Tests []struct {
						Name     string `json:"name"`
						Passed   bool   `json:"passed"`
						Required bool   `json:"required"`
					} `json:"tests"`
					ChangedFiles []struct {
						Path      string `json:"path"`
						AfterHash string `json:"after_hash"`
					} `json:"changed_files"`
					Commands []struct {
						Display string `json:"display"`
					} `json:"commands"`
					Artifacts []struct {
						ID     string `json:"id"`
						Bytes  int64  `json:"bytes"`
						SHA256 string `json:"sha256"`
					} `json:"artifacts"`
				} `json:"result"`
			} `json:"evidence"`
		}
		decodeInto(t, detail, &body)
		if len(body.Evidence) == 0 {
			t.Fatalf("task %q passed with no evidence in its detail", task.ID)
		}
		evidence := body.Evidence[0]
		if evidence.Verdict != "accept" {
			t.Fatalf("task %q verdict = %q, want accept", task.ID, evidence.Verdict)
		}
		if len(evidence.Result.Tests) == 0 || !evidence.Result.Tests[0].Required || !evidence.Result.Tests[0].Passed {
			t.Fatalf("task %q tests = %#v", task.ID, evidence.Result.Tests)
		}
		if len(evidence.Result.ChangedFiles) == 0 || evidence.Result.ChangedFiles[0].AfterHash == "" {
			t.Fatalf("task %q changed files = %#v", task.ID, evidence.Result.ChangedFiles)
		}
		if len(evidence.Result.Commands) == 0 {
			t.Fatalf("task %q recorded no commands", task.ID)
		}
		if len(evidence.Result.Artifacts) == 0 || evidence.Result.Artifacts[0].SHA256 == "" {
			t.Fatalf("task %q artifacts = %#v", task.ID, evidence.Result.Artifacts)
		}
		// The referenced artifact must actually be downloadable.
		download := doRequest(t, handler, http.MethodGet, "/api/v1/boards/"+boardID+"/artifacts/"+evidence.Result.Artifacts[0].ID, nil)
		if download.Code != http.StatusOK || int64(download.Body.Len()) != evidence.Result.Artifacts[0].Bytes {
			t.Fatalf("artifact download = %d, %d bytes, want 200 and %d", download.Code, download.Body.Len(), evidence.Result.Artifacts[0].Bytes)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no task passed, so structured evidence was never checked")
	}
}
