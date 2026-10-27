
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
  $nodeVersion = "24.22.1"
  $architecture = [Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
  switch ($architecture) {
    "X64" { $nodeArch = "x64"; $nodeSHA256 = "d2046d28c2c3dcd211ac5145c0bf30e2047ef9dfb306693c8c6e2eccd8b95fa5" }
    "Arm64" { $nodeArch = "arm64"; $nodeSHA256 = "a0c807319fede24c4a0d8f84564c82a619bd313b16b10c007c383c2609e99171" }
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
