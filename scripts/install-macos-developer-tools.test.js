import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmod, mkdtemp, readFile, writeFile } from "node:fs/promises";
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
  assert.match(text, /tail -c 1 "\$HOME\/\.zshenv"/);
});

test("TruffleHog archives are pinned, verified, and atomically installed", async () => {
  const text = await source();
  const start = text.indexOf("install_trufflehog() {");
  const end = text.indexOf("link_common_tools() {");
  const installTruffleHog = text.slice(start, end);

  assert.match(text, /trufflehog_version="3\.95\.9"/);
  assert.match(text, /4306a58d25b85aad7b5fb6f5732df77c50a9161db2746b56e196649072218691/);
  assert.match(text, /944c6ea3a2993a9f808d08107b40e03ba92bc75972876a1ee47d567bfd6fa1b5/);
  assert.match(text, /"\$binary" --no-update --version/);
  const download = installTruffleHog.indexOf('curl -fsSL --retry 3 --output "$tmp_dir/$archive" "$url"');
  const verify = installTruffleHog.indexOf("shasum -a 256 -c -");
  const extract = installTruffleHog.indexOf('tar -xzf "$tmp_dir/$archive" -C "$tmp_dir" trufflehog');
  const validate = installTruffleHog.indexOf('trufflehog_binary_ready "$candidate"');
  const replace = installTruffleHog.indexOf('mv -f "$candidate" "$target"');
  assert.ok(download >= 0, "TruffleHog download must be present");
  assert.ok(verify > download, "checksum verification must follow the download");
  assert.ok(extract > verify, "archive extraction must follow verification");
  assert.ok(validate > extract, "candidate validation must follow extraction");
  assert.ok(replace > validate, "atomic replacement must follow candidate validation");
});

test("TruffleHog checksum mapping covers Intel and Apple Silicon images", () => {
  const result = spawnSync(
    "bash",
    [
      "-lc",
      [
        `CRABBOX_MACOS_SOURCE_ONLY=1 source ${shellQuote(script)}`,
        "trufflehog_sha256_for_arch amd64",
        "trufflehog_sha256_for_arch arm64",
      ].join("; "),
    ],
    { encoding: "utf8" },
  );

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stdout, /4306a58d25b85aad7b5fb6f5732df77c50a9161db2746b56e196649072218691/);
  assert.match(result.stdout, /944c6ea3a2993a9f808d08107b40e03ba92bc75972876a1ee47d567bfd6fa1b5/);
});

test("pinned TruffleHog is reused without another download", async () => {
  const dir = await mkdtemp(path.join(os.tmpdir(), "crabbox-macos-trufflehog-"));
  const trufflehog = path.join(dir, "trufflehog");
  const curl = path.join(dir, "curl");
  await writeFile(trufflehog, "#!/bin/sh\nprintf 'trufflehog 3.95.9\\n'\n");
  await writeFile(curl, "#!/bin/sh\nexit 99\n");
  await chmod(trufflehog, 0o755);
  await chmod(curl, 0o755);

  const result = spawnSync(
    "bash",
    [
      "-lc",
      [
        `CRABBOX_MACOS_SOURCE_ONLY=1 CRABBOX_MACOS_TRUFFLEHOG_BIN_DIR=${shellQuote(dir)} source ${shellQuote(script)}`,
        "install_trufflehog",
      ].join("; "),
    ],
    {
      encoding: "utf8",
      env: { ...process.env, PATH: `${dir}:${process.env.PATH ?? ""}` },
    },
  );

  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.match(result.stderr, /TruffleHog 3\.95\.9 is already installed/);
});
