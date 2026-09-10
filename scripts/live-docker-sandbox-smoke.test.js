import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";
import { copySmokeRepo, writeExecutable, writeGoStub } from "./test-support/smoke-fixtures.mjs";

const repoRoot = path.resolve(import.meta.dirname, "..");

const prepareSmokeRepo = (dir) =>
  copySmokeRepo(dir, path.join(repoRoot, "scripts", "live-docker-sandbox-smoke.sh"), [
    "lib/live-smoke-json-match.py",
  ]);

test("live docker sandbox smoke honors configured alternate sbx path", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-sbx-smoke-"));
  const bin = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const fakeSbx = path.join(dir, "fake-sbx");
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(bin);

  writeExecutable(
    fakeSbx,
    `#!/usr/bin/env bash
set -euo pipefail
exit 0
`,
  );

  writeExecutable(
    path.join(bin, "go"),
    `#!/usr/bin/env bash
set -euo pipefail
mkdir -p bin
cat <<'EOF' > bin/crabbox
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${calls}"
if [[ "$1" == "doctor" ]]; then
  if [[ -z "\${CRABBOX_DOCKER_SANDBOX_CLI:-}" || ! -x "\${CRABBOX_DOCKER_SANDBOX_CLI}" ]]; then
    printf 'missing configured docker sandbox cli\n' >&2
    exit 92
  fi
  printf 'ok      sbx_version provider=docker-sandbox version=sbx client fake\n'
fi
if [[ "$1" == "warmup" ]]; then
  printf '%s\n' "$5" >"${slugFile}"
fi
if [[ "$1" == "list" ]]; then
  slug="$(cat "${slugFile}")"
  printf '[{"name":"sandbox","labels":{"slug":"%s"}}]\\n' "$slug"
fi
exit 0
EOF
chmod +x bin/crabbox
`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${bin}${path.delimiter}/bin${path.delimiter}/usr/bin`,
      CRABBOX_DOCKER_SANDBOX_CLI: fakeSbx,
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /classification=live_sbx_smoke_passed/);
  assert.match(result.stdout, /sbx_version/);
  assert.doesNotMatch(result.stderr, /sbx not found on PATH/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen.length, 6, JSON.stringify(seen));
  assert.equal(seen[0], "doctor --provider docker-sandbox");
  assert.match(
    seen[1],
    /^warmup --provider docker-sandbox --slug docker-sandbox-smoke-\d{14}-\d+ --keep$/,
  );
  assert.match(
    seen[2],
    /^run --provider docker-sandbox --id docker-sandbox-smoke-\d{14}-\d+ -- echo ok$/,
  );
  assert.match(
    seen[3],
    /^run --provider docker-sandbox --id docker-sandbox-smoke-\d{14}-\d+ -- pwd$/,
  );
  assert.match(seen[4], /^list --provider docker-sandbox --json$/);
  assert.match(seen[5], /^stop --provider docker-sandbox docker-sandbox-smoke-\d{14}-\d+$/);
});

test("live docker sandbox smoke stops a sandbox after partial warmup failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-docker-sandbox-warmup-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2 $3" == "doctor --provider docker-sandbox" ]]; then
  printf 'ok      sbx_version provider=docker-sandbox version=sbx client fake\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  printf 'created sandbox before failing\n' >&2
  exit 37
fi
if [[ "$1" == "stop" ]]; then
  printf '%s\n' "$4" >>"${stopped}"
  exit 0
fi
printf 'unexpected crabbox args: %s\n' "$*" >&2
exit 99`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      HOME: process.env.HOME ?? dir,
      TMPDIR: process.env.TMPDIR ?? os.tmpdir(),
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(result.stderr, /created sandbox before failing/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^docker-sandbox-smoke-\d{14}-\d+\n$/);
});

for (const testCase of [
  {
    name: "invalid list JSON",
    listOutput: "not-json",
    stderrPattern: /classification=validation_failed/,
  },
  {
    name: "list JSON without warmed slug",
    listOutput: "[]",
    stderrPattern: /list JSON did not include slug/,
  },
]) {
  test(`live docker sandbox smoke rejects ${testCase.name}`, () => {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-docker-sandbox-list-"));
    const binDir = path.join(dir, "bin");
    const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
    const stopped = path.join(dir, "stopped.log");
    fs.mkdirSync(binDir, { recursive: true });

    writeGoStub(
      binDir,
      `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2 $3" == "doctor --provider docker-sandbox" ]]; then
  printf 'ok      sbx_version provider=docker-sandbox version=sbx client fake\n'
  exit 0
fi
if [[ "$1" == "warmup" ]]; then
  exit 0
fi
if [[ "$1" == "run" ]]; then
  exit 0
fi
if [[ "$1" == "list" ]]; then
  cat <<'JSON'
${testCase.listOutput}
JSON
  exit 0
fi
if [[ "$1" == "stop" ]]; then
  printf '%s\n' "$4" >>"${stopped}"
  exit 0
fi
printf 'unexpected crabbox args: %s\n' "$*" >&2
exit 99`,
    );

    const result = spawnSync("bash", [smokeScript], {
      cwd: tempRoot,
      env: {
        ...process.env,
        PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
        HOME: process.env.HOME ?? dir,
        TMPDIR: process.env.TMPDIR ?? os.tmpdir(),
      },
      encoding: "utf8",
    });

    assert.equal(result.status, 1, result.stdout + result.stderr);
    assert.match(result.stderr, testCase.stderrPattern);
    assert.match(fs.readFileSync(stopped, "utf8"), /^docker-sandbox-smoke-\d{14}-\d+\n$/);
  });
}

