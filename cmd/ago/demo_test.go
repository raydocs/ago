package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// These tests run the binary a user would actually have.
//
// `go build -o ago ./cmd/ago` is the whole installation story, so every claim
// here is made against that binary — not against a function call, and not with
// a second binary quietly on PATH.

func TestDemoIsReachableFromTheUnifiedCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a server")
	}
	requireTools(t)
	binary := buildAgo(t)

	home := t.TempDir()
	state := filepath.Join(home, ".ago", "demo")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "demo", "--executor", "fake", "--listen", "127.0.0.1:0")
	// PATH carries only what the demo genuinely needs. If `ago demo` were
	// shelling out to an ago-server binary, it could not find one here — which
	// is exactly the claim this makes: the unified CLI links the stack in.
	command.Env = append(minimalEnv(t), "HOME="+home)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
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
	decisions := getJSON(t, ctx, "http://"+address+"/api/v1/boards/ago-demo/decisions")
	if list, _ := decisions["decisions"].([]any); len(list) != 0 {
		t.Fatalf("the demo needed %d human decisions, want zero: %v", len(list), list)
	}
	for _, path := range []string{"ago.db", filepath.Join("greeter", "go.mod"), ".ago-demo-state"} {
		if _, err := os.Stat(filepath.Join(state, path)); err != nil {
			t.Errorf("the demo did not create %s: %v", path, err)
		}
	}
	for _, stray := range strayEntries(t, home) {
		t.Errorf("the demo wrote outside its own state directory: %s", stray)
	}
}

// Ctrl+C, then run the same command again: the board must come back rather than
// start over.
func TestDemoResumesAnExistingBoard(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a server")
	}
	requireTools(t)
	binary := buildAgo(t)
	home := t.TempDir()

	first := startDemo(t, binary, home)
	_, transcript := readAddress(t, first.stdout)
	if !strings.Contains(transcript, "示例仓库已创建") {
		t.Fatalf("the first run did not create the repository:\n%s", transcript)
	}
	first.interrupt(t)

	second := startDemo(t, binary, home)
	defer second.kill()
	_, resumed := readAddress(t, second.stdout)
	t.Logf("second run:\n%s", resumed)
	if !strings.Contains(resumed, "沿用已有的示例仓库") {
		t.Errorf("the second run did not reuse the sample repository:\n%s", resumed)
	}
	if !strings.Contains(resumed, "看板已存在") {
		t.Errorf("the second run did not resume the existing board:\n%s", resumed)
	}
}

// Preflight must fail before anything at all is created, naming the variable to
// set. The list of things that must not exist afterwards is the point: a run
// that got far enough to write a database or claim a directory has already done
// damage a user would have to clean up.
func TestRelayDemoRefusesWithoutACredentialBeforeCreatingAnything(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	binary := buildAgo(t)
	home := t.TempDir()

	command := exec.Command(binary, "demo", "--executor", "relay", "--listen", "127.0.0.1:0")
	command.Env = append(minimalEnv(t), "HOME="+home,
		"AGO_RELAY_BASE_URL=http://127.0.0.1:1/v1", "AGO_RELAY_API_KEY=")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("relay mode started without a credential:\n%s", output)
	}
	if !strings.Contains(string(output), "AGO_RELAY_API_KEY") {
		t.Fatalf("the refusal does not name the variable to set:\n%s", output)
	}
	state := filepath.Join(home, ".ago", "demo")
	for _, path := range []string{"greeter", "ago.db", "artifacts", "worktrees", "integration", ".ago-demo-state"} {
		if _, err := os.Lstat(filepath.Join(state, path)); err == nil {
			t.Errorf("a failed preflight created %s", path)
		}
	}
}

