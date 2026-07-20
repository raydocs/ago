// Package agofake provides the deterministic offline executor and verifier.
//
// It performs no network access, reads no credentials, and touches no
// repository. Its behaviour is a pure function of the scripted outcome and the
// durable attempt number, so a demo or a CI run reproduces exactly.
//
// The split of authority is the same as production: the executor only produces
// evidence, and a separate verifier decides acceptance. The fake never writes
// task state directly.
package agofake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"claudexflow/internal/agoartifact"
	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
)

// Outcome is a scripted behaviour for one task.
type Outcome string

const (
	// OutcomeSuccess executes cleanly and is accepted.
	OutcomeSuccess Outcome = "success"
	// OutcomeTemporaryFailureThenSuccess fails the first attempt with a
	// retryable fault and succeeds on the next one.
	OutcomeTemporaryFailureThenSuccess Outcome = "temporary_failure_then_success"
	// OutcomePermanentFailure fails in a way retrying cannot fix.
	OutcomePermanentFailure Outcome = "permanent_failure"
	// OutcomeTimeout models a bounded executor that ran out of time. A timeout
	// is retryable: the work may simply not have finished.
	OutcomeTimeout Outcome = "timeout"
	// OutcomeVerifierRetryWithFeedback produces evidence the verifier rejects
	// with actionable feedback, so the next attempt can correct it.
	OutcomeVerifierRetryWithFeedback Outcome = "verifier_retry_with_feedback"
	// OutcomeBlockedNeedsInput stops for a user decision.
	OutcomeBlockedNeedsInput Outcome = "blocked_needs_input"
	// OutcomeBlockedPolicy stops because policy forbids the work.
	OutcomeBlockedPolicy Outcome = "blocked_policy"
)

// Valid reports whether an outcome is one this provider knows how to script.
func (outcome Outcome) Valid() bool {
	switch outcome {
	case OutcomeSuccess, OutcomeTemporaryFailureThenSuccess, OutcomePermanentFailure,
		OutcomeTimeout, OutcomeVerifierRetryWithFeedback, OutcomeBlockedNeedsInput, OutcomeBlockedPolicy:
		return true
	default:
		return false
	}
}

// Script selects behaviour per task, falling back to Default.
type Script struct {
	Default Outcome
	ByTask  map[string]Outcome
}

func (script Script) outcomeFor(taskID string) Outcome {
	if outcome, found := script.ByTask[taskID]; found {
		return outcome
	}
	if script.Default == "" {
		return OutcomeSuccess
	}
	return script.Default
}

// Error is a classified executor failure. The scheduler reads FailureClass to
// decide whether the task may be retried, so the fake states its intent rather
// than leaving it to be guessed.
type Error struct {
	Class   agoboardprotocol.FailureClass
	Message string
}

func (e Error) Error() string { return e.Message }

// FailureClass satisfies the interface the scheduler uses to classify errors.
func (e Error) FailureClass() agoboardprotocol.FailureClass { return e.Class }

// ArtifactWriter is the narrow slice of the artifact store the fake needs. It
// takes a byte stream, never a path.
type ArtifactWriter interface {
	Put(context.Context, agoartifact.PutInput, io.Reader) (agoartifact.Descriptor, error)
}

// Provider implements both the executor and the verifier boundaries. They are
// separate interfaces registered under separate identities, so the state
// machine still refuses a worker's attempt to review its own evidence.
type Provider struct {
	script    Script
	artifacts ArtifactWriter
}

// WithArtifacts makes the fake store its command output as managed artifacts,
// so evidence references real bytes rather than describing them.
func (provider *Provider) WithArtifacts(writer ArtifactWriter) *Provider {
	provider.artifacts = writer
	return provider
}

func New(script Script) (*Provider, error) {
	if script.Default != "" && !script.Default.Valid() {
		return nil, fmt.Errorf("unknown default outcome %q", script.Default)
	}
	for taskID, outcome := range script.ByTask {
		if !outcome.Valid() {
			return nil, fmt.Errorf("task %q has unknown outcome %q", taskID, outcome)
		}
	}
	return &Provider{script: script}, nil
}

