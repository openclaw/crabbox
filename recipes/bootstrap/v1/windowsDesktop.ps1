
	if (-not (Test-Path -LiteralPath "C:\Program Files\TightVNC\tvnserver.exe")) {
	  Retry { Invoke-WebRequest -Uri {{ps:tightVNCMSIURL}} -OutFile $tightVNCInstaller -UseBasicParsing }
	  Assert-CrabboxFileSHA256 $tightVNCInstaller {{ps:tightVNCMSISHA256}}
	  $vncPassword = Get-Content -Raw -Path $vncPasswordPath
	  Start-Process -FilePath msiexec.exe -ArgumentList @(
    "/i", $tightVNCInstaller, "/quiet", "/norestart",
    "ADDLOCAL=Server",
    "SERVER_REGISTER_AS_SERVICE=1",
    "SERVER_ADD_FIREWALL_EXCEPTION=0",
    "SET_USEVNCAUTHENTICATION=1", "VALUE_OF_USEVNCAUTHENTICATION=1",
    "SET_PASSWORD=1", "VALUE_OF_PASSWORD=$vncPassword",
    "SET_USECONTROLAUTHENTICATION=1", "VALUE_OF_USECONTROLAUTHENTICATION=1",
    "SET_CONTROLPASSWORD=1", "VALUE_OF_CONTROLPASSWORD=$vncPassword",
    "SET_ALLOWLOOPBACK=1", "VALUE_OF_ALLOWLOOPBACK=1",
    "SET_ACCEPTHTTPCONNECTIONS=1", "VALUE_OF_ACCEPTHTTPCONNECTIONS=0"
  ) -Wait
}
$startupTask = "CrabboxUserVNC"
cmd.exe /c "schtasks.exe /Delete /TN $startupTask /F 2>NUL" | Out-Null
Remove-Item -Force -LiteralPath "C:\ProgramData\crabbox\start-user-vnc.ps1" -ErrorAction SilentlyContinue
Remove-Item -Force -LiteralPath (Join-Path (Join-Path (Join-Path "C:\Users" $user) "AppData\Roaming\Microsoft\Windows\Start Menu\Programs\Startup") "crabbox-user-vnc.cmd") -ErrorAction SilentlyContinue
Get-Process tvnserver -ErrorAction SilentlyContinue | Where-Object { $_.SessionId -ne 0 } | Stop-Process -Force -ErrorAction SilentlyContinue
Get-Service -Name tvnserver | Set-Service -StartupType Automatic
Start-Service -Name tvnserver
$winlogon = "HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon"
$oobe = "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\OOBE"
if (-not (Test-Path -LiteralPath $oobe)) {
  New-Item -Path $oobe | Out-Null
}
# Azure's Windows 11 client images can leave the privacy-consent page in the
# first interactive session even after provisioning has created the account.
# Mark the remaining first-logon gates complete before auto-logon so the
# desktop is actually usable by Crabbox's desktop transport.
New-ItemProperty -Force -Path $oobe -Name PrivacyConsentStatus -PropertyType DWord -Value 1 | Out-Null
New-ItemProperty -Force -Path $oobe -Name SetupDisplayedEula -PropertyType DWord -Value 1 | Out-Null
New-ItemProperty -Force -Path $oobe -Name SkipMachineOOBE -PropertyType DWord -Value 1 | Out-Null
New-ItemProperty -Force -Path $oobe -Name SkipUserOOBE -PropertyType DWord -Value 1 | Out-Null
$systemPolicies = "HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\Policies\System"
New-ItemProperty -Force -Path $systemPolicies -Name EnableFirstLogonAnimation -PropertyType DWord -Value 0 | Out-Null
Set-ItemProperty -Path $winlogon -Name AutoAdminLogon -Value "1" -Type String
Set-ItemProperty -Path $winlogon -Name ForceAutoLogon -Value "1" -Type String
Set-ItemProperty -Path $winlogon -Name DefaultDomainName -Value $env:COMPUTERNAME -Type String
Set-ItemProperty -Path $winlogon -Name DefaultUserName -Value $user -Type String
Set-ItemProperty -Path $winlogon -Name DefaultPassword -Value $userPassword -Type String
if (-not (Test-Path -LiteralPath $setupCompletePath)) {
  Set-Content -NoNewline -Encoding ASCII -Path $setupCompletePath -Value (Get-Date).ToString("o")
	  Restart-Computer -Force
	  exit 0
	}
Restart-Service sshd
