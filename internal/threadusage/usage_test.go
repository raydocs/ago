package threadusage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseValidUsageLine(t *testing.T) {
	line := `{"timestamp":"2026-07-13T19:23:25.903Z","sessionId":"sess-1","requestId":"req-1","isSidechain":false,"message":{"id":"msg-1","model":"gpt-5.6-sol","usage":{"input_tokens":100,"output_tokens":20,"cache_creation_input_tokens":0,"cache_read_input_tokens":50,"speed":"standard"}}}`
	rec, ok := ParseLine([]byte(line))
	if !ok {
		t.Fatal("expected parse success")
	}
	if rec.SessionID != "sess-1" || rec.MessageID != "msg-1" || rec.RequestID != "req-1" {
		t.Fatalf("ids: %+v", rec)
	}
	if rec.Model != "gpt-5.6-sol" || rec.InputTokens != 100 || rec.OutputTokens != 20 || rec.CacheReadTokens != 50 {
		t.Fatalf("tokens: %+v", rec)
	}
	if rec.IsFast || rec.UsageID == "" {
		t.Fatalf("flags/id: %+v", rec)
	}
}

func TestParseLegacyAndSplitCacheBuckets(t *testing.T) {
	legacy := `{"timestamp":"2026-07-13T19:00:00Z","sessionId":"s","requestId":"r","message":{"id":"m1","model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":30,"cache_read_input_tokens":4}}}`
	rec, ok := ParseLine([]byte(legacy))
	if !ok || rec.CacheWrite5mTokens != 30 || rec.CacheWrite1hTokens != 0 {
		t.Fatalf("legacy cache: ok=%v rec=%+v", ok, rec)
	}

	split := `{"timestamp":"2026-07-13T19:00:00Z","sessionId":"s","requestId":"r","message":{"id":"m2","model":"claude-opus-4-8","usage":{"input_tokens":1,"output_tokens":2,"cache_creation":{"ephemeral_5m_input_tokens":10,"ephemeral_1h_input_tokens":20},"cache_read_input_tokens":4,"speed":"fast"}}}`
	rec2, ok := ParseLine([]byte(split))
	if !ok || rec2.CacheWrite5mTokens != 10 || rec2.CacheWrite1hTokens != 20 || !rec2.IsFast {
		t.Fatalf("split cache: ok=%v rec=%+v", ok, rec2)
	}
}

func TestParseMissingUsageMalformedEmptyIDs(t *testing.T) {
	cases := []string{
		`{"timestamp":"2026-07-13T19:00:00Z","type":"user","message":{"role":"user","content":"hi"}}`,
		`{not-json`,
		`{"timestamp":"2026-07-13T19:00:00Z","sessionId":"s","requestId":"","message":{"id":"m","model":"x","usage":{"input_tokens":1,"output_tokens":2}}}`,
		`{"timestamp":"2026-07-13T19:00:00Z","sessionId":"s","requestId":"r","message":{"id":"","model":"x","usage":{"input_tokens":1,"output_tokens":2}}}`,
		`{"timestamp":"2026-07-13T19:00:00Z","sessionId":"s","requestId":"r","message":{"id":"m","model":"","usage":{"input_tokens":1,"output_tokens":2}}}`,
		`{"timestamp":"2026-07-13T19:00:00Z","sessionId":"s","requestId":"r","message":{"id":"m","model":"x","usage":{"input_tokens":1,"output_tokens":2,"speed":"turbo"}}}`,
	}
	for _, line := range cases {
		if _, ok := ParseLine([]byte(line)); ok {
			t.Fatalf("expected skip for %s", line)
		}
	}
}

func TestParseCarriedCost(t *testing.T) {
	line := `{"timestamp":"2026-07-13T19:00:00Z","sessionId":"s","requestId":"r","costUSD":1.25,"message":{"id":"m","model":"x","usage":{"input_tokens":1,"output_tokens":2}}}`
	rec, ok := ParseLine([]byte(line))
	if !ok || rec.CarriedCostUSD == nil || *rec.CarriedCostUSD != 1.25 {
		t.Fatalf("cost: ok=%v rec=%+v", ok, rec)
	}
}

