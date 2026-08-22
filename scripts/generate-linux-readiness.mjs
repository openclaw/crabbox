#!/usr/bin/env node

import { createHash } from "node:crypto";
import { readFile, writeFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const scriptPath = fileURLToPath(import.meta.url);
const repoRoot = resolve(dirname(scriptPath), "..");
const recipeDir = resolve(repoRoot, "recipes/linux/v1");
const defaultManifestPath = "/var/lib/crabbox/readiness/linux.json";
const defaultLegacyMarkerPath = "/var/lib/crabbox/image-ready";
const profileNames = ["linux-minimal", "linux-builder"];
const profileKeys = [
  "schema",
  "profile",
  "description",
  "satisfies",
  "aptPackages",
  "probes",
  "toolchains",
];

export function canonicalJSON(value) {
  if (Array.isArray(value)) {
    return `[${value.map(canonicalJSON).join(",")}]`;
  }
  if (value && typeof value === "object") {
    return `{${Object.keys(value)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonicalJSON(value[key])}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

export function digest(value) {
  return `sha256:${createHash("sha256").update(canonicalJSON(value)).digest("hex")}`;
}

function assertExactKeys(value, expected, context) {
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  if (actual.length !== wanted.length || actual.some((key, index) => key !== wanted[index])) {
    throw new Error(`${context} must contain exactly: ${wanted.join(", ")}`);
  }
}

function assertUniqueStrings(values, context, pattern) {
  if (
    !Array.isArray(values) ||
    values.length === 0 ||
    values.some((value) => typeof value !== "string" || !pattern.test(value)) ||
    new Set(values).size !== values.length
  ) {
    throw new Error(`${context} must contain unique valid names`);
  }
}

export function assertProfile(profile, expectedName) {
  if (!profile || typeof profile !== "object" || Array.isArray(profile)) {
    throw new Error(`${expectedName} must be an object`);
  }
  assertExactKeys(profile, profileKeys, expectedName);
  if (profile.schema !== "crabbox-linux-readiness/v1" || profile.profile !== expectedName) {
    throw new Error(`${expectedName} has an invalid schema or profile name`);
  }
  if (!profileNames.includes(profile.profile)) {
    throw new Error(`${expectedName} is not a supported profile`);
  }
  if (typeof profile.description !== "string" || profile.description.length === 0) {
    throw new Error(`${expectedName} description must be non-empty`);
  }
  assertUniqueStrings(
    profile.satisfies,
    `${expectedName}.satisfies`,
    /^(?:linux-minimal|linux-builder)$/u,
  );
  if (!profile.satisfies.includes(expectedName)) {
    throw new Error(`${expectedName} must satisfy itself`);
  }
  assertUniqueStrings(
    profile.aptPackages,
    `${expectedName}.aptPackages`,
    /^[a-z0-9][a-z0-9+.-]*$/u,
  );

  if (!Array.isArray(profile.probes) || profile.probes.length === 0) {
    throw new Error(`${expectedName}.probes must be a non-empty array`);
  }
  const probeNames = new Set();
  for (const [index, probe] of profile.probes.entries()) {
    if (!probe || typeof probe !== "object" || Array.isArray(probe)) {
      throw new Error(`${expectedName}.probes[${index}] must be an object`);
    }
    assertExactKeys(probe, ["name", "command"], `${expectedName}.probes[${index}]`);
    if (
      typeof probe.name !== "string" ||
      !/^[a-z0-9][a-z0-9-]*$/u.test(probe.name) ||
      probeNames.has(probe.name)
    ) {
      throw new Error(`${expectedName} has an invalid or duplicate probe name`);
    }
    if (typeof probe.command !== "string" || probe.command.length === 0) {
      throw new Error(`${expectedName}.${probe.name} probe command must be non-empty`);
    }
    probeNames.add(probe.name);
  }

  if (!Array.isArray(profile.toolchains)) {
    throw new Error(`${expectedName}.toolchains must be an array`);
  }
  const toolchainNames = new Set();
  for (const [index, toolchain] of profile.toolchains.entries()) {
    if (!toolchain || typeof toolchain !== "object" || Array.isArray(toolchain)) {
      throw new Error(`${expectedName}.toolchains[${index}] must be an object`);
    }
    assertExactKeys(
      toolchain,
      ["name", "version", "source"],
      `${expectedName}.toolchains[${index}]`,
    );
    if (
      typeof toolchain.name !== "string" ||
      !/^[a-z0-9][a-z0-9-]*$/u.test(toolchain.name) ||
      toolchainNames.has(toolchain.name)
    ) {
      throw new Error(`${expectedName} has an invalid or duplicate toolchain name`);
    }
    if (typeof toolchain.version !== "string" || toolchain.version.length === 0) {
      throw new Error(`${expectedName}.${toolchain.name} version must be non-empty`);
    }
    if (typeof toolchain.source !== "string" || toolchain.source.length === 0) {
      throw new Error(`${expectedName}.${toolchain.name} source must be non-empty`);
    }
    toolchainNames.add(toolchain.name);
  }
}

function shellSingleQuote(value) {
  return `'${String(value).replaceAll("'", "'\"'\"'")}'`;
}

