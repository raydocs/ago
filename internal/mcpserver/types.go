package mcpserver

import (
	"time"

	"claudexflow/internal/router"
	"claudexflow/internal/threadfind"
)

const (
	maxConcurrentRuns  = 3
	maxWorkerThreads   = 6
	maxWorkerTurns     = 12
	maxWorkerResumes   = 3
	maxResearchCalls   = 6
	maxNativeCalls     = 4
	maxThreadFindCalls = 4
	maxThreadReadCalls = 4
	maxStartAttempts   = 2
	defaultDeadlineMS  = int64(600_000)
	minDeadlineMS      = int64(30_000)
	maxDeadlineMS      = int64(600_000)
	// T6: hard cumulative model-turn estimate across start+resume (per-invoke MaxTurns stays 10).
	maxCumulativeWorkerModelTurns = 24
)

type CapabilityNeed struct {
	Kind     string   `json:"kind"`
	Question string   `json:"question"`
	Why      string   `json:"why"`
	URLs     []string `json:"urls"`
}

type WorkerReport struct {
	Status       string           `json:"status"`
	Summary      string           `json:"summary"`
	Evidence     []string         `json:"evidence"`
	ChangedPaths []string         `json:"changed_paths"`
	Verification []string         `json:"verification"`
	Needs        []CapabilityNeed `json:"needs"`
}

type EvidenceItem struct {
	Claim  string `json:"claim"`
	Source string `json:"source"`
	Detail string `json:"detail"`
}

type EvidenceReport struct {
	Status        string         `json:"status"`
	Summary       string         `json:"summary"`
	Items         []EvidenceItem `json:"items"`
	OpenQuestions []string       `json:"open_questions"`
}

// WorkerStartInput is intentionally compact. Runtime admission uses the
// marginal contribution and owned scope instead of invented time estimates.
type WorkerStartInput struct {
	RouteID              string   `json:"route_id,omitempty" jsonschema:"route_id returned by route_task when this Worker executes that selected route."`
	SliceID              string   `json:"slice_id" jsonschema:"Stable identifier for this independent slice in the root thread."`
	Objective            string   `json:"objective" jsonschema:"One bounded worker result; never the whole parent task."`
	MarginalContribution string   `json:"marginal_contribution" jsonschema:"Independent output this worker adds and which supervisor work it avoids duplicating."`
	Context              string   `json:"context,omitempty" jsonschema:"Minimum facts, paths, constraints, and decisions needed; do not dump the parent transcript."`
	OutputContract       string   `json:"output_contract" jsonschema:"Required report fields, evidence, residual risk, and maximum useful detail."`
	DoneCondition        string   `json:"done_condition" jsonschema:"Observable acceptance condition, preferably an exact test or command."`
	DeadlineMS           int64    `json:"deadline_ms,omitempty" jsonschema:"Hard worker deadline in milliseconds, 30000 to 600000; defaults to 600000."`
	RetryReason          string   `json:"retry_reason,omitempty" jsonschema:"Required only for the one allowed same-lane retry after runtime reports retry_eligible=true."`
	WorkDir              string   `json:"workdir,omitempty" jsonschema:"Working directory inside the Claude X launch directory."`
	Write                bool     `json:"write,omitempty" jsonschema:"Whether this worker may edit files and run shell commands."`
	Paths                []string `json:"paths,omitempty" jsonschema:"Exclusive write scopes. Required when write is true."`
}

// SuggestedSlice is a zero-model template for splitting a composite packet (T4).
type SuggestedSlice struct {
	SliceID       string   `json:"slice_id"`
	Paths         []string `json:"paths,omitempty"`
	DoneCondition string   `json:"done_condition,omitempty"`
	Note          string   `json:"note,omitempty"`
}

type WorkerAdmission struct {
	Route                string           `json:"route"`
	RouteID              string           `json:"route_id,omitempty"`
	SliceID              string           `json:"slice_id"`
	MarginalContribution string           `json:"marginal_contribution,omitempty"`
	DeadlineMS           int64            `json:"deadline_ms"`
	Result               string           `json:"result"`
	RejectionReasons     []string         `json:"rejection_reasons"`
	SuggestedSlices      []SuggestedSlice `json:"suggested_slices,omitempty"`
}

type WorkerResumeInput struct {
	WorkerID       string `json:"worker_id" jsonschema:"Existing worker ID returned by start_worker."`
	EvidencePacket string `json:"evidence_packet" jsonschema:"New source-preserving evidence or verifier failure output only."`
	Instruction    string `json:"instruction,omitempty" jsonschema:"Narrow continuation or repair instruction."`
}

