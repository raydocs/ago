package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"time"

	"claudexflow/internal/claude"
	"claudexflow/internal/threadread"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const threadReaderModel = "glm-5.2"

func (s *Server) readThread(ctx context.Context, _ *mcp.CallToolRequest, in ThreadReadInput) (*mcp.CallToolResult, SpecialistOutput, error) {
	if strings.TrimSpace(in.ThreadID) == "" || strings.TrimSpace(in.Question) == "" {
		return nil, SpecialistOutput{}, fmt.Errorf("thread_id and question are required")
	}
	if len(in.ThreadID) > 512 || len(in.Question) > 8000 {
		return nil, SpecialistOutput{}, fmt.Errorf("thread read brief exceeds bounded field limits")
	}
	if err := s.validateRouteTool(in.RouteID, "read_thread"); err != nil {
		return nil, SpecialistOutput{}, err
	}
	prepared, err := threadread.Prepare(s.transcriptRoot, in.ThreadID, in.Question, in.MaxSourceBytes)
	if err != nil {
		return nil, SpecialistOutput{}, err
	}
	if s.threadReadCalls.Add(1) > maxThreadReadCalls {
		return nil, SpecialistOutput{}, fmt.Errorf("thread read budget exhausted: hard cap is %d", maxThreadReadCalls)
	}
	if err := s.acquire(ctx); err != nil {
		return nil, SpecialistOutput{}, err
	}
	defer s.release()

	prompt := fmt.Sprintf(`You are the GLM 5.2 Read Thread capability in a cost-bounded coding workflow.

Question:
%s

Sanitized source packet:
%s

Answer only the question. Do not stop at the first relevant hit: check newer events that revise, supersede, revert, or contradict it. A tool call records an attempted action, not an outcome; require the matching result or later verification before claiming success. Use a compaction summary for orientation, never as sole proof when selected original events contain exact requirements, wording, code, commands, chronology, edits, or verification. Preserve thread:// sources in every evidence item. Separate facts from inference, identify conflicts or missing evidence, and do not solve the parent task. Keep the entire final packet under 2500 characters and do not repeat the same caveat in the summary, items, and open questions. Do not invoke tools or another model. Always finish with StructuredOutput.`, in.Question, prepared.Packet)

	rootSessionID, parentSessionID := s.threadBinding()
	result := s.invokeModel(ctx, claude.Request{
		SettingsPath: s.settings, AuthMode: claude.AuthGateway, WorkDir: s.root,
		Prompt: prompt, Model: threadReaderModel, Role: "read_thread",
		RootSessionID: rootSessionID, ParentSessionID: parentSessionID,
		Tools: []string{"StructuredOutput"}, JSONSchema: evidenceJSONSchema, MaxTurns: 4, Timeout: 4 * time.Minute,
	})
	s.recordRouteModelCall(in.RouteID, "read_thread", threadReaderModel, "", result, false)
	if !result.Success {
		s.recordLaneFailure("read_thread", classifyRunFailure(result, false))
		return nil, SpecialistOutput{}, fmt.Errorf("read_thread specialist failed: %s", runFailure(result))
	}
	identity := executionIdentity(threadReaderModel, "", result)
	if identity.ModelVerification == "mismatch" {
		s.recordLaneFailure("read_thread", failureInfo{Class: failureModelMismatch, Detail: fmt.Sprintf("requested %q resolved %q", threadReaderModel, result.ResolvedModel)})
		return nil, SpecialistOutput{}, fmt.Errorf("read_thread model mismatch: requested %q resolved %q", threadReaderModel, result.ResolvedModel)
	}
	report, err := decodeEvidenceReport(result)
	if err != nil {
		s.recordLaneFailure("read_thread", failureInfo{Class: failureInvalidOutput, Detail: err.Error()})
		return nil, SpecialistOutput{}, fmt.Errorf("read_thread returned invalid evidence packet: %w", err)
	}
	s.recordLaneHealthy("read_thread")
	return nil, SpecialistOutput{
		RouteID: in.RouteID, Role: "read_thread", Identity: identity, SessionID: result.SessionID,
		Report: report, ToolUses: result.ToolUses, Usage: tokenUsage(result.Usage), DurationMS: result.DurationMS,
		ThreadSource: &ThreadReadSource{ThreadID: prepared.ThreadID, EventCount: prepared.EventCount, SelectedEvents: prepared.SelectedEvents, SourceBytes: prepared.SourceBytes, LatestCompact: prepared.LatestCompact},
	}, nil
}
