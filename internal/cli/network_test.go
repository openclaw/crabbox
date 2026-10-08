package cli

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestGenericNetworkFlagInputAcceptance(t *testing.T) {
	for _, args := range [][]string{nil, {"--network=auto"}, {"--tailscale=false"}, {"--tailscale-tags="}, {"--tailscale-hostname-template="}, {"--tailscale-exit-node="}, {"--tailscale-exit-node-allow-lan-access=false"}} {
		t.Run(strings.Join(args, ","), func(t *testing.T) {
			cfg := baseConfig()
			fs := newFlagSet("network inputs", io.Discard)
			values := registerNetworkFlags(fs, cfg)
			if err := fs.Parse(args); err != nil {
				t.Fatal(err)
			}
			if err := applyNetworkFlagOverrides(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			got := cfg.inputProvenance.summary(configInputGeneric)
			if (got.state == "present") != (len(args) > 0) || got.complete {
				t.Fatalf("summary=%#v", got)
			}
		})
	}
	for _, value := range []string{"", "synthetic-value"} {
		name := "dynamic-missing"
		if value != "" {
			name = "dynamic-present"
		}
		t.Run(name, func(t *testing.T) {
			const name = "CRABBOX_TEST_GENERIC_NETWORK_INPUT"
			t.Setenv(name, value)
			cfg := baseConfig()
			fs := newFlagSet("network inputs", io.Discard)
			values := registerNetworkFlags(fs, cfg)
			if err := fs.Parse([]string{"--tailscale-auth-key-env=" + name}); err != nil {
				t.Fatal(err)
			}
			if err := applyNetworkFlagOverrides(&cfg, fs, values); err != nil {
				t.Fatal(err)
			}
			want := []string{"flag"}
			if value != "" {
				want = []string{"environment", "flag"}
			}
			got := cfg.inputProvenance.summary(configInputGeneric)
			if !reflect.DeepEqual(got.sources, want) || cfg.Tailscale.AuthKey != value {
				t.Fatal("dynamic input source or value mismatch")
			}
		})
	}
}

func TestNetworkPublicIgnoresTailscaleMetadata(t *testing.T) {
	cfg := baseConfig()
	cfg.Network = NetworkPublic
	server := Server{Labels: map[string]string{
		"lease":          "cbx_abcdef123456",
		"tailscale":      "true",
		"tailscale_fqdn": "crabbox-blue.example.ts.net",
	}}
	target := SSHTarget{Host: "203.0.113.10", Port: "2222"}
	got, err := resolveNetworkTarget(context.Background(), cfg, server, target)
	if err != nil {
		t.Fatal(err)
	}
	if got.Network != NetworkPublic || got.Target.Host != "203.0.113.10" {
		t.Fatalf("resolve public = network=%s host=%s", got.Network, got.Target.Host)
	}
}

func TestNetworkTailscaleRequiresMetadata(t *testing.T) {
	cfg := baseConfig()
	cfg.Network = NetworkTailscale
	_, err := resolveNetworkTarget(context.Background(), cfg, Server{Labels: map[string]string{"lease": "cbx_abcdef123456"}}, SSHTarget{Host: "203.0.113.10"})
	if err == nil {
		t.Fatal("expected network=tailscale without metadata to fail")
	}
}

func TestLoginOnlySSHConfigProxyIgnoresInboundTailscaleSelection(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "islo"
	cfg.Network = NetworkTailscale
	server := Server{Labels: map[string]string{
		"lease":          "isb_crabbox-repo-abcdef",
		"tailscale":      "true",
		"tailscale_fqdn": "outbound-only.example.ts.net",
	}}
	target := SSHTarget{Host: "crabbox-repo-abcdef.islo", Port: "22", SSHConfigProxy: true}
	got, err := resolveSSHTargetNetwork(context.Background(), cfg, server, target, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Target.Host != target.Host || !got.Target.SSHConfigProxy {
		t.Fatalf("login proxy target=%#v", got.Target)
	}
}

func TestSSHConfigProxyStillHonorsInboundTailscaleSelection(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.Network = NetworkTailscale
	target := SSHTarget{Host: "proxy.example", Port: "22", SSHConfigProxy: true}
	if _, err := resolveSSHTargetNetwork(context.Background(), cfg, Server{}, target, true); err == nil {
		t.Fatal("expected non-egress-only proxy target to require tailnet metadata")
	}
}

func TestBootstrapNetworkPrefersTailscaleForExitNode(t *testing.T) {
	cfg := baseConfig()
	cfg.Network = NetworkAuto
	server := Server{
		Labels: map[string]string{
			"tailscale":           "true",
			"tailscale_hostname":  "crabbox-blue-lobster",
			"tailscale_exit_node": "100.123.224.76",
			"tailscale_state":     "ready",
		},
	}
	server.PublicNet.IPv4.IP = "203.0.113.10"
	target := SSHTarget{Host: "203.0.113.10", Port: "2222"}
	got := bootstrapNetworkTarget(cfg, server, target)
	if got.Host != "crabbox-blue-lobster" || got.NetworkKind != NetworkTailscale {
		t.Fatalf("bootstrap target = host=%s network=%s", got.Host, got.NetworkKind)
	}
}

func TestBootstrapNetworkWaitsForReadyTailscaleMetadata(t *testing.T) {
	cfg := baseConfig()
	cfg.Network = NetworkTailscale
	server := Server{Labels: map[string]string{
		"tailscale":          "true",
		"tailscale_hostname": "crabbox-blue-lobster",
		"tailscale_state":    "requested",
	}}
	target := SSHTarget{Host: "203.0.113.10", Port: "2222"}
	got := bootstrapNetworkTarget(cfg, server, target)
	if got.Host != target.Host || got.NetworkKind != "" {
		t.Fatalf("bootstrap target = host=%s network=%s", got.Host, got.NetworkKind)
	}
}

func TestBootstrapNetworkAcceptsLegacyTailscaleMetadataWithoutState(t *testing.T) {
	cfg := baseConfig()
	cfg.Network = NetworkTailscale
	server := Server{Labels: map[string]string{
		"tailscale":          "true",
		"tailscale_hostname": "crabbox-legacy",
	}}
	got := bootstrapNetworkTarget(cfg, server, SSHTarget{Host: "203.0.113.10", Port: "2222"})
	if got.Host != "crabbox-legacy" || got.NetworkKind != NetworkTailscale {
		t.Fatalf("legacy bootstrap target=%#v", got)
	}
}

func TestBootstrapNetworkHonorsExplicitPublic(t *testing.T) {
	cfg := baseConfig()
	cfg.Network = NetworkPublic
	server := Server{Labels: map[string]string{
		"tailscale":           "true",
		"tailscale_hostname":  "crabbox-blue-lobster",
		"tailscale_exit_node": "100.123.224.76",
	}}
	target := SSHTarget{Host: "203.0.113.10", Port: "2222"}
	got := bootstrapNetworkTarget(cfg, server, target)
	if got.Host != "203.0.113.10" || got.NetworkKind != "" {
		t.Fatalf("bootstrap target = host=%s network=%s", got.Host, got.NetworkKind)
	}
}

func TestTailscaleExitNodeEgressCheckFailsClosed(t *testing.T) {
	script := tailscaleExitNodeEgressCheckScript()
	for _, want := range []string{
		"command -v tailscale",
		"tailscale debug prefs",
		"tailscale prefs unavailable",
		"tailscale prefs did not include ExitNodeID",
		"exit node is not selected",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("egress check script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "debug prefs 2>/dev/null || true") {
		t.Fatalf("egress check script must not ignore tailscale prefs failures:\n%s", script)
	}
}

func TestTailscaleExitNodeConfigurationIsTargetAware(t *testing.T) {
	meta := TailscaleMetadata{ExitNode: "exit.example.ts.net", ExitNodeAllowLANAccess: false}
	linux := tailscaleExitNodeConfigureScript(SSHTarget{TargetOS: TargetLinux}, meta)
	if !strings.Contains(linux, "sudo -n tailscale set") ||
		!strings.Contains(linux, "--exit-node-allow-lan-access='false'") {
		t.Fatalf("Linux exit-node script=%s", linux)
	}
	windows := tailscaleExitNodeConfigureScript(SSHTarget{TargetOS: TargetWindows, WindowsMode: WindowsModeNormal}, meta)
	if !strings.Contains(windows, "Get-Command tailscale.exe") ||
		!strings.Contains(windows, "'--exit-node-allow-lan-access=false'") {
		t.Fatalf("Windows exit-node script=%s", windows)
	}
}

func TestTailscaleMetadataParsingForWindowsAndLinux(t *testing.T) {
	output := "100.64.1.9\ncrabbox-blue\ncrabbox-blue.example.ts.net\n100.64.0.1\ntrue\n1.98.4\ndevice-123"
	for _, target := range []SSHTarget{
		{TargetOS: TargetLinux},
		{TargetOS: TargetWindows, WindowsMode: WindowsModeNormal},
	} {
		meta, err := parseTailscaleMetadataOutput(output)
		if err != nil {
			t.Fatal(err)
		}
		if meta.IPv4 != "100.64.1.9" || meta.FQDN != "crabbox-blue.example.ts.net" || meta.DeviceID != "device-123" ||
			meta.State != "ready" || !meta.ExitNodeAllowLANAccess {
			t.Fatalf("target=%s metadata=%#v", target.TargetOS, meta)
		}
		script := tailscaleMetadataReadScript(target)
		if target.TargetOS == TargetWindows {
			if !strings.Contains(script, `C:\ProgramData\crabbox\tailscale`) || strings.Contains(script, "/var/lib/crabbox") {
				t.Fatalf("Windows metadata script=%s", script)
			}
		} else if !strings.Contains(script, "/var/lib/crabbox/tailscale-ipv4") || strings.Contains(script, `C:\ProgramData`) {
			t.Fatalf("Linux metadata script=%s", script)
		} else if strings.Count(script, "tr -d '\\r\\n'") != 7 {
			t.Fatalf("Linux metadata script does not emit one trimmed record per field: %s", script)
		}
		if _, err := parseTailscaleMetadataOutput("not-an-ip\nhost\nfqdn\n\nfalse\n1.98.4\ndevice"); err == nil {
			t.Fatal("expected invalid Tailscale IPv4 metadata to fail")
		}
	}
}

func TestTailscaleLifecycleScriptsAreTargetAware(t *testing.T) {
	windows := SSHTarget{TargetOS: TargetWindows, WindowsMode: WindowsModeNormal}
	if script := tailscaleLogoutScript(windows); !strings.Contains(script, "Get-Command tailscale.exe") || strings.Contains(script, "command -v") {
		t.Fatalf("Windows logout script=%s", script)
	}
	if script := tailscaleLogoutScript(SSHTarget{TargetOS: TargetLinux}); !strings.Contains(script, "tailscale logout") || strings.Contains(script, "Get-Command") {
		t.Fatalf("Linux logout script=%s", script)
	}
	windowsReset := TailscaleIdentityResetScript(TargetWindows, WindowsModeNormal)
	if !strings.Contains(windowsReset, `C:\ProgramData\Tailscale\tailscaled.state`) ||
		!strings.Contains(windowsReset, `C:\ProgramData\crabbox\tailscale`) {
		t.Fatalf("Windows reset script=%s", windowsReset)
	}
	if !strings.Contains(windowsReset, "ErrorAction Stop") ||
		!strings.Contains(windowsReset, "identity state remains") {
		t.Fatalf("Windows reset script does not fail closed: %s", windowsReset)
	}
	linuxReset := TailscaleIdentityResetScript(TargetLinux, "")
	if !strings.Contains(linuxReset, "/var/lib/tailscale/tailscaled.state") ||
		!strings.Contains(linuxReset, "'/var/lib/crabbox'/tailscale-*") {
		t.Fatalf("Linux reset script=%s", linuxReset)
	}
	if !strings.Contains(linuxReset, "systemctl cat tailscaled.service") {
		t.Fatalf("Linux reset script does not tolerate a missing service: %s", linuxReset)
	}
}

func TestTailscaleSSHBootstrapKeepsAuthKeyOutOfRemoteScript(t *testing.T) {
	cfg := baseConfig()
	cfg.TargetOS = TargetLinux
	cfg.Tailscale.Enabled = true
	cfg.Tailscale.AuthKey = "invalid-tailscale-auth-fixture"
	cfg.Tailscale.Hostname = "crabbox-fork"
	cfg.Tailscale.ScrubCloudInitSecrets = true

	remote, input, err := tailscaleSSHBootstrapPayload(cfg, SSHTarget{TargetOS: TargetLinux})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(remote, cfg.Tailscale.AuthKey) {
		t.Fatal("Tailscale auth key leaked into the SSH remote command")
	}
	keyLine, script, ok := strings.Cut(input, "\n")
	if !ok || keyLine != cfg.Tailscale.AuthKey {
		t.Fatalf("stdin auth framing=%q", input)
	}
	if strings.Contains(script, cfg.Tailscale.AuthKey) {
		t.Fatal("Tailscale auth key leaked into the remote bootstrap script")
	}
	if !strings.Contains(script, `: "${TS_AUTHKEY:?}"`) ||
		!strings.Contains(script, "tailscale up --auth-key=file:/dev/stdin") {
		t.Fatalf("direct Tailscale bootstrap script=%s", script)
	}
	if strings.Contains(script, "crabbox-cloud-init-secret-cleanup") ||
		strings.Contains(script, "/etc/cloud/cloud-init.disabled") {
		t.Fatalf("direct Tailscale bootstrap mutated fresh cloud-init state: %s", script)
	}
}

func TestRenderTailscaleHostname(t *testing.T) {
	got := RenderTailscaleHostname("CBX-{slug}-{provider}-{id}", "cbx_abcdef123456", "Blue Lobster", "aws")
	if got != "cbx-blue-lobster-aws-cbx-abcdef123456" {
		t.Fatalf("renderTailscaleHostname=%q", got)
	}
}

func TestCoordinatorTailscaleProviderMismatchRetainsPreviousServer(t *testing.T) {
	previous := Server{CloudID: "i-original", Provider: "aws", Name: "original"}
	updated, err := coordinatorTailscaleResponseServer(Config{Provider: "aws"}, previous, CoordinatorLease{
		ID: "cbx_tailscale_identity", Provider: "external", CloudID: "external-workspace", ServerName: "replacement",
	})
	if !isCoordinatorProviderIdentityError(err) {
		t.Fatalf("error=%v, want typed provider identity mismatch", err)
	}
	if updated.CloudID != previous.CloudID || updated.Provider != previous.Provider || updated.Name != previous.Name {
		t.Fatalf("updated server=%#v, want previous=%#v", updated, previous)
	}
}

func TestValidateNetworkConfigRejectsStaticProvisioning(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "ssh"
	cfg.Tailscale.Enabled = true
	if err := validateNetworkConfig(cfg); err == nil {
		t.Fatal("expected --tailscale static provider validation failure")
	}
}

func TestValidateNetworkConfigAllowsHyperVWindowsTailscale(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider = "hyperv"
	cfg.TargetOS = targetWindows
	cfg.WindowsMode = WindowsModeNormal
	cfg.Tailscale.Enabled = true
	if err := validateNetworkConfig(cfg); err != nil {
		t.Fatalf("validate Hyper-V Windows Tailscale: %v", err)
	}
}
