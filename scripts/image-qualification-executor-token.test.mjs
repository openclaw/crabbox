import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import crypto from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import {
  executorCredentialIdentity,
  readExecutorCredential,
  executorEnvironment,
  executorToken,
  maskExecutorSecret,
} from "./image-qualification-executor-token.mjs";

const token = "6".repeat(64); // Obviously synthetic, never a live capability.
const encode = (value) => Buffer.from(JSON.stringify(value)).toString("base64url");
const tokenSha256 = crypto.createHash("sha256").update(token).digest("hex");
function fixture() {
  const env = {
    GITHUB_REPOSITORY: "example-org/example",
    GITHUB_REPOSITORY_ID: "123",
    GITHUB_RUN_ID: "42",
    GITHUB_RUN_ATTEMPT: "1",
    GITHUB_EVENT_NAME: "workflow_dispatch",
    GITHUB_REF: "refs/heads/main",
    GITHUB_SHA: "a".repeat(40),
    GITHUB_WORKFLOW_REF:
      "example-org/example/.github/workflows/image-qualification.yml@refs/heads/main",
    QUALIFICATION_DEFAULT_BRANCH: "main",
    QUALIFICATION_WORKFLOW_SHA: "a".repeat(40),
    QUALIFICATION_CANDIDATE_SHA: "b".repeat(40),
    QUALIFICATION_HANDOFF_SHA256: "c".repeat(64),
    QUALIFICATION_EXPIRES_AT: new Date(Date.now() + 60_000).toISOString(),
    QUALIFICATION_RELAY_URL: "https://crabbox-image-qualification-relay-42-1.example.workers.dev",
    QUALIFICATION_EXECUTOR_TOKEN_SHA256: tokenSha256,
  };
  const envelope = { ...executorCredentialIdentity(env), token };
  env.QUALIFICATION_EXECUTOR_CREDENTIAL = encode(envelope);
  return { env, envelope };
}

test("manual credential matches exact identity and masks token before use, with no network", () => {
  const { env, envelope } = fixture();
  const events = [];
  const result = readExecutorCredential(env, {
    mask: (value) => events.push(["mask", value]),
    expectedDigest: tokenSha256,
  });
  events.push(["use", result.token]);
  assert.deepEqual(events, [
    ["mask", env.QUALIFICATION_EXECUTOR_CREDENTIAL],
    ["mask", token],
    ["use", token],
  ]);
  assert.deepEqual(result, { token, tokenSha256 });
  assert.equal(Object.hasOwn(executorCredentialIdentity(env), "token"), false);
  assert.equal(envelope.runAttempt, "1");
});

test("each envelope identity, schema, type and expiry mismatch fails closed", () => {
  const { env, envelope } = fixture();
  const bad = [
    undefined,
    "",
    "not json",
    encode(null),
    encode([]),
    encode({ ...envelope, extra: true }),
    encode({ ...envelope, token: "x".repeat(64) }),
    " ".repeat(4097),
    ...Object.entries(envelope).map(([key, value]) =>
      encode({ ...envelope, [key]: typeof value === "string" ? value + "0" : 2 }),
    ),
    ...Object.keys(envelope).map((key) => {
      const value = { ...envelope };
      delete value[key];
      return encode(value);
    }),
  ];
  for (const raw of bad) {
    assert.throws(
      () =>
        readExecutorCredential({ ...env, QUALIFICATION_EXECUTOR_CREDENTIAL: raw }, { mask() {} }),
      { message: "invalid qualification executor credential" },
    );
  }
  for (const now of [
    Date.parse(env.QUALIFICATION_EXPIRES_AT),
    Date.parse(env.QUALIFICATION_EXPIRES_AT) + 1,
    Date.parse(env.QUALIFICATION_EXPIRES_AT) - 120 * 60_000 - 1,
    NaN,
  ])
    assert.throws(() => readExecutorCredential(env, { now, mask() {} }));
  for (const expectedDigest of ["", "bad", "d".repeat(64)])
    assert.throws(() => readExecutorCredential(env, { expectedDigest, mask() {} }));
});

