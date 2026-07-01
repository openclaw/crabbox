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
