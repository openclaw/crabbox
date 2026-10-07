import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { spawnSync } from "node:child_process";

const repoRoot = path.resolve(import.meta.dirname, "..");
const workflow = fs.readFileSync(
  path.join(repoRoot, ".github", "workflows", "devtools-image-publish.yml"),
  "utf8",
);

function stepScript(name) {
  const step = workflow.split(`      - name: ${name}\n`)[1]?.split("\n      - name: ")[0];
  assert.ok(step, `missing workflow step: ${name}`);
  const run = step.match(/^        run: \|\n((?: {10}[^\n]*\n|\n)*)/m)?.[1];
  assert.ok(run, `missing shell body: ${name}`);
  return run
    .split("\n")
    .map((line) => line.slice(10))
    .join("\n");
}

function publicationFixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-publish-os-test-"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  fs.mkdirSync(path.join(dir, "scripts"));
  const output = path.join(dir, "mint.args");
  for (const name of ["mint-aws-devtools-image.sh", "mint-macos-devtools-image.sh"]) {
    fs.writeFileSync(
      path.join(dir, "scripts", name),
      '#!/usr/bin/env bash\nprintf "%s\\0" "${CRABBOX_OS-unset}" "$@" >"$MINT_ARGS"\n',
      { mode: 0o755 },
    );
  }
  const env = {
    ...process.env,
    TARGET: "linux",
    REGION: "eu-west-1",
    LINUX_TYPE: "m7i.large",
    LINUX_OS: "ubuntu:26.04",
    LINUX_ROOT_GB: "",
    WINDOWS_TYPE: "m7i.large",
    WINDOWS_OS: "windows-server:2022",
    MACOS_TYPE: "mac-m4.metal",
    MACOS_HOST: "use-existing",
    MEASURED: "false",
    MAX_P95_RUNNER_TOTAL_MS: "",
    RUNNER_TEMP: dir,
    GITHUB_SHA: "a".repeat(40),
    MINT_ARGS: output,
  };
  delete env.CRABBOX_OS;
  return {
    output,
    run(overrides = {}) {
      return spawnSync(
        "bash",
        [
          "-c",
          [
            stepScript("Validate publication inputs"),
            stepScript("Publish and prove image"),
            'printf "%s" "${CRABBOX_OS-unset}" >"$RUNNER_TEMP/after-os"',
          ].join("\n"),
        ],
        {
          cwd: dir,
          env: { ...env, ...overrides },
          encoding: "utf8",
          timeout: 10000,
        },
      );
    },
    inheritedOS: () => fs.readFileSync(path.join(dir, "after-os"), "utf8"),
  };
}

test("Linux publication offers Ubuntu 24 while retaining the Ubuntu 26 default", () => {
  const input = workflow.match(/^      linux_os:\n((?: {8}[^\n]*\n)+)/m)?.[1];
  assert.ok(input);
  assert.match(input, /default: ubuntu:26\.04/);
  assert.match(input, /type: choice/);
  assert.deepEqual(
    [...input.matchAll(/- (ubuntu:[0-9.]+)/g)].map((match) => match[1]),
    ["ubuntu:26.04", "ubuntu:24.04"],
  );
  assert.equal((workflow.match(/LINUX_OS: \$\{\{ inputs\.linux_os \}\}/g) ?? []).length, 2);
});

for (const [target, linuxOS, expectedOS] of [
  ["linux", "ubuntu:26.04", "ubuntu:26.04"],
  ["linux", "ubuntu:24.04", "ubuntu:24.04"],
  ["windows", "ubuntu:24.04", "unset"],
  ["macos", "ubuntu:24.04", "unset"],
]) {
  test(`publication dispatch scopes ${linuxOS} to the ${target} command`, (t) => {
    const fixture = publicationFixture(t);
    const result = fixture.run({ TARGET: target, LINUX_OS: linuxOS });
    assert.equal(result.status, 0, result.stderr);
    const args = fs.readFileSync(fixture.output, "utf8").split("\0").slice(0, -1);
    assert.equal(args[0], expectedOS);
    assert.equal(fixture.inheritedOS(), "unset");
    if (target !== "macos") {
      assert.equal(args[args.indexOf("--target") + 1], target);
      assert.ok(args.includes("--run"));
    }
  });
}

