
$crabboxSetupWasComplete = Test-Path -LiteralPath $setupCompletePath
if ($crabboxSetupWasComplete) {
  Remove-Item -LiteralPath $setupCompletePath -Force -ErrorAction Stop
}
Ensure-CrabboxWindowsRuntime
