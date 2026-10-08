package cli

import "fmt"

// WindowsActionsRunnerCredentialPath is the guest-local credential escrow used
// to launch a durable GitHub Actions runner as the selected Windows user.
const WindowsActionsRunnerCredentialPath = `C:\ProgramData\crabbox\windows.password`

func windowsRunnerCredentialReadPowerShell(path string) string {
	return windowsCredentialReadPowerShell(path, "Windows runner", false)
}

func windowsScreenshotCredentialReadPowerShell(path string) string {
	return windowsCredentialReadPowerShell(path, "Windows screenshot", true)
}

func windowsVideoCredentialReadPowerShell(path string) string {
	return windowsCredentialReadPowerShell(path, "Windows video", true)
}

func windowsCredentialReadPowerShell(path, label string, required bool) string {
	missing := "$logonValue = $null"
	if required {
		missing = fmt.Sprintf("throw %s", psQuote(label+" credential file is missing"))
	}
	return fmt.Sprintf(`$logonPath = %s
$logonValue = $null
if (Test-Path -LiteralPath $logonPath) {
  $credentialBytes = [IO.File]::ReadAllBytes($logonPath)
  if ($credentialBytes.Length -eq 0) {
    throw %s
  }
  $credentialUTF8 = [Text.UTF8Encoding]::new($false, $true)
  try {
    $logonValue = $credentialUTF8.GetString($credentialBytes)
  } catch [Text.DecoderFallbackException] {
    throw %s
  }
} else {
  %s
}
`, psQuote(path), psQuote(label+" credential file is empty"), psQuote(label+" credential file is not valid UTF-8"), missing)
}
