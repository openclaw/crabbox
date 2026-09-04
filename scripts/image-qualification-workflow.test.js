import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";
import { execFileSync, spawnSync } from "node:child_process";

const root = path.resolve(import.meta.dirname, "..");
const read = (file) => fs.readFileSync(path.join(root, file), "utf8");
const workflow = read(".github/workflows/image-qualification.yml");
const reaper = read(".github/workflows/image-qualification-reaper.yml");
const control = read("scripts/image-qualification-control.mjs");
const executor = read("scripts/image-qualification-execute.sh");
const adapter = read("scripts/image-qualification-crabbox-adapter.sh");
const relaySource = read("scripts/image-qualification-relay-worker.mjs");

test("workflow isolates candidate execution from protected credentials", () => {
  assert.equal(
    fs.existsSync(path.join(root, ".github/workflows/image-qualification-candidate.yml")),
    false,
  );
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.match(workflow, /group: image-qualification\n  cancel-in-progress: false/);
  assert.match(workflow, /environment: image-qualification/);
  assert.match(workflow, /if: always\(\)/);
  const buildJob = workflow.slice(
    workflow.indexOf("  build-candidate:"),
    workflow.indexOf("  admit:"),
  );
  assert.match(buildJob, /needs: authorize/);
  assert.match(buildJob, /ref: \$\{\{ github\.workflow_sha \}\}[\s\S]*path: harness/);
  assert.match(buildJob, /ref: \$\{\{ needs\.authorize\.outputs\.candidate_sha \}\}/);
  assert.match(buildJob, /go-version-file: harness\/go\.mod/);
  assert.match(buildJob, /npm ci --prefix harness\/worker --ignore-scripts/);
  assert.match(buildJob, /image-qualification-control\.mjs prepare-build/);
  assert.match(buildJob, /GOTOOLCHAIN=local GOWORK=off CGO_ENABLED=0 GOFLAGS=-mod=readonly/);
  assert.match(buildJob, /go build -trimpath/);
  assert.match(buildJob, /\.\/node_modules\/\.bin\/wrangler deploy --dry-run/);
  assert.match(buildJob, /image-qualification-control\.mjs manifest/);
  assert.match(buildJob, /build-inputs\.json/);
  assert.match(buildJob, /cache: false/g);
  assert.match(
    buildJob,
    /AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN CLOUDFLARE_API_TOKEN QUALIFICATION_CONTROLLER_TOKEN/,
  );
  assert.doesNotMatch(buildJob, /environment:|secrets\./);
  assert.doesNotMatch(buildJob, /npm ci --prefix candidate|npm exec|go-version-file: candidate/);
  assert.doesNotMatch(workflow, /seal-candidate|image-qualification-candidate-raw/);
  const buildOrder = [
    "image-qualification-control.mjs prepare-build",
    "go build -trimpath",
    "./node_modules/.bin/wrangler deploy --dry-run",
    "image-qualification-control.mjs manifest",
    "Upload immutable candidate bundle",
  ].map((marker) => buildJob.indexOf(marker));
  assert.ok(buildOrder.every((offset) => offset >= 0));
  assert.deepEqual(buildOrder, [...buildOrder].sort((left, right) => left - right));
  assert.match(workflow, /candidate_artifact_id/);
  assert.match(workflow, /path: \$\{\{ runner\.temp \}\}\/image-qualification-candidate/);
  const admitJob = workflow.slice(
    workflow.indexOf("  admit:"),
    workflow.indexOf("  deploy-enroll:"),
  );
  assert.match(admitJob, /image-qualification-control\.mjs admit/);
  assert.doesNotMatch(
    admitJob,
    /CLOUDFLARE_API_TOKEN|AWS_ACCESS_KEY_ID|QUALIFICATION_CONTROLLER_TOKEN/,
  );
  assert.match(workflow, /deploy-enroll:[\s\S]*needs: \[authorize, build-candidate, admit\]/);
  assert.match(workflow, /arm:[\s\S]*environment: image-qualification/);
  assert.match(workflow, /image-qualification-control\.mjs arm/);
  assert.match(
    workflow,
    /execute:[\s\S]*needs: \[authorize, build-candidate, deploy-enroll, arm\]/,
  );
  const executeJob = workflow.slice(
    workflow.indexOf("  execute:"),
    workflow.indexOf("  finalize:"),
  );
  assert.doesNotMatch(
    executeJob,
    /CLOUDFLARE_API_TOKEN|AWS_ACCESS_KEY_ID|QUALIFICATION_CONTROLLER_TOKEN/,
  );
  assert.match(executeJob, /QUALIFICATION_RELAY_URL/);
  assert.match(executeJob, /QUALIFICATION_EXECUTOR_TOKEN/);
  assert.doesNotMatch(executeJob, /QUALIFICATION_ADMIN_TOKEN|QUALIFICATION_SHARED_TOKEN/);
  assert.doesNotMatch(executeJob, /QUALIFICATION_EXPECTED_CANDIDATE_VERSION/);
  const protectedJobs = workflow.slice(workflow.indexOf("  deploy-enroll:"));
  assert.doesNotMatch(protectedJobs, /npm ci|go build|wrangler deploy/);
  assert.match(protectedJobs, /candidate bytes as inert data/);
});

