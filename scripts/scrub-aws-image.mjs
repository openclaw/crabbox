#!/usr/bin/env node

import { createHash } from "node:crypto";
import { spawnSync } from "node:child_process";
import {
  lstat,
  mkdir,
  readlink,
  readdir,
  rename,
  rm,
  writeFile,
} from "node:fs/promises";
import path from "node:path";
import { fileURLToPath } from "node:url";

const scrubSchema = "crabbox-aws-image-scrub/v1";
const tightVNCRegistryKeys = [
  "HKLM:\\Software\\TightVNC\\Server",
  "HKLM:\\Software\\WOW6432Node\\TightVNC\\Server",
];
const tightVNCRegistryValues = ["Password", "ControlPassword"];

function canonicalJSON(value) {
  if (Array.isArray(value)) return `[${value.map(canonicalJSON).join(",")}]`;
  if (value && typeof value === "object") {
    return `{${Object.keys(value)
      .sort()
      .map((key) => `${JSON.stringify(key)}:${canonicalJSON(value[key])}`)
      .join(",")}}`;
  }
  return JSON.stringify(value);
}

function parseArgs(argv) {
  const values = { root: "", report: "", requireRoot: false };
  for (let index = 0; index < argv.length; index += 1) {
    const name = argv[index];
    if (name === "--require-root") {
      values.requireRoot = true;
      continue;
    }
    if (!["--target", "--root", "--report"].includes(name) || index + 1 >= argv.length) {
      throw new Error(
        "usage: scrub-aws-image.mjs --target linux|windows [--root path] [--report path|-] [--require-root]",
      );
    }
    values[name.slice(2)] = argv[index + 1];
    index += 1;
  }
  if (!["linux", "windows"].includes(values.target)) {
    throw new Error("--target must be linux or windows");
  }
  if (!values.root) {
    values.root =
      process.env.CRABBOX_SCRUB_ROOT ||
      (values.target === "windows" ? path.parse(process.cwd()).root : "/");
  }
  if (!values.report) {
    values.report =
      process.env.CRABBOX_SCRUB_REPORT ||
      path.join(
        values.root,
        ...(values.target === "windows"
          ? ["ProgramData", "crabbox", "image-scrub-report.json"]
          : ["var", "lib", "crabbox", "image-scrub-report.json"]),
      );
  }
  return values;
}

async function exists(file) {
  try {
    return await lstat(file);
  } catch (error) {
    if (error?.code === "ENOENT") return null;
    throw error;
  }
}

function inside(root, parts) {
  const resolvedRoot = path.resolve(root);
  const candidate = path.resolve(root, ...parts);
  if (candidate !== resolvedRoot && !candidate.startsWith(`${resolvedRoot}${path.sep}`)) {
    throw new Error("scrub path escaped the selected root");
  }
  return candidate;
}

async function removeEntry(root, parts) {
  const target = inside(root, parts);
  if (!(await exists(target))) return 0;
  await rm(target, { recursive: true, force: true });
  return 1;
}

async function removePrefixedEntries(root, parts, prefix) {
  const directory = inside(root, parts);
  const stat = await exists(directory);
  if (!stat || !stat.isDirectory() || stat.isSymbolicLink()) return 0;
  let removed = 0;
  for (const name of await readdir(directory)) {
    if (name.startsWith(prefix)) removed += await removeEntry(root, [...parts, name]);
  }
  return removed;
}

async function removeUserSSHState(root, parts) {
  const directory = inside(root, parts);
  const stat = await exists(directory);
  if (!stat) return { authorizedKeys: 0, credentials: 0 };
  if (!stat.isDirectory() || stat.isSymbolicLink()) {
    await rm(directory, { force: true });
    return { authorizedKeys: 0, credentials: 1 };
  }
  const authorizedKeys = await removeEntry(root, [...parts, "authorized_keys"]);
  await rm(directory, { recursive: true, force: true });
  return { authorizedKeys, credentials: 1 };
}

