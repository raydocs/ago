import type { AgoCommand, AgoEvent, AgoExternalTool, AgoMessage, AgoSidecar } from "./index.js";

export const MAX_JSONL_BYTES = 1024 * 1024;

export type Initialize = {
  type: "initialize";
  transcript: AgoMessage[];
  cwd: string;
  provider: string;
  model: string;
  tools: AgoExternalTool[];
};

type Sink = { write(data: string | Uint8Array): unknown };

export async function runJsonlProcess(options: {
  create(init: Initialize, onEvent: (event: AgoEvent) => void): Promise<AgoSidecar>;
  stdin: ReadableStream<Uint8Array>;
  stdout: Sink;
  stderr: Sink;
}): Promise<void> {
  let sidecar: AgoSidecar | undefined;
  let toolNames = new Set<string>();
  let closed = false;
  const prompts = new Set<Promise<void>>();
  const emit = (event: AgoEvent) => {
    if (!closed) options.stdout.write(`${JSON.stringify(event)}\n`);
  };
  const shutdown = async () => {
    if (!sidecar) return;
    await sidecar.command({ type: "abort" }).catch(() => undefined);
    await Promise.allSettled(prompts);
    sidecar.close();
  };
  const signal = () => {
    closed = true;
    void shutdown().finally(() => process.exit(0));
  };
  process.once("SIGINT", signal);
  process.once("SIGTERM", signal);

  try {
    let first = true;
    for await (const line of boundedLines(options.stdin, MAX_JSONL_BYTES)) {
      const value = parseObject(line);
      if (first) {
        const init = validateInitialize(value);
        toolNames = new Set(init.tools.map(tool => tool.name));
        sidecar = await options.create(init, emit);
        first = false;
        continue;
      }
      if (!sidecar) throw new Error("initialize required");
      const command = validateCommand(value, toolNames);
      if (command.type === "prompt") {
        const task = sidecar.command(command);
        prompts.add(task);
        void task.catch(error => options.stderr.write(`prompt failed: ${message(error)}\n`)).finally(() => prompts.delete(task));
      } else {
        await sidecar.command(command);
      }
    }
    if (first) throw new Error("initialize required");
    await shutdown();
  } catch (error) {
    closed = true;
    await shutdown();
    options.stderr.write(`protocol error: ${message(error)}\n`);
    process.exitCode = 1;
  } finally {
    process.off("SIGINT", signal);
    process.off("SIGTERM", signal);
  }
}

async function* boundedLines(stream: ReadableStream<Uint8Array>, maximum: number): AsyncGenerator<string> {
  const decoder = new TextDecoder("utf-8", { fatal: true });
  let pending = "";
  for await (const chunk of stream) {
    pending += decoder.decode(chunk, { stream: true });
    if (new TextEncoder().encode(pending).byteLength > maximum && !pending.includes("\n")) throw new Error("input line too large");
    let newline: number;
    while ((newline = pending.indexOf("\n")) >= 0) {
      const line = pending.slice(0, newline);
      pending = pending.slice(newline + 1);
      if (new TextEncoder().encode(line).byteLength > maximum) throw new Error("input line too large");
      if (!line || line.endsWith("\r")) throw new Error("invalid JSONL line");
      yield line;
    }
  }
  pending += decoder.decode();
  if (pending) throw new Error("unterminated JSONL line");
}

function parseObject(line: string): Record<string, unknown> {
  const value: unknown = JSON.parse(line);
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("JSON object required");
  return value as Record<string, unknown>;
}

function exact(value: Record<string, unknown>, keys: string[]) {
  if (Object.keys(value).length !== keys.length || keys.some(key => !(key in value))) throw new Error("unknown or missing field");
}

function string(value: unknown, field: string): string {
  if (typeof value !== "string") throw new Error(`${field} must be a string`);
  return value;
}

function validateInitialize(value: Record<string, unknown>): Initialize {
  exact(value, ["type", "transcript", "cwd", "provider", "model", "tools"]);
  if (value.type !== "initialize" || !Array.isArray(value.transcript)) throw new Error("invalid initialize");
  if (!Array.isArray(value.tools)) throw new Error("tools must be an array");
  const tools = value.tools.map(validateTool);
  const names = new Set(tools.map(tool => tool.name));
  if (names.size !== tools.length) throw new Error("duplicate external tool");
  const transcript = value.transcript.map(message => validateMessage(message, names));
  validateCorrelations(transcript);
  return { type: "initialize", transcript, cwd: string(value.cwd, "cwd"), provider: string(value.provider, "provider"), model: string(value.model, "model"), tools };
}

