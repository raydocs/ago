// Package agoserve is the orchestration stack every Ago entry point runs.
//
// It exists so there is exactly one of it. The demo and the server are the
// same machine with different front doors, and a second copy of the wiring —
// store, artifacts, worktrees, integrator, runtime, scheduler, supervisor,
// API — is a second place for the roles to drift apart.
package agoserve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"claudexflow/internal/agoartifact"
	"claudexflow/internal/agoboardapi"
	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agoboardstore"
	"claudexflow/internal/agoboardui"
	"claudexflow/internal/agoexec"
	"claudexflow/internal/agofake"
	"claudexflow/internal/agogate"
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
)

// Config is one server's whole configuration. Setup runs once the stack
// exists and before the listener accepts, which is where the demo plants its
// goal — a board created through the same runtime any client would use.
type Config struct {
	DatabasePath string
	Listen       string
	Mode         string
	Scenario     string
	Setup        func(context.Context, *Stack) error
	// Announce prints whatever the caller wants a user to read once the
	// address is known.
	Announce func(address string)
	// Out is where startup diagnostics go. Defaults to os.Stdout.
	Out io.Writer
}

// stack is everything one server owns.
type Stack struct {
	store      *agoboardstore.Store
	artifacts  *agoartifact.Store
	worktrees  *agoworktree.Manager
	integrator *agointegrate.Integrator
	runtime    *agoboardruntime.Runtime
	mode       string
}

