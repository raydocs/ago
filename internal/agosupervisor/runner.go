package agosupervisor

import (
	"context"
	"sync"
	"time"

	"claudexflow/internal/agoboardapi"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agoscheduler"
)

// Runner keeps one supervisor per board and drives them all.
//
// Boards appear over time, because a user creates goals while the server is
// already running. The runner therefore discovers them from the durable store
// rather than being told about them, which also means a restarted process
// picks up every goal that was in flight without being reminded.
type Runner struct {
	store     *agoboardstore.Store
	scheduler *agoscheduler.Scheduler
	authorize Authorization
	actorID   string
	now       func() time.Time
	interval  time.Duration

	mu          sync.Mutex
	supervisors map[string]*Supervisor
}

type RunnerOptions struct {
	Store     *agoboardstore.Store
	Scheduler *agoscheduler.Scheduler
	Authorize Authorization
	// CoordinatorID is the identity supervisory commands are issued under.
	CoordinatorID string
	Interval      time.Duration
	Now           func() time.Time
}

func NewRunner(options RunnerOptions) (*Runner, error) {
	if options.Store == nil || options.Scheduler == nil {
		return nil, errRunnerRequirements
	}
	if options.Interval <= 0 {
		options.Interval = time.Second
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.CoordinatorID == "" {
		options.CoordinatorID = "ago-supervisor"
	}
	return &Runner{
		store: options.Store, scheduler: options.Scheduler,
		authorize: options.Authorize, actorID: options.CoordinatorID,
		now: options.Now, interval: options.Interval,
		supervisors: map[string]*Supervisor{},
	}, nil
}

var errRunnerRequirements = errorString("supervisor runner requires a store and a scheduler")

type errorString string

func (e errorString) Error() string { return string(e) }

// Run drives every board until the context ends. A failure on one board is
// recorded and the loop continues: one stuck goal must not stop the others.
func (runner *Runner) Run(ctx context.Context) error {
	ticker := time.NewTicker(runner.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			runner.step(ctx)
		}
	}
}

func (runner *Runner) step(ctx context.Context) {
	boards, err := runner.store.BoardIDs(ctx)
	if err != nil {
		return
	}
	for _, boardID := range boards {
		if err := ctx.Err(); err != nil {
			return
		}
		supervisor, err := runner.supervisorFor(boardID)
		if err != nil {
			continue
		}
		// One pass per tick. Step is idempotent with respect to durable state,
		// so a pass that finds nothing to do is free.
		if _, err := supervisor.Step(ctx); err != nil {
			continue
		}
	}
}

func (runner *Runner) supervisorFor(boardID string) (*Supervisor, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if existing, found := runner.supervisors[boardID]; found {
		return existing, nil
	}
	supervisor, err := New(Options{
		Store: runner.store, Scheduler: runner.scheduler, BoardID: boardID,
		CoordinatorID: runner.actorID, Authorize: runner.authorize, Now: runner.now,
	})
	if err != nil {
		return nil, err
	}
	runner.supervisors[boardID] = supervisor
	return supervisor, nil
}

// PendingDecisions satisfies the API's DecisionSource, so the attention queue a
// user sees is the same one the supervisor is actually waiting on.
func (runner *Runner) PendingDecisions(boardID string) []agoboardapi.PendingDecision {
	runner.mu.Lock()
	supervisor, found := runner.supervisors[boardID]
	runner.mu.Unlock()
	if !found {
		return []agoboardapi.PendingDecision{}
	}
	decisions := supervisor.Decisions()
	pending := make([]agoboardapi.PendingDecision, 0, len(decisions))
	for _, decision := range decisions {
		pending = append(pending, agoboardapi.PendingDecision{
			Kind: string(decision.Kind), TaskID: decision.TaskID, Title: decision.Title,
			Reason: decision.Reason, Suggestion: decision.Suggestion,
			RaisedAt: decision.RaisedAt, AttemptsUsed: decision.AttemptsUsed,
		})
	}
	return pending
}
