import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github", "workflows", "coordinator-admin-token-rotate.yml"),
  "utf8",
);

test("coordinator admin-token rotation is protected and manual", () => {
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.doesNotMatch(workflow, /^  (?:push|pull_request|schedule):/m);
  assert.match(workflow, /environment: coordinator/);
  assert.match(
    workflow,
    /expected_workflow_ref="\$GITHUB_REPOSITORY\/\.github\/workflows\/coordinator-admin-token-rotate\.yml@\$expected_ref"/,
  );
  assert.match(workflow, /\[\[ "\$GITHUB_REF" == "\$expected_ref" \]\]/);
  assert.match(workflow, /\[\[ "\$REF_PROTECTED" == true \]\]/);
  assert.match(workflow, /\[\[ "\$WORKFLOW_SHA" == "\$RUN_SHA" \]\]/);
  assert.match(workflow, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(workflow, /persist-credentials: false/);
  assert.match(workflow, /cancel-in-progress: false/);
});

test("rotation keeps both credentials environment-scoped and off argv", () => {
  assert.equal((workflow.match(/secrets\.CLOUDFLARE_API_TOKEN/g) ?? []).length, 1);
  assert.equal((workflow.match(/secrets\.CRABBOX_ADMIN_TOKEN_NEXT/g) ?? []).length, 1);
  assert.match(
    workflow,
    /printf '%s' "\$CRABBOX_ADMIN_TOKEN_NEXT" \| npx wrangler secret put CRABBOX_ADMIN_TOKEN/,
  );
  assert.doesNotMatch(workflow, /echo "\$CRABBOX_ADMIN_TOKEN_NEXT"/);
  assert.doesNotMatch(workflow, /--token|--secret/);
});
