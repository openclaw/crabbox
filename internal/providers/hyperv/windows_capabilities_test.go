package hyperv

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestWindowsCapabilityConfigCarriesLeaseOptions(t *testing.T) {
	cfg := windowsCapabilityConfig(testBackend(&recordingRunner{}).configForRun(), core.LeaseOptions{
		Desktop: true,
		Browser: true,
	})
	if !cfg.Desktop || !cfg.Browser {
		t.Fatalf("capability config desktop=%v browser=%v, want both true", cfg.Desktop, cfg.Browser)
	}
}

func TestWindowsCapabilityLabelsAppearOnlyAfterReadiness(t *testing.T) {
	cfg := testBackend(&recordingRunner{}).configForRun()
	cfg.Desktop = true
	cfg.Browser = true

	labels := provisionalWindowsCapabilityLabels(cfg, "cbx_capability123", "capability", false, time.Unix(1_800_000_000, 0))
	if labels["desktop"] != "" || labels["browser"] != "" {
		t.Fatalf("provisional labels exposed successful capabilities: %#v", labels)
	}

	markWindowsCapabilitiesReady(labels, cfg)
	if labels["desktop"] != "true" || labels["browser"] != "true" {
		t.Fatalf("ready labels missing capabilities: %#v", labels)
	}
}

func TestEnsureWindowsDesktopTerminalInstallsPinnedMintty(t *testing.T) {
	runner := &recordingRunner{}
	b := testBackend(runner)

	if err := b.ensureWindowsDesktopTerminal(context.Background(), "crabbox-terminal-1234", "crabbox"); err != nil {
		t.Fatalf("ensureWindowsDesktopTerminal: %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("PowerShell calls=%d want 1", len(runner.calls))
	}
	command := strings.Join(runner.calls[0].Args, "\n")
	for _, want := range []string{
		`C:\Program Files\Git\cmd\git.exe`,
		`C:\Program Files\Git\usr\bin\mintty.exe`,
		"Get-FileHash",
		"Git for Windows SHA-256 mismatch",
		"Restart-Service sshd -Force",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("desktop terminal install command missing %q", want)
		}
	}
}

func TestFinalizeWindowsCapabilitiesDoesNotLabelFailedReadiness(t *testing.T) {
	b := testBackend(&recordingRunner{})
	b.waitWindowsVNC = func(context.Context, *core.SSHTarget, io.Writer, time.Duration) error {
		return errors.New("VNC not ready")
	}
	cfg := b.configForRun()
	cfg.Desktop = true
	lease := core.LeaseTarget{Server: core.Server{Labels: map[string]string{}}, SSH: core.SSHTarget{}}

	if err := b.finalizeWindowsCapabilities(context.Background(), cfg, "crabbox-capability-1234", &lease); err == nil {
		t.Fatal("finalizeWindowsCapabilities succeeded before VNC readiness")
	}
	if lease.Server.Labels["desktop"] != "" {
		t.Fatalf("failed readiness persisted desktop label: %#v", lease.Server.Labels)
	}
}

func TestFinalizeWindowsCapabilitiesRequiresConfiguredUserSession(t *testing.T) {
	b := testBackend(&recordingRunner{})
	b.waitWindowsVNC = func(context.Context, *core.SSHTarget, io.Writer, time.Duration) error {
		return nil
	}
	b.waitWindowsDesktopSession = func(context.Context, string, string, time.Duration) error {
		return errors.New("configured user session is not active")
	}
	cfg := b.configForRun()
	cfg.Desktop = true
	lease := core.LeaseTarget{Server: core.Server{Labels: map[string]string{}}, SSH: core.SSHTarget{}}

	err := b.finalizeWindowsCapabilities(context.Background(), cfg, "crabbox-capability-1234", &lease)
	if err == nil || !strings.Contains(err.Error(), "configured user session") {
		t.Fatalf("finalizeWindowsCapabilities err=%v", err)
	}
	if lease.Server.Labels["desktop"] != "" {
		t.Fatalf("failed session readiness persisted desktop label: %#v", lease.Server.Labels)
	}
}

