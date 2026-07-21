# Ago — handoff

Everything you need to continue this work without the conversation that produced it.

- **Repo** `https://github.com/raydocs/ago` — Go module `claudexflow`, Go 1.26
- **Working copy** `/Users/ruirui/orca/projects/x` (the only one; do not mirror it elsewhere)
- **Branch** `feat/fresh-mac-install-kit`, at `574b664`, identical to `origin/master`
- **Last commit** `574b664 D10.3: record what was created instead of inferring it`

---

## 1. What Ago is

A user states one goal in Chinese. Ago turns it into a durable task graph and runs it to completion: decompose, schedule, execute each task in an isolated git worktree, verify independently, promote accepted work onto its own git ref. After the goal is stated, **the number of manual relay messages must be zero.**

Only these interrupt the user:

- push / publish / deploy / anything that costs money
- destructive operations
- credentials
- irreversible product-direction decisions
- ambiguity the repository's own evidence cannot resolve
- budget, time, or permission limits exceeded
- automatic repair and bounded retry both exhausted

Everything else — reading a report, judging whether to believe it, writing a repair instruction, saying "continue" — is mechanical and is done by the supervisor.

### Verified end to end

`ago demo --executor relay` against a real model (via a local OpenAI-compatible relay): one Chinese sentence → 6-task DAG → 4 sequential write tasks, each inheriting the previous integrated revision → independent verification of every one → **0 human decisions** → an integration branch whose own `go test ./...` passes. The user's `main` branch and working tree byte-identical; no credential anywhere in `ago.db`.

---

## 2. Hard constraints

These are standing and non-negotiable.

| | |
|---|---|
| **Never** `git reset` / `clean` / `restore`, never drop a stash | Two safety stashes exist on this branch and must stay |
| **Never** touch or commit `outputs/**` | Untracked deliverables the user owns |
| API keys come from the **environment only** | A flag lands in shell history and the process list. `AGO_RELAY_API_KEY` is not accepted as a flag and must never become one |
| Ago **never pushes** the user's repository | Accepted work goes to `refs/heads/ago/*` only |
| Read/write scope is per task | The executor may read the whole worktree; it may only write the task's declared `PathScopes` |
| No `A-22 Context Package`, no Linear, no webhooks, no external journal | Out of scope by decision |
| Concurrent writers must not touch `protocol.go`, `state_machine.go`, `store.go`, `runtime.go` simultaneously | Serialise edits to these |

---

## 3. Architecture

One goal flows: **planner → scheduler → executor → verifier → integrator**, all mediated by a durable SQLite work graph. No component may do another's job.

```
cmd/ago            user entry point.  `ago demo` + the pre-existing daemon/client
cmd/ago-server     compatibility + development entry point (`serve`, `demo`)
     │  both link internal/agoserve — there is exactly ONE orchestration wiring
     ▼
internal/agoserve  Serve(Config) builds the whole stack; Demo() is the demo command
     │
     ├── agoboardstore     SQLite work graph. THE single source of truth.
     │                     boards.board_json is canonical; all normalised columns
     │                     are derived by syncProjection. Claim() does receipt
     │                     check → readiness → slot counts → generation → token
     │                     mint → attempt+lease → receipt in ONE immediate txn.
     ├── agoboardprotocol  SchemaVersion 3. Commands, states, failure classes,
     │                     fencing tokens. state_machine.go is the ONLY place a
     │                     transition is decided; recordAttemptFailure is the
     │                     single retry decision point.
     ├── agoboardruntime   Goal → plan → board creation
     ├── agoscheduler      THE ONLY authority that claims work. Also verifyPending,
     │                     deferVerification, applyVerdict, integrate,
     │                     resumeIntegrating. Refuses a fused executor/verifier.
     ├── agosupervisor     Closes the loop without a human. Never claims a task,
     │                     never mints a token, never writes a task row — it only
     │                     issues legal protocol commands.
     ├── agoexec           Runs a real model against an isolated worktree, returns
     │                     evidence. CANNOT mark work done.
     ├── agoverify         Deterministic gate pipeline. Runs BEFORE any model
     │                     judgement and outranks it.
     ├── agorelayverifier  The semantic half — a separate model call, separate role
     ├── agorelayplanner   Real planner (produces a different DAG per goal)
     ├── agointegrate      Promotes accepted work onto refs/heads/ago/* only
     ├── agoworktree       Per-attempt detached worktrees, scope checks, patches
     ├── agoartifact       Content-addressed store; takes a byte stream, never a path
     ├── agorelay          OpenAI-compatible transport, with redaction
     ├── agoredact         Credential scrubbing at the durability boundary
     ├── agoboardapi       HTTP API (never mutates task state directly)
     ├── agoboardui        Same-origin UI
     ├── agodemo           The sample repository (a real Go CLI) + the demo goal
     └── agofake           Offline provider AND a SEPARATE offline verifier type
```

