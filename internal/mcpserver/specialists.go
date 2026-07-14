package mcpserver

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"claudexflow/internal/claude"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type specialistProfile struct {
	role    string
	tool    string
	model   string
	effort  string
	tools   []string
	turns   int
	timeout time.Duration
}

var (
	grokExternal = specialistProfile{role: "external_search", tool: "search_external", model: "grok-4.5", effort: "high", tools: []string{"WebSearch", "WebFetch"}, turns: 7, timeout: 5 * time.Minute}
	geminiURLs   = specialistProfile{role: "url_digest", tool: "digest_urls", model: "gemini-3.5-flash", effort: "medium", tools: []string{"WebFetch"}, turns: 6, timeout: 4 * time.Minute}
	terraRepo    = specialistProfile{role: "repo_explore", tool: "explore_repository", model: "gpt-5.6-terra", effort: "high", tools: []string{"Read", "Grep", "Glob"}, turns: 8, timeout: 6 * time.Minute}
)

func (s *Server) searchExternal(ctx context.Context, _ *mcp.CallToolRequest, in ExternalResearchInput) (*mcp.CallToolResult, SpecialistOutput, error) {
	if strings.TrimSpace(in.Question) == "" {
		return nil, SpecialistOutput{}, fmt.Errorf("question is required")
	}
	if len(in.Question) > 8000 || len(in.Context) > 16000 {
		return nil, SpecialistOutput{}, fmt.Errorf("research brief too large")
	}
	if err := s.validateTargetWorker(in.WorkerID); err != nil {
		return nil, SpecialistOutput{}, err
	}
	if err := s.validateRouteTool(in.RouteID, "search_external"); err != nil {
		return nil, SpecialistOutput{}, err
	}
	prompt := fmt.Sprintf(`You are the Grok 4.5 external-research capability in an Amp-style coding workflow.

Exact question:
%s

Minimum context:
%s

Source preference:
%s

Use WebSearch/WebFetch only as needed. Prioritize primary/official/current sources, including X/Twitter when material. Return claims with direct source URLs and concise evidence. Separate sourced facts from inference. Do not solve the parent coding task, edit files, or invoke other models. Always finish with StructuredOutput.`, in.Question, valueOr(in.Context, "None."), valueOr(in.SourcePreference, "Primary and official sources first."))
	return s.runSpecialist(ctx, grokExternal, in.RouteID, in.WorkerID, s.root, prompt)
}

func (s *Server) digestURLs(ctx context.Context, _ *mcp.CallToolRequest, in URLDigestInput) (*mcp.CallToolResult, SpecialistOutput, error) {
	if strings.TrimSpace(in.Question) == "" || len(in.URLs) == 0 {
		return nil, SpecialistOutput{}, fmt.Errorf("question and at least one URL are required")
	}
	if len(in.URLs) > 8 {
		return nil, SpecialistOutput{}, fmt.Errorf("URL digest is capped at 8 URLs")
	}
	for _, raw := range in.URLs {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, SpecialistOutput{}, fmt.Errorf("invalid HTTP(S) URL %q", raw)
		}
	}
	if err := s.validateTargetWorker(in.WorkerID); err != nil {
		return nil, SpecialistOutput{}, err
	}
	if err := s.validateRouteTool(in.RouteID, "digest_urls"); err != nil {
		return nil, SpecialistOutput{}, err
	}
	prompt := fmt.Sprintf(`You are the Gemini 3.5 Flash explicit-URL digestion capability in an Amp-style coding workflow.

Extraction/comparison question:
%s

URLs:
- %s

Use WebFetch, not WebSearch. Extract only the requested fields. Preserve each source URL in every evidence item. If a URL cannot be fetched, report it as an open question instead of searching for a replacement or guessing. Do not solve the parent coding task or invoke another model. Always finish with StructuredOutput.`, in.Question, strings.Join(in.URLs, "\n- "))
	return s.runSpecialist(ctx, geminiURLs, in.RouteID, in.WorkerID, s.root, prompt)
}

