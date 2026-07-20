// Package agoexec runs a real model against a repository and turns what it did
// into evidence.
//
// The safety model has three parts, and none of them trust the model:
//
//  1. Isolation. Every write attempt gets its own git worktree. The model can
//     only ever see and change that copy, so a canonical working tree cannot be
//     damaged by a bad plan, a confused edit, or a prompt injection.
//  2. Scope. The plan declared which paths a task may touch. What the model
//     actually changed is measured from git afterwards and compared against that
//     declaration. A change outside scope fails the attempt; it is never merged.
//  3. Evidence. The executor reports what it did — changed files with before and
//     after hashes, commands, tests, artifacts — and nothing else. It cannot
//     mark work done; an independent verifier decides that.
//
// The model is reached through a relay client, so no provider credential is
// ever placed in a prompt, a task, or an event.
package agoexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"claudexflow/internal/agoartifact"
	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoredact"
	"claudexflow/internal/agorelay"
	"claudexflow/internal/agoworktree"
)

// Model is the narrow slice of the relay this package needs. Keeping it an
// interface is what lets the tests drive every failure mode deterministically
// without a network.
type Model interface {
	CompleteJSON(ctx context.Context, request agorelay.Request, target any) error
}

// Artifacts stores bounded executor output. It takes a byte stream, never a path.
type Artifacts interface {
	Put(context.Context, agoartifact.PutInput, io.Reader) (agoartifact.Descriptor, error)
}

// Worktrees isolates each write attempt.
type Worktrees interface {
	Create(ctx context.Context, repositoryRoot, attemptID string) (agoworktree.Lease, error)
	CreateAt(ctx context.Context, repositoryRoot, attemptID, revision string) (agoworktree.Lease, error)
	Patch(ctx context.Context, lease agoworktree.Lease) ([]byte, error)
	Changes(ctx context.Context, lease agoworktree.Lease) ([]agoworktree.Change, error)
	Remove(ctx context.Context, lease agoworktree.Lease) error
}

// Commands runs a deterministic check inside a worktree. It exists so the
// executor can prove a task's tests actually ran, rather than believing a model
// that says they did.
type Commands interface {
	Run(ctx context.Context, dir string, name string, args ...string) (stdout string, exitCode int, err error)
}

type Options struct {
	Model     Model
	Worktrees Worktrees
	Artifacts Artifacts
	Commands  Commands
	// MaxEditBytes bounds one file the model may write, so a runaway response
	// cannot fill the disk.
	MaxEditBytes int
	// Timeout bounds one whole attempt.
	Timeout  time.Duration
	Redactor *agoredact.Redactor
	Now      func() time.Time
}

type Executor struct{ options Options }

const (
	defaultMaxEditBytes = 256 * 1024
	defaultTimeout      = 10 * time.Minute
	// maxEditsPerAttempt keeps one response from rewriting a whole repository.
	maxEditsPerAttempt = 32
)

func New(options Options) (*Executor, error) {
	if options.Model == nil || options.Worktrees == nil {
		return nil, fmt.Errorf("executor requires a model and a worktree manager")
	}
	if options.MaxEditBytes <= 0 {
		options.MaxEditBytes = defaultMaxEditBytes
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultTimeout
	}
	if options.Redactor == nil {
		options.Redactor = agoredact.NewFromEnvironment(os.Getenv)
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Executor{options: options}, nil
}

// edit is one file change the model proposes. Paths are repository-relative;
// anything else is rejected before it reaches the filesystem.
type edit struct {
	Path     string `json:"path"`
	Contents string `json:"contents"`
	Delete   bool   `json:"delete,omitempty"`
}

// modelPlan is the structured response the model must return. Prose is not a
// result: the scheduler acts on this, so it has to be machine-readable.
type modelPlan struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
	Edits   []edit `json:"edits"`
	// Commands the model believes should be run to demonstrate the work. They
	// are run by Ago, not by the model, and their real exit codes are recorded.
	Commands []string `json:"commands,omitempty"`
	Risks    []string `json:"risks,omitempty"`
	// NeedsInput lets a model stop honestly instead of guessing.
	NeedsInput string `json:"needs_input,omitempty"`
}

// Error is a classified executor failure the scheduler can act on.
type Error struct {
	Class   agoboardprotocol.FailureClass
	Message string
}

func (e Error) Error() string                               { return e.Message }
func (e Error) FailureClass() agoboardprotocol.FailureClass { return e.Class }

