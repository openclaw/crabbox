import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createHash } from "node:crypto";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import test from "node:test";

const installer = path.resolve(import.meta.dirname, "install-linux-developer-tools.sh");
const source = fs.readFileSync(installer, "utf8");
function fixture(t, bash = "bash") {
  const root = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "crabbox-rust-uv-")));
  t.after(() => fs.rmSync(root, { recursive: true, force: true }));
  for (const name of ["home", "tmp", "bin"]) fs.mkdirSync(path.join(root, name));
  const run = (body, env = {}) => {
    // Darwin ignores TMPDIR for bare mktemp -d. Keep disposable probes under
    // this fixture without changing explicit production publication templates.
    const result = spawnSync(bash, ["-c", `source "$INSTALLER"
mktemp() {
  if [[ "$#" == 1 && "$1" == -d ]]; then command mktemp -d "$TMPDIR/probe.XXXXXXXX"
  else command mktemp "$@"; fi
}
${body}`], {
      cwd: root,
      env: { PATH: process.env.PATH, HOME: path.join(root, "home"), TMPDIR: path.join(root, "tmp"),
        INSTALLER: installer, PYTHONDONTWRITEBYTECODE: "1", ...env },
      encoding: "utf8", timeout: 60_000,
    });
    assert.ifError(result.error);
    return result;
  };
  return { root, run };
}
function passed(result) {
  assert.equal(result.status, 0, result.stderr || result.stdout);
}

test("Rust and uv pins retain authenticated upstream URLs and unchanged Rust manifest bytes", (t) => {
  const { run } = fixture(t);
  const pins = [
    ["channel-rust-1.97.1.toml", "03569b1886ceb5c05276b50c8431ab111de944cd6140fe1fa7d821dd8e0f29cf"],
    ["rustup-init-1.29.0-x86_64-unknown-linux-gnu", "4acc9acc76d5079515b46346a485974457b5a79893cfb01112423c89aeb5aa10"],
    ["rustc-1.97.1-x86_64-unknown-linux-gnu.tar.xz", "9819d0a32d56bd339585319c80260e332779f5541fd66838ab7e016d6c814819"],
    ["cargo-1.97.1-x86_64-unknown-linux-gnu.tar.xz", "e1be5f5ff7f7f80ca506fb65770b759edbdc6d303781ed71c5de8ec8a8394779"],
    ["rust-std-1.97.1-x86_64-unknown-linux-gnu.tar.xz", "1c1e704ae80126b7de34f72ea2825f7fd01736dec20732faed47374b95282fba"],
    ["rustfmt-1.97.1-x86_64-unknown-linux-gnu.tar.xz", "907fe97d6afbde1eca1b34c992c76e1406d422e2e6f137813d382acec7eb4d14"],
    ["uv-0.12.11-x86_64-unknown-linux-gnu.tar.gz", "4ae93e0f148a18434cc094072547cec88912fc4a72b984183c7d0d0e9586cb5e"],
  ];
  for (const [name, digest] of pins) {
    const result = run(`toolchain_archive_spec ${name}`);
    passed(result);
    const [algorithm, actual, url] = result.stdout.trim().split(" ");
    assert.equal(algorithm, "sha256");
    assert.equal(actual, digest);
    assert.equal(new URL(url).protocol, "https:");
    assert.equal(new URL(url).search, "");
  }
  assert.match(source, /install -m 0644 "\$staging\/download\/\$name" "\$staging\/\$relative"/);
  assert.doesNotMatch(source, /RUSTUP_INIT_SKIP|RUSTUP_TOOLCHAIN=|rustup-init.*--offline/);
});

test("native runner Go seed binds the same versioned archive and digest as the installer", (t) => {
  const { run } = fixture(t);
  const result = run('toolchain_archive_spec "go$pinned_go_version.linux-amd64.tar.gz"');
  passed(result);
  const digest = result.stdout.trim().split(" ")[1];
  const actions = fs.readFileSync(path.resolve(import.meta.dirname, "../internal/cli/actions.go"), "utf8");
  assert.ok(actions.includes(`("go", "1.27.1", "go1.27.1.linux-amd64.tar.gz",\n             "${digest}")`));
});

