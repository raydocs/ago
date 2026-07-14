package panel

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"claudexflow/internal/claude"
)

type Outcome struct {
	Panelists []claude.Result `json:"panelists"`
	Judge     claude.Result   `json:"judge"`
	Success   bool            `json:"success"`
}

// Run executes the explicit high-cost mode: two independent answers and one
// judge. It is never called by the normal workflow path.
func Run(ctx context.Context, settings, workDir, task string, timeout time.Duration) Outcome {
	models := []struct{ model, effort string }{{"gpt-5.6-sol", "xhigh"}, {"gemini-3.1-pro", "high"}}
	results := make([]claude.Result, len(models))
	var wg sync.WaitGroup
	for i, m := range models {
		wg.Add(1)
		go func(i int, model, effort string) {
			defer wg.Done()
			results[i] = claude.Run(ctx, claude.Request{SettingsPath: settings, WorkDir: workDir, Prompt: blindPrompt(task), Model: model, Effort: effort, Tools: []string{"Read", "Grep", "Glob"}, Timeout: timeout})
		}(i, m.model, m.effort)
	}
	wg.Wait()
	for _, r := range results {
		if !r.Success {
			return Outcome{Panelists: results, Success: false}
		}
	}
	judgePrompt := fmt.Sprintf("You are the final judge. Resolve the task using the two independent answers below. Do not choose by majority. Compare evidence, identify contradictions, and output one concise answer with remaining uncertainty.\n\nTask:\n%s\n\nAnswer A:\n%s\n\nAnswer B:\n%s", task, limit(results[0].Text), limit(results[1].Text))
	judge := claude.Run(ctx, claude.Request{AuthMode: claude.AuthNativeSubscription, WorkDir: workDir, Prompt: judgePrompt, Model: "opus", Effort: "high", Tools: []string{"Read", "Grep", "Glob"}, Timeout: timeout})
	return Outcome{Panelists: results, Judge: judge, Success: judge.Success}
}

func blindPrompt(task string) string {
	return "Independently solve or assess the task below. You will not see another model's answer. Use evidence, state assumptions, focus only on material risks, and be concise.\n\nTask:\n" + task
}

func limit(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 20000 {
		return s[:20000] + "\n...[truncated]"
	}
	return s
}
