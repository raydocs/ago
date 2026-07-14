# Claude X Workflow v1.4.4 — correctness hotfix

Responds to Codex audit `claudex-workflow-v1.4.3-codex-audit-for-grok.md`.

**Binary:** `claudex-flow 1.4.4`  
**Contract:** `claudex-workflow.v1.4.4`

## Track status (honest)

| Track | Status |
|---|---|
| T1 sticky/dual budget | retained |
| T2 handoff | **fixed**: fail-closed allowlist; Bash fully denied; capsule write failure logged; `claudex --from-handoff` implemented |
| T3 context | **fixed**: window-ratio soft/hard (78%/90% of 272k); current sample not max-ever; rolling tool bytes; PostCompact reset; soft warning only |
| T4 admission | **fixed**: partial unknown paths no longer paths-only admit; multi-command done via `&& \|\| ; \| newline` |
| T5 strict Agent | opt-in retained |
| T6 turns | **fixed**: parse `num_turns`; quality exact/upper_bound; resume `MaxTurns=min(10, remaining)` |
| T7 promote | **fixed**: real RouteRecord `state`/`outcome.status`; explicit family/kind; no substring |
| T8 Luna | procedure only (unchanged) |
| T9/T10 | **not completed / deferred** (not in this patch) |
| T11 | **fixed**: handoff Bash deny; destructive git deny; deploy/test budgets retained |
| T12 | **fixed**: flock+atomic write; no auto-clear on healthy; clear CLI requires `--canary-pass` |
| T13 | measure only |

## Canaries (required)

See install verification section in release notes / CI command output.
