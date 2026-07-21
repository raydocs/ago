package agoserve

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"claudexflow/internal/agoboardruntime"
	"claudexflow/internal/agodemo"
	"claudexflow/internal/agointegrate"
	"claudexflow/internal/agoplanner"
	"claudexflow/internal/agorelay"
)

// Demo is the one command that has to work on a machine that has never run Ago
// before.
//
// It makes a real sample repository, checks that this machine can actually do
// the work before promising anything, states a Chinese goal, and serves the
// board. Every failure it can foresee is reported as a sentence about what to
// change, not as a stack trace at minute four.
func Demo(args []string, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	defaultState := filepath.Join(home, ".ago", "demo")

	flags := flag.NewFlagSet("ago demo", flag.ContinueOnError)
	flags.SetOutput(out)
	state := flags.String("state", defaultState, "目录：示例仓库、数据库、工件都放在这里")
	listen := flags.String("listen", "127.0.0.1:0", "回环监听地址；端口为 0 表示自动挑一个空闲端口")
	executor := flags.String("executor", ModeFake, `谁来干活："fake"（离线，不需要任何凭据）或 "relay"（真实模型）`)
	goal := flags.String("goal", agodemo.Objective, "要交给 Ago 的目标")
	open := flags.Bool("open", false, "启动后用默认浏览器打开看板")
	reset := flags.Bool("reset", false, "清理 Ago 自己创建的演示状态后重新开始（只作用于带 Ago 归属标记的目录）")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *executor != ModeFake && *executor != ModeRelay {
		return fmt.Errorf("--executor 只能是 %q 或 %q，收到 %q", ModeFake, ModeRelay, *executor)
	}
	if strings.TrimSpace(*state) == "" {
		return fmt.Errorf("--state 不能为空")
	}

	// Both questions that can refuse this directory are settled first, before
	// anything is created, probed, or written. A directory Ago is going to
	// refuse must not first have a temporary file written into it, and the
	// refusal must be reported in its own words rather than as whatever the
	// next step happened to trip over. Neither call touches anything.
	if *reset {
		if _, err := CheckResetAllowed(*state, home); err != nil {
			return err
		}
	}
	canonical, err := CanClaim(*state)
	if err != nil {
		return err
	}
	// Then the rest of preflight, and only then the delete. A --reset that ran
	// ahead of the credential check would destroy existing state and only then
	// discover it could not do the work.
	if err := preflight(out, canonical, *listen, *executor); err != nil {
		return err
	}
	if *reset {
		if err := ResetState(canonical, home); err != nil {
			return err
		}
		fmt.Fprintf(out, "已清理 Ago 创建的演示状态：%s\n", canonical)
	}

	if _, err := ClaimState(canonical); err != nil {
		return err
	}
	// What is already here before Ago starts. Anything appearing under one of
	// Ago's names afterwards is attributable to this run, and only that is
	// recorded as Ago's to delete later.
	existedBefore := PresentReservedEntries(canonical)

	repository := filepath.Join(canonical, "greeter")
	// A second run reuses the repository and the database, which is what makes
	// restart recovery observable rather than a claim: the board comes back
	// exactly where it stopped.
	if _, err := os.Stat(repository); errors.Is(err, os.ErrNotExist) {
		if err := agodemo.Create(context.Background(), repository); err != nil {
			return fmt.Errorf("创建示例仓库：%w", err)
		}
		fmt.Fprintf(out, "示例仓库已创建：%s\n", repository)
	} else if err != nil {
		return err
	} else {
		fmt.Fprintf(out, "沿用已有的示例仓库：%s\n", repository)
	}

	return Serve(Config{
		DatabasePath: filepath.Join(canonical, "ago.db"),
		Listen:       *listen,
		Mode:         *executor,
		Scenario:     "success",
		Out:          out,
		Setup: func(ctx context.Context, s *Stack) error {
			// The stack has now created whatever it needed. Recording it here
			// is what lets a later reset tell Ago's own directories apart from
			// anything that takes their names afterwards.
			if err := RecordCreatedEntries(canonical, existedBefore); err != nil {
				return err
			}
			return plantDemoGoal(ctx, out, s, repository, *goal)
		},
		Announce: func(address string) {
			url := "http://" + address
			fmt.Fprintf(out, "\n目标：%s\n打开看板：%s\n按 Ctrl+C 结束。\n", *goal, url)
			if *open {
				openBrowser(out, url)
			}
		},
	})
}

const demoBoardID = "ago-demo"

// plantDemoGoal states the goal through the same runtime a client would use.
//
// A board that already exists is left alone: re-running the demo must not
// discard work in progress, and the durable graph — not this function — is
// where the goal lives.
func plantDemoGoal(ctx context.Context, out io.Writer, s *Stack, repository, objective string) error {
	if _, err := s.store.Board(ctx, demoBoardID); err == nil {
		fmt.Fprintln(out, "看板已存在，从上次停下的地方继续。")
		return nil
	}
	ref := agointegrate.RefName(demoBoardID)
	base, err := s.integrator.EnsureRef(ctx, repository, ref, "")
	if err != nil {
		return fmt.Errorf("准备集成分支 %s：%w", ref, err)
	}
	_, err = s.runtime.Create(ctx, agoboardruntime.Goal{
		BoardID:    demoBoardID,
		Repository: agoplanner.Repository{ID: repository, Revision: base},
		Objective:  agoplanner.Objective{ID: "objective", Summary: objective},
		ProjectGates: []agoplanner.ProjectGate{{
			ID: "gate", Title: "目标验收",
			AcceptanceCriteria: []string{"所有任务通过独立验收"},
			VerifierIDs:        []string{"ago-verifier"},
		}},
		Constraints: agoplanner.Constraints{
			PathScopes:     agodemo.PathScopes(),
			CapabilityTags: []string{"repo-read", "repo-write", "tests", "report"},
			VerifierIDs:    []string{"ago-verifier"},
		},
		ExecutionMode: s.mode, BaseRevision: base, IntegrationRef: ref,
	})
	if err != nil {
		return fmt.Errorf("规划这个目标失败：%w", err)
	}
	return nil
}