const bashExecutables = [...new Set(["/bin/bash", ...(process.env.PATH ?? "").split(path.delimiter)
  .map((directory) => path.join(directory, "bash"))].filter((file) => fs.existsSync(file)).map((file) => fs.realpathSync(file)))];
for (const bash of bashExecutables) {
  const version = spawnSync(bash, ["-c", 'printf "%s" "$BASH_VERSION"'], { encoding: "utf8", timeout: 10_000 });
  assert.ifError(version.error);
  passed(version);
  for (const renderer of ["node_pnpm_smoke_script", "go_smoke_script", "bun_smoke_script", "rust_smoke_script", "uv_smoke_script"]) {
    test(`${renderer} retains serialized function syntax with Bash ${version.stdout}`, (t) => {
      const { run } = fixture(t, bash);
      const result = run(renderer);
      passed(result);
      const checked = spawnSync(bash, ["-n"], { input: result.stdout, encoding: "utf8", timeout: 10_000 });
      assert.ifError(checked.error);
      passed(checked);
    });
  }
  test(`serialized Go probe preserves heredoc failure with Bash ${version.stdout}`, (t) => {
    const { root, run } = fixture(t, bash);
    const bin = path.join(root, "go", "bin");
    fs.mkdirSync(bin, { recursive: true });
    fs.writeFileSync(path.join(bin, "go"), `#!/bin/sh
case "$1" in
  version) echo 'go version go1.27.1 linux/amd64' ;;
  env) printf 'linux\\namd64\\n' ;;
  *) echo unexpected-go-execution >&2; exit 92 ;;
esac
`, { mode: 0o755 });
    fs.writeFileSync(path.join(bin, "gofmt"), "#!/bin/sh\necho unexpected-gofmt >&2\nexit 93\n", { mode: 0o755 });
    const result = run(`
definition="$(declare -f check_go_toolchain)"
unset -f check_go_toolchain
eval "$definition"
cat() { return 47; }
if check_go_toolchain "$PWD/go" "$PWD/check"; then exit 94; else exit $?; fi
`);
    assert.equal(result.status, 47, result.stderr);
    assert.doesNotMatch(result.stderr, /unexpected-/);
    assert.equal(fs.existsSync(path.join(root, "check", "formatted.go")), false);
  });
}

for (const [sudo, container, account, expected] of [
  ["alice", "bob", "alice:x:1000:1000::/home/alice:/bin/bash", "alice /home/alice"],
  ["", "bob", "bob:x:1001:1001::/home/bob:/bin/bash", "bob /home/bob"],
  ["", "", "", null],
  ["root", "bob", "root:x:0:0::/root:/bin/bash", null],
  ["alice", "", "mallory:x:1000:1000::/home/mallory:/bin/bash", null],
]) {
  test(`Rust resolves the explicit runtime identity: sudo=${sudo || "absent"} container=${container || "absent"} account=${account.split(":")[0]}`, (t) => {
    const { run } = fixture(t);
    const result = run(`
getent() { printf '%s\\n' "$ACCOUNT"; }
resolve_rust_runtime_user || exit $?
printf '%s %s\\n' "$runtime_user" "$runtime_home"
`, { SUDO_USER: sudo, CRABBOX_SSH_USER: container, ACCOUNT: account });
    if (expected) {
      passed(result);
      assert.equal(result.stdout.trim(), expected);
    } else assert.notEqual(result.status, 0);
  });
}

