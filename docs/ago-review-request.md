# Review request: Ago

I'd like a hard, skeptical review of this system. You have no repository access, so everything you need is below, including the code that matters most. Please read it all before answering.

**What I want from you:** find what is actually wrong or fragile, not a summary of what I wrote. Be specific — name the file, the scenario, and the inputs. If an area is genuinely sound, say so plainly rather than inventing a concern to seem thorough. If you think a design decision is wrong, say what you'd do instead and what it costs.

The specific questions I want answered are in §8 (the system) and §9 (whether the bet is worth making at all). Everything before those is context.

---

## 1. What the system is

Ago takes one goal stated in Chinese and runs it to completion without the user relaying messages between steps. It decomposes the goal into a durable task graph, executes each task with an LLM inside an isolated git worktree, verifies the result independently, and promotes accepted work onto a git ref it owns.

The product claim is: **after the goal is stated, the number of manual messages is zero.** Only these interrupt the user:

- push / publish / deploy / anything that costs money
- destructive operations
- credentials
- irreversible product-direction decisions
- ambiguity the repository's own evidence cannot resolve
- budget, time, or permission limits exceeded
- automatic repair and bounded retry both exhausted

Go, ~30k lines, SQLite, single binary. Runs locally against any OpenAI-compatible endpoint.

**Demonstrated end to end:** one Chinese sentence → a 6-task DAG → 4 sequential write tasks each inheriting the previous integrated revision → independent verification of each → 0 human decisions. The user's own branch and working tree byte-identical afterwards; no credential in the database.

**One correction to that claim, because I made it wrongly before.** The resulting branch does pass the sample project's tests, but **Ago did not check that** — the end-to-end test ran `go test ./...` at the integrated revision afterwards. `ProjectGates` are planned, validated, and stored, and nothing executes them. Ago reports complete when every task has passed, which is not the same as the integrated result being sound. So the system verifies each part; it does not yet verify the whole.

---

## 2. Architecture

```
planner → scheduler → executor → verifier → integrator
              ↑                                  │
              └────────── SQLite work graph ─────┘
                          (single source of truth)
```

| Component | Owns | Explicitly cannot |
|---|---|---|
| `agoboardstore` | The SQLite work graph. `boards.board_json` is canonical; normalised columns are derived. | — |
| `agoboardprotocol` | Every state transition. `state_machine.go` is the only place a transition is decided. | — |
| `agoscheduler` | **The only authority that claims work.** Mints fencing tokens, dispatches, applies verdicts, integrates. | — |
| `agosupervisor` | Closes the loop: reviews stopped work, repairs, escalates. | Claim a task, mint a token, write a task row. It only issues legal protocol commands. |
| `agoexec` | Runs the model in an isolated worktree, produces evidence. | **Mark work done.** |
| `agoverify` | Deterministic gates, then one model judgement. | Be the same object as the executor (construction refuses it). |
| `agointegrate` | Promotes accepted work. | Write outside `refs/heads/ago/*`. Ever push. |
| `agoworktree` | Per-attempt detached worktrees, scope measurement, patches. | — |

Claiming is one SQLite transaction with `_txlock=immediate`: durable receipt check → readiness → concurrency slot counts → generation bump → fencing token mint → attempt + lease insert → receipt write. The intent is that a second scheduler, in this process or another, cannot duplicate a claim or exceed a slot limit, and that a lost reply cannot double-claim.

---

## 3. Invariants, each of which cost a real bug

These are stated as history because the history is the argument.

1. **The executor cannot self-certify.** The offline provider and the offline verifier were once one object with two ID strings, so "verification" accepted because the worker's own summary was non-empty. They are now separate types, and scheduler construction refuses a fused pair via reflection.

2. **Deterministic gates outrank model judgement**, and run first — see §5. A model that says "accept" while a required test failed is not honoured, and the judge is not consulted at all once a deterministic check has failed.

3. **Verification commands are not authors.** `go build` dropping a binary, a formatter rewriting a file — all discarded and reported as warnings. The change under review is exactly what the model proposed and what was scope-checked. Without this, `make` could write anywhere and have the result carried into the patch while the evidence named one in-scope file.

4. **Checks run with `HOME` outside the worktree.** It was the worktree, so a single `go test` left tens of thousands of build-cache and telemetry files in the tree, and the scope check — correctly — read every one as an out-of-scope write and failed honest work.

