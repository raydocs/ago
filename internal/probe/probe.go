package probe

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"claudexflow/internal/catalog"
	"claudexflow/internal/claude"
)

type Result struct {
	Model      string `json:"model"`
	Effort     string `json:"effort,omitempty"`
	Success    bool   `json:"success"`
	ArtifactOK bool   `json:"artifact_ok"`
	BashCalls  int    `json:"bash_calls"`
	DurationMS int64  `json:"duration_ms"`
	Error      string `json:"error,omitempty"`
}

func ToolCall(ctx context.Context, settingsPath, workDir string, models []string, timeout time.Duration) ([]Result, error) {
	root, err := os.MkdirTemp(workDir, ".claudex-flow-probe-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	var results []Result
	for _, id := range models {
		p, ok := catalog.Get(id)
		if !ok {
			return results, fmt.Errorf("unknown model %q", id)
		}
		slug := strings.NewReplacer("/", "-", "[", "-", "]", "", " ", "-").Replace(id)
		artifact := filepath.Join(root, slug+".txt")
		prompt := fmt.Sprintf("Use the Bash tool exactly once to run this exact command: printf 'TOOL_OK\\n' > '%s'. Then reply exactly DONE. Do not use any other tool.", artifact)
		run := claude.Run(ctx, claude.Request{SettingsPath: settingsPath, AuthMode: claude.AuthModeForProvider(p.Provider), WorkDir: workDir, Prompt: prompt, Model: id, Effort: p.DefaultEffort, Tools: []string{"Bash"}, MaxTurns: 3, Timeout: timeout})
		data, readErr := os.ReadFile(artifact)
		artifactOK := readErr == nil && strings.TrimSpace(string(data)) == "TOOL_OK"
		r := Result{Model: id, Effort: p.DefaultEffort, Success: run.Success && artifactOK && run.ToolUses["Bash"] >= 1, ArtifactOK: artifactOK, BashCalls: run.ToolUses["Bash"], DurationMS: run.DurationMS}
		if !r.Success {
			r.Error = strings.TrimSpace(run.ExitError + " " + run.Stderr)
			if readErr != nil {
				r.Error = strings.TrimSpace(r.Error + " artifact: " + readErr.Error())
			}
		}
		results = append(results, r)
	}
	return results, nil
}
