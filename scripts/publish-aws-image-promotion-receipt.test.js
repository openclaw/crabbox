import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { chmod, mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { promisify } from "node:util";

import { canonicalJSON } from "./aws-image-candidate.mjs";

const execFileAsync = promisify(execFile);
const repoRoot = path.resolve(import.meta.dirname, "..");
const publisher = path.join(repoRoot, "scripts", "publish-aws-image-promotion-receipt.sh");
const digest = `sha256:${"a".repeat(64)}`;

function completedAttempt() {
  return {
    schema: "crabbox-image-promotion-attempt/v1",
    idempotencyKey: "run-200-attempt-1",
    inputDigest: "c".repeat(64),
    operation: "promote",
    phase: "completed",
    version: 3,
    claimVersion: 1,
    outcome: "promoted_verified",
    expected: { state: "absent" },
    before: { state: "absent" },
    beforeBinding: { state: "absent" },
    after: { state: "present", imageId: "ami-123abc", revision: "revision-1" },
    desired: {
      state: "present",
      imageId: "ami-123abc",
      accountId: "123456789012",
      snapshotIds: ["snap-123abc"],
    },
    evidence: {
      schema: "crabbox-image-promotion-evidence/v1",
      qualification: {
        reference: `ghcr.io/example-org/qualification@${digest}`,
        digest,
        receiptDigest: digest,
        inputDigest: digest,
        signatureDigest: digest,
      },
      candidate: {
        reference: `ghcr.io/example-org/candidate@${digest}`,
        digest,
        recordDigest: digest,
      },
      source: { repository: "example-org/crabbox", commit: "b".repeat(40) },
      workflow: {
        identity:
          "example-org/crabbox/.github/workflows/devtools-image-qualify.yml@refs/heads/main",
        runId: "100",
        runAttempt: "1",
      },
      scope: {
        provider: "aws",
        target: "linux",
        region: "eu-west-1",
        instanceType: "c7i.4xlarge",
        architecture: "x86_64",
        os: "ubuntu:26.04",
        profile: "default",
        recipeDigest: digest,
      },
      desired: {
        imageId: "ami-123abc",
        accountId: "123456789012",
        snapshotIds: ["snap-123abc"],
      },
    },
    workflow: { runId: "200", runAttempt: "1" },
    mutation: { status: "succeeded", applied: true },
    verification: {
      status: "passed",
      codes: [],
      image: {
        id: "ami-123abc",
        provider: "aws",
        source: "promoted",
        region: "eu-west-1",
        revision: "revision-1",
      },
    },
    cleanup: { status: "passed" },
    createdAt: "2026-08-21T00:00:00.000Z",
    updatedAt: "2026-08-21T00:10:00.000Z",
  };
}

async function setup(existing = false) {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-promotion-publish-"));
  const bin = path.join(root, "bin");
  const log = path.join(root, "commands.log");
  const state = path.join(root, "manifest.json");
  const attempt = path.join(root, "attempt.json");
  await mkdir(bin);
  await writeFile(attempt, `${canonicalJSON(completedAttempt())}\n`);
  await writeFile(
    path.join(bin, "oras"),
    `#!/usr/bin/env bash
set -euo pipefail
printf 'oras %s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
if [[ "\${1:-}" == manifest && "\${2:-}" == fetch ]]; then
  if [[ "\${3:-}" != *@sha256:* ]]; then
    [[ "\${CRABBOX_FAKE_EXISTING:-0}" == 1 ]] && exit 0
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
elif [[ "\${1:-}" == push ]]; then
  receipt_arg="\${5}"
  receipt="\${receipt_arg%:application/vnd.crabbox.aws-image-promotion-receipt.v1+json}"
  node -e '
const crypto = require("crypto");
const fs = require("fs");
const [receiptFile, state] = process.argv.slice(1);
const receipt = fs.readFileSync(receiptFile);
const digest = (bytes) => "sha256:" + crypto.createHash("sha256").update(bytes).digest("hex");
const raw = Buffer.from(JSON.stringify({
  schemaVersion: 2,
  mediaType: "application/vnd.oci.image.manifest.v1+json",
  artifactType: "application/vnd.crabbox.aws-image-promotion-receipt.v1",
  config: {
    mediaType: "application/vnd.oci.empty.v1+json",
    digest: "sha256:" + "0".repeat(64),
    size: 2
  },
  layers: [{
    mediaType: "application/vnd.crabbox.aws-image-promotion-receipt.v1+json",
    digest: digest(receipt),
    size: receipt.length
  }]
}));
fs.writeFileSync(state, raw);
process.stdout.write(JSON.stringify({ digest: digest(raw) }) + "\\n");
' "$receipt" "\${CRABBOX_FAKE_STATE:?}"
fi
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
  return { root, bin, log, state, attempt, existing };
}

async function run(value) {
  try {
    const result = await execFileAsync(
      "bash",
      [
        publisher,
        "--attempt",
        value.attempt,
        "--repository",
        "ghcr.io/example-org/crabbox-aws-image-promotions",
        "--certificate-identity",
        "https://github.com/example-org/crabbox/.github/workflows/devtools-image-promote.yml@refs/heads/main",
      ],
      {
        cwd: repoRoot,
        env: {
          ...process.env,
          PATH: `${value.bin}${path.delimiter}${process.env.PATH}`,
          GOCACHE: path.join(value.root, "go-cache"),
          CRABBOX_FAKE_LOG: value.log,
          CRABBOX_FAKE_STATE: value.state,
          CRABBOX_FAKE_EXISTING: value.existing ? "1" : "0",
        },
      },
    );
    return { code: 0, ...result };
  } catch (error) {
    return { code: error.code, stdout: error.stdout ?? "", stderr: error.stderr ?? "" };
  }
}

test("publisher signs one typed immutable receipt without replaying promotion", async () => {
  const value = await setup();
  const result = await run(value);
  assert.equal(result.code, 0, result.stderr);
  const publication = JSON.parse(result.stdout);
  assert.equal(publication.schema, "crabbox-aws-image-promotion-receipt-publication/v1");
  const commands = (await readFile(value.log, "utf8")).trim().split("\n");
  assert.match(
    commands[1],
    /--artifact-type application\/vnd\.crabbox\.aws-image-promotion-receipt\.v1/,
  );
  assert.match(
    commands[1],
    /promotion-receipt\.json:application\/vnd\.crabbox\.aws-image-promotion-receipt\.v1\+json/,
  );
  assert.match(commands[3], /^cosign sign --new-bundle-format --registry-referrers-mode oci-1-1/);
  assert.match(commands[4], /^cosign verify --new-bundle-format --experimental-oci11/);
  assert.doesNotMatch(commands.join("\n"), /authorization|aws-access-key/i);
});

test("publisher refuses to overwrite an immutable receipt tag", async () => {
  const value = await setup(true);
  const result = await run(value);
  assert.equal(result.code, 1);
  assert.match(result.stderr, /refusing to overwrite existing promotion receipt tag/);
  assert.equal((await readFile(value.log, "utf8")).trim().split("\n").length, 1);
});
