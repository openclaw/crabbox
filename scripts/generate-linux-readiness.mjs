#!/usr/bin/env node

import { createHash } from "node:crypto";
import { realpathSync } from "node:fs";
import { chmod, readFile, writeFile } from "node:fs/promises";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const scriptPath = fileURLToPath(import.meta.url);
const repoRoot = resolve(dirname(scriptPath), "..");
export const readinessSchema = "crabbox-linux-readiness/v1";
const profileNames = ["linux-minimal", "linux-builder"];
const profileKeys = ["schema", "profile", "description", "shell", "inherits", "aptPackages", "probes"];
const manifestKeys = ["profile", "recipeDigest", "schema"];
const minimalPackageContract = [
  "ca-certificates", "curl", "git", "jq", "openssh-server", "rsync", "tmux", "util-linux",
];
const builderAdditionalPackageContract = ["build-essential", "git-lfs", "pkg-config", "python3", "python3-venv"];
const builderVenvProbeCommand = `python3 -c 'with __import__("tempfile").TemporaryDirectory() as directory: __import__("venv").EnvBuilder(with_pip=True).create(directory + "/venv"); __import__("subprocess").run([directory + "/venv/bin/python", "-m", "pip", "--version"], check=True)'`;
const minimalProbeContract = new Map([
  ["ca-certificates", "test -s /etc/ssl/certs/ca-certificates.crt"],
  ["curl", "curl --version"],
  ["flock", "flock --version"],
  ["git", "git --version"],
  ["jq", "jq --version"],
  ["rsync", "rsync --version"],
  ["sshd", "test -x /usr/sbin/sshd"],
  ["tmux", "tmux -V"],
]);
const builderAdditionalProbeContract = new Map([
  ["compiler", "cc --version"],
  ["git-lfs", "git lfs version"],
  ["make", "make --version"],
  ["pkg-config", "pkg-config --version"],
  ["python3", "python3 --version"],
  ["python3-venv", builderVenvProbeCommand],
]);
const allowedSchemaKeys = new Set([
  "$schema", "$id", "title", "description", "type", "additionalProperties", "required",
  "properties", "const", "enum", "minLength", "pattern", "minItems", "uniqueItems", "items",
]);

