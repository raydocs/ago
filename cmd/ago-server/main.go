// Command ago-server runs Ago: a Chinese goal becomes a durable task graph that
// a background scheduler executes, an independent verifier judges, and an
// integration authority promotes onto an Ago-owned git ref.
//
// Two modes:
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
	"claudexflow/internal/agoexec"
	"claudexflow/internal/agofake"
	"claudexflow/internal/agointegrate"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agoredact"
	"claudexflow/internal/agorelay"
	"claudexflow/internal/agorelayplanner"
	"claudexflow/internal/agorelayverifier"
	"claudexflow/internal/agoscheduler"
	"claudexflow/internal/agosupervisor"
	"claudexflow/internal/agoverify"
	"claudexflow/internal/agoworktree"
	"strings"

	"flag"
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
			return runDemo(args[1:])
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
	mode := flags.String("executor", modeFake, `which executor runs the work: "fake" (offline, no credential) or "relay" (a real model behind an OpenAI-compatible endpoint)`)
	// --mode is what this flag was called first. Accepting both keeps existing
	// scripts working rather than making a rename their problem.
	flags.StringVar(mode, "mode", modeFake, `deprecated alias for --executor`)
	scenario := flags.String("scenario", string(agofake.OutcomeSuccess), "scripted outcome, fake mode only: success, temporary_failure_then_success, permanent_failure, timeout, verifier_retry_with_feedback, blocked_needs_input, blocked_policy")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *mode != modeFake && *mode != modeRelay {
		return fmt.Errorf("unsupported mode %q: use %q or %q", *mode, modeFake, modeRelay)
	}
	// A scripted outcome is a property of the offline demo. Accepting one in
	// relay mode would let a run look scripted while a real model did the work.
	if *mode == modeRelay && *scenario != string(agofake.OutcomeSuccess) {
		return fmt.Errorf("--scenario applies to %q mode only", modeFake)
	}
	if host, _, err := net.SplitHostPort(*listen); err != nil {
		return fmt.Errorf("invalid listen address %q: %w", *listen, err)
	} else if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("listen address %q must be a numeric loopback address", *listen)
	}

	return serve(serveConfig{
		DatabasePath: *databasePath, Listen: *listen, Mode: *mode, Scenario: *scenario,
	})
}

// serveConfig is one server's whole configuration. Setup runs once the stack
// exists and before the listener accepts, which is where the demo plants its
// goal — a board created through the same runtime any client would use.
type serveConfig struct {
	DatabasePath string
	Listen       string
	Mode         string
	Scenario     string
	Setup        func(context.Context, *stack) error
	// Announce prints whatever the caller wants a user to read once the
	// address is known.
	Announce func(address string)
}

// stack is everything one server owns.
type stack struct {
	store      *agoboardstore.Store
	artifacts  *agoartifact.Store
	worktrees  *agoworktree.Manager
	integrator *agointegrate.Integrator
	runtime    *agoboardruntime.Runtime
	mode       string
}