// Execute runs one attempt and returns evidence. It never decides acceptance.
func (executor *Executor) Execute(ctx context.Context, dispatch agoboardruntime.Dispatch) (agoboardruntime.ExecutionResult, error) {
	ctx, cancel := context.WithTimeout(ctx, executor.options.Timeout)
	defer cancel()

	repository := dispatch.Goal.Repository.ID
	// Start from the board's integrated revision so this task inherits work its
	// dependencies already had accepted and promoted.
	lease, err := executor.options.Worktrees.CreateAt(ctx, repository, dispatch.AttemptID, dispatch.BaseRevision)
	if err != nil {
		return agoboardruntime.ExecutionResult{}, Error{
			Class:   agoboardprotocol.FailureRepository,
			Message: executor.redact(fmt.Sprintf("无法为该尝试创建隔离工作区：%v", err)),
		}
	}
	// The worktree is Ago's, and it is removed whatever happens. A failed
	// attempt must not leave a half-edited copy behind for the next one to
	// inherit.
	defer func() {
		_ = executor.options.Worktrees.Remove(context.WithoutCancel(ctx), lease)
	}()

	plan, err := executor.ask(ctx, dispatch, lease)
	if err != nil {
		return agoboardruntime.ExecutionResult{}, err
	}
	if strings.TrimSpace(plan.NeedsInput) != "" {
		return agoboardruntime.ExecutionResult{}, Error{
			Class:   agoboardprotocol.FailureNeedsInput,
			Message: executor.redact(plan.NeedsInput),
		}
	}
	if err := executor.applyEdits(lease, dispatch, plan.Edits); err != nil {
		return agoboardruntime.ExecutionResult{}, err
	}

	changes, err := executor.options.Worktrees.Changes(ctx, lease)
	if err != nil {
		return agoboardruntime.ExecutionResult{}, Error{
			Class:   agoboardprotocol.FailureRepository,
			Message: executor.redact(fmt.Sprintf("无法读取工作区变更：%v", err)),
		}
	}
	// What the model actually changed is measured, not asked. A model that
	// claimed one thing and did another is caught here.
	if violations := agoworktree.ViolatesScope(changes, dispatch.Task.PathScopes); len(violations) > 0 {
		return agoboardruntime.ExecutionResult{}, Error{
			Class: agoboardprotocol.FailurePolicy,
			Message: executor.redact(fmt.Sprintf(
				"修改超出允许范围 %v：%v。该结果已被丢弃，不会合入仓库。",
				dispatch.Task.PathScopes, violations)),
		}
	}

	result := agoboardprotocol.EvidenceResult{
		Summary:      executor.redact(plan.Summary),
		Warnings:     executor.options.Redactor.Strings(plan.Risks),
		ChangedFiles: toChangedFiles(changes),
	}
	executor.runCommands(ctx, lease, dispatch, plan.Commands, &result)

	// The commands just ran arbitrary model-authored code inside the worktree.
	// Whatever they wrote is in the tree and would be in the patch, so the
	// scope gate has to be applied again to the state the patch will actually
	// capture. Checking only before the commands would let `node build.js` or
	// `make` write anywhere while the evidence named one in-scope file.
	changes, err = executor.options.Worktrees.Changes(ctx, lease)
	if err != nil {
		return agoboardruntime.ExecutionResult{}, Error{
			Class:   agoboardprotocol.FailureRepository,
			Message: executor.redact(fmt.Sprintf("无法读取命令执行后的变更：%v", err)),
		}
	}
	if violations := agoworktree.ViolatesScope(changes, dispatch.Task.PathScopes); len(violations) > 0 {
		return agoboardruntime.ExecutionResult{}, Error{
			Class: agoboardprotocol.FailurePolicy,
			Message: executor.redact(fmt.Sprintf(
				"验证命令写入了允许范围 %v 之外的文件：%v。该结果已被丢弃。",
				dispatch.Task.PathScopes, violations)),
		}
	}
	// The evidence must describe the tree the patch contains, not the tree as
	// it was before the commands ran.
	result.ChangedFiles = toChangedFiles(changes)

	// The worktree is deleted the moment this returns, so the change has to be
	// captured as durable bytes first. Without this an accepted result would
	// simply vanish.
	if len(changes) > 0 {
		patch, patchErr := executor.options.Worktrees.Patch(ctx, lease)
		if patchErr != nil {
			return agoboardruntime.ExecutionResult{}, Error{
				Class:   agoboardprotocol.FailureRepository,
				Message: executor.redact(fmt.Sprintf("无法保存变更补丁：%v", patchErr)),
			}
		}
		if executor.options.Artifacts == nil {
			return agoboardruntime.ExecutionResult{}, Error{
				Class:   agoboardprotocol.FailureRepository,
				Message: "没有配置工件存储，变更无法持久化。",
			}
		}
		descriptor, putErr := executor.options.Artifacts.Put(ctx, agoartifact.PutInput{
			Type: "text/x-patch", DisplayName: dispatch.Task.ID + ".patch",
		}, bytes.NewReader(patch))
		if putErr != nil {
			return agoboardruntime.ExecutionResult{}, Error{
				Class:   agoboardprotocol.FailureRepository,
				Message: executor.redact(fmt.Sprintf("无法存储变更补丁：%v", putErr)),
			}
		}
		paths := make([]string, 0, len(changes))
		for _, change := range changes {
			paths = append(paths, change.Path)
		}
		result.Patch = &agoboardprotocol.PatchRecord{
			ArtifactID: descriptor.ID, BaseRevision: lease.BaseRevision,
			SHA256: descriptor.SHA256, Bytes: descriptor.Bytes, ChangedPaths: paths,
		}
		result.Artifacts = append(result.Artifacts, agoboardprotocol.ArtifactRef{
			ID: descriptor.ID, Type: descriptor.Type, DisplayName: descriptor.DisplayName,
			Bytes: descriptor.Bytes, SHA256: descriptor.SHA256,
		})
	}

	if result.Summary == "" {
		return agoboardruntime.ExecutionResult{}, Error{
			Class:   agoboardprotocol.FailureTransient,
			Message: "模型没有返回可用的结果摘要。",
		}
	}
	return agoboardruntime.ExecutionResult{
		Artifact: "worktree://" + lease.ID,
		Summary:  result.Summary,
		Result:   result,
	}, nil
}

