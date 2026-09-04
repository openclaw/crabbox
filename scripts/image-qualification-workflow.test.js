import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";
import { execFileSync, spawnSync } from "node:child_process";

const root = path.resolve(import.meta.dirname, "..");
const read = (file) => fs.readFileSync(path.join(root, file), "utf8");
const candidateWorkflow = read(".github/workflows/image-qualification-candidate.yml");
const workflow = read(".github/workflows/image-qualification.yml");
const reaper = read(".github/workflows/image-qualification-reaper.yml");
const control = read("scripts/image-qualification-control.mjs");
const executor = read("scripts/image-qualification-execute.sh");
const adapter = read("scripts/image-qualification-crabbox-adapter.sh");

test("workflow isolates candidate execution from protected credentials", () => {
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.match(candidateWorkflow, /^  pull_request:$/m);
  assert.doesNotMatch(candidateWorkflow, /workflow_dispatch:|environment:|secrets\./);
  assert.match(
    candidateWorkflow,
    /if: github\.event\.pull_request\.head\.repo\.full_name == github\.repository/,
  );
  assert.match(candidateWorkflow, /cache: false/g);
  assert.match(candidateWorkflow, /AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN/);
  assert.match(candidateWorkflow, /CLOUDFLARE_API_TOKEN/);
  assert.match(workflow, /group: image-qualification\n  cancel-in-progress: false/);
  assert.match(workflow, /environment: image-qualification/);
  assert.match(workflow, /if: always\(\)/);
  assert.doesNotMatch(workflow, /go build|npm ci|Check out exact candidate/);
  assert.match(workflow, /candidate_artifact_id/);
  assert.match(workflow, /github-token: \$\{\{ github\.token \}\}/);
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
  assert.match(workflow, /deploy-enroll:[\s\S]*needs: \[authorize, admit\]/);
  const executeJob = workflow.slice(
    workflow.indexOf("  execute:"),
    workflow.indexOf("  finalize:"),
  );
  assert.doesNotMatch(
    executeJob,
    /CLOUDFLARE_API_TOKEN|AWS_ACCESS_KEY_ID|QUALIFICATION_CONTROLLER_TOKEN/,
  );
  assert.match(executeJob, /QUALIFICATION_ADMIN_TOKEN/);
  assert.match(executeJob, /QUALIFICATION_SHARED_TOKEN/);
  const protectedJobs = workflow.slice(workflow.indexOf("  deploy-enroll:"));
  assert.doesNotMatch(protectedJobs, /npm ci|go build|wrangler deploy/);
  assert.match(protectedJobs, /candidate bytes as inert data/);
});

