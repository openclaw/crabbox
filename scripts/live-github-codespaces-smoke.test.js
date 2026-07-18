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
  const fixtureDir = path.join(tempScripts, "fixtures", "github-codespaces");
  const smokeScript = path.join(tempScripts, "live-github-codespaces-smoke.sh");
  fs.mkdirSync(fixtureDir, { recursive: true });
  fs.copyFileSync(path.join(repoRoot, "scripts", "live-github-codespaces-smoke.sh"), smokeScript);
  fs.copyFileSync(
    path.join(repoRoot, "scripts", "fixtures", "github-codespaces", "devcontainer.json"),
    path.join(fixtureDir, "devcontainer.json"),
  );
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
if [[ "$*" == "auth status --active --hostname github.com" ]]; then
  exit 0
fi
if [[ "$*" == "codespace list --limit 1" ]]; then
  printf '[]\\n'
  exit 0
fi
if [[ "$*" == "api repos/example-org/my-app --jq .default_branch" ]]; then
  printf 'trunk\\n'
  exit 0
fi
if [[ "$*" == codespace\\ list\\ --repo\\ *\\ --limit\\ 100\\ --json\\ name,displayName ]]; then
  if [[ -n "\${CRABBOX_FAKE_GH_LIST_WARNING:-}" ]]; then
    printf '%s\\n' "$CRABBOX_FAKE_GH_LIST_WARNING" >&2
  fi
  if [[ -n "\${CRABBOX_FAKE_REMOTE_SLUG_FILE:-}" && -f "$CRABBOX_FAKE_REMOTE_SLUG_FILE" ]]; then
    if [[ -n "\${CRABBOX_FAKE_REMOTE_DELETE_AFTER_LISTS:-}" ]]; then
      count_file="$CRABBOX_FAKE_REMOTE_SLUG_FILE.count"
      count="$(cat "$count_file" 2>/dev/null || printf '0')"
      count=$((count + 1))
      printf '%s' "$count" >"$count_file"
      if [[ "$count" -ge "$CRABBOX_FAKE_REMOTE_DELETE_AFTER_LISTS" ]]; then
        rm -f "$CRABBOX_FAKE_REMOTE_SLUG_FILE"
        printf '[]\\n'
        exit 0
      fi
    fi
    slug="$(cat "$CRABBOX_FAKE_REMOTE_SLUG_FILE")"
    printf '[{"name":"remote-owned-codespace","displayName":"%s"}]\\n' "$slug"
    exit 0
  fi
  printf '%s\\n' "\${CRABBOX_FAKE_REMOTE_CODESPACES:-[]}"
  exit 0
fi
if [[ "$*" == codespace\\ delete\\ --codespace\\ *\\ --force ]]; then
  if [[ -n "\${CRABBOX_FAKE_REMOTE_SLUG_FILE:-}" ]]; then
    rm -f "$CRABBOX_FAKE_REMOTE_SLUG_FILE"
  fi
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
      GH_TOKEN: "test-token-placeholder",
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

test("live github codespaces smoke stops before mutation when codespace scope is missing", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-scope-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const ghCalls = path.join(dir, "gh.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeExecutable(
    path.join(binDir, "gh"),
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>${JSON.stringify(ghCalls)}
if [[ "$*" == "auth status --active --hostname github.com" ]]; then
  exit 0
fi
if [[ "$*" == "codespace list --limit 1" ]]; then
  printf 'HTTP 403: Must have admin rights to Repository. This API operation needs the "codespace" scope. token test-token-placeholder\\n' >&2
  exit 1
fi
exit 97
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "github-codespaces",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REPO: "example-org/my-app",
      GH_TOKEN: "test-token-placeholder",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=credential_bound/);
  assert.match(result.stderr, /reason=github_codespaces_scope_missing/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-token-placeholder/);
  assert.deepEqual(fs.readFileSync(ghCalls, "utf8").trim().split("\n"), [
    "auth status --active --hostname github.com",
    "codespace list --limit 1",
  ]);
});

