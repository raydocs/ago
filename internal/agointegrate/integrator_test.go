package agointegrate_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"claudexflow/internal/agointegrate"
	"claudexflow/internal/agoworktree"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, output)
	}
	return strings.TrimSpace(string(output))
}

func newRepository(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.email", "user@example.com")
	git(t, repo, "config", "user.name", "User")
	git(t, repo, "add", "-A")
	git(t, repo, "commit", "-m", "initial")
	return repo
}

func newIntegrator(t *testing.T) *agointegrate.Integrator {
	t.Helper()
	integrator, err := agointegrate.New(agointegrate.Options{Root: filepath.Join(t.TempDir(), "integration")})
	if err != nil {
		t.Fatal(err)
	}
	return integrator
}

// makePatch produces a real patch by editing an isolated worktree, which is
// exactly how the executor produces one in production.
func makePatch(t *testing.T, repo, revision string, edits map[string]string) ([]byte, string) {
	t.Helper()
	manager, err := agoworktree.New(agoworktree.Options{Root: filepath.Join(t.TempDir(), "worktrees")})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.CreateAt(context.Background(), repo, "attempt", revision)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Remove(context.Background(), lease)
	for path, contents := range edits {
		full := filepath.Join(lease.Path, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	patch, err := manager.Patch(context.Background(), lease)
	if err != nil {
		t.Fatal(err)
	}
	return patch, lease.BaseRevision
}

// An accepted change becomes a durable commit that outlives the worktree it was
// produced in. This is the whole point: before this, accepted work vanished.
func TestAcceptedChangeBecomesADurableCommitThatOutlivesTheWorktree(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	integrator := newIntegrator(t)
	ref := agointegrate.RefName("board-1")

	base, err := integrator.EnsureRef(ctx, repo, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	patch, patchBase := makePatch(t, repo, base, map[string]string{
		"README.md": "# fixture\n\n## 快速开始\n\n运行 go test ./...\n",
	})

	result, err := integrator.Integrate(ctx, agointegrate.Request{
		Repository: repo, IntegrationRef: ref,
		CurrentRevision: base, BaseRevision: patchBase,
		Patch: patch, TaskID: "update-readme", Summary: "增加快速开始章节",
	})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}
	if result.Revision == "" || result.Revision == base {
		t.Fatalf("integration did not advance the chain: %#v", result)
	}

	// The worktree that produced the patch is long gone; the content is still
	// retrievable from the commit.
	content := git(t, repo, "show", result.Revision+":README.md")
	if !strings.Contains(content, "快速开始") {
		t.Fatalf("the integrated commit does not contain the change: %q", content)
	}
	if tip := git(t, repo, "rev-parse", ref); tip != result.Revision {
		t.Fatalf("ref = %s, want %s", tip, result.Revision)
	}
	// The commit is Ago's, attributed to Ago, and names the task.
	subject := git(t, repo, "log", "-1", "--format=%s", result.Revision)
	if !strings.Contains(subject, "update-readme") {
		t.Fatalf("commit subject does not name the task: %q", subject)
	}
	if author := git(t, repo, "log", "-1", "--format=%an", result.Revision); author != "Ago" {
		t.Fatalf("commit author = %q, want Ago rather than the user", author)
	}
}

// Two sequential write tasks form a chain, and the second sees the first's work.
func TestSequentialWritesFormAChainAndDownstreamSeesUpstreamWork(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	integrator := newIntegrator(t)
	ref := agointegrate.RefName("board-chain")

	base, err := integrator.EnsureRef(ctx, repo, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	firstPatch, firstBase := makePatch(t, repo, base, map[string]string{
		"README.md": "# fixture\n\n## 快速开始\n",
	})
	first, err := integrator.Integrate(ctx, agointegrate.Request{
		Repository: repo, IntegrationRef: ref, CurrentRevision: base,
		BaseRevision: firstBase, Patch: firstPatch, TaskID: "task-a", Summary: "第一次写入",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The downstream task starts from the integrated tip, so its worktree
	// already contains the first task's change.
	manager, err := agoworktree.New(agoworktree.Options{Root: filepath.Join(t.TempDir(), "worktrees")})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := manager.CreateAt(ctx, repo, "attempt-2", first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	upstream, err := os.ReadFile(filepath.Join(lease.Path, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(upstream), "快速开始") {
		t.Fatalf("the downstream worktree does not contain upstream work: %q", upstream)
	}
	// git does not track empty directories, so the downstream task creates its
	// own, exactly as a real executor would.
	if err := os.MkdirAll(filepath.Join(lease.Path, "docs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "docs", "report.md"), []byte("# 报告\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secondPatch, err := manager.Patch(ctx, lease)
	if err != nil {
		t.Fatal(err)
	}
	base2 := lease.BaseRevision
	if err := manager.Remove(ctx, lease); err != nil {
		t.Fatal(err)
	}

	second, err := integrator.Integrate(ctx, agointegrate.Request{
		Repository: repo, IntegrationRef: ref, CurrentRevision: first.Revision,
		BaseRevision: base2, Patch: secondPatch, TaskID: "task-b", Summary: "第二次写入",
	})
	if err != nil {
		t.Fatalf("second integration: %v", err)
	}

	// Both changes are present at the tip, and the history is a real chain.
	if content := git(t, repo, "show", second.Revision+":README.md"); !strings.Contains(content, "快速开始") {
		t.Fatal("the second commit lost the first task's change")
	}
	if content := git(t, repo, "show", second.Revision+":docs/report.md"); !strings.Contains(content, "报告") {
		t.Fatal("the second commit is missing its own change")
	}
	if parent := git(t, repo, "rev-parse", second.Revision+"^"); parent != first.Revision {
		t.Fatalf("chain broken: parent of second is %s, want %s", parent, first.Revision)
	}
}

// The user's uncommitted work must be irrelevant to integration, and must
// survive it byte-identical.
func TestDirtyCanonicalWorktreeIsNeverTouched(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	integrator := newIntegrator(t)
	ref := agointegrate.RefName("board-dirty")

	base, err := integrator.EnsureRef(ctx, repo, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	patch, patchBase := makePatch(t, repo, base, map[string]string{
		"README.md": "# fixture\n\n## 由 Ago 写入\n",
	})

	// The user is midway through their own edit of the same file.
	userEdit := "# fixture\n\n用户正在编辑，尚未提交。\n"
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte(userEdit), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "scratch.txt"), []byte("未跟踪的草稿\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	statusBefore := git(t, repo, "status", "--porcelain")
	branchBefore := git(t, repo, "rev-parse", "HEAD")

	result, err := integrator.Integrate(ctx, agointegrate.Request{
		Repository: repo, IntegrationRef: ref, CurrentRevision: base,
		BaseRevision: patchBase, Patch: patch, TaskID: "task", Summary: "写入",
	})
	if err != nil {
		t.Fatalf("Integrate: %v", err)
	}

	// The user's file, their untracked scratch, their branch, and their status
	// are all exactly as they left them.
	after, err := os.ReadFile(filepath.Join(repo, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != userEdit {
		t.Fatalf("the user's uncommitted edit was overwritten: %q", after)
	}
	if _, err := os.Stat(filepath.Join(repo, "scratch.txt")); err != nil {
		t.Fatalf("the user's untracked file was removed: %v", err)
	}
	if status := git(t, repo, "status", "--porcelain"); status != statusBefore {
		t.Fatalf("the user's working tree state changed:\nbefore %q\nafter  %q", statusBefore, status)
	}
	if head := git(t, repo, "rev-parse", "HEAD"); head != branchBefore {
		t.Fatalf("the user's branch moved from %s to %s", branchBefore, head)
	}
	if branch := git(t, repo, "rev-parse", "--abbrev-ref", "HEAD"); branch != "main" {
		t.Fatalf("the user was moved off their branch onto %q", branch)
	}
	// And the work is still safely on Ago's ref.
	if tip := git(t, repo, "rev-parse", ref); tip != result.Revision {
		t.Fatalf("integration ref = %s, want %s", tip, result.Revision)
	}
}

// A change that cannot be replayed onto newer work is reported as a conflict,
// never forced over the top of it.
func TestConflictingChangeIsReportedAndNeverForced(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	integrator := newIntegrator(t)
	ref := agointegrate.RefName("board-conflict")

	base, err := integrator.EnsureRef(ctx, repo, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	// Two patches from the same base that rewrite the same line differently.
	first, firstBase := makePatch(t, repo, base, map[string]string{"README.md": "# fixture\n\n第一个版本\n"})
	second, secondBase := makePatch(t, repo, base, map[string]string{"README.md": "# fixture\n\n第二个版本\n"})

	applied, err := integrator.Integrate(ctx, agointegrate.Request{
		Repository: repo, IntegrationRef: ref, CurrentRevision: base,
		BaseRevision: firstBase, Patch: first, TaskID: "task-a", Summary: "第一个",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = integrator.Integrate(ctx, agointegrate.Request{
		Repository: repo, IntegrationRef: ref, CurrentRevision: applied.Revision,
		BaseRevision: secondBase, Patch: second, TaskID: "task-b", Summary: "第二个",
	})
	if !errors.Is(err, agointegrate.ErrConflict) {
		t.Fatalf("Integrate = %v, want a reported conflict", err)
	}
	// The tip is untouched: the conflicting change was not forced over it.
	if tip := git(t, repo, "rev-parse", ref); tip != applied.Revision {
		t.Fatalf("a conflict moved the integration ref to %s, want it left at %s", tip, applied.Revision)
	}
	if content := git(t, repo, "show", applied.Revision+":README.md"); !strings.Contains(content, "第一个版本") {
		t.Fatal("the already-integrated work was overwritten by a conflicting change")
	}
}

// A change based on an older revision that does not overlap newer work is
// replayed onto the tip rather than rejected.
func TestNonOverlappingChangeIsReplayedOntoNewerWork(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	integrator := newIntegrator(t)
	ref := agointegrate.RefName("board-rebase")

	base, err := integrator.EnsureRef(ctx, repo, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	readme, readmeBase := makePatch(t, repo, base, map[string]string{"README.md": "# fixture\n\n说明\n"})
	docs, docsBase := makePatch(t, repo, base, map[string]string{"docs/guide.md": "# 指南\n"})

	first, err := integrator.Integrate(ctx, agointegrate.Request{
		Repository: repo, IntegrationRef: ref, CurrentRevision: base,
		BaseRevision: readmeBase, Patch: readme, TaskID: "task-a", Summary: "README",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := integrator.Integrate(ctx, agointegrate.Request{
		Repository: repo, IntegrationRef: ref, CurrentRevision: first.Revision,
		BaseRevision: docsBase, Patch: docs, TaskID: "task-b", Summary: "指南",
	})
	if err != nil {
		t.Fatalf("a non-overlapping change was not replayed: %v", err)
	}
	if !second.Rebased {
		t.Fatal("the result does not record that the change was replayed onto newer work")
	}
	if content := git(t, repo, "show", second.Revision+":README.md"); !strings.Contains(content, "说明") {
		t.Fatal("replaying lost the earlier change")
	}
	if content := git(t, repo, "show", second.Revision+":docs/guide.md"); !strings.Contains(content, "指南") {
		t.Fatal("replaying lost its own change")
	}
}

// Ago writes only refs it owns. Anything else is refused outright.
func TestOnlyAgoOwnedRefsMayBeWritten(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	integrator := newIntegrator(t)
	mainBefore := git(t, repo, "rev-parse", "main")

	for name, ref := range map[string]string{
		"user branch":  "refs/heads/main",
		"other branch": "refs/heads/feature",
		"tag":          "refs/tags/v1",
		"head":         "HEAD",
		"traversal":    "refs/heads/ago/../../main",
		"bare":         "main",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := integrator.EnsureRef(ctx, repo, ref, ""); err == nil {
				t.Fatalf("EnsureRef accepted %q", ref)
			}
			_, err := integrator.Integrate(ctx, agointegrate.Request{
				Repository: repo, IntegrationRef: ref, Patch: []byte("diff --git a/x b/x\n"),
			})
			if err == nil {
				t.Fatalf("Integrate accepted %q", ref)
			}
		})
	}
	if after := git(t, repo, "rev-parse", "main"); after != mainBefore {
		t.Fatalf("the user's branch moved from %s to %s", mainBefore, after)
	}
}

// An empty patch is a reportable outcome, not a commit with no content.
func TestEmptyPatchIsRefused(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	integrator := newIntegrator(t)
	ref := agointegrate.RefName("board-empty")
	base, err := integrator.EnsureRef(ctx, repo, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, patch := range [][]byte{nil, {}, []byte("   \n")} {
		if _, err := integrator.Integrate(ctx, agointegrate.Request{
			Repository: repo, IntegrationRef: ref, CurrentRevision: base, Patch: patch,
		}); !errors.Is(err, agointegrate.ErrEmptyPatch) {
			t.Fatalf("Integrate(%q) = %v, want ErrEmptyPatch", patch, err)
		}
	}
}

// A commit message is permanent and widely readable, so caller text cannot
// smuggle structure into it.
func TestCommitSubjectCannotForgeStructure(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	integrator := newIntegrator(t)
	ref := agointegrate.RefName("board-subject")
	base, err := integrator.EnsureRef(ctx, repo, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	patch, patchBase := makePatch(t, repo, base, map[string]string{"README.md": "# fixture\n\nx\n"})

	result, err := integrator.Integrate(ctx, agointegrate.Request{
		Repository: repo, IntegrationRef: ref, CurrentRevision: base, BaseRevision: patchBase,
		Patch: patch, TaskID: "task",
		Summary: "正常摘要\n\nCo-Authored-By: Someone <a@b.c>\nfencing_token: deadbeef",
	})
	if err != nil {
		t.Fatal(err)
	}
	message := git(t, repo, "log", "-1", "--format=%B", result.Revision)
	if strings.Contains(message, "Co-Authored-By") || strings.Contains(message, "fencing_token") {
		t.Fatalf("the commit message carried forged structure:\n%s", message)
	}
}

// Integration leaves no scratch worktrees behind, on success or failure.
func TestScratchWorktreesAreCleanedUp(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	root := filepath.Join(t.TempDir(), "integration")
	integrator, err := agointegrate.New(agointegrate.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	ref := agointegrate.RefName("board-clean")
	base, err := integrator.EnsureRef(ctx, repo, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	good, goodBase := makePatch(t, repo, base, map[string]string{"README.md": "# fixture\n\n好的\n"})
	if _, err := integrator.Integrate(ctx, agointegrate.Request{
		Repository: repo, IntegrationRef: ref, CurrentRevision: base,
		BaseRevision: goodBase, Patch: good, TaskID: "ok", Summary: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	// A patch that cannot apply at all.
	_, _ = integrator.Integrate(ctx, agointegrate.Request{
		Repository: repo, IntegrationRef: ref, Patch: []byte("this is not a patch\n"), TaskID: "bad", Summary: "bad",
	})

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%d scratch worktrees were left behind", len(entries))
	}
	// git's own administrative state is clean too, or later integrations break.
	if listing := git(t, repo, "worktree", "list"); strings.Contains(listing, "integrate-") {
		t.Fatalf("git still lists scratch worktrees:\n%s", listing)
	}
}

// Nothing here ever pushes.
func TestIntegrationNeverPushes(t *testing.T) {
	ctx := context.Background()
	repo := newRepository(t)
	// A remote that would fail loudly if anything tried to reach it.
	git(t, repo, "remote", "add", "origin", "file:///nonexistent/ago-must-not-push")
	integrator := newIntegrator(t)
	ref := agointegrate.RefName("board-nopush")
	base, err := integrator.EnsureRef(ctx, repo, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	patch, patchBase := makePatch(t, repo, base, map[string]string{"README.md": "# fixture\n\nx\n"})
	if _, err := integrator.Integrate(ctx, agointegrate.Request{
		Repository: repo, IntegrationRef: ref, CurrentRevision: base,
		BaseRevision: patchBase, Patch: patch, TaskID: "task", Summary: "x",
	}); err != nil {
		t.Fatal(err)
	}
	// If anything had pushed, the bogus remote would have failed the call above.
	// Confirm no remote ref was created locally either.
	if refs := git(t, repo, "for-each-ref", "--format=%(refname)", "refs/remotes"); refs != "" {
		t.Fatalf("integration created remote refs: %q", refs)
	}
}
