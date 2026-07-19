# Ago Amp Rebuilt-Era Parity Matrix

Status: Product baseline and delivery map
Updated: 2026-07-19

Classification captures Ago's starting point against Amp's observable contracts
and identifies where each capability contributes to the intelligent board.

| Amp public milestone | Date | Observable contract | Ago baseline | Reusable current asset | Primary gap / target phase |
| --- | --- | --- | --- | --- | --- |
| Neo durable remote-controlled thread | 2026-05-06 | CLI thread remains addressable; web can observe, send, queue/dequeue, and cancel | partial | thread sync/archive and Root/Parent/Child graph | Ago does not yet own authoritative thread execution state; Phase 1–2 |
| Queue by default | 2026-05-06 | busy-thread messages persist in ordered queue without interrupting | missing | none | durable mailbox and deterministic ordering; Phase 1 |
| Steer and hard interrupt | 2026-05-06 | steer injects at next safe boundary; hard interrupt cancels and sends immediately | missing | bounded worker cancellation only | canonical safe-boundary and cancellation semantics; Phase 1 |
| Automatic 90% compaction | 2026-05-06 | same logical thread continues with summary while original history survives | partial | compact audit, guidance, transcript compact events | Ago does not own context projection or continuation; Phase 1 |
| Plugin API | 2026-05-06 | lifecycle hooks, tools, commands, UI requests, AI classifier, policy | missing | MCP tools and supervisor gates | Ago-owned lifecycle/plugin boundary; Phase 1 |
| Default automatic tool execution | 2026-05-06 | no per-tool prompts unless policy plugin is configured | partial | current launcher auto-allows; supervisor gates exist | move policy behind plugin interception; Phase 1 |
| 5,000-message runtime performance | 2026-05-06 | responsive large-thread TUI with reported CPU/memory reduction | unmeasured | current archive UI | establish Ago performance budget and benchmark; Phase 2 |
| Plugins on web/TUI | 2026-05-28 | serializable plugin UI synchronized across clients | missing | none | client-neutral UI requests and shared resolution; Phase 2 |
| Agents Everywhere | 2026-06-04 | one web/mobile/CLI view for all active durable agents | partial | Cloudflare archive UI and thread graph | current app is observation-first, not authoritative control; Phase 2 |
| Thread-bound diffs | 2026-06-16 | review any active environment's diff, comment on sections, stage changes | missing | UI visual studies only | environment-bound diff model and staging commands; Phase 2 |
| Custom agents | 2026-06-19 | agent definitions, one-shot run, persistent child thread, async append/wait | partial | routed Claude worker sessions and parent/child capture | children are not Ago-owned durable threads; Phase 3 |
| Managed orbs | 2026-06-30 | fresh isolated machine per thread, common controls, files/terminal/diff/sync | missing | no cloud executor | managed executor adapter and environment lifecycle; Phase 4 |
| Truth-preserving long-thread read | 2026-07-02 | search original events, inspect later revisions, distinguish attempts/results | partial | threadread/threadfind and immutable transcript parsing | retrieval must target Ago-owned full history and later-event semantics; Phase 5 |
| Agent-ready orb lifecycle | 2026-07-02 | prepared image, setup cache, setup/resume hooks, services, logs, guidance | missing | project setup guidance only | reproducible, resumable, observable cloud workspace; Phase 4 |
| Orb sizes | 2026-07-03 | selectable compute and disk profiles | missing | none | provider-neutral resource profiles and cost limits; Phase 4 |
| User-managed runners | 2026-07-08 | interactive/headless clients accept remotely created threads in startup directory | missing | no Ago daemon/runner registry | enrollment, heartbeat, lease, explicit placement; Phase 5 |
| Orb OIDC identity | 2026-07-14 | short-lived audience-bound identity with immutable thread/project claims | missing | no workload identity | OIDC issuer and relying-party contract; Phase 5 |
| Agent-to-agent | 2026-07-17 | spawn on local/runner/orb, authenticated messages and files across threads/projects | partial | parent/child relation and specialist workers | no durable child creation, reply route, executor choice, or file transfer; Phase 6 |
| Intelligent board orchestration | Ago differentiation | one user objective becomes a durable task/dependency graph that is assigned, monitored, retried, and verified by the board agent | partial | durable threads, routing, parent/child relations, mailbox, receipts, and verification evidence | make the board the coordination authority; give workers bounded context; prove dependency-aware scheduling, reassignment, and project-level completion; Phase 3 |
| Automatic capability routing | Ago differentiation | the board assigns each runnable task to the best available agent capability without requiring users to configure models | partial | catalog/router/routeeval/lane health/usage | integrate routing with board-owned assignments and prove against a single-model baseline; Phase 7 |

## Baseline conclusion

The repository began with meaningful routing, observability, transcript,
retrieval, and archive assets. The delivery plan first establishes Ago-owned
durable execution, then turns it into the product's defining interface: an
intelligent board that owns the work graph and coordinates bounded-context
agents until every dependency and acceptance criterion is satisfied.
