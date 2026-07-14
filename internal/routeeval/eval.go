package routeeval

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"claudexflow/internal/router"
)

type Suite struct {
	Version            string `json:"version"`
	Purpose            string `json:"purpose"`
	AccountingUnit     string `json:"accounting_unit"`
	NonInferiorityRule string `json:"non_inferiority_rule"`
	AdoptionThreshold  string `json:"adoption_threshold"`
	Cases              []Case `json:"cases"`
}

type Case struct {
	ID                       string              `json:"id"`
	Family                   string              `json:"family"`
	Request                  router.RouteRequest `json:"request"`
	ExpectedAction           router.Action       `json:"expected_action"`
	ExpectedModel            string              `json:"expected_model"`
	ExpectedTool             string              `json:"expected_tool,omitempty"`
	ExpectedWorkerAdmissible bool                `json:"expected_worker_admissible"`
}

type CaseResult struct {
	ID     string      `json:"id"`
	Family string      `json:"family"`
	Passed bool        `json:"passed"`
	Errors []string    `json:"errors,omitempty"`
	Plan   router.Plan `json:"plan"`
}

type Report struct {
	SuiteVersion       string       `json:"suite_version"`
	Status             string       `json:"status"`
	Cases              int          `json:"cases"`
	Passed             int          `json:"passed"`
	Failed             int          `json:"failed"`
	AccountingUnit     string       `json:"accounting_unit"`
	NonInferiorityRule string       `json:"non_inferiority_rule"`
	AdoptionThreshold  string       `json:"adoption_threshold"`
	Scope              string       `json:"scope"`
	Results            []CaseResult `json:"results"`
}

func Run(path string) (Report, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Report{}, err
	}
	var suite Suite
	if err := json.Unmarshal(raw, &suite); err != nil {
		return Report{}, fmt.Errorf("decode route evaluation suite: %w", err)
	}
	if err := validateSuite(suite); err != nil {
		return Report{}, err
	}
	report := Report{
		SuiteVersion: suite.Version, Status: "PASS", Cases: len(suite.Cases), AccountingUnit: suite.AccountingUnit,
		NonInferiorityRule: suite.NonInferiorityRule, AdoptionThreshold: suite.AdoptionThreshold,
		Scope: "zero-model policy regression only; this does not establish model quality, accepted-result cost, or a durable default",
	}
	for _, testCase := range suite.Cases {
		result := CaseResult{ID: testCase.ID, Family: testCase.Family}
		plan, routeErr := router.PlanRoute(testCase.Request)
		if routeErr != nil {
			result.Errors = append(result.Errors, routeErr.Error())
		} else {
			result.Plan = plan
			if plan.Action != testCase.ExpectedAction {
				result.Errors = append(result.Errors, fmt.Sprintf("action=%s want=%s", plan.Action, testCase.ExpectedAction))
			}
			if plan.SelectedLane.Model != testCase.ExpectedModel {
				result.Errors = append(result.Errors, fmt.Sprintf("model=%s want=%s", plan.SelectedLane.Model, testCase.ExpectedModel))
			}
			if plan.SelectedLane.Tool != testCase.ExpectedTool {
				result.Errors = append(result.Errors, fmt.Sprintf("tool=%s want=%s", plan.SelectedLane.Tool, testCase.ExpectedTool))
			}
			if plan.WorkerAdmissible != testCase.ExpectedWorkerAdmissible {
				result.Errors = append(result.Errors, fmt.Sprintf("worker_admissible=%t want=%t", plan.WorkerAdmissible, testCase.ExpectedWorkerAdmissible))
			}
		}
		result.Passed = len(result.Errors) == 0
		if result.Passed {
			report.Passed++
		} else {
			report.Failed++
			report.Status = "FAIL"
		}
		report.Results = append(report.Results, result)
	}
	return report, nil
}

func validateSuite(suite Suite) error {
	if strings.TrimSpace(suite.Version) == "" || strings.TrimSpace(suite.Purpose) == "" {
		return fmt.Errorf("suite version and purpose are required")
	}
	if suite.AccountingUnit != "relative_resource_intensity" && suite.AccountingUnit != "api_dollars" && suite.AccountingUnit != "subscription_usage_signal" {
		return fmt.Errorf("unsupported accounting_unit %q", suite.AccountingUnit)
	}
	if strings.TrimSpace(suite.NonInferiorityRule) == "" || strings.TrimSpace(suite.AdoptionThreshold) == "" {
		return fmt.Errorf("non_inferiority_rule and adoption_threshold must be declared before evaluation")
	}
	if len(suite.Cases) == 0 || len(suite.Cases) > 64 {
		return fmt.Errorf("suite must contain 1 to 64 cases")
	}
	seen := map[string]bool{}
	for _, testCase := range suite.Cases {
		if strings.TrimSpace(testCase.ID) == "" || strings.TrimSpace(testCase.Family) == "" {
			return fmt.Errorf("every case requires id and family")
		}
		if seen[testCase.ID] {
			return fmt.Errorf("duplicate case id %q", testCase.ID)
		}
		seen[testCase.ID] = true
		if testCase.ExpectedAction == "" || strings.TrimSpace(testCase.ExpectedModel) == "" {
			return fmt.Errorf("case %s requires expected_action and expected_model", testCase.ID)
		}
	}
	return nil
}
