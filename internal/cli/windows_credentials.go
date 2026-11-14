package cli

import "fmt"

// WindowsActionsRunnerCredentialPath is the guest-local credential escrow used
// to launch a durable GitHub Actions runner as the selected Windows user.
const WindowsActionsRunnerCredentialPath = `C:\ProgramData\crabbox\windows.password`

// windowsRunnerCredentialReadPowerShell sets $logonValue to the escrowed
// password exactly as stored (strict UTF-8, never trimmed), or $null when the
// escrow is absent. An empty or undecodable escrow is an error.
func windowsRunnerCredentialReadPowerShell(path string) string {
	return fmt.Sprintf(`$logonPath = %s
$logonValue = $null
$credentialBytes = $null
try {
  $credentialBytes = [IO.File]::ReadAllBytes($logonPath)
} catch [IO.FileNotFoundException], [IO.DirectoryNotFoundException] {
  $credentialBytes = $null
} catch {
  throw
}
if ($null -ne $credentialBytes) {
  if ($credentialBytes.Length -eq 0) {
    throw %s
  }
  $credentialUTF8 = [Text.UTF8Encoding]::new($false, $true)
  try {
    $logonValue = $credentialUTF8.GetString($credentialBytes)
  } catch [Text.DecoderFallbackException] {
    throw %s
  }
}
`, psQuote(path), psQuote("Windows runner credential file is empty"), psQuote("Windows runner credential file is not valid UTF-8"))
}
