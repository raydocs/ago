# Claude X / claudex-flow — Post-v1.4 Upgrade Proposal (for Codex review)

**Status:** design proposal only — not an implementation brief to execute blindly  
**Baseline:** installed `claudex-flow 1.4.0`, contract `claudex-workflow.v1.4`  
**Canonical source:** `/Users/ruirui/orca/projects/x`  
**Date:** 2026-07-14  
**Audience:** Codex (architecture / risk / sequencing review)  
**Primary forensic reference:** Root thread `d791d0a0-2f5d-4fbe-8bae-315aa5f3cafb` (7h wall, Sol-dominated thrash, one oversized Grok worker max-turns failure, 5× compact, 4× “Prompt is too long”)

---

## 0. How to read this document

This proposal answers: **after v1.4 supervisor-gate + composite admission, what is still worth building, in what order, and with what success criteria?**

Codex should review for:

1. **Correct problem framing** — are we solving the real failure modes, or cosplaying Amp?
2. **Hard vs soft enforcement** — which controls must be runtime-enforced vs orchestrator prose?
3. **Sequencing** — which items unlock measurement vs which require measurement first?
4. **Surface limits** — what Claude Code / CLIProxyAPI cannot honestly support?
5. **Blast radius** — hooks that fail closed can brick sessions; fail open can re-enable thrash.
6. **Non-goals** — what we should refuse even if Amp has it.

**Scoring guidance for reviewers:** mark each track `Approve / Approve with changes / Reject / Defer` and name the minimum viable slice.

---

## 1. Current architecture snapshot (truth, not marketing)

### 1.1 Topology

```text
Claude Code process
  lead: gpt-5.6-sol / xhigh  (orchestrator.md soft policy)
  tools: Bash, Read/Edit/Write, native Agent, Playwright, MCP, ...

  hooks (zero-model):
    UserPromptSubmit → route-hint (soft inject)
    PreToolUse       → supervisor-gate (hard deny budgets / handoff)
    PostToolUse*     → thread-hook + stall-watch + supervisor-gate counters
    Pre/PostCompact  → thread-hook (+ gate compact count on PostCompact)
    SessionStart     → contract-guard + thread-hook + gate init

  MCP claudex-flow (long-lived):
    route_task / record_route_outcome
    start_worker / resume_worker / close_worker   → Grok 4.5 high, MaxTurns=10
    search_external → Grok; digest_urls → Gemini Flash; explore_repository → Terra
    find_thread (local zero-model); read_thread → GLM 5.2
    consult_native_claude → subscription Claude
    workflow_status / runtime_contract
    lane health quarantine (session-scoped)

  gateway adapter :8318
    ordinary Sol traffic pass-through
    native compact prompt → rewrite model to gpt-5.6-luna

  cloud threads (CF Worker + D1)
    async ingest of sanitized graph/usage; public read UI
```

### 1.2 What is already HARD (runtime)

| Control | Where | Notes |
|---|---|---|
| Worker field admission | MCP | objective, marginal, done_condition, paths, deadline |
| Composite multi-domain reject | MCP `admission_domains` | keyword/path heuristic, not semantic |
| Write lease / path scope | MCP | overlaps blocked; shell not OS-sandboxed |
| Worker MaxTurns=10, resumes≤3 | MCP | per start/resume invoke |
| Specialist budgets | MCP | research/native/find/read caps |
| Lane quarantine | MCP session | auth / model mismatch |
| Supervisor tool budgets | PreToolUse gate | Playwright 12, screenshot 8, verify verify 3, high-cost soft/hard |
| Root lifecycle force-stop | PreToolUse gate | 3 compacts / 4h / 8MiB → deny construction tools |
| Luna compact rewrite | gateway adapter | marker-based; not Claude Code setting |
| Route outcome ledger write | MCP | append-only child metrics; `supervisor_included=false` |
| Stall rewake | PostToolUse asyncRewake | tool-return stall only |

### 1.3 What is still SOFT (prose / voluntary)

