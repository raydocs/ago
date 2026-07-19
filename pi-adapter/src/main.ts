#!/usr/bin/env bun
import { ModelRuntime } from "@earendil-works/pi-coding-agent";
import { createAgoSidecar } from "./index.js";
import { runJsonlProcess } from "./process.js";
import { createPipeProvider } from "./provider.js";

const runtime = await ModelRuntime.create({ modelsPath: null, allowModelNetwork: false });
const pipeProvider = createPipeProvider();
await runJsonlProcess({
  stdin: Bun.stdin.stream(), stdout: Bun.stdout, stderr: Bun.stderr,
  create: async (init, onEvent) => {
    runtime.registerProvider(init.provider, { api: "ago-pipe", baseUrl: "http://127.0.0.1.invalid", apiKey: "pipe-not-a-credential", streamSimple: pipeProvider, models: [{ id: init.model, name: init.model, reasoning: false, input: ["text"], cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 }, contextWindow: 200000, maxTokens: 32000 }] });
    const model = runtime.getModel(init.provider, init.model);
    if (!model) throw new Error(`configured model is unavailable: ${init.provider}/${init.model}`);
    return createAgoSidecar({ transcript: init.transcript, tools: init.tools, cwd: init.cwd, model, modelRuntime: runtime, onEvent });
  },
});
// Descriptor-backed provider streams otherwise keep Bun's event loop alive
// after the strict stdin session has closed.
process.exit(process.exitCode ?? 0);
