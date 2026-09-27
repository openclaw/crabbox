import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const installer = new URL("./install-linux-developer-tools.sh", import.meta.url).pathname;
const versions = { rust: "1.98.1", uv: "0.12.19" };
const commands = { rust: ["rustc", "cargo", "rustdoc"], uv: ["uv", "uvx"] };

test(
  "reviewed Linux Rust and uv archives execute installed and fresh offline toolchains",
  {
    skip: process.env.CRABBOX_TEST_RUST_UV_DOWNLOAD !== "1",
  },
  (t) => {
    assert.equal(process.platform, "linux", "opt-in proof requires Linux");
    assert.equal(process.arch, "x64", "opt-in proof requires x86_64");
    assert.notEqual(process.getuid(), 0, "opt-in proof requires a nonroot runtime user");
    const root = fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-reviewed-rust-uv-"));
    t.after(() => fs.rmSync(root, { recursive: true, force: true }));
    for (const name of ["home", "tmp", "bin"]) fs.mkdirSync(path.join(root, name));
    // GNU timeout owns the child process group, including downloads, before cleanup.
    const result = spawnSync(
      "/usr/bin/timeout",
      [
        "--kill-after=2s",
        "240s",
        "/bin/bash",
        "-c",
        `
source "$INSTALLER"
public_toolchain_archive_dir="$PWD/archives"
developer_bin_dir="$PWD/bin"
rust_toolchain_root="$PWD/tools/rust"
uv_toolchain_root="$PWD/tools/uv"
install_rust_uv_toolchain rust
install_rust_uv_toolchain uv
printf 'runtime-uid=%s\\n' "$(id -u)"
rustc --version
cargo --version
rustdoc --version
uv --version
uvx --version
offline_rust_probe
printf 'installed-and-fresh-rust-offline-ok\\n'
offline_uv_probe
printf 'installed-and-fresh-uv-offline-ok\\n'
printf 'reviewed-rust-uv-offline-ok\\n'
`,
      ],
      {
        cwd: root,
        env: {
          HOME: `${root}/home`,
          TMPDIR: `${root}/tmp`,
          PATH: `${root}/bin:/usr/bin:/bin`,
          INSTALLER: installer,
        },
        encoding: "utf8",
        timeout: 260_000,
        maxBuffer: 1024 * 1024,
      },
    );
    success(result);
    console.log(
      result.stdout
        .split("\n")
        .filter((line) =>
          /^(runtime-uid=|rustc |cargo |rustdoc |uv |uvx |installed-and-fresh-|reviewed-rust-uv-)/.test(
            line,
          ),
        )
        .join("\n"),
    );
    assert.match(result.stdout, /reviewed-rust-uv-offline-ok/);
    assert.deepEqual(fs.readdirSync(`${root}/tmp`), []);
  },
);

function success(result) {
  assert.equal(result.status, 0, result.error?.message || result.stderr || result.stdout);
}

function executable(file, source) {
  fs.writeFileSync(file, `#!/bin/bash\nset -euo pipefail\n${source}\n`, { mode: 0o755 });
}

