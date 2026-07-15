package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"claudexflow/internal/claude"
	"claudexflow/internal/router"
)

func TestRouteTaskInputSchemaPinsClosedVocabulariesAndBounds(t *testing.T) {
	schema, err := routeTaskInputSchema()
	if err != nil {
		t.Fatal(err)
	}
	wantEnums := map[string][]any{
		"risk":         {"normal", "high"},
		"checkability": {"auto", "objective", "partial", "semantic"},
		"topology":     {"auto", "direct", "worker"},
	}
	for field, want := range wantEnums {
		property := schema.Properties[field]
		if property == nil || !reflect.DeepEqual(property.Enum, want) {
			t.Fatalf("%s enum=%#v want=%#v", field, property, want)
		}
	}
	kind := schema.Properties["kind"]
	if kind == nil || len(kind.Enum) != 14 {
		t.Fatalf("kind enum is not complete: %#v", kind)
	}
	slices := schema.Properties["independent_slices"]
	if slices == nil || slices.Minimum == nil || *slices.Minimum != 0 || slices.Maximum == nil || *slices.Maximum != 3 {
		t.Fatalf("independent_slices bounds missing: %#v", slices)
	}
	for _, field := range []string{"estimated_worker_seconds", "estimated_parallel_savings_seconds"} {
		property := schema.Properties[field]
		if property == nil || property.Minimum == nil || *property.Minimum != 0 || property.Maximum == nil || *property.Maximum != 86400 {
			t.Fatalf("%s bounds missing: %#v", field, property)
		}
	}
}

func TestRouteTaskToolReturnsCompactReceipt(t *testing.T) {
	s := newTestServer(t)
	_, receipt, err := s.routeTaskTool(context.Background(), nil, withRouteROI(router.RouteRequest{
		Objective: "Implement three independent packages.", AcceptanceCriteria: []string{"Package tests pass."},
		VerificationTarget: "go test ./...", WorkerMarginalContribution: "Own one package while the Supervisor owns another.",
		IndependentSlices: 3, Checkability: "objective",
	}))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RouteID == "" || receipt.Action != router.ActionWorker || !receipt.WorkerAdmissible {
		t.Fatalf("bad compact receipt: %#v", receipt)
	}
	if receipt.RootVerifier == nil || receipt.RootVerifier.Status != verifierAvailable || receipt.RootVerifier.Command != "go test ./..." {
		t.Fatalf("Root verifier was not preflighted: %#v", receipt.RootVerifier)
	}
	if len(raw) > 2500 {
		t.Fatalf("route receipt too large: %d bytes", len(raw))
	}
	for _, forbidden := range []string{"candidate_comparison", "surface", "escalation", "accounting_unit"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("compact receipt leaked %q: %s", forbidden, raw)
		}
	}
}

func TestRootVerifierPreflight(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "verify.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		target string
		status string
	}{
		{"available script", "./verify.sh --focused", verifierAvailable},
		{"missing script", "./missing-test.sh", verifierUnavailable},
		{"descriptive", "Run tests and inspect the diff.", verifierResolutionRequired},
		{"missing Python module", "python3 -m claudex_module_that_does_not_exist", verifierUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := preflightRootVerifier(context.Background(), root, tt.target)
			if got.Status != tt.status {
				t.Fatalf("preflight=%#v want status=%s", got, tt.status)
			}
		})
	}
}