### Invariants that cost real bugs to learn

1. **The executor cannot self-certify.** `agofake.Provider` and `agofake.Verifier` are different types; `agoscheduler.New` refuses construction if executor and verifier are the same underlying object.
2. **Deterministic gates outrank model judgement.** A failed required test cannot be talked past. The judge is not even consulted when a deterministic check already failed.
3. **Verification commands are not authors.** `go build` dropping a binary, a formatter rewriting a file, a downloaded module — all discarded and reported. The change under review is what the model proposed and what was scope-checked. `agoworktree.Checkpoint` / `Restore` bound the command phase.
4. **Checks run with HOME outside the worktree.** HOME, GOCACHE, GOMODCACHE, XDG_CACHE_HOME, TMPDIR all point elsewhere, or a single `go test` leaves tens of thousands of cache files in the tree and the scope check correctly fails honest work.
5. **Reading and writing are different permissions.** The executor sees the whole worktree; it may only change the task's scopes. Before this, a task allowed to change `greet_test.go` could not see `greet.go` and correctly refused to invent assertions.
6. **The verifier sees the change, not just its hashes.** The patch is read back from the artifact store by digest. A criterion written in prose cannot be judged from a file digest.
7. **Worktrees are `--detach`.** `worktree add -b` writes real branches into the user's repository that `worktree remove` does not delete.
8. **SSE projects an explicit allowlist.** Embedding `agoboardprotocol.Event` leaked every fencing token.
9. **Not every OpenAI-compatible endpoint delivers the system role.** `agorelay` repeats system instructions inside the user message. A local agent-style proxy silently dropped them, and the verifier's output contract was ignored on every call.

---

## 4. The ownership model — read this before touching `--reset`

`internal/agoserve/state.go`. **This has been wrong four times and each version shipped past a review.** All four failed the same way:

> Provenance was **derived from an observation** of the filesystem instead of **recorded by the code that did the writing.**

| | what it observed | how it failed |
|---|---|---|
| v1 | a name's absence | `--state ~/myproject` then `--reset` deleted `~/myproject/artifacts` |
| v2 | a set of names | a directory holding only `artifacts/report.pdf` is an ordinary project |
| v3 | emptiness, once, never re-decided | `--state ./build`, `make`, `--reset` deleted `build/artifacts` |
| v4 | absence-then-presence over a 58 ms window, pinned to device/inode | a sync client or concurrent build lands in the window; ext4 reissues inodes |

**Current design (v5, `574b664`): nothing is inferred.**

- Ago creates each of its directories with `os.Mkdir` — atomic, fails on EEXIST. **Success is the proof.** It then writes `.ago-created` inside, carrying the claim's random nonce.
- `--reset` removes a directory only if that sentinel is present with that nonce.
- The board database cannot hold a sentinel, so its provenance is its **contents**: a SQLite file containing `CREATE TABLE board_definitions`.
- The state directory's own marker `.ago-demo-state` binds by canonical path **and** device/inode, so a copied, moved, or restored marker speaks for nothing.
- A directory is claimable in exactly three cases: absent (Ago creates it), genuinely empty, or already carrying a marker that still binds. Anything else is refused and **nothing is written into it**.
- Refusal is decided **before** any preflight; deletion happens **after** all of it. A bad credential can never destroy existing state on the way to reporting itself.
- Deny-list: filesystem root, fewer than two segments below the volume, the home directory, an ancestor of the home, a git repository (`.git` as **file or** directory — worktrees and submodules).
- A symlinked `--state` is refused on **both** the claim and reset sides. Symlinked *ancestors* are resolved (macOS `/var` → `/private/var`) and everything operates on the canonical path.

### API