function tsSingleQuoted(value) {
  return `'${String(value)
    .replaceAll("\\", "\\\\")
    .replaceAll("'", "\\'")
    .replaceAll("\n", "\\n")
    .replaceAll("\r", "\\r")
    .replaceAll("\t", "\\t")}'`;
}

function effectiveProbeCommands(profile, options) {
  return profile.probes.map((probe) => {
    if (probe.name === "sshd") {
      return `test -x ${shellSingleQuote(options.sshdPath)}`;
    }
    if (probe.name === "ca-certificates") {
      return `test -s ${shellSingleQuote(options.caPath)}`;
    }
    return probe.command;
  });
}

function runtimeInventoryTargets(profile, options) {
  const targets = [];
  for (const probe of profile.probes) {
    if (probe.name === "sshd") {
      targets.push({ kind: "file", name: probe.name, value: options.sshdPath });
      continue;
    }
    if (probe.name === "ca-certificates") {
      targets.push({ kind: "file", name: probe.name, value: options.caPath });
      continue;
    }
    const command = probe.command.match(/^([a-z0-9][a-z0-9+.-]*)\b/u)?.[1];
    if (!command) {
      throw new Error(`${profile.profile}.${probe.name} cannot derive runtime inventory command`);
    }
    targets.push({ kind: "command", name: probe.name, value: command });
    if (probe.name === "git-lfs") {
      targets.push({ kind: "command", name: "git-lfs-binary", value: "git-lfs" });
    }
  }
  return targets;
}

function runtimeProfileVerifier(profile, recipeDigest, options) {
  const functionName = profile.profile.replaceAll("-", "_");
  const probes = effectiveProbeCommands(profile, options)
    .map((command) => `  ${command}`)
    .join(" &&\n");
  const probeEvidence = profile.probes
    .map(
      (probe) =>
        `    printf '%s\\0%s\\0' ${shellSingleQuote(probe.name)} ${shellSingleQuote(probe.command)}`,
    )
    .join("\n");
  const inventoryEvidence = runtimeInventoryTargets(profile, options)
    .map((target) => {
      if (target.kind === "file") {
        return `    printf '%s\\0%s\\0' ${shellSingleQuote(`file:${target.name}`)} ${shellSingleQuote(target.value)}
    ${shellSingleQuote(options.sha256sumPath)} -- ${shellSingleQuote(target.value)}`;
      }
      return `    command_path="$(type -P ${shellSingleQuote(target.value)})"
    test -n "$command_path"
    printf '%s\\0%s\\0' ${shellSingleQuote(`command:${target.name}`)} "$command_path"
    ${shellSingleQuote(options.sha256sumPath)} -- "$command_path"`;
    })
    .join("\n");
  return `crabbox_${functionName}_readiness_probes() {
${probes}
}
crabbox_${functionName}_runtime_inventory_digest() {
  local command_path
  {
    printf '%s\\0%s\\0' 'schema' 'crabbox-linux-runtime-inventory/v1'
    printf '%s\\0%s\\0' 'profile' ${shellSingleQuote(profile.profile)}
    printf '%s\\0%s\\0' 'recipeDigest' ${shellSingleQuote(recipeDigest)}
${probeEvidence}
${inventoryEvidence}
  } | ${shellSingleQuote(options.sha256sumPath)} | ${shellSingleQuote(options.awkPath)} '{print "sha256:" $1}'
}`;
}

