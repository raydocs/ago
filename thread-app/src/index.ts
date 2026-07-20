import { assembleThreadGraph, normalizeGraphEvents, renderThreadMarkdown } from "./thread-graph";

interface Env {
  DB: D1Database;
  ASSETS: Fetcher;
  INGEST_TOKEN: string;
}

type AnyRecord = Record<string, unknown>;

type NormalizedUsageRecord = {
  usageID: string;
  messageID: string;
  requestID: string;
  observedAt: string;
  model: string;
  inputTokens: number;
  cacheWrite5mTokens: number;
  cacheWrite1hTokens: number;
  cacheReadTokens: number;
  outputTokens: number;
  isFast: boolean;
  carriedCostUSD: number | null;
};

const encoder = new TextEncoder();
const MAX_BODY_BYTES = 256 * 1024;

export default {
  async fetch(request: Request, env: Env): Promise<Response> {
    const url = new URL(request.url);

    if (url.pathname === "/api/health") {
      return json({ ok: true, service: "claudex-threads", now: new Date().toISOString() });
    }
    if (url.pathname === "/api/hooks/claude" && request.method === "POST") {
      return ingestHook(request, env);
    }
    // Read APIs are intentionally public. Only the ingest route requires INGEST_TOKEN.
    if (url.pathname.startsWith("/api/")) {
      if (url.pathname === "/api/stats" && request.method === "GET") {
        return getStats(env);
      }
      if (url.pathname === "/api/threads" && request.method === "GET") {
        return listThreads(url, env);
      }
      const usageMatch = url.pathname.match(/^\/api\/threads\/([^/]+)\/usage(?:\/(export))?$/);
      if (usageMatch && request.method === "GET") {
        const sessionID = decodeURIComponent(usageMatch[1]);
        return getThreadUsage(sessionID, url, env, usageMatch[2] === "export");
      }
      const match = url.pathname.match(/^\/api\/threads\/([^/]+)(?:\/(export(?:\.json|\.md)?|archive))?$/);
      if (match) {
        const sessionID = decodeURIComponent(match[1]);
        if (match[2] === "archive" && request.method === "POST") {
          return archiveThread(sessionID, env);
        }
        if (match[2]?.startsWith("export") && request.method === "GET") {
          return exportThread(sessionID, url, env, match[2] === "export.md" ? "markdown" : "json");
        }
        if (!match[2] && request.method === "GET") {
          return getThread(sessionID, url, env);
        }
      }
      return json({ error: "not_found" }, 404);
    }

    const response = await env.ASSETS.fetch(request);
    const headers = new Headers(response.headers);
    // Public read dashboard: allow embedding in IDE/Orca preview panes.
    // script/style stay same-origin. Ingest remains token-gated on /api/hooks/claude.
    headers.set(
      "Content-Security-Policy",
      "default-src 'self'; script-src 'self'; style-src 'self' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com data:; img-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors *",
    );
    headers.set("Referrer-Policy", "no-referrer");
    headers.set("X-Content-Type-Options", "nosniff");
    headers.set("Cache-Control", url.pathname === "/" || url.pathname.endsWith(".html")
      ? "no-store"
      : "public, max-age=60, must-revalidate");
    return new Response(response.body, { status: response.status, statusText: response.statusText, headers });
  },
};

