param([string]$Root, [string]$PathsFile)
$ErrorActionPreference = 'Stop'
$paths = Get-Content -LiteralPath $PathsFile -Raw -Encoding UTF8 | ConvertFrom-Json
$rows = foreach ($relative in $paths) {
    $file = if ($relative -eq '') { $Root } else { Join-Path $Root $relative }
    $item = Get-Item -LiteralPath $file -Force
    [pscustomobject]@{ path = $relative; attributes = [int]$item.Attributes }
}
ConvertTo-Json -InputObject @($rows) -Compress
