# Project X workspace boundary

The canonical working copy for this project is:

`/Users/ruirui/orca/projects/x`

Operational rules:

- Read, edit, test, and delegate only inside this workspace root.
- Use relative paths whenever possible.
- Do not create another staging mirror to bypass a workspace boundary.
- The older Documents copy is a temporary backup, not the active working copy.
- Do not copy secrets from `~/.config/claudex` or the older `work/thread-secrets.env` into this repository.
- Keep `work/`, local Wrangler state, dependencies, logs, and generated runtime state untracked.
- Before a write worker starts, give it explicit non-overlapping paths inside this root.
- If a tool reports that this root is out of scope, stop and report the scope error instead of copying the project elsewhere.

# Compact instructions

When compacting this project, preserve one concise recovery capsule containing only:

- the user's unchanged objective and frozen acceptance criteria;
- the current gate and the next concrete action;
- confirmed decisions, corrections, and approval boundaries;
- changed paths plus the latest deterministic verification/deployment evidence;
- active Worker IDs/session IDs, owned scopes, resolved models, and unresolved capability requests;
- quarantined lane health with its observed failure class and the repair evidence required;
- unresolved material risks or user-only blockers.

Drop raw logs, superseded plans, repeated discussion, speculative alternatives, hidden reasoning, and completed intermediate steps that no longer affect execution. Never turn an unverified claim into a fact during compaction. After compaction, re-anchor from current files and runtime state before continuing.

# Supervisor gate (v1.4)

Runtime enforces zero-model budgets on the Root Supervisor:

- Playwright/browser tools and screenshots are capped per Root; identical verification fingerprints max out at 3.
- After enough high-cost tools, a re-route check requires restating gate, acceptance remainder, and Worker ownership.
- After 3 compacts, 4h Root age, or ~8 MiB transcript, construction tools are denied until a handoff capsule is written and a new Root is started.
- Worker admission rejects composite multi-domain slices (e.g. migration+API+UI+usage); split domains instead of raising MaxTurns.