async function ingestHook(request: Request, env: Env): Promise<Response> {
  const auth = request.headers.get("authorization") ?? "";
  if (!env.INGEST_TOKEN || !(await secureEqual(auth, `Bearer ${env.INGEST_TOKEN}`))) {
    return json({ error: "unauthorized" }, 401);
  }
  const declaredLength = Number(request.headers.get("content-length") ?? "0");
  if (declaredLength > MAX_BODY_BYTES) return json({ error: "payload_too_large" }, 413);

  const raw = await request.text();
  if (encoder.encode(raw).byteLength > MAX_BODY_BYTES) return json({ error: "payload_too_large" }, 413);

  let payload: AnyRecord;
  try {
    payload = JSON.parse(raw) as AnyRecord;
  } catch {
    return json({ error: "invalid_json" }, 400);
  }

  const sessionID = stringValue(payload.session_id);
  const eventType = stringValue(payload.hook_event_name);
  if (!sessionID || !eventType) return json({ error: "session_id_and_hook_event_name_required" }, 400);
  const graphEvents = normalizeGraphEvents(payload.graph_events ?? [], sessionID);
  if (graphEvents.error) return json({ error: "invalid_graph_events", detail: graphEvents.error }, 400);
  for (let index = 0; index < graphEvents.records.length; index++) {
    const record = graphEvents.records[index];
    if (!stringValue(record.event_id).trim() || !stringValue(record.type).trim()) {
      return json({ error: "invalid_graph_events", detail: `record ${index} requires event_id and type` }, 400);
    }
  }

  // Reuse the canonical recursive sanitizer for the complete diagnostic payload.
  // This ensures the legacy events table never receives the raw request body.
  const diagnosticEnvelope = normalizeGraphEvents([{ session_id: sessionID, payload }], sessionID);
  if (diagnosticEnvelope.error) return json({ error: "invalid_payload", detail: diagnosticEnvelope.error }, 400);
  const sanitizedPayload = recordValue(diagnosticEnvelope.records[0]?.payload);
  const usage = normalizeUsageRecords(sanitizedPayload.usage_records, sessionID);
  if (usage.error) return json({ error: "invalid_usage_records", detail: usage.error }, 400);

  const collector = recordValue(sanitizedPayload.collector);
  const createdAt = validISO(stringValue(collector.observed_at)) ?? new Date().toISOString();
  const eventID = stringValue(collector.event_id) || crypto.randomUUID();
  const cwd = stringValue(sanitizedPayload.cwd);
  const model = stringValue(collector.model);
  const effort = stringValue(collector.effort);
  const role = stringValue(collector.role) || "supervisor";
  const projectName = projectFromCwd(cwd);
  const toolName = stringValue(sanitizedPayload.tool_name);
  const title = eventType === "UserPromptSubmit" ? titleFromPrompt(stringValue(sanitizedPayload.prompt)) : "";
  const state = eventType === "SessionEnd" ? "closed" : eventType === "StopFailure" ? "error" : "active";
  const summary = summarizeEvent(sanitizedPayload, eventType, toolName);
  const suppliedRootSessionID = stringValue(sanitizedPayload.root_session_id).trim();
  const suppliedParentSessionID = stringValue(sanitizedPayload.parent_session_id).trim();
  const rootSessionID = suppliedRootSessionID || sessionID;
  const parentSessionID = suppliedParentSessionID && suppliedParentSessionID !== sessionID
    ? suppliedParentSessionID
    : null;

  const upsert = env.DB.prepare(`
    INSERT INTO threads (
      session_id, parent_session_id, root_session_id, title, cwd, project_name,
      model, effort, role, state, started_at, updated_at, ended_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
    ON CONFLICT(session_id) DO UPDATE SET
      parent_session_id = CASE WHEN excluded.parent_session_id IS NOT NULL THEN excluded.parent_session_id ELSE threads.parent_session_id END,
      root_session_id = CASE WHEN excluded.root_session_id != excluded.session_id OR threads.root_session_id = threads.session_id THEN excluded.root_session_id ELSE threads.root_session_id END,
      title = CASE WHEN excluded.title != 'Untitled thread' THEN excluded.title ELSE threads.title END,
      cwd = CASE WHEN excluded.cwd != '' THEN excluded.cwd ELSE threads.cwd END,
      project_name = CASE WHEN excluded.project_name != '' THEN excluded.project_name ELSE threads.project_name END,
      model = CASE WHEN excluded.model != '' THEN excluded.model ELSE threads.model END,
      effort = CASE WHEN excluded.effort != '' THEN excluded.effort ELSE threads.effort END,
      role = CASE WHEN excluded.role != '' THEN excluded.role ELSE threads.role END,
      state = excluded.state,
      updated_at = excluded.updated_at,
      ended_at = COALESCE(excluded.ended_at, threads.ended_at)
  `).bind(
    sessionID,
    parentSessionID,
    rootSessionID,
    title || "Untitled thread",
    cwd,
    projectName,
    model,
    effort,
    role,
    state,
    createdAt,
    createdAt,
    eventType === "SessionEnd" ? createdAt : null,
  );

  const insertEvent = env.DB.prepare(`
    INSERT OR IGNORE INTO events (event_id, session_id, event_type, tool_name, summary, payload_json, created_at)
    VALUES (?, ?, ?, ?, ?, ?, ?)
  `).bind(eventID, sessionID, eventType, toolName, summary, JSON.stringify(sanitizedPayload), createdAt);

  const graphEventInserts = graphEvents.records.map((record) => env.DB.prepare(`
    INSERT OR IGNORE INTO graph_events (
      event_id, session_id, root_session_id, parent_session_id, parent_event_id,
      worker_id, type, role, model, effort, status, started_at, ended_at,
      duration_ms, summary, content, tool_name, tool_use_id, raw_json
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
  `).bind(
    stringValue(record.event_id).trim(),
    sessionID,
    stringValue(record.root_session_id).trim() || rootSessionID,
    nullableString(record.parent_session_id),
    nullableString(record.parent_event_id),
    nullableString(record.worker_id),
    stringValue(record.type).trim(),
    nullableString(record.role),
    nullableString(record.model),
    nullableString(record.effort),
    nullableString(record.status),
    nullableString(record.started_at),
    nullableString(record.ended_at),
    finiteNumberOrNull(record.duration_ms),
    nullableString(record.summary),
    nullableString(record.content),
    nullableString(record.tool_name),
    nullableString(record.tool_use_id),
    JSON.stringify(recordValue(record.raw)),
  ));

  const usageInserts = usage.records.map((record) => env.DB.prepare(`
    INSERT OR IGNORE INTO usage_records (
      usage_id, session_id, message_id, request_id, observed_at, model,
      input_tokens, cache_write_5m_tokens, cache_write_1h_tokens,
      cache_read_tokens, output_tokens, is_fast, carried_cost_usd
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
  `).bind(
    record.usageID,
    sessionID,
    record.messageID,
    record.requestID,
    record.observedAt,
    record.model,
    record.inputTokens,
    record.cacheWrite5mTokens,
    record.cacheWrite1hTokens,
    record.cacheReadTokens,
    record.outputTokens,
    record.isFast ? 1 : 0,
    record.carriedCostUSD,
  ));

  const results = await env.DB.batch([upsert, insertEvent, ...graphEventInserts, ...usageInserts]);
  const graphStart = 2;
  const usageStart = graphStart + graphEventInserts.length;
  const graphEventsWritten = results.slice(graphStart, usageStart)
    .reduce((total, result) => total + Number(result.meta.changes ?? 0), 0);
  const usageRecordsWritten = results.slice(usageStart)
    .reduce((total, result) => total + Number(result.meta.changes ?? 0), 0);

  return json({
    ok: true,
    event_id: eventID,
    graph_events_written: graphEventsWritten,
    usage_records_written: usageRecordsWritten,
  });
}