// ask sends the task contract and returns the model's structured plan.
//
// The prompt carries the goal, the task, its acceptance criteria, and its path
// scope. It deliberately carries no fencing token and no provider credential:
// the token authorizes Ago's own commands and has no business in a prompt.
func (executor *Executor) ask(ctx context.Context, dispatch agoboardruntime.Dispatch, lease agoworktree.Lease) (modelPlan, error) {
	listing, err := repositoryListing(lease.Path, dispatch.Task.PathScopes)
	if err != nil {
		return modelPlan{}, Error{
			Class:   agoboardprotocol.FailureRepository,
			Message: executor.redact(fmt.Sprintf("无法读取仓库内容：%v", err)),
		}
	}
	request := agorelay.Request{
		System: "你是一个严谨的软件工程执行器。只返回一个 JSON 对象，不要任何解释文字。" +
			"只修改允许范围内的文件。如果信息不足以安全完成任务，把原因写进 needs_input 并且不要编造修改。",
		User: buildContract(dispatch, listing),
	}
	var plan modelPlan
	if err := executor.options.Model.CompleteJSON(ctx, request, &plan); err != nil {
		// A relay that says the request may be retried is a transient fault; a
		// refusal or a malformed response is not something retrying will fix.
		class := agoboardprotocol.FailureTransient
		var status agorelay.StatusError
		if errors.As(err, &status) && !status.Retryable() {
			class = agoboardprotocol.FailurePermanent
		}
		if errors.Is(err, context.DeadlineExceeded) {
			class = agoboardprotocol.FailureTransient
		}
		return modelPlan{}, Error{Class: class, Message: executor.redact(fmt.Sprintf("模型调用失败：%v", err))}
	}
	if len(plan.Edits) > maxEditsPerAttempt {
		return modelPlan{}, Error{
			Class:   agoboardprotocol.FailurePolicy,
			Message: fmt.Sprintf("模型提出了 %d 处修改，超过单次尝试上限 %d。", len(plan.Edits), maxEditsPerAttempt),
		}
	}
	return plan, nil
}

func buildContract(dispatch agoboardruntime.Dispatch, listing string) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "总目标：%s\n\n", dispatch.Goal.Objective.Summary)
	fmt.Fprintf(&builder, "当前任务：%s\n%s\n\n", dispatch.Task.Title, dispatch.Task.Description)
	builder.WriteString("验收标准：\n")
	for _, criterion := range dispatch.Task.AcceptanceCriteria {
		fmt.Fprintf(&builder, "- %s\n", criterion)
	}
	fmt.Fprintf(&builder, "\n允许修改的路径（超出即失败）：%s\n", strings.Join(dispatch.Task.PathScopes, ", "))
	fmt.Fprintf(&builder, "这是第 %d 次尝试。\n\n", dispatch.AttemptNumber)
	builder.WriteString("仓库当前内容：\n")
	builder.WriteString(listing)
	builder.WriteString("\n返回这个形状的 JSON：\n")
	builder.WriteString(`{"status":"completed","summary":"<中文摘要>","edits":[{"path":"<仓库相对路径>","contents":"<完整新内容>"}],"commands":["<可选的验证命令>"],"risks":[],"needs_input":""}`)
	return builder.String()
}

