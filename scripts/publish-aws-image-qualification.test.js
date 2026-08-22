import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { promisify } from "node:util";
import { canonicalJSON, digestBytes, digestJSON } from "./aws-image-candidate.mjs";

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(import.meta.dirname, "..");
const publisher = path.join(repoRoot, "scripts", "publish-aws-image-qualification.sh");
const inputFixture = path.join(
  repoRoot,
  "scripts",
  "fixtures",
  "aws-image-qualification-input.json",
);
const digest = (digit) => `sha256:${digit.repeat(64)}`;

async function setup(existing = false, lookupError = false) {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-qualification-publish-"));
  const bin = path.join(root, "bin");
  const log = path.join(root, "commands.log");
  const state = path.join(root, "manifest.json");
  const receipt = path.join(root, "qualification.json");
  const input = path.join(root, "qualification-input.json");
  const inputBytes = await readFile(inputFixture);
  const inputRecord = JSON.parse(inputBytes);
  const candidateEvidence = inputRecord.evidence.find((item) => item.name === "candidate.json");
  const identity = {
    schema: "crabbox-ready-pool-identity/v1",
    profile: inputRecord.readiness.profile,
    recipeDigest: inputRecord.readiness.recipeDigest,
    inventoryDigest: digest("4"),
    imageID: "ami-123abc",
    architecture: "amd64",
    seedDigest: digest("5"),
    cacheABIDigest: digest("6"),
  };
  await mkdir(bin);
  await writeFile(input, inputBytes);
  await writeFile(
    receipt,
    `${canonicalJSON({
      schema: "crabbox-aws-image-qualification/v1",
      createdAt: "2026-08-21T00:00:00.000Z",
      candidate: {
        artifactDigest: inputRecord.artifact.digest,
        candidateRecordDigest: candidateEvidence.digest,
        qualificationInputDigest: digestBytes(inputBytes),
      },
      target: {
        amiId: inputRecord.target.amiId,
        region: inputRecord.target.region,
        instanceType: inputRecord.target.instanceType,
        architecture: inputRecord.target.architecture,
        osSelector: "ubuntu:26.04",
        market: "on-demand",
        profile: inputRecord.readiness.profile,
        recipeDigest: inputRecord.readiness.recipeDigest,
      },
      pool: { identity, identityDigest: digestJSON(identity) },
      gates: {
        negative: ["ami", "architecture", "recipe", "type"].map((dimension) => ({
          dimension,
          status: "passed",
        })),
        positive: { status: "passed" },
        osSelector: { status: "passed" },
        cleanOverlay: {
          status: "passed",
          mode: "overlay",
          fallback: false,
          trackedTransfer: { files: 0, bytes: 0 },
        },
        dirtyOverlay: {
          status: "passed",
          mode: "overlay",
          fallback: false,
          trackedTransfer: { files: 1, bytes: 40 },
          fixtureDigest: digest("8"),
        },
      },
      cache: {
        advertised: false,
        status: "skipped",
        reason: "provider_capability_not_advertised",
      },
      cleanup: { status: "passed" },
      workflow: {
        identity: "example-org/crabbox/.github/workflows/devtools-image-qualify.yml@refs/heads/main",
        runId: "123",
        runAttempt: "1",
      },
    })}\n`,
  );
  await writeFile(
    path.join(bin, "oras"),
    `#!/usr/bin/env bash
set -euo pipefail
printf 'oras %s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "\${1:-}" in
  manifest)
    if [[ "\${3:-}" != *@sha256:* ]]; then
      if [[ "\${CRABBOX_FAKE_EXISTING:-0}" == 1 ]]; then
        exit 0
      fi
      if [[ "\${CRABBOX_FAKE_LOOKUP_ERROR:-0}" == 1 ]]; then
        printf 'unauthorized\\n' >&2
        exit 1
      fi
      printf 'manifest unknown\\n' >&2
      exit 1
    fi
    output=""
    for ((i=1; i<=$#; i++)); do
      [[ "\${!i}" == --output ]] || continue
      j=$((i + 1))
      output="\${!j}"
    done
    cp "\${CRABBOX_FAKE_STATE:?}" "$output"
    printf '{}\\n'
    ;;
  push)
    receipt_arg="\${5}"
    receipt="\${receipt_arg%:application/vnd.crabbox.aws-image-qualification.v1+json}"
    input_arg="\${6}"
    input="\${input_arg%:application/vnd.crabbox.aws-image-qualification-input.v1+json}"
    node -e '
const crypto = require("crypto");
const fs = require("fs");
const [receiptFile, inputFile, state] = process.argv.slice(1);
const receipt = fs.readFileSync(receiptFile);
const input = fs.readFileSync(inputFile);
const digest = (bytes) => "sha256:" + crypto.createHash("sha256").update(bytes).digest("hex");
const raw = Buffer.from(JSON.stringify({
  schemaVersion: 2,
  mediaType: "application/vnd.oci.image.manifest.v1+json",
  artifactType: "application/vnd.crabbox.aws-image-qualification.v1",
  config: {
    mediaType: "application/vnd.oci.empty.v1+json",
    digest: "sha256:" + "0".repeat(64),
    size: 2,
  },
  layers: [{
    mediaType: "application/vnd.crabbox.aws-image-qualification.v1+json",
    digest: digest(receipt),
    size: receipt.length,
  }, {
    mediaType: "application/vnd.crabbox.aws-image-qualification-input.v1+json",
    digest: digest(input),
    size: input.length,
  }],
}));
fs.writeFileSync(state, raw);
process.stdout.write(JSON.stringify({ digest: digest(raw) }) + "\\n");
' "$receipt" "$input" "\${CRABBOX_FAKE_STATE:?}"
    ;;
esac
`,
  );
  await writeFile(
    path.join(bin, "cosign"),
    `#!/usr/bin/env bash
set -euo pipefail
[[ "\${COSIGN_EXPERIMENTAL:-}" == 1 ]]
printf 'cosign %s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
`,
  );
  await chmod(path.join(bin, "oras"), 0o755);
  await chmod(path.join(bin, "cosign"), 0o755);
  return { root, bin, log, state, receipt, input, existing, lookupError };
}

