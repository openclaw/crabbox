package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

const parallelsSessionUnavailable = "Unable to open new session in this virtual machine. Make sure your virtual machine has finished booting, runs the latest version of Parallels Tools, and is not isolated from the host OS."

func TestParallelsWaitForIPFailsFastWithoutTools(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(fmt.Sprintf("fallback=%t", fallback), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runner := &parallelsIPProbeRunner{t: t, started: time.Now()}
				cfg := Config{TargetOS: targetMacOS, SSHPort: "22"}
				if fallback {
					cfg.Parallels.BootstrapKey = "/Users/runner/.ssh/bootstrap"
				}
				_, err := NewParallelsClient(cfg, runner).WaitForIP(context.Background(), "vm1", 15*time.Minute, ParallelsIPWaitAcquisition)
				if !errors.Is(err, errParallelsGuestToolsUnavailable) || !ParallelsGuestToolsUnavailable(err) || ExitCodeForError(err, 1) != 5 {
					t.Fatalf("want classified Tools failure with exit 5, got %v", err)
				}
				if elapsed := time.Since(runner.started); elapsed != 150*time.Second {
					t.Fatalf("elapsed=%s, want 2m30s rather than the 15m startup timeout", elapsed)
				}
				if len(runner.probes) != 3 || runner.probes[0] < 2*time.Minute {
					t.Fatalf("expected three probes after boot grace, got %v", runner.probes)
				}
				for _, want := range []string{"clone_mode=linked", "--parallels-clone-mode full", "--parallels-startup-timeout", "--parallels-source-snapshot=", "--parallels-source-snapshot-id="} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("missing %q: %v", want, err)
					}
				}
			})
		})
	}
}

func TestParallelsWaitForIPToolsRecovery(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ipAfter     time.Duration
		dhcpAfter   time.Duration
		sshAfter    time.Duration
		execReplies []string
		wantProbes  int
		wantSource  string
	}{
		{name: "IP during boot grace", ipAfter: 30 * time.Second, wantSource: "tools"},
		{name: "Tools unavailable twice then IP", ipAfter: 140 * time.Second, wantProbes: 2, wantSource: "tools"},
		{name: "exec flake then success", ipAfter: 180 * time.Second, execReplies: []string{"PrlJob_GetRetCode: Invalid argument", ""}, wantProbes: 4, wantSource: "tools"},
		{name: "unavailable then successful probe resets count", ipAfter: 200 * time.Second, execReplies: []string{parallelsSessionUnavailable, parallelsSessionUnavailable, "", parallelsSessionUnavailable, parallelsSessionUnavailable, ""}, wantProbes: 6, wantSource: "tools"},
		{name: "unrelated failure resets count", ipAfter: 190 * time.Second, execReplies: []string{parallelsSessionUnavailable, parallelsSessionUnavailable, "transport failed", parallelsSessionUnavailable, parallelsSessionUnavailable}, wantProbes: 5, wantSource: "tools"},
		{name: "DHCP after unavailable probes", dhcpAfter: 140 * time.Second, wantProbes: 2, wantSource: "dhcp-mac"},
		{name: "DHCP address keeps waiting for SSH", dhcpAfter: 125 * time.Second, sshAfter: 180 * time.Second, wantProbes: 1, wantSource: "dhcp-mac"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runner := &parallelsIPProbeRunner{t: t, started: time.Now(), ipAfter: tc.ipAfter, dhcpAfter: tc.dhcpAfter, sshAfter: tc.sshAfter, execReplies: tc.execReplies}
				cfg := Config{TargetOS: targetMacOS, SSHPort: "22"}
				if tc.dhcpAfter > 0 {
					cfg.Parallels.BootstrapKey = "/Users/runner/.ssh/bootstrap"
				}
				vm, err := NewParallelsClient(cfg, runner).WaitForIP(context.Background(), "vm1", 15*time.Minute, ParallelsIPWaitAcquisition)
				if err != nil || vm.IP != "10.211.55.8" || vm.IPSource != tc.wantSource {
					t.Fatalf("vm=%+v err=%v", vm, err)
				}
				if len(runner.probes) != tc.wantProbes {
					t.Fatalf("probes=%v, want %d", runner.probes, tc.wantProbes)
				}
			})
		})
	}
}

func TestParallelsGuestToolsUnavailableSessionError(t *testing.T) {
	for _, message := range []string{parallelsSessionUnavailable, "PRL_ERR_VM_EXEC_GUEST_TOOL_NOT_AVAILABLE", "guest tools are not available", "guest tools not available"} {
		if !ParallelsGuestToolsUnavailable(errors.New(message)) {
			t.Errorf("did not recognize %q", message)
		}
	}
	for _, message := range []string{"PrlJob_GetRetCode: Invalid argument", "Unable to open new session in this virtual machine", "Parallels Tools update failed"} {
		if ParallelsGuestToolsUnavailable(errors.New(message)) {
			t.Errorf("misclassified %q", message)
		}
	}
}

