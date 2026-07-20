import { test, expect, createGoal, snapshot, issuedFencingTokens, assertNoFencingToken, readEventStream, CHINESE_OBJECTIVE, SENTINEL } from "./fixtures.mjs";

// Scenario 1 — a Chinese goal reaches Done with no manual advance.
test("a Chinese goal is scheduled to completion and its evidence is inspectable", async ({ page, ago, networkBodies }) => {
  const boardId = await createGoal(page, ago);

  // Nothing in this test advances the board; the background scheduler does.
  await expect(page.getByTestId("progress-text")).toContainText("5/5 已完成", { timeout: 60_000 });
  await expect(page.getByTestId("progress-text")).toContainText("passed");

  // The objective is carried through unchanged.
  await expect(page.getByTestId("goal-objective")).toHaveText(CHINESE_OBJECTIVE);

  // Every task is in Done, and the seven canonical columns all exist.
  for (const name of ["Backlog", "Ready", "Claimed", "Running", "Review", "Blocked", "Done"]) {
    await expect(page.getByTestId(`column-${name}`)).toBeVisible();
  }
  await expect(page.getByTestId("column-Done").locator(".card")).toHaveCount(5);

  // The drawer shows the deterministic evidence behind the acceptance.
  await page.getByTestId("card-write-report").click();
  const drawer = page.getByTestId("drawer-body");
  await expect(drawer).toContainText("必需");
  await expect(drawer).toContainText("通过");
  await expect(page.getByTestId("verdict")).toContainText("accept");
  await expect(page.getByTestId("attempt-summary")).toContainText("1/3");
  await expect(drawer).toContainText("代次");
  await expect(drawer.locator('[data-testid^="artifact-"]').first()).toBeVisible();

  // No fencing token in the rendered page or in anything the browser received.
  const tokens = await issuedFencingTokens(ago);
  expect(tokens.length, "no token was issued, so the check would prove nothing").toBeGreaterThan(0);
  assertNoFencingToken(await page.content(), tokens, "the page HTML");
  for (const { url, body } of networkBodies) {
    assertNoFencingToken(body, tokens, `the response from ${url}`);
  }
});

// Scenario 2 — a transient failure retries, and the failure stays in history.
test.describe("temporary failure", () => {
  test.use({ scenario: "temporary_failure_then_success" });

  test("a transient failure is retried with a new generation and stays in the audit history", async ({ page, ago }) => {
    const boardId = await createGoal(page, ago);
    await expect(page.getByTestId("progress-text")).toContainText("5/5 已完成", { timeout: 60_000 });

    await page.getByTestId("card-identify-commands").click();
    const drawer = page.getByTestId("drawer-body");

    // Two attempts: the first failed transiently, the second passed.
    const attempts = drawer.locator('[data-testid^="attempt-entry-"]');
    await expect(attempts).toHaveCount(2);
    await expect(attempts.nth(0)).toContainText("failed");
    await expect(attempts.nth(0)).toContainText("transient");
    await expect(attempts.nth(1)).toContainText("passed");

    // The retry ran under a different generation, which is what makes the
    // superseded attempt unable to act.
    const generations = await drawer.locator('[data-testid^="attempt-entry-"]').evaluateAll((nodes) =>
      nodes.map((node) => {
        const match = node.textContent.match(/代次：(\d+)/);
        return match ? Number(match[1]) : null;
      }),
    );
    expect(generations[0]).not.toBeNull();
    expect(generations[1]).toBeGreaterThan(generations[0]);

    // The original failure is still recorded, not erased by the success.
    await expect(drawer).toContainText("第一次执行遇到临时故障");
    await expect(page.getByTestId("attempt-summary")).toContainText("2/3");

    // And the durable graph agrees with what the page shows.
    const board = await snapshot(page, ago, boardId);
    expect(board.progress.status).toBe("passed");
  });
});

