package supervisorgate

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode"
)

// bashReadOnly allows a narrow whitelist of read-only shell forms during handoff.
// Fail closed: unknown shape → not read-only.
func bashReadOnly(raw json.RawMessage) bool {
	cmd := bashCommand(raw)
	if cmd == "" {
		return false
	}
	lower := strings.ToLower(cmd)
	for _, bad := range []string{
		">", ">>", "<<", "$(", "`", "&&", "||", ";", "|",
		"\n", "\r", "tee ", " wrangler", "npm ", "npx ", "pnpm ", "yarn ",
		"git commit", "git push", "git reset", "rm ", "mv ", "cp ", "chmod ",
		"curl ", "wget ", "python ", "python3 ", "node ", "go run", "go test",
		"make ", "deploy", "dd ", "truncate",
	} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	switch fields[0] {
	case "ls", "pwd", "true":
		return true
	case "cat", "head", "tail", "wc", "file", "stat", "rg", "grep":
		return len(fields) >= 2
	default:
		return false
	}
}

// bashIsDestructiveGit detects hard-denied git operations on normal Roots.
// Uses argv-style parsing so global options (-C, -c, …) and flag position
// (e.g. git push origin main --force / -f) cannot bypass the gate.
func bashIsDestructiveGit(raw json.RawMessage) bool {
	cmd := bashCommand(raw)
	if cmd == "" {
		return false
	}
	for _, seg := range splitShellSegments(cmd) {
		if gitSegmentDestructive(seg) {
			return true
		}
	}
	return false
}

// splitShellSegments splits a shell command on top-level chain operators.
// Quote-aware enough for common agent commands; fail-closed on oddities by
// still scanning the unsplit form as a single segment.
func splitShellSegments(cmd string) []string {
	var segs []string
	var b strings.Builder
	inSingle, inDouble, escaped := false, false, false
	flush := func() {
		s := strings.TrimSpace(b.String())
		if s != "" {
			segs = append(segs, s)
		}
		b.Reset()
	}
	runes := []rune(cmd)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && !inSingle {
			escaped = true
			b.WriteRune(r)
			continue
		}
		if r == '\'' && !inDouble {
			inSingle = !inSingle
			b.WriteRune(r)
			continue
		}
		if r == '"' && !inSingle {
			inDouble = !inDouble
			b.WriteRune(r)
			continue
		}
		if !inSingle && !inDouble {
			if r == '\n' || r == '\r' || r == ';' {
				flush()
				continue
			}
			if r == '|' || r == '&' {
				// Treat |, ||, &, && as separators.
				flush()
				// skip doubled operators
				if i+1 < len(runes) && runes[i+1] == r {
					i++
				}
				continue
			}
		}
		b.WriteRune(r)
	}
	flush()
	if len(segs) == 0 {
		return []string{strings.TrimSpace(cmd)}
	}
	return segs
}

func gitSegmentDestructive(seg string) bool {
	fields := shellFields(seg)
	if len(fields) == 0 {
		return false
	}
	// Strip leading VAR=value assignments, then peel env/command wrappers so
	// `env git reset --hard` and `command git push -f` cannot bypass the gate.
	fields = stripLeadingAssignments(fields)
	fields = unwrapExecWrappers(fields)
	if len(fields) == 0 {
		return false
	}
	base := filepath.Base(fields[0])
	if base != "git" && base != "git.exe" {
		return false
	}
	sub, rest := parseGitArgv(fields[1:])
	switch sub {
	case "reset":
		for _, a := range rest {
			if a == "--hard" || a == "-hard" {
				return true
			}
		}
	case "push":
		for _, a := range rest {
			if a == "--force" || a == "-f" || a == "--force-with-lease" ||
				strings.HasPrefix(a, "--force-with-lease=") ||
				strings.HasPrefix(a, "--force=") {
				return true
			}
		}
	case "clean":
		// git clean -fd / -fx is also destructive; deny force-style cleans.
		for _, a := range rest {
			if a == "-f" || a == "-fd" || a == "-fx" || a == "-ff" ||
				a == "--force" || strings.HasPrefix(a, "-") && strings.Contains(a, "f") && !strings.HasPrefix(a, "--") {
				return true
			}
		}
	}
	return false
}

func stripLeadingAssignments(fields []string) []string {
	i := 0
	for i < len(fields) {
		f := fields[i]
		// FOO=bar style; not -flag=value and not git itself.
		eq := strings.IndexByte(f, '=')
		if eq > 0 && !strings.HasPrefix(f, "-") && filepath.Base(f) != "git" && filepath.Base(f) != "git.exe" {
			i++
			continue
		}
		break
	}
	return fields[i:]
}

