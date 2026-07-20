// Package agoredact removes credential-shaped content before it becomes
// durable.
//
// It is applied at the boundary where untrusted executor output turns into
// something a user or an operator can read: evidence, logs, and artifacts. The
// rule is deliberately blunt — it prefers redacting something harmless over
// letting a credential reach SQLite, an artifact file, an HTTP response, or a
// browser. Redaction is not a substitute for never putting secrets in these
// places; it is the last line.
package agoredact

import (
	"bufio"
	"io"
	"regexp"
	"strings"
)

// Placeholder replaces every redacted value. It is deliberately recognisable so
// a reader can tell redaction happened rather than wondering about empty output.
const Placeholder = "[REDACTED]"

// maxScannedLine bounds how much of a single line is examined, so a pathological
// input cannot make redaction quadratic.
const maxScannedLine = 64 * 1024

// secretKeys are the names whose values are always removed, whatever syntax
// carries them.
var secretKeys = []string{
	"authorization", "proxy-authorization",
	"api_key", "api-key", "apikey",
	"access_token", "refresh_token", "id_token", "token",
	"password", "passwd", "secret", "client_secret",
	"session", "cookie", "set-cookie",
	"private_key", "privatekey",
}

var patterns = []*regexp.Regexp{
	// Authorization: Bearer <value>, and bare bearer tokens.
	regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9._\-+/=]{8,}`),
	// key="value" / key: value / key=value, in JSON, YAML, env, and CLI forms.
	regexp.MustCompile(`(?i)\b(` + strings.Join(secretKeys, "|") + `)\b(\s*[:=]\s*|"\s*:\s*)"?[^"'\s,;}&]{4,}"?`),
	// Credentials embedded in a URL: scheme://user:password@host
	regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.\-]*://)[^/\s:@]+:[^/\s@]+@`),
	// Query-string credentials.
	regexp.MustCompile(`(?i)([?&](?:` + strings.Join(secretKeys, "|") + `)=)[^&\s]+`),
	// Common provider key shapes, which are recognisable on their own.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
}

// EnvironmentNames are the environment variables whose values must never reach
// durable output. Callers pass their values to Redactor so exact matches are
// removed even when they appear without a recognisable key.
var EnvironmentNames = []string{
	"AGO_PROVIDER_API_KEY",
	"ANTHROPIC_API_KEY",
	"OPENAI_API_KEY",
	"CLAUDE_CODE_OAUTH_TOKEN",
	"GITHUB_TOKEN",
	"GH_TOKEN",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
}

// Redactor removes credential-shaped content, plus any exact literal it was
// given. Literals cover the case where a value has no recognisable key around
// it, which is exactly what happens when a process echoes an environment value.
type Redactor struct {
	literals []string
}

// New builds a redactor that also removes these exact values. Short values are
// ignored: redacting a two-character literal would destroy ordinary text
// without protecting anything real.
func New(literals ...string) *Redactor {
	kept := make([]string, 0, len(literals))
	for _, literal := range literals {
		if len(strings.TrimSpace(literal)) >= 8 {
			kept = append(kept, literal)
		}
	}
	return &Redactor{literals: kept}
}

// NewFromEnvironment builds a redactor seeded with the current values of the
// known provider variables.
func NewFromEnvironment(lookup func(string) string) *Redactor {
	literals := make([]string, 0, len(EnvironmentNames))
	for _, name := range EnvironmentNames {
		if value := lookup(name); value != "" {
			literals = append(literals, value)
		}
	}
	return New(literals...)
}

// String redacts one value.
func (r *Redactor) String(value string) string {
	if value == "" {
		return value
	}
	// Exact literals first: a known secret is removed even if it also happens
	// to match a pattern, so the placeholder is not left holding a fragment.
	for _, literal := range r.literals {
		if strings.Contains(value, literal) {
			value = strings.ReplaceAll(value, literal, Placeholder)
		}
	}
	if len(value) > maxScannedLine {
		return r.redactLong(value)
	}
	return applyPatterns(value)
}

func (r *Redactor) redactLong(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for start := 0; start < len(value); start += maxScannedLine {
		end := min(start+maxScannedLine, len(value))
		builder.WriteString(applyPatterns(value[start:end]))
	}
	return builder.String()
}

func applyPatterns(value string) string {
	for index, pattern := range patterns {
		switch index {
		case 1:
			// Keep the key so the reader knows what was removed.
			value = pattern.ReplaceAllString(value, "$1="+Placeholder)
		case 2:
			value = pattern.ReplaceAllString(value, "${1}"+Placeholder+"@")
		case 3:
			value = pattern.ReplaceAllString(value, "${1}"+Placeholder)
		default:
			value = pattern.ReplaceAllString(value, Placeholder)
		}
	}
	return value
}

// Strings redacts every element, returning a new slice.
func (r *Redactor) Strings(values []string) []string {
	if values == nil {
		return nil
	}
	out := make([]string, len(values))
	for index, value := range values {
		out[index] = r.String(value)
	}
	return out
}

// Reader wraps a stream so content is redacted as it is copied. It works line
// by line, which bounds memory regardless of how much an executor produces.
func (r *Redactor) Reader(source io.Reader) io.Reader {
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		scanner := bufio.NewScanner(source)
		scanner.Buffer(make([]byte, 0, 64*1024), maxScannedLine)
		for scanner.Scan() {
			if _, err := pipeWriter.Write([]byte(r.String(scanner.Text()) + "\n")); err != nil {
				// The consumer went away; stop rather than block forever.
				_ = pipeReader.CloseWithError(err)
				return
			}
		}
		_ = pipeWriter.CloseWithError(scanner.Err())
	}()
	return pipeReader
}