| Behavior | Reality |
|---|---|
| Call `route_task` before substantial work | Sol can ignore forever |
| Prefer Worker over native Agent/codex-rescue | native tools unrestricted except high-cost counting |
| Record `record_route_outcome` after terminal | optional |
| UI only after contract tests green | not enforced beyond Playwright budget |
| Split work into gates | not a machine-checked state machine |
| Write handoff capsule then new Root | gate **denies tools** but does **not auto-write capsule or open new Root** |
| Durable route promotion from outcomes | ledger written; **never read back into router** |

### 1.4 Honest capability score (split)

| Dimension | Score | Why |
|---|---|---|
| Worker / identity / usage plumbing | **0.75–0.85** | real child sessions, identity, failures, graph hooks much better than 0.4.x |
| Amp-like specialist routing | **0.70** | find/read/search/digest/explore exist; sample quality not proven |
| Long-task unattended stability | **0.55–0.65** | v1.4 hard gates help; still no full scheduler; Prompt-too-long unsolved |
| Accepted-result cost learning | **0.30** | collect-only; no promotion loop |
| Cloud Thread product parity | **0.60** | usable archive; child usage/model rows still incomplete vs Amp |
| Compaction quality | **0.50–0.65** | Luna lane exists; retention/latency not benchmarked vs Sol/Amp |

---

## 2. North star and non-goals

### 2.1 North star (one sentence)

**Minimize accepted-result cost and wall-clock thrash on real coding tasks**, by making the runtime a **scheduler with hard budgets**, not a multi-model demo rack.

### 2.2 Explicit non-goals (reject unless user forces)

1. **Do not raise Worker MaxTurns** to “fix” oversized slices (10 stays; maybe *lower* for riskier classes).
2. **Do not mandatory multi-model fan-out** for Usage aesthetics.
3. **Do not claim Amp superiority** without frozen canary families + non-inferiority thresholds.
4. **Do not bypass Cloudflare Access** for cloud find_thread; local index first.
5. **Do not fail-closed every hook** in ways that freeze Claude when gate code panics (prefer fail-open + metrics).
6. **Do not put full transcript bodies in D1 / route ledgers**.
7. **Do not invent supervisor dollar cost** while subscription + gateway accounting are incomparable.

### 2.3 Design principles (carry forward)

1. Zero-model when possible; model only for irreducible gaps.
2. Hard budgets over longer prompts.
3. One failure dimension at a time.
4. Evidence over tool-call theater.
5. Root is disposable; work is resumable via handoff + find/read_thread.
6. Child Workers are narrow and restartable; Supervisor is the only synthesiser.

---

## 3. Gap inventory (from d791 + code)

### Class A — caused the multi-hour thrash (must fix or mitigate)

| ID | Gap | v1.4 status | Residual risk |
|---|---|---|---|
| A1 | Sol unlimited construction | soft → **partial hard** budgets | native Agent still cheap relative to Playwright; Bash loops not domain-gated |
| A2 | Oversized Worker slice | **hard composite reject** | heuristics brittle; well-worded multi-domain packet may still pass |
| A3 | Compact death spiral | **hard stop after 3** | no auto handoff file; human must start new Root; Prompt-too-long still before compact |
| A4 | Prompt is too long | **unsolved** | stalls before tool hooks; no pre-request context governor |
| A5 | UI pixel loops | **hard Playwright budget** | no “hypothesis change” requirement beyond fingerprint; budget may be too high/low |
| A6 | Failure opacity (Worker) | largely fixed pre-1.4 | still need UX in Thread app for blocked/max_turns |

### Class B — missing scheduler completeness

| ID | Gap | Notes |
|---|---|---|
| B1 | No machine-checked **gate state machine** | orchestrator talks gates; runtime has no `current_gate` object |
| B2 | Handoff is deny-only | no `write_handoff` tool / auto capsule path |
| B3 | Native Agent / codex-rescue bypass workflow graph | counted as high-cost but not forced through MCP |
| B4 | Re-route check is advisory soft | Sol can ignore `CLAUDEX_REROUTE_CHECK` until hard cap |
| B5 | Gate budgets not per-gate, only per-Root | one Root = one budget pot; correct gate reset missing |
| B6 | No escape hatch with user attestation | risk of false deny mid-critical fix |

### Class C — measurement and learning