test("live github codespaces smoke fails non-credential scope probe errors", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-scope-error-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const crabboxCalls = path.join(dir, "crabbox.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeExecutable(
    path.join(binDir, "gh"),
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == "auth status --active --hostname github.com" ]]; then
  exit 0
fi
if [[ "$*" == "codespace list --limit 1" ]]; then
  printf 'TLS handshake timeout\\n' >&2
  exit 52
fi
exit 97
`,
  );
  writeFakeCrabbox(path.join(dir, "crabbox"), `printf '%s\\n' "$*" >>${JSON.stringify(crabboxCalls)}`);

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: path.join(dir, "crabbox"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "github-codespaces",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REPO: "example-org/my-app",
      GH_TOKEN: "test-token-placeholder",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 52, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /TLS handshake timeout/);
  assert.doesNotMatch(result.stderr, /classification=credential_bound/);
  assert.equal(fs.existsSync(crabboxCalls), false);
});

test("live github codespaces smoke classifies rate-limited scope probes as quota blockers", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-rate-limit-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(binDir, { recursive: true });
  writeExecutable(
    path.join(binDir, "gh"),
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == "auth status --active --hostname github.com" ]]; then
  exit 0
fi
if [[ "$*" == "codespace list --limit 1" ]]; then
  printf 'HTTP 403: API rate limit exceeded\\n' >&2
  exit 1
fi
exit 97
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "github-codespaces",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REPO: "example-org/my-app",
      GH_TOKEN: "test-token-placeholder",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=quota_blocked/);
  assert.doesNotMatch(result.stderr, /classification=credential_bound/);
});

test("live github codespaces smoke routes enterprise credentials to the provider host", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-enterprise-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const ghCalls = path.join(dir, "gh.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeExecutable(
    path.join(binDir, "gh"),
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>${JSON.stringify(ghCalls)}
[[ "\${GH_HOST:-}" == "api.enterprise.example:8443" ]]
[[ "\${GH_ENTERPRISE_TOKEN:-}" == "test-token-placeholder" ]]
[[ -z "\${GH_TOKEN:-}" ]]
if [[ "$*" == "auth status --active --hostname api.enterprise.example:8443" ]]; then
  exit 0
fi
if [[ "$*" == "codespace list --limit 1" ]]; then
  printf '[]\\n'
  exit 0
fi
exit 97
`,
  );
  writeFakeCrabbox(path.join(dir, "crabbox"), "printf 'enterprise provider validation\\n' >&2; exit 12");

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: path.join(dir, "crabbox"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "github-codespaces",
      CRABBOX_GITHUB_CODESPACES_API_URL: "https://api.enterprise.example:8443/api/v3",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REPO: "example-org/my-app",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REF: "main",
      GH_TOKEN: "",
      GITHUB_TOKEN: "",
      GH_ENTERPRISE_TOKEN: "test-token-placeholder",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 12, result.stdout + result.stderr);
  assert.match(result.stderr, /enterprise provider validation/);
  assert.deepEqual(fs.readFileSync(ghCalls, "utf8").trim().split("\n"), [
    "auth status --active --hostname api.enterprise.example:8443",
    "codespace list --limit 1",
  ]);
});

test("live github codespaces smoke requires Python before provider mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-python-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const ghCalls = path.join(dir, "gh.log");
  const crabboxCalls = path.join(dir, "crabbox.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeFakeGH(binDir, ghCalls);
  writeFakeCrabbox(path.join(dir, "crabbox"), `printf '%s\\n' "$*" >>${JSON.stringify(crabboxCalls)}`);

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: path.join(dir, "crabbox"),
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "github-codespaces",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REPO: "example-org/my-app",
      CRABBOX_GITHUB_CODESPACES_PYTHON_PATH: path.join(dir, "missing-python"),
      GH_TOKEN: "test-token-placeholder",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=python_missing/);
  assert.equal(fs.existsSync(crabboxCalls), false);
  assert.equal(fs.existsSync(ghCalls), false);
});

