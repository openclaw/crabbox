import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github", "workflows", "devtools-image-publish.yml"),
  "utf8",
);

test("AWS candidate publication is protected, manual, and candidate-only", () => {
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.doesNotMatch(workflow, /^  (?:push|pull_request|schedule):/m);
  assert.match(workflow, /environment: image-publisher/);
  assert.match(workflow, /cancel-in-progress: false/);
  assert.match(
    workflow,
    /expected_workflow_ref="\$GITHUB_REPOSITORY\/\.github\/workflows\/devtools-image-publish\.yml@\$expected_ref"/,
  );
  assert.match(workflow, /\[\[ "\$GITHUB_REF" == "\$expected_ref" \]\]/);
  assert.match(workflow, /\[\[ "\$REF_PROTECTED" == true \]\]/);
  assert.match(workflow, /\[\[ "\$WORKFLOW_SHA" == "\$RUN_SHA" \]\]/);
  assert.match(workflow, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(workflow, /persist-credentials: false/);
  assert.doesNotMatch(workflow, /(?:^|\s)- macos$/m);
});

test("candidate workflow has OCI and keyless-signing permissions and tools", () => {
  assert.match(workflow, /^  contents: read$/m);
  assert.match(workflow, /^  packages: write$/m);
  assert.match(workflow, /^  id-token: write$/m);
  assert.match(workflow, /oras\.land\/oras\/cmd\/oras@v1\.3\.3/);
  assert.match(workflow, /github\.com\/sigstore\/cosign\/v2\/cmd\/cosign@v2\.6\.5/);
  assert.match(workflow, /oras" login ghcr\.io[\s\S]*--password-stdin/);
  assert.match(workflow, /scripts\/publish-aws-image-candidate\.sh/);
  assert.match(
    workflow,
    /repository="ghcr\.io\/\$\{GITHUB_REPOSITORY,,\}-aws-image-candidates"/,
  );
  assert.match(
    workflow,
    /--certificate-identity "https:\/\/github\.com\/\$GITHUB_WORKFLOW_REF"/,
  );
});

test("script CI exercises pinned ORAS and Cosign against only localhost", () => {
  const ci = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "ci.yml"), "utf8");
  assert.match(ci, /oras\.land\/oras\/cmd\/oras@v1\.3\.3/);
  assert.match(ci, /github\.com\/sigstore\/cosign\/v2\/cmd\/cosign@v2\.6\.5/);
  assert.match(
    ci,
    /CRABBOX_ORAS: \$\{\{ runner\.temp \}\}\/image-tools\/oras[\s\S]*CRABBOX_COSIGN: \$\{\{ runner\.temp \}\}\/image-tools\/cosign/,
  );
  assert.match(
    ci,
    /node --test scripts\/consume-aws-image-candidate\.integration\.test\.js/,
  );
});

test("workflow explicitly disables promotion, FSR, and promoted warmup", () => {
  assert.match(
    workflow,
    /scripts\/mint-aws-devtools-image\.sh[\s\S]*--candidate-output "\$CRABBOX_IMAGE_CANDIDATE_OUTPUT"[\s\S]*--run[\s\S]*--no-promote/,
  );
  assert.doesNotMatch(workflow, /--promote(?:\s|$)/);
  assert.doesNotMatch(workflow, /fast-snapshot|fsr-az|image promote|promoted image/);
  assert.match(workflow, /--base-image "\$BASE_IMAGE"/);
  assert.match(workflow, /command\+\=\(--previous-default "\$PREVIOUS_DEFAULT"\)/);
  assert.match(workflow, /node scripts\/aws-image-candidate\.mjs verify/);
});

test("workflow keeps cloud credentials environment-scoped and retains proof", () => {
  assert.match(workflow, /CRABBOX_COORDINATOR: \$\{\{ vars\.CRABBOX_COORDINATOR \}\}/);
  assert.equal(
    (workflow.match(/secrets\.CRABBOX_COORDINATOR_ADMIN_TOKEN/g) ?? []).length,
    2,
  );
  assert.doesNotMatch(workflow, /AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY/);
  assert.doesNotMatch(workflow, /openclaw/i);
  assert.match(
    workflow,
    /CRABBOX_OWNER: \$\{\{ vars\.CRABBOX_IMAGE_PUBLISHER_OWNER \}\}/,
  );
  assert.match(
    workflow,
    /CRABBOX_ORG: \$\{\{ vars\.CRABBOX_IMAGE_PUBLISHER_ORG \}\}/,
  );
  assert.match(workflow, /Set CRABBOX_IMAGE_PUBLISHER_OWNER to a valid email/);
  assert.match(workflow, /Set CRABBOX_IMAGE_PUBLISHER_ORG to a valid tenant/);
  assert.doesNotMatch(workflow, /image-publisher@example\.invalid|CRABBOX_ORG: example-org/);
  assert.match(workflow, /name: Upload candidate proof[\s\S]*if: always\(\)/);
  assert.match(workflow, /if-no-files-found: error/);
  assert.match(workflow, /retention-days: 30/);
});
