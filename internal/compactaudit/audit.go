package compactaudit

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type Boundary struct {
	Timestamp               string  `json:"timestamp,omitempty"`
	Trigger                 string  `json:"trigger"`
	PreTokens               int64   `json:"pre_tokens"`
	PostTokens              int64   `json:"post_tokens"`
	DroppedTokens           int64   `json:"dropped_tokens"`
	CumulativeDroppedTokens int64   `json:"cumulative_dropped_tokens"`
	DurationMS              int64   `json:"duration_ms"`
	RetainedPercent         float64 `json:"retained_percent"`
	NearestPrecedingModel   string  `json:"nearest_preceding_model,omitempty"`
	ModelEvidence           string  `json:"model_evidence"`
}

type Report struct {
	Transcript        string     `json:"transcript"`
	SessionID         string     `json:"session_id,omitempty"`
	Boundaries        []Boundary `json:"boundaries"`
	ManualCount       int        `json:"manual_count"`
	AutomaticCount    int        `json:"automatic_count"`
	AverageDurationMS int64      `json:"average_duration_ms"`
	AveragePreTokens  int64      `json:"average_pre_tokens"`
	AveragePostTokens int64      `json:"average_post_tokens"`
	RuntimeFinding    string     `json:"runtime_finding"`
}

func Parse(path string) (Report, error) {
	file, err := os.Open(path)
	if err != nil {
		return Report{}, err
	}
	defer file.Close()

	report := Report{
		Transcript:     path,
		RuntimeFinding: "Claude Code 2.1.208 constructs ordinary compaction with the active main-loop model and inherited thinking/effort. Claude X's localhost gateway can narrowly rewrite a native Sol compact request to GPT-5.6 Luna. Transcript boundaries do not carry the direct request model, so boundary model fields remain corroborating evidence rather than proof of the routed compact model.",
	}
	var lastModel string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var row map[string]any
		if json.Unmarshal(scanner.Bytes(), &row) != nil {
			continue
		}
		if report.SessionID == "" {
			report.SessionID = stringValue(row["sessionId"])
		}
		if stringValue(row["type"]) == "assistant" {
			if message, ok := row["message"].(map[string]any); ok {
				if model := strings.TrimSpace(stringValue(message["model"])); model != "" && !strings.HasPrefix(model, "<") {
					lastModel = model
				}
			}
			continue
		}
		if stringValue(row["type"]) != "system" || stringValue(row["subtype"]) != "compact_boundary" {
			continue
		}
		metadata, _ := row["compactMetadata"].(map[string]any)
		pre := intValue(metadata["preTokens"])
		post := intValue(metadata["postTokens"])
		dropped := pre - post
		if dropped < 0 {
			dropped = 0
		}
		retained := 0.0
		if pre > 0 {
			retained = float64(post) * 100 / float64(pre)
		}
		boundary := Boundary{
			Timestamp: stringValue(row["timestamp"]), Trigger: firstNonEmpty(stringValue(metadata["trigger"]), "unknown"),
			PreTokens: pre, PostTokens: post, DroppedTokens: dropped,
			CumulativeDroppedTokens: intValue(metadata["cumulativeDroppedTokens"]), DurationMS: intValue(metadata["durationMs"]),
			RetainedPercent: retained, NearestPrecedingModel: lastModel,
			ModelEvidence: "implementation_inference_plus_nearest_preceding_assistant_model",
		}
		report.Boundaries = append(report.Boundaries, boundary)
		switch boundary.Trigger {
		case "manual":
			report.ManualCount++
		case "auto":
			report.AutomaticCount++
		}
		report.AverageDurationMS += boundary.DurationMS
		report.AveragePreTokens += boundary.PreTokens
		report.AveragePostTokens += boundary.PostTokens
	}
	if err := scanner.Err(); err != nil {
		return Report{}, fmt.Errorf("scan transcript: %w", err)
	}
	if count := int64(len(report.Boundaries)); count > 0 {
		report.AverageDurationMS /= count
		report.AveragePreTokens /= count
		report.AveragePostTokens /= count
	}
	return report, nil
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func intValue(value any) int64 {
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
