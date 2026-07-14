package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"claudexflow/internal/catalog"
	"claudexflow/internal/compactaudit"
	"claudexflow/internal/configure"
	"claudexflow/internal/mcpserver"
	"claudexflow/internal/panel"
	"claudexflow/internal/probe"
	"claudexflow/internal/routeeval"
	"claudexflow/internal/routehint"
	"claudexflow/internal/router"
	"claudexflow/internal/stallwatch"
	"claudexflow/internal/threadfind"
	"claudexflow/internal/threadsync"
	"claudexflow/internal/workflow"
)

var version = "dev"
var buildSource = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "models":
		err = models()
	case "route":
		err = route(os.Args[2:])
	case "run":
		err = run(ctx, os.Args[2:])
	case "review":
		err = review(ctx, os.Args[2:])
	case "max":
		err = maxMode(ctx, os.Args[2:])
	case "probe":
		err = probeMode(ctx, os.Args[2:])
	case "doctor":
		err = doctor()
	case "compact-audit":
		err = compactAudit(os.Args[2:])
	case "route-eval":
		err = routeEval(os.Args[2:])
	case "contract":
		err = printJSON(mcpserver.Contract())
	case "contract-guard":
		if guardErr := contractGuard(); guardErr != nil {
			fmt.Fprintln(os.Stderr, "CLAUDEX_CONTRACT_MISMATCH:", guardErr)
			os.Exit(2)
		}
		return
	case "configure-hooks":
		var changed bool
		changed, err = configure.EnsureRouteHint(defaultSettings(), defaultBinary())
		if err == nil {
			err = printJSON(map[string]any{"changed": changed, "route_hint": "installed"})
		}
	case "route-hint":
		if os.Getenv("CLAUDE_CODE_CHILD_SESSION") == "1" {
			return
		}
		var output []byte
		output, err = routehint.Build(os.Stdin)
		if err == nil && len(output) > 0 {
			_, err = os.Stdout.Write(append(output, '\n'))
		}
	case "mcp":
		err = mcpserver.Run(ctx, version, defaultSettings())
	case "thread-hook":
		delivery := threadsync.Collect(ctx, os.Stdin)
		if delivery.Detail != "" && !delivery.Spooled {
			fmt.Fprintln(os.Stderr, delivery.Detail)
		}
	case "thread-sync":
		var sent, pending int
		sent, pending, err = threadsync.Flush(ctx)
		if err == nil {
			err = printJSON(map[string]int{"sent": sent, "pending": pending})
		}
	case "thread-backfill":
		err = threadBackfill(ctx, os.Args[2:])
	case "thread-status":
		var status threadsync.Status
		status, err = threadsync.GetStatus()
		if err == nil {
			err = printJSON(status)
		}
	case "thread-find":
		err = threadFind(os.Args[2:])
	case "thread-open":
		err = threadsync.OpenDashboard()
	case "stall-watch":
		stallWatch(ctx)
		return
	case "version", "--version", "-v":
		fmt.Println(version)
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `claudex-flow: evidence-gated, cost-bounded multi-model workflow

Commands:
  models                    list curated models and effort levels
  route --task TEXT         show the zero-token deterministic route
  run --task TEXT           execute one model; verify; optionally repair once
  review --task TEXT        targeted one-model review
  max --task TEXT           explicit 3-call maximum-quality mode
  probe [--models LIST]     bounded Bash tool-call capability probe
  doctor                    verify local runtime and configuration
  compact-audit             inspect transcript compaction without a model call
  route-eval                run a predeclared zero-model route policy suite
  contract                  print the compiled workflow contract
  contract-guard            fail fast when orchestrator/runtime contracts drift
  configure-hooks           idempotently install root zero-model route hint
  route-hint                internal zero-model UserPromptSubmit route hint
  mcp                       run the Claude X specialist MCP server over stdio
  thread-hook               ingest one Claude Code hook event from stdin
  thread-sync               retry locally spooled cloud events
  thread-backfill           replay one transcript in bounded idempotent batches
  thread-status             show cloud Thread sync state
  thread-find               search local root Threads without a model call
  thread-open               open the public cloud Thread dashboard
  stall-watch               internal asyncRewake guard for stalled tool continuations
`)
}

