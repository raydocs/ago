package routeeval

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDryRunPromoteRealRouteRecordSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "route-outcomes.jsonl")
	// Real-ish RouteRecord shapes: state + outcome.status, not top-level accepted.
	lines := []string{
		`{"route_id":"route-1","state":"accepted","family":"parser","plan":{"kind":"implement","action":"bounded_worker"},"outcome":{"status":"accepted","verification":"go test PASS","human_correction":"none"},"diagnostics":{"duration_ms":100,"usage":{"input_tokens":1}}}`,
		`{"route_id":"route-2","state":"accepted","family":"parser","plan":{"kind":"implement"},"outcome":{"status":"accepted","human_correction":"none"},"diagnostics":{}}`,
		`{"route_id":"route-3","state":"failed","family":"parser","plan":{"kind":"implement"},"outcome":{"status":"failed"},"diagnostics":{}}`,
		// Should not match family filter
		`{"route_id":"route-x","state":"accepted","family":"other","plan":{"kind":"explore"},"outcome":{"status":"accepted"},"diagnostics":{}}`,
	}
	var body string
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dry, err := DryRunPromote(path, "parser")
	if err != nil {
		t.Fatal(err)
	}
	if dry.Accepted != 2 || dry.Failed != 1 || dry.Total != 3 {
		t.Fatalf("got accepted=%d failed=%d total=%d", dry.Accepted, dry.Failed, dry.Total)
	}
	if dry.DistinctRouteIDs != 3 {
		t.Fatalf("distinct=%d", dry.DistinctRouteIDs)
	}
}
