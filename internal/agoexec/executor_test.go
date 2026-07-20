package agoexec_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claudexflow/internal/agoartifact"
	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoexec"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agorelay"
	"claudexflow/internal/agoworktree"
)

const secretSentinel = "sk-ant-EXEC-SENTINEL-never-in-a-prompt-0123"

// scriptedModel returns a canned plan, and records exactly what it was asked.
// Recording the prompt is what lets a test prove no credential or fencing token
// was ever placed in it.
type scriptedModel struct {
	response any
	err      error
	prompts  []agorelay.Request
}

func (m *scriptedModel) CompleteJSON(_ context.Context, request agorelay.Request, target any) error {
	m.prompts = append(m.prompts, request)
	if m.err != nil {
		return m.err
	}
	encoded, err := jsonMarshal(m.response)
	if err != nil {
		return err
	}
	return jsonUnmarshal(encoded, target)
}

// fakeCommands reports a scripted exit code without running anything.
type fakeCommands struct {
	exitCode int
	stdout   string
	ran      []string
	err      error
}

func (c *fakeCommands) Run(_ context.Context, dir, name string, args ...string) (string, int, error) {
	c.ran = append(c.ran, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	return c.stdout, c.exitCode, c.err
}

type fixture struct {
	executor  *agoexec.Executor
	model     *scriptedModel
	commands  *fakeCommands
	worktrees *agoworktree.Manager
	repo      string
	base      string
}

func newFixture(t *testing.T, response any, modelErr error) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	base := t.TempDir()
	repo := initRepository(t, base)
	manager, err := agoworktree.New(agoworktree.Options{Root: filepath.Join(base, "worktrees")})
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := agoartifact.Open(agoartifact.Options{Root: filepath.Join(base, "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	model := &scriptedModel{response: response, err: modelErr}
	commands := &fakeCommands{}
	executor, err := agoexec.New(agoexec.Options{
		Model: model, Worktrees: manager, Artifacts: artifacts, Commands: commands,
		Timeout: 30 * time.Second,
		Now:     func() time.Time { return time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{executor: executor, model: model, commands: commands, worktrees: manager, repo: repo, base: base}
}

func initRepository(t *testing.T, base string) string {
	t.Helper()
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "secret.txt"), []byte("out of scope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "t"},
		{"add", "-A"}, {"commit", "-m", "init"},
	} {
		command := exec.Command("git", args...)
		command.Dir = repo
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, output)
		}
	}
	return repo
}

func dispatchFor(repo string, scopes ...string) agoboardruntime.Dispatch {
	if len(scopes) == 0 {
		scopes = []string{"README.md"}
	}
	return agoboardruntime.Dispatch{
		Goal: agoboardruntime.Goal{
			Repository: agoplanner.Repository{ID: repo, Revision: "HEAD"},
			Objective:  agoplanner.Objective{ID: "goal", Summary: "为 README 增加快速开始章节"},
		},
		Task: agoplanner.TaskProposal{
			ID: "update-readme", Title: "更新 README", Description: "增加快速开始章节。",
			PathScopes: scopes, AcceptanceCriteria: []string{"README 包含快速开始章节"},
			VerifierIDs: []string{"ago-verifier"}, CapabilityTags: []string{"repo-write"},
		},
		AttemptID: "attempt-1", WorkerID: "ago-worker", AttemptNumber: 1,
	}
}

func successPlan() map[string]any {
	return map[string]any{
		"status": "completed", "summary": "已增加快速开始章节。",
		"edits":    []map[string]any{{"path": "README.md", "contents": "# fixture\n\n## 快速开始\n\n运行 go test ./...\n"}},
		"commands": []string{"go version"},
	}
}

// The happy path: the model edits an in-scope file, the change is measured from
// git, and the evidence carries real hashes.
func TestExecutesInIsolationAndReportsMeasuredChanges(t *testing.T) {
	f := newFixture(t, successPlan(), nil)
	f.commands.exitCode = 0
	f.commands.stdout = "go version go1.26 darwin/arm64"

	result, err := f.executor.Execute(context.Background(), dispatchFor(f.repo))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Result.ChangedFiles) != 1 || result.Result.ChangedFiles[0].Path != "README.md" {
		t.Fatalf("changed files = %#v", result.Result.ChangedFiles)
	}
	change := result.Result.ChangedFiles[0]
	if change.BeforeHash == "" || change.AfterHash == "" || change.BeforeHash == change.AfterHash {
		t.Fatalf("hashes are not real measurements: %#v", change)
	}
	if len(result.Result.Tests) != 1 || !result.Result.Tests[0].Passed || !result.Result.Tests[0].Required {
		t.Fatalf("tests = %#v", result.Result.Tests)
	}
	// A change must be captured as durable bytes before the worktree is
	// deleted, otherwise an accepted result would vanish.
	if result.Result.Patch == nil {
		t.Fatal("a change produced no durable patch record")
	}
	if result.Result.Patch.ArtifactID == "" || result.Result.Patch.SHA256 == "" || result.Result.Patch.BaseRevision == "" {
		t.Fatalf("patch record is incomplete: %#v", result.Result.Patch)
	}
	if len(result.Result.Patch.ChangedPaths) != 1 || result.Result.Patch.ChangedPaths[0] != "README.md" {
		t.Fatalf("patch changed paths = %#v", result.Result.Patch.ChangedPaths)
	}
	var patchRef bool
	for _, artifact := range result.Result.Artifacts {
		if artifact.ID == result.Result.Patch.ArtifactID && artifact.SHA256 != "" {
			patchRef = true
		}
	}
	if !patchRef {
		t.Fatalf("the patch is not referenced as an artifact: %#v", result.Result.Artifacts)
	}

	// The canonical repository must be byte-identical: the work happened in an
	// isolated copy, and nothing merges it back.
	content, err := os.ReadFile(filepath.Join(f.repo, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "# fixture\n" {
		t.Fatalf("the canonical repository was modified: %q", content)
	}
	if status := gitStatus(t, f.repo); status != "" {
		t.Fatalf("the canonical repository has uncommitted changes: %q", status)
	}
}

// A model that writes outside its declared scope fails the attempt, and the
// change never reaches the canonical tree.
func TestChangeOutsideScopeFailsTheAttemptAndIsDiscarded(t *testing.T) {
	f := newFixture(t, map[string]any{
		"status": "completed", "summary": "顺便改了别的文件。",
		"edits": []map[string]any{
			{"path": "README.md", "contents": "# fixture\n\n## 快速开始\n"},
			{"path": "secret.txt", "contents": "已被篡改\n"},
		},
	}, nil)

	_, err := f.executor.Execute(context.Background(), dispatchFor(f.repo, "README.md"))
	if err == nil {
		t.Fatal("a change outside the declared scope was accepted")
	}
	var classified interface {
		FailureClass() agoboardprotocol.FailureClass
	}
	if !errors.As(err, &classified) || classified.FailureClass() != agoboardprotocol.FailurePolicy {
		t.Fatalf("failure class = %v, want policy", err)
	}
	if !strings.Contains(err.Error(), "secret.txt") {
		t.Fatalf("the error does not name the offending path: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(f.repo, "secret.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "out of scope\n" {
		t.Fatalf("an out-of-scope file was modified in the canonical repository: %q", content)
	}
}

// A path that tries to leave the worktree is refused before anything is written.
func TestPathEscapeAttemptsAreRefusedBeforeWriting(t *testing.T) {
	victim := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(victim, []byte("untouched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"absolute":  victim,
		"traversal": "../../../../../../etc/ago-escape.txt",
		"parent":    "..",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, map[string]any{
				"status": "completed", "summary": "越界写入。",
				"edits": []map[string]any{{"path": path, "contents": "pwned\n"}},
			}, nil)
			_, err := f.executor.Execute(context.Background(), dispatchFor(f.repo))
			if err == nil {
				t.Fatalf("an escaping path %q was written", path)
			}
			content, readErr := os.ReadFile(victim)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(content) != "untouched\n" {
				t.Fatalf("a file outside the worktree was written: %q", content)
			}
		})
	}
}

// A model that says it needs information stops honestly rather than inventing
// changes, and the stop is classified so the supervisor escalates it.
func TestNeedsInputStopsWithoutWriting(t *testing.T) {
	f := newFixture(t, map[string]any{
		"status": "blocked", "summary": "",
		"needs_input": "不清楚应该使用哪个包管理器。",
	}, nil)
	_, err := f.executor.Execute(context.Background(), dispatchFor(f.repo))
	if err == nil {
		t.Fatal("a blocked model produced a successful result")
	}
	var classified interface {
		FailureClass() agoboardprotocol.FailureClass
	}
	if !errors.As(err, &classified) || classified.FailureClass() != agoboardprotocol.FailureNeedsInput {
		t.Fatalf("failure class = %v, want needs-input", err)
	}
	if status := gitStatus(t, f.repo); status != "" {
		t.Fatalf("a blocked attempt still modified the repository: %q", status)
	}
}

// Relay faults are classified so bounded retry applies to the ones retrying can
// actually fix.
func TestRelayFailuresAreClassified(t *testing.T) {
	for name, testCase := range map[string]struct {
		err  error
		want agoboardprotocol.FailureClass
	}{
		"rate limited": {agorelay.StatusError{Code: 429, Message: "slow down"}, agoboardprotocol.FailureTransient},
		"server error": {agorelay.StatusError{Code: 503, Message: "unavailable"}, agoboardprotocol.FailureTransient},
		"bad request":  {agorelay.StatusError{Code: 400, Message: "bad"}, agoboardprotocol.FailurePermanent},
		"unauthorized": {agorelay.StatusError{Code: 401, Message: "no"}, agoboardprotocol.FailurePermanent},
		"malformed":    {errors.New("could not parse structured output"), agoboardprotocol.FailureTransient},
		"deadline":     {context.DeadlineExceeded, agoboardprotocol.FailureTransient},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil, testCase.err)
			_, err := f.executor.Execute(context.Background(), dispatchFor(f.repo))
			if err == nil {
				t.Fatal("a relay failure produced a successful result")
			}
			var classified interface {
				FailureClass() agoboardprotocol.FailureClass
			}
			if !errors.As(err, &classified) {
				t.Fatalf("error is not classified: %v", err)
			}
			if got := classified.FailureClass(); got != testCase.want {
				t.Fatalf("class = %q, want %q", got, testCase.want)
			}
		})
	}
}

// An oversized edit is refused rather than written.
func TestOversizedEditIsRefused(t *testing.T) {
	f := newFixture(t, map[string]any{
		"status": "completed", "summary": "巨大的写入。",
		"edits": []map[string]any{{"path": "README.md", "contents": strings.Repeat("A", 400*1024)}},
	}, nil)
	_, err := f.executor.Execute(context.Background(), dispatchFor(f.repo))
	if err == nil {
		t.Fatal("an oversized edit was written")
	}
	if !strings.Contains(err.Error(), "超过上限") {
		t.Fatalf("error = %v, want it to name the byte limit", err)
	}
}

// A response proposing an implausible number of edits is refused outright.
func TestTooManyEditsAreRefused(t *testing.T) {
	edits := make([]map[string]any, 40)
	for index := range edits {
		edits[index] = map[string]any{"path": fmt.Sprintf("docs/f%d.md", index), "contents": "x"}
	}
	f := newFixture(t, map[string]any{"status": "completed", "summary": "重写一切。", "edits": edits}, nil)
	if _, err := f.executor.Execute(context.Background(), dispatchFor(f.repo, "docs")); err == nil {
		t.Fatal("an unbounded rewrite was accepted")
	}
}

// Only a small allowlist of commands may run, and shell metacharacters cannot
// smuggle anything past it.
func TestOnlyAllowlistedCommandsRun(t *testing.T) {
	for name, command := range map[string]string{
		"git push":     "git push origin main",
		"remove":       "rm -rf /",
		"curl":         "curl https://example.com",
		"chained":      "go test ./... && rm -rf .",
		"substitution": "go test $(whoami)",
		"redirect":     "go test > /etc/passwd",
	} {
		t.Run(name, func(t *testing.T) {
			plan := successPlan()
			plan["commands"] = []string{command}
			f := newFixture(t, plan, nil)
			result, err := f.executor.Execute(context.Background(), dispatchFor(f.repo))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(f.commands.ran) != 0 {
				t.Fatalf("a forbidden command ran: %#v", f.commands.ran)
			}
			// The refusal is visible to the user rather than silent.
			joined := strings.Join(result.Result.Warnings, " ")
			if !strings.Contains(joined, "拒绝执行") {
				t.Fatalf("the refusal was not recorded: %#v", result.Result.Warnings)
			}
		})
	}
}

// A failing check is reported as a failing check. The executor does not decide
// what that means; the deterministic gate in the state machine does.
func TestFailingCommandIsRecordedAsAFailedRequiredTest(t *testing.T) {
	f := newFixture(t, successPlan(), nil)
	f.commands.exitCode = 1
	f.commands.stdout = "FAIL"

	result, err := f.executor.Execute(context.Background(), dispatchFor(f.repo))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(result.Result.Tests) != 1 || result.Result.Tests[0].Passed {
		t.Fatalf("tests = %#v, want one failed required test", result.Result.Tests)
	}
	if result.Result.RequiredTestsPassed() {
		t.Fatal("evidence with a failing required test claimed its checks passed")
	}
}

// The prompt must never contain a provider credential or a fencing token.
func TestPromptCarriesNoCredentialOrFencingToken(t *testing.T) {
	t.Setenv("AGO_PROVIDER_API_KEY", secretSentinel)
	f := newFixture(t, successPlan(), nil)
	dispatch := dispatchFor(f.repo)
	if _, err := f.executor.Execute(context.Background(), dispatch); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(f.model.prompts) == 0 {
		t.Fatal("the model was never called, so the check proves nothing")
	}
	for _, prompt := range f.model.prompts {
		whole := prompt.System + "\n" + prompt.User
		if strings.Contains(whole, secretSentinel) {
			t.Fatal("the prompt carried the provider credential")
		}
		if strings.Contains(whole, "fencing") || strings.Contains(whole, "token") {
			t.Fatalf("the prompt mentions a fencing token: %q", whole)
		}
		// It must still carry what the task actually needs.
		if !strings.Contains(whole, dispatch.Task.Title) || !strings.Contains(whole, "README.md") {
			t.Fatal("the prompt is missing the task contract")
		}
	}
}

// A credential that leaks into model output must not survive into evidence.
func TestCredentialInModelOutputIsRedactedFromEvidence(t *testing.T) {
	f := newFixture(t, map[string]any{
		"status":  "completed",
		"summary": "完成，密钥是 " + secretSentinel,
		"edits":   []map[string]any{{"path": "README.md", "contents": "# fixture\n\n## 快速开始\n"}},
		"risks":   []string{"api_key=" + secretSentinel},
	}, nil)
	// Seed the redactor with the sentinel the way production seeds it from the
	// environment.
	t.Setenv("AGO_PROVIDER_API_KEY", secretSentinel)
	artifacts, err := agoartifact.Open(agoartifact.Options{Root: filepath.Join(t.TempDir(), "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := agoexec.New(agoexec.Options{
		Model: f.model, Worktrees: f.worktrees, Artifacts: artifacts, Commands: f.commands,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := rebuilt.Execute(context.Background(), dispatchFor(f.repo))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(result.Summary, secretSentinel) {
		t.Fatalf("the summary carried the credential: %q", result.Summary)
	}
	for _, warning := range result.Result.Warnings {
		if strings.Contains(warning, secretSentinel) {
			t.Fatalf("a warning carried the credential: %q", warning)
		}
	}
}

// A worktree is cleaned up whether the attempt succeeded or failed, so a
// failure cannot leave an edited copy for the next attempt to inherit.
func TestWorktreeIsRemovedOnBothSuccessAndFailure(t *testing.T) {
	for name, response := range map[string]any{
		"success": successPlan(),
		"failure": map[string]any{"status": "completed", "summary": "越界。",
			"edits": []map[string]any{{"path": "secret.txt", "contents": "x"}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, response, nil)
			_, _ = f.executor.Execute(context.Background(), dispatchFor(f.repo, "README.md"))
			entries, err := os.ReadDir(filepath.Join(f.base, "worktrees"))
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
				t.Fatalf("%d worktrees were left behind", live)
			}
		})
	}
}

func gitStatus(t *testing.T, repo string) string {
	t.Helper()
	command := exec.Command("git", "status", "--porcelain")
	command.Dir = repo
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

// The scripted model round-trips through JSON so a test exercises the same
// decoding path a real relay response takes.
func jsonMarshal(value any) ([]byte, error)       { return json.Marshal(value) }
func jsonUnmarshal(data []byte, target any) error { return json.Unmarshal(data, target) }

// escapingCommands stands in for what `node build.js` or `make` actually do:
// run model-authored code that can write anywhere in the worktree.
type escapingCommands struct{ dir string }

func (c *escapingCommands) Run(_ context.Context, dir, name string, args ...string) (string, int, error) {
	c.dir = dir
	// Write a file the task was never allowed to touch.
	_ = os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o700)
	_ = os.WriteFile(filepath.Join(dir, ".github", "workflows", "deploy.yml"), []byte("on: push\n"), 0o600)
	return "built", 0, nil
}

// A verification command runs model-authored code AFTER the first scope check.
// Whatever it writes ends up in the patch, so the gate has to be applied again
// to the state the patch actually captures — otherwise an out-of-scope file
// reaches the integration ref while the evidence names only in-scope ones.
func TestCommandsCannotWriteOutsideScopeAfterTheFirstCheck(t *testing.T) {
	f := newFixture(t, successPlan(), nil)
	escaping := &escapingCommands{}
	rebuilt, err := agoexec.New(agoexec.Options{
		Model: f.model, Worktrees: f.worktrees, Artifacts: mustArtifacts(t), Commands: escaping,
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = rebuilt.Execute(context.Background(), dispatchFor(f.repo, "README.md"))
	if err == nil {
		t.Fatal("a command wrote outside the declared scope and the attempt was accepted")
	}
	if escaping.dir == "" {
		t.Fatal("the command never ran, so the test proves nothing")
	}
	var classified interface {
		FailureClass() agoboardprotocol.FailureClass
	}
	if !errors.As(err, &classified) || classified.FailureClass() != agoboardprotocol.FailurePolicy {
		t.Fatalf("failure class = %v, want policy", err)
	}
	if !strings.Contains(err.Error(), "deploy.yml") {
		t.Fatalf("the error does not name the file the command wrote: %v", err)
	}
	// And nothing reached the canonical repository.
	if _, statErr := os.Stat(filepath.Join(f.repo, ".github")); statErr == nil {
		t.Fatal("the out-of-scope file reached the canonical repository")
	}
}

func mustArtifacts(t *testing.T) *agoartifact.Store {
	t.Helper()
	store, err := agoartifact.Open(agoartifact.Options{Root: filepath.Join(t.TempDir(), "artifacts")})
	if err != nil {
		t.Fatal(err)
	}
	return store
}
