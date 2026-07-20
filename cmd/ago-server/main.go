// Command ago-server is the compatibility and development entry point for Ago.
//
// The user-facing command is `ago demo`. This binary stays because scripts and
// harnesses already invoke it, and because running the server alone — without
// planting a demo goal — is useful while developing. Both entry points call the
// same internal/agoserve stack; there is exactly one orchestration wiring.
//
// Two executors:
//
//   - fake: deterministic and offline. No credential, no network. The executor
//     and the verifier are still separate implementations, so the offline demo
//     exercises the same independence the real one does.
//   - relay: a real model behind an OpenAI-compatible endpoint plans, executes,
//     and verifies — as three separate roles making three separate calls.
//
// Credentials come from the environment only. They are never accepted as a
// flag, because a flag lands in shell history and in the process list.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"claudexflow/internal/agofake"
	"claudexflow/internal/agoserve"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ago-server:", err)
		os.Exit(1)
	}
}

func run() error {
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "serve":
			return runServe(args[1:])
		case "demo":
			return agoserve.Demo(args[1:], os.Stdout)
		default:
			return fmt.Errorf("unknown command %q: use \"serve\" or \"demo\"", args[0])
		}
	}
	return runServe(args)
}

func runServe(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("ago-server", flag.ContinueOnError)
	databasePath := flags.String("db", filepath.Join(home, ".ago", "demo", "ago.db"), "Ago board SQLite database")
	listen := flags.String("listen", "127.0.0.1:4317", "loopback listen address")
	mode := flags.String("executor", agoserve.ModeFake, `which executor runs the work: "fake" (offline, no credential) or "relay" (a real model behind an OpenAI-compatible endpoint)`)
	// --mode is what this flag was called first. Accepting both keeps existing
	// scripts working rather than making a rename their problem.
	flags.StringVar(mode, "mode", agoserve.ModeFake, `deprecated alias for --executor`)
	scenario := flags.String("scenario", string(agofake.OutcomeSuccess), "scripted outcome, fake mode only: success, temporary_failure_then_success, permanent_failure, timeout, verifier_retry_with_feedback, blocked_needs_input, blocked_policy")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *mode != agoserve.ModeFake && *mode != agoserve.ModeRelay {
		return fmt.Errorf("unsupported executor %q: use %q or %q", *mode, agoserve.ModeFake, agoserve.ModeRelay)
	}
	// A scripted outcome is a property of the offline demo. Accepting one in
	// relay mode would let a run look scripted while a real model did the work.
	if *mode == agoserve.ModeRelay && *scenario != string(agofake.OutcomeSuccess) {
		return fmt.Errorf("--scenario applies to %q mode only", agoserve.ModeFake)
	}
	return agoserve.Serve(agoserve.Config{
		DatabasePath: *databasePath, Listen: *listen, Mode: *mode, Scenario: *scenario,
	})
}
