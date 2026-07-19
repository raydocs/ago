import { SessionManager, SettingsManager, createAgentSession, defineTool } from "@earendil-works/pi-coding-agent";
import type { AgentSession, AgentSessionEvent, ModelRuntime } from "@earendil-works/pi-coding-agent";
import type { AssistantMessage, Message, Model } from "@earendil-works/pi-ai";
import { Type } from "typebox";

export type AgoExternalTool = { name: string; description: string; inputSchema: Record<string, unknown> };

export type AgoMessage =
  | { role: "summary"; text: string; at: number }
  | { role: "user"; text: string; at: number }
  | { role: "assistant"; api: string; provider: string; model: string; stopReason: "stop" | "length" | "toolUse" | "error" | "aborted"; content: AgoAssistantBlock[]; at: number }
  | { role: "tool"; callId: string; name: string; text: string; error: boolean; at: number };

export type AgoAssistantBlock =
  | { type: "text"; text: string }
  | { type: "toolCall"; callId: string; name: string; input: Record<string, unknown> };

export type AgoToolInvocation = { type: "tool_invocation"; callId: string; name: string; input: Record<string, unknown> };
export type AgoToolResult = { type: "tool_result"; callId: string; name: string; output: string; error?: boolean };
export type AgoProviderUsage = {
  provider: string;
  model: string;
  request_id: string | null;
  status: "provisional" | "final";
  usage: {
    input_tokens: number;
    output_tokens: number;
    cache_read_tokens: number;
    cache_write_tokens: number;
    total_tokens: number;
    cache_write_1h_tokens?: number;
    reasoning_tokens?: number;
  };
  cost: { input: number; output: number; cache_read: number; cache_write: number; total: number };
};
export type AgoEvent =
  | { type: "started" }
  | { type: "text"; delta: string }
  | { type: "assistant_completed"; message: Extract<AgoMessage, { role: "assistant" }>; provider_usage: AgoProviderUsage }
  | AgoToolInvocation
  | { type: "tool_finished"; callId: string; error: boolean }
  | { type: "stopped"; reason: "stop" | "length" | "toolUse" | "error" | "aborted" }
  | { type: "settled" }
  | { type: "queue"; steering: number; followUp: number }
  | { type: "compact_start" }
  | { type: "compact_end"; summary: string; aborted: boolean };

export type AgoCommand =
  | { type: "prompt"; text: string }
  | { type: "steer"; text: string }
  | { type: "follow_up"; text: string }
  | { type: "abort" }
  | AgoToolResult;

export interface AgoSidecar {
  readonly session: AgentSession;
  readonly events: AgoEvent[];
  command(command: AgoCommand): Promise<void>;
  compact(): Promise<void>;
  close(): void;
}

const toPiMessage = (m: Exclude<AgoMessage, { role: "summary" }>): Message => {
  if (m.role === "user") return { role: "user", content: m.text, timestamp: m.at };
  if (m.role === "assistant") return {
    role: "assistant",
    content: m.content.map(block => block.type === "text" ? block : { type: "toolCall", id: block.callId, name: block.name, arguments: block.input }),
    api: m.api as AssistantMessage["api"], provider: m.provider, model: m.model, usage: zeroUsage(), stopReason: m.stopReason, timestamp: m.at,
  };
  return { role: "toolResult", toolCallId: m.callId, toolName: m.name, content: [{ type: "text", text: m.text }], isError: m.error, timestamp: m.at };
};

const zeroUsage = () => ({ input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } });

