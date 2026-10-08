package hyperv

import (
	"context"
	"fmt"
	"github.com/openclaw/crabbox/internal/providers/shared"
	"os"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	windowsTailscaleVersion              = "1.98.4"
	windowsTailscaleMSIURL               = "https://pkgs.tailscale.com/stable/tailscale-setup-1.98.4-amd64.msi"
	windowsTailscaleMSISHA256            = "95fa8601a7195411f5d0685bb650f239b2831d9939456c8d3ae8e286c85b1746"
	hypervTailscaleDeferredExitNodeLabel = "tailscale_exit_node_deferred"
)

func (b *backend) bootstrapWindowsTailscale(ctx context.Context, vmName, user string, cfg core.Config) error {
	if !cfg.Tailscale.Enabled {
		return nil
	}
	authKey := strings.TrimSpace(cfg.Tailscale.AuthKey)
	if authKey == "" {
		return core.Exit(2, "provider=%s target=windows requires a Tailscale auth key when --tailscale is enabled", providerName)
	}
	scriptBlock := windowsTailscaleBootstrapPowerShell(cfg)
	hostScript := fmt.Sprintf(
		`%sInvoke-Command -VMName '%s' -Credential $cred -ArgumentList $env:_CRABBOX_TS_AUTHKEY -ScriptBlock { param([string]$authKey) %s }`,
		powershellCredentialPrelude(user), escapePSString(vmName), scriptBlock,
	)
	env := append(os.Environ(),
		"_CRABBOX_GP="+b.guestPassword(),
		"_CRABBOX_TS_AUTHKEY="+authKey,
	)
	result, err := b.invokeGuestScript(ctx, hostScript, env, b.guestInvokeTimeout)
	if err == nil {
		return nil
	}
	b.logoutWindowsTailscaleBestEffort(ctx, vmName, user)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return shared.LocalCommandError("Windows Tailscale bootstrap", result, err)
}

func (b *backend) logoutWindowsTailscaleBestEffort(_ context.Context, vmName, user string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	script := core.TailscaleLogoutScript(core.TargetWindows, core.WindowsModeNormal)
	if _, err := b.invokeInGuestOnce(ctx, vmName, user, script, "Windows Tailscale logout"); err != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: Tailscale logout failed for Hyper-V VM %s: %v\n", vmName, err)
	}
}

func (b *backend) logoutLinuxTailscaleBestEffort(target core.SSHTarget) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := b.logoutTailscale(ctx, target)
	if err == nil {
		return
	}
	detail := strings.TrimSpace(out)
	if detail == "" {
		detail = err.Error()
	}
	fmt.Fprintf(b.rt.Stderr, "warning: Tailscale logout failed for Hyper-V Linux guest %s: %s\n", target.Host, detail)
}

