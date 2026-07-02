import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const script = await readFile("scripts/install-windows-developer-tools.ps1", "utf8");

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
  assert.match(script, /208ba5ca1dab0b330f457909e0797de340c40b34ddf2edf40d26f382f733297e/);
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
