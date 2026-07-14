package sessionbind

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const maxBindingAge = 24 * time.Hour

type Binding struct {
	ClaudePID  int    `json:"claude_pid"`
	SessionID  string `json:"session_id"`
	CWD        string `json:"cwd"`
	ObservedAt string `json:"observed_at"`
}

func Record(claudePID int, sessionID, cwd string) error {
	sessionID = strings.TrimSpace(sessionID)
	if claudePID <= 0 || sessionID == "" {
		return nil
	}
	cwd = cleanCWD(cwd)
	dir, err := bindingDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	binding := Binding{ClaudePID: claudePID, SessionID: sessionID, CWD: cwd, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	path := filepath.Join(dir, strconv.Itoa(claudePID)+".json")
	tmp := path + fmt.Sprintf(".tmp.%d", os.Getpid())
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func Resolve(claudePID int, cwd string) (Binding, bool) {
	if claudePID <= 0 {
		return Binding{}, false
	}
	dir, err := bindingDir()
	if err != nil {
		return Binding{}, false
	}
	raw, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(claudePID)+".json"))
	if err != nil {
		return Binding{}, false
	}
	var binding Binding
	if json.Unmarshal(raw, &binding) != nil || binding.ClaudePID != claudePID || strings.TrimSpace(binding.SessionID) == "" {
		return Binding{}, false
	}
	observed, err := time.Parse(time.RFC3339Nano, binding.ObservedAt)
	if err != nil || time.Since(observed) > maxBindingAge || time.Until(observed) > 5*time.Minute {
		return Binding{}, false
	}
	requestedCWD := cleanCWD(cwd)
	if requestedCWD != "" && binding.CWD != "" && requestedCWD != binding.CWD {
		return Binding{}, false
	}
	return binding, true
}

func bindingDir() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("CLAUDEX_SESSION_BINDING_DIR")); configured != "" {
		return filepath.Abs(configured)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "claudex", "session-bindings"), nil
}

func cleanCWD(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return filepath.Clean(value)
	}
	return filepath.Clean(abs)
}