test("all workflow actions use immutable repository-standard pins", () => {
  for (const source of [candidateWorkflow, workflow, reaper]) {
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
  assert.match(control, /image-qualification-candidate\.yml/);
  assert.match(control, /candidate build or artifact changed/);
  assert.match(control, /deleted_classes: \["FleetDurableObject"\]/);
  assert.match(control, /controllerCall\(controllerURL, token, "finalize"/);
  assert.match(control, /controllerCall\(controllerURL, token, "retire"/);
  assert.match(control, /workers\/scripts/);
});

test("executor proves auth denial, FSR denial, three launches, rollback, and hard kill", () => {
  assert.match(executor, /promote-cas/);
  assert.match(executor, /\[\[ "\$spoof_status" == 403 \]\]/);
  assert.match(executor, /fast-snapshot-restore/);
  assert.match(executor, /mint_status" -eq 86/);
  assert.match(executor, /launch-count.*-eq 3/);
  assert.match(executor, /kill -KILL "\$\$"/);
  assert.match(executor, /restored previous default image=/);
  assert.match(adapter, /QUALIFICATION_REAL_CRABBOX/);
  assert.match(adapter, /exit 86/);
});

test("manifest binds exact candidate bytes and rejects extras", async () => {
  const module = await import(
    `${pathToFileURL(path.join(root, "scripts/image-qualification-control.mjs"))}?test=${Date.now()}`
  );
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-qualification-manifest-"));
  try {
    const candidate = path.join(temp, "candidate");
    const artifact = path.join(temp, "artifact");
    fs.mkdirSync(candidate, { recursive: true });
    execFileSync("git", ["init", "-q"], { cwd: candidate });
    execFileSync("git", ["config", "user.name", "Qualification Test"], { cwd: candidate });
    execFileSync("git", ["config", "user.email", "qualification@example.invalid"], {
      cwd: candidate,
    });
    fs.writeFileSync(path.join(candidate, "source"), "candidate\n");
    execFileSync("git", ["add", "source"], { cwd: candidate });
    execFileSync("git", ["commit", "-qm", "test source"], { cwd: candidate });
    const candidateSha = execFileSync("git", ["rev-parse", "HEAD"], {
      cwd: candidate,
      encoding: "utf8",
    }).trim();
    const workflowSha = "b".repeat(40);
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
  const environment = {
    GH_TOKEN: "test-token",
    GITHUB_REPOSITORY: "openclaw/crabbox",
    GITHUB_RUN_ATTEMPT: "1",
    GITHUB_WORKFLOW_REF:
      "openclaw/crabbox/.github/workflows/image-qualification.yml@refs/heads/main",
    QUALIFICATION_CANDIDATE_SHA: candidateSha,
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
    } else if (url.pathname.includes("/actions/workflows/")) {
      value = {
        workflow_runs: [
          {
            id: 42,
            event: "pull_request",
            conclusion: "success",
            head_sha: candidateSha,
            head_repository: { full_name: "openclaw/crabbox" },
            path: ".github/workflows/image-qualification-candidate.yml",
            pull_requests: [{ number: 1756 }],
          },
        ],
      };
    } else if (url.pathname.endsWith("/actions/runs/42/artifacts")) {
      value = {
        artifacts: [
          {
            id: 7,
            name: "image-qualification-candidate-42",
            expired: false,
            digest: artifactDigest,
            workflow_run: { id: 42, head_sha: candidateSha },
          },
        ],
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

test("attestation gate requires FSR denial and exact sequential launch order", async () => {
  const module = await import(
    `${pathToFileURL(path.join(root, "scripts/image-qualification-control.mjs"))}?evidence=${Date.now()}`
  );
  const operation = (action, accepted = true) => ({
    action,
    signerDispatches: accepted ? [{ outcome: "accepted" }] : [],
  });
  const attestation = {
    finalized: true,
    operations: [
      {
        ...operation("EnableFastSnapshotRestores", false),
        denialReason: "policy-denied",
        requestedAt: "2026-09-04T00:00:02.000Z",
      },
      { ...operation("RunInstances"), requestedAt: "2026-09-04T00:00:03.000Z" },
      { ...operation("CreateImage"), requestedAt: "2026-09-04T00:00:04.000Z" },
      { ...operation("RunInstances"), requestedAt: "2026-09-04T00:00:05.000Z" },
      { ...operation("RunInstances"), requestedAt: "2026-09-04T00:00:06.000Z" },
    ],
    finalReceipt: {
      failureCodes: [],
      resourcesAtStart: { images: 1, instances: 0, keyPairs: 1, snapshots: 1, volumes: 0 },
    },
  };
  const proof = {
    spoof: { status: 403, catalogUnchanged: true, completedAt: "2026-09-04T00:00:01.000Z" },
    fsr: { rejected: true },
    hardKill: { executorKilled: true, cloudCredentialsPresent: false },
    log: "qualification mint exited 86 only after the promoted smoke\nrestored previous default image=[aws-resource]\n--retire-expected-catalog",
  };
  assert.doesNotThrow(() => module.verifyQualificationEvidence(attestation, proof));
  assert.throws(
    () =>
      module.verifyQualificationEvidence(
        {
          ...attestation,
          operations: [...attestation.operations, operation("RunInstances")],
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
            { ...operation("EnableFastSnapshotRestores"), denialReason: "policy-denied" },
            ...attestation.operations.slice(1),
          ],
        },
        proof,
      ),
    /pre-signer/,
  );
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
    fs.writeFileSync(fake, "#!/usr/bin/env bash\nprintf 'delegated %s\\n' \"$1\"\n", {
      mode: 0o700,
    });
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
  } finally {
    fs.rmSync(temp, { recursive: true, force: true });
  }
});