// applyEdits writes the model's changes into the isolated worktree.
//
// Every path is validated before it is used: an absolute path, a parent
// reference, or anything that resolves outside the worktree is refused. This is
// belt and braces — the scope check afterwards would also catch it — because a
// write that escaped would already have happened by then.
func (executor *Executor) applyEdits(lease agoworktree.Lease, dispatch agoboardruntime.Dispatch, edits []edit) error {
	for _, item := range edits {
		clean := filepath.Clean(item.Path)
		if item.Path == "" || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return Error{
				Class:   agoboardprotocol.FailurePolicy,
				Message: fmt.Sprintf("模型试图写入非法路径 %q。", item.Path),
			}
		}
		target := filepath.Join(lease.Path, clean)
		// Resolve the parent and confirm it is still inside the worktree, so a
		// symlink planted by an earlier edit cannot redirect a later write.
		parent := filepath.Dir(target)
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return Error{Class: agoboardprotocol.FailureRepository, Message: executor.redact(err.Error())}
		}
		resolvedParent, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return Error{Class: agoboardprotocol.FailureRepository, Message: executor.redact(err.Error())}
		}
		resolvedRoot, err := filepath.EvalSymlinks(lease.Path)
		if err != nil {
			return Error{Class: agoboardprotocol.FailureRepository, Message: executor.redact(err.Error())}
		}
		if resolvedParent != resolvedRoot && !strings.HasPrefix(resolvedParent, resolvedRoot+string(filepath.Separator)) {
			return Error{
				Class:   agoboardprotocol.FailurePolicy,
				Message: fmt.Sprintf("写入 %q 会离开隔离工作区。", item.Path),
			}
		}
		if item.Delete {
			if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
				return Error{Class: agoboardprotocol.FailureRepository, Message: executor.redact(err.Error())}
			}
			continue
		}
		if len(item.Contents) > executor.options.MaxEditBytes {
			return Error{
				Class:   agoboardprotocol.FailurePolicy,
				Message: fmt.Sprintf("对 %q 的写入为 %d 字节，超过上限 %d。", item.Path, len(item.Contents), executor.options.MaxEditBytes),
			}
		}
		if err := os.WriteFile(target, []byte(item.Contents), 0o600); err != nil {
			return Error{Class: agoboardprotocol.FailureRepository, Message: executor.redact(err.Error())}
		}
	}
	return nil
}

// runCommands runs the checks inside the worktree and records what actually
// happened. A model saying the tests pass is not evidence; an exit code is.
func (executor *Executor) runCommands(ctx context.Context, lease agoworktree.Lease, dispatch agoboardruntime.Dispatch, commands []string, result *agoboardprotocol.EvidenceResult) {
	if executor.options.Commands == nil {
		return
	}
	for index, display := range commands {
		if index >= 4 {
			result.Warnings = append(result.Warnings, "只运行了前 4 条验证命令。")
			break
		}
		name, args, ok := safeCommand(display)
		if !ok {
			result.Warnings = append(result.Warnings, executor.redact(fmt.Sprintf("拒绝执行不在允许列表中的命令：%s", display)))
			continue
		}
		started := executor.options.Now()
		stdout, exitCode, err := executor.options.Commands.Run(ctx, lease.Path, name, args...)
		record := agoboardprotocol.CommandRecord{
			Display:    executor.redact(display),
			ExitCode:   exitCode,
			DurationMS: executor.options.Now().Sub(started).Milliseconds(),
		}
		if err != nil && exitCode == 0 {
			// The command could not be run at all, which is not a test result.
			result.Warnings = append(result.Warnings, executor.redact(fmt.Sprintf("命令未能执行：%s：%v", display, err)))
			continue
		}
		if executor.options.Artifacts != nil {
			if descriptor, putErr := executor.options.Artifacts.Put(ctx, agoartifact.PutInput{
				Type: "text/plain; charset=utf-8", DisplayName: name + "-output.log",
			}, executor.options.Redactor.Reader(strings.NewReader(stdout))); putErr == nil {
				record.OutputArtifactID = descriptor.ID
				result.Artifacts = append(result.Artifacts, agoboardprotocol.ArtifactRef{
					ID: descriptor.ID, Type: descriptor.Type, DisplayName: descriptor.DisplayName,
					Bytes: descriptor.Bytes, SHA256: descriptor.SHA256,
				})
			}
		}
		result.Commands = append(result.Commands, record)
		result.Tests = append(result.Tests, agoboardprotocol.TestRecord{
			Name: display, Command: executor.redact(display),
			Passed: exitCode == 0, ExitCode: exitCode, Required: true,
		})
	}
}

