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
const goArchive = "go1.27.0.linux-amd64.tar.gz";
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

function writeNodeToolchain(bin, version) {
  const major = version.split(".")[0];
  const dist = path.resolve(bin, "../lib/node_modules/corepack/dist");
  fs.mkdirSync(bin, { recursive: true });
  fs.mkdirSync(dist, { recursive: true });
  writeTool(path.join(bin, "node"), `printf "v${version}\\n"`);
  for (const tool of ["npm", "npx"]) writeTool(path.join(bin, tool), `printf "npm-${major}\\n"`);
  for (const tool of ["pnpm", "pnpx", "yarn", "yarnpkg"]) {
    writeTool(path.join(dist, `${tool}.js`), `printf "${tool}-${major}\\n"`);
  }
  fs.symlinkSync("../lib/node_modules/corepack/dist/corepack.js", path.join(bin, "corepack"));
  writeTool(
    path.join(dist, "corepack.js"),
    `
if [[ "$1" == "--version" ]]; then printf 'corepack-${major}\\n'; exit 0; fi
printf '${major} %s node=%s\\n' "$*" "$(node --version)" >>"$HOME/corepack.log"
if [[ "$1" == "enable" ]]; then
  # Corepack 0.35.0 resolves the public directory through which, not its real binary.
  python3 - "$0" "\${3:-}" <<'PY'
import os
from pathlib import Path
import shutil
import sys

binary, selected = sys.argv[1:]
directory = Path(selected or Path(shutil.which("corepack")).parent).resolve()
dist = Path(binary).resolve().parent
for name in ("pnpm", "pnpx", "yarn", "yarnpkg"):
    link = directory / name
    target = os.path.relpath(dist / (name + ".js"), directory)
    if os.path.lexists(link):
        if link.is_symlink() and os.readlink(link) == target:
            continue
        link.unlink()
    link.symlink_to(target)
PY
elif [[ "$1" != "prepare" ]]; then
  exit 64
fi`,
  );
}

function nodeArchiveFixture(t) {
  const context = fixture(t);
  const { root } = context;
  const bin = path.join(root, "payload", "node", "bin");
  writeNodeToolchain(bin, "24.19.0");
  const archive = path.join(root, "archives", nodeArchive);
  const pack = () => {
    success(
      spawnSync("tar", ["-cJf", archive, "-C", path.join(root, "payload"), "node"], {
        encoding: "utf8",
      }),
    );
    return createHash("sha256").update(fs.readFileSync(archive)).digest("hex");
  };
  const setup = `
public_toolchain_archive_dir="$PWD/archives"
node_toolcache_root="$PWD/tools"
node_link_dir="$PWD/links"
toolchain_archive_spec() { printf 'sha256 %s https://example.invalid/node.tar.xz\\n' "$FIXTURE_DIGEST"; }
`;
  const shell = `${setup}install_pinned_node\n`;
  return { ...context, bin, archive, pack, setup, shell };
}

