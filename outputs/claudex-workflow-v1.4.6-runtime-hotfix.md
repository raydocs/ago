# Claude X Workflow v1.4.6 — runtime canary hotfix

Responds to live `claudex` single-file canary (2026-07-14): 5-minute PostToolUse stalls, resume route_id loss, Grok worker quarantine notes.

**Binary:** `claudex-flow 1.4.6`  
**Contract:** `claudex-workflow.v1.4.6`

## Fixes

### P0 — stall-watch self-deadlock

- **Cause:** `stall-watch` on `PostToolUse` waited up to 300s for transcript growth; Claude Code only appends the tool result after hooks exit → exact 5-minute hang (`idle_ms: 300000`).
- **Fix:**
  - Hook path always `Blocking: false` → immediate `nonblocking_pass`.
  - `configure-hooks` **removes** any installed `stall-watch` handlers.
  - `doctor` fails if `PostToolUse*` still lists stall-watch.

### P0 — Grok worker model mapping (partial)

- Durable quarantine from prior canary: `requested grok-4.5` → `resolved claude-opus-4-8` remains a **gateway resolution** problem (identity canary must still pass before clear).
- Code: `modelMatches` accepts grok family variants (`grok-4.5-build-*`) but **still rejects** opus for grok.
- Do **not** auto-clear `lane-health` — run a real `start_worker` identity canary after gateway maps `grok-4.5` correctly, then `claudex-flow lane-health clear --tool start_worker --canary-pass`.

### P1 — Route ID survives resume

- Open routes now durable at `~/.config/claudex/open-routes.json` (override: `CLAUDEX_OPEN_ROUTES_PATH`).
- `registerRoute` persists; new MCP process hydrates on start; `record_route_outcome` looks up via durable index; terminal outcomes drop open entry (ledger retains history).
- Records include `root_session_id`, `workdir` (MCP cwd at plan time).

### Also retained from 1.4.5 follow-ups

- T3 cache double-count fix
- env/command git wrappers

## Not in this patch

- Full workdir enforcement on Read/Edit
- Flow-tax reduction for one-gate tasks
- Automatic Grok gateway model registry fix
- Live Sol direct vs Grok Worker P50/P90 study (after this is green)

## Install

```bash
~/orca/projects/x/scripts/install-claudex-flow.sh 1.4.6
# strips stall-watch, installs binary + wrapper, contract-guard
```