| ID | Gap | Notes |
|---|---|---|
| C1 | `route-outcomes.jsonl` write-only | router never promotes |
| C2 | Supervisor tokens not in child ledger | `supervisor_included=false` honest but incomplete |
| C3 | No frozen canary suite for accepted-result | route-eval is zero-model policy only |
| C4 | Luna compact quality unbenchmarked | exists ≠ better retention |
| C5 | Lane health is MCP-session local | dies on restart; no durable quarantine file |

### Class D — product / observability

| ID | Gap | Notes |
|---|---|---|
| D1 | Usage often Sol-only aggregation | child Grok/Opus rows incomplete historically |
| D2 | Graph UI noise (TaskCreate thrash) | needs hierarchical collapse by gate/worker |
| D3 | No live supervisor-gate status in Thread | budgets invisible to user |
| D4 | Cloud find across devices | Access-protected; needs service token design |
| D5 | Pricing / cost display | unpriced; example JSON only |

### Class E — platform constraints (honest walls)

| ID | Constraint | Implication |
|---|---|---|
| E1 | Claude Code compact uses mainLoopModel client-side | Luna only via gateway marker rewrite |
| E2 | No OS path sandbox for Bash | write leases incomplete for shell |
| E3 | Effort resolved not in stream | `cli_argument_only` forever until platform changes |
| E4 | MCP subprocess not hot-reloaded | every binary upgrade needs process restart |
| E5 | Prompt-too-long is API-level | hooks cannot block a request already too large |

---

## 4. Proposed upgrade tracks (detailed)

Tracks are versioned **v1.5 → v1.8+** for discussion; numbering can change. Each track has: problem, success criteria, design, files, tests, risks, open questions.

---

### Track T0 — v1.4 hardening (patch, before features)

**Problem:** v1.4 just landed; edge cases and operability matter more than new ideas.

**Success criteria**

1. Gate fail-open path emits structured log (not silent) when JSON decode fails.
2. Budgets configurable via env without rebuild (`CLAUDEX_GATE_PLAYWRIGHT_MAX`, etc.).
3. `workflow_status` or `supervisor-gate status` can dump current Root counters for the active session id.
4. Composite admission unit tests cover false positives (single-domain UI-only, API-only).
5. Doctor checks gate state dir is writable.
6. Document: restart required after upgrade.

**Design**

- Extend `supervisorgate.Config` from env in `main.supervisorGate`.
- Add `claudex-flow gate-status --session ID` zero-model CLI.
- Log gate denials to `~/.config/claudex/supervisor-gate/events.jsonl` (no tool args bodies).

**Files:** `internal/supervisorgate/*`, `cmd/claudex-flow/main.go`, doctor, README  
**Risks:** low  
**Codex question:** Should defaults stay aggressive (12/3/3) or start looser until canaries?

---

### Track T1 — Gate-scoped budgets + attestation escape (v1.5 core)

**Problem:** Per-Root budgets punish long legitimate multi-gate work; false denies need a controlled escape.

**Success criteria**

1. Runtime understands `current_gate_id` (string) set by Supervisor via zero-model MCP tool `declare_gate` or UserPromptSubmit marker.
2. Playwright/verify budgets **reset when gate_id changes** (with max gates per Root, e.g. 8).
3. Escape hatch: `CLAUDEX_GATE_OVERRIDE` tool or PreToolUse allow after explicit user message `/gate-override reason=...` once per Root, recorded in state + Thread event.
4. Soft re-route becomes **sticky**: after soft inject, next **N** high-cost tools denied until Sol calls zero-model `ack_reroute` with structured fields `{gate, remaining_acceptance[], worker_decision}`.

**Design**

```text
declare_gate(gate_id, acceptance[], stop_condition)
  → writes session state; resets tool budgets for that gate
  → caps: max 8 gates / Root; max 1 open gate

ack_reroute(gate_id, remaining[], worker: none|start|resume)
  → clears soft block

gate-override (user-only)
  → one shot; increments override_count; emits graph event
```

**Sticky re-route (critical)**  
Today soft check is ignorable. Sticky deny until `ack_reroute` is the difference between “budget” and “scheduler”.

