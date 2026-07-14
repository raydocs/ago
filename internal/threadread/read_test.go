package threadread

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareUsesCompactForOrientationAndRedactsSecrets(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "thread-123"
	path := filepath.Join(project, id+".jsonl")
	raw := strings.Join([]string{
		`{"type":"user","uuid":"u1","sessionId":"thread-123","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"API key=00000000000000000000000000000000.TESTFIXTURE00000000 implement parser"}}`,
		`{"type":"user","uuid":"compact","sessionId":"thread-123","timestamp":"2026-01-01T00:01:00Z","isCompactSummary":true,"message":{"role":"user","content":"Summary: parser decision remains open"}}`,
		`{"type":"assistant","uuid":"a1","sessionId":"thread-123","timestamp":"2026-01-01T00:02:00Z","message":{"role":"assistant","model":"gpt-5.6-sol","content":[{"type":"text","text":"Parser tests passed with command go test ./parser"}]}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	prepared, err := Prepare(root, "https://example.test/#/thread/"+id, "What verified the parser?", 16*1024)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ThreadID != id || !prepared.LatestCompact || prepared.SelectedEvents != 3 {
		t.Fatalf("unexpected selection: %#v", prepared)
	}
	if strings.Contains(prepared.Packet, "505ba8") || !strings.Contains(prepared.Packet, "[REDACTED]") || !strings.Contains(prepared.Packet, "go test ./parser") || !strings.Contains(prepared.Packet, "thread://thread-123#a1") {
		t.Fatalf("packet is unsafe or incomplete: %s", prepared.Packet)
	}
}

func TestNormalizeThreadIDRejectsTraversal(t *testing.T) {
	for _, value := range []string{"../secret", "", "https://example.test/#/thread/../secret"} {
		if _, err := NormalizeThreadID(value); err == nil {
			t.Fatalf("accepted unsafe thread reference %q", value)
		}
	}
}

func TestSelectionReservesLatestCompactBeforeCommonQueryMatches(t *testing.T) {
	events := make([]event, 0, 24)
	for index := 0; index < 23; index++ {
		events = append(events, event{id: "old", text: strings.Repeat("parser evidence ", 180)})
	}
	events = append(events, event{id: "latest-compact", text: "latest compact orientation", compact: true})
	selected, included := selectEvents(events, "parser", MinSourceBytes)
	if !included {
		t.Fatal("latest compact was crowded out by common query matches")
	}
	found := false
	for _, item := range selected {
		if item.id == "latest-compact" {
			found = true
		}
	}
	if !found {
		t.Fatal("selection flag claimed compact inclusion without retaining it")
	}
}