function nodeRebakeFixture(t) {
  const context = nodeArchiveFixture(t);
  const { root } = context;
  const aptPayload = path.join(root, "apt-payload");
  writeNodeToolchain(path.join(aptPayload, "bin"), "22.0.0");
  writeNodeToolchain(path.join(root, "apt", "bin"), "24.15.0");
  fs.writeFileSync(path.join(root, "package-state"), "installed\t24.15.0-1nodesource1\tamd64\n");
  fs.writeFileSync(
    path.join(root, "apt-versions"),
    [
      "nodejs | 26.0.0-1nodesource1 | https://deb.nodesource.com/node_26.x nodistro/main amd64 Packages",
      "nodejs | 22.1.0-1nodesource1 | https://other.invalid/node_22.x nodistro/main amd64 Packages",
      "nodejs | 22.1.0-1nodesource1 | https://deb.nodesource.com/node_22.x nodistro/main arm64 Packages",
      "nodejs | 22.0.0-1nodesource1 | https://deb.nodesource.com/node_22.x nodistro/main amd64 Packages",
      "nodejs | 22.0.0-1nodesource1 | /var/lib/dpkg/status",
      "",
    ].join("\n"),
  );
  const digest = context.pack();
  const shell = `${context.shell}
export PATH="$node_link_dir:$PWD/apt/bin:$PATH"
for tool in node npm npx corepack pnpm pnpx; do "$tool" --version; done
[[ "$(hash -t node)" == "$node_link_dir/node" ]]
: >"$HOME/corepack.log"
node_major=22
pnpm_version=10.0.0
dpkg() {
  if [[ "$*" == "--print-architecture" ]]; then echo amd64; return; fi
  [[ "$1" == "--compare-versions" ]] || return 90
  python3 - "$2" "$3" "$4" <<'PY'
import re, sys
left, op, right = sys.argv[1:]
key = lambda value: tuple(map(int, re.findall(r"\\d+", value)))
sys.exit(0 if {"gt": key(left) > key(right), "lt": key(left) < key(right)}[op] else 1)
PY
}
apt-cache() {
  [[ "$*" == "madison nodejs:amd64" ]] || return 90
  cat "$PWD/apt-versions"
}
dpkg-query() {
  case "$1" in
    -W) cat "$PWD/package-state" ;;
    -L) printf '%s\\n' "$PWD/apt/bin/node" ;;
    *) return 90 ;;
  esac
}
retry() { "$@"; }
apt-get() {
  printf '%s\\n' "$*" >>"$PWD/apt-calls"
  [[ "\${FIXTURE_APT_EXIT:-0}" == "0" ]] || return "$FIXTURE_APT_EXIT"
  [[ "$1" == install ]] || return 90
  shift
  local selector="" yes=0 no_recommends=0 allow_downgrade=0 arg
  for arg in "$@"; do
    case "$arg" in
      -y) yes=1 ;;
      --no-install-recommends) no_recommends=1 ;;
      --allow-downgrades) allow_downgrade=1 ;;
      nodejs|nodejs:amd64=22.0.0-1nodesource1)
        [[ -z "$selector" ]] || return 90
        selector="$arg"
        ;;
      *) return 90 ;;
    esac
  done
  [[ "$yes" == 1 && "$no_recommends" == 1 ]] || return 90
  # An unversioned install retains the newer installed package, as real APT did.
  if [[ "$selector" == nodejs && "$allow_downgrade" == 0 ]]; then return 0; fi
  [[ "$selector" == nodejs:amd64=22.0.0-1nodesource1 ]] || return 90
  if [[ "$(cat "$PWD/package-state")" == *24.15.0* ]]; then
    [[ "$allow_downgrade" == 1 ]] || return 90
  else
    [[ "$allow_downgrade" == 0 ]] || return 90
  fi
  cp -R "$PWD/apt-payload/." "$PWD/apt/"
  printf 'installed\\t22.0.0-1nodesource1\\tamd64\\n' >"$PWD/package-state"
  fixture_after_apt
}
fixture_after_apt() { :; }
`;
  return {
    ...context,
    shell,
    runRebake: (body) => context.run(shell + body, { FIXTURE_DIGEST: digest }),
    destination: path.join(root, "tools", "node", "24.19.0", "x64"),
  };
}

function goArchiveFixture(t) {
  const context = fixture(t);
  const { root } = context;
  const bin = path.join(root, "payload", "go", "bin");
  fs.mkdirSync(bin, { recursive: true });
  // These executables model publication ordering, not Linux Go/CGO execution.
  writeTool(
    path.join(bin, "go"),
    `
[[ "$GOTOOLCHAIN" == local && "$GOPROXY" == off && "$GOENV" == off ]]
[[ -d "$GOCACHE" && "$GOCACHE" == "$TMPDIR/"* ]]
[[ ! -f "$FIXTURE_MARKER" ]] || exit 92
printf '%s\\n' "$*" >>"$FIXTURE_LOG"
case "$*" in
  version) printf 'go version go1.27.0 linux/amd64\\n' ;;
  'env GOOS GOARCH') printf 'linux\\namd64\\n' ;;
  'test bytes crypto/sha256') exit "\${FIXTURE_GO_FAILURE:-0}" ;;
  'run main.go') [[ "$CGO_ENABLED" == 1 ]]; grep -q 'C.answer()' main.go; printf 'go-cgo-ok\\n' ;;
  *) exit 93 ;;
esac`,
  );
  writeTool(path.join(bin, "gofmt"), 'cat "$1"');
  const archive = path.join(root, "archives", goArchive);
  const pack = () => {
    success(
      spawnSync("tar", ["-czf", archive, "-C", path.join(root, "payload"), "go"], {
        encoding: "utf8",
      }),
    );
    return createHash("sha256").update(fs.readFileSync(archive)).digest("hex");
  };
  const destination = path.join(root, "tools", "go", "1.27.0", "x64");
  const shell = `
public_toolchain_archive_dir="$PWD/archives"
go_toolcache_root="$PWD/tools"
go_link_dir="$PWD/links"
toolchain_archive_spec() { printf 'sha256 %s https://example.invalid/go.tar.gz\\n' "$FIXTURE_DIGEST"; }
`;
  return {
    ...context,
    bin,
    archive,
    pack,
    shell,
    destination,
    runGo: (body, env = {}) =>
      context.run(shell + body, {
        FIXTURE_DIGEST: pack(),
        FIXTURE_MARKER: `${destination}.complete`,
        FIXTURE_LOG: path.join(root, "home", "go.calls"),
        ...env,
      }),
  };
}

