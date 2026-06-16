import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..");
const workflow = path.join(repoRoot, ".github", "workflows", "hydrate.yml");

test("hydrate workflow does not mutate shared runner tool cache", async () => {
  const text = await readFile(workflow, "utf8");
  assert.match(text, /uses:\s+actions\/setup-go@[0-9a-f]{40}/);
  assert.doesNotMatch(text, /rm\s+-rf\s+["']?\$RUNNER_TOOL_CACHE\/go/);
  assert.doesNotMatch(text, /tar\s+-C\s+["']?\$RUNNER_TOOL_CACHE/);
});
