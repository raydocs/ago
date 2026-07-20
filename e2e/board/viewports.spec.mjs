import { test, expect, createGoal } from "./fixtures.mjs";

// Scenario 8 — the same core page must be operable at every supported size.
// This file runs under three projects, so each assertion executes at
// 1440x900, 1280x720, and 390x844.
test("the board is operable and does not overflow at this viewport", async ({ page, ago }) => {
  const size = page.viewportSize();

  // The composer must be usable before anything else exists.
  await page.goto(ago.baseURL + "/");
  await expect(page.getByTestId("objective-input")).toBeVisible();
  await expect(page.getByTestId("repo-input")).toBeVisible();
  await expect(page.getByTestId("create-goal")).toBeVisible();

  await createGoal(page, ago);
  await expect(page.getByTestId("progress-text")).toContainText("5/5 已完成", { timeout: 60_000 });

  // All seven columns must be reachable. On a narrow screen they stack; on a
  // wide one they sit side by side. Either way they are in the document and
  // scrollable into view.
  for (const name of ["Backlog", "Ready", "Claimed", "Running", "Review", "Blocked", "Done"]) {
    const column = page.getByTestId(`column-${name}`);
    await column.scrollIntoViewIfNeeded();
    await expect(column).toBeVisible();
  }

  // The page body must never scroll horizontally, whatever the board does
  // inside its own scroll container.
  const overflow = await page.evaluate(() => ({
    documentScrollWidth: document.documentElement.scrollWidth,
    innerWidth: window.innerWidth,
    boardScrolls: (() => {
      const board = document.querySelector('[data-testid="board"]');
      return board.scrollWidth > board.clientWidth;
    })(),
  }));
  expect(
    overflow.documentScrollWidth,
    `body overflows horizontally at ${size.width}x${size.height}`,
  ).toBeLessThanOrEqual(overflow.innerWidth + 1);

  // Narrow screens stack the board rather than scrolling it; wider ones may
  // scroll it sideways. Both are acceptable, an overflowing body is not.
  if (size.width <= 780) {
    expect(overflow.boardScrolls, "the board should stack rather than scroll on a narrow screen").toBe(false);
  }

  // The controls must be reachable, not covered by anything.
  const pause = page.getByTestId("pause-button");
  await pause.scrollIntoViewIfNeeded();
  await expect(pause).toBeVisible();
  const box = await pause.boundingBox();
  expect(box.width).toBeGreaterThan(0);
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(size.width + 1);
  // Playwright's actionability check fails if the element is obscured, so a
  // successful click is itself the assertion that nothing covers it.
  await pause.click();
  await expect(page.getByTestId("paused-badge")).toBeVisible();
  await page.getByTestId("resume-button").click();

  // The drawer must open and close at this size.
  await page.getByTestId("card-write-report").click();
  await expect(page.getByTestId("task-drawer")).toBeVisible();
  await expect(page.getByTestId("drawer-body")).toContainText("验收标准");
  await page.getByTestId("drawer-close").click();
  await expect(page.getByTestId("task-drawer")).toBeHidden();

  // Reopening after a reload must still work, which is the recovery path a
  // user on a phone is most likely to hit.
  await page.reload();
  await expect(page.getByTestId("progress-text")).toContainText("5/5 已完成");
});
