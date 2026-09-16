import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github", "workflows", "coordinator-deploy.yml"),
  "utf8",
);

const artifactPair = {
  CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "synthetic-artifact-access-id",
  CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "synthetic-artifact-secret-key",
};
const snapshot = 'synthetic-snapshot-"quoted"\nvalue';
const invalidBundleMessage =
  "::error::CRABBOX_ARTIFACTS_CREDENTIALS_JSON must be a JSON object containing exactly CRABBOX_ARTIFACTS_ACCESS_KEY_ID and CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY as nonempty strings.\n";
const snapshotConflictMessage =
  "::error::Remove CRABBOX_DAYTONA_SNAPSHOT from the coordinator GitHub environment before clearing the Worker binding.\n";

function fixture(t, overrides = {}) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "coordinator-deploy-test-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const bin = path.join(dir, "bin");
  const runnerTemp = path.join(dir, "runner-temp");
  fs.mkdirSync(bin);
  fs.mkdirSync(runnerTemp);
  fs.symlinkSync(process.execPath, path.join(bin, "node"));
  return {
    dir,
    bin,
    runnerTemp,
    env: {
      // Do not inherit credentials, Node hooks, or private config from the host.
      PATH: `${bin}:/usr/bin:/bin`,
      HOME: dir,
      TMPDIR: runnerTemp,
      RUNNER_TEMP: runnerTemp,
      CRABBOX_ARTIFACTS_CREDENTIALS_JSON: "",
      CRABBOX_DAYTONA_SNAPSHOT: "",
      CLEAR_DAYTONA_SNAPSHOT: "",
      CRABBOX_ARTIFACTS_ACCESS_KEY_ID: "synthetic-ignored-direct-access-id",
      CRABBOX_ARTIFACTS_SECRET_ACCESS_KEY: "synthetic-ignored-direct-secret-key",
      CRABBOX_ARTIFACTS_SESSION_TOKEN: "synthetic-ignored-session-token",
      ...overrides,
    },
  };
}

function runDeploy(t, overrides = {}) {
  const f = fixture(t, overrides);
  const log = path.join(f.dir, "commands.jsonl");
  for (const command of ["npm", "npx"]) {
    fs.writeFileSync(
      path.join(f.bin, command),
      `#!/usr/bin/env node
const fs = require("node:fs");
const args = process.argv.slice(2);
const event = { command: ${JSON.stringify(command)}, args };
if (event.command === "npm") {
  const index = args.indexOf("--secrets-file");
  if (index !== -1) {
    event.file = args[index + 1];
    event.secrets = JSON.parse(fs.readFileSync(event.file, "utf8"));
    event.mode = fs.statSync(event.file).mode & 0o777;
  }
} else {
  event.secrets = JSON.parse(fs.readFileSync(0, "utf8"));
}
fs.appendFileSync(process.env.COMMAND_LOG, JSON.stringify(event) + "\\n");
if (event.command === "npm") process.exit(Number(process.env.DEPLOY_EXIT_CODE || 0));
`,
      { mode: 0o755 },
    );
  }
  const deployStep = workflow.match(
    /      - name: Deploy\n[^]*?        run: \|\n([^]*?)        working-directory: worker/,
  );
  assert.ok(deployStep, "expected the actual Deploy shell block");
  const script = deployStep[1].replace(/^          /gm, "");
  const result = spawnSync("/bin/bash", ["--noprofile", "--norc", "-c", script], {
    cwd: path.join(repoRoot, "worker"),
    env: { ...f.env, COMMAND_LOG: log },
    encoding: "utf8",
  });
  assert.ifError(result.error);
  const events = fs.existsSync(log)
    ? fs
        .readFileSync(log, "utf8")
        .trim()
        .split("\n")
        .map((line) => JSON.parse(line))
    : [];
  assert.deepEqual(fs.readdirSync(f.runnerTemp), [], "temporary secret files must be removed");
  for (const event of events) {
    if (event.file) {
      assert.equal(event.mode, 0o600);
      assert.equal(fs.existsSync(event.file), false);
    }
  }
  assert.doesNotMatch(result.stdout + result.stderr, /synthetic-/);
  assert.doesNotMatch(JSON.stringify(events.map((event) => event.args)), /synthetic-/);
  return { ...result, events };
}

function runPrepare(t, overrides = {}) {
  const f = fixture(t, overrides);
  const file = path.join(f.runnerTemp, "secrets.json");
  fs.writeFileSync(file, "");
  // Verify the helper enforces private permissions before writing any values.
  fs.chmodSync(file, 0o644);
  const result = spawnSync(
    process.execPath,
    [path.join(repoRoot, "scripts", "prepare-coordinator-deploy-secrets.mjs")],
    {
      env: { ...f.env, SECRETS_FILE: file },
      encoding: "utf8",
    },
  );
  assert.ifError(result.error);
  assert.equal(result.stdout, "");
  assert.doesNotMatch(result.stderr, /synthetic-/);
  return {
    ...result,
    contents: fs.readFileSync(file, "utf8"),
    mode: fs.statSync(file).mode & 0o777,
  };
}

