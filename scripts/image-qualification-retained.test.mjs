import assert from "node:assert/strict";
import { execFileSync, spawnSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

import {
  canonical,
  createRetainedCapsule,
  digest,
  retainedImageFromEnv,
  verifyManifest,
  verifyRetainedSelection,
} from "./image-qualification-control.mjs";
import relay from "./image-qualification-relay-worker.mjs";

const root = path.resolve(import.meta.dirname, "..");
const names = [
  "QUALIFICATION_MODE",
  "QUALIFICATION_RETAINED_IMAGE_ID",
  "QUALIFICATION_RETAINED_SNAPSHOT_ID",
  "QUALIFICATION_RETAINED_SOURCE_SHA",
  "QUALIFICATION_RETAINED_CAPSULE_SHA256",
  "QUALIFICATION_CANDIDATE_SHA",
];
const capsulePaths = [
  "internal/cli/actions.go",
  "recipes/devtools/v1/linux-x86_64.json",
  "recipes/devtools/v1/recipe.schema.json",
  "recipes/linux/v1/linux-builder.json",
  "recipes/linux/v1/linux-minimal.json",
  "recipes/linux/v1/manifest.schema.json",
  "recipes/linux/v1/profile.schema.json",
  "scripts/devtools-image-contract.mjs",
  "scripts/devtools-image-smoke-linux.sh",
  "scripts/generate-linux-readiness.mjs",
  "scripts/install-linux-developer-tools.sh",
  "scripts/linux-readiness.generated.sh",
  "scripts/mint-aws-devtools-image.sh",
];

function scopedEnvironment(callback) {
  const old = Object.fromEntries(names.map((name) => [name, process.env[name]]));
  for (const name of names) delete process.env[name];
  try {
    return callback();
  } finally {
    for (const name of names) {
      if (old[name] === undefined) delete process.env[name];
      else process.env[name] = old[name];
    }
  }
}

test("retained admission requires a complete, exact identity and rejects mint mixing", () => {
  scopedEnvironment(() => {
    assert.equal(retainedImageFromEnv(), undefined);
    process.env.QUALIFICATION_MODE = "retained";
    assert.throws(retainedImageFromEnv, /missing/);
    Object.assign(process.env, {
      QUALIFICATION_RETAINED_IMAGE_ID: "ami-11111111",
      QUALIFICATION_RETAINED_SNAPSHOT_ID: "snap-22222222",
      QUALIFICATION_RETAINED_SOURCE_SHA: "a".repeat(40),
      QUALIFICATION_RETAINED_CAPSULE_SHA256: "b".repeat(64),
    });
    assert.equal(retainedImageFromEnv().imageId, "ami-11111111");
    process.env.QUALIFICATION_MODE = "mint";
    assert.throws(retainedImageFromEnv, /cannot accept/);
    process.env.QUALIFICATION_MODE = "other";
    assert.throws(retainedImageFromEnv, /invalid/);
  });
});

test("retained capsule binds all 13 entries and permits only nonbaked CLI source drift", () => {
  scopedEnvironment(() => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-retained-capsule-"));
    try {
      const git = (...args) =>
        execFileSync("git", args, {
          cwd: temp,
          encoding: "utf8",
          stdio: ["ignore", "pipe", "pipe"],
        }).trim();
      git("init", "-q");
      const records = capsulePaths.map((file) => {
        const bytes = Buffer.from(`${file}\n`);
        fs.mkdirSync(path.dirname(path.join(temp, file)), { recursive: true });
        fs.writeFileSync(path.join(temp, file), bytes);
        return {
          path: file,
          mode: "100644",
          blob: crypto
            .createHash("sha1")
            .update(`blob ${bytes.length}\0`)
            .update(bytes)
            .digest("hex"),
          bytes: bytes.length,
          sha256: digest(bytes),
        };
      });
      git("add", ".");
      git(
        "-c",
        "user.name=Test",
        "-c",
        "user.email=test@example.invalid",
        "-c",
        "commit.gpgsign=false",
        "commit",
        "-qm",
        "fixture",
      );
      const sourceSha = git("rev-parse", "HEAD");
      Object.assign(process.env, {
        QUALIFICATION_MODE: "retained",
        QUALIFICATION_RETAINED_IMAGE_ID: "ami-11111111",
        QUALIFICATION_RETAINED_SNAPSHOT_ID: "snap-22222222",
        QUALIFICATION_RETAINED_SOURCE_SHA: sourceSha,
        QUALIFICATION_CANDIDATE_SHA: sourceSha,
        QUALIFICATION_RETAINED_CAPSULE_SHA256: digest(canonical(records)),
      });
      const receipt = path.join(temp, "receipt.json");
      const actual = createRetainedCapsule(temp, temp, receipt);
      assert.equal(actual.sourceFiles.length, 13);
      assert.equal(actual.bakedInputsIdentical, true);
      const candidate = path.join(temp, "candidate");
      for (const file of capsulePaths) {
        fs.mkdirSync(path.dirname(path.join(candidate, file)), { recursive: true });
        fs.copyFileSync(path.join(temp, file), path.join(candidate, file));
      }
      fs.appendFileSync(path.join(candidate, capsulePaths[0]), "// new CLI runtime\n");
      const candidateGit = (...args) =>
        execFileSync("git", args, {
          cwd: candidate,
          encoding: "utf8",
          stdio: ["ignore", "pipe", "pipe"],
        }).trim();
      candidateGit("init", "-q");
      candidateGit("add", ".");
      candidateGit(
        "-c",
        "user.name=Test",
        "-c",
        "user.email=test@example.invalid",
        "-c",
        "commit.gpgsign=false",
        "commit",
        "-qm",
        "candidate",
      );
      process.env.QUALIFICATION_CANDIDATE_SHA = candidateGit("rev-parse", "HEAD");
      const drifted = createRetainedCapsule(temp, candidate, receipt);
      assert.notEqual(drifted.sourceFiles[0].blob, drifted.candidateFiles[0].blob);
      fs.appendFileSync(path.join(candidate, capsulePaths[11]), "# baked change\n");
      candidateGit("add", ".");
      candidateGit(
        "-c",
        "user.name=Test",
        "-c",
        "user.email=test@example.invalid",
        "-c",
        "commit.gpgsign=false",
        "commit",
        "-qm",
        "baked change",
      );
      process.env.QUALIFICATION_CANDIDATE_SHA = candidateGit("rev-parse", "HEAD");
      assert.throws(
        () => createRetainedCapsule(temp, candidate, receipt),
        /changed a retained image/,
      );
      process.env.QUALIFICATION_CANDIDATE_SHA = sourceSha;
      fs.appendFileSync(path.join(temp, capsulePaths[11]), "# changed\n");
      assert.throws(
        () => createRetainedCapsule(temp, temp, receipt),
        /differs from its exact checkout/,
      );
      fs.writeFileSync(path.join(temp, capsulePaths[11]), `${capsulePaths[11]}\n`);
      process.env.QUALIFICATION_RETAINED_CAPSULE_SHA256 = "f".repeat(64);
      assert.throws(() => createRetainedCapsule(temp, temp, receipt), /reviewed receipt/);
    } finally {
      fs.rmSync(temp, { recursive: true, force: true });
    }
  });
});

