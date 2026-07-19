import { expect, test } from "bun:test";
import { createAssistantMessageEventStream, fauxAssistantMessage, fauxProvider, fauxToolCall } from "@earendil-works/pi-ai";
import type { AssistantMessage, Context, Model, SimpleStreamOptions } from "@earendil-works/pi-ai";
import { ModelRuntime } from "@earendil-works/pi-coding-agent";
import { createAgoSidecar } from "../src/index.js";

let model: Model<any>;
process.env.FAUX_API_KEY = "faux-test-harness";
const runtime = await ModelRuntime.create({ modelsPath: null, allowModelNetwork: false });
await runtime.setRuntimeApiKey("openai", "faux-test-harness");

type Script = { chunks?: string[]; tool?: { id: string; text: string; name?: string }; reason?: "stop" | "toolUse" };

function provider(script: (context: Context, options?: SimpleStreamOptions) => Script | Promise<Script>) {
  const contexts: Context[] = [];
  const faux = fauxProvider({ api: "ago-faux", provider: "openai", models: [{ id: "scripted" }], tokensPerSecond: 1_000_000, tokenSize: { min: 2, max: 3 } });
  runtime.registerProvider("openai", { api: "ago-faux", apiKey: "faux", streamSimple: faux.provider.streamSimple.bind(faux.provider), models: faux.models.map(m => ({ ...m, api: undefined })) });
  model = runtime.getModel("openai", "scripted")!;
  faux.setResponses([async (context, options) => {
    contexts.push({ ...context, messages: structuredClone(context.messages) });
    const step = await script(context, options);
    return step.tool ? fauxAssistantMessage(fauxToolCall(step.tool.name ?? "ago_echo", { text: step.tool.text }, { id: step.tool.id }), { stopReason: "toolUse" }) : fauxAssistantMessage((step.chunks ?? []).join(""));
  }, async (context, options) => { contexts.push({ ...context, messages: structuredClone(context.messages) }); const step = await script(context, options); return fauxAssistantMessage((step.chunks ?? []).join("")); }]);
  return { contexts, close: () => runtime.unregisterProvider("openai") };
}

function providerMessage(message: AssistantMessage) {
  runtime.registerProvider("openai", {
    api: "ago-faux",
    apiKey: "faux",
    streamSimple() {
      const stream = createAssistantMessageEventStream();
      queueMicrotask(() => {
        if (message.stopReason === "error" || message.stopReason === "aborted") {
          stream.push({ type: "error", reason: message.stopReason, error: message });
        } else {
          stream.push({ type: "done", reason: message.stopReason, message });
        }
        stream.end(message);
      });
      return stream;
    },
    models: [{ id: "scripted", name: "Scripted", reasoning: false, input: ["text"], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 200_000, maxTokens: 32_000 }],
  });
  model = runtime.getModel("openai", "scripted")!;
  return () => runtime.unregisterProvider("openai");
}

test("restores Ago transcript in memory and streams only closed Ago events", async () => {
  const p = provider(() => ({ chunks: ["HE", "LLO"] }));
  const sidecar = await createAgoSidecar({ transcript: [{ role: "user", text: "restored", at: 1 }], model, modelRuntime: runtime });
  expect(sidecar.session.sessionManager.getSessionFile()).toBeUndefined();
  expect(sidecar.session.sessionManager.isPersisted()).toBeFalse();
  await sidecar.command({ type: "prompt", text: "go" });
  expect(sidecar.events).not.toContainEqual({ type: "stopped", reason: "error" });
  expect(p.contexts[0]!.messages.map(m => m.role)).toEqual(["user", "user"]);
  expect(sidecar.events.filter(e => ["started", "text", "stopped", "settled"].includes(e.type)).map(e => e.type === "text" ? e.delta : e.type).join("|")).toContain("HE");
  expect(sidecar.events.filter(e => e.type === "text").map(e => e.delta).join("")).toBe("HELLO");
  sidecar.close(); p.close();
});

