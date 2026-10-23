package hyperv

import (
	"context"
	"fmt"

	core "github.com/openclaw/crabbox/internal/cli"
)

// persistWindowsGuestCredential escrows the selected user's password in the
// guest so interactive capture tasks can run as that user. The password reaches
// the host PowerShell process only through _CRABBOX_GP and the guest only
// through the remoting argument list.
func (b *backend) persistWindowsGuestCredential(ctx context.Context, vmName, user string) error {
	return b.invokeInGuestWithPassword(
		ctx,
		vmName,
		user,
		windowsGuestCredentialPowerShell(user, core.WindowsGuestCredentialPath),
		"Windows guest credential escrow",
	)
}

func windowsGuestCredentialPowerShell(user, targetPath string) string {
	return fmt.Sprintf(`
param([AllowEmptyString()][string]$Password)
$ErrorActionPreference = 'Stop'
if ($null -eq $Password -or $Password.Length -eq 0) {
  throw 'Windows guest password must not be empty'
}
$targetPath = '%s'
$selectedUser = '%s'
$selectedUserSid = ([Security.Principal.NTAccount]::new($env:COMPUTERNAME, $selectedUser)).Translate([Security.Principal.SecurityIdentifier])
$allowedSids = @(
  $selectedUserSid,
  [Security.Principal.SecurityIdentifier]::new('S-1-5-32-544'),
  [Security.Principal.SecurityIdentifier]::new('S-1-5-18')
)
$directory = Split-Path -Parent $targetPath
[IO.Directory]::CreateDirectory($directory) | Out-Null
$tempPath = Join-Path $directory ('.windows.password.' + [Guid]::NewGuid().ToString('N') + '.tmp')
$tempStream = $null
$replaced = $false
$committed = $false
try {
  $tempStream = [IO.FileStream]::new(
    $tempPath,
    [IO.FileMode]::CreateNew,
    [Security.AccessControl.FileSystemRights]::FullControl,
    [IO.FileShare]::None,
    4096,
    [IO.FileOptions]::WriteThrough
  )
  $acl = [Security.AccessControl.FileSecurity]::new()
  $acl.SetAccessRuleProtection($true, $false)
  foreach ($sid in $allowedSids) {
    $rule = [Security.AccessControl.FileSystemAccessRule]::new(
      $sid,
      [Security.AccessControl.FileSystemRights]::FullControl,
      [Security.AccessControl.InheritanceFlags]::None,
      [Security.AccessControl.PropagationFlags]::None,
      [Security.AccessControl.AccessControlType]::Allow
    )
    [void]$acl.AddAccessRule($rule)
  }
  $tempStream.SetAccessControl($acl)
  $encoding = [Text.UTF8Encoding]::new($false)
  $encodedPassword = $encoding.GetBytes($Password)
  $tempStream.Write($encodedPassword, 0, $encodedPassword.Length)
  $tempStream.Flush($true)
  $tempStream.Dispose()
  $tempStream = $null

  if (-not ('Crabbox.WindowsNativeFile' -as [type])) {
    Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

namespace Crabbox {
    public static class WindowsNativeFile {
        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        public static extern bool MoveFileEx(string existingFileName, string newFileName, int flags);
    }
}
'@
  }
  $moveFileReplaceExisting = 0x1
  $moveFileWriteThrough = 0x8
  if (-not [Crabbox.WindowsNativeFile]::MoveFileEx($tempPath, $targetPath, $moveFileReplaceExisting -bor $moveFileWriteThrough)) {
    throw [ComponentModel.Win32Exception]::new([Runtime.InteropServices.Marshal]::GetLastWin32Error(), 'Atomic credential replacement failed')
  }
  $replaced = $true
  $tempPath = $null

  if (-not [IO.File]::Exists($targetPath)) {
    throw 'Windows guest credential file was not created'
  }
  $expectedBytes = $encoding.GetBytes($Password)
  $actualBytes = [IO.File]::ReadAllBytes($targetPath)
  if ($actualBytes.Length -ne $expectedBytes.Length) {
    throw 'Windows guest credential readback length mismatch'
  }
  for ($index = 0; $index -lt $expectedBytes.Length; $index++) {
    if ($actualBytes[$index] -ne $expectedBytes[$index]) {
      throw 'Windows guest credential readback mismatch'
    }
  }

  $finalAcl = [IO.File]::GetAccessControl($targetPath, [Security.AccessControl.AccessControlSections]::Access)
  if (-not $finalAcl.AreAccessRulesProtected) {
    throw 'Windows guest credential ACL is not protected'
  }
  $rules = @($finalAcl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
  if ($rules.Count -ne $allowedSids.Count) {
    throw 'Windows guest credential ACL has an unexpected rule count'
  }
  $expectedSidValues = @{}
  foreach ($sid in $allowedSids) {
    $expectedSidValues[$sid.Value] = $true
  }
  $actualSidValues = @{}
  foreach ($rule in $rules) {
    $sidValue = $rule.IdentityReference.Value
    if (
      $rule.IsInherited -or
      $rule.AccessControlType -ne [Security.AccessControl.AccessControlType]::Allow -or
      $rule.FileSystemRights -ne [Security.AccessControl.FileSystemRights]::FullControl -or
      -not $expectedSidValues.ContainsKey($sidValue) -or
      $actualSidValues.ContainsKey($sidValue)
    ) {
      throw 'Windows guest credential ACL contains an unexpected rule'
    }
    $actualSidValues[$sidValue] = $true
  }
  $committed = $true
} finally {
  if ($tempStream) {
    $tempStream.Dispose()
  }
  if ($replaced -and -not $committed -and [IO.File]::Exists($targetPath)) {
    [IO.File]::Delete($targetPath)
  }
  if ($tempPath -and [IO.File]::Exists($tempPath)) {
    [IO.File]::Delete($tempPath)
  }
}
`, escapePSString(targetPath), escapePSString(user))
}
