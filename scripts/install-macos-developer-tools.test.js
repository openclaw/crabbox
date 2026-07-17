import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { mkdtemp, readFile, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const script = path.join(scriptDir, "install-macos-developer-tools.sh");

async function source() {
  return readFile(script, "utf8");
}

function shellQuote(value) {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

test("Homebrew installer is pinned and verified before execution", async () => {
  const text = await source();

  assert.match(text, /die\(\) \{/);
  assert.match(text, /homebrew_install_commit="[0-9a-f]{40}"/);
  assert.match(text, /homebrew_install_sha256="[0-9a-f]{64}"/);
  assert.doesNotMatch(text, /Homebrew\/install\/HEAD\/install\.sh/);
  assert.match(text, /if ! installer="\$\(download_verified_homebrew_installer\)"; then/);
  assert.match(text, /curl -fsSL --retry 3 --output "\$dest" "\$homebrew_install_url"/);
  assert.match(text, /if ! curl -fsSL --retry 3 --output "\$dest" "\$homebrew_install_url"; then/);
  assert.match(text, /shasum -a 256 -c -/);
  assert.ok(
    text.includes(`if ! printf '%s  %s\\n' "$homebrew_install_sha256" "$dest" | shasum -a 256 -c - >/dev/null; then`),
  );
  assert.match(text, /\/bin\/bash "\$installer"/);
});

test("tool links and brew wrapper avoid unconditional replacement", async () => {
  const text = await source();

  assert.doesNotMatch(text, /ln -sf "\$src"/);
  assert.doesNotMatch(text, /rm -f \/usr\/local\/bin\/brew/);
  assert.match(text, /if \[\[ "\$src" == "\$dest" \]\]; then/);
  assert.match(text, /CRABBOX_MACOS_FORCE_LINKS=1/);
  assert.match(text, /refusing to replace existing \$dest/);
  assert.match(text, /# crabbox-managed brew wrapper/);
  assert.match(text, /tmp="\$\(mktemp -d\)"/);
  assert.match(text, /tmp="\$\(mktemp\)"/);
  assert.doesNotMatch(text, /\.brew\.crabbox\.\$\$/);
  assert.match(text, /is_legacy_brew_wrapper "\$dest" "\$brew_path"/);
  assert.match(text, /sudo install -o root -g wheel -m 0755 "\$tmp" "\$dest"/);
});

test("legacy Crabbox brew wrapper is recognized for migration", async () => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "crabbox-macos-tools-test-"));
  const wrapper = path.join(dir, "brew");
  await writeFile(wrapper, '#!/bin/sh\nexec /opt/homebrew/bin/brew "$@"\n');

  const result = spawnSync(
    "bash",
    [
      "-lc",
      [
        `CRABBOX_MACOS_SOURCE_ONLY=1 source ${shellQuote(script)}`,
        `is_legacy_brew_wrapper ${shellQuote(wrapper)} /opt/homebrew/bin/brew`,
      ].join("; "),
    ],
    { encoding: "utf8" },
  );

  assert.equal(result.status, 0, result.stderr || result.stdout);
});

test("custom Homebrew formula parsing does not glob", async () => {
  const text = await source();

  assert.doesNotMatch(text, /formulas=\(\$\{CRABBOX_MACOS_BREW_FORMULAS\}\)/);
  assert.match(text, /while IFS= read -r formula; do/);
  assert.match(text, /read -r -a parts <<<"\$formula"/);
  assert.match(text, /formulas\+=\("\$\{parts\[@\]\}"\)/);
  assert.match(text, /read -r -a formulas <<<"\$CRABBOX_MACOS_BREW_FORMULAS"/);
});

test("Homebrew and SSH shell setup remain noninteractive and idempotent", async () => {
  const text = await source();

  assert.match(text, /brew analytics off/);
  assert.match(text, /HOMEBREW_NO_ASK=1 brew install --no-ask "\$formula"/);
  assert.match(text, /zshenv_line='export PATH="\/usr\/local\/bin:\/opt\/homebrew\/bin:\/opt\/homebrew\/sbin:\$PATH"'/);
  assert.match(text, /grep -qxF "\$zshenv_line" "\$HOME\/\.zshenv"/);
});