type WorkerCloseInput struct {
	WorkerID string `json:"worker_id"`
	Reason   string `json:"reason,omitempty"`
}

type ExternalResearchInput struct {
	RouteID          string `json:"route_id,omitempty" jsonschema:"route_id returned by route_task when this capability executes that selected route."`
	WorkerID         string `json:"worker_id,omitempty" jsonschema:"Originating worker ID when applicable."`
	Question         string `json:"question" jsonschema:"Exact external or current-information question."`
	Context          string `json:"context,omitempty" jsonschema:"Minimum context needed to disambiguate the question."`
	SourcePreference string `json:"source_preference,omitempty" jsonschema:"Preferred official, primary, vendor, or X sources."`
}

type URLDigestInput struct {
	RouteID  string   `json:"route_id,omitempty" jsonschema:"route_id returned by route_task when this capability executes that selected route."`
	WorkerID string   `json:"worker_id,omitempty" jsonschema:"Originating worker ID when applicable."`
	Question string   `json:"question" jsonschema:"Fields or comparisons to extract."`
	URLs     []string `json:"urls" jsonschema:"Explicit HTTP or HTTPS URLs; no URL discovery."`
}

type RepoExploreInput struct {
	RouteID  string   `json:"route_id,omitempty" jsonschema:"route_id returned by route_task when this capability executes that selected route."`
	WorkerID string   `json:"worker_id,omitempty" jsonschema:"Originating worker ID when applicable."`
	Question string   `json:"question" jsonschema:"Exact code-location or dependency question."`
	Scope    []string `json:"scope,omitempty" jsonschema:"Directories or files to prioritize."`
	WorkDir  string   `json:"workdir,omitempty" jsonschema:"Working directory inside the launch root."`
}

type NativeClaudeInput struct {
	RouteID   string `json:"route_id,omitempty" jsonschema:"route_id returned by route_task when this capability executes that selected route."`
	Objective string `json:"objective" jsonschema:"One bounded read-only plan, review, or judgment."`
	Context   string `json:"context,omitempty" jsonschema:"Minimum facts and paths needed."`
	WorkDir   string `json:"workdir,omitempty" jsonschema:"Working directory inside the launch root."`
	Model     string `json:"model,omitempty" jsonschema:"Native alias: opus, sonnet, sonnet[1m], or fable."`
}

type ThreadReadInput struct {
	RouteID        string `json:"route_id,omitempty" jsonschema:"route_id returned by route_task when this capability executes that selected route."`
	ThreadID       string `json:"thread_id" jsonschema:"Local Claude X session ID or Claude X thread URL."`
	Question       string `json:"question" jsonschema:"Exact facts, decisions, requirements, commands, chronology, or verification to extract."`
	MaxSourceBytes int    `json:"max_source_bytes,omitempty" jsonschema:"Bounded sanitized source packet, 16384 to 163840 bytes; defaults to 98304."`
}

type ThreadFindInput struct {
	RouteID         string `json:"route_id,omitempty" jsonschema:"route_id returned by route_task when this capability executes that selected route."`
	Query           string `json:"query,omitempty" jsonschema:"Keywords or Amp-style filters such as a quoted phrase, file:path, project:name, repo:name, after:YYYY-MM-DD, before:YYYY-MM-DD, or after:7d."`
	File            string `json:"file,omitempty" jsonschema:"Exact or suffix file path to find across sanitized root transcript events."`
	Project         string `json:"project,omitempty" jsonschema:"Project, repository, or cwd fragment."`
	After           string `json:"after,omitempty" jsonschema:"Updated after RFC3339, YYYY-MM-DD, or a relative Nd window."`
	Before          string `json:"before,omitempty" jsonschema:"Updated before RFC3339, YYYY-MM-DD, or a relative Nd window."`
	ExcludeThreadID string `json:"exclude_thread_id,omitempty" jsonschema:"Optional current thread ID to omit from results."`
	Limit           int    `json:"limit,omitempty" jsonschema:"Maximum candidates, 1 to 25; defaults to 8."`
}

type ThreadFindOutput struct {
	RouteID    string            `json:"route_id,omitempty"`
	Result     threadfind.Result `json:"result"`
	NextAction string            `json:"next_action"`
}