async function linkChildThread(payload: AnyRecord, parentSessionID: string, createdAt: string, env: Env): Promise<void> {
  const toolName = stringValue(payload.tool_name);
  if (!toolName.includes("claudex-flow")) return;
  if (toolName.endsWith("resume_worker")) return;

  const response = normalizedToolResponse(payload.tool_response);
  const childSessionID = deepString(response, "session_id");
  if (!childSessionID || childSessionID === parentSessionID) return;

  const input = recordValue(payload.tool_input);
  const model = deepString(response, "model");
  const effort = deepString(response, "effort");
  const role = roleFromTool(toolName, response);
  const title = childTitle(role, input);
  const parent = await env.DB.prepare("SELECT root_session_id, cwd, project_name FROM threads WHERE session_id = ?")
    .bind(parentSessionID)
    .first<{ root_session_id: string; cwd: string; project_name: string }>();
  const root = parent?.root_session_id || parentSessionID;

  await env.DB.prepare(`
    INSERT INTO threads (
      session_id, parent_session_id, root_session_id, title, cwd, project_name,
      model, effort, role, state, started_at, updated_at, ended_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, NULL)
    ON CONFLICT(session_id) DO UPDATE SET
      parent_session_id = excluded.parent_session_id,
      root_session_id = excluded.root_session_id,
      title = excluded.title,
      cwd = CASE WHEN threads.cwd = '' THEN excluded.cwd ELSE threads.cwd END,
      project_name = CASE WHEN threads.project_name = '' THEN excluded.project_name ELSE threads.project_name END,
      model = CASE WHEN excluded.model != '' THEN excluded.model ELSE threads.model END,
      effort = CASE WHEN excluded.effort != '' THEN excluded.effort ELSE threads.effort END,
      role = excluded.role,
      updated_at = CASE WHEN excluded.updated_at > threads.updated_at THEN excluded.updated_at ELSE threads.updated_at END
  `).bind(
    childSessionID,
    parentSessionID,
    root,
    title,
    parent?.cwd || "",
    parent?.project_name || "",
    model,
    effort,
    role,
    createdAt,
    createdAt,
  ).run();
}


async function listThreads(url: URL, env: Env): Promise<Response> {
  const query = (url.searchParams.get("q") ?? "").trim().slice(0, 120);
  const includeArchived = url.searchParams.get("archived") === "1";
  const limit = Math.min(Math.max(Number(url.searchParams.get("limit") ?? "100"), 1), 200);
  const like = `%${query}%`;
  const result = await env.DB.prepare(`
    SELECT
      t.*,
      (SELECT COUNT(*) FROM graph_events e WHERE e.session_id = t.session_id) AS event_count,
      (SELECT COUNT(*) FROM threads c WHERE c.parent_session_id = t.session_id) AS child_count
    FROM threads t
    WHERE t.parent_session_id IS NULL
      AND (? = 1 OR t.state != 'archived')
      AND (? = '' OR t.title LIKE ? OR t.project_name LIKE ? OR t.cwd LIKE ? OR t.model LIKE ?)
    ORDER BY t.updated_at DESC
    LIMIT ?
  `).bind(includeArchived ? 1 : 0, query, like, like, like, like, limit).all();
  return json({ threads: result.results });
}

