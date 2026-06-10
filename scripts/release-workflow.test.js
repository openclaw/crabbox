import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");

test("manual release checks out requested tag before setup-go reads go.mod", () => {
  const workflow = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "release.yml"), "utf8");
  const stashConfig = workflow.indexOf("- name: Stash GoReleaser config");
  const checkoutTag = workflow.indexOf("- name: Checkout release tag");
  const setupGo = workflow.indexOf("- name: Setup Go");

  assert.notEqual(stashConfig, -1);
  assert.notEqual(checkoutTag, -1);
  assert.notEqual(setupGo, -1);
  assert.ok(stashConfig < checkoutTag, "GoReleaser config should be saved before checking out historical tags");
  assert.ok(checkoutTag < setupGo, "setup-go should read go.mod from the requested release tag");
  assert.match(workflow.slice(setupGo), /go-version-file:\s+go\.mod/);
});
