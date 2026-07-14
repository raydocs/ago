# Claude X Workflow v1.4.3 — T1–T13 bundle (for Codex audit)

**Binary:** `claudex-flow 1.4.3`  
**Contract:** `claudex-workflow.v1.4.3`  
**Policy:** patch unit only (1.4.2 → 1.4.3), not 1.5.0.

## Track map (what landed)

| Track | Status in 1.4.3 | Where |
|---|---|---|
| T1 sticky / dual budget / declare_gate | **carried from 1.4.2** | `supervisorgate`, MCP gate tools |
| T2 auto capsule | **shipped** | atomic `.json+.md` under `~/.config/claudex/handoffs/` on handoff latch |
| T3 context governor | **shipped (telemetry+latch)** | usage prompt-side tokens, tool_response bytes, StopFailure overflow latch; gateway hard-reject **not** enabled |
| T4 admission 2.0 | **shipped** | path-first (1.4.1+) + `suggested_slices` templates + multi-`&&` done_condition reject |
| T5 Agent strict | **shipped opt-in** | `CLAUDEX_WORKFLOW_STRICT=1` PreToolUse deny Agent |
| T6 Worker economy | **shipped** | `cumulative_model_turns` ledger; cap 24 across start+resume; MaxTurns/invoke stays 10 |
| T7 route learning | **dry-run only** | `claudex-flow route-promote` / `--confirm` writes pending marker only; **no auto merge** |
| T8 Luna canary | **procedure** | `scripts/canary-luna-compact.md` + existing adapter tests; effort **not** lowered |
| T9 cost attribution | **minimal** | usage fields on gate status; full product deferred |
| T10 Thread product | **deferred UI** | no Thread App visual rewrite in this patch; gate counters local |
| T11 Bash/deploy budget | **shipped** | deploy cap 3; test-bash cap 12; handoff mutating Bash still denied |
| T12 durable quarantine | **shipped** | `~/.config/claudex/lane-health.json` TTL 24h merged into liveLaneHealth |
| T13 orchestrator compression | **measure only** | `claudex-flow orchestrator-stats` / `scripts/orchestrator-stats.sh` |

## Explicit non-claims

- Not Amp-superior without canaries.
- MaxTurns not raised.
- No auto spawn of new Claude process.
- No gateway byte hard-reject.
- No automatic route promotion into router defaults.

## Codex audit focus

1. Handoff capsule race under flock correctness.
2. Sticky + dual budget interaction.
3. T3 false positive pressure thresholds (140k/170k).
4. Durable quarantine merge order vs session memory.
5. cumulative_model_turns estimation honesty.
6. suggested_slices map iteration order nondeterminism (OK for templates).

## Install

```bash
cd /Users/ruirui/orca/projects/x
./scripts/install-claudex-flow.sh 1.4.3
# restart Claude X / MCP
claudex-flow doctor
```
