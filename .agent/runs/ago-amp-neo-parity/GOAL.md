# Ago Amp Neo Parity

Goal ID: `ago-amp-neo-parity`
Started: 2026-07-18T21:57:27Z
Parent goal: none
Mode: full
Ledger path: `.agent/runs/ago-amp-neo-parity/`
Plan: `docs/ago-amp-neo-parity-plan.md`

## Objective

Build Ago as an independent local-first coding-agent product that reaches clean-room behavioral parity with rebuilt Amp/Neo across durable threads, headless local execution, desktop control, plugins, remote runners, cloud orbs, and agent-to-agent coordination, then prove stage-level automatic model routing improves outcomes without requiring users to configure models.

## Runtime Policies

- Amp research and implementation child threads use `medium` unless the user explicitly changes the policy.
- GLM is allowed in Ago's model pool when route availability, commercial terms, and evaluation evidence support it.
- Default tool execution aligns with Amp: automatic unless a configured policy intercepts it. Destructive, irreversible, publishing, purchasing, or shared-infrastructure actions still require explicit authorization.
- ClaudeX remains usable during Ago development; migrate proven assets rather than rewriting unrelated code.

## Goal Mode Coupling

`Maintain the agent-owned ledger at /Users/ruirui/orca/projects/x/.agent/runs/ago-amp-neo-parity/ and keep implementation-notes.html current at checkpoints, before compaction, and before final handoff.`

## Finishing Criteria

- [done] Phase 0 contract and existing-capability matrix are complete and verified.
- [done] Ago owns a durable headless local thread runtime with queue, steer, interrupt, cancel, automatic compaction, event replay, restart recovery, and multiple concurrent threads.
- [done] Ago exposes lifecycle-driven plugins without coupling policy to the agent loop, and synchronizes durable plugin UI state across reconnecting clients.
- [todo] Ago ships macOS/CLI clients plus web/mobile remote control that project the same thread state and support transcript, queue, diff, verification, usage, archive, search, and reconnection.
- [todo] Ago exposes custom one-shot agents and independently persistent child threads on the shared thread substrate.
- [todo] Ago supports managed cloud-orb execution through the same command/event/executor contracts before generalizing placement to named runners.
- [todo] Ago preserves original long-thread history after compaction, supports truth-preserving retrieval, explicitly targeted named runners, and scoped orb workload identity.
- [todo] Ago supports non-blocking agent-to-agent thread creation, authenticated messaging, file transfer, parent/child provenance, and bounded fan-out across local, runner, and orb executors.
- [todo] Automatic model routing, including eligible GLM routes, is promoted only after held-out evaluation proves the specified quality or cost advantage over the best single-model baseline.
- [todo] Deterministic protocol, recovery, authorization, executor-conformance, and representative end-to-end checks pass for each completed phase.
- [todo] `implementation-notes.html` remains current with decisions, tradeoffs, changed paths, validation, blockers, worker ownership, and next action.

## Protected Work

- `thread-app/src/index.ts` has pre-existing user changes.
- `thread-app/test/thread-api.test.mjs` has pre-existing user changes.
- Existing untracked output and evidence files are not part of Ago unless explicitly adopted.

## Escape Hatch

Pause, ask the user, or mark a scoped item `[blocked]` / `[incomplete]` if:

- validation contradicts the plan;
- parity requires copying proprietary Amp source or assets;
- provider or relay commercial terms prohibit a planned route;
- the next step requires modifying protected user-owned changes;
- the project requires a material scope change;
- the agent is looping without measurable progress;
- the next step risks deleting or rewriting durable memory;
- the ledger itself contaminates validation.
