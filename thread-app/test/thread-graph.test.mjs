import assert from "node:assert/strict";
import test from "node:test";

import { assembleThreadGraph, normalizeGraphEvents, renderThreadMarkdown } from "../src/thread-graph.ts";

test("normalizeGraphEvents validates session ownership and redacts secrets", () => {
  const { records, error } = normalizeGraphEvents([
    {
      event_id: "call-1",
      session_id: "root-session",
      root_session_id: "root-session",
      type: "tool_call",
      role: "assistant",
      model: "gpt-5.6-sol",
      effort: "xhigh",
      status: "running",
      started_at: "2026-07-13T20:00:00Z",
      summary: "Bash request",
      content: "Authorization: Bearer secret-token-value",
      tool_name: "Bash",
      tool_use_id: "call-1",
      raw: {
        input: {
          command: "curl -H 'Authorization: Bearer secret-token-value' https://example.test",
          password: "password-value",
          safe: "visible",
        },
      },
    },
  ], "root-session");

  assert.equal(error, "");
  assert.equal(records.length, 1);
  const encoded = JSON.stringify(records);
  assert.doesNotMatch(encoded, /secret-token-value|password-value/);
  assert.match(encoded, /\[REDACTED\]/);
  assert.match(encoded, /visible/);

  const wrongSession = normalizeGraphEvents([{ ...records[0], session_id: "other-session" }], "root-session");
  assert.match(wrongSession.error, /does not belong/);
});

test("normalizeGraphEvents redacts password form values without hiding ordinary input", () => {
  const { records, error } = normalizeGraphEvents([
    {
      event_id: "result-browser",
      session_id: "root-session",
      root_session_id: "root-session",
      type: "tool_result",
      role: "system",
      status: "completed",
      started_at: "2026-07-13T20:00:00Z",
      content: "await page.getByRole('textbox', { name: '访问密码' }).fill('local-view');",
      raw: {
        input: {
          fields: [
            { name: "Password", value: "raw-secret-value" },
            { name: "Search", value: "thread query" },
          ],
        },
      },
    },
  ], "root-session");

  assert.equal(error, "");
  const encoded = JSON.stringify(records);
  assert.doesNotMatch(encoded, /local-view|raw-secret-value/);
  assert.match(encoded, /\[REDACTED\]/);
  assert.match(encoded, /thread query/);
});

test("normalizeGraphEvents redacts quoted sensitive keys inside tool output strings", () => {
  const { records, error } = normalizeGraphEvents([
    {
      event_id: "result-read",
      session_id: "root-session",
      root_session_id: "root-session",
      type: "tool_result",
      role: "system",
      status: "completed",
      started_at: "2026-07-13T20:00:00Z",
      content: "{\"ingest_token\": \"local-ingest\", \"machine_id\": \"local-backfill\"}",
    },
  ], "root-session");

  assert.equal(error, "");
  const encoded = JSON.stringify(records);
  assert.doesNotMatch(encoded, /local-ingest/);
  assert.match(encoded, /\[REDACTED\]/);
  assert.match(encoded, /local-backfill/);
});

test("normalizeGraphEvents removes internal Agent continuation metadata", () => {
  const { records, error } = normalizeGraphEvents([
    {
      event_id: "result-agent",
      session_id: "root-session",
      root_session_id: "root-session",
      type: "tool_result",
      role: "system",
      status: "completed",
      started_at: "2026-07-13T20:00:00Z",
      content: "This tool result is internal metadata — never quote or paste it. agentId: internal-agent-id output_file: /tmp/internal.output",
      raw: {
        result: "Codex Task started in the background. agentId: internal-codex-id (use SendMessage with to: 'internal-codex-id' to continue) <usage>subagent_tokens: 12</usage>",
      },
    },
  ], "root-session");

  assert.equal(error, "");
  const encoded = JSON.stringify(records);
  assert.doesNotMatch(encoded, /internal-agent-id|internal-codex-id|SendMessage|internal\.output/);
  assert.match(encoded, /\[INTERNAL METADATA REDACTED\]/);
  assert.match(encoded, /Codex Task started/);
  assert.match(encoded, /subagent_tokens: 12/);
});