async function getThread(sessionID: string, url: URL, env: Env): Promise<Response> {
  const graph = await buildThreadGraph(sessionID, url.searchParams.get("include_descendants") === "1", env);
  return graph ? json(graph) : json({ error: "not_found" }, 404);
}

async function getThreadUsage(sessionID: string, url: URL, env: Env, download: boolean): Promise<Response> {
  const thread = await env.DB.prepare("SELECT session_id FROM threads WHERE session_id = ?").bind(sessionID).first();
  if (!thread) return json({ error: "not_found" }, 404);

  const includeDescendants = url.searchParams.get("include_descendants") === "1";
  const scopeSQL = includeDescendants
    ? `WITH RECURSIVE scoped(session_id) AS (
        SELECT session_id FROM threads WHERE session_id = ?
        UNION ALL
        SELECT child.session_id FROM threads child JOIN scoped parent ON child.parent_session_id = parent.session_id
      )`
    : `WITH scoped(session_id) AS (SELECT session_id FROM threads WHERE session_id = ?)`;
  const totalsQuery = `${scopeSQL}
    SELECT
      COUNT(*) AS requests,
      COALESCE(SUM(input_tokens), 0) AS input_tokens,
      COALESCE(SUM(cache_write_5m_tokens), 0) AS cache_write_5m_tokens,
      COALESCE(SUM(cache_write_1h_tokens), 0) AS cache_write_1h_tokens,
      COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
      COALESCE(SUM(output_tokens), 0) AS output_tokens,
      COALESCE(SUM(input_tokens + cache_write_5m_tokens + cache_write_1h_tokens + cache_read_tokens + output_tokens), 0) AS total_tokens,
      SUM(carried_cost_usd) AS reported_cost_usd,
      COALESCE(SUM(CASE WHEN carried_cost_usd IS NOT NULL THEN 1 ELSE 0 END), 0) AS reported_cost_records,
      MAX(observed_at) AS updated_at
    FROM usage_records
    WHERE session_id IN (SELECT session_id FROM scoped)`;
  const modelsQuery = `${scopeSQL}
    SELECT
      model,
      COUNT(*) AS requests,
      COALESCE(SUM(input_tokens), 0) AS input_tokens,
      COALESCE(SUM(cache_write_5m_tokens), 0) AS cache_write_5m_tokens,
      COALESCE(SUM(cache_write_1h_tokens), 0) AS cache_write_1h_tokens,
      COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
      COALESCE(SUM(output_tokens), 0) AS output_tokens,
      COALESCE(SUM(input_tokens + cache_write_5m_tokens + cache_write_1h_tokens + cache_read_tokens + output_tokens), 0) AS total_tokens,
      SUM(carried_cost_usd) AS reported_cost_usd,
      COALESCE(SUM(CASE WHEN carried_cost_usd IS NOT NULL THEN 1 ELSE 0 END), 0) AS reported_cost_records,
      MAX(observed_at) AS updated_at
    FROM usage_records
    WHERE session_id IN (SELECT session_id FROM scoped)
    GROUP BY model
    ORDER BY total_tokens DESC, model ASC`;

  const [totalsRow, modelsResult] = await Promise.all([
    env.DB.prepare(totalsQuery).bind(sessionID).first<AnyRecord>(),
    env.DB.prepare(modelsQuery).bind(sessionID).all<AnyRecord>(),
  ]);
  const totals = usageSummary(totalsRow ?? {});
  const data = {
    session_id: sessionID,
    include_descendants: includeDescendants,
    price_status: totals.cost_status,
    totals,
    models: modelsResult.results.map((row) => ({ model: stringValue(row.model), ...usageSummary(row) })),
  };
  if (!download) return json(data);
  return new Response(JSON.stringify(data, null, 2), {
    headers: {
      "Content-Type": "application/json; charset=utf-8",
      "Content-Disposition": `attachment; filename="claudex-usage-${safeFilename(sessionID)}.json"`,
      "Cache-Control": "no-store",
    },
  });
}

async function getStats(env: Env): Promise<Response> {
  const stats = await env.DB.prepare(`
    SELECT
      COUNT(*) AS all_threads,
      COALESCE(SUM(CASE WHEN parent_session_id IS NULL THEN 1 ELSE 0 END), 0) AS root_threads,
      COALESCE(SUM(CASE WHEN state = 'active' THEN 1 ELSE 0 END), 0) AS active_threads,
      COUNT(DISTINCT CASE WHEN model != '' THEN model END) AS model_count,
      (SELECT COUNT(*) FROM graph_events e JOIN threads et ON et.session_id = e.session_id WHERE et.state != 'archived') AS event_count
    FROM threads
    WHERE state != 'archived'
  `).first();
  return json({ stats });
}

