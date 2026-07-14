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
