package agoscheduler_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claudexflow/internal/agoartifact"
	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agoexec"
	"claudexflow/internal/agointegrate"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agorelay"
	"claudexflow/internal/agoscheduler"
	"claudexflow/internal/agoworktree"
)

// writingModel edits a file per task, so the graph produces real changes that
// must survive their worktrees and chain together.
type writingModel struct {
	byTask map[string]map[string]string
}

func (m *writingModel) CompleteJSON(_ context.Context, request agorelay.Request, target any) error {
	taskID := ""
	for id := range m.byTask {
		if strings.Contains(request.User, id) {
			taskID = id
		}
	}
	edits := []map[string]any{}
	for path, contents := range m.byTask[taskID] {
		edits = append(edits, map[string]any{"path": path, "contents": contents})
	}
	plan := map[string]any{"status": "completed", "summary": "完成 " + taskID, "edits": edits}
	return roundTripJSON(plan, target)
}

// acceptingVerifierProvider accepts anything with evidence. Its independence is
// its identity: the scheduler calls it under the verifier id, never the worker.
type acceptingVerifierProvider struct{}

func (acceptingVerifierProvider) Verify(_ context.Context, dispatch agoboardruntime.Dispatch, result agoboardruntime.ExecutionResult) (agoboardruntime.Review, error) {
	if result.Summary == "" {
		return agoboardruntime.Review{Accepted: false, Reason: "没有证据"}, nil
	}
	return agoboardruntime.Review{Accepted: true, Reason: "证据满足验收标准"}, nil
}

