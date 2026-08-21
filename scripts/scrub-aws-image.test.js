import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, symlink, writeFile } from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import test from "node:test";
import { scrubImage } from "./scrub-aws-image.mjs";

async function seed(root, relative, content = "super-secret-value") {
  const file = path.join(root, ...relative.split("/"));
  await mkdir(path.dirname(file), { recursive: true });
  await writeFile(file, content);
  return file;
}

async function missing(file) {
  await assert.rejects(readFile(file, "utf8"), { code: "ENOENT" });
}

function fakeWindowsRegistry(entries, retain = false) {
  const values = new Set(entries.map(([key, name]) => `${key}\0${name}`));
  return {
    values,
    async hasValue(key, name) {
      return values.has(`${key}\0${name}`);
    },
    async removeValue(key, name) {
      if (!retain) values.delete(`${key}\0${name}`);
    },
  };
}

test("Linux image scrub removes identity, credentials, workspaces, and prep state", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-linux-scrub-"));
  const fixtures = [
    "root/.ssh/authorized_keys",
    "root/.ssh/id_ed25519",
    "root/.ssh/id_ecdsa",
    "home/alice/.ssh/authorized_keys",
    "home/alice/.ssh/id_rsa",
    "home/alice/.ssh/id_dsa",
    "etc/ssh/ssh_host_ed25519_key",
    "root/.bash_history",
    "home/alice/.zsh_history",
    "var/lib/cloud/instances/i-123/state",
    "var/log/cloud-init.log",
    "root/.aws/credentials",
    "home/alice/.config/gh/hosts.yml",
    "var/lib/crabbox/vnc.password",
    "var/lib/crabbox/vnc.pass",
    "work/crabbox/repo/.git/config",
    "workspaces/repo/.git/config",
    "var/lib/crabbox/workspaces/repo/file",
    "var/lib/crabbox/image-prep.log",
    "etc/machine-id",
  ];
  const files = await Promise.all(fixtures.map((fixture) => seed(root, fixture)));
  const readiness = await seed(
    root,
    "var/lib/crabbox/readiness/linux.json",
    '{"profile":"linux-builder"}',
  );
  const reportPath = path.join(root, "report.json");

  const report = await scrubImage({ target: "linux", root, report: reportPath });

  for (const file of files) await missing(file);
  assert.equal(await readFile(readiness, "utf8"), '{"profile":"linux-builder"}');
  assert.equal(report.schema, "crabbox-aws-image-scrub/v1");
  assert.equal(report.target, "linux");
  assert.deepEqual(report.findings, []);
  assert.match(report.evidenceDigest, /^sha256:[0-9a-f]{64}$/);
  const stored = await readFile(reportPath, "utf8");
  assert.doesNotMatch(stored, /super-secret-value|alice|i-123/);
  assert.deepEqual(JSON.parse(stored), report);
});

test("Linux image scrub removes SSH directory links without following them", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-linux-scrub-link-"));
  const outside = await mkdtemp(path.join(os.tmpdir(), "crabbox-linux-scrub-outside-"));
  const privateKey = await seed(outside, "id_ed25519");
  await mkdir(path.join(root, "home", "alice"), { recursive: true });
  await symlink(outside, path.join(root, "home", "alice", ".ssh"), "dir");

  const report = await scrubImage({
    target: "linux",
    root,
    report: path.join(root, "report.json"),
  });

  assert.deepEqual(report.findings, []);
  await missing(path.join(root, "home", "alice", ".ssh", "id_ed25519"));
  assert.equal(await readFile(privateKey, "utf8"), "super-secret-value");
});

test("Linux image scrub fails closed on linked home directories without traversal", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-linux-home-link-"));
  const outside = await mkdtemp(path.join(os.tmpdir(), "crabbox-linux-home-outside-"));
  const privateKey = await seed(outside, ".ssh/id_ed25519");
  await mkdir(path.join(root, "home"), { recursive: true });
  await symlink(outside, path.join(root, "home", "alice"), "dir");

  const report = await scrubImage({
    target: "linux",
    root,
    report: path.join(root, "report.json"),
  });

  assert.deepEqual(report.findings, ["credentials"]);
  assert.equal(await readFile(privateKey, "utf8"), "super-secret-value");
});

