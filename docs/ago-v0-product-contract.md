# Ago V0 Product Contract and Competitive Benchmark

Status: executable product contract
Contract version: 1.1
Updated: 2026-07-20

This document fixes the smallest Ago product slice that can be evaluated as a
product rather than as a thread-runtime demo. It is normative for V0. The
broader [product and delivery plan](./ago-amp-neo-parity-plan.md) remains a
roadmap; where that roadmap is broader, this contract controls V0.

## 0. Product north star and first executable slice

Ago (Argo in early product discussions) is a natural-language dynamic
orchestration system for Chinese users. One goal becomes a durable task DAG and
a live board. The board is both the user's progress view and the scheduler's
shared work graph; it is not a second, UI-only copy of task state.

The canonical task projection is:

`Backlog -> Ready -> Claimed -> Running -> Review -> Done`, with `Blocked` for
dependency, execution, verification, authorization, or exhausted-retry stops.
Every transition is atomic and append-only-audited. Only the scheduler may
claim work, a worker may only execute its active lease and submit evidence, and
an independent verifier—not the worker—decides acceptance. Dynamic task add,
split, remove, and dependency reorder operations must preserve earlier graph
versions and record their reasons.

The first executable slice is deliberately narrower than the complete V0 gate:

1. accept an unchanged Chinese natural-language goal;
2. validate a bounded objective-to-DAG proposal containing dependencies,
   acceptance criteria, path scope, verifier IDs, and capability tags;
3. persist the graph and project it into the canonical board columns;
4. atomically claim one ready task and dispatch a replaceable local executor;
5. persist artifact/test evidence and require an independent verifier decision;
6. project acceptance as `Done`, or execution/review failure as `Blocked`.

This slice must have a deterministic fake-executor integration test. It does
not claim automatic retry, scheduler restart, Context Package safety, dynamic
re-planning, or provider failover until those paths have their own durable
records and executable acceptance tests.

### Provider and security boundary

Scheduler policy and execution are separate. Amp, Claude Code, and future
providers are adapters behind one capability-oriented executor interface. A
future provider configuration layer owns API base URL, authentication method,
model mapping, capability probing, health, and fallback policy. Credentials
must never enter goals, task contracts, board events, Context Packages, worker
prompts, or child-process logs. Users describe outcomes; routine provider/model
selection remains Ago policy.

### Competitive mechanisms adopted—and boundaries retained

- From PlanWeave (MIT): durable graph nodes, focused claimable work, and review
  and recovery as first-class workflow. Ago does not copy its code or make a
  file package/desktop canvas the product authority.
- From Orca (MIT): provider-neutral local runtimes, isolated execution, diff
  review, and remote/mobile supervision. Ago differentiates by owning the goal
  DAG and evidence-based completion rather than primarily arranging terminals.
- From Multica (public open-source product): teammate-like task lifecycle,
  blocker reporting, runtime visibility, and reusable capabilities. Ago does
  not require an external issue system; its own Work Graph is authoritative.

The A-22 Context Package artifact remains quarantined from dispatch until
adversarial regression tests independently cover descriptor-root/hard-link
containment, preallocated input-size limits, and stable serialized-byte limits.
Passing standalone happy-path tests is not sufficient for that integration.

## 1. Full-V0 target outcome and product claim

Sections 1 through 9 define the unmet full-V0 acceptance target. Present-tense
requirements in those sections are normative behavior to prove before a V0
release, not a claim that the current implementation already provides it.

Given a local repository and one objective, Ago creates a durable Work Graph,
prepares bounded repository context for every dispatched task, automatically
assigns eligible local agents, survives scheduler restart, and reports project
completion only from durable task and verification evidence.

V0 has one product claim:

> A user supplies **repository + objective**. Ago coordinates the resulting
> local project through a visible dependency graph to evidence-backed
> completion without asking the user to choose a model.

This claim is accepted only by **PC-01 through PC-10** below. A successful chat
response, an attractive board, a worker's self-report, or one passing leaf task
is not project completion.

## 2. Frozen V0 flow

The required user input is exactly:

- `repository`: a local repository path selected by the user;
- `objective`: non-empty natural language describing the desired outcome.

The normal flow is:

1. **Intake** validates the repository and records the objective and repository
   revision. No provider, model, agent count, topology, issue tracker, or
   executor choice is required from the user.
2. **Understand** creates a repository map from tracked files, symbols,
   architecture, repository instructions, and available deterministic checks.
3. **Plan** writes a versioned, acyclic Work Graph with task contracts,
   dependencies, and a project completion contract before dispatch.
4. **Package** creates a Context Package for each runnable task.
5. **Assign** selects an eligible local capability under Ago policy. Model names
   may be visible as diagnostics, but no model picker blocks execution.