test("completed assistant retains exact final provider usage", async () => {
  const closeProvider = providerMessage({
    role: "assistant",
    api: "ago-faux",
    provider: "openai",
    model: "scripted",
    responseModel: "provider-model-2026-07-19",
    responseId: "request-123",
    content: [{ type: "text", text: "measured" }],
    usage: {
      input: 101,
      output: 23,
      cacheRead: 47,
      cacheWrite: 11,
      totalTokens: 997,
      cost: { input: 0.00101, output: 0.00046, cacheRead: 0.000047, cacheWrite: 0.00011, total: 9.99 },
    },
    stopReason: "stop",
    timestamp: 1234,
  });
  const sidecar = await createAgoSidecar({ transcript: [], model, modelRuntime: runtime });
  await sidecar.command({ type: "prompt", text: "measure" });

  expect(sidecar.events.find(event => event.type === "assistant_completed")).toEqual({
    type: "assistant_completed",
    message: {
      role: "assistant", api: "ago-faux", provider: "openai", model: "scripted", stopReason: "stop",
      content: [{ type: "text", text: "measured" }], at: 1234,
    },
    provider_usage: {
      provider: "openai",
      model: "provider-model-2026-07-19",
      request_id: "request-123",
      status: "final",
      usage: { input_tokens: 101, output_tokens: 23, cache_read_tokens: 47, cache_write_tokens: 11, total_tokens: 997 },
      cost: { input: 0.00101, output: 0.00046, cache_read: 0.000047, cache_write: 0.00011, total: 9.99 },
    },
  });
  sidecar.close(); closeProvider();
});

test("aborted assistant marks all reported token buckets as provisional", async () => {
  const message: AssistantMessage = {
    role: "assistant", api: "ago-faux", provider: "anthropic", model: "requested-model", responseId: "request-aborted",
    content: [], stopReason: "aborted", errorMessage: "cancelled", timestamp: 1234,
    usage: {
      input: 10, output: 8, cacheRead: 3, cacheWrite: 2, cacheWrite1h: 1, reasoning: 5, totalTokens: 23,
      cost: { input: 0.1, output: 0.2, cacheRead: 0.03, cacheWrite: 0.04, total: 0.37 },
    },
  };
  const closeProvider = providerMessage(message);
  const sidecar = await createAgoSidecar({ transcript: [], model, modelRuntime: runtime });
  await sidecar.command({ type: "prompt", text: "cancel" });

  const completed = sidecar.events.find(event => event.type === "assistant_completed");
  expect(completed).toMatchObject({
    provider_usage: {
      provider: "anthropic", model: "requested-model", request_id: "request-aborted", status: "provisional",
      usage: { input_tokens: 10, output_tokens: 8, cache_read_tokens: 3, cache_write_tokens: 2, cache_write_1h_tokens: 1, reasoning_tokens: 5, total_tokens: 23 },
      cost: { input: 0.1, output: 0.2, cache_read: 0.03, cache_write: 0.04, total: 0.37 },
    },
  });
  sidecar.close(); closeProvider();
});