async function childDirectories(root, parts) {
  const directory = inside(root, parts);
  const stat = await exists(directory);
  if (!stat || !stat.isDirectory() || stat.isSymbolicLink()) return [];
  const children = [];
  for (const name of await readdir(directory)) {
    const child = await exists(inside(root, [...parts, name]));
    if (child?.isDirectory() && !child.isSymbolicLink()) children.push(name);
  }
  return children;
}

async function trustedDirectoryPath(root, parts) {
  for (let length = 1; length <= parts.length; length += 1) {
    const stat = await exists(inside(root, parts.slice(0, length)));
    if (!stat?.isDirectory() || stat.isSymbolicLink()) return false;
  }
  return true;
}

async function unsafeChildDirectoryLinks(root, parts, allowedTargets = {}) {
  const directory = inside(root, parts);
  const stat = await exists(directory);
  if (!stat) return [];
  if (!stat.isDirectory() || stat.isSymbolicLink()) return ["."];
  const links = [];
  for (const name of await readdir(directory)) {
    const childPath = inside(root, [...parts, name]);
    const child = await exists(childPath);
    if (!child?.isSymbolicLink()) continue;
    const expectedParts = Object.hasOwn(allowedTargets, name) ? allowedTargets[name] : null;
    if (expectedParts) {
      const target = await readlink(childPath);
      const resolved = path.resolve(path.dirname(childPath), target);
      if (
        resolved === inside(root, expectedParts) &&
        (await trustedDirectoryPath(root, expectedParts))
      ) {
        continue;
      }
    }
    links.push(name);
  }
  return links;
}

async function hasEntry(root, parts) {
  return Boolean(await exists(inside(root, parts)));
}

async function hasPrefixedEntry(root, parts, prefix) {
  const directory = inside(root, parts);
  const stat = await exists(directory);
  if (!stat || !stat.isDirectory() || stat.isSymbolicLink()) return false;
  return (await readdir(directory)).some((name) => name.startsWith(prefix));
}

async function scrubLinux(root) {
  const removed = {
    authorizedKeys: 0,
    cloudInitState: 0,
    credentials: 0,
    hostIdentity: 0,
    prepArtifacts: 0,
    shellHistory: 0,
    sshHostKeys: 0,
    workspaces: 0,
  };
  const homes = await childDirectories(root, ["home"]);

  const rootSSH = await removeUserSSHState(root, ["root", ".ssh"]);
  removed.authorizedKeys += rootSSH.authorizedKeys;
  removed.credentials += rootSSH.credentials;
  for (const user of homes) {
    const userSSH = await removeUserSSHState(root, ["home", user, ".ssh"]);
    removed.authorizedKeys += userSSH.authorizedKeys;
    removed.credentials += userSSH.credentials;
  }
  removed.sshHostKeys += await removePrefixedEntries(root, ["etc", "ssh"], "ssh_host_");

  for (const history of [".bash_history", ".zsh_history", ".python_history"]) {
    removed.shellHistory += await removeEntry(root, ["root", history]);
    for (const user of homes) {
      removed.shellHistory += await removeEntry(root, ["home", user, history]);
    }
  }

  for (const parts of [
    ["var", "lib", "cloud", "instances"],
    ["var", "lib", "cloud", "instance"],
    ["var", "log", "cloud-init.log"],
    ["var", "log", "cloud-init-output.log"],
  ]) {
    removed.cloudInitState += await removeEntry(root, parts);
  }

  for (const parts of [
    ["root", ".aws"],
    ["root", ".config", "gh"],
    ["etc", "crabbox", "credentials"],
    ["var", "lib", "crabbox", "credentials.json"],
    ["var", "lib", "crabbox", "auth.json"],
    ["var", "lib", "crabbox", "vnc.password"],
    ["var", "lib", "crabbox", "vnc.pass"],
  ]) {
    removed.credentials += await removeEntry(root, parts);
  }
  for (const user of homes) {
    for (const parts of [
      ["home", user, ".aws"],
      ["home", user, ".config", "gh"],
    ]) {
      removed.credentials += await removeEntry(root, parts);
    }
  }

  for (const parts of [
    ["workspace"],
    ["workspaces"],
    ["work", "crabbox"],
    ["var", "lib", "crabbox", "workspaces"],
  ]) {
    removed.workspaces += await removeEntry(root, parts);
  }
  for (const user of homes) {
    removed.workspaces += await removeEntry(root, [
      "home",
      user,
      ".crabbox",
      "workspaces",
    ]);
  }

  removed.prepArtifacts += await removePrefixedEntries(
    root,
    ["var", "lib", "crabbox"],
    "image-prep",
  );
  removed.prepArtifacts += await removePrefixedEntries(root, ["tmp"], "crabbox-image-prep");

  for (const parts of [
    ["etc", "machine-id"],
    ["var", "lib", "dbus", "machine-id"],
  ]) {
    removed.hostIdentity += await removeEntry(root, parts);
  }
  return removed;
}

