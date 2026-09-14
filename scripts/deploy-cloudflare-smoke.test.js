import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { writeExecutable } from "./test-support/smoke-fixtures.mjs";

const root = path.resolve(import.meta.dirname, "..");

function createFixture(t, crabboxBody) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-cf-smoke-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);
  const calls = path.join(dir, "calls.jsonl");
  for (const command of ["go", "npm"]) {
    writeExecutable(path.join(bin, command), "#!/usr/bin/env node\nprocess.exit(0);\n");
  }
  const fakeCrabbox = path.join(dir, "crabbox");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env node
const fs = require("node:fs");
const args = process.argv.slice(2);
fs.appendFileSync(process.env.CRABBOX_FAKE_CALLS, JSON.stringify(args) + "\\n");
${crabboxBody}
process.exit(0);
`,
  );
  return { dir, bin, calls, fakeCrabbox };
}

function runFixture({ dir, bin, calls, fakeCrabbox }, env = {}) {
  return spawnSync("bash", ["scripts/deploy-cloudflare-smoke.sh"], {
    cwd: root,
    env: {
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      HOME: process.env.HOME ?? dir,
      TMPDIR: process.env.TMPDIR ?? os.tmpdir(),
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_FAKE_CALLS: calls,
      CRABBOX_CLOUDFLARE_SKIP_DEPLOY: "1",
      CRABBOX_CLOUDFLARE_SKIP_SMOKE: "0",
      CRABBOX_LIVE_REPO: root,
      CRABBOX_CLOUDFLARE_RUNNER_URL: "https://runner.example.test",
      CRABBOX_CLOUDFLARE_RUNNER_TOKEN: "token",
      ...env,
    },
    encoding: "utf8",
  });
}

function readCalls({ calls }) {
  return fs.readFileSync(calls, "utf8").trim().split("\n").map((line) => JSON.parse(line));
}

test("deploy-cloudflare-smoke ignores lease-like stderr from failed kept run", (t) => {
  const fixture = createFixture(t, `
if (args[0] === "run" && args.includes("--keep")) {
  process.stderr.write("leased cbx_keep from diagnostic stderr\\n");
  process.exit(7);
}
`);
  const result = runFixture(fixture);

  assert.equal(result.status, 7, result.stderr || result.stdout);
  const seen = readCalls(fixture);
  assert.equal(
    seen.some((args) =>
      JSON.stringify(args) ===
      JSON.stringify(["stop", "--provider", "cloudflare", "cbx_keep"]),
    ),
    false,
    `diagnostic stderr should not be parsed as a cleanup lease in ${JSON.stringify(seen)}`,
  );
});

test("deploy-cloudflare-smoke stops kept lease from failed timing JSON", (t) => {
  const fixture = createFixture(t, `
if (args[0] === "run" && args.includes("--keep")) {
  process.stderr.write(JSON.stringify({ leaseId: "cbx_keep", provider: "cloudflare", exitCode: 7 }) + "\\n");
  process.exit(7);
}
`);
  const result = runFixture(fixture);

  assert.equal(result.status, 7, result.stderr || result.stdout);
  const seen = readCalls(fixture);
  assert.ok(
    seen.some((args) =>
      JSON.stringify(args) ===
      JSON.stringify(["stop", "--provider", "cloudflare", "cbx_keep"]),
    ),
    `expected trap stop call in ${JSON.stringify(seen)}`,
  );
});

test("deploy-cloudflare-smoke stops kept lease from failed lease record", (t) => {
  const fixture = createFixture(t, `
if (args[0] === "run" && args.includes("--keep")) {
  process.stderr.write("leased cbx_keep slug=blue-box provider=cloudflare sandbox=cbx_keep\\n");
  process.exit(7);
}
`);
  const result = runFixture(fixture);

  assert.equal(result.status, 7, result.stderr || result.stdout);
  const seen = readCalls(fixture);
  assert.ok(
    seen.some((args) =>
      JSON.stringify(args) ===
      JSON.stringify(["stop", "--provider", "cloudflare", "cbx_keep"]),
    ),
    `expected trap stop call in ${JSON.stringify(seen)}`,
  );
});

test("deploy-cloudflare-smoke stops kept lease parsed from timing JSON", (t) => {
  const fixture = createFixture(t, `
if (args[0] === "run" && args.includes("--keep")) {
  process.stderr.write("cloudflare warning on stderr\\n");
  process.stdout.write("CRABBOX_CF_KEEP_OK\\n");
  process.stderr.write(JSON.stringify({ leaseId: "cbx_keep", provider: "cloudflare", exitCode: 0 }) + "\\n");
}
`);
  const result = runFixture(fixture);

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stderr, /cloudflare warning on stderr/);
  const seen = readCalls(fixture);
  assert.ok(
    seen.some((args) =>
      JSON.stringify(args) ===
      JSON.stringify(["stop", "--provider", "cloudflare", "cbx_keep"]),
    ),
    `expected explicit stop call in ${JSON.stringify(seen)}`,
  );
});

test("deploy-cloudflare-smoke does not run kept smoke from wrong repo", (t) => {
  const fixture = createFixture(t, `
if (args[0] === "run" && !args.includes("--keep")) {
  fs.rmSync(process.env.CRABBOX_FAKE_LIVE_REPO, { recursive: true, force: true });
}
`);
  const liveRepo = path.join(fixture.dir, "live-repo");
  fs.mkdirSync(liveRepo);
  const result = runFixture(fixture, {
    CRABBOX_FAKE_LIVE_REPO: liveRepo,
    CRABBOX_LIVE_REPO: liveRepo,
  });

  assert.notEqual(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout + result.stderr, /live-repo/);
  const seen = readCalls(fixture);
  assert.ok(seen.some((args) => args[0] === "run" && !args.includes("--keep")), "expected initial no-sync run");
  assert.equal(seen.some((args) => args[0] === "run" && args.includes("--keep")), false, "kept run should not execute after repo disappears");
});
