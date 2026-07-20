# Ago Usable Demo Delivery Plan

Status: executable delivery plan
Updated: 2026-07-20

This plan turns Ago's existing durable Work Graph slice into a usable local
product demo. It is subordinate to the
[V0 product contract](./ago-v0-product-contract.md): the contract defines the
product invariants, while this document defines the implementation sequence.

## 1. Demo definition of done

The usable demo must complete this observable path:

> A user enters a Chinese natural-language goal in a browser. Ago creates an
> editable dependency DAG, projects it onto a live board, automatically claims
> ready tasks, dispatches them to a replaceable fake or Claude Code executor,
> records logs, artifacts, tests, and evidence, obtains an independent verifier
> decision, and reaches `Done`, bounded retry, or an actionable `Blocked` state.
> The user can pause, resume, retry, approve risky work, or supply missing input.
> Restarting Ago preserves the graph and history without accepting stale work.

The canonical demonstration objective is:

> 分析当前仓库，为 README 增加一个快速开始章节，运行相关测试，并生成完成报告。

This objective exercises repository reads, a bounded write, deterministic
tests, artifact collection, verification, and failure recovery without
requiring automatic publishing.

### P0 capabilities

The demo is not usable until all of the following are present:

1. Chinese goal intake and repository selection.
2. Goal-to-DAG planning with dependencies, acceptance criteria, required
   capabilities, execution environment, and path scope.
3. A durable seven-column board: `Backlog`, `Ready`, `Claimed`, `Running`,
   `Review`, `Blocked`, and `Done`.
4. Atomic claims and duplicate-dispatch prevention.
5. Global, per-project, and repository-writer concurrency limits.
6. A deterministic fake executor and one real local Claude Code executor.
7. Attempt logs, changed-file records, test evidence, and bounded artifacts.
8. An independent verifier that alone can accept work.
9. Bounded retry, exponential backoff, lease expiry, reconciliation, and fencing.
10. Pause, resume, retry, approval, and missing-input controls.
11. Live browser updates without manual refresh.
12. Durable recovery after process restart.
13. Provider credentials excluded from tasks, events, logs, and browser data.

### P1 capabilities

These capabilities make the demo understandable as a product:

- dependency visualization;
- task details with acceptance criteria, attempts, evidence, and artifacts;
- goal progress and current scheduler activity;
- an append-only audit timeline;
- provider health and capability diagnostics;
- plan preview and editing before execution;
- an audited incremental re-plan after new user information;
- one-click built-in examples.

### Explicit non-goals

The first demo does not include multi-machine scheduling, cloud accounts,
billing, complete RBAC, Kubernetes execution, a plugin marketplace, automatic
push or pull requests, or the quarantined Context Package implementation. It
must not depend on an external issue tracker, webhook, or orchestration journal.

## 2. Target architecture

```text
┌─────────────────────────────────────────────────────────┐
│ Thread App                                               │
│ Goal composer │ Board │ Task detail │ Evidence │ Controls│
└───────────────────────────┬─────────────────────────────┘
                            │ HTTP + SSE
┌───────────────────────────▼─────────────────────────────┐
│ Ago Local API                                            │
│ Goal API │ Board API │ Event stream │ User commands      │
└──────┬─────────────┬──────────────┬───────────────┬─────┘
       │             │              │               │
┌──────▼─────┐ ┌─────▼──────┐ ┌─────▼──────┐ ┌──────▼─────┐
│ Planner    │ │ SQLite     │ │ Scheduler  │ │ Providers  │
│ Goal→DAG   │ │ Work Graph │ │ Claim/retry│ │ Fake/Claude│
│ PlanPatch  │ │ + events   │ │ reconcile  │ │ verifier   │
└────────────┘ └─────┬──────┘ └─────┬──────┘ └──────┬─────┘
                     └───────────────┴───────────────┘
                                     │
                              ┌──────▼───────┐
                              │ Attempts     │
                              │ Logs/tests   │
                              │ Artifacts    │
                              │ Evidence     │
                              └──────────────┘
```