export function readinessEvidenceCommand(
  minimal,
  builder,
  minimalDigest,
  builderDigest,
  overrides = {},
) {
  const options = {
    manifestPath: defaultManifestPath,
    sshdPath: "/usr/sbin/sshd",
    caPath: "/etc/ssl/certs/ca-certificates.crt",
    manifestOwnerUID: "0",
    bashPath: "/bin/bash",
    statPath: "/usr/bin/stat",
    catPath: "/usr/bin/cat",
    jqPath: "/usr/bin/jq",
    sha256sumPath: "/usr/bin/sha256sum",
    awkPath: "/usr/bin/awk",
    ...overrides,
  };
  const script = `set -euo pipefail
crabbox_readiness_manifest_path=${shellSingleQuote(options.manifestPath)}
crabbox_readiness_schema='crabbox-linux-readiness/v1'
crabbox_minimal_profile='linux-minimal'
crabbox_minimal_recipe_digest=${shellSingleQuote(minimalDigest)}
crabbox_builder_profile='linux-builder'
crabbox_builder_recipe_digest=${shellSingleQuote(builderDigest)}
${runtimeProfileVerifier(minimal, minimalDigest, options)}
${runtimeProfileVerifier(builder, builderDigest, options)}
test -f "$crabbox_readiness_manifest_path"
test ! -L "$crabbox_readiness_manifest_path"
test "$(${shellSingleQuote(options.statPath)} -c '%u:%a' "$crabbox_readiness_manifest_path" 2>/dev/null)" = "${options.manifestOwnerUID}:644"
manifest_json="$(${shellSingleQuote(options.catPath)} -- "$crabbox_readiness_manifest_path")"
printf '%s' "$manifest_json" | ${shellSingleQuote(options.jqPath)} -e \
  --arg schema "$crabbox_readiness_schema" \
  'type == "object" and
    .schema == $schema and
    (.profile | type == "string") and
    (.recipeDigest | type == "string" and test("^sha256:[0-9a-f]{64}$")) and
    (.inventoryDigest | type == "string" and test("^sha256:[0-9a-f]{64}$")) and
    (keys | sort) == ["inventoryDigest", "profile", "recipeDigest", "schema"]' >/dev/null
profile="$(printf '%s' "$manifest_json" | ${shellSingleQuote(options.jqPath)} -r '.profile')"
recipe_digest="$(printf '%s' "$manifest_json" | ${shellSingleQuote(options.jqPath)} -r '.recipeDigest')"
case "$profile:$recipe_digest" in
  "$crabbox_minimal_profile:$crabbox_minimal_recipe_digest")
    crabbox_linux_minimal_readiness_probes
    inventory_digest="$(crabbox_linux_minimal_runtime_inventory_digest)"
    ;;
  "$crabbox_builder_profile:$crabbox_builder_recipe_digest")
    crabbox_linux_builder_readiness_probes
    inventory_digest="$(crabbox_linux_builder_runtime_inventory_digest)"
    ;;
  *)
    printf '%s\\n' 'readiness manifest profile or recipe digest is unsupported' >&2
    exit 1
    ;;
esac
printf '{"inventoryDigest":"%s","profile":"%s","recipeDigest":"%s","schema":"%s"}\\n' \
  "$inventory_digest" "$profile" "$recipe_digest" "$crabbox_readiness_schema"`;
  return `${shellSingleQuote(options.bashPath)} --noprofile --norc -c ${shellSingleQuote(script)}`;
}

