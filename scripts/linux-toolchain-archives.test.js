import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const repoRoot = path.resolve(import.meta.dirname, "..");
const installer = path.join(repoRoot, "scripts/install-linux-developer-tools.sh");
const publicArchives = process.env.CRABBOX_TEST_TOOLCHAIN_ARCHIVES;
const nodeArchive = "node-v24.19.0-linux-x64.tar.xz";
const archiveNames = [
  nodeArchive,
  "pnpm-11.22.0.tgz",
  "pnpm-12.3.4.tgz",
  "exe.linux-x64-12.3.4.tgz",
];

function fixture(t) {
  const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-toolchain-")));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  for (const dir of ["archives", "staging", "home", "tmp"]) fs.mkdirSync(path.join(root, dir));
  const run = (body, env = {}) =>
    spawnSync("bash", ["-c", `source "$INSTALLER"\n${body}`], {
      cwd: root,
      env: {
        PATH: process.env.PATH,
        HOME: path.join(root, "home"),
        TMPDIR: path.join(root, "tmp"),
        INSTALLER: installer,
        ...env,
      },
      encoding: "utf8",
      timeout: 60_000,
    });
  return { root, run };
}

function success(result) {
  assert.equal(result.status, 0, result.error?.message || result.stderr || result.stdout);
}

function writeTool(file, body) {
  fs.writeFileSync(file, `#!/usr/bin/env bash\nset -euo pipefail\n${body}\n`, { mode: 0o755 });
}

test("archive staging authenticates private bytes, skips downloads, and rejects symlinks or corruption", (t) => {
  const { root, run } = fixture(t);
  const payload = Buffer.from("reviewed public archive fixture\n");
  const digest = createHash("sha256").update(payload).digest("hex");
  const cached = path.join(root, "archives", "fixture.tgz");
  const staged = path.join(root, "staging", "fixture.tgz");
  const shell = `
public_toolchain_archive_dir="$PWD/archives"
toolchain_archive_spec() { printf 'sha256 %s https://example.invalid/fixture.tgz\\n' "$FIXTURE_DIGEST"; }
curl() { echo unexpected-network >&2; return 89; }
stage_toolchain_archive fixture.tgz "$PWD/staging"
`;
  fs.writeFileSync(cached, payload);
  success(run(shell, { FIXTURE_DIGEST: digest }));
  assert.deepEqual(fs.readFileSync(staged), payload);
  fs.writeFileSync(cached, "changed after staging");
  assert.deepEqual(
    fs.readFileSync(staged),
    payload,
    "execution must use an independent private copy",
  );
  const corrupt = run(shell, { FIXTURE_DIGEST: digest });
  assert.notEqual(corrupt.status, 0);
  assert.match(corrupt.stderr, /checksum mismatch/);
  assert.equal(fs.existsSync(staged), false);
  assert.doesNotMatch(corrupt.stderr, /unexpected-network/);

  fs.writeFileSync(path.join(root, "valid.tgz"), payload);
  fs.unlinkSync(cached);
  fs.symlinkSync(path.join(root, "valid.tgz"), cached);
  const symlink = run(shell, { FIXTURE_DIGEST: digest });
  assert.notEqual(symlink.status, 0);
  assert.match(symlink.stderr, /unavailable offline/);
  assert.equal(fs.existsSync(staged), false);

  const downloaded = run(
    shell
      .replace(
        "curl() { echo unexpected-network >&2; return 89; }",
        `curl() {
  while [[ "$1" != "--output" ]]; do shift; done
  cp "$PWD/valid.tgz" "$2"
}`,
      )
      .replace(
        'stage_toolchain_archive fixture.tgz "$PWD/staging"',
        'stage_toolchain_archive fixture.tgz "$PWD/staging" 1',
      ),
    {
      FIXTURE_DIGEST: digest,
    },
  );
  success(downloaded);
  assert.deepEqual(fs.readFileSync(staged), payload);
});

test("raw pnpm authentication cannot be replaced by forged Corepack metadata or a packed bundle", (t) => {
  const { root, run } = fixture(t);
  fs.writeFileSync(path.join(root, "archives", "pnpm-11.22.0.tgz"), "forged payload");
  const destination = path.join(root, "home", "v1", "pnpm", "11.22.0");
  const rejected = run(`
public_toolchain_archive_dir="$PWD/archives"
if seed_offline_pnpm 11.22.0 "$PWD/staging" "$PWD/home"; then exit 91; fi
`);
  success(rejected);
  assert.match(rejected.stderr, /checksum mismatch/);
  assert.equal(
    fs.existsSync(destination),
    false,
    "failed authentication must not create a trusted cache entry",
  );
  fs.mkdirSync(destination, { recursive: true });
  fs.writeFileSync(path.join(destination, ".corepack"), '{"hash":"sha512.forged"}');
  const forged = run('seed_offline_pnpm 11.22.0 "$PWD/staging" "$PWD/home"');
  assert.notEqual(forged.status, 0);
  const packed = run('stage_toolchain_archive corepack-pack.tgz "$PWD/staging"');
  assert.notEqual(packed.status, 0);
  assert.match(packed.stderr, /no reviewed public toolchain archive/);
});

