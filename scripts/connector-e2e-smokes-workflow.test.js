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

test("pull request runs cancel superseded attempts", () => {
  assert.match(workflow, /concurrency:\s*\n\s+group:/);
  assert.match(workflow, /cancel-in-progress:.*pull_request/);
});
