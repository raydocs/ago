// Package agosupervisor closes the loop on a goal without a human relaying
// messages between steps.
//
// The product problem it solves: a user should state a goal once and then only
// be interrupted for decisions a machine genuinely must not make alone. Reading
// a worker's report, deciding whether to believe it, asking for a review,
// writing a repair instruction, and saying "continue" are all mechanical, and
// all of them are done here from durable state.
//
// What this package is NOT: a second scheduling authority. It never claims a
// task, never mints a fencing token, and never writes a task row. Claiming
// stays in internal/agoscheduler, which is the only component allowed to admit
// work. The supervisor observes the durable graph and issues legal protocol
// commands — patches, retries, and pauses — exactly like a careful human
// operator would, which is why it cannot bypass any fencing or retry rule.
package agosupervisor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"claudexflow/internal/agoboardprotocol"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agoscheduler"
)

// DecisionKind classifies why the supervisor needs a person. Everything not on
// this list is handled without interrupting anyone.
type DecisionKind string

const (
	// DecisionDestructive covers publishing, deleting, or anything else whose
	// effect leaves this machine or cannot be undone.
	DecisionDestructive DecisionKind = "destructive"
	// DecisionCredential covers a provider asking for a secret.
	DecisionCredential DecisionKind = "credential"
	// DecisionAmbiguous covers a goal the repository's evidence cannot resolve.
	DecisionAmbiguous DecisionKind = "ambiguous"
	// DecisionBudget covers exceeding the limits the user set up front.
	DecisionBudget DecisionKind = "budget"
	// DecisionExhausted covers work where automatic repair and bounded retry
	// are both spent.
	DecisionExhausted DecisionKind = "exhausted"
)

// Decision is one item in the user's attention queue. It is deliberately
// self-contained: a user must never have to go find a worker to understand it.
type Decision struct {
	Kind         DecisionKind `json:"kind"`
	TaskID       string       `json:"task_id,omitempty"`
	Title        string       `json:"title"`
	Reason       string       `json:"reason"`
	Suggestion   string       `json:"suggestion"`
	RaisedAt     time.Time    `json:"raised_at"`
	AttemptsUsed int          `json:"attempts_used,omitempty"`
}

// Authorization is what the user granted when the goal started. Anything not
// granted here becomes a decision rather than an action.
type Authorization struct {
	// LocalFileWrites allows repository modification inside the declared scope.
	LocalFileWrites bool
	// LocalCommits allows creating checkpoint commits.
	LocalCommits bool
	// Push, Publish, and Destructive are never inferred; they must be granted.
	Push        bool
	Publish     bool
	Destructive bool
	// MaxRepairsPerTask bounds automatic repair so a task cannot be repaired
	// forever. Zero means the default.
	MaxRepairsPerTask int
	// MaxPatches bounds how much the supervisor may reshape one goal.
	MaxPatches int
}

const (
	defaultMaxRepairsPerTask = 2
	defaultMaxPatches        = 16
)

type Options struct {
	Store     *agoboardstore.Store
	Scheduler *agoscheduler.Scheduler
	BoardID   string
	Authorize Authorization
	// CoordinatorID is the identity the supervisor issues commands under. It
	// must be a coordinator: the supervisor may plan, never execute.
	CoordinatorID string
	Now           func() time.Time
	// After is the injected timer used when the only remaining work is a
	// scheduled retry. Defaults to time.After; a test supplies its own so no
	// test is ever paced by real time.
	After func(time.Duration) <-chan time.Time
	// OnDecision is called when a decision is queued for the user.
	OnDecision func(Decision)
}

type Supervisor struct {
	options   Options
	patches   int
	decisions []Decision
}

