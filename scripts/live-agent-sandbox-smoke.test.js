import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");

function writeExecutable(file, body) {
  fs.writeFileSync(file, body, "utf8");
  fs.chmodSync(file, 0o755);
}

test("Agent Sandbox smoke is non-mutating without live opt-in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-agent-sandbox-no-live-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
printf '%s\\n' "$*" >>"${crabboxLog}"
exit 99
`,
  );

  const result = spawnSync("bash", ["scripts/live-agent-sandbox-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "0",
      CRABBOX_LIVE_PROVIDERS: "agent-sandbox",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^environment_blocked reason=CRABBOX_LIVE_not_enabled/m);
  assert.equal(fs.existsSync(crabboxLog), false);
});

test("Agent Sandbox smoke requires explicit provider selection", () => {
  const result = spawnSync("bash", ["scripts/live-agent-sandbox-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "aws",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^environment_blocked reason=provider_not_selected/m);
});

test("Agent Sandbox smoke classifies missing kubeconfig before mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-agent-sandbox-missing-kubeconfig-"));
  const home = path.join(dir, "home");
  fs.mkdirSync(home);
  const fakeCrabbox = path.join(dir, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
printf '%s\\n' "$*" >>"${crabboxLog}"
exit 99
`,
  );

  const result = spawnSync("bash", ["scripts/live-agent-sandbox-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      HOME: home,
      KUBECONFIG: "",
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "agent-sandbox",
      CRABBOX_AGENT_SANDBOX_CONTEXT: "agent-context",
      CRABBOX_AGENT_SANDBOX_WARM_POOL: "linux-pool",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^environment_blocked reason=missing_kubeconfig/m);
  assert.equal(fs.existsSync(crabboxLog), false);
});

test("Agent Sandbox smoke stops only the created slug", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-agent-sandbox-success-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  const kubeconfig = path.join(dir, "kubeconfig");
  fs.mkdirSync(bin);
  fs.writeFileSync(kubeconfig, "apiVersion: v1\nkind: Config\n", "utf8");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${crabboxLog}"
case "$1" in
  doctor)
    printf 'agent-sandbox ready\\n'
    ;;
  run)
    printf 'leased asbx_123456789abc slug=agent-sandbox-smoke-test-collision provider=agent-sandbox claim=crabbox-agent-sandbox-smoke-test-collision sandbox=sandbox-a pod=pod-a\\n' >&2
    printf 'AGENT_SANDBOX_SMOKE_OK\\n'
    ;;
  status)
    printf 'ready\\n'
    ;;
  list)
    printf '[]\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-agent-sandbox-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "agent-sandbox",
      CRABBOX_AGENT_SANDBOX_KUBECONFIG: kubeconfig,
      CRABBOX_AGENT_SANDBOX_CONTEXT: "agent-context",
      CRABBOX_AGENT_SANDBOX_NAMESPACE: "sandboxes",
      CRABBOX_AGENT_SANDBOX_WARM_POOL: "linux-pool",
      CRABBOX_AGENT_SANDBOX_SMOKE_SLUG: "agent-sandbox-smoke-test",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^live_agent_sandbox_smoke_passed$/m);

  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /doctor --provider agent-sandbox/);
  assert.match(crabboxCalls, /run --provider agent-sandbox/);
  assert.match(crabboxCalls, /--slug agent-sandbox-smoke-test/);
  assert.match(crabboxCalls, /status --provider agent-sandbox .* --id asbx_123456789abc/);
  assert.match(crabboxCalls, /stop --provider agent-sandbox .* asbx_123456789abc/);
  assert.doesNotMatch(crabboxCalls, /stop --provider agent-sandbox .* agent-sandbox-smoke-test$/m);
  assert.doesNotMatch(crabboxCalls, /stop --provider agent-sandbox .*other/);
});

test("Agent Sandbox smoke cleanup reuses resolved provider args", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-agent-sandbox-cleanup-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  const kubeconfig = path.join(dir, "kubeconfig");
  const config = path.join(dir, "crabbox.yaml");
  fs.mkdirSync(bin);
  fs.writeFileSync(kubeconfig, "apiVersion: v1\nkind: Config\n", "utf8");
  fs.writeFileSync(
    config,
    `agentSandbox:
  kubeconfig: ${JSON.stringify(kubeconfig)}
  context: config-context
  namespace: config-namespace
  warmPool: config-pool
`,
    "utf8",
  );
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${crabboxLog}"
case "$1" in
  doctor)
    printf 'agent-sandbox ready\\n'
    ;;
  run)
    printf 'leased asbx_cleanup123 slug=agent-sandbox-smoke-cleanup provider=agent-sandbox claim=crabbox-agent-sandbox-smoke-cleanup sandbox=sandbox-a pod=pod-a\\n' >&2
    printf 'simulated run failure\\n' >&2
    exit 7
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-agent-sandbox-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: config,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "agent-sandbox",
      CRABBOX_AGENT_SANDBOX_SMOKE_SLUG: "agent-sandbox-smoke-cleanup",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^diagnostic_only leased asbx_cleanup123/m);
  assert.match(result.stdout, /simulated run failure/m);

  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(
    crabboxCalls,
    /stop --provider agent-sandbox --agent-sandbox-kubeconfig \S+ --agent-sandbox-context config-context --agent-sandbox-namespace config-namespace --agent-sandbox-warm-pool config-pool/,
  );
  assert.match(crabboxCalls, /--agent-sandbox-forget-missing asbx_cleanup123/);
});
