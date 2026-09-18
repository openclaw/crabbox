#!/usr/bin/env node
import { spawnSync } from "node:child_process";
import { fileURLToPath } from "node:url";

const args = process.argv.slice(2);
if (
  args[0] !== "--image" ||
  !args[1] ||
  args[1].startsWith("-") ||
  args.length > 3 ||
  (args[2] !== undefined && args[2] !== "--mount-source")
) {
  console.error("Usage: preflight.mjs --image LOCAL_RUNNER_IMAGE [--mount-source]");
  process.exit(2);
}

// A missing image is an error in this explicit gate. The integration test is
// skipped only during the ordinary credential-free unit suite.
const workerRoot = fileURLToPath(new URL("../../worker/", import.meta.url));
const result = spawnSync(
  process.execPath,
  ["node_modules/vitest/vitest.mjs", "run", "test/koyeb-runner-integration.test.ts"],
  {
    cwd: workerRoot,
    stdio: "inherit",
    env: {
      ...process.env,
      CRABBOX_TEST_RUNNER_IMAGE: args[1],
      CRABBOX_TEST_RUNNER_MOUNT_SOURCE: args[2] === "--mount-source" ? "1" : "0",
    },
  },
);
if (result.error) console.error(result.error.message);
process.exit(result.status ?? 1);
