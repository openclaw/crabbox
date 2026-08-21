$ErrorActionPreference = "Stop"

Write-Output "devtools-smoke-ok"
Get-ComputerInfo | Select-Object OsName, OsVersion, OsBuildNumber | Format-List
git --version
gh --version | Select-Object -First 1
jq --version
rg --version | Select-Object -First 1
fd --version
python --version
node --version
$nodeMajor = [int](node -p "process.versions.node.split('.')[0]")
if ($nodeMajor -lt 24) {
  throw "Node.js 24 or newer is required, found major $nodeMajor"
}
npm --version
corepack --version
pnpm --version
trufflehog --no-update --version
docker --version
docker version
docker image inspect mcr.microsoft.com/windows/servercore:ltsc2022 | Out-Null