function assertDeploy(event, secrets) {
  assert.equal(event.command, "npm");
  assert.deepEqual(
    event.args,
    secrets ? ["run", "deploy", "--", "--secrets-file", event.file] : ["run", "deploy"],
  );
  assert.deepEqual(event.secrets, secrets);
}

test("artifact-only deploy delivers the bundled pair with the Worker version", (t) => {
  const result = runDeploy(t, {
    CRABBOX_ARTIFACTS_CREDENTIALS_JSON: JSON.stringify(artifactPair),
  });
  assert.equal(result.status, 0, result.stderr);
  assert.equal(result.events.length, 1);
  assertDeploy(result.events[0], artifactPair);
});

test("coordinator deploy keeps optional credentials with the serialized environment owner", () => {
  assert.match(workflow, /environment: coordinator/);
  assert.match(workflow, /group: coordinator-deploy\n  cancel-in-progress: false/);
  assert.match(workflow, /CRABBOX_DAYTONA_SNAPSHOT: \$\{\{ secrets\.CRABBOX_DAYTONA_SNAPSHOT \}\}/);
  assert.match(
    workflow,
    /CRABBOX_ARTIFACTS_CREDENTIALS_JSON: \$\{\{ secrets\.CRABBOX_ARTIFACTS_CREDENTIALS_JSON \}\}/,
  );
  assert.match(
    workflow,
    /paths:\n(?:      - .*\n)*      - "scripts\/prepare-coordinator-deploy-secrets\.mjs"/,
  );
  assert.doesNotMatch(workflow, /secrets\.CRABBOX_ARTIFACTS_(ACCESS_KEY_ID|SECRET_ACCESS_KEY)/);
  assert.doesNotMatch(workflow, /wrangler secret put/);
  assert.match(workflow, /^      clearDaytonaSnapshot:$/m);
  assert.match(workflow, /type: boolean/);
  assert.match(workflow, /CLEAR_DAYTONA_SNAPSHOT: \$\{\{ inputs\.clearDaytonaSnapshot \}\}/);
});

for (const [label, env, expected] of [
  ["neither input", {}, undefined],
  ["snapshot only", { CRABBOX_DAYTONA_SNAPSHOT: snapshot }, { CRABBOX_DAYTONA_SNAPSHOT: snapshot }],
  [
    "artifact only",
    { CRABBOX_ARTIFACTS_CREDENTIALS_JSON: JSON.stringify(artifactPair) },
    artifactPair,
  ],
  [
    "combined",
    {
      CRABBOX_DAYTONA_SNAPSHOT: snapshot,
      CRABBOX_ARTIFACTS_CREDENTIALS_JSON: JSON.stringify(artifactPair),
    },
    { ...artifactPair, CRABBOX_DAYTONA_SNAPSHOT: snapshot },
  ],
]) {
  test(`secrets preparation and deploy preserve exact bindings: ${label}`, (t) => {
    const prepared = runPrepare(t, env);
    assert.equal(prepared.status, 0, prepared.stderr);
    assert.equal(prepared.stderr, "");
    assert.equal(prepared.mode, expected ? 0o600 : 0o644);
    assert.deepEqual(prepared.contents ? JSON.parse(prepared.contents) : undefined, expected);
    const deployed = runDeploy(t, env);
    assert.equal(deployed.status, 0, deployed.stderr);
    assert.equal(deployed.stdout + deployed.stderr, "");
    assert.equal(deployed.events.length, 1);
    assertDeploy(deployed.events[0], expected);
  });
}