for (const fail of ["", "preflight", "seed", "initializer", "install", "probe", "publish"]) {
  test(`Rust installation preserves conditional failure and provenance ordering: ${fail || "success"}`, (t) => {
    const { run } = fixture(t);
    const result = run(`
linux_x64_supported() { return 0; }
resolve_rust_runtime_user() { runtime_user=alice; runtime_home=/home/alice; }
rust_user_state() {
  echo "state-$1" >&2
  [[ "$FAIL" != preflight || "$1" != check ]] || return 41
  [[ "$FAIL" != publish || "$1" != publish ]] || return 46
  [[ "$1" != check ]] || echo fresh
}
prepare_rust_seed() { echo seed >&2; [[ "$FAIL" != seed ]] || return 42; }
run_rust_runtime_user() {
  echo "user-command:$*" >&2
  [[ "$FAIL" != initializer || "$*" != *" -y "* ]] || return 43
  [[ "$FAIL" != install || "$*" != *"toolchain install stable"* ]] || return 44
  [[ "$FAIL" != probe || "$*" != *"probe-fixture"* ]] || return 45
}
rust_smoke_script() { echo probe-fixture; }
if install_rust; then echo success; else exit $?; fi
`, { FAIL: fail });
    assert.equal(result.status, fail ? { preflight: 41, seed: 42, initializer: 43, install: 44, probe: 45, publish: 46 }[fail] : 0, result.stderr);
    if (fail && fail !== "publish") assert.doesNotMatch(result.stderr, /state-publish/);
    if (!fail) {
      assert.match(result.stderr, /toolchain install stable --profile minimal --component rustfmt --no-self-update/);
      assert.ok(result.stderr.indexOf("state-publish") > result.stderr.indexOf("probe-fixture"));
    }
  });
}

test("Rust repeated recipe-owned installation verifies without invoking initialization", (t) => {
  const { run } = fixture(t);
  const result = run(`
linux_x64_supported() { return 0; }
resolve_rust_runtime_user() { runtime_user=alice; runtime_home=/home/alice; }
rust_user_state() { echo reuse; }
prepare_rust_seed() { :; }
rust_smoke_script() { echo probe-fixture; }
run_rust_runtime_user() { [[ "$*" == *probe-fixture* ]] || return 79; }
install_rust
`);
  passed(result);
});

for (const phase of ["zsh startup", "seed preparation"]) {
  for (const state of ["unknown", "recipe-owned"]) {
    test(`Rust rechecks fresh state after ${phase} and preserves ${state} drift`, (t) => {
      const { root, run } = fixture(t);
      const result = run(`
linux_x64_supported() { return 0; }
resolve_rust_runtime_user() { runtime_user=alice; runtime_home="$HOME"; }
rust_user_state() {
  printf 'state-%s\\n' "$1" >>"$HOME/checks"
  [[ "$1" == check ]] || return 92
  if [[ -e "$HOME/.cargo" ]]; then
    [[ "$DRIFT_STATE" == recipe-owned ]] || return 41
    echo reuse
  else echo fresh; fi
}
drift() {
  mkdir "$HOME/.cargo"
  printf 'operator state\\n' >"$HOME/.cargo/keep"
}
check_rust_zsh_directory() { ${phase === "zsh startup" ? "drift" : ":"}; }
prepare_rust_seed() { ${phase === "seed preparation" ? "drift" : ":"}; }
run_rust_runtime_user() { echo unexpected-initialization >&2; return 93; }
rust_smoke_script() { echo unexpected-probe >&2; return 94; }
if install_rust; then exit 95; else exit $?; fi
`, { DRIFT_STATE: state });
      assert.equal(result.status, state === "unknown" ? 41 : 1, result.stderr);
      assert.doesNotMatch(result.stderr, /unexpected-initialization|unexpected-probe/);
      assert.equal(fs.readFileSync(path.join(root, "home", "checks"), "utf8"), "state-check\nstate-check\n");
      assert.deepEqual(fs.readdirSync(path.join(root, "home", ".cargo")), ["keep"]);
      assert.equal(fs.readFileSync(path.join(root, "home", ".cargo", "keep"), "utf8"), "operator state\n");
      assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
    });
  }
}

