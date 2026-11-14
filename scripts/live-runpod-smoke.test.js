import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import http from "node:http";
import { spawn, spawnSync } from "node:child_process";
import test from "node:test";
import { copySmokeRepo, writeExecutable, writeGoStub } from "./test-support/smoke-fixtures.mjs";

const repoRoot = path.resolve(import.meta.dirname, "..");

const prepareSmokeRepo = (dir) =>
  copySmokeRepo(dir, path.join(repoRoot, "scripts", "live-runpod-smoke.sh"), [
    "lib/live-smoke-json-match.py",
  ]);

test("live RunPod smoke embedded cleanup Python compiles", () => {
  const script = fs.readFileSync(path.join(repoRoot, "scripts", "live-runpod-smoke.sh"), "utf8");
  const match = script.match(/raw_runpod_delete_slug\(\) \{\n  CRABBOX_SMOKE_SLUG="\$slug" python3 -c '\n([\s\S]*?)\n'\n\}/);
  assert.ok(match, "raw_runpod_delete_slug Python block should be extractable");
  const result = spawnSync("python3", ["-c", `compile(${JSON.stringify(match[1])}, "raw_runpod_delete_slug", "exec")`], {
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
});

test("live RunPod smoke skips unless opted in", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-runpod-skip-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: { ...process.env, CRABBOX_LIVE: "", CRABBOX_RUNPOD_API_KEY: "", RUNPOD_API_KEY: "" },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=CRABBOX_LIVE_not_enabled/);
});

test("live RunPod smoke skips unless provider filter selects RunPod", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-runpod-filter-"));
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "aws,digitalocean",
      RUNPOD_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=runpod_not_selected/);
});

test("live RunPod smoke requires a token before building", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-runpod-token-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  fs.mkdirSync(binDir, { recursive: true });
  writeExecutable(path.join(binDir, "go"), "#!/usr/bin/env bash\nexit 99\n");
  const result = spawnSync("bash", [smokeScript], {
    cwd: tempRoot,
    env: {
      ...process.env,
      PATH: `${binDir}${path.delimiter}${process.env.PATH ?? ""}`,
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "runpod",
      CRABBOX_RUNPOD_API_KEY: "",
      RUNPOD_API_KEY: "",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /classification=environment_blocked reason=RUNPOD_API_KEY_missing/);
});

test("live RunPod smoke runs guarded lifecycle without exposing the token", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-runpod-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  const slugFile = path.join(dir, "slug.txt");
  fs.mkdirSync(binDir, { recursive: true });
  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${calls}"
if [[ "\${RUNPOD_API_KEY:-}" != "test-secret-token" ]]; then
  exit 91
fi
case "$1" in
  doctor)
    printf 'auth=ready control_plane=ready inventory=ready mutation=false leases=0\n'
    ;;
  warmup)
    printf '%s\n' "$5" >"${slugFile}"
    ;;
  status)
    printf 'status=ready\n'
    ;;
  run)
    printf 'ok\n'
    ;;
  list)
    slug="$(cat "${slugFile}" 2>/dev/null || true)"
    if [[ -z "$slug" || -f "${slugFile}.stopped" ]]; then
      printf '[]\n'
    else
      printf '[{"labels":{"slug":"%s"}}]\n' "$slug"
    fi
    ;;
  stop)
    printf stopped >"${slugFile}.stopped"
    ;;
  *)
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
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "runpod",
      CRABBOX_RUNPOD_API_KEY: "test-secret-token",
      RUNPOD_API_KEY: "",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 0, result.stdout + result.stderr);
  assert.match(result.stdout, /classification=live_runpod_smoke_passed/);
  assert.doesNotMatch(result.stdout + result.stderr, /test-secret-token/);

  const seen = fs.readFileSync(calls, "utf8").trim().split("\n");
  assert.equal(seen[0], "doctor --provider runpod");
  assert.equal(seen[1], "list --provider runpod --json");
  assert.match(seen[2], /^warmup --provider runpod --slug runpod-smoke-\d{14}-\d+ --keep --ttl 20m --idle-timeout 5m$/);
  assert.match(seen[3], /^status --provider runpod --id runpod-smoke-\d{14}-\d+ --wait --wait-timeout 600s$/);
  assert.match(seen[4], /^run --provider runpod --id runpod-smoke-\d{14}-\d+ --no-sync -- echo ok$/);
  assert.equal(seen[5], "list --provider runpod --json");
  assert.match(seen[6], /^stop --provider runpod runpod-smoke-\d{14}-\d+$/);
  assert.equal(seen[7], "list --provider runpod --json");
});

