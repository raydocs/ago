# claudex-flow

`claudex-flow` v1.4.4 是为 Claude Code 实现的 **Amp-style Agent Mode**。它不是“为整项任务挑一个模型”，而是一个有主线程、持久 Worker、能力代理、零模型 prompt route hint、**Supervisor 工具预算与 Root 生命周期闸门**、历史 Thread 搜索/读取、证据回注、路由结果账本、独立 Luna compact lane 与云端 Thread 记录的运行时。

v0.5 的优化目标不是增加 Agent 数量，而是降低**验收通过所需的总成本和关键路径时间**：先冻结验收契约，Sol 能单路径完成时直接做；只有一个独立切片能避免 Supervisor 重复实现时才启动 Worker。

## 核心拓扑

```text
GPT-5.6 Sol xhigh Supervisor（Claude Code 主线程）
  ├─ Grok 4.5 high Worker 1（独立、可恢复 session）
  ├─ Grok 4.5 high Worker 2
  ├─ Grok 4.5 high Worker 3
  └─ Capability Broker
       ├─ Grok 4.5 high：外部/当前/X/供应商信息检索
       ├─ Gemini 3.5 Flash medium：明确 URL 的快速抓取整理
       ├─ GPT-5.6 Terra high：仓库探索、代码定位、依赖映射
       ├─ Local Find Thread：零模型按关键词/文件/项目/日期寻找历史 Thread
       └─ GLM 5.2 Read Thread：读取选中 Thread 的相关上下文

Claude Code native compact request
  └─ localhost:8318 gateway：仅把 gpt-5.6-sol compact 改路由到 GPT-5.6 Luna
```

Worker 不能自行 fan-out。它返回结构化 `needs_capability`；Supervisor 调用对应 capability，再使用同一个 `worker_id` 和同一个 Claude `session_id` 恢复 Worker。上下文不会重新广播。

持久 Worker 和外部检索虽然都使用 Grok 4.5 high，但职责、session 与工具权限分离：Worker 仅使用本地代码工具；`search_external` 是独立只读检索调用，仅使用 WebSearch/WebFetch。

## 使用

```bash
source ~/.zshrc
claudex
```

四个入口：

```bash
claude          # 原生 Claude Code
claudex         # Sol xhigh Supervisor + Amp-style Workflow
claudex-models  # 手动打开 CLIProxyAPI 模型菜单
claudex-threads # 打开公开只读 Cloudflare Thread Archive（写入仍需 INGEST_TOKEN）
```

`claudex` 实际启动：

```bash
claude \
  --settings "$HOME/.config/claudex/settings.json" \
  --mcp-config "$HOME/.config/claudex/mcp.json" \
  --append-system-prompt-file "$HOME/.config/claudex/orchestrator.md" \
  --model gpt-5.6-sol \
  --effort xhigh
```

## Runtime tools

| MCP tool | Runtime |
|---|---|
| `route_task` | 零 Token 比较 Sol direct / specialist / Worker；不启动模型 |
| `record_route_outcome` | 零 Token 关闭 route，记录验收/人工修正/残余风险与 child 调用诊断 |
| `start_worker` | 新建持久 Grok 4.5 high Worker |
| `resume_worker` | 把证据/失败日志回注同一 Worker session |
| `search_external` | Grok WebSearch/WebFetch |
| `digest_urls` | Gemini Flash WebFetch only |
| `explore_repository` | Terra Read/Grep/Glob |
| `find_thread` | 本机零模型按关键词、文件、项目与日期寻找历史 Thread |
| `read_thread` | GLM 5.2 从本机历史 Thread 提取一个有来源的答案 |
| `consult_native_claude` | 原生 Claude subscription 的只读判断/复核 |
| `workflow_status` | 零 Token 状态与预算检查 |
| `runtime_contract` | 零 Token 返回版本、profiles、tools 与准确 Worker schema |
| `close_worker` | 关闭 Worker 并释放写范围 |