func threadFind(args []string) error {
	fs := flag.NewFlagSet("thread-find", flag.ContinueOnError)
	query := fs.String("query", "", "keywords or Amp-style filters")
	file := fs.String("file", "", "file path mentioned or modified")
	project := fs.String("project", "", "project/repository/cwd fragment")
	after := fs.String("after", "", "RFC3339, YYYY-MM-DD, or Nd")
	before := fs.String("before", "", "RFC3339, YYYY-MM-DD, or Nd")
	exclude := fs.String("exclude-thread-id", "", "thread ID to omit")
	limit := fs.Int("limit", 8, "maximum results, 1 to 25")
	root := fs.String("transcript-root", "", "override ~/.claude/projects for tests")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := threadfind.Find(*root, threadfind.Query{
		Text: *query, File: *file, Project: *project, After: *after, Before: *before,
		ExcludeThreadID: *exclude, Limit: *limit,
	})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func stallWatch(ctx context.Context) {
	timeout := 5 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("CLAUDEX_STALL_TIMEOUT_SECONDS")); raw != "" {
		if parsed, parseErr := time.ParseDuration(raw + "s"); parseErr == nil && parsed >= 30*time.Second && parsed <= 30*time.Minute {
			timeout = parsed
		}
	}
	stateDir := strings.TrimSpace(os.Getenv("CLAUDEX_STALL_STATE_DIR"))
	if stateDir == "" {
		stateDir = stallwatch.DefaultStateDir()
	}
	out, err := stallwatch.Watch(ctx, os.Stdin, stallwatch.Config{Timeout: timeout, Poll: time.Second, StateDir: stateDir})
	if err != nil {
		// A watchdog must fail open rather than block or wake a healthy session.
		return
	}
	if out.State == "stalled" {
		fmt.Fprintln(os.Stderr, out.Message)
		os.Exit(2)
	}
}

func models() error {
	return printJSON(catalog.All())
}

func compactAudit(args []string) error {
	fs := flag.NewFlagSet("compact-audit", flag.ContinueOnError)
	transcript := fs.String("transcript", "", "authoritative Claude transcript JSONL path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*transcript) == "" {
		return fmt.Errorf("--transcript is required")
	}
	report, err := compactaudit.Parse(*transcript)
	if err != nil {
		return err
	}
	return printJSON(report)
}

func routeEval(args []string) error {
	fs := flag.NewFlagSet("route-eval", flag.ContinueOnError)
	suite := fs.String("suite", "", "predeclared route policy suite JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*suite) == "" {
		return fmt.Errorf("--suite is required")
	}
	report, err := routeeval.Run(*suite)
	if err != nil {
		return err
	}
	if err := printJSON(report); err != nil {
		return err
	}
	if report.Status != "PASS" {
		return fmt.Errorf("route policy suite failed: %d of %d cases", report.Failed, report.Cases)
	}
	return nil
}

