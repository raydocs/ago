// Package agointegrate promotes accepted work onto an Ago-owned ref.
//
// It exists because "a verifier accepted this evidence" and "this change is
// part of the project" are different facts. An accepted patch that nobody
// applied is not a finished change, and a worker must not be able to apply its
// own work — so promotion is a separate authority with its own rules:
//
//   - Ago writes only to refs it created (refs/heads/ago/<board>). The user's
//     branch, index, and working tree are never written, reset, or cleaned.
//   - A patch is applied to a scratch worktree checked out at the current
//     integration tip, never to the canonical working tree.
//   - If the tip moved under a patch, the patch is retried against the new tip.
//     A conflict is reported, never forced: silently overwriting someone's work
//     is the one outcome that cannot be undone from the audit trail.
//   - The user's uncommitted work makes no difference, because none of this
//     touches their tree. That is checked by a test, not assumed.
package agointegrate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var (
	// ErrConflict means the change could not be replayed onto the current tip.
	// It is a repairable situation, not a reason to force anything.
	ErrConflict = errors.New("the accepted change conflicts with the integrated revision")
	// ErrEmptyPatch means the change contained nothing to apply.
	ErrEmptyPatch = errors.New("the patch is empty")
)

const commandTimeout = 2 * time.Minute

type Options struct {
	// Root is where scratch integration worktrees live. Ago owns it.
	Root string
	// Author identifies Ago's own commits. It is never the user.
	AuthorName  string
	AuthorEmail string
	Now         func() time.Time
}

type Integrator struct {
	root        string
	authorName  string
	authorEmail string
	now         func() time.Time
}

