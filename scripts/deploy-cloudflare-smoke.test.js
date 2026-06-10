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

test("deploy-cloudflare-smoke stops a kept lease after failed kept run", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-cf-smoke-"));
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);
  const calls = path.join(dir, "calls.jsonl");

  writeExecutable(
    path.join(bin, "go"),
    `#!/usr/bin/env node
process.exit(0);
`,
  );
  writeExecutable(
    path.join(bin, "npm"),
    `#!/usr/bin/env node
process.exit(0);
`,
  );
  const fakeCrabbox = path.join(dir, "crabbox");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env node
const fs = require("node:fs");
const calls = process.env.CRABBOX_FAKE_CALLS;
const args = process.argv.slice(2);
fs.appendFileSync(calls, JSON.stringify(args) + "\\n");
if (args[0] === "run" && args.includes("--keep")) {
  process.stderr.write("leased cbx_keep slug=blue-lobster provider=cloudflare sandbox=cbx_keep\\n");
  process.exit(7);
}
process.exit(0);
`,
  );

  const result = spawnSync("bash", ["scripts/deploy-cloudflare-smoke.sh"], {
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
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 7, result.stderr || result.stdout);
  const seen = fs
    .readFileSync(calls, "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  assert.ok(
    seen.some((args) =>
      JSON.stringify(args) ===
      JSON.stringify(["stop", "--provider", "cloudflare", "cbx_keep"]),
    ),
    `expected cleanup stop call in ${JSON.stringify(seen)}`,
  );
});

test("deploy-cloudflare-smoke does not run kept smoke from wrong repo", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-cf-smoke-"));
  const bin = path.join(dir, "bin");
  fs.mkdirSync(bin);
  const liveRepo = path.join(dir, "live-repo");
  fs.mkdirSync(liveRepo);
  const calls = path.join(dir, "calls.jsonl");

  writeExecutable(
    path.join(bin, "go"),
    `#!/usr/bin/env node
process.exit(0);
`,
  );
  writeExecutable(
    path.join(bin, "npm"),
    `#!/usr/bin/env node
process.exit(0);
`,
  );
  const fakeCrabbox = path.join(dir, "crabbox");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env node
const fs = require("node:fs");
const args = process.argv.slice(2);
fs.appendFileSync(process.env.CRABBOX_FAKE_CALLS, JSON.stringify(args) + "\\n");
if (args[0] === "run" && !args.includes("--keep")) {
  fs.rmSync(process.env.CRABBOX_FAKE_LIVE_REPO, { recursive: true, force: true });
}
process.exit(0);
`,
  );

  const result = spawnSync("bash", ["scripts/deploy-cloudflare-smoke.sh"], {
    cwd: root,
    env: {
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      HOME: process.env.HOME ?? dir,
      TMPDIR: process.env.TMPDIR ?? os.tmpdir(),
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_FAKE_CALLS: calls,
      CRABBOX_FAKE_LIVE_REPO: liveRepo,
      CRABBOX_CLOUDFLARE_SKIP_DEPLOY: "1",
      CRABBOX_CLOUDFLARE_SKIP_SMOKE: "0",
      CRABBOX_LIVE_REPO: liveRepo,
      CRABBOX_CLOUDFLARE_RUNNER_URL: "https://runner.example.test",
      CRABBOX_CLOUDFLARE_RUNNER_TOKEN: "token",
    },
    encoding: "utf8",
  });

  assert.notEqual(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout + result.stderr, /live-repo/);
  const seen = fs
    .readFileSync(calls, "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  assert.ok(seen.some((args) => args[0] === "run" && !args.includes("--keep")), "expected initial no-sync run");
  assert.equal(seen.some((args) => args[0] === "run" && args.includes("--keep")), false, "kept run should not execute after repo disappears");
});