func route(args []string) error {
	fs := flag.NewFlagSet("route", flag.ContinueOnError)
	task := fs.String("task", "", "task to route")
	kind := fs.String("kind", "auto", "task kind")
	risk := fs.String("risk", "normal", "normal or high")
	model := fs.String("model", "", "explicit model override")
	effort := fs.String("effort", "", "explicit effort override")
	urls := fs.String("urls", "", "comma-separated already-known URLs")
	acceptance := fs.String("acceptance", "", "observable criteria separated by ||")
	verificationTarget := fs.String("verification-target", "", "exact command, probe, artifact check, or bounded semantic review")
	workerMarginalContribution := fs.String("worker-marginal-contribution", "", "concrete Supervisor work or critical-path delay the Worker avoids")
	independentSlices := fs.Int("independent-slices", 0, "genuinely independent bounded slices, 0 to 3")
	sharedState := fs.Bool("shared-state", false, "candidate workstreams share mutable state")
	checkability := fs.String("checkability", "auto", "auto, objective, partial, or semantic")
	topology := fs.String("topology", "auto", "auto, direct, or worker")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*task) == "" {
		return fmt.Errorf("--task is required")
	}
	if *model != "" || *effort != "" {
		if *urls != "" || *acceptance != "" || *verificationTarget != "" || *workerMarginalContribution != "" || *independentSlices != 0 || *sharedState || *checkability != "auto" || *topology != "auto" {
			return fmt.Errorf("explicit --model/--effort cannot be combined with structural route hints")
		}
		d, err := router.Decide(*task, router.Kind(*kind), *risk, *model, *effort)
		if err != nil {
			return err
		}
		return printJSON(d)
	}
	plan, err := router.PlanRoute(router.RouteRequest{
		Objective: *task, Kind: router.Kind(*kind), Risk: *risk, ExplicitURLs: splitList(*urls),
		AcceptanceCriteria: splitCriteria(*acceptance), VerificationTarget: *verificationTarget, WorkerMarginalContribution: *workerMarginalContribution,
		IndependentSlices: *independentSlices, SharedMutableState: *sharedState,
		Checkability: *checkability, Topology: *topology,
	})
	if err != nil {
		return err
	}
	return printJSON(plan)
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	task := fs.String("task", "", "objective")
	kind := fs.String("kind", "auto", "task kind")
	risk := fs.String("risk", "normal", "normal or high")
	model := fs.String("model", "", "model override")
	effort := fs.String("effort", "", "effort override")
	verify := fs.String("verify", "", "deterministic verification shell command")
	write := fs.Bool("write", false, "enable Edit, Write and Bash tools")
	repair := fs.Bool("repair", true, "allow one same-model repair only after verifier failure")
	workdir := fs.String("workdir", ".", "working directory")
	settings := fs.String("settings", defaultSettings(), "Claude X settings path")
	timeout := fs.Duration("timeout", 10*time.Minute, "per model timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*task) == "" {
		return fmt.Errorf("--task is required")
	}
	abs, err := filepath.Abs(*workdir)
	if err != nil {
		return err
	}
	out, execErr := workflow.Execute(ctx, workflow.Options{Task: *task, Kind: router.Kind(*kind), Risk: *risk, Model: *model, Effort: *effort, VerifyCommand: *verify, WorkDir: abs, SettingsPath: *settings, Write: *write, Repair: *repair, Timeout: *timeout})
	_ = printJSON(out)
	return execErr
}

func review(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	task := fs.String("task", "", "artifact and exact review focus")
	workdir := fs.String("workdir", ".", "working directory")
	settings := fs.String("settings", defaultSettings(), "Claude X settings path")
	model := fs.String("model", "opus", "one reviewer model")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *task == "" {
		return fmt.Errorf("--task is required")
	}
	abs, _ := filepath.Abs(*workdir)
	return run(ctx, []string{"--task", "Review only the material risk in this artifact. Do not redo the work. " + *task, "--kind", "general", "--model", *model, "--risk", "high", "--workdir", abs, "--settings", *settings, "--repair=false"})
}

func maxMode(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("max", flag.ContinueOnError)
	task := fs.String("task", "", "high-risk task")
	workdir := fs.String("workdir", ".", "working directory")
	settings := fs.String("settings", defaultSettings(), "Claude X settings path")
	timeout := fs.Duration("timeout", 15*time.Minute, "per model timeout")
	confirm := fs.Bool("confirm-cost", false, "acknowledge explicit 3-call mode")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *task == "" {
		return fmt.Errorf("--task is required")
	}
	if !*confirm {
		return fmt.Errorf("max mode makes 3 model calls; pass --confirm-cost")
	}
	abs, _ := filepath.Abs(*workdir)
	out := panel.Run(ctx, *settings, abs, *task, *timeout)
	if err := printJSON(out); err != nil {
		return err
	}
	if !out.Success {
		return fmt.Errorf("panel did not complete")
	}
	return nil
}

