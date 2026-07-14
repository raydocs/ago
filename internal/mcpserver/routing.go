package mcpserver

import (
	"context"

	"claudexflow/internal/router"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// routeTask performs a zero-model prospective comparison. It never launches a
// lane; the Supervisor must call the selected capability or admitted Worker.
func (s *Server) routeTask(_ context.Context, _ *mcp.CallToolRequest, in router.RouteRequest) (*mcp.CallToolResult, router.Plan, error) {
	in.LaneHealth = mergeLaneHealth(in.LaneHealth, s.liveLaneHealth())
	plan, err := router.PlanRoute(in)
	if err == nil {
		plan = s.registerRoute(plan)
	}
	return nil, plan, err
}
