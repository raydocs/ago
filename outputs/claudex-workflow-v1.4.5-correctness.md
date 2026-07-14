# Claude X Workflow v1.4.5 — narrow correctness patch

Responds to Codex follow-up audit `claudex-workflow-v1.4.4-codex-followup.md`.

**Binary:** `claudex-flow 1.4.5`  
**Contract:** `claudex-workflow.v1.4.5`  
**Scope:** correctness only — no MaxTurns raise, no UI productization, no compound workers.

## Track status (honest)

| Track | Status |
|---|---|
| T1 sticky/dual budget | retained |
| T2 handoff | **fixed**: wrapper full-arg parse; `--from-handoff` rejects `--resume/--continue`; installer installs `claudex` wrapper; capsule adds transcript/path/verification/workflow snapshot fields |
| T3 context | **partial**: tool-result bytes + transcript tail usage sample + StopFailure latch work; official PostToolUse still has no `input_tokens` — token governor uses transcript when hook fields absent; gateway accounting still not wired |
| T4 admission | **fixed**: unknown-only single structural package admits; known+unknown rejects; no fabricated API+UI for lone `internal/pkg` paths |
| T5 strict Agent | opt-in retained |
| T6 turns | **fixed**: aggregate `turn_accounting_quality` is worst-of (`unknown > upper_bound > exact`); exact cannot upgrade prior upper_bound |
| T7 promote | **improved / partial**: schema + major-correction / child-failure / cohort stability gates before `eligible_for_manual_promote_review`; still requires human `--confirm`. Real RouteRecord often lacks explicit `family` (falls back to `plan.kind`); non-inferiority is **within-cohort stability**, not a strict A/B vs alternate route baseline |
| T8 Luna | procedure only (unchanged) |
| T9/T10 | **not completed / deferred** |
| T11 | **fixed**: argv-style destructive git (`git -C`, any-position `--force`/`-f`/`--hard`) |
| T12 | **fixed**: session+durable merge by `ObservedAt`; fresher durable unavailable beats stale session healthy |
| T13 | measure only |

## Canaries

| Canary | Result |
|---|---|
| C-git-`-C`-hard | **PASS** — `git -C . reset --hard` deny |
| C-git-push-force-trailing | **PASS** — `git push origin main --force` / `-f` deny |
| C-handoff+resume | **PASS** — wrapper exit 2, does not invoke claude |
| C-handoff-mid-args | **PASS** — `--from-handoff` parsed when not first arg |
| C-unknown-only-catalog | **PASS** — `internal/catalog/catalog.go` admits as `unknown` |
| C-t6-quality-no-upgrade | **PASS** — cumulative 12 stays `upper_bound` |
| C-t7-major-block | **PASS** — 10× major correction not eligible |
| C-t12-freshness | **PASS** — fresher durable unavailable wins |
| C-t3-transcript-sample | **PASS** — latest prompt tokens from transcript tail |
| C-artifact-immutable | **PASS** — `dist/claudex-flow-1.4.5-$GOOS-$GOARCH` + sha256 of that file |

## Release

- version: `1.4.5`
- contract: `claudex-workflow.v1.4.5`
- artifact: `dist/claudex-flow-1.4.5-darwin-arm64` (built on install host)
- checksum: `outputs/claudex-flow-1.4.5.sha256` (hashes the dist artifact, not `~/.local/bin`)
- docs: this file; prior 1.4.4 T3 “fixed” claim corrected to **partial** until gateway accounting

## Non-goals (still deferred)

- MaxTurns increase
- Thread App UI productization
- Compound multi-domain workers
- Full gateway request-token accounting for T3 hard governor
