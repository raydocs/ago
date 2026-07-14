package workflow

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"claudexflow/internal/router"
)

func TestBuildBriefDoesNotClaimVerification(t *testing.T) {
	got := buildBrief("inspect", "", false)
	if got == "" || !contains(got, "do not claim tests passed") {
		t.Fatalf("brief lacks honesty guard: %s", got)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("abcdef", 3); got != "abc\n...[truncated]" {
		t.Fatalf("got %q", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestExecuteWithoutVerifierIsUnverifiedAndSingleCall(t *testing.T) {
	dir := t.TempDir()
	count := installFakeClaude(t, dir)
	out, err := Execute(context.Background(), Options{Task: "inspect", Kind: router.KindGeneral, WorkDir: dir, SettingsPath: "ignored", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "completed_unverified" || out.Verify.Performed || out.Verify.Passed {
		t.Fatalf("unexpected outcome: %#v", out)
	}
	if got := readCount(t, count); got != "1" {
		t.Fatalf("model calls=%s, want 1", got)
	}
}

func TestExecuteRepairsAtMostOnceAfterVerifierFailure(t *testing.T) {
	dir := t.TempDir()
	count := installFakeClaude(t, dir)
	verify := "if [ ! -f gate ]; then touch gate; exit 1; fi"
	out, err := Execute(context.Background(), Options{Task: "repair", Kind: router.KindGeneral, WorkDir: dir, SettingsPath: "ignored", VerifyCommand: verify, Repair: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != "passed_after_repair" || len(out.Attempts) != 2 {
		t.Fatalf("unexpected outcome: %#v", out)
	}
	if got := readCount(t, count); got != "2" {
		t.Fatalf("model calls=%s, want 2", got)
	}
}

func TestCapabilityToolsMatchSelectedLane(t *testing.T) {
	tests := []struct {
		tool string
		want []string
	}{
		{"search_external", []string{"WebSearch", "WebFetch"}},
		{"digest_urls", []string{"WebFetch"}},
		{"explore_repository", []string{"Read", "Grep", "Glob"}},
	}
	for _, tt := range tests {
		got := executionTools(router.Decision{Tool: tt.tool}, false)
		if len(got) != len(tt.want) {
			t.Fatalf("%s tools=%v want=%v", tt.tool, got, tt.want)
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Fatalf("%s tools=%v want=%v", tt.tool, got, tt.want)
			}
		}
	}
}

func TestStandaloneRunRejectsCapabilityPlusWriteBeforeModelCall(t *testing.T) {
	dir := t.TempDir()
	count := installFakeClaude(t, dir)
	_, err := Execute(context.Background(), Options{Task: "implement using today's latest X information", Kind: router.KindAuto, Write: true, WorkDir: dir, SettingsPath: "ignored", Timeout: time.Second})
	if err == nil || !contains(err.Error(), "requires the Claude X Supervisor") {
		t.Fatalf("expected compound route rejection, got %v", err)
	}
	if _, statErr := os.Stat(count); !os.IsNotExist(statErr) {
		t.Fatalf("rejected compound route invoked a model: %v", statErr)
	}
}

func installFakeClaude(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	count := filepath.Join(dir, "count")
	script := filepath.Join(bin, "claude")
	body := "#!/bin/sh\nn=0; [ -f \"$FAKE_CLAUDE_COUNT\" ] && n=$(cat \"$FAKE_CLAUDE_COUNT\")\necho $((n+1)) > \"$FAKE_CLAUDE_COUNT\"\necho '{\"type\":\"result\",\"result\":\"OK\"}'\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_CLAUDE_COUNT", count)
	return count
}

func readCount(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(bytesTrimSpace(b))
}

func bytesTrimSpace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\t' || b[start] == '\r') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\t' || b[end-1] == '\r') {
		end--
	}
	return b[start:end]
}
