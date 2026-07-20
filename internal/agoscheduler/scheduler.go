// Package agoscheduler owns work admission for the durable Work Graph.
//
// It is the only authority that claims tasks. Executors run the attempt they
// were dispatched with and submit evidence; an independent verifier decides
// acceptance. Every scheduling decision is made from committed SQLite state
// inside a transaction, so a second scheduler — in this process or another —
// cannot duplicate a claim or exceed a concurrency limit.
package agoscheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"claudexflow/internal/agoartifact"
	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agointegrate"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agoredact"
)

// DefaultLimits are the safe initial concurrency bounds.
var DefaultLimits = agoboardstore.SlotLimits{
	GlobalRunning:     3,
	BoardRunning:      2,
	RepositoryWriters: 1,
	RepositoryReaders: 2,
}

type Options struct {
	Store    *agoboardstore.Store
	Runtime  *agoboardruntime.Runtime
	Executor agoboardruntime.Executor
	Verifier agoboardruntime.Verifier

	CoordinatorID string
	WorkerID      string
	VerifierID    string

	LeaseDuration time.Duration
	Limits        agoboardstore.SlotLimits

	// Now is the injected clock. Every deadline, backoff, and expiry decision
	// reads it, so tests advance time instead of sleeping.
	Now func() time.Time
	// Interval paces Run. Ticks are delivered by Ticker when it is supplied.
	Interval time.Duration
	// Ticker lets a test drive Run deterministically. When nil, Run uses a real
	// time.Ticker at Interval.
	Ticker <-chan time.Time
	// OnCycle observes each completed cycle. It is intended for tests and
	// diagnostics and must not block.
	OnCycle func(Cycle)
	// Integrator promotes accepted changes onto the board's Ago-owned ref. When
	// nil, a task whose evidence carries a patch cannot complete: an accepted
	// change nobody applied must not be reported as finished work.
	Integrator Integrator
	// Redactor strips credential-shaped content from executor output before it
	// becomes durable evidence. A nil value is replaced with one seeded from
	// the process environment, so redaction is never accidentally off.
	Redactor *agoredact.Redactor
	// Artifacts reads back the durable patch an accepted attempt produced.
	Artifacts ArtifactReader
}

// Integrator is the narrow slice of the integration authority the scheduler
// needs. It is an interface so the scheduler never gains the ability to write
// a repository itself.
// ArtifactReader reads stored bytes back. The scheduler needs the patch, not
// the ability to write artifacts.
type ArtifactReader interface {
	Bytes(ctx context.Context, descriptor agoartifact.Descriptor) ([]byte, error)
}

type Integrator interface {
	EnsureRef(ctx context.Context, repository, ref, baseRevision string) (string, error)
	Integrate(ctx context.Context, request agointegrate.Request) (agointegrate.Result, error)
}

// Cycle records what one scheduler pass did.
type Cycle struct {
	Reconciled int
	Claimed    int
	Dispatched int
	Outcomes   []agoboardstore.ClaimOutcome
}

type Scheduler struct {
	options Options
}

