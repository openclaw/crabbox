package cli

import (
	"context"
	"strings"
	"testing"
)

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
		},
	}
	server.PublicNet.IPv4.IP = "203.0.113.10"
	target := SSHTarget{Host: "203.0.113.10", Port: "2222"}
	got := bootstrapNetworkTarget(cfg, server, target)
	if got.Host != "crabbox-blue-lobster" || got.NetworkKind != NetworkTailscale {
		t.Fatalf("bootstrap target = host=%s network=%s", got.Host, got.NetworkKind)
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

func TestRenderTailscaleHostname(t *testing.T) {
	got := renderTailscaleHostname("CBX-{slug}-{provider}-{id}", "cbx_abcdef123456", "Blue Lobster", "aws")
	if got != "cbx-blue-lobster-aws-cbx-abcdef123456" {
		t.Fatalf("renderTailscaleHostname=%q", got)
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
