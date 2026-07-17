import assert from "node:assert/strict";
import test from "node:test";
import {
  filterEventsByMode,
  summarizeExecutions,
  formatCompactLabel,
  detectHandoffSticky,
  collectObservedModels,
  underAttributedModels,
  cacheHitRate,
  honestCostLabel,
  isHousekeepingTool,
  isExecutionTool,
  buildHumanTurns,
  stableTurnAnchor,
  turnDeepLinkHash,
} from "../public/timeline-model.mjs";

test("isHousekeepingTool folds Task* and Todo*", () => {
  assert.equal(isHousekeepingTool("TaskCreate"), true);
  assert.equal(isHousekeepingTool("mcp__x__TodoWrite"), true);
  assert.equal(isHousekeepingTool("Bash"), false);
  assert.equal(isExecutionTool("mcp__claudex-flow__start_worker"), true);
  assert.equal(isExecutionTool("Agent"), true);
});

test("filterEventsByMode decision drops housekeeping tools but keeps results for visible calls", () => {
  const events = [
    { type: "message", role: "user" },
    { event_id: "task-call", type: "tool_call", tool_name: "TaskCreate" },
    { event_id: "task-result", parent_event_id: "task-call", type: "tool_result" },
    { event_id: "bash-call", type: "tool_call", tool_name: "Bash" },
    { event_id: "bash-result", parent_event_id: "bash-call", type: "tool_result", content: "ok" },
    { type: "worker", status: "failed" },
    { type: "compact" },
  ];
  const decision = filterEventsByMode(events, "decision");
  assert.equal(decision.some((e) => e.tool_name === "TaskCreate"), false);
  assert.equal(decision.some((e) => e.event_id === "task-result"), false);
  assert.equal(decision.some((e) => e.tool_name === "Bash"), true);
  assert.equal(decision.some((e) => e.event_id === "bash-result"), true);
  assert.equal(decision.some((e) => e.type === "worker"), true);
  assert.equal(filterEventsByMode(events, "full").length, 7);
  assert.equal(filterEventsByMode(events, "errors").length, 1);

  const [turn] = buildHumanTurns(decision);
  assert.equal(turn.activity[0].event.event_id, "bash-call");
  assert.equal(turn.activity[0].result.event_id, "bash-result");
});

test("summarizeExecutions counts agents workers failed and duration", () => {
  const summary = summarizeExecutions([
    { type: "worker", role: "agent", status: "completed", duration_ms: 1000 },
    { type: "worker", role: "worker", status: "failed", duration_ms: 500 },
    { type: "worker", role: "agent", status: "failed", duration_ms: 200 },
    { type: "message", role: "user" },
  ]);
  assert.equal(summary.agents, 2);
  assert.equal(summary.workers, 1);
  assert.equal(summary.failed, 2);
  assert.equal(summary.totalMs, 1700);
  assert.equal(summary.items.length, 3);
});