6. **Execute** runs ready tasks subject to dependency and ownership rules.
7. **Observe and recover** persists state transitions, evidence, attempts, and
   scheduler decisions; restart reconstructs scheduling from that state.
8. **Integrate and verify** evaluates task criteria and the project completion
   contract using recorded artifacts and deterministic checks where available.
9. **Complete or stop** closes the project only if all mandatory graph nodes and
   the project exit gate pass. Otherwise it exposes a bounded blocker or failed
   criterion.

Clarifying questions are allowed only when repository evidence cannot resolve a
material ambiguity or authorization boundary. They are not an additional
required V0 setup step.

## 3. Canonical truth and adapter boundary

The **Ago Work Graph is the internal coordination truth**. It owns:

- immutable project objective and versioned project completion contract;
- stable workstream/task IDs and task contracts;
- dependency edges, readiness, blockers, and terminal state;
- assignment, lease, attempt count, and retry decision;
- Context Package references;
- artifacts, verification evidence, integration state, and completion evidence.

Linear and GitHub are optional adapters, not schedulers or databases of record.
An adapter may import an external work item into a graph node and project Ago
state, comments, links, and evidence back out. Every imported object retains its
external system and ID. Adapter delivery is idempotent and may be delayed or
replayed. An adapter must not independently make a task ready, select an agent,
increment an attempt, or declare project completion. If an external state
conflicts with a newer canonical graph transition, Ago records the conflict and
does not silently overwrite internal truth.

V0 must work with both adapters disabled. Requiring a Linear workspace, GitHub
issue, pull request, or network connection fails the V0 contract.

## 4. Mandatory capability contracts

### 4.1 Context Package

Every dispatched attempt must reference an immutable Context Package containing:

- project ID, task ID, package version, repository revision, and task contract;
- relevant file/symbol references and the reason each was selected;
- applicable repository instructions and architecture constraints;
- accepted upstream decisions and artifact references;
- task inputs, allowed scope, expected output, acceptance criteria, and checks;
- explicit unknowns and exclusions.

The package is bounded: it references durable evidence rather than copying the
entire project transcript. An attempt without a persisted package is not
dispatchable. Package contents may be revised for a later attempt, but the
attempt retains the exact version it received.

### 4.2 Project completion

Project completion is a first-class contract recorded before execution. It
requires all of the following:

1. every mandatory task is accepted and every dependency is satisfied;
2. required integration has no unresolved ownership or stale-revision conflict;
3. each required check has a durable terminal result for the integrated
   revision;
4. every objective-level acceptance assertion has an evidence reference;
5. no required task is blocked, running, pending retry, or awaiting a decision.

Workers may submit results and evidence but cannot accept their own task or the
project. The board evaluates completion from canonical state. A failed required
check makes the project incomplete even if every worker reports success.

### 4.3 Automatic assignment without a model choice

V0 routes by Ago-owned capability policy. The user-facing intake schema and
normal setup UI contain no required `model`, `provider`, or `agent` field. If no
eligible local capability exists, Ago reports that blocker; it does not ask the
user to solve routine routing by choosing a model. V0 does **not** claim that
Ago has solved globally optimal routing across all models.

## 5. Frozen V0 boundaries

| Boundary | V0 rule | Observable failure if violated |
| --- | --- | --- |
| Execution | Local repository and local executor only | A run requires an orb, remote runner, or cloud workspace |
| Scheduler | Exactly one active scheduler authority per project | Two authorities can lease or transition the same graph |
| Retry | At most one automatic retry per task (two attempts total) | A third attempt is created without an explicit later-version contract |
| Retry input | Retry is based on durable failure evidence and preserves the task contract | Failure causes model fan-out or an unrecorded scope change |
| Routing | Automatic eligible-capability selection | Normal intake blocks on a model/provider picker |
| Tenancy | One local user/trust domain; no complete multi-tenant cloud | V0 depends on tenant provisioning, billing, or cross-tenant isolation |
| Adapters | Linear/GitHub are optional projections | External tracker state determines readiness or completion |
| Completion | Board-owned and evidence-backed | A worker or adapter can directly close the project |
| Recursion | Finite graph planned within configured limits | Unbounded recursive spawning is required |
| Publishing | Not part of autonomous completion | Passing V0 requires push, merge, deploy, or release |

Out of scope is a complete multi-tenant cloud, an orb/runner marketplace,
automatic publishing, and any claim that optimal routing across every model is
solved. These are not hidden acceptance dependencies.

## 6. Competitive benchmark

The benchmark compares public behavior, not internal implementation or broad
feature counts. It is a release test: Ago is differentiated only if the named
observable scenario passes.

