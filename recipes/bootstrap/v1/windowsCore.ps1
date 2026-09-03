
if (-not (Test-Path -LiteralPath $passwordPath)) {
  New-CrabboxPassword | Set-Content -NoNewline -Encoding ASCII -Path $passwordPath
}
$userPassword = (Get-Content -Raw -Path $passwordPath).Trim()
if ($userPassword.Length -lt 12 -or $userPassword -notmatch '[A-Z]' -or $userPassword -notmatch '[a-z]' -or $userPassword -notmatch '[0-9]' -or $userPassword -notmatch '[^A-Za-z0-9]') {
  $userPassword = New-CrabboxPassword
  Set-Content -NoNewline -Encoding ASCII -Path $passwordPath -Value $userPassword
}
$secure = ConvertTo-SecureString $userPassword -AsPlainText -Force
if (-not (Get-LocalUser -Name $user -ErrorAction SilentlyContinue)) {
  New-LocalUser -Name $user -Password $secure -PasswordNeverExpires -AccountNeverExpires | Out-Null
} else {
  Set-LocalUser -Name $user -Password $secure -PasswordNeverExpires $true
}
Add-LocalGroupMember -Group "Administrators" -Member $user -ErrorAction SilentlyContinue
Set-Content -NoNewline -Encoding ASCII -Path $usernamePath -Value $user
$userSID = (Get-LocalUser -Name $user).SID.Value
$credentialPaths = @($passwordPath)
if ($passwordMirrorPath) {
  Set-Content -NoNewline -Encoding ASCII -Path $passwordMirrorPath -Value $userPassword
  $credentialPaths += $passwordMirrorPath
}
foreach ($credentialPath in $credentialPaths) {
  icacls.exe $credentialPath /inheritance:r /grant "*${userSID}:F" /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F" | Out-Null
}
icacls.exe $workRoot /grant "*${userSID}:(OI)(CI)F" | Out-Null
$userSSHDir = Join-Path (Join-Path "C:\Users" $user) ".ssh"
$userAuthorizedKeys = Join-Path $userSSHDir "authorized_keys"
New-Item -ItemType Directory -Force -Path $userSSHDir | Out-Null
Set-Content -Encoding ASCII -Path $userAuthorizedKeys -Value $publicKey
icacls.exe $userSSHDir /inheritance:r /grant "*${userSID}:F" /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F" | Out-Null
icacls.exe $userAuthorizedKeys /inheritance:r /grant "*${userSID}:F" /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F" | Out-Null
if (-not (Get-Service -Name sshd -ErrorAction SilentlyContinue)) {
  Retry { Invoke-WebRequest -Uri {{ps:openSSHWin64ZipURL}} -OutFile $openSSHZip -UseBasicParsing }
  Assert-CrabboxFileSHA256 $openSSHZip {{ps:openSSHWin64ZipSHA256}}
  Remove-Item -Recurse -Force $openSSHInstallRoot -ErrorAction SilentlyContinue
  Expand-Archive -LiteralPath $openSSHZip -DestinationPath (Split-Path -Parent $openSSHInstallRoot) -Force
  $expandedOpenSSH = Join-Path (Split-Path -Parent $openSSHInstallRoot) "OpenSSH-Win64"
  if (Test-Path -LiteralPath $expandedOpenSSH) {
    Rename-Item -LiteralPath $expandedOpenSSH -NewName (Split-Path -Leaf $openSSHInstallRoot) -Force
  }
  & (Join-Path $openSSHInstallRoot "install-sshd.ps1")
}
New-Item -ItemType Directory -Force -Path "$env:ProgramData\ssh" | Out-Null
Set-Content -Encoding ASCII -Path "$env:ProgramData\ssh\administrators_authorized_keys" -Value $publicKey
icacls.exe "$env:ProgramData\ssh\administrators_authorized_keys" /inheritance:r /grant "*S-1-5-32-544:F" /grant "*S-1-5-18:F" | Out-Null
$sshdConfigPath = "$env:ProgramData\ssh\sshd_config"
$sshdConfig = ""
if (Test-Path -LiteralPath $sshdConfigPath) {
  $sshdConfig = Get-Content -Raw -LiteralPath $sshdConfigPath
}
$globalLines = @()
$matchLines = @()
$inMatch = $false
foreach ($line in ($sshdConfig -split "\r?\n")) {
  if ($line -match '^\s*Match\s+') { $inMatch = $true }
  if (-not $inMatch -and $line -match '^\s*Port\s+\d+\s*$') { continue }
  if (-not $inMatch -and $line -match '^\s*Subsystem\s+sftp\s+') { continue }
  if (-not $inMatch -and $line -match '^\s*HostKey\s+') { continue }
  if (-not $inMatch -and $line -match '^\s*(PasswordAuthentication|PubkeyAuthentication)\s+') { continue }
  if ($inMatch) { $matchLines += $line } else { $globalLines += $line }
}
foreach ($port in $sshPorts) { $globalLines += "Port $port" }
$globalLines += "Subsystem sftp internal-sftp"
$globalLines += "HostKey __PROGRAMDATA__/ssh/ssh_host_ed25519_key"
$globalLines += "PubkeyAuthentication yes"
$globalLines += "PasswordAuthentication no"
if (($matchLines -join [Environment]::NewLine) -notmatch '(?im)^\s*Match\s+Group\s+administrators\b') {
  $matchLines += "Match Group administrators"
  $matchLines += "       AuthorizedKeysFile __PROGRAMDATA__/ssh/administrators_authorized_keys"
}
Set-Content -Encoding ASCII -LiteralPath $sshdConfigPath -Value (($globalLines + $matchLines) -join [Environment]::NewLine)
$sshKeygen = Resolve-CrabboxOpenSSHCommand "ssh-keygen.exe"
& $sshKeygen -A
$hostKey = "$env:ProgramData\ssh\ssh_host_ed25519_key"
if (-not (Test-Path -LiteralPath $hostKey)) {
  $hostKeyProcess = Start-Process -FilePath $sshKeygen -ArgumentList ('-q -t ed25519 -N "" -f "' + $hostKey + '"') -Wait -PassThru -NoNewWindow
  if ($hostKeyProcess.ExitCode -ne 0 -or -not (Test-Path -LiteralPath $hostKey)) {
    throw "failed to generate OpenSSH host key"
  }
}
icacls.exe $hostKey /inheritance:r /grant "*S-1-5-18:F" /grant "*S-1-5-32-544:F" | Out-Null
foreach ($port in $sshPorts) {
  $ruleName = "crabbox-sshd-$port"
  if (-not (Get-NetFirewallRule -Name $ruleName -ErrorAction SilentlyContinue)) {
    New-NetFirewallRule -Name $ruleName -DisplayName "Crabbox OpenSSH $port" -Enabled True -Direction Inbound -Protocol TCP -Action Allow -LocalPort $port | Out-Null
  }
}
Set-Service -Name sshd -StartupType Automatic
Start-Service sshd
if ((Get-Service -Name sshd).Status -ne "Running") {
  throw "sshd failed to start with generated sshd_config"
}
if (-not (Test-Path -LiteralPath "C:\Program Files\Git\cmd\git.exe")) {
  Retry { Invoke-WebRequest -Uri {{ps:gitForWindowsSetupURL}} -OutFile $gitInstaller -UseBasicParsing }
  Assert-CrabboxFileSHA256 $gitInstaller {{ps:gitForWindowsSetupSHA256}}
  Start-Process -FilePath $gitInstaller -ArgumentList "/VERYSILENT","/NORESTART","/NOCANCEL","/SP-" -Wait
}
$machinePath = [Environment]::GetEnvironmentVariable("Path", "Machine")
foreach ($path in @($openSSHInstallRoot, $openSSHSystemRoot, "C:\Program Files\Git\cmd", "C:\Program Files\Git\usr\bin")) {
  if ($machinePath -notlike "*$path*") { $machinePath = "$machinePath;$path" }
  if ($env:Path -notlike "*$path*") { $env:Path = "$env:Path;$path" }
}
[Environment]::SetEnvironmentVariable("Path", $machinePath, "Machine")