5. **Reading and writing are different permissions.** The file listing given to the model was built from the task's *write* scope, so a task allowed to change `greet_test.go` could not see `greet.go`, and the model correctly refused to invent assertions about code it had never been shown. Every honest task of that shape stopped and asked a person — the exact interruption the system exists to remove.

6. **The verifier sees the change, not just its hashes.** A criterion written in prose ("the README documents the new command") cannot be judged from a file digest. The patch is read back from the artifact store by digest.

7. **Worktrees are `--detach`.** `worktree add -b` writes real branches into the user's repository that `worktree remove` does not delete.

8. **The event stream projects an explicit allowlist.** Embedding the protocol event leaked every fencing token to any browser.

9. **Not every OpenAI-compatible endpoint delivers the system role.** A local agent-style proxy silently replaced it; the verifier's output contract was dropped on every call, so the model invented its own field names and omitted the verdict entirely. Instructions now travel in the user message as well.

---

## 4. The part I most want reviewed: `--reset` and directory ownership

`ago demo --state DIR --reset` deletes things. **This has been wrong four times, and each version passed a review before the next one found the hole.** All four failed identically:

> Provenance was **derived from an observation** of the filesystem instead of **recorded by the code that did the writing.**

| | observed | how it failed |
|---|---|---|
| v1 | a name's absence — the marker was written whenever the sample repo was missing, true of any directory | `ago demo --state ~/myproject` then `--reset` deleted `~/myproject/artifacts` |
| v2 | a set of names — adopted any directory whose entries were all Ago's reserved names | a directory holding only `artifacts/report.pdf` is an ordinary project directory |
| v3 | emptiness, once, never re-decided | `--state ./build` before it exists, `make` fills it, `--reset` deleted `build/artifacts` |
| v4 | absence-then-presence across the 58 ms startup window, pinned to device+inode | a sync client or concurrent build lands in the window; ext4 reissues inodes freely |

### Current design (v5)

- Ago creates each of its directories with `os.Mkdir` — atomic, fails on EEXIST. **Success is the proof.** It then writes `.ago-created` inside, containing the claim's random nonce.
- `--reset` removes a directory only if that sentinel is present with that nonce.
- The board database cannot hold a sentinel, so its provenance is its **contents**: a SQLite file containing `CREATE TABLE board_definitions`.
- The state directory's marker `.ago-demo-state` binds by canonical path **and** device+inode, so a copied, moved, or restored marker authorises nothing.
- Claimable in exactly three cases: absent, genuinely empty, or already carrying a marker that still binds. Anything else is refused and **nothing is written into it**.
- Refusal is decided **before** any preflight; deletion happens **after** all of it, so a bad credential cannot destroy state on the way to reporting itself.
- Deny-list: filesystem root, fewer than two segments below the volume, the home directory, an ancestor of the home, a git repository (`.git` as file *or* directory, so worktrees and submodules count).
- A symlinked `--state` is refused on **both** the claim and the reset side. Symlinked *ancestors* are resolved (macOS `/var` → `/private/var`) and everything operates on the canonical path.

### The code

```go
const markerName        = ".ago-demo-state"   // in the state directory
const markerMagic       = "ago-demo-state-v2"
const ownedSentinelName = ".ago-created"      // inside each directory Ago creates

type entryID struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

type marker struct {
	Magic     string    `json:"magic"`
	CreatedAt time.Time `json:"created_at"`
	Path      string    `json:"path"`       // canonical path this marker was written for
	Directory entryID   `json:"directory"`  // that directory's identity
	Nonce     string    `json:"nonce"`      // 16 random bytes, hex
}

var reservedDirectories = []string{"greeter", "artifacts", "worktrees", "integration"}
var reservedDatabases   = []string{"ago.db", "ago.db-wal", "ago.db-shm"}
```

```go
// Creation IS the record. os.Mkdir is atomic and fails if the directory
// exists, so a directory somebody else made can never be mistaken for Ago's.
func CreateOwnedDirectory(state, name string) (bool, error) {
	path := filepath.Join(state, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		if os.IsExist(err) {
			return false, nil          // not ours; nothing is marked
		}
		return false, fmt.Errorf("创建 %s：%w", path, err)
	}
	if err := MarkOwnedDirectory(state, name); err != nil {   // writes .ago-created with the nonce
		return false, err
	}
	return true, nil
}

func ownsDirectory(state, name, nonce string) bool {
	if strings.TrimSpace(nonce) == "" {
		return false
	}
	path := filepath.Join(state, name, ownedSentinelName)
	info, err := os.Lstat(path)                    // Lstat: a symlinked sentinel vouches for nothing
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(content)) == nonce
}
```