test("all workflow actions use immutable repository-standard pins", () => {
  for (const source of [workflow, reaper]) {
    for (const line of source.matchAll(/uses:\s+([^@\s]+)@([^\s]+)/g)) {
      assert.match(line[2], /^[0-9a-f]{40}$/);
    }
  }
});

test("reaper is protected, serialized, and artifact-independent", () => {
  assert.match(reaper, /workflow_run:/);
  assert.match(reaper, /schedule:/);
  assert.match(reaper, /environment: image-qualification/);
  assert.match(reaper, /group: image-qualification/);
  assert.doesNotMatch(reaper, /download-artifact|needs\./);
  assert.match(reaper, /image-qualification-control\.mjs reap/);
});

test("control tool fixes the reviewed policy and recovery boundary", () => {
  assert.match(control, /instanceTypes: \["t3\.small", "t3a\.small"\]/);
  assert.match(control, /maxMinutes: 120/);
  assert.match(control, /maxMonthlyUSD: 10/);
  assert.match(control, /maxConcurrentInstances: 1/);
  assert.match(control, /maxLaunches: 3/);
  assert.match(control, /fastSnapshotRestore: false/);
  assert.match(control, /CRABBOX_AWS_QUALIFICATION_TRANSPORT/);
  assert.match(control, /AWSQualificationController/);
  assert.match(control, /candidate artifact is not from this protected workflow run/);
  assert.match(control, /candidate build or artifact changed/);
  assert.match(control, /candidate changed protected build inputs/);
  assert.match(control, /candidate build-input receipt mismatch/);
  assert.match(control, /deleted_classes: \["FleetDurableObject"\]/);
  assert.match(control, /controllerCall\(controllerURL, token, "finalize"/);
  assert.match(control, /controllerCall\(controllerURL, token, "retire"/);
  assert.match(control, /workers\/scripts/);
  assert.match(control, /candidate Worker version changed after protected deployment/);
  assert.match(control, /await preparePrivateCandidate\(cf, candidateWorker, stagingBindings\)/);
  assert.match(control, /candidateMetadata\("private-bootstrap\.mjs", bindings, true\)/);
  assert.match(control, /assertWorkerIsolation\(cf, candidateWorker, 1, false\)/);
  assert.match(control, /assertWorkerIsolation\(cf, relayWorker, 0, true\)/);
  assert.match(control, /await deleteRelay\(cf, relayWorker\)/);
  assert.match(control, /authority attestation identity does not match protected expectations/);
  assert.doesNotMatch(control, /identity revalidation warning/);
});

test("executor proves auth denial, FSR denial, three launches, rollback, and hard kill", () => {
  assert.match(executor, /promote-cas/);
  assert.match(executor, /"expectedCurrent":\{"state":"capture"\}/);
  assert.match(executor, /\[\[ "\$spoof_status" == 403 \]\]/);
  assert.match(executor, /\[\[ "\$stale_status" == 409 \]\]/);
  assert.match(executor, /stale-cas-response\.json/);
  assert.match(executor, /fast-snapshot-restore/);
  assert.match(executor, /mint_status" -eq 86/);
  assert.match(executor, /launch-count.*-eq 3/);
  assert.match(executor, /kill -KILL "\$\$"/);
  assert.match(executor, /catalog-rollback\.json/);
  assert.match(executor, /execution-state\.json/);
  assert.match(executor, /qualification\/shared\/v1\/images/);
  assert.match(executor, /QUALIFICATION_EXECUTOR_TOKEN/);
  assert.doesNotMatch(executor, /QUALIFICATION_CANDIDATE_URL/);
  assert.doesNotMatch(executor, /v1\/images\?provider/);
  assert.match(adapter, /QUALIFICATION_REAL_CRABBOX/);
  assert.match(adapter, /promotion-receipt\.json/);
  assert.match(adapter, /rollback-receipt\.json/);
  assert.match(adapter, /exit 86/);
});

test("protected build prep binds source, rejects substitution, and seals exact bytes", async () => {
  const module = await import(
    `${pathToFileURL(path.join(root, "scripts/image-qualification-control.mjs"))}?test=${Date.now()}`
  );
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-qualification-manifest-"));
  try {
    const harness = path.join(temp, "harness");
    const candidate = path.join(temp, "candidate");
    const artifact = path.join(temp, "artifact");
    const initialize = (directory, source) => {
      fs.mkdirSync(path.join(directory, "worker", "src"), { recursive: true });
      fs.mkdirSync(path.join(directory, "worker", "test"), { recursive: true });
      fs.writeFileSync(path.join(directory, "go.mod"), "module example.invalid/qualification\n");
      fs.writeFileSync(path.join(directory, "go.sum"), "");
      fs.writeFileSync(path.join(directory, "worker", "package.json"), "{}\n");
      fs.writeFileSync(path.join(directory, "worker", "package-lock.json"), "{}\n");
      fs.writeFileSync(path.join(directory, "worker", "wrangler.jsonc"), "{}\n");
      fs.writeFileSync(path.join(directory, "worker", "tsconfig.json"), "{}\n");
      fs.writeFileSync(path.join(directory, "worker", "src", "index.ts"), source);
      fs.writeFileSync(path.join(directory, "worker", "test", "index.test.ts"), "test\n");
      execFileSync("git", ["init", "-q"], { cwd: directory });
      execFileSync("git", ["config", "user.name", "Qualification Test"], { cwd: directory });
      execFileSync("git", ["config", "user.email", "qualification@example.invalid"], {
        cwd: directory,
      });
      execFileSync("git", ["add", "."], { cwd: directory });
      execFileSync("git", ["commit", "-qm", "test source"], { cwd: directory });
      return execFileSync("git", ["rev-parse", "HEAD"], {
        cwd: directory,
        encoding: "utf8",
      }).trim();
    };
    const workflowSha = initialize(harness, "export default { base: true };\n");
    let candidateSha = initialize(candidate, "export default { candidate: true };\n");
    const receiptFile = path.join(artifact, "build-inputs.json");
    const receipt = module.prepareCandidateBuild(
      harness,
      candidate,
      receiptFile,
      candidateSha,
      workflowSha,
    );
    assert.equal(receipt.candidateSha, candidateSha);
    assert.equal(receipt.workflowSha, workflowSha);
    assert.equal(
      fs.readFileSync(path.join(harness, "worker", "src", "index.ts"), "utf8"),
      "export default { candidate: true };\n",
    );

    fs.mkdirSync(path.join(artifact, "bin"), { recursive: true });
    fs.mkdirSync(path.join(artifact, "worker"), { recursive: true });
    fs.writeFileSync(path.join(artifact, "bin", "crabbox"), "candidate-cli");
    fs.writeFileSync(path.join(artifact, "worker", "index.js"), "export default {};");
    const manifest = module.createManifest(candidate, artifact, candidateSha, workflowSha);
    assert.equal(
      module.verifyManifest(artifact, candidateSha, workflowSha).manifestSha256,
      manifest.manifestSha256,
    );
    fs.writeFileSync(path.join(artifact, "unexpected"), "nope");
    assert.throws(() => module.verifyManifest(artifact, candidateSha, workflowSha), /extra files/);

    fs.writeFileSync(
      path.join(candidate, "worker", "package.json"),
      '{"scripts":{"build":"write-substituted-artifact"}}\n',
    );
    execFileSync("git", ["add", "worker/package.json"], { cwd: candidate });
    execFileSync("git", ["commit", "-qm", "candidate build hook"], { cwd: candidate });
    candidateSha = execFileSync("git", ["rev-parse", "HEAD"], {
      cwd: candidate,
      encoding: "utf8",
    }).trim();
    assert.throws(
      () =>
        module.prepareCandidateBuild(harness, candidate, receiptFile, candidateSha, workflowSha),
      /candidate changed protected build inputs/,
    );

    fs.writeFileSync(path.join(candidate, "worker", "package.json"), "{}\n");
    fs.symlinkSync("../package.json", path.join(candidate, "worker", "src", "escape.ts"));
    execFileSync("git", ["add", "worker/package.json", "worker/src/escape.ts"], {
      cwd: candidate,
    });
    execFileSync("git", ["commit", "-qm", "candidate source link"], { cwd: candidate });
    candidateSha = execFileSync("git", ["rev-parse", "HEAD"], {
      cwd: candidate,
      encoding: "utf8",
    }).trim();
    assert.throws(
      () =>
        module.prepareCandidateBuild(harness, candidate, receiptFile, candidateSha, workflowSha),
      /candidate Worker source contains a non-file entry/,
    );
  } finally {
    fs.rmSync(temp, { recursive: true, force: true });
  }
});

test("publisher admission requires transactional rollback before paid deployment", async () => {
  const module = await import(
    `${pathToFileURL(path.join(root, "scripts/image-qualification-control.mjs"))}?admit=${Date.now()}`
  );
  const admissible = `
CRABBOX_BIN="\${CRABBOX_BIN:-$ROOT/bin/crabbox}"
cleanup() {
  rollback_promoted_image "$promotion_log"
}
trap cleanup EXIT
rollback_promoted_image() {
  args+=(--expected-current-image "$current_id" --expected-current-revision "$current_revision" --retire-expected-catalog "$rollback_image")
  printf 'promoted-image smoke failed; restored previous default image=%s\\n' "$rollback_image"
}
run_cmd "$CRABBOX_BIN" stop --provider aws --target "$target" "$candidate_lease"
candidate_lease=""
promote_args=(image promote --target "$target" --json --expected-current-image capture)
rollback_pending=1
run_json_tee "$promotion_log" "$CRABBOX_BIN" "\${promote_args[@]}"
promoted_lease="$(warmup promoted)"
smoke "$promoted_lease"
rollback_pending=0
`;
  assert.doesNotThrow(() => module.verifyPublisherContract(admissible));
  assert.throws(
    () =>
      module.verifyPublisherContract(
        admissible.replace("--expected-current-image capture", "ami-candidate"),
      ),
    /transactional rollback contract/,
  );
  assert.throws(
    () =>
      module.verifyPublisherContract(
        admissible.replace('candidate_lease=""', 'candidate_lease="still-running"'),
      ),
    /rollback ordering/,
  );
});

test("authorization selects one exact same-PR candidate artifact and detects replacement", async () => {
  const module = await import(
    `${pathToFileURL(path.join(root, "scripts/image-qualification-control.mjs"))}?identity=${Date.now()}`
  );
  const candidateSha = "a".repeat(40);
  const workflowSha = "b".repeat(40);
  const artifactDigest = `sha256:${"c".repeat(64)}`;
  let producerPath = ".github/workflows/image-qualification.yml@refs/heads/main";
  const environment = {
    GH_TOKEN: "test-token",
    GITHUB_REPOSITORY: "openclaw/crabbox",
    GITHUB_RUN_ID: "42",
    GITHUB_RUN_ATTEMPT: "1",
    GITHUB_WORKFLOW_REF:
      "openclaw/crabbox/.github/workflows/image-qualification.yml@refs/heads/main",
    QUALIFICATION_CANDIDATE_SHA: candidateSha,
    QUALIFICATION_CANDIDATE_ARTIFACT_DIGEST: artifactDigest.slice("sha256:".length),
    QUALIFICATION_CANDIDATE_ARTIFACT_ID: "7",
    QUALIFICATION_CANDIDATE_RUN_ID: "42",
    QUALIFICATION_CONFIRM: "qualify",
    QUALIFICATION_DEFAULT_BRANCH: "main",
    QUALIFICATION_PULL_REQUEST: "1756",
    QUALIFICATION_WORKFLOW_SHA: workflowSha,
  };
  const originalEnvironment = Object.fromEntries(
    Object.keys(environment).map((key) => [key, process.env[key]]),
  );
  const originalFetch = globalThis.fetch;
  Object.assign(process.env, environment);
  globalThis.fetch = async (input) => {
    const url = new URL(input);
    let value;
    if (url.pathname.endsWith("/pulls/1756")) {
      value = {
        state: "open",
        head: { repo: { full_name: "openclaw/crabbox" }, sha: candidateSha },
        base: { sha: workflowSha },
      };
    } else if (url.pathname.endsWith("/commits/main")) {
      value = { sha: workflowSha };
    } else if (url.pathname.endsWith("/actions/runs/42")) {
      value = {
        id: 42,
        run_attempt: 1,
        event: "workflow_dispatch",
        head_sha: workflowSha,
        head_repository: { full_name: "openclaw/crabbox" },
        path: producerPath,
      };
    } else if (url.pathname.includes("/actions/artifacts/")) {
      value = {
        id: 7,
        name: "image-qualification-candidate-42",
        expired: false,
        digest: artifactDigest,
        workflow_run: { id: 42, head_sha: workflowSha },
      };
    } else {
      throw new Error(`unexpected test URL: ${url}`);
    }
    return new Response(JSON.stringify(value), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  };
  try {
    assert.deepEqual(await module.verifyCandidateIdentity({ artifact: true }), {
      artifactDigest,
      artifactId: "7",
      candidateSha,
      number: "1756",
      repository: "openclaw/crabbox",
      runId: "42",
      workflowSha,
    });
    producerPath = ".github/workflows/image-qualification.yml@refs/heads/feature";
    await assert.rejects(
      module.verifyCandidateIdentity({ artifact: true }),
      /candidate build or artifact changed/,
    );
    producerPath = ".github/workflows/image-qualification.yml@refs/heads/main";
    process.env.QUALIFICATION_CANDIDATE_ARTIFACT_ID = "8";
    await assert.rejects(
      module.verifyCandidateIdentity({ artifact: true }),
      /candidate build or artifact changed/,
    );
  } finally {
    globalThis.fetch = originalFetch;
    delete process.env.QUALIFICATION_CANDIDATE_ARTIFACT_ID;
    for (const [key, value] of Object.entries(originalEnvironment)) {
      if (value === undefined) delete process.env[key];
      else process.env[key] = value;
    }
  }
});

test("artifact digest normalization matches upload and REST API contracts", async () => {
  const module = await import(
    `${pathToFileURL(path.join(root, "scripts/image-qualification-control.mjs"))}?digest=${Date.now()}`
  );
  const digest = "c".repeat(64);
  assert.equal(module.normalizeArtifactDigest(digest), `sha256:${digest}`);
  assert.equal(module.normalizeArtifactDigest(`sha256:${digest}`), `sha256:${digest}`);
  assert.throws(() => module.normalizeArtifactDigest("not-a-digest"), /absent or invalid/);
});

test("protected producer identity accepts only the default-branch workflow path", async () => {
  const module = await import(
    `${pathToFileURL(path.join(root, "scripts/image-qualification-control.mjs"))}?path=${Date.now()}`
  );
  const expected = ".github/workflows/image-qualification.yml";
  assert.equal(module.workflowRunPathMatches(expected, expected, "main"), true);
  assert.equal(module.workflowRunPathMatches(`${expected}@refs/heads/main`, expected, "main"), true);
  assert.equal(
    module.workflowRunPathMatches(`${expected}@refs/heads/feature/test`, expected, "main"),
    false,
  );
  assert.equal(
    module.workflowRunPathMatches(`${expected}@refs/pull/1756/merge`, expected, "main"),
    false,
  );
  assert.equal(module.workflowRunPathMatches(`${expected}@main`, expected, "main"), false);
});

test("attestation gate requires FSR denial and exact sequential launch order", async () => {
  const module = await import(
    `${pathToFileURL(path.join(root, "scripts/image-qualification-control.mjs"))}?evidence=${Date.now()}`
  );
  const operation = (action, requestedAt, accepted = true) => ({
    action,
    requestedAt,
    signerDispatches: accepted
      ? [
          {
            beforeAt: new Date(Date.parse(requestedAt) + 10).toISOString(),
            afterAt: new Date(Date.parse(requestedAt) + 20).toISOString(),
            outcome: "accepted",
          },
        ]
      : [],
  });
  const attestation = {
    finalized: true,
    operations: [
      operation("DescribeImages", "2026-09-04T00:00:00.900Z"),
      operation("DescribeImages", "2026-09-04T00:00:01.200Z"),
      {
        ...operation("EnableFastSnapshotRestores", "2026-09-04T00:00:02.000Z", false),
        denialReason: "policy-denied",
      },
      operation("RunInstances", "2026-09-04T00:00:03.000Z"),
      operation("CreateImage", "2026-09-04T00:00:04.000Z"),
      operation("RunInstances", "2026-09-04T00:00:05.000Z"),
      operation("RunInstances", "2026-09-04T00:00:06.000Z"),
    ],
    finalReceipt: {
      failureCodes: [],
      resourcesAtStart: { images: 1, instances: 0, keyPairs: 1, snapshots: 1, volumes: 0 },
    },
  };
  const proof = {
    spoof: {
      status: 403,
      catalogUnchanged: true,
      startedAt: "2026-09-04T00:00:01.000Z",
      completedAt: "2026-09-04T00:00:01.100Z",
    },
    fsr: { rejected: true },
    catalog: {
      seededDefaultReadback: true,
      priorDefaultImageRestored: true,
      rollbackRevisionAdvanced: true,
      failedCatalogRevisionRetired: true,
      staleCASRejected: true,
      staleReadbackUnchanged: true,
      priorImageDigest: "a".repeat(64),
      priorRevisionDigest: "b".repeat(64),
      failedImageDigest: "c".repeat(64),
      failedRevisionDigest: "d".repeat(64),
      restoredRevisionDigest: "e".repeat(64),
    },
    execution: {
      mintExit: 86,
      injectedAfterPromotedSmoke: true,
      launchCount: 3,
      smokeCount: 3,
    },
    hardKill: { executorKilled: true, cloudCredentialsPresent: false },
    log: "supplemental log can be empty of rollback assertions",
  };
  assert.doesNotThrow(() => module.verifyQualificationEvidence(attestation, proof));
  assert.throws(
    () =>
      module.verifyQualificationEvidence(
        {
          ...attestation,
          operations: [
            ...attestation.operations,
            operation("RunInstances", "2026-09-04T00:00:07.000Z"),
          ],
        },
        proof,
      ),
    /launch order/,
  );
  assert.throws(
    () =>
      module.verifyQualificationEvidence(
        {
          ...attestation,
          operations: [
            {
              ...operation("EnableFastSnapshotRestores", "2026-09-04T00:00:02.000Z"),
              denialReason: "policy-denied",
            },
            ...attestation.operations.filter(
              (entry) => entry.action !== "EnableFastSnapshotRestores",
            ),
          ],
        },
        proof,
      ),
    /pre-signer/,
  );
  assert.throws(
    () =>
      module.verifyQualificationEvidence(
        {
          ...attestation,
          operations: [
            ...attestation.operations,
            operation("DescribeImages", "2026-09-04T00:00:01.050Z"),
          ],
        },
        proof,
      ),
    /spoofed admin probe/,
  );
  assert.throws(
    () =>
      module.verifyQualificationEvidence(
        {
          ...attestation,
          operations: attestation.operations.map((entry, index) =>
            index === 0
              ? {
                  ...entry,
                  signerDispatches: [
                    {
                      ...entry.signerDispatches[0],
                      afterAt: "2026-09-04T00:00:01.100Z",
                    },
                  ],
                }
              : entry,
          ),
        },
        proof,
      ),
    /spoofed admin probe/,
  );
});

test("catalog proof requires a fresh rollback revision and rejects stale CAS mutation", async () => {
  const module = await import(
    `${pathToFileURL(path.join(root, "scripts/image-qualification-control.mjs"))}?catalog=${Date.now()}`
  );
  const prior = { id: "ami-11111111", revision: "revision-prior", promotedAt: "before" };
  const failed = { id: "ami-22222222", revision: "revision-failed", promotedAt: "during" };
  const restored = {
    ...prior,
    revision: "revision-rollback",
    promotedAt: "after",
    fastSnapshotRestores: [],
  };
  const input = {
    seed: prior,
    seededReadback: { image: prior },
    promotion: {
      image: failed,
      previous: { state: "present", imageId: prior.id, revision: prior.revision },
    },
    rollback: {
      image: restored,
      previous: { state: "present", imageId: failed.id, revision: failed.revision },
    },
    restoredReadback: { image: restored },
    failedReadback: {},
    failedStatus: 404,
    staleResponse: {
      error: "image_promotion_precondition_failed",
      expected: { state: "present", imageId: prior.id, revision: prior.revision },
      current: { state: "present", imageId: restored.id, revision: restored.revision },
    },
    staleStatus: 409,
    staleReadback: { image: structuredClone(restored) },
    staleFailedReadback: {},
    staleFailedStatus: 404,
  };
  const evidence = module.verifyCatalogRollbackEvidence(input);
  assert.equal(evidence.priorDefaultImageRestored, true);
  assert.equal(evidence.rollbackRevisionAdvanced, true);
  assert.equal(evidence.failedCatalogRevisionRetired, true);
  assert.equal(evidence.staleCASRejected, true);
  assert.equal(evidence.staleReadbackUnchanged, true);
  assert.match(evidence.restoredRevisionDigest, /^[0-9a-f]{64}$/);
  assert.equal("priorDefaultRevisionRestored" in evidence, false);
  assert.throws(
    () =>
      module.verifyCatalogRollbackEvidence({
        ...input,
        rollback: { ...input.rollback, image: prior },
      }),
    /fresh revision/,
  );
  assert.throws(
    () =>
      module.verifyCatalogRollbackEvidence({
        ...input,
        restoredReadback: { image: { ...restored, revision: "revision-mismatch" } },
      }),
    /receipt and restored candidate API readback/,
  );
  assert.throws(
    () =>
      module.verifyCatalogRollbackEvidence({
        ...input,
        rollback: {
          ...input.rollback,
          previous: { state: "present", imageId: failed.id, revision: prior.revision },
        },
      }),
    /fresh revision/,
  );
  assert.throws(
    () =>
      module.verifyCatalogRollbackEvidence({
        ...input,
        staleResponse: {
          ...input.staleResponse,
          current: { state: "present", imageId: restored.id, revision: failed.revision },
        },
      }),
    /stale CAS response/,
  );
  assert.throws(
    () =>
      module.verifyCatalogRollbackEvidence({
        ...input,
        failedStatus: 200,
        failedReadback: { image: failed },
      }),
    /remains in the candidate catalog/,
  );
  assert.throws(
    () =>
      module.verifyCatalogRollbackEvidence({
        ...input,
        staleReadback: { image: { ...restored, fastSnapshotRestores: ["us-east-1a"] } },
      }),
    /stale CAS changed/,
  );
  assert.throws(
    () =>
      module.verifyCatalogRollbackEvidence({
        ...input,
        staleFailedReadback: { image: failed },
        staleFailedStatus: 200,
      }),
    /stale CAS changed/,
  );
});

test("final attestation must match every protected identity field and timestamp", async () => {
  const module = await import(
    `${pathToFileURL(path.join(root, "scripts/image-qualification-control.mjs"))}?attestation=${Date.now()}`
  );
  const expected = {
    runId: "image-qualification-1-1",
    candidateSha: "a".repeat(40),
    candidateWorker: "crabbox-image-qualification-1-1",
    deploymentHash: "b".repeat(64),
    authoritySha: "c".repeat(40),
    authorityVersion: "qualification-v1",
    policyHash: "d".repeat(64),
    enrolledAt: "2026-09-04T00:00:00.000Z",
    expiresAt: "2026-09-04T02:00:00.000Z",
  };
  const attestation = {
    version: 1,
    ...expected,
    finalizingAt: "2026-09-04T01:00:00.000Z",
    finalizedAt: "2026-09-04T01:01:00.000Z",
    finalized: true,
  };
  assert.doesNotThrow(() =>
    module.verifyAttestationIdentity(attestation, expected, { finalized: true }),
  );
  assert.throws(
    () =>
      module.verifyAttestationIdentity({ ...attestation, policyHash: "e".repeat(64) }, expected, {
        finalized: true,
      }),
    /protected expectations/,
  );
  assert.throws(
    () =>
      module.verifyAttestationIdentity(
        { ...attestation, finalizedAt: "2026-09-03T23:59:00.000Z" },
        expected,
        { finalized: true },
      ),
    /timestamps/,
  );
});

test("execution manifest binds the exact final Worker version and authority policy", async () => {
  const module = await import(
    `${pathToFileURL(path.join(root, "scripts/image-qualification-control.mjs"))}?execution=${Date.now()}`
  );
  const identity = {
    runId: "image-qualification-1-1",
    candidateSha: "a".repeat(40),
    candidateWorker: "crabbox-image-qualification-1-1",
    candidateVersion: "worker-version-1",
    relayWorker: "crabbox-image-qualification-relay-1-1",
    relayVersion: "relay-version-1",
    relayBindingDigest: "0".repeat(64),
    deploymentHash: "b".repeat(64),
    manifestSha256: "c".repeat(64),
    bindingDigest: "d".repeat(64),
    authoritySha: "e".repeat(40),
    authorityVersion: "qualification-v1",
    policyHash: "f".repeat(64),
    enrolledAt: "2026-09-04T00:00:00.000Z",
    expiresAt: "2026-09-04T02:00:00.000Z",
  };
  const exact = module.executionManifestDigest(identity);
  assert.notEqual(
    module.executionManifestDigest({ ...identity, candidateVersion: "worker-version-2" }),
    exact,
  );
  assert.notEqual(
    module.executionManifestDigest({ ...identity, relayVersion: "relay-version-2" }),
    exact,
  );
  assert.notEqual(
    module.executionManifestDigest({ ...identity, policyHash: "0".repeat(64) }),
    exact,
  );
});

test("relay rejects non-contract requests before invoking the private candidate", async () => {
  const worker = (
    await import(
      `${pathToFileURL(path.join(root, "scripts/image-qualification-relay-worker.mjs"))}?test=${Date.now()}`
    )
  ).default;
  const calls = [];
  const env = {
    EXECUTOR_TOKEN: "e".repeat(64),
    CANDIDATE_ADMIN_TOKEN: "a".repeat(64),
    CANDIDATE_SHARED_TOKEN: "s".repeat(64),
    AWS_REGION: "us-east-1",
    QUALIFICATION_OWNER: "image-qualification-1-1@example.invalid",
    QUALIFICATION_ORG: "image-qualification",
    CANDIDATE: {
      fetch: async (request) => {
        calls.push(request);
        return new Response(
          JSON.stringify({ authorization: request.headers.get("authorization") }),
          {
            status: 200,
            headers: { "content-type": "application/json" },
          },
        );
      },
    },
  };
  const authorization = { authorization: `Bearer ${env.EXECUTOR_TOKEN}` };
  const allowed = await worker.fetch(
    new Request(
      "https://relay.invalid/v1/images/ami-12345678?provider=aws&target=linux&region=us-east-1",
      { headers: authorization },
    ),
    env,
  );
  assert.equal(allowed.status, 200);
  assert.doesNotMatch(await allowed.text(), new RegExp(env.CANDIDATE_ADMIN_TOKEN));
  assert.equal(calls.length, 1);
  assert.equal(
    calls[0].url,
    "https://candidate.invalid/v1/images/ami-12345678?provider=aws&region=us-east-1&target=linux",
  );
  assert.equal(calls[0].headers.get("authorization"), `Bearer ${env.CANDIDATE_ADMIN_TOKEN}`);
  assert.equal(calls[0].headers.get("x-crabbox-owner"), env.QUALIFICATION_OWNER);

  const shared = await worker.fetch(
    new Request(
      "https://relay.invalid/qualification/shared/v1/images/ami-12345678/promote-cas?provider=aws&target=linux&region=us-east-1",
      {
        method: "POST",
        headers: { ...authorization, "content-type": "application/json" },
        body: "{}",
      },
    ),
    env,
  );
  assert.equal(shared.status, 200);
  assert.doesNotMatch(await shared.text(), new RegExp(env.CANDIDATE_SHARED_TOKEN));
  assert.equal(calls.length, 2);
  assert.equal(calls[1].headers.get("authorization"), `Bearer ${env.CANDIDATE_SHARED_TOKEN}`);
  assert.equal(
    calls[1].url,
    "https://candidate.invalid/v1/images/ami-12345678/promote-cas?provider=aws&region=us-east-1&target=linux",
  );

  const lease = await worker.fetch(
    new Request("https://relay.invalid/v1/leases", {
      method: "POST",
      headers: {
        ...authorization,
        "content-type": "application/json",
        prefer: "respond-async",
      },
      body: JSON.stringify({
        provider: "aws",
        target: "linux",
        class: "standard",
        serverType: "t3.small",
        awsRegion: "us-east-1",
        desktop: false,
        browser: false,
        tailscale: false,
        capacity: { market: "on-demand" },
      }),
    }),
    env,
  );
  assert.equal(lease.status, 200);
  assert.equal(calls.length, 3);
  assert.equal(calls[2].url, "https://candidate.invalid/v1/leases");
  assert.equal(calls[2].headers.get("prefer"), "respond-async");
  assert.equal(calls[2].headers.get("authorization"), `Bearer ${env.CANDIDATE_ADMIN_TOKEN}`);

  const rejected = [
    new Request("https://relay.invalid/v1/images/ami-12345678", {
      headers: { authorization: "Bearer wrong" },
    }),
    new Request("https://relay.invalid/v1/admin/leases", { headers: authorization }),
    new Request(
      "https://relay.invalid/v1/images/ami-12345678?provider=aws&target=windows&region=us-east-1",
      { headers: authorization },
    ),
    new Request(
      "https://relay.invalid/v1/images/ami-12345678?provider=aws&target=linux&region=us-east-1",
      { method: "PUT", headers: authorization },
    ),
    new Request(
      "https://relay.invalid/v1/images/ami-12345678?provider=aws&target=linux&region=us-east-1",
      { headers: { ...authorization, "x-original-url": "https://attacker.invalid/" } },
    ),
    new Request("https://relay.invalid/v1/images", {
      method: "POST",
      headers: { ...authorization, "content-type": "application/json" },
      body: JSON.stringify({ value: "x".repeat(16 * 1024) }),
    }),
    new Request("https://relay.invalid/v1/images", {
      method: "POST",
      headers: { ...authorization, "content-type": "text/plain" },
      body: "{}",
    }),
    new Request("https://relay.invalid/v1/leases", {
      method: "POST",
      headers: {
        ...authorization,
        "content-type": "application/json",
        prefer: "respond-async",
      },
      body: JSON.stringify({
        provider: "aws",
        target: "windows",
        class: "standard",
        serverType: "t3.small",
        awsRegion: "us-east-1",
        desktop: false,
        browser: false,
        tailscale: false,
        capacity: { market: "on-demand" },
      }),
    }),
  ];
  for (const request of rejected) {
    const response = await worker.fetch(request, env);
    assert.ok(response.status >= 400);
  }
  assert.equal(calls.length, 3);
  assert.doesNotMatch(relaySource, /AUTHORITY|CONTROLLER|CLOUDFLARE|AWS_ACCESS_KEY/);
});

test("controller rejects unauthenticated and oversized control requests", async () => {
  const worker = (
    await import(
      `${pathToFileURL(path.join(root, "scripts/image-qualification-controller-worker.mjs"))}?test=${Date.now()}`
    )
  ).default;
  const env = {
    CONTROLLER_TOKEN: "x".repeat(32),
    AUTHORITY: { discover: async () => undefined },
  };
  const forbidden = await worker.fetch(
    new Request("https://controller.invalid/discover", { method: "POST" }),
    env,
  );
  assert.equal(forbidden.status, 403);
  const oversized = await worker.fetch(
    new Request("https://controller.invalid/discover", {
      method: "POST",
      headers: { authorization: `Bearer ${env.CONTROLLER_TOKEN}` },
      body: "x".repeat(4097),
    }),
    env,
  );
  assert.equal(oversized.status, 409);
  const discovered = await worker.fetch(
    new Request("https://controller.invalid/discover", {
      method: "POST",
      headers: { authorization: `Bearer ${env.CONTROLLER_TOKEN}` },
    }),
    env,
  );
  assert.equal(discovered.status, 200);
  assert.deepEqual(await discovered.json(), { run: null });
  const failed = await worker.fetch(
    new Request("https://controller.invalid/discover", {
      method: "POST",
      headers: { authorization: `Bearer ${env.CONTROLLER_TOKEN}` },
    }),
    {
      ...env,
      AUTHORITY: {
        discover: async () => {
          throw new Error("private stack and resource details");
        },
      },
    },
  );
  assert.equal(failed.status, 409);
  assert.deepEqual(await failed.json(), { error: "controller_error" });
});

test("adapter delegates and injects exit 86 only after the third launch smoke", () => {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-qualification-adapter-"));
  try {
    const fake = path.join(temp, "crabbox");
    fs.writeFileSync(
      fake,
      `#!/usr/bin/env bash
if [[ "\${1:-}" == image && "\${2:-}" == promote ]]; then
  printf '{"image":{"id":"ami-11111111","revision":"revision"},"previous":{"state":"present","imageId":"ami-22222222","revision":"previous"}}\\n'
else
  printf 'delegated %s\\n' "\${1:-}"
fi
`,
      { mode: 0o700 },
    );
    const env = {
      ...process.env,
      QUALIFICATION_REAL_CRABBOX: fake,
      QUALIFICATION_ADAPTER_STATE: path.join(temp, "state"),
    };
    for (let index = 0; index < 3; index += 1) {
      assert.equal(
        spawnSync(path.join(root, "scripts/image-qualification-crabbox-adapter.sh"), ["warmup"], {
          env,
        }).status,
        0,
      );
    }
    const smoke = spawnSync(
      path.join(root, "scripts/image-qualification-crabbox-adapter.sh"),
      ["run"],
      { env, encoding: "utf8" },
    );
    assert.equal(smoke.status, 86);
    assert.match(smoke.stdout, /delegated run/);
    assert.equal(
      spawnSync(path.join(root, "scripts/image-qualification-crabbox-adapter.sh"), ["run"], {
        env,
      }).status,
      0,
    );
    assert.equal(
      spawnSync(
        path.join(root, "scripts/image-qualification-crabbox-adapter.sh"),
        ["image", "promote", "--expected-current-image", "capture"],
        { env },
      ).status,
      0,
    );
    assert.equal(
      spawnSync(
        path.join(root, "scripts/image-qualification-crabbox-adapter.sh"),
        ["image", "promote", "--retire-expected-catalog", "ami-11111111"],
        { env },
      ).status,
      0,
    );
    assert.ok(fs.existsSync(path.join(temp, "state", "promotion-receipt.json")));
    assert.ok(fs.existsSync(path.join(temp, "state", "rollback-receipt.json")));
  } finally {
    fs.rmSync(temp, { recursive: true, force: true });
  }
});
