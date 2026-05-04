package cli

import (
	"context"
	"fmt"
	"io"
	"time"
)

func bootstrapAWSWindowsDesktop(ctx context.Context, cfg Config, target *SSHTarget, publicKey string, stderr io.Writer) error {
	if cfg.Provider != "aws" || cfg.TargetOS != targetWindows || cfg.WindowsMode != windowsModeNormal {
		return waitForSSH(ctx, target, stderr)
	}
	bootstrapTarget := *target
	bootstrapTarget.User = "Administrator"
	bootstrapTarget.ReadyCheck = powershellCommand(`$PSVersionTable.PSVersion | Out-Null`)
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
