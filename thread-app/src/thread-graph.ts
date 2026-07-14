export type GraphEvent = Record<string, unknown>;
export type ThreadGraph = Record<string, unknown>;

const REDACTION_POLICY = "claudex-redaction.v1";
const THREAD_SCHEMA = "claudex-thread.v1";
const REDACTED = "[REDACTED]";
const INTERNAL_METADATA_REDACTED = "[INTERNAL METADATA REDACTED]";

const SENSITIVE_KEY = /(?:^|[_-])(?:api[_-]?key|authorization|cookie|credentials?|password|passwd|passphrase|secret|client[_-]?secret|session[_-]?secret|private[_-]?key|access[_-]?token|refresh[_-]?token|auth[_-]?token|token|bearer)(?:$|[_-])/i;
const HIDDEN_REASONING_KEY = /^(?:thinking|reasoning|hidden[_-]?(?:thinking|reasoning|summary)|chain[_-]?of[_-]?thought|internal[_-]?(?:thoughts?|reasoning))$/i;
const HIDDEN_REASONING_TYPE = /^(?:thinking|reasoning|chain[_-]?of[_-]?thought|internal[_-]?(?:thoughts?|reasoning))$/i;
const SENSITIVE_FORM_LABEL = /(?:password|passwd|passphrase|secret|token|api[_ -]?key|访问密码|密码)/i;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function stringValue(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function finiteNumber(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function redactString(value: string): string {
  // Keep these replacements deliberately local: safe prose and paths must remain byte-for-byte intact.
  if (/this tool result is internal metadata/i.test(value)) return INTERNAL_METADATA_REDACTED;
  return value
    .replace(/\s*agentId:\s*[A-Za-z0-9_-]+[\s\S]*?(?=<usage>|$)/gi, ` ${INTERNAL_METADATA_REDACTED} `)
    .replace(/((?:password|passwd|passphrase|secret|token|api[_ -]?key|访问密码|密码)[\s\S]{0,160}?\.fill\(\s*)(["'`])[\s\S]*?\2(\s*\))/gi, `$1$2${REDACTED}$2$3`)
    .replace(/(bearer\s+)[A-Za-z0-9._~+/=-]{8,}/gi, `$1${REDACTED}`)
    .replace(/\bsk-[A-Za-z0-9_-]{16,}\b/g, REDACTED)
    .replace(/\b(?:gh[opusr]_[A-Za-z0-9_]{16,}|github_pat_[A-Za-z0-9_]{16,})\b/gi, REDACTED)
    .replace(/\bAKIA[A-Z0-9]{16}\b/g, REDACTED)
    .replace(/\beyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\b/g, REDACTED)
    .replace(/\b[0-9a-f]{24,}\.[A-Za-z0-9_-]{12,}\b/gi, REDACTED)
    .replace(/((?:api[_-]?key|auth[_-]?token|access[_-]?token|refresh[_-]?token|token|client[_-]?secret|secret|password|passwd|passphrase)["']?\s*[:=]\s*)(["']?)[^\s,;"']{8,}\2/gi, `$1$2${REDACTED}$2`)
    .replace(/(https?:\/\/[^\s/:@]+:)[^\s/@]+(@)/gi, `$1${REDACTED}$2`)
    .replace(/-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----[\s\S]*?-----END (?:RSA |EC |OPENSSH )?PRIVATE KEY-----/g, REDACTED);
}

function redactValue(value: unknown, key = "", seen = new WeakSet<object>()): unknown {
  if (SENSITIVE_KEY.test(key) || HIDDEN_REASONING_KEY.test(key)) return REDACTED;
  if (typeof value === "string") return redactString(value);
  if (value === null || typeof value !== "object") return value;
  if (seen.has(value)) return "[Circular]";
  seen.add(value);

  if (Array.isArray(value)) {
    const output = value.map((item) => redactValue(item, "", seen));
    seen.delete(value);
    return output;
  }

  const output: Record<string, unknown> = {};
  let hiddenReasoningObject = false;
  let sensitiveFormObject = false;
  try {
    const record = value as Record<string, unknown>;
    hiddenReasoningObject = HIDDEN_REASONING_TYPE.test(stringValue(record.type));
    sensitiveFormObject = Object.entries(record).some(([descriptor, child]) =>
      /^(?:name|label|element|placeholder|aria[_-]?label)$/i.test(descriptor)
      && SENSITIVE_FORM_LABEL.test(stringValue(child)));
  } catch {
    // An unreadable discriminator is handled by the per-property guard below.
  }
  for (const childKey of Object.keys(value)) {
    let child: unknown;
    try {
      child = (value as Record<string, unknown>)[childKey];
    } catch {
      child = REDACTED;
    }
    const hidesReasoningPayload = hiddenReasoningObject
      && /^(?:content|text|summary|raw|payload|message)$/i.test(childKey);
    const hidesSensitiveFormValue = sensitiveFormObject && /^(?:value|text|content)$/i.test(childKey);
    Object.defineProperty(output, childKey, {
      value: hidesReasoningPayload || hidesSensitiveFormValue ? REDACTED : redactValue(child, childKey, seen),
      enumerable: true,
      configurable: true,
      writable: true,
    });
  }
  seen.delete(value);
  return output;
}

function sanitizedRecord(value: unknown): GraphEvent {
  const sanitized = redactValue(value);
  return isRecord(sanitized) ? sanitized : {};
}

/** Validate and sanitize one session's uploaded canonical graph records. */
export function normalizeGraphEvents(value: unknown, sessionID: string): { records: GraphEvent[]; error: string } {
  try {
    if (!Array.isArray(value)) return { records: [], error: "graph events must be an array" };
    if (typeof sessionID !== "string" || sessionID.trim() === "") {
      return { records: [], error: "sessionID is required" };
    }

    const records: GraphEvent[] = [];
    for (let index = 0; index < value.length; index += 1) {
      const candidate = value[index];
      if (!isRecord(candidate)) return { records: [], error: `record ${index} must be an object` };

      let recordSessionID: unknown;
      try {
        recordSessionID = candidate.session_id;
      } catch {
        return { records: [], error: `record ${index} has an unreadable session_id` };
      }
      if (recordSessionID !== sessionID) {
        return { records: [], error: `record ${index} does not belong to this session` };
      }
      records.push(sanitizedRecord(candidate));
    }
    return { records, error: "" };
  } catch {
    return { records: [], error: "graph events could not be normalized" };
  }
}

function compareEvents(left: GraphEvent, right: GraphEvent): number {
  const leftTime = stringValue(left.started_at);
  const rightTime = stringValue(right.started_at);
  if (leftTime !== rightTime) {
    if (!leftTime) return 1;
    if (!rightTime) return -1;
    return leftTime < rightTime ? -1 : 1;
  }
  return stringValue(left.event_id).localeCompare(stringValue(right.event_id));
}

function nestedRecord(value: unknown, key: string): Record<string, unknown> {
  if (!isRecord(value)) return {};
  return isRecord(value[key]) ? value[key] as Record<string, unknown> : {};
}

function findEvidenceString(value: unknown, keys: ReadonlySet<string>, seen = new WeakSet<object>()): string {
  if (value === null || typeof value !== "object" || seen.has(value)) return "";
  seen.add(value);
  try {
    if (Array.isArray(value)) {
      for (const item of value) {
        const found = findEvidenceString(item, keys, seen);
        if (found) return found;
      }
      return "";
    }
    for (const key of Object.keys(value)) {
      const child = (value as Record<string, unknown>)[key];
      if (keys.has(key.toLowerCase()) && typeof child === "string" && child.trim()) return child.trim();
    }
    for (const key of Object.keys(value)) {
      const found = findEvidenceString((value as Record<string, unknown>)[key], keys, seen);
      if (found) return found;
    }
  } catch {
    return "";
  }
  return "";
}

function terminalToolName(event: GraphEvent): string {
  const name = stringValue(event.tool_name).toLowerCase();
  return name.split(/[:/]/).pop()?.split("__").pop() ?? name;
}

function isWorkerCall(event: GraphEvent): boolean {
  if (event.type !== "tool_call") return false;
  const terminalName = terminalToolName(event);
  return terminalName === "agent" || terminalName === "worker" || terminalName === "start_worker" || terminalName === "resume_worker";
}

function resultForCall(call: GraphEvent, events: GraphEvent[]): GraphEvent | undefined {
  const callID = stringValue(call.event_id) || stringValue(call.tool_use_id);
  return events
    .filter((event) => event.type === "tool_result"
      && (stringValue(event.parent_event_id) === callID || stringValue(event.tool_use_id) === callID))
    .sort(compareEvents)[0];
}

function workerEvent(call: GraphEvent, result: GraphEvent | undefined): GraphEvent {
  const callRaw = isRecord(call.raw) ? call.raw : {};
  const input = nestedRecord(callRaw, "input");
  const resultRaw = result && isRecord(result.raw) ? result.raw : {};
  const resultEvidence = isRecord(resultRaw.result) ? resultRaw.result : resultRaw;
  const requestedModel = stringValue(input.model) || stringValue(call.requested_model);
  const resolvedModel = stringValue(result?.model)
    || findEvidenceString(resultEvidence, new Set(["resolved_model", "resolvedmodel", "model"]));
  const requestedEffort = stringValue(input.effort) || stringValue(call.effort);
  const resolvedEffort = stringValue(result?.effort)
    || findEvidenceString(resultEvidence, new Set(["effort"]));
  const callID = stringValue(call.event_id) || stringValue(call.tool_use_id);
  const workerID = stringValue(result?.worker_id)
    || findEvidenceString(resultEvidence, new Set(["worker_id", "workerid", "agent_id", "agentid"]));
  const toolName = terminalToolName(call);
  const executionRole = toolName === "start_worker" || toolName === "resume_worker" || toolName === "worker"
    ? "worker"
    : "agent";
  const agentType = executionRole === "agent" ? stringValue(input.subagent_type) : "";

  return sanitizedRecord({
    event_id: `worker:${callID}`,
    session_id: call.session_id,
    root_session_id: call.root_session_id,
    parent_session_id: call.parent_session_id,
    parent_event_id: callID,
    worker_id: workerID || undefined,
    type: "worker",
    role: executionRole,
    model: resolvedModel || undefined,
    effort: resolvedEffort || requestedEffort || undefined,
    status: stringValue(result?.status) || stringValue(call.status) || "unknown",
    started_at: call.started_at,
    ended_at: result?.ended_at,
    duration_ms: result?.duration_ms ?? null,
    summary: stringValue(call.summary) || stringValue(result?.summary) || "Worker invocation",
    content: stringValue(result?.content),
    tool_name: call.tool_name,
    tool_use_id: call.tool_use_id || callID,
    raw: {
      objective: stringValue(input.objective) || stringValue(input.description) || undefined,
      agent_type: agentType || null,
      requested_model: requestedModel || null,
      resolved_model: resolvedModel || null,
      requested_effort: requestedEffort || null,
      resolved_effort: resolvedEffort || null,
      call_event_id: callID,
      result_event_id: stringValue(result?.event_id) || null,
    },
  });
}

function relationship(type: string, from: string, to: string, extra: GraphEvent = {}): GraphEvent {
  return { type, from, to, ...extra };
}

function participantKey(participant: GraphEvent): string {
  return [participant.session_id, participant.role, participant.model, participant.effort].map(stringValue).join("\u0000");
}

function deriveParticipants(thread: GraphEvent, threads: GraphEvent[], events: GraphEvent[]): GraphEvent[] {
  const participants = new Map<string, GraphEvent>();
  const add = (source: GraphEvent, fallbackRole = "") => {
    const role = stringValue(source.role) || fallbackRole;
    const model = stringValue(source.model);
    const sessionID = stringValue(source.session_id);
    if (!role && !model && !sessionID) return;
    const participant: GraphEvent = {
      participant_id: [sessionID, role, model].filter(Boolean).join(":") || "unknown",
      session_id: sessionID || null,
      role: role || "unknown",
      model: model || null,
      effort: stringValue(source.effort) || null,
      state: stringValue(source.state) || undefined,
    };
    const key = participantKey(participant);
    if (!participants.has(key)) participants.set(key, participant);
  };

  add(thread, "supervisor");
  for (const child of threads) add(child, "worker");
  for (const event of events) {
    const role = stringValue(event.role);
    // System events are timeline mechanics, not model participants.
    if (role && role !== "system") add(event, event.type === "worker" ? "worker" : role);
  }
  return [...participants.values()].sort((a, b) => participantKey(a).localeCompare(participantKey(b)));
}

function stableJSON(value: unknown): string {
  try {
    return JSON.stringify(value, Object.keys(isRecord(value) ? value : {}).sort());
  } catch {
    return String(value);
  }
}

function derivedArtifacts(events: GraphEvent[]): GraphEvent[] {
  const actions = new Map([
    ["write", "write"],
    ["edit", "edit"],
    ["notebookedit", "edit"],
  ]);
  const artifacts: GraphEvent[] = [];
  for (const event of events) {
    if (event.type !== "tool_call") continue;
    const action = actions.get(terminalToolName(event));
    if (!action) continue;
    const input = nestedRecord(isRecord(event.raw) ? event.raw : {}, "input");
    const path = stringValue(input.file_path) || stringValue(input.notebook_path) || stringValue(input.path);
    if (!path) continue;
    artifacts.push({
      artifact_id: `artifact:${stringValue(event.event_id)}`,
      event_id: event.event_id,
      session_id: event.session_id,
      path,
      action,
      status: event.status,
    });
  }
  return artifacts;
}

function collectList(input: Record<string, unknown>, events: GraphEvent[], name: "artifacts" | "verification"): unknown[] {
  const found: unknown[] = Array.isArray(input[name]) ? [...input[name]] : [];
  if (name === "artifacts") found.push(...derivedArtifacts(events));
  for (const event of events) {
    if (Array.isArray(event[name])) found.push(...event[name]);
    const raw = isRecord(event.raw) ? event.raw : {};
    if (Array.isArray(raw[name])) found.push(...raw[name]);
    const result = isRecord(raw.result) ? raw.result : {};
    if (Array.isArray(result[name])) found.push(...result[name]);
  }
  const deduplicated = new Map<string, unknown>();
  for (const item of found.map((item) => redactValue(item))) deduplicated.set(stableJSON(item), item);
  return [...deduplicated.values()];
}

function canonicalUsage(value: unknown): GraphEvent {
  const usage = sanitizedRecord(value);
  const totals = isRecord(usage.totals) ? { ...usage.totals } : {};
  const cost = finiteNumber(totals.reported_cost_usd);
  if (totals.reported_cost_usd === undefined || totals.reported_cost_usd === null || cost === null) {
    totals.reported_cost_usd = null;
  }
  if (!stringValue(totals.cost_status)) totals.cost_status = totals.reported_cost_usd === null ? "unpriced" : "reported";
  return {
    ...usage,
    totals,
    models: Array.isArray(usage.models) ? usage.models : [],
    sessions: Array.isArray(usage.sessions) ? usage.sessions : [],
    roles: Array.isArray(usage.roles) ? usage.roles : [],
  };
}

function durationTotals(events: GraphEvent[]): { activeDurationMS: number | null; workerComputeDurationMS: number | null } {
  const activeStarts = new Map<string, number>();
  let activeDurationMS = 0;
  let completedIntervals = 0;
  let workerComputeDurationMS = 0;
  let measuredWorkers = 0;

  for (const event of events) {
    const sessionID = stringValue(event.session_id);
    const timestamp = Date.parse(stringValue(event.started_at));
    if (event.type === "message" && event.role === "user" && sessionID && Number.isFinite(timestamp)) {
      activeStarts.set(sessionID, timestamp);
    } else if (event.type === "message" && event.role === "assistant" && sessionID && Number.isFinite(timestamp)) {
      const startedAt = activeStarts.get(sessionID);
      if (startedAt !== undefined && timestamp >= startedAt) {
        activeDurationMS += timestamp - startedAt;
        completedIntervals += 1;
        activeStarts.delete(sessionID);
      }
    }
    if (event.type === "worker") {
      const duration = finiteNumber(event.duration_ms);
      if (duration !== null && duration >= 0) {
        workerComputeDurationMS += duration;
        measuredWorkers += 1;
      }
    }
  }

  return {
    activeDurationMS: completedIntervals > 0 ? activeDurationMS : null,
    workerComputeDurationMS: measuredWorkers > 0 ? workerComputeDurationMS : null,
  };
}

/** Assemble the storage/query response into the stable, redacted Thread graph contract. */
export function assembleThreadGraph(input: Record<string, unknown>): ThreadGraph {
  try {
    const safeInput = isRecord(input) ? input : {};
    const thread = sanitizedRecord(safeInput.thread);
    const threads = Array.isArray(safeInput.threads)
      ? safeInput.threads.filter(isRecord).map((item) => sanitizedRecord(item))
      : [];
    const sourceEvents = Array.isArray(safeInput.events)
      ? safeInput.events.filter(isRecord).map((item) => sanitizedRecord(item))
      : [];
    const derivedWorkers: GraphEvent[] = [];
    const relationships: GraphEvent[] = [];

    for (const child of threads) {
      const childID = stringValue(child.session_id);
      const parentID = stringValue(child.parent_session_id);
      if (childID && parentID) relationships.push(relationship("parent_session", parentID, childID));
    }

    for (const event of sourceEvents) {
      const parentEventID = stringValue(event.parent_event_id);
      const eventID = stringValue(event.event_id);
      if (parentEventID && eventID) relationships.push(relationship("tool_result", parentEventID, eventID));
    }

    for (const call of sourceEvents.filter(isWorkerCall)) {
      const result = resultForCall(call, sourceEvents);
      const worker = workerEvent(call, result);
      derivedWorkers.push(worker);
      const callID = stringValue(call.event_id) || stringValue(call.tool_use_id);
      relationships.push(relationship("worker_invocation", callID, stringValue(worker.event_id)));

      // Only the tool-result payload is child-session evidence. The result event's own
      // session_id identifies the parent transcript and must never be mistaken for a child.
      if (result && isRecord(result.raw)) {
        const evidence = isRecord(result.raw.result) ? result.raw.result : result.raw;
        const childSessionID = findEvidenceString(evidence, new Set(["child_session_id", "childsessionid", "session_id", "sessionid"]));
        if (childSessionID && childSessionID !== stringValue(call.session_id)) {
          relationships.push(relationship("worker_session", stringValue(worker.event_id), childSessionID, {
            worker_id: worker.worker_id,
          }));
        }
      }
    }

    const events = [...sourceEvents, ...derivedWorkers].sort(compareEvents);
    const usage = canonicalUsage(safeInput.usage);
    const totals = isRecord(usage.totals) ? usage.totals : {};
    const durations = durationTotals(events);
    totals.active_duration_ms = durations.activeDurationMS;
    totals.worker_compute_duration_ms = durations.workerComputeDurationMS;
    usage.totals = totals;
    const errors = Array.isArray(safeInput.errors) ? safeInput.errors.map((item) => redactValue(item)) : [];
    for (const event of events) {
      if (["failed", "error"].includes(stringValue(event.status).toLowerCase())) {
        errors.push({
          event_id: event.event_id,
          type: event.type,
          status: event.status,
          summary: event.summary,
        });
      }
    }

    return {
      schema_version: THREAD_SCHEMA,
      exported_at: stringValue(safeInput.exportedAt) || stringValue(safeInput.exported_at) || null,
      thread,
      participants: deriveParticipants(thread, threads, events),
      relationships,
      events,
      artifacts: collectList(safeInput, events, "artifacts"),
      verification: collectList(safeInput, events, "verification"),
      errors,
      usage,
      current_state: redactValue(safeInput.current_state ?? thread.state ?? null),
      next_step: redactValue(safeInput.next_step ?? null),
      redaction: { applied: true, policy_version: REDACTION_POLICY },
    };
  } catch {
    return {
      schema_version: THREAD_SCHEMA,
      exported_at: null,
      thread: {},
      participants: [],
      relationships: [],
      events: [],
      artifacts: [],
      verification: [],
      errors: [{ status: "error", summary: "Thread graph input could not be assembled" }],
      usage: canonicalUsage(undefined),
      current_state: null,
      next_step: null,
      redaction: { applied: true, policy_version: REDACTION_POLICY },
    };
  }
}

function yamlValue(value: unknown): string {
  if (value === null || value === undefined || value === "") return "null";
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  return JSON.stringify(redactString(String(value)));
}

function display(value: unknown, fallback = "Unknown"): string {
  if (value === null || value === undefined || value === "") return fallback;
  return redactString(String(value));
}

function markdownList(items: unknown[], empty: string): string[] {
  if (items.length === 0) return [`- ${empty}`];
  return items.map((item) => {
    if (typeof item === "string") return `- ${display(item)}`;
    if (!isRecord(item)) return `- ${display(item)}`;
    const label = stringValue(item.name) || stringValue(item.path) || stringValue(item.summary)
      || stringValue(item.status) || "Recorded item";
    const detail = stringValue(item.result) || stringValue(item.detail) || stringValue(item.url);
    return `- ${display(label)}${detail ? ` — ${display(detail)}` : ""}`;
  });
}

function eventHeading(event: GraphEvent): string {
  const model = stringValue(event.model);
  switch (stringValue(event.type)) {
    case "message":
      if (event.role === "user") return "User";
      if (event.role === "assistant") return `Assistant${model ? ` · ${model}` : ""}`;
      return `${display(event.role, "Message")}${model ? ` · ${model}` : ""}`;
    case "tool_call": return `Tool call · ${display(event.tool_name, "unknown tool")}`;
    case "tool_result": return `Tool result${event.tool_name ? ` · ${display(event.tool_name)}` : ""}`;
    case "worker": {
      const raw = isRecord(event.raw) ? event.raw : {};
      const label = event.role === "agent"
        ? `Agent${raw.agent_type ? ` · ${display(raw.agent_type)}` : ""}`
        : "Worker";
      return `${label}${model ? ` · ${model}` : ""}`;
    }
    case "compact": return "Compact";
    default: return display(event.type, "Event");
  }
}

/** Render only public canonical fields; raw payloads and hidden reasoning are never emitted. */
export function renderThreadMarkdown(graph: ThreadGraph): string {
  const safe = sanitizedRecord(graph);
  const thread = isRecord(safe.thread) ? safe.thread : {};
  const events = Array.isArray(safe.events) ? safe.events.filter(isRecord).sort(compareEvents) : [];
  const participants = Array.isArray(safe.participants) ? safe.participants.filter(isRecord) : [];
  const artifacts = Array.isArray(safe.artifacts) ? safe.artifacts : [];
  const verification = Array.isArray(safe.verification) ? safe.verification : [];
  const errors = Array.isArray(safe.errors) ? safe.errors : [];
  const usage = isRecord(safe.usage) ? safe.usage : {};
  const totals = isRecord(usage.totals) ? usage.totals : {};
  const title = display(thread.title, "Untitled thread");
  const schema = stringValue(safe.schema_version) || THREAD_SCHEMA;
  const lines: string[] = [
    "---",
    `schema_version: ${schema === THREAD_SCHEMA ? THREAD_SCHEMA : yamlValue(schema)}`,
    `exported_at: ${yamlValue(safe.exported_at)}`,
    `session_id: ${yamlValue(thread.session_id)}`,
    `root_session_id: ${yamlValue(thread.root_session_id)}`,
    `title: ${yamlValue(title)}`,
    `state: ${yamlValue(thread.state)}`,
    `redaction_policy: ${yamlValue(isRecord(safe.redaction) ? safe.redaction.policy_version : REDACTION_POLICY)}`,
    "---",
    "",
    `# ${title}`,
    "",
    "## Overview",
    "",
    `- Session: ${display(thread.session_id)}`,
    `- State: ${display(thread.state)}`,
    `- Started: ${display(thread.started_at)}`,
    `- Updated: ${display(thread.updated_at)}`,
    `- Events: ${events.length}`,
    "",
    "## Participants and Models Used",
    "",
  ];

  if (participants.length === 0) lines.push("- No participants recorded.");
  for (const participant of participants) {
    const role = display(participant.role, "participant");
    const model = display(participant.model, "model not reported");
    const effort = participant.effort ? ` · ${display(participant.effort)}` : "";
    const session = participant.session_id ? ` · session ${display(participant.session_id)}` : "";
    lines.push(`- **${role}** — ${model}${effort}${session}`);
  }

  lines.push("", "## Timeline", "");
  if (events.length === 0) lines.push("No events recorded.", "");
  for (const event of events) {
    lines.push(`### ${eventHeading(event)}`, "");
    const metadata = [display(event.started_at, "Time unknown")];
    if (event.status) metadata.push(`status: ${display(event.status)}`);
    if (event.duration_ms !== undefined && event.duration_ms !== null) metadata.push(`duration: ${display(event.duration_ms)} ms`);
    if (event.worker_id && event.role === "worker") metadata.push(`worker: ${display(event.worker_id)}`);
    lines.push(`_${metadata.join(" · ")}_`, "");
    const content = stringValue(event.content) || stringValue(event.summary);
    if (content) lines.push(redactString(content), "");
  }

  lines.push(
    "## Artifacts, Verification & Errors",
    "",
    "### Artifacts",
    "",
    ...markdownList(artifacts, "No artifacts recorded."),
    "",
    "### Verification",
    "",
    ...markdownList(verification, "No verification recorded."),
    "",
    "### Errors",
    "",
    ...markdownList(errors, "No errors recorded."),
    "",
    "## Usage",
    "",
    `- Requests: ${display(totals.requests, "not reported")}`,
    `- Input tokens: ${display(totals.input_tokens, "not reported")}`,
    `- Cache write (5m): ${display(totals.cache_write_5m_tokens, "not reported")}`,
    `- Cache write (1h): ${display(totals.cache_write_1h_tokens, "not reported")}`,
    `- Cache read: ${display(totals.cache_read_tokens, "not reported")}`,
    `- Output tokens: ${display(totals.output_tokens, "not reported")}`,
    `- Total tokens: ${display(totals.total_tokens, "not reported")}`,
    `- Active duration: ${typeof totals.active_duration_ms === "number" ? `${totals.active_duration_ms} ms` : "unknown"}`,
    `- Worker compute duration: ${typeof totals.worker_compute_duration_ms === "number" ? `${totals.worker_compute_duration_ms} ms` : "unknown"}`,
    `- Price status: ${display(totals.cost_status, totals.reported_cost_usd === null ? "unpriced" : "unknown")}`,
    `- Reported cost (USD): ${totals.reported_cost_usd === null || totals.reported_cost_usd === undefined ? "unknown" : display(totals.reported_cost_usd)}`,
    "",
    "## Current State",
    "",
    display(safe.current_state ?? thread.state, "Unknown"),
    "",
    "## Next Step",
    "",
    display(safe.next_step, "Not specified"),
    "",
  );

  return lines.join("\n");
}