async function hasRegistryValue(registry, key, name) {
  return registry ? Boolean(await registry.hasValue(key, name)) : false;
}

async function scrubWindows(root, registry) {
  const removed = {
    authorizedKeys: 0,
    cloudInitState: 0,
    credentials: 0,
    hostIdentity: 0,
    prepArtifacts: 0,
    shellHistory: 0,
    sshHostKeys: 0,
    workspaces: 0,
  };
  const users = await childDirectories(root, ["Users"]);

  removed.authorizedKeys += await removeEntry(root, [
    "ProgramData",
    "ssh",
    "administrators_authorized_keys",
  ]);
  for (const user of users) {
    const userSSH = await removeUserSSHState(root, ["Users", user, ".ssh"]);
    removed.authorizedKeys += userSSH.authorizedKeys;
    removed.credentials += userSSH.credentials;
  }
  removed.sshHostKeys += await removePrefixedEntries(
    root,
    ["ProgramData", "ssh"],
    "ssh_host_",
  );

  for (const user of users) {
    removed.shellHistory += await removeEntry(root, [
      "Users",
      user,
      "AppData",
      "Roaming",
      "Microsoft",
      "Windows",
      "PowerShell",
      "PSReadLine",
      "ConsoleHost_history.txt",
    ]);
  }

  removed.hostIdentity += await removeEntry(root, [
    "ProgramData",
    "Amazon",
    "EC2Launch",
    "state",
    "previous-state.json",
  ]);

  for (const parts of [
    ["ProgramData", "Amazon", "EC2Launch", "log"],
    ["ProgramData", "Amazon", "EC2Launch", "state"],
    ["ProgramData", "Amazon", "EC2-Windows", "Launch", "Log"],
    ["ProgramData", "Amazon", "EC2-Windows", "Launch", "State"],
  ]) {
    removed.cloudInitState += await removeEntry(root, parts);
  }

  for (const parts of [
    ["ProgramData", "crabbox", "credentials"],
    ["ProgramData", "crabbox", "auth.json"],
    ["ProgramData", "crabbox", "vnc.password"],
    ["ProgramData", "crabbox", "vnc.pass"],
    ["ProgramData", "crabbox", "windows.password"],
    ["ProgramData", "crabbox", "windows.username"],
  ]) {
    removed.credentials += await removeEntry(root, parts);
  }
  for (const key of tightVNCRegistryKeys) {
    for (const name of tightVNCRegistryValues) {
      if (await hasRegistryValue(registry, key, name)) {
        await registry.removeValue(key, name);
        removed.credentials += 1;
      }
    }
  }
  for (const user of users) {
    for (const parts of [
      ["Users", user, ".aws"],
      ["Users", user, ".config", "gh"],
    ]) {
      removed.credentials += await removeEntry(root, parts);
    }
  }

  for (const parts of [
    ["workspace"],
    ["workspaces"],
    ["crabbox"],
    ["ProgramData", "crabbox", "workspaces"],
  ]) {
    removed.workspaces += await removeEntry(root, parts);
  }
  removed.prepArtifacts += await removePrefixedEntries(
    root,
    ["ProgramData", "crabbox"],
    "image-prep",
  );
  return removed;
}

