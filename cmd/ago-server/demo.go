package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
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

// runDemo is the one command that has to work on a machine that has never run
// Ago before.
//
// It does the whole thing: makes a real sample repository, checks that this
// machine can actually do the work before promising anything, states a Chinese
// goal, and serves the board. Every failure it can foresee is reported as a
// sentence about what to change, not as a stack trace at minute four.
func runDemo(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	defaultState := filepath.Join(home, ".ago", "demo")

	flags := flag.NewFlagSet("ago-server demo", flag.ContinueOnError)
	state := flags.String("state", defaultState, "目录：示例仓库、数据库、工件都放在这里")
	listen := flags.String("listen", "127.0.0.1:0", "回环监听地址；端口为 0 表示自动挑一个空闲端口")
	executor := flags.String("executor", modeFake, `谁来干活："fake"（离线，不需要任何凭据）或 "relay"（真实模型）`)
	goal := flags.String("goal", agodemo.Objective, "要交给 Ago 的目标")
	open := flags.Bool("open", false, "启动后用默认浏览器打开看板")
	reset := flags.Bool("reset", false, "删除已有的演示状态重新开始")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *executor != modeFake && *executor != modeRelay {
		return fmt.Errorf("--executor 只能是 %q 或 %q，收到 %q", modeFake, modeRelay, *executor)
	}
	// Reset first: preflight then checks the directory the run will actually
	// use rather than the one that is about to be deleted.
	if *reset {
		if err := os.RemoveAll(*state); err != nil {
			return fmt.Errorf("清理演示状态 %s：%w", *state, err)
		}
	}
	if err := preflight(*state, *listen, *executor); err != nil {
		return err
	}

	repository := filepath.Join(*state, "greeter")
	// A second run reuses the repository and the database, which is what makes
	// restart recovery observable rather than a claim: the board comes back
	// exactly where it stopped.
	if _, err := os.Stat(repository); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(*state, 0o700); err != nil {
			return fmt.Errorf("准备演示目录 %s：%w", *state, err)
		}
		if err := agodemo.Create(context.Background(), repository); err != nil {
			return fmt.Errorf("创建示例仓库：%w", err)
		}
		fmt.Printf("示例仓库已创建：%s\n", repository)
	} else if err != nil {
		return err
	} else {
		fmt.Printf("沿用已有的示例仓库：%s\n", repository)
	}

	return serve(serveConfig{
		DatabasePath: filepath.Join(*state, "ago.db"),
		Listen:       *listen,
		Mode:         *executor,
		Scenario:     "success",
		Setup: func(ctx context.Context, s *stack) error {
			return plantDemoGoal(ctx, s, repository, *goal)
		},
		Announce: func(address string) {
			url := "http://" + address
			fmt.Printf("\n目标：%s\n打开看板：%s\n按 Ctrl+C 结束。\n", *goal, url)
			if *open {
				openBrowser(url)
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
func plantDemoGoal(ctx context.Context, s *stack, repository, objective string) error {
	if _, err := s.store.Board(ctx, demoBoardID); err == nil {
		fmt.Println("看板已存在，从上次停下的地方继续。")
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
// created and before a user is told to open a page.
//
// The point is the timing. Every one of these failures used to surface halfway
// through a run, as a failed task, in a language about leases and evidence —
// when the real problem was that git was missing or the port was taken.
func preflight(state, listen, executor string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("找不到 git。Ago 用 git worktree 隔离每一次尝试，请先安装 git")
	}
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("找不到 go。示例仓库是一个 Go 项目，Ago 需要真的跑它的测试，请先安装 Go")
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return fmt.Errorf("演示目录 %s 不可写：%w", state, err)
	}
	probe := filepath.Join(state, ".writable")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return fmt.Errorf("演示目录 %s 不可写：%w", state, err)
	}
	_ = os.Remove(probe)

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
	if executor == modeRelay {
		return checkRelayHealth()
	}
	return nil
}

// checkRelayHealth makes one real, small call. A configured credential is not
// the same as a working one, and finding that out here costs one request
// instead of a whole run.
func checkRelayHealth() error {
	baseURL := strings.TrimSpace(os.Getenv(envBaseURL))
	if baseURL == "" {
		return fmt.Errorf("relay 模式需要 %s（例如 http://127.0.0.1:8317/v1）", envBaseURL)
	}
	if strings.TrimSpace(os.Getenv(envAPIKey)) == "" {
		return fmt.Errorf("relay 模式需要在环境变量里提供 %s；它不接受命令行参数，因为那会留在 shell 历史和进程列表里", envAPIKey)
	}
	model := envOr(envPlannerModel, "claude-sonnet-5")
	client, err := agorelay.New(agorelay.Profile{
		ID: "relay-preflight", BaseURL: baseURL, Model: model, APIKeyEnv: envAPIKey,
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
		// The relay client redacts the credential before it can reach an
		// error, so this is safe to print.
		return fmt.Errorf("无法通过 %s 调用模型 %s：%w\n请检查 %s、%s 和模型名称",
			baseURL, model, err, envBaseURL, envAPIKey)
	}
	fmt.Printf("模型可用：%s\n", model)
	return nil
}

// openBrowser is best effort. A demo that failed because a browser would not
// open would be a worse demo than one that printed a URL.
func openBrowser(url string) {
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
		fmt.Printf("（没能自动打开浏览器：%v）\n", err)
		return
	}
	go func() { _ = command.Wait() }()
}
