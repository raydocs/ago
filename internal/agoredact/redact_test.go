package agoredact_test

import (
	"io"
	"strings"
	"testing"

	"claudexflow/internal/agoredact"
)

const sentinel = "sk-ant-SENTINELsecretVALUE0123456789"

func TestCredentialShapesAreRemoved(t *testing.T) {
	redactor := agoredact.New()
	for name, input := range map[string]string{
		"authorization header": `Authorization: Bearer abcdef0123456789abcdef`,
		"api key assignment":   `api_key=abcdef0123456789`,
		"api key json":         `{"api_key": "abcdef0123456789"}`,
		"api key dash":         `api-key: abcdef0123456789`,
		"password env":         `PASSWORD=hunter2hunter2`,
		"token flag":           `--token abcdef0123456789abcdef`,
		"url credentials":      `https://alice:s3cr3tpassword@example.com/repo.git`,
		"query credential":     `https://example.com/v1?api_key=abcdef0123456789&x=1`,
		"openai key":           `sk-abcdefghijklmnopqrstuvwxyz0123`,
		"anthropic key":        `sk-ant-abcdefghijklmnopqrstuvwxyz`,
		"github token":         `ghp_abcdefghijklmnopqrstuvwxyz0123`,
		"aws key id":           `AKIAIOSFODNN7EXAMPLE`,
		"private key":          `-----BEGIN RSA PRIVATE KEY-----`,
		"client secret":        `client_secret: abcdef0123456789`,
		"set-cookie":           `Set-Cookie: session=abcdef0123456789`,
	} {
		t.Run(name, func(t *testing.T) {
			got := redactor.String(input)
			if !strings.Contains(got, agoredact.Placeholder) {
				t.Fatalf("nothing was redacted in %q: %q", input, got)
			}
			// The secret-looking run must be gone.
			for _, leak := range []string{"abcdef0123456789", "hunter2hunter2", "s3cr3tpassword", "IOSFODNN7EXAMPLE"} {
				if strings.Contains(got, leak) {
					t.Fatalf("redacted %q still contains %q", input, leak)
				}
			}
		})
	}
}

// An exact literal is removed even with no recognisable key around it, which is
// what happens when a process simply echoes an environment value.
func TestExactLiteralsAreRemovedWithoutAnySurroundingKey(t *testing.T) {
	redactor := agoredact.New(sentinel)
	for _, input := range []string{
		sentinel,
		"值是 " + sentinel + " 结束",
		"prefix" + sentinel + "suffix",
		strings.Repeat("padding ", 100) + sentinel,
	} {
		got := redactor.String(input)
		if strings.Contains(got, sentinel) {
			t.Fatalf("sentinel survived redaction in %q: %q", input, got)
		}
	}
}

func TestShortLiteralsAreIgnoredSoOrdinaryTextSurvives(t *testing.T) {
	redactor := agoredact.New("ab")
	got := redactor.String("这是一段普通的中文文本，about 常规内容。")
	if strings.Contains(got, agoredact.Placeholder) {
		t.Fatalf("a two-character literal destroyed ordinary text: %q", got)
	}
}

func TestEnvironmentSeededRedactorRemovesProviderValues(t *testing.T) {
	values := map[string]string{
		"AGO_PROVIDER_API_KEY": sentinel,
		"ANTHROPIC_API_KEY":    "sk-ant-another-secret-value-here",
	}
	redactor := agoredact.NewFromEnvironment(func(name string) string { return values[name] })
	got := redactor.String("provider responded with " + sentinel + " and " + values["ANTHROPIC_API_KEY"])
	for _, secret := range values {
		if strings.Contains(got, secret) {
			t.Fatalf("environment-seeded redaction missed %q: %q", secret, got)
		}
	}
}

func TestReaderRedactsAStreamWithoutBufferingItAll(t *testing.T) {
	redactor := agoredact.New(sentinel)
	var builder strings.Builder
	for range 2000 {
		builder.WriteString("ordinary log line without anything interesting\n")
	}
	builder.WriteString("leaked: " + sentinel + "\n")
	builder.WriteString("Authorization: Bearer abcdef0123456789abcdef\n")

	got, err := io.ReadAll(redactor.Reader(strings.NewReader(builder.String())))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), sentinel) {
		t.Fatal("the streaming redactor let the sentinel through")
	}
	if strings.Contains(string(got), "abcdef0123456789abcdef") {
		t.Fatal("the streaming redactor let a bearer token through")
	}
	if !strings.Contains(string(got), "ordinary log line") {
		t.Fatal("the streaming redactor destroyed ordinary content")
	}
}

// A consumer that stops reading must not leave the redactor's goroutine blocked
// on a pipe write forever.
func TestReaderStopsWhenTheConsumerGoesAway(t *testing.T) {
	redactor := agoredact.New()
	var builder strings.Builder
	for range 50_000 {
		builder.WriteString("line of output that nobody will read to the end\n")
	}
	reader := redactor.Reader(strings.NewReader(builder.String()))
	buffer := make([]byte, 16)
	if _, err := reader.Read(buffer); err != nil {
		t.Fatal(err)
	}
	if closer, ok := reader.(io.Closer); ok {
		if err := closer.Close(); err != nil {
			t.Fatal(err)
		}
	}
	// If the writer goroutine were stuck, the race detector and the test
	// timeout would surface it; reaching here with the pipe closed is the
	// observable contract.
}

func TestStringsRedactsEveryElement(t *testing.T) {
	redactor := agoredact.New(sentinel)
	got := redactor.Strings([]string{"clean", "leaked " + sentinel, "api_key=abcdef0123456789"})
	if len(got) != 3 || got[0] != "clean" {
		t.Fatalf("Strings = %#v", got)
	}
	if strings.Contains(got[1], sentinel) || !strings.Contains(got[2], agoredact.Placeholder) {
		t.Fatalf("Strings did not redact every element: %#v", got)
	}
}

func TestEmptyInputIsUnchanged(t *testing.T) {
	if got := agoredact.New(sentinel).String(""); got != "" {
		t.Fatalf("String(\"\") = %q", got)
	}
	if got := agoredact.New().Strings(nil); got != nil {
		t.Fatalf("Strings(nil) = %#v", got)
	}
}
