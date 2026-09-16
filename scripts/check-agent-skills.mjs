#!/usr/bin/env node
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";

const root = process.cwd();
const skillsDir = path.join(root, "skills");
const names = fs
  .readdirSync(skillsDir, { withFileTypes: true })
  .filter((entry) => entry.isDirectory())
  .map((entry) => entry.name)
  .sort();
assert.ok(names.length, "skills/ must contain at least one publishable Agent Skill");

// A quickstart that grows into a second full reference stops being a quickstart,
// so its budget is a CI contract rather than an editorial intention.
const defaultLineCap = 500;
const lineCaps = { "crabbox-quickstart": 155 };

for (const name of names) {
  const canonical = fs.readFileSync(path.join(skillsDir, name, "SKILL.md"), "utf8");
  const projection = fs.readFileSync(path.join(root, ".agents", "skills", name, "SKILL.md"), "utf8");

  assert.equal(
    projection,
    canonical,
    `.agents/skills/${name}/SKILL.md must remain byte-identical to skills/${name}/SKILL.md`,
  );
  assert.match(
    canonical,
    new RegExp(`^---\\nname: ${name}\\ndescription: "[^"]*Use when[^"]*"\\nlicense: MIT\\n---\\n`),
  );

  const lineCount = canonical.split("\n").length - (canonical.endsWith("\n") ? 1 : 0);
  const cap = lineCaps[name] ?? defaultLineCap;
  assert.ok(lineCount <= cap, `skills/${name}/SKILL.md should stay at or below ${cap} lines; found ${lineCount}`);
  console.log(`validated publishable Agent Skill ${name}: ${lineCount} lines, canonical and projection identical`);
}

// Crabbox-specific: pins the exact detection trigger and heading emitted by
// skillTemplate() in internal/cli/init.go, and asserts init.go still emits it,
// so the CLI and the published Skill cannot drift apart.
const detectionTrigger = /Use when crabbox\.yaml or \.crabbox\.yaml exists, the crabbox CLI is available/;
const crabbox = fs.readFileSync(path.join(skillsDir, "crabbox", "SKILL.md"), "utf8");
assert.match(crabbox, detectionTrigger);
assert.match(crabbox, /\n---\n\n# Crabbox\n/);
assert.match(
  fs.readFileSync(path.join(root, "internal", "cli", "init.go"), "utf8"),
  detectionTrigger,
  "skillTemplate() in internal/cli/init.go must emit the same detection trigger as skills/crabbox",
);

console.log(`validated ${names.length} publishable Agent Skill${names.length === 1 ? "" : "s"}: ${names.join(", ")}`);