test("live github codespaces smoke fails provider errors that mention token options", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-command-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const ghCalls = path.join(dir, "gh.log");
  const crabboxCalls = path.join(dir, "crabbox.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeFakeGH(binDir, ghCalls);
  writeFakeCrabbox(
    path.join(dir, "crabbox"),
    `if [[ "\${CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH+x}" == "x" ]]; then
  printf 'base devcontainer leaked into smoke subprocess\\n' >&2
  exit 13
fi
printf '%s\\n' "$*" >>${JSON.stringify(crabboxCalls)}
printf 'invalid provider flag; usage: --token string\\n' >&2
exit 12`,
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
      CRABBOX_GITHUB_CODESPACES_SMOKE_REF: "main",
      CRABBOX_GITHUB_CODESPACES_SMOKE_DEVCONTAINER_PATH: "",
      CRABBOX_GITHUB_CODESPACES_DEVCONTAINER_PATH: ".devcontainer/base.json",
      GH_TOKEN: "test-token-placeholder",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 12, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /invalid provider flag; usage: --token string/);
  assert.equal(fs.readFileSync(crabboxCalls, "utf8").trim().split("\n").length, 1);
  assert.doesNotMatch(fs.readFileSync(crabboxCalls, "utf8"), /devcontainer/);
});

test("live github codespaces smoke rebuilds the default binary", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-fresh-build-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const ghCalls = path.join(dir, "gh.log");
  const buildLog = path.join(dir, "go.log");
  const freshCrabbox = path.join(dir, "fresh-crabbox");
  const checkoutBin = path.join(tempRoot, "bin");
  fs.mkdirSync(binDir, { recursive: true });
  fs.mkdirSync(checkoutBin, { recursive: true });
  writeFakeGH(binDir, ghCalls);
  writeFakeCrabbox(path.join(checkoutBin, "crabbox"), "printf 'stale binary used\\n' >&2; exit 77");
  writeFakeCrabbox(freshCrabbox, "printf 'fresh provider validation\\n' >&2; exit 12");
  writeExecutable(
    path.join(binDir, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\\n' "$*" >>${JSON.stringify(buildLog)}
cp ${JSON.stringify(freshCrabbox)} "$4"
chmod +x "$4"
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_BIN: "",
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "github-codespaces",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REPO: "example-org/my-app",
      CRABBOX_GITHUB_CODESPACES_SMOKE_REF: "main",
      GH_TOKEN: "test-token-placeholder",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 12, result.stdout + result.stderr);
  assert.match(fs.readFileSync(buildLog, "utf8"), /^build -trimpath -o bin\/crabbox \.\/cmd\/crabbox/m);
  assert.match(result.stderr, /fresh provider validation/);
  assert.doesNotMatch(result.stderr, /stale binary used/);
});

