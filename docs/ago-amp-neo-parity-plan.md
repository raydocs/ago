# Ago — Product and Delivery Plan

Status: active
Plan version: 1.0
Started: 2026-07-18

## 1. Product Contract

Ago is an intelligent project board that plans, delegates, monitors, and
finishes work with agents. A user opens a repository and states an objective.
Ago turns that objective into a durable work graph with tasks, dependencies,
acceptance criteria, assigned agents, live status, artifacts, and verification.
Before planning, Ago builds a repository map and reusable context packages from
the codebase, architecture, project rules, and current state.

The board is itself an orchestration agent rather than a passive task list. It
automatically dispatches runnable work, observes durable agent progress,
re-plans around failures or changed evidence, integrates results, and owns the
project-level completion decision. Users do not choose providers, models, agent
topology, or harness configuration.

Board state is the durable project memory. Each worker receives a bounded task
contract and relevant evidence instead of inheriting one ever-growing root
conversation. This prevents long projects from failing because one context is
full while preserving the history required to monitor, resume, and verify the
whole project.

Amp/Neo parity supplies the reliable thread, client, diff, plugin, and execution
foundation. It is an implementation baseline, not Ago's product identity. Ago's
primary differentiation is board-native autonomous orchestration; automatic
capability/model selection is one policy within that system.

The concise product model is:

```text
automatic capability routing (Amp pattern)
  + repository context engineering (RepoPrompt pattern)
  + autonomous project board (Linear pattern)
  + durable collaborative work items (GitHub Issues pattern)
  = Ago
```

These names describe public product patterns, not affiliations or source-code
dependencies. Ago joins the patterns into one control loop: a durable work item
defines the contract, the context engine assembles its bounded inputs, the
router assigns the best eligible capability, and the board supervises execution
through acceptance.

This is clean-room behavioral parity:

- use Amp's public documentation and observable behavior as the specification;
- reuse only code whose license permits it;
- do not copy Amp private source, branding, icons, copy, or proprietary assets;
- keep Ago's protocol, persistence, gateway, UI, and runtime implementation
  independently owned.

## 2. Fixed Decisions

- Product name: **Ago**.
- Primary interaction: one user objective creates or updates an Ago board.
- Coordination authority: the board agent owns decomposition, dependencies,
  assignment, monitoring, re-planning, integration, and final acceptance.
- Context policy: project history and evidence remain durable on board tasks;
  worker agents receive bounded task-specific context.
- Work hierarchy: project board -> workstream/subproject -> task -> agent
  thread/attempt. Any oversized task can be refined again before dispatch.
- Work-item contract: each task behaves like a durable issue with stable
  identity, owner, status, discussion/events, dependencies, artifacts, attempts,
  acceptance criteria, and automation triggers.
- Context engine: repository files, symbols, architecture, rules, prior
  decisions, and upstream artifacts are selected into task-specific context
  packages rather than copied wholesale into prompts.
- Assignment policy: users do not manually choose models or spawn agents for
  ordinary work. Ago selects an eligible capability and executor from policy,
  availability, cost, and verification requirements.
- Initial execution: local machine and local repository.
- Initial client: macOS desktop app; headless daemon comes before the UI.
- Default tool policy: execute automatically, matching Amp's advanced-user
  default. Confirm only destructive, irreversible, publishing, purchasing, or
  shared-infrastructure actions.
- Runtime shape: durable threads plus pluggable executors.
- UI/runtime boundary: commands in, events out. The renderer never owns agent
  execution state.
- Agent kernel candidate: pinned Pi packages behind an Ago-owned adapter and
  conformance suite. A short bakeoff may select a pinned OpenCode daemon if Pi
  cannot satisfy the contract.
- Model access: Ago Gateway in front of the existing relay. Clients never receive
  relay credentials.
- Model policy: capability aliases, not model names, are part of product logic.
  GLM is allowed when evaluation supports it.
- Amp research and implementation child threads launched by the project use
  Amp `medium` unless the user explicitly changes that policy.
- Existing ClaudeX/claudex-flow commands remain as a compatibility layer while
  Ago's board-native commands and clients become the primary surface.
- Delivery order follows Amp's published rebuilt-era dependency chain: Neo local
  runtime and plugins, synchronized clients, diffs, custom agents, managed orbs,
  long-thread retrieval, user-managed runners, workload identity, then
  agent-to-agent orchestration. Ago does not move named runners ahead of orbs.
- Current user-owned changes in `thread-app/src/index.ts` and
  `thread-app/test/thread-api.test.mjs`, plus unrelated untracked outputs, are
  protected and must not be overwritten, reverted, staged, or folded into Ago
  changes without explicit authorization.

