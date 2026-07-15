package threadsync

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"claudexflow/internal/sessionbind"
	"claudexflow/internal/threadgraph"
	"claudexflow/internal/threadusage"
)

const (
	defaultMaxPayload = 192 * 1024
	maxInputBytes     = 4 * 1024 * 1024
)

type Config struct {
	Enabled         bool   `json:"enabled"`
	Endpoint        string `json:"endpoint"`
	DashboardURL    string `json:"dashboard_url"`
	IngestToken     string `json:"ingest_token"`
	ViewPassword    string `json:"view_password,omitempty"` // deprecated: view is public; ignored
	MachineID       string `json:"machine_id"`
	MaxPayloadBytes int    `json:"max_payload_bytes,omitempty"`
	SpoolPath       string `json:"spool_path,omitempty"`
}

type Delivery struct {
	Enabled bool   `json:"enabled"`
	Sent    bool   `json:"sent"`
	Spooled bool   `json:"spooled"`
	EventID string `json:"event_id,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

type Status struct {
	Enabled       bool   `json:"enabled"`
	Endpoint      string `json:"endpoint,omitempty"`
	DashboardURL  string `json:"dashboard_url,omitempty"`
	MachineID     string `json:"machine_id,omitempty"`
	PendingEvents int    `json:"pending_events"`
	ConfigPath    string `json:"config_path"`
	SpoolPath     string `json:"spool_path,omitempty"`
}

var (
	sensitiveKey = regexp.MustCompile(`(?i)(api[_-]?key|auth(?:orization)?|bearer|cookie|credential|password|secret|session[_-]?secret|token)`)
	bearerValue  = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	openAIKey    = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}\b`)
	dottedKey    = regexp.MustCompile(`\b[0-9a-fA-F]{24,}\.[A-Za-z0-9_-]{12,}\b`)
	assignedKey  = regexp.MustCompile(`(?i)((?:api[_-]?key|auth[_-]?token|token|secret|password)\s*[:=]\s*)[^\s,;"']{8,}`)
)

func Collect(ctx context.Context, input io.Reader) Delivery {
	cfg, path, err := LoadConfig()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Delivery{Detail: "thread sync is not configured"}
		}
		return Delivery{Detail: err.Error()}
	}
	if !cfg.Enabled {
		return Delivery{Detail: "thread sync is disabled"}
	}
	raw, err := io.ReadAll(io.LimitReader(input, maxInputBytes+1))
	if err != nil {
		return Delivery{Enabled: true, Detail: err.Error()}
	}
	if len(raw) > maxInputBytes {
		return Delivery{Enabled: true, Detail: "hook payload exceeded local input limit"}
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Delivery{Enabled: true, Detail: "invalid hook JSON: " + err.Error()}
	}
	defer cleanupEphemeralSecurityKey(payload)

	eventID := newEventID()
	sessionID := stringFromAny(payload["session_id"])
	// Command hooks run as children of the active Claude process. Bind that
	// process to the observed session so its long-lived MCP server can attach
	// delegated model sessions to the correct Root/Parent Thread.
	_ = sessionbind.Record(os.Getppid(), sessionID, stringFromAny(payload["cwd"]))
	rootSessionID := firstConfigured(stringFromAny(payload["root_session_id"]), os.Getenv("CLAUDEX_THREAD_ROOT_SESSION_ID"), sessionID)
	graphAttach := threadgraph.AttachToPayload(payload, "", threadgraph.Context{
		SessionID:       sessionID,
		RootSessionID:   rootSessionID,
		ParentSessionID: firstConfigured(stringFromAny(payload["parent_session_id"]), os.Getenv("CLAUDEX_THREAD_PARENT_SESSION_ID")),
		Role:            firstConfigured(os.Getenv("CLAUDEX_THREAD_ROLE"), "supervisor"),
		Effort:          os.Getenv("CLAUDEX_THREAD_EFFORT"),
	})
	// Attach numeric usage deltas before redaction so only compact usage fields are present.
	usageAttach := threadusage.AttachToPayload(payload, "")
	payload = sanitizeMap(payload, 16_000)
	// Re-normalize usage_records after sanitize to keep pure numeric maps (no content).
	if rawUsage, ok := payload["usage_records"]; ok {
		payload["usage_records"] = sanitizeUsageRecords(rawUsage)
	}
	payload["collector"] = map[string]any{
		"event_id":    eventID,
		"observed_at": time.Now().UTC().Format(time.RFC3339Nano),
		"machine_id":  cfg.MachineID,
		"model":       os.Getenv("CLAUDEX_THREAD_MODEL"),
		"effort":      os.Getenv("CLAUDEX_THREAD_EFFORT"),
		"role":        os.Getenv("CLAUDEX_THREAD_ROLE"),
		"version":     1,
	}
	encoded, err := boundedJSON(payload, cfg.maxPayload())
	if err != nil {
		// If the payload is still too large, drop bulk text fields but keep usage_records.
		if _, ok := payload["usage_records"]; ok {
			for _, key := range []string{"tool_response", "tool_input", "last_assistant_message", "prompt"} {
				delete(payload, key)
			}
			encoded, err = boundedJSON(payload, cfg.maxPayload())
		}
		if err != nil {
			return Delivery{Enabled: true, EventID: eventID, Detail: err.Error()}
		}
	}
	if err := send(ctx, cfg, encoded); err == nil {
		_ = threadusage.CommitCursor(usageAttach)
		_ = threadgraph.CommitCursor(graphAttach)
		return Delivery{Enabled: true, Sent: true, EventID: eventID}
	} else if spoolErr := appendSpool(cfg.spool(path), encoded); spoolErr != nil {
		return Delivery{Enabled: true, EventID: eventID, Detail: fmt.Sprintf("send failed: %v; spool failed: %v", err, spoolErr)}
	} else {
		// Cursor advances after spool so retries use the same payload without double-scan growth.
		_ = threadusage.CommitCursor(usageAttach)
		_ = threadgraph.CommitCursor(graphAttach)
		return Delivery{Enabled: true, Spooled: true, EventID: eventID, Detail: err.Error()}
	}
}