func New(options Options) (*Scheduler, error) {
	if options.Store == nil || options.Runtime == nil || options.Executor == nil || options.Verifier == nil {
		return nil, fmt.Errorf("scheduler requires a store, runtime, executor, and verifier")
	}
	if options.CoordinatorID == "" || options.WorkerID == "" || options.VerifierID == "" {
		return nil, fmt.Errorf("scheduler requires coordinator, worker, and verifier identities")
	}
	if options.WorkerID == options.VerifierID {
		return nil, fmt.Errorf("the verifier must be independent from the worker")
	}
	if options.LeaseDuration <= 0 {
		return nil, fmt.Errorf("scheduler requires a positive lease duration")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Interval <= 0 {
		options.Interval = 250 * time.Millisecond
	}
	if options.Limits == (agoboardstore.SlotLimits{}) {
		options.Limits = DefaultLimits
	}
	if options.Redactor == nil {
		options.Redactor = agoredact.NewFromEnvironment(os.Getenv)
	}
	return &Scheduler{options: options}, nil
}

// maxClaimsPerCycle bounds one pass so a scheduler cannot spin indefinitely on
// a board whose work keeps becoming eligible.
const maxClaimsPerCycle = 16

// RunOnce performs one deterministic scheduler cycle across every board.
func (scheduler *Scheduler) RunOnce(ctx context.Context) (Cycle, error) {
	return scheduler.runCycle(ctx, "")
}

// RunOnceForBoard is RunOnce scoped to a single board.
//
// A caller that already iterates boards must use this: calling RunOnce per
// board would make one pass quadratic, because each call would sweep the whole
// fleet again. Reconciliation still runs fleet-wide, since an expired lease
// anywhere holds a concurrency slot everywhere.
func (scheduler *Scheduler) RunOnceForBoard(ctx context.Context, boardID string) (Cycle, error) {
	return scheduler.runCycle(ctx, boardID)
}

func (scheduler *Scheduler) runCycle(ctx context.Context, onlyBoard string) (Cycle, error) {
	cycle := Cycle{Outcomes: []agoboardstore.ClaimOutcome{}}
	now := scheduler.options.Now().UTC()

	// Reconciliation runs before admission so a lease whose deadline elapsed
	// releases its task in the same pass that might re-claim it.
	expired, err := scheduler.options.Store.ExpireDueLeases(ctx, now, scheduler.coordinator())
	if err != nil {
		return cycle, fmt.Errorf("reconcile expired leases: %w", err)
	}
	cycle.Reconciled = len(expired)

	boards := []string{onlyBoard}
	if onlyBoard == "" {
		listed, err := scheduler.options.Store.BoardIDs(ctx)
		if err != nil {
			return cycle, fmt.Errorf("list boards: %w", err)
		}
		boards = listed
	}
	for _, boardID := range boards {
		if err := ctx.Err(); err != nil {
			return cycle, err
		}
		for range maxClaimsPerCycle {
			claimed, outcome, err := scheduler.claimAndDispatch(ctx, boardID)
			if err != nil {
				return cycle, err
			}
			cycle.Outcomes = append(cycle.Outcomes, outcome)
			if !claimed {
				break
			}
			cycle.Claimed++
			cycle.Dispatched++
		}
	}
	if scheduler.options.OnCycle != nil {
		scheduler.options.OnCycle(cycle)
	}
	return cycle, nil
}

// Run drives cycles until the context is cancelled, then returns nil. A cycle
// in progress is allowed to finish, so Run never abandons a claimed attempt.
func (scheduler *Scheduler) Run(ctx context.Context) error {
	ticks := scheduler.options.Ticker
	if ticks == nil {
		ticker := time.NewTicker(scheduler.options.Interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, open := <-ticks:
			if !open {
				return nil
			}
			if _, err := scheduler.RunOnce(ctx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil
				}
				// A cycle failure must not kill the scheduler: the next tick
				// re-reads durable state and retries.
				if scheduler.options.OnCycle != nil {
					scheduler.options.OnCycle(Cycle{Outcomes: []agoboardstore.ClaimOutcome{agoboardstore.ClaimOutcome("error: " + err.Error())}})
				}
			}
		}
	}
}

// claimAndDispatch takes one task and runs it to a durable decision. Dispatch
// happens only after the claim transaction has committed, and only when this
// caller is the fresh owner.
func (scheduler *Scheduler) claimAndDispatch(ctx context.Context, boardID string) (bool, agoboardstore.ClaimOutcome, error) {
	board, err := scheduler.options.Store.Board(ctx, boardID)
	if err != nil {
		return false, "", err
	}
	now := scheduler.options.Now().UTC()
	// Deriving the claim identity from the observed board version makes two
	// schedulers racing at the same version produce the same command, so the
	// durable receipt elects exactly one fresh owner.
	commandID := "claim:" + boardID + ":" + strconv.FormatUint(board.Version, 10)
	claim, err := scheduler.options.Store.Claim(ctx, agoboardstore.ClaimRequest{
		BoardID: boardID, CommandID: commandID, Actor: scheduler.coordinator(),
		WorkerID: scheduler.options.WorkerID, Now: now,
		LeaseDuration: scheduler.options.LeaseDuration, Limits: scheduler.options.Limits,
	})
	if err != nil {
		return false, "", err
	}
	if !claim.Dispatchable() {
		return false, claim.Outcome, nil
	}
	if err := scheduler.dispatch(ctx, boardID, claim); err != nil {
		return true, claim.Outcome, err
	}
	return true, claim.Outcome, nil
}