The SQLite Work Graph remains the sole source of truth. The UI projects that
truth. The scheduler owns claims. Executors can only run their active attempt
and submit evidence. A separate verifier decides acceptance.

## 3. Delivery sequence

The work is divided into ten independently verifiable increments. Each increment
must leave the repository in a green state and must not rely on a later increment
to make its safety claims true.

| Increment | Outcome | Depends on |
| --- | --- | --- |
| D1 | Goal and board HTTP API | Existing runtime/store |
| D2 | Durable SSE event stream | D1 |
| D3 | Background scheduler and concurrency slots | D1 |
| D4 | Lease expiry, fencing, retry, and reconciliation | D3 |
| D5 | Fake executor and independent verifier E2E | D3–D4 |
| D6 | Evidence and bounded artifact store | D4–D5 |
| D7 | Live board UI and task detail | D1–D2, D5–D6 |
| D8 | Claude Code local provider | D4–D6 |
| D9 | User input and audited PlanPatch | D4, D7 |
| D10 | One-command demo, recovery tests, and documentation | D1–D9 |

## 4. D1 — Local Goal and Board API

### Outcome

A Chinese goal submitted over HTTP creates a durable board and returns a board
snapshot without requiring the user to select a model.

### Implementation

Add a small Go API boundary, preferably under `internal/agoboardapi`, and an
`ago-server` command. HTTP handlers must issue protocol commands through the
runtime/store; they must never update task rows directly.

Minimum endpoints:

```text
POST /api/v1/goals
GET  /api/v1/boards/{boardID}
GET  /api/v1/boards/{boardID}/tasks/{taskID}
POST /api/v1/boards/{boardID}/pause
POST /api/v1/boards/{boardID}/resume
POST /api/v1/boards/{boardID}/tasks/{taskID}/retry
POST /api/v1/boards/{boardID}/tasks/{taskID}/input
GET  /api/v1/providers
```

Goal requests contain only the product inputs and optional safe demo mode:

```json
{
  "objective": "分析当前仓库，为 README 增加快速开始章节并运行测试",
  "repository": {
    "root": "/path/to/repository",
    "revision": "HEAD"
  },
  "executionMode": "fake"
}
```

Every mutating request carries a command ID. Exact replay returns the recorded
result; reuse with different input is rejected. Board snapshots include the
goal, graph version, canonical columns, dependencies, progress, latest event
sequence, and paused/completed state.

### Tests

- Chinese objective round-trips unchanged.
- Exact command replay creates one board.
- Invalid or missing repositories receive a structured Chinese error.
- A board survives API process restart.
- API responses never expose provider secrets.

## 5. D2 — Durable live event stream

### Outcome

The browser receives board changes in real time and can recover after a network
disconnect without missing or duplicating semantic state.

### Implementation

Add `GET /api/v1/boards/{boardID}/events` using server-sent events. Events are
read from the existing append-only log and carry a monotonically increasing
sequence. `Last-Event-ID` resumes after the last acknowledged event. The client
reloads a board snapshot if it observes a sequence gap.

Do not use an in-memory event bus as authority. An optional in-memory notifier
may wake subscribers, but every delivered event must be recoverable from SQLite.

### Tests

- Creation, claim, execution, review, and terminal events stream in order.
- Reconnecting from a cursor returns only later events.
- Replaying a cursor does not mutate the board.
- A server restart preserves resumability.
- Slow clients have a bounded buffer and reconnect rather than exhausting RAM.

## 6. D3 — Durable background scheduler

### Outcome

Ready work advances automatically without manually calling the current
synchronous runtime tick.

### Tick algorithm

Each scheduler tick performs:

1. preflight database and provider health;
2. reconciliation of expired or orphaned attempts;
3. readiness calculation from accepted dependencies;
4. concurrency-slot calculation;
5. atomic lease acquisition and attempt creation;
6. asynchronous dispatch using the recorded attempt identity.

Safe initial limits:

```text
global running attempts:        3
running attempts per board:     2
writers per repository:         1
read-only tasks per repository: 2
concurrent verifiers:           2
```