export function canonicalJSON(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(",")}]`;
  if (value !== null && typeof value === "object") {
    return `{${Object.keys(value).sort().map((key) => `${JSON.stringify(key)}:${canonicalJSON(value[key])}`).join(",")}}`;
  }
  return JSON.stringify(value);
}

export function parseStrictJSON(source, label = "JSON") {
  let offset = 0;
  const fail = (message) => {
    throw new Error(`${label}: ${message} at byte ${offset}`);
  };
  const whitespace = () => {
    while (offset < source.length && /[\t\n\r ]/u.test(source[offset])) offset += 1;
  };
  const string = () => {
    const start = offset++;
    while (offset < source.length) {
      const character = source[offset++];
      if (character === "\\") {
        offset += 1;
      } else if (character === '"') {
        try {
          return JSON.parse(source.slice(start, offset));
        } catch {
          fail("invalid string");
        }
      }
    }
    fail("unterminated string");
  };
  const value = () => {
    whitespace();
    if (source[offset] === '"') return string();
    if (source[offset] === "{") {
      offset += 1;
      whitespace();
      const object = {};
      const seen = new Set();
      if (source[offset] === "}") {
        offset += 1;
        return object;
      }
      while (offset < source.length) {
        if (source[offset] !== '"') fail("expected an object key");
        const key = string();
        if (seen.has(key)) fail(`duplicate key ${JSON.stringify(key)}`);
        seen.add(key);
        whitespace();
        if (source[offset++] !== ":") fail("expected a colon");
        Object.defineProperty(object, key, { value: value(), enumerable: true, configurable: true, writable: true });
        whitespace();
        const delimiter = source[offset++];
        if (delimiter === "}") return object;
        if (delimiter !== ",") fail("expected an object delimiter");
        whitespace();
      }
      fail("unterminated object");
    }
    if (source[offset] === "[") {
      offset += 1;
      whitespace();
      const array = [];
      if (source[offset] === "]") {
        offset += 1;
        return array;
      }
      while (offset < source.length) {
        array.push(value());
        whitespace();
        const delimiter = source[offset++];
        if (delimiter === "]") return array;
        if (delimiter !== ",") fail("expected an array delimiter");
        whitespace();
      }
      fail("unterminated array");
    }
    const match = source.slice(offset).match(/^(?:true|false|null|-?(?:0|[1-9]\d*)(?:\.\d+)?(?:[eE][+-]?\d+)?)/u);
    if (!match) fail("expected a JSON value");
    offset += match[0].length;
    return JSON.parse(match[0]);
  };
  const result = value();
  whitespace();
  if (offset !== source.length) fail("trailing JSON content");
  return result;
}

function assertExactKeys(value, expected, context) {
  const actual = Object.keys(value).sort();
  const wanted = [...expected].sort();
  if (actual.length !== wanted.length || actual.some((key, index) => key !== wanted[index])) {
    throw new Error(`${context} must contain exactly: ${wanted.join(", ")}`);
  }
}

export function validateJSONSchema(value, schema, context = "value") {
  if (!schema || typeof schema !== "object" || Array.isArray(schema)) {
    throw new Error(`${context} has an invalid JSON schema`);
  }
  for (const key of Object.keys(schema)) {
    if (!allowedSchemaKeys.has(key)) throw new Error(`${context} uses unsupported schema keyword ${key}`);
  }
  if ("const" in schema && value !== schema.const) throw new Error(`${context} must equal ${JSON.stringify(schema.const)}`);
  if (schema.enum && !schema.enum.includes(value)) throw new Error(`${context} is not an allowed value`);
  if (schema.type === "object") {
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error(`${context} must be an object`);
    for (const key of schema.required ?? []) {
      if (!Object.hasOwn(value, key)) throw new Error(`${context}.${key} is required`);
    }
    for (const [key, item] of Object.entries(value)) {
      if (!Object.hasOwn(schema.properties ?? {}, key)) {
        if (schema.additionalProperties === false) throw new Error(`${context}.${key} is not allowed`);
      } else {
        validateJSONSchema(item, schema.properties[key], `${context}.${key}`);
      }
    }
  } else if (schema.type === "array") {
    if (!Array.isArray(value)) throw new Error(`${context} must be an array`);
    if (schema.minItems !== undefined && value.length < schema.minItems) throw new Error(`${context} has too few items`);
    if (schema.uniqueItems && new Set(value.map(canonicalJSON)).size !== value.length) {
      throw new Error(`${context} contains duplicate items`);
    }
    value.forEach((item, index) => validateJSONSchema(item, schema.items, `${context}[${index}]`));
  } else if (schema.type === "string") {
    if (typeof value !== "string") throw new Error(`${context} must be a string`);
    if (schema.minLength !== undefined && value.length < schema.minLength) throw new Error(`${context} must not be empty`);
    if (schema.pattern && !new RegExp(schema.pattern, "u").test(value)) throw new Error(`${context} has an invalid format`);
  } else if (schema.type !== undefined) {
    throw new Error(`${context} uses unsupported schema type ${schema.type}`);
  }
}

export function validateSchemaDefinition(schema, kind) {
  if (!schema || typeof schema !== "object" || Array.isArray(schema)) throw new Error(`${kind} schema must be an object`);
  for (const key of Object.keys(schema)) {
    if (!allowedSchemaKeys.has(key)) throw new Error(`${kind} schema uses unsupported keyword ${key}`);
  }
  if (schema.$schema !== "https://json-schema.org/draft/2020-12/schema") {
    throw new Error(`${kind} schema must declare JSON Schema draft 2020-12`);
  }
  if (schema.type !== "object" || schema.additionalProperties !== false) {
    throw new Error(`${kind} schema must reject unknown object fields`);
  }
  const keys = kind === "profile" ? profileKeys : manifestKeys;
  assertExactKeys(schema.properties ?? {}, keys, `${kind} schema properties`);
  if (canonicalJSON([...schema.required ?? []].sort()) !== canonicalJSON([...keys].sort())) {
    throw new Error(`${kind} schema must require every contract field`);
  }
  if (schema.properties.schema?.const !== readinessSchema) throw new Error(`${kind} schema has an invalid contract version`);
  if (canonicalJSON([...(schema.properties.profile?.enum ?? [])].sort()) !== canonicalJSON([...profileNames].sort())) {
    throw new Error(`${kind} schema has unsupported profiles`);
  }
  if (kind === "manifest" && schema.properties.recipeDigest?.pattern !== "^sha256:[0-9a-f]{64}$") {
    throw new Error("manifest schema must require a lowercase SHA-256 digest");
  }
  if (kind === "profile" && schema.properties.shell?.const !== "posix") {
    throw new Error("profile schema must require the supported POSIX shell contract");
  }
}

function assertSortedUnique(values, context) {
  if (values.some((value, index) => index > 0 && values[index - 1] >= value)) {
    throw new Error(`${context} must be sorted and unique`);
  }
}

export function assertProfile(profile, expectedName, profileSchema) {
  validateJSONSchema(profile, profileSchema, expectedName);
  if (profile.profile !== expectedName) throw new Error(`${expectedName} recipe has the wrong profile name`);
  assertSortedUnique(profile.inherits, `${expectedName}.inherits`);
  assertSortedUnique(profile.aptPackages, `${expectedName}.aptPackages`);
  assertSortedUnique(profile.probes.map((probe) => probe.name), `${expectedName}.probes`);
  for (const probe of profile.probes) {
    const safeCommand = /^[A-Za-z0-9_./ :'+-]+$/u.test(probe.command)
      || (probe.name === "python3-venv" && probe.command === builderVenvProbeCommand);
    if (!safeCommand || probe.command.includes("  ")) {
      throw new Error(`${expectedName}.${probe.name} contains unsupported or unsafe shell syntax`);
    }
    if ((probe.command.match(/'/gu) ?? []).length % 2 !== 0) {
      throw new Error(`${expectedName}.${probe.name} contains an unmatched shell quote`);
    }
  }
}

export function assertInheritance(profiles) {
  const visiting = new Set();
  const visited = new Set();
  const visit = (name) => {
    if (visiting.has(name)) throw new Error(`profile inheritance cycle involving ${name}`);
    if (visited.has(name)) return;
    const profile = profiles.get(name);
    if (!profile) throw new Error(`unknown inherited profile ${name}`);
    visiting.add(name);
    for (const inherited of profile.inherits) {
      visit(inherited);
      const parent = profiles.get(inherited);
      for (const required of parent.aptPackages) {
        if (!profile.aptPackages.includes(required)) throw new Error(`${name} is missing inherited package ${required}`);
      }
      for (const required of parent.probes) {
        if (!profile.probes.some((probe) => probe.name === required.name && probe.command === required.command)) {
          throw new Error(`${name} is missing exact inherited probe ${required.name}`);
        }
      }
    }
    visiting.delete(name);
    visited.add(name);
  };
  for (const name of profiles.keys()) visit(name);
  if (profiles.get("linux-minimal")?.inherits.length !== 0) throw new Error("linux-minimal must not inherit another profile");
  if (canonicalJSON(profiles.get("linux-builder")?.inherits) !== '["linux-minimal"]') {
    throw new Error("linux-builder must directly inherit linux-minimal");
  }
}

export function assertFrozenContract(minimal, builder) {
  if (canonicalJSON(minimal.aptPackages) !== canonicalJSON(minimalPackageContract)) {
    throw new Error("linux-minimal must contain exactly the frozen v1 baseline packages");
  }
  const expectedBuilderPackages = [...minimalPackageContract, ...builderAdditionalPackageContract].sort();
  if (canonicalJSON(builder.aptPackages) !== canonicalJSON(expectedBuilderPackages)) {
    throw new Error("linux-builder must contain exactly the frozen v1 generic builder packages");
  }
  for (const [profile, expected] of [
    [minimal, minimalProbeContract],
    [builder, new Map([...minimalProbeContract, ...builderAdditionalProbeContract])],
  ]) {
    const actual = new Map(profile.probes.map((probe) => [probe.name, probe.command]));
    if (actual.size !== expected.size) throw new Error(`${profile.profile} must contain exactly its frozen v1 functional probes`);
    for (const [name, command] of expected) {
      if (actual.get(name) !== command) throw new Error(`${profile.profile} must contain the exact v1 ${name} probe`);
    }
  }
}

export function digest(profile) {
  const executableContract = {
    schema: profile.schema,
    profile: profile.profile,
    shell: profile.shell,
    inherits: profile.inherits,
    aptPackages: profile.aptPackages,
    probes: profile.probes,
  };
  return `sha256:${createHash("sha256").update(canonicalJSON(executableContract)).digest("hex")}`;
}

export function manifestFor(profile, recipeDigest) {
  return { profile, recipeDigest, schema: readinessSchema };
}

function shellQuote(value) {
  return `'${String(value).replaceAll("'", "'\"'\"'")}'`;
}