const currentInstaller = fs.readFileSync(
  path.join(root, "scripts/install-linux-developer-tools.sh"),
  "utf8",
);
const legacyInstaller = currentInstaller.replace(
  /^(?:rust|uv)_smoke_script\(\) \{\n[\s\S]*?^\}\n/gm,
  "",
);
// The retained installer adds these exact generator declarations. Keep the real
// Node/Go/Bun installer source instead of substituting permissive shell stubs.
const retainedInstaller =
  legacyInstaller +
  String.raw`
rust_smoke_script() {
  printf 'pinned_rust_version=%q\nrust_seed_root=%q\n' "$pinned_rust_version" "$rust_seed_root"
  declare -f log linux_x64_supported toolchain_archive_spec verify_toolchain_archive check_root_owned_path rust_seed_inputs rust_seed_path check_rust_seed rust_runtime_probe
  printf '%s\n' 'if linux_x64_supported; then' '  rust_runtime_probe' 'fi'
}

uv_smoke_script() {
  printf 'public_toolchain_archive_dir=%q\npinned_uv_version=%q\n' "$public_toolchain_archive_dir" "$pinned_uv_version"
  declare -f log linux_x64_supported toolchain_archive_spec verify_toolchain_archive stage_toolchain_archive offline_uv_probe
  printf '%s\n' 'if linux_x64_supported; then' '  offline_uv_probe' 'fi'
}
`;

