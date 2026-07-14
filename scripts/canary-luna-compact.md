# T8 Luna compact effort canary (measurement only)

Do **not** lower compact effort until this canary is green.

## Fixed transcript

Use a frozen compact boundary + preceding messages from a real Root (e.g. d791 compact #1).

## Matrix

| Model | Effort | Duration | Post tokens | Capsule fields present? |
|---|---|---|---|---|
| gpt-5.6-luna | xhigh | | | objective/gate/paths/verify |
| gpt-5.6-luna | high | | | |
| gpt-5.6-luna | medium | | | |

## Commands

```bash
# Adapter unit tests (routing only, no paid Luna calls)
node --test adapter/model-filter-proxy.test.mjs

# Ensure gateway routes Sol compact markers → Luna
# Then run one real compact under each effort via controlled session.
```

## Gate to change effort

high/medium must be non-inferior on:

1. recovery capsule field coverage
2. path/command factual retention (spot check ≥5 facts)
3. p50 latency ≤ xhigh * 1.2 (optional)

Until then: keep effort inheritance from the Sol compact request.
