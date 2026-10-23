package cli

import "fmt"

// WindowsGuestCredentialPath is the guest-local escrow of the selected
// Windows user's password. Interactive capture tasks read it to run as that user.
const WindowsGuestCredentialPath = `C:\ProgramData\crabbox\windows.password`

func windowsScreenshotCredentialReadPowerShell(path string) string {
	return windowsCredentialReadPowerShell(path, "Windows screenshot")
}

// windowsCredentialReadPowerShell decodes the escrow as strict UTF-8 and fails
// closed when it is missing, empty, unreadable, or malformed.
func windowsCredentialReadPowerShell(path, label string) string {
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
} else {
  throw %s
}
`, psQuote(path), psQuote(label+" credential file is empty"), psQuote(label+" credential file is not valid UTF-8"), psQuote(label+" credential file is missing"))
}