async function rewriteInput(value, mutate, bindReceipt) {
  const record = JSON.parse(await readFile(value.input, "utf8"));
  mutate(record);
  const bytes = Buffer.from(`${canonicalJSON(record)}\n`);
  await writeFile(value.input, bytes);
  if (bindReceipt) {
    const receipt = JSON.parse(await readFile(value.receipt, "utf8"));
    receipt.candidate.qualificationInputDigest = digestBytes(bytes);
    await writeFile(value.receipt, `${canonicalJSON(receipt)}\n`);
  }
}

async function run(value) {
  try {
    const result = await execFileAsync(
      "bash",
      [
        publisher,
        "--receipt", value.receipt,
        "--qualification-input", value.input,
        "--repository", "ghcr.io/example-org/crabbox-aws-image-qualifications",
        "--certificate-identity",
        "https://github.com/example-org/crabbox/.github/workflows/devtools-image-qualify.yml@refs/heads/main",
      ],
      {
        cwd: repoRoot,
        env: {
          ...process.env,
          PATH: `${value.bin}${path.delimiter}${process.env.PATH}`,
          CRABBOX_FAKE_LOG: value.log,
          CRABBOX_FAKE_STATE: value.state,
          CRABBOX_FAKE_EXISTING: value.existing ? "1" : "0",
          CRABBOX_FAKE_LOOKUP_ERROR: value.lookupError ? "1" : "0",
        },
      },
    );
    return { code: 0, ...result };
  } catch (error) {
    return { code: error.code, stdout: error.stdout ?? "", stderr: error.stderr ?? "" };
  }
}

test("publisher pushes one typed receipt then signs and verifies the immutable digest", async () => {
  const value = await setup();
  const result = await run(value);
  assert.equal(result.code, 0, result.stderr);
  const publication = JSON.parse(result.stdout);
  assert.equal(publication.schema, "crabbox-aws-image-qualification-publication/v1");
  assert.match(publication.tag, /^sha256-[0-9a-f]{64}$/);
  const commands = (await readFile(value.log, "utf8")).trim().split("\n");
  assert.match(commands[1], /--artifact-type application\/vnd\.crabbox\.aws-image-qualification\.v1/);
  assert.match(
    commands[1],
    / qualification\.json:application\/vnd\.crabbox\.aws-image-qualification\.v1\+json /,
  );
  assert.match(
    commands[1],
    / qualification-input\.json:application\/vnd\.crabbox\.aws-image-qualification-input\.v1\+json /,
  );
  assert.doesNotMatch(commands[1], /crabbox-aws-qualification-publish\./);
  assert.doesNotMatch(commands[1], /--disable-path-validation/);
  assert.match(commands[2], /^oras manifest fetch .*@sha256:/);
  assert.match(commands[3], /^cosign sign --new-bundle-format --registry-referrers-mode oci-1-1/);
  assert.match(commands[4], /^cosign verify --new-bundle-format --experimental-oci11/);
});

test("publisher rejects qualification input bytes that do not match the receipt", async () => {
  const value = await setup();
  await rewriteInput(value, (record) => {
    record.source.repository = "example-org/other-repo";
  }, false);
  const result = await run(value);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /qualification input digest does not match receipt/);
  await assert.rejects(readFile(value.log), { code: "ENOENT" });
});

test("publisher rejects schema-invalid qualification input before registry or signing access", async () => {
  const value = await setup();
  await rewriteInput(value, (record) => {
    record.source.sshHost = "host.example.invalid";
  }, true);
  const result = await run(value);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /validate document/);
  await assert.rejects(readFile(value.log), { code: "ENOENT" });
});

test("publisher rejects privacy-unsafe qualification input before registry or signing access", async () => {
  const value = await setup();
  await rewriteInput(value, (record) => {
    record.source.repository = "127.0.0.1/repository";
  }, true);
  const result = await run(value);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /qualification input contains private machine data/);
  await assert.rejects(readFile(value.log), { code: "ENOENT" });
});

test("publisher refuses to overwrite an existing receipt tag", async () => {
  const value = await setup(true);
  const result = await run(value);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /refusing to overwrite existing qualification tag/);
  assert.equal((await readFile(value.log, "utf8")).trim().split("\n").length, 1);
});

test("publisher fails closed when tag absence cannot be proved", async () => {
  const value = await setup(false, true);
  const result = await run(value);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /could not prove qualification tag absence/);
  assert.equal((await readFile(value.log, "utf8")).trim().split("\n").length, 1);
});
