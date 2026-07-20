package main_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ago-server is the compatibility entry point now; `ago demo` is what a user
// runs, and cmd/ago carries the end-to-end coverage. What still has to hold
// here is that the old invocation keeps working and reaches the same stack.
func TestServerCompatibilityEntryPointStillParses(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	binary := filepath.Join(t.TempDir(), "ago-server")
	if output, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build ago-server: %v\n%s", err, output)
	}

	// The rename kept the old spelling working, because breaking a flag is
	// making a rename someone else's problem.
	for _, args := range [][]string{
		{"--mode", "nonsense"},
		{"--executor", "nonsense"},
		{"serve", "--executor", "nonsense"},
	} {
		output, err := exec.Command(binary, args...).CombinedOutput()
		if err == nil {
			t.Fatalf("%v was accepted:\n%s", args, output)
		}
		if !strings.Contains(string(output), "unsupported executor") {
			t.Fatalf("%v did not reach executor validation:\n%s", args, output)
		}
	}
	// A scripted outcome is a property of the offline mode only.
	output, err := exec.Command(binary, "--executor", "relay", "--scenario", "permanent_failure").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "--scenario applies") {
		t.Fatalf("relay mode accepted a scripted outcome:\n%s", output)
	}
	// And the demo subcommand is still reachable from this binary.
	output, err = exec.Command(binary, "demo", "--executor", "nonsense").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "--executor") {
		t.Fatalf("the demo subcommand is not reachable from ago-server:\n%s", output)
	}
	output, err = exec.Command(binary, "nonsense").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "unknown command") {
		t.Fatalf("an unknown command was not reported:\n%s", output)
	}
}
