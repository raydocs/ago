package mcpserver

import (
	"sort"
	"strings"

	"claudexflow/internal/router"
)

func (s *Server) recordLaneHealthy(tool string) {
	s.recordLaneHealth(router.LaneHealth{Tool: tool, Status: "healthy"})
}

func (s *Server) recordLaneFailure(tool string, info failureInfo) {
	status := ""
	switch info.Class {
	case failureAuthConfiguration, failureModelMismatch:
		status = "unavailable"
	case failureTransport, failureTimeout, failureInvalidOutput:
		status = "degraded"
	}
	if status == "" {
		return
	}
	s.recordLaneHealth(router.LaneHealth{
		Tool: tool, Status: status, FailureClass: info.Class, Reason: compactHealthReason(info.Detail),
	})
}

func (s *Server) recordLaneHealth(health router.LaneHealth) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.laneHealth == nil {
		s.laneHealth = map[string]router.LaneHealth{}
	}
	s.laneHealth[health.Tool] = health
	// T12: persist auth/model-mismatch style unavailability across MCP restarts.
	persistDurableLaneHealth(health)
}

func (s *Server) liveLaneHealth() []router.LaneHealth {
	s.mu.Lock()
	values := make(map[string]router.LaneHealth, len(s.laneHealth)+5)
	for tool, health := range s.laneHealth {
		values[tool] = health
	}
	s.mu.Unlock()
	// Durable quarantine only fills tools missing from this MCP session memory.
	// Explicit session healthy/canary state always wins within the process.
	for _, h := range loadDurableLaneHealth() {
		if _, ok := values[h.Tool]; !ok {
			values[h.Tool] = h
		}
	}

	if s.workerStarts.Load() >= maxWorkerThreads || s.workerTurns.Load() >= maxWorkerTurns {
		values["start_worker"] = router.LaneHealth{Tool: "start_worker", Status: "unavailable", FailureClass: "budget_exhausted", Reason: "current MCP session Worker budget is exhausted"}
	}
	if s.researchCalls.Load() >= maxResearchCalls {
		for _, tool := range []string{"search_external", "digest_urls", "explore_repository"} {
			values[tool] = router.LaneHealth{Tool: tool, Status: "unavailable", FailureClass: "budget_exhausted", Reason: "current MCP session research capability budget is exhausted"}
		}
	}
	if s.nativeCalls.Load() >= maxNativeCalls {
		values["consult_native_claude"] = router.LaneHealth{Tool: "consult_native_claude", Status: "unavailable", FailureClass: "budget_exhausted", Reason: "current MCP session native Claude budget is exhausted"}
	}
	if s.threadFindCalls.Load() >= maxThreadFindCalls {
		values["find_thread"] = router.LaneHealth{Tool: "find_thread", Status: "unavailable", FailureClass: "budget_exhausted", Reason: "current MCP session Thread Find budget is exhausted"}
	}
	if s.threadReadCalls.Load() >= maxThreadReadCalls {
		values["read_thread"] = router.LaneHealth{Tool: "read_thread", Status: "unavailable", FailureClass: "budget_exhausted", Reason: "current MCP session Thread Read budget is exhausted"}
	}

	tools := make([]string, 0, len(values))
	for tool := range values {
		tools = append(tools, tool)
	}
	sort.Strings(tools)
	out := make([]router.LaneHealth, 0, len(tools))
	for _, tool := range tools {
		out = append(out, values[tool])
	}
	return out
}

func mergeLaneHealth(caller, runtime []router.LaneHealth) []router.LaneHealth {
	values := map[string]router.LaneHealth{}
	for _, health := range caller {
		values[health.Tool] = health
	}
	// Runtime evidence wins over caller-supplied observations.
	for _, health := range runtime {
		values[health.Tool] = health
	}
	tools := make([]string, 0, len(values))
	for tool := range values {
		tools = append(tools, tool)
	}
	sort.Strings(tools)
	out := make([]router.LaneHealth, 0, len(tools))
	for _, tool := range tools {
		out = append(out, values[tool])
	}
	return out
}

func compactHealthReason(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 2000 {
		return value[:2000] + "...[truncated]"
	}
	return value
}
