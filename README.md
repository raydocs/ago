# Ago

**An intelligent project board that plans, delegates, monitors, and finishes work with agents.**

Ago turns one user objective into a live execution board. The board is not a passive task list: it is itself an orchestration agent. It decomposes the objective, assigns bounded work, watches progress and dependencies, reacts to failures, verifies each result independently, and promotes accepted work onto a git ref it owns.

> 输入一个目标，Ago 自动生成看板、拆解任务、分配 Agent，并持续监控和验收，直到项目完成。

## Try it

```bash
go build -o ago ./cmd/ago
./ago demo --executor fake     # offline, no credential
./ago demo --executor relay    # a real model does the work
```

Then open the printed URL. Full walkthrough: [快速开始](docs/ago-quickstart.zh.md).

## Status

Ago is pre-release and has no users. This section is the honest inventory; the
rest of this document describes where it is going.

**Built and exercised end to end.** A durable SQLite work graph with fencing
tokens, leases, exactly-once claims, and generations. Per-attempt isolated git
worktrees. An executor that cannot mark its own work done. An independent
verifier whose deterministic checks outrank its model judgement. Integration of
accepted work onto `refs/heads/ago/*`, with later tasks inheriting earlier
verified revisions. An autonomous supervisor that repairs, retries with
backoff, and escalates only on a defined list of conditions. Crash recovery. A
live board UI. Credential redaction at the durability boundary.

One real run: a Chinese goal became a 6-task graph, four sequential write tasks
each built on the previous verified revision, every result independently
verified, zero human decisions. The resulting branch does pass the sample
project's tests — **but Ago did not check that.** The end-to-end test did,
afterwards. See the gap below.

**The gap that matters most.** `ProjectGates` are planned, validated, and
stored, and nothing executes them. Ago reports a goal complete when every task
has passed, which is not the same as the integrated result being sound. Two
individually correct changes can combine into something broken and Ago will
still say "complete". Until the project gate runs against the integrated
revision, treat completion as "every part was checked", not "the whole was".

**Not built.** Capability routing (one executor serves every task; capability
tags are recorded and never used for dispatch). Context engineering. Parallel
execution — the scheduler dispatches synchronously, so one model task runs at a
time. Remote execution. Multi-repository. Teams. Cost accounting. The HTTP API
accepts the offline executor only; the real one is reachable from the CLI.

**Scale is unproven.** Everything above was demonstrated on one six-file sample
repository with one goal. Nothing here has met a large codebase, a long task
graph, or two changes that genuinely conflict.


## The simplest mental model

**Ago combines automatic agent routing, repository-aware context engineering,
an intelligent project board, and durable issue-style work items.**

