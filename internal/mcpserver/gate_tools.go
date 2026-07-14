package mcpserver

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"claudexflow/internal/supervisorgate"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type DeclareGateInput struct {
	SessionID     string   `json:"session_id,omitempty" jsonschema:"Root session_id; defaults to the bound Claude X Root."`
	GateID        string   `json:"gate_id" jsonschema:"Stable gate id unique within the Root, e.g. ui-contract."`
	Acceptance    []string `json:"acceptance" jsonschema:"Observable acceptance criteria for this gate only."`
	StopCondition string   `json:"stop_condition,omitempty" jsonschema:"When this gate is done and must not continue thrashing."`
}

type CloseGateInput struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"Root session_id; defaults to the bound Claude X Root."`
	GateID    string `json:"gate_id" jsonschema:"Open gate id to close."`
	Status    string `json:"status,omitempty" jsonschema:"closed (default) or abandoned."`
}

type AckRerouteInput struct {
	SessionID           string   `json:"session_id,omitempty" jsonschema:"Root session_id; defaults to the bound Claude X Root."`
	GateID              string   `json:"gate_id" jsonschema:"Current open gate_id."`
	RemainingAcceptance []string `json:"remaining_acceptance" jsonschema:"Acceptance items still open after restating the gate."`
	WorkerDecision      string   `json:"worker_decision" jsonschema:"none, start, or resume."`
	HypothesisChange    string   `json:"hypothesis_change,omitempty" jsonschema:"What variable or hypothesis changed before more construction tools."`
}

type GateStatusInput struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"Root session_id; defaults to the bound Claude X Root."`
}

func (s *Server) resolveGateSession(explicit string) (string, error) {
	if id := strings.TrimSpace(explicit); id != "" {
		return id, nil
	}
	root, _ := s.threadBinding()
	if root == "" {
		return "", fmt.Errorf("no Root session binding; pass session_id or ensure SessionStart hooks ran")
	}
	return root, nil
}

func gateStateDir() string {
	if d := strings.TrimSpace(os.Getenv("CLAUDEX_SUPERVISOR_GATE_DIR")); d != "" {
		return d
	}
	return supervisorgate.DefaultStateDir()
}

func (s *Server) declareGate(_ context.Context, _ *mcp.CallToolRequest, in DeclareGateInput) (*mcp.CallToolResult, supervisorgate.Status, error) {
	sessionID, err := s.resolveGateSession(in.SessionID)
	if err != nil {
		return nil, supervisorgate.Status{}, err
	}
	st, err := supervisorgate.DeclareGate(gateStateDir(), supervisorgate.DeclareGateInput{
		SessionID: sessionID, GateID: in.GateID, Acceptance: in.Acceptance, StopCondition: in.StopCondition,
	}, time.Now())
	return nil, st, err
}

func (s *Server) closeGate(_ context.Context, _ *mcp.CallToolRequest, in CloseGateInput) (*mcp.CallToolResult, supervisorgate.Status, error) {
	sessionID, err := s.resolveGateSession(in.SessionID)
	if err != nil {
		return nil, supervisorgate.Status{}, err
	}
	st, err := supervisorgate.CloseGate(gateStateDir(), supervisorgate.CloseGateInput{
		SessionID: sessionID, GateID: in.GateID, Status: in.Status,
	}, time.Now())
	return nil, st, err
}

func (s *Server) ackReroute(_ context.Context, _ *mcp.CallToolRequest, in AckRerouteInput) (*mcp.CallToolResult, supervisorgate.Status, error) {
	sessionID, err := s.resolveGateSession(in.SessionID)
	if err != nil {
		return nil, supervisorgate.Status{}, err
	}
	st, err := supervisorgate.AckReroute(gateStateDir(), supervisorgate.AckRerouteInput{
		SessionID: sessionID, GateID: in.GateID, RemainingAcceptance: in.RemainingAcceptance,
		WorkerDecision: in.WorkerDecision, HypothesisChange: in.HypothesisChange,
	}, time.Now())
	return nil, st, err
}

func (s *Server) gateStatusTool(_ context.Context, _ *mcp.CallToolRequest, in GateStatusInput) (*mcp.CallToolResult, supervisorgate.Status, error) {
	sessionID, err := s.resolveGateSession(in.SessionID)
	if err != nil {
		return nil, supervisorgate.Status{}, err
	}
	st, err := supervisorgate.LoadStatus(gateStateDir(), sessionID)
	return nil, st, err
}