test("Node toolcache replaces a same-version poisoned tree and publishes completion only after functional checks", (t) => {
  const { root, run } = fixture(t);
  const bin = path.join(root, "payload", "node", "bin");
  fs.mkdirSync(bin, { recursive: true });
  writeTool(path.join(bin, "node"), 'printf "v24.19.0\\n"');
  writeTool(path.join(bin, "npm"), 'printf "11.0.0\\n"');
  writeTool(
    path.join(bin, "corepack"),
    `
if [[ "$1" == "--version" ]]; then printf '0.35.0\\n'; exit 0; fi
[[ "$*" == "enable --install-directory "* ]]
for tool in pnpm pnpx; do ln -s corepack "$3/$tool"; done`,
  );
  const archive = path.join(root, "archives", nodeArchive);
  const pack = () => {
    const result = spawnSync("tar", ["-cJf", archive, "-C", path.join(root, "payload"), "node"], {
      encoding: "utf8",
    });
    success(result);
    return createHash("sha256").update(fs.readFileSync(archive)).digest("hex");
  };
  const destination = path.join(root, "tools", "node", "24.19.0", "x64");
  fs.mkdirSync(destination, { recursive: true });
  fs.writeFileSync(path.join(destination, "poison"), "same-version cache is not authenticated");
  fs.mkdirSync(path.join(destination, "bin"));
  writeTool(
    path.join(destination, "bin", "node"),
    'touch "$HOME/poison-executed"; printf "v24.19.0\\n"',
  );
  fs.writeFileSync(`${destination}.complete`, "");
  const shell = `
public_toolchain_archive_dir="$PWD/archives"
node_toolcache_root="$PWD/tools"
node_link_dir="$PWD/links"
toolchain_archive_spec() { printf 'sha256 %s https://example.invalid/node.tar.xz\\n' "$FIXTURE_DIGEST"; }
install_pinned_node
`;
  success(run(shell, { FIXTURE_DIGEST: pack() }));
  assert.equal(fs.existsSync(path.join(destination, "poison")), false);
  assert.equal(fs.existsSync(path.join(root, "home", "poison-executed")), false);
  assert.equal(fs.existsSync(`${destination}.complete`), true);
  assert.equal(
    fs.readlinkSync(path.join(root, "links", "node")),
    path.join(destination, "bin", "node"),
  );
  assert.equal(fs.existsSync(path.join(root, "links", "pnpm")), true);

  writeTool(path.join(bin, "npm"), "exit 47");
  const failure = run(shell, { FIXTURE_DIGEST: pack() });
  assert.equal(failure.status, 47, failure.stderr);
  assert.equal(fs.existsSync(`${destination}.complete`), false);
  assert.deepEqual(
    fs.readdirSync(path.join(root, "tmp")),
    [],
    "failure must remove private extraction state",
  );
  success(spawnSync(path.join(destination, "bin", "npm"), ["--version"], { encoding: "utf8" }));
});

for (const [major, arch, pnpm, pinned] of [
  ["24", "amd64", "", true],
  ["24", "amd64", "11.22.0", true],
  ["24", "amd64", "12.3.4", true],
  ["22", "amd64", "10.0.0", false],
  ["24", "arm64", "11.22.0", false],
]) {
  test(`Node ${major}/${arch} preserves pnpm ${pnpm || "default"} selection`, (t) => {
    const { run } = fixture(t);
    const result = run(
      `
dpkg() { printf '%s\\n' "$FIXTURE_ARCH"; }
cache_public_toolchain_archives() { echo public-archives; }
install_pinned_node() { echo pinned-node; }
apt_install() { echo "apt $*"; }
command() { return 0; }
corepack() { echo "corepack $*"; }
install_node_pnpm
`,
      {
        CRABBOX_LINUX_NODE_MAJOR: major,
        CRABBOX_LINUX_PNPM_VERSION: pnpm,
        FIXTURE_ARCH: arch,
      },
    );
    success(result);
    assert.match(result.stdout, new RegExp(`corepack prepare pnpm@${pnpm || "11.1.0"} --activate`));
    assert.equal(result.stdout.includes("pinned-node"), pinned);
    assert.equal(result.stdout.includes("apt nodejs"), !pinned);
  });
}

