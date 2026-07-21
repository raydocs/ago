# Codex review — the project gate

Paste everything below the line into Codex, in `/Users/ruirui/orca/projects/x`.

---

You have full access to this repository. **Read only** — do not modify, stage, commit, or push anything, do not touch `outputs/**` or `AGENTS.md`, and do not `git reset`, `clean`, `restore`, or drop a stash. You may build into `/tmp` and run tests.

A previous direction review found the most serious thing wrong with this system, and I have just tried to fix it. **I want you to check whether I actually fixed it, or whether I moved it.**

## What the review found

> No project gate on the final integrated result. The integrator applies the patch and commits without running tests; the supervisor declares completion as soon as every task has passed. The final `go test ./...` was **test code, run after Ago had already finished** — not a product mechanism.
>
> The first failure mode is therefore **false green**: task-level acceptance was being treated as project-level completion.

That was correct. `ProjectGates` were planned, validated, persisted, and executed by nobody. I had also repeatedly reported "the integration branch passes its tests" as evidence the system worked, which was an overclaim — the test checked it, Ago did not.

## What I changed

Commits `b0b98a2` and `63d5138`. Start from `git show --stat` on both, then read:

- `internal/agogate/` — discovery and execution of the checks
- `internal/agoboardprotocol/protocol.go` — `ProjectGate`, `GateState`, `GateSpec`, `BoardSpec.GateCommands`, `SchemaVersion` 3 → 4
- `internal/agoboardprotocol/state_machine.go` — the `gate.pass` / `gate.fail` transitions and `newProjectGate`
- `internal/agoboardstore/store.go` — `Completion` now consults the gate
- `internal/agoscheduler/scheduler.go` — `runProjectGate`
- `internal/agosupervisor/supervisor.go` — `status()`, `reviewFailedGate`, `repairForGate`
- `internal/agointegrate/integrator.go` — `Scratch`
- `internal/agoserve/` — where the gate is discovered and wired

The design in one paragraph: once nothing is outstanding, the scheduler runs the repository's own checks against the **integrated revision** in a throwaway checkout, and records the verdict durably. Completion requires that verdict, and a pass is bound to the revision it was recorded for. The commands are **discovered from the repository or supplied by the user, never proposed by a model** — letting the planner choose what would prove its own plan is the self-certification problem the independent verifier exists to prevent, one level up. A repository whose ecosystem is unrecognised gets an **absent** gate, which is not a pass.

## What I want you to attack

**1. Is the false-green actually closed, or only narrowed?** Find a way to make Ago report a goal complete when the integrated result does not pass its own checks. Consider: an absent gate, a gate that errors rather than fails, a revision that changes between the gate running and completion being read, a board with zero tasks, a read-only goal with no integration chain, `--reset` mid-run, two schedulers. Build the binary and try it:

```bash
go build -o /tmp/ago ./cmd/ago
HOME=$(mktemp -d) /tmp/ago demo --executor fake --listen 127.0.0.1:0
```

**2. `runProjectGate` swallows its own errors.** When the gate cannot run, it returns `nil` and leaves the state pending, on the reasoning that an unrunnable gate is like a verifier outage and should be retried rather than blamed on the work. Is that right? Can a permanently unrunnable gate now hang a goal forever with no decision raised and nothing in the attention queue? Compare against how verification exhaustion is handled (`abandonVerification`), which had exactly this bug once and was fixed by failing the attempt after a bound. **I think this is the most likely real defect in the change.**

**3. The repair heuristic.** `reviewFailedGate` retries the task that produced the rejected revision, with the failure output added to its acceptance. A gate can fail because of an interaction between changes rather than the last one. How badly does this behave when the culprit is an earlier task? Does it converge, thrash, or spend its budget on the wrong task? The budget is `MaxGateRepairs = 2`, counted as `Gate.Failures` on the board.

I know the better fix is a dedicated repair task and I did not do it: `plan.patch` adds a task to the board but **not to the plan definition**, so a patched-in task gets an empty `TaskProposal`, no `PathScopes`, and can edit nothing. Confirm that gap is real, and say whether fixing it is the right next move or whether the heuristic is good enough.

**4. Ordering and idempotence.** `runProjectGate` requires every task settled and skips when `Gate.Revision == revision`. `reviewFailedGate` additionally requires `Gate.Revision == IntegratedRevision` and no outstanding tasks, to avoid acting on a stale verdict. Are those guards sufficient? What happens if the process dies between `update-ref` and the gate command, or between `gate.fail` and the repair patch?

**5. Command execution.** `agogate` refuses shell metacharacters and runs a fixed argv. Is the refusal list complete? What about an argument that is itself dangerous without any metacharacter, or a repository that ships a `go` binary earlier on `PATH`? Note the gate inherits `agoexec.SystemCommands`, which sets a scratch `HOME` and cache directories — check that the gate gets that too, and that a gate run cannot pollute the repository it is checking.

**6. Old boards.** `agoboardprotocol.SchemaVersion` went 3 → 4 with no migration written. Note this is the board-JSON version and is separate from `agoboardstore.CurrentSchemaVersion`, which governs the SQL schema — I want you to confirm I have not confused the two.

What I traced: a board stored before this change has no `gate` key, so it decodes to a zero-value `ProjectGate` whose `State` is the empty string rather than `GateAbsent`, and whose `Established()` is false. Everything that reads the gate goes through `Established()` or compares against `GateFailed`, so an old board should complete exactly as it did before. **Verify that, and find anything that compares `State == GateAbsent` and would therefore be wrong on the empty value.** There are stores from earlier runs under `~/.ago/demo` to test against.

**7. Is `Completion` and `status.Complete` now consistent?** They are two separate implementations of the same question, in `agoboardstore/store.go` and `agosupervisor/supervisor.go`. Can they disagree? If so, which one does the API report and which one stops the supervisor loop?

## Also worth your time

- `docs/ago-falsification-benchmark.md` — the experiment this gate is a prerequisite for, frozen before the code was written. **Is it actually falsifiable, or have I left myself an escape hatch?** The thresholds are in §1 and the anti-tampering rule in §2.
- `internal/agoscheduler/gate_test.go`, and the gate tests in `internal/agogate/gate_test.go`. For each, ask whether it would still pass if the property were violated. Earlier rounds on this codebase produced three tests that proved less than they claimed, so treat mine with suspicion.

## Gates

These pass on the current tree. A failure is a finding.

```bash
env -u CLAUDE_CODE_CHILD_SESSION go test -race -count=1 ./cmd/ago ./cmd/ago-server ./internal/agoserve ./internal/agoscheduler ./internal/agosupervisor ./internal/agogate
env -u CLAUDE_CODE_CHILD_SESSION go test -count=1 ./...
go vet ./...
```

`CLAUDE_CODE_CHILD_SESSION` in the environment makes `internal/supervisorgate` fail spuriously — hence `env -u`. `internal/workflow` has a pre-existing 1-second-timeout flake under the full parallel run.

## Output

Rank by severity. For each: file and line, a concrete reproduction with the commands you ran, the impact, and **CONFIRMED** (you ran it) versus **PLAUSIBLE** (you reasoned it) — kept separate.

End with one sentence: does completion now mean what it says?