func New(options Options) (*Supervisor, error) {
	if options.Store == nil || options.Scheduler == nil {
		return nil, fmt.Errorf("supervisor requires a store and a scheduler")
	}
	if strings.TrimSpace(options.BoardID) == "" || strings.TrimSpace(options.CoordinatorID) == "" {
		return nil, fmt.Errorf("supervisor requires a board id and a coordinator identity")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.After == nil {
		options.After = time.After
	}
	if options.Authorize.MaxRepairsPerTask <= 0 {
		options.Authorize.MaxRepairsPerTask = defaultMaxRepairsPerTask
	}
	if options.Authorize.MaxPatches <= 0 {
		options.Authorize.MaxPatches = defaultMaxPatches
	}
	return &Supervisor{options: options, decisions: []Decision{}}, nil
}

// Status is what the goal looks like right now.
type Status struct {
	Complete  bool
	Blocked   bool
	Passed    int
	Failed    int
	Remaining int
	Decisions []Decision
}

// Step advances the goal by one supervisory pass and reports the result.
//
// The order matters: scheduling runs first so the graph is as far along as it
// can get, then the supervisor looks at what stopped and decides whether it can
// act. A pass that changes nothing and raises no decision means the goal is
// either finished or genuinely waiting on the scheduler.
func (supervisor *Supervisor) Step(ctx context.Context) (Status, error) {
	// The scheduler owns claiming. The supervisor only asks it to run, and only
	// for its own board: a fleet-wide sweep per supervisor would make one pass
	// quadratic in the number of goals.
	if _, err := supervisor.options.Scheduler.RunOnceForBoard(ctx, supervisor.options.BoardID); err != nil {
		return Status{}, fmt.Errorf("scheduler cycle: %w", err)
	}
	board, err := supervisor.options.Store.Board(ctx, supervisor.options.BoardID)
	if err != nil {
		return Status{}, err
	}
	if err := supervisor.reviewStoppedWork(ctx, board); err != nil {
		return Status{}, err
	}
	return supervisor.status(ctx)
}

// Run drives the goal to a terminal state without a human in the loop.
//
// It stops when the goal completes, when every remaining stop needs a person,
// or when the context ends. It never stops merely because a task finished:
// that is the behaviour this whole package exists to remove.
func (supervisor *Supervisor) Run(ctx context.Context, maxSteps int) (Status, error) {
	if maxSteps <= 0 {
		maxSteps = 512
	}
	var status Status
	for range maxSteps {
		if err := ctx.Err(); err != nil {
			return status, err
		}
		next, err := supervisor.Step(ctx)
		if err != nil {
			return status, err
		}
		status = next
		if status.Complete || status.Blocked {
			return status, nil
		}
		// A task inside its retry backoff is not making progress and cannot be
		// hurried. Spinning through it burned the whole step budget on a goal
		// whose only remaining work was a scheduled retry — the run then
		// reported "did not reach a terminal state" for work that was simply
		// waiting. Wait for the clock instead of the budget.
		if err := supervisor.waitForRetry(ctx); err != nil {
			return status, err
		}
	}
	return status, fmt.Errorf("goal did not reach a terminal state within %d supervisory steps", maxSteps)
}

// waitForRetry pauses until the earliest scheduled retry becomes eligible, but
// only when a scheduled retry is the ONLY thing left. Any task that could be
// claimed right now means the next pass has real work to do, so it returns
// immediately.
//
// The wait goes through the injected clock, so a test never sleeps.
func (supervisor *Supervisor) waitForRetry(ctx context.Context) error {
	board, err := supervisor.options.Store.Board(ctx, supervisor.options.BoardID)
	if err != nil {
		return err
	}
	now := supervisor.options.Now()
	earliest := time.Time{}
	for _, task := range board.Tasks {
		if task.State == agoboardprotocol.TaskPassed || task.Cancelled || task.SupersededBy != "" {
			continue
		}
		if task.State != agoboardprotocol.TaskRetryWait {
			// Something else is outstanding: running, verifying, integrating,
			// or ready. The next pass has work.
			if task.State != agoboardprotocol.TaskFailed || !supervisor.alreadyRaised(task.ID) {
				return nil
			}
			continue
		}
		if earliest.IsZero() || task.NextEligibleAt.Before(earliest) {
			earliest = task.NextEligibleAt
		}
	}
	if earliest.IsZero() {
		return nil
	}
	delay := earliest.Sub(now)
	if delay <= 0 {
		return nil
	}
	timer := supervisor.options.After(delay)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer:
		return nil
	}
}

