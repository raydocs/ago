package threadgraph

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var (
	sensitiveKey = regexp.MustCompile(`(?i)(api[_-]?key|authorization|cookie|credential|password|secret|access[_-]?token|refresh[_-]?token|auth[_-]?token|^token$|bearer)`)
	bearerValue  = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	openAIKey    = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`)
	dottedKey    = regexp.MustCompile(`\b[0-9a-fA-F]{24,}\.[A-Za-z0-9_-]{12,}\b`)
	assignedKey  = regexp.MustCompile(`(?i)((?:api[_-]?key|auth[_-]?token|access[_-]?token|refresh[_-]?token|token|secret|password)\s*[:=]\s*)[^\s,;"']{8,}`)
)

// Context identifies the thread hierarchy for normalized transcript events.
type Context struct {
	SessionID       string
	RootSessionID   string
	ParentSessionID string
	Role            string
	Effort          string
}

// Event is the compact, sanitized event shape uploaded to the canonical Thread graph.
type Event struct {
	EventID         string         `json:"event_id"`
	SessionID       string         `json:"session_id"`
	RootSessionID   string         `json:"root_session_id"`
	ParentSessionID string         `json:"parent_session_id,omitempty"`
	ParentEventID   string         `json:"parent_event_id,omitempty"`
	WorkerID        string         `json:"worker_id,omitempty"`
	Type            string         `json:"type"`
	Role            string         `json:"role"`
	Model           string         `json:"model,omitempty"`
	Effort          string         `json:"effort,omitempty"`
	Status          string         `json:"status"`
	StartedAt       string         `json:"started_at"`
	EndedAt         string         `json:"ended_at,omitempty"`
	DurationMS      *int64         `json:"duration_ms,omitempty"`
	Summary         string         `json:"summary"`
	Content         string         `json:"content,omitempty"`
	ToolName        string         `json:"tool_name,omitempty"`
	ToolUseID       string         `json:"tool_use_id,omitempty"`
	Raw             map[string]any `json:"raw"`
}

// Parser converts Claude transcript JSONL records into canonical events.
type Parser struct {
	context Context
}

func NewParser(context Context) *Parser {
	return &Parser{context: context}
}

func (p *Parser) ParseLine(line []byte) []Event {
	var row map[string]any
	if json.Unmarshal(line, &row) != nil {
		return nil
	}
	message, _ := row["message"].(map[string]any)
	role := stringField(message, "role")
	timestamp := stringField(row, "timestamp")
	rowID := stringField(row, "uuid")
	sessionID := firstNonEmpty(stringField(row, "sessionId"), stringField(row, "session_id"), p.context.SessionID)
	base := Event{
		SessionID:       sessionID,
		RootSessionID:   firstNonEmpty(p.context.RootSessionID, sessionID),
		ParentSessionID: p.context.ParentSessionID,
		Effort:          p.context.Effort,
		Status:          "completed",
		StartedAt:       timestamp,
	}

	switch stringField(row, "type") {
	case "user":
		if role != "user" {
			return nil
		}
		if blocks, ok := message["content"].([]any); ok {
			return p.parseToolResults(base, row, rowID, blocks)
		}
		if boolField(row, "isMeta") {
			return nil
		}
		content, ok := message["content"].(string)
		if !ok || strings.TrimSpace(content) == "" {
			return nil
		}
		content = sanitizeString(content, 32*1024)
		event := base
		event.EventID = stableSourceID(rowID, "message", 0)
		event.Type = "message"
		event.Role = "user"
		event.Content = content
		event.Summary = compact(content, 180)
		event.Raw = map[string]any{"source_uuid": rowID}
		return []Event{event}
	case "assistant":
		if role != "assistant" {
			return nil
		}
		blocks, _ := message["content"].([]any)
		model := stringField(message, "model")
		messageID := stringField(message, "id")
		var events []Event
		for index, value := range blocks {
			block, _ := value.(map[string]any)
			switch stringField(block, "type") {
			case "text":
				content := stringField(block, "text")
				if strings.TrimSpace(content) == "" {
					continue
				}
				content = sanitizeString(content, 32*1024)
				event := base
				event.EventID = stableSourceID(rowID, "message", index)
				event.Type = "message"
				event.Role = "assistant"
				event.Model = model
				event.Content = content
				event.Summary = compact(content, 180)
				event.Raw = map[string]any{"source_uuid": rowID, "message_id": messageID}
				events = append(events, event)
			case "tool_use":
				toolUseID := stringField(block, "id")
				toolName := stringField(block, "name")
				if toolUseID == "" || toolName == "" {
					continue
				}
				input, _ := block["input"].(map[string]any)
				input = sanitizeMap(input, 16*1024)
				event := base
				event.EventID = toolUseID
				event.Type = "tool_call"
				event.Role = "assistant"
				event.Model = model
				event.Status = "running"
				event.ToolName = toolName
				event.ToolUseID = toolUseID
				event.Summary = toolSummary(toolName, input)
				event.Raw = map[string]any{"source_uuid": rowID, "message_id": messageID, "input": input}
				events = append(events, event)
			}
		}
		return events
	case "system":
		if stringField(row, "subtype") != "compact_boundary" {
			return nil
		}
		metadata, _ := row["compactMetadata"].(map[string]any)
		event := base
		event.EventID = stableSourceID(rowID, "compact", 0)
		event.Type = "compact"
		event.Role = "system"
		event.Summary = "Conversation compacted"
		if trigger := stringField(metadata, "trigger"); trigger != "" {
			event.Summary += " · " + trigger
		}
		if duration, ok := integerField(metadata, "durationMs", "duration_ms"); ok {
			event.DurationMS = &duration
		}
		event.Raw = map[string]any{
			"source_uuid":               rowID,
			"trigger":                   stringField(metadata, "trigger"),
			"pre_tokens":                numberValue(metadata["preTokens"]),
			"post_tokens":               numberValue(metadata["postTokens"]),
			"cumulative_dropped_tokens": numberValue(metadata["cumulativeDroppedTokens"]),
		}
		return []Event{event}
	default:
		return nil
	}
}

func (p *Parser) parseToolResults(base Event, row map[string]any, rowID string, blocks []any) []Event {
	metadata, _ := row["toolUseResult"].(map[string]any)
	var events []Event
	for index, value := range blocks {
		block, _ := value.(map[string]any)
		if stringField(block, "type") != "tool_result" {
			continue
		}
		toolUseID := stringField(block, "tool_use_id")
		if toolUseID == "" {
			continue
		}
		content := sanitizeString(visibleContent(block["content"]), 16*1024)
		status := firstNonEmpty(stringField(metadata, "status"), "completed")
		if boolField(block, "is_error") || status == "error" {
			status = "failed"
		}
		event := base
		event.EventID = stableSourceID(rowID, "tool_result", index)
		event.ParentEventID = toolUseID
		event.Type = "tool_result"
		event.Role = "system"
		event.Model = firstNonEmpty(stringField(metadata, "resolvedModel"), stringField(metadata, "resolved_model"), stringField(metadata, "model"))
		event.Effort = firstNonEmpty(stringField(metadata, "effort"), p.context.Effort)
		event.Status = status
		event.Content = content
		event.Summary = compact(firstNonEmpty(content, "Tool completed"), 180)
		event.ToolUseID = toolUseID
		event.WorkerID = firstNonEmpty(stringField(metadata, "worker_id"), stringField(metadata, "agentId"), stringField(metadata, "agent_id"))
		if duration, ok := integerField(metadata, "duration_ms", "durationMs"); ok {
			event.DurationMS = &duration
		}
		event.Raw = map[string]any{"source_uuid": rowID, "result": sanitizeMap(metadata, 16*1024)}
		events = append(events, event)
	}
	return events
}

func visibleContent(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var parts []string
		for _, value := range typed {
			block, _ := value.(map[string]any)
			if stringField(block, "type") == "text" {
				if text := stringField(block, "text"); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func toolSummary(name string, input map[string]any) string {
	for _, key := range []string{"description", "objective", "question", "instruction", "command", "file_path"} {
		if value := stringField(input, key); value != "" {
			return compact(name+" · "+value, 180)
		}
	}
	return name
}

func integerField(value map[string]any, keys ...string) (int64, bool) {
	for _, key := range keys {
		switch number := value[key].(type) {
		case float64:
			return int64(number), true
		case int64:
			return number, true
		case int:
			return int64(number), true
		}
	}
	return 0, false
}

func sanitizeMap(input map[string]any, maxString int) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = sanitizeValue(key, value, maxString)
	}
	return out
}

func sanitizeValue(key string, value any, maxString int) any {
	if sensitiveKey.MatchString(key) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]any:
		return sanitizeMap(typed, maxString)
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = sanitizeValue("", item, maxString)
		}
		return out
	case string:
		return sanitizeString(typed, maxString)
	default:
		return value
	}
}

func sanitizeString(value string, max int) string {
	value = bearerValue.ReplaceAllString(value, `${1}[REDACTED]`)
	value = openAIKey.ReplaceAllString(value, "[REDACTED]")
	value = dottedKey.ReplaceAllString(value, "[REDACTED]")
	value = assignedKey.ReplaceAllString(value, `${1}[REDACTED]`)
	if len(value) > max {
		return value[:max] + "…[truncated]"
	}
	return value
}

func numberValue(value any) int64 {
	switch number := value.(type) {
	case float64:
		return int64(number)
	case int64:
		return number
	case int:
		return int64(number)
	default:
		return 0
	}
}

func stableSourceID(source, kind string, index int) string {
	return fmt.Sprintf("%s:%s:%d", source, kind, index)
}

func compact(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) <= max {
		return value
	}
	return value[:max-1] + "…"
}

func stringField(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return strings.TrimSpace(result)
}

func boolField(value map[string]any, key string) bool {
	result, _ := value[key].(bool)
	return result
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
