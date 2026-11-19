import assert from "node:assert/strict";
import crypto from "node:crypto";
import fs from "node:fs";
import http from "node:http";
import os from "node:os";
import path from "node:path";
import { spawn } from "node:child_process";
import test from "node:test";
import {
  prepareFirstWalkWithEvidence,
  preparationReason,
} from "./image-qualification-first-walk-http.mjs";

const token = "PRIVATE_EXECUTOR_TOKEN_DO_NOT_EXPORT";
const claim = "PRIVATE_CLAIM_DO_NOT_EXPORT";
const intent = {
  action: "prepare",
  claim,
  leaseID: "cbx_fixture",
  readinessReceiptSha256: "a".repeat(64),
};
const requestID = "PRIVATE_CORRELATION_DO_NOT_EXPORT";
const privateText =
  "PRIVATE_UNRELATED_BODY_DO_NOT_EXPORT https://private.invalid/capability vol-12345678 192.0.2.1";
async function fixture(handler, work) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "first-walk-http-"));
  let requests = 0;
  const server = http.createServer(async (req, res) => {
    requests++;
    assert.equal(req.method, "POST");
    assert.equal(req.url, "/v1/checkpoints/chk_fixture/first-walk");
    assert.equal(req.headers.authorization, `Bearer ${token}`);
    let body = "";
    for await (const part of req) body += part;
    assert.deepEqual(JSON.parse(body), intent, "same immutable intent on every retry");
    await handler(req, res, requests);
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const options = {
    relay: `http://127.0.0.1:${server.address().port}`,
    token,
    intent,
    checkpointID: "chk_fixture",
    proof: directory,
    remaining: () => 5000,
    wait: async () => {},
  };
  const read = () =>
    JSON.parse(fs.readFileSync(path.join(directory, "first-walk-preparation.json")));
  try {
    await work({
      options,
      read,
      directory,
      get requests() {
        return requests;
      },
    });
    const text = fs.readFileSync(path.join(directory, "first-walk-preparation.json"), "utf8");
    for (const value of [
      token,
      claim,
      requestID,
      privateText,
      "private.invalid",
      "vol-12345678",
      "192.0.2.1",
    ])
      assert.equal(text.includes(value), false, `private value was exported: ${value}`);
    assert.ok(Buffer.byteLength(text) < 4096);
  } finally {
    server.closeAllConnections();
    await new Promise((resolve) => server.close(resolve));
    fs.rmSync(directory, { recursive: true, force: true });
  }
}
const send = (res, status, data) => {
  res.writeHead(status, { "content-type": "application/json", "cf-ray": requestID });
  res.end(JSON.stringify(data));
};
const attached = { phase: "attached", volumeID: "vol-22222222" };

test("retains fixed first/last preparation classifications after a successful readback", async () => {
  await fixture(
    (req, res, n) =>
      n === 1
        ? send(res, 500, {
            error: "first-walk instance account is not verified",
            token,
            claim,
            privateText,
          })
        : send(res, 200, attached),
    async ({ options, read }) => {
      assert.deepEqual(await prepareFirstWalkWithEvidence(options), attached);
      assert.equal(read().attempts, 2);
      assert.equal(read().responses, 2);
      assert.equal(read().outcome, "response-received");
      assert.equal(read().firstError.reason, "instance-account-unverified");
      assert.equal(
        read().firstError.correlationSha256,
        crypto.createHash("sha256").update(requestID).digest("hex"),
      );
    },
  );
});
for (const [label, reply, reason] of [
  ["missing error", (req, res) => send(res, 500, {}), "unclassified"],
  [
    "foreign error shape",
    (req, res) => send(res, 500, { error: { token }, stage: privateText }),
    "unclassified",
  ],
  ["unknown error", (req, res) => send(res, 500, { error: privateText }), "unclassified"],
  ["denied", (req, res) => send(res, 403, { error: privateText }), "unclassified"],
  ["malformed", (req, res) => res.writeHead(500).end("{"), "malformed"],
  ["empty", (req, res) => res.writeHead(500).end(), "malformed"],
  ["oversized", (req, res) => res.writeHead(500).end("x".repeat(64 * 1024 + 1)), "oversized"],
  ["uncertain response", (req, res) => req.socket.destroy(), "transport-uncertain"],
  [
    "redirect",
    (req, res) => res.writeHead(302, { location: "/must-not-follow" }).end(),
    "malformed",
  ],
  ["invalid success body", (req, res) => res.writeHead(200).end("{"), "malformed"],
])
  test(`classifies ${label} without exporting remote text or changing the retry bound`, async () => {
    await fixture(reply, async (f) => {
      await assert.rejects(prepareFirstWalkWithEvidence(f.options), new RegExp(reason));
      assert.equal(f.requests, 120);
      assert.equal(f.read().attempts, 120);
      assert.equal(f.read().outcome, "error");
      assert.equal(f.read().firstError.reason, reason);
      assert.equal(f.read().lastError.reason, reason);
      assert.equal(f.read().errorCounts[reason], 120);
    });
  });
test("allowlists provider codes without exporting AWS XML or guessing a cause for unknown errors", () => {
  assert.deepEqual(
    preparationReason({
      error: `first-walk DescribeSnapshots http 403: <Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>${privateText}</Message></Error></Errors></Response>`,
    }),
    {
      stage: "provider-response",
      reason: "provider-rejected",
      action: "DescribeSnapshots",
      providerStatus: 403,
      providerCode: "UnauthorizedOperation",
    },
  );
  assert.equal(
    preparationReason({ error: `first-walk DescribeSnapshots http 400: <Code>${token}</Code>` })
      .providerCode,
    "unclassified",
  );
  assert.deepEqual(
    preparationReason({
      error: `prefix first-walk DescribeSnapshots http 403: <Code>UnauthorizedOperation</Code>`,
    }),
    { stage: "unlocated", reason: "unclassified" },
  );
  assert.deepEqual(
    preparationReason({
      error: "first-walk capture is not a single owned completed snapshot up to 400 GiB",
    }),
    { stage: "snapshot-readback", reason: "snapshot-not-admitted" },
  );
});
test("an expired forward window stops before dispatch and cannot be reset by retries", async () => {
  await fixture(
    (req, res) => send(res, 500, {}),
    async (f) => {
      f.options.remaining = () => {
        throw new Error("first-walk cutoff");
      };
      await assert.rejects(prepareFirstWalkWithEvidence(f.options), /cutoff/);
      assert.equal(f.requests, 0);
      assert.equal(f.read().attempts, 0);
    },
  );
  await fixture(
    (req, res) => send(res, 500, {}),
    async (f) => {
      let checks = 0;
      f.options.remaining = () => {
        if (++checks > 1) throw new Error("first-walk cutoff");
        return 5000;
      };
      await assert.rejects(prepareFirstWalkWithEvidence(f.options), /cutoff/);
      assert.equal(f.requests, 1);
      assert.equal(f.read().attempts, 1);
      assert.equal(f.read().lastError.reason, "unclassified");
    },
  );
});
test("a response deadline preserves an uncertain outcome before stopping", async () => {
  await fixture(
    () => {},
    async (f) => {
      f.options.remaining = () => 25;
      f.options.wait = async () => {
        throw new Error("fixture stop after uncertainty");
      };
      await assert.rejects(prepareFirstWalkWithEvidence(f.options), /fixture stop/);
      assert.equal(f.read().attempts, 1);
      assert.equal(f.read().lastError.reason, "timeout");
      assert.equal(f.read().responses, 0);
    },
  );
});
test("fresh process observes unresolved durable intent after executor death without another request", async () => {
  let received;
  const seen = new Promise((resolve) => {
    received = resolve;
  });
  await fixture(
    () => {
      received();
    },
    async (f) => {
      const moduleURL = new URL("./image-qualification-first-walk-http.mjs", import.meta.url).href;
      const child = spawn(
        process.execPath,
        [
          "--input-type=module",
          "-e",
          `import {prepareFirstWalkWithEvidence} from ${JSON.stringify(moduleURL)}; await prepareFirstWalkWithEvidence({...JSON.parse(process.env.FIXTURE_OPTIONS), remaining:()=>5000});`,
        ],
        {
          env: { PATH: process.env.PATH, FIXTURE_OPTIONS: JSON.stringify(f.options) },
          stdio: ["ignore", "ignore", "ignore"],
          timeout: 10000,
        },
      );
      const done = new Promise((resolve, reject) => {
        child.on("error", reject);
        child.on("close", resolve);
      });
      await seen;
      assert.equal(f.read().outcome, "request-started");
      child.kill("SIGKILL");
      await done;
      assert.equal(f.read().attempts, 1);
      await assert.rejects(prepareFirstWalkWithEvidence(f.options), /EEXIST/);
      assert.equal(f.requests, 1, "fresh executor cannot overwrite custody and replay");
    },
  );
});
test("refuses a pre-existing evidence symlink before network effects", async () => {
  await fixture(
    (req, res) => send(res, 200, attached),
    async (f) => {
      const protectedFile = path.join(f.directory, "protected");
      fs.writeFileSync(protectedFile, "{}");
      fs.symlinkSync(protectedFile, path.join(f.directory, "first-walk-preparation.json"));
      await assert.rejects(prepareFirstWalkWithEvidence(f.options), /EEXIST/);
      assert.equal(f.requests, 0);
      assert.equal(fs.readFileSync(protectedFile, "utf8"), "{}");
    },
  );
});