const defaultGoPreparation = `
uname() { printf 'Linux\\n'; }
dpkg() { printf 'amd64\\n'; }
cache_public_toolchain_archives() { printf 'cache\\n' >>"$HOME/preparation.log"; }
curl() { printf 'network\\n' >>"$HOME/preparation.log"; return 89; }
`;

test("Go image publication uses fresh authenticated bytes and marks completion after functional checks", (t) => {
  const { root, destination, runGo } = goArchiveFixture(t);
  fs.mkdirSync(path.join(destination, "bin"), { recursive: true });
  writeTool(path.join(destination, "bin", "go"), 'touch "$HOME/poison-executed"; exit 0');
  fs.writeFileSync(`${destination}.complete`, "");
  success(runGo("install_pinned_go"));
  assert.equal(fs.existsSync(path.join(root, "home", "poison-executed")), false);
  assert.equal(fs.existsSync(`${destination}.complete`), true);
  for (const tool of ["go", "gofmt"]) {
    assert.equal(
      fs.readlinkSync(path.join(root, "links", tool)),
      path.join(destination, "bin", tool),
    );
  }
  assert.match(
    fs.readFileSync(path.join(root, "home", "go.calls"), "utf8"),
    /test bytes crypto\/sha256\nrun main.go\n/,
  );
  assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);

  const failed = runGo(`${defaultGoPreparation}install_go_toolchain`, { FIXTURE_GO_FAILURE: "47" });
  assert.equal(failed.status, 47, failed.stderr);
  assert.equal(fs.existsSync(`${destination}.complete`), false);
  assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
});

for (const failure of ["tamper", "wrong architecture"]) {
  test(`Go image publication rejects ${failure} without executing a cached tree`, (t) => {
    const { root, bin, destination, runGo } = goArchiveFixture(t);
    if (failure === "wrong architecture") {
      writeTool(path.join(bin, "go"), "printf 'go version go1.27.0 linux/arm64\\n'");
    }
    const result = runGo(`
${failure === "tamper" ? `printf corrupt >"$public_toolchain_archive_dir/${goArchive}"` : ""}
install_pinned_go
`);
    assert.notEqual(result.status, 0);
    if (failure === "tamper") assert.match(result.stderr, /checksum mismatch/);
    else assert.match(result.stderr, /unexpected Go version or architecture/);
    assert.equal(fs.existsSync(`${destination}.complete`), false);
    assert.equal(fs.existsSync(path.join(root, "links", "go")), false);
    assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
  });
}

for (const state of ["fresh", "current", "dangling"]) {
  test(`Go public aliases retain exact ownership on ${state} complete-flow repeat`, (t) => {
    const { root, destination, runGo } = goArchiveFixture(t);
    if (state !== "fresh") {
      success(runGo("install_pinned_go"));
      if (state === "dangling") fs.rmSync(path.join(destination, "bin"), { recursive: true });
    }
    const result = runGo(`${defaultGoPreparation}
node_major=22
export PATH="$go_link_dir:$PATH"
for pass in 1 2; do
  install_go_toolchain
  for tool in go gofmt; do
    [[ "$(command -v "$tool")" == "$go_link_dir/$tool" ]] || exit 91
  done
  printf 'package fixture\\n' >"$HOME/input.go"
  [[ "$(gofmt "$HOME/input.go")" == 'package fixture' ]] || exit 92
done
`);
    success(result);
    for (const tool of ["go", "gofmt"]) {
      assert.equal(
        fs.readlinkSync(path.join(root, "links", tool)),
        path.join(destination, "bin", tool),
      );
    }
    assert.equal(fs.existsSync(`${destination}.complete`), true);
    assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
    assert.deepEqual(fs.readdirSync(path.join(root, "links")).sort(), ["go", "gofmt"]);
  });
}

