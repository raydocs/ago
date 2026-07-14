# Panel handoff slices P0b / P4 / P5 — completion report

**Date:** 2026-07-14  
**Live:** https://claudex-threads.ppop.workers.dev/?r=panel2  
**Gold:** https://claudex-threads.ppop.workers.dev/?r=panel2#/thread/d791d0a0-2f5d-4fbe-8bae-315aa5f3cafb  
**APP_VERSION:** 0.3.1  
**Prior push:** `069e4ca` (P0–P3 Decision mode)

## What shipped

### P0b — Honest attribution (no fake usage)

- **Not faked:** d791 still shows only `gpt-5.6-sol` in `usage.models` (historical under-attribution).
- **Observed executors** section on Usage: models from participants + worker cards (e.g. `claude-sonnet-5`, `claude-opus-4-8[1m]`) with roles/sources.
- Banner lists models missing from usage_records: “do not treat missing rows as never ran”.
- Runtime collectors unchanged this pass; `go test ./internal/threadusage ./internal/threadgraph ./internal/threadsync` still **PASS**.
- **Live multi-model canary** (new worker run writing non-Sol usage rows) still requires a real ingest session + `INGEST_TOKEN` — not run here; path remains: start_worker → child Stop hooks → usage_records.

### P4 — Gate / handoff badges (degrade-friendly)

- Tightened `detectHandoffSticky` so **CSS `position: sticky`** and casual “handoff.md” / “handoff implementation” text do **not** light badges.
- Explicit markers only: `CLAUDEX_ROOT_HANDOFF`, `handoff_required`, `CLAUDEX_STICKY`, `sticky re-route`, `type=gate|workflow`, `gate:open/close`.
- Badges: Sticky · ack required / Handoff / Gate · open|cleared.
- No matches → host stays `hidden` (no error empty state).
- Decision mode keeps gate/workflow events when present.
- Gate timeline markers styled when explicit events exist.

### P5 — d791-scale performance

- Progressive turn paint: `TURN_RENDER_CHUNK = 8` via `requestAnimationFrame`, cancelled by `renderToken` on re-select.
- CSS `content-visibility: auto` + `contain-intrinsic-size` on `.turn-card`.
- Empty Decision/Errors copy clarified.
- Default Decision mode already cuts housekeeping noise (from P1).

## Tests

| Check | Result |
|---|---|
| `npm test` | **29/29 pass** |
| `npm run typecheck` | pass |
| runtime go tests | pass |
| deploy | `claudex-threads` Version `b2b16e01-…` |

## Files

- `thread-app/public/timeline-model.mjs`
- `thread-app/public/app.js` (0.3.1)
- `thread-app/public/styles.css`
- `thread-app/public/index.html` (`?r=panel2`)
- `thread-app/package.json`
- `thread-app/test/timeline-model.test.mjs`
- `thread-app/test/frontend-contract.test.mjs`

## Remaining / out of scope

- New live canary session proving child model **usage_rows** (needs real worker + hook ingest).
- Full virtual list windowing (chunked paint + content-visibility is the P5 done bar; deeper virtualization optional).
- Cloud gate event opt-in (`CLAUDEX_GATE_EVENTS_CLOUD=1`) — UI ready to display if events appear.
