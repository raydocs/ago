package threadfind

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"claudexflow/internal/threadgraph"
)

const (
	DefaultLimit = 8
	MaxLimit     = 25
	maxSnippet   = 240
	maxSnippets  = 3
)

// Query is a deterministic, local-only search over Claude Code root
// transcripts. Structured fields and Amp-style filters in Text are combined.
type Query struct {
	Text            string `json:"query,omitempty"`
	File            string `json:"file,omitempty"`
	Project         string `json:"project,omitempty"`
	After           string `json:"after,omitempty"`
	Before          string `json:"before,omitempty"`
	ExcludeThreadID string `json:"exclude_thread_id,omitempty"`
	Limit           int    `json:"limit,omitempty"`
}

type Match struct {
	ThreadID  string   `json:"thread_id"`
	Title     string   `json:"title"`
	Project   string   `json:"project"`
	StartedAt string   `json:"started_at,omitempty"`
	UpdatedAt string   `json:"updated_at"`
	MatchedBy []string `json:"matched_by"`
	Snippets  []string `json:"snippets,omitempty"`
	Source    string   `json:"source"`
	Score     int      `json:"score"`
}

type Result struct {
	Query          Query   `json:"query"`
	Matches        []Match `json:"matches"`
	ThreadsSeen    int     `json:"threads_seen"`
	ThreadsScanned int     `json:"threads_scanned"`
	BytesScanned   int64   `json:"bytes_scanned"`
	DurationMS     int64   `json:"duration_ms"`
	SearchMode     string  `json:"search_mode"`
}

type parsedQuery struct {
	Query
	terms  []string
	after  time.Time
	before time.Time
}

type rowMeta struct {
	Type      string `json:"type"`
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
	AITitle   string `json:"aiTitle"`
}

// Validate checks the bounded query contract without opening transcript files.
func Validate(input Query) error {
	_, err := parseQuery(input, time.Now())
	return err
}

// Find searches only direct root transcript files under ~/.claude/projects.
// It streams files and uses the canonical parser before returning snippets, so
// raw credentials or arbitrary transcript JSON never cross the tool boundary.
func Find(transcriptRoot string, input Query) (Result, error) {
	started := time.Now()
	query, err := parseQuery(input, started)
	if err != nil {
		return Result{}, err
	}
	root, err := canonicalRoot(transcriptRoot)
	if err != nil {
		return Result{}, err
	}
	paths, err := rootTranscripts(root)
	if err != nil {
		return Result{}, err
	}
	result := Result{Query: query.Query, ThreadsSeen: len(paths), SearchMode: "streaming_local_root_transcripts"}
	for _, path := range paths {
		info, statErr := os.Stat(path)
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		if !query.after.IsZero() && info.ModTime().Before(query.after) {
			continue
		}
		if !query.before.IsZero() && !info.ModTime().Before(query.before) {
			continue
		}
		threadID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		if threadID == query.ExcludeThreadID {
			continue
		}
		encodedProject := filepath.Base(filepath.Dir(path))
		if query.Project != "" && !projectMatchesEncoded(encodedProject, query.Project) {
			// The encoded directory is derived from cwd. Avoid opening unrelated
			// projects when an explicit project filter is present.
			continue
		}
		match, scanned, ok, scanErr := scanTranscript(path, threadID, encodedProject, info, query)
		result.BytesScanned += scanned
		result.ThreadsScanned++
		if scanErr != nil {
			return Result{}, fmt.Errorf("scan thread %s: %w", threadID, scanErr)
		}
		if ok {
			result.Matches = append(result.Matches, match)
		}
	}
	sort.Slice(result.Matches, func(i, j int) bool {
		if result.Matches[i].Score != result.Matches[j].Score {
			return result.Matches[i].Score > result.Matches[j].Score
		}
		return result.Matches[i].UpdatedAt > result.Matches[j].UpdatedAt
	})
	if len(result.Matches) > query.Limit {
		result.Matches = result.Matches[:query.Limit]
	}
	result.DurationMS = time.Since(started).Milliseconds()
	return result, nil
}