// Scenario 3 — pause holds across a reload, and resume continues.
test("pause stops new claims, survives a reload, and resume completes the goal", async ({ page, ago }) => {
  const boardId = await createGoal(page, ago);

  await page.getByTestId("pause-button").click();
  await expect(page.getByTestId("paused-badge")).toBeVisible();
  await expect(page.getByTestId("pause-button")).toBeDisabled();

  // While paused the graph must stop advancing. Sample the durable version
  // twice with scheduler ticks in between.
  const first = await snapshot(page, ago, boardId);
  await page.waitForTimeout(3_000);
  const second = await snapshot(page, ago, boardId);
  expect(second.version).toBe(first.version);
  expect(second.paused).toBe(true);

  // A reload must show it still paused, because the state is durable.
  await page.reload();
  await expect(page.getByTestId("paused-badge")).toBeVisible();
  await expect(page.getByTestId("resume-button")).toBeEnabled();

  await page.getByTestId("resume-button").click();
  await expect(page.getByTestId("paused-badge")).toBeHidden();
  await expect(page.getByTestId("progress-text")).toContainText("5/5 已完成", { timeout: 60_000 });
});

// Scenario 4 — artifacts download safely.
test("an artifact downloads with safe headers and matches its recorded metadata", async ({ page, ago }) => {
  const boardId = await createGoal(page, ago);
  await expect(page.getByTestId("progress-text")).toContainText("5/5 已完成", { timeout: 60_000 });

  await page.getByTestId("card-write-report").click();
  const link = page.getByTestId("drawer-body").locator('[data-testid^="artifact-"]').first();
  await expect(link).toBeVisible();
  const href = await link.getAttribute("href");

  const response = await page.request.get(ago.baseURL + href);
  expect(response.status()).toBe(200);

  const disposition = response.headers()["content-disposition"];
  expect(disposition).toContain("attachment");
  // The name must not be able to break out of the header's structure.
  expect(disposition).not.toMatch(/[\r\n]/);
  expect(response.headers()["x-content-type-options"]).toBe("nosniff");

  const body = await response.body();

  // Cross-check size and digest against what the durable evidence recorded.
  const board = await snapshot(page, ago, boardId);
  const doneTask = board.columns.find((c) => c.name === "Done").tasks.find((t) => t.id === "write-report");
  const detail = await (await page.request.get(`${ago.baseURL}/api/v1/boards/${boardId}/tasks/${doneTask.id}`)).json();
  const reference = detail.evidence[0].result.artifacts[0];
  expect(body.length).toBe(reference.bytes);

  const digest = await page.evaluate(async (bytes) => {
    const buffer = await crypto.subtle.digest("SHA-256", new Uint8Array(bytes));
    return [...new Uint8Array(buffer)].map((b) => b.toString(16).padStart(2, "0")).join("");
  }, [...body]);
  expect(digest).toBe(reference.sha256);

  // Nothing dangerous in the headers, the name, or the bytes.
  const headerText = JSON.stringify(response.headers());
  expect(headerText).not.toContain(SENTINEL);
  expect(headerText, "the download headers leaked a local path").not.toContain(ago.stateDir);
  assertNoFencingToken(headerText, await issuedFencingTokens(ago), "the download headers");
  expect(body.toString("utf8")).not.toContain(SENTINEL);
});

