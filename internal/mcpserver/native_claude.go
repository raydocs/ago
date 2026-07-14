package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"claudexflow/internal/claude"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) consultNativeClaude(ctx context.Context, _ *mcp.CallToolRequest, in NativeClaudeInput) (*mcp.CallToolResult, SpecialistOutput, error) {
	if strings.TrimSpace(in.Objective) == "" {
		return nil, SpecialistOutput{}, fmt.Errorf("objective is required")
	}
	if len(in.Objective) > 8000 || len(in.Context) > 24000 {
		return nil, SpecialistOutput{}, fmt.Errorf("native Claude brief too large")
	}
	model, err := nativeClaudeModel(in.Model)
	if err != nil {
		return nil, SpecialistOutput{}, err
	}
	dir, err := s.scopedDir(in.WorkDir)
	if err != nil {
		return nil, SpecialistOutput{}, err
	}
	if err := s.validateRouteTool(in.RouteID, "consult_native_claude"); err != nil {
		return nil, SpecialistOutput{}, err
	}
	if s.nativeCalls.Add(1) > maxNativeCalls {
		return nil, SpecialistOutput{}, fmt.Errorf("native Claude consultation budget exhausted: hard cap is %d", maxNativeCalls)
	}
	if err := s.acquire(ctx); err != nil {
		return nil, SpecialistOutput{}, err
	}
	defer s.release()

	prompt := fmt.Sprintf(`You are a native Claude read-only consultant inside a cost-bounded multi-model coding workflow. You are authenticated directly with the user's local claude.ai subscription, not an API gateway.

Bounded objective:
%s

Minimum context:
%s

Use Read/Grep/Glob only when local evidence is needed. Do not edit files, browse the web, invoke another model, or broaden the parent task. Return a concise evidence packet. Separate facts from assumptions and always finish with StructuredOutput.`, in.Objective, valueOr(in.Context, "No additional context supplied."))

	rootSessionID, parentSessionID := s.threadBinding()
	result := s.invokeModel(ctx, claude.Request{
		AuthMode:        claude.AuthNativeSubscription,
		WorkDir:         dir,
		Prompt:          prompt,
		Model:           model,
		Effort:          "high",
		Role:            "native_claude",
		RootSessionID:   rootSessionID,
		ParentSessionID: parentSessionID,
		Tools:           []string{"Read", "Grep", "Glob"},
		JSONSchema:      evidenceJSONSchema,
		MaxTurns:        8,
		Timeout:         8 * time.Minute,
	})
	s.recordRouteModelCall(in.RouteID, "native_claude", model, "high", result, false)
	if !result.Success {
		s.recordLaneFailure("consult_native_claude", classifyRunFailure(result, false))
		return nil, SpecialistOutput{}, fmt.Errorf("native Claude consultation failed: %s", runFailure(result))
	}
	identity := executionIdentity(model, "high", result)
	if identity.ModelVerification == "mismatch" {
		s.recordLaneFailure("consult_native_claude", failureInfo{Class: failureModelMismatch, Detail: fmt.Sprintf("requested %q resolved %q", model, result.ResolvedModel)})
		return nil, SpecialistOutput{}, fmt.Errorf("native Claude model mismatch: requested %q resolved %q", model, result.ResolvedModel)
	}
	report, err := decodeEvidenceReport(result)
	if err != nil {
		s.recordLaneFailure("consult_native_claude", failureInfo{Class: failureInvalidOutput, Detail: err.Error()})
		return nil, SpecialistOutput{}, fmt.Errorf("native Claude returned invalid evidence packet: %w", err)
	}
	s.recordLaneHealthy("consult_native_claude")
	return nil, SpecialistOutput{
		RouteID:    in.RouteID,
		Role:       "native_claude",
		Identity:   identity,
		SessionID:  result.SessionID,
		Report:     report,
		ToolUses:   result.ToolUses,
		Usage:      tokenUsage(result.Usage),
		DurationMS: result.DurationMS,
	}, nil
}

func nativeClaudeModel(value string) (string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	switch value {
	case "", "opus":
		return "opus", nil
	case "sonnet", "sonnet[1m]", "fable":
		return value, nil
	case "claude-fable-5":
		return "fable", nil
	default:
		return "", fmt.Errorf("unsupported native Claude model %q; allowed: opus, sonnet, sonnet[1m], fable", value)
	}
}