test("assembleThreadGraph derives actual worker invocations in deterministic order", () => {
  const graph = assembleThreadGraph({
    exportedAt: "2026-07-13T21:00:00.000Z",
    thread: {
      session_id: "root-session",
      root_session_id: "root-session",
      parent_session_id: null,
      title: "Build the archive",
      model: "gpt-5.6-sol",
      effort: "xhigh",
      role: "supervisor",
      state: "active",
      started_at: "2026-07-13T20:00:00.000Z",
      updated_at: "2026-07-13T20:02:00.000Z",
    },
    threads: [],
    events: [
      {
        event_id: "result-worker",
        session_id: "root-session",
        root_session_id: "root-session",
        parent_event_id: "call-worker",
        worker_id: "worker-1",
        type: "tool_result",
        role: "system",
        model: "grok-4.5-build",
        effort: "high",
        status: "failed",
        started_at: "2026-07-13T20:00:03.000Z",
        duration_ms: 102553,
        summary: "Reached max turns",
        content: "Reached max turns",
        tool_use_id: "call-worker",
        raw: { result: { status: "failed", session_id: "child-session" } },
      },
      {
        event_id: "message-user",
        session_id: "root-session",
        root_session_id: "root-session",
        type: "message",
        role: "user",
        status: "completed",
        started_at: "2026-07-13T20:00:00.000Z",
        summary: "Please build the archive",
        content: "Please build the archive",
        raw: {},
      },
      {
        event_id: "call-worker",
        session_id: "root-session",
        root_session_id: "root-session",
        type: "tool_call",
        role: "assistant",
        model: "gpt-5.6-sol",
        effort: "xhigh",
        status: "running",
        started_at: "2026-07-13T20:00:02.000Z",
        summary: "start_worker · Build usage foundation",
        tool_name: "mcp__claudex-flow__start_worker",
        tool_use_id: "call-worker",
        raw: { input: { objective: "Build usage foundation", model: "grok-4.5", effort: "high" } },
      },
    ],
    usage: {
      totals: {
        requests: 10,
        input_tokens: 75002,
        cache_write_5m_tokens: 0,
        cache_write_1h_tokens: 0,
        cache_read_tokens: 306048,
        output_tokens: 13632,
        total_tokens: 394682,
        reported_cost_usd: null,
        cost_status: "unpriced",
      },
      models: [],
      sessions: [],
      roles: [],
    },
  });

  assert.equal(graph.schema_version, "claudex-thread.v1");
  assert.equal(graph.events[0].event_id, "message-user");
  const worker = graph.events.find((event) => event.type === "worker");
  assert.ok(worker);
  assert.equal(worker.worker_id, "worker-1");
  assert.equal(worker.model, "grok-4.5-build");
  assert.equal(worker.status, "failed");
  assert.equal(worker.duration_ms, 102553);
  assert.equal(worker.raw.requested_model, "grok-4.5");
  assert.equal(worker.raw.resolved_model, "grok-4.5-build");
  assert.ok(graph.relationships.some((relationship) => relationship.type === "worker_session" && relationship.to === "child-session"));
  assert.ok(graph.participants.some((participant) => participant.model === "gpt-5.6-sol"));
  assert.ok(graph.participants.some((participant) => participant.model === "grok-4.5-build"));
  assert.equal(graph.usage.totals.reported_cost_usd, null);
  assert.equal(graph.usage.totals.cost_status, "unpriced");
  assert.equal(graph.usage.totals.worker_compute_duration_ms, 102553);
  assert.deepEqual(graph.redaction, { applied: true, policy_version: "claudex-redaction.v1" });
});

