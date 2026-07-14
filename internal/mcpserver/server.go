package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"claudexflow/internal/claude"
	"claudexflow/internal/router"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	settings       string
	root           string
	parentPID      int
	transcriptRoot string

	mu              sync.Mutex
	workers         map[string]*workerState
	leases          map[string]string
	attemptedSlices map[string]int
	preparingSlices map[string]bool
	sliceInputs     map[string]WorkerStartInput
	sliceHistory    []*sliceState
	laneHealth      map[string]router.LaneHealth
	routes          map[string]*RouteRecord
	routeLedgerPath string
	nextID          atomic.Int32
	nextRoute       atomic.Int32
	runModel        func(context.Context, claude.Request) claude.Result

	workerStarts    atomic.Int32
	workerTurns     atomic.Int32
	researchCalls   atomic.Int32
	nativeCalls     atomic.Int32
	threadFindCalls atomic.Int32
	threadReadCalls atomic.Int32
	activeRuns      atomic.Int32
	slots           chan struct{}
}

type workerState struct {
	mu            sync.Mutex
	id            string
	sliceID       string
	sessionID     string
	workDir       string
	write         bool
	paths         []string
	leased        bool
	turn          int
	deadlineMS    int64
	state         string
	report        WorkerReport
	admission     WorkerAdmission
	startAttempts int
	error         string
	durationMS    int64
	toolUses      map[string]int
	usage         TokenUsage
	identity      ExecutionIdentity
	failureClass  string
	retryEligible bool
}

type sliceState struct {
	status SliceStatus
	input  WorkerStartInput
}