for (const entry of ["install_go_toolchain", "install_pinned_go"]) {
  for (const kind of ["regular", "directory", "foreign", "wrong-name", "relative", "newline"]) {
    test(`Go public aliases reject last ${kind} conflict through ${entry} before preparation`, (t) => {
      const { root, destination, runGo, run, shell, pack } = goArchiveFixture(t);
      success(runGo("install_pinned_go"));
      const links = path.join(root, "links");
      const conflict = path.join(links, "gofmt");
      fs.unlinkSync(conflict);
      if (kind === "regular") {
        writeTool(conflict, "echo operator-gofmt");
      } else if (kind === "directory") {
        fs.mkdirSync(conflict);
        fs.writeFileSync(path.join(conflict, "keep"), "operator directory");
      } else {
        const target = {
          foreign: path.join(root, "foreign", "gofmt"),
          "wrong-name": path.join(destination, "bin", "go"),
          relative: path.relative(links, path.join(destination, "bin", "gofmt")),
          newline: path.join(destination, "bin", "gofmt") + "\n",
        }[kind];
        fs.symlinkSync(target, conflict);
      }
      fs.writeFileSync(path.join(destination, "keep"), "existing tree");
      fs.writeFileSync(`${destination}.complete`, "existing marker");
      const digest = pack();
      const calls = path.join(root, "home", "go.calls");
      const before = {
        links: fileState(links),
        tree: fileState(destination),
        marker: fileState(`${destination}.complete`),
        archives: fileState(path.join(root, "archives")),
        calls: fileState(calls),
      };
      const result = run(`${shell}${defaultGoPreparation}${entry}`, {
        FIXTURE_DIGEST: digest,
        FIXTURE_MARKER: `${destination}.complete`,
        FIXTURE_LOG: calls,
      });
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /public tool.*gofmt.*resolve before rebake/i);
      assert.deepEqual(fileState(links), before.links);
      assert.deepEqual(fileState(destination), before.tree);
      assert.deepEqual(fileState(`${destination}.complete`), before.marker);
      assert.deepEqual(fileState(path.join(root, "archives")), before.archives);
      assert.deepEqual(fileState(calls), before.calls);
      assert.equal(fs.existsSync(path.join(root, "home", "preparation.log")), false);
      assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
    });
  }
}

test("Go public aliases recheck the whole pair after preparation before replacing the slot", (t) => {
  const { root, destination, runGo } = goArchiveFixture(t);
  success(runGo("install_pinned_go"));
  const links = path.join(root, "links");
  fs.writeFileSync(path.join(destination, "keep"), "existing tree");
  fs.writeFileSync(`${destination}.complete`, "existing marker");
  const tree = fileState(destination);
  const marker = fileState(`${destination}.complete`);
  const calls = fileState(path.join(root, "home", "go.calls"));
  const result = runGo(`${defaultGoPreparation}
cache_public_toolchain_archives() {
  rm "$go_link_dir/gofmt"
  printf 'operator-gofmt\\n' >"$go_link_dir/gofmt"
}
install_go_toolchain
`);
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /public tool.*gofmt.*resolve before rebake/i);
  assert.equal(fs.readFileSync(path.join(links, "gofmt"), "utf8"), "operator-gofmt\n");
  assert.equal(fs.readlinkSync(path.join(links, "go")), path.join(destination, "bin", "go"));
  assert.deepEqual(fileState(destination), tree);
  assert.deepEqual(fileState(`${destination}.complete`), marker);
  assert.deepEqual(fileState(path.join(root, "home", "go.calls")), calls);
  assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
});

test("Go archive pin remains exact 1.27.0 independently of the Node major", (t) => {
  const { run } = fixture(t);
  const result = run(`node_major=22\ntoolchain_archive_spec ${goArchive}`);
  success(result);
  assert.equal(
    result.stdout.trim(),
    `sha256 675c26c449cbb18fc24b74650de1eabbae6e16f64326fd85a283fb3b58280685 https://go.dev/dl/${goArchive}`,
  );
});

test(
  "Go public-archive native smoke requires a nonroot Linux x64 host",
  {
    skip:
      (process.platform !== "linux" ||
        process.arch !== "x64" ||
        process.getuid?.() === 0 ||
        !publicArchives ||
        !fs.existsSync(path.join(publicArchives, goArchive))) &&
      "requires a nonroot Linux x64 host with the reviewed Go archive",
  },
  (t) => {
    const { run } = fixture(t);
    success(
      run('public_toolchain_archive_dir="$PUBLIC_ARCHIVES"\noffline_go_probe', {
        PUBLIC_ARCHIVES: publicArchives,
      }),
    );
  },
);

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
  const { root, run, bin, pack, shell } = nodeArchiveFixture(t);
  const destination = path.join(root, "tools", "node", "24.19.0", "x64");
  fs.mkdirSync(destination, { recursive: true });
  fs.writeFileSync(path.join(destination, "poison"), "same-version cache is not authenticated");
  fs.mkdirSync(path.join(destination, "bin"));
  writeTool(
    path.join(destination, "bin", "node"),
    'touch "$HOME/poison-executed"; printf "v24.19.0\\n"',
  );
  fs.writeFileSync(`${destination}.complete`, "");
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

const publicNodeTools = ["node", "npm", "npx", "corepack", "pnpm", "pnpx"];
const defaultNodePreparation = `
dpkg() { printf 'amd64\\n'; }
cache_public_toolchain_archives() { printf 'cache\\n' >>"$HOME/preparation.log"; }
curl() { printf 'network\\n' >>"$HOME/preparation.log"; return 89; }
`;

function fileState(file) {
  let stat;
  try {
    stat = fs.lstatSync(file);
  } catch (error) {
    if (error.code === "ENOENT") return null;
    throw error;
  }
  if (stat.isSymbolicLink()) return { link: fs.readlinkSync(file) };
  if (stat.isDirectory()) {
    return Object.fromEntries(
      fs
        .readdirSync(file)
        .sort()
        .map((name) => [name, fileState(path.join(file, name))]),
    );
  }
  return { mode: stat.mode, contents: fs.readFileSync(file).toString("base64") };
}

