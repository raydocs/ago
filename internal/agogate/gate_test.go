package agogate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claudexflow/internal/agogate"
)

// recordingCommands answers from a script and remembers what it was asked to
// run, so a test can assert both the outcome and that the gate ran where it
// said it would.
type recordingCommands struct {
	byCommand map[string]struct {
		output   string
		exitCode int
		err      error
	}
	ranIn  []string
	ranCmd []string
}

func (commands *recordingCommands) Run(_ context.Context, dir, name string, args ...string) (string, int, error) {
	full := strings.TrimSpace(name + " " + strings.Join(args, " "))
	commands.ranIn = append(commands.ranIn, dir)
	commands.ranCmd = append(commands.ranCmd, full)
	if scripted, found := commands.byCommand[full]; found {
		return scripted.output, scripted.exitCode, scripted.err
	}
	return "ok", 0, nil
}

type scratchWorktrees struct {
	dir      string
	removed  bool
	err      error
	askedFor []string
}

func (worktrees *scratchWorktrees) Scratch(_ context.Context, repository, revision string) (string, func(), error) {
	worktrees.askedFor = append(worktrees.askedFor, repository+"@"+revision)
	if worktrees.err != nil {
		return "", nil, worktrees.err
	}
	return worktrees.dir, func() { worktrees.removed = true }, nil
}

