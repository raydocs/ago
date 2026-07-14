package threadsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"claudexflow/internal/threadgraph"
	"claudexflow/internal/threadusage"
)

const (
	defaultBackfillGraphBytes   = 64 * 1024
	defaultBackfillUsageRecords = 75
)

// BackfillOptions describes one deterministic transcript replay into Thread storage.
type BackfillOptions struct {
	Config          Config
	SessionID       string
	RootSessionID   string
	ParentSessionID string
	TranscriptPath  string
	CWD             string
	Model           string
	Effort          string
	Role            string
	MaxGraphBytes   int
	MaxUsageRecords int
}

// BackfillResult reports counts only; transcript content never reaches stdout.
type BackfillResult struct {
	Batches      int `json:"batches"`
	GraphEvents  int `json:"graph_events"`
	UsageRecords int `json:"usage_records"`
}

// Backfill replays one complete transcript in bounded, idempotent upload batches.
// It does not read or mutate the live hook cursor files.
func Backfill(ctx context.Context, options BackfillOptions) (BackfillResult, error) {
	cfg := options.Config
	if !cfg.Enabled || strings.TrimSpace(cfg.Endpoint) == "" || strings.TrimSpace(cfg.IngestToken) == "" {
		return BackfillResult{}, fmt.Errorf("enabled Thread sync config with endpoint and ingest token is required")
	}
	sessionID := strings.TrimSpace(options.SessionID)
	transcriptPath := strings.TrimSpace(options.TranscriptPath)
	if sessionID == "" || transcriptPath == "" {
		return BackfillResult{}, fmt.Errorf("session ID and transcript path are required")
	}
	info, err := os.Stat(transcriptPath)
	if err != nil {
		return BackfillResult{}, err
	}
	rootSessionID := strings.TrimSpace(options.RootSessionID)
	if rootSessionID == "" {
		rootSessionID = sessionID
	}
	role := strings.TrimSpace(options.Role)
	if role == "" {
		role = "supervisor"
	}
	graphBudget := options.MaxGraphBytes
	if graphBudget <= 0 {
		graphBudget = defaultBackfillGraphBytes
	}
	usageLimit := options.MaxUsageRecords
	if usageLimit <= 0 {
		usageLimit = defaultBackfillUsageRecords
	}

	var (
		result      BackfillResult
		graphOffset int64
		usageCursor threadusage.FileCursor
		titleSent   bool
	)
	for graphOffset < info.Size() || usageCursor.Offset < info.Size() {
		graphStart := graphOffset
		usageStart := usageCursor.Offset
		graphScan := threadgraph.ScanResult{Next: graphOffset}
		if graphOffset < info.Size() {
			graphScan, err = threadgraph.ScanFile(transcriptPath, graphOffset, threadgraph.Context{
				SessionID:       sessionID,
				RootSessionID:   rootSessionID,
				ParentSessionID: strings.TrimSpace(options.ParentSessionID),
				Role:            role,
				Effort:          strings.TrimSpace(options.Effort),
			}, graphBudget)
			if err != nil {
				return result, err
			}
			graphOffset = graphScan.Next
		}

		usageScan := threadusage.ScanResult{Next: usageCursor}
		if usageCursor.Offset < info.Size() {
			usageScan, err = threadusage.ScanFileLimited(transcriptPath, usageCursor, usageLimit)
			if err != nil {
				return result, err
			}
			usageCursor = usageScan.Next
		}

		if graphOffset == graphStart && usageCursor.Offset == usageStart {
			return result, fmt.Errorf("transcript backfill made no progress; the file may end with a partial JSONL record")
		}
		if len(graphScan.Events) == 0 && len(usageScan.Records) == 0 {
			continue
		}

		hookEvent := "Stop"
		prompt := ""
		if !titleSent {
			for _, event := range graphScan.Events {
				if event.Type == "message" && event.Role == "user" && strings.TrimSpace(event.Content) != "" {
					hookEvent = "UserPromptSubmit"
					prompt = event.Content
					titleSent = true
					break
				}
			}
		}
		observedAt := firstBackfillTimestamp(graphScan.Events, usageScan.Records)
		model := strings.TrimSpace(options.Model)
		if model == "" {
			model = firstBackfillModel(graphScan.Events, usageScan.Records)
		}
		payload := map[string]any{
			"session_id":      sessionID,
			"root_session_id": rootSessionID,
			"hook_event_name": hookEvent,
			"cwd":             options.CWD,
			"collector": map[string]any{
				"event_id":    backfillEventID(sessionID, graphStart, graphOffset, usageStart, usageCursor.Offset),
				"observed_at": observedAt,
				"machine_id":  cfg.MachineID,
				"model":       model,
				"effort":      strings.TrimSpace(options.Effort),
				"role":        role,
				"version":     1,
			},
			"graph_events":  graphScan.Events,
			"usage_records": threadusage.CompactRecords(usageScan.Records),
		}
		if parent := strings.TrimSpace(options.ParentSessionID); parent != "" {
			payload["parent_session_id"] = parent
		}
		if prompt != "" {
			payload["prompt"] = prompt
		}
		encoded, err := boundedJSON(payload, cfg.maxPayload())
		if err != nil {
			return result, fmt.Errorf("encode backfill batch: %w", err)
		}
		if err := send(ctx, cfg, encoded); err != nil {
			return result, fmt.Errorf("send backfill batch %d: %w", result.Batches+1, err)
		}
		result.Batches++
		result.GraphEvents += len(graphScan.Events)
		result.UsageRecords += len(usageScan.Records)
	}
	return result, nil
}

func firstBackfillTimestamp(events []threadgraph.Event, records []threadusage.Record) string {
	for _, event := range events {
		if strings.TrimSpace(event.StartedAt) != "" {
			return event.StartedAt
		}
	}
	for _, record := range records {
		if strings.TrimSpace(record.ObservedAt) != "" {
			return record.ObservedAt
		}
	}
	return "1970-01-01T00:00:00Z"
}

func firstBackfillModel(events []threadgraph.Event, records []threadusage.Record) string {
	for _, event := range events {
		if strings.TrimSpace(event.Model) != "" {
			return event.Model
		}
	}
	for _, record := range records {
		if strings.TrimSpace(record.Model) != "" {
			return record.Model
		}
	}
	return ""
}

func backfillEventID(sessionID string, graphStart, graphEnd, usageStart, usageEnd int64) string {
	value := fmt.Sprintf("%s|%d|%d|%d|%d", sessionID, graphStart, graphEnd, usageStart, usageEnd)
	sum := sha256.Sum256([]byte(value))
	return "backfill-" + hex.EncodeToString(sum[:12])
}
