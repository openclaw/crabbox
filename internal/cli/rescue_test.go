package cli

import (
	"bytes"
	"reflect"
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

func TestRescueCommandsCarryExternalPrivateRouting(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const leaseID = "cbx_abcdef123456"
	stored := ExternalConfig{Command: "provider-command", WorkRoot: "/work/crabbox"}
	stored.Connection.Desktop = ExternalDesktopConfig{Username: "screen-user", PasswordEnv: "SCREEN_SHARING_PASSWORD"}
	routingPath, err := PersistExternalRouting(leaseID, stored)
	if err != nil {
		t.Fatal(err)
	}
	loadedRouting, err := LoadExternalRouting(routingPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := rescueContext{
		Cfg: Config{
			Provider: "external",
			TargetOS: targetMacOS,
			External: ExternalConfig{Connection: ExternalConnectionConfig{Desktop: ExternalDesktopConfig{
				Username:    "screen-user",
				PasswordEnv: "SCREEN_SHARING_PASSWORD",
			}}},
		},
		Target:  SSHTarget{TargetOS: targetMacOS},
		LeaseID: leaseID,
	}
	for name, got := range map[string]string{
		"doctor": desktopDoctorCommand(ctx),
		"status": webVNCStatusRescueCommand(ctx),
		"reset":  webVNCResetRescueCommand(ctx),
		"daemon": webVNCDaemonStartRescueCommand(ctx),
		"retry":  desktopLaunchRetryCommand(ctx, []string{"open", "https://example.test"}),
	} {
		for _, want := range []string{
			"--provider external",
			"--target macos",
			"--external-routing-file " + routingPath,
			"--external-routing-digest " + ExternalRoutingDigest(loadedRouting),
			"--external-desktop-username screen-user",
			"--external-desktop-password-env SCREEN_SHARING_PASSWORD",
			"--id " + leaseID,
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("%s command missing %q: %s", name, want, got)
			}
		}
	}
}

func TestExternalLeaseCommandRoutingArgsSafeFlagFallback(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SCREEN_SHARING_PASSWORD", "operator-secret")
	cfg := Config{Provider: "external"}
	cfg.External.Command = "/usr/local/bin/provider"
	cfg.External.WorkRoot = "/work/crabbox"
	cfg.External.Capabilities.IdempotentLeaseID = true
	cfg.External.Connection.Desktop.Username = "screen-user"
	cfg.External.Connection.Desktop.PasswordEnv = "SCREEN_SHARING_PASSWORD"
	got := externalLeaseCommandRoutingArgs(cfg, "cbx_abcdef123456")
	want := []string{
		"--external-command", "/usr/local/bin/provider",
		"--external-work-root", "/work/crabbox",
		"--external-idempotent-lease-id=true",
		"--external-desktop-username", "screen-user",
		"--external-desktop-password-env", "SCREEN_SHARING_PASSWORD",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routing args=%#v, want %#v", got, want)
	}
	if strings.Contains(strings.Join(got, " "), "operator-secret") {
		t.Fatalf("routing args leaked desktop password: %#v", got)
	}
}

func TestExternalLeaseCommandRoutingArgsDoNotPromoteRepositoryDesktopCredentials(t *testing.T) {
	cfg := Config{Provider: "external"}
	cfg.External.Command = "/usr/local/bin/provider"
	cfg.External.WorkRoot = "/work/crabbox"
	cfg.External.Connection.Desktop.Username = "repository-user"
	cfg.External.Connection.Desktop.PasswordEnv = "GH_TOKEN"
	cfg.credentialProvenance.externalDesktopUser = credentialSourceRepository
	cfg.credentialProvenance.externalDesktopEnv = credentialSourceRepository

	got := externalLeaseCommandRoutingArgs(cfg, "cbx_abcdef123456")
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "--external-desktop-username") || strings.Contains(joined, "--external-desktop-password-env") {
		t.Fatalf("repository desktop routing was promoted to trusted flags: %#v", got)
	}
}

func TestExternalLeaseCommandRoutingArgsKeepComplexStateOffArgv(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const leaseID = "cbx_abcdef123456"
	cfg := Config{Provider: "external"}
	cfg.External.Command = "provider-command"
	cfg.External.Args = []string{"--token", "adapter-secret"}
	cfg.External.Config = map[string]any{"token": "config-secret"}
	got := externalLeaseCommandRoutingArgs(cfg, leaseID)
	routingPath, err := ExternalRoutingPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"--external-routing-file", routingPath, "--external-routing-digest", ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routing args=%#v, want fail-closed %#v", got, want)
	}
	joined := strings.Join(got, " ")
	for _, secret := range []string{"adapter-secret", "config-secret"} {
		if strings.Contains(joined, secret) {
			t.Fatalf("routing args leaked %q: %#v", secret, got)
		}
	}
}

func TestExternalLeaseCommandRoutingArgsPreserveExplicitDesktopCredentialClears(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const leaseID = "cbx_abcdef123456"
	routing := ExternalConfig{Command: "provider-command", WorkRoot: "/work/crabbox"}
	routing.Connection.Desktop = ExternalDesktopConfig{
		Username:    "stored-user",
		PasswordEnv: "STORED_DESKTOP_PASSWORD",
	}
	path, err := PersistExternalRouting(leaseID, routing)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExternalRouting(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Provider: "external", External: loaded}
	cfg.External.Connection.Desktop = ExternalDesktopConfig{}
	MarkExternalDesktopUsernameExplicit(&cfg)
	MarkExternalDesktopPasswordEnvExplicit(&cfg)

	got := externalLeaseCommandRoutingArgs(cfg, leaseID)
	want := []string{
		"--external-routing-file", path,
		"--external-routing-digest", ExternalRoutingDigest(loaded),
		"--external-desktop-username", "",
		"--external-desktop-password-env", "",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("routing args=%#v, want %#v", got, want)
	}
	command := desktopDoctorCommand(rescueContext{Cfg: cfg, LeaseID: leaseID})
	for _, clear := range []string{
		"--external-desktop-username ''",
		"--external-desktop-password-env ''",
	} {
		if !strings.Contains(command, clear) {
			t.Fatalf("rescue command lost explicit clear %q: %s", clear, command)
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
