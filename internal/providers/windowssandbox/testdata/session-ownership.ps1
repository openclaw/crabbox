param([string]$HostSource, [string]$FallbackSource)
# Adapted from https://github.com/openclaw/crabbox/issues/2883#issuecomment-6257293768.
# Only audited helper/guard ASTs run. Process discovery and termination are fake.
$ErrorActionPreference = 'Stop'
Import-Module Microsoft.PowerShell.Utility
$PSModuleAutoLoadingPreference = 'None'
$script:cases = 0
$script:failures = @()

function global:Start-Process { throw 'SAFETY: native launch forbidden' }
function global:Stop-Process {
  [CmdletBinding()]param([Parameter(ValueFromPipeline=$true)]$InputObject, [int]$Id, [switch]$Force)
  process {
    $target = $InputObject
    if ($PSBoundParameters.ContainsKey('Id')) { $target = $script:inventory | Where-Object Id -eq $Id | Select-Object -First 1 }
    if ($null -eq $target -or -not $target.Fake) { throw 'SAFETY: non-fixture stop' }
    $script:stops += $target
  }
}
function global:Get-Process {
  [CmdletBinding()]param([string[]]$Name, [int]$Id)
  foreach ($p in $script:inventory) {
    if (-not $p.Fake) { throw 'SAFETY: non-fixture inventory' }
    if (($PSBoundParameters.ContainsKey('Id') -and $p.Id -eq $Id) -or ($Name -contains $p.ProcessName)) { $p }
  }
}
function global:Get-CimInstance {
  [CmdletBinding()]param([string]$ClassName)
  if ($ClassName -ne 'Win32_Process') { throw 'SAFETY: unexpected CIM class' }
  $script:rows
}
function global:Get-Date { $script:clock = $script:clock.AddSeconds(10); $script:clock }
function global:Start-Sleep { param([int]$Milliseconds, [int]$Seconds) }
function global:Write-Warning { param([string]$Message) $script:warnings += $Message }
function global:Set-Content {
  [CmdletBinding()]param([string]$LiteralPath, $Value, [string]$Encoding)
  $script:files[$LiteralPath] = $Value
}
function global:Get-Content {
  [CmdletBinding()]param([string]$LiteralPath, [switch]$Raw)
  if (-not $script:files.ContainsKey($LiteralPath)) { throw 'Missing fake identity file' }
  $script:files[$LiteralPath]
}
function global:Move-Item {
  param([string]$LiteralPath, [string]$Destination, [switch]$Force)
  $script:files[$Destination] = $script:files[$LiteralPath]
  $script:files.Remove($LiteralPath)
}

function Parse-Source([string]$Text) {
  $tokens = $null; $errors = $null
  $ast = [System.Management.Automation.Language.Parser]::ParseInput($Text, [ref]$tokens, [ref]$errors)
  if ($errors.Count) { throw ($errors | Out-String) }
  $ast
}
function Assert-InertSlice([string]$Text) {
  $ast = Parse-Source $Text
  $allowed = @('Get-SandboxProcesses','Open-SandboxProcess','Save-SandboxIdentities','Add-SandboxIdentity',
    'Update-SandboxIdentities','Get-OwnedSandboxProcesses','Report-UnconfirmedSandbox','Report-UnownedSandboxProcesses',
    'Stop-SandboxSession','Wait-SandboxSession','Get-Process','Stop-Process','Get-CimInstance','Get-Date','Start-Sleep',
    'Write-Warning','Get-Content','Set-Content','Move-Item','ConvertTo-Json','ConvertFrom-Json','Where-Object')
  foreach ($command in $ast.FindAll({param($n) $n -is [System.Management.Automation.Language.CommandAst]}, $true)) {
    if ($allowed -notcontains $command.GetCommandName() -or $command.Redirections.Count -or
        $command.InvocationOperator -ne [System.Management.Automation.Language.TokenKind]::Unknown) { throw 'SAFETY: forbidden command' }
  }
  foreach ($type in $ast.FindAll({param($n) $n -is [System.Management.Automation.Language.TypeExpressionAst]}, $true)) {
    if ($type.Extent.Text -ne '[array]') { throw 'SAFETY: forbidden static type' }
  }
  foreach ($method in $ast.FindAll({param($n) $n -is [System.Management.Automation.Language.InvokeMemberExpressionAst]}, $true)) {
    if ($method.Member.Extent.Text -notin @('AddSeconds','ToUniversalTime','Kill','WaitForExit','Reverse','Dispose')) { throw 'SAFETY: forbidden method' }
    if ($method.Member.Extent.Text -in @('Kill','WaitForExit') -and $method.Expression.Extent.Text -ne '$candidate') { throw 'SAFETY: unexpected termination receiver' }
  }
  [scriptblock]::Create($Text)
}
foreach ($bad in @('Start-Process ignored','Microsoft.PowerShell.Management\Stop-Process -Id 1','& "Stop-Process" -Id 1','[System.Diagnostics.Process]::GetCurrentProcess().Kill()')) {
  $rejected = $false
  try { $null = Assert-InertSlice $bad } catch { $rejected = $true }
  if (-not $rejected) { throw 'SAFETY: negative control admitted' }
}
$hostAst = Parse-Source ([System.IO.File]::ReadAllText($HostSource))
foreach ($definition in $hostAst.FindAll({param($n) $n -is [System.Management.Automation.Language.FunctionDefinitionAst]}, $true)) {
  if ($definition.Name -eq 'Flush-Log') { continue }
  . (Assert-InertSlice $definition.Extent.Text)
}
$fallback = Assert-InertSlice ([System.IO.File]::ReadAllText($FallbackSource))

