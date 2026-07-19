# Ago Web / Mobile PWA

An independent, responsive schema-v1 projection client for desktop browsers and touch/mobile layouts. It is a **Mobile PWA**, not a native mobile application and not the whole Phase 2 program. The daemon remains authoritative. An in-memory reconnect resumes from the validated current cursor. A cold reload rebuilds the complete transcript from sequence 0 unless a complete validated event cache is available; a bare persisted cursor is never restored because it would omit history.

```sh
bun install
bun test
bun run typecheck
bun run build
# development (from this directory)
bunx serve .
```

Set the daemon base URL and thread ID in the connection bar. The service worker caches shell files only; API reads and mutations are never cached. An authenticated remote bridge is a Root-owned follow-up and is intentionally not implemented here.