**Files:** `internal/supervisorgate`, new MCP tools in `mcpserver`, orchestrator.md  
**Tests:** unit for reset; sticky deny without ack; override once  
**Risks:** Sol may spam `declare_gate` to reset budgets → need gate change rate limit + require acceptance list change hash  
**Codex question:** MCP tool vs UserPromptSubmit DSL for declare_gate? (MCP is enforceable; DSL is lighter.)

---

### Track T2 — Automatic handoff capsule (v1.5/v1.6)

**Problem:** Handoff deny without a written capsule recreates d791 “compact then re-explore” at session boundary.

**Success criteria**

1. When handoff triggers, runtime **materializes** `~/.config/claudex/handoffs/<root_session>.md` (and optional `.json`) from:
   - last known declare_gate state
   - supervisor-gate counters
   - workflow_status snapshot if MCP reachable
   - compact count, transcript path, changed-path hints from recent Edit/Write tool names only (not bodies)
2. PreToolUse deny reason includes **absolute path** of capsule.
3. Optional: `claudex --from-handoff PATH` wrapper starts new session with UserPromptSubmit seed = capsule.
4. Capsule schema matches `CLAUDE.md` Compact instructions (recovery capsule fields).

**Design**

- Zero-model generation first (templates). Optional later: Luna summary of last K transcript events via existing read_thread selector **without** loading full JSONL into Sol.
- Never put secrets; reuse threadgraph redaction.

**Files:** `internal/handoff`, `cmd/claudex` wrapper (if exists outside repo — check install scripts), gate  
**Risks:** incomplete capsule if state was never declared → require minimum fields + “unknown”  
**Codex question:** Should handoff **block all tools** until capsule file exists on disk (gate writes it itself) — yes recommended.

---

### Track T3 — Context governor for Prompt-too-long (v1.6, highest residual thrash)

**Problem:** API returns “Prompt is too long” **before** tools; supervisor-gate cannot see it. d791 had multi-hour dead zones here.

**Success criteria**

1. Detect context pressure **before** blow-up using available signals:
   - transcript file size growth rate
   - optional Claude Code usage fields if present in hooks
   - consecutive large tool results (PostToolUse byte sizes if available)
2. At soft threshold (e.g. 70% of known window or 6MiB transcript): inject `CLAUDEX_CONTEXT_PRESSURE` → force compact or handoff path.
3. At hard threshold: deny tools that append large outputs (Playwright snapshot, Bash unbounded) **and** recommend `/compact` or auto-handoff.
4. Never silently drop user messages.

**Design options (pick one in review)**

| Option | Mechanism | Pros | Cons |
|---|---|---|---|
| T3-A | Transcript size governor only | simple, zero-model | coarse |
| T3-B | Hook on StopFailure / API error text | precise on “too long” | reactive, after pain |
| T3-C | Gateway counts request bytes on :8318 | true pre-send | only models via gateway; need careful streaming |

**Recommended:** T3-A + T3-B hybrid; explore T3-C later for Sol traffic.

**Files:** `supervisorgate`, maybe adapter  
**Risks:** false pressure → premature handoff; tune with canaries  
**Codex question:** Is gateway request-byte reject safer than client-side? (Probably yes for Sol-via-8318.)

---

### Track T4 — Semantic slice admission 2.0 (v1.5)

**Problem:** Keyword composite detection is necessary but brittle (false allow / false deny).

**Success criteria**

1. Path-set structural rules:
   - if write paths intersect both `thread-app/public/**` and `migrations/**` → deny
   - if paths span >2 top-level packages under `internal/` + `thread-app/` → deny
2. `done_condition` must name **one** verifier command (regex: single line, no `&&` chains of >2 commands) for write workers.
3. Optional: require `paths` directory ownership ≤ N files estimated via walk cap.
4. Rejection returns **suggested split** (2–3 slice skeletons) as structured data, not only a string.

**Design**

```json
{
  "result": "rejected",
  "reasons": ["composite_slice:..."],
  "suggested_slices": [
    {"slice_id": "...", "paths": [...], "done_condition": "go test ./..."}
  ]
}
```

Supervisor can re-admit without rethinking from zero.

**Files:** `admission.go`, `admission_domains.go`, tests from d791 worker objective text  
**Risks:** over-splitting increases coordination cost — OK vs max-turns waste  
**Codex question:** Should suggested_slices be zero-model templates only (yes)?