func parseQuery(input Query, now time.Time) (parsedQuery, error) {
	if len(input.Text) > 4000 || len(input.File) > 2048 || len(input.Project) > 1024 || len(input.After) > 64 || len(input.Before) > 64 || len(input.ExcludeThreadID) > 128 {
		return parsedQuery{}, fmt.Errorf("find_thread query exceeds bounded field limits")
	}
	input.Text = strings.TrimSpace(input.Text)
	input.File = strings.TrimSpace(input.File)
	input.Project = strings.TrimSpace(input.Project)
	input.After = strings.TrimSpace(input.After)
	input.Before = strings.TrimSpace(input.Before)
	input.ExcludeThreadID = strings.TrimSpace(strings.TrimPrefix(input.ExcludeThreadID, "@"))
	if input.Limit == 0 {
		input.Limit = DefaultLimit
	}
	if input.Limit < 1 || input.Limit > MaxLimit {
		return parsedQuery{}, fmt.Errorf("limit must be between 1 and %d", MaxLimit)
	}
	var terms []string
	for _, token := range splitQuery(input.Text) {
		key, value, filtered := splitFilter(token)
		if filtered {
			switch key {
			case "file":
				if input.File == "" {
					input.File = value
				}
			case "project", "repo":
				if input.Project == "" {
					input.Project = value
				}
			case "after":
				if input.After == "" {
					input.After = value
				}
			case "before":
				if input.Before == "" {
					input.Before = value
				}
			default:
				terms = append(terms, token)
			}
			continue
		}
		terms = append(terms, token)
	}
	input.File = cleanTerm(input.File)
	input.Project = cleanTerm(input.Project)
	for index := range terms {
		terms[index] = cleanTerm(terms[index])
	}
	terms = compactTerms(terms)
	if len(terms) == 0 && input.File == "" && input.Project == "" && input.After == "" && input.Before == "" {
		return parsedQuery{}, fmt.Errorf("provide a keyword, file, project, after, or before filter")
	}
	after, err := parseBound(input.After, now)
	if err != nil {
		return parsedQuery{}, fmt.Errorf("after: %w", err)
	}
	before, err := parseBound(input.Before, now)
	if err != nil {
		return parsedQuery{}, fmt.Errorf("before: %w", err)
	}
	if !after.IsZero() && !before.IsZero() && !after.Before(before) {
		return parsedQuery{}, fmt.Errorf("after must be earlier than before")
	}
	return parsedQuery{Query: input, terms: terms, after: after, before: before}, nil
}

func scanTranscript(path, threadID, encodedProject string, info os.FileInfo, query parsedQuery) (Match, int64, bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return Match{}, 0, false, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 128*1024)
	parser := threadgraph.NewParser(threadgraph.Context{SessionID: threadID, RootSessionID: threadID})
	found := make(map[string]bool, len(query.terms))
	fileFound := query.File == ""
	projectFound := query.Project == ""
	matchedBy := map[string]bool{}
	match := Match{ThreadID: threadID, Project: decodeProject(encodedProject), UpdatedAt: info.ModTime().UTC().Format(time.RFC3339), Source: "thread://" + threadID}
	fallbackTitle := ""
	var scanned int64
	for {
		line, readErr := reader.ReadBytes('\n')
		scanned += int64(len(line))
		if len(line) > 0 {
			var meta rowMeta
			needsMeta := match.StartedAt == "" || match.Title == "" || !projectFound
			if needsMeta && json.Unmarshal(bytes.TrimSpace(line), &meta) == nil {
				if match.StartedAt == "" && meta.Timestamp != "" {
					match.StartedAt = meta.Timestamp
				}
				if meta.AITitle != "" {
					match.Title = trimDisplay(meta.AITitle, 160)
				}
				if meta.CWD != "" {
					match.Project = filepath.Base(filepath.Clean(meta.CWD))
					projectFound = query.Project == "" || projectMatchesPath(meta.CWD, query.Project)
				}
			}

			if mayContainAny(line, query.terms, query.File) {
				for _, event := range parser.ParseLine(bytes.TrimSuffix(line, []byte{'\n'})) {
					doc := eventDocument(event)
					lineMatched := false
					for _, term := range query.terms {
						if !found[term] && containsFold(doc, term) {
							found[term] = true
							matchedBy["keyword:"+term] = true
							lineMatched = true
						}
					}
					if !fileFound && containsFold(doc, query.File) {
						fileFound = true
						matchedBy["file:"+query.File] = true
						lineMatched = true
					}
					if lineMatched && len(match.Snippets) < maxSnippets {
						summary := evidenceSnippet(event, query.terms, query.File)
						if summary != "" {
							match.Snippets = append(match.Snippets, fmt.Sprintf("%s · thread://%s#%s", summary, threadID, event.EventID))
						}
						if fallbackTitle == "" && !isBoilerplateTitle(event.Summary) {
							fallbackTitle = trimDisplay(event.Summary, 160)
						}
					}
				}
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return Match{}, scanned, false, readErr
		}
	}
	if match.Title == "" {
		if fallbackTitle != "" {
			match.Title = fallbackTitle
		} else {
			match.Title = "Claude X thread " + threadID[:min(8, len(threadID))]
		}
	}
	if !projectFound || !fileFound || !allFound(found, query.terms) {
		return Match{}, scanned, false, nil
	}
	if query.Project != "" {
		matchedBy["project:"+query.Project] = true
	}
	if query.After != "" {
		matchedBy["after:"+query.After] = true
	}
	if query.Before != "" {
		matchedBy["before:"+query.Before] = true
	}
	for value := range matchedBy {
		match.MatchedBy = append(match.MatchedBy, value)
	}
	sort.Strings(match.MatchedBy)
	match.Score = scoreMatch(match, query)
	return match, scanned, true, nil
}

func canonicalRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		root = filepath.Join(home, ".claude", "projects")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("invalid transcript root: %w", err)
	}
	return resolved, nil
}

func rootTranscripts(root string) ([]string, error) {
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, project := range projects {
		if !project.IsDir() || project.Type()&os.ModeSymlink != 0 {
			continue
		}
		dir := filepath.Join(root, project.Name())
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".jsonl" {
				continue
			}
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func splitQuery(value string) []string {
	var out []string
	var current strings.Builder
	quoted := false
	for _, r := range value {
		switch {
		case r == '"':
			quoted = !quoted
		case unicode.IsSpace(r) && !quoted:
			if current.Len() > 0 {
				out = append(out, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		out = append(out, current.String())
	}
	return out
}

func splitFilter(token string) (string, string, bool) {
	index := strings.IndexByte(token, ':')
	if index <= 0 || index == len(token)-1 {
		return "", "", false
	}
	return strings.ToLower(token[:index]), token[index+1:], true
}

func parseBound(value string, now time.Time) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if len(value) > 1 && strings.HasSuffix(value, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(value, "d"))
		if err == nil && days > 0 && days <= 3650 {
			return now.Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("use RFC3339, YYYY-MM-DD, or Nd")
}

func eventDocument(event threadgraph.Event) string {
	raw, _ := json.Marshal(event.Raw)
	return event.Summary + "\n" + event.Content + "\n" + event.ToolName + "\n" + string(raw)
}

func evidenceSnippet(event threadgraph.Event, terms []string, file string) string {
	doc := strings.Join([]string{event.Content, event.Summary, event.ToolName, eventDocument(event)}, "\n")
	if file != "" && containsFold(doc, file) {
		return windowAround(doc, file, maxSnippet)
	}
	for _, term := range terms {
		if containsFold(doc, term) {
			return windowAround(doc, term, maxSnippet)
		}
	}
	return trimDisplay(event.Summary, maxSnippet)
}

func windowAround(value, needle string, width int) string {
	lower := strings.ToLower(value)
	index := strings.Index(lower, strings.ToLower(needle))
	if index < 0 {
		return trimDisplay(value, width)
	}
	runes := []rune(value)
	startRune := utf8.RuneCountInString(value[:index])
	needleRunes := len([]rune(needle))
	available := max(0, width-needleRunes)
	left := available / 2
	right := available - left
	start := max(0, startRune-left)
	end := min(len(runes), startRune+needleRunes+right)
	prefix, suffix := "", ""
	if start > 0 {
		prefix = "…"
	}
	if end < len(runes) {
		suffix = "…"
	}
	return prefix + trimDisplay(string(runes[start:end]), width) + suffix
}

func isBoilerplateTitle(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return lower == "" || strings.HasPrefix(lower, "you are a persistent") || strings.HasPrefix(lower, "you are the") || strings.HasPrefix(lower, "# claude x")
}

func mayContainAny(line []byte, terms []string, file string) bool {
	if len(terms) == 0 && file == "" {
		return false
	}
	lower := strings.ToLower(string(line))
	if file != "" && strings.Contains(lower, strings.ToLower(file)) {
		return true
	}
	for _, term := range terms {
		if strings.Contains(lower, strings.ToLower(term)) {
			return true
		}
	}
	return false
}

func allFound(found map[string]bool, terms []string) bool {
	for _, term := range terms {
		if !found[term] {
			return false
		}
	}
	return true
}

func scoreMatch(match Match, query parsedQuery) int {
	score := len(query.terms) * 10
	if query.File != "" {
		score += 80
	}
	if query.Project != "" {
		score += 20
	}
	for _, term := range query.terms {
		if containsFold(match.Title, term) {
			score += 8
		}
	}
	return score
}

func cleanTerm(value string) string {
	return strings.TrimSpace(strings.Trim(value, "\"'"))
}

func compactTerms(values []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, value := range values {
		value = cleanTerm(value)
		key := strings.ToLower(value)
		if value == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}

func decodeProject(encoded string) string {
	encoded = strings.Trim(encoded, "-")
	parts := strings.Split(encoded, "-")
	if len(parts) == 0 {
		return encoded
	}
	return parts[len(parts)-1]
}

func projectMatchesEncoded(encoded, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	for _, segment := range strings.Split(strings.ToLower(strings.Trim(encoded, "-")), "-") {
		if segment == query {
			return true
		}
	}
	// Multi-character repository names may be supplied as a distinctive
	// fragment. One-character filters must match a whole path segment.
	return len([]rune(query)) >= 3 && containsFold(encoded, query)
}

func projectMatchesPath(path, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	for _, segment := range strings.Split(strings.ToLower(clean), "/") {
		if segment == query {
			return true
		}
	}
	return len([]rune(query)) >= 3 && containsFold(clean, query)
}

func trimDisplay(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max]) + "…"
}

func containsFold(value, query string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(query))
}