// dispatch runs the executor and the independent verifier for a claimed
// attempt. Every command carries the attempt's fencing token, so work that has
// been superseded in the meantime cannot change the graph.
func (scheduler *Scheduler) dispatch(ctx context.Context, boardID string, claim agoboardstore.ClaimResult) error {
	goal, plan, err := scheduler.options.Runtime.Definition(ctx, boardID)
	if err != nil {
		return err
	}
	proposal, found := proposalFor(plan, claim.TaskID)
	if !found {
		return fmt.Errorf("task %q has no planner proposal", claim.TaskID)
	}
	attemptNumber := attemptNumberOf(claim.Board, claim.AttemptID)
	work := agoboardruntime.Dispatch{
		Goal: goal, Task: proposal,
		AttemptID: claim.AttemptID, WorkerID: scheduler.options.WorkerID,
		AttemptNumber: attemptNumber,
		BaseRevision:  claim.Board.IntegratedRevision,
	}

	started, err := scheduler.apply(ctx, boardID, claim, agoboardprotocol.CommandAttemptStart, func(command *agoboardprotocol.Command) {})
	if err != nil {
		return fmt.Errorf("start attempt: %w", err)
	}

	result, executionErr := scheduler.options.Executor.Execute(ctx, work)
	if executionErr != nil || result.Artifact == "" || result.Summary == "" {
		class, reason := classifyExecutionFailure(executionErr)
		_, err := scheduler.applyFrom(ctx, boardID, claim, started.Board.Version, agoboardprotocol.CommandAttemptFail, func(command *agoboardprotocol.Command) {
			command.FailureClass = class
			command.Reason = reason
			command.NextEligibleAt = scheduler.nextEligible(attemptNumber)
		})
		return err
	}

	evidenceID := "evidence:" + claim.AttemptID
	// Executor output is untrusted text. Redact before it becomes durable, so a
	// credential that leaked into a summary or a command line never reaches
	// SQLite, the event stream, or a browser.
	structured := redactEvidence(scheduler.options.Redactor, result.Result)
	structured.Summary = scheduler.options.Redactor.String(firstNonEmpty(structured.Summary, result.Summary))
	submitted, err := scheduler.applyFrom(ctx, boardID, claim, started.Board.Version, agoboardprotocol.CommandEvidenceSubmit, func(command *agoboardprotocol.Command) {
		command.Evidence = &agoboardprotocol.EvidenceSpec{
			ID: evidenceID, TaskID: claim.TaskID, AttemptID: claim.AttemptID,
			Artifact: scheduler.options.Redactor.String(result.Artifact),
			Summary:  structured.Summary,
			Result:   structured,
		}
	})
	if err != nil {
		return fmt.Errorf("submit evidence: %w", err)
	}

	// Deterministic checks are consulted before the verifier is even asked:
	// a failed required test is not a matter of opinion.
	if failed := structured.FailedRequiredTests(); len(failed) > 0 {
		_, err := scheduler.applyVerifier(ctx, boardID, claim, submitted.Board.Version, agoboardprotocol.CommandEvidenceReject, func(command *agoboardprotocol.Command) {
			command.Evidence = &agoboardprotocol.EvidenceSpec{ID: evidenceID}
			command.Reason = fmt.Sprintf("必需的检查未通过：%v", failed)
			command.FailureClass = agoboardprotocol.FailureVerifierFeedback
			command.NextEligibleAt = scheduler.nextEligible(attemptNumber)
		})
		return err
	}

	review, err := scheduler.options.Verifier.Verify(ctx, work, result)
	if err != nil {
		// A verifier outage leaves the work in review; the lease deadline and
		// reconciliation decide what happens next rather than guessing here.
		return fmt.Errorf("verify evidence: %w", err)
	}
	commandType := agoboardprotocol.CommandEvidenceReject
	if review.Accepted {
		commandType = agoboardprotocol.CommandEvidenceAccept
	}
	decided, err := scheduler.applyVerifier(ctx, boardID, claim, submitted.Board.Version, commandType, func(command *agoboardprotocol.Command) {
		command.Evidence = &agoboardprotocol.EvidenceSpec{ID: evidenceID}
		command.Reason = scheduler.options.Redactor.String(review.Reason)
		if !review.Accepted {
			command.FailureClass = rejectionClass(review)
			command.NextEligibleAt = scheduler.nextEligible(attemptNumber)
		}
	})
	if err != nil || !review.Accepted {
		return err
	}
	// Acceptance is not completion for a change: the patch still has to be
	// promoted onto the board's own ref, by an authority the worker is not.
	return scheduler.integrate(ctx, boardID, claim, structured, decided.Board)
}