// reviewStoppedWork decides what to do about every task that has stopped.
//
// A stopped task is either repairable — the failure describes something a new
// attempt could fix — or it needs a person. Nothing is retried silently past
// its budget, and nothing is escalated that the machine could have handled.
func (supervisor *Supervisor) reviewStoppedWork(ctx context.Context, board agoboardprotocol.Board) error {
	for _, task := range board.Tasks {
		if task.State != agoboardprotocol.TaskFailed || task.Cancelled || task.SupersededBy != "" {
			continue
		}
		if supervisor.alreadyRaised(task.ID) {
			continue
		}
		kind, needsUser := escalationFor(task, supervisor.options.Authorize)
		if needsUser {
			supervisor.raise(Decision{
				Kind: kind, TaskID: task.ID, Title: task.Title,
				Reason:       failureNarrative(task),
				Suggestion:   suggestionFor(kind, task),
				RaisedAt:     supervisor.options.Now().UTC(),
				AttemptsUsed: task.AttemptCount,
			})
			continue
		}
		// Repairable: give the task a fresh budget with an acceptance criterion
		// that names what went wrong, so the next attempt is aimed at the
		// failure rather than repeating the same work blindly.
		//
		// The count comes from the durable graph, not from memory. A restarted
		// supervisor must reach the same decision as the one it replaced;
		// counting in memory would re-issue repairs whose command receipts
		// already exist and silently burn the budget on commands that do
		// nothing.
		if task.UserRetries >= supervisor.options.Authorize.MaxRepairsPerTask {
			supervisor.raise(Decision{
				Kind: DecisionExhausted, TaskID: task.ID, Title: task.Title,
				Reason:       fmt.Sprintf("自动修复已用尽（%d 次）：%s", task.UserRetries, failureNarrative(task)),
				Suggestion:   "检查任务契约是否可行，或缩小范围后手动重试。",
				RaisedAt:     supervisor.options.Now().UTC(),
				AttemptsUsed: task.AttemptCount,
			})
			continue
		}
		if err := supervisor.repair(ctx, task); err != nil {
			// One task that cannot be repaired must not stop the others from
			// being reviewed. It becomes a decision instead of a silent stall.
			supervisor.raise(Decision{
				Kind: DecisionExhausted, TaskID: task.ID, Title: task.Title,
				Reason:       fmt.Sprintf("自动修复失败：%v", err),
				Suggestion:   "检查任务契约是否可行，或缩小范围后手动重试。",
				RaisedAt:     supervisor.options.Now().UTC(),
				AttemptsUsed: task.AttemptCount,
			})
		}
	}
	return nil
}

