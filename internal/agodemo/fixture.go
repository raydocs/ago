// Package agodemo materializes the sample repository the demo and the
// end-to-end test both work on.
//
// It is a real Go module with a real CLI and a real test, because a goal like
// "add a version subcommand, document it, and add a test" cannot be honestly
// demonstrated against a repository that has no commands and no test runner.
// An earlier run against a bare README stopped and asked for the project's test
// command — correct behaviour, and a sign the fixture was the problem.
//
// Nothing here touches the user's own repositories: a fixture is created in a
// directory the caller names, and the caller is responsible for it.
package agodemo

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// files is the sample project. It is deliberately small enough to read in one
// sitting and complete enough to run: a CLI with one subcommand, a test that
// exercises it, and a README that documents what exists today.
var files = map[string]string{
	"go.mod": `module example.com/greeter

go 1.26
`,
	"main.go": `// Command greeter is a small example CLI.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "greeter:", err)
		os.Exit(1)
	}
}

func run(args []string, out *os.File) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: greeter <command>")
	}
	switch args[0] {
	case "greet":
		name := "world"
		if len(args) > 1 {
			name = args[1]
		}
		fmt.Fprintf(out, "hello, %s\n", name)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
`,
	"greet.go": `package main

// Greeting builds the message the greet subcommand prints.
func Greeting(name string) string {
	if name == "" {
		name = "world"
	}
	return "hello, " + name
}
`,
	"greet_test.go": `package main

import "testing"

func TestGreetingUsesTheGivenName(t *testing.T) {
	if got := Greeting("ago"); got != "hello, ago" {
		t.Fatalf("Greeting(\"ago\") = %q", got)
	}
}

func TestGreetingFallsBackToWorld(t *testing.T) {
	if got := Greeting(""); got != "hello, world" {
		t.Fatalf("Greeting(\"\") = %q", got)
	}
}
`,
	"README.md": `# greeter

A small example CLI.

## Commands

- ` + "`greeter greet [name]`" + ` — prints a greeting.

## Tests

Run the tests with:

` + "```" + `
go test ./...
` + "```" + `
`,
	"docs/.gitkeep": "",
}

// Objective is the canonical demonstration goal for this fixture. It requires
// reading the project, writing code, documenting it, and proving it with a
// test — which is what makes it a real exercise rather than a scripted one.
const Objective = "为这个示例 CLI 增加 version 子命令，补充 README 使用说明，增加自动化测试，并生成完成报告。"

// Create writes the fixture into root and makes it a git repository with one
// commit.
//
// It refuses a root that already holds anything, so it can never overwrite
// something the caller cares about. An EMPTY directory is accepted, because a
// caller that just created the directory itself — and recorded that it did —
// needs to hand it here without a gap in which the directory exists but
// nothing says who made it.
func Create(ctx context.Context, root string) error {
	if entries, err := os.ReadDir(root); err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".ago-") {
				// Bookkeeping the caller wrote about this directory, not
				// content.
				continue
			}
			return fmt.Errorf("refusing to write the fixture into a non-empty directory %q", root)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("the sample repository needs git: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	// The commits are the fixture's own, attributed locally so the command
	// never depends on the user's git identity being configured.
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "fixture@ago.localhost"},
		{"config", "user.name", "Ago Fixture"},
		{"add", "-A"},
		{"commit", "-m", "initial greeter"},
	} {
		if err := runGit(ctx, root, args...); err != nil {
			return err
		}
	}
	return nil
}

// PathScopes are the paths a goal against this fixture may change. They are
// narrow on purpose: a demo that allowed everything would not demonstrate the
// scope boundary holding.
func PathScopes() []string {
	return []string{"README.md", "main.go", "greet.go", "greet_test.go", "version.go", "version_test.go", "docs"}
}

func runGit(ctx context.Context, dir string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
