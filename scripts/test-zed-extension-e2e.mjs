import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";

const root = process.cwd();
const tasksPath = path.join(root, "integrations", "zed", "project-tasks.json");
const tasks = JSON.parse(fs.readFileSync(tasksPath, "utf8"));
const temp = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-zed-e2e-"));
const bin = path.join(temp, "bin");
const calls = path.join(temp, "calls.log");
fs.mkdirSync(bin, { recursive: true });
fs.writeFileSync(
  path.join(bin, "crabbox"),
  `#!/usr/bin/env bash\nset -euo pipefail\nprintf '%q ' "$@" >> "${calls}"\nprintf '\\n' >> "${calls}"\nif [ "\${1-}" = list ]; then printf 'swift-crab ready\\n'; fi\n`,
  { mode: 0o755 },
);

const env = {
  ...process.env,
  PATH: `${bin}${path.delimiter}${process.env.PATH ?? ""}`,
  ZED_WORKTREE_ROOT: temp,
  ZED_SELECTED_TEXT: "printf selected-ok",
};

function inputFor(label) {
  if (label === "Crabbox: Run command on box…") return "swift-crab\nprintf remote-ok\n";
  if (label.includes("box…")) return "swift-crab\n";
  return "";
}

for (const task of tasks) {
  const result = spawnSync(task.command, task.args ?? [], {
    cwd: temp,
    env,
    input: inputFor(task.label),
    encoding: "utf8",
  });
  assert.equal(
    result.status,
    0,
    `${task.label} failed\nstdout:\n${result.stdout}\nstderr:\n${result.stderr}`,
  );
}

const recorded = fs.readFileSync(calls, "utf8");
for (const expected of [
  "doctor",
  "warmup",
  "run -- sh -lc printf\\ selected-ok",
  "job run detected",
  "list",
  "status --id swift-crab --wait",
  "run --id swift-crab -- sh -lc printf\\ remote-ok",
  "connect swift-crab",
  "inspect --id swift-crab",
  "stop swift-crab",
]) {
  assert.ok(recorded.includes(expected), `missing CLI call: ${expected}\n${recorded}`);
}

console.log(`executed ${tasks.length} Zed tasks through the Crabbox CLI boundary`);
