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

	// Each case names the rule that must refuse it. Asserting only "some
	// refusal happened" is how a deny-list rule ends up unexercised: the
	// refusal arrives from an unrelated check and the rule itself could be
	// deleted with the test still green.
	type target struct {
		name       string
		wantReason string
		prepare    func(t *testing.T, home string) (state string, guarded string)
	}
	targets := []target{{
		name:       "a user's own directory",
		wantReason: "归属标记",
		prepare: func(t *testing.T, home string) (string, string) {
			state := filepath.Join(t.TempDir(), "my-work")
			mustWriteSentinel(t, state, sentinel, body)
			return state, state
		},
	}, {
		name:       "the user's home",
		wantReason: "主目录",
		prepare: func(t *testing.T, home string) (string, string) {
			mustWriteSentinel(t, home, sentinel, body)
			return home, home
		},
	}, {
		name:       "the filesystem root",
		wantReason: "根目录",
		prepare: func(t *testing.T, home string) (string, string) {
			mustWriteSentinel(t, home, sentinel, body)
			return "/", home
		},
	}, {
		name:       "a symlink to a real demo directory",
		wantReason: "符号链接",
		prepare: func(t *testing.T, home string) (string, string) {
			// A directory Ago genuinely owns, reached through a link. Only the
			// symlink rule can refuse this one: the marker is real.
			real := filepath.Join(t.TempDir(), "real")
			claimed := startDemoAt(t, binary, home, real)
			readAddress(t, claimed.stdout)
			claimed.interrupt(t)
			mustWriteSentinel(t, real, sentinel, body)
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
			if !strings.Contains(string(output), target.wantReason) {
				t.Fatalf("the refusal did not come from the %s rule:\n%s", target.wantReason, output)
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
	return startDemoAt(t, binary, home, "")
}

func startDemoAt(t *testing.T, binary, home, state string, extra ...string) *demoProcess {
	t.Helper()
	args := []string{"demo", "--executor", "fake", "--listen", "127.0.0.1:0"}
	if state != "" {
		args = append(args, "--state", state)
	}
	args = append(args, extra...)
	command := exec.Command(binary, args...)
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

// The exploit, against the binary, in every shape it took.
//
// Ownership was first granted whenever the sample repository was absent, then
// narrowed to directories whose entries all carried Ago's names — the same
// hole with a longer condition, because "artifacts" and "integration" are
// ordinary directory names and a name is not provenance.
//
// Every case here contains ONLY names Ago reserves. An earlier version of this
// test added an extra "src" directory, and passed while the hole was open: the
// refusal it observed came from the one file outside the allowlist, not from
// the ownership rule.
func TestDemoRefusesToOccupyADirectoryItDidNotCreate(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	requireTools(t)
	binary := buildAgo(t)

	sqliteHeader := append([]byte("SQLite format 3\x00"), make([]byte, 84)...)
	cases := map[string]map[string][]byte{
		"artifacts holding a report":  {"artifacts/report.pdf": []byte("%PDF-1.7 user data\n")},
		"integration holding a patch": {"integration/user.patch": []byte("--- a\n+++ b\n")},
		"worktrees holding a note":    {"worktrees/note.txt": []byte("my notes\n")},
		"greeter holding a readme":    {"greeter/README.md": []byte("# my project\n")},
		"a plain file named ago.db":   {"ago.db": []byte("not actually a database\n")},
		"a real SQLite file":          {"ago.db": sqliteHeader},
		"a database and an artifacts": {"ago.db": sqliteHeader, "artifacts/report.pdf": []byte("user data\n")},
	}

	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			project := filepath.Join(t.TempDir(), "myproject")
			for relative, content := range files {
				path := filepath.Join(project, filepath.FromSlash(relative))
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, content, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			before := snapshotTree(t, project)

			// Step one: the mistyped --state. It must be refused, and above all
			// it must not leave a marker behind.
			occupy := exec.Command(binary, "demo", "--executor", "fake",
				"--listen", "127.0.0.1:0", "--state", project)
			occupy.Env = append(minimalEnv(t), "HOME="+home)
			output, err := occupy.CombinedOutput()
			if err == nil {
				t.Fatalf("the demo occupied a directory it did not create:\n%s", output)
			}
			if !strings.Contains(string(output), "拒绝把") {
				t.Fatalf("the refusal does not explain itself:\n%s", output)
			}
			if _, err := os.Lstat(filepath.Join(project, ".ago-demo-state")); err == nil {
				t.Fatal("a refused directory was still marked as Ago's")
			}
			assertTreeUnchanged(t, project, before)

			// Step two: the reset that used to follow. With no marker it cannot
			// run, now or ever.
			reset := exec.Command(binary, "demo", "--executor", "fake",
				"--listen", "127.0.0.1:0", "--state", project, "--reset")
			reset.Env = append(minimalEnv(t), "HOME="+home)
			if output, err := reset.CombinedOutput(); err == nil {
				t.Fatalf("reset ran on a directory Ago never claimed:\n%s", output)
			}
			assertTreeUnchanged(t, project, before)
		})
	}
}

// The legitimate paths still work end to end: an empty directory is usable,
// and a directory Ago owns can be reset without taking a user's later file
// with it.
func TestDemoUsesAnEmptyDirectoryAndResetsOnlyItsOwnEntries(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a server")
	}
	requireTools(t)
	binary := buildAgo(t)
	home := t.TempDir()
	state := filepath.Join(t.TempDir(), "empty")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}

	first := startDemoAt(t, binary, home, state)
	_, transcript := readAddress(t, first.stdout)
	if !strings.Contains(transcript, "示例仓库已创建") {
		t.Fatalf("an empty directory was not usable:\n%s", transcript)
	}
	first.interrupt(t)
	if _, err := os.Lstat(filepath.Join(state, ".ago-demo-state")); err != nil {
		t.Fatalf("a directory Ago filled was not marked as its own: %v", err)
	}

	// A file the user drops in afterwards must survive the reset.
	note := filepath.Join(state, "my-note.txt")
	if err := os.WriteFile(note, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := startDemoAt(t, binary, home, state, "--reset")
	defer second.kill()
	_, resetTranscript := readAddress(t, second.stdout)
	if !strings.Contains(resetTranscript, "已清理") {
		t.Fatalf("the reset did not report itself:\n%s", resetTranscript)
	}
	content, err := os.ReadFile(note)
	if err != nil {
		t.Fatalf("the reset removed a file Ago did not create: %v", err)
	}
	if string(content) != "keep me\n" {
		t.Fatalf("the reset modified a file Ago did not create: %q", content)
	}
}

// snapshotTree records every path under root with its contents, so a test can
// assert that nothing changed rather than that one sentinel survived.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			tree[relative+"/"] = ""
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[relative] = string(content)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func assertTreeUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := snapshotTree(t, root)
	for path, content := range before {
		got, present := after[path]
		if !present {
			t.Errorf("%s was removed", path)
			continue
		}
		if got != content {
			t.Errorf("%s was modified: %q", path, got)
		}
	}
	for path := range after {
		if _, present := before[path]; !present {
			t.Errorf("%s was created in a directory Ago was refused", path)
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

// The v3 hole, reproduced end to end against the binary exactly as the review
// found it.
//
// A claim was correct when it was made and then never re-decided: point
// --state at a directory that is empty or absent, let something else fill it,
// and reset removed what that something else made. No cleanup step, no
// symlink, no forged marker — two documented commands with an ordinary build
// in between.
func TestResetLeavesWhatSomethingElsePutInAClaimedDirectory(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a server")
	}
	requireTools(t)
	binary := buildAgo(t)
	home := t.TempDir()
	// The realistic shape: a scratch directory inside a project that does not
	// exist yet, which the build system will later fill.
	state := filepath.Join(t.TempDir(), "myrepo", "build")

	// Run one: Ago creates and claims it. Ordinary, non-destructive.
	first := startDemoAt(t, binary, home, state)
	_, transcript := readAddress(t, first.stdout)
	if !strings.Contains(transcript, "示例仓库已创建") {
		t.Fatalf("the first run did not claim the directory:\n%s", transcript)
	}
	first.interrupt(t)

	// Now something else owns the names. Ago's own artifacts are replaced by a
	// different directory holding the user's work.
	if err := os.RemoveAll(filepath.Join(state, "artifacts")); err != nil {
		t.Fatal(err)
	}
	thesis := filepath.Join(state, "artifacts", "thesis.pdf")
	if err := os.MkdirAll(filepath.Dir(thesis), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thesis, []byte("IRREPLACEABLE\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Run two: the reset.
	second := startDemoAt(t, binary, home, state, "--reset")
	defer second.kill()
	_, resetTranscript := readAddress(t, second.stdout)
	t.Logf("reset run:\n%s", resetTranscript)

	content, err := os.ReadFile(thesis)
	if err != nil {
		t.Fatalf("reset destroyed a file Ago never created: %v", err)
	}
	if string(content) != "IRREPLACEABLE\n" {
		t.Fatalf("reset modified a file Ago never created: %q", content)
	}
}

// Delete authority does not travel with a copy of the marker. Moving a demo
// directory and repurposing it is the most likely way a user would arrive
// here.
func TestAMovedDemoDirectoryLosesItsDeleteAuthority(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a server")
	}
	requireTools(t)
	binary := buildAgo(t)
	home := t.TempDir()
	root := t.TempDir()
	state := filepath.Join(root, "demo")

	first := startDemoAt(t, binary, home, state)
	readAddress(t, first.stdout)
	first.interrupt(t)

	moved := filepath.Join(root, "myproject")
	if err := os.Rename(state, moved); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(moved, "artifacts", "user.pdf")
	if err := os.MkdirAll(filepath.Dir(mine), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mine, []byte("mine\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(binary, "demo", "--executor", "fake",
		"--listen", "127.0.0.1:0", "--state", moved, "--reset")
	command.Env = append(minimalEnv(t), "HOME="+home)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("a moved directory kept its delete authority:\n%s", output)
	}
	if _, err := os.Stat(mine); err != nil {
		t.Fatalf("the user's file was destroyed: %v", err)
	}
}
