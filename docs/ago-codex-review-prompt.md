# Codex review prompt — Ago

Paste everything below the line into Codex, in `/Users/ruirui/orca/projects/x`.

---

You have full access to this repository. **Do not modify, stage, commit, or push anything. Do not touch `outputs/**`. Do not `git reset`, `clean`, `restore`, or drop a stash** — two safety stashes on this branch must survive. You may build binaries into `/tmp`, run tests, and mutate a *copy* of the tree.

I want an adversarial review of Ago. Not a summary — I wrote the code, I know what it says it does. **I want you to reproduce failures, and to attack claims I have already made and believe.** Where you find nothing, say so plainly; a fabricated concern is worse than a short report.

## Start here

- `docs/ago-codex-handoff.md` — architecture, invariants, constraints, gates, next tasks, known risks
- `docs/ago-review-request.md` — the same system framed as a review request, with the competitive thesis in §9

Read both, then form your own view from the code. **Where the docs and the code disagree, the code wins and the disagreement is a finding.**

## What matters most: `--reset` and directory ownership

`internal/agoserve/state.go` decides what `ago demo --state DIR --reset` deletes. **This has been wrong four times.** Each version passed a review before the next reviewer found the hole, and all four failed the same way: provenance was *derived from an observation* of the filesystem instead of *recorded by the code that did the writing*.

- v1 observed a name's absence → `--state ~/myproject --reset` deleted `~/myproject/artifacts`
- v2 observed a set of names → a directory holding only `artifacts/report.pdf` is an ordinary project
- v3 observed emptiness once and never re-decided → `--state ./build`, `make`, `--reset` deleted `build/artifacts`
- v4 observed absence-then-presence across a 58 ms startup window, pinned to device+inode → a sync client or concurrent build lands in it; ext4 reissues inodes

v5 claims to fix this categorically: `os.Mkdir` is atomic, so **success is the proof**, and it writes `.ago-created` carrying the claim's random nonce inside each directory it creates. Reset removes a directory only with that sentinel and that nonce. The database is identified by its **contents**.

**Your job is to make v5 into a v6.** Concretely, try to get a valid `.ago-created` into a directory Ago did not create, or get `ResetState` to remove something Ago did not make. Build the binary and run it:

```bash
go build -o /tmp/ago ./cmd/ago
HOME=$(mktemp -d) /tmp/ago demo --executor fake --listen 127.0.0.1:0 --state <somewhere>
```

Angles I have already covered — beat them or move past them, don't re-report them as new:
symlinked `--state` (refused on both sides), symlinked ancestors (resolved), symlinked marker and symlinked sentinel (Lstat), copied/moved/restored marker (path + device/inode binding), forged marker magic, marker temp-file debris, a sentinel carrying another claim's nonce, a user's directory present at startup, a same-inode directory with no sentinel, high-risk paths (root, <2 segments, home, ancestor-of-home, git repo including worktrees where `.git` is a file).

Angles I have **not** covered and want you to take:
- **Concurrency.** Two `ago demo` on one `--state`. There is no state-directory lock. What corrupts, and can it produce a wrong deletion rather than just a broken database?
- **The `RemoveAll` inside a directory Ago owns.** A hostile or unlucky tree under `artifacts/` — a symlink swapped in mid-walk, a directory made unreadable, a mount point.
- **`isAgoDatabase`** (`state.go:~300`) scans the first 256 KB for `CREATE TABLE board_definitions`. Corrupt file, huge schema, a user's file that legitimately contains that string, a WAL without its database.
- **Filesystems.** If you can reach Linux/ext4, measure whether inode numbers are actually reused after `rm -rf dir && mkdir dir` in the same parent. I could not test this and it is listed as an unverified risk.

## Second: verify my verification

I claim **all fifteen safety rules in `state.go` are mutation-tested**. Check that claim by doing it yourself, on a copy:

```bash
cp -r /Users/ruirui/orca/projects/x /tmp/agomut && cd /tmp/agomut
# neuter one rule at a time, then:
env -u CLAUDE_CODE_CHILD_SESSION go test -count=1 ./internal/agoserve ./cmd/ago
```

