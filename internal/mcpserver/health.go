package mcpserver

import (
	"sort"
	"strings"
	"time"

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
	if strings.TrimSpace(health.ObservedAt) == "" {
		health.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
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

	// Merge durable quarantine by ObservedAt freshness (T12).
	// Durable hard unavailable must not be overwritten by an older session healthy.
	for _, h := range loadDurableLaneHealth() {
		prev, ok := values[h.Tool]
		if !ok {
			values[h.Tool] = h
			continue
		}
		values[h.Tool] = preferFresherLaneHealth(prev, h)
	}

	// Budget exhaustion is live-process truth and always wins for the current session.
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if s.workerStarts.Load() >= maxWorkerThreads || s.workerTurns.Load() >= maxWorkerTurns {
		values["start_worker"] = router.LaneHealth{Tool: "start_worker", Status: "unavailable", FailureClass: "budget_exhausted", Reason: "current MCP session Worker budget is exhausted", ObservedAt: now}
	}
	if s.researchCalls.Load() >= maxResearchCalls {
		for _, tool := range []string{"search_external", "digest_urls", "explore_repository"} {
			values[tool] = router.LaneHealth{Tool: tool, Status: "unavailable", FailureClass: "budget_exhausted", Reason: "current MCP session research capability budget is exhausted", ObservedAt: now}
		}
	}
	if s.nativeCalls.Load() >= maxNativeCalls {
		values["consult_native_claude"] = router.LaneHealth{Tool: "consult_native_claude", Status: "unavailable", FailureClass: "budget_exhausted", Reason: "current MCP session native Claude budget is exhausted", ObservedAt: now}
	}
	if s.threadFindCalls.Load() >= maxThreadFindCalls {
		values["find_thread"] = router.LaneHealth{Tool: "find_thread", Status: "unavailable", FailureClass: "budget_exhausted", Reason: "current MCP session Thread Find budget is exhausted", ObservedAt: now}
	}
	if s.threadReadCalls.Load() >= maxThreadReadCalls {
		values["read_thread"] = router.LaneHealth{Tool: "read_thread", Status: "unavailable", FailureClass: "budget_exhausted", Reason: "current MCP session Thread Read budget is exhausted", ObservedAt: now}
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

// preferFresherLaneHealth picks the observation with the later ObservedAt.
// When timestamps are equal/missing, durable hard-unavailable beats stale healthy
// only if the durable entry has a parseable ObservedAt and session does not, or
// durable is strictly newer. Equal timestamps prefer unavailable hard classes.
func preferFresherLaneHealth(session, durable router.LaneHealth) router.LaneHealth {
	st, sErr := time.Parse(time.RFC3339Nano, session.ObservedAt)
	dt, dErr := time.Parse(time.RFC3339Nano, durable.ObservedAt)
	if sErr == nil && dErr == nil {
		if dt.After(st) {
			return durable
		}
		if st.After(dt) {
			return session
		}
		// Equal time: hard durable unavailable wins over healthy (authoritative quarantine).
		if durable.Status == "unavailable" && isHardQuarantineClass(durable.FailureClass) {
			return durable
		}
		return session
	}
	if dErr == nil && sErr != nil {
		// Session missing timestamp is treated as older than dated durable.
		return durable
	}
	if sErr == nil && dErr != nil {
		return session
	}
	// Both undated: keep session memory (legacy behavior for tests without timestamps).
	return session
}

func isHardQuarantineClass(class string) bool {
	return class == failureAuthConfiguration || class == failureModelMismatch
}

func mergeLaneHealth(caller, runtime []router.LaneHealth) []router.LaneHealth {
	values := map[string]router.LaneHealth{}
	for _, health := range caller {
		values[health.Tool] = health
	}
	// Runtime evidence wins over caller-supplied observations (by freshness when both dated).
	for _, health := range runtime {
		if prev, ok := values[health.Tool]; ok {
			values[health.Tool] = preferFresherLaneHealth(prev, health)
			// Runtime is authoritative when undated on both: prefer runtime.
			if strings.TrimSpace(prev.ObservedAt) == "" && strings.TrimSpace(health.ObservedAt) == "" {
				values[health.Tool] = health
			}
		} else {
			values[health.Tool] = health
		}
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
