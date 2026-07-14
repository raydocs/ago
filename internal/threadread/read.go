package threadread

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"claudexflow/internal/threadgraph"
)

const (
	DefaultMaxSourceBytes = 96 * 1024
	MinSourceBytes        = 16 * 1024
	MaxSourceBytes        = 160 * 1024
)

var threadIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

type Prepared struct {
	ThreadID       string `json:"thread_id"`
	TranscriptPath string `json:"transcript_path"`
	EventCount     int    `json:"event_count"`
	SelectedEvents int    `json:"selected_events"`
	SourceBytes    int    `json:"source_bytes"`
	LatestCompact  bool   `json:"latest_compact_included"`
	Packet         string `json:"packet"`
}

type event struct {
	id        string
	timestamp string
	role      string
	kind      string
	text      string
	compact   bool
}

func Prepare(transcriptRoot, reference, question string, maxSourceBytes int) (Prepared, error) {
	threadID, err := NormalizeThreadID(reference)
	if err != nil {
		return Prepared{}, err
	}
	if maxSourceBytes == 0 {
		maxSourceBytes = DefaultMaxSourceBytes
	}
	if maxSourceBytes < MinSourceBytes || maxSourceBytes > MaxSourceBytes {
		return Prepared{}, fmt.Errorf("max_source_bytes must be between %d and %d", MinSourceBytes, MaxSourceBytes)
	}
	path, err := locateTranscript(transcriptRoot, threadID)
	if err != nil {
		return Prepared{}, err
	}
	events, err := parseTranscript(path, threadID)
	if err != nil {
		return Prepared{}, err
	}
	if len(events) == 0 {
		return Prepared{}, fmt.Errorf("thread %s has no readable transcript events", threadID)
	}
	selected, latestCompact := selectEvents(events, question, maxSourceBytes)
	packet := render(threadID, question, selected, len(events), latestCompact)
	if len(packet) > maxSourceBytes {
		packet = packet[:maxSourceBytes]
	}
	return Prepared{
		ThreadID: threadID, TranscriptPath: path, EventCount: len(events), SelectedEvents: len(selected),
		SourceBytes: len(packet), LatestCompact: latestCompact, Packet: packet,
	}, nil
}

func NormalizeThreadID(reference string) (string, error) {
	value := strings.TrimSpace(reference)
	for _, marker := range []string{"#/thread/", "/threads/", "/thread/"} {
		if index := strings.LastIndex(value, marker); index >= 0 {
			value = value[index+len(marker):]
			if stop := strings.IndexAny(value, "/?#"); stop >= 0 {
				value = value[:stop]
			}
			break
		}
	}
	value = strings.TrimSpace(strings.TrimPrefix(value, "@"))
	if !threadIDPattern.MatchString(value) {
		return "", fmt.Errorf("thread_id must be a local Claude X session ID or thread URL")
	}
	return value, nil
}

func locateTranscript(root, threadID string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".claude", "projects")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", threadID+".jsonl"))
	if err != nil {
		return "", err
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("local transcript for thread %s was not found under %s", threadID, root)
	}
	sort.Strings(matches)
	return matches[0], nil
}

func parseTranscript(path, threadID string) ([]event, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	parser := threadgraph.NewParser(threadgraph.Context{SessionID: threadID, RootSessionID: threadID})
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var events []event
	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		compactSummary := false
		var row map[string]any
		if json.Unmarshal(line, &row) == nil {
			compactSummary, _ = row["isCompactSummary"].(bool)
		}
		for _, parsed := range parser.ParseLine(line) {
			text := strings.TrimSpace(parsed.Content)
			if text == "" {
				text = strings.TrimSpace(parsed.Summary)
			}
			if text == "" {
				continue
			}
			events = append(events, event{
				id: parsed.EventID, timestamp: parsed.StartedAt, role: parsed.Role, kind: parsed.Type,
				text: text, compact: compactSummary,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func selectEvents(events []event, question string, budget int) ([]event, bool) {
	selected := map[int]bool{}
	used := len(question) + 512
	add := func(index int) bool {
		if index < 0 || index >= len(events) || selected[index] {
			return true
		}
		cost := len(events[index].text) + 160
		if used+cost > budget {
			return false
		}
		selected[index] = true
		used += cost
		return true
	}
	latestCompact := -1
	for index := range events {
		if events[index].compact {
			latestCompact = index
		}
	}
	if latestCompact >= 0 {
		add(latestCompact)
	}
	terms := queryTerms(question)
	// Prefer newer matching evidence, while retaining two neighboring events for
	// chronology. Bound this phase so a common query term cannot crowd out the
	// current state.
	matched := 0
	for index := len(events) - 1; index >= 0 && matched < 32; index-- {
		item := events[index]
		if matchesQuery(item.text, terms) {
			matched++
			add(index)
			for distance := 1; distance <= 2; distance++ {
				add(index - distance)
				add(index + distance)
			}
		}
	}
	// Always retain the latest exchange and recent tool results. This gives the
	// extractor a trustworthy current state even when the query uses synonyms.
	for index := len(events) - 1; index >= 0 && len(selected) < 96; index-- {
		add(index)
	}
	indices := make([]int, 0, len(selected))
	for index := range selected {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	kept := make([]event, 0, len(indices))
	for _, index := range indices {
		kept = append(kept, events[index])
	}
	return kept, latestCompact >= 0 && selected[latestCompact]
}

func queryTerms(question string) []string {
	parts := strings.FieldsFunc(strings.ToLower(question), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && r != '_' && r != '-'
	})
	seen := map[string]bool{}
	var out []string
	for _, part := range parts {
		if len([]rune(part)) < 2 || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	return out
}

func matchesQuery(text string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	lower := strings.ToLower(text)
	matches := 0
	for _, term := range terms {
		if strings.Contains(lower, term) {
			matches++
		}
	}
	return matches >= 1
}

func render(threadID, question string, events []event, total int, latestCompact bool) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "Thread: %s\nQuestion: %s\nSelection: %d of %d sanitized events; latest compact summary included=%t.\n", threadID, strings.TrimSpace(question), len(events), total, latestCompact)
	builder.WriteString("Use compact summaries for orientation only. Prefer original events below for exact requirements, commands, chronology, edits, and verification.\n\n")
	for _, item := range events {
		fmt.Fprintf(&builder, "[%s] [%s/%s] [thread://%s#%s]\n%s\n\n", item.timestamp, item.role, item.kind, threadID, item.id, item.text)
	}
	return builder.String()
}
