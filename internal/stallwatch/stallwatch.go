package stallwatch

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxHookInputBytes = 4 * 1024 * 1024

type Config struct {
	Timeout  time.Duration
	Poll     time.Duration
	StateDir string
	// Blocking waits for transcript progress up to Timeout.
	// MUST be false for Claude Code PostToolUse hooks: the tool result cannot
	// enter the transcript until the hook exits, so a wait self-deadlocks until
	// timeout (observed ~300s with CLAUDEX_STALL_TIMEOUT_SECONDS=300).
	// Unit tests of rewake claim logic may set Blocking=true.
	Blocking bool
}

type HookInput struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	HookEventName  string `json:"hook_event_name"`
	ToolName       string `json:"tool_name"`
	ToolUseID      string `json:"tool_use_id"`
}

type Outcome struct {
	State      string `json:"state"`
	SessionID  string `json:"session_id,omitempty"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	RecoveryID string `json:"recovery_id,omitempty"`
	Message    string `json:"message,omitempty"`
}

type fingerprint struct {
	size    int64
	modTime time.Time
}

type stallEvent struct {
	Event      string `json:"event"`
	ObservedAt string `json:"observed_at"`
	SessionID  string `json:"session_id"`
	HookEvent  string `json:"hook_event"`
	ToolName   string `json:"tool_name,omitempty"`
	ToolUseID  string `json:"tool_use_id,omitempty"`
	RecoveryID string `json:"recovery_id"`
	IdleMS     int64  `json:"idle_ms"`
	Transcript string `json:"transcript"`
}

func Watch(ctx context.Context, input io.Reader, cfg Config) (Outcome, error) {
	if cfg.Timeout <= 0 {
		return Outcome{}, errors.New("stall timeout must be positive")
	}
	if cfg.Poll <= 0 {
		cfg.Poll = time.Second
	}
	raw, err := io.ReadAll(io.LimitReader(input, maxHookInputBytes+1))
	if err != nil {
		return Outcome{}, err
	}
	if len(raw) > maxHookInputBytes {
		return Outcome{}, errors.New("hook input exceeded 4 MiB")
	}
	var hook HookInput
	if err := json.Unmarshal(raw, &hook); err != nil {
		return Outcome{}, fmt.Errorf("parse hook input: %w", err)
	}

	// Production hook path: never block the tool pipeline.
	if !cfg.Blocking {
		return Outcome{
			State:     "nonblocking_pass",
			SessionID: hook.SessionID,
			ToolName:  hook.ToolName,
			ToolUseID: hook.ToolUseID,
		}, nil
	}

	path, err := expandPath(hook.TranscriptPath)
	if err != nil {
		return unavailable(hook), nil
	}
	initial, err := statFingerprint(path)
	if err != nil {
		// Fail open: an uncertain observer must never wake or block Claude.
		return unavailable(hook), nil
	}

	timer := time.NewTimer(cfg.Timeout)
	defer timer.Stop()
	ticker := time.NewTicker(cfg.Poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		case <-ticker.C:
			current, statErr := statFingerprint(path)
			if statErr != nil {
				return unavailable(hook), nil
			}
			if current != initial {
				return Outcome{State: "progressed", SessionID: hook.SessionID, ToolName: hook.ToolName, ToolUseID: hook.ToolUseID}, nil
			}
		case <-timer.C:
			recoveryID := recoveryID(hook, path, initial)
			claimed, claimErr := claimRecovery(cfg.StateDir, recoveryID)
			if claimErr != nil {
				return Outcome{}, claimErr
			}
			if !claimed {
				return Outcome{State: "duplicate", SessionID: hook.SessionID, ToolName: hook.ToolName, ToolUseID: hook.ToolUseID, RecoveryID: recoveryID}, nil
			}
			message := fmt.Sprintf("CLAUDEX_STALL_REWAKE %s: no transcript progress for %s after %s returned. Continue from the existing tool result. Do not repeat a side-effecting action; inspect current state once, complete only the unfinished acceptance gate, then stop or report a bounded blocker.", recoveryID, durationLabel(cfg.Timeout), valueOr(hook.ToolName, "the last tool"))
			event := stallEvent{
				Event:      "workflow_stall_rewake",
				ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
				SessionID:  hook.SessionID,
				HookEvent:  hook.HookEventName,
				ToolName:   hook.ToolName,
				ToolUseID:  hook.ToolUseID,
				RecoveryID: recoveryID,
				IdleMS:     cfg.Timeout.Milliseconds(),
				Transcript: filepath.Base(path),
			}
			if err := appendEvent(cfg.StateDir, event); err != nil {
				releaseRecovery(cfg.StateDir, recoveryID)
				return Outcome{}, err
			}
			return Outcome{State: "stalled", SessionID: hook.SessionID, ToolName: hook.ToolName, ToolUseID: hook.ToolUseID, RecoveryID: recoveryID, Message: message}, nil
		}
	}
}

func DefaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "claudex", "stall-watch")
}

func unavailable(hook HookInput) Outcome {
	return Outcome{State: "unavailable", SessionID: hook.SessionID, ToolName: hook.ToolName, ToolUseID: hook.ToolUseID}
}

func statFingerprint(path string) (fingerprint, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fingerprint{}, err
	}
	if info.IsDir() {
		return fingerprint{}, fmt.Errorf("transcript path is a directory")
	}
	return fingerprint{size: info.Size(), modTime: info.ModTime()}, nil
}

func expandPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("transcript_path is required")
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return filepath.Abs(path)
}

func recoveryID(hook HookInput, path string, initial fingerprint) string {
	identity := strings.Join([]string{hook.SessionID, hook.ToolUseID, hook.ToolName, path, fmt.Sprint(initial.size), initial.modTime.UTC().Format(time.RFC3339Nano)}, "\x00")
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:8])
}

func claimRecovery(stateDir, id string) (bool, error) {
	if strings.TrimSpace(stateDir) == "" {
		return false, errors.New("stall state directory is required")
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "claims"), 0o700); err != nil {
		return false, err
	}
	path := filepath.Join(stateDir, "claims", id)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, file.Close()
}

func releaseRecovery(stateDir, id string) {
	_ = os.Remove(filepath.Join(stateDir, "claims", id))
}

func appendEvent(stateDir string, event stallEvent) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(stateDir, "events.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoded, err := json.Marshal(event)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(file)
	if _, err := writer.Write(append(encoded, '\n')); err != nil {
		return err
	}
	return writer.Flush()
}

func durationLabel(value time.Duration) string {
	if value%time.Second == 0 {
		return value.String()
	}
	return fmt.Sprintf("%dms", value.Milliseconds())
}

func valueOr(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
