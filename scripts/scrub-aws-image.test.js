import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, writeFile } from "node:fs/promises";
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
    "home/alice/.ssh/authorized_keys",
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
});
