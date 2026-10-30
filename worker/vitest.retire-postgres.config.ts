import { configDefaults, defineConfig } from "vitest/config";

import base from "./vitest.config";

export default defineConfig({
  ...base,
  test: {
    ...base.test,
    include: ["test/admin-create-retirement.postgres.test.ts"],
    exclude: configDefaults.exclude,
    maxWorkers: 1,
  },
});
