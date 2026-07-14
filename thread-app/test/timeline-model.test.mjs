import assert from "node:assert/strict";
import test from "node:test";
import {
  filterEventsByMode,
  summarizeExecutions,
  formatCompactLabel,
  detectHandoffSticky,
  cacheHitRate,
  honestCostLabel,
  isHousekeepingTool,
  isExecutionTool,
} from "../public/timeline-model.mjs";

test("isHousekeepingTool folds Task* and Todo*", () => {
  assert.equal(isHousekeepingTool("TaskCreate"), true);
  assert.equal(isHousekeepingTool("mcp__x__TodoWrite"), true);
  assert.equal(isHousekeepingTool("Bash"), false);
  assert.equal(isExecutionTool("mcp__claudex-flow__start_worker"), true);
  assert.equal(isExecutionTool("Agent"), true);
});

test("filterEventsByMode decision drops housekeeping tools", () => {
  const events = [
    { type: "message", role: "user" },
    { type: "tool_call", tool_name: "TaskCreate" },
    { type: "tool_call", tool_name: "Bash" },
    { type: "worker", status: "failed" },
    { type: "compact" },
  ];
  const decision = filterEventsByMode(events, "decision");
  assert.equal(decision.some((e) => e.tool_name === "TaskCreate"), false);
  assert.equal(decision.some((e) => e.tool_name === "Bash"), true);
  assert.equal(decision.some((e) => e.type === "worker"), true);
  assert.equal(filterEventsByMode(events, "full").length, 5);
  assert.equal(filterEventsByMode(events, "errors").length, 1);
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
    { content: "STICKY re-route ack required" },
  ]);
  assert.equal(flags.handoff, true);
  assert.equal(flags.sticky, true);
});

test("honestCostLabel never fakes zero dollars", () => {
  assert.equal(honestCostLabel({ cost_status: "unpriced", cost_usd: 0 }), "Price pending");
  assert.equal(honestCostLabel({ cost_status: "reported", cost_usd: 1.25 }), "$1.2500");
  assert.equal(cacheHitRate({ input_tokens: 25, cache_read_tokens: 75 }), 0.75);
  assert.equal(cacheHitRate({ input_tokens: 0, cache_read_tokens: 0 }), null);
});