test(
  "reviewed public archives seed the bundled Corepack and install a local dependency offline",
  {
    skip:
      !publicArchives && "set CRABBOX_TEST_TOOLCHAIN_ARCHIVES to the four reviewed public archives",
  },
  (t) => {
    const { root, run } = fixture(t);
    const staged = run(
      `
public_toolchain_archive_dir="$PUBLIC_ARCHIVES"
for archive in ${archiveNames.join(" ")}; do stage_toolchain_archive "$archive" "$PWD/staging"; done
mkdir node
tar --no-same-owner -xJf "$PWD/staging/${nodeArchive}" -C node --strip-components=1
seed_offline_pnpm 11.22.0 "$PWD/staging" "$PWD/corepack"
seed_offline_pnpm 12.3.4 "$PWD/staging" "$PWD/corepack"
`,
      { PUBLIC_ARCHIVES: publicArchives },
    );
    success(staged);
    const native = path.join(root, "corepack", "v1", "pnpm", "12.3.4", "pnpm-native");
    assert.equal(
      fs.readFileSync(native).subarray(0, 4).toString("hex"),
      "7f454c46",
      "pnpm 12 requires the separately verified Linux executable",
    );
    fs.copyFileSync(
      path.join(publicArchives, "pnpm-12.3.4.tgz"),
      path.join(root, "archives", "pnpm-12.3.4.tgz"),
    );
    fs.writeFileSync(
      path.join(root, "archives", "exe.linux-x64-12.3.4.tgz"),
      "forged native payload",
    );
    const forgedNative = run(`
public_toolchain_archive_dir="$PWD/archives"
seed_offline_pnpm 12.3.4 "$PWD/staging" "$PWD/forged-corepack"
`);
    assert.notEqual(forgedNative.status, 0);
    assert.match(forgedNative.stderr, /checksum mismatch/);
    const rejectedNative = path.join(root, "forged-corepack", "v1", "pnpm", "12.3.4");
    assert.equal(fs.existsSync(path.join(rejectedNative, ".corepack")), false);
    assert.equal(fs.existsSync(path.join(rejectedNative, "pnpm-native")), false);
    const corepack = path.join(
      root,
      "node",
      "lib",
      "node_modules",
      "corepack",
      "dist",
      "corepack.js",
    );
    const home = path.join(root, "corepack");
    const metadata = JSON.parse(
      fs.readFileSync(path.join(home, "v1", "pnpm", "11.22.0", ".corepack"), "utf8"),
    );
    const dependency = path.join(root, "dependency");
    fs.mkdirSync(dependency);
    fs.writeFileSync(
      path.join(root, "package.json"),
      '{"name":"offline-fixture","version":"1.0.0","dependencies":{"offline-dependency":"file:./dependency"}}',
    );
    fs.writeFileSync(
      path.join(dependency, "package.json"),
      '{"name":"offline-dependency","version":"1.0.0","main":"index.js"}',
    );
    fs.writeFileSync(path.join(dependency, "index.js"), "module.exports = 42;");
    const env = {
      PATH: `${path.dirname(process.execPath)}${path.delimiter}${process.env.PATH}`,
      HOME: path.join(root, "home"),
      COREPACK_HOME: home,
      COREPACK_ENABLE_NETWORK: "0",
      COREPACK_DEFAULT_TO_LATEST: "0",
      COREPACK_ENABLE_PROJECT_SPEC: "0",
      COREPACK_ENV_FILE: "0",
      CI: "1",
    };
    const args = [corepack, `pnpm@${metadata.locator.reference}`];
    const invoke = (tail, overrides = {}) =>
      spawnSync(process.execPath, [...args, ...tail], {
        cwd: root,
        env: { ...env, ...overrides },
        encoding: "utf8",
        timeout: 60_000,
      });
    const absent = invoke(["--version"], { COREPACK_HOME: path.join(root, "empty-corepack") });
    assert.notEqual(absent.status, 0);
    assert.match(absent.stderr, /Network access disabled/);
    const version = invoke(["--version"]);
    success(version);
    assert.equal(version.stdout.trim(), "11.22.0");
    success(invoke(["install", "--offline", "--ignore-scripts", "--no-frozen-lockfile"]));
    success(
      spawnSync(
        process.execPath,
        ["-e", 'if (require("offline-dependency") !== 42) process.exit(1)'],
        {
          cwd: root,
          env,
          encoding: "utf8",
        },
      ),
    );
  },
);

test(
  "Linux x64 nonroot smoke executes both reviewed pnpm versions from fresh private archives",
  {
    skip:
      (!publicArchives ||
        process.platform !== "linux" ||
        process.arch !== "x64" ||
        process.getuid?.() === 0) &&
      "requires the reviewed public archives and a nonroot Linux x64 host",
  },
  (t) => {
    const { run } = fixture(t);
    success(
      run('public_toolchain_archive_dir="$PUBLIC_ARCHIVES"\noffline_node_pnpm_probe', {
        PUBLIC_ARCHIVES: publicArchives,
      }),
    );
  },
);