test("credential decoding rejects noncanonical base64url, invalid UTF-8 and malformed JSON", () => {
  const { env } = fixture();
  const raw = env.QUALIFICATION_EXECUTOR_CREDENTIAL;
  for (const value of [
    raw + "=",
    raw + "\n",
    raw + " ",
    "+/==",
    "A", // Impossible base64 length.
    "Zh", // Nonzero padding bits; canonical encoding is Zg.
    Buffer.from([0xff, 0xfe]).toString("base64url"),
    Buffer.from('{"token":').toString("base64url"),
    JSON.stringify({ token }), // The old JSON secret format is refused.
  ]) {
    const masks = [];
    assert.throws(
      () =>
        readExecutorCredential(
          { ...env, QUALIFICATION_EXECUTOR_CREDENTIAL: value },
          { mask: (secret) => masks.push(secret) },
        ),
      { message: "invalid qualification executor credential" },
    );
    assert.deepEqual(masks, [value]);
  }
});

test("runtime context substitutions cannot legitimize an old envelope", () => {
  const { env } = fixture();
  for (const [key, value] of Object.entries({
    GITHUB_REPOSITORY_ID: "124",
    GITHUB_RUN_ID: "43",
    GITHUB_RUN_ATTEMPT: "2",
    GITHUB_EVENT_NAME: "push",
    GITHUB_REF: "refs/heads/other",
    GITHUB_SHA: "d".repeat(40),
    GITHUB_WORKFLOW_REF: "example-org/example/.github/workflows/other.yml@refs/heads/main",
    QUALIFICATION_DEFAULT_BRANCH: "other",
    QUALIFICATION_WORKFLOW_SHA: "d".repeat(40),
    QUALIFICATION_CANDIDATE_SHA: "d".repeat(40),
    QUALIFICATION_HANDOFF_SHA256: "d".repeat(64),
    QUALIFICATION_EXPIRES_AT: new Date(Date.now() + 120_000).toISOString(),
  })) {
    assert.throws(() => readExecutorCredential({ ...env, [key]: value }, { mask() {} }), key);
  }
  for (const relay of [
    "https://attacker.invalid",
    env.QUALIFICATION_RELAY_URL + "path",
    env.QUALIFICATION_RELAY_URL.replace("42-1", "43-1"),
    env.QUALIFICATION_RELAY_URL.replace("https:", "http:"),
  ])
    assert.throws(() => executorToken({ ...env, QUALIFICATION_RELAY_URL: relay }));
  assert.throws(() => executorToken({ ...env, QUALIFICATION_EXECUTOR_TOKEN_SHA256: undefined }));
});

test("executor strips envelope and runner credentials and refuses admin bindings even when empty", () => {
  const { env } = fixture();
  const child = executorEnvironment(
    {
      ...env,
      ACTIONS_ID_TOKEN_REQUEST_TOKEN: "fixture",
      ACTIONS_RUNTIME_TOKEN: "fixture",
      GH_TOKEN: "fixture",
      GITHUB_TOKEN: "fixture",
      PATH: "/bin",
    },
    token,
  );
  assert.equal(child.PATH, "/bin");
  assert.equal(child.QUALIFICATION_EXECUTOR_TOKEN, token);
  assert.equal(
    Object.keys(child).some((key) =>
      /^(ACTIONS_|GH_TOKEN$|GITHUB_TOKEN$|QUALIFICATION_EXECUTOR_CREDENTIAL$)/.test(key),
    ),
    false,
  );
  for (const name of [
    "AWS_ACCESS_KEY_ID",
    "AWS_SECRET_ACCESS_KEY",
    "AWS_SESSION_TOKEN",
    "CLOUDFLARE_API_TOKEN",
    "QUALIFICATION_CONTROLLER_TOKEN",
    "QUALIFICATION_ADMIN_TOKEN",
    "QUALIFICATION_SHARED_TOKEN",
  ])
    assert.throws(() => executorEnvironment({ ...env, [name]: "" }, token), name);
});