for (const state of ["fresh", "current", "dangling"]) {
  test(`default Node complete flow preserves public ownership and Yarn on ${state} repeat`, (t) => {
    const { root, run, setup, shell, pack } = nodeArchiveFixture(t);
    const digest = pack();
    const destination = path.join(root, "tools", "node", "24.19.0", "x64");
    const links = path.join(root, "links");
    if (state !== "fresh") {
      success(run(shell, { FIXTURE_DIGEST: digest }));
      if (state === "dangling") fs.rmSync(path.join(destination, "bin"), { recursive: true });
    }
    fs.mkdirSync(links, { recursive: true });
    writeTool(path.join(links, "yarn"), "echo operator-yarn");
    writeTool(path.join(root, "operator-yarnpkg"), "echo operator-yarnpkg");
    fs.symlinkSync(path.join(root, "operator-yarnpkg"), path.join(links, "yarnpkg"));
    const yarn = fileState(path.join(links, "yarn"));
    const yarnpkg = fileState(path.join(links, "yarnpkg"));
    const result = run(
      `${setup}${defaultNodePreparation}
: >"$HOME/corepack.log"
for pass in 1 2; do
  install_node_pnpm
  [[ "$(node --version)" == v24.19.0 ]] || exit 91
  for tool in node npm npx corepack pnpm pnpx; do
    [[ "$(command -v "$tool")" == "$node_link_dir/$tool" ]] || exit 92
    "$tool" --version
  done
done
`,
      { FIXTURE_DIGEST: digest },
    );
    success(result);
    for (const tool of publicNodeTools) {
      assert.equal(fs.readlinkSync(path.join(links, tool)), path.join(destination, "bin", tool));
    }
    assert.deepEqual(fileState(path.join(links, "yarn")), yarn);
    assert.deepEqual(fileState(path.join(links, "yarnpkg")), yarnpkg);
    const calls = fs
      .readFileSync(path.join(root, "home", "corepack.log"), "utf8")
      .trim()
      .split("\n");
    assert.equal(calls.length, 4, "each pass enables only private shims, then prepares pnpm");
    assert.equal(calls.filter((line) => line.includes("enable --install-directory")).length, 2);
    assert.equal(
      calls.filter((line) => line === "24 prepare pnpm@11.1.0 --activate node=v24.19.0").length,
      2,
    );
    assert.equal(fs.existsSync(`${destination}.complete`), true);
    assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
    assert.deepEqual(fs.readdirSync(links).sort(), [...publicNodeTools, "yarn", "yarnpkg"].sort());
  });
}

for (const entry of ["install_node_pnpm", "install_pinned_node"]) {
  for (const kind of [
    "regular",
    "directory",
    "foreign",
    "wrong-name",
    "relative",
    "newline",
    "legacy-corepack",
  ]) {
    test(`default Node ${entry} rejects last ${kind} alias before changing prepared state`, (t) => {
      const { root, run, setup, shell, pack } = nodeArchiveFixture(t);
      const digest = pack();
      success(run(shell, { FIXTURE_DIGEST: digest }));
      const destination = path.join(root, "tools", "node", "24.19.0", "x64");
      const links = path.join(root, "links");
      const conflict = path.join(links, "pnpx");
      fs.unlinkSync(conflict);
      if (kind === "regular") {
        writeTool(conflict, "echo operator-pnpx");
      } else if (kind === "directory") {
        fs.mkdirSync(conflict);
        fs.writeFileSync(path.join(conflict, "keep"), "operator directory");
      } else {
        const target = {
          foreign: path.join(root, "foreign", "pnpx"),
          "wrong-name": path.join(destination, "bin", "node"),
          relative: path.relative(links, path.join(destination, "bin", "pnpx")),
          newline: path.join(destination, "bin", "pnpx") + "\n",
          "legacy-corepack": path.relative(
            links,
            path.join(destination, "lib/node_modules/corepack/dist/pnpx.js"),
          ),
        }[kind];
        fs.symlinkSync(target, conflict);
      }
      fs.writeFileSync(path.join(destination, "keep"), "existing tree");
      fs.writeFileSync(`${destination}.complete`, "existing marker");
      const before = {
        links: fileState(links),
        tree: fileState(destination),
        marker: fileState(`${destination}.complete`),
        archives: fileState(path.join(root, "archives")),
      };
      const result = run(
        `${setup}${defaultNodePreparation}
if ${entry}; then exit 91; fi
`,
        { FIXTURE_DIGEST: digest },
      );
      success(result);
      assert.match(result.stderr, /public tool.*pnpx.*resolve before rebake/i);
      assert.deepEqual(fileState(links), before.links);
      assert.deepEqual(fileState(destination), before.tree);
      assert.deepEqual(fileState(`${destination}.complete`), before.marker);
      assert.deepEqual(fileState(path.join(root, "archives")), before.archives);
      assert.equal(fs.existsSync(path.join(root, "home", "preparation.log")), false);
      assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
    });
  }
}

