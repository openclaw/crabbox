#!/usr/bin/env node
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";

const root = process.cwd();
const canonicalPath = path.join(root, "skills", "crabbox", "SKILL.md");
const projectionPath = path.join(root, ".agents", "skills", "crabbox", "SKILL.md");
const canonical = fs.readFileSync(canonicalPath, "utf8");
const projection = fs.readFileSync(projectionPath, "utf8");

assert.equal(
  projection,
  canonical,
  ".agents/skills/crabbox/SKILL.md must remain byte-identical to skills/crabbox/SKILL.md",
);
assert.match(canonical, /^---\nname: crabbox\ndescription: "[^"]*Use when[^"]*"\nlicense: MIT\n---\n/);
assert.match(
  canonical,
  /Use when crabbox\.yaml or \.crabbox\.yaml exists, the crabbox CLI is available/,
);
assert.match(canonical, /\n---\n\n# Crabbox\n/);

const lineCount = canonical.split("\n").length - (canonical.endsWith("\n") ? 1 : 0);
assert.ok(lineCount <= 500, `Crabbox Skill should stay at or below 500 lines; found ${lineCount}`);

console.log(`validated publishable Crabbox Agent Skill: ${lineCount} lines, canonical and projection identical`);