test("Rust privilege drop uses an empty environment and a neutral directory", (t) => {
  const { run } = fixture(t);
  const result = run(`
runtime_user=alice runtime_home=/home/alice
runuser() { printf 'cwd=%s\\n' "$PWD"; printf '%s\\n' "$@"; }
run_rust_runtime_user /bin/bash -lc true
`, { RUSTUP_CI: "hostile", RUSTUP_TOOLCHAIN: "nightly", CARGO_HOME: "/unrelated",
      BASH_ENV: "", HTTPS_PROXY: "http://example.invalid" });
  passed(result);
  assert.match(result.stdout, /^cwd=\/\n-u\nalice\n--\nenv\n-i\n/);
  assert.match(result.stdout, /HOME=\/home\/alice/);
  assert.match(result.stdout, /RUSTUP_DIST_SERVER=file:\/\/\/opt\/crabbox\/rust\/1\.97\.1/);
  assert.doesNotMatch(result.stdout, /hostile|nightly|unrelated|HTTPS_PROXY|RUSTUP_CI|BASH_ENV|CARGO_HOME/);
  const reporting = source.slice(source.indexOf("print_versions()"), source.indexOf("install_node_only()"));
  assert.match(reporting, /run_rust_runtime_user env RUSTUP_AUTO_INSTALL=0 \/bin\/bash -lc/);
  assert.match(reporting, /'set -e; cd \/; rustup --version; rustc --version; cargo --version; rustfmt --version'/);
});

for (const variant of ["home", "owned directory", "outside home", "symlinked directory"]) {
  test(`Rustup ZDOTDIR preflight preserves startup paths: ${variant}`, (t) => {
    const { root, run } = fixture(t);
    const home = path.join(root, "home");
    const nested = path.join(home, "shell");
    fs.mkdirSync(nested);
    fs.writeFileSync(path.join(nested, ".zshenv"), "# owned shell fixture\n");
    let selected = variant === "home" ? home : nested;
    if (variant === "outside home") selected = root;
    if (variant === "symlinked directory") {
      selected = path.join(home, "linked");
      fs.symlinkSync(nested, selected);
    }
    fs.writeFileSync(path.join(root, "bin", "zsh"),
      `#!/bin/sh\nprintf '%s' ${JSON.stringify(selected)}\n`, { mode: 0o755 });
    const result = run('run_rust_runtime_user() { "$@"; }\ncheck_rust_zsh_directory', {
      PATH: `${path.join(root, "bin")}${path.delimiter}${process.env.PATH}`,
    });
    assert.equal(result.status === 0, variant === "home" || variant === "owned directory", result.stderr);
    assert.equal(fs.readFileSync(path.join(nested, ".zshenv"), "utf8"), "# owned shell fixture\n");
    assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
  });
}

function rustStateFixture(t) {
  const value = fixture(t);
  const { root } = value;
  const home = path.join(root, "home");
  const record = path.join(root, "root-state", "runtime-user.json");
  fs.mkdirSync(path.dirname(record), { mode: 0o755 });
  const systemRoot = path.join(root, "system");
  // Model host ancestry and system-tool locations, not fixture-owned state.
  // Real fixture files retain their modes, links, contents and inodes.
  const program = source.match(/rust_user_state\(\) \{[\s\S]*?<<'PY'\n([\s\S]*?)\nPY\n\}/)[1];
  const driver = `
import os, pathlib, pwd, stat, sys, types
fixture_home = os.environ["FIXTURE_HOME"]
fixture_root = pathlib.Path(fixture_home).parent
host_ancestors = set(fixture_root.parents)
system_root = pathlib.Path(os.environ["SYSTEM_ROOT"])
uid = os.getuid() or 1000
pwd.getpwnam = lambda user: types.SimpleNamespace(pw_uid=uid, pw_dir=fixture_home)
# Path.lstat calls Path.stat on newer Python; model each OS result only once.
original_stat, original_lstat = os.stat, os.lstat
original_lexists = os.path.lexists
def isolated_lexists(entry):
    entry = pathlib.Path(entry)
    if str(entry.parent) in ("/usr/local/bin", "/usr/bin", "/bin") and entry.name in ("rustup", "rustc", "cargo", "rustfmt"):
        entry = system_root / str(entry).lstrip("/")
    return original_lexists(entry)
os.path.lexists = isolated_lexists
def modeled(function, entry, *args, **kwargs):
    value = list(function(entry, *args, **kwargs))
    if entry in host_ancestors:
        value[0] = stat.S_IFDIR | 0o755
    if not str(entry).startswith(fixture_home):
        value[4] = 0
    else:
        value[4] = uid
    value[2] += int(os.environ.get("REMOUNT", "0"))
    if os.environ.get("FOREIGN_FS") and str(entry) == fixture_home + "/.cargo":
        value[2] += 1
    return os.stat_result(value)
pathlib.Path.stat = lambda entry, *args, **kwargs: modeled(original_stat, entry, *args, **kwargs)
pathlib.Path.lstat = lambda entry, *args, **kwargs: modeled(original_lstat, entry, *args, **kwargs)
exec(${JSON.stringify(program)})
`;
  const state = (action, env = {}) => {
    const result = spawnSync("python3", ["-c", driver, action, "alice", home, record, "1.97.1"], {
      env: { PATH: process.env.PATH, PYTHONDONTWRITEBYTECODE: "1", FIXTURE_HOME: home,
        SYSTEM_ROOT: systemRoot, ...env },
      encoding: "utf8", timeout: 10_000,
    });
    assert.ifError(result.error);
    return result;
  };
  return { ...value, home, record, state, systemRoot };
}

