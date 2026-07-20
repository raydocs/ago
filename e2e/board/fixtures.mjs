import { test as base, expect } from "@playwright/test";
import { startServer } from "./server.mjs";
import { readFile } from "node:fs/promises";
import path from "node:path";

export const CHINESE_OBJECTIVE =
  "分析当前仓库，为 README 增加一个快速开始章节，运行相关测试，并生成完成报告。";

// Planted in the provider environment. The browser is never told a provider
// credential, so this value must not appear on any surface a page can observe.
//
// Note what this deliberately does NOT test: a secret the user types into the
// objective themselves. That text is theirs, and the interface is supposed to
// echo it back, so asserting its absence would be asserting a bug. A hostile
// executor planting secrets into every evidence field is covered by the Go
// test TestSecretSentinelNeverReachesAnyDurableOrVisibleSurface, which can
// drive an executor this suite cannot.
export const SENTINEL = "sk-ant-E2E-SENTINEL-must-never-surface-0123456789";

/**
 * The `ago` fixture starts a dedicated server per test and always stops it,
 * including when the test fails, because the teardown runs after the body
 * regardless of outcome.
 */
export const test = base.extend({
  scenario: ["success", { option: true }],

  ago: async ({ scenario }, use) => {
    const server = await startServer({
      scenario,
      env: {
        // Planted so every test implicitly checks that a configured provider
        // credential never reaches the page.
        AGO_PROVIDER_API_KEY: SENTINEL,
      },
    });
    try {
      await use(server);
    } finally {
      await server.stop();
    }
  },

  // Closes the page's event stream before the page itself is torn down.
  //
  // An SSE response never completes on its own, and Playwright's trace
  // recorder waits for pending network entries when it stops. Leaving the
  // stream open therefore stalled trace finalisation for the full test
  // timeout after any failure. Ending it here is what the page does on unload
  // anyway, and it runs on the failure path because fixture teardown always
  // runs.
  releaseStream: [async ({ page }, use) => {
    await use();
    try {
      await page.evaluate(() => window.agoStopStream && window.agoStopStream());
    } catch (_) {
      // The page may already be gone; nothing to release.
    }
  }, { auto: true }],

  // Records response bodies the page received, so a secret scan can look at
  // what actually crossed the network rather than only at the DOM.
  //
  // The event stream is deliberately excluded: it never completes, so awaiting
  // its body would leave a promise pending forever and hang teardown after any
  // failure. SSE content is scanned separately by readEventStream, which reads
  // a bounded prefix and then stops.
  networkBodies: async ({ page }, use) => {
    const bodies = [];
    const pending = new Set();
    page.on("response", (response) => {
      const type = response.headers()["content-type"] || "";
      if (type.includes("text/event-stream") || type.includes("image") || type.includes("font")) {
        return;
      }
      const task = response
        .text()
        .then((body) => { bodies.push({ url: response.url(), body }); })
        .catch(() => { /* the page navigated away or the body was discarded */ })
        .finally(() => pending.delete(task));
      pending.add(task);
    });
    await use(bodies);
    // Drain what is already in flight, but never wait on it indefinitely: a
    // response that will not settle must not be able to hang the run.
    await Promise.race([
      Promise.allSettled([...pending]),
      new Promise((resolve) => setTimeout(resolve, 2_000)),
    ]);
  },
});

export { expect };

/** Creates a goal through the interface and waits for the board to render. */
export async function createGoal(page, ago, objective = CHINESE_OBJECTIVE) {
  await page.goto(ago.baseURL + "/");
  await page.getByTestId("objective-input").fill(objective);
  await page.getByTestId("repo-input").fill(ago.repositoryRoot);
  await page.getByTestId("create-goal").click();
  await expect(page.getByTestId("goal-panel")).toBeVisible();
  await expect(page.getByTestId("board")).toBeVisible();
  return page.url().split("/boards/")[1];
}

/** Reads the board snapshot straight from the API, for cross-checking the UI. */
export async function snapshot(page, ago, boardId) {
  const response = await page.request.get(`${ago.baseURL}/api/v1/boards/${boardId}`);
  expect(response.ok()).toBeTruthy();
  return response.json();
}

// The fencing token is the credential that authorizes driving an attempt. Its
// shape (64 hex characters) is shared by artifact identifiers and SHA-256
// evidence hashes, both of which are legitimately public, so shape alone cannot
// discriminate. Instead the suite reads the real tokens out of the durable
// store and searches for those exact values — the same approach the Go
// regression test takes.

/** Every fencing token the durable store has issued for this server. */
export async function issuedFencingTokens(ago) {
  const tokens = new Set();
  for (const suffix of ["", "-wal"]) {
    let raw;
    try {
      raw = await readFile(path.join(ago.stateDir, "ago.db" + suffix), "latin1");
    } catch (_) {
      continue; // The WAL may not exist.
    }
    for (const match of raw.matchAll(/"fencing_token":"([0-9a-f]{64})"/g)) {
      tokens.add(match[1]);
    }
  }
  return [...tokens];
}

/**
 * assertNoFencingToken fails if any real issued token appears in the text, or
 * if the field name itself is present.
 */
export function assertNoFencingToken(text, tokens, where) {
  expect(text, `${where} carries a fencing_token field`).not.toContain("fencing_token");
  const leaked = tokens.filter((token) => text.includes(token));
  expect(leaked, `${where} exposed a fencing token`).toEqual([]);
}

/**
 * readEventStream reads a bounded prefix of a board's SSE stream and returns
 * it as text. It always stops: on byte budget, on frame count, or on deadline.
 * Nothing here can wait on a stream that never ends.
 */
export async function readEventStream(ago, boardId, { maxBytes = 256 * 1024, maxMillis = 10_000 } = {}) {
  const controller = new AbortController();
  const stop = setTimeout(() => controller.abort(), maxMillis);
  let text = "";
  try {
    const response = await fetch(`${ago.baseURL}/api/v1/boards/${boardId}/events`, {
      signal: controller.signal,
    });
    const decoder = new TextDecoder();
    for await (const chunk of response.body) {
      text += decoder.decode(chunk, { stream: true });
      if (text.length >= maxBytes) break;
    }
  } catch (_) {
    // Aborting is the expected way this ends.
  } finally {
    clearTimeout(stop);
    controller.abort();
  }
  return text;
}