test("default Node rechecks all aliases after preparation before replacing the image slot", (t) => {
  const { root, run, setup, shell, pack } = nodeArchiveFixture(t);
  const digest = pack();
  success(run(shell, { FIXTURE_DIGEST: digest }));
  const destination = path.join(root, "tools", "node", "24.19.0", "x64");
  const links = path.join(root, "links");
  fs.writeFileSync(path.join(destination, "keep"), "existing tree");
  fs.writeFileSync(`${destination}.complete`, "existing marker");
  const tree = fileState(destination);
  const marker = fileState(`${destination}.complete`);
  const result = run(
    `${setup}${defaultNodePreparation}
cache_public_toolchain_archives() {
  rm "$node_link_dir/pnpx"
  printf 'operator-pnpx\\n' >"$node_link_dir/pnpx"
}
if install_node_pnpm; then exit 91; fi
`,
    { FIXTURE_DIGEST: digest },
  );
  success(result);
  assert.match(result.stderr, /public tool.*pnpx.*resolve before rebake/i);
  assert.equal(fs.readFileSync(path.join(links, "pnpx"), "utf8"), "operator-pnpx\n");
  for (const tool of publicNodeTools.slice(0, -1)) {
    assert.equal(fs.readlinkSync(path.join(links, tool)), path.join(destination, "bin", tool));
  }
  assert.deepEqual(fileState(destination), tree);
  assert.deepEqual(fileState(`${destination}.complete`), marker);
});

for (const dangling of [false, true]) {
  test(`Node-major rebake selects Node22 after a pinned install with ${dangling ? "dangling" : "live"} owned links`, (t) => {
    const { root, runRebake, destination, archive } = nodeRebakeFixture(t);
    const before = fs.readFileSync(archive);
    const result = runRebake(`
${dangling ? 'mv "$node_toolcache_root/node/24.19.0/x64/bin" "$node_toolcache_root/node/24.19.0/x64/saved-bin"' : ""}
install_node_pnpm
printf 'selected=%s\\n' "$(node --version)"
for tool in node npm npx corepack pnpm pnpx; do
  [[ "$(command -v "$tool")" == "$PWD/apt/bin/$tool" ]] || exit 91
  "$tool" --version
done
`);
    success(result);
    assert.match(result.stdout, /selected=v22\.0\.0/);
    assert.deepEqual(
      new Set(fs.readFileSync(path.join(root, "apt-calls"), "utf8").trim().split(/\s+/)),
      new Set([
        "install",
        "-y",
        "--no-install-recommends",
        "--allow-downgrades",
        "nodejs:amd64=22.0.0-1nodesource1",
      ]),
    );
    for (const tool of ["node", "npm", "npx", "corepack", "pnpm", "pnpx"]) {
      assert.equal(fs.existsSync(path.join(root, "links", tool)), false);
      assert.throws(() => fs.lstatSync(path.join(root, "links", tool)), { code: "ENOENT" });
    }
    assert.equal(
      fs.readFileSync(path.join(root, "home", "corepack.log"), "utf8"),
      "22 enable node=v22.0.0\n22 prepare pnpm@10.0.0 --activate node=v22.0.0\n",
    );
    assert.deepEqual(fs.readFileSync(archive), before);
    assert.equal(fs.existsSync(`${destination}.complete`), true);
    assert.equal(
      fs.existsSync(path.join(destination, dangling ? "saved-bin" : "bin", "node")),
      true,
    );
  });
}

test("Node-major rebake preserves exact owned links when APT fails even in a conditional caller", (t) => {
  const { root, runRebake, destination } = nodeRebakeFixture(t);
  const result = runRebake(`
FIXTURE_APT_EXIT=43
if install_node_pnpm; then exit 91; fi
[[ "$(node --version)" == v24.19.0 ]]
`);
  success(result);
  for (const tool of ["node", "npm", "npx", "corepack", "pnpm", "pnpx"]) {
    assert.equal(
      fs.readlinkSync(path.join(root, "links", tool)),
      path.join(destination, "bin", tool),
    );
  }
  assert.equal(fs.readFileSync(path.join(root, "home", "corepack.log"), "utf8"), "");
  assert.equal(fs.existsSync(`${destination}.complete`), true);
});