// repair issues an audited plan patch that sharpens the task's acceptance with
// the recorded failure, then a retry. Both are legal protocol commands; the
// scheduler picks the work up on its next cycle exactly as it would any other
// ready task.
func (supervisor *Supervisor) repair(ctx context.Context, task agoboardprotocol.Task) error {
	if supervisor.patches >= supervisor.options.Authorize.MaxPatches {
		supervisor.raise(Decision{
			Kind: DecisionBudget, TaskID: task.ID, Title: task.Title,
			Reason:     fmt.Sprintf("已达到本目标的计划修改上限（%d 次）。", supervisor.options.Authorize.MaxPatches),
			Suggestion: "提高预算或人工确认剩余工作。",
			RaisedAt:   supervisor.options.Now().UTC(),
		})
		return nil
	}
	// UserRetries is the durable record of how many times this task was
	// restarted, so the identity below is stable across a restart.
	count := task.UserRetries + 1
	supervisor.patches++

	criterion := fmt.Sprintf("修复上一次失败：%s", failureNarrative(task))
	acceptance := agoboardprotocol.TerminalContract{
		Outcome:            task.TerminalContract.Outcome,
		AcceptanceCriteria: append(append([]string(nil), task.TerminalContract.AcceptanceCriteria...), criterion),
	}
	board, err := supervisor.options.Store.Board(ctx, supervisor.options.BoardID)
	if err != nil {
		return err
	}
	patchID := fmt.Sprintf("repair:%s:%d", task.ID, count)
	if _, err := supervisor.options.Store.ApplyBoard(ctx, supervisor.options.BoardID, agoboardprotocol.Command{
		SchemaVersion:   agoboardprotocol.SchemaVersion,
		ID:              "cmd:" + patchID,
		ExpectedVersion: board.Version,
		Actor:           supervisor.coordinator(),
		Type:            agoboardprotocol.CommandPlanPatch,
		Patch: &agoboardprotocol.PatchSpec{
			ID:     patchID,
			Reason: fmt.Sprintf("验收未通过，自动生成修复要求（第 %d 次）", count),
			Steps: []agoboardprotocol.PatchStep{{
				Operation: agoboardprotocol.PatchUpdateAcceptance,
				TaskID:    task.ID, Acceptance: &acceptance,
			}},
		},
	}); err != nil {
		// The same repair was already recorded, which is what a restart looks
		// like. Treat it as done rather than as a failure: retrying would
		// re-issue an identical command, and returning an error would abandon
		// every other stopped task in this pass.
		if !errors.Is(err, agoboardstore.ErrCommandConflict) {
			return fmt.Errorf("apply repair patch for %q: %w", task.ID, err)
		}
		return nil
	}

	updated, err := supervisor.options.Store.Board(ctx, supervisor.options.BoardID)
	if err != nil {
		return err
	}
	if _, err := supervisor.options.Store.ApplyBoard(ctx, supervisor.options.BoardID, agoboardprotocol.Command{
		SchemaVersion:   agoboardprotocol.SchemaVersion,
		ID:              "cmd:retry:" + patchID,
		ExpectedVersion: updated.Version,
		Actor:           supervisor.coordinator(),
		Type:            agoboardprotocol.CommandTaskRetry,
		TaskID:          task.ID,
		Reason:          fmt.Sprintf("自动修复第 %d 次：%s", count, failureNarrative(task)),
	}); err != nil {
		if !errors.Is(err, agoboardstore.ErrCommandConflict) {
			return fmt.Errorf("retry task %q after repair: %w", task.ID, err)
		}
	}
	return nil
}

// escalationFor decides whether a stopped task needs a person.
//
// The rule is about the CLASS of failure, not about how many times it happened:
// a missing credential does not become machine-fixable by retrying, and a
// verifier's feedback does not become a human problem just because it recurred.
func escalationFor(task agoboardprotocol.Task, authorize Authorization) (DecisionKind, bool) {
	switch task.FailureClass {
	case agoboardprotocol.FailureAuth:
		return DecisionCredential, true
	case agoboardprotocol.FailureNeedsInput:
		return DecisionAmbiguous, true
	case agoboardprotocol.FailurePolicy:
		// Policy stops are exactly the destructive and out-of-scope actions the
		// user did or did not authorize up front.
		if authorize.Destructive {
			return "", false
		}
		return DecisionDestructive, true
	case agoboardprotocol.FailureRepository:
		return DecisionAmbiguous, true
	case agoboardprotocol.FailureExhausted:
		return DecisionExhausted, true
	case agoboardprotocol.FailureVerifierFeedback, agoboardprotocol.FailureTransient, agoboardprotocol.FailurePermanent:
		// Actionable: the evidence says what was wrong, so a repaired attempt
		// has something concrete to aim at.
		return "", false
	default:
		// An unclassified stop is escalated rather than guessed at.
		return DecisionAmbiguous, true
	}
}

func suggestionFor(kind DecisionKind, task agoboardprotocol.Task) string {
	switch kind {
	case DecisionCredential:
		return "在环境中配置所需凭据后重试；凭据不会写入任务或事件。"
	case DecisionDestructive:
		return "拒绝并把该操作拆成一个需要显式授权的任务。"
	case DecisionAmbiguous:
		return "补充这项信息，然后从任务抽屉重试。"
	case DecisionExhausted:
		return "检查任务契约是否可行，或缩小范围后手动重试。"
	case DecisionBudget:
		return "提高预算或人工确认剩余工作。"
	default:
		return "需要人工判断。"
	}
}

