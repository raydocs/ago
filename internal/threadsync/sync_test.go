package threadsync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"claudexflow/internal/sessionbind"
)

func TestSanitizeRemovesSecretsAndPreservesWorkflowFields(t *testing.T) {
	payload := map[string]any{
		"session_id":      "session-1",
		"authorization":   "Bearer top-secret",
		"prompt":          "use api_key=abc123456789 and sk-abcdefghijklmnop then aabbccddeeff00112233445566778899.ExampleToken1234",
		"tool_input":      map[string]any{"objective": "implement parser", "token": "secret-value"},
		"hook_event_name": "UserPromptSubmit",
	}
	got := sanitizeMap(payload, 16_000)
	raw, _ := json.Marshal(got)
	text := string(raw)
	for _, secret := range []string{"top-secret", "abc123456789", "sk-abcdefghijklmnop", "aabbccddeeff00112233445566778899", "secret-value"} {
		if strings.Contains(text, secret) {
			t.Fatalf("secret %q survived sanitization: %s", secret, text)
		}
	}
	if !strings.Contains(text, "session-1") || !strings.Contains(text, "implement parser") {
		t.Fatalf("workflow fields were lost: %s", text)
	}
}

func TestCollectSendsSanitizedEvent(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ingest" {
			t.Fatalf("unexpected auth %q", r.Header.Get("Authorization"))
		}
		received, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "thread-sync.json")
	raw, _ := json.Marshal(Config{Enabled: true, Endpoint: server.URL, IngestToken: "ingest", MachineID: "test-mac"})
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDEX_THREAD_SYNC_CONFIG", configPath)
	t.Setenv("CLAUDEX_THREAD_MODEL", "grok-4.5")
	t.Setenv("CLAUDEX_THREAD_EFFORT", "high")
	t.Setenv("CLAUDEX_THREAD_ROLE", "worker")
	t.Setenv("CLAUDEX_SESSION_BINDING_DIR", filepath.Join(dir, "session-bindings"))

	delivery := Collect(context.Background(), strings.NewReader(`{"session_id":"s1","cwd":"`+dir+`","hook_event_name":"SessionStart","token":"do-not-send"}`))
	if !delivery.Sent || delivery.Spooled {
		t.Fatalf("unexpected delivery %#v", delivery)
	}
	if strings.Contains(string(received), "do-not-send") || !strings.Contains(string(received), "grok-4.5") || !strings.Contains(string(received), "test-mac") {
		t.Fatalf("unexpected payload %s", received)
	}
	if binding, ok := sessionbind.Resolve(os.Getppid(), dir); !ok || binding.SessionID != "s1" {
		t.Fatalf("Claude process/session binding was not recorded: %#v ok=%t", binding, ok)
	}
}

