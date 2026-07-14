package mcpserver

import (
	"context"
	"fmt"

	"claudexflow/internal/threadfind"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func (s *Server) findThread(ctx context.Context, _ *mcp.CallToolRequest, in ThreadFindInput) (*mcp.CallToolResult, ThreadFindOutput, error) {
	if err := s.validateRouteTool(in.RouteID, "find_thread"); err != nil {
		return nil, ThreadFindOutput{}, err
	}
	query := threadfind.Query{
		Text: in.Query, File: in.File, Project: in.Project, After: in.After, Before: in.Before,
		ExcludeThreadID: in.ExcludeThreadID, Limit: in.Limit,
	}
	if err := threadfind.Validate(query); err != nil {
		return nil, ThreadFindOutput{}, err
	}
	if s.threadFindCalls.Add(1) > maxThreadFindCalls {
		return nil, ThreadFindOutput{}, fmt.Errorf("thread find budget exhausted: hard cap is %d", maxThreadFindCalls)
	}
	if err := s.acquire(ctx); err != nil {
		return nil, ThreadFindOutput{}, err
	}
	defer s.release()
	result, err := threadfind.Find(s.transcriptRoot, query)
	if err != nil {
		s.recordLaneFailure("find_thread", failureInfo{Class: failureInvalidOutput, Detail: err.Error()})
		return nil, ThreadFindOutput{}, err
	}
	s.recordLaneHealthy("find_thread")
	next := "No matching local Thread was found; refine one filter instead of broadening every dimension."
	if len(result.Matches) > 0 {
		next = "Choose the smallest relevant candidate and call read_thread with its thread_id plus the exact question. Do not inject the complete transcript."
	}
	return nil, ThreadFindOutput{RouteID: in.RouteID, Result: result, NextAction: next}, nil
}
