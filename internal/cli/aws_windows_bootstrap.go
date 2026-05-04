package cli

import (
	"context"
	"fmt"
	"io"
	"time"
)

func bootstrapAWSWindowsDesktop(ctx context.Context, cfg Config, target *SSHTarget, publicKey string, stderr io.Writer) error {
	if cfg.Provider != "aws" || cfg.TargetOS != targetWindows {
		return waitForSSH(ctx, target, stderr)
	}
	bootstrapTarget := *target
	bootstrapTarget.User = "Administrator"
	bootstrapTarget.WindowsMode = windowsModeNormal
	bootstrapTarget.ReadyCheck = powershellCommand(`$PSVersionTable.PSVersion | Out-Null`)
	if cfg.WindowsMode == windowsModeWSL2 {
		target.User = "Administrator"
		return bootstrapAWSWindowsWSL2(ctx, cfg, target, bootstrapTarget, publicKey, stderr)
	}
	if err := waitForSSHReady(ctx, &bootstrapTarget, stderr, "windows openssh", 20*time.Minute); err != nil {
		return err
	}
	fmt.Fprintln(stderr, "running Windows desktop bootstrap over SSH")
	remote := powershellCommand(`$ErrorActionPreference = "Stop"
$path = "C:\ProgramData\crabbox-bootstrap.ps1"
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $path) | Out-Null
$input | Set-Content -Encoding UTF8 -LiteralPath $path
powershell.exe -NoProfile -ExecutionPolicy Bypass -File $path
exit $LASTEXITCODE`)
	err := runSSHInputQuiet(ctx, bootstrapTarget, remote, windowsBootstrapPowerShell(cfg, publicKey))
	if err != nil {
		fmt.Fprintf(stderr, "warning: Windows bootstrap SSH command ended before completion; waiting for reboot/ready state: %v\n", err)
	}
	return waitForSSH(ctx, target, stderr)
}

func bootstrapAWSWindowsWSL2(ctx context.Context, cfg Config, target *SSHTarget, bootstrapTarget SSHTarget, publicKey string, stderr io.Writer) error {
	for attempt := 1; attempt <= 5; attempt++ {
		if err := waitForSSHReady(ctx, &bootstrapTarget, stderr, "windows openssh", 20*time.Minute); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "running Windows WSL2 bootstrap over SSH attempt=%d\n", attempt)
		remote := powershellCommand(`$ErrorActionPreference = "Stop"
$path = "C:\ProgramData\crabbox-bootstrap.ps1"
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $path) | Out-Null
$input | Set-Content -Encoding UTF8 -LiteralPath $path
powershell.exe -NoProfile -ExecutionPolicy Bypass -File $path
exit $LASTEXITCODE`)
		err := runSSHInputQuiet(ctx, bootstrapTarget, remote, windowsBootstrapPowerShell(cfg, publicKey))
		if err != nil {
			fmt.Fprintf(stderr, "warning: Windows WSL2 bootstrap SSH command ended before completion; waiting for reboot/ready state: %v\n", err)
		}
		if err := waitForSSHReady(ctx, &bootstrapTarget, stderr, "windows openssh", 20*time.Minute); err != nil {
			return err
		}
		if probeSSHReady(ctx, target, 20*time.Second) {
			return nil
		}
	}
	return waitForSSH(ctx, target, stderr)
}
