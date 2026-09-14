import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import fs from "node:fs";
import http from "node:http";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { verifyPublisherAuth } from "./image-publisher-auth-check.mjs";

const env = {
  CRABBOX_COORDINATOR: "https://crabbox.openclaw.ai",
  CRABBOX_COORDINATOR_ADMIN_TOKEN: "test-only-private-token",
};
const identity = { auth: "bearer", admin: true, owner: "private-owner", org: "" };

function transport(onResponse, statusCode = 200) {
  const req = new EventEmitter();
  const res = new EventEmitter();
  let calls = 0;
  req.destroy = () => {
    req.destroyed = true;
  };
  res.destroy = () => {
    res.destroyed = true;
  };
  res.statusCode = statusCode;
  const request = (url, options, callback) => {
    calls++;
    assert.equal(url, "https://crabbox.openclaw.ai/v1/whoami");
    assert.equal(options.method, "GET");
    assert.equal(options.rejectUnauthorized, true);
    assert.equal(options.agent, false);
    assert.equal(options.maxHeaderSize, 16 * 1024);
    req.options = options;
    req.end = () => {
      callback(res);
      onResponse?.(res, req);
    };
    return req;
  };
  return { request, req, res, calls: () => calls };
}

function jsonResponse(body) {
  return transport((res) => {
    res.emit("data", Buffer.from(JSON.stringify(body)));
    res.emit("end");
  });
}

for (const auth of ["bearer", "github", "device", "proxy"]) {
  test(`recognizes ${auth} admin only from strict whoami response`, async () => {
    const fixture = jsonResponse({ ...identity, auth, tokenExpiresAt: "private-expiry" });
    const result = await verifyPublisherAuth(env, fixture.request);
    assert.deepEqual(result, { status: "verified", httpStatus: 200, auth, admin: true });
    assert.equal(fixture.calls(), 1);
    assert.equal(fixture.req.destroyed, true);
    assert.equal(fixture.res.destroyed, true);
    assert.deepEqual(fixture.req.options.headers, {
      Authorization: `Bearer ${env.CRABBOX_COORDINATOR_ADMIN_TOKEN}`,
      Accept: "application/json",
    });
  });
}

test("preserves literal credential bytes and sends Access secrets only as headers", async () => {
  const fixture = jsonResponse(identity);
  const configured = {
    ...env,
    CRABBOX_COORDINATOR_ADMIN_TOKEN: " literal-token ",
    CRABBOX_ACCESS_CLIENT_ID: "test-access-id",
    CRABBOX_ACCESS_CLIENT_SECRET: "test-access-secret",
  };
  const result = await verifyPublisherAuth(configured, fixture.request);
  assert.equal(fixture.req.options.headers.Authorization, "Bearer  literal-token ");
  assert.equal(fixture.req.options.headers["CF-Access-Client-Id"], "test-access-id");
  assert.equal(fixture.req.options.headers["CF-Access-Client-Secret"], "test-access-secret");
  assert.doesNotMatch(JSON.stringify(result), /literal-token|test-access|private/);
});

test("200 non-admin is not successful verification", async () => {
  const fixture = jsonResponse({ ...identity, auth: "github", admin: false });
  assert.deepEqual(await verifyPublisherAuth(env, fixture.request), {
    status: "not_admin",
    httpStatus: 200,
    auth: "github",
    admin: false,
  });
});

for (const statusCode of [301, 302, 307, 308, 401, 403, 429, 500]) {
  test(`HTTP ${statusCode} never follows, retries, or consumes the response`, async () => {
    const fixture = transport((res) => {
      assert.equal(res.destroyed, true);
      assert.equal(res.listenerCount("data"), 0);
    }, statusCode);
    const result = await verifyPublisherAuth(env, fixture.request);
    assert.deepEqual(result, {
      status: "http_error",
      httpStatus: statusCode,
      auth: "unknown",
      admin: false,
    });
    assert.equal(fixture.calls(), 1);
  });
}

for (const body of [
  null,
  [],
  {},
  { ...identity, admin: "true" },
  { ...identity, admin: 1 },
  { ...identity, auth: "private-token" },
  { ...identity, owner: 12 },
  { ...identity, org: null },
]) {
  test(`rejects malformed whoami shape ${JSON.stringify(body)}`, async () => {
    const fixture = jsonResponse(body);
    const result = await verifyPublisherAuth(env, fixture.request);
    assert.deepEqual(result, {
      status: "invalid_response",
      httpStatus: 200,
      auth: "unknown",
      admin: false,
    });
  });
}

