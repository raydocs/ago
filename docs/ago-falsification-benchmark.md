# Falsification benchmark — design, frozen before any code changes

**Status: proposed. Freeze this before building the project gate.**

The order matters. Once the gate exists it becomes very easy to adjust the
question until the gate passes it, and that is exactly the failure this
document is written to prevent. So the question is fixed here, in advance, by
someone who does not yet know the answer.

---

## 1. What is being tested

Not "does Ago work". The claim under test is the one the whole project rests
on, and it is falsifiable:

> **Verified integration measurably reduces false completion.**
>
> Against the same model on the same goal in the same repository, Ago reports
> "done" on a result that fails the project's own gate **less often** than a
> single agent does — and it does so without needing more human messages.

If that is false, the durable graph, the independent verifier, and the
integration stage are ceremony. Everything else Ago does — isolation, crash
recovery, audit — is real but commodity, and the project should stop being a
product and become a library.

### The three outcomes, decided now

| Outcome | Meaning | What I do |
|---|---|---|
| **Ago wins** — false-completion rate at least 30 points lower than the control, with human messages ≤ control | The thesis holds | Continue; build routing next |
| **Tie** — within 15 points either way | The ceremony is not paying for itself yet | Continue one more month **only** on the specific failure mode the data names. No new features. |
| **Ago loses** — equal or higher false completion, or needs more human messages | The thesis is wrong as built | Stop the product. Keep the code as a library for durable task execution and safe integration. |

30 and 15 are chosen before seeing data and do not move. If the sample is too
small to distinguish them, that is a "tie" and I say so.

---

## 2. The rule that makes this a falsification and not a demo

**Once §3–§6 are frozen, the goals, the repositories, and the gates do not
change.** Not after a failure, not to "make it fairer", not because a goal
turned out to be ambiguous. Ambiguity in a goal is data — real users write
ambiguous goals.

What I *may* change after a failure: Ago's code. What I may **not** change: the
question, the repositories, the gates, the metrics, or the thresholds above.

If a goal turns out to be impossible for *both* arms, it is reported as such
and excluded from the rate — but only if both arms fail it, and the exclusion
is stated in the results with its reason.

---

## 3. Repositories

Three real, external, brownfield Go repositories. Requirements:

- 10k–200k lines, not written by me, not a tutorial or sample
- a green test suite that runs in under 5 minutes on this machine
- permissive licence, vendored or `go mod download`-able offline
- a mix I do not control: at least one with sparse tests, at least one with a
  non-trivial dependency graph

**Frozen at a specific commit**, recorded by SHA, cloned once, and used
read-only. Both arms start from the identical commit.

Chosen repositories and SHAs go here before the first run — **left blank
deliberately, to be filled in during setup, not during analysis.**

| # | Repository | Commit | Lines | Test command | Test runtime |
|---|---|---|---|---|---|
| A | | | | | |
| B | | | | | |
| C | | | | | |

---

## 4. Goals

Five goals per repository, fifteen total. Each written **before** looking at
whether Ago can do it, and each drawn from a category that a real user would
actually ask for:

1. **Add a small feature** with an observable behaviour (a flag, a subcommand,
   an option) — touches ≥2 files
2. **Fix a real defect** identified from the repository's own issue tracker or
   a failing edge case — the fix must be checkable by a test
3. **Add test coverage** for an untested exported function
4. **Refactor with behaviour preserved** — rename or restructure across ≥3
   files, no behaviour change
5. **Cross-cutting change** — one that requires two parts of the codebase to
   agree, e.g. a change to a type plus every call site

Category 5 is the important one: it is where task-level acceptance and
project-level truth come apart, and it is the category Ago should win if the
thesis is right. Categories 1–4 are there so a win in 5 cannot be dismissed as
cherry-picking.

Goals are written in Chinese, as the product intends, and recorded verbatim.

---

## 5. The gate — defined per goal, executed outside Ago

For each goal, before any run:

```yaml
goal_id: A-05
repository: A
commit: <sha>
objective: "<the Chinese sentence given to both arms>"
gate:
  build:  "go build ./..."
  test:   "go test ./..."
  vet:    "go vet ./..."
  # goal-specific proof that the thing was actually done, not just that
  # nothing broke. Written before the run.
  behaviour:
    - "go test ./... -run TestNewFlagRejectsEmptyInput"
    - "./bin/tool --new-flag '' 2>&1 | grep -q 'must not be empty'"
```

**The gate runs in a clean checkout of the candidate revision, by the harness,
after the arm reports done.** Not by Ago, not by the control agent, not in the
worktree either of them used. A gate that passes in a dirty tree proves
nothing.

