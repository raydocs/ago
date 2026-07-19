import { describe, expect, test } from "bun:test";
import { pathToFileURL } from "node:url";
import { PluginRuntime, RPCFailure, DEFAULT_PERMISSION_PLUGIN_ID } from "./runtime";

const params = (plugins: any[] = []) => ({ supportedProtocolVersions: [1], generation: 7, workspaceUri: null, plugins, capabilities: { ui: [], renderMode: "headless" }, limits: { maxMessageBytes: 10000, maxInflight: 4 } });
const fixture = { pluginId: "test.plugin", entryUri: pathToFileURL(import.meta.dir + "/test-plugin.ts").href, config: {} };

describe("trusted plugin runtime", () => {
  test("initializes registrations and immutable default last", async () => {
    const result = await new PluginRuntime().initialize(params([fixture]));
    expect(result.generation).toBe(7);
    expect(result.plugins.map(p => p.pluginId)).toEqual(["test.plugin", DEFAULT_PERMISSION_PLUGIN_ID]);
    expect(result.plugins[0].tools?.[0].name).toBe("echo");
    expect(result.plugins[0].commands?.[0].id).toBe("run");
  });
  test("default permission hook allows", async () => {
    const runtime = new PluginRuntime(); await runtime.initialize(params());
    expect(await runtime.invoke("hook.invoke", { generation: 7, invocationId: "i", threadId: "thread-1", turnId: "turn-1", deadlineUnixMs: 0, payload: { hook: "tool.call" } })).toEqual([{ action: "allow" }]);
  });
  test("rejects duplicate plugin registrations", async () => {
    await expect(new PluginRuntime().initialize(params([fixture, fixture]))).rejects.toBeInstanceOf(RPCFailure);
    const duplicateTool = { pluginId: "duplicate.plugin", entryUri: pathToFileURL(import.meta.dir + "/duplicate-plugin.ts").href, config: {} };
    await expect(new PluginRuntime().initialize(params([duplicateTool]))).rejects.toMatchObject({ code: "INVALID_RESULT" });
  });
  test("cancellation aborts only the matching invocation", async () => {
    const runtime = new PluginRuntime(); await runtime.initialize(params([fixture]));
    const pending = runtime.invoke("hook.invoke", { generation: 7, invocationId: "cancel-me", threadId: "thread-1", turnId: "turn-1", deadlineUnixMs: 0, payload: { hook: "cancel" } });
    await Bun.sleep(5); runtime.cancel({ generation: 7, invocationId: "cancel-me", reason: "test" });
    expect(await pending).toEqual(["aborted"]);
  });
  test("ctx.ai.ask sends correlated bounded reverse RPC and validates result", async () => {
    const emitted: any[] = []; const runtime = new PluginRuntime(message => emitted.push(message));
    await runtime.initialize(params([fixture]));
    const pending = runtime.invoke("hook.invoke", { generation: 7, invocationId: "ask-1", threadId: "thread-authoritative", turnId: "turn-authoritative", deadlineUnixMs: Date.now() + 1000, payload: { hook: "ask", payload: { question: "Proceed?" } } });
    await Bun.sleep(1); const request = emitted.find(m => m.method === "ai.ask");
    expect(request.params).toMatchObject({ question: "Proceed?", generation: 7, pluginId: "test.plugin", invocationId: "ask-1", threadId: "thread-authoritative", turnId: "turn-authoritative" });
    runtime.resolveResponse({ type: "response", id: request.id, ok: true, result: { answer: "yes", probability: .8, reason: "safe" } });
    expect(await pending).toEqual([{ answer: "yes", probability: .8, reason: "safe" }]);
  });
  test("ctx.ai.ask fails closed on malformed results and cancellation", async () => {
    const emitted: any[] = []; const runtime = new PluginRuntime(message => emitted.push(message)); await runtime.initialize(params([fixture]));
    const malformed = runtime.invoke("hook.invoke", { generation: 7, invocationId: "bad", threadId: "thread-1", turnId: "turn-1", deadlineUnixMs: Date.now() + 1000, payload: { hook: "ask", payload: { question: "Proceed?" } } });
    await Bun.sleep(1); let request = emitted.find(m => m.method === "ai.ask"); runtime.resolveResponse({ type: "response", id: request.id, ok: true, result: { answer: "yes", probability: NaN, reason: "bad" } });
    await expect(malformed).rejects.toMatchObject({ code: "INVALID_RESULT" });
    const cancelled = runtime.invoke("hook.invoke", { generation: 7, invocationId: "cancel-ask", threadId: "thread-1", turnId: "turn-1", deadlineUnixMs: Date.now() + 1000, payload: { hook: "ask", payload: { question: "Proceed?" } } });
    await Bun.sleep(1); runtime.cancel({ generation: 7, invocationId: "cancel-ask", reason: "stop" });
    await expect(cancelled).rejects.toMatchObject({ code: "CANCELLED" });
  });
  test("invocation completion cancels an unawaited ai.ask", async () => {
    const emitted: any[] = []; const runtime = new PluginRuntime(message => emitted.push(message)); await runtime.initialize(params([fixture]));
    expect(await runtime.invoke("hook.invoke", { generation: 7, invocationId: "unawaited", threadId: "thread-1", turnId: "turn-1", deadlineUnixMs: Date.now() + 1000, payload: { hook: "ask-unawaited" } })).toEqual(["done"]);
    await Bun.sleep(1); const request = emitted.find(m => m.method === "ai.ask");
    expect(request).toBeDefined();
    expect(runtime.resolveResponse({ type: "response", id: request.id, ok: true, result: { answer: "yes", probability: 1, reason: "late" } })).toBe(false);
  });
  test("disposes plugins and registrations in reverse order", async () => {
    const runtime = new PluginRuntime(); const order: string[] = [];
    (runtime as any).generation = 7;
    (runtime as any).plugins = [{ id: "a", tools: [], commands: [], hooks: [], disposables: [{ dispose: () => order.push("a1") }, { dispose: () => order.push("a2") }] }, { id: "b", tools: [], commands: [], hooks: [], disposables: [{ dispose: () => order.push("b1") }] }];
    await runtime.dispose({ generation: 7, reason: "test" }); expect(order).toEqual(["b1", "a2", "a1"]);
  });
  test("child stdout remains JSONL when plugin calls console.log", async () => {
    const child = Bun.spawn(["bun", import.meta.dir + "/main.ts"], { stdin: "pipe", stdout: "pipe", stderr: "pipe" });
    child.stdin.write(JSON.stringify({ type: "request", id: "1", method: "initialize", params: params([fixture]) }) + "\n"); child.stdin.end();
    const output = await new Response(child.stdout).text(); await child.exited;
    const lines = output.trim().split("\n"); expect(lines).toHaveLength(1); expect(() => JSON.parse(lines[0])).not.toThrow(); expect(JSON.parse(lines[0]).ok).toBe(true);
  });
});
