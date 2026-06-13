import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const root = path.resolve(import.meta.dirname, "..");

function writeExecutable(file, body) {
  fs.writeFileSync(file, body, "utf8");
  fs.chmodSync(file, 0o755);
}

function runSmoke(env = {}) {
  return spawnSync("bash", ["scripts/deploy-cloudflare-dynamic-workers-smoke.sh"], {
    cwd: root,
    env: {
      PATH: process.env.PATH ?? "",
      HOME: process.env.HOME ?? os.tmpdir(),
      TMPDIR: process.env.TMPDIR ?? os.tmpdir(),
      ...env,
    },
    encoding: "utf8",
  });
}

test("dynamic workers smoke is blocked and non-mutating without live gate", () => {
  const result = runSmoke({
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN: "secret-token",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /environment_blocked/);
  assert.match(result.stdout, /provider=cloudflare-dynamic-workers/);
  assert.match(result.stdout, /mutation=false/);
  assert.match(result.stdout, /reason=live_gate_missing/);
  assert.doesNotMatch(result.stdout + result.stderr, /secret-token/);
});

test("dynamic workers smoke reports missing live credentials without echoing secrets", () => {
  const result = runSmoke({
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "cloudflare-dynamic-workers",
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN: "secret-token",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /auth_blocked/);
  assert.match(result.stdout, /mutation=false/);
  assert.match(result.stdout, /missing=.*CLOUDFLARE_ACCOUNT_ID/);
  assert.doesNotMatch(result.stdout + result.stderr, /secret-token/);
});

test("dynamic workers smoke classifies deploy quota failures", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-cfdw-smoke-"));
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);
  const fakeCrabbox = path.join(dir, "crabbox");
  const npxCalls = path.join(dir, "npx-calls.jsonl");

  writeExecutable(fakeCrabbox, "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(path.join(bin, "npm"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    path.join(bin, "npx"),
    `#!/usr/bin/env node
const fs = require("node:fs");
fs.appendFileSync(process.env.CRABBOX_FAKE_NPX_CALLS, JSON.stringify(process.argv.slice(2)) + "\\n");
process.stderr.write("quota exceeded for dynamic workers\\n");
process.exit(7);
`,
  );

  const result = runSmoke({
    PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
    CRABBOX_BIN: fakeCrabbox,
    CRABBOX_FAKE_NPX_CALLS: npxCalls,
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "cloudflare-dynamic-workers",
    CRABBOX_CFDW_SKIP_LOCAL_CHECKS: "1",
    CLOUDFLARE_ACCOUNT_ID: "account",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /quota_blocked/);
  assert.match(result.stdout, /reason=deploy_failed/);
  const seen = fs
    .readFileSync(npxCalls, "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  assert.ok(
    !seen.some((args) => args[0] === "wrangler" && args[1] === "delete"),
    `unexpected Worker cleanup before deploy attempt in ${JSON.stringify(seen)}`,
  );
});

test("dynamic workers smoke stops a kept run parsed from timing JSON after failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-cfdw-smoke-"));
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);
  const calls = path.join(dir, "calls.jsonl");

  writeExecutable(path.join(bin, "npm"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(path.join(bin, "npx"), "#!/usr/bin/env bash\ncat >/dev/null\nexit 0\n");

  const fakeCrabbox = path.join(dir, "crabbox");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env node
const fs = require("node:fs");
const calls = process.env.CRABBOX_FAKE_CALLS;
const args = process.argv.slice(2);
fs.appendFileSync(calls, JSON.stringify(args) + "\\n");
if (args[0] === "doctor") process.exit(0);
if (args[0] === "run" && args.includes("--keep")) {
  process.stderr.write(JSON.stringify({ leaseId: "cfdw_keep", provider: "cloudflare-dynamic-workers", exitCode: 7 }) + "\\n");
  process.stderr.write("runner-secret\\n");
  process.exit(7);
}
if (args[0] === "stop") process.exit(0);
process.exit(0);
`,
  );

  const result = runSmoke({
    PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
    CRABBOX_BIN: fakeCrabbox,
    CRABBOX_FAKE_CALLS: calls,
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "cloudflare-dynamic-workers",
    CRABBOX_CFDW_SKIP_LOCAL_CHECKS: "1",
    CRABBOX_CFDW_SKIP_DEPLOY: "1",
    CLOUDFLARE_ACCOUNT_ID: "account",
    CLOUDFLARE_API_TOKEN: "cloudflare-secret",
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL: "https://runner.example.test",
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN: "runner-secret",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /environment_blocked|auth_blocked|quota_blocked/);
  assert.doesNotMatch(result.stdout + result.stderr, /cloudflare-secret|runner-secret/);

  const seen = fs
    .readFileSync(calls, "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  assert.ok(
    seen.some((args) =>
      JSON.stringify(args) ===
      JSON.stringify(["stop", "--provider", "cloudflare-dynamic-workers", "--id", "cfdw_keep"]),
    ),
    `expected trap stop call in ${JSON.stringify(seen)}`,
  );
});

test("dynamic workers smoke classifies an unavailable live repo", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-cfdw-smoke-"));
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);

  writeExecutable(path.join(bin, "npm"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(path.join(bin, "npx"), "#!/usr/bin/env bash\ncat >/dev/null\nexit 0\n");
  const fakeCrabbox = path.join(dir, "crabbox");
  writeExecutable(fakeCrabbox, "#!/usr/bin/env bash\nexit 0\n");

  const result = runSmoke({
    PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
    CRABBOX_BIN: fakeCrabbox,
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "cloudflare-dynamic-workers",
    CRABBOX_CFDW_SKIP_LOCAL_CHECKS: "1",
    CRABBOX_CFDW_SKIP_DEPLOY: "1",
    CRABBOX_LIVE_REPO: path.join(dir, "missing"),
    CLOUDFLARE_ACCOUNT_ID: "account",
    CLOUDFLARE_API_TOKEN: "cloudflare-secret",
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL: "https://runner.example.test",
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN: "runner-secret",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /environment_blocked/);
  assert.match(result.stdout, /reason=live_repo_unavailable/);
  assert.doesNotMatch(result.stdout + result.stderr, /cloudflare-secret|runner-secret/);
});

test("dynamic workers smoke forces blocked egress for the egress probe", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-cfdw-smoke-"));
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);
  const calls = path.join(dir, "calls.jsonl");

  writeExecutable(path.join(bin, "npm"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(path.join(bin, "npx"), "#!/usr/bin/env bash\ncat >/dev/null\nexit 0\n");

  const fakeCrabbox = path.join(dir, "crabbox");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env node
const fs = require("node:fs");
const calls = process.env.CRABBOX_FAKE_CALLS;
const args = process.argv.slice(2);
fs.appendFileSync(calls, JSON.stringify(args) + "\\n");
if (args[0] === "doctor") process.exit(0);
if (args[0] === "run" && args.includes("--keep")) {
  process.stdout.write("CRABBOX_CFDW_OK\\n");
  process.stderr.write(JSON.stringify({ leaseId: "cfdw_keep", provider: "cloudflare-dynamic-workers", exitCode: 0 }) + "\\n");
  process.exit(0);
}
if (args[0] === "run") {
  process.stdout.write("CRABBOX_CFDW_EGRESS_BLOCKED\\n");
  process.stderr.write(JSON.stringify({ leaseId: "cfdw_egress", provider: "cloudflare-dynamic-workers", exitCode: 0 }) + "\\n");
  process.exit(0);
}
process.exit(0);
`,
  );

  const result = runSmoke({
    PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
    CRABBOX_BIN: fakeCrabbox,
    CRABBOX_FAKE_CALLS: calls,
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "cloudflare-dynamic-workers",
    CRABBOX_CFDW_SKIP_LOCAL_CHECKS: "1",
    CRABBOX_CFDW_SKIP_DEPLOY: "1",
    CLOUDFLARE_ACCOUNT_ID: "account",
    CLOUDFLARE_API_TOKEN: "cloudflare-secret",
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_URL: "https://runner.example.test",
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN: "runner-secret",
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_EGRESS: "intercept",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /live_cloudflare_dynamic_workers_smoke_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /cloudflare-secret|runner-secret/);

  const seen = fs
    .readFileSync(calls, "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  assert.ok(
    seen.some(
      (args) =>
        args[0] === "run" &&
        !args.includes("--keep") &&
        args.includes("--cloudflare-dynamic-workers-cache") &&
        args.includes("one-shot") &&
        args.includes("--cloudflare-dynamic-workers-egress") &&
        args.includes("blocked"),
    ),
    `expected egress run to force blocked mode in ${JSON.stringify(seen)}`,
  );
});

