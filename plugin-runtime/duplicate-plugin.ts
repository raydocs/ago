export default {
  id: "duplicate.plugin", apiVersion: 1,
  activate(api: any) {
    api.registerTool({ name: "same", execute: () => null });
    api.registerTool({ name: "same", execute: () => null });
  }
};
