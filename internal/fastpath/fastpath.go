// Package fastpath conservatively recognizes small deterministic code changes
// that should not pay the full Supervisor routing lifecycle.
package fastpath

import (
	"fmt"
	"regexp"
	"strings"
)

var codePath = regexp.MustCompile(`(?i)(?:^|[\s` + "`" + `"'(])([./~A-Za-z0-9_-]+\.(?:go|rs|py|js|jsx|ts|tsx|mjs|cjs|java|kt|swift|rb|php|sh|bash|zsh|yaml|yml|json|toml|md))(?:$|[\s` + "`" + `"'),:])`)

var verifierPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:^|\b)(go\s+test\s+[^\n,;` + "`" + `]+)`),
	regexp.MustCompile(`(?i)(?:^|\b)(pytest(?:\s+[^\n,;` + "`" + `]+)?)`),
	regexp.MustCompile(`(?i)(?:^|\b)(npm\s+(?:run\s+test|test)(?:\s+[^\n,;` + "`" + `]+)?)`),
	regexp.MustCompile(`(?i)(?:^|\b)(npx\s+vitest(?:\s+[^\n,;` + "`" + `]+)?)`),
}

type Contract struct {
	TargetPath string
	Verifier   string
}

// Detect returns true only for a single-file implementation request with an
// explicit deterministic verification cue. Ambiguous or cross-domain work
// remains on the normal workflow.
func Detect(prompt string) bool {
	_, ok := Parse(prompt)
	return ok
}

// Parse returns the exact file and verifier that define a Fast Path turn.
// Requiring an explicit command keeps the hard gate conservative: generic
// "test it" requests remain on the normal workflow.
func Parse(prompt string) (Contract, bool) {
	p := strings.ToLower(strings.TrimSpace(prompt))
	if p == "" {
		return Contract{}, false
	}
	for _, blocked := range []string{
		"deploy", "migration", "schema", "database", "ui", "dashboard",
		"research", "search the web", "browse", "architecture", "refactor repository",
	} {
		if strings.Contains(p, blocked) {
			return Contract{}, false
		}
	}
	if !(strings.Contains(p, "fix") || strings.Contains(p, "change") || strings.Contains(p, "update") || strings.Contains(p, "modify") || strings.Contains(p, "implement")) {
		return Contract{}, false
	}
	matches := codePath.FindAllStringSubmatch(prompt, -1)
	seen := map[string]bool{}
	for _, match := range matches {
		if len(match) > 1 {
			seen[strings.TrimSpace(match[1])] = true
		}
	}
	if len(seen) != 1 {
		return Contract{}, false
	}
	verifier := extractVerifier(prompt)
	if verifier == "" || !SafeVerifier(verifier) {
		return Contract{}, false
	}
	var target string
	for path := range seen {
		target = path
	}
	return Contract{TargetPath: target, Verifier: verifier}, true
}

func extractVerifier(prompt string) string {
	for _, pattern := range verifierPatterns {
		match := pattern.FindStringSubmatch(prompt)
		if len(match) > 1 {
			// Dots are part of valid Go package patterns such as ./... and must
			// never be stripped from the frozen command.
			return strings.TrimSpace(strings.Trim(match[1], " \t\r\n\"'"))
		}
	}
	return ""
}

// SafeVerifier accepts one foreground test command only. Shell composition can
// mask a failing exit status (for example `go test ./... || true`) and therefore
// must never arm VerifiedStop.
func SafeVerifier(command string) bool {
	command = strings.TrimSpace(command)
	if command == "" || strings.ContainsAny(command, "\n\r;|&><`") || strings.Contains(command, "$(") {
		return false
	}
	lower := strings.ToLower(command)
	return strings.HasPrefix(lower, "go test ") || lower == "pytest" || strings.HasPrefix(lower, "pytest ") ||
		lower == "npm test" || strings.HasPrefix(lower, "npm test ") || lower == "npm run test" ||
		strings.HasPrefix(lower, "npm run test ") || lower == "npx vitest" || strings.HasPrefix(lower, "npx vitest ")
}

func Context(contract Contract) string {
	return fmt.Sprintf("CLAUDEX_FAST_PATH v3: target=%q verifier=%q. This is a conservative single-file deterministic slice. Skip route_task, gates, Worker, specialists, browser, and broad shell commands. Read and edit only the target. If final inspection is needed, use Read for a new file or exactly `git diff -- %s` once for an existing tracked file; never combine it with the verifier. Run the exact verifier once after the edit, then report immediately.", contract.TargetPath, contract.Verifier, contract.TargetPath)
}
