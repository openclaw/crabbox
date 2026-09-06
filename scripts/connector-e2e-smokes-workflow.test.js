import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github", "workflows", "connector-e2e-smokes.yml"),
  "utf8",
);

test("connector lifecycle gate runs on pull requests, main pushes, and manual dispatch only", () => {
  const trigger = workflow.slice(workflow.indexOf("\non:"), workflow.indexOf("permissions:"));
  assert.match(trigger, /pull_request:/);
  assert.match(trigger, /push:\s*\n\s+branches:\s*\n\s+- main/);
  assert.match(trigger, /workflow_dispatch:/);
  assert.doesNotMatch(
    workflow,
    /schedule:/,
    "the hermetic gate must not run on a schedule; tier 2 needs a maintainer credential policy first",
  );
});

test("connector lifecycle gate stays hermetic: no secret references anywhere", () => {
  assert.ok(
    !workflow.includes("secrets."),
    "workflow must not reference secrets; tier 1 of https://github.com/openclaw/crabbox/issues/944 is zero-credential",
  );
});

test("matrix rows do not fail fast and are time-bounded", () => {
  assert.match(workflow, /fail-fast:\s*false/);
  assert.match(workflow, /timeout-minutes: \$\{\{ matrix\.timeout-minutes \|\| 15 \}\}/);
  const localContainer = workflow.match(
    /- name: local-container\n([\s\S]*?)(?=\n {10}- name:)/,
  )?.[1];
  assert.ok(localContainer, "local-container row exists");
  assert.match(localContainer, /timeout-minutes: 30/);
  assert.equal((workflow.match(/^ {12}timeout-minutes:/gm) ?? []).length, 1);
});

test("SSH localhost asserts amd64 only on its hosted x64 lifecycle row", () => {
  const rows = [...workflow.matchAll(/^ {10}- name: ssh-localhost\n((?: {12}[^\n]*\n)*)/gm)];
  assert.equal(rows.length, 1, "keep one existing SSH localhost row");
  const row = rows[0][1];
  assert.match(row, /^ {12}runner: ubuntu-latest$/m);
  assert.match(row, /^ {12}build-cli: true$/m);
  assert.match(
    row,
    /^ {12}smoke: CRABBOX_ARCH=amd64 CRABBOX_BIN="\$PWD\/bin\/crabbox" scripts\/live-ssh-localhost-smoke\.sh$/m,
  );
  assert.equal((workflow.match(/CRABBOX_ARCH/g) ?? []).length, 1);
  assert.match(workflow, /runs-on: \$\{\{ matrix\.runner \}\}/);
  assert.match(workflow, /run: \$\{\{ matrix\.smoke \}\}/);
  assert.match(workflow, /permissions:\n {2}contents: read\n\n/);
  assert.doesNotMatch(
    workflow,
    /pull_request_target:|self-hosted|^\s*(?:environment|services|container|continue-on-error):/m,
  );
});

test("pull request runs cancel superseded attempts", () => {
  assert.match(workflow, /concurrency:\s*\n\s+group:/);
  assert.match(workflow, /cancel-in-progress:.*pull_request/);
});

test("failed bootstrap diagnostics read only the unique smoke container", (t) => {
  const marker = "      - name: Diagnose local-container bootstrap\n";
  const step = workflow.slice(workflow.indexOf(marker) + marker.length);
  const script = step
    .slice(step.indexOf("        run: |\n") + "        run: |\n".length)
    .split("\n")
    .map((line) => line.replace(/^ {10}/, ""))
    .join("\n");
  assert.match(step, /if: failure\(\) && matrix\.name == 'local-container'/);
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-bootstrap-diagnostics-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  const docker = path.join(dir, "docker");
  fs.writeFileSync(
    docker,
    `#!/usr/bin/env bash
set -eu
printf '%s\\n' "$*" >> "$DIAGNOSTIC_CALLS"
case "$1" in
  ps) printf '%s\\n' "$DIAGNOSTIC_IDS" ;;
  inspect) printf '{"Running":false,"ExitCode":100}\\n' ;;
  logs) printf 'synthetic apt failure\\n' ;;
  *) exit 99 ;;
esac
`,
    { mode: 0o755 },
  );
  fs.writeFileSync(path.join(dir, "timeout"), '#!/usr/bin/env bash\nshift\nexec "$@"\n', {
    mode: 0o755,
  });
  const log = path.join(dir, "local-container-smoke.log");
  const calls = path.join(dir, "calls.log");
  const lease = "cbx_123456789abc";
  const container = "a".repeat(64);
  const launch = `provisioning provider=local-container lease=${lease} slug=synthetic\n`;
  for (const scenario of [
    { name: "exact", log: launch, ids: container, inspect: true },
    { name: "missing log", log: null, ids: container, inventory: false },
    { name: "no lease", log: "build failed\n", ids: container, inventory: false },
    { name: "multiple leases", log: launch + launch, ids: container, inventory: false },
    { name: "no container", log: launch, ids: "" },
    { name: "ambiguous containers", log: launch, ids: container + "\n" + "b".repeat(64) },
  ]) {
    fs.writeFileSync(calls, "");
    if (scenario.log === null) fs.rmSync(log, { force: true });
    else fs.writeFileSync(log, scenario.log);
    const result = spawnSync("bash", ["-euo", "pipefail", "-c", script], {
      encoding: "utf8",
      env: {
        ...process.env,
        PATH: `${dir}:${process.env.PATH}`,
        RUNNER_TEMP: dir,
        DIAGNOSTIC_CALLS: calls,
        DIAGNOSTIC_IDS: scenario.ids,
      },
    });
    assert.equal(result.status, 0, `${scenario.name}: ${result.stdout}${result.stderr}`);
    const observed = fs.readFileSync(calls, "utf8");
    if (scenario.inventory === false) assert.equal(observed, "", scenario.name);
    else
      assert.match(
        observed,
        new RegExp(
          `^ps -aq --no-trunc --filter label=crabbox=true --filter label=provider=local-container --filter label=lease=${lease}\\n`,
        ),
      );
    if (scenario.inspect) {
      assert.match(
        observed,
        new RegExp(`inspect --format \\{\\{json \\.State\\}\\} ${container}\\n`),
      );
      assert.match(observed, new RegExp(`logs --tail 100 ${container}\\n`));
      assert.match(result.stdout, /synthetic apt failure/);
    } else assert.doesNotMatch(observed, /^(inspect|logs) /m, scenario.name);
    assert.doesNotMatch(observed, /Config|(^|\n)(rm|exec|stop|kill) /);
  }
});