test("formatCompactLabel and handoff detection", () => {
  assert.match(formatCompactLabel({ summary: "auto compact" }, 0), /Compact #1/);
  const flags = detectHandoffSticky([
    { summary: "CLAUDEX_ROOT_HANDOFF capsule ready" },
    { content: "sticky re-route ack required" },
  ]);
  assert.equal(flags.handoff, true);
  assert.equal(flags.sticky, true);
});

test("detectHandoffSticky ignores CSS position sticky and casual handoff docs", () => {
  const flags = detectHandoffSticky([
    { type: "tool_result", content: ".topbar { position: sticky; z-index: 20; }" },
    { type: "message", summary: "read claudex-thread-app-handoff.md" },
    { type: "worker", summary: "Map handoff implementation drift" },
  ]);
  assert.equal(flags.sticky, false);
  assert.equal(flags.handoff, false);
  assert.equal(flags.hasGate, false);
});

test("detectHandoffSticky gate events degrade-friendly", () => {
  const empty = detectHandoffSticky([]);
  assert.equal(empty.hasGate, false);
  assert.equal(empty.sticky, false);
  const gated = detectHandoffSticky([
    { type: "gate", status: "active", summary: "gate:open" },
    { type: "gate", status: "cleared", summary: "gate:cleared" },
  ]);
  assert.equal(gated.hasGate, true);
  assert.equal(gated.gateOpen, true);
  assert.equal(gated.gateClose, true);
  // Decision mode keeps gate rows
  const decision = filterEventsByMode([{ type: "gate", status: "open" }, { type: "tool_call", tool_name: "TaskCreate" }], "decision");
  assert.equal(decision.some((e) => e.type === "gate"), true);
  assert.equal(decision.some((e) => e.tool_name === "TaskCreate"), false);
});

test("collectObservedModels and underAttributedModels stay honest", () => {
  const observed = collectObservedModels(
    [
      { role: "supervisor", model: "gpt-5.6-sol" },
      { role: "agent", model: "claude-sonnet-5" },
      { role: "assistant", model: "<synthetic>" },
    ],
    [
      { type: "worker", role: "agent", model: "claude-opus-4-8[1m]" },
      { type: "message", model: "ignored" },
    ],
  );
  assert.deepEqual(observed.map((r) => r.model).sort(), [
    "claude-opus-4-8[1m]",
    "claude-sonnet-5",
    "gpt-5.6-sol",
  ]);
  const missing = underAttributedModels(observed, [{ model: "gpt-5.6-sol" }]);
  assert.ok(missing.includes("claude-sonnet-5"));
  assert.ok(missing.includes("claude-opus-4-8[1m]"));
  assert.equal(missing.includes("gpt-5.6-sol"), false);
});

test("honestCostLabel never fakes zero dollars", () => {
  assert.equal(honestCostLabel({ cost_status: "unpriced", cost_usd: 0 }), "Price pending");
  assert.equal(honestCostLabel({ cost_status: "reported", cost_usd: 1.25 }), "$1.2500");
  assert.equal(cacheHitRate({ input_tokens: 25, cache_read_tokens: 75 }), 0.75);
  assert.equal(cacheHitRate({ input_tokens: 0, cache_read_tokens: 0 }), null);
});

test("buildHumanTurns groups adjacent work with assistant output and derives honest turn metadata", () => {
  const turns = buildHumanTurns([
    { event_id: "user/1", type: "message", role: "user", started_at: "2026-07-17T10:00:00Z", content: "Please inspect" },
    { event_id: "note-1", type: "message", role: "assistant", started_at: "2026-07-17T10:00:01Z", content: "I will inspect." },
    { event_id: "call-1", type: "tool_call", tool_name: "Bash", status: "completed", started_at: "2026-07-17T10:00:02Z", duration_ms: 1000 },
    { event_id: "result-1", parent_event_id: "call-1", type: "tool_result", started_at: "2026-07-17T10:00:03Z" },
    { event_id: "worker-1", type: "worker", role: "worker", status: "failed", started_at: "2026-07-17T10:00:04Z" },
    { event_id: "answer-1", type: "message", role: "assistant", status: "completed", started_at: "2026-07-17T10:00:05Z", ended_at: "2026-07-17T10:00:06Z", content: "Result" },
  ]);

  assert.equal(turns.length, 1);
  assert.equal(turns[0].activity.length, 2);
  assert.equal(turns[0].activity[0].result.event_id, "result-1");
  assert.deepEqual(turns[0].processNotes.map((event) => event.event_id), ["note-1"]);
  assert.deepEqual(turns[0].finalAnswers.map((event) => event.event_id), ["answer-1"]);
  assert.equal(turns[0].durationMs, 6000);
  assert.equal(turns[0].status, "failed");
});

test("buildHumanTurns does not move pre-work assistant narration after the work", () => {
  const [turn] = buildHumanTurns([
    { event_id: "user-1", type: "message", role: "user", content: "Inspect" },
    { event_id: "note-1", type: "message", role: "assistant", content: "I will inspect." },
    { event_id: "call-1", type: "tool_call", tool_name: "Read" },
  ]);

  assert.deepEqual(turn.processNotes.map((event) => event.event_id), ["note-1"]);
  assert.deepEqual(turn.finalAnswers, []);
  assert.equal(turn.activity[0].event.event_id, "call-1");
});

test("turn anchors and deep links are deterministic and URL-safe", () => {
  const turn = { id: "user/1:prompt" };
  assert.equal(stableTurnAnchor(turn, 0), "turn-user-1-prompt");
  assert.equal(stableTurnAnchor({}, 2), "turn-3");
  assert.equal(
    turnDeepLinkHash("session/id", turn, 0),
    "#/thread/session%2Fid/turn/turn-user-1-prompt",
  );
});

test("buildHumanTurns leaves duration and status absent when source data is absent", () => {
  const [turn] = buildHumanTurns([{ type: "message", role: "assistant", content: "Only text" }]);
  assert.equal(turn.durationMs, null);
  assert.equal(turn.status, "");
  assert.equal(stableTurnAnchor(turn, 0), "turn-1");
});
