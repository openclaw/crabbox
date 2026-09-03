import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { spawnSync } from "node:child_process";
import { resolve } from "node:path";
import test from "node:test";
import { fragmentTokens, loadSources, repoRoot } from "./generate-bootstrap.mjs";

const sources = await loadSources();
const fragment = sources.fragments.find((entry) => entry.name === "windowsRuntime");
const runtime = fragmentTokens(fragment, sources.constants).map((token) => token.literal).join("");
const generated = await readFile(resolve(repoRoot, "scripts/windows-runtime.generated.ps1"), "utf8");
const core = sources.fragments.find((entry) => entry.name === "windowsCore").source;

test("standalone Windows runtime helper is the generated shared fragment with canonical pins", () => {
  assert.equal(generated.slice(generated.indexOf("\n") + 1), runtime);
  for (const name of ["windowsVCRuntimeX64", "windowsVCRuntimeARM64"]) {
    const url = sources.constants[`${name}URL`];
    const digest = sources.constants[`${name}SHA256`];
    assert.equal(new URL(url).hostname, "download.visualstudio.microsoft.com");
    assert.equal(new URL(url).pathname.split("/").at(-2).toLowerCase(), digest);
    assert.ok(runtime.includes(url) && runtime.includes(digest));
  }
  assert.doesNotMatch(runtime, /\{\{|aka\.ms|Restart-Computer|pnpm|node\.exe|setup-complete/u);
});

test("runtime verification, native architecture and fresh functional probes fail closed", () => {
  assert.match(runtime, /IsWow64Process2\(GetCurrentProcess\(\)/u);
  assert.match(runtime, /hostMachine\[0\] -ne 0 -or \$hostMachine\[2\] -ne 8/u);
  assert.match(runtime, /0x8664 \{ return 'AMD64' \}/u);
  assert.match(runtime, /0xAA64 \{ return 'ARM64' \}/u);
  assert.doesNotMatch(runtime, /PROCESSOR_ARCHITECTURE|PROCESSOR_ARCHITEW6432/u);
  assert.match(runtime, /LoadLibraryEx\(path, IntPtr.Zero, 0x00000800\)/u);
  assert.match(runtime, /Environment.SystemDirectory/u);
  assert.match(runtime, /New-Object Diagnostics.Process/u);
  assert.match(runtime, /-NoLogo -NoProfile -NonInteractive -EncodedCommand/u);
  assert.match(runtime, /if \('\$Architecture' -eq 'AMD64'\) \{ `\$dlls \+= 'vcruntime140_1.dll' \}/u);
  assert.match(runtime, /'vcruntime140.dll', 'msvcp140.dll'/u);
  const verifier = runtime.slice(runtime.indexOf("function Assert-CrabboxWindowsRuntimeArtifact"), runtime.indexOf("function Get-CrabboxWindowsRuntimeBoot"));
  assert.ok(verifier.indexOf("Get-FileHash") < verifier.indexOf("Get-AuthenticodeSignature"));
  assert.match(verifier, /Status -ne 'Valid' -or \$null -eq \$signature.SignerCertificate/u);
  assert.match(verifier, /X500DistinguishedNameFlags\]::UseNewLines/u);
  assert.match(verifier, /subject.ContainsKey\(\$key\)/u);
  assert.match(verifier, /\[string\]::Equals\(\$value, \$expected\[\$key\]/u);
  assert.match(verifier, /\$subject.Count -ne \$expected.Count/u);
});

test("installation is outside download retries and gates success on postcondition and reboot state", () => {
  const ensure = runtime.slice(runtime.indexOf("function Ensure-CrabboxWindowsRuntime"));
  const positions = [
    "Get-CrabboxWindowsRuntimeArchitecture", "Get-CrabboxWindowsRuntimePendingBoot",
    "if (Test-CrabboxWindowsRuntime $architecture)", "Assert-CrabboxWindowsRuntimeAdministrator",
    "New-CrabboxWindowsRuntimeStage", "Invoke-WebRequest", "Assert-CrabboxWindowsRuntimeArtifact $installer",
    "Set-CrabboxWindowsRuntimePendingBoot (Get-CrabboxWindowsRuntimeBoot)", "Start-Process",
    "if (-not (Test-CrabboxWindowsRuntime $architecture))",
  ].map((part) => {
    const index = ensure.indexOf(part);
    assert.ok(index >= 0, part);
    return index;
  });
  assert.deepEqual([...positions].sort((a, b) => a - b), positions);
  const downloadLoop = ensure.slice(ensure.indexOf("for ($attempt"), ensure.indexOf("Assert-CrabboxWindowsRuntimeArtifact $installer"));
  assert.match(downloadLoop, /\$attempt -le 3/u);
  assert.doesNotMatch(downloadLoop, /Start-Process|Get-FileHash|Get-AuthenticodeSignature/u);
  assert.match(ensure, /-ArgumentList '\/install','\/quiet','\/norestart' -Wait -PassThru/u);
  assert.match(ensure, /3010 \{ throw .*reboot required/u);
  assert.match(ensure, /1641 \{ throw .*unexpected restart/u);
  assert.match(ensure, /default \{ throw/u);
  assert.match(ensure, /finally \{\s+Remove-Item -LiteralPath \$stage -Recurse -Force/u);
  const staging = runtime.slice(runtime.indexOf("function New-CrabboxWindowsRuntimeStage"), runtime.indexOf("function Assert-CrabboxWindowsRuntimeArtifact"));
  assert.match(staging, /Guid\]::NewGuid/u);
  assert.match(staging, /SetAccessRuleProtection\(\$true, \$false\)/u);
  assert.match(staging, /\$directory.Create\(\$acl\)/u);
  assert.match(staging, /AreAccessRulesProtected/u);
  assert.match(staging, /\$rules.Count -ne 2/u);
  assert.match(staging, /ReparsePoint/u);
  assert.ok(core.indexOf("Remove-Item -LiteralPath $setupCompletePath") < core.indexOf("\nEnsure-CrabboxWindowsRuntime\n"));
  assert.ok(core.indexOf("\nEnsure-CrabboxWindowsRuntime\n") < core.indexOf("Get-LocalUser"));
  assert.match(core, /\$crabboxSetupWasComplete = Test-Path/u);
  const desktop = sources.fragments.find((entry) => entry.name === "windowsDesktop").source;
  assert.match(desktop, /if \(-not \$crabboxSetupWasComplete\) \{\s+Restart-Computer -Force/u);
});

test("native Windows behavioral harness", (t) => {
  if (process.platform !== "win32") {
    t.skip("Requires native Windows; run powershell.exe -NoProfile -ExecutionPolicy Bypass -File scripts/test-windows-runtime.ps1");
    return;
  }
  const result = spawnSync("powershell.exe", ["-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", resolve(repoRoot, "scripts/test-windows-runtime.ps1")], { encoding: "utf8", timeout: 180000 });
  assert.equal(result.status, 0, result.stdout + result.stderr);
});
