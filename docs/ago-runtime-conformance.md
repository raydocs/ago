# Ago Runtime Conformance Contract

Version: 1
Status: Phase 0 contract

Every Ago executor and agent-kernel adapter must satisfy this contract before it
can be enabled for users. The contract is behaviorally derived from Amp's public
rebuilt-era behavior; it does not claim Amp uses these internal interfaces.

## 1. Thread durability

- Creating a thread durably records identity, agent-definition snapshot,
  project/workspace, executor intent, provenance, and initial message before
  acknowledging the command.
- A client disconnect never cancels the thread.
- Original messages, tool calls, tool results, and compaction boundaries remain
  immutable and retrievable.
- At most one turn mutates a thread transcript at a time; different threads may
  run concurrently.

## 2. Mailbox semantics

- An idle-thread append starts or schedules the next turn.
- A busy-thread normal append receives a stable message ID and preserves FIFO
  order without cancelling current work.
- A queued message can be removed by ID before dispatch.
- Steering marks an accepted message for the next documented safe boundary; it
  does not inject into an arbitrary token stream.
- Hard interrupt cancels the active turn, records the cancellation outcome, and
  submits the replacement message exactly once.
- Duplicate command delivery with the same idempotency key returns the original
  accepted result.

## 3. Events and replay

- Durable events use schema version 1, globally unique event IDs, stable thread
  IDs, and a strictly increasing per-thread sequence starting at one.
- Delivery may be at least once; consumers deduplicate by event ID.
- Reconnect from `afterSequence` reconstructs the same projected state as a
  fresh snapshot plus later events.
- User, internal, and audit visibility are separate protocol fields.

## 4. Compaction and retrieval

- Automatic compaction begins at the configured context threshold, initially
  90%.
- A compaction snapshot records objective, acceptance criteria, decisions,
  changed paths, verification, active work, unresolved issues, and next action.
- Compaction creates a new inference projection; it never deletes original
  history.
- Long-thread retrieval can search original events, inspect later revisions,
  and distinguish tool attempts from confirmed results.

## 5. Executor placement

- Executor intent is explicit: `local`, `orb`, or `runner:<id>`.
- Local, orb, and runner executors never silently substitute for one another.
- A runner ID is hostname-valid and is routing metadata, not authorization.
- An orb begins from its authorized project/base revision and does not claim to
  see uncommitted local state.
- Executor loss produces a visible offline/error transition while durable
  commands remain ordered.

### 5.1 macOS automatic-tool boundary

- Phase 1 automatic/model commands use a pipe-only trusted broker and one tiny
  per-job supervisor. The supervisor is the actual parent, owns a liveness
  capability, and terminates the command session/process group with
  TERM → bounded grace → KILL.
- The immutable launch digest binds origin, executable, argv, canonical cwd,
  exact environment values, read/write roots, synthetic HOME/TMP, Seatbelt
  profile ID/hash, network mode, TTY mode, deadline, output budget, and a
  one-use approval nonce. Changed or unsupported plans fail closed.
- macOS launch uses absolute `/usr/bin/sandbox-exec`, a deny-default
  parameterized Seatbelt profile, no direct network permission, no ambient
  environment inheritance, no startup files, and an inherited-FD allowlist.
- Restart metadata is audit-only. Ago never signals a persisted PID/PGID: PID
  reuse has no public macOS pidfd-equivalent race-free authority.
- Native Phase 1 does not claim perfect adversarial descendant containment,
  CPU/memory/PID quotas, hard-link object confinement, or a supported
  long-lived Apple sandbox API. A descendant can escape process-group cleanup
  with a new session while retaining Seatbelt restrictions. Requirements that
  need stronger containment must use a disposable VM.
- Release is conditional on a signed/notarized Gate 0 spike passing the
  supported macOS/architecture matrix, including workspace and secret denials,
  network/IPC probes, output saturation, background descendants, broker death,
  deliberate `setsid` escape, and OS-update profile drift.

## 6. Provenance and child threads

- Parent, root, source, and reply-route identifiers are immutable and authored
  by Ago, not accepted from prompt text.
- Creating a child returns its ID after durable acceptance and before inference
  finishes.
- Parent execution continues unless it explicitly waits.
- Parent cancellation does not cascade by default.
- Inter-thread messages and artifacts are authorized independently and retain
  source/destination provenance.

## 7. Tool and plugin lifecycle

- Lifecycle order is `session.start`, `agent.start`, repeated
  `tool.call`/`tool.result`, then `agent.end`.
- Tool policy can allow, reject-and-continue, modify, or synthesize without
  changing the agent kernel.
- Plugin UI requests are serializable and resolvable from any supported client.
- Background threads never depend on an unavailable foreground-only modal; they
  enter `awaiting-approval` and notify clients.
- Default policy allows normal tools automatically, matching Amp; destructive,
  irreversible, publishing, purchasing, or shared-infrastructure actions remain
  approval boundaries.
- Trusted plugins run in one supervised Bun child per workspace/runtime
  generation. This is a failure and reload boundary, not a security sandbox.
- Reload initializes and validates G+1 before atomic publication. Failed G+1
  leaves G active; successful publication cancels G invocations, resolves its
  pending UI, disposes registrations/plugins in reverse order, and terminates G.
- `ago.permission.default` is an ordinary built-in plugin, cannot be replaced,
  is registered last, and returns advisory `allow` from `tool.call`.
- Pre-tool policy errors, malformed decisions, and timeouts fail closed. Result
  observation is fail-open after logging. Semantic UI has conservative
  headless behavior: notify acknowledges, confirm is false, and input/select are
  unavailable.

## 8. Cancellation and recovery

- Tool requests are persisted before dispatch and results after completion.
- An unknown side-effecting tool outcome is not blindly replayed.
- Restart resumes from the last committed safe boundary.
- Turn cancellation is distinct from thread archive and executor destruction.
- Process death, duplicate delivery, stale leases, and reconnect are covered by
  deterministic tests.

## 9. Artifacts and diffs

- Artifacts are content-addressed or checksum-verified and tenant/thread scoped.
- File transfer uses workspace-relative normalized paths, rejects traversal and
  symlink escape, and defaults to no overwrite.
- Initial cross-thread transfer parity limit is 4 MiB unless later evidence
  deliberately changes it.
- Diff review is bound to a thread and executor revision; comments/staging detect
  or reconcile underlying working-tree changes.

## 10. Required proof

An adapter is conformant only when it passes:

1. state and protocol unit tests;
2. queue/steer/interrupt race tests;
3. event replay and duplicate-delivery tests;
4. process-death and restart tests;
5. provenance and authorization tests;
6. executor-specific lifecycle tests;
7. one representative code-change task with deterministic verification.
