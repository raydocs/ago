import { expect, test } from "bun:test";
import { fauxAssistantMessage, fauxProvider } from "@earendil-works/pi-ai";
import { ModelRuntime } from "@earendil-works/pi-coding-agent";
import { runJsonlProcess } from "../src/process.js";
import { createAgoSidecar } from "../src/index.js";

if (process.env.AGO_PROCESS_TEST_CHILD === "1") {
  const runtime = await ModelRuntime.create({ modelsPath: null, allowModelNetwork: false });
  await runtime.setRuntimeApiKey("openai", "test-only");
  const faux = fauxProvider({ api: "ago-faux", provider: "openai", models: [{ id: "scripted" }], tokensPerSecond: 1_000_000 });
  faux.setResponses([async (_context, options) => {
    await Bun.sleep(80);
    if (options?.signal?.aborted) return fauxAssistantMessage("", { stopReason: "aborted" });
    return fauxAssistantMessage("STREAMED");
  }]);
  runtime.registerProvider("openai", { api: "ago-faux", apiKey: "test-only", streamSimple: faux.provider.streamSimple.bind(faux.provider), models: faux.models.map(model => ({ ...model, api: undefined })) });
  await runJsonlProcess({
    create: async (init, onEvent) => createAgoSidecar({ transcript: init.transcript, tools: init.tools, cwd: init.cwd, modelRuntime: runtime, model: runtime.getModel(init.provider, init.model)!, onEvent }),
    stdin: Bun.stdin.stream(), stdout: Bun.stdout, stderr: Bun.stderr,
  });
  process.exit(process.exitCode ?? 0);
}

function child() {
  return Bun.spawn([process.execPath, "test/process.test.ts"], {
    cwd: import.meta.dir + "/..",
    env: { ...process.env, AGO_PROCESS_TEST_CHILD: "1" },
    stdin: "pipe", stdout: "pipe", stderr: "pipe",
  });
}

const init = { type: "initialize", transcript: [{ role: "user", text: "restored", at: 1 }], cwd: "/tmp", provider: "openai", model: "scripted", tools: [{ name: "ago_echo", description: "Echo", inputSchema: { type: "object", properties: { text: { type: "string" } }, required: ["text"] } }] };
const line = (value: unknown) => JSON.stringify(value) + "\n";

test("spawned sidecar initializes then streams strict events", async () => {
  const proc = child();
  proc.stdin.write(line(init) + line({ type: "prompt", text: "go" }));
  const reader = proc.stdout.getReader();
  const decoder = new TextDecoder();
  let stdout = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    stdout += decoder.decode(value, { stream: true });
    const completeLines = stdout.split("\n");
    completeLines.pop();
    if (completeLines.filter(Boolean).some(value => JSON.parse(value).type === "settled")) break;
  }
  proc.stdin.end();
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    stdout += decoder.decode(value, { stream: true });
  }
  stdout += decoder.decode();
  const stderr = await new Response(proc.stderr).text();
  expect(await proc.exited).toBe(0);
  const events = stdout.trim().split("\n").map(value => JSON.parse(value));
  expect(events.some(event => event.type === "started")).toBeTrue();
  expect(events.filter(event => event.type === "text").map(event => event.delta).join("")).toBe("STREAMED");
  expect(events.at(-1)).toEqual({ type: "settled" });
  expect(stderr).toBe("");
  expect(stdout.split("\n").filter(Boolean).every(value => { try { JSON.parse(value); return true; } catch { return false; } })).toBeTrue();
});

test("stdin remains duplex so abort reaches an active prompt", async () => {
  const proc = child();
  proc.stdin.write(line(init) + line({ type: "prompt", text: "go" }));
  await Bun.sleep(20);
  proc.stdin.write(line({ type: "abort" }));
  proc.stdin.end();
  const events = (await new Response(proc.stdout).text()).trim().split("\n").filter(Boolean).map(value => JSON.parse(value));
  expect(await proc.exited).toBe(0);
  expect(events).toContainEqual({ type: "stopped", reason: "aborted" });
  expect(events.at(-1)).toEqual({ type: "settled" });
  const count = events.length;
  await Bun.sleep(30);
  expect(events).toHaveLength(count);
});

test.each([
  ["malformed", "{nope}\n"],
  ["oversized", "x".repeat(1024 * 1024 + 1) + "\n"],
  ["unknown field", line({ ...init, surprise: true })],
])("fails closed on %s input", async (_name, input) => {
  const proc = child();
  proc.stdin.write(input);
  proc.stdin.end();
  expect(await proc.exited).not.toBe(0);
  expect(await new Response(proc.stdout).text()).toBe("");
  expect(await new Response(proc.stderr).text()).not.toBe("");
});

test.each([
  ["legacy assistant", { role: "assistant", text: "old", at: 2 }],
  ["unknown assistant metadata", { role: "assistant", api: "ago-faux", provider: "openai", model: "scripted", stopReason: "stop", content: [{ type: "text", text: "ok" }], at: 2, surprise: true }],
  ["unknown content block", { role: "assistant", api: "ago-faux", provider: "openai", model: "scripted", stopReason: "stop", content: [{ type: "thinking", thinking: "no" }], at: 2 }],
  ["unknown tool-call field", { role: "assistant", api: "ago-faux", provider: "openai", model: "scripted", stopReason: "toolUse", content: [{ type: "toolCall", callId: "c1", name: "ago_echo", input: { text: "x" }, extra: true }], at: 2 }],
])("rejects %s in initialized transcript", async (_name, message) => {
  const proc = child();
  proc.stdin.write(line({ ...init, transcript: [message] }));
  proc.stdin.end();
  expect(await proc.exited).not.toBe(0);
  expect(await new Response(proc.stderr).text()).toContain("protocol error");
});

test("accepts one leading Ago recovery summary", async () => {
  const proc = child();
  proc.stdin.write(line({ ...init, transcript: [{ role: "summary", text: "recovered", at: 1 }, { role: "user", text: "tail", at: 2 }] }));
  proc.stdin.end();
  expect(await proc.exited).toBe(0);
  expect(await new Response(proc.stderr).text()).toBe("");
});

test.each([
  [[{ role: "user", text: "tail", at: 1 }, { role: "summary", text: "late", at: 2 }]],
  [[{ role: "summary", text: "one", at: 1 }, { role: "summary", text: "two", at: 2 }]],
])("rejects misplaced or repeated recovery summaries", async transcript => {
  const proc = child();
  proc.stdin.write(line({ ...init, transcript }));
  proc.stdin.end();
  expect(await proc.exited).not.toBe(0);
  expect(await new Response(proc.stderr).text()).toContain("protocol error");
});

test("spawn leaves no Pi session file", async () => {
  const cwd = `/tmp/ago-pi-${crypto.randomUUID()}`;
  const proc = child();
  proc.stdin.write(line({ ...init, cwd }));
  proc.stdin.end();
  expect(await proc.exited).toBe(0);
  expect(await new Response(proc.stdout).text()).toBe("");
  expect(await Bun.file(cwd).exists()).toBeFalse();
});
