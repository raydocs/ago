import { createReadStream, createWriteStream } from "node:fs";
import { createAssistantMessageEventStream } from "@earendil-works/pi-ai";
import type { AssistantMessage, Context, Model, SimpleStreamOptions } from "@earendil-works/pi-ai";

const MAX = 1024 * 1024;
type Response = { type: "delta"; id: string; delta: string } | { type: "result"; id: string; message: AssistantMessage } | { type: "error"; id: string; error: string };

export function createPipeProvider(requestFD = 3, responseFD = 4) {
  const input = createReadStream("", { fd: responseFD, autoClose: false });
  const output = createWriteStream("", { fd: requestFD, autoClose: false });
  const pending = new Map<string, (value: Response) => void>();
  let buffer = "";
  input.setEncoding("utf8");
  input.on("data", chunk => {
    buffer += chunk;
    if (Buffer.byteLength(buffer) > MAX && !buffer.includes("\n")) return input.destroy(new Error("provider frame exceeds budget"));
    for (let newline; (newline = buffer.indexOf("\n")) >= 0;) {
      const line = buffer.slice(0, newline); buffer = buffer.slice(newline + 1);
      if (Buffer.byteLength(line) > MAX) return input.destroy(new Error("provider frame exceeds budget"));
      const value: unknown = JSON.parse(line);
      if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("invalid provider frame");
      const raw = value as Record<string, unknown>;
      const keys = raw.type === "delta" ? ["type", "id", "delta"] : raw.type === "result" ? ["type", "id", "message"] : ["type", "id", "error"];
      if (Object.keys(raw).length !== keys.length || keys.some(key => !(key in raw)) || typeof raw.id !== "string") throw new Error("invalid provider frame");
      pending.get(raw.id)?.(raw as Response);
    }
  });
  return (model: Model<any>, context: Context, options?: SimpleStreamOptions) => {
    const stream = createAssistantMessageEventStream();
    const id = crypto.randomUUID();
    const request = JSON.stringify({ type: "inference_request", id, provider: model.provider, model: model.id, context, options: { maxTokens: options?.maxTokens } });
    if (Buffer.byteLength(request) > MAX) throw new Error("provider request exceeds budget");
    let text = "";
    const base: AssistantMessage = { role: "assistant", api: model.api, provider: model.provider, model: model.id, content: [], usage: zeroUsage(), stopReason: "stop", timestamp: Date.now() };
    stream.push({ type: "start", partial: base });
    pending.set(id, response => {
      if (response.type === "delta") {
        if (!text) stream.push({ type: "text_start", contentIndex: 0, partial: base });
        text += response.delta;
        stream.push({ type: "text_delta", contentIndex: 0, delta: response.delta, partial: { ...base, content: [{ type: "text", text }] } });
      } else if (response.type === "result") {
        pending.delete(id); stream.push({ type: "done", reason: response.message.stopReason as "stop" | "length" | "toolUse", message: response.message });
      } else {
        pending.delete(id); stream.push({ type: "error", reason: options?.signal?.aborted ? "aborted" : "error", error: { ...base, stopReason: options?.signal?.aborted ? "aborted" : "error", errorMessage: response.error } });
      }
    });
    options?.signal?.addEventListener("abort", () => output.write(`${JSON.stringify({ type: "cancel", id })}\n`), { once: true });
    output.write(`${request}\n`);
    return stream;
  };
}

const zeroUsage = () => ({ input: 0, output: 0, cacheRead: 0, cacheWrite: 0, totalTokens: 0, cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0 } });
