/**
 * Pure timeline helpers for Decision/Full/Errors modes + execution strip.
 * Used by public/app.js and unit tests — do not reimplement in tests.
 */

const HOUSEKEEPING = new Set([
  "TaskCreate", "TaskGet", "TaskUpdate", "TaskList", "TaskOutput",
  "TodoWrite", "TodoRead", "TodoList",
]);

export function shortToolName(name) {
  return String(name || "Tool")
    .replace(/^functions\./, "")
    .replace(/^mcp__[^_]+__/, "")
    .replace(/^mcp__/, "")
    .replace(/^tool_/, "");
}

export function isFailedStatus(status) {
  return ["failed", "error", "blocked", "cancelled"].includes(String(status || "").toLowerCase());
}

export function isHousekeepingTool(name) {
  const tool = shortToolName(name);
  return HOUSEKEEPING.has(tool) || /^Task/i.test(tool) || /^Todo/i.test(tool);
}

export function isExecutionTool(name) {
  const tool = shortToolName(name);
  const lower = tool.toLowerCase();
  return (
    tool === "Agent"
    || lower.includes("start_worker")
    || lower.includes("resume_worker")
    || lower.includes("fail_worker")
  );
}

/**
 * @param {"decision"|"full"|"errors"} mode
 */
export function filterEventsByMode(events, mode = "decision") {
  const list = Array.isArray(events) ? events : [];
  if (mode === "full") return list.slice();
  if (mode === "errors") {
    return list.filter((event) => {
      if (event.type === "error") return true;
      if (isFailedStatus(event.status)) return true;
      return false;
    });
  }
  // decision: messages, workers, compact, errors, non-housekeeping tools
  // (tool results paired with calls are still dropped later by deriveTurns)
  return list.filter((event) => {
    if (event.type === "message") return true;
    if (event.type === "worker") return true;
    if (event.type === "compact") return true;
    if (event.type === "error") return true;
    if (isFailedStatus(event.status)) return true;
    if (event.type === "tool_call") {
      if (isHousekeepingTool(event.tool_name)) return false;
      return true;
    }
    if (event.type === "tool_result") return false;
    return true;
  });
}

/**
 * Summarize worker/agent executions for the strip under the topbar.
 * @returns {{ workers: number, agents: number, failed: number, totalMs: number, items: object[] }}
 */
export function summarizeExecutions(events) {
  const list = Array.isArray(events) ? events : [];
  const items = [];
  let workers = 0;
  let agents = 0;
  let failed = 0;
  let totalMs = 0;

  for (const event of list) {
    if (event.type === "worker") {
      const isAgent = String(event.role || "").toLowerCase() === "agent";
      if (isAgent) agents += 1;
      else workers += 1;
      if (isFailedStatus(event.status)) failed += 1;
      const ms = Number(event.duration_ms);
      if (Number.isFinite(ms) && ms > 0) totalMs += ms;
      items.push({
        kind: isAgent ? "agent" : "worker",
        status: event.status || "unknown",
        model: event.model || event.raw?.resolved_model || event.raw?.requested_model || "",
        effort: event.effort || event.raw?.resolved_effort || "",
        summary: event.summary || event.raw?.objective || "",
        duration_ms: Number.isFinite(ms) ? ms : null,
        event_id: event.event_id,
        failed: isFailedStatus(event.status),
      });
      continue;
    }
    if (event.type === "tool_call" && isExecutionTool(event.tool_name)) {
      // Count launch rows only when no paired worker card represents them.
      // Still surface in strip total if they look failed.
      if (isFailedStatus(event.status)) failed += 1;
    }
  }

  return { workers, agents, failed, totalMs, items };
}

export function formatCompactLabel(event, index = 0) {
  const n = index + 1;
  const mode = String(event.raw?.trigger || event.raw?.mode || event.summary || "").toLowerCase();
  let kind = "compact";
  if (mode.includes("manual") || mode.includes("user")) kind = "manual";
  else if (mode.includes("auto")) kind = "auto";
  const pre = event.raw?.pre_tokens ?? event.raw?.tokens_before;
  const post = event.raw?.post_tokens ?? event.raw?.tokens_after;
  const bits = [`Compact #${n}`];
  if (kind !== "compact") bits.push(kind);
  if (pre != null && post != null) bits.push(`${pre}→${post}`);
  return bits.join(" · ");
}

export function detectHandoffSticky(events) {
  const list = Array.isArray(events) ? events : [];
  let sticky = false;
  let handoff = false;
  for (const event of list) {
    const blob = `${event.summary || ""} ${event.content || ""}`;
    if (/CLAUDEX_ROOT_HANDOFF|HANDOFF_REQUIRED|handoff_required/i.test(blob)) handoff = true;
    if (/CLAUDEX_STICKY|STICKY|sticky re-?route/i.test(blob)) sticky = true;
  }
  return { sticky, handoff };
}

export function cacheHitRate(row) {
  const input = Number(row?.input_tokens) || 0;
  const cacheRead = Number(row?.cache_read_tokens) || 0;
  const denom = input + cacheRead;
  if (denom <= 0) return null;
  return cacheRead / denom;
}

export function honestCostLabel(row) {
  if (!row || typeof row !== "object") return "Price pending";
  if (row.cost_status === "reported" && row.cost_usd != null && Number.isFinite(Number(row.cost_usd))) {
    return `$${Number(row.cost_usd).toFixed(4)}`;
  }
  // Never show $0 for unpriced
  return "Price pending";
}