function withInstallerBundle(source, callback) {
  return scopedEnvironment(() => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-retained-admission-"));
    try {
      const candidateSha = "a".repeat(40);
      const workflowSha = "b".repeat(40);
      const installerPath = "scripts/install-linux-developer-tools.sh";
      const records = capsulePaths.map((file) => {
        const bytes = Buffer.from(file === installerPath ? source : `${file}\n`);
        return {
          path: file,
          mode: "100644",
          blob: crypto
            .createHash("sha1")
            .update(`blob ${bytes.length}\0`)
            .update(bytes)
            .digest("hex"),
          bytes: bytes.length,
          sha256: digest(bytes),
        };
      });
      Object.assign(process.env, {
        QUALIFICATION_MODE: "retained",
        QUALIFICATION_RETAINED_IMAGE_ID: "ami-11111111",
        QUALIFICATION_RETAINED_SNAPSHOT_ID: "snap-22222222",
        QUALIFICATION_RETAINED_SOURCE_SHA: candidateSha,
        QUALIFICATION_RETAINED_CAPSULE_SHA256: digest(canonical(records)),
        QUALIFICATION_CANDIDATE_SHA: candidateSha,
      });
      const files = {
        [`candidate/${installerPath}`]: source,
        "build-inputs.json": JSON.stringify({
          version: 1,
          candidateSha,
          workflowSha,
          workerSourceSha256: "c".repeat(64),
          protectedBuildInputsSha256: "d".repeat(64),
        }),
        "retained-capsule.json": JSON.stringify({
          version: 1,
          sourceSha: candidateSha,
          candidateSha,
          sourceCapsuleSha256: process.env.QUALIFICATION_RETAINED_CAPSULE_SHA256,
          sourceFiles: records,
          candidateFiles: records,
          bakedInputsIdentical: true,
        }),
      };
      const manifest = {
        version: 1,
        candidateSha,
        workflowSha,
        files: Object.entries(files).map(([file, bytes]) => {
          fs.mkdirSync(path.dirname(path.join(temp, file)), { recursive: true });
          fs.writeFileSync(path.join(temp, file), bytes);
          return { path: file, bytes: Buffer.byteLength(bytes), sha256: digest(bytes) };
        }),
      };
      manifest.manifestSha256 = digest(canonical(manifest));
      fs.writeFileSync(path.join(temp, "manifest.json"), JSON.stringify(manifest));
      return callback(() => verifyManifest(temp, candidateSha, workflowSha), temp);
    } finally {
      fs.rmSync(temp, { recursive: true, force: true });
    }
  });
}

