# Ago — Amp Neo Parity Plan

Status: active
Plan version: 1.0
Started: 2026-07-18

## 1. Product Contract

Ago is an independent, local-first coding-agent product. A user opens a local
repository, states an objective, and receives an implemented and verified
result without choosing providers, models, agents, or harness configuration.

Ago will first reproduce the public behavior of rebuilt Amp/Neo. Differentiation
comes only after the parity foundation is reliable. The first planned
differentiator is stage-level automatic model selection.

This is clean-room behavioral parity:

- use Amp's public documentation and observable behavior as the specification;
- reuse only code whose license permits it;
- do not copy Amp private source, branding, icons, copy, or proprietary assets;
- keep Ago's protocol, persistence, gateway, UI, and runtime implementation
  independently owned.

## 2. Fixed Decisions

- Product name: **Ago**.
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
- Existing ClaudeX/claudex-flow behavior remains available while Ago is built.
  Rename or migration work happens only when a parity slice requires it.
- Delivery order follows Amp's published rebuilt-era dependency chain: Neo local
  runtime and plugins, synchronized clients, diffs, custom agents, managed orbs,
  long-thread retrieval, user-managed runners, workload identity, then
  agent-to-agent orchestration. Ago does not move named runners ahead of orbs.
- Current user-owned changes in `thread-app/src/index.ts` and
  `thread-app/test/thread-api.test.mjs`, plus unrelated untracked outputs, are
  protected and must not be overwritten, reverted, staged, or folded into Ago
  changes without explicit authorization.

## 3. Existing Assets And Gaps

The current repository is not a blank slate. Reuse candidates include:

- Go model catalog, router, route evaluation, lane health, and usage ledger;
- Claude execution and bounded repair workflow;
- transcript parser and sanitized Root/Parent/Child thread graph;
- thread read/find/usage support;
- compaction auditing and supervisor gates;
- Cloudflare thread archive and event ingest;
- existing Amp-style UI studies and parity reports.

The largest gaps relative to rebuilt Amp are:

1. existing thread data is primarily observed from Claude transcripts rather
   than owned by an Ago durable thread runtime;
2. there is no Ago headless local daemon that owns agent execution independently
   from a UI;
3. there is no canonical command mailbox with queue, steer, interrupt, and
   idempotency semantics;
4. there is no executor abstraction covering local, named runner, and orb;
5. remote web control is archival/read-oriented rather than a scoped command
   plane;
6. child agents are currently worker sessions, not fully durable independent
   threads with authenticated reply routes and file transfer;
7. no Ago desktop app consumes a stable command/event protocol;
8. plugin behavior is not exposed through an Ago-owned lifecycle API;
9. model routing exists, but it is not yet integrated into an Ago-owned
   stage/thread runtime with accepted-result evaluation.

## 4. Target Architecture

```text
Desktop / CLI / Web / Mobile
            |
            | versioned commands + ordered events
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

### Phase 3 — Custom agents and persistent child threads

Goal: build custom and built-in agent handles on the plugin/thread substrate,
matching Amp's local custom-agent release before remote executor orchestration.

Deliverables:

- agent-definition snapshots: model, instructions, tools, effort, label/color;
- built-in and custom agent handles;
- one-shot `run` and persistent `createThread` APIs;
- asynchronous message acceptance separated from response waiting;
- parent-thread linkage, observable state, and cancellation;
- selectable custom main-agent modes;
- configured-concurrency background child threads visible in the same sidebar.

Exit gate:

- a custom agent can run once or create a persistent child thread;
- appending to a child returns after durable acceptance rather than waiting for
  inference;
- waiting for a response is an explicit operation;
- parent completion does not implicitly cancel or join independent children.

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
