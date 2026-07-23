import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github", "workflows", "devtools-image-publish.yml"),
  "utf8",
);

test("developer image publication is a protected manual admin workflow", () => {
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.doesNotMatch(workflow, /^  (?:push|pull_request|schedule):/m);
  assert.match(workflow, /environment: image-publisher/);
  assert.match(
    workflow,
    /expected_workflow_ref="\$GITHUB_REPOSITORY\/\.github\/workflows\/devtools-image-publish\.yml@\$expected_ref"/,
  );
  assert.match(workflow, /\[\[ "\$GITHUB_REF" == "\$expected_ref" \]\]/);
  assert.match(workflow, /\[\[ "\$REF_PROTECTED" == true \]\]/);
  assert.match(workflow, /\[\[ "\$WORKFLOW_SHA" == "\$RUN_SHA" \]\]/);
  assert.match(workflow, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(workflow, /persist-credentials: false/);
  assert.match(workflow, /cancel-in-progress: false/);
});

test("publication uses the existing source candidate promotion proof wrappers", () => {
  assert.match(workflow, /scripts\/mint-aws-devtools-image\.sh[\s\S]*--target linux[\s\S]*--run/);
  assert.match(workflow, /scripts\/mint-aws-devtools-image\.sh[\s\S]*--target windows[\s\S]*--windows-mode normal[\s\S]*--run/);
  assert.match(workflow, /scripts\/mint-macos-devtools-image\.sh[\s\S]*"--\$MACOS_HOST"/);
  assert.doesNotMatch(workflow, /--no-promote/);
  assert.match(workflow, /go build -trimpath -o bin\/crabbox \.\/cmd\/crabbox/);
});

test("publication keeps credentials environment-scoped and retains proof", () => {
  assert.match(workflow, /CRABBOX_COORDINATOR: \$\{\{ vars\.CRABBOX_COORDINATOR \}\}/);
  assert.equal(
    (workflow.match(/secrets\.CRABBOX_COORDINATOR_ADMIN_TOKEN/g) ?? []).length,
    2,
  );
  assert.doesNotMatch(workflow, /AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY/);
  assert.match(workflow, /name: Upload publication proof[\s\S]*if: always\(\)/);
  assert.match(workflow, /if-no-files-found: error/);
  assert.match(workflow, /retention-days: 30/);
});
