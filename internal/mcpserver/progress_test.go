package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkerProgressPersistsStartAndFinishWithoutClientToken(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDEX_PROGRESS_DIR", dir)
	s := &Server{}
	p := s.beginWorkerProgress(context.Background(), nil, "worker-7", "grok-4.5/high")
	p.finish("done")
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Count(text, "worker_progress") != 2 || !strings.Contains(text, "Worker started") || !strings.Contains(text, `"message":"done"`) {
		t.Fatalf("unexpected progress log: %s", text)
	}
}
