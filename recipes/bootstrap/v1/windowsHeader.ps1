{{include:windowsUtilities}}
$user = {{user}}
$publicKey = {{publicKey}}
$workRoot = {{workRoot}}
$sshPorts = {{ports}}
$base = "C:\ProgramData\crabbox"
$setupCompletePath = Join-Path $base "setup-complete"
$openSSHZip = "$env:TEMP\OpenSSH-Win64.zip"
$openSSHInstallRoot = "C:\Program Files\OpenSSH"
$openSSHSystemRoot = Join-Path $env:WINDIR "System32\OpenSSH"
$gitInstaller = "$env:TEMP\Git-2.56.0.3-64-bit.exe"
New-Item -ItemType Directory -Force -Path $base, $workRoot | Out-Null
New-Item -Path "HKLM:\SYSTEM\CurrentControlSet\Control\Network\NewNetworkWindowOff" -Force | Out-Null
Set-ItemProperty -Path "HKLM:\SOFTWARE\Microsoft\ServerManager" -Name DoNotOpenServerManagerAtLogon -Type DWord -Value 1 -ErrorAction SilentlyContinue