test("completed assistant rejects malformed provider usage instead of normalizing it", async () => {
  const base: AssistantMessage = {
    role: "assistant", api: "ago-faux", provider: "openai", model: "scripted", responseId: "request-invalid",
    content: [{ type: "text", text: "invalid" }], stopReason: "stop", timestamp: 1234,
    usage: { input: 1, output: 2, cacheRead: 3, cacheWrite: 4, totalTokens: 10, cost: { input: 0.1, output: 0.2, cacheRead: 0.3, cacheWrite: 0.4, total: 1 } },
  };
  const malformed: [string, (message: any) => void][] = [
    ["negative token", message => { message.usage.input = -1; }],
    ["fractional token", message => { message.usage.output = 1.5; }],
    ["unsafe token", message => { message.usage.totalTokens = Number.MAX_SAFE_INTEGER + 1; }],
    ["nonfinite token", message => { message.usage.cacheRead = Infinity; }],
    ["negative cost", message => { message.usage.cost.total = -0.01; }],
    ["nonfinite cost", message => { message.usage.cost.output = Number.NaN; }],
    ["unknown usage field", message => { message.usage.estimated = 99; }],
    ["unknown cost field", message => { message.usage.cost.currency = "USD"; }],
    ["malformed usage", message => { message.usage = []; }],
    ["empty request identity", message => { message.responseId = ""; }],
  ];
  for (const [name, mutate] of malformed) {
    const message = structuredClone(base) as any;
    mutate(message);
    const closeProvider = providerMessage(message);
    const sidecar = await createAgoSidecar({ transcript: [], model, modelRuntime: runtime });
    await sidecar.command({ type: "prompt", text: name }).catch(() => undefined);
    expect(sidecar.events.some(event => event.type === "assistant_completed" && event.provider_usage.request_id === "request-invalid"), name).toBeFalse();
    expect(sidecar.events).toContainEqual({ type: "stopped", reason: "error" });
    sidecar.close(); closeProvider();
  }
});

test("restart from only Ago transcript rebuilds identical provider context", async () => {
  const transcript = [
    { role: "user" as const, text: "restored", at: 1 },
    { role: "assistant" as const, api: "ago-faux", provider: "openai", model: "scripted", stopReason: "stop" as const, content: [{ type: "text" as const, text: "prior" }], at: 2 }
  ];
  const firstProvider = provider(() => ({ chunks: ["same"] }));
  const first = await createAgoSidecar({ transcript, model, modelRuntime: runtime });
  await first.command({ type: "prompt", text: "continue" });
  const firstContext = firstProvider.contexts[0]!.messages.map(({ timestamp: _timestamp, ...message }) => message);
  expect(first.session.sessionManager.getSessionFile()).toBeUndefined();
  first.close(); firstProvider.close();

  const secondProvider = provider(() => ({ chunks: ["same"] }));
  const second = await createAgoSidecar({ transcript, model, modelRuntime: runtime });
  await second.command({ type: "prompt", text: "continue" });
  expect(secondProvider.contexts[0]!.messages.map(({ timestamp: _timestamp, ...message }) => message)).toEqual(firstContext);
  expect(second.session.sessionManager.getSessionFile()).toBeUndefined();
  second.close(); secondProvider.close();
});

test("restart from Ago summary plus tail rebuilds compaction-aware provider context", async () => {
  const transcript = [
    { role: "summary" as const, text: "durable recovery summary", at: 10 },
    { role: "user" as const, text: "tail", at: 11 },
  ];
  const firstProvider = provider(() => ({ chunks: ["same"] }));
  const first = await createAgoSidecar({ transcript, model, modelRuntime: runtime });
  await first.command({ type: "prompt", text: "continue" });
  const semantic = firstProvider.contexts[0]!.messages.map(({ timestamp: _timestamp, ...message }) => message);
  expect(semantic[0]).toMatchObject({ role: "user" });
  expect(JSON.stringify(semantic[0])).toContain("durable recovery summary");
  expect(JSON.stringify(semantic)).toContain("tail");
  expect(JSON.stringify(semantic)).not.toContain("summarized-away");
  first.close(); firstProvider.close();

  const secondProvider = provider(() => ({ chunks: ["same"] }));
  const second = await createAgoSidecar({ transcript, model, modelRuntime: runtime });
  await second.command({ type: "prompt", text: "continue" });
  expect(secondProvider.contexts[0]!.messages.map(({ timestamp: _timestamp, ...message }) => message)).toEqual(semantic);
  expect(second.session.sessionManager.getSessionFile()).toBeUndefined();
  second.close(); secondProvider.close();
});

