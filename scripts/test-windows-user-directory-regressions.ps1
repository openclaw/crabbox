#Requires -Version 5.1
<#
.SYNOPSIS
Runs the Windows user-directory regression gate.

.DESCRIPTION
These packages resolve per-user configuration, state, and cache directories, so a
run that inherits the ambient profile can pass by reusing state another test left
behind. Every invocation below therefore gets a fresh HOME/USERPROFILE/APPDATA/
LOCALAPPDATA and no XDG_* overrides, which is what makes the failures deterministic
on Windows.

Go derives its module and build caches from the same profile variables, so the
caches are resolved once up front and pinned explicitly. Without that, each
isolated invocation would re-download modules and recompile from scratch.
#>
[CmdletBinding()]
param(
    [string]$TestTimeout = '20m'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repoRoot = Split-Path -Parent $PSScriptRoot
Set-Location -LiteralPath $repoRoot

function Get-GoEnvValue {
    param([Parameter(Mandatory)][string]$Name)

    $lines = @(& go env $Name)
    if ($LASTEXITCODE -ne 0) {
        throw "go env $Name failed with exit code $LASTEXITCODE"
    }
    return ([string]::Join([Environment]::NewLine, [string[]]$lines)).Trim()
}

# Resolve caches before the profile variables are redirected.
$goModCache = Get-GoEnvValue -Name 'GOMODCACHE'
$goBuildCache = Get-GoEnvValue -Name 'GOCACHE'

$isolationRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("crabbox-userdirs-" + [guid]::NewGuid().ToString('N'))
$script:isolationIndex = 0

function Invoke-IsolatedGoTest {
    param(
        [Parameter(Mandatory)][string]$Description,
        [Parameter(Mandatory)][string[]]$GoTestArgument
    )

    $script:isolationIndex++
    $caseRoot = Join-Path $isolationRoot ("case-{0:d2}" -f $script:isolationIndex)
    $profileHome = Join-Path $caseRoot 'home'
    $roaming = Join-Path $caseRoot 'appdata\roaming'
    $local = Join-Path $caseRoot 'appdata\local'
    New-Item -ItemType Directory -Force -Path $profileHome, $roaming, $local | Out-Null

    $preserved = @{}
    $managed = @(
        'HOME', 'USERPROFILE', 'APPDATA', 'LOCALAPPDATA',
        'XDG_CONFIG_HOME', 'XDG_STATE_HOME', 'XDG_CACHE_HOME', 'XDG_DATA_HOME',
        'GOMODCACHE', 'GOCACHE'
    )
    foreach ($name in $managed) {
        $preserved[$name] = [Environment]::GetEnvironmentVariable($name)
    }

    try {
        [Environment]::SetEnvironmentVariable('HOME', $profileHome)
        [Environment]::SetEnvironmentVariable('USERPROFILE', $profileHome)
        [Environment]::SetEnvironmentVariable('APPDATA', $roaming)
        [Environment]::SetEnvironmentVariable('LOCALAPPDATA', $local)
        foreach ($name in @('XDG_CONFIG_HOME', 'XDG_STATE_HOME', 'XDG_CACHE_HOME', 'XDG_DATA_HOME')) {
            [Environment]::SetEnvironmentVariable($name, $null)
        }
        [Environment]::SetEnvironmentVariable('GOMODCACHE', $goModCache)
        [Environment]::SetEnvironmentVariable('GOCACHE', $goBuildCache)

        Write-Host "==> $Description"
        & go test @GoTestArgument
        $code = $LASTEXITCODE
        if ($code -ne 0) {
            throw "$Description failed with exit code $code"
        }
    } finally {
        foreach ($name in $managed) {
            [Environment]::SetEnvironmentVariable($name, $preserved[$name])
        }
    }
}

# Scoped to the behaviors this gate owns. ./internal/cli as a whole is not green on
# Windows yet, so widening this selector will report unrelated pre-existing failures
# instead of the regressions it is meant to catch.
$cliSelector = '^Test(Config|Controller|ExternalRouting|Lease|' +
    'LegacyControllerOwnerTokenIdentityIsStaleButStoppable|PondMeshCancel|' +
    'RunCommandWithPlatformStreams|RunSSHStream|WaitForLoopbackVNC|' +
    'WaitForSSHReady|WebVNC)'

Invoke-IsolatedGoTest -Description 'CLI user directory, controller, lease, and transport regressions' -GoTestArgument @(
    './internal/cli'
    '-run', $cliSelector
    '-count=1'
    '-timeout', $TestTimeout
)

# Packages that resolve per-user paths and are green on Windows. Each runs with its
# own isolated profile so ordering between them cannot mask a missing lookup.
$isolatedPackages = @(
    './internal/applevmhelper'
    './internal/providers/agentsandbox'
    './internal/providers/all'
    './internal/providers/anthropicsandboxruntime'
    './internal/providers/applevm'
    './internal/providers/asciibox'
    './internal/providers/coder'
    './internal/providers/dockersandbox'
    './internal/providers/external'
    './internal/providers/firecracker'
    './internal/providers/githubcodespaces'
    './internal/providers/hostinger'
    './internal/providers/incus'
    './internal/providers/kubevirt'
    './internal/providers/lume'
    './internal/providers/morph'
    './internal/providers/nomad'
    './internal/providers/nvidiabrev'
    './internal/providers/opencomputer'
    './internal/providers/tenki'
    './internal/providers/xcpng'
    './runtimes/aws-lambda-microvm'
)

Invoke-IsolatedGoTest -Description 'Provider and runtime user directory regressions' -GoTestArgument (
    @($isolatedPackages) + @('-count=1', '-timeout', $TestTimeout)
)

# Behaviors fixed by this work that live in packages still carrying unrelated
# pre-existing Windows failures. Named explicitly so the gate stays meaningful
# without asserting those packages are green.
$scopedPackageSelectors = @(
    @{
        Package  = './internal/providers/blacksmith'
        Selector = '^(TestBlacksmithKeepOnFailureKeepsTestboxAndWritesBundle|TestInstallVerifiedAPTKeyringScript)$'
    },
    @{
        Package  = './internal/providers/cloudrunsandbox'
        Selector = '^TestFlagsExpandLocalPathsAndPreserveGuestWorkRoot$'
    },
    @{
        Package  = './internal/providers/codesandbox'
        Selector = '^TestSDKBridgeExecutesAgainstDocumentedSDKContracts$'
    },
    @{
        Package  = './internal/providers/localcontainer'
        Selector = '^(TestAcquireTerminalContainerFailsPromptlyAndPreservesRecoveryPolicy|' +
            'TestCheckpointScopeForServerCompletesPrivateRoutingOmittedFromLabels|' +
            'TestCheckpointScopeForServerUsesExactClaimSnapshot|' +
            'TestCheckpointScopeForServerUsesPersistedLabels|' +
            'TestCleanupRemovesExpiredLocalContainers|' +
            'TestConfigForRunFallsBackToPodmanWhenDockerIsUnavailable|' +
            'TestCreateContainerMountsDockerHostUnixSocket|' +
            'TestDoctorFailsWhenLocalClaimsAreUnreadable|' +
            'TestReleaseLeaseWithIDResolvesHostWorkRoot)$'
    },
    @{
        Package  = './internal/providers/sealosdevbox'
        Selector = '^TestParseAndPersistDevboxSecretKeysRedactsMaterial$'
    }
)

foreach ($entry in $scopedPackageSelectors) {
    Invoke-IsolatedGoTest -Description ("Scoped user directory regressions in {0}" -f $entry.Package) -GoTestArgument @(
        $entry.Package
        '-run', $entry.Selector
        '-count=1'
        '-timeout', $TestTimeout
    )
}

Remove-Item -LiteralPath $isolationRoot -Recurse -Force -ErrorAction SilentlyContinue
Write-Host 'Windows user directory regression gate passed.'