test("HTML or malformed JSON cannot leak into results", async () => {
  const fixture = transport((res) => {
    res.emit("data", Buffer.from(`<html>${env.CRABBOX_COORDINATOR_ADMIN_TOKEN}</html>`));
    res.emit("end");
  });
  const result = await verifyPublisherAuth(env, fixture.request);
  assert.equal(result.status, "invalid_response");
  assert.doesNotMatch(JSON.stringify(result), /html|private-token/);
});

test("response cap counts bytes across chunks and destroys the request", async () => {
  const fixture = transport((res) => {
    res.emit("data", Buffer.alloc(32 * 1024, " "));
    res.emit("data", Buffer.alloc(32 * 1024, " "));
    assert.notEqual(res.destroyed, true);
    res.emit("data", Buffer.from("x"));
    assert.equal(res.destroyed, true);
    res.emit("end");
  });
  assert.equal((await verifyPublisherAuth(env, fixture.request)).status, "response_too_large");
  assert.equal(fixture.req.destroyed, true);
});

for (const phase of ["headers", "body"]) {
  test(`absolute deadline aborts while waiting for ${phase}`, async (t) => {
    t.mock.timers.enable({ apis: ["setTimeout"] });
    const fixture = transport(() => {});
    const request = (url, options, callback) => {
      const req = fixture.request(url, options, callback);
      if (phase === "headers") req.end = () => {};
      return req;
    };
    const pending = verifyPublisherAuth(env, request);
    t.mock.timers.tick(9_999);
    assert.notEqual(fixture.req.destroyed, true);
    if (phase === "body") fixture.res.emit("data", Buffer.from(" "));
    t.mock.timers.tick(1);
    const result = await pending;
    assert.equal(result.status, "timeout");
    assert.equal(fixture.req.destroyed, true);
    assert.equal(fixture.calls(), 1);
  });
}

for (const event of ["error", "aborted", "close"]) {
  test(`response ${event} is a sanitized failure`, async () => {
    const fixture = transport((res) =>
      res.emit(event, new Error(env.CRABBOX_COORDINATOR_ADMIN_TOKEN)),
    );
    const result = await verifyPublisherAuth(env, fixture.request);
    assert.equal(result.status, "response_error");
    assert.doesNotMatch(JSON.stringify(result), /private-token/);
  });
}

test("request errors and synchronous header errors are sanitized", async () => {
  for (const synchronous of [true, false]) {
    const fixture = transport((res, req) =>
      req.emit("error", new Error(env.CRABBOX_COORDINATOR_ADMIN_TOKEN)),
    );
    const request = synchronous
      ? () => {
          throw new Error(env.CRABBOX_COORDINATOR_ADMIN_TOKEN);
        }
      : fixture.request;
    const result = await verifyPublisherAuth(env, request);
    assert.equal(result.status, "request_error");
    assert.doesNotMatch(JSON.stringify(result), /private-token/);
  }
});

for (const overrides of [
  { CRABBOX_COORDINATOR_ADMIN_TOKEN: "" },
  { CRABBOX_COORDINATOR: "" },
  { CRABBOX_COORDINATOR: "http://crabbox.openclaw.ai" },
  { CRABBOX_COORDINATOR: "https://crabbox.openclaw.ai/" },
  { CRABBOX_COORDINATOR: "https://crabbox.openclaw.ai.evil.invalid" },
  { CRABBOX_COORDINATOR: "https://user@crabbox.openclaw.ai" },
  { CRABBOX_COORDINATOR: "https://crabbox.openclaw.ai?query" },
  { CRABBOX_COORDINATOR: "https://crabbox.openclaw.ai#fragment" },
  { CRABBOX_ACCESS_CLIENT_ID: "test-id" },
  { CRABBOX_ACCESS_CLIENT_SECRET: "test-secret" },
]) {
  test(`invalid configuration prevents any request: ${JSON.stringify(overrides)}`, async () => {
    let calls = 0;
    const result = await verifyPublisherAuth({ ...env, ...overrides }, () => {
      calls++;
    });
    assert.equal(result.status, "configuration_error");
    assert.equal(result.httpStatus, null);
    assert.equal(calls, 0);
  });
}

test("CLI emits only the allowlisted result and exits nonzero on configuration failure", () => {
  const result = spawnSync(
    process.execPath,
    [new URL("./image-publisher-auth-check.mjs", import.meta.url).pathname],
    {
      env: { ...env, CRABBOX_COORDINATOR: "https://private.invalid" },
      encoding: "utf8",
      timeout: 5_000,
    },
  );
  assert.equal(result.status, 1);
  assert.equal(result.stderr, "");
  assert.deepEqual(JSON.parse(result.stdout), {
    status: "configuration_error",
    httpStatus: null,
    auth: "unknown",
    admin: false,
  });
});

