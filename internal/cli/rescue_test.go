package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestClassifyDesktopFailure(t *testing.T) {
	cases := []struct {
		output string
		want   string
	}{
		{output: "missing xdotool; warm a new --desktop lease", want: rescueInputStackDead},
		{output: "missing clipboard tool; install xclip or xsel", want: rescueClipboardUnavailable},
		{output: "clipboard helper failed while serving paste (status=124)", want: rescueClipboardDeliveryFailed},
		{output: "desktop command exited during launch (status=1)", want: rescueDesktopCommandNotLaunched},
		{output: "browser process not found", want: rescueBrowserNotLaunched},
		{output: "Error: Can't open display: :99", want: rescueDesktopSessionMissing},
		{output: "capture failed screenshot repair=restart desktop services", want: rescueScreenshotCaptureBroken},
	}
	for _, tc := range cases {
		t.Run(tc.output, func(t *testing.T) {
			if got := classifyDesktopFailure(tc.output); got != tc.want {
				t.Fatalf("problem=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestRescueCommandsCarryLeaseRoutingFlags(t *testing.T) {
	ctx := rescueContext{
		Cfg: Config{
			Provider:    "aws",
			TargetOS:    targetWindows,
			Network:     NetworkTailscale,
			WindowsMode: windowsModeWSL2,
		},
		Target:  SSHTarget{TargetOS: targetWindows, WindowsMode: windowsModeWSL2},
		LeaseID: "cbx_1",
	}
	for name, got := range map[string]string{
		"doctor": desktopDoctorCommand(ctx),
		"status": webVNCStatusRescueCommand(ctx),
		"reset":  webVNCResetRescueCommand(ctx),
		"daemon": webVNCDaemonStartRescueCommand(ctx),
	} {
		for _, want := range []string{"--provider aws", "--target windows", "--network tailscale", "--windows-mode wsl2", "--id cbx_1"} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s command missing %q: %s", name, want, got)
			}
		}
	}
	if !strings.Contains(webVNCResetRescueCommand(ctx), "--open") {
		t.Fatalf("reset command should open portal: %s", webVNCResetRescueCommand(ctx))
	}
}

func TestRescueCommandsCarryStaticHostFlags(t *testing.T) {
	ctx := rescueContext{
		Cfg: Config{
			Provider: staticProvider,
			TargetOS: targetLinux,
			Static: StaticConfig{
				Host:     "devbox.local",
				User:     "qa",
				Port:     "2222",
				WorkRoot: "/srv/crabbox",
			},
		},
		Target:  SSHTarget{TargetOS: targetLinux},
		LeaseID: "static_devbox_local",
	}
	got := desktopDoctorCommand(ctx)
	for _, want := range []string{
		"--provider ssh",
		"--target linux",
		"--static-host devbox.local",
		"--static-user qa",
		"--static-port 2222",
		"--static-work-root /srv/crabbox",
		"--id static_devbox_local",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("static rescue command missing %q: %s", want, got)
		}
	}
}

func TestRescueCommandsCarryResolvedStaticTargetFallback(t *testing.T) {
	ctx := rescueContext{
		Cfg:     Config{Provider: staticProvider, TargetOS: targetLinux},
		Target:  SSHTarget{TargetOS: targetLinux, Host: "flagged.local", User: "runner", Port: "2022"},
		LeaseID: "static_flagged_local",
	}
	got := desktopDoctorCommand(ctx)
	for _, want := range []string{"--static-host flagged.local", "--static-user runner", "--static-port 2022"} {
		if !strings.Contains(got, want) {
			t.Fatalf("static rescue command missing resolved target %q: %s", want, got)
		}
	}
}

func TestPrintRescueWithFallback(t *testing.T) {
	var b bytes.Buffer
	printRescueWithFallback(&b, rescueVNCBridgeDisconnected, "dial tcp EOF", "crabbox vnc --id cbx_1 --open", "crabbox webvnc status --id cbx_1")
	got := b.String()
	for _, want := range []string{
		"problem: VNC bridge disconnected\n",
		"detail: dial tcp EOF\n",
		"rescue: crabbox webvnc status --id cbx_1\n",
		"fallback: crabbox vnc --id cbx_1 --open\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rescue output missing %q:\n%s", want, got)
		}
	}
}