for (const linuxOS of ["", "ubuntu:22.04", "ubuntu:24.04\ntrue"]) {
  test(`publication guard rejects invalid Linux OS ${JSON.stringify(linuxOS)} before Mint`, (t) => {
    const fixture = publicationFixture(t);
    const result = fixture.run({ LINUX_OS: linuxOS });
    assert.equal(result.status, 2, result.stderr);
    assert.equal(result.signal, null);
    assert.match(result.stderr, /linux_os must be ubuntu:26\.04 or ubuntu:24\.04/);
    assert.equal(fs.existsSync(fixture.output), false);
  });
}

test("developer image publication is a protected manual admin workflow", () => {
  assert.match(workflow, /^  workflow_dispatch:$/m);
  assert.doesNotMatch(workflow, /^  (?:push|pull_request|schedule):/m);
  assert.match(workflow, /environment: image-publisher/);
  assert.match(
    workflow,
    /expected_workflow_ref="\$GITHUB_REPOSITORY\/\.github\/workflows\/devtools-image-publish\.yml@\$expected_ref"/,
  );
  assert.match(workflow, /\[\[ "\$GITHUB_REF" == "\$expected_ref" \]\]/);
  assert.match(workflow, /\[\[ "\$REF_PROTECTED" == true \]\]/);
  assert.match(workflow, /\[\[ "\$WORKFLOW_SHA" == "\$RUN_SHA" \]\]/);
  assert.match(workflow, /ref: \$\{\{ github\.workflow_sha \}\}/);
  assert.match(workflow, /persist-credentials: false/);
  assert.match(workflow, /cancel-in-progress: false/);
});

test("publication uses the existing source candidate promotion proof wrappers", () => {
  assert.match(workflow, /scripts\/mint-aws-devtools-image\.sh[\s\S]*--target linux[\s\S]*--run/);
  assert.match(
    workflow,
    /scripts\/mint-aws-devtools-image\.sh[\s\S]*--target windows[\s\S]*--windows-mode normal[\s\S]*--run/,
  );
  assert.match(workflow, /scripts\/mint-macos-devtools-image\.sh[\s\S]*"--\$MACOS_HOST"/);
  assert.doesNotMatch(workflow, /--no-promote/);
  assert.match(workflow, /go build -trimpath -o bin\/crabbox \.\/cmd\/crabbox/);
});

test("publication keeps credentials environment-scoped and retains proof", () => {
  assert.match(workflow, /CRABBOX_COORDINATOR: \$\{\{ vars\.CRABBOX_COORDINATOR \}\}/);
  assert.equal((workflow.match(/secrets\.CRABBOX_COORDINATOR_ADMIN_TOKEN/g) ?? []).length, 2);
  assert.doesNotMatch(workflow, /AWS_ACCESS_KEY_ID|AWS_SECRET_ACCESS_KEY/);
  assert.match(
    workflow,
    /name: Upload publication diagnostics\s+if: always\(\) && !inputs\.measured/,
  );
  assert.match(workflow, /if-no-files-found: error/);
  assert.match(workflow, /retention-days: 30/);
});