function validateTool(raw: unknown): AgoExternalTool {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) throw new Error("invalid external tool");
  const value = raw as Record<string, unknown>;
  exact(value, ["name", "description", "inputSchema"]);
  if (!value.inputSchema || typeof value.inputSchema !== "object" || Array.isArray(value.inputSchema)) throw new Error("invalid external tool schema");
  return { name: string(value.name, "name"), description: string(value.description, "description"), inputSchema: value.inputSchema as Record<string, unknown> };
}

function validateMessage(raw: unknown, toolNames: Set<string>): AgoMessage {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) throw new Error("invalid transcript message");
  const value = raw as Record<string, unknown>;
  if (value.role === "summary") {
    exact(value, ["role", "text", "at"]);
    if (typeof value.at !== "number" || !Number.isFinite(value.at)) throw new Error("invalid summary timestamp");
    return { role: "summary", text: string(value.text, "text"), at: value.at };
  }
  if (value.role === "user") {
    exact(value, ["role", "text", "at"]);
    if (typeof value.at !== "number" || !Number.isFinite(value.at)) throw new Error("invalid message timestamp");
    return { role: "user", text: string(value.text, "text"), at: value.at };
  }
  if (value.role === "assistant") {
    exact(value, ["role", "api", "provider", "model", "stopReason", "content", "at"]);
    if (typeof value.at !== "number" || !Number.isFinite(value.at) || !Array.isArray(value.content)) throw new Error("invalid assistant message");
    const reasons = ["stop", "length", "toolUse", "error", "aborted"] as const;
    if (!reasons.includes(value.stopReason as typeof reasons[number])) throw new Error("invalid stop reason");
    return {
      role: "assistant", api: string(value.api, "api"), provider: string(value.provider, "provider"), model: string(value.model, "model"),
      stopReason: value.stopReason as typeof reasons[number], content: value.content.map(block => validateAssistantBlock(block, toolNames)), at: value.at,
    };
  }
  exact(value, ["role", "callId", "name", "text", "error", "at"]);
  const name = string(value.name, "name");
  if (value.role !== "tool" || !toolNames.has(name) || typeof value.error !== "boolean" || typeof value.at !== "number" || !Number.isFinite(value.at)) throw new Error("invalid tool message");
  return { role: "tool", callId: string(value.callId, "callId"), name, text: string(value.text, "text"), error: value.error, at: value.at };
}

function validateAssistantBlock(raw: unknown, toolNames: Set<string>): Extract<AgoMessage, { role: "assistant" }>["content"][number] {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) throw new Error("invalid assistant content block");
  const value = raw as Record<string, unknown>;
  if (value.type === "text") {
    exact(value, ["type", "text"]);
    return { type: "text", text: string(value.text, "text") };
  }
  if (value.type === "toolCall") {
    exact(value, ["type", "callId", "name", "input"]);
    const name = string(value.name, "name");
    if (!toolNames.has(name) || !value.input || typeof value.input !== "object" || Array.isArray(value.input)) throw new Error("invalid tool call block");
    return { type: "toolCall", callId: string(value.callId, "callId"), name, input: value.input as Record<string, unknown> };
  }
  throw new Error("unknown assistant content block");
}

function validateCorrelations(transcript: AgoMessage[]): void {
  const summaries = transcript.flatMap((message, index) => message.role === "summary" ? [index] : []);
  if (summaries.length > 1 || (summaries.length === 1 && summaries[0] !== 0)) throw new Error("summary must be the first and only summary transcript entry");
  const calls = new Map<string, string>();
  for (const message of transcript) {
    if (message.role === "assistant") for (const block of message.content) {
      if (block.type === "toolCall") {
        if (calls.has(block.callId)) throw new Error("duplicate tool call");
        calls.set(block.callId, block.name);
      }
    }
    if (message.role === "tool") {
      if (calls.get(message.callId) !== message.name) throw new Error("uncorrelated tool result");
      calls.delete(message.callId);
    }
  }
}

function validateCommand(value: Record<string, unknown>, toolNames: Set<string>): AgoCommand {
  switch (value.type) {
    case "prompt": case "steer": case "follow_up":
      exact(value, ["type", "text"]);
      return { type: value.type, text: string(value.text, "text") };
    case "abort": exact(value, ["type"]); return { type: "abort" };
    case "tool_result": {
      const allowed = value.error === undefined ? ["type", "callId", "name", "output"] : ["type", "callId", "name", "output", "error"];
      exact(value, allowed);
      const name = string(value.name, "name");
      if (!toolNames.has(name) || (value.error !== undefined && typeof value.error !== "boolean")) throw new Error("invalid tool result");
      return { type: "tool_result", callId: string(value.callId, "callId"), name, output: string(value.output, "output"), ...(value.error === undefined ? {} : { error: value.error as boolean }) };
    }
    default: throw new Error("unknown command type");
  }
}

const message = (error: unknown) => error instanceof Error ? error.message : String(error);
