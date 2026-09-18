import { fileURLToPath } from "node:url";

import { configDefaults, defineConfig } from "vitest/config";

export default defineConfig({
  resolve: {
    alias: {
      "cloudflare:workers": fileURLToPath(
        new URL("./test/cloudflare-workers-runtime.ts", import.meta.url),
      ),
    },
  },
  test: {
    environment: "node",
    globals: false,
    // The real-store proof runs separately against its job-local PostgreSQL service.
    exclude: [...configDefaults.exclude, "test/aws-cleanup-recovery.postgres.test.ts"],
  },
});
