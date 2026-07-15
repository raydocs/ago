package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"claudexflow/internal/claude"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const workerModel = "grok-4.5"
const workerEffort = "high"

func (s *Server) startWorker(ctx context.Context, req *mcp.CallToolRequest, in WorkerStartInput) (*mcp.CallToolResult, WorkerOutput, error) {
	// Normalize the workdir and owned paths before admission. Admission reasons
	// are structural and must not depend on whether the caller used an absolute
	// path or the equivalent launch-root-relative path. Keeping the normalized
	// packet also makes retries and write-scope leases compare the same identity.
	dir, err := s.scopedDir(in.WorkDir)
	if err != nil {
		return nil, WorkerOutput{}, err
	}
	paths, err := s.normalizedPaths(in.Paths)
	if err != nil {
		return nil, WorkerOutput{}, err
	}
	in.WorkDir = dir
	in.Paths = paths
	if err := s.prepareWorkerRoute(&in); err != nil {
		return nil, WorkerOutput{}, err
	}

	admission := evaluateWorkerAdmission(in)
	if admission.Result == admissionRejected {
		s.recordRejectedAdmission(admission, in)
		return nil, WorkerOutput{}, admissionError(admission)
	}
	if err := s.validateRouteTool(in.RouteID, "start_worker"); err != nil {
		admission.Result = admissionRejected
		admission.RejectionReasons = append(admission.RejectionReasons, err.Error())
		s.recordRejectedAdmission(admission, in)
		return nil, WorkerOutput{}, admissionError(admission)
	}
	record, err := s.reserveSlice(admission, in)
	if err != nil {
		return nil, WorkerOutput{}, err
	}
	defer s.clearPreparingSlice(admission.SliceID)

	if in.Write && len(paths) == 0 {
		err := fmt.Errorf("write workers require explicit non-overlapping paths")
		s.rejectPreparedSlice(record, err.Error())
		return nil, WorkerOutput{}, err
	}

	id := fmt.Sprintf("worker-%d", s.nextID.Add(1))
	w := &workerState{
		id:         id,
		sliceID:    admission.SliceID,
		workDir:    dir,
		write:      in.Write,
		paths:      paths,
		allowBash:  admission.VerifierMode == verifierExactOnce,
		turn:       1,
		deadlineMS: admission.DeadlineMS,
		state:      "starting",
		admission:  admission,
		toolUses:   map[string]int{},
		background: in.Background,
		done:       make(chan struct{}),
		identity: ExecutionIdentity{
			RequestedModel: workerModel, RequestedEffort: workerEffort,
			ModelVerification: "unverified", EffortVerification: "cli_argument_only", AuthSource: string(claude.AuthGateway),
		},
	}
	if err := s.acquireLease(w); err != nil {
		s.rejectPreparedSlice(record, "write_scope_overlap: "+err.Error())
		return nil, WorkerOutput{}, err
	}
	if err := s.acquire(ctx); err != nil {
		s.releaseLease(w)
		s.rejectPreparedSlice(record, err.Error())
		return nil, WorkerOutput{}, err
	}
	if err := s.markSliceStarted(record, w); err != nil {
		s.releaseLease(w)
		s.release()
		return nil, WorkerOutput{}, err
	}
	s.recordRouteIntegrationStart(in.RouteID, admission.SliceID)
	w.state = "running"
	s.updateSliceFromWorker(record, w)

	if in.Background {
		base := s.serverCtx
		if base == nil {
			base = context.Background()
		}
		runCtx, cancel := context.WithTimeout(base, time.Duration(admission.DeadlineMS)*time.Millisecond)
		w.cancel = cancel
		go func() {
			defer func() {
				cancel()
				s.release()
				w.doneOnce.Do(func() { close(w.done) })
			}()
			_, runErr := s.executeWorkerStart(runCtx, nil, in, w, record)
			w.mu.Lock()
			w.backgroundErr = runErr
			w.mu.Unlock()
		}()
		w.mu.Lock()
		out := workerOutputLocked(w)
		w.mu.Unlock()
		return nil, out, nil
	}

	defer s.release()
	out, err := s.executeWorkerStart(ctx, req, in, w, record)
	if err == nil {
		digest := s.workerIntegrationDigest(w)
		w.mu.Lock()
		w.integration = digest
		w.mu.Unlock()
		out.Integration = digest
		s.recordRouteIntegrationResult(in.RouteID, admission.SliceID, digest)
	}
	w.doneOnce.Do(func() { close(w.done) })
	return nil, out, err
}

