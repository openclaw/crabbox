import assert from "node:assert/strict";
import { execFile } from "node:child_process";
import { mkdtemp, writeFile } from "node:fs/promises";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const script = path.resolve(import.meta.dirname, "aws-image-promotion-client.mjs");
const digest = `sha256:${"a".repeat(64)}`;

function evidence() {
  return {
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
      identity: "example-org/crabbox/.github/workflows/devtools-image-qualify.yml@refs/heads/main",
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
      imageId: "ami-next",
      accountId: "123456789012",
      snapshotIds: ["snap-next"],
    },
  };
}

function attempt(phase, overrides = {}) {
  return {
    schema: "crabbox-image-promotion-attempt/v1",
    idempotencyKey: "promotion-200-promote",
    inputDigest: "c".repeat(64),
    operation: "promote",
    phase,
    version: 2,
    claimVersion: 1,
    expected: { state: "absent" },
    before: { state: "absent" },
    beforeBinding: { state: "absent" },
    desired: {
      state: "present",
      imageId: "ami-next",
      accountId: "123456789012",
      snapshotIds: ["snap-next"],
    },
    evidence: evidence(),
    workflow: { runId: "200", runAttempt: "1" },
    mutation: { status: "pending", applied: false },
    createdAt: "2026-08-21T00:00:00.000Z",
    updatedAt: "2026-08-21T00:00:01.000Z",
    ...overrides,
  };
}

async function fixtureServer(handler) {
  const server = http.createServer(handler);
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const address = server.address();
  return {
    endpoint: `http://127.0.0.1:${address.port}`,
    close: () => new Promise((resolve) => server.close(resolve)),
  };
}

test("mutate polls a pending claim and returns only awaiting verification", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-promotion-client-"));
  const evidenceFile = path.join(root, "evidence.json");
  await writeFile(evidenceFile, JSON.stringify(evidence()));
  let requests = 0;
  const fixture = await fixtureServer((request, response) => {
    requests += 1;
    assert.equal(request.headers.authorization, ["Bearer", "test-promotion-token"].join(" "));
    response.setHeader("content-type", "application/json");
    if (request.url === "/v1/image-promotions") {
      response.statusCode = 202;
      response.end(JSON.stringify({ attempt: attempt("mutating") }));
      return;
    }
    assert.equal(request.url, "/v1/image-promotions/promotion-200-promote/recover");
    response.end(
      JSON.stringify({
        image: {
          id: "ami-next",
          state: "available",
          provider: "aws",
          target: "linux",
          region: "eu-west-1",
          revision: "revision-next",
        },
        attempt: attempt("awaiting_verification", {
          after: { state: "present", imageId: "ami-next", revision: "revision-next" },
          mutation: { status: "succeeded", applied: true },
        }),
      }),
    );
  });
  try {
    const result = await execFileAsync(
      process.execPath,
      [
        script,
        "mutate",
        "--evidence",
        evidenceFile,
        "--qualification-ref",
        evidence().qualification.reference,
        "--expected-current-image",
        "none",
        "--expected-current-revision",
        "",
        "--idempotency-key",
        "promotion-200-promote",
        "--workflow-run-id",
        "200",
        "--workflow-run-attempt",
        "2",
        "--operation",
        "promote",
      ],
      {
        env: {
          ...process.env,
          CRABBOX_COORDINATOR: fixture.endpoint,
          CRABBOX_COORDINATOR_PROMOTION_TOKEN: "test-promotion-token",
          CRABBOX_PROMOTION_CLIENT_POLL_MS: "0",
        },
      },
    );
    assert.equal(JSON.parse(result.stdout).attempt.phase, "awaiting_verification");
    assert.equal(requests, 2);
    assert.doesNotMatch(result.stderr, /test-promotion-token/);
  } finally {
    await fixture.close();
  }
});

test("rejects non-allowlisted coordinator response fields", async () => {
  for (const payload of [
    { attempt: attempt("completed"), secret: "not-allowed" },
    {
      attempt: attempt("completed", {
        mutation: { status: "succeeded", applied: true, authorization: "Bearer secret" },
      }),
    },
  ]) {
    const fixture = await fixtureServer((_request, response) => {
      response.setHeader("content-type", "application/json");
      response.end(JSON.stringify(payload));
    });
    try {
      await assert.rejects(
        execFileAsync(
          process.execPath,
          [script, "recover", "--idempotency-key", "promotion-200-promote"],
          {
            env: {
              ...process.env,
              CRABBOX_COORDINATOR: fixture.endpoint,
              CRABBOX_COORDINATOR_PROMOTION_TOKEN: "test-promotion-token",
            },
          },
        ),
        /invalid coordinator response|invalid promotion mutation/,
      );
    } finally {
      await fixture.close();
    }
  }
});

test("default-state is read-only and strictly projected", async () => {
  const fixture = await fixtureServer((request, response) => {
    assert.match(request.url, /^\/v1\/image-promotions\/default-state\?/);
    response.setHeader("content-type", "application/json");
    response.end(
      JSON.stringify({
        provider: "azure",
        scope: {
          provider: "azure",
          target: "linux",
          os: "ubuntu:26.04",
          region: "westeurope",
          architecture: "amd64",
          serverType: "Standard_D4s_v5",
        },
        state: { state: "present", imageId: "snapshot-a", revision: "revision-a" },
      }),
    );
  });
  try {
    const result = await execFileAsync(
      process.execPath,
      [
        script,
        "default-state",
        "--provider",
        "azure",
        "--target",
        "linux",
        "--os",
        "ubuntu:26.04",
        "--region",
        "westeurope",
        "--architecture",
        "amd64",
        "--server-type",
        "Standard_D4s_v5",
      ],
      {
        env: {
          ...process.env,
          CRABBOX_COORDINATOR: fixture.endpoint,
          CRABBOX_COORDINATOR_PROMOTION_TOKEN: "test-promotion-token",
        },
      },
    );
    assert.deepEqual(JSON.parse(result.stdout).state, {
      state: "present",
      imageId: "snapshot-a",
      revision: "revision-a",
    });
  } finally {
    await fixture.close();
  }
});