func serve(cfg serveConfig) error {
	databasePath, listen, mode, scenario := cfg.DatabasePath, cfg.Listen, cfg.Mode, cfg.Scenario
	// Preflight the database directory before opening it so a permission
	// problem is reported as configuration rather than as a runtime failure.
	if err := os.MkdirAll(filepath.Dir(databasePath), 0o700); err != nil {
		return fmt.Errorf("prepare database directory: %w", err)
	}
	store, err := agoboardstore.Open(databasePath)
	if err != nil {
		return err
	}
	defer store.Close()

	stateDir := filepath.Dir(databasePath)
	artifacts, err := agoartifact.Open(agoartifact.Options{Root: filepath.Join(stateDir, "artifacts")})
	if err != nil {
		return err
	}
	worktrees, err := agoworktree.New(agoworktree.Options{Root: filepath.Join(stateDir, "worktrees")})
	if err != nil {
		return err
	}
	integrator, err := agointegrate.New(agointegrate.Options{Root: filepath.Join(stateDir, "integration")})
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
	planner, executor, judge, err := buildRoles(mode, scenario, artifacts, worktrees, integrator)
	if err != nil {
		return err
	}
	runtime := agoboardruntime.New(store, planner, agoboardruntime.Options{
		CoordinatorID: "ago-scheduler",
		WorkerID:      "ago-worker",
		VerifierID:    "ago-verifier",
		LeaseDuration: 5 * time.Minute,
		Now:           time.Now,
	})
	verification, err := agoverify.New(agoverify.Options{Judge: judge, Artifacts: artifacts})
	if err != nil {
		return err
	}
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime, Executor: executor, Verification: verification,
		Integrator: integrator, Artifacts: artifacts,
		CoordinatorID: "ago-scheduler", WorkerID: "ago-worker", VerifierID: "ago-verifier",
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
		Runtime: runtime, Store: store, Providers: describeRoles(mode), Artifacts: artifacts,
		Decisions: supervisorRunner, Integration: integrator,
	})
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", listen, err)
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

	// The goal is planted before the listener accepts, so a user who follows
	// the printed URL never sees an empty board that fills in underneath them.
	if cfg.Setup != nil {
		if err := cfg.Setup(context.Background(), &stack{
			store: store, artifacts: artifacts, worktrees: worktrees,
			integrator: integrator, runtime: runtime, mode: mode,
		}); err != nil {
			return err
		}
	}

	// Startup diagnostics are deliberately free of credentials and paths that
	// could contain them.
	printReady(listener.Addr().String(), databasePath, mode, scenario)
	if cfg.Announce != nil {
		cfg.Announce(listener.Addr().String())
	}

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

const (
	modeFake  = "fake"
	modeRelay = "relay"
)

// Relay configuration comes from the environment. A credential passed as a flag
// would be visible in shell history and in the process list, so it is not
// accepted there at all.
const (
	envBaseURL       = "AGO_RELAY_BASE_URL"
	envAPIKey        = "AGO_RELAY_API_KEY"
	envPlannerModel  = "AGO_PLANNER_MODEL"
	envExecutorModel = "AGO_EXECUTOR_MODEL"
	envVerifierModel = "AGO_VERIFIER_MODEL"
)

// buildRoles constructs the three roles for a mode.
//
// They are separate implementations, not one object under three names. In relay
// mode they may share a transport and even an endpoint, but each makes its own
// call with its own model mapping — which is what makes the verifier's opinion
// independent of the executor's.
func buildRoles(mode, scenario string, artifacts *agoartifact.Store, worktrees *agoworktree.Manager, integrator *agointegrate.Integrator) (agoplanner.Planner, agoboardruntime.Executor, agoverify.Judge, error) {
	switch mode {
	case modeFake:
		provider, err := agofake.New(agofake.Script{Default: agofake.Outcome(scenario)})
		if err != nil {
			return nil, nil, nil, err
		}
		judge, err := agofake.NewVerifier(agofake.Script{Default: agofake.Outcome(scenario)})
		if err != nil {
			return nil, nil, nil, err
		}
		return agoplanner.DemoPlanner{}, provider.WithArtifacts(artifacts), judge, nil
	case modeRelay:
		baseURL := os.Getenv(envBaseURL)
		if strings.TrimSpace(baseURL) == "" {
			return nil, nil, nil, fmt.Errorf("relay mode requires %s", envBaseURL)
		}
		if strings.TrimSpace(os.Getenv(envAPIKey)) == "" {
			return nil, nil, nil, fmt.Errorf("relay mode requires %s in the environment (never as a flag)", envAPIKey)
		}
		client := func(model, fallback string) (*agorelay.Client, error) {
			name := os.Getenv(model)
			if strings.TrimSpace(name) == "" {
				name = fallback
			}
			return agorelay.New(agorelay.Profile{
				ID: "relay-" + name, BaseURL: baseURL, Model: name, APIKeyEnv: envAPIKey,
				Timeout: 3 * time.Minute, MaxOutputBytes: 1 << 20,
			}, nil, os.Getenv)
		}
		plannerClient, err := client(envPlannerModel, "claude-sonnet-5")
		if err != nil {
			return nil, nil, nil, err
		}
		executorClient, err := client(envExecutorModel, "claude-sonnet-5")
		if err != nil {
			return nil, nil, nil, err
		}
		verifierClient, err := client(envVerifierModel, "claude-sonnet-5")
		if err != nil {
			return nil, nil, nil, err
		}
		planner, err := agorelayplanner.New(agorelayplanner.Options{Model: plannerClient})
		if err != nil {
			return nil, nil, nil, err
		}
		executor, err := agoexec.New(agoexec.Options{
			Model: executorClient, Worktrees: worktrees, Artifacts: artifacts,
			Commands: agoexec.SystemCommands{}, Timeout: 5 * time.Minute,
		})
		if err != nil {
			return nil, nil, nil, err
		}
		judge, err := agorelayverifier.New(agorelayverifier.Options{Model: verifierClient})
		if err != nil {
			return nil, nil, nil, err
		}
		return planner, executor, agoverify.RelayJudge{Verifier: judge}, nil
	}
	return nil, nil, nil, fmt.Errorf("unsupported mode %q", mode)
}

