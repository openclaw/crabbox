package cli

func windowsInteractiveScheduledTaskPowerShell() string {
	return `
function Protect-CrabboxInteractiveDirectory([string]$Path) {
  New-Item -ItemType Directory -Force -Path $Path | Out-Null
  $userSID = [Security.Principal.WindowsIdentity]::GetCurrent().User
  $acl = [Security.AccessControl.DirectorySecurity]::new()
  $acl.SetAccessRuleProtection($true, $false)
  foreach ($sid in @($userSID, [Security.Principal.SecurityIdentifier]::new("S-1-5-32-544"), [Security.Principal.SecurityIdentifier]::new("S-1-5-18"))) {
    $rule = [Security.AccessControl.FileSystemAccessRule]::new(
      $sid, [Security.AccessControl.FileSystemRights]::FullControl,
      [Security.AccessControl.InheritanceFlags]"ContainerInherit, ObjectInherit",
      [Security.AccessControl.PropagationFlags]::None, [Security.AccessControl.AccessControlType]::Allow)
    $acl.AddAccessRule($rule)
  }
  [IO.DirectoryInfo]::new($Path).SetAccessControl($acl)
}
function Remove-CrabboxInteractiveTask([string]$TaskName) {
  Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
  Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false -ErrorAction SilentlyContinue
}
function Register-CrabboxInteractiveTask([string]$TaskName, [string]$Execute, [string]$Arguments) {
  Remove-CrabboxInteractiveTask $TaskName
  $action = New-ScheduledTaskAction -Execute $Execute -Argument $Arguments -WorkingDirectory "C:\ProgramData\crabbox"
  $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::FromMinutes(10)) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
  $userID = "$env:COMPUTERNAME\$env:USERNAME"
  $principal = New-ScheduledTaskPrincipal -UserId $userID -LogonType Interactive -RunLevel Highest
  $task = New-ScheduledTask -Action $action -Principal $principal -Settings $settings
  Register-ScheduledTask -TaskName $TaskName -InputObject $task -Force | Out-Null
}
function Start-CrabboxInteractiveTask([string]$TaskName) {
  Start-ScheduledTask -TaskName $TaskName -ErrorAction Stop
}
`
}
