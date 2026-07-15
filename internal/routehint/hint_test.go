package routehint

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func build(t *testing.T, prompt string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "prompt": prompt})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Build(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func TestSpecialistPromptsReceiveOneBoundedHint(t *testing.T) {
	tests := map[string]string{
		"research today's current vendor announcement":                      "search_external",
		"extract the model table from https://ampcode.com/models":           "digest_urls",
		"locate repository dependencies and map the implementation surface": "explore_repository",
		"find thread that modified thread-app/src/usage.ts":                 "find_thread",
		"read https://ampcode.com/threads/T-123 and extract the decision":   "digest_urls",
		"read https://claudex-threads.ppop.workers.dev/#/thread/T-123":      "read_thread",
	}
	for prompt, tool := range tests {
		got := build(t, prompt)
		if !strings.Contains(got, "mcp__claudex-flow__"+tool) || !strings.Contains(got, "zero-model") {
			t.Fatalf("prompt %q did not route to %s: %s", prompt, tool, got)
		}
	}
}

func TestOrdinaryAndComplexImplementationPromptsStaySilent(t *testing.T) {
	for _, prompt := range []string{
		"fix the localized parser bug in parser.go",
		"implement the workflow router and run go test",
		"continue",
	} {
		if got := build(t, prompt); got != "" {
			t.Fatalf("prompt %q received unnecessary routing context: %s", prompt, got)
		}
	}
}

func TestSingleFileVerifiedChangeGetsFastPath(t *testing.T) {
	got := build(t, "fix internal/catalog/catalog.go and run go test ./internal/catalog")
	if !strings.Contains(got, "CLAUDEX_FAST_PATH") || !strings.Contains(got, "Skip route_task") {
		t.Fatalf("missing fast path: %s", got)
	}
}

func TestNonPromptEventStaysSilent(t *testing.T) {
	raw := []byte(`{"hook_event_name":"Stop","prompt":"research today's news"}`)
	out, err := Build(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("unexpected output: %s", out)
	}
}