---

### Track T5 — Close the native Agent / codex-rescue loophole (v1.6)

**Problem:** d791 used Explore/Opus and codex-rescue outside MCP graph; workflow “tested” poorly; usage incomplete.

**Success criteria**

1. PreToolUse policy modes (settings):
   - `workflow_strict=1`: deny native `Agent` unless `subagent_type` in allowlist **or** tool is MCP claudex-flow
   - default: warn+count; strict for canaries
2. When Agent allowed, force graph event with resolved model (already partial).
3. Document: preferred path is MCP worker/specialist; native Agent is escape.

**Design**

- Matcher on PreToolUse tool name `Agent`.
- Deny message: call `mcp__claudex-flow__start_worker` or `explore_repository` instead.

**Risks:** breaks legitimate codex-rescue workflows user wants — make **opt-in strict** first  
**Codex question:** default soft or strict? Proposal: soft default, strict via env for unattended canaries.

---

### Track T6 — Worker turn economy (not MaxTurns++) (v1.6)

**Problem:** MaxTurns=10 kills fat slices; raising to 20 hides bad packets.

**Success criteria**

1. Keep MaxTurns=10 for start; allow **resume** to add turns only with new evidence (already), but track **cumulative turns** hard cap (e.g. 24) across resumes.
2. On max_turns blocked with partial `changed_paths`, auto-surface `resume_worker` template in tool error (already partially).
3. Add `estimated_turns` optional field: if Supervisor claims ≤5 and packet large, warn; if paths count > K, reject.
4. Optional **read-only scout worker** MaxTurns=4, write=false, cheaper model? (Terra already explore_repository — maybe enough.)

**Non-goal:** dynamic MaxTurns based on model mood.

**Codex question:** cumulative turn cap value?

---

### Track T7 — Route learning loop (v1.7)

**Problem:** outcomes JSONL is a graveyard; router still static heuristics.

**Success criteria**

1. `route-eval` gains **offline learner dry-run**: read last N outcomes, propose default flips with confidence.
2. Promotion requires:
   - ≥K accepted runs on same task family hash
   - non-inferiority: duration/tokens/human_correction within thresholds
   - explicit `claudex-flow route-promote --family X --confirm`
3. Router loads `~/.config/claudex/route-defaults.json` (versioned) only after promote.
4. Never auto-promote in MCP process without confirm file.

**Files:** `routeeval`, `router`, ledger format  
**Risks:** overfitting to tiny N; require K≥5 default  
**Codex question:** family hash definition (objective embedding vs manual tags)? Prefer **manual family tags** first.

---

### Track T8 — Luna compact quality canary (v1.5 measurement)

**Problem:** Luna exists; quality/latency unknown; d791 compact ~3min @ Sol era.

**Success criteria**

1. Offline script: take a frozen transcript segment + compact markers, send twice (if dual routing available) or compare Luna-only metrics:
   - duration_ms
   - post_tokens
   - rubric: recovery capsule fields present (objective, gate, paths, verification)
2. Store results in `outputs/compact-canary-*.json`.
3. Only then change default compaction model or effort inheritance (today effort preserved from Sol xhigh — may make Luna expensive).

**Open design issue:** adapter preserves thinking/effort from Sol request — **may need strip effort for compact** to save cost. Codex should review whether lowering compact effort is safe.

---

### Track T9 — Supervisor cost attribution (v1.7)

**Problem:** Thread usage shows Sol-only; child models undercounted; accepted-result cost incomplete.

**Success criteria**

1. PostToolUse / assistant hooks attach per-request usage when Claude exposes it.
2. Graph events for native Agent include model + tokens when present.
3. Usage UI: roles supervisor / worker / specialist / native_agent.
4. Keep subscription vs gateway cost as separate columns; no fake unified USD.

**Depends on:** what JSONL already contains (d791 had usage rows for Sol). Extend parser, not invent.

---

### Track T10 — Thread product (parallel, not blocking scheduler)

**Success criteria**

1. Collapse TaskCreate/TaskGet noise under a “supervisor choreography” group.
2. Surface worker blocked/max_turns with session_id deep link.
3. Show supervisor-gate counters live (optional API from local state via backfill).
4. “Handoff” badge when compact_count≥3.