func TestRouteBoundStartInheritsFrozenDefaults(t *testing.T) {
	s := newTestServer(t)
	s.runModel = func(context.Context, claude.Request) claude.Result { return reportResult("completed") }
	_, plan, err := s.routeTask(context.Background(), nil, withRouteROI(router.RouteRequest{
		Objective: "Implement two independent parsers.", AcceptanceCriteria: []string{"Parser tests pass."},
		VerificationTarget: "go test ./parser", WorkerMarginalContribution: "Own one parser while the Supervisor owns the other.",
		IndependentSlices: 2, Checkability: "objective",
	}))
	if err != nil {
		t.Fatal(err)
	}
	in := WorkerStartInput{
		RouteID: plan.RouteID, SliceID: "parser-a", Objective: "Implement parser A.",
		Write: true, Paths: []string{"parser/a.go"},
	}
	_, out, err := s.startWorker(context.Background(), nil, in)
	if err != nil {
		t.Fatal(err)
	}
	if out.Admission.DeadlineMS != plan.WorkerPolicy.MaxWorkerDeadlineMS || out.Admission.MarginalContribution == "" {
		t.Fatalf("route defaults not inherited: %#v", out.Admission)
	}
	stored := s.sliceInputs["parser-a"]
	if stored.EstimatedWorkerSeconds != plan.WorkerPolicy.EstimatedWorkerSeconds ||
		stored.EstimatedParallelSavings != plan.WorkerPolicy.EstimatedParallelSavings ||
		stored.OutputContract == "" || !strings.Contains(stored.DoneCondition, "Root-owned") {
		t.Fatalf("stored compact start was not expanded from route: %#v", stored)
	}
}

