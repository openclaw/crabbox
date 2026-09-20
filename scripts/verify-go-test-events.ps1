param(
  [Parameter(Mandatory)]
  [AllowEmptyCollection()]
  [string[]]$Events,
  [Parameter(Mandatory)]
  [string[]]$RequiredTests,
  [Parameter(Mandatory)]
  [string]$Label
)

$ErrorActionPreference = 'Stop'
$records = @($Events | ForEach-Object { $_ | ConvertFrom-Json })
foreach ($name in $RequiredTests) {
  foreach ($action in @('run', 'pass')) {
    if (-not ($records | Where-Object { $_.Test -eq $name -and $_.Action -eq $action })) {
      throw "required $Label $name did not $action"
    }
  }
}
if ($records | Where-Object { $_.Action -eq 'skip' }) { throw "required $Label was skipped" }