```go
CanClaim(state) (canonical string, err error)      // pure; touches nothing
ClaimState(state) (canonical string, err error)    // creates + writes marker
CreateOwnedDirectory(state, name) (created bool, err error)
MarkOwnedDirectory(state, name) error              // only right after creating it
OwnsDirectory(state, name) bool
ResetOwnedDirectory(state, name) error             // refuses what Ago can't prove
CheckResetAllowed(state, home) (canonical, error)  // pure
ResetState(state, home) error
```

### The rule for changing any of this

**Every safety rule is mutation-tested and all fifteen are caught.** If you change `state.go`, re-run the sweep: mutate each rule to a no-op, confirm the suite fails. Three tests in earlier versions proved less than they claimed — one asserted a substring that *both* refusal messages shared, so its rule could be deleted with the suite green. **Assert the distinctive part of a message, never a shared one.** And when a mutant survives, first ask whether the mechanism does anything at all: one of them (rotating the nonce after a reset) was deleted rather than tested, because it had no effect except a bad one.

### Known residual, documented not fixed

A directory Ago created belongs to Ago **whole**. A file placed inside `artifacts/`, `worktrees/`, `integration/` or `greeter/` goes when that directory goes. Only unrecognised entries at the **top level** of the state directory survive. `docs/ago-quickstart.zh.md` §7 says this plainly.

---

## 5. Gates — run all of these before committing

```bash
cd /Users/ruirui/orca/projects/x

# Go. CLAUDE_CODE_CHILD_SESSION in the environment makes internal/supervisorgate
# fail spuriously — unset it.
env -u CLAUDE_CODE_CHILD_SESSION go test -race -count=1 \
    ./cmd/ago ./cmd/ago-server ./internal/agoserve ./internal/agodemo
env -u CLAUDE_CODE_CHILD_SESSION go test -count=1 ./...
env -u CLAUDE_CODE_CHILD_SESSION go test -count=3 ./internal/supervisorgate ./internal/workflow
go vet ./...
gofmt -l cmd internal            # some pre-existing files outside your scope are unformatted

# Browser
cd e2e/board && npx playwright test              # 11/11, uses locally installed Chrome
node --test unit/stream-model.test.mjs           # 7/7 — note: `node --test unit/` (directory
                                                 # form) exits 1 on this Node; use the file

# Thread app
cd thread-app && npm test && npm run typecheck   # 36/36, clean

# Hygiene
git diff --check
grep -rInE "sk-[A-Za-z0-9]{20,}" cmd internal docs README.md
```

### Opt-in, needs a provider — not in the normal gate

```bash
AGO_RELAY_BASE_URL=http://127.0.0.1:8317/v1 AGO_RELAY_API_KEY=... \
  go test -count=1 -run TestRealRelayCompletesAMultiTaskGoal -timeout 40m ./internal/agodemo
```

### Known flake, not yours

`internal/workflow` `TestExecuteWithoutVerifierIsUnverifiedAndSingleCall` gives a spawned process a 1 s wall-clock timeout. Under the full parallel `./...` run it occasionally exceeds it. Passes 5/5 in isolation and under the `-count=3` gate. That package is untouched by this work.

### Two process traps I fell into — don't repeat them

1. **Never run a mutation sweep and a manual build at the same time.** The sweep rewrites `state.go`; a binary built during it is not the code you think it is. I produced a bogus "finding" this way.
2. **A mutation sweep against a suite that is already red proves nothing** — every mutant looks caught. Get to green first.

---

## 6. Next tasks, in priority order

Relay CI is deliberately **not** first. Do not put a networked relay call in the normal PR gate: a flaky proxy, model drift, cost, or a missing secret would turn every commit red.

### 6.1 `ago version` and build-time version injection

There is no version command and no version anywhere in the binary today.

- `ago version` prints version, git commit, build date, Go version, OS/arch
- Injected via `-ldflags -X`, with honest fallbacks when built without them (`dev`, `unknown`) — never a fabricated version
- `--version` / `-v` on the root command too
- The `demo` startup block should print the version, so a bug report identifies the build
- Test: build with and without ldflags, assert both outputs; assert the unset build does not claim a real version

Watch `cmd/ago/dispatch()` — flags starting with `-` route to the daemon, bare words to the client, plus the `daemon` and `demo` cases. `TestDispatchStillRoutesDaemonAndClient` guards this; extend it rather than replacing it.