func failureNarrative(task agoboardprotocol.Task) string {
	reason := strings.TrimSpace(task.BlockedReason)
	if reason == "" {
		reason = "未记录原因"
	}
	if task.FailureClass == agoboardprotocol.FailureNone {
		return reason
	}
	return fmt.Sprintf("%s（%s）", reason, task.FailureClass)
}

func (supervisor *Supervisor) status(ctx context.Context) (Status, error) {
	completion, err := supervisor.options.Store.Completion(ctx, supervisor.options.BoardID)
	if err != nil {
		return Status{}, err
	}
	board, err := supervisor.options.Store.Board(ctx, supervisor.options.BoardID)
	if err != nil {
		return Status{}, err
	}
	status := Status{
		Passed: completion.Passed, Failed: completion.Failed, Remaining: completion.Remaining,
		Decisions: append([]Decision(nil), supervisor.decisions...),
	}
	// The goal is complete only when nothing is left. A superseded or cancelled
	// task counts as resolved: it was replaced or withdrawn on the record.
	outstanding := 0
	for _, task := range board.Tasks {
		switch {
		case task.State == agoboardprotocol.TaskPassed:
		case task.Cancelled || task.SupersededBy != "":
		default:
			outstanding++
		}
	}
	status.Complete = outstanding == 0
	// Blocked means every remaining stop is waiting on a person: there is
	// nothing left the supervisor is allowed to do by itself.
	if !status.Complete {
		status.Blocked = supervisor.everyRemainingStopNeedsAPerson(board)
	}
	return status, nil
}

// everyRemainingStopNeedsAPerson reports that no further progress is possible
// without the user.
//
// It is not enough to look at the tasks that failed: work waiting on a
// dependency that itself needs a person is equally stuck, and treating it as
// merely "pending" is what makes a supervisor spin. Waiting is therefore
// computed transitively across the dependency graph.
func (supervisor *Supervisor) everyRemainingStopNeedsAPerson(board agoboardprotocol.Board) bool {
	waiting := map[string]bool{}
	for _, task := range board.Tasks {
		if task.State == agoboardprotocol.TaskFailed && supervisor.alreadyRaised(task.ID) {
			waiting[task.ID] = true
		}
	}
	// Propagate until the set stops growing; the graph is acyclic and small, so
	// a fixed-point pass is simpler than a topological order.
	for changed := true; changed; {
		changed = false
		for _, dependency := range board.Dependencies {
			if waiting[dependency.DependsOn] && !waiting[dependency.TaskID] {
				waiting[dependency.TaskID] = true
				changed = true
			}
		}
	}
	pending := 0
	stuck := 0
	for _, task := range board.Tasks {
		if task.State == agoboardprotocol.TaskPassed || task.Cancelled || task.SupersededBy != "" {
			continue
		}
		pending++
		if waiting[task.ID] {
			stuck++
		}
	}
	return pending > 0 && pending == stuck
}

func (supervisor *Supervisor) alreadyRaised(taskID string) bool {
	for _, decision := range supervisor.decisions {
		if decision.TaskID == taskID {
			return true
		}
	}
	return false
}

func (supervisor *Supervisor) raise(decision Decision) {
	if supervisor.alreadyRaised(decision.TaskID) {
		return
	}
	supervisor.decisions = append(supervisor.decisions, decision)
	sort.SliceStable(supervisor.decisions, func(i, j int) bool {
		return supervisor.decisions[i].TaskID < supervisor.decisions[j].TaskID
	})
	if supervisor.options.OnDecision != nil {
		supervisor.options.OnDecision(decision)
	}
}

// Decisions is the user's attention queue.
func (supervisor *Supervisor) Decisions() []Decision {
	return append([]Decision(nil), supervisor.decisions...)
}

// ErrNeedsUser reports that a goal stopped on decisions only a person can make.
var ErrNeedsUser = errors.New("the goal is waiting on a user decision")

func (supervisor *Supervisor) coordinator() agoboardprotocol.Actor {
	return agoboardprotocol.Actor{ID: supervisor.options.CoordinatorID, Role: agoboardprotocol.RoleCoordinator}
}
