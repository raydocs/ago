import test from "node:test";
import assert from "node:assert/strict";
import { reduceStream, boundedTimeline, APPLY, IGNORE, RESYNC } from
  "../../../internal/agoboardui/assets/stream-model.mjs";

// Scenario 6 — the gap handler. Making a browser drop one specific frame
// mid-stream is not reliably reproducible, so the rule itself is tested
// directly here while board.spec.mjs proves the real disconnect and resume.

test("a consecutive event advances the cursor", () => {
  assert.deepEqual(reduceStream({ cursor: 4 }, { sequence: 5 }), { cursor: 5, action: APPLY });
  assert.deepEqual(reduceStream({ cursor: 0 }, { sequence: 1 }), { cursor: 1, action: APPLY });
});

test("an already-seen event is ignored, which is what makes a replay harmless", () => {
  assert.deepEqual(reduceStream({ cursor: 9 }, { sequence: 9 }), { cursor: 9, action: IGNORE });
  assert.deepEqual(reduceStream({ cursor: 9 }, { sequence: 3 }), { cursor: 9, action: IGNORE });
});

test("a gap asks for a resync instead of guessing at the missing events", () => {
  // Sequence 6 was dropped; 7 arrives next.
  assert.deepEqual(reduceStream({ cursor: 5 }, { sequence: 7 }), { cursor: 5, action: RESYNC });
  // The cursor does NOT advance on a gap: advancing it would hide the loss.
  const result = reduceStream({ cursor: 5 }, { sequence: 99 });
  assert.equal(result.action, RESYNC);
  assert.equal(result.cursor, 5);
});

test("a frame without a usable sequence is treated as a gap, not dropped", () => {
  for (const event of [{}, { sequence: null }, { sequence: "x" }, { sequence: 0 }, { sequence: -1 }]) {
    assert.equal(reduceStream({ cursor: 3 }, event).action, RESYNC, JSON.stringify(event));
  }
});

test("a full ordered stream applies every event exactly once", () => {
  let state = { cursor: 0 };
  const applied = [];
  for (let sequence = 1; sequence <= 20; sequence++) {
    const next = reduceStream(state, { sequence });
    if (next.action === APPLY) applied.push(sequence);
    state = { cursor: next.cursor };
  }
  assert.deepEqual(applied, Array.from({ length: 20 }, (_, i) => i + 1));
  assert.equal(state.cursor, 20);
});

test("a resume after a gap continues cleanly once the snapshot resets the cursor", () => {
  // The page missed 6 and 7 and resynced; the snapshot reports sequence 8.
  let state = { cursor: 5 };
  assert.equal(reduceStream(state, { sequence: 8 }).action, RESYNC);
  // The snapshot moves the cursor forward authoritatively.
  state = { cursor: 8 };
  assert.deepEqual(reduceStream(state, { sequence: 9 }), { cursor: 9, action: APPLY });
});

test("the timeline stays bounded", () => {
  let timeline = [];
  for (let i = 0; i < 1200; i++) {
    timeline = boundedTimeline(timeline, { sequence: i }, 500);
  }
  assert.equal(timeline.length, 500);
  assert.equal(timeline[0].sequence, 700);
  assert.equal(timeline[499].sequence, 1199);
});