test("live docker sandbox smoke preserves JSON parser selection and failure boundaries", async (t) => {
  const realTools = Object.fromEntries(
    ["python3", "node", "jq"].map((name) => {
      const result = spawnSync("sh", ["-c", `command -v ${name}`], { encoding: "utf8" });
      assert.equal(result.status, 0, `${name} is required for parser selection coverage`);
      return [name, result.stdout.trim()];
    }),
  );

  for (const testCase of [
    { name: "python", hidden: [], expectedStatus: 0, selected: "python3" },
    { name: "node", hidden: ["python3"], expectedStatus: 0, selected: "node" },
    { name: "jq", hidden: ["python3", "node"], expectedStatus: 0, selected: "jq" },
    { name: "none", hidden: ["python3", "node", "jq"], expectedStatus: 1, selected: "" },
    {
      name: "node failure does not fall through to jq",
      hidden: ["python3"],
      expectedStatus: 7,
      selected: "node",
      failing: "node",
    },
  ]) {
    await t.test(testCase.name, () => {
      const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-docker-parser-"));
      const binDir = path.join(dir, "bin");
      const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
      const selectedFile = path.join(dir, "selected.log");
      const slugFile = path.join(dir, "slug.txt");
      const bashEnv = path.join(dir, "bash-env");
      fs.mkdirSync(binDir, { recursive: true });

      fs.writeFileSync(
        bashEnv,
        `command() {
  if [[ "\${1:-}" == "-v" ]]; then
    case "\${2:-}" in
${testCase.hidden.map((name) => `      ${name}) return 1 ;;`).join("\n")}
    esac
  fi
  builtin command "$@"
}
`,
        "utf8",
      );

      for (const name of ["python3", "node", "jq"]) {
        writeExecutable(
          path.join(binDir, name),
          `#!/usr/bin/env bash
printf '%s\\n' ${JSON.stringify(name)} >>${JSON.stringify(selectedFile)}
${testCase.failing === name ? "exit 7" : `exec ${JSON.stringify(realTools[name])} "$@"`}
`,
        );
      }

      writeGoStub(
        binDir,
        `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  doctor) printf 'ok      sbx_version provider=docker-sandbox version=sbx-client-fake\\n' ;;
  warmup)
    while [[ "$#" -gt 0 ]]; do
      if [[ "$1" == "--slug" ]]; then printf '%s' "$2" >${JSON.stringify(slugFile)}; break; fi
      shift
    done
    ;;
  run|stop) exit 0 ;;
  list) printf '[{"labels":{"slug":"%s"}}]\\n' "$(cat ${JSON.stringify(slugFile)})" ;;
  *) exit 99 ;;
esac`,
      );

      const result = spawnSync("bash", [smokeScript], {
        cwd: tempRoot,
        env: {
          ...process.env,
          BASH_ENV: bashEnv,
          PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
        },
        encoding: "utf8",
      });

      assert.equal(result.status, testCase.expectedStatus, result.stdout + result.stderr);
      const selected = fs.existsSync(selectedFile)
        ? fs.readFileSync(selectedFile, "utf8").trim().split("\n")
        : [];
      assert.deepEqual(selected, testCase.selected ? [testCase.selected] : []);
      if (testCase.name === "none") {
        assert.match(result.stderr, /no JSON parser available/);
      }
      if (testCase.failing) {
        assert.doesNotMatch(selected.join("\n"), /^jq$/m);
      }
    });
  }
});

test("live docker sandbox smoke classifies provider preflight failures", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-docker-sandbox-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2 $3" == "doctor --provider docker-sandbox" ]]; then
  printf 'virtualization unavailable\n' >&2
  exit 23
fi
printf 'unexpected crabbox args: %s\n' "$*" >&2
exit 99`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      HOME: process.env.HOME ?? dir,
      TMPDIR: process.env.TMPDIR ?? os.tmpdir(),
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 23, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(result.stderr, /doctor\\ --provider\\ docker-sandbox/);
  assert.match(result.stderr, /virtualization unavailable/);
});

test("live docker sandbox smoke classifies quota-like provider blockers", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-docker-sandbox-quota-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(binDir, { recursive: true });

  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
if [[ "$1 $2 $3" == "doctor --provider docker-sandbox" ]]; then
  printf 'Docker Sandbox quota exceeded for this account\n' >&2
  exit 29
fi
printf 'unexpected crabbox args: %s\n' "$*" >&2
exit 99`,
  );

  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      HOME: process.env.HOME ?? dir,
      TMPDIR: process.env.TMPDIR ?? os.tmpdir(),
    },
    encoding: "utf8",
  });

  assert.equal(result.status, 29, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=quota_blocked/);
  assert.match(result.stderr, /quota exceeded/);
});