test("live github codespaces smoke fails classified errors after provisioning", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-validation-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const ghCalls = path.join(dir, "gh.log");
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeFakeGH(binDir, ghCalls);
  writeFakeCrabbox(
    path.join(dir, "crabbox"),
    `printf '%s\\n' "$*" >>${JSON.stringify(calls)}
case "$1" in
  doctor|warmup|stop) exit 0 ;;
  status)
    printf 'GitHub Codespaces SSH is unsupported\\n' >&2
    exit 42
    ;;
  *) exit 99 ;;
esac`,
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
      CRABBOX_GITHUB_CODESPACES_SMOKE_REF: "main",
      GH_TOKEN: "test-token-placeholder",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 42, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /GitHub Codespaces SSH is unsupported/);
  assert.doesNotMatch(result.stderr, /classification=environment_blocked/);
  assert.match(
    fs.readFileSync(calls, "utf8"),
    /stop --provider github-codespaces --github-codespaces-delete-on-release=true/,
  );
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
if [[ "\${GH_TOKEN:-}" != "test-token-placeholder" ]]; then
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
    printf 'successful list warning\\n' >&2
    slug="$(cat ${JSON.stringify(slugFile)} 2>/dev/null || true)"
    if [[ -z "$slug" || -f ${JSON.stringify(`${slugFile}.stopped`)} ]]; then
      printf '[{"labels":{"slug":"unrelated-retained-lease"},"name":"unrelated-retained-lease"}]\\n'
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
      CRABBOX_GITHUB_CODESPACES_SMOKE_DEVCONTAINER_PATH:
        "scripts/fixtures/github-codespaces/devcontainer.json",
      CRABBOX_FAKE_REMOTE_SLUG_FILE: slugFile,
      CRABBOX_FAKE_REMOTE_DELETE_AFTER_LISTS: "2",
      CRABBOX_FAKE_GH_LIST_WARNING: "successful gh list warning",
      GH_TOKEN: "test-token-placeholder",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_github_codespaces_smoke_passed/);
  assert.match(result.stderr, /successful list warning/);
  assert.match(result.stderr, /successful gh list warning/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-token-placeholder/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  const smokeSlug = seen[1].match(/--slug (\S+)/)?.[1] ?? "";
  assert.match(smokeSlug, /^g[0-9a-f]{12}$/);
  assert.equal(
    seen[0],
    "doctor --provider github-codespaces --github-codespaces-repo example-org/my-app --github-codespaces-ref main --github-codespaces-machine basicLinux32gb --github-codespaces-delete-on-release=true --github-codespaces-devcontainer-path scripts/fixtures/github-codespaces/devcontainer.json",
  );
  assert.match(
    seen[1],
    /^warmup --provider github-codespaces --github-codespaces-repo example-org\/my-app --github-codespaces-ref main --github-codespaces-machine basicLinux32gb --github-codespaces-delete-on-release=true --github-codespaces-devcontainer-path scripts\/fixtures\/github-codespaces\/devcontainer\.json --slug g[0-9a-f]{12} --keep=true --ttl 20m --idle-timeout 5m$/,
  );
  assert.match(seen[2], /^status --provider github-codespaces --id g[0-9a-f]{12} --wait --wait-timeout 600s$/);
  assert.match(seen[3], /^run --provider github-codespaces --id g[0-9a-f]{12} --full-resync -- sh -lc cleanup\(\) \{ git reset --hard HEAD/);
  assert.match(seen[4], /^ssh --provider github-codespaces --id g[0-9a-f]{12}$/);
  assert.equal(seen[5], "list --provider github-codespaces --json");
  assert.match(
    seen[6],
    /^stop --provider github-codespaces --github-codespaces-delete-on-release=true g[0-9a-f]{12}$/,
  );
  assert.equal(seen[7], "cleanup --provider github-codespaces --dry-run");
  assert.equal(seen[8], "list --provider github-codespaces --json");
  assert.deepEqual(fs.readFileSync(ghCalls, "utf8").trim().split("\n"), [
    "auth status --active --hostname github.com",
    "codespace list --limit 1",
    "codespace list --repo example-org/my-app --limit 100 --json name,displayName",
    "codespace list --repo example-org/my-app --limit 100 --json name,displayName",
  ]);
});

test("live github codespaces smoke reports malformed final remote inventory", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-malformed-remote-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });
  writeExecutable(
    path.join(binDir, "gh"),
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$*" == "auth status --active --hostname github.com" ]]; then exit 0; fi
if [[ "$*" == "codespace list --limit 1" ]]; then printf '[]\\n'; exit 0; fi
if [[ "$*" == codespace\\ list\\ --repo\\ *\\ --limit\\ 100\\ --json\\ name,displayName ]]; then
  printf 'not-json\\n'
  exit 0
fi
exit 97
`,
  );
  writeFakeCrabbox(
    path.join(dir, "crabbox"),
    `case "$1" in
  doctor|status|stop|cleanup) exit 0 ;;
  warmup)
    for ((i=1; i<=\$#; i++)); do
      if [[ "\${!i}" == "--slug" ]]; then
        j=\$((i + 1))
        printf '%s\\n' "\${!j}" >${JSON.stringify(slugFile)}
      fi
    done
    ;;
  run) printf 'github-codespaces-smoke-ok\\n' ;;
  ssh) printf 'ssh -F /tmp/github-codespaces.conf codespace.example\\n' ;;
  list)
    if [[ -f ${JSON.stringify(slugFile)} ]]; then
      slug="\$(cat ${JSON.stringify(slugFile)})"
      printf '[{"labels":{"slug":"%s"}}]\\n' "\$slug"
      rm -f ${JSON.stringify(slugFile)}
    else
      printf '[]\\n'
    fi
    ;;
  *) exit 99 ;;
