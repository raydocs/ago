# Review request: Ago

I'd like a hard, skeptical review of this system. You have no repository access, so everything you need is below, including the code that matters most. Please read it all before answering.

**What I want from you:** find what is actually wrong or fragile, not a summary of what I wrote. Be specific — name the file, the scenario, and the inputs. If an area is genuinely sound, say so plainly rather than inventing a concern to seem thorough. If you think a design decision is wrong, say what you'd do instead and what it costs.

The specific questions I want answered are in §8. Everything before that is context.

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

**Demonstrated end to end:** one Chinese sentence → a 6-task DAG → 4 sequential write tasks each inheriting the previous integrated revision → independent verification of each → 0 human decisions → an integration branch whose own `go test ./...` passes. The user's own branch and working tree byte-identical afterwards; no credential in the database.

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