// safeCommand allows only a small set of read-only or build-and-test commands.
// Anything that publishes, deletes, or rewrites history is refused outright
// rather than being approved case by case at runtime.
func safeCommand(display string) (string, []string, bool) {
	fields := strings.Fields(display)
	if len(fields) == 0 {
		return "", nil, false
	}
	allowed := map[string]bool{"go": true, "npm": true, "node": true, "make": true, "cat": true, "ls": true}
	if !allowed[fields[0]] {
		return "", nil, false
	}
	forbidden := []string{"push", "publish", "deploy", "reset", "clean", "restore", "rm", "-rf", "sudo", "curl", "wget"}
	for _, field := range fields {
		for _, bad := range forbidden {
			if strings.EqualFold(field, bad) {
				return "", nil, false
			}
		}
	}
	// Shell metacharacters would let a single allowed word smuggle in anything.
	if strings.ContainsAny(display, "&|;><`$\n") {
		return "", nil, false
	}
	return fields[0], fields[1:], true
}

func toChangedFiles(changes []agoworktree.Change) []agoboardprotocol.ChangedFile {
	files := make([]agoboardprotocol.ChangedFile, 0, len(changes))
	for _, change := range changes {
		files = append(files, agoboardprotocol.ChangedFile{
			Path: change.Path, BeforeHash: change.BeforeHash, AfterHash: change.AfterHash,
		})
	}
	return files
}

// repositoryListing gives the model the content it is allowed to work with. It
// is bounded so a large repository cannot produce an unbounded prompt.
func repositoryListing(root string, scopes []string) (string, error) {
	var builder strings.Builder
	budget := 24 * 1024
	for _, scope := range scopes {
		target := filepath.Join(root, filepath.Clean(scope))
		info, err := os.Stat(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if info.IsDir() {
			entries, err := os.ReadDir(target)
			if err != nil {
				return "", err
			}
			for _, entry := range entries {
				fmt.Fprintf(&builder, "- %s/%s\n", scope, entry.Name())
			}
			continue
		}
		content, err := os.ReadFile(target)
		if err != nil {
			return "", err
		}
		if len(content) > budget {
			content = content[:budget]
		}
		budget -= len(content)
		fmt.Fprintf(&builder, "--- %s ---\n%s\n", scope, content)
		if budget <= 0 {
			builder.WriteString("（内容已截断）\n")
			break
		}
	}
	return builder.String(), nil
}

func (executor *Executor) redact(value string) string {
	return executor.options.Redactor.String(value)
}

// SystemCommands runs a check inside a worktree as a real process.
//
// It is deliberately thin: the allowlist and the argument validation live in
// the executor, so this only spawns what it is given. The process group is
// killed on cancellation, because a test runner that spawns children would
// otherwise outlive the attempt.
type SystemCommands struct {
	// MaxOutputBytes bounds captured output so a runaway command cannot fill
	// memory. Zero uses 256 KiB.
	MaxOutputBytes int
}

func (commands SystemCommands) Run(ctx context.Context, dir, name string, args ...string) (string, int, error) {
	limit := commands.MaxOutputBytes
	if limit <= 0 {
		limit = 256 * 1024
	}
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	// A minimal environment: a child gets what it needs to run, not the
	// operator's credentials.
	command.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + dir,
		"LANG=en_US.UTF-8",
	}
	var buffer bytes.Buffer
	writer := &boundedWriter{limit: limit, buffer: &buffer}
	command.Stdout, command.Stderr = writer, writer
	err := command.Run()
	exitCode := 0
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
		err = nil
	}
	return buffer.String(), exitCode, err
}

// boundedWriter stops accepting output once the limit is reached, so a command
// that never stops printing cannot exhaust memory.
type boundedWriter struct {
	limit   int
	written int
	buffer  *bytes.Buffer
}

func (writer *boundedWriter) Write(p []byte) (int, error) {
	remaining := writer.limit - writer.written
	if remaining <= 0 {
		// Report the bytes as consumed so the command is not killed by a short
		// write; the output is simply not retained.
		return len(p), nil
	}
	if len(p) > remaining {
		p = p[:remaining]
	}
	written, err := writer.buffer.Write(p)
	writer.written += written
	return len(p), err
}