func Serve(cfg Config) error {
	if cfg.Out == nil {
		cfg.Out = os.Stdout
	}
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
	// The project gate proves the integrated result. It runs the repository's
	// own checks in a throwaway checkout, so a goal completes on what was
	// promoted rather than on every task having been individually accepted.
	gate, err := agogate.New(agogate.Options{
		Commands: agoexec.SystemCommands{}, Worktrees: integrator, Timeout: 10 * time.Minute,
	})
	if err != nil {
		return err
	}
	scheduler, err := agoscheduler.New(agoscheduler.Options{
		Store: store, Runtime: runtime, Executor: executor, Verification: verification,
		Integrator: integrator, Artifacts: artifacts, Gate: gate,
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
		if err := cfg.Setup(context.Background(), &Stack{
			store: store, artifacts: artifacts, worktrees: worktrees,
			integrator: integrator, runtime: runtime, mode: mode,
		}); err != nil {
			return err
		}
	}

	// Startup diagnostics are deliberately free of credentials and paths that
	// could contain them.
	printReady(cfg.Out, listener.Addr().String(), databasePath, mode, scenario)
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

const (
	ModeFake  = "fake"
	ModeRelay = "relay"
)

// Relay configuration comes from the environment. A credential passed as a flag
// would be visible in shell history and in the process list, so it is not
// accepted there at all.
const (
	EnvBaseURL       = "AGO_RELAY_BASE_URL"
	EnvAPIKey        = "AGO_RELAY_API_KEY"
	EnvPlannerModel  = "AGO_PLANNER_MODEL"
	EnvExecutorModel = "AGO_EXECUTOR_MODEL"
	EnvVerifierModel = "AGO_VERIFIER_MODEL"
)

// buildRoles constructs the three roles for a mode.
//
// They are separate implementations, not one object under three names. In relay
// mode they may share a transport and even an endpoint, but each makes its own
// call with its own model mapping — which is what makes the verifier's opinion
// independent of the executor's.
func buildRoles(mode, scenario string, artifacts *agoartifact.Store, worktrees *agoworktree.Manager, integrator *agointegrate.Integrator) (agoplanner.Planner, agoboardruntime.Executor, agoverify.Judge, error) {
	switch mode {
	case ModeFake:
		provider, err := agofake.New(agofake.Script{Default: agofake.Outcome(scenario)})
		if err != nil {
			return nil, nil, nil, err
		}
		judge, err := agofake.NewVerifier(agofake.Script{Default: agofake.Outcome(scenario)})
		if err != nil {
			return nil, nil, nil, err
		}
		return agoplanner.DemoPlanner{}, provider.WithArtifacts(artifacts), judge, nil
	case ModeRelay:
		baseURL := os.Getenv(EnvBaseURL)
		if strings.TrimSpace(baseURL) == "" {
			return nil, nil, nil, fmt.Errorf("relay mode requires %s", EnvBaseURL)
		}
		if strings.TrimSpace(os.Getenv(EnvAPIKey)) == "" {
			return nil, nil, nil, fmt.Errorf("relay mode requires %s in the environment (never as a flag)", EnvAPIKey)
		}
		client := func(model, fallback string) (*agorelay.Client, error) {
			name := os.Getenv(model)
			if strings.TrimSpace(name) == "" {
				name = fallback
			}
			return agorelay.New(agorelay.Profile{
				ID: "relay-" + name, BaseURL: baseURL, Model: name, APIKeyEnv: EnvAPIKey,
				Timeout: 3 * time.Minute, MaxOutputBytes: 1 << 20,
			}, nil, os.Getenv)
		}
		plannerClient, err := client(EnvPlannerModel, "claude-sonnet-5")
		if err != nil {
			return nil, nil, nil, err
		}
		executorClient, err := client(EnvExecutorModel, "claude-sonnet-5")
		if err != nil {
			return nil, nil, nil, err
		}
		verifierClient, err := client(EnvVerifierModel, "claude-sonnet-5")
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
	if mode == ModeFake {
		return []agoboardapi.Provider{
			{ID: "ago-demo-planner", Kind: "planner", Capabilities: []string{"planning"}},
			{ID: "ago-demo-executor", Kind: "executor", Capabilities: []string{"repo-read", "repo-write", "tests", "report"}},
			{ID: "ago-demo-verifier", Kind: "verifier", Capabilities: []string{"verification"}},
		}
	}
	configured := strings.TrimSpace(os.Getenv(EnvAPIKey)) != ""
	model := func(name, fallback string) string {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
		return fallback
	}
	return []agoboardapi.Provider{
		{ID: "relay-planner", Kind: "planner", Model: model(EnvPlannerModel, "claude-sonnet-5"),
			Capabilities: []string{"planning"}, AuthConfigured: configured},
		{ID: "relay-executor", Kind: "executor", Model: model(EnvExecutorModel, "claude-sonnet-5"),
			Capabilities: []string{"repo-read", "repo-write", "tests"}, AuthConfigured: configured},
		{ID: "relay-verifier", Kind: "verifier", Model: model(EnvVerifierModel, "claude-sonnet-5"),
			Capabilities: []string{"verification"}, AuthConfigured: configured},
	}
}

// printReady prints startup diagnostics. Nothing here can carry a credential:
// the mode, the model names, and local paths only.
func printReady(out io.Writer, address, databasePath, mode, scenario string) {
	fmt.Fprintf(out, "Ago is ready\nUI:       http://%s\nDatabase: %s\nMode:     %s\n", address, databasePath, mode)
	if mode == ModeFake {
		fmt.Fprintf(out, "Scenario: %s\n", scenario)
		// Said plainly, because it would otherwise be easy to mistake this for
		// a model doing the work. Fake mode exercises the machinery — the
		// graph, the leases, the independent verifier, the integration ref —
		// against a fixed plan and scripted outcomes. Use --executor relay for
		// a run where a model actually decides anything.
		fmt.Fprintln(out, "Note:     fake mode runs a fixed plan with scripted outcomes; no model decides anything.")
		return
	}
	fmt.Fprintf(out, "Planner:  %s\nExecutor: %s\nVerifier: %s\nAuth:     %s\n",
		envOr(EnvPlannerModel, "claude-sonnet-5"), envOr(EnvExecutorModel, "claude-sonnet-5"),
		envOr(EnvVerifierModel, "claude-sonnet-5"), configuredLabel())
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func configuredLabel() string {
	if strings.TrimSpace(os.Getenv(EnvAPIKey)) != "" {
		return "configured"
	}
	return "missing"
}
