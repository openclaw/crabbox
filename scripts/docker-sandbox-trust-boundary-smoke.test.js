import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");

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
  assert.match(execArgs, /--env-file/);
  assert.match(execArgs, /printenv/);
  assert.doesNotMatch(execArgs, /crabbox-trust-boundary-proof-value/);
  assert.match(envSnapshot, /CRABBOX_TRUST_BOUNDARY_TOKEN=crabbox-trust-boundary-proof-value/);
});