## GPT-5.6 Luna compact lane

Claude Code 2.1.208 本身没有公开的独立普通 compact-model setting：客户端仍以当前 `mainLoopModel` 构造 compact 请求。Claude X 在现有 `127.0.0.1:8318` gateway adapter 上增加了一条窄路由：只有 `POST /v1/messages` 的**最后一条 user message**以 Claude Code 两个原生 compact prompt 之一开头，且原模型是 `gpt-5.6-sol` 时，才把顶层 `model` 改成 `gpt-5.6-luna`。

- 普通 Sol 请求、Worker、specialist 和其他模型不变。
- 仅改 model；原请求的 thinking/effort、messages、tools 和 token limits 全部保留。
- 不会静默 fallback 到 Grok；Grok 只作为以后受控质量/延迟对比的候选。
- 日志只记录时间、from/to model 和请求字节数，不记录 prompt、正文或凭据。
- 返回头提供 `x-claudex-route-role: compaction` 与 routed model，便于真实 canary 和 usage 归因。

安装或恢复这一窄路由：

```bash
cd ~/orca/projects/x
./scripts/install-claudex-gateway.sh
```

脚本会先跑 Node 单元/代理集成测试，备份原 adapter，原子替换后仅重启本地 gateway adapter；不会重启 Claude X 主进程或 `claudex-flow` MCP。

## v0.5 Worker admission contract

`start_worker` 的唯一字段集合由运行时 `claudex-flow contract` 输出：

```text
context, deadline_ms, done_condition, marginal_contribution, objective,
output_contract, paths, retry_reason, slice_id, workdir, write
```

- `slice_id`：同一 Root Thread 内稳定且唯一的切片标识。
- `marginal_contribution`：Worker 独立增加什么、避免 Supervisor 重复做什么。
- `output_contract`：必须返回的证据、路径、验证和残余风险。
- `done_condition`：可观察的通过条件，优先使用准确命令。
- `deadline_ms`：30–600 秒；默认 600 秒。
- 写 Worker 必须声明独占 `paths`。

Admission 在任何 model call、turn budget、concurrency slot 或 write lease 之前执行。拒绝记录可在 `workflow_status.slices` 查看，但不消耗模型。

Worker 正常只启动一次。只有第一次启动被运行时分类为 `retry_eligible=true` 时，才允许修复基础设施后，用**相同** `slice_id`、目标、上下文、契约、deadline 和 scope 再启动一次，并填写 `retry_reason`。不跨模型 fallback；有 `session_id` 的 child 只能通过 `resume_worker` 延续。

每个 Worker/Specialist 输出分别记录 `requested_model`、`resolved_model`、`model_verification`、auth source、token usage 和 duration。Effort 当前只标记为 `cli_argument_only`，因为 Claude Code child stream 尚未提供独立的 resolved-effort 证据。

## v1.3 Accepted-result router

当且仅当 substantial task 的 direct / capability / Worker 选择确实不清楚时，Supervisor 可调用零模型 `route_task`。Router 会显式比较：

1. 当前 Sol xhigh 单 Agent；
2. 最便宜且可能 non-inferior、并且在当前 surface 真能切换的单 Agent lane；当前没有经验证的 in-session lead switch，所以仍诚实显示 Sol baseline；
3. 选中的 routed route：一个 specialist capability 或一个带协调、验证、失败与 rescue 成本的 Grok Worker。

Router 检查 ambiguity、coupling、state depth、semantic risk、checkability 和 context duplication。Worker 只有在明确声明独立切片、没有共享可变状态、且存在可负担的 objective/partial verifier 时才可选。

Worker 路由还必须提供 `worker_marginal_contribution`，明确它具体替 Supervisor 避免了什么重复实现或关键路径等待；空值会让路由保持 Sol direct。Worker 返回包上限 32 KiB，specialist evidence 上限 24 KiB，且每次只允许一个 `needs_capability`，避免把主上下文重新撑大。

