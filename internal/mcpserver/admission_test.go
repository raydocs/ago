package mcpserver

import (
	"strings"
	"testing"
)

func TestAdmissionRejectsCompositeMultiDomainSlice(t *testing.T) {
	in := WorkerStartInput{
		SliceID:              "usage-full-stack",
		Objective:            "Implement usage foundation, D1 migration, thread API, and Amp-like UI polish with Playwright checks.",
		MarginalContribution: "Own the entire usage+UI stack.",
		Context:              "Root thread needs usage page and mobile layout.",
		OutputContract:       "migration, API, styles.css, app.js, screenshots",
		DoneCondition:        "wrangler deploy and playwright viewport pass",
		Paths: []string{
			"thread-app/migrations/0002_usage.sql",
			"thread-app/src/index.ts",
			"thread-app/public/styles.css",
			"thread-app/public/app.js",
		},
		Write:      true,
		DeadlineMS: 120_000,
	}
	admission := evaluateWorkerAdmission(in)
	if admission.Result != admissionRejected {
		t.Fatalf("expected composite rejection, got %#v", admission)
	}
	joined := strings.Join(admission.RejectionReasons, "; ")
	if !strings.Contains(joined, "composite_slice") {
		t.Fatalf("expected composite_slice reason, got %q", joined)
	}
	if len(admission.SuggestedSlices) < 2 {
		t.Fatalf("expected suggested_slices templates, got %#v", admission.SuggestedSlices)
	}
}

func TestAdmissionAcceptsSingleDomainBoundedSlice(t *testing.T) {
	in := WorkerStartInput{
		SliceID:              "usage-parse-only",
		Objective:            "Implement isolated transcript usage parser for one JSONL file.",
		MarginalContribution: "Own parser so supervisor only runs go test.",
		Context:              "internal/threadusage is empty.",
		OutputContract:       "changed paths and go test output",
		DoneCondition:        "go test ./internal/threadusage passes",
		Paths:                []string{"internal/threadusage/parse.go"},
		Write:                true,
		DeadlineMS:           120_000,
	}
	admission := evaluateWorkerAdmission(in)
	if admission.Result != admissionAdmitted {
		t.Fatalf("expected admission, got %#v", admission)
	}
}

func TestAdmissionAcceptsUsagePageUIOnly(t *testing.T) {
	// Legitimate: only polish existing Usage page UI; API/schema frozen out of scope.
	in := WorkerStartInput{
		SliceID: "usage-ui-polish",
		Objective: "Only modify the existing Usage page UI. API, Schema, data computation, " +
			"and deploy are frozen and not in scope.",
		MarginalContribution: "Own CSS/layout so supervisor verifies screenshots.",
		Context:              "Usage foundation already shipped; do not touch migrations or thread-app/src.",
		OutputContract:       "changed public assets + screenshot evidence",
		DoneCondition:        "frontend-contract tests pass for usage panel",
		Paths: []string{
			"thread-app/public/app.js",
			"thread-app/public/styles.css",
			"thread-app/public/index.html",
		},
		Write:      true,
		DeadlineMS: 120_000,
	}
	admission := evaluateWorkerAdmission(in)
	if admission.Result != admissionAdmitted {
		t.Fatalf("Usage UI-only slice must admit, got %#v", admission)
	}
	domains := detectSliceDomains(in)
	if len(domains) != 1 || domains[0] != domainUI {
		t.Fatalf("expected only ui_frontend from paths, got %v", domains)
	}
}

func TestDetectSliceDomainsFromPaths(t *testing.T) {
	domains := detectSliceDomains(WorkerStartInput{
		Objective: "wire storage",
		Paths:     []string{"thread-app/migrations/0002_usage.sql", "thread-app/public/app.js"},
	})
	if len(domains) < 2 {
		t.Fatalf("expected multi-domain from paths, got %v", domains)
	}
}

func TestExclusionProseDoesNotAddAPIDomain(t *testing.T) {
	// No write paths: text-only. Exclusion clause should not create api domain.
	domains := detectSliceDomains(WorkerStartInput{
		Objective:     "Polish mobile layout only. API handlers are out of scope.",
		DoneCondition: "screenshot at 390px has no horizontal overflow",
	})
	for _, d := range domains {
		if d == domainAPI {
			t.Fatalf("exclusion prose must not add api domain: %v", domains)
		}
	}
}

func TestPartialUnknownPathRejectsOrMultiDomain(t *testing.T) {
	in := WorkerStartInput{
		SliceID:              "partial-paths",
		Objective:            "Update UI and backend API handler.",
		MarginalContribution: "Own mixed surface",
		OutputContract:       "paths + test",
		DoneCondition:        "go test ./...",
		Paths:                []string{"thread-app/public/app.js", "cmd/backend.go"},
		Write:                true,
		DeadlineMS:           60_000,
	}
	admission := evaluateWorkerAdmission(in)
	if admission.Result != admissionRejected {
		t.Fatalf("UI path + unknown backend path must not admit as single-domain UI: %#v domains=%v", admission, detectSliceDomains(in))
	}
}

func TestUnknownOnlySinglePackageAdmits(t *testing.T) {
	// Codex follow-up: internal/catalog/catalog.go must not be fabricated as API+UI.
	in := WorkerStartInput{
		SliceID:              "catalog-fix",
		Objective:            "Fix a localized catalog lookup bug.",
		MarginalContribution: "Own catalog package so supervisor only verifies go test.",
		OutputContract:       "changed paths + go test output",
		DoneCondition:        "go test ./internal/catalog",
		Paths:                []string{"internal/catalog/catalog.go"},
		Write:                true,
		DeadlineMS:           60_000,
	}
	admission := evaluateWorkerAdmission(in)
	if admission.Result != admissionAdmitted {
		t.Fatalf("unknown-only single package must admit, got %#v domains=%v", admission, detectSliceDomains(in))
	}
	domains := detectSliceDomains(in)
	if len(domains) != 1 || domains[0] != domainUnknown {
		t.Fatalf("expected only unknown domain, got %v", domains)
	}
}

func TestUnknownMultiPackageRejects(t *testing.T) {
	in := WorkerStartInput{
		SliceID:              "multi-pkg",
		Objective:            "Touch two unrelated packages.",
		MarginalContribution: "bad slice",
		OutputContract:       "paths",
		DoneCondition:        "go test ./...",
		Paths:                []string{"internal/catalog/catalog.go", "cmd/claudex-flow/main.go"},
		Write:                true,
		DeadlineMS:           60_000,
	}
	admission := evaluateWorkerAdmission(in)
	if admission.Result != admissionRejected {
		t.Fatalf("unknown multi-package must reject, got %#v", admission)
	}
}
