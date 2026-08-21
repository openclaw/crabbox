import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github", "workflows", "devtools-image-promote.yml"),
  "utf8",
);
const slice = (from, to) =>
  workflow.slice(workflow.indexOf(`  ${from}:`), to ? workflow.indexOf(`  ${to}:`) : undefined);
const guard = slice("guard", "verify");
const verify = slice("verify", "mutate");
const mutate = slice("mutate", "probe");
const probe = slice("probe", "finalize");
const finalize = slice("finalize", "sign");
const sign = slice("sign");

test("protected promotion is manual, AWS/Linux-only, and serialized by normalized scope", () => {
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.doesNotMatch(
    workflow,
    /^  (?:push|pull_request|schedule|workflow_call|repository_dispatch):/m,
  );
  assert.match(workflow, /group: devtools-image-promote-\$\{\{ inputs\.normalized_scope \}\}/);
  assert.match(workflow, /value\.scope\.provider, value\.scope\.target/);
  assert.match(workflow, /value\.scope\.architecture, value\.scope\.os/);
  assert.doesNotMatch(workflow, /ami_id:|previous_default:|catalog-only|fast-snapshot|fsr-az/);
  assert.match(workflow, /scripts\/verify-aws-image-promotion\.sh/);
  assert.match(workflow, /scripts\/aws-image-promotion-client\.mjs mutate/);
  assert.match(workflow, /--expected-current-image "\$EXPECTED_CURRENT_IMAGE"/);
  assert.match(workflow, /--operation "\$OPERATION"/);
  assert.doesNotMatch(workflow, /automatic rollback|image promote ami-/i);
});

test("artifact identity and the logical attempt key survive selective job reruns", () => {
  const uploadSteps = workflow
    .split(/^      - name: /m)
    .slice(1)
    .filter((step) => step.includes("uses: actions/upload-artifact@"));
  const expectedNames = ["evidence", "mutation", "attempt", "receipt"].map(
    (artifact) => `aws-image-promotion-${artifact}-\${{ github.run_id }}`,
  );
  assert.equal(uploadSteps.length, expectedNames.length);
  assert.deepEqual(
    uploadSteps.map((step) => step.match(/^          name: (.+)$/m)?.[1]),
    expectedNames,
  );
  for (const step of uploadSteps) {
    assert.match(step, /^          overwrite: true$/m);
    assert.doesNotMatch(step, /github\.run_attempt/);
  }
  assert.match(mutate, /promotion-\$\{GITHUB_RUN_ID\}-\$\{OPERATION\}/);
  assert.doesNotMatch(mutate, /promotion-\$\{GITHUB_RUN_ID\}-\$\{GITHUB_RUN_ATTEMPT\}/);
});