### 6.2 Install script for macOS and Linux

- `curl … | sh` style, or a `Makefile` target — decide and state why
- Detects OS/arch, installs a versioned binary, verifies a checksum
- Never writes outside a directory the user names or an obvious default; refuses to overwrite something it did not install (**the same ownership discipline as `--reset` — reuse the thinking in §4**)
- Prints exactly what it did and how to undo it
- Test from a fresh temporary HOME, like `TestDemoIsReachableFromTheUnifiedCLI` does

### 6.3 `ago doctor`

Everything `preflight` in `internal/agoserve/demo.go` already checks, as a standalone command that reports rather than refuses:

- `git` and `go` present, with versions
- state directory: exists, writable, claimed by Ago or not, what `--reset` would remove
- relay: `AGO_RELAY_BASE_URL` set, `AGO_RELAY_API_KEY` present (**never print its value**), one real bounded health call, per-role model names
- board database: present, readable, schema version, board count
- port availability
- exit non-zero when something is actually broken; `--json` for machines

Reuse `preflight()`; do not fork a second copy of these checks.

### 6.4 Normal CI with a keyless local relay fixture

An in-process OpenAI-compatible HTTP fixture that serves canned responses. It must exercise the **contract**, not the models:

- planner returns a plan; malformed plans are rejected
- executor/verifier/planner are three separate calls with three separate roles
- verifier fail-closed semantics: malformed JSON, missing verdict, unknown verdict, criteria coverage mismatch, fabricated citations, accept-contradicting-its-own-criteria
- `UnavailableError` vs `InvalidVerdictError` — an outage must not be recorded as a rejection
- **the system-role-dropping proxy**, since a real one broke this and the fixture is the only place that stays tested

`internal/agorelay` already has `server.go`; check whether it can host this before writing a new one.

### 6.5 Real relay smoke — manual or nightly only

- protected GitHub Environment secrets; **fork PRs never receive credentials**
- bounded budget, timeout, and concurrency
- runs `TestRealRelayCompletesAMultiTaskGoal`
- failure notifies, never blocks a merge

---

## 7. Unresolved risks

| Risk | State |
|---|---|
| **ext4 inode reuse** | The v5 design no longer uses inode identity for entry provenance, so reuse has no consequence. But whether ext4 actually reuses was never measured — no Linux available. APFS: 0 reuses in 2000 delete/recreate cycles. |
| **Windows** | `fileIdentity` returns not-ok on non-unix, so `writeMarker` errors and `ago demo` cannot claim any directory. Fail-closed to the point of being non-functional. Path rules are code-aligned but never run on Windows. |
| **Network mounts** | Device numbers can change across a remount, so a valid claim can stop verifying. Fail-closed: a refusal, never a wrong deletion. |
| **No state-directory lock** | Two `ago demo` on one `--state` will fight over the same SQLite database. |
| **`--reset` inside Ago's own directories** | Documented in §4, deliberately not engineered away. |
| **`internal/supervisorgate`, `internal/workflow`** | Pre-existing, untouched. See the environment-variable and flake notes in §5. |

---

## 8. Commit history for this line of work

```
574b664  D10.3: record what was created instead of inferring it
1f58b47  D10.2: make ownership a binding instead of a flag
831a5de  D10.1: earn the right to delete, and make `ago demo` the one entry point
6d47613  D10:   one command that works on a machine which has never run Ago
e729559  Part 4: a real Chinese goal reaches an integrated, tested revision
5697cfd  feat: make the verifier structurally independent, and add relay mode
eb8f602  Part 1/2: durable integration + real relay planner
444c149  supervisor in ago-server + attention queue
f7b0eb1  D8 isolated executor + worktrees
70cc05d  D9/D11/D12 PlanPatch + supervisor + zero-relay
53c1158  Playwright E2E + 2 UI bug fixes
```

Read the commit messages. They record *why*, including the mistakes — which is the part that keeps getting re-learned.

---

## 9. Docs

- `README.md` — vision, plus a three-line Try it
- `docs/ago-quickstart.zh.md` — the 10-section Chinese walkthrough; §7 is the `--reset` safety contract and must stay true to `state.go`
- `docs/ago-v0-product-contract.md`, `docs/ago-usable-demo-delivery-plan.md` — earlier product framing
- `CLAUDE.md` — workspace boundary and compaction rules