function fixture(t, tool, { version = versions[tool], failure = "" } = {}) {
  const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-rust-uv-")));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  for (const name of ["archives", "tmp", "home", "bin", "fixture-bin"])
    fs.mkdirSync(path.join(root, name));
  const packageName =
    tool === "rust"
      ? `rust-${versions.rust}-x86_64-unknown-linux-gnu`
      : "uv-x86_64-unknown-linux-gnu";
  const payload = path.join(root, packageName);
  fs.mkdirSync(payload);
  const binaryDirectory = tool === "rust" ? path.join(payload, "rustc", "bin") : payload;
  fs.mkdirSync(binaryDirectory, { recursive: true });
  if (tool === "rust") {
    for (const name of ["cargo", "rust-std-x86_64-unknown-linux-gnu"])
      fs.mkdirSync(path.join(payload, name));
    fs.writeFileSync(
      path.join(payload, "components"),
      "rustc\ncargo\nrust-std-x86_64-unknown-linux-gnu\n",
    );
    fs.writeFileSync(path.join(payload, "rust-installer-version"), "3\n");
    fs.writeFileSync(
      path.join(payload, "install.sh"),
      `#!/bin/sh
set -eu
test "$2" = --components=rustc,cargo,rust-std-x86_64-unknown-linux-gnu && test "$3" = --disable-ldconfig
destination="\${1#--prefix=}"
mkdir -p "$destination/bin"
cp "$(dirname "$0")/rustc/bin/"* "$destination/bin/"
`,
      { mode: 0o755 },
    );
  }
  for (const name of commands[tool]) {
    executable(
      path.join(binaryDirectory, name),
      `
name=${name}
[[ "$HOME" == */home && "$TMPDIR" == */check || "$TMPDIR" == */installed || "$TMPDIR" == */extracted ]]
[[ -z "\${RUSTUP_HOME:-}" && -z "\${PYTHONPATH:-}" && -z "\${CARGO_TARGET_DIR:-}" ]]
printf '%s %s\\n' "$name" "$*" >>${JSON.stringify(path.join(root, "calls"))}
[[ '${failure}' != "$name:\${1:-}" ]] || exit 73
case "$name:$*" in
  rustc:"--version --verbose") printf 'rustc ${version} (fixture)\\nhost: x86_64-unknown-linux-gnu\\n' ;;
  cargo:--version|rustdoc:--version) printf '%s ${version} (fixture)\\n' "$name" ;;
  uv:--version|uvx:--version) printf '%s ${version}\\n' "$name" ;;
  cargo:"test --offline") [[ "$CARGO_NET_OFFLINE" == true && -f src/main.rs && -f Cargo.toml && "$CARGO_HOME" == "$TMPDIR/cargo" ]] ;;
  cargo:"run --offline --quiet") printf '42\\n' ;;
  rustdoc:"--test src/main.rs") [[ -f src/main.rs ]] ;;
  uv:"venv --python "*)
    [[ "$UV_OFFLINE" == 1 && "$UV_NO_CONFIG" == 1 && "$UV_PYTHON_DOWNLOADS" == never && "$UV_CACHE_DIR" == "$TMPDIR/cache" ]]
    mkdir -p "$4/bin"
    printf '#!/bin/sh\\nexit 0\\n' >"$4/bin/python"
    chmod 755 "$4/bin/python" ;;
  uv:"pip install --python "*) [[ -f "$5" && -x "$4" ]] ;;
  uvx:"--python "*) [[ "$3" == --from && -f "$4" && "$5" == crabbox-uv-smoke ]]; printf '42\\n' ;;
  *) exit 64 ;;
esac
`,
    );
  }
  const archiveName = `${tool}-${versions[tool]}-x86_64-unknown-linux-gnu.tar.${tool === "rust" ? "xz" : "gz"}`;
  const archive = path.join(root, "archives", archiveName);
  success(
    spawnSync("tar", [tool === "rust" ? "-cJf" : "-czf", archive, "-C", root, packageName], {
      encoding: "utf8",
    }),
  );
  const digest = createHash("sha256").update(fs.readFileSync(archive)).digest("hex");
  for (const [name, output] of [
    ["uname", "Linux"],
    ["dpkg", "amd64"],
    ["getconf", "glibc 2.39"],
    ["id", "1000"],
  ]) {
    executable(path.join(root, "fixture-bin", name), `echo '${output}'`);
  }
  // Darwin's mktemp ignores TMPDIR without a template; all fixture scratch stays owned.
  executable(
    path.join(root, "fixture-bin", "mktemp"),
    `
if [[ "$#" == 1 && "$1" == -d ]]; then exec /usr/bin/mktemp -d "$TMPDIR/tmp.XXXXXXXX"; fi
exec /usr/bin/mktemp "$@"
`,
  );
  const setup = `
public_toolchain_archive_dir="$PWD/archives"
developer_bin_dir="$PWD/bin"
rust_toolchain_root="$PWD/tools/rust"
uv_toolchain_root="$PWD/tools/uv"
toolchain_archive_spec() { [[ "$1" == '${archiveName}' ]] || return 1; printf 'sha256 ${digest} https://example.invalid/%s\\n' "$1"; }
curl() { echo unexpected-network >&2; return 89; }
`;
  const run = (body, env = {}) =>
    spawnSync("/bin/bash", ["-c", `source "$INSTALLER"\n${setup}\n${body}`], {
      cwd: root,
      env: {
        PATH: `${root}/fixture-bin:${root}/bin:/bin:/usr/bin:${process.env.PATH}`,
        HOME: `${root}/home`,
        TMPDIR: `${root}/tmp`,
        INSTALLER: installer,
        ...env,
      },
      encoding: "utf8",
      timeout: 30_000,
    });
  return { root, run, archive, destination: `${root}/tools/${tool}/${versions[tool]}` };
}

