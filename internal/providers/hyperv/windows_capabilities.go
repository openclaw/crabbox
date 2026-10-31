package hyperv

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

const (
	windowsDesktopInitialSessionTimeout = 30 * time.Second
	windowsDesktopRetrySessionTimeout   = time.Minute
	windowsDesktopReadySessionTimeout   = 5 * time.Minute
	windowsDesktopSessionPollInterval   = 2 * time.Second
	windowsDesktopSSHStableTimeout      = 45 * time.Second
	windowsDesktopAutoLogonReboots      = 2
)

func windowsCapabilityConfig(cfg core.Config, options core.LeaseOptions) core.Config {
	cfg.Desktop = cfg.Desktop || options.Desktop
	cfg.Browser = cfg.Browser || options.Browser
	return cfg
}

func provisionalWindowsCapabilityLabels(cfg core.Config, leaseID, slug string, keep bool, now time.Time) map[string]string {
	provisional := cfg
	provisional.Desktop = false
	provisional.Browser = false
	return core.DirectLeaseLabels(provisional, leaseID, slug, providerName, "", keep, now)
}

func markWindowsCapabilitiesReady(labels map[string]string, cfg core.Config) {
	if cfg.Desktop {
		labels["desktop"] = "true"
		labels["desktop_env"] = "xfce"
	}
	if cfg.Browser {
		labels["browser"] = "true"
	}
}

func (b *backend) ensureWindowsDesktopTerminal(ctx context.Context, vmName, user string) error {
	return b.invokeInGuest(
		ctx,
		vmName,
		user,
		core.ManagedWindowsDesktopTerminalBootstrapPowerShell(),
		"Windows desktop terminal install",
	)
}

func (b *backend) finalizeWindowsCapabilities(ctx context.Context, cfg core.Config, vmName string, lease *core.LeaseTarget) error {
	if cfg.Desktop {
		if err := b.waitWindowsVNC(ctx, &lease.SSH, b.rt.Stderr, 5*time.Minute); err != nil {
			return err
		}
		if err := b.waitWindowsDesktopSession(ctx, vmName, cfg.HyperV.User, windowsDesktopReadySessionTimeout); err != nil {
			return err
		}
		if err := b.waitWindowsSSHStable(ctx, &lease.SSH, b.rt.Stderr, windowsDesktopSSHStableTimeout); err != nil {
			return err
		}
	}
	if cfg.Browser {
		if err := b.probeWindowsBrowser(ctx, vmName, cfg.HyperV.User); err != nil {
			return err
		}
	}
	markWindowsCapabilitiesReady(lease.Server.Labels, cfg)
	return nil
}

func (b *backend) bootstrapWindowsDesktop(ctx context.Context, vmName, user string) error {
	bootstrapCtx, cancel := context.WithTimeout(ctx, b.guestReadyBudget+(2*b.guestInvokeTimeout))
	defer cancel()

	script := core.ManagedWindowsDesktopBootstrapPowerShell(user)
	if _, err := b.invokeInGuestWithPasswordOnce(bootstrapCtx, vmName, user, script, "Windows desktop bootstrap"); err != nil {
		fmt.Fprintf(b.rt.Stderr, "Windows desktop bootstrap interrupted while the guest may be rebooting: %v\n", err)
	}
	if err := b.waitGuestReady(bootstrapCtx, vmName, user); err != nil {
		return fmt.Errorf("guest did not return after Windows desktop bootstrap reboot: %w", err)
	}
	if err := b.invokeInGuestWithPassword(bootstrapCtx, vmName, user, script, "Windows desktop bootstrap retry"); err != nil {
		return fmt.Errorf("Windows desktop bootstrap did not complete after reboot: %w", err)
	}
	for reboot := 1; reboot <= windowsDesktopAutoLogonReboots; reboot++ {
		sessionTimeout := windowsDesktopInitialSessionTimeout
		if reboot > 1 {
			sessionTimeout = windowsDesktopRetrySessionTimeout
		}
		err := b.waitWindowsDesktopSession(bootstrapCtx, vmName, user, sessionTimeout)
		if err == nil {
			return nil
		}
		if bootstrapCtx.Err() != nil {
			return context.Cause(bootstrapCtx)
		}
		fmt.Fprintf(b.rt.Stderr, "Windows desktop auto-logon session is not active yet; rebooting the guest to apply auto-logon attempt=%d/%d: %v\n", reboot, windowsDesktopAutoLogonReboots, err)
		if err := b.restartWindowsDesktopSession(bootstrapCtx, vmName, user); err != nil {
			return err
		}
		if err := b.waitGuestReady(bootstrapCtx, vmName, user); err != nil {
			return fmt.Errorf("guest did not return after Windows desktop auto-logon reboot %d: %w", reboot, err)
		}
		if err := b.invokeInGuestWithPassword(bootstrapCtx, vmName, user, script, "Windows desktop bootstrap after auto-logon reboot"); err != nil {
			return fmt.Errorf("Windows desktop bootstrap did not complete after auto-logon reboot %d: %w", reboot, err)
		}
	}
	return nil
}

func (b *backend) restartWindowsDesktopSession(ctx context.Context, vmName, user string) error {
	script := `$ErrorActionPreference = 'Stop'
Restart-Computer -Force
Start-Sleep -Seconds 120`
	if _, err := b.invokeInGuestOnce(ctx, vmName, user, script, "Windows desktop auto-logon reboot"); err != nil {
		fmt.Fprintf(b.rt.Stderr, "Windows desktop auto-logon reboot interrupted PowerShell Direct as expected: %v\n", err)
	}
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return nil
}