`record_route_outcome` 会把每次 terminal route 追加到 `~/.config/claudex/route-outcomes.jsonl`（0600）。记录只累计 `claudex-flow` 观察到的 child model call：requested/resolved model、分离的 input/cache/output tokens、duration、tool uses、same-lane retries。Claude Code 主 Supervisor 不在 MCP 进程内，所以明确标记 `supervisor_included=false`，不伪造精确总价。

`UserPromptSubmit` 还运行一个同步、零模型、通常静默的 `route-hint`。它只在当前 prompt 有明确能力缺口时注入一条短 system reminder：当前/外部资料 → Grok，已知 URL → Gemini Flash，广泛仓库定位 → Terra，本机历史 Thread → Find/Read Thread。普通实现、`continue` 和复杂但尚未形成独立 slice 的任务不注入任何内容。它不启动模型、不强制 Worker，MCP admission 与 live lane health 仍是最终 gate。外部 Amp Thread URL 明确按 URL digestion 处理，不会再误送给只读取本机 Claude X transcript 的 `read_thread`。

Native Claude 调用不会再加载 Claude X gateway settings overlay；否则其中的 `ANTHROPIC_BASE_URL`/token 会覆盖本机 claude.ai subscription 并禁用 connectors。Opus subscription tool canary 已通过。Fable 当前配额耗尽，保持非自动路由且不再探测。

`find_thread` + `read_thread` 对齐 Amp 的历史 Thread 复用链：前者在本机 root transcripts 上按关键词、文件、项目/仓库和日期做流式零模型搜索，支持 quoted phrase、`file:`、`project:`/`repo:`、`after:`、`before:`，只返回经过 canonical parser 脱敏的候选与 `thread://` 来源；后者再按选中的 session ID 用 GLM 5.2 提取答案。`read_thread` 用最新 compact summary 做方向参考，同时按问题选取匹配的原始事件和最近状态。默认 source packet 96 KiB、最大 160 KiB，返回 evidence 仍受 24 KiB 上限约束；不会把 11MB 级完整 transcript 塞回 Supervisor。

本机同时包含 Claude subscription 和 gateway 路径，没有一个统一可比的 spend signal，因此只报告 `relative_resource_intensity`，不再声称 `1x/2x` 精确成本。一轮 heuristic 不会升级成 durable default；长期默认只能来自固定 acceptance contract 的受控代表性 canary。

失败时只改变一个维度：先修 context；仅对 runtime 标记的 transient failure 同 lane 重试一次；清晰任务检查不足才升 effort；只有 reasoning/ambiguity/state mismatch 才换 lane。硬判断解决后回到更便宜的合格 lane。

## Cloud Threads

`claudex` 专用 settings 已接入 Claude Code lifecycle hooks。主线程和 child model sessions 会把经过本地脱敏的事件异步写入 Cloudflare D1；原生 `claude` 不受影响。

```bash
claudex-threads                # 打开公开只读云端 Thread App
claudex-flow thread-status     # 查看同步状态和本地失败队列
claudex-flow thread-sync       # 手动重试离线事件
```

Dashboard：<https://claudex-threads.ppop.workers.dev>

云端 App 当前是只读 archive：支持主/子 Thread、模型与 effort、tool/capability timeline、搜索、JSON 导出和归档。它不从网页远程执行本机命令。

## 停滞恢复

普通 MCP 调用具有 120 秒 hard wall-clock timeout，防止 Playwright/Context7 等工具无限挂起；`claudex-flow` 单独覆盖为 11 分钟，保留真实 Worker turn 的执行空间。

`PostToolUse` 与 `PostToolUseFailure` 还接入一个零模型 watchdog。若工具已经返回、但主 Thread transcript 连续 5 分钟没有任何增长，Claude Code 的 `asyncRewake` 会向同一 turn 注入一次 `CLAUDEX_STALL_REWAKE`：Supervisor 先核对副作用状态，只继续未完成的 gate，不重放部署或写入。正常推理一旦产生 transcript 增量，watchdog 会立即退出。

