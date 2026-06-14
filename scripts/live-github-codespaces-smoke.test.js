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

function prepareSmokeRepo(dir) {
  const tempRoot = path.join(dir, "repo");
  const tempScripts = path.join(tempRoot, "scripts");
  const smokeScript = path.join(tempScripts, "live-github-codespaces-smoke.sh");
  fs.mkdirSync(tempScripts, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-github-codespaces-smoke.sh"), smokeScript);
  fs.chmodSync(smokeScript, 0o755);
  fs.writeFileSync(path.join(tempRoot, "go.mod"), "module example.org/smoke\n", "utf8");
  return { tempRoot, smokeScript };
}

function writeFakeGH(binDir, callsFile) {
  writeExecutable(
    path.join(binDir, "gh"),
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>${JSON.stringify(callsFile)}
if [[ "$*" == "auth status" ]]; then
  exit 0
fi
printf 'unexpected gh args: %s\\n' "$*" >&2
exit 97
`,
  );
}

function writeFakeCrabbox(file, body) {
  writeExecutable(
    file,
    `#!/usr/bin/env bash
set -euo pipefail
${body}
`,
  );
}

test("live github codespaces smoke skips unless opted in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-skip-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const crabbox = path.join(dir, "crabbox");
  writeFakeCrabbox(crabbox, "exit 99");

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: { ...process.env, CRABBOX_BIN: crabbox, CRABBOX_LIVE: "", GH_TOKEN: "" },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
});

test("live github codespaces smoke skips unless provider filter selects it", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-filter-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const crabbox = path.join(dir, "crabbox");
  writeFakeCrabbox(crabbox, "exit 99");

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      CRABBOX_BIN: crabbox,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "linode,digitalocean",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REPO: "example-org/my-app",
      GH_TOKEN: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=github_codespaces_not_selected/);
});

test("live github codespaces smoke requires explicit credential source before gh", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-token-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const ghCalls = path.join(dir, "gh.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeFakeGH(binDir, ghCalls);

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "github-codespaces",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REPO: "example-org/my-app",
      GH_TOKEN: "",
      GITHUB_TOKEN: "",
      CRABBOX_GITHUB_CODESPACES_USE_GH_AUTH: "",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=credential_bound reason=github_token_missing_or_gh_auth_not_enabled/);
  assert.equal(fs.existsSync(ghCalls), false);
});

test("live github codespaces smoke runs guarded lifecycle and redacts token", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  const ghCalls = path.join(dir, "gh.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });
  writeFakeGH(binDir, ghCalls);
  writeFakeCrabbox(
    path.join(dir, "crabbox"),
    `printf '%s\\n' "$*" >>${JSON.stringify(calls)}
if [[ "\${GH_TOKEN:-}" != "test-secret-token" ]]; then
  printf 'missing token\\n' >&2
  exit 91
fi
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready api=list mutation=false leases=0 runtime=unchecked\\n'
    ;;
  warmup)
    for ((i=1; i<=$#; i++)); do
      if [[ "\${!i}" == "--slug" ]]; then
        j=$((i + 1))
        printf '%s\\n' "\${!j}" >${JSON.stringify(slugFile)}
      fi
    done
    ;;
  status)
    printf 'status=ready\\n'
    ;;
  run)
    printf 'github-codespaces-smoke-ok\\n'
    ;;
  ssh)
    printf 'ssh -F /tmp/github-codespaces.conf codespace.example\\n'
    ;;
  list)
    slug="$(cat ${JSON.stringify(slugFile)} 2>/dev/null || true)"
    if [[ -z "$slug" || -f ${JSON.stringify(`${slugFile}.stopped`)} ]]; then
      printf '[]\\n'
    else
      printf '[{"labels":{"slug":"%s"},"name":"%s"}]\\n' "$slug" "$slug"
    fi
    ;;
  stop)
    printf stopped >${JSON.stringify(`${slugFile}.stopped`)}
    ;;
  cleanup)
    printf 'skip codespace=none reason=missing claim\\n'
    ;;
  *)
    printf 'unexpected args: %s\\n' "$*" >&2
    exit 99
    ;;
esac
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: path.join(dir, "crabbox"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "codespaces",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REPO: "example-org/my-app",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REF: "main",
      CRABBOX_GITHUB_CODESPACES_SMOKE_MACHINE: "basicLinux32gb",
      CRABBOX_GITHUB_CODESPACES_SMOKE_DEVCONTAINER_PATH: ".devcontainer/devcontainer.json",
      GH_TOKEN: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_github_codespaces_smoke_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-secret-token/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(
    seen[0],
    "doctor --provider github-codespaces --github-codespaces-repo example-org/my-app --github-codespaces-ref main --github-codespaces-machine basicLinux32gb --github-codespaces-devcontainer-path .devcontainer/devcontainer.json --github-codespaces-delete-on-release=true",
  );
  assert.match(
    seen[1],
    /^warmup --provider github-codespaces --github-codespaces-repo example-org\/my-app --github-codespaces-ref main --github-codespaces-machine basicLinux32gb --github-codespaces-devcontainer-path \.devcontainer\/devcontainer\.json --github-codespaces-delete-on-release=true --slug github-codespaces-smoke-\d{14}-\d+ --keep=false --ttl 20m --idle-timeout 5m$/,
  );
  assert.match(seen[2], /^status --provider github-codespaces --id github-codespaces-smoke-\d{14}-\d+ --wait --wait-timeout 600s$/);
  assert.match(seen[3], /^run --provider github-codespaces --id github-codespaces-smoke-\d{14}-\d+ --full-resync -- sh -lc test -f go\.mod && echo github-codespaces-smoke-ok$/);
  assert.match(seen[4], /^ssh --provider github-codespaces --id github-codespaces-smoke-\d{14}-\d+$/);
  assert.equal(seen[5], "list --provider github-codespaces --json");
  assert.match(seen[6], /^stop --provider github-codespaces github-codespaces-smoke-\d{14}-\d+$/);
  assert.equal(seen[7], "cleanup --provider github-codespaces --dry-run");
  assert.equal(seen[8], "list --provider github-codespaces --json");
});

test("live github codespaces smoke attempts cleanup after partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
  const calls = path.join(dir, "calls.log");
  const ghCalls = path.join(dir, "gh.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeFakeGH(binDir, ghCalls);
  writeFakeCrabbox(
    path.join(dir, "crabbox"),
    `printf '%s\\n' "$*" >>${JSON.stringify(calls)}
if [[ "$1" == "doctor" ]]; then
  printf 'auth=ready\\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  printf 'created codespace before failing\\n' >&2
  exit 37
fi
if [[ "$1" == "stop" ]]; then
  printf '%s\\n' "$4" >>${JSON.stringify(stopped)}
  exit 0
fi
exit 99
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: path.join(dir, "crabbox"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "github-codespaces",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REPO: "example-org/my-app",
      GH_TOKEN: "test-secret-token",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(result.stderr, /created codespace before failing/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^github-codespaces-smoke-\d{14}-\d+\n$/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-secret-token/);
});