test("retained installer admission rejects the real legacy installer before deployment", () => {
  withInstallerBundle(legacyInstaller, (verify) => {
    assert.throws(verify, /lacks rust_smoke_script generator/);
  });
  const control = fs.readFileSync(
    path.join(root, "scripts/image-qualification-control.mjs"),
    "utf8",
  );
  const deploy = control.slice(
    control.indexOf("async function deploy()"),
    control.indexOf("async function arm()"),
  );
  const admission = deploy.indexOf("verifyManifest(");
  const deployment = deploy.indexOf("await preparePrivateCandidate(");
  assert.ok(admission >= 0 && deployment > admission);
  const admit = control.slice(
    control.indexOf('if (command === "admit")'),
    control.indexOf('if (command === "manifest")'),
  );
  assert.match(admit, /verifyManifest\(/);
});

test("retained installer admission accepts the supported layout without executing source", () => {
  withInstallerBundle(retainedInstaller, (verify, temp) => {
    const sentinel = path.join(temp, "executed");
    withInstallerBundle(`touch '${sentinel}'\n${retainedInstaller}`, (verifySentinel) => {
      assert.doesNotThrow(verifySentinel);
      assert.equal(fs.existsSync(sentinel), false);
    });
    assert.doesNotThrow(verify);
  });
});

for (const name of ["node_pnpm", "go", "bun", "rust", "uv"]) {
  test(`retained installer admission requires the ${name} generator declaration`, () => {
    const declaration = `${name}_smoke_script() {`;
    assert.ok(retainedInstaller.includes(declaration));
    const changed = retainedInstaller.replace(declaration, `${name}_removed() {`);
    const incidental = `\n# ${declaration}\nprintf '%s\\n' '${declaration}'\n`;
    withInstallerBundle(changed + incidental, (verify) => {
      assert.throws(verify, new RegExp(`lacks ${name}_smoke_script generator`));
    });
  });
}

test("retained installer admission bounds source bytes and leaves mint mode unchanged", () => {
  withInstallerBundle(retainedInstaller + "#".repeat(256 * 1024), (verify) => {
    assert.throws(verify, /retained installer exceeds the source byte limit/);
  });
  withInstallerBundle(legacyInstaller, (verify) => {
    for (const name of names) delete process.env[name];
    process.env.QUALIFICATION_MODE = "mint";
    assert.doesNotThrow(verify);
  });
});

test("selection requires the actual AMI, region, promoted source, and exact revision", () => {
  const image = { id: "ami-11111111", revision: "revision-1" };
  const lease = {
    id: "cbx_test",
    cloudID: "i-11111111",
    provider: "aws",
    target: "linux",
    region: "us-east-1",
    serverType: "t3.small",
    image: { ...image, kind: "aws-ami", source: "promoted", region: "us-east-1" },
  };
  assert.equal(verifyRetainedSelection(lease, { image }, "us-east-1").exactPromotedRevision, true);
  for (const change of [
    { id: "ami-22222222" },
    { revision: undefined },
    { revision: "revision-other" },
    { source: "explicit" },
    { region: "us-west-2" },
  ]) {
    assert.throws(
      () =>
        verifyRetainedSelection(
          { ...lease, image: { ...lease.image, ...change } },
          { image },
          "us-east-1",
        ),
      /exact normally selected/,
    );
  }
});

test("retained relay allows only normal scoped leases and never image capture", async () => {
  const calls = [];
  const env = {
    EXECUTOR_TOKEN: "e".repeat(64),
    CANDIDATE_ADMIN_TOKEN: "a".repeat(64),
    CANDIDATE_SHARED_TOKEN: "s".repeat(64),
    AWS_REGION: "us-east-1",
    QUALIFICATION_MODE: "retained",
    QUALIFICATION_OWNER: "test@example.invalid",
    QUALIFICATION_ORG: "qualification",
    QUALIFICATION_EXPIRES_AT: new Date(Date.now() + 60_000).toISOString(),
    CANDIDATE: {
      fetch: async (request) => {
        calls.push(request);
        return Response.json({ ok: true });
      },
    },
  };
  const lease = {
    provider: "aws",
    target: "linux",
    class: "standard",
    serverType: "t3.small",
    awsRegion: "us-east-1",
    os: "ubuntu:24.04",
    architecture: "x86_64",
    desktop: true,
    browser: true,
    tailscale: false,
    capacity: { market: "on-demand" },
  };
  const send = (route, body) =>
    relay.fetch(
      new Request(`https://relay.invalid${route}`, {
        method: "POST",
        headers: {
          authorization: `Bearer ${env.EXECUTOR_TOKEN}`,
          "content-type": "application/json",
        },
        body: JSON.stringify(body),
      }),
      env,
    );
  assert.equal((await send("/v1/leases", lease)).status, 200);
  for (const changes of [
    { awsAMI: "ami-11111111" },
    { awsSnapshot: "snap-22222222" },
    { awsUseStockImage: true },
    { os: "ubuntu:26.04" },
    { architecture: "arm64" },
    { desktop: false },
  ]) {
    assert.notEqual((await send("/v1/leases", { ...lease, ...changes })).status, 200);
  }
  assert.notEqual((await send("/v1/images", {})).status, 200);
  assert.equal(calls.length, 1);
  for (const [method, route, body] of [
    ["PUT", "/v1/leases/cbx_test", lease],
    ["POST", "/v1/leases/cbx_test/cancel-create", { createAttempt: "fixture" }],
    ["PUT", "/v1/runs/run_test", { provider: "aws", target: "linux", class: "", serverType: "" }],
    ["GET", "/v1/runs/run_test"],
  ]) {
    assert.equal(
      (
        await relay.fetch(
          new Request(`https://relay.invalid${route}`, {
            method,
            headers: {
              authorization: `Bearer ${env.EXECUTOR_TOKEN}`,
              ...(body ? { "content-type": "application/json" } : {}),
            },
            ...(body ? { body: JSON.stringify(body) } : {}),
          }),
          env,
        )
      ).status,
      200,
    );
  }
  assert.equal((await send("/v1/runs", { provider: "aws", target: "linux" })).status, 404);
});

const executorFixture = String.raw`#!/usr/bin/env node
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
const args = process.argv.slice(2);
const statePath = process.env.FIXTURE_STATE;
const state = fs.existsSync(statePath) ? JSON.parse(fs.readFileSync(statePath)) : { events: [] };
const prior = { id: "ami-33333333", name: "prior", state: "available", revision: "prior", promotedAt: "before" };
const candidate = { id: "ami-11111111", revision: "candidate", promotedAt: "during" };
const current = (image) => ({ state: "present", imageId: image.id, revision: image.revision });
const emit = (value) => process.stdout.write(JSON.stringify(value) + "\n");
const event = (name) => { state.events.push(name); fs.writeFileSync(statePath, JSON.stringify(state)); };
if (path.basename(process.argv[1]) === "curl") {
  const url = new URL(args.at(-1));
  let status = 200;
  let body;
  if (url.pathname.startsWith("/qualification/shared/")) {
    event("shared-denied"); status = 403; body = { error: "forbidden" };
  } else if (url.pathname.endsWith("/promote-cas")) {
    assert.equal(state.phase, "restored");
    event("stale-cas");
    status = 409;
    body = { error: "image_promotion_precondition_failed", expected: current(candidate), current: current(prior) };
  } else if (url.pathname.endsWith("/promote")) {
    const input = JSON.parse(fs.readFileSync(args[args.indexOf("--data-binary") + 1].slice(1)));
    assert.equal(input.fastSnapshotRestore, true);
    event("fsr-denied"); status = 403; body = { error: "denied" };
  } else if (url.pathname.startsWith("/v1/leases/")) {
    event("selection");
    body = { lease: {
      id: "cbx_test", cloudID: "i-11111111", provider: "aws", target: "linux", serverType: "t3.small",
      region: "us-east-1", image: { ...candidate, kind: "aws-ami", region: "us-east-1",
        source: process.env.FIXTURE_FAILURE === "selection" ? "explicit" : "promoted" },
    } };
  } else {
    assert.equal(url.searchParams.get("os"), "ubuntu:24.04");
    body = { image: url.pathname.endsWith(prior.id)
      ? (state.phase ? prior : { id: prior.id })
      : (state.phase === "restored" ? { id: candidate.id, state: "available" } : candidate) };
  }
  const output = args.indexOf("-o");
  if (output !== -1) fs.writeFileSync(args[output + 1], JSON.stringify(body));
  else process.stdout.write(JSON.stringify(body));
  if (args.includes("-w")) process.stdout.write(String(status));
} else if (args[0] === "image") {
  if (args.includes("--restore-receipt")) {
    const receipt = JSON.parse(fs.readFileSync(args[args.indexOf("--restore-receipt") + 1]));
    assert.equal(receipt.image.revision, candidate.revision);
    state.phase = "restored"; event("restore");
    emit({ image: prior, previous: current(candidate) });
  } else if (args.includes("capture")) {
    state.phase = "promoted"; event("promote");
    emit({ image: candidate, previous: { ...current(prior), aliases: [
      { alias: "regional", state: "present", image: prior },
    ] } });
  } else {
    state.phase = "seeded"; event("seed"); emit(prior);
  }
} else if (args[0] === "warmup") {
  assert.equal(process.env.CRABBOX_AWS_AMI, undefined);
  assert.equal(process.env.CRABBOX_AWS_ROOT_GB, "400");
  assert.ok(args.includes("--desktop") && args.includes("--browser"));
  event("warmup");
  if (process.env.FIXTURE_FAILURE === "lost-launch-response") process.exit(7);
  emit({ leaseId: "cbx_test" });
} else if (args[0] === "run") {
  if (args.includes("--script")) {
    event("readiness");
    if (process.env.FIXTURE_FAILURE === "readiness") process.exit(9);
    console.log("readiness-ok");
  } else {
    assert.ok(args.at(-1).endsWith("echo devtools-smoke-ok"));
    assert.ok(args.at(-1).includes("rust-archive-probe"));
    event("full-smoke"); console.log("devtools-smoke-ok");
  }
} else if (args[0] === "stop") {
  assert.equal(state.phase, "restored");
  event("release");
} else throw new Error("unexpected fixture command");
`;

test("retained executor refuses an exhausted work window before any CLI or resource work", () => {
  const env = {
    ...process.env,
    QUALIFICATION_MODE: "retained",
    QUALIFICATION_BOUNDED_CHILD: "0",
    QUALIFICATION_EXPIRES_AT: new Date(Date.now() + 7 * 60_000).toISOString(),
    QUALIFICATION_RELAY_URL: "https://relay.invalid",
    QUALIFICATION_EXECUTOR_TOKEN: "e".repeat(64),
    QUALIFICATION_BASE_AMI_ID: "ami-33333333",
    QUALIFICATION_AWS_REGION: "us-east-1",
    QUALIFICATION_ARTIFACT_DIR: "/nonexistent-qualification-fixture",
    QUALIFICATION_PROOF_DIR: "/nonexistent-qualification-fixture",
  };
  const result = spawnSync("bash", [path.join(root, "scripts/image-qualification-execute.sh")], {
    env,
    encoding: "utf8",
    timeout: 5_000,
  });
  assert.equal(result.error, undefined);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, "");
  assert.equal(result.stderr, "");
});

