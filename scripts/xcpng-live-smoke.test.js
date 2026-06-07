import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(scriptDir, "..");

function writeExecutable(file, body) {
  fs.writeFileSync(file, body, "utf8");
  fs.chmodSync(file, 0o755);
}

test("xcpng live smoke env file does not override existing process env", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-xcpng-smoke-test-"));
  const envFile = path.join(dir, "xcpng.env");
  const evidenceDir = path.join(dir, "evidence");
  const fakeCrabbox = path.join(dir, "crabbox");

  fs.writeFileSync(
    envFile,
    "CRABBOX_XCP_NG_USERNAME=stale-env-file-user\n",
    "utf8",
  );
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env node
if (process.argv.slice(2).join(" ") !== "doctor --provider xcp-ng --json") {
  process.stderr.write("unexpected command: " + process.argv.slice(2).join(" ") + "\\n");
  process.exit(2);
}
process.stdout.write(JSON.stringify({
  username: process.env.CRABBOX_XCP_NG_USERNAME,
  status: "ok",
}) + "\\n");
`,
  );

  const result = spawnSync("bash", ["scripts/xcpng-live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      HOME: dir,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_XCP_NG_ENV_FILE: envFile,
      CRABBOX_XCP_NG_SMOKE_DIR: evidenceDir,
      CRABBOX_XCP_NG_USERNAME: "current-env-user",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /classification=read_only_doctor_passed/);

  const doctorLog = result.stdout.match(/^doctor_log=(.+)$/m)?.[1];
  assert.ok(doctorLog, `expected doctor_log in ${result.stdout}`);
  const doctorPath = path.isAbsolute(doctorLog)
    ? doctorLog
    : path.join(repoRoot, doctorLog);
  assert.match(
    fs.readFileSync(doctorPath, "utf8"),
    /current-env-user/,
  );
});