func windowsTailscaleBootstrapPowerShell(cfg core.Config) string {
	paths := core.TailscaleGuestMetadataPaths(core.TargetWindows, core.WindowsModeNormal)
	hostname := escapePSString(strings.TrimSpace(cfg.Tailscale.Hostname))
	tags := escapePSString(strings.Join(cfg.Tailscale.Tags, ","))
	exitNode := escapePSString(strings.TrimSpace(cfg.Tailscale.ExitNode))
	joinExitNode := exitNode
	if cfg.Tailscale.ExitNode != "" && !cfg.Tailscale.ExitNodeAllowLANAccess {
		joinExitNode = ""
	}
	controlURL := escapePSString(strings.TrimSpace(os.Getenv("TS_CONTROL_URL")))
	allowLANArg := ""
	if joinExitNode != "" && cfg.Tailscale.ExitNodeAllowLANAccess {
		allowLANArg = `$upArgs.Add('--exit-node-allow-lan-access'); `
	}
	return fmt.Sprintf(
		`$ErrorActionPreference = 'Stop'; `+
			`$ProgressPreference = 'SilentlyContinue'; `+
			`if ([string]::IsNullOrWhiteSpace($authKey)) { throw 'Tailscale auth key was not provided' }; `+
			`$tailscalePath = $null; `+
			`$command = Get-Command tailscale.exe -ErrorAction SilentlyContinue; `+
			`if ($command) { $tailscalePath = $command.Source }; `+
			`if (-not $tailscalePath) { $candidate = Join-Path $env:ProgramFiles 'Tailscale\tailscale.exe'; if (Test-Path -LiteralPath $candidate) { $tailscalePath = $candidate } }; `+
			`if (-not $tailscalePath) { `+
			`$msiPath = Join-Path $env:TEMP 'crabbox-tailscale-%s.msi'; `+
			`try { `+
			`Invoke-WebRequest -UseBasicParsing -Uri '%s' -OutFile $msiPath; `+
			`$actualHash = (Get-FileHash -Algorithm SHA256 -LiteralPath $msiPath).Hash.ToLowerInvariant(); `+
			`if ($actualHash -ne '%s') { throw "Tailscale MSI SHA-256 mismatch: $actualHash" }; `+
			`$quotedMSIPath = '"' + $msiPath + '"'; `+
			`$installer = Start-Process -FilePath (Join-Path $env:SystemRoot 'System32\msiexec.exe') -ArgumentList @('/i',$quotedMSIPath,'/qn','/norestart') -Wait -PassThru; `+
			`if ($installer.ExitCode -notin @(0,1641,3010)) { throw "Tailscale MSI install failed with exit code $($installer.ExitCode)" } `+
			`} finally { Remove-Item -LiteralPath $msiPath -Force -ErrorAction SilentlyContinue }; `+
			`$candidate = Join-Path $env:ProgramFiles 'Tailscale\tailscale.exe'; `+
			`if (-not (Test-Path -LiteralPath $candidate)) { throw 'Tailscale MSI completed but tailscale.exe is absent' }; `+
			`$tailscalePath = $candidate `+
			`}; `+
			`%s`+
			`$service = Get-Service -Name Tailscale -ErrorAction Stop; `+
			`if ($service.Status -ne 'Running') { Start-Service -Name Tailscale; $service.WaitForStatus('Running',[TimeSpan]::FromSeconds(30)) }; `+
			`$authPath = Join-Path $env:TEMP ('crabbox-ts-auth-' + [Guid]::NewGuid().ToString('N')); `+
			`try { `+
			`[System.IO.File]::WriteAllText($authPath,$authKey,[System.Text.UTF8Encoding]::new($false)); `+
			`$upArgs = [System.Collections.Generic.List[string]]::new(); `+
			`$upArgs.Add('up'); $upArgs.Add('--reset'); $upArgs.Add('--auth-key=file:' + $authPath); `+
			`if ('%s') { $upArgs.Add('--hostname=%s') }; `+
			`if ('%s') { $upArgs.Add('--advertise-tags=%s') }; `+
			`if ('%s') { $upArgs.Add('--exit-node=%s') }; `+
			`%s`+
			`if ('%s') { $upArgs.Add('--login-server=%s') }; `+
			`& $tailscalePath @upArgs; `+
			`if ($LASTEXITCODE -ne 0) { throw "tailscale up failed with exit code $LASTEXITCODE" } `+
			`} finally { $authKey = $null; Remove-Item -LiteralPath $authPath -Force -ErrorAction SilentlyContinue }; `+
			`$ipv4 = $null; `+
			`for ($attempt = 0; $attempt -lt 24 -and -not $ipv4; $attempt++) { `+
			`$candidateIPv4 = [string](& $tailscalePath ip -4 2>$null | Where-Object { -not [string]::IsNullOrWhiteSpace($_) } | Select-Object -First 1); `+
			`$parsedIPv4 = $null; `+
			`if ($candidateIPv4 -and [System.Net.IPAddress]::TryParse($candidateIPv4,[ref]$parsedIPv4) -and $parsedIPv4.AddressFamily -eq [System.Net.Sockets.AddressFamily]::InterNetwork) { $ipv4 = $candidateIPv4 }; `+
			`if (-not $ipv4) { Start-Sleep -Seconds 5 } `+
			`}; `+
			`if (-not $ipv4) { throw 'Tailscale did not publish an IPv4 address' }; `+
			`$status = & $tailscalePath status --json | ConvertFrom-Json -ErrorAction Stop; `+
			`$fqdn = [string]$status.Self.DNSName; `+
			`$deviceID = [string]$(if ($status.Self.ID) { $status.Self.ID } elseif ($status.Self.NodeID) { $status.Self.NodeID } else { $status.Self.StableID }); `+
			`$version = [string](& $tailscalePath version | Select-Object -First 1); `+
			`$metadataDir = '%s'; New-Item -ItemType Directory -Force -Path $metadataDir | Out-Null; `+
			`function Set-CrabboxTailscaleMetadata([string]$name,[string]$value) { `+
			`$path = Join-Path $metadataDir $name; $temp = $path + '.' + [Guid]::NewGuid().ToString('N') + '.tmp'; `+
			`[System.IO.File]::WriteAllText($temp,[string]$value,[System.Text.UTF8Encoding]::new($false)); Move-Item -LiteralPath $temp -Destination $path -Force `+
			`}; `+
			`Set-CrabboxTailscaleMetadata 'ipv4' $ipv4; `+
			`Set-CrabboxTailscaleMetadata 'hostname' '%s'; `+
			`Set-CrabboxTailscaleMetadata 'fqdn' $fqdn; `+
			`Set-CrabboxTailscaleMetadata 'version' $version; `+
			`Set-CrabboxTailscaleMetadata 'device-id' $deviceID; `+
			`if ('%s') { Set-CrabboxTailscaleMetadata 'exit-node' '%s'; Set-CrabboxTailscaleMetadata 'exit-node-allow-lan-access' '%s' }`,
		windowsTailscaleVersion, windowsTailscaleMSIURL, windowsTailscaleMSISHA256,
		core.TailscaleIdentityResetScript(core.TargetWindows, core.WindowsModeNormal),
		hostname, hostname, tags, tags, joinExitNode, joinExitNode,
		allowLANArg,
		controlURL, controlURL, escapePSString(paths.Directory), hostname,
		exitNode, exitNode, escapePSString(fmt.Sprint(cfg.Tailscale.ExitNodeAllowLANAccess)),
	)
}