Conceptually, it brings together four proven interaction patterns. **Two of the
four are built today; the other two are intent, not description** — see
[Status](#status) before relying on any of this:

- **Amp-like automatic capability routing** *(not built)* — use the best
  available model or agent for each kind of work; customers do not select
  models themselves. Today every task goes to one executor;
- **RepoPrompt-like context engineering** *(not built)* — understand the
  repository and assemble a focused context package for each task instead of
  sending the whole codebase or conversation. Today a task gets a bounded
  listing of its worktree;
- **Linear-like board orchestration** *(built)* — visualize dependencies and
  progress while the board automatically schedules, monitors, retries, and
  re-plans;
- **GitHub Issues-like durable work items** *(built)* — every task has an
  identity, contract, owner, discussion, status, artifacts, history, and
  acceptance evidence that tools and automations can reference.

These are product inspirations, not integrations or affiliations. In Ago they
form one intended execution loop: the issue-style task defines the work, the
context engine prepares its inputs, capability routing selects its agent, and
the board supervises it through verified completion. The durable half of that
loop exists; the routing and context halves do not yet.

## The product

Most agent products put one long conversation at the center. As work grows, that conversation accumulates plans, code, logs, tool output, corrections, and unfinished branches until its context becomes the bottleneck.

Ago puts a durable board at the center instead.

```text
┌──────────────────────┐
│ User objective       │
│ "Build / fix / ship" │
└──────────┬───────────┘
           ▼
┌──────────────────────────────────────────────┐
│ Ago Board Agent                              │
│                                              │
│  understand goal ─▶ create work graph        │
│  assign agents   ─▶ monitor dependencies     │
│  inspect evidence ─▶ repair / re-plan        │
│  integrate results ─▶ verify completion      │
└───────┬────────────────┬────────────────┬─────┘
        ▼                ▼                ▼
┌─────────────┐   ┌─────────────┐   ┌─────────────┐
│ Agent task  │   │ Agent task  │   │ Agent task  │
│ isolated    │   │ isolated    │   │ isolated    │
│ context     │   │ context     │   │ context     │
└──────┬──────┘   └──────┬──────┘   └──────┬──────┘
       └─────────────────┴─────────────────┘
                         ▼
              ┌────────────────────┐
              │ Integrated result  │
              │ + verification     │
              └────────────────────┘
```

The key architectural decision is to move orchestration memory out of any one
model's context window. Repository understanding, the work graph, decisions,
progress, artifacts, and verification live in durable system state. Models can
then be selected for what they do best without asking one of them to remember
and repeatedly reread the entire project.

## Why a board

### 1. The system can see and operate the whole project

The board makes work explicit: tasks, owners, dependencies, status, blockers, artifacts, verification, and acceptance criteria. The orchestration agent can monitor the project in real time and automatically dispatch the next runnable task instead of waiting for a human to coordinate every step.

### 2. Context stays bounded

Each worker agent receives only the task contract and evidence it needs. Large logs and histories remain attached to durable tasks rather than filling one root prompt. The board retains project memory while agents use short, focused contexts, so long projects can continue without a single conversation becoming saturated.

### 3. Progress is durable and recoverable

Agent execution is not owned by a UI session. Threads, task state, events, artifacts, diffs, checks, and decisions are persisted. A renderer can close or reconnect without stopping active work or losing the project plan.

### 4. Completion is evidence-based

Workers do not declare their own work finished merely because they produced text: an independent verifier decides, and deterministic checks outrank its judgement. Every board item carries observable acceptance criteria and verification evidence.

**Today that guarantee stops at the task.** Ago promotes each accepted change onto its own ref, and closes the goal when every task has passed — it does **not** yet run the project-level gate against the integrated result. Until it does, "the whole thing holds together" is not something Ago has checked. Closing that gap is the current priority.

## One project, many specialized agents

Different agents are good at different kinds of work. A UI specialist may be
the best choice for interface design, an orchestration specialist for planning
and dependency analysis, and another agent for focused implementation. Ago
treats those as capabilities rather than forcing the whole project through one
model.

The user does not need to select those models manually. The board evaluates the
task contract, required tools, available context, historical quality, cost, and
current lane health, then assigns an eligible agent. Model names can change;
the durable task and its acceptance criteria remain stable.

## Hierarchical work, bounded context

Ago does not flatten a large project into one enormous checklist. It can refine
work recursively while keeping every level linked:

```text
Project board
├── Workstream / subproject
│   ├── Task
│   │   ├── Agent thread / attempt
│   │   └── Evidence and verification
│   └── Task
└── Workstream / subproject
    └── Task
```

Each task has a clear input, output, dependency set, owner, and completion
contract. A child agent receives a repository context package assembled for
that task—relevant files, symbols, decisions, and upstream artifacts—rather
than the full history of the root project. If a task is still too broad, the
board can split it again before execution.

## Core workflow

1. **Input** — The user submits a repository and describes an outcome, not an agent topology.
2. **Repository understanding** — Ago maps relevant files, symbols, architecture, constraints, and existing project state to build reusable context packages.
3. **Board generation** — Ago recursively creates workstreams and tasks with dependencies, acceptance criteria, risk boundaries, and verification steps.
4. **Automatic assignment** — The board assigns runnable tasks to suitable agents and execution environments according to capability.
5. **Continuous supervision** — Ago consumes durable progress events, detects blockers, retries safe work, and re-plans when evidence changes.
6. **Integration** — Results are reconciled into the shared project with ownership and stale-state checks.
7. **Verification** — Checks, diffs, costs, and artifacts are reviewed against the original objective.
8. **Completion** — The board closes when every required task has passed. Proving the project-level exit gate against the integrated result is **not yet implemented**; see [Status](#status).

Users should not need to choose a model or manually spawn agents. Ago owns routing and orchestration policy; the interface exposes the work and its evidence.

## What the user sees

Ago presents the same project at two levels:

- **Board and progress view** — workstreams, active agents, dependencies,
  blockers, completion estimates, checks, and what is expected to happen next;
- **Engineering detail view** — agent threads, source context, tool events,
  changed files, diffs, logs, artifacts, and verification evidence.

A user can supervise the outcome from the board without reading implementation
details, or drill all the way down to the code whenever intervention is useful.

## What exists today

The repository already contains the local-first substrate for this product direction:

- durable SQLite threads, events, mailbox, queue, steer, interrupt, and restart recovery;
- independent local execution that continues when clients disconnect;
- plugin lifecycle, tools, commands, typed UI requests, and AI classification;
- bounded child execution and evidence-oriented supervisor gates;
- Ago-owned write receipts, Git diff review, section change requests, staging, and safe revert;
- durable verification results plus provider usage and cost projection;
- attachments and repository file mentions;
- CLI, installable Web/PWA, macOS SwiftUI, and iOS SwiftUI clients;
- cross-client canonical projection conformance and 5,000-event performance gates;
- authenticated relay transport and optional recent-passkey remote mutations;
- closed macOS app-bundle and signed-update tooling.

The next product layer is the board-native orchestration model: durable work graphs, automatic assignment, persistent child-agent threads, board-level scheduling, and project-level completion.

## Architecture

```text
Clients: macOS / iOS / Web / CLI
                   │
                   ▼
Ago Board Control Plane
  project · workstreams · tasks · dependencies · assignment · status
                   │
                   ▼
Repository Context Engine
  files · symbols · architecture · decisions · task context packages
                   │
                   ▼
Ago Durable Thread Runtime
  mailbox · events · dialogs · artifacts · diff · checks · usage
                   │
          ┌────────┼────────┐
          ▼        ▼        ▼
       local    runners    cloud orbs
          │        │        │
          └────────┼────────┘
                   ▼
       Agent runtime + tool broker
```

The board is the coordination authority. Threads are durable execution units. Agents may finish, restart, or compact without erasing project state, and independent child threads do not inherit one giant parent context.

## Run from source

Current development requirements:

- Go 1.26+
- Bun
- Swift 6 / Xcode for Apple clients
- Git

```bash
git clone https://github.com/raydocs/ago.git
cd ago

go test ./...
go run ./cmd/ago daemon
```

In another terminal:

```bash
go run ./cmd/ago list
go run ./cmd/ago create --workspace /absolute/path/to/repository --content "Implement the requested change"
```

Client-specific development instructions live in:

- [`ago-clients/web`](./ago-clients/web/README.md)
- [`ago-clients/apple`](./ago-clients/apple/README.md)

## Repository map

```text
cmd/ago/                 daemon and CLI
cmd/ago-relay/           authenticated remote relay
internal/agothreadstore/ durable thread, event, board-ready state substrate
internal/agocoordinator/ execution and durable tool coordination
internal/agopluginhost/  plugin lifecycle and reverse requests
internal/agogit/         diff, staging, comments, and safe revert
internal/agoverifier/    server-owned deterministic checks
ago-clients/web/         installable PWA and remote client
ago-clients/apple/       shared Swift core, macOS, and iOS clients
pi-adapter/              pinned agent-kernel adapter
plugin-runtime/          trusted plugin runtime
docs/                    product plan, parity matrix, and runtime contracts
```

## Compatibility

This repository grew from the earlier ClaudeX Flow orchestration runtime. Existing `claudex-flow` commands and configuration remain available as a compatibility layer while Ago's board-native interfaces become the primary product surface. Compatibility code is retained deliberately; it does not define the new product identity.

## Development status

Ago is under active development. Local runtime and synchronized-client foundations are implemented and tested. A broadly distributed macOS release still requires a Developer ID identity, Apple notarization credentials, and a production update-signing key. Managed cloud execution and the complete autonomous board scheduler remain roadmap work.

See the executable [`Ago V0 product contract`](./docs/ago-v0-product-contract.md), the [`usable demo delivery plan`](./docs/ago-usable-demo-delivery-plan.md), the broader [`delivery plan`](./docs/ago-amp-neo-parity-plan.md), and the [`runtime conformance contract`](./docs/ago-runtime-conformance.md).

## Security

- Never commit provider credentials, OAuth tokens, passkey private material, release private keys, or local transcripts.
- Relay and provider credentials remain server-side.
- Remote mutations are scoped to explicit project/thread publications and can require a recent passkey assertion.
- Destructive or shared-infrastructure actions require explicit authorization.

## License

[MIT](./LICENSE)
