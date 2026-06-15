import assert from "node:assert/strict";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import { spawnSync } from "node:child_process";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const checker = path.join(repoRoot, "scripts", "check-docs-links.mjs");

test("docs link checker accepts GitHub duplicate heading anchors", (t) => {
  const dir = newDocsFixture(t);
  writeFile(path.join(dir, "README.md"), "[second setup](docs/guide.md#setup-1)\n");
  writeFile(path.join(dir, "docs", "guide.md"), "# Setup\n\n## Details\n\n# Setup\n");

  const result = runChecker(dir);

  assert.equal(result.status, 0, result.stderr);
});

test("docs link checker avoids collisions with suffixed headings", (t) => {
  const dir = newDocsFixture(t);
  writeFile(path.join(dir, "README.md"), "[suffixed setup](docs/guide.md#setup-1-1)\n");
  writeFile(path.join(dir, "docs", "guide.md"), "# Setup\n\n# Setup\n\n# Setup-1\n");

  const result = runChecker(dir);

  assert.equal(result.status, 0, result.stderr);
});

test("docs link checker rejects missing duplicate heading anchors", (t) => {
  const dir = newDocsFixture(t);
  writeFile(path.join(dir, "README.md"), "[third setup](docs/guide.md#setup-2)\n");
  writeFile(path.join(dir, "docs", "guide.md"), "# Setup\n\n# Setup\n");

  const result = runChecker(dir);

  assert.notEqual(result.status, 0, result.stdout);
  assert.match(result.stderr, /missing heading docs\/guide\.md#setup-2/);
});

test("docs link checker accepts local links with titles and angle brackets", (t) => {
  const dir = newDocsFixture(t);
  writeFile(
    path.join(dir, "README.md"),
    [
      '[guide](<docs/guide.md#quoted-heading> "Contributor\'s guide")',
      '[guide](<docs/guide.md#quoted-heading> \'The "guide" page\')',
      '[guide](<docs/guide.md#quoted-heading> "A \\"quoted\\" guide")',
      "",
    ].join("\n"),
  );
  writeFile(path.join(dir, "docs", "guide.md"), "# Quoted Heading\n");

  const result = runChecker(dir);

  assert.equal(result.status, 0, result.stderr);
});

function newDocsFixture(t) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-docs-links-"));
  fs.mkdirSync(path.join(dir, "docs"));
  t.after(() => fs.rmSync(dir, { recursive: true, force: true }));
  return dir;
}

function writeFile(file, body) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, body, "utf8");
}

function runChecker(cwd) {
  return spawnSync(process.execPath, [checker], {
    cwd,
    encoding: "utf8",
  });
}