// An unreachable endpoint is the other half: the credential is present but the
// relay does not answer. Nothing may be created on the way to finding out.
func TestRelayDemoRefusesAnUnreachableEndpointBeforeCreatingAnything(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	binary := buildAgo(t)
	home := t.TempDir()

	command := exec.Command(binary, "demo", "--executor", "relay", "--listen", "127.0.0.1:0")
	command.Env = append(minimalEnv(t), "HOME="+home,
		"AGO_RELAY_BASE_URL=http://127.0.0.1:1/v1", "AGO_RELAY_API_KEY=not-a-real-key-0123456789")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("relay mode started against an unreachable endpoint:\n%s", output)
	}
	// The credential must not be echoed back in the failure.
	if strings.Contains(string(output), "not-a-real-key-0123456789") {
		t.Fatalf("the credential leaked into the error output:\n%s", output)
	}
	if _, err := os.Lstat(filepath.Join(home, ".ago", "demo", "greeter")); err == nil {
		t.Error("a failed relay preflight created the sample repository")
	}
}

// The release blocker, at the level a user meets it: `--state <anything>
// --reset` must not be able to delete a directory Ago never created.
func TestResetRefusesUnownedDirectoriesFromTheCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	requireTools(t)
	binary := buildAgo(t)

	const sentinel = "IMPORTANT-USER-FILE.txt"
	const body = "这是用户自己的文件。\n"

	type target struct {
		name    string
		prepare func(t *testing.T, home string) (state string, guarded string)
	}
	targets := []target{{
		name: "a user's own directory",
		prepare: func(t *testing.T, home string) (string, string) {
			state := filepath.Join(t.TempDir(), "my-work")
			mustWriteSentinel(t, state, sentinel, body)
			return state, state
		},
	}, {
		name: "the user's home",
		prepare: func(t *testing.T, home string) (string, string) {
			mustWriteSentinel(t, home, sentinel, body)
			return home, home
		},
	}, {
		name: "the filesystem root",
		prepare: func(t *testing.T, home string) (string, string) {
			mustWriteSentinel(t, home, sentinel, body)
			return "/", home
		},
	}, {
		name: "a symlink to a marked directory",
		prepare: func(t *testing.T, home string) (string, string) {
			real := filepath.Join(t.TempDir(), "real")
			mustWriteSentinel(t, real, sentinel, body)
			// Marked, so only the symlink refusal can stop this one.
			if err := os.WriteFile(filepath.Join(real, ".ago-demo-state"),
				[]byte(`{"magic":"ago-demo-state-v1"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			return link, real
		},
	}}

	for _, target := range targets {
		t.Run(target.name, func(t *testing.T) {
			home := t.TempDir()
			state, guarded := target.prepare(t, home)

			command := exec.Command(binary, "demo", "--executor", "fake",
				"--listen", "127.0.0.1:0", "--state", state, "--reset")
			command.Env = append(minimalEnv(t), "HOME="+home)
			output, err := command.CombinedOutput()
			if err == nil {
				t.Fatalf("reset was allowed on %s:\n%s", target.name, output)
			}
			if !strings.Contains(string(output), "拒绝重置") {
				t.Fatalf("the refusal is not a refusal to reset:\n%s", output)
			}
			content, readErr := os.ReadFile(filepath.Join(guarded, sentinel))
			if readErr != nil {
				t.Fatalf("the sentinel was destroyed: %v", readErr)
			}
			if string(content) != body {
				t.Fatalf("the sentinel was modified: %q", content)
			}
		})
	}
}

// A relay preflight failure must not destroy existing state, even when --reset
// was asked for. Reset runs after preflight for exactly this reason.
func TestResetDoesNotRunWhenPreflightFails(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	requireTools(t)
	binary := buildAgo(t)
	home := t.TempDir()
	state := filepath.Join(home, ".ago", "demo")

	// A directory Ago legitimately owns, with real state in it.
	mustWriteSentinel(t, state, "ago.db", "pretend this is a database\n")
	if err := os.WriteFile(filepath.Join(state, ".ago-demo-state"),
		[]byte(`{"magic":"ago-demo-state-v1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(binary, "demo", "--executor", "relay",
		"--listen", "127.0.0.1:0", "--state", state, "--reset")
	command.Env = append(minimalEnv(t), "HOME="+home,
		"AGO_RELAY_BASE_URL=http://127.0.0.1:1/v1", "AGO_RELAY_API_KEY=")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("relay mode started without a credential:\n%s", output)
	}
	content, readErr := os.ReadFile(filepath.Join(state, "ago.db"))
	if readErr != nil {
		t.Fatalf("a failed preflight still reset existing state: %v\n%s", readErr, output)
	}
	if string(content) != "pretend this is a database\n" {
		t.Fatalf("existing state was modified: %q", content)
	}
}

// The demo must not have displaced anything cmd/ago already did.
func TestDispatchStillRoutesDaemonAndClient(t *testing.T) {
	for _, args := range [][]string{nil, {"--socket", "/tmp/x"}, {"daemon", "--socket", "/tmp/x"}} {
		if mode, _ := dispatch(args); mode != "daemon" {
			t.Fatalf("dispatch(%q) = %q, want daemon", args, mode)
		}
	}
	if mode, _ := dispatch([]string{"list"}); mode != "client" {
		t.Fatal("a client subcommand no longer routes to the client")
	}
	mode, rest := dispatch([]string{"demo", "--executor", "fake"})
	if mode != "demo" {
		t.Fatalf("dispatch(demo) = %q, want demo", mode)
	}
	if len(rest) != 2 || rest[0] != "--executor" {
		t.Fatalf("demo arguments were not passed through: %q", rest)
	}
}

func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"git", "go"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable", tool)
		}
	}
}

