package compactaudit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseCompactionBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	data := "" +
		`{"type":"assistant","sessionId":"s1","message":{"model":"gpt-5.6-sol"}}` + "\n" +
		`{"type":"system","subtype":"compact_boundary","sessionId":"s1","timestamp":"2026-07-13T00:00:00Z","compactMetadata":{"trigger":"manual","preTokens":180000,"postTokens":20000,"durationMs":200000,"cumulativeDroppedTokens":160000}}` + "\n" +
		`{"type":"assistant","sessionId":"s1","message":{"model":"<synthetic>"}}` + "\n" +
		`{"type":"system","subtype":"compact_boundary","sessionId":"s1","timestamp":"2026-07-13T01:00:00Z","compactMetadata":{"trigger":"auto","preTokens":240000,"postTokens":24000,"durationMs":100000,"cumulativeDroppedTokens":376000}}` + "\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if report.SessionID != "s1" || report.ManualCount != 1 || report.AutomaticCount != 1 || len(report.Boundaries) != 2 {
		t.Fatalf("unexpected report: %#v", report)
	}
	if report.AveragePreTokens != 210000 || report.AveragePostTokens != 22000 || report.AverageDurationMS != 150000 {
		t.Fatalf("unexpected averages: %#v", report)
	}
	if report.Boundaries[0].NearestPrecedingModel != "gpt-5.6-sol" || report.Boundaries[0].DroppedTokens != 160000 {
		t.Fatalf("model or token evidence missing: %#v", report.Boundaries[0])
	}
}
