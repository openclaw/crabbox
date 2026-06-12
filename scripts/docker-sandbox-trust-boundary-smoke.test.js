import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const tokenValue = "crabbox-trust-boundary-proof-value";

test("docker sandbox trust boundary smoke records sbx handoff without argv secret leak", () => {
  const proofDir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-trust-boundary-test-"));
  const result = spawnSync("bash", ["scripts/docker-sandbox-trust-boundary-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_DOCKER_SANDBOX_TRUST_PROOF_DIR: proofDir,
      CRABBOX_KEEP_TRUST_BOUNDARY_PROOF: "1",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=trust_boundary_proof_passed/);
  assert.match(result.stdout, /workspace_sent=true/);
  assert.match(result.stdout, /command_sent=true/);
  assert.match(result.stdout, /env_file_sent=true/);
  assert.match(result.stdout, /env_value_in_argv=false/);
  assert.match(result.stdout, /env_value_received=true/);

  const execArgs = fs.readFileSync(path.join(proofDir, "exec.args"), "utf8");
  const envSnapshot = fs.readFileSync(path.join(proofDir, "env-file.snapshot"), "utf8");
  const execArgLines = execArgs.trim().split("\n");
  const envFileIndex = execArgLines.indexOf("--env-file");
  const workdirIndex = execArgLines.indexOf("--workdir");
  assert.notEqual(envFileIndex, -1);
  assert.notEqual(workdirIndex, -1);
  const sandboxName = execArgLines.at(-3);
  assert.match(sandboxName, /^crabbox-project-with-spaces-/);
  assert.deepEqual(execArgLines.slice(-2), ["printenv", "CRABBOX_TRUST_BOUNDARY_TOKEN"]);
  assert.doesNotMatch(execArgs, new RegExp(tokenValue));
  assert.match(envSnapshot, new RegExp(`CRABBOX_TRUST_BOUNDARY_TOKEN=${tokenValue}`));

  const fakeSbx = path.join(proofDir, "sbx");
  const envFile = path.join(proofDir, "bad.env");
  fs.writeFileSync(envFile, `CRABBOX_TRUST_BOUNDARY_TOKEN=${tokenValue}\n`, "utf8");
  const fakeEnv = {
    ...process.env,
    CRABBOX_FAKE_SBX_LOG_DIR: proofDir,
    CRABBOX_FAKE_SBX_WORKSPACE: execArgLines[workdirIndex + 1],
  };
  const malformedExecs = [
    ["exec", "--workdir", fakeEnv.CRABBOX_FAKE_SBX_WORKSPACE, "--env-file", envFile, "printenv", "CRABBOX_TRUST_BOUNDARY_TOKEN", sandboxName],
    ["exec", "--workdir", fakeEnv.CRABBOX_FAKE_SBX_WORKSPACE, "--env-file", envFile, sandboxName],
    ["exec", "--workdir", fakeEnv.CRABBOX_FAKE_SBX_WORKSPACE, sandboxName, "--env-file", envFile, "printenv", "CRABBOX_TRUST_BOUNDARY_TOKEN"],
  ];
  for (const args of malformedExecs) {
    const bad = spawnSync(fakeSbx, args, {
      cwd: repoRoot,
      env: fakeEnv,
      encoding: "utf8",
    });
    assert.notEqual(bad.status, 0, `malformed fake exec unexpectedly passed: ${args.join(" ")}`);
    assert.doesNotMatch(bad.stdout, new RegExp(tokenValue));
  }
});
