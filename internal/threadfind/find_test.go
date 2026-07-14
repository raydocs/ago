package threadfind

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFindByKeywordFileProjectAndAmpFilters(t *testing.T) {
	root := t.TempDir()
	writeThread(t, root, "-Users-test-alpha", "thread-alpha", strings.Join([]string{
		`{"type":"user","uuid":"u1","sessionId":"thread-alpha","timestamp":"2026-07-10T10:00:00Z","cwd":"/Users/test/alpha","message":{"role":"user","content":"Implement the database migration."}}`,
		`{"type":"assistant","uuid":"a1","sessionId":"thread-alpha","timestamp":"2026-07-10T10:01:00Z","cwd":"/Users/test/alpha","message":{"role":"assistant","model":"gpt-5.6-sol","content":[{"type":"tool_use","id":"tool-1","name":"Edit","input":{"file_path":"src/server/index.ts","new_string":"done"}}]}}`,
		`{"type":"ai-title","aiTitle":"Database migration implementation","sessionId":"thread-alpha"}`,
	}, "\n")+"\n")
	writeThread(t, root, "-Users-test-beta", "thread-beta", `{"type":"user","uuid":"u2","sessionId":"thread-beta","timestamp":"2026-07-11T10:00:00Z","cwd":"/Users/test/beta","message":{"role":"user","content":"Unrelated UI work"}}`+"\n")
	writeThread(t, root, "-Users-test-xylophone", "thread-xylophone", `{"type":"user","uuid":"u3","sessionId":"thread-xylophone","timestamp":"2026-07-11T10:00:00Z","cwd":"/Users/test/xylophone","message":{"role":"user","content":"database migration src/server/index.ts"}}`+"\n")

	result, err := Find(root, Query{Text: `"database migration" file:src/server/index.ts project:alpha`, Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 {
		t.Fatalf("matches=%#v", result.Matches)
	}
	match := result.Matches[0]
	if match.ThreadID != "thread-alpha" || match.Title != "Database migration implementation" || match.Project != "alpha" || match.Source != "thread://thread-alpha" {
		t.Fatalf("unexpected match: %#v", match)
	}
	joined := strings.Join(match.MatchedBy, " ")
	if !strings.Contains(joined, "keyword:database migration") || !strings.Contains(joined, "file:src/server/index.ts") || !strings.Contains(joined, "project:alpha") {
		t.Fatalf("match evidence incomplete: %#v", match)
	}
	if result.ThreadsSeen != 3 || result.ThreadsScanned != 1 || result.BytesScanned == 0 || result.SearchMode != "streaming_local_root_transcripts" {
		t.Fatalf("search accounting incomplete: %#v", result)
	}
}

func TestFindReturnsSanitizedSnippetAndExcludesSidechains(t *testing.T) {
	root := t.TempDir()
	writeThread(t, root, "project", "thread-secret", `{"type":"user","uuid":"u1","sessionId":"thread-secret","timestamp":"2026-07-10T10:00:00Z","cwd":"/tmp/project","message":{"role":"user","content":"api_key=00000000000000000000000000000000.TESTFIXTURE00000000 parser requirement"}}`+"\n")
	sidechainDir := filepath.Join(root, "project", "thread-secret", "subagents")
	if err := os.MkdirAll(sidechainDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidechainDir, "agent.jsonl"), []byte(`{"type":"user","message":{"role":"user","content":"sidechain-only-term"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := Find(root, Query{Text: "parser"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Matches) != 1 || strings.Contains(strings.Join(result.Matches[0].Snippets, " "), "505ba8") || !strings.Contains(strings.Join(result.Matches[0].Snippets, " "), "[REDACTED]") {
		t.Fatalf("secret was not sanitized: %#v", result.Matches)
	}
	missing, err := Find(root, Query{Text: "sidechain-only-term"})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing.Matches) != 0 || missing.ThreadsSeen != 1 {
		t.Fatalf("nested sidechain was indexed: %#v", missing)
	}
}

func TestFindBoundsDatesAndInput(t *testing.T) {
	root := t.TempDir()
	writeThread(t, root, "project", "thread-1", `{"type":"user","uuid":"u1","sessionId":"thread-1","timestamp":"2026-07-10T10:00:00Z","cwd":"/tmp/project","message":{"role":"user","content":"migration"}}`+"\n")
	path := filepath.Join(root, "project", "thread-1.jsonl")
	mtime := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	result, err := Find(root, Query{Text: "migration", After: "2026-07-10", Before: "2026-07-11", ExcludeThreadID: "other"})
	if err != nil || len(result.Matches) != 1 {
		t.Fatalf("date bounds failed: result=%#v err=%v", result, err)
	}
	for _, input := range []Query{{}, {Text: "x", Limit: MaxLimit + 1}, {After: "tomorrow"}, {After: "2026-07-11", Before: "2026-07-10"}} {
		if _, err := Find(root, input); err == nil {
			t.Fatalf("accepted invalid input: %#v", input)
		}
	}
}

func writeThread(t *testing.T, root, project, id, raw string) {
	t.Helper()
	dir := filepath.Join(root, project)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
}