// Execute produces evidence or a classified failure. It never decides whether
// the task is done.
func (provider *Provider) Execute(ctx context.Context, dispatch agoboardruntime.Dispatch) (agoboardruntime.ExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return agoboardruntime.ExecutionResult{}, err
	}
	outcome := provider.script.outcomeFor(dispatch.Task.ID)
	switch outcome {
	case OutcomePermanentFailure:
		return agoboardruntime.ExecutionResult{}, Error{
			Class:   agoboardprotocol.FailurePermanent,
			Message: fmt.Sprintf("任务《%s》遇到无法通过重试修复的错误。", dispatch.Task.Title),
		}
	case OutcomeTimeout:
		return agoboardruntime.ExecutionResult{}, Error{
			Class:   agoboardprotocol.FailureTransient,
			Message: fmt.Sprintf("任务《%s》执行超时，已取消。", dispatch.Task.Title),
		}
	case OutcomeBlockedNeedsInput:
		return agoboardruntime.ExecutionResult{}, Error{
			Class:   agoboardprotocol.FailureNeedsInput,
			Message: fmt.Sprintf("任务《%s》需要用户补充信息后才能继续。", dispatch.Task.Title),
		}
	case OutcomeBlockedPolicy:
		return agoboardruntime.ExecutionResult{}, Error{
			Class:   agoboardprotocol.FailurePolicy,
			Message: fmt.Sprintf("任务《%s》被策略拒绝，需要显式授权。", dispatch.Task.Title),
		}
	case OutcomeTemporaryFailureThenSuccess:
		// The decision reads the durable attempt number, so a restart in the
		// middle of a retry does not change what happens next.
		if dispatch.AttemptNumber <= 1 {
			return agoboardruntime.ExecutionResult{}, Error{
				Class:   agoboardprotocol.FailureTransient,
				Message: fmt.Sprintf("任务《%s》第一次执行遇到临时故障。", dispatch.Task.Title),
			}
		}
	}
	summary := fmt.Sprintf("任务《%s》在第 %d 次尝试完成。", dispatch.Task.Title, dispatch.AttemptNumber)
	testCommand := "go test ./..."
	// A verifier-feedback run deliberately reports an unsatisfied required
	// check on its first attempt, so the deterministic gate is what stops it.
	requiredPassed := true
	if outcome == OutcomeVerifierRetryWithFeedback && dispatch.AttemptNumber <= 1 {
		requiredPassed = false
	}
	result := agoboardprotocol.EvidenceResult{
		Summary: summary,
		ChangedFiles: []agoboardprotocol.ChangedFile{{
			Path:       changedPathFor(dispatch),
			BeforeHash: deterministicHash("before", dispatch.Task.ID),
			AfterHash:  deterministicHash("after", dispatch.Task.ID, fmt.Sprint(dispatch.AttemptNumber)),
		}},
		Commands: []agoboardprotocol.CommandRecord{{
			Display: testCommand, ExitCode: exitCodeFor(requiredPassed), DurationMS: 120,
		}},
		Tests: []agoboardprotocol.TestRecord{{
			Name: "任务验收测试", Command: testCommand,
			Passed: requiredPassed, ExitCode: exitCodeFor(requiredPassed), Required: true,
		}},
	}
	if provider.artifacts != nil {
		log := fmt.Sprintf("$ %s\nexit code: %d\n任务《%s》第 %d 次尝试的输出。\n",
			testCommand, exitCodeFor(requiredPassed), dispatch.Task.Title, dispatch.AttemptNumber)
		descriptor, err := provider.artifacts.Put(ctx, agoartifact.PutInput{
			Type: "text/plain; charset=utf-8", DisplayName: dispatch.Task.ID + "-output.log",
		}, strings.NewReader(log))
		if err != nil {
			return agoboardruntime.ExecutionResult{}, Error{
				Class:   agoboardprotocol.FailureTransient,
				Message: fmt.Sprintf("保存执行输出失败：%v", err),
			}
		}
		result.Commands[0].OutputArtifactID = descriptor.ID
		result.Artifacts = append(result.Artifacts, agoboardprotocol.ArtifactRef{
			ID: descriptor.ID, Type: descriptor.Type, DisplayName: descriptor.DisplayName,
			Bytes: descriptor.Bytes, SHA256: descriptor.SHA256,
		})
	}
	return agoboardruntime.ExecutionResult{
		Artifact: fmt.Sprintf("artifact://ago-fake/%s/%d", dispatch.Task.ID, dispatch.AttemptNumber),
		Summary:  summary,
		Result:   result,
	}, nil
}

// changedPathFor keeps the declared change inside the task's own path scope, so
// evidence never describes work outside what the plan authorized.
func changedPathFor(dispatch agoboardruntime.Dispatch) string {
	if len(dispatch.Task.PathScopes) > 0 {
		return dispatch.Task.PathScopes[0]
	}
	return "README.md"
}

func exitCodeFor(passed bool) int {
	if passed {
		return 0
	}
	return 1
}

func deterministicHash(parts ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(digest[:])
}

// Verify is deliberately absent. This type is the EXECUTOR. Acceptance belongs
// to agofake.Verifier, which reads persisted evidence and has no access to what
// the executor was thinking — see verifier.go.
