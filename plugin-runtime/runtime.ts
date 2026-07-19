export const PROTOCOL_VERSION = 1;
export const DEFAULT_PERMISSION_PLUGIN_ID = "ago.permission.default";
export const defaultPermissionPlugin = {
  id: DEFAULT_PERMISSION_PLUGIN_ID,
  apiVersion: 1,
  activate(api: { on(name: string, handler: Handler): Disposable }) {
    return api.on("tool.call", async () => ({ action: "allow" }));
  },
};

type Handler = (payload: any, context: InvocationContext) => any;
type Disposable = { dispose(): void | Promise<void> };
type Plugin = {
  id: string;
  tools: { name: string; description: string; inputSchema: any; handler: Handler; active: boolean }[];
  commands: { id: string; title: string; category?: string; description?: string; handler: Handler; active: boolean }[];
  hooks: { name: string; handler: Handler; active: boolean }[];
  disposables: Disposable[];
};

export type InvocationContext = {
  generation: number;
  invocationId: string;
  deadlineUnixMs: number;
  signal: AbortSignal;
  log(level: string, message: string): void;
  ai: { ask(request: { question: string; context?: string; options?: string[] }): Promise<{ answer: "yes" | "no" | "uncertain"; probability: number; reason: string }> };
  ui: {
    request(request: any): Promise<any>;
    notify(request: any): Promise<void>;
    confirm(request: any): Promise<boolean>;
    input(request: any): Promise<string | undefined>;
    select(request: any): Promise<string | undefined>;
  };
};

export class RPCFailure extends Error {
  constructor(public code: string, message: string, public data?: any) { super(message); }
}

export class PluginRuntime {
  generation?: number;
  plugins: Plugin[] = [];
  active = new Map<string, { generation: number; controller: AbortController }>();
  private requestSequence = 0;
  private maxInflight = 1;
  private reversePending = new Map<string, { kind: "ui" | "ai"; generation: number; invocationId: string; resolve(value: any): void; reject(error: any): void; cleanup(): void }>();
  constructor(private emit: (message: any) => void = () => {}) {}

  async initialize(params: any) {
    if (this.generation !== undefined) throw new RPCFailure("INVALID_REQUEST", "initialize may only be called once");
    if (!Array.isArray(params?.supportedProtocolVersions) || !params.supportedProtocolVersions.includes(1))
      throw new RPCFailure("INCOMPATIBLE_VERSION", "protocol version 1 is required");
    if (!Number.isInteger(params.generation)) throw new RPCFailure("INVALID_REQUEST", "generation is required");
    if (!Number.isInteger(params.limits?.maxInflight) || params.limits.maxInflight < 1)
      throw new RPCFailure("INVALID_REQUEST", "positive maxInflight is required");
    this.generation = params.generation;
    this.maxInflight = params.limits.maxInflight;
    const ids = new Set<string>();
    const toolNames = new Set<string>();
    const commands = new Set<string>();
    for (const config of params.plugins ?? []) {
      if (!config.pluginId || ids.has(config.pluginId) || config.pluginId === DEFAULT_PERMISSION_PLUGIN_ID)
        throw new RPCFailure("INVALID_RESULT", `duplicate or reserved plugin ID: ${config.pluginId}`);
      const uri = new URL(config.entryUri);
      uri.searchParams.set("agoGeneration", String(this.generation));
      const module = await import(uri.href);
      const contract = module.default ?? module;
      if (contract.id !== config.pluginId || contract.apiVersion !== 1 || typeof contract.activate !== "function")
        throw new RPCFailure("INVALID_RESULT", `invalid plugin module: ${config.pluginId}`);
      ids.add(contract.id);
      const plugin: Plugin = { id: contract.id, tools: [], commands: [], hooks: [], disposables: [] };
      this.plugins.push(plugin);
      const disposable = (remove: () => void): Disposable => {
        let live = true;
        const d = { dispose: () => { if (live) { live = false; remove(); } } };
        plugin.disposables.push(d);
        return d;
      };
      const api = {
        config: config.config,
        workspaceUri: params.workspaceUri,
        on: (name: string, handler: Handler) => {
          if (!name || typeof handler !== "function") throw new RPCFailure("INVALID_RESULT", "invalid hook registration");
          const item = { name, handler, active: true }; plugin.hooks.push(item);
          return disposable(() => { item.active = false; });
        },
        registerTool: (registration: any, handler?: Handler) => {
          const value = typeof handler === "function" ? { ...registration, execute: handler } : registration;
          if (!value?.name || typeof value.execute !== "function" || toolNames.has(value.name))
            throw new RPCFailure("INVALID_RESULT", `duplicate or invalid tool: ${value?.name}`);
          toolNames.add(value.name);
          const item = { name: value.name, description: value.description ?? "", inputSchema: value.inputSchema ?? {}, handler: value.execute, active: true };
          plugin.tools.push(item); return disposable(() => { item.active = false; });
        },
        registerCommand: (registration: any, handler?: Handler) => {
          const value = typeof handler === "function" ? { ...registration, execute: handler } : registration;
          const canonical = `${plugin.id}:${value?.id}`;
          if (!value?.id || typeof value.execute !== "function" || commands.has(canonical))
            throw new RPCFailure("INVALID_RESULT", `duplicate or invalid command: ${canonical}`);
          commands.add(canonical);
          const item = { id: value.id, title: value.title ?? value.id, category: value.category, description: value.description, handler: value.execute, active: true };
          plugin.commands.push(item); return disposable(() => { item.active = false; });
        },
      };
      const activation = await contract.activate(api);
      if (activation && typeof activation.dispose === "function") plugin.disposables.push(activation);
    }
    const builtIn: Plugin = { id: defaultPermissionPlugin.id, tools: [], commands: [], hooks: [], disposables: [] };
    this.plugins.push(builtIn);
    const activation = await defaultPermissionPlugin.activate({
      on: (name, handler) => {
        const item = { name, handler, active: true };
        builtIn.hooks.push(item);
        let live = true;
        const registration = { dispose: () => { if (live) { live = false; item.active = false; } } };
        builtIn.disposables.push(registration);
        return registration;
      },
    });
    if (activation && !builtIn.disposables.includes(activation)) builtIn.disposables.push(activation);
    return { protocolVersion: 1, generation: this.generation, plugins: this.plugins.map(p => ({
      pluginId: p.id,
      ...(p.tools.length ? { tools: p.tools.map(({ name, description, inputSchema }) => ({ name, description, inputSchema })) } : {}),
      ...(p.commands.length ? { commands: p.commands.map(({ id, title, category, description }) => ({ id, title, ...(category ? { category } : {}), ...(description ? { description } : {}) })) } : {}),
      ...(p.hooks.length ? { hooks: p.hooks.map(h => h.name) } : {}),
    })) };
  }