for (const failure of [
  "",
  "selection",
  "readiness",
  "lost-launch-response",
  "receipt-write",
  "receipt-truncated",
  "receipt-invalid",
  "receipt-alias",
]) {
  test(`retained executor closes the exact transaction (${failure || "full smoke"})`, () => {
    const temp = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-retained-executor-"));
    try {
      const artifact = path.join(temp, "artifact");
      const scripts = path.join(artifact, "candidate/scripts");
      const tools = path.join(temp, "tools");
      fs.mkdirSync(scripts, { recursive: true });
      fs.mkdirSync(path.join(artifact, "bin"));
      fs.mkdirSync(tools);
      const fixture = path.join(temp, "fixture.mjs");
      fs.writeFileSync(fixture, executorFixture, { mode: 0o700 });
      fs.symlinkSync(fixture, path.join(artifact, "bin/crabbox"));
      fs.symlinkSync(fixture, path.join(tools, "curl"));
      fs.writeFileSync(
        path.join(tools, "tee"),
        `#!/usr/bin/env node
const fs = require("node:fs");
const input = fs.readFileSync(0);
const failure = process.env.FIXTURE_FAILURE;
const target = process.argv[2];
const promotion = target.endsWith("/promotion-receipt.json.partial");
fs.writeFileSync(target, promotion && failure === "receipt-truncated" ? '{"image":'
  : promotion && failure === "receipt-invalid" ? '{"image":{"id":"ami-11111111","revision":"candidate"},"previous":{"state":"absent"}}'
  : promotion && failure === "receipt-alias" ? '{"image":{"id":"ami-11111111","revision":"candidate"},"previous":{"state":"present","imageId":"ami-33333333","revision":"prior","aliases":[{"alias":"regional","state":"present","image":{"id":"ami-33333333","revision":"prior"}}]}}'
  : input);
process.stdout.write(input);
if (promotion && failure === "receipt-write") process.exit(28);
`,
        { mode: 0o700 },
      );
      fs.writeFileSync(path.join(tools, "sleep"), "#!/bin/sh\nexit 0\n", { mode: 0o700 });
      fs.writeFileSync(path.join(tools, "sha256sum"), '#!/bin/sh\nexec shasum -a 256 "$@"\n', {
        mode: 0o700,
      });
      fs.writeFileSync(path.join(scripts, "linux-readiness.generated.sh"), "#!/bin/sh\nexit 0\n");
      fs.writeFileSync(
        path.join(scripts, "devtools-image-smoke-linux.sh"),
        "echo devtools-smoke-ok\n",
      );
      fs.writeFileSync(
        path.join(scripts, "install-linux-developer-tools.sh"),
        ["node_pnpm", "go", "bun", "rust", "uv"]
          .map((name) => `${name}_smoke_script() { echo 'echo ${name}-archive-probe'; }\n`)
          .join(""),
      );
      const env = {
        ...process.env,
        PATH: `${tools}:${process.env.PATH}`,
        FIXTURE_STATE: path.join(temp, "state.json"),
        FIXTURE_FAILURE: failure,
        QUALIFICATION_MODE: "retained",
        QUALIFICATION_BOUNDED_CHILD: "1",
        QUALIFICATION_RELAY_URL: "https://relay.invalid",
        QUALIFICATION_EXECUTOR_TOKEN: "e".repeat(64),
        QUALIFICATION_BASE_AMI_ID: "ami-33333333",
        QUALIFICATION_RETAINED_IMAGE_ID: "ami-11111111",
        QUALIFICATION_AWS_REGION: "us-east-1",
        QUALIFICATION_ARTIFACT_DIR: artifact,
        QUALIFICATION_PROOF_DIR: path.join(temp, "proof"),
        RUNNER_TEMP: temp,
        CRABBOX_AWS_AMI: "ami-must-be-cleared",
      };
      for (const key of [
        "AWS_ACCESS_KEY_ID",
        "AWS_SECRET_ACCESS_KEY",
        "AWS_SESSION_TOKEN",
        "CLOUDFLARE_API_TOKEN",
        "QUALIFICATION_CONTROLLER_TOKEN",
        "QUALIFICATION_ADMIN_TOKEN",
        "QUALIFICATION_SHARED_TOKEN",
      ])
        delete env[key];
      const result = spawnSync(
        "bash",
        [path.join(root, "scripts/image-qualification-execute.sh")],
        {
          env,
          encoding: "utf8",
          timeout: 15_000,
        },
      );
      const events = JSON.parse(fs.readFileSync(env.FIXTURE_STATE)).events;
      assert.equal(
        result.error,
        undefined,
        `${result.stderr}\nfixture stages: ${events.join(", ")}`,
      );
      const receiptFailure = failure.startsWith("receipt-");
      assert.equal(
        events.filter((value) => value === "warmup").length,
        receiptFailure ? 0 : 1,
        result.stderr,
      );
      assert.equal(events.includes("restore"), !receiptFailure, result.stderr);
      if (failure) {
        assert.notEqual(result.status, 0, result.stderr);
        assert.ok(!events.includes("full-smoke"));
        assert.equal(
          events.includes("release"),
          !receiptFailure && failure !== "lost-launch-response",
        );
        if (receiptFailure) {
          assert.ok(events.includes("promote"));
          assert.ok(!events.includes("stale-cas"));
          assert.match(
            result.stderr,
            /qualification execution failed before successful full-smoke injection/,
          );
        }
      } else {
        assert.equal(result.signal, "SIGKILL", result.stderr);
        assert.deepEqual(events.slice(events.indexOf("seed")), [
          "seed",
          "promote",
          "warmup",
          "selection",
          "readiness",
          "full-smoke",
          "restore",
          "stale-cas",
          "release",
        ]);
        const proof = JSON.parse(
          fs.readFileSync(path.join(env.QUALIFICATION_PROOF_DIR, "execution-state.json")),
        );
        assert.equal(proof.launchCount, 1);
        assert.equal(proof.normalSelection, true);
        assert.equal(proof.exactPromotedRevision, true);
      }
      assert.ok(
        !fs.readdirSync(temp).some((name) => name.startsWith("image-qualification-execute.")),
      );
    } finally {
      fs.rmSync(temp, { recursive: true, force: true });
    }
  });
}

