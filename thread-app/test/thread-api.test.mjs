import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { mkdir, readFile, readdir, rm } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

import { build } from "esbuild";
import { Miniflare } from "miniflare";

const appDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const runtimeDir = path.join(appDir, ".wrangler", "thread-api-test");
const workerPath = path.join(runtimeDir, "worker.mjs");

let mf;
let db;

before(async () => {
  await rm(runtimeDir, { recursive: true, force: true });
  await mkdir(runtimeDir, { recursive: true });
  await build({
    entryPoints: [path.join(appDir, "src", "index.ts")],
    outfile: workerPath,
    bundle: true,
    format: "esm",
    platform: "browser",
    target: "es2022",
  });

  mf = new Miniflare({
    scriptPath: workerPath,
    modules: true,
    d1Databases: ["DB"],
    bindings: {
      INGEST_TOKEN: "ingest-test-token",
    },
  });
  db = await mf.getD1Database("DB");

  const migrationNames = (await readdir(path.join(appDir, "migrations")))
    .filter((name) => name.endsWith(".sql"))
    .sort();
  for (const name of migrationNames) {
    const sql = await readFile(path.join(appDir, "migrations", name), "utf8");
    const statements = sql.split(";").map((query) => query.trim()).filter(Boolean);
    await db.batch(statements.map((query) => db.prepare(query)));
  }
});

after(async () => {
  await mf?.dispose();
  await rm(runtimeDir, { recursive: true, force: true });
});

async function ingest(payload, token = "ingest-test-token") {
  const headers = { "Content-Type": "application/json" };
  if (token) headers.Authorization = `Bearer ${token}`;
  return mf.dispatchFetch("http://local.test/api/hooks/claude", {
    method: "POST",
    headers,
    body: JSON.stringify(payload),
  });
}

function graphMessage(sessionID, rootSessionID, content, startedAt) {
  return {
    event_id: `${sessionID}:message:0`,
    session_id: sessionID,
    root_session_id: rootSessionID,
    type: "message",
    role: "user",
    status: "completed",
    started_at: startedAt,
    summary: content,
    content,
    raw: { safe: "visible", password: "must-not-survive" },
  };
}

test("anonymous read APIs work without view password", async () => {
  const health = await mf.dispatchFetch("http://local.test/api/health");
  assert.equal(health.status, 200);

  const stats = await mf.dispatchFetch("http://local.test/api/stats");
  assert.equal(stats.status, 200);
  assert.equal((await stats.json()).stats.root_threads, 0);

  const list = await mf.dispatchFetch("http://local.test/api/threads");
  assert.equal(list.status, 200);
  assert.deepEqual((await list.json()).threads, []);
});

test("ingest requires INGEST_TOKEN", async () => {
  const payload = {
    session_id: "auth-check",
    hook_event_name: "SessionStart",
    collector: { event_id: "auth-check-event", observed_at: "2026-07-13T20:00:00.000Z" },
  };
  const missing = await ingest(payload, "");
  assert.equal(missing.status, 401);
  const wrong = await ingest(payload, "wrong-token");
  assert.equal(wrong.status, 401);
});

test("ingest stores sanitized canonical graph events idempotently", async () => {
  const payload = {
    session_id: "root-session",
    root_session_id: "root-session",
    hook_event_name: "Stop",
    cwd: "/workspace/project-x",
    authorization: "Bearer must-not-survive",
    collector: {
      event_id: "hook-root",
      observed_at: "2026-07-13T20:00:01.000Z",
      model: "gpt-5.6-sol",
      effort: "xhigh",
      role: "supervisor",
    },
    graph_events: [
      graphMessage("root-session", "root-session", "Build the archive", "2026-07-13T20:00:00.000Z"),
      {
        ...graphMessage("root-session", "root-session", "Archive ready", "2026-07-13T20:00:00.500Z"),
        event_id: "root-session:message:1",
        role: "assistant",
      },
    ],
    usage_records: [],
  };

  const first = await ingest(payload);
  assert.equal(first.status, 200);
  assert.equal((await first.json()).graph_events_written, 2);
  const second = await ingest(payload);
  assert.equal(second.status, 200);
  assert.equal((await second.json()).graph_events_written, 0);

  const graphRow = await db.prepare("SELECT content, raw_json FROM graph_events WHERE event_id = ?")
    .bind("root-session:message:0")
    .first();
  assert.equal(graphRow.content, "Build the archive");
  assert.doesNotMatch(String(graphRow.raw_json), /must-not-survive/);

  const hookRow = await db.prepare("SELECT payload_json FROM events WHERE event_id = ?")
    .bind("hook-root")
    .first();
  assert.doesNotMatch(String(hookRow.payload_json), /must-not-survive/);

  const listResponse = await mf.dispatchFetch("http://local.test/api/threads");
  assert.equal(listResponse.status, 200);
  const list = await listResponse.json();
  assert.equal(list.threads.find((thread) => thread.session_id === "root-session")?.event_count, 2);
});

test("Human JSON and Markdown endpoints reuse one descendant-aware graph", async () => {
  const childPayload = {
    session_id: "child-session",
    root_session_id: "root-session",
    parent_session_id: "root-session",
    hook_event_name: "Stop",
    cwd: "/workspace/project-x",
    collector: {
      event_id: "hook-child",
      observed_at: "2026-07-13T20:00:03.000Z",
      model: "grok-4.5",
      effort: "high",
      role: "worker",
    },
    graph_events: [graphMessage("child-session", "root-session", "Child result", "2026-07-13T20:00:02.000Z")],
    usage_records: [],
  };
  const child = await ingest(childPayload);
  assert.equal(child.status, 200);

  const detailResponse = await mf.dispatchFetch(
    "http://local.test/api/threads/root-session?include_descendants=1",
  );
  assert.equal(detailResponse.status, 200);
  const detail = await detailResponse.json();
  assert.equal(detail.schema_version, "claudex-thread.v1");
  assert.deepEqual(detail.events.map((event) => event.content), ["Build the archive", "Archive ready", "Child result"]);
  assert.ok(detail.relationships.some((item) => item.type === "parent_session" && item.to === "child-session"));
  assert.equal(detail.usage.totals.reported_cost_usd, null);
  assert.equal(detail.usage.totals.cost_status, "unpriced");

  const jsonResponse = await mf.dispatchFetch(
    "http://local.test/api/threads/root-session/export.json?include_descendants=1",
  );
  assert.equal(jsonResponse.status, 200);
  assert.match(jsonResponse.headers.get("content-disposition") ?? "", /claudex-thread-root-session\.json/);
  const exported = await jsonResponse.json();
  assert.deepEqual(exported.events, detail.events);
  assert.deepEqual(exported.relationships, detail.relationships);

  const markdownResponse = await mf.dispatchFetch(
    "http://local.test/api/threads/root-session/export.md?include_descendants=1",
  );
  assert.equal(markdownResponse.status, 200);
  assert.match(markdownResponse.headers.get("content-type") ?? "", /text\/markdown/);
  const markdown = await markdownResponse.text();
  assert.match(markdown, /root_session_id: "root-session"/);
  assert.match(markdown, /Build the archive[\s\S]*Child result/);
  assert.match(markdown, /Price status: unpriced/);
  assert.doesNotMatch(markdown, /payload_json|must-not-survive/);

  const statsResponse = await mf.dispatchFetch("http://local.test/api/stats");
  assert.equal(statsResponse.status, 200);
  const { stats } = await statsResponse.json();
  assert.equal(stats.event_count, 3);
});
