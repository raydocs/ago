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

function validTime(value) {
  const time = Date.parse(value);
  return Number.isFinite(time) ? time : null;
}

function turnStatus(events) {
  const statuses = events
    .map((event) => String(event?.status || "").trim().toLowerCase())
    .filter(Boolean);
  for (const status of ["failed", "error", "blocked", "cancelled", "running", "active", "completed", "closed"]) {
    if (statuses.includes(status)) return status;
  }
  return statuses.at(-1) || "";
}

export function stableTurnAnchor(turn, index = 0) {
  const fallback = `turn-${index + 1}`;
  const raw = String(turn?.id || fallback).trim() || fallback;
  const safe = raw.replace(/[^a-zA-Z0-9_-]/g, "-");
  return safe.startsWith("turn-") ? safe : `turn-${safe}`;
}

export function turnDeepLinkHash(sessionID, turn, index = 0) {
  return `#/thread/${encodeURIComponent(String(sessionID || ""))}/turn/${encodeURIComponent(stableTurnAnchor(turn, index))}`;
}

/**
 * Build deterministic Human View turns from already sorted, mode-filtered events.
 * Tool results represented by their call, and worker calls represented by worker cards,
 * are folded into the adjacent assistant work rather than duplicated.
 */
export function buildHumanTurns(events) {
  const list = Array.isArray(events) ? events : [];
  const orderByEvent = new WeakMap();
  list.forEach((event, index) => {
    if (event && typeof event === "object") orderByEvent.set(event, index);
  });
  const resultsByCall = new Map();
  const callIDs = new Set(list
    .filter((event) => event.type === "tool_call")
    .map((event) => event.event_id || event.tool_use_id)
    .filter(Boolean));
  const represented = new Set();

  for (const event of list) {
    if (event.type === "worker") {
      if (event.raw?.call_event_id) represented.add(event.raw.call_event_id);
      if (event.raw?.result_event_id) represented.add(event.raw.result_event_id);
    }
    if (event.type === "tool_result") {
      const parent = event.parent_event_id || event.tool_use_id;
      if (parent && !resultsByCall.has(parent)) resultsByCall.set(parent, event);
    }
  }

  const turns = [];
  let current = null;
  const createTurn = (userEvent, seedEvent) => ({
    id: userEvent?.event_id || seedEvent?.event_id || `turn-${turns.length + 1}`,
    user: userEvent || null,
    assistantMessages: [],
    activity: [],
    processNotes: [],
    finalAnswers: [],
    startedAt: userEvent?.started_at || seedEvent?.started_at || null,
    endedAt: null,
    durationMs: null,
    status: "",
  });
  const finalize = () => {
    if (!current) return;
    const boundaries = [];
    const sourceEvents = [];
    const collect = (event) => {
      if (!event) return;
      sourceEvents.push(event);
      const start = validTime(event.started_at);
      const end = validTime(event.ended_at);
      if (start != null) boundaries.push(start);
      if (end != null) boundaries.push(end);
      const duration = Number(event.duration_ms);
      if (start != null && Number.isFinite(duration) && duration > 0) boundaries.push(start + duration);
    };
    collect(current.user);
    for (const message of current.assistantMessages) collect(message);
    for (const item of current.activity) {
      collect(item.event);
      collect(item.result);
    }
    if (boundaries.length) {
      const start = Math.min(...boundaries);
      const end = Math.max(...boundaries);
      current.startedAt = current.startedAt || new Date(start).toISOString();
      current.endedAt = new Date(end).toISOString();
      if (end > start) current.durationMs = end - start;
    }
    current.status = turnStatus(sourceEvents);

    if (!current.assistantMessages.length) {
      turns.push(current);
      current = null;
      return;
    }
    if (!current.activity.length) {
      current.finalAnswers = current.assistantMessages.slice();
    } else {
      const lastAssistant = current.assistantMessages.at(-1);
      const lastAssistantOrder = orderByEvent.get(lastAssistant) ?? -1;
      const lastActivityOrder = current.activity.reduce(
        (latest, item) => Math.max(latest, orderByEvent.get(item.event) ?? -1),
        -1,
      );
      if (lastAssistantOrder > lastActivityOrder) {
        current.finalAnswers = [lastAssistant];
        current.processNotes = current.assistantMessages.slice(0, -1);
      } else {
        current.processNotes = current.assistantMessages.slice();
      }
    }
    turns.push(current);
    current = null;
  };

  for (const event of list) {
    if (represented.has(event.event_id)) continue;
    if (event.type === "tool_result" && callIDs.has(event.parent_event_id || event.tool_use_id)) continue;
    if (event.type === "message" && event.role === "user") {
      finalize();
      current = createTurn(event, event);
      continue;
    }
    if (!current) current = createTurn(null, event);
    if (event.type === "message" && event.role === "assistant") {
      current.assistantMessages.push(event);
    } else if (event.type === "tool_call") {
      current.activity.push({ kind: "tool", event, result: resultsByCall.get(event.event_id || event.tool_use_id) });
    } else if (event.type === "worker") {
      current.activity.push({ kind: "execution", event });
    } else if (event.type === "message") {
      current.activity.push({ kind: "message", event });
    } else {
      current.activity.push({ kind: "system", event });
    }
  }
  finalize();
  return turns;
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
  // decision: messages, workers, compact, errors, gate/workflow, non-housekeeping tools.
  // Keep results paired with visible calls so expanded Show Work retains output/error detail;
  // buildHumanTurns folds those result rows into the call instead of duplicating them.
  const visibleCallIDs = new Set();
  for (const event of list) {
    if (event.type !== "tool_call") continue;
    if (!isFailedStatus(event.status) && isHousekeepingTool(event.tool_name)) continue;
    const callID = event.event_id || event.tool_use_id;
    if (callID) visibleCallIDs.add(callID);
  }
  return list.filter((event) => {
    if (event.type === "message") return true;
    if (event.type === "worker") return true;
    if (event.type === "compact") return true;
    if (event.type === "error") return true;
    if (event.type === "gate" || event.type === "workflow") return true;
    if (isFailedStatus(event.status)) return true;
    if (event.type === "tool_call") return !isHousekeepingTool(event.tool_name);
    if (event.type === "tool_result") {
      const parent = event.parent_event_id || event.tool_use_id;
      return Boolean(parent && visibleCallIDs.has(parent));
    }
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

/**
 * Infer sticky / handoff / gate flags from events.
 * Degrades to all-false when nothing matches — never throws.
 * Avoid matching CSS `position: sticky` or casual "handoff" docs.
 */
export function detectHandoffSticky(events) {
  const list = Array.isArray(events) ? events : [];
  let sticky = false;
  let handoff = false;
  let gateOpen = false;
  let gateClose = false;
  let gateEvents = 0;

  for (const event of list) {
    try {
      const type = String(event?.type || "").toLowerCase();
      if (type === "gate" || type === "workflow") {
        gateEvents += 1;
        const status = String(event?.status || "").toLowerCase();
        if (status === "active" || status === "open" || status === "required") gateOpen = true;
        if (status === "cleared" || status === "close" || status === "closed") gateClose = true;
      }

      const blob = [
        event?.summary,
        event?.content,
        event?.raw?.class,
        event?.raw?.kind,
        event?.status,
      ].filter(Boolean).join(" ");
      if (!blob) continue;

      // Explicit runtime markers only — not docs titled *handoff*.md
      if (/CLAUDEX_ROOT_HANDOFF|\bHANDOFF_REQUIRED\b|\bhandoff_required\b/i.test(blob)) {
        handoff = true;
      }
      // Avoid CSS `position: sticky` (common in tool_result dumps)
      if (
        /CLAUDEX_STICKY/i.test(blob)
        || /\bsticky[_ -]?re-?route\b/i.test(blob)
        || /\bsticky\b[^\n]{0,48}\back(?:nowledg(?:e|ment))? required\b/i.test(blob)
      ) {
        sticky = true;
      }
      if (/\bgate:(open|close|active|cleared)\b/i.test(blob) || /CLAUDEX_GATE_/i.test(blob)) {
        gateEvents += 1;
        if (/gate:open|gate:active|CLAUDEX_GATE_OPEN|required/i.test(blob)) gateOpen = true;
        if (/gate:close|gate:cleared|CLAUDEX_GATE_CLEAR/i.test(blob)) gateClose = true;
      }
    } catch {
      // degrade-friendly: skip malformed events
    }
  }

  return {
    sticky,
    handoff,
    gateOpen,
    gateClose,
    hasGate: gateEvents > 0,
    gateEvents,
  };
}

/**
 * Models observed on participants / worker cards (not usage_records).
 * Used to surface under-attribution honestly without inventing tokens.
 */
export function collectObservedModels(participants = [], events = []) {
  const map = new Map();
  const upsert = (model, role, source) => {
    const m = String(model || "").trim();
    if (!m || m === "<synthetic>") return;
    let row = map.get(m);
    if (!row) {
      row = { model: m, roles: new Set(), sources: new Set() };
      map.set(m, row);
    }
    if (role) row.roles.add(String(role));
    if (source) row.sources.add(source);
  };

  for (const p of Array.isArray(participants) ? participants : []) {
    upsert(p?.model, p?.role, "participant");
  }
  for (const event of Array.isArray(events) ? events : []) {
    if (event?.type !== "worker") continue;
    const model = event.model || event.raw?.resolved_model || event.raw?.requested_model;
    upsert(model, event.role || "worker", "execution");
  }

  return [...map.values()]
    .map((row) => ({
      model: row.model,
      roles: [...row.roles].sort(),
      sources: [...row.sources].sort(),
    }))
    .sort((a, b) => a.model.localeCompare(b.model));
}

/**
 * Models present in observed executors but missing from usage.models.
 */
export function underAttributedModels(observed, usageModels = []) {
  const billed = new Set(
    (Array.isArray(usageModels) ? usageModels : [])
      .map((row) => String(row?.model || row || "").trim())
      .filter(Boolean),
  );
  return (Array.isArray(observed) ? observed : [])
    .map((row) => row.model)
    .filter((model) => model && !billed.has(model));
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
