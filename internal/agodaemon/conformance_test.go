package agodaemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"claudexflow/internal/agogit"
	"claudexflow/internal/agoprotocol"
	"claudexflow/internal/agothreadstore"
	"claudexflow/internal/agoverifier"
	"claudexflow/internal/agowritebroker"
)

func TestRealDaemonCrossClientGitConformance(t *testing.T) {
	workspace := t.TempDir()
	gitConformanceRun(t, workspace, "init", "-q")
	gitConformanceRun(t, workspace, "config", "user.name", "Ago Test")
	gitConformanceRun(t, workspace, "config", "user.email", "ago@example.invalid")
	if err := os.WriteFile(filepath.Join(workspace, "change.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitConformanceRun(t, workspace, "add", "change.txt")
	gitConformanceRun(t, workspace, "commit", "-qm", "base")
	if err := os.WriteFile(filepath.Join(workspace, "change.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	databasePath := filepath.Join(t.TempDir(), "ago.db")
	store, err := agothreadstore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	created, err := store.CreateConfiguredThread(context.Background(), agoprotocol.Command{SchemaVersion: 1, CommandID: "create-conformance", IdempotencyKey: "create-conformance", ActorID: "test", Type: agoprotocol.CommandThreadCreate}, agothreadstore.ThreadSpec{Workspace: workspace, Mode: agoprotocol.AgentModeMedium, Executor: agoprotocol.ExecutorTarget{Type: agoprotocol.ExecutorLocal}})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(New(store, nil).WithGitRefresher(agogit.NewService(store)).Handler())
	t.Cleanup(httpServer.Close)
	threadURL := httpServer.URL + "/v1/threads/" + created.ThreadID

	refresh := requestJSON(t, http.MethodPost, threadURL+"/diff/refresh", map[string]any{"idempotency_key": "initial"})
	if refresh.StatusCode != http.StatusAccepted {
		t.Fatalf("refresh status = %d", refresh.StatusCode)
	}
	_ = refresh.Body.Close()
	initial := conformanceProjection(t, threadURL)
	var snapshotProjection struct {
		Unstaged []struct {
			ID string `json:"id"`
		} `json:"unstaged"`
	}
	if initial.Diff.Snapshot == nil || json.Unmarshal(initial.Diff.Snapshot.Projection, &snapshotProjection) != nil || len(snapshotProjection.Unstaged) != 1 {
		t.Fatalf("initial diff = %#v", initial.Diff)
	}
	stageBody := map[string]any{
		"command_id": "git:web-stage", "idempotency_key": "web-stage", "actor_id": "web-pwa",
		"expected_sequence": initial.SnapshotSequence, "expected_snapshot_revision": initial.Diff.Snapshot.Revision,
		"expected_snapshot_digest": initial.Diff.Snapshot.Digest, "selected_unit_ids": []string{snapshotProjection.Unstaged[0].ID},
	}
	stage := requestJSON(t, http.MethodPost, threadURL+"/diff/stage", stageBody)
	if stage.StatusCode != http.StatusOK {
		t.Fatalf("web stage status = %d", stage.StatusCode)
	}
	_ = stage.Body.Close()
	retry := requestJSON(t, http.MethodPost, threadURL+"/diff/stage", stageBody)
	if retry.StatusCode != http.StatusOK {
		t.Fatalf("lost-response exact stage retry status = %d", retry.StatusCode)
	}
	_ = retry.Body.Close()
	staleBody := map[string]any{}
	for key, value := range stageBody {
		staleBody[key] = value
	}
	staleBody["command_id"], staleBody["idempotency_key"], staleBody["actor_id"] = "git:apple-stale", "apple-stale", "ago-desktop"
	stale := requestJSON(t, http.MethodPost, threadURL+"/diff/stage", staleBody)
	if stale.StatusCode != http.StatusConflict {
		t.Fatalf("stale macOS stage status = %d, want 409", stale.StatusCode)
	}
	_ = stale.Body.Close()

	afterStage := conformanceProjection(t, threadURL)
	var stagedProjection struct {
		Staged []struct {
			ID string `json:"id"`
		} `json:"staged"`
	}
	if json.Unmarshal(afterStage.Diff.Snapshot.Projection, &stagedProjection) != nil || len(stagedProjection.Staged) != 1 {
		t.Fatalf("staged projection = %s", afterStage.Diff.Snapshot.Projection)
	}
	unstage := requestJSON(t, http.MethodPost, threadURL+"/diff/unstage", map[string]any{
		"command_id": "git:cli-unstage", "idempotency_key": "cli-unstage", "actor_id": "ago-cli",
		"expected_sequence": afterStage.SnapshotSequence, "expected_snapshot_revision": afterStage.Diff.Snapshot.Revision,
		"expected_snapshot_digest": afterStage.Diff.Snapshot.Digest, "selected_unit_ids": []string{stagedProjection.Staged[0].ID},
	})
	if unstage.StatusCode != http.StatusOK {
		t.Fatalf("CLI unstage status = %d", unstage.StatusCode)
	}
	_ = unstage.Body.Close()

	receipt, err := agowritebroker.New(store).WriteFile(context.Background(), agowritebroker.WriteFileRequest{
		ThreadID: created.ThreadID, Path: "owned.txt", Content: []byte("Ago-owned\n"), OperationID: "turn-conformance",
		ToolCallID: "write-conformance", ToolName: agowritebroker.ToolNameWriteFile, IdempotencyKey: "write-conformance",
	})
	if err != nil {
		t.Fatal(err)
	}
	check, err := agoverifier.New(store, store, agoverifier.StaticCatalog{
		"representative": {Executable: "/usr/bin/true"},
	}, conformanceVerifier{}, agoverifier.Limits{}).Run(context.Background(), agoverifier.Request{
		ThreadID: created.ThreadID, TurnID: "turn-conformance", ToolCallID: "verify-conformance",
		IdempotencyKey: "verify-conformance", CheckID: "representative",
	})
	if err != nil || check.Status != agothreadstore.VerificationPassed {
		t.Fatalf("representative verification = %#v, %v", check, err)
	}
	beforeRevert := conformanceProjection(t, threadURL)
	verificationEvents := 0
	for _, event := range beforeRevert.Events {
		if event.Type == agoprotocol.EventVerificationRecorded {
			verificationEvents++
		}
	}
	if verificationEvents != 2 {
		t.Fatalf("durable verification truth events = %d, want running and terminal", verificationEvents)
	}
	revert := requestJSON(t, http.MethodPost, threadURL+"/diff/revert", map[string]any{
		"command_id": "git:apple-revert", "idempotency_key": "apple-revert", "actor_id": "ago-desktop",
		"expected_sequence": beforeRevert.SnapshotSequence, "expected_snapshot_revision": beforeRevert.Diff.Snapshot.Revision,
		"expected_snapshot_digest": beforeRevert.Diff.Snapshot.Digest, "receipt_id": receipt.ReceiptID,
	})
	if revert.StatusCode != http.StatusOK {
		t.Fatalf("macOS revert status = %d", revert.StatusCode)
	}
	_ = revert.Body.Close()
	if _, err := os.Lstat(filepath.Join(workspace, "owned.txt")); !os.IsNotExist(err) {
		t.Fatalf("receipt revert did not restore absent image: %v", err)
	}

	// Independent CLI/Web/macOS reconnects must decode the same authoritative
	// sequence, mailbox, dialogs, and diff snapshot rather than client-local state.
	cli, web, apple := conformanceProjection(t, threadURL), conformanceProjection(t, threadURL), conformanceProjection(t, threadURL)
	for name, projection := range map[string]agothreadstore.ClientProjection{"web": web, "apple": apple} {
		if projection.SnapshotSequence != cli.SnapshotSequence || !reflect.DeepEqual(projection.Mailbox, cli.Mailbox) || !reflect.DeepEqual(projection.Dialogs, cli.Dialogs) || !reflect.DeepEqual(projection.Diff, cli.Diff) {
			t.Fatalf("%s projection diverged from CLI at sequence %d", name, cli.SnapshotSequence)
		}
	}
	if cli.NextAfterSequence != cli.SnapshotSequence || cli.HasMore {
		t.Fatalf("final reconnect cursor = %d/%d has_more=%v", cli.NextAfterSequence, cli.SnapshotSequence, cli.HasMore)
	}
	for index, event := range cli.Events {
		if index > 0 && event.Sequence != cli.Events[index-1].Sequence+1 {
			t.Fatalf("event sequence gap or duplicate at %d: %d after %d", index, event.Sequence, cli.Events[index-1].Sequence)
		}
	}
	if got := strings.TrimSpace(string(gitConformanceOutput(t, workspace, "diff", "--cached"))); got != "" {
		t.Fatalf("stage/unstage/revert left staged bytes: %q", got)
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "change.txt")); err != nil || string(got) != "changed\n" {
		t.Fatalf("index mutations changed worktree bytes: %q, %v", got, err)
	}

	// Restart the actual daemon/store boundary, mutate after reopening, then
	// reconnect from the pre-restart cursor. Only the new durable event may be
	// delivered and the full projection must retain the same Git snapshot.
	preRestartSequence := cli.SnapshotSequence
	httpServer.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = agothreadstore.Open(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	httpServer = httptest.NewServer(New(store, nil).WithGitRefresher(agogit.NewService(store)).Handler())
	threadURL = httpServer.URL + "/v1/threads/" + created.ThreadID
	archive := requestJSON(t, http.MethodPost, threadURL+"/archive", map[string]any{
		"command_id": "archive-after-restart", "idempotency_key": "archive-after-restart", "actor_id": "web-pwa", "expected_sequence": preRestartSequence,
	})
	if archive.StatusCode != http.StatusAccepted {
		t.Fatalf("post-restart archive status = %d", archive.StatusCode)
	}
	_ = archive.Body.Close()
	reconnected := conformanceProjectionAfter(t, threadURL, preRestartSequence)
	if len(reconnected.Events) != 1 || reconnected.Events[0].Sequence != preRestartSequence+1 || reconnected.Events[0].Type != agoprotocol.EventThreadArchived || reconnected.NextAfterSequence != preRestartSequence+1 {
		t.Fatalf("post-restart reconnect = %#v", reconnected)
	}
	fullAfterRestart := conformanceProjection(t, threadURL)
	if fullAfterRestart.Diff.Snapshot == nil || fullAfterRestart.Diff.Snapshot.Digest != cli.Diff.Snapshot.Digest {
		t.Fatalf("restart lost authoritative diff: before=%#v after=%#v", cli.Diff.Snapshot, fullAfterRestart.Diff.Snapshot)
	}
}

type conformanceVerifier struct{}

func (conformanceVerifier) Execute(_ context.Context, request agoverifier.ExecutionRequest) (agoverifier.ExecutionResult, error) {
	if request.Executable != "/usr/bin/true" || request.ThreadID == "" || request.Workspace == "" {
		return agoverifier.ExecutionResult{}, os.ErrInvalid
	}
	return agoverifier.ExecutionResult{Output: []byte("representative check passed")}, nil
}

func conformanceProjection(t *testing.T, threadURL string) agothreadstore.ClientProjection {
	return conformanceProjectionAfter(t, threadURL, 0)
}

func conformanceProjectionAfter(t *testing.T, threadURL string, after uint64) agothreadstore.ClientProjection {
	t.Helper()
	response := requestJSON(t, http.MethodGet, threadURL+"/projection?after="+strconv.FormatUint(after, 10)+"&limit=1000", nil)
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("projection status = %d", response.StatusCode)
	}
	var projection agothreadstore.ClientProjection
	if err := json.NewDecoder(response.Body).Decode(&projection); err != nil {
		t.Fatal(err)
	}
	return projection
}

func gitConformanceRun(t *testing.T, workspace string, args ...string) {
	_ = gitConformanceOutput(t, workspace, args...)
}

func gitConformanceOutput(t *testing.T, workspace string, args ...string) []byte {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = workspace
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return output
}
