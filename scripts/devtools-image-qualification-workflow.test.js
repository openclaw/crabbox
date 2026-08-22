import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github", "workflows", "devtools-image-qualify.yml"),
  "utf8",
);
const qualifyJob = workflow.slice(workflow.indexOf("  qualify:"), workflow.indexOf("  publish:"));
const publishJob = workflow.slice(workflow.indexOf("  publish:"));

test("AWS qualification is manual, protected, and isolated from publication", () => {
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.doesNotMatch(workflow, /^  (?:push|pull_request|schedule):/m);
  assert.match(workflow, /environment: image-qualifier/);
  assert.match(
    workflow,
    /expected_workflow_ref="\$GITHUB_REPOSITORY\/\.github\/workflows\/devtools-image-qualify\.yml@\$expected_ref"/,
  );
  assert.match(workflow, /\[\[ "\$GITHUB_REF" == "\$expected_ref" \]\]/);
  assert.match(workflow, /\[\[ "\$REF_PROTECTED" == true \]\]/);
  assert.match(workflow, /\[\[ "\$WORKFLOW_SHA" == "\$RUN_SHA" \]\]/);
  assert.match(workflow, /\[\[ "\$SOURCE_COMMIT" == "\$RUN_SHA" \]\]/);
  assert.match(workflow, /OS_SELECTOR: \$\{\{ inputs\.os_selector \}\}/);
  assert.match(workflow, /\[\[ "\$OS_SELECTOR" =~ \^ubuntu:/);
  assert.match(workflow, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(workflow, /persist-credentials: false/);
  assert.doesNotMatch(workflow, /--promote|fast-snapshot|fsr-az|workflow_call|repository_dispatch/);
});

test("qualification preserves exact Q1 bytes, signs one public receipt, and exposes no raw proof", () => {
  assert.match(workflow, /scripts\/consume-aws-image-candidate\.sh/);
  assert.match(workflow, /scripts\/qualify-aws-image-candidate\.sh/);
  assert.match(workflow, /--os-selector "\$OS_SELECTOR"/);
  assert.match(workflow, /scripts\/publish-aws-image-qualification\.sh/);
  assert.match(
    workflow,
    /repository="ghcr\.io\/\$\{GITHUB_REPOSITORY,,\}-aws-image-qualifications"/,
  );
  assert.match(workflow, /^permissions: \{\}$/m);
  assert.match(qualifyJob, /^      contents: read$/m);
  assert.match(qualifyJob, /^      packages: read$/m);
  assert.doesNotMatch(qualifyJob, /packages: write|id-token: write/);
  assert.match(publishJob, /^      contents: read$/m);
  assert.match(publishJob, /^      packages: write$/m);
  assert.match(publishJob, /^      id-token: write$/m);
  assert.doesNotMatch(
    publishJob,
    /CRABBOX_COORDINATOR|CRABBOX_OWNER|CRABBOX_ORG|CRABBOX_ACCESS_CLIENT|secrets\./,
  );
  assert.match(
    qualifyJob,
    /actions\/upload-artifact@ea165f8d65b6e75b540449e92b4886f43607fa02/,
  );
  assert.match(
    publishJob,
    /actions\/download-artifact@d3f86a106a0bac45b974a628896c90dbdf5c8093/,
  );
  assert.match(
    qualifyJob,
    /cp "\$proof_dir\/input\/qualification-input\.json" "\$proof_dir\/handoff\/qualification-input\.json"/,
  );
  assert.match(
    qualifyJob,
    /path: \$\{\{ runner\.temp \}\}\/devtools-image-qualification\/handoff$/m,
  );
  assert.match(
    publishJob,
    /cp "\$proof_dir\/handoff\/qualification-input\.json" "\$proof_dir\/public\/qualification-input\.json"/,
  );
  assert.match(
    publishJob,
    /--qualification-input "\$proof_dir\/public\/qualification-input\.json"/,
  );
  assert.match(publishJob, /path: \$\{\{ runner\.temp \}\}\/devtools-image-qualification\/public/);
  assert.doesNotMatch(qualifyJob, /path:.*\/work|path:.*\/private|path:.*\/input/);
  assert.ok(
    qualifyJob.indexOf("scripts/qualify-aws-image-candidate.sh") <
      qualifyJob.indexOf("actions/upload-artifact@"),
  );
});

test("runbook inventory matches the two-layer OCI artifact and three-file Actions proof", () => {
  const runbook = fs.readFileSync(
    path.join(repoRoot, "docs", "features", "image-bake-runbook.md"),
    "utf8",
  );
  assert.match(
    runbook,
    /Actions proof artifact contains exactly `qualification-input\.json`,\n`qualification\.json`, and `publication\.json`/,
  );
  assert.match(
    runbook,
    /signed OCI artifact contains\s+exactly `qualification-input\.json` and `qualification\.json`/,
  );
});

test("CODEOWNERS protects every qualification trust-chain input", () => {
  const codeowners = fs.readFileSync(path.join(repoRoot, ".github", "CODEOWNERS"), "utf8");
  for (const protectedPath of [
    "/recipes/aws/",
    "/recipes/linux/",
    "/scripts/consume-aws-image-candidate.mjs",
    "/scripts/consume-aws-image-candidate.sh",
    "/scripts/fixtures/aws-image-qualification-overlay.txt",
    "/scripts/aws-image-qualification.mjs",
    "/scripts/qualify-aws-image-candidate.sh",
    "/scripts/publish-aws-image-qualification.sh",
    "/scripts/validate-json-schema/",
  ]) {
    assert.match(
      codeowners,
      new RegExp(`^${protectedPath.replaceAll(".", "\\.")} @openclaw/openclaw-secops$`, "m"),
    );
  }
});

test("script CI runs both localhost ORAS and Cosign qualification integrations", () => {
  const ci = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "ci.yml"), "utf8");
  assert.match(ci, /consume-aws-image-candidate\.integration\.test\.js/);
  assert.match(ci, /publish-aws-image-qualification\.integration\.test\.js/);
  assert.match(
    ci,
    /CRABBOX_ORAS: \$\{\{ runner\.temp \}\}\/image-tools\/oras[\s\S]*CRABBOX_COSIGN: \$\{\{ runner\.temp \}\}\/image-tools\/cosign/,
  );
});
