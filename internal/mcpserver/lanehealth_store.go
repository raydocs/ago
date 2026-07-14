package mcpserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"claudexflow/internal/router"
	"golang.org/x/sys/unix"
)

const durableLaneHealthTTL = 24 * time.Hour

type durableLaneEntry struct {
	Tool         string `json:"tool"`
	Status       string `json:"status"`
	FailureClass string `json:"failure_class,omitempty"`
	Reason       string `json:"reason,omitempty"`
	ObservedAt   string `json:"observed_at"`
}

type durableLaneFile struct {
	UpdatedAt string             `json:"updated_at"`
	Lanes     []durableLaneEntry `json:"lanes"`
}

func durableLanePath() string {
	if p := os.Getenv("CLAUDEX_LANE_HEALTH_PATH"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claudex", "lane-health.json")
}

func withDurableLaneLock(fn func() error) error {
	path := durableLanePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lockPath := path + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	return fn()
}

func loadDurableLaneHealth() []router.LaneHealth {
	var out []router.LaneHealth
	_ = withDurableLaneLock(func() error {
		raw, err := os.ReadFile(durableLanePath())
		if err != nil {
			return nil
		}
		var file durableLaneFile
		if json.Unmarshal(raw, &file) != nil {
			return nil
		}
		now := time.Now()
		for _, e := range file.Lanes {
			obs, err := time.Parse(time.RFC3339Nano, e.ObservedAt)
			if err != nil || now.Sub(obs) > durableLaneHealthTTL {
				continue
			}
			if e.Status != "unavailable" {
				// degraded is session-local only; durable quarantine is for hard classes.
				continue
			}
			out = append(out, router.LaneHealth{
				Tool: e.Tool, Status: e.Status, FailureClass: e.FailureClass, Reason: e.Reason,
			})
		}
		return nil
	})
	return out
}

func persistDurableLaneHealth(health router.LaneHealth) {
	// Only persist hard unavailability classes. Ordinary success does NOT auto-clear;
	// use lane-health clear --canary-pass after an explicit health canary.
	if health.Status != "unavailable" {
		return
	}
	if health.FailureClass != failureAuthConfiguration && health.FailureClass != failureModelMismatch {
		return
	}
	_ = withDurableLaneLock(func() error {
		path := durableLanePath()
		file := durableLaneFile{UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
		if raw, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(raw, &file)
		}
		now := time.Now().UTC()
		nowS := now.Format(time.RFC3339Nano)
		found := false
		for i := range file.Lanes {
			if file.Lanes[i].Tool != health.Tool {
				continue
			}
			// Freshness: do not overwrite a newer durable entry with an older reason.
			if prev, err := time.Parse(time.RFC3339Nano, file.Lanes[i].ObservedAt); err == nil && prev.After(now) {
				found = true
				break
			}
			file.Lanes[i] = durableLaneEntry{
				Tool: health.Tool, Status: health.Status, FailureClass: health.FailureClass,
				Reason: health.Reason, ObservedAt: nowS,
			}
			found = true
			break
		}
		if !found {
			file.Lanes = append(file.Lanes, durableLaneEntry{
				Tool: health.Tool, Status: health.Status, FailureClass: health.FailureClass,
				Reason: health.Reason, ObservedAt: nowS,
			})
		}
		file.UpdatedAt = nowS
		return atomicWriteJSON(path, file)
	})
}

// ClearDurableLane removes one tool from durable quarantine after explicit canary.
func ClearDurableLane(tool string) error {
	tool = filepath.Clean(tool)
	if tool == "" || tool == "." {
		return fmt.Errorf("tool is required")
	}
	return withDurableLaneLock(func() error {
		path := durableLanePath()
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		var file durableLaneFile
		if err := json.Unmarshal(raw, &file); err != nil {
			return err
		}
		next := file.Lanes[:0]
		for _, e := range file.Lanes {
			if e.Tool != tool {
				next = append(next, e)
			}
		}
		file.Lanes = next
		file.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return atomicWriteJSON(path, file)
	})
}

func atomicWriteJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".lane-health-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
