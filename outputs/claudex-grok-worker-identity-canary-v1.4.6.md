# Grok Worker identity canary — v1.4.6

**Date:** 2026-07-14  
**Binary:** `claudex-flow 1.4.6`

## Problem

Durable lane quarantine blocked all Workers:

```text
start_worker unavailable model_mismatch
requested "grok-4.5" resolved to "claude-opus-4-8"
```

## Live identity canary (MCP `start_worker`)

Isolated lane-health path (so old quarantine did not block the canary itself):

| Field | Value |
|---|---|
| requested_model | `grok-4.5` |
| resolved_model | `grok-4.5-build` |
| model_verification | **verified** |
| state | **completed** |
| session_id | `ae42e343-745e-4ef3-9cbc-ca19496b8fac` |
| usage.output_tokens | 204 |
| report | module claudexflow read OK |

Also confirmed with direct `claude --model grok-4.5` → assistant `model=grok-4.5-build`.

## Clear quarantine

```bash
claudex-flow lane-health clear --tool start_worker --canary-pass
```

After clear, real (non-isolated) `route_task` for an implementable slice:

- `action`: **bounded_worker**
- `worker_admissible`: **true**
- `selected_lane`: `grok-4.5` / `high` / `start_worker`

Remaining durable quarantine: `search_external` auth_configuration only (unrelated).

## Artifacts

- `outputs/grok-worker-identity-canary-v1.4.6.json` (stdout)
- `outputs/grok-worker-identity-worker-v1.4.6.json` (full worker packet)

## Note

No gateway config change was required today — Grok resolves correctly through CLIProxy as `grok-4.5-build`. The prior opus mismatch was a stale durable observation, not current mapping.