for (const directory of ["usr/local/bin", "usr/bin", "bin"]) {
  for (const tool of ["rustup", "rustc", "cargo", "rustfmt"]) {
    test(`Rust rejects an existing system tool without modifying it: ${directory}/${tool}`, (t) => {
      const { state, record, systemRoot } = rustStateFixture(t);
      const file = path.join(systemRoot, directory, tool);
      fs.mkdirSync(path.dirname(file), { recursive: true });
      fs.writeFileSync(file, "operator tool\n");
      assert.notEqual(state("check").status, 0);
      assert.equal(fs.readFileSync(file, "utf8"), "operator tool\n");
      assert.equal(fs.existsSync(record), false);
    });
  }
}

test("Rust rejects writable fixture-owned record ancestry", (t) => {
  const { state, record } = rustStateFixture(t);
  fs.chmodSync(path.dirname(record), 0o777);
  assert.notEqual(state("check").status, 0);
  assert.equal(fs.existsSync(record), false);
});

for (const conflict of [".cargo", ".rustup", "symlink-profile", "writable-profile"]) {
  test(`Rust leaves unknown runtime state unchanged: ${conflict}`, (t) => {
    const { home, state, record } = rustStateFixture(t);
    if (conflict.startsWith(".")) fs.mkdirSync(path.join(home, conflict));
    else {
      fs.writeFileSync(path.join(home, "private-profile"), "private-fixture\n");
      if (conflict === "symlink-profile") fs.symlinkSync("private-profile", path.join(home, ".profile"));
      else {
        fs.writeFileSync(path.join(home, ".profile"), "private-fixture\n");
        fs.chmodSync(path.join(home, ".profile"), 0o666);
      }
    }
    const before = fs.readdirSync(home);
    const result = state("check");
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /unknown existing Cargo\/Rustup state/);
    assert.deepEqual(fs.readdirSync(home), before);
    assert.equal(fs.existsSync(record), false);
  });
}

test("Rust provenance survives a remount but rejects replaced or foreign-filesystem state", (t) => {
  const { home, state, record } = rustStateFixture(t);
  fs.writeFileSync(path.join(home, ".profile"), "# managed image PATH\n", { mode: 0o644 });
  passed(state("check"));
  for (const name of [".cargo", ".rustup"]) fs.mkdirSync(path.join(home, name), { mode: 0o755 });
  passed(state("publish"));
  const recorded = fs.readFileSync(record);
  assert.doesNotMatch(recorded.toString(), /st_dev|device/);
  passed(state("check", { REMOUNT: "100" }));
  assert.notEqual(state("check", { FOREIGN_FS: "1" }).status, 0);
  fs.renameSync(path.join(home, ".cargo"), path.join(home, "original-cargo"));
  fs.mkdirSync(path.join(home, ".cargo"));
  assert.notEqual(state("check").status, 0);
  assert.deepEqual(fs.readFileSync(record), recorded);
});