const invalidBundles = [
  ["invalid JSON", '{"synthetic-invalid-json'],
  ["whitespace JSON", " "],
  ["null", "null"],
  ["array", "[]"],
  ["string", '"synthetic-nonobject"'],
  ["number", "42"],
  ["boolean", "true"],
  ["empty object", "{}"],
  ["extra snapshot", JSON.stringify({ ...artifactPair, CRABBOX_DAYTONA_SNAPSHOT: snapshot })],
  [
    "extra session token",
    JSON.stringify({ ...artifactPair, CRABBOX_ARTIFACTS_SESSION_TOKEN: "synthetic-extra" }),
  ],
  ["extra prototype key", JSON.stringify({ ...artifactPair, ["__proto__"]: "synthetic-extra" })],
];
for (const key of Object.keys(artifactPair)) {
  const partial = { ...artifactPair };
  delete partial[key];
  invalidBundles.push([`missing ${key}`, JSON.stringify(partial)]);
  for (const value of [null, 1, false, [], {}, "", " ", "\t\r\n"]) {
    invalidBundles.push([
      `${key} is ${JSON.stringify(value)}`,
      JSON.stringify({ ...artifactPair, [key]: value }),
    ]);
  }
}
for (const [label, bundle] of invalidBundles) {
  test(`invalid bundle stops preparation and deployment without disclosure: ${label}`, (t) => {
    const env = {
      CRABBOX_ARTIFACTS_CREDENTIALS_JSON: bundle,
      CRABBOX_DAYTONA_SNAPSHOT: snapshot,
    };
    const prepared = runPrepare(t, env);
    assert.equal(prepared.status, 1);
    assert.equal(prepared.stderr, invalidBundleMessage);
    assert.equal(prepared.contents, "");
    assert.equal(prepared.mode, 0o644, "validation must precede file mutation");
    const deployed = runDeploy(t, env);
    assert.equal(deployed.status, 1);
    assert.equal(deployed.stdout, "");
    assert.equal(deployed.stderr, invalidBundleMessage);
    assert.deepEqual(deployed.events, []);
  });
}

for (const withArtifacts of [false, true]) {
  const label = withArtifacts ? "with artifact pair" : "without artifact pair";
  const env = {
    CLEAR_DAYTONA_SNAPSHOT: "true",
    CRABBOX_ARTIFACTS_CREDENTIALS_JSON: withArtifacts ? JSON.stringify(artifactPair) : "",
  };
  test(`explicit clear follows successful deployment only: ${label}`, (t) => {
    const prepared = runPrepare(t, env);
    assert.equal(prepared.status, 0, prepared.stderr);
    assert.equal(prepared.stderr, "");
    assert.deepEqual(
      prepared.contents ? JSON.parse(prepared.contents) : undefined,
      withArtifacts ? artifactPair : undefined,
    );
    const deployed = runDeploy(t, env);
    assert.equal(deployed.status, 0, deployed.stderr);
    assert.equal(deployed.stdout + deployed.stderr, "");
    assert.equal(deployed.events.length, 2);
    assertDeploy(deployed.events[0], withArtifacts ? artifactPair : undefined);
    assert.deepEqual(deployed.events[1], {
      command: "npx",
      args: ["wrangler", "secret", "bulk"],
      secrets: { CRABBOX_DAYTONA_SNAPSHOT: null },
    });
  });
  test(`snapshot and clear conflict stops before writing or deploying: ${label}`, (t) => {
    const conflicting = { ...env, CRABBOX_DAYTONA_SNAPSHOT: snapshot };
    const prepared = runPrepare(t, conflicting);
    assert.equal(prepared.status, 1);
    assert.equal(prepared.stderr, snapshotConflictMessage);
    assert.equal(prepared.contents, "");
    assert.equal(prepared.mode, 0o644);
    const deployed = runDeploy(t, conflicting);
    assert.equal(deployed.status, 1);
    assert.equal(deployed.stdout, "");
    assert.equal(deployed.stderr, snapshotConflictMessage);
    assert.deepEqual(deployed.events, []);
  });
  test(`deploy failure prevents clear and cleans temporary files: ${label}`, (t) => {
    const result = runDeploy(t, { ...env, DEPLOY_EXIT_CODE: "17" });
    assert.equal(result.status, 17);
    assert.equal(result.stdout + result.stderr, "");
    assert.equal(result.events.length, 1);
    assertDeploy(result.events[0], withArtifacts ? artifactPair : undefined);
  });
}

test("invalid bundle prevents an otherwise valid clear request", (t) => {
  const result = runDeploy(t, {
    CLEAR_DAYTONA_SNAPSHOT: "true",
    CRABBOX_ARTIFACTS_CREDENTIALS_JSON: "synthetic-invalid-json",
  });
  assert.equal(result.status, 1);
  assert.equal(result.stdout, "");
  assert.equal(result.stderr, invalidBundleMessage);
  assert.deepEqual(result.events, []);
});

test("secrets preparation reports file failures without paths or values", (t) => {
  const f = fixture(t, {
    CRABBOX_ARTIFACTS_CREDENTIALS_JSON: JSON.stringify(artifactPair),
  });
  const result = spawnSync(
    process.execPath,
    [path.join(repoRoot, "scripts", "prepare-coordinator-deploy-secrets.mjs")],
    {
      env: { ...f.env, SECRETS_FILE: path.join(f.runnerTemp, "synthetic-missing-file") },
      encoding: "utf8",
    },
  );
  assert.ifError(result.error);
  assert.equal(result.status, 1);
  assert.equal(result.stdout, "");
  assert.equal(result.stderr, "::error::Unable to prepare coordinator deploy secrets file.\n");
  assert.deepEqual(fs.readdirSync(f.runnerTemp), []);
});
