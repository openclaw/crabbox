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
      "crabbox_list",
      "crabbox_run",
      "crabbox_status",
      "crabbox_stop",
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

test("crabbox_events includes optional flags", async () => {
  const fake = createFakeCrabbox();
  const tools = registerWithConfig({ binary: fake.file });
  const result = await getTool(tools, "crabbox_events").execute("call-1", {
    id: "run_123",
    after: 4,
    limit: 25,
    json: true,
  });
  assert.deepEqual(JSON.parse(result.details.stdout).argv, [
    "events",
    "--id",
    "run_123",
    "--after",
    "4",
    "--limit",
    "25",
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