test("retained adapter ignores readiness and injects only after the complete smoke", () => {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-retained-adapter-"));
  try {
    const cli = path.join(temp, "cli");
    fs.writeFileSync(cli, '#!/usr/bin/env bash\nexit "${FIXTURE_EXIT:-0}"\n', { mode: 0o700 });
    const env = {
      ...process.env,
      QUALIFICATION_MODE: "retained",
      QUALIFICATION_REAL_CRABBOX: cli,
      QUALIFICATION_ADAPTER_STATE: path.join(temp, "state"),
    };
    fs.mkdirSync(env.QUALIFICATION_ADAPTER_STATE);
    fs.writeFileSync(
      path.join(env.QUALIFICATION_ADAPTER_STATE, "promotion-receipt.json"),
      JSON.stringify({
        image: { id: "ami-11111111", revision: "candidate" },
        previous: { state: "absent", aliases: [{ alias: "regional", state: "absent" }] },
      }),
    );
    const invoke = (args, extra = {}) =>
      spawnSync(path.join(root, "scripts/image-qualification-crabbox-adapter.sh"), args, {
        env: { ...env, ...extra },
        encoding: "utf8",
      });
    assert.equal(invoke(["warmup"]).status, 0);
    for (const args of [
      ["run"],
      ["run", "--script", "readiness.sh", "--", "--verify", "linux-builder"],
      ["run", "--shell", "--", "echo readiness-ok"],
      ["run", "--shell", "--", "echo devtools-smoke-ok\necho not-finished"],
    ])
      assert.equal(invoke(args).status, 0);
    assert.equal(
      invoke(["run", "--shell", "--", "echo devtools-smoke-ok"], { FIXTURE_EXIT: "7" }).status,
      7,
    );
    assert.equal(invoke(["run", "--shell", "--", "set -e\necho devtools-smoke-ok\n"]).status, 86);
    assert.equal(invoke(["run", "--shell", "--", "echo devtools-smoke-ok"]).status, 0);
  } finally {
    fs.rmSync(temp, { recursive: true, force: true });
  }
});

test("retained adapter refuses warmup without a usable receipt and never records a launch", () => {
  const temp = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-retained-receipt-"));
  try {
    const result = spawnSync(
      path.join(root, "scripts/image-qualification-crabbox-adapter.sh"),
      ["warmup"],
      {
        env: {
          ...process.env,
          QUALIFICATION_MODE: "retained",
          QUALIFICATION_REAL_CRABBOX: "/nonexistent-qualification-cli",
          QUALIFICATION_ADAPTER_STATE: temp,
        },
        encoding: "utf8",
      },
    );
    assert.equal(result.status, 1);
    assert.match(result.stderr, /qualification receipt is missing or invalid/);
    assert.equal(fs.existsSync(path.join(temp, "launch-count")), false);
    assert.equal(fs.existsSync(path.join(temp, "rollback-receipt.json")), false);
  } finally {
    fs.rmSync(temp, { recursive: true, force: true });
  }
});
