#!/usr/bin/env bun
import { InMemoryCredentialStore } from "@earendil-works/pi-ai";
import type { Context, ModelsSimpleStreamOptions } from "@earendil-works/pi-ai";
import { ModelRuntime } from "@earendil-works/pi-coding-agent";

const MAX_FRAME_BYTES = 1024 * 1024;

async function readBoundedInput(): Promise<string> {
  const reader = Bun.stdin.stream().getReader();
  const chunks: Uint8Array[] = [];
  let size = 0;
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    size += value.byteLength;
    if (size > MAX_FRAME_BYTES) throw new Error("provider request exceeds 1 MiB");
    chunks.push(value);
  }
  const merged = new Uint8Array(size);
  let offset = 0;
  for (const chunk of chunks) { merged.set(chunk, offset); offset += chunk.byteLength; }
  return new TextDecoder().decode(merged).trim();
}

function parseRequest(text: string): { type: "inference_request"; id: string; provider: string; model: string; context: Context; options: ModelsSimpleStreamOptions } {
  const value: unknown = JSON.parse(text);
  if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("invalid provider request");
  const raw = value as Record<string, unknown>;
  const fields = ["type", "id", "provider", "model", "context", "options"];
  if (Object.keys(raw).length !== fields.length || fields.some(field => !(field in raw)) || raw.type !== "inference_request" || typeof raw.id !== "string" || !raw.id || typeof raw.provider !== "string" || !raw.provider || typeof raw.model !== "string" || !raw.model || !raw.context || typeof raw.context !== "object" || Array.isArray(raw.context) || !raw.options || typeof raw.options !== "object" || Array.isArray(raw.options)) throw new Error("invalid provider request");
  const options = raw.options as Record<string, unknown>;
  if (Object.keys(options).some(key => key !== "maxTokens") || (options.maxTokens !== undefined && (!Number.isInteger(options.maxTokens) || (options.maxTokens as number) <= 0))) throw new Error("invalid provider options");
  return raw as ReturnType<typeof parseRequest>;
}

function emit(value: unknown) {
  const encoded = JSON.stringify(value);
  if (Buffer.byteLength(encoded) > MAX_FRAME_BYTES) throw new Error("provider response exceeds 1 MiB");
  process.stdout.write(encoded + "\n");
}

try {
  const request = parseRequest(await readBoundedInput());
  const runtime = await ModelRuntime.create({ credentials: new InMemoryCredentialStore(), modelsPath: null, allowModelNetwork: true });
  const model = runtime.getModel(request.provider, request.model);
  if (!model) throw new Error("hidden provider route is unavailable");
  const stream = runtime.streamSimple(model, request.context, { ...request.options, cacheRetention: "none" });
  let terminal = false;
  for await (const event of stream) {
    if (event.type === "text_delta") emit({ type: "delta", id: request.id, delta: event.delta });
    if (event.type === "done") { emit({ type: "result", id: request.id, message: event.message }); terminal = true; }
    if (event.type === "error") { emit({ type: "error", id: request.id, error: event.error.errorMessage || event.reason }); terminal = true; }
  }
  if (!terminal) throw new Error("provider stream ended without a terminal event");
} catch (error) {
  emit({ type: "error", id: "provider-process", error: error instanceof Error ? error.message : String(error) });
  process.exitCode = 1;
}
