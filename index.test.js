import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import plugin from "./index.js";

function createFakeCrabbox() {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-plugin-"));
  const file = path.join(dir, "crabbox-fake.js");
  fs.writeFileSync(
    file,
    `#!/usr/bin/env node
const payload = { argv: process.argv.slice(2), env: { CRABBOX_TEST_VALUE: process.env.CRABBOX_TEST_VALUE } };
process.stdout.write(JSON.stringify(payload));
if (process.env.CRABBOX_FAKE_EXIT) process.exit(Number(process.env.CRABBOX_FAKE_EXIT));
`,
    "utf8",
  );
  fs.chmodSync(file, 0o755);
  return { dir, file };
}

function registerWithConfig(pluginConfig) {
  const tools = [];
  plugin.register({
    pluginConfig,
    registerTool(tool) {
      tools.push(tool);
    },
    logger: { info() {} },
  });
  return tools;
}

function getTool(tools, name) {
  const tool = tools.find((entry) => entry.name === name);
  assert.ok(tool, `expected ${name} to be registered`);
  return tool;
}

test("registers the Crabbox tool surface", () => {
  const tools = registerWithConfig({});
  assert.deepEqual(
    tools.map((tool) => tool.name).sort(),
    [
      "crabbox_events",
      "crabbox_history",
      "crabbox_list",
      "crabbox_logs",
      "crabbox_results",
      "crabbox_run",
      "crabbox_status",
      "crabbox_stop",
      "crabbox_usage",
      "crabbox_warmup",
    ],
  );
});

test("crabbox_run executes the CLI without shell wrapping", async () => {
  const fake = createFakeCrabbox();
  const tools = registerWithConfig({ binary: fake.file });
  const result = await getTool(tools, "crabbox_run").execute("call-1", {
    id: "blue-lobster",
    command: ["go", "test", "./..."],
    env: { CRABBOX_TEST_VALUE: "present" },
  });
  assert.equal(result.details.code, 0);
  assert.deepEqual(JSON.parse(result.details.stdout).argv, [
    "run",
    "--id",
    "blue-lobster",
    "--",
    "go",
    "test",
    "./...",
  ]);
  assert.equal(JSON.parse(result.details.stdout).env.CRABBOX_TEST_VALUE, "present");
});

test("crabbox_status includes optional flags", async () => {
  const fake = createFakeCrabbox();
  const tools = registerWithConfig({ binary: fake.file });
  const result = await getTool(tools, "crabbox_status").execute("call-1", {
    id: "cbx_123",
    wait: true,
    waitTimeout: "10m",
    json: true,
  });
  assert.deepEqual(JSON.parse(result.details.stdout).argv, [
    "status",
    "--id",
    "cbx_123",
    "--wait",
    "--wait-timeout",
    "10m",
    "--json",
  ]);
});

test("crabbox_history includes run filters", async () => {
  const fake = createFakeCrabbox();
  const tools = registerWithConfig({ binary: fake.file });
  const result = await getTool(tools, "crabbox_history").execute("call-1", {
    lease: "cbx_abcdef123456",
    owner: "peter@example.com",
    org: "openclaw",
    state: "failed",
    limit: 25,
    json: true,
  });
  assert.deepEqual(JSON.parse(result.details.stdout).argv, [
    "history",
    "--lease",
    "cbx_abcdef123456",
    "--owner",
    "peter@example.com",
    "--org",
    "openclaw",
    "--state",
    "failed",
    "--limit",
    "25",
    "--json",
  ]);
});

test("crabbox_events includes pagination flags", async () => {
  const fake = createFakeCrabbox();
  const tools = registerWithConfig({ binary: fake.file });
  const result = await getTool(tools, "crabbox_events").execute("call-1", {
    id: "run_abcdef123456",
    after: 42,
    limit: 100,
    json: true,
  });
  assert.deepEqual(JSON.parse(result.details.stdout).argv, [
    "events",
    "--id",
    "run_abcdef123456",
    "--after",
    "42",
    "--limit",
    "100",
    "--json",
  ]);
});

test("crabbox_logs and results pass run IDs", async () => {
  const fake = createFakeCrabbox();
  const tools = registerWithConfig({ binary: fake.file });
  const logs = await getTool(tools, "crabbox_logs").execute("call-1", {
    id: "run_abcdef123456",
    json: true,
  });
  assert.deepEqual(JSON.parse(logs.details.stdout).argv, [
    "logs",
    "--id",
    "run_abcdef123456",
    "--json",
  ]);

  const results = await getTool(tools, "crabbox_results").execute("call-2", {
    id: "run_abcdef123456",
    json: true,
  });
  assert.deepEqual(JSON.parse(results.details.stdout).argv, [
    "results",
    "--id",
    "run_abcdef123456",
    "--json",
  ]);
});

test("crabbox_usage includes scope filters", async () => {
  const fake = createFakeCrabbox();
  const tools = registerWithConfig({ binary: fake.file });
  const result = await getTool(tools, "crabbox_usage").execute("call-1", {
    scope: "org",
    org: "openclaw",
    month: "2026-05",
    json: true,
  });
  assert.deepEqual(JSON.parse(result.details.stdout).argv, [
    "usage",
    "--scope",
    "org",
    "--org",
    "openclaw",
    "--month",
    "2026-05",
    "--json",
  ]);
});

test("disabled run tool fails before invoking crabbox", async () => {
  const fake = createFakeCrabbox();
  const tools = registerWithConfig({ binary: fake.file, allowRun: false });
  await assert.rejects(
    getTool(tools, "crabbox_run").execute("call-1", {
      id: "blue-lobster",
      command: ["go", "test", "./..."],
    }),
    /disabled/,
  );
});

test("disabled inspection tool fails before invoking crabbox", async () => {
  const fake = createFakeCrabbox();
  const tools = registerWithConfig({ binary: fake.file, allowInspection: false });
  await assert.rejects(
    getTool(tools, "crabbox_logs").execute("call-1", {
      id: "run_abcdef123456",
    }),
    /disabled/,
  );
});
