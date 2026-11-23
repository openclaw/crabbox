
function Test-CrabboxNode {
  try {
    if (-not (Get-Command node.exe -ErrorAction SilentlyContinue) -or -not (Get-Command npm.cmd -ErrorAction SilentlyContinue)) { return $false }
    & node.exe --version | Out-Null
    if ($LASTEXITCODE -ne 0) { return $false }
    & npm.cmd --version | Out-Null
    return $LASTEXITCODE -eq 0
  } catch { return $false }
}
function Ensure-CrabboxNode {
  $started = Get-Date
  $nodeRoot = Join-Path $env:ProgramFiles "nodejs"
  $machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
  $env:Path = $machinePath
  if (Test-CrabboxNode) { return }
  $nodeVersion = "24.22.3"
  $architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
  switch ($architecture) {
    "X64" { $nodeArch = "x64"; $nodeSHA256 = "e612af99f626ab415ea225af6bce5c971feb448cb5cae250bb1c3fa50c5f7d9f" }
    "Arm64" { $nodeArch = "arm64"; $nodeSHA256 = "b3cc74a295b39d4f273271e96f58c06dd2b35757ca12abec64c7d0598b434f14" }
    default { throw "unsupported Windows Node architecture: $architecture" }
  }
  $archiveName = "node-v$nodeVersion-win-$nodeArch"
  $stage = Join-Path $env:TEMP ("crabbox-node-" + [Guid]::NewGuid().ToString("N"))
  try {
    New-Item -ItemType Directory -Path $stage | Out-Null
    $zip = Join-Path $stage "node.zip"
    Retry { Invoke-WebRequest -Uri "https://nodejs.org/dist/v$nodeVersion/$archiveName.zip" -OutFile $zip -UseBasicParsing }
    Assert-CrabboxFileSHA256 $zip $nodeSHA256
    Expand-Archive -LiteralPath $zip -DestinationPath $stage
    $extracted = Join-Path $stage $archiveName
    & (Join-Path $extracted "node.exe") --version | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "downloaded Node runtime failed" }
    & (Join-Path $extracted "npm.cmd") --version | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "downloaded npm failed" }
    New-Item -ItemType Directory -Force -Path $nodeRoot | Out-Null
    Copy-Item -Path (Join-Path $extracted "*") -Destination $nodeRoot -Recurse -Force
  } finally {
    Remove-Item -LiteralPath $stage -Recurse -Force -ErrorAction SilentlyContinue
  }
  # Prepend the repaired prefix so a broken earlier installation cannot mask it.
  $machinePath = (@($nodeRoot) + @($machinePath -split ";" | Where-Object { $_ -and $_.TrimEnd("\") -ine $nodeRoot })) -join ";"
  [Environment]::SetEnvironmentVariable("Path", $machinePath, "Machine")
  $env:Path = $machinePath + ";" + [Environment]::GetEnvironmentVariable("Path", "User")
  if (-not (Test-CrabboxNode)) { throw "Node/npm baseline failed verification" }
  Write-Output ("Node baseline installed in {0:N1}s" -f ((Get-Date) - $started).TotalSeconds)
}
Ensure-CrabboxNode
