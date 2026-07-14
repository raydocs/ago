package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"claudexflow/internal/claude"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const workerModel = "grok-4.5"
const workerEffort = "high"

func (s *Server) startWorker(ctx context.Context, _ *mcp.CallToolRequest, in WorkerStartInput) (*mcp.CallToolResult, WorkerOutput, error) {
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

	dir, err := s.scopedDir(in.WorkDir)
	if err != nil {
		s.rejectPreparedSlice(record, err.Error())
		return nil, WorkerOutput{}, err
	}
	paths, err := s.normalizedPaths(in.Paths)
	if err != nil {
		s.rejectPreparedSlice(record, err.Error())
		return nil, WorkerOutput{}, err
	}
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
		turn:       1,
		deadlineMS: admission.DeadlineMS,
		state:      "starting",
		admission:  admission,
		toolUses:   map[string]int{},
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
	defer s.release()
	if err := s.markSliceStarted(record, w); err != nil {
		s.releaseLease(w)
		return nil, WorkerOutput{}, err
	}

	rootSessionID, parentSessionID := s.threadBinding()
	result := s.invokeModel(ctx, claude.Request{
		SettingsPath:    s.settings,
		AuthMode:        claude.AuthGateway,
		WorkDir:         dir,
		Prompt:          workerStartBrief(in),
		Model:           workerModel,
		Effort:          workerEffort,
		Role:            "worker",
		RootSessionID:   rootSessionID,
		ParentSessionID: parentSessionID,
		Tools:           workerTools(in.Write),
		JSONSchema:      workerJSONSchema,
		MaxTurns:        workerInvokeMaxTurns(0),
		Timeout:         time.Duration(admission.DeadlineMS) * time.Millisecond,
	})
	s.recordRouteModelCall(in.RouteID, "worker_start", workerModel, workerEffort, result, w.startAttempts > 1)

	w.mu.Lock()
	defer w.mu.Unlock()
	s.applyResult(w, result, workerInvokeMaxTurns(0))
	if violation := s.changedPathViolation(w, result.ChangedPaths); violation != "" {
		w.state = "blocked"
		w.failureClass = failureScopeViolation
		w.error = violation
		w.retryEligible = false
		w.report = blockedReport(violation, result.ChangedPaths)
		s.updateSliceFromWorker(record, w)
		s.releaseLease(w)
		return nil, workerOutputLocked(w), nil
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
		return nil, workerOutputLocked(w), nil
	}
	if !result.Success || result.SessionID == "" {
		info := classifyRunFailure(result, in.Write)
		s.recordLaneFailure("start_worker", info)
		w.failureClass = info.Class
		w.error = info.Detail
		w.retryEligible = info.RetryEligible && w.startAttempts < maxStartAttempts
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
			return nil, workerOutputLocked(w), nil
		}
		return nil, WorkerOutput{}, workerStartError(w)
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
		return nil, workerOutputLocked(w), nil
	}
	w.report = report
	s.recordLaneHealthy("start_worker")
	w.state = report.Status
	w.failureClass = failureNone
	w.error = ""
	w.retryEligible = false
	s.updateSliceFromWorker(record, w)
	if report.Status != "needs_capability" {
		s.releaseLease(w)
	}
	return nil, workerOutputLocked(w), nil
}

func (s *Server) resumeWorker(ctx context.Context, _ *mcp.CallToolRequest, in WorkerResumeInput) (*mcp.CallToolResult, WorkerOutput, error) {
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
		Tools:           workerTools(w.write),
		JSONSchema:      workerJSONSchema,
		MaxTurns:        maxTurns,
		Timeout:         time.Duration(w.deadlineMS) * time.Millisecond,
	})
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
	s.updateSliceForWorker(w)
	if report.Status != "needs_capability" {
		s.releaseLease(w)
	}
	return nil, workerOutputLocked(w), nil
}

func (s *Server) closeWorker(_ context.Context, _ *mcp.CallToolRequest, in WorkerCloseInput) (*mcp.CallToolResult, WorkerStatus, error) {
	w, err := s.getWorker(in.WorkerID)
	if err != nil {
		return nil, WorkerStatus{}, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	s.releaseLease(w)
	w.state = "closed"
	s.updateSliceForWorker(w)
	return nil, workerStatusLocked(w), nil
}

func (s *Server) applyResult(w *workerState, result claude.Result, requestedMaxTurns int) {
	w.durationMS += result.DurationMS
	w.toolUses = mergeToolUses(w.toolUses, result.ToolUses)
	w.usage = addTokenUsage(w.usage, tokenUsage(result.Usage))
	w.identity = executionIdentity(workerModel, workerEffort, result)
	turns, quality := result.AccountedTurns(requestedMaxTurns)
	w.cumulativeModelTurns += turns
	w.turnAccountingQuality = quality
	if result.SessionID != "" {
		w.sessionID = result.SessionID
	}
}

// workerInvokeMaxTurns keeps per-invoke MaxTurns ≤ 10 and never exceeds remaining cumulative budget.
func workerInvokeMaxTurns(cumulative int) int {
	const perInvoke = 10
	remaining := maxCumulativeWorkerModelTurns - cumulative
	if remaining <= 0 {
		return 0
	}
	if remaining < perInvoke {
		return remaining
	}
	return perInvoke
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
	}
}

func workerTools(write bool) []string {
	tools := []string{"Read", "Grep", "Glob"}
	if write {
		tools = append(tools, "Edit", "Write", "Bash")
	}
	return tools
}

func workerStartBrief(in WorkerStartInput) string {
	mode := "read-only"
	if in.Write {
		mode = "write-enabled only inside these exclusive scopes: " + strings.Join(in.Paths, ", ")
	}
	return fmt.Sprintf(`You are a persistent Grok 4.5 high worker. The GPT-5.6 Sol xhigh supervisor retains the parent objective and final acceptance.

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

Mode: %s

Rules:
- Own only this slice; do not restate or broaden the parent task.
- Use the narrowest local tools and verification.
- Do not browse, invoke another model, or spawn agents.
- Request exactly one needs_capability item only for a material external_search, url_digest, repo_explore, find_thread, or read_thread gap. Use find_thread when the missing fact is which prior Thread to read; then read only the selected Thread.
- Report facts, changed paths, verification, and residual risk honestly.
- Always finish with StructuredOutput.`, valueOr(in.RouteID, "Direct Worker call without route_task."), in.SliceID, in.Objective, in.MarginalContribution, valueOr(strings.Join(in.Paths, ", "), "No write scope; read-only"), valueOr(in.Context, "No additional context supplied."), in.OutputContract, in.DoneCondition, mode)
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
