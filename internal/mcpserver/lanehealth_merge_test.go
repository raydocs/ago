package mcpserver

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"claudexflow/internal/router"
)

func TestLiveLaneHealthPrefersFresherDurableOverStaleSessionHealthy(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDEX_LANE_HEALTH_PATH", filepath.Join(dir, "lane-health.json"))

	s := &Server{laneHealth: map[string]router.LaneHealth{}}
	// Process A: stale session healthy at T1.
	t1 := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339Nano)
	s.laneHealth["search_external"] = router.LaneHealth{
		Tool: "search_external", Status: "healthy", ObservedAt: t1,
	}

	// Process B wrote durable unavailable at T2 (newer).
	t2 := time.Now().UTC().Add(-1 * time.Minute).Format(time.RFC3339Nano)
	path := durableLanePath()
	raw := []byte(`{
  "updated_at": "` + t2 + `",
  "lanes": [
    {
      "tool": "search_external",
      "status": "unavailable",
      "failure_class": "auth_configuration",
      "reason": "gateway auth failed",
      "observed_at": "` + t2 + `"
    }
  ]
}
`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	live := s.liveLaneHealth()
	var found *router.LaneHealth
	for i := range live {
		if live[i].Tool == "search_external" {
			found = &live[i]
			break
		}
	}
	if found == nil {
		t.Fatal("search_external missing from live health")
	}
	if found.Status != "unavailable" {
		t.Fatalf("stale session healthy overrode fresher durable: %#v", found)
	}
}

func TestSameSessionHealthyAfterFailureRestores(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDEX_LANE_HEALTH_PATH", filepath.Join(dir, "lane-health.json"))
	s := &Server{}
	s.recordLaneFailure("search_external", failureInfo{Class: failureAuthConfiguration, Detail: "auth"})
	// Immediate healthy in same process is later ObservedAt → restores.
	time.Sleep(2 * time.Millisecond)
	s.recordLaneHealthy("search_external")
	live := s.liveLaneHealth()
	for _, h := range live {
		if h.Tool == "search_external" && h.Status != "healthy" {
			t.Fatalf("same-session healthy should restore: %#v", h)
		}
	}
}