for (const fail of ["", "download", "version", "publish"]) {
  test(`uv authenticated installation validates both aliases and preserves failures: ${fail || "success"}`, (t) => {
    const { root, run } = fixture(t);
    const payload = path.join(root, "uv-x86_64-unknown-linux-gnu");
    fs.mkdirSync(payload);
    for (const tool of ["uv", "uvx"]) fs.writeFileSync(path.join(payload, tool),
      `#!/bin/sh\nprintf '${tool} ${fail === "version" ? "0.0.0" : "0.12.11"}\\n'\n`, { mode: 0o755 });
    const name = "uv-0.12.11-x86_64-unknown-linux-gnu.tar.gz";
    passed(spawnSync("tar", ["-czf", path.join(root, name), "-C", root, path.basename(payload)], { encoding: "utf8" }));
    const hash = createHash("sha256").update(fs.readFileSync(path.join(root, name))).digest("hex");
    const result = run(`
linux_x64_supported() { return 0; }
check_root_owned_path() { :; }
public_toolchain_archive_dir="$PWD"
uv_toolchain_root="$PWD/tools/uv"
uv_bin_dir="$PWD/bin"
toolchain_archive_spec() { printf 'sha256 ${hash} https://example.invalid/archive\\n'; }
cache_public_toolchain_archives() { [[ "$FAIL" != download ]] || return 22; }
${fail === "publish" ? 'public_tool_links() { [[ "$1" != publish ]] || return 49; }' : ""}
if install_uv; then echo success; else exit $?; fi
`, { FAIL: fail });
    assert.equal(result.status, { "": 0, download: 22, version: 1, publish: 49 }[fail], result.stderr);
    assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
    if (!fail) {
      for (const tool of ["uv", "uvx"]) assert.equal(fs.readlinkSync(path.join(root, "bin", tool)),
        path.join(root, "tools", "uv", "0.12.11", tool));
    } else assert.equal(fs.existsSync(path.join(root, "bin", "uv")), false);
  });
}