test("dynamic workers smoke deletes its Worker before the KV namespace", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-cfdw-smoke-"));
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);
  const npxCalls = path.join(dir, "npx-calls.jsonl");
  const runnerToken = 'runner#token="quoted"';

  writeExecutable(path.join(bin, "npm"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    path.join(bin, "npx"),
    `#!/usr/bin/env node
const fs = require("node:fs");
const args = process.argv.slice(2);
fs.appendFileSync(process.env.CRABBOX_FAKE_NPX_CALLS, JSON.stringify(args) + "\\n");
if (args[0] === "wrangler" && args[1] === "kv" && args[2] === "namespace" && args[3] === "create") {
  if (!args.includes("--update-config")) process.exit(2);
  const configPath = args[args.indexOf("--config") + 1];
  const config = JSON.parse(fs.readFileSync(configPath, "utf8"));
  config.kv_namespaces = [{ binding: "RUNS", id: "fake-kv-id" }];
  fs.writeFileSync(configPath, JSON.stringify(config));
}
if (args[0] === "wrangler" && args[1] === "deploy") {
  const configPath = args[args.indexOf("--config") + 1];
  const config = JSON.parse(fs.readFileSync(configPath, "utf8"));
  const cleanupMigration = config.migrations?.some((migration) =>
    migration.deleted_classes?.includes("DynamicWorkerRunCoordinator")
  );
  if (cleanupMigration) {
    if (config.durable_objects !== undefined) process.exit(6);
    process.exit(0);
  }
  const coordinator = config.durable_objects?.bindings?.find(
    (binding) => binding.name === "RUN_COORDINATOR",
  );
  if (coordinator?.class_name !== "DynamicWorkerRunCoordinator") process.exit(4);
  if (
    !config.migrations?.some((migration) =>
      migration.new_sqlite_classes?.includes("DynamicWorkerRunCoordinator"),
    )
  ) process.exit(5);
  const secretsPath = args[args.indexOf("--secrets-file") + 1];
  const secrets = JSON.parse(fs.readFileSync(secretsPath, "utf8"));
  if (secrets.CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN !== process.env.CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN) process.exit(3);
  process.stdout.write("https://crabbox-cfdw-smoke.example.workers.dev\\n");
}
`,
  );

  const fakeCrabbox = path.join(dir, "crabbox");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env node
const args = process.argv.slice(2);
if (args[0] === "doctor") process.exit(0);
if (args[0] === "run" && args.includes("--keep")) {
  process.stdout.write("CRABBOX_CFDW_OK\\n");
  process.stderr.write(JSON.stringify({ leaseId: "cfdw_keep", provider: "cloudflare-dynamic-workers", exitCode: 0 }) + "\\n");
  process.exit(0);
}
if (args[0] === "run") {
  process.stdout.write("CRABBOX_CFDW_EGRESS_BLOCKED\\n");
  process.exit(0);
}
process.exit(0);
`,
  );

  const result = runSmoke({
    PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
    CRABBOX_BIN: fakeCrabbox,
    CRABBOX_FAKE_NPX_CALLS: npxCalls,
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "cloudflare-dynamic-workers",
    CRABBOX_CFDW_SKIP_LOCAL_CHECKS: "1",
    CLOUDFLARE_ACCOUNT_ID: "account",
    CRABBOX_CLOUDFLARE_DYNAMIC_WORKERS_TOKEN: runnerToken,
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /live_cloudflare_dynamic_workers_smoke_passed/);
  assert.ok(!(result.stdout + result.stderr).includes(runnerToken));

  const seen = fs
    .readFileSync(npxCalls, "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  const deleteKV = seen.findIndex(
    (args) =>
      args[0] === "wrangler" &&
      args[1] === "kv" &&
      args[2] === "namespace" &&
      args[3] === "delete" &&
      args.includes("fake-kv-id"),
  );
  const deleteWorker = seen.findIndex(
    (args) => args[0] === "wrangler" && args[1] === "delete",
  );
  const retireDurableObject = seen.findIndex(
    (args) =>
      args[0] === "wrangler" &&
      args[1] === "deploy" &&
      !args.includes("--secrets-file"),
  );
  assert.ok(deleteKV >= 0, `missing KV cleanup in ${JSON.stringify(seen)}`);
  assert.ok(deleteWorker >= 0, `missing Worker cleanup in ${JSON.stringify(seen)}`);
  assert.ok(
    retireDurableObject >= 0,
    `missing Durable Object cleanup migration in ${JSON.stringify(seen)}`,
  );
  assert.ok(
    retireDurableObject < deleteWorker,
    `expected Durable Object cleanup before Worker cleanup in ${JSON.stringify(seen)}`,
  );
  assert.ok(deleteWorker < deleteKV, `expected Worker cleanup before KV cleanup in ${JSON.stringify(seen)}`);
});

test("dynamic workers smoke does not report success when ephemeral cleanup fails", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-cfdw-smoke-"));
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);
  const npxCalls = path.join(dir, "npx-calls.jsonl");

  writeExecutable(path.join(bin, "npm"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    path.join(bin, "npx"),
    `#!/usr/bin/env node
const fs = require("node:fs");
const args = process.argv.slice(2);
fs.appendFileSync(process.env.CRABBOX_FAKE_NPX_CALLS, JSON.stringify(args) + "\\n");
if (args[0] === "wrangler" && args[1] === "kv" && args[2] === "namespace" && args[3] === "create") {
  const configPath = args[args.indexOf("--config") + 1];
  const config = JSON.parse(fs.readFileSync(configPath, "utf8"));
  config.kv_namespaces = [{ binding: "RUNS", id: "cleanup-failure-kv-id" }];
  fs.writeFileSync(configPath, JSON.stringify(config));
  process.exit(0);
}
if (args[0] === "wrangler" && args[1] === "deploy") {
  process.stdout.write("https://crabbox-cfdw-smoke.example.workers.dev\\n");
  process.exit(0);
}
if (args[0] === "wrangler" && args[1] === "kv" && args[2] === "namespace" && args[3] === "delete") {
  process.exit(9);
}
process.exit(0);
`,
  );

  const fakeCrabbox = path.join(dir, "crabbox");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env node
const args = process.argv.slice(2);
if (args[0] === "run" && args.includes("--keep")) {
  process.stdout.write("CRABBOX_CFDW_OK\\n");
  process.stderr.write(JSON.stringify({ leaseId: "cfdw_keep", provider: "cloudflare-dynamic-workers", exitCode: 0 }) + "\\n");
} else if (args[0] === "run") {
  process.stdout.write("CRABBOX_CFDW_EGRESS_BLOCKED\\n");
}
process.exit(0);
`,
  );

  const result = runSmoke({
    PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
    CRABBOX_BIN: fakeCrabbox,
    CRABBOX_FAKE_NPX_CALLS: npxCalls,
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "cloudflare-dynamic-workers",
    CRABBOX_CFDW_SKIP_LOCAL_CHECKS: "1",
    CLOUDFLARE_ACCOUNT_ID: "account",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /environment_blocked/);
  assert.match(result.stdout, /reason=ephemeral_cleanup_failed/);
  assert.doesNotMatch(result.stdout, /live_cloudflare_dynamic_workers_smoke_passed/);

  const seen = fs
    .readFileSync(npxCalls, "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  assert.ok(
    seen.some((args) => args[0] === "wrangler" && args[1] === "delete"),
    `expected Worker cleanup attempt before KV failure in ${JSON.stringify(seen)}`,
  );
});

