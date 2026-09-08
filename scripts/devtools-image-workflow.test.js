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
  assert.match(
    workflow,
    /scripts\/mint-aws-devtools-image\.sh[\s\S]*--target windows[\s\S]*--windows-mode normal[\s\S]*--run/,
  );
  assert.match(workflow, /scripts\/mint-macos-devtools-image\.sh[\s\S]*"--\$MACOS_HOST"/);
  assert.doesNotMatch(workflow, /--no-promote/);
  assert.match(workflow, /go build -trimpath -o bin\/crabbox \.\/cmd\/crabbox/);
});

test("publication keeps credentials environment-scoped and retains proof", () => {
  assert.match(workflow, /CRABBOX_COORDINATOR: \$\{\{ vars\.CRABBOX_COORDINATOR \}\}/);
  assert.equal((workflow.match(/secrets\.CRABBOX_COORDINATOR_ADMIN_TOKEN/g) ?? []).length, 2);
  assert.doesNotMatch(workflow, /AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY/);
  assert.match(
    workflow,
    /name: Upload publication diagnostics\s+if: always\(\) && !inputs\.measured/,
  );
  assert.match(workflow, /if-no-files-found: error/);
  assert.match(workflow, /retention-days: 30/);
});

test("measured Linux publication is explicit and declares its threshold and extra launches", () => {
  assert.match(workflow, /measured:[\s\S]*type: boolean[\s\S]*default: false/);
  assert.match(workflow, /max_p95_runner_total_ms:/);
  assert.match(workflow, /MEASURED: \$\{\{ inputs\.measured \}\}/);
  assert.match(workflow, /MAX_P95_RUNNER_TOTAL_MS: \$\{\{ inputs\.max_p95_runner_total_ms \}\}/);
  assert.match(workflow, /\[\[ "\$TARGET" == linux \]\]/);
  assert.match(workflow, /\[\[ "\$MAX_P95_RUNNER_TOTAL_MS" =~ \^\[1-9\]\[0-9\]\*\$ \]\]/);
  assert.match(workflow, /12 planned leases/);
  assert.match(workflow, /no hard launch-attempt or dollar cap/);
  assert.match(
    workflow,
    /command\+=\(--measured --max-p95-runner-total-ms "\$MAX_P95_RUNNER_TOTAL_MS"\)/,
  );
  assert.match(
    workflow,
    /name: Upload allowlisted measurement manifest[\s\S]*if: always\(\) && inputs\.measured/,
  );
  assert.match(workflow, /path: \$\{\{ runner\.temp \}\}\/devtools-image-proof\/manifest\.json/);
  assert.match(
    workflow,
    /export CRABBOX_IMAGE_PUBLIC_OUTCOME="\$proof_dir\/manifest\.json"/,
  );
  assert.match(workflow, /public_outcome="\$RUNNER_TEMP\/devtools-image-proof\/manifest\.json"/);
  assert.match(workflow, /name: Initialize measured publication outcome/);
  assert.match(workflow, /"\$\{command\[@\]\}" >"\$private_dir\/publish\.log" 2>&1/);
  assert.match(workflow, /devtools-image-proof\.mjs validate/);
  assert.doesNotMatch(workflow, /\bcp "\$\{manifests/);
  assert.doesNotMatch(workflow, /run-name:.*\$\{\{ inputs\.(?:region|linux_type)/);
  assert.doesNotMatch(workflow, /sanitized.*(?:logs|diagnostics)/i);
});
