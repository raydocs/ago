package ledger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Run struct {
	ID           string `json:"id"`
	StartedAt    string `json:"started_at"`
	Objective    string `json:"objective"`
	Model        string `json:"model"`
	Effort       string `json:"effort,omitempty"`
	Kind         string `json:"kind"`
	Risk         string `json:"risk"`
	Status       string `json:"status"`
	Verification string `json:"verification,omitempty"`
	Evidence     any    `json:"evidence,omitempty"`
	NextStep     string `json:"next_step,omitempty"`
}

func New(objective, model, effort, kind, risk string) Run {
	now := time.Now()
	return Run{ID: now.Format("20060102-150405.000"), StartedAt: now.Format(time.RFC3339), Objective: objective, Model: model, Effort: effort, Kind: kind, Risk: risk, Status: "doing"}
}

func Save(workDir string, run Run) (string, error) {
	dir := filepath.Join(workDir, ".claudex-flow", "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, run.ID+".json")
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func MustSave(workDir string, run Run) string {
	path, err := Save(workDir, run)
	if err != nil {
		return fmt.Sprintf("ledger save failed: %v", err)
	}
	return path
}
