import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";

const script = await readFile("scripts/install-windows-developer-tools.ps1", "utf8");

test("Windows developer tools prep verifies Chocolatey installer before execution", () => {
  assert.match(script, /CRABBOX_WINDOWS_CHOCO_INSTALL_URL/);
  assert.match(script, /CRABBOX_WINDOWS_CHOCO_INSTALL_SHA256/);
  assert.match(script, /https:\/\/community\.chocolatey\.org\/install\.ps1/);
  assert.match(script, /44e045ed5350758616d664c5af631e7f2cd10165f5bf2bd82cbf3a0bb8f63462/);
  assert.match(script, /Get-FileHash -LiteralPath \$Path -Algorithm SHA256/);
  assert.match(script, /Assert-FileSHA256 -Path \$installer -Expected \$SHA256 -Name \$Name/);
  assert.match(script, /powershell\.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File \$installer/);
  assert.doesNotMatch(script, /DownloadString/);
  assert.doesNotMatch(script, /Invoke-Expression/);

  const download = script.indexOf("Invoke-WebRequest -Uri $Url -OutFile $installer");
  const verify = script.indexOf("Assert-FileSHA256 -Path $installer");
  const execute = script.indexOf("powershell.exe -NoLogo -NoProfile -ExecutionPolicy Bypass -File $installer");
  assert.ok(download >= 0, "installer download must be present");
  assert.ok(verify > download, "checksum verification must follow the download");
  assert.ok(execute > verify, "installer execution must follow checksum verification");
});