## 3. Existing Foundation And Product Gap

The current repository is not a blank slate. Implemented foundation includes:

- durable Ago threads, ordered events, mailbox controls, and restart recovery;
- local execution, plugin lifecycle, bounded repair, and supervisor gates;
- macOS, iOS, Web/PWA, and CLI projections over one authoritative daemon;
- attachments, usage, verification, Git review, staging, and safe revert;
- authenticated relay transport and recent-passkey remote mutation gates;
- model catalog, route evaluation, lane health, and usage infrastructure;
- transcript retrieval, compaction auditing, and parent/child graph assets.

The largest remaining product gaps are:

1. no durable board aggregate yet owns task nodes, dependencies, readiness,
   assignment, evidence, and project-level completion;
2. child agents are not yet fully durable independent Ago threads attached to
   board tasks with authenticated reply routes and bounded handoffs;
3. the scheduler does not yet automatically lease every runnable task to an
   eligible agent/executor and recover expired work;
4. board clients do not yet provide the complete live project view and control
   surface across task status, dependencies, blockers, artifacts, and checks;
5. managed cloud orbs and named runners are not yet available through the common
   executor contract;
6. automatic capability routing is not yet evaluated and promoted as a
   board-stage policy against held-out project outcomes.

## 4. Target Architecture

```text
Desktop / CLI / Web / Mobile
            |
            | versioned commands + ordered events
            v
Ago Board Control Plane
- objective and project completion contract
- hierarchical work graph and dependency readiness
- automatic agent/executor assignment
- live status, blockers, evidence, and artifacts
- lease expiry, retry, re-plan, and integration
            |
            v
Ago Repository Context Engine
- repository map, symbols, and architecture
- relevant-file and constraint selection
- task-specific context packages
- upstream decisions and artifact references
            |
            v
Ago Thread Control Plane
- durable thread records
- ordered mailbox
- queue / steer / interrupt
- parent / child provenance
- compaction snapshots
- artifacts and file transfers
- usage and executor binding
            |
            v
Executor adapters
- local daemon (first)
- named runner
- cloud orb
            |
            v
Ago Agent Runtime
- pinned agent kernel adapter
- Ago tool broker
- plugin lifecycle
- permission policy
- context projection and compaction
            |
            +---- local/cloud workspace tools
            |
            +---- Ago Model Gateway ---- relay/providers
```

### 4.1 Trust boundaries

- The board agent is the coordination authority, but it may mutate work only
  through versioned board/thread commands and durable evidence.
- A worker cannot mark its own task or project accepted. Board acceptance is
  derived from the frozen criteria and verifier results.
- Desktop renderer is untrusted presentation code and communicates only through
  the Ago protocol.
- The local daemon owns SQLite, thread state, worker lifecycle, permissions,
  gateway access, and workspace leases.
- Repository commands run in a workspace process separated from the renderer.
- Relay credentials remain server-side. The desktop receives a short-lived,
  account-scoped Ago token.
- Inter-thread source and reply metadata is generated by Ago, never trusted from
  prompt text.

## 5. Canonical Thread Model

A durable thread contains:

- stable thread ID and URL-safe public identifier;
- owner, project, repository, working directory, and base revision;
- immutable parent thread ID and root correlation ID;
- immutable snapshot of selected agent mode/definition;
- executor intent and current executor lease;
- ordered messages and tool events;
- normal and steering message queues;
- current state and active turn;
- compaction snapshots;
- artifacts, changed files, verification, and usage;
- creation, update, archive, and terminal timestamps.

A durable board contains:

- stable board ID, project identity, objective, and completion contract;
- hierarchical workstream/task nodes with immutable task contracts, parent
  links, and dependency edges;
- readiness, assignment, lease, attempt, blocker, and terminal state;
- attached worker thread IDs and bounded context/handoff references;
- artifacts, changed paths, verification evidence, cost, and accepted result;
- board-level decisions, re-plans, and project completion evidence.

Public thread states initially match Amp's observable surface:

```text
idle
running
awaiting-approval
error
```

Ago may maintain internal substates such as `requested`, `provisioning`,
`offline`, `compacting`, and `cancelled`, but clients receive a stable projected
state.

## 6. Command And Event Contract

Every command has a schema version, command ID, idempotency key, authenticated
actor, thread ID when applicable, and expected aggregate sequence when required.

Initial commands:

```text
thread.create
thread.archive
message.append
message.steer
message.dequeue
turn.interrupt
turn.cancel
approval.resolve
thread.create_child
thread.send_message
artifact.upload
artifact.download
```

Initial durable events:

