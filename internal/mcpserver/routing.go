package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"claudexflow/internal/router"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// routeTaskInputSchema keeps the zero-model router's small closed vocabularies
// visible to the client. A prose-only description lets the lead model invent a
// plausible synonym, which then costs an avoidable validation/recovery turn.
// Runtime validation in router.PlanRoute remains the defense in depth.
func routeTaskInputSchema() (*jsonschema.Schema, error) {
	schema, err := jsonschema.For[router.RouteRequest](nil)
	if err != nil {
		return nil, err
	}
	enums := map[string][]any{
		"kind": {
			"auto", "general", "quick", "hard", "implement", "complex-implement",
			"explore", "fast-research", "deep-research", "realtime", "chinese",
			"long-context", "find-thread", "read-thread",
		},
		"risk":         {"normal", "high"},
		"checkability": {"auto", "objective", "partial", "semantic"},
		"topology":     {"auto", "direct", "worker"},
	}
	for field, values := range enums {
		if property := schema.Properties[field]; property != nil {
			property.Enum = values
		}
	}
	if property := schema.Properties["independent_slices"]; property != nil {
		property.Minimum = jsonschema.Ptr(0.0)
		property.Maximum = jsonschema.Ptr(3.0)
	}
	for _, field := range []string{"estimated_worker_seconds", "estimated_parallel_savings_seconds"} {
		if property := schema.Properties[field]; property != nil {
			property.Minimum = jsonschema.Ptr(0.0)
			property.Maximum = jsonschema.Ptr(86400.0)
		}
	}
	if property := schema.Properties["explicit_urls"]; property != nil {
		property.MaxItems = jsonschema.Ptr(8)
	}
	return schema, nil
}

// RouteReceipt is the latency-sensitive public response. The complete Plan is
// retained in the route record for enforcement and diagnostics, but echoing it
// into the Supervisor context made the next start_worker turn needlessly large.
type RouteReceipt struct {
	RouteID                string              `json:"route_id"`
	Action                 router.Action       `json:"action"`
	SelectedLane           router.Lane         `json:"selected_lane"`
	Reason                 string              `json:"reason"`
	WorkerAdmissible       bool                `json:"worker_admissible"`
	WorkerRejectionReasons []string            `json:"worker_rejection_reasons,omitempty"`
	WorkerPolicy           router.WorkerPolicy `json:"worker_policy"`
	AcceptanceReady        bool                `json:"acceptance_ready"`
	BlockedCapability      string              `json:"blocked_capability,omitempty"`
	RootVerifier           *VerifierPreflight  `json:"root_verifier,omitempty"`
	NextAction             string              `json:"next_action"`
}

// routeTask performs a zero-model prospective comparison. It never launches a
// lane; the Supervisor must call the selected capability or admitted Worker.
func (s *Server) routeTask(ctx context.Context, _ *mcp.CallToolRequest, in router.RouteRequest) (*mcp.CallToolResult, router.Plan, error) {
	preflight := resolveProjectVerifier(ctx, s.root, in.VerificationTarget)
	return s.routeTaskWithVerifier(in, preflight)
}

func (s *Server) routeTaskWithVerifier(in router.RouteRequest, preflight VerifierPreflight) (*mcp.CallToolResult, router.Plan, error) {
	in.LaneHealth = mergeLaneHealth(in.LaneHealth, s.liveLaneHealth())
	in.RuntimeVerifierStatus = preflight.Status
	fingerprint := routeRequestFingerprint(in, preflight)
	plan, err := router.PlanRoute(in)
	if err == nil {
		plan = s.registerRoute(plan, fingerprint)
	}
	return nil, plan, err
}

// routeTaskTool preserves routeTask's full internal/test API while returning a
// compact receipt on the MCP surface.
func (s *Server) routeTaskTool(ctx context.Context, req *mcp.CallToolRequest, in router.RouteRequest) (*mcp.CallToolResult, RouteReceipt, error) {
	preflight := resolveProjectVerifier(ctx, s.root, in.VerificationTarget)
	_, plan, err := s.routeTaskWithVerifier(in, preflight)
	if err != nil {
		return nil, RouteReceipt{}, err
	}
	next := "Continue with the Supervisor; no child lane was selected."
	var rootVerifier *VerifierPreflight
	if strings.TrimSpace(in.VerificationTarget) != "" {
		rootVerifier = &preflight
	}
	switch plan.Action {
	case router.ActionWorker:
		next = "Start admitted slices in parallel with route_id, slice_id, objective, paths, write=true, background=true; route estimates, deadline, output contract, and verifier defaults are inherited."
		switch preflight.Status {
		case verifierAvailable, verifierAvailableFallback:
			next += " Root must run root_verifier.command exactly once after integration."
		default:
			next += " Root must not trial the unavailable/descriptive target; resolve one repository-supported verifier from existing evidence, then execute it once."
		}
	case router.ActionDirect:
		if rootVerifier != nil && !verifierRunnable(preflight.Status) {
			next = "Stay Supervisor-direct because the project verifier is not executable. Repair or explicitly supply the verifier contract before any Worker delegation; do not install or guess dependencies."
		}
	case router.ActionCapability:
		next = "Call the selected capability once with this route_id, then return its bounded evidence to the Supervisor."
	case router.ActionBlocked:
		next = "Stop and repair the selected capability lane; do not substitute another model or tool."
	}
	return nil, RouteReceipt{
		RouteID: plan.RouteID, Action: plan.Action, SelectedLane: plan.SelectedLane, Reason: plan.Reason,
		WorkerAdmissible: plan.WorkerAdmissible, WorkerRejectionReasons: plan.WorkerRejectionReasons,
		WorkerPolicy: plan.WorkerPolicy, AcceptanceReady: plan.Acceptance.Ready,
		BlockedCapability: plan.BlockedCapability, RootVerifier: rootVerifier, NextAction: next,
	}, nil
}

func routeRequestFingerprint(in router.RouteRequest, preflight VerifierPreflight) string {
	raw, _ := json.Marshal(struct {
		Request  router.RouteRequest `json:"request"`
		Verifier VerifierPreflight   `json:"verifier"`
	}{Request: in, Verifier: preflight})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}
