export default {
  id: "hanging.plugin",
  apiVersion: 1,
  activate(api: any) {
    api.on("tool.call", () => {
      for (;;) {}
    });
  },
};