function Guard([string]$Kind) {
  $outer = @($hostAst.FindAll({param($n) $n -is [System.Management.Automation.Language.IfStatementAst]}, $true) | Where-Object {
    $condition = $_.Clauses[0].Item1.Extent.Text
    if ($Kind -eq 'normal') { $condition -eq 'Test-Path -LiteralPath $exitFile' }
    elseif ($Kind -eq 'cancel') { $condition -eq 'Test-Path -LiteralPath $cancelFile' }
    else { $condition -eq '(Get-Date) -gt $deadline' -and $_.Extent.Text.Contains('Windows Sandbox timed out') }
  })
  if ($outer.Count -ne 1) { throw 'SAFETY: ambiguous lifecycle guard' }
  $parts = @()
  foreach ($statement in $outer[0].Clauses[0].Item2.Statements) {
    if ($statement -is [System.Management.Automation.Language.AssignmentStatementAst] -and $statement.Left.Extent.Text -eq '$keepOpen') { $parts += $statement.Extent.Text }
    if ($statement -is [System.Management.Automation.Language.IfStatementAst] -and $statement.Clauses[0].Item2.Extent.Text.Contains('SandboxSession')) { $parts += $statement.Extent.Text }
  }
  if ($parts.Count -ne $(if ($Kind -eq 'timeout') {1} else {2})) { throw 'SAFETY: unexpected guard shape' }
  Assert-InertSlice ($parts -join "`n")
}
$guards = @{normal=(Guard 'normal');cancel=(Guard 'cancel');timeout=(Guard 'timeout')}