```text
thread.created
thread.state_changed
message.accepted
message.queued
message.steered
message.dequeued
turn.started
turn.completed
turn.failed
turn.cancelled
tool.started
tool.completed
tool.failed
approval.required
approval.resolved
compaction.started
compaction.completed
executor.bound
executor.offline
artifact.created
artifact.transferred
usage.recorded
thread.archived
```

Delivery is at least once. Consumers deduplicate by event ID and order by
per-thread sequence. A client reconnects from `afterSequence`; if history has
expired it reloads a thread snapshot and resumes from that sequence.

## 7. Neo-Parity Delivery Phases

### Phase 0 — Contract and baseline

Goal: freeze Ago's clean-room behavior specification and establish a measured
baseline before runtime changes.

Deliverables:

- this plan and persistent goal ledger;
- public Amp Chronicle feature timeline and parity matrix;
- runtime-kernel conformance contract;
- baseline map of existing repository capabilities;
- protected-worktree record;
- deterministic tests for the first protocol types and state transitions.

Exit gate:

- every parity feature is classified as existing, partial, missing, or deferred;
- protocol types compile and their invariants are covered by focused tests;
- no current user-owned changes were modified.

### Phase 1 — Neo local foundation and plugin substrate

Goal: reproduce the rebuilt Amp local runtime, compaction, and plugin substrate
before building a polished app.

Deliverables:

- Ago headless daemon;
- SQLite durable thread/event store;
- one-active-turn-per-thread mailbox;
- normal queue, steer, dequeue, interrupt, and cancel;
- local executor binding;
- automatic tool execution default;
- automatic compaction with structured recovery snapshot;
- immutable full history alongside the compacted inference projection;
- headless CLI/debug client;
- restart and event-replay recovery;
- multiple concurrent threads;
- lifecycle events: session start, agent start/end, tool call/result;
- internal tool, command, policy, and serializable UI-request registration.

Exit gate:

- a thread can run without the desktop UI;
- closing and reopening the client does not stop the daemon or lose state;
- queued and steering messages obey deterministic ordering tests;
- daemon restart resumes from a safe boundary;
- compaction preserves objective, decisions, changed paths, verification, active
  work, and next action;
- original pre-compaction events remain searchable and retrievable;
- cancellation is acknowledged and leaves a coherent thread state;
- the default permission policy is implemented as a plugin, not hard-coded into
  the agent loop.

### Phase 2 — Synchronized clients and thread-bound diffs

Goal: make the local foundation usable through Amp-like desktop, CLI, and remote
web/mobile projections, then close the remote-control loop with thread-bound
diff review.

Deliverables:

- macOS desktop shell;
- thread sidebar and active/background status;
- transcript with expandable thinking and tool blocks;
- queued-message editing, dequeueing, steering, and forced interrupt;
- attachments and file mentions;
- verification and cost display;
- notifications, archive, search, and project switching;
- renderer reconnection after crash/reload;
- remote control of already-running local threads from web/mobile;
- synchronized plugin notifications, confirms, inputs, and selects;
- optional recent-passkey gate for remote control;
- per-thread diff review, section-specific change requests, and interactive
  staging while the environment is active;
- duplicate-block-aware diff presentation;
- signed build and update channel.

Exit gate:

- renderer termination does not terminate active work;
- the complete thread can be restored from daemon state;
- every client projects the same authoritative thread, queue, plugin-dialog, and
  diff state;
- a user can open a repository, request a change, review the diff and checks, and
  accept or revert Ago-owned changes without selecting a model;
- 5,000-message transcript performance remains within an explicit CPU/memory/UI
  latency budget.

### Phase 3 — Board-native orchestration and persistent child agents

Goal: make the Ago board the autonomous coordination authority. One objective
must become a durable task graph whose runnable nodes are assigned to persistent
child-agent threads with bounded context, observable progress, and evidence-based
acceptance.

Deliverables:

- durable board, task, dependency, attempt, assignment, and evidence records;
- repository mapping and task-specific context packages with file, symbol,
  architecture, constraint, decision, and upstream-artifact references;
- objective-to-board recursive planning with workstreams, explicit task
  contracts, refinement of oversized tasks, and a project exit gate;
- dependency-aware readiness and automatic assignment of runnable work;
- board-agent monitoring over durable events rather than worker prompt claims;
- bounded task briefs and artifact/evidence references instead of copied history;
- lease expiry, safe retry, blocker propagation, and evidence-driven re-planning;
- project-level integration and acceptance that workers cannot self-approve;
- agent-definition snapshots: model, instructions, tools, effort, label/color;
- built-in and custom agent handles;
- one-shot `run` and persistent `createThread` APIs;
- asynchronous message acceptance separated from response waiting;
- parent-thread linkage, observable state, and cancellation;
- selectable custom main-agent modes;
- configured-concurrency background child threads visible on the board and in
  the same project sidebar;