每个 tool result 最多触发一次恢复；记录保存在 `~/.config/claudex/stall-watch/events.jsonl`。这个机制不会回溯已经发生在安装前的停滞，也不会自动重启或杀掉 Claude Code。

## Supervisor gate (v1.4.2)

`supervisor-gate` 是零模型硬约束，装在 `PreToolUse` / `PostToolUse` / `PostToolUseFailure` / `PostCompact` / `UserPromptSubmit`：

| 闸门 | Root 预算 | Gate 预算 | 行为 |
|---|---|---|---|
| Playwright | 12 | 4 | 任一超额 deny |
| Screenshots | 8 | 3 | 同上 |
| 相同验证指纹 | 3 | 3 | 同上 |
| 高成本施工 | soft 8 / hard 24 | soft 4 / hard 8 | soft 后 **sticky deny** 直到 `ack_reroute` |
| Root 生命周期 | 3×compact / 4h / ~8MiB | — | handoff：拒 Write/Edit/Agent/Task/Skill/mutating Bash |

v1.4.2（T1，patch 升版）：

- MCP：`declare_gate` / `close_gate` / `ack_reroute` / `gate_status`（控制工具在 sticky 下仍 allow）；
- **Root 总预算永不随 gate 重置**；每 gate 子预算仅在 `declare_gate` 时清零；
- 防刷：最多 8 gates/Root、gate_id 唯一、acceptance hash 不得与上一 gate 相同；
- 用户 `/gate-override reason=... paths=a,b`（10m/3 次；模型不可自授）；
- v1.4.1 flock / handoff Bash 白名单 / path-first admission 保留。

Child Worker session（`CLAUDE_CODE_CHILD_SESSION=1`）跳过该闸门。状态：`~/.config/claudex/supervisor-gate/`。

## Worker 状态机

```text
start_worker
   ├─ admission rejected ──> 0 model call / repair packet
   ├─ retryable_failed ────> repair infra / one identical retry
   ├─ completed ───────────> Supervisor verifier ──> PASS/stop
   ├─ blocked ────> Supervisor decision
   └─ needs_capability
          ├─ external_search -> Grok
          ├─ url_digest      -> Gemini Flash
          ├─ repo_explore    -> Terra
          ├─ find_thread     -> local zero-model scan
          └─ read_thread     -> GLM 5.2
                    ↓
             resume_worker(same ID/session)
                    ↓
             completed / next material need
```

## 成本与并发

- 简单任务：Supervisor 直接完成，0 Worker。
- 一般可分解任务：通常 1 Worker。
- 最多 3 个 child model runs 并发。
- 每个 Claude X session 最多 6 个 Worker threads、12 个 Worker turns、6 次 research calls。
- 每个 Worker 最多恢复 3 次。
- 写 Worker 必须声明路径；重叠路径 lease 会被拒绝。
- child 使用 strict empty MCP，无法递归创建 agent。
- Worker 启动失败不会自动切换 Opus、Codex Rescue 或其他模型。

## 验证

```bash
go test ./...
go vet ./...
go test -race ./...
node --test adapter/model-filter-proxy.test.mjs
claudex-flow doctor
claudex-flow contract
```

本机安装必须从唯一 canonical source 执行，脚本会拒绝 Documents 旧副本：

```bash
cd ~/orca/projects/x
./scripts/install-claudex-flow.sh 1.3.2
```

`doctor` 同时检查 binary build source、orchestrator contract/schema marker、settings timeout、MCP command/timeout 和 gateway。`SessionStart` 的同步 `contract-guard` 会在启动阶段阻止 prompt/runtime schema 漂移；不会调用模型。

真实闭环已经通过：Grok 4.5 Worker 请求外部事实 → Grok 检索 → 证据回注原 session → Worker 写文件并验证 → Supervisor 再验证。