  resolveResponse(message: any) {
    const pending = this.reversePending.get(message.id);
    if (!pending) return false;
    this.reversePending.delete(message.id); pending.cleanup();
    if (pending.generation !== this.generation) pending.reject(new RPCFailure("STALE_GENERATION", "stale reverse response"));
    else if (message.ok) pending.resolve(message.result);
    else pending.reject(new RPCFailure(message.error?.code ?? "PLUGIN_ERROR", message.error?.message ?? "reverse request failed"));
    return true;
  }

  private context(pluginId: string, params: any, controller: AbortController): InvocationContext {
    const request = (body: any) => new Promise<any>((resolve, reject) => {
      if (this.reversePending.size >= this.maxInflight) throw new RPCFailure("OVERLOADED", "too many pending reverse requests");
      const id = `ui-${this.generation}-${++this.requestSequence}`;
      this.reversePending.set(id, { kind: "ui", generation: this.generation!, invocationId: params.invocationId, resolve, reject, cleanup: () => {} });
      this.emit({ type: "request", id, method: "ui.request", params: { generation: this.generation, pluginId, invocationId: params.invocationId, deadlineUnixMs: params.deadlineUnixMs, request: body } });
    }).then((result: any) => {
      if (result?.status === "ok") return result.value;
      const codes: any = { cancelled: "CANCELLED", timeout: "TIMEOUT", unavailable: "UNAVAILABLE" };
      throw new RPCFailure(codes[result?.status] ?? "INVALID_RESULT", `UI request ${result?.status ?? "invalid"}`);
    });
    const ask = (body: any) => new Promise<any>((resolve, reject) => {
      if (!body || typeof body.question !== "string" || !body.question.length || body.question.length > 4096 || (body.context !== undefined && (typeof body.context !== "string" || body.context.length > 16384)) || !Array.isArray(body.options ?? []) || (body.options ?? []).length > 16 || (body.options ?? []).some((v: any) => typeof v !== "string" || !v.length || v.length > 1024))
        throw new RPCFailure("INVALID_REQUEST", "invalid ai.ask request");
      if (this.reversePending.size >= this.maxInflight) throw new RPCFailure("OVERLOADED", "too many pending reverse requests");
      if (controller.signal.aborted) throw new RPCFailure("CANCELLED", "invocation cancelled");
      const id = `ai-${this.generation}-${++this.requestSequence}`; const timeout = Math.max(0, params.deadlineUnixMs - Date.now());
      const fail = (error: RPCFailure) => { const p = this.reversePending.get(id); if (p) { this.reversePending.delete(id); p.cleanup(); reject(error); } };
      const onAbort = () => fail(new RPCFailure("CANCELLED", "invocation cancelled"));
      const timer = setTimeout(() => fail(new RPCFailure("TIMEOUT", "ai.ask deadline exceeded")), timeout);
      const cleanup = () => { clearTimeout(timer); controller.signal.removeEventListener("abort", onAbort); };
      controller.signal.addEventListener("abort", onAbort, { once: true });
      this.reversePending.set(id, { kind: "ai", generation: this.generation!, invocationId: params.invocationId, resolve, reject, cleanup });
      this.emit({ type: "request", id, method: "ai.ask", params: { question: body.question, ...(body.context === undefined ? {} : { context: body.context }), ...(body.options === undefined ? {} : { options: body.options }), generation: this.generation, pluginId, invocationId: params.invocationId, threadId: params.threadId, turnId: params.turnId, deadlineUnixMs: params.deadlineUnixMs } });
    }).then(result => {
      if (!result || !["yes", "no", "uncertain"].includes(result.answer) || typeof result.probability !== "number" || !Number.isFinite(result.probability) || result.probability < 0 || result.probability > 1 || typeof result.reason !== "string" || !result.reason.length || result.reason.length > 4096 || Object.keys(result).some(k => !["answer", "probability", "reason"].includes(k))) throw new RPCFailure("INVALID_RESULT", "invalid ai.ask result");
      return result;
    });
    return { generation: this.generation!, invocationId: params.invocationId, deadlineUnixMs: params.deadlineUnixMs, signal: controller.signal,
      log: (level, message) => this.emit({ type: "notification", method: "log", params: { generation: this.generation, pluginId, invocationId: params.invocationId, level, message } }),
      ai: { ask },
      ui: { request, notify: async r => { await request({ ...r, kind: "notify" }); }, confirm: async r => Boolean(await request({ ...r, kind: "confirm" })), input: r => request({ ...r, kind: "input" }), select: r => request({ ...r, kind: "select" }) } };
  }

