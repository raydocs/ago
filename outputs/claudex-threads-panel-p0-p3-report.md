# Panel handoff slices P0–P3 — completion report

**Date:** 2026-07-14  
**Live:** https://claudex-threads.ppop.workers.dev/?r=panel1  
**Gold:** https://claudex-threads.ppop.workers.dev/?r=panel1#/thread/d791d0a0-2f5d-4fbe-8bae-315aa5f3cafb  
**APP_VERSION:** 0.3.0

## P0 — Data attribution
- `go test ./internal/threadusage ./internal/threadgraph ./internal/threadsync` **PASS**
- No runtime ingest code change in this pass (existing collectors already PASS tests)
- d791 historical Sol-only usage treated as **under-attribution**, not backfilled
- UI shows honest attribution banner on Usage

## P1 — Decision mode + execution strip
- Default **Decision** / Full / Errors toolbar
- Decision filters housekeeping Task*/Todo* tools
- Execution strip: `1 worker · 5 agents · 2 failed` on d791
- Files: `public/timeline-model.mjs`, `app.js`, `styles.css`, `index.html`

## P2 — Worker/Agent cards
- Execution cards always on main path (not buried in Show Work)
- Failed cards open by default + red chip + fail note
- d791: 6 execution cards, 2 failed open

## P3 — Usage honesty
- Models / Roles / Sessions tables retained
- Cost: **Price pending** (never $0 for unpriced)
- Cache hit rate + definitions copy
- Supervisor vs workers bar when roles exist

## Tests
- `npm test` → **25/25 pass**
- `npm run typecheck` → pass
- d791 DOM: decision active, strip, failed chips/cards, 5 compact markers, usage banner

## Screenshots
- `outputs/d791-panel1-decision.png`
- `outputs/d791-panel1-usage.png`

## Not done (later slices)
- P0 new canary session proving child model usage rows (needs live worker run + ingest)
- P4 gate events cloud opt-in
- P5 virtual list for 1k+ events
