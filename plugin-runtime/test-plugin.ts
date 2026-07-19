import { writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";

export default {
  id: "test.plugin", apiVersion: 1,
  activate(api: any) {
    console.log("plugin raw log");
    if (api.config?.malformedPolicy) api.on("tool.call", () => "not-a-policy-decision");
    api.on("cancel", (_: any, ctx: any) => new Promise(resolve => ctx.signal.addEventListener("abort", () => resolve("aborted"), { once: true })));
    api.on("ui", async (_: any, ctx: any) => ctx.ui.confirm({ title: "Allow?" }));
    api.on("ask", async (input: any, ctx: any) => ctx.ai.ask({ question: input.question, context: input.context, options: input.options }));
    api.on("ask-unawaited", (_input: any, ctx: any) => { void ctx.ai.ask({ question: "background?" }).catch(() => {}); return "done"; });
    api.registerTool({ name: "echo", description: "Echo", inputSchema: {}, execute: (input: any) => input });
    if (api.config?.codeChange) api.registerTool({
      name: "write_test_file", description: "Write the deterministic Ago conformance artifact",
      inputSchema: { type: "object", properties: { text: { type: "string" } }, required: ["text"], additionalProperties: false },
      execute: async (input: any) => {
        const workspace = api.workspaceUri.endsWith("/") ? api.workspaceUri : `${api.workspaceUri}/`;
        await writeFile(fileURLToPath(new URL("ago-proof.txt", workspace)), input.text, "utf8");
        return "wrote ago-proof.txt";
      },
    });
    api.registerCommand({ id: "run", title: "Run", execute: () => "ran" });
    api.registerCommand({ id: "confirm", title: "Confirm", execute: async (_input: any, ctx: any) => ctx.ui.confirm({ title: "Allow?" }) });
  }
};