Claim and attempt creation occur in one transaction. Each attempt records owner,
lease generation, fencing token, acquired time, expiry time, and capability
selection. Only the fresh claim owner may dispatch.

### Tests

- Two scheduler instances racing create one active lease.
- Global and per-board limits are never exceeded.
- Two write tasks for one repository do not overlap.
- Independent read tasks can run in parallel.
- Paused boards create no new attempts.
- Resume restores scheduling from durable state.

## 7. D4 — Retry, lease expiry, fencing, and reconciliation

### Outcome

Crashes and temporary failures do not lose tasks, duplicate accepted work, or
produce an infinite retry loop.

### Retry policy

Use bounded exponential backoff:

```text
delay(attempt) = min(2^attempt × 2 seconds, 30 seconds)
default maximum attempts = 3
```

Retryable failures include provider timeout, temporary network failure, executor
crash, and verifier feedback that can be corrected without changing scope.
Missing credentials, unsafe commands, missing authorization, invalid repository
state, and exhausted attempts become `Blocked`.

Every executor update carries attempt ID and fencing token. Once a lease expires
or a new attempt is created, updates from the old token are recorded as stale
diagnostics and cannot change task state.

### Tests

- A temporary first failure retries after the recorded delay.
- The final allowed failure becomes `Blocked` with no extra attempt.
- Scheduler restart reconciles an expired lease.
- A live but stale executor cannot submit accepted evidence.
- Completed tasks are never redispatched.
- Retry reasons and decisions appear in the audit history.

## 8. D5 — Fake executor and verifier end-to-end

### Outcome

CI and demonstrations can deterministically exercise success, retry, review
rejection, and user-blocked paths without network access or model credentials.

### Implementation

The fake provider accepts a scripted sequence such as:

```json
{
  "outcomes": ["temporary_failure", "success"],
  "verification": "accept"
}
```

Executor output creates evidence but never changes the task to `Done`.
Verifier output is one of:

```text
accept
retry_with_feedback
blocked_needs_input
blocked_policy
```

The state machine rejects verifier decisions made with the active worker's
identity.

### Tests

- Chinese goal reaches `Done` through Fake executor and independent verifier.
- Temporary failure retries once and then succeeds.
- Review rejection records feedback and creates a legal later attempt.
- Missing user input reaches an actionable `Blocked` state.
- Worker self-review is rejected.

## 9. D6 — Evidence and artifact safety

### Outcome

Users can inspect why work was accepted, and untrusted executor output cannot
escape Ago's managed artifact root.

### Evidence contract

An attempt may submit:

- a concise result summary;
- changed paths with before/after hashes;
- commands, exit codes, duration, and bounded output references;
- test results;
- artifact ID, type, byte size, and SHA-256;
- unresolved warnings.

Deterministic checks outrank model judgment. If acceptance can be evaluated by a
test command, file hash, schema check, or parser, the verifier must use that
evidence before requesting a model opinion.

### Artifact boundary

Artifacts live under an Ago-owned directory and receive generated IDs. Reject
absolute paths, parent traversal, symlink or hard-link escape, non-regular files,
input larger than the configured byte limit, and output whose final byte count
or hash differs from the persisted metadata.

Do not integrate the quarantined Context Package artifact until adversarial
hard-link/root containment, preallocated-file size, and stable serialized-byte
tests pass independently.

### Tests

- A task without evidence cannot reach `Done`.
- A failed required test cannot be accepted.
- Hash mismatch is rejected.
- Traversal, link escape, and oversized artifact attempts are rejected.
- A stale attempt cannot replace current evidence.
- Secrets are redacted before logs or artifacts become user-visible.

## 10. D7 — Live board user interface

### Outcome

A non-developer can create and supervise a goal entirely from the browser.

### UI structure

The Thread App gains an isolated Ago board route with:

1. a Chinese goal composer and repository selector;
2. goal progress, scheduler state, and pause/resume controls;
3. the seven canonical columns;
4. task cards showing dependencies, attempt, executor, duration, and blocker;
5. a task drawer with contract, attempts, evidence, artifacts, and events;
6. an activity timeline;
7. provider health without secret values;
8. inline approval and missing-input forms for blocked work.

The UI must not optimistically invent authoritative task state. Dragging a card,
if enabled later, submits a legal protocol command and displays the state
machine's result.

Because `thread-app/src/index.ts` previously contained protected in-flight work,
new board behavior should live in new modules and use the smallest possible
entry-point wiring. Existing canonical-root archive tests remain mandatory.

### Tests

- Creating a goal renders its DAG.
- SSE moves a task through claim, running, review, and done.
- Blocked tasks show the correct approval/input action.
- Refresh and server restart preserve the view.
- Desktop and narrow mobile layouts remain operable.
- Provider credentials never occur in DOM content or network responses.

## 11. D8 — Claude Code local provider

### Outcome

The same scheduler that runs the deterministic fake can execute one real,
bounded repository task through Claude Code.

### Provider boundary

Define capability-oriented provider, executor, and verifier interfaces. The
provider reports health and capabilities such as planning, code read, code
write, shell, tests, verification, long context, and streaming.

The Claude Code adapter:

- executes only inside the selected canonical repository root;
- receives a structured task contract and bounded context references;
- has turn and wall-clock timeouts;
- supports cancellation;
- captures bounded stdout/stderr;
- records changed files and tests;
- cannot access scheduler state transitions directly;
- cannot push, publish, or run destructive commands without approval.

### Provider configuration

OpenAI-compatible relay configuration is introduced behind the same secure
profile layer:

```text
AGO_PROVIDER_BASE_URL
AGO_PROVIDER_API_KEY
AGO_PLANNER_MODEL
AGO_EXECUTOR_MODEL
AGO_VERIFIER_MODEL
```

SQLite stores only a provider profile ID and non-secret capability metadata.
The API reports whether authentication is configured, never its value. Child
processes receive only the minimum environment required by their provider.

### Failure behavior

- Planner outage may fall back to a deterministic planner in demo mode.
- A write executor is not silently replaced with another provider.
- Verifier outage keeps work in `Review` and retries within policy.
- Every fallback or provider change is audited.

### Tests

- Capability mismatch prevents dispatch.
- Timeout cancels the Claude Code process and records a retryable failure.
- Provider health degradation is visible.
- Credentials do not appear in SQLite, events, logs, artifacts, HTTP, or DOM.
- A controlled fixture repository completes one real task with evidence.

## 12. D9 — User input and dynamic PlanPatch

### Outcome

New information changes the active plan without deleting history or silently
rewriting running work.

Supported patch operations are:

```text
add_task
split_task
supersede_task
add_dependency
remove_dependency
update_acceptance
block_task
cancel_task
```

Each patch records before, after, reason, actor, command ID, graph version, and
timestamp. Completed tasks remain immutable history. Running work is either
allowed to finish under its original contract or explicitly cancelled before a
replacement task is admitted. Every patch is validated for cycles, missing
references, illegal state changes, and path-ownership conflicts.

Safe patches may apply automatically. Scope expansion, destructive actions,
publishing, credential changes, and cancellation of active writes require user
approval through the same goal UI.

### Tests

- Added tasks receive correct readiness.
- Split tasks preserve the superseded task and its history.
- Cyclic patches are rejected atomically.
- Running tasks are not silently removed.
- Exact patch replay is idempotent.
- Approval-gated patches wait for the user and then resume scheduling.

## 13. D10 — One-command demo and release gate

### Outcome

A new user can launch the fake demo without credentials and can opt into the
Claude Code demo after explicit local configuration.

Target commands:

```text
ago demo --executor fake
ago demo --executor claude-code
```

Startup prints only safe diagnostics:

```text
Ago is ready
UI:       http://127.0.0.1:4317
Database: ~/.ago/demo/ago.db
Provider: fake
```

Preflight verifies the SQLite directory, repository root, provider binary,
provider configuration, port availability, and artifact directory permissions.
A scoped reset command may remove one Ago-owned demo board; it must never clean
the repository, alter Git state, delete stashes, or remove unrelated files.