test("jobs keep credentials and permissions separated by responsibility", () => {
  assert.match(workflow, /^permissions: \{\}$/m);
  assert.match(guard, /^    permissions: \{\}$/m);
  assert.doesNotMatch(guard, /environment:|secrets\./);

  assert.match(verify, /^      contents: read$/m);
  assert.match(verify, /^      packages: read$/m);
  assert.doesNotMatch(verify, /environment:|secrets\.|CRABBOX_COORDINATOR/);

  assert.match(mutate, /environment: image-promoter/);
  assert.match(mutate, /secrets\.CRABBOX_COORDINATOR_PROMOTION_TOKEN/);
  assert.doesNotMatch(mutate, /packages:|id-token:|AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY/);
  assert.doesNotMatch(mutate, /go build|bin\/crabbox/);
  assert.match(mutate, /node scripts\/aws-image-promotion-client\.mjs mutate/);

  assert.match(probe, /environment: image-prober/);
  assert.match(probe, /secrets\.CRABBOX_IMAGE_PROBER_TOKEN/);
  assert.doesNotMatch(probe, /PROMOTION_TOKEN|ADMIN_TOKEN|packages:|id-token:/);

  assert.match(finalize, /environment: image-promoter/);
  assert.match(finalize, /secrets\.CRABBOX_COORDINATOR_PROMOTION_TOKEN/);
  assert.doesNotMatch(finalize, /packages:|id-token:|AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY/);
  assert.doesNotMatch(finalize, /bin\/crabbox|fetch\(/);
  assert.match(finalize, /aws-image-promotion-client\.mjs recover/);
  assert.match(finalize, /aws-image-promotion-client\.mjs verify/);

  assert.match(sign, /^      contents: read$/m);
  assert.match(sign, /^      packages: write$/m);
  assert.match(sign, /^      id-token: write$/m);
  assert.doesNotMatch(
    sign,
    /CRABBOX_COORDINATOR|CRABBOX_IMAGE_PROBER_TOKEN|AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY|secrets\./,
  );
});

test("probe is a fresh non-pool lease with explicit image overrides cleared", () => {
  assert.match(
    probe,
    /CRABBOX_CONFIG: \$\{\{ runner\.temp \}\}\/aws-image-promotion\/empty-config\.yaml/,
  );
  assert.match(probe, /CRABBOX_AWS_AMI: ""/);
  assert.match(probe, /: >"\$CRABBOX_CONFIG"/);
  assert.match(probe, /bin\/crabbox run/);
  assert.match(probe, /--provider aws/);
  assert.match(probe, /--target linux/);
  assert.match(probe, /--market on-demand/);
  assert.match(probe, /--no-sync/);
  assert.match(probe, /--stop-after always/);
  assert.doesNotMatch(probe, /--pool|--id |--aws-ami|--aws-snapshot|--stock-image/);
  const promotionClient = fs.readFileSync(
    path.join(repoRoot, "scripts", "aws-image-promotion-client.mjs"),
    "utf8",
  );
  assert.match(promotionClient, /crabbox-image-promotion-verification\/v1/);
  assert.match(probe, /failure_code=lease_start_failed/);
  assert.match(probe, /needs\.mutate\.outputs\.phase == 'awaiting_verification'/);
  assert.match(mutate, /promotion-\$\{GITHUB_RUN_ID\}-\$\{OPERATION\}/);
  assert.doesNotMatch(mutate, /promotion-\$\{GITHUB_RUN_ID\}-\$\{GITHUB_RUN_ATTEMPT\}/);
  assert.match(finalize, /aws-image-promotion-client\.mjs recover/);
  assert.match(finalize, /probe_not_run/);
  assert.match(finalize, /probe_command_failed/);
  assert.match(promotionClient, /probeStatus: "failed"/);
  assert.match(promotionClient, /failureCode/);
  assert.doesNotMatch(finalize, /needs\.probe\.outputs\.lease_id != ''/);
  assert.match(finalize, /authoritative-attempt\.json/);
});

test("signing consumes the persisted attempt and real integration is localhost-only", () => {
  assert.match(sign, /scripts\/publish-aws-image-promotion-receipt\.sh/);
  assert.match(sign, /authoritative-attempt\.json/);
  assert.doesNotMatch(sign, /bin\/crabbox image promote|\/v1\/image-promotions/);
  const integration = fs.readFileSync(
    path.join(repoRoot, "scripts", "publish-aws-image-promotion-receipt.integration.test.js"),
    "utf8",
  );
  assert.match(integration, /scripts", "test-oci-registry\.mjs/);
  assert.match(integration, /CRABBOX_TEST_REGISTRY/);
  assert.doesNotMatch(integration, /ghcr\.io\/v2|docker\.io|quay\.io/);
  const ci = fs.readFileSync(path.join(repoRoot, ".github", "workflows", "ci.yml"), "utf8");
  assert.match(ci, /publish-aws-image-promotion-receipt\.integration\.test\.js/);
});

test("CODEOWNERS protects the complete promotion trust chain", () => {
  const codeowners = fs.readFileSync(path.join(repoRoot, ".github", "CODEOWNERS"), "utf8");
  for (const protectedPath of [
    "/.github/workflows/devtools-image-promote.yml",
    "/internal/cli/config.go",
    "/internal/cli/coordinator.go",
    "/internal/cli/image.go",
    "/recipes/aws/",
    "/scripts/aws-image-candidate.mjs",
    "/scripts/aws-image-promotion-client.mjs",
    "/scripts/aws-image-promotion-client.test.js",
    "/scripts/aws-image-promotion-evidence.mjs",
    "/scripts/aws-image-promotion-*",
    "/scripts/aws-image-qualification.mjs",
    "/scripts/consume-aws-image-candidate.mjs",
    "/scripts/publish-aws-image-promotion-receipt.sh",
    "/scripts/verify-aws-image-promotion.sh",
    "/worker/src/coordinator-entry.ts",
    "/worker/src/coordinator-runtime.ts",
    "/worker/src/aws.ts",
    "/worker/src/fleet.ts",
    "/worker/src/types.ts",
  ]) {
    assert.match(
      codeowners,
      new RegExp(
        `^${protectedPath.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")} @openclaw/openclaw-secops$`,
        "m",
      ),
    );
  }
});
