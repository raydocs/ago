import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import path from "node:path";
import { fileURLToPath } from "node:url";

const appDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

async function frontendSources() {
  const [html, javascript] = await Promise.all([
    readFile(path.join(appDir, "public", "index.html"), "utf8"),
    readFile(path.join(appDir, "public", "app.js"), "utf8"),
  ]);
  return { html, javascript };
}

test("frontend JavaScript only binds selectors present in the Human View DOM", async () => {
  const { html, javascript } = await frontendSources();
  const ids = new Set([...html.matchAll(/\bid="([^"]+)"/g)].map((match) => match[1]));
  const queriedIDs = new Set([...javascript.matchAll(/\$\("#([^"]+)"\)/g)].map((match) => match[1]));
  const missing = [...queriedIDs].filter((id) => !ids.has(id)).sort();

  assert.deepEqual(missing, []);
});

test("frontend JavaScript renders canonical graph events rather than diagnostic hook rows", async () => {
  const { javascript } = await frontendSources();

  assert.match(javascript, /event\.type/);
  assert.doesNotMatch(javascript, /event\.event_type|payload_json/);
});

test("Amp web shell chrome is present in HTML", async () => {
  const { html } = await frontendSources();
  assert.match(html, /id="session-rail"/);
  assert.match(html, /id="search-toggle"/);
  assert.match(html, /id="organize-toggle"/);
  assert.match(html, /id="organize-menu"/);
  assert.match(html, /id="nav-activity"/);
  assert.match(html, /id="nav-projects"/);
  assert.match(html, /id="rail-user"/);
  assert.match(html, /id="main-topbar"/);
  assert.match(html, /id="composer-chrome"/);
  assert.match(html, /thread-doc-icon/);
  // Chat-bubble style row icon (Amp web), not only document.
  assert.match(html, /M21 15a2 2 0 0 1-2 2H7l-4 4V5/);
});

test("panel handoff chrome: timeline modes + execution strip", async () => {
  const { html, javascript } = await frontendSources();
  assert.match(html, /id="execution-strip"/);
  assert.match(html, /id="timeline-toolbar"/);
  assert.match(html, /data-mode="decision"/);
  assert.match(html, /data-mode="full"/);
  assert.match(html, /data-mode="errors"/);
  assert.match(javascript, /filterEventsByMode/);
  assert.match(javascript, /summarizeExecutions/);
  assert.match(javascript, /timelineMode/);
  assert.match(javascript, /Price pending/);
  // P6: modes live in Details drawer, not mid-page hero chrome
  assert.match(html, /id="thread-metadata"[\s\S]*id="timeline-toolbar"/);
});

test("panel P4/P5: gate badges, observed models, progressive turns", async () => {
  const { html, javascript } = await frontendSources();
  assert.match(html, /id="thread-badges"/);
  assert.match(javascript, /detectHandoffSticky/);
  assert.match(javascript, /collectObservedModels/);
  assert.match(javascript, /underAttributedModels/);
  assert.match(javascript, /Observed executors/);
  assert.match(javascript, /TURN_RENDER_CHUNK/);
  assert.match(javascript, /badge-gate/);
  assert.match(javascript, /APP_VERSION = "0\.3\.2"/);
  assert.match(html, /\?r=p6v1/);
});

test("panel P6 visual: Work strip collapse, topbar flags, no mid-page mode control", async () => {
  const { html, javascript } = await frontendSources();
  assert.match(javascript, /work-strip/);
  assert.match(javascript, /Work · /);
  assert.match(javascript, /topbar-flag/);
  assert.match(html, /class="topbar-flags"/);
  // Modes only in Details drawer; badges sit in main topbar.
  assert.ok(html.indexOf('id="thread-metadata"') < html.indexOf('id="timeline-toolbar"'));
  assert.ok(html.indexOf('id="main-topbar"') < html.indexOf('id="thread-badges"'));
  assert.ok(html.indexOf('id="thread-badges"') < html.indexOf('id="event-list"'));
  assert.match(javascript, /return "—"/);
});