test("uv wheel proof uses the distro backend and isolated offline system-Python environments", () => {
  const probe = source.slice(source.indexOf("offline_uv_probe()"), source.indexOf("uv_smoke_script()"));
  assert.match(probe, /cd "\$staging" \|\| return \$\?\n  "\$\{offline_env\[@\]\}" \/usr\/bin\/python3 -I -B -m build --wheel --no-isolation/);
  assert.match(probe, /env -i "HOME=\$staging\/home"/);
  assert.match(probe, /--offline --no-config --no-python-downloads venv --python \/usr\/bin\/python3/);
  assert.match(probe, /uvx" --offline --no-config --no-python-downloads --python \/usr\/bin\/python3 --from/);
  assert.doesNotMatch(probe, /pip install (build|setuptools|wheel)|--python-preference managed/);
});

test("uv wheel building ignores an ambient shadow build.py and cleans its owned staging", (t) => {
  const { root, run } = fixture(t);
  const marker = path.join(root, "shadow-executed");
  const shadow = `open(${JSON.stringify(marker)}, "w").write("ambient module executed")\n`;
  fs.writeFileSync(path.join(root, "build.py"), shadow);
  for (const tool of ["uv", "uvx"]) fs.writeFileSync(path.join(root, "bin", tool),
    `#!/bin/sh\nif [ "$1" = --version ]; then echo '${tool} 0.12.11'; else exit 73; fi\n`, { mode: 0o755 });
  const cwdLog = path.join(root, "build-cwd");
  const result = run(`
id() { echo 1000; }
stage_toolchain_archive() { :; }
tar() { :; }
env() {
  if [[ "$*" == *"/usr/bin/python3 "* ]]; then printf '%s\\n' "$PWD" >"$BUILD_CWD"; fi
  command env "$@"
}
offline_uv_probe
`, { PATH: `${path.join(root, "bin")}${path.delimiter}${process.env.PATH}`, BUILD_CWD: cwdLog });
  // A host without distro build fails at import; otherwise the fake uv stops
  // after the real isolated wheel build. Neither path may execute the shadow.
  assert.notEqual(result.status, 0, result.stdout);
  assert.equal(fs.existsSync(marker), false, "ambient build.py was imported");
  const buildCwd = fs.readFileSync(cwdLog, "utf8").trim();
  assert.equal(path.dirname(buildCwd), path.join(root, "tmp"));
  assert.equal(fs.existsSync(buildCwd), false);
  assert.equal(fs.readFileSync(path.join(root, "build.py"), "utf8"), shadow);
  assert.equal(fs.existsSync(path.join(root, "__pycache__")), false);
  assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
});

test("Rust missing baked stable fails without hydrating or changing runtime state", { skip: process.getuid?.() === 0 }, (t) => {
  const { root, run } = fixture(t);
  const bin = path.join(root, "home", ".cargo", "bin");
  fs.mkdirSync(bin, { recursive: true });
  for (const tool of ["rustup", "cargo", "rustc", "rustfmt"]) fs.writeFileSync(path.join(bin, tool),
    '#!/bin/sh\necho unexpected-tool-execution >&2\nexit 91\n', { mode: 0o755 });
  fs.mkdirSync(path.join(root, "home", ".rustup"));
  fs.writeFileSync(path.join(root, "home", ".rustup", "settings.toml"), "# fixture\n");
  const result = run(`check_rust_seed() { :; }\nrust_runtime_probe`, {
    PATH: `${bin}${path.delimiter}${process.env.PATH}`,
  });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /baked stable Rust toolchain is missing/);
  assert.doesNotMatch(result.stderr, /unexpected-tool-execution/);
  assert.deepEqual(fs.readdirSync(path.join(root, "home", ".rustup")), ["settings.toml"]);
  assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
  const probe = source.slice(source.indexOf("rust_runtime_probe()"), source.indexOf("install_rust()"));
  assert.match(probe, /CARGO_NET_OFFLINE=true RUSTUP_AUTO_INSTALL=0/);
});

for (const fail of ["", "rustfmt", "check", "test", "run", "install"]) {
  test(`Rust offline functional bundle uses empty owned caches and cleans on ${fail || "success"}`, { skip: process.getuid?.() === 0 }, (t) => {
    const { root, run } = fixture(t);
    const home = path.join(root, "home");
    const bin = path.join(home, ".cargo", "bin");
    fs.mkdirSync(bin, { recursive: true });
    fs.mkdirSync(path.join(home, ".rustup", "toolchains", "stable-x86_64-unknown-linux-gnu"), { recursive: true });
    fs.writeFileSync(path.join(home, ".rustup", "settings.toml"), "# fixture\n");
    const events = path.join(root, "events");
    for (const tool of ["rustup", "rustc", "cargo", "rustfmt"]) {
      fs.writeFileSync(path.join(bin, tool), `#!/bin/bash
set -euo pipefail
[[ "$RUSTUP_AUTO_INSTALL" == 0 && "$CARGO_NET_OFFLINE" == true ]]
[[ "$CARGO_HOME" == "$TMPDIR/cargo" && "$CARGO_TARGET_DIR" == "$TMPDIR/target" ]]
[[ -z "\${RUSTUP_TOOLCHAIN+x}" && -z "\${HTTPS_PROXY+x}" ]]
printf '${tool} %s\\n' "$*" >>${JSON.stringify(events)}
[[ "${fail}" != "${tool}" ]] || exit 73
if [[ "${tool}" == rustc ]]; then echo 'rustc 1.97.1 (fixture)'; exit 0; fi
if [[ "${tool}" == cargo ]]; then
  [[ "\${1:-}" != +stable ]] || shift
  [[ "${fail}" != "$1" ]] || exit 73
  case "$1" in
    generate-lockfile) [[ ! -e "$CARGO_HOME/registry" ]]; printf '# fixture\\n' >Cargo.lock ;;
    check|test|run) [[ "$*" == *"--offline --locked"* ]]; [[ "$1" != run ]] || echo rust-offline-ok ;;
    fmt) [[ "$*" == "fmt --check" ]] ;;
    install)
      [[ "$*" == "install --offline --locked --path ." ]]
      mkdir -p "$CARGO_HOME/bin"
      printf '#!/bin/sh\\necho rust-offline-ok\\n' >"$CARGO_HOME/bin/crabbox-offline-rust"
      chmod +x "$CARGO_HOME/bin/crabbox-offline-rust" ;;
    *) exit 74 ;;
  esac
fi
`, { mode: 0o755 });
    }
    const result = run("check_rust_seed() { :; }\nrust_runtime_probe", {
      PATH: `${bin}${path.delimiter}${process.env.PATH}`,
      RUSTUP_TOOLCHAIN: "unrelated", HTTPS_PROXY: "http://example.invalid",
    });
    assert.equal(result.status, fail ? 73 : 0, result.stderr);
    assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
    assert.deepEqual(fs.readdirSync(path.join(home, ".cargo")), ["bin"]);
    if (!fail) {
      const log = fs.readFileSync(events, "utf8");
      assert.match(log, /cargo \+stable check --offline --locked/);
      assert.match(log, /cargo \+stable test --offline --locked/);
      assert.match(log, /cargo \+stable run --offline --locked --quiet/);
      assert.match(log, /cargo install --offline --locked --path \./);
    }
  });
}