export async function createAgoSidecar(options: { transcript: AgoMessage[]; tools?: AgoExternalTool[]; model: Model<any>; modelRuntime?: ModelRuntime; cwd?: string; onEvent?: (e: AgoEvent) => void }): Promise<AgoSidecar> {
  const manager = SessionManager.inMemory(options.cwd);
  const summary = options.transcript[0]?.role === "summary" ? options.transcript[0] : undefined;
  if (options.transcript.slice(summary ? 1 : 0).some(message => message.role === "summary")) throw new Error("summary must be the first and only summary transcript entry");
  let firstKeptEntryId: string | undefined;
  for (const message of options.transcript.slice(summary ? 1 : 0)) {
    if (message.role === "summary") throw new Error("summary must be the first and only summary transcript entry");
    const entryID = manager.appendMessage(toPiMessage(message));
    firstKeptEntryId ??= entryID;
  }
  if (summary) manager.appendCompaction(summary.text, firstKeptEntryId ?? "", 0, { authority: "ago", at: summary.at }, true);
  if (manager.getSessionFile() !== undefined || manager.isPersisted()) throw new Error("Pi persistence authority violation");
  const settings = SettingsManager.inMemory();
  const events: AgoEvent[] = [];
  const pending = new Map<string, { name: string; resolve(result: AgoToolResult): void }>();
  const emit = (event: AgoEvent) => { events.push(event); options.onEvent?.(event); };
  const definitions = options.tools ?? [{ name: "ago_echo", description: "Call Ago's external echo tool", inputSchema: Type.Object({ text: Type.String() }) }];
  const customTools = definitions.map(definition => defineTool({
    name: definition.name, label: definition.name, description: definition.description, parameters: definition.inputSchema as any,
    async execute(callId, input: Record<string, unknown>, signal) {
      const result = await new Promise<AgoToolResult>((resolve, reject) => {
        pending.set(callId, { name: definition.name, resolve });
        signal?.addEventListener("abort", () => reject(new Error("aborted")), { once: true });
        emit({ type: "tool_invocation", callId, name: definition.name, input });
      });
      return { content: [{ type: "text", text: result.output }], details: {}, isError: !!result.error };
    }
  }));
  const { session } = await createAgentSession({ model: options.model, modelRuntime: options.modelRuntime, sessionManager: manager, settingsManager: settings, noTools: "builtin", customTools, cwd: options.cwd });
  const unsubscribe = session.subscribe((raw) => { for (const event of mapPiEvent(raw)) emit(event); });
  return {
    session, events,
    async command(command) {
      switch (command.type) {
        case "prompt": await session.prompt(command.text); return;
        case "steer": await session.steer(command.text); return;
        case "follow_up": await session.followUp(command.text); return;
        case "abort": await session.abort(); return;
        case "tool_result": {
          const waiting = pending.get(command.callId);
          if (!waiting || waiting.name !== command.name) throw new Error(`Uncorrelated tool result: ${command.callId}`);
          pending.delete(command.callId); waiting.resolve(command); return;
        }
      }
    },
    async compact() {
      emit({ type: "compact_start" });
      const summary = "AGO_SUMMARY";
      const entries = manager.getEntries();
      const first = entries.at(-1)?.id;
      if (first) manager.appendCompaction(summary, first, 0, { authority: "ago" }, true);
      session.agent.state.messages = manager.buildSessionContext().messages;
      emit({ type: "compact_end", summary, aborted: false });
      if (manager.getSessionFile() !== undefined || manager.isPersisted()) throw new Error("Pi persistence authority violation");
    },
    close: unsubscribe
  };
}

function mapPiEvent(event: AgentSessionEvent): AgoEvent[] {
  switch (event.type) {
    case "agent_start": return [{ type: "started" }];
    case "message_update": return event.assistantMessageEvent.type === "text_delta" ? [{ type: "text", delta: event.assistantMessageEvent.delta }] : [];
    case "tool_execution_end": return [{ type: "tool_finished", callId: event.toolCallId, error: event.isError }];
    case "agent_end": { const last = event.messages.at(-1); return last?.role === "assistant" ? [{ type: "stopped", reason: last.stopReason }] : []; }
    case "agent_settled": return [{ type: "settled" }];
    case "queue_update": return [{ type: "queue", steering: event.steering.length, followUp: event.followUp.length }];
    case "compaction_start": return [{ type: "compact_start" }];
    case "compaction_end": return [{ type: "compact_end", summary: event.result?.summary ?? "", aborted: event.aborted }];
    case "message_end": return event.message.role === "assistant" ? [{
      type: "assistant_completed",
      message: canonicalAssistant(event.message),
      provider_usage: canonicalProviderUsage(event.message),
    }] : [];
    case "turn_start": case "turn_end": case "message_start": case "tool_execution_start": case "tool_execution_update":
    case "entry_appended": case "session_info_changed": case "thinking_level_changed": case "auto_retry_start": case "auto_retry_end": return [];
    default: {
      const exhaustive: never = event;
      throw new Error(`Unknown Pi event rejected: ${String((exhaustive as { type?: unknown }).type)}`);
    }
  }
}