| Reference | Role in this contract | Public baseline | V0 differentiator | Executable comparison |
| --- | --- | --- | --- | --- |
| [Factory Missions](https://docs.factory.ai/features/missions/overview) | Direct competitor | A user collaborates with Droid to build and approve features/milestones, then Mission Control orchestrates execution and QA. Factory describes this as a research preview. | Ago accepts repository + objective as sufficient normal intake, makes the durable dependency graph its canonical truth, and requires an inspectable Context Package plus project completion evidence. It does not claim broader autonomy or better model quality. | Run **CB-01** with no planning dialogue, model choice, or tracker. Inspect the graph, per-attempt package, and completion evidence. Any missing artifact or required setup prompt fails. |
| [OpenAI Symphony](https://github.com/openai/symphony/blob/main/SPEC.md) | Scheduler reference | A scheduler/runner polls a tracker, maintains one orchestrator state, creates per-issue workspaces, retries with backoff, and can recover without a persistent database. Ticket writes are generally agent behavior; a successful run may stop at human review. | Ago persists a product-owned Work Graph and completion contract independent of Linear/GitHub, and closes the project from integrated evidence rather than equating a scheduler handoff with completion. | Run **CB-02** with adapters disabled, restart the scheduler in the specified cut point, and prove canonical readiness, bounded retry, and project completion remain correct. |
| [Linear Agents](https://linear.app/docs/agents-in-linear) | UX benchmark | Agents act like workspace contributors: users delegate issues or mention agents, observe activity, and retain human responsibility as assignee. Linear issues/workspace remain the collaboration surface. | Ago matches inspectable work/activity UX while accepting one project objective and internally deriving/scheduling a dependency graph. Linear can mirror that graph but cannot own it. | Run **CB-03** with a disconnected fake Linear adapter. The local board must continue; reconnection may replay projections but cannot duplicate attempts or alter completion. |

### Competitive test fixtures

- **CB-01 — objective-only intake:** In a clean local fixture repository, submit
  only its path and `Implement the fixture feature and prove its acceptance
  checks`. Assert that planning reaches dispatch without a required prompt for
  model, agent topology, tracker, or executor. Assert that a Work Graph,
  Context Package for every attempt, and project completion evidence are
  queryable.
- **CB-02 — truth survives restart:** Execute the restart scenario in section 8
  with Linear and GitHub adapters absent. Assert the same graph/version is
  reconstructed and the final completion decision cites the integrated
  revision and check records.
- **CB-03 — adapter is not authority:** Attach a deterministic fake adapter,
  disconnect it after task A, finish local execution, then reconnect and replay
  all adapter events twice. Assert graph state and attempt counts are unchanged,
  outgoing external updates are idempotent, and the adapter receives the final
  projection only after canonical completion.

## 7. Product claims mapped to executable acceptance

The following IDs are normative test cases. Implementations may choose package
names, but CI must expose one automated test per ID and retain the listed
evidence on failure.

| ID | Product claim | Given / when / then assertion | Required evidence |
| --- | --- | --- | --- |
| PC-01 | Repository + objective are sufficient | Given a valid local fixture repo, when intake receives only those two values, then a project and graph are durably created without another required setup field. | Accepted intake command, project ID, graph version |
| PC-02 | V0 does not require model selection | Given the same request with no model/provider value, when the first task is dispatched, then policy records an eligible capability; no user-input event requests a model. | Intake schema/UI trace, assignment decision event |
| PC-03 | Work Graph is canonical truth | Given adapters disabled or replaying stale/conflicting states, when scheduling proceeds, then readiness, attempts, and completion equal graph state and adapter conflicts are recorded. | Graph event log, adapter event log, conflict record |
| PC-04 | Every attempt receives context | Given any task becomes runnable, when it is leased, then a complete immutable Context Package version was persisted first and is linked to that attempt. | Package record and sequence before lease event |
| PC-05 | Dependencies gate dispatch | Given `A -> {B,C} -> D`, then B/C never lease before A is accepted and D never leases before both B and C are accepted. | Ordered graph/lease events |
| PC-06 | One scheduler and no duplicate lease | Given concurrent start/restart pressure, when authorities contend, then only one owns the project epoch and a task has at most one active lease. | Scheduler epoch/fencing records and lease query |
| PC-07 | Automatic retry is bounded | Given B has one retryable failure, then exactly one evidence-linked retry is created; another failure leaves B failed/blocked and creates no third attempt. | Failure, retry-decision, and attempt records |
| PC-08 | Restart is durable | Given the cut point in section 8, when the scheduler restarts, then it reconstructs the same graph, does not redispatch completed work, and resumes/reconciles the persisted attempt. | Pre/post snapshots and event/attempt cardinalities |
| PC-09 | Project completion is mandatory and board-owned | Given all workers report success but a required project check fails, then the project remains incomplete; after the integrated-revision check passes, the board may complete it exactly once. | Worker reports, check records, single completion event |
| PC-10 | V0 stays local-only | Given the complete acceptance suite with network denied and no external credentials, then PC-01 through PC-09 can pass. | Network-denied test environment and suite result |

## 8. Normative restart E2E: `A -> {B,C} -> D`

### Fixture

Create a local repository whose deterministic project check passes only after D
combines artifacts from B and C. Seed this graph and freeze it as version 1:

```text
A: establish fixture baseline
├── B: produce artifact B (attempt 1 fails with a retryable fixture error)
└── C: produce artifact C
    B + C
      └── D: integrate both artifacts and run the project check
```

Each node has an explicit task contract and Context Package. A, B, C, and D are
mandatory. The completion contract requires all four accepted plus the passing
integrated-revision check.

### Procedure and assertions

1. Start one local scheduler and submit only repository + objective.
2. Assert A leases once, completes once, and is accepted before B or C leases.
3. Allow B attempt 1 to emit the fixture's retryable failure. Assert the durable
   retry decision cites that failure and creates B attempt 2, not a replacement
   task or changed task contract.
4. Allow C to complete and be accepted.
5. Pause B attempt 2 after its lease and Context Package references are durable
   but before its terminal result. Record the event cursor and graph snapshot,
   then terminate the scheduler process.
6. Restart the scheduler against the same durable store. It must acquire a new
   fenced scheduler epoch, replay from the recorded cursor, and reconcile or
   resume B attempt 2 from a safe boundary. It must not create B attempt 3,
   rerun A/C, or lease D yet.
7. Complete B attempt 2. Assert B is accepted exactly once and D becomes ready
   only now.
8. Run D once. Record its integrated repository revision and deterministic check
   result. Assert the board emits one project-completed event only after D and
   the project check are accepted.
9. Restart once more and replay duplicate scheduler/adapter delivery. Assert no
   new attempt, lease, acceptance, or project-completed event appears.

The E2E passes only with these cardinalities:

| Record | Expected count |
| --- | ---: |
| Task nodes | 4 |
| A attempts | 1 |
| B attempts | 2 |
| C attempts | 1 |
| D attempts | 1 |
| Active leases per task at any instant | 0 or 1 |
| Accepted transitions per task | 1 |
| Required integrated check terminal passes | 1 |
| Project completion transitions | 1 |

The test must also query event ordering for PC-04 and PC-05; cardinalities alone
cannot prove package-before-lease or dependency gating.

## 9. Release evidence and stopping rule

V0 evidence consists of the PC-01–PC-10 automated results, the CB-01–CB-03
comparison runs, the restart event trace, Context Package samples, graph
snapshot, adapter replay log, integrated revision, and project check record.
Screenshots may illustrate UX but cannot replace those records.

Do not describe V0 as complete while any mandatory test is missing, skipped, or
dependent on manual model selection. Do not infer competitive superiority from
this benchmark: it proves only the explicit behavioral distinctions above.

## 10. Current repository fit

The existing repository already supplies durable SQLite threads/events,
mailbox/coordinator recovery, immutable verification records, local execution,
and a process-restart E2E. See the [runtime conformance contract](./ago-runtime-conformance.md)
and [`cmd/ago/restart_e2e_test.go`](../cmd/ago/restart_e2e_test.go).

The first board-native slice now adds a versioned Work Graph protocol, bounded
DAG planner contract, SQLite board/event/lease store, atomic graph admission,
durable Goal and Plan definition reload, deterministic ready-task claim,
replaceable executor boundary, independent evidence review, and canonical board
projection. Its deterministic tests cover Chinese objective preservation,
accept/reject/execution-failure outcomes, whole-graph rollback, command
idempotency, lease contention, and runtime metadata recovery.

This is not the complete V0. Context Packages remain quarantined; bounded retry
policy, scheduler epoch fencing, in-flight attempt reconciliation, dynamic graph
mutation/history, provider routing and failover, adapter replay, authenticated
actor identities, integrated-revision checks, and project-level completion gates
remain unimplemented. Sections 1 through 9 stay open until every PC/CB test and
the normative restart E2E pass.

## 11. Authoritative public references

- Factory Missions overview: https://docs.factory.ai/features/missions/overview
- Factory Missions architecture: https://factory.ai/news/missions-architecture
- OpenAI Symphony announcement: https://openai.com/index/open-source-codex-orchestration-symphony/
- OpenAI Symphony specification: https://github.com/openai/symphony/blob/main/SPEC.md
- Linear AI Agents: https://linear.app/docs/agents-in-linear
- Linear assignment and delegation: https://linear.app/docs/assigning-issues