func (b *backend) waitForWindowsDesktopSession(ctx context.Context, vmName, user string, timeout time.Duration) error {
	return waitForWindowsDesktopSessionReady(ctx, timeout, windowsDesktopSessionPollInterval, func(probeCtx context.Context) error {
		_, err := b.invokeInGuestCommandOnceWithTimeout(
			probeCtx,
			vmName,
			user,
			windowsDesktopSessionProbePowerShell(user),
			"Windows desktop interactive session probe",
			false,
			b.guestReadyProbeTimeout,
		)
		return err
	})
}

func (b *backend) waitForWindowsSSHStable(ctx context.Context, target *core.SSHTarget, stderr io.Writer, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = windowsDesktopSSHStableTimeout
	}
	deadline := time.Now().Add(timeout)
	const probes = 3
	const interval = 3 * time.Second
	for probe := 1; probe <= probes; probe++ {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return core.Exit(5, "timed out waiting for stable Windows SSH after desktop bootstrap")
		}
		if err := b.sshReady(ctx, target, stderr, "hyperv windows desktop ssh", minDuration(15*time.Second, remaining)); err != nil {
			return err
		}
		if probe == probes {
			fmt.Fprintln(stderr, "Windows desktop SSH stable")
			return nil
		}
		timer := time.NewTimer(minDuration(interval, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
	return nil
}

func waitForWindowsDesktopSessionReady(ctx context.Context, timeout, interval time.Duration, probe func(context.Context) error) error {
	if timeout <= 0 {
		timeout = windowsDesktopReadySessionTimeout
	}
	if interval <= 0 {
		interval = windowsDesktopSessionPollInterval
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return core.Exit(5, "timed out waiting for an active interactive Windows session for the configured guest user: %v", lastErr)
		}
		probeCtx, cancel := context.WithTimeout(ctx, remaining)
		err := probe(probeCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		remaining = time.Until(deadline)
		if remaining <= 0 {
			return core.Exit(5, "timed out waiting for an active interactive Windows session for the configured guest user: %v", lastErr)
		}
		timer := time.NewTimer(minDuration(interval, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return context.Cause(ctx)
		case <-timer.C:
		}
	}
}

func windowsDesktopSessionProbePowerShell(user string) string {
	return fmt.Sprintf(`
$expectedUser = %s
if (-not ("CrabboxActiveWindowsSession" -as [type])) {
  Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

public static class CrabboxActiveWindowsSession {
    private const int WTSActive = 0;
    private const int WTSUserName = 5;

    [StructLayout(LayoutKind.Sequential)]
    private struct WTS_SESSION_INFO {
        public int SessionID;
        public IntPtr WinStationName;
        public int State;
    }

    [DllImport("wtsapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool WTSEnumerateSessionsW(IntPtr server, int reserved, int version, out IntPtr sessions, out int count);
    [DllImport("wtsapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool WTSQuerySessionInformationW(IntPtr server, int sessionId, int infoClass, out IntPtr buffer, out int bytes);
    [DllImport("wtsapi32.dll")]
    private static extern void WTSFreeMemory(IntPtr memory);

    private static string UserName(int sessionId) {
        IntPtr buffer;
        int bytes;
        if (!WTSQuerySessionInformationW(IntPtr.Zero, sessionId, WTSUserName, out buffer, out bytes)) return "";
        try {
            return Marshal.PtrToStringUni(buffer) ?? "";
        } finally {
            WTSFreeMemory(buffer);
        }
    }

    public static int Find(string expectedUser) {
        IntPtr sessions;
        int count;
        if (!WTSEnumerateSessionsW(IntPtr.Zero, 0, 1, out sessions, out count)) {
            return -1;
        }
        try {
            int size = Marshal.SizeOf(typeof(WTS_SESSION_INFO));
            for (int index = 0; index < count; index++) {
                WTS_SESSION_INFO session = (WTS_SESSION_INFO)Marshal.PtrToStructure(
                    IntPtr.Add(sessions, index * size),
                    typeof(WTS_SESSION_INFO));
                if (session.State == WTSActive &&
                    string.Equals(UserName(session.SessionID), expectedUser, StringComparison.OrdinalIgnoreCase)) {
                    return session.SessionID;
                }
            }
            return -1;
        } finally {
            WTSFreeMemory(sessions);
        }
    }
}
'@
}
$sessionID = [CrabboxActiveWindowsSession]::Find($expectedUser)
if ($sessionID -lt 1) {
  throw "no active interactive Windows session for the configured guest user"
}
$shell = Get-Process -Name explorer -ErrorAction SilentlyContinue | Where-Object { $_.SessionId -eq $sessionID } | Select-Object -First 1
if ($null -eq $shell) {
  throw "the configured guest user's interactive Windows session has no desktop shell"
}
Write-Output ("WINDOWS_DESKTOP_SESSION=" + $sessionID)
`, "'"+escapePSString(user)+"'")
}

func (b *backend) probeWindowsBrowser(ctx context.Context, vmName, user string) error {
	result, err := b.invokeInGuestOnce(ctx, vmName, user, core.WindowsBrowserProbePowerShell(), "Windows browser probe")
	if err != nil {
		return core.Exit(2, "browser=true requested but no supported Edge or Chrome browser was found in the Hyper-V guest: %v", err)
	}
	env := parseCapabilityEnv(result.Stdout)
	if env["BROWSER"] == "" {
		return core.Exit(2, "browser=true requested but no supported Edge or Chrome browser was found in the Hyper-V guest")
	}
	return nil
}

func (b *backend) invokeInGuestOnce(ctx context.Context, vmName, user, scriptBlock, label string) (core.LocalCommandResult, error) {
	return b.invokeInGuestCommandOnce(ctx, vmName, user, scriptBlock, label, false)
}

func parseCapabilityEnv(input string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(input, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && key != "" {
			env[key] = strings.TrimSpace(value)
		}
	}
	return env
}
