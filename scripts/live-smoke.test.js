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

test("OpenSandbox live smoke dispatches to the provider-specific script", () => {
  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "opensandbox",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /environment_blocked missing CRABBOX_OPENSANDBOX_API_KEY or OPEN_SANDBOX_API_KEY/);
  assert.match(result.stderr, /admin active-lease check skipped/);
});

test("Proxmox live smoke dispatches to the provider-specific proof script", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-proxmox-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const proof = path.join(dir, "proof");
  const log = path.join(dir, "calls.log");
  fs.mkdirSync(proof, { mode: 0o700 });
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
case "$1" in
  config)
    printf '{}\\n'
    ;;
  doctor)
    printf '{"ok":true,"provider":"proxmox","checks":[]}\\n'
    ;;
  list)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "proxmox",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_PROXMOX_LIVE_SMOKE_DIR: proof,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=external_user_owned/);
  assert.match(result.stdout, /proof_dir=<proof-dir>/);
  assert.match(result.stderr, /admin active-lease check skipped/);

  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^doctor --provider proxmox --json$/m);
  assert.match(calls, /^list --provider proxmox --json$/m);
  assert.doesNotMatch(calls, /^warmup /m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("XCP-ng live smoke dispatches to the provider-specific read-only proof script", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-xcpng-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const evidence = path.join(dir, "evidence");
  const log = path.join(dir, "calls.log");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
case "$*" in
  "config path")
    exit 0
    ;;
  "config show --json")
    printf '{}\\n'
    ;;
  "doctor --provider xcp-ng --json")
    printf '{"ok":true,"provider":"xcp-ng","checks":[]}\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "xcp-ng",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_XCP_NG_SMOKE_DIR: evidence,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=read_only_doctor_passed/);
  assert.match(result.stdout, /mutation=false/);
  assert.match(result.stderr, /admin active-lease check skipped/);

  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^doctor --provider xcp-ng --json$/m);
  assert.doesNotMatch(calls, /^warmup /m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("Phala live smoke dispatches to the provider-specific confidential VM script", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-phala-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const log = path.join(dir, "calls.log");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
case "$1" in
  doctor)
    printf 'phala status failed: not logged in; run phala login\\n' >&2
    exit 1
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "phala",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^environment_blocked .*not logged in/m);
  assert.match(result.stderr, /admin active-lease check skipped/);
  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^doctor --provider phala$/m);
  assert.doesNotMatch(calls, /^list --provider phala/m);
  assert.doesNotMatch(calls, /^warmup /m);
  assert.doesNotMatch(calls, /^run /m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("Agent Sandbox live smoke dispatches to the provider-specific Kubernetes script", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-agent-sandbox-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const home = path.join(dir, "home");
  const log = path.join(dir, "calls.log");
  fs.mkdirSync(home);
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
printf 'unexpected crabbox args: %s\\n' "$*" >&2
exit 99
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "agent-sandbox",
      CRABBOX_LIVE_REPO: repoRoot,
      HOME: home,
      KUBECONFIG: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^environment_blocked reason=missing_kubeconfig/m);
  assert.match(result.stderr, /admin active-lease check skipped/);
  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^config path$/m);
  assert.doesNotMatch(calls, /^doctor --provider agent-sandbox/m);
  assert.doesNotMatch(calls, /^warmup /m);
  assert.doesNotMatch(calls, /^run /m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("Scaleway live smoke dispatches to the provider-specific script", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-scaleway-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const log = path.join(dir, "calls.log");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
printf 'unexpected crabbox args: %s\\n' "$*" >&2
exit 99
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "scaleway",
      CRABBOX_LIVE_REPO: repoRoot,
      SCW_ACCESS_KEY: "",
      SCW_SECRET_KEY: "",
      SCW_DEFAULT_ORGANIZATION_ID: "",
      SCW_DEFAULT_PROJECT_ID: "",
      SCW_DEFAULT_REGION: "",
      SCW_DEFAULT_ZONE: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /^classification=environment_blocked reason=SCW_ACCESS_KEY_missing/m);
  assert.match(result.stderr, /admin active-lease check skipped/);
  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^config path$/m);
  assert.doesNotMatch(calls, /^doctor --provider scaleway/m);
  assert.doesNotMatch(calls, /^warmup /m);
  assert.doesNotMatch(calls, /^run /m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("KubeVirt live smoke requires an explicit VM template before provider mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-kubevirt-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(dir, "crabbox");
  const log = path.join(dir, "calls.log");
  fs.mkdirSync(bin);
  writeExecutable(path.join(bin, "kubectl"), "#!/usr/bin/env bash\nexit 99\n");
  writeExecutable(path.join(bin, "virtctl"), "#!/usr/bin/env bash\nexit 99\n");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
case "$*" in
  "config path")
    exit 0
    ;;
  "config show --json")
    printf '{}\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "kubevirt",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_LIVE_KUBEVIRT_TEMPLATE: "",
      CRABBOX_KUBEVIRT_TEMPLATE: "",
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(
    result.stderr,
    /kubevirt smoke requires CRABBOX_LIVE_KUBEVIRT_TEMPLATE, CRABBOX_KUBEVIRT_TEMPLATE, or kubevirt\.template/,
  );
  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^config path$/m);
  assert.doesNotMatch(calls, /^doctor --provider kubevirt/m);
  assert.doesNotMatch(calls, /^warmup /m);
  assert.doesNotMatch(calls, /^run /m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("Daytona live smoke requires an explicit snapshot before provider mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-daytona-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const log = path.join(dir, "calls.log");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
case "$*" in
  "config path")
    exit 0
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_DAYTONA_SNAPSHOT: "",
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "daytona",
      CRABBOX_LIVE_REPO: repoRoot,
      DAYTONA_SNAPSHOT: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(
    result.stderr,
    /daytona smoke requires CRABBOX_DAYTONA_SNAPSHOT, DAYTONA_SNAPSHOT, or daytona\.snapshot/,
  );
  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^config path$/m);
  assert.doesNotMatch(calls, /^run --provider daytona/m);
  assert.doesNotMatch(calls, /^list --provider daytona/m);
  assert.doesNotMatch(calls, /^warmup /m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("Namespace Devbox live smoke requires the devbox CLI before provider mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-namespace-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(dir, "crabbox");
  const log = path.join(dir, "calls.log");
  fs.mkdirSync(bin);
  writeExecutable(
    path.join(bin, "dirname"),
    `#!/bin/bash
case "$1" in
  */*) printf '%s\\n' "\${1%/*}" ;;
  *) printf '.\\n' ;;
esac
`,
  );
  writeExecutable(path.join(bin, "jq"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    fakeCrabbox,
    `#!/bin/bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
case "$*" in
  "config path")
    exit 0
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("/bin/bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "namespace-devbox",
      CRABBOX_LIVE_REPO: repoRoot,
      PATH: bin,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(result.stderr, /namespace-devbox smoke requires the Namespace devbox CLI on PATH/);
  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^config path$/m);
  assert.doesNotMatch(calls, /^run --provider namespace-devbox/m);
  assert.doesNotMatch(calls, /^list --provider namespace-devbox/m);
  assert.doesNotMatch(calls, /^warmup /m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("Semaphore live smoke requires host before provider mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-semaphore-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const log = path.join(dir, "calls.log");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
case "$*" in
  "config path")
    exit 0
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "semaphore",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_SEMAPHORE_HOST: "",
      CRABBOX_SEMAPHORE_PROJECT: "",
      CRABBOX_SEMAPHORE_TOKEN: "",
      SEMAPHORE_API_TOKEN: "",
      SEMAPHORE_HOST: "",
      SEMAPHORE_PROJECT: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(
    result.stderr,
    /semaphore smoke requires CRABBOX_SEMAPHORE_HOST, SEMAPHORE_HOST, or semaphore\.host/,
  );
  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^config path$/m);
  assert.doesNotMatch(calls, /^warmup --provider semaphore/m);
  assert.doesNotMatch(calls, /^run --provider semaphore/m);
  assert.doesNotMatch(calls, /^list --provider semaphore/m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("Sprites live smoke requires the sprite CLI before provider mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-sprites-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(dir, "crabbox");
  const log = path.join(dir, "calls.log");
  fs.mkdirSync(bin);
  writeExecutable(path.join(bin, "jq"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(path.join(bin, "rg"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
case "$*" in
  "config path")
    exit 0
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "sprites",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_SPRITES_TOKEN: "",
      PATH: `${bin}${path.delimiter}/usr/bin${path.delimiter}/bin`,
      SETUP_SPRITE_TOKEN: "",
      SPRITE_TOKEN: "",
      SPRITES_TOKEN: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(
    result.stderr,
    /sprites smoke requires the authenticated Sprites sprite CLI on PATH/,
  );
  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^config path$/m);
  assert.doesNotMatch(calls, /^warmup --provider sprites/m);
  assert.doesNotMatch(calls, /^status --provider sprites/m);
  assert.doesNotMatch(calls, /^ssh --provider sprites/m);
  assert.doesNotMatch(calls, /^run --provider sprites/m);
  assert.doesNotMatch(calls, /^list --provider sprites/m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("W&B live smoke requires an API key before provider mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-wandb-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(dir, "crabbox");
  const log = path.join(dir, "calls.log");
  fs.mkdirSync(bin);
  writeExecutable(
    path.join(bin, "dirname"),
    `#!/bin/bash
case "$1" in
  */*) printf '%s\\n' "\${1%/*}" ;;
  *) printf '.\\n' ;;
esac
`,
  );
  writeExecutable(path.join(bin, "jq"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    fakeCrabbox,
    `#!/bin/bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
case "$*" in
  "config path")
    exit 0
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "wandb",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_WANDB_API_KEY: "",
      WANDB_API_KEY: "",
      WANDB_ENTITY_NAME: "example-org",
      PATH: `${bin}${path.delimiter}/usr/bin${path.delimiter}/bin`,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(
    result.stderr,
    /wandb smoke requires CRABBOX_WANDB_API_KEY or WANDB_API_KEY/,
  );
  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^config path$/m);
  assert.doesNotMatch(calls, /^doctor --provider wandb/m);
  assert.doesNotMatch(calls, /^run --provider wandb/m);
  assert.doesNotMatch(calls, /^list --provider wandb/m);
});

test("Incus live smoke requires rg before provider mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-incus-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(dir, "crabbox");
  const log = path.join(dir, "calls.log");
  fs.mkdirSync(bin);
  writeExecutable(path.join(bin, "jq"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
case "$*" in
  "config path")
    exit 0
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("/bin/bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "incus",
      CRABBOX_LIVE_REPO: repoRoot,
      HOME: dir,
      PATH: bin,
      TMPDIR: dir,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(result.stderr, /missing required tool: rg/);
  const calls = fs.existsSync(log) ? fs.readFileSync(log, "utf8") : "";
  assert.doesNotMatch(calls, /^doctor --provider incus/m);
  assert.doesNotMatch(calls, /^warmup --provider incus/m);
  assert.doesNotMatch(calls, /^status --provider incus/m);
  assert.doesNotMatch(calls, /^run --provider incus/m);
  assert.doesNotMatch(calls, /^list --provider incus/m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("E2B live smoke requires an API key before provider mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-e2b-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(dir, "crabbox");
  const log = path.join(dir, "calls.log");
  fs.mkdirSync(bin);
  writeExecutable(path.join(bin, "jq"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(path.join(bin, "rg"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
case "$*" in
  "config path")
    exit 0
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_E2B_API_KEY: "",
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "e2b",
      CRABBOX_LIVE_REPO: repoRoot,
      E2B_API_KEY: "",
      PATH: `${bin}${path.delimiter}/usr/bin${path.delimiter}/bin`,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(
    result.stderr,
    /e2b smoke requires CRABBOX_E2B_API_KEY or E2B_API_KEY/,
  );
  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^config path$/m);
  assert.doesNotMatch(calls, /^warmup --provider e2b/m);
  assert.doesNotMatch(calls, /^status --provider e2b/m);
  assert.doesNotMatch(calls, /^run --provider e2b/m);
  assert.doesNotMatch(calls, /^list --provider e2b/m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("Modal live smoke requires the Python client before provider mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-modal-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(dir, "crabbox");
  const log = path.join(dir, "calls.log");
  fs.mkdirSync(bin);
  writeExecutable(path.join(bin, "jq"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(path.join(bin, "rg"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    path.join(bin, "python3"),
    `#!/usr/bin/env bash
printf 'missing modal client\\n' >&2
exit 1
`,
  );
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${log}"
case "$*" in
  "config path")
    exit 0
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "modal",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_MODAL_PYTHON: "",
      PATH: `${bin}${path.delimiter}/usr/bin${path.delimiter}/bin`,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(
    result.stderr,
    /modal smoke requires the Modal Python client for python3/,
  );
  const calls = fs.readFileSync(log, "utf8");
  assert.match(calls, /^config path$/m);
  assert.doesNotMatch(calls, /^warmup --provider modal/m);
  assert.doesNotMatch(calls, /^status --provider modal/m);
  assert.doesNotMatch(calls, /^run --provider modal/m);
  assert.doesNotMatch(calls, /^list --provider modal/m);
  assert.doesNotMatch(calls, /^stop /m);
});

test("Tenki live smoke proves paused status waits do not resume the session", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-tenki-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const fakeTenki = path.join(bin, "tenki");
  const crabboxLog = path.join(dir, "crabbox.log");
  const tenkiLog = path.join(dir, "tenki.log");
  const stateFile = path.join(dir, "tenki-state");
  fs.mkdirSync(bin);
  fs.writeFileSync(stateFile, "RUNNING\n", "utf8");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  config)
    exit 0
    ;;
  doctor)
    printf 'ok provider=tenki\\n'
    ;;
  warmup)
    printf 'provisioning provider=tenki lease=cbx_123456789abc slug=tenki-smoke-test session=crabbox-tenki-smoke-test keep=true\\n'
    printf 'provisioned lease=cbx_123456789abc tenki_session=00000000-0000-0000-0000-000000000001 state=ready\\n'
    ;;
  status)
    if [[ "$*" == *"--wait-timeout 2s"* ]]; then
      printf 'timed out waiting for lease tenki-smoke-test to become ready\\n' >&2
      exit 5
    fi
    printf 'lease=cbx_123456789abc slug=tenki-smoke-test provider=tenki state=ready ready=true\\n'
    ;;
  run)
    printf 'crabbox-tenki-ok\\n'
    ;;
  list)
    printf 'crabbox list warning\\n' >&2
    printf '[{"id":"cbx_123456789abc","serverId":"00000000-0000-0000-0000-000000000001","slug":"tenki-smoke-test","provider":"tenki","state":"ready"}]\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  writeExecutable(
    fakeTenki,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${TENKI_FAKE_LOG:?}"
case "$1" in
  --version)
    printf 'tenki version v0.test\\n'
    ;;
  status)
    printf 'tenki status warning\\n' >&2
    printf '{"status":"Logged in (API key)","api_endpoint":"https://api.tenki.test","workspace_id":"ws_test","project_id":"proj_test"}\\n'
    ;;
  sandbox)
    case "$2" in
      pause)
        printf 'PAUSED\\n' >"\${TENKI_FAKE_STATE:?}"
        printf '{"state":"PAUSED"}\\n'
        ;;
      get)
        printf '{"id":"00000000-0000-0000-0000-000000000001","state":"%s"}\\n' "$(tr -d '\\n' <"\${TENKI_FAKE_STATE:?}")"
        ;;
      *)
        printf 'unexpected tenki sandbox args: %s\\n' "$*" >&2
        exit 98
        ;;
    esac
    ;;
  *)
    printf 'unexpected tenki args: %s\\n' "$*" >&2
    exit 97
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "tenki",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_TENKI_ENDPOINT: "https://sandbox.tenki.test",
      TENKI_CLI: fakeTenki,
      TENKI_FAKE_LOG: tenkiLog,
      TENKI_FAKE_STATE: stateFile,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-tenki-ok/);
  assert.match(result.stdout, /paused-session readiness check preserved state=paused/);
  assert.match(result.stderr, /tenki status warning/);
  assert.match(result.stderr, /crabbox list warning/);

  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /doctor --provider tenki/);
  assert.match(crabboxCalls, /warmup --provider tenki --slug tenki-smoke-/);
  assert.match(crabboxCalls, /status --provider tenki --id tenki-smoke-test --wait --wait-timeout 120s/);
  assert.match(crabboxCalls, /run --provider tenki --id tenki-smoke-test --no-sync -- echo crabbox-tenki-ok/);
  assert.match(crabboxCalls, /list --provider tenki --json/);
  assert.match(crabboxCalls, /status --provider tenki --id tenki-smoke-test --wait --wait-timeout 2s/);
  assert.match(crabboxCalls, /stop --provider tenki tenki-smoke-test/);

  const tenkiCalls = fs.readFileSync(tenkiLog, "utf8");
  assert.match(
    tenkiCalls,
    /sandbox pause --endpoint https:\/\/sandbox\.tenki\.test --session 00000000-0000-0000-0000-000000000001/,
  );
  assert.match(
    tenkiCalls,
    /sandbox get --endpoint https:\/\/sandbox\.tenki\.test --output json 00000000-0000-0000-0000-000000000001/,
  );
  assert.doesNotMatch(tenkiCalls, /sandbox resume/);
  assert.equal(fs.readFileSync(stateFile, "utf8").trim(), "PAUSED");
});

test("blacksmith live smoke requires an explicit organization", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-blacksmith-missing-org-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const config = path.join(dir, "crabbox.yaml");
  fs.mkdirSync(bin);
  fs.writeFileSync(
    config,
    `blacksmith:
  workflow: .github/workflows/blacksmith-testbox.yml
  job: go
`,
    "utf8",
  );
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf 'unexpected crabbox args: %s\\n' "$*" >&2
exit 99
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_BLACKSMITH_ORG: "",
      CRABBOX_CONFIG: config,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "blacksmith-testbox",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(result.stderr, /requires CRABBOX_BLACKSMITH_ORG, blacksmith\.org, or actions\.repo/);
});

test("blacksmith live smoke derives organization from actions repo", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-blacksmith-actions-org-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  const config = path.join(dir, "crabbox.yaml");
  fs.mkdirSync(bin);
  fs.writeFileSync(
    config,
    `blacksmith:
  workflow: .github/workflows/blacksmith-testbox.yml
  job: go
actions:
  repo: example-org/my-app
`,
    "utf8",
  );
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  list)
    printf '[]\\n'
    ;;
  run)
    printf 'blacksmith-crabbox-ok\\n'
    ;;
  admin)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_BLACKSMITH_ORG: "",
      CRABBOX_CONFIG: config,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "blacksmith-testbox",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /blacksmith-crabbox-ok/);
  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /run --provider blacksmith-testbox --blacksmith-org example-org/);
});

test("external live smoke accepts declarative lifecycle configuration", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-external-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  const config = path.join(dir, "crabbox.yaml");
  fs.mkdirSync(bin);
  fs.writeFileSync(
    config,
    `provider: external
external:
  lifecycle:
    acquire:
      argv: [devboxctl, new, "{{name}}"]
    list:
      argv: [devboxctl, list]
      output: json-name-array
    release:
      argv: [devboxctl, rm, "{{name}}"]
  connection:
    ssh:
      user: developer
      host: "{{name}}"
`,
    "utf8",
  );

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf 'external_command=%s args=%s\\n' "\${CRABBOX_EXTERNAL_COMMAND:-}" "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  doctor)
    printf 'ok provider=external\\n'
    ;;
  warmup)
    printf 'provisioning provider=external lease=cbx_123456789abc slug=external-smoke-test\\n'
    ;;
  status)
    printf 'lease=cbx_123456789abc slug=external-smoke-test provider=external state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_123456789abc","slug":"external-smoke-test","provider":"external","state":"ready"}\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  list)
    printf '[{"id":"cbx_123456789abc","slug":"external-smoke-test","provider":"external","state":"ready"}]\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: config,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "external",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /args=doctor --provider external/);
  assert.match(crabboxCalls, /args=warmup --provider external/);
  assert.match(crabboxCalls, /args=stop --provider external external-smoke-test/);
  assert.doesNotMatch(crabboxCalls, /external_command=[^ \n]+/);
});

test("default live smoke keeps Morph opt-in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-default-providers-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  warmup)
    printf 'provisioning provider=test lease=cbx_123456789abc slug=default-smoke-test\\n'
    printf 'provisioned lease=cbx_123456789abc slug=default-smoke-test state=ready\\n'
    ;;
  status)
    printf 'lease=cbx_123456789abc slug=default-smoke-test provider=test state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_123456789abc","slug":"default-smoke-test","provider":"test","state":"ready","serverType":"type","host":"example.test","ready":true,"lastTouchedAt":"2026-06-10T00:00:00Z","expiresAt":"2026-06-10T00:15:00Z"}\\n'
    ;;
  ssh)
    exit 0
    ;;
  cache)
    printf '[]\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  history)
    printf 'history ok\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    if [[ "\${CRABBOX_FAKE_ADMIN_FAIL:-0}" == "1" ]]; then
      printf 'admin endpoint unavailable\\n' >&2
      exit 42
    fi
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const env = { ...process.env };
  delete env.CRABBOX_LIVE_PROVIDERS;
  delete env.CRABBOX_MORPH_API_KEY;
  delete env.MORPH_API_KEY;

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...env,
      PATH: `${bin}${path.delimiter}${env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /warmup --provider aws/);
  assert.match(crabboxCalls, /warmup --provider hetzner/);
  assert.doesNotMatch(crabboxCalls, /--provider morph/);
  assert.doesNotMatch(result.stderr, /CRABBOX_MORPH_API_KEY|MORPH_API_KEY|morph\.apiKey/);

  const failedAudit = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...env,
      PATH: `${bin}${path.delimiter}${env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_FAKE_ADMIN_FAIL: "1",
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_ADMIN_AUDIT: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });
  assert.equal(failedAudit.status, 42, failedAudit.stdout + failedAudit.stderr);
  assert.match(failedAudit.stderr, /error: admin active-lease check failed: admin endpoint unavailable/);
  assert.doesNotMatch(failedAudit.stderr, /unbound variable/);
});

test("apple-vz live smoke rejects an invalid explicit helper binary", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-apple-vz-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  fs.mkdirSync(bin);

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf 'unexpected crabbox args: %s\\n' "$*" >&2
exit 99
`,
  );

  const missingHelper = path.join(dir, "missing-helper");
  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "apple-vz",
      CRABBOX_LIVE_APPLE_VZ_HELPER: missingHelper,
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 2, result.stdout + result.stderr);
  assert.match(result.stderr, /CRABBOX_LIVE_APPLE_VZ_HELPER must point to an executable helper/);
  assert.match(result.stderr, new RegExp(missingHelper.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
});

test("apple-container live smoke uses the generic SSH lease lifecycle", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-apple-container-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  warmup|status|inspect|ssh|cache|run|history|stop)
    if [[ "\${CRABBOX_PROVIDER:-}" != "apple-container" ]]; then
      printf 'missing apple-container provider environment for: %s\\n' "$*" >&2
      exit 97
    fi
    ;;
esac
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  warmup)
    printf 'provisioning provider=apple-container lease=cbx_123456789abc slug=apple-container-smoke-test\\n'
    printf 'provisioned lease=cbx_123456789abc slug=apple-container-smoke-test state=ready\\n'
    ;;
  status)
    printf 'lease=cbx_123456789abc slug=apple-container-smoke-test provider=apple-container state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_123456789abc","slug":"apple-container-smoke-test","provider":"apple-container","state":"ready","serverType":"apple-container","host":"127.0.0.1","ready":true,"lastTouchedAt":"2026-06-11T00:00:00Z","expiresAt":"2026-06-11T00:15:00Z"}\\n'
    ;;
  ssh)
    exit 0
    ;;
  cache)
    printf '[]\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  history)
    printf 'history ok\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "apple-container",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const calls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(calls, /^warmup --provider apple-container --ttl 15m --idle-timeout 5m$/m);
  for (const command of ["status", "inspect", "ssh", "cache", "run", "history", "stop"]) {
    assert.match(calls, new RegExp(`^${command}(?: |$)`, "m"));
  }
});

test("local-container live smoke uses the generic SSH lease lifecycle", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-local-container-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  warmup|status|inspect|ssh|cache|run|history|stop)
    if [[ "\${CRABBOX_PROVIDER:-}" != "local-container" ]]; then
      printf 'missing local-container provider environment for: %s\\n' "$*" >&2
      exit 97
    fi
    ;;
esac
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  warmup)
    printf 'provisioning provider=local-container lease=cbx_123456789abc slug=local-container-smoke-test\\n'
    printf 'provisioned lease=cbx_123456789abc slug=local-container-smoke-test state=ready\\n'
    ;;
  status)
    printf 'lease=cbx_123456789abc slug=local-container-smoke-test provider=local-container state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_123456789abc","slug":"local-container-smoke-test","provider":"local-container","state":"ready","serverType":"local-container","host":"127.0.0.1","ready":true,"lastTouchedAt":"2026-06-11T00:00:00Z","expiresAt":"2026-06-11T00:15:00Z"}\\n'
    ;;
  ssh)
    exit 0
    ;;
  cache)
    printf '[]\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  history)
    printf 'history ok\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "local-container",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const calls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(calls, /^warmup --provider local-container --ttl 15m --idle-timeout 5m$/m);
  for (const command of ["status", "inspect", "ssh", "cache", "run", "history", "stop"]) {
    assert.match(calls, new RegExp(`^${command}(?: |$)`, "m"));
  }
});

test("docker-sandbox live smoke dispatches to the provider-specific smoke", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-docker-sandbox-dispatch-"));
  const bin = path.join(dir, "bin");
  const tempRepo = path.join(dir, "repo");
  const crabboxLog = path.join(dir, "crabbox.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(bin);
  fs.mkdirSync(tempRepo);

  writeExecutable(
    path.join(bin, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
out=""
while [[ "$#" -gt 0 ]]; do
  if [[ "$1" == "-o" ]]; then
    out="$2"
    shift 2
    continue
  fi
  shift
done
mkdir -p "$(dirname "$out")"
cat >"$out" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${crabboxLog}"
case "$1" in
  doctor)
    printf 'ok      sbx_version provider=docker-sandbox version=sbx client fake\n'
    ;;
  warmup)
    printf '%s\n' "$5" >"${slugFile}"
    ;;
  run)
    printf 'crabbox-docker-sandbox-ok\n'
    ;;
  list)
    slug="$(cat "${slugFile}")"
    printf '[{"name":"sandbox","labels":{"slug":"%s"}}]\n' "$slug"
    ;;
  stop)
    printf 'stopped %s\n' "\${*: -1}"
    ;;
  *)
    printf 'unexpected crabbox args: %s\n' "$*" >&2
    exit 99
    ;;
esac
SCRIPT
chmod +x "$out"
`,
  );

  const result = spawnSync("bash", [path.join(repoRoot, "scripts", "live-smoke.sh")], {
    cwd: tempRepo,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "docker-sandbox",
      CRABBOX_LIVE_REPO: tempRepo,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_sbx_smoke_passed/);
  assert.match(result.stdout, /cleanup=complete/);
  const calls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(calls, /^doctor --provider docker-sandbox$/m);
  assert.match(calls, /^warmup --provider docker-sandbox --slug docker-sandbox-smoke-\d{14}-\d+ --keep$/m);
  assert.match(calls, /^run --provider docker-sandbox --id docker-sandbox-smoke-\d{14}-\d+ -- echo ok$/m);
  assert.match(calls, /^run --provider docker-sandbox --id docker-sandbox-smoke-\d{14}-\d+ -- pwd$/m);
  assert.match(calls, /^list --provider docker-sandbox --json$/m);
  assert.match(calls, /^stop --provider docker-sandbox docker-sandbox-smoke-\d{14}-\d+$/m);
});

test("smolvm live smoke dispatches to the provider-specific smoke", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-smolvm-dispatch-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const calls = path.join(dir, "calls.log");
  const state = path.join(dir, "lease.state");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "$1" in
  run)
    if [[ "$*" == *"SMOLVM_SMOKE_V1_OK"* ]]; then
      requested_slug=""
      while [[ "$#" -gt 0 ]]; do
        case "$1" in
          --slug)
            requested_slug="\${2:-}"
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done
      printf '%s\\n' "$requested_slug" >"${state}"
      printf 'SMOLVM_SMOKE_V1_OK\\n'
      printf '{"provider":"smolvm","leaseId":"cbx_test"}\\n' >&2
    elif [[ "$*" == *"SMOLVM_SMOKE_V2_OK"* ]]; then
      test -f "${state}"
      printf 'SMOLVM_SMOKE_V2_OK\\n'
      printf '{"provider":"smolvm","leaseId":"cbx_test"}\\n' >&2
    elif [[ "$*" == *"SMOLVM_SMOKE_EXIT_23"* ]]; then
      test -f "${state}"
      printf 'SMOLVM_SMOKE_EXIT_23\\n'
      exit 23
    else
      printf 'unexpected run command\\n' >&2
      exit 64
    fi
    ;;
  status)
    test -f "${state}"
    printf '{"id":"cbx_test","slug":"%s","provider":"smolvm","state":"running"}\\n' "$(cat "${state}")"
    ;;
  list)
    if [[ -f "${state}" ]]; then
      printf '[{"Provider":"smolvm","slug":"%s","state":"running"}]\\n' "$(cat "${state}")"
    else
      printf '[]\\n'
    fi
    ;;
  doctor)
    printf '{"ok":true,"provider":"smolvm"}\\n'
    ;;
  stop)
    rm -f "${state}"
    ;;
  *)
    printf 'unexpected command %s\\n' "$1" >&2
    exit 64
    ;;
esac
`,
  );

  const result = spawnSync("bash", [path.join(repoRoot, "scripts", "live-smoke.sh")], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "smolvm",
      CRABBOX_SMOLVM_API_KEY: "smk_test",
      CRABBOX_SMOLVM_CLEANUP_RETRY_DELAY_SECONDS: "0",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_smolvm_smoke_passed/);
  assert.equal(fs.existsSync(state), false);
  const seen = fs.readFileSync(calls, "utf8");
  assert.match(seen, /^run --provider smolvm --keep --slug smolvm-live-smoke-/m);
  assert.match(seen, /^status --provider smolvm --id smolvm-live-smoke-.* --wait --json$/m);
  assert.match(seen, /^doctor --provider smolvm --json$/m);
  assert.match(seen, /^run --provider smolvm --id smolvm-live-smoke-.* --no-sync -- /m);
  assert.match(seen, /^stop --provider smolvm smolvm-live-smoke-/m);
});

test("superserve live smoke dispatches to the provider-specific smoke", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-superserve-dispatch-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const calls = path.join(dir, "calls.log");
  const state = path.join(dir, "lease.state");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    printf '{"ok":true,"provider":"superserve"}\\n'
    ;;
  run)
    if [[ "$*" == *"SUPERSERVE_SMOKE_V1_OK"* ]]; then
      requested_slug=""
      while [[ "$#" -gt 0 ]]; do
        case "$1" in
          --slug)
            requested_slug="\${2:-}"
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done
      printf '%s\\n' "$requested_slug" >"${state}"
      printf 'SUPERSERVE_SMOKE_STDOUT\\n'
      printf 'SUPERSERVE_SMOKE_STDERR\\n' >&2
      printf 'SUPERSERVE_SMOKE_V1_OK\\n'
      printf '{"provider":"superserve","leaseId":"ssbx_test"}\\n' >&2
    elif [[ "$*" == *"SUPERSERVE_SMOKE_V2_OK"* ]]; then
      test -f "${state}"
      printf 'SUPERSERVE_SMOKE_V2_OK\\n'
      printf '{"provider":"superserve","leaseId":"ssbx_test"}\\n' >&2
    elif [[ "$*" == *"SUPERSERVE_SMOKE_EXIT_23"* ]]; then
      test -f "${state}"
      printf 'SUPERSERVE_SMOKE_EXIT_23\\n'
      exit 23
    else
      printf 'unexpected run command\\n' >&2
      exit 64
    fi
    ;;
  status)
    test -f "${state}"
    printf '{"id":"ssbx_test","slug":"%s","provider":"superserve","state":"running"}\\n' "$(cat "${state}")"
    ;;
  list)
    if [[ -f "${state}" ]]; then
      printf '[{"provider":"superserve","slug":"%s","state":"running"}]\\n' "$(cat "${state}")"
    else
      printf '[]\\n'
    fi
    ;;
  stop)
    rm -f "${state}"
    ;;
  *)
    printf 'unexpected command %s\\n' "$1" >&2
    exit 64
    ;;
esac
`,
  );

  const apiKey = "ss_live_fake_value";
  const result = spawnSync("bash", [path.join(repoRoot, "scripts", "live-smoke.sh")], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "superserve",
      CRABBOX_SUPERSERVE_API_KEY: apiKey,
      CRABBOX_SUPERSERVE_CLEANUP_RETRY_DELAY_SECONDS: "0",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_superserve_smoke_passed/);
  assert.match(result.stdout, /cleanup=confirmed provider=superserve slug=ss-live-/);
  assert.doesNotMatch(result.stdout + result.stderr, new RegExp(apiKey));
  assert.equal(fs.existsSync(state), false);
  const seen = fs.readFileSync(calls, "utf8");
  assert.match(seen, /^doctor --provider superserve --json$/m);
  assert.match(seen, /^run --provider superserve --keep --slug ss-live-/m);
  assert.match(seen, /^status --provider superserve --id ss-live-.* --wait --json$/m);
  assert.match(seen, /^run --provider superserve --id ss-live-.* --no-sync -- /m);
  assert.match(seen, /^stop --provider superserve ss-live-/m);
});

test("vercel-sandbox live smoke dispatches to the provider-specific smoke", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-vercel-sandbox-dispatch-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const fakeSandbox = path.join(dir, "sandbox");
  const calls = path.join(dir, "calls.log");
  const state = path.join(dir, "lease.state");
  const remoteStopped = path.join(dir, "remote-stopped.state");

  writeExecutable(
    fakeSandbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf 'sandbox %s\\n' "$*" >>"${calls}"
case "\${1:-}" in
  --help)
    printf 'sandbox help\\n'
    ;;
  list)
    if [[ -f "${remoteStopped}" && "\${FAKE_SANDBOX_STALE_LIST:-0}" == "1" ]]; then
      printf 'NAME STATUS\\n'
      printf 'test stopped\\n'
    else
      printf 'No sandboxes found\\n'
    fi
    ;;
  stop)
    : >"${remoteStopped}"
    printf 'Stopped %s\\n' "\${*: -1}"
    ;;
  rm)
    rm -f "${remoteStopped}"
    printf 'Removed %s\\n' "\${*: -1}"
    ;;
  *)
    printf 'unexpected sandbox command %s\\n' "\${1:-}" >&2
    exit 64
    ;;
esac
`,
  );

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf 'crabbox %s\\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    printf '{"ok":true,"provider":"vercel-sandbox"}\\n'
    ;;
  run)
    if [[ "$*" == *"VERCEL_SANDBOX_SMOKE_V1_OK"* ]]; then
      requested_slug=""
      while [[ "$#" -gt 0 ]]; do
        case "$1" in
          --slug)
            requested_slug="\${2:-}"
            shift 2
            ;;
          *)
            shift
            ;;
        esac
      done
      printf '%s\\n' "$requested_slug" >"${state}"
      printf 'VERCEL_SANDBOX_SMOKE_STDOUT\\n'
      printf 'VERCEL_SANDBOX_SMOKE_STDERR\\n' >&2
      printf 'VERCEL_SANDBOX_SMOKE_V1_OK\\n'
      printf '{"provider":"vercel-sandbox","leaseId":"vsbx_test"}\\n' >&2
    elif [[ "$*" == *"VERCEL_SANDBOX_SMOKE_V2_OK"* ]]; then
      test -f "${state}"
      printf 'VERCEL_SANDBOX_SMOKE_V2_OK\\n'
      printf '{"provider":"vercel-sandbox","leaseId":"vsbx_test"}\\n' >&2
    elif [[ "$*" == *"VERCEL_SANDBOX_STREAM_START"* ]]; then
      test -f "${state}"
      printf 'VERCEL_SANDBOX_STREAM_START\\n'
      sleep 0.3
      printf 'VERCEL_SANDBOX_STREAM_END\\n'
    elif [[ "$*" == *"VERCEL_SANDBOX_SMOKE_EXIT_23"* ]]; then
      test -f "${state}"
      printf 'VERCEL_SANDBOX_SMOKE_EXIT_23\\n'
      exit 23
    else
      printf 'unexpected run command\\n' >&2
      exit 64
    fi
    ;;
  status)
    test -f "${state}"
    printf '{"id":"vsbx_test","slug":"%s","provider":"vercel-sandbox","state":"running"}\\n' "$(cat "${state}")"
    ;;
  list)
    if [[ -f "${state}" ]]; then
      printf '[{"provider":"vercel-sandbox","slug":"%s","state":"running"}]\\n' "$(cat "${state}")"
    else
      printf '[]\\n'
    fi
    ;;
  stop)
    rm -f "${state}"
    ;;
  *)
    printf 'unexpected command %s\\n' "$1" >&2
    exit 64
    ;;
esac
`,
  );

  const authValue = "vercel_fake_redaction_value";
  const result = spawnSync("bash", [path.join(repoRoot, "scripts", "live-smoke.sh")], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${dir}${path.delimiter}${process.env.PATH}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "vercel-sandbox",
      CRABBOX_VERCEL_SANDBOX_AUTH_TOKEN: authValue,
      CRABBOX_VERCEL_SANDBOX_CLEANUP_RETRY_DELAY_SECONDS: "0",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_vercel_sandbox_smoke_passed/);
  assert.match(result.stdout, /session_stop=confirmed provider=vercel-sandbox sandbox=test/);
  assert.match(result.stdout, /streaming=confirmed provider=vercel-sandbox sandbox=test/);
  assert.match(result.stdout, /cleanup=confirmed provider=vercel-sandbox slug=vs-live-.* sandbox=test/);
  assert.doesNotMatch(result.stdout + result.stderr, new RegExp(authValue));
  assert.equal(fs.existsSync(state), false);
  const seen = fs.readFileSync(calls, "utf8");
  assert.match(seen, /^sandbox --help$/m);
  assert.match(seen, /^sandbox list --all --limit 1$/m);
  assert.match(seen, /^crabbox doctor --provider vercel-sandbox --json$/m);
  assert.match(seen, /^crabbox run --provider vercel-sandbox --keep --slug vs-live-/m);
  assert.match(seen, /^crabbox status --provider vercel-sandbox --id vs-live-.* --wait --json$/m);
  assert.match(seen, /^sandbox stop test$/m);
  assert.match(seen, /^crabbox run --provider vercel-sandbox --id vs-live-.*VERCEL_SANDBOX_SMOKE_V2_OK/m);
  assert.match(seen, /^crabbox run --provider vercel-sandbox --id vs-live-.*VERCEL_SANDBOX_STREAM_START/m);
  assert.match(seen, /^crabbox run --provider vercel-sandbox --id vs-live-.* --no-sync -- /m);
  assert.match(seen, /^crabbox stop --provider vercel-sandbox vs-live-/m);
  assert.match(seen, /^sandbox list --all --name-prefix test --sort-by name --limit 50$/m);
});

test("linode live smoke dispatches to the provider-specific smoke", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-linode-dispatch-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
if [[ "\${LINODE_TOKEN:-}" != "linode_fake_value" ]]; then
  printf 'missing Linode auth\\n' >&2
  exit 91
fi
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready api=list mutation=false leases=0 runtime=unchecked default_type=g6-standard-1 region=us-ord image=linode/ubuntu24.04\\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"}}]\\n' "$slug"
    fi
    ;;
  warmup)
    requested_slug=""
    while [[ "$#" -gt 0 ]]; do
      case "$1" in
        --slug)
          requested_slug="\${2:-}"
          shift 2
          ;;
        *)
          shift
          ;;
      esac
    done
    printf '%s\\n' "$requested_slug" >"${slugFile}"
    ;;
  status)
    printf 'status=ready\\n'
    ;;
  run)
    printf 'ok\\n'
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
    ;;
  cleanup)
    printf 'skip server id=none name=none reason=missing labels\\n'
    ;;
  *)
    printf 'unexpected args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [path.join(repoRoot, "scripts", "live-smoke.sh")], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "linode",
      LINODE_TOKEN: "linode_fake_value",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_linode_smoke_passed slug=linode-smoke-/);
  assert.doesNotMatch(result.stdout + result.stderr, /linode_fake_value/);
  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider linode");
  assert.equal(seen[1], "list --provider linode --json");
  assert.match(seen[2], /^warmup --provider linode --slug linode-smoke-\d{14}-\d+ --keep --type g6-standard-1 --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider linode --id linode-smoke-\d{14}-\d+ --wait --wait-timeout 300s$/);
  assert.match(seen[4], /^run --provider linode --id linode-smoke-\d{14}-\d+ --no-sync -- echo ok$/);
  assert.equal(seen[5], "list --provider linode --json");
  assert.match(seen[6], /^stop --provider linode linode-smoke-\d{14}-\d+$/);
  assert.equal(seen[7], "cleanup --provider linode --dry-run");
  assert.equal(seen[8], "list --provider linode --json");
});

test("digitalocean live smoke dispatches to the provider-specific smoke", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-digitalocean-dispatch-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(dir, "crabbox");
  const fakePython = path.join(bin, "python3");
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");
  const realPython = spawnSync("sh", ["-c", "command -v python3"], { encoding: "utf8" }).stdout.trim();
  assert.ok(realPython, "python3 is required for DigitalOcean dispatch smoke");
  fs.mkdirSync(bin);

  writeExecutable(
    fakePython,
    `#!/usr/bin/env bash
if [[ "$*" == *"/account/keys"* ]]; then
  printf '[]\\n'
  exit 0
fi
if [[ "$*" == *"urllib.request"* ]]; then
  exit 1
fi
exec ${JSON.stringify(realPython)} "$@"
`,
  );

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
if [[ "\${DIGITALOCEAN_TOKEN:-}" != "digitalocean_fake_value" ]]; then
  printf 'missing DigitalOcean auth\\n' >&2
  exit 91
fi
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready api=list mutation=false leases=0 runtime=unchecked default_type=s-1vcpu-1gb region=nyc3 image=ubuntu-24-04-x64\\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"}}]\\n' "$slug"
    fi
    ;;
  warmup)
    requested_slug=""
    while [[ "$#" -gt 0 ]]; do
      case "$1" in
        --slug)
          requested_slug="\${2:-}"
          shift 2
          ;;
        *)
          shift
          ;;
      esac
    done
    printf '%s\\n' "$requested_slug" >"${slugFile}"
    ;;
  status)
    printf 'status=ready\\n'
    ;;
  run)
    printf 'ok\\n'
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
    ;;
  cleanup)
    printf 'skip server id=none name=none reason=missing labels\\n'
    ;;
  *)
    printf 'unexpected args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [path.join(repoRoot, "scripts", "live-smoke.sh")], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "digitalocean",
      DIGITALOCEAN_TOKEN: "digitalocean_fake_value",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_digitalocean_smoke_passed slug=digitalocean-smoke-/);
  assert.doesNotMatch(result.stdout + result.stderr, /digitalocean_fake_value/);
  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider digitalocean");
  assert.equal(seen[1], "list --provider digitalocean --json");
  assert.match(seen[2], /^warmup --provider digitalocean --slug digitalocean-smoke-\d{14}-\d+ --keep --type s-1vcpu-1gb --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider digitalocean --id digitalocean-smoke-\d{14}-\d+ --wait --wait-timeout 300s$/);
  assert.match(seen[4], /^run --provider digitalocean --id digitalocean-smoke-\d{14}-\d+ --no-sync -- echo ok$/);
  assert.equal(seen[5], "list --provider digitalocean --json");
  assert.match(seen[6], /^stop --provider digitalocean digitalocean-smoke-\d{14}-\d+$/);
  assert.equal(seen[7], "cleanup --provider digitalocean --dry-run");
  assert.equal(seen[8], "list --provider digitalocean --json");
});

test("nebius live smoke dispatches to the provider-specific smoke", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nebius-dispatch-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready leases=0 runtime=unchecked\\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"crabbox_slug":"%s"},"provider":"nebius"}]\\n' "$slug"
    fi
    ;;
  warmup)
    requested_slug=""
    while [[ "$#" -gt 0 ]]; do
      case "$1" in
        --slug)
          requested_slug="\${2:-}"
          shift 2
          ;;
        *)
          shift
          ;;
      esac
    done
    printf '%s\\n' "$requested_slug" >"${slugFile}"
    ;;
  status)
    printf 'status=ready\\n'
    ;;
  run)
    printf 'ok\\n'
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
    ;;
  cleanup)
    printf 'skip nebius instance id=none name=none reason=missing labels\\n'
    ;;
  *)
    printf 'unexpected args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [path.join(repoRoot, "scripts", "live-smoke.sh")], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "nebius",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_nebius_smoke_passed slug=nebius-smoke-/);
  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider nebius");
  assert.equal(seen[1], "list --provider nebius --json");
  assert.match(seen[2], /^warmup --provider nebius --slug nebius-smoke-\d+-\d+-[0-9a-f]{8} --keep --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider nebius --id nebius-smoke-\d+-\d+-[0-9a-f]{8} --wait --wait-timeout 300s$/);
  assert.match(seen[4], /^run --provider nebius --id nebius-smoke-\d+-\d+-[0-9a-f]{8} --no-sync -- echo ok$/);
  assert.equal(seen[5], "list --provider nebius --json");
  assert.match(seen[6], /^stop --provider nebius nebius-smoke-\d+-\d+-[0-9a-f]{8}$/);
  assert.equal(seen[7], "cleanup --provider nebius --dry-run");
  assert.equal(seen[8], "list --provider nebius --json");
});

test("ovh live smoke dispatches to the provider-specific smoke", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ovh-dispatch-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
if [[ "\${OVH_APPLICATION_KEY:-}" != "ovh_app_fake" || "\${OVH_APPLICATION_SECRET:-}" != "ovh_secret_fake" || "\${OVH_CONSUMER_KEY:-}" != "ovh_consumer_fake" ]]; then
  printf 'missing OVH auth\\n' >&2
  exit 91
fi
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready mutation=false leases=0 endpoint=https://api.us.ovhcloud.com/1.0 region=BHS5 image=Ubuntu 24.04 flavor=b3-8\\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"},"provider":"ovh"}]\\n' "$slug"
    fi
    ;;
  warmup)
    requested_slug=""
    while [[ "$#" -gt 0 ]]; do
      case "$1" in
        --slug)
          requested_slug="\${2:-}"
          shift 2
          ;;
        *)
          shift
          ;;
      esac
    done
    printf '%s\\n' "$requested_slug" >"${slugFile}"
    ;;
  status)
    printf 'status=ready\\n'
    ;;
  run)
    printf 'ok\\n'
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
    ;;
  cleanup)
    printf 'skip server id=none name=none reason=missing labels\\n'
    ;;
  *)
    printf 'unexpected args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [path.join(repoRoot, "scripts", "live-smoke.sh")], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "ovh",
      OVH_APPLICATION_KEY: "ovh_app_fake",
      OVH_APPLICATION_SECRET: "ovh_secret_fake",
      OVH_CONSUMER_KEY: "ovh_consumer_fake",
      CRABBOX_OVH_PROJECT_ID: "project-test",
      CRABBOX_OVH_REGION: "BHS5",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_ovh_smoke_passed slug=ovh-smoke-/);
  assert.doesNotMatch(result.stdout + result.stderr, /ovh_secret_fake|ovh_consumer_fake/);
  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider ovh");
  assert.equal(seen[1], "list --provider ovh --json");
  assert.match(seen[2], /^warmup --provider ovh --slug ovh-smoke-\d{14}-\d+ --keep --type b3-8 --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider ovh --id ovh-smoke-\d{14}-\d+ --wait --wait-timeout 300s$/);
  assert.match(seen[4], /^run --provider ovh --id ovh-smoke-\d{14}-\d+ --no-sync -- echo ok$/);
  assert.equal(seen[5], "list --provider ovh --json");
  assert.match(seen[6], /^stop --provider ovh ovh-smoke-\d{14}-\d+$/);
  assert.equal(seen[7], "doctor --provider ovh");
  assert.equal(seen[8], "cleanup --provider ovh --dry-run");
  assert.equal(seen[9], "list --provider ovh --json");
});

test("nvidia brev live smoke dispatches to the provider-specific smoke", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-nvidia-brev-dispatch-"));
  const fakeCrabbox = path.join(dir, "crabbox");
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready leases=0 runtime=unchecked\\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"},"provider":"nvidia-brev"}]\\n' "$slug"
    fi
    ;;
  warmup)
    requested_slug=""
    while [[ "$#" -gt 0 ]]; do
      case "$1" in
        --slug)
          requested_slug="\${2:-}"
          shift 2
          ;;
        *)
          shift
          ;;
      esac
    done
    printf '%s\\n' "$requested_slug" >"${slugFile}"
    ;;
  status)
    printf 'status=ready\\n'
    ;;
  run)
    printf 'NVIDIA-SMI 555.55.55\\n'
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
    ;;
  *)
    printf 'unexpected args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [path.join(repoRoot, "scripts", "live-smoke.sh")], {
    cwd: repoRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "nvidia-brev",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_nvidia_brev_smoke_passed slug=nbrev-smoke-/);
  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider nvidia-brev --nvidia-brev-cli brev");
  assert.equal(seen[1], "list --provider nvidia-brev --nvidia-brev-cli brev --json");
  assert.match(seen[2], /^warmup --provider nvidia-brev --nvidia-brev-cli brev --nvidia-brev-release-action delete --slug nbrev-smoke-\d+-\d+-[0-9a-f]{8} --keep=false --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider nvidia-brev --nvidia-brev-cli brev --nvidia-brev-release-action delete --id nbrev-smoke-\d+-\d+-[0-9a-f]{8} --wait --wait-timeout 300s$/);
  assert.match(seen[4], /^run --provider nvidia-brev --nvidia-brev-cli brev --nvidia-brev-release-action delete --id nbrev-smoke-\d+-\d+-[0-9a-f]{8} --no-sync -- nvidia-smi$/);
  assert.equal(seen[5], "list --provider nvidia-brev --nvidia-brev-cli brev --json");
  assert.match(seen[6], /^stop --provider nvidia-brev --nvidia-brev-cli brev --nvidia-brev-release-action delete nbrev-smoke-\d+-\d+-[0-9a-f]{8}$/);
});

test("anthropic sandbox runtime live smoke dispatches to the provider-specific smoke", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-srt-dispatch-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(dir, "crabbox");
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(bin);
  writeExecutable(path.join(bin, "srt"), "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(path.join(bin, "curl"), "#!/usr/bin/env bash\nexit 0\n");

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"${calls}"
case "$*" in
  "doctor --provider anthropic-sandbox-runtime")
    printf 'ok      srt_help provider=anthropic-sandbox-runtime mutation=false\\n'
    ;;
  "run --provider anthropic-sandbox-runtime -- echo ok")
    printf 'ok\\n'
    ;;
  run\\ --provider\\ anthropic-sandbox-runtime\\ --anthropic-sandbox-runtime-settings*allowed*)
    printf 'ok\\n'
    ;;
  run\\ --provider\\ anthropic-sandbox-runtime\\ --anthropic-sandbox-runtime-settings*cat*)
    printf 'Operation not permitted\\n' >&2
    exit 5
    ;;
  run\\ --provider\\ anthropic-sandbox-runtime\\ --anthropic-sandbox-runtime-settings*curl*)
    printf 'Connection blocked by network allowlist\\n' >&2
    exit 7
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [path.join(repoRoot, "scripts", "live-smoke.sh")], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "anthropic-sandbox-runtime",
      TMPDIR: dir,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_anthropic_sandbox_runtime_smoke_passed/);
  assert.match(result.stderr, /Operation not permitted/);
  assert.match(result.stderr, /Connection blocked by network allowlist/);
  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen.length, 5, JSON.stringify(seen));
  assert.equal(seen[0], "doctor --provider anthropic-sandbox-runtime");
  assert.equal(seen[1], "run --provider anthropic-sandbox-runtime -- echo ok");
});

test("multipass live smoke uses the generic SSH lease lifecycle", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-multipass-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  warmup|status|inspect|ssh|cache|run|history|stop)
    if [[ "\${CRABBOX_PROVIDER:-}" != "multipass" ]]; then
      printf 'missing multipass provider environment for: %s\\n' "$*" >&2
      exit 97
    fi
    ;;
esac
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  warmup)
    printf 'provisioning provider=multipass lease=cbx_123456789abc slug=multipass-smoke-test\\n'
    printf 'provisioned lease=cbx_123456789abc slug=multipass-smoke-test state=ready\\n'
    ;;
  status)
    printf 'lease=cbx_123456789abc slug=multipass-smoke-test provider=multipass state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_123456789abc","slug":"multipass-smoke-test","provider":"multipass","state":"ready","serverType":"26.04","host":"127.0.0.1","ready":true,"lastTouchedAt":"2026-06-11T00:00:00Z","expiresAt":"2026-06-11T00:15:00Z"}\\n'
    ;;
  ssh)
    exit 0
    ;;
  cache)
    printf '[]\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  history)
    printf 'history ok\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "multipass",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const calls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(calls, /^warmup --provider multipass --ttl 15m --idle-timeout 5m$/m);
  for (const command of ["status", "inspect", "ssh", "cache", "run", "history", "stop"]) {
    assert.match(calls, new RegExp(`^${command}(?: |$)`, "m"));
  }
});

test("tart live smoke uses the generic SSH lease lifecycle", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-tart-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  warmup|status|inspect|ssh|cache|run|history|stop)
    if [[ "\${CRABBOX_PROVIDER:-}" != "tart" ]]; then
      printf 'missing tart provider environment for: %s\\n' "$*" >&2
      exit 97
    fi
    ;;
esac
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  warmup)
    printf 'provisioning provider=tart lease=cbx_123456789abc slug=tart-smoke-test\\n'
    printf 'provisioned lease=cbx_123456789abc slug=tart-smoke-test state=ready\\n'
    ;;
  status)
    printf 'lease=cbx_123456789abc slug=tart-smoke-test provider=tart state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_123456789abc","slug":"tart-smoke-test","provider":"tart","state":"ready","serverType":"ghcr.io/cirruslabs/macos-sequoia-base:latest","host":"127.0.0.1","ready":true,"lastTouchedAt":"2026-06-11T00:00:00Z","expiresAt":"2026-06-11T00:30:00Z"}\\n'
    ;;
  ssh)
    exit 0
    ;;
  cache)
    printf '[]\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  history)
    printf 'history ok\\n'
    ;;
  stop)
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "tart",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const calls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(calls, /^warmup --provider tart --ttl 30m --idle-timeout 5m$/m);
  for (const command of ["status", "inspect", "ssh", "cache", "run", "history", "stop"]) {
    assert.match(calls, new RegExp(`^${command}(?: |$)`, "m"));
  }
});

test("apple-vz live smoke preserves the helper override for the full lifecycle", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-apple-vz-lifecycle-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const helper = path.join(bin, "custom-apple-vz-helper");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);
  writeExecutable(helper, "#!/usr/bin/env bash\nexit 0\n");
  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "\${CRABBOX_APPLE_VZ_HELPER:-}" != "\${CRABBOX_FAKE_EXPECTED_HELPER:?}" ]]; then
  printf 'missing helper override for: %s\\n' "$*" >&2
  exit 98
fi
case "$1" in
  warmup|status|inspect|ssh|cache|run|history|stop)
    if [[ "\${CRABBOX_PROVIDER:-}" != "apple-vz" ]]; then
      printf 'missing apple-vz provider environment for: %s\\n' "$*" >&2
      exit 97
    fi
    ;;
esac
case "$1" in
  cache|history)
    if [[ " $* " == *" --provider "* ]]; then
      printf 'unsupported provider flag for: %s\\n' "$*" >&2
      exit 96
    fi
    ;;
esac
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  warmup)
    printf 'provisioning provider=apple-vz lease=cbx_123456789abc slug=apple-vz-smoke-test\\n'
    printf 'provisioned lease=cbx_123456789abc slug=apple-vz-smoke-test state=ready\\n'
    ;;
  status)
    printf 'lease=cbx_123456789abc slug=apple-vz-smoke-test provider=apple-vz state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_123456789abc","slug":"apple-vz-smoke-test","provider":"apple-vz","state":"ready","serverType":"apple-vz","host":"127.0.0.1","ready":true,"lastTouchedAt":"2026-06-11T00:00:00Z","expiresAt":"2026-06-11T00:15:00Z"}\\n'
    ;;
  ssh)
    exit 0
    ;;
  cache)
    printf '[]\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  history)
    printf 'history ok\\n'
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

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_FAKE_EXPECTED_HELPER: helper,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "apple-vz",
      CRABBOX_LIVE_APPLE_VZ_HELPER: helper,
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const calls = fs.readFileSync(crabboxLog, "utf8");
  for (const command of ["warmup", "status", "inspect", "ssh", "cache", "run", "history", "stop"]) {
    assert.match(calls, new RegExp(`^${command}(?: |$)`, "m"));
  }
  assert.doesNotMatch(calls, /--apple-vz-helper/);
});

test("live smoke fails when final active lease audit fails", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-admin-audit-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  fs.mkdirSync(bin);

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  admin)
    printf 'admin endpoint unavailable\\n' >&2
    exit 42
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_ADMIN_AUDIT: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 42, result.stdout + result.stderr);
  assert.match(result.stderr, /error: admin active-lease check failed: admin endpoint unavailable/);
});

test("live smoke skips final active lease audit when coordinator is disabled", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-admin-skip-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  fs.mkdirSync(bin);

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf 'unexpected crabbox args: %s\\n' "$*" >&2
exit 99
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: path.join(dir, "missing-crabbox.yaml"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "",
      CRABBOX_LIVE_REPO: repoRoot,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /admin active-lease check skipped/);
  assert.match(result.stdout, /^0\n?$/);
});

test("morph live smoke dispatches the expected argv to crabbox", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-morph-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  config)
    exit 0
    ;;
  doctor)
    printf 'ok provider=morph\\n'
    ;;
  warmup)
    printf 'provisioning provider=morph lease=cbx_1a2b3c4d5e6f slug=morph-smoke-test\\n'
    printf 'provisioned lease=cbx_1a2b3c4d5e6f slug=morph-smoke-test state=ready\\n'
    ;;
  status)
    printf 'lease=cbx_1a2b3c4d5e6f slug=morph-smoke-test provider=morph state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_1a2b3c4d5e6f","slug":"morph-smoke-test","provider":"morph","state":"ready","serverType":"snapshot_test","host":"ssh.cloud.morph.so","ready":true,"lastTouchedAt":"2026-06-09T20:00:00Z","expiresAt":"2026-06-09T20:15:00Z"}\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  list)
    printf '[{"id":"cbx_1a2b3c4d5e6f","slug":"morph-smoke-test","provider":"morph","state":"ready"}]\\n'
    ;;
  stop)
    [[ "\${CRABBOX_MORPH_DELETE_ON_RELEASE:-}" == "1" ]] || exit 96
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "morph",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_MORPH_API_KEY: "dummy-morph-key",
      CRABBOX_LIVE_MORPH_SNAPSHOT: "snapshot_test",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /^doctor$/m);
  assert.match(crabboxCalls, /^warmup --keep=false --slug morph-smoke-\d+ --ttl 15m --idle-timeout 5m$/m);
  assert.match(crabboxCalls, /^status --id morph-smoke-test --wait --wait-timeout 120s$/m);
  assert.match(crabboxCalls, /^inspect --id morph-smoke-test --json$/m);
  assert.match(crabboxCalls, /^run --id morph-smoke-test --shell --/m);
  assert.match(crabboxCalls, /^list --json$/m);
  assert.match(crabboxCalls, /^stop morph-smoke-test$/m);
  assert.doesNotMatch(crabboxCalls, /dummy-morph-key/);
});

test("morph live smoke accepts the API key from config", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-morph-config-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  const config = path.join(dir, "crabbox.yaml");
  fs.mkdirSync(bin);
  fs.writeFileSync(
    config,
    `morph:
  apiKey: config-backed-morph-key
`,
    "utf8",
  );

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
case "$1" in
  doctor)
    printf 'ok provider=morph\\n'
    ;;
  warmup)
    printf 'provisioning provider=morph lease=cbx_1a2b3c4d5e6f slug=morph-smoke-test\\n'
    printf 'provisioned lease=cbx_1a2b3c4d5e6f slug=morph-smoke-test state=ready\\n'
    ;;
  status)
    printf 'lease=cbx_1a2b3c4d5e6f slug=morph-smoke-test provider=morph state=ready ready=true\\n'
    ;;
  inspect)
    printf '{"id":"cbx_1a2b3c4d5e6f","slug":"morph-smoke-test","provider":"morph","state":"ready","serverType":"snapshot_test","host":"ssh.cloud.morph.so","ready":true,"lastTouchedAt":"2026-06-09T20:00:00Z","expiresAt":"2026-06-09T20:15:00Z"}\\n'
    ;;
  run)
    printf 'crabbox-live-ok\\n'
    ;;
  list)
    printf '[{"id":"cbx_1a2b3c4d5e6f","slug":"morph-smoke-test","provider":"morph","state":"ready"}]\\n'
    ;;
  stop)
    [[ "\${CRABBOX_MORPH_DELETE_ON_RELEASE:-}" == "1" ]] || exit 96
    printf 'stopped %s\\n' "\${*: -1}"
    ;;
  admin)
    printf '[]\\n'
    ;;
  *)
    printf 'unexpected crabbox args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const env = { ...process.env };
  delete env.CRABBOX_MORPH_API_KEY;
  delete env.MORPH_API_KEY;

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...env,
      PATH: `${bin}${path.delimiter}${env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_CONFIG: config,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "morph",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_LIVE_MORPH_SNAPSHOT: "snapshot_test",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /crabbox-live-ok/);
  const crabboxCalls = fs.readFileSync(crabboxLog, "utf8");
  assert.match(crabboxCalls, /^doctor$/m);
  assert.match(crabboxCalls, /^warmup --keep=false --slug morph-smoke-\d+ --ttl 15m --idle-timeout 5m$/m);
  assert.match(crabboxCalls, /^stop morph-smoke-test$/m);
  assert.doesNotMatch(crabboxCalls, /config-backed-morph-key/);
});

test("morph live smoke aborts cleanly when no API key is configured", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-morph-nokey-"));
  const bin = path.join(dir, "bin");
  const fakeCrabbox = path.join(bin, "crabbox");
  const crabboxLog = path.join(dir, "crabbox.log");
  fs.mkdirSync(bin);

  writeExecutable(
    fakeCrabbox,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>"\${CRABBOX_FAKE_LOG:?}"
exit 0
`,
  );

  const env = { ...process.env };
  delete env.CRABBOX_MORPH_API_KEY;
  delete env.MORPH_API_KEY;

  const result = spawnSync("bash", ["scripts/live-smoke.sh"], {
    cwd: repoRoot,
    env: {
      ...env,
      PATH: `${bin}${path.delimiter}${env.PATH ?? ""}`,
      CRABBOX_BIN: fakeCrabbox,
      CRABBOX_FAKE_LOG: crabboxLog,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_COORDINATOR: "0",
      CRABBOX_LIVE_PROVIDERS: "morph",
      CRABBOX_LIVE_REPO: repoRoot,
      CRABBOX_LIVE_MORPH_SNAPSHOT: "snapshot_test",
    },
    encoding: "utf8",
  });

  assert.notEqual(result.status, 0, "expected non-zero exit when morph key is missing");
  assert.match(result.stderr, /CRABBOX_MORPH_API_KEY/);
  assert.match(result.stderr, /MORPH_API_KEY/);
  assert.match(result.stderr, /morph\.apiKey/);
  const calls = fs.existsSync(crabboxLog) ? fs.readFileSync(crabboxLog, "utf8") : "";
  assert.doesNotMatch(calls, /--provider morph/, "no morph-specific crabbox call may be issued when the key is missing");
});