test("masking escapes workflow-command data", () => {
  const original = process.stdout.write;
  let output = "";
  process.stdout.write = (value) => {
    output += value;
    return true;
  };
  try {
    maskExecutorSecret("fixture%\r\nnext");
  } finally {
    process.stdout.write = original;
  }
  assert.equal(output, "::add-mask::fixture%25%0D%0Anext\n");
});

function launcher(t, script) {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), "qualification-manual-token-"));
  t.after(() => fs.rmSync(directory, { recursive: true, force: true }));
  fs.copyFileSync(
    new URL("./image-qualification-executor-token.mjs", import.meta.url),
    path.join(directory, "launcher.mjs"),
  );
  fs.writeFileSync(path.join(directory, "image-qualification-execute.sh"), script);
  return path.join(directory, "launcher.mjs");
}
function ordinary(output) {
  return output
    .split("\n")
    .filter((line) => !line.startsWith("::add-mask::"))
    .join("\n");
}

test("real process replacement preserves hard kill and excludes envelope from child", (t) => {
  const file = launcher(
    t,
    `set -eu\ntest -n "$QUALIFICATION_EXECUTOR_TOKEN"\ntest -z "\${QUALIFICATION_EXECUTOR_CREDENTIAL+x}"\ntest -z "\${ACTIONS_RUNTIME_TOKEN+x}"\necho executor-started\nkill -KILL $$\n`,
  );
  const { env } = fixture();
  const result = spawnSync(process.execPath, [file], {
    env: { PATH: process.env.PATH, ...env, ACTIONS_RUNTIME_TOKEN: "fixture" },
    encoding: "utf8",
    timeout: 10_000,
  });
  assert.equal(result.signal, "SIGKILL", result.stderr);
  assert.match(result.stdout, /executor-started/);
  assert.doesNotMatch(ordinary(result.stdout) + result.stderr, new RegExp(token));
});

test("runner-style literal masking covers step header and decoded token before child output", (t) => {
  const file = launcher(t, 'printf "child-token=%s\\n" "$QUALIFICATION_EXECUTOR_TOKEN"\n');
  const { env } = fixture();
  const encoded = env.QUALIFICATION_EXECUTOR_CREDENTIAL;
  const result = spawnSync(process.execPath, [file], {
    env: { PATH: process.env.PATH, ...env },
    encoding: "utf8",
    timeout: 10_000,
  });
  assert.equal(result.status, 0, result.stderr);
  const masks = [encoded]; // GitHub secrets context knows the exact stored value at step start.
  const commands = [];
  const rendered = [];
  for (const line of [
    `  env: QUALIFICATION_EXECUTOR_CREDENTIAL: ${encoded}`,
    ...result.stdout.trim().split("\n"),
  ]) {
    if (line.startsWith("::add-mask::")) {
      const value = line.slice("::add-mask::".length);
      masks.push(value);
      commands.push(value);
    } else {
      rendered.push(masks.reduce((text, value) => text.replaceAll(value, "***"), line));
    }
  }
  assert.deepEqual(commands, [encoded, token]);
  assert.deepEqual(rendered, ["  env: QUALIFICATION_EXECUTOR_CREDENTIAL: ***", "child-token=***"]);
  assert.doesNotMatch(rendered.join("\n") + result.stderr, new RegExp(`${encoded}|${token}`));
});

