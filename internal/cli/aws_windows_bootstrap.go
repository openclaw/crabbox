package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
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
		writeWindowsBootstrapSSHWarning(stderr, phase, err, bootstrapOutput.String())
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
		if probeWindowsSSHStable(ctx, target, deadline) {
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

func probeWindowsSSHStable(ctx context.Context, target *SSHTarget, deadline time.Time) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	probeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	return probeSSHReady(probeCtx, target, minDuration(10*time.Second, remaining))
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
		if err := sleepContext(ctx, 5*time.Second); err != nil {
			return context.Cause(ctx)
		}
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
		var bootstrapOutput bytes.Buffer
		bootstrapWriter := &synchronizedFanoutWriter{writers: []io.Writer{stderr, &bootstrapOutput}}
		err := runWindowsWSL2BootstrapAttempt(ctx, cfg, bootstrapTarget, publicKey, bootstrapWriter)
		if err != nil {
			writeWindowsBootstrapSSHWarning(stderr, "Windows WSL2 bootstrap", err, bootstrapOutput.String())
		}
		if err := waitForWindowsBootstrapSSHReady(ctx, &bootstrapTarget, stderr, 20*time.Minute); err != nil {
			return err
		}
		target.Port = bootstrapTarget.Port
		if probeWindowsWSL2BootstrapComplete(ctx, bootstrapTarget, target, 20*time.Second) {
			return nil
		}
		fmt.Fprintln(stderr, "Windows WSL2 setup marker is not ready after bootstrap; retrying bootstrap")
	}
	return waitForSSHReady(ctx, target, stderr, "bootstrap", bootstrapWaitTimeout(cfg))
}

type synchronizedFanoutWriter struct {
	mu      sync.Mutex
	writers []io.Writer
}

func (w *synchronizedFanoutWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, writer := range w.writers {
		if writer == nil {
			continue
		}
		if _, err := writer.Write(data); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

func writeWindowsBootstrapSSHWarning(stderr io.Writer, phase string, err error, output string) {
	detail := strings.TrimSpace(output)
	if detail == "" {
		fmt.Fprintf(stderr, "warning: %s SSH command ended before completion; waiting for reboot/ready state: %v\n", phase, err)
		return
	}
	fmt.Fprintf(stderr, "warning: %s SSH command ended before completion; waiting for reboot/ready state: %v\n%s\n", phase, err, detail)
}

func probeWindowsWSL2BootstrapComplete(ctx context.Context, bootstrapTarget SSHTarget, target *SSHTarget, timeout time.Duration) bool {
	if target.Host == "" {
		return false
	}
	profile := sshReadinessProfileForTarget(bootstrapTarget)
	timeout = profile.probeTimeout(timeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	remote := powershellCommand(`$ErrorActionPreference = "Stop"
if (-not (Test-Path -LiteralPath "C:\ProgramData\crabbox\setup-complete")) {
  throw "setup-complete marker missing"
}`)
	for _, port := range sshPortCandidates(bootstrapTarget.Port, bootstrapTarget.FallbackPorts) {
		probe := bootstrapTarget
		probe.Port = port
		probe.FallbackPorts = []string{}
		if runSSHQuietWithOptions(ctx, probe, remote, profile.connectTimeout, profile.connectionAttempts) == nil {
			target.Port = probe.Port
			return true
		}
	}
	return false
}

func runWindowsWSL2BootstrapAttempt(ctx context.Context, cfg Config, bootstrapTarget SSHTarget, publicKey string, stderr io.Writer) error {
	attemptCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	remote := powershellCommand(`$ErrorActionPreference = "Stop"
$path = "C:\ProgramData\crabbox-bootstrap.ps1"
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $path) | Out-Null
$input | Set-Content -Encoding UTF8 -LiteralPath $path
powershell.exe -NoProfile -ExecutionPolicy Bypass -File $path
exit $LASTEXITCODE`)
	err := runSSHInput(attemptCtx, bootstrapTarget, remote, strings.NewReader(windowsBootstrapPowerShell(cfg, publicKey)), stderr, stderr)
	if attemptCtx.Err() == context.DeadlineExceeded {
		return exit(7, "Windows WSL2 bootstrap command timed out after 20m0s")
	}
	return err
}