```go
// readMarker refuses unless the marker still describes THIS directory.
func readMarker(state string) (marker, error) {
	path := filepath.Join(state, markerName)
	info, err := os.Lstat(path)
	if err != nil {
		return marker{}, fmt.Errorf("no ownership marker in %s", state)
	}
	if !info.Mode().IsRegular() {
		return marker{}, fmt.Errorf("marker is not a regular file")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return marker{}, err
	}
	var decoded marker
	if err := json.Unmarshal(content, &decoded); err != nil || decoded.Magic != markerMagic {
		return marker{}, fmt.Errorf("marker was not written by Ago")
	}
	if decoded.Path != canonicalDir(state) {       // copied or moved here
		return marker{}, fmt.Errorf("marker records a different directory (%s)", decoded.Path)
	}
	identity, ok := identityOf(state)              // device+inode via syscall.Stat_t
	if !ok {
		return marker{}, fmt.Errorf("cannot read filesystem identity of %s", state)
	}
	if !decoded.Directory.known() || decoded.Directory != identity {
		return marker{}, fmt.Errorf("this is no longer the directory the marker described")
	}
	return decoded, nil
}
```

```go
func ResetState(state, home string) error {
	resolved, err := CheckResetAllowed(state, home)   // pure: canonicalise, deny-list, marker
	if err != nil {
		return err
	}
	recorded, err := readMarker(resolved)
	if err != nil {
		return err
	}
	// Directories: only with this claim's sentinel.
	for _, name := range reservedDirectories {
		if !ownsDirectory(resolved, name, recorded.Nonce) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(resolved, name)); err != nil {
			return err
		}
	}
	// The database: only if its CONTENTS are Ago's board. wal/shm go with it,
	// never on their own.
	if isAgoDatabase(filepath.Join(resolved, "ago.db")) {
		for _, name := range reservedDatabases {
			if err := os.Remove(filepath.Join(resolved, name)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}
```

```go
// CanClaim is pure — a directory Ago will refuse must not first have a
// temporary probe file written into it.
func CanClaim(state string) (string, error) {
	resolved, err := statePath(state)     // refuses a symlinked --state; canonicalises ancestors
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(resolved)
	if os.IsNotExist(err) {
		return resolved, nil              // Ago will create it
	}
	if err != nil {
		return "", err
	}
	if OwnsState(resolved) {
		return resolved, nil
	}
	// Ago's own half-written markers (CreateTemp+rename debris from a crash)
	// do not count against emptiness, or a hidden file would make the
	// directory permanently unclaimable.
	remaining, first := 0, ""
	for _, entry := range entries {
		if isMarkerTemporary(entry.Name()) {
			continue
		}
		if remaining == 0 {
			first = entry.Name()
		}
		remaining++
	}
	if remaining == 0 {
		return resolved, nil
	}
	return "", fmt.Errorf("refusing to use %s as the demo directory: it is not empty and has no "+
		"Ago ownership marker (it contains %s)", resolved, first)
}
```

### Known residual, documented not fixed

A directory Ago created belongs to Ago **whole**. A file placed *inside* `artifacts/` goes when `artifacts/` goes. Only unrecognised entries at the **top level** of the state directory survive.

### How it is tested

Fifteen safety rules, each **mutation-tested**: mutate the rule to a no-op, confirm the suite fails. All fifteen are caught. Getting there exposed three tests that proved less than they claimed — most instructively, the root-directory test asserted a substring that *both* refusal messages contained, so the rule could be deleted with the suite green. One mechanism (rotating the nonce after a reset) survived mutation because it genuinely does nothing except risk orphaning Ago's own leftovers; it was deleted rather than tested.

---

## 5. Verification pipeline

Ordering is deliberate — deterministic checks are free and definitive, model judgement is expensive and arguable.

```
1. Required tests passed?          — a check that ran and failed is not a matter of opinion
2. Evidence non-empty?             — "I'm done" with nothing to show is not evidence
3. Artifacts present, digests match?
4. Changed paths inside declared scope?   — independent of the executor's own check,
                                            which ran inside the thing being verified
5. Patch consistent with the evidence?    — declared paths, base revision, digest
6. ... only now is model judgement worth paying for
```

