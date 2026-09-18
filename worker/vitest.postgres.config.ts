import { configDefaults, defineConfig } from "vitest/config";

import base from "./vitest.config";

export default defineConfig({
  ...base,
  test: {
    ...base.test,
    include: ["test/aws-cleanup-recovery.postgres.test.ts"],
    exclude: configDefaults.exclude,
  },
});
