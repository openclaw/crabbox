import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";
import test from "node:test";

const root = path.resolve(import.meta.dirname, "..");
const readme = fs.readFileSync(path.join(root, "README.md"), "utf8");
const egress = fs.readFileSync(path.join(root, "docs/commands/egress.md"), "utf8");

test("the quick-start shell recipe opens a session", () => {
  const command = readme.match(/# Open a shell[^\n]*\n([^\n]+)/)?.[1];
  assert.ok(command, "quick start must include its interactive shell command");
  assert.match(command, /^crabbox connect\b/, "ssh only prints a command; connect opens the session");
});

test("README shell and failure-inspection shortcuts open a session", () => {
  const rows = readme.split("\n").filter((line) =>
    line.startsWith("|") && /interactive shell|inspect the failure/.test(line),
  );
  assert.equal(rows.length, 2);
  for (const row of rows) {
    assert.match(row, /`crabbox connect --id <box>`/);
  }
});

test("egress troubleshooting opens the box before reading its log", () => {
  const recipe = egress.match(/```sh\n([^`]*cat \/tmp\/crabbox-egress-client\.log[^`]*)```/)?.[1];
  assert.ok(recipe, "egress troubleshooting must include the remote log recipe");
  assert.match(recipe, /^crabbox connect --id [^\n]+\ncat /);
});