for (const state of ["valid", "missing", "corrupt"]) {
  test(`Rust public seed checking is nonwriting and fails closed: ${state}`, (t) => {
    const { root, run } = fixture(t);
    const seed = path.join(root, "seed");
    fs.mkdirSync(path.join(seed, "dist", "2026-07-16"), { recursive: true });
    const names = [
      "channel-rust-1.97.1.toml", "rustup-init-1.29.0-x86_64-unknown-linux-gnu",
      ...["rustc", "cargo", "rust-std", "rustfmt"].map((name) => `${name}-1.97.1-x86_64-unknown-linux-gnu.tar.xz`),
    ];
    const cases = [];
    const files = [];
    for (const name of names) {
      const file = name.startsWith("channel") ? path.join(seed, "dist", "channel-rust-stable.toml")
        : name.startsWith("rustup-init") ? path.join(seed, "rustup-init") : path.join(seed, "dist", "2026-07-16", name);
      fs.writeFileSync(file, name);
      files.push(file);
      const hash = createHash("sha256").update(name).digest("hex");
      cases.push(`${name}) echo 'sha256 ${hash} https://example.invalid/fixture' ;;`);
    }
    fs.writeFileSync(path.join(seed, "dist", "channel-rust-stable.toml.sha256"),
      "03569b1886ceb5c05276b50c8431ab111de944cd6140fe1fa7d821dd8e0f29cf  channel-rust-stable.toml\n");
    if (state === "missing") fs.unlinkSync(files.at(-1));
    if (state === "corrupt") fs.writeFileSync(files.at(-1), "corrupt");
    const before = files.map((file) => fs.existsSync(file) ? fs.readFileSync(file).toString("hex") : null);
    const result = run(`
rust_seed_root="$PWD/seed"
check_root_owned_path() { :; }
toolchain_archive_spec() { case "$1" in ${cases.join("\n")} *) return 1 ;; esac; }
curl() { echo unexpected-network >&2; return 90; }
check_rust_seed
`);
    assert.equal(result.status === 0, state === "valid", result.stderr);
    assert.doesNotMatch(result.stderr, /unexpected-network/);
    assert.deepEqual(files.map((file) => fs.existsSync(file) ? fs.readFileSync(file).toString("hex") : null), before);
    assert.deepEqual(fs.readdirSync(path.join(root, "tmp")), []);
  });
}