async function resolveCanonicalRoot(sessionID: string, env: Env): Promise<{ rootSessionID: string } | null> {
  const selected = await env.DB.prepare(
    "SELECT session_id, root_session_id FROM threads WHERE session_id = ?",
  ).bind(sessionID).first<{ session_id: string; root_session_id: string | null }>();
  if (!selected) return null;
  const rootSessionID = stringValue(selected.root_session_id).trim() || selected.session_id;
  return { rootSessionID };
}

async function archiveThread(sessionID: string, env: Env): Promise<Response> {
  const resolved = await resolveCanonicalRoot(sessionID, env);
  if (!resolved) return json({ error: "not_found" }, 404);

  const result = await env.DB.prepare(`
    UPDATE threads SET state = 'archived', updated_at = ?
    WHERE session_id = ? OR root_session_id = ?
  `).bind(new Date().toISOString(), resolved.rootSessionID, resolved.rootSessionID).run();
  return json({
    ok: true,
    root_session_id: resolved.rootSessionID,
    changed: Number(result.meta.changes ?? 0),
  });
}

async function exportThread(sessionID: string, url: URL, env: Env, format: "json" | "markdown"): Promise<Response> {
  const graph = await buildThreadGraph(sessionID, url.searchParams.get("include_descendants") === "1", env);
  if (!graph) return json({ error: "not_found" }, 404);
  const markdown = format === "markdown";
  return new Response(markdown ? renderThreadMarkdown(graph) : JSON.stringify(graph, null, 2), {
    headers: {
      "Content-Type": markdown ? "text/markdown; charset=utf-8" : "application/json; charset=utf-8",
      "Content-Disposition": `attachment; filename="claudex-thread-${safeFilename(sessionID)}.${markdown ? "md" : "json"}"`,
      "Cache-Control": "no-store",
    },
  });
}

