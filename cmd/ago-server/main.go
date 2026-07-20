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

	"claudexflow/internal/agoartifact"
	"claudexflow/internal/agoboardapi"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agoboardui"
	"claudexflow/internal/agofake"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agoredact"
	"claudexflow/internal/agoscheduler"
	"claudexflow/internal/agosupervisor"

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
	artifacts, err := agoartifact.Open(agoartifact.Options{Root: filepath.Join(filepath.Dir(*databasePath), "artifacts")})
	if err != nil {
		return err
	}
	// Clear crash debris before serving: a temp file or an object no evidence
	// references is discardable, and nothing referenced is ever removed.
	if referenced, err := store.ReferencedArtifacts(context.Background()); err == nil {
		if _, err := artifacts.Reconcile(context.Background(), referenced); err != nil {
			return fmt.Errorf("reconcile artifact store: %w", err)
		}
	}
	provider, err := agofake.New(agofake.Script{Default: agofake.Outcome(*scenario)})
	if err != nil {
		return err
	}
	provider = provider.WithArtifacts(artifacts)
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime, Executor: provider, Verifier: provider,
		CoordinatorID: "ago-scheduler", WorkerID: "ago-demo-worker", VerifierID: "ago-verifier",
		LeaseDuration: 5 * time.Minute, Interval: time.Second, Now: time.Now,
		Redactor: agoredact.NewFromEnvironment(os.Getenv),
	})
	if err != nil {
		return err
	}

	// The supervisor closes the loop: it reviews stopped work, repairs what it
	// can, and only queues a decision when a person genuinely must choose.
	// Authorization is deliberately narrow — local file writes and local
	// commits — so publishing or destructive work becomes a queued decision
	// rather than something the machine does on its own.
	supervisorRunner, err := agosupervisor.NewRunner(agosupervisor.RunnerOptions{
		Store: store, Scheduler: scheduler,
		Authorize: agosupervisor.Authorization{LocalFileWrites: true, LocalCommits: true},
		Interval:  2 * time.Second, Now: time.Now,
	})
	if err != nil {
		return err
	}
	server, err := agoboardapi.New(agoboardapi.Options{
		Runtime: runtime, Store: store, Providers: demoProviders(), Artifacts: artifacts,
		Decisions: supervisorRunner,
	})
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listen, err)
	}
	// The interface and the API share this origin, so the page needs no CORS
	// and never has to reach a plaintext localhost address from an HTTPS page.
	ui, err := agoboardui.Handler()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", server.Handler())
	mux.Handle("/", ui)

	httpServer := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// WriteTimeout stays unset: the event stream is long-lived and its
		// lifetime is bounded by the client's request context instead.
	}

	// Startup diagnostics are deliberately free of credentials and paths that
	// could contain them.
	fmt.Printf("Ago is ready\nUI:       http://%s\nDatabase: %s\nProvider: %s\nScenario: %s\n", listener.Addr(), *databasePath, *executionMode, *scenario)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The supervisor owns the loop and calls the scheduler itself, so there is
	// exactly one driver. Running the scheduler separately as well would mean
	// two cycles racing for the same work with nothing gained.
	supervisorCtx, stopSupervisor := context.WithCancel(context.Background())
	supervisorDone := make(chan struct{})
	go func() {
		defer close(supervisorDone)
		_ = supervisorRunner.Run(supervisorCtx)
	}()
	// Shutdown waits for the current pass, so a claimed attempt is never
	// abandoned mid-dispatch.
	defer func() {
		stopSupervisor()
		<-supervisorDone
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
