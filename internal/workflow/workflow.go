package workflow

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"claudexflow/internal/claude"
	"claudexflow/internal/ledger"
	"claudexflow/internal/router"
)

type Options struct {
	Task          string
	Kind          router.Kind
	Risk          string
	Model         string
	Effort        string
	VerifyCommand string
	WorkDir       string
	SettingsPath  string
	Write         bool
	Repair        bool
	Timeout       time.Duration
}

type Outcome struct {
	Decision   router.Decision `json:"decision"`
	Attempts   []claude.Result `json:"attempts"`
	Verify     VerifyResult    `json:"verify"`
	Status     string          `json:"status"`
	LedgerPath string          `json:"ledger_path"`
}

type VerifyResult struct {
	Command   string `json:"command,omitempty"`
	Output    string `json:"output,omitempty"`
	Performed bool   `json:"performed"`
	Passed    bool   `json:"passed"`
}

func Execute(ctx context.Context, opts Options) (Outcome, error) {
	decision, err := router.Decide(opts.Task, opts.Kind, opts.Risk, opts.Model, opts.Effort)
	if err != nil {
		return Outcome{}, err
	}
	if opts.Write && decision.Tool != "" {
		return Outcome{}, fmt.Errorf("route selected read-only capability %s; a compound research/explore + write task requires the Claude X Supervisor", decision.Tool)
	}
	tools := executionTools(decision, opts.Write)
	brief := buildBrief(opts.Task, opts.VerifyCommand, opts.Write)
	run := ledger.New(opts.Task, decision.Profile.ID, decision.Profile.DefaultEffort, string(decision.Kind), decision.Risk)
	out := Outcome{Decision: decision, Status: "doing"}

	authMode := claude.AuthModeForProvider(decision.Profile.Provider)
	first := claude.Run(ctx, claude.Request{SettingsPath: opts.SettingsPath, AuthMode: authMode, WorkDir: opts.WorkDir, Prompt: brief, Model: decision.Profile.ID, Effort: decision.Profile.DefaultEffort, Tools: tools, Timeout: opts.Timeout})
	out.Attempts = append(out.Attempts, first)
	if !first.Success {
		out.Status = "failed"
		run.Status = "failed"
		run.Evidence = first
		run.NextStep = "inspect the model execution error; no automatic model fan-out was performed"
		out.LedgerPath = ledger.MustSave(opts.WorkDir, run)
		return out, fmt.Errorf("model execution failed: %s", first.ExitError)
	}

	if strings.TrimSpace(opts.VerifyCommand) == "" {
		out.Verify = VerifyResult{Performed: false, Passed: false, Output: "no deterministic verifier supplied"}
		out.Status = "completed_unverified"
		run.Status = "incomplete"
		run.Evidence = first
		run.NextStep = "supply --verify when a deterministic acceptance check exists"
		out.LedgerPath = ledger.MustSave(opts.WorkDir, run)
		return out, nil
	}

	out.Verify = verify(ctx, opts.WorkDir, opts.VerifyCommand)
	if out.Verify.Passed {
		out.Status = "passed"
		run.Status = "done"
		run.Verification = out.Verify.Command
		run.Evidence = out.Verify
		out.LedgerPath = ledger.MustSave(opts.WorkDir, run)
		return out, nil
	}

	if !opts.Repair || opts.VerifyCommand == "" {
		out.Status = "review"
		run.Status = "incomplete"
		run.Verification = out.Verify.Command
		run.Evidence = out.Verify
		run.NextStep = "verification did not pass; enable --repair for one evidence-scoped retry"
		out.LedgerPath = ledger.MustSave(opts.WorkDir, run)
		return out, nil
	}

	repairPrompt := buildRepairBrief(opts.Task, out.Verify)
	repair := claude.Run(ctx, claude.Request{SettingsPath: opts.SettingsPath, AuthMode: authMode, WorkDir: opts.WorkDir, Prompt: repairPrompt, Model: decision.Profile.ID, Effort: decision.Profile.DefaultEffort, Tools: tools, Timeout: opts.Timeout})
	out.Attempts = append(out.Attempts, repair)
	out.Verify = verify(ctx, opts.WorkDir, opts.VerifyCommand)
	if repair.Success && out.Verify.Passed {
		out.Status = "passed_after_repair"
		run.Status = "done"
		run.Evidence = out.Verify
	} else {
		out.Status = "review"
		run.Status = "incomplete"
		run.Evidence = out.Verify
		run.NextStep = "same-model repair failed; escalate only the unresolved slice or request a decision"
	}
	run.Verification = out.Verify.Command
	out.LedgerPath = ledger.MustSave(opts.WorkDir, run)
	return out, nil
}

func executionTools(decision router.Decision, write bool) []string {
	switch decision.Tool {
	case "search_external":
		return []string{"WebSearch", "WebFetch"}
	case "digest_urls":
		return []string{"WebFetch"}
	case "explore_repository", "consult_native_claude":
		return []string{"Read", "Grep", "Glob"}
	default:
		tools := []string{"Read", "Grep", "Glob"}
		if write {
			tools = append(tools, "Edit", "Write", "Bash")
		}
		return tools
	}
}

func buildBrief(task, verify string, write bool) string {
	mode := "read-only: do not modify files"
	if write {
		mode = "write-enabled: make only changes required by the objective"
	}
	check := "No external verification command was supplied; return concrete evidence and do not claim tests passed unless you ran them."
	if verify != "" {
		check = "The orchestrator will run this exact verification after you finish: " + verify
	}
	return fmt.Sprintf("Objective:\n%s\n\nMode:\n%s\n\nRules:\n- Use the narrowest path that satisfies the objective.\n- Do not expand scope.\n- Do not call or simulate other models.\n- Distinguish facts from assumptions.\n- Stop once the objective is satisfied.\n\nVerification:\n%s\n\nReturn:\nA concise summary, changed paths if any, and evidence.", task, mode, check)
}

func buildRepairBrief(task string, failed VerifyResult) string {
	return fmt.Sprintf("Objective remains:\n%s\n\nA deterministic verification failed. Fix only the bounded defect evidenced below, then stop. Do not redesign unrelated code and do not call another model.\n\nVerification command:\n%s\n\nFailure evidence:\n%s", task, failed.Command, truncate(failed.Output, 12000))
}

func verify(parent context.Context, workDir, command string) VerifyResult {
	if strings.TrimSpace(command) == "" {
		return VerifyResult{Performed: false, Passed: false, Output: "no verifier supplied"}
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "zsh", "-lc", command)
	cmd.Dir = workDir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	err := cmd.Run()
	return VerifyResult{Command: command, Output: truncate(buf.String(), 20000), Performed: true, Passed: err == nil}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n...[truncated]"
}
