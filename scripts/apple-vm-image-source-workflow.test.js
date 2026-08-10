import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github/workflows/apple-vm-image-source.yml"),
  "utf8",
);

test("Apple VM image source workflow covers source changes and stable releases", () => {
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.match(workflow, /^  schedule:\n    - cron: "17 4 \* \* 1"$/m);
  assert.match(workflow, /^  pull_request:\n    paths:/m);
  assert.match(workflow, /^  push:\n    branches:\n      - main\n    tags:\n      - "v\*"\n    paths:/m);
  for (const source of [
    "internal/cli/os_image.go",
    "internal/providers/applevm/backend.go",
    "scripts/apple-vm-image-source/**",
    ".github/workflows/apple-vm-image-source.yml",
  ]) {
    assert.equal(workflow.split(`- "${source}"`).length - 1, 2, `${source} must trigger PR and main checks`);
  }
  assert.match(workflow, /^permissions:\n  contents: read$/m);
  assert.doesNotMatch(workflow, /contents: write|secrets[.]/);
  assert.match(workflow, /timeout-minutes: 5/);
  assert.match(workflow, /actions\/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0/);
  assert.match(workflow, /actions\/setup-go@924ae3a1cded613372ab5595356fb5720e22ba16/);
  assert.match(workflow, /go-version-file: go[.]mod\n\s+cache: false/);
  assert.match(workflow, /run: go run [.][/]scripts\/apple-vm-image-source/);
  assert.doesNotMatch(workflow, /setup-node|node-version/);
});
