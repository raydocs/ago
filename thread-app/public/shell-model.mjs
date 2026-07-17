/**
 * Pure view-model helpers for Amp-like shell chrome.
 * Used by public/app.js and unit tests — do not reimplement in tests.
 */

export function threadBucket(thread, now = Date.now()) {
  const updated = Date.parse(thread?.updated_at || thread?.started_at);
  if (!Number.isFinite(updated)) return "older";
  const age = now - updated;
  // Amp web (ampcode.com): recent stream first, then "Inactive Last 24h", then older.
  if (age <= 6 * 60 * 60 * 1000) return "active";
  if (age <= 24 * 60 * 60 * 1000) return "inactive24h";
  return "older";
}

export function stableTitleFromThread(thread, maxLen = 72) {
  const raw = String(thread?.title || "Untitled thread").trim();
  if (!raw) return "Untitled thread";
  const firstLine = raw.split(/\r?\n/).map((line) => line.trim()).find(Boolean) || "Untitled thread";
  return firstLine.length > maxLen ? `${firstLine.slice(0, maxLen - 1)}…` : firstLine;
}

/**
 * Amp web labels only inactive/older groups; the recent stream is unlabeled
 * (matches ampcode.com screenshots that show "Inactive Last 24h" without an Active header).
 * @returns {{ key: string, label: string|null, threads: object[] }[]}
 */
export function groupThreadsForRail(threads, now = Date.now()) {
  const groups = { active: [], inactive24h: [], older: [] };
  for (const thread of threads || []) {
    groups[threadBucket(thread, now)].push(thread);
  }
  const out = [];
  if (groups.active.length) out.push({ key: "active", label: null, threads: groups.active });
  if (groups.inactive24h.length) {
    out.push({ key: "inactive24h", label: "Inactive Last 24h", threads: groups.inactive24h });
  }
  if (groups.older.length) out.push({ key: "older", label: "Older", threads: groups.older });
  return out;
}

export function compactModelName(model) {
  const value = String(model || "").trim();
  if (!value) return "";
  return value.replace(/^claude-/, "").replace(/-build$/, "");
}

/**
 * Return only public thread metadata that is present in the API contract.
 * Identity, visibility, authentication, and billing labels are intentionally absent.
 */
export function humanThreadMetadata(thread, eventCount) {
  const record = thread && typeof thread === "object" ? thread : {};
  const entries = [];
  const add = (key, value) => {
    if (value === null || value === undefined || String(value).trim() === "") return;
    entries.push({ key, value });
  };
  add("state", record.state);
  add("updated_at", record.updated_at);
  add("started_at", record.started_at);
  add("project", record.project_name);
  add("model", record.model);
  add("effort", record.effort);
  if (Number.isFinite(Number(eventCount)) && Number(eventCount) >= 0) {
    entries.push({ key: "events", value: Number(eventCount) });
  }
  return entries;
}
