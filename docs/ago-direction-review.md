# Codex direction review — Ago

Paste everything below the line into Codex, in `/Users/ruirui/orca/projects/x`.

---

You have full access to this repository. **Read only** — do not modify, stage, commit, or push anything, do not touch `outputs/**`, and do not `git reset`, `clean`, `restore`, or drop a stash.

This is **not** a bug hunt. I do not want another security audit; the code has had several. I want you to tell me whether I am **building the right thing, in the right order**, and what comes next.

Answer as someone who has seen projects like this fail. If the honest answer is "the direction is wrong" or "you are polishing the wrong thing", say that.

## What Ago is trying to be

A user states one goal. Ago decomposes it into a durable task graph, executes each task with an LLM in an isolated git worktree, verifies each result **independently**, and integrates accepted work onto a git ref it owns — with **zero manual messages after the goal is stated**, interrupting only for: publishing/spending money, destructive actions, credentials, irreversible product decisions, unresolvable ambiguity, exceeded limits, or exhausted automatic repair.

The bet, versus [Amp](https://ampcode.com) whose [Puck](https://ampcode.com/news/meet-puck) is the nearest thing: Puck fans agents out and **compiles their feedback**; Ago fans out and **integrates work that has been proven to hold together**. Amp's Puck announcement makes no claim about verification at all. Amp also has real remote sandboxes (orbs, $0.10–$1.66/hour) and I have none.

## Read these

- `README.md` — the vision, written before most of the code
- `docs/ago-codex-handoff.md` — architecture, invariants, what each component may not do
- `docs/ago-review-request.md` §9 — the competitive framing and where the pitch outruns the code
- `internal/agoscheduler/`, `internal/agosupervisor/`, `internal/agoverify/`, `internal/agoboardstore/` — the parts that carry the thesis

## The facts you need, measured, not claimed

**The README promises four pillars. One is built.**

| Pillar | Reality |
|---|---|
| Amp-like **capability routing** — best model per kind of work | **Not built in the Ago path.** `agoscheduler` has exactly one `Executor` and dispatches every task to it (`scheduler.go:41`, `:303`). `CapabilityTags` are on every task, validated by the planner, and never read by the scheduler. An `internal/router` package exists from an earlier system; the Ago path does not import it. What exists is per-*role* model selection: planner/executor/verifier as three env vars. |
| RepoPrompt-like **context engineering** | **Not built.** No context-package assembly; the executor gets a bounded listing of the worktree. |
| Linear-like **board orchestration** | **Built.** Durable SQLite graph, fencing tokens, leases, exactly-once claims, generations, retry with backoff, an autonomous supervisor, a live UI. |
| GitHub Issues-like **durable work items** | **Built.** Identity, contract, acceptance criteria, attempts, evidence, artifacts, history. |

**Where the effort actually went.** Recent commits, by size:

```
eb8f602  durable integration + real relay planner      2643 +
5697cfd  structural verifier independence              2769 +
70cc05d  PlanPatch + autonomous supervisor             2262 +
f7b0eb1  isolated real-model executor + worktrees      1886 +
831a5de  D10.1  --reset safety + unified CLI           1960 +, 645 -
1f58b47  D10.2  ownership as a binding                 1225 +, 313 -
574b664  D10.3  provenance by creation record           603 +, 184 -
```

**The last three commits — roughly 4,000 lines — are all one thing: making `--reset` on a demo command unable to delete a user's files.** It took five iterations because four of them were subtly wrong, each caught by a review after the previous one was signed off. The demo is unreleased and has no users.

**Verified end to end:** one Chinese sentence → 6-task DAG → 4 sequential write tasks each inheriting the previous *verified* revision → independent verification of each → 0 human decisions. That is one goal, one repository, one sample fixture, one time. The branch does pass the fixture's tests — but the end-to-end test checked that, not Ago.

**Not built:** remote execution, multi-repo, team/multi-user, cross-agent messaging, cost/budget accounting, any CI, `ago version`, an installer, `ago doctor`.

## What I want you to answer

**1. Is the thesis a real difference, or am I calling an implementation detail an architecture?**
Durable graph over conversational coordinator; verification as a structural role; integration as a first-class stage. Amp's implicit bet is that a strong enough model with good enough tools does not need this ceremony. **Where is Amp's bet right?** If a frontier model in 12 months makes independent verification redundant, what is left of Ago?

**2. Was the last month well spent?**
Five iterations on `--reset` safety for an unreleased demo, versus zero progress on the routing that the README leads with. Two defensible readings: (a) a tool that deletes user data must be trustworthy before anything else, and the discipline learned there is the product's real character; (b) I gold-plated a demo flag while the differentiating feature stayed unbuilt. **Which is it, and what should I have done instead?** Be blunt.

**3. Is the vision/code gap the right gap, or is the vision wrong?**
Three of four README pillars are unbuilt. Either the README is a roadmap I am behind on, or it is aspirational marketing that should be cut down to what the system actually is. **Which pillars are load-bearing for the thesis and which are decoration?** I would rather delete a pillar than fake it.

**4. What is the next thing, and why that one?**
Candidates, roughly ordered by my own uncertainty:
- **Capability routing** — the swarm I claim and do not have. Makes the README true. Risks being a feature nobody asked for.
- **Remote execution** — what Amp has and I do not. Unlocks unsupervised parallelism. Large infrastructure cost for one person.
- **Multi-repo / larger goals** — the demo is one fixture; nothing proves this survives a real codebase.
- **Distribution** (`ago version`, installer, `ago doctor`, CI) — cheap, unglamorous, and the difference between a project and a thing people can run.
- **More proof** — run it against genuinely hard goals and publish what breaks.

**Argue for exactly one.** Not a sequence, not a list. Assume one month of one person, and that anything not chosen slips a quarter.

**5. What breaks first when this meets reality?**
It has completed one goal on one 6-file fixture. What is the first thing that falls over at 50 tasks, on a 200k-line repository, with tasks whose acceptance depends on another task's runtime behaviour rather than its diff, or with two write tasks that genuinely conflict? Name the specific mechanism that fails, from the code, not in general terms.

**6. What would make this pointless?**
Concretely: what could Amp — or Claude Code, or Cursor — ship in the next two quarters that would make Ago not worth continuing? If the answer is "verification gates on Puck's fan-out", say so and say how long I have. If you think it is already pointless, say that; I would rather hear it now.

**7. If you were me, would you keep going?**
Answer this one last, after the others, and mean it.

## How to answer

Be concrete and cite the code where it matters. Do not summarise what I wrote back to me. Do not soften a negative conclusion to be encouraging — a wrong direction confirmed politely costs me a quarter. If you disagree with my framing of a question, answer the question you think I should have asked and say why.
