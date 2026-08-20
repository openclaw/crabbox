import assert from "node:assert/strict";
import { chmod, mkdir, mkdtemp, readFile, rm, symlink, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { test } from "node:test";
import { fileURLToPath } from "node:url";
import { spawnSync } from "node:child_process";

import {
  assertBuilderSuperset,
  assertProfile,
  digest,
  minimalBootstrap,
} from "./generate-linux-readiness.mjs";

const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..");

function shellQuote(value) {
  return `'${String(value).replaceAll("'", "'\"'\"'")}'`;
}

async function writeExecutable(path, body) {
  await writeFile(path, `#!/usr/bin/env bash\n${body}\n`);
  await chmod(path, 0o755);
}

async function readProfiles() {
  return {
    minimal: JSON.parse(
      await readFile(resolve(repoRoot, "recipes/linux/v1/linux-minimal.json"), "utf8"),
    ),
    builder: JSON.parse(
      await readFile(resolve(repoRoot, "recipes/linux/v1/linux-builder.json"), "utf8"),
    ),
  };
}

async function readRepoVersions() {
  const goMod = await readFile(resolve(repoRoot, "go.mod"), "utf8");
  const node = (await readFile(resolve(repoRoot, ".node-version"), "utf8")).trim();
  const go = goMod.match(/^toolchain go(\S+)$/mu)?.[1] ?? "";
  assert.ok(go);
  assert.ok(node);
  return { go, node };
}

async function createShellFixture(t) {
  const root = await mkdtemp(join(tmpdir(), "crabbox-readiness-"));
  t.after(async () => {
    await rm(root, { recursive: true, force: true });
  });
  const bin = join(root, "bin");
  const manifestPath = join(root, "readiness", "linux.json");
  const legacyMarkerPath = join(root, "image-ready");
  const sshdPath = join(root, "sshd");
  const caPath = join(root, "ca-certificates.crt");
  const packageLog = join(root, "package-manager.log");
  await mkdir(bin, { recursive: true });
  await writeExecutable(sshdPath, "exit 0");
  await writeFile(caPath, "test-ca\n");

  for (const command of ["curl", "git", "rsync", "tmux", "flock"]) {
    await writeExecutable(join(bin, command), `printf '%s test\\n' ${shellQuote(command)}`);
  }
  const jqPath = spawnSync("sh", ["-c", "command -v jq"], { encoding: "utf8" }).stdout.trim();
  assert.ok(jqPath, "jq is required for readiness shell tests");
  await writeExecutable(join(bin, "jq"), `exec ${shellQuote(jqPath)} "$@"`);
  await writeExecutable(
    join(bin, "stat"),
    `if [ "$1" != "-c" ]; then exec /usr/bin/stat "$@"; fi
file="$3"
if [ "$(uname -s)" = "Darwin" ]; then
  exec /usr/bin/stat -f '%u:%Lp' "$file"
fi
exec /usr/bin/stat -c '%u:%a' "$file"`,
  );
  for (const command of ["apt", "apt-get", "apt-cache", "dpkg", "dpkg-query"]) {
    await writeExecutable(
      join(bin, command),
      `printf '%s\\n' ${shellQuote(command)} >>"$CRABBOX_PACKAGE_LOG"
exit 97`,
    );
  }

  const { minimal, builder } = await readProfiles();
  const minimalDigest = digest(minimal);
  const builderDigest = digest(builder);
  const ownerUID = String(process.getuid?.() ?? 0);

  function shell(overrides = {}) {
    return minimalBootstrap(minimal, minimalDigest, builderDigest, {
      manifestPath,
      legacyMarkerPath,
      sshdPath,
      caPath,
      aptConfigPath: join(root, "80-crabbox-retries"),
      manifestOwnerUID: ownerUID,
      manifestOwnerSpec: "",
      manifestDirectoryOwnerFlags: "",
      ...overrides,
    });
  }

  async function writeManifest(profile, recipeDigest, options = {}) {
    await mkdir(dirname(manifestPath), { recursive: true });
    await rm(manifestPath, { force: true });
    await writeFile(
      manifestPath,
      `${JSON.stringify({
        inventoryDigest: `sha256:${"0".repeat(64)}`,
        profile,
        recipeDigest,
        schema: "crabbox-linux-readiness/v1",
      })}\n`,
    );
    await chmod(manifestPath, options.mode ?? 0o644);
  }

  function run(overrides = {}) {
    return spawnSync(
      "bash",
      ["-c", `set -euo pipefail\nretry() { "$@"; }\n${shell(overrides)}`],
      {
        encoding: "utf8",
        env: {
          ...process.env,
          CRABBOX_PACKAGE_LOG: packageLog,
          PATH: `${bin}:${process.env.PATH}`,
        },
      },
    );
  }

  async function packageCalls() {
    try {
      return (await readFile(packageLog, "utf8")).trim().split("\n").filter(Boolean);
    } catch (error) {
      if (error?.code === "ENOENT") return [];
      throw error;
    }
  }

  return {
    builderDigest,
    legacyMarkerPath,
    manifestPath,
    minimalDigest,
    ownerUID,
    packageCalls,
    run,
    writeManifest,
  };
}

test("generated readiness contracts are current and bind repository toolchain versions", async () => {
  const generated = spawnSync(process.execPath, ["scripts/generate-linux-readiness.mjs", "--check"], {
    cwd: repoRoot,
    encoding: "utf8",
  });
  assert.equal(generated.status, 0, generated.stderr || generated.stdout);

  const { minimal, builder } = await readProfiles();
  const versions = await readRepoVersions();
  assertProfile(minimal, "linux-minimal");
  assertProfile(builder, "linux-builder");
  assertBuilderSuperset(minimal, builder, versions);

  const drifted = structuredClone(builder);
  drifted.toolchains.find((toolchain) => toolchain.name === "node").version = `${versions.node}.drift`;
  assert.throws(
    () => assertBuilderSuperset(minimal, drifted, versions),
    /must match .node-version/u,
  );

  const weakened = structuredClone(builder);
  weakened.probes.find((probe) => probe.name === "flock").command = "true";
  assert.throws(
    () => assertBuilderSuperset(minimal, weakened, versions),
    /missing exact minimal probe flock/u,
  );
});

test("profile validation enforces the complete strict schema", async () => {
  const { minimal, builder } = await readProfiles();
  for (const mutate of [
    (profile) => {
      profile.extra = true;
    },
    (profile) => {
      profile.description = "";
    },
    (profile) => {
      profile.probes.push({ ...profile.probes[0] });
    },
    (profile) => {
      profile.toolchains = [{ name: "go", version: "1.26.5", source: "go.mod", extra: true }];
    },
  ]) {
    const invalid = structuredClone(builder);
    mutate(invalid);
    assert.throws(() => assertProfile(invalid, "linux-builder"));
  }
  assert.doesNotThrow(() => assertProfile(minimal, "linux-minimal"));
});

test("valid minimal and builder manifests execute zero package-manager commands", async (t) => {
  const fixture = await createShellFixture(t);
  for (const [profile, recipeDigest] of [
    ["linux-minimal", fixture.minimalDigest],
    ["linux-builder", fixture.builderDigest],
  ]) {
    await fixture.writeManifest(profile, recipeDigest);
    const result = fixture.run();
    assert.equal(result.status, 0, result.stderr || result.stdout);
    assert.deepEqual(await fixture.packageCalls(), []);
    const manifest = JSON.parse(await readFile(fixture.manifestPath, "utf8"));
    assert.equal(manifest.profile, profile);
    assert.equal(manifest.recipeDigest, recipeDigest);
  }
});

test("legacy marker plus minimal probes migrates without package-manager commands", async (t) => {
  const fixture = await createShellFixture(t);
  await writeFile(fixture.legacyMarkerPath, "crabbox-devtools-v1\n");
  const result = fixture.run();
  assert.equal(result.status, 0, result.stderr || result.stdout);
  assert.deepEqual(await fixture.packageCalls(), []);
  const manifest = JSON.parse(await readFile(fixture.manifestPath, "utf8"));
  assert.equal(manifest.profile, "linux-minimal");
  assert.equal(manifest.recipeDigest, fixture.minimalDigest);
  assert.match(manifest.inventoryDigest, /^sha256:[0-9a-f]{64}$/u);
});

test("untrusted or stale manifests fall back instead of authorizing readiness", async (t) => {
  const cases = [
    {
      name: "stale digest",
      setup: async (fixture) => {
        await fixture.writeManifest("linux-minimal", `sha256:${"f".repeat(64)}`);
      },
    },
    {
      name: "symlink",
      setup: async (fixture) => {
        const target = `${fixture.manifestPath}.target`;
        await fixture.writeManifest("linux-minimal", fixture.minimalDigest);
        await writeFile(target, await readFile(fixture.manifestPath));
        await rm(fixture.manifestPath);
        await symlink(target, fixture.manifestPath);
      },
    },
    {
      name: "wrong mode",
      setup: async (fixture) => {
        await fixture.writeManifest("linux-minimal", fixture.minimalDigest, { mode: 0o600 });
      },
    },
    {
      name: "wrong owner",
      setup: async (fixture) => {
        await fixture.writeManifest("linux-minimal", fixture.minimalDigest);
      },
      overrides: (fixture) => ({ manifestOwnerUID: String(Number(fixture.ownerUID) + 1) }),
    },
  ];

  for (const testCase of cases) {
    await t.test(testCase.name, async (subtest) => {
      const fixture = await createShellFixture(subtest);
      await testCase.setup(fixture);
      const result = fixture.run(testCase.overrides?.(fixture));
      assert.notEqual(result.status, 0);
      assert.deepEqual(await fixture.packageCalls(), ["apt-get"]);
    });
  }
});
