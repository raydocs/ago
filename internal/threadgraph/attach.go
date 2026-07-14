package threadgraph

import (
	"fmt"
	"os"
)

var graphHookEvents = map[string]struct{}{
	"Stop":        {},
	"StopFailure": {},
	"SessionEnd":  {},
}

// AttachResult describes graph events attached to one hook payload.
type AttachResult struct {
	Events     []Event
	Next       FileCursor
	Transcript string
	StatePath  string
	Attached   bool
	Detail     string
}

// AttachToPayload incrementally adds sanitized canonical events from the hook transcript.
func AttachToPayload(payload map[string]any, statePath string, context Context) AttachResult {
	if payload == nil {
		return AttachResult{Detail: "nil payload"}
	}
	if _, ok := graphHookEvents[stringField(payload, "hook_event_name")]; !ok {
		return AttachResult{Detail: "hook event does not trigger graph scan"}
	}
	transcript := stringField(payload, "transcript_path")
	if transcript == "" {
		return AttachResult{Detail: "transcript_path missing"}
	}
	if statePath == "" {
		var err error
		statePath, err = DefaultStatePath()
		if err != nil {
			return AttachResult{Transcript: transcript, Detail: err.Error()}
		}
	}
	cursor, err := loadCursor(statePath, transcript)
	if err != nil {
		return AttachResult{Transcript: transcript, StatePath: statePath, Detail: err.Error()}
	}
	result, err := ScanFile(transcript, cursor.Offset, context, 96*1024)
	if err != nil {
		return AttachResult{Transcript: transcript, StatePath: statePath, Detail: err.Error()}
	}
	info, err := os.Stat(transcript)
	if err != nil {
		return AttachResult{Transcript: transcript, StatePath: statePath, Detail: err.Error()}
	}
	next := FileCursor{Offset: result.Next, Size: info.Size(), MtimeNs: info.ModTime().UnixNano()}
	if len(result.Events) > 0 {
		payload["graph_events"] = result.Events
	}
	payload["graph_scan"] = map[string]any{
		"events":        len(result.Events),
		"scanned_bytes": result.Scanned,
		"partial_line":  result.PartialEnd,
	}
	return AttachResult{
		Events:     result.Events,
		Next:       next,
		Transcript: transcript,
		StatePath:  statePath,
		Attached:   true,
		Detail:     fmt.Sprintf("%d graph events", len(result.Events)),
	}
}

// CommitCursor advances the graph cursor after a successful send or durable spool.
func CommitCursor(result AttachResult) error {
	if !result.Attached || result.Transcript == "" || result.StatePath == "" {
		return nil
	}
	return storeCursor(result.StatePath, result.Transcript, result.Next)
}