func Run(ctx context.Context, version, settings string) error {
	root, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	s := &Server{
		settings:        settings,
		root:            root,
		parentPID:       os.Getppid(),
		transcriptRoot:  os.Getenv("CLAUDEX_TRANSCRIPT_ROOT"),
		workers:         map[string]*workerState{},
		leases:          map[string]string{},
		attemptedSlices: map[string]int{},
		preparingSlices: map[string]bool{},
		sliceInputs:     map[string]WorkerStartInput{},
		routes:          map[string]*RouteRecord{},
		routeLedgerPath: defaultRouteLedgerPath(),
		slots:           make(chan struct{}, maxConcurrentRuns),
		runModel:        claude.Run,
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "claudex-flow", Version: version}, nil)
	readOnly, openWorld, destructive := true, true, true

	mcp.AddTool(server, &mcp.Tool{
		Name:        "route_task",
		Description: "Zero-model prospective route comparison for a substantial task when the choice between Sol direct, one specialist capability, and one bounded Worker is materially uncertain. It reports task-shape factors, the frozen acceptance contract, live lane health, three candidates, relative resource intensity, worker admissibility, verification, and a one-dimension-at-a-time escalation ladder. Automatic or mandatory Worker routing requires observable acceptance_criteria, a concrete verification_target, and a truthful worker_marginal_contribution naming the Supervisor work or critical-path delay the Worker avoids. A quarantined lane is never silently replaced. It never launches a model or establishes a durable default from one heuristic.",
		Annotations: &mcp.ToolAnnotations{Title: "Plan accepted-result route", ReadOnlyHint: readOnly},
	}, s.routeTask)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "record_route_outcome",
		Description: "Close one route_id returned by route_task as accepted, failed, or abandoned. Accepted requires concrete verification evidence. Records human correction, residual risk, and child-call diagnostics without invoking a model, then appends one compact local JSONL record. Supervisor cost remains explicitly unobserved rather than estimated. Use only for substantial tasks that actually called route_task.",
		Annotations: &mcp.ToolAnnotations{Title: "Record accepted route outcome", ReadOnlyHint: false},
	}, s.recordRouteOutcome)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "start_worker",
		Description: "Create one persistent Grok 4.5 high worker for a bounded independent slice. Runtime admission requires a stable slice_id, marginal contribution, output contract, done condition, bounded deadline, and explicit non-overlapping write paths. Admission rejection invokes no model or budget. A failed start may be retried once only when runtime reports retry_eligible=true, with the identical slice and a retry_reason; no model fallback is performed.",
		Annotations: &mcp.ToolAnnotations{Title: "Start Grok 4.5 worker", ReadOnlyHint: false, DestructiveHint: &destructive},
	}, s.startWorker)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "resume_worker",
		Description: "Append evidence or verifier feedback to the same persistent Grok 4.5 high worker thread and wait for its next structured response. Always use the original worker_id; do not create a replacement worker or resend the full task. Typical loop: worker needs capability -> specialist returns evidence -> resume this worker -> verify -> optionally resume once with bounded failure evidence.",
		Annotations: &mcp.ToolAnnotations{Title: "Resume original worker", ReadOnlyHint: false, DestructiveHint: &destructive},
	}, s.resumeWorker)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_external",
		Description: "Use Grok 4.5 high with WebSearch/WebFetch for missing external, current, market, vendor, product, or X/Twitter information. Return a source-preserving evidence packet. Any role may request this capability, but the supervisor performs the call and injects the result back into the originating worker with resume_worker.",
		Annotations: &mcp.ToolAnnotations{Title: "Grok external research", ReadOnlyHint: readOnly, OpenWorldHint: &openWorld},
	}, s.searchExternal)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "digest_urls",
		Description: "Use Gemini 3.5 Flash medium with WebFetch to quickly extract or compare information from explicit URLs. It does not perform WebSearch. Return a source-preserving evidence packet that can be injected into the originating worker.",
		Annotations: &mcp.ToolAnnotations{Title: "Gemini URL digest", ReadOnlyHint: readOnly, OpenWorldHint: &openWorld},
	}, s.digestURLs)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "explore_repository",
		Description: "Use GPT-5.6 Terra high with Read/Grep/Glob for broad repository exploration, locating code, tracing dependencies, and mapping the smallest implementation surface. Return paths and symbol evidence for the originating worker. Do not use it for a trivial known-file read.",
		Annotations: &mcp.ToolAnnotations{Title: "Terra repository explorer", ReadOnlyHint: readOnly},
	}, s.exploreRepository)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "find_thread",
		Description: "Search prior local Claude X root Threads deterministically by keyword, file, project/repository, and date. Supports quoted phrases plus file:, project:/repo:, after:, and before: filters. Returns bounded sanitized candidate metadata and thread:// evidence without invoking a child model. Then call read_thread only for the selected candidate.",
		Annotations: &mcp.ToolAnnotations{Title: "Find prior Claude X threads", ReadOnlyHint: readOnly},
	}, s.findThread)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_thread",
		Description: "Use GLM 5.2 to extract one bounded answer from a prior local Claude X transcript. The runtime sanitizes secrets before the model call, uses the latest compaction only for orientation, selects original events for exact requirements, commands, chronology, edits, and verification, and caps returned evidence. It does not browse or read arbitrary files.",
		Annotations: &mcp.ToolAnnotations{Title: "Read prior Claude X thread", ReadOnlyHint: readOnly},
	}, s.readThread)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "consult_native_claude",
		Description: "Ask native Claude for one bounded read-only plan, review, judgment, or fresh-context verification using the local claude.ai subscription directly. Opus is default; Sonnet, Sonnet 1M, and Fable are available only when residual semantic risk justifies them.",
		Annotations: &mcp.ToolAnnotations{Title: "Native Claude consultation", ReadOnlyHint: readOnly},
	}, s.consultNativeClaude)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "close_worker",
		Description: "Close an abandoned or no-longer-needed worker and release its write-scope lease. Completed workers are released automatically.",
		Annotations: &mcp.ToolAnnotations{Title: "Close worker", ReadOnlyHint: readOnly},
	}, s.closeWorker)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "workflow_status",
		Description: "Inspect worker threads, states, turns, budgets, concurrency, and current-session lane health/quarantine evidence without invoking a model.",
		Annotations: &mcp.ToolAnnotations{Title: "Workflow status", ReadOnlyHint: readOnly},
	}, s.workflowStatus)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "runtime_contract",
		Description: "Return the zero-model Claude X runtime contract, available tools, profiles, and exact start_worker fields.",
		Annotations: &mcp.ToolAnnotations{Title: "Runtime contract", ReadOnlyHint: readOnly},
	}, s.runtimeContract)
	return server.Run(ctx, &mcp.StdioTransport{})
}

func (s *Server) invokeModel(ctx context.Context, request claude.Request) claude.Result {
	if s.runModel != nil {
		return s.runModel(ctx, request)
	}
	return claude.Run(ctx, request)
}

func (s *Server) acquire(ctx context.Context) error {
	select {
	case s.slots <- struct{}{}:
		s.activeRuns.Add(1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) release() {
	<-s.slots
	s.activeRuns.Add(-1)
}

func (s *Server) scopedDir(candidate string) (string, error) {
	root, err := filepath.EvalSymlinks(s.root)
	if err != nil {
		return "", fmt.Errorf("invalid launch directory: %w", err)
	}
	if strings.TrimSpace(candidate) == "" {
		return root, nil
	}
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(root, candidate)
	}
	abs, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("invalid workdir: %w", err)
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("workdir must stay inside Claude X launch directory %s", root)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("invalid workdir: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workdir is not a directory: %s", abs)
	}
	return abs, nil
}