function canonicalAssistant(message: AssistantMessage): Extract<AgoMessage, { role: "assistant" }> {
  return {
    role: "assistant", api: message.api, provider: message.provider, model: message.model, stopReason: message.stopReason,
    content: message.content.map(block => {
      if (block.type === "text") return { type: "text", text: block.text };
      if (block.type === "toolCall") {
        return { type: "toolCall", callId: block.id, name: block.name, input: block.arguments };
      }
      throw new Error(`Unsupported assistant content block: ${block.type}`);
    }),
    at: message.timestamp,
  };
}

function canonicalProviderUsage(message: AssistantMessage): AgoProviderUsage {
  const usage = strictObject(message.usage, "provider usage");
  exactKeys(usage, ["input", "output", "cacheRead", "cacheWrite", "totalTokens", "cost"], ["cacheWrite1h", "reasoning"], "provider usage");
  const cost = strictObject(usage.cost, "provider cost");
  exactKeys(cost, ["input", "output", "cacheRead", "cacheWrite", "total"], [], "provider cost");
  const responseModel = optionalIdentity(message.responseModel, "response model");
  const requestId = optionalIdentity(message.responseId, "request id");
  return {
    provider: identity(message.provider, "provider"),
    model: responseModel ?? identity(message.model, "model"),
    request_id: requestId ?? null,
    status: message.stopReason === "error" || message.stopReason === "aborted" ? "provisional" : "final",
    usage: {
      input_tokens: tokenBucket(usage.input, "input"),
      output_tokens: tokenBucket(usage.output, "output"),
      cache_read_tokens: tokenBucket(usage.cacheRead, "cacheRead"),
      cache_write_tokens: tokenBucket(usage.cacheWrite, "cacheWrite"),
      total_tokens: tokenBucket(usage.totalTokens, "totalTokens"),
      ...(usage.cacheWrite1h === undefined ? {} : { cache_write_1h_tokens: tokenBucket(usage.cacheWrite1h, "cacheWrite1h") }),
      ...(usage.reasoning === undefined ? {} : { reasoning_tokens: tokenBucket(usage.reasoning, "reasoning") }),
    },
    cost: {
      input: costBucket(cost.input, "input"),
      output: costBucket(cost.output, "output"),
      cache_read: costBucket(cost.cacheRead, "cacheRead"),
      cache_write: costBucket(cost.cacheWrite, "cacheWrite"),
      total: costBucket(cost.total, "total"),
    },
  };
}

function strictObject(value: unknown, name: string): Record<string, unknown> {
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${name} must be an object`);
  return value as Record<string, unknown>;
}

function exactKeys(value: Record<string, unknown>, required: string[], optional: string[], name: string): void {
  const allowed = new Set([...required, ...optional]);
  if (required.some(key => !(key in value)) || Object.keys(value).some(key => !allowed.has(key))) throw new Error(`invalid ${name} shape`);
}

function identity(value: unknown, name: string): string {
  if (typeof value !== "string" || !value) throw new Error(`${name} must be a non-empty string`);
  return value;
}

function optionalIdentity(value: unknown, name: string): string | undefined {
  return value === undefined ? undefined : identity(value, name);
}

function tokenBucket(value: unknown, name: string): number {
  if (typeof value !== "number" || !Number.isSafeInteger(value) || value < 0) throw new Error(`${name} must be a nonnegative safe integer`);
  return value;
}

function costBucket(value: unknown, name: string): number {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) throw new Error(`${name} cost must be finite and nonnegative`);
  return value;
}

export function encodeJsonl(value: AgoCommand | AgoEvent): string { return `${JSON.stringify(value)}\n`; }
export function decodeCommand(line: string): AgoCommand { return JSON.parse(line) as AgoCommand; }