function probeCommands(profile, options, omit = new Set()) {
  return profile.probes.filter((probe) => !omit.has(probe.name)).map((probe) => {
    if (probe.name === "sshd") return `test -x ${shellQuote(options.sshdPath)}`;
    if (probe.name === "ca-certificates") return `test -s ${shellQuote(options.caPath)}`;
    return `command ${probe.command} >/dev/null 2>&1`;
  });
}

function shellDefaults(overrides = {}) {
  return {
    manifestPath: "/var/lib/crabbox-readiness/linux.json",
    legacyMarkerPath: "/var/lib/crabbox/image-ready",
    systemPath: "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
    sshdPath: "/usr/sbin/sshd",
    caPath: "/etc/ssl/certs/ca-certificates.crt",
    aptConfigPath: "/etc/apt/apt.conf.d/80-crabbox-retries",
    trustRoot: "/",
    ownerUID: "0",
    ownerGID: "0",
    performChown: true,
    ...overrides,
  };
}

export function readinessFunctions(minimal, builder, overrides = {}) {
  const options = shellDefaults(overrides);
  const minimalDigest = digest(minimal);
  const builderDigest = digest(builder);
  const minimalPayload = canonicalJSON(manifestFor("linux-minimal", minimalDigest));
  const builderPayload = canonicalJSON(manifestFor("linux-builder", builderDigest));
  const minimalBytes = `${minimalPayload}\n`;
  const builderBytes = `${builderPayload}\n`;
  const minimalHash = createHash("sha256").update(minimalBytes).digest("hex");
  const builderHash = createHash("sha256").update(builderBytes).digest("hex");
  const minimalNames = new Set(minimal.probes.map((probe) => probe.name));
  const minimalProbes = probeCommands(minimal, options).map((command) => `  ${command}`).join(" &&\n");
  const builderProbes = probeCommands(builder, options, minimalNames).map((command) => `  ${command}`).join(" &&\n");
  const owner = shellQuote(`${options.ownerUID}:${options.ownerGID}`);
  const chownLine = options.performChown ? `  chown ${owner} "$crabbox_temp_path" || return 1\n` : "";
  const directoryChown = options.performChown ? `    chown ${owner} "$crabbox_directory" || return 1\n` : "";
  const markerDirectoryChown = options.performChown ? `    chown ${owner} "$crabbox_marker_parent" || return 1\n` : "";
  return `crabbox_readiness_manifest_path=${shellQuote(options.manifestPath)}
crabbox_legacy_image_marker_path=${shellQuote(options.legacyMarkerPath)}
crabbox_readiness_trust_root=${shellQuote(options.trustRoot)}
crabbox_readiness_owner_uid=${shellQuote(options.ownerUID)}
crabbox_readiness_owner_gid=${shellQuote(options.ownerGID)}
crabbox_readiness_system_path=${shellQuote(options.systemPath)}
crabbox_minimal_manifest_payload=${shellQuote(minimalPayload)}
crabbox_minimal_manifest_sha256=${shellQuote(minimalHash)}
crabbox_builder_manifest_payload=${shellQuote(builderPayload)}
crabbox_builder_manifest_sha256=${shellQuote(builderHash)}

crabbox_minimal_readiness_probes() (
  PATH="$crabbox_readiness_system_path"
  export PATH
${minimalProbes}
)

crabbox_builder_additional_readiness_probes() (
  PATH="$crabbox_readiness_system_path"
  export PATH
${builderProbes}
)

crabbox_builder_readiness_probes() {
  crabbox_minimal_readiness_probes && crabbox_builder_additional_readiness_probes
}

crabbox_readiness_stat() {
  command -v stat >/dev/null 2>&1 || return 1
  crabbox_stat_result="$(LC_ALL=C LANG=C stat -c '%F:%u:%g:%a:%s' -- "$1" 2>/dev/null)" || return 1
  IFS=: read -r crabbox_stat_type crabbox_stat_uid crabbox_stat_gid crabbox_stat_mode crabbox_stat_size crabbox_stat_extra <<CRABBOX_READINESS_STAT
$crabbox_stat_result
CRABBOX_READINESS_STAT
  test -z "$crabbox_stat_extra" || return 1
  case "$crabbox_stat_uid:$crabbox_stat_gid:$crabbox_stat_mode:$crabbox_stat_size" in
    *[!0-9:]*|*::*|:) return 1 ;;
  esac
}

crabbox_trusted_directory() {
  test -d "$1" && test ! -L "$1" || return 1
  crabbox_readiness_stat "$1" || return 1
  test "$crabbox_stat_type" = 'directory' || return 1
  test "$crabbox_stat_uid" = "$crabbox_readiness_owner_uid" || return 1
  test "$crabbox_stat_gid" = "$crabbox_readiness_owner_gid" || return 1
  case "$crabbox_stat_mode" in
    *[2367][0-7]|*[0-7][2367]) return 1 ;;
  esac
}

crabbox_trusted_parents() {
  crabbox_parent_path="$1"
  crabbox_parent_root="$crabbox_readiness_trust_root"
  test "$crabbox_parent_root" = '/' || crabbox_parent_root="\${crabbox_parent_root%/}"
  case "$crabbox_parent_path" in
    "$crabbox_parent_root") crabbox_parent_rest='' ;;
    "$crabbox_parent_root"/*) crabbox_parent_rest="\${crabbox_parent_path#"$crabbox_parent_root"/}" ;;
    *) test "$crabbox_parent_root" = '/' || return 1; crabbox_parent_rest="\${crabbox_parent_path#/}" ;;
  esac
  crabbox_parent_current="$crabbox_parent_root"
  crabbox_trusted_directory "$crabbox_parent_current" || return 1
  while test -n "$crabbox_parent_rest"; do
    case "$crabbox_parent_rest" in
      */*) crabbox_parent_part="\${crabbox_parent_rest%%/*}"; crabbox_parent_rest="\${crabbox_parent_rest#*/}" ;;
      *) crabbox_parent_part="$crabbox_parent_rest"; crabbox_parent_rest='' ;;
    esac
    test -n "$crabbox_parent_part" && test "$crabbox_parent_part" != '.' && test "$crabbox_parent_part" != '..' || return 1
    if test "$crabbox_parent_current" = '/'; then
      crabbox_parent_current="/$crabbox_parent_part"
    else
      crabbox_parent_current="$crabbox_parent_current/$crabbox_parent_part"
    fi
    crabbox_trusted_directory "$crabbox_parent_current" || return 1
  done
}

crabbox_trusted_file() {
  crabbox_file_path="$1"
  crabbox_file_mode="$2"
  crabbox_file_parent="\${crabbox_file_path%/*}"
  test -n "$crabbox_file_parent" || crabbox_file_parent='/'
  crabbox_trusted_parents "$crabbox_file_parent" || return 1
  test -f "$crabbox_file_path" && test ! -L "$crabbox_file_path" || return 1
  crabbox_readiness_stat "$crabbox_file_path" || return 1
  case "$crabbox_stat_type" in
    'regular file'|'regular empty file') ;;
    *) return 1 ;;
  esac
  test "$crabbox_stat_uid" = "$crabbox_readiness_owner_uid" || return 1
  test "$crabbox_stat_gid" = "$crabbox_readiness_owner_gid" || return 1
  test "$crabbox_stat_mode" = "$crabbox_file_mode" || return 1
  test "$crabbox_stat_size" -le 4096 || return 1
}

crabbox_readiness_file_sha256() {
  command -v sha256sum >/dev/null 2>&1 || return 1
  crabbox_sha_result="$(sha256sum -- "$1" 2>/dev/null)" || return 1
  crabbox_file_sha256="\${crabbox_sha_result%% *}"
  test "\${#crabbox_file_sha256}" -eq 64 || return 1
  case "$crabbox_file_sha256" in *[!0-9a-f]*) return 1 ;; esac
}

crabbox_readiness_manifest_metadata_matches() {
  crabbox_trusted_file "$crabbox_readiness_manifest_path" 644 || return 1
  crabbox_readiness_file_sha256 "$crabbox_readiness_manifest_path" || return 1
  test "$crabbox_file_sha256" = "$crabbox_minimal_manifest_sha256" ||
    test "$crabbox_file_sha256" = "$crabbox_builder_manifest_sha256"
}

crabbox_readiness_manifest_matches() {
  crabbox_readiness_manifest_metadata_matches || return 1
  if test "$crabbox_file_sha256" = "$crabbox_minimal_manifest_sha256"; then
    crabbox_minimal_readiness_probes
  elif test "$crabbox_file_sha256" = "$crabbox_builder_manifest_sha256"; then
    crabbox_builder_readiness_probes
  else
    return 1
  fi
}

crabbox_ensure_directory() {
  crabbox_directory="$1"
  crabbox_directory_parent="\${crabbox_directory%/*}"
  test -n "$crabbox_directory_parent" || crabbox_directory_parent='/'
  crabbox_trusted_parents "$crabbox_directory_parent" || return 1
  if test -e "$crabbox_directory" || test -L "$crabbox_directory"; then
    crabbox_trusted_directory "$crabbox_directory" || return 1
  else
    mkdir -m 0755 -- "$crabbox_directory" || return 1
${directoryChown}    crabbox_trusted_directory "$crabbox_directory" || return 1
  fi
}

crabbox_atomic_write() (
  crabbox_destination="$1"
  crabbox_contents="$2"
  crabbox_destination_parent="\${crabbox_destination%/*}"
  test -n "$crabbox_destination_parent" || crabbox_destination_parent='/'
  crabbox_trusted_parents "$crabbox_destination_parent" || return 1
  if test -L "$crabbox_destination" || { test -e "$crabbox_destination" && test ! -f "$crabbox_destination"; }; then
    return 1
  fi
  crabbox_temp_path=''
  trap 'if test -n "$crabbox_temp_path"; then rm -f -- "$crabbox_temp_path"; fi' EXIT HUP INT TERM
  crabbox_temp_path="$(mktemp "$crabbox_destination_parent/.linux-readiness.XXXXXXXXXX")" || return 1
${chownLine}  crabbox_trusted_file "$crabbox_temp_path" 600 || return 1
  printf '%s\\n' "$crabbox_contents" >"$crabbox_temp_path" || return 1
  sync "$crabbox_temp_path" 2>/dev/null || sync -f "$crabbox_temp_path" 2>/dev/null || sync || return 1
  chmod 0644 "$crabbox_temp_path" || return 1
  crabbox_trusted_file "$crabbox_temp_path" 644 || return 1
  crabbox_trusted_parents "$crabbox_destination_parent" || return 1
  test ! -L "$crabbox_destination" || return 1
  mv -f -- "$crabbox_temp_path" "$crabbox_destination" || return 1
  crabbox_temp_path=''
  crabbox_trusted_file "$crabbox_destination" 644 || return 1
  sync "$crabbox_destination_parent" 2>/dev/null || sync -f "$crabbox_destination_parent" 2>/dev/null || sync || return 1
)

crabbox_write_readiness_manifest() {
  crabbox_manifest_directory="\${crabbox_readiness_manifest_path%/*}"
  crabbox_ensure_directory "$crabbox_manifest_directory" || return 1
  crabbox_atomic_write "$crabbox_readiness_manifest_path" "$1" || return 1
  crabbox_readiness_manifest_metadata_matches || return 1
}

crabbox_legacy_image_marker_parent_safe() {
  crabbox_marker_parent="\${crabbox_legacy_image_marker_path%/*}"
  crabbox_marker_ancestor="\${crabbox_marker_parent%/*}"
  test -n "$crabbox_marker_ancestor" || crabbox_marker_ancestor='/'
  crabbox_trusted_parents "$crabbox_marker_ancestor" || return 1
  test -d "$crabbox_marker_parent" && test ! -L "$crabbox_marker_parent" || return 1
  crabbox_readiness_stat "$crabbox_marker_parent" || return 1
  test "$crabbox_stat_type" = 'directory' || return 1
  case "$crabbox_stat_mode" in
    *[2367][0-7]|*[0-7][2367]) return 1 ;;
  esac
}

crabbox_ensure_legacy_image_marker_parent() {
  crabbox_marker_parent="\${crabbox_legacy_image_marker_path%/*}"
  crabbox_marker_ancestor="\${crabbox_marker_parent%/*}"
  test -n "$crabbox_marker_ancestor" || crabbox_marker_ancestor='/'
  crabbox_trusted_parents "$crabbox_marker_ancestor" || return 1
  if test ! -e "$crabbox_marker_parent" && test ! -L "$crabbox_marker_parent"; then
    mkdir -m 0755 -- "$crabbox_marker_parent" || return 1
${markerDirectoryChown}    crabbox_trusted_directory "$crabbox_marker_parent" || return 1
    test "$crabbox_stat_mode" = 755 || return 1
  fi
  crabbox_legacy_image_marker_parent_safe || return 1
}

crabbox_legacy_image_marker_trusted() {
  # The runtime user may own this parent; the marker is only a compatibility hint.
  crabbox_legacy_image_marker_parent_safe || return 1
  test -f "$crabbox_legacy_image_marker_path" && test ! -L "$crabbox_legacy_image_marker_path" || return 1
  crabbox_readiness_stat "$crabbox_legacy_image_marker_path" || return 1
  case "$crabbox_stat_type" in
    'regular file'|'regular empty file') ;;
    *) return 1 ;;
  esac
  test "$crabbox_stat_uid" = "$crabbox_readiness_owner_uid" || return 1
  test "$crabbox_stat_gid" = "$crabbox_readiness_owner_gid" || return 1
  test "$crabbox_stat_mode" = 644 || return 1
  test "$crabbox_stat_size" -le 4096 || return 1
  crabbox_readiness_file_sha256 "$crabbox_legacy_image_marker_path" || return 1
  test "$crabbox_file_sha256" = '${createHash("sha256").update("crabbox-devtools-v1\n").digest("hex")}'
}

crabbox_write_legacy_image_marker() (
  crabbox_ensure_legacy_image_marker_parent || return 1
  crabbox_marker_parent="\${crabbox_legacy_image_marker_path%/*}"
  crabbox_manifest_directory="\${crabbox_readiness_manifest_path%/*}"
  crabbox_trusted_parents "$crabbox_manifest_directory" || return 1
  crabbox_manifest_device="$(stat -c '%d' -- "$crabbox_manifest_directory" 2>/dev/null)" || return 1
  crabbox_marker_device="$(stat -c '%d' -- "$crabbox_marker_parent" 2>/dev/null)" || return 1
  case "$crabbox_manifest_device:$crabbox_marker_device" in *[!0-9:]*|*::*|:) return 1 ;; esac
  test "$crabbox_manifest_device" = "$crabbox_marker_device" || return 1
  if test -L "$crabbox_legacy_image_marker_path" || { test -e "$crabbox_legacy_image_marker_path" && test ! -f "$crabbox_legacy_image_marker_path"; }; then
    return 1
  fi
  crabbox_temp_path=''
  trap 'if test -n "$crabbox_temp_path"; then rm -f -- "$crabbox_temp_path"; fi' EXIT HUP INT TERM
  # Build bytes only in the root-controlled namespace; same-device rename never follows the marker destination.
  crabbox_temp_path="$(mktemp "$crabbox_manifest_directory/.linux-readiness-marker.XXXXXXXXXX")" || return 1
${chownLine}  crabbox_trusted_file "$crabbox_temp_path" 600 || return 1
  printf '%s\\n' 'crabbox-devtools-v1' >"$crabbox_temp_path" || return 1
  sync "$crabbox_temp_path" 2>/dev/null || sync -f "$crabbox_temp_path" 2>/dev/null || sync || return 1
  chmod 0644 "$crabbox_temp_path" || return 1
  crabbox_trusted_file "$crabbox_temp_path" 644 || return 1
  crabbox_legacy_image_marker_parent_safe || return 1
  test ! -L "$crabbox_legacy_image_marker_path" || return 1
  mv -fT -- "$crabbox_temp_path" "$crabbox_legacy_image_marker_path" || return 1
  crabbox_temp_path=''
  crabbox_legacy_image_marker_trusted || return 1
  sync "$crabbox_marker_parent" 2>/dev/null || sync -f "$crabbox_marker_parent" 2>/dev/null || sync || return 1
)`;
}