func TestDedupMessageRequestAndSidechain(t *testing.T) {
	base := Record{
		UsageID: "a", SessionID: "s", MessageID: "m1", RequestID: "r1", ObservedAt: "t1", Model: "m",
		InputTokens: 10, OutputTokens: 1, isSidechain: false, hasSpeed: false,
	}
	dupExact := base
	dupExact.UsageID = "b"
	dupExact.InputTokens = 5 // smaller total should lose

	sidechain := base
	sidechain.UsageID = "c"
	sidechain.RequestID = "r2"
	sidechain.isSidechain = true
	sidechain.InputTokens = 100 // larger but sidechain loses to parent

	richer := base
	richer.UsageID = "d"
	richer.RequestID = "r3"
	richer.hasSpeed = true
	// same total, richer should replace if collision exact — different request so only sidechain path applies

	out := Dedup([]Record{base, dupExact, sidechain})
	if len(out) != 1 {
		t.Fatalf("expected 1 after exact+sidechain, got %d %#v", len(out), out)
	}
	if out[0].InputTokens != 10 || out[0].isSidechain {
		t.Fatalf("expected parent kept: %+v", out[0])
	}

	// exact key collision with richer speed field and larger tokens
	existing := base
	existing.hasSpeed = false
	candidate := base
	candidate.InputTokens = 50
	candidate.hasSpeed = true
	out2 := Dedup([]Record{existing, candidate})
	if len(out2) != 1 || out2[0].InputTokens != 50 || !out2[0].hasSpeed {
		t.Fatalf("expected richer/larger: %+v", out2)
	}

	// no message id always kept
	noID := Record{UsageID: "x", InputTokens: 1, OutputTokens: 1}
	out3 := Dedup([]Record{noID, noID})
	if len(out3) != 2 {
		t.Fatalf("expected both no-id records kept, got %d", len(out3))
	}
}

func TestScanIncrementalPartialAndTruncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	line1 := `{"timestamp":"2026-07-13T19:00:00Z","sessionId":"s","requestId":"r1","message":{"id":"m1","model":"x","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n"
	partial := `{"timestamp":"2026-07-13T19:00:01Z","sessionId":"s","requestId":"r2","message":{"id":"m2","model":"x","usage":{"input_tokens":3,"output_tokens":4`
	if err := os.WriteFile(path, []byte(line1+partial), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := ScanFile(path, FileCursor{})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 1 || !first.PartialEnd {
		t.Fatalf("first scan: records=%d partial=%v", len(first.Records), first.PartialEnd)
	}
	if first.Next.Offset != int64(len(line1)) {
		t.Fatalf("offset should stop before partial line: got %d want %d", first.Next.Offset, len(line1))
	}

	// Complete the partial line and append another.
	complete := `{"timestamp":"2026-07-13T19:00:01Z","sessionId":"s","requestId":"r2","message":{"id":"m2","model":"x","usage":{"input_tokens":3,"output_tokens":4}}}` + "\n"
	line3 := `{"timestamp":"2026-07-13T19:00:02Z","sessionId":"s","requestId":"r3","message":{"id":"m3","model":"x","usage":{"input_tokens":5,"output_tokens":6}}}` + "\n"
	if err := os.WriteFile(path, []byte(line1+complete+line3), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := ScanFile(path, first.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Records) != 2 {
		t.Fatalf("second scan expected 2 new records, got %d", len(second.Records))
	}

	// Append-only: third scan with same cursor should yield nothing when file unchanged.
	third, err := ScanFile(path, second.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Records) != 0 || third.Scanned != 0 {
		t.Fatalf("third scan should be empty: %#v", third)
	}

	// Truncate/restart should rescan safely.
	if err := os.WriteFile(path, []byte(line1), 0o600); err != nil {
		t.Fatal(err)
	}
	restart, err := ScanFile(path, second.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(restart.Records) != 1 {
		t.Fatalf("truncate rescan expected 1, got %d", len(restart.Records))
	}
}

func TestScanFileLimitedBatchesUsageWithoutSkippingLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	lines := []string{
		`{"type":"user","sessionId":"s","timestamp":"2026-07-13T19:00:00Z","message":{"role":"user","content":"visible prompt"}}`,
		`{"timestamp":"2026-07-13T19:00:01Z","sessionId":"s","requestId":"r1","message":{"id":"m1","model":"x","usage":{"input_tokens":1,"output_tokens":2}}}`,
		`{"type":"assistant","sessionId":"s","timestamp":"2026-07-13T19:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"visible answer"}]}}`,
		`{"timestamp":"2026-07-13T19:00:03Z","sessionId":"s","requestId":"r2","message":{"id":"m2","model":"x","usage":{"input_tokens":3,"output_tokens":4}}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := ScanFileLimited(path, FileCursor{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 1 || first.Next.Offset <= 0 || first.Next.Offset >= int64(len(strings.Join(lines, "\n")+"\n")) {
		t.Fatalf("unexpected first batch: %#v", first)
	}
	second, err := ScanFileLimited(path, first.Next, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Records) != 1 || second.Next.Offset <= first.Next.Offset {
		t.Fatalf("unexpected second batch: %#v", second)
	}
	third, err := ScanFileLimited(path, second.Next, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Records) != 0 || third.Scanned != 0 {
		t.Fatalf("expected completed scan, got %#v", third)
	}
}

func TestAtomicCursorState(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "thread-usage-state.json")
	transcript := "/tmp/secret-session.jsonl"
	if err := StoreCursor(statePath, transcript, FileCursor{Offset: 42, Size: 100, MtimeNs: 7}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("state perms too open: %v", info.Mode())
	}
	got, err := LoadCursor(statePath, transcript)
	if err != nil {
		t.Fatal(err)
	}
	if got.Offset != 42 || got.Size != 100 {
		t.Fatalf("cursor: %+v", got)
	}
	raw, _ := os.ReadFile(statePath)
	if strings.Contains(string(raw), transcript) {
		t.Fatalf("state should hash path, not store raw transcript path: %s", raw)
	}
}

func TestNoPromptContentInEncodedUpload(t *testing.T) {
	rec := Record{
		UsageID: "u1", SessionID: "s", MessageID: "m", RequestID: "r", ObservedAt: "t",
		Model: "x", InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3,
	}
	encoded, err := EncodeUploadJSON([]Record{rec})
	if err != nil {
		t.Fatal(err)
	}
	if ContainsContentLeak(encoded) {
		t.Fatalf("content leak in upload: %s", encoded)
	}
	var rows []map[string]any
	if err := json.Unmarshal(encoded, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["input_tokens"].(float64) != 1 {
		t.Fatalf("rows: %s", encoded)
	}
}

func TestAttachToPayloadStopOnly(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "t.jsonl")
	line := `{"timestamp":"2026-07-13T19:00:00Z","sessionId":"sess","requestId":"r1","message":{"id":"m1","model":"x","usage":{"input_tokens":9,"output_tokens":1,"cache_read_input_tokens":100}}}` + "\n"
	if err := os.WriteFile(transcript, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, "state.json")

	// Non-stop events do not attach.
	payload := map[string]any{
		"session_id":      "sess",
		"hook_event_name": "UserPromptSubmit",
		"transcript_path": transcript,
		"prompt":          "do not upload this as usage",
	}
	if res := AttachToPayload(payload, statePath); res.Attached {
		t.Fatal("should not attach on UserPromptSubmit")
	}

	stop := map[string]any{
		"session_id":      "sess",
		"hook_event_name": "Stop",
		"transcript_path": transcript,
	}
	res := AttachToPayload(stop, statePath)
	if !res.Attached || len(res.Records) != 1 {
		t.Fatalf("attach failed: %#v", res)
	}
	records, ok := stop["usage_records"].([]map[string]any)
	if !ok || len(records) != 1 {
		t.Fatalf("usage_records missing: %#v", stop["usage_records"])
	}
	raw, _ := json.Marshal(stop["usage_records"])
	if ContainsContentLeak(raw) || strings.Contains(string(raw), "do not upload") {
		t.Fatalf("usage payload leaked content: %s", raw)
	}
	if err := CommitCursor(res); err != nil {
		t.Fatal(err)
	}
	// Second attach should see no new records.
	stop2 := map[string]any{
		"session_id":      "sess",
		"hook_event_name": "SessionEnd",
		"transcript_path": transcript,
	}
	res2 := AttachToPayload(stop2, statePath)
	if !res2.Attached || len(res2.Records) != 0 {
		t.Fatalf("expected empty delta after cursor commit: %#v", res2)
	}
}