func hypervTailscaleBootstrapConfig(cfg core.Config) core.Config {
	cfg.Tailscale.ScrubCloudInitSecrets = cfg.Tailscale.Enabled
	cfg.Tailscale.ResetIdentityBeforeUp = cfg.Tailscale.Enabled
	if cfg.Tailscale.ExitNode != "" && !cfg.Tailscale.ExitNodeAllowLANAccess {
		cfg.Tailscale.DeferExitNode = true
	}
	return cfg
}

func markHyperVTailscaleLabels(labels map[string]string, cfg core.Config) {
	if cfg.Tailscale.Enabled && cfg.Tailscale.ExitNode != "" && !cfg.Tailscale.ExitNodeAllowLANAccess {
		labels[hypervTailscaleDeferredExitNodeLabel] = "true"
	}
}

func (b *backend) UpdateTailscaleMetadata(ctx context.Context, lease core.LeaseTarget, meta core.TailscaleMetadata) (core.Server, error) {
	_ = ctx
	leaseID := strings.TrimSpace(lease.LeaseID)
	name := strings.TrimSpace(shared.FirstNonBlank(lease.Server.CloudID, lease.Server.Labels["instance"]))
	claim, ok, exact, err := core.ResolveLeaseClaimForProviderWithExact(leaseID, providerName)
	if err != nil {
		return core.Server{}, err
	}
	serverInstance := strings.TrimSpace(lease.Server.Labels["instance"])
	serverLeaseID := strings.TrimSpace(lease.Server.Labels["lease"])
	if !ok || !exact || claim.LeaseID != leaseID || name == "" ||
		claim.CloudID != name || claim.ProviderScope != instanceScope(name) || instanceNameFromClaim(claim) != name ||
		(serverInstance != "" && serverInstance != name) || (serverLeaseID != "" && serverLeaseID != leaseID) ||
		(lease.Server.Provider != "" && lease.Server.Provider != providerName) {
		return core.Server{}, core.Exit(4, "hyperv lease %q has no exact local claim bound to VM %q; refusing Tailscale metadata update", leaseID, name)
	}
	server := lease.Server
	server.Labels = make(map[string]string, len(claim.Labels))
	for key, value := range claim.Labels {
		server.Labels[key] = value
	}
	core.ApplyTailscaleMetadataToServer(&server, meta)
	updated, err := core.UpdateLeaseClaimEndpointIfUnchanged(leaseID, claim, server, lease.SSH)
	if err != nil {
		return core.Server{}, err
	}
	core.SetServerLeaseClaimSnapshot(&server, updated, true)
	return server, nil
}