export function minimalBootstrap(minimal, minimalDigest, builderDigest, overrides = {}) {
  const options = {
    manifestPath: defaultManifestPath,
    legacyMarkerPath: defaultLegacyMarkerPath,
    sshdPath: "/usr/sbin/sshd",
    caPath: "/etc/ssl/certs/ca-certificates.crt",
    aptConfigPath: "/etc/apt/apt.conf.d/80-crabbox-retries",
    manifestOwnerUID: "0",
    manifestOwnerSpec: "root:root",
    manifestDirectoryOwnerFlags: "-o root -g root",
    ...overrides,
  };
  const packages = minimal.aptPackages.join(" ");
  const probes = effectiveProbeCommands(minimal, options)
    .map((command) => `  ${command}`)
    .join(" &&\n");
  const packageArgs = minimal.aptPackages.map(shellSingleQuote).join(" ");
  const directoryOwnerFlags = options.manifestDirectoryOwnerFlags
    ? ` ${options.manifestDirectoryOwnerFlags}`
    : "";
  const chownLine = options.manifestOwnerSpec
    ? `  chown ${shellSingleQuote(options.manifestOwnerSpec)} "$manifest_tmp"\n`
    : "";
  return `crabbox_readiness_manifest_path=${shellSingleQuote(options.manifestPath)}
crabbox_legacy_image_marker_path=${shellSingleQuote(options.legacyMarkerPath)}
crabbox_readiness_schema=${shellSingleQuote(minimal.schema)}
crabbox_minimal_profile='linux-minimal'
crabbox_minimal_recipe_digest=${shellSingleQuote(minimalDigest)}
crabbox_builder_profile='linux-builder'
crabbox_builder_recipe_digest=${shellSingleQuote(builderDigest)}
crabbox_readiness_packages=${shellSingleQuote(packages)}
crabbox_readiness_sshd_path=${shellSingleQuote(options.sshdPath)}
crabbox_readiness_ca_path=${shellSingleQuote(options.caPath)}
crabbox_minimal_readiness_probes() {
${probes}
}
crabbox_readiness_manifest_matches() {
  test -f "$crabbox_readiness_manifest_path" &&
  test ! -L "$crabbox_readiness_manifest_path" &&
  test "$(stat -c '%u:%a' "$crabbox_readiness_manifest_path" 2>/dev/null)" = "${options.manifestOwnerUID}:644" &&
  command -v jq >/dev/null &&
  jq -e \
    --arg schema "$crabbox_readiness_schema" \
    --arg minimalProfile "$crabbox_minimal_profile" \
    --arg minimalDigest "$crabbox_minimal_recipe_digest" \
    --arg builderProfile "$crabbox_builder_profile" \
    --arg builderDigest "$crabbox_builder_recipe_digest" \
    'type == "object" and
      .schema == $schema and
      (.inventoryDigest | type == "string" and test("^sha256:[0-9a-f]{64}$")) and
      (
        (.profile == $minimalProfile and .recipeDigest == $minimalDigest) or
        (.profile == $builderProfile and .recipeDigest == $builderDigest)
      ) and
      (keys | sort) == ["inventoryDigest", "profile", "recipeDigest", "schema"]' \
    "$crabbox_readiness_manifest_path" >/dev/null &&
  crabbox_minimal_readiness_probes
}
crabbox_probe_inventory_digest() {
  local command_name command_path
  {
    printf '%s\\n' "$crabbox_minimal_recipe_digest"
    sha256sum "$crabbox_readiness_sshd_path" "$crabbox_readiness_ca_path"
    for command_name in curl git rsync jq tmux flock; do
      command_path="$(command -v "$command_name")"
      printf '%s\\n' "$command_path"
      sha256sum "$command_path"
    done
  } | LC_ALL=C sort | sha256sum | awk '{print "sha256:" $1}'
}
crabbox_package_inventory_digest() {
  dpkg-query -W -f='\${binary:Package}=\${Version}\\n' ${packageArgs} |
    LC_ALL=C sort |
    sha256sum |
    awk '{print "sha256:" $1}'
}
crabbox_write_readiness_manifest() {
  local profile="$1" recipe_digest="$2" inventory_digest="$3"
  local manifest_dir manifest_tmp
  manifest_dir="$(dirname "$crabbox_readiness_manifest_path")"
  install -d -m 0755${directoryOwnerFlags} "$manifest_dir"
  manifest_tmp="$(mktemp "$manifest_dir/.linux.json.XXXXXX")"
  printf '{"inventoryDigest":"%s","profile":"%s","recipeDigest":"%s","schema":"%s"}\\n' \
    "$inventory_digest" \
    "$profile" \
    "$recipe_digest" \
    "$crabbox_readiness_schema" >"$manifest_tmp"
${chownLine}  chmod 0644 "$manifest_tmp"
  mv -f "$manifest_tmp" "$crabbox_readiness_manifest_path"
}
if crabbox_readiness_manifest_matches; then
  echo 'crabbox Linux readiness manifest verified; skipping apt bootstrap'
elif test -f "$crabbox_legacy_image_marker_path" &&
  test ! -L "$crabbox_legacy_image_marker_path" &&
  crabbox_minimal_readiness_probes; then
  crabbox_write_readiness_manifest \
    "$crabbox_minimal_profile" \
    "$crabbox_minimal_recipe_digest" \
    "$(crabbox_probe_inventory_digest)"
  echo 'crabbox legacy image readiness migrated without package-manager work'
else
  cat >${shellSingleQuote(options.aptConfigPath)} <<'APT'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
APT
  retry apt-get update
  retry apt-get install -y --no-install-recommends $crabbox_readiness_packages
  crabbox_minimal_readiness_probes
  crabbox_write_readiness_manifest \
    "$crabbox_minimal_profile" \
    "$crabbox_minimal_recipe_digest" \
    "$(crabbox_package_inventory_digest)"
fi`;
}