// integrate promotes an accepted change and completes the task, or records why
// it could not be promoted so a repair can be planned.
func (scheduler *Scheduler) integrate(ctx context.Context, boardID string, claim agoboardstore.ClaimResult, evidence agoboardprotocol.EvidenceResult, board agoboardprotocol.Board) error {
	if evidence.Patch == nil {
		// Nothing changed, so acceptance already completed the task.
		return nil
	}
	fail := func(class agoboardprotocol.FailureClass, reason string) error {
		current, err := scheduler.options.Store.Board(ctx, boardID)
		if err != nil {
			return err
		}
		_, err = scheduler.options.Store.ApplyBoard(ctx, boardID, agoboardprotocol.Command{
			SchemaVersion: agoboardprotocol.SchemaVersion,
			ID:            "integration-fail:" + claim.AttemptID, ExpectedVersion: current.Version,
			Actor: scheduler.coordinator(), Type: agoboardprotocol.CommandIntegrationFail,
			TaskID: claim.TaskID, FailureClass: class,
			Reason: scheduler.options.Redactor.String(reason),
		})
		return err
	}
	if scheduler.options.Integrator == nil || scheduler.options.Artifacts == nil {
		return fail(agoboardprotocol.FailureRepository, "未配置集成权威，已接受的变更无法应用。")
	}
	patch, err := scheduler.options.Artifacts.Bytes(ctx, agoartifact.Descriptor{
		ID: evidence.Patch.ArtifactID, Bytes: evidence.Patch.Bytes, SHA256: evidence.Patch.SHA256,
	})
	if err != nil {
		return fail(agoboardprotocol.FailureRepository, fmt.Sprintf("无法读取变更补丁：%v", err))
	}
	result, err := scheduler.options.Integrator.Integrate(ctx, agointegrate.Request{
		Repository: board.Repository, IntegrationRef: board.IntegrationRef,
		CurrentRevision: board.IntegratedRevision, BaseRevision: evidence.Patch.BaseRevision,
		Patch: patch, TaskID: claim.TaskID, Summary: evidence.Summary,
	})
	if err != nil {
		// A conflict is repairable work, not a reason to force anything.
		class := agoboardprotocol.FailureRepository
		if errors.Is(err, agointegrate.ErrConflict) {
			class = agoboardprotocol.FailureVerifierFeedback
		}
		return fail(class, fmt.Sprintf("集成失败：%v", err))
	}
	current, err := scheduler.options.Store.Board(ctx, boardID)
	if err != nil {
		return err
	}
	_, err = scheduler.options.Store.ApplyBoard(ctx, boardID, agoboardprotocol.Command{
		SchemaVersion: agoboardprotocol.SchemaVersion,
		ID:            "integration:" + claim.AttemptID, ExpectedVersion: current.Version,
		Actor: scheduler.coordinator(), Type: agoboardprotocol.CommandIntegrationComplete,
		TaskID: claim.TaskID, Revision: result.Revision,
		Reason: fmt.Sprintf("已集成到 %s", result.Revision),
	})
	return err
}

func (scheduler *Scheduler) apply(ctx context.Context, boardID string, claim agoboardstore.ClaimResult, commandType agoboardprotocol.CommandType, configure func(*agoboardprotocol.Command)) (agoboardstore.Result, error) {
	return scheduler.applyFrom(ctx, boardID, claim, claim.Board.Version, commandType, configure)
}