for (const tool of ["rust", "uv"]) {
  test(`${tool} publishes the offline smoke renderer used by image acceptance`, () => {
    const result = spawnSync(
      "/bin/bash",
      ["-c", `source "$1"; ${tool}_smoke_script`, "_", installer],
      {
        encoding: "utf8",
        timeout: 10_000,
      },
    );
    assert.equal(result.status, 0, result.stderr);
    assert.match(result.stdout, new RegExp(`offline_${tool}_probe`));
    assert.equal(spawnSync("/bin/bash", ["-n"], { input: result.stdout }).status, 0);
  });
}

for (const tool of ["rust", "uv"]) {
  test(`${tool} installs verified archives, preserves user homes, and repeats offline`, (t) => {
    const { root, run, destination } = fixture(t, tool);
    for (const name of [".cargo", ".rustup", ".config"]) {
      fs.mkdirSync(path.join(root, "home", name));
      fs.writeFileSync(path.join(root, "home", name, "keep"), "operator state");
    }
    success(
      run(
        `install_rust_uv_toolchain ${tool}\ninstall_rust_uv_toolchain ${tool}\noffline_${tool}_probe`,
        {
          CARGO_TARGET_DIR: "/do-not-use",
          RUSTUP_HOME: "/do-not-use",
          PYTHONPATH: "/do-not-use",
        },
      ),
    );
    for (const name of commands[tool]) {
      assert.equal(fs.readlinkSync(`${root}/bin/${name}`), `${destination}/bin/${name}`);
      assert.equal(fs.statSync(`${destination}/bin/${name}`).mode & 0o777, 0o755);
    }
    for (const name of [".cargo", ".rustup", ".config"])
      assert.equal(fs.readFileSync(`${root}/home/${name}/keep`, "utf8"), "operator state");
    const calls = fs.readFileSync(`${root}/calls`, "utf8");
    assert.match(calls, tool === "rust" ? /cargo test --offline/ : /uv pip install --python/);
    assert.match(
      calls,
      tool === "rust"
        ? /rustdoc --test src\/main.rs/
        : /uvx --python .* --from .*\.whl crabbox-uv-smoke/,
    );
    assert.deepEqual(fs.readdirSync(`${root}/tmp`), []);
  });

  for (const name of commands[tool]) {
    test(`${tool} preserves conflicting public ${name} before downloading or altering its slot`, (t) => {
      const { root, run, destination, archive } = fixture(t, tool);
      fs.mkdirSync(destination, { recursive: true });
      fs.writeFileSync(`${destination}/keep`, "existing slot");
      fs.writeFileSync(`${root}/bin/${name}`, "operator tool");
      const before = fs.readFileSync(archive);
      const result = run(`if install_rust_uv_toolchain ${tool}; then exit 91; else exit "$?"; fi`);
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /public tool conflict/);
      assert.doesNotMatch(result.stderr, /unexpected-network/);
      assert.equal(fs.readFileSync(`${root}/bin/${name}`, "utf8"), "operator tool");
      assert.equal(fs.readFileSync(`${destination}/keep`, "utf8"), "existing slot");
      assert.deepEqual(fs.readFileSync(archive), before);
    });
  }

  test(`${tool} rejects corrupted, linked, and missing offline archives`, (t) => {
    const { root, run, archive } = fixture(t, tool);
    fs.writeFileSync(archive, "corrupt");
    const corrupt = run(`install_rust_uv_toolchain ${tool}`);
    assert.notEqual(corrupt.status, 0);
    assert.match(corrupt.stderr, /checksum mismatch/);
    assert.doesNotMatch(corrupt.stderr, /unexpected-network/);
    fs.unlinkSync(archive);
    fs.symlinkSync("missing", archive);
    const linked = run(`install_rust_uv_toolchain ${tool}`);
    assert.notEqual(linked.status, 0);
    assert.match(linked.stderr, /malformed Rust or uv archive/);
    fs.unlinkSync(archive);
    const missing = run(`offline_${tool}_probe`);
    assert.notEqual(missing.status, 0);
    assert.match(missing.stderr, /unavailable offline/);
    assert.doesNotMatch(missing.stderr, /unexpected-network/);
    assert.deepEqual(fs.readdirSync(`${root}/bin`), []);
    assert.deepEqual(fs.readdirSync(`${root}/tmp`), []);
  });

  test(`${tool} fails before publishing an authenticated wrong-version executable`, (t) => {
    const { root, run } = fixture(t, tool, { version: "0.0.1" });
    assert.notEqual(run(`install_rust_uv_toolchain ${tool}`).status, 0);
    assert.deepEqual(fs.readdirSync(`${root}/bin`), []);
    assert.deepEqual(fs.readdirSync(`${root}/tmp`), []);
  });

  test(`${tool} fails on symlinked destination before touching archives`, (t) => {
    const { root, run } = fixture(t, tool);
    fs.mkdirSync(`${root}/outside`);
    fs.symlinkSync(`${root}/outside`, `${root}/tools`);
    const result = run(`install_rust_uv_toolchain ${tool}`);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /symlinked developer toolchain directory/);
    assert.deepEqual(fs.readdirSync(`${root}/outside`), []);
    assert.deepEqual(fs.readdirSync(`${root}/bin`), []);
  });

  test(`${tool} smoke rejects root and propagates functional failure with cleanup`, (t) => {
    const failure = tool === "rust" ? "cargo:test" : "uv:venv";
    const { root, run } = fixture(t, tool, { failure });
    const install = run(`if install_rust_uv_toolchain ${tool}; then exit 91; else exit "$?"; fi`);
    assert.equal(install.status, 73, install.stderr);
    assert.deepEqual(fs.readdirSync(`${root}/bin`), []);
    const rootResult = run(`id() { echo 0; }; offline_${tool}_probe`);
    assert.notEqual(rootResult.status, 0);
    assert.match(rootResult.stderr, /nonroot/);
    assert.deepEqual(fs.readdirSync(`${root}/tmp`), []);
  });

  test(`${tool} supports only glibc Linux amd64 and is independent of Node selection`, (t) => {
    const { root, run } = fixture(t, tool);
    for (const predicate of [
      "uname() { echo Darwin; }",
      "dpkg() { echo arm64; }",
      "getconf() { echo musl; }",
    ]) {
      success(run(`${predicate}\ninstall_rust_uv_toolchain ${tool}`));
      assert.deepEqual(fs.readdirSync(`${root}/bin`), []);
    }
    success(run(`node_major=22\ninstall_rust_uv_toolchain ${tool}`));
  });
}
