package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

func bootstrapManagedWindowsDesktop(ctx context.Context, cfg Config, target *SSHTarget, publicKey string, stderr io.Writer) error {
	if cfg.TargetOS != targetWindows {
		return waitForSSHReady(ctx, target, stderr, "bootstrap", bootstrapWaitTimeout(cfg))
	}
	if cfg.WindowsMode == windowsModeWSL2 {
		bootstrapTarget := *target
		bootstrapTarget.WindowsMode = windowsModeNormal
		bootstrapTarget.ReadyCheck = powershellCommand(`$PSVersionTable.PSVersion | Out-Null`)
		if cfg.Provider == "aws" {
			bootstrapTarget.User = "Administrator"
			target.User = "Administrator"
		}
		return bootstrapManagedWindowsWSL2(ctx, cfg, target, bootstrapTarget, publicKey, stderr)
	}
	if cfg.Provider == "azure" && cfg.WindowsMode == windowsModeNormal && cfg.Desktop {
		bootstrapTarget := *target
		return runWindowsBootstrapOverSSH(ctx, cfg, target, bootstrapTarget, publicKey, stderr, "Windows desktop bootstrap")
	}
	if cfg.Provider != "aws" {
		return waitForSSHReady(ctx, target, stderr, "bootstrap", bootstrapWaitTimeout(cfg))
	}
	bootstrapTarget := *target
	bootstrapTarget.User = "Administrator"
	bootstrapTarget.WindowsMode = windowsModeNormal
	bootstrapTarget.ReadyCheck = powershellCommand(`$PSVersionTable.PSVersion | Out-Null`)
	phase := "Windows core bootstrap"
	if cfg.Desktop {
		phase = "Windows desktop bootstrap"
	}
	return runWindowsBootstrapOverSSH(ctx, cfg, target, bootstrapTarget, publicKey, stderr, phase)
}

func bootstrapAWSWindowsDesktop(ctx context.Context, cfg Config, target *SSHTarget, publicKey string, stderr io.Writer) error {
	return bootstrapManagedWindowsDesktop(ctx, cfg, target, publicKey, stderr)
}

func runWindowsBootstrapOverSSH(ctx context.Context, cfg Config, target *SSHTarget, bootstrapTarget SSHTarget, publicKey string, stderr io.Writer, phase string) error {
	if err := waitForSSHReady(ctx, &bootstrapTarget, stderr, "windows openssh", 20*time.Minute); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "running %s over SSH\n", phase)
	remote := powershellCommand(`$ErrorActionPreference = "Stop"
$path = "C:\ProgramData\crabbox-bootstrap.ps1"
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $path) | Out-Null
$input | Set-Content -Encoding UTF8 -LiteralPath $path
powershell.exe -NoProfile -ExecutionPolicy Bypass -File $path
exit $LASTEXITCODE`)
	var bootstrapOutput bytes.Buffer
	err := runSSHInput(ctx, bootstrapTarget, remote, strings.NewReader(windowsBootstrapPowerShell(cfg, publicKey)), &bootstrapOutput, &bootstrapOutput)
	if err != nil {
		detail := strings.TrimSpace(bootstrapOutput.String())
		if detail == "" {
			fmt.Fprintf(stderr, "warning: %s SSH command ended before completion; waiting for reboot/ready state: %v\n", phase, err)
		} else {
			fmt.Fprintf(stderr, "warning: %s SSH command ended before completion; waiting for reboot/ready state: %v\n%s\n", phase, err, detail)
		}
	}
	if err := waitForWindowsBootstrapSSHReady(ctx, target, stderr, bootstrapWaitTimeout(cfg)); err != nil {
		return err
	}
	if cfg.Desktop && cfg.WindowsMode == windowsModeNormal {
		return waitForManagedWindowsLoopbackVNC(ctx, target, stderr, 5*time.Minute)
	}
	return nil
}

func waitForWindowsBootstrapSSHReady(ctx context.Context, target *SSHTarget, stderr io.Writer, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	if err := waitForSSHReady(ctx, target, stderr, "bootstrap", timeout); err != nil {
		return err
	}
	const stableProbes = 3
	const stableInterval = 10 * time.Second
	stable := 1
	for stable < stableProbes {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return exit(5, "timed out waiting for stable Windows SSH on %s during bootstrap; %s", target.Host, sshWaitNextAction("bootstrap"))
		}
		timer := time.NewTimer(minDuration(stableInterval, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		case <-timer.C:
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return exit(5, "timed out waiting for stable Windows SSH on %s during bootstrap; %s", target.Host, sshWaitNextAction("bootstrap"))
		}
		if probeSSHReady(ctx, target, minDuration(10*time.Second, remaining)) {
			stable++
			continue
		}
		fmt.Fprintln(stderr, "Windows bootstrap SSH reached once but is still settling; waiting for stable ready-check")
		stable = 0
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return exit(5, "timed out waiting for stable Windows SSH on %s during bootstrap; %s", target.Host, sshWaitNextAction("bootstrap"))
		}
		if err := waitForSSHReady(ctx, target, stderr, "bootstrap", remaining); err != nil {
			return err
		}
		stable = 1
	}
	fmt.Fprintln(stderr, "Windows bootstrap SSH stable")
	return nil
}

func waitForManagedWindowsLoopbackVNC(ctx context.Context, target *SSHTarget, stderr io.Writer, timeout time.Duration) error {
	fmt.Fprintln(stderr, "waiting for Windows desktop VNC on 127.0.0.1:5900")
	deadline := time.Now().Add(timeout)
	for {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		for _, port := range sshPortCandidates(target.Port, target.FallbackPorts) {
			probe := *target
			probe.Port = port
			if err := probeLoopbackVNC(ctx, probe, "5", "1"); err == nil {
				target.Port = port
				fmt.Fprintln(stderr, "Windows desktop VNC ready")
				return nil
			}
		}
		if time.Now().After(deadline) {
			return exit(5, "managed Windows desktop did not expose VNC on 127.0.0.1:5900")
		}
		time.Sleep(5 * time.Second)
	}
}

func BootstrapAWSWindowsDesktop(ctx context.Context, cfg Config, target *SSHTarget, publicKey string, stderr io.Writer) error {
	return bootstrapAWSWindowsDesktop(ctx, cfg, target, publicKey, stderr)
}

func BootstrapManagedWindowsDesktop(ctx context.Context, cfg Config, target *SSHTarget, publicKey string, stderr io.Writer) error {
	return bootstrapManagedWindowsDesktop(ctx, cfg, target, publicKey, stderr)
}

func bootstrapManagedWindowsWSL2(ctx context.Context, cfg Config, target *SSHTarget, bootstrapTarget SSHTarget, publicKey string, stderr io.Writer) error {
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
		if err := waitForWindowsBootstrapSSHReady(ctx, &bootstrapTarget, stderr, 20*time.Minute); err != nil {
			return err
		}
		target.Port = bootstrapTarget.Port
		if probeSSHReady(ctx, target, 20*time.Second) {
			return nil
		}
	}
	return waitForSSHReady(ctx, target, stderr, "bootstrap", bootstrapWaitTimeout(cfg))
}