async function buildThreadGraph(sessionID: string, includeDescendants: boolean, env: Env): Promise<AnyRecord | null> {
  const thread = await env.DB.prepare("SELECT * FROM threads WHERE session_id = ?").bind(sessionID).first<AnyRecord>();
  if (!thread) return null;

  const scopeSQL = includeDescendants
    ? `WITH RECURSIVE scoped(session_id) AS (
        SELECT session_id FROM threads WHERE session_id = ?
        UNION
        SELECT child.session_id FROM threads child JOIN scoped parent ON child.parent_session_id = parent.session_id
      )`
    : `WITH scoped(session_id) AS (SELECT session_id FROM threads WHERE session_id = ?)`;

  const [threadsResult, eventsResult, totalsRow, modelsResult, sessionsResult, rolesResult] = await Promise.all([
    env.DB.prepare(`${scopeSQL}
      SELECT * FROM threads
      WHERE session_id IN (SELECT session_id FROM scoped) AND session_id != ?
      ORDER BY started_at ASC, session_id ASC`).bind(sessionID, sessionID).all<AnyRecord>(),
    env.DB.prepare(`${scopeSQL}
      SELECT * FROM graph_events
      WHERE session_id IN (SELECT session_id FROM scoped)
      ORDER BY started_at ASC, event_id ASC`).bind(sessionID).all<AnyRecord>(),
    env.DB.prepare(`${scopeSQL}
      SELECT
        COUNT(*) AS requests,
        COALESCE(SUM(input_tokens), 0) AS input_tokens,
        COALESCE(SUM(cache_write_5m_tokens), 0) AS cache_write_5m_tokens,
        COALESCE(SUM(cache_write_1h_tokens), 0) AS cache_write_1h_tokens,
        COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
        COALESCE(SUM(output_tokens), 0) AS output_tokens,
        COALESCE(SUM(input_tokens + cache_write_5m_tokens + cache_write_1h_tokens + cache_read_tokens + output_tokens), 0) AS total_tokens,
        SUM(carried_cost_usd) AS reported_cost_usd,
        COALESCE(SUM(CASE WHEN carried_cost_usd IS NOT NULL THEN 1 ELSE 0 END), 0) AS reported_cost_records,
        MAX(observed_at) AS updated_at
      FROM usage_records WHERE session_id IN (SELECT session_id FROM scoped)`).bind(sessionID).first<AnyRecord>(),
    env.DB.prepare(`${scopeSQL}
      SELECT model, COUNT(*) AS requests,
        COALESCE(SUM(input_tokens), 0) AS input_tokens,
        COALESCE(SUM(cache_write_5m_tokens), 0) AS cache_write_5m_tokens,
        COALESCE(SUM(cache_write_1h_tokens), 0) AS cache_write_1h_tokens,
        COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
        COALESCE(SUM(output_tokens), 0) AS output_tokens,
        COALESCE(SUM(input_tokens + cache_write_5m_tokens + cache_write_1h_tokens + cache_read_tokens + output_tokens), 0) AS total_tokens,
        SUM(carried_cost_usd) AS reported_cost_usd,
        COALESCE(SUM(CASE WHEN carried_cost_usd IS NOT NULL THEN 1 ELSE 0 END), 0) AS reported_cost_records,
        MAX(observed_at) AS updated_at
      FROM usage_records WHERE session_id IN (SELECT session_id FROM scoped)
      GROUP BY model ORDER BY total_tokens DESC, model ASC`).bind(sessionID).all<AnyRecord>(),
    env.DB.prepare(`${scopeSQL}
      SELECT session_id, COUNT(*) AS requests,
        COALESCE(SUM(input_tokens), 0) AS input_tokens,
        COALESCE(SUM(cache_write_5m_tokens), 0) AS cache_write_5m_tokens,
        COALESCE(SUM(cache_write_1h_tokens), 0) AS cache_write_1h_tokens,
        COALESCE(SUM(cache_read_tokens), 0) AS cache_read_tokens,
        COALESCE(SUM(output_tokens), 0) AS output_tokens,
        COALESCE(SUM(input_tokens + cache_write_5m_tokens + cache_write_1h_tokens + cache_read_tokens + output_tokens), 0) AS total_tokens,
        SUM(carried_cost_usd) AS reported_cost_usd,
        COALESCE(SUM(CASE WHEN carried_cost_usd IS NOT NULL THEN 1 ELSE 0 END), 0) AS reported_cost_records,
        MAX(observed_at) AS updated_at
      FROM usage_records WHERE session_id IN (SELECT session_id FROM scoped)
      GROUP BY session_id ORDER BY session_id ASC`).bind(sessionID).all<AnyRecord>(),
    env.DB.prepare(`${scopeSQL}
      SELECT t.role, COUNT(u.usage_id) AS requests,
        COALESCE(SUM(u.input_tokens), 0) AS input_tokens,
        COALESCE(SUM(u.cache_write_5m_tokens), 0) AS cache_write_5m_tokens,
        COALESCE(SUM(u.cache_write_1h_tokens), 0) AS cache_write_1h_tokens,
        COALESCE(SUM(u.cache_read_tokens), 0) AS cache_read_tokens,
        COALESCE(SUM(u.output_tokens), 0) AS output_tokens,
        COALESCE(SUM(u.input_tokens + u.cache_write_5m_tokens + u.cache_write_1h_tokens + u.cache_read_tokens + u.output_tokens), 0) AS total_tokens,
        SUM(u.carried_cost_usd) AS reported_cost_usd,
        COALESCE(SUM(CASE WHEN u.carried_cost_usd IS NOT NULL THEN 1 ELSE 0 END), 0) AS reported_cost_records,
        MAX(u.observed_at) AS updated_at
      FROM threads t JOIN usage_records u ON u.session_id = t.session_id
      WHERE t.session_id IN (SELECT session_id FROM scoped)
      GROUP BY t.role ORDER BY t.role ASC`).bind(sessionID).all<AnyRecord>(),
  ]);

  const events = eventsResult.results.map(graphEventFromRow);
  const usage = {
    totals: usageSummary(totalsRow ?? {}),
    models: modelsResult.results.map((row) => ({ model: stringValue(row.model), ...usageSummary(row) })),
    sessions: sessionsResult.results.map((row) => ({ session_id: stringValue(row.session_id), ...usageSummary(row) })),
    roles: rolesResult.results.map((row) => ({ role: stringValue(row.role), ...usageSummary(row) })),
  };
  return assembleThreadGraph({
    exportedAt: new Date().toISOString(),
    thread,
    // Include the requested row as relationship evidence too (notably when it is a child).
    threads: [thread, ...threadsResult.results],
    events,
    usage,
  });
}

function graphEventFromRow(row: AnyRecord): AnyRecord {
  let raw: AnyRecord = {};
  try {
    raw = recordValue(JSON.parse(stringValue(row.raw_json)));
  } catch {
    raw = {};
  }
  return {
    event_id: row.event_id,
    session_id: row.session_id,
    root_session_id: row.root_session_id,
    parent_session_id: row.parent_session_id,
    parent_event_id: row.parent_event_id,
    worker_id: row.worker_id,
    type: row.type,
    role: row.role,
    model: row.model,
    effort: row.effort,
    status: row.status,
    started_at: row.started_at,
    ended_at: row.ended_at,
    duration_ms: row.duration_ms,
    summary: row.summary,
    content: row.content,
    tool_name: row.tool_name,
    tool_use_id: row.tool_use_id,
    raw,
  };
}