  async invoke(method: string, params: any) {
    if (this.generation === undefined) throw new RPCFailure("INVALID_REQUEST", "initialize must be first");
    if (params?.generation !== this.generation) throw new RPCFailure("STALE_GENERATION", "stale plugin generation");
    if (!params.invocationId || !params.threadId || !params.turnId || this.active.has(params.invocationId)) throw new RPCFailure("INVALID_REQUEST", "invalid invocation correlation");
    if (this.active.size >= this.maxInflight) throw new RPCFailure("OVERLOADED", "too many active plugin invocations");
    const controller = new AbortController(); this.active.set(params.invocationId, { generation: this.generation, controller });
    try {
      if (method === "hook.invoke") {
        const hook = params.payload?.hook ?? params.payload?.name;
        if (!hook) throw new RPCFailure("INVALID_REQUEST", "hook name is required");
        const results = [];
        for (const plugin of this.plugins) for (const item of plugin.hooks) {
          if (!item.active || item.name !== hook) continue;
          try {
            results.push(await item.handler(params.payload?.payload ?? params.payload, this.context(plugin.id, params, controller)));
          } catch (error: any) {
            if (hook === "tool.call" || (error instanceof RPCFailure && ["INVALID_REQUEST", "INVALID_RESULT", "CANCELLED", "TIMEOUT", "STALE_GENERATION", "UNAVAILABLE", "OVERLOADED"].includes(error.code))) throw error;
            this.emit({ type: "notification", method: "log", params: { generation: this.generation, pluginId: plugin.id, invocationId: params.invocationId, level: "error", message: `${hook} handler failed: ${error?.message ?? String(error)}` } });
            results.push(undefined);
          }
        }
        return results;
      }
      if (method === "tool.execute") {
        const name = params.payload?.name;
        for (const plugin of this.plugins) { const item = plugin.tools.find(t => t.active && t.name === name); if (item) return await item.handler(params.payload?.input ?? params.payload, this.context(plugin.id, params, controller)); }
        throw new RPCFailure("NOT_FOUND", `tool not found: ${name}`);
      }
      if (method === "command.execute") {
        const canonical = params.payload?.commandId ?? params.payload?.id;
        for (const plugin of this.plugins) for (const item of plugin.commands) if (item.active && (`${plugin.id}:${item.id}` === canonical || item.id === canonical))
          return await item.handler(params.payload?.input ?? {}, this.context(plugin.id, params, controller));
        throw new RPCFailure("NOT_FOUND", `command not found: ${canonical}`);
      }
      throw new RPCFailure("INVALID_REQUEST", `unknown method: ${method}`);
    } finally {
      controller.abort("invocation completed");
      for (const [id, pending] of this.reversePending) if (pending.invocationId === params.invocationId) {
        this.reversePending.delete(id); pending.cleanup(); pending.reject(new RPCFailure("CANCELLED", "invocation completed"));
      }
      this.active.delete(params.invocationId);
    }
  }

  cancel(params: any) {
    if (params?.generation !== this.generation) throw new RPCFailure("STALE_GENERATION", "stale plugin generation");
    this.active.get(params.invocationId)?.controller.abort(params.reason);
  }
  async dispose(params: any) {
    if (params?.generation !== this.generation) throw new RPCFailure("STALE_GENERATION", "stale plugin generation");
    for (const value of this.active.values()) value.controller.abort(params.reason);
    for (const [id, pending] of this.reversePending) { this.reversePending.delete(id); pending.cleanup(); pending.reject(new RPCFailure("CANCELLED", "plugin generation retired")); }
    for (const plugin of [...this.plugins].reverse()) for (const item of [...plugin.disposables].reverse()) await item.dispose();
    return null;
  }
}