func TestFinalizeWindowsCapabilitiesOrdersReadinessBeforeLabels(t *testing.T) {
	var order []string
	runner := &recordingRunner{
		onRun: func(req core.LocalCommandRequest) {
			if strings.Contains(strings.Join(req.Args, "\n"), `Microsoft\Edge\Application\msedge.exe`) {
				order = append(order, "browser")
			}
		},
		respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
			if strings.Contains(strings.Join(req.Args, "\n"), `Microsoft\Edge\Application\msedge.exe`) {
				return core.LocalCommandResult{Stdout: "BROWSER=C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe\n"}, nil, true
			}
			return core.LocalCommandResult{}, nil, false
		},
	}
	b := testBackend(runner)
	b.waitWindowsVNC = func(context.Context, *core.SSHTarget, io.Writer, time.Duration) error {
		order = append(order, "vnc")
		return nil
	}
	b.waitWindowsDesktopSession = func(_ context.Context, _ string, user string, _ time.Duration) error {
		if user != "crabbox" {
			t.Fatalf("session user=%q want crabbox", user)
		}
		order = append(order, "session")
		return nil
	}
	b.waitWindowsSSHStable = func(context.Context, *core.SSHTarget, io.Writer, time.Duration) error {
		order = append(order, "ssh")
		return nil
	}
	cfg := b.configForRun()
	cfg.Desktop = true
	cfg.Browser = true
	lease := core.LeaseTarget{Server: core.Server{Labels: map[string]string{}}, SSH: core.SSHTarget{}}

	if err := b.finalizeWindowsCapabilities(context.Background(), cfg, "crabbox-capability-1234", &lease); err != nil {
		t.Fatalf("finalizeWindowsCapabilities: %v", err)
	}
	if got := strings.Join(order, ","); got != "vnc,session,ssh,browser" {
		t.Fatalf("readiness order=%q", got)
	}
	if lease.Server.Labels["desktop"] != "true" || lease.Server.Labels["browser"] != "true" {
		t.Fatalf("ready labels=%#v", lease.Server.Labels)
	}
}

func TestWaitForWindowsDesktopSessionReadyRetriesToSuccess(t *testing.T) {
	attempts := 0
	err := waitForWindowsDesktopSessionReady(context.Background(), time.Second, time.Millisecond, func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("session not ready")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 {
		t.Fatalf("attempts=%d want 3", attempts)
	}
}

func TestWaitForWindowsDesktopSessionReadyTimesOut(t *testing.T) {
	attempts := 0
	started := time.Now()
	err := waitForWindowsDesktopSessionReady(context.Background(), 20*time.Millisecond, time.Millisecond, func(context.Context) error {
		attempts++
		return errors.New("no active session")
	})
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for an active interactive Windows session") {
		t.Fatalf("wait error=%v", err)
	}
	if attempts < 2 {
		t.Fatalf("attempts=%d want retries", attempts)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("timeout elapsed=%s", elapsed)
	}
}

func TestWaitForWindowsDesktopSessionBoundsBlockedPowerShellDirectProbe(t *testing.T) {
	probes := 0
	runner := &recordingRunner{
		blockUntilCtx: func(req core.LocalCommandRequest) bool {
			if strings.Contains(strings.Join(req.Args, "\n"), "CrabboxActiveWindowsSession") {
				probes++
				return true
			}
			return false
		},
	}
	b := testBackend(runner)
	b.guestReadyProbeTimeout = 10 * time.Millisecond

	started := time.Now()
	err := b.waitForWindowsDesktopSession(context.Background(), "crabbox-capability-1234", "crabbox", 40*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out waiting for an active interactive Windows session") {
		t.Fatalf("wait error=%v", err)
	}
	if probes != 1 {
		t.Fatalf("probes=%d want one bounded probe within the short timeout", probes)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("blocked PowerShell Direct probe elapsed=%s", elapsed)
	}
}

