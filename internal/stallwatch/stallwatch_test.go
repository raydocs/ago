package stallwatch

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWatchReturnsWhenTranscriptProgresses(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(transcript, []byte("tool result\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := hookJSON(t, HookInput{SessionID: "session-1", TranscriptPath: transcript, HookEventName: "PostToolUse", ToolName: "Read", ToolUseID: "tool-1"})
	go func() {
		time.Sleep(25 * time.Millisecond)
		file, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = file.WriteString("assistant thinking\n")
			_ = file.Close()
		}
	}()
	out, err := Watch(context.Background(), bytes.NewReader(input), Config{Timeout: time.Second, Poll: 5 * time.Millisecond, StateDir: filepath.Join(dir, "state"), Blocking: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "progressed" {
		t.Fatalf("state=%q, want progressed", out.State)
	}
}

func TestWatchNonBlockingNeverWaits(t *testing.T) {
	// Production PostToolUse path must return immediately (self-deadlock fix).
	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(transcript, []byte("tool result\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := hookJSON(t, HookInput{SessionID: "s", TranscriptPath: transcript, HookEventName: "PostToolUse", ToolName: "Bash", ToolUseID: "t1"})
	started := time.Now()
	out, err := Watch(context.Background(), bytes.NewReader(input), Config{Timeout: 5 * time.Minute, Poll: time.Second, StateDir: filepath.Join(dir, "state"), Blocking: false})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "nonblocking_pass" {
		t.Fatalf("state=%q", out.State)
	}
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("non-blocking watch took too long: %s", time.Since(started))
	}
}

func TestWatchClaimsOneRewakeAndLogsIt(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(transcript, []byte("tool result\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hook := HookInput{SessionID: "session-2", TranscriptPath: transcript, HookEventName: "PostToolUse", ToolName: "Playwright", ToolUseID: "tool-2"}
	input := hookJSON(t, hook)
	cfg := Config{Timeout: 25 * time.Millisecond, Poll: 5 * time.Millisecond, StateDir: filepath.Join(dir, "state"), Blocking: true}
	first, err := Watch(context.Background(), bytes.NewReader(input), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != "stalled" || first.RecoveryID == "" || !strings.Contains(first.Message, "CLAUDEX_STALL_REWAKE") {
		t.Fatalf("unexpected first outcome: %#v", first)
	}
	second, err := Watch(context.Background(), bytes.NewReader(input), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if second.State != "duplicate" || second.RecoveryID != first.RecoveryID {
		t.Fatalf("unexpected duplicate outcome: %#v", second)
	}
	log, err := os.ReadFile(filepath.Join(cfg.StateDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(log)), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], first.RecoveryID) {
		t.Fatalf("unexpected event log: %s", log)
	}
}

func TestWatchFailsOpenWhenTranscriptIsUnavailable(t *testing.T) {
	dir := t.TempDir()
	input := hookJSON(t, HookInput{SessionID: "session-3", TranscriptPath: filepath.Join(dir, "missing.jsonl"), HookEventName: "PostToolUse"})
	out, err := Watch(context.Background(), bytes.NewReader(input), Config{Timeout: time.Millisecond, Poll: time.Millisecond, StateDir: filepath.Join(dir, "state"), Blocking: true})
	if err != nil {
		t.Fatal(err)
	}
	if out.State != "unavailable" {
		t.Fatalf("state=%q, want unavailable", out.State)
	}
}

func hookJSON(t *testing.T, input HookInput) []byte {
	t.Helper()
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