func New(options Options) (*Integrator, error) {
	if strings.TrimSpace(options.Root) == "" {
		return nil, fmt.Errorf("integration root is required")
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return nil, err
	}
	if options.AuthorName == "" {
		options.AuthorName = "Ago"
	}
	if options.AuthorEmail == "" {
		options.AuthorEmail = "ago@localhost"
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Integrator{root: root, authorName: options.AuthorName, authorEmail: options.AuthorEmail, now: options.Now}, nil
}

// Request is one promotion.
type Request struct {
	Repository string
	// IntegrationRef is the Ago-owned ref to advance, e.g. refs/heads/ago/<board>.
	IntegrationRef string
	// CurrentRevision is the tip the board believes it is at.
	CurrentRevision string
	// BaseRevision is what the patch was produced against. When it differs from
	// CurrentRevision the patch is being replayed onto newer work.
	BaseRevision string
	Patch        []byte
	TaskID       string
	// Summary becomes the commit subject. It is caller-supplied text and is
	// passed as an argument, never interpolated into a shell.
	Summary string
}

// Result is the durable outcome of a promotion.
type Result struct {
	Revision   string
	Rebased    bool
	ChangedRaw string
}

// EnsureRef creates the board's integration ref at the base revision if it does
// not exist yet. It refuses to touch a ref outside Ago's own namespace.
func (integrator *Integrator) EnsureRef(ctx context.Context, repository, ref, baseRevision string) (string, error) {
	if err := validateAgoRef(ref); err != nil {
		return "", err
	}
	if existing, err := integrator.git(ctx, repository, "rev-parse", "--verify", "--quiet", ref); err == nil {
		return strings.TrimSpace(string(existing)), nil
	}
	base := strings.TrimSpace(baseRevision)
	if base == "" {
		resolved, err := integrator.git(ctx, repository, "rev-parse", "HEAD")
		if err != nil {
			return "", fmt.Errorf("resolve base revision: %w", err)
		}
		base = strings.TrimSpace(string(resolved))
	}
	// update-ref writes only the named ref. It does not move HEAD, touch the
	// index, or change the user's working tree.
	if _, err := integrator.git(ctx, repository, "update-ref", ref, base); err != nil {
		return "", fmt.Errorf("create integration ref %q: %w", ref, err)
	}
	return base, nil
}

// Integrate applies an accepted patch and advances the integration ref.
//
// Everything happens in a scratch worktree that is removed afterwards, so the
// user's checkout is untouched whether this succeeds or fails.
func (integrator *Integrator) Integrate(ctx context.Context, request Request) (Result, error) {
	if err := validateAgoRef(request.IntegrationRef); err != nil {
		return Result{}, err
	}
	if len(bytes.TrimSpace(request.Patch)) == 0 {
		return Result{}, ErrEmptyPatch
	}
	tip := strings.TrimSpace(request.CurrentRevision)
	if tip == "" {
		resolved, err := integrator.git(ctx, request.Repository, "rev-parse", request.IntegrationRef)
		if err != nil {
			return Result{}, err
		}
		tip = strings.TrimSpace(string(resolved))
	}

	scratch, err := os.MkdirTemp(integrator.root, "integrate-")
	if err != nil {
		return Result{}, err
	}
	if _, err := integrator.git(ctx, request.Repository, "worktree", "add", "--detach", scratch, tip); err != nil {
		_ = os.RemoveAll(scratch)
		return Result{}, fmt.Errorf("prepare integration worktree: %w", err)
	}
	defer func() {
		// Ago created this worktree, so Ago removes it. Failing to clean up
		// would leave git administrative state that eventually breaks later
		// integrations.
		_, _ = integrator.git(context.WithoutCancel(ctx), request.Repository, "worktree", "remove", "--force", scratch)
		_ = os.RemoveAll(scratch)
	}()

	rebased := strings.TrimSpace(request.BaseRevision) != "" && request.BaseRevision != tip
	if err := integrator.applyPatch(ctx, scratch, request.Patch); err != nil {
		// A patch that will not replay onto newer work is a conflict for a
		// person or a repair task to resolve. It is never forced.
		return Result{}, fmt.Errorf("%w: task %s onto %s: %v", ErrConflict, request.TaskID, short(tip), err)
	}
	if _, err := integrator.git(ctx, scratch, "add", "-A"); err != nil {
		return Result{}, err
	}
	staged, err := integrator.git(ctx, scratch, "diff", "--cached", "--name-only")
	if err != nil {
		return Result{}, err
	}
	if len(bytes.TrimSpace(staged)) == 0 {
		return Result{}, ErrEmptyPatch
	}

	subject := commitSubject(request)
	if _, err := integrator.git(ctx, scratch,
		"-c", "user.name="+integrator.authorName,
		"-c", "user.email="+integrator.authorEmail,
		"commit", "--no-verify", "-m", subject,
	); err != nil {
		return Result{}, fmt.Errorf("record integration commit: %w", err)
	}
	revisionRaw, err := integrator.git(ctx, scratch, "rev-parse", "HEAD")
	if err != nil {
		return Result{}, err
	}
	revision := strings.TrimSpace(string(revisionRaw))

	// Advance the ref only after the commit exists, and only from the tip we
	// started at, so a concurrent integration cannot be silently clobbered.
	if _, err := integrator.git(ctx, request.Repository, "update-ref", request.IntegrationRef, revision, tip); err != nil {
		return Result{}, fmt.Errorf("advance integration ref: %w", err)
	}
	return Result{Revision: revision, Rebased: rebased, ChangedRaw: strings.TrimSpace(string(staged))}, nil
}

// applyPatch replays a patch with a three-way merge, which is what lets an
// accepted change land on top of newer work when the two do not overlap.
func (integrator *Integrator) applyPatch(ctx context.Context, dir string, patch []byte) error {
	command := exec.CommandContext(ctx, "git", "apply", "--3way", "--whitespace=nowarn", "-")
	command.Dir = dir
	command.Stdin = bytes.NewReader(patch)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// commitSubject builds a commit message from caller text. The fencing token and
// any credential are deliberately absent: a commit message is permanent and
// widely readable.
func commitSubject(request Request) string {
	summary := strings.TrimSpace(request.Summary)
	if summary == "" {
		summary = "integrate accepted change"
	}
	// One line only, so caller text cannot forge trailers.
	if index := strings.IndexAny(summary, "\r\n"); index >= 0 {
		summary = summary[:index]
	}
	if len(summary) > 120 {
		summary = summary[:120]
	}
	if request.TaskID != "" {
		return fmt.Sprintf("ago(%s): %s", request.TaskID, summary)
	}
	return "ago: " + summary
}

// validateAgoRef refuses any ref outside Ago's own namespace. This is what
// guarantees Ago never advances the user's branch.
func validateAgoRef(ref string) error {
	if !strings.HasPrefix(ref, "refs/heads/ago/") {
		return fmt.Errorf("refusing to write ref %q: Ago only writes refs under refs/heads/ago/", ref)
	}
	rest := strings.TrimPrefix(ref, "refs/heads/ago/")
	if rest == "" || strings.Contains(rest, "..") || strings.HasSuffix(ref, "/") || strings.ContainsAny(ref, " \t\n~^:?*[\\") {
		return fmt.Errorf("invalid integration ref %q", ref)
	}
	return nil
}

// RefName is the conventional integration ref for a board.
func RefName(boardID string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, boardID)
	return "refs/heads/ago/" + strings.Trim(safe, "-")
}

func (integrator *Integrator) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Kill the whole group: git can spawn helpers that outlive the parent.
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func short(revision string) string {
	if len(revision) > 8 {
		return revision[:8]
	}
	return revision
}