**Non-goal:** remote execution from web.

---

### Track T11 — Bash / deploy side-effect budget (v1.6)

**Problem:** Playwright is gated; `wrangler deploy` loops and flaky test reruns are not.

**Success criteria**

1. Classify Bash commands: `deploy`, `test`, `build`, `other`.
2. Caps: deploy 3/Root (or 1/gate), same test fingerprint 3 (already partially via verify fingerprint).
3. Deny destructive git (`reset --hard`, `push --force`) always unless user override.

**Risks:** command classifier evasion (`wrangler` via npm script) — match argv substrings + package scripts later.

---

### Track T12 — Durable lane quarantine (v1.6)

**Problem:** quarantine dies with MCP process; next session retries broken Grok auth.

**Success criteria**

1. Persist quarantine to `~/.config/claudex/lane-health.json` with TTL + repair evidence.
2. `route_task` and admission read durable state.
3. `claudex-flow lane-health clear --tool X --canary-pass` after explicit probe.

---

### Track T13 — Orchestrator compression (ongoing)

**Problem:** long orchestrator.md increases every Sol turn cost.

**Success criteria**

1. Split into always-on ≤N tokens core + skill-like on-demand sections.
2. Measure prompt prefix size before/after.
3. Keep contract markers for doctor.

---

## 5. Recommended roadmap (sequenced)

```text
v1.4.1  T0 hardening (config, status, logs, tests)           [1–2 days]
v1.5    T1 sticky re-route + declare_gate budgets
        T4 admission 2.0 + suggested splits
        T8 Luna compact measurement (parallel)              [3–5 days]
v1.6    T2 auto handoff capsule + claudex --from-handoff
        T3 context governor (transcript + StopFailure)
        T5 strict Agent mode (opt-in)
        T11 Bash/deploy budgets
        T12 durable lane quarantine                         [1–2 weeks]
v1.7    T7 route promote (manual confirm)
        T9 usage attribution completeness
        T10 Thread UI collapse                              [parallel product]
v1.8+   gateway request-byte governor (T3-C)
        cloud find with service token (if product needs)
        only then consider lead-lane switch experiments
```

**Do not start with:** more models, MaxTurns increase, mandatory multi-agent panels, cloud Access bypass.

---

## 6. Frozen canary suite (must exist before claiming wins)

Create `config/canary-suite-v1.json` with **real repo tasks**, each with frozen acceptance:

| ID | Family | Acceptance | Expectation under v1.5+ |
|---|---|---|---|
| C-parse | single-domain worker | `go test ./internal/threadusage` | Worker admitted, ≤10 turns, completed |
| C-composite | multi-domain packet | start_worker | **rejected** composite_slice, 0 model calls |
| C-ui-budget | Playwright thrash script | 15 browser tools | deny ≤13th |
| C-verify-loop | same `go test` 4× | 4th | VERIFY_BUDGET deny |
| C-handoff | force 3 PostCompact | Edit | ROOT_HANDOFF deny + capsule file exists |
| C-resume | max_turns partial | resume_worker | continues same session_id |
| C-find-read | file:query | find→read | ≤1 GLM call, evidence sourced |
| C-route-direct | localized fix | route_task | supervisor_direct, no worker |

Run under `workflow_strict=1` where relevant. Record wall time, tokens, human interventions.

---

## 7. Threats / failure modes of the proposal itself

| Threat | Mitigation |
|---|---|
| Over-gating freezes useful work | T0 env knobs + T1 override + fail-open on gate crash |
| Sol spams declare_gate | rate limit + acceptance hash change required |
| Sticky re-route ignored via Bash | count Bash deploy/test in high-cost; T11 |
| Composite heuristic false deny | suggested_slices + unit corpus from real packets |
| Handoff capsule empty | gate self-writes minimum state machine dump |
| Measurement theater | canary suite required before v1.7 promote |
| Hook latency | keep supervisor-gate <50ms p99; no network |

---

## 8. File / package map (implementation geography)