### Built-in fixture DAG

The offline fixture should deterministically produce:

1. inspect project metadata;
2. identify build and test commands;
3. run tests after task 2;
4. summarize after tasks 1 and 3;
5. independently verify task 4.

Tasks 1 and 2 may run in parallel. The fixture includes selectable success,
temporary-failure-then-success, and user-input-required scenarios.

## 14. Final demonstration script

### Scenario A — successful goal

1. Start `ago demo --executor fake`.
2. Enter the canonical Chinese objective.
3. Inspect and approve the generated DAG.
4. Watch tasks move through the canonical columns.
5. Open a task and inspect commands, changed files, tests, hashes, artifacts,
   and verifier reasoning.
6. Observe evidence-backed goal completion.

### Scenario B — bounded failure recovery

1. Configure the fake executor to fail temporarily once.
2. Observe the recorded retry decision and countdown.
3. Observe the second attempt succeed.
4. Confirm the first failure remains in the audit history.

### Scenario C — user decision

1. A task requests approval and moves to `Blocked`.
2. The user approves from the task drawer.
3. Ago records the approval event, returns the task to eligibility, and resumes.
4. The user never needs to locate or message a temporary worker.

### Scenario D — dynamic re-plan

1. While work is active, enter: `不要修改脚本，只更新文档，并增加 macOS 验证。`
2. Review the generated PlanPatch.
3. Apply it and observe new tasks and dependencies.
4. Confirm superseded tasks and earlier events remain inspectable.

### Scenario E — restart recovery

1. Stop Ago while a task is running or under review.
2. Restart the same demo database.
3. Observe graph recovery and lease reconciliation.
4. Confirm completed work is not redispatched and stale evidence is rejected.

## 15. Quality and security gates

Each increment runs the narrowest relevant tests followed by the combined gate:

```bash
go test -race -count=1 ./internal/agoboardprotocol \
  ./internal/agoboardstore ./internal/agoplanner \
  ./internal/agoboardruntime
go vet ./...
go test -count=1 ./...

cd thread-app
npm run typecheck
npm test

git diff --check
```

As the API, scheduler, and browser packages are added, their focused race,
integration, restart, and browser tests join this gate. Before publishing a
commit, scan the staged content for credentials.

The demo release gate requires:

- Chinese goal intake reaches an evidence-backed terminal result;
- concurrent schedulers cannot duplicate an active claim;
- concurrency limits hold under race tests;
- retry is bounded and auditable;
- stale executor evidence is fenced out;
- the fake path works without network access;
- one Claude Code fixture completes safely;
- pause, resume, retry, approval, and user input work;
- an incremental PlanPatch preserves history;
- process restart recovers the graph;
- API, SQLite, logs, artifacts, and DOM contain no provider secret;
- artifact containment adversarial tests pass;
- all Go, TypeScript, API, and browser checks pass;
- a clean-machine user can start the fake demo from the README in ten minutes.

## 16. Schedule and immediate next action

Estimated focused implementation time:

| Work | Estimate |
| --- | ---: |
| API and durable SSE | 1–2 days |
| Scheduler, fencing, retry, reconciliation | 2–3 days |
| Fake provider, evidence, and verifier | 1–2 days |
| Live board UI | 2–3 days |
| Claude Code provider | 1–2 days |
| Dynamic PlanPatch and user controls | 2 days |
| Packaging, E2E, and security hardening | 1–2 days |
| Total | 10–16 focused development days |

A reduced 5–7 day demo can stop after D7 with the deterministic fake executor,
but it must still include retry, blocking, independent verification, live board
updates, and restart recovery. The real Claude Code path and dynamic PlanPatch
then follow without changing the Work Graph authority model.

The immediate next implementation is **D1 followed by D2**: write failing API
contract and event-resume tests, add the local Goal/Board API, and prove with a
Chinese request that `Goal -> DAG -> durable board -> live events` works before
expanding the scheduler.
