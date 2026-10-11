$ErrorActionPreference = "Stop"
$WindowsBuild = [int](Get-CimInstance Win32_OperatingSystem).BuildNumber
$ServerCoreTag = switch ($WindowsBuild) {
  20348 { "ltsc2022" }
  26100 { "ltsc2025" }
  default { throw "Unsupported Windows Server build: $WindowsBuild" }
}
if ($ExpectedWindowsBuild -and $WindowsBuild -ne [int]$ExpectedWindowsBuild) {
  throw "Windows image OS mismatch: expected build $ExpectedWindowsBuild, found $WindowsBuild"
}
Get-ComputerInfo | Select-Object OsName, OsVersion, OsBuildNumber | Format-List
git --version
gh --version | Select-Object -First 1
jq --version
rg --version | Select-Object -First 1
fd --version
python --version
node --version
$nodeMajor = [int](node -p "process.versions.node.split('.')[0]")
if ($nodeMajor -lt 24) { throw "Node.js 24 or newer is required, found major $nodeMajor" }
npm --version
corepack --version
pnpm --version
trufflehog --no-update --version
docker --version
docker version
docker image inspect "mcr.microsoft.com/windows/servercore:$ServerCoreTag" | Out-Null
if ($LASTEXITCODE -ne 0) { throw "Docker image smoke failed: $LASTEXITCODE" }
Write-Output "devtools-smoke-ok"