function normalizeUsageRecords(value: unknown, sessionID: string): { records: NormalizedUsageRecord[]; error: string } {
  if (value === undefined || value === null) return { records: [], error: "" };
  if (!Array.isArray(value)) return { records: [], error: "usage_records must be an array" };
  if (value.length > 750) return { records: [], error: "usage_records exceeds the per-hook limit" };

  const records: NormalizedUsageRecord[] = [];
  for (let index = 0; index < value.length; index++) {
    const row = recordValue(value[index]);
    const usageID = stringValue(row.usage_id).trim();
    const rowSessionID = stringValue(row.session_id).trim();
    const observedAt = validISO(stringValue(row.observed_at));
    if (!usageID || usageID.length > 128) return { records: [], error: `record ${index} has an invalid usage_id` };
    if (rowSessionID && rowSessionID !== sessionID) return { records: [], error: `record ${index} does not belong to this session` };
    if (!observedAt) return { records: [], error: `record ${index} has an invalid observed_at` };

    const inputTokens = nonNegativeInteger(row.input_tokens);
    const cacheWrite5mTokens = nonNegativeInteger(row.cache_write_5m_tokens);
    const cacheWrite1hTokens = nonNegativeInteger(row.cache_write_1h_tokens);
    const cacheReadTokens = nonNegativeInteger(row.cache_read_tokens);
    const outputTokens = nonNegativeInteger(row.output_tokens);
    if ([inputTokens, cacheWrite5mTokens, cacheWrite1hTokens, cacheReadTokens, outputTokens].some((item) => item === null)) {
      return { records: [], error: `record ${index} has an invalid token bucket` };
    }
    if (row.is_fast !== undefined && typeof row.is_fast !== "boolean") {
      return { records: [], error: `record ${index} has an invalid is_fast value` };
    }
    let carriedCostUSD: number | null = null;
    if (row.carried_cost_usd !== undefined && row.carried_cost_usd !== null) {
      if (typeof row.carried_cost_usd !== "number" || !Number.isFinite(row.carried_cost_usd) || row.carried_cost_usd < 0) {
        return { records: [], error: `record ${index} has an invalid carried_cost_usd` };
      }
      carriedCostUSD = row.carried_cost_usd;
    }

    records.push({
      usageID,
      messageID: stringValue(row.message_id).trim(),
      requestID: stringValue(row.request_id).trim(),
      observedAt,
      model: stringValue(row.model).trim(),
      inputTokens: inputTokens as number,
      cacheWrite5mTokens: cacheWrite5mTokens as number,
      cacheWrite1hTokens: cacheWrite1hTokens as number,
      cacheReadTokens: cacheReadTokens as number,
      outputTokens: outputTokens as number,
      isFast: row.is_fast === true,
      carriedCostUSD,
    });
  }
  return { records, error: "" };
}

function usageSummary(row: AnyRecord): AnyRecord {
  const requests = numberValue(row.requests);
  const reportedCostRecords = numberValue(row.reported_cost_records);
  const reportedCostUSD = typeof row.reported_cost_usd === "number" && Number.isFinite(row.reported_cost_usd)
    ? row.reported_cost_usd
    : null;
  return {
    requests,
    input_tokens: numberValue(row.input_tokens),
    cache_write_5m_tokens: numberValue(row.cache_write_5m_tokens),
    cache_write_1h_tokens: numberValue(row.cache_write_1h_tokens),
    cache_read_tokens: numberValue(row.cache_read_tokens),
    output_tokens: numberValue(row.output_tokens),
    total_tokens: numberValue(row.total_tokens),
    reported_cost_usd: reportedCostUSD,
    cost_status: reportedCostRecords === 0 ? "unpriced" : reportedCostRecords === requests ? "reported" : "partially_reported",
    updated_at: stringValue(row.updated_at) || null,
  };
}

function nonNegativeInteger(value: unknown): number | null {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0 ? value : null;
}

function numberValue(value: unknown): number {
  return typeof value === "number" && Number.isFinite(value) ? value : 0;
}