// minimalEnv gives a child only what the demo needs: the directories holding
// git and go, and nothing else. In particular no ago-server, and no relay
// configuration inherited from the developer's shell.
func minimalEnv(t *testing.T) []string {
	t.Helper()
	var directories []string
	for _, tool := range []string{"git", "go"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("%s unavailable", tool)
		}
		directories = append(directories, filepath.Dir(path))
	}
	return []string{"PATH=" + strings.Join(directories, string(os.PathListSeparator))}
}

func buildAgo(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "ago")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ago: %v\n%s", err, output)
	}
	return binary
}

type demoProcess struct {
	command *exec.Cmd
	stdout  io.Reader
}

func startDemo(t *testing.T, binary, home string) *demoProcess {
	t.Helper()
	command := exec.Command(binary, "demo", "--executor", "fake", "--listen", "127.0.0.1:0")
	command.Env = append(minimalEnv(t), "HOME="+home)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	return &demoProcess{command: command, stdout: stdout}
}

// interrupt asks for a graceful shutdown and insists it actually happens: a
// demo a user cannot Ctrl+C is a demo they have to hunt in a process list.
func (process *demoProcess) interrupt(t *testing.T) {
	t.Helper()
	if err := process.command.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- process.command.Wait() }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		process.kill()
		t.Fatal("the demo did not shut down on SIGINT")
	}
}

func (process *demoProcess) kill() {
	_ = syscall.Kill(-process.command.Process.Pid, syscall.SIGKILL)
	_ = process.command.Wait()
}