func probeMode(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	list := fs.String("models", "opus,sonnet,claude-fable-5,gpt-5.6-sol,gpt-5.6-luna,gpt-5.6-terra,gemini-3.1-pro,gemini-3.5-flash,grok-4.5,glm-5.2", "comma-separated models")
	workdir := fs.String("workdir", ".", "working directory")
	settings := fs.String("settings", defaultSettings(), "Claude X settings path")
	timeout := fs.Duration("timeout", 3*time.Minute, "per model timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}
	abs, _ := filepath.Abs(*workdir)
	results, err := probe.ToolCall(ctx, *settings, abs, splitList(*list), *timeout)
	if err != nil {
		return err
	}
	if err := printJSON(results); err != nil {
		return err
	}
	for _, r := range results {
		if !r.Success {
			return fmt.Errorf("one or more probes failed")
		}
	}
	return nil
}

func threadBackfill(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("thread-backfill", flag.ContinueOnError)
	sessionID := fs.String("session-id", "", "session ID to replay")
	rootSessionID := fs.String("root-session-id", "", "root session ID; defaults to session ID")
	parentSessionID := fs.String("parent-session-id", "", "optional parent session ID")
	transcript := fs.String("transcript", "", "authoritative transcript JSONL path")
	cwd := fs.String("cwd", "", "recorded workspace path")
	model := fs.String("model", "", "root model when known")
	effort := fs.String("effort", "", "root effort when known")
	role := fs.String("role", "supervisor", "thread role")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*sessionID) == "" || strings.TrimSpace(*transcript) == "" {
		return fmt.Errorf("--session-id and --transcript are required")
	}
	cfg, _, err := threadsync.LoadConfig()
	if err != nil {
		return err
	}
	result, err := threadsync.Backfill(ctx, threadsync.BackfillOptions{
		Config:          cfg,
		SessionID:       *sessionID,
		RootSessionID:   *rootSessionID,
		ParentSessionID: *parentSessionID,
		TranscriptPath:  *transcript,
		CWD:             *cwd,
		Model:           *model,
		Effort:          *effort,
		Role:            *role,
	})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func doctor() error {
	checks := map[string]any{
		"settings":       defaultSettings(),
		"models":         len(catalog.All()),
		"contract":       mcpserver.Contract(),
		"binary_version": version,
		"build_source":   buildSource,
	}
	var failures []string
	home, _ := os.UserHomeDir()
	expectedSource := filepath.Join(home, "orca", "projects", "x")
	if buildSource != expectedSource {
		failures = append(failures, fmt.Sprintf("installed binary source mismatch: got %q, want %q", buildSource, expectedSource))
	}
	routeSuite := filepath.Join(expectedSource, "config", "route-policy-suite-v1.3.json")
	if report, err := routeeval.Run(routeSuite); err != nil {
		checks["route_policy_status"] = err.Error()
		failures = append(failures, "route policy suite is unreadable or invalid")
	} else if report.Status != "PASS" {
		checks["route_policy_status"] = fmt.Sprintf("FAIL: %d/%d cases", report.Failed, report.Cases)
		failures = append(failures, "route policy suite failed")
	} else {
		checks["route_policy_status"] = fmt.Sprintf("PASS: %d/%d cases", report.Passed, report.Cases)
	}
	for _, name := range []string{"claude", "cli-proxy-api"} {
		path, err := exec.LookPath(name)
		if err != nil {
			checks[name] = "missing"
			failures = append(failures, name+" binary is missing")
		} else {
			checks[name] = path
		}
	}
	settings, err := loadJSONObject(defaultSettings())
	if err != nil {
		checks["settings_status"] = err.Error()
		failures = append(failures, "settings are unreadable")
	} else {
		var settingsFailures []string
		env, _ := settings["env"].(map[string]any)
		if fmt.Sprint(env["MCP_TOOL_TIMEOUT"]) != "120000" {
			settingsFailures = append(settingsFailures, "MCP_TOOL_TIMEOUT must be 120000")
			failures = append(failures, "ordinary MCP timeout is not 120000ms")
		}
		guardCommand := filepath.Join(home, ".local", "bin", "claudex-flow")
		if !hasCommandHook(settings, "SessionStart", guardCommand, []string{"contract-guard"}) {
			settingsFailures = append(settingsFailures, "SessionStart contract-guard is missing")
			failures = append(failures, "SessionStart contract-guard is missing")
		}
		if !hasCommandHook(settings, "UserPromptSubmit", guardCommand, []string{"route-hint"}) {
			settingsFailures = append(settingsFailures, "UserPromptSubmit route-hint is missing")
			failures = append(failures, "UserPromptSubmit zero-model route hint is missing")
		}
		for _, event := range []string{"PreCompact", "PostCompact"} {
			if !hasCommandHook(settings, event, guardCommand, []string{"thread-hook"}) {
				settingsFailures = append(settingsFailures, event+" thread-hook is missing")
				failures = append(failures, event+" Thread observability hook is missing")
			}
		}
		if len(settingsFailures) == 0 {
			checks["settings_status"] = "ok"
		} else {
			checks["settings_status"] = strings.Join(settingsFailures, "; ")
		}
	}
	mcpConfigPath := defaultMCPConfig()
	mcpConfig, err := loadJSONObject(mcpConfigPath)
	if err != nil {
		checks["mcp_config_status"] = err.Error()
		failures = append(failures, "MCP config is unreadable")
	} else {
		servers, _ := mcpConfig["mcpServers"].(map[string]any)
		flow, _ := servers["claudex-flow"].(map[string]any)
		timeout, _ := flow["timeout"].(float64)
		expectedCommand := filepath.Join(home, ".local", "bin", "claudex-flow")
		if fmt.Sprint(flow["command"]) != expectedCommand || int64(timeout) < maxWorkerServerTimeoutMS() {
			checks["mcp_config_status"] = "claudex-flow command/timeout mismatch"
			failures = append(failures, "claudex-flow MCP server is not pinned with sufficient timeout")
		} else {
			checks["mcp_config_status"] = "ok"
		}
	}
	if err := mcpserver.ValidateOrchestrator(defaultOrchestrator()); err != nil {
		checks["orchestrator_status"] = err.Error()
		failures = append(failures, err.Error())
	} else {
		checks["orchestrator_status"] = "ok"
	}
	conn, err := net.DialTimeout("tcp", "127.0.0.1:8318", time.Second)
	if err != nil {
		checks["gateway_127.0.0.1:8318"] = err.Error()
		failures = append(failures, "gateway is unreachable")
	} else {
		checks["gateway_127.0.0.1:8318"] = "reachable"
		_ = conn.Close()
	}
	checks["ok"] = len(failures) == 0
	checks["failures"] = failures
	if err := printJSON(checks); err != nil {
		return err
	}
	if len(failures) > 0 {
		return fmt.Errorf("doctor found %d runtime contract failure(s)", len(failures))
	}
	return nil
}

func contractGuard() error {
	return mcpserver.ValidateOrchestrator(defaultOrchestrator())
}

func loadJSONObject(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func hasCommandHook(settings map[string]any, event, command string, args []string) bool {
	hooks, _ := settings["hooks"].(map[string]any)
	groups, _ := hooks[event].([]any)
	for _, rawGroup := range groups {
		group, _ := rawGroup.(map[string]any)
		entries, _ := group["hooks"].([]any)
		for _, rawEntry := range entries {
			entry, _ := rawEntry.(map[string]any)
			if fmt.Sprint(entry["command"]) != command {
				continue
			}
			rawArgs, _ := entry["args"].([]any)
			if len(rawArgs) != len(args) {
				continue
			}
			matches := true
			for i := range args {
				if fmt.Sprint(rawArgs[i]) != args[i] {
					matches = false
					break
				}
			}
			if matches {
				return true
			}
		}
	}
	return false
}

func maxWorkerServerTimeoutMS() int64 { return 660_000 }

func defaultSettings() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claudex", "settings.json")
}

func defaultBinary() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin", "claudex-flow")
}

func defaultMCPConfig() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claudex", "mcp.json")
}

func defaultOrchestrator() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "claudex", "orchestrator.md")
}

func splitList(s string) []string {
	var out []string
	for _, item := range strings.Split(s, ",") {
		if v := strings.TrimSpace(item); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func splitCriteria(s string) []string {
	var out []string
	for _, item := range strings.Split(s, "||") {
		if value := strings.TrimSpace(item); value != "" {
			out = append(out, value)
		}
	}
	return out
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