func TestRouteBoundStartNarrowsCompositeRootVerifier(t *testing.T) {
	t.Setenv("CLAUDEX_PROJECT_VERIFIER", "go test ./...")
	s := newTestServer(t)
	s.runModel = func(context.Context, claude.Request) claude.Result { return reportResult("completed") }
	_, plan, err := s.routeTask(context.Background(), nil, withRouteROI(router.RouteRequest{
		Objective: "Implement two independent packages.", AcceptanceCriteria: []string{"Both package tests pass."},
		VerificationTarget: "go test ./a && go test ./b", WorkerMarginalContribution: "Own package A while the Supervisor owns package B.",
		IndependentSlices: 2, Checkability: "objective",
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.startWorker(context.Background(), nil, WorkerStartInput{
		RouteID: plan.RouteID, SliceID: "package-a", Objective: "Implement package A.",
		Write: true, Paths: []string{"a/a.go"},
	})
	if err != nil {
		t.Fatalf("compact start inherited a rejected composite verifier: %v", err)
	}
	if got := s.sliceInputs["package-a"].DoneCondition; multiCommandDone(got) || !strings.Contains(got, "Supervisor retains") {
		t.Fatalf("slice verifier was not narrowed while preserving Root ownership: %q", got)
	}
}

func TestVerifierAvailabilityControlsWorkerAdmission(t *testing.T) {
	s := newTestServer(t)
	request := withRouteROI(router.RouteRequest{
		Objective: "Implement two independent packages.", AcceptanceCriteria: []string{"Tests pass."},
		VerificationTarget: "./missing-verifier.sh", WorkerMarginalContribution: "Own one package while Root owns another.",
		IndependentSlices: 2, Checkability: "objective",
	})
	_, direct, err := s.routeTask(context.Background(), nil, request)
	if err != nil {
		t.Fatal(err)
	}
	if direct.Action != router.ActionDirect || direct.WorkerAdmissible {
		t.Fatalf("unavailable verifier must keep automatic route direct: %#v", direct)
	}
	if !strings.Contains(strings.Join(direct.WorkerRejectionReasons, " "), "verifier is not executable") {
		t.Fatalf("missing verifier rejection evidence: %#v", direct.WorkerRejectionReasons)
	}

	t.Setenv("CLAUDEX_PROJECT_VERIFIER", "go test ./...")
	_, receipt, err := s.routeTaskTool(context.Background(), nil, request)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Action != router.ActionWorker || receipt.RootVerifier == nil || receipt.RootVerifier.Source != "explicit_environment" {
		t.Fatalf("explicit project verifier did not restore admission: %#v", receipt)
	}
}

func TestProjectVerifierDiscoveryAndSetupContract(t *testing.T) {
	goRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(goRoot, "go.mod"), []byte("module example.test/x\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	goVerifier := resolveProjectVerifier(context.Background(), goRoot, "./missing.sh")
	if goVerifier.Status != verifierAvailableFallback || goVerifier.Command != "go test ./..." || goVerifier.Source != "go.mod" {
		t.Fatalf("Go verifier discovery failed: %#v", goVerifier)
	}

	nodeRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(nodeRoot, "package.json"), []byte(`{"scripts":{"test":"node --test"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodeRoot, "package-lock.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	nodeVerifier := resolveProjectVerifier(context.Background(), nodeRoot, "./missing.sh")
	if nodeVerifier.Status != verifierSetupRequired || nodeVerifier.SetupCommand != "npm ci" || nodeVerifier.SetupAllowed {
		t.Fatalf("Node setup contract was not reported safely: %#v", nodeVerifier)
	}
}

func TestSingleExplicitWriteScopeUsesVerifierGatedTools(t *testing.T) {
	s := newTestServer(t)
	path := filepath.Join(s.root, "pkg", "a.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := s.workerTools(true, []string{"pkg/a.go"}, true), []string{"Read", "Edit", "Bash"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("existing single-file tools=%v want=%v", got, want)
	}
	if got := s.workerTools(true, []string{"pkg/a.go"}, false); !reflect.DeepEqual(got, []string{"Read", "Edit"}) {
		t.Fatalf("root-only verifier exposed extra tools=%v", got)
	}
	if got := s.workerTools(true, []string{"pkg/new.go"}, true); !reflect.DeepEqual(got, []string{"Read", "Edit", "Bash", "Write"}) {
		t.Fatalf("new-file tools=%v", got)
	}
	if got := s.workerTools(true, []string{"pkg/a.go", "pkg/b.go"}, true); !reflect.DeepEqual(got, []string{"Read", "Grep", "Edit", "Write", "Bash"}) {
		t.Fatalf("multi-file tools=%v", got)
	}
}

func TestRootOnlyWorkerUsesLowerInitialTurnCap(t *testing.T) {
	if got := workerInitialMaxTurns(false); got != 8 {
		t.Fatalf("root-only initial turns=%d want=8", got)
	}
	if got := workerInitialMaxTurns(true); got != 12 {
		t.Fatalf("exact-verifier initial turns=%d want=12", got)
	}
}

func TestWorkerIntegrationDigestProvidesScopedPatch(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	path := filepath.Join(root, "owned.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "owned.txt")
	run("commit", "-qm", "base")
	if err := os.WriteFile(path, []byte("after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactDir := t.TempDir()
	t.Setenv("CLAUDEX_INTEGRATION_ARTIFACT_DIR", artifactDir)
	s := &Server{root: root}
	digest := s.workerIntegrationDigest(&workerState{workDir: root, paths: []string{"owned.txt"}, sliceID: "owned", admission: WorkerAdmission{RouteID: "route-test"}})
	if digest.DiffCheck != "pass" || digest.PatchTruncated || digest.ArtifactPath == "" || digest.PatchBytes == 0 || digest.PatchSHA256 == "" || !strings.Contains(digest.ReviewContract, "patch artifact") {
		t.Fatalf("bad integration digest: %#v", digest)
	}
	patch, err := os.ReadFile(digest.ArtifactPath)
	if err != nil || !strings.Contains(string(patch), "+after") {
		t.Fatalf("scoped patch artifact missing: %q err=%v", patch, err)
	}
	raw, err := json.Marshal(digest)
	if err != nil || bytes.Contains(raw, []byte(`"patch":`)) || len(raw) > 2500 {
		t.Fatalf("Supervisor digest leaked patch or grew too large (%d): %s", len(raw), raw)
	}
}

func TestWorkerIntegrationDigestAutoFixesOnlyIntroducedHygiene(t *testing.T) {
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "Test")
	path := filepath.Join(root, "owned.txt")
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "owned.txt")
	run("commit", "-qm", "base")
	if err := os.WriteFile(path, []byte("after  \n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDEX_INTEGRATION_ARTIFACT_DIR", t.TempDir())
	s := &Server{root: root}
	digest := s.workerIntegrationDigest(&workerState{workDir: root, paths: []string{"owned.txt"}, sliceID: "owned", admission: WorkerAdmission{RouteID: "route-test"}})
	if digest.DiffCheck != "pass" || !digest.AutoFixed || len(digest.AutoFixes) != 2 {
		t.Fatalf("hygiene was not repaired and rechecked: %#v", digest)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "after\n" {
		t.Fatalf("unexpected repaired file %q err=%v", got, err)
	}
}

func TestReadOnlyWorkerIntegrationNeedsNoPatch(t *testing.T) {
	s := &Server{root: t.TempDir()}
	digest := s.workerIntegrationDigest(&workerState{workDir: s.root, write: false})
	if digest.DiffCheck != "pass" || digest.ArtifactPath != "" || !strings.Contains(digest.ReviewContract, "Read-only") {
		t.Fatalf("read-only integration should pass without a patch: %#v", digest)
	}
}

func TestExactWorkerVerifierClassification(t *testing.T) {
	tests := []struct {
		value string
		exact bool
	}{
		{"go test ./internal/router", true},
		{"PYTHONPATH=src .venv/bin/python -m pytest -q", true},
		{"./scripts/test.sh --focused", true},
		{"Run the visible pytest suite (python -m pytest) and inspect the diff.", false},
		{"python -m pytest && git diff --check", false},
		{"python -m pip install pytest", false},
		{"", false},
	}
	for _, tt := range tests {
		_, got := exactWorkerVerifier(tt.value)
		if got != tt.exact {
			t.Errorf("exactWorkerVerifier(%q)=%v want=%v", tt.value, got, tt.exact)
		}
	}
}

func TestDescriptiveVerifierAdmitsRootOnlyWithoutBash(t *testing.T) {
	in := qualifiedInput()
	in.DoneCondition = "Run package tests and inspect the final diff."
	admission := evaluateWorkerAdmission(in)
	if admission.Result != admissionAdmitted || admission.VerifierMode != verifierRootOnly || admission.VerifierCommand != "" {
		t.Fatalf("descriptive verifier should be admitted Root-only: %#v", admission)
	}
	brief := workerStartBrief(in)
	if !strings.Contains(brief, "Bash is withheld") || !strings.Contains(brief, "report verification as unverified") {
		t.Fatalf("Root-only verifier instructions missing: %s", brief)
	}
}

func TestExactVerifierAdmissionIsOneShot(t *testing.T) {
	in := qualifiedInput()
	in.DoneCondition = "go test ./parser"
	admission := evaluateWorkerAdmission(in)
	if admission.VerifierMode != verifierExactOnce || admission.VerifierCommand != in.DoneCondition {
		t.Fatalf("exact verifier mode missing: %#v", admission)
	}
	brief := workerStartBrief(in)
	for _, want := range []string{"exact-once", "at most once", "Do not substitute commands", "install dependencies"} {
		if !strings.Contains(brief, want) {
			t.Fatalf("exact verifier instruction missing %q: %s", want, brief)
		}
	}
}

func TestMaxTurnsWithInScopePatchIsProvisional(t *testing.T) {
	s := newTestServer(t)
	s.runModel = func(context.Context, claude.Request) claude.Result {
		return claude.Result{
			Success: false, SessionID: "worker-session", ResolvedModel: "grok-4.5-build",
			Subtype: "error_max_turns", TerminalReason: "max turns", ExitError: "exit status 1",
			ChangedPaths: []string{"pkg/a.go"}, ToolUses: map[string]int{"Read": 1, "Edit": 1},
		}
	}
	in := qualifiedInput()
	in.Write = true
	in.Paths = []string{"pkg/a.go"}
	_, out, err := s.startWorker(context.Background(), nil, in)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "provisional" || !out.Provisional || out.RetryEligible || out.Report.Status != "provisional" {
		t.Fatalf("bounded patch must require verification without being discarded: %#v", out)
	}
	if len(out.Report.ChangedPaths) != 1 || out.Report.ChangedPaths[0] != "pkg/a.go" {
		t.Fatalf("provisional paths lost: %#v", out.Report)
	}
}
