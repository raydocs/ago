// Package agogate decides whether a goal's integrated result actually holds
// together, by running the project's own checks against it.
//
// This closes the gap that made "complete" a lie. Ago verified every task
// independently and then reported the goal done, which is not the same claim:
// two individually correct changes can combine into something broken. The
// end-to-end test caught that only because the TEST ran the project's suite
// afterwards — the product never did.
//
// Two rules give the gate its meaning, and both are about who writes the exam:
//
//  1. The commands are discovered from the repository or supplied by the user.
//     A model never proposes them. Letting the planner choose what would prove
//     its own plan is the self-certification problem the independent verifier
//     exists to prevent, re-introduced one level up.
//  2. They run against the integrated revision in a clean throwaway worktree,
//     never in a tree any task worked in. A check that passes in a dirty
//     worktree proves nothing about what was promoted.
package agogate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Commands runs one check. It is an interface so tests need no toolchain.
type Commands interface {
	Run(ctx context.Context, dir string, name string, args ...string) (stdout string, exitCode int, err error)
}

// Worktrees provides the clean checkout the gate runs in.
type Worktrees interface {
	// Scratch checks revision out of repository into a fresh directory and
	// returns it along with a function that removes it.
	Scratch(ctx context.Context, repository, revision string) (dir string, remove func(), err error)
}

// Check is one command and what happened when it ran.
type Check struct {
	Command  string `json:"command"`
	ExitCode int    `json:"exit_code"`
	Passed   bool   `json:"passed"`
	// Output is bounded; it exists so a repair task has something to act on.
	Output     string `json:"output,omitempty"`
	DurationMS int64  `json:"duration_ms"`
}

// Result is the whole gate.
type Result struct {
	Revision string  `json:"revision"`
	Checks   []Check `json:"checks"`
	Passed   bool    `json:"passed"`
	// Summary is a single sentence a person can read without opening anything.
	Summary string `json:"summary"`
}

// maxOutputPerCheck bounds what a failing command contributes, so a runaway
// test suite cannot fill the database or a repair prompt.
const maxOutputPerCheck = 16 * 1024

// Discover returns the checks a repository proves itself with.
//
// It is deliberately small and explicit rather than clever. An ecosystem it
// does not recognise gets no commands, and the caller must then decide whether
// a goal without a gate may be reported complete — silently inventing a check
// would be worse than admitting there is none.
func Discover(repositoryRoot string) []string {
	if exists(filepath.Join(repositoryRoot, "go.mod")) {
		return []string{"go build ./...", "go vet ./...", "go test ./..."}
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

type Options struct {
	Commands  Commands
	Worktrees Worktrees
	// Timeout bounds one check.
	Timeout time.Duration
	Now     func() time.Time
}

type Gate struct{ options Options }

const defaultTimeout = 10 * time.Minute

func New(options Options) (*Gate, error) {
	if options.Commands == nil || options.Worktrees == nil {
		return nil, fmt.Errorf("agogate: a command runner and a worktree source are required")
	}
	if options.Timeout <= 0 {
		options.Timeout = defaultTimeout
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Gate{options: options}, nil
}

// Run checks the integrated revision.
//
// Every command runs even after one fails. A goal whose build breaks usually
// also has failing tests, and a repair task aimed at all of it is better than
// one aimed at whichever check happened to be first.
func (gate *Gate) Run(ctx context.Context, repository, revision string, commands []string) (Result, error) {
	if strings.TrimSpace(repository) == "" || strings.TrimSpace(revision) == "" {
		return Result{}, fmt.Errorf("agogate: a repository and a revision are required")
	}
	if len(commands) == 0 {
		return Result{}, fmt.Errorf("agogate: no checks to run")
	}
	dir, remove, err := gate.options.Worktrees.Scratch(ctx, repository, revision)
	if err != nil {
		return Result{}, fmt.Errorf("agogate: prepare a clean checkout of %s: %w", revision, err)
	}
	defer remove()

	result := Result{Revision: revision, Passed: true}
	for _, command := range commands {
		check := gate.run(ctx, dir, command)
		result.Checks = append(result.Checks, check)
		if !check.Passed {
			result.Passed = false
		}
	}
	result.Summary = summarise(result)
	return result, nil
}

func (gate *Gate) run(ctx context.Context, dir, command string) Check {
	name, args, err := splitCommand(command)
	if err != nil {
		// A malformed check is a failed check, not a skipped one. Treating it
		// as absent would let a typo in a gate silently remove the gate.
		return Check{Command: command, Passed: false, ExitCode: -1, Output: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, gate.options.Timeout)
	defer cancel()

	started := gate.options.Now()
	output, exitCode, runErr := gate.options.Commands.Run(ctx, dir, name, args...)
	elapsed := gate.options.Now().Sub(started)

	check := Check{
		Command: command, ExitCode: exitCode,
		Passed:     runErr == nil && exitCode == 0,
		DurationMS: elapsed.Milliseconds(),
	}
	if runErr != nil {
		// The command could not be run at all — missing binary, timeout. That
		// is a gate failure: an unprovable result is not a proven one.
		check.ExitCode = -1
		output = strings.TrimSpace(output + "\n" + runErr.Error())
	}
	if !check.Passed {
		check.Output = bound(output)
	}
	return check
}

// splitCommand refuses anything a shell would interpret.
//
// The gate runs commands, not shell. A gate string carrying a pipe, a
// redirect, or a substitution would be a way to run arbitrary code through
// what is supposed to be a fixed, auditable list of checks.
func splitCommand(command string) (string, []string, error) {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return "", nil, fmt.Errorf("空的检查命令")
	}
	for _, forbidden := range []string{"|", ">", "<", "&", ";", "$", "`", "\n", "\\"} {
		if strings.Contains(trimmed, forbidden) {
			return "", nil, fmt.Errorf("检查命令 %q 含有 shell 元字符 %q；门禁只运行固定命令，不经过 shell", command, forbidden)
		}
	}
	fields := strings.Fields(trimmed)
	return fields[0], fields[1:], nil
}

func bound(output string) string {
	if len(output) <= maxOutputPerCheck {
		return output
	}
	// The end of a failing build or test run is where the reason is.
	return "（前面的输出已截断）\n" + output[len(output)-maxOutputPerCheck:]
}

func summarise(result Result) string {
	if result.Passed {
		return fmt.Sprintf("集成结果 %s 通过全部 %d 项项目门禁。", short(result.Revision), len(result.Checks))
	}
	var failed []string
	for _, check := range result.Checks {
		if !check.Passed {
			failed = append(failed, check.Command)
		}
	}
	return fmt.Sprintf("集成结果 %s 未通过项目门禁：%s。",
		short(result.Revision), strings.Join(failed, "、"))
}

func short(revision string) string {
	if len(revision) > 8 {
		return revision[:8]
	}
	return revision
}
