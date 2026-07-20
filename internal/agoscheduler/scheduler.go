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

	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoboardstore"
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
	// Redactor strips credential-shaped content from executor output before it
	// becomes durable evidence. A nil value is replaced with one seeded from
	// the process environment, so redaction is never accidentally off.
	Redactor *agoredact.Redactor
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

// RunOnce performs exactly one deterministic scheduler cycle: preflight,
// reconcile expired leases, then claim and dispatch eligible work within the
// configured slots. It is the unit tests drive; Run is a loop around it.
func (scheduler *Scheduler) RunOnce(ctx context.Context) (Cycle, error) {
	cycle := Cycle{Outcomes: []agoboardstore.ClaimOutcome{}}
	now := scheduler.options.Now().UTC()

	// Reconciliation runs before admission so a lease whose deadline elapsed
	// releases its task in the same pass that might re-claim it.
	expired, err := scheduler.options.Store.ExpireDueLeases(ctx, now, scheduler.coordinator())
	if err != nil {
		return cycle, fmt.Errorf("reconcile expired leases: %w", err)
	}
	cycle.Reconciled = len(expired)

	boards, err := scheduler.options.Store.BoardIDs(ctx)
	if err != nil {
		return cycle, fmt.Errorf("list boards: %w", err)
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
	_, err = scheduler.applyVerifier(ctx, boardID, claim, submitted.Board.Version, commandType, func(command *agoboardprotocol.Command) {
		command.Evidence = &agoboardprotocol.EvidenceSpec{ID: evidenceID}
		command.Reason = scheduler.options.Redactor.String(review.Reason)
		if !review.Accepted {
			command.FailureClass = rejectionClass(review)
			command.NextEligibleAt = scheduler.nextEligible(attemptNumber)
		}
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