test("live RunPod smoke attempts cleanup after a partial failure", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-runpod-fail-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const stopped = path.join(dir, "stopped.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  doctor|list)
    printf '[]\n'
    ;;
  warmup)
    printf 'created RunPod pod before failing\n' >&2
    exit 37
    ;;
  stop)
    printf '%s\n' "$4" >>"${stopped}"
    ;;
  *)
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
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "runpod",
      RUNPOD_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 37, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=environment_blocked/);
  assert.match(fs.readFileSync(stopped, "utf8"), /^runpod-smoke-\d{14}-\d+\n$/);
});

test("live RunPod smoke refuses non-empty inventory before mutation", () => {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-live-runpod-nonempty-"));
  const binDir = path.join(dir, "bin");
  const { tempRoot, smokeScript } = prepareSmokeRepo(dir);
  const calls = path.join(dir, "calls.log");
  fs.mkdirSync(binDir, { recursive: true });
  writeGoStub(
    binDir,
    `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${calls}"
case "$1" in
  doctor)
    printf 'auth=ready\n'
    ;;
  list)
    printf '[{"labels":{"slug":"existing"}}]\n'
    ;;
  *)
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
      CRABBOX_LIVE: "1",
      CRABBOX_LIVE_PROVIDERS: "runpod",
      RUNPOD_API_KEY: "test-secret-token",
    },
    encoding: "utf8",
  });
  assert.equal(result.status, 1, result.stdout + result.stderr);
  assert.match(result.stderr, /classification=validation_failed/);
  assert.match(result.stderr, /inventory is not empty/);
  assert.deepEqual(fs.readFileSync(calls, "utf8").trim().split("\n"), [
    "doctor --provider runpod",
    "list --provider runpod --json",
  ]);
});

// Exercise only the extracted cleanup helper, with an inert token and loopback API.
const cleanupPython = () => {
  const script = fs.readFileSync(path.join(repoRoot, "scripts", "live-runpod-smoke.sh"), "utf8");
  return script.match(/raw_runpod_delete_slug\(\) \{\n  CRABBOX_SMOKE_SLUG="\$slug" python3 -c '\n([\s\S]*?)\n'\n\}/)[1];
};
const pod = (id, name = "crabbox-fixture-run-abcd1234") => ({ id, name, status: "RUNNING" });
const page = (pods, nextCursor = null) => ({ pods, pagination: { hasNextPage: nextCursor !== null, nextCursor } });
async function cleanupFixture(handler, override) {
  const requests = [];
  const server = http.createServer((req, res) => {
    requests.push(`${req.method} ${req.url}`);
    assert.equal(req.headers.authorization, "Bearer inert-fixture-token");
    handler(req, res);
  });
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  try {
    const result = await new Promise((resolve, reject) => {
      const child = spawn("python3", ["-c", cleanupPython()], {
        env: { ...process.env, CRABBOX_SMOKE_SLUG: "fixture-run", RUNPOD_API_KEY: "inert-fixture-token", CRABBOX_RUNPOD_API_URL: override ?? `http://127.0.0.1:${server.address().port}/v2` },
      });
      let stdout = "", stderr = "";
      child.stdout.on("data", (data) => { stdout += data; });
      child.stderr.on("data", (data) => { stderr += data; });
      child.on("error", reject);
      child.on("exit", (code) => resolve({ code, stdout, stderr }));
    });
    return { ...result, requests };
  } finally {
    await new Promise((resolve) => server.close(resolve));
  }
}