test("assembleThreadGraph derives native Agent executions without inventing child sessions", () => {
  const graph = assembleThreadGraph({
    exportedAt: "2026-07-13T21:00:00.000Z",
    thread: { session_id: "root-session", root_session_id: "root-session", role: "supervisor", model: "gpt-5.6-sol" },
    events: [
      {
        event_id: "call-agent", session_id: "root-session", root_session_id: "root-session",
        type: "tool_call", role: "assistant", model: "gpt-5.6-sol", status: "running",
        started_at: "2026-07-13T20:00:00.000Z", tool_name: "Agent", tool_use_id: "call-agent",
        summary: "Explore the repository", raw: { input: { subagent_type: "Explore", model: "opus", description: "Map the code" } },
      },
      {
        event_id: "result-agent", session_id: "root-session", root_session_id: "root-session",
        parent_event_id: "call-agent", worker_id: "agent-1", type: "tool_result", role: "system",
        model: "claude-opus-4-8[1m]", effort: "high", status: "completed",
        started_at: "2026-07-13T20:00:01.000Z", duration_ms: 2500, tool_use_id: "call-agent",
        summary: "Repository mapped", raw: { result: { agentId: "agent-1", resolvedModel: "claude-opus-4-8[1m]" } },
      },
    ],
    usage: { totals: { reported_cost_usd: null, cost_status: "unpriced" } },
  });

  const execution = graph.events.find((event) => event.type === "worker");
  assert.ok(execution);
  assert.equal(execution.role, "agent");
  assert.equal(execution.model, "claude-opus-4-8[1m]");
  assert.equal(execution.raw.requested_model, "opus");
  assert.equal(execution.raw.resolved_model, "claude-opus-4-8[1m]");
  assert.equal(execution.raw.agent_type, "Explore");
  const markdown = renderThreadMarkdown(graph);
  assert.match(markdown, /### Agent · Explore · claude-opus-4-8\[1m\]/);
  assert.doesNotMatch(markdown, /worker: agent-1/);
  assert.ok(graph.participants.some((participant) => participant.role === "agent" && participant.model === "claude-opus-4-8[1m]"));
  assert.ok(!graph.relationships.some((relationship) => relationship.type === "worker_session"));
});

test("assembleThreadGraph derives modified-file artifacts from canonical tool calls", () => {
  const graph = assembleThreadGraph({
    thread: { session_id: "root-session", root_session_id: "root-session", role: "supervisor" },
    events: [
      {
        event_id: "call-write", session_id: "root-session", root_session_id: "root-session",
        type: "tool_call", role: "assistant", status: "running", started_at: "2026-07-13T20:00:00.000Z",
        tool_name: "Write", tool_use_id: "call-write", summary: "Write report",
        raw: { input: { file_path: "/workspace/report.md", content: "password=must-not-survive" } },
      },
    ],
    usage: { totals: { reported_cost_usd: null, cost_status: "unpriced" } },
  });

  assert.ok(graph.artifacts.some((artifact) => artifact.path === "/workspace/report.md" && artifact.action === "write"));
  assert.doesNotMatch(JSON.stringify(graph.artifacts), /must-not-survive/);
});

test("renderThreadMarkdown uses the canonical graph rather than a database dump", () => {
  const graph = assembleThreadGraph({
    exportedAt: "2026-07-13T21:00:00.000Z",
    thread: {
      session_id: "root-session",
      root_session_id: "root-session",
      parent_session_id: null,
      title: "Human readable thread",
      model: "gpt-5.6-sol",
      effort: "xhigh",
      role: "supervisor",
      state: "active",
      started_at: "2026-07-13T20:00:00.000Z",
      updated_at: "2026-07-13T20:02:00.000Z",
    },
    threads: [],
    events: [
      { event_id: "u1", session_id: "root-session", root_session_id: "root-session", type: "message", role: "user", status: "completed", started_at: "2026-07-13T20:00:00.000Z", summary: "Question", content: "Can you finish the implementation?", raw: {} },
      { event_id: "a1", session_id: "root-session", root_session_id: "root-session", type: "message", role: "assistant", model: "gpt-5.6-sol", effort: "xhigh", status: "completed", started_at: "2026-07-13T20:00:01.000Z", summary: "Answer", content: "The implementation is complete.", raw: {} },
      { event_id: "c1", session_id: "root-session", root_session_id: "root-session", type: "compact", role: "system", status: "completed", started_at: "2026-07-13T20:00:02.000Z", summary: "Conversation compacted · manual", content: "", raw: { trigger: "manual" } },
    ],
    usage: { totals: { requests: 1, total_tokens: 39, reported_cost_usd: null, cost_status: "unpriced" }, models: [], sessions: [], roles: [] },
  });

  assert.equal(graph.usage.totals.active_duration_ms, 1000);
  const markdown = renderThreadMarkdown(graph);
  assert.match(markdown, /schema_version: claudex-thread\.v1/);
  assert.match(markdown, /root_session_id: "root-session"/);
  assert.match(markdown, /# Human readable thread/);
  assert.match(markdown, /### User[\s\S]*Can you finish the implementation\?/);
  assert.match(markdown, /### Assistant · gpt-5\.6-sol[\s\S]*The implementation is complete\./);
  assert.match(markdown, /Conversation compacted · manual/);
  assert.match(markdown, /## Usage[\s\S]*Active duration: 1000 ms[\s\S]*Price status: unpriced/);
  assert.doesNotMatch(markdown, /"event_id"|payload_json/);
});
