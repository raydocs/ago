package threadusage

import (
	"bytes"
	"encoding/json"
	"strings"
)

// ParseLine extracts a usage record from one Claude transcript JSONL line.
// Lines without numeric usage, malformed JSON, or empty identity fields are skipped.
func ParseLine(line []byte) (Record, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return Record{}, false
	}
	// Fast reject: only assistant usage lines carry this marker.
	if !bytes.Contains(line, []byte(`"usage"`)) {
		return Record{}, false
	}

	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return Record{}, false
	}

	message, _ := raw["message"].(map[string]any)
	if message == nil {
		return Record{}, false
	}
	usage, _ := message["usage"].(map[string]any)
	if usage == nil {
		return Record{}, false
	}

	input, okInput := asInt64(usage["input_tokens"])
	output, okOutput := asInt64(usage["output_tokens"])
	if !okInput || !okOutput {
		return Record{}, false
	}

	// Unknown speed values indicate an unsupported log shape.
	speed, hasSpeed := usage["speed"].(string)
	if hasSpeed && speed != "fast" && speed != "standard" {
		return Record{}, false
	}

	messageID := asString(message["id"])
	requestID := firstString(raw, "requestId", "request_id")
	sessionID := firstString(raw, "sessionId", "session_id")
	model := asString(message["model"])
	if model == "<synthetic>" {
		model = ""
	}
	// Present-but-empty identity fields are invalid.
	if messageID == "" && hasKey(message, "id") {
		return Record{}, false
	}
	if requestID == "" && (hasKey(raw, "requestId") || hasKey(raw, "request_id")) {
		return Record{}, false
	}
	if model == "" && hasKey(message, "model") {
		return Record{}, false
	}
	if sessionID == "" && (hasKey(raw, "sessionId") || hasKey(raw, "session_id")) {
		return Record{}, false
	}

	timestamp := firstString(raw, "timestamp", "observed_at")
	if timestamp == "" {
		return Record{}, false
	}

	cacheWrite5m, cacheWrite1h := normalizeCacheWrite(usage)
	cacheRead, _ := asInt64(usage["cache_read_input_tokens"])
	isSidechain, _ := raw["isSidechain"].(bool)

	var cost *float64
	if v, ok := asFloat64(raw["costUSD"]); ok {
		cost = &v
	}

	rec := Record{
		SessionID:          sessionID,
		MessageID:          messageID,
		RequestID:          requestID,
		ObservedAt:         timestamp,
		Model:              model,
		InputTokens:        input,
		CacheWrite5mTokens: cacheWrite5m,
		CacheWrite1hTokens: cacheWrite1h,
		CacheReadTokens:    cacheRead,
		OutputTokens:       output,
		IsFast:             speed == "fast",
		CarriedCostUSD:     cost,
		isSidechain:        isSidechain,
		hasSpeed:           hasSpeed,
	}
	rec.UsageID = deterministicUsageID(
		rec.SessionID,
		rec.MessageID,
		rec.RequestID,
		rec.Model,
		rec.ObservedAt,
		rec.InputTokens,
		rec.CacheWrite5mTokens,
		rec.CacheWrite1hTokens,
		rec.CacheReadTokens,
		rec.OutputTokens,
		rec.IsFast,
	)
	return rec, true
}

func normalizeCacheWrite(usage map[string]any) (cache5m, cache1h int64) {
	if creation, ok := usage["cache_creation"].(map[string]any); ok {
		cache5m, _ = asInt64(creation["ephemeral_5m_input_tokens"])
		cache1h, _ = asInt64(creation["ephemeral_1h_input_tokens"])
		return cache5m, cache1h
	}
	// Legacy aggregate field is treated as all 5m writes.
	cache5m, _ = asInt64(usage["cache_creation_input_tokens"])
	return cache5m, 0
}

func asString(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := asString(m[key]); s != "" {
			return s
		}
	}
	return ""
}

func hasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}

func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

func asFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}
