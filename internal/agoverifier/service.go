package agoverifier

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"claudexflow/internal/agothreadstore"
)

type Request struct {
	ThreadID       string `json:"thread_id"`
	TurnID         string `json:"turn_id"`
	ToolCallID     string `json:"tool_call_id"`
	IdempotencyKey string `json:"idempotency_key"`
	CheckID        string `json:"check_id"`
}

type Check struct {
	Executable string
	Args       []string
	Timeout    time.Duration
}

type StaticCatalog map[string]Check

func (catalog StaticCatalog) Resolve(_ context.Context, checkID string) (Check, error) {
	check, ok := catalog[checkID]
	if !ok {
		return Check{}, fmt.Errorf("verification check %q is not registered", checkID)
	}
	return check, nil
}

type ExecutionRequest struct {
	ThreadID       string
	TurnID         string
	ToolCallID     string
	Workspace      string
	Executable     string
	Args           []string
	MaxOutputBytes int
}

type ExecutionResult struct {
	ExitCode int
	Output   []byte
}

type Executor interface {
	Execute(context.Context, ExecutionRequest) (ExecutionResult, error)
}

type CheckCatalog interface {
	Resolve(context.Context, string) (Check, error)
}

type ThreadSource interface {
	Thread(context.Context, string) (agothreadstore.ThreadRecord, error)
}

type VerificationLedger interface {
	RecordVerificationCheck(context.Context, agothreadstore.VerificationCheckInput) (agothreadstore.VerificationCheck, error)
	VerificationChecks(context.Context, string) ([]agothreadstore.VerificationCheck, error)
}

type Limits struct {
	DefaultTimeout, MaxTimeout time.Duration
	MaxOutputBytes             int
}

type Service struct {
	ledger   VerificationLedger
	threads  ThreadSource
	catalog  CheckCatalog
	executor Executor
	limits   Limits
	locks    *requestLocks
}

type requestLocks struct {
	mu      sync.Mutex
	entries map[string]*requestLock
}

type requestLock struct {
	mu   sync.Mutex
	refs int
}

type ExecutionError struct{ Summary string }

func (err ExecutionError) Error() string { return "verification failed: " + err.Summary }

var processRequestLocks requestLocks

const (
	defaultTimeout     = 30 * time.Second
	maximumTimeout     = 2 * time.Minute
	defaultMaxOutput   = 8 * 1024
	maxIdentityBytes   = 256
	maxArgumentCount   = 128
	maxCommandBytes    = 4 * 1024
	persistenceTimeout = 5 * time.Second
)

func New(ledger VerificationLedger, threads ThreadSource, catalog CheckCatalog, executor Executor, limits Limits) *Service {
	if limits.DefaultTimeout <= 0 {
		limits.DefaultTimeout = defaultTimeout
	}
	if limits.MaxTimeout <= 0 {
		limits.MaxTimeout = maximumTimeout
	}
	if limits.DefaultTimeout > limits.MaxTimeout {
		limits.DefaultTimeout = limits.MaxTimeout
	}
	if limits.MaxOutputBytes <= 0 || limits.MaxOutputBytes > defaultMaxOutput {
		limits.MaxOutputBytes = defaultMaxOutput
	}
	return &Service{ledger: ledger, threads: threads, catalog: catalog, executor: executor, limits: limits, locks: &processRequestLocks}
}

func (service *Service) Run(ctx context.Context, request Request) (agothreadstore.VerificationCheck, error) {
	if err := validateRequest(request); err != nil {
		return agothreadstore.VerificationCheck{}, err
	}
	if service == nil || service.ledger == nil || service.threads == nil || service.catalog == nil || service.executor == nil || service.locks == nil {
		return agothreadstore.VerificationCheck{}, fmt.Errorf("verification service dependencies are required")
	}
	release := service.locks.acquire(request.ThreadID + "\x00" + request.IdempotencyKey)
	defer release()

	durableCtx, cancelDurable := persistenceContext(ctx)
	thread, err := service.threads.Thread(durableCtx, request.ThreadID)
	if err != nil {
		cancelDurable()
		return agothreadstore.VerificationCheck{}, fmt.Errorf("resolve verification thread: %w", err)
	}
	workspace, err := filepath.EvalSymlinks(thread.Workspace)
	if err != nil || !filepath.IsAbs(workspace) {
		if err == nil {
			err = fmt.Errorf("workspace is not absolute")
		}
		cancelDurable()
		return agothreadstore.VerificationCheck{}, fmt.Errorf("resolve canonical verification workspace: %w", err)
	}
	checks, err := service.ledger.VerificationChecks(durableCtx, request.ThreadID)
	if err != nil {
		cancelDurable()
		return agothreadstore.VerificationCheck{}, err
	}
	runningSummary := fmt.Sprintf("running; turn=%s; tool_call=%s", request.TurnID, request.ToolCallID)
	prior, found := checkByKey(checks, request.IdempotencyKey)
	if found && (prior.CheckID != request.CheckID || prior.OutputSummary != runningSummary) {
		cancelDurable()
		return agothreadstore.VerificationCheck{}, agothreadstore.VerificationConflictError{Reason: "idempotency key was already used for a different verification request"}
	}
	check, err := service.catalog.Resolve(durableCtx, request.CheckID)
	if err != nil {
		cancelDurable()
		return agothreadstore.VerificationCheck{}, err
	}
	if err := validateCheck(check); err != nil {
		cancelDurable()
		return agothreadstore.VerificationCheck{}, err
	}

	command := commandSummary(check)
	runningInput := agothreadstore.VerificationCheckInput{
		ThreadID: request.ThreadID, IdempotencyKey: request.IdempotencyKey, CheckID: request.CheckID,
		Command: command, Status: agothreadstore.VerificationUnknown,
		OutputSummary: runningSummary,
	}
	if found {
		if _, err := service.ledger.RecordVerificationCheck(durableCtx, runningInput); err != nil {
			cancelDurable()
			return agothreadstore.VerificationCheck{}, err
		}
		cancelDurable()
		if final, ok := checkByKey(checks, finalKey(request.IdempotencyKey)); ok {
			return terminalResult(final)
		}
		return prior, nil
	}
	if _, err := service.ledger.RecordVerificationCheck(durableCtx, runningInput); err != nil {
		cancelDurable()
		return agothreadstore.VerificationCheck{}, err
	}
	cancelDurable()

	timeout := check.Timeout
	if timeout <= 0 {
		timeout = service.limits.DefaultTimeout
	}
	if timeout > service.limits.MaxTimeout {
		timeout = service.limits.MaxTimeout
	}
	runCtx, cancelRun := context.WithTimeout(ctx, timeout)
	result, executionErr := service.executor.Execute(runCtx, ExecutionRequest{
		ThreadID: request.ThreadID, TurnID: request.TurnID, ToolCallID: request.ToolCallID,
		Workspace: workspace, Executable: check.Executable, Args: append([]string(nil), check.Args...), MaxOutputBytes: service.limits.MaxOutputBytes,
	})
	runContextErr := runCtx.Err()
	cancelRun()

	status, summary := terminalTruth(result, executionErr, runContextErr, service.limits.MaxOutputBytes)
	finalCtx, cancelFinal := persistenceContext(ctx)
	defer cancelFinal()
	final, recordErr := service.ledger.RecordVerificationCheck(finalCtx, agothreadstore.VerificationCheckInput{
		ThreadID: request.ThreadID, IdempotencyKey: finalKey(request.IdempotencyKey), CheckID: request.CheckID,
		Command: command, Status: status, OutputSummary: summary,
	})
	if recordErr != nil {
		return agothreadstore.VerificationCheck{}, fmt.Errorf("record terminal verification truth: %w", recordErr)
	}
	return terminalResult(final)
}

func persistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), persistenceTimeout)
}

func IsConflict(err error) bool {
	var conflict agothreadstore.VerificationConflictError
	return errors.As(err, &conflict)
}

func validateRequest(request Request) error {
	for name, value := range map[string]string{
		"thread ID": request.ThreadID, "turn ID": request.TurnID, "tool call ID": request.ToolCallID,
		"idempotency key": request.IdempotencyKey, "check ID": request.CheckID,
	} {
		if value == "" || len(value) > maxIdentityBytes {
			return fmt.Errorf("verification %s must contain 1..%d bytes", name, maxIdentityBytes)
		}
	}
	return nil
}

func validateCheck(check Check) error {
	if !filepath.IsAbs(check.Executable) {
		return fmt.Errorf("catalog verification executable must be absolute")
	}
	if len(check.Args) > maxArgumentCount {
		return fmt.Errorf("catalog verification argv exceeds %d arguments", maxArgumentCount)
	}
	if len(commandSummary(check)) > maxCommandBytes {
		return fmt.Errorf("catalog verification command exceeds %d bytes", maxCommandBytes)
	}
	return nil
}

func commandSummary(check Check) string {
	parts := make([]string, 1, len(check.Args)+1)
	parts[0] = check.Executable
	for _, argument := range check.Args {
		parts = append(parts, strconv.Quote(argument))
	}
	return strings.Join(parts, " ")
}

func finalKey(key string) string { return key + ":final" }

func checkByKey(checks []agothreadstore.VerificationCheck, key string) (agothreadstore.VerificationCheck, bool) {
	for _, check := range checks {
		if check.IdempotencyKey == key {
			return check, true
		}
	}
	return agothreadstore.VerificationCheck{}, false
}

func terminalTruth(result ExecutionResult, executionErr, contextErr error, maxOutput int) (agothreadstore.VerificationStatus, string) {
	if contextErr != nil {
		if errors.Is(contextErr, context.DeadlineExceeded) {
			return agothreadstore.VerificationFailed, "timed out: " + contextErr.Error()
		}
		return agothreadstore.VerificationFailed, "canceled: " + contextErr.Error()
	}
	if executionErr != nil {
		return agothreadstore.VerificationFailed, boundedText("executor failure: "+executionErr.Error(), defaultMaxOutput)
	}
	output := result.Output
	truncated := len(output) > maxOutput
	if truncated {
		output = output[:maxOutput]
	}
	summary := fmt.Sprintf("exit %d", result.ExitCode)
	if len(output) == 0 {
		summary += "; no output"
	} else {
		summary += "; output: " + string(output)
	}
	if truncated {
		summary += " [truncated]"
	}
	if result.ExitCode == 0 {
		return agothreadstore.VerificationPassed, summary
	}
	return agothreadstore.VerificationFailed, summary
}

func boundedText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + " [truncated]"
}

func terminalResult(record agothreadstore.VerificationCheck) (agothreadstore.VerificationCheck, error) {
	if record.Status == agothreadstore.VerificationPassed {
		return record, nil
	}
	return record, ExecutionError{Summary: record.OutputSummary}
}

func (locks *requestLocks) acquire(key string) func() {
	locks.mu.Lock()
	if locks.entries == nil {
		locks.entries = make(map[string]*requestLock)
	}
	entry := locks.entries[key]
	if entry == nil {
		entry = &requestLock{}
		locks.entries[key] = entry
	}
	entry.refs++
	locks.mu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		locks.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(locks.entries, key)
		}
		locks.mu.Unlock()
	}
}
