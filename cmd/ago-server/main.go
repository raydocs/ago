// Command ago-server runs the local Ago Goal and Board API over a durable
// SQLite Work Graph.
//
// It is the demo entry point for increments D1 and D2: a Chinese goal creates a
// durable DAG and board, and the browser follows progress through a resumable
// server-sent event stream. Scheduling is still driven by explicit advance
// calls; the background scheduler arrives with D3.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"claudexflow/internal/agoboardapi"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agofake"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agoscheduler"

	"flag"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ago-server:", err)
		os.Exit(1)
	}
}

func run() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("ago-server", flag.ContinueOnError)
	databasePath := flags.String("db", filepath.Join(home, ".ago", "demo", "ago.db"), "Ago board SQLite database")
	listen := flags.String("listen", "127.0.0.1:4317", "loopback listen address")
	executionMode := flags.String("executor", agoboardapi.ExecutionModeFake, "executor family for admitted goals")
	scenario := flags.String("scenario", string(agofake.OutcomeSuccess), "scripted fake outcome: success, temporary_failure_then_success, permanent_failure, timeout, verifier_retry_with_feedback, blocked_needs_input, blocked_policy")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *executionMode != agoboardapi.ExecutionModeFake {
		return fmt.Errorf("unsupported executor %q: only %q is available until the Claude Code provider lands", *executionMode, agoboardapi.ExecutionModeFake)
	}
	if host, _, err := net.SplitHostPort(*listen); err != nil {
		return fmt.Errorf("invalid listen address %q: %w", *listen, err)
	} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address %q must be a numeric loopback address", *listen)
	}

	// Preflight the database directory before opening it so a permission
	// problem is reported as configuration rather than as a runtime failure.
	if err := os.MkdirAll(filepath.Dir(*databasePath), 0o700); err != nil {
		return fmt.Errorf("prepare database directory: %w", err)
	}
	store, err := agoboardstore.Open(*databasePath)
	if err != nil {
		return err
	}
	defer store.Close()

	runtime := agoboardruntime.New(store, agoplanner.DemoPlanner{}, agoboardruntime.Options{
		CoordinatorID: "ago-scheduler",
		WorkerID:      "ago-demo-worker",
		VerifierID:    "ago-verifier",
		LeaseDuration: 5 * time.Minute,
		Now:           time.Now,
	})
	server, err := agoboardapi.New(agoboardapi.Options{Runtime: runtime, Store: store, Providers: demoProviders()})
	if err != nil {
		return err
	}

	provider, err := agofake.New(agofake.Script{Default: agofake.Outcome(*scenario)})
	if err != nil {
		return err
	}
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime, Executor: provider, Verifier: provider,
		CoordinatorID: "ago-scheduler", WorkerID: "ago-demo-worker", VerifierID: "ago-verifier",
		LeaseDuration: 5 * time.Minute, Interval: time.Second, Now: time.Now,
	})
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listen, err)
	}
	httpServer := &http.Server{
		Handler:           server.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout stays unset: the event stream is long-lived and its
		// lifetime is bounded by the client's request context instead.
	}

	// Startup diagnostics are deliberately free of credentials and paths that
	// could contain them.
	fmt.Printf("Ago is ready\nUI:       http://%s\nDatabase: %s\nProvider: %s\nScenario: %s\n", listener.Addr(), *databasePath, *executionMode, *scenario)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The scheduler advances the board; nothing external drives it.
	schedulerCtx, stopScheduler := context.WithCancel(context.Background())
	schedulerDone := make(chan struct{})
	go func() {
		defer close(schedulerDone)
		_ = scheduler.Run(schedulerCtx)
	}()
	// Shutdown waits for the scheduler to finish its cycle, so a claimed attempt
	// is never abandoned mid-dispatch.
	defer func() {
		stopScheduler()
		<-schedulerDone
	}()

	errs := make(chan error, 1)
	go func() { errs <- httpServer.Serve(listener) }()
	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	}
}

// demoProviders reports the offline capability roster. These providers need no
// credentials, so auth_configured is truthfully false; the relay-backed
// providers introduced with D8 populate it from a provider profile and still
// never expose a credential value.
func demoProviders() []agoboardapi.Provider {
	return []agoboardapi.Provider{
		{ID: "ago-demo-planner", Kind: "planner", Capabilities: []string{"planning"}, AuthConfigured: false},
		{ID: "ago-demo-executor", Kind: "executor", Capabilities: []string{"repo-read", "repo-write", "tests", "report"}, AuthConfigured: false},
		{ID: "ago-verifier", Kind: "verifier", Capabilities: []string{"verification"}, AuthConfigured: false},
	}
}
