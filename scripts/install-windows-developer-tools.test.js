import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import { spawnSync } from "node:child_process";

const script = await readFile("scripts/install-windows-developer-tools.ps1", "utf8");

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
  assert.match(script, /catch \{\s+return \$false\s+\}/);
  assert.match(script, /& \$Path --no-update --version/);

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