function Fake-Process([int]$Number, [string]$Name, [bool]$Owned, [int]$Generation = 0) {
  $p = [pscustomobject]@{Fake=$true;Id=$Number;ProcessName=$Name;Name=$Name;Owned=$Owned;
    Stamp=[datetime]::new(2020,1,1,0,0,$Generation,[DateTimeKind]::Utc);HasExited=$false;Handle=1;Unreadable=$false;ReplaceOnCheck=$false}
  $p | Add-Member ScriptProperty StartTime {
    if ($this.Unreadable) { throw 'fixture access denied' }
    if ($this.ReplaceOnCheck) { $script:inventory = @($script:replacement) }
    $this.Stamp
  }
  $p | Add-Member ScriptMethod Kill {
    if (-not $this.Fake) { throw 'SAFETY: non-fixture kill' }
    $script:stops += $this
    $this.HasExited = $true
  }
  $p | Add-Member ScriptMethod WaitForExit { param([int]$Timeout) $this.HasExited }
  $p | Add-Member ScriptMethod Dispose { }
  $p
}
function Reset-Fixture([string]$Layout, [string]$Scenario) {
  $script:stops=@(); $script:warnings=@(); $script:rows=@(); $script:files=@{}; $script:ownershipWarning=$false
  $script:clock=[datetime]::new(2020,1,1)
  $script:IdentityPath='fake-session.json'
  $names = if ($Layout -eq 'legacy') {@('WindowsSandbox','WindowsSandboxClient')} else {@('WindowsSandboxServer','WindowsSandboxRemoteSession')}
  $script:process=Fake-Process 41000 'WindowsSandbox' $true
  $script:original=@((Fake-Process 41001 $names[0] $true 1),(Fake-Process 41002 $names[1] $true 2))
  $script:identities = @($script:process)+$script:original | ForEach-Object {
    [pscustomobject]@{Id=$_.Id;StartTimeTicks=$_.Stamp.Ticks;Stop=$true}
  }
  $script:replacement=Fake-Process $(if ($Scenario -eq 'pid-reuse') {41001} else {42001}) $names[0] $false 3
  $script:inventory=@($script:process)+$script:original
  if ($Scenario -eq 'handoff') { $script:process.HasExited=$true; $script:inventory=$script:original }
  if ($Scenario -in @('replacement','pid-reuse','absent')) {
    $script:process.HasExited=$true
    $script:inventory=@()
    if ($Scenario -ne 'absent') { $script:inventory=@($script:replacement) }
  }
  if ($Scenario -eq 'unreadable') { foreach ($p in $script:inventory) { $p.Unreadable=$true } }
  $script:files[$IdentityPath] = ConvertTo-Json -InputObject @($script:identities) -Compress
}
function Check([string]$Label, [bool]$Cleanup) {
  $script:cases++
  $unowned=@($script:stops | Where-Object {-not $_.Owned})
  if ($unowned.Count -or ($Cleanup -and $script:stops.Count -eq 0) -or (-not $Cleanup -and $script:stops.Count)) {
    $script:failures += "$Label stops=$($script:stops.Count) unowned=$($unowned.Count)"
  }
}
foreach ($layout in @('legacy','modern')) {
  foreach ($scenario in @('live','handoff','replacement','pid-reuse','absent','unreadable')) {
    foreach ($path in @('normal','cancel','timeout','fallback')) {
      foreach ($policy in @('none','keep','keep-on-failure-success','keep-on-failure-error')) {
        Reset-Fixture $layout $scenario
        $script:Keep=[pscustomobject]@{IsPresent=($policy -eq 'keep')}
        $script:KeepOnFailure=[pscustomobject]@{IsPresent=$policy.StartsWith('keep-on-failure')}
        $script:exitCode=if ($policy -eq 'keep-on-failure-error') {7} else {0}
        $suppressed = $Keep.IsPresent -or ($KeepOnFailure.IsPresent -and ($path -ne 'normal' -or $exitCode -ne 0))
        if ($path -eq 'fallback') { if (-not $suppressed) { . $fallback } } else { . $guards[$path] }
        Check "$layout/$scenario/$path/$policy" (-not $suppressed -and $scenario -in @('live','handoff'))
        if (-not $suppressed -and $scenario -in @('replacement','pid-reuse','unreadable') -and $script:warnings.Count -eq 0) {
          $script:failures += "$layout/$scenario/$path must report unconfirmed ownership"
        }
      }
    }
  }
  Reset-Fixture $layout 'live'
  Check "$layout/admission-existing" $false
  if (@(Get-SandboxProcesses).Count -eq 0) { $script:failures += 'admission missed existing session' }
  $script:inventory=@()
  if (@(Get-SandboxProcesses).Count) { $script:failures += 'admission rejected empty inventory' }
}

# Exercise actual capture, disk record, and ancestry discovery on inert objects.
if (Get-Command Update-SandboxIdentities -ErrorAction SilentlyContinue) {
  foreach ($scenario in @('child','stale-child','reused-parent','no-parent','unknown-parent')) {
    Reset-Fixture 'modern' 'live'
    $script:identities=@()
    Add-SandboxIdentity $script:process $true
    $child=$script:original[0]
    $script:rows=@([pscustomobject]@{ProcessId=$child.Id;ParentProcessId=41000;CreationDate=$child.Stamp})
    if ($scenario -eq 'stale-child') { $script:rows[0].CreationDate=$child.Stamp.AddSeconds(-1) }
    if ($scenario -eq 'reused-parent') { $script:process.Stamp=$script:process.Stamp.AddSeconds(4) }
    if ($scenario -eq 'no-parent') { $script:process.HasExited=$true }
    if ($scenario -eq 'unknown-parent') { $script:rows[0].ParentProcessId=42000 }
    Update-SandboxIdentities
    $captured=@($script:identities | Where-Object Id -eq $child.Id).Count -eq 1
    $script:cases++
    if ($captured -ne ($scenario -eq 'child')) { $script:failures += "capture/$scenario unexpected ownership" }
  }
  foreach ($badFile in @('missing','malformed','empty')) {
    Reset-Fixture 'legacy' 'live'
    if ($badFile -eq 'missing') { $script:files.Remove($IdentityPath) }
    elseif ($badFile -eq 'malformed') { $script:files[$IdentityPath]='{' }
    else { $script:files[$IdentityPath]='[]' }
    . $fallback
    Check "fallback/$badFile" $false
    if (-not $script:files.ContainsKey($IdentityPath+'.unconfirmed')) { $script:failures += "$badFile missing preservation marker" }
  }
  Reset-Fixture 'legacy' 'live'
  $script:identities=@($script:identities[1])
  $script:original[0].ReplaceOnCheck=$true
  Stop-SandboxSession
  Check 'replacement-after-verification-uses-pinned-object' $true
}
[Console]::WriteLine("INERT_SELECTION_RESULTS total=$script:cases failures=$($script:failures.Count)")
foreach ($failure in $script:failures) { [Console]::WriteLine("FAIL $failure") }
if ($script:failures.Count) { exit 1 }