func mustWriteSentinel(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

var demoAddressPattern = regexp.MustCompile(`UI:\s+http://(\S+)`)

func readAddress(t *testing.T, stdout io.Reader) (string, string) {
	t.Helper()
	var transcript strings.Builder
	var address string
	scanner := bufio.NewScanner(stdout)
	deadline := time.Now().Add(90 * time.Second)
	for scanner.Scan() {
		line := scanner.Text()
		transcript.WriteString(line + "\n")
		if match := demoAddressPattern.FindStringSubmatch(line); match != nil {
			address = match[1]
		}
		// The announce line is printed last at startup, so waiting for it means
		// the transcript is the whole startup block rather than however much of
		// it happened to be flushed.
		if address != "" && strings.Contains(line, "打开看板") {
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
	t.Fatalf("the demo never printed a listen address:\n%s", transcript.String())
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
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		}
		stray = append(stray, fmt.Sprintf("%s (%s)", entry.Name(), kind))
	}
	return stray
}

// The exploit an adversarial review found against the first version of the
// reset fix, reproduced at the level a user meets it.
//
// Ownership was granted by writing a marker whenever the sample repository was
// absent, which is true of any directory --state can name. Two ordinary
// commands then destroyed real data, because "artifacts" and "integration" are
// ordinary directory names.
func TestDemoRefusesToOccupyADirectoryFullOfSomeoneElsesWork(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	requireTools(t)
	binary := buildAgo(t)
	home := t.TempDir()

	project := filepath.Join(t.TempDir(), "myproject")
	for _, entry := range []string{"artifacts", "integration", "src"} {
		if err := os.MkdirAll(filepath.Join(project, entry), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	report := filepath.Join(project, "artifacts", "report.pdf")
	if err := os.WriteFile(report, []byte("user data i care about\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Step one: the mistyped --state. It must not be accepted, and above all
	// it must not leave a marker behind.
	occupy := exec.Command(binary, "demo", "--executor", "fake", "--listen", "127.0.0.1:0", "--state", project)
	occupy.Env = append(minimalEnv(t), "HOME="+home)
	output, err := occupy.CombinedOutput()
	if err == nil {
		t.Fatalf("the demo occupied a directory full of someone else's work:\n%s", output)
	}
	if !strings.Contains(string(output), "拒绝把") {
		t.Fatalf("the refusal does not explain itself:\n%s", output)
	}
	if _, err := os.Lstat(filepath.Join(project, ".ago-demo-state")); err == nil {
		t.Fatal("a refused directory was still marked as Ago's")
	}

	// Step two: the reset that used to follow. With no marker it cannot run.
	reset := exec.Command(binary, "demo", "--executor", "fake", "--listen", "127.0.0.1:0", "--state", project, "--reset")
	reset.Env = append(minimalEnv(t), "HOME="+home)
	output, err = reset.CombinedOutput()
	if err == nil {
		t.Fatalf("reset ran on a directory Ago never owned:\n%s", output)
	}
	content, readErr := os.ReadFile(report)
	if readErr != nil {
		t.Fatalf("the user's file was destroyed: %v", readErr)
	}
	if string(content) != "user data i care about\n" {
		t.Fatalf("the user's file was modified: %q", content)
	}
	for _, entry := range []string{"artifacts", "integration", "src"} {
		if _, err := os.Stat(filepath.Join(project, entry)); err != nil {
			t.Errorf("the user's %s directory was removed: %v", entry, err)
		}
	}
}

// A base URL can carry a credential in its query string or userinfo. The
// relay client redacts it inside the wrapped error; the surrounding message
// used to print it in the clear on the line above.
func TestRelayPreflightDoesNotEchoASecretBaseURL(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	binary := buildAgo(t)
	home := t.TempDir()

	const secret = "SUPERSECRET0123456789"
	command := exec.Command(binary, "demo", "--executor", "relay", "--listen", "127.0.0.1:0")
	command.Env = append(minimalEnv(t), "HOME="+home,
		"AGO_RELAY_BASE_URL=http://127.0.0.1:1/v1?api_key="+secret,
		"AGO_RELAY_API_KEY=k-not-in-url-abcdef")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("relay mode started against an unreachable endpoint:\n%s", output)
	}
	if strings.Contains(string(output), secret) {
		t.Fatalf("a credential in the base URL was echoed:\n%s", output)
	}
	// It still has to be recognisable enough to debug.
	if !strings.Contains(string(output), "127.0.0.1:1") {
		t.Fatalf("the failure does not say which endpoint was tried:\n%s", output)
	}
}

// A failed run must not leave a state directory tree behind for the user to
// wonder about.
func TestAFailedPreflightLeavesNoDirectories(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	requireTools(t)
	binary := buildAgo(t)
	home := t.TempDir()

	command := exec.Command(binary, "demo", "--executor", "relay", "--listen", "127.0.0.1:0")
	command.Env = append(minimalEnv(t), "HOME="+home,
		"AGO_RELAY_BASE_URL=http://127.0.0.1:1/v1", "AGO_RELAY_API_KEY=")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("relay mode started without a credential:\n%s", output)
	}
	if _, err := os.Lstat(filepath.Join(home, ".ago")); err == nil {
		t.Error("a failed preflight left ~/.ago behind")
	}
}