func TestBootstrapWindowsDesktopRetriesExpectedRebootWithinBounds(t *testing.T) {
	desktopCalls := 0
	runner := &recordingRunner{
		blockUntilCtx: func(req core.LocalCommandRequest) bool {
			if !strings.Contains(strings.Join(req.Args, "\n"), "desktop-setup-complete") {
				return false
			}
			desktopCalls++
			return desktopCalls == 1
		},
	}
	b := testBackend(runner)
	guestPassword := b.guestPassword()
	b.guestInvokeTimeout = 20 * time.Millisecond
	b.guestReadyProbeTimeout = 20 * time.Millisecond
	b.guestReadyBudget = 200 * time.Millisecond
	b.guestRetryBackoff = time.Millisecond

	started := time.Now()
	if err := b.bootstrapWindowsDesktop(context.Background(), "crabbox-capability-1234", "crabbox"); err != nil {
		t.Fatalf("bootstrapWindowsDesktop: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("desktop reboot recovery took %s, want bounded retry", elapsed)
	}
	if desktopCalls != 2 {
		t.Fatalf("desktop bootstrap calls=%d want 2", desktopCalls)
	}

	var desktopRequest *core.LocalCommandRequest
	for i := range runner.calls {
		call := &runner.calls[i]
		if strings.Contains(strings.Join(call.Args, "\n"), "desktop-setup-complete") {
			desktopRequest = call
			break
		}
	}
	if desktopRequest == nil {
		t.Fatal("desktop bootstrap was not invoked over PowerShell Direct")
	}
	args := strings.Join(desktopRequest.Args, "\n")
	if strings.Contains(args, guestPassword) {
		t.Fatal("guest password appeared on the PowerShell argv")
	}
	if !strings.Contains(args, "-ArgumentList $env:_CRABBOX_GP") {
		t.Fatalf("desktop bootstrap did not pass the guest password as a remoting argument: %s", args)
	}
	if !strings.Contains(args, `C:\ProgramData\crabbox\vnc.password`) {
		t.Fatalf("desktop bootstrap did not use the separate VNC password path: %s", args)
	}
	if !strings.Contains(args, "LoopbackOnly") || !strings.Contains(args, "SERVER_ADD_FIREWALL_EXCEPTION=0") {
		t.Fatalf("desktop bootstrap did not enforce loopback-only VNC without a firewall exception: %s", args)
	}
	if !containsEnv(desktopRequest.Env, "_CRABBOX_GP="+guestPassword) {
		t.Fatal("guest password was not supplied through the host environment")
	}
}

func TestBootstrapWindowsDesktopBoundsAutoLogonReboots(t *testing.T) {
	countReboots := func(runner *recordingRunner) int {
		reboots := 0
		for _, call := range runner.calls {
			args := strings.Join(call.Args, "\n")
			if strings.Contains(args, "Restart-Computer -Force") && strings.Contains(args, "Start-Sleep -Seconds 120") {
				reboots++
			}
		}
		return reboots
	}

	t.Run("session never appears", func(t *testing.T) {
		runner := &recordingRunner{}
		b := testBackend(runner)
		b.guestRetryBackoff = 0
		var waits []time.Duration
		b.waitWindowsDesktopSession = func(_ context.Context, _, _ string, timeout time.Duration) error {
			waits = append(waits, timeout)
			return errors.New("no active session")
		}
		if err := b.bootstrapWindowsDesktop(context.Background(), "crabbox-capability-1234", "crabbox"); err != nil {
			t.Fatalf("bootstrapWindowsDesktop: %v", err)
		}
		// The documented contract is at most two explicit auto-logon reboots.
		if reboots := countReboots(runner); reboots != 2 {
			t.Fatalf("explicit auto-logon reboots=%d want 2", reboots)
		}
		if want := []time.Duration{30 * time.Second, time.Minute}; len(waits) != len(want) || waits[0] != want[0] || waits[1] != want[1] {
			t.Fatalf("session waits=%v want %v", waits, want)
		}
	})

	t.Run("session appears after the first reboot", func(t *testing.T) {
		runner := &recordingRunner{}
		b := testBackend(runner)
		b.guestRetryBackoff = 0
		checks := 0
		b.waitWindowsDesktopSession = func(context.Context, string, string, time.Duration) error {
			checks++
			if checks == 1 {
				return errors.New("no active session")
			}
			return nil
		}
		if err := b.bootstrapWindowsDesktop(context.Background(), "crabbox-capability-1234", "crabbox"); err != nil {
			t.Fatalf("bootstrapWindowsDesktop: %v", err)
		}
		if reboots := countReboots(runner); reboots != 1 {
			t.Fatalf("explicit auto-logon reboots=%d want 1", reboots)
		}
	})
}

func TestProbeWindowsBrowserAcceptsEdgeAndChrome(t *testing.T) {
	tests := map[string]string{
		"edge":   `C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		"chrome": `C:\Program Files\Google\Chrome\Application\chrome.exe`,
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			runner := &recordingRunner{
				respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
					if !strings.Contains(strings.Join(req.Args, "\n"), "Microsoft\\Edge\\Application\\msedge.exe") {
						return core.LocalCommandResult{}, nil, false
					}
					return core.LocalCommandResult{Stdout: "BROWSER=" + path + "\nCHROME_BIN=" + path + "\n"}, nil, true
				},
			}
			if err := testBackend(runner).probeWindowsBrowser(context.Background(), "crabbox-browser-1234", "crabbox"); err != nil {
				t.Fatalf("probeWindowsBrowser: %v", err)
			}
		})
	}
}

func TestProbeWindowsBrowserRejectsAbsentBrowser(t *testing.T) {
	err := testBackend(&recordingRunner{}).probeWindowsBrowser(context.Background(), "crabbox-browser-1234", "crabbox")
	if err == nil || !strings.Contains(err.Error(), "no supported Edge or Chrome browser") {
		t.Fatalf("probeWindowsBrowser err=%v", err)
	}
}

func TestProbeWindowsBrowserSurfacesProbeFailure(t *testing.T) {
	runner := &recordingRunner{
		respond: func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
			if strings.Contains(strings.Join(req.Args, "\n"), "Microsoft\\Edge\\Application\\msedge.exe") {
				return core.LocalCommandResult{Stderr: "probe failed"}, errors.New("exit status 1"), true
			}
			return core.LocalCommandResult{}, nil, false
		},
	}
	err := testBackend(runner).probeWindowsBrowser(context.Background(), "crabbox-browser-1234", "crabbox")
	if err == nil || !strings.Contains(err.Error(), "browser=true requested") {
		t.Fatalf("probeWindowsBrowser err=%v", err)
	}
}

func containsEnv(env []string, want string) bool {
	for _, value := range env {
		if value == want {
			return true
		}
	}
	return false
}