// Some Claude/custom-model runtime builds create a per-working-copy
// logs/security/.security-key. It is session plumbing, not project output.
// Remove it only on terminal hooks and only when the directory contains the
// exact 32-byte key and nothing else; any real security log makes this a no-op.
func cleanupEphemeralSecurityKey(payload map[string]any) {
	event := stringFromAny(payload["hook_event_name"])
	if event != "Stop" && event != "SessionEnd" {
		return
	}
	cwd := stringFromAny(payload["cwd"])
	if cwd == "" {
		return
	}
	dir := filepath.Join(cwd, "logs", "security")
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != ".security-key" || entries[0].Type()&os.ModeSymlink != 0 {
		return
	}
	key := filepath.Join(dir, ".security-key")
	info, err := os.Lstat(key)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 32 {
		return
	}
	if os.Remove(key) != nil || os.Remove(dir) != nil {
		return
	}
	_ = os.Remove(filepath.Join(cwd, "logs")) // succeeds only when empty
}

// sanitizeUsageRecords keeps only the compact numeric usage shape for upload.
func sanitizeUsageRecords(value any) any {
	switch typed := value.(type) {
	case []map[string]any:
		out := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, compactUsageMap(item))
		}
		return out
	case []any:
		out := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			if m, ok := item.(map[string]any); ok {
				out = append(out, compactUsageMap(m))
			}
		}
		return out
	default:
		return []map[string]any{}
	}
}

func compactUsageMap(item map[string]any) map[string]any {
	out := map[string]any{
		"usage_id":              stringFromAny(item["usage_id"]),
		"session_id":            stringFromAny(item["session_id"]),
		"message_id":            stringFromAny(item["message_id"]),
		"request_id":            stringFromAny(item["request_id"]),
		"observed_at":           stringFromAny(item["observed_at"]),
		"model":                 stringFromAny(item["model"]),
		"input_tokens":          intFromAny(item["input_tokens"]),
		"cache_write_5m_tokens": intFromAny(item["cache_write_5m_tokens"]),
		"cache_write_1h_tokens": intFromAny(item["cache_write_1h_tokens"]),
		"cache_read_tokens":     intFromAny(item["cache_read_tokens"]),
		"output_tokens":         intFromAny(item["output_tokens"]),
		"is_fast":               boolFromAny(item["is_fast"]),
	}
	if v, ok := item["carried_cost_usd"]; ok && v != nil {
		out["carried_cost_usd"] = v
	}
	return out
}