function finiteNumberOrNull(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function nullableString(value: unknown): string | null {
  const normalized = stringValue(value).trim();
  return normalized || null;
}

function summarizeEvent(payload: AnyRecord, eventType: string, toolName: string): string {
  if (eventType === "UserPromptSubmit") return compact(stringValue(payload.prompt), 1200);
  if (eventType === "Stop") return compact(stringValue(payload.last_assistant_message) || "Turn completed", 1200);
  if (eventType === "StopFailure") return compact(stringValue(payload.error) || stringValue(payload.error_message) || "Turn failed", 1200);
  if (eventType === "SessionStart") return `Session ${stringValue(payload.source) || "started"}`;
  if (eventType === "SessionEnd") return `Session ended: ${stringValue(payload.reason) || "other"}`;
  if (eventType === "PostToolUse" || eventType === "PostToolUseFailure") {
    const input = recordValue(payload.tool_input);
    const response = normalizedToolResponse(payload.tool_response);
    const report = recordValue(deepValue(response, "report"));
    const reportSummary = stringValue(report.summary);
    const objective = stringValue(input.objective) || stringValue(input.question) || stringValue(input.instruction);
    const suffix = reportSummary || objective || (eventType === "PostToolUseFailure" ? "Tool failed" : "Tool completed");
    return compact(`${shortToolName(toolName)} · ${suffix}`, 1200);
  }
  return eventType;
}

function normalizedToolResponse(value: unknown): unknown {
  if (typeof value === "string") {
    try { return normalizedToolResponse(JSON.parse(value)); } catch { return value; }
  }
  const record = recordValue(value);
  if (Object.keys(record).length === 0) return value;
  if (record.structuredContent) return normalizedToolResponse(record.structuredContent);
  if (typeof record.content === "string") return normalizedToolResponse(record.content);
  return record;
}

function deepValue(value: unknown, key: string, depth = 0): unknown {
  if (depth > 6 || value === null || value === undefined) return undefined;
  if (Array.isArray(value)) {
    for (const item of value) {
      const found = deepValue(item, key, depth + 1);
      if (found !== undefined) return found;
    }
    return undefined;
  }
  if (typeof value !== "object") return undefined;
  const record = value as AnyRecord;
  if (record[key] !== undefined) return record[key];
  for (const child of Object.values(record)) {
    const found = deepValue(child, key, depth + 1);
    if (found !== undefined) return found;
  }
  return undefined;
}

function deepString(value: unknown, key: string): string {
  return stringValue(deepValue(value, key));
}

function roleFromTool(toolName: string, response: unknown): string {
  if (toolName.endsWith("start_worker") || toolName.endsWith("resume_worker")) return "worker";
  if (toolName.endsWith("search_external")) return "external_search";
  if (toolName.endsWith("digest_urls")) return "url_digest";
  if (toolName.endsWith("explore_repository")) return "repo_explore";
  return deepString(response, "role") || "specialist";
}

function childTitle(role: string, input: AnyRecord): string {
  const subject = stringValue(input.objective) || stringValue(input.question) || "Delegated work";
  const label: Record<string, string> = {
    worker: "Worker",
    external_search: "External search",
    url_digest: "URL digest",
    repo_explore: "Repository explore",
  };
  return compact(`${label[role] || "Agent"}: ${subject}`, 120);
}

function shortToolName(name: string): string {
  return name.replace(/^mcp__[^_]+__/, "").replace(/^mcp__claudex-flow__/, "");
}

function titleFromPrompt(prompt: string): string {
  const first = prompt.split(/\r?\n/).map((line) => line.trim()).find(Boolean) || "Untitled thread";
  return compact(first.replace(/^#+\s*/, ""), 100);
}

function projectFromCwd(cwd: string): string {
  if (!cwd) return "";
  const parts = cwd.replace(/\\/g, "/").split("/").filter(Boolean);
  return parts.at(-1) || "";
}

function compact(value: string, max: number): string {
  const text = value.replace(/\s+/g, " ").trim();
  return text.length <= max ? text : `${text.slice(0, max - 1)}…`;
}

function stringValue(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function recordValue(value: unknown): AnyRecord {
  return value && typeof value === "object" && !Array.isArray(value) ? value as AnyRecord : {};
}

function validISO(value: string): string | null {
  if (!value || Number.isNaN(Date.parse(value))) return null;
  return new Date(value).toISOString();
}

function safeFilename(value: string): string {
  return value.replace(/[^a-zA-Z0-9_-]/g, "-");
}


async function secureEqual(a: string, b: string): Promise<boolean> {
  const [left, right] = await Promise.all([
    crypto.subtle.digest("SHA-256", encoder.encode(a)),
    crypto.subtle.digest("SHA-256", encoder.encode(b)),
  ]);
  const x = new Uint8Array(left);
  const y = new Uint8Array(right);
  let diff = 0;
  for (let index = 0; index < x.length; index++) diff |= x[index] ^ y[index];
  return diff === 0;
}

function base64URL(bytes: Uint8Array): string {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function json(data: unknown, status = 200, extraHeaders: Record<string, string> = {}): Response {
  return new Response(JSON.stringify(data), {
    status,
    headers: {
      "Content-Type": "application/json; charset=utf-8",
      "Cache-Control": "no-store",
      ...extraHeaders,
    },
  });
}