`behaviour` is what stops the benchmark from rewarding "changed nothing, broke
nothing". A goal is only satisfied if the new behaviour is demonstrated.

---

## 6. The two arms

Same model, same endpoint, same repository, same commit, same goal text.

**Arm 1 — Ago.** `ago demo --executor relay --goal "<objective>"` against the
frozen repository. Runs to its own terminal state. Whatever revision it
promotes to `refs/heads/ago/*` is the candidate.

**Arm 2 — control, a single agent.** The same model, given the same goal and
the repository, in one loop with file read/write and shell, allowed to run the
project's tests itself and iterate. Same wall-clock and token ceilings as Ago.
Its final working tree is the candidate.

The control is deliberately the *strong* version — a real coding agent that can
test and fix, not a one-shot. Beating a weak control proves nothing. Note that
this is currently a **stronger executor than Ago's own**, which does one model
call and cannot iterate on test results; that asymmetry is part of what is
being measured, and if it is what loses, that is the finding.

**Ceilings, identical for both:** 30 minutes wall clock, and a token budget
recorded and capped at the same number. An arm that hits a ceiling is recorded
as "did not finish", which is **not** the same as a false completion — the
distinction is the point.

Three runs per goal per arm, because both arms are stochastic. 15 goals × 2
arms × 3 runs = 90 runs.

---

## 7. Metrics

Recorded per run, machine-readable, no judgement calls:

| Metric | Definition |
|---|---|
| **`claimed_done`** | The arm reported completion |
| **`gate_passed`** | The externally executed gate passed on the candidate revision |
| **`false_completion`** | `claimed_done && !gate_passed` ← **the headline** |
| **`honest_incompletion`** | `!claimed_done` — stopped, asked, or ran out |
| **`human_messages`** | Messages a person would have had to send. For Ago, the attention-queue decisions. For the control, every point it stopped and asked. |
| **`wall_clock_seconds`** | |
| **`output_tokens`** | From the relay |
| **`gate_failure_kind`** | build / test / vet / behaviour — which part failed |

**Primary comparison:** `false_completion` rate, Ago versus control, across all
90 runs and broken out by goal category.

**Secondary:** `human_messages`. A system that avoids false completion by
asking about everything has not solved the problem — it has moved it.

---

## 8. What must be built to run this

Deliberately small, and none of it changes Ago's behaviour:

- `bench/` — a harness that clones a frozen repo, runs one arm, captures the
  candidate revision, runs the gate in a clean checkout, and writes one JSON
  record per run
- the control arm: a single-agent loop against the same relay
- goal and gate definitions as YAML, one file per goal, committed **before**
  the first run
- a results table generator

**Not** part of this: the project gate inside Ago. That is step 2, and it comes
after. Building it first would mean measuring a system I have just changed for
the purpose of the measurement.

---

## 9. Threats to validity, written down before they can be excuses

- **My goals may be biased toward what Ago does well.** Mitigation: categories
  fixed in §4 before checking feasibility; category 5 chosen specifically
  because I expect it to be hard for both.
- **Three repositories is a small sample.** Accepted. This is a falsification
  attempt, not a paper. A clear loss is informative; a narrow win is not.
- **The control's quality depends on how well I write it.** Mitigation: it gets
  the stronger executor, and its prompt is committed before the runs and not
  tuned between them.
- **Model non-determinism.** Mitigation: three runs per cell; report the spread,
  not just the mean.
- **I will want Ago to win.** Mitigation: thresholds and stopping rule are in
  §1 and frozen. The gate runs outside Ago. The goals do not change after a
  failure.
- **Ago's current caps may make some goals impossible** — 32 tasks, 48 KiB of
  repository content per task, 3 attempts. If a goal is impossible for Ago
  because of a cap, that is a **loss, not an exclusion.** The cap is part of
  the system.

---

## 10. Timeline

| Week | |
|---|---|
| 1 | Choose and freeze repositories; write 15 goals and gates; commit them; build the harness |
| 2 | Build the control arm; smoke-run both arms on one goal per repository |
| 3 | Run all 90; no code changes to Ago during this week |
| 4 | Analyse, write up, decide against §1 |

The week-3 rule — **no changes to Ago while the runs are happening** — is what
keeps the result meaningful.

---

## 11. What I expect

Written now so it can be wrong later.

I expect Ago to lose on categories 1–3, where a single strong agent that can
run tests and iterate should simply be better than one model call with no
feedback loop. I expect it to win on category 5, if it wins anywhere, because
that is where task-level acceptance and project-level truth diverge. And I
expect the overall false-completion rate to be **worse than I would like**,
because Ago currently reports completion without checking the integrated
result at all — which is the very gap step 2 exists to close.

If Ago loses on 5 as well, the thesis is wrong and I stop.
