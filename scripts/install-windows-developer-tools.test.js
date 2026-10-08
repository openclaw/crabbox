import assert from "node:assert/strict";
import { mkdtemp, readFile, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { spawnSync } from "node:child_process";

const script = await readFile("scripts/install-windows-developer-tools.ps1", "utf8");

const truffleHogFunctions = script.slice(script.indexOf("function Test-TruffleHogBinary"), script.indexOf("function Install-StaticDockerEngine"));
const psQuote = (value) => `'${value.replaceAll("'", "''")}'`;

async function truffleHogFixture(t, body) {
  const dir = await mkdtemp(join(tmpdir(), "crabbox-trufflehog-test-"));
  try {
    const binary = join(dir, "fixture.exe");
    const compile = process.platform === "win32" ? `
Add-Type -OutputAssembly $fixture -OutputType ConsoleApplication -TypeDefinition @'
using System;
public class Fixture {
  public static int Main(string[] args) {
    if (args.Length != 2 || args[0] != "--no-update" || args[1] != "--version") return 89;
    Console.Out.WriteLine(Environment.GetEnvironmentVariable("CRABBOX_TEST_STDOUT"));
    Console.Error.WriteLine(Environment.GetEnvironmentVariable("CRABBOX_TEST_STDERR"));
    return int.Parse(Environment.GetEnvironmentVariable("CRABBOX_TEST_EXIT"));
  }
}
'@
` : "";
    if (process.platform !== "win32") {
      await writeFile(binary, '#!/bin/sh\n[ "$1" = "--no-update" ] && [ "$2" = "--version" ] || exit 89\nprintf "%s\\n" "$CRABBOX_TEST_STDOUT"\nprintf "%s\\n" "$CRABBOX_TEST_STDERR" >&2\nexit "$CRABBOX_TEST_EXIT"\n', { mode: 0o755 });
    }
    const fixtureScript = join(dir, "fixture.ps1");
    await writeFile(fixtureScript, `
$ErrorActionPreference = 'Stop'
$env:TEMP = ${psQuote(dir)}
$fixture = ${psQuote(binary)}
${compile}
$TruffleHogVersion = '3.95.9'
$TruffleHogInstallDir = Join-Path $env:TEMP 'install with spaces'
$env:CRABBOX_TEST_STDOUT = 'trufflehog 3.95.9'
$env:CRABBOX_TEST_STDERR = ''
$env:CRABBOX_TEST_EXIT = '0'
function Write-Log { param([string]$Message) Write-Host "windows-tools: $Message" }
${truffleHogFunctions}
${body}
`);
    const result = spawnSync(process.platform === "win32" ? "powershell.exe" : "pwsh", [
      "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", fixtureScript,
    ], { encoding: "utf8", timeout: 30000 });
    if (result.error?.code === "ENOENT") { t.skip("PowerShell is not installed"); return; }
    assert.equal(result.status, 0, result.stdout + result.stderr);
    return result;
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
}

const truffleHogInstallFixture = `
function Resolve-TruffleHogAsset { @{ Name = 'amd64'; SHA256 = (Get-FileHash $fixture).Hash } }
function Retry { param([scriptblock]$ScriptBlock) & $ScriptBlock }
function Invoke-WebRequest { param($Uri, $OutFile, [switch]$UseBasicParsing) Copy-Item -LiteralPath $fixture -Destination $OutFile }
${script.slice(script.indexOf("function Assert-FileSHA256"), script.indexOf("function Install-VerifiedChocolateyPackage"))}
function tar.exe { param($xzf, $C, $entry) Copy-Item -LiteralPath $xzf -Destination (Join-Path $C $entry); $global:LASTEXITCODE = 0 }
function Add-MachinePath { param($Path) $script:addedPath = $Path }
`;

test("TruffleHog installs a runnable candidate before atomic replacement and reuses the installed binary", async (t) => {
  const result = await truffleHogFixture(t, `${truffleHogInstallFixture}
Install-TruffleHog
$target = Join-Path $TruffleHogInstallDir 'trufflehog.exe'
if (-not (Test-TruffleHogBinary $target)) { throw 'installed binary did not run' }
if ((Get-ChildItem $TruffleHogInstallDir).Count -ne 1) { throw 'candidate was not cleaned up' }
if ($script:addedPath -ne $TruffleHogInstallDir) { throw 'installation was not added to PATH' }
function Invoke-WebRequest { throw 'unexpected second download' }
Install-TruffleHog
Write-Output 'install-ok'
`);
  if (result) assert.match(result.stdout, /install-ok/);
});

test("TruffleHog rejects a wrong version without replacing the existing target", async (t) => {
  await truffleHogFixture(t, `${truffleHogInstallFixture}
New-Item -ItemType Directory -Path $TruffleHogInstallDir | Out-Null
$target = Join-Path $TruffleHogInstallDir 'trufflehog.exe'
Copy-Item -LiteralPath $fixture -Destination $target
Add-Content -LiteralPath $target -Value '# existing target'
$before = (Get-FileHash $target).Hash
$env:CRABBOX_TEST_STDOUT = 'trufflehog 3.95.90'
$rejected = $false
try { Install-TruffleHog } catch { $rejected = $_.Exception.Message -match 'did not report version' }
if (-not $rejected) { throw 'wrong version was accepted' }
if ((Get-FileHash $target).Hash -ne $before) { throw 'existing target was replaced' }
if ((Get-ChildItem $TruffleHogInstallDir).Count -ne 1) { throw 'failed candidate was not cleaned up' }
`);
});

test("TruffleHog failures report the native exit code and both output streams without polluting the boolean result", async (t) => {
  const result = await truffleHogFixture(t, `
$env:CRABBOX_TEST_STDERR = 'fixture loader diagnostic'
$env:CRABBOX_TEST_EXIT = '17'
$valid = @(Test-TruffleHogBinary $fixture)
if ($valid.Count -ne 1 -or $valid[0] -isnot [bool] -or $valid[0]) { throw 'invalid boolean result' }
`);
  if (result) {
    assert.match(result.stdout, /exit code[=: ]+17/i);
    assert.match(result.stdout, /trufflehog 3\.95\.9/);
    assert.match(result.stdout, /fixture loader diagnostic/);
  }
});

test("TruffleHog accepts stderr version output and diagnoses launch failures", async (t) => {
  const result = await truffleHogFixture(t, `
$env:CRABBOX_TEST_STDOUT = ''
$env:CRABBOX_TEST_STDERR = 'trufflehog 3.95.9'
if (-not (Test-TruffleHogBinary $fixture)) { throw 'stderr version was rejected' }
$invalid = Join-Path $env:TEMP 'invalid.exe'
Set-Content -LiteralPath $invalid -Value 'not an executable'
if (Test-TruffleHogBinary $invalid) { throw 'invalid executable was accepted' }
`);
  if (result) assert.match(result.stdout, /exit code[=: ]+not started/i);
});

for (const [build, expected, succeeds] of [[20348, 20348, true], [26100, 26100, true], [20348, 26100, false]]) {
  test(`Windows smoke checks guest build ${build} against requested ${expected}`, async (t) => {
    const text = await readFile("scripts/devtools-image-smoke-windows.ps1", "utf8");
    const prelude = text.split("Get-ComputerInfo")[0];
    const result = spawnSync(process.platform === "win32" ? "powershell.exe" : "pwsh", [
      "-NoProfile", "-NonInteractive", "-Command",
      `$ExpectedWindowsBuild = '${expected}'; function Get-CimInstance { @{ BuildNumber = '${build}' } }; ${prelude}; Write-Output 'guest-version-ok'`,
    ], { encoding: "utf8", timeout: 30000 });
    if (result.error?.code === "ENOENT") return t.skip("PowerShell is not installed");
    assert.equal(result.status === 0, succeeds, result.stderr);
    if (succeeds) assert.match(result.stdout, /guest-version-ok/);
    else {
      assert.match(result.stderr, /Windows image OS mismatch/);
      assert.doesNotMatch(result.stdout, /guest-version-ok/);
    }
  });
}

for (const file of ["install-windows-developer-tools.ps1", "devtools-image-smoke-windows.ps1"]) {
  for (const [build, tag] of [[20348, "ltsc2022"], [26100, "ltsc2025"], [17763, undefined]]) {
    test(`${file} chooses a host-compatible Server Core image for build ${build}`, async (t) => {
      const text = await readFile(`scripts/${file}`, "utf8");
      const selection = text.match(/\$WindowsBuild = [\s\S]*?\n\}/)?.[0];
      assert.ok(selection, "missing host build selection");
      const shell = process.platform === "win32" ? "powershell.exe" : "pwsh";
      const result = spawnSync(shell, ["-NoProfile", "-NonInteractive", "-Command",
        `$ErrorActionPreference = 'Stop'; function Get-CimInstance { @{ BuildNumber = '${build}' } }; ${selection}; Write-Output $ServerCoreTag`],
        { encoding: "utf8", timeout: 30000 });
      if (result.error?.code === "ENOENT") return t.skip("PowerShell is not installed");
      if (tag) {
        assert.equal(result.status, 0, result.stderr);
        assert.equal(result.stdout.trim(), tag);
      } else {
        assert.notEqual(result.status, 0);
        assert.match(result.stderr, /Unsupported Windows Server build/);
      }
      assert.match(text, /mcr\.microsoft\.com\/windows\/servercore:\$ServerCoreTag/);
    });
  }
}

test("Windows developer tools prep verifies a versioned Chocolatey package before installation", () => {
  assert.match(script, /CRABBOX_WINDOWS_CHOCO_PACKAGE_URL/);
  assert.match(script, /CRABBOX_WINDOWS_CHOCO_PACKAGE_SHA256/);
  assert.match(script, /https:\/\/community\.chocolatey\.org\/api\/v2\/package\/chocolatey\/2\.7\.3/);
  assert.match(script, /40778cc59245b3eb6ea5147aeef5bea5d577419e5abce22a224189740dc16db5/);
  assert.match(script, /Get-FileHash -LiteralPath \$Path -Algorithm SHA256/);
  assert.match(script, /Assert-FileSHA256 -Path \$package -Expected \$SHA256 -Name "Chocolatey package"/);
  assert.match(script, /Expand-Archive -LiteralPath \$package -DestinationPath \$extractDir -Force/);
  assert.match(script, /& \$installScript/);
  assert.doesNotMatch(script, /community\.chocolatey\.org\/install\.ps1/);
  assert.doesNotMatch(script, /DownloadString/);
  assert.doesNotMatch(script, /Invoke-Expression/);

  const download = script.indexOf("Invoke-WebRequest -Uri $Url -OutFile $package");
  const verify = script.indexOf("Assert-FileSHA256 -Path $package");
  const extract = script.indexOf("Expand-Archive -LiteralPath $package");
  const execute = script.indexOf("& $installScript");
  assert.ok(download >= 0, "package download must be present");
  assert.ok(verify > download, "checksum verification must follow the download");
  assert.ok(extract > verify, "package extraction must follow checksum verification");
  assert.ok(execute > extract, "package installation must follow extraction");
});

test("Windows developer tools prep verifies the Node MSI before installation", () => {
  assert.match(script, /CRABBOX_WINDOWS_NODE_SHA256/);
  assert.match(script, /f0f66c2a80c08a30a5ab5179ee9ea9e45f9b46289436a8cc87ff833b852db351/);
  assert.match(
    script,
    /CRABBOX_WINDOWS_NODE_SHA256 is required when CRABBOX_WINDOWS_NODE_VERSION overrides \$DefaultNodeVersion/,
  );

  const start = script.indexOf("function Install-Node");
  const end = script.indexOf("function Enable-CorepackPnpm");
  const installNode = script.slice(start, end);
  const download = installNode.indexOf("Invoke-WebRequest -Uri $url -OutFile $msi");
  const verify = installNode.indexOf('Assert-FileSHA256 -Path $msi -Expected $NodeSHA256 -Name "Node MSI"');
  const install = installNode.indexOf('Start-Process -FilePath "msiexec.exe"');
  const cleanup = installNode.indexOf("Remove-Item -Recurse -Force -LiteralPath $workDir");
  assert.ok(download >= 0, "Node MSI download must be present");
  assert.ok(verify > download, "Node MSI verification must follow the download");
  assert.ok(install > verify, "Node MSI installation must follow verification");
  assert.ok(cleanup > install, "Node MSI cleanup must follow installation");
});

test("Windows developer tools prep verifies the Docker archive before extraction and service registration", () => {
  assert.match(script, /CRABBOX_WINDOWS_DOCKER_SHA256/);
  assert.match(script, /7008d54da30461fa745d4539beb87d3d14dd38c7ab0110657720526e16f5f2d3/);
  assert.match(
    script,
    /CRABBOX_WINDOWS_DOCKER_SHA256 is required when CRABBOX_WINDOWS_DOCKER_VERSION overrides \$DefaultDockerVersion/,
  );

  const start = script.indexOf("function Install-StaticDockerEngine");
  const end = script.indexOf("function Install-DockerEngine");
  const installDocker = script.slice(start, end);
  assert.match(installDocker, /-not \$dockerService\) \{/);
  const download = installDocker.indexOf("Invoke-WebRequest -Uri $url -OutFile $zip");
  const verify = installDocker.indexOf(
    'Assert-FileSHA256 -Path $zip -Expected $DockerSHA256 -Name "Docker Engine archive"',
  );
  const extract = installDocker.indexOf("Expand-Archive -LiteralPath $zip");
  const cleanup = installDocker.indexOf("Remove-Item -Recurse -Force -LiteralPath $workDir");
  const register = installDocker.indexOf("& $dockerd --register-service");
  assert.ok(download >= 0, "Docker archive download must be present");
  assert.ok(verify > download, "Docker archive verification must follow the download");
  assert.ok(extract > verify, "Docker archive extraction must follow verification");
  assert.ok(cleanup > extract, "Docker archive cleanup must follow extraction");
  assert.ok(register > cleanup, "Docker service registration must follow verified extraction");
});

test("Windows developer tools prep verifies and atomically installs pinned TruffleHog archives", () => {
  assert.match(script, /\$TruffleHogVersion = "3\.95\.9"/);
  assert.match(script, /25cc731f678922c870edba49f19c324aa6c8e7190b551c4fbe49d0c4e1c5446a/);
  assert.match(script, /df982afbf72d1c1a125e4871b624b7f959b2f62caa40e4d14bf861fb93c237bb/);
  assert.match(script, /Resolve-TruffleHogAsset/);

  const start = script.indexOf("function Install-TruffleHog");
  const end = script.indexOf("function Install-StaticDockerEngine");
  const installTruffleHog = script.slice(start, end);
  assert.match(installTruffleHog, /if \(Test-TruffleHogBinary -Path \$target\) \{/);
  assert.match(installTruffleHog, /TruffleHog \$TruffleHogVersion is already installed/);
  const download = installTruffleHog.indexOf("Invoke-WebRequest -Uri $url -OutFile $archive");
  const verify = installTruffleHog.indexOf(
    'Assert-FileSHA256 -Path $archive -Expected $asset.SHA256 -Name "TruffleHog archive"',
  );
  const extract = installTruffleHog.indexOf("& tar.exe -xzf $archive");
  const validate = installTruffleHog.indexOf("Test-TruffleHogBinary -Path $candidate");
  const replace = installTruffleHog.indexOf("Move-Item -LiteralPath $candidate -Destination $target -Force");
  assert.ok(download >= 0, "TruffleHog download must be present");
  assert.ok(verify > download, "checksum verification must follow the download");
  assert.ok(extract > verify, "archive extraction must follow verification");
  assert.ok(validate > extract, "candidate validation must follow extraction");
  assert.ok(replace > validate, "atomic replacement must follow candidate validation");
});