func TestParallelsWaitForIPDeadlineBoundsCommands(t *testing.T) {
	for _, tc := range []struct {
		name       string
		block      string
		timeout    time.Duration
		fallback   bool
		dhcpAfter  time.Duration
		wantProbes int
	}{
		{name: "inventory", block: "list", timeout: 7 * time.Second},
		{name: "DHCP read", block: "/bin/cat", timeout: 7 * time.Second, fallback: true},
		{name: "SSH probe", block: "/usr/bin/nc", timeout: 7 * time.Second, fallback: true, dhcpAfter: time.Second},
		{name: "exec bounded to ten seconds", block: "exec", timeout: 170 * time.Second, wantProbes: 2},
		{name: "exec bounded to remaining startup budget", block: "exec", timeout: 125 * time.Second, wantProbes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runner := &parallelsIPProbeRunner{t: t, started: time.Now(), block: tc.block, dhcpAfter: tc.dhcpAfter}
				cfg := Config{TargetOS: targetMacOS, SSHPort: "22"}
				if tc.fallback {
					cfg.Parallels.BootstrapKey = "/Users/runner/.ssh/bootstrap"
				}
				_, err := NewParallelsClient(cfg, runner).WaitForIP(context.Background(), "vm1", tc.timeout, ParallelsIPWaitAcquisition)
				if err == nil || !strings.Contains(err.Error(), "timed out waiting") || ParallelsGuestToolsUnavailable(err) {
					t.Fatalf("want timeout, not missing Tools: %v", err)
				}
				if elapsed := time.Since(runner.started); elapsed != tc.timeout {
					t.Fatalf("elapsed=%s, want startup bound %s", elapsed, tc.timeout)
				}
				if len(runner.probes) != tc.wantProbes {
					t.Fatalf("probes=%v, want %d", runner.probes, tc.wantProbes)
				}
			})
		})
	}
}

func TestParallelsWaitForIPPreservesCancellationDuringProbe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		runner := &parallelsIPProbeRunner{t: t, started: time.Now(), block: "exec"}
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		cause := errors.New("caller canceled acquisition")
		go func() {
			time.Sleep(125 * time.Second)
			cancel(cause)
		}()
		_, err := NewParallelsClient(Config{TargetOS: targetMacOS}, runner).WaitForIP(ctx, "vm1", 15*time.Minute, ParallelsIPWaitAcquisition)
		if !errors.Is(err, cause) || time.Since(runner.started) != 125*time.Second || len(runner.probes) != 1 {
			t.Fatalf("err=%v elapsed=%s probes=%v", err, time.Since(runner.started), runner.probes)
		}
	})
}

type parallelsIPProbeRunner struct {
	t           *testing.T
	started     time.Time
	ipAfter     time.Duration
	dhcpAfter   time.Duration
	sshAfter    time.Duration
	execReplies []string
	probes      []time.Duration
	block       string
}

func (r *parallelsIPProbeRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	elapsed := time.Since(r.started)
	if req.Name == "prlctl" && req.Args[0] == "exec" {
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Sub(time.Now()) > 10*time.Second {
			r.t.Fatalf("guest-exec probe has no ten-second bound: %v, %t", deadline, ok)
		}
		r.probes = append(r.probes, elapsed)
	}
	if r.block != "" && (req.Name == r.block || req.Args[0] == r.block) {
		<-ctx.Done()
		// Even partial Tools-unavailable output from a hung exec must not
		// count as a completed unavailable probe.
		var result LocalCommandResult
		if r.block == "exec" {
			result.Stderr = parallelsSessionUnavailable
		}
		return result, ctx.Err()
	}
	switch {
	case req.Name == "prlctl" && req.Args[0] == "list":
		ip := ""
		if r.ipAfter > 0 && elapsed >= r.ipAfter {
			ip = "10.211.55.8"
		}
		return LocalCommandResult{Stdout: fmt.Sprintf(`[{"ID":"vm1","State":"running","GuestTools":{"state":"not_installed"},"ip_configured":%q,"Hardware":{"net0":{"enabled":true,"mac":"001C4233EEDD"}}}]`, ip)}, nil
	case req.Name == "prlctl" && req.Args[0] == "exec":
		message := parallelsSessionUnavailable
		if len(r.execReplies) > 0 {
			message = r.execReplies[min(len(r.probes)-1, len(r.execReplies)-1)]
		}
		if message == "" {
			return LocalCommandResult{}, nil
		}
		return LocalCommandResult{Stderr: message}, errors.New("exit status 255")
	case req.Name == "/bin/cat":
		if r.dhcpAfter > 0 && elapsed >= r.dhcpAfter {
			return LocalCommandResult{Stdout: fmt.Sprintf("[vnic0]\n10.211.55.8=\"%d,1800,001c4233eedd,01001c4233eedd\"\n", time.Now().Add(time.Hour).Unix())}, nil
		}
		return LocalCommandResult{}, nil
	case req.Name == "/usr/bin/nc":
		if elapsed < r.sshAfter {
			return LocalCommandResult{}, errors.New("connection refused")
		}
		return LocalCommandResult{}, nil
	default:
		r.t.Fatalf("unexpected command %s %v", req.Name, req.Args)
		return LocalCommandResult{}, errors.New("unexpected command")
	}
}