func (s *Server) executeWorkerStart(ctx context.Context, req *mcp.CallToolRequest, in WorkerStartInput, w *workerState, record *sliceState) (WorkerOutput, error) {
	rootSessionID, parentSessionID := s.threadBinding()
	progress := s.beginWorkerProgress(ctx, req, w.id, workerModel+"/"+workerEffort)
	maxTurns := workerInitialMaxTurns(w.allowBash)
	result := s.invokeModel(ctx, claude.Request{
		SettingsPath:    s.settings,
		AuthMode:        claude.AuthGateway,
		WorkDir:         w.workDir,
		Prompt:          workerStartBrief(in),
		Model:           workerModel,
		Effort:          workerEffort,
		Role:            "worker",
		RootSessionID:   rootSessionID,
		ParentSessionID: parentSessionID,
		Tools:           s.workerTools(in.Write, in.Paths, w.allowBash),
		JSONSchema:      workerJSONSchema,
		MaxTurns:        maxTurns,
		Timeout:         time.Duration(w.admission.DeadlineMS) * time.Millisecond,
	})
	progress.finish("Worker model call returned; validating structured result")
	s.recordRouteModelCall(in.RouteID, "worker_start", workerModel, workerEffort, result, w.startAttempts > 1)

	w.mu.Lock()
	defer w.mu.Unlock()
	s.applyResult(w, result, maxTurns)
	if violation := s.changedPathViolation(w, result.ChangedPaths); violation != "" {
		w.state = "blocked"
		w.failureClass = failureScopeViolation
		w.error = violation
		w.retryEligible = false
		w.report = blockedReport(violation, result.ChangedPaths)
		s.updateSliceFromWorker(record, w)
		s.releaseLease(w)
		return workerOutputLocked(w), nil
	}
	if w.identity.ModelVerification == "mismatch" {
		w.state = "model_mismatch"
		w.failureClass = failureModelMismatch
		w.error = fmt.Sprintf("requested model %q resolved to %q", workerModel, result.ResolvedModel)
		w.retryEligible = false
		w.report = blockedReport(w.error, result.ChangedPaths)
		s.recordLaneFailure("start_worker", failureInfo{Class: failureModelMismatch, Detail: w.error})
		s.updateSliceFromWorker(record, w)
		s.releaseLease(w)
		return workerOutputLocked(w), nil
	}
	if !result.Success || result.SessionID == "" {
		if w.closing || (s.serverCtx != nil && s.serverCtx.Err() != nil) {
			w.state = "canceled"
			w.failureClass = failureNone
			w.error = "background Worker canceled by Supervisor or server shutdown"
			w.retryEligible = false
			w.report = blockedReport(w.error, result.ChangedPaths)
			s.updateSliceFromWorker(record, w)
			s.releaseLease(w)
			return workerOutputLocked(w), nil
		}
		info := classifyRunFailure(result, in.Write)
		w.failureClass = info.Class
		w.error = info.Detail
		w.retryEligible = info.RetryEligible && w.startAttempts < maxStartAttempts
		if in.Write && len(result.ChangedPaths) > 0 && result.SessionID != "" && (info.Class == failureMaxTurns || info.Class == failureTimeout) {
			w.state = "provisional"
			w.provisional = true
			w.retryEligible = false
			w.report = provisionalReport(info.Detail, result.ChangedPaths)
			s.updateSliceFromWorker(record, w)
			s.releaseLease(w)
			return workerOutputLocked(w), nil
		}
		s.recordLaneFailure("start_worker", info)
		switch {
		case result.SessionID != "":
			w.state = "blocked"
		case w.retryEligible:
			w.state = "retryable_failed"
		default:
			w.state = "error"
		}
		w.report = blockedReport(info.Detail, result.ChangedPaths)
		s.updateSliceFromWorker(record, w)
		s.releaseLease(w)
		if result.SessionID != "" {
			return workerOutputLocked(w), nil
		}
		return WorkerOutput{}, workerStartError(w)
	}
	report, err := decodeWorkerReport(result)
	if err != nil {
		w.state = "blocked"
		w.failureClass = failureInvalidOutput
		w.error = "invalid structured report: " + err.Error()
		w.retryEligible = false
		w.report = blockedReport(w.error, result.ChangedPaths)
		s.recordLaneFailure("start_worker", failureInfo{Class: failureInvalidOutput, Detail: w.error})
		s.updateSliceFromWorker(record, w)
		s.releaseLease(w)
		return workerOutputLocked(w), nil
	}
	w.report = report
	s.recordLaneHealthy("start_worker")
	w.state = report.Status
	w.provisional = false
	w.failureClass = failureNone
	w.error = ""
	w.retryEligible = false
	s.updateSliceFromWorker(record, w)
	if report.Status != "needs_capability" {
		s.releaseLease(w)
	}
	return workerOutputLocked(w), nil
}

// collectWorker is deliberately a single blocking rendezvous, not a polling
// surface. A successful collection consumes the result exactly once.
func (s *Server) collectWorker(ctx context.Context, _ *mcp.CallToolRequest, in WorkerCollectInput) (*mcp.CallToolResult, WorkerCollectOutput, error) {
	id := strings.TrimSpace(in.WorkerID)
	if id == "" {
		return nil, WorkerCollectOutput{}, fmt.Errorf("worker_id is required")
	}
	w, err := s.getWorker(id)
	if err != nil {
		return nil, WorkerCollectOutput{}, err
	}
	w.mu.Lock()
	if !w.background {
		w.mu.Unlock()
		return nil, WorkerCollectOutput{}, fmt.Errorf("worker %s was started synchronously and has no background result to collect", id)
	}
	if w.collected {
		w.mu.Unlock()
		return nil, WorkerCollectOutput{}, fmt.Errorf("worker %s background result was already collected", id)
	}
	if w.collecting {
		w.mu.Unlock()
		return nil, WorkerCollectOutput{}, fmt.Errorf("worker %s already has a collect in progress", id)
	}
	w.collecting = true
	done := w.done
	w.mu.Unlock()

	select {
	case <-done:
		w.mu.Lock()
		w.collecting = false
		w.collected = true
		out := workerCollectOutputLocked(w)
		w.mu.Unlock()
		digest := s.workerIntegrationDigest(w)
		w.mu.Lock()
		w.integration = digest
		w.mu.Unlock()
		out.Integration = digest
		s.recordRouteIntegrationResult(w.admission.RouteID, w.sliceID, digest)
		return nil, out, nil
	case <-ctx.Done():
		w.mu.Lock()
		w.collecting = false
		w.mu.Unlock()
		return nil, WorkerCollectOutput{}, fmt.Errorf("collect worker %s: %w", id, ctx.Err())
	}
}

