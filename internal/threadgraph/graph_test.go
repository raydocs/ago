package threadgraph

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseVisibleMessages(t *testing.T) {
	parser := NewParser(Context{
		SessionID:     "root-session",
		RootSessionID: "root-session",
		Role:          "supervisor",
		Effort:        "xhigh",
	})

	userLine := transcriptLine(t, map[string]any{
		"type":      "user",
		"uuid":      "user-row",
		"sessionId": "root-session",
		"timestamp": "2026-07-13T20:00:00Z",
		"isMeta":    false,
		"message": map[string]any{
			"role":    "user",
			"content": "Please preserve the complete visible prompt.",
		},
	})
	assistantLine := transcriptLine(t, map[string]any{
		"type":      "assistant",
		"uuid":      "assistant-row",
		"sessionId": "root-session",
		"timestamp": "2026-07-13T20:00:02Z",
		"message": map[string]any{
			"id":    "message-1",
			"role":  "assistant",
			"model": "gpt-5.6-sol",
			"content": []any{
				map[string]any{"type": "thinking", "thinking": "hidden reasoning"},
				map[string]any{"type": "text", "text": "This is the complete visible response."},
			},
		},
	})

	events := append(parser.ParseLine(userLine), parser.ParseLine(assistantLine)...)
	if len(events) != 2 {
		t.Fatalf("expected two visible message events, got %d: %#v", len(events), events)
	}
	if events[0].Type != "message" || events[0].Role != "user" || events[0].Content != "Please preserve the complete visible prompt." {
		t.Fatalf("unexpected user event: %#v", events[0])
	}
	if events[1].Type != "message" || events[1].Role != "assistant" || events[1].Model != "gpt-5.6-sol" || events[1].Content != "This is the complete visible response." {
		t.Fatalf("unexpected assistant event: %#v", events[1])
	}
	if encoded, _ := json.Marshal(events); string(encoded) == "" || contains(string(encoded), "hidden reasoning") {
		t.Fatalf("hidden reasoning entered the canonical graph: %s", encoded)
	}
}

func TestParseToolCallAndResultMetadata(t *testing.T) {
	parser := NewParser(Context{SessionID: "root-session", RootSessionID: "root-session", Role: "supervisor", Effort: "xhigh"})

	callLine := transcriptLine(t, map[string]any{
		"type":      "assistant",
		"uuid":      "agent-call-row",
		"sessionId": "root-session",
		"timestamp": "2026-07-13T20:01:00Z",
		"message": map[string]any{
			"role":  "assistant",
			"model": "gpt-5.6-sol",
			"content": []any{map[string]any{
				"type": "tool_use",
				"id":   "call-agent",
				"name": "Agent",
				"input": map[string]any{
					"subagent_type": "Explore",
					"description":   "Inspect the repository",
					"prompt":        "Find the relevant code paths.",
				},
			}},
		},
	})
	resultLine := transcriptLine(t, map[string]any{
		"type":      "user",
		"uuid":      "agent-result-row",
		"sessionId": "root-session",
		"timestamp": "2026-07-13T20:01:03Z",
		"message": map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": "call-agent",
				"content":     "Repository inspection completed.",
			}},
		},
		"toolUseResult": map[string]any{
			"status":        "completed",
			"agentId":       "agent-1",
			"resolvedModel": "claude-opus-4-8[1m]",
			"duration_ms":   3200,
			"usage": map[string]any{
				"input_tokens":  100,
				"output_tokens": 20,
			},
		},
	})

	callEvents := parser.ParseLine(callLine)
	resultEvents := parser.ParseLine(resultLine)
	if len(callEvents) != 1 || callEvents[0].Type != "tool_call" || callEvents[0].ToolUseID != "call-agent" || callEvents[0].ToolName != "Agent" {
		t.Fatalf("unexpected tool call events: %#v", callEvents)
	}
	if len(resultEvents) != 1 {
		t.Fatalf("expected one tool result event, got %#v", resultEvents)
	}
	result := resultEvents[0]
	if result.Type != "tool_result" || result.ParentEventID != "call-agent" || result.WorkerID != "agent-1" || result.Model != "claude-opus-4-8[1m]" || result.Status != "completed" {
		t.Fatalf("unexpected tool result metadata: %#v", result)
	}
	if result.DurationMS == nil || *result.DurationMS != 3200 {
		t.Fatalf("unexpected duration: %#v", result.DurationMS)
	}
}

func TestParseFailedWorkerResult(t *testing.T) {
	parser := NewParser(Context{SessionID: "root-session", RootSessionID: "root-session", Role: "supervisor"})
	line := transcriptLine(t, map[string]any{
		"type":      "user",
		"uuid":      "failed-worker-row",
		"sessionId": "root-session",
		"timestamp": "2026-07-13T20:02:00Z",
		"message": map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": "call-worker",
				"content":     "Worker launch reached max turns.",
				"is_error":    true,
			}},
		},
	})

	events := parser.ParseLine(line)
	if len(events) != 1 || events[0].Type != "tool_result" || events[0].Status != "failed" || events[0].ParentEventID != "call-worker" {
		t.Fatalf("failed Worker result was not preserved: %#v", events)
	}
}

