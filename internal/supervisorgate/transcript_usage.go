package supervisorgate

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// sampleTranscriptPromptTokens reads the tail of a Claude transcript JSONL and
// returns the latest assistant message.usage prompt-side token sum.
// Official PostToolUse hooks do not carry input_tokens; this is the preventive
// signal path for T3 until gateway accounting is wired.
func sampleTranscriptPromptTokens(path string) (int64, bool) {
	path = expandHome(strings.TrimSpace(path))
	if path == "" {
		return 0, false
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, false
	}
	const maxTail = 256 * 1024
	size := info.Size()
	var start int64
	if size > maxTail {
		start = size - maxTail
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return 0, false
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxTail+1))
	if err != nil {
		return 0, false
	}
	// If we started mid-line, drop the partial first line.
	if start > 0 {
		if i := bytes.IndexByte(raw, '\n'); i >= 0 && i+1 < len(raw) {
			raw = raw[i+1:]
		}
	}
	lines := bytes.Split(raw, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || !bytes.Contains(line, []byte(`"usage"`)) {
			continue
		}
		if n, ok := promptTokensFromTranscriptLine(line); ok && n > 0 {
			return n, true
		}
	}
	return 0, false
}

func promptTokensFromTranscriptLine(line []byte) (int64, bool) {
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil {
		return 0, false
	}
	message, _ := raw["message"].(map[string]any)
	if message == nil {
		return 0, false
	}
	usage, _ := message["usage"].(map[string]any)
	if usage == nil {
		return 0, false
	}
	var total int64
	for _, key := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
		if v, ok := asInt64Any(usage[key]); ok {
			total += v
		}
	}
	// Nested cache_creation (5m/1h) when aggregate field absent.
	if creation, ok := usage["cache_creation"].(map[string]any); ok {
		if v, ok := asInt64Any(creation["ephemeral_5m_input_tokens"]); ok {
			total += v
		}
		if v, ok := asInt64Any(creation["ephemeral_1h_input_tokens"]); ok {
			total += v
		}
	}
	if total <= 0 {
		return 0, false
	}
	return total, true
}

func asInt64Any(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

func appendPathHint(hints []string, path string, max int) []string {
	path = strings.TrimSpace(path)
	if path == "" {
		return hints
	}
	for _, h := range hints {
		if h == path {
			return hints
		}
	}
	hints = append(hints, path)
	if max > 0 && len(hints) > max {
		hints = hints[len(hints)-max:]
	}
	return hints
}