// preflight checks what this machine can actually do, before anything is
// created, deleted, or promised.
//
// The point is the timing. Every one of these failures used to surface halfway
// through a run, as a failed task, in a language about leases and evidence —
// when the real problem was that git was missing or the port was taken.
func preflight(out io.Writer, state, listen, executor string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("找不到 git。Ago 用 git worktree 隔离每一次尝试，请先安装 git")
	}
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("找不到 go。示例仓库是一个 Go 项目，Ago 需要真的跑它的测试，请先安装 Go")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("监听地址 %q 无效：%w", listen, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("监听地址 %q 必须是回环地址（例如 127.0.0.1）", listen)
	}
	if port != "0" {
		listener, err := net.Listen("tcp", listen)
		if err != nil {
			return fmt.Errorf("端口已被占用：%s。换一个端口，或用 --listen 127.0.0.1:0 自动挑一个", listen)
		}
		_ = listener.Close()
	}
	// The relay check runs before the state directory is even created, so a
	// misconfigured endpoint leaves the machine exactly as it found it.
	if executor == ModeRelay {
		if err := checkRelayHealth(out); err != nil {
			return err
		}
	}
	return checkStateWritable(state)
}

// checkStateWritable creates nothing that outlives the check. An existing
// directory is probed in place; a missing one is created and removed again, so
// a preflight failure after this point has not left a state directory behind.
func checkStateWritable(state string) error {
	if info, err := os.Lstat(state); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("演示目录 %s 已存在但不是目录", state)
		}
		return probeWritable(state, state)
	}
	// The nearest ancestor that exists is probed. Creating the parent here
	// would leave a directory behind whenever a later step failed — with the
	// default --state that meant a stray ~/.ago after every failed run.
	ancestor := filepath.Dir(state)
	for {
		if info, err := os.Lstat(ancestor); err == nil && info.IsDir() {
			return probeWritable(ancestor, state)
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return fmt.Errorf("演示目录 %s 的上级目录都不存在", state)
		}
		ancestor = parent
	}
}

func probeWritable(directory, state string) error {
	probe, err := os.CreateTemp(directory, ".ago-writable-*")
	if err != nil {
		return fmt.Errorf("演示目录 %s 不可写：%w", state, err)
	}
	name := probe.Name()
	_ = probe.Close()
	return os.Remove(name)
}

// checkRelayHealth makes one real, small call. A configured credential is not
// the same as a working one, and finding that out here costs one request
// instead of a whole run.
func checkRelayHealth(out io.Writer) error {
	baseURL := strings.TrimSpace(os.Getenv(EnvBaseURL))
	if baseURL == "" {
		return fmt.Errorf("relay 模式需要 %s（例如 http://127.0.0.1:8317/v1）", EnvBaseURL)
	}
	if strings.TrimSpace(os.Getenv(EnvAPIKey)) == "" {
		return fmt.Errorf("relay 模式需要在环境变量里提供 %s；它不接受命令行参数，因为那会留在 shell 历史和进程列表里", EnvAPIKey)
	}
	model := envOr(EnvPlannerModel, "claude-sonnet-5")
	client, err := agorelay.New(agorelay.Profile{
		ID: "relay-preflight", BaseURL: baseURL, Model: model, APIKeyEnv: EnvAPIKey,
		Timeout: 60 * time.Second, MaxOutputBytes: 1 << 16,
	}, nil, os.Getenv)
	if err != nil {
		return fmt.Errorf("relay 配置无效：%w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := client.Complete(ctx, agorelay.Request{
		System: "Reply with the single word: ok",
		User:   "ok",
	}); err != nil {
		// The wrapped error is already redacted by the relay client. The base
		// URL is NOT: it can carry a credential in its userinfo or query
		// string, and printing it raw put the same secret in the clear one
		// line above the redacted copy. Only the endpoint's identity is shown.
		return fmt.Errorf("无法通过 %s 调用模型 %s：%w\n请检查 %s、%s 和模型名称",
			safeEndpoint(baseURL), model, err, EnvBaseURL, EnvAPIKey)
	}
	fmt.Fprintf(out, "模型可用：%s\n", model)
	return nil
}

// safeEndpoint reduces a base URL to what a user needs in order to recognise
// it — scheme and host — and drops the parts that can carry a secret.
func safeEndpoint(baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" {
		return "（已隐藏的端点）"
	}
	return parsed.Scheme + "://" + parsed.Host
}

// openBrowser is best effort. A demo that failed because a browser would not
// open would be a worse demo than one that printed a URL.
func openBrowser(out io.Writer, url string) {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	if err := command.Start(); err != nil {
		fmt.Fprintf(out, "（没能自动打开浏览器：%v）\n", err)
		return
	}
	go func() { _ = command.Wait() }()
}