func initGitRepository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"}, {"config", "user.email", "u@example.com"}, {"config", "user.name", "U"},
		{"add", "-A"}, {"commit", "-m", "initial"},
	} {
		command := exec.Command("git", args...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
	}
	return repo
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// A two-task chain proves the whole point of integration: an accepted change
// becomes a durable commit that outlives its worktree, and the downstream task
// starts from work that already contains it.
func TestAcceptedWorkIsIntegratedAndInheritedDownstream(t *testing.T) {
	ctx := context.Background()
	repo := initGitRepository(t)
	base := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := agoboardstore.Open(filepath.Join(base, "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	artifacts, err := agoartifact.Open(agoartifact.Options{Root: filepath.Join(base, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	worktrees, err := agoworktree.New(agoworktree.Options{Root: filepath.Join(base, "worktrees")})
	if err != nil {
		t.Fatal(err)
	}
	integrator, err := agointegrate.New(agointegrate.Options{Root: filepath.Join(base, "integration")})
	if err != nil {
		t.Fatal(err)
	}

	model := &writingModel{byTask: map[string]map[string]string{
		"first":  {"README.md": "# fixture\n\n## 快速开始\n"},
		"second": {"NOTES.md": "# 说明\n\n引用了 README。\n"},
	}}
	executor, err := agoexec.New(agoexec.Options{
		Model: model, Worktrees: worktrees, Artifacts: artifacts, Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	boardID := "board-integration"
	ref := agointegrate.RefName(boardID)
	baseRevision, err := integrator.EnsureRef(ctx, repo, ref, "")
	if err != nil {
		t.Fatal(err)
	}

	plan := agoplanner.Plan{
		SchemaVersion: agoplanner.SchemaVersion,
		Repository:    agoplanner.Repository{ID: repo, Revision: baseRevision},
		Objective:     agoplanner.Objective{ID: "objective", Summary: "增加快速开始并补充说明"},
		ProjectGates: []agoplanner.ProjectGate{{
			ID: "gate", Title: "验收", AcceptanceCriteria: []string{"通过"}, VerifierIDs: []string{"ago-verifier"},
		}},
		Tasks: []agoplanner.TaskProposal{
			{ID: "first", Title: "first", Description: "写 README", PathScopes: []string{"README.md"},
				AcceptanceCriteria: []string{"README 有章节"}, VerifierIDs: []string{"ago-verifier"}, CapabilityTags: []string{"repo-write"}},
			{ID: "second", Title: "second", Description: "写说明", PathScopes: []string{"NOTES.md"},
				AcceptanceCriteria: []string{"说明存在"}, VerifierIDs: []string{"ago-verifier"}, CapabilityTags: []string{"repo-write"}},
		},
		Dependencies: []agoplanner.DependencyProposal{{TaskID: "second", DependsOn: "first"}},
	}
	runtime := agoboardruntime.New(store, agoplanner.FixturePlanner{Proposal: plan}, agoboardruntime.Options{
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: time.Now,
	})
	if _, err := runtime.Create(ctx, agoboardruntime.Goal{
		BoardID: boardID, Repository: plan.Repository, Objective: plan.Objective,
		ProjectGates: plan.ProjectGates,
		Constraints: agoplanner.Constraints{
			PathScopes: []string{"README.md", "NOTES.md"}, CapabilityTags: []string{"repo-write"},
			VerifierIDs: []string{"ago-verifier"},
		},
		BaseRevision: baseRevision, IntegrationRef: ref,
	}); err != nil {
		t.Fatal(err)
	}

	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime, Executor: executor, Verifier: acceptingVerifierProvider{},
		Integrator: integrator, Artifacts: artifacts,
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 12 {
		if _, err := scheduler.RunOnce(ctx); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		completion, err := store.Completion(ctx, boardID)
		if err != nil {
			t.Fatal(err)
		}
		if completion.Status == agoboardstore.CompletionPassed {
			break
		}
	}

	board, err := store.Board(ctx, boardID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range board.Tasks {
		if task.State != agoboardprotocol.TaskPassed {
			t.Fatalf("task %q = %q (%s), want passed", task.ID, task.State, task.BlockedReason)
		}
		if task.IntegratedRevision == "" {
			t.Fatalf("write task %q passed without an integrated revision", task.ID)
		}
	}

	// The commits exist and the worktrees that produced them are long gone.
	tip := gitOutput(t, repo, "rev-parse", ref)
	if tip == baseRevision {
		t.Fatal("the integration ref never advanced")
	}
	if content := gitOutput(t, repo, "show", tip+":README.md"); !strings.Contains(content, "快速开始") {
		t.Fatalf("the first task's change is not in the integrated revision: %q", content)
	}
	if content := gitOutput(t, repo, "show", tip+":NOTES.md"); !strings.Contains(content, "说明") {
		t.Fatalf("the second task's change is not in the integrated revision: %q", content)
	}
	entries, err := os.ReadDir(filepath.Join(base, "worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	live := 0
	for _, entry := range entries {
		if entry.IsDir() {
			live++
		}
	}
	if live != 0 {
		t.Fatalf("%d worktrees survived; the commits must be what persists", live)
	}

	// The user's branch and working tree are exactly as they were.
	if head := gitOutput(t, repo, "rev-parse", "main"); head != baseRevision {
		t.Fatalf("the user's branch moved from %s to %s", baseRevision, head)
	}
	if status := gitOutput(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("the user's working tree is dirty: %q", status)
	}
	if content, err := os.ReadFile(filepath.Join(repo, "README.md")); err != nil || string(content) != "# fixture\n" {
		t.Fatalf("the user's checkout was modified: %q %v", content, err)
	}
}

// A rejected attempt must never reach the integration ref.
func TestRejectedWorkIsNeverIntegrated(t *testing.T) {
	ctx := context.Background()
	repo := initGitRepository(t)
	base := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := agoboardstore.Open(filepath.Join(base, "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	artifacts, err := agoartifact.Open(agoartifact.Options{Root: filepath.Join(base, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	worktrees, err := agoworktree.New(agoworktree.Options{Root: filepath.Join(base, "worktrees")})
	if err != nil {
		t.Fatal(err)
	}
	integrator, err := agointegrate.New(agointegrate.Options{Root: filepath.Join(base, "integration")})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := agoexec.New(agoexec.Options{
		Model:     &writingModel{byTask: map[string]map[string]string{"only": {"README.md": "# 被拒绝的修改\n"}}},
		Worktrees: worktrees, Artifacts: artifacts, Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	boardID := "board-rejected"
	ref := agointegrate.RefName(boardID)
	baseRevision, err := integrator.EnsureRef(ctx, repo, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	plan := agoplanner.Plan{
		SchemaVersion: agoplanner.SchemaVersion,
		Repository:    agoplanner.Repository{ID: repo, Revision: baseRevision},
		Objective:     agoplanner.Objective{ID: "objective", Summary: "会被拒绝的目标"},
		ProjectGates: []agoplanner.ProjectGate{{
			ID: "gate", Title: "验收", AcceptanceCriteria: []string{"通过"}, VerifierIDs: []string{"ago-verifier"},
		}},
		Tasks: []agoplanner.TaskProposal{{
			ID: "only", Title: "only", Description: "写 README", PathScopes: []string{"README.md"},
			AcceptanceCriteria: []string{"必须被拒绝"}, VerifierIDs: []string{"ago-verifier"}, CapabilityTags: []string{"repo-write"},
		}},
		Dependencies: []agoplanner.DependencyProposal{},
	}
	runtime := agoboardruntime.New(store, agoplanner.FixturePlanner{Proposal: plan}, agoboardruntime.Options{
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: time.Now,
	})
	if _, err := runtime.Create(ctx, agoboardruntime.Goal{
		BoardID: boardID, Repository: plan.Repository, Objective: plan.Objective, ProjectGates: plan.ProjectGates,
		Constraints: agoplanner.Constraints{
			PathScopes: []string{"README.md"}, CapabilityTags: []string{"repo-write"}, VerifierIDs: []string{"ago-verifier"},
		},
		BaseRevision: baseRevision, IntegrationRef: ref,
	}); err != nil {
		t.Fatal(err)
	}

	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime, Executor: executor,
		Verifier:   rejectingVerifier{},
		Integrator: integrator, Artifacts: artifacts,
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: time.Minute, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 6 {
		if _, err := scheduler.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// The ref never moved, and the rejected content is nowhere in the repository.
	if tip := gitOutput(t, repo, "rev-parse", ref); tip != baseRevision {
		t.Fatalf("a rejected change advanced the integration ref to %s", tip)
	}
	if content := gitOutput(t, repo, "show", baseRevision+":README.md"); strings.Contains(content, "被拒绝") {
		t.Fatal("rejected content reached the repository")
	}
}

type rejectingVerifier struct{}

func (rejectingVerifier) Verify(context.Context, agoboardruntime.Dispatch, agoboardruntime.ExecutionResult) (agoboardruntime.Review, error) {
	return agoboardruntime.Review{
		Accepted: false, FailureClass: agoboardprotocol.FailurePolicy, Reason: "不符合要求",
	}, nil
}

// roundTripJSON encodes a scripted plan and decodes it into the target the same
// way a real relay response would arrive.
func roundTripJSON(value any, target any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}