for (const [name, setup, diagnostic, transaction] of [
  ["missing candidate", ': >"$PWD/apt-versions"', /no NodeSource Node 22 package/, false],
  [
    "foreign repository only",
    "sed -i.bak 's|https://deb.nodesource.com|https://other.invalid|g' \"$PWD/apt-versions\"",
    /no NodeSource Node 22 package/,
    false,
  ],
  [
    "foreign architecture only",
    "sed -i.bak 's/amd64 Packages/arm64 Packages/g' \"$PWD/apt-versions\"",
    /no NodeSource Node 22 package/,
    false,
  ],
  [
    "same-major downgrade",
    "printf 'installed\\t22.1.0-1nodesource1\\tamd64\\n' >\"$PWD/package-state\"",
    /refusing Node package downgrade/,
    false,
  ],
  [
    "other-major downgrade",
    "printf 'installed\\t26.0.0-1nodesource1\\tamd64\\n' >\"$PWD/package-state\"",
    /refusing Node package downgrade/,
    false,
  ],
  [
    "wrong installed version",
    "fixture_after_apt() { printf 'installed\\t24.15.0-1nodesource1\\tamd64\\n' >\"$PWD/package-state\"; }",
    /package replacement verification failed/,
    true,
  ],
  [
    "wrong installed architecture",
    "fixture_after_apt() { printf 'installed\\t22.0.0-1nodesource1\\tarm64\\n' >\"$PWD/package-state\"; }",
    /package replacement verification failed/,
    true,
  ],
  [
    "incomplete installation",
    "fixture_after_apt() { printf 'unpacked\\t22.0.0-1nodesource1\\tamd64\\n' >\"$PWD/package-state\"; }",
    /package replacement verification failed/,
    true,
  ],
  [
    "wrong package binary",
    'fixture_after_apt() { cp "$node_toolcache_root/node/24.19.0/x64/bin/node" "$PWD/apt/bin/node"; }',
    /package-owned Node binary verification failed/,
    true,
  ],
  [
    "symlink package binary",
    'fixture_after_apt() { rm "$PWD/apt/bin/node"; ln -s "$PWD/apt-payload/bin/node" "$PWD/apt/bin/node"; }',
    /package-owned Node binary verification failed/,
    true,
  ],
]) {
  test(`Node-major rebake preserves owned links and caches on ${name}`, (t) => {
    const { root, runRebake } = nodeRebakeFixture(t);
    const result = runRebake(`
${setup}
cp -R "$node_link_dir" "$PWD/links-before"
cp -R "$node_toolcache_root" "$PWD/tools-before"
if install_node_pnpm; then exit 91; fi
[[ "$(hash -t node)" == "$node_link_dir/node" ]]
[[ "$(node --version)" == v24.19.0 ]]
`);
    success(result);
    assert.match(result.stderr, diagnostic);
    assert.doesNotMatch(result.stderr, /resolve PATH/);
    assert.deepEqual(
      fileState(path.join(root, "links")),
      fileState(path.join(root, "links-before")),
    );
    assert.deepEqual(
      fileState(path.join(root, "tools")),
      fileState(path.join(root, "tools-before")),
    );
    assert.equal(fs.readFileSync(path.join(root, "home", "corepack.log"), "utf8"), "");
    assert.equal(fs.existsSync(path.join(root, "apt-calls")), transaction);
  });
}

test("Node-major rebake updates an older selected-major package without permitting downgrades", (t) => {
  const { root, runRebake } = nodeRebakeFixture(t);
  success(
    runRebake(`
printf 'installed\\t22.0.0-0nodesource1\\tamd64\\n' >"$PWD/package-state"
install_node_pnpm
[[ "$(node --version)" == v22.0.0 ]]
`),
  );
  assert.deepEqual(
    new Set(fs.readFileSync(path.join(root, "apt-calls"), "utf8").trim().split(/\s+/)),
    new Set(["install", "-y", "--no-install-recommends", "nodejs:amd64=22.0.0-1nodesource1"]),
  );
});

test("Node-major rebake rechecks its retirement set and preserves an operator replacement during APT", (t) => {
  const { root, runRebake } = nodeRebakeFixture(t);
  success(
    runRebake(`
fixture_after_apt() {
  rm "$node_link_dir/node"
  cp "$PWD/apt-payload/bin/node" "$node_link_dir/node"
}
install_node_pnpm
[[ "$(node --version)" == v22.0.0 ]]
`),
  );
  assert.equal(fs.lstatSync(path.join(root, "links", "node")).isFile(), true);
  assert.deepEqual(
    fs.readFileSync(path.join(root, "links", "node")),
    fs.readFileSync(path.join(root, "apt-payload", "bin", "node")),
  );
});

test("Node-major rebake supports a system with no owned public aliases", (t) => {
  const { root, runRebake } = nodeRebakeFixture(t);
  success(
    runRebake(`
rm "$node_link_dir/"*
install_node_pnpm
[[ "$(node --version)" == v22.0.0 ]]
`),
  );
  assert.deepEqual(fs.readdirSync(path.join(root, "links")), []);
  assert.match(
    fs.readFileSync(path.join(root, "home", "corepack.log"), "utf8"),
    /22 prepare pnpm@10.0.0 --activate node=v22.0.0/,
  );
});

