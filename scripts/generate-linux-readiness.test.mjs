import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmod, cp, lstat, mkdir, mkdtemp, readFile, readdir, rm, stat, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import {
  assertInheritance,
  assertFrozenContract,
  assertProfile,
  canonicalJSON,
  digest,
  goSource,
  loadRecipes,
  manifestFor,
  minimalBootstrap,
  parseStrictJSON,
  producerSource,
  tsSource,
  validateJSONSchema,
  validateSchemaDefinition,
} from "./generate-linux-readiness.mjs";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const packageManagers = ["apt", "apt-get", "apt-cache", "dpkg", "dpkg-query"];
const fixtureRecipes = loadRecipes();

function quote(value) {
  return `'${String(value).replaceAll("'", "'\"'\"'")}'`;
}

async function executable(path, source) {
  await writeFile(path, `#!/usr/bin/env bash\nset -eu\n${source}\n`);
  await chmod(path, 0o755);
}

async function createFixture(t) {
  const { minimal, builder } = await fixtureRecipes;
  const root = await mkdtemp(join(tmpdir(), "crabbox-linux-readiness-"));
  t.after(async () => rm(root, { recursive: true, force: true }));
  const bin = join(root, "bin");
  const state = join(root, "state");
  const readiness = join(root, "readiness");
  const manifest = join(readiness, "linux.json");
  const marker = join(state, "image-ready");
  const ca = join(root, "ca.crt");
  const sshd = join(root, "sshd");
  const aptConfig = join(root, "apt.conf");
  const packageLog = join(root, "packages.log");
  await mkdir(bin);
  await mkdir(state, { mode: 0o755 });
  await mkdir(readiness, { mode: 0o755 });
  await writeFile(ca, "test-ca\n");
  await executable(sshd, "exit 0");
  const commands = new Set(builder.probes.filter((probe) => !probe.command.startsWith("test ")).map((probe) => probe.command.split(" ")[0]));
  for (const command of commands) {
    await executable(
      join(bin, command),
      `probe=${quote(command)}
if [[ -n "\${CRABBOX_FIXTURE_EXPECT_LC_ALL:-}" && "\${LC_ALL:-}" != "$CRABBOX_FIXTURE_EXPECT_LC_ALL" ]]; then exit 92; fi
if [[ -n "\${CRABBOX_FIXTURE_EXPECT_LANG:-}" && "\${LANG:-}" != "$CRABBOX_FIXTURE_EXPECT_LANG" ]]; then exit 93; fi
if [[ "$probe" == git && "\${1:-}" == lfs ]]; then probe=git-lfs; fi
if [[ "$probe" == python3 && "\${1:-}" == -c ]]; then
  probe=python3-venv
  if [[ "\${CRABBOX_FIXTURE_REAL_VENV:-0}" == 1 ]]; then
    printf '%s\\n' "$2" >>"$CRABBOX_FIXTURE_ROOT/venv-probes.log"
    exec "$CRABBOX_FIXTURE_PYTHON" "$@"
  fi
fi
if [[ -f "$CRABBOX_FIXTURE_ROOT/disabled-$probe" ]]; then exit 91; fi
printf '%s fixture\\n' "$probe"`,
    );
  }
  for (const command of packageManagers) {
    await executable(
      join(bin, command),
      `printf '%s %s\\n' ${quote(command)} "$*" >>"$CRABBOX_PACKAGE_LOG"
if [[ "\${CRABBOX_APT_SUCCESS:-0}" == 1 && ${quote(command)} == apt-get ]]; then
  if [[ "\${1:-}" == install ]]; then
    rm -f "$CRABBOX_FIXTURE_ROOT"/disabled-*
  fi
  exit 0
fi
exit 97`,
    );
  }
  await executable(
    join(bin, "mv"),
    `[[ "\${CRABBOX_FAIL_MV:-0}" != 1 ]] || exit 92
if [[ "\${CRABBOX_FAIL_MARKER_MV:-0}" == 1 && "\${1:-}" == -fT ]]; then exit 93; fi
if [[ "$(uname -s)" == Darwin && "\${1:-}" == -fT ]]; then shift; set -- -f "$@"; fi
exec /bin/mv "$@"`,
  );
  const ownerUID = String(process.getuid?.() ?? 0);
  // BSD temporary directories inherit their parent's group, not the process GID.
  const ownerGID = String((await stat(root)).gid);
  const options = {
    manifestPath: manifest,
    legacyMarkerPath: marker,
    sshdPath: sshd,
    caPath: ca,
    aptConfigPath: aptConfig,
    trustRoot: root,
    ownerUID,
    ownerGID,
    performChown: false,
    systemPath: `${bin}:/usr/bin:/bin`,
  };
  const shell = minimalBootstrap(minimal, builder, options);
  async function writeManifest(profile = "linux-minimal", bytes) {
    await mkdir(readiness, { recursive: true, mode: 0o755 });
    const value = bytes ?? `${canonicalJSON(manifestFor(profile, digest(profile === "linux-minimal" ? minimal : builder)))}\n`;
    await writeFile(manifest, value, { mode: 0o644 });
    await chmod(manifest, 0o644);
  }
  async function writeMarker() {
    await writeFile(marker, "crabbox-devtools-v1\n", { mode: 0o644 });
    await chmod(marker, 0o644);
  }
  async function packageCalls() {
    try {
      return (await readFile(packageLog, "utf8")).trim().split("\n").filter(Boolean);
    } catch (error) {
      if (error?.code === "ENOENT") return [];
      throw error;
    }
  }
  function run(source = shell, overrides = {}) {
    const nativeStat = process.platform === "darwin"
      ? `if [[ "\${2:-}" == %d ]]; then command /usr/bin/stat -f '%d' "$file"; return; fi
  output="$(command /usr/bin/stat -f '%HT:%u:%g:%Lp:%z' "$file")" || return 1
  output="\${output/Regular File:/regular file:}"
  output="\${output/Directory:/directory:}"
  output="\${output/Symbolic Link:/symbolic link:}"`
      : `if [[ "\${2:-}" == %d ]]; then command /usr/bin/stat "$@"; return; fi
  output="$(command /usr/bin/stat "$@")" || return 1`;
    const helpers = `retry() { "$@"; }
sync() { return 0; }
stat() {
  [[ "\${CRABBOX_STAT_FAIL:-0}" != 1 ]] || return 88
  local file="\${@: -1}" output kind owner group mode size
  if [[ "\${2:-}" == %d && "$file" == "\${CRABBOX_STAT_DEVICE_OVERRIDE_PATH:-}" ]]; then
    printf '%s\\n' "\${CRABBOX_STAT_DEVICE:-999999}"
    return
  fi
  ${nativeStat}
  if [[ "\${CRABBOX_STAT_TRANSLATE_LOCALE:-0}" == 1 && "\${2:-}" == '%F:%u:%g:%a:%s' && "\${LC_ALL:-}" != C ]]; then
    output="\${output/directory:/Verzeichnis:}"
    output="\${output/regular empty file:/leere regulaere Datei:}"
    output="\${output/regular file:/regulaere Datei:}"
  fi
  if [[ "$file" == "\${CRABBOX_STAT_OVERRIDE_PATH:-}" ]]; then
    IFS=: read -r kind owner group mode size <<<"$output"
    owner="\${CRABBOX_STAT_OWNER:-$owner}"
    group="\${CRABBOX_STAT_GROUP:-$group}"
    mode="\${CRABBOX_STAT_MODE:-$mode}"
    output="$kind:$owner:$group:$mode:$size"
  fi
  printf '%s\\n' "$output"
}`;
    const shellFlag = overrides.CRABBOX_FIXTURE_TRACE === "1" ? "-xc" : "-c";
    return spawnSync("bash", [shellFlag, `set -euo pipefail\n${helpers}\n${source}`], {
      cwd: repoRoot,
      encoding: "utf8",
      env: {
        ...process.env,
        ...overrides,
        CRABBOX_FIXTURE_ROOT: root,
        CRABBOX_PACKAGE_LOG: packageLog,
        PATH: overrides.PATH ?? `${bin}:${process.env.PATH}`,
      },
    });
  }
  function actualGenerated(source) {
    return source
      .replaceAll("/var/lib/crabbox-readiness/linux.json", manifest)
      .replaceAll("/var/lib/crabbox/image-ready", marker)
      .replaceAll("/usr/sbin/sshd", sshd)
      .replaceAll("/etc/ssl/certs/ca-certificates.crt", ca)
      .replaceAll("/etc/apt/apt.conf.d/80-crabbox-retries", aptConfig)
      .replace("crabbox_readiness_trust_root='/'", `crabbox_readiness_trust_root=${quote(root)}`)
      .replace("crabbox_readiness_owner_uid='0'", `crabbox_readiness_owner_uid=${quote(ownerUID)}`)
      .replace("crabbox_readiness_owner_gid='0'", `crabbox_readiness_owner_gid=${quote(ownerGID)}`)
      .replace(
        "crabbox_readiness_system_path='/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'",
        `crabbox_readiness_system_path=${quote(options.systemPath)}`,
      )
      .replaceAll("chown '0:0' \"$crabbox_directory\"", "true")
      .replaceAll("chown '0:0' \"$crabbox_marker_parent\"", "true")
      .replaceAll("chown '0:0' \"$crabbox_temp_path\"", "true");
  }
  function runProducer(overrides = {}) {
    const producer = producerSource(minimal, builder, options);
    return run(producer.slice(producer.indexOf("crabbox_readiness_manifest_path=")), overrides);
  }
  return { root, bin, state, readiness, manifest, marker, ca, sshd, aptConfig, packageLog, minimal, builder, ownerUID, ownerGID, options, shell, writeManifest, writeMarker, packageCalls, run, runProducer, actualGenerated };
}

