import { defineConfig, devices } from "@playwright/test";

// The suite drives a real ago-server started per test with its own temporary
// database, artifact root, and fixture repository. There is no shared server
// and no shared state, so tests can run in parallel and a failure cannot leave
// another test looking at someone else's board.
export default defineConfig({
  testDir: ".",
  testMatch: /.*\.spec\.mjs/,
  // A cold Go build plus a full scheduler run needs headroom on a loaded
  // machine; the individual waits inside the tests are much tighter.
  timeout: 120_000,
  expect: { timeout: 20_000 },
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  workers: process.env.CI ? 2 : undefined,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : [["list"]],
  use: {
    // The locally installed Google Chrome, not a downloaded Chromium build.
    // The interface is a local product a user opens in their own browser, so
    // testing against the browser they actually have is the honest target, and
    // it removes a large download from the suite's setup.
    channel: "chrome",
    // Nothing is captured on success: a passing run leaves no screenshots,
    // videos, or traces to commit by accident. Failures keep enough to debug.
    screenshot: "only-on-failure",
    video: "retain-on-failure",
    // Trace is off deliberately. Playwright's trace recorder waits for pending
    // network entries when it stops, and this interface holds a server-sent
    // event stream open for its whole lifetime, so a trace stalls for the full
    // test timeout after any failure — turning a 9-second failure into a
    // 131-second one. Screenshot and video still capture failures, and the
    // suite asserts against the server's own stdout and stderr, which is where
    // the interesting detail lives anyway.
    trace: "off",
    actionTimeout: 15_000,
  },
  projects: [
    {
      name: "chromium-desktop",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1440, height: 900 } },
    },
    {
      name: "chromium-laptop",
      use: { ...devices["Desktop Chrome"], viewport: { width: 1280, height: 720 } },
      testMatch: /viewports\.spec\.mjs/,
    },
    {
      name: "chromium-mobile",
      use: { ...devices["Desktop Chrome"], viewport: { width: 390, height: 844 }, isMobile: false },
      testMatch: /viewports\.spec\.mjs/,
    },
  ],
});
