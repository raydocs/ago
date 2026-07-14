package mcpserver

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const ContractVersion = "claudex-workflow.v1.4.5"

type RuntimeContract struct {
	Version           string   `json:"version"`
	SupervisorProfile string   `json:"supervisor_profile"`
	WorkerProfile     string   `json:"worker_profile"`
	Tools             []string `json:"tools"`
	WorkerStartFields []string `json:"worker_start_fields"`
}

func Contract() RuntimeContract {
	return RuntimeContract{
		Version:           ContractVersion,
		SupervisorProfile: "gpt-5.6-sol/xhigh",
		WorkerProfile:     workerModel + "/" + workerEffort,
		Tools: []string{
			"route_task",
			"record_route_outcome",
			"start_worker",
			"resume_worker",
			"search_external",
			"digest_urls",
			"explore_repository",
			"find_thread",
			"read_thread",
			"consult_native_claude",
			"close_worker",
			"workflow_status",
			"declare_gate",
			"close_gate",
			"ack_reroute",
			"gate_status",
			"runtime_contract",
		},
		WorkerStartFields: workerStartFields(),
	}
}

func ValidateOrchestrator(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	markers := []string{
		"Contract: `" + ContractVersion + "`",
		workerFieldMarker(),
	}
	for _, marker := range markers {
		if !strings.Contains(string(raw), marker) {
			return fmt.Errorf("orchestrator contract mismatch: expected %s", marker)
		}
	}
	return nil
}

func workerFieldMarker() string {
	return "Runtime start_worker fields: `" + strings.Join(workerStartFields(), ", ") + "`"
}

func workerStartFields() []string {
	typeOf := reflect.TypeOf(WorkerStartInput{})
	fields := make([]string, 0, typeOf.NumField())
	for i := 0; i < typeOf.NumField(); i++ {
		name := strings.Split(typeOf.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			fields = append(fields, name)
		}
	}
	sort.Strings(fields)
	return fields
}

func (s *Server) runtimeContract(_ context.Context, _ *mcp.CallToolRequest, _ EmptyInput) (*mcp.CallToolResult, RuntimeContract, error) {
	return nil, Contract(), nil
}
