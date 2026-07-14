package threadusage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Record is one normalized numeric usage row extracted from a Claude transcript JSONL line.
// It never carries prompt, assistant content, or tool payloads.
type Record struct {
	UsageID            string   `json:"usage_id"`
	SessionID          string   `json:"session_id"`
	MessageID          string   `json:"message_id,omitempty"`
	RequestID          string   `json:"request_id,omitempty"`
	ObservedAt         string   `json:"observed_at"`
	Model              string   `json:"model"`
	InputTokens        int64    `json:"input_tokens"`
	CacheWrite5mTokens int64    `json:"cache_write_5m_tokens"`
	CacheWrite1hTokens int64    `json:"cache_write_1h_tokens"`
	CacheReadTokens    int64    `json:"cache_read_tokens"`
	OutputTokens       int64    `json:"output_tokens"`
	IsFast             bool     `json:"is_fast"`
	CarriedCostUSD     *float64 `json:"carried_cost_usd,omitempty"`

	// Internal-only fields used for local dedup; omitted from upload encoding.
	isSidechain bool
	hasSpeed    bool
}

// TotalTokens returns the normalized token total across all buckets.
func (r Record) TotalTokens() int64 {
	return r.InputTokens + r.CacheWrite5mTokens + r.CacheWrite1hTokens + r.CacheReadTokens + r.OutputTokens
}

// Compact returns the upload-safe map representation (numeric + identity only).
func (r Record) Compact() map[string]any {
	out := map[string]any{
		"usage_id":              r.UsageID,
		"session_id":            r.SessionID,
		"message_id":            r.MessageID,
		"request_id":            r.RequestID,
		"observed_at":           r.ObservedAt,
		"model":                 r.Model,
		"input_tokens":          r.InputTokens,
		"cache_write_5m_tokens": r.CacheWrite5mTokens,
		"cache_write_1h_tokens": r.CacheWrite1hTokens,
		"cache_read_tokens":     r.CacheReadTokens,
		"output_tokens":         r.OutputTokens,
		"is_fast":               r.IsFast,
	}
	if r.CarriedCostUSD != nil {
		out["carried_cost_usd"] = *r.CarriedCostUSD
	}
	return out
}

// CompactRecords converts records into upload-safe maps.
func CompactRecords(records []Record) []map[string]any {
	out := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		out = append(out, rec.Compact())
	}
	return out
}

func deterministicUsageID(sessionID, messageID, requestID, model, observedAt string, input, cache5m, cache1h, cacheRead, output int64, isFast bool) string {
	payload := strings.Join([]string{
		sessionID,
		messageID,
		requestID,
		model,
		observedAt,
		fmt.Sprintf("%d", input),
		fmt.Sprintf("%d", cache5m),
		fmt.Sprintf("%d", cache1h),
		fmt.Sprintf("%d", cacheRead),
		fmt.Sprintf("%d", output),
		fmt.Sprintf("%t", isFast),
	}, "|")
	sum := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(sum[:])
}

// EncodeUploadJSON marshals compact records and asserts no content keys are present.
func EncodeUploadJSON(records []Record) ([]byte, error) {
	return json.Marshal(CompactRecords(records))
}
