import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";

const root = process.cwd();
const extensionRoot = path.join(root, "integrations", "zed");

function read(relativePath) {
  return fs.readFileSync(path.join(extensionRoot, relativePath), "utf8");
}

function readJSON(relativePath) {
  return JSON.parse(read(relativePath));
}

const manifest = read("extension.toml");
assert.match(manifest, /^id = "crabbox"$/m);
assert.match(manifest, /^name = "Crabbox"$/m);
assert.match(manifest, /^version = "\d+\.\d+\.\d+"$/m);
assert.match(manifest, /^schema_version = 1$/m);
assert.match(manifest, /^repository = "https:\/\/github\.com\/openclaw\/crabbox"$/m);
assert.match(manifest, /^snippets = \["\.\/snippets\/crabbox\.json"\]$/m);
assert.match(manifest, /^\[grammars\.yaml\]$/m);
assert.match(manifest, /^rev = "[0-9a-f]{40}"$/m);
assert.doesNotMatch(manifest, /context_servers|mcp/i);
assert.match(read("LICENSE"), /^MIT License$/m);

const languageConfig = read("languages/crabbox/config.toml");
assert.match(languageConfig, /^name = "Crabbox"$/m);
assert.match(languageConfig, /^grammar = "yaml"$/m);
assert.match(languageConfig, /"\.crabbox\.yaml"/);

const languageTasks = readJSON("languages/crabbox/tasks.json");
const projectTasks = readJSON("project-tasks.json");
assert.deepEqual(projectTasks, languageTasks, "language and project task packs must stay identical");
assert.ok(languageTasks.length >= 8, "expected a complete lifecycle task pack");

const labels = new Set();
for (const task of languageTasks) {
  assert.equal(typeof task.label, "string");
  assert.match(task.label, /^Crabbox: /);
  assert.ok(!labels.has(task.label), `duplicate task label: ${task.label}`);
  labels.add(task.label);
  assert.ok(["crabbox", "bash"].includes(task.command), `unexpected command: ${task.command}`);
  assert.equal(task.cwd, "$ZED_WORKTREE_ROOT");
  assert.notEqual(task.command, "zed");
  assert.doesNotMatch(JSON.stringify(task), /crabbox open|--editor|mcp/i);
}

for (const expected of [
  "Crabbox: Doctor",
  "Crabbox: Spawn reusable box",
  "Crabbox: Run selected command",
  "Crabbox: Run detected project job",
  "Crabbox: List boxes",
  "Crabbox: Status of box…",
  "Crabbox: Run command on box…",
  "Crabbox: SSH into box…",
  "Crabbox: Inspect box…",
  "Crabbox: Stop box…",
]) {
  assert.ok(labels.has(expected), `missing task: ${expected}`);
}

const snippets = readJSON("snippets/crabbox.json");
assert.ok(snippets["Crabbox project configuration"]);
assert.ok(snippets["Crabbox job"]);

console.log(`validated registry-ready Crabbox Zed extension: ${languageTasks.length} tasks, ${Object.keys(snippets).length} snippets`);
