import { test, expect, createGoal } from "./fixtures.mjs";

// The attention queue is the whole point of the supervisor from the user's
// side: a decision must appear on the goal page, self-contained, without the
// user going to find any worker.
test.describe("attention queue", () => {
  test.use({ scenario: "blocked_policy" });

  test("a decision the supervisor cannot make alone appears on the goal page", async ({ page, ago }) => {
    await createGoal(page, ago);

    // No intervention: the supervisor reviews the stopped work and queues it.
    await expect(page.getByTestId("attention")).toBeVisible({ timeout: 60_000 });
    const items = page.getByTestId("attention-list").locator("li");
    await expect(items.first()).toBeVisible();

    const first = items.first();
    await expect(first).toContainText("destructive");
    // Self-contained: what happened and what to do about it.
    await expect(first).toContainText("策略拒绝");
    await expect(first).toContainText("建议：");

    // And it links straight to the task rather than to a worker thread.
    await first.getByRole("button", { name: "打开任务" }).click();
    await expect(page.getByTestId("task-drawer")).toBeVisible();
    await expect(page.getByTestId("drawer-body")).toContainText("验收标准");
  });
});

test("a goal that completes on its own shows no attention queue", async ({ page, ago }) => {
  await createGoal(page, ago);
  await expect(page.getByTestId("progress-text")).toContainText("5/5 已完成", { timeout: 60_000 });
  await expect(page.getByTestId("attention")).toBeHidden();
});