The model verdict is then itself validated, fail-closed: every acceptance criterion must get exactly one entry matched by exact string; `passed: true` must cite evidence; every citation must be the evidence ID, a listed artifact ID, or a listed test name; `accept` while any criterion is `passed: false` is refused. A provider outage is `ErrUnavailable` — distinct from a rejection, so the worker is never re-run for someone else's downtime, bounded by a deferral limit after which the attempt fails as `exhausted` rather than stranding.

---

## 6. Autonomy: when a human is interrupted

```go
func escalationFor(task Task, authorize Authorization) (DecisionKind, bool) {
	switch task.FailureClass {
	case FailureAuth:            return DecisionCredential, true
	case FailureNeedsInput:      return DecisionAmbiguous,  true
	case FailurePolicy:
		if authorize.Destructive { return "", false }
		return DecisionDestructive, true
	case FailureRepository:      return DecisionAmbiguous,  true
	case FailureExhausted:       return DecisionExhausted,  true
	case FailureVerifierFeedback, FailureTransient, FailurePermanent:
		return "", false          // actionable: repair and retry
	default:
		return DecisionAmbiguous, true   // unclassified is escalated, not guessed
	}
}
```

Repair budget and retry counts come from the durable graph, not process memory, so a restarted supervisor reaches the same decision as the one it replaced. Retries wait on an injected clock rather than burning the step budget.

---

## 7. Risks I already know about

| Risk | State |
|---|---|
| ext4 inode reuse | v5 no longer uses inode identity for entry provenance, so reuse has no consequence. Whether ext4 actually reuses was never measured — no Linux available. APFS: 0 reuses in 2000 delete/recreate cycles. |
| Windows | `fileIdentity` returns not-ok on non-unix, so the marker cannot be written and `ago demo` cannot claim any directory. Fail-closed to the point of being non-functional. |
| Network mounts | Device numbers can change across a remount, so a valid claim can stop verifying. Fail-closed: refusal, never a wrong deletion. |
| No state-directory lock | Two `ago demo` on one `--state` will fight over the same SQLite database. |
| Relay E2E is opt-in | Not in the normal gate, deliberately: a flaky proxy or model drift would turn every commit red. |

---

## 8. What I want you to answer

1. **Is the v5 ownership model actually sound, or is it the same mistake in a new costume?** Four versions were signed off before the next reviewer found the hole. The claim is that "creation is the record" is categorically different from the four observation-based schemes. Attack that claim. Where can a `.ago-created` sentinel with a matching nonce end up in a directory Ago did not create?

2. **The nonce is stored in a `0600` file in the same directory tree as the sentinels it authorises.** Is that a real weakness? Who is the threat model — a confused user, a racing process, or a local attacker with read access? Does it change your answer that the marker is not secret from the user themselves?

3. **Do you agree with the residual I chose to document rather than fix** — that a file inside `artifacts/` is deleted with it? What would fixing it cost, and is that cost worth paying?

4. **`isAgoDatabase` decides provenance by scanning the first 256 KB for `CREATE TABLE board_definitions`.** Is that adequate, and what does it get wrong? Consider a corrupt database, a very large schema, and a user's file that legitimately contains that string.

5. **The claim/reset asymmetry**: a symlinked `--state` is refused, symlinked ancestors are resolved. I chose that because refusing ancestors refuses most real machines. Is that the right line?

6. **Is the deterministic-then-judgement ordering in §5 right**, and is there a class of acceptance criterion it handles badly? Specifically: is fail-closed on a malformed model verdict correct, or does it make an outage indistinguishable from a bad model?

7. **Is the autonomy boundary in §6 the right one?** Which of those failure classes would you move, and is "unclassified is escalated" the right default or an excuse for incomplete classification?

8. **Where is this system most likely to fail in a way none of the above anticipates?** That is the question I actually care about.

Please rank your findings by severity, give a concrete failure scenario with specific inputs for each, and separate what you traced through the code shown from what you are inferring.

---

## 9. Competitive framing — and where my own claim is not yet true