func (s *Server) normalizedPaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if filepath.IsAbs(p) {
			rel, err := filepath.Rel(s.root, filepath.Clean(p))
			if err != nil {
				return nil, err
			}
			p = rel
		}
		p = filepath.Clean(p)
		if p == ".." || strings.HasPrefix(p, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("worker path must stay inside launch directory: %s", p)
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func overlaps(a, b string) bool {
	if a == "." || b == "." || a == b {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(a, b+sep) || strings.HasPrefix(b, a+sep)
}

func (s *Server) acquireLease(w *workerState) error {
	if !w.write || w.leased {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range w.paths {
		for held, owner := range s.leases {
			if owner != w.id && overlaps(p, held) {
				return fmt.Errorf("write scope %q overlaps worker %s scope %q", p, owner, held)
			}
		}
	}
	for _, p := range w.paths {
		s.leases[p] = w.id
	}
	w.leased = true
	return nil
}

func (s *Server) releaseLease(w *workerState) {
	if !w.leased {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, owner := range s.leases {
		if owner == w.id {
			delete(s.leases, p)
		}
	}
	w.leased = false
}

func (s *Server) getWorker(id string) (*workerState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workers[id]
	if !ok {
		return nil, fmt.Errorf("unknown worker_id %q", id)
	}
	return w, nil
}

func (s *Server) validateTargetWorker(id string) error {
	if id == "" {
		return nil
	}
	w, err := s.getWorker(id)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.state == "closed" {
		return fmt.Errorf("worker %s is closed", id)
	}
	return nil
}

func (s *Server) workflowStatus(_ context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, WorkflowStatusOutput, error) {
	s.mu.Lock()
	workers := make([]*workerState, 0, len(s.workers))
	for _, w := range s.workers {
		workers = append(workers, w)
	}
	slices := make([]SliceStatus, 0, len(s.sliceHistory))
	for _, record := range s.sliceHistory {
		status := cloneSliceStatus(record.status)
		if attempts := s.attemptedSlices[status.Admission.SliceID]; attempts > status.StartAttempts {
			status.StartAttempts = attempts
		}
		slices = append(slices, status)
	}
	routes := make([]RouteRecord, 0, len(s.routes))
	for _, record := range s.routes {
		routes = append(routes, cloneRouteRecord(*record))
	}
	s.mu.Unlock()
	statuses := make([]WorkerStatus, 0, len(workers))
	for _, w := range workers {
		w.mu.Lock()
		statuses = append(statuses, workerStatusLocked(w))
		w.mu.Unlock()
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].WorkerID < statuses[j].WorkerID })
	sort.Slice(routes, func(i, j int) bool { return routes[i].CreatedAt < routes[j].CreatedAt })
	return nil, WorkflowStatusOutput{
		Contract:        Contract(),
		Supervisor:      "gpt-5.6-sol/xhigh",
		WorkerProfile:   "grok-4.5/high",
		ActiveRuns:      int(s.activeRuns.Load()),
		WorkerStarts:    s.workerStarts.Load(),
		WorkerTurns:     s.workerTurns.Load(),
		ResearchCalls:   s.researchCalls.Load(),
		NativeCalls:     s.nativeCalls.Load(),
		ThreadFindCalls: s.threadFindCalls.Load(),
		ThreadReadCalls: s.threadReadCalls.Load(),
		Limits:          map[string]int{"concurrent_runs": maxConcurrentRuns, "worker_threads": maxWorkerThreads, "worker_turns": maxWorkerTurns, "worker_resumes_each": maxWorkerResumes, "worker_start_attempts_each": maxStartAttempts, "research_calls": maxResearchCalls, "native_claude_calls": maxNativeCalls, "thread_find_calls": maxThreadFindCalls, "thread_read_calls": maxThreadReadCalls},
		Workers:         statuses,
		Slices:          slices,
		LaneHealth:      s.liveLaneHealth(),
		Routes:          routes,
		RouteLedgerPath: s.routeLedgerPath,
		Timestamp:       time.Now(),
	}, nil
}

func workerStatusLocked(w *workerState) WorkerStatus {
	return WorkerStatus{
		SliceID:       w.sliceID,
		Admission:     cloneAdmission(w.admission),
		StartAttempts: w.startAttempts,
		WorkerID:      w.id,
		SessionID:     w.sessionID,
		State:         w.state,
		Turn:          w.turn,
		Write:         w.write,
		Paths:         append([]string(nil), w.paths...),
		Report:        w.report,
		Error:         w.error,
		DurationMS:    w.durationMS,
		ToolUses:      cloneToolUses(w.toolUses),
		Usage:         w.usage,
		Identity:      w.identity,
		FailureClass:  w.failureClass,
		RetryEligible: w.retryEligible,
	}
}

func cloneAdmission(in WorkerAdmission) WorkerAdmission {
	in.RejectionReasons = append([]string(nil), in.RejectionReasons...)
	return in
}

func cloneSliceStatus(in SliceStatus) SliceStatus {
	in.Admission = cloneAdmission(in.Admission)
	in.ToolUses = cloneToolUses(in.ToolUses)
	if in.Identity != nil {
		identity := *in.Identity
		in.Identity = &identity
	}
	if in.Usage != nil {
		usage := *in.Usage
		in.Usage = &usage
	}
	return in
}

func cloneToolUses(in map[string]int) map[string]int {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]int, len(in))
	for name, count := range in {
		out[name] = count
	}
	return out
}