// unwrapExecWrappers peels env(1) and bash `command` wrappers (possibly nested).
func unwrapExecWrappers(fields []string) []string {
	for len(fields) > 0 {
		base := filepath.Base(fields[0])
		switch base {
		case "env":
			// env [-i] [-u name]... [NAME=value]... utility [argument...]
			rest := fields[1:]
			for len(rest) > 0 {
				f := rest[0]
				if f == "-i" || f == "-" || f == "--ignore-environment" || f == "--" {
					rest = rest[1:]
					continue
				}
				if f == "-u" || f == "--unset" || f == "-P" || f == "-S" || f == "-C" || f == "--chdir" || f == "--split-string" {
					if len(rest) >= 2 {
						rest = rest[2:]
					} else {
						rest = rest[1:]
					}
					continue
				}
				if strings.HasPrefix(f, "--unset=") || strings.HasPrefix(f, "--chdir=") || strings.HasPrefix(f, "-u") && len(f) > 2 {
					rest = rest[1:]
					continue
				}
				if strings.HasPrefix(f, "-") && f != "-" {
					// unknown env flag (single token); skip conservatively
					rest = rest[1:]
					continue
				}
				eq := strings.IndexByte(f, '=')
				if eq > 0 {
					rest = rest[1:]
					continue
				}
				break // utility
			}
			if len(rest) == 0 || len(rest) >= len(fields) {
				return fields // no utility / no progress
			}
			fields = rest
			continue
		case "command":
			// command [-pVv] name [arg ...]
			rest := fields[1:]
			for len(rest) > 0 {
				f := rest[0]
				if f == "-p" || f == "-v" || f == "-V" {
					rest = rest[1:]
					continue
				}
				if strings.HasPrefix(f, "-") {
					rest = rest[1:]
					continue
				}
				break
			}
			if len(rest) == 0 {
				return fields
			}
			fields = rest
			continue
		default:
			return fields
		}
	}
	return fields
}

// parseGitArgv skips git global options and returns (subcommand, remaining args).
func parseGitArgv(args []string) (sub string, rest []string) {
	// Global options that take a separate value.
	takesValue := map[string]bool{
		"-C": true, "-c": true, "--git-dir": true, "--work-tree": true,
		"--namespace": true, "--config-env": true, "-o": true,
	}
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "" {
			i++
			continue
		}
		if a == "--" {
			i++
			break
		}
		if takesValue[a] {
			i += 2
			continue
		}
		// -Cpath style is rare; handle -c key=value already as single token via =
		if strings.HasPrefix(a, "-C") && len(a) > 2 {
			i++
			continue
		}
		if strings.HasPrefix(a, "--git-dir=") || strings.HasPrefix(a, "--work-tree=") ||
			strings.HasPrefix(a, "--namespace=") || strings.HasPrefix(a, "--config-env=") {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			// bare global flag (-p, --no-pager, --bare, …)
			i++
			continue
		}
		// first non-option token is the subcommand
		return a, args[i+1:]
	}
	return "", nil
}

// shellFields is a lightweight field splitter: whitespace outside simple quotes.
func shellFields(s string) []string {
	var out []string
	var b strings.Builder
	inSingle, inDouble, escaped := false, false, false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		if escaped {
			b.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && !inSingle {
			escaped = true
			continue
		}
		if r == '\'' && !inDouble {
			inSingle = !inSingle
			continue
		}
		if r == '"' && !inSingle {
			inDouble = !inDouble
			continue
		}
		if !inSingle && !inDouble && unicode.IsSpace(r) {
			flush()
			continue
		}
		b.WriteRune(r)
	}
	flush()
	return out
}

// bashClass classifies Bash for high-cost / budget counting (T11 partial).
// Returns: readonly | test | deploy | mutate | other
func bashClass(raw json.RawMessage) string {
	cmd := strings.ToLower(bashCommand(raw))
	if cmd == "" {
		return "other"
	}
	if bashReadOnly(raw) {
		return "readonly"
	}
	if strings.Contains(cmd, "wrangler deploy") || strings.Contains(cmd, "npm run deploy") ||
		strings.Contains(cmd, "cloudflare deploy") || strings.Contains(cmd, " production deploy") {
		return "deploy"
	}
	if strings.Contains(cmd, "go test") || strings.Contains(cmd, "npm test") ||
		strings.Contains(cmd, "npm run test") || strings.Contains(cmd, "npx vitest") ||
		strings.Contains(cmd, "pytest") {
		return "test"
	}
	if strings.Contains(cmd, ">") || strings.Contains(cmd, ">>") || strings.Contains(cmd, "git commit") ||
		strings.Contains(cmd, "rm ") || strings.Contains(cmd, "mv ") {
		return "mutate"
	}
	return "other"
}

func bashCommand(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err == nil {
		for _, key := range []string{"command", "cmd"} {
			if v, ok := obj[key].(string); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

func toolPathFromInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	for _, key := range []string{"file_path", "path", "filePath"} {
		if v, ok := obj[key].(string); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