test("RunPod v2 cleanup paginates before deleting and verifies every removal page", async () => {
  let deleted = false;
  const result = await cleanupFixture((req, res) => {
    const url = new URL(req.url, "http://fixture");
    if (req.method === "DELETE") { deleted = true; res.writeHead(204); res.end(); return; }
    if (url.pathname === "/v2/pods/owned") { res.end(JSON.stringify(pod("owned"))); return; }
    assert.equal(url.searchParams.get("limit"), "1000");
    assert.equal(url.searchParams.size, url.searchParams.has("cursor") ? 2 : 1);
    if (!url.searchParams.has("cursor")) { res.end(JSON.stringify(page([pod("manual", "manual-pod")], "next +/="))); return; }
    assert.equal(url.searchParams.get("cursor"), "next +/=");
    res.end(JSON.stringify(page(deleted ? [] : [pod("owned")])));
  });
  assert.equal(result.code, 0, result.stderr);
  assert.deepEqual(result.requests.map((r) => r.split(" ")[0]), ["GET", "GET", "GET", "DELETE", "GET", "GET"]);
  assert.match(result.requests[3], /^DELETE \/v2\/pods\/owned$/);
});

for (const scenario of ["ambiguous later page", "missing id", "changed identity", "missing cursor", "repeated cursor", "missing pagination", "later page error", "empty unrelated inventory"]) {
  test(`RunPod v2 cleanup refuses unsafe deletion: ${scenario}`, async () => {
    const result = await cleanupFixture((req, res) => {
      const url = new URL(req.url, "http://fixture");
      if (url.pathname === "/v2/pods/owned") { res.end(JSON.stringify(pod("owned", "manual-renamed"))); return; }
      if (scenario === "empty unrelated inventory") { res.end(JSON.stringify(page([pod("manual", "manual-pod")]))); return; }
      if (scenario === "missing id") { res.end(JSON.stringify(page([{ name: pod("owned").name }]))); return; }
      if (scenario === "changed identity") { res.end(JSON.stringify(page([pod("owned")]))); return; }
      if (scenario === "missing pagination") { res.end(JSON.stringify({ pods: [pod("owned")] })); return; }
      if (scenario === "missing cursor") { res.end(JSON.stringify({ pods: [pod("owned")], pagination: { hasNextPage: true } })); return; }
      if (!url.searchParams.has("cursor") || scenario === "repeated cursor") { res.end(JSON.stringify(page([pod("owned")], "next"))); return; }
      if (scenario === "later page error") { res.writeHead(403, { "Content-Type": "application/problem+json" }); res.end(JSON.stringify({ title: "Forbidden", detail: "cannot read inert-fixture-token", errors: ["missing scope"] })); return; }
      res.end(JSON.stringify(page([pod("other")])));
    });
    assert.equal(result.requests.some((r) => r.startsWith("DELETE")), false);
    assert.equal(result.code === 0, scenario === "empty unrelated inventory", result.stderr);
    assert.doesNotMatch(result.stderr, /inert-fixture-token/);
    if (scenario === "later page error") assert.match(result.stderr, /Forbidden.*missing scope/);
  });
}

test("RunPod v2 cleanup rejects the retired explicit v1 URL before HTTP", async () => {
  const result = await cleanupFixture(() => assert.fail("unexpected HTTP request"), "https://rest.runpod.io/v1/");
  assert.notEqual(result.code, 0);
  assert.match(result.stderr, /2026-11-15.*https:\/\/api\.runpod\.io\/v2/);
  assert.deepEqual(result.requests, []);
});
