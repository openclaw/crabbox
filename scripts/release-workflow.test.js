import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");

test("manual release checks out requested tag before setup-go reads go.mod", () => {
  const workflow = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "release.yml"), "utf8");
  const stashConfig = workflow.indexOf("- name: Stash current GoReleaser config");
  const checkoutTag = workflow.indexOf("- name: Checkout release tag");
  const preservePreHelperConfig = workflow.indexOf("- name: Preserve pre-helper release configuration");
  const setupGo = workflow.indexOf("- name: Setup Go");

  assert.notEqual(stashConfig, -1);
  assert.notEqual(checkoutTag, -1);
  assert.notEqual(preservePreHelperConfig, -1);
  assert.notEqual(setupGo, -1);
  assert.ok(stashConfig < checkoutTag, "GoReleaser config should be saved before checking out historical tags");
  assert.ok(checkoutTag < preservePreHelperConfig, "pre-helper detection should inspect the requested release tag");
  assert.ok(
    preservePreHelperConfig < setupGo,
    "pre-helper release configuration should be selected before setup-go",
  );
  assert.match(
    workflow.slice(preservePreHelperConfig, setupGo),
    /if \[ ! -d cmd\/crabbox-apple-vz-helper \]; then\s+cp \.goreleaser\.yaml \/tmp\/\.goreleaser\.yaml/s,
  );
  assert.ok(checkoutTag < setupGo, "setup-go should read go.mod from the requested release tag");
  assert.match(workflow.slice(setupGo), /go-version-file:\s+go\.mod/);
});