type TokenUsage struct {
	InputTokens         int64 `json:"input_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
}

type ExecutionIdentity struct {
	RequestedModel     string `json:"requested_model"`
	ResolvedModel      string `json:"resolved_model,omitempty"`
	ModelVerification  string `json:"model_verification"`
	RequestedEffort    string `json:"requested_effort,omitempty"`
	ResolvedEffort     string `json:"resolved_effort,omitempty"`
	EffortVerification string `json:"effort_verification"`
	AuthSource         string `json:"auth_source"`
}

type WorkerOutput struct {
	SliceID              string            `json:"slice_id"`
	Admission            WorkerAdmission   `json:"admission"`
	StartAttempts        int               `json:"start_attempts"`
	WorkerID             string            `json:"worker_id"`
	Identity             ExecutionIdentity `json:"identity"`
	SessionID            string            `json:"session_id,omitempty"`
	Turn                   int               `json:"turn"`
	CumulativeModelTurns   int               `json:"cumulative_model_turns,omitempty"`
	TurnAccountingQuality  string            `json:"turn_accounting_quality,omitempty"`
	State                  string            `json:"state"`
	Report               WorkerReport      `json:"report"`
	ToolUses             map[string]int    `json:"tool_uses,omitempty"`
	Usage                TokenUsage        `json:"usage"`
	DurationMS           int64             `json:"duration_ms"`
	FailureClass         string            `json:"failure_class,omitempty"`
	RetryEligible        bool              `json:"retry_eligible"`
	Error                string            `json:"error,omitempty"`
}

type SpecialistOutput struct {
	RouteID        string            `json:"route_id,omitempty"`
	Role           string            `json:"role"`
	Identity       ExecutionIdentity `json:"identity"`
	SessionID      string            `json:"session_id,omitempty"`
	TargetWorkerID string            `json:"target_worker_id,omitempty"`
	Report         EvidenceReport    `json:"report"`
	ToolUses       map[string]int    `json:"tool_uses,omitempty"`
	Usage          TokenUsage        `json:"usage"`
	DurationMS     int64             `json:"duration_ms"`
	ThreadSource   *ThreadReadSource `json:"thread_source,omitempty"`
}

type ThreadReadSource struct {
	ThreadID       string `json:"thread_id"`
	EventCount     int    `json:"event_count"`
	SelectedEvents int    `json:"selected_events"`
	SourceBytes    int    `json:"source_bytes"`
	LatestCompact  bool   `json:"latest_compact_included"`
}

type WorkerStatus struct {
	SliceID       string            `json:"slice_id"`
	Admission     WorkerAdmission   `json:"admission"`
	StartAttempts int               `json:"start_attempts"`
	WorkerID      string            `json:"worker_id"`
	Identity      ExecutionIdentity `json:"identity"`
	SessionID     string            `json:"session_id,omitempty"`
	State         string            `json:"state"`
	Turn          int               `json:"turn"`
	Write         bool              `json:"write"`
	Paths         []string          `json:"paths,omitempty"`
	Report        WorkerReport      `json:"last_report"`
	FailureClass  string            `json:"failure_class,omitempty"`
	RetryEligible bool              `json:"retry_eligible"`
	Error         string            `json:"error,omitempty"`
	DurationMS    int64             `json:"duration_ms,omitempty"`
	ToolUses      map[string]int    `json:"tool_uses,omitempty"`
	Usage         TokenUsage        `json:"usage"`
}

type SliceStatus struct {
	Admission     WorkerAdmission    `json:"admission"`
	StartAttempts int                `json:"start_attempts"`
	WorkerID      string             `json:"worker_id,omitempty"`
	Identity      *ExecutionIdentity `json:"identity,omitempty"`
	SessionID     string             `json:"session_id,omitempty"`
	State         string             `json:"state"`
	FailureClass  string             `json:"failure_class,omitempty"`
	RetryEligible bool               `json:"retry_eligible"`
	Error         string             `json:"error,omitempty"`
	DurationMS    int64              `json:"duration_ms,omitempty"`
	ToolUses      map[string]int     `json:"tool_uses,omitempty"`
	Usage         *TokenUsage        `json:"usage,omitempty"`
}

type WorkflowStatusOutput struct {
	Contract        RuntimeContract     `json:"contract"`
	Supervisor      string              `json:"supervisor"`
	WorkerProfile   string              `json:"worker_profile"`
	ActiveRuns      int                 `json:"active_runs"`
	WorkerStarts    int32               `json:"worker_starts"`
	WorkerTurns     int32               `json:"worker_turns"`
	ResearchCalls   int32               `json:"research_calls"`
	NativeCalls     int32               `json:"native_claude_calls"`
	ThreadFindCalls int32               `json:"thread_find_calls"`
	ThreadReadCalls int32               `json:"thread_read_calls"`
	Limits          map[string]int      `json:"limits"`
	Workers         []WorkerStatus      `json:"workers"`
	Slices          []SliceStatus       `json:"slices"`
	LaneHealth      []router.LaneHealth `json:"lane_health,omitempty"`
	Routes          []RouteRecord       `json:"routes,omitempty"`
	RouteLedgerPath string              `json:"route_ledger_path,omitempty"`
	Timestamp       time.Time           `json:"timestamp"`
}

type RouteOutcomeInput struct {
	RouteID         string `json:"route_id" jsonschema:"route_id returned by route_task."`
	Status          string `json:"status" jsonschema:"accepted, failed, or abandoned."`
	Verification    string `json:"verification,omitempty" jsonschema:"Exact verifier result or bounded acceptance evidence. Required for accepted."`
	HumanCorrection string `json:"human_correction,omitempty" jsonschema:"unknown, none, minor, or major."`
	ResidualRisk    string `json:"residual_risk,omitempty" jsonschema:"Material unencoded risk remaining after verification."`
}

type RouteOutcome struct {
	Status          string `json:"status"`
	Verification    string `json:"verification,omitempty"`
	HumanCorrection string `json:"human_correction"`
	ResidualRisk    string `json:"residual_risk,omitempty"`
	RecordedAt      string `json:"recorded_at"`
}

// RouteDiagnostics counts each child model invocation once. Token classes stay
// separate so cached input is never silently added to ordinary input twice.
// The Claude Code Supervisor is outside this MCP process and is therefore
// explicitly excluded instead of estimated.
type RouteDiagnostics struct {
	Coverage           string         `json:"coverage"`
	AccountingUnit     string         `json:"accounting_unit"`
	SupervisorIncluded bool           `json:"supervisor_included"`
	ComparableSpend    bool           `json:"comparable_spend"`
	Calls              int            `json:"child_model_calls"`
	FailedCalls        int            `json:"failed_child_calls"`
	WorkerStarts       int            `json:"worker_starts"`
	WorkerResumes      int            `json:"worker_resumes"`
	SpecialistCalls    int            `json:"specialist_calls"`
	Retries            int            `json:"same_lane_retries"`
	Rescues            int            `json:"cross_model_rescues"`
	RequestedModels    map[string]int `json:"requested_models,omitempty"`
	ResolvedModels     map[string]int `json:"resolved_models,omitempty"`
	ToolUses           map[string]int `json:"tool_uses,omitempty"`
	Usage              TokenUsage     `json:"usage"`
	DurationMS         int64          `json:"duration_ms"`
}

type RouteRecord struct {
	RouteID         string           `json:"route_id"`
	State           string           `json:"state"`
	Plan            router.Plan      `json:"plan"`
	CreatedAt       string           `json:"created_at"`
	// RootSessionID is best-effort bind for resume / multi-process recovery.
	RootSessionID   string           `json:"root_session_id,omitempty"`
	ParentSessionID string           `json:"parent_session_id,omitempty"`
	// WorkDir absolute cwd when the route was planned (boundary for gate/verify).
	WorkDir         string           `json:"workdir,omitempty"`
	Diagnostics     RouteDiagnostics `json:"diagnostics"`
	Outcome         *RouteOutcome    `json:"outcome,omitempty"`
	LedgerStatus    string           `json:"ledger_status"`
	LedgerError     string           `json:"ledger_error,omitempty"`
}

type EmptyInput struct{}

const workerJSONSchema = `{
  "type":"object",
  "properties":{
    "status":{"type":"string","enum":["completed","needs_capability","blocked"]},
    "summary":{"type":"string"},
    "evidence":{"type":"array","items":{"type":"string"}},
    "changed_paths":{"type":"array","items":{"type":"string"}},
    "verification":{"type":"array","items":{"type":"string"}},
    "needs":{"type":"array","items":{"type":"object","properties":{
      "kind":{"type":"string","enum":["external_search","url_digest","repo_explore","find_thread","read_thread"]},
      "question":{"type":"string"},
      "why":{"type":"string"},
      "urls":{"type":"array","items":{"type":"string"}}
    },"required":["kind","question","why","urls"],"additionalProperties":false}}
  },
  "required":["status","summary","evidence","changed_paths","verification","needs"],
  "additionalProperties":false
}`

const evidenceJSONSchema = `{
  "type":"object",
  "properties":{
    "status":{"type":"string","enum":["completed","blocked"]},
    "summary":{"type":"string"},
    "items":{"type":"array","items":{"type":"object","properties":{
      "claim":{"type":"string"},
      "source":{"type":"string"},
      "detail":{"type":"string"}
    },"required":["claim","source","detail"],"additionalProperties":false}},
    "open_questions":{"type":"array","items":{"type":"string"}}
  },
  "required":["status","summary","items","open_questions"],
  "additionalProperties":false
}`