test("launcher rejects credentials with safe settlement guidance and no secret diagnostics", (t) => {
  const file = launcher(t, "echo must-not-execute\nexit 0\n");
  const { env, envelope } = fixture();
  const expired = new Date(Date.now() - 60_000).toISOString();
  for (const overrides of [
    { QUALIFICATION_EXECUTOR_CREDENTIAL: undefined },
    { QUALIFICATION_EXECUTOR_CREDENTIAL: "not json" },
    { QUALIFICATION_EXECUTOR_CREDENTIAL: encode({ ...envelope, runId: "43" }) },
    { QUALIFICATION_EXECUTOR_CREDENTIAL: encode({ ...envelope, token: "bad" }) },
    { QUALIFICATION_EXECUTOR_CREDENTIAL: encode({ ...envelope, token: "7".repeat(64) }) },
    {
      QUALIFICATION_EXPIRES_AT: expired,
      QUALIFICATION_EXECUTOR_CREDENTIAL: encode({ ...envelope, expiresAt: expired }),
    },
  ]) {
    const result = spawnSync(process.execPath, [file], {
      env: { PATH: process.env.PATH, ...env, ...overrides },
      encoding: "utf8",
      timeout: 10_000,
    });
    assert.equal(result.status, 1);
    assert.doesNotMatch(
      ordinary(result.stdout) + result.stderr,
      new RegExp(`${token}|must-not-execute|not json`),
    );
    assert.match(result.stderr, /^qualification executor authentication failed\n/);
    assert.match(result.stderr, /executor-identity\.json for the current admitted run/);
    assert.match(
      result.stderr,
      /both protected environments contain its same original encoded credential/,
    );
    assert.match(
      result.stderr,
      /finalizer\/reaper settle this run, then start a newly authorized run/,
    );
    assert.match(
      result.stderr,
      /Do not replace an enrolled token, extend expiry, or reuse another run's credential/,
    );
    assert.doesNotMatch(result.stderr, /[0-9a-f]{64}/);
    for (const value of Object.values(overrides)) {
      if (typeof value === "string") assert.equal(result.stderr.includes(value), false);
    }
  }
});

test("matched credential still authorizes only existing relay operations and expires", async () => {
  const { env } = fixture();
  const result = readExecutorCredential(env, { mask() {} });
  const relay = (await import("./image-qualification-relay-worker.mjs")).default;
  let calls = 0;
  const relayEnv = {
    EXECUTOR_TOKEN: result.token,
    QUALIFICATION_EXPIRES_AT: env.QUALIFICATION_EXPIRES_AT,
    AWS_REGION: "eu-west-1",
    QUALIFICATION_OWNER: "fixture@example.invalid",
    QUALIFICATION_ORG: "image-qualification",
    CANDIDATE_ADMIN_TOKEN: "admin-fixture",
    CANDIDATE_SHARED_TOKEN: "shared-fixture",
    CANDIDATE: {
      async fetch() {
        calls++;
        return Response.json({ ok: true });
      },
    },
  };
  const request = (suffix, bearer = token) =>
    new Request(`https://relay.invalid${suffix}`, {
      headers: { authorization: `Bearer ${bearer}` },
    });
  assert.equal(
    (
      await relay.fetch(
        request("/v1/images/ami-12345678?provider=aws&target=linux&region=eu-west-1"),
        relayEnv,
      )
    ).status,
    200,
  );
  assert.equal(calls, 1);
  assert.equal((await relay.fetch(request("/qualification/executor-token"), relayEnv)).status, 404);
  assert.equal((await relay.fetch(request("/v1/admin/leases"), relayEnv)).status, 404);
  assert.equal((await relay.fetch(request("/v1/images", "7".repeat(64)), relayEnv)).status, 403);
  assert.equal(
    (
      await relay.fetch(request("/v1/images"), {
        ...relayEnv,
        QUALIFICATION_EXPIRES_AT: new Date(0).toISOString(),
      })
    ).status,
    403,
  );
  assert.equal(calls, 1);
});

test("only first-walk admits the longer handoff while preserving exact expiry", () => {
  const { env } = fixture();
  const expiry = Date.parse(env.QUALIFICATION_EXPIRES_AT);
  for (const [mode, minutes] of [
    [undefined, 120],
    ["mint", 120],
    ["retained", 120],
    ["first-walk", 150],
  ]) {
    const candidate = { ...env, QUALIFICATION_MODE: mode };
    assert.equal(
      readExecutorCredential(candidate, { now: expiry - minutes * 60_000, mask() {} }).token,
      token,
    );
    assert.throws(() =>
      readExecutorCredential(candidate, { now: expiry - minutes * 60_000 - 1, mask() {} }),
    );
    assert.throws(() => readExecutorCredential(candidate, { now: expiry, mask() {} }));
  }
});
