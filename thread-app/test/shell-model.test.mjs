import assert from "node:assert/strict";
import test from "node:test";
import {
  threadBucket,
  groupThreadsForRail,
  stableTitleFromThread,
  compactModelName,
} from "../public/shell-model.mjs";

const NOW = Date.parse("2026-07-14T12:00:00Z");

test("threadBucket splits active / inactive24h / older by updated_at age", () => {
  assert.equal(
    threadBucket({ updated_at: "2026-07-14T11:00:00Z" }, NOW),
    "active",
  );
  assert.equal(
    threadBucket({ updated_at: "2026-07-13T18:00:00Z" }, NOW),
    "inactive24h",
  );
  assert.equal(
    threadBucket({ updated_at: "2026-07-10T12:00:00Z" }, NOW),
    "older",
  );
  assert.equal(threadBucket({ updated_at: "not-a-date" }, NOW), "older");
});

test("groupThreadsForRail leaves Active unlabeled and labels Inactive Last 24h", () => {
  const threads = [
    { session_id: "a", title: "Active one", updated_at: "2026-07-14T11:00:00Z" },
    { session_id: "b", title: "Inactive one", updated_at: "2026-07-13T18:00:00Z" },
    { session_id: "c", title: "Older one", updated_at: "2026-07-10T12:00:00Z" },
  ];
  const groups = groupThreadsForRail(threads, NOW);
  assert.deepEqual(
    groups.map((g) => ({ key: g.key, label: g.label, n: g.threads.length })),
    [
      { key: "active", label: null, n: 1 },
      { key: "inactive24h", label: "Inactive Last 24h", n: 1 },
      { key: "older", label: "Older", n: 1 },
    ],
  );
});

test("stableTitleFromThread uses first line and truncates long titles", () => {
  assert.equal(stableTitleFromThread({ title: "  Hello\nWorld  " }), "Hello");
  assert.equal(stableTitleFromThread({ title: "" }), "Untitled thread");
  const long = "x".repeat(80);
  const titled = stableTitleFromThread({ title: long }, 72);
  assert.equal(titled.length, 72);
  assert.ok(titled.endsWith("…"));
});

test("compactModelName strips claude- prefix and -build suffix", () => {
  assert.equal(compactModelName("claude-sonnet-4-build"), "sonnet-4");
  assert.equal(compactModelName("gpt-5.6-sol"), "gpt-5.6-sol");
  assert.equal(compactModelName(""), "");
});