func TestCollectPreservesNumericUsageRecords(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "thread-sync.json")
	config, _ := json.Marshal(Config{Enabled: true, Endpoint: server.URL, IngestToken: "ingest"})
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(dir, "session.jsonl")
	line := `{"timestamp":"2026-07-13T19:00:00Z","sessionId":"s1","requestId":"r1","message":{"id":"m1","model":"gpt-5.6-sol","content":"must-not-be-uploaded","usage":{"input_tokens":11,"output_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":5},"cache_read_input_tokens":13,"speed":"standard"}}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDEX_THREAD_SYNC_CONFIG", configPath)
	t.Setenv("CLAUDEX_THREAD_USAGE_STATE", filepath.Join(dir, "usage-state.json"))

	input, _ := json.Marshal(map[string]any{
		"session_id":      "s1",
		"hook_event_name": "Stop",
		"transcript_path": transcriptPath,
	})
	delivery := Collect(context.Background(), strings.NewReader(string(input)))
	if !delivery.Sent {
		t.Fatalf("expected sent delivery, got %#v", delivery)
	}

	var payload struct {
		UsageRecords []struct {
			InputTokens        int64 `json:"input_tokens"`
			CacheWrite5mTokens int64 `json:"cache_write_5m_tokens"`
			CacheWrite1hTokens int64 `json:"cache_write_1h_tokens"`
			CacheReadTokens    int64 `json:"cache_read_tokens"`
			OutputTokens       int64 `json:"output_tokens"`
		} `json:"usage_records"`
	}
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.UsageRecords) != 1 {
		t.Fatalf("expected one usage record, got %s", received)
	}
	record := payload.UsageRecords[0]
	if record.InputTokens != 11 || record.CacheWrite5mTokens != 3 || record.CacheWrite1hTokens != 5 || record.CacheReadTokens != 13 || record.OutputTokens != 7 {
		t.Fatalf("numeric usage buckets were not preserved: %+v payload=%s", record, received)
	}
	if strings.Contains(string(received), "must-not-be-uploaded") {
		t.Fatalf("transcript content leaked into hook payload: %s", received)
	}
}

func TestCollectPreservesCanonicalGraphEvents(t *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "thread-sync.json")
	config, _ := json.Marshal(Config{Enabled: true, Endpoint: server.URL, IngestToken: "ingest"})
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(dir, "session.jsonl")
	lines := []string{
		`{"type":"user","uuid":"user-row","sessionId":"root-session","timestamp":"2026-07-13T20:00:00Z","message":{"role":"user","content":"Keep this complete visible prompt."}}`,
		`{"type":"assistant","uuid":"assistant-row","sessionId":"root-session","timestamp":"2026-07-13T20:00:01Z","message":{"id":"message-1","role":"assistant","model":"gpt-5.6-sol","content":[{"type":"thinking","thinking":"hidden chain of thought"},{"type":"text","text":"Complete visible response."},{"type":"tool_use","id":"call-read","name":"Read","input":{"file_path":"README.md","password":"must-not-upload"}}]}}`,
		`{"type":"user","uuid":"result-row","sessionId":"root-session","timestamp":"2026-07-13T20:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-read","content":"Read completed.","is_error":false}]},"toolUseResult":{"duration_ms":12}}`,
		`{"type":"system","subtype":"compact_boundary","uuid":"compact-row","sessionId":"root-session","timestamp":"2026-07-13T20:00:03Z","content":"Conversation compacted","compactMetadata":{"trigger":"manual","durationMs":25,"hiddenSummary":"must-not-upload"}}`,
	}
	if err := os.WriteFile(transcriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDEX_THREAD_SYNC_CONFIG", configPath)
	t.Setenv("CLAUDEX_THREAD_USAGE_STATE", filepath.Join(dir, "usage-state.json"))
	t.Setenv("CLAUDEX_THREAD_GRAPH_STATE", filepath.Join(dir, "graph-state.json"))
	t.Setenv("CLAUDEX_THREAD_ROLE", "supervisor")
	t.Setenv("CLAUDEX_THREAD_EFFORT", "xhigh")

	input, _ := json.Marshal(map[string]any{
		"session_id":      "root-session",
		"hook_event_name": "Stop",
		"transcript_path": transcriptPath,
	})
	delivery := Collect(context.Background(), strings.NewReader(string(input)))
	if !delivery.Sent {
		t.Fatalf("expected sent delivery, got %#v", delivery)
	}

	var payload struct {
		GraphEvents []struct {
			Type     string         `json:"type"`
			Role     string         `json:"role"`
			Content  string         `json:"content"`
			ToolName string         `json:"tool_name"`
			Status   string         `json:"status"`
			Raw      map[string]any `json:"raw"`
		} `json:"graph_events"`
	}
	if err := json.Unmarshal(received, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.GraphEvents) != 5 {
		t.Fatalf("expected five canonical graph events, got %d payload=%s", len(payload.GraphEvents), received)
	}
	if payload.GraphEvents[0].Type != "message" || payload.GraphEvents[0].Role != "user" || payload.GraphEvents[0].Content != "Keep this complete visible prompt." {
		t.Fatalf("user message missing: %#v", payload.GraphEvents[0])
	}
	if payload.GraphEvents[1].Type != "message" || payload.GraphEvents[1].Content != "Complete visible response." {
		t.Fatalf("assistant message missing: %#v", payload.GraphEvents[1])
	}
	if payload.GraphEvents[2].Type != "tool_call" || payload.GraphEvents[2].ToolName != "Read" || payload.GraphEvents[3].Type != "tool_result" || payload.GraphEvents[4].Type != "compact" {
		t.Fatalf("tool or compact events missing: %#v", payload.GraphEvents)
	}
	for _, forbidden := range []string{"hidden chain of thought", "must-not-upload"} {
		if strings.Contains(string(received), forbidden) {
			t.Fatalf("forbidden content %q leaked into graph upload: %s", forbidden, received)
		}
	}
}

func TestCollectUsageSpoolDoesNotDuplicateRecords(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "thread-sync.json")
	config, _ := json.Marshal(Config{Enabled: true, Endpoint: "http://127.0.0.1:1/hooks", IngestToken: "ingest"})
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	transcriptPath := filepath.Join(dir, "session.jsonl")
	line := `{"timestamp":"2026-07-13T19:00:00Z","sessionId":"s1","requestId":"r1","message":{"id":"m1","model":"gpt-5.6-sol","content":"must-not-be-uploaded","usage":{"input_tokens":11,"output_tokens":7,"cache_read_input_tokens":13}}}` + "\n"
	if err := os.WriteFile(transcriptPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDEX_THREAD_SYNC_CONFIG", configPath)
	t.Setenv("CLAUDEX_THREAD_USAGE_STATE", filepath.Join(dir, "usage-state.json"))

	collect := func(event string) Delivery {
		input, _ := json.Marshal(map[string]any{
			"session_id":      "s1",
			"hook_event_name": event,
			"transcript_path": transcriptPath,
		})
		return Collect(context.Background(), strings.NewReader(string(input)))
	}
	if delivery := collect("Stop"); !delivery.Spooled {
		t.Fatalf("expected first delivery to spool, got %#v", delivery)
	}
	if delivery := collect("SessionEnd"); !delivery.Spooled {
		t.Fatalf("expected second delivery to spool, got %#v", delivery)
	}

	var received [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = append(received, body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	cfg, err := loadConfigAt(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Endpoint = server.URL
	updated, _ := json.Marshal(cfg)
	if err := os.WriteFile(configPath, updated, 0o600); err != nil {
		t.Fatal(err)
	}

	sent, pending, err := Flush(context.Background())
	if err != nil || sent != 2 || pending != 0 {
		t.Fatalf("flush sent=%d pending=%d err=%v", sent, pending, err)
	}
	usageRows := 0
	for _, body := range received {
		if strings.Contains(string(body), "must-not-be-uploaded") {
			t.Fatalf("transcript content leaked into spool: %s", body)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		if rows, ok := payload["usage_records"].([]any); ok {
			usageRows += len(rows)
		}
	}
	if usageRows != 1 {
		t.Fatalf("expected one usage record across retried payloads, got %d", usageRows)
	}
}

func TestBackfillBatchesCanonicalGraphAndUsageDeterministically(t *testing.T) {
	var payloads []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ingest" {
			t.Fatalf("unexpected auth header %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "must-not-upload") {
			t.Fatalf("secret leaked into backfill payload: %s", body)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	dir := t.TempDir()
	transcript := filepath.Join(dir, "session.jsonl")
	lines := []string{
		`{"type":"user","uuid":"user-row","sessionId":"root-session","timestamp":"2026-07-13T20:00:00Z","message":{"role":"user","content":"Backfill the real thread."}}`,
		`{"type":"assistant","uuid":"assistant-row","sessionId":"root-session","timestamp":"2026-07-13T20:00:01Z","requestId":"req-1","message":{"id":"message-1","role":"assistant","model":"gpt-5.6-sol","usage":{"input_tokens":11,"output_tokens":7},"content":[{"type":"text","text":"Visible response."},{"type":"tool_use","id":"call-read","name":"Read","input":{"file_path":"README.md","password":"must-not-upload"}}]}}`,
		`{"type":"user","uuid":"result-row","sessionId":"root-session","timestamp":"2026-07-13T20:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-read","content":"Read completed.","is_error":false}]},"toolUseResult":{"duration_ms":12}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	options := BackfillOptions{
		Config:          Config{Enabled: true, Endpoint: server.URL, IngestToken: "ingest", MachineID: "test"},
		SessionID:       "root-session",
		TranscriptPath:  transcript,
		CWD:             "/workspace/x",
		Model:           "gpt-5.6-sol",
		Effort:          "xhigh",
		Role:            "supervisor",
		MaxGraphBytes:   1,
		MaxUsageRecords: 1,
	}
	first, err := Backfill(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if first.GraphEvents != 4 || first.UsageRecords != 1 || first.Batches < 2 {
		t.Fatalf("unexpected backfill result: %#v", first)
	}
	firstPayloads := append([]map[string]any(nil), payloads...)
	payloads = nil
	second, err := Backfill(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if second != first || len(payloads) != len(firstPayloads) {
		t.Fatalf("rerun changed backfill shape: first=%#v second=%#v", first, second)
	}
	for index := range payloads {
		firstCollector := firstPayloads[index]["collector"].(map[string]any)
		secondCollector := payloads[index]["collector"].(map[string]any)
		if firstCollector["event_id"] != secondCollector["event_id"] {
			t.Fatalf("batch %d event id changed: %v != %v", index, firstCollector["event_id"], secondCollector["event_id"])
		}
	}
	if firstPayloads[0]["hook_event_name"] != "UserPromptSubmit" || firstPayloads[0]["prompt"] != "Backfill the real thread." {
		t.Fatalf("first batch did not preserve title evidence: %#v", firstPayloads[0])
	}
}

func TestCollectSpoolsAndFlushes(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "thread-sync.json")
	raw, _ := json.Marshal(Config{Enabled: true, Endpoint: "http://127.0.0.1:1/hooks", IngestToken: "ingest", MachineID: "test"})
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDEX_THREAD_SYNC_CONFIG", configPath)
	delivery := Collect(context.Background(), strings.NewReader(`{"session_id":"s1","hook_event_name":"SessionStart"}`))
	if !delivery.Spooled {
		t.Fatalf("expected spool, got %#v", delivery)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	cfg, err := loadConfigAt(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Endpoint = server.URL
	updated, _ := json.Marshal(cfg)
	if err := os.WriteFile(configPath, updated, 0o600); err != nil {
		t.Fatal(err)
	}
	sent, pending, err := Flush(context.Background())
	if err != nil || sent != 1 || pending != 0 {
		t.Fatalf("flush sent=%d pending=%d err=%v", sent, pending, err)
	}
}

func TestCleanupEphemeralSecurityKeyIsTerminalAndFailClosed(t *testing.T) {
	cwd := t.TempDir()
	dir := filepath.Join(cwd, "logs", "security")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, ".security-key")
	if err := os.WriteFile(key, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupEphemeralSecurityKey(map[string]any{"hook_event_name": "PostToolUse", "cwd": cwd})
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("non-terminal hook removed key: %v", err)
	}
	cleanupEphemeralSecurityKey(map[string]any{"hook_event_name": "Stop", "cwd": cwd})
	if _, err := os.Stat(filepath.Join(cwd, "logs")); !os.IsNotExist(err) {
		t.Fatalf("terminal hook did not remove empty plumbing tree: %v", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audit.log"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupEphemeralSecurityKey(map[string]any{"hook_event_name": "SessionEnd", "cwd": cwd})
	if _, err := os.Stat(key); err != nil {
		t.Fatalf("cleanup touched directory with real logs: %v", err)
	}
}