I am building this because I think it can be better than [Amp](https://ampcode.com), whose July 2026 release [Puck](https://ampcode.com/news/meet-puck) is the closest thing to what I want. **Please treat this section adversarially too — I am more interested in where my thesis is wrong than in confirmation.**

### What Amp ships today

- **Orbs** — remote sandboxes where agents run unsupervised. 1–16 CPUs, up to 32 GB, $0.10–$1.66/hour, billed by the minute.
- **Puck** — a conversational assistant and "home base for launching and coordinating other agents". You say *"For each script in ./scripts, spawn an agent in an orb to try and run it against the dev server, then compile their feedback about what worked"*, and it fans out and aggregates.
- Agents can spawn agents, message each other, and exchange files across threads.
- The interaction model is conversational; the user decides what to start and reviews the outcome.
- **The announcement makes no claim about verification, code review, or quality control.**

That last point is the whole basis of my thesis, so I want it challenged rather than assumed.

### My thesis

Puck fans out and **compiles feedback**. What I want is to fan out and **integrate work** — several agents' output combined into one product that has been proven to hold together. The four differences I believe are structural, not cosmetic:

1. **The centre is a durable graph, not a conversation.** Puck's coordination lives in a thread. Ago's lives in SQLite with fencing tokens, leases, exactly-once claims, and generations. Kill the process mid-flight and the graph resumes; a second scheduler cannot duplicate a claim. A conversation that is the coordinator cannot survive its own context window.

2. **Verification is a role, not a step someone remembers to take.** The executor structurally cannot mark its own work done — construction refuses a fused executor/verifier pair. Deterministic gates run before and outrank model judgement. "Compile their feedback" has no such property: the thing reporting success is the thing that did the work.

3. **Integration is a first-class stage.** Accepted work is promoted onto a ref Ago owns, and sequential write tasks inherit the previous integrated revision — so task 4 builds on the *verified* output of tasks 1–3, not on a stale base. Aggregating text is easy; aggregating code that still compiles and passes its own tests is where swarms usually fail.

4. **The autonomy boundary is defined, not emergent.** §6 lists exactly which failure classes interrupt a human. "You retain oversight of outcomes" is not a boundary, it is a disclaimer.

Plus a smaller one: this runs locally against any OpenAI-compatible endpoint. No per-hour VM. That is a real cost difference, and also a real capability difference in Amp's favour — see below.

### Where I am behind, stated plainly

- **Amp has remote execution infrastructure and I have none.** Orbs are a product; my worktrees are directories on one laptop. Anything that needs isolation stronger than a POSIX process, or more machine than the user has, I cannot do.
- **Amp is a shipped product** with distribution, a subscription, and models included. I have one demo fixture and a sample repository.
- **No multi-repo, no team features, no cross-agent messaging.**

### The part where my own pitch is currently false

I describe Ago as combining several agents' strengths — routing each kind of work to the model best suited to it. **That is not what the code does.** Concretely:

- `agoscheduler` has exactly **one** `Executor` and dispatches every task to it (`scheduler.go:41`, `:303`).
- `CapabilityTags` exist on every task and are validated by the planner, but nothing consumes them for dispatch — the scheduler never reads them.
- There *is* an `internal/router` package, from an earlier system. **The Ago path does not import it.**
- What genuinely exists is per-**role** model selection: planner, executor, and verifier can be three different models via three environment variables. That is three roles, not a swarm of specialists.

So today Ago is: one planner model, one executor model applied to every task, one verifier model — coordinated by a durable graph with real verification. The graph, the isolation, the verification, and the integration are built and tested. **The heterogeneity is not.**

I would rather you review the system that exists and tell me whether the missing piece is the important one, than review the pitch.

### Questions for this section

9. **Is my thesis actually a difference, or am I describing an implementation detail as an architecture?** Specifically: does a durable graph beat a conversational coordinator for real work, or does it just move the failure? Amp's bet seems to be that a good enough model with good enough tools does not need the ceremony. Where is that bet right?

10. **Is "verification as a structural role" worth what it costs?** It roughly doubles model calls per task and adds a whole failure surface (outages, malformed verdicts, deferral limits). Is there a cheaper mechanism that gets most of the benefit? Would you rather have one strong model self-reviewing than a weaker one policed by a separate call?

11. **Which missing piece would you build first** — capability routing (the swarm I claim and don't have), remote execution (the thing Amp has and I don't), or multi-repo? Argue for one, not a list. Assume I can do exactly one well in the next month.

12. **Is "integration is a first-class stage" the real moat, or the hardest thing to get right and therefore the worst place to plant a flag?** Sequential write tasks inheriting verified revisions works for a 6-task DAG on one repository. What breaks at 50 tasks, at conflicting concurrent writes, at a task whose acceptance depends on another's runtime behaviour rather than its diff?

13. **What is Amp likely to ship next that would make this project pointless?** Be concrete. If the answer is "verification gates on Puck's fan-out", say so, and say how long I have.