test("dynamic workers smoke attempts all cleanup after an ambiguous deploy failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-cfdw-smoke-"));
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);
  const npxCalls = path.join(dir, "npx-calls.jsonl");

  writeExecutable(path.join(bin, "npm"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    path.join(bin, "npx"),
    `#!/usr/bin/env node
const fs = require("node:fs");
const args = process.argv.slice(2);
fs.appendFileSync(process.env.CRABBOX_FAKE_NPX_CALLS, JSON.stringify(args) + "\\n");
if (args[0] === "wrangler" && args[1] === "kv" && args[2] === "namespace" && args[3] === "create") {
  if (!args.includes("--update-config")) process.exit(2);
  const configPath = args[args.indexOf("--config") + 1];
  const config = JSON.parse(fs.readFileSync(configPath, "utf8"));
  config.kv_namespaces = [{ binding: "RUNS", id: "partial-kv-id" }];
  fs.writeFileSync(configPath, JSON.stringify(config));
  process.exit(0);
}
if (args[0] === "wrangler" && args[1] === "deploy") {
  const configPath = args[args.indexOf("--config") + 1];
  const config = JSON.parse(fs.readFileSync(configPath, "utf8"));
  const cleanupMigration = config.migrations?.some((migration) =>
    migration.deleted_classes?.includes("DynamicWorkerRunCoordinator")
  );
  if (cleanupMigration) process.stderr.write("cleanup migration also failed\\n");
  process.stderr.write("quota exceeded for dynamic workers\\n");
  process.exit(7);
}
if (args[0] === "wrangler" && args[1] === "delete") {
  process.stderr.write("Worker not found (404)\\n");
  process.exit(1);
}
process.exit(0);
`,
  );

  const fakeCrabbox = path.join(dir, "crabbox");
  writeExecutable(fakeCrabbox, "#!/usr/bin/env bash\nexit 0\n");

  const result = runSmoke({
    PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
    CRABBOX_BIN: fakeCrabbox,
    CRABBOX_FAKE_NPX_CALLS: npxCalls,
    CRABBOX_LIVE: "1",
    CRABBOX_LIVE_PROVIDERS: "cloudflare-dynamic-workers",
    CRABBOX_CFDW_SKIP_LOCAL_CHECKS: "1",
    CLOUDFLARE_ACCOUNT_ID: "account",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /quota_blocked/);
  assert.match(result.stdout, /reason=deploy_failed/);

  const seen = fs
    .readFileSync(npxCalls, "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  const retireDurableObject = seen.findIndex(
    (args) =>
      args[0] === "wrangler" &&
      args[1] === "deploy" &&
      !args.includes("--secrets-file"),
  );
  const deleteWorker = seen.findIndex(
    (args) => args[0] === "wrangler" && args[1] === "delete",
  );
  const deleteKV = seen.findIndex(
    (args) =>
      args[0] === "wrangler" &&
      args[1] === "kv" &&
      args[2] === "namespace" &&
      args[3] === "delete" &&
      args.includes("partial-kv-id"),
  );
  assert.ok(
    retireDurableObject >= 0,
    `expected Durable Object retirement after ambiguous deploy in ${JSON.stringify(seen)}`,
  );
  assert.ok(
    retireDurableObject < deleteWorker,
    `expected Durable Object retirement before Worker deletion in ${JSON.stringify(seen)}`,
  );
  assert.ok(
    deleteWorker < deleteKV,
    `expected Worker deletion before partial KV cleanup in ${JSON.stringify(seen)}`,
  );
  assert.ok(
    deleteKV >= 0,
    `expected partial KV cleanup in ${JSON.stringify(seen)}`,
  );
});