test("strict profile and manifest schemas validate recipes and both generated manifests", async () => {
  const { profileSchema, manifestSchema, minimal, builder } = await fixtureRecipes;
  assertProfile(minimal, "linux-minimal", profileSchema);
  assertProfile(builder, "linux-builder", profileSchema);
  validateJSONSchema(manifestFor("linux-minimal", digest(minimal)), manifestSchema);
  validateJSONSchema(manifestFor("linux-builder", digest(builder)), manifestSchema);
  assert.deepEqual(minimal.aptPackages, ["ca-certificates", "curl", "git", "jq", "openssh-server", "rsync", "tmux", "util-linux"]);
  assert.deepEqual(builder.aptPackages.filter((name) => !minimal.aptPackages.includes(name)), ["build-essential", "git-lfs", "pkg-config", "python3", "python3-venv"]);
  assert.doesNotMatch(JSON.stringify({ minimal, builder }), /go\.mod|\.node-version|go version|node --version|Crabbox development/u);
  const unexpectedPackage = structuredClone(minimal);
  unexpectedPackage.aptPackages.push("z-extra");
  assert.throws(() => assertFrozenContract(unexpectedPackage, builder), /frozen v1 baseline packages/u);
  const unexpectedBuilder = structuredClone(builder);
  unexpectedBuilder.aptPackages.push("z-extra");
  assert.throws(() => assertFrozenContract(minimal, unexpectedBuilder), /frozen v1 generic builder packages/u);
  const changedProbe = structuredClone(minimal);
  changedProbe.probes.find((probe) => probe.name === "curl").command = "curl --help";
  assert.throws(() => assertFrozenContract(changedProbe, builder), /exact v1 curl probe/u);
  const weakenedVenvProbe = structuredClone(builder);
  weakenedVenvProbe.probes.find((probe) => probe.name === "python3-venv").command = "python3 -c 'import venv'";
  assert.throws(() => assertFrozenContract(minimal, weakenedVenvProbe), /exact v1 python3-venv probe/u);
  const venvProbe = builder.probes.find((probe) => probe.name === "python3-venv").command;
  assert.match(venvProbe, /TemporaryDirectory\(\)/u);
  assert.match(venvProbe, /EnvBuilder\(with_pip=True\)\.create/u);
  assert.match(venvProbe, /\/venv\/bin\/python.*"pip".*"--version"/u);
  const invalidSchema = structuredClone(manifestSchema);
  invalidSchema.additionalProperties = true;
  assert.throws(() => validateSchemaDefinition(invalidSchema, "manifest"), /unknown object fields/u);
  const invalidDigestSchema = structuredClone(manifestSchema);
  invalidDigestSchema.properties.recipeDigest.pattern = ".*";
  assert.throws(() => validateSchemaDefinition(invalidDigestSchema, "manifest"), /lowercase SHA-256/u);
});