A mutant that survives is a rule with no coverage. **Two traps that already invalidated my own runs, so avoid them:** a mutation sweep against an already-red suite proves nothing (every mutant looks caught — get to green first), and never build a binary while a sweep is rewriting the tree.

Also judge the tests themselves: for each test that claims to prove a safety property, would it still pass if the property were violated? Three tests in earlier versions proved less than they claimed — most instructively, the root-directory test asserted a substring that *both* refusal messages contained, so the rule could be deleted with the suite green.

## Third: the parts I have not stress-tested

- **`agoboardstore.Claim`** — one immediate transaction doing receipt check → readiness → slot counts → generation → token mint → attempt+lease → receipt. Is it actually free of TOCTOU? Can two schedulers duplicate a claim, exceed a concurrency slot, or double-claim on a retried command?
- **`agoscheduler` verification lifecycle** — `verifyPending`, `deferVerification`, `applyVerdict`, `abandonVerification`. Can a task get stuck in a state nothing can leave? That happened once already: evidence past its deferral bound was skipped forever, stranding the task in `verifying` while the supervisor read it as merely pending and spun.
- **`agoexec` scope boundary** — scope is checked before commands run and again after, with `agoworktree.Checkpoint`/`Restore` discarding anything a command wrote. Can model-authored commands still get a file into the patch?
- **`agoverify` ordering** — deterministic gates before model judgement, and a fail-closed verdict contract. Can a malformed verdict, an outage, or a fabricated citation produce an accept?
- **Credential handling** — `AGO_RELAY_API_KEY` must never reach stdout, an error, an event, the board, or the database. `agoredact` is the boundary. Trace it and try to leak it. Note `checkRelayHealth` deliberately prints only scheme+host of the base URL because the URL itself can carry a secret.

## Gates

These should all pass on the current tree. If any fails, that is a finding.

```bash
env -u CLAUDE_CODE_CHILD_SESSION go test -race -count=1 ./cmd/ago ./cmd/ago-server ./internal/agoserve ./internal/agodemo
env -u CLAUDE_CODE_CHILD_SESSION go test -count=1 ./...
env -u CLAUDE_CODE_CHILD_SESSION go test -count=3 ./internal/supervisorgate ./internal/workflow
go vet ./...
cd e2e/board && npx playwright test            # 11/11, uses locally installed Chrome
cd thread-app && npm test && npm run typecheck  # 36/36
```

Known and not yours: `CLAUDE_CODE_CHILD_SESSION` in the environment makes `internal/supervisorgate` fail spuriously — hence `env -u`. `internal/workflow`'s `TestExecuteWithoutVerifierIsUnverifiedAndSingleCall` gives a spawned process a 1 s wall-clock timeout and occasionally exceeds it under the full parallel run; it passes in isolation. Some files outside this work are not gofmt-clean.

## Fourth: the product bet

`docs/ago-review-request.md` §9 has the full framing. In short: Amp's Puck fans out agents and **compiles their feedback**; Ago fans out and **integrates work that has been proven to hold together**. Amp's Puck announcement makes no claim about verification at all — that absence is the whole basis of my thesis.

One thing to know before you judge it: **the pitch is currently ahead of the code.** I describe Ago as combining several agents' strengths, and it does not. `agoscheduler` has exactly one `Executor` and dispatches every task to it (`scheduler.go:41`, `:303`); `CapabilityTags` are validated by the planner and never consumed for dispatch; `internal/router` exists but the Ago path does not import it. What exists is per-**role** model selection — planner, executor, verifier as three environment variables. Three roles, not a swarm.

So: **which do I build next — capability routing (the swarm I claim and don't have), remote execution (what Amp has and I don't), or multi-repo?** Argue for exactly one, from what the code can support today, not from the pitch. Assume one month.

## Output

Rank findings by severity. For each: file and line, a concrete reproduction with the exact commands and inputs you ran, the impact, and **CONFIRMED** (you ran it) versus **PLAUSIBLE** (you reasoned it). Keep those two separate — I have been burned by a confident inference before.

End with the one thing you would fix first if you could only fix one.