func (scheduler *Scheduler) applyFrom(ctx context.Context, boardID string, claim agoboardstore.ClaimResult, version uint64, commandType agoboardprotocol.CommandType, configure func(*agoboardprotocol.Command)) (agoboardstore.Result, error) {
	command := agoboardprotocol.Command{
		SchemaVersion:   agoboardprotocol.SchemaVersion,
		ID:              string(commandType) + ":" + claim.AttemptID,
		ExpectedVersion: version,
		Actor:           agoboardprotocol.Actor{ID: scheduler.options.WorkerID, Role: agoboardprotocol.RoleWorker},
		Type:            commandType,
		TaskID:          claim.TaskID,
		AttemptID:       claim.AttemptID,
		FencingToken:    claim.FencingToken,
	}
	configure(&command)
	return scheduler.options.Store.ApplyBoard(ctx, boardID, command)
}

func (scheduler *Scheduler) applyVerifier(ctx context.Context, boardID string, claim agoboardstore.ClaimResult, version uint64, commandType agoboardprotocol.CommandType, configure func(*agoboardprotocol.Command)) (agoboardstore.Result, error) {
	command := agoboardprotocol.Command{
		SchemaVersion:   agoboardprotocol.SchemaVersion,
		ID:              string(commandType) + ":" + claim.AttemptID,
		ExpectedVersion: version,
		Actor:           agoboardprotocol.Actor{ID: scheduler.options.VerifierID, Role: agoboardprotocol.RoleVerifier},
		Type:            commandType,
		TaskID:          claim.TaskID,
		AttemptID:       claim.AttemptID,
		FencingToken:    claim.FencingToken,
	}
	configure(&command)
	return scheduler.options.Store.ApplyBoard(ctx, boardID, command)
}

func (scheduler *Scheduler) nextEligible(attemptNumber int) time.Time {
	return scheduler.options.Now().UTC().Add(agoboardprotocol.RetryDelay(attemptNumber))
}

func (scheduler *Scheduler) coordinator() agoboardprotocol.Actor {
	return agoboardprotocol.Actor{ID: scheduler.options.CoordinatorID, Role: agoboardprotocol.RoleCoordinator}
}

// classifyExecutionFailure maps an executor error onto a durable failure class.
// An unrecognised error is transient, because an executor crash is the common
// case; a provider that means "do not retry" says so with a typed error.
func classifyExecutionFailure(err error) (agoboardprotocol.FailureClass, string) {
	if err == nil {
		return agoboardprotocol.FailureTransient, "executor returned incomplete evidence"
	}
	var classified interface {
		FailureClass() agoboardprotocol.FailureClass
	}
	if errors.As(err, &classified) {
		return classified.FailureClass(), err.Error()
	}
	return agoboardprotocol.FailureTransient, err.Error()
}

func proposalFor(plan agoplanner.Plan, taskID string) (agoplanner.TaskProposal, bool) {
	for _, item := range plan.Tasks {
		if item.ID == taskID {
			return item, true
		}
	}
	return agoplanner.TaskProposal{}, false
}

func attemptNumberOf(board agoboardprotocol.Board, attemptID string) int {
	for _, attempt := range board.Attempts {
		if attempt.ID == attemptID {
			return attempt.Number
		}
	}
	return 1
}

// rejectionClass maps a verifier's decision onto a durable failure class. An
// unqualified rejection is feedback the next attempt can act on; a verifier
// that means "stop" must say which terminal class applies.
func rejectionClass(review agoboardruntime.Review) agoboardprotocol.FailureClass {
	if review.FailureClass == agoboardprotocol.FailureNone {
		return agoboardprotocol.FailureVerifierFeedback
	}
	return review.FailureClass
}

// redactEvidence removes credential-shaped content from every field an
// executor controls. Hashes, sizes, and exit codes are machine-generated and
// are left alone.
func redactEvidence(redactor *agoredact.Redactor, result agoboardprotocol.EvidenceResult) agoboardprotocol.EvidenceResult {
	result.Summary = redactor.String(result.Summary)
	result.Warnings = redactor.Strings(result.Warnings)
	for index := range result.Commands {
		result.Commands[index].Display = redactor.String(result.Commands[index].Display)
	}
	for index := range result.Tests {
		result.Tests[index].Name = redactor.String(result.Tests[index].Name)
		result.Tests[index].Command = redactor.String(result.Tests[index].Command)
	}
	for index := range result.Artifacts {
		result.Artifacts[index].DisplayName = redactor.String(result.Artifacts[index].DisplayName)
	}
	for index := range result.ChangedFiles {
		result.ChangedFiles[index].Path = redactor.String(result.ChangedFiles[index].Path)
	}
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