test("strict JSON parsing rejects duplicate object keys and trailing documents", () => {
  assert.throws(() => parseStrictJSON('{"profile":"a","profile":"b"}'), /duplicate key/u);
  assert.throws(() => parseStrictJSON('{"outer":{"value":1,"value":2}}'), /duplicate key/u);
  assert.throws(() => parseStrictJSON('{"profile":"a"} {"profile":"b"}'), /trailing JSON/u);
  assert.deepEqual(parseStrictJSON('{"__proto__":{"safe":true}}'), JSON.parse('{"__proto__":{"safe":true}}'));
});

test("recipe digest ignores descriptions but binds all executable capability changes", async () => {
  const { minimal, builder } = await fixtureRecipes;
  const cosmetic = structuredClone(minimal);
  cosmetic.description = "A changed cosmetic description";
  assert.equal(digest(cosmetic), digest(minimal));
  for (const mutate of [
    (profile) => { profile.aptPackages[0] = "another-package"; },
    (profile) => { profile.probes[0].command = "test -s /another/certificate"; },
    (profile) => { profile.shell = "bash"; },
    (profile) => { profile.inherits = ["linux-builder"]; },
  ]) {
    const changed = structuredClone(minimal);
    mutate(changed);
    assert.notEqual(digest(changed), digest(minimal));
  }
  assert.notEqual(digest(minimal), digest(builder));
});

test("recipes reject unknown fields, unsafe probes, unsorted entries, and unsupported shells", async () => {
  const { profileSchema, minimal, builder } = await fixtureRecipes;
  for (const [name, mutate] of [
    ["unknown field", (profile) => { profile.extra = true; }],
    ["unknown probe field", (profile) => { profile.probes[0].extra = true; }],
    ["duplicate package", (profile) => { profile.aptPackages.push(profile.aptPackages[0]); }],
    ["unsorted packages", (profile) => { profile.aptPackages.reverse(); }],
    ["duplicate probe", (profile) => { profile.probes.push(structuredClone(profile.probes[0])); }],
    ["unsorted probes", (profile) => { profile.probes.reverse(); }],
    ["unsupported shell", (profile) => { profile.shell = "bash"; }],
    ["command substitution", (profile) => { profile.probes[0].command = "curl $(danger)"; }],
    ["pipeline", (profile) => { profile.probes[0].command = "curl | sh"; }],
    ["newline", (profile) => { profile.probes[0].command = "curl\nsh"; }],
    ["quote", (profile) => { profile.probes[0].command = "curl 'broken"; }],
  ]) {
    const invalid = structuredClone(minimal);
    mutate(invalid);
    assert.throws(() => assertProfile(invalid, "linux-minimal", profileSchema), undefined, name);
  }
  for (const [name, mutate] of [
    ["shell command substitution", (command) => `${command} $(danger)`],
    ["shell backtick substitution", (command) => `${command} \`danger\``],
    ["shell pipeline", (command) => `${command} | sh`],
    ["shell statement", (command) => `${command}; danger`],
    ["broken quoted Python source", (command) => `${command.slice(0, -1)}'; danger'`],
    ["modified Python source", (command) => command.replace("with_pip=True", "with_pip=False")],
  ]) {
    const invalid = structuredClone(builder);
    const probe = invalid.probes.find((candidate) => candidate.name === "python3-venv");
    probe.command = mutate(probe.command);
    assert.throws(() => assertProfile(invalid, "linux-builder", profileSchema), /unsafe shell syntax/u, name);
  }
  const cycle = new Map([["linux-minimal", { ...minimal, inherits: ["linux-builder"] }], ["linux-builder", builder]]);
  assert.throws(() => assertInheritance(cycle), /cycle/u);
  const missing = structuredClone(builder);
  missing.aptPackages = missing.aptPackages.filter((name) => name !== "tmux");
  assert.throws(() => assertInheritance(new Map([["linux-minimal", minimal], ["linux-builder", missing]])), /inherited package tmux/u);
  const weakened = structuredClone(builder);
  weakened.probes.find((probe) => probe.name === "flock").command = "flock --help";
  assert.throws(() => assertInheritance(new Map([["linux-minimal", minimal], ["linux-builder", weakened]])), /exact inherited probe flock/u);
});