test("bridges exactly one correlated external tool result into next provider context", async () => {
  let calls = 0;
  const p = provider(() => ++calls === 1 ? { tool: { id: "c1", text: "abc", name: "plugin_echo" }, reason: "toolUse" } : { chunks: ["done"] });
  let sidecar: Awaited<ReturnType<typeof createAgoSidecar>>;
  sidecar = await createAgoSidecar({ transcript: [], tools: [{ name: "plugin_echo", description: "Plugin echo", inputSchema: { type: "object", properties: { text: { type: "string" } }, required: ["text"] } }], model, modelRuntime: runtime, onEvent: e => { if (e.type === "tool_invocation") void sidecar.command({ type: "tool_result", callId: e.callId, name: "plugin_echo", output: "PLUGIN:abc" }); } });
  await sidecar.command({ type: "prompt", text: "tool" });
  expect(sidecar.events.filter(e => e.type === "tool_invocation")).toEqual([{ type: "tool_invocation", callId: "c1", name: "plugin_echo", input: { text: "abc" } }]);
  expect(p.contexts).toHaveLength(2);
  expect(JSON.stringify(p.contexts[1])).toContain("PLUGIN:abc");
  sidecar.close(); p.close();
});

test("completed canonical tool call roundtrips before its correlated tool result", async () => {
  let calls = 0;
  const firstProvider = provider(() => ++calls === 1 ? { tool: { id: "c1", text: "abc" } } : { chunks: ["done"] });
  let first: Awaited<ReturnType<typeof createAgoSidecar>>;
  first = await createAgoSidecar({ transcript: [], model, modelRuntime: runtime, onEvent: event => {
    if (event.type === "tool_invocation") void first.command({ type: "tool_result", callId: event.callId, name: "ago_echo", output: "AGO:abc" });
  } });
  await first.command({ type: "prompt", text: "tool" });
  const completed = first.events.find(event => event.type === "assistant_completed" && event.message.stopReason === "toolUse");
  if (!completed || completed.type !== "assistant_completed") throw new Error("missing completed tool call");
  expect(typeof completed.message.at).toBe("number");
  expect(completed.message).toEqual({
    role: "assistant", api: "ago-faux", provider: "openai", model: "scripted", stopReason: "toolUse",
    content: [{ type: "toolCall", callId: "c1", name: "ago_echo", input: { text: "abc" } }],
    at: completed.message.at,
  });
  expect(completed.provider_usage).toMatchObject({ provider: "openai", model: "scripted", request_id: null, status: "final" });
  const semantic = (messages: Context["messages"]) => messages.slice(0, 3).map(message => {
    if (message.role === "user") return { role: message.role, text: typeof message.content === "string" ? message.content : message.content.map(block => block.type === "text" ? block.text : "").join("") };
    if (message.role === "assistant") return { role: message.role, api: message.api, provider: message.provider, model: message.model, stopReason: message.stopReason, content: message.content };
    if (message.role === "toolResult") return { role: message.role, callId: message.toolCallId, name: message.toolName, content: message.content, error: message.isError };
    return message;
  });
  const expected = semantic(firstProvider.contexts[1]!.messages);
  first.close(); firstProvider.close();

  const transcript = [
    { role: "user" as const, text: "tool", at: 1 },
    completed.message,
    { role: "tool" as const, callId: "c1", name: "ago_echo" as const, text: "AGO:abc", error: false, at: 2 },
  ];
  const restartedProvider = provider(() => ({ chunks: ["continued"] }));
  const restarted = await createAgoSidecar({ transcript, model, modelRuntime: runtime });
  await restarted.command({ type: "prompt", text: "next" });
  expect(semantic(restartedProvider.contexts[0]!.messages)).toEqual(expected);
  restarted.close(); restartedProvider.close();
});

