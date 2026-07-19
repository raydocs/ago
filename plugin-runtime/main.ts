#!/usr/bin/env bun
import { PluginRuntime, RPCFailure } from "./runtime";

const rawWrite = process.stdout.write.bind(process.stdout);
const send = (message: any) => rawWrite(JSON.stringify(message) + "\n");
console.log = (...args: any[]) => console.error(...args);
(process.stdout as any).write = (chunk: any, ...rest: any[]) => (process.stderr as any).write(chunk, ...rest);
const runtime = new PluginRuntime(send);
let initialized = false;

async function handle(message: any) {
  let terminal: any;
  try {
    let result: any;
    if (!initialized) throw new RPCFailure("INVALID_REQUEST", "initialize must be first");
    if (message.method === "initialize") throw new RPCFailure("INVALID_REQUEST", "initialize may only be called once");
    if (message.method === "host.dispose") result = await runtime.dispose(message.params);
    else result = await runtime.invoke(message.method, message.params);
    terminal = { type: "response", id: message.id, ok: true, result };
  } catch (error: any) {
    const failure = error instanceof RPCFailure ? error : new RPCFailure("PLUGIN_ERROR", error?.message ?? String(error));
    terminal = { type: "response", id: message.id, ok: false, error: { code: failure.code, message: failure.message, ...(failure.data === undefined ? {} : { data: failure.data }) } };
  }
  send(terminal);
}

for await (const line of console) {
  if (!line.trim()) continue;
  let message: any;
  try { message = JSON.parse(line); }
  catch { console.error("invalid JSON received before a request could be identified"); continue; }
  if (message.type === "response" && runtime.resolveResponse(message)) continue;
  if (message.type === "notification" && message.method === "invocation.cancel") {
    try { runtime.cancel(message.params); } catch {} continue;
  }
  if (message.type !== "request" || !message.id) continue;
  if (!initialized && message.method === "initialize") {
    runtime.initialize(message.params).then(
      result => { initialized = true; send({ type: "response", id: message.id, ok: true, result }); },
      (error: any) => { const failure = error instanceof RPCFailure ? error : new RPCFailure("PLUGIN_ERROR", error?.message ?? String(error)); send({ type: "response", id: message.id, ok: false, error: { code: failure.code, message: failure.message } }); },
    );
  } else {
    void handle(message);
  }
}