esac`,
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
      CRABBOX_GITHUB_CODESPACES_SMOKE_REF: "main",
      CRABBOX_GITHUB_CODESPACES_SMOKE_DELETE_TIMEOUT_SECONDS: "1",
      GH_TOKEN: "test-token-placeholder",
    },
    encoding: "utf8",
  });

  assert.notEqual(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /JSONDecodeError/);
});

test("live github codespaces smoke attempts cleanup after partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
  const remoteSlug = path.join(dir, "remote-slug.txt");
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
  for ((i=1; i<=$#; i++)); do
    if [[ "\${!i}" == "--slug" ]]; then
      j=$((i + 1))
      printf '%s\\n' "\${!j}" >${JSON.stringify(remoteSlug)}
    fi
  done
    printf 'capacity unavailable after creating codespace\\n' >&2
    exit 37
fi
if [[ "$1" == "stop" ]]; then
  printf '%s\\n' "\${!#}" >>${JSON.stringify(stopped)}
  if [[ -f ${JSON.stringify(remoteSlug)} ]]; then
    printf 'refusing to delete dirty Codespace\\n' >&2
    exit 43
  fi
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
      CRABBOX_FAKE_REMOTE_SLUG_FILE: remoteSlug,
      GH_TOKEN: "test-token-placeholder",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=quota_blocked/);
  assert.match(result.stderr, /classification=cleanup_fallback/);
  assert.match(result.stderr, /refusing to delete dirty Codespace/);
  assert.match(result.stderr, /capacity unavailable after creating codespace/);
  const stoppedSlugs = fs.readFileSync(stopped, "utf8").trim().split("\n");
  assert.equal(stoppedSlugs.length, 2);
  assert.match(stoppedSlugs[0], /^g[0-9a-f]{12}$/);
  assert.equal(stoppedSlugs[1], stoppedSlugs[0]);
  assert.match(
    fs.readFileSync(ghCalls, "utf8"),
    /codespace delete --codespace remote-owned-codespace --force/,
  );
  assert.match(fs.readFileSync(ghCalls, "utf8"), /api repos\/example-org\/my-app --jq \.default_branch/);
  assert.match(fs.readFileSync(calls, "utf8"), /--github-codespaces-ref trunk/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-token-placeholder/);
});

test("live github codespaces smoke fails cleanup when local inventory cannot be verified", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-ghcs-cleanup-list-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const ghCalls = path.join(dir, "gh.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeFakeGH(binDir, ghCalls);
  writeFakeCrabbox(
    path.join(dir, "crabbox"),
    `case "$1" in
  doctor) exit 0 ;;
  warmup)
    printf 'capacity unavailable after creating codespace\\n' >&2
    exit 37
    ;;
  stop)
    printf 'dirty checkout blocks provider deletion\\n' >&2
    exit 43
    ;;
  list)
    printf 'inventory backend unavailable\\n' >&2
    exit 51
    ;;
  *) exit 99 ;;
esac`,
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
      CRABBOX_GITHUB_CODESPACES_SMOKE_REF: "main",
      GH_TOKEN: "test-token-placeholder",
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 43, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=quota_blocked/);
  assert.match(result.stderr, /reason=local_claim_reconciliation/);
  assert.match(result.stderr, /command=.*list.*exit=51/);
  assert.match(result.stderr, /inventory backend unavailable/);
});