test("three generated artifacts are deterministic and --check rejects every stale artifact", async (t) => {
  const { minimal, builder } = await fixtureRecipes;
  const bootstrap = minimalBootstrap(minimal, builder);
  const generated = [
    ["internal/cli/linux_readiness_generated.go", goSource(bootstrap)],
    ["worker/src/linux-readiness.generated.ts", tsSource(bootstrap)],
    ["scripts/linux-readiness.generated.sh", producerSource(minimal, builder)],
  ];
  for (const [path, expected] of generated) {
    const actual = await readFile(resolve(repoRoot, path), "utf8");
    assert.equal(actual, expected);
  }
  const result = spawnSync(process.execPath, ["scripts/generate-linux-readiness.mjs", "--check"], { cwd: repoRoot, encoding: "utf8" });
  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.equal(bootstrap, minimalBootstrap(minimal, builder));
  const source = await readFile(resolve(repoRoot, "scripts/generate-linux-readiness.mjs"), "utf8");
  assert.doesNotMatch(source, /readFile\([^\n]*(?:go\.mod|\.node-version)/u);
  for (const [artifact] of generated) {
    assert.ok(source.includes(artifact), `generator source does not reference ${artifact}`);
    await t.test(`stale ${artifact}`, async (subtest) => {
      const copy = await mkdtemp(join(tmpdir(), "crabbox-readiness-generated-"));
      subtest.after(async () => rm(copy, { recursive: true, force: true }));
      for (const directory of ["recipes/linux/v1", "internal/cli", "worker/src", "scripts"]) {
        await mkdir(join(copy, directory), { recursive: true });
      }
      await cp(join(repoRoot, "recipes/linux/v1"), join(copy, "recipes/linux/v1"), { recursive: true });
      for (const [path] of generated) {
        await cp(join(repoRoot, path), join(copy, path));
      }
      await cp(join(repoRoot, "scripts/generate-linux-readiness.mjs"), join(copy, "scripts/generate-linux-readiness.mjs"));
      await writeFile(join(copy, artifact), "stale generated content\n");
      const stale = spawnSync(process.execPath, [join(copy, "scripts/generate-linux-readiness.mjs"), "--check"], { cwd: copy, encoding: "utf8" });
      assert.notEqual(stale.status, 0);
      assert.match(stale.stderr, /is stale/u);
    });
  }
});

test("standalone producer prints each frozen package contract without requiring root or writing evidence", async () => {
  const { minimal, builder } = await fixtureRecipes;
  const producer = resolve(repoRoot, "scripts/linux-readiness.generated.sh");
  for (const profile of [minimal, builder]) {
    const result = spawnSync(producer, ["--print-packages", profile.profile], { cwd: repoRoot, encoding: "utf8" });
    assert.equal(result.status, 0, result.stderr || result.stdout);
    assert.equal(result.stdout, `${profile.aptPackages.join(" ")}\n`);
  }
  for (const args of [["--print-packages"], ["--print-packages", "unknown"], ["--print-packages", "linux-minimal", "extra"]]) {
    const result = spawnSync(producer, args, { cwd: repoRoot, encoding: "utf8" });
    assert.equal(result.status, 2, `${args.join(" ")}: ${result.stderr || result.stdout}`);
  }
  assert.doesNotMatch(producerSource(minimal, builder), /^crabbox_readiness_packages=/mu);
});

test("valid minimal and builder manifests rerun their complete probes without package managers", async (t) => {
  for (const profile of ["linux-minimal", "linux-builder"]) {
    await t.test(profile, async (subtest) => {
      const fixture = await createFixture(subtest);
      await fixture.writeManifest(profile);
      const result = fixture.run();
      assert.equal(result.status, 0, result.stderr || result.stdout);
      assert.match(result.stdout, /readiness manifest verified/u);
      assert.deepEqual(await fixture.packageCalls(), []);
    });
  }
});

test("bootstrap and producer parse metadata in the C locale without changing probe locales", async (t) => {
  const locale = { LC_ALL: "fr_FR.UTF-8", LANG: "de_DE.UTF-8" };
  const environment = {
    ...locale,
    CRABBOX_STAT_TRANSLATE_LOCALE: "1",
    CRABBOX_FIXTURE_EXPECT_LC_ALL: locale.LC_ALL,
    CRABBOX_FIXTURE_EXPECT_LANG: locale.LANG,
  };
  for (const scenario of ["bootstrap", "producer"]) {
    await t.test(scenario, async (subtest) => {
      const fixture = await createFixture(subtest);
      if (scenario === "bootstrap") await fixture.writeManifest();
      const result = scenario === "bootstrap"
        ? fixture.run(fixture.shell, environment)
        : fixture.runProducer(environment);
      assert.equal(result.status, 0, result.stderr || result.stdout);
      const profile = scenario === "bootstrap" ? "linux-minimal" : "linux-builder";
      const recipe = scenario === "bootstrap" ? fixture.minimal : fixture.builder;
      assert.equal(
        await readFile(fixture.manifest, "utf8"),
        `${canonicalJSON(manifestFor(profile, digest(recipe)))}\n`,
      );
      if (scenario === "producer") {
        assert.equal(await readFile(fixture.marker, "utf8"), "crabbox-devtools-v1\n");
      }
      assert.deepEqual(await fixture.packageCalls(), []);
    });
  }
});

test("trusted marker-only migration emits canonical minimal bytes with no apt or dpkg calls", async (t) => {
  const fixture = await createFixture(t);
  await fixture.writeMarker();
  const result = fixture.run();
  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.deepEqual(await fixture.packageCalls(), []);
  const expected = `${canonicalJSON(manifestFor("linux-minimal", digest(fixture.minimal)))}\n`;
  assert.equal(await readFile(fixture.manifest, "utf8"), expected);
});

test("marker-only migration never creates a missing legacy parent", async (t) => {
  const fixture = await createFixture(t);
  await rm(fixture.state, { recursive: true });
  const functions = fixture.shell.slice(0, fixture.shell.indexOf("\ncrabbox_readiness_packages="));
  const result = fixture.run(`${functions}\nif crabbox_legacy_image_marker_trusted; then exit 94; fi\ntest ! -e ${quote(fixture.state)}`);
  assert.equal(result.status, 0, result.stderr || result.stdout);
  await assert.rejects(stat(fixture.state), { code: "ENOENT" });
  assert.deepEqual(await fixture.packageCalls(), []);
});

test("legacy migration accepts the real runtime-owned marker parent only after independent minimal probes", async (t) => {
  for (const [name, missing] of [["all probes pass", null], ["minimal probe missing", "tmux"]]) {
    await t.test(name, async (subtest) => {
      const fixture = await createFixture(subtest);
      await fixture.writeMarker();
      if (missing) await writeFile(join(fixture.root, `disabled-${missing}`), "1");
      const result = fixture.run(fixture.shell, {
        CRABBOX_STAT_OVERRIDE_PATH: fixture.state,
        CRABBOX_STAT_OWNER: String(Number(fixture.ownerUID) + 1),
        CRABBOX_STAT_GROUP: String(Number(fixture.ownerGID) + 1),
      });
      if (missing) {
        assert.notEqual(result.status, 0);
        assert.deepEqual(await fixture.packageCalls(), ["apt-get update"]);
        await assert.rejects(readFile(fixture.manifest));
      } else {
        assert.equal(result.status, 0, result.stderr || result.stdout);
        assert.deepEqual(await fixture.packageCalls(), []);
        assert.equal(JSON.parse(await readFile(fixture.manifest, "utf8")).profile, "linux-minimal");
      }
    });
  }
});

test("readiness probes ignore user PATH entries and shadowing shell functions", async (t) => {
  const fixture = await createFixture(t);
  await fixture.writeMarker();
  await writeFile(join(fixture.root, "disabled-curl"), "1");
  const hostile = join(fixture.root, "hostile");
  const executionLog = join(fixture.root, "hostile-executed");
  await mkdir(hostile);
  await executable(join(hostile, "curl"), `touch ${quote(executionLog)}\nexit 0`);
  const source = `curl() { touch ${quote(executionLog)}; return 0; }\n${fixture.shell}`;
  const result = fixture.run(source, { PATH: `${hostile}:${fixture.bin}:${process.env.PATH}` });
  assert.notEqual(result.status, 0);
  assert.deepEqual(await fixture.packageCalls(), ["apt-get update"]);
  await assert.rejects(readFile(executionLog));
  await assert.rejects(readFile(fixture.manifest));
});

test("a valid authoritative manifest does not depend on its compatibility marker directory", async (t) => {
  const fixture = await createFixture(t);
  await fixture.writeManifest();
  await chmod(fixture.state, 0o757);
  const result = fixture.run();
  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.deepEqual(await fixture.packageCalls(), []);
});

test("truly clean bootstrap creates both trusted parents and atomically writes canonical minimal evidence", async (t) => {
  const fixture = await createFixture(t);
  await rm(fixture.readiness, { recursive: true });
  await rm(fixture.state, { recursive: true });
  const result = fixture.run(fixture.shell, { CRABBOX_APT_SUCCESS: "1" });
  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.deepEqual(await fixture.packageCalls(), [
    "apt-get update",
    `apt-get install -y --no-install-recommends ${fixture.minimal.aptPackages.join(" ")}`,
  ]);
  assert.equal(await readFile(fixture.manifest, "utf8"), `${canonicalJSON(manifestFor("linux-minimal", digest(fixture.minimal)))}\n`);
  assert.equal(await readFile(fixture.marker, "utf8"), "crabbox-devtools-v1\n");
  for (const directory of [fixture.readiness, fixture.state]) {
    const metadata = await stat(directory);
    assert.equal(metadata.uid, Number(fixture.ownerUID));
    assert.equal(metadata.gid, Number(fixture.ownerGID));
    assert.equal(metadata.mode & 0o777, 0o755);
  }
  const markerMetadata = await stat(fixture.marker);
  assert.equal(markerMetadata.uid, Number(fixture.ownerUID));
  assert.equal(markerMetadata.gid, Number(fixture.ownerGID));
  assert.equal(markerMetadata.mode & 0o777, 0o644);
  assert.deepEqual((await readdir(fixture.readiness)).filter((name) => name.startsWith(".linux-readiness.")), []);
  assert.deepEqual((await readdir(fixture.readiness)).filter((name) => name.startsWith(".linux-readiness-marker.")), []);
});

test("missing legacy parent is never created beneath untrusted, writable, or symlinked ancestors", async (t) => {
  const cases = [
    ["runtime-owned ancestor", async (fixture, ancestor) => {
      await mkdir(ancestor, { mode: 0o755 });
      return { CRABBOX_STAT_OVERRIDE_PATH: ancestor, CRABBOX_STAT_OWNER: String(Number(fixture.ownerUID) + 1) };
    }],
    ["wrong-group ancestor", async (fixture, ancestor) => {
      await mkdir(ancestor, { mode: 0o755 });
      return { CRABBOX_STAT_OVERRIDE_PATH: ancestor, CRABBOX_STAT_GROUP: String(Number(fixture.ownerGID) + 1) };
    }],
    ["group-writable ancestor", async (_fixture, ancestor) => {
      await mkdir(ancestor, { mode: 0o755 });
      await chmod(ancestor, 0o775);
      return {};
    }],
    ["world-writable ancestor", async (_fixture, ancestor) => {
      await mkdir(ancestor, { mode: 0o755 });
      await chmod(ancestor, 0o757);
      return {};
    }],
    ["symlink ancestor", async (fixture, ancestor) => {
      const protectedDirectory = join(fixture.root, "protected-ancestor");
      await mkdir(protectedDirectory, { mode: 0o755 });
      await symlink(protectedDirectory, ancestor);
      return {};
    }],
    ["nondirectory ancestor", async (_fixture, ancestor) => {
      await writeFile(ancestor, "untouched\n");
      return {};
    }],
  ];
  for (const [name, prepare] of cases) {
    await t.test(name, async (subtest) => {
      const fixture = await createFixture(subtest);
      const ancestor = join(fixture.root, "legacy-ancestor");
      const parent = join(ancestor, "crabbox");
      const marker = join(parent, "image-ready");
      const environment = await prepare(fixture, ancestor);
      const source = minimalBootstrap(fixture.minimal, fixture.builder, { ...fixture.options, legacyMarkerPath: marker });
      const result = fixture.run(source, { ...environment, CRABBOX_APT_SUCCESS: "1" });
      assert.notEqual(result.status, 0, result.stderr || result.stdout);
      assert.deepEqual(await fixture.packageCalls(), [
        "apt-get update",
        `apt-get install -y --no-install-recommends ${fixture.minimal.aptPackages.join(" ")}`,
      ]);
      await assert.rejects(stat(parent), (error) => error?.code === "ENOENT" || error?.code === "ENOTDIR");
      await assert.rejects(readFile(marker));
      assert.deepEqual((await readdir(fixture.readiness)).filter((entry) => entry.startsWith(".linux-readiness-marker.")), []);
    });
  }
});

test("noncanonical, stale, oversized, or unsafe manifest never allows marker rescue", async (t) => {
  const malformed = [
    ["duplicate key", (fixture) => '{"profile":"wrong","profile":"linux-minimal","recipeDigest":"' + digest(fixture.minimal) + '","schema":"crabbox-linux-readiness/v1"}\n'],
    ["trailing document", (fixture) => canonicalJSON(manifestFor("linux-minimal", digest(fixture.minimal))) + '\n{}\n'],
    ["unknown field", (fixture) => canonicalJSON({ ...manifestFor("linux-minimal", digest(fixture.minimal)), unknown: true }) + '\n'],
    ["extra whitespace", (fixture) => JSON.stringify(manifestFor("linux-minimal", digest(fixture.minimal)), null, 2) + '\n'],
    ["missing newline", (fixture) => canonicalJSON(manifestFor("linux-minimal", digest(fixture.minimal)))],
    ["stale digest", () => canonicalJSON(manifestFor("linux-minimal", `sha256:${"f".repeat(64)}`)) + '\n'],
    ["unknown profile", (fixture) => canonicalJSON(manifestFor("linux-unknown", digest(fixture.minimal))) + '\n'],
    ["unsupported schema", (fixture) => canonicalJSON({ ...manifestFor("linux-minimal", digest(fixture.minimal)), schema: "crabbox-linux-readiness/v2" }) + '\n'],
    ["oversized", () => "x".repeat(4097)],
  ];
  for (const [name, contents] of malformed) {
    await t.test(name, async (subtest) => {
      const fixture = await createFixture(subtest);
      await fixture.writeMarker();
      await fixture.writeManifest("linux-minimal", contents(fixture));
      const result = fixture.run();
      assert.notEqual(result.status, 0);
      assert.deepEqual(await fixture.packageCalls(), ["apt-get update"]);
    });
  }
});

test("manifest and marker reject symlinks, untrusted parents, ownership, group, modes, and stat failures", async (t) => {
  const cases = [
    ["manifest symlink", async (fixture) => { await fixture.writeManifest(); await writeFile(`${fixture.manifest}.target`, await readFile(fixture.manifest)); await rm(fixture.manifest); await symlink(`${fixture.manifest}.target`, fixture.manifest); }],
    ["manifest parent symlink", async (fixture) => { await rm(fixture.readiness, { recursive: true }); const alternate = join(fixture.root, "alternate"); await mkdir(alternate); await symlink(alternate, fixture.readiness); await fixture.writeManifest(); }],
    ["marker symlink", async (fixture) => { await writeFile(`${fixture.marker}.target`, "crabbox-devtools-v1\n"); await symlink(`${fixture.marker}.target`, fixture.marker); }],
    ["marker parent symlink", async (fixture) => { await rm(fixture.state, { recursive: true }); const alternate = join(fixture.root, "alternate"); await mkdir(alternate); await symlink(alternate, fixture.state); await fixture.writeMarker(); }],
    ["marker wrong mode", async (fixture) => { await fixture.writeMarker(); await chmod(fixture.marker, 0o600); }],
    ["marker wrong owner", async (fixture) => { await fixture.writeMarker(); return { CRABBOX_STAT_OVERRIDE_PATH: fixture.marker, CRABBOX_STAT_OWNER: String(Number(fixture.ownerUID) + 1) }; }],
    ["marker wrong group", async (fixture) => { await fixture.writeMarker(); return { CRABBOX_STAT_OVERRIDE_PATH: fixture.marker, CRABBOX_STAT_GROUP: String(Number(fixture.ownerGID) + 1) }; }],
    ["oversized marker", async (fixture) => { await writeFile(fixture.marker, "x".repeat(4097), { mode: 0o644 }); }],
    ["unexpected marker contents", async (fixture) => { await writeFile(fixture.marker, "not-a-trusted-marker\n", { mode: 0o644 }); }],
    ["group-writable parent", async (fixture) => { await fixture.writeManifest(); await chmod(fixture.readiness, 0o775); }],
    ["world-writable authoritative parent", async (fixture) => { await fixture.writeManifest(); await chmod(fixture.readiness, 0o757); }],
    ["world-writable marker parent", async (fixture) => { await fixture.writeMarker(); await chmod(fixture.state, 0o757); }],
    ["wrong file mode", async (fixture) => { await fixture.writeManifest(); await chmod(fixture.manifest, 0o600); }],
    ["nonregular manifest", async (fixture) => { await mkdir(fixture.manifest); }],
    ["wrong owner", async (fixture) => { await fixture.writeManifest(); return { CRABBOX_STAT_OVERRIDE_PATH: fixture.manifest, CRABBOX_STAT_OWNER: String(Number(fixture.ownerUID) + 1) }; }],
    ["wrong group", async (fixture) => { await fixture.writeManifest(); return { CRABBOX_STAT_OVERRIDE_PATH: fixture.manifest, CRABBOX_STAT_GROUP: String(Number(fixture.ownerGID) + 1) }; }],
    ["stat unavailable", async (fixture) => { await fixture.writeManifest(); return { CRABBOX_STAT_FAIL: "1" }; }],
  ];
  for (const [name, setup] of cases) {
    await t.test(name, async (subtest) => {
      const fixture = await createFixture(subtest);
      const overrides = await setup(fixture);
      const result = fixture.run(fixture.shell, overrides);
      assert.notEqual(result.status, 0, result.stderr || result.stdout);
      assert.deepEqual(await fixture.packageCalls(), ["apt-get update"]);
    });
  }
});

test("every missing minimal or builder probe rejects the corresponding manifest", async (t) => {
  const { minimal, builder } = await fixtureRecipes;
  for (const profile of [minimal, builder]) {
    for (const probe of profile.probes) {
      const name = probe.name === "compiler" ? "cc" : probe.name;
      await t.test(`${profile.profile}/${name}`, async (subtest) => {
        const fixture = await createFixture(subtest);
        await fixture.writeManifest(profile.profile);
        await fixture.writeMarker();
        if (name === "ca-certificates") await writeFile(fixture.ca, "");
        else if (name === "sshd") await chmod(fixture.sshd, 0o644);
        else await writeFile(join(fixture.root, `disabled-${name}`), "1");
        const result = fixture.run();
        assert.notEqual(result.status, 0);
        assert.deepEqual(await fixture.packageCalls(), ["apt-get update"]);
      });
    }
  }
});

test("atomic-write failures clean their private same-directory temporary files", async (t) => {
  const fixture = await createFixture(t);
  await fixture.writeMarker();
  const result = fixture.run(fixture.shell, { CRABBOX_FAIL_MV: "1" });
  assert.notEqual(result.status, 0);
  assert.deepEqual(await fixture.packageCalls(), []);
  assert.deepEqual((await readdir(fixture.readiness)).filter((name) => name.startsWith(".linux-readiness.")), []);
});

test("compatibility-marker writer rejects symlinks, cross-device writes, and rename failures without unsafe leftovers", async (t) => {
  for (const [name, prepare] of [
    ["destination symlink", async (fixture) => {
      const target = join(fixture.root, "protected-target");
      await writeFile(target, "untouched\n");
      await symlink(target, fixture.marker);
      return [{}, target];
    }],
    ["different filesystem", async (fixture) => [{ CRABBOX_STAT_DEVICE_OVERRIDE_PATH: fixture.state }, null]],
    ["rename failure", async () => [{ CRABBOX_FAIL_MARKER_MV: "1" }, null]],
  ]) {
    await t.test(name, async (subtest) => {
      const fixture = await createFixture(subtest);
      const [environment, protectedTarget] = await prepare(fixture);
      const result = fixture.runProducer(environment);
      assert.notEqual(result.status, 0);
      assert.equal(JSON.parse(await readFile(fixture.manifest, "utf8")).profile, "linux-builder");
      assert.deepEqual((await readdir(fixture.readiness)).filter((name) => name.startsWith(".linux-readiness-marker.")), []);
      if (protectedTarget) assert.equal(await readFile(protectedTarget, "utf8"), "untouched\n");
    });
  }
});

test("compatibility-marker writer never replaces an existing symlink or nondirectory parent", async (t) => {
  for (const name of ["symlink parent", "nondirectory parent"]) {
    await t.test(name, async (subtest) => {
      const fixture = await createFixture(subtest);
      await rm(fixture.state, { recursive: true });
      if (name === "symlink parent") {
        const protectedDirectory = join(fixture.root, "protected-parent");
        await mkdir(protectedDirectory, { mode: 0o755 });
        await symlink(protectedDirectory, fixture.state);
      } else {
        await writeFile(fixture.state, "untouched\n");
      }
      const result = fixture.runProducer();
      assert.notEqual(result.status, 0, result.stderr || result.stdout);
      assert.equal((await lstat(fixture.state)).isSymbolicLink(), name === "symlink parent");
      if (name === "symlink parent") assert.deepEqual(await readdir(fixture.state), []);
      else assert.equal(await readFile(fixture.state, "utf8"), "untouched\n");
      await assert.rejects(readFile(fixture.marker));
      assert.deepEqual((await readdir(fixture.readiness)).filter((entry) => entry.startsWith(".linux-readiness-marker.")), []);
    });
  }
});

test("producer supports the AWS runtime-owned legacy directory while keeping authoritative state separately owned", async (t) => {
  for (const [name, disabled, expectedProfile] of [
    ["builder image", null, "linux-builder"],
    ["minimal downgrade", "git-lfs", "linux-minimal"],
  ]) {
    await t.test(name, async (subtest) => {
      const fixture = await createFixture(subtest);
      if (disabled) await writeFile(join(fixture.root, `disabled-${disabled}`), "1");
      const result = fixture.runProducer({
        CRABBOX_STAT_OVERRIDE_PATH: fixture.state,
        CRABBOX_STAT_OWNER: String(Number(fixture.ownerUID) + 1),
        CRABBOX_STAT_GROUP: String(Number(fixture.ownerGID) + 1),
      });
      assert.equal(result.status, 0, result.stderr || result.stdout);
      assert.equal(JSON.parse(await readFile(fixture.manifest, "utf8")).profile, expectedProfile);
      assert.equal(await readFile(fixture.marker, "utf8"), "crabbox-devtools-v1\n");
      assert.deepEqual(await fixture.packageCalls(), []);
    });
  }
});

test("standalone producer emits builder, downgrades missing builder proof, and refuses missing minimal proof", async (t) => {
  for (const [name, disabled, expectedProfile] of [
    ["complete builder", null, "linux-builder"],
    ["missing Git LFS", "git-lfs", "linux-minimal"],
    ["missing compiler", "cc", "linux-minimal"],
    ["missing Python virtual environments", "python3-venv", "linux-minimal"],
    ["missing minimal tmux", "tmux", null],
    ["missing minimal flock", "flock", null],
  ]) {
    await t.test(name, async (subtest) => {
      const fixture = await createFixture(subtest);
      if (disabled) await writeFile(join(fixture.root, `disabled-${disabled}`), "1");
      const result = fixture.runProducer();
      assert.deepEqual(await fixture.packageCalls(), []);
      if (expectedProfile === null) {
        assert.notEqual(result.status, 0);
        assert.match(result.stderr, /minimal capability proof failed/u);
        await assert.rejects(readFile(fixture.manifest));
        await assert.rejects(readFile(fixture.marker));
      } else {
        assert.equal(result.status, 0, result.stderr || result.stdout);
        assert.equal(JSON.parse(await readFile(fixture.manifest, "utf8")).profile, expectedProfile);
        assert.equal(await readFile(fixture.marker, "utf8"), "crabbox-devtools-v1\n");
      }
    });
  }
});

test("actual generated producer proves and cleans a real pip-enabled Python virtual environment", async (t) => {
  const fixture = await createFixture(t);
  const resolvedPython = spawnSync("python3", ["-c", "import sys; print(sys.executable)"], { encoding: "utf8" });
  assert.equal(resolvedPython.status, 0, resolvedPython.stderr);
  const temporaryRoot = join(fixture.root, "venv-temporary");
  await mkdir(temporaryRoot);
  const producer = await readFile(resolve(repoRoot, "scripts/linux-readiness.generated.sh"), "utf8");
  const source = fixture.actualGenerated(producer.slice(producer.indexOf("crabbox_readiness_manifest_path=")));
  const result = fixture.run(source, {
    CRABBOX_FIXTURE_REAL_VENV: "1",
    CRABBOX_FIXTURE_PYTHON: resolvedPython.stdout.trim(),
    TMPDIR: temporaryRoot,
  });
  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.equal(JSON.parse(await readFile(fixture.manifest, "utf8")).profile, "linux-builder");
  const probes = (await readFile(join(fixture.root, "venv-probes.log"), "utf8")).trim().split("\n");
  assert.ok(probes.length >= 2, "producer and final manifest verification must each create a real venv");
  for (const probe of probes) {
    assert.match(probe, /EnvBuilder\(with_pip=True\)\.create/u);
    assert.match(probe, /"pip", "--version"/u);
  }
  assert.deepEqual(await readdir(temporaryRoot), [], "successful venv probes must clean their temporary directories");
  assert.deepEqual(await fixture.packageCalls(), []);
});

test("actual generated producer downgrades when venv imports but pip-enabled creation fails", async (t) => {
  const fixture = await createFixture(t);
  const resolvedPython = spawnSync("python3", ["-c", "import sys; print(sys.executable)"], { encoding: "utf8" });
  assert.equal(resolvedPython.status, 0, resolvedPython.stderr);
  const modules = join(fixture.root, "python-modules");
  const temporaryRoot = join(fixture.root, "venv-temporary");
  await mkdir(modules);
  await mkdir(temporaryRoot);
  await writeFile(join(modules, "venv.py"), `class EnvBuilder:\n    def __init__(self, *, with_pip):\n        self.with_pip = with_pip\n\n    def create(self, directory):\n        raise RuntimeError("ensurepip is unavailable")\n`);
  const importOnly = spawnSync(resolvedPython.stdout.trim(), ["-c", "import venv"], {
    encoding: "utf8",
    env: { ...process.env, PYTHONPATH: modules },
  });
  assert.equal(importOnly.status, 0, importOnly.stderr);
  const producer = await readFile(resolve(repoRoot, "scripts/linux-readiness.generated.sh"), "utf8");
  const source = fixture.actualGenerated(producer.slice(producer.indexOf("crabbox_readiness_manifest_path=")));
  const result = fixture.run(source, {
    CRABBOX_FIXTURE_REAL_VENV: "1",
    CRABBOX_FIXTURE_PYTHON: resolvedPython.stdout.trim(),
    PYTHONPATH: modules,
    TMPDIR: temporaryRoot,
  });
  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.equal(JSON.parse(await readFile(fixture.manifest, "utf8")).profile, "linux-minimal");
  assert.match(await readFile(join(fixture.root, "venv-probes.log"), "utf8"), /EnvBuilder\(with_pip=True\)\.create/u);
  assert.deepEqual(await readdir(temporaryRoot), [], "failed venv probes must clean their temporary directories");
  assert.deepEqual(await fixture.packageCalls(), []);
});

test("standalone producer replaces an existing builder claim when a builder capability disappears", async (t) => {
  const fixture = await createFixture(t);
  await fixture.writeManifest("linux-builder");
  await fixture.writeMarker();
  await writeFile(join(fixture.root, "disabled-git-lfs"), "1");
  const result = fixture.runProducer();
  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.equal(JSON.parse(await readFile(fixture.manifest, "utf8")).profile, "linux-minimal");
  assert.deepEqual(await fixture.packageCalls(), []);
});

test("actual generated Go and Worker bootstrap fragments make identical decisions and bytes", async (t) => {
  const go = await readFile(resolve(repoRoot, "internal/cli/linux_readiness_generated.go"), "utf8");
  const worker = await readFile(resolve(repoRoot, "worker/src/linux-readiness.generated.ts"), "utf8");
  const goMatch = go.match(/linuxMinimalReadinessBootstrap\s*=\s*("(?:\\.|[^"\\])*")/u);
  const workerMatch = worker.match(/linuxMinimalReadinessBootstrap\s*=\s*("(?:\\.|[^"\\])*")/u);
  assert.ok(goMatch);
  assert.ok(workerMatch);
  const goFragment = JSON.parse(goMatch[1]);
  const workerFragment = JSON.parse(workerMatch[1]);
  assert.equal(goFragment, workerFragment);
  for (const [scenario, prepare, environment] of [
    ["minimal", async (fixture) => fixture.writeManifest(), {}],
    ["builder", async (fixture) => fixture.writeManifest("linux-builder"), {}],
    ["legacy", async (fixture) => fixture.writeMarker(), {}],
    ["cold", async (fixture) => {
      await rm(fixture.readiness, { recursive: true });
      await rm(fixture.state, { recursive: true });
    }, { CRABBOX_APT_SUCCESS: "1" }],
    ["unsafe", async (fixture) => { await fixture.writeMarker(); await fixture.writeManifest("linux-minimal", "{}\n"); }, {}],
  ]) {
    await t.test(scenario, async (subtest) => {
      const fixture = await createFixture(subtest);
      await prepare(fixture);
      const result = fixture.run(fixture.actualGenerated(goFragment), environment);
      if (scenario === "unsafe") {
        assert.notEqual(result.status, 0);
        assert.deepEqual(await fixture.packageCalls(), ["apt-get update"]);
        assert.equal(await readFile(fixture.manifest, "utf8"), "{}\n");
        return;
      }
      assert.equal(result.status, 0, result.stderr || result.stdout);
      const profile = scenario === "builder" ? "linux-builder" : "linux-minimal";
      assert.equal(JSON.parse(await readFile(fixture.manifest, "utf8")).profile, profile);
      assert.equal((await fixture.packageCalls()).length, scenario === "cold" ? 2 : 0);
    });
  }
});