func (s *Server) exploreRepository(ctx context.Context, _ *mcp.CallToolRequest, in RepoExploreInput) (*mcp.CallToolResult, SpecialistOutput, error) {
	if strings.TrimSpace(in.Question) == "" {
		return nil, SpecialistOutput{}, fmt.Errorf("question is required")
	}
	if err := s.validateTargetWorker(in.WorkerID); err != nil {
		return nil, SpecialistOutput{}, err
	}
	if err := s.validateRouteTool(in.RouteID, "explore_repository"); err != nil {
		return nil, SpecialistOutput{}, err
	}
	dir, err := s.scopedDir(in.WorkDir)
	if err != nil {
		return nil, SpecialistOutput{}, err
	}
	prompt := fmt.Sprintf(`You are the GPT-5.6 Terra repository-exploration capability in an Amp-style coding workflow.

Exact repository question:
%s

Priority scope:
%s

Use Read/Grep/Glob to locate relevant files, symbols, dependencies, and the smallest likely implementation surface. Return path-and-symbol evidence, not a speculative implementation. Do not edit files, solve the parent task, or invoke another model. Always finish with StructuredOutput.`, in.Question, valueOr(strings.Join(in.Scope, ", "), "Repository root, but keep exploration narrow."))
	return s.runSpecialist(ctx, terraRepo, in.RouteID, in.WorkerID, dir, prompt)
}

func (s *Server) runSpecialist(ctx context.Context, p specialistProfile, routeID, workerID, workdir, prompt string) (*mcp.CallToolResult, SpecialistOutput, error) {
	if s.researchCalls.Add(1) > maxResearchCalls {
		return nil, SpecialistOutput{}, fmt.Errorf("research capability budget exhausted: hard cap is %d", maxResearchCalls)
	}
	if err := s.acquire(ctx); err != nil {
		return nil, SpecialistOutput{}, err
	}
	defer s.release()
	rootSessionID, parentSessionID := s.threadBinding()
	result := s.invokeModel(ctx, claude.Request{
		SettingsPath:    s.settings,
		AuthMode:        claude.AuthGateway,
		WorkDir:         workdir,
		Prompt:          prompt,
		Model:           p.model,
		Effort:          p.effort,
		Role:            p.role,
		RootSessionID:   rootSessionID,
		ParentSessionID: parentSessionID,
		Tools:           p.tools,
		JSONSchema:      evidenceJSONSchema,
		MaxTurns:        p.turns,
		Timeout:         p.timeout,
	})
	s.recordRouteModelCall(routeID, p.role, p.model, p.effort, result, false)
	if !result.Success {
		s.recordLaneFailure(p.tool, classifyRunFailure(result, false))
		return nil, SpecialistOutput{}, fmt.Errorf("%s specialist failed: %s", p.role, runFailure(result))
	}
	identity := executionIdentity(p.model, p.effort, result)
	if identity.ModelVerification == "mismatch" {
		s.recordLaneFailure(p.tool, failureInfo{Class: failureModelMismatch, Detail: fmt.Sprintf("requested %q resolved %q", p.model, result.ResolvedModel)})
		return nil, SpecialistOutput{}, fmt.Errorf("%s specialist model mismatch: requested %q resolved %q", p.role, p.model, result.ResolvedModel)
	}
	report, err := decodeEvidenceReport(result)
	if err != nil {
		s.recordLaneFailure(p.tool, failureInfo{Class: failureInvalidOutput, Detail: err.Error()})
		return nil, SpecialistOutput{}, fmt.Errorf("%s returned invalid evidence packet: %w", p.role, err)
	}
	s.recordLaneHealthy(p.tool)
	return nil, SpecialistOutput{RouteID: routeID, Role: p.role, Identity: identity, SessionID: result.SessionID, TargetWorkerID: workerID, Report: report, ToolUses: result.ToolUses, Usage: tokenUsage(result.Usage), DurationMS: result.DurationMS}, nil
}