async function residualFindings(target, root, windowsRegistry) {
  const findings = new Set();
  const add = (condition, name) => {
    if (condition) findings.add(name);
  };
  if (target === "linux") {
    const homes = await childDirectories(root, ["home"]);
    add((await unsafeChildDirectoryLinks(root, ["home"])).length > 0, "credentials");
    add(await hasEntry(root, ["root", ".ssh"]), "credentials");
    add(await hasPrefixedEntry(root, ["etc", "ssh"], "ssh_host_"), "sshHostKeys");
    add(await hasEntry(root, ["root", ".aws"]), "credentials");
    add(await hasEntry(root, ["root", ".config", "gh"]), "credentials");
    add(await hasEntry(root, ["etc", "crabbox", "credentials"]), "credentials");
    add(await hasEntry(root, ["var", "lib", "crabbox", "credentials.json"]), "credentials");
    add(await hasEntry(root, ["var", "lib", "crabbox", "auth.json"]), "credentials");
    add(await hasEntry(root, ["var", "lib", "crabbox", "vnc.password"]), "credentials");
    add(await hasEntry(root, ["var", "lib", "crabbox", "vnc.pass"]), "credentials");
    add(await hasEntry(root, ["var", "lib", "cloud", "instances"]), "cloudInitState");
    add(await hasEntry(root, ["var", "lib", "cloud", "instance"]), "cloudInitState");
    add(await hasEntry(root, ["var", "log", "cloud-init.log"]), "cloudInitState");
    add(await hasEntry(root, ["var", "log", "cloud-init-output.log"]), "cloudInitState");
    add(await hasEntry(root, ["workspace"]), "workspaces");
    add(await hasEntry(root, ["workspaces"]), "workspaces");
    add(await hasEntry(root, ["work", "crabbox"]), "workspaces");
    add(await hasEntry(root, ["var", "lib", "crabbox", "workspaces"]), "workspaces");
    add(await hasPrefixedEntry(root, ["var", "lib", "crabbox"], "image-prep"), "prepArtifacts");
    add(await hasPrefixedEntry(root, ["tmp"], "crabbox-image-prep"), "prepArtifacts");
    add(await hasEntry(root, ["etc", "machine-id"]), "hostIdentity");
    add(await hasEntry(root, ["var", "lib", "dbus", "machine-id"]), "hostIdentity");
    for (const history of [".bash_history", ".zsh_history", ".python_history"]) {
      add(await hasEntry(root, ["root", history]), "shellHistory");
    }
    for (const user of homes) {
      add(await hasEntry(root, ["home", user, ".ssh"]), "credentials");
      add(await hasEntry(root, ["home", user, ".aws"]), "credentials");
      add(await hasEntry(root, ["home", user, ".config", "gh"]), "credentials");
      add(
        await hasEntry(root, ["home", user, ".crabbox", "workspaces"]),
        "workspaces",
      );
      for (const history of [".bash_history", ".zsh_history", ".python_history"]) {
        add(await hasEntry(root, ["home", user, history]), "shellHistory");
      }
    }
  } else {
    const users = await childDirectories(root, ["Users"]);
    add(
      (
        await unsafeChildDirectoryLinks(root, ["Users"], {
          "All Users": ["ProgramData"],
          "Default User": ["Users", "Default"],
        })
      ).length > 0,
      "credentials",
    );
    add(
      await hasEntry(root, [
        "ProgramData",
        "ssh",
        "administrators_authorized_keys",
      ]),
      "authorizedKeys",
    );
    add(
      await hasPrefixedEntry(root, ["ProgramData", "ssh"], "ssh_host_"),
      "sshHostKeys",
    );
    add(
      await hasEntry(root, ["ProgramData", "Amazon", "EC2Launch", "state"]),
      "cloudInitState",
    );
    add(
      await hasEntry(root, ["ProgramData", "Amazon", "EC2Launch", "log"]),
      "cloudInitState",
    );
    add(
      await hasEntry(root, ["ProgramData", "Amazon", "EC2-Windows", "Launch", "State"]),
      "cloudInitState",
    );
    add(
      await hasEntry(root, ["ProgramData", "Amazon", "EC2-Windows", "Launch", "Log"]),
      "cloudInitState",
    );
    add(
      await hasEntry(root, [
        "ProgramData",
        "Amazon",
        "EC2Launch",
        "state",
        "previous-state.json",
      ]),
      "hostIdentity",
    );
    add(
      await hasEntry(root, ["ProgramData", "crabbox", "credentials"]),
      "credentials",
    );
    add(
      await hasEntry(root, ["ProgramData", "crabbox", "auth.json"]),
      "credentials",
    );
    add(
      await hasEntry(root, ["ProgramData", "crabbox", "vnc.password"]),
      "credentials",
    );
    add(
      await hasEntry(root, ["ProgramData", "crabbox", "vnc.pass"]),
      "credentials",
    );
    add(
      await hasEntry(root, ["ProgramData", "crabbox", "windows.password"]),
      "credentials",
    );
    add(
      await hasEntry(root, ["ProgramData", "crabbox", "windows.username"]),
      "credentials",
    );
    for (const key of tightVNCRegistryKeys) {
      for (const name of tightVNCRegistryValues) {
        add(await hasRegistryValue(windowsRegistry, key, name), "credentials");
      }
    }
    add(await hasEntry(root, ["workspace"]), "workspaces");
    add(await hasEntry(root, ["workspaces"]), "workspaces");
    add(await hasEntry(root, ["crabbox"]), "workspaces");
    add(
      await hasEntry(root, ["ProgramData", "crabbox", "workspaces"]),
      "workspaces",
    );
    add(
      await hasPrefixedEntry(root, ["ProgramData", "crabbox"], "image-prep"),
      "prepArtifacts",
    );
    for (const user of users) {
      add(await hasEntry(root, ["Users", user, ".ssh"]), "credentials");
      add(await hasEntry(root, ["Users", user, ".aws"]), "credentials");
      add(await hasEntry(root, ["Users", user, ".config", "gh"]), "credentials");
      add(
        await hasEntry(root, [
          "Users",
          user,
          "AppData",
          "Roaming",
          "Microsoft",
          "Windows",
          "PowerShell",
          "PSReadLine",
          "ConsoleHost_history.txt",
        ]),
        "shellHistory",
      );
    }
  }
  return [...findings].sort();
}