test("steering enters the active run before its next provider turn", async () => {
  let enterFirst!: () => void;
  const firstEntered = new Promise<void>(resolve => { enterFirst = resolve; });
  let releaseFirst!: () => void;
  const firstReleased = new Promise<void>(resolve => { releaseFirst = resolve; });
  let calls = 0;
  const p = provider(async () => {
    if (++calls === 1) {
      enterFirst();
      await firstReleased;
      return { chunks: ["first"] };
    }
    return { chunks: ["second"] };
  });
  const sidecar = await createAgoSidecar({ transcript: [], model, modelRuntime: runtime });
  const prompt = sidecar.command({ type: "prompt", text: "start" });
  await firstEntered;
  await sidecar.command({ type: "steer", text: "urgent" });
  releaseFirst();
  await prompt;

  expect(p.contexts).toHaveLength(2);
  expect(JSON.stringify(p.contexts[1]!.messages)).toContain("urgent");
  expect(sidecar.events.filter(event => event.type === "started")).toHaveLength(1);
  expect(sidecar.events.at(-1)).toEqual({ type: "settled" });
  sidecar.close(); p.close();
});

test("follow-up waits for the active turn to stop before its next provider turn", async () => {
  let enterFirst!: () => void;
  const firstEntered = new Promise<void>(resolve => { enterFirst = resolve; });
  let releaseFirst!: () => void;
  const firstReleased = new Promise<void>(resolve => { releaseFirst = resolve; });
  let calls = 0;
  const p = provider(async () => {
    if (++calls === 1) {
      enterFirst();
      await firstReleased;
      return { chunks: ["first"] };
    }
    return { chunks: ["second"] };
  });
  const sidecar = await createAgoSidecar({ transcript: [], model, modelRuntime: runtime });
  const prompt = sidecar.command({ type: "prompt", text: "start" });
  await firstEntered;
  await sidecar.command({ type: "follow_up", text: "after" });
  expect(p.contexts).toHaveLength(1);
  releaseFirst();
  await prompt;

  expect(p.contexts).toHaveLength(2);
  expect(JSON.stringify(p.contexts[1]!.messages)).toContain("after");
  expect(sidecar.events.filter(event => event.type === "started")).toHaveLength(1);
  expect(sidecar.events.at(-1)).toEqual({ type: "settled" });
  sidecar.close(); p.close();
});

test("abort propagates to the provider and settles without later work", async () => {
  let providerEntered!: () => void;
  const entered = new Promise<void>(resolve => { providerEntered = resolve; });
  const p = provider(async (_context, options) => {
    providerEntered();
    await new Promise<void>(resolve => options?.signal?.addEventListener("abort", () => resolve(), { once: true }));
    return { chunks: ["late"] };
  });
  const sidecar = await createAgoSidecar({ transcript: [], model, modelRuntime: runtime });
  const prompt = sidecar.command({ type: "prompt", text: "start" });
  await entered;
  await sidecar.command({ type: "abort" });
  await prompt;

  expect(p.contexts).toHaveLength(1);
  expect(sidecar.events).toContainEqual({ type: "stopped", reason: "aborted" });
  expect(sidecar.events.at(-1)).toEqual({ type: "settled" });
  const settledCount = sidecar.events.length;
  await Bun.sleep(20);
  expect(sidecar.events).toHaveLength(settledCount);
  sidecar.close(); p.close();
});

test("deterministic Ago compaction stays memory-only", async () => {
  const p = provider(() => ({ chunks: ["ok"] }));
  const sidecar = await createAgoSidecar({ transcript: [{ role: "user", text: "old", at: 1 }], model, modelRuntime: runtime });
  await sidecar.compact();
  expect(sidecar.events.slice(-2)).toEqual([{ type: "compact_start" }, { type: "compact_end", summary: "AGO_SUMMARY", aborted: false }]);
  expect(sidecar.session.sessionManager.buildSessionContext().messages[0]).toMatchObject({ role: "compactionSummary", summary: "AGO_SUMMARY" });
  expect(sidecar.session.sessionManager.getSessionFile()).toBeUndefined();
  await sidecar.command({ type: "prompt", text: "after compact" });
  expect(JSON.stringify(p.contexts[0]!.messages)).toContain("AGO_SUMMARY");
  expect(JSON.stringify(p.contexts[0]!.messages)).not.toContain('"old"');
  sidecar.close(); p.close();
});
