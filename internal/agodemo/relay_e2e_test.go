package agodemo_test

import (
	"context"
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
	"claudexflow/internal/agodemo"
	"claudexflow/internal/agoexec"
	"claudexflow/internal/agointegrate"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agorelay"
	"claudexflow/internal/agorelayplanner"
	"claudexflow/internal/agorelayverifier"
	"claudexflow/internal/agoscheduler"
	"claudexflow/internal/agosupervisor"
	"claudexflow/internal/agoverify"
	"claudexflow/internal/agoworktree"
)

// The full product claim, against a real model and a real repository:
//
//	a Chinese goal → a real plan → multiple tasks executed in isolation →
//	independent verification → durable integration → a final revision
//
// with no human message after the goal is stated.
//
// It is opt-in because it needs a provider. The offline suites cover every
// mechanism it exercises; what only this test can show is that the pieces hold
// together when a real model is doing the work.
//
// Run with:
//
//	AGO_RELAY_BASE_URL=... AGO_RELAY_API_KEY=... \
//	  go test -run TestRealRelayCompletesAMultiTaskGoal -timeout 30m ./internal/agodemo
func TestRealRelayCompletesAMultiTaskGoal(t *testing.T) {
	baseURL, apiKey := os.Getenv("AGO_RELAY_BASE_URL"), os.Getenv("AGO_RELAY_API_KEY")
	if baseURL == "" || apiKey == "" {
		t.Skip("relay not configured; set AGO_RELAY_BASE_URL and AGO_RELAY_API_KEY")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()

	base := t.TempDir()
	repo := filepath.Join(base, "greeter")
	if err := agodemo.Create(ctx, repo); err != nil {
		t.Fatal(err)
	}
	// Everything the fixture starts with, so a later comparison can prove what
	// the user's own branch looked like before and after.
	userBranchBefore := git(t, repo, "rev-parse", "main")
	userStatusBefore := git(t, repo, "status", "--porcelain")

	store, err := agoboardstore.Open(filepath.Join(base, "ago.db"))
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

	model := func(env, fallback string) *agorelay.Client {
		name := os.Getenv(env)
		if name == "" {
			name = fallback
		}
		client, err := agorelay.New(agorelay.Profile{
			ID: "relay-" + name, BaseURL: baseURL, Model: name, APIKeyEnv: "AGO_RELAY_API_KEY",
			Timeout: 4 * time.Minute, MaxOutputBytes: 1 << 20,
		}, nil, os.Getenv)
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	// Three roles, three clients, three separate calls.
	planner, err := agorelayplanner.New(agorelayplanner.Options{Model: model("AGO_PLANNER_MODEL", "claude-sonnet-5")})
	if err != nil {
		t.Fatal(err)
	}
	executor, err := agoexec.New(agoexec.Options{
		Model: model("AGO_EXECUTOR_MODEL", "claude-sonnet-5"), Worktrees: worktrees,
		Artifacts: artifacts, Commands: agoexec.SystemCommands{}, Timeout: 6 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	judge, err := agorelayverifier.New(agorelayverifier.Options{Model: model("AGO_VERIFIER_MODEL", "claude-sonnet-5")})
	if err != nil {
		t.Fatal(err)
	}
	verification, err := agoverify.New(agoverify.Options{
		Judge: agoverify.RelayJudge{Verifier: judge}, Artifacts: artifacts,
	})
	if err != nil {
		t.Fatal(err)
	}

	boardID := "board-relay-e2e"
	ref := agointegrate.RefName(boardID)
	baseRevision, err := integrator.EnsureRef(ctx, repo, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	runtime := agoboardruntime.New(store, planner, agoboardruntime.Options{
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: 10 * time.Minute, Now: time.Now,
	})
	if _, err := runtime.Create(ctx, agoboardruntime.Goal{
		BoardID:    boardID,
		Repository: agoplanner.Repository{ID: repo, Revision: baseRevision},
		Objective:  agoplanner.Objective{ID: "objective", Summary: agodemo.Objective},
		ProjectGates: []agoplanner.ProjectGate{{
			ID: "gate", Title: "目标验收",
			AcceptanceCriteria: []string{"所有任务通过独立验收"}, VerifierIDs: []string{"ago-verifier"},
		}},
		Constraints: agoplanner.Constraints{
			PathScopes:     agodemo.PathScopes(),
			CapabilityTags: []string{"repo-read", "repo-write", "tests", "report"},
			VerifierIDs:    []string{"ago-verifier"},
		},
		ExecutionMode: "relay", BaseRevision: baseRevision, IntegrationRef: ref,
	}); err != nil {
		t.Fatalf("the planner could not produce an admissible plan: %v", err)
	}

	planned, err := store.Board(ctx, boardID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("plan: %d tasks, %d dependencies", len(planned.Tasks), len(planned.Dependencies))
	for _, task := range planned.Tasks {
		t.Logf("  %s [%s] %s", task.ID, task.AccessMode, task.Title)
	}
	if len(planned.Tasks) < 3 {
		t.Fatalf("the plan has %d tasks; this goal needs several", len(planned.Tasks))
	}
	if len(planned.Dependencies) == 0 {
		t.Fatal("the plan has no dependencies, so nothing proves ordering")
	}

	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime, Executor: executor, Verification: verification,
		Integrator: integrator, Artifacts: artifacts,
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
		LeaseDuration: 10 * time.Minute, Now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := agosupervisor.New(agosupervisor.Options{
		Store: store, Scheduler: scheduler, BoardID: boardID, CoordinatorID: "ago-supervisor",
		Authorize: agosupervisor.Authorization{LocalFileWrites: true, LocalCommits: true},
		Now:       time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}

	// From here to the end there is no human input of any kind.
	started := time.Now()
	status, runErr := supervisor.Run(ctx, 120)
	elapsed := time.Since(started)

	final, err := store.Board(ctx, boardID)
	if err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		for _, task := range final.Tasks {
			t.Logf("  %s = %s attempts=%d class=%s blocked=%s", task.ID, task.State,
				task.AttemptCount, task.FailureClass, task.BlockedReason)
		}
		t.Fatalf("the supervisor could not drive the goal to a terminal state: %v", runErr)
	}
	t.Logf("elapsed %s, complete=%v blocked=%v decisions=%d", elapsed.Round(time.Second),
		status.Complete, status.Blocked, len(status.Decisions))
	for _, task := range final.Tasks {
		t.Logf("  %s = %s attempts=%d rev=%s", task.ID, task.State, task.AttemptCount, short(task.IntegratedRevision))
	}
	for _, decision := range status.Decisions {
		t.Logf("  decision: [%s] %s — %s", decision.Kind, decision.TaskID, decision.Reason)
	}

	// The user's own branch and working tree are untouched, whatever happened.
	if after := git(t, repo, "rev-parse", "main"); after != userBranchBefore {
		t.Fatalf("the user's branch moved from %s to %s", userBranchBefore, after)
	}
	if after := git(t, repo, "status", "--porcelain"); after != userStatusBefore {
		t.Fatalf("the user's working tree changed: %q", after)
	}
	// No credential anywhere durable.
	assertNoSecret(t, filepath.Join(base, "ago.db"), apiKey)
	assertNoSecret(t, filepath.Join(base, "ago.db-wal"), apiKey)

	if !status.Complete {
		t.Fatalf("the goal did not complete: %d decisions outstanding", len(status.Decisions))
	}
	if len(status.Decisions) != 0 {
		t.Fatalf("the goal needed %d human decisions, want zero", len(status.Decisions))
	}

	// Integration produced a real revision containing the work.
	tip := git(t, repo, "rev-parse", ref)
	if tip == baseRevision {
		t.Fatal("the integration ref never advanced, so nothing was actually integrated")
	}
	t.Logf("integrated revision: %s", tip)
	t.Logf("integration log:\n%s", git(t, repo, "log", "--oneline", baseRevision+".."+tip))

	// The goal asked for a version subcommand, documentation, and a test. Check
	// the integrated tree for evidence of each, from git rather than from any
	// model's claim about itself.
	tree := git(t, repo, "ls-tree", "-r", "--name-only", tip)
	readme := git(t, repo, "show", tip+":README.md")
	if !strings.Contains(strings.ToLower(readme), "version") {
		t.Errorf("the integrated README does not document the version command:\n%s", readme)
	}
	sources := git(t, repo, "grep", "-l", "version", tip, "--", "*.go")
	if strings.TrimSpace(sources) == "" {
		t.Errorf("no Go source in the integrated revision mentions version; tree:\n%s", tree)
	}

	// The project's own tests must pass at the integrated revision. This is the
	// project-level gate: a model saying it added a test is not the same as the
	// test existing and passing.
	checkout := filepath.Join(base, "verify-checkout")
	git(t, repo, "worktree", "add", "--detach", checkout, tip)
	defer git(t, repo, "worktree", "remove", "--force", checkout)
	output, testErr := runIn(ctx, checkout, "go", "test", "./...")
	t.Logf("go test at the integrated revision:\n%s", output)
	if testErr != nil {
		t.Fatalf("the integrated revision does not pass its own tests: %v", testErr)
	}
}

func short(revision string) string {
	if len(revision) > 8 {
		return revision[:8]
	}
	return revision
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, _ := command.CombinedOutput()
	return strings.TrimSpace(string(output))
}

func runIn(ctx context.Context, dir, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	return string(output), err
}

func assertNoSecret(t *testing.T, path, secret string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if strings.Contains(string(content), secret) {
		t.Fatalf("the credential reached %s", path)
	}
}

var _ = agoboardprotocol.TaskPassed
