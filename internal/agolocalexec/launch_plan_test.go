package agolocalexec

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLaunchPlanDigestBindsEverySecurityField(t *testing.T) {
	workspace := t.TempDir()
	plan := LaunchPlan{
		Origin:        "model:thread-1/turn-1",
		Executable:    "/usr/bin/printf",
		Arguments:     []string{"hello"},
		WorkingDir:    workspace,
		Environment:   map[string]string{"LANG": "C", "PATH": "/usr/bin:/bin"},
		ReadRoots:     []string{workspace, "/usr/bin"},
		WriteRoots:    []string{workspace},
		SyntheticHome: filepath.Join(workspace, ".ago-home"),
		SyntheticTemp: filepath.Join(workspace, ".ago-tmp"),
		ProfileID:     "ago.model.v1",
		ProfileHash:   "profile-sha256",
		Network:       NetworkDisabled,
		TTY:           false,
		Deadline:      30 * time.Second,
		Output:        OutputBudget{HeadBytes: 4096, TailBytes: 4096},
		Protocol:      ProtocolBudget{ID: "pi-jsonl-v1", MaxFrameBytes: 4096, MaxEvents: 10, MaxEventBytes: 8192, AbortGrace: time.Second},
		ApprovalNonce: "one-use-nonce",
	}
	base, err := plan.Digest()
	if err != nil {
		t.Fatalf("digest base plan: %v", err)
	}

	mutations := map[string]func(*LaunchPlan){
		"origin":         func(p *LaunchPlan) { p.Origin = "model:thread-1/turn-2" },
		"executable":     func(p *LaunchPlan) { p.Executable = "/usr/bin/true" },
		"arguments":      func(p *LaunchPlan) { p.Arguments = []string{"changed"} },
		"stdin":          func(p *LaunchPlan) { p.Stdin = []byte("changed") },
		"working dir":    func(p *LaunchPlan) { p.WorkingDir = filepath.Dir(workspace) },
		"environment":    func(p *LaunchPlan) { p.Environment = map[string]string{"LANG": "en_US.UTF-8", "PATH": "/usr/bin:/bin"} },
		"read roots":     func(p *LaunchPlan) { p.ReadRoots = []string{workspace} },
		"write roots":    func(p *LaunchPlan) { p.WriteRoots = nil },
		"synthetic home": func(p *LaunchPlan) { p.SyntheticHome += "-changed" },
		"synthetic temp": func(p *LaunchPlan) { p.SyntheticTemp += "-changed" },
		"profile id":     func(p *LaunchPlan) { p.ProfileID = "ago.model.v2" },
		"profile hash":   func(p *LaunchPlan) { p.ProfileHash = "changed" },
		"network":        func(p *LaunchPlan) { p.Network = NetworkAllowed },
		"tty":            func(p *LaunchPlan) { p.TTY = true },
		"deadline":       func(p *LaunchPlan) { p.Deadline = time.Minute },
		"output":         func(p *LaunchPlan) { p.Output.HeadBytes++ },
		"protocol":       func(p *LaunchPlan) { p.Protocol.MaxEvents++ },
		"approval nonce": func(p *LaunchPlan) { p.ApprovalNonce = "changed" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := plan.Clone()
			mutate(&changed)
			digest, err := changed.Digest()
			if err != nil && name != "network" && name != "tty" && name != "profile id" {
				t.Fatalf("digest changed plan: %v", err)
			}
			if err == nil && digest == base {
				t.Fatal("security-relevant mutation did not change launch digest")
			}
		})
	}
}

func TestLaunchPlanRejectsUnsafeProtocolBudgets(t *testing.T) {
	p := LaunchPlan{Origin: "test", Executable: "/bin/cat", WorkingDir: t.TempDir(), ReadRoots: []string{"/bin"}, SyntheticHome: "/tmp/home", SyntheticTemp: "/tmp/tmp", ProfileID: "ago.model.v1", ProfileHash: "hash", Network: NetworkDisabled, Deadline: time.Second, Output: OutputBudget{1, 1}, ApprovalNonce: "nonce"}
	for name, budget := range map[string]ProtocolBudget{
		"unknown":    {ID: "other", MaxFrameBytes: 1, MaxEvents: 1, MaxEventBytes: 1, AbortGrace: time.Second},
		"oversized":  {ID: "pi-jsonl-v1", MaxFrameBytes: (1 << 20) + 1, MaxEvents: 1, MaxEventBytes: 1, AbortGrace: time.Second},
		"incomplete": {ID: "pi-jsonl-v1"},
	} {
		t.Run(name, func(t *testing.T) {
			p.Protocol = budget
			if _, err := p.Digest(); err == nil {
				t.Fatal("accepted unsafe protocol budget")
			}
		})
	}
}

func TestLaunchPlanFailsClosedForUnsupportedAutomaticLaunch(t *testing.T) {
	workspace := t.TempDir()
	valid := LaunchPlan{
		Origin: "model:thread-1/turn-1", Executable: "/usr/bin/true", WorkingDir: workspace,
		Environment: map[string]string{"PATH": "/usr/bin:/bin"}, ReadRoots: []string{workspace}, WriteRoots: []string{workspace},
		SyntheticHome: filepath.Join(workspace, ".home"), SyntheticTemp: filepath.Join(workspace, ".tmp"),
		ProfileID: "ago.model.v1", ProfileHash: "sha256", Network: NetworkDisabled,
		Deadline: time.Second, Output: OutputBudget{HeadBytes: 1, TailBytes: 1}, ApprovalNonce: "nonce",
	}

	for name, mutate := range map[string]func(*LaunchPlan){
		"relative executable": func(p *LaunchPlan) { p.Executable = "true" },
		"ambient network":     func(p *LaunchPlan) { p.Network = NetworkAllowed },
		"tty":                 func(p *LaunchPlan) { p.TTY = true },
		"unknown profile":     func(p *LaunchPlan) { p.ProfileID = "ago.unknown.v1" },
		"missing nonce":       func(p *LaunchPlan) { p.ApprovalNonce = "" },
		"empty output budget": func(p *LaunchPlan) { p.Output = OutputBudget{} },
		"noncanonical cwd":    func(p *LaunchPlan) { p.WorkingDir = workspace + "/../" + filepath.Base(workspace) },
	} {
		t.Run(name, func(t *testing.T) {
			plan := valid.Clone()
			mutate(&plan)
			if _, err := plan.Digest(); err == nil {
				t.Fatal("unsupported automatic launch was accepted")
			}
		})
	}
}