test("Linux image scrub fails closed on a linked home root without traversal", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-linux-home-root-link-"));
  const outside = await mkdtemp(path.join(os.tmpdir(), "crabbox-linux-home-root-outside-"));
  const privateKey = await seed(outside, "alice/.ssh/id_ed25519");
  await symlink(outside, path.join(root, "home"), "dir");

  const report = await scrubImage({
    target: "linux",
    root,
    report: path.join(root, "report.json"),
  });

  assert.deepEqual(report.findings, ["credentials"]);
  assert.equal(await readFile(privateKey, "utf8"), "super-secret-value");
});

test("Linux image scrub fails closed unless the effective UID is root", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-linux-scrub-root-"));
  const credential = await seed(root, "var/lib/crabbox/vnc.password");
  await assert.rejects(
    scrubImage({
      target: "linux",
      root,
      report: path.join(root, "report.json"),
      requireRoot: true,
      effectiveUid: 1000,
    }),
    /requires effective UID 0/,
  );
  assert.equal(await readFile(credential, "utf8"), "super-secret-value");

  const report = await scrubImage({
    target: "linux",
    root,
    report: path.join(root, "report.json"),
    requireRoot: true,
    effectiveUid: 0,
  });
  assert.deepEqual(report.findings, []);
  await missing(credential);
});

test("Windows image scrub removes SSH, EC2Launch, credentials, and workspace state", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-scrub-"));
  const fixtures = [
    "ProgramData/ssh/administrators_authorized_keys",
    "ProgramData/ssh/ssh_host_rsa_key",
    "Users/alice/.ssh/authorized_keys",
    "Users/alice/.ssh/id_ed25519",
    "Users/alice/.ssh/id_rsa",
    "Users/alice/AppData/Roaming/Microsoft/Windows/PowerShell/PSReadLine/ConsoleHost_history.txt",
    "Users/alice/.aws/credentials",
    "Users/alice/.config/gh/hosts.yml",
    "ProgramData/Amazon/EC2Launch/log/agent.log",
    "ProgramData/Amazon/EC2Launch/state/previous-state.json",
    "ProgramData/crabbox/credentials/token",
    "ProgramData/crabbox/vnc.password",
    "ProgramData/crabbox/vnc.pass",
    "ProgramData/crabbox/windows.password",
    "ProgramData/crabbox/windows.username",
    "crabbox/repo/.git/config",
    "ProgramData/crabbox/workspaces/repo/file",
    "ProgramData/crabbox/image-prep.exit",
  ];
  const files = await Promise.all(fixtures.map((fixture) => seed(root, fixture)));
  const reportPath = path.join(root, "report.json");
  const registry = fakeWindowsRegistry([
    ["HKLM:\\Software\\TightVNC\\Server", "Password"],
    ["HKLM:\\Software\\TightVNC\\Server", "ControlPassword"],
    ["HKLM:\\Software\\WOW6432Node\\TightVNC\\Server", "Password"],
    ["HKLM:\\Software\\WOW6432Node\\TightVNC\\Server", "ControlPassword"],
  ]);

  const report = await scrubImage({
    target: "windows",
    root,
    report: reportPath,
    windowsRegistry: registry,
  });

  for (const file of files) await missing(file);
  assert.equal(registry.values.size, 0);
  assert.equal(report.target, "windows");
  assert.deepEqual(report.findings, []);
  assert.ok(report.removed.cloudInitState >= 1);
  assert.ok(report.removed.credentials >= 1);
  assert.ok(report.removed.hostIdentity >= 1);
  const stored = await readFile(reportPath, "utf8");
  assert.doesNotMatch(stored, /super-secret-value|alice/);
  assert.deepEqual(JSON.parse(stored), report);
});