| Area | Path |
|---|---|
| Gate | `internal/supervisorgate/` |
| Hooks install | `internal/configure/hooks.go` |
| Worker admission | `internal/mcpserver/admission*.go` |
| Worker runtime | `internal/mcpserver/worker.go` |
| Router | `internal/router/router.go` |
| Route outcomes | `internal/mcpserver/lifecycle.go` |
| Compact Luna | `adapter/model-filter-proxy.mjs` |
| CLI | `cmd/claudex-flow/main.go` |
| Orchestrator | `~/.config/claudex/orchestrator.md` (installed artifact) |
| Thread app | `thread-app/` |
| Install | `scripts/install-claudex-flow.sh` |

---

## 9. Codex review checklist (please answer explicitly)

### Architecture

1. Is **sticky re-route (T1)** the correct next hard control, or is **auto handoff (T2)** higher leverage?
2. Should `declare_gate` be MCP (enforceable) or prompt convention (cheaper)?
3. Is per-gate budget reset safe against gaming? What rate limit formula?
4. For Prompt-too-long, prefer transcript governor, StopFailure hook, or gateway byte limit first?

### Safety

5. Fail-open vs fail-closed on PreToolUse gate panic — confirm fail-open + log?
6. Any deny rules that could block emergency hotfix? Is one-shot override enough?
7. Composite path rules: false positive risk on monorepos?

### Product / measurement

8. Minimum K and metrics for route promotion (T7)?
9. Should Luna compact strip xhigh effort (cost) before quality canary completes?
10. Is opt-in strict Agent deny (T5) sufficient, or must default flip?

### Sequencing

11. Approve roadmap order T0→T1→T4→T2→T3… or reorder?
12. Which tracks should be **rejected** as premature?

### Non-goals

13. Confirm: no MaxTurns raise; no mandatory multi-model; no Access bypass.

---

## 10. Suggested Codex output format

Please return:

```text
## Verdict
overall: Approve with changes | ...

## Track decisions
| Track | Decision | Conditions |

## Reorder
...

## Concrete deltas to the proposal
1. ...

## Minimal v1.5 scope (must ship)
- ...

## Explicit rejects
- ...

## Open questions for human
- ...
```

---

## 11. Appendix A — d791 failure → control mapping

| d791 symptom | Control today | Next control |
|---|---|---|
| 100+ Playwright | budget 12 | per-gate + sticky re-route |
| 5× manual compact | stop at 3 | auto capsule + new Root launcher |
| Prompt too long ×4 | none | T3 governor |
| Grok max_turns 10 on mega slice | composite reject | admission 2.0 + suggested splits |
| Native Agent instead of MCP | soft high-cost count | T5 strict mode |
| Usage Sol-only | partial graph | T9 attribution |
| Worker connectors false error | fixed 0.4.1+ | keep regression tests |
| 7h wall / 0.78h active | lifecycle 4h | handoff earlier + context pressure |

## Appendix B — API sketches (informative)

### `declare_gate` (MCP, zero-model)

```json
{
  "gate_id": "ui-contract-tests",
  "acceptance": ["node test/frontend-contract.test.mjs passes"],
  "stop_condition": "all acceptance green; no further CSS without failure evidence"
}
```

### `ack_reroute` (MCP, zero-model)

```json
{
  "gate_id": "ui-contract-tests",
  "remaining_acceptance": ["mobile overflow fixed"],
  "worker_decision": "none",
  "hypothesis_change": "width constraint is in .thread-column not body"
}
```

### Handoff capsule JSON (file)

```json
{
  "schema": "claudex-handoff.v1",
  "from_root_session_id": "...",
  "objective": "...",
  "current_gate": "...",
  "acceptance": [],
  "changed_paths": [],
  "verification": [],
  "workers": [],
  "gate_counters": {},
  "residual_risks": [],
  "next_action": "..."
}
```

---

## 12. Author notes for Codex

- Luna compact **does** exist at gateway; do not re-litigate “compact always Sol” as current architecture fact. Do litigate **quality, effort inheritance, and measurement**.
- v1.4 is the first **hard Supervisor scheduler surface**; the largest remaining gap is **context overflow + handoff automation + sticky re-route**, not more specialist models.
- Prefer smaller version slices with canaries over a monorepo “v2 rewrite”.

**End of proposal.**