// describeRoles reports the capability roster. It states whether a credential
// is configured and never what it is, and it never echoes the base URL, which
// can itself carry credentials in a query string.
func describeRoles(mode string) []agoboardapi.Provider {
	if mode == modeFake {
		return []agoboardapi.Provider{
			{ID: "ago-demo-planner", Kind: "planner", Capabilities: []string{"planning"}},
			{ID: "ago-demo-executor", Kind: "executor", Capabilities: []string{"repo-read", "repo-write", "tests", "report"}},
			{ID: "ago-demo-verifier", Kind: "verifier", Capabilities: []string{"verification"}},
		}
	}
	configured := strings.TrimSpace(os.Getenv(envAPIKey)) != ""
	model := func(name, fallback string) string {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
		return fallback
	}
	return []agoboardapi.Provider{
		{ID: "relay-planner", Kind: "planner", Model: model(envPlannerModel, "claude-sonnet-5"),
			Capabilities: []string{"planning"}, AuthConfigured: configured},
		{ID: "relay-executor", Kind: "executor", Model: model(envExecutorModel, "claude-sonnet-5"),
			Capabilities: []string{"repo-read", "repo-write", "tests"}, AuthConfigured: configured},
		{ID: "relay-verifier", Kind: "verifier", Model: model(envVerifierModel, "claude-sonnet-5"),
			Capabilities: []string{"verification"}, AuthConfigured: configured},
	}
}

// printReady prints startup diagnostics. Nothing here can carry a credential:
// the mode, the model names, and local paths only.
func printReady(address, databasePath, mode, scenario string) {
	fmt.Printf("Ago is ready\nUI:       http://%s\nDatabase: %s\nMode:     %s\n", address, databasePath, mode)
	if mode == modeFake {
		fmt.Printf("Scenario: %s\n", scenario)
		// Said plainly, because it would otherwise be easy to mistake this for
		// a model doing the work. Fake mode exercises the machinery — the
		// graph, the leases, the independent verifier, the integration ref —
		// against a fixed plan and scripted outcomes. Use --executor relay for
		// a run where a model actually decides anything.
		fmt.Println("Note:     fake mode runs a fixed plan with scripted outcomes; no model decides anything.")
		return
	}
	fmt.Printf("Planner:  %s\nExecutor: %s\nVerifier: %s\nAuth:     %s\n",
		envOr(envPlannerModel, "claude-sonnet-5"), envOr(envExecutorModel, "claude-sonnet-5"),
		envOr(envVerifierModel, "claude-sonnet-5"), configuredLabel())
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func configuredLabel() string {
	if strings.TrimSpace(os.Getenv(envAPIKey)) != "" {
		return "configured"
	}
	return "missing"
}