test("Windows scrub reports retained TightVNC registry credentials", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-registry-scrub-"));
  const registry = fakeWindowsRegistry(
    [["HKLM:\\Software\\TightVNC\\Server", "ControlPassword"]],
    true,
  );
  const report = await scrubImage({
    target: "windows",
    root,
    report: path.join(root, "report.json"),
    windowsRegistry: registry,
  });
  assert.deepEqual(report.findings, ["credentials"]);
});

test("Windows image scrub fails closed on linked profiles without traversal", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-profile-link-"));
  const outside = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-profile-outside-"));
  const privateKey = await seed(outside, ".ssh/id_rsa");
  await mkdir(path.join(root, "Users"), { recursive: true });
  await symlink(outside, path.join(root, "Users", "alice"), "dir");

  const report = await scrubImage({
    target: "windows",
    root,
    report: path.join(root, "report.json"),
    windowsRegistry: fakeWindowsRegistry([]),
  });

  assert.deepEqual(report.findings, ["credentials"]);
  assert.equal(await readFile(privateKey, "utf8"), "super-secret-value");
});

test("Windows image scrub validates compatibility link targets", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-compat-link-"));
  const outside = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-compat-outside-"));
  const privateKey = await seed(outside, ".ssh/id_rsa");
  await mkdir(path.join(root, "Users", "Default"), { recursive: true });
  await mkdir(path.join(root, "ProgramData"), { recursive: true });
  await symlink(path.join(root, "ProgramData"), path.join(root, "Users", "All Users"), "dir");
  await symlink(outside, path.join(root, "Users", "Default User"), "dir");

  const report = await scrubImage({
    target: "windows",
    root,
    report: path.join(root, "report.json"),
    windowsRegistry: fakeWindowsRegistry([]),
  });

  assert.deepEqual(report.findings, ["credentials"]);
  assert.equal(await readFile(privateKey, "utf8"), "super-secret-value");
});

test("Windows image scrub accepts exact in-root compatibility links", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-compat-valid-"));
  await mkdir(path.join(root, "Users", "Default"), { recursive: true });
  await mkdir(path.join(root, "ProgramData"), { recursive: true });
  await symlink(path.join(root, "ProgramData"), path.join(root, "Users", "All Users"), "dir");
  await symlink(
    path.join(root, "Users", "Default"),
    path.join(root, "Users", "Default User"),
    "dir",
  );

  const report = await scrubImage({
    target: "windows",
    root,
    report: path.join(root, "report.json"),
    windowsRegistry: fakeWindowsRegistry([]),
  });

  assert.deepEqual(report.findings, []);
});

test("Windows image scrub rejects compatibility links with a second linked hop", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-compat-hop-"));
  const outside = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-compat-hop-outside-"));
  const privateKey = await seed(outside, ".ssh/id_rsa");
  await mkdir(path.join(root, "Users"), { recursive: true });
  await symlink(outside, path.join(root, "ProgramData"), "dir");
  await symlink(path.join(root, "ProgramData"), path.join(root, "Users", "All Users"), "dir");

  const report = await scrubImage({
    target: "windows",
    root,
    report: path.join(root, "report.json"),
    windowsRegistry: fakeWindowsRegistry([]),
  });

  assert.deepEqual(report.findings, ["credentials"]);
  assert.equal(await readFile(privateKey, "utf8"), "super-secret-value");
});

test("Windows image scrub treats prototype-named profile links as credentials", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-prototype-link-"));
  const outside = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-prototype-outside-"));
  const privateKey = await seed(outside, ".ssh/id_rsa");
  await mkdir(path.join(root, "Users"), { recursive: true });
  await symlink(outside, path.join(root, "Users", "constructor"), "dir");

  const report = await scrubImage({
    target: "windows",
    root,
    report: path.join(root, "report.json"),
    windowsRegistry: fakeWindowsRegistry([]),
  });

  assert.deepEqual(report.findings, ["credentials"]);
  assert.equal(await readFile(privateKey, "utf8"), "super-secret-value");
});