function goSource(profiles, bootstrap, evidenceCommand) {
  return `// Code generated by scripts/generate-linux-readiness.mjs; DO NOT EDIT.

package cli

const linuxReadinessManifestPath = ${JSON.stringify(defaultManifestPath)}
const linuxMinimalRecipeDigest = ${JSON.stringify(profiles.minimal.digest)}
const linuxBuilderRecipeDigest = ${JSON.stringify(profiles.builder.digest)}
const linuxReadinessEvidenceCommand = ${JSON.stringify(evidenceCommand)}
const linuxMinimalReadinessBootstrap = \`${bootstrap}\`
`;
}

function tsSource(profiles, bootstrap) {
  return `// Code generated by scripts/generate-linux-readiness.mjs; DO NOT EDIT.

export const linuxReadinessManifestPath = ${JSON.stringify(defaultManifestPath)};
export const linuxMinimalRecipeDigest =
  ${JSON.stringify(profiles.minimal.digest)};
export const linuxBuilderRecipeDigest =
  ${JSON.stringify(profiles.builder.digest)};
export const linuxMinimalReadinessBootstrap =
  ${tsSingleQuoted(bootstrap)};
`;
}

async function readProfile(name) {
  return JSON.parse(await readFile(resolve(recipeDir, `${name}.json`), "utf8"));
}

async function readRepoToolchainVersions() {
  const goMod = await readFile(resolve(repoRoot, "go.mod"), "utf8");
  const nodeVersion = (await readFile(resolve(repoRoot, ".node-version"), "utf8")).trim();
  const goMatch = goMod.match(/^toolchain go(\S+)$/mu);
  if (!goMatch || nodeVersion.length === 0) {
    throw new Error("repository Go or Node version source is missing");
  }
  return { go: goMatch[1], node: nodeVersion };
}

export function assertBuilderSuperset(minimal, builder, versions) {
  if (!builder.satisfies.includes("linux-minimal")) {
    throw new Error("linux-builder must explicitly satisfy linux-minimal");
  }
  for (const required of minimal.aptPackages) {
    if (!builder.aptPackages.includes(required)) {
      throw new Error(`linux-builder is missing minimal package ${required}`);
    }
  }
  for (const required of minimal.probes) {
    if (
      !builder.probes.some(
        (probe) => probe.name === required.name && probe.command === required.command,
      )
    ) {
      throw new Error(`linux-builder is missing exact minimal probe ${required.name}`);
    }
  }
  for (const [name, version, source] of [
    ["go", versions.go, "go.mod toolchain"],
    ["node", versions.node, ".node-version"],
  ]) {
    const toolchain = builder.toolchains.find((candidate) => candidate.name === name);
    if (!toolchain || toolchain.version !== version || toolchain.source !== source) {
      throw new Error(`linux-builder ${name} must match ${source}`);
    }
  }
}

async function update(path, content, check) {
  let existing = "";
  try {
    existing = await readFile(path, "utf8");
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }
  if (existing === content) return;
  if (check) {
    throw new Error(`${path.slice(repoRoot.length + 1)} is stale`);
  }
  await writeFile(path, content);
}

async function main() {
  const check = process.argv.includes("--check");
  const minimal = await readProfile("linux-minimal");
  const builder = await readProfile("linux-builder");
  const versions = await readRepoToolchainVersions();
  assertProfile(minimal, "linux-minimal");
  assertProfile(builder, "linux-builder");
  assertBuilderSuperset(minimal, builder, versions);

  const profiles = {
    minimal: { value: minimal, digest: digest(minimal) },
    builder: { value: builder, digest: digest(builder) },
  };
  const bootstrap = minimalBootstrap(
    minimal,
    profiles.minimal.digest,
    profiles.builder.digest,
  );
  const evidenceCommand = readinessEvidenceCommand(
    minimal,
    builder,
    profiles.minimal.digest,
    profiles.builder.digest,
  );

  await update(
    resolve(repoRoot, "internal/cli/linux_readiness_generated.go"),
    goSource(profiles, bootstrap, evidenceCommand),
    check,
  );
  await update(
    resolve(repoRoot, "worker/src/linux-readiness.generated.ts"),
    tsSource(profiles, bootstrap),
    check,
  );
}

if (process.argv[1] && resolve(process.argv[1]) === scriptPath) {
  await main();
}
