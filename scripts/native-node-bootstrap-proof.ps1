param([Parameter(Mandatory=$true)][string]$RepositoryRoot, [Parameter(Mandatory=$true)][string]$WorkRoot, [Parameter(Mandatory=$true)][string]$OldPrefix)
$ErrorActionPreference = "Stop"
if ($env:GITHUB_ACTIONS -ne "true" -or $env:RUNNER_OS -ne "Windows") { throw "requires disposable native Windows Actions runner" }
# A Windows PowerShell grandchild must not autoload incompatible PowerShell 7 modules.
$modulePathBefore = $env:PSModulePath
$env:PSModulePath = "$PSHOME\Modules"
Import-Module Microsoft.PowerShell.Utility -ErrorAction Stop
$machineBefore = [Environment]::GetEnvironmentVariable("Path", "Machine")
$programFilesBefore = $env:ProgramFiles
$pathBefore = $env:Path
$basePath = "$env:WINDIR\System32;$env:WINDIR;$env:WINDIR\System32\WindowsPowerShell\v1.0"
$env:ProgramFiles = Join-Path $WorkRoot "program-files"
$selected = Join-Path $env:ProgramFiles "nodejs"
$stateFile = Join-Path $WorkRoot "existing-state.json"
Set-Content -LiteralPath $stateFile -Value '{"existing":"usable","value":42}' -Encoding ASCII
$stateHash = (Get-FileHash $stateFile -Algorithm SHA256).Hash
$consumerProbe = Join-Path $WorkRoot "state-consumer.cjs"
Set-Content -LiteralPath $consumerProbe -Encoding ASCII -Value 'const fs=require("node:fs");const d=JSON.parse(fs.readFileSync(process.argv[2],"utf8"));if(d.value!==42||d.existing!=="usable")process.exit(47)'
$tokens = $null; $errors = $null
$header = [System.Management.Automation.Language.Parser]::ParseFile((Join-Path $RepositoryRoot "recipes/bootstrap/v1/windowsHeader.ps1"), [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw "bootstrap header parse failed" }
foreach ($name in @("Retry", "Assert-CrabboxFileSHA256")) {
  $definition = $header.Find({param($node) $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name}, $true)
  if (-not $definition) { throw "missing canonical bootstrap helper $name" }
  Invoke-Expression $definition.Extent.Text
}
$installer = Join-Path $RepositoryRoot "recipes/bootstrap/v1/windowsNodeInstall.ps1"
function Invoke-Installer { . $installer }
function Assert-State { if ((Get-FileHash $stateFile -Algorithm SHA256).Hash -ne $stateHash) { throw "existing state changed" } }
function Assert-Selected {
  $actual = (Get-Command node.exe -ErrorAction Stop).Source
  $expected = Join-Path $selected "node.exe"
  if ($actual -ine $expected) { throw "wrong selected executable: $actual" }
  if ((& $actual --version) -ne "v24.22.1") { throw "wrong selected runtime version" }
  & (Join-Path $selected "npm.cmd") --version
  if ($LASTEXITCODE -ne 0) { throw "selected npm failed" }
  for ($pass=0; $pass -lt 2; $pass++) {
    & $actual $consumerProbe $stateFile
    if ($LASTEXITCODE -ne 0) { throw "new consumer process cannot read existing state" }
  }
  Assert-State
}
try {
  [Environment]::SetEnvironmentVariable("Path", "$OldPrefix;$basePath", "Machine")
  Invoke-Installer
  if ((& node.exe --version) -ne "v24.19.0") { throw "healthy old runtime compatibility changed" }
  Assert-State
  [Environment]::SetEnvironmentVariable("Path", "$selected;$basePath", "Machine")
  Invoke-Installer
  Assert-Selected
  $first = (Get-FileHash (Join-Path $selected "node.exe") -Algorithm SHA256).Hash
  Invoke-Installer
  Assert-Selected
  if ((Get-FileHash (Join-Path $selected "node.exe") -Algorithm SHA256).Hash -ne $first) { throw "repeat replaced healthy selected bytes" }
  [IO.File]::WriteAllText((Join-Path $selected "node.exe"), "deliberately broken private fixture")
  Invoke-Installer
  Assert-Selected
  if ((Get-FileHash (Join-Path $selected "node.exe") -Algorithm SHA256).Hash -ne $first) { throw "recovery selected different runtime bytes" }
  @{ executable=(Join-Path $selected "node.exe"); sha256=$first.ToLowerInvariant(); healthyOld="retained-24.19.0"; fresh=$true; repeat=$true; brokenRecovery=$true; consumerRestarts=2; stateSHA256=$stateHash.ToLowerInvariant() } | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $WorkRoot "component.json") -Encoding UTF8
} finally {
  [Environment]::SetEnvironmentVariable("Path", $machineBefore, "Machine")
  $env:ProgramFiles = $programFilesBefore
  $env:Path = $pathBefore
  $env:PSModulePath = $modulePathBefore
}
if ([Environment]::GetEnvironmentVariable("Path", "Machine") -cne $machineBefore) { throw "machine PATH restoration failed" }