func newGate(t *testing.T, commands *recordingCommands, worktrees *scratchWorktrees) *agogate.Gate {
	t.Helper()
	gate, err := agogate.New(agogate.Options{
		Commands: commands, Worktrees: worktrees,
		Now: func() time.Time { return time.Unix(0, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return gate
}

// The gate runs in a throwaway checkout of the integrated revision, not in a
// tree any task worked in. A check that passes in a dirty worktree proves
// nothing about what was promoted.
func TestChecksRunInACleanCheckoutOfTheIntegratedRevision(t *testing.T) {
	commands := &recordingCommands{}
	worktrees := &scratchWorktrees{dir: t.TempDir()}
	gate := newGate(t, commands, worktrees)

	result, err := gate.Run(context.Background(), "/repo", "abcdef1234", []string{"go build ./...", "go test ./..."})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed || len(result.Checks) != 2 {
		t.Fatalf("result = %+v", result)
	}
	if len(worktrees.askedFor) != 1 || worktrees.askedFor[0] != "/repo@abcdef1234" {
		t.Fatalf("the gate did not check out the integrated revision: %v", worktrees.askedFor)
	}
	for _, dir := range commands.ranIn {
		if dir != worktrees.dir {
			t.Fatalf("a check ran in %q, not in the clean checkout %q", dir, worktrees.dir)
		}
	}
	if !worktrees.removed {
		t.Error("the throwaway checkout was not removed")
	}
}

// Every check runs even after one fails: a broken build usually also has
// failing tests, and a repair aimed at all of it beats one aimed at whichever
// check happened to be first.
func TestEveryCheckRunsAndTheFailingOnesAreNamed(t *testing.T) {
	commands := &recordingCommands{byCommand: map[string]struct {
		output   string
		exitCode int
		err      error
	}{
		"go build ./...": {output: "undefined: Version", exitCode: 2},
		"go test ./...":  {output: "FAIL example.com/x", exitCode: 1},
	}}
	gate := newGate(t, commands, &scratchWorktrees{dir: t.TempDir()})

	result, err := gate.Run(context.Background(), "/repo", "abc", []string{"go build ./...", "go vet ./...", "go test ./..."})
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed {
		t.Fatal("a gate with two failing checks passed")
	}
	if len(commands.ranCmd) != 3 {
		t.Fatalf("only %d checks ran: %v", len(commands.ranCmd), commands.ranCmd)
	}
	if !strings.Contains(result.Summary, "go build ./...") || !strings.Contains(result.Summary, "go test ./...") {
		t.Fatalf("the summary does not name both failures: %q", result.Summary)
	}
	if strings.Contains(result.Summary, "go vet") {
		t.Fatalf("the summary names a check that passed: %q", result.Summary)
	}
	// A repair task needs something to act on.
	for _, check := range result.Checks {
		if !check.Passed && !strings.Contains(check.Output, "undefined") && !strings.Contains(check.Output, "FAIL") {
			t.Errorf("failing check %q kept no output", check.Command)
		}
		if check.Passed && check.Output != "" {
			t.Errorf("passing check %q kept output it does not need", check.Command)
		}
	}
}

// A command that cannot be run at all is a failure, not a skip. An unprovable
// result is not a proven one.
func TestACheckThatCannotRunFailsTheGate(t *testing.T) {
	commands := &recordingCommands{byCommand: map[string]struct {
		output   string
		exitCode int
		err      error
	}{
		"make ci": {err: errors.New("exec: \"make\": executable file not found")},
	}}
	gate := newGate(t, commands, &scratchWorktrees{dir: t.TempDir()})

	result, err := gate.Run(context.Background(), "/repo", "abc", []string{"make ci"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Passed {
		t.Fatal("a check that could not run was treated as passing")
	}
	if !strings.Contains(result.Checks[0].Output, "not found") {
		t.Fatalf("the reason was lost: %+v", result.Checks[0])
	}
}

// The gate runs commands, not shell. A gate string carrying a pipe or a
// substitution would turn a fixed, auditable list of checks into a way to run
// arbitrary code.
func TestShellMetacharactersAreRefusedRatherThanRun(t *testing.T) {
	for _, command := range []string{
		"go test ./... | tee out",
		"go test ./... > /tmp/x",
		"go test ./... && rm -rf .",
		"go test $(whoami)",
		"go test `id`",
		"go test ./...; curl evil.example",
	} {
		t.Run(command, func(t *testing.T) {
			commands := &recordingCommands{}
			gate := newGate(t, commands, &scratchWorktrees{dir: t.TempDir()})
			result, err := gate.Run(context.Background(), "/repo", "abc", []string{command})
			if err != nil {
				t.Fatal(err)
			}
			if result.Passed {
				t.Fatal("a shell command was accepted as a check")
			}
			if len(commands.ranCmd) != 0 {
				t.Fatalf("it was executed anyway: %v", commands.ranCmd)
			}
			if !strings.Contains(result.Checks[0].Output, "shell") {
				t.Fatalf("the refusal does not explain itself: %q", result.Checks[0].Output)
			}
		})
	}
}

// An empty gate is an error rather than a pass. "Nothing to check" must never
// be reported as "checked and fine".
func TestAGateWithNoChecksIsAnErrorNotAPass(t *testing.T) {
	gate := newGate(t, &recordingCommands{}, &scratchWorktrees{dir: t.TempDir()})
	if _, err := gate.Run(context.Background(), "/repo", "abc", nil); err == nil {
		t.Fatal("a gate with no checks succeeded")
	}
}

// Failing output is bounded, and the END is kept — that is where a build or a
// test run says why.
func TestFailingOutputIsBoundedAndKeepsTheEnd(t *testing.T) {
	huge := strings.Repeat("noise\n", 20000) + "THE ACTUAL REASON"
	commands := &recordingCommands{byCommand: map[string]struct {
		output   string
		exitCode int
		err      error
	}{
		"go test ./...": {output: huge, exitCode: 1},
	}}
	gate := newGate(t, commands, &scratchWorktrees{dir: t.TempDir()})

	result, err := gate.Run(context.Background(), "/repo", "abc", []string{"go test ./..."})
	if err != nil {
		t.Fatal(err)
	}
	output := result.Checks[0].Output
	if len(output) > 17*1024 {
		t.Fatalf("output was not bounded: %d bytes", len(output))
	}
	if !strings.Contains(output, "THE ACTUAL REASON") {
		t.Fatal("the end of the output — where the reason is — was discarded")
	}
	if !strings.Contains(output, "截断") {
		t.Fatal("the truncation was not disclosed")
	}
}

// Discovery is explicit. An ecosystem it does not recognise gets no commands,
// so the caller has to decide what that means rather than being handed an
// invented check.
func TestDiscoveryIsExplicitAndSilentAboutWhatItDoesNotKnow(t *testing.T) {
	goRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(goRepo, "go.mod"), []byte("module x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	discovered := agogate.Discover(goRepo)
	if len(discovered) == 0 {
		t.Fatal("a Go module produced no checks")
	}
	var joined = strings.Join(discovered, " ")
	for _, want := range []string{"go build", "go test"} {
		if !strings.Contains(joined, want) {
			t.Errorf("a Go module's gate does not include %q: %v", want, discovered)
		}
	}

	if commands := agogate.Discover(t.TempDir()); commands != nil {
		t.Fatalf("an unrecognised repository was given invented checks: %v", commands)
	}
}