func stringFromAny(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func firstConfigured(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func intFromAny(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func boolFromAny(v any) bool {
	b, _ := v.(bool)
	return b
}

func Flush(ctx context.Context) (sent, pending int, err error) {
	cfg, path, err := LoadConfig()
	if err != nil {
		return 0, 0, err
	}
	spool := cfg.spool(path)
	draining := spool + ".draining-" + newEventID()
	if err := os.Rename(spool, draining); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	file, err := os.Open(draining)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	defer os.Remove(draining)

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxInputBytes)
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := send(ctx, cfg, line); err != nil {
			pending++
			if appendErr := appendSpool(spool, line); appendErr != nil {
				return sent, pending, fmt.Errorf("requeue failed after send error %v: %w", err, appendErr)
			}
			continue
		}
		sent++
	}
	if err := scanner.Err(); err != nil {
		return sent, pending, err
	}
	return sent, pending, nil
}

func GetStatus() (Status, error) {
	cfg, path, err := LoadConfig()
	if err != nil {
		return Status{ConfigPath: path}, err
	}
	spool := cfg.spool(path)
	count, _ := countLines(spool)
	return Status{
		Enabled:       cfg.Enabled,
		Endpoint:      cfg.Endpoint,
		DashboardURL:  cfg.DashboardURL,
		MachineID:     cfg.MachineID,
		PendingEvents: count,
		ConfigPath:    path,
		SpoolPath:     spool,
	}, nil
}

func OpenDashboard() error {
	cfg, _, err := LoadConfig()
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.DashboardURL) == "" {
		return errors.New("dashboard_url is not configured")
	}
	var command string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		command, args = "open", []string{cfg.DashboardURL}
	case "linux":
		command, args = "xdg-open", []string{cfg.DashboardURL}
	default:
		return fmt.Errorf("open %s manually", cfg.DashboardURL)
	}
	if err := exec.Command(command, args...).Start(); err != nil {
		return err
	}
	fmt.Println("dashboard opened:", cfg.DashboardURL)
	return nil
}

func LoadConfig() (Config, string, error) {
	path := os.Getenv("CLAUDEX_THREAD_SYNC_CONFIG")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Config{}, "", err
		}
		path = filepath.Join(home, ".config", "claudex", "thread-sync.json")
	}
	cfg, err := loadConfigAt(path)
	return cfg, path, err
}

func loadConfigAt(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.Enabled && (strings.TrimSpace(cfg.Endpoint) == "" || strings.TrimSpace(cfg.IngestToken) == "") {
		return Config{}, errors.New("enabled thread sync requires endpoint and ingest_token")
	}
	return cfg, nil
}

func send(parent context.Context, cfg Config, payload []byte) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.IngestToken)
	req.Header.Set("User-Agent", "claudex-flow-thread-hook/1")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("ingest returned %s: %s", response.Status, strings.TrimSpace(string(detail)))
	}
	return nil
}

func sanitizeMap(input map[string]any, maxString int) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = sanitizeValue(key, value, maxString)
	}
	return out
}

func sanitizeValue(key string, value any, maxString int) any {
	if sensitiveKey.MatchString(key) {
		return "[REDACTED]"
	}
	switch typed := value.(type) {
	case map[string]any:
		return sanitizeMap(typed, maxString)
	case []any:
		out := make([]any, len(typed))
		for index, item := range typed {
			out[index] = sanitizeValue("", item, maxString)
		}
		return out
	case string:
		return sanitizeString(typed, maxString)
	default:
		return value
	}
}

func sanitizeString(value string, max int) string {
	value = bearerValue.ReplaceAllString(value, `${1}[REDACTED]`)
	value = openAIKey.ReplaceAllString(value, "[REDACTED]")
	value = dottedKey.ReplaceAllString(value, "[REDACTED]")
	value = assignedKey.ReplaceAllString(value, `${1}[REDACTED]`)
	if len(value) > max {
		return value[:max] + "…[truncated]"
	}
	return value
}

func boundedJSON(payload map[string]any, max int) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(encoded) <= max {
		return encoded, nil
	}
	for _, key := range []string{"tool_response", "tool_input", "last_assistant_message", "prompt"} {
		if value, ok := payload[key]; ok {
			payload[key] = sanitizeValue(key, value, 2_000)
		}
	}
	encoded, err = json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(encoded) > max {
		payload["tool_response"] = "[TRUNCATED: payload exceeded configured cloud limit]"
		payload["tool_input"] = "[TRUNCATED: payload exceeded configured cloud limit]"
		encoded, err = json.Marshal(payload)
	}
	if err != nil {
		return nil, err
	}
	if len(encoded) > max {
		return nil, fmt.Errorf("sanitized hook payload is still larger than %d bytes", max)
	}
	return encoded, nil
}

func appendSpool(path string, payload []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	line := append(append([]byte(nil), payload...), '\n')
	_, err = file.Write(line)
	return err
}

func countLines(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer file.Close()
	count := 0
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) > 0 {
			count++
		}
	}
	return count, scanner.Err()
}

func (cfg Config) maxPayload() int {
	if cfg.MaxPayloadBytes <= 0 || cfg.MaxPayloadBytes > 240*1024 {
		return defaultMaxPayload
	}
	return cfg.MaxPayloadBytes
}

func (cfg Config) spool(configPath string) string {
	if cfg.SpoolPath != "" {
		return cfg.SpoolPath
	}
	return filepath.Join(filepath.Dir(configPath), "thread-spool.jsonl")
}

func newEventID() string {
	return fmt.Sprintf("%d-%d", time.Now().UTC().UnixNano(), os.Getpid())
}