test("real HTTP lifecycle completes or fails closed with no repeated request", async (t) => {
  for (const scenario of ["success", "redirect", "truncated"]) {
    let calls = 0;
    const server = http.createServer((req, res) => {
      calls++;
      assert.equal(req.method, "GET");
      assert.equal(req.url, "/v1/whoami");
      if (scenario === "redirect") {
        res.writeHead(302, { Location: "https://private.invalid" });
        res.end("private redirect body");
      } else if (scenario === "truncated") {
        res.writeHead(200, { "Content-Length": "1000" });
        res.write("{");
        setImmediate(() => res.destroy());
      } else {
        res.end(JSON.stringify(identity));
      }
    });
    t.after(() => server.close());
    await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
    const request = (_url, options, callback) =>
      http.request(`http://127.0.0.1:${server.address().port}/v1/whoami`, options, callback);
    const result = await verifyPublisherAuth(env, request);
    assert.equal(
      result.status,
      { success: "verified", redirect: "http_error", truncated: "response_error" }[scenario],
    );
    assert.equal(calls, 1);
    server.close();
  }
});

test("protected guard executes successfully only for the protected default-branch workflow", () => {
  const workflow = fs.readFileSync(
    new URL("../.github/workflows/image-publisher-auth-check.yml", import.meta.url),
    "utf8",
  );
  const guard = workflow
    .match(/        run: \|\n([\s\S]*?)\n  verify:/)[1]
    .split("\n")
    .map((line) => line.slice(10))
    .join("\n");
  const configured = {
    DEFAULT_BRANCH: "main",
    REF_PROTECTED: "true",
    RUN_SHA: "test-sha",
    WORKFLOW_SHA: "test-sha",
    GITHUB_EVENT_NAME: "workflow_dispatch",
    GITHUB_REF: "refs/heads/main",
    GITHUB_REPOSITORY: "example-org/crabbox",
    GITHUB_WORKFLOW_REF:
      "example-org/crabbox/.github/workflows/image-publisher-auth-check.yml@refs/heads/main",
  };
  for (const override of [
    null,
    { REF_PROTECTED: "false" },
    { WORKFLOW_SHA: "other-sha" },
    { GITHUB_EVENT_NAME: "push" },
    { GITHUB_REF: "refs/heads/topic" },
    {
      GITHUB_WORKFLOW_REF:
        "example-org/crabbox/.github/workflows/image-publisher-auth-check.yml@refs/heads/topic",
    },
  ]) {
    const result = spawnSync("bash", ["-c", guard], {
      env: { PATH: process.env.PATH, ...configured, ...override },
      encoding: "utf8",
      timeout: 5_000,
    });
    assert.equal(result.status === 0, override === null);
  }
});

test("workflow is manual, protected, pinned, and has no provider or credential-write path", () => {
  const workflow = fs.readFileSync(
    new URL("../.github/workflows/image-publisher-auth-check.yml", import.meta.url),
    "utf8",
  );
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.doesNotMatch(workflow, /^  (push|pull_request|schedule|workflow_call):|\binputs:/m);
  assert.match(workflow, /permissions:\n  contents: read/);
  assert.match(workflow, /needs: guard/);
  assert.match(workflow, /environment: image-publisher/);
  assert.match(workflow, /cancel-in-progress: false/);
  for (const guard of [
    '[[ "$GITHUB_EVENT_NAME" == workflow_dispatch ]]',
    '[[ "$GITHUB_REF" == "$expected_ref" ]]',
    '[[ "$GITHUB_WORKFLOW_REF" == "$expected_workflow_ref" ]]',
    '[[ "$REF_PROTECTED" == true ]]',
    '[[ "$WORKFLOW_SHA" == "$RUN_SHA" ]]',
  ])
    assert.ok(workflow.includes(guard));
  assert.match(workflow, /image-publisher-auth-check\.yml@\$expected_ref/);
  assert.match(workflow, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(workflow, /persist-credentials: false/);
  const [beforeSecretStep, secretStep] = workflow.split(
    "      - name: Check admin access without publishing\n",
  );
  assert.doesNotMatch(beforeSecretStep, /secrets\./);
  assert.equal((secretStep.match(/secrets\./g) ?? []).length, 3);
  assert.match(secretStep, /run: node scripts\/image-publisher-auth-check\.mjs\s*$/);
  assert.doesNotMatch(
    workflow,
    /id-token:|write|upload-artifact|setup-go|npm |npx |curl |aws |wrangler|mint-|warmup|promote|deploy|GITHUB_ENV/,
  );
});
