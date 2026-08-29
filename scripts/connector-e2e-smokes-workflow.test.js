import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github", "workflows", "connector-e2e-smokes.yml"),
  "utf8",
);

test("connector lifecycle gate runs on pull requests, main pushes, and manual dispatch only", () => {
  const trigger = workflow.slice(workflow.indexOf("\non:"), workflow.indexOf("permissions:"));
  assert.match(trigger, /pull_request:/);
  assert.match(trigger, /push:\s*\n\s+branches:\s*\n\s+- main/);
  assert.match(trigger, /workflow_dispatch:/);
  assert.doesNotMatch(
    workflow,
    /schedule:/,
    "the hermetic gate must not run on a schedule; tier 2 needs a maintainer credential policy first",
  );
});

test("connector lifecycle gate stays hermetic: no secret references anywhere", () => {
  assert.ok(
    !workflow.includes("secrets."),
    "workflow must not reference secrets; tier 1 of https://github.com/openclaw/crabbox/issues/944 is zero-credential",
  );
});

test("matrix rows do not fail fast and are time-bounded", () => {
  assert.match(workflow, /fail-fast:\s*false/);
  assert.match(workflow, /timeout-minutes:\s*15/);
});

test("SSH localhost asserts amd64 only on its hosted x64 lifecycle row", () => {
  const rows = [
    ...workflow.matchAll(/^ {10}- name: ssh-localhost\n((?: {12}[^\n]*\n)*)/gm),
  ];
  assert.equal(rows.length, 1, "keep one existing SSH localhost row");
  const row = rows[0][1];
  assert.match(row, /^ {12}runner: ubuntu-latest$/m);
  assert.match(row, /^ {12}build-cli: true$/m);
  assert.match(
    row,
    /^ {12}smoke: CRABBOX_ARCH=amd64 CRABBOX_BIN="\$PWD\/bin\/crabbox" scripts\/live-ssh-localhost-smoke\.sh$/m,
  );
  assert.equal((workflow.match(/CRABBOX_ARCH/g) ?? []).length, 1);
  assert.match(workflow, /runs-on: \$\{\{ matrix\.runner \}\}/);
  assert.match(workflow, /run: \$\{\{ matrix\.smoke \}\}/);
  assert.match(workflow, /permissions:\n {2}contents: read\n\n/);
  assert.doesNotMatch(
    workflow,
    /pull_request_target:|self-hosted|^\s*(?:environment|services|container|continue-on-error):/m,
  );
});

test("pull request runs cancel superseded attempts", () => {
  assert.match(workflow, /concurrency:\s*\n\s+group:/);
  assert.match(workflow, /cancel-in-progress:.*pull_request/);
});