- a high-level board/progress view and a drill-down engineering view for agent
  threads, context, code, diffs, logs, artifacts, and checks.

Exit gate:

- one user objective creates a board with valid tasks and acyclic dependencies;
- every dispatched task has a bounded repository context package and an
  explicit input, output, owner, dependencies, and completion contract;
- an oversized task can be split into linked child tasks without copying the
  full project history into any child thread;
- every runnable task is assigned automatically without the user choosing an
  agent or model;
- blocked dependencies prevent dispatch and completion unlocks dependents;
- the board can recover after coordinator restart without duplicate assignment;
- a failed or expired task is retried or re-planned only from durable evidence;
- board completion requires every mandatory task plus the project exit gate;
- a custom agent can run once or create a persistent child thread;
- appending to a child returns after durable acceptance rather than waiting for
  inference;
- waiting for a response is an explicit operation;
- parent completion does not implicitly cancel or join independent children;
- a long project completes while each worker sees only bounded task context.

### Phase 4 — Amp-style managed orbs

Goal: add Ago-managed isolated cloud execution before generalizing execution to
user-managed runners, matching Amp's published sequence.

Deliverables:

- cloud executor adapter;
- one fresh workspace/filesystem per orb thread;
- project/repository authorization and fresh clone;
- prepared base image and agent-legible installed tools;
- `.agents/setup` on fresh creation with setup-result snapshot/cache;
- `.agents/resume` on wake with bounded idempotent repair;
- pause, wake, archive, inactivity expiry, and paused-cost semantics;
- file browser, terminal, diff/review, and usage through existing clients;
- local sync/export while remote work continues;
- execute-mode orb launch;
- supervised services and authenticated preview portals;
- selectable compute/disk profiles.

Exit gate:

- local and orb executors pass the same thread/runtime conformance suite;
- an orb can be created, observed, steered, cancelled, paused, resumed, reviewed,
  and synchronized;
- a blank VM without setup/resume/observability does not count as parity;
- cloud cost has a hard per-thread limit;
- no assumption is made that an orb sees local uncommitted changes.

### Phase 5 — Truth-preserving retrieval, named runners, and workload identity

Goal: make very large compacted threads trustworthy, generalize executor
placement to user-managed machines, and replace durable orb secrets with scoped
workload identity.

Deliverables:

- `read_thread(question)` implemented as search plus selective original-event
  reads, not one giant prompt;
- compaction summaries used for orientation, never as the only historical truth;
- later-event checks for revision, contradiction, reversion, and supersession;
- distinction between attempted tool calls and confirmed results;
- runner enrollment, revocation, and outbound authenticated connection;
- interactive clients optionally accepting remote thread creation;
- `--no-tui` headless runner mode;
- stable hostname-valid runner IDs plus host/directory/capability metadata;
- multiple runners per host and arbitrary startup directories;
- durable runner-offline queue with no silent executor fallback;
- short-lived audience-bound OIDC identity with immutable workspace/project/user/
  thread claims and standard discovery/JWKS.

Exit gate:

- exact claims and chronology are recovered from original durable events after
  many compactions;
- no inbound port is required on an enrolled runner;
- disconnect/reconnect preserves order and identity;
- an unavailable runner is never silently replaced by an orb;
- relying services verify OIDC signature, issuer, audience, expiry, and immutable
  authorization claims;
- long-lived cloud credentials are absent from orb workspaces for supported
  identity exchanges.

### Phase 6 — Agent-to-agent parity

Goal: match Amp's public ability for agents to create and coordinate durable
threads anywhere.

Deliverables:

- non-blocking child-thread creation;
- immutable parent/root/source/reply provenance;
- child executor selection: local, orb, named runner;
- authenticated inter-thread messages;
- automatic reply routes;
- same-user/thread authorization;
- file upload/download with initial 4 MiB limit and no-overwrite default;
- paused-orb wake and runner-online semantics;
- fan-out grouping, aggregate state and cost;
- spawn depth, concurrency, artifact, and spend quotas;
- cross-project authorized child creation.

Exit gate:

- a parent creates several medium child threads, continues immediately, and
  receives authenticated replies later;
- child failure never silently corrupts or cancels the parent;
- messages and files preserve provenance;
- runner unavailability does not silently fall back to an orb;
- recursive creation respects hard depth, concurrency, and spend limits.

### Phase 7 — Automatic model selection advantage

Goal: improve on parity without exposing model operations to users.

