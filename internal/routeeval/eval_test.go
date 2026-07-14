package routeeval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunPolicySuite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "suite.json")
	raw := `{
  "version":"test.v1",
  "purpose":"policy regression",
  "accounting_unit":"relative_resource_intensity",
  "non_inferiority_rule":"all frozen policy cases match",
  "adoption_threshold":"10 of 10 policy cases pass; no model-quality claim",
  "cases":[{
    "id":"direct",
    "family":"serial implementation",
    "request":{"objective":"Fix a localized parser bug."},
    "expected_action":"supervisor_direct",
    "expected_model":"gpt-5.6-sol",
    "expected_worker_admissible":false
  }]
}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Run(path)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != "PASS" || report.Passed != 1 || report.Failed != 0 {
		t.Fatalf("unexpected report: %#v", report)
	}
}