// Scenario 5 — a real disconnect and reconnect, with no page refresh.
test("the event stream survives a real disconnect and resumes without a refresh", async ({ page, context, ago }) => {
  const boardId = await createGoal(page, ago);
  await expect(page.getByTestId("stream-state")).toHaveText("已连接");
  const beforeCursor = (await page.evaluate(() => window.agoDiagnostics())).cursor;

  // Cut the browser's network immediately, while there is still plenty of work
  // left. The scheduler is a separate process and keeps going regardless.
  //
  // The page's own connection state is deliberately not asserted here: whether
  // an already-established EventSource is torn down promptly by offline
  // emulation is browser behaviour, not Ago's contract. What matters is that
  // the page converges on the truth once the network returns.
  await context.setOffline(true);

  // Real progress must happen while the page is blind to it. This runs in Node,
  // which is not subject to the browser's offline emulation.
  await expect
    .poll(async () => {
      const response = await fetch(`${ago.baseURL}/api/v1/boards/${boardId}`);
      return (await response.json()).latest_event_sequence;
    }, { timeout: 45_000, intervals: [250] })
    .toBeGreaterThan(beforeCursor);

  await context.setOffline(false);

  // Without any refresh, the page must catch up to the authoritative state.
  await expect(page.getByTestId("stream-state")).toHaveText("已连接", { timeout: 45_000 });
  await expect(page.getByTestId("progress-text")).toContainText("5/5 已完成", { timeout: 60_000 });

  const board = await snapshot(page, ago, boardId);
  await expect
    .poll(async () => (await page.evaluate(() => window.agoDiagnostics())).cursor, { timeout: 20_000 })
    .toBe(board.latest_event_sequence);

  // Reconnection must not duplicate work: five tasks, each rendered once.
  await expect(page.getByTestId("column-Done").locator(".card")).toHaveCount(5);
  const cardIds = await page.locator(".card").evaluateAll((nodes) => nodes.map((n) => n.dataset.testid));
  expect(new Set(cardIds).size).toBe(cardIds.length);

  // And the durable graph has exactly one accepted attempt per task.
  for (const column of board.columns) {
    for (const task of column.tasks) {
      const detail = await (await page.request.get(`${ago.baseURL}/api/v1/boards/${boardId}/tasks/${task.id}`)).json();
      const passed = detail.attempts.filter((a) => a.state === "passed");
      expect(passed.length, `task ${task.id} accepted attempts`).toBe(1);
    }
  }
});

// Scenario 7 — the provider credential must not reach anything the browser sees.
test("no provider secret or fencing token reaches the DOM or the network", async ({ page, ago, networkBodies }) => {
  const boardId = await createGoal(page, ago);
  await expect(page.getByTestId("progress-text")).toContainText("5/5 已完成", { timeout: 60_000 });

  // Open every task so all evidence, tests, commands and artifacts render.
  const board = await snapshot(page, ago, boardId);
  for (const column of board.columns) {
    for (const task of column.tasks) {
      await page.getByTestId(`card-${task.id}`).click();
      await expect(page.getByTestId("drawer-body")).toBeVisible();
      await page.getByTestId("drawer-close").click();
    }
  }

  const tokens = await issuedFencingTokens(ago);
  expect(tokens.length, "no token was issued, so the check would prove nothing").toBeGreaterThan(0);

  assertNoFencingToken(await page.content(), tokens, "the page HTML");
  assertNoFencingToken(await page.locator("body").innerText(), tokens, "the visible page text");

  // The provider credential lives only in the server's environment. It must
  // reach neither the page nor anything the browser was sent.
  expect(await page.content(), "the page HTML leaked the provider credential").not.toContain(SENTINEL);
  for (const { url, body } of networkBodies) {
    assertNoFencingToken(body, tokens, `the response from ${url}`);
    expect(body, `the response from ${url} leaked the provider credential`).not.toContain(SENTINEL);
  }

  // The event stream is read separately, with a bounded prefix, because it
  // never completes on its own.
  const stream = await readEventStream(ago, boardId);
  expect(stream, "the event stream carried no frames, so the scan proves nothing").toContain("data:");
  assertNoFencingToken(stream, tokens, "the event stream");
  expect(stream, "the event stream leaked the provider credential").not.toContain(SENTINEL);

  // Nor the server's own diagnostics.
  expect(ago.stdout(), "server stdout leaked the provider credential").not.toContain(SENTINEL);
  expect(ago.stderr(), "server stderr leaked the provider credential").not.toContain(SENTINEL);
});