Capabilities may include:

```text
FAST
MAIN
REPO_EXPLORE
DEEP_REASONING
FRONTEND_VISUAL
DEBUGGING
REVIEW
MEDIA
COMPACTION
```

GLM, GPT, Kimi, Gemini, Grok, Claude, and future models are eligible only when
their real route is commercially permitted and their evaluation evidence meets
the capability gate.

Deliverables:

- versioned model cards;
- capability hard filters;
- provider health and circuit breakers;
- stage-level route decisions;
- fallback by classified failure;
- fixed single-model baselines;
- golden repository task suite;
- canary and rollback policy;
- accepted-result, cost, latency, repair, and human-correction metrics.

Exit gate:

- routing improves accepted-result success by at least 8 percentage points at
  comparable cost, or lowers variable cost by at least 20% without material
  quality regression, on a held-out task set;
- a model regression can be quarantined without a desktop release;
- users never need to configure providers or models.

## 8. Recovery And Durability Rules

- Persist a command before acknowledging it.
- Persist a tool request before dispatch and its result after completion.
- Do not blindly replay a side-effecting tool call whose completion is unknown.
- Restart the current turn/stage from the last committed safe snapshot.
- Parent completion does not wait for child completion unless an explicit wait
  primitive is used.
- Parent cancellation does not cascade by default; cascade is explicit.
- A runner going offline retains queued commands and exposes waiting state.
- An orb and local runner never silently substitute for one another because
  their filesystem and trust semantics differ.

## 9. Security And Product Policy

- Default automatic execution is intentional and visible in onboarding.
- Project instructions and repository content are untrusted input.
- Protect paths outside the selected workspace and sensitive user locations.
- Never ship relay/provider credentials in the desktop app.
- Never infer authorization from thread IDs, runner names, or prompt text.
- Sign and audit remote commands, executor assignment, file transfer, publishing,
  secret use, and destructive operations.
- Require explicit approval for destructive, irreversible, purchase, publish,
  deployment, or shared-infrastructure actions.
- Add policy plugins rather than static shell-string security theater.

## 10. Verification Strategy

Each parity slice requires:

1. protocol/state-machine unit tests;
2. runtime conformance tests independent of UI;
3. process-death/restart tests for durable execution;
4. focused integration tests for the affected executor/client;
5. security tests for path, provenance, authorization, and duplicate delivery;
6. one representative end-to-end task;
7. comparison against the corresponding publicly documented Amp behavior.

Do not claim parity from screenshots alone. Do not claim model superiority from
one run. Do not use LLM judgment where a deterministic invariant exists.

## 11. Explicit Non-Goals Before Parity

- inventing a novel agent-team abstraction;
- exposing model/provider selection;
- user-editable routing rules;
- arbitrary multi-harness support;
- a plugin marketplace;
- self-hosted cloud VM infrastructure;
- production deployment automation;
- unrestricted recursive agent spawning;
- broad language/framework promises before the runtime is durable;
- redesigning unrelated existing ClaudeX functionality;
- cosmetic rebranding of the entire repository before Ago protocol boundaries
  exist.

## 12. Immediate Execution Gate

The first implementation slice is deliberately small and foundational:

1. inventory existing code against Phase 0 and preserve user-owned changes;
2. add Ago-owned versioned thread command/event types and invariants;
3. add focused tests for sequence, provenance, queue class, executor intent, and
   idempotency fields;
4. decide the durable local store boundary only after those contracts compile;
5. then implement the SQLite-backed thread store and daemon.

The first slice is complete only when focused tests pass and no existing
thread-app changes are touched.

## 13. Official Reference Set

- https://ampcode.com/chronicle
- https://ampcode.com/news/neo
- https://ampcode.com/news/agents-everywhere
- https://ampcode.com/news/custom-agents
- https://ampcode.com/news/agents-in-orbs
- https://ampcode.com/news/from-agent-to-agent
- https://ampcode.com/manual
- https://ampcode.com/manual/plugin-api
- https://ampcode.com/manual/orbs

Research threads:

- Chronicle timeline: https://ampcode.com/threads/T-019f7737-b9b6-739b-8bfb-da4a40dadc64
- Neo protocol: https://ampcode.com/threads/T-019f7737-e127-766a-a9eb-1c62687bd14c
- Agent-to-agent: https://ampcode.com/threads/T-019f7737-e7ce-7709-8c99-7eaa5980b997
- Runtime foundation: https://ampcode.com/threads/T-019f7737-ed89-744e-944a-425e02f4b448
- UX parity: https://ampcode.com/threads/T-019f7737-f7ab-751f-a667-7fd66aa4735b