for (const [name, replacement, target, fails] of [
  ["regular operator file", 'cp "$PWD/apt-payload/bin/node" "$node_link_dir/node"', null, false],
  [
    "foreign absolute link",
    'ln -s "$PWD/apt-payload/bin/node" "$node_link_dir/node"',
    "apt-payload/bin/node",
    false,
  ],
  [
    "relative owned-tree link",
    'ln -s ../tools/node/24.19.0/x64/bin/node "$node_link_dir/node"',
    "../tools/node/24.19.0/x64/bin/node",
    true,
  ],
  [
    "different-name owned-tree link",
    'ln -s "$node_toolcache_root/node/24.19.0/x64/bin/npm" "$node_link_dir/node"',
    "tools/node/24.19.0/x64/bin/npm",
    true,
  ],
  [
    "operator wrong-major shadow",
    'cp "$node_toolcache_root/node/24.19.0/x64/bin/node" "$node_link_dir/node"',
    null,
    true,
  ],
]) {
  test(`Node-major rebake preserves ${name}${fails ? " and fails before Corepack" : ""}`, (t) => {
    const { root, runRebake, destination } = nodeRebakeFixture(t);
    const result = runRebake(`
rm "$node_link_dir/node"
${replacement}
cp "$node_link_dir/node" "$PWD/operator-before"
install_node_pnpm
`);
    if (fails) {
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /requested Node major 22.*resolve.*PATH/i);
      assert.equal(fs.readFileSync(path.join(root, "home", "corepack.log"), "utf8"), "");
    } else {
      success(result);
      assert.equal(
        fs.readFileSync(path.join(root, "home", "corepack.log"), "utf8"),
        "22 enable node=v22.0.0\n22 prepare pnpm@10.0.0 --activate node=v22.0.0\n",
      );
    }
    const link = path.join(root, "links", "node");
    if (target) {
      assert.equal(
        fs.readlinkSync(link),
        target.startsWith("..") ? target : path.join(root, target),
      );
    } else {
      assert.equal(fs.lstatSync(link).isFile(), true);
      assert.deepEqual(fs.readFileSync(link), fs.readFileSync(path.join(root, "operator-before")));
    }
    assert.equal(fs.existsSync(`${destination}.complete`), true);
  });
}

test("Node-major rebake leaves operator directories, relative and newline targets, and unrelated link names intact", (t) => {
  const { root, runRebake, destination } = nodeRebakeFixture(t);
  success(
    runRebake(`
rm "$node_link_dir/npm" "$node_link_dir/npx" "$node_link_dir/pnpm" "$node_link_dir/pnpx"
cp "$PWD/apt-payload/bin/npm" "$node_link_dir/npm"
mkdir "$node_link_dir/npx"
printf 'operator directory\\n' >"$node_link_dir/npx/keep"
ln -s ../tools/node/24.19.0/x64/bin/pnpm "$node_link_dir/pnpm"
ln -s "$node_toolcache_root/node/24.19.0/x64/bin/pnpx"$'\\n' "$node_link_dir/pnpx"
ln -s "$node_toolcache_root/node/24.19.0/x64/bin/node" "$node_link_dir/node-other"
install_node_pnpm
[[ "$(node --version)" == v22.0.0 ]]
`),
  );
  const links = path.join(root, "links");
  assert.equal(fs.lstatSync(path.join(links, "npm")).isFile(), true);
  assert.deepEqual(
    fs.readFileSync(path.join(links, "npm")),
    fs.readFileSync(path.join(root, "apt-payload", "bin", "npm")),
  );
  assert.equal(fs.readFileSync(path.join(links, "npx", "keep"), "utf8"), "operator directory\n");
  assert.equal(fs.readlinkSync(path.join(links, "pnpm")), "../tools/node/24.19.0/x64/bin/pnpm");
  assert.equal(
    fs.readlinkSync(path.join(links, "pnpx")),
    path.join(destination, "bin", "pnpx") + "\n",
  );
  assert.equal(
    fs.readlinkSync(path.join(links, "node-other")),
    path.join(destination, "bin", "node"),
  );
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
node_link_dir="$PWD/links"
node_toolcache_root="$PWD/tools"
dpkg() { printf '%s\\n' "$FIXTURE_ARCH"; }
cache_public_toolchain_archives() { echo public-archives; }
install_pinned_node() { echo pinned-node; }
apt_install() { echo "apt $*"; }
install_requested_node() { echo "apt nodejs"; }
command() { return 0; }
node() { printf 'v%s.0.0\\n' "$CRABBOX_LINUX_NODE_MAJOR"; }
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
