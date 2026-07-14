package threadusage

import (
	"fmt"
	"strings"
)

// UsageHookEvents are the Claude hook events that trigger an incremental usage scan.
var UsageHookEvents = map[string]struct{}{
	"Stop":         {},
	"StopFailure":  {},
	"SessionEnd":   {},
}

// ShouldAttach reports whether the hook event should carry usage deltas.
func ShouldAttach(hookEvent string) bool {
	_, ok := UsageHookEvents[hookEvent]
	return ok
}

// AttachResult describes usage attached to a hook payload.
type AttachResult struct {
	Records      []Record
	Next         FileCursor
	Transcript   string
	StatePath    string
	Attached     bool
	Detail       string
}

// AttachToPayload scans the transcript referenced by the hook and injects compact usage_records.
// No prompt/content is read into the upload payload beyond numeric usage fields.
// Cursor is returned but not persisted; call CommitCursor after successful send/spool.
func AttachToPayload(payload map[string]any, statePath string) AttachResult {
	if payload == nil {
		return AttachResult{Detail: "nil payload"}
	}
	event := stringField(payload, "hook_event_name")
	if !ShouldAttach(event) {
		return AttachResult{Detail: "hook event does not trigger usage scan"}
	}
	transcript := stringField(payload, "transcript_path")
	if transcript == "" {
		return AttachResult{Detail: "transcript_path missing"}
	}
	if statePath == "" {
		var err error
		statePath, err = DefaultStatePath()
		if err != nil {
			return AttachResult{Detail: err.Error()}
		}
	}

	result, err := ScanTranscript(statePath, transcript)
	if err != nil {
		return AttachResult{Transcript: transcript, StatePath: statePath, Detail: err.Error()}
	}

	// Prefer session_id from the hook payload when records omit it.
	sessionID := stringField(payload, "session_id")
	for i := range result.Records {
		if result.Records[i].SessionID == "" {
			result.Records[i].SessionID = sessionID
			// Recompute usage id once session is filled.
			result.Records[i].UsageID = deterministicUsageID(
				result.Records[i].SessionID,
				result.Records[i].MessageID,
				result.Records[i].RequestID,
				result.Records[i].Model,
				result.Records[i].ObservedAt,
				result.Records[i].InputTokens,
				result.Records[i].CacheWrite5mTokens,
				result.Records[i].CacheWrite1hTokens,
				result.Records[i].CacheReadTokens,
				result.Records[i].OutputTokens,
				result.Records[i].IsFast,
			)
		}
	}

	if len(result.Records) > 0 {
		payload["usage_records"] = CompactRecords(result.Records)
	}
	payload["usage_scan"] = map[string]any{
		"records":      len(result.Records),
		"scanned_bytes": result.Scanned,
		"partial_line": result.PartialEnd,
	}

	return AttachResult{
		Records:    result.Records,
		Next:       result.Next,
		Transcript: transcript,
		StatePath:  statePath,
		Attached:   true,
		Detail:     fmt.Sprintf("%d usage records", len(result.Records)),
	}
}

// CommitCursor persists the next file cursor after a successful send or spool.
func CommitCursor(result AttachResult) error {
	if !result.Attached || result.Transcript == "" || result.StatePath == "" {
		return nil
	}
	return StoreCursor(result.StatePath, result.Transcript, result.Next)
}

// ContainsContentLeak is a test helper that scans compact upload JSON for forbidden content keys.
func ContainsContentLeak(encoded []byte) bool {
	lower := strings.ToLower(string(encoded))
	for _, needle := range []string{
		`"content"`,
		`"prompt"`,
		`"tool_input"`,
		`"tool_response"`,
		`"last_assistant_message"`,
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func stringField(payload map[string]any, key string) string {
	v, _ := payload[key].(string)
	return strings.TrimSpace(v)
}
