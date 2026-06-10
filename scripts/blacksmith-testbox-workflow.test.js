import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");

test("blacksmith testbox workflow pins external actions to commit shas", () => {
  const workflow = fs.readFileSync(
    path.join(repoRoot, ".github", "workflows", "blacksmith-testbox.yml"),
    "utf8",
  );
  const actionRefs = [...workflow.matchAll(/^\s*uses:\s*([^@\s]+)@([^\s#]+)\s*$/gm)];

  assert.notEqual(actionRefs.length, 0, "expected action references in workflow");
  for (const [, action, ref] of actionRefs) {
    assert.match(ref, /^[0-9a-f]{40}$/, `${action} is not pinned to a full commit SHA`);
  }
});
