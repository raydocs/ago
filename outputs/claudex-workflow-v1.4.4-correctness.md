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

| Canary | Result |
|---|---|
| C-handoff-unknown-mcp | **PASS** — `mcp__filesystem__write_file` deny after compact=3 |
| C-handoff-bash | **PASS** — `pwd & touch`, `ls <(touch)`, `ls` all deny |
| C-git-destructive | **PASS** — `git reset --hard` / `git push --force` deny |
| C-t3-compact-reset | **PASS** — tokens/bytes reset after PostCompact; soft no handoff |
| C-t6-exact-turns | **PASS** — `num_turns` parse; cumulative 20 → MaxTurns 4 |
| C-t7-ledger-fixture | **PASS** — real RouteRecord `outcome.status=accepted` counted |
| C-t4-partial-path | **PASS** — UI+unknown backend path rejects |
| C-t12-race | **PASS** — `go test -race` supervisorgate/mcpserver/routeeval |
| C-from-handoff | **PASS** (wrapper) — `claudex --from-handoff` implemented; full interactive e2e not run |

## Release

- commit: `1f29658`
- tag: `v1.4.4`
- remote: `origin/master` pushed
- artifact: `outputs/claudex-flow-1.4.4.sha256`, `outputs/runtime-contract-1.4.4.json`