test("Windows image scrub fails closed on a linked Users root without traversal", async () => {
  const root = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-users-root-link-"));
  const outside = await mkdtemp(path.join(os.tmpdir(), "crabbox-windows-users-root-outside-"));
  const privateKey = await seed(outside, "alice/.ssh/id_rsa");
  await symlink(outside, path.join(root, "Users"), "dir");

  const report = await scrubImage({
    target: "windows",
    root,
    report: path.join(root, "report.json"),
    windowsRegistry: fakeWindowsRegistry([]),
  });

  assert.deepEqual(report.findings, ["credentials"]);
  assert.equal(await readFile(privateKey, "utf8"), "super-secret-value");
});

test("Windows scrub unregisters and verifies removal of the image prep task", async () => {
  const source = await readFile(
    path.join(import.meta.dirname, "scrub-aws-windows-image.ps1"),
    "utf8",
  );
  assert.match(
    source,
    /Get-ScheduledTask -TaskName "CrabboxImagePrep" -ErrorAction SilentlyContinue[\s\S]*Unregister-ScheduledTask -TaskName "CrabboxImagePrep" -Confirm:\$false/,
  );
  assert.match(
    source,
    /Add-ScrubFinding \(\$null -ne \(Get-ScheduledTask -TaskName "CrabboxImagePrep" -ErrorAction SilentlyContinue\)\) "prepArtifacts"/,
  );
  assert.match(
    source,
    /Remove-ItemProperty -LiteralPath \$RegistryPath -Name \$Name -Force/,
  );
  assert.match(
    source,
    /Add-ScrubFinding \(\$Properties\.PSObject\.Properties\.Name -contains \$Name\) "credentials"/,
  );
  assert.ok(source.includes('"HKLM:\\Software\\TightVNC\\Server"'));
  assert.ok(source.includes('"HKLM:\\Software\\WOW6432Node\\TightVNC\\Server"'));
  assert.ok(source.includes('$TightVNCPasswordNames = @("Password", "ControlPassword")'));
  assert.match(source, /function Remove-UserSSHState/);
  assert.match(source, /FileAttributes\]::ReparsePoint/);
  assert.match(source, /function Test-CompatibilityProfileLink/);
  assert.match(source, /function Get-ScrubChildEntry/);
  assert.match(source, /function Test-TrustedScrubDirectory/);
  assert.match(source, /if \(-not \(Test-TrustedScrubDirectory \$ExpectedRelative\)\)/);
  assert.match(source, /"All Users" \{ "ProgramData" \}/);
  assert.match(source, /"Default User" \{ "Users\\Default" \}/);
  assert.match(source, /\[string\]::Equals\(\$Actual, \$Expected, \[StringComparison\]::OrdinalIgnoreCase\)/);
  assert.doesNotMatch(source, /ExpandEnvironmentVariables/);
  assert.match(source, /\$UsersRootEntry = Get-ScrubChildEntry \$Root "Users"/);
  assert.match(source, /\$LinkedUsersRoot = \$null -ne \$UsersRootEntry/);
  assert.match(source, /\$ReparseProfiles = @\(\$UserEntries \| Where-Object/);
  assert.match(
    source,
    /Add-ScrubFinding \(\$LinkedUsersRoot -or \$ReparseProfiles\.Count -gt 0\) "credentials"/,
  );
  assert.match(
    source,
    /\$UserEntries = if \(\$null -ne \$UsersRootEntry -and -not \$LinkedUsersRoot\) \{[\s\S]*Get-ChildItem -LiteralPath \$UsersRoot -Directory -Force/,
  );
  assert.match(
    source,
    /Add-ScrubFinding \(Test-Path -LiteralPath \(Join-Path \$Root "Users\\\$\(\$User\.Name\)\\\.ssh"\)\) "credentials"/,
  );
});