test("measured Linux publication is explicit and declares its threshold and extra launches", () => {
  assert.match(workflow, /measured:[\s\S]*type: boolean[\s\S]*default: false/);
  assert.match(workflow, /max_p95_runner_total_ms:/);
  assert.match(workflow, /MEASURED: \$\{\{ inputs\.measured \}\}/);
  assert.match(workflow, /MAX_P95_RUNNER_TOTAL_MS: \$\{\{ inputs\.max_p95_runner_total_ms \}\}/);
  assert.match(workflow, /\[\[ "\$TARGET" == linux \]\]/);
  assert.match(workflow, /\[\[ "\$MAX_P95_RUNNER_TOTAL_MS" =~ \^\[1-9\]\[0-9\]\*\$ \]\]/);
  assert.match(workflow, /12 planned leases/);
  assert.match(workflow, /no hard launch-attempt or dollar cap/);
  assert.match(
    workflow,
    /command\+=\(--measured --max-p95-runner-total-ms "\$MAX_P95_RUNNER_TOTAL_MS"\)/,
  );
  assert.match(
    workflow,
    /name: Upload allowlisted measurement manifest[\s\S]*if: always\(\) && inputs\.measured/,
  );
  assert.match(workflow, /path: \$\{\{ runner\.temp \}\}\/devtools-image-proof\/manifest\.json/);
  assert.match(workflow, /export CRABBOX_IMAGE_PUBLIC_OUTCOME="\$proof_dir\/manifest\.json"/);
  assert.match(workflow, /public_outcome="\$RUNNER_TEMP\/devtools-image-proof\/manifest\.json"/);
  assert.match(workflow, /name: Initialize measured publication outcome/);
  assert.match(workflow, /echo '- Status: `outcome_unavailable`'/);
  assert.match(workflow, /"\$\{command\[@\]\}" >"\$private_dir\/publish\.log" 2>&1/);
  assert.match(workflow, /devtools-image-proof\.mjs validate/);
  assert.doesNotMatch(workflow, /\bcp "\$\{manifests/);
  assert.doesNotMatch(workflow, /run-name:.*\$\{\{ inputs\.(?:region|linux_type)/);
  assert.doesNotMatch(workflow, /sanitized.*(?:logs|diagnostics)/i);
});

test("publication inputs remain within the 25-input dispatch limit", () => {
  const inputs = workflow.split("    inputs:\n")[1].split("\npermissions:")[0];
  assert.equal([...inputs.matchAll(/^      [a-z0-9_]+:$/gm)].length, 11);
  assert.match(inputs, /linux_root_gb:[\s\S]*default: ""/);
  assert.equal(
    (workflow.match(/LINUX_ROOT_GB: \$\{\{ inputs\.linux_root_gb \}\}/g) ?? []).length,
    2,
  );
});

test("Windows publication offers 2025 while retaining the 2022 default", () => {
  const input = workflow.match(/^      windows_os:\n((?: {8}[^\n]*\n)+)/m)?.[1];
  assert.ok(input);
  assert.match(input, /default: windows-server:2022/);
  assert.match(input, /type: choice/);
  assert.deepEqual([...input.matchAll(/- (windows-server:[0-9]+)/g)].map((m) => m[1]),
    ["windows-server:2022", "windows-server:2025"]);
  assert.equal((workflow.match(/WINDOWS_OS: \$\{\{ inputs\.windows_os \}\}/g) ?? []).length, 2);
});

for (const target of ["linux", "windows", "macos"]) {
  for (const year of ["2022", "2025"]) {
    test(`Windows ${year} dispatch scopes stock selection to ${target}`, (t) => {
      const fixture = publicationFixture(t);
      const result = fixture.run({ TARGET: target, WINDOWS_OS: `windows-server:${year}` });
      assert.equal(result.status, 0, result.stderr);
      const args = fs.readFileSync(fixture.output, "utf8").split("\0").slice(0, -1);
      const stock = target === "windows" && year === "2025";
      assert.equal(args[0], target === "linux" ? "ubuntu:26.04" : stock ? "windows-server:2025" : "unset");
      assert.equal(args.includes("--stock-source"), stock);
      assert.equal(fixture.inheritedOS(), "unset");
    });
  }
}

for (const windowsOS of ["", "windows-server:2019", "windows-server:2025\ntrue"]) {
  test(`publication guard rejects invalid Windows OS ${JSON.stringify(windowsOS)}`, (t) => {
    const fixture = publicationFixture(t);
    const result = fixture.run({ TARGET: "windows", WINDOWS_OS: windowsOS });
    assert.equal(result.status, 2, result.stderr);
    assert.match(result.stderr, /windows_os must be windows-server:2022 or windows-server:2025/);
    assert.equal(fs.existsSync(fixture.output), false);
  });
}
for (const root of ["", "16", "40", "400"]) {
  test(`Linux root dispatch forwards source-only flags for ${JSON.stringify(root)}`, (t) => {
    const fixture = publicationFixture(t);
    const result = fixture.run({ LINUX_ROOT_GB: root });
    assert.equal(result.status, 0, result.stderr);
    const args = fs.readFileSync(fixture.output, "utf8").split("\0").slice(0, -1);
    assert.equal(args.includes("--stock-source"), root !== "");
    assert.equal(args.includes("--root-gb"), root !== "");
    if (root) assert.equal(args[args.indexOf("--root-gb") + 1], root);
  });
}
for (const overrides of [
  { LINUX_ROOT_GB: "15" },
  { LINUX_ROOT_GB: "401" },
  { LINUX_ROOT_GB: "040" },
  { LINUX_ROOT_GB: "1.5" },
  { LINUX_ROOT_GB: "999999999999999999999999" },
  { LINUX_ROOT_GB: "40\ntrue" },
  { LINUX_ROOT_GB: "40", TARGET: "windows" },
  { LINUX_ROOT_GB: "40", TARGET: "macos" },
]) {
  test(`Linux root guard rejects ${JSON.stringify(overrides)} before mint`, (t) => {
    const fixture = publicationFixture(t);
    const result = fixture.run(overrides);
    assert.equal(result.status, 2, result.stderr);
    assert.match(result.stderr, /linux_root_gb.*Linux.*16-400/);
    assert.equal(fs.existsSync(fixture.output), false);
  });
}
