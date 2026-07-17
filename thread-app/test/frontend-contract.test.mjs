import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import path from "node:path";
import { fileURLToPath } from "node:url";

const appDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");

async function frontendSources() {
  const [html, javascript, css] = await Promise.all([
    readFile(path.join(appDir, "public", "index.html"), "utf8"),
    readFile(path.join(appDir, "public", "app.js"), "utf8"),
    readFile(path.join(appDir, "public", "styles.css"), "utf8"),
  ]);
  return { html, javascript, css };
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

test("Human View is the default thread detail shell", async () => {
  const { html } = await frontendSources();
  assert.match(html, /id="session-rail"/);
  assert.match(html, /id="search-toggle"/);
  assert.match(html, /id="organize-toggle"/);
  assert.match(html, /id="organize-menu"/);
  assert.match(html, /id="nav-activity"/);
  assert.match(html, /id="nav-projects"/);
  assert.match(html, /id="main-topbar"/);
  assert.match(html, /id="human-thread-header"/);
  assert.match(html, /id="human-thread-title"/);
  assert.match(html, /id="human-thread-meta"/);
  assert.match(html, /class="view-panel human-view"/);
  assert.match(html, />Inspect</);
  assert.doesNotMatch(html, /id="composer-chrome"|id="rail-user"/);
  assert.doesNotMatch(html, /Unlisted|Signed in|Logged in/);
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

test("Human View keeps gate and usage observability with progressive rendering", async () => {
  const { html, javascript } = await frontendSources();
  assert.match(html, /id="thread-badges"/);
  assert.match(javascript, /detectHandoffSticky/);
  assert.match(javascript, /collectObservedModels/);
  assert.match(javascript, /underAttributedModels/);
  assert.match(javascript, /Observed executors/);
  assert.match(javascript, /TURN_RENDER_CHUNK/);
  assert.match(javascript, /LONG_CONTENT_CHARS/);
  assert.match(javascript, /WORK_CLUSTER_PREVIEW/);
  assert.match(javascript, /badge-gate/);
  assert.match(javascript, /APP_VERSION = "0\.4\.0"/);
  assert.match(html, /\?r=human-v1/);
});

test("Show Work, turn deep links, and existing observability controls remain available", async () => {
  const { html, javascript, css } = await frontendSources();
  assert.match(javascript, /Show Work · \$\{countText\}/);
  assert.match(javascript, /stableTurnAnchor/);
  assert.match(javascript, /turnDeepLinkHash/);
  assert.match(javascript, /Copy link/);
  assert.match(javascript, /content-expander/);
  assert.match(javascript, /show-more-work/);
  assert.match(javascript, /more execution\$\{remaining === 1 \? "" : "s"\} available in Full timeline/);
  assert.match(javascript, /isFailedStatus\(result\?\.status\) \|\| isFailedStatus\(event\.status\)/);
  assert.match(css, /\.thread-card:focus:not\(:focus-visible\)/);
  assert.match(css, /@media \(prefers-reduced-motion: reduce\)/);
  assert.match(html, /class="topbar-flags"/);
  assert.ok(html.indexOf('id="thread-metadata"') < html.indexOf('id="timeline-toolbar"'));
  assert.ok(html.indexOf('id="thread-metadata"') < html.indexOf('id="execution-strip"'));
  for (const id of [
    "thread-list", "thread-search", "timeline-toolbar", "usage-tab", "usage-export-link",
    "export-markdown-link", "export-json-link", "copy-thread-md", "archive-button",
  ]) {
    assert.match(html, new RegExp(`id="${id}"`));
  }
  assert.match(javascript, /return "—"/);
});
