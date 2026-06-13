import { defineConfig } from "vitest/config";

export default defineConfig({
  resolve: {
    alias: {
      "cloudflare:workers": new URL("./test/cloudflare-workers-runtime.ts", import.meta.url)
        .pathname,
    },
  },
  test: {
    environment: "node",
    globals: false,
  },
});