async function writeAtomic(file, content) {
  const directory = path.dirname(file);
  await mkdir(directory, { recursive: true, mode: 0o755 });
  const temporary = path.join(
    directory,
    `.${path.basename(file)}.${process.pid}.${Date.now()}.tmp`,
  );
  await writeFile(temporary, content, { mode: 0o600 });
  await rename(temporary, file);
}

export async function scrubImage({
  target,
  root,
  report,
  requireRoot = false,
  effectiveUid = process.getuid?.(),
  windowsRegistry = null,
}) {
  if (requireRoot && effectiveUid !== 0) {
    throw new Error("image scrub requires effective UID 0");
  }
  const removed =
    target === "linux" ? await scrubLinux(root) : await scrubWindows(root, windowsRegistry);
  const findings = await residualFindings(target, root, windowsRegistry);
  const evidence = { schema: scrubSchema, target, removed, findings };
  const evidenceDigest = `sha256:${createHash("sha256")
    .update(canonicalJSON(evidence))
    .digest("hex")}`;
  const result = { ...evidence, evidenceDigest };
  const output = `${canonicalJSON(result)}\n`;
  if (report === "-") {
    process.stdout.write(output);
  } else {
    await writeAtomic(report, output);
    process.stdout.write(output);
  }
  return result;
}

if (import.meta.url === `file://${process.argv[1]}`) {
  try {
    const argv = process.argv.slice(2);
    const options = parseArgs(argv);
    if (options.requireRoot && process.getuid?.() !== 0) {
      const elevated = spawnSync(
        "sudo",
        ["-n", "--", process.execPath, fileURLToPath(import.meta.url), ...argv],
        { stdio: "inherit" },
      );
      if (elevated.error) throw elevated.error;
      if (elevated.status !== 0) {
        throw new Error(`root image scrub failed with status ${elevated.status ?? "unknown"}`);
      }
      process.exit(0);
    }
    await scrubImage(options);
  } catch (error) {
    process.stderr.write(`${error.message}\n`);
    process.exitCode = 1;
  }
}
