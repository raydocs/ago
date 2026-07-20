package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The packaging gate: does `ago-server demo` work on a machine that has never
// run Ago before?
//
// It runs from a fresh temporary HOME with no configuration and no credential,
// against the binary as a user would get it. What it proves is narrow and
// specific: the command reaches a served board that completes, and it writes
// nothing outside the directory it said it would use. Everything about the
// quality of the work is covered elsewhere; this is about the first five
// minutes.
func TestDemoRunsFromAFreshHome(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a server")
	}
	for _, tool := range []string{"git", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable", tool)
		}
	}
	binary := buildServer(t)

	home := t.TempDir()
	// Anything written outside HOME is the bug this checks for, so HOME is the
	// only writable location the run is told about.
	state := filepath.Join(home, ".ago", "demo")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "demo", "--executor", "fake", "--listen", "127.0.0.1:0")
	command.Env = append(os.Environ(), "HOME="+home)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	// The whole process group goes, so a supervisor goroutine or a spawned
	// check cannot outlive the test.
	defer func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		_ = command.Wait()
	}()

	address, transcript := readAddress(t, stdout)
	t.Logf("startup transcript:\n%s", transcript)
	if !strings.Contains(transcript, "示例仓库已创建") {
		t.Errorf("the run did not report creating the sample repository:\n%s", transcript)
	}
	// A user must not be able to mistake the offline mode for a model run.
	if !strings.Contains(transcript, "no model decides anything") {
		t.Errorf("fake mode did not say its plan is scripted:\n%s", transcript)
	}

	board := waitForCompletion(t, ctx, address)
	if progress, ok := board["progress"].(map[string]any); !ok || progress["passed"] == float64(0) {
		t.Fatalf("the demo board completed with nothing passed: %v", board["progress"])
	}
	// Zero decisions is the product claim: after the goal, nobody was asked
	// anything.
	decisions := getJSON(t, ctx, "http://"+address+"/api/v1/boards/ago-demo/decisions")
	if list, _ := decisions["decisions"].([]any); len(list) != 0 {
		t.Fatalf("the demo needed %d human decisions, want zero: %v", len(list), list)
	}

	// Everything it created is under the directory it announced.
	if _, err := os.Stat(filepath.Join(state, "ago.db")); err != nil {
		t.Fatalf("the database is not where the run said it would be: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "greeter", "go.mod")); err != nil {
		t.Fatalf("the sample repository is not a real Go module: %v", err)
	}
	for _, stray := range strayEntries(t, home) {
		t.Errorf("the demo wrote outside its own state directory: %s", stray)
	}
}

// Preflight must fail before anything is created, with a sentence naming what
// to change — not with a runtime error several minutes in.
func TestRelayDemoRefusesWithoutACredential(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	binary := buildServer(t)
	home := t.TempDir()

	command := exec.Command(binary, "demo", "--executor", "relay", "--listen", "127.0.0.1:0")
	// A relay endpoint with no key: the missing credential is the point.
	command.Env = append(os.Environ(), "HOME="+home,
		"AGO_RELAY_BASE_URL=http://127.0.0.1:1/v1", "AGO_RELAY_API_KEY=")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("relay mode started without a credential:\n%s", output)
	}
	if !strings.Contains(string(output), "AGO_RELAY_API_KEY") {
		t.Fatalf("the refusal does not name the variable to set:\n%s", output)
	}
	// Nothing was created on the way to failing.
	if _, err := os.Stat(filepath.Join(home, ".ago", "demo", "greeter")); err == nil {
		t.Fatal("a failed preflight still created the sample repository")
	}
}

func buildServer(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "ago-server")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ago-server: %v\n%s", err, output)
	}
	return binary
}

var addressPattern = regexp.MustCompile(`UI:\s+http://(\S+)`)

func readAddress(t *testing.T, stdout interface{ Read([]byte) (int, error) }) (string, string) {
	t.Helper()
	var transcript strings.Builder
	var address string
	scanner := bufio.NewScanner(stdout)
	deadline := time.Now().Add(90 * time.Second)
	for scanner.Scan() {
		line := scanner.Text()
		transcript.WriteString(line + "\n")
		if match := addressPattern.FindStringSubmatch(line); match != nil {
			address = match[1]
		}
		// The announce line is the last thing printed at startup, so waiting
		// for it means the transcript is the whole startup block rather than
		// however much of it happened to be flushed.
		if address != "" && strings.Contains(line, "打开看板") {
			// Keep draining so the child never blocks on a full pipe.
			go func() {
				for scanner.Scan() {
				}
			}()
			return address, transcript.String()
		}
		if time.Now().After(deadline) {
			break
		}
	}
	t.Fatalf("the server never printed a listen address:\n%s", transcript.String())
	return "", ""
}

func waitForCompletion(t *testing.T, ctx context.Context, address string) map[string]any {
	t.Helper()
	url := "http://" + address + "/api/v1/boards/ago-demo"
	deadline := time.Now().Add(2 * time.Minute)
	var board map[string]any
	for time.Now().Before(deadline) {
		board = getJSON(t, ctx, url)
		if completed, _ := board["completed"].(bool); completed {
			return board
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the demo board never completed: %v", board["progress"])
	return nil
}

func getJSON(t *testing.T, ctx context.Context, url string) map[string]any {
	t.Helper()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
	return decoded
}

// strayEntries reports anything under HOME that is not the announced state
// directory. Go's own caches are excluded: they belong to the toolchain the
// test itself invoked, not to Ago.
func strayEntries(t *testing.T, home string) []string {
	t.Helper()
	var stray []string
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case ".ago", "go", ".cache", "Library", ".config":
			continue
		}
		stray = append(stray, fmt.Sprintf("%s (%s)", entry.Name(), entryKind(entry)))
	}
	return stray
}

func entryKind(entry os.DirEntry) string {
	if entry.IsDir() {
		return "directory"
	}
	return "file"
}