export function minimalBootstrap(minimal, builder, overrides = {}) {
  const options = shellDefaults(overrides);
  return `${readinessFunctions(minimal, builder, overrides)}

crabbox_readiness_packages=${shellQuote(minimal.aptPackages.join(" "))}

if crabbox_readiness_manifest_matches; then
  echo 'crabbox Linux readiness manifest verified; skipping apt bootstrap'
elif test ! -e "$crabbox_readiness_manifest_path" &&
  test ! -L "$crabbox_readiness_manifest_path" &&
  crabbox_legacy_image_marker_trusted && crabbox_minimal_readiness_probes; then
  crabbox_write_readiness_manifest "$crabbox_minimal_manifest_payload"
  echo 'crabbox legacy image readiness migrated without package-manager work'
else
  cat >${shellQuote(options.aptConfigPath)} <<'CRABBOX_APT'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
CRABBOX_APT
  retry apt-get update
  retry apt-get install -y --no-install-recommends $crabbox_readiness_packages
  crabbox_minimal_readiness_probes
  crabbox_write_readiness_manifest "$crabbox_minimal_manifest_payload"
  crabbox_write_legacy_image_marker
fi`;
}

export function producerSource(minimal, builder, overrides = {}) {
  return `#!/usr/bin/env bash
# Code generated by scripts/generate-linux-readiness.mjs; DO NOT EDIT.
set -euo pipefail

if test "\${1:-}" = '--print-packages'; then
  test "$#" -eq 2 || { echo 'usage: linux-readiness.generated.sh --print-packages PROFILE' >&2; exit 2; }
  case "$2" in
    linux-minimal) printf '%s\\n' ${shellQuote(minimal.aptPackages.join(" "))} ;;
    linux-builder) printf '%s\\n' ${shellQuote(builder.aptPackages.join(" "))} ;;
    *) echo 'unknown Linux readiness profile' >&2; exit 2 ;;
  esac
  exit 0
fi

if test "$(id -u)" -ne 0; then
  command -v sudo >/dev/null 2>&1 || { echo 'Linux readiness producer requires root' >&2; exit 1; }
  exec sudo -H bash "$0" "$@"
fi

if test "\${1:-}" = '--install'; then
  install -d -m 0755 -o root -g root /usr/local/libexec/crabbox
  install -m 0755 -o root -g root "$0" /usr/local/libexec/crabbox/linux-readiness.generated.sh
  exit 0
fi
test "$#" -eq 0 || { echo 'usage: linux-readiness.generated.sh [--install|--print-packages PROFILE]' >&2; exit 2; }

${readinessFunctions(minimal, builder, overrides)}

if ! crabbox_minimal_readiness_probes; then
  echo 'Linux readiness producer: minimal capability proof failed' >&2
  exit 1
fi
if crabbox_builder_additional_readiness_probes; then
  crabbox_write_readiness_manifest "$crabbox_builder_manifest_payload"
else
  crabbox_write_readiness_manifest "$crabbox_minimal_manifest_payload"
fi
crabbox_write_legacy_image_marker
crabbox_readiness_manifest_matches
echo 'crabbox Linux readiness manifest and compatibility marker verified'
`;
}