func (s *Server) resumeWorker(ctx context.Context, req *mcp.CallToolRequest, in WorkerResumeInput) (*mcp.CallToolResult, WorkerOutput, error) {
	if strings.TrimSpace(in.WorkerID) == "" || strings.TrimSpace(in.EvidencePacket) == "" {
		return nil, WorkerOutput{}, fmt.Errorf("worker_id and evidence_packet are required")
	}
	if len(in.EvidencePacket) > 32000 || len(in.Instruction) > 4000 {
		return nil, WorkerOutput{}, fmt.Errorf("resume packet too large; evidence<=32000, instruction<=4000 bytes")
	}
	w, err := s.getWorker(in.WorkerID)
	if err != nil {
		return nil, WorkerOutput{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.background && !w.collected {
		return nil, WorkerOutput{}, fmt.Errorf("worker %s background result must be collected before resume", w.id)
	}
	if w.state == "closed" {
		return nil, WorkerOutput{}, fmt.Errorf("worker %s is closed", w.id)
	}
	if w.state == "model_mismatch" || w.failureClass == failureScopeViolation {
		return nil, WorkerOutput{}, fmt.Errorf("worker %s cannot resume after %s", w.id, valueOr(w.failureClass, w.state))
	}
	if w.sessionID == "" {
		return nil, WorkerOutput{}, fmt.Errorf("worker %s has no resumable session", w.id)
	}
	if w.turn >= 1+maxWorkerResumes {
		return nil, WorkerOutput{}, fmt.Errorf("worker %s resume budget exhausted: max %d resumes", w.id, maxWorkerResumes)
	}
	maxTurns := workerInvokeMaxTurns(w.cumulativeModelTurns)
	if maxTurns <= 0 {
		return nil, WorkerOutput{}, fmt.Errorf("worker %s cumulative model turns %d exhausted (cap %d); split the slice instead of raising MaxTurns", w.id, w.cumulativeModelTurns, maxCumulativeWorkerModelTurns)
	}
	if err := s.acquireLease(w); err != nil {
		return nil, WorkerOutput{}, err
	}
	if err := s.acquire(ctx); err != nil {
		s.releaseLease(w)
		return nil, WorkerOutput{}, err
	}
	defer s.release()
	if err := s.reserveWorkerTurn(); err != nil {
		s.releaseLease(w)
		return nil, WorkerOutput{}, err
	}

	w.turn++
	w.state = "running"
	rootSessionID, parentSessionID := s.threadBinding()
	progress := s.beginWorkerProgress(ctx, req, w.id, workerModel+"/"+workerEffort)
	result := s.invokeModel(ctx, claude.Request{
		SettingsPath:    s.settings,
		AuthMode:        claude.AuthGateway,
		WorkDir:         w.workDir,
		Prompt:          workerResumeBrief(in),
		Model:           workerModel,
		Effort:          workerEffort,
		Role:            "worker",
		RootSessionID:   rootSessionID,
		ParentSessionID: parentSessionID,
		ResumeSession:   w.sessionID,
		Tools:           s.workerTools(w.write, w.paths, w.allowBash),
		JSONSchema:      workerJSONSchema,
		MaxTurns:        maxTurns,
		Timeout:         time.Duration(w.deadlineMS) * time.Millisecond,
	})
	progress.finish("Worker resume returned; validating structured result")
	s.recordRouteModelCall(w.admission.RouteID, "worker_resume", workerModel, workerEffort, result, false)
	s.applyResult(w, result, maxTurns)
	if violation := s.changedPathViolation(w, result.ChangedPaths); violation != "" {
		w.state, w.failureClass, w.error, w.retryEligible = "blocked", failureScopeViolation, violation, false
		w.report = blockedReport(violation, result.ChangedPaths)
		s.updateSliceForWorker(w)
		s.releaseLease(w)
		return nil, workerOutputLocked(w), nil
	}
	if w.identity.ModelVerification == "mismatch" {
		w.state, w.failureClass, w.retryEligible = "model_mismatch", failureModelMismatch, false
		w.error = fmt.Sprintf("requested model %q resolved to %q", workerModel, result.ResolvedModel)
		w.report = blockedReport(w.error, result.ChangedPaths)
		s.recordLaneFailure("start_worker", failureInfo{Class: failureModelMismatch, Detail: w.error})
		s.updateSliceForWorker(w)
		s.releaseLease(w)
		return nil, workerOutputLocked(w), nil
	}
	if !result.Success {
		info := classifyRunFailure(result, w.write)
		if w.write && len(result.ChangedPaths) > 0 && (info.Class == failureMaxTurns || info.Class == failureTimeout) {
			w.state, w.failureClass, w.error, w.retryEligible = "provisional", info.Class, info.Detail, false
			w.provisional = true
			w.report = provisionalReport(info.Detail, result.ChangedPaths)
			s.updateSliceForWorker(w)
			s.releaseLease(w)
			return nil, workerOutputLocked(w), nil
		}
		s.recordLaneFailure("start_worker", info)
		w.state, w.failureClass, w.error, w.retryEligible = "blocked", info.Class, info.Detail, false
		w.report = blockedReport(info.Detail, result.ChangedPaths)
		s.updateSliceForWorker(w)
		s.releaseLease(w)
		return nil, workerOutputLocked(w), nil
	}
	report, err := decodeWorkerReport(result)
	if err != nil {
		w.state, w.failureClass, w.retryEligible = "blocked", failureInvalidOutput, false
		w.error = "invalid structured report: " + err.Error()
		w.report = blockedReport(w.error, result.ChangedPaths)
		s.recordLaneFailure("start_worker", failureInfo{Class: failureInvalidOutput, Detail: w.error})
		s.updateSliceForWorker(w)
		s.releaseLease(w)
		return nil, workerOutputLocked(w), nil
	}
	s.recordLaneHealthy("start_worker")
	w.report, w.state, w.failureClass, w.error, w.retryEligible = report, report.Status, failureNone, "", false
	w.provisional = false
	s.updateSliceForWorker(w)
	if report.Status != "needs_capability" {
		s.releaseLease(w)
	}
	out := workerOutputLocked(w)
	if w.write {
		digest := s.workerIntegrationDigest(w)
		w.integration = digest
		out.Integration = digest
		s.recordRouteIntegrationResult(w.admission.RouteID, w.sliceID, digest)
	}
	return nil, out, nil
}

func (s *Server) closeWorker(ctx context.Context, _ *mcp.CallToolRequest, in WorkerCloseInput) (*mcp.CallToolResult, WorkerStatus, error) {
	w, err := s.getWorker(in.WorkerID)
	if err != nil {
		return nil, WorkerStatus{}, err
	}
	w.mu.Lock()
	backgroundRunning := w.background && !channelClosed(w.done)
	w.closing = backgroundRunning
	cancel := w.cancel
	done := w.done
	w.mu.Unlock()
	if backgroundRunning {
		if cancel != nil {
			cancel()
		}
		select {
		case <-done:
		case <-ctx.Done():
			return nil, WorkerStatus{}, fmt.Errorf("close worker %s: %w", w.id, ctx.Err())
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	s.releaseLease(w)
	w.state = "closed"
	s.updateSliceForWorker(w)
	return nil, workerStatusLocked(w), nil
}

func channelClosed(ch <-chan struct{}) bool {
	if ch == nil {
		return true
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (s *Server) applyResult(w *workerState, result claude.Result, requestedMaxTurns int) {
	w.durationMS += result.DurationMS
	w.toolUses = mergeToolUses(w.toolUses, result.ToolUses)
	w.usage = addTokenUsage(w.usage, tokenUsage(result.Usage))
	w.identity = executionIdentity(workerModel, workerEffort, result)
	turns, quality := result.AccountedTurns(requestedMaxTurns)
	w.cumulativeModelTurns += turns
	// T6: aggregate quality is the worst sample ever applied (unknown > upper_bound > exact).
	// An earlier upper_bound charge must not be re-labeled exact by a later exact sample.
	w.turnAccountingQuality = worseTurnQuality(w.turnAccountingQuality, quality)
	if result.SessionID != "" {
		w.sessionID = result.SessionID
	}
}

// worseTurnQuality returns the more conservative of two accounting qualities.
// Order (worst first): unknown > upper_bound > exact > "".
func worseTurnQuality(a, b string) string {
	rank := func(q string) int {
		switch q {
		case "unknown":
			return 3
		case "upper_bound":
			return 2
		case "exact":
			return 1
		default:
			return 0
		}
	}
	if rank(b) > rank(a) {
		return b
	}
	if a == "" {
		return b
	}
	return a
}

// workerInvokeMaxTurns keeps per-invoke MaxTurns ≤ 12 and never exceeds the
// remaining cumulative budget. The deadline remains the primary wall-clock
// guard; the extra two turns avoid discarding a valid scoped patch while the
// worker is formatting its final evidence packet.
func workerInvokeMaxTurns(cumulative int) int {
	const perInvoke = maxWorkerTurns
	remaining := maxCumulativeWorkerModelTurns - cumulative
	if remaining <= 0 {
		return 0
	}
	if remaining < perInvoke {
		return remaining
	}
	return perInvoke
}

func workerInitialMaxTurns(allowBash bool) int {
	if allowBash {
		return workerInvokeMaxTurns(0)
	}
	// Root-only-verifier Workers need only read, edit, and package evidence.
	// Eight turns preserve one repair/formatting turn while bounding drift.
	return 8
}

func blockedReport(summary string, changed []string) WorkerReport {
	return WorkerReport{Status: "blocked", Summary: summary, Evidence: []string{}, ChangedPaths: append([]string(nil), changed...), Verification: []string{}, Needs: []CapabilityNeed{}}
}

func workerStartError(w *workerState) error {
	detail := fmt.Sprintf("worker %s failed: class=%s retry_eligible=%t: %s", w.id, w.failureClass, w.retryEligible, w.error)
	if w.retryEligible {
		detail += "; after repairing the infrastructure, call start_worker once with the identical slice and a non-empty retry_reason; do not change model or create a replacement slice_id"
	}
	return fmt.Errorf("%s", detail)
}

func (s *Server) changedPathViolation(w *workerState, changed []string) string {
	for _, raw := range changed {
		path := filepath.Clean(raw)
		if filepath.IsAbs(path) {
			rel, err := filepath.Rel(s.root, path)
			if err != nil {
				return fmt.Sprintf("cannot validate changed path %q: %v", raw, err)
			}
			path = rel
		}
		if path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
			return fmt.Sprintf("worker changed path outside launch root: %s", raw)
		}
		if !w.write {
			return fmt.Sprintf("read-only worker attempted to change %s", raw)
		}
		allowed := false
		for _, scope := range w.paths {
			if scope == "." || path == scope || strings.HasPrefix(path, scope+string(filepath.Separator)) {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Sprintf("worker changed %s outside owned scopes %v", raw, w.paths)
		}
	}
	return ""
}

func (s *Server) ensureSliceStateLocked() {
	if s.workers == nil {
		s.workers = map[string]*workerState{}
	}
	if s.attemptedSlices == nil {
		s.attemptedSlices = map[string]int{}
	}
	if s.preparingSlices == nil {
		s.preparingSlices = map[string]bool{}
	}
	if s.sliceInputs == nil {
		s.sliceInputs = map[string]WorkerStartInput{}
	}
}

func (s *Server) recordRejectedAdmission(admission WorkerAdmission, in WorkerStartInput) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSliceStateLocked()
	s.sliceHistory = append(s.sliceHistory, &sliceState{input: in, status: SliceStatus{
		Admission: admission, StartAttempts: s.attemptedSlices[admission.SliceID], State: "rejected", Error: strings.Join(admission.RejectionReasons, "; "),
	}})
}

func (s *Server) reserveSlice(admission WorkerAdmission, in WorkerStartInput) (*sliceState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSliceStateLocked()
	attempts := s.attemptedSlices[admission.SliceID]
	if s.preparingSlices[admission.SliceID] {
		return nil, s.rejectSliceLocked(admission, in, attempts, "slice_id start is already in progress")
	}
	if attempts == 0 {
		if strings.TrimSpace(in.RetryReason) != "" {
			return nil, s.rejectSliceLocked(admission, in, attempts, "retry_reason is allowed only after retry_eligible=true")
		}
		s.sliceInputs[admission.SliceID] = canonicalSliceInput(in)
	} else {
		latest := s.latestSliceLocked(admission.SliceID)
		if attempts >= maxStartAttempts || latest == nil || !latest.status.RetryEligible {
			return nil, s.rejectSliceLocked(admission, in, attempts, "slice_id has no eligible same-lane start retry")
		}
		if strings.TrimSpace(in.RetryReason) == "" {
			return nil, s.rejectSliceLocked(admission, in, attempts, "retry_reason is required for the one eligible same-lane retry")
		}
		if !sameSliceInput(s.sliceInputs[admission.SliceID], in) {
			return nil, s.rejectSliceLocked(admission, in, attempts, "retry must keep the original objective, context, output contract, done condition, deadline, workdir, write mode, and paths")
		}
	}
	s.preparingSlices[admission.SliceID] = true
	record := &sliceState{input: in, status: SliceStatus{Admission: admission, StartAttempts: attempts, State: "admitted"}}
	s.sliceHistory = append(s.sliceHistory, record)
	return record, nil
}

func (s *Server) rejectSliceLocked(admission WorkerAdmission, in WorkerStartInput, attempts int, reason string) error {
	admission.Result = admissionRejected
	admission.RejectionReasons = append(admission.RejectionReasons, reason)
	s.sliceHistory = append(s.sliceHistory, &sliceState{input: in, status: SliceStatus{Admission: admission, StartAttempts: attempts, State: "rejected", Error: reason}})
	return admissionError(admission)
}

func (s *Server) latestSliceLocked(sliceID string) *sliceState {
	for i := len(s.sliceHistory) - 1; i >= 0; i-- {
		if s.sliceHistory[i].status.Admission.SliceID == sliceID && s.sliceHistory[i].status.State != "rejected" {
			return s.sliceHistory[i]
		}
	}
	return nil
}

func canonicalSliceInput(in WorkerStartInput) WorkerStartInput {
	in.RetryReason = ""
	if in.DeadlineMS == 0 {
		in.DeadlineMS = defaultDeadlineMS
	}
	in.SliceID = strings.TrimSpace(in.SliceID)
	in.RouteID = strings.TrimSpace(in.RouteID)
	in.Objective = strings.TrimSpace(in.Objective)
	in.MarginalContribution = strings.TrimSpace(in.MarginalContribution)
	in.Context = strings.TrimSpace(in.Context)
	in.OutputContract = strings.TrimSpace(in.OutputContract)
	in.DoneCondition = strings.TrimSpace(in.DoneCondition)
	in.WorkDir = strings.TrimSpace(in.WorkDir)
	paths := make([]string, 0, len(in.Paths))
	for _, path := range in.Paths {
		if value := strings.TrimSpace(path); value != "" {
			paths = append(paths, filepath.Clean(value))
		}
	}
	sort.Strings(paths)
	in.Paths = paths
	return in
}

func sameSliceInput(first, retry WorkerStartInput) bool {
	first = canonicalSliceInput(first)
	retry = canonicalSliceInput(retry)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(retry)
	return string(a) == string(b)
}

func (s *Server) clearPreparingSlice(sliceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.preparingSlices, sliceID)
}

func (s *Server) rejectPreparedSlice(record *sliceState, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	admission := &record.status.Admission
	admission.Result = admissionRejected
	admission.RejectionReasons = append(admission.RejectionReasons, reason)
	record.status.StartAttempts = s.attemptedSlices[admission.SliceID]
	record.status.State = "rejected"
	record.status.Error = reason
	delete(s.preparingSlices, admission.SliceID)
}

func (s *Server) markSliceStarted(record *sliceState, w *workerState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureSliceStateLocked()
	attempts := s.attemptedSlices[w.sliceID]
	if attempts >= maxStartAttempts {
		return rejectPreparedStartLocked(record, attempts, "slice_id start attempt limit reached")
	}
	if s.workerStarts.Load() >= maxWorkerThreads {
		return rejectPreparedStartLocked(record, attempts, fmt.Sprintf("worker thread budget exhausted: hard cap is %d", maxWorkerThreads))
	}
	if s.workerTurns.Load() >= maxWorkerTurns {
		return rejectPreparedStartLocked(record, attempts, fmt.Sprintf("worker turn budget exhausted: hard cap is %d", maxWorkerTurns))
	}
	if routeID := strings.TrimSpace(w.admission.RouteID); routeID != "" {
		if route := s.routes[routeID]; route != nil {
			limit := route.Plan.WorkerPolicy.MaxWorkerStarts
			started := 0
			for _, existing := range s.workers {
				if existing.admission.RouteID == routeID {
					started++
				}
			}
			if limit < 1 || started >= limit {
				return rejectPreparedStartLocked(record, attempts, fmt.Sprintf("route worker budget exhausted: started=%d max=%d; Supervisor owns %d remaining slice(s)", started, limit, route.Plan.WorkerPolicy.RootOwnedSlices))
			}
		}
	}
	attempts++
	s.attemptedSlices[w.sliceID] = attempts
	s.workerStarts.Add(1)
	s.workerTurns.Add(1)
	w.startAttempts = attempts
	record.status.StartAttempts = attempts
	record.status.WorkerID = w.id
	record.status.State = "starting"
	s.workers[w.id] = w
	delete(s.preparingSlices, w.sliceID)
	return nil
}

func rejectPreparedStartLocked(record *sliceState, attempts int, reason string) error {
	admission := &record.status.Admission
	admission.Result = admissionRejected
	admission.RejectionReasons = append(admission.RejectionReasons, reason)
	record.status.StartAttempts = attempts
	record.status.State = "rejected"
	record.status.Error = reason
	return admissionError(*admission)
}

func (s *Server) reserveWorkerTurn() error {
	if s.workerTurns.Add(1) > maxWorkerTurns {
		s.workerTurns.Add(-1)
		return fmt.Errorf("worker turn budget exhausted: hard cap is %d", maxWorkerTurns)
	}
	return nil
}

func (s *Server) updateSliceFromWorker(record *sliceState, w *workerState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.status = sliceStatusLocked(w)
}

func (s *Server) updateSliceForWorker(w *workerState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.sliceHistory) - 1; i >= 0; i-- {
		if s.sliceHistory[i].status.WorkerID == w.id {
			s.sliceHistory[i].status = sliceStatusLocked(w)
			return
		}
	}
}

func sliceStatusLocked(w *workerState) SliceStatus {
	identity := w.identity
	usage := w.usage
	return SliceStatus{
		Admission: w.admission, StartAttempts: w.startAttempts, WorkerID: w.id, Identity: &identity,
		SessionID: w.sessionID, State: w.state, FailureClass: w.failureClass, RetryEligible: w.retryEligible,
		Error: w.error, DurationMS: w.durationMS, ToolUses: cloneToolUses(w.toolUses), Usage: &usage,
		Provisional: w.provisional,
	}
}

func (s *Server) workerTools(write bool, paths []string, allowBash bool) []string {
	tools := []string{"Read", "Grep", "Glob"}
	if !write {
		return tools
	}
	// A single explicit write target does not need repository-wide discovery.
	// Bash is exposed only for one exact frozen verifier. Descriptive verifier
	// prose keeps execution Root-owned, preventing interpreter/dependency search
	// and installation loops. Include Write only for a new leased target.
	if len(paths) == 1 {
		tools = []string{"Read", "Edit"}
		if allowBash {
			tools = append(tools, "Bash")
		}
		if _, err := os.Stat(filepath.Join(s.root, paths[0])); os.IsNotExist(err) {
			tools = append(tools, "Write")
		}
		return tools
	}
	// Multi-file slices may need symbol search, but explicit leased paths make
	// Glob unnecessary. Unknown-scope exploration belongs in repo_explore.
	tools = []string{"Read", "Grep", "Edit", "Write"}
	if allowBash {
		tools = append(tools, "Bash")
	}
	return tools
}

func workerVerifierInstruction(doneCondition string) string {
	if command, ok := exactWorkerVerifier(doneCondition); ok {
		return fmt.Sprintf("Worker verifier mode: exact-once. After the edit, run exactly `%s` at most once. Do not substitute commands, probe interpreters, install dependencies, search outside the repository, chain diff/cleanup, or retry a failed/unavailable verifier; report that evidence to Root immediately.", command)
	}
	return "Worker verifier mode: Root-only. The done condition is descriptive rather than one literal executable command, so Bash is withheld. Make the bounded patch, report verification as unverified, and return immediately; do not search for runners, install dependencies, or simulate a verifier."
}

func workerStartBrief(in WorkerStartInput) string {
	mode := "read-only"
	if in.Write {
		mode = "write-enabled only inside these exclusive scopes: " + strings.Join(in.Paths, ", ")
	}
	return fmt.Sprintf(`You are a persistent Grok 4.5 high worker. The GPT-5.6 Sol supervisor retains the parent objective and final acceptance; its launch-time effort may be medium, high, or xhigh.

Route ID: %s

Slice ID: %s

Goal:
%s

Marginal contribution:
%s

Owned scope:
%s

Inputs:
%s

Output contract:
%s

Done condition:
%s

%s

Mode: %s

Rules:
- Own only this slice; do not restate or broaden the parent task.
- Use the narrowest local tools and verification.
- When one owned file is explicit, read that target and its named specification directly; do not spend turns rediscovering paths. Use at most one reconnaissance round before the first edit.
- Reserve the final model turn for StructuredOutput. Once the owned change is complete and the verifier policy above is satisfied, stop immediately; do not repeat checks or clean unrelated generated files.
- Do not browse, invoke another model, or spawn agents.
- Request exactly one needs_capability item only for a material external_search, url_digest, repo_explore, find_thread, or read_thread gap. Use find_thread when the missing fact is which prior Thread to read; then read only the selected Thread.
- Report facts, changed paths, verification, and residual risk honestly.
- Always finish with StructuredOutput.`, valueOr(in.RouteID, "Direct Worker call without route_task."), in.SliceID, in.Objective, in.MarginalContribution, valueOr(strings.Join(in.Paths, ", "), "No write scope; read-only"), valueOr(in.Context, "No additional context supplied."), in.OutputContract, in.DoneCondition, workerVerifierInstruction(in.DoneCondition), mode)
}

func workerResumeBrief(in WorkerResumeInput) string {
	return fmt.Sprintf(`Continue the same bounded worker objective in this existing session. Do not restart completed analysis.

New evidence or verifier feedback:
%s

Supervisor instruction:
%s

Finish the original output contract or repair only the evidenced failure. Preserve sources, distinguish facts from assumptions, do not fan out, and always finish with StructuredOutput.`, in.EvidencePacket, valueOr(in.Instruction, "Continue toward the original done condition."))
}

func decodeWorkerReport(result claude.Result) (WorkerReport, error) {
	var report WorkerReport
	raw := result.Structured
	if len(raw) == 0 {
		raw = json.RawMessage(result.Text)
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return WorkerReport{}, err
	}
	if err := validateWorkerReport(raw, report); err != nil {
		return WorkerReport{}, err
	}
	return report, nil
}

func workerOutputLocked(w *workerState) WorkerOutput {
	return WorkerOutput{
		SliceID: w.sliceID, Admission: w.admission, StartAttempts: w.startAttempts, WorkerID: w.id,
		Identity: w.identity, SessionID: w.sessionID, Turn: w.turn,
		CumulativeModelTurns: w.cumulativeModelTurns, TurnAccountingQuality: w.turnAccountingQuality,
		State: w.state, Report: w.report,
		ToolUses: cloneToolUses(w.toolUses), Usage: w.usage, DurationMS: w.durationMS,
		FailureClass: w.failureClass, RetryEligible: w.retryEligible, Error: w.error,
		Background: w.background, Collected: w.collected, Provisional: w.provisional,
	}
}

func workerCollectOutputLocked(w *workerState) WorkerCollectOutput {
	return WorkerCollectOutput{
		SliceID: w.sliceID, WorkerID: w.id, Identity: w.identity, SessionID: w.sessionID,
		State: w.state, Report: w.report, DurationMS: w.durationMS,
		FailureClass: w.failureClass, RetryEligible: w.retryEligible,
		Background: w.background, Collected: w.collected, Provisional: w.provisional,
		Integration: w.integration,
	}
}

const maxIntegrationArtifactBytes = 1 << 20

var diffCheckLineRE = regexp.MustCompile(`^(.*):([0-9]+): (trailing whitespace\.|new blank line at EOF\.)$`)

func (s *Server) workerIntegrationDigest(w *workerState) *IntegrationDigest {
	paths := append([]string(nil), w.paths...)
	digest := &IntegrationDigest{
		OwnedPaths:     paths,
		DiffCheck:      "unavailable",
		ReviewContract: "Use the compact diff stat and acceptance report first. Read the patch artifact only for a concrete residual risk or repair; do not reread owned files. The Root verifier remains mandatory.",
	}
	if len(paths) == 0 && !w.write {
		digest.DiffCheck = "pass"
		digest.ReviewContract = "Read-only Worker has no integration patch. Use its acceptance report, then run the Root verifier once."
		return digest
	}
	if len(paths) == 0 || strings.TrimSpace(w.workDir) == "" {
		return digest
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	patch, err := runGitCapture(ctx, w.workDir, append([]string{"diff", "--no-ext-diff", "--unified=1", "--"}, paths...)...)
	if err != nil {
		digest.ReviewContract = "Runtime could not produce a scoped patch; Root may inspect each owned path once before verification."
		return digest
	}
	if stat, statErr := runGitCapture(ctx, w.workDir, append([]string{"diff", "--stat", "--"}, paths...)...); statErr == nil {
		digest.DiffStat = compactText(string(stat), 1000)
	}
	checkOut, checkErr := runGitCapture(ctx, w.workDir, append([]string{"diff", "--check", "--"}, paths...)...)
	if checkErr != nil {
		if fixes, ok := autoFixOwnedDiffHygiene(w.workDir, paths, string(checkOut)); ok {
			digest.AutoFixed = true
			digest.AutoFixes = fixes
			patch, err = runGitCapture(ctx, w.workDir, append([]string{"diff", "--no-ext-diff", "--unified=1", "--"}, paths...)...)
			if err == nil {
				checkOut, checkErr = runGitCapture(ctx, w.workDir, append([]string{"diff", "--check", "--"}, paths...)...)
			}
		}
	}
	if checkErr == nil {
		digest.DiffCheck = "pass"
	} else {
		digest.DiffCheck = "fail: " + compactText(string(checkOut), 1000)
	}
	digest.PatchBytes = len(patch)
	digest.PatchSHA256 = fmt.Sprintf("%x", sha256.Sum256(patch))
	artifactPatch := patch
	if len(artifactPatch) > maxIntegrationArtifactBytes {
		artifactPatch = artifactPatch[:maxIntegrationArtifactBytes]
		digest.PatchTruncated = true
	}
	if len(artifactPatch) > 0 {
		if path, writeErr := writeIntegrationArtifact(w.admission.RouteID, w.sliceID, artifactPatch); writeErr == nil {
			digest.ArtifactPath = path
		} else {
			digest.ReviewContract = "Patch artifact write failed; Root may inspect the scoped diff once before verification."
		}
	}
	return digest
}

func runGitCapture(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if stdout.Len() == 0 && stderr.Len() > 0 {
		return stderr.Bytes(), err
	}
	return stdout.Bytes(), err
}

func compactText(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) > limit {
		return value[:limit] + "...[truncated]"
	}
	return value
}

type hygieneEdit struct {
	line int
	kind string
}

func autoFixOwnedDiffHygiene(workDir string, ownedPaths []string, output string) ([]string, bool) {
	owned := make(map[string]bool, len(ownedPaths))
	for _, path := range ownedPaths {
		owned[filepath.ToSlash(filepath.Clean(path))] = true
	}
	edits := map[string][]hygieneEdit{}
	lastWasMessage := false
	for _, raw := range strings.Split(strings.TrimSpace(output), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		match := diffCheckLineRE.FindStringSubmatch(line)
		if match == nil {
			if lastWasMessage && strings.HasPrefix(raw, "+") {
				lastWasMessage = false
				continue
			}
			return nil, false
		}
		path := filepath.ToSlash(filepath.Clean(match[1]))
		if !owned[path] {
			return nil, false
		}
		lineNo, err := strconv.Atoi(match[2])
		if err != nil || lineNo < 1 {
			return nil, false
		}
		edits[path] = append(edits[path], hygieneEdit{line: lineNo, kind: match[3]})
		lastWasMessage = true
	}
	if len(edits) == 0 {
		return nil, false
	}
	type pendingWrite struct {
		path string
		data []byte
		mode os.FileMode
	}
	var writes []pendingWrite
	var fixes []string
	for path, pathEdits := range edits {
		full := filepath.Join(workDir, filepath.FromSlash(path))
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, false
		}
		data, err := os.ReadFile(full)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil, false
		}
		text := string(data)
		lines := strings.Split(text, "\n")
		normalizeEOF := false
		for _, edit := range pathEdits {
			switch edit.kind {
			case "trailing whitespace.":
				if edit.line > len(lines) {
					return nil, false
				}
				value := lines[edit.line-1]
				cr := strings.HasSuffix(value, "\r")
				value = strings.TrimSuffix(value, "\r")
				value = strings.TrimRight(value, " \t")
				if cr {
					value += "\r"
				}
				lines[edit.line-1] = value
				fixes = append(fixes, path+": trimmed introduced trailing whitespace")
			case "new blank line at EOF.":
				normalizeEOF = true
			}
		}
		text = strings.Join(lines, "\n")
		if normalizeEOF {
			ending := "\n"
			if strings.Contains(text, "\r\n") {
				ending = "\r\n"
			}
			text = strings.TrimRight(text, "\r\n")
			if text != "" {
				text += ending
			}
			fixes = append(fixes, path+": normalized blank lines at EOF")
		}
		writes = append(writes, pendingWrite{path: full, data: []byte(text), mode: info.Mode().Perm()})
	}
	for _, write := range writes {
		if err := os.WriteFile(write.path, write.data, write.mode); err != nil {
			return nil, false
		}
	}
	sort.Strings(fixes)
	return fixes, true
}

func writeIntegrationArtifact(routeID, sliceID string, patch []byte) (string, error) {
	dir := strings.TrimSpace(os.Getenv("CLAUDEX_INTEGRATION_ARTIFACT_DIR"))
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config", "claudex", "integration-artifacts")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := safeArtifactName(routeID) + "-" + safeArtifactName(sliceID) + ".patch"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, patch, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func safeArtifactName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unbound"
	}
	var out strings.Builder
	for _, r := range value {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	return out.String()
}

func provisionalReport(summary string, changed []string) WorkerReport {
	return WorkerReport{
		Status:   "provisional",
		Summary:  "Worker reached its bounded turn/deadline after producing an in-scope patch. Treat the patch as untrusted until one Supervisor verifier passes. " + summary,
		Evidence: []string{}, ChangedPaths: append([]string(nil), changed...),
		Verification: []string{}, Needs: []CapabilityNeed{},
	}
}

func runFailure(result claude.Result) string {
	detail := result.FailureDetail()
	if len(detail) > 2000 {
		detail = detail[:2000] + "...[truncated]"
	}
	return detail
}

func tokenUsage(usage claude.Usage) TokenUsage {
	return TokenUsage{InputTokens: usage.InputTokens, CacheCreationTokens: usage.CacheCreationTokens, CacheReadTokens: usage.CacheReadTokens, OutputTokens: usage.OutputTokens}
}

func addTokenUsage(a, b TokenUsage) TokenUsage {
	return TokenUsage{
		InputTokens:         a.InputTokens + b.InputTokens,
		CacheCreationTokens: a.CacheCreationTokens + b.CacheCreationTokens,
		CacheReadTokens:     a.CacheReadTokens + b.CacheReadTokens,
		OutputTokens:        a.OutputTokens + b.OutputTokens,
	}
}

func mergeToolUses(a, b map[string]int) map[string]int {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := cloneToolUses(a)
	if out == nil {
		out = map[string]int{}
	}
	for name, count := range b {
		out[name] += count
	}
	return out
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