func TestParseRedactsCanonicalContent(t *testing.T) {
	parser := NewParser(Context{SessionID: "root-session", RootSessionID: "root-session"})
	line := transcriptLine(t, map[string]any{
		"type":      "assistant",
		"uuid":      "secret-tool-row",
		"sessionId": "root-session",
		"timestamp": "2026-07-13T20:03:00Z",
		"message": map[string]any{
			"role":  "assistant",
			"model": "gpt-5.6-sol",
			"content": []any{map[string]any{
				"type": "tool_use",
				"id":   "call-secret",
				"name": "Bash",
				"input": map[string]any{
					"command":        "curl -H 'Authorization: Bearer secret-token-value' https://example.test",
					"password":       "password-value",
					"safe_parameter": "visible",
				},
			}},
		},
	})

	events := parser.ParseLine(line)
	encoded, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-token-value", "password-value"} {
		if contains(string(encoded), secret) {
			t.Fatalf("secret %q entered canonical event: %s", secret, encoded)
		}
	}
	if !contains(string(encoded), "visible") || !contains(string(encoded), "[REDACTED]") {
		t.Fatalf("safe fields or redaction marker missing: %s", encoded)
	}
}

func TestParseCompactBoundary(t *testing.T) {
	parser := NewParser(Context{SessionID: "root-session", RootSessionID: "root-session", Role: "supervisor"})
	line := transcriptLine(t, map[string]any{
		"type":      "system",
		"subtype":   "compact_boundary",
		"uuid":      "compact-row",
		"sessionId": "root-session",
		"timestamp": "2026-07-13T20:04:00Z",
		"content":   "Conversation compacted",
		"compactMetadata": map[string]any{
			"trigger":                 "manual",
			"preTokens":               179623,
			"postTokens":              19925,
			"cumulativeDroppedTokens": 159698,
			"durationMs":              170888,
			"hiddenSummary":           "must not be exported",
		},
	})

	events := parser.ParseLine(line)
	if len(events) != 1 || events[0].Type != "compact" || events[0].DurationMS == nil || *events[0].DurationMS != 170888 {
		t.Fatalf("compact boundary not normalized: %#v", events)
	}
	encoded, _ := json.Marshal(events)
	if contains(string(encoded), "must not be exported") {
		t.Fatalf("compact hidden content entered graph: %s", encoded)
	}
}

func TestScanFilePreservesPartialLineAndStableOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	first := transcriptLine(t, map[string]any{
		"type": "user", "uuid": "row-1", "sessionId": "root-session", "timestamp": "2026-07-13T20:05:00Z",
		"message": map[string]any{"role": "user", "content": "First prompt"},
	})
	second := transcriptLine(t, map[string]any{
		"type": "assistant", "uuid": "row-2", "sessionId": "root-session", "timestamp": "2026-07-13T20:05:01Z",
		"message": map[string]any{"role": "assistant", "model": "gpt-5.6-sol", "content": []any{map[string]any{
			"type": "tool_use", "id": "call-read", "name": "Read", "input": map[string]any{"file_path": "README.md"},
		}}},
	})
	third := transcriptLine(t, map[string]any{
		"type": "assistant", "uuid": "row-3", "sessionId": "root-session", "timestamp": "2026-07-13T20:05:02Z",
		"message": map[string]any{"role": "assistant", "model": "gpt-5.6-sol", "content": []any{map[string]any{
			"type": "text", "text": "Finished reading.",
		}}},
	})
	partial := third[:len(third)/2]
	if err := os.WriteFile(path, append(append(append([]byte{}, first...), '\n'), append(second, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(partial); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	context := Context{SessionID: "root-session", RootSessionID: "root-session"}
	firstScan, err := ScanFile(path, 0, context, 128*1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(firstScan.Events) != 2 || !firstScan.PartialEnd || firstScan.Next <= 0 {
		t.Fatalf("unexpected first scan: %#v", firstScan)
	}
	if firstScan.Events[0].EventID != "row-1:message:0" || firstScan.Events[1].EventID != "call-read" {
		t.Fatalf("events were not in stable transcript order: %#v", firstScan.Events)
	}

	if err := os.Truncate(path, firstScan.Next); err != nil {
		t.Fatal(err)
	}
	file, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(third, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	secondScan, err := ScanFile(path, firstScan.Next, context, 128*1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondScan.Events) != 1 || secondScan.Events[0].EventID != "row-3:message:0" || secondScan.PartialEnd {
		t.Fatalf("unexpected second scan: %#v", secondScan)
	}
}

func transcriptLine(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func contains(value, needle string) bool {
	for index := 0; index+len(needle) <= len(value); index++ {
		if value[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