export function goSource(bootstrap) {
  return `// Code generated by scripts/generate-linux-readiness.mjs; DO NOT EDIT.

package cli

const linuxMinimalReadinessBootstrap = ${JSON.stringify(bootstrap)}
`;
}

export function tsSource(bootstrap) {
  return `// Code generated by scripts/generate-linux-readiness.mjs; DO NOT EDIT.

// prettier-ignore
export const linuxMinimalReadinessBootstrap = ${JSON.stringify(bootstrap)};
`;
}

export async function loadRecipes(root = repoRoot) {
  const directory = resolve(root, "recipes/linux/v1");
  const profileSchema = parseStrictJSON(await readFile(resolve(directory, "profile.schema.json"), "utf8"), "profile.schema.json");
  const manifestSchema = parseStrictJSON(await readFile(resolve(directory, "manifest.schema.json"), "utf8"), "manifest.schema.json");
  validateSchemaDefinition(profileSchema, "profile");
  validateSchemaDefinition(manifestSchema, "manifest");
  const profiles = new Map();
  for (const name of profileNames) {
    const profile = parseStrictJSON(await readFile(resolve(directory, `${name}.json`), "utf8"), `${name}.json`);
    assertProfile(profile, name, profileSchema);
    profiles.set(name, profile);
  }
  assertInheritance(profiles);
  assertFrozenContract(profiles.get("linux-minimal"), profiles.get("linux-builder"));
  for (const [name, profile] of profiles) {
    validateJSONSchema(manifestFor(name, digest(profile)), manifestSchema, `${name} manifest`);
  }
  return { profileSchema, manifestSchema, minimal: profiles.get("linux-minimal"), builder: profiles.get("linux-builder") };
}

async function update(path, content, check, executable = false) {
  let existing;
  try {
    existing = await readFile(path, "utf8");
  } catch (error) {
    if (error?.code !== "ENOENT") throw error;
  }
  if (existing === content) return;
  if (check) throw new Error(`${path.slice(repoRoot.length + 1)} is stale`);
  await writeFile(path, content);
  if (executable) await chmod(path, 0o755);
}

export async function main(args = process.argv.slice(2)) {
  if (args.some((argument) => argument !== "--check")) throw new Error("usage: generate-linux-readiness.mjs [--check]");
  const check = args.includes("--check");
  const { minimal, builder } = await loadRecipes();
  const bootstrap = minimalBootstrap(minimal, builder);
  await update(resolve(repoRoot, "internal/cli/linux_readiness_generated.go"), goSource(bootstrap), check);
  await update(resolve(repoRoot, "worker/src/linux-readiness.generated.ts"), tsSource(bootstrap), check);
  await update(resolve(repoRoot, "scripts/linux-readiness.generated.sh"), producerSource(minimal, builder), check, true);
}

if (process.argv[1] && realpathSync(process.argv[1]) === realpathSync(scriptPath)) {
  await main();
}
