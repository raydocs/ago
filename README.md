<p align="center">
  <img src="./assets/readme/hero.svg" width="100%" alt="claudex-flow routes one Claude Code supervisor through bounded workers, specialist capabilities, and evidence gates">
</p>

**claudex-flow** 是 Claude Code 的本地编排运行时：保留一个 Sol Supervisor 主线程，只在独立切片能缩短验收路径时启动持久 Worker，并把外部检索、URL 摘要、仓库探索和历史 Thread 读取隔离成专用能力。

> 当前仓库内可验证版本为 **v1.4.6**。它不是“自动多开 Agent”，也不会替代 Claude Code；它用 admission、预算、写路径租约和验收记录约束何时值得分流。

## 一眼看懂运行时

```text
Claude Code / Sol Supervisor
  ├─ route_task ─────────────── 只比较路线，不调用模型
  ├─ start_worker ───────────── 持久 Grok Worker；禁止自行 fan-out
  ├─ specialist capabilities ── 搜索 / URL / 仓库 / Thread
  ├─ supervisor-gate ────────── 工具预算、生命周期与 sticky reroute
  └─ record_route_outcome ───── 验收、修正、token 与残余风险账本
```

真实契约可直接从二进制读取，而不是依赖 README：

```bash
claudex-flow version
claudex-flow contract
claudex-flow doctor
```

## 核心边界

| 机制 | 做什么 | 不做什么 |
|---|---|---|
| `route_task` | 零模型比较 direct / specialist / Worker | 不启动模型 |
| `start_worker` / `resume_worker` | 在同一 session 延续一个有边界的实现切片 | Worker 不递归创建 Agent |
| Capability Broker | 分离外部搜索、URL 抓取、仓库探索、历史 Thread | 不把检索权限混入写 Worker |
| Supervisor Gate | 限制高成本工具、重复验证和 Root 生命周期 | 不重置 Root 总预算 |
| Thread Archive | 本地脱敏后异步写入只读 Cloudflare archive | 网页端不远程执行本机命令 |
| Luna compact lane | 仅重写符合原生 compact prompt 的 Sol 请求模型 | 普通请求与其他模型不受影响 |

## 安装

安装脚本包含一个**硬边界**：只接受 canonical source `~/orca/projects/x`。从其他目录运行会以 `refusing non-canonical build` 退出；不要复制目录绕过。

```bash
cd ~/orca/projects/x
./scripts/install-claudex-flow.sh 1.4.6
```

脚本会依次运行 Go 测试、`go vet`、adapter 测试，构建不可变版本产物，安装 `~/.local/bin/claudex-flow` 与 `~/.local/bin/claudex`，配置 hooks，并输出 runtime contract 与 SHA-256。

安装后重新载入 shell：

```bash
source ~/.zshrc
claudex
```

常用入口：

```bash
claude          # 原生 Claude Code
claudex         # Sol Supervisor + claudex-flow workflow
claudex-models  # CLIProxyAPI 模型菜单
claudex-threads # 打开只读 Cloud Thread Archive
```

## Worker admission

`claudex-flow contract` 是字段集合的唯一运行时真相。v1.4.6 的 `start_worker` 接受：

```text
context, deadline_ms, done_condition, marginal_contribution, objective,
output_contract, paths, retry_reason, slice_id, workdir, write
```

- 写 Worker 必须声明独占 `paths`；重叠 lease 会被拒绝。
- `deadline_ms` 为 30–600 秒，默认 600 秒。
- Worker 正常只启动一次；只有运行时标记 `retry_eligible=true` 才允许同 lane、同 slice 重试一次。
- 有 `session_id` 的 child 只能用 `resume_worker` 延续，不能重新广播上下文。
- 结果包、specialist evidence 和 capability request 都有大小/数量上限。

## Thread 与 compact

```bash
claudex-flow thread-status
claudex-flow thread-sync
claudex-flow thread-find --query '"route outcome" project:x after:7d'
```

`find_thread` 做本机零模型候选搜索；`read_thread` 再从选中的 transcript 提取带来源答案。同步链只记录脱敏事件，失败事件先落本地队列。

compact adapter 位于 `adapter/model-filter-proxy.mjs`：只有 Claude Code 原生 compact 请求、原模型为 `gpt-5.6-sol` 时才改写到 Luna；messages、tools、thinking/effort 和 token limits 原样保留。

## 开发与验证

```bash
go test ./...
go vet ./...
go test -race ./...
node --test adapter/model-filter-proxy.test.mjs
```

重点目录：

```text
cmd/claudex-flow/       CLI 入口
internal/mcpserver/     MCP contract、admission、worker 与 capability
internal/supervisorgate Root 工具预算与生命周期闸门
internal/thread*/       Thread 发现、读取、记录与同步
internal/route*/        路由、hint、评估与 outcome
thread-app/             Cloudflare 只读 Thread Archive
outputs/                runtime contract、canary 与验收报告
```

## 已知事实与限制

- Supervisor 本身不在 MCP 进程内，因此 outcome 只统计 `claudex-flow` 看到的 child model call；`supervisor_included=false` 是刻意的诚实标记。
- child stream 目前不能证明 resolved effort；运行时只标记 `cli_argument_only`。
- 本地同时存在 subscription 与 gateway 路径，没有统一可比 spend signal，只报告相对资源强度。
- `stall-watch` 在 v1.4.6+ 为非阻塞 no-op，避免 PostToolUse 等待 transcript 而自锁。
- Cloud Thread App 是只读 archive；写入需要 `INGEST_TOKEN`，网页不具备本机执行能力。

更多变更与 canary 证据见 [`outputs/`](./outputs/)。
